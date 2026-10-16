package sessions

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"

	"github.com/Gitlawb/zero/internal/sandbox"
)

// CheckpointsDir is the per-session subdirectory holding content-addressed blobs.
const CheckpointsDir = "checkpoints"

const defaultMaxCheckpointBytes = 5 << 20 // 5 MiB

// CheckpointFile records the before-mutation state of one workspace file.
type CheckpointFile struct {
	Path    string `json:"path"`
	Blob    string `json:"blob,omitempty"`    // sha256 of prior content, "" if absent/skipped
	Absent  bool   `json:"absent,omitempty"`  // file did not exist before (restore -> delete)
	Skipped bool   `json:"skipped,omitempty"` // exceeded size cap; not recoverable
	Bytes   int    `json:"bytes,omitempty"`
	Mode    uint32 `json:"mode,omitempty"` // unix permission bits of the prior file; 0 if unknown (restore preserves existing mode)
}

// CheckpointPayload is the payload of an EventSessionCheckpoint event. It indexes
// the before-state blobs captured for one mutating tool call.
type CheckpointPayload struct {
	Tool  string           `json:"tool"`
	Files []CheckpointFile `json:"files"`
}

// CheckpointsEnabled reports whether checkpoint capture is enabled (default on;
// disabled with ZERO_CHECKPOINTS=off).
func CheckpointsEnabled() bool {
	return os.Getenv("ZERO_CHECKPOINTS") != "off"
}

func maxCheckpointBytes() int {
	if raw := os.Getenv("ZERO_CHECKPOINT_MAX_BYTES"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			return n
		}
	}
	return defaultMaxCheckpointBytes
}

func (store *Store) blobsDir(sessionID string) string {
	return filepath.Join(store.sessionPath(sessionID), CheckpointsDir, "blobs")
}

func (store *Store) blobPath(sessionID, hash string) string {
	return filepath.Join(store.blobsDir(sessionID), hash)
}

// CaptureToolCheckpoint snapshots the current (before-mutation) content of each
// path and records an EventSessionCheckpoint indexing the blobs. Capture is
// best-effort: an unreadable file is recorded as skipped rather than failing the
// caller. Returns the appended event (or a zero Event if there was nothing to do).
func (store *Store) CaptureToolCheckpoint(sessionID, workspaceRoot, tool string, paths []string) (Event, error) {
	if !ValidSessionID(sessionID) {
		return Event{}, fmt.Errorf("invalid zero session id %q", sessionID)
	}
	if !CheckpointsEnabled() || len(paths) == 0 {
		return Event{}, nil
	}
	// Hold the session lock across writing the blobs AND appending the
	// referencing event so a concurrent pruneOrphanBlobs cannot delete a blob in
	// the gap between writeBlob and the event that references it.
	unlock, err := store.lockSession(sessionID)
	if err != nil {
		return Event{}, err
	}
	defer unlock()

	payload, ok := store.SnapshotForCheckpoint(sessionID, workspaceRoot, tool, paths)
	if !ok {
		return Event{}, nil
	}
	return store.appendEventLocked(sessionID, AppendEventInput{Type: EventSessionCheckpoint, Payload: payload})
}

