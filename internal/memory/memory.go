// Package memory is a durable, scoped note store that survives a session.
//
// WHAT IT IS FOR. A session ends and everything it worked out goes with it —
// the convention this repo actually follows, the decision behind a strange-
// looking guard, the finding an audit already confirmed. AGENTS.md holds what
// someone sat down and wrote; this holds what accumulates, and it is scoped so
// the two do not become one file nobody prunes.
//
// NOT A SECOND PLAN STORE. The "no new store" invariant is about PLAN STATE —
// "plan state is session events", ARCHITECTURE.md — because two stores for one
// fact eventually disagree. Notes are not derivable from any event log, so there
// is nothing here for a second store to contradict.
//
// THE SAFETY RULES ARE PLAN_STORE'S, deliberately reused rather than rewritten:
// an allow-list name that cannot spell a traversal component, containment of
// every operation to a handle on the workspace (internal/pathjail), and an
// O_EXCL temp file renamed into place. A note store is a write primitive pointed
// at a path the model chooses, which is the same shape as "save my plan" and
// needs the same answers.
package memory

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/Gitlawb/zero/internal/pathjail"
)

// Scope is where a note lives, and who else sees it.
type Scope string

const (
	// ScopeProject is checked in beside the repo: shared with everyone who clones
	// it, and therefore reviewed like any other file in the tree.
	ScopeProject Scope = "project"
	// ScopeLocal is this machine only. The natural home for anything specific to
	// one checkout, one operator, or one afternoon.
	ScopeLocal Scope = "local"
)

// fileExt is the stored extension. Markdown with frontmatter, because a note is
// meant to be readable by the person whose repo it is sitting in.
const fileExt = ".md"

// gitignoreName and localIgnoreContent are what makes the local store private.
const gitignoreName = ".gitignore"

const localIgnoreContent = "# Notes saved to the local scope stay on this machine.\n*\n"

// tempExt is what an in-progress write carries. Deliberately not fileExt, so a
// temp file a crash left behind is never listed as a note.
const tempExt = ".tmp"

// maxNoteBytes bounds one note. Generous for prose, small enough that a runaway
// write cannot quietly fill a repo.
const maxNoteBytes = 64 << 10

// maxDescriptionBytes bounds the ONE-LINE summary, separately from the note.
//
// The listing prints every note's description, so the field is shared screen
// space: the total-size check alone let a single note carry a 60 KiB single-line
// description and consume the listing everyone else has to fit in. A description
// is a sentence telling a reader whether to open the note — anything past this is
// the note's own body in the wrong field, so it is truncated rather than refused.
const maxDescriptionBytes = 200

// namePattern is an ALLOW-LIST, the same rule plan names use
// (specialist/manifest.go): enumerate what is permitted rather than forbidding
// traversal, because every deny-list in this repo has leaked at least once.
//
// LOWERCASE ONLY, and that is the point rather than a style choice. Windows and
// a default APFS volume fold case, so "Findings" and "findings" are one file
// there: writing the second silently replaced the first's body, a read could
// hand the model a note under a name it does not hold, and deleting one removed
// the other. Refusing the second spelling makes the collision unrepresentable
// rather than resolving it after the damage.
var namePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)

// reservedDeviceNames are the DOS device names Win32 resolves ahead of any file
// with the same stem. os.Root addresses a note relative to a directory handle
// and bypasses that parsing entirely, so "con.md" is created, listed and read
// like any other note — which is exactly what makes this easy to miss.
//
// GIT is what breaks on them. `git add -A` fails outright on such a path and
// stages NOTHING, including the user's unrelated edits, while `git status` never
// names the file, so there is no route from the symptom back to the cause. The
// other direction is worse: a note committed from macOS makes the repo
// un-checkoutable on Windows — the clone fails and leaves an empty tree, so a
// Windows contributor gets no repository at all, not merely no note.
var reservedDeviceNames = map[string]bool{
	"con": true, "prn": true, "aux": true, "nul": true,
	"com0": true, "com1": true, "com2": true, "com3": true, "com4": true,
	"com5": true, "com6": true, "com7": true, "com8": true, "com9": true,
	"lpt0": true, "lpt1": true, "lpt2": true, "lpt3": true, "lpt4": true,
	"lpt5": true, "lpt6": true, "lpt7": true, "lpt8": true, "lpt9": true,
}

