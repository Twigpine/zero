package cli

import (
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/Twigpine/zero/internal/agentsessions"
	"github.com/Twigpine/zero/internal/redaction"
	"github.com/Twigpine/zero/internal/sessions"
)

// runSessionsDiscover lists sessions belonging to other coding agents.
//
// Read-only in every sense: it opens transcripts, never credentials, and never
// writes to another agent's store. See internal/agentsessions for why the file
// access is glob-bounded rather than a directory walk.
func runSessionsDiscover(options sessionCommandOptions, stdout io.Writer, stderr io.Writer) int {
	cwd := ""
	if !options.allWorkspaces {
		// Scope to this workspace by default. On a machine with a thousand
		// transcripts, an unscoped list is not a list anyone can use.
		working, err := os.Getwd()
		if err != nil {
			return writeAppError(stderr, "resolve working directory: "+err.Error(), exitCrash)
		}
		cwd = working
	}

	found, problems := agentsessions.DiscoverAll(agentsessions.OSEnv(), cwd)
	found = filterDiscoveredByAgent(found, options.agent)

	if options.json {
		if err := writePrettyJSON(stdout, redaction.RedactValue(discoveredSnapshots(found), redaction.Options{})); err != nil {
			return exitCrash
		}
		return reportDiscoveryProblems(stderr, problems)
	}
	if _, err := fmt.Fprintln(stdout, formatDiscoveredSessions(found, cwd)); err != nil {
		return exitCrash
	}
	return reportDiscoveryProblems(stderr, problems)
}

// reportDiscoveryProblems mentions adapters that failed without failing the
// command. What did work is still worth having.
func reportDiscoveryProblems(stderr io.Writer, problems []error) int {
	for _, problem := range problems {
		// AN ERROR STRING IS NOT AUTOMATICALLY SAFE. These wrap paths and ids read
		// out of another agent's store, so the untrusted bytes arrive here wearing
		// an error's clothing rather than a field's.
		fmt.Fprintln(stderr, "warning: "+agentsessions.DisplayField(problem.Error()))
	}
	return exitSuccess
}

func filterDiscoveredByAgent(found []agentsessions.ForeignSession, agent string) []agentsessions.ForeignSession {
	wanted := strings.TrimSpace(agent)
	if wanted == "" {
		return found
	}
	filtered := make([]agentsessions.ForeignSession, 0, len(found))
	for _, session := range found {
		if strings.EqualFold(session.Agent, wanted) {
			filtered = append(filtered, session)
		}
	}
	return filtered
}

type discoveredSnapshot struct {
	Agent     string `json:"agent"`
	Ref       string `json:"ref"`
	ID        string `json:"id"`
	Title     string `json:"title"`
	Cwd       string `json:"cwd"`
	GitBranch string `json:"gitBranch,omitempty"`
	ModelID   string `json:"modelId,omitempty"`
	StartedAt string `json:"startedAt,omitempty"`
	UpdatedAt string `json:"updatedAt,omitempty"`
	Path      string `json:"path"`
}

func discoveredSnapshots(found []agentsessions.ForeignSession) []discoveredSnapshot {
	out := make([]discoveredSnapshot, 0, len(found))
	for _, session := range found {
		out = append(out, discoveredSnapshot{
			Agent:     agentsessions.DisplayField(session.Agent),
			Ref:       agentsessions.DisplayField(session.Agent + ":" + session.ID),
			ID:        agentsessions.DisplayField(session.ID),
			Title:     agentsessions.DisplayField(session.Title),
			Cwd:       agentsessions.DisplayField(session.Cwd),
			GitBranch: agentsessions.DisplayField(session.GitBranch),
			ModelID:   agentsessions.DisplayField(session.ModelID),
			StartedAt: formatDiscoveredTime(session.StartedAt),
			UpdatedAt: formatDiscoveredTime(session.UpdatedAt),
			Path:      agentsessions.DisplayField(session.Path),
		})
	}
	return out
}

func formatDiscoveredTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339)
}

// describeAge renders a last-activity stamp the way a person scanning a list
// wants it: a clock time for today, a date for anything older.
func describeAge(value time.Time, now time.Time) string {
	if value.IsZero() {
		return ""
	}
	local, reference := value.Local(), now.Local()
	switch {
	case local.Year() == reference.Year() && local.YearDay() == reference.YearDay():
		return "today " + local.Format("15:04")
	case local.Year() == reference.Year():
		return local.Format("Jan _2 15:04")
	default:
		return local.Format("2006-01-02")
	}
}

