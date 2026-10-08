package pathjail

import (
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// linkDir points link at target, using whatever kind of link this platform lets
// an unprivileged process create.
//
// On Windows that is a JUNCTION, not a symlink, and the difference is the whole
// reason this package exists twice over. A symlink needs
// SeCreateSymbolicLinkPrivilege, which an ordinary account does not hold, so a
// test written with os.Symlink silently skips on Windows and proves nothing
// there. A junction needs no privilege at all, so it is both the reachable
// attack and the only one that can be tested.
func linkDir(t *testing.T, target, link string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
			t.Skipf("cannot create a junction: %v %s", err, out)
		}
		return
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("cannot create a symlink: %v", err)
	}
}

// The reported defect: a link in an ANCESTOR of the target, not at the target
// itself. Every guard that checked only the final directory and file passed
// this, because the components it checked were genuinely not links; the ones
// the syscall then traversed were.
func TestAnAncestorLinkCannotRedirectAWrite(t *testing.T) {
	base := t.TempDir()
	outside := filepath.Join(base, "outside")
	workspace := filepath.Join(base, "workspace")
	for _, dir := range []string{outside, workspace} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	linkDir(t, outside, filepath.Join(workspace, ".zero"))

	handle, relative, err := Open(workspace, filepath.Join(workspace, ".zero", "store"))
	if err != nil {
		t.Fatalf("Open should confine, not refuse outright: %v", err)
	}
	defer handle.Close()

	if err := handle.MkdirAll(relative, 0o700); err == nil {
		t.Fatal("created a directory through a linked ancestor, so a write can still leave the workspace")
	}
	if _, err := os.Stat(filepath.Join(outside, "store")); !os.IsNotExist(err) {
		t.Errorf("something was created outside the workspace, stat error = %v", err)
	}
}

// RefuseReparse has to catch a junction, which is the case os.ModeSymlink alone
// misses. Asserted through the exported helper rather than by reading the mode,
// so the test fails if the predicate is ever narrowed back.
func TestRefuseReparseCatchesAJunctionNotJustASymlink(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "target")
	inside := filepath.Join(base, "inside")
	for _, dir := range []string{target, inside} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	linkDir(t, target, filepath.Join(inside, "linked"))

	handle, relative, err := Open(inside, inside)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()

	err = RefuseReparse(handle, filepath.Join(relative, "linked"))
	if !errors.Is(err, ErrReparse) {
		t.Fatalf("RefuseReparse(link) = %v, want ErrReparse; on Windows a junction reports ModeIrregular, so a ModeSymlink-only check returns nil here", err)
	}
}

// A name that does not exist yet is what a create is for, so it must not be
// refused. Without this the guard above would be satisfied by refusing
// everything.
func TestRefuseReparseAllowsAnAbsentName(t *testing.T) {
	base := t.TempDir()
	handle, relative, err := Open(base, base)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()
	if err := RefuseReparse(handle, filepath.Join(relative, "not-there.md")); err != nil {
		t.Errorf("an absent name was refused: %v", err)
	}
	if err := RefuseReparse(handle, relative); err != nil {
		t.Errorf("an ordinary directory was refused: %v", err)
	}
}

