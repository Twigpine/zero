package agentsessions

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/Twigpine/zero/internal/sessions"
	"github.com/Twigpine/zero/internal/tools"
)

// codex reads Codex's rollout transcripts.
//
// Same idea as family 1, two differences that stop it sharing the code:
//
//   - The store is partitioned by DATE, not by working directory:
//     ~/.codex/sessions/YYYY/MM/DD/rollout-<iso>-<uuid>.jsonl. There is no slug
//     to narrow with, so scoping to a workspace always means indexing the lot —
//     affordable only because indexing is a bounded head read.
//   - Every record wraps its real content in a "payload" object, and the record
//     kind that matters is payload.type rather than the outer type.
type codex struct {
	root string
}

// Codex reads Codex's rollout transcripts.
func Codex(env Env) Adapter {
	return codex{root: codexRoot(env)}
}

func (adapter codex) Name() string { return "codex" }

// transcripts lists rollout files at exactly three levels below the root
// (year/month/day). Fixed depth for the same reason as everywhere else in this
// package: ~/.codex/auth.json holds a live OPENAI_API_KEY and an OAuth token,
// and a walk from the codex home would reach it.
func (adapter codex) transcripts() []string {
	if strings.TrimSpace(adapter.root) == "" {
		return nil
	}
	return globTranscripts(adapter.root, filepath.Join(adapter.root, "*", "*", "*", "rollout-*"+transcriptExt))
}

func (adapter codex) Discover(cwd string) ([]ForeignSession, error) {
	found := []ForeignSession{}
	for _, path := range adapter.transcripts() {
		session, ok := indexCodexTranscript(adapter.Name(), adapter.root, path)
		if !ok {
			continue
		}
		if strings.TrimSpace(cwd) != "" && !sameDir(cwd, session.Cwd) {
			continue
		}
		found = append(found, session)
	}
	sortByRecency(found)
	return found, nil
}

func (adapter codex) Read(source ForeignSession, options ReadOptions) ([]sessions.AppendEventInput, error) {
	if source.Agent != adapter.Name() || source.ID != codexID(source.Path) {
		return nil, errors.New("agentsessions: selected session does not belong to codex")
	}
	// One handle from identity check to final check: see openSelectedSource.
	file, err := openSelectedSource(adapter.root, source)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	events, err := translateCodex(file, options)
	if err != nil {
		return nil, err
	}
	if err := validateSourceHandle(file, source); err != nil {
		return nil, err
	}
	return events, nil
}

type codexRecord struct {
	Type      string       `json:"type"`
	Timestamp string       `json:"timestamp"`
	Payload   codexPayload `json:"payload"`
}

// codexPayload is the union of the payload shapes this adapter reads:
// session_meta, turn_context, and the several response_item kinds.
//
// Any field whose TYPE differs between those shapes must be json.RawMessage and
// be decoded at the point of use. "summary" is the cautionary example: on a
// reasoning item it is an array of blocks, on turn_context it is the plain
// string "auto". Typing it as []codexBlock made encoding/json reject the entire
// turn_context record — so the adapter silently lost the model id on every
// session, with no error anywhere, because one unrelated field disagreed.
type codexPayload struct {
	// session_meta
	SessionID string `json:"session_id"`
	Cwd       string `json:"cwd"`
	// turn_context
	Model string `json:"model"`
	// response_item
	Type      string          `json:"type"`
	Role      string          `json:"role"`
	Content   []codexBlock    `json:"content"`
	Summary   json.RawMessage `json:"summary"`
	Name      string          `json:"name"`
	CallID    string          `json:"call_id"`
	Arguments string          `json:"arguments"`
	Input     string          `json:"input"`
	Output    json.RawMessage `json:"output"`
}

// codexContextTags are the wrappers Codex uses to inject harness state as if it
// were a user turn. They are the same category as its "developer" messages —
// the harness talking to itself — and are excluded from titles and from the
// imported conversation.
//
// An explicit list rather than "any message starting with '<'": a user pasting
// XML is saying something real, and guessing would silently drop their words.
var codexContextTags = []string{"<environment_context>"}

func isCodexContextInjection(text string) bool {
	trimmed := strings.TrimSpace(text)
	for _, tag := range codexContextTags {
		if strings.HasPrefix(trimmed, tag) {
			return true
		}
	}
	return false
}

type codexBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// codexUUID matches the session id Codex appends to every rollout filename.
var codexUUID = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// codexID is the session id, taken from the tail of the filename.
//
// The full base name — rollout-2026-07-18T11-39-15-019f73d7-... — is not
// something anyone wants to retype, and its trailing uuid is exactly the
// session_id recorded inside the file. Falling back to the whole base name keeps
// a renamed or reshaped file addressable rather than invisible.
func codexID(path string) string {
	base := transcriptID(path)
	if found := codexUUID.FindString(base); found != "" {
		return found
	}
	return base
}

