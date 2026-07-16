package app

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func runSkillCLI(t *testing.T, home, input string, args ...string) cliResult {
	t.Helper()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("RELAY_HOME", "")
	t.Setenv("CODEX_HOME", "")
	t.Setenv("CLAUDE_HOME", "")
	t.Setenv("CURSOR_HOME", "")
	t.Setenv("OPENCODE_HOME", "")
	t.Setenv("PATH", "/usr/bin:/bin")
	var stdout, stderr bytes.Buffer
	code := runCLI(args, strings.NewReader(input), &stdout, &stderr)
	return cliResult{code: code, stdout: stdout.String(), stderr: stderr.String()}
}

func TestEmbeddedPinSkillHasPortableFrontmatter(t *testing.T) {
	text := string(embeddedPinSkill)
	if !strings.HasPrefix(text, "---\nname: pin\ndescription: ") {
		t.Fatalf("unexpected skill frontmatter:\n%s", text)
	}
	if strings.Count(text, "\n---\n") != 1 {
		t.Fatalf("skill must have one closing frontmatter delimiter:\n%s", text)
	}
}

func TestSkillDirectLifecycle(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".agents", "skills", "pin")

	result := runSkillCLI(t, home, "", "skill", "install", "--target", "codex")
	if result.code != 0 {
		t.Fatalf("install failed: %s", result.stderr)
	}
	requireContains(t, result.stdout, "installed: codex "+path)
	requireContains(t, mustReadFile(t, filepath.Join(path, "SKILL.md")), "name: pin")
	if _, err := os.Stat(filepath.Join(path, skillMarkerName)); err != nil {
		t.Fatalf("managed marker missing: %v", err)
	}

	result = runSkillCLI(t, home, "", "skill", "status", "--target", "codex")
	if result.code != 0 {
		t.Fatalf("status failed: %s", result.stderr)
	}
	requireContains(t, result.stdout, "codex: current detected=yes path="+path)

	result = runSkillCLI(t, home, "", "skill", "install", "--target", "codex")
	if result.code != 0 {
		t.Fatalf("repeat install failed: %s", result.stderr)
	}
	requireContains(t, result.stdout, "unchanged: codex "+path)

	appendFile(t, filepath.Join(path, "SKILL.md"), "\nlocal edit\n")
	result = runSkillCLI(t, home, "", "skill", "status", "--target", "codex")
	requireContains(t, result.stdout, "codex: modified")

	result = runSkillCLI(t, home, "", "skill", "remove", "--target", "codex")
	if result.code == 0 {
		t.Fatal("remove unexpectedly replaced a locally modified skill")
	}
	requireContains(t, result.stderr, "skill is modified")

	result = runSkillCLI(t, home, "", "skill", "remove", "--target", "codex", "--force")
	if result.code != 0 {
		t.Fatalf("forced remove failed: %s", result.stderr)
	}
	requireContains(t, result.stdout, "removed: codex "+path)
	if pathExists(path) {
		t.Fatal("skill still exists after remove")
	}
}

func TestSkillInstallProtectsUnmanagedSkill(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".claude", "skills", "pin")
	writeFile(t, filepath.Join(path, "SKILL.md"), "user skill\n")

	result := runSkillCLI(t, home, "", "skill", "install", "--target", "claude")
	if result.code == 0 {
		t.Fatal("install unexpectedly replaced an unmanaged skill")
	}
	requireContains(t, result.stderr, "already exists and is unmanaged")
	if got := mustReadFile(t, filepath.Join(path, "SKILL.md")); got != "user skill\n" {
		t.Fatalf("unmanaged skill changed: %q", got)
	}

	result = runSkillCLI(t, home, "", "skill", "install", "--target", "claude", "--force")
	if result.code != 0 {
		t.Fatalf("forced install failed: %s", result.stderr)
	}
	requireContains(t, mustReadFile(t, filepath.Join(path, "SKILL.md")), "name: pin")
}

func TestSkillInstallUpdatesUnmodifiedManagedSkill(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".cursor", "skills", "pin")
	oldSkill := []byte("---\nname: pin\ndescription: old\n---\n\nOld instructions.\n")
	writeFile(t, filepath.Join(path, "SKILL.md"), string(oldSkill))
	marker := `{"schema":1,"skill":"pin","digest":"` + skillDigest(oldSkill) + `"}`
	writeFile(t, filepath.Join(path, skillMarkerName), marker)

	state, err := inspectSkillAt(path)
	if err != nil {
		t.Fatal(err)
	}
	if state != skillOutdated {
		t.Fatalf("state = %s, want outdated", state)
	}
	result := runSkillCLI(t, home, "", "skill", "install", "--target", "cursor")
	if result.code != 0 {
		t.Fatalf("managed update failed: %s", result.stderr)
	}
	requireContains(t, result.stdout, "installed: cursor "+path)
	if state, err := inspectSkillAt(path); err != nil || state != skillCurrent {
		t.Fatalf("state after update = %s, err=%v", state, err)
	}
}

