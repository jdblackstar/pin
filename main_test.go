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

func appendFile(t *testing.T, path, text string) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := file.WriteString(text); err != nil {
		t.Fatal(err)
	}
}

func writeTool(t *testing.T, repo, version string) {
	t.Helper()
	writeFile(t, filepath.Join(repo, "pyproject.toml"), `[build-system]
requires = []
build-backend = "demo_backend"
backend-path = ["."]

[project]
name = "demo-tool"
version = "0.1.0"

[project.scripts]
demo-tool = "demo_tool:main"
`)
	writeFile(t, filepath.Join(repo, "demo_tool.py"), `def main():
    print("demo `+version+`")
    return 0
`)
	writeFile(t, filepath.Join(repo, "demo_backend.py"), `import base64
import csv
import hashlib
import io
import pathlib
import zipfile

NAME = "demo_tool"
VERSION = "0.1.0"
DIST_INFO = f"{NAME}-{VERSION}.dist-info"


def _hash(data):
    digest = hashlib.sha256(data).digest()
    return "sha256=" + base64.urlsafe_b64encode(digest).rstrip(b"=").decode()


def _record_row(path, data):
    return [path, _hash(data), str(len(data))]


def build_wheel(wheel_directory, config_settings=None, metadata_directory=None):
    wheel_name = f"{NAME}-{VERSION}-py3-none-any.whl"
    wheel_path = pathlib.Path(wheel_directory, wheel_name)
    files = {
        "demo_tool.py": pathlib.Path("demo_tool.py").read_bytes(),
        f"{DIST_INFO}/METADATA": b"Metadata-Version: 2.1\nName: demo-tool\nVersion: 0.1.0\n",
        f"{DIST_INFO}/WHEEL": b"Wheel-Version: 1.0\nGenerator: demo-backend\nRoot-Is-Purelib: true\nTag: py3-none-any\n",
        f"{DIST_INFO}/entry_points.txt": b"[console_scripts]\ndemo-tool = demo_tool:main\n",
    }
    record = io.StringIO()
    writer = csv.writer(record, lineterminator="\n")
    for path, data in files.items():
        writer.writerow(_record_row(path, data))
    writer.writerow([f"{DIST_INFO}/RECORD", "", ""])
    files[f"{DIST_INFO}/RECORD"] = record.getvalue().encode()

    with zipfile.ZipFile(wheel_path, "w", zipfile.ZIP_DEFLATED) as wheel:
        for path, data in files.items():
            wheel.writestr(path, data)
    return wheel_name


def prepare_metadata_for_build_wheel(metadata_directory, config_settings=None):
    dist_info = pathlib.Path(metadata_directory, DIST_INFO)
    dist_info.mkdir(parents=True, exist_ok=True)
    dist_info.joinpath("METADATA").write_text("Metadata-Version: 2.1\nName: demo-tool\nVersion: 0.1.0\n")
    dist_info.joinpath("WHEEL").write_text("Wheel-Version: 1.0\nGenerator: demo-backend\nRoot-Is-Purelib: true\nTag: py3-none-any\n")
    return DIST_INFO
`)
	writeFile(t, filepath.Join(repo, "pin.toml"), `name = "demo-tool"
branch = "main"
remote = "origin"
preflight = [["python3", "-c", "from pathlib import Path; assert Path('demo_tool.py').is_file()"]]
verify = ["demo-tool"]
link = true
`)
}

