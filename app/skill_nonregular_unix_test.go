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

func TestSkillPrimaryNonRegularFileDoesNotBlockLifecycle(t *testing.T) {
	for _, kind := range []struct {
		name string
		mode os.FileMode
		make func(*testing.T, string)
	}{
		{"fifo", os.ModeNamedPipe, func(t *testing.T, path string) {
			if err := syscall.Mkfifo(path, 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{"socket", os.ModeSocket, makeUnixSocket},
	} {
		for _, file := range []string{"SKILL.md", skillMarkerName} {
			for _, args := range [][]string{
				{"status"}, {"install", "--yes"}, {"remove", "--yes"},
				{"install", "--yes", "--force"}, {"remove", "--yes", "--force"},
			} {
				t.Run(kind.name+"/"+file+"/"+strings.Join(args, " "), func(t *testing.T) {
					home := t.TempDir()
					t.Setenv("HOME", home)
					t.Setenv("PIN_SKILL_HOME", home)
					t.Setenv("CLAUDE_CONFIG_DIR", "")
					t.Setenv("PATH", "/usr/bin:/bin")
					path := filepath.Join(home, ".agents", "skills", pinSkillName)
					if err := writeBundledSkill(path); err != nil {
						t.Fatal(err)
					}
					special := filepath.Join(path, file)
					if err := os.Remove(special); err != nil {
						t.Fatal(err)
					}
					kind.make(t, special)
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
						if kind.mode == os.ModeNamedPipe {
							unblockFIFOReader(t, special, done)
						}
						t.Fatalf("skill operation blocked on a primary-file %s", kind.name)
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
						if info, err := os.Lstat(special); err != nil || info.Mode()&kind.mode == 0 {
							t.Fatalf("unforced operation changed %s: %v", kind.name, err)
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
}

// makeUnixSocket binds a socket at a short temporary path, which sun_path
// requires, and moves it into place.
func makeUnixSocket(t *testing.T, path string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "pin-sock")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	fd, err := syscall.Socket(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(fd)
	bound := filepath.Join(dir, "s")
	if err := syscall.Bind(fd, &syscall.SockaddrUnix{Name: bound}); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(bound, path); err != nil {
		t.Fatal(err)
	}
}