// SnapshotForCheckpoint reads and stores the before-mutation blobs for paths and
// returns the checkpoint payload WITHOUT appending an event.
//
// CONTRACT: the returned payload references blobs that are ORPHAN-VULNERABLE — a
// concurrent pruneOrphanBlobs/ApplyRewind can delete them — until the caller
// appends an EventSessionCheckpoint carrying this payload. The caller therefore
// MUST record that event promptly, including on cancellation paths. Prefer
// CaptureToolCheckpoint, which writes the blobs AND appends the event atomically
// under one session lock; use SnapshotForCheckpoint only when the event must be
// batched IN ORDER with other session events (the TUI captures before each
// mutating tool, batches the checkpoint with the run's other events to preserve
// recorded ordering, and flushes them at end-of-run and on cancel).
//
// Returns ok=false when there is nothing to record (disabled, no paths, or no
// capturable files).
func (store *Store) SnapshotForCheckpoint(sessionID, workspaceRoot, tool string, paths []string) (CheckpointPayload, bool) {
	// Validate the session id (as CaptureToolCheckpoint does) so an exported caller
	// can't route blob writes through an unexpected/invalid session path.
	if !ValidSessionID(sessionID) {
		return CheckpointPayload{}, false
	}
	if !CheckpointsEnabled() || len(paths) == 0 {
		return CheckpointPayload{}, false
	}
	root, err := os.OpenRoot(workspaceRoot)
	if err != nil {
		return CheckpointPayload{}, false
	}
	defer root.Close()
	capBytes := int64(maxCheckpointBytes())
	exclusions := sandbox.ProtectedCredentialExclusions(workspaceRoot)
	files := make([]CheckpointFile, 0, len(paths))
	for _, rel := range paths {
		entry := CheckpointFile{Path: rel}
		// Confine every capture target to the workspace using the SAME guard the
		// restore path uses (EvalSymlinks-resolved, no "../" escape). A target that
		// does not resolve inside the workspace is Skipped — never read into a blob,
		// and never recorded as Absent (which would delete it on rewind).
		abs, ok := resolveWithinWorkspace(workspaceRoot, rel)
		if !ok {
			entry.Skipped = true
			files = append(files, entry)
			continue
		}
		file, openErr := root.Open(rel)
		if openErr != nil {
			if os.IsNotExist(openErr) {
				// Genuinely new file — restore deletes it.
				entry.Absent = true
			} else {
				// Permission / IO / symlink-loop errors must NOT be rewound as a
				// delete; record them as skipped so restore leaves the file alone.
				entry.Skipped = true
			}
			files = append(files, entry)
			continue
		}
		info, statErr := file.Stat()
		if statErr != nil || exclusions.FileHandleExcluded(abs, file, info) {
			file.Close()
			entry.Skipped = true
			files = append(files, entry)
			continue
		}
		if info.IsDir() {
			file.Close()
			continue
		}
		// Compare sizes as int64 so a file larger than the cap is never silently
		// truncated past the cap via an int overflow on a 32-bit platform.
		if info.Size() > capBytes {
			file.Close()
			entry.Skipped = true
			if info.Size() <= int64(^uint(0)>>1) {
				entry.Bytes = int(info.Size())
			}
			files = append(files, entry)
			continue
		}
		// Record the prior permission bits so a restore can put them back (an
		// executable script must not return as 0o644).
		entry.Mode = uint32(info.Mode().Perm())
		content, readErr := io.ReadAll(file)
		closeErr := file.Close()
		if readErr != nil || closeErr != nil {
			entry.Skipped = true
			files = append(files, entry)
			continue
		}
		hash, writeErr := store.writeBlob(sessionID, content)
		if writeErr != nil {
			entry.Skipped = true
			files = append(files, entry)
			continue
		}
		entry.Blob = hash
		entry.Bytes = len(content)
		files = append(files, entry)
	}
	if len(files) == 0 {
		return CheckpointPayload{}, false
	}
	return CheckpointPayload{Tool: tool, Files: files}, true
}

// writeBlob stores content under its sha256 (content-addressed, deduplicated) and
// returns the hex hash. An existing blob with the same hash is left untouched.
func (store *Store) writeBlob(sessionID string, content []byte) (string, error) {
	sum := sha256.Sum256(content)
	hash := hex.EncodeToString(sum[:])
	dir := store.blobsDir(sessionID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create checkpoint blob dir: %w", err)
	}
	path := store.blobPath(sessionID, hash)
	if _, err := os.Stat(path); err == nil {
		return hash, nil // dedup: identical content already stored
	}
	if err := store.writeFileAtomicSync(path, content, 0o600); err != nil {
		return "", fmt.Errorf("write checkpoint blob: %w", err)
	}
	return hash, nil
}

