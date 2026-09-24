//go:build linux || darwin

package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

func newIntegrityTestRelease(t *testing.T, files map[string]string) (string, config) {
	t.Helper()
	release := t.TempDir()
	for rel, content := range files {
		writeFile(t, filepath.Join(release, rel), content)
	}
	cfg := config{raw: map[string]any{"name": "demo", "source": "a.py"}}
	if err := writeReleaseMetadata(release, release, cfg, "test"); err != nil {
		t.Fatal(err)
	}
	return release, cfg
}

// settleIntegrityFiles shortens the racy window and waits it out so files
// written so far become cacheable. The window stays far above timestamp
// granularity, so a later write always changes ctime.
func settleIntegrityFiles(t *testing.T) {
	t.Helper()
	previous := integrityRacyWindow
	integrityRacyWindow = 50 * time.Millisecond
	t.Cleanup(func() { integrityRacyWindow = previous })
	time.Sleep(150 * time.Millisecond)
}

func readTestIntegrityManifest(t *testing.T, release string) (string, integrityManifest) {
	t.Helper()
	metadata, err := readReleaseMetadata(release)
	if err != nil {
		t.Fatal(err)
	}
	reference, err := parseIntegrityReference(metadata)
	if err != nil {
		t.Fatal(err)
	}
	var manifest integrityManifest
	if err := json.Unmarshal([]byte(mustReadFile(t, filepath.Join(release, metadataDir, integrityName))), &manifest); err != nil {
		t.Fatal(err)
	}
	return reference.ManifestSHA256, manifest
}

func knownIntegrityCacheSlots(t *testing.T, release string) int {
	t.Helper()
	digest, manifest := readTestIntegrityManifest(t, release)
	known := 0
	for _, slot := range loadIntegrityCache(release, digest, len(manifest.Entries)) {
		if slot.known {
			known++
		}
	}
	return known
}

// forgeIntegrityCacheSlot records rel's current state as verified, as if the
// file had been hashed in that state and matched the manifest.
func forgeIntegrityCacheSlot(t *testing.T, release, rel string) {
	t.Helper()
	digest, manifest := readTestIntegrityManifest(t, release)
	slots := loadIntegrityCache(release, digest, len(manifest.Entries))
	if slots == nil {
		slots = make([]integrityCacheSlot, len(manifest.Entries))
	}
	info, err := os.Lstat(filepath.Join(release, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	state, ok := integrityFileStateOf(info)
	if !ok {
		t.Fatal("file state is unavailable")
	}
	for i, entry := range manifest.Entries {
		if entry.Path == rel {
			slots[i] = integrityCacheSlot{known: true, state: state}
			if err := os.WriteFile(integrityCachePath(release), encodeIntegrityCache(digest, slots), 0o644); err != nil {
				t.Fatal(err)
			}
			return
		}
	}
	t.Fatalf("manifest has no entry for %s", rel)
}

func requireIntegrityError(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("integrity check passed, want error containing %q", want)
	}
	requireContains(t, err.Error(), want)
}

func TestIntegrityCacheOnlyRecordsSettledFiles(t *testing.T) {
	previous := integrityRacyWindow
	integrityRacyWindow = time.Hour
	t.Cleanup(func() { integrityRacyWindow = previous })
	release, cfg := newIntegrityTestRelease(t, map[string]string{"a.py": "a\n", "pkg/b.py": "b\n"})
	if err := verifyReleaseIntegrity(release, cfg, fullIntegrityCheck); err != nil {
		t.Fatal(err)
	}
	if known := knownIntegrityCacheSlots(t, release); known != 0 {
		t.Fatalf("known cache slots for files changed within the racy window = %d, want 0", known)
	}

	settleIntegrityFiles(t)
	if err := verifyReleaseIntegrity(release, cfg, fullIntegrityCheck); err != nil {
		t.Fatal(err)
	}
	if known := knownIntegrityCacheSlots(t, release); known != 2 {
		t.Fatalf("known cache slots for settled files = %d, want 2 (a.py and pkg/b.py)", known)
	}
}

func TestCachedIntegrityTrustsRecordedStateButFullCheckRehashes(t *testing.T) {
	release, cfg := newIntegrityTestRelease(t, map[string]string{"a.py": "a\n", "pkg/b.py": "original\n"})
	settleIntegrityFiles(t)
	if err := verifyReleaseIntegrity(release, cfg, fullIntegrityCheck); err != nil {
		t.Fatal(err)
	}

	writeFile(t, filepath.Join(release, "pkg", "b.py"), "tampered\n")
	forgeIntegrityCacheSlot(t, release, "pkg/b.py")

	// A forged cache is outside the drift model, like a rewritten manifest;
	// passing here proves the cached check skipped rehashing.
	if err := verifyReleaseIntegrity(release, cfg, cachedIntegrityCheck); err != nil {
		t.Fatalf("cached check rehashed a file whose state was recorded: %v", err)
	}
	requireIntegrityError(t, verifyReleaseIntegrity(release, cfg, fullIntegrityCheck), "content changed for pkg/b.py")
}

func TestCachedIntegrityDetectsKernelVisibleChanges(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(t *testing.T, path string)
		want   string
	}{
		{
			name: "same-size rewrite with restored mtime",
			mutate: func(t *testing.T, path string) {
				info, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				writeFile(t, path, "tampered\n")
				if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
					t.Fatal(err)
				}
			},
			want: "content changed for pkg/b.py",
		},
		{
			name: "same-size replacement by rename with restored mtime",
			mutate: func(t *testing.T, path string) {
				info, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				replacement := path + ".new"
				writeFile(t, replacement, "tampered\n")
				if err := os.Chtimes(replacement, info.ModTime(), info.ModTime()); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(replacement, path); err != nil {
					t.Fatal(err)
				}
			},
			want: "content changed for pkg/b.py",
		},
		{
			name: "permission change",
			mutate: func(t *testing.T, path string) {
				if err := os.Chmod(path, 0o755); err != nil {
					t.Fatal(err)
				}
			},
			want: "mode changed for pkg/b.py",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			release, cfg := newIntegrityTestRelease(t, map[string]string{"a.py": "a\n", "pkg/b.py": "original\n"})
			settleIntegrityFiles(t)
			if err := verifyReleaseIntegrity(release, cfg, fullIntegrityCheck); err != nil {
				t.Fatal(err)
			}
			if known := knownIntegrityCacheSlots(t, release); known != 2 {
				t.Fatalf("known cache slots = %d, want 2", known)
			}
			tc.mutate(t, filepath.Join(release, "pkg", "b.py"))
			requireIntegrityError(t, verifyReleaseIntegrity(release, cfg, cachedIntegrityCheck), tc.want)
		})
	}
}

