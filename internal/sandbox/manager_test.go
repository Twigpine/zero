package sandbox

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/Gitlawb/zero/internal/oauth"
)

// TestCredentialPublicationDirSuffixMatchesStore keeps the duplicated suffix in
// step with the store that creates the directory: if they ever diverge, the
// profile denies a directory the store does not publish through, and the
// plaintext token passes through an unprotected path instead.
func TestCredentialPublicationDirSuffixMatchesStore(t *testing.T) {
	if credentialPublicationDirSuffix != oauth.PublicationDirSuffix {
		t.Fatalf("publication dir suffix = %q, want oauth.PublicationDirSuffix %q", credentialPublicationDirSuffix, oauth.PublicationDirSuffix)
	}
}

func TestPermissionProfileFromPolicyBuildsWorkspaceWriteProfile(t *testing.T) {
	workspace := t.TempDir()
	extra := t.TempDir()
	denyRead := filepath.Join(workspace, "private")
	denyWrite := filepath.Join(workspace, "readonly")
	if err := mkdirAll(denyRead, denyWrite); err != nil {
		t.Fatal(err)
	}
	scope, err := NewScope(workspace, []string{extra})
	if err != nil {
		t.Fatalf("NewScope: %v", err)
	}
	policy := DefaultPolicy()
	policy.DenyRead = []string{denyRead}
	policy.DenyWrite = []string{denyWrite}

	profile := PermissionProfileFromPolicy(workspace, policy, scope)
	if profile.FileSystem.Kind != FileSystemRestricted {
		t.Fatalf("filesystem kind = %q, want restricted", profile.FileSystem.Kind)
	}
	roots := scope.Roots()
	if len(profile.FileSystem.WriteRoots) != len(roots) {
		t.Fatalf("write roots = %#v, want scope roots %#v", profile.FileSystem.WriteRoots, roots)
	}
	for i, root := range roots {
		if profile.FileSystem.WriteRoots[i].Root != root {
			t.Fatalf("write roots = %#v, want scope roots %#v", profile.FileSystem.WriteRoots, roots)
		}
	}
	if !stringSliceContains(profile.FileSystem.ReadRoots, profileRootPath()) {
		t.Fatalf("read roots = %#v, want full read root %q", profile.FileSystem.ReadRoots, profileRootPath())
	}
	if !stringSliceContains(profile.FileSystem.WriteRoots[0].ProtectedMetadataNames, ".zero") || !stringSliceContains(profile.FileSystem.WriteRoots[0].ProtectedMetadataNames, ".agents") {
		t.Fatalf("protected metadata names = %#v, want workspace metadata protected", profile.FileSystem.WriteRoots[0].ProtectedMetadataNames)
	}
	resolvedRoot := profile.FileSystem.WriteRoots[0].Root
	wantGitCarveouts := []string{filepath.Join(resolvedRoot, ".git", "hooks"), filepath.Join(resolvedRoot, ".git", "config")}
	for _, want := range wantGitCarveouts {
		if !stringSliceContains(profile.FileSystem.WriteRoots[0].ReadOnlySubpaths, want) {
			t.Fatalf("read-only subpaths = %#v, want git metadata carveout %q", profile.FileSystem.WriteRoots[0].ReadOnlySubpaths, want)
		}
	}
	// DenyRead may also carry default credential-store entries when the host
	// has them, so assert containment rather than an exact count. Compare the
	// normalized (symlink-resolved) form the profile stores.
	if !stringSliceContains(profile.FileSystem.DenyRead, normalizeProfilePaths([]string{denyRead})[0]) || len(profile.FileSystem.DenyWrite) != 1 {
		t.Fatalf("deny paths = %#v / %#v, want configured entries present", profile.FileSystem.DenyRead, profile.FileSystem.DenyWrite)
	}
	if profile.Network.Mode != NetworkDeny {
		t.Fatalf("network profile = %#v, want deny", profile.Network)
	}
	if !profile.RequiresPlatformSandbox() {
		t.Fatal("workspace-write profile must require a platform sandbox")
	}
}

func TestPermissionProfileFromPolicyIncludesDefaultTempWriteRoots(t *testing.T) {
	tmpdir := t.TempDir()
	if runtime.GOOS == "windows" {
		t.Setenv("TEMP", tmpdir)
		t.Setenv("TMP", tmpdir)
	} else {
		t.Setenv("TMPDIR", tmpdir)
	}
	workspace := t.TempDir()

	profile := PermissionProfileFromPolicy(workspace, DefaultPolicy(), nil)
	if !writeRootsContain(profile.FileSystem.WriteRoots, workspace) {
		t.Fatalf("write roots = %#v, want workspace %q", profile.FileSystem.WriteRoots, workspace)
	}
	if !writeRootsContain(profile.FileSystem.WriteRoots, tmpdir) {
		t.Fatalf("write roots = %#v, want temp root %q", profile.FileSystem.WriteRoots, tmpdir)
	}
	// /tmp is a default temp write root on POSIX only (see
	// defaultTempWriteRootCandidatesForGOOS); on Windows the bare path resolves
	// against the current drive, so a stray C:\tmp must not turn this on.
	if runtime.GOOS != "windows" && pathExists("/tmp") && !writeRootsContain(profile.FileSystem.WriteRoots, "/tmp") {
		t.Fatalf("write roots = %#v, want /tmp", profile.FileSystem.WriteRoots)
	}
}

func writeRootsContain(roots []WritableRoot, want string) bool {
	want = normalizeProfilePath(want)
	for _, root := range roots {
		if normalizeProfilePath(root.Root) == want {
			return true
		}
	}
	return false
}

func TestUnknownNetworkModeFailsClosed(t *testing.T) {
	for _, mode := range []NetworkMode{"scoped", "proxy"} {
		if got := NormalizeNetworkMode(mode); got != NetworkDeny {
			t.Fatalf("NormalizeNetworkMode(%s) = %q, want %q", mode, got, NetworkDeny)
		}
	}
	profile := PermissionProfileFromPolicy(t.TempDir(), Policy{
		Mode:             ModeEnforce,
		Network:          NetworkMode("scoped"),
		EnforceWorkspace: true,
	}, nil)
	if profile.Network.Mode != NetworkDeny {
		t.Fatalf("unknown network mode profile = %#v, want deny", profile.Network)
	}
	if !shouldUnshareLinuxNetwork(NetworkPolicy{Mode: NetworkMode("scoped")}) {
		t.Fatal("unknown Linux network mode must unshare network")
	}
}

func TestPermissionProfileFromDisabledPolicyDoesNotRequirePlatformSandbox(t *testing.T) {
	policy := DefaultPolicy()
	policy.Mode = ModeDisabled
	profile := PermissionProfileFromPolicy(t.TempDir(), policy, nil)
	if profile.FileSystem.Kind != FileSystemUnrestricted || profile.Network.Mode != NetworkAllow {
		t.Fatalf("disabled profile = %#v, want unrestricted filesystem and allow network", profile)
	}
	if profile.RequiresPlatformSandbox() {
		t.Fatalf("disabled profile must not require platform sandbox: %#v", profile)
	}
}

func TestSandboxManagerBuildsExecutionRequestFromProfile(t *testing.T) {
	backend := Backend{Name: BackendLinuxBwrap, Available: true, Executable: "/usr/bin/zero-linux-sandbox", Platform: "linux"}
	policy := DefaultPolicy()
	profile := PermissionProfileFromPolicy("/workspace", policy, nil)
	request, err := NewSandboxManager(SandboxManagerOptions{GOOS: "linux", Backend: backend}).BuildExecutionRequest(SandboxManagerRequest{
		WorkspaceRoot:     "/workspace",
		Command:           CommandSpec{Name: "/bin/sh", Args: []string{"-c", "true"}, Dir: "/workspace"},
		Policy:            policy,
		Profile:           profile,
		Preference:        SandboxPreferenceAuto,
		ValidateExecution: true,
	})
	if err != nil {
		t.Fatalf("BuildExecutionRequest: %v", err)
	}
	if request.TargetBackend != BackendLinuxBwrap || !request.CommandWrapped || request.EnforcementLevel != EnforcementNative {
		t.Fatalf("execution request = %#v, want native linux-bwrap wrapping", request)
	}
	if request.PermissionProfile.FileSystem.Kind != FileSystemRestricted || !request.RequiresPlatformSandbox {
		t.Fatalf("execution request profile = %#v, requires=%t", request.PermissionProfile, request.RequiresPlatformSandbox)
	}
}

func TestSandboxManagerBuildsCommandPlanThroughLinuxHelper(t *testing.T) {
	backend := Backend{Name: BackendLinuxBwrap, Available: true, Executable: "/usr/bin/zero-linux-sandbox", Platform: "linux"}
	policy := DefaultPolicy()
	policy.BlockUnixSockets = true
	manager := NewSandboxManager(SandboxManagerOptions{GOOS: "linux", Backend: backend})
	plan, err := manager.BuildCommandPlan(SandboxManagerRequest{
		WorkspaceRoot:     "/workspace",
		Command:           CommandSpec{Name: "/bin/sh", Args: []string{"-c", "pwd"}, Dir: "/workspace/nested"},
		Policy:            policy,
		Profile:           PermissionProfileFromPolicy("/workspace", policy, nil),
		Preference:        SandboxPreferenceAuto,
		ValidateExecution: true,
	})
	if err != nil {
		t.Fatalf("BuildCommandPlan: %v", err)
	}
	if !plan.Wrapped || plan.Name != "/usr/bin/zero-linux-sandbox" || plan.TargetBackend != BackendLinuxBwrap {
		t.Fatalf("command plan = %#v, want native linux helper wrapper", plan)
	}
	if plan.EnforcementLevel != EnforcementNative {
		t.Fatalf("command metadata = %#v, want helper backend with native enforcement", plan)
	}
	assertArgsContainSequence(t, plan.Args, "--sandbox-policy-cwd", "/workspace")
	assertArgsContainSequence(t, plan.Args, "--command-cwd", "/workspace/nested")
	assertArgsContainSequence(t, plan.Args, "--block-unix-sockets")
	assertArgsContainSequence(t, plan.Args, "--", "/bin/sh", "-c", "pwd")
}