func writeScriptTool(t *testing.T, repo, version string) {
	t.Helper()
	writeFile(t, filepath.Join(repo, "automation", "demo_tool.py"), `import sys
from pathlib import Path

message = Path("data/message.txt").read_text().strip()
print("script `+version+` " + message + " " + " ".join(sys.argv[1:]))
`)
	writeFile(t, filepath.Join(repo, "data", "message.txt"), "from-source\n")
	writeFile(t, filepath.Join(repo, "pin.toml"), `name = "demo-tool"
script = "automation/demo_tool.py"
requirements = "requirements.txt"
branch = "main"
remote = "origin"
preflight = [["python3", "-c", "from pathlib import Path; compile(Path('automation/demo_tool.py').read_text(), 'automation/demo_tool.py', 'exec')"]]
verify = [["demo-tool", "verify"]]
link = true
`)
	writeFile(t, filepath.Join(repo, "requirements.txt"), "")
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

func TestE2ECompiledBinaryPythonScriptLifecycle(t *testing.T) {
	root := t.TempDir()
	repo, _ := sourceRepo(t, root)
	writeScriptTool(t, repo, "1")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "switch to script tool")
	git(t, repo, "push")
	oldSHA := git(t, repo, "rev-parse", "HEAD")

	result := runTool(t, runCompiledPin, root, repo, "update")
	requireCode(t, result, 0)
	requireContains(t, result.stdout, "updated: demo-tool "+oldSHA)
	requireReleaseLink(t, root, "current", oldSHA)
	output := run(t, "", filepath.Join(root, "bin", "demo-tool"), "cron")
	if output != "script 1 from-source cron" {
		t.Fatalf("script output = %q, want %q", output, "script 1 from-source cron")
	}

	metadataPath := filepath.Join(root, "share", "demo-tool", "releases", oldSHA, ".pin", "release.json")
	data, err := os.ReadFile(metadataPath)
	if err != nil {
		t.Fatal(err)
	}
	var metadata map[string]any
	if err := json.Unmarshal(data, &metadata); err != nil {
		t.Fatal(err)
	}
	config, ok := metadata["config"].(map[string]any)
	if !ok || config["script"] != "automation/demo_tool.py" {
		t.Fatalf("metadata config script = %#v", metadata["config"])
	}

	writeScriptTool(t, repo, "2")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "update script")
	git(t, repo, "push")
	newSHA := git(t, repo, "rev-parse", "HEAD")

	result = runTool(t, runCompiledPin, root, repo, "update")
	requireCode(t, result, 0)
	requireContains(t, result.stdout, "updated: demo-tool "+newSHA)
	requireReleaseLink(t, root, "current", newSHA)
	requireReleaseLink(t, root, "previous", oldSHA)
	output = run(t, "", filepath.Join(root, "bin", "demo-tool"), "agent")
	if output != "script 2 from-source agent" {
		t.Fatalf("script output = %q, want %q", output, "script 2 from-source agent")
	}

	result = runTool(t, runCompiledPin, root, repo, "rollback")
	requireCode(t, result, 0)
	requireReleaseLink(t, root, "current", oldSHA)
	output = run(t, "", filepath.Join(root, "bin", "demo-tool"), "cron")
	if output != "script 1 from-source cron" {
		t.Fatalf("script output after rollback = %q, want %q", output, "script 1 from-source cron")
	}
}

func TestE2ECompiledBinaryPythonScriptEntrypointVenvCollisionDoesNotActivate(t *testing.T) {
	root := t.TempDir()
	repo, _ := sourceRepo(t, root)
	writeScriptTool(t, repo, "1")
	appendFile(t, filepath.Join(repo, "pin.toml"), `entrypoint = "python"`+"\n")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "collide with venv python")
	git(t, repo, "push")

	result := runTool(t, runCompiledPin, root, repo, "update")
	requireCode(t, result, 2)
	requireContains(t, result.stderr, `entrypoint "python" conflicts with an existing file in the release virtualenv`)

	currentLink := filepath.Join(root, "share", "demo-tool", "current")
	if _, err := os.Lstat(currentLink); !os.IsNotExist(err) {
		t.Fatalf("current release exists after venv entrypoint collision: %s", currentLink)
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

func TestE2ECompiledBinaryVerifyFailureDoesNotActivateRelease(t *testing.T) {
	root := t.TempDir()
	repo, oldSHA := sourceRepo(t, root)

	result := runTool(t, runCompiledPin, root, repo, "update")
	requireCode(t, result, 0)
	requireInstalledVersion(t, root, "1")

	writeTool(t, repo, "2")
	replaceInFile(
		t,
		filepath.Join(repo, "pin.toml"),
		`verify = ["demo-tool"]`,
		`verify = [["python3", "-c", "import sys; sys.stderr.write('verify boom'); raise SystemExit(9)"]]`,
	)
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "break verify")
	newSHA := git(t, repo, "rev-parse", "HEAD")
	git(t, repo, "push")

	result = runTool(t, runCompiledPin, root, repo, "update")
	requireCode(t, result, 2)
	requireContains(t, result.stderr, "verify release")
	requireContains(t, result.stderr, "verify boom")
	requireInstalledVersion(t, root, "1")
	requireReleaseLink(t, root, "current", oldSHA)

	if _, err := os.Stat(filepath.Join(root, "share", "demo-tool", "releases", newSHA)); err != nil {
		t.Fatalf("failed candidate release should remain inspectable: %v", err)
	}
}

func TestE2ECompiledBinaryEntrypointCollisionDoesNotActivate(t *testing.T) {
	root := t.TempDir()
	repo, _ := sourceRepo(t, root)
	stableEntrypoint := filepath.Join(root, "bin", "demo-tool")
	writeFile(t, stableEntrypoint, "not managed by pin\n")

	result := runTool(t, runCompiledPin, root, repo, "update")
	requireCode(t, result, 2)
	requireContains(t, result.stderr, "entrypoint path exists and is not a symlink")

	currentLink := filepath.Join(root, "share", "demo-tool", "current")
	if _, err := os.Lstat(currentLink); !os.IsNotExist(err) {
		t.Fatalf("current release exists after entrypoint collision: %s", currentLink)
	}
	data, err := os.ReadFile(stableEntrypoint)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "not managed by pin\n" {
		t.Fatalf("entrypoint collision file was modified: %q", string(data))
	}
}

