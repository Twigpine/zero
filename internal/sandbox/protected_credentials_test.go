package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Gitlawb/zero/internal/remotetoken"
)

// protectedTokenFixture writes a bridge token inside the workspace and points
// ZERO_DAEMON_REMOTE_TOKEN_FILE at it.
func protectedTokenFixture(t *testing.T) (string, string) {
	t.Helper()
	ws, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	token := filepath.Join(ws, "bridge-token")
	if err := os.WriteFile(token, []byte("secret\n"), 0o600); err != nil {
		t.Fatalf("write token: %v", err)
	}
	t.Setenv(daemonRemoteTokenEnv, "")
	t.Setenv(daemonRemoteTokenFileEnv, token)
	return ws, token
}

func TestProtectedCredentialPathsResolveLikeTheDaemonReader(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	token := filepath.Join(base, "token")
	if err := os.WriteFile(token, []byte("secret\n"), 0o600); err != nil {
		t.Fatalf("write token: %v", err)
	}

	t.Run("absent variable protects nothing", func(t *testing.T) {
		t.Setenv(daemonRemoteTokenEnv, "")
		t.Setenv(daemonRemoteTokenFileEnv, "")
		if got := protectedCredentialPaths(); len(got) != 0 {
			t.Fatalf("protected paths = %#v, want none", got)
		}
	})

	t.Run("inline token leaves the unused file pointer unprotected", func(t *testing.T) {
		t.Setenv(daemonRemoteTokenEnv, "from-env")
		t.Setenv(daemonRemoteTokenFileEnv, token)
		if got := protectedCredentialPaths(); len(got) != 0 {
			t.Fatalf("protected paths = %#v, want none when the inline token takes precedence", got)
		}
	})

	t.Run("relative value resolves against the working directory", func(t *testing.T) {
		// os.ReadFile — what the daemon uses — resolves a relative value against the
		// working directory, so the protected path must do the same.
		t.Chdir(base)
		t.Setenv(daemonRemoteTokenEnv, "")
		t.Setenv(daemonRemoteTokenFileEnv, "token")
		if got := protectedCredentialPaths(); !stringSliceContains(got, token) {
			t.Fatalf("protected paths = %#v, want %q", got, token)
		}
	})

	t.Run("a literal tilde is not home-expanded", func(t *testing.T) {
		// os.ReadFile treats "~" as an ordinary directory name; expanding it here
		// would protect a path the daemon never reads.
		t.Chdir(base)
		t.Setenv(daemonRemoteTokenEnv, "")
		t.Setenv(daemonRemoteTokenFileEnv, filepath.Join("~", "token"))
		want := filepath.Join(base, "~", "token")
		got := protectedCredentialPaths()
		if !stringSliceContains(got, want) {
			t.Fatalf("protected paths = %#v, want literal %q", got, want)
		}
		home, err := os.UserHomeDir()
		if err == nil && stringSliceContains(got, filepath.Join(home, "token")) {
			t.Fatalf("protected paths = %#v, must not home-expand the value", got)
		}
	})

	t.Run("filename whitespace is part of the pathname", func(t *testing.T) {
		// The daemon reads exactly the bytes the variable names, so trimming the
		// pointer here would protect "<dir>/token" while the bearer file is
		// "<dir>/token " — leaving the real credential readable and replaceable.
		if runtime.GOOS == "windows" {
			t.Skip("Windows filenames cannot end in a space")
		}
		spaced := filepath.Join(base, "token ")
		if err := os.WriteFile(spaced, []byte("secret\n"), 0o600); err != nil {
			t.Fatalf("write token: %v", err)
		}
		t.Setenv(daemonRemoteTokenEnv, "")
		t.Setenv(daemonRemoteTokenFileEnv, spaced)
		got := protectedCredentialPaths()
		if !stringSliceContains(got, spaced) {
			t.Fatalf("protected paths = %#v, want %q", got, spaced)
		}
		if stringSliceContains(got, token) {
			t.Fatalf("protected paths = %#v, must not protect the trimmed name %q", got, token)
		}
	})

	t.Run("a symlinked pathname protects the link and its target", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("symlink creation needs elevation on Windows")
		}
		link := filepath.Join(base, "token-link")
		if err := os.Symlink(token, link); err != nil {
			t.Skipf("symlink unsupported: %v", err)
		}
		t.Setenv(daemonRemoteTokenEnv, "")
		t.Setenv(daemonRemoteTokenFileEnv, link)
		got := protectedCredentialPaths()
		for _, want := range []string{link, token} {
			if !stringSliceContains(got, want) {
				t.Fatalf("protected paths = %#v, want %q", got, want)
			}
		}
	})

	t.Run("startup source survives symlink retarget until restart", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("symlink creation needs elevation on Windows")
		}
		oldTarget := filepath.Join(base, "old-token")
		newTarget := filepath.Join(base, "new-token")
		for _, path := range []string{oldTarget, newTarget} {
			if err := os.WriteFile(path, []byte("secret\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		link := filepath.Join(base, "rotating-token")
		if err := os.Symlink(oldTarget, link); err != nil {
			t.Skipf("symlink unsupported: %v", err)
		}
		t.Setenv(daemonRemoteTokenEnv, "")
		t.Setenv(daemonRemoteTokenFileEnv, link)
		t.Setenv(daemonRemoteTokenFileResolvedEnv, oldTarget)
		if err := os.Remove(link); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(newTarget, link); err != nil {
			t.Fatal(err)
		}

		got := protectedCredentialPaths()
		for _, want := range []string{link, oldTarget} {
			if !stringSliceContains(got, want) {
				t.Fatalf("protected paths after retarget = %#v, want startup identity %q", got, want)
			}
		}
		if stringSliceContains(got, newTarget) {
			t.Fatalf("protected paths after retarget = %#v, current run must not silently re-authorize new target %q", got, newTarget)
		}

		t.Setenv(daemonRemoteTokenFileResolvedEnv, newTarget)
		if got := protectedCredentialPaths(); !stringSliceContains(got, newTarget) {
			t.Fatalf("protected paths after simulated restart = %#v, want new startup target %q", got, newTarget)
		}
	})
}

