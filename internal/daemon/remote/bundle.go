package remote

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"math"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Gitlawb/zero/internal/daemon"
	"github.com/Gitlawb/zero/internal/fsutil"
	"github.com/Gitlawb/zero/internal/lockutil"
)

// gitTimeout bounds a single git invocation (bundle create/verify, clone) so a
// hung git process cannot pin a connection or the upload path indefinitely.
const gitTimeout = 2 * time.Minute

// bundleChunkSize is the per-frame payload when streaming a bundle file. It is
// kept comfortably under daemon.MaxFrameSize (1 MiB).
const bundleChunkSize = 512 << 10

// bundleHeader is the first control frame of a bundle upload (after the auth
// handshake): it declares the link id and the exact byte size that follows.
type bundleHeader struct {
	LinkID string `json:"link_id"`
	Size   int64  `json:"size"`
}

// bundleResult is the bridge's reply once the bundle is received and extracted.
type bundleResult struct {
	OK      bool   `json:"ok"`
	Path    string `json:"path,omitempty"`
	Message string `json:"message,omitempty"`
}

func writeBundleHeader(w io.Writer, h bundleHeader) error {
	payload, err := json.Marshal(h)
	if err != nil {
		return err
	}
	return daemon.WriteFrame(w, daemon.KindCtrl, payload)
}

func readBundleHeader(r io.Reader) (bundleHeader, error) {
	kind, payload, err := daemon.ReadFrame(r)
	if err != nil {
		return bundleHeader{}, err
	}
	if kind != daemon.KindCtrl {
		return bundleHeader{}, errors.New("remote: expected bundle header frame")
	}
	var h bundleHeader
	if err := json.Unmarshal(payload, &h); err != nil {
		return bundleHeader{}, fmt.Errorf("remote: decode bundle header: %w", err)
	}
	return h, nil
}

func writeBundleResult(w io.Writer, res bundleResult) error {
	payload, err := json.Marshal(res)
	if err != nil {
		return err
	}
	return daemon.WriteFrame(w, daemon.KindCtrl, payload)
}

func readBundleResult(r io.Reader) (bundleResult, error) {
	kind, payload, err := daemon.ReadFrame(r)
	if err != nil {
		return bundleResult{}, err
	}
	if kind != daemon.KindCtrl {
		return bundleResult{}, errors.New("remote: expected bundle result frame")
	}
	var res bundleResult
	if err := json.Unmarshal(payload, &res); err != nil {
		return bundleResult{}, fmt.Errorf("remote: decode bundle result: %w", err)
	}
	return res, nil
}

// ---- server side -----------------------------------------------------------

// handleBundle receives an uploaded bundle, extracts it into a per-link working
// tree, and reports the outcome. It always closes conn.
func (b *Bridge) handleBundle(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	res := b.receiveBundle(conn)
	if !res.OK {
		b.logf("remote: bundle upload rejected: %s", res.Message)
	}
	_ = writeBundleResult(conn, res)
}

