package sessions

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// MinimumPruneAge is the shortest cutoff Prune accepts.
//
// A floor under the lease, not a substitute for it. An older Zero, or anything
// else that writes a session without taking the lease, is invisible to prune,
// and a day is long enough that a session such a writer is actively using has
// been written since.
const MinimumPruneAge = 24 * time.Hour

// PruneOptions chooses what Prune removes.
type PruneOptions struct {
	// OlderThan makes a session a candidate when it was last updated at least
	// this long ago. It must be at least MinimumPruneAge.
	OlderThan time.Duration
	// DryRun decides everything a real run would and removes nothing. It takes
	// the same locks to decide, so a Zero opening a session at the moment it is
	// being checked is told to try again, as it would be during a real run.
	DryRun bool
}

// PruneEntry is one session Prune removed (or in a dry run would remove), kept
// although it was old enough, or failed to remove.
type PruneEntry struct {
	SessionID string      `json:"sessionId"`
	Title     string      `json:"title,omitempty"`
	Kind      SessionKind `json:"kind,omitempty"`
	UpdatedAt string      `json:"updatedAt"`
	Bytes     int64       `json:"bytes"`
	Reason    string      `json:"reason,omitempty"`
}

// PruneReport says what Prune did, or in a dry run what it would do. Sessions
// newer than the cutoff are not listed.
type PruneReport struct {
	Cutoff string `json:"cutoff"`
	DryRun bool   `json:"dryRun"`
	// Removed are the sessions removed, or in a dry run the ones a real run
	// would remove.
	Removed []PruneEntry `json:"removed"`
	// Kept are sessions Prune left for a reason other than being recent.
	Kept []PruneEntry `json:"kept"`
	// Failed are sessions whose removal went wrong, or in a dry run could not be
	// checked; Reason says how. A failure after the metadata was removed leaves
	// a directory List no longer shows.
	Failed []PruneEntry `json:"failed"`
}

// The reasons Prune keeps a session that is old enough to remove.
const (
	PruneKeptOpen    = "open in another Zero process"
	PruneKeptParent  = "parent of a session that is kept"
	PruneKeptUpdated = "updated while pruning"
	PruneKeptUndated = "its last update time could not be read"
)

// pruneRemoveSeam runs after Prune holds a session's lease and write lock and
// before it re-reads the metadata. Nil in production.
var pruneRemoveSeam func(sessionID string)

// pruneRemoveDirSeam runs just before Prune removes a session's directory, and
// says whether Prune still holds the lease. Nil in production.
var pruneRemoveDirSeam func(sessionID string, leaseHeld bool)

// prunePlannedSeam runs once Prune has made its plan and before it removes
// anything. Nil in production.
var prunePlannedSeam func()