func TestCachedIntegrityDetectsEntryAddedBeforeCachedFiles(t *testing.T) {
	release, cfg := newIntegrityTestRelease(t, map[string]string{"b.py": "b\n", "c.py": "c\n"})
	settleIntegrityFiles(t)
	if err := verifyReleaseIntegrity(release, cfg, fullIntegrityCheck); err != nil {
		t.Fatal(err)
	}
	// Sorting first shifts every later entry away from its cache slot.
	writeFile(t, filepath.Join(release, "a.py"), "a\n")
	requireIntegrityError(t, verifyReleaseIntegrity(release, cfg, cachedIntegrityCheck), "unexpected a.py")
}

func TestIntegrityCacheIgnoresUnusableCache(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cache func(digest string, slots []integrityCacheSlot) []byte
	}{
		{"other manifest", func(_ string, slots []integrityCacheSlot) []byte {
			return encodeIntegrityCache(hashBytes([]byte("other manifest")), slots)
		}},
		{"truncated", func(digest string, slots []integrityCacheSlot) []byte {
			data := encodeIntegrityCache(digest, slots)
			return data[:len(data)-1]
		}},
		{"wrong slot count", func(digest string, slots []integrityCacheSlot) []byte {
			return encodeIntegrityCache(digest, append(slots, integrityCacheSlot{}))
		}},
		{"garbage", func(string, []integrityCacheSlot) []byte { return []byte("{}") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			release, cfg := newIntegrityTestRelease(t, map[string]string{"a.py": "a\n", "pkg/b.py": "original\n"})
			digest, manifest := readTestIntegrityManifest(t, release)
			writeFile(t, filepath.Join(release, "pkg", "b.py"), "tampered\n")
			// Every slot claims the current state, so trusting this cache would
			// hide the tampering.
			slots := make([]integrityCacheSlot, len(manifest.Entries))
			for i, entry := range manifest.Entries {
				info, err := os.Lstat(filepath.Join(release, filepath.FromSlash(entry.Path)))
				if err != nil {
					t.Fatal(err)
				}
				state, _ := integrityFileStateOf(info)
				slots[i] = integrityCacheSlot{known: entry.Type == "file", state: state}
			}
			if err := os.WriteFile(integrityCachePath(release), tc.cache(digest, slots), 0o644); err != nil {
				t.Fatal(err)
			}
			requireIntegrityError(t, verifyReleaseIntegrity(release, cfg, cachedIntegrityCheck), "content changed for pkg/b.py")
		})
	}
}