func TestE2ECompiledBinaryRollbackEntrypointCollisionDoesNotActivate(t *testing.T) {
	root := t.TempDir()
	repo, oldSHA := sourceRepo(t, root)

	result := runTool(t, runCompiledPin, root, repo, "update")
	requireCode(t, result, 0)
	newSHA := commitToolVersion(t, repo, "2", false)
	git(t, repo, "push")
	result = runTool(t, runCompiledPin, root, repo, "update")
	requireCode(t, result, 0)
	requireInstalledVersion(t, root, "2")

	stableEntrypoint := filepath.Join(root, "bin", "demo-tool")
	if err := os.Remove(stableEntrypoint); err != nil {
		t.Fatal(err)
	}
	writeFile(t, stableEntrypoint, "not managed by pin\n")

	result = runTool(t, runCompiledPin, root, repo, "rollback")
	requireCode(t, result, 2)
	requireContains(t, result.stderr, "entrypoint path exists and is not a symlink")
	requireReleaseLink(t, root, "current", newSHA)
	requireReleaseLink(t, root, "previous", oldSHA)
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
		{"script", "../escape.py"},
		{"script", "/tmp/escape.py"},
		{"requirements", "../requirements.txt"},
		{"requirements", "/tmp/requirements.txt"},
	}
	for _, tc := range cases {
		t.Run(tc.key+"="+tc.badValue, func(t *testing.T) {
			root := t.TempDir()
			repo, _ := sourceRepo(t, root)
			if tc.key == "entrypoint" || tc.key == "script" || tc.key == "requirements" {
				appendFile(t, filepath.Join(repo, "pin.toml"), tc.key+` = "`+tc.badValue+`"`+"\n")
			} else {
				replacePinValue(t, repo, tc.key, "demo-tool", tc.badValue)
			}

			result := runTool(t, runPin, root, repo, "update")
			if result.code != 2 {
				t.Fatalf("update code = %d, want 2", result.code)
			}
			want := "pin.toml key \"" + tc.key + "\" must be a single path segment"
			if tc.key == "script" || tc.key == "requirements" {
				want = "pin.toml key \"" + tc.key + "\" must"
			}
			if !strings.Contains(result.stderr, want) {
				t.Fatalf("unexpected error: %s", result.stderr)
			}
			if _, err := os.Stat(filepath.Join(root, "escape")); !os.IsNotExist(err) {
				t.Fatalf("escape path should not exist")
			}
		})
	}
}

func TestConfigRejectsRequirementsWithoutScript(t *testing.T) {
	root := t.TempDir()
	repo, _ := sourceRepo(t, root)
	appendFile(t, filepath.Join(repo, "pin.toml"), `requirements = "requirements.txt"`+"\n")

	result := runTool(t, runPin, root, repo, "update")
	requireCode(t, result, 2)
	requireContains(t, result.stderr, `pin.toml key "requirements" requires key "script"`)
}

func TestConfigDefaultsEntrypointToName(t *testing.T) {
	root := t.TempDir()
	repo, _ := sourceRepo(t, root)

	result := runTool(t, runPin, root, repo, "status")
	requireCode(t, result, 0)
	requireContains(t, result.stdout, "entrypoint: "+filepath.Join(root, "bin", "demo-tool"))
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

func TestPythonInstallEnvDefaultsCachesInsideRelease(t *testing.T) {
	t.Setenv("UV_CACHE_DIR", "")
	t.Setenv("PIP_CACHE_DIR", "")
	env := pythonInstallEnv("/tmp/release")

	requireContains(t, strings.Join(env, "\n"), "UV_CACHE_DIR=/tmp/release/.cache/uv")
	requireContains(t, strings.Join(env, "\n"), "PIP_CACHE_DIR=/tmp/release/.cache/pip")
}

func TestPythonInstallEnvPreservesConfiguredCaches(t *testing.T) {
	t.Setenv("UV_CACHE_DIR", "/tmp/custom-uv")
	t.Setenv("PIP_CACHE_DIR", "/tmp/custom-pip")
	env := pythonInstallEnv("/tmp/release")
	joined := strings.Join(env, "\n")

	requireContains(t, joined, "UV_CACHE_DIR=/tmp/custom-uv")
	requireContains(t, joined, "PIP_CACHE_DIR=/tmp/custom-pip")
	if strings.Contains(joined, "UV_CACHE_DIR=/tmp/release/.cache/uv") {
		t.Fatalf("UV_CACHE_DIR default was added despite configured cache: %s", joined)
	}
	if strings.Contains(joined, "PIP_CACHE_DIR=/tmp/release/.cache/pip") {
		t.Fatalf("PIP_CACHE_DIR default was added despite configured cache: %s", joined)
	}
}

func TestSplitCommandHandlesShellLikeQuoting(t *testing.T) {
	got, err := splitCommand(`python3 -c "print('hello friend')" path\ with\ spaces`)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"python3", "-c", "print('hello friend')", "path with spaces"}
	if len(got) != len(want) {
		t.Fatalf("split length = %d, want %d: %#v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("split[%d] = %q, want %q: %#v", i, got[i], want[i], got)
		}
	}
}

func TestSplitCommandRejectsUnfinishedQuote(t *testing.T) {
	if _, err := splitCommand(`python3 -c "print(1)`); err == nil {
		t.Fatal("expected unfinished quote to fail")
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