// Prune removes sessions last updated before the cutoff that OlderThan sets.
//
// Only on request: nothing in Zero calls it by itself (#971). It never removes:
//   - a session another process has open, which holds its lease (see Hold);
//   - a session with a descendant that is kept, including one created after the
//     plan was made, because Lineage and Tree fail on a missing ancestor;
//   - a session written between the plan and its removal;
//   - a session whose last update time cannot be read.
//
// Descendants are removed before their ancestors, so a descendant that turns
// out to be open part way through still keeps every ancestor above it. Within a
// session the metadata goes first, under both the lease and the write lock:
// from then on List and Get do not show it, so a removal that fails part way
// leaves nothing that looks like a session.
func (store *Store) Prune(options PruneOptions) (PruneReport, error) {
	if options.OlderThan < MinimumPruneAge {
		return PruneReport{}, fmt.Errorf("prune cutoff %s is shorter than the minimum of %s", options.OlderThan, MinimumPruneAge)
	}
	cutoff := store.now().UTC().Add(-options.OlderThan)
	report := PruneReport{
		Cutoff:  cutoff.Format(time.RFC3339),
		DryRun:  options.DryRun,
		Removed: []PruneEntry{},
		Kept:    []PruneEntry{},
		Failed:  []PruneEntry{},
	}
	all, err := store.List()
	if err != nil {
		return report, err
	}
	byID := make(map[string]Metadata, len(all))
	for _, session := range all {
		byID[session.SessionID] = session
	}

	candidates := map[string]bool{}
	for _, session := range all {
		updated, err := time.Parse(time.RFC3339, session.UpdatedAt)
		if err != nil {
			report.Kept = append(report.Kept, store.pruneEntry(session, PruneKeptUndated))
			continue
		}
		if !updated.Before(cutoff) {
			continue
		}
		// Open elsewhere at planning time. Checked again under the lease when the
		// session is removed; this pass is what lets its ancestors be kept too.
		release, locked, err := store.HoldExclusive(session.SessionID)
		if err != nil {
			report.Kept = append(report.Kept, store.pruneEntry(session, "its lease could not be checked: "+err.Error()))
			continue
		}
		if !locked {
			report.Kept = append(report.Kept, store.pruneEntry(session, PruneKeptOpen))
			continue
		}
		release()
		candidates[session.SessionID] = true
	}

	// Every ancestor of a session that stays, stays.
	for _, session := range all {
		if candidates[session.SessionID] {
			continue
		}
		for _, ancestor := range pruneAncestors(byID, session.SessionID) {
			if candidates[ancestor] {
				delete(candidates, ancestor)
				report.Kept = append(report.Kept, store.pruneEntry(byID[ancestor], PruneKeptParent))
			}
		}
	}

	order := make([]Metadata, 0, len(candidates))
	for id := range candidates {
		order = append(order, byID[id])
	}
	depth := func(id string) int { return len(pruneAncestors(byID, id)) }
	sort.Slice(order, func(left, right int) bool {
		leftDepth, rightDepth := depth(order[left].SessionID), depth(order[right].SessionID)
		if leftDepth != rightDepth {
			return leftDepth > rightDepth
		}
		if order[left].UpdatedAt != order[right].UpdatedAt {
			return order[left].UpdatedAt < order[right].UpdatedAt
		}
		return order[left].SessionID < order[right].SessionID
	})
	if prunePlannedSeam != nil {
		prunePlannedSeam()
	}

	keep := map[string]bool{}
	for _, session := range order {
		if keep[session.SessionID] {
			report.Kept = append(report.Kept, store.pruneEntry(session, PruneKeptParent))
			continue
		}
		entry := store.pruneEntry(session, "")
		removed, keptReason, err := store.pruneSession(session.SessionID, cutoff, byID, options.DryRun)
		switch {
		case err != nil:
			entry.Reason = err.Error()
			report.Failed = append(report.Failed, entry)
		case keptReason != "":
			entry.Reason = keptReason
			report.Kept = append(report.Kept, entry)
		case removed:
			report.Removed = append(report.Removed, entry)
		}
		if !removed {
			for _, ancestor := range pruneAncestors(byID, session.SessionID) {
				keep[ancestor] = true
			}
		}
	}
	return report, nil
}

// pruneAncestors lists a session's ancestors, nearest first, stopping at a
// missing one or a cycle.
func pruneAncestors(byID map[string]Metadata, sessionID string) []string {
	var ancestors []string
	seen := map[string]bool{sessionID: true}
	current, ok := byID[sessionID]
	for ok && current.ParentSessionID != "" && !seen[current.ParentSessionID] {
		parentID := current.ParentSessionID
		seen[parentID] = true
		ancestors = append(ancestors, parentID)
		current, ok = byID[parentID]
	}
	return ancestors
}

func (store *Store) pruneEntry(session Metadata, reason string) PruneEntry {
	return PruneEntry{
		SessionID: session.SessionID,
		Title:     session.Title,
		Kind:      session.SessionKind,
		UpdatedAt: session.UpdatedAt,
		Bytes:     store.sessionBytes(session.SessionID),
		Reason:    reason,
	}
}

// sessionBytes is what a session occupies on disk, without following links.
func (store *Store) sessionBytes(sessionID string) int64 {
	var total int64
	_ = filepath.WalkDir(store.sessionPath(sessionID), func(_ string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return nil
		}
		if info, err := entry.Info(); err == nil && info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	})
	return total
}

