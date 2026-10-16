package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func TestBuildCommandPlanWrapsLinuxHelper(t *testing.T) {
	root := t.TempDir()
	resolvedRoot := resolvedTestPath(t, root)
	nested := filepath.Join(root, "nested")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	resolvedNested := resolvedTestPath(t, nested)
	engine := NewEngine(EngineOptions{
		WorkspaceRoot: root,
		Policy:        DefaultPolicy(),
		Backend: Backend{
			Name:       BackendLinuxBwrap,
			Available:  true,
			Executable: "/usr/bin/zero-linux-sandbox",
			Platform:   "linux",
			Message:    "Linux sandbox helper available",
		},
	})

	plan, err := engine.BuildCommandPlan(CommandSpec{
		Name: "/bin/sh",
		Args: []string{"-c", "pwd"},
		Dir:  nested,
	})
	if err != nil {
		t.Fatalf("BuildCommandPlan: %v", err)
	}

	if !plan.Wrapped || plan.Name != "/usr/bin/zero-linux-sandbox" || plan.Backend.Name != BackendLinuxBwrap {
		t.Fatalf("plan backend = %#v, want wrapped Linux helper", plan)
	}
	assertArgsContainSequence(t, plan.Args, "--sandbox-policy-cwd", resolvedRoot)
	assertArgsContainSequence(t, plan.Args, "--command-cwd", resolvedNested)
	assertArgsContainSequence(t, plan.Args, "--", "/bin/sh", "-c", "pwd")
	if plan.SandboxDir != resolvedNested {
		t.Fatalf("SandboxDir = %q, want command cwd", plan.SandboxDir)
	}
	if plan.Dir != resolvedNested {
		t.Fatalf("helper host Dir = %q, want command cwd", plan.Dir)
	}
}

func TestBuildCommandPlanWrapsSandboxExec(t *testing.T) {
	root := t.TempDir()
	resolvedRoot := resolvedTestPath(t, root)
	engine := NewEngine(EngineOptions{
		WorkspaceRoot: root,
		Policy:        DefaultPolicy(),
		Backend: Backend{
			Name:       BackendMacOSSeatbelt,
			Available:  true,
			Executable: "/usr/bin/sandbox-exec",
			Platform:   "darwin",
			Message:    "macOS Seatbelt backend available",
		},
	})

	plan, err := engine.BuildCommandPlan(CommandSpec{
		Name: "/bin/sh",
		Args: []string{"-c", "pwd"},
		Dir:  root,
	})
	if err != nil {
		t.Fatalf("BuildCommandPlan: %v", err)
	}

	if !plan.Wrapped || plan.Name != "/usr/bin/sandbox-exec" || plan.Backend.Name != BackendMacOSSeatbelt {
		t.Fatalf("plan backend = %#v, want wrapped macOS Seatbelt", plan)
	}
	if len(plan.Args) < 5 || plan.Args[0] != "-p" {
		t.Fatalf("sandbox-exec args = %#v, want profile and command", plan.Args)
	}
	profile := plan.Args[1]
	for _, want := range []string{
		"(deny default)",
		"(deny network*)",
		`(subpath "` + sandboxProfileString(resolvedRoot) + `")`,
		`(literal "/dev/null")`,
		`(subpath "/private/tmp")`,
	} {
		if !strings.Contains(profile, want) {
			t.Fatalf("profile missing %q:\n%s", want, profile)
		}
	}
	assertArgsContainSequence(t, plan.Args, "/bin/sh", "-c", "pwd")
	if plan.Dir != resolvedRoot || plan.SandboxDir != resolvedRoot {
		t.Fatalf("sandbox-exec dirs = host %q sandbox %q, want %q", plan.Dir, plan.SandboxDir, resolvedRoot)
	}
}

func TestBuildCommandPlanDegradesUnavailableFallback(t *testing.T) {
	root := t.TempDir()
	engine := NewEngine(EngineOptions{
		WorkspaceRoot: root,
		Policy:        DefaultPolicy(),
		Backend:       Backend{Name: BackendUnavailable, Message: "native sandbox unavailable"},
	})

	plan, err := engine.BuildCommandPlan(CommandSpec{
		Name: "/bin/sh",
		Args: []string{"-c", "pwd"},
		Dir:  root,
	})
	if err != nil {
		t.Fatalf("BuildCommandPlan: %v", err)
	}
	if plan.Wrapped || plan.EnforcementLevel != EnforcementDegraded || plan.DowngradeReason != "native sandbox unavailable" {
		t.Fatalf("plan = %#v, want degraded direct plan", plan)
	}
}

// TestBuildCommandPlanDegradedFallbackScrubsInheritedEnv covers the
// EnforcementDegraded fallback path: when the native backend is
// unavailable, BuildCommandPlan falls back to a direct (unwrapped) plan
// whose spec.Env is nil, so exec.Cmd would otherwise inherit the caller's
// environment — including configured and dynamically named credentials —
// unscrubbed.
func TestBuildCommandPlanDegradedFallbackScrubsInheritedEnv(t *testing.T) {
	t.Setenv("COMPANY_LLM_SECRET", "custom-secret")
	t.Setenv("ZERO_OAUTH_ACME_CLIENT_SECRET", "oauth-secret")
	t.Setenv("SAFE_VAR", "hello")

	root := t.TempDir()
	engine := NewEngine(EngineOptions{
		WorkspaceRoot:    root,
		Policy:           DefaultPolicy(),
		Backend:          Backend{Name: BackendUnavailable, Message: "native sandbox unavailable"},
		SensitiveEnvKeys: []string{"COMPANY_LLM_SECRET"},
	})

	plan, err := engine.BuildCommandPlan(CommandSpec{
		Name: "/bin/sh",
		Args: []string{"-c", "pwd"},
		Dir:  root,
	})
	if err != nil {
		t.Fatalf("BuildCommandPlan: %v", err)
	}
	if plan.Wrapped || plan.EnforcementLevel != EnforcementDegraded {
		t.Fatalf("plan = %#v, want degraded direct plan", plan)
	}
	for _, entry := range plan.Env {
		key, _, _ := strings.Cut(entry, "=")
		if strings.EqualFold(key, "COMPANY_LLM_SECRET") || strings.EqualFold(key, "ZERO_OAUTH_ACME_CLIENT_SECRET") {
			t.Fatalf("degraded plan.Env retained sensitive key %q: %v", key, plan.Env)
		}
	}
	if got := envListValue(plan.Env, "SAFE_VAR", ""); got != "hello" {
		t.Fatalf("SAFE_VAR = %q, want hello", got)
	}
}

