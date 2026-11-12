package tui

import (
	"container/list"
	"sync"
)

const defaultTranscriptBodyHeightCacheMaxEntries = 2048

// transcriptBodyHeightCacheHardCap bounds the cache's auto-growth. Height
// entries are content-addressed (the key embeds row text, status, width,
// theme-relevant hints) so they stay valid forever and cost ~200B each; the
// cap only exists to keep a marathon session's stale variants from
// accumulating unboundedly.
const transcriptBodyHeightCacheHardCap = 65536

type transcriptBodyHeightCache struct {
	mu         sync.Mutex
	maxEntries int
	items      map[string]*list.Element
	lru        *list.List
}

type transcriptBodyHeightCacheEntry struct {
	key    string
	height int
}

func newTranscriptBodyHeightCache(maxEntries int) *transcriptBodyHeightCache {
	return &transcriptBodyHeightCache{
		maxEntries: maxEntries,
		items:      map[string]*list.Element{},
		lru:        list.New(),
	}
}

// reset shrinks the ceiling back to the default after a full transcript
// rebuild (theme change, /clear, /resume): the old working set is gone, and
// stale entries can never be hit again with different content because keys
// are content-addressed, but releasing the memory is the honest thing to do.
func (c *transcriptBodyHeightCache) reset() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items = map[string]*list.Element{}
	c.lru.Init()
	c.maxEntries = defaultTranscriptBodyHeightCacheMaxEntries
}

func (c *transcriptBodyHeightCache) get(key string) (int, bool) {
	if c == nil || key == "" {
		return 0, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	element, ok := c.items[key]
	if !ok {
		return 0, false
	}
	c.lru.MoveToFront(element)
	return element.Value.(*transcriptBodyHeightCacheEntry).height, true
}

// transcriptBodyRenderCache memoises the fully rendered (display string +
// selectable geometry at bodyY origin 0) result of a transcript body item.
// layoutVisibleTranscriptBodyItems re-renders every visible item on EVERY
// frame — including idle throbber ticks — and the render closures re-wrap
// markdown, ansi-strip every line, and re-derive tool-card toggle state each
// time. Settled rows are content-addressed by the same key the height cache
// already computes (renderRowCacheKey covers row text, status, rc hints/args,
// width, bodyCap, theme-relevant fields), so a hit skips all of that work.
// Selection highlight and hover are painted fresh after the hit; the stored
// selectable slice is copied on every hit so callers can shift/extend it.
type transcriptBodyRenderCache struct {
	mu            sync.Mutex
	maxEntries    int
	maxCharacters int
	retained      int
	items         map[string]*list.Element
	lru           *list.List
	hits          int
	misses        int
}

type transcriptBodyRenderedEntry struct {
	key        string
	rendered   string
	selectable []transcriptSelectableLine
	chars      int
}

func newTranscriptBodyRenderCache(maxEntries int, maxCharacters int) *transcriptBodyRenderCache {
	return &transcriptBodyRenderCache{
		maxEntries:    maxEntries,
		maxCharacters: maxCharacters,
		items:         map[string]*list.Element{},
		lru:           list.New(),
	}
}

const (
	defaultTranscriptBodyRenderCacheMaxEntries    = 256
	defaultTranscriptBodyRenderCacheMaxCharacters = 512 * 1024
	// transcriptBodyRenderCacheHardEntries bounds auto-growth of the render
	// cache. Like the height cache, a live tail larger than the static default
	// thrashes the LRU every frame; entries are content-addressed, so growth
	// trades bounded resident render text for eliminating full re-renders.
	transcriptBodyRenderCacheHardEntries = 8192
	// transcriptBodyRenderCacheHardCharacters caps retained render bytes.
	transcriptBodyRenderCacheHardCharacters = 8 * 1024 * 1024
)

var defaultTranscriptBodyRenders = newTranscriptBodyRenderCache(
	defaultTranscriptBodyRenderCacheMaxEntries,
	defaultTranscriptBodyRenderCacheMaxCharacters,
)

// clear drops every cached render. Like the static render cache, it must run
// when the active theme changes so stale entries painted in the old palette
// are never reused.
func (c *transcriptBodyRenderCache) clear() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items = map[string]*list.Element{}
	c.lru.Init()
	c.retained = 0
}

