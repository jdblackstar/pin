//go:build !linux && !darwin

package app

import "os"

// Platforms without a portable inode and change time never reuse cached
// digests, so every integrity check hashes every protected file.
func integrityFileStateOf(os.FileInfo) (integrityFileState, bool) {
	return integrityFileState{}, false
}
