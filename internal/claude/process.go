package claude

import (
	"os"
	"runtime"
	"syscall"
)

// processAlive reports whether a registered Claude Code process still runs, so
// registry files left by a crashed session do not keep it marked live. On
// Windows FindProcess opens the process and fails once it has exited.
func processAlive(pid int) bool {
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	if runtime.GOOS == "windows" {
		_ = process.Release()
		return true
	}
	return process.Signal(syscall.Signal(0)) == nil
}
