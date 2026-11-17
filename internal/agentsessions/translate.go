package agentsessions

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/Twigpine/zero/internal/redaction"
	"github.com/Twigpine/zero/internal/sessions"
	"github.com/Twigpine/zero/internal/tools"
)

// The payload field names below are a CONTRACT with the TUI, not a convention.
// internal/tui/session.go's transcriptRowsFromSessionEvents reads exactly these
// keys ("role", "content", "name", "toolCallId", "arguments", "status",
// "output"); a misspelling renders an empty row and reports no error anywhere.
// Every event this package produces is built by one of the four constructors
// here so there is a single place for those names to be right.
//
// These constructors are also the redaction chokepoint (repo invariant #6).
// Imported text is untrusted input (invariant #8) — a foreign transcript can
// contain a key the other agent echoed into its own log — and routing every
// event through here means no future caller can add an unredacted path without
// deleting a call they can see.

// redact runs secret redaction AND control-stripping on imported text. Every
// field a foreign transcript supplies routes through here — the content-bearing
// ones and the structural ones (role, name, toolCallId) alike — because a
// malicious transcript can hide a credential in any of them and Zero then
// persists and renders it. This is the redaction chokepoint (invariant #6): no
// imported byte reaches a picker row or transcript line as a secret or as a live
// control sequence.
// THE ORDER IS THE WHOLE GUARANTEE. Strip first, then match.
//
// RedactString matches secrets by SHAPE, and stripControl deletes a control byte
// without leaving a gap, so it is also a REASSEMBLER. Running it second meant a
// transcript could split a credential with a NUL, an ESC or any C1 byte, sail
// past the shape patterns because neither half looks like a key, and then have
// the halves rejoined on the way out. Every shape leaked that way: sk-ant-,
// ghp_, AKIA. Normalizing first means the patterns see the text after the
// control classes this sanitizer actually removes (C0, DEL, C1, and selected
// format runes). Combining marks and other non-control Unicode remain unchanged
// and are not claimed as part of this redaction boundary.
//
// Same defect as #835, where an MCP failure reason was redacted before the
// terminal sanitizer rejoined its halves. Any normalizer that removes bytes
// without leaving a gap has to run BEFORE whatever matches on them.
//
// AND ALSO AFTER IT — BOTH DIRECTIONS HOLD AT ONCE. Stripping changes what the
// matcher can see in two opposite ways. Removing a control INSIDE a key
// assembles a key the patterns could not see before (the case above). Removing
// a control immediately BEFORE an intact key erases the word boundary every
// pattern anchors on: "progress\rsk-ant-api03-…" was a recognizable key after a
// carriage return, and became "progresssk-ant-api03-…" — a mid-word run that
// \bsk-ant- refuses to match — so the whole key persisted through messageEvent.
// NUL and every other deleted separator reproduce it. DisplayField already ran
// redaction on both sides of normalization for metadata; the transcript
// constructors did not, and a transcript is where a pasted key actually lands.
// Pass one catches the intact key while its separator still stands, pass two
// catches the split key once the separator is gone. Reported by @jatmn.
func redact(value string) string {
	if value == "" {
		return ""
	}
	normalized, boundaries := stripControlWithBoundaries(value)
	return redactAtRemovedBoundaries(normalized, boundaries)
}

func stripControlWithBoundaries(value string) (string, []int) {
	var b strings.Builder
	b.Grow(len(value))
	boundaries := []int{}
	for index := 0; index < len(value); {
		if value[index] == 0x1b {
			boundaries = appendBoundary(boundaries, b.Len())
			index = displayEscapeEnd(value, index)
			continue
		}
		r, size := utf8.DecodeRuneInString(value[index:])
		index += size
		switch {
		case r == '\t' || r == '\n':
			b.WriteRune(r)
		case r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f):
			boundaries = appendBoundary(boundaries, b.Len())
		// FORMAT CHARACTERS ARE NOT CONTROL CHARACTERS, and unicode.IsControl
		// says so — but U+202E RIGHT-TO-LEFT OVERRIDE reorders everything after
		// it, so a tool name or title can be made to render as something else
		// entirely while the bytes stay innocent. Category Cf is invisible by
		// definition; nothing in a transcript needs it.
		case unicode.Is(unicode.Cf, r):
			boundaries = appendBoundary(boundaries, b.Len())
		default:
			b.WriteRune(r)
		}
	}
	return b.String(), boundaries
}

