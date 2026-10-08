package tools

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// ErrFileChangedOnDisk is returned by CheckConflict when a file's current
// on-disk content no longer matches what a tool last read or wrote in this
// session — i.e. it was modified outside Zero. The write tools surface it with
// guidance to re-read before retrying, so a stale overwrite cannot silently
// clobber an external edit.
var ErrFileChangedOnDisk = errors.New("the file changed on disk since you last read it")

// FileVersion is what a tool last observed for a file: an authoritative content
// hash plus cheap stat metadata kept for diagnostics.
type FileVersion struct {
	Hash  string
	Size  int64
	MTime time.Time
}

// FileTracker records, per absolute path, the version of each file a tool read
// or wrote within a single session. A later write compares the current on-disk
// content against this baseline to detect a modification made outside Zero.
//
// Construct with NewFileTracker. A nil *FileTracker is a valid no-op — every
// method tolerates it — so a caller that has not wired the feature (tests, MCP)
// needs no nil guards.
type FileTracker struct {
	mu       sync.Mutex
	versions map[string]FileVersion
	seen     map[string]fileObservation
	// created preserves the order in which brand-new files (paths that did not
	// exist before a write tool created them) were first written this session.
	// A map would lose that order and complicates de-duplication, so a slice
	// plus a seen-set is used instead.
	created     []string
	createdSeen map[string]bool
}

func NewFileTracker() *FileTracker {
	return &FileTracker{
		versions: make(map[string]FileVersion),
		seen:     make(map[string]fileObservation),
	}
}

type lineRange struct {
	start int
	end   int
}

type fileObservation struct {
	whole bool
	// total is the file's line count as of the last recorded read, so coverage
	// can be judged from the ranges alone. Without it a file read in pieces
	// could never be known to have been seen in full.
	total      int
	ranges     []lineRange
	totalBytes int
	byteRanges []lineRange // zero-based, half-open byte intervals
}

type pendingFileObservation struct {
	path     string
	output   string
	hash     string
	start    int
	end      int
	total    int
	byteMode bool
}

// Record stores the version of absPath given its content and optional stat info.
func (tracker *FileTracker) Record(absPath string, content []byte, info os.FileInfo) {
	tracker.RecordHash(absPath, HashContent(content), info)
}

// RecordHash stores the version of absPath when the caller already computed
// the same HashContent-compatible SHA-256 while reading the file.
func (tracker *FileTracker) RecordHash(absPath string, hash string, info os.FileInfo) {
	if tracker == nil {
		return
	}
	version := FileVersion{Hash: hash}
	if info != nil {
		version.Size = info.Size()
		version.MTime = info.ModTime()
	}
	tracker.mu.Lock()
	if previous, ok := tracker.versions[absPath]; !ok || previous.Hash != hash {
		delete(tracker.seen, absPath)
	}
	tracker.versions[absPath] = version
	tracker.mu.Unlock()
}

// RecordSeenRange records exact, non-elided source lines returned to the model.
// Ranges accumulate while the content hash remains unchanged.
func (tracker *FileTracker) RecordSeenRange(absPath string, start, end, total int) {
	if tracker == nil || start < 1 {
		return
	}
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	observation := tracker.seen[absPath]
	if total == 0 {
		observation.whole = true
		observation.ranges = nil
		tracker.seen[absPath] = observation
		return
	}
	if end < start {
		return
	}
	// A new line count means the file changed shape since the last read, so
	// previously recorded ranges no longer describe it.
	if observation.total != 0 && observation.total != total {
		observation = fileObservation{}
	}
	observation.total = total
	if start == 1 && end >= total {
		observation.whole = true
		observation.ranges = nil
		tracker.seen[absPath] = observation
		return
	}
	observation.ranges = append(observation.ranges, lineRange{start: start, end: end})
	tracker.seen[absPath] = observation
}

