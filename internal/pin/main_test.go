package pin

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
	allArgs := append([]string{"--pin-home", filepath.Join(root, "share")}, args...)
	code := RunCLI(allArgs, &stdout, &stderr)
	return cliResult{code: code, stdout: stdout.String(), stderr: stderr.String()}
}

func runCompiledPin(t *testing.T, root string, args ...string) cliResult {
	t.Helper()
	prepareToolEnv(t, root)
	bin := filepath.Join(root, "pin")
	if _, err := os.Stat(bin); err != nil {
		run(t, "", "go", "build", "-o", bin, "../..")
	}
	allArgs := append([]string{"--pin-home", filepath.Join(root, "share")}, args...)
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

func mustReadFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
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
source = "."
branch = "main"
remote = "origin"
preflight = [["python3", "-c", "from pathlib import Path; assert Path('demo_tool.py').is_file()"]]
verify = ["demo-tool"]
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
source = "automation/demo_tool.py"
requirements = "requirements.txt"
branch = "main"
remote = "origin"
preflight = [["python3", "-c", "from pathlib import Path; compile(Path('automation/demo_tool.py').read_text(), 'automation/demo_tool.py', 'exec')"]]
verify = [["demo-tool", "verify"]]
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
	output := run(t, "", activeEntrypoint(root))
	if output != "demo "+version {
		t.Fatalf("demo-tool output = %q, want %q", output, "demo "+version)
	}
}

func activeEntrypoint(root string) string {
	return filepath.Join(root, "share", "demo-tool", "current", ".venv", "bin", "demo-tool")
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
	if got, _ := metadata["schema_version"].(float64); got != float64(schemaVersion) {
		t.Fatalf("metadata schema_version = %v, want %d", metadata["schema_version"], schemaVersion)
	}
}

func TestUpdateStatusVerifyAndList(t *testing.T) {
	testUpdateStatusVerifyAndList(t, runPin)
}

func TestGlobalHelpPrintsUsageOnce(t *testing.T) {
	result := runPin(t, t.TempDir(), "--help")
	requireCode(t, result, 0)
	if got := strings.Count(result.stdout, "Usage: pin "); got != 1 {
		t.Fatalf("usage count = %d, want 1\nstdout:\n%s", got, result.stdout)
	}
}

func TestInitCreatesDefaultConfig(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "daily-report")
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}

	result := runPin(t, root, "init", repo)
	requireCode(t, result, 0)
	configPath := filepath.Join(repo, "pin.toml")
	requireContains(t, result.stdout, "created: "+configPath)

	config, err := loadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if config.name != "daily-report" {
		t.Fatalf("name = %q, want daily-report", config.name)
	}
	if config.branch != "main" {
		t.Fatalf("branch = %q, want main", config.branch)
	}
	if config.remote != "origin" {
		t.Fatalf("remote = %q, want origin", config.remote)
	}
	if config.source.path != "." || config.source.kind != sourcePackage {
		t.Fatalf("source = %#v, want package .", config.source)
	}
	if got := config.verify; len(got) != 1 || strings.Join(got[0], " ") != "daily-report --help" {
		t.Fatalf("verify = %#v, want daily-report --help", got)
	}
}

