package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type cliResult struct {
	code   int
	stdout string
	stderr string
}

type pinRunner func(t *testing.T, root string, args ...string) cliResult

func runPin(t *testing.T, root string, args ...string) cliResult {
	t.Helper()
	prepareToolEnv(t, root)
	var stdout, stderr bytes.Buffer
	allArgs := append([]string{"--pin-home", filepath.Join(root, "share"), "--pin-bin", filepath.Join(root, "bin")}, args...)
	code := runCLI(allArgs, &stdout, &stderr)
	return cliResult{code: code, stdout: stdout.String(), stderr: stderr.String()}
}

func runCompiledPin(t *testing.T, root string, args ...string) cliResult {
	t.Helper()
	prepareToolEnv(t, root)
	bin := filepath.Join(root, "pin")
	if _, err := os.Stat(bin); err != nil {
		run(t, "", "go", "build", "-o", bin, ".")
	}
	allArgs := append([]string{"--pin-home", filepath.Join(root, "share"), "--pin-bin", filepath.Join(root, "bin")}, args...)
	command := exec.Command(bin, allArgs...)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	code := 0
	if err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			code = exit.ExitCode()
		} else {
			t.Fatalf("pin failed: %v", err)
		}
	}
	return cliResult{code: code, stdout: stdout.String(), stderr: stderr.String()}
}

func prepareToolEnv(t *testing.T, root string) {
	t.Helper()
	t.Setenv("UV_CACHE_DIR", filepath.Join(root, "uv-cache"))
	t.Setenv("PIP_CACHE_DIR", filepath.Join(root, "pip-cache"))
}

func runTool(t *testing.T, runner pinRunner, root, repo, command string) cliResult {
	t.Helper()
	return runner(t, root, command, repo)
}

func git(t *testing.T, repo string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = repo
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		t.Fatalf("git %s failed: %v: %s", strings.Join(args, " "), err, stderr.String())
	}
	return strings.TrimSpace(string(output))
}

func writeFile(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeTool(t *testing.T, repo, version string) {
	t.Helper()
	writeFile(t, filepath.Join(repo, "setup.py"), `from setuptools import setup

setup(
    name="demo-tool",
    version="0.1.0",
    py_modules=["demo_tool"],
    entry_points={"console_scripts": ["demo-tool=demo_tool:main"]},
)
`)
	writeFile(t, filepath.Join(repo, "demo_tool.py"), `def main():
    print("demo `+version+`")
    return 0
`)
	writeFile(t, filepath.Join(repo, "pin.toml"), `name = "demo-tool"
entrypoint = "demo-tool"
branch = "main"
remote = "origin"
preflight = [["python3", "-c", "from pathlib import Path; assert Path('demo_tool.py').is_file()"]]
verify = ["demo-tool"]
link = true
`)
}

func replacePinValue(t *testing.T, repo, key, oldValue, newValue string) {
	t.Helper()
	replaceInFile(t, filepath.Join(repo, "pin.toml"), key+` = "`+oldValue+`"`, key+` = "`+newValue+`"`)
}

func replaceInFile(t *testing.T, path, old, new string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	updated := strings.Replace(string(data), old, new, 1)
	if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
		t.Fatal(err)
	}
}

func commitToolVersion(t *testing.T, repo, version string, amend bool) string {
	t.Helper()
	writeTool(t, repo, version)
	git(t, repo, "add", ".")
	args := []string{"commit", "-m", "update"}
	if amend {
		args = []string{"commit", "--amend", "-m", "rewrite history"}
	}
	git(t, repo, args...)
	return git(t, repo, "rev-parse", "HEAD")
}

func forceRemoteMain(t *testing.T, repo, sha string) {
	t.Helper()
	git(t, repo, "push", "--force", "origin", sha+":main")
}

func sourceRepo(t *testing.T, root string) (string, string) {
	t.Helper()
	remote := filepath.Join(root, "remote.git")
	run(t, "", "git", "init", "--bare", remote)
	repo := filepath.Join(root, "repo")
	run(t, "", "git", "clone", remote, repo)
	git(t, repo, "config", "user.email", "pin@example.test")
	git(t, repo, "config", "user.name", "Pin Test")
	writeTool(t, repo, "1")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "initial")
	git(t, repo, "branch", "-M", "main")
	git(t, repo, "push", "-u", "origin", "main")
	return repo, git(t, repo, "rev-parse", "HEAD")
}

