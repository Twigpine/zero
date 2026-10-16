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

func TestApplyPatchDeniesDaemonTokenUnderExactWhitespaceCwd(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Win32 normalizes terminal directory whitespace")
	}
	for _, cwd := range []string{" token-dir", "token-dir "} {
		t.Run(strings.ReplaceAll(cwd, " ", "_"), func(t *testing.T) {
			workspace := t.TempDir()
			dir := filepath.Join(workspace, cwd)
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			token := filepath.Join(dir, "token")
			if err := os.WriteFile(token, []byte("bridge-secret\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv(remote.EnvToken, "")
			t.Setenv(remote.EnvTokenFile, token)

			registry := NewRegistry()
			registry.Register(NewScopedApplyPatchTool(workspace, nil))
			engine := sandbox.NewEngine(sandbox.EngineOptions{
				WorkspaceRoot: workspace,
				Policy:        sandbox.DefaultPolicy(),
			})
			result := registry.RunWithOptions(context.Background(), "apply_patch", map[string]any{
				"cwd": cwd,
				"patch": "--- a/token\n" +
					"+++ b/token\n" +
					"@@ -1 +1 @@\n" +
					"-bridge-secret\n" +
					"+attacker-controlled\n",
			}, RunOptions{Sandbox: engine, PermissionGranted: true})
			if result.Status == StatusOK || !strings.Contains(result.Output, "remote bridge token") {
				t.Fatalf("apply_patch with cwd %q: status=%s output=%q, want token denial", cwd, result.Status, result.Output)
			}
			contents, err := os.ReadFile(token)
			if err != nil || string(contents) != "bridge-secret\n" {
				t.Fatalf("token changed after denied patch: contents=%q err=%v", contents, err)
			}
		})
	}
}
