package handoff

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"codemap/internal/projectpath"
)

// TestHelperProcessHandoffLock is a re-executed copy of the test binary that
// acquires the real cross-process commit lock and then sleeps until the parent
// kills it. It only runs when CODEMAP_TEST_HANDOFF_HELPER is set, so a normal
// `go test` invocation skips it.
func TestHelperProcessHandoffLock(t *testing.T) {
	if os.Getenv("CODEMAP_TEST_HANDOFF_HELPER") == "" {
		t.Skip()
	}
	root := argAfterDashDash(os.Args)
	marker := os.Getenv("CODEMAP_TEST_HANDOFF_MARKER")
	fail := func(message string) {
		_ = os.WriteFile(marker, []byte("err: "+message), 0o644)
	}

	runtimeDir, err := projectpath.CheckedRuntimeCodemapDir(root)
	if err != nil {
		fail(err.Error())
		return
	}
	lock, err := acquireCommitLock(runtimeDir)
	if err != nil {
		fail(err.Error())
		return
	}
	defer lock.release()
	if err := os.WriteFile(marker, []byte("ready"), 0o644); err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Second)
}

func argAfterDashDash(args []string) string {
	for i, a := range args {
		if a == "--" && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// startLockHelper spawns the helper process and blocks until it has acquired
// the commit lock and signaled readiness. The returned stop function kills
// and reaps the helper; it is also registered with t.Cleanup.
func startLockHelper(t *testing.T, root string) func() {
	t.Helper()
	marker := filepath.Join(t.TempDir(), "ready")
	cmd := exec.Command(os.Args[0], "-test.run=TestHelperProcessHandoffLock", "--", root)
	cmd.Env = append(os.Environ(),
		"CODEMAP_TEST_HANDOFF_HELPER=1",
		"CODEMAP_TEST_HANDOFF_MARKER="+marker,
	)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start lock helper: %v", err)
	}
	waited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(waited) }()

	deadline := time.Now().Add(10 * time.Second)
	for {
		data, err := os.ReadFile(marker)
		if err == nil {
			if string(data) == "ready" {
				break
			}
			t.Fatalf("lock helper failed to acquire: %s", string(data))
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			t.Fatal("lock helper never became ready")
		}
		time.Sleep(10 * time.Millisecond)
	}

	stop := func() { _ = cmd.Process.Kill(); <-waited }
	t.Cleanup(stop)
	return stop
}

func TestCommitLockReclaimsDeadProcessLock(t *testing.T) {
	root := t.TempDir()
	runtimeDir, err := projectpath.CheckedRuntimeCodemapDir(root)
	if err != nil {
		t.Fatal(err)
	}
	stop := startLockHelper(t, root)

	lockPath := filepath.Join(runtimeDir, commitLockFilename)
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("helper lock missing: %v", err)
	}

	// Killing the holder simulates a process interrupted mid-commit: the next
	// commit must detect the dead PID, reap the lock and succeed.
	stop()
	result, err := Commit(root, commitTestArtifact("feature/reaped"))
	if err != nil {
		t.Fatalf("commit after dead holder failed: %v", err)
	}
	if !result.Changed || result.Generation != 1 {
		t.Fatalf("result = %+v, want generation 1", result)
	}
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("released lock file still present: %v", err)
	}
	committed, err := ReadLatest(root)
	if err != nil || committed == nil {
		t.Fatalf("ReadLatest after reclaim: %v", err)
	}
}

func TestCommitLockRejectedByLiveHolder(t *testing.T) {
	root := t.TempDir()
	startLockHelper(t, root)
	runtimeDir, err := projectpath.CheckedRuntimeCodemapDir(root)
	if err != nil {
		t.Fatal(err)
	}

	previousWait := commitLockWait
	commitLockWait = 200 * time.Millisecond
	t.Cleanup(func() { commitLockWait = previousWait })

	start := time.Now()
	lock, err := acquireCommitLock(runtimeDir)
	elapsed := time.Since(start)
	if lock != nil {
		lock.release()
		t.Fatal("acquire succeeded while a live process held the lock")
	}
	if !errors.Is(err, ErrCommitLocked) {
		t.Fatalf("err = %v, want ErrCommitLocked", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("lock wait took %v, expected to honor the shortened bound", elapsed)
	}
}