func TestSkillForcedRemoveDoesNotFollowSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink setup requires additional Windows privileges")
	}
	home := t.TempDir()
	target := filepath.Join(home, "user-skill")
	writeFile(t, filepath.Join(target, "SKILL.md"), "keep me\n")
	path := filepath.Join(home, ".cursor", "skills", "pin")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}

	result := runSkillCLI(t, home, "", "skill", "remove", "--target", "cursor", "--force")
	if result.code != 0 {
		t.Fatalf("forced symlink removal failed: %s", result.stderr)
	}
	if pathExists(path) {
		t.Fatal("skill symlink still exists")
	}
	if got := mustReadFile(t, filepath.Join(target, "SKILL.md")); got != "keep me\n" {
		t.Fatalf("symlink target changed: %q", got)
	}
}

func TestSkillDefaultDetectionPromptsForAllDirectAgents(t *testing.T) {
	home := t.TempDir()
	for _, dir := range []string{".codex", ".claude", ".cursor"} {
		if err := os.MkdirAll(filepath.Join(home, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	result := runSkillCLI(t, home, "n\n", "skill", "install")
	if result.code != 0 {
		t.Fatalf("cancel failed: %s", result.stderr)
	}
	requireContains(t, result.stdout, "Detected: Codex, Claude Code, Cursor.")
	requireContains(t, result.stdout, "cancelled")
	if pathExists(filepath.Join(home, ".agents", "skills", "pin")) {
		t.Fatal("skill installed after cancellation")
	}

	result = runSkillCLI(t, home, "\n", "skill", "install")
	if result.code != 0 {
		t.Fatalf("confirmed install failed: %s", result.stderr)
	}
	for _, path := range []string{
		filepath.Join(home, ".agents", "skills", "pin"),
		filepath.Join(home, ".claude", "skills", "pin"),
		filepath.Join(home, ".cursor", "skills", "pin"),
	} {
		if !pathExists(filepath.Join(path, "SKILL.md")) {
			t.Fatalf("skill missing from %s", path)
		}
	}
}

func TestSkillDefaultMutationRequiresInteractiveConfirmation(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o755); err != nil {
		t.Fatal(err)
	}

	result := runSkillCLI(t, home, "", "skill", "install")
	if result.code == 0 {
		t.Fatal("install unexpectedly proceeded without confirmation")
	}
	requireContains(t, result.stderr, "confirmation required")
}

func TestSkillRelayTakesPrecedenceAndRemovalCleansManagedCopies(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Relay process locking is Unix-only")
	}
	home := t.TempDir()
	configRoot := filepath.Join(home, ".config", "relay")
	central := filepath.Join(configRoot, "skills")
	claudeSkills := filepath.Join(home, "relay-targets", "claude")
	codexSkills := filepath.Join(home, "relay-targets", "codex")
	opencodeSkills := filepath.Join(home, "relay-targets", "opencode")
	config := `enabled_tools = ["claude", "codex", "opencode"]
central_skills_dir = "` + central + `"
claude_skills_dir = "` + claudeSkills + `"
codex_skills_dir = "` + codexSkills + `"
opencode_skills_dir = "` + opencodeSkills + `"
`
	writeFile(t, filepath.Join(configRoot, "config.toml"), config)
	for _, dir := range []string{".codex", ".claude", ".cursor"} {
		if err := os.MkdirAll(filepath.Join(home, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	binDir := filepath.Join(home, "bin")
	relayLog := filepath.Join(home, "relay.log")
	writeFile(t, filepath.Join(binDir, "relay"), "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$RELAY_TEST_LOG\"\n")
	if err := os.Chmod(filepath.Join(binDir, "relay"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RELAY_TEST_LOG", relayLog)

	t.Setenv("PATH", binDir+":/usr/bin:/bin")
	result := runSkillCLIWithCurrentEnv(t, home, "", "skill", "install", "--yes")
	if result.code != 0 {
		t.Fatalf("Relay install failed: %s", result.stderr)
	}
	requireContains(t, result.stdout, "installed: relay "+filepath.Join(central, "pin"))
	if got := mustReadFile(t, relayLog); got != "sync\n" {
		t.Fatalf("unexpected Relay invocation: %q", got)
	}
	if !pathExists(filepath.Join(central, "pin", "SKILL.md")) {
		t.Fatal("central Relay skill missing")
	}
	if pathExists(filepath.Join(home, ".agents", "skills", "pin")) || pathExists(filepath.Join(home, ".cursor", "skills", "pin")) {
		t.Fatal("PIN bypassed Relay during default installation")
	}

	for _, base := range []string{claudeSkills, codexSkills, opencodeSkills} {
		if err := writeBundledSkill(filepath.Join(base, "pin")); err != nil {
			t.Fatal(err)
		}
	}
	result = runSkillCLIWithCurrentEnv(t, home, "", "skill", "remove", "--yes")
	if result.code != 0 {
		t.Fatalf("Relay remove failed: %s", result.stderr)
	}
	for _, base := range []string{central, claudeSkills, codexSkills, opencodeSkills} {
		if pathExists(filepath.Join(base, "pin")) {
			t.Fatalf("Relay-managed copy still exists under %s", base)
		}
	}
	if !pathExists(filepath.Join(configRoot, "runtime", "relay.lock")) {
		t.Fatal("Relay-compatible process lock was not used")
	}
}

func runSkillCLIWithCurrentEnv(t *testing.T, home, input string, args ...string) cliResult {
	t.Helper()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("RELAY_HOME", "")
	t.Setenv("CODEX_HOME", "")
	t.Setenv("CLAUDE_HOME", "")
	t.Setenv("CURSOR_HOME", "")
	t.Setenv("OPENCODE_HOME", "")
	var stdout, stderr bytes.Buffer
	code := runCLI(args, strings.NewReader(input), &stdout, &stderr)
	return cliResult{code: code, stdout: stdout.String(), stderr: stderr.String()}
}

func TestSkillInstallRejectsConfiguredRelayWithoutExecutable(t *testing.T) {
	home := t.TempDir()
	configRoot := filepath.Join(home, ".config", "relay")
	writeFile(t, filepath.Join(configRoot, "config.toml"), "central_skills_dir = \""+filepath.Join(configRoot, "skills")+"\"\n")

	result := runSkillCLI(t, home, "", "skill", "install", "--yes")
	if result.code == 0 {
		t.Fatal("install unexpectedly succeeded without Relay executable")
	}
	requireContains(t, result.stderr, "relay executable is not in PATH")
	if pathExists(filepath.Join(configRoot, "skills", "pin")) {
		t.Fatal("skill source was written before checking Relay executable")
	}
}

func TestSkillRelayRemovePreflightsEveryCopy(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Relay process locking is Unix-only")
	}
	home := t.TempDir()
	configRoot := filepath.Join(home, ".config", "relay")
	central := filepath.Join(configRoot, "skills")
	codexSkills := filepath.Join(home, "relay-targets", "codex")
	config := `enabled_tools = ["codex"]
central_skills_dir = "` + central + `"
codex_skills_dir = "` + codexSkills + `"
`
	writeFile(t, filepath.Join(configRoot, "config.toml"), config)
	if err := writeBundledSkill(filepath.Join(central, "pin")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(codexSkills, "pin", "SKILL.md"), "unmanaged copy\n")

	result := runSkillCLI(t, home, "", "skill", "remove", "--target", "relay")
	if result.code == 0 {
		t.Fatal("Relay remove unexpectedly deleted an unmanaged copy")
	}
	requireContains(t, result.stderr, "is unmanaged")
	if !pathExists(filepath.Join(central, "pin")) {
		t.Fatal("central skill was removed before preflight completed")
	}
	if !pathExists(filepath.Join(codexSkills, "pin")) {
		t.Fatal("unmanaged Relay copy was removed")
	}
}

func TestSkillStatusRejectsUnsupportedRelayPathSyntax(t *testing.T) {
	home := t.TempDir()
	configRoot := filepath.Join(home, ".config", "relay")
	writeFile(t, filepath.Join(configRoot, "config.toml"), "central_skills_dir = \"$UNSUPPORTED/skills\"\n")

	result := runSkillCLI(t, home, "", "skill", "status")
	if result.code == 0 {
		t.Fatal("status unexpectedly accepted unsupported Relay path syntax")
	}
	requireContains(t, result.stderr, "unsupported shell syntax")
}

func TestSkillCommandHelp(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runCLI([]string{"skill", "install", "--help"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("help failed: %s", stderr.String())
	}
	requireContains(t, stdout.String(), "Usage: pin skill install")
}

func TestE2ESkillLifecycleCompiledBinary(t *testing.T) {
	home := t.TempDir()
	bin := filepath.Join(home, "pin")
	build := exec.Command("go", "build", "-o", bin, "..")
	build.Dir = "."
	var buildStderr bytes.Buffer
	build.Stderr = &buildStderr
	if err := build.Run(); err != nil {
		t.Fatalf("build failed: %v: %s", err, buildStderr.String())
	}

	runBinary := func(args ...string) cliResult {
		command := exec.Command(bin, args...)
		command.Env = append(os.Environ(), "HOME="+home, "PATH=/usr/bin:/bin", "XDG_CONFIG_HOME=")
		var stdout, stderr bytes.Buffer
		command.Stdout = &stdout
		command.Stderr = &stderr
		err := command.Run()
		code := 0
		if err != nil {
			if exit, ok := err.(*exec.ExitError); ok {
				code = exit.ExitCode()
			} else {
				t.Fatalf("binary failed: %v", err)
			}
		}
		return cliResult{code: code, stdout: stdout.String(), stderr: stderr.String()}
	}

	result := runBinary("skill", "install", "--target", "codex")
	if result.code != 0 {
		t.Fatalf("compiled install failed: %s", result.stderr)
	}
	result = runBinary("skill", "status", "--target", "codex")
	requireContains(t, result.stdout, "codex: current")
	result = runBinary("skill", "remove", "--target", "codex")
	if result.code != 0 {
		t.Fatalf("compiled remove failed: %s", result.stderr)
	}
	if pathExists(filepath.Join(home, ".agents", "skills", "pin")) {
		t.Fatal("compiled remove left the skill installed")
	}
}
