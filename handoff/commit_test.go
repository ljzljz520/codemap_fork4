package handoff

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"codemap/internal/projectpath"
)

// commitTestArtifact builds a valid, normalized-looking artifact whose branch
// makes every instance content-distinct unless branches match.
func commitTestArtifact(branch string) *Artifact {
	return &Artifact{
		SchemaVersion: SchemaVersion,
		GeneratedAt:   time.Now(),
		Root:          "/repo",
		Branch:        branch,
		BaseRef:       "main",
		Prefix: PrefixSnapshot{
			FileCount: 3,
			Hubs:      []HubSummary{{Path: "a.go", Importers: 4}},
		},
		Delta: DeltaSnapshot{
			Changed:   []FileStub{{Path: "a.go", Status: "modified"}},
			NextSteps: []string{"run tests"},
		},
	}
}

func commitAgentEntry(agent string, index int, started time.Time) AgentEntry {
	return AgentEntry{
		AgentID:     agent,
		StartedAt:   started.Add(time.Duration(index) * time.Minute),
		EndedAt:     started.Add(time.Duration(index)*time.Minute + time.Second),
		FilesEdited: []string{"a.go", "a.go", "b.go"},
		Summary:     fmt.Sprintf("session-%d", index),
	}
}

func assertArtifactSelfConsistent(t *testing.T, artifact *Artifact) {
	t.Helper()
	prefixHash, _, err := hashCanonical(artifact.Prefix)
	if err != nil {
		t.Fatalf("hash prefix: %v", err)
	}
	deltaHash, _, err := hashCanonical(artifact.Delta)
	if err != nil {
		t.Fatalf("hash delta: %v", err)
	}
	if prefixHash != artifact.PrefixHash {
		t.Fatalf("prefix hash drift: stored %q recomputed %q", artifact.PrefixHash, prefixHash)
	}
	if deltaHash != artifact.DeltaHash {
		t.Fatalf("delta hash drift: stored %q recomputed %q", artifact.DeltaHash, deltaHash)
	}
	if artifact.CombinedHash != hashFromStrings(prefixHash, deltaHash) {
		t.Fatalf("combined hash drift on generation %d", artifact.Generation)
	}
}

func metricsLineCount(t *testing.T, root string) int {
	t.Helper()
	data, err := os.ReadFile(MetricsPath(root))
	if err != nil {
		t.Fatalf("read metrics log: %v", err)
	}
	count := 0
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.TrimSpace(line) != "" {
			count++
		}
	}
	return count
}

func assertNoStagingDirs(t *testing.T, root string) {
	t.Helper()
	runtimeDir, err := projectpath.CheckedRuntimeCodemapDir(root)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(runtimeDir)
	if err != nil {
		t.Fatalf("read runtime dir: %v", err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), stagingDirPrefix) {
			t.Fatalf("staging debris left behind: %s", entry.Name())
		}
	}
}

func TestCommitPublishesWholeGeneration(t *testing.T) {
	root := t.TempDir()
	artifact := commitTestArtifact("feature/a")

	result, err := Commit(root, artifact)
	if err != nil {
		t.Fatalf("Commit failed: %v", err)
	}
	if !result.Changed || result.Generation != 1 {
		t.Fatalf("result = %+v, want changed generation 1", result)
	}

	for _, path := range []string{LatestPath(root), PrefixPath(root), DeltaPath(root), MetricsPath(root)} {
		if info, err := os.Stat(path); err != nil || info.IsDir() {
			t.Fatalf("expected regular file at %s: %v", path, err)
		}
	}

	committed, err := ReadLatest(root)
	if err != nil || committed == nil {
		t.Fatalf("ReadLatest failed: %v", err)
	}
	if committed.Generation != 1 {
		t.Fatalf("committed generation = %d, want 1", committed.Generation)
	}
	assertArtifactSelfConsistent(t, committed)

	var prefix PrefixSnapshot
	data, err := os.ReadFile(PrefixPath(root))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &prefix); err != nil {
		t.Fatalf("prefix snapshot unreadable: %v", err)
	}
	var delta DeltaSnapshot
	data, err = os.ReadFile(DeltaPath(root))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &delta); err != nil {
		t.Fatalf("delta snapshot unreadable: %v", err)
	}
	if len(prefix.Hubs) != 1 || len(delta.Changed) != 1 {
		t.Fatalf("derived snapshots do not match committed generation: %+v %+v", prefix, delta)
	}

	logData, err := os.ReadFile(MetricsPath(root))
	if err != nil {
		t.Fatal(err)
	}
	var line metricsLine
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(logData))), &line); err != nil {
		t.Fatalf("metrics line unreadable: %v", err)
	}
	if line.Generation != 1 || line.CombinedHash != committed.CombinedHash {
		t.Fatalf("metrics line = %+v", line)
	}
}

