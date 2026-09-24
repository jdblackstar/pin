//go:build unix

package app

import (
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestReleaseEvidenceFIFOFailsClosedWithoutBlocking(t *testing.T) {
	tests := []struct {
		name     string
		file     string
		message  string
		commands func(repo string) [][]string
	}{
		{
			name:    "metadata",
			file:    metadataName,
			message: "invalid metadata: ",
			commands: func(repo string) [][]string {
				return [][]string{{"status", "demo-tool"}, {"verify", "demo-tool"}, {"run", "demo-tool"}, {"update", repo}}
			},
		},
		{
			name:    "integrity manifest",
			file:    integrityName,
			message: "invalid release integrity manifest: ",
			commands: func(repo string) [][]string {
				return [][]string{{"verify", "demo-tool"}, {"run", "demo-tool"}, {"update", repo}}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			repo, sha := sourceRepo(t, root)
			result := runTool(t, runPin, root, repo, "update")
			requireCode(t, result, 0)

			fifo := filepath.Join(root, "share", "demo-tool", "releases", sha, metadataDir, test.file)
			if err := os.Remove(fifo); err != nil {
				t.Fatal(err)
			}
			if err := syscall.Mkfifo(fifo, 0o644); err != nil {
				t.Fatal(err)
			}

			for _, command := range test.commands(repo) {
				result, ok := runPinUnlessBlockedOnFIFO(t, root, fifo, command...)
				if !ok {
					t.Errorf("pin %v blocked opening FIFO %s, want it rejected without waiting for a writer", command, fifo)
					continue
				}
				requireCode(t, result, 2)
				requireContains(t, result.stderr, test.message+fifo+": not a regular file")
			}
		})
	}
}

// A release file swapped for a FIFO after the integrity walk's lstat must be
// rejected by hashIntegrityFile rather than block it.
func TestHashIntegrityFileRejectsFIFOWithoutBlocking(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "payload.py")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, _, err := hashIntegrityFile(fifo, integrityFileState{}, false, sha256.New(), make([]byte, 4096), time.Time{})
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, errNotRegularFile) {
			t.Fatalf("hashIntegrityFile(FIFO) error = %v, want %v", err, errNotRegularFile)
		}
		requireContains(t, err.Error(), fifo)
	case <-time.After(5 * time.Second):
		unblockFIFOReader(t, fifo, done)
		t.Fatal("hashIntegrityFile blocked opening a FIFO, want it rejected without waiting for a writer")
	}
}

// runPinUnlessBlockedOnFIFO runs pin and reports false if it is still running
// after a timeout, after unblocking it so it finishes before the test ends.
func runPinUnlessBlockedOnFIFO(t *testing.T, root, fifo string, args ...string) (cliResult, bool) {
	t.Helper()
	done := make(chan cliResult, 1)
	go func() { done <- runPin(t, root, args...) }()
	select {
	case result := <-done:
		return result, true
	case <-time.After(5 * time.Second):
		unblockFIFOReader(t, fifo, done)
		return cliResult{}, false
	}
}

// unblockFIFOReader opens and closes fifo as a writer so a reader blocked in
// open(2) gets EOF, then waits for done. If done stays open, the goroutine is
// stuck somewhere other than the FIFO open, so the test fails instead of hanging.
func unblockFIFOReader[T any](t *testing.T, fifo string, done <-chan T) {
	t.Helper()
	if writer, err := os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
		writer.Close()
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("still blocked after opening FIFO %s as a writer", fifo)
	}
}
