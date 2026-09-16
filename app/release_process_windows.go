//go:build windows

package app

import "os/exec"

func configureProcessTree(*exec.Cmd) {
	// CommandContext terminates the direct child. Windows does not expose a
	// process-group equivalent through os/exec, so descendants are best-effort.
}