// RecordSeenBytes records an exact, non-elided byte interval returned to the
// model. Byte intervals make oversized single-line files recoverable without
// crediting the model for the omitted middle of a truncated line read.
func (tracker *FileTracker) RecordSeenBytes(absPath string, start, end, total int) {
	if tracker == nil || start < 0 || end < start || total < 0 || end > total {
		return
	}
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	observation := tracker.seen[absPath]
	if total == 0 {
		observation.whole = true
		observation.byteRanges = nil
		tracker.seen[absPath] = observation
		return
	}
	if observation.totalBytes != 0 && observation.totalBytes != total {
		observation = fileObservation{}
	}
	observation.totalBytes = total
	if start == 0 && end >= total {
		observation.whole = true
		observation.byteRanges = nil
		tracker.seen[absPath] = observation
		return
	}
	observation.byteRanges = append(observation.byteRanges, lineRange{start: start, end: end})
	tracker.seen[absPath] = observation
}

// RecordEdit re-baselines absPath after an edit THIS SESSION made, keeping the
// reads the edit did not disturb.
//
// WHY THIS EXISTS RATHER THAN Record. RecordHash drops every recorded range when
// the content hash moves, which is right for a change we did not make: we cannot
// say which lines still hold what was read. After our own edit we can say
// exactly. The content before the first changed line is byte-identical and sits
// at the same line numbers; the content after the last changed line is
// byte-identical and has moved by a known delta. Only the lines the edit
// actually spans stop describing the file.
//
// Dropping the lot instead cost a real run. A 371-line file was read in three
// pieces (40-45, 85-260, 260-371) and one two-line edit at line 92 erased the
// credit for all three: the next six edits — into regions that had been read,
// that the edit did not touch, in a file whose line count had not changed — were
// each refused as content "not read in this session", and the repeated-failure
// guard halted the run. The error even told the model to re-read, which would
// have been undone by its next successful edit. The guard is right that a model
// must not edit what it has not seen; it was wrong about what it had seen.
func (tracker *FileTracker) RecordEdit(absPath string, before, after []byte, info os.FileInfo) {
	if tracker == nil {
		return
	}
	firstLine, lastLineBefore, lineDelta := changedLineSpan(string(before), string(after))
	firstByte, lastByteBefore, byteDelta := changedByteSpan(before, after)

	tracker.mu.Lock()
	defer tracker.mu.Unlock()

	version := FileVersion{Hash: HashContent(after)}
	if info != nil {
		version.Size = info.Size()
		version.MTime = info.ModTime()
	}
	tracker.versions[absPath] = version

	observation, tracked := tracker.seen[absPath]
	if !tracked {
		return
	}
	// The DERIVED answer, not the raw flag. Branching on observation.whole here
	// sent a file read whole in two chunks — seenWhole true, flag false — into
	// the split path below, where it stopped being seen whole after one edit.
	if seenWhole(observation) {
		// Still whole: every line was read, and an edit of ours does not make
		// that untrue. Re-baseline as a single covering observation so the
		// answer survives regardless of how the ranges shifted.
		observation.whole = true
		observation.ranges = nil
		observation.byteRanges = nil
		observation.total = countLines(after)
		observation.totalBytes = len(after)
		tracker.seen[absPath] = observation
		return
	}

	// SPLIT AROUND THE EDIT, never drop the whole range. A read almost always
	// SPANS the line it is about to edit — that is why it was read — so dropping
	// on overlap would have thrown away 85-260 to change line 92 and left the
	// original defect in place under a longer implementation.
	kept := make([]lineRange, 0, len(observation.ranges)+1)
	for _, seen := range observation.ranges {
		if end := min(seen.end, firstLine-1); seen.start <= end {
			kept = append(kept, lineRange{start: seen.start, end: end})
		}
		if start := max(seen.start, lastLineBefore+1); start <= seen.end {
			kept = append(kept, lineRange{start: start + lineDelta, end: seen.end + lineDelta})
		}
	}
	observation.ranges = kept
	if observation.total != 0 {
		observation.total = countLines(after)
	}

	// Same split, on half-open byte intervals.
	keptBytes := make([]lineRange, 0, len(observation.byteRanges)+1)
	for _, seen := range observation.byteRanges {
		if end := min(seen.end, firstByte); seen.start < end {
			keptBytes = append(keptBytes, lineRange{start: seen.start, end: end})
		}
		if start := max(seen.start, lastByteBefore); start < seen.end {
			keptBytes = append(keptBytes, lineRange{start: start + byteDelta, end: seen.end + byteDelta})
		}
	}
	observation.byteRanges = keptBytes
	if observation.totalBytes != 0 {
		observation.totalBytes = len(after)
	}
	tracker.seen[absPath] = observation
}

