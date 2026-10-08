package sessions

import (
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/Gitlawb/zero/internal/redaction"
)

type ExecMode string

const (
	ModeNew    ExecMode = "new"
	ModeResume ExecMode = "resume"
	ModeFork   ExecMode = "fork"
)

type ExecError struct {
	message string
}

func (err ExecError) Error() string {
	return err.message
}

type PrepareExecOptions struct {
	Store            *Store
	SessionID        string
	Title            string
	Cwd              string
	ModelID          string
	Provider         string
	Tag              string
	Depth            int
	CallingSessionID string
	CallingToolUseID string
	AgentName        string
	TaskID           string
	Resume           string
	ResumeLatest     bool
	Fork             string
}

type PreparedExec struct {
	Mode          ExecMode
	Session       Metadata
	ContextEvents []Event
	Store         *Store
}

func PrepareExec(options PrepareExecOptions) (PreparedExec, error) {
	resumeID := strings.TrimSpace(options.Resume)
	forkID := strings.TrimSpace(options.Fork)
	if (resumeID != "" || options.ResumeLatest) && forkID != "" {
		return PreparedExec{}, ExecError{"Use either --resume or --fork, not both."}
	}

	store := options.Store
	if store == nil {
		store = NewStore(StoreOptions{})
	}

	if forkID != "" {
		parent, err := store.Get(forkID)
		if err != nil {
			return PreparedExec{}, err
		}
		if parent == nil {
			return PreparedExec{}, ExecError{"Zero session not found: " + forkID}
		}
		contextEvents, err := readExecContextEvents(store, parent.SessionID)
		if err != nil {
			return PreparedExec{}, err
		}
		session, err := store.Fork(parent.SessionID, ForkInput{
			SessionID: options.SessionID,
			Title:     firstNonEmpty(options.Title, forkTitle(parent.Title)),
			Cwd:       options.Cwd,
			ModelID:   options.ModelID,
			Provider:  options.Provider,
		})
		if err != nil {
			return PreparedExec{}, err
		}
		return PreparedExec{Mode: ModeFork, Session: session, ContextEvents: contextEvents, Store: store}, nil
	}

	if resumeID != "" || options.ResumeLatest {
		sessionID := resumeID
		if sessionID == "" && options.ResumeLatest {
			latest, err := store.LatestResumable()
			if err != nil {
				return PreparedExec{}, err
			}
			if latest == nil {
				return PreparedExec{}, ExecError{"No Zero sessions available to resume."}
			}
			sessionID = latest.SessionID
		}
		session, err := store.Get(sessionID)
		if err != nil {
			return PreparedExec{}, err
		}
		if session == nil {
			return PreparedExec{}, ExecError{"Zero session not found: " + sessionID}
		}
		if !IsResumableKind(session.SessionKind) {
			return PreparedExec{}, ExecError{"Zero session is not resumable: " + sessionID}
		}
		contextEvents, err := readExecContextEvents(store, session.SessionID)
		if err != nil {
			return PreparedExec{}, err
		}
		return PreparedExec{Mode: ModeResume, Session: *session, ContextEvents: contextEvents, Store: store}, nil
	}

	createInput := CreateInput{
		SessionID: options.SessionID,
		Title:     options.Title,
		Cwd:       options.Cwd,
		ModelID:   options.ModelID,
		Provider:  options.Provider,
		Tag:       options.Tag,
		Depth:     options.Depth,
	}
	if strings.TrimSpace(options.CallingSessionID) != "" {
		parentSessionID := strings.TrimSpace(options.CallingSessionID)
		parent, err := store.Get(parentSessionID)
		if err != nil {
			return PreparedExec{}, err
		}
		if parent == nil {
			return PreparedExec{}, ExecError{"Zero parent session not found: " + parentSessionID}
		}
		createInput.SessionKind = SessionKindChild
		createInput.ParentSessionID = parent.SessionID
		createInput.RootSessionID = firstNonEmpty(parent.RootSessionID, parent.SessionID)
		createInput.AgentName = strings.TrimSpace(options.AgentName)
		createInput.TaskID = strings.TrimSpace(firstNonEmpty(options.TaskID, options.SessionID))
		createInput.SpawnedFromEventID = strings.TrimSpace(options.CallingToolUseID)
	}
	session, err := store.Create(createInput)
	if err != nil {
		return PreparedExec{}, err
	}
	return PreparedExec{Mode: ModeNew, Session: session, ContextEvents: []Event{}, Store: store}, nil
}

