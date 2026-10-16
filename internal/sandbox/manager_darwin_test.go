package sandbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSandboxManagerRejectsMacOSTokenHardLinkableIntoWritableWorkspace(t *testing.T) {
	workspace := t.TempDir()
	token := filepath.Join(t.TempDir(), "bridge-token")
	if err := os.WriteFile(token, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}

	probe := filepath.Join(workspace, "token-hard-link-probe")
	if err := os.Link(token, probe); err != nil {
		t.Skipf("fixture paths are not hard-linkable: %v", err)
	}
	if err := os.Remove(probe); err != nil {
		t.Fatal(err)
	}

	t.Setenv(daemonRemoteTokenEnv, "")
	t.Setenv(daemonRemoteTokenFileEnv, token)
	t.Setenv(daemonRemoteTokenFileResolvedEnv, "")
	policy := DefaultPolicy()
	backend := Backend{
		Name:            BackendMacOSSeatbelt,
		Available:       true,
		Executable:      "/usr/bin/sandbox-exec",
		Platform:        "darwin",
		CommandWrapping: true,
		NativeIsolation: true,
	}
	_, err := NewSandboxManager(SandboxManagerOptions{GOOS: "darwin", Backend: backend}).BuildCommandPlan(SandboxManagerRequest{
		WorkspaceRoot:     workspace,
		Command:           CommandSpec{Name: "/bin/sh", Args: []string{"-c", "true"}, Dir: workspace},
		Policy:            policy,
		Profile:           PermissionProfileFromPolicy(workspace, policy, nil),
		Preference:        SandboxPreferenceAuto,
		ValidateExecution: true,
	})
	if err == nil || !strings.Contains(err.Error(), "file-backed remote token") {
		t.Fatalf("BuildCommandPlan error = %v, want macOS file-token shell refusal", err)
	}
}
