package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func runSkillCLI(t *testing.T, home, input string, args ...string) cliResult {
	t.Helper()
	return runSkillCLIWithPath(t, home, "/usr/bin:/bin", input, args...)
}

func runSkillCLIWithPath(t *testing.T, home, path, input string, args ...string) cliResult {
	t.Helper()
	return runSkillCLIWithPathAndClaudeConfig(t, home, path, "", input, args...)
}

func runSkillCLIWithPathAndClaudeConfig(t *testing.T, home, path, claudeConfig, input string, args ...string) cliResult {
	t.Helper()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("RELAY_HOME", "")
	t.Setenv("CODEX_HOME", "")
	t.Setenv("CLAUDE_CONFIG_DIR", claudeConfig)
	t.Setenv("CURSOR_HOME", "")
	t.Setenv("OPENCODE_HOME", "")
	t.Setenv("PATH", path)
	var stdout, stderr bytes.Buffer
	code := runCLI(args, strings.NewReader(input), &stdout, &stderr)
	return cliResult{code: code, stdout: stdout.String(), stderr: stderr.String()}
}

func installFakeRelay(t *testing.T, home, mode string) (string, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake Relay executable uses a POSIX shell")
	}
	binDir := filepath.Join(home, "bin")
	relayPath := filepath.Join(binDir, "relay")
	logPath := filepath.Join(home, "relay-argv.log")
	script := `#!/bin/sh
	printf '%s' "$#" >> "$RELAY_TEST_LOG"
	for arg in "$@"; do
	  printf '\t%s' "$arg" >> "$RELAY_TEST_LOG"
	done
	printf '\n' >> "$RELAY_TEST_LOG"
if [ "$1" = "capabilities" ] && [ "$2" = "--json" ]; then
  case "$RELAY_TEST_MODE" in
    supported|sync-fail)
      printf '%s\n' '{"schema_version":1,"capabilities":{"skills.sync.scoped":1}}'
      exit 0
      ;;
    old)
      printf '%s\n' 'error: unrecognized subcommand capabilities' >&2
      exit 2
      ;;
    malformed)
      printf '%s\n' '{not-json'
      exit 0
      ;;
    absent)
      printf '%s\n' '{"schema_version":1,"capabilities":{}}'
      exit 0
      ;;
    unknown-schema)
      printf '%s\n' '{"schema_version":2,"capabilities":{"skills.sync.scoped":1}}'
      exit 0
      ;;
  esac
fi
if [ "$1" = "sync" ] && [ "$2" = "skill" ]; then
  if [ "$RELAY_TEST_MODE" = "sync-fail" ]; then
    printf '%s\n' 'scoped sync failed' >&2
    exit 9
  fi
  exit 0
fi
printf '%s\n' 'unexpected fake Relay invocation' >&2
exit 64
`
	writeFile(t, relayPath, script)
	if err := os.Chmod(relayPath, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RELAY_TEST_LOG", logPath)
	t.Setenv("RELAY_TEST_MODE", mode)
	return binDir + ":/usr/bin:/bin", logPath
}

func relayInvocations(t *testing.T, logPath string) [][]string {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.FieldsFunc(string(data), func(r rune) bool { return r == '\n' })
	invocations := make([][]string, 0, len(lines))
	for _, line := range lines {
		fields := strings.Split(line, "\t")
		if len(fields) == 0 {
			t.Fatalf("empty Relay invocation log line")
		}
		wantCount := len(fields) - 1
		if fields[0] != fmt.Sprintf("%d", wantCount) {
			t.Fatalf("Relay invocation count = %q, args=%#v", fields[0], fields[1:])
		}
		invocations = append(invocations, fields[1:])
	}
	return invocations
}

