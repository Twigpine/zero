package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/config"
	"github.com/Twigpine/zero/internal/sandbox"
)

func TestRunSandboxGrantsAllowListDenyRevokeAndClear(t *testing.T) {
	store := newSandboxTestStore(t)
	deps := appDeps{newSandboxStore: func() (*sandbox.GrantStore, error) { return store, nil }}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := runWithDeps([]string{"sandbox", "grants", "allow", "write_file", "--reason", "workspace edits", "--json"}, &stdout, &stderr, deps)
	if exitCode != exitSuccess {
		t.Fatalf("allow exit = %d, stderr %q", exitCode, stderr.String())
	}
	var allowPayload struct {
		Grant sandbox.Grant `json:"grant"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &allowPayload); err != nil {
		t.Fatalf("decode allow JSON: %v\n%s", err, stdout.String())
	}
	if allowPayload.Grant.ToolName != "write_file" || allowPayload.Grant.Decision != sandbox.GrantAllow {
		t.Fatalf("unexpected allow payload: %#v", allowPayload)
	}

	stdout.Reset()
	stderr.Reset()
	exitCode = runWithDeps([]string{"sandbox", "grants", "deny", "bash", "--reason=network blocked"}, &stdout, &stderr, deps)
	if exitCode != exitSuccess {
		t.Fatalf("deny exit = %d, stderr %q", exitCode, stderr.String())
	}
	if !strings.Contains(stdout.String(), "bash") || !strings.Contains(stdout.String(), "deny") {
		t.Fatalf("unexpected deny text: %q", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	exitCode = runWithDeps([]string{"sandbox", "grants", "list", "--json"}, &stdout, &stderr, deps)
	if exitCode != exitSuccess {
		t.Fatalf("list exit = %d, stderr %q", exitCode, stderr.String())
	}
	var listPayload struct {
		Grants []sandbox.Grant `json:"grants"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &listPayload); err != nil {
		t.Fatalf("decode list JSON: %v\n%s", err, stdout.String())
	}
	if len(listPayload.Grants) != 2 || listPayload.Grants[0].ToolName != "bash" || listPayload.Grants[1].ToolName != "write_file" {
		t.Fatalf("unexpected sorted grants: %#v", listPayload.Grants)
	}

	stdout.Reset()
	stderr.Reset()
	exitCode = runWithDeps([]string{"sandbox", "grants", "revoke", "bash", "--json"}, &stdout, &stderr, deps)
	if exitCode != exitSuccess {
		t.Fatalf("revoke exit = %d, stderr %q", exitCode, stderr.String())
	}
	var revokePayload struct {
		Revoked int `json:"revoked"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &revokePayload); err != nil {
		t.Fatalf("decode revoke JSON: %v\n%s", err, stdout.String())
	}
	if revokePayload.Revoked != 1 {
		t.Fatalf("revoked = %d, want 1", revokePayload.Revoked)
	}

	stdout.Reset()
	stderr.Reset()
	exitCode = runWithDeps([]string{"sandbox", "grants", "clear", "--json"}, &stdout, &stderr, deps)
	if exitCode != exitUsage {
		t.Fatalf("clear without confirm exit = %d, want usage", exitCode)
	}
	if !strings.Contains(stderr.String(), "--confirm") {
		t.Fatalf("expected confirm error, got %q", stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	exitCode = runWithDeps([]string{"sandbox", "grants", "clear", "--confirm", "--json"}, &stdout, &stderr, deps)
	if exitCode != exitSuccess {
		t.Fatalf("clear exit = %d, stderr %q", exitCode, stderr.String())
	}
	var clearPayload struct {
		Cleared int `json:"cleared"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &clearPayload); err != nil {
		t.Fatalf("decode clear JSON: %v\n%s", err, stdout.String())
	}
	if clearPayload.Cleared != 1 {
		t.Fatalf("cleared = %d, want 1", clearPayload.Cleared)
	}
}

func TestRunSandboxGrantsListIncludesCommandPrefixes(t *testing.T) {
	store := newSandboxTestStore(t)
	if _, err := store.GrantCommandPrefix(sandbox.CommandPrefixInput{ToolName: "bash", Prefix: []string{"git", "status"}, Reason: "status checks"}); err != nil {
		t.Fatalf("GrantCommandPrefix: %v", err)
	}
	deps := appDeps{newSandboxStore: func() (*sandbox.GrantStore, error) { return store, nil }}

	var stdout, stderr bytes.Buffer
	exitCode := runWithDeps([]string{"sandbox", "grants", "list", "--json"}, &stdout, &stderr, deps)
	if exitCode != exitSuccess {
		t.Fatalf("list json exit = %d, stderr %q", exitCode, stderr.String())
	}
	var payload struct {
		CommandPrefixes []sandbox.CommandPrefixGrant `json:"commandPrefixes"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("decode list JSON: %v\n%s", err, stdout.String())
	}
	if len(payload.CommandPrefixes) != 1 || !reflect.DeepEqual(payload.CommandPrefixes[0].Prefix, []string{"git", "status"}) {
		t.Fatalf("unexpected commandPrefixes payload: %#v", payload.CommandPrefixes)
	}

	stdout.Reset()
	stderr.Reset()
	exitCode = runWithDeps([]string{"sandbox", "grants", "list"}, &stdout, &stderr, deps)
	if exitCode != exitSuccess {
		t.Fatalf("list text exit = %d, stderr %q", exitCode, stderr.String())
	}
	for _, want := range []string{"bash", "`git status`", "command-prefix", "status checks"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("list text = %q, missing %q", stdout.String(), want)
		}
	}
}

func TestRunSandboxGrantsCreateAndRevokeByPath(t *testing.T) {
	store := newSandboxTestStore(t)
	deps := appDeps{newSandboxStore: func() (*sandbox.GrantStore, error) { return store, nil }}
	path := filepath.Join(t.TempDir(), "secret.txt")

	var stdout, stderr bytes.Buffer
	run := func(args ...string) int {
		stdout.Reset()
		stderr.Reset()
		return runWithDeps(args, &stdout, &stderr, deps)
	}

	// An exact-path grant and a tool-wide grant for the same tool.
	if exit := run("sandbox", "grants", "allow", "write_file", "--path", path); exit != exitSuccess {
		t.Fatalf("allow --path exit = %d, stderr %q", exit, stderr.String())
	}
	if exit := run("sandbox", "grants", "allow", "write_file"); exit != exitSuccess {
		t.Fatalf("allow tool-wide exit = %d, stderr %q", exit, stderr.String())
	}

	// Revoking by path removes only the path-scoped grant.
	if exit := run("sandbox", "grants", "revoke", "write_file", "--path", path, "--json"); exit != exitSuccess {
		t.Fatalf("revoke --path exit = %d, stderr %q", exit, stderr.String())
	}
	var payload struct {
		Revoked int `json:"revoked"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("decode revoke JSON: %v\n%s", err, stdout.String())
	}
	if payload.Revoked != 1 {
		t.Fatalf("revoked = %d, want 1", payload.Revoked)
	}

	grants, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(grants) != 1 || grants[0].ScopeKind != sandbox.ScopeToolWide {
		t.Fatalf("expected only the tool-wide grant to remain, got %#v", grants)
	}
}

