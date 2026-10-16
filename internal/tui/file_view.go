// file_view.go is the drill-in view for a touched file, opened from the FILES
// sidebar section (files_panel.go). It reuses the subchat pattern: while
// active, the chat column's body swaps to this file's content — the sidebar,
// composer, and scroll engine keep working unchanged (transcriptBodyItems is
// the single source the viewport, renderer, and hit-testers all read, so
// swapping there keeps every consumer consistent). Two modes:
//
//	diff (default) — the file's edit cards from this session, full-depth
//	full           — the file as it stands on disk, syntax highlighted, with
//	                 gutter markers on the lines this session's diffs added
//
// d/f switch modes (with an empty composer), Esc returns to the chat at the
// scroll position it was left at.
package tui

import (
	"bufio"
	"container/list"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	tea "charm.land/bubbletea/v2"
)

// fileViewMaxLines caps the full-file mode so a giant generated file can't
// freeze a render; the tail collapses into a "… more lines (file truncated at N for display)" trailer.
const (
	fileViewMaxLines               = 4000
	fileViewMaxBytes               = 1 << 20 // 1 MiB total read budget per file
	fileViewMaxLineBytes           = 4096    // 4 KiB max line length budget
	defaultFileViewCacheMaxEntries = 64
	// defaultFileViewCacheMaxBytes bounds the total retained bytes of the file
	// view cache so wide renders of large files cannot grow without limit
	// (measured at tens to hundreds of MiB before this cap). It counts the stable
	// source+display payload plus every rendered variant.
	defaultFileViewCacheMaxBytes = 8 << 20 // 8 MiB
	fileViewMaxRenderVariants    = 4       // max rendered variants (width/fingerprint) per cached file entry
	fileViewMaxPathRevisions     = 1024    // hard cap on retained per-path revision counters
	fileViewMinPathRevisions     = 256     // floor for the revision bound on small caches
	fileViewLoadingPlaceholder   = "Loading…"
)

const (
	fileViewDiff = iota
	fileViewFull
)

var (
	fileViewLifetimeTS  atomic.Uint64
	fileViewLifetimeSeq atomic.Uint32
)

// nextFileViewLifetimeToken produces a monotonic, time-ordered 128-bit UUIDv7 (RFC 9562)
// to uniquely identify the lifecycle of a file view session without heap allocations.
func nextFileViewLifetimeToken() [16]byte {
	nowMs := uint64(time.Now().UnixMilli())
	for {
		last := fileViewLifetimeTS.Load()
		if nowMs > last {
			if fileViewLifetimeTS.CompareAndSwap(last, nowMs) {
				fileViewLifetimeSeq.Store(0)
				break
			}
		} else {
			nowMs = last
			break
		}
	}
	seq := fileViewLifetimeSeq.Add(1)
	var u [16]byte
	u[0] = byte(nowMs >> 40)
	u[1] = byte(nowMs >> 32)
	u[2] = byte(nowMs >> 24)
	u[3] = byte(nowMs >> 16)
	u[4] = byte(nowMs >> 8)
	u[5] = byte(nowMs)
	u[6] = 0x70 | byte((seq>>8)&0x0F) // Version 7
	u[7] = byte(seq & 0xFF)
	u[8] = 0x80 | byte((seq>>16)&0x3F) // RFC 9562 variant
	u[9] = byte(seq >> 24)
	return u
}

// fileViewCacheStats tracks disk I/O, Chroma highlighting, and cache hits/misses.
type fileViewCacheStats struct {
	DiskReads        int
	HighlightCalls   int
	CacheHits        int
	CacheMisses      int
	Evictions        int
	ThemeClears      int
	RenderEvictions  int
	SkippedOversized int
}

type fileViewCachedEntry struct {
	targetPath   string
	displayPath  string
	modTime      time.Time
	size         int64
	sourceRev    uint64
	sourceHash   [sha256.Size]byte
	sourceBytes  int
	lines        []string
	display      []string
	truncated    bool
	omittedLines bool

	rendersMu  sync.RWMutex
	renderKeys []string          // LRU order: oldest at index 0, most recent at end
	renders    map[string]string // key: "width:changedLinesFingerprint" -> formatted ANSI string
}

// byteSize is the total retained payload for the entry: the stable source and
// display lines plus every rendered variant. The reader uses it to enforce the
// cache-wide byte budget.
func (e *fileViewCachedEntry) byteSize() int {
	n := e.sourceBytes
	e.rendersMu.RLock()
	for k, v := range e.renders {
		n += len(k) + len(v)
	}
	e.rendersMu.RUnlock()
	return n
}

// fileViewSourceHash fingerprints the bounded source lines so a metadata-only
// change (cp -p, rsync -t, tar -xp restoring mtime and size) cannot serve a
// stale entry. Lines are joined with a separator the reader never emits inside
// a line, so shifted boundaries cannot collide.
func fileViewSourceHash(lines []string) [sha256.Size]byte {
	h := sha256.New()
	for i, line := range lines {
		if i > 0 {
			h.Write([]byte{'\n'})
		}
		h.Write([]byte(line))
	}
	var out [sha256.Size]byte
	copy(out[:], h.Sum(nil))
	return out
}

func (e *fileViewCachedEntry) getRender(key string) (string, bool) {
	e.rendersMu.RLock()
	val, ok := e.renders[key]
	e.rendersMu.RUnlock()
	if !ok {
		return "", false
	}
	e.rendersMu.Lock()
	for i, k := range e.renderKeys {
		if k == key {
			e.renderKeys = append(append(e.renderKeys[:i], e.renderKeys[i+1:]...), key)
			break
		}
	}
	e.rendersMu.Unlock()
	return val, true
}