func assertNoBroadRelaySync(t *testing.T, invocations [][]string) {
	t.Helper()
	for _, invocation := range invocations {
		if len(invocation) > 0 && invocation[0] == "sync" && (len(invocation) < 2 || invocation[1] != "skill") {
			t.Fatalf("broad Relay sync was invoked: %q", invocation)
		}
	}
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

func TestSkillInstallAlwaysWritesCanonicalStore(t *testing.T) {
	home := t.TempDir()
	canonical := filepath.Join(home, ".agents", "skills", "pin")

	result := runSkillCLI(t, home, "", "skill", "install", "--yes")
	if result.code != 0 {
		t.Fatalf("install failed: %s", result.stderr)
	}
	requireContains(t, result.stdout, "installed skill: "+canonical)
	requireContains(t, mustReadFile(t, filepath.Join(canonical, "SKILL.md")), "name: pin")
	if _, err := os.Stat(filepath.Join(canonical, skillMarkerName)); err != nil {
		t.Fatalf("managed marker missing: %v", err)
	}
	if pathExists(filepath.Join(home, ".claude", "skills", "pin")) {
		t.Fatal("Claude compatibility copy installed without detecting Claude Code")
	}
}

func TestSkillInstallWithoutRelayUsesClaudeFallback(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	canonical := filepath.Join(home, ".agents", "skills", "pin")
	claude := filepath.Join(home, ".claude", "skills", "pin")

	result := runSkillCLI(t, home, "", "skill", "install", "--yes")
	if result.code != 0 {
		t.Fatalf("install failed: %s", result.stderr)
	}
	for _, path := range []string{canonical, claude} {
		if !pathExists(filepath.Join(path, "SKILL.md")) {
			t.Fatalf("skill missing from %s", path)
		}
	}
	requireContains(t, result.stdout, "installed compatibility copy: "+claude)
	if result.stderr != "" {
		t.Fatalf("unexpected warning without Relay: %s", result.stderr)
	}
}

func TestSkillInstallOldRelayFallsBackWithoutFailing(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	path, logPath := installFakeRelay(t, home, "old")

	result := runSkillCLIWithPath(t, home, path, "", "skill", "install", "--yes")
	if result.code != 0 {
		t.Fatalf("old Relay should not block install: %s", result.stderr)
	}
	if !pathExists(filepath.Join(home, ".claude", "skills", "pin", "SKILL.md")) {
		t.Fatal("standalone Claude fallback was not installed")
	}
	invocations := relayInvocations(t, logPath)
	if !reflect.DeepEqual(invocations, [][]string{{"capabilities", "--json"}}) {
		t.Fatalf("unexpected old Relay invocations: %#v", invocations)
	}
	assertNoBroadRelaySync(t, invocations)
	if result.stderr != "" {
		t.Fatalf("unsupported Relay should be a silent fallback: %s", result.stderr)
	}
}

func TestSkillInstallUnsupportedRelayCapabilityFallsBack(t *testing.T) {
	for _, mode := range []string{"malformed", "absent", "unknown-schema"} {
		t.Run(mode, func(t *testing.T) {
			home := t.TempDir()
			if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
				t.Fatal(err)
			}
			path, logPath := installFakeRelay(t, home, mode)

			result := runSkillCLIWithPath(t, home, path, "", "skill", "install", "--yes")
			if result.code != 0 {
				t.Fatalf("unsupported capability should not block install: %s", result.stderr)
			}
			if !pathExists(filepath.Join(home, ".claude", "skills", "pin", "SKILL.md")) {
				t.Fatal("standalone Claude fallback was not installed")
			}
			invocations := relayInvocations(t, logPath)
			if !reflect.DeepEqual(invocations, [][]string{{"capabilities", "--json"}}) {
				t.Fatalf("unexpected Relay invocations: %#v", invocations)
			}
			assertNoBroadRelaySync(t, invocations)
			if result.stderr != "" {
				t.Fatalf("unsupported capability should be silent: %s", result.stderr)
			}
		})
	}
}

func TestSkillInstallSupportedRelayUsesExactScopedArgv(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home with spaces")
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	path, logPath := installFakeRelay(t, home, "supported")
	canonical := filepath.Join(home, ".agents", "skills", "pin")

	result := runSkillCLIWithPath(t, home, path, "", "skill", "install", "--yes")
	if result.code != 0 {
		t.Fatalf("supported Relay install failed: %s", result.stderr)
	}
	invocations := relayInvocations(t, logPath)
	want := [][]string{
		{"capabilities", "--json"},
		{"sync", "skill", "--quiet", "--fail-on-conflict", canonical},
	}
	if !reflect.DeepEqual(invocations, want) {
		t.Fatalf("Relay argv = %#v, want %#v", invocations, want)
	}
	assertNoBroadRelaySync(t, invocations)
	if pathExists(filepath.Join(home, ".claude", "skills", "pin")) {
		t.Fatal("PIN wrote a direct Claude copy after successful scoped sync")
	}
	wantStdout := "installed skill: " + canonical + "\n"
	if result.stdout != wantStdout || result.stderr != "" {
		t.Fatalf("successful ambient integration was noisy: stdout=%q stderr=%q", result.stdout, result.stderr)
	}
}