func TestRunSandboxGrantsRejectsEmptyPath(t *testing.T) {
	store := newSandboxTestStore(t)
	deps := appDeps{newSandboxStore: func() (*sandbox.GrantStore, error) { return store, nil }}

	var stdout, stderr bytes.Buffer
	run := func(args ...string) int {
		stdout.Reset()
		stderr.Reset()
		return runWithDeps(args, &stdout, &stderr, deps)
	}

	// Seed a tool-wide grant first so a buggy "revoke all for tool" or "allow
	// tool-wide" from a rejected call would actually change the store and be caught
	// (a revoke-all on an empty store is a silent no-op).
	if _, err := store.Grant(sandbox.GrantInput{ToolName: "write_file", Decision: sandbox.GrantAllow}); err != nil {
		t.Fatalf("seed grant: %v", err)
	}

	// An explicit but empty --path must fail closed rather than silently widening
	// an allow to tool-wide or a revoke to all-grants-for-tool. Check immutability
	// after EACH rejected call so a mutation in one isn't masked by a later call.
	for _, args := range [][]string{
		{"sandbox", "grants", "allow", "write_file", "--path", ""},
		{"sandbox", "grants", "allow", "write_file", "--path="},
		{"sandbox", "grants", "revoke", "write_file", "--path", ""},
		{"sandbox", "grants", "revoke", "write_file", "--path="},
	} {
		before, err := store.List()
		if err != nil {
			t.Fatalf("%v: List before: %v", args, err)
		}
		if exit := run(args...); exit == exitSuccess {
			t.Fatalf("%v: expected a usage error for an empty --path, got success", args)
		}
		// Either rejection path is acceptable (the `--path=` form hits the
		// non-empty check; the `--path ""` form is rejected as a missing value) —
		// what matters is that an empty --path never silently widens scope.
		if !strings.Contains(stderr.String(), "path") {
			t.Fatalf("%v: stderr should explain the empty --path, got %q", args, stderr.String())
		}
		after, err := store.List()
		if err != nil {
			t.Fatalf("%v: List after: %v", args, err)
		}
		if !reflect.DeepEqual(after, before) {
			t.Fatalf("%v: rejected call mutated grants; before=%#v after=%#v", args, before, after)
		}
	}

	// Only the seeded grant remains — nothing added or removed by the rejected calls.
	grants, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(grants) != 1 || grants[0].ScopeKind != sandbox.ScopeToolWide {
		t.Fatalf("expected only the seeded tool-wide grant to remain, got %#v", grants)
	}
}

func TestRunSandboxPolicyInspectTextAndJSON(t *testing.T) {
	store := newSandboxTestStore(t)
	deps := appDeps{
		getwd:           func() (string, error) { return t.TempDir(), nil },
		newSandboxStore: func() (*sandbox.GrantStore, error) { return store, nil },
		resolveConfig: func(string, config.Overrides) (config.ResolvedConfig, error) {
			return config.ResolvedConfig{}, nil
		},
		selectSandboxBackend: func(options sandbox.BackendOptions) sandbox.Backend {
			return sandbox.Backend{
				Name:     sandbox.BackendUnavailable,
				Platform: "windows",
				Fallback: true,
				Message:  "Windows sandbox command runner is not available",
			}
		},
	}

	for _, args := range [][]string{
		{"sandbox", "policy"},
		{"sandbox", "policy", "--json"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var stdout bytes.Buffer
			var stderr bytes.Buffer
			exitCode := runWithDeps(args, &stdout, &stderr, deps)
			if exitCode != exitSuccess {
				t.Fatalf("policy exit = %d, stderr %q", exitCode, stderr.String())
			}
			if stderr.Len() != 0 {
				t.Fatalf("expected empty stderr, got %q", stderr.String())
			}
			if strings.Contains(strings.Join(args, " "), "--json") {
				var payload struct {
					Policy  sandbox.Policy  `json:"policy"`
					Backend sandbox.Backend `json:"backend"`
					Plan    struct {
						TargetBackend    sandbox.BackendName         `json:"targetBackend"`
						CommandWrapped   bool                        `json:"commandWrapped"`
						EnforcementLevel sandbox.EnforcementLevel    `json:"enforcementLevel"`
						DowngradeReason  string                      `json:"downgradeReason"`
						SupportLevel     string                      `json:"supportLevel"`
						Capabilities     []sandbox.BackendCapability `json:"capabilities"`
						Restrictions     []string                    `json:"restrictions"`
						Warnings         []string                    `json:"warnings"`
					} `json:"plan"`
					Grants string `json:"grantsPath"`
				}
				if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
					t.Fatalf("decode policy JSON: %v\n%s", err, stdout.String())
				}
				if payload.Policy.Mode != sandbox.ModeEnforce || payload.Backend.Name != sandbox.BackendUnavailable || payload.Grants == "" {
					t.Fatalf("unexpected policy JSON: %#v", payload)
				}
				if payload.Backend.Platform != "windows" || !payload.Backend.Fallback || payload.Backend.NativeIsolation || payload.Backend.CommandWrapping {
					t.Fatalf("unexpected backend capability JSON: %#v", payload.Backend)
				}
				if payload.Plan.SupportLevel != string(sandbox.BackendSupportUnavailable) {
					t.Fatalf("support level = %q, want unavailable", payload.Plan.SupportLevel)
				}
				if payload.Plan.TargetBackend != sandbox.BackendWindowsRestrictedToken || payload.Plan.CommandWrapped || payload.Plan.EnforcementLevel != sandbox.EnforcementDegraded || payload.Plan.DowngradeReason == "" {
					t.Fatalf("unexpected manager baseline fields: %#v", payload.Plan)
				}
				if sandboxPolicyCapabilityStatus(payload.Plan.Capabilities, "native_process_isolation") != sandbox.CapabilityUnavailable {
					t.Fatalf("expected native isolation unavailable, got %#v", payload.Plan.Capabilities)
				}
				if !sandboxPolicyRestrictionContains(payload.Plan.Restrictions, "native process isolation unavailable on windows") {
					t.Fatalf("expected JSON plan to document Windows fallback, got %#v", payload.Plan.Restrictions)
				}
				if !sandboxPolicyRestrictionContains(payload.Plan.Warnings, "Windows sandbox command runner is not available") {
					t.Fatalf("expected JSON warnings to document Windows fallback, got %#v", payload.Plan.Warnings)
				}
			} else {
				output := stdout.String()
				for _, want := range []string{
					"Zero sandbox policy",
					"backend: unavailable",
					"target_backend: windows-restricted-token",
					"support_level: unavailable",
					"command_wrapped: false",
					"enforcement_level: degraded",
					"downgrade_reason: Windows sandbox command runner is not available",
					"backend_fallback: true",
					"backend_command_wrapping: false",
					"backend_native_isolation: false",
					"backend_platform: windows",
					"Windows sandbox command runner is not available",
				} {
					if !strings.Contains(output, want) {
						t.Fatalf("expected policy text to contain %q, got %q", want, output)
					}
				}
			}
		})
	}
}

