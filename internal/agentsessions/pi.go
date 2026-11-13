package agentsessions

import (
	"encoding/json"
	"strings"

	"github.com/Twigpine/zero/internal/sessions"
	"github.com/Twigpine/zero/internal/tools"
)

// pi reads Pi's agent transcripts: ~/.pi/agent/sessions/<slug>/<ts>_<uuid>.jsonl.
//
// PI SHARES FAMILY 1'S DIRECTORY LAYOUT AND NOT ITS MESSAGE SCHEMA. It was
// routed through the Claude parser on the strength of the layout, and the
// parser's block vocabulary (tool_use / tool_result, is_error, snake_case ids)
// is simply not what Pi writes:
//
//	header   {"type":"session","id":…,"timestamp":…,"cwd":…}
//	entry    {"type":"message","id":…,"parentId":…,"timestamp":…,"message":{…}}
//	user     {"role":"user","content":"…" | [{"type":"text","text":…}]}
//	assistant{"role":"assistant","model":…,"content":[{"type":"text"|"thinking"|"toolCall",…}]}
//	toolCall {"type":"toolCall","id":…,"name":…,"arguments":{…object…}}
//	result   {"role":"toolResult","toolCallId":…,"toolName":…,"isError":bool,"content":[{"type":"text",…}]}
//
// The shared parser recognized none of the tool records and rejected the
// toolResult role outright, so a Pi session containing a request and a completed
// tool call imported "successfully" with no call, no result and no activity
// summary — the work needed to continue it silently omitted — and the outer
// type of "message" on every entry meant the title check for type "user" never
// fired, so every Pi session was "untitled". Both were one wrong assumption.
// Reported by @jatmn.
//
// Bounded reading, event construction, redaction, the reasoning opt-in and the
// activity summary stay shared; only the vendor's parsing differs.
type pi struct {
	root string
}

// Pi reads Pi's agent transcripts.
func Pi(env Env) Adapter {
	return pi{root: piRoot(env)}
}

func (adapter pi) Name() string { return "pi" }

func (adapter pi) Discover(cwd string) ([]ForeignSession, error) {
	return discoverFamily1(adapter.Name(), adapter.root, cwd, indexPiTranscript)
}

func (adapter pi) Read(source ForeignSession, options ReadOptions) ([]sessions.AppendEventInput, error) {
	if source.Agent != adapter.Name() || source.ID != transcriptID(source.Path) {
		return nil, errNotThisAgent(adapter.Name())
	}
	// One handle from identity check to final check: see openSelectedSource.
	file, err := openSelectedSource(adapter.root, source)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	events, err := translatePi(file, options)
	if err != nil {
		return nil, err
	}
	if err := validateSourceHandle(file, source); err != nil {
		return nil, err
	}
	return events, nil
}

// piEntry is one line of a Pi session file: the session header or an entry.
// Unknown entry types (thinking_level_change, model_change, compaction, …)
// carry no message and drop out naturally.
type piEntry struct {
	Type      string     `json:"type"`
	Cwd       string     `json:"cwd"`
	Timestamp string     `json:"timestamp"`
	Message   *piMessage `json:"message"`
}

type piMessage struct {
	Role    string          `json:"role"`
	Model   string          `json:"model"`
	Content json.RawMessage `json:"content"`
	// toolResult fields.
	ToolCallID string `json:"toolCallId"`
	ToolName   string `json:"toolName"`
	IsError    bool   `json:"isError"`
}

type piBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// indexPiTranscript builds an index entry from a bounded read of the head. The
// workspace and start time come from the session header; the title is the
// first user prompt, which is inside a "message" entry, not a "user" record.
func indexPiTranscript(agent string, root string, path string) (ForeignSession, bool) {
	session := ForeignSession{Agent: agent, ID: transcriptID(path), Path: path}
	firstPrompt := ""
	_, snapshot, err := scanHeadSnapshot(root, path, defaultHeadLimit, func(line []byte, truncated bool) bool {
		var entry piEntry
		if json.Unmarshal(line, &entry) != nil {
			if truncated {
				recovered := topLevelStrings(line, "cwd", "timestamp")
				if session.Cwd == "" {
					session.Cwd = recovered["cwd"]
				}
				if session.StartedAt.IsZero() {
					session.StartedAt = parseTimestamp(recovered["timestamp"])
				}
			}
			return true
		}
		if session.Cwd == "" {
			session.Cwd = entry.Cwd
		}
		if session.StartedAt.IsZero() {
			session.StartedAt = parseTimestamp(entry.Timestamp)
		}
		if entry.Type != "message" || entry.Message == nil {
			return true
		}
		if session.ModelID == "" && entry.Message.Role == "assistant" {
			session.ModelID = entry.Message.Model
		}
		if firstPrompt == "" && entry.Message.Role == "user" {
			firstPrompt = piBlocksText(entry.Message.Content)
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

// translatePi converts a Pi session into Zero events. Same lossy-in-one-direction
// policy as translateFamily1: the conversation and the tool work are kept, the
// provider's private machinery (signatures, usage, diagnostics) is dropped.
func translatePi(file readSeekStater, options ReadOptions) ([]sessions.AppendEventInput, error) {
	events := newEventTail(effectiveMaxEvents(options.MaxEvents))
	identities := &importCallIdentities{}
	activity := newActivityLog(options.Cwd)
	omitted := 0
	prefixOmitted, err := streamTailLines(file, importLineLimit, importByteLimit, func(line []byte, truncated bool) bool {
		if truncated {
			omitted++
			return true
		}
		var entry piEntry
		if json.Unmarshal(line, &entry) != nil || entry.Type != "message" || entry.Message == nil {
			return true
		}
		message := entry.Message
		switch strings.ToLower(strings.TrimSpace(message.Role)) {
		case "user":
			if text := piBlocksText(message.Content); strings.TrimSpace(text) != "" {
				events.add(messageEvent("user", text))
			}
		case "assistant":
			var blocks []piBlock
			if json.Unmarshal(message.Content, &blocks) != nil {
				return true
			}
			for _, block := range blocks {
				switch block.Type {
				case "text":
					if strings.TrimSpace(block.Text) != "" {
						events.add(messageEvent("assistant", block.Text))
					}
				case "thinking":
					if options.IncludeReasoning && strings.TrimSpace(block.Thinking) != "" {
						events.add(messageEvent("reasoning", block.Thinking))
					}
				case "toolCall":
					arguments := string(block.Arguments)
					activity.observeCall(block.ID, block.Name, arguments)
					events.add(toolCallEvent(identities, block.Name, block.ID, arguments))
				}
			}
		case "toolresult":
			// Pi records the outcome explicitly, so it is carried as fact: only a
			// confirmed success commits an activity claim (observeResult).
			status := tools.StatusOK
			if message.IsError {
				status = tools.StatusError
			}
			name := strings.TrimSpace(message.ToolName)
			if name == "" {
				name = "unknown"
			}
			output := piBlocksText(message.Content)
			activity.observeResult(message.ToolCallID, name, status, output)
			events.add(toolResultEvent(identities, name, message.ToolCallID, status, output))
		}
		return true
	})
	if err != nil {
		return nil, err
	}
	contextEvents := activity.summaryEvents()
	if prefixOmitted {
		contextEvents = append(contextEvents, omittedPrefixEvent())
	}
	if omitted > 0 {
		contextEvents = append(contextEvents, omittedRecordsEvent(omitted))
	}
	return capTranslatedEventsDropped(events.values(), contextEvents, effectiveMaxEvents(options.MaxEvents), events.dropped), nil
}

// piBlocksText flattens Pi content, which is a bare string or an array of
// blocks, to its text. Images and tool calls contribute nothing here.
func piBlocksText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var blocks []piBlock
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	parts := []string{}
	for _, block := range blocks {
		if block.Type == "text" && strings.TrimSpace(block.Text) != "" {
			parts = append(parts, block.Text)
		}
	}
	return strings.Join(parts, "\n")
}
