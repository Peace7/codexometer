package claude

import (
	"os"
	"strconv"
	"syscall"
	"testing"
)

func TestProcessAliveRejectsReusedPID(t *testing.T) {
	handle, err := syscall.OpenProcess(processQueryLimitedInformation, false, uint32(os.Getpid()))
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.CloseHandle(handle)
	var creation, exit, kernel, user syscall.Filetime
	if err := syscall.GetProcessTimes(handle, &creation, &exit, &kernel, &user); err != nil {
		t.Fatal(err)
	}
	actual := uint64(creation.HighDateTime)<<32 | uint64(creation.LowDateTime)
	if !processAlive(os.Getpid(), strconv.FormatUint(actual, 10)) {
		t.Fatal("matching start time reported dead")
	}
	if processAlive(os.Getpid(), strconv.FormatUint(actual+1, 10)) {
		t.Fatal("different start time (reused PID) reported alive")
	}
}
