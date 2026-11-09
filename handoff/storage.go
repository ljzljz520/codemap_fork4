package handoff

import (
	"bytes"
	"os"
	"path/filepath"

	"codemap/internal/projectpath"
	"codemap/internal/runtimefile"
)

const (
	latestFilename  = "handoff.latest.json"
	prefixFilename  = "handoff.prefix.json"
	deltaFilename   = "handoff.delta.json"
	metricsFilename = "handoff.metrics.log"
	maxMetricsLines = 500
)

// LatestPath returns the absolute location of the latest handoff artifact.
func LatestPath(root string) string {
	runtimeRoot := projectpath.ProjectRuntimeDir(root)
	absRoot, err := filepath.Abs(runtimeRoot)
	if err != nil {
		return filepath.Join(runtimeRoot, latestFilename)
	}
	return filepath.Join(absRoot, latestFilename)
}

// PrefixPath returns the absolute location of the prefix snapshot.
func PrefixPath(root string) string {
	runtimeRoot := projectpath.ProjectRuntimeDir(root)
	absRoot, err := filepath.Abs(runtimeRoot)
	if err != nil {
		return filepath.Join(runtimeRoot, prefixFilename)
	}
	return filepath.Join(absRoot, prefixFilename)
}

// DeltaPath returns the absolute location of the delta snapshot.
func DeltaPath(root string) string {
	runtimeRoot := projectpath.ProjectRuntimeDir(root)
	absRoot, err := filepath.Abs(runtimeRoot)
	if err != nil {
		return filepath.Join(runtimeRoot, deltaFilename)
	}
	return filepath.Join(absRoot, deltaFilename)
}

// MetricsPath returns the absolute location of the handoff metrics log.
func MetricsPath(root string) string {
	runtimeRoot := projectpath.ProjectRuntimeDir(root)
	absRoot, err := filepath.Abs(runtimeRoot)
	if err != nil {
		return filepath.Join(runtimeRoot, metricsFilename)
	}
	return filepath.Join(absRoot, metricsFilename)
}

// ReadLatest reads the latest committed handoff artifact if it exists.
// Because Commit renames handoff.latest.json as its single commit point and
// the artifact embeds every layer, a successful read is always one complete
// generation, never a mixture of generations. Returns (nil, nil) when no
// artifact is present.
func ReadLatest(root string) (*Artifact, error) {
	runtimeDir, err := projectpath.CheckedRuntimeCodemapDir(root)
	if err != nil {
		return nil, err
	}
	return readCommittedAt(runtimeDir)
}

// WriteLatest persists an artifact through Commit, the single atomic
// generation entry point shared by the CLI, MCP and session-stop paths. It is
// retained as a compatibility wrapper; new callers should use Commit directly.
func WriteLatest(root string, artifact *Artifact) error {
	_, err := Commit(root, artifact)
	return err
}

func runtimePath(root, name string) (string, error) {
	dir, err := projectpath.CheckedRuntimeCodemapDir(root)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, name), nil
}

// appendMetrics adds one metrics entry outside of a generation commit. The
// log is read and rewritten atomically so concurrent appenders cannot
// interleave or tear lines. Commit performs the equivalent rewrite while
// holding the commit lock; this helper exists for standalone metrics tooling
// and tests.
func appendMetrics(root string, artifact *Artifact) error {
	path, err := runtimePath(root, metricsFilename)
	if err != nil {
		return err
	}
	return appendMetricsAt(path, artifact)
}

func appendMetricsAt(path string, artifact *Artifact) error {
	line, err := marshalMetricsLine(artifact)
	if err != nil {
		return err
	}
	existing, err := runtimefile.Read(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}

	lines := make([][]byte, 0, maxMetricsLines)
	for _, raw := range bytes.Split(bytes.TrimSpace(existing), []byte("\n")) {
		if len(raw) > 0 {
			lines = append(lines, raw)
		}
	}
	lines = append(lines, bytes.TrimRight(line, "\n"))
	if len(lines) > maxMetricsLines {
		lines = lines[len(lines)-maxMetricsLines:]
	}

	var buf bytes.Buffer
	for _, raw := range lines {
		buf.Write(raw)
		buf.WriteByte('\n')
	}
	return runtimefile.WriteAtomic(path, buf.Bytes(), 0o644)
}