func TestCommitAdoptsLegacyArtifactAsGenerationZero(t *testing.T) {
	root := t.TempDir()
	runtimeDir, err := projectpath.CheckedRuntimeCodemapDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(runtimeDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Legacy on-disk format: no generation field, history already present.
	legacy := commitTestArtifact("feature/legacy")
	legacy.AgentHistory = []AgentEntry{
		{AgentID: "codex", StartedAt: time.Now().Add(-time.Hour), EndedAt: time.Now()},
	}
	legacyData, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runtimeDir, latestFilename), legacyData, 0o644); err != nil {
		t.Fatal(err)
	}

	migrated, err := ReadLatest(root)
	if err != nil {
		t.Fatalf("legacy artifact must remain readable: %v", err)
	}
	if migrated.Generation != 0 || len(migrated.AgentHistory) != 1 {
		t.Fatalf("legacy read-back = gen %d history %d", migrated.Generation, len(migrated.AgentHistory))
	}

	next := commitTestArtifact("feature/next")
	result, err := Commit(root, next)
	if err != nil {
		t.Fatalf("commit over legacy artifact failed: %v", err)
	}
	if result.Generation != 1 || !result.Changed {
		t.Fatalf("result = %+v, want generation 1", result)
	}

	committed, err := ReadLatest(root)
	if err != nil {
		t.Fatal(err)
	}
	if committed.Generation != 1 {
		t.Fatalf("generation = %d, want 1", committed.Generation)
	}
	if len(committed.AgentHistory) != 1 || committed.AgentHistory[0].AgentID != "codex" {
		t.Fatalf("legacy history not carried forward: %+v", committed.AgentHistory)
	}
}