func TestBuildCommandPlanRejectsOutsideDirectory(t *testing.T) {
	root := t.TempDir()
	engine := NewEngine(EngineOptions{
		WorkspaceRoot: root,
		Policy:        DefaultPolicy(),
		Backend:       Backend{Name: BackendUnavailable},
	})

	_, err := engine.BuildCommandPlan(CommandSpec{Name: "/bin/sh", Dir: tempDirOutsideDefaultTemp(t)})
	if err == nil || !strings.Contains(err.Error(), "outside_workspace") {
		t.Fatalf("error = %v, want outside workspace block", err)
	}
}

func assertArgsContainSequence(t *testing.T, args []string, sequence ...string) {
	t.Helper()
	if len(sequence) == 0 {
		return
	}
	for index := 0; index <= len(args)-len(sequence); index++ {
		matched := true
		for offset, want := range sequence {
			if args[index+offset] != want {
				matched = false
				break
			}
		}
		if matched {
			return
		}
	}
	t.Fatalf("args %#v do not contain sequence %#v", args, sequence)
}

// TestSandboxExecProfileAllowsDevNullAndTemp reproduces the audit finding that
// the generated sandbox-exec profile blocked `> /dev/null` and mktemp because
// only the workspace was writable. It runs real commands through sandbox-exec
// when that backend is available on the host.
func TestSandboxExecProfileAllowsDevNullAndTemp(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is macOS-only")
	}
	backend := SelectBackend(BackendOptions{})
	if !backend.Available || backend.Name != BackendMacOSSeatbelt {
		t.Skipf("macOS Seatbelt backend unavailable: %s", backend.Message)
	}
	root := t.TempDir()
	engine := NewEngine(EngineOptions{WorkspaceRoot: root, Policy: DefaultPolicy(), Backend: backend})

	run := func(script string) (string, error) {
		command, _, err := engine.CommandContext(context.Background(), CommandSpec{
			Name: "/bin/sh",
			Args: []string{"-c", script},
			Dir:  root,
		})
		if err != nil {
			return "", err
		}
		out, runErr := command.CombinedOutput()
		return string(out), runErr
	}

	for _, script := range []string{"echo hi > /dev/null", "mktemp"} {
		if out, err := run(script); err != nil {
			t.Fatalf("sandboxed %q failed: %v\noutput: %s", script, err, out)
		}
	}

	// The workspace remains writable; a sibling write still lands.
	if out, err := run("echo ok > probe.txt && cat probe.txt"); err != nil {
		t.Fatalf("workspace write failed: %v\noutput: %s", err, out)
	}

	// A sandboxed script must be able to kill the children it spawns; without the
	// signal allowance seatbelt denies kill() with "Operation not permitted".
	if out, err := run("sleep 5 & child=$!; sleep 0.2; kill $child"); err != nil {
		t.Fatalf("sandboxed self-kill failed (signal allowance missing?): %v\noutput: %s", err, out)
	}

	// A write outside the workspace must still be denied — the richer profile must
	// not have loosened the boundary.
	if out, err := run("echo leak > /etc/zero_sandbox_should_fail 2>/dev/null"); err == nil {
		t.Fatalf("write outside workspace unexpectedly succeeded: output: %s", out)
	}
}

func resolvedTestPath(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", path, err)
	}
	return resolved
}

func TestSandboxExecProfileIncludesExtraWriteRoots(t *testing.T) {
	profile := sandboxExecProfile([]string{"/ws", "/extra root"}, Policy{Mode: ModeEnforce, EnforceWorkspace: true}, "")
	if !strings.Contains(profile, "(allow file-write*") {
		t.Fatalf("profile missing file-write rule:\n%s", profile)
	}
	// Every granted write root is its own (subpath ...) filter.
	for _, root := range []string{"/ws", "/extra root"} {
		if !strings.Contains(profile, `(subpath "`+root+`")`) {
			t.Fatalf("profile missing write root %q:\n%s", root, profile)
		}
	}
	// The baseline temp tree + standard device nodes (parity with the bubblewrap
	// backend) are kept alongside the granted roots.
	if !strings.Contains(profile, `(subpath "/tmp")`) || !strings.Contains(profile, `(literal "/dev/null")`) {
		t.Fatalf("profile missing baseline temp/device write allowances:\n%s", profile)
	}
}

