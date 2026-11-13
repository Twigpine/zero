package agentsessions

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Twigpine/zero/internal/sessions"
)

// family1 is the layout two of the agents surveyed independently arrived at:
// one JSONL file per session, in a directory named after the working directory,
// with records carrying {type, cwd, timestamp, message:{role, content}} and
// content blocks of text / thinking / tool_use / tool_result.
//
//	Claude Code   ~/.claude/projects/<slug>/<uuid>.jsonl
//	Factory Droid ~/.factory/sessions/<slug>/<uuid>.jsonl
//
// They differ only in where the store lives and which record carries the title:
// Claude Code writes an "ai-title" record, Factory puts a "title" on
// "session_start". Everything else — the block vocabulary, the tool_use/
// tool_result pairing, the role names — is byte-for-byte the same shape, which
// is why one parser serves both. Pi shares the DIRECTORY layout and reuses
// discoverFamily1 for it, but its message schema is its own: see pi.go.
//
// An adapter is therefore a name and a root.
type family1 struct {
	name string
	root string
}

func (adapter family1) Name() string { return adapter.name }

func (adapter family1) Discover(cwd string) ([]ForeignSession, error) {
	return discoverFamily1(adapter.name, adapter.root, cwd, indexFamily1Transcript)
}

// Read translates one session into Zero events.
//
// Unlike Discover, this reports its errors: the user has named a specific
// session, and returning an empty conversation because the file moved would be
// a lie about what that session contained.
func (adapter family1) Read(source ForeignSession, options ReadOptions) ([]sessions.AppendEventInput, error) {
	if source.Agent != adapter.name || source.ID != transcriptID(source.Path) {
		return nil, errNotThisAgent(adapter.name)
	}
	// One handle from identity check to final check: see openSelectedSource.
	file, err := openSelectedSource(adapter.root, source)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	events, err := translateFamily1(file, options)
	if err != nil {
		return nil, err
	}
	if err := validateSourceHandle(file, source); err != nil {
		return nil, err
	}
	return events, nil
}

// ClaudeCode reads Claude Code's transcripts.
func ClaudeCode(env Env) Adapter {
	return family1{name: "claude-code", root: claudeCodeRoot(env)}
}

// FactoryDroid reads Factory's droid transcripts.
func FactoryDroid(env Env) Adapter {
	return family1{name: "factory", root: factoryRoot(env)}
}

// family1Record is the subset of a family-1 transcript record this package
// reads. Unknown fields are ignored by encoding/json, which is what lets the
// adapter survive the format gaining records it has never seen.
type family1Record struct {
	Type      string `json:"type"`
	Cwd       string `json:"cwd"`
	GitBranch string `json:"gitBranch"`
	SessionID string `json:"sessionId"`
	Timestamp string `json:"timestamp"`
	UUID      string `json:"uuid"`
	AITitle   string `json:"aiTitle"`
	// Title is Factory's spelling of the same idea, carried on its
	// session_start record rather than on a record of its own.
	Title   string          `json:"title"`
	Message *family1Message `json:"message"`
}

type family1Message struct {
	Role    string          `json:"role"`
	Model   string          `json:"model"`
	Content json.RawMessage `json:"content"`
}

// family1Block is one content block. Message content is either a bare string or
// an array of these.
type family1Block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

// discoverFamily1 is the shared body for the slugged-directory JSONL agents.
//
// The slug is a hint only. When a candidate directory exists it is searched
// first, which turns 1,266 files into the ~223 that belong to this workspace;
// when none matches, every project directory is indexed and the cwd recorded
// INSIDE each transcript decides. That ordering matters because the slug cannot
// be reversed — see slugCandidates.
func discoverFamily1(
	agent string,
	root string,
	cwd string,
	index func(agent string, root string, path string) (ForeignSession, bool),
) ([]ForeignSession, error) {
	if strings.TrimSpace(root) == "" {
		return nil, nil
	}

	// Discovery and the later selected-source open both rely on the directories
	// returned by globSessionDirs, which Lstat-skips a symlinked project directory
	// (following one could descend into a credential tree). The slug fast path
	// must narrow WITHIN that same set, not join root/slug on its own.
	sessionDirs := globSessionDirs(root)
	dirs := []string{}
	if strings.TrimSpace(cwd) != "" {
		wanted := map[string]bool{}
		for _, slug := range slugCandidates(cwd) {
			wanted[slug] = true
		}
		for _, dir := range sessionDirs {
			if wanted[filepath.Base(dir)] &&
				len(globTranscripts(root, filepath.Join(dir, "*"+transcriptExt))) > 0 {
				dirs = append(dirs, dir)
			}
		}
	}
	if len(dirs) == 0 {
		dirs = sessionDirs
	}

	found := []ForeignSession{}
	for _, dir := range dirs {
		for _, path := range globTranscripts(root, filepath.Join(dir, "*"+transcriptExt)) {
			session, ok := index(agent, root, path)
			if !ok {
				continue
			}
			// The transcript's own cwd is the authority, never the directory name.
			if strings.TrimSpace(cwd) != "" && !sameDir(cwd, session.Cwd) {
				continue
			}
			found = append(found, session)
		}
	}
	sortByRecency(found)
	return found, nil
}