// putRender stores a formatted variant and returns the change in retained
// bytes (positive on growth, negative when an old variant is dropped or
// replaced). The caller folds the delta into the cache-wide budget.
func (e *fileViewCachedEntry) putRender(key string, val string) int {
	e.rendersMu.Lock()
	defer e.rendersMu.Unlock()
	if e.renders == nil {
		e.renders = make(map[string]string)
	}
	delta := 0
	if prev, ok := e.renders[key]; ok {
		delta = len(val) - len(prev)
	} else {
		for len(e.renders) >= fileViewMaxRenderVariants {
			if len(e.renderKeys) > 0 {
				oldKey := e.renderKeys[0]
				e.renderKeys = e.renderKeys[1:]
				delta -= len(oldKey) + len(e.renders[oldKey])
				delete(e.renders, oldKey)
			} else {
				for k, v := range e.renders {
					delta -= len(k) + len(v)
					delete(e.renders, k)
					break
				}
			}
		}
		e.renderKeys = append(e.renderKeys, key)
		delta += len(key) + len(val)
	}
	e.renders[key] = val
	return delta
}

type fileViewRenderCache struct {
	mu            sync.Mutex
	maxEntries    int
	maxBytes      int
	retained      int
	items         map[string]*list.Element // targetPath -> *list.Element containing *fileViewCachedEntry
	lru           *list.List
	gen           int
	pathRevisions map[string]uint64
	revOrder      *list.List               // LRU of revision keys: front = most recently invalidated path
	revElems      map[string]*list.Element // targetPath -> *list.Element containing the path string
	revEpochFloor uint64                   // minimum required revision for any path absent from pathRevisions
	statsData     fileViewCacheStats
}

var defaultFileViewCache = newFileViewRenderCache(defaultFileViewCacheMaxEntries, defaultFileViewCacheMaxBytes)

func newFileViewRenderCache(maxEntries int, maxBytes int) *fileViewRenderCache {
	return &fileViewRenderCache{
		maxEntries:    maxEntries,
		maxBytes:      maxBytes,
		items:         make(map[string]*list.Element),
		lru:           list.New(),
		pathRevisions: make(map[string]uint64),
		revOrder:      list.New(),
		revElems:      make(map[string]*list.Element),
	}
}

func (c *fileViewRenderCache) invalidatePath(targetPath string) uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	rev := c.pathRevisions[targetPath]
	if rev < c.revEpochFloor {
		rev = c.revEpochFloor
	}
	rev++
	c.pathRevisions[targetPath] = rev
	c.touchRevisionLocked(targetPath)
	c.trimRevisionsLocked()
	c.removeItemLocked(targetPath)
	return rev
}

func (c *fileViewRenderCache) requiredRevision(targetPath string) uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.requiredRevisionLocked(targetPath)
}

// requiredRevisionLocked returns targetPath's revision, or the epoch floor when
// its metadata has been evicted. Any request older than an evicted revision is
// therefore rejected without needing the dead key to stay resident.
func (c *fileViewRenderCache) requiredRevisionLocked(targetPath string) uint64 {
	if rev, ok := c.pathRevisions[targetPath]; ok {
		return rev
	}
	return c.revEpochFloor
}

// maxPathRevisions bounds how many per-path revision counters are retained. It
// scales with the entry budget but never drops below a small floor and never
// grows past a hard cap.
func (c *fileViewRenderCache) maxPathRevisions() int {
	limit := c.maxEntries * 4
	if limit < fileViewMinPathRevisions {
		limit = fileViewMinPathRevisions
	}
	if limit > fileViewMaxPathRevisions {
		limit = fileViewMaxPathRevisions
	}
	return limit
}

// touchRevisionLocked moves targetPath to the front of the revision LRU,
// inserting it when new.
func (c *fileViewRenderCache) touchRevisionLocked(targetPath string) {
	if c.revOrder == nil {
		c.revOrder = list.New()
	}
	if c.revElems == nil {
		c.revElems = make(map[string]*list.Element)
	}
	if elem, ok := c.revElems[targetPath]; ok {
		c.revOrder.MoveToFront(elem)
		return
	}
	c.revElems[targetPath] = c.revOrder.PushFront(targetPath)
}

// dropRevisionLocked removes a path's revision metadata and advances the epoch
// floor so requests captured before the removal cannot commit.
func (c *fileViewRenderCache) dropRevisionLocked(targetPath string) {
	rev, ok := c.pathRevisions[targetPath]
	if !ok {
		return
	}
	delete(c.pathRevisions, targetPath)
	if c.revElems != nil {
		if elem, ok := c.revElems[targetPath]; ok {
			c.revOrder.Remove(elem)
			delete(c.revElems, targetPath)
		}
	}
	if rev > c.revEpochFloor {
		c.revEpochFloor = rev
	}
}

// trimRevisionsLocked evicts the oldest revision keys once the map exceeds the
// bound, advancing the epoch floor as it goes.
func (c *fileViewRenderCache) trimRevisionsLocked() {
	limit := c.maxPathRevisions()
	for len(c.pathRevisions) > limit {
		back := c.revOrder.Back()
		if back == nil {
			return
		}
		c.dropRevisionLocked(back.Value.(string))
	}
}

// removeItemLocked drops targetPath from the LRU and folds its bytes back out
// of retained. It intentionally leaves pathRevisions alone: an invalidated path
// stays dirty until its entry is evicted by capacity or the cache is purged.
func (c *fileViewRenderCache) removeItemLocked(targetPath string) {
	elem, ok := c.items[targetPath]
	if !ok {
		return
	}
	c.retained -= elem.Value.(*fileViewCachedEntry).byteSize()
	c.lru.Remove(elem)
	delete(c.items, targetPath)
}

// evictOverflowLocked trims the LRU until both the entry count and the retained
// byte budget fit. Evicting an entry also drops its path revision: the path is no
// longer cached, so keeping the revision would only leak one key per file.
func (c *fileViewRenderCache) evictOverflowLocked() {
	for len(c.items) > c.maxEntries || (c.maxBytes > 0 && c.retained > c.maxBytes) {
		back := c.lru.Back()
		if back == nil {
			c.retained = 0
			return
		}
		backEntry := back.Value.(*fileViewCachedEntry)
		c.retained -= backEntry.byteSize()
		delete(c.items, backEntry.targetPath)
		c.lru.Remove(back)
		c.dropRevisionLocked(backEntry.targetPath)
		c.statsData.Evictions++
	}
}

