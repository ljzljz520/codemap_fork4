//go:build !windows

package handoff

import (
	"os"
	"syscall"
	"time"
)

func init() {
	commitLockWait = 5 * time.Second
}

// processAlive probes liveness with signal 0; FindProcess always succeeds on
// Unix, so the signal result is the only meaningful answer.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}
