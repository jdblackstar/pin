//go:build unix

package app

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// configureArchiveTestCommands installs fake archive tools and temporary command limits.
func configureArchiveTestCommands(t *testing.T, gitScript, tarScript string, limits commandLimits) {
	t.Helper()
	bin := t.TempDir()
	for name, script := range map[string]string{"git": gitScript, "tar": tarScript} {
		path := filepath.Join(bin, name)
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	previous := defaultCommandLimits
	defaultCommandLimits = limits
	t.Cleanup(func() { defaultCommandLimits = previous })
}

// TestExtractGitArchiveReportsBoundedArchiveFailure verifies bounded Git diagnostics.
func TestExtractGitArchiveReportsBoundedArchiveFailure(t *testing.T) {
	configureArchiveTestCommands(t, `
i=0
while [ "$i" -lt 100 ]; do
	printf 'archive noise archive noise archive noise archive noise\n' >&2
	i=$((i + 1))
done
printf 'archive final diagnostic\n' >&2
exit 7
`, `/bin/sleep 10`, commandLimits{timeout: 2 * time.Second, outputLimit: 128})
	started := time.Now()

	err := extractGitArchive(t.TempDir(), "revision", t.TempDir())
	if err == nil {
		t.Fatal("extractGitArchive returned nil error for archive failure")
	}
	if elapsed := time.Since(started); elapsed >= time.Second {
		t.Fatalf("archive failure returned after %s, want less than 1s", elapsed)
	}
	requireContains(t, err.Error(), "git archive failed")
	requireContains(t, err.Error(), strings.TrimSpace(truncatedOutputMarker))
	requireContains(t, err.Error(), "archive final diagnostic")
	if len(err.Error()) > 256 {
		t.Fatalf("archive error diagnostics are not bounded: length = %d", len(err.Error()))
	}
}

// TestExtractGitArchiveReportsExtractionFailureAndCleansPartialOutput verifies tar cleanup.
func TestExtractGitArchiveReportsExtractionFailureAndCleansPartialOutput(t *testing.T) {
	configureArchiveTestCommands(t, `/bin/sleep 10`, `
printf 'partial' > "$4/partial"
printf 'extract final diagnostic\n' >&2
exit 9
`, commandLimits{timeout: 2 * time.Second, outputLimit: 128})
	destination := filepath.Join(t.TempDir(), "release")
	if err := os.Mkdir(destination, 0o755); err != nil {
		t.Fatal(err)
	}
	started := time.Now()

	err := cleanupOnError(destination, func() error {
		return extractGitArchive(t.TempDir(), "revision", destination)
	})
	if err == nil {
		t.Fatal("extractGitArchive returned nil error for extraction failure")
	}
	if elapsed := time.Since(started); elapsed >= time.Second {
		t.Fatalf("extraction failure returned after %s, want less than 1s", elapsed)
	}
	requireContains(t, err.Error(), "tar extract failed")
	requireContains(t, err.Error(), "extract final diagnostic")
	if _, statErr := os.Stat(destination); !os.IsNotExist(statErr) {
		t.Fatalf("partial extraction was not cleaned up: stat error = %v", statErr)
	}
}

// TestExtractGitArchiveReportsBothFailuresDeterministically verifies neither stage is hidden.
func TestExtractGitArchiveReportsBothFailuresDeterministically(t *testing.T) {
	root := t.TempDir()
	gitReady := filepath.Join(root, "git-ready")
	tarReady := filepath.Join(root, "tar-ready")
	configureArchiveTestCommands(t, `
printf 'git failure diagnostic\n' >&2
: > `+shellQuote(gitReady)+`
while [ ! -e `+shellQuote(tarReady)+` ]; do /bin/sleep 0.01; done
exit 7
`, `
printf 'tar failure diagnostic\n' >&2
: > `+shellQuote(tarReady)+`
while [ ! -e `+shellQuote(gitReady)+` ]; do /bin/sleep 0.01; done
exit 9
`, commandLimits{timeout: 2 * time.Second, outputLimit: 128})

	err := extractGitArchive(t.TempDir(), "revision", t.TempDir())
	if err == nil {
		t.Fatal("extractGitArchive returned nil error when both stages failed")
	}
	message := err.Error()
	gitIndex := strings.Index(message, "git archive failed")
	tarIndex := strings.Index(message, "tar extract failed")
	if gitIndex == -1 || tarIndex == -1 {
		t.Fatalf("pipeline error did not report both failures: %v", err)
	}
	if gitIndex > tarIndex {
		t.Fatalf("pipeline failures are not in deterministic Git-then-tar order: %v", err)
	}
	requireContains(t, message, "git failure diagnostic")
	requireContains(t, message, "tar failure diagnostic")
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("pipeline error does not preserve an underlying exit error: %v", err)
	}
}

