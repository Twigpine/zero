package sessions

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// RestoreReport summarizes a workspace restore.
type RestoreReport struct {
	TargetSequence int      `json:"targetSequence"`
	FilesRestored  int      `json:"filesRestored"`
	FilesDeleted   int      `json:"filesDeleted"`
	Skipped        []string `json:"skipped,omitempty"` // paths whose before-state was not recoverable
}

// RewindMarker is the payload of the EventSessionRewind event appended after a rewind.
type RewindMarker struct {
	TargetSequence int           `json:"targetSequence"`
	Report         RestoreReport `json:"report"`
}

// RestoreToSequence reverts workspace files to their state at targetSeq by applying
// the before-snapshots of every checkpoint after the target, newest-first (so the
// snapshot closest to the target wins). It does not modify the event log.
func (store *Store) RestoreToSequence(sessionID, workspaceRoot string, targetSeq int) (RestoreReport, error) {
	report := RestoreReport{TargetSequence: targetSeq}
	if !ValidSessionID(sessionID) {
		return report, fmt.Errorf("invalid zero session id %q", sessionID)
	}
	unlock, err := store.lockSession(sessionID)
	if err != nil {
		return report, err
	}
	defer unlock()
	return store.restoreToSequenceLocked(sessionID, workspaceRoot, targetSeq)
}

// restoreToSequenceLocked is the body of RestoreToSequence WITHOUT acquiring the
// session lock. The caller MUST already hold store.lockSession(sessionID). It
// lets ApplyRewind run restore/truncate/prune/marker atomically under one lock.
func (store *Store) restoreToSequenceLocked(sessionID, workspaceRoot string, targetSeq int) (RestoreReport, error) {
	report := RestoreReport{TargetSequence: targetSeq}
	metadata, err := store.readMetadata(sessionID)
	if err != nil {
		return report, err
	}
	imported := IsImportedSession(metadata)
	checkpoints, err := store.sortedCheckpointsAfter(sessionID, targetSeq)
	if err != nil {
		return report, err
	}
	// sortedCheckpointsAfter returns newest-first; iterate oldest-first
	// (closest-to-target first) so the per-path short-circuit below keeps the
	// snapshot closest to the target and ignores all newer ones.
	restored := map[string]bool{}
	for i := len(checkpoints) - 1; i >= 0; i-- {
		ev := checkpoints[i]
		var payload CheckpointPayload
		if err := json.Unmarshal(ev.Payload, &payload); err != nil {
			// Fail the rewind rather than skipping: silently continuing would let a
			// NEWER checkpoint win for that path and restore a later state than the
			// caller asked for. Corruption is a hard error.
			return report, fmt.Errorf("decode checkpoint payload seq %d: %w", ev.Sequence, err)
		}
		checkpointRoot, err := store.trustedCheckpointRoot(payload, workspaceRoot, imported)
		if err != nil {
			return report, fmt.Errorf("checkpoint seq %d: %w", ev.Sequence, err)
		}
		for _, f := range payload.Files {
			// Resolve/confine the target FIRST so the dedupe key below is the
			// canonical workspace path. Defense in depth: never write/delete outside
			// the workspace, even if a checkpoint event was tampered with (path
			// traversal via "../") or an in-workspace symlink points outside the
			// root. The boundary is re-resolved here, immediately before the
			// mutation, to keep the check-to-use window small.
			//
			// Residual: a symlink-swap TOCTOU remains — a concurrent process with
			// workspace write access could replace a validated intermediate
			// directory with a symlink between this check and the
			// os.Remove/writeFileAtomic below. Fully closing it needs
			// descriptor-based traversal (openat2 RESOLVE_BENEATH on Linux /
			// per-component O_NOFOLLOW), which is platform-specific; tracked for
			// the CLI/TUI rewind-wiring work. The narrow window plus the
			// workspace-write-access precondition make this low-risk here.
			abs, ok := resolveWithinWorkspace(checkpointRoot, f.Path)

			// Process only the CLOSEST-to-target entry per RESOLVED path. We iterate
			// closest-to-target first, so the first time we see a resolved path is
			// its closest-to-target snapshot; any newer (already-handled) entry for
			// the SAME underlying file must be ignored — even when it was recorded
			// under an equivalent-but-different raw path (./a.txt, dir/../a.txt, a
			// symlink alias). Deduping on the raw path would let such an alias slip
			// past and overwrite the closest snapshot. Unresolvable targets dedupe
			// on their raw path (there is no canonical key for them).
			key := f.Path
			if ok {
				key = abs
			}
			if restored[key] {
				continue
			}
			restored[key] = true

			if !ok {
				report.Skipped = append(report.Skipped, f.Path)
				continue
			}
			switch {
			case f.Skipped:
				report.Skipped = append(report.Skipped, f.Path)
			case f.Absent:
				if err := os.Remove(abs); err == nil || os.IsNotExist(err) {
					report.FilesDeleted++
				} else {
					report.Skipped = append(report.Skipped, f.Path)
				}
			case f.Blob != "":
				content, rerr := store.readBlob(sessionID, f.Blob)
				if rerr != nil {
					report.Skipped = append(report.Skipped, f.Path)
					continue
				}
				if err := store.writeFileAtomic(abs, content, f.Mode); err != nil {
					report.Skipped = append(report.Skipped, f.Path)
					continue
				}
				report.FilesRestored++
			}
		}
	}
	return report, nil
}