// receiveBundle reads the header + framed bundle bytes, verifies the bundle, and
// extracts it under bundleDir. Every failure returns a non-OK result rather than
// panicking, and the staged temp file is always removed.
func (b *Bridge) receiveBundle(conn net.Conn) bundleResult {
	hdr, err := readBundleHeader(conn)
	if err != nil {
		return bundleResult{Message: "read bundle header: " + err.Error()}
	}
	id, err := sanitizeLinkID(hdr.LinkID)
	if err != nil {
		return bundleResult{Message: err.Error()}
	}
	if hdr.Size <= 0 || hdr.Size > b.maxBundleBytes {
		return bundleResult{Message: fmt.Sprintf("invalid bundle size %d (max %d)", hdr.Size, b.maxBundleBytes)}
	}

	tmp, err := os.CreateTemp("", "zero-remote-*.bundle")
	if err != nil {
		return bundleResult{Message: "stage bundle: " + err.Error()}
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := streamFramesToFile(conn, tmp, hdr.Size); err != nil {
		_ = tmp.Close()
		return bundleResult{Message: "receive bundle: " + err.Error()}
	}
	if err := tmp.Close(); err != nil {
		return bundleResult{Message: "stage bundle: " + err.Error()}
	}

	verifyCtx, cancelVerify := context.WithTimeout(context.Background(), gitTimeout)
	defer cancelVerify()
	if err := gitBundleVerify(verifyCtx, tmpName); err != nil {
		return bundleResult{Message: "bundle verify: " + err.Error()}
	}
	dest := filepath.Join(b.bundleDir, id)
	if !withinDir(b.bundleDir, dest) {
		return bundleResult{Message: "invalid link id"}
	}
	// extractBundle starts the clone's own gitTimeout once it holds the lock for
	// dest, so an upload queued behind another does not spend that budget waiting.
	if err := extractBundle(context.Background(), tmpName, dest, b.logf); err != nil {
		if errors.Is(err, errCommittedNotReleased) {
			// The tree at dest is the one this upload sent. Reporting a
			// rejection would have the client send it again over itself, so the
			// result carries both facts and the fault is logged for an operator
			// rather than turned into a retry.
			b.logf("remote: extracted %s but could not release its extract lock: %v", dest, err)
			return bundleResult{OK: true, Path: dest, Message: "extract bundle: " + err.Error()}
		}
		return bundleResult{Message: "extract bundle: " + err.Error()}
	}
	return bundleResult{OK: true, Path: dest}
}

// streamFramesToFile copies exactly size bytes from KindData frames on r into w.
// A non-data frame, or any frame that would overrun the declared size, is an
// error (fail closed) so a peer cannot write past the cap.
func streamFramesToFile(r io.Reader, w io.Writer, size int64) error {
	remaining := size
	for remaining > 0 {
		kind, payload, err := daemon.ReadFrame(r)
		if err != nil {
			return err
		}
		if kind != daemon.KindData {
			return errors.New("expected bundle data frame")
		}
		if int64(len(payload)) > remaining {
			return errors.New("bundle exceeds declared size")
		}
		if _, err := w.Write(payload); err != nil {
			return err
		}
		remaining -= int64(len(payload))
	}
	return nil
}

// stagingPrefix names the per-extract staging directories created beside dest.
// sanitizeLinkID refuses every dot-prefixed id so a link can never name one.
const stagingPrefix = ".staging-"

// keptPrefix names a staged backup that recovery decided to keep rather than
// order against a tree it had just put back. Recovery runs again on every start,
// and by then it can no longer tell that tree from one a later extract
// published, so the copy is parked under a name the scan does not enumerate.
// sanitizeLinkID refuses every dot-prefixed id, so this can never take a link's
// name.
const keptPrefix = ".kept-"

// lockDirName holds the per-link advisory lock files that serialize extracts
// across processes. Dot-prefixed for the same reason stagingPrefix is.
const lockDirName = ".extract-locks"

// stagingMarkerFile records, inside a staging dir, the transaction that created
// it: its kind, the link it is for, and the sequence in its own name. Without it
// a crash leaves an orphan nothing can attribute, and recovery has to retain a
// copy it can never name.
const stagingMarkerFile = "txn"

// committedFile records, inside a staging dir, that the publish rename landed.
// It is the only evidence that the copy in backup was superseded, so recovery
// has to keep a backup that carries no flag.
const committedFile = "committed"

// txnKindBundleExtract is the marker kind extractBundle writes. A marker naming
// another kind belongs to another site's transaction and is not this code's to
// act on.
const txnKindBundleExtract = "bundle-extract"

// extractLockPoll is how often a cross-process extract lock is retried.
const extractLockPoll = 50 * time.Millisecond

// fsOps is the filesystem seam the write path, the allocator, and recovery take
// every step through. A step that calls os directly cannot be made to fail, and
// the crash each of these steps exists to survive is then only reasoned about.
// Every field is one call, so a test can fail the second remove or the rename
// whose source is one particular backup and leave the rest of the pass real.
type fsOps struct {
	rename     func(from, to string) error
	removeAll  func(path string) error
	stat       func(name string) (os.FileInfo, error)
	lstat      func(name string) (os.FileInfo, error)
	readDir    func(name string) ([]os.DirEntry, error)
	readFile   func(name string) ([]byte, error)
	mkdir      func(name string, perm os.FileMode) error
	writeFile  func(name string, data []byte, perm os.FileMode) error
	create     func(name string, flag int, perm os.FileMode) (*os.File, error)
	createTemp func(dir, pattern string) (*os.File, error)
}

// stagingFS is the seam every filesystem step in this file goes through. Tests
// swap a field and restore it; nothing else writes to it.
var stagingFS = fsOps{
	rename:     os.Rename,
	removeAll:  os.RemoveAll,
	stat:       os.Stat,
	lstat:      os.Lstat,
	readDir:    os.ReadDir,
	readFile:   os.ReadFile,
	mkdir:      os.Mkdir,
	writeFile:  os.WriteFile,
	create:     os.OpenFile,
	createTemp: os.CreateTemp,
}

// errCommittedNotReleased reports the one failure an operation can have after
// its own mutation already landed: the extract lock is released last, so a
// release failure is all that is left to go wrong. A caller that read it as a
// plain failure would repeat a destructive step that already ran, so the error
// says both facts and every caller that can act on the difference tests for it.
var ErrCommittedNotReleased = errors.New("remote: the change landed but its extract lock was not released")

// errCommittedNotReleased is the name this package uses internally. Exported
// above because the kept-backup command has to tell a removal that completed
// from one that did not happen, and only the sentinel carries that.
var errCommittedNotReleased = ErrCommittedNotReleased

// extractFileLock is the part of lockutil.FileLock this file uses. Release joins
// the platform unlock and the close, and neither can be made to fail through a
// real lock, so without a seam the one path that reports a cleanup failure could
// only be reasoned about.
type extractFileLock interface{ Release() error }

// acquireExtractLock is the seam the cross-process extract lock is taken
// through. Tests swap it and restore it; nothing else writes to it.
var acquireExtractLock = func(root, path string) (extractFileLock, error) {
	lock, err := lockutil.TryAcquireFileLockAt(root, path)
	if err != nil {
		return nil, err
	}
	return lock, nil
}

// extractLocks serializes extracts per destination. Each bundle upload runs in
// its own connection goroutine, so two uploads of one link id would otherwise
// interleave their swap steps and clobber each other.
var extractLocks = struct {
	mu    sync.Mutex
	locks map[string]*extractLock
}{locks: map[string]*extractLock{}}

type extractLock struct {
	mu   sync.Mutex
	refs int
}

// lockExtract blocks until dest is free and returns its release func. Entries
// are refcounted so the map cannot grow with every link id ever uploaded.
func lockExtract(dest string) func() {
	entry := holdExtractRef(dest)
	entry.mu.Lock()
	return func() {
		entry.mu.Unlock()
		dropExtractRef(dest)
	}
}

// waitForExtract takes the in-process lock under the same budget the per-link
// file lock waits out, so a queued upload gives the connection slot back rather
// than holding it for as long as the extract ahead of it runs. A bare Lock here
// is the one step in the whole extract with no bound: it is taken before the
// file lock, so its wait is not covered by that one's deadline.
func waitForExtract(ctx context.Context, dest string) (func(), error) {
	deadline := time.NewTimer(gitTimeout)
	defer deadline.Stop()
	for {
		release, held := tryLockExtract(dest)
		if !held {
			return release, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline.C:
			return nil, fmt.Errorf("remote: timed out waiting for an extract in this process to finish with %s", dest)
		case <-time.After(extractLockPoll):
		}
	}
}

// tryLockExtract takes the in-process lock without waiting, reporting held when
// a goroutine in this daemon owns dest. Recovery cannot use lockExtract: an
// extract holds its destination across the whole clone, so a recovering pass
// that blocked here would wait out that clone before deciding anything about a
// destination it was going to skip either way.
func tryLockExtract(dest string) (release func(), held bool) {
	entry := holdExtractRef(dest)
	if !entry.mu.TryLock() {
		dropExtractRef(dest)
		return nil, true
	}
	return func() {
		entry.mu.Unlock()
		dropExtractRef(dest)
	}, false
}

// holdExtractRef returns dest's lock entry with a reference taken, so the entry
// cannot be dropped from the map while a caller is waiting on it.
func holdExtractRef(dest string) *extractLock {
	extractLocks.mu.Lock()
	defer extractLocks.mu.Unlock()
	entry := extractLocks.locks[dest]
	if entry == nil {
		entry = &extractLock{}
		extractLocks.locks[dest] = entry
	}
	entry.refs++
	return entry
}

// dropExtractRef releases one reference and forgets the entry once nothing holds
// it, which is what keeps the map from growing with every link id ever seen.
func dropExtractRef(dest string) {
	extractLocks.mu.Lock()
	defer extractLocks.mu.Unlock()
	entry := extractLocks.locks[dest]
	if entry == nil {
		return
	}
	entry.refs--
	if entry.refs == 0 {
		delete(extractLocks.locks, dest)
	}
}

// lockExtractFile takes the cross-process advisory lock for dest, waiting until
// ctx is done or the wait budget runs out. The in-process lock already excludes
// this daemon's own goroutines; this excludes a second daemon sharing the dir.
func lockExtractFile(ctx context.Context, bundleDir, dest string) (func() error, error) {
	deadline := time.NewTimer(gitTimeout)
	defer deadline.Stop()
	for {
		release, held, err := tryLockExtractFile(bundleDir, dest)
		if err != nil {
			return nil, err
		}
		if !held {
			return release, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline.C:
			return nil, fmt.Errorf("remote: timed out waiting for the extract lock on %s", dest)
		case <-time.After(extractLockPoll):
		}
	}
}

// tryLockExtractFile takes the per-link advisory lock without waiting. It
// reports held when a live extract owns the link, which is never an error: the
// caller either waits or leaves that link alone.
func tryLockExtractFile(bundleDir, dest string) (release func() error, held bool, err error) {
	lockDir := filepath.Join(bundleDir, lockDirName)
	if err := os.MkdirAll(lockDir, 0o700); err != nil {
		return nil, false, err
	}
	lock, err := acquireExtractLock(bundleDir, filepath.Join(lockDir, filepath.Base(dest)+".lock"))
	if err != nil {
		if errors.Is(err, lockutil.ErrLockHeld) {
			return nil, true, nil
		}
		return nil, false, err
	}
	// Release joins the platform unlock and the close. It closes the handle
	// whichever of the two fails, and closing the descriptor drops the lock, so
	// a failure here does not leave the next caller waiting on a lock nobody
	// holds. What it does say is that a step of this operation did not run to
	// completion, so the caller merges it into its own outcome rather than
	// reporting a clean run over it.
	return lock.Release, false, nil
}

// mergeReleaseFailure folds a lock release failure into an operation's own
// outcome. committed says the operation's mutation already landed, which the
// error has to keep saying: a caller that read this as "nothing happened" would
// repeat a destructive step that already ran, publishing a bundle over the tree
// it just installed or re-running a removal that is done.
func mergeReleaseFailure(err, relErr error, committed bool, id string) error {
	if relErr == nil {
		return err
	}
	if err != nil {
		// The primary cause stays the wrapped one: it is what the operation
		// failed on, and the lock is a second fact about the same run.
		return fmt.Errorf("%w (releasing the extract lock on %s also failed: %v)", err, id, relErr)
	}
	if committed {
		return fmt.Errorf("%w: %s: %v", errCommittedNotReleased, id, relErr)
	}
	return fmt.Errorf("remote: could not release the extract lock on %s: %w", id, relErr)
}

// recoverBundleDir repairs what a crash left behind in dir. It runs in two
// halves: a scan that discovers and classifies every candidate without touching
// anything, and a reconcile per destination that takes that destination's locks,
// re-validates everything it is about to act on, and only then acts. Splitting
// them is what keeps a decision from being made against state nobody was
// holding, and what keeps one destination's fault from reaching another's.
//
// Nothing here reads a clock, and nothing here remembers the previous pass. Both
// were sources of deletes the on-disk state did not license: an mtime says when
// a directory was written, not who owns it, and a per-pass map makes the second
// pass over the same directory reach a different answer from the first.
//
// It is called once at bridge construction, before any upload is served, and
// never removes a staging dir a live extract may hold.
func recoverBundleDir(dir string, logf func(string, ...any)) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	byDest := scanBundleDir(dir, logf)
	// Sorted, so two passes over one directory visit destinations in the same
	// order and a failure is reproducible rather than a function of readDir.
	for _, id := range slices.Sorted(maps.Keys(byDest)) {
		reconcileLink(dir, id, byDest[id], logf)
	}
}

