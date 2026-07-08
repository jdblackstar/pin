//go:build windows

package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type toolLock struct {
	path string
}

func acquireToolLock(ctx pinContext) (*toolLock, error) {
	if err := os.MkdirAll(ctx.toolRoot(), 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(ctx.toolRoot(), ".pin.lock")
	deadline := time.Now().Add(5 * time.Minute)
	for {
		err := os.Mkdir(path, 0o700)
		if err == nil {
			return &toolLock{path: path}, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("timed out waiting for release lock: %s", path)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (lock *toolLock) release() {
	if lock == nil || lock.path == "" {
		return
	}
	_ = os.Remove(lock.path)
}
