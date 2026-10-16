package sandbox

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildAndParseWindowsSandboxSetupArgs(t *testing.T) {
	home := t.TempDir()
	args, err := BuildWindowsSandboxSetupArgs(WindowsSandboxSetupArgsOptions{
		SandboxHome:    home,
		CommandCWD:     `C:\workspace\src`,
		WorkspaceRoots: []string{`C:\workspace`},
		PermissionProfile: PermissionProfile{
			FileSystem: FileSystemPolicy{
				Kind: FileSystemRestricted,
				WriteRoots: []WritableRoot{
					{Root: `C:\workspace`, ProtectedMetadataNames: []string{".git"}},
				},
				DenyRead: []string{`C:\workspace\secret`},
			},
			Network: NetworkPolicy{Mode: NetworkDeny},
		},
	})
	if err != nil {
		t.Fatalf("BuildWindowsSandboxSetupArgs: %v", err)
	}
	config, err := ParseWindowsSandboxSetupArgs(args)
	if err != nil {
		t.Fatalf("ParseWindowsSandboxSetupArgs: %v", err)
	}
	if config.SandboxHome != home || config.CommandCWD != `C:\workspace\src` || len(config.WorkspaceRoots) != 1 || config.WorkspaceRoots[0] != `C:\workspace` {
		t.Fatalf("setup config = %#v, want sandbox home, command cwd, and workspace root", config)
	}
	if config.PermissionProfile.FileSystem.Kind != FileSystemRestricted || len(config.PermissionProfile.FileSystem.DenyRead) != 1 {
		t.Fatalf("permission profile = %#v, want restricted deny-read profile", config.PermissionProfile)
	}
}

func TestWindowsSandboxSetupPathForRunner(t *testing.T) {
	got := WindowsSandboxSetupPathForRunner(filepath.Join("C:", "zero", WindowsSandboxCommandRunnerName))
	want := filepath.Join("C:", "zero", WindowsSandboxSetupName)
	if got != want {
		t.Fatalf("WindowsSandboxSetupPathForRunner = %q, want %q", got, want)
	}
	if got := WindowsSandboxSetupPathForRunner(""); got != "" {
		t.Fatalf("empty runner setup path = %q, want empty", got)
	}
}

func TestRunWindowsSandboxSetupRejectsInvalidArgs(t *testing.T) {
	var stderr bytes.Buffer
	code := RunWindowsSandboxSetup([]string{"--command-cwd", `C:\workspace`}, &stderr)
	if code != 2 {
		t.Fatalf("exit code = %d, want usage error", code)
	}
	if !strings.Contains(stderr.String(), WindowsSandboxSetupName) {
		t.Fatalf("stderr = %q, want setup helper name", stderr.String())
	}
}

func TestRunWindowsSandboxSetupRejectsDenyReadUpfront(t *testing.T) {
	home := t.TempDir()
	args, err := BuildWindowsSandboxSetupArgs(WindowsSandboxSetupArgsOptions{
		SandboxHome:    home,
		CommandCWD:     `C:\workspace\src`,
		WorkspaceRoots: []string{`C:\workspace`},
		PermissionProfile: PermissionProfile{
			FileSystem: FileSystemPolicy{
				Kind:       FileSystemRestricted,
				WriteRoots: []WritableRoot{{Root: `C:\workspace`}},
				DenyRead:   []string{`C:\workspace\secret`},
			},
			Network: NetworkPolicy{Mode: NetworkDeny},
		},
	})
	if err != nil {
		t.Fatalf("BuildWindowsSandboxSetupArgs: %v", err)
	}
	var stderr bytes.Buffer
	code := RunWindowsSandboxSetup(args, &stderr)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1 for unsupported DenyRead", code)
	}
	if !strings.Contains(stderr.String(), "DenyRead is not supported") {
		t.Fatalf("stderr = %q, want DenyRead unsupported rejection before elevation check", stderr.String())
	}
	if strings.Contains(stderr.String(), "Administrator rights are required") {
		t.Fatalf("stderr = %q, should not claim Administrator rights when DenyRead is unsupported", stderr.String())
	}
}

func TestWindowsSandboxSetupMarkerRefreshesWhenProfileChanges(t *testing.T) {
	config := WindowsSandboxSetupConfig{
		SandboxHome:    t.TempDir(),
		CommandCWD:     `C:\workspace`,
		WorkspaceRoots: []string{`C:\workspace`},
		PermissionProfile: PermissionProfile{
			FileSystem: FileSystemPolicy{
				Kind:       FileSystemRestricted,
				WriteRoots: []WritableRoot{{Root: `C:\workspace`}},
				DenyRead:   []string{`C:\workspace\secret-read`},
			},
			Network: NetworkPolicy{Mode: NetworkDeny},
		},
	}
	if _, err := WriteWindowsSandboxSetupMarker(config); err != nil {
		t.Fatalf("WriteWindowsSandboxSetupMarker: %v", err)
	}
	if err := ValidateWindowsSandboxSetupMarker(config); err != nil {
		t.Fatalf("ValidateWindowsSandboxSetupMarker unchanged: %v", err)
	}

	changed := config
	changed.PermissionProfile.FileSystem.DenyRead = []string{`C:\workspace\other-secret`}
	err := ValidateWindowsSandboxSetupMarker(changed)
	if err == nil || !strings.Contains(err.Error(), "out of date") {
		t.Fatalf("ValidateWindowsSandboxSetupMarker changed error = %v, want out of date", err)
	}
}