// bundleCandidate is a staging directory the scan attributed to a destination:
// its name passed the strict grammar, it holds no work tree at its root, and its
// marker agrees with both the name and a link id the write path would accept.
type bundleCandidate struct {
	path string
	seq  int64
	dest string
}

// scanBundleDir reads dir once and groups every attributable staging directory
// by the destination its marker names. It mutates nothing: a directory that is
// going to be deleted is decided on under the destination's lock, and this runs
// before any lock is held. Entries it cannot attribute are reported here, once
// per pass, because a copy nothing names is one an operator cannot find. An
// entry whose probe failed is grouped too, when its marker still names a
// destination, so the fault reaches the reconcile that can stop that
// destination rather than disappearing with the entry.
//
// An entry it cannot attribute at all ends the scan with no groups, so the pass
// restores nothing and deletes nothing.
func scanBundleDir(dir string, logf func(string, ...any)) map[string][]bundleCandidate {
	entries, err := stagingFS.readDir(dir)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			logf("remote: could not scan bundle dir %s: %v", dir, err)
		}
		return nil
	}
	byDest := map[string][]bundleCandidate{}
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), stagingPrefix) {
			continue
		}
		staging := filepath.Join(dir, entry.Name())
		seq, ok := stagingStamp(entry.Name())
		if !ok {
			logf("remote: %s does not carry a name this code writes; leaving it in place", staging)
			continue
		}
		id, ok, unreadable := attributeStagingDir(dir, staging, seq, logf)
		if unreadable && id == "" {
			// Nothing on disk names a destination for this entry, so no group
			// can carry the fault and the whole pass declines instead. Dropping
			// the entry read as the cheaper trade until the fault it is most
			// likely to come from turned up: a fault on the staging directory
			// itself blocks the work-tree probe and the marker read together,
			// which is precisely how an entry ends up naming nothing, and the
			// group for its link is then left holding only the older copies.
			// Publishing one of those is not a failure to prevent a wrong
			// version, it is installing one over a newer copy nobody could
			// read. Declining costs the destinations that already serve
			// nothing: recovery only ever puts a tree back, so a pass that does
			// not run leaves every live tree exactly where it is.
			logf("remote: %s cannot be attributed to a link; recovering nothing in %s until it can be read", staging, dir)
			return nil
		}
		if !ok && !unreadable {
			continue
		}
		// An entry that could not be read is grouped like any other candidate
		// for the destination it names: reconcile re-probes it under the lock,
		// and a fault that is still there stops that destination. Dropping it
		// here would leave the group holding only the older copies, and one of
		// those would be published over the copy nobody could read.
		byDest[id] = append(byDest[id], bundleCandidate{path: staging, seq: seq, dest: id})
	}
	return byDest
}

// attributeStagingDir decides whether staging is this code's to act on. The
// order is load-bearing: the work-tree veto runs before the marker is read,
// because link ids starting with '.' used to be accepted, so a published work
// tree can sit under the exact generated name AND carry a file called txn at its
// root. Reading the marker first would let that file license a delete.
func attributeStagingDir(dir, staging string, seq int64, logf func(string, ...any)) (id string, ok bool, unreadable bool) {
	if _, err := stagingFS.stat(filepath.Join(staging, ".git")); err == nil {
		logf("remote: %s holds a work tree, not a staged extract; leaving it in place", staging)
		return "", false, false
	} else if !errors.Is(err, fs.ErrNotExist) {
		// Not knowing whether this is a work tree is not the same as knowing it
		// is not one. The veto sits ahead of the marker because a published tree
		// can carry a file named txn at its own root, so falling through on an
		// unreadable probe would let the tree answer for itself. Reading the
		// marker for the destination's name alone is the same fall-through: the
		// name is what stops a link's recovery, and a foreign tree that supplied
		// it stops that link on every pass for as long as its own probe fails.
		// So the question is put a second time, through a different call, before
		// anything the marker says is used.
		switch _, lerr := stagingFS.lstat(filepath.Join(staging, ".git")); {
		case lerr == nil:
			logf("remote: %s could not be checked for a work tree (%v) and holds one at its root; leaving it in place", staging, err)
			return "", false, false
		case errors.Is(lerr, fs.ErrNotExist):
			// The work-tree question is settled: nothing sits at .git, so the
			// marker is the staged copy's own and names the destination this
			// fault has to stop. ok stays false, so nothing the marker says can
			// license an action here.
			logf("remote: %s could not be checked for a work tree (%v); leaving it in place", staging, err)
			return markerDest(dir, staging, seq), false, true
		default:
			logf("remote: %s could not be checked for a work tree (%v, then %v); leaving it in place", staging, err, lerr)
			return "", false, true
		}
	}
	m, err := readMarker(staging)
	if err != nil {
		logf("remote: %s has no usable transaction marker (%v); leaving it in place", staging, err)
		// A marker that is absent says this transaction never got far enough to
		// claim the directory. A marker that could not be read says nothing at
		// all, and treating the two alike lets an older copy be published over
		// the one whose state is still unknown.
		return "", false, !errors.Is(err, errMarkerMissing)
	}
	if m.Kind != txnKindBundleExtract {
		logf("remote: %s carries a %q transaction marker, which no extract wrote; leaving it in place", staging, m.Kind)
		return "", false, false
	}
	if m.Seq != seq {
		// The name is the authority: createSequencedStagingDir walks up from the
		// number it asked for, and it reads the name back before using it. A
		// marker that disagrees with the name proves nothing about who wrote
		// either of them.
		logf("remote: %s carries a marker for sequence %d but is named %d; leaving it in place", staging, m.Seq, seq)
		return "", false, false
	}
	// Everything read off disk goes back through the write path's own checks. A
	// marker is a file, and a file can be edited by anything that can reach the
	// bundle dir.
	id, err = sanitizeLinkID(m.Dest)
	if err != nil {
		logf("remote: staged tree in %s names an invalid link (%v); leaving it in place", staging, err)
		return "", false, false
	}
	if !withinDir(dir, filepath.Join(dir, id)) {
		logf("remote: staged tree in %s names a link outside the bundle dir; leaving it in place", staging)
		return "", false, false
	}
	return id, true, false
}