var (
	ErrBadName  = errors.New("a memory name must be lowercase, start with a letter, use only letters, digits and hyphen, be at most 64 characters, and not be a reserved device name")
	ErrNoStore  = errors.New("memory is not available in this run")
	ErrTooLarge = fmt.Errorf("a memory note may be at most %d bytes", maxNoteBytes)
	ErrNotFound = errors.New("no such memory")
	ErrBadScope = errors.New(`scope must be "project" or "local"`)
	// ErrNotPrivate is returned rather than writing a local note the repository
	// would then track. The local scope's whole promise is that the note stays on
	// this machine, and a promise that degrades quietly is worse than one that
	// refuses.
	ErrNotPrivate = errors.New("the local memory store is not ignored by git, so a note saved there would not stay on this machine")
	// ErrIsSymlink is pathjail's refusal, kept under this package's own name so
	// callers testing for it keep working. It now covers a Windows junction as
	// well as a symlink, which the old ModeSymlink-only check did not.
	ErrIsSymlink = pathjail.ErrReparse
)

// Paths locates the two scopes. An empty directory means that scope is simply
// unavailable, and a write to it is refused with a reason rather than silently
// written somewhere else.
type Paths struct {
	// Root is the containment boundary. Every operation below is performed
	// relative to a handle on it, so no component of ProjectDir or LocalDir can
	// redirect a write or a delete outside the tree. Empty means no store: a
	// boundary is not optional, because without one the directories below are
	// just strings the filesystem re-resolves on every syscall.
	Root       string
	ProjectDir string
	LocalDir   string
}

// DefaultPaths puts project memory beside the repo and local memory in a
// subdirectory of it.
//
// The local store makes itself private on first write (keepLocalScopePrivate)
// rather than relying on a line in the workspace's .gitignore: this runs in
// whatever repository the user opened, and a rule that lives in one repo's
// ignore file protects only that repo.
func DefaultPaths(workspaceRoot string) Paths {
	if strings.TrimSpace(workspaceRoot) == "" {
		return Paths{}
	}
	base := filepath.Join(workspaceRoot, ".zero", "memory")
	return Paths{Root: workspaceRoot, ProjectDir: base, LocalDir: filepath.Join(base, "local")}
}

// Available reports whether this run has a usable store.
//
// ROOT COUNTS, and that is the fix rather than a nicety. openScope refuses a
// blank Root with ErrNoStore, and List treats ErrNoStore as "that scope is not
// configured" and moves on — so a Paths carrying both directories and no Root
// produced an empty listing, and the caller told the user there were no notes
// when the store was in fact switched off. Callers ask here instead of testing
// the fields themselves, so the rule has one home and cannot drift.
func (paths Paths) Available() bool {
	if strings.TrimSpace(paths.Root) == "" {
		return false
	}
	return paths.ProjectDir != "" || paths.LocalDir != ""
}

func (paths Paths) dirFor(scope Scope) (string, error) {
	switch scope {
	case ScopeProject:
		if paths.ProjectDir == "" {
			return "", ErrNoStore
		}
		return paths.ProjectDir, nil
	case ScopeLocal:
		if paths.LocalDir == "" {
			return "", ErrNoStore
		}
		return paths.LocalDir, nil
	default:
		return "", ErrBadScope
	}
}

// openScope opens a handle on the containment root and returns the scope's
// store directory relative to it. The caller closes the handle.
//
// Every filesystem operation in this file goes through here. The store used to
// Lstat its own directory and file and then hand those same strings to
// MkdirAll, CreateTemp, Rename and Remove, which re-resolve every ancestor: a
// link anywhere above the store redirected the write, and the checks passed
// because they were looking at the wrong components. On Windows they also
// missed a junction outright, since a junction is a reparse point but not a
// symlink.
func (paths Paths) openScope(scope Scope) (*os.Root, string, error) {
	dir, err := paths.dirFor(scope)
	if err != nil {
		return nil, "", err
	}
	if strings.TrimSpace(paths.Root) == "" {
		return nil, "", ErrNoStore
	}
	handle, relative, err := pathjail.Open(paths.Root, dir)
	if err != nil {
		return nil, "", err
	}
	// EVERY COMPONENT, not only the note. os.Root refuses to traverse OUT of the
	// workspace, which is what the comment above was relying on — but it happily
	// follows a link that resolves back INSIDE it, and the store path is checked
	// in. A repository shipping ".zero -> redirected" redirected every read and
	// write to another directory in the same tree while each individual check
	// passed, because the only component ever inspected was the note file at the
	// end.
	if err := refuseReparseChain(handle, paths.Root, relative); err != nil {
		handle.Close()
		return nil, "", err
	}
	return handle, relative, nil
}