func TestSeatbeltProfileGrantsAncestorMetadataForTraversal(t *testing.T) {
	// A deeply-nested workspace: the (subpath …) read filter grants the root and
	// its descendants but not its parents, so resolving `cd /Users/me/proj/app`
	// must stat /Users, /Users/me, /Users/me/proj — none of which are granted by
	// the subpath alone. Seatbelt then denies the chdir and reports the misleading
	// "Not a directory". The path-ancestors metadata grant fixes the traversal.
	profile := PermissionProfile{
		FileSystem: FileSystemPolicy{
			Kind:                 FileSystemRestricted,
			ReadRoots:            []string{"/Users/me/proj/app"},
			WriteRoots:           []WritableRoot{{Root: "/Users/me/proj/app"}},
			IncludePlatformRoots: true,
		},
		Network: NetworkPolicy{Mode: NetworkDeny},
	}
	sbpl := seatbeltProfileFromPermissionProfile(profile, Policy{Mode: ModeEnforce}, "")
	if !strings.Contains(sbpl, "(allow file-read-metadata file-test-existence") {
		t.Fatalf("profile missing ancestor metadata rule:\n%s", sbpl)
	}
	if !strings.Contains(sbpl, `(path-ancestors "/Users/me/proj/app")`) {
		t.Fatalf("profile missing path-ancestors for the workspace read root:\n%s", sbpl)
	}
	// Metadata only — the ancestors must NOT get a content-read (subpath) grant.
	if strings.Contains(sbpl, `file-read-data`) && strings.Contains(sbpl, `(path-ancestors`) {
		// guard against a future change that widens the ancestor grant to data
		if strings.Contains(sbpl, `file-read* file-test-existence file-read-data`) {
			t.Fatalf("ancestor grant must stay metadata-only:\n%s", sbpl)
		}
	}
}

func TestSeatbeltAncestorRuleSkipsFilesystemRoot(t *testing.T) {
	// A "/" read root has no ancestors; (path-ancestors "/") is invalid SBPL and
	// makes sandbox-exec abort (exit 65), so the rule must skip it entirely.
	if rule := seatbeltAncestorMetadataRule([]string{"/"}); rule != "" {
		t.Fatalf(`expected no ancestor rule for "/", got: %q`, rule)
	}
	// Mixed roots: a real root still gets its grant; "/" is dropped.
	rule := seatbeltAncestorMetadataRule([]string{"/", "/Users/me/app"})
	if strings.Contains(rule, `(path-ancestors "/")`) {
		t.Fatalf(`rule must not contain (path-ancestors "/"): %q`, rule)
	}
	if !strings.Contains(rule, `(path-ancestors "/Users/me/app")`) {
		t.Fatalf("expected ancestor grant for the real root: %q", rule)
	}
	// The compat builder hard-codes ReadRoots=["/"] and flips to restricted under
	// EnforceWorkspace — the generated profile must not contain the malformed rule.
	compat := sandboxExecProfile([]string{"/ws"}, Policy{Mode: ModeEnforce, EnforceWorkspace: true}, "")
	if strings.Contains(compat, `(path-ancestors "/")`) {
		t.Fatalf("compat profile must not contain (path-ancestors \"/\"):\n%s", compat)
	}
}

func TestSeatbeltProfileGrantsCLTToolchain(t *testing.T) {
	// /usr/bin/git, clang, make, etc. are stubs that resolve the real binary under
	// the active developer dir (/Library/Developer/CommandLineTools). The platform
	// read roots must include it or the stub fails with "xcode-select: No developer
	// tools were found".
	profile := PermissionProfile{
		FileSystem: FileSystemPolicy{Kind: FileSystemRestricted, ReadRoots: []string{"/ws"}, IncludePlatformRoots: true},
		Network:    NetworkPolicy{Mode: NetworkDeny},
	}
	sbpl := seatbeltProfileFromPermissionProfile(profile, Policy{Mode: ModeEnforce}, "")
	if !strings.Contains(sbpl, `(subpath "/Library/Developer")`) {
		t.Fatalf("profile must grant read on the CLT toolchain (/Library/Developer):\n%s", sbpl)
	}
	// /Library/Developer is not top-level, so its ancestor (/Library) must be
	// stat-able or a chdir-style traversal into the toolchain ENOTDIRs even though
	// reads succeed. Platform read roots must get the ancestor-metadata grant too.
	if !strings.Contains(sbpl, `(path-ancestors "/Library/Developer")`) {
		t.Fatalf("profile must grant ancestor metadata for /Library/Developer so /Library is traversable:\n%s", sbpl)
	}
}

func TestUserGitConfigReadPathsScopedToConfigFiles(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		t.Skip("no home dir")
	}
	paths := userGitConfigReadPaths()
	found := false
	for _, p := range paths {
		if p == filepath.Join(home, ".gitconfig") {
			found = true
		}
		// Must NOT grant the whole ~/.config/git dir — it can hold a credential store.
		if p == filepath.Join(home, ".config", "git") {
			t.Fatalf("must not grant the ~/.config/git directory (credential store): %v", paths)
		}
	}
	if !found {
		t.Fatalf("expected ~/.gitconfig in git config read paths, got %v", paths)
	}
}

func TestSeatbeltProfileConsumesPermissionProfile(t *testing.T) {
	profile := PermissionProfile{
		FileSystem: FileSystemPolicy{
			Kind:      FileSystemRestricted,
			ReadRoots: []string{"/read-root"},
			WriteRoots: []WritableRoot{{
				Root: "/write-root",
			}},
			IncludePlatformRoots: true,
			AllowTemp:            true,
		},
		Network: NetworkPolicy{Mode: NetworkDeny},
	}
	sbpl := seatbeltProfileFromPermissionProfile(profile, Policy{Mode: ModeEnforce}, "")
	for _, want := range []string{
		`(subpath "/read-root")`,
		`(subpath "/write-root")`,
		`(subpath "/usr/bin")`,
		`(subpath "/System/Library/Frameworks")`,
		`(subpath "/private/var/db")`,
		`(subpath "/tmp")`,
		`(literal "/dev/null")`,
		`(deny network*)`,
	} {
		if !strings.Contains(sbpl, want) {
			t.Fatalf("Seatbelt profile missing %q:\n%s", want, sbpl)
		}
	}
	for _, forbidden := range []string{"network-bind", "network-inbound", `remote ip "localhost:*"`} {
		if strings.Contains(sbpl, forbidden) {
			t.Fatalf("restricted Seatbelt profile must not contain host-local rule %q:\n%s", forbidden, sbpl)
		}
	}
	if strings.Contains(sbpl, "(allow file-read*)\n(allow file-write*)") {
		t.Fatalf("restricted permission profile must not become full read/write:\n%s", sbpl)
	}
}

