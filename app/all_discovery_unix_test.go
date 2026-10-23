//go:build unix

package app

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAllDiscoveryIsolatesCandidateErrors(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission-denied fixture requires an unprivileged user")
	}
	root := t.TempDir()
	repo, sha := namedSourceRepo(t, filepath.Join(root, "beta-source"), "beta")
	requireCode(t, runPinWithHome(t, root, "update", repo), 0)
	legacyHome := filepath.Join(root, ".local", "share")
	loop := filepath.Join(legacyHome, "unrelated", "releases")
	if err := os.MkdirAll(filepath.Dir(loop), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("releases", loop); err != nil {
		t.Fatal(err)
	}
	for _, command := range []struct{ name, summary string }{
		{"verify", "summary: 1 passed, 0 failed"},
		{"check", "summary: 1 current, 0 non-current, 0 failed"},
		{"update", "all tools are current"},
	} {
		result := runPinWithHome(t, root, command.name, "--all")
		requireCode(t, result, 0)
		requireContains(t, result.stdout, command.summary)
		if result.stderr != "" {
			t.Fatalf("unrelated legacy loop produced diagnostics: %s", result.stderr)
		}
	}

	broken := filepath.Join(legacyHome, "pin", "alpha")
	if err := os.MkdirAll(filepath.Join(broken, "releases"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(broken, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(broken, 0o755) })
	if _, err := os.Lstat(filepath.Join(broken, "releases")); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("fixture did not produce a real permission error: %v", err)
	}
	for _, command := range []struct{ name, summary string }{
		{"verify", "summary: 1 passed, 1 failed"},
		{"check", "summary: 1 current, 0 non-current, 1 failed"},
		{"update", "summary: 0 updated, 1 failed"},
	} {
		result := runPinWithHome(t, root, command.name, "--all")
		requireCode(t, result, 1)
		requireContains(t, result.stdout, command.summary)
		requireContains(t, result.stderr, "failed: alpha:")
		requireContains(t, result.stderr, "permission denied")
		if strings.Contains(result.stderr, "unrelated") {
			t.Fatalf("unrelated legacy loop treated as an install: %s", result.stderr)
		}
	}
	result := runPinWithHome(t, root, "list")
	requireCode(t, result, 0)
	requireContains(t, result.stdout, "beta\t"+sha+"\t")
	if strings.Contains(result.stdout, "alpha") || result.stderr != "" {
		t.Fatalf("list did not preserve healthy-only output: %#v", result)
	}
}

func TestLegacyDiscoveryContinuesPastUnreadableRelease(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission-denied fixture requires an unprivileged user")
	}
	root := t.TempDir()
	opts := globalOptions{pinHome: filepath.Join(root, "pin"), legacyPinHome: filepath.Join(root, "share")}
	ctx := pinContext{name: "damaged", pinHome: opts.legacyPinHome}
	unreadable := filepath.Join(ctx.releasesDir(), "a-unreadable")
	for _, path := range []string{unreadable, filepath.Join(ctx.releasesDir(), "z-readable", metadataDir)} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(unreadable, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(unreadable, 0o755) })
	if _, err := os.Lstat(filepath.Join(unreadable, metadataDir)); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("fixture did not produce a real permission error: %v", err)
	}
	names, err := installedToolNames(opts)
	if err != nil || strings.Join(names, ",") != "damaged" {
		t.Fatalf("readable PIN evidence was hidden: names=%v err=%v", names, err)
	}
}

func TestListOmitsMalformedInstallsWithoutHidingHealthyTools(t *testing.T) {
	root := t.TempDir()
	repo, sha := namedSourceRepo(t, filepath.Join(root, "beta-source"), "beta")
	requireCode(t, runPin(t, root, "update", repo), 0)
	writeFile(t, filepath.Join(root, "share", "alpha", "releases", "revision", metadataDir, metadataName), "{")
	if err := os.Symlink("releases/revision", filepath.Join(root, "share", "alpha", "current")); err != nil {
		t.Fatal(err)
	}
	result := runPin(t, root, "list")
	requireCode(t, result, 0)
	requireContains(t, result.stdout, "beta\t"+sha+"\t")
	if strings.Contains(result.stdout, "alpha") || result.stderr != "" {
		t.Fatalf("list did not preserve healthy-only output: %#v", result)
	}
}

func TestAllDiscoveryKeepsHomeEnumerationErrorsFatal(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "not-a-directory")
	writeFile(t, file, "not a directory")
	for _, opts := range []globalOptions{
		{pinHome: file},
		{pinHome: filepath.Join(root, "missing"), legacyPinHome: file},
	} {
		if _, err := installedToolNames(opts); err == nil {
			t.Fatal("home enumeration failure was ignored")
		}
	}
}