// render returns the display string and a selectable copy shifted to
// startBodyY. When stable is false or key is empty (live rows whose render
// embeds per-frame state like the spinner), the cache is bypassed entirely.
func (c *transcriptBodyRenderCache) render(key string, stable bool, startBodyY int, build func() (string, []transcriptSelectableLine)) (string, []transcriptSelectableLine) {
	if c == nil || !stable || key == "" {
		rendered, selectable := build()
		return rendered, shiftSelectableBodyY(selectable, startBodyY)
	}
	c.mu.Lock()
	if element, ok := c.items[key]; ok {
		c.hits++
		c.lru.MoveToFront(element)
		entry := element.Value.(*transcriptBodyRenderedEntry)
		selectable := shiftSelectableBodyY(copySelectableLines(entry.selectable), startBodyY)
		c.mu.Unlock()
		return entry.rendered, selectable
	}
	c.mu.Unlock()
	rendered, selectable := build()
	// The builder renders at startBodyY == 0; the returned slice is owned by
	// the caller and shifted in place, while the cache stores the origin copy.
	cached := copySelectableLines(selectable)
	c.mu.Lock()
	c.misses++
	chars := len(rendered)
	if c.maxEntries > 0 && c.maxCharacters > 0 && chars <= c.maxCharacters {
		c.items[key] = c.lru.PushFront(&transcriptBodyRenderedEntry{key: key, rendered: rendered, selectable: cached, chars: chars})
		c.retained += chars
		c.growLocked()
		for len(c.items) > c.maxEntries || c.retained > c.maxCharacters {
			element := c.lru.Back()
			if element == nil {
				c.retained = 0
				break
			}
			entry := element.Value.(*transcriptBodyRenderedEntry)
			delete(c.items, entry.key)
			c.lru.Remove(element)
			c.retained -= entry.chars
		}
	}
	c.mu.Unlock()
	return rendered, shiftSelectableBodyY(selectable, startBodyY)
}

// growLocked raises the entry and character ceilings toward their hard caps
// as the working set grows, for the same reason and with the same shape as
// transcriptBodyHeightCache.growLocked: a long run's live tail previously
// evicted rows the very next tick still needed, turning every spinner frame
// into a full markdown re-wrap + highlight + selectable re-derivation.
func (c *transcriptBodyRenderCache) growLocked() {
	if c.maxEntries < transcriptBodyRenderCacheHardEntries {
		c.maxEntries = minInt(maxInt(c.maxEntries*2, c.maxEntries+256), transcriptBodyRenderCacheHardEntries)
	}
	if c.maxCharacters < transcriptBodyRenderCacheHardCharacters {
		c.maxCharacters = minInt(maxInt(c.maxCharacters*2, c.maxCharacters+defaultTranscriptBodyRenderCacheMaxCharacters), transcriptBodyRenderCacheHardCharacters)
	}
}

// reset drops every cached render and shrinks the ceilings back to their
// defaults. Like clear(), it must run when the active theme changes so stale
// entries painted in the old palette are never reused; unlike clear() it also
// releases the growth a previous working set earned, since that set is gone.
func (c *transcriptBodyRenderCache) reset() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items = map[string]*list.Element{}
	c.lru.Init()
	c.retained = 0
	c.maxEntries = defaultTranscriptBodyRenderCacheMaxEntries
	c.maxCharacters = defaultTranscriptBodyRenderCacheMaxCharacters
}

func copySelectableLines(lines []transcriptSelectableLine) []transcriptSelectableLine {
	if lines == nil {
		return nil
	}
	out := make([]transcriptSelectableLine, len(lines))
	copy(out, lines)
	return out
}

func shiftSelectableBodyY(lines []transcriptSelectableLine, delta int) []transcriptSelectableLine {
	if delta == 0 {
		return lines
	}
	for index := range lines {
		lines[index].bodyY += delta
	}
	return lines
}

func (c *transcriptBodyHeightCache) set(key string, height int) {
	if c == nil || key == "" || c.maxEntries <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if element, ok := c.items[key]; ok {
		element.Value.(*transcriptBodyHeightCacheEntry).height = height
		c.lru.MoveToFront(element)
		return
	}
	c.items[key] = c.lru.PushFront(&transcriptBodyHeightCacheEntry{key: key, height: height})
	c.growLocked()
	for len(c.items) > c.maxEntries {
		element := c.lru.Back()
		if element == nil {
			return
		}
		entry := element.Value.(*transcriptBodyHeightCacheEntry)
		delete(c.items, entry.key)
		c.lru.Remove(element)
	}
}

// growLocked raises the entry ceiling toward the hard cap as the working set
// grows. A long run whose live tail (81-row tool outputs, plan checklists,
// specialist cards) exceeds the static cap turned every frame into full
// re-renders: the LRU evicted rows the next tick still needed (confirmed by
// profile: ~99% height-cache miss rate on sessions with big tails). Heights
// are content-addressed and cheap to retain, so the cache grows to fit the
// observed working set instead of thrashing; eviction resumes at the cap.
func (c *transcriptBodyHeightCache) growLocked() {
	if c.maxEntries >= transcriptBodyHeightCacheHardCap {
		return
	}
	next := maxInt(c.maxEntries*2, c.maxEntries+512)
	c.maxEntries = minInt(next, transcriptBodyHeightCacheHardCap)
}