func normalizeArtifact(artifact *Artifact) {
	ensureSchemaVersion(artifact)
	promoteLegacyFieldsIntoDelta(artifact)
	ensureNonNilSnapshotFields(artifact)
	mirrorDeltaToLegacy(artifact)
	backfillHashes(artifact)
}

func ensureSchemaVersion(artifact *Artifact) {
	if artifact.SchemaVersion == 0 {
		artifact.SchemaVersion = SchemaVersion
	}
}

func promoteLegacyFieldsIntoDelta(artifact *Artifact) {
	if artifact.Delta.Changed == nil && len(artifact.ChangedFiles) > 0 {
		artifact.Delta.Changed = make([]FileStub, 0, len(artifact.ChangedFiles))
		for _, path := range artifact.ChangedFiles {
			artifact.Delta.Changed = append(artifact.Delta.Changed, FileStub{Path: path})
		}
	}
	if artifact.Delta.RiskFiles == nil {
		artifact.Delta.RiskFiles = append([]RiskFile{}, artifact.RiskFiles...)
	}
	if artifact.Delta.RecentEvents == nil {
		artifact.Delta.RecentEvents = append([]EventSummary{}, artifact.RecentEvents...)
	}
	if artifact.Delta.NextSteps == nil {
		artifact.Delta.NextSteps = append([]string{}, artifact.NextSteps...)
	}
	if artifact.Delta.OpenQuestions == nil {
		artifact.Delta.OpenQuestions = append([]string{}, artifact.OpenQuestions...)
	}
}

func ensureNonNilSnapshotFields(artifact *Artifact) {
	artifact.Prefix.Hubs = nonNilHubs(artifact.Prefix.Hubs)
	artifact.Delta.Changed = nonNilStubs(artifact.Delta.Changed)
	artifact.Delta.RiskFiles = nonNilRiskFiles(artifact.Delta.RiskFiles)
	artifact.Delta.RecentEvents = nonNilEvents(artifact.Delta.RecentEvents)
	artifact.Delta.NextSteps = nonNilStrings(artifact.Delta.NextSteps)
	artifact.Delta.OpenQuestions = nonNilStrings(artifact.Delta.OpenQuestions)
}

func mirrorDeltaToLegacy(artifact *Artifact) {
	if artifact.ChangedFiles == nil {
		artifact.ChangedFiles = stubPaths(artifact.Delta.Changed)
	}
	if artifact.RiskFiles == nil {
		artifact.RiskFiles = append([]RiskFile{}, artifact.Delta.RiskFiles...)
	}
	if artifact.RecentEvents == nil {
		artifact.RecentEvents = append([]EventSummary{}, artifact.Delta.RecentEvents...)
	}
	if artifact.NextSteps == nil {
		artifact.NextSteps = append([]string{}, artifact.Delta.NextSteps...)
	}
	if artifact.OpenQuestions == nil {
		artifact.OpenQuestions = append([]string{}, artifact.OpenQuestions...)
	}
}

func backfillHashes(artifact *Artifact) {
	if artifact.PrefixHash == "" {
		if hash, _, err := hashCanonical(artifact.Prefix); err == nil {
			artifact.PrefixHash = hash
		}
	}
	if artifact.DeltaHash == "" {
		if hash, _, err := hashCanonical(artifact.Delta); err == nil {
			artifact.DeltaHash = hash
		}
	}
	if artifact.CombinedHash == "" {
		artifact.CombinedHash = hashFromStrings(artifact.PrefixHash, artifact.DeltaHash)
	}
}