// indexFamily1Transcript builds an index entry from a bounded read of the file's
// head. It returns false for anything it cannot identify as a session, which is
// how a partially written, empty, or reshaped file drops out of discovery
// instead of failing it.
func indexFamily1Transcript(agent string, root string, path string) (ForeignSession, bool) {
	session := ForeignSession{
		Agent: agent,
		ID:    transcriptID(path),
		Path:  path,
	}
	firstPrompt := ""

	_, snapshot, err := scanHeadSnapshot(root, path, defaultHeadLimit, func(line []byte, truncated bool) bool {
		var record family1Record
		if json.Unmarshal(line, &record) != nil {
			// A RECORD TOO LONG TO PARSE STILL CARRIES ITS METADATA AT THE FRONT.
			// Skipping it whole is right for the body — the cap exists so a giant
			// tool result is not held in memory — but cwd, the git branch and the
			// timestamp sit before it, and dropping them with the body cost the
			// entire session: with no workspace to bind to, discovery dropped a
			// transcript the user can see on disk, and it looked exactly like a
			// legitimate stub.
			//
			// Only for a TRUNCATED line. A genuinely malformed one is skipped as
			// before: transcripts are appended live and the last line is routinely
			// half-written, and guessing at fields there would invent them.
			if truncated {
				recovered := topLevelStrings(line, "cwd", "gitBranch", "timestamp")
				if session.Cwd == "" {
					session.Cwd = recovered["cwd"]
				}
				if session.GitBranch == "" {
					session.GitBranch = recovered["gitBranch"]
				}
				if session.StartedAt.IsZero() {
					session.StartedAt = parseTimestamp(recovered["timestamp"])
				}
			}
			// One malformed line is not a malformed file: transcripts are
			// appended live and the last line is routinely half-written.
			return true
		}
		if session.Cwd == "" {
			session.Cwd = record.Cwd
		}
		if session.GitBranch == "" {
			session.GitBranch = record.GitBranch
		}
		if session.StartedAt.IsZero() {
			session.StartedAt = parseTimestamp(record.Timestamp)
		}
		if record.Message != nil && session.ModelID == "" {
			session.ModelID = record.Message.Model
		}
		// An agent-supplied title beats a truncated first prompt, so it wins when
		// present: Claude Code writes one as an "ai-title" record, Factory as a
		// "title" on "session_start". Pi writes none, so the prompt is used.
		if title := firstNonBlank(record.AITitle, record.Title); title != "" {
			session.Title = title
		}
		if firstPrompt == "" && record.Type == "user" && record.Message != nil {
			firstPrompt = family1Text(record.Message.Content)
		}
		return true
	})
	if err != nil {
		return ForeignSession{}, false
	}

	if session.Title == "" {
		session.Title = summarizeTitle(firstPrompt)
	}
	// A file with no cwd is not a session transcript — most likely a sidecar the
	// agent dropped into the same directory. Refusing it here keeps discovery
	// from listing entries that Read could never make sense of.
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

// transcriptID is the session's identifier: the file's base name without its
// extension.
//
// Deliberately NOT stored as a path. Read resolves an id by globbing and
// comparing base names, so a hostile or mistyped id such as "../../auth" can
// never be joined onto a root and opened — it simply matches nothing.
func transcriptID(path string) string {
	base := filepath.Base(path)
	return strings.TrimSuffix(base, filepath.Ext(base))
}

// family1Text flattens a message's content to plain text. Content is either a
// bare string or an array of typed blocks; tool calls and thinking contribute
// nothing here, since this feeds titles and previews.
func family1Text(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var blocks []family1Block
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

func sortByRecency(items []ForeignSession) {
	sort.SliceStable(items, func(left, right int) bool {
		if !items[left].UpdatedAt.Equal(items[right].UpdatedAt) {
			return items[left].UpdatedAt.After(items[right].UpdatedAt)
		}
		return items[left].ID < items[right].ID
	})
}

func parseTimestamp(value string) time.Time {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if parsed, err := time.Parse(layout, trimmed); err == nil {
			return parsed
		}
	}
	return time.Time{}
}

// summarizeTitle collapses a prompt to a single short line. Runes, not bytes,
// so a multi-byte character is never split into invalid UTF-8.
func summarizeTitle(prompt string) string {
	// Sanitize the complete source before shortening it. Cutting first can remove
	// the suffix or closing quote that makes a credential recognizable and leave
	// a secret-bearing fragment in the persisted title.
	collapsed := strings.Join(strings.Fields(DisplayField(prompt)), " ")
	if collapsed == "" {
		return "untitled"
	}
	const limit = 72
	runes := []rune(collapsed)
	if len(runes) <= limit {
		return collapsed
	}
	return strings.TrimSpace(string(runes[:limit])) + "…"
}

func errNotThisAgent(name string) error {
	return errors.New("agentsessions: selected session does not belong to " + name)
}

func firstNonBlank(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
