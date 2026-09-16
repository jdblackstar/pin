//go:build unix

package app

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

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