// refuseReparseChain refuses a link or reparse point at every component of
// relative, outermost first.
//
// Built on pathjail.RefuseReparse rather than reimplementing the test, so the
// Windows junction handling and the trailing-separator care stay in one place.
// Outermost first because that is the component whose redirection decides where
// everything below it lands, and it makes the error name the link the caller can
// actually act on.
func refuseReparseChain(handle *os.Root, root string, relative string) error {
	relative = filepath.Clean(relative)
	if relative == "." || relative == string(filepath.Separator) {
		return nil
	}
	parts := strings.Split(relative, string(filepath.Separator))
	absentAt := ""
	for i := range parts {
		component := filepath.Join(parts[:i+1]...)
		if err := pathjail.RefuseReparse(handle, component); err != nil {
			return err
		}
		// A COMPONENT CANNOT EXIST INSIDE ONE THAT DOES NOT. RefuseReparse reports
		// absence as fine, which is right on its own — a store that has not been
		// created yet is what a first write is for. But absence must then hold all
		// the way down, and where it does not, the handle is refusing to traverse
		// something rather than telling us the path is empty: a Windows junction
		// at an ancestor reports that way, and the store beneath it then read as
		// simply having no notes. "Refused" and "absent" are different answers,
		// and only one of them should let a caller conclude its notes are gone.
		exists, err := componentExists(handle, component)
		if err != nil {
			return err
		}
		if !exists {
			// ABSENT TO THE HANDLE IS NOT ABSENT. A store that has not been
			// created yet is what a first write is for, and reporting that as a
			// problem would break every clean workspace. But a component the
			// confined handle will not open while it is PRESENT ON DISK has been
			// REFUSED, and a Windows junction whose target leaves the workspace
			// reports exactly that way: the chain read it and everything under it
			// as missing, the later open became ErrNotFound, and List drops
			// ErrNotFound — so a tampered store presented as an empty one.
			//
			// That is the outcome memory.go's own contract forbids, because the
			// model then concludes its notes are gone and writes over whatever is
			// really there. Absence and refusal have to be different answers.
			if presentOnDisk(root, component) {
				return fmt.Errorf("%w: %s cannot be opened inside the workspace", ErrIsSymlink, component)
			}
			if absentAt == "" {
				absentAt = component
			}
			continue
		}
		if absentAt != "" {
			return fmt.Errorf("%w: %s is unreadable, so %s cannot be inspected",
				ErrIsSymlink, absentAt, component)
		}
	}
	return nil
}

// presentOnDisk reports whether relative exists under root by ordinary pathname.
//
// Deliberately NOT through the confined handle — the whole point is to ask a
// different question than the handle answers, so that "the handle will not open
// this" can be told apart from "there is nothing here". It stats and never
// opens or reads, so nothing is traversed on the strength of this answer; it
// only decides which ERROR the caller is given.
func presentOnDisk(root string, relative string) bool {
	if strings.TrimSpace(root) == "" {
		return false
	}
	_, err := os.Lstat(filepath.Join(root, relative))
	return err == nil
}

// componentExists reports whether relative is present, without following a link.
func componentExists(handle *os.Root, relative string) (bool, error) {
	if _, err := handle.Lstat(relative); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("inspect %s: %w", relative, err)
	}
	return true, nil
}

// Note is one stored memory.
type Note struct {
	Name string
	// Description is the one-line summary from frontmatter. It is what a listing
	// shows, so a reader can decide what to open WITHOUT reading everything —
	// which is the whole reason notes carry frontmatter at all.
	Description string
	Scope       Scope
	Body        string
}

// ValidName reports whether a name is storable.
//
// The reserved check is separate from the pattern because it is a different kind
// of rule: the pattern says which characters may appear, this says which
// otherwise-legal spellings the platform will not let git carry.
func ValidName(name string) bool {
	return name != "" && len(name) <= 64 &&
		namePattern.MatchString(name) && !reservedDeviceNames[name]
}