func formatDiscoveredSessions(found []agentsessions.ForeignSession, cwd string) string {
	if len(found) == 0 {
		where := "this workspace"
		if strings.TrimSpace(cwd) == "" {
			where = "any workspace"
		}
		return "No sessions from other coding agents were found for " + where + ".\n" +
			"Agents this build can read: " + strings.Join(agentsessions.AdapterNames(agentsessions.OSEnv()), ", ") + "."
	}

	lines := []string{fmt.Sprintf("%d session(s) from other coding agents:", len(found)), ""}
	for _, session := range found {
		age := ""
		if !session.UpdatedAt.IsZero() {
			age = describeAge(session.UpdatedAt, time.Now())
		}
		// EVERY FIELD HERE IS ANOTHER PRODUCT'S BYTES. The id, title, branch and
		// model all come out of a file this program did not write, and they were
		// printed straight to the terminal: an escape repaints the listing, and a
		// title is frequently the user's first prompt, which is exactly where a
		// pasted credential ends up. The --json path is structurally escaped and
		// redacted already; this is the human-readable one.
		header := session.Agent + ":" + agentsessions.DisplayField(session.ID)
		lines = append(lines, header)
		detail := "    " + agentsessions.DisplayField(session.Title)
		lines = append(lines, detail)
		meta := []string{}
		if session.GitBranch != "" {
			meta = append(meta, "branch "+agentsessions.DisplayField(session.GitBranch))
		}
		if session.ModelID != "" {
			meta = append(meta, agentsessions.DisplayField(session.ModelID))
		}
		if age != "" {
			meta = append(meta, age)
		}
		if len(meta) > 0 {
			lines = append(lines, "    "+strings.Join(meta, "  ·  "))
		}
		lines = append(lines, "")
	}
	lines = append(lines,
		"Import one with:  zero sessions import <agent>:<id>",
		"Then continue it: zero exec --resume <zero-session-id> \"…\"",
	)
	return strings.Join(lines, "\n")
}

// runSessionsImport copies one foreign session into Zero's own store, after
// which every existing session verb — resume, fork, rewind, compact — works on
// it unchanged.
func runSessionsImport(store *sessions.Store, ref string, options sessionCommandOptions, stdout io.Writer, stderr io.Writer) int {
	env := agentsessions.OSEnv()
	adapter, id, err := agentsessions.ParseRef(env, ref)
	if err != nil {
		// The ref is user-supplied and echoed back by ParseRef's message.
		return writeExecUsageError(stderr, agentsessions.DisplayField(err.Error()))
	}

	result, err := agentsessions.Import(store, adapter, id, agentsessions.ReadOptions{
		MaxEvents:        options.maxEvents,
		IncludeReasoning: options.includeReasoning,
	})
	if err != nil {
		return writeAppError(stderr, agentsessions.DisplayField(err.Error()), exitCrash)
	}

	if options.json {
		if err := writePrettyJSON(stdout, redaction.RedactValue(map[string]any{
			"sessionId": result.Session.SessionID,
			"title":     agentsessions.DisplayField(result.Session.Title),
			"cwd":       agentsessions.DisplayField(result.Session.Cwd),
			"events":    result.Events,
			"source":    discoveredSnapshots([]agentsessions.ForeignSession{result.Source})[0],
		}, redaction.Options{})); err != nil {
			return exitCrash
		}
		return exitSuccess
	}

	lines := importSummaryLines(result)
	if warning := importWorkspaceWarning(result.Source.Cwd); warning != "" {
		lines = append(lines, "", warning)
	}
	lines = append(lines, "",
		"Continue it with:",
		"  zero exec --resume "+result.Session.SessionID+" \"Summarise where this left off and what remains\"",
	)
	if _, err := fmt.Fprintln(stdout, strings.Join(lines, "\n")); err != nil {
		return exitCrash
	}
	return exitSuccess
}

