//go:build windows

package app

import "os/exec"

// configureProcessTree documents the direct-child cancellation available on Windows.
func configureProcessTree(*exec.Cmd) {
	// CommandContext terminates the direct child. Windows does not expose a
	// process-group equivalent through os/exec, so descendants are best-effort.
}
