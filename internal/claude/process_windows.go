package claude

import (
	"strconv"
	"syscall"
)

const processQueryLimitedInformation = 0x1000

// processAlive reports whether a registered Claude Code process still runs.
// Windows reuses PIDs, so the registry's procStart (the process creation time
// as a FILETIME) must match the running process, when it is recorded.
func processAlive(pid int, procStart string) bool {
	handle, err := syscall.OpenProcess(processQueryLimitedInformation, false, uint32(pid))
	if err != nil {
		return false
	}
	defer syscall.CloseHandle(handle)
	var creation, exit, kernel, user syscall.Filetime
	if err := syscall.GetProcessTimes(handle, &creation, &exit, &kernel, &user); err != nil {
		return false
	}
	if exit.HighDateTime != 0 || exit.LowDateTime != 0 {
		return false
	}
	if procStart == "" {
		return true
	}
	want, err := strconv.ParseUint(procStart, 10, 64)
	if err != nil {
		return true
	}
	return uint64(creation.HighDateTime)<<32|uint64(creation.LowDateTime) == want
}
