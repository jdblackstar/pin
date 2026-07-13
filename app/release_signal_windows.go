//go:build windows

package app

import "os"

func activationSignals() []os.Signal {
	return []os.Signal{os.Interrupt}
}

func exitCodeForSignal(os.Signal) int {
	return 130
}