func TestSandboxManagerBuildsCommandPlanThroughWindowsRunner(t *testing.T) {
	// This exercises the native wrapped path, which requires the workspace to be
	// sandbox-initialized; stub the marker present (otherwise it degrades).
	restore := windowsSandboxInitialized
	t.Cleanup(func() { windowsSandboxInitialized = restore })
	windowsSandboxInitialized = func() bool { return true }
	backend := Backend{Name: BackendWindowsRestrictedToken, Available: true, Executable: `C:\zero\zero-windows-command-runner.exe`, Platform: "windows"}
	policy := DefaultPolicy()
	// Build the usual restricted FS profile, then clear DenyRead. On non-Windows
	// hosts PermissionProfileFromPolicy injects credential-store DenyRead paths,
	// and the Windows plan path rejects any non-empty DenyRead (PR #640). This
	// happy-path plan must exercise a valid restricted profile without DenyRead;
	// rejection coverage lives in TestSandboxManagerRejectsWindowsDenyReadOnBothRestrictedTokenTiers.
	profile := PermissionProfileFromPolicy(`C:\workspace`, policy, nil)
	profile.FileSystem.DenyRead = nil
	manager := NewSandboxManager(SandboxManagerOptions{GOOS: "windows", Backend: backend})
	plan, err := manager.BuildCommandPlan(SandboxManagerRequest{
		WorkspaceRoot:     `C:\workspace`,
		Command:           CommandSpec{Name: "cmd.exe", Args: []string{"/d", "/s", "/c", "dir"}, Dir: `C:\workspace\src`, Env: []string{"PATH=C:\\Tools", "TERM=xterm"}},
		Policy:            policy,
		Profile:           profile,
		Preference:        SandboxPreferenceAuto,
		ValidateExecution: true,
	})
	if err != nil {
		t.Fatalf("BuildCommandPlan: %v", err)
	}
	if !plan.Wrapped || plan.Name != `C:\zero\zero-windows-command-runner.exe` || plan.TargetBackend != BackendWindowsRestrictedToken {
		t.Fatalf("command plan = %#v, want native windows command runner wrapper", plan)
	}
	if plan.EnforcementLevel != EnforcementNative {
		t.Fatalf("command metadata = %#v, want native restricted-token backend", plan)
	}
	assertArgsContainSequence(t, plan.Args, "--command-cwd", `C:\workspace\src`)
	assertArgsContainSequence(t, plan.Args, "--sandbox-home")
	assertArgsContainSequence(t, plan.Args, "--windows-sandbox-level", string(WindowsSandboxLevelRestrictedToken))
	assertArgsContainSequence(t, plan.Args, "--workspace-root", `C:\workspace`)
	assertArgsContainSequence(t, plan.Args, "--", "cmd.exe", "/d", "/s", "/c", "dir")

	config, err := ParseWindowsSandboxCommandArgs(plan.Args)
	if err != nil {
		t.Fatalf("ParseWindowsSandboxCommandArgs: %v", err)
	}
	if config.SandboxHome == "" || config.CommandCWD != `C:\workspace\src` || len(config.WorkspaceRoots) != 1 || config.WorkspaceRoots[0] != `C:\workspace` {
		t.Fatalf("parsed roots = %#v cwd=%q, want workspace root and command cwd", config.WorkspaceRoots, config.CommandCWD)
	}
	if config.PermissionProfile.FileSystem.Kind != FileSystemRestricted || config.PermissionProfile.Network.Mode != NetworkDeny {
		t.Fatalf("parsed permission profile = %#v, want restricted deny profile", config.PermissionProfile)
	}
	if config.Env[EnvSandboxed] != "1" || config.Env[EnvSandboxBackend] != string(BackendWindowsRestrictedToken) || config.Env["COMSPEC"] == "" {
		t.Fatalf("parsed env = %#v, want sandbox markers and COMSPEC", config.Env)
	}
}

// TestSandboxManagerRejectsWindowsDenyReadOnBothRestrictedTokenTiers is the
// regression for PR #640: DenyRead cannot be launched or provisioned through
// either the elevated restricted-token path or the unelevated auto fallback.
// Both build the same fully restricted narrow-SID token.
func TestSandboxManagerRejectsWindowsDenyReadOnBothRestrictedTokenTiers(t *testing.T) {
	backend := Backend{Name: BackendWindowsRestrictedToken, Available: true, Executable: `C:\zero\zero-windows-command-runner.exe`, Platform: "windows"}
	manager := NewSandboxManager(SandboxManagerOptions{GOOS: "windows", Backend: backend})
	policy := DefaultPolicy()
	profile := PermissionProfile{
		FileSystem: FileSystemPolicy{
			Kind:       FileSystemRestricted,
			WriteRoots: []WritableRoot{{Root: `C:\workspace`}},
			DenyRead:   []string{`C:\workspace\secret`},
		},
		Network: NetworkPolicy{Mode: NetworkDeny},
	}
	cmd := CommandSpec{Name: "cmd.exe", Args: []string{"/c", "dir"}, Dir: `C:\workspace`}

	t.Run("elevated_restricted_token", func(t *testing.T) {
		restore := windowsSandboxInitialized
		t.Cleanup(func() { windowsSandboxInitialized = restore })
		windowsSandboxInitialized = func() bool { return true }

		_, err := manager.BuildCommandPlan(SandboxManagerRequest{
			WorkspaceRoot:     `C:\workspace`,
			Command:           cmd,
			Policy:            policy,
			Profile:           profile,
			Preference:        SandboxPreferenceAuto,
			ValidateExecution: true,
		})
		if err == nil {
			t.Fatal("expected BuildCommandPlan error for elevated restricted-token DenyRead")
		}
		msg := err.Error()
		for _, want := range []string{"DenyRead", "not supported", "restricted-token"} {
			if !strings.Contains(msg, want) {
				t.Fatalf("error %q missing %q", msg, want)
			}
		}
	})

	t.Run("unelevated_auto_fallback", func(t *testing.T) {
		restore := windowsSandboxInitialized
		t.Cleanup(func() { windowsSandboxInitialized = restore })
		windowsSandboxInitialized = func() bool { return false }

		req, err := manager.BuildExecutionRequest(SandboxManagerRequest{
			WorkspaceRoot:     `C:\workspace`,
			Command:           cmd,
			Policy:            policy,
			Profile:           profile,
			Preference:        SandboxPreferenceAuto,
			ValidateExecution: true,
		})
		if err != nil {
			t.Fatalf("BuildExecutionRequest: %v", err)
		}
		if req.EnforcementLevel != EnforcementUnelevated {
			t.Fatalf("EnforcementLevel = %v, want unelevated auto fallback before DenyRead rejection", req.EnforcementLevel)
		}
		_, err = manager.BuildCommandPlan(SandboxManagerRequest{
			WorkspaceRoot:     `C:\workspace`,
			Command:           cmd,
			Policy:            policy,
			Profile:           profile,
			Preference:        SandboxPreferenceAuto,
			ValidateExecution: true,
		})
		if err == nil {
			t.Fatal("expected BuildCommandPlan error for unelevated DenyRead")
		}
		if !strings.Contains(err.Error(), "DenyRead") || !strings.Contains(err.Error(), "not supported") {
			t.Fatalf("unelevated DenyRead error = %v", err)
		}
		if strings.Contains(err.Error(), "Use `--sandbox forbid`, the unelevated") {
			t.Fatalf("error still recommends unelevated as a workaround: %v", err)
		}
	})
}

func TestSandboxManagerDegradesUnavailableCommandPlan(t *testing.T) {
	policy := DefaultPolicy()
	backend := Backend{Name: BackendUnavailable, Platform: "windows", Fallback: true, Message: "native sandbox unavailable"}
	manager := NewSandboxManager(SandboxManagerOptions{GOOS: "windows", Backend: backend})
	plan, err := manager.BuildCommandPlan(SandboxManagerRequest{
		WorkspaceRoot:     `C:\workspace`,
		Command:           CommandSpec{Name: "cmd.exe", Args: []string{"/c", "dir"}, Dir: `C:\workspace`},
		Policy:            policy,
		Profile:           PermissionProfileFromPolicy(`C:\workspace`, policy, nil),
		Preference:        SandboxPreferenceAuto,
		ValidateExecution: true,
	})
	if err != nil {
		t.Fatalf("BuildCommandPlan: %v", err)
	}
	if plan.Wrapped || plan.EnforcementLevel != EnforcementDegraded || plan.DowngradeReason != "native sandbox unavailable" {
		t.Fatalf("plan = %#v, want degraded direct plan", plan)
	}
}

func TestSandboxManagerSelectsPlatformBackend(t *testing.T) {
	tests := []struct {
		name       string
		goos       string
		lookupName string
		lookupPath string
		setupPath  string
		want       BackendName
		wantTarget BackendName
	}{
		{name: "linux", goos: "linux", lookupName: LinuxSandboxHelperName, lookupPath: "/usr/bin/zero-linux-sandbox", want: BackendLinuxBwrap, wantTarget: BackendLinuxBwrap},
		{name: "macos", goos: "darwin", lookupName: "sandbox-exec", lookupPath: "/usr/bin/sandbox-exec", want: BackendMacOSSeatbelt, wantTarget: BackendMacOSSeatbelt},
		{name: "windows", goos: "windows", lookupName: WindowsSandboxCommandRunnerName, lookupPath: `C:\zero\zero-windows-command-runner.exe`, setupPath: `C:\zero\zero-windows-sandbox-setup.exe`, want: BackendWindowsRestrictedToken, wantTarget: BackendWindowsRestrictedToken},
		{name: "unsupported", goos: "plan9", want: BackendUnavailable, wantTarget: BackendUnavailable},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manager := NewSandboxManager(SandboxManagerOptions{
				GOOS: test.goos,
				LookupExecutable: func(name string) (string, error) {
					if name == test.lookupName && test.lookupPath != "" {
						return test.lookupPath, nil
					}
					if test.goos == "linux" && name == "bwrap" {
						return "/usr/bin/bwrap", nil
					}
					if name == WindowsSandboxSetupName && test.setupPath != "" {
						return test.setupPath, nil
					}
					return "", errors.New("missing")
				},
			})
			backend := manager.Backend()
			if backend.Name != test.want {
				t.Fatalf("backend = %#v, want %q", backend, test.want)
			}
			if backend.TargetBackend() != test.wantTarget {
				t.Fatalf("target backend = %q, want %q for %#v", backend.TargetBackend(), test.wantTarget, backend)
			}
		})
	}
}

func TestSandboxManagerInfersPlatformFromExplicitBackend(t *testing.T) {
	tests := []struct {
		name     string
		backend  BackendName
		wantGOOS string
	}{
		{name: "linux helper", backend: BackendLinuxBwrap, wantGOOS: "linux"},
		{name: "macos seatbelt", backend: BackendMacOSSeatbelt, wantGOOS: "darwin"},
		{name: "windows runner", backend: BackendWindowsRestrictedToken, wantGOOS: "windows"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manager := NewSandboxManager(SandboxManagerOptions{
				Backend: Backend{Name: test.backend, Available: true, Executable: "sandbox-helper"},
			})
			if manager.goos != test.wantGOOS || manager.backend.Platform != test.wantGOOS {
				t.Fatalf("manager = %#v, want platform/goos %q", manager, test.wantGOOS)
			}
		})
	}
}