// markerDest reads the destination a staged tree names, with none of the
// authority attributeStagingDir grants a clean read: it exists so an entry whose
// probe failed can still be attributed to the destination whose recovery has to
// stop. Every check attribution makes is repeated, because a marker that fails
// one of them names no destination this code would have written, and refusing to
// answer is what keeps a fault from stopping a link it never belonged to. The
// one check it does not repeat is the work-tree veto, and its only caller
// settles that question before calling: a tree that may be a published work tree
// must never reach this at all.
func markerDest(dir, staging string, seq int64) string {
	m, err := readMarker(staging)
	if err != nil || m.Kind != txnKindBundleExtract || m.Seq != seq {
		return ""
	}
	id, err := sanitizeLinkID(m.Dest)
	if err != nil || !withinDir(dir, filepath.Join(dir, id)) {
		return ""
	}
	return id
}

// candidateState is one candidate classified per the recovery design. empty,
// committed and usable are three independent facts, and the disposition is a
// function of them plus the destination's own state; nothing else.
type candidateState struct {
	bundleCandidate
	empty     bool
	committed bool
	usable    bool
}

// candidateVerdict separates the two ways a candidate leaves the reconcile
// without being acted on. Dropped means it is no longer attributable, which is
// this candidate's business alone. Unreadable means a filesystem fault, which
// says nothing about the candidate and stops the whole destination: acting on
// the rest would be deciding against a directory that could not be read.
type candidateVerdict int

const (
	verdictOwned candidateVerdict = iota
	verdictDropped
	verdictUnreadable
)

// reconcileLink classifies and then acts on every candidate for one destination,
// with that destination's in-process and cross-process locks held across both.
// The lock covers the check and the action together: a live extract mid-swap is
// indistinguishable on disk from a crashed one, and the lock is the only thing
// that tells them apart.
func reconcileLink(dir, id string, cands []bundleCandidate, logf func(string, ...any)) {
	dest := filepath.Join(dir, id)
	unlock, held := tryLockExtract(dest)
	if held {
		logf("remote: an extract in this process holds %s; leaving its staged copies alone", id)
		return
	}
	defer unlock()
	unlockFile, heldFile, err := tryLockExtractFile(dir, dest)
	if err != nil {
		logf("remote: could not lock %s while recovering it: %v", id, err)
		return
	}
	if heldFile {
		logf("remote: another process holds %s; leaving its staged copies alone", id)
		return
	}
	defer func() {
		// Recovery returns nothing, so this log is the only place a lock it
		// could not let go of can be seen. A silent failure here reads as a
		// clean pass while the next one waits out a lock nobody holds.
		if err := unlockFile(); err != nil {
			logf("remote: could not release the extract lock on %s: %v", id, err)
		}
	}()

	states, ok := classifyCandidates(dir, id, cands, logf)
	if !ok {
		return
	}
	present, usable, ok := classifyDest(dest, logf)
	if !ok {
		return
	}
	if present && usable {
		reconcileAgainstUsableDest(dir, id, states, logf)
		return
	}
	restoreForDest(dir, id, dest, present, states, logf)
}

// classifyCandidates re-validates every candidate under the lock and returns
// them newest first. The scan's reading is deliberately not trusted: it ran
// before the lock, so anything it saw could have been changed by the process the
// lock now excludes.
func classifyCandidates(dir, id string, cands []bundleCandidate, logf func(string, ...any)) ([]candidateState, bool) {
	states := make([]candidateState, 0, len(cands))
	for _, c := range cands {
		st, verdict := classifyCandidate(dir, c, logf)
		switch verdict {
		case verdictOwned:
			states = append(states, st)
		case verdictDropped:
			continue
		case verdictUnreadable:
			logf("remote: leaving every staged copy for %s in place until %s can be read", id, c.path)
			return nil, false
		}
	}
	// Sequence order is transaction order: an extract that reads an existing
	// entry always numbers above it, so a higher sequence is a later
	// transaction whatever the clock did in between.
	slices.SortStableFunc(states, func(a, b candidateState) int { return cmp.Compare(b.seq, a.seq) })
	return states, true
}

func classifyCandidate(dir string, c bundleCandidate, logf func(string, ...any)) (candidateState, candidateVerdict) {
	st := candidateState{bundleCandidate: c}
	id, ok, unreadable := attributeStagingDir(dir, c.path, c.seq, logf)
	if unreadable {
		return st, verdictUnreadable
	}
	if !ok || id != c.dest {
		return st, verdictDropped
	}
	backup := filepath.Join(c.path, "backup")
	switch _, err := stagingFS.stat(backup); {
	case err == nil:
	case errors.Is(err, fs.ErrNotExist):
		// A readable marker with no set-aside content is the one shape that
		// proves the directory holds nothing: owned, and empty.
		st.empty = true
		return st, verdictOwned
	default:
		logf("remote: could not read the staged tree in %s: %v", c.path, err)
		return st, verdictUnreadable
	}
	switch _, err := stagingFS.stat(filepath.Join(c.path, committedFile)); {
	case err == nil:
		st.committed = true
	case errors.Is(err, fs.ErrNotExist):
	default:
		logf("remote: could not read the commit flag in %s: %v", c.path, err)
		return st, verdictUnreadable
	}
	switch _, err := stagingFS.stat(filepath.Join(backup, ".git")); {
	case err == nil:
		st.usable = true
	case errors.Is(err, fs.ErrNotExist):
		// Durable: this copy will never pass the predicate, so it is skipped in
		// selection and kept. That is a different fact from the error below.
	default:
		logf("remote: could not tell whether the staged tree in %s is usable: %v", c.path, err)
		return st, verdictUnreadable
	}
	return st, verdictOwned
}

// classifyDest reports whether dest exists and whether it can serve. Usability
// is dest/.git present, checked structurally. isGitWorktree shells out to git
// rev-parse, which discovers upward, so a bundle dir under any enclosing
// checkout would answer "usable" for an empty destination and license deleting
// every copy beside it.
func classifyDest(dest string, logf func(string, ...any)) (present, usable, ok bool) {
	switch _, err := stagingFS.stat(dest); {
	case err == nil:
	case errors.Is(err, fs.ErrNotExist):
		return false, false, true
	default:
		logf("remote: could not read %s: %v; leaving its staged copies in place", dest, err)
		return false, false, false
	}
	switch _, err := stagingFS.stat(filepath.Join(dest, ".git")); {
	case err == nil:
		return true, true, true
	case errors.Is(err, fs.ErrNotExist):
		return true, false, true
	default:
		logf("remote: could not tell whether %s is usable: %v; leaving its staged copies in place", dest, err)
		return true, false, false
	}
}

// reconcileAgainstUsableDest handles the destination that is present and can
// serve. Only a commit flag licenses a delete here: the flag is written by the
// transaction that published over that exact copy, so it is evidence about this
// copy. "The destination exists" is not, and deleting on it is what loses the
// last copy of a tree when the destination came from anywhere else.
func reconcileAgainstUsableDest(dir, id string, states []candidateState, logf func(string, ...any)) {
	for _, c := range states {
		switch {
		case c.empty:
			reapOwnedEmpty(c.path, logf)
		case c.committed:
			// The upload that published dest reported success to its client
			// while this whole copy of the prior tree was still on disk. Say
			// what is being reclaimed, so a bridge running without a logger is
			// not the difference between the space being accounted for and not.
			logf("remote: reclaiming the staged tree in %s that %s's live tree superseded", c.path, id)
			if err := stagingFS.removeAll(c.path); err != nil {
				logf("remote: could not remove superseded staging dir %s: %v", c.path, err)
			}
		default:
			logf("remote: keeping the staged tree in %s: nothing proves %s published over it", c.path, id)
			parkKeptBackup(dir, c.path, id, logf)
		}
	}
}

