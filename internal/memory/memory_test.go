package memory

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func testPaths(t *testing.T) Paths {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return DefaultPaths(root)
}

func TestANoteRoundTripsThroughItsScope(t *testing.T) {
	paths := testPaths(t)
	for _, scope := range []Scope{ScopeProject, ScopeLocal} {
		if _, err := Write(paths, scope, "conventions", "how this repo does errors", "Wrap with %w.\n"); err != nil {
			t.Fatalf("%s: write: %v", scope, err)
		}
		note, err := Read(paths, scope, "conventions")
		if err != nil {
			t.Fatalf("%s: read: %v", scope, err)
		}
		if note.Description != "how this repo does errors" {
			t.Errorf("%s: description = %q", scope, note.Description)
		}
		if !strings.Contains(note.Body, "Wrap with %w.") {
			t.Errorf("%s: body = %q", scope, note.Body)
		}
		if note.Scope != scope {
			t.Errorf("scope = %q, want %q", note.Scope, scope)
		}
	}
}

// BOTH SCOPES ARE LISTED. Unlike saved plans, where project shadows user, these
// hold different KINDS of thing — hiding one behind the other would lose a note
// rather than resolve a conflict.
func TestListingShowsBothScopesWithoutShadowing(t *testing.T) {
	paths := testPaths(t)
	// The project notes are written in REVERSE lexical order, so the per-scope
	// sort has to do real work to produce the expected result: remove or reverse
	// it and these come back in insertion order. Ordering is what a reader relies
	// on to find a note in a long listing, so it is worth asserting rather than
	// only checking that both scopes appear.
	if _, err := Write(paths, ScopeProject, "zebra", "last alphabetically", "x"); err != nil {
		t.Fatal(err)
	}
	if _, err := Write(paths, ScopeProject, "shared", "team convention", "x"); err != nil {
		t.Fatal(err)
	}
	if _, err := Write(paths, ScopeProject, "alpha", "first alphabetically", "x"); err != nil {
		t.Fatal(err)
	}
	if _, err := Write(paths, ScopeLocal, "shared", "my own note", "y"); err != nil {
		t.Fatal(err)
	}
	notes, listErr := List(paths)
	if listErr != nil {
		t.Fatal(listErr)
	}
	var got []string
	for _, note := range notes {
		got = append(got, string(note.Scope)+"/"+note.Name)
	}
	want := []string{"project/alpha", "project/shared", "project/zebra", "local/shared"}
	if !slices.Equal(got, want) {
		t.Errorf("listing = %v,\n    want %v — project scope first, each scope sorted by name, and neither scope shadowing the other's \"shared\"", got, want)
	}
}

// THE NAME IS THE PATH GUARD, an allow-list for the same reason plan names use
// one: every deny-list in this repo has leaked at least once.
func TestNamesCannotTraverse(t *testing.T) {
	paths := testPaths(t)
	for _, name := range []string{
		"../escape", "..", ".", "a/b", `a\b`, "a b", "", strings.Repeat("x", 65),
		"~/evil", "a;b", "a\x00b", "note.md",
		// Uppercase and underscore are refused now, not merely unconventional.
		// Windows and a default APFS volume fold case, so "Findings" and
		// "findings" would be one file: refusing the second spelling makes that
		// collision unrepresentable rather than something to resolve afterwards.
		"Findings", "A1", "audit_findings", "1leading",
		// Reserved DOS device names. The store itself handles them fine — os.Root
		// bypasses Win32 device parsing — but `git add -A` then fails on the path
		// and stages NOTHING, and a repo carrying one cannot be checked out on
		// Windows at all.
		"con", "prn", "aux", "nul", "com1", "lpt9", "CON", "Nul",
	} {
		if _, err := Write(paths, ScopeProject, name, "d", "b"); !errors.Is(err, ErrBadName) {
			t.Errorf("Write accepted %q: %v", name, err)
		}
		if _, err := Read(paths, ScopeProject, name); !errors.Is(err, ErrBadName) {
			t.Errorf("Read accepted %q: %v", name, err)
		}
	}
	// A device name is reserved only as the WHOLE stem; it is ordinary inside a
	// longer name, and refusing those too would be a deny-list overreaching.
	for _, name := range []string{"conventions", "decision-2", "console", "nullable", "com10"} {
		if _, err := Write(paths, ScopeProject, name, "d", "b"); err != nil {
			t.Errorf("Write rejected %q: %v", name, err)
		}
	}
}

