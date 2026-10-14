//go:build unix

package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestRunWithSignalRestorationDrainsSignalDeliveredWhileStopping(t *testing.T) {
	var signals chan<- os.Signal
	restored := false
	err := runWithSignalRestorationSignals(
		func() error { restored = true; return nil },
		func(context.Context) error { return nil },
		func(channel chan<- os.Signal, _ ...os.Signal) { signals = channel },
		func(chan<- os.Signal) { signals <- syscall.SIGTERM },
	)
	var exit exitError
	if !errors.As(err, &exit) || exit.code != 143 {
		t.Fatalf("error = %v, want exit code 143", err)
	}
	if !restored {
		t.Fatal("interrupted completion did not restore links")
	}
}

func TestRunWithSignalRestorationWaitsForCleanupBeforeRestore(t *testing.T) {
	var signals chan<- os.Signal
	notified := make(chan struct{})
	cleanupStarted := make(chan struct{})
	cleanupGate := make(chan struct{})
	restored := make(chan struct{})
	stopped := make(chan struct{})
	returned := make(chan error, 1)
	finishCleanup := sync.OnceFunc(func() { close(cleanupGate) })
	t.Cleanup(finishCleanup)
	ctx := pinContext{name: "demo", pinHome: t.TempDir()}
	lock, err := acquireToolLock(ctx)
	if err != nil {
		t.Fatal(err)
	}
	contender, err := os.OpenFile(filepath.Join(ctx.toolRoot(), ".pin.lock"), os.O_RDWR, 0)
	if err != nil {
		lock.release()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = contender.Close() })

	go func() {
		err := runWithSignalRestorationSignals(
			func() error {
				select {
				case <-stopped:
					t.Error("signal interception stopped before restoring an interrupted operation")
				default:
				}
				close(restored)
				return nil
			},
			func(ctx context.Context) error {
				<-ctx.Done()
				close(cleanupStarted)
				<-cleanupGate
				return ctx.Err()
			},
			func(channel chan<- os.Signal, _ ...os.Signal) {
				signals = channel
				close(notified)
			},
			func(chan<- os.Signal) { close(stopped) },
		)
		lock.release()
		returned <- err
	}()
	<-notified
	signals <- os.Interrupt
	select {
	case <-cleanupStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("signal did not start operation cleanup")
	}
	if err := syscall.Flock(int(contender.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); !errors.Is(err, syscall.EWOULDBLOCK) {
		t.Fatalf("tool lock released before cleanup finished: %v", err)
	}

	select {
	case <-restored:
		t.Fatal("links restored before operation cleanup finished")
	case <-stopped:
		t.Fatal("signal interception stopped before interrupted cleanup finished")
	case err := <-returned:
		t.Fatalf("operation returned before cleanup finished: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	finishCleanup()
	select {
	case err := <-returned:
		var exit exitError
		if !errors.As(err, &exit) || exit.code != 130 {
			t.Fatalf("error = %v, want exit code 130", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for cleanup and restoration")
	}
	select {
	case <-restored:
	default:
		t.Fatal("operation returned before restoring links")
	}
	if err := syscall.Flock(int(contender.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("tool lock remained held after cleanup and restoration: %v", err)
	}
	_ = syscall.Flock(int(contender.Fd()), syscall.LOCK_UN)
}

func TestActivationInterruptStopsVerifierAndRestoresLinks(t *testing.T) {
	tests := []struct {
		name     string
		command  string
		signal   os.Signal
		exitCode int
	}{
		{name: "update SIGINT", command: "update", signal: os.Interrupt, exitCode: 130},
		{name: "update SIGTERM", command: "update", signal: syscall.SIGTERM, exitCode: 143},
		{name: "rollback SIGINT", command: "rollback", signal: os.Interrupt, exitCode: 130},
		{name: "rollback SIGTERM", command: "rollback", signal: syscall.SIGTERM, exitCode: 143},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			repo, _ := sourceRepo(t, root)
			currentLink := filepath.Join(root, "share", "demo-tool", "current")
			marker := filepath.Join(root, "active-verification-started")
			setRepoVerifyToWaitWhenActive(t, repo, currentLink, marker)
			git(t, repo, "add", ".")
			git(t, repo, "commit", "-m", "configure gated verification")
			oldSHA := git(t, repo, "rev-parse", "HEAD")
			git(t, repo, "push")
			if result := runTool(t, runCompiledPin, root, repo, "update"); result.code != 0 {
				t.Fatalf("first update failed: %s", result.stderr)
			}

			writeToolVersionOnly(t, repo, "2")
			git(t, repo, "add", ".")
			git(t, repo, "commit", "-m", "wait during active verification")
			newSHA := git(t, repo, "rev-parse", "HEAD")
			git(t, repo, "push")
			if test.command == "rollback" {
				result := runTool(t, runCompiledPin, root, repo, "update")
				requireCode(t, result, 0)
			}
			writeFile(t, marker+".enable", "enabled")

			command := compiledPinCommand(t, root, test.command, repo)
			if err := command.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = command.Process.Kill() })
			waitForFile(t, marker)
			verifierPID := readProcessPID(t, marker)
			childPID := readProcessPID(t, marker+".child")
			t.Cleanup(func() {
				_ = syscall.Kill(verifierPID, syscall.SIGKILL)
				_ = syscall.Kill(childPID, syscall.SIGKILL)
			})
			if err := command.Process.Signal(test.signal); err != nil {
				t.Fatal(err)
			}
			err := waitForCommand(t, command)
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() != test.exitCode {
				t.Fatalf("interrupted %s error = %v, want exit code %d", test.command, err, test.exitCode)
			}

			requireProcessStopped(t, verifierPID)
			requireProcessStopped(t, childPID)
			if test.command == "rollback" {
				requireReleaseLink(t, root, "current", newSHA)
				requireReleaseLink(t, root, "previous", oldSHA)
			} else {
				requireReleaseLink(t, root, "current", oldSHA)
				if _, err := os.Lstat(filepath.Join(root, "share", "demo-tool", "previous")); !os.IsNotExist(err) {
					t.Fatalf("previous link exists after interrupted activation: %v", err)
				}
			}
			if _, err := os.Stat(filepath.Join(root, "share", "demo-tool", "releases", newSHA)); err != nil {
				t.Fatalf("interrupted candidate release should remain inspectable: %v", err)
			}
			// The tool lock must be released only after verifier cleanup. A
			// subsequent update can proceed without an old verifier mutating it.
			result := runTool(t, runCompiledPin, root, repo, "update")
			requireCode(t, result, 0)
			writeFile(t, marker+".gate", "open")
			if _, err := os.Stat(marker + ".survived"); !os.IsNotExist(err) {
				t.Fatalf("old verifier executed after interruption: %v", err)
			}
		})
	}
}

func setRepoVerifyToWaitWhenActive(t *testing.T, repo, currentLink, marker string) {
	t.Helper()
	encodedCurrent, _ := json.Marshal(currentLink)
	encodedMarker, _ := json.Marshal(marker)
	script := `from pathlib import Path; import os,subprocess,sys,time
current = Path(` + string(encodedCurrent) + `)
marker = Path(` + string(encodedMarker) + `)
if current.resolve() == Path.cwd().resolve() and Path(str(marker)+'.enable').exists() and not marker.exists():
    child = "from pathlib import Path; import os,sys,time; Path(sys.argv[1]+'.child').write_text(str(os.getpid())); end=time.monotonic()+20; exec('while not Path(sys.argv[1]+\".gate\").exists() and time.monotonic()<end: time.sleep(.01)'); Path(sys.argv[1]+'.survived').write_text('child survived')"
    subprocess.Popen([sys.executable, '-c', child, str(marker)])
    while not Path(str(marker)+'.child').exists(): time.sleep(.01)
    marker.write_text(str(os.getpid()))
    end = time.monotonic() + 20
    while not Path(str(marker)+'.gate').exists() and time.monotonic()<end: time.sleep(.01)
    Path(str(marker)+'.survived').write_text('verifier survived')
`
	encodedScript, _ := json.Marshal(script)
	replaceInFile(
		t,
		filepath.Join(repo, "pin.toml"),
		`verify = ["demo-tool"]`,
		`verify = [["python3", "-c", `+string(encodedScript)+`]]`,
	)
}

func writeToolVersionOnly(t *testing.T, repo, version string) {
	t.Helper()
	replaceInFile(t, filepath.Join(repo, "demo_tool.py"), `print("demo 1")`, `print("demo `+version+`")`)
}

func compiledPinCommand(t *testing.T, root string, args ...string) *exec.Cmd {
	t.Helper()
	prepareToolEnv(t, root)
	bin := filepath.Join(root, "pin")
	if _, err := os.Stat(bin); err != nil {
		run(t, "", "go", "build", "-o", bin, "..")
	}
	allArgs := append([]string{"--pin-home", filepath.Join(root, "share")}, args...)
	command := exec.Command(bin, allArgs...)
	devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = devNull.Close() })
	command.Stdout = devNull
	command.Stderr = devNull
	return command
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		} else if !os.IsNotExist(err) {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", path)
}

func waitForCommand(t *testing.T, command *exec.Cmd) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	select {
	case err := <-done:
		return err
	case <-time.After(10 * time.Second):
		_ = command.Process.Kill()
		t.Fatalf("timed out waiting for interrupted update")
		return nil
	}
}
