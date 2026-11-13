package agentsessions

import (
	"encoding/json"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/Twigpine/zero/internal/sessions"
	"github.com/Twigpine/zero/internal/tools"
)

// An imported session's tool work never reaches the model.
//
// sessions.promptContextEvents — the filter behind both `zero exec --resume` and
// the TUI's /resume — passes only EventMessage, EventCompaction, EventError and
// the session-lifecycle types. EventToolCall and EventToolResult are not in that
// list, so every file the other agent read, every command it ran and every error
// it hit is visible in the transcript and invisible to the model continuing the
// work. On a real 22-event import that left 2 messages and 1,155 characters of
// context.
//
// This file closes that gap without touching the shared filter: while
// translating, it records what the tools actually did and emits the result as
// an assistant EventMessage, a type the filter already passes.
//
// EventCompaction also passes the filter and was the first choice, but it
// carries a second contract on the replay side — RehydrateEvents restructures
// the transcript around the last one — so a summary with no CompactionPayload
// bookkeeping is hoisted to the front on resume. EventMessage passes the digest
// with no such side effect; see noteEvent.
//
// Zero's own compaction was the other alternative and is strictly worse here:
// sessions.toolPayloadPreview allow-lists id/name/toolName/status and drops
// "arguments" and "output", so a compaction summariser learns that a Read failed
// but never which file or why. At translation time those values are still in
// hand.

// maxSummaryEventBytes keeps one summary event inside the digest's per-event
// budget. sessions.summarizePayload truncates every event to 500 bytes, so
// a single long summary would be sliced mid-sentence; several short ones each
// arrive intact. The margin absorbs the payload's own framing.
const maxSummaryEventBytes = 460

// maxSummaryItems bounds one line's list before it collapses to a count. A
// session that touched ninety files should say so rather than name eleven of
// them and imply that was all.
const maxSummaryItems = 12

// activityLog accumulates what another agent's tools did during a translation.
// It is filled from the same pass that builds the events, so it costs no extra
// read of the transcript.
type activityLog struct {
	cwd string

	calls  int
	failed int

	read              []string
	changed           []string
	commands          []string
	searches          []string
	failures          []string
	requestedCommands []string
	requestedSearches []string

	// pendingPath maps a call id to the activity claim it would contribute, held
	// until the call's RESULT is known. A path is committed to its bucket only
	// once a success result confirms the tool ran: a Read of a path that does
	// not exist must not appear under "Files read", and a Write whose result
	// never arrives (an interrupted final call) must not appear under "Files
	// changed" — both read as fact and are exactly the wrong thing to tell the
	// next model. Unconfirmed command/search requests are labeled as requests;
	// unanswered file claims are discarded.
	pendingPath map[string]pathClaim

	// toolCounts is the fallback when nothing could be extracted from a tool's
	// arguments: naming the tools and how often they ran is still true, where
	// guessing at a filename would not be.
	toolCounts map[string]int

	seen map[string]bool
}

// pathClaim is a file path a call would contribute to a bucket, remembered until
// its result is known and only then committed.
type pathClaim struct {
	bucket string
	value  string
}

func newActivityLog(cwd string) *activityLog {
	return &activityLog{
		cwd:         cwd,
		toolCounts:  map[string]int{},
		seen:        map[string]bool{},
		pendingPath: map[string]pathClaim{},
	}
}

// commitClaim records a pending file claim now that its call has succeeded.
// Committing on success rather than withdrawing on failure is what keeps the
// keying correct: a successful Write of a path and a later FAILED Edit of the
// same path each carry their own call id, so the failure simply never commits
// and cannot erase the success — the withdraw-by-value it replaced could.
func (log *activityLog) commitClaim(claim pathClaim) {
	list := &log.read
	switch claim.bucket {
	case "changed":
		list = &log.changed
	case "cmd":
		list = &log.commands
	case "search":
		list = &log.searches
	}
	log.add(claim.bucket, list, claim.value)
}

// add appends value to list unless an equal value is already recorded under
// bucket. Deduplicated because agents re-read the same file repeatedly and a
// list of forty identical paths tells the reader nothing.
func (log *activityLog) add(bucket string, list *[]string, value string) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return
	}
	key := bucket + "\x00" + trimmed
	if log.seen[key] {
		return
	}
	log.seen[key] = true
	*list = append(*list, trimmed)
}