// commitRenderPut applies a variant's byte delta and re-trims the cache. The
// entry is only accounted for while it is still the resident cache value for
// its path: a concurrent purge, eviction, or replacement must not leak bytes
// into c.retained from a detached entry.
func (c *fileViewRenderCache) commitRenderPut(entry *fileViewCachedEntry, key string, val string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	elem, ok := c.items[entry.targetPath]
	if !ok || elem.Value.(*fileViewCachedEntry) != entry {
		return
	}
	delta := entry.putRender(key, val)
	c.retained += delta
	c.evictOverflowLocked()
}

func (c *fileViewRenderCache) evictPath(targetPath string) {
	c.mu.Lock()
	c.removeItemLocked(targetPath)
	c.mu.Unlock()
}

func (c *fileViewRenderCache) purgeLocked() {
	c.gen++
	c.items = make(map[string]*list.Element)
	c.lru.Init()
	c.retained = 0
	c.pathRevisions = make(map[string]uint64)
	c.revOrder = list.New()
	c.revElems = make(map[string]*list.Element)
	c.revEpochFloor = 0
}

func (c *fileViewRenderCache) clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.purgeLocked()
	c.statsData.ThemeClears++
}

func (c *fileViewRenderCache) invalidateUnknownScope() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.purgeLocked()
}

func (c *fileViewRenderCache) generation() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.gen
}

func (c *fileViewRenderCache) resetStats() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.statsData = fileViewCacheStats{}
}

// recordHit notes a snapshot served entirely from resident state. The accepted
// snapshot bypasses the LRU lookup, so it accounts for its own hit to keep the
// no-I/O contract observable in the cache statistics.
func (c *fileViewRenderCache) recordHit() {
	c.mu.Lock()
	c.statsData.CacheHits++
	c.mu.Unlock()
}

func (c *fileViewRenderCache) stats() fileViewCacheStats {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.statsData
}

type fileViewReadResult struct {
	lines        []string
	modTime      time.Time
	size         int64
	truncated    bool
	omittedLines bool
	err          error
}

type fileViewByteCounter struct {
	r io.Reader
	n int
}

func (c *fileViewByteCounter) Read(p []byte) (int, error) {
	got, err := c.r.Read(p)
	c.n += got
	return got, err
}

func (c *fileViewByteCounter) delivered(buf *bufio.Reader) int {
	n := c.n - buf.Buffered()
	if n < 0 {
		return 0
	}
	return n
}

func stripFileViewLineEnding(b []byte) []byte {
	if n := len(b); n > 0 && b[n-1] == '\n' {
		b = b[:n-1]
	}
	if n := len(b); n > 0 && b[n-1] == '\r' {
		b = b[:n-1]
	}
	return b
}

func readFileViewBounded(path string, maxLines int, maxLineBytes int, maxTotalBytes int) fileViewReadResult {
	return readFileViewBoundedCancellable(path, maxLines, maxLineBytes, maxTotalBytes, nil, 0)
}

func readFileViewBoundedCancellable(path string, maxLines int, maxLineBytes int, maxTotalBytes int, liveSeq *atomic.Uint64, seq uint64) fileViewReadResult {
	if fileViewSuperseded(liveSeq, seq) {
		return fileViewReadResult{err: errFileViewSuperseded}
	}

	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return fileViewReadResult{err: err}
	}
	defer file.Close()

	stat, err := file.Stat()
	if err != nil {
		return fileViewReadResult{err: err}
	}
	if !stat.Mode().IsRegular() {
		return fileViewReadResult{err: fmt.Errorf("cannot read non-regular file (mode %s)", stat.Mode().String())}
	}
	if fileViewSuperseded(liveSeq, seq) {
		return fileViewReadResult{err: errFileViewSuperseded}
	}

	var lines []string
	truncated := false
	omittedLines := false
	counter := &fileViewByteCounter{r: io.LimitReader(file, int64(maxTotalBytes)+1)}
	reader := bufio.NewReader(counter)

	moreSource := func() bool {
		if reader.Buffered() > 0 {
			return true
		}
		_, err := reader.Peek(1)
		return err == nil
	}

	for len(lines) < maxLines {
		if fileViewSuperseded(liveSeq, seq) {
			return fileViewReadResult{err: errFileViewSuperseded}
		}
		if fileViewInsideDiskRead != nil {
			fileViewInsideDiskRead()
			if fileViewSuperseded(liveSeq, seq) {
				return fileViewReadResult{err: errFileViewSuperseded}
			}
		}
		start := counter.delivered(reader)
		if start >= maxTotalBytes {
			if moreSource() {
				truncated = true
				omittedLines = true
			}
			break
		}

		var raw []byte
		for {
			if fileViewSuperseded(liveSeq, seq) {
				return fileViewReadResult{err: errFileViewSuperseded}
			}
			frag, err := reader.ReadSlice('\n')
			raw = append(raw, frag...)
			if errors.Is(err, bufio.ErrBufferFull) {
				continue
			}
			if err != nil && !errors.Is(err, io.EOF) {
				if len(lines) == 0 && len(raw) == 0 {
					return fileViewReadResult{err: err}
				}
				truncated = true
				omittedLines = true
				break
			}
			break
		}

		if len(raw) == 0 {
			break
		}

		consumed := counter.delivered(reader)
		budget := maxTotalBytes - start
		if budget < 0 {
			budget = 0
		}
		kept := raw
		if len(kept) > budget {
			kept = kept[:budget]
			truncated = true
			omittedLines = true
		}
		display := stripFileViewLineEnding(kept)
		lineTruncated := false
		if len(display) > maxLineBytes {
			display = display[:maxLineBytes]
			lineTruncated = true
			truncated = true
		}
		if consumed > maxTotalBytes {
			truncated = true
			omittedLines = true
		}
		if lineTruncated {
			truncated = true
		}
		lines = append(lines, string(display))
		if consumed >= maxTotalBytes {
			if moreSource() {
				truncated = true
				omittedLines = true
			}
			break
		}
		if len(raw) > 0 && raw[len(raw)-1] != '\n' && !moreSource() {
			break
		}
	}

	if !omittedLines && len(lines) >= maxLines && moreSource() {
		truncated = true
		omittedLines = true
	}

	return fileViewReadResult{
		lines:        lines,
		modTime:      stat.ModTime(),
		size:         stat.Size(),
		truncated:    truncated,
		omittedLines: omittedLines,
	}
}