func indexCodexTranscript(agent string, root string, path string) (ForeignSession, bool) {
	session := ForeignSession{Agent: agent, ID: codexID(path), Path: path}
	firstPrompt := ""

	_, snapshot, err := scanHeadSnapshot(root, path, defaultHeadLimit, func(line []byte, _ bool) bool {
		var record codexRecord
		if json.Unmarshal(line, &record) != nil {
			return true
		}
		if session.Cwd == "" {
			session.Cwd = record.Payload.Cwd
		}
		if session.ModelID == "" {
			session.ModelID = record.Payload.Model
		}
		if session.StartedAt.IsZero() {
			session.StartedAt = parseTimestamp(record.Timestamp)
		}
		// The first thing the USER said, not the first message: Codex opens every
		// session with several large "developer" messages carrying its own system
		// instructions, and titling a session with those would label them all the
		// same.
		if firstPrompt == "" && record.Payload.Type == "message" && strings.EqualFold(record.Payload.Role, "user") {
			if text := codexBlocksText(record.Payload.Content); !isCodexContextInjection(text) {
				firstPrompt = text
			}
		}
		return true
	})
	if err != nil {
		return ForeignSession{}, false
	}

	session.Title = summarizeTitle(firstPrompt)
	if strings.TrimSpace(session.Cwd) == "" {
		return ForeignSession{}, false
	}
	session.source = snapshot
	session.UpdatedAt = snapshot.modTime
	if session.StartedAt.IsZero() {
		session.StartedAt = session.UpdatedAt
	}
	return session, true
}

func translateCodex(file readSeekStater, options ReadOptions) ([]sessions.AppendEventInput, error) {
	return translateCodexWithByteLimit(file, options, importByteLimit)
}

// translateCodexWithByteLimit keeps the production cap fixed while allowing a
// regression test to exercise tail truncation without allocating 32 MiB.
func translateCodexWithByteLimit(file readSeekStater, options ReadOptions, maxBytes int) ([]sessions.AppendEventInput, error) {
	events := newEventTail(effectiveMaxEvents(options.MaxEvents))
	toolNames := map[string]string{}
	identities := &importCallIdentities{}
	activity := newActivityLog(options.Cwd)

	omitted := 0
	prefixOmitted, err := streamTailLines(file, importLineLimit, maxBytes, func(line []byte, truncated bool) bool {
		// A RECORD TOO LONG EVEN FOR THE IMPORT CAP IS REPORTED, NOT DROPPED.
		// Skipping it silently produced a transcript that looked complete: a
		// question, no answer, then the follow-up. The marker is the honest
		// answer — the bytes are gone either way, but the reader can see it.
		if truncated {
			omitted++
			return true
		}
		var record codexRecord
		if json.Unmarshal(line, &record) != nil || record.Type != "response_item" {
			return true
		}
		payload := record.Payload

		switch payload.Type {
		case "message":
			// "developer" messages are Codex's own system prompt — permissions
			// boilerplate, collaboration-mode text, tool instructions. They are
			// the harness talking to itself, not the work, and importing them
			// would bury the conversation in another product's prompt.
			role := strings.ToLower(strings.TrimSpace(payload.Role))
			if role != "user" && role != "assistant" {
				return true
			}
			text := codexBlocksText(payload.Content)
			if strings.TrimSpace(text) == "" || isCodexContextInjection(text) {
				return true
			}
			events.add(messageEvent(role, text))
		case "reasoning":
			if options.IncludeReasoning {
				var summary []codexBlock
				if json.Unmarshal(payload.Summary, &summary) == nil {
					if text := codexBlocksText(summary); strings.TrimSpace(text) != "" {
						events.add(messageEvent("reasoning", text))
					}
				}
			}
		case "function_call", "custom_tool_call":
			// The two call shapes differ only in where the arguments live.
			toolNames[payload.CallID] = payload.Name
			arguments := firstNonBlank(payload.Arguments, payload.Input)
			activity.observeCall(payload.CallID, payload.Name, arguments)
			events.add(toolCallEvent(identities, payload.Name, payload.CallID, arguments))
		case "function_call_output", "custom_tool_call_output":
			name := toolNames[payload.CallID]
			if name == "" {
				name = "unknown"
			}
			// Codex records no structured success flag on an output. Preserve that
			// uncertainty instead of turning arbitrary output text into either a
			// success claim or a brittle error heuristic.
			activity.observeResult(payload.CallID, name, tools.StatusUnknown, "")
			events.add(toolResultEvent(identities, name, payload.CallID, tools.StatusUnknown, codexOutputText(payload.Output)))
			delete(toolNames, payload.CallID)
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

// codexBlocksText flattens input_text/output_text blocks to plain text.
func codexBlocksText(blocks []codexBlock) string {
	parts := []string{}
	for _, block := range blocks {
		if strings.TrimSpace(block.Text) != "" {
			parts = append(parts, block.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// codexOutputText renders a tool output, which is a bare string on some record
// kinds and an array of blocks on others.
func codexOutputText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		// Codex frequently stores the array as an ESCAPED STRING rather than as
		// JSON, so a decoded string may itself be a blocks array.
		var nested []codexBlock
		if json.Unmarshal([]byte(text), &nested) == nil {
			if flattened := codexBlocksText(nested); flattened != "" {
				return flattened
			}
		}
		return text
	}
	var blocks []codexBlock
	if json.Unmarshal(raw, &blocks) == nil {
		if flattened := codexBlocksText(blocks); flattened != "" {
			return flattened
		}
	}
	return string(raw)
}