// changedLineSpan reports the 1-based first line that differs between before and
// after, the 1-based last line of BEFORE that differs, and the line-count delta.
//
// Computed from a common prefix and suffix rather than from the caller's
// replacement spans: one edit_file call with replace_all can rewrite many
// scattered occurrences, and the span between the outermost two is the only
// region that is honestly unknown afterwards.
// The span is derived by scanning BYTES, not by materialising lines.
//
// The first version split both versions into []string. That allocates one string
// header per line for each version — work that scales with the SIZE OF THE FILE
// rather than the size of the edit, and it ran before RecordEdit had even checked
// whether there was a tracked observation to update. @jatmn measured 32,036,640
// bytes for two 2 MB versions of a million short lines, and extrapolated roughly
// 1.6 GB of headers for a 100 MB file before counting the content and hashes
// already live alongside them.
//
// That mattered beyond memory pressure: edit_file writes the updated bytes BEFORE
// calling RecordEdit, so an out-of-memory kill lands after the user's file has
// changed and before the tracker baseline catches up — the file and the record of
// it disagree, and nothing says so.
//
// Bytes give the same answer in bounded memory. Two identical byte prefixes share
// every line that ends inside them, so counting newlines in the common prefix
// counts the unchanged leading lines directly.
func changedLineSpan(before, after string) (firstChanged, lastChangedBefore, delta int) {
	beforeCount := countTrackedLines(before)
	afterCount := countTrackedLines(after)

	prefix := commonLinePrefix(before, after)
	// Never more leading lines than the shorter version has. The byte scan can
	// otherwise claim a line that only one side possesses — two empty versions
	// share a whole "line" that neither actually contains.
	prefix = min(prefix, min(beforeCount, afterCount))
	// The suffix may not reach back past the lines the prefix already claimed,
	// exactly as the line-array loop bounded itself.
	suffix := commonLineSuffix(before, after, min(beforeCount-prefix, afterCount-prefix))
	return prefix + 1, beforeCount - suffix, afterCount - beforeCount
}

// commonLinePrefix counts the leading lines that are byte-identical in both.
func commonLinePrefix(before, after string) int {
	limit := min(len(before), len(after))
	at := 0
	for at < limit && before[at] == after[at] {
		at++
	}
	lines := strings.Count(before[:at], "\n")
	// A LINE THAT ENDS EXACTLY WHERE THE SCAN STOPPED IS STILL SHARED. Scanning
	// halts where the bytes diverge OR where the shorter version runs out, and in
	// the second case the line containing that point can be complete and equal on
	// both sides — "a\nb" against "a\nb\nc" diverges only because the first ended,
	// and "b" is a whole matching line in each.
	if lineBoundary(before, at) && lineBoundary(after, at) {
		lines++
	}
	return lines
}

// commonLineSuffix counts the trailing lines that are byte-identical in both, up
// to the ceiling the prefix leaves.
func commonLineSuffix(before, after string, ceiling int) int {
	if ceiling <= 0 {
		return 0
	}
	limit := min(len(before), len(after))
	back := 0
	for back < limit && before[len(before)-1-back] == after[len(after)-1-back] {
		back++
	}
	beforeAt, afterAt := len(before)-back, len(after)-back
	// Each newline inside the shared tail closes a line that lies wholly within
	// it. The line the tail STARTS in is shared only when the tail begins on a
	// line boundary in both versions — otherwise its opening bytes differ and it
	// is a changed line that merely ends the same way.
	lines := strings.Count(before[beforeAt:], "\n")
	if lineStart(before, beforeAt) && lineStart(after, afterAt) {
		lines++
	}
	return min(lines, ceiling)
}

// lineBoundary reports whether at is the end of a line: the end of the text, or
// the position of the newline that closes it.
func lineBoundary(text string, at int) bool { return at == len(text) || text[at] == '\n' }

// lineStart reports whether at begins a line.
func lineStart(text string, at int) bool { return at == 0 || text[at-1] == '\n' }

