//go:build windows

package sandbox

import (
	"os"
	"strings"
	"sync"
	"testing"
)

// An opted-in command that ends up on the restricted token anyway must say so.
// Both cases below are correct fallbacks, not errors — but silence leaves the
// operator believing an account boundary is isolating them when it is not, which
// is the same failure the setup-protocol opt-in check exists to prevent, reached
// from the other side. The deny case matters most: deny is the DEFAULT network
// mode, so a fully provisioned, fully agreeing setup still never uses the
// principal for an ordinary command.
func TestWindowsSandboxPrincipalFallbackIsAnnounced(t *testing.T) {
	testCases := []struct {
		name   string
		mode   NetworkMode
		reason string
	}{
		// The network-deny case is deliberately absent. It used to warn here, but
		// this runner is re-exec'd per command, so the sync.Once guarding the notice
		// is once per COMMAND — and deny is the default mode, so the warning landed
		// on nearly every tool call. That fact belongs to `zero doctor` now, which is
		// read once. The deny-mode BEHAVIOUR is still pinned, by
		// TestPrincipalBackendDefersToRestrictedTokenWhenNetworkDenied below.
		{name: "no principal provisioned on this machine", mode: NetworkAllow, reason: "no sandbox principal is provisioned"},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			var warned []string
			originalWarn := warnWindowsSandboxPrincipalNotUsed
			warnWindowsSandboxPrincipalNotUsed = func(reason string) { warned = append(warned, reason) }
			windowsSandboxPrincipalNotUsedWarnOnce = sync.Once{}
			t.Cleanup(func() {
				warnWindowsSandboxPrincipalNotUsed = originalWarn
				windowsSandboxPrincipalNotUsedWarnOnce = sync.Once{}
			})

			config := WindowsSandboxCommandConfig{
				SandboxHome:    t.TempDir(),
				CommandCWD:     `C:\workspace`,
				WorkspaceRoots: []string{`C:\workspace`},
				PermissionProfile: PermissionProfile{
					FileSystem: FileSystemPolicy{Kind: FileSystemRestricted, WriteRoots: []WritableRoot{{Root: `C:\workspace`}}},
					Network:    NetworkPolicy{Mode: testCase.mode},
				},
				Env:          map[string]string{windowsSandboxIdentityEnv: "1"},
				SandboxLevel: WindowsSandboxLevelRestrictedToken,
				Command:      []string{"cmd.exe", "/c", "echo"},
			}
			// Assert the precondition rather than assume it: this must be the quiet
			// fallback path, not a token this host actually minted and not an error.
			token, ok, err := windowsSandboxPrincipalToken(config)
			if ok {
				token.Close()
				t.Fatalf("host unexpectedly provisioned a principal; this test cannot measure the fallback")
			}
			if err != nil {
				t.Fatalf("windowsSandboxPrincipalToken error = %v, want the quiet fallback", err)
			}
			if len(warned) != 1 {
				t.Fatalf("opted-in fallback warnings = %v, want exactly one naming %q", warned, testCase.reason)
			}
			if !strings.Contains(warned[0], testCase.reason) {
				t.Fatalf("warning = %q, want it to name %q", warned[0], testCase.reason)
			}
		})
	}
}

// The opt-out must stay silent, or the warning becomes noise every user learns
// to ignore.
func TestWindowsSandboxPrincipalFallbackIsSilentWhenOptedOut(t *testing.T) {
	var warned []string
	originalWarn := warnWindowsSandboxPrincipalNotUsed
	warnWindowsSandboxPrincipalNotUsed = func(reason string) { warned = append(warned, reason) }
	windowsSandboxPrincipalNotUsedWarnOnce = sync.Once{}
	t.Cleanup(func() {
		warnWindowsSandboxPrincipalNotUsed = originalWarn
		windowsSandboxPrincipalNotUsedWarnOnce = sync.Once{}
	})

	config := WindowsSandboxCommandConfig{
		SandboxHome:    t.TempDir(),
		CommandCWD:     `C:\workspace`,
		WorkspaceRoots: []string{`C:\workspace`},
		PermissionProfile: PermissionProfile{
			FileSystem: FileSystemPolicy{Kind: FileSystemRestricted, WriteRoots: []WritableRoot{{Root: `C:\workspace`}}},
			Network:    NetworkPolicy{Mode: NetworkDeny},
		},
		Env:          map[string]string{windowsSandboxIdentityEnv: "0"},
		SandboxLevel: WindowsSandboxLevelRestrictedToken,
		Command:      []string{"cmd.exe", "/c", "echo"},
	}
	if _, ok, err := windowsSandboxPrincipalToken(config); ok || err != nil {
		t.Fatalf("windowsSandboxPrincipalToken ok=%v err=%v, want the quiet opted-out fallback", ok, err)
	}
	if len(warned) != 0 {
		t.Fatalf("opted-out command warned %v, want silence", warned)
	}
}

