//go:build unix

package app

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

type removingDirEntry struct {
	os.DirEntry
	path string
}

func (entry removingDirEntry) Info() (os.FileInfo, error) {
	info, err := entry.DirEntry.Info()
	if err == nil {
		err = os.Remove(entry.path)
	}
	return info, err
}

type errorDirEntry struct {
	os.DirEntry
	err error
}

func (entry errorDirEntry) Info() (os.FileInfo, error) {
	return nil, entry.err
}

func TestInjectRuntimePathsSeedsPrivateFilesAndDirectories(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	writeFile(t, filepath.Join(source, ".env"), "TOKEN=secret\n")
	writeFile(t, filepath.Join(source, "tokens", "token.json"), "secret\n")
	requireChmod(t, filepath.Join(source, ".env"), 0o666)
	requireChmod(t, filepath.Join(source, "tokens"), 0o777)
	requireChmod(t, filepath.Join(source, "tokens", "token.json"), 0o666)

	ctx := pinContext{name: "demo", pinHome: filepath.Join(root, "pin-home")}
	config := config{sourcePath: source, inject: []string{".env", "tokens"}}
	release := filepath.Join(root, "release")
	requireMkdir(t, release)
	if err := injectRuntimePaths(ctx, release, config); err != nil {
		t.Fatal(err)
	}

	requireMode(t, ctx.sharedDir(), 0o700)
	requireMode(t, ctx.sharedPath(".env"), 0o600)
	requireMode(t, ctx.sharedPath("tokens"), 0o700)
	requireMode(t, ctx.sharedPath(filepath.Join("tokens", "token.json")), 0o600)
}

func TestInjectRuntimePathsTightensExistingPathsOnEveryUpdate(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	ctx := pinContext{name: "demo", pinHome: filepath.Join(root, "pin-home")}
	shared := ctx.sharedDir()
	writeFile(t, filepath.Join(shared, ".env"), "TOKEN=existing\n")
	writeFile(t, filepath.Join(shared, "tokens", "token.json"), "existing\n")
	writeFile(t, filepath.Join(shared, "tokens", "owner-read-only"), "existing\n")
	makeSharedTreePermissive(t, shared)
	requireChmod(t, filepath.Join(shared, "tokens", "owner-read-only"), 0o444)

	config := config{sourcePath: source, inject: []string{".env", "tokens"}}
	release1 := filepath.Join(root, "release-1")
	requireMkdir(t, release1)
	if err := injectRuntimePaths(ctx, release1, config); err != nil {
		t.Fatal(err)
	}
	assertSharedTreePrivate(t, shared)
	requireMode(t, filepath.Join(shared, "tokens", "owner-read-only"), 0o400)

	makeSharedTreePermissive(t, shared)
	release2 := filepath.Join(root, "release-2")
	requireMkdir(t, release2)
	if err := injectRuntimePaths(ctx, release2, config); err != nil {
		t.Fatal(err)
	}
	assertSharedTreePrivate(t, shared)
}

func TestHardenInjectedPathToleratesConcurrentRemoval(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "runtime")
	writeFile(t, path, "runtime\n")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	entry := entries[0]

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := hardenInjectedPath(path, entry, nil); err != nil {
		t.Fatalf("removal before Info returned %v", err)
	}

	writeFile(t, path, "runtime\n")
	entries, err = os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	entry = removingDirEntry{DirEntry: entries[0], path: path}
	if err := hardenInjectedPath(path, entry, nil); err != nil {
		t.Fatalf("removal before Chmod returned %v", err)
	}

	walkErr := &os.PathError{Op: "readdir", Path: path, Err: os.ErrNotExist}
	if err := hardenInjectedPath(path, nil, walkErr); err != nil {
		t.Fatalf("walk removal returned %v", err)
	}
}

func TestHardenInjectedPathReturnsOtherErrors(t *testing.T) {
	walkErr := &os.PathError{Op: "readdir", Path: "runtime", Err: os.ErrPermission}
	if err := hardenInjectedPath("runtime", nil, walkErr); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("walk error = %v, want permission denied", err)
	}

	root := t.TempDir()
	path := filepath.Join(root, "runtime")
	writeFile(t, path, "runtime\n")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	entry := errorDirEntry{DirEntry: entries[0], err: os.ErrPermission}
	if err := hardenInjectedPath(path, entry, nil); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("Info error = %v, want permission denied", err)
	}
}

func makeSharedTreePermissive(t *testing.T, shared string) {
	t.Helper()
	requireChmod(t, shared, 0o755)
	requireChmod(t, filepath.Join(shared, ".env"), 0o644)
	requireChmod(t, filepath.Join(shared, "tokens"), 0o755)
	requireChmod(t, filepath.Join(shared, "tokens", "token.json"), 0o644)
}

func assertSharedTreePrivate(t *testing.T, shared string) {
	t.Helper()
	requireMode(t, shared, 0o700)
	requireMode(t, filepath.Join(shared, ".env"), 0o600)
	requireMode(t, filepath.Join(shared, "tokens"), 0o700)
	requireMode(t, filepath.Join(shared, "tokens", "token.json"), 0o600)
}

func requireChmod(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func requireMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
}

func requireMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("mode for %s = %04o, want %04o", path, got, want)
	}
}