func TestSeatbeltProfileIncludesRuntimeStartupAllowances(t *testing.T) {
	sbpl := sandboxExecProfile([]string{"/ws"}, Policy{Mode: ModeEnforce, EnforceWorkspace: true}, "")
	for _, want := range []string{
		`(allow file-map-executable`,
		`(subpath "/System/Library/Frameworks")`,
		`(allow system-mac-syscall (mac-policy-name "vnguard"))`,
		`(allow file-read* file-test-existence (literal "/"))`,
		`(allow user-preference-read)`,
		`(allow pseudo-tty)`,
		`(allow ipc-posix-sem)`,
	} {
		if !strings.Contains(sbpl, want) {
			t.Fatalf("Seatbelt profile missing runtime startup allowance %q:\n%s", want, sbpl)
		}
	}
}

func TestSeatbeltCommandPlanUsesExecutionPermissionProfile(t *testing.T) {
	request := SandboxExecutionRequest{
		Command: CommandSpec{Name: "/bin/sh", Args: []string{"-c", "true"}, Dir: "/workspace"},
		Backend: Backend{
			Name:            BackendMacOSSeatbelt,
			Available:       true,
			Executable:      "/usr/bin/sandbox-exec",
			CommandWrapping: true,
			NativeIsolation: true,
		},
		WorkspaceRoot: "/workspace",
		PermissionProfile: PermissionProfile{
			FileSystem: FileSystemPolicy{
				Kind:       FileSystemRestricted,
				ReadRoots:  []string{"/"},
				WriteRoots: []WritableRoot{{Root: "/profile-write"}},
				AllowTemp:  true,
			},
			Network: NetworkPolicy{Mode: NetworkDeny},
		},
		TargetBackend:           BackendMacOSSeatbelt,
		CommandWrapped:          true,
		EnforcementLevel:        EnforcementNative,
		RequiresPlatformSandbox: true,
	}
	plan, err := buildPlatformCommandPlan(request, Policy{Mode: ModeEnforce, EnforceWorkspace: true})
	if err != nil {
		t.Fatalf("buildPlatformCommandPlan: %v", err)
	}
	if len(plan.Args) < 2 {
		t.Fatalf("plan args = %#v, want sandbox-exec profile", plan.Args)
	}
	sbpl := plan.Args[1]
	if !strings.Contains(sbpl, `(subpath "/profile-write")`) {
		t.Fatalf("plan profile did not use PermissionProfile write root:\n%s", sbpl)
	}
}

func TestSeatbeltProfileProtectsMetadataAndDenyOrdering(t *testing.T) {
	profile := PermissionProfile{
		FileSystem: FileSystemPolicy{
			Kind:      FileSystemRestricted,
			ReadRoots: []string{"/"},
			WriteRoots: []WritableRoot{{
				Root:                   "/repo",
				ReadOnlySubpaths:       []string{"/repo/vendor"},
				ProtectedMetadataNames: []string{".git", ".zero"},
			}},
			DenyRead:  []string{"/repo/secret-read"},
			DenyWrite: []string{"/repo/secret-write"},
			AllowTemp: true,
		},
		Network: NetworkPolicy{Mode: NetworkDeny},
	}
	sbpl := seatbeltProfileFromPermissionProfile(profile, Policy{Mode: ModeEnforce}, "")
	normalizedSecretRead := sandboxProfileString(normalizeProfilePath("/repo/secret-read"))
	normalizedSecretWrite := sandboxProfileString(normalizeProfilePath("/repo/secret-write"))
	denySecretReadRule := `(deny file-read* (subpath "` + normalizedSecretRead + `"))`
	denySecretReadUnlinkRule := `(deny file-write-unlink (subpath "` + normalizedSecretRead + `"))`
	denySecretWriteRule := `(deny file-write* (subpath "` + normalizedSecretWrite + `"))`
	for _, want := range []string{
		`(deny file-write* (literal "/repo/vendor"))`,
		`(deny file-write* (subpath "/repo/vendor"))`,
		`(deny file-write* (regex #"^/repo/\.git(/.*)?$"))`,
		`(deny file-write* (regex #"^/repo/\.zero(/.*)?$"))`,
		denySecretReadRule,
		denySecretReadUnlinkRule,
		denySecretWriteRule,
	} {
		if !strings.Contains(sbpl, want) {
			t.Fatalf("Seatbelt profile missing %q:\n%s", want, sbpl)
		}
	}
	// A user-configured read-denied path keeps the write direction: a cache or
	// generated directory the build legitimately writes must not become read-only
	// just because it is excluded from reads. Only Zero's own automatic credential
	// entries are write-denied (see TestProtectedCredentialsDenyReadAndWrite...).
	if strings.Contains(sbpl, `(deny file-write* (subpath "`+normalizedSecretRead+`"))`) ||
		strings.Contains(sbpl, `(deny file-write* (literal "`+normalizedSecretRead+`"))`) {
		t.Fatalf("a user-configured DenyRead path must stay writable:\n%s", sbpl)
	}
	allowIdx := strings.Index(sbpl, "(allow file-write*")
	denyReadIdx := strings.Index(sbpl, denySecretReadRule)
	metadataIdx := strings.Index(sbpl, `(deny file-write* (regex #"^/repo/\.git(/.*)?$"))`)
	denyWriteIdx := strings.Index(sbpl, denySecretWriteRule)
	if allowIdx < 0 || denyReadIdx < allowIdx || metadataIdx < allowIdx || denyWriteIdx < allowIdx {
		t.Fatalf("deny rules must follow the broad write allow (allow=%d denyRead=%d metadata=%d denyWrite=%d):\n%s", allowIdx, denyReadIdx, metadataIdx, denyWriteIdx, sbpl)
	}
}