func appendBoundary(boundaries []int, position int) []int {
	if len(boundaries) == 0 || boundaries[len(boundaries)-1] != position {
		return append(boundaries, position)
	}
	return boundaries
}

// redactAtRemovedBoundaries preserves the fact that a removed control separated
// two adjacent bytes. The ordinary post-normalization pass catches credentials
// assembled across an internal control. At a former boundary, a known secret
// prefix also owns the complete adjacent token even when deleting the control
// glued preceding prose to it and erased the redactor's word boundary.
//
// Each boundary and each byte in a matched token is visited at most a constant
// number of times. The previous implementation redacted every remaining suffix
// once per removed byte, making a control-dense accepted value quadratic.
func redactAtRemovedBoundaries(value string, boundaries []int) string {
	const boundaryHint = "\ue000"
	spans := make([]byteSpan, 0)
	coveredThrough := 0
	for _, position := range boundaries {
		if position < coveredThrough || position < 0 || position >= len(value) {
			continue
		}
		if !fullDisplaySecretPrefixAt(value[position:]) {
			continue
		}
		probeEnd := position
		for probeEnd < len(value) && probeEnd-position < maxDisplaySecretProbeBytes && isTextSecretByte(value[probeEnd]) {
			probeEnd++
		}
		candidate := value[position:probeEnd]
		probe := redaction.RedactString(boundaryHint+candidate, redaction.Options{})
		detected := strings.Contains(probe, redaction.RedactedSecret)
		if !detected && len(candidate) >= maxDisplaySecretProbeBytes {
			// A supported delayed-match shape (notably a long JWT) can still be
			// incomplete at the work cap. Abandoning it would fail open.
			detected = true
		}
		if !detected {
			continue
		}
		end := probeEnd
		for end < len(value) && isTextSecretByte(value[end]) {
			end++
		}
		spans = append(spans, byteSpan{start: position, end: end})
		coveredThrough = end
	}
	for index := len(spans) - 1; index >= 0; index-- {
		span := spans[index]
		value = value[:span.start] + redaction.RedactedSecret + value[span.end:]
	}
	return redaction.RedactString(value, redaction.Options{})
}

type byteSpan struct {
	start int
	end   int
}

// Every fixed-prefix text credential reaches its minimum recognizable shape
// within 40 joined bytes. The larger cap leaves room for OpenAI's digit-based
// false-positive filter while making work per possible start independent of the
// discovery-line size. An unresolved sk- candidate at the cap fails closed.
const maxDisplaySecretProbeBytes = 256

var displaySecretPrefixes = []string{
	"sk-", "github_pat_", "ghp_", "gho_", "ghu_", "ghs_", "ghr_",
	"glpat-", "AIza", "xoxb-", "xoxa-", "xoxp-", "xoxr-", "xoxs-",
	"AKIA", "ASIA", "eyJ",
}

// redactDisplaySpaceSplits catches credentials whose bytes were separated by
// layout controls. DisplayField keeps those controls as a space for legibility,
// but an attacker can put one inside a key: the ordinary matcher then redacts a
// recognizable prefix and leaves the suffix on screen. Ask the shared redactor
// whether the adjacent ASCII fragments form a known secret, then replace the
// complete original span (including the display space).
//
// It also remaps the remaining removed-control boundaries. Their byte offsets
// feed redactAtRemovedBoundaries, so leaving offsets from before a replacement
// could redact unrelated text later in the field.
func redactDisplaySpaceSplits(value string, boundaries []int) (string, []int) {
	const boundaryHint = "\ue000"
	return redactDisplaySpaceSplitsWithProbe(value, boundaries, func(candidate string) bool {
		probe := redaction.RedactString(boundaryHint+candidate, redaction.Options{})
		return strings.Contains(probe, redaction.RedactedSecret) ||
			(len(candidate) >= maxDisplaySecretProbeBytes && fullDisplaySecretPrefixAt(candidate))
	})
}