func TestInitAcceptsConfigFlags(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}

	result := runPin(
		t,
		root,
		"init",
		"--name", "daily-report",
		"--entrypoint", "report",
		"--source", "scripts/report.py",
		"--requirements", "requirements.txt",
		"--inject", ".env",
		"--inject", "config/local.toml",
		"--branch", "stable",
		"--remote", "upstream",
		"--preflight", "python3 -m py_compile scripts/report.py",
		"--verify", "report --help",
		repo,
	)
	requireCode(t, result, 0)

	config, err := loadConfig(filepath.Join(repo, "pin.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if config.name != "daily-report" || config.entrypoint != "report" || config.source.path != "scripts/report.py" || config.source.kind != sourceScript || config.requirements != "requirements.txt" {
		t.Fatalf("unexpected config: %#v", config)
	}
	if got := strings.Join(config.inject, ","); got != ".env,config/local.toml" {
		t.Fatalf("inject = %q, want .env,config/local.toml", got)
	}
	if config.branch != "stable" || config.remote != "upstream" {
		t.Fatalf("branch/remote = %s/%s, want stable/upstream", config.branch, config.remote)
	}
	if got := config.preflight; len(got) != 1 || strings.Join(got[0], " ") != "python3 -m py_compile scripts/report.py" {
		t.Fatalf("preflight = %#v", got)
	}
	if got := config.verify; len(got) != 1 || strings.Join(got[0], " ") != "report --help" {
		t.Fatalf("verify = %#v", got)
	}
}

