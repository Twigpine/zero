package tools

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Gitlawb/zero/internal/daemon/remote"
	"github.com/Gitlawb/zero/internal/sandbox"
)

// TestApplyPatchExecutesHeaderPathBytesVerbatim closes the parser/consumer gap
// between the layer that authorizes a patch and the layer that applies it.
//
// The executor parser supplies both the paths the sandbox authorizes and the
// operations that run. If either consumer trimmed those paths independently, a
// patch naming the unprotected sibling "bridge-token " could clear the gate and
// then mutate the protected "bridge-token" beside it.
//
// Each case therefore proves both halves at once: the whitespace-bearing name
// is a real, patchable file (the control effect lands on it, byte for byte),
// and the protected token file sitting next to it is untouched.
func TestApplyPatchExecutesHeaderPathBytesVerbatim(t *testing.T) {
	const (
		tokenName     = "bridge-token"
		tokenContents = "bridge-secret\n"
		siblingBefore = "sibling-original\n"
		siblingAfter  = "sibling-patched\n"
	)

	for _, tc := range []struct {
		name string
		// sibling is the unprotected file whose name differs from the token's
		// only by whitespace the executor must not trim away.
		sibling string
		patch   string
		// moved, when set, is the name the patch renames or copies sibling to.
		moved string
		// sourceGone marks a rename, where the source must not survive.
		sourceGone bool
	}{
		{
			name:    "update with trailing space",
			sibling: tokenName + " ",
			patch: "--- a/bridge-token \n" +
				"+++ b/bridge-token \n" +
				"@@ -1 +1 @@\n" +
				"-sibling-original\n" +
				"+sibling-patched\n",
		},
		{
			name:    "update with leading space",
			sibling: " " + tokenName,
			patch: "--- a/ bridge-token\n" +
				"+++ b/ bridge-token\n" +
				"@@ -1 +1 @@\n" +
				"-sibling-original\n" +
				"+sibling-patched\n",
		},
		{
			name:    "c-quoted update with trailing space",
			sibling: tokenName + " ",
			patch: "diff --git \"a/bridge-token \" \"b/bridge-token \"\n" +
				"--- \"a/bridge-token \"\n" +
				"+++ \"b/bridge-token \"\n" +
				"@@ -1 +1 @@\n" +
				"-sibling-original\n" +
				"+sibling-patched\n",
		},
		{
			name:    "copy with trailing space source",
			sibling: tokenName + " ",
			patch: "diff --git a/bridge-token  b/bridge-copy\n" +
				"similarity index 100%\n" +
				"copy from bridge-token \n" +
				"copy to bridge-copy\n",
			moved: "bridge-copy",
		},
		{
			name:    "rename with leading space source",
			sibling: " " + tokenName,
			patch: "diff --git a/ bridge-token b/bridge-renamed\n" +
				"similarity index 100%\n" +
				"rename from  bridge-token\n" +
				"rename to bridge-renamed\n",
			moved:      "bridge-renamed",
			sourceGone: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if runtime.GOOS == "windows" && strings.HasSuffix(tc.sibling, " ") {
				t.Skip("Windows filenames cannot end in a space")
			}

			ws, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatalf("EvalSymlinks: %v", err)
			}
			token := filepath.Join(ws, tokenName)
			if err := os.WriteFile(token, []byte(tokenContents), 0o600); err != nil {
				t.Fatalf("write token: %v", err)
			}
			sibling := filepath.Join(ws, tc.sibling)
			if err := os.WriteFile(sibling, []byte(siblingBefore), 0o600); err != nil {
				t.Fatalf("write sibling: %v", err)
			}

			t.Setenv(remote.EnvToken, "")
			t.Setenv(remote.EnvTokenFile, token)
			t.Setenv(remote.EnvTokenFileResolved, "")
			if err := remote.CanonicalizeTokenFileEnv(); err != nil {
				t.Fatalf("CanonicalizeTokenFileEnv: %v", err)
			}

			engine := sandbox.NewEngine(sandbox.EngineOptions{WorkspaceRoot: ws, Policy: sandbox.DefaultPolicy()})
			registry := NewRegistry()
			registry.Register(NewScopedApplyPatchTool(ws, nil))
			result := registry.RunWithOptions(context.Background(), "apply_patch", map[string]any{
				"patch": tc.patch,
			}, RunOptions{Sandbox: engine, PermissionGranted: true})
			// The control: the patch names an unprotected file, so it must be
			// executable. A refusal here would make the token assertion below
			// vacuous — every patch protects the token if no patch ever runs.
			if result.Status != StatusOK {
				t.Fatalf("patch on the unprotected sibling was refused: status=%s output=%q", result.Status, result.Output)
			}

			target, want := sibling, siblingAfter
			if tc.moved != "" {
				target, want = filepath.Join(ws, tc.moved), siblingBefore
			}
			contents, err := os.ReadFile(target)
			if err != nil || string(contents) != want {
				t.Fatalf("patch target %q: contents=%q err=%v, want %q", filepath.Base(target), contents, err, want)
			}
			if tc.sourceGone {
				if _, err := os.Stat(sibling); !os.IsNotExist(err) {
					t.Fatalf("rename left its source %q in place: err=%v", tc.sibling, err)
				}
			}

			// The whole point: the effect landed on the name the gate read, and
			// the protected token one byte away is untouched.
			tokenContentsAfter, err := os.ReadFile(token)
			if err != nil || string(tokenContentsAfter) != tokenContents {
				t.Fatalf("protected token changed: contents=%q err=%v", tokenContentsAfter, err)
			}
		})
	}
}