func TestSkillInstallScopedRelayFailureUsesFallback(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	path, logPath := installFakeRelay(t, home, "sync-fail")
	canonical := filepath.Join(home, ".agents", "skills", "pin")
	claude := filepath.Join(home, ".claude", "skills", "pin")

	result := runSkillCLIWithPath(t, home, path, "", "skill", "install", "--yes")
	if result.code != 0 {
		t.Fatalf("optional Relay failure blocked install: %s", result.stderr)
	}
	for _, skill := range []string{canonical, claude} {
		if !pathExists(filepath.Join(skill, "SKILL.md")) {
			t.Fatalf("skill missing from %s", skill)
		}
	}
	requireContains(t, result.stderr, "Relay could not synchronize compatibility copies; used the standalone fallback")
	invocations := relayInvocations(t, logPath)
	want := [][]string{
		{"capabilities", "--json"},
		{"sync", "skill", "--quiet", "--fail-on-conflict", canonical},
	}
	if !reflect.DeepEqual(invocations, want) {
		t.Fatalf("unexpected Relay invocations: %#v", invocations)
	}
	assertNoBroadRelaySync(t, invocations)
}

func TestSkillInstallProtectsUnmanagedClaudeFallback(t *testing.T) {
	home := t.TempDir()
	canonical := filepath.Join(home, ".agents", "skills", "pin")
	claude := filepath.Join(home, ".claude", "skills", "pin")
	writeFile(t, filepath.Join(claude, "SKILL.md"), "user skill\n")

	result := runSkillCLI(t, home, "", "skill", "install", "--yes")
	if result.code == 0 {
		t.Fatal("install unexpectedly replaced an unmanaged Claude skill")
	}
	requireContains(t, result.stderr, "canonical skill remains installed")
	if !pathExists(filepath.Join(canonical, "SKILL.md")) {
		t.Fatal("canonical skill was lost when fallback failed")
	}
	if got := mustReadFile(t, filepath.Join(claude, "SKILL.md")); got != "user skill\n" {
		t.Fatalf("unmanaged skill changed: %q", got)
	}
}

func TestSkillLifecycleProtectsModifiedCopiesAndPreflightsRemoval(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	canonical := filepath.Join(home, ".agents", "skills", "pin")
	claude := filepath.Join(home, ".claude", "skills", "pin")
	result := runSkillCLI(t, home, "", "skill", "install", "--yes")
	if result.code != 0 {
		t.Fatalf("install failed: %s", result.stderr)
	}
	appendFile(t, filepath.Join(claude, "SKILL.md"), "\nlocal edit\n")

	result = runSkillCLI(t, home, "", "skill", "remove", "--yes")
	if result.code == 0 {
		t.Fatal("remove unexpectedly deleted a modified compatibility copy")
	}
	if !pathExists(canonical) || !pathExists(claude) {
		t.Fatal("removal was not preflighted before deleting copies")
	}
	requireContains(t, result.stderr, "skill is modified")

	result = runSkillCLI(t, home, "", "skill", "remove", "--yes", "--force")
	if result.code != 0 {
		t.Fatalf("forced remove failed: %s", result.stderr)
	}
	if pathExists(canonical) || pathExists(claude) {
		t.Fatal("forced remove left a managed skill copy")
	}
}

func TestSkillRemoveDoesNotInvokeRelay(t *testing.T) {
	home := t.TempDir()
	path, logPath := installFakeRelay(t, home, "supported")
	result := runSkillCLIWithPath(t, home, path, "", "skill", "install", "--yes")
	if result.code != 0 {
		t.Fatalf("install failed: %s", result.stderr)
	}
	before := relayInvocations(t, logPath)

	result = runSkillCLIWithPath(t, home, path, "", "skill", "remove", "--yes")
	if result.code != 0 {
		t.Fatalf("remove failed: %s", result.stderr)
	}
	after := relayInvocations(t, logPath)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("remove invoked Relay: before=%#v after=%#v", before, after)
	}
}