// ResolveScopes turns a requested scope into the scopes to search.
//
// ONE place decides, because there were two and they disagreed: the write path
// validated through memory.Scope and refused an unknown value, while the read
// path mapped every unrecognised spelling to BOTH stores. A typo therefore
// widened access on the only path where widening matters, and a perfectly valid
// "project" was ignored on the listing path, which always read both. An empty
// request means "search everywhere"; anything non-empty must be a scope this
// package knows.
func ResolveScopes(requested string) ([]Scope, error) {
	trimmed := strings.TrimSpace(requested)
	if trimmed == "" {
		return []Scope{ScopeProject, ScopeLocal}, nil
	}
	switch scope := Scope(strings.ToLower(trimmed)); scope {
	case ScopeProject, ScopeLocal:
		return []Scope{scope}, nil
	default:
		return nil, ErrBadScope
	}
}

// List returns every note in the given scopes, project first, each sorted by
// name, along with any store that could not be read.
//
// LOCAL SHADOWS NOTHING. Unlike saved plans, where project shadows user because
// a repo's own plan is what its contributors should get, both scopes are listed:
// they hold different KINDS of thing, and hiding one behind the other would lose
// a note rather than resolve a conflict.
//
// The error is JOINED rather than returned in place of the notes: a store that
// cannot be read must be reported, but the notes that did read are still the
// best answer available, and dropping them would turn one unreadable file into
// an empty memory.
func List(paths Paths, scopes ...Scope) ([]Note, error) {
	if len(scopes) == 0 {
		scopes = []Scope{ScopeProject, ScopeLocal}
	}
	var out []Note
	var problems []error
	for _, scope := range scopes {
		handle, relative, err := paths.openScope(scope)
		if err != nil {
			// A scope this store is not configured for is not a failure to
			// report; a bad scope name is.
			if !errors.Is(err, ErrNoStore) {
				problems = append(problems, err)
			}
			continue
		}
		directory, err := handle.Open(relative)
		if err != nil {
			handle.Close()
			// A store that has never been written to has no directory yet, and
			// that is an empty list rather than a failure. Anything else is an
			// operational error and is reported.
			if !os.IsNotExist(err) {
				problems = append(problems, fmt.Errorf("open %s store: %w", scope, err))
			}
			continue
		}
		entries, err := directory.ReadDir(-1)
		directory.Close()
		handle.Close()
		if err != nil {
			problems = append(problems, fmt.Errorf("read %s store: %w", scope, err))
			continue
		}
		var scoped []Note
		for _, entry := range entries {
			// The extension is matched EXACTLY, not case-insensitively. Read
			// reopens name+fileExt, which is always lowercase, so accepting
			// "notes.MD" here listed an entry that the very next Read could not
			// open on a case-sensitive filesystem — and the error was swallowed
			// below, so the note vanished from the listing with no explanation.
			// This store only ever writes lowercase, so an exact match is the
			// spelling that keeps List and Read agreeing.
			if entry.IsDir() || filepath.Ext(entry.Name()) != fileExt {
				continue
			}
			name := strings.TrimSuffix(entry.Name(), fileExt)
			note, err := Read(paths, scope, name)
			if err != nil {
				// A name the store would not accept, or a note deleted between
				// the ReadDir and the read, is legitimately not listable. A
				// refused link, an oversized file or a permission error is a
				// problem the caller needs told about rather than a note that
				// silently vanishes from the listing.
				if !errors.Is(err, ErrNotFound) && !errors.Is(err, ErrBadName) {
					problems = append(problems, fmt.Errorf("read %s/%s: %w", scope, name, err))
				}
				continue
			}
			scoped = append(scoped, note)
		}
		sort.Slice(scoped, func(i, j int) bool { return scoped[i].Name < scoped[j].Name })
		out = append(out, scoped...)
	}
	return out, errors.Join(problems...)
}