func readExecContextEvents(store *Store, sessionID string) ([]Event, error) {
	contextEvents, err := store.ReadRehydratedEvents(sessionID)
	if err == nil {
		return contextEvents, nil
	}
	rawEvents, rawErr := store.ReadEvents(sessionID)
	if rawErr != nil {
		return nil, err
	}
	log.Printf("zero sessions: failed to rehydrate compaction events for %s; falling back to raw events: %v", sessionID, err)
	return rawEvents, nil
}

func FormatExecPrompt(prompt string, prepared PreparedExec) string {
	if prepared.Mode == ModeNew || len(prepared.ContextEvents) == 0 {
		return prompt
	}
	events := promptContextEvents(prepared.ContextEvents)

	lines := []string{}
	for _, event := range events {
		lines = append(lines, fmt.Sprintf("- #%d %s: %s", event.Sequence, event.Type, summarizePayload(event.Payload)))
	}
	label := "Continuing"
	sessionID := prepared.Session.SessionID
	if prepared.Mode == ModeFork {
		label = "Forked from"
		if prepared.Session.ParentSessionID != "" {
			sessionID = prepared.Session.ParentSessionID
		}
	}
	return strings.Join([]string{
		fmt.Sprintf("%s Zero session %s.", label, sessionID),
		"Previous session context:",
		strings.Join(lines, "\n"),
		"",
		"Current user request:",
		prompt,
	}, "\n")
}

// promptContextEvents chooses what a resumed turn is told about the session so
// far.
//
// A RESUMED TURN NEEDS TO KNOW WHAT THE PREVIOUS ONE DID, NOT ONLY WHAT IT SAID.
//
// Conversation events alone were selected here, so a turn that read six files
// and then died before answering left this behind:
//
//   - #1 message: add retries to the http client
//   - #6 error: provider error: upstream timeout
//
// Nothing names the files, so the next turn re-reads them from scratch (#913).
// On a turn that ENDS NORMALLY the assistant's answer describes the work, which
// is why this was survivable; an interrupted turn produces no such answer, and
// the record of the work goes with it.
//
// ONLY THE UNSUMMARIZED TAIL, NOT EVERY TOOL EVENT. Filtering tool events out
// was deliberate (#460) and is right for work the assistant has already
// described: forty read_file results add length and no information next to the
// answer that explains them. It is wrong only for work with no answer after it,
// which is precisely what an interrupted turn leaves behind. So the events after
// the last assistant message come along, and everything the assistant already
// spoke for stays filtered.
//
// The tail also gets its own allowance rather than sharing the conversation
// budget, so a tool-heavy interrupted turn can never push an earlier message out
// of the context. That is the property #460 added this filter for and it still
// holds.
func promptContextEvents(events []Event) []Event {
	const maxPromptContextEvents = 80
	// Enough to describe an interrupted turn's work, small enough that the
	// conversation still dominates the prompt.
	const maxPromptContextTailEvents = 24

	lastSpoken := -1
	for index, event := range events {
		if event.Type == EventMessage && payloadRole(event.Payload) == "assistant" {
			lastSpoken = index
		}
	}

	conversation := make([]Event, 0, len(events))
	tail := make([]Event, 0, maxPromptContextTailEvents)
	for index, event := range events {
		switch event.Type {
		case EventMessage, EventCompaction, EventSessionFork, EventSessionChild, EventSpecialistStart, EventSpecialistStop, EventError:
			conversation = append(conversation, event)
		case EventToolCall:
			if index > lastSpoken {
				tail = append(tail, toolCallIdentity(event))
			}
		case EventToolResult:
			if index > lastSpoken {
				tail = append(tail, toolResultOutcome(event))
			}
		}
	}
	if len(conversation) == 0 && len(tail) == 0 {
		// An unrecognized event stream still says more than nothing.
		conversation = append(conversation, events...)
	}
	if len(conversation) > maxPromptContextEvents {
		conversation = conversation[len(conversation)-maxPromptContextEvents:]
	}
	// Tail slots exist only when the conversation uses fewer than the 80-event
	// cap. A session whose conversation already fills it carries no interrupted
	// tool work, which is what every session did before tool events were admitted
	// at all, and what the #460 cap is there to hold. Reserving a minimum tail by
	// trimming conversation further is a product decision, not this change.
	tailBudget := min(maxPromptContextTailEvents, maxPromptContextEvents-len(conversation))
	if tailBudget < 0 {
		tailBudget = 0
	}
	if len(tail) > tailBudget {
		tail = tail[len(tail)-tailBudget:]
	}
	if len(tail) == 0 {
		return conversation
	}
	// Merged back into execution order: the sequence numbers are rendered, so a
	// list that jumps backwards would read as a corrupted history.
	merged := make([]Event, 0, len(conversation)+len(tail))
	merged = append(merged, conversation...)
	merged = append(merged, tail...)
	sort.SliceStable(merged, func(i, j int) bool { return merged[i].Sequence < merged[j].Sequence })
	return merged
}