// TestProtectedCredentialsSurviveAllowRead locks in the non-opt-out guarantee:
// the bridge token grants control of the daemon, so neither AllowRead, an
// AllowWrite root, nor a granted permission may re-include it.
func TestProtectedCredentialsSurviveAllowRead(t *testing.T) {
	ws, token := protectedTokenFixture(t)
	policy := Policy{
		Mode:             ModeEnforce,
		EnforceWorkspace: true,
		AllowRead:        []string{ws, token},
		AllowWrite:       []string{ws},
	}
	scope, err := NewScope(ws, nil)
	if err != nil {
		t.Fatalf("NewScope: %v", err)
	}

	for _, sideEffect := range []SideEffect{SideEffectRead, SideEffectWrite, SideEffectOutOfWorkspace} {
		block := validatePathWithPolicy(scope, policy, sideEffect, true, ws, token)
		if block == nil || !strings.Contains(block.Reason, "remote bridge token") {
			t.Fatalf("%s on the bridge token: block = %#v, want a bridge-token deny", sideEffect, block)
		}
	}

	// The search-walk matcher enforces the same exclusion without consulting
	// AllowRead, and it is active even though DenyRead is empty.
	engine := NewEngine(EngineOptions{WorkspaceRoot: ws, Policy: policy, Scope: scope})
	rx := engine.ReadExclusions()
	if !rx.Active() {
		t.Fatal("read exclusions must be active for the automatic credential deny")
	}
	if !rx.PathExcluded(token) {
		t.Fatalf("read exclusions must exclude the bridge token %q", token)
	}
	if rx.PathExcluded(filepath.Join(ws, "main.go")) {
		t.Fatal("read exclusions must not exclude ordinary workspace files")
	}
	if globs := ReadExclusionGlobs(policy, scope); !stringSliceContains(globs, "!bridge-token") {
		t.Fatalf("read exclusion globs = %#v, want the bridge token excluded", globs)
	}
}

func TestProtectedCredentialDirExcluded(t *testing.T) {
	ws, token := protectedTokenFixture(t)
	engine := NewEngine(EngineOptions{
		WorkspaceRoot: ws,
		Policy:        Policy{Mode: ModeEnforce, EnforceWorkspace: true, AllowRead: []string{token}},
	})

	if !engine.ReadExclusions().DirExcluded(token) {
		t.Fatalf("DirExcluded must enforce the protected credential path %q", token)
	}
}

func TestProtectedCredentialPreventsUnsandboxedExecution(t *testing.T) {
	ws, _ := protectedTokenFixture(t)
	engine := NewEngine(EngineOptions{WorkspaceRoot: ws, Policy: DefaultPolicy()})

	if engine.UnsandboxedExecutionAllowed() {
		t.Fatal("UnsandboxedExecutionAllowed must stay false while a credential path is protected")
	}
}

// TestProtectedCredentialsRejectSessionPermissionProfile covers the other
// re-inclusion route: a session/turn permission profile that asks for the token
// path must not be auto-applicable.
func TestProtectedCredentialsRejectSessionPermissionProfile(t *testing.T) {
	ws, token := protectedTokenFixture(t)
	scope, err := NewScope(ws, nil)
	if err != nil {
		t.Fatalf("NewScope: %v", err)
	}
	engine := NewEngine(EngineOptions{
		WorkspaceRoot: ws,
		Policy:        Policy{Mode: ModeEnforce, EnforceWorkspace: true},
		Scope:         scope,
	})
	if engine.CoversRequestPermissions(RequestPermissionProfile{
		FileSystem: &FileSystemPermissions{Read: []string{token}},
	}) {
		t.Fatal("a permission request covering the bridge token must not read as already-granted")
	}
	if !engine.CoversRequestPermissions(RequestPermissionProfile{
		FileSystem: &FileSystemPermissions{Read: []string{filepath.Join(ws, "main.go")}},
	}) {
		t.Fatal("an ordinary workspace read request must stay covered by policy")
	}
}

// TestProtectedCredentialsDenyReadAndWriteInSeatbeltProfile covers the macOS
// backend: a token under a writable root was read-denied but still truncatable
// through the broad write allow. A user-configured DenyRead entry keeps the write
// direction (see TestSeatbeltProfileProtectsMetadataAndDenyOrdering).
func TestProtectedCredentialsDenyReadAndWriteInSeatbeltProfile(t *testing.T) {
	ws, token := protectedTokenFixture(t)
	userDenied := filepath.Join(ws, "generated")
	profile := PermissionProfile{
		FileSystem: FileSystemPolicy{
			Kind:       FileSystemRestricted,
			ReadRoots:  []string{string(filepath.Separator)},
			WriteRoots: []WritableRoot{{Root: ws}},
			DenyRead:   []string{token, userDenied},
			AllowTemp:  true,
		},
		Network: NetworkPolicy{Mode: NetworkDeny},
	}
	sbpl := seatbeltProfileFromPermissionProfile(profile, Policy{Mode: ModeEnforce, DenyRead: []string{userDenied}}, "")
	escaped := sandboxProfileString(normalizeProfilePath(token))
	denyRead := `(deny file-read* (literal "` + escaped + `"))`
	denyWrite := `(deny file-write* (literal "` + escaped + `"))`
	for _, want := range []string{denyRead, denyWrite} {
		if !strings.Contains(sbpl, want) {
			t.Fatalf("Seatbelt profile missing %q:\n%s", want, sbpl)
		}
	}
	if strings.Contains(sbpl, `(deny file-write* (literal "`+sandboxProfileString(normalizeProfilePath(userDenied))+`"))`) {
		t.Fatalf("a user-configured DenyRead path must stay writable:\n%s", sbpl)
	}
	// Seatbelt is last-match-wins, so the denial must follow the broad allow.
	if allow := strings.Index(sbpl, "(allow file-write*"); allow < 0 || strings.Index(sbpl, denyWrite) < allow {
		t.Fatalf("the write denial must follow the broad write allow:\n%s", sbpl)
	}
}