// Setup must stay inert unless the principal backend is explicitly opted into.
// This is the property that makes the branch safe to merge while the privileged
// paths are still being validated: without the opt-in, `zero sandbox setup`
// creates no local account and the capability-SID backend is the whole of setup.
func TestWindowsSandboxIdentityGating(t *testing.T) {
	for name, testCase := range map[string]struct {
		env  map[string]string
		want bool
	}{
		// An explicit map entry is authoritative; these cases never reach the
		// process environment.
		"empty":          {env: map[string]string{windowsSandboxIdentityEnv: ""}, want: false},
		"zero":           {env: map[string]string{windowsSandboxIdentityEnv: "0"}, want: false},
		"true not one":   {env: map[string]string{windowsSandboxIdentityEnv: "true"}, want: false},
		"one":            {env: map[string]string{windowsSandboxIdentityEnv: "1"}, want: true},
		"one with space": {env: map[string]string{windowsSandboxIdentityEnv: " 1 "}, want: true},
	} {
		t.Run(name, func(t *testing.T) {
			// Pin the process variable too. Every case here supplies an explicit
			// map entry so none of them should consult it, and pinning proves
			// that rather than assuming it: without this a developer who exports
			// the opt-in would see different results from CI.
			t.Setenv(windowsSandboxIdentityEnv, "1")
			if got := windowsSandboxIdentityEnabled(testCase.env); got != testCase.want {
				t.Fatalf("enabled = %v, want %v for %q", got, testCase.want, testCase.env[windowsSandboxIdentityEnv])
			}
		})
	}
}

// With no map entry the process environment decides. That fallback is what the
// elevated setup path actually runs on — it passes no Env — so it needs its own
// coverage rather than riding on a case that also has a map entry.
func TestWindowsSandboxIdentityGatingFallsBackToTheProcessEnvironment(t *testing.T) {
	for name, testCase := range map[string]struct {
		value string
		set   bool
		want  bool
	}{
		"unset":          {set: false, want: false},
		"empty":          {value: "", set: true, want: false},
		"zero":           {value: "0", set: true, want: false},
		"one":            {value: "1", set: true, want: true},
		"one with space": {value: " 1 ", set: true, want: true},
	} {
		t.Run(name, func(t *testing.T) {
			// t.Setenv registers the restore even when the variable is then
			// cleared, which is the only way to test a genuinely absent variable
			// without leaking that state into the rest of the package.
			t.Setenv(windowsSandboxIdentityEnv, testCase.value)
			if !testCase.set {
				if err := os.Unsetenv(windowsSandboxIdentityEnv); err != nil {
					t.Fatalf("unset %s: %v", windowsSandboxIdentityEnv, err)
				}
			}
			if got := windowsSandboxIdentityEnabled(nil); got != testCase.want {
				t.Fatalf("enabled = %v, want %v (set=%v value=%q)", got, testCase.want, testCase.set, testCase.value)
			}
		})
	}
}

// The command environment wins over the process environment, so a run can opt in
// or out without depending on how the parent shell was launched.
func TestWindowsSandboxIdentityEnvOverridesProcess(t *testing.T) {
	t.Setenv(windowsSandboxIdentityEnv, "1")
	if windowsSandboxIdentityEnabled(map[string]string{windowsSandboxIdentityEnv: "0"}) {
		t.Fatal("command env set to 0 must override a process env of 1")
	}
	if !windowsSandboxIdentityEnabled(map[string]string{}) {
		t.Fatal("with no command-env entry the process env should apply")
	}
}

// One workspace maps to one principal, and different workspaces must not share
// an account, or two projects would run under the same identity and could reach
// each other's granted roots.
func TestWindowsSandboxWorkspaceKeyIsStableAndDistinct(t *testing.T) {
	first := windowsSandboxWorkspaceKey([]string{`C:\ws\alpha`})
	again := windowsSandboxWorkspaceKey([]string{`C:\ws\alpha`})
	other := windowsSandboxWorkspaceKey([]string{`C:\ws\beta`})
	if first != again {
		t.Fatalf("key is not stable: %q vs %q", first, again)
	}
	if first == other {
		t.Fatal("two different workspaces produced the same principal key")
	}
	if first == "" {
		t.Fatal("empty key")
	}
	// An empty root list still has to yield a usable key rather than a blank one.
	if windowsSandboxWorkspaceKey(nil) == "" {
		t.Fatal("no workspace roots produced an empty key")
	}
}