func TestCommitDuplicateSubmissionIsIdempotent(t *testing.T) {
	root := t.TempDir()
	artifact := commitTestArtifact("feature/dup")

	first, err := Commit(root, artifact)
	if err != nil {
		t.Fatalf("first commit: %v", err)
	}
	second, err := Commit(root, commitTestArtifact("feature/dup"))
	if err != nil {
		t.Fatalf("duplicate commit: %v", err)
	}
	if second.Changed || second.Generation != first.Generation {
		t.Fatalf("duplicate submission rewrote a generation: first %+v second %+v", first, second)
	}
	if metricsLineCount(t, root) != 1 {
		t.Fatal("duplicate submission appended another metrics line")
	}

	// A new session entry makes the same content a new generation; resubmitting
	// that exact generation must not duplicate the entry.
	withSession := commitTestArtifact("feature/dup")
	withSession.AgentHistory = []AgentEntry{
		{AgentID: "claude-code", StartedAt: time.Now(), EndedAt: time.Now().Add(time.Second)},
	}
	third, err := Commit(root, withSession)
	if err != nil {
		t.Fatalf("session commit: %v", err)
	}
	if !third.Changed || third.Generation != 2 {
		t.Fatalf("session result = %+v, want generation 2", third)
	}
	fourth, err := Commit(root, withSession)
	if err != nil {
		t.Fatalf("session resubmission: %v", err)
	}
	if fourth.Changed {
		t.Fatal("resubmitted session generation was not idempotent")
	}
	committed, err := ReadLatest(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(committed.AgentHistory) != 1 {
		t.Fatalf("session entry duplicated: %+v", committed.AgentHistory)
	}
	if metricsLineCount(t, root) != 2 {
		t.Fatalf("metrics lines = %d, want 2", metricsLineCount(t, root))
	}
}

func TestCommitMergesAndDeduplicatesHistory(t *testing.T) {
	root := t.TempDir()
	base := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	entryA := commitAgentEntry("claude-code", 0, base)
	entryB := commitAgentEntry("codex", 1, base)

	first := commitTestArtifact("feature/history")
	first.AgentHistory = []AgentEntry{entryA, entryA, entryB}
	if _, err := Commit(root, first); err != nil {
		t.Fatalf("first commit: %v", err)
	}

	// Same entries again plus a new one; the old ones must not duplicate.
	entryC := commitAgentEntry("cursor", 2, base)
	second := commitTestArtifact("feature/history-2")
	second.AgentHistory = []AgentEntry{entryA, entryC}
	if _, err := Commit(root, second); err != nil {
		t.Fatalf("second commit: %v", err)
	}

	committed, err := ReadLatest(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(committed.AgentHistory) != 3 {
		t.Fatalf("history = %d entries, want 3: %+v", len(committed.AgentHistory), committed.AgentHistory)
	}
	gotAgents := []string{
		committed.AgentHistory[0].AgentID,
		committed.AgentHistory[1].AgentID,
		committed.AgentHistory[2].AgentID,
	}
	wantAgents := []string{"claude-code", "codex", "cursor"}
	if strings.Join(gotAgents, ",") != strings.Join(wantAgents, ",") {
		t.Fatalf("history order = %v, want %v", gotAgents, wantAgents)
	}
	for _, entry := range committed.AgentHistory {
		seen := map[string]bool{}
		for _, path := range entry.FilesEdited {
			if seen[path] {
				t.Fatalf("edited path duplicated inside entry: %+v", entry.FilesEdited)
			}
			seen[path] = true
		}
	}
}

func TestCommitCapsHistoryToNewestEntries(t *testing.T) {
	root := t.TempDir()
	base := time.Now().Add(-10 * time.Hour).Truncate(time.Second)

	first := commitTestArtifact("feature/cap")
	first.AgentHistory = make([]AgentEntry, 15)
	for i := range first.AgentHistory {
		first.AgentHistory[i] = commitAgentEntry("old", i, base)
	}
	if _, err := Commit(root, first); err != nil {
		t.Fatalf("seed commit: %v", err)
	}

	second := commitTestArtifact("feature/cap-2")
	second.AgentHistory = make([]AgentEntry, 10)
	for i := range second.AgentHistory {
		second.AgentHistory[i] = commitAgentEntry("new", i, base.Add(5*time.Hour))
	}
	if _, err := Commit(root, second); err != nil {
		t.Fatalf("overflow commit: %v", err)
	}

	committed, err := ReadLatest(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(committed.AgentHistory) != maxAgentHistoryEntries {
		t.Fatalf("history len = %d, want %d", len(committed.AgentHistory), maxAgentHistoryEntries)
	}
	// 15 old + 10 new = 25, keep newest 20: 10 old entries survive, then 10 new.
	if committed.AgentHistory[0].AgentID != "old" || committed.AgentHistory[19].AgentID != "new" {
		t.Fatalf("cap did not keep the newest 20 entries: %+v", committed.AgentHistory)
	}
}

func TestCommitRecomputesMetricsAgainstLockedPrevious(t *testing.T) {
	root := t.TempDir()
	seed := commitTestArtifact("feature/metrics")
	seedResult, err := Commit(root, seed)
	if err != nil {
		t.Fatalf("seed commit: %v", err)
	}
	committedSeed, err := ReadLatest(root)
	if err != nil {
		t.Fatal(err)
	}

	// Candidate carries deliberately stale telemetry; Commit must overwrite it
	// using the generation actually on disk. A different branch forces a new
	// generation while the snapshots stay identical, so reuse still applies.
	candidate := commitTestArtifact("feature/metrics-2")
	candidate.Metrics = CacheMetrics{PreviousCombinedHash: "stale-hash", ReuseRatio: 0.99}
	result, err := Commit(root, candidate)
	if err != nil {
		t.Fatalf("second commit: %v", err)
	}
	if result.Generation != seedResult.Generation+1 {
		t.Fatalf("generation = %d", result.Generation)
	}

	committed, err := ReadLatest(root)
	if err != nil {
		t.Fatal(err)
	}
	if committed.Metrics.PreviousCombinedHash != committedSeed.CombinedHash {
		t.Fatalf("previous hash = %q, want %q", committed.Metrics.PreviousCombinedHash, committedSeed.CombinedHash)
	}
	if !committed.Metrics.PrefixReused || !committed.Metrics.DeltaReused {
		t.Fatalf("identical snapshots should be marked reused: %+v", committed.Metrics)
	}
	if committed.Metrics.ReuseRatio != 1.0 {
		t.Fatalf("reuse ratio = %v, want 1", committed.Metrics.ReuseRatio)
	}
}

func TestCommitConcurrentSessionsSerializeAndNeverTear(t *testing.T) {
	root := t.TempDir()
	const sessions = 16
	base := time.Now().Add(-time.Hour).Truncate(time.Second)

	stopReaders := make(chan struct{})
	var readerWG sync.WaitGroup
	readerWG.Add(4)
	readerFailures := make(chan error, 32)
	for r := 0; r < 4; r++ {
		go func() {
			defer readerWG.Done()
			for {
				select {
				case <-stopReaders:
					return
				default:
					artifact, err := ReadLatest(root)
					if err != nil {
						readerFailures <- fmt.Errorf("reader error: %w", err)
						return
					}
					if artifact != nil {
						prefixHash, _, _ := hashCanonical(artifact.Prefix)
						deltaHash, _, _ := hashCanonical(artifact.Delta)
						if prefixHash != artifact.PrefixHash || deltaHash != artifact.DeltaHash {
							readerFailures <- fmt.Errorf("torn generation %d observed", artifact.Generation)
							return
						}
					}
				}
			}
		}()
	}

	var wg sync.WaitGroup
	errs := make(chan error, sessions)
	for i := 0; i < sessions; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			artifact := commitTestArtifact(fmt.Sprintf("feature/concurrent-%02d", i))
			artifact.AgentHistory = []AgentEntry{commitAgentEntry(
				fmt.Sprintf("agent-%02d", i), i, base)}
			result, err := Commit(root, artifact)
			if err != nil {
				errs <- err
				return
			}
			if !result.Changed {
				errs <- fmt.Errorf("session %d commit reported no-op", i)
			}
		}(i)
	}
	wg.Wait()
	close(stopReaders)
	readerWG.Wait()
	close(readerFailures)
	for err := range readerFailures {
		t.Fatal(err)
	}
	select {
	case err := <-errs:
		t.Fatalf("concurrent commit failed: %v", err)
	default:
	}

	committed, err := ReadLatest(root)
	if err != nil {
		t.Fatal(err)
	}
	if committed.Generation != sessions {
		t.Fatalf("final generation = %d, want %d", committed.Generation, sessions)
	}
	if len(committed.AgentHistory) != sessions {
		t.Fatalf("history lost under concurrency: got %d entries, want %d", len(committed.AgentHistory), sessions)
	}
	seen := map[string]bool{}
	for _, entry := range committed.AgentHistory {
		if seen[entry.AgentID] {
			t.Fatalf("duplicate session in merged history: %s", entry.AgentID)
		}
		seen[entry.AgentID] = true
	}
	if metricsLineCount(t, root) != sessions {
		t.Fatalf("metrics lines = %d, want %d", metricsLineCount(t, root), sessions)
	}
	assertNoStagingDirs(t, root)
	assertArtifactSelfConsistent(t, committed)
}

func TestCommitFaultAtEveryStageLeavesPreviousGenerationIntact(t *testing.T) {
	stages := []string{
		stageLatest, stagePrefix, stageDelta, stageMetrics,
		publishPrefix, publishDelta, publishMetrics, publishLatest,
	}
	for _, stage := range stages {
		t.Run(stage, func(t *testing.T) {
			root := t.TempDir()
			seed := commitTestArtifact("feature/fault-seed")
			if _, err := Commit(root, seed); err != nil {
				t.Fatalf("seed commit: %v", err)
			}
			previous, err := ReadLatest(root)
			if err != nil {
				t.Fatal(err)
			}

			var once sync.Once
			commitFaultInjection = func(actual string) error {
				if actual == stage {
					var injected error
					once.Do(func() {
						injected = errors.New("injected write failure")
					})
					return injected
				}
				return nil
			}
			t.Cleanup(func() { commitFaultInjection = nil })

			_, err = Commit(root, commitTestArtifact("feature/fault-next"))
			if err == nil {
				t.Fatalf("fault at %s did not surface", stage)
			}

			// Readers must still see the complete previous generation.
			current, err := ReadLatest(root)
			if err != nil {
				t.Fatalf("previous generation unreadable after fault: %v", err)
			}
			if current.Generation != previous.Generation || current.CombinedHash != previous.CombinedHash {
				t.Fatalf("generation changed despite failed commit at %s", stage)
			}
			assertArtifactSelfConsistent(t, current)
			assertNoStagingDirs(t, root)

			// Recovery: the next attempt commits cleanly and prunes any orphan
			// metrics line the interrupted attempt left behind.
			retry, err := Commit(root, commitTestArtifact("feature/fault-next"))
			if err != nil {
				t.Fatalf("retry after fault: %v", err)
			}
			if !retry.Changed || retry.Generation != previous.Generation+1 {
				t.Fatalf("retry result = %+v", retry)
			}
			recovered, err := ReadLatest(root)
			if err != nil {
				t.Fatal(err)
			}
			assertArtifactSelfConsistent(t, recovered)
			if count := metricsLineCount(t, root); count != 2 {
				t.Fatalf("metrics lines after recovery = %d, want 2 (no orphan duplicates)", count)
			}
			assertNoStagingDirs(t, root)
		})
	}
}

func TestCommitRecoversStaleStagingDirectory(t *testing.T) {
	root := t.TempDir()
	runtimeDir, err := projectpath.CheckedRuntimeCodemapDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(runtimeDir, stagingDirPrefix+"dead-process"), 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := Commit(root, commitTestArtifact("feature/recover")); err != nil {
		t.Fatalf("commit with stale staging dir: %v", err)
	}
	assertNoStagingDirs(t, root)
}

func TestCommitRejectsNilCandidate(t *testing.T) {
	if _, err := Commit(t.TempDir(), nil); err == nil {
		t.Fatal("Commit accepted nil artifact")
	}
}

func TestReadLatestFailsClosedOnCorruptArtifact(t *testing.T) {
	root := t.TempDir()
	runtimeDir, err := projectpath.CheckedRuntimeCodemapDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(runtimeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runtimeDir, latestFilename), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadLatest(root); err == nil {
		t.Fatal("corrupt committed artifact did not fail closed")
	}
}

func TestCommitLocksArePerProjectRuntime(t *testing.T) {
	setup := t.TempDir()
	projectpath.SetSetupRoot(setup)
	t.Cleanup(projectpath.ResetSetupRoot)

	rootA, rootB := t.TempDir(), t.TempDir()
	dirA, err := projectpath.CheckedRuntimeCodemapDir(rootA)
	if err != nil {
		t.Fatal(err)
	}
	dirB, err := projectpath.CheckedRuntimeCodemapDir(rootB)
	if err != nil {
		t.Fatal(err)
	}
	if dirA == dirB {
		t.Fatal("projects share a runtime directory")
	}

	lockA, err := acquireCommitLock(dirA)
	if err != nil {
		t.Fatalf("acquire lock A: %v", err)
	}
	t.Cleanup(lockA.release)

	// A held lock in project A must not serialize project B's commits: worktree
	// and project isolation stays intact.
	if _, err := Commit(rootB, commitTestArtifact("feature/isolated")); err != nil {
		t.Fatalf("project B blocked by project A lock: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dirA, commitLockFilename)); err != nil {
		t.Fatalf("project A lock missing: %v", err)
	}
}
