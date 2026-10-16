package tools

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Gitlawb/zero/internal/daemon/remote"
)

func TestProtectedReadOpenParentFallback(t *testing.T) {
	for _, kind := range []string{"hard link", "sibling symlink", "escaping symlink"} {
		t.Run(kind, func(t *testing.T) {
			ws := t.TempDir()
			outside := t.TempDir()
			path := filepath.Join(outside, "notes.txt")
			token := filepath.Join(outside, "bridge-token")
			if kind == "escaping symlink" {
				token = filepath.Join(t.TempDir(), "bridge-token")
			}
			for name, content := range map[string]string{path: "ordinary notes\n", token: "bridge-secret\n"} {
				if err := os.WriteFile(name, []byte(content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv(remote.EnvToken, "")
			t.Setenv(remote.EnvTokenFile, token)
			t.Setenv(remote.EnvTokenFileResolved, "")
			t.Setenv(remote.EnvTokenFileIdentity, "")
			// Exercise the fallback directly: callers have already authorized an
			// extra root or spill file, but the workspace root cannot contain it.
			if _, _, err := rootedPathWithin([]string{ws}, path); err == nil {
				t.Fatal("fixture did not select the parent-directory fallback")
			}
			file, _, err := protectedReadOpen(path, ws)
			if err != nil {
				t.Fatalf("ordinary fallback read: %v", err)
			}
			content, readErr := io.ReadAll(file)
			closeErr := file.Close()
			if readErr != nil || closeErr != nil || string(content) != "ordinary notes\n" {
				t.Fatalf("ordinary content = %q, read error = %v, close error = %v", content, readErr, closeErr)
			}
			// Replace the previously authorized ordinary path. A hard link or
			// sibling symlink stays within the fallback root, so only the handle
			// credential check can refuse it; an escaping symlink must also fail.
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "hard link":
				err = os.Link(token, path)
			case "sibling symlink":
				err = os.Symlink(filepath.Base(token), path)
			default:
				err = os.Symlink(token, path)
			}
			if err != nil {
				t.Skipf("%s unavailable: %v", kind, err)
			}
			file, info, err := protectedReadOpen(path, ws)
			if file != nil {
				file.Close()
			}
			if err == nil || file != nil || info != nil {
				t.Fatalf("fallback returned a protected handle: file=%v info=%v err=%v", file, info, err)
			}
			if kind != "escaping symlink" && !strings.Contains(err.Error(), "remote bridge token and is never readable") {
				t.Fatalf("wanted credential rejection, got %v", err)
			}
		})
	}
}

// TestEngineLessRegistryMatrix drives every registry-dispatched tool that
// names a path through the plain registry API (Registry.Run — no sandbox
// engine) with a protected token selected, and separately with no token
// selected at all. Before protected_credentials.go this matrix would have
// failed on read_file, read_minified_file, grep, and glob: nothing upstream of
// those tools' own Run() ever consulted the protected set when no engine was
// supplied, because Registry.RunWithOptions only asks the engine at all when
// options.Sandbox is non-nil.
func TestEngineLessRegistryMatrix(t *testing.T) {
	setup := func(t *testing.T) (ws, token string) {
		t.Helper()
		ws = t.TempDir()
		token = filepath.Join(ws, "bridge-token")
		if err := os.WriteFile(token, []byte("bridge-secret\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(ws, "ordinary.txt"), []byte("ordinary\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("ZERO_DAEMON_REMOTE_TOKEN", "")
		t.Setenv("ZERO_DAEMON_REMOTE_TOKEN_FILE", token)
		return ws, token
	}

	t.Run("read_file", func(t *testing.T) {
		ws, _ := setup(t)
		registry := NewRegistry()
		registry.Register(NewScopedReadFileTool(ws, nil))
		result := registry.Run(context.Background(), "read_file", map[string]any{"path": "bridge-token"})
		if result.Status == StatusOK || strings.Contains(result.Output, "bridge-secret") {
			t.Fatalf("read_file leaked token: %+v", result)
		}
	})

	t.Run("read_minified_file", func(t *testing.T) {
		ws, _ := setup(t)
		registry := NewRegistry()
		registry.Register(NewScopedReadMinifiedFileTool(ws, nil))
		result := registry.Run(context.Background(), "read_minified_file", map[string]any{"path": "bridge-token"})
		if result.Status == StatusOK || strings.Contains(result.Output, "bridge-secret") {
			t.Fatalf("read_minified_file leaked token: %+v", result)
		}
	})

	t.Run("grep", func(t *testing.T) {
		ws, _ := setup(t)
		registry := NewRegistry()
		registry.Register(NewScopedGrepTool(ws, nil))
		result := registry.Run(context.Background(), "grep", map[string]any{"pattern": "bridge-secret"})
		if strings.Contains(result.Output, "bridge-token") {
			t.Fatalf("grep surfaced the protected filename: %+v", result)
		}
	})

	t.Run("glob", func(t *testing.T) {
		ws, _ := setup(t)
		registry := NewRegistry()
		registry.Register(NewScopedGlobTool(ws, nil))
		result := registry.Run(context.Background(), "glob", map[string]any{"pattern": "*"})
		if strings.Contains(result.Output, "bridge-token") {
			t.Fatalf("glob surfaced the protected filename: %+v", result)
		}
	})

	t.Run("list_directory", func(t *testing.T) {
		ws, _ := setup(t)
		registry := NewRegistry()
		registry.Register(NewScopedListDirectoryTool(ws, nil))
		result := registry.Run(context.Background(), "list_directory", map[string]any{"path": "."})
		if strings.Contains(result.Output, "bridge-token") {
			t.Fatalf("list_directory surfaced the protected filename: %+v", result)
		}
	})

	t.Run("write_file", func(t *testing.T) {
		ws, token := setup(t)
		registry := NewRegistry()
		registry.Register(NewScopedWriteFileTool(ws, nil))
		result := registry.Run(context.Background(), "write_file", map[string]any{"path": "bridge-token", "content": "attacker\n", "overwrite": true})
		if result.Status == StatusOK {
			t.Fatalf("write_file overwrote the protected token: %+v", result)
		}
		contents, err := os.ReadFile(token)
		if err != nil || string(contents) != "bridge-secret\n" {
			t.Fatalf("token changed after a denied write: contents=%q err=%v", contents, err)
		}
	})

	t.Run("edit_file", func(t *testing.T) {
		ws, token := setup(t)
		registry := NewRegistry()
		registry.Register(NewScopedEditFileTool(ws, nil))
		result := registry.Run(context.Background(), "edit_file", map[string]any{"path": "bridge-token", "old_string": "bridge-secret", "new_string": "attacker"})
		if result.Status == StatusOK {
			t.Fatalf("edit_file rewrote the protected token: %+v", result)
		}
		contents, err := os.ReadFile(token)
		if err != nil || string(contents) != "bridge-secret\n" {
			t.Fatalf("token changed after a denied edit: contents=%q err=%v", contents, err)
		}
	})

	t.Run("apply_patch unified", func(t *testing.T) {
		ws, token := setup(t)
		registry := NewRegistry()
		registry.Register(NewScopedApplyPatchTool(ws, nil))
		patch := "--- a/bridge-token\n+++ b/bridge-token\n@@ -1 +1 @@\n-bridge-secret\n+attacker\n"
		result := registry.Run(context.Background(), "apply_patch", map[string]any{"patch": patch})
		if result.Status == StatusOK {
			t.Fatalf("apply_patch rewrote the protected token: %+v", result)
		}
		contents, err := os.ReadFile(token)
		if err != nil || string(contents) != "bridge-secret\n" {
			t.Fatalf("token changed after a denied unified patch: contents=%q err=%v", contents, err)
		}
	})

	t.Run("apply_patch structured", func(t *testing.T) {
		ws, token := setup(t)
		registry := NewRegistry()
		registry.Register(NewScopedApplyPatchTool(ws, nil))
		patch := "*** Begin Patch\n*** Update File: bridge-token\n@@\n-bridge-secret\n+attacker\n*** End Patch\n"
		result := registry.Run(context.Background(), "apply_patch", map[string]any{"patch": patch})
		if result.Status == StatusOK {
			t.Fatalf("apply_patch (structured) rewrote the protected token: %+v", result)
		}
		contents, err := os.ReadFile(token)
		if err != nil || string(contents) != "bridge-secret\n" {
			t.Fatalf("token changed after a denied structured patch: contents=%q err=%v", contents, err)
		}
	})

	// Ordinary content must still flow through every tool with no token
	// configured at all — the mandatory guard must never turn into a permanent
	// deny of everything.
	t.Run("no token selected", func(t *testing.T) {
		ws := t.TempDir()
		if err := os.WriteFile(filepath.Join(ws, "ordinary.txt"), []byte("ordinary\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("ZERO_DAEMON_REMOTE_TOKEN", "")
		t.Setenv("ZERO_DAEMON_REMOTE_TOKEN_FILE", "")

		registry := NewRegistry()
		registry.Register(NewScopedReadFileTool(ws, nil))
		result := registry.Run(context.Background(), "read_file", map[string]any{"path": "ordinary.txt"})
		if result.Status != StatusOK || !strings.Contains(result.Output, "ordinary") {
			t.Fatalf("read_file with no token configured wrongly refused: %+v", result)
		}
	})
}

// TestProtectedReadOpenClosesTheCheckToUseWindow is the deterministic
// swap-race regression: a pathname check followed by a SEPARATE later open
// (what every direct file tool did before protectedReadOpen existed) leaves a
// window where a concurrent writer can replace an already-checked ordinary
// file with a symlink to the token before the later open runs. protectedReadOpen
// closes that window by deciding from the SAME handle content is read through,
// so there is no separate "check" step to race at all — every open
// independently re-verifies identity from its own freshly-obtained os.FileInfo.
//
// This is exercised without real goroutine concurrency (which cannot be made
// deterministic) by proving the invariant that makes the race impossible: an
// ordinary path served successfully once is re-verified, not cached, the next
// time it is opened — so a path that becomes the protected token BETWEEN two
// calls is caught on the second call exactly as if it had always been the
// token. Nothing about the first call's outcome could ever leak into the
// second.
func TestProtectedReadOpenClosesTheCheckToUseWindow(t *testing.T) {
	ws := t.TempDir()
	token := filepath.Join(ws, "bridge-token")
	if err := os.WriteFile(token, []byte("bridge-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	swappable := filepath.Join(ws, "notes.txt")
	if err := os.WriteFile(swappable, []byte("ordinary notes\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ZERO_DAEMON_REMOTE_TOKEN", "")
	t.Setenv("ZERO_DAEMON_REMOTE_TOKEN_FILE", token)

	registry := NewRegistry()
	registry.Register(NewScopedReadFileTool(ws, nil))

	before := registry.Run(context.Background(), "read_file", map[string]any{"path": "notes.txt"})
	if before.Status != StatusOK || !strings.Contains(before.Output, "ordinary notes") {
		t.Fatalf("ordinary read before the swap failed: %+v", before)
	}

	// The attacker's move: atomically replace the already-served ordinary file
	// with a hard link to the protected token. A pre-open pathname check taken
	// before this point would still describe the ordinary file; only a check
	// bound to the NEXT open's own handle can see the swap.
	if err := os.Remove(swappable); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(token, swappable); err != nil {
		t.Skipf("workspace filesystem is not hard-linkable: %v", err)
	}

	after := registry.Run(context.Background(), "read_file", map[string]any{"path": "notes.txt"})
	if after.Status == StatusOK || strings.Contains(after.Output, "bridge-secret") {
		t.Fatalf("read_file served the token through a path swapped after an earlier successful read: %+v", after)
	}
}

func TestStartupIdentityLifecycleProtectsReadFileAndGrep(t *testing.T) {
	ws := t.TempDir()
	token := filepath.Join(ws, "bridge-token")
	alias := filepath.Join(ws, "retired-token-alias")
	const oldSecret = "startup-lifecycle-secret"
	if err := os.WriteFile(token, []byte(oldSecret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(token, alias); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	t.Setenv(remote.EnvToken, "")
	t.Setenv(remote.EnvTokenFile, token)
	t.Setenv(remote.EnvTokenFileResolved, "")
	t.Setenv(remote.EnvTokenFileIdentity, "")
	auth, err := remote.NewAuthenticatorFromEnv()
	if err != nil {
		t.Fatalf("daemon startup: %v", err)
	}
	replacement := filepath.Join(ws, "replacement")
	if err := os.WriteFile(replacement, []byte("replacement-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, token); err != nil {
		t.Fatal(err)
	}
	if err := auth.Authenticate(oldSecret); err != nil {
		t.Fatalf("startup credential must remain live after path replacement: %v", err)
	}

	registry := NewRegistry()
	registry.Register(NewScopedReadFileTool(ws, nil))
	registry.Register(NewScopedGrepTool(ws, nil))
	read := registry.Run(context.Background(), "read_file", map[string]any{"path": filepath.Base(alias)})
	if read.Status == StatusOK || strings.Contains(read.Output, oldSecret) {
		t.Fatalf("read_file disclosed retired startup token: %+v", read)
	}
	grep := registry.Run(context.Background(), "grep", map[string]any{"pattern": oldSecret})
	if strings.Contains(grep.Output, filepath.Base(alias)) || strings.Contains(grep.Output, oldSecret) {
		t.Fatalf("grep disclosed retired startup token: %+v", grep)
	}
}

func TestProtectedReadOpenRejectsRacedEscapingSymlink(t *testing.T) {
	ws := t.TempDir()
	token := filepath.Join(t.TempDir(), "bridge-token")
	if err := os.WriteFile(token, []byte("bridge-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(ws, "notes.txt")
	if err := os.WriteFile(path, []byte("ordinary\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ZERO_DAEMON_REMOTE_TOKEN", "")
	t.Setenv("ZERO_DAEMON_REMOTE_TOKEN_FILE", token)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(token, path); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	file, _, err := protectedReadOpen(path, ws)
	if file != nil {
		file.Close()
	}
	if err == nil {
		t.Fatal("rooted read followed a raced symlink outside the workspace")
	}
}

func TestRootedMutationPublishDoesNotClobberRacedTokenAlias(t *testing.T) {
	for _, name := range []string{"write_file", "edit_file", "unified_patch"} {
		t.Run(name, func(t *testing.T) {
			ws := t.TempDir()
			token := filepath.Join(ws, "bridge-token")
			path := filepath.Join(ws, "notes.txt")
			if err := os.WriteFile(token, []byte("bridge-secret\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("ordinary\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("ZERO_DAEMON_REMOTE_TOKEN", "")
			t.Setenv("ZERO_DAEMON_REMOTE_TOKEN_FILE", token)
			if err := protectedMutationDenied(path, ws); err != nil {
				t.Fatalf("ordinary preflight failed: %v", err)
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Link(token, path); err != nil {
				t.Skipf("workspace filesystem is not hard-linkable: %v", err)
			}
			root, err := os.OpenRoot(ws)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			switch name {
			case "unified_patch":
				// A unified diff is translated into the same operations the
				// structured engine applies, so it publishes through the same
				// rooted atomic write rather than an unrestricted pathname open.
				operations, perr := parseUnifiedPatch("--- a/notes.txt\n+++ b/notes.txt\n@@ -1 +1 @@\n-ordinary\n+patched\n")
				if perr != nil {
					t.Fatal(perr)
				}
				var changes []structuredPatchChange
				changes, err = planStructuredPatch(root, operations, nil)
				if err == nil {
					_, err = applyStructuredPatchChanges(root, ws, changes, nil)
				}
			default:
				_, err = writeRootedFile(root, "notes.txt", path, ws, []byte("updated\n"), 0o644, false)
			}
			// Planning or the write-side handle check refuses the raced alias. The
			// token must remain unchanged regardless of which layer catches it.
			if err != nil && !strings.Contains(err.Error(), "remote bridge token") {
				t.Fatal(err)
			}
			if content, err := os.ReadFile(token); err != nil || string(content) != "bridge-secret\n" {
				t.Fatalf("token changed through raced alias: content=%q err=%v", content, err)
			}
		})
	}
}

func TestStructuredPatchPublishDoesNotClobberRacedTokenAlias(t *testing.T) {
	ws := t.TempDir()
	token := filepath.Join(ws, "bridge-token")
	path := filepath.Join(ws, "notes.txt")
	if err := os.WriteFile(token, []byte("bridge-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("ordinary\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ZERO_DAEMON_REMOTE_TOKEN", "")
	t.Setenv("ZERO_DAEMON_REMOTE_TOKEN_FILE", token)
	operations, err := parseStructuredPatch("*** Begin Patch\n*** Update File: notes.txt\n@@\n-ordinary\n+updated\n*** End Patch\n")
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(ws)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	changes, err := planStructuredPatch(root, operations, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 || strings.Contains(changes[0].before, "bridge-secret") {
		t.Fatalf("structured plan exposed unexpected content: %#v", changes)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(token, path); err != nil {
		t.Skipf("workspace filesystem is not hard-linkable: %v", err)
	}
	// The commit either refuses the swapped target (the file changed between
	// planning and commit, or it is the protected credential) or publishes
	// atomically over the alias. Either is acceptable; an altered token is not.
	_, err = applyStructuredPatchChanges(root, ws, changes, nil)
	if err != nil && !strings.Contains(err.Error(), "changed on disk") &&
		!strings.Contains(err.Error(), "remote bridge token") {
		t.Fatal(err)
	}
	if content, err := os.ReadFile(token); err != nil || string(content) != "bridge-secret\n" {
		t.Fatalf("token changed through raced structured patch alias: content=%q err=%v", content, err)
	}
}

func TestStructuredPatchPlanningRejectsProtectedAliasHandle(t *testing.T) {
	ws := t.TempDir()
	token := filepath.Join(ws, "bridge-token")
	alias := filepath.Join(ws, "notes.txt")
	if err := os.WriteFile(token, []byte("bridge-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(token, alias); err != nil {
		t.Skipf("workspace filesystem is not hard-linkable: %v", err)
	}
	t.Setenv("ZERO_DAEMON_REMOTE_TOKEN", "")
	t.Setenv("ZERO_DAEMON_REMOTE_TOKEN_FILE", token)
	operations, err := parseStructuredPatch("*** Begin Patch\n*** Update File: notes.txt\n@@\n-bridge-secret\n+attacker\n*** End Patch\n")
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(ws)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if changes, err := planStructuredPatch(root, operations, nil); err == nil || len(changes) != 0 {
		t.Fatalf("structured plan accepted protected alias: changes=%#v err=%v", changes, err)
	}
}

func TestDirectFileToolsPreserveHardLinks(t *testing.T) {
	for _, name := range []string{"write_file", "edit_file"} {
		t.Run(name, func(t *testing.T) {
			ws := t.TempDir()
			path := filepath.Join(ws, "file.txt")
			alias := filepath.Join(ws, "alias.txt")
			if err := os.WriteFile(path, []byte("before\n"), 0o640); err != nil {
				t.Fatal(err)
			}
			if err := os.Link(path, alias); err != nil {
				t.Skipf("workspace filesystem is not hard-linkable: %v", err)
			}

			var result Result
			switch name {
			case "write_file":
				result = NewScopedWriteFileTool(ws, nil).(optionsAwareTool).RunWithOptions(context.Background(), map[string]any{
					"path": "file.txt", "content": "after\n", "overwrite": true,
				}, RunOptions{})
			case "edit_file":
				tracker := NewFileTracker()
				read := NewScopedReadFileTool(ws, nil).(optionsAwareTool).RunWithOptions(context.Background(), map[string]any{"path": "file.txt"}, RunOptions{FileTracker: tracker})
				if read.Status != StatusOK {
					t.Fatalf("read failed: %+v", read)
				}
				result = NewScopedEditFileTool(ws, nil).(optionsAwareTool).RunWithOptions(context.Background(), map[string]any{
					"path": "file.txt", "old_string": "before", "new_string": "after",
				}, RunOptions{FileTracker: tracker})
			}
			if result.Status != StatusOK {
				t.Fatalf("%s failed: %+v", name, result)
			}
			if content, err := os.ReadFile(alias); err != nil || string(content) != "after\n" {
				t.Fatalf("hard-link content = %q, err=%v", content, err)
			}
			pathInfo, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			aliasInfo, err := os.Stat(alias)
			if err != nil {
				t.Fatal(err)
			}
			if !os.SameFile(pathInfo, aliasInfo) {
				t.Fatal("tool replaced the existing inode and broke its hard link")
			}
		})
	}
}