func run(t *testing.T, cwd string, name string, args ...string) string {
	t.Helper()
	command := exec.Command(name, args...)
	if cwd != "" {
		command.Dir = cwd
	}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		t.Fatalf("%s %s failed: %v: %s", name, strings.Join(args, " "), err, stderr.String())
	}
	return strings.TrimSpace(string(output))
}

func requireCode(t *testing.T, result cliResult, want int) {
	t.Helper()
	if result.code != want {
		t.Fatalf("exit code = %d, want %d\nstdout:\n%s\nstderr:\n%s", result.code, want, result.stdout, result.stderr)
	}
}

func requireContains(t *testing.T, text, want string) {
	t.Helper()
	if !strings.Contains(text, want) {
		t.Fatalf("expected output to contain %q\noutput:\n%s", want, text)
	}
}

func requireInstalledVersion(t *testing.T, root, version string) {
	t.Helper()
	output := run(t, "", filepath.Join(root, "bin", "demo-tool"))
	if output != "demo "+version {
		t.Fatalf("demo-tool output = %q, want %q", output, "demo "+version)
	}
}

func requireReleaseLink(t *testing.T, root, linkName, sha string) {
	t.Helper()
	target, err := filepath.EvalSymlinks(filepath.Join(root, "share", "demo-tool", linkName))
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(target) != sha {
		t.Fatalf("%s points to %s, want %s", linkName, filepath.Base(target), sha)
	}
}

func requireReleaseMetadata(t *testing.T, root, sha string) {
	t.Helper()
	path := filepath.Join(root, "share", "demo-tool", "releases", sha, ".pin", "release.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var metadata map[string]any
	if err := json.Unmarshal(data, &metadata); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		"tool":       "demo-tool",
		"entrypoint": "demo-tool",
		"git_sha":    sha,
		"branch":     "main",
		"remote":     "origin",
	} {
		if got, _ := metadata[key].(string); got != want {
			t.Fatalf("metadata[%s] = %q, want %q", key, got, want)
		}
	}
	if _, ok := metadata["config"].(map[string]any); !ok {
		t.Fatalf("metadata config is missing or invalid: %#v", metadata["config"])
	}
}

func TestUpdateStatusVerifyAndList(t *testing.T) {
	testUpdateStatusVerifyAndList(t, runPin)
}

func TestCompiledBinaryUpdateStatusVerifyAndList(t *testing.T) {
	testUpdateStatusVerifyAndList(t, runCompiledPin)
}

func testUpdateStatusVerifyAndList(t *testing.T, runner pinRunner) {
	root := t.TempDir()
	repo, sha := sourceRepo(t, root)

	result := runTool(t, runner, root, repo, "update")
	if result.code != 0 {
		t.Fatalf("update failed: %s", result.stderr)
	}
	if !strings.Contains(result.stdout, "updated: demo-tool "+sha) {
		t.Fatalf("unexpected update output: %s", result.stdout)
	}

	release := filepath.Join(root, "share", "demo-tool", "releases", sha)
	if info, err := os.Stat(release); err != nil || !info.IsDir() {
		t.Fatalf("missing release: %s", release)
	}
	currentTarget, err := filepath.EvalSymlinks(filepath.Join(root, "share", "demo-tool", "current"))
	if err != nil {
		t.Fatal(err)
	}
	canonicalRelease, err := filepath.EvalSymlinks(release)
	if err != nil {
		t.Fatal(err)
	}
	if currentTarget != canonicalRelease {
		t.Fatalf("current points to %s, want %s", currentTarget, canonicalRelease)
	}
	entrypointTarget, err := os.Readlink(filepath.Join(root, "bin", "demo-tool"))
	if err != nil {
		t.Fatal(err)
	}
	expectedEntrypoint := filepath.Join(root, "share", "demo-tool", "current", "venv", "bin", "demo-tool")
	if entrypointTarget != expectedEntrypoint {
		t.Fatalf("entrypoint points to %s, want %s", entrypointTarget, expectedEntrypoint)
	}

	result = runTool(t, runner, root, repo, "status")
	if result.code != 0 {
		t.Fatalf("status failed: %s", result.stderr)
	}
	if !strings.Contains(result.stdout, "installed: yes") || !strings.Contains(result.stdout, "release: "+sha) {
		t.Fatalf("unexpected status output: %s", result.stdout)
	}

	result = runTool(t, runner, root, repo, "verify")
	if result.code != 0 {
		t.Fatalf("verify failed: %s", result.stderr)
	}
	if !strings.Contains(result.stdout, "verified: demo-tool "+sha) {
		t.Fatalf("unexpected verify output: %s", result.stdout)
	}

	result = runner(t, root, "list")
	if result.code != 0 {
		t.Fatalf("list failed: %s", result.stderr)
	}
	if !strings.Contains(result.stdout, "demo-tool\t"+sha+"\t") {
		t.Fatalf("unexpected list output: %s", result.stdout)
	}
}