// copyBlobs copies every checkpoint blob from src into dst's blobs dir. It is
// used by Fork so a forked session carries the parent's content-addressed blobs
// and a rewind on the fork can restore file content. Blobs are content-addressed
// (the filename is the sha256), so an already-present blob is left untouched.
// A missing source blobs dir is not an error (the session had no checkpoints).
func (store *Store) copyBlobs(srcSessionID, dstSessionID string) error {
	srcDir := store.blobsDir(srcSessionID)
	entries, err := os.ReadDir(srcDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read checkpoint blobs: %w", err)
	}
	dstDir := store.blobsDir(dstSessionID)
	madeDir := false
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		dstPath := store.blobPath(dstSessionID, entry.Name())
		if _, err := os.Stat(dstPath); err == nil {
			continue // content-addressed: identical blob already present
		}
		content, err := os.ReadFile(store.blobPath(srcSessionID, entry.Name()))
		if err != nil {
			return fmt.Errorf("read checkpoint blob %s: %w", entry.Name(), err)
		}
		if !madeDir {
			if err := os.MkdirAll(dstDir, 0o700); err != nil {
				return fmt.Errorf("create checkpoint blob dir: %w", err)
			}
			madeDir = true
		}
		if err := store.writeFileAtomicSync(dstPath, content, 0o600); err != nil {
			return fmt.Errorf("write checkpoint blob: %w", err)
		}
	}
	return nil
}

// readBlob returns the content stored under a hash, verifying that the content
// still hashes to the requested sha256. A mismatch (corruption/tampering) is
// returned as an error so the caller skips the path rather than writing
// untrusted content as truth.
func (store *Store) readBlob(sessionID, hash string) ([]byte, error) {
	content, err := os.ReadFile(store.blobPath(sessionID, hash))
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(content)
	if got := hex.EncodeToString(sum[:]); got != hash {
		return nil, fmt.Errorf("checkpoint blob %s failed integrity check (got %s)", hash, got)
	}
	return content, nil
}

// pruneOrphanBlobsLocked is the body of pruneOrphanBlobs WITHOUT acquiring the
// session lock. The caller MUST already hold store.lockSession(sessionID).
// Holding the lock around referencedBlobs + ReadDir + Remove is what closes the
// race with a concurrent CaptureToolCheckpoint (which writes a blob and appends
// the referencing event under the same lock): the prune either sees the blob
// before it is written, or sees the event that references it — never the gap.
func (store *Store) pruneOrphanBlobsLocked(sessionID string) (int, error) {
	referenced, err := store.referencedBlobs(sessionID)
	if err != nil {
		return 0, err
	}
	entries, err := os.ReadDir(store.blobsDir(sessionID))
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	removed := 0
	for _, e := range entries {
		if e.IsDir() || referenced[e.Name()] {
			continue
		}
		if err := os.Remove(store.blobPath(sessionID, e.Name())); err == nil {
			removed++
		}
	}
	return removed, nil
}

func (store *Store) referencedBlobs(sessionID string) (map[string]bool, error) {
	events, err := store.ReadEvents(sessionID)
	if err != nil {
		return nil, err
	}
	refs := map[string]bool{}
	for _, ev := range events {
		if ev.Type != EventSessionCheckpoint {
			continue
		}
		var payload CheckpointPayload
		if err := json.Unmarshal(ev.Payload, &payload); err != nil {
			continue
		}
		for _, f := range payload.Files {
			if f.Blob != "" {
				refs[f.Blob] = true
			}
		}
	}
	return refs, nil
}

// sortedCheckpointsAfter returns checkpoint events with Sequence > targetSeq,
// newest first (so restoring applies the snapshot closest to the target last).
func (store *Store) sortedCheckpointsAfter(sessionID string, targetSeq int) ([]Event, error) {
	events, err := store.ReadEvents(sessionID)
	if err != nil {
		return nil, err
	}
	var checkpoints []Event
	for _, ev := range events {
		if ev.Type == EventSessionCheckpoint && ev.Sequence > targetSeq {
			checkpoints = append(checkpoints, ev)
		}
	}
	sort.Slice(checkpoints, func(i, j int) bool {
		return checkpoints[i].Sequence > checkpoints[j].Sequence
	})
	return checkpoints, nil
}
