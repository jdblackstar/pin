//go:build windows

package app

import "os"

func prepareInjectedSharedRoot(path string) error {
	return os.MkdirAll(path, 0o700)
}
