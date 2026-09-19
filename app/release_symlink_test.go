package app

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestUpdateRejectsArchivedSymlinksOutsideRelease(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, root, repo string)
	}{
		{
			name: "absolute package source",
			setup: func(t *testing.T, root, repo string) {
				outside := filepath.Join(root, "outside-package")
				writeFailingBuildPackage(t, outside, filepath.Join(root, "external-step-ran"))
				if err := os.Symlink(outside, filepath.Join(repo, "package")); err != nil {
					t.Fatal(err)
				}
				replacePinValue(t, repo, "source", ".", "package")
			},
		},
		{
			name: "relative script source",
			setup: func(t *testing.T, root, repo string) {
				writeScriptTool(t, repo, "1")
				writeFile(t, filepath.Join(root, "outside-script.py"), "from pathlib import Path\nPath("+strconv.Quote(filepath.Join(root, "external-step-ran"))+").write_text('ran')\n")
				if err := os.Remove(filepath.Join(repo, "automation", "demo_tool.py")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("../../outside-script.py", filepath.Join(repo, "automation", "demo_tool.py")); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "relative requirements file",
			setup: func(t *testing.T, root, repo string) {
				writeScriptTool(t, repo, "1")
				outside := filepath.Join(root, "outside-dependency")
				writeFailingBuildPackage(t, outside, filepath.Join(root, "external-step-ran"))
				writeFile(t, filepath.Join(root, "outside-requirements.txt"), outside+"\n")
				if err := os.Remove(filepath.Join(repo, "requirements.txt")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("../outside-requirements.txt", filepath.Join(repo, "requirements.txt")); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "chained symlink script path component",
			setup: func(t *testing.T, root, repo string) {
				writeScriptTool(t, repo, "1")
				outside := filepath.Join(root, "outside-automation")
				writeFile(t, filepath.Join(outside, "demo_tool.py"), "from pathlib import Path\nPath("+strconv.Quote(filepath.Join(root, "external-step-ran"))+").write_text('ran')\n")
				if err := os.RemoveAll(filepath.Join(repo, "automation")); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(filepath.Join(repo, "nested"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("..", filepath.Join(repo, "nested", "bridge")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("nested/bridge/../../outside-automation", filepath.Join(repo, "automation")); err != nil {
					t.Fatal(err)
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			repo, _ := sourceRepo(t, root)
			test.setup(t, root, repo)
			removePreflight(t, repo)
			git(t, repo, "add", "-A")
			git(t, repo, "commit", "-m", "add escaping symlink")
			git(t, repo, "push")

			result := runTool(t, runPin, root, repo, "update")
			if _, err := os.Stat(filepath.Join(root, "external-step-ran")); !os.IsNotExist(err) {
				t.Fatalf("external build or execution occurred before symlink rejection: %v", err)
			}
			requireCode(t, result, 2)
			requireContains(t, result.stderr, "archived symlink must resolve inside release")
			if _, err := os.Lstat(filepath.Join(root, "share", "demo-tool", "current")); !os.IsNotExist(err) {
				t.Fatalf("current release exists after escaping archive symlink: %v", err)
			}
		})
	}
}

func TestUpdateAllowsArchivedSymlinksWithinRelease(t *testing.T) {
	root := t.TempDir()
	repo, _ := sourceRepo(t, root)
	writeScriptTool(t, repo, "1")

	writeFile(t, filepath.Join(repo, "scripts", "demo_tool.py"), mustReadFile(t, filepath.Join(repo, "automation", "demo_tool.py")))
	if err := os.Remove(filepath.Join(repo, "automation", "demo_tool.py")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../scripts/demo_tool.py", filepath.Join(repo, "automation", "demo_tool.py")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(repo, "config", "requirements.txt"), "")
	if err := os.Remove(filepath.Join(repo, "requirements.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("config/requirements.txt", filepath.Join(repo, "requirements.txt")); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-m", "use in-release symlinks")
	git(t, repo, "push")

	result := runTool(t, runPin, root, repo, "update")
	requireCode(t, result, 0)
	if output := run(t, "", activeEntrypoint(root), "safe"); output != "script 1 from-source safe" {
		t.Fatalf("script output = %q, want %q", output, "script 1 from-source safe")
	}
}

func TestUpdateAllowsPackageSourceSymlinkWithinRelease(t *testing.T) {
	root := t.TempDir()
	repo, _ := sourceRepo(t, root)
	if err := os.Symlink(".", filepath.Join(repo, "package")); err != nil {
		t.Fatal(err)
	}
	replacePinValue(t, repo, "source", ".", "package")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "use in-release package symlink")
	git(t, repo, "push")

	result := runTool(t, runPin, root, repo, "update")
	requireCode(t, result, 0)
	requireInstalledVersion(t, root, "1")
}

func writeFailingBuildPackage(t *testing.T, path, marker string) {
	t.Helper()
	writeFile(t, filepath.Join(path, "pyproject.toml"), `[build-system]
requires = []
build-backend = "backend"
backend-path = ["."]
`)
	writeFile(t, filepath.Join(path, "backend.py"), `from pathlib import Path

def fail():
    Path(`+strconv.Quote(marker)+`).write_text("ran")
    raise RuntimeError("external build ran")

def build_wheel(wheel_directory, config_settings=None, metadata_directory=None):
    return fail()

def prepare_metadata_for_build_wheel(metadata_directory, config_settings=None):
    return fail()
`)
}

func removePreflight(t *testing.T, repo string) {
	t.Helper()
	data := mustReadFile(t, filepath.Join(repo, "pin.toml"))
	for _, line := range []string{
		`preflight = [["python3", "-c", "from pathlib import Path; assert Path('demo_tool.py').is_file()"]]` + "\n",
		`preflight = [["python3", "-c", "from pathlib import Path; compile(Path('automation/demo_tool.py').read_text(), 'automation/demo_tool.py', 'exec')"]]` + "\n",
	} {
		data = strings.Replace(data, line, "", 1)
	}
	writeFile(t, filepath.Join(repo, "pin.toml"), data)
}