func redactDisplaySpaceSplitsWithProbe(value string, boundaries []int, detectsSecret func(string) bool) (string, []int) {
	type fragment struct{ start, end int }
	fragments := []fragment{}
	for index := 0; index < len(value); {
		if !isTextSecretByte(value[index]) {
			index++
			continue
		}
		start := index
		for index < len(value) && isTextSecretByte(value[index]) {
			index++
		}
		fragments = append(fragments, fragment{start: start, end: index})
	}

	spans := []byteSpan{}
	for start := 0; start < len(fragments); start++ {
		var candidate strings.Builder
		for end := start; end < len(fragments); end++ {
			if end > start {
				gap := value[fragments[end-1].end:fragments[end].start]
				if strings.Trim(gap, " ") != "" {
					break
				}
			}
			fragmentText := value[fragments[end].start:fragments[end].end]
			remaining := maxDisplaySecretProbeBytes - candidate.Len()
			if len(fragmentText) > remaining {
				fragmentText = fragmentText[:remaining]
			}
			candidate.WriteString(fragmentText)
			probeText := candidate.String()
			if !displaySecretPrefixPossible(probeText) {
				break
			}
			detected := detectsSecret(probeText)
			if !detected {
				if candidate.Len() >= maxDisplaySecretProbeBytes {
					break
				}
				continue
			}

			// Once a secret-shaped prefix is recognized, consume following long
			// fragments separated only by layout. Leaving those fragments visible
			// recreates the split-key leak; a short prose word after the key remains
			// readable and cannot expose the eight-byte run this boundary protects.
			last := end
			for next := end + 1; next < len(fragments); next++ {
				gap := value[fragments[next-1].end:fragments[next].start]
				if strings.Trim(gap, " ") != "" || fragments[next].end-fragments[next].start < 8 {
					break
				}
				last = next
			}
			spans = append(spans, byteSpan{start: fragments[start].start, end: fragments[last].end})
			start = last
			break
		}
	}
	if len(spans) == 0 {
		return value, boundaries
	}

	// Boundaries are discovered left-to-right, so overlapping spans can only
	// extend the previous span. Merging first prevents duplicate markers when a
	// credential contains more than one layout control.
	merged := spans[:0]
	for _, span := range spans {
		if len(merged) > 0 && span.start <= merged[len(merged)-1].end {
			if span.end > merged[len(merged)-1].end {
				merged[len(merged)-1].end = span.end
			}
			continue
		}
		merged = append(merged, span)
	}

	for i := len(merged) - 1; i >= 0; i-- {
		span := merged[i]
		value = value[:span.start] + redaction.RedactedSecret + value[span.end:]
		delta := len(redaction.RedactedSecret) - (span.end - span.start)
		for j, position := range boundaries {
			switch {
			case position >= span.start && position <= span.end:
				boundaries[j] = -1
			case position > span.end:
				boundaries[j] += delta
			}
		}
	}
	kept := boundaries[:0]
	for _, position := range boundaries {
		if position >= 0 {
			kept = append(kept, position)
		}
	}
	return value, kept
}

func displaySecretPrefixPossible(candidate string) bool {
	for _, prefix := range displaySecretPrefixes {
		if strings.HasPrefix(prefix, candidate) || strings.HasPrefix(candidate, prefix) {
			return true
		}
	}
	return false
}

func fullDisplaySecretPrefixAt(candidate string) bool {
	for _, prefix := range displaySecretPrefixes {
		if strings.HasPrefix(candidate, prefix) {
			return true
		}
	}
	return false
}

func isTextSecretByte(value byte) bool {
	return value >= 'a' && value <= 'z' ||
		value >= 'A' && value <= 'Z' ||
		value >= '0' && value <= '9' ||
		value == '_' || value == '-' || value == '.'
}

// displayEscapeEnd returns the first byte after one ANSI CSI or OSC sequence.
// A foreign title can contain a real escape such as ESC[31m; deleting only ESC
// leaves the printable "[31m" bytes between credential fragments and defeats
// the post-normalization matcher. Other ESC spellings retain their printable
// byte and only lose the introducer, matching the prior behavior.
func displayEscapeEnd(value string, start int) int {
	if start+1 >= len(value) {
		return start + 1
	}
	switch value[start+1] {
	case '[':
		for index := start + 2; index < len(value); index++ {
			if value[index] >= 0x40 && value[index] <= 0x7e {
				return index + 1
			}
		}
		return len(value)
	case ']':
		for index := start + 2; index < len(value); index++ {
			if value[index] == '\a' {
				return index + 1
			}
			if value[index] == 0x1b && index+1 < len(value) && value[index+1] == '\\' {
				return index + 2
			}
		}
		return len(value)
	default:
		return start + 1
	}
}

