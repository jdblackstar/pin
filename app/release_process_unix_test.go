//go:build unix

package app

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