func TestSelectBackendDelegatesToSandboxManagerSelection(t *testing.T) {
	backend := SelectBackend(BackendOptions{
		GOOS: "linux",
		LookupExecutable: func(name string) (string, error) {
			if name == LinuxSandboxHelperName {
				return "/usr/bin/zero-linux-sandbox", nil
			}
			if name == "bwrap" {
				return "/usr/bin/bwrap", nil
			}
			return "", errors.New("missing")
		},
	})
	managerBackend := NewSandboxManager(SandboxManagerOptions{
		GOOS: "linux",
		LookupExecutable: func(name string) (string, error) {
			if name == LinuxSandboxHelperName {
				return "/usr/bin/zero-linux-sandbox", nil
			}
			if name == "bwrap" {
				return "/usr/bin/bwrap", nil
			}
			return "", errors.New("missing")
		},
	}).Backend()
	if !reflect.DeepEqual(backend, managerBackend) {
		t.Fatalf("SelectBackend = %#v, manager backend = %#v", backend, managerBackend)
	}
}

func TestSandboxManagerFailsClosedWhenNativeRequiredAndUnavailable(t *testing.T) {
	policy := DefaultPolicy()
	profile := PermissionProfileFromPolicy("/workspace", policy, nil)
	_, err := NewSandboxManager(SandboxManagerOptions{
		GOOS:    "windows",
		Backend: Backend{Name: BackendUnavailable, Platform: "windows", Fallback: true},
	}).BuildExecutionRequest(SandboxManagerRequest{
		WorkspaceRoot:     "/workspace",
		Command:           CommandSpec{Name: "cmd.exe", Dir: "/workspace"},
		Policy:            policy,
		Profile:           profile,
		Preference:        SandboxPreferenceRequire,
		ValidateExecution: true,
	})
	if !errors.Is(err, errNativeSandboxUnavailable) {
		t.Fatalf("BuildExecutionRequest error = %v, want native sandbox unavailable", err)
	}
}

func mkdirAll(paths ...string) error {
	for _, path := range paths {
		if err := os.MkdirAll(path, 0o755); err != nil {
			return err
		}
	}
	return nil
}

