package memory

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// linkDir uses a junction on Windows: it needs no privilege, unlike a symlink,
// so it is both the reachable attack and the only one testable on an ordinary
// Windows account.
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

// The store used to check only its own directory and file, so a link at the
// ANCESTOR .zero turned every operation into one aimed outside the workspace:
// Write and Forget were arbitrary write and delete, and Read was an arbitrary
// read in a tool the model can call by name.
//
// All three are asserted, because fixing one and leaving the others is exactly
// what the original guard did.
func TestAnAncestorLinkCannotTakeTheStoreOutOfTheWorkspace(t *testing.T) {
	base := t.TempDir()
	outside := filepath.Join(base, "outside")
	workspace := filepath.Join(base, "workspace")
	for _, dir := range []string{outside, workspace} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// A note already sitting in the external directory, so a successful read
	// would be visible rather than merely "no error".
	if err := os.MkdirAll(filepath.Join(outside, "memory"), 0o700); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(outside, "memory", "secret.md")
	if err := os.WriteFile(secret, []byte("do not read me"), 0o600); err != nil {
		t.Fatal(err)
	}
	linkDir(t, outside, filepath.Join(workspace, ".zero"))

	paths := DefaultPaths(workspace)

	if _, err := Write(paths, ScopeProject, "escaped", "d", "b"); err == nil {
		t.Error("Write went through the linked ancestor")
	}
	if _, err := os.Stat(filepath.Join(outside, "memory", "escaped.md")); !os.IsNotExist(err) {
		t.Errorf("a note was written outside the workspace, stat error = %v", err)
	}
	if _, err := Read(paths, ScopeProject, "secret"); err == nil {
		t.Error("Read returned a note from outside the workspace")
	}
	if err := Forget(paths, ScopeProject, "secret"); err == nil {
		t.Error("Forget accepted a target outside the workspace")
	}
	if _, err := os.Stat(secret); err != nil {
		t.Errorf("Forget deleted a file outside the workspace: %v", err)
	}
	notes, listErr := List(paths)
	if len(notes) != 0 {
		t.Errorf("List surfaced %d note(s) from outside the workspace", len(notes))
	}
	// The refusal is REPORTED, not swallowed. A store that cannot be opened
	// because a link redirects it out of the workspace is an operational failure;
	// returning an empty list for it would tell the caller there are no notes,
	// which is how a redirected store looks exactly like an empty one.
	if listErr == nil {
		t.Error("List reported no problem for a store redirected outside the workspace")
	}
}

// The ordinary path still works, or the test above would pass against a store
// that refused everything.
func TestAnOrdinaryWorkspaceStoreStillRoundTrips(t *testing.T) {
	workspace := t.TempDir()
	paths := DefaultPaths(workspace)
	if _, err := Write(paths, ScopeProject, "note", "a summary", "the body"); err != nil {
		t.Fatalf("Write: %v", err)
	}
	note, err := Read(paths, ScopeProject, "note")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if note.Description != "a summary" || strings.TrimSpace(note.Body) != "the body" {
		t.Errorf("round trip lost content: %+v", note)
	}
	notes, listErr := List(paths)
	if listErr != nil {
		t.Fatal(listErr)
	}
	if len(notes) != 1 {
		t.Errorf("List returned %d notes, want 1", len(notes))
	}
	if err := Forget(paths, ScopeProject, "note"); err != nil {
		t.Fatalf("Forget: %v", err)
	}
	if _, err := Read(paths, ScopeProject, "note"); err == nil {
		t.Error("the note survived Forget")
	}
}