// TestSeatbeltProfileRendersCredentialBaselineAndCarveouts pins the rendering of
// the credential baseline, which the struct-level tests do not cover: a
// DenyReadIfExists entry must reach the profile as a real deny rule (dropping
// the field from denyReadRules would silently make every credential store
// readable again on macOS), and a DenyReadCarveouts entry must be re-allowed
// AFTER it, since SBPL is last-match-wins.
func TestSeatbeltProfileRendersCredentialBaselineAndCarveouts(t *testing.T) {
	credentialDir := filepath.Join(t.TempDir(), "zero")
	pluginRoot := filepath.Join(credentialDir, "plugins")
	if err := os.MkdirAll(pluginRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	profile := PermissionProfile{
		FileSystem: FileSystemPolicy{
			Kind:              FileSystemRestricted,
			ReadRoots:         []string{"/"},
			WriteRoots:        []WritableRoot{{Root: "/repo"}},
			DenyReadIfExists:  []string{credentialDir},
			DenyReadCarveouts: []string{pluginRoot},
			AllowTemp:         true,
		},
		Network: NetworkPolicy{Mode: NetworkDeny},
	}
	sbpl := seatbeltProfileFromPermissionProfile(profile, Policy{Mode: ModeEnforce}, "")
	denyRule := `(deny file-read* (subpath "` + sandboxProfileString(normalizeProfilePath(credentialDir)) + `"))`
	allowRule := `(allow file-read* file-test-existence (subpath "` + sandboxProfileString(normalizeProfilePath(pluginRoot)) + `"))`
	denyIdx := strings.Index(sbpl, denyRule)
	allowIdx := strings.Index(sbpl, allowRule)
	if denyIdx < 0 {
		t.Fatalf("Seatbelt profile missing credential baseline deny %q:\n%s", denyRule, sbpl)
	}
	if allowIdx < 0 {
		t.Fatalf("Seatbelt profile missing carveout allow %q:\n%s", allowRule, sbpl)
	}
	if allowIdx < denyIdx {
		t.Fatalf("carveout allow (%d) must follow the deny (%d) to win under last-match-wins:\n%s", allowIdx, denyIdx, sbpl)
	}
}

func TestSeatbeltProfileReappliesCredentialDeniesInsideCarveouts(t *testing.T) {
	credentialDir := filepath.Join(t.TempDir(), "zero")
	var carveouts []string
	var denied []string
	for _, name := range []string{"plugins", "specialists", "commands"} {
		root := filepath.Join(credentialDir, name)
		if err := os.MkdirAll(root, 0o700); err != nil {
			t.Fatal(err)
		}
		carveouts = append(carveouts, root)
		for _, path := range []string{filepath.Join(root, "tokens.json"), filepath.Join(root, "tokens.json.secret")} {
			if err := os.WriteFile(path, []byte("secret"), 0o600); err != nil {
				t.Fatal(err)
			}
			denied = append(denied, path)
		}
	}
	profile := PermissionProfile{
		FileSystem: FileSystemPolicy{
			Kind:                        FileSystemRestricted,
			ReadRoots:                   []string{"/"},
			DenyReadIfExists:            append([]string{credentialDir}, denied...),
			DenyReadCarveouts:           carveouts,
			ProcessTrustedDenyReadFiles: denied,
		},
		Network: NetworkPolicy{Mode: NetworkDeny},
	}

	sbpl := seatbeltProfileFromPermissionProfile(profile, Policy{Mode: ModeEnforce}, "")
	for _, root := range carveouts {
		allowRule := `(allow file-read* file-test-existence (subpath "` + sandboxProfileString(normalizeProfilePath(root)) + `"))`
		allowIdx := strings.Index(sbpl, allowRule)
		if allowIdx < 0 {
			t.Fatalf("Seatbelt profile missing carveout allow %q:\n%s", allowRule, sbpl)
		}
		for _, path := range []string{filepath.Join(root, "tokens.json"), filepath.Join(root, "tokens.json.secret")} {
			denyRule := `(deny file-read* (literal "` + sandboxProfileString(normalizeProfilePath(path)) + `"))`
			if denyIdx := strings.LastIndex(sbpl, denyRule); denyIdx < allowIdx {
				t.Fatalf("nested credential deny %q must follow carveout allow (deny=%d allow=%d):\n%s", denyRule, denyIdx, allowIdx, sbpl)
			}
		}
		ordinary := filepath.Join(root, "extension.json")
		if strings.Contains(sbpl, `(deny file-read* (literal "`+sandboxProfileString(normalizeProfilePath(ordinary))+`"))`) {
			t.Fatalf("ordinary extension file under %q must remain readable:\n%s", root, sbpl)
		}
	}
}

func TestSeatbeltProfileDoesNotRenderSymlinkCarveout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation is not reliably available on Windows CI")
	}
	credentialDir := filepath.Join(t.TempDir(), "zero")
	if err := os.MkdirAll(credentialDir, 0o700); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(credentialDir, "oauth-tokens.json")
	if err := os.WriteFile(secret, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	pluginRoot := filepath.Join(credentialDir, "plugins")
	if err := os.Symlink(secret, pluginRoot); err != nil {
		t.Fatal(err)
	}
	profile := PermissionProfile{
		FileSystem: FileSystemPolicy{
			Kind:              FileSystemRestricted,
			ReadRoots:         []string{"/"},
			DenyReadIfExists:  []string{credentialDir},
			DenyReadCarveouts: []string{pluginRoot},
		},
		Network: NetworkPolicy{Mode: NetworkDeny},
	}

	sbpl := seatbeltProfileFromPermissionProfile(profile, Policy{Mode: ModeEnforce}, "")
	for _, path := range []string{pluginRoot, secret, normalizeProfilePath(pluginRoot), normalizeProfilePath(secret)} {
		allow := `(allow file-read* file-test-existence (subpath "` + sandboxProfileString(path) + `"))`
		if strings.Contains(sbpl, allow) {
			t.Fatalf("Seatbelt profile re-allowed symlink carveout path %q:\n%s", path, sbpl)
		}
	}
}

