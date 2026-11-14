// Package agentsessions reads the session transcripts other coding agents
// (Claude Code, Codex, Factory Droid, Pi, …) leave on the local disk and
// translates them into Zero session events, so work started elsewhere can be
// continued in Zero.
//
// Three rules hold for every adapter in this package, and the tests enforce
// them rather than trusting the code to be careful:
//
//  1. READ ONLY. Nothing here writes to, moves, or locks another agent's store.
//
//  2. PATH-EXACT, NEVER A WALK. Every one of these agents keeps live
//     credentials in the same directory tree as its transcripts —
//     ~/.codex/auth.json, ~/.grok/auth.json, ~/.factory/auth.v2.key, and
//     most pointedly ~/.pi/agent/auth.json, which is the sibling of
//     ~/.pi/agent/sessions/. A filepath.WalkDir rooted one level too high
//     reads a live OAuth token. So discovery uses fixed-depth globs against a
//     specific extension and nothing else. This is repo invariant #2
//     (allow-lists, not deny-lists) applied to filesystem input.
//
//  3. IMPORTED TEXT IS UNTRUSTED (invariant #8). Every string crossing into
//     Zero's event log goes through internal/redaction first (invariant #6:
//     redaction is a chokepoint, not a sprinkle) — a foreign transcript can
//     easily contain a key the other agent echoed into its own log.
package agentsessions

import (
	"os"
	"time"

	"github.com/Twigpine/zero/internal/sessions"
)

// ForeignSession is the index entry for one session belonging to another agent.
// It is built during discovery from a deliberately cheap read — enough to list
// and choose, never the whole transcript. A single Claude Code transcript on a
// working machine can exceed 70 MB, so anything that requires parsing the full
// file belongs in Read, not here.
type ForeignSession struct {
	// Agent is the adapter name ("claude-code", "codex", …).
	Agent string
	// ID identifies the session within that agent. It is the adapter's own
	// identifier, not a path, so it survives being printed and pasted back.
	ID string
	// Title is a short human label, usually derived from the first user prompt.
	Title string
	// Cwd is the working directory the session ran in, taken from the record
	// body rather than from the directory name. See paths.go: the slugged
	// directory name these agents use is lossy and cannot be reversed.
	Cwd string
	// GitBranch is the branch recorded at the time, when the agent stores one.
	GitBranch string
	// ModelID is the model the other agent was using, when recorded.
	ModelID string
	// StartedAt and UpdatedAt bound the session in time. UpdatedAt falls back to
	// the file's modification time, which avoids reading the tail of a large
	// transcript just to learn when it stopped.
	StartedAt time.Time
	UpdatedAt time.Time
	// Path is the file or directory backing the session, shown for
	// troubleshooting and used by Read to reopen it.
	Path string
	// source binds this index row to the exact regular file observed during
	// discovery. It is intentionally unexported: paths and foreign IDs are
	// display/provenance, not authority for opening an arbitrary file.
	source sourceSnapshot
}

type sourceSnapshot struct {
	info    os.FileInfo
	size    int64
	modTime time.Time
}

// ReadOptions tunes a full read. The zero value is the intended default:
// reasoning dropped, with a defensive event cap.
type ReadOptions struct {
	// MaxEvents caps how many events a session contributes, keeping the LAST
	// MaxEvents. The tail is what matters for continuing work — the most recent
	// exchanges are the ones a resume needs. Zero uses the package's bounded
	// default; a positive value is used as the explicit cap.
	MaxEvents int
	// Cwd is the session's working directory, used only to shorten absolute
	// paths in the activity summary. Empty just means paths stay absolute.
	Cwd string
	// IncludeReasoning keeps the other model's thinking/reasoning blocks. Off by
	// default: they are provider-private, frequently larger than the visible
	// conversation, and of no use to a different model that will not be
	// continuing that chain of thought.
	IncludeReasoning bool
}

// Adapter is one foreign agent's on-disk session store.
//
// Every adapter is independently skippable. A store that is absent, unreadable,
// or has quietly changed shape must yield zero sessions rather than failing the
// command — these are undocumented formats belonging to other products, and one
// vendor shipping a new layout must not break discovery for the other six.
// That fail-soft rule applies to DISCOVERY only; Read reports its errors, since
// by then the user has named a specific session and silence would be a lie.
type Adapter interface {
	// Name is the stable identifier used in "<agent>:<id>" and in output.
	Name() string
	// Discover returns the sessions this adapter believes belong to cwd. An
	// empty cwd means "every session this adapter can see".
	Discover(cwd string) ([]ForeignSession, error)
	// Read translates the exact session returned by Discover into Zero events.
	// Implementations verify the source identity before and after reading so
	// metadata and content cannot come from independently-resolved files.
	Read(source ForeignSession, options ReadOptions) ([]sessions.AppendEventInput, error)
}
