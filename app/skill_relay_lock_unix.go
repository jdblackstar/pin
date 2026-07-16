//go:build unix

package app

import (
	"os"
	"path/filepath"
	"syscall"
)

type relaySkillLock struct {
	file *os.File
}

func acquireRelaySkillLock(path string) (*relaySkillLock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		_ = file.Close()
		return nil, err
	}
	return &relaySkillLock{file: file}, nil
}

func (lock *relaySkillLock) release() {
	if lock == nil || lock.file == nil {
		return
	}
	_ = syscall.Flock(int(lock.file.Fd()), syscall.LOCK_UN)
	_ = lock.file.Close()
}