// Read returns one note.
func Read(paths Paths, scope Scope, name string) (Note, error) {
	if !ValidName(name) {
		return Note{}, ErrBadName
	}
	handle, relative, err := paths.openScope(scope)
	if err != nil {
		return Note{}, err
	}
	defer handle.Close()
	relativePath := filepath.Join(relative, name+fileExt)
	// Reads are confined by the same rule as writes. A note read through a link
	// is an exfiltration primitive in a tool the model can call by name — the
	// write hole with the arrow reversed — and Write and Forget both refuse a
	// reparse point at this position, so a read that did not was the asymmetry.
	//
	// WHAT THIS COVERS, PRECISELY. os.Root already refuses a link whose target
	// leaves the root, so what this adds is refusing one that stays INSIDE it:
	// ".zero/memory/linked.md -> ../../secret.txt" resolves to a path still under
	// Paths.Root and was served happily. It is a REPARSE-POINT guard, not an
	// identity check — a HARD LINK carries no reparse bit, so neither this nor
	// os.Root can see one, and a hard link at a note position still reads through
	// to its target. Closing that needs identity (st_dev/st_ino, or the Windows
	// file id), which is deliberately not attempted here so that the claim in
	// this comment matches what the code actually does.
	if err := pathjail.RefuseReparse(handle, relativePath); err != nil {
		return Note{}, err
	}
	body, err := readBounded(handle, relativePath)
	if err != nil {
		if os.IsNotExist(err) {
			return Note{}, ErrNotFound
		}
		// Anything else is an operational failure — a permission error, an
		// oversized file, a corrupt store — and reaches the caller as itself.
		// Reporting it as "no such note" tells the model the note is absent, and
		// the next thing it does is write the note again over whatever is there.
		return Note{}, err
	}
	description, text := splitFrontmatter(string(body))
	return Note{Name: name, Description: description, Scope: scope, Body: text}, nil
}

// keepLocalScopePrivate installs, or verifies, the ignore that keeps local notes
// out of the repository.
//
// "local" promises the note stays on this machine, and it did not: the store
// lives at <workspace>/.zero/memory/local, inside the working tree, so a default
// write showed up in git status and could be committed — and the local scope is
// the DEFAULT, so that is the ordinary path rather than a corner.
//
// The ignore file lives INSIDE the store rather than as a line in the repo's
// .gitignore, because this tool runs in whatever workspace the user opened. A
// line in zero's own .gitignore would protect exactly one repository; a store
// that makes itself private travels with every one. "*" covers the notes and the
// ignore file itself, so the directory contributes nothing to the index.
//
// IT FAILS CLOSED, and used to fail open. This was best-effort with no error
// return, on the reasoning that a store which cannot hold an ignore file is
// still a working store and failing the write would trade privacy for an outage.
// That reasoning is wrong for this particular function, because O_EXCL cannot
// tell "a previous run wrote the ignore" from "something else is already there":
// precreating an empty .gitignore made every subsequent local write succeed with
// the store fully tracked. The promise is the feature here — a note the user was
// told stays on this machine, sitting in git status, is worse than a refused
// write, because the refusal is visible and the leak is not.
func keepLocalScopePrivate(handle *os.Root, scope Scope, relative string) error {
	if scope != ScopeLocal {
		return nil
	}
	ignorePath := filepath.Join(relative, gitignoreName)
	// O_EXCL still decides whether this is the first write, in one syscall — but
	// now the "already exists" answer leads to a check rather than to silence.
	file, err := handle.OpenFile(ignorePath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	switch {
	case err == nil:
		defer file.Close()
		if _, writeErr := file.WriteString(localIgnoreContent); writeErr != nil {
			return fmt.Errorf("write %s: %w", ignorePath, writeErr)
		}
		return nil
	case !errors.Is(err, fs.ErrExist):
		return fmt.Errorf("create %s: %w", ignorePath, err)
	}
	// THE IGNORE FILE ITSELF MUST NOT BE A LINK. git does not follow a symlinked
	// .gitignore — it warns and treats the rule as absent — so reading through
	// one and accepting the target's "*" certified a privacy rule that is not in
	// force. An in-workspace RELATIVE link is followed by os.Root (an absolute
	// one is refused, which is why an earlier check of this looked clean), so the
	// bypass needed nothing outside the workspace.
	if err := pathjail.RefuseReparse(handle, ignorePath); err != nil {
		return fmt.Errorf("%w: %s is a link, and git does not read one: %v", ErrNotPrivate, ignorePath, err)
	}
	existing, err := readBounded(handle, ignorePath)
	if err != nil {
		return fmt.Errorf("read %s: %w", ignorePath, err)
	}
	if !ignoresEverything(string(existing)) {
		return fmt.Errorf("%w: %s", ErrNotPrivate, ignorePath)
	}
	return nil
}

// ignoresEverything reports whether an existing ignore file actually excludes the
// whole directory.
//
// A bare "*" is what this store writes, so that is what is recognised; anything
// narrower is treated as not covering, because guessing at the effect of an
// arbitrary pattern set is how a privacy check ends up agreeing with a file that
// does not protect anything. A re-inclusion line cancels the cover no matter
// where it sits, so one "!" is enough to fail the whole file.
//
// IT FOLLOWS GIT'S WHITESPACE RULES, and trimming each line got both of them
// backwards. Verified against git 2.55.0 rather than read off the spec:
//
//	"  *"    gate said covered; git does NOT ignore, because LEADING whitespace
//	         is part of the pattern — so a note the user was told stays on this
//	         machine was picked up by a routine `git add -A`, which is the exact
//	         outcome this function exists to prevent, arrived at silently
//	"\ufeff*"  gate said not covered; git strips a UTF-8 BOM and honours the rule
//	         — so an ignore written by an ordinary editor failed every local write
//
// Leading whitespace is therefore significant and is NOT stripped; trailing
// whitespace is not significant to git and is; a BOM is stripped once, at the
// start of the file, exactly as git does.
func ignoresEverything(content string) bool {
	content = strings.TrimPrefix(content, "\ufeff")
	covered := false
	for _, line := range strings.Split(content, "\n") {
		// TrimRight, not TrimSpace: git drops trailing whitespace from a pattern
		// and keeps leading whitespace as part of it.
		line = strings.TrimRight(strings.TrimSuffix(line, "\r"), " \t")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "!") {
			return false
		}
		if line == "*" {
			covered = true
		}
	}
	return covered
}

