package app

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// integrityCheck selects how verifyReleaseIntegrity obtains file digests.
// Both modes compare every protected path, type, mode, and symlink target with
// the build-time manifest; they differ only in when regular files are rehashed.
type integrityCheck int

const (
	// fullIntegrityCheck rehashes every protected regular file.
	fullIntegrityCheck integrityCheck = iota
	// cachedIntegrityCheck skips rehashing a file whose kernel-reported state
	// is unchanged since it was hashed and matched the manifest, and hashes
	// every other file. Use it for pin run and for checks that immediately
	// follow a full check within the same command.
	cachedIntegrityCheck
)

const (
	integrityCacheName  = "integrity.cache"
	integrityHashBuffer = 64 << 10
)

// integrityRacyWindow keeps recently changed files out of the state cache.
// Filesystem timestamps are coarser than the clock, so a write landing in the
// same timestamp tick as the recorded state could otherwise go unnoticed. Two
// seconds covers the coarsest common granularity (FAT mtime). Tests shorten it.
var integrityRacyWindow = 2 * time.Second

// integrityFileState is the kernel-reported identity of a regular file. On
// Unix, every content write, truncation, chmod, or replacement changes ctime
// or the inode, and unprivileged processes cannot set ctime.
type integrityFileState struct {
	Dev     uint64
	Ino     uint64
	Size    int64
	MtimeNS int64
	CtimeNS int64
}

func (state integrityFileState) settledBefore(cutoff time.Time) bool {
	limit := cutoff.UnixNano()
	return state.MtimeNS < limit && state.CtimeNS < limit
}

// integrityCacheSlot records the settled state in which the regular file at
// the same index in the manifest was last hashed and matched its digest.
type integrityCacheSlot struct {
	known bool
	state integrityFileState
}

func integrityCachePath(release string) string {
	return filepath.Join(release, metadataDir, integrityCacheName)
}

// The cache is a private performance artifact rewritten after verifications
// that hash new files, so it uses a compact binary layout instead of JSON:
// magic, the manifest digest it is bound to, a slot count, and fixed-size slots
// aligned with the manifest entries.
var integrityCacheMagic = []byte("PINIC001")

const integrityCacheSlotSize = 1 + 5*8

func encodeIntegrityCache(manifestDigest string, slots []integrityCacheSlot) []byte {
	data := make([]byte, 0, len(integrityCacheMagic)+len(manifestDigest)+8+len(slots)*integrityCacheSlotSize)
	data = append(data, integrityCacheMagic...)
	data = append(data, manifestDigest...)
	data = binary.LittleEndian.AppendUint64(data, uint64(len(slots)))
	for _, slot := range slots {
		if !slot.known {
			data = append(data, make([]byte, integrityCacheSlotSize)...)
			continue
		}
		data = append(data, 1)
		for _, field := range []uint64{slot.state.Dev, slot.state.Ino, uint64(slot.state.Size), uint64(slot.state.MtimeNS), uint64(slot.state.CtimeNS)} {
			data = binary.LittleEndian.AppendUint64(data, field)
		}
	}
	return data
}

// decodeIntegrityCache returns nil unless data is a well-formed cache bound to
// manifestDigest with one slot per manifest entry.
func decodeIntegrityCache(data []byte, manifestDigest string, entries int) []integrityCacheSlot {
	header := len(integrityCacheMagic) + len(manifestDigest) + 8
	if len(data) < header ||
		!bytes.Equal(data[:len(integrityCacheMagic)], integrityCacheMagic) ||
		string(data[len(integrityCacheMagic):header-8]) != manifestDigest ||
		binary.LittleEndian.Uint64(data[header-8:header]) != uint64(entries) ||
		len(data) != header+entries*integrityCacheSlotSize {
		return nil
	}
	slots := make([]integrityCacheSlot, entries)
	for i := range slots {
		raw := data[header+i*integrityCacheSlotSize:]
		if raw[0] != 1 {
			continue
		}
		field := func(n int) uint64 { return binary.LittleEndian.Uint64(raw[1+8*n:]) }
		slots[i] = integrityCacheSlot{known: true, state: integrityFileState{
			Dev:     field(0),
			Ino:     field(1),
			Size:    int64(field(2)),
			MtimeNS: int64(field(3)),
			CtimeNS: int64(field(4)),
		}}
	}
	return slots
}

// loadIntegrityCache returns nil when the cache is missing, unreadable, or
// belongs to a different manifest; callers then hash every file.
func loadIntegrityCache(release, manifestDigest string, entries int) []integrityCacheSlot {
	data, err := os.ReadFile(integrityCachePath(release))
	if err != nil {
		return nil
	}
	return decodeIntegrityCache(data, manifestDigest, entries)
}

// saveIntegrityCache is best effort: a release directory that cannot be
// written only loses the fast path, never correctness.
func saveIntegrityCache(release, manifestDigest string, previous, slots []integrityCacheSlot) {
	if slices.Equal(previous, slots) {
		return
	}
	data := encodeIntegrityCache(manifestDigest, slots)
	_ = atomicWriteFile(integrityCachePath(release), func(file io.Writer) error {
		_, err := file.Write(data)
		return err
	})
}

type integrityWalkItem struct {
	rel      string // release-relative, slash-separated
	dirEntry os.DirEntry
}

