package agentrun

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const checkpointDigestStableJSON = "stable_json_sha256"

func checkpointRelativeRef(turnID string) (string, error) {
	if err := validateRunID(turnID); err != nil {
		return "", fmt.Errorf("checkpoint_ref_invalid: %w", err)
	}
	return filepath.ToSlash(filepath.Join("context", "checkpoints", turnID+".json")), nil
}

func digestContextCheckpoint(checkpoint ContextCheckpoint) string {
	encoded, _ := json.Marshal(checkpoint)
	return digestBytes(encoded)
}

func resolveCheckpointPath(sessionDir, turnID, reference string) (string, string) {
	canonical, err := checkpointRelativeRef(turnID)
	if err != nil {
		return "", "checkpoint_ref_invalid"
	}
	if strings.TrimSpace(reference) == "" {
		return "", "checkpoint_missing"
	}
	referencePath := filepath.FromSlash(reference)
	candidate := referencePath
	if !filepath.IsAbs(candidate) {
		if filepath.ToSlash(filepath.Clean(referencePath)) != canonical ||
			filepath.ToSlash(referencePath) != canonical {
			return "", "checkpoint_ref_invalid"
		}
		candidate = filepath.Join(sessionDir, referencePath)
	}
	rootAbs, err := filepath.Abs(sessionDir)
	if err != nil {
		return "", "checkpoint_ref_invalid"
	}
	candidateAbs, err := filepath.Abs(candidate)
	if err != nil {
		return "", "checkpoint_ref_invalid"
	}
	relative, err := filepath.Rel(rootAbs, candidateAbs)
	if err != nil || filepath.ToSlash(relative) != canonical {
		return "", "checkpoint_ref_outside_session"
	}
	if _, err := os.Stat(candidateAbs); err != nil {
		if os.IsNotExist(err) {
			return "", "checkpoint_missing"
		}
		return "", "checkpoint_ref_invalid"
	}
	resolvedRoot, rootErr := filepath.EvalSymlinks(rootAbs)
	resolvedCandidate, candidateErr := filepath.EvalSymlinks(candidateAbs)
	if rootErr != nil || candidateErr != nil {
		return "", "checkpoint_ref_invalid"
	}
	resolvedRelative, err := filepath.Rel(resolvedRoot, resolvedCandidate)
	if err != nil || filepath.ToSlash(resolvedRelative) != canonical {
		return "", "checkpoint_ref_outside_session"
	}
	return candidateAbs, ""
}

func normalizeExportedCheckpoint(
	sessionDir string,
	manifest ContextManifest,
) (ContextManifest, *ContextCheckpoint) {
	path, reason := resolveCheckpointPath(sessionDir, manifest.TurnID, manifest.SummaryRef)
	if reason != "" {
		return clearInvalidCheckpoint(manifest, reason), nil
	}
	var checkpoint ContextCheckpoint
	if err := readJSON(path, &checkpoint); err != nil {
		return clearInvalidCheckpoint(manifest, "checkpoint_missing"), nil
	}
	if checkpoint.SessionID != manifest.SessionID || checkpoint.TurnID != manifest.TurnID {
		return clearInvalidCheckpoint(manifest, "checkpoint_identity_mismatch"), nil
	}
	stableDigest := digestContextCheckpoint(checkpoint)
	valid := manifest.SummaryDigest == stableDigest
	if manifest.SummaryDigestKind == "" && !valid {
		if fileDigest, err := digestFile(path); err == nil {
			valid = manifest.SummaryDigest == fileDigest
		}
	}
	if !valid {
		return clearInvalidCheckpoint(manifest, "checkpoint_digest_mismatch"), nil
	}
	canonical, _ := checkpointRelativeRef(manifest.TurnID)
	if manifest.SummaryDigestKind == "" && manifest.SummaryDigest != stableDigest {
		manifest.LegacySummaryDigest = manifest.SummaryDigest
	}
	manifest.SummaryRef = canonical
	manifest.SummaryDigest = stableDigest
	manifest.SummaryDigestKind = checkpointDigestStableJSON
	manifest.CheckpointError = ""
	manifest.Compacted = true
	return manifest, &checkpoint
}

func normalizeImportedCheckpoint(
	sessionID, turnID string,
	manifest ContextManifest,
	checkpoint ContextCheckpoint,
) (ContextManifest, *ContextCheckpoint) {
	if manifest.SessionID != "" && manifest.SessionID != sessionID ||
		manifest.TurnID != "" && manifest.TurnID != turnID {
		return clearInvalidCheckpoint(manifest, "checkpoint_identity_mismatch"), nil
	}
	if checkpoint.SessionID != sessionID || checkpoint.TurnID != turnID {
		return clearInvalidCheckpoint(manifest, "checkpoint_identity_mismatch"), nil
	}
	stableDigest := digestContextCheckpoint(checkpoint)
	if manifest.SummaryDigest == "" || manifest.SummaryDigest != stableDigest {
		return clearInvalidCheckpoint(manifest, "checkpoint_digest_mismatch"), nil
	}
	canonical, err := checkpointRelativeRef(turnID)
	if err != nil {
		return clearInvalidCheckpoint(manifest, "checkpoint_ref_invalid"), nil
	}
	manifest.SessionID, manifest.TurnID = sessionID, turnID
	manifest.SummaryRef = canonical
	manifest.SummaryDigest = stableDigest
	manifest.SummaryDigestKind = checkpointDigestStableJSON
	manifest.LegacySummaryDigest = ""
	manifest.CheckpointError = ""
	manifest.Compacted = true
	return manifest, &checkpoint
}

func clearInvalidCheckpoint(manifest ContextManifest, reason string) ContextManifest {
	manifest.SummaryRef = ""
	manifest.SummaryDigest = ""
	manifest.SummaryDigestKind = ""
	manifest.LegacySummaryDigest = ""
	manifest.SummaryRange = SequenceRange{}
	manifest.Compacted = false
	manifest.CheckpointError = reason
	return manifest
}

func writeCheckpointCurrent(sessionDir string, checkpoint ContextCheckpoint, manifest ContextManifest) error {
	current := map[string]any{
		"schema_version": SessionSchemaVersion, "session_id": checkpoint.SessionID,
		"turn_id": checkpoint.TurnID, "checkpoint_ref": manifest.SummaryRef,
		"checkpoint_digest": manifest.SummaryDigest, "checkpoint_digest_kind": manifest.SummaryDigestKind,
		"covered_message_range": checkpoint.CoveredMessageRange, "updated_at": checkpoint.CreatedAt,
	}
	return writeJSONAtomic(filepath.Join(sessionDir, "context", "current.json"), current)
}