// toolCallIdentityKeys are the argument fields that say WHAT a call was about
// without carrying what it was about to write, run, or search for.
//
// Path, directory, URL, name and pattern fields identify the work: the file that
// was read, the tree that was listed, the expression that was searched. Body
// fields carry payload: write_file's content, edit_file's old and new strings,
// apply_patch's hunks, a shell command with whatever credential was on its
// line. The result side already drops payload through toolResultOutcome, and
// admitting calls into resume context without the same care put an interrupted
// write_file's content into the next turn's prompt while its result body did
// not. Same fact, one door left open.
//
// AN ALLOW-LIST, NOT A DENY-LIST, so an argument this file has never heard of is
// dropped rather than replayed. A new tool with a new body field is then a
// missing path in a resume prompt, which is visible, instead of a new leak, which
// is not. The keys cover every alias the tools accept for the identity fields.
var toolCallIdentityKeys = map[string]bool{
	// files and directories
	"path": true, "file": true, "file_path": true, "filepath": true, "filename": true,
	"dir": true, "directory": true, "cwd": true, "workdir": true,
	// what a search was for; these are what the interrupted turn was looking at
	"pattern": true, "glob": true, "query": true, "regex": true, "expression": true,
	"match": true,
	// fetches and named resources
	"url": true, "name": true,
	// read windows, so a resumed turn knows which part it already had. Every
	// form read_file accepts, because a resumed turn that knows the path but not
	// which slice was asked for has half the identity and will read again.
	"offset": true, "limit": true,
	"start_line": true, "end_line": true, "max_lines": true,
	"byte_offset": true, "byte_limit": true,
}

// toolCallIdentityKeysByTool holds the argument names whose meaning depends on
// which tool was called, so they cannot live in the table above.
//
// grep accepts `search` for its pattern, and edit_file accepts the same word
// for the text being replaced. Admitting it globally for the sake of the first
// would replay file contents through the second, which is the whole thing this
// projection exists to stop. The rule this encodes: an argument name that any
// MUTATING tool accepts for body content is scoped to the tools that mean
// something else by it, never added to the shared table.
var toolCallIdentityKeysByTool = map[string]map[string]bool{
	"grep": {"search": true},
}

