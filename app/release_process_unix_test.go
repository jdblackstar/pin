//go:build unix

package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

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

func TestExtractGitArchiveReportsBoundedArchiveFailure(t *testing.T) {
	configureArchiveTestCommands(t, `
i=0
while [ "$i" -lt 100 ]; do
	printf 'archive noise archive noise archive noise archive noise\n' >&2
	i=$((i + 1))
done
printf 'archive final diagnostic\n' >&2
exit 7
`, `/bin/cat >/dev/null`, commandLimits{timeout: time.Second, outputLimit: 128})

	err := extractGitArchive(t.TempDir(), "revision", t.TempDir())
	if err == nil {
		t.Fatal("extractGitArchive returned nil error for archive failure")
	}
	requireContains(t, err.Error(), "git archive failed")
	requireContains(t, err.Error(), strings.TrimSpace(truncatedOutputMarker))
	requireContains(t, err.Error(), "archive final diagnostic")
	if len(err.Error()) > 256 {
		t.Fatalf("archive error diagnostics are not bounded: length = %d", len(err.Error()))
	}
}

func TestExtractGitArchiveReportsExtractionFailureAndCleansPartialOutput(t *testing.T) {
	configureArchiveTestCommands(t, `printf 'not a tar archive'`, `
printf 'partial' > "$4/partial"
printf 'extract final diagnostic\n' >&2
exit 9
`, commandLimits{timeout: time.Second, outputLimit: 128})
	destination := filepath.Join(t.TempDir(), "release")
	if err := os.Mkdir(destination, 0o755); err != nil {
		t.Fatal(err)
	}

	err := cleanupOnError(destination, func() error {
		return extractGitArchive(t.TempDir(), "revision", destination)
	})
	if err == nil {
		t.Fatal("extractGitArchive returned nil error for extraction failure")
	}
	requireContains(t, err.Error(), "tar extract failed")
	requireContains(t, err.Error(), "extract final diagnostic")
	if _, statErr := os.Stat(destination); !os.IsNotExist(statErr) {
		t.Fatalf("partial extraction was not cleaned up: stat error = %v", statErr)
	}
}

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