// childCreatedAfterPlan reports whether a session the plan did not see names
// sessionID as its parent. Only sessions missing from planned are read, so the
// cost is one directory listing plus whatever was created since the plan.
func (store *Store) childCreatedAfterPlan(sessionID string, planned map[string]Metadata) (bool, error) {
	entries, err := os.ReadDir(store.RootDir)
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		id := entry.Name()
		if !entry.IsDir() || id == sessionID {
			continue
		}
		if _, seen := planned[id]; seen {
			continue
		}
		child, err := store.readMetadata(id)
		if err != nil {
			continue // not a session, or one without its metadata
		}
		if child.ParentSessionID == sessionID {
			return true, nil
		}
	}
	return false, nil
}

// pruneSession removes one session that planning chose. It reports removed
// once the metadata is gone, even when leftovers could not be deleted (err says
// what), and a keptReason when the session turned out to be open, was written
// since the plan, or has a child the plan did not know about. planned is every
// session the plan saw. A dry run makes every one of those checks under the
// same locks and stops before removing anything; removed then says a real run
// would have gone on to remove the session.
func (store *Store) pruneSession(sessionID string, cutoff time.Time, planned map[string]Metadata, dryRun bool) (removed bool, keptReason string, err error) {
	releaseLease, locked, err := store.HoldExclusive(sessionID)
	if err != nil {
		return false, "", fmt.Errorf("check the session's lease: %w", err)
	}
	if !locked {
		return false, PruneKeptOpen, nil
	}
	leaseHeld := true
	letLeaseGo := func() {
		leaseHeld = false
		releaseLease()
	}
	defer letLeaseGo()
	release, err := store.lockSessionWithoutLease(sessionID)
	if err != nil {
		return false, "", err
	}
	released := false
	defer func() {
		if !released {
			release()
		}
	}()
	if pruneRemoveSeam != nil {
		pruneRemoveSeam(sessionID)
	}

	session, err := store.readMetadata(sessionID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, "", fmt.Errorf("the session disappeared while pruning")
		}
		return false, "", err
	}
	updated, err := time.Parse(time.RFC3339, session.UpdatedAt)
	if err != nil {
		return false, PruneKeptUndated, nil
	}
	if !updated.Before(cutoff) {
		return false, PruneKeptUpdated, nil
	}
	// A session forked or given a child after the plan was made, by a process that
	// has exited since, is not in the plan, and nothing else would stop its parent
	// going. Every way of creating a session under a parent holds that parent
	// first (holdParent), so none can start while this lease is held exclusively,
	// and one that finished has its metadata on disk.
	child, err := store.childCreatedAfterPlan(sessionID, planned)
	if err != nil {
		return false, "", fmt.Errorf("look for sessions created under it: %w", err)
	}
	if child {
		return false, PruneKeptParent, nil
	}
	if dryRun {
		return true, "", nil
	}

	// THE METADATA FIRST. From here the session no longer exists to List or Get.
	if err := os.Remove(store.metadataPath(sessionID)); err != nil {
		return false, "", fmt.Errorf("remove the session metadata: %w", err)
	}
	dir := store.sessionPath(sessionID)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return true, "", fmt.Errorf("list what is left of the session: %w", err)
	}
	for _, entry := range entries {
		// session.lock is held right here; it goes after the release below. The
		// lease file is held too, but it was opened for deletion (openLeaseFile).
		if entry.Name() == filepath.Base(store.lockPath(sessionID)) {
			continue
		}
		if err := os.RemoveAll(filepath.Join(dir, entry.Name())); err != nil {
			return true, "", fmt.Errorf("remove %s: %w", entry.Name(), err)
		}
	}
	release()
	released = true
	// A writer that takes session.lock in the gap only keeps an empty directory
	// alive, and its own write then fails: the metadata is already gone.
	if err := os.Remove(store.lockPath(sessionID)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return true, "", fmt.Errorf("remove the session lock: %w", err)
	}
	// Let go of the lease before the directory. lease.lock went with the rest, but
	// where a delete only takes effect once the last handle closes (Windows without
	// POSIX delete semantics: older builds, or FAT and exFAT volumes), this handle
	// would keep the directory from being empty.
	letLeaseGo()
	if pruneRemoveDirSeam != nil {
		pruneRemoveDirSeam(sessionID, leaseHeld)
	}
	if err := os.Remove(dir); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return true, "", fmt.Errorf("remove the session directory: %w", err)
	}
	return true, "", nil
}