func TestE2ECompiledBinaryLifecycle(t *testing.T) {
	root := t.TempDir()
	repo, oldSHA := sourceRepo(t, root)

	result := runTool(t, runCompiledPin, root, repo, "status")
	requireCode(t, result, 0)
	requireContains(t, result.stdout, "installed: no")
	requireContains(t, result.stdout, "branch: origin/main")

	result = runTool(t, runCompiledPin, root, repo, "check")
	requireCode(t, result, 1)
	requireContains(t, result.stdout, "status: not-installed")
	requireContains(t, result.stdout, "target: "+oldSHA)

	result = runTool(t, runCompiledPin, root, repo, "update")
	requireCode(t, result, 0)
	requireContains(t, result.stdout, "updated: demo-tool "+oldSHA)
	requireInstalledVersion(t, root, "1")
	requireReleaseLink(t, root, "current", oldSHA)
	requireReleaseMetadata(t, root, oldSHA)

	result = runTool(t, runCompiledPin, root, repo, "status")
	requireCode(t, result, 0)
	requireContains(t, result.stdout, "installed: yes")
	requireContains(t, result.stdout, "release: "+oldSHA)

	result = runTool(t, runCompiledPin, root, repo, "verify")
	requireCode(t, result, 0)
	requireContains(t, result.stdout, "verified: demo-tool "+oldSHA)

	result = runCompiledPin(t, root, "list")
	requireCode(t, result, 0)
	requireContains(t, result.stdout, "demo-tool\t"+oldSHA+"\t")

	result = runTool(t, runCompiledPin, root, repo, "check")
	requireCode(t, result, 0)
	requireContains(t, result.stdout, "status: current")

	newSHA := commitToolVersion(t, repo, "2", false)
	git(t, repo, "push")

	result = runTool(t, runCompiledPin, root, repo, "check")
	requireCode(t, result, 1)
	requireContains(t, result.stdout, "active: "+oldSHA)
	requireContains(t, result.stdout, "target: "+newSHA)
	requireContains(t, result.stdout, "status: behind")

	result = runTool(t, runCompiledPin, root, repo, "update")
	requireCode(t, result, 0)
	requireContains(t, result.stdout, "updated: demo-tool "+newSHA)
	requireInstalledVersion(t, root, "2")
	requireReleaseLink(t, root, "current", newSHA)
	requireReleaseLink(t, root, "previous", oldSHA)
	requireReleaseMetadata(t, root, newSHA)

	result = runTool(t, runCompiledPin, root, repo, "check")
	requireCode(t, result, 0)
	requireContains(t, result.stdout, "status: current")

	forceRemoteMain(t, repo, oldSHA)
	result = runTool(t, runCompiledPin, root, repo, "check")
	requireCode(t, result, 1)
	requireContains(t, result.stdout, "active: "+newSHA)
	requireContains(t, result.stdout, "target: "+oldSHA)
	requireContains(t, result.stdout, "status: ahead")

	result = runTool(t, runCompiledPin, root, repo, "rollback")
	requireCode(t, result, 0)
	requireContains(t, result.stdout, "rolled back: demo-tool "+oldSHA)
	requireInstalledVersion(t, root, "1")
	requireReleaseLink(t, root, "current", oldSHA)
	requireReleaseLink(t, root, "previous", newSHA)

	result = runTool(t, runCompiledPin, root, repo, "check")
	requireCode(t, result, 0)
	requireContains(t, result.stdout, "status: current")
}

func TestE2ECompiledBinaryCheckDiverged(t *testing.T) {
	root := t.TempDir()
	repo, oldSHA := sourceRepo(t, root)

	result := runTool(t, runCompiledPin, root, repo, "update")
	requireCode(t, result, 0)

	newSHA := commitToolVersion(t, repo, "2", true)
	git(t, repo, "push", "--force", "origin", "main")

	result = runTool(t, runCompiledPin, root, repo, "check")
	requireCode(t, result, 1)
	requireContains(t, result.stdout, "active: "+oldSHA)
	requireContains(t, result.stdout, "target: "+newSHA)
	requireContains(t, result.stdout, "status: diverged")
}