// Every event a translation produces carries sessions.ImportedEventKey. The
// resume digest keeps only the last 80 eligible events, so the boundary note
// persisted ahead of the transcript aged out of the window on the first resume
// of any import longer than that, while the foreign turns it was labelling
// stayed. The marker is what lets FormatExecPrompt tell, from the retained
// window alone, that what it is about to hand the model is foreign — and
// regenerate the label rather than depend on one event surviving truncation,
// compaction, or a fork. Reported by @jatmn.
func messageEvent(role string, content string) sessions.AppendEventInput {
	return sessions.AppendEventInput{
		Type: sessions.EventMessage,
		Payload: map[string]any{
			"role":                    redact(role),
			"content":                 redact(content),
			sessions.ImportedEventKey: true,
		},
	}
}

type importCallIdentities struct {
	key []byte
}

func (identities *importCallIdentities) opaque(foreign string) string {
	if len(identities.key) == 0 {
		identities.key = []byte(rand.Text())
	}
	digest := hmac.New(sha256.New, identities.key)
	_, _ = digest.Write([]byte(foreign))
	return fmt.Sprintf("import-call-%x", digest.Sum(nil))
}

func toolCallEvent(identities *importCallIdentities, name string, foreignCallID string, arguments string) sessions.AppendEventInput {
	return sessions.AppendEventInput{
		Type: sessions.EventToolCall,
		Payload: map[string]any{
			"name": redact(name),
			// Identity is structural, not display text. Foreign ids may contain
			// secrets, and redaction is deliberately many-to-one, so persisting a
			// redacted foreign id can collapse distinct call/result pairs. This
			// per-import opaque id is non-secret and one-to-one.
			"toolCallId":              identities.opaque(foreignCallID),
			"arguments":               redactArguments(arguments),
			sessions.ImportedEventKey: true,
		},
	}
}

// redactArguments sanitizes a tool call's arguments as the VALUES a consumer
// will decode, not as the bytes they are serialized in.
//
// arguments is JSON from every adapter that has structured calls, and JSON
// escapes are a representation the sanitizer above cannot see through: a
// foreign path of "\u001b[2J FORGED \u0067hp_AAAA…" contains no ESC byte and
// no recognizable key prefix while it is encoded, so redact() passed it whole —
// and the TUI's argHint → firstArgValue then json-decoded it on resume into an
// actual escape followed by a complete PAT, in the tool row. A scan of the
// encoded text can prove nothing about strings that will be unescaped later.
// The sanitizer has to run where the value exists: decode, redact every string
// leaf (nested included — an argument object is routinely a tree), re-encode.
// Reported by @jatmn.
//
// Not JSON — Codex's custom_tool_call carries a bare script in "input" — is
// text and is sanitized as text, exactly as before. A JSON value that is not an
// object or array (a bare string) decodes to its leaf and is handled the same
// way. The encoded result stays valid JSON for its existing consumers.
func redactArguments(arguments string) string {
	trimmed := strings.TrimSpace(arguments)
	if trimmed == "" {
		return ""
	}
	var decoded any
	if err := json.Unmarshal([]byte(trimmed), &decoded); err != nil {
		return redact(arguments)
	}
	// First normalize every decoded key and value using the imported-text policy,
	// then retain the complete object shape while applying sensitive-key rules.
	// A leaf-only walk cannot know that an opaque value belongs to "password",
	// and leaving map keys untouched can persist a credential in a property name.
	sanitized := redaction.RedactValue(redactJSONValue(decoded), redaction.Options{})
	encoded, err := json.Marshal(sanitized)
	if err != nil {
		return redact(arguments)
	}
	return string(encoded)
}

// redactJSONValue walks a decoded JSON value and applies imported-text
// normalization to string leaves and object keys. Ordinary schema keys remain
// unchanged; credential-bearing keys are intentionally rewritten before the
// object-aware redactor handles sensitive key/value relationships.
func redactJSONValue(value any) any {
	switch typed := value.(type) {
	case string:
		return redact(typed)
	case []any:
		for index := range typed {
			typed[index] = redactJSONValue(typed[index])
		}
		return typed
	case map[string]any:
		redacted := make(map[string]any, len(typed))
		for key := range typed {
			redacted[redact(key)] = redactJSONValue(typed[key])
		}
		return redacted
	default:
		return value
	}
}

func toolResultEvent(identities *importCallIdentities, name string, foreignCallID string, status tools.Status, output string) sessions.AppendEventInput {
	return sessions.AppendEventInput{
		Type: sessions.EventToolResult,
		Payload: map[string]any{
			"name":                    redact(name),
			"toolCallId":              identities.opaque(foreignCallID),
			"status":                  string(status),
			"output":                  redact(output),
			sessions.ImportedEventKey: true,
		},
	}
}