// TestExtractGitArchiveTimesOutAndCleansPartialOutput verifies timeout cleanup.
func TestExtractGitArchiveTimesOutAndCleansPartialOutput(t *testing.T) {
	configureArchiveTestCommands(t, `
printf 'archive waiting\n' >&2
/bin/sleep 10
`, `
printf 'partial' > "$4/partial"
/bin/cat >/dev/null
`, commandLimits{timeout: 100 * time.Millisecond, outputLimit: 128})
	destination := filepath.Join(t.TempDir(), "release")
	if err := os.Mkdir(destination, 0o755); err != nil {
		t.Fatal(err)
	}
	started := time.Now()

	err := cleanupOnError(destination, func() error {
		return extractGitArchive(t.TempDir(), "revision", destination)
	})
	if err == nil {
		t.Fatal("extractGitArchive returned nil error for timed-out pipeline")
	}
	if elapsed := time.Since(started); elapsed >= time.Second {
		t.Fatalf("timed-out archive pipeline returned after %s, want less than 1s", elapsed)
	}
	requireContains(t, err.Error(), "git archive and tar extract timed out after 100ms")
	requireContains(t, err.Error(), "git: archive waiting")
	if _, statErr := os.Stat(destination); !os.IsNotExist(statErr) {
		t.Fatalf("timed-out partial extraction was not cleaned up: stat error = %v", statErr)
	}
}

// TestRunCommandTimeoutKillsProcessTree verifies descendants are terminated on timeout.
func TestRunCommandTimeoutKillsProcessTree(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "descendant-survived")
	script := `(sleep 1; printf survived > "$1") & sleep 10`
	limits := commandLimits{timeout: 100 * time.Millisecond, outputLimit: 1024}

	_, err := runCommandWithLimits([]string{"sh", "-c", script, "sh", marker}, t.TempDir(), nil, limits)
	if err == nil {
		t.Fatal("runCommandWithLimits returned nil error for timed-out process tree")
	}
	requireContains(t, err.Error(), "command timed out")
	time.Sleep(1200 * time.Millisecond)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("timed-out descendant was not terminated: stat error = %v", err)
	}
}

// Reproduce the assessment's exited-parent fixture. Cancellation alone cannot
// clean up descendants once os/exec has stopped watching the command context.
func TestRunCommandCleansDescendantsAfterParentExits(t *testing.T) {
	for _, test := range []struct {
		name    string
		timeout time.Duration
		pipes   bool
		exit    int
	}{
		{"inherited pipes", 5 * time.Second, true, 0},
		{"inherited pipes at deadline", 500 * time.Millisecond, true, 0},
		{"closed pipes", 5 * time.Second, false, 0},
		{"failed parent", 5 * time.Second, false, 7},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			pidfile := filepath.Join(root, "child.pid")
			gate := filepath.Join(root, "gate")
			marker := filepath.Join(root, "survived")
			child := `from pathlib import Path; import os,sys,time
Path(sys.argv[1]).write_text(str(os.getpid()))
end = time.monotonic() + 20
while not Path(sys.argv[2]).exists() and time.monotonic() < end: time.sleep(.01)
if Path(sys.argv[2]).exists(): Path(sys.argv[3]).write_text('survived')
`
			parent := `from pathlib import Path; import subprocess,sys,time
kwargs = {} if sys.argv[5] == 'true' else dict(stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
subprocess.Popen([sys.executable, '-c', sys.argv[1], *sys.argv[2:5]], **kwargs)
while not Path(sys.argv[2]).exists(): time.sleep(.01)
sys.exit(int(sys.argv[6]))
`
			started := time.Now()
			result, err := runCommandWithLimits([]string{"python3", "-c", parent, child, pidfile, gate, marker, strconv.FormatBool(test.pipes), strconv.Itoa(test.exit)}, root, nil, commandLimits{timeout: test.timeout, outputLimit: 128})
			if elapsed := time.Since(started); elapsed > 3*time.Second {
				t.Errorf("command took %s, want bounded pipe wait", elapsed)
			}
			if (err != nil) != (test.pipes || test.exit != 0) {
				t.Errorf("result = %#v, err = %v", result, err)
			}
			pid := readProcessPID(t, pidfile)
			t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
			requireProcessStopped(t, pid)
			writeFile(t, gate, "open")
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("descendant executed after command returned: %v", err)
			}
		})
	}
}

func readProcessPID(t *testing.T, path string) int {
	t.Helper()
	pid, err := strconv.Atoi(strings.TrimSpace(mustReadFile(t, path)))
	if err != nil || pid <= 0 {
		t.Fatalf("invalid PID in %s: %d, %v", path, pid, err)
	}
	return pid
}

func requireProcessStopped(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
			return
		}
		// Orphans may remain zombies until the orb's PID 1 reaps them. A
		// zombie cannot execute; kill(pid, 0) alone would report it as alive.
		if runtime.GOOS == "linux" {
			data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
			if os.IsNotExist(err) {
				return
			}
			if err == nil {
				_, state, ok := strings.Cut(string(data), ") ")
				if ok && strings.HasPrefix(state, "Z ") {
					return
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("process %d is still running after cleanup", pid)
}