// A LINK AT THE NOTE'S OWN POSITION IS REFUSED. A note store is a write
// primitive pointed at a path the model chooses, which is the same shape as
// "save my plan" and needs the same answer.
//
// Uses linkDir rather than os.Symlink so it runs on WINDOWS too. os.Symlink
// needs SeCreateSymbolicLinkPrivilege, which an ordinary account lacks, so the
// old version skipped there — leaving the reparse guard with no coverage on the
// one platform where a junction makes the attack reachable without privilege.
// Both Write and Forget are checked, since each has its own RefuseReparse call.
func TestWritingRefusesToFollowALinkAtTheNotePosition(t *testing.T) {
	paths := testPaths(t)
	base := filepath.Dir(paths.ProjectDir)
	if err := os.MkdirAll(paths.ProjectDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// A directory target, because a Windows junction can only point at one.
	target := filepath.Join(base, "precious")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(target, "keepme")
	if err := os.WriteFile(sentinel, []byte("do not clobber"), 0o600); err != nil {
		t.Fatal(err)
	}
	linkDir(t, target, filepath.Join(paths.ProjectDir, "evil"+fileExt))

	if _, err := Write(paths, ScopeProject, "evil", "d", "b"); !errors.Is(err, ErrIsSymlink) {
		t.Fatalf("Write followed a link: %v", err)
	}
	if err := Forget(paths, ScopeProject, "evil"); !errors.Is(err, ErrIsSymlink) {
		t.Fatalf("Forget followed a link: %v", err)
	}
	body, err := os.ReadFile(sentinel)
	if err != nil {
		t.Fatalf("the link target was disturbed: %v", err)
	}
	if string(body) != "do not clobber" {
		t.Fatalf("the link target was overwritten: %q", body)
	}
}

// An unavailable scope is refused with a reason rather than written elsewhere.
func TestAnUnavailableScopeIsRefused(t *testing.T) {
	if _, err := Write(Paths{}, ScopeProject, "x", "d", "b"); !errors.Is(err, ErrNoStore) {
		t.Errorf("a write with no store configured returned %v", err)
	}
	if _, err := Write(testPaths(t), Scope("elsewhere"), "x", "d", "b"); !errors.Is(err, ErrBadScope) {
		t.Errorf("an unknown scope returned %v", err)
	}
}

// A note written by hand, without frontmatter, must still be readable — losing
// it would be the store punishing the reader it exists to serve.
func TestAHandWrittenNoteWithoutFrontmatterStillReads(t *testing.T) {
	paths := testPaths(t)
	if err := os.MkdirAll(paths.ProjectDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(paths.ProjectDir, "manual"+fileExt), []byte("just some prose\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	note, err := Read(paths, ScopeProject, "manual")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(note.Body, "just some prose") {
		t.Errorf("body = %q", note.Body)
	}
}

func TestOversizedNotesAreRefusedAndForgetIsIdempotent(t *testing.T) {
	paths := testPaths(t)
	if _, err := Write(paths, ScopeProject, "big", "d", strings.Repeat("x", maxNoteBytes+1)); !errors.Is(err, ErrTooLarge) {
		t.Errorf("an oversized note was accepted: %v", err)
	}
	if err := Forget(paths, ScopeProject, "never-existed"); err != nil {
		t.Errorf("forgetting a missing note errored: %v", err)
	}
	if _, err := Write(paths, ScopeLocal, "temp", "d", "b"); err != nil {
		t.Fatal(err)
	}
	if err := Forget(paths, ScopeLocal, "temp"); err != nil {
		t.Fatalf("forget: %v", err)
	}
	if _, err := Read(paths, ScopeLocal, "temp"); !errors.Is(err, ErrNotFound) {
		t.Errorf("a forgotten note is still readable: %v", err)
	}
}

// CRLF frontmatter must parse. Project-scope notes are checked in, and Git for
// Windows defaults to autocrlf=true, so a note that merely round-trips through a
// clone comes back with "---\r\n". Under the LF-only split the entire header,
// delimiters included, fell through into the body and the description was lost.
func TestFrontmatterParsesWithCRLFLineEndings(t *testing.T) {
	paths := testPaths(t)
	if err := os.MkdirAll(paths.ProjectDir, 0o700); err != nil {
		t.Fatal(err)
	}
	content := "---\r\ndescription: what a clone gives back\r\n---\r\n\r\nthe body\r\n"
	if err := os.WriteFile(filepath.Join(paths.ProjectDir, "clone-note"+fileExt), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	note, err := Read(paths, ScopeProject, "clone-note")
	if err != nil {
		t.Fatal(err)
	}
	if note.Description != "what a clone gives back" {
		t.Errorf("description = %q, want it parsed from the CRLF frontmatter", note.Description)
	}
	if strings.Contains(note.Body, "description:") || strings.Contains(note.Body, "---") {
		t.Errorf("the frontmatter leaked into the body: %q", note.Body)
	}
}

// A link at the note position must be refused on READ too, not only on write and
// delete. A note read through a link is an exfiltration primitive in a tool the
// model can call by name — the write hole with the arrow reversed.
//
// The link points INSIDE the root on purpose: os.Root already refuses one whose
// target leaves the root, so a target it permits is the case that needs this
// guard, and it is the case that was being served.
func TestReadingRefusesALinkAtTheNotePosition(t *testing.T) {
	paths := testPaths(t)
	if err := os.MkdirAll(paths.ProjectDir, 0o700); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(paths.Root, "secret.txt")
	if err := os.WriteFile(secret, []byte("do not exfiltrate"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A RELATIVE target that resolves back INSIDE the root. An absolute target,
	// or one leaving the root, is refused by os.Root on its own and would let
	// this test pass without the guard it exists to check. "../../secret.txt"
	// from .zero/memory lands on the workspace root, which os.Root permits — and
	// which was therefore served.
	if err := os.Symlink(filepath.Join("..", "..", "secret.txt"), filepath.Join(paths.ProjectDir, "linked"+fileExt)); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	note, err := Read(paths, ScopeProject, "linked")
	if !errors.Is(err, ErrIsSymlink) {
		t.Fatalf("Read(link) = %v (body %q), want ErrIsSymlink", err, note.Body)
	}
	if strings.Contains(note.Body, "do not exfiltrate") {
		t.Error("Read served the link target's contents")
	}
}

// The same read guard, exercised on WINDOWS. The test above proves the actual
// content leak but needs os.Symlink, which an ordinary Windows account cannot
// call, so it skips on the one platform where a junction makes this reachable
// without privilege — the identical gap Vasanth found in the write test, which
// linkDir exists to close. RefuseReparse runs before the open, so a directory
// target still exercises the guard.
func TestReadingRefusesALinkAtTheNotePositionOnEveryPlatform(t *testing.T) {
	paths := testPaths(t)
	if err := os.MkdirAll(paths.ProjectDir, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(paths.Root, "target")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	linkDir(t, target, filepath.Join(paths.ProjectDir, "linked"+fileExt))

	if _, err := Read(paths, ScopeProject, "linked"); !errors.Is(err, ErrIsSymlink) {
		t.Fatalf("Read(link) = %v, want ErrIsSymlink", err)
	}
}

// The size ceiling has to hold on the way OUT as well as in. Project notes are
// checked in, so a note larger than the limit arrives by clone or by hand and
// used to be read whole — and List read every note in the store that way.
func TestReadingRefusesAnOversizedNoteWrittenOutOfBand(t *testing.T) {
	paths := testPaths(t)
	if err := os.MkdirAll(paths.ProjectDir, 0o700); err != nil {
		t.Fatal(err)
	}
	oversized := strings.Repeat("x", maxNoteBytes+1)
	if err := os.WriteFile(filepath.Join(paths.ProjectDir, "huge"+fileExt), []byte(oversized), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(paths, ScopeProject, "huge"); !errors.Is(err, ErrTooLarge) {
		t.Errorf("Read(oversized) = %v, want ErrTooLarge", err)
	}
	// And the listing reports it rather than dropping the note in silence.
	if _, listErr := List(paths, ScopeProject); listErr == nil {
		t.Error("List silently skipped an oversized note instead of reporting it")
	}
}

// A scope is resolved in ONE place. An unknown spelling must be refused rather
// than widened to every store, and a valid one must actually narrow the search.
func TestResolveScopesRefusesUnknownAndHonoursValid(t *testing.T) {
	both, err := ResolveScopes("")
	if err != nil || len(both) != 2 {
		t.Errorf("ResolveScopes(\"\") = %v, %v; want both scopes", both, err)
	}
	for _, requested := range []string{"project", "PROJECT", " local "} {
		got, err := ResolveScopes(requested)
		if err != nil || len(got) != 1 {
			t.Errorf("ResolveScopes(%q) = %v, %v; want exactly one scope", requested, got, err)
		}
	}
	for _, requested := range []string{"invalid", "both", "user", "../project"} {
		if got, err := ResolveScopes(requested); !errors.Is(err, ErrBadScope) {
			t.Errorf("ResolveScopes(%q) = %v, %v; want ErrBadScope rather than a widened search", requested, got, err)
		}
	}
}

// A listing restricted to one scope must not read the other.
func TestListHonoursTheRequestedScope(t *testing.T) {
	paths := testPaths(t)
	if _, err := Write(paths, ScopeProject, "shared", "d", "x"); err != nil {
		t.Fatal(err)
	}
	if _, err := Write(paths, ScopeLocal, "private", "d", "y"); err != nil {
		t.Fatal(err)
	}
	notes, err := List(paths, ScopeProject)
	if err != nil {
		t.Fatal(err)
	}
	if len(notes) != 1 || notes[0].Name != "shared" {
		t.Errorf("List(project) = %+v, want only the project note", notes)
	}
}

// "local" says the note stays on this machine, and the store sits inside the
// working tree, so it has to make itself private wherever it is created rather
// than relying on the workspace having the right .gitignore line.
func TestTheLocalStoreIgnoresItself(t *testing.T) {
	paths := testPaths(t)
	if _, err := Write(paths, ScopeLocal, "private", "d", "y"); err != nil {
		t.Fatal(err)
	}
	ignore, err := os.ReadFile(filepath.Join(paths.LocalDir, ".gitignore"))
	if err != nil {
		t.Fatalf("the local store did not make itself private: %v", err)
	}
	if !strings.Contains(string(ignore), "*") {
		t.Errorf("local .gitignore = %q, want it to ignore everything in the directory", ignore)
	}
	// The project store is checked in on purpose and must NOT be ignored.
	if _, err := os.Stat(filepath.Join(paths.ProjectDir, ".gitignore")); err == nil {
		t.Error("the project store ignored itself; project notes are meant to be shared")
	}
}

// Line endings are normalised on every path, not just the one with frontmatter.
// The two used to differ: a note WITH a header came back LF and one WITHOUT kept
// its CRLF, which no caller asked for and nothing documented.
func TestBodyLineEndingsAreNormalisedWithAndWithoutFrontmatter(t *testing.T) {
	paths := testPaths(t)
	if err := os.MkdirAll(paths.ProjectDir, 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(paths.ProjectDir, name+fileExt), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("headed", "---\r\ndescription: d\r\n---\r\n\r\nline one\r\nline two\r\n")
	write("bare", "line one\r\nline two\r\n")

	for _, name := range []string{"headed", "bare"} {
		note, err := Read(paths, ScopeProject, name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if strings.Contains(note.Body, "\r") {
			t.Errorf("%s: body kept a carriage return: %q", name, note.Body)
		}
	}
}

// The listing prints every description, so the field is shared screen space: one
// note carrying a 60 KiB single-line description consumed the listing everyone
// else has to fit in. Bounded separately from the note's own size limit.
func TestADescriptionCannotConsumeTheListing(t *testing.T) {
	paths := testPaths(t)
	huge := strings.Repeat("verbose ", 5000)
	if _, err := Write(paths, ScopeProject, "noisy", huge, "body"); err != nil {
		t.Fatal(err)
	}
	note, err := Read(paths, ScopeProject, "noisy")
	if err != nil {
		t.Fatal(err)
	}
	if len(note.Description) > maxDescriptionBytes {
		t.Errorf("description is %d bytes, want at most %d", len(note.Description), maxDescriptionBytes)
	}
	if note.Description == "" {
		t.Error("the description was dropped entirely rather than truncated")
	}
	// The body is untouched: only the summary is bounded.
	if !strings.Contains(note.Body, "body") {
		t.Errorf("the body was disturbed: %q", note.Body)
	}
}

// The cap has to be LINEAR. The first version dropped one rune per iteration and
// re-encoded the slice to measure it, taking 13.3s on a 64 KiB description —
// which maxNoteBytes permits, so it was slowest on exactly the input it exists
// to handle.
func TestTheDescriptionCapIsLinear(t *testing.T) {
	for _, size := range []int{1 << 10, 16 << 10, 64 << 10} {
		start := time.Now()
		got := boundedDescription(strings.Repeat("verbose ", size/8))
		elapsed := time.Since(start)
		if len(got) > maxDescriptionBytes {
			t.Errorf("%d bytes in: result is %d bytes, over the %d cap", size, len(got), maxDescriptionBytes)
		}
		if elapsed > time.Second {
			t.Errorf("%d bytes in: took %v — the cap is not linear", size, elapsed)
		}
	}
	// Multi-byte runes must not be split, and must not be slow either.
	multi := strings.Repeat("é", 16<<10)
	start := time.Now()
	got := boundedDescription(multi)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("multi-byte input took %v", elapsed)
	}
	if !utf8.ValidString(got) {
		t.Errorf("the cap split a multi-byte rune: %q", got)
	}
	if len(got) > maxDescriptionBytes {
		t.Errorf("multi-byte result is %d bytes, over the %d cap", len(got), maxDescriptionBytes)
	}
}

// A note that arrives with a CLONE never passed through this process's write
// path, so the bound has to run where the description is parsed. Otherwise a
// checked-in note crowds the listing everyone shares.
func TestACheckedInDescriptionIsBoundedOnRead(t *testing.T) {
	paths := testPaths(t)
	if err := os.MkdirAll(paths.ProjectDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// Written directly to disk, as a clone would deliver it.
	huge := strings.Repeat("noise ", 4000)
	content := "---\nname: cloned\ndescription: " + huge + "\n---\n\nbody\n"
	if err := os.WriteFile(filepath.Join(paths.ProjectDir, "cloned"+fileExt), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	note, err := Read(paths, ScopeProject, "cloned")
	if err != nil {
		t.Fatal(err)
	}
	if len(note.Description) > maxDescriptionBytes {
		t.Errorf("a checked-in description reached the listing at %d bytes, over the %d cap",
			len(note.Description), maxDescriptionBytes)
	}
}