func TestE2ECompiledBinaryUpdateWithoutStableLink(t *testing.T) {
	root := t.TempDir()
	repo, _ := sourceRepo(t, root)

	replaceInFile(t, filepath.Join(repo, "pin.toml"), "link = true", "link = false")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "disable stable link")
	git(t, repo, "push")
	sha := git(t, repo, "rev-parse", "HEAD")

	result := runTool(t, runCompiledPin, root, repo, "update")
	requireCode(t, result, 0)
	requireContains(t, result.stdout, "updated: demo-tool "+sha)

	stableEntrypoint := filepath.Join(root, "bin", "demo-tool")
	if _, err := os.Lstat(stableEntrypoint); !os.IsNotExist(err) {
		t.Fatalf("stable entrypoint exists despite link=false: %s", stableEntrypoint)
	}

	result = runTool(t, runCompiledPin, root, repo, "verify")
	requireCode(t, result, 0)
	requireContains(t, result.stdout, "verified: demo-tool "+sha)

	releaseEntrypoint := filepath.Join(root, "share", "demo-tool", "current", "venv", "bin", "demo-tool")
	output := run(t, "", releaseEntrypoint)
	if output != "demo 1" {
		t.Fatalf("release entrypoint output = %q, want %q", output, "demo 1")
	}
}

func TestE2ECompiledBinaryPreflightFailureStopsUpdate(t *testing.T) {
	root := t.TempDir()
	repo, _ := sourceRepo(t, root)

	replaceInFile(
		t,
		filepath.Join(repo, "pin.toml"),
		`preflight = [["python3", "-c", "from pathlib import Path; assert Path('demo_tool.py').is_file()"]]`,
		`preflight = [["python3", "-c", "raise SystemExit(7)"]]`,
	)
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "break preflight")
	git(t, repo, "push")

	result := runTool(t, runCompiledPin, root, repo, "update")
	requireCode(t, result, 2)
	requireContains(t, result.stderr, "command failed")
	requireContains(t, result.stderr, "raise SystemExit(7)")

	currentLink := filepath.Join(root, "share", "demo-tool", "current")
	if _, err := os.Lstat(currentLink); !os.IsNotExist(err) {
		t.Fatalf("current release exists after failed preflight: %s", currentLink)
	}
}

func TestUpdateRefusesDirtySource(t *testing.T) {
	root := t.TempDir()
	repo, _ := sourceRepo(t, root)
	writeFile(t, filepath.Join(repo, "dirty.txt"), "dirty\n")

	result := runTool(t, runCompiledPin, root, repo, "update")
	if result.code != 2 {
		t.Fatalf("update code = %d, want 2", result.code)
	}
	if !strings.Contains(result.stderr, "source checkout is dirty") {
		t.Fatalf("unexpected error: %s", result.stderr)
	}
}

func TestConfigRejectsPathEscapingValues(t *testing.T) {
	cases := []struct {
		key      string
		badValue string
	}{
		{"name", "../escape"},
		{"entrypoint", "../demo-tool"},
		{"entrypoint", "/tmp/demo-tool"},
	}
	for _, tc := range cases {
		t.Run(tc.key+"="+tc.badValue, func(t *testing.T) {
			root := t.TempDir()
			repo, _ := sourceRepo(t, root)
			replacePinValue(t, repo, tc.key, "demo-tool", tc.badValue)

			result := runTool(t, runPin, root, repo, "update")
			if result.code != 2 {
				t.Fatalf("update code = %d, want 2", result.code)
			}
			if !strings.Contains(result.stderr, "pin.toml key \""+tc.key+"\" must be a single path segment") {
				t.Fatalf("unexpected error: %s", result.stderr)
			}
			if _, err := os.Stat(filepath.Join(root, "escape")); !os.IsNotExist(err) {
				t.Fatalf("escape path should not exist")
			}
		})
	}
}

func TestEntrypointEnvReplacesExistingPath(t *testing.T) {
	t.Setenv("PATH", "/usr/bin")
	env := entrypointEnv("/tmp/release/venv/bin/demo-tool")

	pathEntries := 0
	for _, item := range env {
		if strings.HasPrefix(item, "PATH=") {
			pathEntries++
			if item != "PATH=/tmp/release/venv/bin:/usr/bin" {
				t.Fatalf("PATH = %q", item)
			}
		}
	}
	if pathEntries != 1 {
		t.Fatalf("PATH entries = %d, want 1", pathEntries)
	}
}

