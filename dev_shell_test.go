package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestDevShellIsolatesAndPersistsNamedProfile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("development shell is POSIX-only")
	}

	repo, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	devHome := filepath.Join(t.TempDir(), "pin-dev")
	script := filepath.Join(repo, "scripts", "dev-shell")

	command := exec.Command(script, "ux-test", "--", "sh", "-c", `
		[ "$PIN_DEV_PROFILE" = ux-test ] || exit 10
		[ "$PIN_HOME" = "$PIN_DEV_HOME/profiles/ux-test" ] || exit 11
		[ "$(command -v pin)" = "$PWD/.pin-dev/bin/pin" ] || exit 12
		case "$(pin version)" in
			"pin dev-"*) ;;
			*) exit 13 ;;
		esac
		touch "$PIN_HOME/persisted"
		exit 23
	`)
	command.Dir = repo
	command.Env = append(os.Environ(), "PIN_DEV_HOME="+devHome)
	output, err := command.CombinedOutput()
	if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 23 {
		t.Fatalf("dev-shell exit = %v, want 23\n%s", err, output)
	}
	if _, err := os.Stat(filepath.Join(devHome, "profiles", "ux-test", "persisted")); err != nil {
		t.Fatalf("profile did not persist: %v", err)
	}

	reset := exec.Command(filepath.Join(repo, "scripts", "dev-reset"), "ux-test")
	reset.Env = append(os.Environ(), "PIN_DEV_HOME="+devHome)
	if output, err := reset.CombinedOutput(); err != nil {
		t.Fatalf("dev-reset failed: %v\n%s", err, output)
	}
	if _, err := os.Stat(filepath.Join(devHome, "profiles", "ux-test")); !os.IsNotExist(err) {
		t.Fatalf("profile still exists after reset: %v", err)
	}
}

func TestDevScriptsRejectUnsafeProfileNames(t *testing.T) {
	for _, script := range []string{"dev-shell", "dev-reset"} {
		command := exec.Command(filepath.Join("scripts", script), "../live")
		output, err := command.CombinedOutput()
		if err == nil {
			t.Fatalf("%s accepted unsafe profile", script)
		}
		if !strings.Contains(string(output), "invalid profile name") {
			t.Fatalf("%s output = %q", script, output)
		}
	}
}