// Open refuses a target outside its root before opening anything, so a caller
// cannot act on the path first.
func TestOpenRefusesATargetOutsideTheRoot(t *testing.T) {
	base := t.TempDir()
	inside := filepath.Join(base, "inside")
	if err := os.MkdirAll(inside, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{
		filepath.Join(base, "sibling"),
		filepath.Join(inside, "..", "..", "escape"),
	} {
		if _, _, err := Open(inside, target); !errors.Is(err, ErrEscapes) {
			t.Errorf("Open(%q, %q) = %v, want ErrEscapes", inside, target, err)
		}
	}
	if _, _, err := Open("", filepath.Join(base, "anything")); !errors.Is(err, ErrEscapes) {
		t.Error("an empty root was accepted; a boundary is not optional")
	}
}

// CreateTemp must use O_EXCL and an unpredictable name, so a link planted at a
// guessable temp path is refused by the kernel rather than followed.
func TestCreateTempIsExclusiveAndUnpredictable(t *testing.T) {
	base := t.TempDir()
	handle, relative, err := Open(base, base)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()

	seen := map[string]bool{}
	for i := 0; i < 8; i++ {
		file, name, err := CreateTemp(handle, relative, "note", ".tmp")
		if err != nil {
			t.Fatal(err)
		}
		file.Close()
		if seen[name] {
			t.Fatalf("CreateTemp reused the name %q, so two concurrent writes would stomp each other", name)
		}
		seen[name] = true
		if filepath.Ext(name) != ".tmp" {
			t.Errorf("temp name %q lost its suffix, so a listing may pick it up as real content", name)
		}
	}
}

// A legitimate directory whose name carries a space must be confined as spelled.
// Trimming it would open a different filesystem object.
//
// A LEADING space, deliberately: Win32 normalizes a component's trailing spaces
// and dots away, so "root " and "root" are one object there and a trailing-space
// case cannot express the bug on Windows. A leading space survives on every
// platform, so this runs everywhere rather than being gated to unix.
// The path is RELATIVE, under t.Chdir, so the leading space sits at the start of
// the whole string. An absolute filepath.Join(base, " root") would put the space
// in the middle, where TrimSpace cannot reach it — the test would then pass
// against the very guard it exists to catch, on every platform.
func TestOpenPreservesSpaceInPaths(t *testing.T) {
	base := t.TempDir()
	t.Chdir(base)
	const spaced = " root" // the leading space is part of the name

	handle, relative, err := Open(spaced, spaced)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()

	// Write through the handle, then confirm the bytes landed under the spelling
	// the caller asked for — the property that actually matters, and one
	// handle.Name() alone would not prove.
	file, _, err := CreateTemp(handle, relative, "probe", ".tmp")
	if err != nil {
		t.Fatal(err)
	}
	name := file.Name()
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(base, spaced, filepath.Base(name))); err != nil {
		t.Errorf("a file created through the handle is not under %q: %v — the leading space was trimmed away", spaced, err)
	}
	if _, err := os.Stat(filepath.Join(base, "root")); err == nil {
		t.Error(`a trimmed "root" was created instead of the requested " root"`)
	}
}

// A whitespace-only name is a legal directory on unix, so it must be confined as
// given rather than treated as "no root" or silently swapped for the root.
func TestOpenAcceptsAWhitespaceOnlyComponent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Win32 normalizes a whitespace-only component away; the name is not expressible there")
	}
	// The path must be whitespace-only IN FULL for this to bite, so it has to be
	// relative: an absolute path ending in " " still has a non-empty TrimSpace and
	// would sail past the old guard, making the test prove nothing.
	t.Chdir(t.TempDir())
	handle, _, err := Open(" ", " ")
	if err != nil {
		t.Fatalf("a whitespace-only path was rejected as missing: %v", err)
	}
	defer handle.Close()
	if _, err := os.Stat(" "); err != nil {
		t.Errorf("the whitespace-only directory was not created as spelled: %v", err)
	}
}

// prefix and suffix are name fragments. A separator or traversal in either must
// be refused, or the file lands outside dir and (for a suffix erasing the random
// component) loses the O_EXCL unpredictability it depends on.
func TestCreateTempRejectsPathFragmentsThatEscapeTheDir(t *testing.T) {
	base := t.TempDir()
	inside := filepath.Join(base, "inside")
	if err := os.MkdirAll(inside, 0o700); err != nil {
		t.Fatal(err)
	}
	// root is base, dir is the subdirectory: a "../escaped" fragment lands in
	// base — still inside the os.Root, so os.Root does NOT catch it — but outside
	// the dir CreateTemp promised. This is the case the reviewers reproduced.
	handle, relative, err := Open(base, inside)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()
	cases := []struct{ prefix, suffix string }{
		{"../escaped", ".tmp"},
		{`..\sibling`, ".tmp"},
		{"note", ".tmp/../evil"},
		{"note", ".."},
		{"..", ".tmp"},
	}
	for _, c := range cases {
		if _, _, err := CreateTemp(handle, relative, c.prefix, c.suffix); !errors.Is(err, ErrEscapes) {
			t.Errorf("CreateTemp(prefix=%q, suffix=%q) = %v, want ErrEscapes", c.prefix, c.suffix, err)
		}
	}
	// Nothing may have leaked into base above the promised dir.
	entries, _ := os.ReadDir(base)
	for _, entry := range entries {
		if entry.Name() != "inside" {
			t.Errorf("a temp fragment escaped into the parent: %q", entry.Name())
		}
	}
}