func TestIntegrityCacheIgnoresOversizedCacheWithoutReadingIt(t *testing.T) {
	release, cfg := newIntegrityTestRelease(t, map[string]string{"a.py": "a\n"})
	digest, manifest := readTestIntegrityManifest(t, release)
	cachePath := integrityCachePath(release)
	if err := os.WriteFile(cachePath, encodeIntegrityCache(digest, make([]integrityCacheSlot, len(manifest.Entries))), 0o644); err != nil {
		t.Fatal(err)
	}
	// A valid prefix extended to a sparse 8 GiB file.
	if err := os.Truncate(cachePath, 8<<30); err != nil {
		t.Fatal(err)
	}

	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	slots := loadIntegrityCache(release, digest, len(manifest.Entries))
	runtime.ReadMemStats(&after)
	if slots != nil {
		t.Fatal("oversized cache was accepted")
	}
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 1<<20 {
		t.Fatalf("loading an oversized cache allocated %d bytes, want it rejected before reading", allocated)
	}
	if err := verifyReleaseIntegrity(release, cfg, cachedIntegrityCheck); err != nil {
		t.Fatal(err)
	}
}

func TestIntegrityCacheIgnoresFIFOWithoutBlocking(t *testing.T) {
	release, cfg := newIntegrityTestRelease(t, map[string]string{"a.py": "a\n"})
	digest, manifest := readTestIntegrityManifest(t, release)
	cachePath := integrityCachePath(release)
	if err := os.Remove(cachePath); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(cachePath, 0o644); err != nil {
		t.Fatal(err)
	}

	loaded := make(chan []integrityCacheSlot, 1)
	go func() { loaded <- loadIntegrityCache(release, digest, len(manifest.Entries)) }()
	select {
	case slots := <-loaded:
		if slots != nil {
			t.Fatal("FIFO cache was accepted")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("loading a FIFO cache blocked, want it rejected without waiting for a writer")
	}
	if err := verifyReleaseIntegrity(release, cfg, cachedIntegrityCheck); err != nil {
		t.Fatal(err)
	}
}

func TestIntegrityCacheRoundTrip(t *testing.T) {
	digest := hashBytes([]byte("manifest"))
	slots := []integrityCacheSlot{
		{},
		{known: true, state: integrityFileState{Dev: 1<<63 + 5, Ino: 42, Size: 7, MtimeNS: 1_700_000_000_123_456_789, CtimeNS: -1}},
		{known: true, state: integrityFileState{Dev: 3, Ino: 1<<40 + 1, Size: 1 << 33, MtimeNS: 2, CtimeNS: 1}},
	}
	got := decodeIntegrityCache(encodeIntegrityCache(digest, slots), digest, len(slots))
	if len(got) != len(slots) {
		t.Fatalf("decoded %d slots, want %d", len(got), len(slots))
	}
	for i := range slots {
		if got[i] != slots[i] {
			t.Fatalf("slot %d = %#v, want %#v", i, got[i], slots[i])
		}
	}
}

func TestIntegrityCheckModesByCommand(t *testing.T) {
	root := t.TempDir()
	repo, firstSHA := sourceRepo(t, root)
	requireCode(t, runTool(t, runPin, root, repo, "update"), 0)
	secondSHA := commitToolVersion(t, repo, "2", false)
	git(t, repo, "push")
	requireCode(t, runTool(t, runPin, root, repo, "update"), 0)

	releases := filepath.Join(root, "share", "demo-tool", "releases")
	for _, sha := range []string{firstSHA, secondSHA} {
		release := filepath.Join(releases, sha)
		replaceInFile(t, filepath.Join(release, "demo_tool.py"), "print(", "print (")
		forgeIntegrityCacheSlot(t, release, "demo_tool.py")
	}

	// pin run trusts recorded file states.
	result := runPin(t, root, "run", "demo-tool")
	requireCode(t, result, 0)
	requireContains(t, result.stdout, "demo 2")

	// Explicit verification, same-SHA reuse, and rollback rehash everything
	// before running any release code, not only in the post-verify recheck.
	for _, command := range [][]string{{"verify", "demo-tool"}, {"update", repo}, {"rollback", "demo-tool"}} {
		result = runPin(t, root, command...)
		requireCode(t, result, 2)
		requireContains(t, result.stderr, "release integrity mismatch: content changed for demo_tool.py")
		if strings.Contains(result.stderr, "recheck release integrity") {
			t.Fatalf("pin %v ran verify commands before detecting the change:\n%s", command, result.stderr)
		}
	}
}