// restoreForDest handles the destination that is absent or cannot serve. It
// selects before it moves anything: a destination with no usable candidate is
// left exactly as it was found, because taking its husk apart with nothing to
// put in its place leaves the operator with strictly less than they had.
func restoreForDest(dir, id, dest string, present bool, states []candidateState, logf func(string, ...any)) {
	winner := selectCandidate(states)
	if winner < 0 {
		logf("remote: %s has no usable staged copy to restore; leaving it as it is", id)
		parkRemaining(dir, id, states, -1, logf)
		return
	}
	husk := ""
	if present {
		var err error
		husk, err = setAsideHusk(dir, id, dest, logf)
		if err != nil {
			logf("remote: could not set the unusable tree at %s aside: %v; leaving %s alone", dest, err, id)
			return
		}
	}
	sel := states[winner]
	if err := fsutil.RenameWithRetry(filepath.Join(sel.path, "backup"), dest, stagingFS.rename); err != nil {
		logf("remote: could not restore the staged tree for %s from %s: %v", id, sel.path, err)
		// No fallback to an older copy: installing one would put a tree at dest
		// that the next pass reads as having published over the newer copy,
		// which is how a retained copy turns into a deleted one.
		if husk != "" {
			putHuskBack(dest, husk, logf)
		}
		return
	}
	logf("remote: restored the work tree for %s from %s after an interrupted extract", id, sel.path)
	// The winner's directory is now owned and empty, which is the one delete
	// that needs no flag.
	reapOwnedEmpty(sel.path, logf)
	if husk != "" {
		logf("remote: keeping the tree that could not serve at %s", dest)
		parkKeptBackup(dir, husk, id, logf)
	}
	parkRemaining(dir, id, states, winner, logf)
}

// selectCandidate is the whole selection rule: the newest uncommitted usable
// copy, and only when there is none, the newest committed usable one. A
// committed copy is second because the transaction that flagged it published
// something over it; an uncommitted one is the copy no publish is known to have
// replaced.
func selectCandidate(states []candidateState) int {
	for i, c := range states {
		if !c.empty && !c.committed && c.usable {
			return i
		}
	}
	for i, c := range states {
		if !c.empty && c.committed && c.usable {
			return i
		}
	}
	return -1
}

// parkRemaining keeps every candidate selection did not restore. Nothing here is
// deleted: without a commit flag beside a usable destination there is no
// evidence any of these was superseded, and a copy with no evidence against it
// may be the last one.
func parkRemaining(dir, id string, states []candidateState, winner int, logf func(string, ...any)) {
	for i, c := range states {
		if i == winner {
			continue
		}
		if c.empty {
			reapOwnedEmpty(c.path, logf)
			continue
		}
		logf("remote: keeping the staged tree in %s that %s was not restored from", c.path, id)
		parkKeptBackup(dir, c.path, id, logf)
	}
}

// reapOwnedEmpty removes a staging dir that carries a valid marker and holds no
// set-aside content. The marker is what makes this safe: a directory with none
// names no destination, so no lock excludes the live allocation that may be
// sitting between its Mkdir and its marker write right now.
func reapOwnedEmpty(staging string, logf func(string, ...any)) {
	if err := stagingFS.removeAll(staging); err != nil {
		logf("remote: could not remove the empty staging dir %s: %v", staging, err)
	}
}

// setAsideHusk moves a destination that cannot serve into a fresh sequenced
// staging dir, so the restore has somewhere to land. It allocates and attributes
// before it moves anything, so a failure never leaves a copy of a tree in a
// directory no marker names.
func setAsideHusk(dir, id, dest string, logf func(string, ...any)) (string, error) {
	seq, err := nextStagingSeq(dir)
	if err != nil {
		return "", err
	}
	staging, err := createSequencedStagingDir(dir, seq)
	if err != nil {
		return "", err
	}
	claimed, _ := stagingStamp(filepath.Base(staging))
	if err := writeMarker(staging, txnMarker{Kind: txnKindBundleExtract, Dest: id, Seq: claimed}); err != nil {
		// Nothing has moved yet, so the directory holds nothing and removing it
		// loses nothing. Leaving it would be residue no marker attributes.
		reapOwnedEmpty(staging, logf)
		return "", err
	}
	if err := fsutil.RenameWithRetry(dest, filepath.Join(staging, "backup"), stagingFS.rename); err != nil {
		reapOwnedEmpty(staging, logf)
		return "", err
	}
	return staging, nil
}

// putHuskBack undoes a set-aside whose restore then failed, so the destination
// is left exactly as recovery found it. If the move back fails too, the husk
// stays where it is as an owned candidate of its own and both failures are
// named: the copy is still on disk, which is the property that matters.
func putHuskBack(dest, husk string, logf func(string, ...any)) {
	if err := fsutil.RenameWithRetry(filepath.Join(husk, "backup"), dest, stagingFS.rename); err != nil {
		logf("remote: and could not put the tree at %s back from %s: %v", dest, husk, err)
		return
	}
	reapOwnedEmpty(husk, logf)
}

// stagingStamp reads back the per-directory sequence createSequencedStagingDir
// put in a staging name. The grammar is exact: the prefix, stagingSeqDigits
// ASCII digits, the suffix, nothing else. No released version wrote a stamped
// name at all, so a looser parse buys no migration and costs ownership: v0.8.0
// staged under os.MkdirTemp(parent, ".staging-*"), whose decimal suffix carries
// no second '-', and dot-prefixed link ids were once accepted, so a published
// work tree can sit under any name at all. One that reads as a sequence gets a
// say in the ordering, and one at the maximum stops the allocator for every link
// in the directory.
func stagingStamp(name string) (int64, bool) {
	digits, ok := strings.CutPrefix(name, stagingPrefix)
	if !ok {
		return 0, false
	}
	digits, ok = strings.CutSuffix(digits, stagingSeqSuffix)
	if !ok || len(digits) != stagingSeqDigits {
		return 0, false
	}
	for i := 0; i < len(digits); i++ {
		// ParseInt accepts a leading sign, and a link id may contain '-', so
		// without this a legacy name reads back as a negative sequence the
		// writer could never have emitted.
		if digits[i] < '0' || digits[i] > '9' {
			return 0, false
		}
	}
	stamp, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		// Digits alone can still name a number past int64.
		return 0, false
	}
	return stamp, true
}

// stagingSeqSuffix closes a sequenced staging name, and stagingSeqDigits is the
// width createSequencedStagingDir's %020d writes. The parser requires both
// exactly, and createSequencedStagingDir reads its own name back before using
// it, so a format string that drifted from either would fail there rather than
// putting a name on disk that reads as unstamped: an unstamped entry sorts last
// and is always retained, so the ordering key would simply stop existing with
// nothing failing.
const (
	stagingSeqSuffix = "-seq"
	stagingSeqDigits = 20
)

// stagingSeqAttempts bounds the walk up from a taken number, in the spirit of
// the retry limit os.MkdirTemp applies to its own random names.
const stagingSeqAttempts = 10000

