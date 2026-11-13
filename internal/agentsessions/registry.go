package agentsessions

import (
	"errors"
	"fmt"
	"strings"

	"github.com/Twigpine/zero/internal/sessions"
)

// Adapters returns every adapter in a stable order.
//
// Adding an agent is meant to be this line plus one file. The order is the
// order results are listed in when two sessions share a timestamp, so it stays
// deterministic rather than depending on map iteration.
func Adapters(env Env) []Adapter {
	return []Adapter{
		ClaudeCode(env),
		FactoryDroid(env),
		Pi(env),
		Codex(env),
	}
}

// DiscoverAll indexes every adapter's store and returns the sessions belonging
// to cwd, most recently updated first. An empty cwd means every session.
//
// One adapter failing never denies the user the others: these are undocumented
// formats owned by other products, and one of them changing shape must not take
// discovery down with it. Errors are returned alongside the results so a caller
// can mention them without withholding what did work.
func DiscoverAll(env Env, cwd string) ([]ForeignSession, []error) {
	found := []ForeignSession{}
	problems := []error{}
	for _, adapter := range Adapters(env) {
		// Source IDs are adapter-global public identities. Establish uniqueness
		// across the complete readable store before applying the workspace view;
		// otherwise discover can advertise a ref that import later finds
		// ambiguous, and durable agent:id provenance collapses two sources.
		discovered, err := adapter.Discover("")
		if err != nil {
			problems = append(problems, errors.New(adapter.Name()+": "+err.Error()))
			continue
		}
		counts := make(map[string]int, len(discovered))
		for _, session := range discovered {
			counts[session.ID]++
		}
		ambiguous := map[string]bool{}
		for _, session := range discovered {
			if counts[session.ID] > 1 {
				if !ambiguous[session.ID] && (strings.TrimSpace(cwd) == "" || sameDir(cwd, session.Cwd)) {
					problems = append(problems, fmt.Errorf("%s: session id %q is ambiguous across multiple transcripts", adapter.Name(), DisplayField(session.ID)))
					ambiguous[session.ID] = true
				}
				continue
			}
			if strings.TrimSpace(cwd) != "" && !sameDir(cwd, session.Cwd) {
				continue
			}
			found = append(found, session)
		}
	}
	sortByRecency(found)
	return found, problems
}

// ParseRef splits an "<agent>:<id>" reference and resolves the adapter.
//
// The agent prefix is required rather than inferred. Two agents can hold
// sessions with the same id — they are all uuids — and guessing which one the
// user meant would silently import the wrong conversation.
func ParseRef(env Env, ref string) (Adapter, string, error) {
	trimmed := strings.TrimSpace(ref)
	name, id, found := strings.Cut(trimmed, ":")
	if !found {
		return nil, "", errors.New("expected <agent>:<id>, for example claude-code:" +
			"3f2a1b4c-... — run `zero sessions discover` to list them")
	}
	name = strings.TrimSpace(name)
	id = strings.TrimSpace(id)
	if name == "" || id == "" {
		return nil, "", errors.New("both an agent and a session id are required, as <agent>:<id>")
	}
	for _, adapter := range Adapters(env) {
		if strings.EqualFold(adapter.Name(), name) {
			return adapter, id, nil
		}
	}
	return nil, "", errors.New("unknown agent " + name + "; known agents: " + strings.Join(AdapterNames(env), ", "))
}

// AdapterNames lists the agents this build can read.
func AdapterNames(env Env) []string {
	names := []string{}
	for _, adapter := range Adapters(env) {
		names = append(names, adapter.Name())
	}
	return names
}

// importTagPrefix marks a Zero session as a copy of another agent's transcript.
const importTagPrefix = "imported:"

// ImportTag is the provenance stamp an imported session carries:
// "imported:v1:<base64url agent>:<base64url foreign session id>".
//
// The foreign id is part of the tag, not a second metadata field, so there is
// exactly one record of where a session came from (repo invariant #5 — two
// places holding the same fact will drift). It is what lets a caller tell an
// already-imported session apart from one still only on the other agent's disk,
// which the /resume picker needs in order not to list both.
func ImportTag(agent string, sourceID string) string {
	return sessions.ImportedSessionTag(agent, sourceID)
}

// ParseImportTag splits an import tag back into its agent and source id.
// Reports false for a tag that is not an import stamp, including the older
// two-part "imported:<agent>" form, which records no source id to return.
func ParseImportTag(tag string) (agent string, sourceID string, ok bool) {
	return sessions.ParseImportedSessionTag(tag)
}

// ForeignSourceRefFromTag returns the display-level "agent:id" identity used
// to suppress a foreign picker row after it has been imported. Unlike
// ParseImportTag, this deliberately recognizes the legacy
// "imported:<agent>:<plaintext id>" spelling written by older builds. It is
// not an authority check: callers deciding whether a session may inherit
// foreign state must continue to use the strict versioned parser.
func ForeignSourceRefFromTag(tag string) (string, bool) {
	if agent, sourceID, ok := ParseImportTag(tag); ok {
		return agent + ":" + sourceID, true
	}
	trimmed := strings.TrimSpace(tag)
	rest := strings.TrimPrefix(trimmed, importTagPrefix)
	if rest == trimmed || strings.HasPrefix(rest, "v1:") {
		return "", false
	}
	agent, sourceID, found := strings.Cut(rest, ":")
	agent = strings.TrimSpace(agent)
	sourceID = strings.TrimSpace(sourceID)
	if !found || agent == "" || sourceID == "" {
		return "", false
	}
	return agent + ":" + sourceID, true
}