func TestMandatoryTokenKeepsExactSeatbeltDenialsUnderParentDenyRead(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Seatbelt is only available on macOS")
	}
	workspace, token := protectedTokenFixture(t)
	policy := DefaultPolicy()
	policy.DenyRead = []string{workspace}

	profile := PermissionProfileFromPolicy(workspace, policy, nil)
	if !stringSliceContains(profile.FileSystem.DenyReadIfExists, token) {
		t.Fatalf("DenyReadIfExists = %#v, want mandatory token %q despite parent DenyRead", profile.FileSystem.DenyReadIfExists, token)
	}
	if !stringSliceContains(profile.FileSystem.MandatoryDenyReadPaths, token) {
		t.Fatalf("MandatoryDenyReadPaths = %#v, want token %q", profile.FileSystem.MandatoryDenyReadPaths, token)
	}

	sbpl := seatbeltProfileFromPermissionProfile(profile, policy, "")
	escaped := sandboxProfileString(token)
	for _, want := range []string{
		`(deny file-read* (literal "` + escaped + `"))`,
		`(deny file-write* (literal "` + escaped + `"))`,
	} {
		if !strings.Contains(sbpl, want) {
			t.Fatalf("Seatbelt profile missing mandatory token rule %q:\n%s", want, sbpl)
		}
	}
}

func TestProtectedCredentialFilenameWhitespaceReachesOSSandbox(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows filenames cannot end in a space")
	}
	workspace, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	token := filepath.Join(workspace, "bridge-token ")
	if err := os.WriteFile(token, []byte("secret"), 0o600); err != nil {
		t.Fatalf("write token: %v", err)
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	t.Setenv("APPDATA", filepath.Join(home, "AppData", "Roaming"))
	t.Setenv("LOCALAPPDATA", filepath.Join(home, "AppData", "Local"))
	for _, key := range []string{"CLOUDSDK_CONFIG", "NPM_CONFIG_USERCONFIG", "npm_config_userconfig", "GH_CONFIG_DIR", "NETRC", "DOCKER_CONFIG", "KUBECONFIG", "GOOGLE_APPLICATION_CREDENTIALS", "ZERO_OAUTH_TOKENS_PATH", "ZERO_OAUTH_STORAGE", "ZERO_MCP_OAUTH_TOKENS_PATH"} {
		t.Setenv(key, "")
	}
	t.Setenv(daemonRemoteTokenEnv, "")
	t.Setenv(daemonRemoteTokenFileEnv, token)
	t.Setenv(daemonRemoteTokenFileResolvedEnv, "")
	t.Setenv(daemonRemoteTokenFileIdentityEnv, "")

	profile := PermissionProfileFromPolicy(workspace, DefaultPolicy(), nil)
	if !stringSliceContains(profile.FileSystem.DenyReadIfExists, token) {
		t.Fatalf("DenyReadIfExists = %#v, want exact token pathname %q", profile.FileSystem.DenyReadIfExists, token)
	}

	sbpl := seatbeltProfileFromPermissionProfile(profile, DefaultPolicy(), "")
	escaped := sandboxProfileString(token)
	for _, want := range []string{
		`(deny file-read* (literal "` + escaped + `"))`,
		`(deny file-write* (literal "` + escaped + `"))`,
	} {
		if !strings.Contains(sbpl, want) {
			t.Fatalf("Seatbelt profile missing %q:\n%s", want, sbpl)
		}
	}

	plan := mustBuildLinuxBwrapFilesystemPlan(t, profile)
	assertArgsContainSequence(t, plan.Args, "--ro-bind", "/dev/null", token)

}

