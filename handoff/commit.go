package handoff

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"codemap/internal/gitexclude"
	"codemap/internal/projectpath"
	"codemap/internal/runtimefile"
)

const (
	// maxAgentHistoryEntries caps how many agent sessions one artifact carries.
	maxAgentHistoryEntries = 20
	// stagingDirPrefix marks private staging directories created inside the
	// runtime directory; interrupted commits leave them behind for recovery.
	stagingDirPrefix = ".handoff.staging-"
)

// CommitResult reports what one Commit changed.
type CommitResult struct {
	// Generation is the sequence number of the committed generation. For a
	// no-op commit it equals the already-committed generation.
	Generation int64
	// Changed is false when the candidate matched the committed generation,
	// including identical re-submission: nothing was rewritten.
	Changed bool
	// Path is the location of the committed latest artifact.
	Path string
}

// commitFaultInjection, when non-nil, is invoked at every persistence stage
// so tests can simulate write failures and process interruptions. Production
// code never sets it.
var commitFaultInjection func(stage string) error

func commitFault(stage string) error {
	if commitFaultInjection == nil {
		return nil
	}
	return commitFaultInjection(stage)
}

// Commit persistence stages. Order matters: derived files and the metrics log
// are replaced before handoff.latest.json, whose rename is the single commit
// point. Readers of handoff.latest.json therefore observe either the complete
// previous generation or the complete new generation, never a mixture.
const (
	stageLatest    = "stage-latest"
	stagePrefix    = "stage-prefix"
	stageDelta     = "stage-delta"
	stageMetrics   = "stage-metrics"
	publishPrefix  = "publish-prefix"
	publishDelta   = "publish-delta"
	publishMetrics = "publish-metrics"
	publishLatest  = "publish-latest"
)