func TestHiddenWindowsSandboxSubcommandsSelfDispatch(t *testing.T) {
	// The runner/setup sentinels are routed before normal CLI parsing, straight
	// into the sandbox helper mains — never the unknown-command path. With no
	// helper-specific flags they fail their own arg validation (non-zero), but
	// crucially the message is the helper's, proving the dispatch.
	for _, tc := range []struct {
		name string
		arg  string
		want string
	}{
		{"runner", sandbox.WindowsCommandRunnerSubcommand, sandbox.WindowsSandboxCommandRunnerName},
		{"setup", sandbox.WindowsSandboxSetupSubcommand, sandbox.WindowsSandboxSetupName},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			exit := runWithDeps([]string{tc.arg}, &stdout, &stderr, appDeps{})
			if exit == 0 {
				t.Fatalf("expected non-zero exit from the helper's own arg validation, got 0")
			}
			if !strings.Contains(stderr.String(), tc.want) {
				t.Fatalf("stderr = %q, want the %s helper's message (proves self-dispatch)", stderr.String(), tc.want)
			}
			if strings.Contains(stderr.String(), "unknown command") {
				t.Fatalf("sentinel %q fell through to normal CLI dispatch: %q", tc.arg, stderr.String())
			}
		})
	}
}

func TestRunSandboxSetupRunsWindowsHelper(t *testing.T) {
	workspace := t.TempDir()
	runnerDir := t.TempDir()
	runnerPath := filepath.Join(runnerDir, sandbox.WindowsSandboxCommandRunnerName)
	var gotPath string
	var gotArgs []string
	deps := appDeps{
		getwd: func() (string, error) { return workspace, nil },
		resolveConfig: func(string, config.Overrides) (config.ResolvedConfig, error) {
			return config.ResolvedConfig{
				Sandbox: config.SandboxConfig{Network: "allow"},
			}, nil
		},
		selectSandboxBackend: func(options sandbox.BackendOptions) sandbox.Backend {
			return sandbox.Backend{
				Name:            sandbox.BackendWindowsRestrictedToken,
				Available:       true,
				Platform:        "windows",
				Executable:      runnerPath,
				CommandWrapping: true,
				NativeIsolation: true,
			}
		},
		runSandboxSetupHelper: func(path string, args []string, stdout io.Writer, stderr io.Writer) error {
			gotPath = path
			gotArgs = append([]string{}, args...)
			return nil
		},
	}

	var stdout, stderr bytes.Buffer
	exitCode := runWithDeps([]string{"sandbox", "setup", "--json"}, &stdout, &stderr, deps)
	if exitCode != exitSuccess {
		t.Fatalf("setup exit = %d, stderr %q", exitCode, stderr.String())
	}
	// The setup helper is now resolved independently of backend.Executable: an
	// adjacent zero-windows-sandbox-setup.exe in release, else self-dispatch via
	// the running binary. Under `go test` no sibling helper exists, so it
	// self-dispatches: path is the running binary and the first arg is the hidden
	// setup subcommand, followed by the real setup args.
	wantPath, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	if gotPath != wantPath {
		t.Fatalf("setup helper path = %q, want self-dispatch binary %q", gotPath, wantPath)
	}
	if len(gotArgs) == 0 || gotArgs[0] != sandbox.WindowsSandboxSetupSubcommand {
		t.Fatalf("setup args = %#v, want leading %q subcommand", gotArgs, sandbox.WindowsSandboxSetupSubcommand)
	}
	config, err := sandbox.ParseWindowsSandboxSetupArgs(gotArgs[1:])
	if err != nil {
		t.Fatalf("ParseWindowsSandboxSetupArgs: %v", err)
	}
	if config.CommandCWD != workspace || len(config.WorkspaceRoots) != 1 || config.WorkspaceRoots[0] != workspace {
		t.Fatalf("setup args config = %#v, want workspace cwd/root", config)
	}
	if config.PermissionProfile.Network.Mode != sandbox.NetworkAllow {
		t.Fatalf("setup network profile = %#v, want allow", config.PermissionProfile.Network)
	}
	var payload sandboxSetupResult
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("decode setup JSON: %v\n%s", err, stdout.String())
	}
	if !payload.Ran || payload.Helper != wantPath || payload.Backend != sandbox.BackendWindowsRestrictedToken {
		t.Fatalf("setup payload = %#v, want ran Windows helper", payload)
	}
}