func TestInitRefusesToOverwriteConfig(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	writeFile(t, filepath.Join(repo, "pin.toml"), "name = \"existing\"\n")

	result := runPin(t, root, "init", repo)
	requireCode(t, result, 2)
	requireContains(t, result.stderr, "pin.toml already exists")
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
	if _, err := os.Stat(filepath.Join(release, "pin.toml")); err != nil {
		t.Fatalf("release should expose checkout files at root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(release, "src")); !os.IsNotExist(err) {
		t.Fatalf("release should not contain nested src directory: %s", filepath.Join(release, "src"))
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
	if _, err := os.Stat(activeEntrypoint(root)); err != nil {
		t.Fatalf("missing active release entrypoint: %v", err)
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

func TestE2ECompiledBinaryReleaseEntrypoint(t *testing.T) {
	root := t.TempDir()
	repo, _ := sourceRepo(t, root)

	sha := git(t, repo, "rev-parse", "HEAD")

	result := runTool(t, runCompiledPin, root, repo, "update")
	requireCode(t, result, 0)
	requireContains(t, result.stdout, "updated: demo-tool "+sha)

	result = runTool(t, runCompiledPin, root, repo, "verify")
	requireCode(t, result, 0)
	requireContains(t, result.stdout, "verified: demo-tool "+sha)

	output := run(t, "", activeEntrypoint(root))
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
	output := run(t, "", activeEntrypoint(root), "cron")
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
	if !ok || config["source"] != "automation/demo_tool.py" {
		t.Fatalf("metadata config source = %#v", metadata["config"])
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
	output = run(t, "", activeEntrypoint(root), "agent")
	if output != "script 2 from-source agent" {
		t.Fatalf("script output = %q, want %q", output, "script 2 from-source agent")
	}

	result = runTool(t, runCompiledPin, root, repo, "rollback")
	requireCode(t, result, 0)
	requireReleaseLink(t, root, "current", oldSHA)
	output = run(t, "", activeEntrypoint(root), "cron")
	if output != "script 1 from-source cron" {
		t.Fatalf("script output after rollback = %q, want %q", output, "script 1 from-source cron")
	}
}

func TestE2ECompiledBinaryInjectSeedsPinRuntimeFiles(t *testing.T) {
	root := t.TempDir()
	repo, _ := sourceRepo(t, root)
	writeScriptTool(t, repo, "1")
	writeFile(t, filepath.Join(repo, ".gitignore"), ".env\nconfig/local.toml\n")
	writeFile(t, filepath.Join(repo, ".env"), "TOKEN=alpha\n")
	writeFile(t, filepath.Join(repo, "config", "local.toml"), "mode = \"local\"\n")
	writeFile(t, filepath.Join(repo, "automation", "demo_tool.py"), `import sys
from pathlib import Path

print(Path(".env").read_text().strip() + " " + Path("config/local.toml").read_text().strip() + " " + " ".join(sys.argv[1:]))
`)
	appendFile(t, filepath.Join(repo, "pin.toml"), `inject = [".env", "config/local.toml"]`+"\n")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "add runtime injection")
	git(t, repo, "push")
	sha := git(t, repo, "rev-parse", "HEAD")

	result := runTool(t, runCompiledPin, root, repo, "update")
	requireCode(t, result, 0)
	requireContains(t, result.stdout, "updated: demo-tool "+sha)

	releaseEnv := filepath.Join(root, "share", "demo-tool", "releases", sha, ".env")
	target, err := os.Readlink(releaseEnv)
	if err != nil {
		t.Fatal(err)
	}
	sharedEnv := filepath.Join(root, "share", "demo-tool", "shared", ".env")
	if target != "../../shared/.env" {
		t.Fatalf("inject symlink = %s, want ../../shared/.env", target)
	}
	releaseConfig := filepath.Join(root, "share", "demo-tool", "releases", sha, "config", "local.toml")
	target, err = os.Readlink(releaseConfig)
	if err != nil {
		t.Fatal(err)
	}
	sharedConfig := filepath.Join(root, "share", "demo-tool", "shared", "config", "local.toml")
	if target != "../../../shared/config/local.toml" {
		t.Fatalf("inject symlink = %s, want ../../../shared/config/local.toml", target)
	}

	output := run(t, "", activeEntrypoint(root), "first")
	if output != `TOKEN=alpha mode = "local" first` {
		t.Fatalf("script output = %q", output)
	}

	writeFile(t, sharedEnv, "TOKEN=beta\n")
	output = run(t, "", activeEntrypoint(root), "second")
	if output != `TOKEN=beta mode = "local" second` {
		t.Fatalf("script output after env rotation = %q", output)
	}
	if got := strings.TrimSpace(mustReadFile(t, filepath.Join(repo, ".env"))); got != "TOKEN=alpha" {
		t.Fatalf("source env = %q, want TOKEN=alpha", got)
	}

	result = runTool(t, runCompiledPin, root, repo, "status")
	requireCode(t, result, 0)
	requireContains(t, result.stdout, "inject: "+sharedEnv)
	requireContains(t, result.stdout, "inject: "+sharedConfig)

	result = runTool(t, runCompiledPin, root, repo, "verify")
	requireCode(t, result, 0)
}

func TestE2ECompiledBinaryStatusReportsSchemaTwoInjectPaths(t *testing.T) {
	root := t.TempDir()
	repo, _ := sourceRepo(t, root)
	writeScriptTool(t, repo, "1")
	writeFile(t, filepath.Join(repo, ".gitignore"), ".env\n")
	writeFile(t, filepath.Join(repo, ".env"), "TOKEN=legacy\n")
	appendFile(t, filepath.Join(repo, "pin.toml"), `inject = [".env"]`+"\n")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "inject env")
	git(t, repo, "push")
	sha := git(t, repo, "rev-parse", "HEAD")

	result := runTool(t, runCompiledPin, root, repo, "update")
	requireCode(t, result, 0)

	metadataPath := filepath.Join(root, "share", "demo-tool", "releases", sha, ".pin", "release.json")
	replaceInFile(t, metadataPath, `"schema_version": 3`, `"schema_version": 2`)

	result = runTool(t, runCompiledPin, root, repo, "status")
	requireCode(t, result, 0)
	requireContains(t, result.stdout, "inject: "+filepath.Join(repo, ".env"))
	if strings.Contains(result.stdout, filepath.Join(root, "share", "demo-tool", "shared", ".env")) {
		t.Fatalf("schema 2 status reported pin-home inject path:\n%s", result.stdout)
	}
}

func TestE2ECompiledBinaryInjectRuntimeDirectories(t *testing.T) {
	root := t.TempDir()
	repo, _ := sourceRepo(t, root)
	writeScriptTool(t, repo, "1")
	writeFile(t, filepath.Join(repo, ".gitignore"), "tokens/\nlogs/\n")
	writeFile(t, filepath.Join(repo, "tokens", "secret.txt"), "alpha\n")
	writeFile(t, filepath.Join(repo, "tokens", "processed.txt"), "source-state\n")
	writeFile(t, filepath.Join(repo, "logs", "seed.log"), "seed\n")
	writeFile(t, filepath.Join(repo, "automation", "demo_tool.py"), `import os
import sys
from pathlib import Path

tokens = Path("tokens")
logs = Path("logs")
secret = tokens.joinpath("secret.txt").read_text().strip()
tmp = tokens.joinpath(".processed.tmp")
tmp.write_text("shared-state\n")
os.replace(tmp, tokens.joinpath("processed.txt"))
logs.joinpath("run.log").write_text("ran " + " ".join(sys.argv[1:]))
print(secret)
`)
	appendFile(t, filepath.Join(repo, "pin.toml"), `inject = ["tokens", "logs"]`+"\n")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "add injected runtime paths")
	git(t, repo, "push")
	sha := git(t, repo, "rev-parse", "HEAD")

	result := runTool(t, runCompiledPin, root, repo, "update")
	requireCode(t, result, 0)
	requireContains(t, result.stdout, "updated: demo-tool "+sha)

	sharedTokens := filepath.Join(root, "share", "demo-tool", "shared", "tokens")
	sharedLogs := filepath.Join(root, "share", "demo-tool", "shared", "logs")
	releaseTokens := filepath.Join(root, "share", "demo-tool", "releases", sha, "tokens")
	target, err := os.Readlink(releaseTokens)
	if err != nil {
		t.Fatal(err)
	}
	if target != "../../shared/tokens" {
		t.Fatalf("shared symlink = %s, want ../../shared/tokens", target)
	}
	releaseLogs := filepath.Join(root, "share", "demo-tool", "releases", sha, "logs")
	target, err = os.Readlink(releaseLogs)
	if err != nil {
		t.Fatal(err)
	}
	if target != "../../shared/logs" {
		t.Fatalf("shared symlink = %s, want ../../shared/logs", target)
	}
	if got := strings.TrimSpace(run(t, "", activeEntrypoint(root), "cron")); got != "alpha" {
		t.Fatalf("script output = %q", got)
	}
	if got := strings.TrimSpace(mustReadFile(t, filepath.Join(sharedTokens, "processed.txt"))); got != "shared-state" {
		t.Fatalf("shared processed = %q, want shared-state", got)
	}
	if got := strings.TrimSpace(mustReadFile(t, filepath.Join(repo, "tokens", "processed.txt"))); got != "source-state" {
		t.Fatalf("source processed = %q, want source-state", got)
	}
	if got := strings.TrimSpace(mustReadFile(t, filepath.Join(sharedLogs, "run.log"))); got != "ran cron" {
		t.Fatalf("shared log = %q, want ran cron", got)
	}

	result = runTool(t, runCompiledPin, root, repo, "status")
	requireCode(t, result, 0)
	requireContains(t, result.stdout, "inject: "+sharedTokens)
	requireContains(t, result.stdout, "inject: "+sharedLogs)

	result = runTool(t, runCompiledPin, root, repo, "verify")
	requireCode(t, result, 0)
}

func TestE2ECompiledBinaryMissingInjectedPathStopsUpdate(t *testing.T) {
	root := t.TempDir()
	repo, _ := sourceRepo(t, root)
	writeScriptTool(t, repo, "1")
	appendFile(t, filepath.Join(repo, "pin.toml"), `inject = ["tokens"]`+"\n")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "require injected tokens")
	git(t, repo, "push")

	result := runTool(t, runCompiledPin, root, repo, "update")
	requireCode(t, result, 2)
	requireContains(t, result.stderr, "inject runtime paths: injected path is missing")

	currentLink := filepath.Join(root, "share", "demo-tool", "current")
	if _, err := os.Lstat(currentLink); !os.IsNotExist(err) {
		t.Fatalf("current release exists after missing injected path: %s", currentLink)
	}
}

func TestE2ECompiledBinaryInjectRefusesTrackedArchivePath(t *testing.T) {
	root := t.TempDir()
	repo, _ := sourceRepo(t, root)
	writeFile(t, filepath.Join(repo, "tokens", "readme.txt"), "tracked\n")
	appendFile(t, filepath.Join(repo, "pin.toml"), `inject = ["tokens"]`+"\n")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "track injected path")
	git(t, repo, "push")

	result := runTool(t, runCompiledPin, root, repo, "update")
	requireCode(t, result, 2)
	requireContains(t, result.stderr, "inject target already exists in archived checkout")

	sharedTokens := filepath.Join(root, "share", "demo-tool", "shared", "tokens")
	if _, err := os.Lstat(sharedTokens); !os.IsNotExist(err) {
		t.Fatalf("injected path should not be seeded for tracked archive path: %s", sharedTokens)
	}
}