// collectIntegrityEntries walks protected release content in lexical order,
// the same order the manifest was written in. A regular file at index i is
// not rehashed when expected[i] has the same path and known[i] records the
// file's current state; it takes expected[i]'s digest instead. The returned
// slots, aligned with the returned entries, are the states safe to cache.
func collectIntegrityEntries(release string, config config, expected []integrityEntry, known []integrityCacheSlot) ([]integrityEntry, []integrityCacheSlot, error) {
	settledCutoff := time.Now().Add(-integrityRacyWindow)
	root := filepath.Clean(release)
	items, err := walkIntegrityItems(root, config, len(expected))
	if err != nil {
		return nil, nil, err
	}
	entries := make([]integrityEntry, len(items))
	slots := make([]integrityCacheSlot, len(items))
	errs := make([]error, len(items))
	var next atomic.Int64
	var wg sync.WaitGroup
	for range max(1, min(len(items), runtime.GOMAXPROCS(0))) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			hasher := sha256.New()
			buffer := make([]byte, integrityHashBuffer)
			for {
				i := int(next.Add(1) - 1)
				if i >= len(items) {
					return
				}
				var want integrityEntry
				var cached integrityCacheSlot
				if i < len(expected) && i < len(known) && expected[i].Path == items[i].rel {
					want, cached = expected[i], known[i]
				}
				entries[i], slots[i], errs[i] = scanIntegrityItem(root, items[i], want, cached, hasher, buffer, settledCutoff)
			}
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return nil, nil, err
		}
	}
	return entries, slots, nil
}

// walkIntegrityItems lists protected content under the clean root in the same
// pre-order, name-sorted order as filepath.WalkDir, without following
// symlinks. It joins paths by concatenation because every name comes from
// ReadDir. sizeHint is the expected entry count.
func walkIntegrityItems(root string, config config, sizeHint int) ([]integrityWalkItem, error) {
	items := make([]integrityWalkItem, 0, sizeHint)
	var walk func(dir, relDir string) error
	walk = func(dir, relDir string) error {
		dirEntries, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		for _, dirEntry := range dirEntries {
			rel := dirEntry.Name()
			if relDir != "" {
				rel = relDir + "/" + rel
			}
			if integrityPathExcluded(rel, config) {
				continue
			}
			items = append(items, integrityWalkItem{rel: rel, dirEntry: dirEntry})
			if dirEntry.IsDir() {
				if err := walk(dir+string(filepath.Separator)+dirEntry.Name(), rel); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return items, walk(root, "")
}

// scanIntegrityItem builds one entry using caller-owned hashing scratch state.
// A regular file still in cached.state takes want's digest without rehashing.
func scanIntegrityItem(root string, item integrityWalkItem, want integrityEntry, cached integrityCacheSlot, hasher hash.Hash, buffer []byte, settledCutoff time.Time) (integrityEntry, integrityCacheSlot, error) {
	info, err := item.dirEntry.Info()
	if err != nil {
		return integrityEntry{}, integrityCacheSlot{}, err
	}
	path := root + string(filepath.Separator) + filepath.FromSlash(item.rel)
	entry := integrityEntry{Path: item.rel, Mode: formatIntegrityMode(info.Mode().Perm())}
	switch {
	case info.Mode().IsRegular():
		entry.Type = "file"
		state, stateOK := integrityFileStateOf(info)
		if cached.known && want.Type == "file" && stateOK && cached.state == state {
			entry.SHA256 = want.SHA256
			return entry, cached, nil
		}
		var slot integrityCacheSlot
		entry.SHA256, slot, err = hashIntegrityFile(path, state, stateOK, hasher, buffer, settledCutoff)
		return entry, slot, err
	case info.IsDir():
		entry.Type = "directory"
	case info.Mode()&os.ModeSymlink != 0:
		entry.Type = "symlink"
		target, err := os.Readlink(path)
		if err != nil {
			return integrityEntry{}, integrityCacheSlot{}, err
		}
		entry.SHA256 = hashBytes([]byte(target))
	default:
		return integrityEntry{}, integrityCacheSlot{}, fmt.Errorf("unsupported release file type: %s", path)
	}
	return entry, integrityCacheSlot{}, nil
}

// hashIntegrityFile hashes one file. Its state is cacheable only if the open
// file still has the state observed during the walk (so it is the same,
// unmodified inode) and that state is settled.
func hashIntegrityFile(path string, state integrityFileState, stateOK bool, hasher hash.Hash, buffer []byte, settledCutoff time.Time) (string, integrityCacheSlot, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", integrityCacheSlot{}, err
	}
	defer file.Close()
	hasher.Reset()
	// Hide *os.File's WriterTo so the copy uses buffer instead of allocating one.
	if _, err := io.CopyBuffer(hasher, struct{ io.Reader }{file}, buffer); err != nil {
		return "", integrityCacheSlot{}, err
	}
	var sum [sha256.Size]byte
	digest := hex.EncodeToString(hasher.Sum(sum[:0]))
	if !stateOK || !state.settledBefore(settledCutoff) {
		return digest, integrityCacheSlot{}, nil
	}
	info, err := file.Stat()
	if err != nil {
		return digest, integrityCacheSlot{}, nil
	}
	after, ok := integrityFileStateOf(info)
	if !ok || after != state {
		return digest, integrityCacheSlot{}, nil
	}
	return digest, integrityCacheSlot{known: true, state: state}, nil
}

// integrityModeStrings[perm] renders permission bits like fmt's %04o.
var integrityModeStrings = func() [os.ModePerm + 1]string {
	var modes [os.ModePerm + 1]string
	for perm := range modes {
		mode := strconv.FormatUint(uint64(perm), 8)
		modes[perm] = strings.Repeat("0", 4-len(mode)) + mode
	}
	return modes
}()

func formatIntegrityMode(perm os.FileMode) string {
	return integrityModeStrings[perm&os.ModePerm]
}
