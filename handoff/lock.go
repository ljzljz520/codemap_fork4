package handoff

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"codemap/internal/gitexclude"
)

// ErrCommitLocked reports that another process holds the handoff commit lock.
var ErrCommitLocked = errors.New("handoff commit already in progress")

const commitLockFilename = "handoff.commit.lock"

// commitLockWait is defined per platform (process liveness costs differ).
// It stays below the 8s hook timeout so a blocked session-stop fails visibly.
var commitLockWait time.Duration

type commitLockRecord struct {
	PID       int    `json:"pid"`
	Token     string `json:"token"`
	StartedAt string `json:"started_at"`
}

// commitGates serializes goroutines inside one process before they contend
// for the cross-process O_EXCL lock.
var (
	commitGateMu sync.Mutex
	commitGates  = map[string]*sync.Mutex{}
)

func gateFor(runtimeDir string) *sync.Mutex {
	commitGateMu.Lock()
	defer commitGateMu.Unlock()
	gate, ok := commitGates[runtimeDir]
	if !ok {
		gate = &sync.Mutex{}
		commitGates[runtimeDir] = gate
	}
	return gate
}

// commitLock is the recursive-free, single-release handle returned by
// acquireCommitLock.
type commitLock struct {
	path  string
	token string
	gate  *sync.Mutex
}

// acquireCommitLock serializes whole-generation handoff commits across
// goroutines and processes. A lock left by a dead process is reclaimed; a
// lock held by a live process makes the caller fail with ErrCommitLocked
// after commitLockWait.
func acquireCommitLock(runtimeDir string) (*commitLock, error) {
	gate := gateFor(runtimeDir)
	gate.Lock()
	acquired := false
	defer func() {
		if !acquired {
			gate.Unlock()
		}
	}()

	if err := gitexclude.MkdirAll(runtimeDir, 0o755); err != nil {
		return nil, err
	}
	if info, err := os.Lstat(runtimeDir); err != nil || !info.IsDir() {
		return nil, fmt.Errorf("unsafe runtime directory %q", runtimeDir)
	}

	path := filepath.Join(runtimeDir, commitLockFilename)
	deadline := time.Now().Add(commitLockWait)
	for {
		token, err := tryWriteCommitLock(path)
		if err == nil {
			acquired = true
			return &commitLock{path: path, token: token, gate: gate}, nil
		}
		if !errors.Is(err, ErrCommitLocked) {
			return nil, err
		}
		// Reap a dead holder's lock and retry immediately; otherwise wait.
		if tryReapStaleCommitLock(path) {
			continue
		}
		if time.Now().After(deadline) {
			return nil, ErrCommitLocked
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func tryWriteCommitLock(path string) (string, error) {
	tokenBytes := make([]byte, 16)
	if _, err := rand.Read(tokenBytes); err != nil {
		return "", err
	}
	token := hex.EncodeToString(tokenBytes)
	record := commitLockRecord{
		PID:       os.Getpid(),
		Token:     token,
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}
	payload, err := json.Marshal(record)
	if err != nil {
		return "", err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err == nil {
		if _, writeErr := file.Write(payload); writeErr != nil {
			_ = file.Close()
			_ = os.Remove(path)
			return "", writeErr
		}
		if closeErr := file.Close(); closeErr != nil {
			_ = os.Remove(path)
			return "", closeErr
		}
		return token, nil
	}
	if !os.IsExist(err) {
		return "", err
	}
	return "", ErrCommitLocked
}

// tryReapStaleCommitLock removes the lock only when its recorded process is
// dead and the file is a well-formed regular file. An unreadable lock is
// kept as evidence rather than deleted, so corruption fails closed.
func tryReapStaleCommitLock(path string) bool {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var record commitLockRecord
	if json.Unmarshal(data, &record) != nil || record.PID <= 0 || record.Token == "" {
		return false
	}
	if processAlive(record.PID) {
		return false
	}
	if err := os.Remove(path); err != nil {
		return false
	}
	return true
}

func (l *commitLock) release() {
	if l == nil {
		return
	}
	if l.path != "" {
		if data, err := os.ReadFile(l.path); err == nil {
			var record commitLockRecord
			if json.Unmarshal(data, &record) == nil && record.Token == l.token {
				_ = os.Remove(l.path)
			}
		}
	}
	if l.gate != nil {
		l.gate.Unlock()
	}
}