// nextStagingSeq is the number a new staging dir should claim: one past the
// highest already in the directory. That is what makes the order survive a
// clock that moves backward. An extract that reads an existing entry always
// allocates above it, and no clock correction can invert that, whereas the
// wall-clock stamp this replaces inverted whenever the clock did.
//
// Kept backups count. parkKeptBackup derives the parked name from the staging
// name, so a number handed out twice makes the second park land on an occupied
// name; that rename refuses, the backup stays under stagingPrefix, and the next
// pass deletes it as superseded. Counting kept names keeps a parked number out
// of circulation. Recovery still does not enumerate them.
func nextStagingSeq(dir string) (int64, error) {
	entries, err := stagingFS.readDir(dir)
	if err != nil {
		return 0, err
	}
	var high int64
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		// Read only the names this package writes. The sibling directories here
		// are the per-link work trees, and a link id is whatever the uploading
		// client sent; stagingStamp trims its prefix with TrimPrefix, which is a
		// no-op on a name that lacks it, so an unfiltered scan would read a link
		// named "2024-project" as sequence 2024 and one named for int64's
		// maximum as a permanent refusal to allocate anything.
		name := entry.Name()
		switch {
		case strings.HasPrefix(name, keptPrefix):
			name = stagingPrefix + strings.TrimPrefix(name, keptPrefix)
		case strings.HasPrefix(name, stagingPrefix):
		default:
			continue
		}
		stamp, ok := stagingStamp(name)
		if !ok || stamp <= high {
			continue
		}
		// A dir with a .git at its own root is a work tree, not something this
		// package wrote: link ids starting with '.' used to be accepted, so one
		// can carry the exact generated name. The clone and the set-aside tree a
		// live staging dir holds are one level down, in repo/ and backup/, so
		// this vetoes the legacy tree without releasing a number that is still
		// spoken for.
		if _, err := stagingFS.stat(filepath.Join(dir, entry.Name(), ".git")); err == nil {
			continue
		}
		high = stamp
	}
	if high == math.MaxInt64 {
		// The addition below would wrap negative, and %020d of a negative
		// renders a '-' the parser reads as empty digits, so the entry would
		// drop out of the ordering without saying so. Refuse instead.
		return 0, fmt.Errorf("remote: %s holds a staging name at the maximum sequence; remove it before extracting again", dir)
	}
	return high + 1, nil
}

// createSequencedStagingDir claims the first free name from n upward. Exclusive
// creation is what arbitrates: two extracts racing in one directory cannot both
// win a name, and the loser walks up to a number strictly above the winner's.
func createSequencedStagingDir(dir string, n int64) (string, error) {
	if n < 1 {
		return "", fmt.Errorf("remote: refusing to allocate staging sequence %d", n)
	}
	for i := 0; i < stagingSeqAttempts; i++ {
		name := fmt.Sprintf("%s%020d%s", stagingPrefix, n, stagingSeqSuffix)
		// The writer and the reader agree on the format or nothing is written.
		// Checking here rather than trusting the format string is what keeps a
		// silently unstamped name from reaching disk.
		if stamp, ok := stagingStamp(name); !ok || stamp != n {
			return "", fmt.Errorf("remote: staging name %q does not read back as sequence %d", name, n)
		}
		path := filepath.Join(dir, name)
		err := stagingFS.mkdir(path, 0o700)
		if err == nil {
			return path, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return "", err
		}
		if n == math.MaxInt64 {
			return "", fmt.Errorf("remote: staging sequence exhausted in %s", dir)
		}
		n++
	}
	return "", fmt.Errorf("remote: could not claim a staging name in %s after %d attempts", dir, stagingSeqAttempts)
}

// parkKeptBackup moves a staging dir out of the prefix recovery scans, so the
// next pass leaves alone what this one deliberately kept instead of reading the
// restored tree at dest as a later extract publishing over it. A rename onto an
// existing directory fails rather than replacing it, so a legacy link that
// happens to carry the parked name is never clobbered; the copy just stays where
// it is, which is the same fail-safe one pass later.
func parkKeptBackup(dir, staging, id string, logf func(string, ...any)) {
	parked := filepath.Join(dir, keptPrefix+strings.TrimPrefix(filepath.Base(staging), stagingPrefix))
	if err := fsutil.RenameWithRetry(staging, parked, stagingFS.rename); err != nil {
		logf("remote: could not park the kept backup for %s at %s: %v", id, parked, err)
	}
}

// keptStamp reads the sequence out of a Kept backup name. It is the same
// grammar the scanned prefix uses with the other prefix in front, and it is
// deliberately the ONLY prefix this file's operator commands accept: a directory
// under stagingPrefix may be the staging dir of an extract running right now,
// and recovery has a disposition for it either way.
func keptStamp(name string) (int64, bool) {
	rest, ok := strings.CutPrefix(name, keptPrefix)
	if !ok {
		return 0, false
	}
	return stagingStamp(stagingPrefix + rest)
}

// KeptBackup is one copy recovery moved under the Kept prefix rather than
// deleted, as an operator sees it. Recovery never enumerates that prefix, so
// this listing is the only way a retained copy is found again; nothing reclaims
// one on its own. Owned false is a directory carrying the Kept name that nothing
// on disk attributes, listed beside the real backups so recovery's residue is
// visible rather than silent until a disk fills.
type KeptBackup struct {
	Path  string
	Dest  string
	Seq   int64
	Bytes int64
	Owned bool
}

// ListKeptBackups reports every Kept backup in a bundle dir. The destination
// comes off the marker and never off the name: a bundle Kept name carries no
// link id at all, so a name-derived destination would be an invention.
func ListKeptBackups(dir string) ([]KeptBackup, error) {
	entries, err := stagingFS.readDir(dir)
	if err != nil {
		return nil, err
	}
	var out []KeptBackup
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		seq, ok := keptStamp(entry.Name())
		if !ok {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		backup := KeptBackup{Path: path, Seq: seq, Bytes: dirBytes(path)}
		// The same proof recovery requires before it touches a directory: the
		// work-tree veto, then a marker agreeing with the name and naming a link
		// the write path would have accepted.
		if id, ok, _ := attributeStagingDir(dir, path, seq, discardLog); ok {
			backup.Dest = id
			backup.Owned = true
		}
		out = append(out, backup)
	}
	slices.SortFunc(out, func(a, b KeptBackup) int { return cmp.Compare(a.Path, b.Path) })
	return out, nil
}