func TestSkillRemoveDefaultsToNo(t *testing.T) {
	home := t.TempDir()
	canonical := filepath.Join(home, ".agents", "skills", "pin")
	result := runSkillCLI(t, home, "", "skill", "install", "--yes")
	if result.code != 0 {
		t.Fatalf("install failed: %s", result.stderr)
	}

	result = runSkillCLI(t, home, "\n", "skill", "remove")
	if result.code != 0 {
		t.Fatalf("cancel failed: %s", result.stderr)
	}
	requireContains(t, result.stdout, "[y/N]")
	requireContains(t, result.stdout, "cancelled")
	if strings.Contains(result.stdout, "compatibility copy") {
		t.Fatalf("remove prompt mentioned a missing compatibility copy: %q", result.stdout)
	}
	if !pathExists(canonical) {
		t.Fatal("default removal confirmation deleted the skill")
	}
}

func TestSkillRemovePromptIncludesExistingCompatibilityCopy(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	result := runSkillCLI(t, home, "", "skill", "install", "--yes")
	if result.code != 0 {
		t.Fatalf("install failed: %s", result.stderr)
	}

	result = runSkillCLI(t, home, "n\n", "skill", "remove")
	if result.code != 0 {
		t.Fatalf("cancel failed: %s", result.stderr)
	}
	requireContains(t, result.stdout, "and its standard compatibility copy")
}

func TestSkillRemoveRollsBackWhenSecondTargetCannotBeStaged(t *testing.T) {
	home := t.TempDir()
	canonical := filepath.Join(home, ".agents", "skills", "pin")
	compatibility := filepath.Join(home, ".claude", "skills", "pin")
	for _, path := range []string{canonical, compatibility} {
		if _, err := installSkillAt(path, false); err != nil {
			t.Fatal(err)
		}
	}

	rename := func(oldPath, newPath string) error {
		if oldPath == compatibility {
			return errors.New("injected compatibility removal failure")
		}
		return os.Rename(oldPath, newPath)
	}
	removed, err := removeSkillTargetsWithOps(
		[]skillTarget{{path: canonical}, {path: compatibility}},
		false,
		rename,
		os.RemoveAll,
	)
	if err == nil {
		t.Fatal("removal unexpectedly succeeded")
	}
	if len(removed) != 0 {
		t.Fatalf("removed paths = %#v, want none", removed)
	}
	for _, path := range []string{canonical, compatibility} {
		state, inspectErr := inspectSkillAt(path)
		if inspectErr != nil || state != skillCurrent {
			t.Fatalf("restored state for %s = %s, err=%v", path, state, inspectErr)
		}
		matches, globErr := filepath.Glob(filepath.Join(filepath.Dir(path), ".pin-skill-remove-*"))
		if globErr != nil {
			t.Fatal(globErr)
		}
		if len(matches) != 0 {
			t.Fatalf("rollback left temporary paths for %s: %#v", path, matches)
		}
	}
}

func TestSkillInstallRequiresInteractiveConfirmation(t *testing.T) {
	home := t.TempDir()
	result := runSkillCLI(t, home, "", "skill", "install")
	if result.code == 0 {
		t.Fatal("install unexpectedly proceeded without confirmation")
	}
	requireContains(t, result.stderr, "confirmation required; rerun with --yes")
	if pathExists(filepath.Join(home, ".agents", "skills", "pin")) {
		t.Fatal("skill installed without confirmation")
	}
}

func TestSkillInstallConfirmationIncludesPossibleClaudeCopy(t *testing.T) {
	home := t.TempDir()
	claudeConfig := filepath.Join(t.TempDir(), "claude config")
	result := runSkillCLIWithPathAndClaudeConfig(t, home, "/usr/bin:/bin", claudeConfig, "n\n", "skill", "install")
	if result.code != 0 {
		t.Fatalf("cancel failed: %s", result.stderr)
	}
	requireContains(t, result.stdout, "Claude Code compatibility copy at "+filepath.Join(claudeConfig, "skills", "pin"))
	if pathExists(filepath.Join(home, ".agents", "skills", "pin")) || pathExists(filepath.Join(claudeConfig, "skills", "pin")) {
		t.Fatal("cancelled confirmation installed a skill")
	}
}

func TestSkillInstallHonorsClaudeConfigDir(t *testing.T) {
	home := t.TempDir()
	claudeConfig := filepath.Join(t.TempDir(), "claude config")
	result := runSkillCLIWithPathAndClaudeConfig(t, home, "/usr/bin:/bin", claudeConfig, "", "skill", "install", "--yes")
	if result.code != 0 {
		t.Fatalf("install failed: %s", result.stderr)
	}
	if !pathExists(filepath.Join(claudeConfig, "skills", "pin", "SKILL.md")) {
		t.Fatal("compatibility copy did not use CLAUDE_CONFIG_DIR")
	}
	if pathExists(filepath.Join(home, ".claude", "skills", "pin")) {
		t.Fatal("compatibility copy unexpectedly used the default Claude config directory")
	}
}