func TestRunSandboxSetupNoopsForNonWindowsBackend(t *testing.T) {
	deps := appDeps{
		getwd: func() (string, error) { return t.TempDir(), nil },
		resolveConfig: func(string, config.Overrides) (config.ResolvedConfig, error) {
			return config.ResolvedConfig{}, nil
		},
		selectSandboxBackend: func(options sandbox.BackendOptions) sandbox.Backend {
			return sandbox.Backend{Name: sandbox.BackendLinuxBwrap, Platform: "linux", Available: true}
		},
		runSandboxSetupHelper: func(string, []string, io.Writer, io.Writer) error {
			t.Fatal("setup helper should not run for non-Windows backend")
			return nil
		},
	}

	var stdout, stderr bytes.Buffer
	exitCode := runWithDeps([]string{"sandbox", "setup"}, &stdout, &stderr, deps)
	if exitCode != exitSuccess {
		t.Fatalf("setup exit = %d, stderr %q", exitCode, stderr.String())
	}
	if !strings.Contains(stdout.String(), "No native sandbox setup is required for linux") {
		t.Fatalf("setup output = %q, want non-Windows no-op message", stdout.String())
	}
}

func TestTUISandboxSetupCommandGatedToWindowsNativeBackend(t *testing.T) {
	if got := tuiSandboxSetupCommand(sandbox.Backend{Name: sandbox.BackendLinuxBwrap, Platform: "linux", Available: true}, appDeps{}); got != nil {
		t.Fatal("linux backend should not enable the TUI sandbox setup command")
	}
	if got := tuiSandboxSetupCommand(sandbox.Backend{Name: sandbox.BackendUnavailable, Platform: "windows", Available: false}, appDeps{}); got != nil {
		t.Fatal("unavailable Windows backend should not enable the TUI sandbox setup command")
	}
	got := tuiSandboxSetupCommand(sandbox.Backend{
		Name:            sandbox.BackendWindowsRestrictedToken,
		Platform:        "windows",
		Available:       true,
		Executable:      filepath.Join(t.TempDir(), sandbox.WindowsSandboxCommandRunnerName),
		CommandWrapping: true,
		NativeIsolation: true,
	}, appDeps{})
	if got == nil {
		t.Fatal("native Windows backend should enable the TUI sandbox setup command")
	}
}

func TestRunSandboxPolicyJSONGoldenIncludesManagerBaselineFields(t *testing.T) {
	// Pin before any t.TempDir so an ambient temp dir cannot reach the snapshot.
	// Unix reads TMPDIR (and always considers /tmp). Windows reads TEMP and TMP.
	// XDG_RUNTIME_DIR is not a temp write-root candidate.
	pinSandboxPolicyGoldenTempEnv(t)
	assertSandboxPolicyJSONGolden(t, "")
}

// TestRunSandboxPolicyJSONGoldenNestedTempDir stages the temp-dir shape from
// #1121 and then pins over it. On Unix that shape is a TMPDIR inside /tmp; on
// Windows it is TMP inside TEMP. NewScope drops the inner root, the policy
// JSON still lists it, and the golden normalizer leaves it behind. The pin
// replaces those variables before the snapshot, so the golden matches and the
// nested path does not survive. The directories come from t.TempDir, never
// from the package working directory, so a checkout under /tmp is not special.
func TestRunSandboxPolicyJSONGoldenNestedTempDir(t *testing.T) {
	nested := stageNestedSandboxTempEnv(t)
	pinSandboxPolicyGoldenTempEnv(t)
	normalized := assertSandboxPolicyJSONGolden(t, "")
	if strings.Contains(normalized, nested) || strings.Contains(normalized, filepath.ToSlash(nested)) {
		t.Fatalf("normalized policy leaked nested temp root %q:\n%s", nested, normalized)
	}
}

// stageNestedSandboxTempEnv installs the leaking temp-dir shape and returns
// the inner path. The caller pins afterward. Skip, rather than fail, when
// that shape cannot be built.
func stageNestedSandboxTempEnv(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		outer := t.TempDir()
		inner := filepath.Join(outer, "inner")
		if err := os.MkdirAll(inner, 0o755); err != nil {
			t.Skipf("create nested TEMP/TMP: %v", err)
		}
		t.Setenv("TEMP", outer)
		t.Setenv("TMP", inner)
		return inner
	}
	resolvedTmp, err := filepath.EvalSymlinks("/tmp")
	if err != nil {
		t.Skipf("resolve /tmp: %v", err)
	}
	info, err := os.Stat(resolvedTmp)
	if err != nil || !info.IsDir() {
		t.Skip("/tmp is not a directory; cannot stage a nested TMPDIR")
	}
	// t.TempDir is a child of os.TempDir(). A further child is the ambient
	// case: TMPDIR inside the hard-coded /tmp candidate.
	nested := filepath.Join(t.TempDir(), "nested")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Skipf("create nested TMPDIR: %v", err)
	}
	resolvedNested, err := filepath.EvalSymlinks(nested)
	if err != nil {
		t.Skipf("resolve nested TMPDIR: %v", err)
	}
	rel, err := filepath.Rel(resolvedTmp, resolvedNested)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		t.Skipf("temp dir %q is not inside /tmp; cannot stage the nested TMPDIR case", resolvedNested)
	}
	t.Setenv("TMPDIR", resolvedNested)
	return resolvedNested
}

// pinSandboxPolicyGoldenTempEnv fixes the temp variables
// defaultTempWriteRootCandidatesForGOOS reads so the golden does not depend on
// the process environment. An empty TMPDIR leaves the Unix hard-coded /tmp as
// the only candidate. TEMP and TMP name one directory so neither is nested in
// the other.
func pinSandboxPolicyGoldenTempEnv(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		dir := t.TempDir()
		t.Setenv("TEMP", dir)
		t.Setenv("TMP", dir)
		return
	}
	t.Setenv("TMPDIR", "")
}