// noteEventSummaryKey marks a message as a Zero-generated activity summary
// rather than a translated foreign-transcript turn. The TUI and the resume
// digest also reads "role" and "content", so the marker itself is metadata even
// though the generated summary remains visible to both the user and the model.
// NoteEventIsSummary lets consumers distinguish it from a foreign turn.
const noteEventSummaryKey = "importedActivitySummary"

// importBoundaryKey is owned by internal/sessions rather than here, because the
// resume digest has to recognize the boundary without importing this package.
const importBoundaryKey = sessions.ImportedBoundaryKey

func importBoundaryEvent(agentName string) sessions.AppendEventInput {
	return sessions.AppendEventInput{
		Type: sessions.EventMessage,
		Payload: map[string]any{
			"role":            "user",
			"content":         sessions.ImportedBoundaryText(DisplayField(agentName)),
			importBoundaryKey: true,
		},
	}
}

// NoteEventIsBoundary reports whether an event is Zero's generated trust
// boundary rather than content copied from the foreign transcript.
func NoteEventIsBoundary(payload any) bool {
	m, ok := payload.(map[string]any)
	if !ok {
		return false
	}
	flag, _ := m[importBoundaryKey].(bool)
	return flag
}

func hasImportableSourceContent(events []sessions.AppendEventInput) bool {
	for _, event := range events {
		switch event.Type {
		case sessions.EventToolCall, sessions.EventToolResult:
			return true
		case sessions.EventMessage:
			if NoteEventIsSummary(event.Payload) || NoteEventIsBoundary(event.Payload) {
				continue
			}
			payload, ok := event.Payload.(map[string]any)
			if !ok {
				encoded, err := json.Marshal(event.Payload)
				if err != nil || json.Unmarshal(encoded, &payload) != nil {
					continue
				}
			}
			role, _ := payload["role"].(string)
			content, _ := payload["content"].(string)
			if (role == "user" || role == "assistant") && strings.TrimSpace(content) != "" {
				return true
			}
		}
	}
	return false
}

// NoteEventIsSummary reports whether an event is a Zero-generated activity
// summary message (see noteEvent) rather than a translated transcript turn. It
// takes any so callers can pass an AppendEventInput.Payload directly.
func NoteEventIsSummary(payload any) bool {
	m, ok := payload.(map[string]any)
	if !ok {
		return false
	}
	flag, _ := m[noteEventSummaryKey].(bool)
	return flag
}

// noteEvent carries an imported-session activity summary as an assistant
// message. NOT EventCompaction: that type has a second contract on the replay
// side. RehydrateEvents restructures the transcript around the last
// EventCompaction, and an activity summary with no CompactionPayload bookkeeping
// (no CompactableEvents, CompactedThroughSequence 0) makes rehydration hoist
// this note to the FRONT of the transcript. EventMessage still passes
// promptContextEvents — the resume digest — without that restructuring. The
// summary marker keeps it distinguishable from a real assistant turn.
func noteEvent(summary string) sessions.AppendEventInput {
	return sessions.AppendEventInput{
		Type: sessions.EventMessage,
		Payload: map[string]any{
			"role":                    "assistant",
			"content":                 redact(summary),
			noteEventSummaryKey:       true,
			sessions.ImportedEventKey: true,
		},
	}
}

