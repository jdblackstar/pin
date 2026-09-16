//go:build unix

package app

import (
	"os"
	"path/filepath"
)

func prepareInjectedSharedRoot(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	return filepath.WalkDir(path, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		mode := privateInjectedMode(info.Mode())
		if info.Mode().Perm() == mode {
			return nil
		}
		return os.Chmod(path, mode)
	})
}