func TestE2ECompiledBinaryRollbackUsesPreviousReleaseInjectedPathList(t *testing.T) {
	root := t.TempDir()
	repo, _ := sourceRepo(t, root)
	writeFile(t, filepath.Join(repo, ".gitignore"), "tokens/\nlogs/\n")
	writeFile(t, filepath.Join(repo, "tokens", "secret.txt"), "old\n")
	appendFile(t, filepath.Join(repo, "pin.toml"), `inject = ["tokens"]`+"\n")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "share tokens")
	git(t, repo, "push")
	oldSHA := git(t, repo, "rev-parse", "HEAD")

	result := runTool(t, runCompiledPin, root, repo, "update")
	requireCode(t, result, 0)
	requireReleaseLink(t, root, "current", oldSHA)

	writeFile(t, filepath.Join(repo, "logs", "seed.log"), "new\n")
	replaceInFile(
		t,
		filepath.Join(repo, "pin.toml"),
		`inject = ["tokens"]`,
		`inject = ["tokens", "logs"]`,
	)
	git(t, repo, "add", "pin.toml", ".gitignore")
	git(t, repo, "commit", "-m", "share logs")
	git(t, repo, "push")
	newSHA := git(t, repo, "rev-parse", "HEAD")

	result = runTool(t, runCompiledPin, root, repo, "update")
	requireCode(t, result, 0)
	requireReleaseLink(t, root, "current", newSHA)
	requireReleaseLink(t, root, "previous", oldSHA)

	if err := os.RemoveAll(filepath.Join(root, "share", "demo-tool", "shared", "logs")); err != nil {
		t.Fatal(err)
	}

	result = runTool(t, runCompiledPin, root, repo, "rollback")
	requireCode(t, result, 0)
	requireContains(t, result.stdout, "rolled back: demo-tool "+oldSHA)
	requireReleaseLink(t, root, "current", oldSHA)
	requireReleaseLink(t, root, "previous", newSHA)
}