func (store *Store) trustedCheckpointRoot(payload CheckpointPayload, requestedRoot string, imported bool) (string, error) {
	boundRoot := strings.TrimSpace(payload.WorkspaceRoot)
	if len(payload.WorkspaceBinding) > 0 {
		verified, err := store.checkpointRootCrypter().Open(payload.WorkspaceBinding)
		if err != nil {
			return "", fmt.Errorf("verify local workspace binding: %w", err)
		}
		verifiedRoot := strings.TrimSpace(string(verified))
		if verifiedRoot == "" || boundRoot == "" || !sessionWorkspacePathEqual(verifiedRoot, boundRoot) {
			return "", fmt.Errorf("workspace root does not match its verified local binding")
		}
		return verifiedRoot, nil
	}
	if imported {
		return "", fmt.Errorf("has no verified local workspace binding; refusing to rewind imported session")
	}

	// Native checkpoints written before WorkspaceBinding existed remain usable,
	// but their event-supplied root gets no authority. It must either be absent or
	// agree with the caller's trusted workspace, and restore stays under the
	// caller-supplied root.
	requestedRoot = strings.TrimSpace(requestedRoot)
	if requestedRoot == "" {
		return "", fmt.Errorf("has no trusted workspace root")
	}
	if boundRoot != "" && !sessionWorkspacePathEqual(boundRoot, requestedRoot) {
		return "", fmt.Errorf("legacy workspace root does not match the requested workspace")
	}
	return requestedRoot, nil
}

// resolveWithinWorkspace joins rel to root and confirms the result stays inside
// root. It rejects lexical traversal ("../") and absolute escapes, AND resolves
// symlinks (like tools.resolveWorkspaceTargetPath): it EvalSymlinks the deepest
// existing ancestor, re-joins the missing segments, and verifies the result is
// under EvalSymlinks(root). This blocks an in-workspace symlink that points
// outside the workspace from redirecting a restore write/delete outside it.
func resolveWithinWorkspace(root, rel string) (string, bool) {
	cleanRoot, err := filepath.EvalSymlinks(filepath.Clean(root))
	if err != nil {
		return "", false
	}

	abs := filepath.Join(cleanRoot, rel)

	// Walk down from the target to the deepest ancestor that exists on disk,
	// collecting the not-yet-created trailing segments.
	existing := abs
	missingSegments := []string{}
	for {
		if _, err := os.Lstat(existing); err == nil {
			break
		} else if os.IsNotExist(err) {
			parent := filepath.Dir(existing)
			if parent == existing {
				return "", false
			}
			missingSegments = append([]string{filepath.Base(existing)}, missingSegments...)
			existing = parent
			continue
		} else {
			return "", false
		}
	}

	resolved, err := filepath.EvalSymlinks(existing)
	if err != nil {
		return "", false
	}
	for _, segment := range missingSegments {
		resolved = filepath.Join(resolved, segment)
	}

	within, err := filepath.Rel(cleanRoot, resolved)
	if err != nil {
		return "", false
	}
	if within == ".." || strings.HasPrefix(within, ".."+string(filepath.Separator)) || filepath.IsAbs(within) {
		return "", false
	}
	return resolved, true
}

