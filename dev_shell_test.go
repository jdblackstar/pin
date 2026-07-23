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
	realDevHome := filepath.Join(t.TempDir(), "real-pin-dev")
	realProfile := filepath.Join(realDevHome, "profiles", "ux-test")
	if err := os.MkdirAll(realProfile, 0o755); err != nil {
		t.Fatal(err)
	}
	realSentinel := filepath.Join(realProfile, "keep")
	if err := os.WriteFile(realSentinel, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PIN_DEV_HOME", realDevHome)

	realHome := t.TempDir()
	realClaudeConfig := filepath.Join(realHome, "claude-config")
	if err := os.MkdirAll(realClaudeConfig, 0o755); err != nil {
		t.Fatal(err)
	}
	devHome := filepath.Join(t.TempDir(), "pin-dev")
	script := filepath.Join(repo, "scripts", "dev-shell")
	devEnv := envWith(
		os.Environ(),
		"PIN_DEV_HOME="+devHome,
		"HOME="+realHome,
		"CLAUDE_CONFIG_DIR="+realClaudeConfig,
		"GOMODCACHE="+goEnv(t, "GOMODCACHE"),
		"GOCACHE="+goEnv(t, "GOCACHE"),
	)

	command := exec.Command(script, "ux-test", "--", "sh", "-c", `
		[ "$PIN_DEV_PROFILE" = ux-test ] || exit 10
		[ "$PIN_HOME" = "$PIN_DEV_HOME/profiles/ux-test" ] || exit 11
		[ "$PIN_SKILL_HOME" = "$PIN_HOME/skill-home" ] || exit 14
		[ "$(command -v pin)" = "$PWD/.pin-dev/bin/pin" ] || exit 12
		case "$(pin version)" in
			"pin dev-"*) ;;
			*) exit 13 ;;
		esac
		pin skill install --yes || exit 15
		[ -f "$PIN_SKILL_HOME/.agents/skills/pin/SKILL.md" ] || exit 16
		[ -f "$PIN_SKILL_HOME/.claude/skills/pin/SKILL.md" ] || exit 17
		touch "$PIN_HOME/persisted"
		exit 23
	`)
	command.Dir = repo
	command.Env = devEnv
	output, err := command.CombinedOutput()
	if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 23 {
		t.Fatalf("dev-shell exit = %v, want 23\n%s", err, output)
	}
	if _, err := os.Stat(filepath.Join(devHome, "profiles", "ux-test", "persisted")); err != nil {
		t.Fatalf("profile did not persist: %v", err)
	}
	for _, path := range []string{
		filepath.Join(realHome, ".agents", "skills", "pin"),
		filepath.Join(realClaudeConfig, "skills", "pin"),
	} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("skill escaped development profile to %s: %v", path, err)
		}
	}

	command = exec.Command(script, "ux-test", "--", "pin", "skill", "status")
	command.Dir = repo
	command.Env = devEnv
	output, err = command.CombinedOutput()
	if err != nil {
		t.Fatalf("persistent profile status failed: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "pin: current path="+filepath.Join(devHome, "profiles", "ux-test", "skill-home", ".agents", "skills", "pin")) {
		t.Fatalf("persistent profile lost canonical skill:\n%s", output)
	}
	if !strings.Contains(string(output), "claude compatibility: current") {
		t.Fatalf("persistent profile lost Claude compatibility copy:\n%s", output)
	}

	command = exec.Command(script, "other-profile", "--", "pin", "skill", "install", "--yes")
	command.Dir = repo
	command.Env = devEnv
	if output, err = command.CombinedOutput(); err != nil {
		t.Fatalf("second profile install failed: %v\n%s", err, output)
	}
	otherSkill := filepath.Join(devHome, "profiles", "other-profile", "skill-home", ".agents", "skills", "pin")
	if _, err := os.Stat(filepath.Join(otherSkill, "SKILL.md")); err != nil {
		t.Fatalf("second profile skill missing: %v", err)
	}

	reset := exec.Command(filepath.Join(repo, "scripts", "dev-reset"), "ux-test")
	reset.Env = devEnv
	if output, err := reset.CombinedOutput(); err != nil {
		t.Fatalf("dev-reset failed: %v\n%s", err, output)
	}
	if _, err := os.Stat(filepath.Join(devHome, "profiles", "ux-test")); !os.IsNotExist(err) {
		t.Fatalf("profile still exists after reset: %v", err)
	}
	if _, err := os.Stat(realSentinel); err != nil {
		t.Fatalf("existing PIN_DEV_HOME was modified: %v", err)
	}
	if _, err := os.Stat(filepath.Join(otherSkill, "SKILL.md")); err != nil {
		t.Fatalf("reset removed another profile's skill: %v", err)
	}
}