// A LINK ABOVE THE STORE REDIRECTS EVERYTHING BELOW IT.
//
// os.Root refuses to traverse OUT of the workspace, and the note file itself was
// checked — but a link that resolves back INSIDE the workspace is followed, and
// the store path is checked in. A repository shipping ".zero -> redirected"
// served notes from a directory nobody asked for, with every individual check
// passing, because the only component inspected was the note at the end.
//
// The components are derived from Paths rather than spelled out, so this keeps
// testing every ancestor if the layout moves.
func TestALinkAboveTheStoreIsRefused(t *testing.T) {
	layout := DefaultPaths(string(filepath.Separator) + "workspace")
	for _, store := range []struct {
		scope Scope
		dir   string
	}{{ScopeProject, layout.ProjectDir}, {ScopeLocal, layout.LocalDir}} {
		relative, err := filepath.Rel(layout.Root, store.dir)
		if err != nil {
			t.Fatal(err)
		}
		parts := strings.Split(relative, string(filepath.Separator))
		for i := range parts {
			ancestor := filepath.Join(parts[:i+1]...)
			t.Run(string(store.scope)+"/"+ancestor, func(t *testing.T) {
				root := t.TempDir()
				paths := DefaultPaths(root)
				// The target carries a complete store rooted where the link
				// lands, so the ONLY thing between the caller and the planted
				// note is the ancestor check.
				target := filepath.Join(root, "redirected")
				planted := filepath.Join(target, filepath.Join(parts[i+1:]...))
				if err := os.MkdirAll(planted, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(planted, "secret"+fileExt),
					[]byte("---\nname: secret\ndescription: planted\n---\n\nplanted body\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				linkPath := filepath.Join(root, ancestor)
				if err := os.MkdirAll(filepath.Dir(linkPath), 0o700); err != nil {
					t.Fatal(err)
				}
				// linkDir, not os.Symlink: on Windows this has to be a JUNCTION,
				// which is a reparse point but not a symlink — the exact case
				// RefuseReparse tests ModeIrregular for. Calling os.Symlink
				// directly skipped the whole test there, so the guard's Windows
				// behaviour went unasserted while the run reported green.
				linkDir(t, target, linkPath)
				if note, err := Read(paths, store.scope, "secret"); err == nil {
					t.Fatalf("a link at %s redirected the read: got %q", ancestor, note.Body)
				} else if !errors.Is(err, ErrIsSymlink) {
					t.Errorf("read through a link at %s failed with %v, want ErrIsSymlink", ancestor, err)
				}
				// Writes and deletes go through the same door.
				if _, err := Write(paths, store.scope, "secret", "d", "body"); !errors.Is(err, ErrIsSymlink) {
					t.Errorf("write through a link at %s = %v, want ErrIsSymlink", ancestor, err)
				}
				if err := Forget(paths, store.scope, "secret"); !errors.Is(err, ErrIsSymlink) {
					t.Errorf("forget through a link at %s = %v, want ErrIsSymlink", ancestor, err)
				}
			})
		}
	}
}

// THE PRIVACY PROMISE FAILS CLOSED.
//
// O_EXCL cannot tell "a previous run wrote the ignore" from "something else got
// there first", and the old code read every failure as the former. Precreating
// an empty .gitignore made every local write succeed with the store fully
// tracked — the note the user was told stays on this machine, sitting in git
// status.
func TestALocalWriteRefusesAnIneffectiveIgnore(t *testing.T) {
	for name, content := range map[string]string{
		"empty":                       "",
		"comments only":               "# nothing here\n\n",
		"narrower than all":           "*.md\n",
		"cancelled by a re-inclusion": "*\n!keep.md\n",
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			paths := DefaultPaths(root)
			if err := os.MkdirAll(paths.LocalDir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(paths.LocalDir, ".gitignore"), []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Write(paths, ScopeLocal, "private", "d", "secret body"); !errors.Is(err, ErrNotPrivate) {
				t.Errorf("Write with a %s ignore = %v, want ErrNotPrivate — the note would be tracked", name, err)
			}
		})
	}

	// An ignore that DOES cover everything is accepted, including one this store
	// did not write itself.
	for name, content := range map[string]string{
		"exactly what we write": "# Notes saved to the local scope stay on this machine.\n*\n",
		"bare star":             "*\n",
		"star with a comment":   "# mine\n*\n",
	} {
		t.Run("accepted/"+name, func(t *testing.T) {
			root := t.TempDir()
			paths := DefaultPaths(root)
			if err := os.MkdirAll(paths.LocalDir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(paths.LocalDir, ".gitignore"), []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Write(paths, ScopeLocal, "private", "d", "secret body"); err != nil {
				t.Errorf("Write with a %s ignore = %v, want success", name, err)
			}
		})
	}

	// And the first write into a clean store still installs one.
	clean := t.TempDir()
	if _, err := Write(DefaultPaths(clean), ScopeLocal, "private", "d", "body"); err != nil {
		t.Fatal(err)
	}
	written, err := os.ReadFile(filepath.Join(DefaultPaths(clean).LocalDir, ".gitignore"))
	if err != nil {
		t.Fatal(err)
	}
	if !ignoresEverything(string(written)) {
		t.Errorf("the ignore this store installs does not cover everything: %q", written)
	}
}

// GIT DOES NOT READ A SYMLINKED .gitignore, SO NEITHER DOES THE GATE.
//
// keepLocalScopePrivate saw O_EXCL return EEXIST and then read through the link,
// accepting the target's "*" as proof of a rule git warns about and does not
// apply. An in-workspace RELATIVE link is followed by os.Root — an absolute one
// is refused, which is why an earlier check of exactly this looked clean and was
// not.
func TestALinkedIgnoreIsNotProofOfPrivacy(t *testing.T) {
	// A FILE symlink, which is what a .gitignore would actually be. Windows needs
	// a privilege for these, so the junction case below carries that platform.
	t.Run("file symlink", func(t *testing.T) {
		root := t.TempDir()
		paths := DefaultPaths(root)
		if err := os.MkdirAll(paths.LocalDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(paths.LocalDir, "decoy"), []byte("*\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("decoy", filepath.Join(paths.LocalDir, ".gitignore")); err != nil {
			t.Skipf("cannot create a file symlink here: %v", err)
		}
		if _, err := Write(paths, ScopeLocal, "private", "d", "secret"); !errors.Is(err, ErrNotPrivate) {
			t.Errorf("a symlinked ignore was accepted as privacy: %v", err)
		}
	})

	// A DIRECTORY reparse point at the ignore path, which linkDir builds as a
	// junction on Windows and a symlink elsewhere. The refusal arrives
	// differently by platform — ErrNotPrivate where the link is inspected as a
	// link, "is a directory" where the create simply cannot open it — so what is
	// asserted is the property that matters on both: the write DOES NOT SUCCEED.
	// Asserting the sentinel here is what turned this test red on Windows while
	// the behaviour was correct.
	t.Run("directory reparse point", func(t *testing.T) {
		root := t.TempDir()
		paths := DefaultPaths(root)
		if err := os.MkdirAll(filepath.Join(paths.LocalDir, "decoy"), 0o700); err != nil {
			t.Fatal(err)
		}
		linkDir(t, "decoy", filepath.Join(paths.LocalDir, ".gitignore"))
		if _, err := Write(paths, ScopeLocal, "private", "d", "secret"); err == nil {
			t.Error("a reparse point at the ignore path was accepted as privacy")
		}
	})
}

// THE GATE FOLLOWS GIT'S WHITESPACE RULES, and trimming each line had both
// backwards. Every expectation here was measured against git 2.55.0, not read
// off the spec.
func TestTheIgnoreGateAgreesWithGit(t *testing.T) {
	for name, tc := range map[string]struct {
		content string
		covered bool
	}{
		// LEADING whitespace is part of the pattern, so this ignores nothing —
		// and the gate said it did, which let a note the user was told stays on
		// this machine be picked up by a routine `git add -A`.
		"leading spaces": {"  *\n", false},
		"leading tab":    {"\t*\n", false},
		// A BOM is stripped by git, so this rule works and must be accepted.
		"utf-8 bom": {"\xef\xbb\xbf*\n", true},
		// TRAILING whitespace is not significant to git.
		"trailing spaces":   {"*   \n", true},
		"plain":             {"*\n", true},
		"comment then star": {"# mine\n*\n", true},
		// CRLF, because a .gitignore written on Windows carries them and git does
		// not care. The gate strips the carriage return before comparing; if that
		// ever stops, every local write on a Windows checkout fails for a file
		// that was doing its job.
		"crlf":              {"*\r\n", true},
		"crlf with comment": {"# mine\r\n*\r\n", true},
		"cancelled":         {"*\n!keep.md\n", false},
	} {
		if got := ignoresEverything(tc.content); got != tc.covered {
			t.Errorf("%s: ignoresEverything(%q) = %v, want %v (git's answer)", name, tc.content, got, tc.covered)
		}
	}
}

// ABSENT TO THE HANDLE IS NOT ABSENT ON DISK.
//
// A component the confined handle will not open, while it is present on disk,
// has been REFUSED — and a Windows junction whose target leaves the workspace
// reports exactly that way. The chain read it and everything under it as
// missing, the later open became ErrNotFound, and List drops ErrNotFound, so a
// tampered store presented as an empty one. presentOnDisk is what tells the two
// answers apart, so its own behaviour is pinned here.
func TestPresentOnDiskDistinguishesAbsenceFromRefusal(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".zero", "memory"), 0o700); err != nil {
		t.Fatal(err)
	}
	if !presentOnDisk(root, ".zero") {
		t.Error("an existing directory was reported absent")
	}
	if !presentOnDisk(root, filepath.Join(".zero", "memory")) {
		t.Error("an existing nested directory was reported absent")
	}
	if presentOnDisk(root, "never-created") {
		t.Error("a missing path was reported present")
	}
	if presentOnDisk("", ".zero") {
		t.Error("a blank root was treated as containing something")
	}
	// A LINK COUNTS AS PRESENT, without being followed — that is the whole case
	// this exists for, since the junction's target is outside the workspace.
	linkDir(t, filepath.Join(root, ".zero"), filepath.Join(root, "aliased"))
	if !presentOnDisk(root, "aliased") {
		t.Error("a reparse point was reported absent, so a refusal would read as absence")
	}
	// And a DANGLING link is still present: Lstat does not follow it.
	if err := os.Symlink("nowhere-at-all", filepath.Join(root, "dangling")); err == nil {
		if !presentOnDisk(root, "dangling") {
			t.Error("a dangling link was reported absent")
		}
	}
}

// A clean workspace must keep working: absence really is absence there, and
// treating it as a refusal would break every first write.
func TestAnUncreatedStoreIsStillAbsence(t *testing.T) {
	fresh := t.TempDir()
	paths := DefaultPaths(fresh)
	if _, err := Read(paths, ScopeProject, "nothing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("a note in an uncreated store = %v, want ErrNotFound", err)
	}
	notes, err := List(paths, ScopeProject)
	if err != nil || len(notes) != 0 {
		t.Errorf("listing an uncreated store = %+v, %v; want empty and no error", notes, err)
	}
	if _, err := Write(paths, ScopeLocal, "first", "d", "b"); err != nil {
		t.Errorf("the first write into a clean workspace failed: %v", err)
	}
}

// REFUSED IS NOT ABSENT, AND THE TEST FOR IT RUNS HERE.
//
// The first attempt at this keyed on the component being ABSENT to the handle
// and only then compared against disk. A junction is not absent to it —
// handle.Lstat does not traverse a reparse point, so it reports the junction as
// being right there, the branch never ran, and the walk continued past the
// component it was meant to refuse. The later open failed as ErrNotFound and
// List dropped it, which is how a tampered store presented as an empty one.
//
// The question that separates the two is what the handle can OPEN, because
// opening is what traverses. A directory that is present but cannot be entered
// produces exactly that disagreement — Lstat sees it, Open refuses it — which is
// the same shape as the junction and, unlike the junction, constructible here.
func TestAPresentButUnopenableStoreIsReportedNotHidden(t *testing.T) {
	root := t.TempDir()
	paths := DefaultPaths(root)
	if err := os.MkdirAll(paths.ProjectDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Write(paths, ScopeProject, "note", "d", "body"); err != nil {
		t.Fatal(err)
	}
	// NOT CONSTRUCTIBLE EVERYWHERE, and the platforms invert. On Windows
	// os.Chmod only toggles the read-only attribute, so this arm cannot build the
	// refusal there — which is the opposite of what an earlier version of this
	// comment claimed. The companion below covers Windows with a junction, which
	// IS constructible there and is not here.
	blocked := filepath.Join(root, ".zero")
	if err := os.Chmod(blocked, 0o000); err != nil {
		t.Skipf("cannot remove access here: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(blocked, 0o700) })
	// CLOSE IT. Discarding the handle leaked an open descriptor, and on Windows
	// an open handle blocks the directory's removal — so the skip below left
	// t.TempDir's cleanup failing and the run red on the very platform this test
	// is trying to stand in for.
	if probe, err := os.Open(filepath.Join(blocked, "memory")); err == nil {
		probe.Close()
		t.Skip("this environment ignores directory permissions, so the refusal cannot be built")
	}

	_, readErr := Read(paths, ScopeProject, "note")
	if errors.Is(readErr, ErrNotFound) {
		t.Errorf("an unreadable store was reported as a missing note, which is what makes the model overwrite it: %v", readErr)
	}
	if !errors.Is(readErr, ErrUnreadable) {
		t.Errorf("Read of an unreadable store = %v, want ErrUnreadable", readErr)
	}
	if _, listErr := List(paths, ScopeProject); listErr == nil {
		t.Error("List presented an unreadable store as an empty one")
	}
}

// SAVE A NOTE, READ IT STRAIGHT BACK, GET YOUR OWN.
//
// memory_write and memory_forget default to local while an unscoped read
// resolved project first, and scope is optional in every schema. A model that
// omitted it was handed content it never wrote, and told a note was deleted
// after which the name still read. A checked-in project note is the ordinary way
// that happens — it arrives with a clone, and the model has no reason to expect
// it.
func TestAnUnscopedRoundTripReturnsWhatWasWritten(t *testing.T) {
	root := t.TempDir()
	paths := DefaultPaths(root)
	if err := os.MkdirAll(paths.ProjectDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// The checked-in note that used to win.
	if err := os.WriteFile(filepath.Join(paths.ProjectDir, "findings"+fileExt),
		[]byte("---\nname: findings\ndescription: theirs\n---\n\nPROJECT-CONTENT\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Write(paths, ScopeLocal, "findings", "mine", "MY-OWN-CONTENT"); err != nil {
		t.Fatal(err)
	}

	scopes, err := ResolveScopes("")
	if err != nil {
		t.Fatal(err)
	}
	if len(scopes) == 0 || scopes[0] != ScopeLocal {
		t.Fatalf("the unscoped order is %v; a write lands in local, so a read must look there first", scopes)
	}
	var got Note
	for _, scope := range scopes {
		if note, err := Read(paths, scope, "findings"); err == nil {
			got = note
			break
		}
	}
	if !strings.Contains(got.Body, "MY-OWN-CONTENT") {
		t.Errorf("an unscoped read returned %q, not what the unscoped write saved", got.Body)
	}

	// A project note is still reachable when nothing local shadows it — the order
	// decides which wins, not what exists.
	if err := Forget(paths, ScopeLocal, "findings"); err != nil {
		t.Fatal(err)
	}
	for _, scope := range scopes {
		if note, err := Read(paths, scope, "findings"); err == nil {
			got = note
			break
		}
	}
	if !strings.Contains(got.Body, "PROJECT-CONTENT") {
		t.Errorf("after forgetting the local note the project one should be found; got %q", got.Body)
	}
}

// A NOTE THE LISTING DENIES EXISTS MUST NOT BE DESTROYED BY THE NEXT WRITE.
//
// List matches the extension exactly; Read reopened name+".md", and on a
// case-insensitive filesystem that is the same file as a hand-authored
// "findings.MD". So the listing reported an empty store, writing was the
// reasonable next move, and a checked-in file was silently overwritten.
//
// Read now agrees with List, and the write refuses rather than taking the
// occupant's place.
func TestADifferentlySpelledNoteIsNotSilentlyOverwritten(t *testing.T) {
	root := t.TempDir()
	paths := DefaultPaths(root)
	if err := os.MkdirAll(paths.ProjectDir, 0o700); err != nil {
		t.Fatal(err)
	}
	shouty := filepath.Join(paths.ProjectDir, "findings.MD")
	original := "---\nname: findings\ndescription: d\n---\n\nHAND-WRITTEN\n"
	if err := os.WriteFile(shouty, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	probe := filepath.Join(root, ".CaseProbe")
	if err := os.WriteFile(probe, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	_, insensitiveErr := os.Stat(filepath.Join(root, ".caseprobe"))
	os.Remove(probe)
	if insensitiveErr != nil {
		t.Skip("this filesystem is case-sensitive, so the two names are genuinely different files")
	}

	// Read agrees with the listing: neither offers it.
	notes, err := List(paths, ScopeProject)
	if err != nil || len(notes) != 0 {
		t.Fatalf("List = %+v, %v; the exact-extension rule should not show findings.MD", notes, err)
	}
	if _, err := Read(paths, ScopeProject, "findings"); err == nil {
		t.Error("Read handed back a note the listing denies exists")
	}

	// And the write refuses rather than destroying it.
	if _, err := Write(paths, ScopeProject, "findings", "d", "OVERWRITTEN"); !errors.Is(err, ErrNameClash) {
		t.Errorf("Write = %v, want ErrNameClash rather than taking the occupant's place", err)
	}
	after, err := os.ReadFile(shouty)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != original {
		t.Errorf("the hand-authored note was modified: %q", string(after))
	}
}

// THE COMPANION FOR THE PLATFORM THE OTHER ARM CANNOT REACH.
//
// TestAPresentButUnopenableStoreIsReportedNotHidden builds its refusal with
// chmod, which Windows does not honour — os.Chmod there only toggles the
// read-only attribute. So that arm skips on Windows and the branch would have no
// coverage on the one platform where reparse points are ordinary.
//
// A junction is the reverse: constructible on Windows, not here. Between the two
// arms the refusal is exercised everywhere, and neither passes vacuously —
// each skips loudly where its own mechanism is unavailable.
func TestAStoreBehindAReparsePointIsRefusedOnEveryPlatform(t *testing.T) {
	root := t.TempDir()
	paths := DefaultPaths(root)
	target := filepath.Join(root, "elsewhere")
	if err := os.MkdirAll(filepath.Join(target, "memory"), 0o700); err != nil {
		t.Fatal(err)
	}
	linkDir(t, target, filepath.Join(root, ".zero"))

	if _, err := Read(paths, ScopeProject, "anything"); errors.Is(err, ErrNotFound) {
		t.Error("a store behind a reparse point read as a missing note, which is what makes a model overwrite it")
	} else if err == nil {
		t.Error("a store behind a reparse point was read through")
	}
	if _, err := List(paths, ScopeProject); err == nil {
		t.Error("List presented a store behind a reparse point as an empty one")
	}
}

// runStoreGit runs one git command in dir and fails the test if it does not
// succeed. Identity comes from the environment rather than repository config so
// nothing here depends on a git new enough for GIT_CONFIG_GLOBAL.
func runStoreGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// seedStoreRepo makes root a repository with one commit, so HEAD exists and the
// index is a real one rather than the empty index of a bare `git init`.
func seedStoreRepo(t *testing.T, root string) {
	t.Helper()
	runStoreGit(t, root, "init")
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("readme\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runStoreGit(t, root, "add", "README.md")
	runStoreGit(t, root, "commit", "-m", "seed")
}

// writeStoreFile creates one file under the local store, making the store
// directory first.
func writeStoreFile(t *testing.T, paths Paths, name, content string) string {
	t.Helper()
	if err := os.MkdirAll(paths.LocalDir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(paths.LocalDir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// AN IGNORED PATH IS NOT NECESSARILY A PRIVATE ONE.
//
// git applies no ignore rule to a path already in the INDEX, so "*" in
// local/.gitignore proves privacy only for an untracked path. A clone, an
// earlier `git add -f`, or a hand-authored repository restores both the ignore
// and the note as tracked files; the gate read the ignore, agreed the store was
// private, and the atomic rename replaced a TRACKED file with the body the user
// was told stays on this machine. `git status` showed it as a modification ready
// to commit. Reported by @jatmn.
func TestALocalWriteIsRefusedWhenGitTracksTheNote(t *testing.T) {
	root := t.TempDir()
	paths := DefaultPaths(root)
	const committedBody = "---\nname: private\n---\n\ncommitted body\n"
	notePath := writeStoreFile(t, paths, "private.md", committedBody)
	writeStoreFile(t, paths, gitignoreName, localIgnoreContent)

	seedStoreRepo(t, root)
	// -f, because the ignore this repository is committing covers both files —
	// which is the whole point: the rule is in force AND the paths are tracked.
	runStoreGit(t, root, "add", "-f", ".zero/memory/local/.gitignore", ".zero/memory/local/private.md")
	runStoreGit(t, root, "commit", "-m", "track the local store")

	if _, err := Write(paths, ScopeLocal, "private", "d", "machine-local secret"); !errors.Is(err, ErrNotPrivate) {
		t.Fatalf("Write into a tracked local store = %v, want ErrNotPrivate", err)
	} else if !strings.Contains(err.Error(), "private.md") {
		t.Errorf("refusal %q does not name the tracked note", err)
	}

	// The exact committed bytes, not merely "unchanged length": the failure being
	// pinned is a body swap of the same shape.
	body, readErr := os.ReadFile(notePath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(body) != committedBody {
		t.Errorf("tracked note body = %q, want the committed %q", body, committedBody)
	}
	if status := runStoreGit(t, root, "status", "--porcelain"); status != "" {
		t.Errorf("git status = %q, want empty — the refused write left an index-visible change", status)
	}
}

// THE SAME REFUSAL ON THE FIRST-WRITE BRANCH.
//
// O_EXCL is what decides whether the gate is installing the ignore or checking
// one, and only the checking branch used to look at anything. A repository that
// tracks the note without the ignore therefore walked straight through: the gate
// created a fresh ignore, reported a clean first write, and overwrote the tracked
// note anyway. Deleting one file from the worktree is all that separates this
// from the case above, so gating one branch and not the other fixes nothing.
func TestALocalWriteIsRefusedWhenTrackedWithNoIgnorePresent(t *testing.T) {
	root := t.TempDir()
	paths := DefaultPaths(root)
	const committedBody = "---\nname: private\n---\n\ncommitted body\n"
	notePath := writeStoreFile(t, paths, "private.md", committedBody)

	seedStoreRepo(t, root)
	runStoreGit(t, root, "add", ".zero/memory/local/private.md")
	runStoreGit(t, root, "commit", "-m", "track the local note")

	if _, err := Write(paths, ScopeLocal, "private", "d", "machine-local secret"); !errors.Is(err, ErrNotPrivate) {
		t.Fatalf("Write into a tracked store with no ignore = %v, want ErrNotPrivate", err)
	}
	body, readErr := os.ReadFile(notePath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(body) != committedBody {
		t.Errorf("tracked note body = %q, want the committed %q", body, committedBody)
	}
	if status := runStoreGit(t, root, "status", "--porcelain"); status != "" {
		t.Errorf("git status = %q, want empty", status)
	}
}

// THE ORDINARY CASE KEEPS WORKING, which is the constraint the refusal above has
// to live inside: a repository whose local store is untracked is the normal
// state, and a note written there must still land and still be invisible to git.
func TestALocalWriteInsideAnUntrackedStoreStillLands(t *testing.T) {
	root := t.TempDir()
	paths := DefaultPaths(root)
	seedStoreRepo(t, root)

	if _, err := Write(paths, ScopeLocal, "private", "d", "machine-local secret"); err != nil {
		t.Fatalf("Write into an untracked store inside a repository = %v, want success", err)
	}
	note, err := Read(paths, ScopeLocal, "private")
	if err != nil {
		t.Fatal(err)
	}
	if note.Body != "machine-local secret\n" {
		t.Errorf("note body = %q, want %q", note.Body, "machine-local secret\n")
	}
	if status := runStoreGit(t, root, "status", "--porcelain"); status != "" {
		t.Errorf("git status = %q, want empty — the note must not be visible to git", status)
	}
}

// A CHECKED-IN IGNORE IS NOT A LEAK. Committing local/.gitignore hands every
// clone the rule before its first write, and the file carries no note, so it is
// the one tracked entry the store may hold. Refusing it would break a deliberate
// and correct setup in the name of the fix above.
func TestACheckedInIgnoreDoesNotBlockLocalWrites(t *testing.T) {
	root := t.TempDir()
	paths := DefaultPaths(root)
	writeStoreFile(t, paths, gitignoreName, localIgnoreContent)

	seedStoreRepo(t, root)
	runStoreGit(t, root, "add", "-f", ".zero/memory/local/.gitignore")
	runStoreGit(t, root, "commit", "-m", "check in the ignore")

	if _, err := Write(paths, ScopeLocal, "private", "d", "machine-local secret"); err != nil {
		t.Fatalf("Write with only the ignore tracked = %v, want success", err)
	}
	if status := runStoreGit(t, root, "status", "--porcelain"); status != "" {
		t.Errorf("git status = %q, want empty", status)
	}
}

// PROJECT SCOPE IS SUPPOSED TO BE TRACKED. It is checked in beside the repo and
// reviewed like any other file, so the index check must not reach it: a second
// write to a committed project note is an ordinary edit, not a broken promise.
func TestAProjectNoteStaysWritableWhileTracked(t *testing.T) {
	root := t.TempDir()
	paths := DefaultPaths(root)
	if _, err := Write(paths, ScopeProject, "shared", "d", "first"); err != nil {
		t.Fatal(err)
	}
	seedStoreRepo(t, root)
	runStoreGit(t, root, "add", ".zero/memory/shared.md")
	runStoreGit(t, root, "commit", "-m", "track the project note")

	if _, err := Write(paths, ScopeProject, "shared", "d", "second"); err != nil {
		t.Fatalf("Write to a tracked project note = %v, want success", err)
	}
	note, err := Read(paths, ScopeProject, "shared")
	if err != nil {
		t.Fatal(err)
	}
	if note.Body != "second\n" {
		t.Errorf("project note body = %q, want %q", note.Body, "second\n")
	}
}

// UNANSWERABLE INSIDE A REPOSITORY IS A REFUSAL.
//
// The repository was found on disk, so the store IS somewhere git can track it;
// without git there is no way to learn whether it already does. Treating that as
// "nothing is tracked" is the fail-open being closed here, so it refuses instead
// — and only here, because a store with no repository above it never reaches
// this call.
func TestALocalWriteIsRefusedWhenGitCannotBeAsked(t *testing.T) {
	root := t.TempDir()
	paths := DefaultPaths(root)
	seedStoreRepo(t, root)

	t.Setenv("PATH", "")
	if _, err := Write(paths, ScopeLocal, "private", "d", "machine-local secret"); !errors.Is(err, ErrNotPrivate) {
		t.Fatalf("Write with git unreachable inside a repository = %v, want ErrNotPrivate", err)
	}
}

// NO REPOSITORY MEANS NOTHING TO ASK. The common workspace is not a checkout at
// all, and there git may not even be installed — so repository membership is
// decided from the filesystem and no subprocess runs. Removing git from PATH
// must not change the answer.
func TestALocalWriteOutsideAnyRepositoryNeedsNoGit(t *testing.T) {
	root := t.TempDir()
	paths := DefaultPaths(root)

	t.Setenv("PATH", "")
	if _, err := Write(paths, ScopeLocal, "private", "d", "machine-local secret"); err != nil {
		t.Fatalf("Write outside a repository with no git on PATH = %v, want success", err)
	}
	note, err := Read(paths, ScopeLocal, "private")
	if err != nil {
		t.Fatal(err)
	}
	if note.Body != "machine-local secret\n" {
		t.Errorf("note body = %q, want %q", note.Body, "machine-local secret\n")
	}
}

// AN INHERITED INDEX REDIRECT MUST NOT ANSWER FOR THE CHECKOUT.
//
// A hook, a rebase, or any git wrapper exports GIT_INDEX_FILE and GIT_DIR into
// the environment its children inherit, and zero inherits them like anything
// else. Left in place they point `git ls-files` at a different index — during a
// rebase, one holding none of the worktree's paths — and it answers "nothing is
// tracked" about a store that plainly is.
func TestAnInheritedIndexRedirectCannotHideTracking(t *testing.T) {
	root := t.TempDir()
	paths := DefaultPaths(root)
	writeStoreFile(t, paths, "private.md", "---\nname: private\n---\n\ncommitted body\n")

	seedStoreRepo(t, root)
	runStoreGit(t, root, "add", ".zero/memory/local/private.md")
	runStoreGit(t, root, "commit", "-m", "track the local note")

	// A path with no index at it reads as an empty index, which is exactly the
	// shape that makes a tracked store look clean.
	t.Setenv("GIT_INDEX_FILE", filepath.Join(t.TempDir(), "elsewhere.index"))
	if _, err := Write(paths, ScopeLocal, "private", "d", "machine-local secret"); !errors.Is(err, ErrNotPrivate) {
		t.Fatalf("Write with GIT_INDEX_FILE redirected = %v, want ErrNotPrivate", err)
	}
}

// THE REPOSITORY IS LOOKED FOR WHERE GIT WILL LOOK.
//
// os/exec chdirs into the store, so git discovers the repository from the
// resulting getcwd — the physical path. Walking the lexical path up instead
// visits ancestors the child process never has: with the workspace reached
// through a link into a repository subdirectory, the lexical walk leaves the
// repository immediately, finds no .git, and concludes nothing can be tracked
// about a store git tracks.
func TestTheRepositoryIsFoundThroughALinkedWorkspace(t *testing.T) {
	base := t.TempDir()
	repo := filepath.Join(base, "repo")
	inner := filepath.Join(repo, "inner")
	if err := os.MkdirAll(inner, 0o700); err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(base, "workspace")
	linkDir(t, inner, workspace)

	paths := DefaultPaths(workspace)
	writeStoreFile(t, paths, "private.md", "---\nname: private\n---\n\ncommitted body\n")
	seedStoreRepo(t, repo)
	runStoreGit(t, repo, "add", "inner/.zero/memory/local/private.md")
	runStoreGit(t, repo, "commit", "-m", "track the local note")

	if _, err := Write(paths, ScopeLocal, "private", "d", "machine-local secret"); !errors.Is(err, ErrNotPrivate) {
		t.Fatalf("Write through a linked workspace = %v, want ErrNotPrivate", err)
	}
	if status := runStoreGit(t, repo, "status", "--porcelain"); status != "" {
		t.Errorf("git status = %q, want empty", status)
	}
}

// A LINKED WORKTREE CARRIES .git AS A FILE, NOT A DIRECTORY, and it tracks its
// files exactly like an ordinary checkout. gitDirName's comment says so; nothing
// tested it. A verifier changed the presence check to `err == nil && info.IsDir()`
// and every other test in this file still passed, which means the whole
// linked-worktree and submodule class was resting on a comment.
//
// A REAL FIXTURE RATHER THAN A MOCK: `git worktree add` produces the genuine
// article — a .git FILE whose body is "gitdir: <path>" — so this exercises the
// same code a user's linked worktree does, instead of a stand-in that agrees
// with whatever the check happens to be.
func TestALocalWriteIsRefusedInsideALinkedWorktree(t *testing.T) {
	root := t.TempDir()
	main := filepath.Join(root, "main")
	if err := os.MkdirAll(main, 0o700); err != nil {
		t.Fatal(err)
	}
	seedStoreRepo(t, main)
	linked := filepath.Join(root, "linked")
	runStoreGit(t, main, "worktree", "add", linked)

	marker, err := os.Lstat(filepath.Join(linked, gitDirName))
	if err != nil {
		t.Fatalf("the linked worktree has no %s: %v", gitDirName, err)
	}
	if marker.IsDir() {
		t.Skipf("this git writes %s as a directory in a linked worktree; the case under test does not arise", gitDirName)
	}

	paths := DefaultPaths(linked)
	writeStoreFile(t, paths, "private.md", "---\nname: private\n---\n\nmachine-local\n")
	writeStoreFile(t, paths, ".gitignore", localIgnoreContent)
	runStoreGit(t, linked, "add", "-f", filepath.Join(".zero", "memory", "local", "private.md"))
	runStoreGit(t, linked, "commit", "-m", "track the local note")

	_, err = Write(paths, ScopeLocal, "private", "d", "replaced")
	if !errors.Is(err, ErrNotPrivate) {
		t.Fatalf("a tracked local note inside a linked worktree was writable: %v", err)
	}
	if status := runStoreGit(t, linked, "status", "--porcelain"); strings.TrimSpace(status) != "" {
		t.Errorf("the refused write still left an index-visible change:\n%s", status)
	}
}
