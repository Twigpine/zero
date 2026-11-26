package memory

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/gitsafe"
)

func TestLocalPrivacyNeverFetchesMissingSparseTrees(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_DEFAULT_HASH", "sha1")
	root := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		return strings.TrimSpace(runStoreGit(t, root, args...))
	}
	var major, minor int
	if _, err := fmt.Sscanf(git("--version"), "git version %d.%d", &major, &minor); err != nil {
		t.Fatal(err)
	}
	// Only the fixture needs sparse-checkout init --sparse-index (Git 2.32).
	// The production query retains its existing Git 1.9 floor.
	if major < 2 || (major == 2 && minor < 32) {
		t.Skip("sparse-index fixture requires Git 2.32")
	}
	git("init", "-q")
	paths := DefaultPaths(root)
	const original = "---\nname: private\n---\n\noriginal\n"
	writeStoreFile(t, paths, "private.md", original)
	if err := os.Mkdir(filepath.Join(root, "keep"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "keep", "note.md"), []byte("kept"), 0600); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("-c", "core.hooksPath="+os.DevNull, "-c", "commit.gpgSign=false", "commit", "-qm", "fixture")
	tree := git("rev-parse", "HEAD:.zero")
	git("sparse-checkout", "init", "--cone", "--sparse-index")
	git("sparse-checkout", "set", "keep")
	index, err := os.ReadFile(filepath.Join(root, ".git", "index"))
	if err != nil {
		t.Fatal(err)
	}
	oid, err := hex.DecodeString(tree)
	if err != nil || len(oid) != 20 || !bytes.Contains(index, []byte("sdir")) || !bytes.Contains(index, oid) {
		t.Fatalf("fixture did not retain the excluded tree in a sparse index: %v", err)
	}
	// A healthy sparse query still finds tracked notes, without advice being
	// mistaken for a broken query. Restore a working note for the write test.
	entries, err := trackedStoreEntries(root, paths.LocalDir)
	if err != nil || strings.Join(entries, ",") != "private.md" {
		t.Fatalf("healthy sparse query = %q, %v", entries, err)
	}
	note := writeStoreFile(t, paths, "private.md", original)
	writeStoreFile(t, paths, gitignoreName, localIgnoreContent)
	marker := filepath.Join(root, "upload-pack-ran")
	program := "echo invoked > '" + strings.ReplaceAll(filepath.ToSlash(marker), "'", "'\\''") + "'; exit 1 #"
	git("config", "core.repositoryformatversion", "1")
	git("config", "extensions.partialClone", "origin")
	git("config", "remote.origin.url", filepath.ToSlash(t.TempDir()))
	git("config", "remote.origin.promisor", "true")
	git("config", "remote.origin.partialclonefilter", "blob:none")
	git("config", "remote.origin.uploadpack", program)
	git("config", "protocol.file.allow", "always")
	if err := os.Remove(filepath.Join(root, ".git", "objects", tree[:2], tree[2:])); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_ALLOW_PROTOCOL", "file")
	t.Setenv("GIT_NO_LAZY_FETCH", "0")
	relative, err := filepath.Rel(root, paths.LocalDir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	// Positive control: the same query with the fetch guards removed really
	// reaches the benign marker-writing upload-pack. No network is involved.
	control := gitsafe.IndexEntriesCommand(ctx, root, filepath.ToSlash(relative))
	control.Env = append(withoutPrivacyFetchGuards(control.Env), "GIT_ALLOW_PROTOCOL=file", "GIT_NO_LAZY_FETCH=0")
	out, _ := control.CombinedOutput()
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("positive control did not reach upload-pack: %v; %s", err, out)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	for _, fallback := range []bool{false, true} {
		t.Run(map[bool]string{false: "no-lazy-fetch", true: "transport-backstop"}[fallback], func(t *testing.T) {
			cmd := gitsafe.IndexEntriesCommand(ctx, root, filepath.ToSlash(relative))
			if fallback {
				// Emulate Git that ignores the newer no-lazy-fetch setting:
				// the protocol allow-list must stop transport independently.
				cmd.Env = append(withoutPrivacyFetchGuards(cmd.Env), "GIT_ALLOW_PROTOCOL=", "GIT_NO_LAZY_FETCH=0")
			}
			var diagnostics bytes.Buffer
			cmd.Stderr = &diagnostics
			if err := cmd.Run(); err == nil && diagnostics.Len() == 0 {
				t.Error("missing tree was silently accepted as a complete query")
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("privacy query ran repository upload-pack: %v", err)
			}
		})
	}
	// Git can return zero with an empty stdout even after a missing-tree error.
	// The real write boundary must refuse that result and retain the old note.
	if _, err := Write(paths, ScopeLocal, "private", "d", "replacement"); !errors.Is(err, ErrNotPrivate) {
		t.Errorf("missing sparse tree admitted a local write: %v", err)
	}
	if body, err := os.ReadFile(note); err != nil || string(body) != original {
		t.Errorf("failed privacy query replaced the old note: %q, %v", body, err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("local write ran repository upload-pack: %v", err)
	}
}

func withoutPrivacyFetchGuards(env []string) []string {
	var filtered []string
	for _, entry := range env {
		name, _, _ := strings.Cut(entry, "=")
		if strings.EqualFold(name, "GIT_ALLOW_PROTOCOL") || strings.EqualFold(name, "GIT_NO_LAZY_FETCH") {
			continue
		}
		filtered = append(filtered, entry)
	}
	return filtered
}