func TestSeatbeltTemporaryWritesFollowPermissionProfile(t *testing.T) {
	runtimeRules := seatbeltPlatformRuntimeRules()
	if strings.Contains(runtimeRules, `file-write* (subpath "/tmp")`) {
		t.Fatal("platform runtime rules must not bypass FileSystemPolicy.AllowTemp")
	}
	withoutTemp := seatbeltWriteRule(FileSystemPolicy{Kind: FileSystemRestricted})
	if strings.Contains(withoutTemp, `(subpath "/tmp")`) {
		t.Fatal("restricted profile with AllowTemp=false unexpectedly permits /tmp writes")
	}
	withTemp := seatbeltWriteRule(FileSystemPolicy{Kind: FileSystemRestricted, AllowTemp: true})
	if !strings.Contains(withTemp, `(subpath "/tmp")`) {
		t.Fatal("restricted profile with AllowTemp=true must permit /tmp writes")
	}
}

// TestSeatbeltProfileAllowsGitWritesExceptHooksAndConfig locks in the fix for
// git subprocesses (fetch, commit, add, ...) failing under the sandbox: the
// default profile must stop write-denying the whole .git tree and only carve
// out .git/hooks and .git/config, which stay dangerous (auto-executing
// scripts, remote/credential-helper rewrites) regardless of what wrote them.
func TestSeatbeltProfileAllowsGitWritesExceptHooksAndConfig(t *testing.T) {
	workspace := t.TempDir()
	profile := DefaultPermissionProfile(workspace)
	sbpl := seatbeltProfileFromPermissionProfile(profile, Policy{Mode: ModeEnforce}, "")

	resolvedWorkspace := normalizeProfilePath(workspace)
	gitRegex := `(deny file-write* (regex #"^` + regexpQuoteMeta(resolvedWorkspace) + `/\.git(/.*)?$"))`
	if strings.Contains(sbpl, gitRegex) {
		t.Fatalf("seatbelt profile must not blanket-deny the whole .git tree:\n%s", sbpl)
	}
	hooksPath := sandboxProfileString(filepath.Join(resolvedWorkspace, ".git", "hooks"))
	configPath := sandboxProfileString(filepath.Join(resolvedWorkspace, ".git", "config"))
	for _, want := range []string{
		`(deny file-write* (literal "` + hooksPath + `"))`,
		`(deny file-write* (subpath "` + hooksPath + `"))`,
		`(deny file-write* (literal "` + configPath + `"))`,
		`(deny file-write* (subpath "` + configPath + `"))`,
	} {
		if !strings.Contains(sbpl, want) {
			t.Fatalf("seatbelt profile missing %q:\n%s", want, sbpl)
		}
	}
}

func TestSandboxExecProfileTagsDenialsWhenMonitoring(t *testing.T) {
	off := sandboxExecProfile([]string{"/ws"}, Policy{Mode: ModeEnforce, EnforceWorkspace: true}, "")
	if strings.Contains(off, "with message") {
		t.Fatalf("denials must not be tagged when monitoring is off:\n%s", off)
	}
	if !strings.Contains(off, "(deny default)") {
		t.Fatalf("profile missing the plain default-deny:\n%s", off)
	}

	on := sandboxExecProfile([]string{"/ws"}, Policy{Mode: ModeEnforce, EnforceWorkspace: true, MonitorDenials: true}, "run-tag-123")
	if !strings.Contains(on, `(deny default (with message "run-tag-123"))`) {
		t.Fatalf("denials must be tagged when monitoring is on:\n%s", on)
	}
}

func TestSandboxExecCommandPlanUsesUniquePerPlanDenialTag(t *testing.T) {
	policy := Policy{Mode: ModeEnforce, EnforceWorkspace: true, MonitorDenials: true}
	backend := Backend{Name: BackendMacOSSeatbelt, Available: true, Executable: "/usr/bin/sandbox-exec"}
	spec := CommandSpec{Name: "/bin/sh", Args: []string{"-c", "true"}, Dir: "/ws"}
	profile := seatbeltCompatibilityPermissionProfile([]string{"/ws"}, policy)

	p1 := seatbeltCommandPlanWithProfile(spec, "/ws", profile, policy, backend)
	p2 := seatbeltCommandPlanWithProfile(spec, "/ws", profile, policy, backend)
	if p1.MonitorTag == "" || p2.MonitorTag == "" {
		t.Fatalf("monitored plans must carry a denial tag: %q %q", p1.MonitorTag, p2.MonitorTag)
	}
	if p1.MonitorTag == p2.MonitorTag {
		t.Fatalf("each monitored plan must get a unique tag so monitors can't cross-ingest, both = %q", p1.MonitorTag)
	}
	// The profile embedded in each plan must carry that plan's own tag (the monitor
	// matches on it).
	if !strings.Contains(strings.Join(p1.Args, " "), p1.MonitorTag) {
		t.Fatalf("plan profile must embed its own tag %q:\n%v", p1.MonitorTag, p1.Args)
	}

	offPolicy := Policy{Mode: ModeEnforce, EnforceWorkspace: true}
	off := seatbeltCommandPlanWithProfile(spec, "/ws", seatbeltCompatibilityPermissionProfile([]string{"/ws"}, offPolicy), offPolicy, backend)
	if off.MonitorTag != "" {
		t.Fatalf("a non-monitored plan must carry no tag, got %q", off.MonitorTag)
	}
}