// A junction named with a trailing separator must still be refused. On Windows
// os.Root.Lstat resolves the terminal component when the name ends in a
// separator, so without trimming it the link is stat'd as its target and waved
// through.
func TestRefuseReparseCatchesALinkNamedWithATrailingSeparator(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "target")
	inside := filepath.Join(base, "inside")
	for _, dir := range []string{target, inside} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	linkDir(t, target, filepath.Join(inside, "linked"))
	handle, relative, err := Open(inside, inside)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()
	name := filepath.Join(relative, "linked") + string(filepath.Separator)
	if err := RefuseReparse(handle, name); !errors.Is(err, ErrReparse) {
		t.Fatalf("RefuseReparse(%q) = %v, want ErrReparse", name, err)
	}
}

// The O_EXCL retry is the branch that protects an existing file, and a real
// random source will not collide on demand — so force it. The first two draws
// return the same bytes: the name is pre-created, CreateTemp must refuse to
// reuse it, leave its contents alone, and come back with a different name.
func TestCreateTempRetriesWithoutClobberingAnExistingName(t *testing.T) {
	base := t.TempDir()
	handle, relative, err := Open(base, base)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()

	draws := [][]byte{
		{1, 1, 1, 1, 1, 1, 1, 1}, // collides with the pre-created file
		{1, 1, 1, 1, 1, 1, 1, 1}, // and again, so the retry must keep trying
		{2, 2, 2, 2, 2, 2, 2, 2}, // finally a free name
	}
	previous := randomBytes
	t.Cleanup(func() { randomBytes = previous })
	call := 0
	randomBytes = func(b []byte) (int, error) {
		copy(b, draws[min(call, len(draws)-1)])
		call++
		return len(b), nil
	}

	taken := filepath.Join(base, "note."+hex.EncodeToString(draws[0])+".tmp")
	if err := os.WriteFile(taken, []byte("do not clobber me"), 0o600); err != nil {
		t.Fatal(err)
	}

	file, name, err := CreateTemp(handle, relative, "note", ".tmp")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	if filepath.Base(name) == filepath.Base(taken) {
		t.Fatalf("CreateTemp returned the name that was already taken (%q), so O_EXCL did not protect it", name)
	}
	kept, err := os.ReadFile(taken)
	if err != nil {
		t.Fatal(err)
	}
	if string(kept) != "do not clobber me" {
		t.Errorf("the pre-existing file was overwritten: %q", kept)
	}
	if call < 2 {
		t.Errorf("the random source was drawn %d time(s); the collision path never ran", call)
	}
}

// On unix a backslash is an ordinary filename character, not a separator, so a
// link genuinely named `linked\` must still be refused. Trimming backslashes
// unconditionally would inspect "linked" instead and report "not a link" for one
// that is.
func TestRefuseReparseKeepsABackslashInAUnixName(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip(`backslash is a separator on Windows, so this name is not expressible there`)
	}
	base := t.TempDir()
	target := filepath.Join(base, "target")
	inside := filepath.Join(base, "inside")
	for _, dir := range []string{target, inside} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	linkDir(t, target, filepath.Join(inside, `linked\`))

	handle, relative, err := Open(inside, inside)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()

	name := filepath.Join(relative, `linked\`)
	if err := RefuseReparse(handle, name); !errors.Is(err, ErrReparse) {
		t.Fatalf("RefuseReparse(%q) = %v, want ErrReparse — the backslash is part of the name here, not a separator", name, err)
	}
}
