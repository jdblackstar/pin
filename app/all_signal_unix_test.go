//go:build unix

package app

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
)

func TestUpdateAllInterruptStopsBeforeNextTool(t *testing.T) {
	for _, test := range []struct {
		name string
		sig  os.Signal
		code int
	}{
		{"SIGINT", os.Interrupt, 130},
		{"SIGTERM", syscall.SIGTERM, 143},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			alphaRepo, alphaOld := namedSourceRepo(t, filepath.Join(root, "alpha-source"), "alpha")
			betaRepo, betaOld := namedSourceRepo(t, filepath.Join(root, "beta-source"), "beta")
			for _, repo := range []string{alphaRepo, betaRepo} {
				requireCode(t, runTool(t, runCompiledPin, root, repo, "update"), 0)
			}

			marker := filepath.Join(root, "alpha-verifier-started")
			commitNamedToolVersion(t, alphaRepo, "alpha", "2")
			setNamedRepoVerifyToWaitWhenActive(t, alphaRepo, filepath.Join(root, "share", "alpha", "current"), marker)
			git(t, alphaRepo, "add", "pin.toml")
			git(t, alphaRepo, "commit", "-m", "gate active verification")
			git(t, alphaRepo, "push")
			alphaNew := git(t, alphaRepo, "rev-parse", "HEAD")
			betaNew := commitNamedToolVersion(t, betaRepo, "beta", "2")

			command := compiledPinCommand(t, root, "update", "--all", "--yes")
			if err := command.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = command.Process.Kill() })
			waitForFile(t, marker)
			verifierPID := readProcessPID(t, marker)
			t.Cleanup(func() { _ = syscall.Kill(verifierPID, syscall.SIGKILL) })
			if err := command.Process.Signal(test.sig); err != nil {
				t.Fatal(err)
			}
			err := waitForCommand(t, command)
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() != test.code {
				t.Fatalf("update --all error = %v, want exit code %d", err, test.code)
			}

			requireProcessStopped(t, verifierPID)
			requireReleaseLinkForTool(t, root, "alpha", "current", alphaOld)
			requireReleaseLinkForTool(t, root, "beta", "current", betaOld)
			for _, name := range []string{"alpha", "beta"} {
				if _, err := os.Lstat(filepath.Join(root, "share", name, "previous")); !os.IsNotExist(err) {
					t.Fatalf("previous link for %s changed after interruption: %v", name, err)
				}
			}
			if _, err := os.Stat(filepath.Join(root, "share", "alpha", "releases", alphaNew)); err != nil {
				t.Fatalf("first candidate release should remain: %v", err)
			}
			if _, err := os.Stat(filepath.Join(root, "share", "beta", "releases", betaNew)); !os.IsNotExist(err) {
				t.Fatalf("second tool was updated after interruption: %v", err)
			}
		})
	}
}

func setNamedRepoVerifyToWaitWhenActive(t *testing.T, repo, currentLink, marker string) {
	t.Helper()
	encodedCurrent, _ := json.Marshal(currentLink)
	encodedMarker, _ := json.Marshal(marker)
	script := `from pathlib import Path; import os,time
current = Path(` + string(encodedCurrent) + `)
marker = Path(` + string(encodedMarker) + `)
if current.resolve() == Path.cwd().resolve():
    marker.write_text(str(os.getpid()))
    while True: time.sleep(.01)
`
	encodedScript, _ := json.Marshal(script)
	replaceInFile(t, filepath.Join(repo, "pin.toml"), `verify = ["demo-tool"]`, `verify = [["python3", "-c", `+string(encodedScript)+`]]`)
}