// changedByteSpan is changedLineSpan in bytes: the first differing offset, the
// end offset of the changed region in BEFORE, and the size delta.
func changedByteSpan(before, after []byte) (firstChanged, lastChangedBefore, delta int) {
	prefix := 0
	for prefix < len(before) && prefix < len(after) && before[prefix] == after[prefix] {
		prefix++
	}
	suffix := 0
	for suffix < len(before)-prefix && suffix < len(after)-prefix &&
		before[len(before)-1-suffix] == after[len(after)-1-suffix] {
		suffix++
	}
	return prefix, len(before) - suffix, len(after) - len(before)
}

// countLines reports the line count a READER would give the content, which is
// what observation.total is compared against.
//
// The trailing newline does not open a line. strings.Split leaves an empty final
// element for "a\nb\nc\n" and so counted 4 where a read reports 3 — and because
// SeenWhole asks whether the ranges cover 1..total, an inflated total made full
// coverage unreachable for the very file that had just been read in full. Almost
// every text file ends in a newline, so this was the common case rather than an
// edge one.
//
// countTrackedLines keeps the OTHER rule deliberately: the span arithmetic has to
// match the indices strings.Split produced, so it counts the empty element after
// a trailing newline where this one does not. Two rules, two names, both stated —
// they were one function with an if, and that is how they were confused.
func countLines(content []byte) int {
	// Counted, not split. This only ever needed a number, and building a slice of
	// every line to take its length was the third full per-line allocation in the
	// same call path.
	if len(content) == 0 {
		return 0
	}
	lines := bytes.Count(content, []byte{'\n'})
	if content[len(content)-1] != '\n' {
		lines++
	}
	return lines
}

// countTrackedLines is countLines' rule for the string form: the number of
// elements strings.Split would produce, which counts the empty element after a
// trailing newline. The two differ deliberately — observation.total compares
// against what a READER reports, while the span arithmetic has to match the
// indices the split produced.
func countTrackedLines(text string) int {
	if text == "" {
		return 0
	}
	return strings.Count(text, "\n") + 1
}

// coversFully reports whether ranges together cover every line in [start, end].
//
// Ranges are merged rather than scanned line by line: a caller asking about a
// large file would otherwise walk every line against every recorded range, and
// the answer is the same.
func coversFully(ranges []lineRange, start int, end int) bool {
	if start > end {
		return true
	}
	if len(ranges) == 0 {
		return false
	}
	sorted := make([]lineRange, len(ranges))
	copy(sorted, ranges)
	sort.Slice(sorted, func(left int, right int) bool {
		if sorted[left].start != sorted[right].start {
			return sorted[left].start < sorted[right].start
		}
		return sorted[left].end < sorted[right].end
	})
	next := start
	for _, seen := range sorted {
		if seen.start > next {
			return false
		}
		if seen.end >= next {
			next = seen.end + 1
		}
		if next > end {
			return true
		}
	}
	return next > end
}

// coversBytes reports whether half-open byte ranges cover [start, end).
func coversBytes(ranges []lineRange, start, end int) bool {
	if start >= end {
		return true
	}
	if len(ranges) == 0 {
		return false
	}
	sorted := make([]lineRange, len(ranges))
	copy(sorted, ranges)
	sort.Slice(sorted, func(left, right int) bool {
		if sorted[left].start != sorted[right].start {
			return sorted[left].start < sorted[right].start
		}
		return sorted[left].end < sorted[right].end
	})
	next := start
	for _, seen := range sorted {
		if seen.start > next {
			return false
		}
		if seen.end > next {
			next = seen.end
		}
		if next >= end {
			return true
		}
	}
	return false
}

// SeenRange reports whether every line in the requested range was returned
// exactly to the model for the currently tracked version.
func (tracker *FileTracker) SeenRange(absPath string, start, end int) bool {
	if tracker == nil {
		return true
	}
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	observation, ok := tracker.seen[absPath]
	if !ok {
		return false
	}
	if observation.whole {
		return true
	}
	return coversFully(observation.ranges, start, end)
}

// SeenBytes reports whether every byte in [start, end) was returned exactly to
// the model for the currently tracked version.
func (tracker *FileTracker) SeenBytes(absPath string, start, end int) bool {
	if tracker == nil {
		return true
	}
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	observation, ok := tracker.seen[absPath]
	if !ok {
		return false
	}
	return observation.whole || coversBytes(observation.byteRanges, start, end)
}

