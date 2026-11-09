//go:build windows

package handoff

import (
	"os"
	"syscall"
	"time"
)

func init() {
	// Windows commit paths pay ReplaceFileW costs; keep the wait under the
	// 8s hook timeout.
	commitLockWait = 8 * time.Second
}

// processAlive opens the process and reads its exit code: a live process
// reports STILL_ACTIVE (259). os.Process.Signal(0) is unsupported on Windows.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	const stillActive = 259
	handle, err := syscall.OpenProcess(syscall.PROCESS_QUERY_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer syscall.CloseHandle(handle)
	var code uint32
	if err := syscall.GetExitCodeProcess(handle, &code); err != nil {
		return false
	}
	return code == stillActive
}