// ImportedAgent is the agent a session was imported from, or "" for a session
// Zero produced itself. Unlike ParseImportTag this accepts the older
// "imported:<agent>" form, so sessions imported before the tag carried a source
// id still group under the right agent.
func ImportedAgent(tag string) string {
	if agent, _, ok := ParseImportTag(tag); ok {
		return agent
	}
	rest := strings.TrimSpace(tag)
	trimmed := strings.TrimPrefix(rest, importTagPrefix)
	if trimmed == rest {
		return ""
	}
	agent, _, _ := strings.Cut(trimmed, ":")
	return strings.TrimSpace(agent)
}

// ImportResult reports what an import produced.
type ImportResult struct {
	Session sessions.Metadata
	Events  int
	Source  ForeignSession
}

var ErrNoImportableContent = errors.New("foreign session has no importable content")

// Import copies one foreign session into the Zero session store and returns the
// new Zero session.
//
// The store assigns the id rather than deriving one from the foreign id. An
// import is a SNAPSHOT of another agent's transcript at a moment in time, and
// that session may well be continued in its own tool afterwards; a second
// import is therefore a legitimate second snapshot, not a mistake to be refused.
// Provenance lives in the tag ("imported:claude-code:<foreign session id>") and
// in the title.
//
// Nothing is written to the foreign store at any point.
func Import(store *sessions.Store, adapter Adapter, id string, options ReadOptions) (ImportResult, error) {
	source, err := describe(adapter, id)
	if err != nil {
		return ImportResult{}, err
	}
	return ImportSource(store, adapter, source, options)
}

// ImportSource imports the exact discovery row selected by a caller. Picker
// flows use this form so a file disappearing or a same-ID file appearing after
// rendering cannot silently redirect the selection.
func ImportSource(store *sessions.Store, adapter Adapter, source ForeignSession, options ReadOptions) (ImportResult, error) {
	id := source.ID
	if adapter == nil || !strings.EqualFold(strings.TrimSpace(source.Agent), adapter.Name()) || strings.TrimSpace(id) == "" {
		return ImportResult{}, errors.New("foreign session source does not match its adapter")
	}
	// Exact snapshots prevent redirection, but they cannot make a duplicate
	// adapter ID a durable identity. Reject the same ambiguity as CLI import.
	if _, err := describe(adapter, id); err != nil {
		return ImportResult{}, err
	}
	if strings.TrimSpace(options.Cwd) == "" {
		options.Cwd = source.Cwd
	}
	events, err := adapter.Read(source, options)
	if err != nil {
		return ImportResult{}, err
	}
	if !hasImportableSourceContent(events) {
		return ImportResult{}, fmt.Errorf("import %s: %w", id, ErrNoImportableContent)
	}
	// The imported transcript crosses a model trust boundary. Persist one
	// generated note before any foreign turn so every resume projection tells the
	// continuing model that history is reference context, not instructions or
	// inherited authorization. This sits outside the adapter's source-event cap:
	// MaxEvents describes copied transcript events, while the boundary is Zero's
	// mandatory safety metadata.
	events = append([]sessions.AppendEventInput{importBoundaryEvent(adapter.Name())}, events...)

	created, discardCreated, err := store.CreateDiscardable(sessions.CreateInput{
		// THE STORE IS THE CHOKEPOINT FOR DISPLAY VALUES. These fields are
		// another product's bytes and every reader draws them: `zero sessions
		// list`, the /resume picker, the import summary, the workspace warning.
		// stripControl was not enough for a stored value — it deliberately keeps
		// newlines, which is right for a transcript line and wrong for a label
		// drawn as one row, and it does not redact, so a title (usually the
		// user's first prompt, where a pasted key lands) stayed a live secret in
		// the store for every consumer to leak independently. Two of them did.
		// DisplayField is the one helper that strips controls FIRST and then
		// redacts, the order redaction_order_test.go pins.
		Title:         DisplayField(source.Title),
		Cwd:           DisplayField(source.Cwd),
		WorkspaceKey:  normalizeDir(source.Cwd),
		SourceModelID: DisplayField(source.ModelID),
		Tag:           ImportTag(adapter.Name(), id),
	})
	if err != nil {
		return ImportResult{}, err
	}
	if _, err := store.AppendEvents(created.SessionID, events); err != nil {
		cleanupErr := discardCreated()
		if cleanupErr != nil {
			return ImportResult{}, errors.Join(
				fmt.Errorf("import %s into zero session %s: %w", id, created.SessionID, err),
				fmt.Errorf("clean up failed import: %w", cleanupErr),
			)
		}
		return ImportResult{}, fmt.Errorf("import %s: %w", id, err)
	}
	return ImportResult{Session: created, Events: len(events), Source: source}, nil
}

// describe finds the index entry for an id so the imported session inherits the
// title, cwd and model. Discovery is unfiltered here because the session being
// imported need not belong to the current directory — importing a session from
// another workspace is a reasonable thing to want.
func describe(adapter Adapter, id string) (ForeignSession, error) {
	found, err := adapter.Discover("")
	if err != nil {
		return ForeignSession{}, err
	}
	matches := []ForeignSession{}
	for _, session := range found {
		if session.ID == id {
			matches = append(matches, session)
		}
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	if len(matches) > 1 {
		return ForeignSession{}, fmt.Errorf("%s session id %q is ambiguous across %d transcripts; remove or rename the duplicate before importing", adapter.Name(), DisplayField(id), len(matches))
	}
	return ForeignSession{}, errors.New("no " + adapter.Name() + " session with id " + id +
		" — run `zero sessions discover --all` to list them")
}