func TestCredentialDenyReadPathsIn(t *testing.T) {
	home := t.TempDir()
	awsDir := filepath.Join(home, ".aws")
	gcloudDir := filepath.Join(home, ".config", "gcloud")
	npmrc := filepath.Join(home, ".npmrc")
	ghHosts := filepath.Join(home, ".config", "gh", "hosts.yml")
	netrc := filepath.Join(home, ".netrc")
	dockerConfig := filepath.Join(home, ".docker", "config.json")
	kubeConfig := filepath.Join(home, ".kube", "config")
	configDir := filepath.Join(home, ".config")
	zeroDir := filepath.Join(configDir, "zero")
	if err := mkdirAll(
		awsDir,
		gcloudDir,
		filepath.Dir(ghHosts),
		filepath.Dir(dockerConfig),
		filepath.Dir(kubeConfig),
		zeroDir,
	); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{npmrc, ghHosts, netrc, dockerConfig, kubeConfig} {
		if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	keyFile := filepath.Join(home, "sa-key.json")
	oauthDir := filepath.Join(home, "oauth-store")
	mcpDir := filepath.Join(home, "mcp-store")
	if err := mkdirAll(oauthDir, mcpDir); err != nil {
		t.Fatal(err)
	}
	oauthOverride := filepath.Join(oauthDir, "tokens.json")
	mcpOverride := filepath.Join(mcpDir, "tokens.json")
	overrideFiles := []string{
		oauthOverride,
		oauthOverride + ".tmp",
		oauthOverride + ".lockfile",
		oauthOverride + ".secret",
		oauthOverride + ".secret.tmp",
		oauthOverride + ".secret.lock",
		mcpOverride,
		mcpOverride + ".migrated",
	}
	// The migrated legacy MCP token backup and the atomic-write temp siblings
	// every store publishes before its rename; none of these are itemized by
	// name, so they only stay protected if the whole zeroDir is denied.
	zeroFiles := []string{
		filepath.Join(zeroDir, "config.json"),
		filepath.Join(zeroDir, "credentials.json"),
		filepath.Join(zeroDir, "credentials.enc"),
		filepath.Join(zeroDir, "credentials.enc.secret"),
		filepath.Join(zeroDir, "oauth-tokens.json"),
		filepath.Join(zeroDir, "oauth-tokens.json.secret"),
		filepath.Join(zeroDir, "mcp-oauth-tokens.json"),
		filepath.Join(zeroDir, "mcp-oauth-tokens.json.secret"),
		filepath.Join(zeroDir, "mcp-oauth-tokens.json.migrated"),
		filepath.Join(zeroDir, "oauth-tokens.json.tmp-1234-5678"),
		filepath.Join(zeroDir, "credentials.enc.9-1.tmp"),
		filepath.Join(zeroDir, ".zero-config-1.tmp"),
	}
	for _, path := range append(append([]string{keyFile}, overrideFiles...), zeroFiles...) {
		if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	options := credentialPathOptions{
		Homes:              []string{home},
		ConfigDirs:         []string{configDir},
		CloudSDKConfigDirs: []string{gcloudDir},
		GoogleCredentials:  []string{keyFile},
		NPMUserConfigs:     []string{npmrc},
		GHConfigDirs:       []string{filepath.Dir(ghHosts)},
		Netrcs:             []string{netrc},
		DockerConfigDirs:   []string{filepath.Dir(dockerConfig)},
		KubeConfigs:        []string{kubeConfig},
		OAuthTokens:        []string{oauthOverride},
		MCPOAuthTokens:     []string{mcpOverride},
	}
	credentials := credentialDenyReadPathsIn(options, nil)
	paths := credentials.Paths
	wantPaths := append([]string{
		awsDir,
		gcloudDir,
		keyFile,
		npmrc,
		ghHosts,
		netrc,
		dockerConfig,
		kubeConfig,
		zeroDir,
	}, overrideFiles...)
	for _, want := range normalizeProfilePaths(wantPaths) {
		if !stringSliceContains(paths, want) {
			t.Errorf("credential deny paths = %#v, want %q included", paths, want)
		}
	}
	// The store publishes through a per-store directory whose name is derived
	// from the store path, so it can be denied even though the random file name
	// inside it cannot be.
	for _, want := range normalizeProfilePaths([]string{oauthOverride + ".publish"}) {
		if !stringSliceContains(paths, want) {
			t.Errorf("credential deny paths = %#v, want publication directory %q included", paths, want)
		}
	}
	// Zero owns its config directory and the publication directories, so the
	// mount-based backend may create them to guarantee a mask exists.
	for _, want := range normalizeProfilePaths([]string{zeroDir, oauthOverride + ".publish"}) {
		if !stringSliceContains(credentials.EnsureDirs, want) {
			t.Errorf("credential ensure dirs = %#v, want %q", credentials.EnsureDirs, want)
		}
	}
	if stringSliceContains(credentials.EnsureDirs, normalizeProfilePath(mcpOverride+".publish")) {
		t.Errorf("credential ensure dirs = %#v, legacy MCP input must not create publication directories", credentials.EnsureDirs)
	}
	if stringSliceContains(credentials.EnsureDirs, normalizeProfilePaths([]string{awsDir})[0]) {
		t.Errorf("credential ensure dirs = %#v, must not create third-party stores", credentials.EnsureDirs)
	}
	// The user plugin/specialist/command roots live in the denied config
	// directory and are executed through the sandbox, so they stay readable.
	for _, name := range []string{"plugins", "specialists", "commands"} {
		want := filepath.Join(zeroDir, name)
		if !stringSliceContains(credentials.Carveouts, normalizeProfilePaths([]string{want})[0]) {
			t.Errorf("credential carveouts = %#v, want %q", credentials.Carveouts, want)
		}
	}
	// zeroFiles is covered by the zeroDir subpath deny above, not by an
	// itemized entry — including the never-enumerated migrated backup and
	// temp-write siblings.
	for _, zeroFile := range zeroFiles {
		if stringSliceContains(paths, normalizeProfilePaths([]string{zeroFile})[0]) {
			t.Errorf("credential deny paths = %#v, want itemized %q dropped in favor of the zeroDir subpath rule", paths, zeroFile)
		}
	}

	// A default candidate absent from disk at profile-build time is still
	// emitted so pathname-policy backends can reserve it. Mount-based Linux
	// retains the same profile baseline but can mask only paths that exist when
	// Bubblewrap assembles the namespace.
	if !stringSliceContains(paths, normalizeProfilePath(filepath.Join(home, ".azure"))) {
		t.Errorf("credential deny paths = %#v, want the not-yet-existing ~/.azure included", paths)
	}

	// An explicit AllowRead entry covering a store is an opt-out.
	optedOut := credentialDenyReadPathsIn(options, []string{awsDir, zeroDir})
	if stringSliceContains(optedOut.Paths, normalizeProfilePaths([]string{awsDir})[0]) {
		t.Errorf("credential deny paths = %#v, want AllowRead opt-out to drop ~/.aws", optedOut.Paths)
	}
	if stringSliceContains(optedOut.Paths, normalizeProfilePaths([]string{zeroDir})[0]) {
		t.Errorf("credential deny paths = %#v, want AllowRead opt-out to drop %q", optedOut.Paths, zeroDir)
	}
	if !stringSliceContains(optedOut.Paths, normalizeProfilePaths([]string{keyFile})[0]) {
		t.Errorf("credential deny paths = %#v, want unrelated entries kept after opt-out", optedOut.Paths)
	}
	// Nothing is created or carved out for a directory that is no longer denied.
	if stringSliceContains(optedOut.EnsureDirs, normalizeProfilePaths([]string{zeroDir})[0]) {
		t.Errorf("credential ensure dirs = %#v, want the opted-out config dir dropped", optedOut.EnsureDirs)
	}
	if stringSliceContains(optedOut.Carveouts, normalizeProfilePaths([]string{filepath.Join(zeroDir, "plugins")})[0]) {
		t.Errorf("credential carveouts = %#v, want no allow-back inside an opted-out deny", optedOut.Carveouts)
	}

	if got := credentialDenyReadPathsIn(credentialPathOptions{}, nil); len(got.Paths) != 0 {
		t.Errorf("credential deny paths for blank home = %#v, want none", got.Paths)
	}

	// The GOOGLE_APPLICATION_CREDENTIALS target stays protected even when no
	// home directory is resolvable.
	homeless := credentialDenyReadPathsIn(credentialPathOptions{GoogleCredentials: []string{keyFile}}, nil)
	if !stringSliceContains(homeless.Paths, normalizeProfilePaths([]string{keyFile})[0]) {
		t.Errorf("credential deny paths without home = %#v, want key file included", homeless.Paths)
	}
}

// TestCredentialDenyReadPathsInOverrideMatchesStoreResolution reproduces the
// audit finding that a relative-and-tilde ZERO_OAUTH_TOKENS_PATH /
// ZERO_MCP_OAUTH_TOKENS_PATH override produced a deny rule for a DIFFERENT
// path than the one the token stores actually resolve (oauth.ResolveStorePath
// / mcp.ResolveTokenStorePath never expand "~"; they resolve a relative
// override literally against the working directory), leaving the real file
// unprotected.
func TestCredentialPathOptionsResolveAgainstCommandDirectory(t *testing.T) {
	commandDir := t.TempDir()
	override := "~/relative-tilde-tokens.json"
	options := credentialPathOptionsFromEnvironment(credentialCommandBaseDirs(commandDir), []string{
		"HOME=",
		"USERPROFILE=" + filepath.Join(commandDir, "profile-home"),
		"XDG_CONFIG_HOME=~/literal-xdg",
		"ZERO_OAUTH_TOKENS_PATH=" + override,
		"ZERO_MCP_OAUTH_TOKENS_PATH=mcp/tokens.json",
	})
	paths := credentialDenyReadPathsIn(options, nil).Paths

	wantHome := filepath.Join(commandDir, "profile-home")
	if !stringSliceContains(options.Homes, wantHome) {
		t.Fatalf("homes = %#v, want USERPROFILE fallback %q", options.Homes, wantHome)
	}
	wantConfig := filepath.Join(commandDir, "~", "literal-xdg")
	if !stringSliceContains(options.ConfigDirs, wantConfig) {
		t.Fatalf("config dirs = %#v, want command-relative literal XDG path %q", options.ConfigDirs, wantConfig)
	}
	for _, want := range []string{
		filepath.Join(wantConfig, "zero"),
		filepath.Join(commandDir, override),
		filepath.Join(commandDir, override) + ".tmp",
		filepath.Join(commandDir, override) + ".lockfile",
		filepath.Join(commandDir, override) + ".secret.lock",
		filepath.Join(commandDir, "mcp", "tokens.json"),
		filepath.Join(commandDir, "mcp", "tokens.json.migrated"),
	} {
		if !stringSliceContains(paths, normalizeProfilePath(want)) {
			t.Errorf("credential deny paths = %#v, want command-relative root %q", paths, want)
		}
	}
}

func TestCredentialDenyReadPathsInConfigDirMatchesLiteralXDGResolution(t *testing.T) {
	configDir := "~/literal-xdg"
	commandDir := t.TempDir()
	resolvedConfigDirs := credentialPathOptionsFromEnvironment(credentialCommandBaseDirs(commandDir), []string{"XDG_CONFIG_HOME=" + configDir}).ConfigDirs
	paths := credentialDenyReadPathsIn(credentialPathOptions{ConfigDirs: resolvedConfigDirs}, nil).Paths

	want := filepath.Join(commandDir, configDir, "zero")
	if !stringSliceContains(resolvedConfigDirs, filepath.Dir(want)) {
		t.Fatalf("zero credential config dirs = %#v, want literal XDG resolution %q", resolvedConfigDirs, filepath.Dir(want))
	}
	if !stringSliceContains(paths, normalizeProfilePath(want)) {
		t.Fatalf("credential deny paths = %#v, want literal XDG resolution %q", paths, want)
	}
	if expanded := normalizeProfilePaths([]string{filepath.Join(configDir, "zero")})[0]; expanded != want && stringSliceContains(paths, expanded) {
		t.Fatalf("credential deny paths = %#v, must not use tilde-expanded XDG path %q", paths, expanded)
	}
}

func TestBuildCommandPlanUsesCommandCredentialContext(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows credential deny-read is tracked separately")
	}
	workspace := t.TempDir()
	commandDir := filepath.Join(workspace, "nested")
	if err := os.MkdirAll(commandDir, 0o755); err != nil {
		t.Fatal(err)
	}
	engine := NewEngine(EngineOptions{
		WorkspaceRoot: workspace,
		Policy:        DefaultPolicy(),
		Backend:       Backend{Name: BackendUnavailable, Platform: runtime.GOOS},
	})
	plan, err := engine.BuildCommandPlan(CommandSpec{
		Name: "true",
		Dir:  commandDir,
		Env: []string{
			"HOME=" + filepath.Join(workspace, "home"),
			"ZERO_OAUTH_TOKENS_PATH=credentials/tokens.json",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(plan.Dir, "credentials", "tokens.json")
	if stringSliceContains(plan.PermissionProfile.FileSystem.DenyReadIfExists, want) {
		t.Fatalf("DenyReadIfExists = %#v, command-relative override must not mask allowed workspace path %q", plan.PermissionProfile.FileSystem.DenyReadIfExists, want)
	}
	if stringSliceContains(plan.PermissionProfile.FileSystem.DenyReadIfExists, filepath.Dir(want)) {
		t.Fatalf("DenyReadIfExists = %#v, must not mask override parent %q", plan.PermissionProfile.FileSystem.DenyReadIfExists, filepath.Dir(want))
	}
}

// TestBuildCommandPlanDeniesRelativeOverrideAtProcessAndCommandDir covers the
// bypass a command-directory-only resolution left open: oauth.ResolveStorePath
// and mcp.ResolveTokenStorePath call filepath.Abs, so a relative override names
// a file under the ZERO PROCESS working directory, while a sandboxed command
// runs with its own cwd. Denying only the command-relative path left the real
// store readable under the read-all posture.
func TestBuildCommandPlanDeniesRelativeOverrideAtProcessAndCommandDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows credential deny-read is tracked separately")
	}
	workspace := resolvedTempDir(t)
	processDir := filepath.Join(workspace, "process")
	commandDir := filepath.Join(workspace, "process", "nested")
	if err := mkdirAll(commandDir); err != nil {
		t.Fatal(err)
	}
	originalDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(processDir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(originalDir) })
	// The process base dir is pinned at startup rather than re-read per plan, so
	// a test that moves the process has to move the pin with it.
	PinProcessCredentialBaseDir(t, processDir)
	t.Setenv("ZERO_OAUTH_TOKENS_PATH", "tokens.json")

	engine := NewEngine(EngineOptions{
		WorkspaceRoot: workspace,
		Policy:        DefaultPolicy(),
		Backend:       Backend{Name: BackendUnavailable, Platform: runtime.GOOS},
	})
	plan, err := engine.BuildCommandPlan(CommandSpec{
		Name: "true",
		Dir:  commandDir,
		Env:  []string{"HOME=" + filepath.Join(workspace, "home"), "ZERO_OAUTH_TOKENS_PATH=tokens.json"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// The store the Zero process actually writes.
	processStore := filepath.Join(processDir, "tokens.json")
	// The path a sandboxed child (e.g. a nested Zero) would resolve instead.
	commandStore := filepath.Join(commandDir, "tokens.json")
	if !stringSliceContains(plan.PermissionProfile.FileSystem.DenyReadIfExists, normalizeProfilePath(processStore)) {
		t.Fatalf("DenyReadIfExists = %#v, want process override resolved to %q", plan.PermissionProfile.FileSystem.DenyReadIfExists, processStore)
	}
	if stringSliceContains(plan.PermissionProfile.FileSystem.DenyReadIfExists, normalizeProfilePath(commandStore)) {
		t.Fatalf("DenyReadIfExists = %#v, command-relative override must not mask allowed workspace path %q", plan.PermissionProfile.FileSystem.DenyReadIfExists, commandStore)
	}
	// Neither resolution may mask the parent directory itself: that would hide the
	// workspace or a temp root from every sandboxed command.
	for _, unwanted := range []string{processDir, commandDir} {
		if stringSliceContains(plan.PermissionProfile.FileSystem.DenyReadIfExists, normalizeProfilePath(unwanted)) {
			t.Fatalf("DenyReadIfExists = %#v, must not mask override parent %q", plan.PermissionProfile.FileSystem.DenyReadIfExists, unwanted)
		}
	}
}

func TestCredentialCarveoutsUseNormalizedParentForMissingChildren(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation is not reliably available on Windows CI")
	}
	realConfig := filepath.Join(t.TempDir(), "real-config")
	if err := os.MkdirAll(realConfig, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "config-link")
	if err := os.Symlink(realConfig, alias); err != nil {
		t.Fatal(err)
	}

	credentials := credentialDenyReadPathsIn(credentialPathOptions{ConfigDirs: []string{alias}}, nil)
	wantZeroDir := normalizeProfilePath(filepath.Join(realConfig, "zero"))
	if !stringSliceContains(credentials.Paths, wantZeroDir) {
		t.Fatalf("credential deny paths = %#v, want canonical missing Zero dir %q", credentials.Paths, wantZeroDir)
	}
	if _, err := os.Stat(wantZeroDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("test requires the Zero dir to remain absent, got %v", err)
	}
	for _, name := range zeroConfigReadCarveoutNames {
		want := filepath.Join(wantZeroDir, name)
		if !stringSliceContains(credentials.Carveouts, want) {
			t.Errorf("credential carveouts = %#v, want lexical child of canonical root %q", credentials.Carveouts, want)
		}
	}
}

func TestCredentialCarveoutsRejectSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation is not reliably available on Windows CI")
	}
	configDir := t.TempDir()
	zeroDir := filepath.Join(configDir, "zero")
	if err := os.MkdirAll(zeroDir, 0o700); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(zeroDir, "oauth-tokens.json")
	if err := os.WriteFile(secret, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	pluginRoot := filepath.Join(zeroDir, "plugins")
	if err := os.Symlink(secret, pluginRoot); err != nil {
		t.Fatal(err)
	}

	credentials := credentialDenyReadPathsIn(credentialPathOptions{ConfigDirs: []string{configDir}}, nil)
	normalizedZeroDir := normalizeProfilePath(zeroDir)
	if stringSliceContains(credentials.Carveouts, filepath.Join(normalizedZeroDir, "plugins")) || stringSliceContains(credentials.Carveouts, normalizeProfilePath(secret)) {
		t.Fatalf("credential carveouts = %#v, must not re-allow a symlink or its credential target", credentials.Carveouts)
	}
	if want := filepath.Join(normalizedZeroDir, "specialists"); !stringSliceContains(credentials.Carveouts, want) {
		t.Fatalf("credential carveouts = %#v, want unrelated fixed subtree %q", credentials.Carveouts, want)
	}
}

func TestPermissionProfileUnionsProcessAndCommandCredentialRootsWithoutCreatingCommandDirs(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows credential deny-read is tracked separately")
	}
	parentHome := t.TempDir()
	parentConfig := filepath.Join(parentHome, "config")
	parentToken := filepath.Join(parentHome, "trusted-store", "tokens.json")
	t.Setenv("HOME", parentHome)
	t.Setenv("USERPROFILE", parentHome)
	t.Setenv("XDG_CONFIG_HOME", parentConfig)
	t.Setenv("CLOUDSDK_CONFIG", "")
	t.Setenv("ZERO_OAUTH_TOKENS_PATH", parentToken)
	t.Setenv("ZERO_MCP_OAUTH_TOKENS_PATH", "")
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "")
	t.Setenv("NPM_CONFIG_USERCONFIG", "")
	t.Setenv("npm_config_userconfig", "")
	t.Setenv("GH_CONFIG_DIR", "")
	t.Setenv("NETRC", "")
	t.Setenv("DOCKER_CONFIG", "")
	t.Setenv("KUBECONFIG", "")

	workspace := t.TempDir()
	childHome := filepath.Join(workspace, "child-home")
	childConfig := filepath.Join(childHome, "config")
	childToken := filepath.Join(workspace, "child-store", "tokens.json")
	engine := NewEngine(EngineOptions{
		WorkspaceRoot: workspace,
		Policy:        DefaultPolicy(),
		Backend:       Backend{Name: BackendUnavailable, Platform: runtime.GOOS},
	})
	plan, err := engine.BuildCommandPlan(CommandSpec{
		Name: "true",
		Dir:  workspace,
		Env: []string{
			"HOME=" + childHome,
			"XDG_CONFIG_HOME=" + childConfig,
			"ZERO_OAUTH_TOKENS_PATH=" + childToken,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	fs := plan.PermissionProfile.FileSystem
	parentZero := filepath.Join(parentConfig, "zero")
	childZero := filepath.Join(childConfig, "zero")
	childCredentialPaths := []string{
		filepath.Join(childHome, ".npmrc"),
		filepath.Join(childHome, ".netrc"),
		filepath.Join(childHome, ".docker", "config.json"),
		filepath.Join(childHome, ".kube", "config"),
		filepath.Join(childConfig, "gh", "hosts.yml"),
		filepath.Join(childConfig, "gcloud"),
	}
	for _, want := range []string{parentZero, parentToken} {
		if !stringSliceContains(fs.DenyReadIfExists, normalizeProfilePath(want)) {
			t.Errorf("DenyReadIfExists = %#v, want process root %q", fs.DenyReadIfExists, want)
		}
	}
	for _, unwanted := range append([]string{childZero, childToken}, childCredentialPaths...) {
		if stringSliceContains(fs.DenyReadIfExists, normalizeProfilePath(unwanted)) {
			t.Errorf("DenyReadIfExists = %#v, command credential setting masked allowed root %q", fs.DenyReadIfExists, unwanted)
		}
	}
	for _, want := range []string{parentZero, parentToken + credentialPublicationDirSuffix} {
		if !stringSliceContains(fs.EnsureDenyReadDirs, normalizeProfilePath(want)) {
			t.Errorf("EnsureDenyReadDirs = %#v, want trusted process dir %q", fs.EnsureDenyReadDirs, want)
		}
	}
	for _, unwanted := range []string{childZero, childToken + credentialPublicationDirSuffix} {
		if stringSliceContains(fs.EnsureDenyReadDirs, normalizeProfilePath(unwanted)) {
			t.Errorf("EnsureDenyReadDirs = %#v, must not include command-controlled dir %q", fs.EnsureDenyReadDirs, unwanted)
		}
	}
	if !stringSliceContains(fs.ProcessTrustedDenyReadFiles, normalizeProfilePath(parentToken)) {
		t.Fatalf("ProcessTrustedDenyReadFiles = %#v, want process override %q", fs.ProcessTrustedDenyReadFiles, parentToken)
	}
	if stringSliceContains(fs.ProcessTrustedDenyReadFiles, normalizeProfilePath(childToken)) {
		t.Fatalf("ProcessTrustedDenyReadFiles = %#v, must not trust command override %q", fs.ProcessTrustedDenyReadFiles, childToken)
	}

	config := LinuxSandboxHelperConfig{
		SandboxPolicyCWD:  workspace,
		CommandCWD:        workspace,
		PermissionProfile: plan.PermissionProfile,
		Command:           []string{"true"},
	}
	if _, err := BuildLinuxSandboxBwrapArgs(LinuxSandboxBwrapOptions{Config: config, HelperPath: "/usr/bin/zero-linux-sandbox"}); err == nil || !strings.Contains(err.Error(), "atomic replacement") {
		t.Fatalf("BuildLinuxSandboxBwrapArgs error = %v, want fail-closed atomic replacement error", err)
	}
	for _, unwanted := range append([]string{parentZero, parentToken + credentialPublicationDirSuffix, childZero, childToken + credentialPublicationDirSuffix}, childCredentialPaths...) {
		if _, err := os.Stat(unwanted); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("failed bwrap planning created credential path %q on the host: %v", unwanted, err)
		}
	}
}

// TestCommandSuppliedTokenOverrideFailsClosedOnBubblewrap covers jatmn's #681
// P1 finding on command-controlled overrides. A token store named only by the
// command's environment used to get the weakest possible treatment on Linux: an
// absent path was skipped outright, and an existing one got a /dev/null bind
// that the store's next atomic rename detaches. Neither is a deny. The path
// still earns none of the process-trusted privileges — no EnsureDenyReadDir, so
// a command cannot steer Zero into creating host directories — but bubblewrap
// now refuses the command instead of running it behind a mask that does not
// hold.
func TestCommandSuppliedTokenOverrideFailsClosedOnBubblewrap(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows credential deny-read is tracked separately")
	}
	home := t.TempDir()
	config := filepath.Join(home, "config")
	// Deliberately no process-level override: without one the process-trusted
	// list is empty and bubblewrap planning succeeds, so a failure below can only
	// come from the command-supplied path.
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", config)
	t.Setenv("ZERO_OAUTH_TOKENS_PATH", "")
	t.Setenv("ZERO_MCP_OAUTH_TOKENS_PATH", "")
	workspace := t.TempDir()

	baseline := permissionProfileFromPolicy(workspace, DefaultPolicy(), nil, workspace, nil)
	if len(baseline.FileSystem.ProcessTrustedDenyReadFiles) != 0 || len(baseline.FileSystem.CommandDenyReadFinalFiles) != 0 {
		t.Fatalf("baseline profile should have no replaceable final files: %#v", baseline.FileSystem)
	}
	if _, err := BuildLinuxSandboxBwrapArgs(LinuxSandboxBwrapOptions{
		Config: LinuxSandboxHelperConfig{
			SandboxPolicyCWD: workspace, CommandCWD: workspace,
			PermissionProfile: baseline, Command: []string{"true"},
		},
		HelperPath: "/usr/bin/zero-linux-sandbox",
	}); err != nil {
		t.Fatalf("baseline bwrap planning must succeed, got %v", err)
	}

	commandToken := filepath.Join(tempDirOutsideDefaultTemp(t), "command-store", "tokens.json")
	profile := permissionProfileFromPolicy(workspace, DefaultPolicy(), nil, workspace,
		[]string{"ZERO_OAUTH_TOKENS_PATH=" + commandToken})
	fs := profile.FileSystem
	if !stringSliceContains(fs.CommandDenyReadFinalFiles, normalizeProfilePath(commandToken)) {
		t.Fatalf("CommandDenyReadFinalFiles = %#v, want command override %q", fs.CommandDenyReadFinalFiles, commandToken)
	}
	if stringSliceContains(fs.ProcessTrustedDenyReadFiles, normalizeProfilePath(commandToken)) {
		t.Fatalf("ProcessTrustedDenyReadFiles = %#v, must not trust a command override", fs.ProcessTrustedDenyReadFiles)
	}
	if stringSliceContains(fs.EnsureDenyReadDirs, normalizeProfilePath(filepath.Dir(commandToken))) {
		t.Fatalf("EnsureDenyReadDirs = %#v, must not include a command-controlled dir", fs.EnsureDenyReadDirs)
	}

	_, err := BuildLinuxSandboxBwrapArgs(LinuxSandboxBwrapOptions{
		Config: LinuxSandboxHelperConfig{
			SandboxPolicyCWD: workspace, CommandCWD: workspace,
			PermissionProfile: profile, Command: []string{"true"},
		},
		HelperPath: "/usr/bin/zero-linux-sandbox",
	})
	if err == nil || !strings.Contains(err.Error(), "atomic replacement") {
		t.Fatalf("BuildLinuxSandboxBwrapArgs error = %v, want fail-closed atomic replacement error", err)
	}
	// Refusing must not have touched the host on the command's behalf.
	for _, unwanted := range []string{commandToken, filepath.Dir(commandToken), commandToken + credentialPublicationDirSuffix} {
		if _, statErr := os.Stat(unwanted); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("failed bwrap planning created command-controlled path %q: %v", unwanted, statErr)
		}
	}
}

