//go:build unix

package app

import (
	"errors"
	"os"
	"path/filepath"
)

func prepareInjectedSharedRoot(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	return filepath.WalkDir(path, hardenInjectedPath)
}

func hardenInjectedPath(path string, entry os.DirEntry, walkErr error) error {
	if errors.Is(walkErr, os.ErrNotExist) {
		return nil
	}
	if walkErr != nil {
		return walkErr
	}
	if entry.Type()&os.ModeSymlink != 0 {
		return nil
	}
	info, err := entry.Info()
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	mode := privateInjectedMode(info.Mode())
	if info.Mode().Perm() == mode {
		return nil
	}
	if err := os.Chmod(path, mode); errors.Is(err, os.ErrNotExist) {
		return nil
	} else {
		return err
	}
}