func TestE2ECompiledBinaryRollbackUsesPreviousReleaseInjectList(t *testing.T) {
	root := t.TempDir()
	repo, _ := sourceRepo(t, root)
	writeFile(t, filepath.Join(repo, ".gitignore"), ".env\nconfig/local.toml\n")
	writeFile(t, filepath.Join(repo, ".env"), "TOKEN=old\n")
	appendFile(t, filepath.Join(repo, "pin.toml"), `inject = [".env"]`+"\n")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "inject env")
	git(t, repo, "push")
	oldSHA := git(t, repo, "rev-parse", "HEAD")

	result := runTool(t, runCompiledPin, root, repo, "update")
	requireCode(t, result, 0)
	requireReleaseLink(t, root, "current", oldSHA)

	writeFile(t, filepath.Join(repo, "config", "local.toml"), "mode = \"new\"\n")
	replaceInFile(
		t,
		filepath.Join(repo, "pin.toml"),
		`inject = [".env"]`,
		`inject = [".env", "config/local.toml"]`,
	)
	git(t, repo, "add", "pin.toml", ".gitignore")
	git(t, repo, "commit", "-m", "inject local config")
	git(t, repo, "push")
	newSHA := git(t, repo, "rev-parse", "HEAD")

	result = runTool(t, runCompiledPin, root, repo, "update")
	requireCode(t, result, 0)
	requireReleaseLink(t, root, "current", newSHA)
	requireReleaseLink(t, root, "previous", oldSHA)

	if err := os.Remove(filepath.Join(repo, "config", "local.toml")); err != nil {
		t.Fatal(err)
	}

	result = runTool(t, runCompiledPin, root, repo, "rollback")
	requireCode(t, result, 0)
	requireContains(t, result.stdout, "rolled back: demo-tool "+oldSHA)
	requireReleaseLink(t, root, "current", oldSHA)
	requireReleaseLink(t, root, "previous", newSHA)
}