func TestKeyringOAuthOverrideDoesNotFailClosedOnBubblewrap(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows credential deny-read is tracked separately")
	}
	home := t.TempDir()
	config := filepath.Join(home, "config")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", config)
	t.Setenv("ZERO_MCP_OAUTH_TOKENS_PATH", "")
	workspace := t.TempDir()
	tokenPath := filepath.Join(tempDirOutsideDefaultTemp(t), "tokens.json")

	t.Run("process environment", func(t *testing.T) {
		t.Setenv("ZERO_OAUTH_TOKENS_PATH", tokenPath)
		t.Setenv("ZERO_OAUTH_STORAGE", "keyring")
		profile := permissionProfileFromPolicy(workspace, DefaultPolicy(), nil, workspace, nil)
		fs := profile.FileSystem
		if !stringSliceContains(fs.DenyReadIfExists, normalizeProfilePath(tokenPath)) {
			t.Fatalf("DenyReadIfExists = %#v, want keyring override retained in ordinary deny baseline", fs.DenyReadIfExists)
		}
		if len(fs.ProcessTrustedDenyReadFiles) != 0 {
			t.Fatalf("ProcessTrustedDenyReadFiles = %#v, keyring does not publish token files", fs.ProcessTrustedDenyReadFiles)
		}
		for _, publicationDir := range credentialPublicationDirs(tokenPath) {
			if stringSliceContains(fs.DenyReadIfExists, normalizeProfilePath(publicationDir)) || stringSliceContains(fs.EnsureDenyReadDirs, normalizeProfilePath(publicationDir)) {
				t.Fatalf("keyring profile retained unused publication directory %q: %#v", publicationDir, fs)
			}
		}
		if err := validateLinuxBwrapPermissionProfile(profile); err != nil {
			t.Fatalf("keyring override must not make bubblewrap fail closed: %v", err)
		}
		_ = buildLinuxBwrapFilesystemPlan(profile)
		for _, publicationDir := range credentialPublicationDirs(tokenPath) {
			if _, err := os.Stat(publicationDir); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("keyring planning created unused publication directory %q: %v", publicationDir, err)
			}
		}
	})

	t.Run("command environment", func(t *testing.T) {
		t.Setenv("ZERO_OAUTH_TOKENS_PATH", "")
		t.Setenv("ZERO_OAUTH_STORAGE", "")
		profile := permissionProfileFromPolicy(workspace, DefaultPolicy(), nil, workspace, []string{
			"ZERO_OAUTH_TOKENS_PATH=" + tokenPath,
			"ZERO_OAUTH_STORAGE=keyring",
		})
		fs := profile.FileSystem
		if !stringSliceContains(fs.DenyReadIfExists, normalizeProfilePath(tokenPath)) {
			t.Fatalf("DenyReadIfExists = %#v, want command keyring override retained in ordinary deny baseline", fs.DenyReadIfExists)
		}
		if len(fs.CommandDenyReadFinalFiles) != 0 {
			t.Fatalf("CommandDenyReadFinalFiles = %#v, keyring does not publish token files", fs.CommandDenyReadFinalFiles)
		}
		for _, publicationDir := range credentialPublicationDirs(tokenPath) {
			if stringSliceContains(fs.DenyReadIfExists, normalizeProfilePath(publicationDir)) || stringSliceContains(fs.EnsureDenyReadDirs, normalizeProfilePath(publicationDir)) {
				t.Fatalf("command keyring profile retained unused publication directory %q: %#v", publicationDir, fs)
			}
		}
		if err := validateLinuxBwrapPermissionProfile(profile); err != nil {
			t.Fatalf("command keyring override must not make bubblewrap fail closed: %v", err)
		}
	})
}