// One setup marker must validly serve BOTH network modes: an approved (allow)
// network command and an ordinary (deny) command both validate against the same
// setup. This is the fix for the "network policy changed" brick that hit every
// approved network command (curl, git push, …). The per-command mode is enforced
// at runtime by the token's SID set, not by which marker exists.
func TestWindowsSandboxSetupMarkerValidatesBothNetworkModes(t *testing.T) {
	deny := WindowsSandboxSetupConfig{
		SandboxHome:    t.TempDir(),
		CommandCWD:     `C:\workspace`,
		WorkspaceRoots: []string{`C:\workspace`},
		PermissionProfile: PermissionProfile{
			FileSystem: FileSystemPolicy{
				Kind:       FileSystemRestricted,
				WriteRoots: []WritableRoot{{Root: `C:\workspace`}},
			},
			Network: NetworkPolicy{Mode: NetworkDeny},
		},
	}
	// Set up once (as a deny command would).
	if _, err := WriteWindowsSandboxSetupMarker(deny); err != nil {
		t.Fatalf("WriteWindowsSandboxSetupMarker: %v", err)
	}
	if err := ValidateWindowsSandboxSetupMarker(deny); err != nil {
		t.Fatalf("deny command must validate against its own setup: %v", err)
	}
	// An APPROVED (allow) command must ALSO validate against the SAME setup —
	// previously this bricked with "network policy changed".
	allow := deny
	allow.PermissionProfile.Network = NetworkPolicy{Mode: NetworkAllow}
	if err := ValidateWindowsSandboxSetupMarker(allow); err != nil {
		t.Fatalf("approved (allow) command must validate against the deny setup, got: %v", err)
	}
}

// A pre-v5 marker on disk must be rejected as out of date so the schema bump
// forces a clean re-setup (old markers had legacy DenyWrite ACEs).
func TestWindowsSandboxSetupMarkerRejectsOldSchema(t *testing.T) {
	config := WindowsSandboxSetupConfig{
		SandboxHome:    t.TempDir(),
		CommandCWD:     `C:\workspace`,
		WorkspaceRoots: []string{`C:\workspace`},
		PermissionProfile: PermissionProfile{
			FileSystem: FileSystemPolicy{Kind: FileSystemRestricted, WriteRoots: []WritableRoot{{Root: `C:\workspace`}}},
			Network:    NetworkPolicy{Mode: NetworkDeny},
		},
	}
	marker, err := BuildWindowsSandboxSetupMarker(config)
	if err != nil {
		t.Fatalf("BuildWindowsSandboxSetupMarker: %v", err)
	}
	marker.SchemaVersion = 4
	bytes, err := json.Marshal(marker)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(WindowsSandboxSetupMarkerPath(config.SandboxHome), bytes, 0o600); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	err = ValidateWindowsSandboxSetupMarker(config)
	if err == nil || !strings.Contains(err.Error(), "out of date") {
		t.Fatalf("schema-4 marker must be out of date, got: %v", err)
	}
}

func TestWindowsSandboxSetupConfigFromCommandPreservesProfileInputs(t *testing.T) {
	command := WindowsSandboxCommandConfig{
		SandboxHome:    t.TempDir(),
		CommandCWD:     `C:\workspace\src`,
		WorkspaceRoots: []string{`C:\workspace`},
		PermissionProfile: PermissionProfile{
			FileSystem: FileSystemPolicy{
				Kind:       FileSystemRestricted,
				WriteRoots: []WritableRoot{{Root: `C:\workspace`}},
				DenyRead:   []string{`C:\workspace\secret`},
			},
			Network: NetworkPolicy{Mode: NetworkDeny},
		},
		Env:     map[string]string{"ZERO_SANDBOXED": "1"},
		Command: []string{"cmd.exe", "/c", "dir"},
	}
	setup := WindowsSandboxSetupConfigFromCommand(command)
	if setup.SandboxHome != command.SandboxHome || setup.CommandCWD != command.CommandCWD || len(setup.WorkspaceRoots) != 1 || setup.WorkspaceRoots[0] != `C:\workspace` {
		t.Fatalf("setup config = %#v, want command roots", setup)
	}
	if setup.PermissionProfile.FileSystem.Kind != FileSystemRestricted || len(setup.PermissionProfile.FileSystem.DenyRead) != 1 {
		t.Fatalf("setup profile = %#v, want command permission profile", setup.PermissionProfile)
	}
}

func TestWindowsACLPlanHashIsStableAcrossEntryOrder(t *testing.T) {
	left, err := WindowsACLPlanHash(WindowsACLPlan{Entries: []WindowsACLEntry{
		{Action: WindowsACLDenyRead, Path: `C:\workspace\secret`, Capability: "S-1-5-21-3", Materialize: true},
		{Action: WindowsACLAllowWrite, Path: `C:\workspace`, Capability: "S-1-5-21-1"},
		{Action: WindowsACLDenyWrite, Path: `C:\workspace\.git`, Capability: "S-1-5-21-1"},
	}})
	if err != nil {
		t.Fatalf("WindowsACLPlanHash left: %v", err)
	}
	right, err := WindowsACLPlanHash(WindowsACLPlan{Entries: []WindowsACLEntry{
		{Action: WindowsACLAllowWrite, Path: `c:/workspace`, Capability: "s-1-5-21-1"},
		{Action: WindowsACLDenyWrite, Path: `c:/workspace/.git`, Capability: "S-1-5-21-1"},
		{Action: WindowsACLDenyRead, Path: `c:/workspace/secret`, Capability: "S-1-5-21-3", Materialize: true},
	}})
	if err != nil {
		t.Fatalf("WindowsACLPlanHash right: %v", err)
	}
	if left != right {
		t.Fatalf("ACL plan hashes differ: %q vs %q", left, right)
	}
}