func TestSkillStatusReportsCanonicalAndClaudeCompatibility(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	result := runSkillCLI(t, home, "", "skill", "install", "--yes")
	if result.code != 0 {
		t.Fatalf("install failed: %s", result.stderr)
	}
	result = runSkillCLI(t, home, "", "skill", "status")
	if result.code != 0 {
		t.Fatalf("status failed: %s", result.stderr)
	}
	requireContains(t, result.stdout, "pin: current path="+filepath.Join(home, ".agents", "skills", "pin"))
	requireContains(t, result.stdout, "claude compatibility: current detected=yes path="+filepath.Join(home, ".claude", "skills", "pin"))
}

func TestSkillInstallUpdatesUnmodifiedManagedSkill(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".agents", "skills", "pin")
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
	result := runSkillCLI(t, home, "", "skill", "install", "--yes")
	if result.code != 0 {
		t.Fatalf("managed update failed: %s", result.stderr)
	}
	if state, err := inspectSkillAt(path); err != nil || state != skillCurrent {
		t.Fatalf("state after update = %s, err=%v", state, err)
	}
}

func TestSkillPackageIdentityProtectsAddedEntries(t *testing.T) {
	tests := []struct {
		name string
		add  func(t *testing.T, path string)
	}{
		{
			name: "extra file",
			add: func(t *testing.T, path string) {
				writeFile(t, filepath.Join(path, "notes.txt"), "local notes\n")
			},
		},
		{
			name: "nested content",
			add: func(t *testing.T, path string) {
				writeFile(t, filepath.Join(path, "scripts", "check.sh"), "#!/bin/sh\n")
			},
		},
		{
			name: "empty directory",
			add: func(t *testing.T, path string) {
				if err := os.MkdirAll(filepath.Join(path, "assets", "empty"), 0o755); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), ".agents", "skills", "pin")
			if _, err := installSkillAt(path, false); err != nil {
				t.Fatal(err)
			}
			test.add(t, path)

			state, err := inspectSkillAt(path)
			if err != nil {
				t.Fatal(err)
			}
			if state != skillModified {
				t.Fatalf("state = %s, want modified", state)
			}
			if _, err := installSkillAt(path, false); err == nil {
				t.Fatal("update unexpectedly replaced a package with local additions")
			}
			if _, err := removeSkillAt(path, false); err == nil {
				t.Fatal("remove unexpectedly deleted a package with local additions")
			}
			if !pathExists(path) {
				t.Fatal("protected package was deleted")
			}
		})
	}
}

func TestSkillPackageIdentityProtectsEditedSupportingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".agents", "skills", "pin")
	if err := writeBundledSkill(path); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(path, "scripts", "check.sh"), "original\n")
	digest, err := skillPackageDigest(path)
	if err != nil {
		t.Fatal(err)
	}
	marker := skillMarker{Schema: skillMarkerSchema, Skill: pinSkillName, Digest: digest}
	markerData, err := json.Marshal(marker)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(path, skillMarkerName), string(markerData))
	appendFile(t, filepath.Join(path, "scripts", "check.sh"), "local edit\n")

	state, err := inspectSkillAt(path)
	if err != nil {
		t.Fatal(err)
	}
	if state != skillModified {
		t.Fatalf("state = %s, want modified", state)
	}
	if _, err := removeSkillAt(path, false); err == nil {
		t.Fatal("remove unexpectedly deleted an edited supporting file")
	}
}

func TestLegacySkillMarkerWithExtraEntryIsModified(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".agents", "skills", "pin")
	writeFile(t, filepath.Join(path, "SKILL.md"), string(embeddedPinSkill))
	marker := `{"schema":1,"skill":"pin","digest":"` + skillDigest(embeddedPinSkill) + `"}`
	writeFile(t, filepath.Join(path, skillMarkerName), marker)
	writeFile(t, filepath.Join(path, "local.txt"), "keep me\n")

	state, err := inspectSkillAt(path)
	if err != nil {
		t.Fatal(err)
	}
	if state != skillModified {
		t.Fatalf("state = %s, want modified", state)
	}
	if _, err := installSkillAt(path, false); err == nil {
		t.Fatal("legacy package with an extra entry was auto-migrated")
	}
	if _, err := removeSkillAt(path, false); err == nil {
		t.Fatal("legacy package with an extra entry was removed")
	}
}

