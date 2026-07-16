//go:build windows

package app

import "fmt"

type relaySkillLock struct{}

func acquireRelaySkillLock(path string) (*relaySkillLock, error) {
	return nil, fmt.Errorf("Relay skill management is not supported on Windows: %s", path)
}

func (lock *relaySkillLock) release() {}