// translateFamily1 converts a family-1 transcript into Zero events.
//
// The mapping is deliberately lossy in one direction only: everything that
// affects what a reader (human or model) needs in order to continue the work is
// kept, and everything that belongs to the other model's private machinery is
// dropped. Zero's own resume renders these events to a text digest anyway
// (sessions.FormatExecPrompt), so perfect structural fidelity would buy nothing.
func translateFamily1(file readSeekStater, options ReadOptions) ([]sessions.AppendEventInput, error) {
	events := newEventTail(effectiveMaxEvents(options.MaxEvents))
	// A tool result names only the id of the call it answers, so the call's name
	// has to be carried forward. Every family-1 agent writes the tool_use before
	// the matching tool_result, so this is populated by the time it is read.
	toolNames := map[string]string{}
	identities := &importCallIdentities{}
	activity := newActivityLog(options.Cwd)

	omitted := 0
	prefixOmitted, err := streamTailLines(file, importLineLimit, importByteLimit, func(line []byte, truncated bool) bool {
		// A RECORD TOO LONG EVEN FOR THE IMPORT CAP IS REPORTED, NOT DROPPED.
		// Skipping it silently produced a transcript that looked complete: a
		// question, no answer, then the follow-up. The marker is the honest
		// answer — the bytes are gone either way, but the reader can see it.
		if truncated {
			omitted++
			return true
		}
		var record family1Record
		if json.Unmarshal(line, &record) != nil || record.Message == nil {
			// Torn or unrecognised lines are skipped, not fatal: transcripts are
			// appended live and the final line is routinely half-written.
			return true
		}
		role := roleFor(record)
		if role == "" {
			return true
		}

		// Content is either a bare string (a plain user prompt) or an array of
		// typed blocks.
		var text string
		if json.Unmarshal(record.Message.Content, &text) == nil {
			if strings.TrimSpace(text) != "" {
				events.add(messageEvent(role, text))
			}
			return true
		}

		var blocks []family1Block
		if json.Unmarshal(record.Message.Content, &blocks) != nil {
			return true
		}
		for _, block := range blocks {
			switch block.Type {
			case "text":
				if strings.TrimSpace(block.Text) != "" {
					events.add(messageEvent(role, block.Text))
				}
			case "thinking":
				// The other model's reasoning. Dropped by default: it is private
				// to that provider, frequently larger than the visible
				// conversation, and a different model continuing this work will
				// not be picking up that chain of thought.
				if options.IncludeReasoning && strings.TrimSpace(block.Thinking) != "" {
					events.add(messageEvent("reasoning", block.Thinking))
				}
			case "tool_use":
				toolNames[block.ID] = block.Name
				activity.observeCall(block.ID, block.Name, string(block.Input))
				events.add(toolCallEvent(identities, block.Name, block.ID, string(block.Input)))
			case "tool_result":
				name := toolNames[block.ToolUseID]
				if name == "" {
					name = "unknown"
				}
				status := tools.StatusOK
				if block.IsError {
					status = tools.StatusError
				}
				output := family1ResultText(block.Content)
				activity.observeResult(block.ToolUseID, name, status, output)
				events.add(toolResultEvent(identities, name, block.ToolUseID, status, output))
				delete(toolNames, block.ToolUseID)
			}
		}
		return true
	})
	if err != nil {
		return nil, err
	}
	// SAID OUT LOUD. A resumed conversation that quietly lost a record reads as
	// complete to both the user and the model continuing it — the failure this
	// makes visible is a question with no answer followed by a follow-up.
	contextEvents := activity.summaryEvents()
	if prefixOmitted {
		contextEvents = append(contextEvents, omittedPrefixEvent())
	}
	if omitted > 0 {
		contextEvents = append(contextEvents, omittedRecordsEvent(omitted))
	}
	return capTranslatedEventsDropped(events.values(), contextEvents, effectiveMaxEvents(options.MaxEvents), events.dropped), nil
}

// roleFor admits only visible conversation roles. System, developer, and other
// harness records belong to the foreign agent and must not become instructions
// or user-visible turns in Zero.
func roleFor(record family1Record) string {
	if record.Message == nil {
		return ""
	}
	switch role := strings.ToLower(strings.TrimSpace(record.Message.Role)); role {
	case "user", "assistant":
		return role
	default:
		return ""
	}
}

// family1ResultText flattens a tool result's content, which may be a bare string
// or an array of blocks.
func family1ResultText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	if flattened := family1Text(raw); flattened != "" {
		return flattened
	}
	// Structured content with no text blocks (an image result, say). Keeping the
	// raw JSON is better than an empty row: the reader at least learns that the
	// call returned something and what shape it was.
	return string(raw)
}

// capEvents keeps the LAST max events, because the tail is what a resume needs
// — the most recent exchanges describe where the work actually stopped.
//
// The drop is announced rather than silent. A truncated import that looks
// complete is how someone concludes the other agent never did the work.
func capEvents(events []sessions.AppendEventInput, max int) []sessions.AppendEventInput {
	return capEventsDropped(events, max, 0)
}

