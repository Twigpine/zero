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
		"cancelled":         {"*\n!keep.md\n", false},
	} {
		if got := ignoresEverything(tc.content); got != tc.covered {
			t.Errorf("%s: ignoresEverything(%q) = %v, want %v (git's answer)", name, tc.content, got, tc.covered)
		}
	}
}