// Commit publishes candidate as the next handoff generation. It is the single
// persistence entry point used by the CLI, the MCP server and session-stop
// hooks.
//
// Under one cross-process lock it: reads the committed generation, merges and
// deduplicates agent session history, recomputes previous-generation reuse
// metrics against the generation actually on disk, stages every output in a
// private directory, and publishes derived files before atomically renaming
// handoff.latest.json last. Any failure before that rename leaves the previous
// generation fully in place.
func Commit(root string, candidate *Artifact) (*CommitResult, error) {
	if candidate == nil {
		return nil, errors.New("handoff: cannot commit a nil artifact")
	}

	runtimeDir, err := projectpath.CheckedRuntimeCodemapDir(root)
	if err != nil {
		return nil, err
	}
	if err := gitexclude.MkdirAll(runtimeDir, 0o755); err != nil {
		return nil, err
	}

	lock, err := acquireCommitLock(runtimeDir)
	if err != nil {
		return nil, err
	}
	defer lock.release()

	// The lock guarantees no other commit is live, so every staging directory
	// present here is debris from an interrupted process.
	if err := recoverStagingDirs(runtimeDir); err != nil {
		return nil, err
	}

	previous, err := readCommittedAt(runtimeDir)
	if err != nil {
		return nil, err
	}

	next := *candidate
	normalizeArtifact(&next)

	mergedHistory, historyAdded := mergeAgentHistory(agentHistoryOf(previous), next.AgentHistory)
	next.AgentHistory = mergedHistory

	prefixHash, prefixBytes, err := hashCanonical(next.Prefix)
	if err != nil {
		return nil, fmt.Errorf("handoff: hash prefix snapshot: %w", err)
	}
	deltaHash, deltaBytes, err := hashCanonical(next.Delta)
	if err != nil {
		return nil, fmt.Errorf("handoff: hash delta snapshot: %w", err)
	}
	next.PrefixHash = prefixHash
	next.DeltaHash = deltaHash
	next.CombinedHash = hashFromStrings(prefixHash, deltaHash)

	// Reuse metrics are computed against the generation read under the lock,
	// not the possibly-stale artifact consulted while building the candidate.
	next.Metrics = buildCacheMetrics(previous, prefixHash, deltaHash, prefixBytes, deltaBytes)

	if previous != nil && previous.PrefixHash == prefixHash && previous.DeltaHash == deltaHash &&
		!previous.GeneratedAt.IsZero() {
		next.GeneratedAt = previous.GeneratedAt
	} else if next.GeneratedAt.IsZero() {
		next.GeneratedAt = time.Now()
	}

	latestPath := filepath.Join(runtimeDir, latestFilename)
	if previous != nil && !historyAdded && sameGenerationContent(&next, previous) {
		// Idempotent re-submission of the same generation.
		return &CommitResult{Generation: previous.Generation, Changed: false, Path: latestPath}, nil
	}

	if previous != nil {
		next.Generation = previous.Generation + 1
	} else {
		next.Generation = 1
	}
	normalizeArtifact(&next)

	staging, err := os.MkdirTemp(runtimeDir, stagingDirPrefix+"*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(staging)

	latestData, err := json.MarshalIndent(&next, "", "  ")
	if err != nil {
		return nil, err
	}
	prefixData, err := json.MarshalIndent(next.Prefix, "", "  ")
	if err != nil {
		return nil, err
	}
	deltaData, err := json.MarshalIndent(next.Delta, "", "  ")
	if err != nil {
		return nil, err
	}
	metricsLine, err := marshalMetricsLine(&next)
	if err != nil {
		return nil, err
	}

	stage := func(stageName, filename string, data []byte) error {
		if err := commitFault(stageName); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(staging, filename), data, 0o644)
	}
	if err := stage(stageLatest, latestFilename, latestData); err != nil {
		return nil, err
	}
	if err := stage(stagePrefix, prefixFilename, prefixData); err != nil {
		return nil, err
	}
	if err := stage(stageDelta, deltaFilename, deltaData); err != nil {
		return nil, err
	}
	metricsData, err := mergeMetricsLog(runtimeDir, metricsLine, next.Generation)
	if err != nil {
		return nil, err
	}
	if err := commitFault(stageMetrics); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(staging, metricsFilename), metricsData, 0o644); err != nil {
		return nil, err
	}

	// Publish derived state first. Every rename is individually atomic and no
	// other writer can interleave because the commit lock is held.
	publish := func(stageName, filename string, data []byte) error {
		if err := commitFault(stageName); err != nil {
			return err
		}
		return runtimefile.WriteAtomic(filepath.Join(runtimeDir, filename), data, 0o644)
	}
	if err := publish(publishPrefix, prefixFilename, prefixData); err != nil {
		return nil, err
	}
	if err := publish(publishDelta, deltaFilename, deltaData); err != nil {
		return nil, err
	}
	if err := publish(publishMetrics, metricsFilename, metricsData); err != nil {
		return nil, err
	}
	// The commit point: after this rename every reader sees the new generation.
	if err := publish(publishLatest, latestFilename, latestData); err != nil {
		return nil, err
	}

	return &CommitResult{Generation: next.Generation, Changed: true, Path: latestPath}, nil
}

// readCommittedAt loads and normalizes the generation currently committed in
// runtimeDir. Missing storage yields (nil, nil); an unreadable artifact is an
// error rather than a silent reset, so corruption never destroys evidence.
func readCommittedAt(runtimeDir string) (*Artifact, error) {
	path := filepath.Join(runtimeDir, latestFilename)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var artifact Artifact
	if err := json.Unmarshal(data, &artifact); err != nil {
		return nil, fmt.Errorf("handoff: committed artifact %q is unreadable: %w", path, err)
	}
	normalizeArtifact(&artifact)
	return &artifact, nil
}