func TestSeatbeltCompatibilityProfileUsesCredentialEnvironment(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows credential deny-read is tracked separately")
	}
	override := filepath.Join(t.TempDir(), "mcp-tokens.json")
	t.Setenv("ZERO_MCP_OAUTH_TOKENS_PATH", override)

	profile := seatbeltCompatibilityPermissionProfile([]string{"/ws"}, DefaultPolicy())
	for _, want := range []string{
		override,
		override + ".migrated",
	} {
		if !stringSliceContains(profile.FileSystem.DenyReadIfExists, normalizeProfilePath(want)) {
			t.Fatalf("DenyReadIfExists = %#v, want environment override sibling %q", profile.FileSystem.DenyReadIfExists, want)
		}
	}
}

func TestSandboxExecProfileGrantsSignalAndMachLookup(t *testing.T) {
	profile := sandboxExecProfile([]string{"/ws"}, Policy{Mode: ModeEnforce, EnforceWorkspace: true}, "")

	// Signalling is allowed so a sandboxed command can kill the children it spawns
	// AND user-owned processes the user asks it to terminate (e.g. a stale dev
	// server from a previous session, in a different process group). The kernel
	// still enforces UID ownership, so root/other-user processes stay protected.
	if !strings.Contains(profile, "(allow signal)") {
		t.Fatalf("profile missing signal allowance:\n%s", profile)
	}
	// Curated mach-lookup so keychain/opendirectory/preferences/network-config
	// tools work without touching the file or network boundary.
	if !strings.Contains(profile, "(allow mach-lookup") {
		t.Fatalf("profile missing mach-lookup rule:\n%s", profile)
	}
	for _, service := range []string{
		"com.apple.securityd",
		"com.apple.system.opendirectoryd.libinfo",
		"com.apple.cfprefsd.daemon",
	} {
		if !strings.Contains(profile, `(global-name "`+service+`")`) {
			t.Fatalf("profile missing mach service %q:\n%s", service, profile)
		}
	}
	// The security boundary must remain: default-deny plus scoped file-write.
	if !strings.Contains(profile, "(deny default)") || !strings.Contains(profile, "(allow file-write*\n") {
		t.Fatalf("profile lost its default-deny / scoped-write boundary:\n%s", profile)
	}
}

func TestLinuxHelperPlanCarriesExtraWriteRoots(t *testing.T) {
	workspace := t.TempDir()
	extra := tempDirOutsideDefaultTemp(t)
	scope, err := NewScope(workspace, []string{extra})
	if err != nil {
		t.Fatalf("NewScope: %v", err)
	}
	engine := NewEngine(EngineOptions{
		WorkspaceRoot: workspace,
		Policy:        DefaultPolicy(),
		Scope:         scope,
		Backend:       Backend{Name: BackendLinuxBwrap, Available: true, Executable: "/usr/bin/zero-linux-sandbox"},
	})
	plan, err := engine.BuildCommandPlan(CommandSpec{Name: "true"})
	if err != nil {
		t.Fatalf("BuildCommandPlan: %v", err)
	}
	config, err := ParseLinuxSandboxHelperArgs(plan.Args)
	if err != nil {
		t.Fatalf("ParseLinuxSandboxHelperArgs: %v", err)
	}
	resolvedExtra := scope.Roots()[1]
	if len(config.PermissionProfile.FileSystem.WriteRoots) < 2 || config.PermissionProfile.FileSystem.WriteRoots[1].Root != resolvedExtra {
		t.Fatalf("helper profile missing extra write root %q: %#v", resolvedExtra, config.PermissionProfile.FileSystem.WriteRoots)
	}
}

func TestResolveCommandDirAllowsExtraRootCwd(t *testing.T) {
	workspace := t.TempDir()
	extra := tempDirOutsideDefaultTemp(t)
	scope, err := NewScope(workspace, []string{extra})
	if err != nil {
		t.Fatalf("NewScope: %v", err)
	}
	engine := NewEngine(EngineOptions{WorkspaceRoot: workspace, Policy: DefaultPolicy(), Scope: scope})
	if _, _, err := engine.resolveCommandDir(extra, engine.policy); err != nil {
		t.Fatalf("resolveCommandDir(extra root) = %v, want nil", err)
	}
	if _, _, err := engine.resolveCommandDir(tempDirOutsideDefaultTemp(t), engine.policy); err == nil {
		t.Fatal("resolveCommandDir(outside all roots) = nil error, want block")
	}
}