// RemoveKeptBackup deletes the one Kept backup an operator named. It is not an
// rm: the name has to be a single component under dir, pass the Kept grammar,
// hold no work tree at its root, and carry a marker whose kind, link, and
// sequence all agree, and the link's extract locks have to be free. Anything
// else is refused and left for the operator to remove by hand, because this is
// the only command that deletes a Kept backup and a wrong answer here is the
// last copy of a tree.
func RemoveKeptBackup(dir, name string) (err error) {
	if name == "" || name == "." || name == ".." || name != filepath.Base(name) {
		// Ahead of every filesystem call: a name carrying a separator joins to a
		// path outside dir, which would make this a remote rm.
		return fmt.Errorf("remote: %q is not the name of an entry in %s", name, dir)
	}
	seq, ok := keptStamp(name)
	if !ok {
		return fmt.Errorf("remote: %s is not a kept backup; recovery may still act on it, so remove it by hand", name)
	}
	path := filepath.Join(dir, name)
	id, ok, _ := attributeStagingDir(dir, path, seq, discardLog)
	if !ok {
		return fmt.Errorf("remote: nothing on disk attributes %s to a link; remove it by hand", name)
	}
	dest := filepath.Join(dir, id)
	unlock, held := tryLockExtract(dest)
	if held {
		return fmt.Errorf("remote: an extract in this process holds %s; try again once it finishes", id)
	}
	defer unlock()
	unlockFile, heldFile, err := tryLockExtractFile(dir, dest)
	if err != nil {
		return err
	}
	if heldFile {
		return fmt.Errorf("remote: another process holds %s; try again once it finishes", id)
	}
	// removed is what keeps a release failure from reading as "nothing was
	// removed": this is the only command that deletes a Kept backup, and an
	// operator sent back to it by an ambiguous error finds the entry gone.
	removed := false
	defer func() { err = mergeReleaseFailure(err, unlockFile(), removed, id) }()
	// Re-read under the lock. The attribution above ran with nothing excluding a
	// live extract, and a directory that stopped being this link's in between is
	// one that must not be deleted on the strength of the earlier reading.
	if again, ok, _ := attributeStagingDir(dir, path, seq, discardLog); !ok || again != id {
		return fmt.Errorf("remote: %s is no longer attributable to %s; leaving it in place", name, id)
	}
	if rmErr := stagingFS.removeAll(path); rmErr != nil {
		return rmErr
	}
	removed = true
	return nil
}

// dirBytes sums the regular files under path. Unreadable entries are skipped
// rather than failing the listing: a size an operator cannot see is a worse
// answer than a size that is short, and the entry itself is still reported.
func dirBytes(path string) int64 {
	var total int64
	_ = filepath.WalkDir(path, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		total += info.Size()
		return nil
	})
	return total
}

// discardLog is the reporter the operator commands hand the recovery helpers
// they reuse. Those helpers narrate every refusal for the recovery pass's log;
// here the refusal comes back as an error instead.
func discardLog(string, ...any) {}

// txnMarker is what a staging dir carries to prove which transaction created it.
// Seq repeats the number in the directory's own name: a marker that disagrees
// with its name proves nothing about who wrote either, so the pair is what makes
// the directory owned rather than the name alone.
type txnMarker struct {
	Kind string `json:"kind"`
	Dest string `json:"dest"`
	Seq  int64  `json:"seq"`
}

// errMarkerMissing reports that a staging dir carries no marker at all, which is
// a different fact from a marker that could not be read: the first is a crash
// before the marker was written, the second is a filesystem fault. Deciding
// between them off a bare error would fold a fault into "not ours".
var errMarkerMissing = errors.New("remote: staging dir carries no transaction marker")

// writeMarker publishes m into dir under stagingMarkerFile, through a temp file
// in that same directory so the rename is the only step a reader can observe. A
// plain write can be torn by a crash, and a half-written marker parses as
// nothing while still occupying the name recovery reads to decide ownership.
func writeMarker(dir string, m txnMarker) error {
	payload, err := json.Marshal(m)
	if err != nil {
		return err
	}
	tmp, err := stagingFS.createTemp(dir, stagingMarkerFile+"-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	// Sync before the rename: the rename can reach disk ahead of the bytes it
	// names, which is exactly the partial marker the temp file exists to avoid.
	if _, err := tmp.Write(payload); err != nil {
		_ = tmp.Close()
		_ = stagingFS.removeAll(name)
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = stagingFS.removeAll(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = stagingFS.removeAll(name)
		return err
	}
	if err := fsutil.RenameWithRetry(name, filepath.Join(dir, stagingMarkerFile), stagingFS.rename); err != nil {
		_ = stagingFS.removeAll(name)
		return err
	}
	return nil
}

// readMarker reads back what writeMarker published. A dir with no marker yields
// errMarkerMissing; every other failure is wrapped, so a caller can tell "this
// was never ours" from "this could not be read", which are opposite decisions.
func readMarker(dir string) (txnMarker, error) {
	raw, err := stagingFS.readFile(filepath.Join(dir, stagingMarkerFile))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return txnMarker{}, errMarkerMissing
		}
		return txnMarker{}, fmt.Errorf("read transaction marker in %s: %w", dir, err)
	}
	var m txnMarker
	if err := json.Unmarshal(raw, &m); err != nil {
		return txnMarker{}, fmt.Errorf("parse transaction marker in %s: %w", dir, err)
	}
	return m, nil
}

// extractBundle clones bundleFile into a staging dir beside dest, then swaps the
// clone into place (replacing any prior extraction for this link id). git clone
// needs a non-existent target, hence the staging dir. The steps run in this
// order: allocate the sequenced staging dir, write its transaction marker, clone
// into it, set the live tree aside, publish the clone, record the commit flag,
// remove the staging dir. The marker precedes the first destructive rename and
// the flag follows the publish, so every copy this leaves on disk names the
// transaction that wrote it and says whether that transaction committed. The
// live tree is moved aside rather than deleted and is put back if the publish
// fails, so on every error return dest holds either the prior extraction or the
// new one, never neither. Swapping a directory is two renames and cannot be made
// atomic, so a crash between them leaves dest absent with the prior tree in
// staging/backup, which the marker is what lets recovery attribute and put back.
// logf may be nil.
func extractBundle(ctx context.Context, bundleFile, dest string, logf func(string, ...any)) (err error) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	parent := filepath.Dir(dest)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return err
	}
	unlock, err := waitForExtract(ctx, dest)
	if err != nil {
		return err
	}
	defer unlock()
	unlockFile, err := lockExtractFile(ctx, parent, dest)
	if err != nil {
		return err
	}
	// published is the fact the release failure must not erase. The lock is let
	// go of after the swap, so a bare release error would tell the client the
	// upload did nothing and have it send the same bundle again over the tree
	// this extract just installed.
	published := false
	defer func() { err = mergeReleaseFailure(err, unlockFile(), published, filepath.Base(dest)) }()

	// The sequence records this extract's place in the order, which is what lets
	// recovery tell an older leftover staging dir from a newer one. It is one
	// past the highest number already in the directory, claimed by exclusive
	// creation, so a concurrent extract for another link cannot take the same
	// value and a clock that moves backward cannot invert the order. Every number
	// on disk is one of these per-directory sequences: no released version wrote
	// a stamped name at all (v0.8.0 staged under os.MkdirTemp's random suffix),
	// so nothing here is ordering against a wall clock it inherited.
	seq, err := nextStagingSeq(parent)
	if err != nil {
		return err
	}
	staging, err := createSequencedStagingDir(parent, seq)
	if err != nil {
		return err
	}
	// Staging also holds the prior tree while the swap is in flight, so it is
	// only cleaned up once dest is known to hold one of the two trees.
	cleanupStaging := true
	defer func() {
		if !cleanupStaging {
			return
		}
		// A failure here strands a whole copy of the prior tree under a
		// dot-prefixed dir nothing else enumerates, so say so rather than
		// leaking it silently.
		if err := stagingFS.removeAll(staging); err != nil {
			logf("remote: could not remove bundle staging dir %s: %v", staging, err)
		}
	}()
	// Attribute the staging dir before anything else touches the filesystem: from
	// here on every failure can leave a copy of a tree behind, and a copy no
	// marker names is one recovery can neither put back nor ever reclaim. The
	// name is the authority on the sequence, because createSequencedStagingDir
	// walks up from seq when a name is taken, and a marker that disagrees with
	// its own directory name proves nothing.
	claimed, _ := stagingStamp(filepath.Base(staging))
	if err := writeMarker(staging, txnMarker{Kind: txnKindBundleExtract, Dest: filepath.Base(dest), Seq: claimed}); err != nil {
		return err
	}
	cloneCtx, cancelClone := context.WithTimeout(ctx, gitTimeout)
	defer cancelClone()
	cloneDest := filepath.Join(staging, "repo")
	if err := gitClone(cloneCtx, bundleFile, cloneDest); err != nil {
		return err
	}

	// Every rename stays inside parent, so none of them crosses a filesystem.
	backup := filepath.Join(staging, "backup")
	restore := func() error { return nil }
	if err := fsutil.RenameWithRetry(dest, backup, stagingFS.rename); err == nil {
		restore = func() error { return fsutil.RenameWithRetry(backup, dest, stagingFS.rename) }
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := fsutil.RenameWithRetry(cloneDest, dest, stagingFS.rename); err != nil {
		if restoreErr := restore(); restoreErr != nil {
			// dest is empty and the only copy of the prior tree is the backup,
			// so keep staging rather than deleting the tree on the way out.
			cleanupStaging = false
			return fmt.Errorf("publish extraction: %w (prior tree left in %s: %v)", err, backup, restoreErr)
		}
		return err
	}
	published = true
	// The flag is the only evidence that the copy in backup was published over.
	// Without it that copy has to be kept, so a failure here costs one retained
	// tree and never the publish, which has already landed and is reported as
	// the success it is.
	flag, err := stagingFS.create(filepath.Join(staging, committedFile), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		cleanupStaging = false
		logf("remote: published %s but could not record the commit flag in %s: %v", dest, staging, err)
		return nil
	}
	_ = flag.Close()
	return nil
}

