//go:build !windows

package claude

import (
	"os"
	"syscall"
)

// processAlive reports whether a registered Claude Code process still runs.
// procStart is platform-specific and only checked on Windows.
func processAlive(pid int, _ string) bool {
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return process.Signal(syscall.Signal(0)) == nil
}