func TestCheckReportsBehind(t *testing.T) {
	root := t.TempDir()
	repo, oldSHA := sourceRepo(t, root)
	if result := runTool(t, runPin, root, repo, "update"); result.code != 0 {
		t.Fatalf("update failed: %s", result.stderr)
	}

	newSHA := commitToolVersion(t, repo, "2", false)
	git(t, repo, "push")
	if newSHA == oldSHA {
		t.Fatal("new sha should differ")
	}

	result := runTool(t, runPin, root, repo, "check")
	if result.code != 1 {
		t.Fatalf("check code = %d, want 1", result.code)
	}
	if !strings.Contains(result.stdout, "active: "+oldSHA) || !strings.Contains(result.stdout, "target: "+newSHA) || !strings.Contains(result.stdout, "status: behind") {
		t.Fatalf("unexpected check output: %s", result.stdout)
	}
}

func TestCheckReportsAheadAsFailure(t *testing.T) {
	root := t.TempDir()
	repo, oldSHA := sourceRepo(t, root)
	if result := runTool(t, runPin, root, repo, "update"); result.code != 0 {
		t.Fatalf("update failed: %s", result.stderr)
	}
	newSHA := commitToolVersion(t, repo, "2", false)
	git(t, repo, "push")
	if result := runTool(t, runPin, root, repo, "update"); result.code != 0 {
		t.Fatalf("second update failed: %s", result.stderr)
	}

	forceRemoteMain(t, repo, oldSHA)

	result := runTool(t, runPin, root, repo, "check")
	if result.code != 1 {
		t.Fatalf("check code = %d, want 1", result.code)
	}
	if !strings.Contains(result.stdout, "active: "+newSHA) || !strings.Contains(result.stdout, "target: "+oldSHA) || !strings.Contains(result.stdout, "status: ahead") {
		t.Fatalf("unexpected check output: %s", result.stdout)
	}
}

func TestCheckReportsDivergedAsFailure(t *testing.T) {
	root := t.TempDir()
	repo, oldSHA := sourceRepo(t, root)
	if result := runTool(t, runPin, root, repo, "update"); result.code != 0 {
		t.Fatalf("update failed: %s", result.stderr)
	}

	newSHA := commitToolVersion(t, repo, "2", true)
	git(t, repo, "push", "--force", "origin", "main")
	if newSHA == oldSHA {
		t.Fatal("new sha should differ")
	}

	result := runTool(t, runPin, root, repo, "check")
	if result.code != 1 {
		t.Fatalf("check code = %d, want 1", result.code)
	}
	if !strings.Contains(result.stdout, "active: "+oldSHA) || !strings.Contains(result.stdout, "target: "+newSHA) || !strings.Contains(result.stdout, "status: diverged") {
		t.Fatalf("unexpected check output: %s", result.stdout)
	}
}

func TestRollbackSwapsCurrentAndPrevious(t *testing.T) {
	root := t.TempDir()
	repo, oldSHA := sourceRepo(t, root)
	if result := runTool(t, runPin, root, repo, "update"); result.code != 0 {
		t.Fatalf("update failed: %s", result.stderr)
	}

	newSHA := commitToolVersion(t, repo, "2", false)
	git(t, repo, "push")
	if result := runTool(t, runPin, root, repo, "update"); result.code != 0 {
		t.Fatalf("second update failed: %s", result.stderr)
	}

	current, err := filepath.EvalSymlinks(filepath.Join(root, "share", "demo-tool", "current"))
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(current) != newSHA {
		t.Fatalf("current = %s, want %s", filepath.Base(current), newSHA)
	}
	previous, err := filepath.EvalSymlinks(filepath.Join(root, "share", "demo-tool", "previous"))
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(previous) != oldSHA {
		t.Fatalf("previous = %s, want %s", filepath.Base(previous), oldSHA)
	}

	result := runTool(t, runPin, root, repo, "rollback")
	if result.code != 0 {
		t.Fatalf("rollback failed: %s", result.stderr)
	}
	if !strings.Contains(result.stdout, "rolled back: demo-tool "+oldSHA) {
		t.Fatalf("unexpected rollback output: %s", result.stdout)
	}
	current, err = filepath.EvalSymlinks(filepath.Join(root, "share", "demo-tool", "current"))
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(current) != oldSHA {
		t.Fatalf("current = %s, want %s", filepath.Base(current), oldSHA)
	}
	previous, err = filepath.EvalSymlinks(filepath.Join(root, "share", "demo-tool", "previous"))
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(previous) != newSHA {
		t.Fatalf("previous = %s, want %s", filepath.Base(previous), newSHA)
	}
}