// importSummaryLines renders the human-readable block for one import. The
// --json branch beside it is structurally escaped and redacted; this is the one
// that writes another product's bytes to a terminal, so every foreign field goes
// through DisplayField.
//
// A SEPARATE FUNCTION SO THE SANITIZING CAN BE PROVED. The title and cwd come
// back from a store that Import now cleans on the way in, so reached only
// through runSessionsImport these calls are unobservable — a test would pass
// with or without them and pin nothing. They are not redundant: Import began
// sanitizing at this change, so every session imported by an earlier build still
// holds exactly what the foreign transcript said, and this block is what draws
// it. Taking an ImportResult directly is what lets a test supply that record.
func importSummaryLines(result agentsessions.ImportResult) []string {
	return []string{
		"Imported " + result.Source.Agent + " session " + agentsessions.DisplayField(result.Source.ID),
		"",
		"  zero session: " + result.Session.SessionID,
		"  title:        " + displayOrNone(agentsessions.DisplayField(result.Session.Title)),
		"  cwd:          " + displayOrNone(agentsessions.DisplayField(result.Session.Cwd)),
		fmt.Sprintf("  events:       %d", result.Events),
	}
}

// importWorkspaceWarning flags a session that ran somewhere else. Resuming it
// here is allowed — that is a reasonable thing to want — but the file paths in
// its transcript will refer to a different tree, and silence about that is how
// someone spends ten minutes wondering why nothing matches.
func importWorkspaceWarning(sessionCwd string) string {
	recorded := strings.TrimSpace(sessionCwd)
	if recorded == "" {
		return ""
	}
	working, err := os.Getwd()
	if err != nil {
		return ""
	}
	return importWorkspaceWarningForOS(recorded, working, runtime.GOOS)
}

func importWorkspaceWarningForOS(recorded string, working string, goos string) string {
	if pathsEqualForOS(working, recorded, goos) {
		return ""
	}
	// The comparison above runs on the recorded path because that is the
	// functional question; the SENTENCE prints another agent's bytes, so it goes
	// through the same sanitizer as every other displayed foreign field.
	return "Note: this session ran in " + agentsessions.DisplayField(recorded) + ", not the current directory.\n" +
		"      Paths mentioned in it refer to that tree."
}

func pathsEqualForOS(left string, right string, goos string) bool {
	if goos == "windows" && windowsPathCategory(left) != windowsPathCategory(right) {
		return false
	}
	left = cleanPathForOS(left, goos)
	right = cleanPathForOS(right, goos)
	if left == "." || right == "." {
		return false
	}
	if goos == "windows" {
		if strings.EqualFold(left, right) {
			return true
		}
	} else if left == right {
		return true
	}
	// Semantic identity is meaningful only when goos describes the host running
	// the comparison. Tests also exercise foreign path syntax (for example,
	// Windows paths on Unix), where handing those strings to os.Stat would inspect
	// unrelated host paths. On the native host, SameFile recognizes symlinks,
	// junctions, short names, and other aliases that lexical cleaning cannot.
	if goos != runtime.GOOS {
		return false
	}
	if goos == "windows" && (!importWorkspaceLocal(left) || !importWorkspaceLocal(right)) {
		return false
	}
	leftInfo, leftErr := importWorkspaceStat(left)
	rightInfo, rightErr := importWorkspaceStat(right)
	return leftErr == nil && rightErr == nil && os.SameFile(leftInfo, rightInfo)
}

var importWorkspaceLocal = agentsessions.PassiveWorkspaceIsLocal
var importWorkspaceStat = os.Stat

func windowsPathCategory(value string) string {
	value = strings.TrimSpace(strings.ReplaceAll(value, `\`, "/"))
	if len(value) >= 2 && value[1] == ':' && ((value[0] >= 'a' && value[0] <= 'z') || (value[0] >= 'A' && value[0] <= 'Z')) {
		if len(value) >= 3 && value[2] == '/' {
			return "drive-rooted"
		}
		return "drive-relative"
	}
	if strings.HasPrefix(value, "//") {
		return "unc"
	}
	if strings.HasPrefix(value, "/") {
		return "root-relative"
	}
	return "relative"
}

func cleanPathForOS(value string, goos string) string {
	value = strings.TrimSpace(value)
	if goos == "windows" {
		return path.Clean(strings.ReplaceAll(value, `\`, "/"))
	}
	return filepath.Clean(value)
}

func displayOrNone(value string) string {
	if strings.TrimSpace(value) == "" {
		return "(none)"
	}
	return value
}