func assertSandboxPolicyJSONGolden(t *testing.T, mustList string) string {
	t.Helper()
	// Point all user-config roots at an empty directory so the platform-specific
	// credential deny baseline can be asserted without leaking host paths into
	// the platform-neutral golden comparison.
	emptyHome := t.TempDir()
	t.Setenv("HOME", emptyHome)
	t.Setenv("USERPROFILE", emptyHome)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(emptyHome, ".config"))
	t.Setenv("CLOUDSDK_CONFIG", "")
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "")
	t.Setenv("NPM_CONFIG_USERCONFIG", "")
	t.Setenv("npm_config_userconfig", "")
	t.Setenv("GH_CONFIG_DIR", "")
	t.Setenv("NETRC", "")
	t.Setenv("DOCKER_CONFIG", "")
	t.Setenv("KUBECONFIG", "")
	t.Setenv("ZERO_OAUTH_TOKENS_PATH", "")
	t.Setenv("ZERO_MCP_OAUTH_TOKENS_PATH", "")
	store := newSandboxTestStore(t)
	workspace := t.TempDir()
	deps := appDeps{
		getwd:           func() (string, error) { return workspace, nil },
		newSandboxStore: func() (*sandbox.GrantStore, error) { return store, nil },
		resolveConfig: func(string, config.Overrides) (config.ResolvedConfig, error) {
			return config.ResolvedConfig{}, nil
		},
		selectSandboxBackend: func(options sandbox.BackendOptions) sandbox.Backend {
			return sandbox.Backend{
				Name:     sandbox.BackendUnavailable,
				Platform: "windows",
				Fallback: true,
				Message:  "Windows sandbox command runner is not available",
			}
		},
	}

	var stdout, stderr bytes.Buffer
	exitCode := runWithDeps([]string{"sandbox", "policy", "--json"}, &stdout, &stderr, deps)
	if exitCode != exitSuccess {
		t.Fatalf("policy exit = %d, stderr %q", exitCode, stderr.String())
	}

	raw := stdout.String()
	got := replacePathToken(raw, workspace, "$WORKSPACE")
	got = replacePathToken(got, store.FilePath(), "$GRANTS")
	gotBytes := normalizeSandboxPolicyGoldenTempRoots(t, []byte(got), workspace, emptyHome)
	wantBytes, err := os.ReadFile(filepath.Join("testdata", "sandbox_policy_windows_unavailable.golden.json"))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if !jsonValuesEqual(t, wantBytes, gotBytes) {
		t.Fatalf("policy JSON golden mismatch\nwant:\n%s\ngot:\n%s", string(wantBytes), string(gotBytes))
	}
	if mustList != "" && !strings.Contains(raw, mustList) {
		t.Fatalf("policy JSON did not list pinned temp root %q\n%s", mustList, raw)
	}
	return string(gotBytes)
}

func normalizeSandboxPolicyGoldenTempRoots(t *testing.T, gotBytes []byte, workspace string, emptyHome string) []byte {
	t.Helper()
	scope, err := sandbox.NewScope(workspace, nil)
	if err != nil {
		t.Fatalf("NewScope(%q): %v", workspace, err)
	}
	roots := scope.Roots()
	if len(roots) <= 1 {
		return gotBytes
	}
	tempRoots := map[string]struct{}{}
	for _, root := range roots[1:] {
		tempRoots[root] = struct{}{}
	}
	var value any
	if err := json.Unmarshal(gotBytes, &value); err != nil {
		t.Fatalf("decode got JSON for normalization: %v\n%s", err, string(gotBytes))
	}
	plan, _ := value.(map[string]any)["plan"].(map[string]any)
	profile, _ := plan["permissionProfile"].(map[string]any)
	fileSystem, _ := profile["fileSystem"].(map[string]any)
	wantDenyRead := []string(nil)
	credentialHome := emptyHome
	if runtime.GOOS != "windows" {
		if resolved, err := filepath.EvalSymlinks(emptyHome); err == nil {
			credentialHome = resolved
		}
		wantDenyRead = []string{
			filepath.Join(credentialHome, ".aws"),
			filepath.Join(credentialHome, ".azure"),
			// git's cleartext credential stores, in both the home and XDG
			// layouts (#816). Listed here so the exported policy JSON is what
			// catches a regression: this baseline is the contract a user reads
			// with `zero sandbox policy --json`.
			filepath.Join(credentialHome, ".git-credentials"),
			filepath.Join(credentialHome, ".config", "git", "credentials"),
			filepath.Join(credentialHome, ".npmrc"),
			filepath.Join(credentialHome, ".netrc"),
			filepath.Join(credentialHome, ".kube", "config"),
			filepath.Join(credentialHome, ".docker", "config.json"),
			filepath.Join(credentialHome, ".config", "gh", "hosts.yml"),
			filepath.Join(credentialHome, ".config", "gcloud"),
			filepath.Join(credentialHome, ".config", "zero"),
		}
	}
	gotDenyRead := jsonStringSlice(fileSystem["denyReadIfExists"])
	sort.Strings(gotDenyRead)
	sort.Strings(wantDenyRead)
	if !reflect.DeepEqual(gotDenyRead, wantDenyRead) {
		t.Fatalf("manager credential deny baseline = %#v, want %#v", gotDenyRead, wantDenyRead)
	}
	wantCarveouts := []string(nil)
	wantEnsureDirs := []string(nil)
	if runtime.GOOS != "windows" {
		zeroDir := filepath.Join(credentialHome, ".config", "zero")
		wantCarveouts = []string{
			filepath.Join(zeroDir, "plugins"),
			filepath.Join(zeroDir, "specialists"),
			filepath.Join(zeroDir, "commands"),
		}
		wantEnsureDirs = []string{zeroDir}
	}
	if gotCarveouts := jsonStringSlice(fileSystem["denyReadCarveouts"]); !reflect.DeepEqual(gotCarveouts, wantCarveouts) {
		t.Fatalf("manager credential carveouts = %#v, want %#v", gotCarveouts, wantCarveouts)
	}
	if gotEnsureDirs := jsonStringSlice(fileSystem["ensureDenyReadDirs"]); !reflect.DeepEqual(gotEnsureDirs, wantEnsureDirs) {
		t.Fatalf("manager credential ensure dirs = %#v, want %#v", gotEnsureDirs, wantEnsureDirs)
	}
	delete(fileSystem, "denyReadIfExists")
	delete(fileSystem, "denyReadCarveouts")
	delete(fileSystem, "ensureDenyReadDirs")
	fileSystem["readRoots"] = filterJSONStringRoots(fileSystem["readRoots"], tempRoots)
	fileSystem["writeRoots"] = filterJSONWriteRoots(fileSystem["writeRoots"], tempRoots)
	normalized, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatalf("encode normalized policy JSON: %v", err)
	}
	return append(normalized, '\n')
}

func jsonStringSlice(value any) []string {
	values, ok := value.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(values))
	for _, value := range values {
		text, ok := value.(string)
		if !ok {
			return nil
		}
		out = append(out, text)
	}
	return out
}

