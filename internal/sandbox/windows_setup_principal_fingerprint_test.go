package sandbox

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func principalFingerprintConfig(sandboxHome string, readRoots []string) WindowsSandboxSetupConfig {
	workspace := filepath.FromSlash("/ws/project")
	return WindowsSandboxSetupConfig{
		SandboxHome:    sandboxHome,
		CommandCWD:     workspace,
		WorkspaceRoots: []string{workspace},
		PrincipalOptIn: true,
		PermissionProfile: PermissionProfile{
			FileSystem: FileSystemPolicy{
				Kind:       FileSystemRestricted,
				WriteRoots: []WritableRoot{{Root: workspace}},
				ReadRoots:  readRoots,
			},
		},
	}
}

// Narrowing a principal's read roots MUST invalidate setup.
//
// ACLPlanHash covers BuildWindowsACLPlan, the capability-SID plan. Principal
// grants are built separately by buildWindowsPrincipalACLPlan from the same
// profile, so before this the marker was blind to them: remove a read root and
// setup still looked current while the AllowRead ACE stayed on disk. Narrowing
// a policy has to be able to take access away.
func TestSetupMarkerInvalidatesWhenPrincipalReadRootsShrink(t *testing.T) {
	home := t.TempDir()
	wide, err := BuildWindowsSandboxSetupMarker(principalFingerprintConfig(home, []string{
		filepath.FromSlash("/ws/project"),
		filepath.FromSlash("/ws/extra-read"),
	}))
	if err != nil {
		t.Fatalf("build wide marker: %v", err)
	}
	narrow, err := BuildWindowsSandboxSetupMarker(principalFingerprintConfig(home, []string{
		filepath.FromSlash("/ws/project"),
	}))
	if err != nil {
		t.Fatalf("build narrow marker: %v", err)
	}

	if wide.PrincipalPlanHash == "" {
		t.Fatal("no principal fingerprint recorded while opted in, so principal grants are unfingerprinted")
	}
	if wide.PrincipalPlanHash == narrow.PrincipalPlanHash {
		t.Error("dropping a principal read root did not change the fingerprint, so stale AllowRead ACEs survive a narrowed policy")
	}
}

// A stale principal fingerprint must be refused through the real validator,
// which reads the marker off disk, with a message that says what to do.
func TestValidateRefusesAChangedPrincipalFingerprint(t *testing.T) {
	home := t.TempDir()
	config := principalFingerprintConfig(home, []string{filepath.FromSlash("/ws/project")})
	if _, err := WriteWindowsSandboxSetupMarker(config); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	if err := ValidateWindowsSandboxSetupMarker(config); err != nil {
		t.Fatalf("a freshly written marker did not validate, so this test cannot isolate the fingerprint: %v", err)
	}

	// Rewrite only the principal fingerprint, the way a policy change would.
	path := WindowsSandboxSetupMarkerPath(home)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read marker: %v", err)
	}
	var marker map[string]any
	if err := json.Unmarshal(raw, &marker); err != nil {
		t.Fatalf("parse marker: %v", err)
	}
	marker["principalPlanHash"] = "stale-hash-from-an-earlier-policy"
	rewritten, err := json.Marshal(marker)
	if err != nil {
		t.Fatalf("marshal marker: %v", err)
	}
	if err := os.WriteFile(path, rewritten, 0o600); err != nil {
		t.Fatalf("rewrite marker: %v", err)
	}

	err = ValidateWindowsSandboxSetupMarker(config)
	if err == nil {
		t.Fatal("a marker whose principal grants no longer match the policy was accepted")
	}
	if !strings.Contains(err.Error(), "principal grants changed") {
		t.Errorf("refused for the wrong reason: %v", err)
	}
}

// The default install must be untouched: opted out means no fingerprint, so the
// marker does not churn for the overwhelming majority of users.
func TestSetupMarkerHasNoPrincipalFingerprintWhenOptedOut(t *testing.T) {
	config := principalFingerprintConfig(t.TempDir(), []string{filepath.FromSlash("/ws/project")})
	config.PrincipalOptIn = false

	marker, err := BuildWindowsSandboxSetupMarker(config)
	if err != nil {
		t.Fatalf("build marker: %v", err)
	}
	if marker.PrincipalPlanHash != "" {
		t.Errorf("principal fingerprint %q recorded while opted out", marker.PrincipalPlanHash)
	}
}