// TruncateEvents atomically rewrites events.jsonl keeping only events with
// Sequence <= keepThroughSequence, and updates metadata EventCount.
func (store *Store) TruncateEvents(sessionID string, keepThroughSequence int) error {
	if !ValidSessionID(sessionID) {
		return fmt.Errorf("invalid zero session id %q", sessionID)
	}
	unlock, err := store.lockSession(sessionID)
	if err != nil {
		return err
	}
	defer unlock()
	return store.truncateEventsLocked(sessionID, keepThroughSequence)
}

// truncateEventsLocked is the body of TruncateEvents WITHOUT acquiring the
// session lock. The caller MUST already hold store.lockSession(sessionID).
func (store *Store) truncateEventsLocked(sessionID string, keepThroughSequence int) error {
	events, err := store.ReadEvents(sessionID)
	if err != nil {
		return err
	}
	var kept [][]byte
	keptCount := 0
	for _, ev := range events {
		if ev.Sequence > keepThroughSequence {
			continue
		}
		data, err := json.Marshal(ev)
		if err != nil {
			return fmt.Errorf("encode kept event: %w", err)
		}
		kept = append(kept, data)
		keptCount++
	}
	var encoded []byte
	if len(kept) > 0 {
		encoded = append(bytes.Join(kept, []byte{'\n'}), '\n')
	}
	path := store.eventsPath(sessionID)
	if err := store.writeFileAtomicSync(path, encoded, 0o600); err != nil {
		return fmt.Errorf("write truncated events: %w", err)
	}
	session, err := store.readMetadata(sessionID)
	if err != nil {
		return err
	}
	session.EventCount = keptCount
	session.UpdatedAt = store.timestamp()
	return store.writeMetadata(session)
}

// ApplyRewind performs a full safe rewind: restore workspace files to targetSeq,
// truncate the event log, prune now-orphaned blobs, and append an EventSessionRewind
// marker. Returns the restore report.
func (store *Store) ApplyRewind(sessionID, workspaceRoot string, targetSeq int) (RestoreReport, error) {
	if !ValidSessionID(sessionID) {
		return RestoreReport{TargetSequence: targetSeq}, fmt.Errorf("invalid zero session id %q", sessionID)
	}
	// Hold the session lock ONCE across restore + truncate + prune + marker so a
	// concurrent writer cannot interleave between the sub-steps. The sub-steps
	// use *Locked variants that assume the lock is already held; re-locking here
	// would deadlock the non-reentrant in-process mutex.
	unlock, err := store.lockSession(sessionID)
	if err != nil {
		return RestoreReport{TargetSequence: targetSeq}, err
	}
	defer unlock()

	report, err := store.restoreToSequenceLocked(sessionID, workspaceRoot, targetSeq)
	if err != nil {
		return report, err
	}
	if err := store.truncateEventsLocked(sessionID, targetSeq); err != nil {
		return report, err
	}
	_, _ = store.pruneOrphanBlobsLocked(sessionID)
	if _, err := store.appendEventLocked(sessionID, AppendEventInput{
		Type:    EventSessionRewind,
		Payload: RewindMarker{TargetSequence: targetSeq, Report: report},
	}); err != nil {
		return report, err
	}
	return report, nil
}

// writeFileAtomic writes content to path via a temp file + rename. mode is the
// checkpoint-recorded permission bits to restore; when it is 0 (mode unknown —
// e.g. an old checkpoint without the field) the original mode of the file being
// overwritten is preserved, falling back to 0o644 for a newly created file.
func (store *Store) writeFileAtomic(path string, content []byte, mode uint32) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	perm := os.FileMode(0o644)
	if mode != 0 {
		perm = os.FileMode(mode).Perm()
	} else if info, err := os.Stat(path); err == nil {
		// Mode not captured: preserve the existing file's permission bits.
		perm = info.Mode().Perm()
	}
	tmp := fmt.Sprintf("%s.zero-restore-tmp-%d", path, store.idCounter.Add(1))
	// fsync the temp so a restored file's bytes are durable, not just in the page
	// cache, before it is renamed into place.
	if err := writeFileSync(tmp, content, perm); err != nil {
		return err
	}
	// writeFileSync applies perm only on creation and is subject to umask; force
	// the exact bits so an executable script's mode is faithfully restored.
	if err := os.Chmod(tmp, perm); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	// fsync the parent dir so the rename itself survives a crash.
	return syncDir(filepath.Dir(path))
}