func filterJSONStringRoots(value any, excluded map[string]struct{}) any {
	roots, ok := value.([]any)
	if !ok {
		return value
	}
	out := make([]any, 0, len(roots))
	for _, root := range roots {
		text, ok := root.(string)
		if !ok {
			out = append(out, root)
			continue
		}
		if _, skip := excluded[text]; !skip {
			out = append(out, root)
		}
	}
	return out
}

func filterJSONWriteRoots(value any, excluded map[string]struct{}) any {
	roots, ok := value.([]any)
	if !ok {
		return value
	}
	out := make([]any, 0, len(roots))
	for _, root := range roots {
		entry, ok := root.(map[string]any)
		if !ok {
			out = append(out, root)
			continue
		}
		text, _ := entry["root"].(string)
		if _, skip := excluded[text]; !skip {
			out = append(out, root)
		}
	}
	return out
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func jsonValuesEqual(t *testing.T, wantBytes []byte, gotBytes []byte) bool {
	t.Helper()
	var wantValue any
	if err := json.Unmarshal(wantBytes, &wantValue); err != nil {
		t.Fatalf("decode wanted JSON: %v", err)
	}
	var gotValue any
	if err := json.Unmarshal(gotBytes, &gotValue); err != nil {
		t.Fatalf("decode got JSON: %v\n%s", err, string(gotBytes))
	}
	normalizePortableJSONRootString(&wantValue)
	normalizePortableJSONRootString(&gotValue)
	return reflect.DeepEqual(wantValue, gotValue)
}

func normalizePortableJSONRootString(value *any) {
	switch current := (*value).(type) {
	case string:
		// Normalize OS-specific path separators so the shared golden file
		// matches on both Unix (slash) and Windows (backslash) runners.
		// readOnlySubpaths carve-outs are built with filepath.Join, which
		// renders as backslash paths on Windows.
		if current != "" {
			*value = strings.ReplaceAll(current, `\`, "/")
		}
	case []any:
		for index := range current {
			normalizePortableJSONRootString(&current[index])
		}
	case map[string]any:
		for key := range current {
			entry := current[key]
			normalizePortableJSONRootString(&entry)
			current[key] = entry
		}
	}
}

func replacePathToken(value string, path string, token string) string {
	replace := func(value string, path string) string {
		value = strings.ReplaceAll(value, path, token)
		if encoded, err := json.Marshal(path); err == nil && len(encoded) >= 2 {
			value = strings.ReplaceAll(value, string(encoded[1:len(encoded)-1]), token)
		}
		return value
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil && resolved != path {
		value = replace(value, resolved)
	}
	return replace(value, path)
}

func TestRunSandboxPolicyEffectiveTextAndJSON(t *testing.T) {
	store := newSandboxTestStore(t)
	deps := appDeps{
		getwd:           func() (string, error) { return t.TempDir(), nil },
		newSandboxStore: func() (*sandbox.GrantStore, error) { return store, nil },
		resolveConfig: func(string, config.Overrides) (config.ResolvedConfig, error) {
			return config.ResolvedConfig{}, nil
		},
		selectSandboxBackend: func(options sandbox.BackendOptions) sandbox.Backend {
			return sandbox.Backend{
				Name:     sandbox.BackendUnavailable,
				Platform: "darwin",
				Fallback: true,
				Message:  "native sandbox unavailable",
			}
		},
	}

	t.Run("text", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		exitCode := runWithDeps([]string{"sandbox", "policy", "--effective"}, &stdout, &stderr, deps)
		if exitCode != exitSuccess {
			t.Fatalf("effective exit = %d, stderr %q", exitCode, stderr.String())
		}
		output := stdout.String()
		for _, want := range []string{
			"Zero effective sandbox policy",
			"mode: enforce",
			"network: deny",
			"enforce_workspace: true",
			"interactive_command_guard: enabled",
			"support_level: unavailable",
		} {
			if !strings.Contains(output, want) {
				t.Fatalf("effective text missing %q, got %q", want, output)
			}
		}
	})

	t.Run("json", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		exitCode := runWithDeps([]string{"sandbox", "policy", "--effective", "--json"}, &stdout, &stderr, deps)
		if exitCode != exitSuccess {
			t.Fatalf("effective json exit = %d, stderr %q", exitCode, stderr.String())
		}
		var payload struct {
			Policy struct {
				Mode             string `json:"mode"`
				Network          string `json:"network"`
				EnforceWorkspace bool   `json:"enforceWorkspace"`
			} `json:"policy"`
			Backend struct {
				Name string `json:"name"`
			} `json:"backend"`
			Plan struct {
				SupportLevel string `json:"supportLevel"`
			} `json:"plan"`
			Guards struct {
				InteractiveCommand bool `json:"interactiveCommand"`
				Network            bool `json:"network"`
				Workspace          bool `json:"workspace"`
			} `json:"guards"`
			GrantsPath            string `json:"grantsPath"`
			PermissionProfileNote string `json:"permissionProfileNote"`
		}
		if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
			t.Fatalf("decode effective JSON: %v\n%s", err, stdout.String())
		}
		if payload.Policy.Mode != "enforce" || payload.Policy.Network != "deny" {
			t.Fatalf("unexpected effective policy: %#v", payload.Policy)
		}
		if !payload.Policy.EnforceWorkspace {
			t.Fatalf("expected workspace guard enabled: %#v", payload.Policy)
		}
		if !payload.Guards.InteractiveCommand {
			t.Fatalf("expected guards reported: %#v", payload.Guards)
		}
		if payload.Plan.SupportLevel != string(sandbox.BackendSupportUnavailable) || payload.GrantsPath == "" {
			t.Fatalf("unexpected effective plan/grants: %#v %q", payload.Plan, payload.GrantsPath)
		}
		if payload.PermissionProfileNote != permissionProfileScopeNote {
			t.Fatalf("permissionProfileNote = %q, want %q", payload.PermissionProfileNote, permissionProfileScopeNote)
		}
	})
}

func TestEffectiveSandboxPolicyListsWriteRoots(t *testing.T) {
	output := formatEffectiveSandboxPolicy("/ws", sandbox.DefaultPolicy(), sandbox.Backend{}, sandbox.BackendPlan{}, resolveSandboxGuards(sandbox.DefaultPolicy()), "/grants", []string{"/ws", "/extra"}, nil)
	if !strings.Contains(output, "write_roots: /ws, /extra") {
		t.Fatalf("expected write_roots line, got:\n%s", output)
	}
	if !strings.Contains(output, "enforce_workspace: true\nwrite_roots: /ws, /extra") {
		t.Fatalf("write_roots should directly follow enforce_workspace, got:\n%s", output)
	}
	if !strings.Contains(output, "permission_profile_note: "+permissionProfileScopeNote) {
		t.Fatalf("effective text missing permission profile scope warning:\n%s", output)
	}
	if strings.Contains(output, "write_roots_error") {
		t.Fatalf("unexpected write_roots_error line without an error:\n%s", output)
	}
}

func TestEffectiveSandboxPolicyShowsWriteRootsError(t *testing.T) {
	scopeErr := errors.New(`write root "/gone": write root must exist`)
	output := formatEffectiveSandboxPolicy("/ws", sandbox.DefaultPolicy(), sandbox.Backend{}, sandbox.BackendPlan{}, resolveSandboxGuards(sandbox.DefaultPolicy()), "/grants", []string{"/ws"}, scopeErr)
	if !strings.Contains(output, "write_roots: /ws") {
		t.Fatalf("expected fallback write_roots line, got:\n%s", output)
	}
	if !strings.Contains(output, `write_roots_error: write root "/gone": write root must exist`) {
		t.Fatalf("expected write_roots_error line, got:\n%s", output)
	}
}

func TestRunSandboxPolicyEffectiveListsConfiguredWriteRoots(t *testing.T) {
	store := newSandboxTestStore(t)
	extra := tempDirOutsideDefaultTemp(t)
	resolvedExtra, err := filepath.EvalSymlinks(extra)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q) returned error: %v", extra, err)
	}
	deps := appDeps{
		getwd:           func() (string, error) { return t.TempDir(), nil },
		newSandboxStore: func() (*sandbox.GrantStore, error) { return store, nil },
		resolveConfig: func(string, config.Overrides) (config.ResolvedConfig, error) {
			return config.ResolvedConfig{Sandbox: config.SandboxConfig{AdditionalWriteRoots: []string{extra}}}, nil
		},
	}

	var stdout, stderr bytes.Buffer
	if code := runWithDeps([]string{"sandbox", "policy", "--effective"}, &stdout, &stderr, deps); code != exitSuccess {
		t.Fatalf("effective exit = %d, stderr %q", code, stderr.String())
	}
	output := stdout.String()
	if !strings.Contains(output, "write_roots: ") {
		t.Fatalf("effective text missing write_roots line:\n%s", output)
	}
	if !strings.Contains(output, resolvedExtra) {
		t.Fatalf("write_roots should include the configured extra root %q:\n%s", resolvedExtra, output)
	}
	if strings.Contains(output, "write_roots_error") {
		t.Fatalf("unexpected write_roots_error for valid roots:\n%s", output)
	}

	stdout.Reset()
	stderr.Reset()
	if code := runWithDeps([]string{"sandbox", "policy", "--effective", "--json"}, &stdout, &stderr, deps); code != exitSuccess {
		t.Fatalf("effective json exit = %d, stderr %q", code, stderr.String())
	}
	var payload struct {
		WriteRoots []string `json:"writeRoots"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("decode effective JSON: %v\n%s", err, stdout.String())
	}
	if len(payload.WriteRoots) < 2 {
		t.Fatalf("writeRoots = %#v, want workspace root, extra root, and default temp roots", payload.WriteRoots)
	}
	if !containsString(payload.WriteRoots, resolvedExtra) {
		t.Fatalf("writeRoots = %#v, want configured extra root %q", payload.WriteRoots, resolvedExtra)
	}
	if strings.Contains(stdout.String(), "writeRootsError") {
		t.Fatalf("unexpected writeRootsError key for valid roots:\n%s", stdout.String())
	}
}

func TestRunSandboxPolicyEffectiveWriteRootsFailSoft(t *testing.T) {
	store := newSandboxTestStore(t)
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	deps := appDeps{
		getwd:           func() (string, error) { return t.TempDir(), nil },
		newSandboxStore: func() (*sandbox.GrantStore, error) { return store, nil },
		resolveConfig: func(string, config.Overrides) (config.ResolvedConfig, error) {
			return config.ResolvedConfig{Sandbox: config.SandboxConfig{AdditionalWriteRoots: []string{missing}}}, nil
		},
	}

	var stdout, stderr bytes.Buffer
	if code := runWithDeps([]string{"sandbox", "policy", "--effective"}, &stdout, &stderr, deps); code != exitSuccess {
		t.Fatalf("effective exit = %d, want success (stale config must fail soft), stderr %q", code, stderr.String())
	}
	output := stdout.String()
	if !strings.Contains(output, "write_roots_error: ") {
		t.Fatalf("expected visible write_roots_error line for stale config entry:\n%s", output)
	}
	if !strings.Contains(output, "write_roots: ") {
		t.Fatalf("expected workspace-only write_roots fallback line:\n%s", output)
	}

	stdout.Reset()
	stderr.Reset()
	if code := runWithDeps([]string{"sandbox", "policy", "--effective", "--json"}, &stdout, &stderr, deps); code != exitSuccess {
		t.Fatalf("effective json exit = %d, want success (stale config must fail soft), stderr %q", code, stderr.String())
	}
	var payload struct {
		WriteRoots      []string `json:"writeRoots"`
		WriteRootsError string   `json:"writeRootsError"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("decode effective JSON: %v\n%s", err, stdout.String())
	}
	if payload.WriteRootsError == "" {
		t.Fatalf("expected writeRootsError in JSON for stale config entry:\n%s", stdout.String())
	}
	if !strings.Contains(payload.WriteRootsError, missing) {
		t.Fatalf("writeRootsError = %q, want it to name the stale root %q", payload.WriteRootsError, missing)
	}
	if len(payload.WriteRoots) == 0 {
		t.Fatalf("writeRoots = %#v, want workspace/default-temp fallback", payload.WriteRoots)
	}
	if containsString(payload.WriteRoots, missing) {
		t.Fatalf("writeRoots = %#v, must not include stale root %q", payload.WriteRoots, missing)
	}
}

func TestRunSandboxPolicyEffectiveHelpListed(t *testing.T) {
	var stdout, stderr bytes.Buffer
	exitCode := runWithDeps([]string{"sandbox", "policy", "--help"}, &stdout, &stderr, appDeps{})
	if exitCode != exitSuccess {
		t.Fatalf("help exit = %d, stderr %q", exitCode, stderr.String())
	}
	if !strings.Contains(stdout.String(), "--effective") {
		t.Fatalf("policy help should document --effective, got %q", stdout.String())
	}
}

func TestRunSandboxHelpDoesNotOpenStore(t *testing.T) {
	deps := appDeps{newSandboxStore: func() (*sandbox.GrantStore, error) {
		t.Fatal("newSandboxStore should not be called for help")
		return nil, nil
	}}
	for _, args := range [][]string{
		{"sandbox", "--help"},
		{"sandbox", "grants", "--help"},
		{"sandbox", "grants", "allow", "--help"},
		{"sandbox", "policy", "--help"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var stdout bytes.Buffer
			var stderr bytes.Buffer
			exitCode := runWithDeps(args, &stdout, &stderr, deps)
			if exitCode != exitSuccess {
				t.Fatalf("help exit = %d, stderr %q", exitCode, stderr.String())
			}
			if stdout.Len() == 0 {
				t.Fatalf("expected help output")
			}
		})
	}
}

func TestRunSandboxPolicyTextOmitsMaxAutonomy(t *testing.T) {
	store := newSandboxTestStore(t)
	deps := appDeps{
		newSandboxStore: func() (*sandbox.GrantStore, error) { return store, nil },
		resolveConfig: func(workspaceRoot string, overrides config.Overrides) (config.ResolvedConfig, error) {
			return config.ResolvedConfig{}, nil
		},
	}

	var stdout, stderr bytes.Buffer
	if code := runWithDeps([]string{"sandbox", "policy"}, &stdout, &stderr, deps); code != exitSuccess {
		t.Fatalf("policy exit = %d, stderr %q", code, stderr.String())
	}
	if strings.Contains(stdout.String(), "max_autonomy") {
		t.Fatalf("policy text should omit max_autonomy:\n%s", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := runWithDeps([]string{"sandbox", "policy", "--effective"}, &stdout, &stderr, deps); code != exitSuccess {
		t.Fatalf("effective policy exit = %d, stderr %q", code, stderr.String())
	}
	if strings.Contains(stdout.String(), "max_autonomy") {
		t.Fatalf("effective policy text should omit max_autonomy:\n%s", stdout.String())
	}
}

func TestRunSandboxPolicySurfacesResolveConfigError(t *testing.T) {
	store := newSandboxTestStore(t)
	deps := appDeps{
		getwd:           func() (string, error) { return t.TempDir(), nil },
		newSandboxStore: func() (*sandbox.GrantStore, error) { return store, nil },
		resolveConfig: func(string, config.Overrides) (config.ResolvedConfig, error) {
			return config.ResolvedConfig{}, fmt.Errorf("invalid sandbox.network %q", "maybe")
		},
	}

	var stdout, stderr bytes.Buffer
	exitCode := runWithDeps([]string{"sandbox", "policy"}, &stdout, &stderr, deps)
	if exitCode != exitProvider {
		t.Fatalf("policy exit = %d, want provider exit %d (resolve error surfaced, not silent DefaultPolicy fallback)", exitCode, exitProvider)
	}
	if !strings.Contains(stderr.String(), "invalid sandbox.network") {
		t.Fatalf("expected surfaced resolve error in stderr, got %q", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("expected empty stdout on resolve error, got %q", stdout.String())
	}
}

func TestApplyConfiguredSandboxPolicyDiagnosticsFlags(t *testing.T) {
	base := sandbox.DefaultPolicy()
	if base.BlockUnixSockets || base.MonitorDenials {
		t.Fatalf("precondition: diagnostic flags must default off")
	}

	// Omitted keys leave the (off) defaults untouched.
	if got := applyConfiguredSandboxPolicy(sandbox.DefaultPolicy(), config.SandboxConfig{}); got.BlockUnixSockets || got.MonitorDenials {
		t.Fatalf("empty config must not enable diagnostic flags: %#v", got)
	}

	// The flags opt in.
	got := applyConfiguredSandboxPolicy(sandbox.DefaultPolicy(), config.SandboxConfig{
		BlockUnixSockets: true,
		MonitorDenials:   true,
	})
	if !got.BlockUnixSockets || !got.MonitorDenials {
		t.Fatalf("diagnostic config not applied to policy: %#v", got)
	}
}

func newSandboxTestStore(t *testing.T) *sandbox.GrantStore {
	t.Helper()
	store, err := sandbox.NewGrantStore(sandbox.StoreOptions{
		FilePath: filepath.Join(t.TempDir(), "sandbox-grants.json"),
		Now:      fixedCLITime("2026-06-05T14:45:00Z"),
	})
	if err != nil {
		t.Fatalf("NewGrantStore returned error: %v", err)
	}
	return store
}

func sandboxPolicyRestrictionContains(restrictions []string, value string) bool {
	for _, restriction := range restrictions {
		if strings.Contains(restriction, value) {
			return true
		}
	}
	return false
}

func sandboxPolicyCapabilityStatus(capabilities []sandbox.BackendCapability, key string) sandbox.CapabilityStatus {
	for _, capability := range capabilities {
		if capability.Key == key {
			return capability.Status
		}
	}
	return ""
}

func TestApplyConfiguredSandboxPolicyEnabledFalseDisables(t *testing.T) {
	disabled := false
	if got := applyConfiguredSandboxPolicy(sandbox.DefaultPolicy(), config.SandboxConfig{Enabled: &disabled}); got.Mode != sandbox.ModeDisabled {
		t.Fatalf("enabled:false must yield ModeDisabled, got %q", got.Mode)
	}
	// Omitted (nil) and explicit true keep the default enforcing mode.
	if got := applyConfiguredSandboxPolicy(sandbox.DefaultPolicy(), config.SandboxConfig{}); got.Mode == sandbox.ModeDisabled {
		t.Fatal("omitted enabled must not disable the sandbox")
	}
	enabled := true
	if got := applyConfiguredSandboxPolicy(sandbox.DefaultPolicy(), config.SandboxConfig{Enabled: &enabled}); got.Mode == sandbox.ModeDisabled {
		t.Fatal("enabled:true must not disable the sandbox")
	}
}