func (tracker *FileTracker) SeenWhole(absPath string) bool {
	if tracker == nil {
		return true
	}
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	observation, ok := tracker.seen[absPath]
	if !ok {
		return false
	}
	return seenWhole(observation)
}

// seenWhole is the DERIVED answer to "has every line been seen", and the single
// place that decides it. Callers must hold the lock.
//
// Derived from the ranges rather than read off the flag, because the flag is
// only ever set by a SINGLE read covering the file: a file read in two halves
// stayed "not seen whole" forever even though SeenRange agreed every line had
// been seen, and write_file, which gates on this, refused the overwrite with
// advice to read the file that could not change the answer.
//
// IT HAS TO BE ONE FUNCTION. RecordEdit branched on the raw flag while this
// derived the answer, so the very case the derivation exists to rescue fell into
// the wrong arm and lost: a file read whole in two chunks reported SeenWhole
// true, then reported false after a single edit, putting the write_file refusal
// back within one edit of where it was fixed.
func seenWhole(observation fileObservation) bool {
	if observation.whole {
		return true
	}
	if observation.total > 0 && coversFully(observation.ranges, 1, observation.total) {
		return true
	}
	return observation.totalBytes > 0 && coversBytes(observation.byteRanges, 0, observation.totalBytes)
}

// Version returns the recorded version for absPath and whether one exists.
func (tracker *FileTracker) Version(absPath string) (FileVersion, bool) {
	if tracker == nil {
		return FileVersion{}, false
	}
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	version, ok := tracker.versions[absPath]
	return version, ok
}

// RecordCreated notes that absPath is a brand-new file a tool just created in
// this session (as opposed to an overwrite of something that already existed).
func (tracker *FileTracker) RecordCreated(absPath string) {
	if tracker == nil {
		return
	}
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	if tracker.createdSeen == nil {
		tracker.createdSeen = make(map[string]bool)
	}
	if tracker.createdSeen[absPath] {
		return
	}
	tracker.createdSeen[absPath] = true
	tracker.created = append(tracker.created, absPath)
}

// CreatedFiles returns the absolute paths of every brand-new file recorded via
// RecordCreated during this session, in first-created order. The caller owns
// the returned slice.
func (tracker *FileTracker) CreatedFiles() []string {
	if tracker == nil {
		return nil
	}
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	out := make([]string, len(tracker.created))
	copy(out, tracker.created)
	return out
}

// Forget drops any recorded version for absPath, so the next read re-establishes
// the baseline. Used after a tool rewrites a file by a path other than its own
// (e.g. apply_patch) where the new content is not cheaply available to Record.
func (tracker *FileTracker) Forget(absPath string) {
	if tracker == nil {
		return
	}
	tracker.mu.Lock()
	delete(tracker.versions, absPath)
	delete(tracker.seen, absPath)
	tracker.mu.Unlock()
}

// CheckConflict reports ErrFileChangedOnDisk when absPath has a recorded baseline
// and currentContent (the bytes the caller just read from disk) no longer matches
// it. An untracked path returns nil: with no baseline there is nothing to
// conflict against, so a first-touch write is always allowed.
func (tracker *FileTracker) CheckConflict(absPath string, currentContent []byte) error {
	if tracker == nil {
		return nil
	}
	version, ok := tracker.Version(absPath)
	if !ok {
		return nil
	}
	if HashContent(currentContent) != version.Hash {
		return ErrFileChangedOnDisk
	}
	return nil
}

// HashContent returns the hex-encoded SHA-256 of content. Exported so the write
// tools and tests share one definition of a file's identity.
func HashContent(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

// fileConflictMessage is the actionable error a write tool returns when a target
// changed on disk since it was last read, telling the model how to recover.
func fileConflictMessage(relativePath string) string {
	return "Error writing " + relativePath + ": " + ErrFileChangedOnDisk.Error() +
		" (it may have been edited outside Zero). Re-read it with read_file, then re-apply your change so you do not overwrite the newer content."
}

func fileUnseenMessage(relativePath string) string {
	return "Error writing " + relativePath + ": the intended change depends on content that has not been read exactly in this session. Read the affected lines with read_file using offset/limit (or byte_offset/byte_limit for a very long line), then retry the edit."
}
