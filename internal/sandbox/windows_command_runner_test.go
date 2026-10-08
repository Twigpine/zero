package sandbox

import (
	"strings"
	"testing"
)

func TestWindowsDenyReadRestrictedTokenUnsupported(t *testing.T) {
	// Both restricted-token runner levels reject DenyRead before launch/setup.
	for _, level := range []WindowsSandboxLevel{
		WindowsSandboxLevelRestrictedToken,
		WindowsSandboxLevelUnelevated,
	} {
		err := windowsDenyReadRestrictedTokenUnsupported(WindowsSandboxCommandConfig{
			SandboxLevel: level,
			PermissionProfile: PermissionProfile{
				FileSystem: FileSystemPolicy{
					Kind:     FileSystemRestricted,
					DenyRead: []string{`C:\secret`, `D:\private`},
				},
			},
		})
		if err == nil {
			t.Fatalf("expected unsupported error for %s DenyRead profile", level)
		}
		msg := err.Error()
		for _, want := range []string{"DenyRead", "not supported", "restricted-token", "unelevated", "path count: 2"} {
			if !strings.Contains(msg, want) {
				t.Fatalf("%s error %q missing %q", level, msg, want)
			}
		}
		// DenyRead often names credential or private-file paths; keep them out of stderr.
		for _, secret := range []string{`C:\secret`, `D:\private`} {
			if strings.Contains(msg, secret) {
				t.Fatalf("%s error leaked DenyRead path %q: %q", level, secret, msg)
			}
		}
		if strings.Contains(msg, "--sandbox forbid") {
			t.Fatalf("%s error advertises unsupported --sandbox forbid recovery: %q", level, msg)
		}
		if strings.Contains(msg, "sandbox_permissions") || strings.Contains(msg, "require_escalated") {
			t.Fatalf("%s error advertises unusable escalation recovery: %q", level, msg)
		}
		if !strings.Contains(msg, "Remove DenyRead") {
			t.Fatalf("%s error should advise removing DenyRead: %q", level, msg)
		}
	}

	// No DenyRead: allowed (WRITE_RESTRICTED path can launch system tools).
	for _, level := range []WindowsSandboxLevel{
		WindowsSandboxLevelRestrictedToken,
		WindowsSandboxLevelUnelevated,
	} {
		if err := windowsDenyReadRestrictedTokenUnsupported(WindowsSandboxCommandConfig{
			SandboxLevel: level,
			PermissionProfile: PermissionProfile{
				FileSystem: FileSystemPolicy{Kind: FileSystemRestricted},
			},
		}); err != nil {
			t.Fatalf("unexpected error without DenyRead at %s: %v", level, err)
		}
	}
}
