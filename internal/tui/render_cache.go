package tui

import (
	"container/list"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/Gitlawb/zero/internal/agent"
)

const (
	defaultRenderCacheMaxEntries    = 256
	defaultRenderCacheMaxCharacters = 512 * 1024
)

var defaultRenderCache = newStaticRenderCache(defaultRenderCacheMaxEntries, defaultRenderCacheMaxCharacters)

type renderCacheStats struct {
	Hits             int
	Misses           int
	Evictions        int
	SkippedOversized int
}

type staticRenderCache struct {
	mu            sync.Mutex
	maxEntries    int
	maxCharacters int
	retained      int
	items         map[string]*list.Element
	lru           *list.List
	statsData     renderCacheStats
}

type staticRenderCacheEntry struct {
	key   string
	value string
	chars int
}

func newStaticRenderCache(maxEntries int, maxCharacters int) *staticRenderCache {
	return &staticRenderCache{
		maxEntries:    maxEntries,
		maxCharacters: maxCharacters,
		items:         map[string]*list.Element{},
		lru:           list.New(),
	}
}

// clear drops every cached render. Called when the active theme changes so stale
// entries painted in the old palette are never reused.
func (c *staticRenderCache) clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items = map[string]*list.Element{}
	c.lru.Init()
	c.retained = 0
}

func (c *staticRenderCache) render(key string, stable bool, render func() string) string {
	if render == nil {
		return ""
	}
	if c == nil || !stable || key == "" {
		return render()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if element, ok := c.items[key]; ok {
		c.statsData.Hits++
		c.lru.MoveToFront(element)
		return element.Value.(*staticRenderCacheEntry).value
	}

	c.statsData.Misses++
	value := render()
	chars := utf8.RuneCountInString(value)
	if c.maxEntries <= 0 || c.maxCharacters <= 0 || chars > c.maxCharacters {
		c.statsData.SkippedOversized++
		return value
	}

	entry := &staticRenderCacheEntry{key: key, value: value, chars: chars}
	c.items[key] = c.lru.PushFront(entry)
	c.retained += chars
	c.evictOverflow()
	return value
}

func (c *staticRenderCache) evictOverflow() {
	for len(c.items) > c.maxEntries || c.retained > c.maxCharacters {
		element := c.lru.Back()
		if element == nil {
			c.retained = 0
			return
		}
		entry := element.Value.(*staticRenderCacheEntry)
		delete(c.items, entry.key)
		c.lru.Remove(element)
		c.retained -= entry.chars
		c.statsData.Evictions++
	}
}

func (m model) renderRowCacheKey(row transcriptRow, width int, rc rowContext, opts cardRenderOptions, flush bool) (string, bool) {
	stable := true
	switch row.kind {
	case rowWelcome:
		return "", false
	case rowToolCall:
		if m.pending && row.runID != 0 && row.runID == m.activeRunID {
			stable = false
		}
	case rowPermission:
		event := row.permission
		if event != nil && event.ToolCallID != "" && event.Action == agent.PermissionActionPrompt &&
			!rc.decided[rcKey(row.runID, event.ToolCallID)] &&
			m.pending && row.runID != 0 && row.runID == m.activeRunID {
			stable = false
		}
	case rowSpecialist:
		if row.specialistInfo != nil && row.specialistInfo.status == specialistRunning {
			stable = false
		}
	}

	var b strings.Builder
	appendRenderCacheField(&b, "render-row-v1")
	appendRenderCacheField(&b, strconv.Itoa(width))
	appendRenderCacheField(&b, strconv.FormatBool(flush))
	appendRenderCacheField(&b, strconv.Itoa(opts.bodyCap))
	appendRenderCacheField(&b, strconv.FormatBool(opts.expanded))
	appendRenderCacheField(&b, opts.cwd)
	appendRenderCacheField(&b, strconv.Itoa(int(row.kind)))
	appendRenderCacheField(&b, row.id)
	appendRenderCacheField(&b, row.text)
	appendRenderCacheField(&b, row.tool)
	appendRenderCacheField(&b, fmt.Sprint(row.status))
	appendRenderCacheField(&b, row.detail)
	appendRenderCacheField(&b, row.arg)
	appendRenderCacheField(&b, strconv.Itoa(row.runID))
	appendRenderCacheField(&b, strconv.FormatBool(row.expanded))
	appendRenderCacheField(&b, strconv.FormatBool(row.final))
	appendRenderCacheField(&b, strconv.Itoa(row.turnTools))
	appendRenderCacheField(&b, strconv.FormatInt(int64(row.turnElapsed), 10))
	appendRenderCacheField(&b, strconv.Itoa(row.attachments.images))
	appendRenderCacheField(&b, strconv.Itoa(row.attachments.documents))
	// The FILES selection tints this row's card border, so selecting/deselecting
	// a file must miss the cache entry rendered under the other state.
	appendRenderCacheField(&b, strconv.FormatBool(m.rowTouchesSelectedFile(row)))

	key := rcKey(row.runID, row.id)
	appendRenderCacheField(&b, rc.hints[key])
	appendRenderCacheField(&b, rc.args[key])
	appendRenderCacheField(&b, strconv.FormatBool(rc.auto[key]))
	appendRenderCacheField(&b, permissionCacheFingerprint(row.permission))
	appendRenderCacheField(&b, askUserCacheFingerprint(row.askUser))
	return b.String(), stable
}

func appendRenderCacheField(b *strings.Builder, value string) {
	b.WriteString(strconv.Itoa(len(value)))
	b.WriteByte(':')
	b.WriteString(value)
	b.WriteByte('|')
}

func permissionCacheFingerprint(event *agent.PermissionEvent) string {
	if event == nil {
		return ""
	}
	fields := []string{
		event.ToolCallID,
		event.ToolName,
		string(event.Action),
		string(event.DecisionAction),
		event.Permission,
		string(event.PermissionMode),
		event.Autonomy,
		event.SideEffect,
		event.Reason,
		event.Scope,
		string(event.Risk.Level),
		strconv.FormatBool(event.GrantMatched),
		strconv.FormatBool(event.Grant != nil),
		strconv.FormatBool(event.Grant != nil && event.Grant.Session),
	}
	if event.Block != nil {
		fields = append(fields,
			string(event.Block.Code),
			string(event.Block.Risk.Level),
			event.Block.Path,
			event.Block.Reason,
		)
	}
	var b strings.Builder
	for _, field := range fields {
		appendRenderCacheField(&b, field)
	}
	return b.String()
}

func askUserCacheFingerprint(request *agent.AskUserRequest) string {
	if request == nil {
		return ""
	}
	var b strings.Builder
	appendRenderCacheField(&b, request.ToolCallID)
	appendRenderCacheField(&b, request.Header)
	for _, question := range request.Questions {
		appendRenderCacheField(&b, question.Question)
		appendRenderCacheField(&b, strconv.FormatBool(question.MultiSelect))
		for _, option := range question.Options {
			appendRenderCacheField(&b, option)
		}
	}
	return b.String()
}