// observeCall records one tool invocation.
//
// Classification is by ARGUMENT KEY, not by tool name. Tool names differ across
// agents and versions (Read/read_file/view, Bash/exec/shell), but the parameter
// carrying a path is reliably called some spelling of "path", and a command is
// reliably "command". Keys are allow-listed rather than sniffed: an unknown
// schema falls back to the tool-name count instead of guessing which field is a
// filename (repo invariant #2, and #8 — these arguments are untrusted input).
func (log *activityLog) observeCall(callID string, name string, arguments string) {
	log.calls++
	trimmedName := strings.TrimSpace(DisplayField(name))
	if trimmedName == "" {
		trimmedName = "unknown"
	}

	fields := map[string]any{}
	if json.Unmarshal([]byte(arguments), &fields) != nil {
		// Not a JSON object. Codex's custom_tool_call carries a bare script in
		// "input", which is a command in every sense that matters here.
		if script := strings.TrimSpace(arguments); script != "" {
			log.pendingPath[callID] = pathClaim{bucket: "cmd", value: log.shorten(script)}
			return
		}
		log.toolCounts[trimmedName]++
		return
	}

	if command := firstStringField(fields, "command", "cmd", "script", "input"); command != "" {
		log.pendingPath[callID] = pathClaim{bucket: "cmd", value: log.shorten(command)}
		return
	}
	if pattern := firstStringField(fields, "pattern", "query", "search_query", "regex"); pattern != "" {
		log.pendingPath[callID] = pathClaim{bucket: "search", value: log.shorten(pattern)}
		return
	}
	if path := firstStringField(fields, "file_path", "filePath", "path", "notebook_path", "target_file"); path != "" {
		bucket := "read"
		if isMutatingToolName(trimmedName) {
			bucket = "changed"
		}
		// Hold the claim, do not record it yet: it is committed only when this
		// call's result confirms success (observeResult). A failed call never
		// commits, and a call whose result never arrives stays pending and is
		// dropped, so neither can claim a file it may not have touched.
		log.pendingPath[callID] = pathClaim{bucket: bucket, value: log.relative(path)}
		return
	}
	log.toolCounts[trimmedName]++
}

// observeResult records a tool's outcome. A success commits the call's held
// file claim (see observeCall); a failure drops it and is itself recorded in the
// failures bucket — the single most useful thing to carry forward, since it is
// what the next model would otherwise repeat.
func (log *activityLog) observeResult(callID string, name string, status tools.Status, output string) {
	claim, hadClaim := log.pendingPath[callID]
	delete(log.pendingPath, callID)
	if status == tools.StatusOK {
		// Success confirms the call ran: only now is its path recorded.
		if hadClaim {
			log.commitClaim(claim)
		}
		return
	}
	if status != tools.StatusError {
		// A foreign format that supplies no outcome evidence is not proof of
		// success or failure. Drop its pending factual claim while preserving the
		// raw result event for history.
		if hadClaim {
			log.retainUnconfirmed(claim)
		}
		return
	}
	if hadClaim {
		log.retainUnconfirmed(claim)
	}
	// The call did not do what it claimed, so its pending claim is dropped
	// (never committed) and the failure itself is recorded below.
	log.failed++
	trimmedName := strings.TrimSpace(DisplayField(name))
	if trimmedName == "" {
		trimmedName = "unknown"
	}
	detail := log.shorten(output)
	if detail == "" {
		detail = "failed"
	}
	log.add("fail", &log.failures, trimmedName+": "+detail)
}

func (log *activityLog) retainUnconfirmed(claim pathClaim) {
	switch claim.bucket {
	case "cmd":
		log.add("requested-cmd", &log.requestedCommands, claim.value)
	case "search":
		log.add("requested-search", &log.requestedSearches, claim.value)
	}
}

// isMutatingToolName reports whether a tool that takes a path changes the file.
// Substring matching on the name is deliberate: it holds across Write/write_file/
// FileWrite/apply_patch without a per-agent table to keep in sync.
func isMutatingToolName(name string) bool {
	lowered := strings.ToLower(name)
	for _, marker := range []string{"write", "edit", "patch", "create", "append", "insert", "replace", "delete", "remove"} {
		if strings.Contains(lowered, marker) {
			return true
		}
	}
	return false
}

// relative shortens an absolute path against the session's working directory,
// so a summary reads "internal/tui/model.go" instead of consuming a third of its
// character budget on a repeated home-directory prefix. A path outside the
// workspace keeps its absolute form — that it lies elsewhere is the interesting
// part.
func (log *activityLog) relative(path string) string {
	trimmed := strings.TrimSpace(path)
	root := strings.TrimSpace(log.cwd)
	if trimmed == "" || root == "" {
		return log.shorten(trimmed)
	}
	if relative, err := filepath.Rel(root, trimmed); err == nil && !strings.HasPrefix(relative, "..") {
		return log.shorten(relative)
	}
	return log.shorten(trimmed)
}

// shorten caps a single item so one long value cannot crowd out the rest of its
// line.
func (log *activityLog) shorten(value string) string {
	const limit = 120
	// Redact sees the complete value before this presentation copy loses later
	// lines or its tail. Truncating first can cut away the syntax that lets the
	// shared redactor recognize a credential. Keep the established first-line
	// activity policy after that complete-value pass.
	selected := firstLine(redact(value))
	// DisplayField adds the presentation-only split-credential pass after the
	// complete-value redactor has retained the established first-line boundary.
	// Run it before Fields rejoins whitespace-separated credential fragments.
	collapsed := strings.Join(strings.Fields(DisplayField(selected)), " ")
	runes := []rune(collapsed)
	if len(runes) <= limit {
		return collapsed
	}
	return string(runes[:limit]) + "…"
}

