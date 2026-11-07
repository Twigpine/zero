package sandbox_test

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/Twigpine/zero/internal/mcp"
	"github.com/Twigpine/zero/internal/oauth"
	"github.com/Twigpine/zero/internal/sandbox"
)

func TestCredentialDeniesMatchTokenStoreFallbacks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows credential deny-read is tracked separately")
	}
	workspace, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	profileHome, err := os.MkdirTemp(".", ".credential-profile-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(profileHome) })
	profileHome, err = filepath.Abs(profileHome)
	if err != nil {
		t.Fatal(err)
	}
	envMap := map[string]string{"HOME": "", "USERPROFILE": profileHome, "XDG_CONFIG_HOME": ""}
	oauthPath, err := oauth.ResolveStorePath(envMap)
	if err != nil {
		t.Fatal(err)
	}
	mcpPath, err := mcp.ResolveTokenStorePath(envMap)
	if err != nil {
		t.Fatal(err)
	}
	engine := sandbox.NewEngine(sandbox.EngineOptions{
		WorkspaceRoot: workspace,
		Policy:        sandbox.DefaultPolicy(),
		Backend:       sandbox.Backend{Name: sandbox.BackendUnavailable, Platform: runtime.GOOS},
	})
	plan, err := engine.BuildCommandPlan(sandbox.CommandSpec{
		Name: "true",
		Dir:  workspace,
		Env:  []string{"HOME=", "USERPROFILE=" + profileHome, "XDG_CONFIG_HOME="},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, storePath := range []string{oauthPath, mcpPath} {
		want := filepath.Dir(storePath)
		if !containsPath(plan.PermissionProfile.FileSystem.DenyReadIfExists, want) {
			t.Fatalf("DenyReadIfExists = %#v, want token-store root %q", plan.PermissionProfile.FileSystem.DenyReadIfExists, want)
		}
	}
}

// TestCredentialDeniesMatchRelativeTokenOverridesFromStoreResolution runs the
// sandboxed command from a DIFFERENT directory than the Zero process, which is
// what makes this meaningful: the stores resolve a relative override with
// filepath.Abs against the process working directory, so that is the path the
// profile has to deny.
func TestCredentialDeniesMatchRelativeTokenOverridesFromStoreResolution(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows credential deny-read is tracked separately")
	}
	workspace := t.TempDir()
	commandDir := filepath.Join(workspace, "nested")
	if err := os.MkdirAll(commandDir, 0o755); err != nil {
		t.Fatal(err)
	}
	originalDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	// The Zero process stays in the workspace root while the command runs in the
	// nested directory, so the two resolutions differ.
	if err := os.Chdir(workspace); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(originalDir) }()
	// Model a process whose stores resolved while it was here. The profile pins
	// this directory once instead of re-reading it per plan, precisely so it
	// keeps naming the file oauth.ResolveStorePath below computed — the two
	// resolutions are asserted equal at the end.
	sandbox.PinProcessCredentialBaseDir(t, workspace)

	envMap := map[string]string{
		"HOME":                       filepath.Join(workspace, "home"),
		"ZERO_OAUTH_TOKENS_PATH":     "oauth/tokens.json",
		"ZERO_MCP_OAUTH_TOKENS_PATH": "mcp/tokens.json",
	}
	oauthPath, err := oauth.ResolveStorePath(envMap)
	if err != nil {
		t.Fatal(err)
	}
	mcpPath, err := mcp.ResolveTokenStorePath(envMap)
	if err != nil {
		t.Fatal(err)
	}
	engine := sandbox.NewEngine(sandbox.EngineOptions{
		WorkspaceRoot: workspace,
		Policy:        sandbox.DefaultPolicy(),
		Backend:       sandbox.Backend{Name: sandbox.BackendUnavailable, Platform: runtime.GOOS},
	})
	plan, err := engine.BuildCommandPlan(sandbox.CommandSpec{
		Name: "true",
		Dir:  commandDir,
		Env: []string{
			"HOME=" + envMap["HOME"],
			"ZERO_OAUTH_TOKENS_PATH=" + envMap["ZERO_OAUTH_TOKENS_PATH"],
			"ZERO_MCP_OAUTH_TOKENS_PATH=" + envMap["ZERO_MCP_OAUTH_TOKENS_PATH"],
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, storePath := range []string{oauthPath, mcpPath} {
		if containsPath(plan.PermissionProfile.FileSystem.DenyReadIfExists, storePath) {
			t.Fatalf("DenyReadIfExists = %#v, command override must not mask allowed workspace path %q", plan.PermissionProfile.FileSystem.DenyReadIfExists, storePath)
		}
	}
}

func containsPath(paths []string, want string) bool {
	want = filepath.Clean(want)
	for _, path := range paths {
		if filepath.Clean(path) == want {
			return true
		}
	}
	return false
}