// retainedIdentityValue decides whether a value under a permitted key may be
// kept, and what it looks like if so.
//
// AN ALLOW-LISTED KEY IS NOT A SAFE VALUE. A url is a valid place for a
// credential to appear: web_fetch accepts a query token and redacts the URL it
// reports back, so an interrupted fetch of
// https://api.example/data?access_token=... had its token dropped from the
// result and kept verbatim in the call. This projection is what admits call
// arguments into a later turn's prompt, so the same scrub belongs here or the
// credential is replayed on resume. Host and path survive it, which is the
// identity the resumed turn needs.
//
// AND A KEY IS NOT PERMISSION FOR WHATEVER HANGS UNDER IT. Scrubbing strings
// and passing everything else through read as "numbers are nothing to scrub",
// which is true of a number and not of an object: MCP schemas allow object and
// array properties, so {"query":{"api_key":"...","content":"private body"}}
// arrives under a permitted key and carries a whole payload with it. Redacting
// a serialized object would not help either, since ordinary private text has no
// credential shape to match.
//
// So the retained shapes are named: a string, scrubbed, and the scalars a read
// window is made of. A container is dropped. The call keeps its name and id, so
// a resumed turn still knows which tool ran and can ask again; what it does not
// get is arbitrary nested content it was never meant to see.
func retainedIdentityValue(value any) (any, bool) {
	switch typed := value.(type) {
	case string:
		return redaction.RedactString(typed, redaction.Options{}), true
	case float64, int, int64, bool, json.Number:
		return typed, true
	default:
		// Objects, arrays, null, and anything a future decoder invents.
		return nil, false
	}
}

// toolCallIdentityKeyAllowed reports whether key is an identity field for the
// tool that was called.
func toolCallIdentityKeyAllowed(toolName, key string) bool {
	lowered := strings.ToLower(key)
	if toolCallIdentityKeys[lowered] {
		return true
	}
	return toolCallIdentityKeysByTool[strings.ToLower(strings.TrimSpace(toolName))][lowered]
}

// toolCallIdentityFields are the top-level payload fields a projected call
// keeps: which call it was, and which tool. Everything else on the payload is
// dropped, including a field added by a future producer that this file has
// never seen.
var toolCallIdentityFields = []string{"id", "name"}

// toolCallIdentity keeps a tool call's identity and drops its payload, the
// symmetric half of toolResultOutcome.
//
// The arguments travel as a JSON string inside the payload. They are decoded,
// reduced to the identity keys, and re-encoded; anything that does not decode as
// an object is removed outright rather than passed through as text, since text
// that could not be read is text that cannot be checked.
//
// REBUILT, NOT PATCHED, WHICH IS WHAT MAKES IT THE SYMMETRIC HALF.
// toolResultOutcome constructs its output from the two fields it keeps, so a
// field it has never heard of cannot reach a prompt through it. Editing
// arguments in place and returning the rest of the payload is the opposite
// contract: it drops what it recognises as a body and forwards every sibling
// key, so a producer recording one more top-level field would put that field
// into the next turn. The projection names what survives.
func toolCallIdentity(event Event) Event {
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(event.Payload, &decoded); err != nil {
		event.Payload = json.RawMessage(`{}`)
		return event
	}
	projected := map[string]json.RawMessage{}
	for _, field := range toolCallIdentityFields {
		if value, present := decoded[field]; present {
			projected[field] = value
		}
	}
	// The tool decides what some argument names mean, so the projection has to
	// know which tool this was before it can say which keys are identity.
	toolName := ""
	if raw, present := decoded["name"]; present {
		_ = json.Unmarshal(raw, &toolName)
	}
	kept := map[string]any{}
	if raw, present := decoded["arguments"]; present {
		var argumentsText string
		if err := json.Unmarshal(raw, &argumentsText); err == nil {
			var arguments map[string]any
			if err := json.Unmarshal([]byte(argumentsText), &arguments); err == nil {
				for key, value := range arguments {
					if !toolCallIdentityKeyAllowed(toolName, key) {
						continue
					}
					if retained, ok := retainedIdentityValue(value); ok {
						kept[key] = retained
					}
				}
			}
		}
	}
	if len(kept) > 0 {
		if reduced, err := json.Marshal(kept); err == nil {
			if quoted, err := json.Marshal(string(reduced)); err == nil {
				projected["arguments"] = quoted
			}
		}
	}
	rebuilt, err := json.Marshal(projected)
	if err != nil {
		event.Payload = json.RawMessage(`{}`)
		return event
	}
	event.Payload = json.RawMessage(rebuilt)
	return event
}