func capEventsDropped(events []sessions.AppendEventInput, max int, alreadyDropped int) []sessions.AppendEventInput {
	if alreadyDropped == 0 && (max <= 0 || len(events) <= max) {
		return events
	}
	if max <= 0 {
		out := []sessions.AppendEventInput{noteEvent(plural(alreadyDropped, "earlier event") +
			" from this session were not imported; all retained events are shown.")}
		return append(out, events...)
	}
	if max == 1 {
		// A one-event budget cannot disclose loss and retain source content. Keep
		// the latest source event, matching the public tail guarantee.
		if len(events) > 0 {
			return []sessions.AppendEventInput{events[len(events)-1]}
		}
		return []sessions.AppendEventInput{noteEvent(plural(alreadyDropped, "earlier event") + " from this session were not imported.")}
	}
	// The note itself occupies one of the max slots, so one more original event
	// (the oldest of the tail) is dropped to make room for it. The reported
	// count must include that event: len(events)-max alone understates the loss
	// by one, and a truncation that reads as smaller than it was is how someone
	// concludes the other agent did less than it did.
	shownCount := min(len(events), max-1)
	shown := events[len(events)-shownCount:]
	baseShown := len(shown)
	shown, orphaned := withoutOrphanToolResults(shown)
	dropped := alreadyDropped + len(events) - baseShown + orphaned
	out := make([]sessions.AppendEventInput, 0, max)
	out = append(out, noteEvent(plural(dropped, "earlier event")+
		" from this session were not imported; the most recent "+
		itoaEvents(len(shown))+" are shown."))
	return append(out, shown...)
}

// capTranslatedEvents applies MaxEvents without allowing generated summaries
// to evict the actual transcript tail. Context receives spare/reserved slots,
// but at least the final source event always survives when source exists.
func capTranslatedEventsDropped(source, contextEvents []sessions.AppendEventInput, max int, alreadyDropped int) []sessions.AppendEventInput {
	// Re-establish call/result structure after every lossy source boundary. A
	// byte-tail read can omit the call while retaining its result even when the
	// event cap never fires; event-cap loss is already represented by
	// alreadyDropped and includes paired results removed here.
	var orphaned int
	source, orphaned = withoutOrphanToolResults(source)
	alreadyDropped += orphaned
	if alreadyDropped == 0 && (max <= 0 || len(source)+len(contextEvents) <= max) {
		return append(append([]sessions.AppendEventInput{}, source...), contextEvents...)
	}
	if len(source) == 0 {
		return capEvents(contextEvents, max)
	}
	if len(contextEvents) == 0 {
		if max == 1 {
			// A one-event budget cannot hold both a disclosure and source content.
			// Preserve the promised transcript tail instead of returning only the
			// generated omission marker.
			return []sessions.AppendEventInput{source[len(source)-1]}
		}
		return capEventsDropped(source, max, alreadyDropped)
	}
	contextSlots := min(len(contextEvents), max-1)
	if contextSlots < 0 {
		contextSlots = 0
	}
	sourceSlots := max - contextSlots
	// If source must be truncated and the budget has room, reserve a second
	// source slot for capEvents' disclosure note. Keeping only the final source
	// event would satisfy the tail guarantee while silently hiding that earlier
	// transcript events were dropped.
	if (len(source) > sourceSlots || alreadyDropped > 0) && max >= 2 && sourceSlots < 2 {
		sourceSlots = 2
		contextSlots = max - sourceSlots
	}
	var keptSource []sessions.AppendEventInput
	if sourceSlots <= 1 {
		// A one-event budget cannot hold both a disclosure and source content.
		// The flag promises transcript-tail events, so the final source event wins;
		// larger budgets retain the explicit omission marker below.
		keptSource = append(keptSource, source[len(source)-1])
	} else {
		keptSource = capEventsDropped(source, sourceSlots, alreadyDropped)
	}
	return append(keptSource, contextEvents[len(contextEvents)-contextSlots:]...)
}

const (
	defaultImportMaxEvents = 4096
	importByteLimit        = 32 << 20
)

func effectiveMaxEvents(requested int) int {
	if requested > 0 {
		return requested
	}
	return defaultImportMaxEvents
}

type eventTail struct {
	events  []sessions.AppendEventInput
	max     int
	start   int
	dropped int
}

func newEventTail(max int) *eventTail {
	return &eventTail{
		events: make([]sessions.AppendEventInput, 0, min(max, 128)),
		max:    max,
	}
}

func (tail *eventTail) add(event sessions.AppendEventInput) {
	if len(tail.events) < tail.max {
		tail.events = append(tail.events, event)
		return
	}
	tail.events[tail.start] = event
	tail.start = (tail.start + 1) % len(tail.events)
	tail.dropped++
}