func TestCommandCredentialDirectoriesFailClosedWithoutHostMutation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows credential deny-read is tracked separately")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("ZERO_OAUTH_TOKENS_PATH", "")
	t.Setenv("ZERO_MCP_OAUTH_TOKENS_PATH", "")
	workspace := t.TempDir()
	commandConfig := filepath.Join(tempDirOutsideDefaultTemp(t), "missing-command-config")
	commandZero := filepath.Join(commandConfig, "zero")
	profile := permissionProfileFromPolicy(workspace, DefaultPolicy(), nil, workspace, []string{
		"HOME=" + filepath.Dir(commandConfig),
		"XDG_CONFIG_HOME=" + commandConfig,
	})
	if !stringSliceContains(profile.FileSystem.CommandDenyReadDirs, normalizeProfilePath(commandZero)) {
		t.Fatalf("CommandDenyReadDirs = %#v, want future command credential root %q", profile.FileSystem.CommandDenyReadDirs, commandZero)
	}
	if err := validateLinuxBwrapPermissionProfile(profile); err == nil || !strings.Contains(err.Error(), "created after launch") {
		t.Fatalf("validateLinuxBwrapPermissionProfile error = %v, want future-directory failure", err)
	}
	_ = buildLinuxBwrapFilesystemPlan(profile)
	if _, err := os.Stat(commandConfig); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("planning created command-controlled directory %q: %v", commandConfig, err)
	}

	fileRoot := filepath.Join(tempDirOutsideDefaultTemp(t), "not-a-directory")
	if err := os.WriteFile(fileRoot, []byte("not a credential directory\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	profile.FileSystem.CommandDenyReadDirs = []string{fileRoot}
	if err := validateLinuxBwrapPermissionProfile(profile); err == nil || !strings.Contains(err.Error(), "created after launch") {
		t.Fatalf("regular-file command credential root error = %v, want fail closed", err)
	}

	existingRoot := tempDirOutsideDefaultTemp(t)
	profile.FileSystem.CommandDenyReadDirs = []string{existingRoot}
	profile.FileSystem.DenyReadIfExists = append(profile.FileSystem.DenyReadIfExists, existingRoot)
	if err := validateLinuxBwrapPermissionProfile(profile); err != nil {
		t.Fatalf("existing command credential directory must remain maskable: %v", err)
	}
	plan := buildLinuxBwrapFilesystemPlan(profile)
	if !strings.Contains(strings.Join(plan.Args, "\x00"), "--perms\x00000\x00--tmpfs\x00"+normalizeProfilePath(existingRoot)) {
		t.Fatalf("existing command credential directory was not masked: %#v", plan.Args)
	}
	if stringSliceContains(profile.FileSystem.EnsureDenyReadDirs, normalizeProfilePath(existingRoot)) {
		t.Fatalf("existing command credential directory gained host-creation privilege: %#v", profile.FileSystem.EnsureDenyReadDirs)
	}
}

func TestCommandCredentialSettingsDoNotMaskAllowedRoots(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows credential deny-read is tracked separately")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	workspace := t.TempDir()
	externalCloud := tempDirOutsideDefaultTemp(t)
	profile := permissionProfileFromPolicy(workspace, DefaultPolicy(), nil, workspace, []string{
		"CLOUDSDK_CONFIG=" + workspace,
		"GOOGLE_APPLICATION_CREDENTIALS=" + filepath.Join(externalCloud, "service-account.json"),
	})
	fs := profile.FileSystem
	if stringSliceContains(fs.DenyReadIfExists, normalizeProfilePath(workspace)) {
		t.Fatalf("command CLOUDSDK_CONFIG masked workspace: %#v", fs.DenyReadIfExists)
	}
	serviceAccount := normalizeProfilePath(filepath.Join(externalCloud, "service-account.json"))
	if !stringSliceContains(fs.DenyReadIfExists, serviceAccount) {
		t.Fatalf("non-overlapping credential file was dropped: %#v", fs.DenyReadIfExists)
	}
	bwrap := buildLinuxBwrapFilesystemPlan(profile)
	if strings.Contains(strings.Join(bwrap.Args, "\x00"), "--perms\x00000\x00--tmpfs\x00"+normalizeProfilePath(workspace)) {
		t.Fatalf("bwrap overlaid an unreadable mask on the workspace: %#v", bwrap.Args)
	}
	sbpl := seatbeltProfileFromPermissionProfile(profile, DefaultPolicy(), "")
	if strings.Contains(sbpl, `(deny file-read* (subpath "`+sandboxProfileString(normalizeProfilePath(workspace))+`"))`) {
		t.Fatalf("Seatbelt denied the command workspace:\n%s", sbpl)
	}
	if !strings.Contains(sbpl, sandboxProfileString(serviceAccount)) {
		t.Fatalf("Seatbelt dropped non-overlapping credential deny %q:\n%s", serviceAccount, sbpl)
	}
}

func TestKeyringExceptionKeepsFileBackedTokenStoresFailClosed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows credential deny-read is tracked separately")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	workspace := t.TempDir()

	tests := []struct {
		name          string
		processEnv    map[string]string
		commandEnv    []string
		wantTokenPath string
		commandMarker bool
	}{
		{
			name: "process encrypted OAuth store",
			processEnv: map[string]string{
				"ZERO_OAUTH_STORAGE":         "encrypted-file",
				"ZERO_OAUTH_TOKENS_PATH":     filepath.Join(t.TempDir(), "oauth.json"),
				"ZERO_MCP_OAUTH_TOKENS_PATH": "",
			},
		},
		{
			name: "command encrypted OAuth store",
			processEnv: map[string]string{
				"ZERO_OAUTH_STORAGE":         "",
				"ZERO_OAUTH_TOKENS_PATH":     "",
				"ZERO_MCP_OAUTH_TOKENS_PATH": "",
			},
			commandEnv: []string{
				"ZERO_OAUTH_STORAGE=encrypted-file",
				"ZERO_OAUTH_TOKENS_PATH=" + filepath.Join(tempDirOutsideDefaultTemp(t), "command-oauth.json"),
			},
			commandMarker: true,
		},
	}
	for i := range tests {
		test := &tests[i]
		if len(test.commandEnv) > 0 {
			_, test.wantTokenPath, _ = strings.Cut(test.commandEnv[len(test.commandEnv)-1], "=")
		} else if test.processEnv["ZERO_OAUTH_TOKENS_PATH"] != "" {
			test.wantTokenPath = test.processEnv["ZERO_OAUTH_TOKENS_PATH"]
		} else {
			test.wantTokenPath = test.processEnv["ZERO_MCP_OAUTH_TOKENS_PATH"]
		}
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			for key, value := range test.processEnv {
				t.Setenv(key, value)
			}
			profile := permissionProfileFromPolicy(workspace, DefaultPolicy(), nil, workspace, test.commandEnv)
			markers := profile.FileSystem.ProcessTrustedDenyReadFiles
			if test.commandMarker {
				markers = profile.FileSystem.CommandDenyReadFinalFiles
			}
			for _, want := range []string{test.wantTokenPath, test.wantTokenPath + ".secret"} {
				if !stringSliceContains(markers, normalizeProfilePath(want)) {
					t.Fatalf("final-file markers = %#v, want %q", markers, want)
				}
			}
			if err := validateLinuxBwrapPermissionProfile(profile); err == nil || !strings.Contains(err.Error(), "atomic replacement") {
				t.Fatalf("validateLinuxBwrapPermissionProfile error = %v, want atomic-replacement failure", err)
			}
		})
	}
}

func TestLegacyMCPOverrideDoesNotFailClosedOnBubblewrap(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows credential deny-read is tracked separately")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("ZERO_OAUTH_TOKENS_PATH", "")
	legacy := filepath.Join(tempDirOutsideDefaultTemp(t), "mcp-tokens.json")
	if err := os.WriteFile(legacy, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ZERO_MCP_OAUTH_TOKENS_PATH", "")
	workspace := t.TempDir()
	tests := []struct {
		name       string
		processMCP string
		commandEnv []string
	}{
		{name: "process override", processMCP: legacy},
		{name: "command override", commandEnv: []string{"ZERO_MCP_OAUTH_TOKENS_PATH=" + legacy}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("ZERO_MCP_OAUTH_TOKENS_PATH", test.processMCP)
			profile := permissionProfileFromPolicy(workspace, DefaultPolicy(), nil, workspace, test.commandEnv)
			fs := profile.FileSystem
			for _, want := range []string{legacy, legacy + ".migrated"} {
				if !stringSliceContains(fs.DenyReadIfExists, normalizeProfilePath(want)) {
					t.Fatalf("DenyReadIfExists = %#v, want legacy path %q", fs.DenyReadIfExists, want)
				}
			}
			if len(fs.ProcessTrustedDenyReadFiles) != 0 || len(fs.CommandDenyReadFinalFiles) != 0 {
				t.Fatalf("legacy source must not be an atomic writer: process=%#v command=%#v", fs.ProcessTrustedDenyReadFiles, fs.CommandDenyReadFinalFiles)
			}
			for _, unwanted := range []string{
				legacy + ".tmp", legacy + ".lockfile", legacy + ".secret",
				legacy + ".secret.tmp", legacy + ".secret.lock",
				legacy + ".publish", legacy + ".secret.publish",
			} {
				for field, paths := range map[string][]string{
					"DenyReadIfExists":    fs.DenyReadIfExists,
					"EnsureDenyReadDirs":  fs.EnsureDenyReadDirs,
					"CommandDenyReadDirs": fs.CommandDenyReadDirs,
				} {
					if stringSliceContains(paths, normalizeProfilePath(unwanted)) {
						t.Fatalf("%s = %#v, legacy source must not derive %q", field, paths, unwanted)
					}
				}
			}
			if err := validateLinuxBwrapPermissionProfile(profile); err != nil {
				t.Fatalf("legacy MCP migration input must not make Bubblewrap fail closed: %v", err)
			}
			args := buildLinuxBwrapFilesystemPlan(profile).Args
			foundMask := false
			for i := 0; i+2 < len(args); i++ {
				if args[i] == "--ro-bind" && args[i+1] == "/dev/null" && args[i+2] == normalizeProfilePath(legacy) {
					foundMask = true
					break
				}
			}
			if !foundMask {
				t.Fatalf("Bubblewrap plan does not exactly mask existing legacy source %q: %#v", legacy, args)
			}
		})
	}
}