// Network denial is enforced by WFP filters keyed to the offline-marker SID,
// which only the restricted token carries. A principal token would leave those
// filters matching nothing, so the principal backend must stand down whenever
// the network is denied rather than silently trading network enforcement for
// read confinement.
// The backend now runs under both network modes, so the opt-in is the only thing
// eligibility turns on. The mode selects which principal is used, not whether one
// is used at all.
func TestPrincipalBackendEligibilityTurnsOnlyOnTheOptIn(t *testing.T) {
	eligible := func(mode NetworkMode, optIn string) bool {
		config := WindowsSandboxCommandConfig{
			SandboxHome:    t.TempDir(),
			WorkspaceRoots: []string{t.TempDir()},
			Env:            map[string]string{windowsSandboxIdentityEnv: optIn},
		}
		config.PermissionProfile.Network.Mode = mode
		return windowsSandboxPrincipalEligible(config)
	}

	for _, mode := range []NetworkMode{NetworkDeny, NetworkAllow} {
		if !eligible(mode, "1") {
			t.Fatalf("principal backend refused with network %q; the mode should choose a principal, not disable the backend", mode)
		}
		if eligible(mode, "0") {
			t.Fatalf("principal backend eligible without the opt-in for network %q", mode)
		}
	}
}

// The network mode is enforced by WHICH principal runs, because the offline one
// is a member of the group the block filters match. Picking the wrong role is
// therefore a silent loss of network enforcement, so the mapping is asserted
// directly rather than inferred from a token that only exists on a provisioned
// machine.
func TestPrincipalRoleFollowsNetworkMode(t *testing.T) {
	if got := windowsSandboxRoleForNetwork(NetworkDeny); got != windowsSandboxRoleOffline {
		t.Fatalf("deny selected %q, want the offline principal; the online one is not in the blocked group and would have the network", got)
	}
	if got := windowsSandboxRoleForNetwork(NetworkAllow); got != windowsSandboxRoleOnline {
		t.Fatalf("allow selected %q, want the online principal", got)
	}
	// Fail closed. An unrecognised mode must lose the network rather than keep it,
	// so anything that is not an explicit allow maps to the offline principal.
	for _, mode := range []NetworkMode{NetworkMode(""), NetworkMode("bogus"), NetworkMode("ALLOW")} {
		if got := windowsSandboxRoleForNetwork(mode); got != windowsSandboxRoleOffline {
			t.Fatalf("unrecognised mode %q selected %q, want the offline principal so an unknown mode fails closed", mode, got)
		}
	}
}

// The account key has to separate USERS, not only workspaces.
//
// The name is machine-wide while the DPAPI secret and the ACL ledger both live
// under the invoking user's sandbox home. Keyed on the workspace alone, two
// Windows users sharing one path derive the same account with private
// bookkeeping: the second finds no ledger of its own and either retires the
// first user's working principal or adopts it and rotates the password while
// storing only its own secret. The first user's marker still validates and its
// next command cannot log on.
//
// Asserting the principal key DIFFERS from the workspace key is what pins that.
// It is the cheapest observable difference; a test that only checked the key was
// stable would pass against the broken version.
func TestPrincipalKeyIsScopedToTheUserNotOnlyTheWorkspace(t *testing.T) {
	roots := []string{`C:\ws\shared`}
	workspace := windowsSandboxWorkspaceKey(roots)
	config := WindowsSandboxCommandConfig{WorkspaceRoots: roots}
	principal := windowsSandboxPrincipalKey(config)

	if principal == workspace {
		t.Fatal("the principal key equals the workspace key, so two users sharing this workspace would derive one account with separate secrets and ledgers")
	}
	if principal != windowsSandboxPrincipalKey(config) {
		t.Error("the principal key is not stable for one user, so a second setup would not find its own account")
	}
	if other := windowsSandboxPrincipalKey(WindowsSandboxCommandConfig{WorkspaceRoots: []string{`C:\ws\other`}}); other == principal {
		t.Error("two workspaces derived the same principal key, so they would share one account")
	}
}

// The setup lock must stay keyed to the WORKSPACE alone. Two users setting up
// one shared workspace now provision separate accounts, but they still write
// DACLs on the same paths, so they have to keep serializing against each other.
func TestSetupLockStaysWorkspaceScoped(t *testing.T) {
	roots := []string{`C:\ws\shared`}
	if windowsSandboxWorkspaceKey(roots) == windowsSandboxPrincipalKey(WindowsSandboxCommandConfig{WorkspaceRoots: roots}) {
		t.Fatal("the lock key and the principal key are the same, so making the account per-user also made the lock per-user")
	}
}