// ---- client side -----------------------------------------------------------

// UploadRepoBundle creates a git bundle of repoDir's full history and uploads it
// to the remote bridge over an authenticated, bundle-mode TLS connection. The
// bridge extracts it into a per-link working tree and returns its path, captured
// in the returned SessionLink. repoDir must be a git work tree.
func UploadRepoBundle(cfg RemoteConfig, repoDir, linkID string) (*SessionLink, error) {
	id, err := sanitizeLinkID(linkID)
	if err != nil {
		return nil, err
	}
	repoDir = strings.TrimSpace(repoDir)
	if repoDir == "" {
		return nil, errors.New("remote: repo dir is required")
	}
	if !isGitWorktree(repoDir) {
		return nil, fmt.Errorf("remote: %s is not a git repository", repoDir)
	}

	// Reserve a unique temp name, then let git create the bundle fresh at it.
	tmp, err := os.CreateTemp("", "zero-bundle-*.bundle")
	if err != nil {
		return nil, fmt.Errorf("remote: stage bundle: %w", err)
	}
	tmpName := tmp.Name()
	_ = tmp.Close()
	_ = os.Remove(tmpName)
	defer func() { _ = os.Remove(tmpName) }()

	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	if err := gitBundleCreate(ctx, repoDir, tmpName); err != nil {
		return nil, fmt.Errorf("remote: create bundle: %w", err)
	}
	sum, size, err := hashFile(tmpName)
	if err != nil {
		return nil, fmt.Errorf("remote: hash bundle: %w", err)
	}

	conn, err := dialAuthenticated(cfg, ModeBundle)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()

	if err := writeBundleHeader(conn, bundleHeader{LinkID: id, Size: size}); err != nil {
		return nil, fmt.Errorf("remote: send bundle header: %w", err)
	}
	if err := streamFileFrames(conn, tmpName); err != nil {
		return nil, fmt.Errorf("remote: send bundle: %w", err)
	}
	res, err := readBundleResult(conn)
	if err != nil {
		return nil, fmt.Errorf("remote: bundle result: %w", err)
	}
	if !res.OK {
		return nil, fmt.Errorf("remote: bundle rejected: %s", res.Message)
	}

	return &SessionLink{
		Address:      strings.TrimSpace(cfg.Address),
		ServerName:   strings.TrimSpace(cfg.ServerName),
		CACertFile:   strings.TrimSpace(cfg.CACertFile),
		LinkID:       id,
		RemotePath:   res.Path,
		BundleSHA256: sum,
	}, nil
}

// streamFileFrames writes the file at path to w as a sequence of KindData frames.
func streamFileFrames(w io.Writer, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	buf := make([]byte, bundleChunkSize)
	for {
		n, err := f.Read(buf)
		if n > 0 {
			if werr := daemon.WriteFrame(w, daemon.KindData, buf[:n]); werr != nil {
				return werr
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// ---- git + path helpers ----------------------------------------------------

func gitBundleCreate(ctx context.Context, repoDir, outFile string) error {
	return runGit(ctx, repoDir, "bundle", "create", outFile, "--all")
}

func gitBundleVerify(ctx context.Context, bundleFile string) error {
	return runGit(ctx, "", "bundle", "verify", bundleFile)
}

func gitClone(ctx context.Context, bundleFile, destDir string) error {
	return runGit(ctx, "", "clone", "--quiet", bundleFile, destDir)
}

// isGitWorktree reports whether dir is inside a git work tree.
func isGitWorktree(dir string) bool {
	cmd := exec.Command("git", "-C", dir, "rev-parse", "--is-inside-work-tree")
	out, err := cmd.CombinedOutput()
	return err == nil && strings.TrimSpace(string(out)) == "true"
}

// runGit runs a git subcommand, returning a concise single-line error on failure.
func runGit(ctx context.Context, dir string, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := firstLine(strings.TrimSpace(string(out)))
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("git %s: %s", args[0], msg)
	}
	return nil
}

// sanitizeLinkID validates a link id used as a single path component under the
// bundle dir. It allows letters, digits, '-', '_', '.', forbids a leading '.',
// and caps the length, so an id can never escape the bundle dir and can never
// name one of the stagingPrefix directories an extract creates beside it.
func sanitizeLinkID(id string) (string, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return "", errors.New("remote: link id is required")
	}
	if len(id) > 128 {
		return "", errors.New("remote: link id too long (max 128)")
	}
	if strings.HasPrefix(id, ".") {
		return "", errors.New("remote: link id may not start with '.'")
	}
	// Windows drops a trailing dot or space when it resolves a path, and maps
	// the reserved device names onto devices rather than directory entries. An
	// id that is not one distinct directory on every platform breaks the thing
	// both extract locks assume: they key on the id, so two ids that resolve to
	// one tree take two different locks and clone into it at the same time.
	if strings.HasSuffix(id, ".") {
		return "", errors.New("remote: link id may not end with '.'")
	}
	if isReservedDeviceName(id) {
		return "", errors.New("remote: link id may not be a reserved device name")
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
		default:
			return "", errors.New("remote: link id may only contain letters, digits, '-', '_', '.'")
		}
	}
	return id, nil
}

// withinDir reports whether target resolves to a path inside root.
func withinDir(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

// hashFile returns the hex SHA-256 and byte size of the file at path.
func hashFile(path string) (sum string, size int64, err error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// isReservedDeviceName reports whether name is one of the Windows device names,
// which resolve to a device on that platform whatever directory they sit in.
// The check is case-insensitive and ignores an extension, because CON, con and
// AUX.txt all resolve the same way.
func isReservedDeviceName(name string) bool {
	if i := strings.IndexByte(name, '.'); i >= 0 {
		name = name[:i]
	}
	switch strings.ToUpper(name) {
	case "CON", "PRN", "AUX", "NUL":
		return true
	}
	if len(name) == 4 && (strings.EqualFold(name[:3], "COM") || strings.EqualFold(name[:3], "LPT")) {
		return name[3] >= '1' && name[3] <= '9'
	}
	return false
}