// readBounded reads at most maxNoteBytes, refusing anything larger rather than
// allocating it.
//
// The ceiling used to be enforced only on the way IN, so a note that arrived by
// hand or through a clone — project scope is checked in — was read whole however
// large, and List did that for every note in the store. A memory bound has to
// hold on the path that allocates.
func readBounded(handle *os.Root, relativePath string) ([]byte, error) {
	file, err := handle.Open(relativePath)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	// One byte past the ceiling, so a note exactly at the limit still reads and
	// only a genuinely oversized one is refused.
	body, err := io.ReadAll(io.LimitReader(file, maxNoteBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxNoteBytes {
		return nil, fmt.Errorf("%w: %s", ErrTooLarge, relativePath)
	}
	return body, nil
}

// Write stores a note, replacing any note of the same name in the same scope.
//
// Every step runs against a handle on the containment root, so the directory
// this lands in cannot be changed underneath it: create the tree, refuse a link
// at the destination, write an O_EXCL temp file with an unpredictable name,
// rename into place. An edit is therefore atomic, and a crash mid-write leaves
// the previous note rather than a half-written one.
func Write(paths Paths, scope Scope, name, description, body string) (string, error) {
	if !ValidName(name) {
		return "", ErrBadName
	}
	dir, err := paths.dirFor(scope)
	if err != nil {
		return "", err
	}
	content := renderNote(name, description, body)
	if len(content) > maxNoteBytes {
		return "", ErrTooLarge
	}
	handle, relative, err := paths.openScope(scope)
	if err != nil {
		return "", err
	}
	defer handle.Close()
	if err := handle.MkdirAll(relative, 0o700); err != nil {
		return "", fmt.Errorf("create %s: %w", dir, err)
	}
	if err := keepLocalScopePrivate(handle, scope, relative); err != nil {
		return "", err
	}
	path := filepath.Join(dir, name+fileExt)
	relativePath := filepath.Join(relative, name+fileExt)
	if err := pathjail.RefuseReparse(handle, relativePath); err != nil {
		return "", err
	}
	file, temp, err := pathjail.CreateTemp(handle, relative, name, tempExt)
	if err != nil {
		return "", fmt.Errorf("create a temporary file in %s: %w", dir, err)
	}
	writeErr := func() error {
		if _, err := file.WriteString(content); err != nil {
			return err
		}
		return file.Close()
	}()
	if writeErr != nil {
		_ = file.Close()
		_ = handle.Remove(temp)
		return "", fmt.Errorf("write %s: %w", path, writeErr)
	}
	if err := handle.Rename(temp, relativePath); err != nil {
		_ = handle.Remove(temp)
		return "", fmt.Errorf("save %s: %w", path, err)
	}
	return path, nil
}

// Forget removes a note. Missing is not an error: the caller asked for it to be
// gone, and it is.
func Forget(paths Paths, scope Scope, name string) error {
	if !ValidName(name) {
		return ErrBadName
	}
	handle, relative, err := paths.openScope(scope)
	if err != nil {
		return err
	}
	defer handle.Close()
	// A delete is the sharpest of these: a write lands a file, a delete removes
	// somebody else's. Same handle, same reason.
	relativePath := filepath.Join(relative, name+fileExt)
	if err := pathjail.RefuseReparse(handle, relativePath); err != nil {
		return err
	}
	if err := handle.Remove(relativePath); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func renderNote(name, description, body string) string {
	var b strings.Builder
	b.WriteString("---\nname: ")
	b.WriteString(name)
	if trimmed := strings.TrimSpace(description); trimmed != "" {
		b.WriteString("\ndescription: ")
		b.WriteString(boundedDescription(singleLine(trimmed)))
	}
	b.WriteString("\n---\n\n")
	b.WriteString(strings.TrimRight(body, "\n"))
	b.WriteString("\n")
	return b.String()
}

// splitFrontmatter returns the description and the body. A note without
// frontmatter is not an error — it is a file someone wrote by hand, and losing
// it because it lacks a header would be the store punishing the reader it exists
// to serve.
//
// CRLF is accepted as well as LF. Project-scope notes are checked in, and Git for
// Windows defaults to autocrlf=true, so a note that merely round-trips through a
// clone comes back with "---\r\n" — under an LF-only split the whole header,
// delimiters included, fell through into the body and the description was lost.
//
// LINE ENDINGS ARE NORMALISED TO LF in what this returns, on every path. The
// earlier version normalised only for the split and then returned the body from
// whichever string that path happened to hold, so a note WITH frontmatter came
// back as LF and one WITHOUT kept its CRLF — a difference no caller asked for and
// nothing documented. The file on disk is untouched either way; this is only
// what the reader is handed.
// boundedDescription caps the summary so one note cannot crowd out the listing.
// Truncated on a rune boundary with an ellipsis, so the result stays readable and
// is visibly cut rather than looking like the whole of a short description.
func boundedDescription(text string) string {
	if len(text) <= maxDescriptionBytes {
		return text
	}
	const ellipsis = "…"
	// WALK FORWARD ONCE. The first version dropped a rune per iteration and
	// re-encoded the whole slice each time to measure it, which is quadratic in
	// the input — and maxNoteBytes lets a 64 KiB description through the write
	// path, so the cap was at its slowest on exactly the input it exists for:
	//
	//	 1 KiB ->   1.6ms      16 KiB -> 381ms      64 KiB -> 13.3s
	//
	// Ranging over the string yields rune boundaries with their byte offsets
	// directly, so the cut point is found in one pass and no intermediate string
	// is built.
	budget := maxDescriptionBytes - len(ellipsis)
	cut := 0
	for index := range text {
		if index > budget {
			break
		}
		cut = index
	}
	return text[:cut] + ellipsis
}

func splitFrontmatter(content string) (description string, body string) {
	normalized := strings.ReplaceAll(content, "\r\n", "\n")
	if !strings.HasPrefix(normalized, "---\n") {
		return "", normalized
	}
	rest := normalized[len("---\n"):]
	end := strings.Index(rest, "\n---\n")
	if end < 0 {
		return "", normalized
	}
	for _, line := range strings.Split(rest[:end], "\n") {
		if value, ok := strings.CutPrefix(strings.TrimSpace(line), "description:"); ok {
			// Bounded HERE, not only where a note is written. A project-scope
			// note arrives with a clone, so its description never passed through
			// this process's write path — leaving the listing crowdable by a file
			// nobody here created.
			description = boundedDescription(strings.TrimSpace(value))
		}
	}
	return description, strings.TrimLeft(rest[end+len("\n---\n"):], "\n")
}

func singleLine(text string) string {
	return strings.Join(strings.Fields(text), " ")
}
