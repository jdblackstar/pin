//go:build unix

package app

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestSkillPrimaryFIFODoesNotBlockLifecycle(t *testing.T) {
	for _, file := range []string{"SKILL.md", skillMarkerName} {
		for _, args := range [][]string{
			{"status"}, {"install", "--yes"}, {"remove", "--yes"},
			{"install", "--yes", "--force"}, {"remove", "--yes", "--force"},
		} {
			t.Run(file+"/"+strings.Join(args, " "), func(t *testing.T) {
				home := t.TempDir()
				t.Setenv("HOME", home)
				t.Setenv("PIN_SKILL_HOME", home)
				t.Setenv("CLAUDE_CONFIG_DIR", "")
				t.Setenv("PATH", "/usr/bin:/bin")
				path := filepath.Join(home, ".agents", "skills", pinSkillName)
				if err := writeBundledSkill(path); err != nil {
					t.Fatal(err)
				}
				fifo := filepath.Join(path, file)
				if err := os.Remove(fifo); err != nil {
					t.Fatal(err)
				}
				if err := syscall.Mkfifo(fifo, 0o644); err != nil {
					t.Fatal(err)
				}
				done := make(chan cliResult, 1)
				go func() {
					var stdout, stderr bytes.Buffer
					code := runCLI(append([]string{"skill"}, args...), strings.NewReader(""), &stdout, &stderr)
					done <- cliResult{code: code, stdout: stdout.String(), stderr: stderr.String()}
				}()
				var result cliResult
				select {
				case result = <-done:
				case <-time.After(5 * time.Second):
					unblockFIFOReader(t, fifo, done)
					t.Fatal("skill operation blocked on a primary-file FIFO")
				}
				force := args[len(args)-1] == "--force"
				if args[0] == "status" || !force {
					wantCode := 2
					if args[0] == "status" {
						wantCode = 0
						requireContains(t, result.stdout, "unmanaged")
					} else {
						requireContains(t, result.stderr, "unmanaged")
					}
					requireCode(t, result, wantCode)
					if info, err := os.Lstat(fifo); err != nil || info.Mode()&os.ModeNamedPipe == 0 {
						t.Fatalf("unforced operation changed FIFO: %v", err)
					}
				} else {
					requireCode(t, result, 0)
					if args[0] == "install" {
						state, err := inspectSkillAt(path)
						if err != nil || state != skillCurrent {
							t.Fatalf("forced install state = %s, err = %v", state, err)
						}
					} else if _, err := os.Lstat(path); !os.IsNotExist(err) {
						t.Fatalf("forced removal did not remove skill: %v", err)
					}
				}
			})
		}
	}
}