func TestOAuthOverridesInsideCredentialCarveoutsRemainFailClosedOnBubblewrap(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows credential deny-read is tracked separately")
	}
	home := t.TempDir()
	configDir := filepath.Join(home, "config")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", configDir)
	t.Setenv("ZERO_MCP_OAUTH_TOKENS_PATH", "")
	workspace := t.TempDir()

	for _, name := range []string{"plugins", "specialists", "commands"} {
		t.Run(name, func(t *testing.T) {
			token := filepath.Join(configDir, "zero", name, "tokens.json")
			t.Setenv("ZERO_OAUTH_TOKENS_PATH", token)
			profile := permissionProfileFromPolicy(workspace, DefaultPolicy(), nil, workspace, nil)
			for _, want := range []string{token, token + ".secret"} {
				if !stringSliceContains(profile.FileSystem.ProcessTrustedDenyReadFiles, normalizeCredentialFinalPath(want)) {
					t.Fatalf("ProcessTrustedDenyReadFiles = %#v, carveout must retain final-file marker %q", profile.FileSystem.ProcessTrustedDenyReadFiles, want)
				}
			}
			if err := validateLinuxBwrapPermissionProfile(profile); err == nil || !strings.Contains(err.Error(), "atomic replacement") {
				t.Fatalf("validateLinuxBwrapPermissionProfile error = %v, want atomic-replacement failure", err)
			}
		})
	}
}

// TestProcessCredentialBaseDirDoesNotFollowWorkingDirectoryChanges covers
// jatmn's #681 finding on rolling Getwd: the token stores resolve a relative
// override once, when the store is opened, and write to that fixed path
// thereafter. Re-resolving it against the CURRENT working directory on every
// BuildCommandPlan would deny a path nothing writes while the real store stayed
// readable.
func TestProcessCredentialBaseDirDoesNotFollowWorkingDirectoryChanges(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows credential deny-read is tracked separately")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("ZERO_OAUTH_TOKENS_PATH", "relative-tokens.json")
	t.Setenv("ZERO_MCP_OAUTH_TOKENS_PATH", "")
	workspace := t.TempDir()

	before := permissionProfileFromPolicy(workspace, DefaultPolicy(), nil, "", nil).FileSystem.ProcessTrustedDenyReadFiles
	if len(before) == 0 {
		t.Fatal("a relative token override must produce a process-trusted deny")
	}
	t.Chdir(t.TempDir())
	after := permissionProfileFromPolicy(workspace, DefaultPolicy(), nil, "", nil).FileSystem.ProcessTrustedDenyReadFiles
	if !slices.Equal(before, after) {
		t.Fatalf("process-trusted denies followed the working directory: before %#v, after %#v", before, after)
	}
}

func TestCommandControlledConfigAncestorDoesNotSuppressProcessTrustedFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows credential deny-read is tracked separately")
	}
	home := t.TempDir()
	commandConfig := filepath.Join(t.TempDir(), "command-config")
	tokenPath := filepath.Join(commandConfig, "zero", "oauth-tokens.json")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("ZERO_OAUTH_TOKENS_PATH", tokenPath)
	t.Setenv("ZERO_MCP_OAUTH_TOKENS_PATH", "")

	profile := permissionProfileFromPolicy(t.TempDir(), DefaultPolicy(), nil, t.TempDir(), []string{"XDG_CONFIG_HOME=" + commandConfig})
	if !stringSliceContains(profile.FileSystem.ProcessTrustedDenyReadFiles, normalizeProfilePath(tokenPath)) {
		t.Fatalf("ProcessTrustedDenyReadFiles = %#v, command-controlled config ancestor must not suppress %q", profile.FileSystem.ProcessTrustedDenyReadFiles, tokenPath)
	}
}

func TestProcessTrustedFileMarkerRequiresStrictUserDenyAncestor(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows credential deny-read is tracked separately")
	}
	home := t.TempDir()
	tokenPath := filepath.Join(home, "stores", "oauth-tokens.json")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("ZERO_OAUTH_TOKENS_PATH", tokenPath)
	t.Setenv("ZERO_MCP_OAUTH_TOKENS_PATH", "")

	exactPolicy := DefaultPolicy()
	exactPolicy.DenyRead = []string{tokenPath}
	exact := PermissionProfileFromPolicy(t.TempDir(), exactPolicy, nil)
	if !stringSliceContains(exact.FileSystem.ProcessTrustedDenyReadFiles, normalizeProfilePath(tokenPath)) {
		t.Fatalf("exact user file deny removed fail-closed marker: %#v", exact.FileSystem.ProcessTrustedDenyReadFiles)
	}

	directoryPolicy := DefaultPolicy()
	directoryPolicy.DenyRead = []string{filepath.Dir(tokenPath)}
	directory := PermissionProfileFromPolicy(t.TempDir(), directoryPolicy, nil)
	if stringSliceContains(directory.FileSystem.ProcessTrustedDenyReadFiles, normalizeProfilePath(tokenPath)) {
		t.Fatalf("user directory deny did not remove covered marker: %#v", directory.FileSystem.ProcessTrustedDenyReadFiles)
	}
}

func TestProcessTrustedFinalPathsPreserveTerminalSymlinkNamesDespiteTargetAllowRead(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation is not reliably available on Windows CI")
	}
	home := t.TempDir()
	zeroDir := filepath.Join(home, ".config", "zero")
	if err := os.MkdirAll(zeroDir, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(zeroDir, "oauth-tokens.json")
	if err := os.WriteFile(target, []byte("tokens"), 0o600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "token-link")
	if err := os.Symlink(target, outside); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("ZERO_OAUTH_TOKENS_PATH", outside)
	t.Setenv("ZERO_MCP_OAUTH_TOKENS_PATH", "")
	policy := DefaultPolicy()
	policy.AllowRead = []string{target}

	profile := PermissionProfileFromPolicy(t.TempDir(), policy, nil)
	for _, want := range []string{normalizeCredentialFinalPath(outside), normalizeCredentialFinalPath(outside + ".secret")} {
		if !stringSliceContains(profile.FileSystem.DenyReadIfExists, want) {
			t.Errorf("DenyReadIfExists = %#v, want lexical final pathname %q", profile.FileSystem.DenyReadIfExists, want)
		}
		if !stringSliceContains(profile.FileSystem.ProcessTrustedDenyReadFiles, want) {
			t.Errorf("ProcessTrustedDenyReadFiles = %#v, want lexical final pathname %q", profile.FileSystem.ProcessTrustedDenyReadFiles, want)
		}
		sbpl := seatbeltProfileFromPermissionProfile(profile, Policy{Mode: ModeEnforce}, "")
		if !strings.Contains(sbpl, `(literal "`+sandboxProfileString(want)+`")`) {
			t.Errorf("Seatbelt profile missing lexical deny pathname %q:\n%s", want, sbpl)
		}
	}
}

func TestProcessTrustedExactAllowReadDoesNotIncludeSecret(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows credential deny-read is tracked separately")
	}
	home := t.TempDir()
	tokenPath := filepath.Join(t.TempDir(), "store", "oauth-tokens.json")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("ZERO_OAUTH_TOKENS_PATH", tokenPath)
	t.Setenv("ZERO_MCP_OAUTH_TOKENS_PATH", "")
	policy := DefaultPolicy()
	policy.AllowRead = []string{tokenPath}

	profile := PermissionProfileFromPolicy(t.TempDir(), policy, nil)
	primary := normalizeCredentialFinalPath(tokenPath)
	secret := normalizeCredentialFinalPath(tokenPath + ".secret")
	if stringSliceContains(profile.FileSystem.ProcessTrustedDenyReadFiles, primary) {
		t.Fatalf("ProcessTrustedDenyReadFiles = %#v, exact AllowRead must opt primary out", profile.FileSystem.ProcessTrustedDenyReadFiles)
	}
	if !stringSliceContains(profile.FileSystem.DenyReadIfExists, secret) || !stringSliceContains(profile.FileSystem.ProcessTrustedDenyReadFiles, secret) {
		t.Fatalf("exact primary AllowRead must retain secret deny and marker: denies=%#v markers=%#v", profile.FileSystem.DenyReadIfExists, profile.FileSystem.ProcessTrustedDenyReadFiles)
	}
	if err := validateLinuxBwrapPermissionProfile(profile); err == nil || !strings.Contains(err.Error(), secret) {
		t.Fatalf("validateLinuxBwrapPermissionProfile error = %v, want fail-closed secret marker", err)
	}
}

func TestProcessTrustedFileMarkerHonorsOutOfTreeAllowRead(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows credential deny-read is tracked separately")
	}
	home := t.TempDir()
	tokenPath := filepath.Join(t.TempDir(), "allowed", "oauth-tokens.json")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("ZERO_OAUTH_TOKENS_PATH", tokenPath)
	t.Setenv("ZERO_MCP_OAUTH_TOKENS_PATH", "")
	policy := DefaultPolicy()
	policy.AllowRead = []string{filepath.Dir(tokenPath)}

	profile := PermissionProfileFromPolicy(t.TempDir(), policy, nil)
	if stringSliceContains(profile.FileSystem.DenyReadIfExists, normalizeProfilePath(tokenPath)) {
		t.Fatalf("DenyReadIfExists = %#v, AllowRead must opt out of token deny", profile.FileSystem.DenyReadIfExists)
	}
	if len(profile.FileSystem.ProcessTrustedDenyReadFiles) != 0 {
		t.Fatalf("ProcessTrustedDenyReadFiles = %#v, AllowRead opt-out must not retain marker", profile.FileSystem.ProcessTrustedDenyReadFiles)
	}
}

func TestEngineBuildCommandPlanValidatesBwrapBeforeCreatingRuntime(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows credential deny-read is tracked separately")
	}
	home := t.TempDir()
	tokenPath := filepath.Join(home, "trusted-store", "tokens.json")
	runtimeCache := filepath.Join(t.TempDir(), "cache")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("ZERO_OAUTH_TOKENS_PATH", tokenPath)
	t.Setenv("ZERO_MCP_OAUTH_TOKENS_PATH", "")
	oldUserCacheDir := sandboxUserCacheDir
	sandboxUserCacheDir = func() (string, error) { return runtimeCache, nil }
	t.Cleanup(func() { sandboxUserCacheDir = oldUserCacheDir })

	workspace := t.TempDir()
	engine := NewEngine(EngineOptions{
		WorkspaceRoot: workspace,
		Policy:        DefaultPolicy(),
		Backend: Backend{
			Name: BackendLinuxBwrap, Available: true, Platform: "linux",
			Executable: "/usr/bin/zero-linux-sandbox",
		},
	})
	if _, err := engine.BuildCommandPlan(CommandSpec{Name: "true", Dir: workspace}); err == nil || !strings.Contains(err.Error(), "atomic replacement") {
		t.Fatalf("BuildCommandPlan error = %v, want fail-closed atomic replacement error", err)
	}
	for _, unwanted := range []string{runtimeCache, filepath.Join(home, ".config", "zero"), tokenPath + credentialPublicationDirSuffix} {
		if _, err := os.Stat(unwanted); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("failed main-process validation created path %q: %v", unwanted, err)
		}
	}
}