func TestSkillPackageSymlinkIsModified(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink setup requires additional Windows privileges")
	}
	path := filepath.Join(t.TempDir(), ".agents", "skills", "pin")
	if _, err := installSkillAt(path, false); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("SKILL.md", filepath.Join(path, "linked-skill")); err != nil {
		t.Fatal(err)
	}
	state, err := inspectSkillAt(path)
	if err != nil {
		t.Fatal(err)
	}
	if state != skillModified {
		t.Fatalf("state = %s, want modified", state)
	}
}

func TestSkillInstallPublishFailureRestoresPreviousPackage(t *testing.T) {
	path := writeOutdatedLegacySkill(t)
	renameCalls := 0
	rename := func(oldPath, newPath string) error {
		renameCalls++
		if renameCalls == 2 {
			return errors.New("injected publish failure")
		}
		return os.Rename(oldPath, newPath)
	}

	if _, err := installSkillAtWithRename(path, false, rename); err == nil {
		t.Fatal("install unexpectedly succeeded")
	}
	if got := mustReadFile(t, filepath.Join(path, "SKILL.md")); !strings.Contains(got, "Old instructions") {
		t.Fatalf("previous package was not restored: %q", got)
	}
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".pin-skill-install-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("temporary install directory was not cleaned up: %#v", matches)
	}
}

func TestSkillInstallDoubleRenameFailurePreservesRecoveryPath(t *testing.T) {
	path := writeOutdatedLegacySkill(t)
	renameCalls := 0
	rename := func(oldPath, newPath string) error {
		renameCalls++
		if renameCalls >= 2 {
			return fmt.Errorf("injected rename failure %d", renameCalls)
		}
		return os.Rename(oldPath, newPath)
	}

	_, installErr := installSkillAtWithRename(path, false, rename)
	if installErr == nil {
		t.Fatal("install unexpectedly succeeded")
	}
	recoveryPaths, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".pin-skill-install-*", "previous"))
	if err != nil {
		t.Fatal(err)
	}
	if len(recoveryPaths) != 1 {
		t.Fatalf("recovery paths = %#v, want one", recoveryPaths)
	}
	if !strings.Contains(installErr.Error(), recoveryPaths[0]) {
		t.Fatalf("error %q does not report recovery path %q", installErr, recoveryPaths[0])
	}
	if got := mustReadFile(t, filepath.Join(recoveryPaths[0], "SKILL.md")); !strings.Contains(got, "Old instructions") {
		t.Fatalf("preserved package is wrong: %q", got)
	}
}

func writeOutdatedLegacySkill(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), ".agents", "skills", "pin")
	oldSkill := []byte("---\nname: pin\ndescription: old\n---\n\nOld instructions.\n")
	writeFile(t, filepath.Join(path, "SKILL.md"), string(oldSkill))
	marker := `{"schema":1,"skill":"pin","digest":"` + skillDigest(oldSkill) + `"}`
	writeFile(t, filepath.Join(path, skillMarkerName), marker)
	return path
}

func TestSkillForcedRemoveDoesNotFollowSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink setup requires additional Windows privileges")
	}
	home := t.TempDir()
	target := filepath.Join(home, "user-skill")
	writeFile(t, filepath.Join(target, "SKILL.md"), "keep me\n")
	path := filepath.Join(home, ".agents", "skills", "pin")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}

	result := runSkillCLI(t, home, "", "skill", "remove", "--yes", "--force")
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

func TestSkillCommandHelp(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runCLI([]string{"skill", "install", "--help"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("help failed: %s", stderr.String())
	}
	requireContains(t, stdout.String(), "Usage: pin skill install [--yes] [--force]")
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
		command.Env = append(os.Environ(), "HOME="+home, "PATH=/usr/bin:/bin", "XDG_CONFIG_HOME=", "CLAUDE_CONFIG_DIR=")
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

	result := runBinary("skill", "install", "--yes")
	if result.code != 0 {
		t.Fatalf("compiled install failed: %s", result.stderr)
	}
	result = runBinary("skill", "status")
	requireContains(t, result.stdout, "pin: current")
	result = runBinary("skill", "remove", "--yes")
	if result.code != 0 {
		t.Fatalf("compiled remove failed: %s", result.stderr)
	}
	if pathExists(filepath.Join(home, ".agents", "skills", "pin")) {
		t.Fatal("compiled remove left the skill installed")
	}
}