func TestE2ECompiledBinaryMissingInjectedFileStopsUpdate(t *testing.T) {
	root := t.TempDir()
	repo, _ := sourceRepo(t, root)
	writeScriptTool(t, repo, "1")
	appendFile(t, filepath.Join(repo, "pin.toml"), `inject = [".env"]`+"\n")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "require injected file")
	git(t, repo, "push")

	result := runTool(t, runCompiledPin, root, repo, "update")
	requireCode(t, result, 2)
	requireContains(t, result.stderr, "inject runtime paths: injected path is missing")

	currentLink := filepath.Join(root, "share", "demo-tool", "current")
	if _, err := os.Lstat(currentLink); !os.IsNotExist(err) {
		t.Fatalf("current release exists after missing injected file: %s", currentLink)
	}
}

func TestE2ECompiledBinaryInjectRefusesSymlinkedArchiveParent(t *testing.T) {
	root := t.TempDir()
	repo, _ := sourceRepo(t, root)
	writeScriptTool(t, repo, "1")
	outside := filepath.Join(root, "outside")
	if err := os.Mkdir(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(repo, "config")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(repo, "config")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(outside, "local.toml"), "mode = \"local\"\n")
	appendFile(t, filepath.Join(repo, "pin.toml"), `inject = ["config/local.toml"]`+"\n")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "add symlinked config")
	git(t, repo, "push")

	result := runTool(t, runCompiledPin, root, repo, "update")
	requireCode(t, result, 2)
	requireContains(t, result.stderr, "inject parent is a symlink in archived checkout")
	data, err := os.ReadFile(filepath.Join(outside, "local.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "mode = \"local\"\n" {
		t.Fatalf("source file changed through archive symlink: %q", data)
	}
}

func TestE2ECompiledBinaryTrackedReservedRuntimePathStopsUpdate(t *testing.T) {
	root := t.TempDir()
	repo, _ := sourceRepo(t, root)
	writeFile(t, filepath.Join(repo, ".venv", "tracked.txt"), "tracked\n")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "track reserved runtime path")
	git(t, repo, "push")

	result := runTool(t, runCompiledPin, root, repo, "update")
	requireCode(t, result, 2)
	requireContains(t, result.stderr, "reserved runtime path already exists in archived checkout")
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
		{"source", "../escape.py"},
		{"source", "/tmp/escape.py"},
		{"requirements", "../requirements.txt"},
		{"requirements", "/tmp/requirements.txt"},
		{"inject", "../.env"},
		{"inject", "/tmp/.env"},
		{"inject", ".venv/file"},
		{"inject", ".pin/env"},
	}
	for _, tc := range cases {
		t.Run(tc.key+"="+tc.badValue, func(t *testing.T) {
			root := t.TempDir()
			repo, _ := sourceRepo(t, root)
			if tc.key == "entrypoint" || tc.key == "requirements" {
				appendFile(t, filepath.Join(repo, "pin.toml"), tc.key+` = "`+tc.badValue+`"`+"\n")
			} else if tc.key == "inject" {
				appendFile(t, filepath.Join(repo, "pin.toml"), tc.key+` = ["`+tc.badValue+`"]`+"\n")
			} else {
				oldValue := "demo-tool"
				if tc.key == "source" {
					oldValue = "."
				}
				replacePinValue(t, repo, tc.key, oldValue, tc.badValue)
			}

			result := runTool(t, runPin, root, repo, "update")
			if result.code != 2 {
				t.Fatalf("update code = %d, want 2", result.code)
			}
			want := "pin.toml key \"" + tc.key + "\" must be a single path segment"
			if tc.key == "source" || tc.key == "requirements" || tc.key == "inject" {
				want = "pin.toml key \"" + tc.key + "\" must"
			}
			if tc.key == "inject" && isReservedRuntimePath(filepath.Clean(tc.badValue)) {
				want = `pin.toml key "` + tc.key + `" uses reserved runtime path`
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

func TestConfigRejectsOverlappingInjectedPaths(t *testing.T) {
	root := t.TempDir()
	repo, _ := sourceRepo(t, root)
	appendFile(t, filepath.Join(repo, "pin.toml"), "inject = [\"tokens\", \"tokens/gmail_token.json\"]\n")

	result := runTool(t, runPin, root, repo, "update")
	requireCode(t, result, 2)
	requireContains(t, result.stderr, `pin.toml key "inject" contains overlapping paths`)
}

func TestConfigRejectsRequirementsForPackageSource(t *testing.T) {
	root := t.TempDir()
	repo, _ := sourceRepo(t, root)
	appendFile(t, filepath.Join(repo, "pin.toml"), `requirements = "requirements.txt"`+"\n")

	result := runTool(t, runPin, root, repo, "update")
	requireCode(t, result, 2)
	requireContains(t, result.stderr, `pin.toml key "requirements" requires key "source" to point to a Python script`)
}

func TestConfigRejectsMissingSource(t *testing.T) {
	root := t.TempDir()
	repo, _ := sourceRepo(t, root)
	replaceInFile(t, filepath.Join(repo, "pin.toml"), "source = \".\"\n", "")

	result := runTool(t, runPin, root, repo, "update")
	requireCode(t, result, 2)
	requireContains(t, result.stderr, `pin.toml requires non-empty string key "source"`)
}

func TestConfigRejectsDeprecatedScriptKey(t *testing.T) {
	root := t.TempDir()
	repo, _ := sourceRepo(t, root)
	replacePinValue(t, repo, "source", ".", "automation/demo_tool.py")
	replaceInFile(t, filepath.Join(repo, "pin.toml"), `source = "automation/demo_tool.py"`, `script = "automation/demo_tool.py"`)

	result := runTool(t, runPin, root, repo, "update")
	requireCode(t, result, 2)
	requireContains(t, result.stderr, `pin.toml key "script" has been replaced by key "source"`)
}

func TestConfigDefaultsEntrypointToName(t *testing.T) {
	root := t.TempDir()
	repo, _ := sourceRepo(t, root)

	result := runTool(t, runPin, root, repo, "update")
	requireCode(t, result, 0)
	if _, err := os.Stat(activeEntrypoint(root)); err != nil {
		t.Fatalf("missing default entrypoint in active release: %v", err)
	}
}

func TestEntrypointEnvReplacesExistingPath(t *testing.T) {
	t.Setenv("PATH", "/usr/bin")
	env := entrypointEnv("/tmp/release/.venv/bin/demo-tool")

	pathEntries := 0
	for _, item := range env {
		if strings.HasPrefix(item, "PATH=") {
			pathEntries++
			if item != "PATH=/tmp/release/.venv/bin:/usr/bin" {
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