func TestLinuxHelperPlanPreservesRealExtraRootCwd(t *testing.T) {
	workspace := t.TempDir()
	extra := tempDirOutsideDefaultTemp(t)
	credentialHome := filepath.Join(workspace, "credential-home")
	configHome := filepath.Join(credentialHome, "config")
	if err := os.MkdirAll(filepath.Join(configHome, "zero"), 0o700); err != nil {
		t.Fatal(err)
	}
	scope, err := NewScope(workspace, []string{extra})
	if err != nil {
		t.Fatalf("NewScope: %v", err)
	}
	engine := NewEngine(EngineOptions{
		WorkspaceRoot: workspace,
		Policy:        DefaultPolicy(),
		Scope:         scope,
		Backend:       Backend{Name: BackendLinuxBwrap, Available: true, Executable: "/usr/bin/zero-linux-sandbox"},
	})
	resolvedExtra := scope.Roots()[1]
	plan, err := engine.BuildCommandPlan(CommandSpec{Name: "true", Dir: extra, Env: []string{"HOME=" + credentialHome, "XDG_CONFIG_HOME=" + configHome}})
	if err != nil {
		t.Fatalf("BuildCommandPlan: %v", err)
	}
	if filepath.Clean(plan.SandboxDir) != filepath.Clean(resolvedExtra) {
		t.Fatalf("SandboxDir=%q want real extra-root path %q", plan.SandboxDir, resolvedExtra)
	}
	assertArgsContainSequence(t, plan.Args, "--command-cwd", resolvedExtra)
	config, err := ParseLinuxSandboxHelperArgs(plan.Args)
	if err != nil {
		t.Fatalf("ParseLinuxSandboxHelperArgs: %v", err)
	}
	bwrapArgs, err := BuildLinuxSandboxBwrapArgs(LinuxSandboxBwrapOptions{Config: config, HelperPath: "/usr/bin/zero-linux-sandbox"})
	if err != nil {
		t.Fatalf("BuildLinuxSandboxBwrapArgs: %v", err)
	}
	assertArgsContainSequence(t, bwrapArgs, "--chdir", resolvedExtra)
}

func TestScrubSensitiveEnv(t *testing.T) {
	inputEnv := []string{
		"PATH=/usr/bin",
		"OPENAI_API_KEY=sk-proj-12345",
		"ANTHROPIC_API_KEY=sk-ant-12345",
		"GEMINI_API_KEY=AIzaSy12345",
		"DEEPSEEK_API_KEY=ds-12345",
		"GITHUB_TOKEN=ghp_12345",
		"AWS_ACCESS_KEY_ID=AKIA12345",
		"AWS_SECRET_ACCESS_KEY=secret12345",
		"GOOGLE_API_KEY=AIzaSy67890",
		"XAI_API_KEY=xai-12345",
		"HUGGINGFACE_API_KEY=hf_12345",
		"GOOGLE_APPLICATION_CREDENTIALS=/home/user/sa-key.json",
		// Both forms of the daemon bridge token. The inline one was already
		// scrubbed; the file form was not, which left a sandboxed command the
		// path to read it from (#677).
		"ZERO_DAEMON_REMOTE_TOKEN=bridge-token-inline",
		"ZERO_DAEMON_REMOTE_TOKEN_FILE=/home/user/.zero/remote-token",
		"ZERO_INTERNAL_DAEMON_REMOTE_TOKEN_FILE_RESOLVED=/home/user/.zero/resolved-remote-token",
		"ZERO_INTERNAL_DAEMON_REMOTE_TOKEN_FILE_IDENTITY=unix:1:2",
		"COMPANY_LLM_SECRET=custom-secret",
		"ZERO_OAUTH_MY_SVC_CLIENT_SECRET=oauth-secret",
		"zero_oauth_second_client_secret=case-insensitive-secret",
		"ZERO_OAUTH_CLIENT_SECRET=not-a-provider-secret",
		"ZERO_DAEMON_REMOTE_TOKEN_FILE=/home/user/daemon-token",
		"AWS_PROFILE=staging",
		"SAFE_VAR=hello",
	}
	scrubbed := scrubSensitiveEnv(inputEnv, " COMPANY_LLM_SECRET ", "company_llm_secret", "GITHUB_TOKEN=ghp_pasted-assignment", "=", "")
	expected := map[string]string{
		"PATH":                     "/usr/bin",
		"SAFE_VAR":                 "hello",
		"AWS_PROFILE":              "staging",
		"ZERO_OAUTH_CLIENT_SECRET": "not-a-provider-secret",
	}
	actual := make(map[string]string, len(scrubbed))
	for _, entry := range scrubbed {
		key, value, _ := strings.Cut(entry, "=")
		if _, dup := actual[key]; dup {
			t.Errorf("duplicate key %q in scrubbed env: %v", key, scrubbed)
		}
		actual[key] = value
	}
	if !reflect.DeepEqual(actual, expected) {
		t.Errorf("scrubSensitiveEnv() = %v, want %v", actual, expected)
	}
}

func TestEngineScrubsConfiguredSensitiveEnvKeys(t *testing.T) {
	workspace := t.TempDir()
	engine := NewEngine(EngineOptions{
		WorkspaceRoot:    workspace,
		Policy:           DefaultPolicy(),
		Backend:          Backend{Name: BackendLinuxBwrap, Available: true, Executable: "/usr/bin/zero-linux-sandbox"},
		SensitiveEnvKeys: []string{"COMPANY_LLM_SECRET"},
	})
	plan, err := engine.BuildCommandPlan(CommandSpec{
		Name: "true",
		Env: []string{
			"PATH=/usr/bin",
			"COMPANY_LLM_SECRET=custom-secret",
			"ZERO_OAUTH_CUSTOM_CLIENT_SECRET=oauth-secret",
			"SAFE_VAR=hello",
		},
	})
	if err != nil {
		t.Fatalf("BuildCommandPlan: %v", err)
	}
	for _, entry := range plan.Env {
		key, _, _ := strings.Cut(entry, "=")
		if strings.EqualFold(key, "COMPANY_LLM_SECRET") || strings.EqualFold(key, "ZERO_OAUTH_CUSTOM_CLIENT_SECRET") {
			t.Fatalf("plan.Env retained sensitive key %q: %v", key, plan.Env)
		}
	}
	if got := envListValue(plan.Env, "SAFE_VAR", ""); got != "hello" {
		t.Fatalf("SAFE_VAR = %q, want hello", got)
	}
}