func (tail *eventTail) values() []sessions.AppendEventInput {
	if tail.start == 0 {
		return tail.events
	}
	out := make([]sessions.AppendEventInput, 0, len(tail.events))
	out = append(out, tail.events[tail.start:]...)
	return append(out, tail.events[:tail.start]...)
}

func withoutOrphanToolResults(events []sessions.AppendEventInput) ([]sessions.AppendEventInput, int) {
	calls := map[string]bool{}
	out := make([]sessions.AppendEventInput, 0, len(events))
	dropped := 0
	for _, event := range events {
		payload, _ := event.Payload.(map[string]any)
		id, _ := payload["toolCallId"].(string)
		if event.Type == sessions.EventToolCall {
			calls[id] = true
		}
		if event.Type == sessions.EventToolResult {
			if !calls[id] {
				dropped++
				continue
			}
		}
		out = append(out, event)
	}
	return out, dropped
}

func omittedPrefixEvent() sessions.AppendEventInput {
	return noteEvent("Older transcript records were not imported; only a bounded tail of this foreign session was read.")
}

func itoaEvents(value int) string { return strconv.Itoa(value) }

func plural(count int, noun string) string {
	if count == 1 {
		return "1 " + noun
	}
	return itoaEvents(count) + " " + noun + "s"
}

// omittedRecordsEvent names what an import could not carry across.
//
// It is an EventError rather than a message because it is not part of the
// conversation and must not read as one: a model continuing this session should
// see a note about the transcript, not a turn somebody took. The count is the
// honest limit of what can be said — the bytes were never parsed, so their role,
// author and content are all unknown.
func omittedRecordsEvent(count int) sessions.AppendEventInput {
	noun := "record"
	if count != 1 {
		noun = "records"
	}
	return sessions.AppendEventInput{
		Type: sessions.EventError,
		Payload: map[string]any{
			"message": fmt.Sprintf("%d %s in the source transcript exceeded the import size limit and could not be read. "+
				"This imported conversation is missing that content.", count, noun),
		},
	}
}

// DisplayField makes one foreign metadata value safe to draw in a terminal.
//
// TWO SEPARATE HAZARDS. The value is a field another product
// wrote into its own file: it can carry terminal escapes that repaint the rows
// around it, and it can carry something shaped like a credential — a title is
// often the user's first prompt, which is where a pasted key ends up.
//
// Controls are normalized before redaction so a secret split by one cannot
// evade the shape match.
// Layout goes as well, unlike the transcript
// helper, because a metadata field is drawn as one row and a newline in it moves
// the rest of the line somewhere the caller did not intend.
//
// TAB, NEWLINE AND RETURN BECOME A SPACE RATHER THAN VANISHING, and that gap is
// load-bearing in the opposite direction to the stripping above. Every secret
// pattern anchors on \b, so deleting the separator in "key<TAB>sk-ant-…" glued a
// word character onto the shape and the match no longer fired — a title is
// usually the user's first prompt, and a pasted key on the line after "key:" is
// exactly how one arrives. Substituting keeps the field one row while leaving the
// boundary the patterns need. It costs nothing that was being protected:
// redaction_order_test.go already establishes that a credential cannot contain a
// raw newline, so joining across one never reassembled a real secret. The
// invisible bytes — C0, DEL, C1, Cf — are still DELETED, because those are the
// ones an escape can hide inside a key.
func DisplayField(value string) string {
	var b strings.Builder
	b.Grow(len(value))
	boundaries := []int{}
	for index := 0; index < len(value); {
		if value[index] == 0x1b {
			boundaries = appendBoundary(boundaries, b.Len())
			index = displayEscapeEnd(value, index)
			continue
		}
		r, size := utf8.DecodeRuneInString(value[index:])
		index += size
		if r == '\t' || r == '\n' || r == '\r' {
			boundaries = appendBoundary(boundaries, b.Len())
			b.WriteRune(' ')
			continue
		}
		// Cf as well as control: see stripControl. A bidi override in a picker row
		// reorders the rows's visible text without changing a byte of it.
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			boundaries = appendBoundary(boundaries, b.Len())
			continue
		}
		b.WriteRune(r)
	}
	normalized, boundaries := redactDisplaySpaceSplits(b.String(), boundaries)
	return strings.TrimSpace(redactAtRemovedBoundaries(normalized, boundaries))
}