func TestPermissionProfileDropsAutomaticMasksCoveredByUserDeny(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows credential deny-read is tracked separately")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("ZERO_OAUTH_TOKENS_PATH", "")
	t.Setenv("ZERO_MCP_OAUTH_TOKENS_PATH", "")
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "")
	policy := DefaultPolicy()
	policy.DenyRead = []string{home}

	profile := PermissionProfileFromPolicy(t.TempDir(), policy, nil)
	fs := profile.FileSystem
	if len(fs.DenyReadIfExists) != 0 || len(fs.DenyReadCarveouts) != 0 || len(fs.EnsureDenyReadDirs) != 0 {
		t.Fatalf("automatic credential fields beneath user deny were retained: paths=%#v carveouts=%#v ensure=%#v", fs.DenyReadIfExists, fs.DenyReadCarveouts, fs.EnsureDenyReadDirs)
	}
	zeroDir := filepath.Join(home, ".config", "zero")
	plan := buildLinuxBwrapFilesystemPlan(profile)
	if stringSliceContains(plan.Args, zeroDir) {
		t.Fatalf("bwrap args contain a nested automatic mount beneath user deny %q: %#v", home, plan.Args)
	}
}

func TestPermissionProfileDropsRedundantAutomaticMasksInsideZeroDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows credential deny-read is tracked separately")
	}
	home := t.TempDir()
	configHome := filepath.Join(home, ".config")
	zeroDir := filepath.Join(configHome, "zero")
	tokenPath := filepath.Join(zeroDir, "oauth-tokens.json")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", configHome)
	t.Setenv("ZERO_OAUTH_TOKENS_PATH", tokenPath)
	t.Setenv("ZERO_MCP_OAUTH_TOKENS_PATH", "")
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "")

	profile := PermissionProfileFromPolicy(t.TempDir(), DefaultPolicy(), nil)
	fs := profile.FileSystem
	if !stringSliceContains(fs.DenyReadIfExists, normalizeProfilePath(zeroDir)) {
		t.Fatalf("DenyReadIfExists = %#v, want Zero directory mask", fs.DenyReadIfExists)
	}
	for _, path := range credentialTokenStorePaths(tokenPath) {
		if stringSliceContains(fs.DenyReadIfExists, normalizeProfilePath(path)) {
			t.Fatalf("DenyReadIfExists = %#v, redundant nested mask %q must be covered by Zero directory", fs.DenyReadIfExists, path)
		}
	}
	if stringSliceContains(fs.EnsureDenyReadDirs, normalizeProfilePath(tokenPath+credentialPublicationDirSuffix)) {
		t.Fatalf("EnsureDenyReadDirs = %#v, redundant nested publication dir must not be created", fs.EnsureDenyReadDirs)
	}
	if len(fs.ProcessTrustedDenyReadFiles) != 0 {
		t.Fatalf("ProcessTrustedDenyReadFiles = %#v, default config-directory mask must cover token path", fs.ProcessTrustedDenyReadFiles)
	}
}

func TestPermissionProfileIncludesZeroCredentialPaths(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows credential deny-read is tracked separately")
	}
	// Resolve the temp base up front so macOS /var -> /private/var does not
	// diverge between the pre-mkdir Clean fallback and the post-mkdir
	// EvalSymlinks success path inside normalizeProfilePath.
	configHome := resolvedTempDir(t)
	t.Setenv("XDG_CONFIG_HOME", configHome)
	zeroDir := filepath.Join(configHome, "zero")

	// Build the profile before the store directory exists to verify that profile
	// derivation retains the baseline candidate. Pathname-policy backends can
	// reserve it immediately; mount-based Linux applies it only if the path
	// exists when Bubblewrap assembles the namespace.
	profile := PermissionProfileFromPolicy(t.TempDir(), DefaultPolicy(), nil)
	want := normalizeProfilePaths([]string{zeroDir})[0]
	if !stringSliceContains(profile.FileSystem.DenyReadIfExists, want) {
		t.Fatalf("DenyReadIfExists = %#v, want Zero config directory %q even before it exists", profile.FileSystem.DenyReadIfExists, want)
	}

	if err := os.MkdirAll(zeroDir, 0o700); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(zeroDir, "oauth-tokens.json")
	if err := os.WriteFile(secret, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	migrated := filepath.Join(zeroDir, "mcp-oauth-tokens.json.migrated")
	if err := os.WriteFile(migrated, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Re-derive after the files exist: the same directory rule covers both
	// the known store filename and the never-itemized migrated backup.
	// Recompute want once the directory exists so EvalSymlinks can resolve
	// the full path (macOS would otherwise compare a pre-mkdir Clean path
	// against a post-mkdir /private/var form).
	profile = PermissionProfileFromPolicy(t.TempDir(), DefaultPolicy(), nil)
	want = normalizeProfilePaths([]string{zeroDir})[0]
	if !stringSliceContains(profile.FileSystem.DenyReadIfExists, want) {
		t.Fatalf("DenyReadIfExists = %#v, want Zero config directory %q", profile.FileSystem.DenyReadIfExists, want)
	}
}

func TestCredentialDenyReadPathsForEnvironmentHonorsConfigOverrides(t *testing.T) {
	root := t.TempDir()
	configHome := filepath.Join(root, "xdg")
	npmrc := filepath.Join(root, "npm", "userconfig")
	ghConfigDir := filepath.Join(root, "gh")
	cloudSDKConfig := filepath.Join(root, "gcloud")
	netrc := filepath.Join(root, "netrc")
	dockerConfigDir := filepath.Join(root, "docker")
	kubeConfigA := filepath.Join(root, "kube", "a")
	kubeConfigB := filepath.Join(root, "kube", "b")
	zeroConfig := filepath.Join(configHome, "zero")
	filePaths := []string{
		npmrc,
		filepath.Join(ghConfigDir, "hosts.yml"),
		netrc,
		filepath.Join(dockerConfigDir, "config.json"),
		kubeConfigA,
		kubeConfigB,
		zeroConfig,
	}
	if err := os.MkdirAll(cloudSDKConfig, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, path := range filePaths {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if path == zeroConfig {
			if err := os.MkdirAll(path, 0o755); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	options := credentialPathOptionsFromEnvironment([]string{root}, []string{
		"HOME=",
		"USERPROFILE=",
		"XDG_CONFIG_HOME=" + configHome,
		"CLOUDSDK_CONFIG=" + cloudSDKConfig,
		"NPM_CONFIG_USERCONFIG=" + npmrc,
		"GH_CONFIG_DIR=" + ghConfigDir,
		"NETRC=" + netrc,
		"DOCKER_CONFIG=" + dockerConfigDir,
		"KUBECONFIG=" + strings.Join([]string{kubeConfigA, kubeConfigB}, string(filepath.ListSeparator)),
	})
	got := credentialDenyReadPathsIn(options, nil).Paths
	wantPaths := append(filePaths, cloudSDKConfig)
	for _, want := range normalizeProfilePaths(wantPaths) {
		if !stringSliceContains(got, want) {
			t.Errorf("credential deny paths = %#v, want override %q included", got, want)
		}
	}
	if stringSliceContains(got, normalizeProfilePath(dockerConfigDir)) {
		t.Fatalf("credential deny paths = %#v, must deny Docker's credential file without hiding its config directory", got)
	}
}

func TestCredentialPathOptionsFromEnvironmentResolvesToolPathLists(t *testing.T) {
	processDir := t.TempDir()
	commandDir := t.TempDir()
	home := t.TempDir()
	absoluteKubeConfig := filepath.Join(t.TempDir(), "absolute-kubeconfig")
	options := credentialPathOptionsFromEnvironment([]string{processDir, commandDir}, []string{
		"HOME=" + home,
		"XDG_CONFIG_HOME=" + filepath.Join(home, ".config"),
		"CLOUDSDK_CONFIG=relative-gcloud",
		"NPM_CONFIG_USERCONFIG=upper-npmrc",
		"npm_config_userconfig=lower-npmrc",
		"GH_CONFIG_DIR=relative-gh",
		"NETRC=relative-netrc",
		"DOCKER_CONFIG=relative-docker",
		"KUBECONFIG=" + strings.Join([]string{"", "relative-kubeconfig", absoluteKubeConfig, ""}, string(filepath.ListSeparator)),
	})
	credentials := credentialDenyReadPathsIn(options, nil)

	for _, baseDir := range []string{processDir, commandDir} {
		for _, want := range []string{
			filepath.Join(baseDir, "upper-npmrc"),
			filepath.Join(baseDir, "relative-gh", "hosts.yml"),
			filepath.Join(baseDir, "relative-netrc"),
			filepath.Join(baseDir, "relative-docker", "config.json"),
			filepath.Join(baseDir, "relative-kubeconfig"),
			filepath.Join(baseDir, "relative-gcloud"),
		} {
			if !stringSliceContains(credentials.Paths, normalizeProfilePath(want)) {
				t.Errorf("credential deny paths = %#v, want resolved tool path %q", credentials.Paths, want)
			}
		}
		if unwanted := filepath.Join(baseDir, "lower-npmrc"); stringSliceContains(credentials.Paths, normalizeProfilePath(unwanted)) {
			t.Errorf("credential deny paths = %#v, lower-case npm override %q must lose to upper-case override", credentials.Paths, unwanted)
		}
		for _, unwantedDir := range []string{filepath.Join(baseDir, "relative-gh"), filepath.Join(baseDir, "relative-docker")} {
			if stringSliceContains(credentials.Paths, normalizeProfilePath(unwantedDir)) || stringSliceContains(credentials.Dirs, normalizeProfilePath(unwantedDir)) {
				t.Errorf("credential paths=%#v dirs=%#v, must protect a credential file without masking tool directory %q", credentials.Paths, credentials.Dirs, unwantedDir)
			}
		}
		cloudSDKDir := normalizeProfilePath(filepath.Join(baseDir, "relative-gcloud"))
		if !stringSliceContains(credentials.Dirs, cloudSDKDir) || stringSliceContains(credentials.EnsureDirs, cloudSDKDir) {
			t.Errorf("credential dirs=%#v ensure=%#v, want untrusted cloud config directory %q masked but never created", credentials.Dirs, credentials.EnsureDirs, cloudSDKDir)
		}
	}
	if !stringSliceContains(credentials.Paths, normalizeProfilePath(absoluteKubeConfig)) {
		t.Errorf("credential deny paths = %#v, want absolute KUBECONFIG entry %q", credentials.Paths, absoluteKubeConfig)
	}
}