func recoverStagingDirs(runtimeDir string) error {
	entries, err := os.ReadDir(runtimeDir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() && strings.HasPrefix(entry.Name(), stagingDirPrefix) {
			if err := os.RemoveAll(filepath.Join(runtimeDir, entry.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

func agentHistoryOf(artifact *Artifact) []AgentEntry {
	if artifact == nil {
		return nil
	}
	return artifact.AgentHistory
}

// agentEntryKey identifies one agent session for deduplication. Re-submitted
// commits and retried session-stop hooks carry the same (agent, start, end)
// triple and collapse into a single entry.
func agentEntryKey(entry AgentEntry) string {
	return strings.Join([]string{
		entry.AgentID,
		entry.StartedAt.UTC().Format(time.RFC3339Nano),
		entry.EndedAt.UTC().Format(time.RFC3339Nano),
	}, "|")
}

// mergeAgentHistory folds incoming session entries onto the previously
// committed history, dropping duplicate sessions and duplicate edited paths
// while preserving order, and caps the result to the newest entries. The
// boolean reports whether incoming contributed at least one new entry.
func mergeAgentHistory(previous, incoming []AgentEntry) ([]AgentEntry, bool) {
	seen := make(map[string]struct{}, len(previous)+len(incoming))
	merged := make([]AgentEntry, 0, len(previous)+len(incoming))
	consume := func(entries []AgentEntry) {
		for _, entry := range entries {
			key := agentEntryKey(entry)
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			entry.FilesEdited = dedupeStringsPreserveOrder(entry.FilesEdited)
			merged = append(merged, entry)
		}
	}
	consume(previous)
	before := len(merged)
	consume(incoming)
	added := len(merged) > before
	if len(merged) > maxAgentHistoryEntries {
		merged = merged[len(merged)-maxAgentHistoryEntries:]
	}
	return merged, added
}

func dedupeStringsPreserveOrder(items []string) []string {
	if len(items) == 0 {
		return items
	}
	seen := make(map[string]struct{}, len(items))
	out := make([]string, 0, len(items))
	for _, item := range items {
		if _, ok := seen[item]; ok {
			continue
		}
		seen[item] = struct{}{}
		out = append(out, item)
	}
	return out
}

// sameGenerationContent decides whether a candidate reproduces an already
// committed generation. It compares identity fields and content hashes rather
// than the full struct so bookkeeping fields (metrics, generation, timestamps)
// cannot force a pointless rewrite.
func sameGenerationContent(next, previous *Artifact) bool {
	return next.Root == previous.Root &&
		next.Branch == previous.Branch &&
		next.BaseRef == previous.BaseRef &&
		next.PrefixHash == previous.PrefixHash &&
		next.DeltaHash == previous.DeltaHash &&
		next.CombinedHash == previous.CombinedHash
}

// metricsLine is one handoff.metrics.log record. Generation lets crash
// recovery discard entries for generations that never committed.
type metricsLine struct {
	Generation   int64        `json:"generation"`
	GeneratedAt  string       `json:"generated_at"`
	Branch       string       `json:"branch"`
	BaseRef      string       `json:"base_ref"`
	PrefixHash   string       `json:"prefix_hash"`
	DeltaHash    string       `json:"delta_hash"`
	CombinedHash string       `json:"combined_hash"`
	Metrics      CacheMetrics `json:"metrics"`
}

func marshalMetricsLine(artifact *Artifact) ([]byte, error) {
	line := metricsLine{
		Generation:   artifact.Generation,
		GeneratedAt:  artifact.GeneratedAt.Format("2006-01-02T15:04:05Z07:00"),
		Branch:       artifact.Branch,
		BaseRef:      artifact.BaseRef,
		PrefixHash:   artifact.PrefixHash,
		DeltaHash:    artifact.DeltaHash,
		CombinedHash: artifact.CombinedHash,
		Metrics:      artifact.Metrics,
	}
	data, err := json.Marshal(line)
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// mergeMetricsLog rewrites the metrics log under the commit lock: it preserves
// existing lines, drops orphan entries stamped with a generation at or beyond
// the one being committed (debris of an interrupted commit), appends the new
// entry and enforces the line cap. The whole log is replaced atomically, so
// concurrent processes can no longer interleave appends.
func mergeMetricsLog(runtimeDir string, newLine []byte, generation int64) ([]byte, error) {
	path := filepath.Join(runtimeDir, metricsFilename)
	existing, err := runtimefile.Read(path)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}

	kept := make([][]byte, 0, maxMetricsLines)
	for _, line := range bytes.Split(existing, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		if generation > 0 {
			var parsed metricsLine
			if json.Unmarshal(line, &parsed) == nil && parsed.Generation >= generation {
				continue
			}
		}
		kept = append(kept, line)
	}
	kept = append(kept, bytes.TrimRight(newLine, "\n"))
	if len(kept) > maxMetricsLines {
		kept = kept[len(kept)-maxMetricsLines:]
	}

	var buf bytes.Buffer
	for _, line := range kept {
		buf.Write(line)
		buf.WriteByte('\n')
	}
	return buf.Bytes(), nil
}
