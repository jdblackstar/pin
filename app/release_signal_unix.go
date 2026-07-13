//go:build !windows

package app

import (
	"os"
	"syscall"
)

func activationSignals() []os.Signal {
	return []os.Signal{os.Interrupt, syscall.SIGTERM}
}

func exitCodeForSignal(signal os.Signal) int {
	switch signal {
	case os.Interrupt:
		return 130
	case syscall.SIGTERM:
		return 143
	default:
		return 1
	}
}
