//go:build !windows

package app

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestUpdateRestoresLinksWhenActiveVerificationIsInterrupted(t *testing.T) {
	tests := []struct {
		name     string
		signal   os.Signal
		exitCode int
	}{
		{name: "SIGINT", signal: os.Interrupt, exitCode: 130},
		{name: "SIGTERM", signal: syscall.SIGTERM, exitCode: 143},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			repo, oldSHA := sourceRepo(t, root)
			if result := runTool(t, runCompiledPin, root, repo, "update"); result.code != 0 {
				t.Fatalf("first update failed: %s", result.stderr)
			}

			currentLink := filepath.Join(root, "share", "demo-tool", "current")
			marker := filepath.Join(root, "active-verification-started")
			setRepoVerifyToWaitWhenActive(t, repo, currentLink, marker)
			writeToolVersionOnly(t, repo, "2")
			git(t, repo, "add", ".")
			git(t, repo, "commit", "-m", "wait during active verification")
			newSHA := git(t, repo, "rev-parse", "HEAD")
			git(t, repo, "push")

			command := compiledPinCommand(t, root, "update", repo)
			if err := command.Start(); err != nil {
				t.Fatal(err)
			}
			waitForFile(t, marker)
			if err := command.Process.Signal(test.signal); err != nil {
				t.Fatal(err)
			}
			err := waitForCommand(t, command)
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() != test.exitCode {
				t.Fatalf("interrupted update error = %v, want exit code %d", err, test.exitCode)
			}

			requireReleaseLink(t, root, "current", oldSHA)
			if _, err := os.Lstat(filepath.Join(root, "share", "demo-tool", "previous")); !os.IsNotExist(err) {
				t.Fatalf("previous link exists after interrupted activation: %v", err)
			}
			if _, err := os.Stat(filepath.Join(root, "share", "demo-tool", "releases", newSHA)); err != nil {
				t.Fatalf("interrupted candidate release should remain inspectable: %v", err)
			}
		})
	}
}

func setRepoVerifyToWaitWhenActive(t *testing.T, repo, currentLink, marker string) {
	t.Helper()
	encodedCurrent, _ := json.Marshal(currentLink)
	encodedMarker, _ := json.Marshal(marker)
	script := "from pathlib import Path; import time; current = Path(" + string(encodedCurrent) + "); marker = Path(" + string(encodedMarker) + "); active = current.resolve() == Path.cwd().resolve(); marker.write_text('active') if active else None; exec(\"while current.resolve() == Path.cwd().resolve(): time.sleep(0.02)\") if active else None"
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