// toolResultOutcome strips a tool result down to WHICH tool ran and HOW IT
// ENDED, dropping the output body.
//
// The tail exists so a resumed turn knows what the interrupted one did, and the
// CALL already carries that: the tool name and its arguments, which is the path
// for a read. The result adds only a 500-byte prefix of the output, which is
// worth little beside the call and is the one part of an event that can carry
// file contents. Nothing redacts on the way into a prompt, so keeping it would
// re-emit raw tool output into a later turn on the strength of a truncation
// limit alone.
//
// The status stays, because dropping it would be worse than dropping the whole
// result: a bare call reads as work that succeeded, so a failed read would come
// back as a file the next turn believes it already has.
func toolResultOutcome(event Event) Event {
	var decoded struct {
		Name   string `json:"name"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(event.Payload, &decoded); err != nil {
		// Undecodable: drop the payload rather than pass an unknown shape through.
		event.Payload = json.RawMessage(`{}`)
		return event
	}
	trimmed, err := json.Marshal(map[string]string{
		"name":   decoded.Name,
		"status": decoded.Status,
	})
	if err != nil {
		event.Payload = json.RawMessage(`{}`)
		return event
	}
	event.Payload = json.RawMessage(trimmed)
	return event
}

// payloadRole reads the "role" field of a message payload, returning "" when the
// payload is not a decodable object or carries no role.
func payloadRole(payload json.RawMessage) string {
	if len(payload) == 0 {
		return ""
	}
	var decoded struct {
		Role string `json:"role"`
	}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return ""
	}
	return strings.TrimSpace(decoded.Role)
}

func forkTitle(title string) string {
	if title == "" {
		return ""
	}
	return title + " (fork)"
}

func summarizePayload(payload any) string {
	text := extractText(payload)
	text = strings.Join(strings.Fields(text), " ")
	if text == "" {
		data, err := json.Marshal(payload)
		if err != nil {
			return "{}"
		}
		text = string(data)
	}
	if len(text) > 500 {
		return truncateUTF8(text, 500)
	}
	return text
}

// truncateUTF8 returns the longest prefix of s that is at most n bytes,
// backing off to the nearest rune boundary so a multi-byte character isn't
// split — a split rune here would embed invalid UTF-8 into the exec prompt.
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func extractText(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case json.RawMessage:
		var decoded any
		if err := json.Unmarshal(typed, &decoded); err == nil {
			return extractText(decoded)
		}
		return string(typed)
	case float64, bool, int:
		return fmt.Sprint(typed)
	case []any:
		parts := []string{}
		for _, item := range typed {
			if text := extractText(item); text != "" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, " ")
	case map[string]any:
		if summary, ok := typed["summary"].(string); ok && strings.TrimSpace(summary) != "" {
			return summary
		}
		// BY KEY, BECAUSE THIS TEXT BECOMES PART OF A PROMPT. Go randomizes map
		// iteration, so the same session rendered its resume context in a
		// different field order in every process: one run said
		// `c1 read_file {"path":...}` and the next `{"path":...} c1 read_file`.
		// The block goes into the user prompt, so the prompt itself changed run
		// to run, which gives a provider nothing stable to prefix-cache and
		// leaves two resumes of one session impossible to diff while debugging.
		//
		// Only the ordering is decided here. Which fields survive is the
		// caller's projection, and a slice keeps the order it was written in.
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, key := range keys {
			if text := extractText(typed[key]); text != "" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, " ")
	default:
		return ""
	}
}