// canonicalChangedLineKey normalizes a line for changed-line matching:
// - Replaces tabs with 4 spaces (matching sanitizeRawFileLine)
// - Strips ANSI escape sequences and non-printable control characters
// - Trims leading and trailing whitespace
func canonicalChangedLineKey(s string) string {
	var out strings.Builder
	for _, r := range s {
		if r == '\t' {
			out.WriteString("    ")
		} else if r == '\r' || r == '\n' {
			continue
		} else if r < 32 || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
			if r == '\x1b' {
				out.WriteString("^[")
			}
		} else {
			out.WriteRune(r)
		}
	}
	return strings.TrimSpace(out.String())
}

func changedLinesFingerprint(changed map[string]bool) string {
	if len(changed) == 0 {
		return ""
	}
	keys := make([]string, 0, len(changed))
	for k, v := range changed {
		if v {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return strings.Join(keys, "\x00")
}

func formatFileViewLines(lines []string, display []string, changed map[string]bool, truncated bool, omittedLines bool, width int, theme tuiTheme) string {
	gutterW := len(fmt.Sprintf("%d", len(lines)))
	textBudget := maxInt(8, width-gutterW-3) // gutter + space + marker column

	var b strings.Builder
	for i, line := range display {
		line = fitStyledLine(line, textBudget)
		if i > 0 {
			b.WriteString("\n")
		}
		marker := " "
		if changed != nil && len(lines) > i && changed[canonicalChangedLineKey(lines[i])] {
			marker = theme.accent.Render("▎")
		}
		b.WriteString(theme.faintest.Render(fmt.Sprintf("%*d ", gutterW, i+1)))
		b.WriteString(marker)
		b.WriteString(line)
	}
	if truncated {
		// No exact remaining-line count: computing one would require reading the
		// rest of the file, defeating the bounded read above.
		b.WriteString("\n")
		if omittedLines {
			b.WriteString(theme.faint.Render(fmt.Sprintf("… more lines (file truncated at %d for display)", len(lines))))
		} else {
			b.WriteString(theme.faint.Render("… (line content truncated at display limit)"))
		}
	}
	return b.String()
}

// peekRenderOnly looks up an already-formatted variant in memory. It performs
// strictly 0 I/O and 0 string formatting/allocations, guaranteeing O(1) instantaneous
// access on the View() drawing path.
func (c *fileViewRenderCache) peekRenderOnly(targetPath string, width int, changedFingerprint string, loadedSeq, desiredSeq uint64, loadedRev, reqRev uint64) (string, bool) {
	if loadedSeq != desiredSeq || loadedRev < reqRev {
		return "", false
	}
	c.mu.Lock()
	elem, ok := c.items[targetPath]
	if !ok {
		c.mu.Unlock()
		return "", false
	}
	entry := elem.Value.(*fileViewCachedEntry)
	if entry.sourceRev < reqRev {
		c.mu.Unlock()
		return "", false
	}
	c.lru.MoveToFront(elem)
	c.statsData.CacheHits++
	c.mu.Unlock()

	renderKey := fmt.Sprintf("%d:%s", width, changedFingerprint)
	return entry.getRender(renderKey)
}

// loadAndRender performs the bounded read, Chroma highlighting, and formatting.
// The bounded source is always read: mtime/size are kept only as a hint because
// `cp -p`, `rsync -t` and `tar -xp` can restore matching metadata over different
// bytes, so the content hash decides freshness. On a matching metadata+hash pair
// the highlight/format work is reused (re-formatting only for a new width
// variant). It is intended to run from a tea.Cmd / background worker, off the
// View path.
var errFileViewSuperseded = errors.New("file view request superseded")

var (
	fileViewInsideLoad           func()
	fileViewBeforeCacheHitFormat func()
	fileViewBeforeDiskRead       func()
	fileViewInsideDiskRead       func()
	fileViewBeforeHighlight      func()
	fileViewInsideHighlight      func()
	fileViewBeforeFormat         func()
	fileViewBeforeCacheCommit    func()
	fileViewHighlightMu          sync.Mutex
)

func fileViewSuperseded(liveSeq *atomic.Uint64, seq uint64) bool {
	return liveSeq != nil && liveSeq.Load() != seq
}

func (c *fileViewRenderCache) loadAndRender(targetPath string, displayPath string, width int, changed map[string]bool, changedFingerprint string, reqGen int, theme tuiTheme, reqSourceRev uint64, liveSeq *atomic.Uint64, seq uint64) (string, error) {
	if fileViewSuperseded(liveSeq, seq) {
		return "", errFileViewSuperseded
	}
	if fileViewInsideLoad != nil {
		fileViewInsideLoad()
	}
	if fileViewSuperseded(liveSeq, seq) {
		return "", errFileViewSuperseded
	}

	stat, err := os.Stat(targetPath)
	if err != nil {
		c.evictPath(targetPath)
		rendered := theme.faint.Render("Could not read file: " + err.Error())
		return rendered, err
	}
	if !stat.Mode().IsRegular() {
		c.evictPath(targetPath)
		readErr := fmt.Errorf("cannot display non-regular file %s (mode %s)", displayPath, stat.Mode().String())
		rendered := theme.faint.Render("Could not read file: " + readErr.Error())
		return rendered, readErr
	}

	modTime := stat.ModTime()
	size := stat.Size()
	renderKey := fmt.Sprintf("%d:%s", width, changedFingerprint)

	c.mu.Lock()
	if c.gen != reqGen {
		c.mu.Unlock()
		return "", errors.New("request superseded by cache invalidation")
	}
	c.statsData.DiskReads++
	c.mu.Unlock()

	if fileViewBeforeDiskRead != nil {
		fileViewBeforeDiskRead()
	}
	if fileViewSuperseded(liveSeq, seq) {
		return "", errFileViewSuperseded
	}

	readRes := readFileViewBoundedCancellable(targetPath, fileViewMaxLines, fileViewMaxLineBytes, fileViewMaxBytes, liveSeq, seq)
	if readRes.err != nil && len(readRes.lines) == 0 {
		c.evictPath(targetPath)
		rendered := theme.faint.Render("Could not read file: " + readRes.err.Error())
		return rendered, readRes.err
	}
	sourceHash := fileViewSourceHash(readRes.lines)

	c.mu.Lock()
	if c.gen != reqGen {
		c.mu.Unlock()
		return "", errors.New("request superseded by cache invalidation")
	}

	curRequiredRev := c.requiredRevisionLocked(targetPath)
	if reqSourceRev < curRequiredRev {
		reqSourceRev = curRequiredRev
	}

	if elem, ok := c.items[targetPath]; ok {
		entry := elem.Value.(*fileViewCachedEntry)
		forceReload := entry.sourceRev < reqSourceRev
		if !forceReload &&
			entry.modTime.Equal(modTime) &&
			entry.size == size &&
			entry.displayPath == displayPath &&
			entry.sourceHash == sourceHash &&
			entry.truncated == readRes.truncated &&
			entry.omittedLines == readRes.omittedLines {
			if fileViewSuperseded(liveSeq, seq) {
				c.mu.Unlock()
				return "", errFileViewSuperseded
			}
			c.statsData.CacheHits++
			c.lru.MoveToFront(elem)
			c.mu.Unlock()

			if rendered, ok := entry.getRender(renderKey); ok {
				return rendered, nil
			}

			if fileViewBeforeCacheHitFormat != nil {
				fileViewBeforeCacheHitFormat()
			}
			if fileViewSuperseded(liveSeq, seq) {
				return "", errFileViewSuperseded
			}

			// Re-format for the new width or changed markers using cached display and lines
			rendered := formatFileViewLines(entry.lines, entry.display, changed, entry.truncated, entry.omittedLines, width, theme)
			if fileViewBeforeCacheCommit != nil {
				fileViewBeforeCacheCommit()
			}
			if fileViewSuperseded(liveSeq, seq) {
				return "", errFileViewSuperseded
			}
			c.commitRenderPut(entry, renderKey, rendered)
			return rendered, nil
		}
	}

	if fileViewSuperseded(liveSeq, seq) {
		c.mu.Unlock()
		return "", errFileViewSuperseded
	}

	c.statsData.CacheMisses++
	c.mu.Unlock()

	cleanLines := make([]string, len(readRes.lines))
	for i, l := range readRes.lines {
		cleanLines[i] = sanitizeRawFileLine(l)
	}

	if fileViewBeforeHighlight != nil {
		fileViewBeforeHighlight()
	}
	if fileViewSuperseded(liveSeq, seq) {
		return "", errFileViewSuperseded
	}

	display, ok := func() ([]string, bool) {
		fileViewHighlightMu.Lock()
		defer fileViewHighlightMu.Unlock()
		if fileViewSuperseded(liveSeq, seq) {
			return nil, false
		}
		if fileViewInsideHighlight != nil {
			fileViewInsideHighlight()
			if fileViewSuperseded(liveSeq, seq) {
				return nil, false
			}
		}
		c.mu.Lock()
		c.statsData.HighlightCalls++
		c.mu.Unlock()
		return highlightCodeForPathWithTheme(cleanLines, displayPath, 1<<20, nil, theme)
	}()
	if fileViewSuperseded(liveSeq, seq) {
		return "", errFileViewSuperseded
	}
	if !ok || len(display) != len(cleanLines) {
		display = cleanLines
	}

	if fileViewBeforeFormat != nil {
		fileViewBeforeFormat()
	}
	if fileViewSuperseded(liveSeq, seq) {
		return "", errFileViewSuperseded
	}

	rendered := formatFileViewLines(cleanLines, display, changed, readRes.truncated, readRes.omittedLines, width, theme)

	if fileViewBeforeCacheCommit != nil {
		fileViewBeforeCacheCommit()
	}
	if fileViewSuperseded(liveSeq, seq) {
		return "", errFileViewSuperseded
	}

	sourceBytes := 0
	for _, l := range cleanLines {
		sourceBytes += len(l)
	}
	for _, d := range display {
		sourceBytes += len(d)
	}

	entry := &fileViewCachedEntry{
		targetPath:   targetPath,
		displayPath:  displayPath,
		modTime:      readRes.modTime,
		size:         readRes.size,
		sourceRev:    reqSourceRev,
		sourceHash:   sourceHash,
		sourceBytes:  sourceBytes,
		lines:        cleanLines,
		display:      display,
		truncated:    readRes.truncated,
		omittedLines: readRes.omittedLines,
		renderKeys:   []string{renderKey},
		renders:      map[string]string{renderKey: rendered},
	}

	c.mu.Lock()
	if c.gen != reqGen {
		c.mu.Unlock()
		return "", errors.New("request superseded by cache invalidation")
	}
	if fileViewSuperseded(liveSeq, seq) {
		c.mu.Unlock()
		return "", errFileViewSuperseded
	}
	if c.maxEntries <= 0 || c.maxBytes <= 0 || entry.byteSize() > c.maxBytes {
		c.statsData.SkippedOversized++
		c.removeItemLocked(targetPath)
		c.mu.Unlock()
		return rendered, nil
	}

	if elem, ok := c.items[targetPath]; ok {
		existing := elem.Value.(*fileViewCachedEntry)
		if existing.modTime.After(readRes.modTime) && existing.sourceRev > reqSourceRev {
			c.mu.Unlock()
			return rendered, nil
		}
		c.removeItemLocked(targetPath)
	}
	c.items[targetPath] = c.lru.PushFront(entry)
	c.retained += entry.byteSize()
	c.evictOverflowLocked()
	c.mu.Unlock()

	return rendered, nil
}

// sanitizeRawFileLine strips or transforms raw control characters and terminal escape sequences
// when syntax highlighting is bypassed or unavailable, preventing terminal screen corruption.
func sanitizeRawFileLine(s string) string {
	var out strings.Builder
	for _, r := range s {
		if r == '\t' {
			out.WriteString("    ")
		} else if r == '\r' || r == '\n' {
			continue
		} else if r < 32 || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
			if r == '\x1b' {
				out.WriteString("^[")
			}
		} else {
			out.WriteRune(r)
		}
	}
	return out.String()
}

func (c *fileViewRenderCache) getOrRender(targetPath string, displayPath string, width int, changed map[string]bool) string {
	fingerprint := changedLinesFingerprint(changed)
	rendered, _ := c.loadAndRender(targetPath, displayPath, width, changed, fingerprint, c.generation(), zeroTheme, 0, nil, 0)
	return rendered
}

// fileViewLoadedMsg delivers the result of an asynchronous file read & render.
type fileViewLoadedMsg struct {
	lifetimeToken     [16]byte
	seq               uint64
	requiredSourceRev uint64
	generation        int
	targetPath        string
	displayPath       string
	width             int
	fingerprint       string
	rendered          string
	err               error
}

func loadFileViewCmd(targetPath string, displayPath string, width int, changed map[string]bool, fingerprint string, token [16]byte, seq uint64, gen int, reqRev uint64, theme tuiTheme, liveSeq *atomic.Uint64) tea.Cmd {
	return func() tea.Msg {
		if fileViewSuperseded(liveSeq, seq) {
			return fileViewLoadedMsg{lifetimeToken: token, seq: seq, requiredSourceRev: reqRev, generation: gen, targetPath: targetPath, displayPath: displayPath, width: width, fingerprint: fingerprint, err: errFileViewSuperseded}
		}
		rendered, err := defaultFileViewCache.loadAndRender(targetPath, displayPath, width, changed, fingerprint, gen, theme, reqRev, liveSeq, seq)
		return fileViewLoadedMsg{
			lifetimeToken:     token,
			seq:               seq,
			requiredSourceRev: reqRev,
			generation:        gen,
			targetPath:        targetPath,
			displayPath:       displayPath,
			width:             width,
			fingerprint:       fingerprint,
			rendered:          rendered,
			err:               err,
		}
	}
}

// fileViewState manages the drill-in view for a touched file. When active, the
// transcript body swaps to the file's diff/content instead of the chat rows.
type fileViewState struct {
	active             bool
	path               string // workspace-relative, as carried by changedFiles
	mode               int    // fileViewDiff | fileViewFull
	parentScrollOffset int
	// preservedScrollOffset holds the reader's latest scroll intent while an
	// async reload swaps the body for the one-line loading placeholder. Scroll
	// actions and prompt submission update it during the pending window, and
	// handleFileViewLoaded reconciles it against the real body.
	preservedScrollOffset int
	// completedBodyLines is the rendered body height of the last accepted
	// snapshot, including screen wrapping. A loading placeholder never updates it.
	completedBodyLines int

	// View session lifetime identity (UUIDv7 RFC 9562 0-alloc)
	lifetimeToken [16]byte

	// Monotonically advancing desired snapshot sequence & requested parameters
	desiredSeq         uint64
	requiredSourceRev  uint64
	desiredWidth       int
	desiredFingerprint string
	desiredGen         int

	// Authoritative completed snapshot (only valid when loadedSeq == desiredSeq)
	renderedContent   string
	loadedPath        string
	loadedWidth       int
	loadedGen         int
	loadedFingerprint string
	loadedToken       [16]byte
	loadedSeq         uint64
	loadedRev         uint64
	loading           bool
	hasError          bool
	snapshotReady     bool
	liveSeq           *atomic.Uint64
}

func (m model) startFileViewLoadCmd(width int) (model, tea.Cmd) {
	return m.startFileViewLoad(width, false)
}

func (m model) startFileViewRefreshCmd(width int) (model, tea.Cmd) {
	return m.startFileViewLoad(width, true)
}

// refreshFileViewMarkers reloads an open full-file view so its gutter markers
// and fingerprint are recomputed against the current transcript after a session
// or transcript transition. No-op unless a full view is open.
func (m model) refreshFileViewMarkers() (model, tea.Cmd) {
	if m.fileView.active && m.fileView.mode == fileViewFull {
		return m.startFileViewLoadCmd(m.chatColumnWidth())
	}
	return m, nil
}

func (m model) startFileViewLoad(width int, refreshSource bool) (model, tea.Cmd) {
	if !m.fileView.active || m.fileView.mode != fileViewFull || m.fileView.path == "" {
		return m, nil
	}
	target := m.fileView.path
	if !filepath.IsAbs(target) {
		target = filepath.Join(m.cwd, target)
	}
	if refreshSource {
		rev := defaultFileViewCache.invalidatePath(target)
		if rev > m.fileView.requiredSourceRev {
			m.fileView.requiredSourceRev = rev
		} else {
			m.fileView.requiredSourceRev++
		}
	}
	curReq := defaultFileViewCache.requiredRevision(target)
	if curReq > m.fileView.requiredSourceRev {
		m.fileView.requiredSourceRev = curReq
	}

	// Remember where the reader is before the body temporarily becomes the
	// loading placeholder; syncChatScroll holds it and handleFileViewLoaded
	// reconciles it against the real body. Intentional resets (new file,
	// diff/full switch) zero chatScrollOffset before reaching here.
	m.fileView.preservedScrollOffset = m.chatScrollOffset
	m.fileView.desiredSeq++
	m.fileView.desiredWidth = width
	m.fileView.loading = true
	m.fileView.snapshotReady = false
	if m.fileView.liveSeq == nil {
		m.fileView.liveSeq = new(atomic.Uint64)
	}
	seq := m.fileView.desiredSeq
	m.fileView.liveSeq.Store(seq)
	token := m.fileView.lifetimeToken
	gen := defaultFileViewCache.generation()
	changed := m.fileViewChangedLines()
	fingerprint := changedLinesFingerprint(changed)
	m.fileView.desiredFingerprint = fingerprint
	m.fileView.desiredGen = gen
	reqRev := m.fileView.requiredSourceRev
	theme := zeroTheme
	return m, loadFileViewCmd(target, m.fileView.path, width, changed, fingerprint, token, seq, gen, reqRev, theme, m.fileView.liveSeq)
}

// openFileView activates the drill-in for path in diff mode. Opening from an
// already-open view (clicking another FILES row) keeps the original saved chat
// scroll position rather than saving the file view's own offset as "parent".
// Re-opening the file that is ALREADY being viewed is a no-op: a stray
// re-click must not bounce the user from full mode back to diff or reset
// their scroll position.
func (m model) revokeFileViewRequest() {
	if m.fileView.liveSeq != nil {
		m.fileView.liveSeq.Add(1)
	}
}

func (m model) openFileView(path string) (model, tea.Cmd) {
	if m.fileView.active && m.fileView.path == path {
		return m, nil
	}
	if m.fileView.active {
		m.revokeFileViewRequest()
	}
	if !m.fileView.active {
		m.fileView.parentScrollOffset = m.chatScrollOffset
	}
	target := path
	if !filepath.IsAbs(target) {
		target = filepath.Join(m.cwd, target)
	}
	m.fileView.active = true
	m.fileView.path = path
	m.fileView.mode = fileViewDiff
	m.fileView.lifetimeToken = nextFileViewLifetimeToken()
	m.fileView.renderedContent = ""
	m.fileView.loadedToken = [16]byte{}
	m.fileView.loadedSeq = 0
	m.fileView.loadedRev = 0
	m.fileView.requiredSourceRev = defaultFileViewCache.requiredRevision(target)
	m.fileView.hasError = false
	// A file only the git sweep knows about (bash/subagent mutation) has no edit
	// cards to stack — open straight on the full file instead of a placeholder.
	if len(m.fileViewResultRows()) == 0 {
		m.fileView.mode = fileViewFull
	}
	m.chatScrollOffset = 0
	m = m.clearHover() // bodyY numbering differs between the file body and the chat
	if m.fileView.mode == fileViewFull {
		return m.startFileViewLoadCmd(m.chatColumnWidth())
	}
	return m, nil
}

// exitFileView deactivates the view and restores the chat scroll position.
func (m model) exitFileView() model {
	if !m.fileView.active {
		return m
	}
	m.revokeFileViewRequest()
	m.chatScrollOffset = m.fileView.parentScrollOffset
	m.fileView = fileViewState{}
	m = m.clearHover()
	return m
}

// setFileViewMode switches diff/full, resetting the scroll to the bottom-anchored
// start since the two bodies have unrelated heights.
func (m model) setFileViewMode(mode int) (model, tea.Cmd) {
	if !m.fileView.active || m.fileView.mode == mode {
		return m, nil
	}
	if m.fileView.mode == fileViewFull && mode != fileViewFull {
		m.revokeFileViewRequest()
	}
	m.fileView.mode = mode
	m.chatScrollOffset = 0
	if mode == fileViewFull {
		return m.startFileViewLoadCmd(m.chatColumnWidth())
	}
	return m, nil
}

func (m model) handleFileViewLoaded(msg fileViewLoadedMsg) (model, tea.Cmd) {
	if !m.fileView.active || m.fileView.mode != fileViewFull {
		return m, nil
	}
	if errors.Is(msg.err, errFileViewSuperseded) {
		return m, nil
	}
	// 1. Session lifetime identity match
	if m.fileView.lifetimeToken != msg.lifetimeToken || m.fileView.path != msg.displayPath {
		return m, nil
	}
	// 2. Exact desired snapshot match: reject superseded / out-of-order completions without any side effects
	if msg.seq != m.fileView.desiredSeq ||
		msg.width != m.fileView.desiredWidth ||
		msg.fingerprint != m.fileView.desiredFingerprint {
		// Out-of-order or obsolete sequence: drop without modifying state or triggering retries
		return m, nil
	}

	// 3. Current sequence with invalid theme generation: request a retry inheriting current source revision requirement
	if msg.generation != defaultFileViewCache.generation() {
		return m.startFileViewLoadCmd(m.chatColumnWidth())
	}

	// 4. Source revision monotonic check: if required revision was advanced while in flight, request a refresh
	if msg.requiredSourceRev < m.fileView.requiredSourceRev {
		return m.startFileViewRefreshCmd(m.chatColumnWidth())
	}

	m.fileView.loading = false
	m.fileView.snapshotReady = true
	m.fileView.renderedContent = msg.rendered
	m.fileView.loadedPath = msg.displayPath
	m.fileView.loadedWidth = msg.width
	m.fileView.loadedGen = msg.generation
	m.fileView.loadedFingerprint = msg.fingerprint
	m.fileView.loadedToken = msg.lifetimeToken
	m.fileView.loadedSeq = msg.seq
	m.fileView.loadedRev = msg.requiredSourceRev
	m.fileView.hasError = (msg.err != nil)
	// Reconcile the held reading offset against the real body now that the
	// loading placeholder is gone. Apply the height delta exactly once to
	// preserve the absolute position, while offset zero keeps following the tail.
	current, maxOffset := m.chatScrollMetrics()
	if m.fileView.preservedScrollOffset > 0 {
		m.chatScrollOffset = clampInt(m.fileView.preservedScrollOffset+current-m.fileView.completedBodyLines, 0, maxOffset)
		if m.chatScrollOffset > 0 {
			m.chatBodyLines = current
		} else {
			m.chatBodyLines = 0
		}
	}
	m.fileView.completedBodyLines = current
	return m, nil
}

// fileViewNavBar renders the single-line header shown in place of the pinned
// title bar while the view is active: the path plus the key hints. One line
// exactly, so every scrollableTranscriptFrame computed against it agrees with
// the title bar's geometry.
func (m model) fileViewNavBar(width int) string {
	mode := "diff"
	other := "f full"
	if m.fileView.mode == fileViewFull {
		mode = "full"
		other = "d diff"
	}
	left := zeroTheme.accent.Render("← "+truncatePathLeft(m.fileView.path, maxInt(8, width/2))) +
		zeroTheme.faint.Render("  ·  "+mode)
	right := zeroTheme.faint.Render(other + " · esc back")
	return fitStyledLine(joinHeaderLine(left, right, width), width)
}

// fileViewBodyItems builds the body items the transcript machinery renders
// while the view is active — one pre-rendered block, so scrolling and height
// accounting flow through the exact same path as chat rows.
func (m model) fileViewBodyItems(width int) []transcriptBodyItem {
	var block string
	if m.fileView.mode == fileViewFull {
		block = m.renderFileViewFull(width)
	} else {
		block = m.renderFileViewDiff(width)
	}
	return []transcriptBodyItem{transcriptBlockBodyItem(transcriptBodyItemRow, -1, block)}
}

// fileViewResultRows returns the transcript's tool-result rows that touched the
// viewed file, in chronological order.
func (m model) fileViewResultRows() []transcriptRow {
	var rows []transcriptRow
	for _, row := range m.transcript {
		if row.kind != rowToolResult {
			continue
		}
		for _, p := range row.changedFiles {
			if p == m.fileView.path {
				rows = append(rows, row)
				break
			}
		}
	}
	return rows
}

// renderFileViewDiff renders the session's edits to the file as its full-depth
// tool cards (bodyCap 0, the detailed-view depth), stacked chronologically —
// the same cards the chat shows, so the diffs read identically in both places.
func (m model) renderFileViewDiff(width int) string {
	rows := m.fileViewResultRows()
	if len(rows) == 0 {
		return zeroTheme.faint.Render("No recorded edits for this file in this session.")
	}
	rc := buildRowContext(m.transcript)
	opts := cardRenderOptions{bodyCap: 0, cwd: m.cwd}
	var b strings.Builder
	for i, row := range rows {
		if i > 0 {
			b.WriteString("\n\n")
		}
		if len(rows) > 1 {
			b.WriteString(zeroTheme.faint.Render(fmt.Sprintf("edit %d of %d", i+1, len(rows))))
			b.WriteString("\n")
		}
		b.WriteString(m.renderRowModeUncached(row, width, rc, opts))
	}
	return b.String()
}

// renderFileViewFull renders the file as it currently stands on disk, syntax
// highlighted, with a line-number gutter and an accent ▎ marker on the lines
// this session's diffs added (matched by exact text — an approximation that
// tolerates later drift; a stale marker just doesn't highlight).
// It is strictly non-blocking and performs O(1) lookup without invoking formatters.
func (m model) renderFileViewFull(width int) string {
	if m.fileView.hasError {
		return m.fileView.renderedContent
	}
	target := m.fileView.path
	if !filepath.IsAbs(target) {
		target = filepath.Join(m.cwd, target)
	}
	if m.fileView.snapshotReady &&
		m.fileView.loadedPath == m.fileView.path &&
		m.fileView.loadedSeq == m.fileView.desiredSeq &&
		m.fileView.loadedRev >= m.fileView.requiredSourceRev &&
		m.fileView.loadedGen == defaultFileViewCache.generation() &&
		m.fileView.loadedToken == m.fileView.lifetimeToken {
		defaultFileViewCache.recordHit()
		return m.fileView.renderedContent
	}
	if cached, ok := defaultFileViewCache.peekRenderOnly(target, width, m.fileView.desiredFingerprint, m.fileView.loadedSeq, m.fileView.desiredSeq, m.fileView.loadedRev, m.fileView.requiredSourceRev); ok {
		return cached
	}
	return zeroTheme.faint.Render(fileViewLoadingPlaceholder)
}

// fileViewChangedLines collects the trimmed text of every line the session's
// diffs ADDED to the viewed file, for the full-mode gutter markers. Very short
// lines ("}", "return", ")") are skipped: text-matching them would mark every
// unrelated occurrence across the file, which misleads far more than a missing
// marker on a brace line ever could.
func (m model) fileViewChangedLines() map[string]bool {
	changed := map[string]bool{}
	for _, row := range m.fileViewResultRows() {
		for _, line := range strings.Split(row.detail, "\n") {
			if !strings.HasPrefix(line, "+") || strings.HasPrefix(line, "+++") {
				continue
			}
			raw := strings.TrimPrefix(line, "+")
			if key := canonicalChangedLineKey(raw); len(key) >= 4 {
				changed[key] = true
			}
		}
	}
	return changed
}

// toolResultMayMutateUnknownScope reports tools that can change workspace files
// but do not reliably report the affected paths (ChangedFiles). Their completion
// must trigger the conservative unknown-scope refresh so an open file view
// cannot keep rendering a stale snapshot. Task/TaskOutput successful results are
// suppressed from the transcript, so their completion boundaries carry this
// refresh separately.
func toolResultMayMutateUnknownScope(name string) bool {
	switch name {
	case "bash", "exec_command", "terminal_session", "write_stdin",
		"swarm_collect", "swarm_status", "Task", "TaskOutput":
		return true
	}
	return false
}

// invalidateFileViewForUnknownMutation performs the conservative refresh used
// when a completed action may have changed workspace files but no exact path is
// known: purge the shared render cache and, when a full file view is open,
// schedule an async reload so its snapshot cannot stay stale.
func (m model) invalidateFileViewForUnknownMutation() (model, tea.Cmd) {
	defaultFileViewCache.invalidateUnknownScope()
	var selfCmd tea.Cmd
	if m.fileView.active && m.fileView.mode == fileViewFull {
		m, selfCmd = m.startFileViewRefreshCmd(m.chatColumnWidth())
	}
	var peerCmd tea.Cmd
	m, peerCmd = m.refreshHiddenParentFileView()
	return m, batchCommands(selfCmd, peerCmd)
}

// recoverInvalidatedFileView schedules a snapshot for a surface whose shared
// cache generation changed, or whose open path's required revision advanced,
// while another BTW surface handled the invalidation. Path-reporting tools bump
// only that revision: outside a git workspace no sweep follows, so the other
// surface must notice the revision itself and reload immediately.
func (m model) recoverInvalidatedFileView() (model, tea.Cmd) {
	if !m.fileView.active || m.fileView.mode != fileViewFull {
		return m, nil
	}
	generation := m.fileView.loadedGen
	if m.fileView.loading {
		generation = m.fileView.desiredGen
	}
	if generation != defaultFileViewCache.generation() {
		return m.startFileViewLoadCmd(m.chatColumnWidth())
	}
	target := m.fileView.path
	if !filepath.IsAbs(target) {
		target = filepath.Join(m.cwd, target)
	}
	if defaultFileViewCache.requiredRevision(target) > m.fileView.requiredSourceRev {
		return m.startFileViewLoadCmd(m.chatColumnWidth())
	}
	return m, nil
}