func firstLine(value string) string {
	if index := strings.IndexAny(value, "\r\n"); index >= 0 {
		return value[:index]
	}
	return value
}

// firstStringField returns the first non-empty string among the named keys.
func firstStringField(fields map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := fields[key].(string); ok && strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

// summaryEvents renders the log as assistant EventMessage events, one per category.
//
// Several small events rather than one large one, because the resume digest
// truncates each event at 500 characters — a single combined summary would lose
// its tail. Failures come last so they sit closest to the user's new request.
func (log *activityLog) summaryEvents() []sessions.AppendEventInput {
	// An unanswered request is useful history, but is not completed work.
	// Sort call identities so map order cannot reshuffle the resume context.
	ids := make([]string, 0, len(log.pendingPath))
	for id := range log.pendingPath {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		log.retainUnconfirmed(log.pendingPath[id])
	}
	if log == nil || log.calls == 0 {
		return nil
	}

	events := []sessions.AppendEventInput{}
	headline := "Prior session activity: " + countPhrase(log.calls, "tool call")
	if log.failed > 0 {
		headline += ", " + countPhrase(log.failed, "failure")
	}
	headline += "."
	if extra := log.toolBreakdown(); extra != "" {
		headline += " " + extra
	}
	// THE BUDGET APPLIES TO THE ASSEMBLED LINE, NOT TO A PIECE OF IT. Only the
	// breakdown was capped, so the count prefix rode on top of an already
	// full-length tail: a session full of unrecognised tool names produced a
	// 500-character note, which is exactly where sessions.summarizePayload cuts —
	// the mid-sentence truncation maxSummaryEventChars exists to keep off.
	events = append(events, noteEvent(truncateToBudget(headline, maxSummaryEventBytes)))

	for _, section := range []struct {
		label string
		items []string
	}{
		{"Files read", log.read},
		{"Files changed", log.changed},
		{"Commands run", log.commands},
		{"Searched for", log.searches},
		{"Command requests (execution unconfirmed)", log.requestedCommands},
		{"Search requests (execution unconfirmed)", log.requestedSearches},
		{"Failures", log.failures},
	} {
		if line := summaryLine(section.label, section.items); line != "" {
			events = append(events, noteEvent(line))
		}
	}
	return events
}

// toolBreakdown names tools whose arguments yielded nothing, so an unrecognised
// schema degrades to "Also: exec x4" rather than to silence.
//
// It returns the full list and does NOT cap it: the cap belongs to the caller,
// which appends this to a count prefix. Capping here as well would truncate
// twice and leave an ellipsis inside the line as well as at its end.
func (log *activityLog) toolBreakdown() string {
	if len(log.toolCounts) == 0 {
		return ""
	}
	names := make([]string, 0, len(log.toolCounts))
	for name := range log.toolCounts {
		names = append(names, name)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, name := range names {
		count := log.toolCounts[name]
		if count > 1 {
			parts = append(parts, name+" x"+itoaEvents(count))
			continue
		}
		parts = append(parts, name)
	}
	return "Also: " + strings.Join(parts, ", ")
}

// summaryLine renders one category, collapsing to a count once the list grows
// past what is useful or past the character budget. The overflow is always
// stated: a truncated list that looks complete is how a reader concludes the
// other agent touched four files when it touched forty.
func summaryLine(label string, items []string) string {
	if len(items) == 0 {
		return ""
	}
	kept := items
	dropped := 0
	if len(kept) > maxSummaryItems {
		dropped = len(kept) - maxSummaryItems
		kept = kept[:maxSummaryItems]
	}
	for {
		prefix := label + ": "
		line := prefix + strings.Join(kept, ", ")
		suffix := ""
		if dropped > 0 {
			suffix = " (+" + itoaEvents(dropped) + " more)"
			line += suffix
		}
		if len(line) <= maxSummaryEventBytes {
			return line
		}
		if len(kept) <= 1 {
			itemBudget := maxSummaryEventBytes - len(prefix) - len(suffix)
			if itemBudget <= 0 {
				return truncateToBudget(prefix+suffix, maxSummaryEventBytes)
			}
			return prefix + truncateToBudget(kept[0], itemBudget) + suffix
		}
		dropped++
		kept = kept[:len(kept)-1]
	}
}

func truncateToBudget(value string, budget int) string {
	if budget <= 0 {
		return ""
	}
	if len(value) <= budget {
		return value
	}
	const ellipsis = "…"
	if budget < len(ellipsis) {
		return strings.Repeat(".", budget)
	}
	end := budget - len(ellipsis)
	for end > 0 && !utf8.RuneStart(value[end]) {
		end--
	}
	return value[:end] + ellipsis
}

func countPhrase(count int, noun string) string {
	if count == 1 {
		return "1 " + noun
	}
	return itoaEvents(count) + " " + noun + "s"
}