// A mandatory token named by a symlink fails the plan outright: bubblewrap
// cannot bind-mask a mutable symlink destination, so masking the link's current
// target would be detached by a retarget mid-session.
func TestMandatoryLinuxTokenSymlinkFailsPlan(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs elevation on Windows")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "token")
	if err := os.WriteFile(target, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "token-link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	t.Setenv(daemonRemoteTokenEnv, "")
	t.Setenv(daemonRemoteTokenFileEnv, link)
	profile := PermissionProfileFromPolicy(dir, DefaultPolicy(), nil)

	_, err := buildLinuxBwrapFilesystemPlan(profile)
	if err == nil {
		t.Fatal("buildLinuxBwrapFilesystemPlan succeeded for a symlinked mandatory token, want a fail-closed error")
	}
	if !strings.Contains(err.Error(), "mandatory credential symlink") {
		t.Fatalf("buildLinuxBwrapFilesystemPlan error = %v, want a mandatory credential symlink refusal", err)
	}
}

// The helper-level refusal above must also hold at the command-plan level, so a
// future change cannot restore the unsafe accepted-symlink behavior while only a
// helper test stays green.
func TestSandboxManagerRefusesLinuxShellForSymlinkedToken(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs elevation on Windows")
	}
	workspace := t.TempDir()
	dir := t.TempDir()
	target := filepath.Join(dir, "token")
	if err := os.WriteFile(target, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "token-link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	t.Setenv(daemonRemoteTokenEnv, "")
	t.Setenv(daemonRemoteTokenFileEnv, link)

	policy := DefaultPolicy()
	backend := Backend{Name: BackendLinuxBwrap, Available: true, Executable: "/usr/bin/zero-linux-sandbox", Platform: "linux"}
	_, err := NewSandboxManager(SandboxManagerOptions{GOOS: "linux", Backend: backend}).BuildCommandPlan(SandboxManagerRequest{
		WorkspaceRoot:     workspace,
		Command:           CommandSpec{Name: "/bin/sh", Args: []string{"-c", "true"}, Dir: workspace},
		Policy:            policy,
		Profile:           PermissionProfileFromPolicy(workspace, policy, nil),
		Preference:        SandboxPreferenceAuto,
		ValidateExecution: true,
	})
	if err == nil {
		t.Fatal("BuildCommandPlan succeeded with a symlinked mandatory token, want the shell refused")
	}
	if !strings.Contains(err.Error(), "mandatory credential symlink") && !strings.Contains(err.Error(), "hard-link aliases") {
		t.Fatalf("BuildCommandPlan error = %v, want a fail-closed credential refusal", err)
	}
}

// A hard-link alias is a second directory entry for the token's inode, so the
// /dev/null bind over the configured pathname does not hide it. Plan
// construction must fail closed instead of running behind that mask.
func TestSandboxManagerRejectsLinuxTokenHardLinkAlias(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("hard-link inode probing is exercised on Linux")
	}
	workspace, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tokenDir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	token := filepath.Join(tokenDir, "bridge-token")
	if err := os.WriteFile(token, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(tokenDir, "token-alias")
	if err := os.Link(token, alias); err != nil {
		t.Skipf("fixture paths are not hard-linkable: %v", err)
	}

	t.Setenv(daemonRemoteTokenEnv, "")
	t.Setenv(daemonRemoteTokenFileEnv, token)

	if _, err := pathHardLinkCount(token); err != nil {
		t.Fatalf("pathHardLinkCount could not inspect the token fixture: %v", err)
	}
	if credential, linkable := protectedCredentialLinkableIntoLinuxShellRoot(
		PermissionProfileFromPolicy(workspace, DefaultPolicy(), nil),
		protectedCredentialPaths(),
	); !linkable || credential != token {
		t.Fatalf("linkable = %t credential = %q, want the aliased token %q reported", linkable, credential, token)
	}

	policy := DefaultPolicy()
	backend := Backend{Name: BackendLinuxBwrap, Available: true, Executable: "/usr/bin/zero-linux-sandbox", Platform: "linux"}
	_, err = NewSandboxManager(SandboxManagerOptions{GOOS: "linux", Backend: backend}).BuildCommandPlan(SandboxManagerRequest{
		WorkspaceRoot:     workspace,
		Command:           CommandSpec{Name: "/bin/sh", Args: []string{"-c", "cat " + alias}, Dir: workspace},
		Policy:            policy,
		Profile:           PermissionProfileFromPolicy(workspace, policy, nil),
		Preference:        SandboxPreferenceAuto,
		ValidateExecution: true,
	})
	if err == nil {
		t.Fatal("BuildCommandPlan succeeded with a hard-linked token alias, want plan construction to fail closed")
	}
	if !strings.Contains(err.Error(), "hard-link aliases") {
		t.Fatalf("BuildCommandPlan error = %v, want a hard-link alias refusal", err)
	}
}

// A read-only "/" root exposes the namespace, not an alias for the masked token
// inode. A token on a distinct filesystem is therefore the supported Linux
// placement; a writable root on the token filesystem remains unsafe.
func TestSandboxManagerAllowsLinuxTokenOnSeparateFilesystem(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux mount identity is exercised on Linux")
	}
	workspace := t.TempDir()
	tokenDir, err := os.MkdirTemp("/dev/shm", "zero-token-")
	if err != nil {
		t.Skipf("distinct tmpfs unavailable: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tokenDir) })
	if shared, known := pathsShareFilesystem(workspace, tokenDir); shared || !known {
		t.Skip("/dev/shm is not a verifiably distinct filesystem on this host")
	}
	token := filepath.Join(tokenDir, "bridge-token")
	if err := os.WriteFile(token, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(daemonRemoteTokenEnv, "")
	t.Setenv(daemonRemoteTokenFileEnv, token)

	policy := DefaultPolicy()
	profile := PermissionProfileFromPolicy(workspace, policy, nil)
	if credential, linkable := protectedCredentialLinkableIntoLinuxShellRoot(profile, protectedCredentialPaths()); linkable {
		t.Fatalf("separate-filesystem token %q reported linkable through %q", token, credential)
	}
	backend := Backend{Name: BackendLinuxBwrap, Available: true, Executable: "/usr/bin/zero-linux-sandbox", Platform: "linux"}
	if _, err := NewSandboxManager(SandboxManagerOptions{GOOS: "linux", Backend: backend}).BuildCommandPlan(SandboxManagerRequest{
		WorkspaceRoot:     workspace,
		Command:           CommandSpec{Name: "/bin/sh", Args: []string{"-c", "true"}, Dir: workspace},
		Policy:            policy,
		Profile:           profile,
		Preference:        SandboxPreferenceAuto,
		ValidateExecution: true,
	}); err != nil {
		t.Fatalf("BuildCommandPlan with separate-filesystem token: %v", err)
	}

	sameFilesystemToken := filepath.Join(t.TempDir(), "bridge-token")
	if err := os.WriteFile(sameFilesystemToken, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(daemonRemoteTokenFileEnv, sameFilesystemToken)
	if credential, linkable := protectedCredentialLinkableIntoLinuxShellRoot(profile, protectedCredentialPaths()); !linkable || credential != sameFilesystemToken {
		t.Fatalf("same-filesystem writable-root linkable = %t credential = %q, want %q", linkable, credential, sameFilesystemToken)
	}
	if _, err := NewSandboxManager(SandboxManagerOptions{GOOS: "linux", Backend: backend}).BuildCommandPlan(SandboxManagerRequest{
		WorkspaceRoot:     workspace,
		Command:           CommandSpec{Name: "/bin/sh", Args: []string{"-c", "true"}, Dir: workspace},
		Policy:            policy,
		Profile:           profile,
		Preference:        SandboxPreferenceAuto,
		ValidateExecution: true,
	}); err == nil || !strings.Contains(err.Error(), "hard-link aliases") {
		t.Fatalf("BuildCommandPlan same-filesystem error = %v, want hard-link alias refusal", err)
	}
}

// This admission test deliberately has no writable roots, so linkability does
// not reject the request before the startup-identity check is exercised.
func TestLinuxShellAdmissionRejectsRotatedStartupToken(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux shell admission requires native filesystem identity")
	}
	workspace := t.TempDir()
	dir := t.TempDir()
	token := filepath.Join(dir, "bridge-token")
	if err := os.WriteFile(token, []byte("startup-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(token)
	if err != nil {
		t.Fatal(err)
	}
	identity, ok := remotetoken.IdentityOfFile(file)
	file.Close()
	if !ok {
		t.Fatal("stable file identity unavailable")
	}
	t.Setenv(daemonRemoteTokenEnv, "")
	t.Setenv(daemonRemoteTokenFileEnv, token)
	t.Setenv(daemonRemoteTokenFileResolvedEnv, token)
	t.Setenv(daemonRemoteTokenFileIdentityEnv, identity)
	profile := PermissionProfile{
		FileSystem: FileSystemPolicy{Kind: FileSystemRestricted, ReadRoots: []string{string(filepath.Separator)}},
		Network:    NetworkPolicy{Mode: NetworkDeny},
	}
	manager := NewSandboxManager(SandboxManagerOptions{GOOS: "linux", Backend: Backend{Name: BackendLinuxBwrap, Available: true, Platform: "linux", NativeIsolation: true, CommandWrapping: true}})
	request := SandboxManagerRequest{WorkspaceRoot: workspace, Policy: DefaultPolicy(), Profile: profile, Preference: SandboxPreferenceAuto, ValidateExecution: true}
	if _, err := manager.BuildExecutionRequest(request); err != nil {
		t.Fatalf("unrotated startup token rejected: %v", err)
	}
	if err := os.Rename(token, token+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(token, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.BuildExecutionRequest(request); err == nil || !strings.Contains(err.Error(), "no longer names the token object") {
		t.Fatalf("rotated startup token error = %v, want identity-drift refusal", err)
	}
	if err := os.Remove(token); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.BuildExecutionRequest(request); err == nil || !strings.Contains(err.Error(), "no longer names the token object") {
		t.Fatalf("missing startup token error = %v, want identity-drift refusal", err)
	}
	if err := os.Rename(token+".old", token); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.BuildExecutionRequest(request); err != nil {
		t.Fatalf("restored startup token rejected: %v", err)
	}
}

// macOS intentionally has no file-token placement exception: Seatbelt cannot
// deny every inode alias across token rotation and daemon restart.
func TestSandboxManagerRejectsMacOSFileTokenRegardlessOfLinkLayout(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS file-token shell contract is exercised on darwin")
	}
	workspace := t.TempDir()
	tokenDir := t.TempDir()
	token := filepath.Join(tokenDir, "bridge-token")
	if err := os.WriteFile(token, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(daemonRemoteTokenEnv, "")
	t.Setenv(daemonRemoteTokenFileEnv, token)

	profile := PermissionProfile{
		FileSystem: FileSystemPolicy{
			Kind:      FileSystemRestricted,
			ReadRoots: []string{string(filepath.Separator)},
		},
		Network: NetworkPolicy{Mode: NetworkDeny},
	}
	policy := DefaultPolicy()
	backend := Backend{Name: BackendMacOSSeatbelt, Available: true, Executable: "/usr/bin/sandbox-exec", Platform: "darwin", CommandWrapping: true, NativeIsolation: true}
	_, err := NewSandboxManager(SandboxManagerOptions{GOOS: "darwin", Backend: backend}).BuildCommandPlan(SandboxManagerRequest{
		WorkspaceRoot:     workspace,
		Command:           CommandSpec{Name: "/bin/sh", Args: []string{"-c", "true"}, Dir: workspace},
		Policy:            policy,
		Profile:           profile,
		Preference:        SandboxPreferenceAuto,
		ValidateExecution: true,
	})
	if err == nil || !strings.Contains(err.Error(), "file-backed remote token") {
		t.Fatalf("BuildCommandPlan error = %v, want unconditional macOS file-token shell refusal", err)
	}
}

// Windows has no read-deny rule at all: credentialDenyReadPaths returns nothing
// there, so a wrapped shell would run with the bridge token readable under every
// pathname. The refusal is therefore unconditional, exactly like the macOS one
// above, and does not depend on the host actually being Windows — a Windows plan
// is buildable from any host.
func TestSandboxManagerRejectsWindowsFileBackedTokenShell(t *testing.T) {
	workspace := t.TempDir()
	tokenDir := t.TempDir()
	token := filepath.Join(tokenDir, "bridge-token")
	if err := os.WriteFile(token, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(daemonRemoteTokenEnv, "")
	t.Setenv(daemonRemoteTokenFileEnv, token)
	t.Setenv(daemonRemoteTokenFileResolvedEnv, "")

	policy := DefaultPolicy()
	backend := Backend{Name: BackendWindowsRestrictedToken, Available: true, Platform: "windows", CommandWrapping: true, NativeIsolation: true}
	manager := NewSandboxManager(SandboxManagerOptions{GOOS: "windows", Backend: backend})
	_, err := manager.BuildExecutionRequest(SandboxManagerRequest{
		WorkspaceRoot:     workspace,
		Command:           CommandSpec{Name: "cmd.exe", Args: []string{"/c", "type bridge-token"}, Dir: workspace},
		Policy:            policy,
		Profile:           PermissionProfileFromPolicy(workspace, policy, nil),
		Preference:        SandboxPreferenceAuto,
		ValidateExecution: true,
	})
	if err == nil || !strings.Contains(err.Error(), "file-backed remote token") {
		t.Fatalf("BuildExecutionRequest error = %v, want a Windows file-token shell refusal", err)
	}

	// Without a file-backed token the same request must still build: the refusal
	// is scoped to the credential, not to Windows.
	t.Setenv(daemonRemoteTokenFileEnv, "")
	if _, err := manager.BuildExecutionRequest(SandboxManagerRequest{
		WorkspaceRoot:     workspace,
		Command:           CommandSpec{Name: "cmd.exe", Args: []string{"/c", "echo hi"}, Dir: workspace},
		Policy:            policy,
		Profile:           PermissionProfileFromPolicy(workspace, policy, nil),
		Preference:        SandboxPreferenceAuto,
		ValidateExecution: true,
	}); err != nil {
		t.Fatalf("BuildExecutionRequest without a token file = %v, want success", err)
	}
}

// A write root the sandbox has not created yet still resolves to a filesystem:
// it lands on whichever one its parent is on. A root under the workspace is
// therefore a place a hard link to a same-filesystem token could be created, and
// the planner must refuse — while a root whose whole ancestor chain is missing
// (the macOS-only spellings the profile carries on every platform) must NOT be
// read as an unknown that refuses everything.
func TestLinuxTokenPlannerReadsUncreatedWriteRootFromItsParent(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux mount identity is exercised on Linux")
	}
	workspace, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tokenDir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	token := filepath.Join(tokenDir, "bridge-token")
	if err := os.WriteFile(token, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(daemonRemoteTokenEnv, "")
	t.Setenv(daemonRemoteTokenFileEnv, token)
	t.Setenv(daemonRemoteTokenFileResolvedEnv, "")

	profileWith := func(root string) PermissionProfile {
		return PermissionProfile{
			FileSystem: FileSystemPolicy{
				Kind:       FileSystemRestricted,
				ReadRoots:  []string{string(filepath.Separator)},
				WriteRoots: []WritableRoot{{Root: root}},
			},
			Network: NetworkPolicy{Mode: NetworkDeny},
		}
	}

	// Both temp dirs are on the same filesystem, so a root that will be created
	// under the workspace is a hard-link site for the token.
	missing := filepath.Join(workspace, "not-created-yet")
	if shared, known := pathsShareFilesystem(missing, token); !known || !shared {
		t.Skipf("workspace %q and token %q are not on one filesystem (shared=%t known=%t)", workspace, token, shared, known)
	}
	if credential, linkable := protectedCredentialLinkableIntoLinuxShellRoot(profileWith(missing), protectedCredentialPaths()); !linkable || credential != token {
		t.Fatalf("linkable = %t credential = %q, want %q refused behind a not-yet-created same-filesystem write root", linkable, credential, token)
	}

	// /private/... is a macOS spelling with no Linux counterpart, and the profile
	// carries it on every platform. Answering it from `/` keeps it a real answer
	// instead of an unknown that would refuse every file-backed token on Linux —
	// the regression that broke TestSandboxManagerAllowsLinuxTokenOnSeparateFilesystem.
	if _, err := os.Stat("/private"); err == nil {
		t.Skip("/private exists on this host, so it is not the platform-foreign case")
	}
	if _, known := pathsShareFilesystem("/private/tmp", token); !known {
		t.Fatal(`pathsShareFilesystem("/private/tmp", token) reported unknown, want the answer resolved from an existing ancestor`)
	}
}

// The optional (non-mandatory) credential candidates keep the older behavior:
// never bind over the link itself, and mask the resolved destination when it is
// the path that actually exists.
func TestOptionalLinuxCredentialSymlinkMasksResolvedDestination(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs elevation on Windows")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "token")
	if err := os.WriteFile(target, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "token-link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	resolvedTarget, err := filepath.EvalSymlinks(target)
	if err != nil {
		t.Fatalf("EvalSymlinks target: %v", err)
	}

	profile := PermissionProfile{
		FileSystem: FileSystemPolicy{
			Kind:             FileSystemRestricted,
			ReadRoots:        []string{dir},
			DenyReadIfExists: []string{link, resolvedTarget},
		},
	}
	plan := mustBuildLinuxBwrapFilesystemPlan(t, profile)
	if stringSliceContains(plan.Args, link) {
		t.Fatalf("bubblewrap plan attempted to mount over symlink destination %q: %#v", link, plan.Args)
	}
	assertArgsContainSequence(t, plan.Args, "--ro-bind", "/dev/null", resolvedTarget)
}

// TestProtectedCredentialsSurviveDisabledPolicy covers the one route that skips
// validatePathWithPolicy entirely: ModeDisabled drops every user-configured
// restriction, but the bridge token authenticates the caller driving these tools.
func TestProtectedCredentialsSurviveDisabledPolicy(t *testing.T) {
	ws, token := protectedTokenFixture(t)
	engine := NewEngine(EngineOptions{WorkspaceRoot: ws, Policy: Policy{Mode: ModeDisabled}})

	for _, sideEffect := range []SideEffect{SideEffectRead, SideEffectWrite} {
		decision := engine.Evaluate(context.Background(), Request{
			ToolName:      "read_file",
			WorkspaceRoot: ws,
			SideEffect:    sideEffect,
			Args:          map[string]any{"path": token},
		})
		if decision.Action != ActionDeny || !strings.Contains(decision.Reason, "remote bridge token") {
			t.Fatalf("%s under a disabled policy: action = %q reason = %q, want a bridge-token deny", sideEffect, decision.Action, decision.Reason)
		}
	}

	// Everything else stays allowed: a disabled sandbox is still disabled.
	decision := engine.Evaluate(context.Background(), Request{
		ToolName:      "read_file",
		WorkspaceRoot: ws,
		SideEffect:    SideEffectRead,
		Args:          map[string]any{"path": filepath.Join(ws, "main.go")},
	})
	if decision.Action != ActionAllow {
		t.Fatalf("ordinary read under a disabled policy: action = %q reason = %q, want allow", decision.Action, decision.Reason)
	}

	rx := engine.ReadExclusions()
	if !rx.Active() || !rx.PathExcluded(token) {
		t.Fatalf("read exclusions under a disabled policy must still exclude %q", token)
	}
	if rx.PathExcluded(filepath.Join(ws, "main.go")) {
		t.Fatal("read exclusions under a disabled policy must not exclude ordinary files")
	}
}

// TestDisabledPolicyLeavesShellOutsideTheTokenBoundary pins the boundary jatmn
// asked to see stated for #685: under ModeDisabled the bridge-token exclusion
// covers Zero's in-process file tools and nothing else. No OS wrapper is built
// at all in that mode, so a shell command is confined by nothing and an
// escalation has nothing to bypass. This test exists so that stops being an
// implicit property — changing any of it should mean changing this test on
// purpose, not discovering the behavior later.
func TestDisabledPolicyLeavesShellOutsideTheTokenBoundary(t *testing.T) {
	ws, token := protectedTokenFixture(t)
	engine := NewEngine(EngineOptions{WorkspaceRoot: ws, Policy: Policy{Mode: ModeDisabled}})

	shell := engine.Evaluate(context.Background(), Request{
		ToolName:      "bash",
		WorkspaceRoot: ws,
		SideEffect:    SideEffectShell,
		Args:          map[string]any{"command": "cat " + token},
	})
	if shell.Action != ActionAllow {
		t.Fatalf("shell under a disabled policy = %q (%s); the token boundary is documented as in-process only", shell.Action, shell.Reason)
	}

	// The same command's payload IS blocked when it arrives as a path-carrying
	// request, which is the whole of the guarantee.
	read := engine.Evaluate(context.Background(), Request{
		ToolName:      "read_file",
		WorkspaceRoot: ws,
		SideEffect:    SideEffectRead,
		Args:          map[string]any{"path": token},
	})
	if read.Action != ActionDeny {
		t.Fatalf("in-process read under a disabled policy = %q (%s), want deny", read.Action, read.Reason)
	}

	if !engine.UnsandboxedExecutionAllowed() {
		t.Fatal("escalation under a disabled policy must stay allowed: there is no wrapper for it to bypass")
	}

	// With the sandbox on, the same configured token flips both: the profile is
	// built, so escalating out of it would drop a real deny rule.
	enforcing := NewEngine(EngineOptions{WorkspaceRoot: ws, Policy: DefaultPolicy()})
	if enforcing.UnsandboxedExecutionAllowed() {
		t.Fatal("escalation must be refused while a bridge token is protected by an active profile")
	}
}

func TestProtectedCredentialsFollowFilesystemCaseSemantics(t *testing.T) {
	policy := Policy{Mode: ModeEnforce, EnforceWorkspace: true}

	for _, existing := range []bool{false, true} {
		name := "absent token pathname"
		if existing {
			name = "existing token file"
		}
		t.Run(name, func(t *testing.T) {
			ws, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatalf("EvalSymlinks: %v", err)
			}
			wantDenied := filesystemFoldsCaseForTest(t, ws)
			configured := filepath.Join(ws, "bridge-token")
			if existing {
				if err := os.WriteFile(configured, []byte("secret"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv(daemonRemoteTokenEnv, "")
			t.Setenv(daemonRemoteTokenFileEnv, configured)
			variant := filepath.Join(ws, strings.ToUpper(filepath.Base(configured)))
			scope, err := NewScope(ws, nil)
			if err != nil {
				t.Fatalf("NewScope: %v", err)
			}

			for _, sideEffect := range []SideEffect{SideEffectRead, SideEffectWrite, SideEffectOutOfWorkspace} {
				block := validatePathWithPolicy(scope, policy, sideEffect, true, ws, variant)
				denied := block != nil && strings.Contains(block.Reason, "remote bridge token")
				if denied != wantDenied {
					t.Fatalf("%s on case variant %q: denied = %t, want %t (block = %#v)", sideEffect, variant, denied, wantDenied, block)
				}
			}
			engine := NewEngine(EngineOptions{WorkspaceRoot: ws, Policy: policy, Scope: scope})
			if excluded := engine.ReadExclusions().PathExcluded(variant); excluded != wantDenied {
				t.Fatalf("read exclusions on case variant %q: excluded = %t, want %t", variant, excluded, wantDenied)
			}
			if block := validatePathWithPolicy(scope, policy, SideEffectWrite, true, ws, configured); block == nil || !strings.Contains(block.Reason, "remote bridge token") {
				t.Fatalf("the configured token %q must stay unwritable", configured)
			}
		})
	}
}

func filesystemFoldsCaseForTest(t *testing.T, dir string) bool {
	t.Helper()
	probe := filepath.Join(dir, "zero-case-probe")
	variant := filepath.Join(dir, "ZERO-CASE-PROBE")
	if err := os.WriteFile(probe, []byte("probe"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(probe) })
	probeInfo, err := os.Stat(probe)
	if err != nil {
		t.Fatal(err)
	}
	variantInfo, err := os.Stat(variant)
	if os.IsNotExist(err) {
		return false
	}
	if err != nil {
		t.Skipf("cannot determine filesystem case semantics: %v", err)
	}
	return os.SameFile(probeInfo, variantInfo)
}

func TestDetectPathCaseSemanticsUsesNearestExistingAncestor(t *testing.T) {
	backing := filepath.Join(t.TempDir(), "backing")
	if err := os.WriteFile(backing, []byte("probe"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(backing)
	if err != nil {
		t.Fatal(err)
	}
	parent := filepath.Join(t.TempDir(), "ProbeDir")
	configured := filepath.Join(parent, "missing", "bridge-token")
	variantName, ok := caseVariant(filepath.Base(parent))
	if !ok {
		t.Fatal("test parent has no case variant")
	}
	variantParent := filepath.Join(filepath.Dir(parent), variantName)

	statWithVariant := func(variantResult error) func(string) (os.FileInfo, error) {
		return func(path string) (os.FileInfo, error) {
			switch path {
			case parent:
				return info, nil
			case variantParent:
				if variantResult != nil {
					return nil, variantResult
				}
				return info, nil
			default:
				return nil, os.ErrNotExist
			}
		}
	}
	if got := detectPathCaseSemantics(configured, statWithVariant(nil)); got != pathCaseInsensitive {
		t.Fatalf("case-insensitive ancestor detection = %v, want %v", got, pathCaseInsensitive)
	}
	if got := detectPathCaseSemantics(configured, statWithVariant(os.ErrNotExist)); got != pathCaseSensitive {
		t.Fatalf("case-sensitive ancestor detection = %v, want %v", got, pathCaseSensitive)
	}
	if got := detectPathCaseSemantics(configured, statWithVariant(os.ErrPermission)); got != pathCaseUnknown {
		t.Fatalf("indeterminate ancestor detection = %v, want %v", got, pathCaseUnknown)
	}
}

// TestProtectedCredentialsDoNotBlockUnrelatedRequests keeps the exclusion inert
// for everyone who does not run the remote bridge.
func TestProtectedCredentialsDoNotBlockUnrelatedRequests(t *testing.T) {
	t.Setenv(daemonRemoteTokenEnv, "")
	t.Setenv(daemonRemoteTokenFileEnv, "")
	ws, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	engine := NewEngine(EngineOptions{
		WorkspaceRoot: ws,
		Policy:        Policy{Mode: ModeEnforce, EnforceWorkspace: true},
	})
	if rx := engine.ReadExclusions(); rx.Active() {
		t.Fatal("read exclusions must stay inactive without a configured token file")
	}
	decision := engine.Evaluate(context.Background(), Request{
		ToolName:      "read_file",
		WorkspaceRoot: ws,
		SideEffect:    SideEffectRead,
		Args:          map[string]any{"path": filepath.Join(ws, "main.go")},
	})
	if decision.Action == ActionDeny {
		t.Fatalf("ordinary workspace read was denied: %q", decision.Reason)
	}
}

func TestProtectedCredentialIdentitySurvivesConfiguredFileReplacement(t *testing.T) {
	dir := t.TempDir()
	token := filepath.Join(dir, "token")
	alias := filepath.Join(dir, "old-token-alias")
	if err := os.WriteFile(token, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(token, alias); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	startup, err := os.Open(token)
	if err != nil {
		t.Fatal(err)
	}
	identity, ok := remotetoken.IdentityOfFile(startup)
	startup.Close()
	if !ok {
		t.Fatal("stable file identity unavailable")
	}
	t.Setenv(daemonRemoteTokenEnv, "")
	t.Setenv(daemonRemoteTokenFileEnv, token)
	t.Setenv(daemonRemoteTokenFileResolvedEnv, token)
	t.Setenv(daemonRemoteTokenFileIdentityEnv, identity)
	replacement := filepath.Join(dir, "replacement")
	if err := os.WriteFile(replacement, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, token); err != nil {
		t.Fatal(err)
	}

	rx := ProtectedCredentialExclusions(dir)
	aliasFile, err := os.Open(alias)
	if err != nil {
		t.Fatal(err)
	}
	aliasInfo, err := aliasFile.Stat()
	if err != nil {
		aliasFile.Close()
		t.Fatal(err)
	}
	if !rx.FileHandleExcluded(alias, aliasFile, aliasInfo) {
		aliasFile.Close()
		t.Fatal("hard-link alias of startup token became readable after configured path replacement")
	}
	aliasFile.Close()
	if !rx.PathExcluded(token) {
		t.Fatal("configured token name was not reserved")
	}
}