func TestDevShellPromptSurvivesStartupFiles(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("development shell is POSIX-only")
	}

	repo, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(repo, "scripts", "dev-shell")

	tests := []struct {
		name      string
		shellName string
		configure func(t *testing.T, home string) []string
	}{
		{
			name:      "bash",
			shellName: "bash",
			configure: func(t *testing.T, home string) []string {
				t.Helper()
				bashrc := filepath.Join(home, ".bashrc")
				if err := os.WriteFile(bashrc, []byte("PS1='user> '\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				return []string{"PIN_DEV_ORIGINAL_BASHRC=" + bashrc}
			},
		},
		{
			name:      "zsh",
			shellName: "zsh",
			configure: func(t *testing.T, home string) []string {
				t.Helper()
				zdotdir := filepath.Join(home, "zsh")
				if err := os.MkdirAll(zdotdir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(zdotdir, ".zshenv"), []byte("export PIN_TEST_ZSHENV=loaded\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				zshrc := "[[ $PIN_TEST_ZSHENV = loaded ]] || exit 71\nPROMPT='user> '\n"
				if err := os.WriteFile(filepath.Join(zdotdir, ".zshrc"), []byte(zshrc), 0o644); err != nil {
					t.Fatal(err)
				}
				return []string{"ZDOTDIR=" + zdotdir}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			shell, err := exec.LookPath(test.shellName)
			if err != nil {
				t.Skipf("%s is not installed", test.shellName)
			}
			home := t.TempDir()
			overrides := []string{
				"PIN_DEV_HOME=" + filepath.Join(t.TempDir(), "pin-dev"),
				"SHELL=" + shell,
			}
			overrides = append(overrides, test.configure(t, home)...)

			command := exec.Command(script, "prompt-test")
			command.Dir = repo
			command.Env = envWith(os.Environ(), overrides...)
			command.Stdin = strings.NewReader("exit\n")
			output, err := command.CombinedOutput()
			if err != nil {
				t.Fatalf("dev-shell failed: %v\n%s", err, output)
			}
			if !strings.Contains(string(output), "[pin dev:prompt-test] user> ") {
				t.Fatalf("development prompt missing after startup files:\n%s", output)
			}
		})
	}
}

func TestDevScriptsRejectUnsafeProfileNames(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("development scripts are POSIX-only")
	}

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

func envWith(base []string, overrides ...string) []string {
	keys := make(map[string]struct{}, len(overrides))
	for _, entry := range overrides {
		if key, _, ok := strings.Cut(entry, "="); ok {
			keys[key] = struct{}{}
		}
	}

	env := make([]string, 0, len(base)+len(overrides))
	for _, entry := range base {
		key, _, ok := strings.Cut(entry, "=")
		if _, overridden := keys[key]; ok && overridden {
			continue
		}
		env = append(env, entry)
	}
	return append(env, overrides...)
}

func goEnv(t *testing.T, name string) string {
	t.Helper()
	command := exec.Command("go", "env", name)
	output, err := command.Output()
	if err != nil {
		t.Fatalf("go env %s failed: %v", name, err)
	}
	return strings.TrimSpace(string(output))
}