// TestApplyPatchDeniesWhitespaceNeighbourOfProtectedToken is the inverse of the
// case above: when the whitespace-bearing name IS the protected token, the same
// bytes must reach the gate and be refused before any file is opened.
func TestApplyPatchDeniesWhitespaceNeighbourOfProtectedToken(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows filenames cannot end in a space")
	}
	const tokenContents = "bridge-secret\n"

	for _, tc := range []struct {
		name  string
		patch string
		moved string
	}{
		{
			name: "update",
			patch: "--- a/bridge-token \n" +
				"+++ b/bridge-token \n" +
				"@@ -1 +1 @@\n" +
				"-bridge-secret\n" +
				"+attacker-controlled\n",
		},
		{
			name: "copy",
			patch: "diff --git a/bridge-token  b/exfiltrated\n" +
				"similarity index 100%\n" +
				"copy from bridge-token \n" +
				"copy to exfiltrated\n",
			moved: "exfiltrated",
		},
		{
			name: "rename",
			patch: "diff --git a/bridge-token  b/exfiltrated\n" +
				"similarity index 100%\n" +
				"rename from bridge-token \n" +
				"rename to exfiltrated\n",
			moved: "exfiltrated",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatalf("EvalSymlinks: %v", err)
			}
			token := filepath.Join(ws, "bridge-token ")
			if err := os.WriteFile(token, []byte(tokenContents), 0o600); err != nil {
				t.Fatalf("write token: %v", err)
			}

			t.Setenv(remote.EnvToken, "")
			t.Setenv(remote.EnvTokenFile, token)
			t.Setenv(remote.EnvTokenFileResolved, "")
			if err := remote.CanonicalizeTokenFileEnv(); err != nil {
				t.Fatalf("CanonicalizeTokenFileEnv: %v", err)
			}

			engine := sandbox.NewEngine(sandbox.EngineOptions{WorkspaceRoot: ws, Policy: sandbox.DefaultPolicy()})
			registry := NewRegistry()
			registry.Register(NewScopedApplyPatchTool(ws, nil))
			result := registry.RunWithOptions(context.Background(), "apply_patch", map[string]any{
				"patch": tc.patch,
			}, RunOptions{Sandbox: engine, PermissionGranted: true})
			if result.Status == StatusOK || !strings.Contains(result.Output, "remote bridge token") {
				t.Fatalf("apply_patch: status=%s output=%q, want bridge-token denial", result.Status, result.Output)
			}
			contents, err := os.ReadFile(token)
			if err != nil || string(contents) != tokenContents {
				t.Fatalf("token changed after denied patch: contents=%q err=%v", contents, err)
			}
			if tc.moved != "" {
				if _, err := os.Stat(filepath.Join(ws, tc.moved)); !os.IsNotExist(err) {
					t.Fatalf("denied patch created %q: err=%v", tc.moved, err)
				}
			}
		})
	}
}
