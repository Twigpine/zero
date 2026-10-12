package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"

	"github.com/Gitlawb/zero/internal/execution"
	"github.com/Gitlawb/zero/internal/providercatalog"
)

var errNativeSandboxUnavailable = errors.New("native sandbox backend is unavailable")

func nativeSandboxUnavailableError(backend Backend) error {
	message := strings.TrimSpace(backend.Message)
	if message == "" {
		return errNativeSandboxUnavailable
	}
	return fmt.Errorf("%w: %s", errNativeSandboxUnavailable, message)
}

type CommandSpec struct {
	Name string
	Args []string
	Dir  string
	Env  []string
	// sensitiveEnvKeys is populated by Engine from config-derived credential
	// variable names and carried through SandboxManager to the platform plan.
	sensitiveEnvKeys []string
}

type CommandPlan struct {
	Backend                 Backend           `json:"backend"`
	TargetBackend           BackendName       `json:"targetBackend"`
	WorkspaceRoot           string            `json:"workspaceRoot"`
	Policy                  Policy            `json:"policy"`
	PermissionProfile       PermissionProfile `json:"permissionProfile"`
	Wrapped                 bool              `json:"wrapped"`
	SandboxEnvMarkers       []string          `json:"sandboxEnvMarkers,omitempty"`
	EnforcementLevel        EnforcementLevel  `json:"enforcementLevel"`
	DowngradeReason         string            `json:"downgradeReason,omitempty"`
	RequiresPlatformSandbox bool              `json:"requiresPlatformSandbox"`
	Name                    string            `json:"name"`
	Args                    []string          `json:"args"`
	Dir                     string            `json:"dir,omitempty"`
	Env                     []string          `json:"env,omitempty"`
	SandboxDir              string            `json:"sandboxDir,omitempty"`
	// MonitorTag, when non-empty, is the unique marker embedded in the
	// sandbox-exec profile's denial messages; a caller passes it to
	// StartDenialMonitor to capture what the sandbox blocked. Empty unless
	// Policy.MonitorDenials is set on a macOS sandbox-exec plan.
	MonitorTag string `json:"monitorTag,omitempty"`
	// Notes records auditable least-privilege downgrade notes, such as native
	// isolation being unavailable in the selected environment. Surfaced to the
	// operator; never affects enforcement.
	Notes []string `json:"notes,omitempty"`
	// cleanup releases resources tied to the plan's lifetime. It is never
	// serialized; callers invoke it via Cleanup() once the command has finished.
	cleanup func()
	// executionReportPath is an adapter-owned side channel outside the
	// workspace. It carries structured policy facts; command output is never
	// parsed as the control protocol.
	executionReportPath string
}

// Cleanup releases any resources the plan holds. It is safe to call on a zero
// plan and to call more than once.
func (plan CommandPlan) Cleanup() {
	if plan.cleanup != nil {
		plan.cleanup()
	}
}

// ExecutionReport reads the structured platform-adapter report for a finished
// command. A command with no adapter report returns an empty report.
func (plan CommandPlan) ExecutionReport() (execution.AdapterReport, error) {
	if strings.TrimSpace(plan.executionReportPath) == "" {
		return execution.AdapterReport{}, nil
	}
	data, err := os.ReadFile(plan.executionReportPath)
	if os.IsNotExist(err) {
		return execution.AdapterReport{}, nil
	}
	if err != nil {
		return execution.AdapterReport{}, fmt.Errorf("read sandbox execution report: %w", err)
	}
	var report execution.AdapterReport
	if err := json.Unmarshal(data, &report); err != nil {
		return execution.AdapterReport{}, fmt.Errorf("decode sandbox execution report: %w", err)
	}
	return report, nil
}

func (engine *Engine) CommandContext(ctx context.Context, spec CommandSpec) (*exec.Cmd, CommandPlan, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	plan, err := engine.BuildCommandPlan(spec)
	if err != nil {
		return nil, CommandPlan{}, err
	}
	command := exec.CommandContext(ctx, plan.Name, plan.Args...)
	command.Dir = plan.Dir
	command.Env = plan.Env
	return command, plan, nil
}

// PrepareExecution adapts the platform-neutral execution interface to the
// sandbox engine. Callers never need to construct native sandbox commands or
// interpret adapter reports themselves.
func (engine *Engine) PrepareExecution(ctx context.Context, request execution.Request) (execution.PreparedCommand, error) {
	if err := request.Validate(); err != nil {
		return execution.PreparedCommand{}, err
	}
	command, plan, err := engine.CommandContext(ctx, CommandSpec{
		Name: request.Command.Name,
		Args: append([]string(nil), request.Command.Args...),
		Dir:  request.WorkingDirectory,
		Env:  append([]string(nil), request.Command.Env...),
	})
	if err != nil {
		return execution.PreparedCommand{}, err
	}
	return execution.PreparedCommand{
		Command: command,
		Enforcement: execution.Enforcement{
			Backend:         string(plan.TargetBackend),
			Level:           string(plan.EnforcementLevel),
			Degraded:        plan.EnforcementLevel == EnforcementDegraded,
			DowngradeReason: plan.DowngradeReason,
		},
		Report:  plan.ExecutionReport,
		Cleanup: plan.Cleanup,
	}, nil
}

func (engine *Engine) BuildCommandPlan(spec CommandSpec) (CommandPlan, error) {
	if engine == nil {
		return directCommandPlan(spec, Backend{Name: BackendUnavailable, Message: "sandbox disabled"}, Policy{}, ""), nil
	}
	policy := engine.effectivePolicy(engine.policy)
	if policy.Mode == "" {
		policy = DefaultPolicy()
	}
	workspaceRoot, commandDir, err := engine.resolveCommandDir(spec.Dir, policy)
	if err != nil {
		return CommandPlan{}, err
	}
	spec.Name = strings.TrimSpace(spec.Name)
	if spec.Name == "" {
		return CommandPlan{}, errors.New("sandbox command name is required")
	}
	spec.Dir = commandDir
	spec.sensitiveEnvKeys = engine.sensitiveEnvKeys

	backend := engine.backend
	if backend.Name == "" {
		backend = Backend{Name: BackendUnavailable, Message: "native sandbox backend was not selected"}
	}
	backend = inferBackendCapabilities(backend)
	preference := SandboxPreferenceAuto
	// Re-entrancy guard: a command spawned by a process we already wrapped (both
	// ZERO_SANDBOXED=1 and ZERO_SANDBOX_BACKEND set in its env — see
	// IsAlreadySandboxed) must not be wrapped again — nested platform wrappers
	// fail and a second sandbox wrapper would be redundant. Return a pass-through
	// plan.
	if IsAlreadySandboxed() {
		preference = SandboxPreferenceForbid
	}
	if policy.Mode == ModeDisabled {
		preference = SandboxPreferenceForbid
	}
	profile := permissionProfileFromPolicy(workspaceRoot, policy, engine.scope, spec.Dir, spec.Env)
	// Validate in the main process before creating runtime state or launching a
	// potentially version-skewed helper. Keep the helper check as defense in depth.
	if preference != SandboxPreferenceForbid && backend.Name == BackendLinuxBwrap && backend.Available && backend.CommandWrapping && backend.NativeIsolation {
		if err := validateLinuxBwrapPermissionProfile(profile); err != nil {
			return CommandPlan{}, err
		}
	}
	var runtimeCleanup func()
	if preference != SandboxPreferenceForbid && policy.Mode != ModeDisabled {
		runtimeState, cleanup, runtimeErr := prepareSandboxRuntime(workspaceRoot)
		if runtimeErr != nil {
			return CommandPlan{}, runtimeErr
		}
		runtimeCleanup = cleanup
		profile = permissionProfileWithRuntime(profile, runtimeState)
	}
	manager := NewSandboxManager(SandboxManagerOptions{
		GOOS:    backend.Platform,
		Backend: backend,
	})
	plan, err := manager.BuildCommandPlan(SandboxManagerRequest{
		WorkspaceRoot:     workspaceRoot,
		Command:           spec,
		Policy:            policy,
		Scope:             engine.scope,
		Profile:           profile,
		Preference:        preference,
		ValidateExecution: true,
	})
	if err != nil {
		if runtimeCleanup != nil {
			runtimeCleanup()
		}
		return CommandPlan{}, err
	}
	plan.cleanup = combineSandboxCleanups(plan.cleanup, runtimeCleanup)
	return plan, nil
}

func buildPlatformCommandPlan(execRequest SandboxExecutionRequest, policy Policy) (CommandPlan, error) {
	if policy.Mode == "" {
		policy = DefaultPolicy()
	}
	spec := execRequest.Command
	backend := execRequest.Backend
	workspaceRoot := execRequest.WorkspaceRoot
	if execRequest.EnforcementLevel == EnforcementDisabled || execRequest.EnforcementLevel == EnforcementDegraded || execRequest.TargetBackend == BackendNone || !execRequest.RequiresPlatformSandbox {
		return withSandboxExecutionMetadata(directCommandPlan(spec, backend, policy, workspaceRoot), execRequest), nil
	}
	switch backend.Name {
	case BackendLinuxBwrap:
		if backend.Available && backend.Executable != "" {
			return linuxSandboxHelperCommandPlan(execRequest, policy)
		}
	case BackendMacOSSeatbelt:
		if backend.Available && backend.Executable != "" {
			return withSandboxExecutionMetadata(seatbeltCommandPlan(execRequest, policy, backend), execRequest), nil
		}
	case BackendWindowsRestrictedToken:
		if backend.Available && backend.Executable != "" {
			return windowsRestrictedTokenCommandPlan(execRequest, policy)
		}
	case BackendWSL:
		return CommandPlan{}, nativeSandboxUnavailableError(backend)
	}
	return CommandPlan{}, nativeSandboxUnavailableError(backend)
}

func linuxSandboxHelperCommandPlan(execRequest SandboxExecutionRequest, policy Policy) (CommandPlan, error) {
	spec := execRequest.Command
	helper := LinuxSandboxHelperCommand{}
	if execRequest.Backend.Name == BackendLinuxBwrap && execRequest.Backend.Executable != "" {
		helper.Name = execRequest.Backend.Executable
	} else {
		resolved, err := linuxSandboxHelperCommand()
		if err != nil {
			return CommandPlan{}, err
		}
		helper = resolved
	}
	command := append([]string{spec.Name}, spec.Args...)
	reportPath, err := newLinuxExecutionReportPath()
	if err != nil {
		return CommandPlan{}, err
	}
	args, err := BuildLinuxSandboxCommandArgs(LinuxSandboxCommandArgsOptions{
		SandboxPolicyCWD:  execRequest.WorkspaceRoot,
		CommandCWD:        spec.Dir,
		PermissionProfile: execRequest.PermissionProfile,
		BlockUnixSockets:  policy.BlockUnixSockets,
		PolicyReportPath:  reportPath,
		Command:           command,
	})
	if err != nil {
		return CommandPlan{}, err
	}
	env := sandboxEnvironmentForCommandWithSensitiveEnv(spec.Env, policy, BackendLinuxBwrap, "", spec.sensitiveEnvKeys)
	env = sandboxRuntimeEnvironment(env, execRequest.PermissionProfile.Runtime)
	planDir := spec.Dir
	if helper.Dir != "" {
		planDir = helper.Dir
	}
	plan := CommandPlan{
		Backend:             execRequest.Backend,
		TargetBackend:       execRequest.TargetBackend,
		WorkspaceRoot:       execRequest.WorkspaceRoot,
		Policy:              policy,
		Wrapped:             true,
		SandboxEnvMarkers:   execRequest.SandboxEnvMarkers,
		EnforcementLevel:    execRequest.EnforcementLevel,
		Name:                helper.Name,
		Args:                append(append([]string{}, helper.ArgsPrefix...), args...),
		Dir:                 planDir,
		Env:                 env,
		SandboxDir:          spec.Dir,
		executionReportPath: reportPath,
		cleanup: func() {
			_ = os.Remove(reportPath)
		},
	}
	return withSandboxExecutionMetadata(plan, execRequest), nil
}

func newLinuxExecutionReportPath() (string, error) {
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", fmt.Errorf("generate sandbox execution report path: %w", err)
	}
	return filepath.Join("/tmp", "zero-sandbox-report-"+hex.EncodeToString(token[:])+".json"), nil
}

func withSandboxExecutionMetadata(plan CommandPlan, request SandboxExecutionRequest) CommandPlan {
	plan.Backend = request.Backend
	plan.TargetBackend = request.TargetBackend
	plan.PermissionProfile = request.PermissionProfile
	plan.SandboxEnvMarkers = request.SandboxEnvMarkers
	plan.EnforcementLevel = request.EnforcementLevel
	plan.DowngradeReason = request.DowngradeReason
	plan.RequiresPlatformSandbox = request.RequiresPlatformSandbox
	return plan
}

func directCommandPlan(spec CommandSpec, backend Backend, policy Policy, workspaceRoot string) CommandPlan {
	return CommandPlan{
		Backend:           backend,
		TargetBackend:     backend.TargetBackend(),
		WorkspaceRoot:     workspaceRoot,
		Policy:            policy,
		Wrapped:           false,
		SandboxEnvMarkers: backend.SandboxEnvMarkers(policy),
		EnforcementLevel:  backend.EnforcementLevel(policy),
		DowngradeReason:   backend.DowngradeReason(policy),
		Name:              spec.Name,
		Args:              cloneStrings(spec.Args),
		Dir:               spec.Dir,
		Env:               directCommandEnv(spec),
	}
}

// directCommandEnv scrubs sensitive credentials from the environment for a
// direct (unwrapped) command plan: the platform sandbox backend is
// unavailable, disabled, or not required, so this is the actual environment
// the child process inherits. Without scrubbing here, config-derived
// apiKeyEnv and dynamic OAuth client-secret variables would leak into
// commands that fall back to this path (e.g. EnforcementDegraded).
func directCommandEnv(spec CommandSpec) []string {
	env := cloneStrings(spec.Env)
	if spec.Env == nil {
		// Match the wrapped-plan behavior: an unset spec.Env means "inherit the
		// caller's environment," so scrub a snapshot of it rather than passing
		// nil through to exec.Cmd, which would skip scrubbing entirely.
		env = os.Environ()
	}
	return scrubSensitiveEnv(env, spec.sensitiveEnvKeys...)
}

func (engine *Engine) resolveCommandDir(dir string, policy Policy) (string, string, error) {
	workspaceRoot := strings.TrimSpace(engine.workspaceRoot)
	if workspaceRoot == "" {
		return "", "", errors.New("sandbox workspace root is required")
	}
	workspaceRoot = filepath.Clean(workspaceRoot)
	if !filepath.IsAbs(workspaceRoot) {
		absolute, err := filepath.Abs(workspaceRoot)
		if err != nil {
			return "", "", fmt.Errorf("resolve sandbox workspace: %w", err)
		}
		workspaceRoot = absolute
	}
	if resolved, err := filepath.EvalSymlinks(workspaceRoot); err == nil {
		workspaceRoot = resolved
	}

	commandDir := strings.TrimSpace(dir)
	if commandDir == "" {
		commandDir = workspaceRoot
	} else if !filepath.IsAbs(commandDir) {
		commandDir = filepath.Join(workspaceRoot, commandDir)
	}
	commandDir = filepath.Clean(commandDir)
	if resolved, err := filepath.EvalSymlinks(commandDir); err == nil {
		commandDir = resolved
	}
	if policy.EnforceWorkspace {
		if block := engine.scopeFor(engine.workspaceRoot).validate(commandDir); block != nil {
			return "", "", Block{
				Code:     block.Code,
				ToolName: "sandbox_command",
				Action:   ActionDeny,
				Risk: Risk{
					Level:      RiskCritical,
					Categories: []string{"path_escape"},
					Reason:     "critical risk: path_escape",
				},
				Path:   block.Path,
				Reason: block.Reason,
			}
		}
	}
	return workspaceRoot, commandDir, nil
}

func seatbeltCommandPlan(execRequest SandboxExecutionRequest, policy Policy, backend Backend) CommandPlan {
	return seatbeltCommandPlanWithProfile(execRequest.Command, execRequest.WorkspaceRoot, execRequest.PermissionProfile, policy, backend)
}

func seatbeltCommandPlanWithProfile(spec CommandSpec, workspaceRoot string, profile PermissionProfile, policy Policy, backend Backend) CommandPlan {
	denialTag := ""
	if policy.MonitorDenials {
		denialTag = nextSandboxDenialTag()
	}
	args := []string{"-p", seatbeltProfileFromPermissionProfile(profile, policy, denialTag), spec.Name}
	args = append(args, spec.Args...)
	envBackend := backend.Name
	if envBackend == "" {
		envBackend = BackendMacOSSeatbelt
	}
	env := sandboxEnvironmentForCommandWithSensitiveEnv(spec.Env, policy, envBackend, "", spec.sensitiveEnvKeys)
	env = sandboxRuntimeEnvironment(env, profile.Runtime)
	plan := CommandPlan{
		Backend:           backend,
		TargetBackend:     backend.TargetBackend(),
		WorkspaceRoot:     workspaceRoot,
		Policy:            policy,
		Wrapped:           true,
		SandboxEnvMarkers: backend.SandboxEnvMarkers(policy),
		EnforcementLevel:  backend.EnforcementLevel(policy),
		Name:              backend.Executable,
		Args:              args,
		Dir:               spec.Dir,
		Env:               env,
		SandboxDir:        spec.Dir,
	}
	// The plan's monitor tag MUST equal the one embedded in the profile above so the
	// monitor matches exactly this run's denials.
	plan.MonitorTag = denialTag
	return plan
}

func sandboxEnvironmentForCommand(specEnv []string, policy Policy, backend BackendName, workspaceRoot string) []string {
	return sandboxEnvironmentForCommandWithSensitiveEnv(specEnv, policy, backend, workspaceRoot, nil)
}

func sandboxEnvironmentForCommandWithSensitiveEnv(specEnv []string, policy Policy, backend BackendName, workspaceRoot string, sensitiveEnvKeys []string) []string {
	env := cloneStrings(specEnv)
	if specEnv == nil {
		// Preserve the caller environment for sandboxed commands. The sandbox
		// boundary is the filesystem/network policy, not env scrubbing; explicit
		// command env values still replace inherited values below.
		env = os.Environ()
	}
	env = scrubSensitiveEnv(env, sensitiveEnvKeys...)
	pathValue := envListValue(env, "PATH", defaultPath())
	if runtime.GOOS == "darwin" {
		// Preserve standard user tool locations so a bare `python3`/`node`
		// resolves to the user's installed tool while the sandbox profile keeps
		// filesystem access narrow via explicit allowances for those trees.
		pathValue = ensureMacToolPaths(pathValue)
	}
	overrides := []string{
		"PATH=" + pathValue,
		"TERM=" + envListValue(env, "TERM", "dumb"),
		EnvSandboxBackend + "=" + string(backend),
		"ZERO_SANDBOX_NETWORK=" + string(policy.Network),
		EnvSandboxed + "=1",
	}
	if workspaceRoot != "" {
		overrides = append(overrides, "HOME="+workspaceRoot)
	}
	if backend == BackendWindowsRestrictedToken || backend == BackendWindowsElevated {
		overrides = append(overrides,
			"COMSPEC="+envListValue(env, "COMSPEC", "cmd.exe"),
			"SystemRoot="+envListValue(env, "SystemRoot", `C:\Windows`),
			"WINDIR="+envListValue(env, "WINDIR", `C:\Windows`),
		)
	}
	return upsertEnvList(env, overrides...)
}

func cloneStrings(values []string) []string {
	if values == nil {
		return nil
	}
	return append([]string{}, values...)
}

func envListValue(env []string, key string, fallback string) string {
	for _, entry := range env {
		existingKey, value, ok := strings.Cut(entry, "=")
		if ok && strings.EqualFold(existingKey, key) && strings.TrimSpace(value) != "" {
			return value
		}
	}
	return fallback
}

func defaultPath() string {
	if runtime.GOOS == "windows" {
		return os.Getenv("PATH")
	}
	return "/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"
}

// macToolPaths are the user-package bin dirs that hold node/python/etc. on macOS
// (Homebrew on Apple Silicon under /opt/homebrew, and /usr/local for Intel/older
// installs). They are NOT in the scrubbed default PATH, so the sandbox would
// otherwise miss user-installed interpreters.
var macToolPaths = []string{
	"/opt/homebrew/bin",
	"/opt/homebrew/sbin",
	"/usr/local/bin",
	"/usr/local/sbin",
}

// ensureMacToolPaths prepends any missing macToolPaths to a PATH value so a
// sandboxed command can find Homebrew/usr-local tools. Existing entries are kept
// in place (no duplicates, original order preserved after the prepended dirs).
func ensureMacToolPaths(path string) string {
	present := make(map[string]bool)
	for _, entry := range strings.Split(path, ":") {
		if entry != "" {
			present[entry] = true
		}
	}
	missing := make([]string, 0, len(macToolPaths))
	for _, dir := range macToolPaths {
		if !present[dir] {
			missing = append(missing, dir)
		}
	}
	if len(missing) == 0 {
		return path
	}
	if path == "" {
		return strings.Join(missing, ":")
	}
	return strings.Join(missing, ":") + ":" + path
}

// sandboxWritableDevices are the standard character devices that virtually every
// command needs to write to (e.g. `> /dev/null`). The bubblewrap backend exposes
// these via `--dev /dev`; the sandbox-exec profile must allow them explicitly or
// the equivalent operations fail with "Operation not permitted".
var sandboxWritableDevices = []string{
	"/dev/null",
	"/dev/zero",
	"/dev/random",
	"/dev/urandom",
	"/dev/stdin",
	"/dev/stdout",
	"/dev/stderr",
	"/dev/tty",
	"/dev/dtracehelper",
}

// sandboxWritableSubpaths are non-workspace trees the sandbox-exec profile must
// keep writable for parity with the shared host temp roots used by the Linux
// bubblewrap backend.
// macOS resolves /tmp and /var to their /private counterparts before the sandbox
// check, so both forms are listed. /dev/fd covers process-substitution writes.
var sandboxWritableSubpaths = []string{
	"/tmp",
	"/private/tmp",
	"/var/tmp",
	"/private/var/tmp",
	"/var/folders",
	"/private/var/folders",
	"/dev/fd",
}

// sandboxMachServices is the curated allowlist of Mach services a sandboxed
// command may look up. Under the seatbelt default-deny, XPC to common system
// daemons is otherwise blocked, so tools that touch the keychain
// (securityd/trustd), user/group lookup (opendirectoryd), preferences
// (cfprefsd), network config (SystemConfiguration), launch services, or the
// pasteboard fail. None of these grant filesystem or network access — those stay
// governed by the file-write and network rules below — so the workspace boundary
// is unaffected.
var sandboxMachServices = []string{
	"com.apple.system.opendirectoryd.libinfo",
	"com.apple.system.opendirectoryd.membership",
	"com.apple.system.opendirectoryd.api",
	"com.apple.system.logger",
	"com.apple.logd",
	"com.apple.cfprefsd.daemon",
	"com.apple.cfprefsd.agent",
	"com.apple.securityd",
	"com.apple.securityd.xpc",
	"com.apple.SecurityServer",
	"com.apple.trustd",
	"com.apple.trustd.agent",
	"com.apple.SystemConfiguration.configd",
	"com.apple.SystemConfiguration.DNSConfiguration",
	"com.apple.lsd.mapdb",
	"com.apple.coreservices.launchservicesd",
	"com.apple.pasteboard.1",
}

// sandboxDenialLogTag is the base marker for a sandbox-exec denial in the unified
// log when Policy.MonitorDenials is set; nextSandboxDenialTag derives a unique
// per-plan tag from it so the runtime monitor can find this run's denials via
// `log stream`.
const sandboxDenialLogTag = "zero-sandbox-denied-v1"

// sandboxDenialTagSeq makes each monitored plan's denial tag unique.
var sandboxDenialTagSeq atomic.Uint64

// nextSandboxDenialTag returns a process-unique denial tag. Without uniqueness,
// two concurrent monitored commands share one marker and StartDenialMonitor —
// which filters `log stream` only by tag — would ingest each other's denials,
// leaking unrelated paths/hosts into the wrong <sandbox_blocks> block. The pid
// disambiguates across processes; the counter across plans within a process.
func nextSandboxDenialTag() string {
	return fmt.Sprintf("%s-%d-%d", sandboxDenialLogTag, os.Getpid(), sandboxDenialTagSeq.Add(1))
}

func sandboxMachLookupRule() string {
	filters := make([]string, 0, len(sandboxMachServices))
	for _, service := range sandboxMachServices {
		filters = append(filters, `(global-name "`+sandboxProfileString(service)+`")`)
	}
	return "(allow mach-lookup\n  " + strings.Join(filters, "\n  ") + ")"
}

func seatbeltProfileFromPermissionProfile(profile PermissionProfile, policy Policy, denialTag string) string {
	networkRule := networkRuleForProfile(profile.Network)
	readRule := seatbeltReadRule(profile.FileSystem)
	writeRule := seatbeltWriteRule(profile.FileSystem)
	denyDefault := "(deny default)"
	if denialTag != "" {
		// Tag denials so the runtime log monitor can attribute them to THIS run; the
		// message is emitted to the unified log on every deny and StartDenialMonitor
		// filters `log stream` for this exact (per-plan) tag.
		denyDefault = `(deny default (with message "` + sandboxProfileString(denialTag) + `"))`
	}
	rules := []string{
		"(version 1)",
		denyDefault,
		"(allow process*)",
		// Process info for all processes so `ps`, `lsof`, `pgrep` and friends can find
		// the processes the user asks the agent to inspect or terminate (e.g. a stale
		// dev server). Read-only inspection; actually signalling them is governed by
		// the signal rule below, and the kernel enforces UID ownership either way.
		"(allow process-info*)",
		"(allow sysctl-read)",
		"(allow sysctl-write (sysctl-name \"kern.grade_cputype\"))",
		"(allow iokit-open (iokit-registry-entry-class \"RootDomainUserClient\"))",
		"(allow ipc-posix-sem)",
		`(allow ipc-posix-shm-read-data ipc-posix-shm-write-create ipc-posix-shm-write-unlink (ipc-posix-name-regex #"^/__KMP_REGISTERED_LIB_[0-9]+$"))`,
		"(allow pseudo-tty)",
		`(allow file-read* file-write* file-ioctl (literal "/dev/ptmx"))`,
		`(allow file-read* file-write* (require-all (regex #"^/dev/ttys[0-9]+") (extension "com.apple.sandbox.pty")))`,
		`(allow file-ioctl (regex #"^/dev/ttys[0-9]+"))`,
		"(allow ipc-posix-shm-read* (ipc-posix-name-prefix \"apple.cfprefs.\"))",
		"(allow user-preference-read)",
		// Let a sandboxed command send signals. This covers its own children (test
		// runners, timeouts, `sleep 30 & kill %1`) AND user-owned processes the user
		// asks the agent to terminate — e.g. a stale dev server left listening on a
		// port by a previous session, which lives in a different process group and so
		// was previously unkillable. The kernel still enforces UID ownership: a
		// sandboxed command runs as the user, so it can only signal the user's own
		// processes, never root's or another user's.
		"(allow signal)",
		sandboxMachLookupRule(),
		seatbeltPlatformRuntimeRules(),
		readRule,
		writeRule,
	}
	rules = append(rules, denyReadRules(profile.FileSystem)...)
	// SBPL is last-match-wins, so the carveouts MUST follow the deny rules above:
	// they re-include the supported non-secret subtrees of a directory-level
	// credential deny (Zero's user plugin/specialist/command roots).
	rules = append(rules, denyReadCarveoutRules(profile.FileSystem)...)
	// A token override may live below a carveout. Reapply only those nested
	// denies after the broad allow so credentials remain protected under
	// Seatbelt's last-match-wins evaluation without hiding ordinary extension
	// files.
	rules = append(rules, denyReadRulesInsideCarveouts(profile.FileSystem)...)
	rules = append(rules, writeRootCarveoutDenyRules(profile.FileSystem)...)
	rules = append(rules, denyWriteRulesFromPaths(profile.FileSystem.DenyWrite)...)
	rules = append(rules, networkRule)
	return strings.Join(nonEmptyStrings(rules), "\n")
}

func seatbeltReadRule(fs FileSystemPolicy) string {
	if fs.Kind == FileSystemUnrestricted {
		return "(allow file-read*)"
	}
	// The user's global git config files so a sandboxed git can read identity and
	// config. Granted here (macOS seatbelt) rather than the cross-platform
	// PermissionProfile so the HOME-dependent paths don't leak into the platform-
	// agnostic policy snapshot.
	gitConfig := normalizeProfilePaths(userGitConfigReadPaths())
	filters := make([]string, 0, len(fs.ReadRoots)+len(macosPlatformReadRoots())+len(gitConfig))
	for _, root := range fs.ReadRoots {
		filters = appendSeatbeltSubpathFilter(filters, root)
	}
	if fs.IncludePlatformRoots {
		for _, root := range macosPlatformReadRoots() {
			filters = appendSeatbeltSubpathFilter(filters, root)
		}
	}
	for _, path := range gitConfig {
		filters = appendSeatbeltSubpathFilter(filters, path)
	}
	if len(filters) == 0 {
		return ""
	}
	rule := "(allow file-read* file-test-existence\n  " + strings.Join(filters, "\n  ") + ")"
	// Grant stat/existence on the ANCESTOR chain of every granted read root so path
	// resolution can traverse down to it. A (subpath "/a/b/ws") filter grants the
	// root and its descendants but NOT its parents (/a, /a/b), so resolving an
	// absolute path like `cd /a/b/ws` (or any path the kernel canonicalises) is
	// denied at the first ungranted parent — and macOS seatbelt surfaces that
	// denial as the misleading "cd: …: Not a directory" (ENOTDIR), not a clear
	// permission error. This is metadata only: ancestor directory *contents* stay
	// unreadable. Platform read roots are included too: not all are top-level —
	// /Library/Developer (the CLT toolchain) needs /Library stat-able, or a `cd`
	// into it (and any chdir-style traversal) ENOTDIRs even though reads succeed.
	ancestorRoots := append([]string{}, fs.ReadRoots...)
	if fs.IncludePlatformRoots {
		ancestorRoots = append(ancestorRoots, macosPlatformReadRoots()...)
	}
	ancestorRoots = append(ancestorRoots, gitConfig...)
	if ancestors := seatbeltAncestorMetadataRule(ancestorRoots); ancestors != "" {
		rule += "\n" + ancestors
	}
	return rule
}

// seatbeltAncestorMetadataRule allows stat/test-existence on the ancestor
// directories of each root via the path-ancestors filter, so chdir and
// absolute-path resolution can traverse to a deeply-nested granted root.
func seatbeltAncestorMetadataRule(roots []string) string {
	filters := make([]string, 0, len(roots))
	for _, root := range roots {
		root = strings.TrimSpace(root)
		if root == "" {
			continue
		}
		// A filesystem/volume root (e.g. "/") has no ancestors, and (path-ancestors
		// "/") is INVALID SBPL — sandbox-exec aborts and the command can't launch at
		// all. Skip it; the root itself is already covered by its subpath read grant.
		if clean := filepath.Clean(root); filepath.Dir(clean) == clean {
			continue
		}
		filters = append(filters, `(path-ancestors "`+sandboxProfileString(root)+`")`)
	}
	if len(filters) == 0 {
		return ""
	}
	return "(allow file-read-metadata file-test-existence\n  " + strings.Join(filters, "\n  ") + ")"
}

func seatbeltWriteRule(fs FileSystemPolicy) string {
	if fs.Kind == FileSystemUnrestricted {
		return "(allow file-write*)"
	}
	filters := make([]string, 0, len(fs.WriteRoots)+len(sandboxWritableSubpaths)+len(sandboxWritableDevices))
	for _, root := range fs.WriteRoots {
		if filter := seatbeltWritableRootFilter(root); filter != "" {
			filters = append(filters, filter)
		}
	}
	if fs.AllowTemp {
		for _, subpath := range sandboxWritableSubpaths {
			filters = append(filters, `(subpath "`+sandboxProfileString(subpath)+`")`)
		}
	}
	for _, device := range sandboxWritableDevices {
		filters = append(filters, `(literal "`+sandboxProfileString(device)+`")`)
	}
	if len(filters) == 0 {
		return ""
	}
	return "(allow file-write*\n  " + strings.Join(filters, "\n  ") + ")"
}

func seatbeltWritableRootFilter(root WritableRoot) string {
	rootPath := strings.TrimSpace(root.Root)
	if rootPath == "" {
		return ""
	}
	return `(subpath "` + sandboxProfileString(rootPath) + `")`
}

func seatbeltProtectedMetadataRegex(root string, name string) string {
	root = strings.TrimSpace(filepath.ToSlash(filepath.Clean(root)))
	name = strings.Trim(strings.TrimSpace(name), `/\`)
	if root == "" || name == "" || name == "." {
		return ""
	}
	root = strings.TrimRight(root, "/")
	if root == "" {
		root = "/"
	}
	escapedRoot := regexpQuoteMeta(root)
	escapedName := regexpQuoteMeta(name)
	if root == "/" {
		return "^/" + escapedName + "(/.*)?$"
	}
	return "^" + escapedRoot + "/" + escapedName + "(/.*)?$"
}

func denyReadRules(fs FileSystemPolicy) []string {
	denied := dedupeStrings(append(append([]string{}, fs.DenyRead...), fs.DenyReadIfExists...))
	rules := denySeatbeltPathRules("file-read*", denied)
	// Process-trusted final names have already had only their parent
	// canonicalized. Preserve that terminal pathname here: normalizing it again
	// would follow a terminal symlink and lose the atomic-replacement deny. The
	// command-supplied finals get the same treatment: seatbelt denies a pathname
	// whether or not it exists, so unlike bubblewrap it has no reason to refuse
	// the command over them.
	finals := dedupeStrings(append(append([]string{}, fs.ProcessTrustedDenyReadFiles...), fs.CommandDenyReadFinalFiles...))
	return dedupeStrings(append(rules, denySeatbeltNormalizedPathRules("file-read*", finals)...))
}

// denyReadCarveoutRules re-allows reads for the non-secret subtrees of a denied
// credential directory. Writes are untouched: the credential directory is not a
// write root, so the profile's write rule keeps denying them. Only
// DenyReadCarveouts entries are emitted, and the profile builder derives those
// exclusively from Zero's own config directory (never from a user-configured
// DenyRead root), so no user deny is weakened here.
func denyReadCarveoutRules(fs FileSystemPolicy) []string {
	denied := dedupeStrings(append(append([]string{}, fs.DenyRead...), fs.DenyReadIfExists...))
	resolved := credentialCarveoutPaths(denied, fs.DenyReadCarveouts)
	if len(resolved) == 0 {
		return nil
	}
	out := make([]string, 0, len(resolved)+1)
	for _, path := range resolved {
		escaped := sandboxProfileString(path)
		out = append(out,
			`(allow file-read* file-test-existence (subpath "`+escaped+`"))`,
			`(allow file-read* file-test-existence (literal "`+escaped+`"))`,
		)
	}
	// Resolving a path into the carveout also needs stat on its ancestors, and the
	// deny rule above covers the denied directory itself. This grants metadata
	// only, so the denied directory stays unreadable and unlistable — the same
	// split seatbeltReadRule already relies on for deeply nested read roots.
	if ancestors := seatbeltAncestorMetadataRule(resolved); ancestors != "" {
		out = append(out, ancestors)
	}
	return out
}

func denyReadRulesInsideCarveouts(fs FileSystemPolicy) []string {
	denied := dedupeStrings(append(append([]string{}, fs.DenyRead...), fs.DenyReadIfExists...))
	carveouts := credentialCarveoutPaths(denied, fs.DenyReadCarveouts)
	if len(carveouts) == 0 {
		return nil
	}
	inside := func(paths []string) []string {
		var out []string
		for _, path := range paths {
			if credentialPathReincluded(carveouts, path) {
				out = append(out, path)
			}
		}
		return out
	}
	rules := denySeatbeltNormalizedPathRules("file-read*", inside(normalizeProfilePaths(denied)))
	finals := dedupeStrings(append(append([]string{}, fs.ProcessTrustedDenyReadFiles...), fs.CommandDenyReadFinalFiles...))
	return dedupeStrings(append(rules, denySeatbeltNormalizedPathRules("file-read*", inside(finals))...))
}

func writeRootCarveoutDenyRules(fs FileSystemPolicy) []string {
	if fs.Kind != FileSystemRestricted {
		return nil
	}
	var out []string
	for _, root := range fs.WriteRoots {
		for _, subpath := range root.ReadOnlySubpaths {
			subpath = strings.TrimSpace(subpath)
			if subpath == "" {
				continue
			}
			escaped := sandboxProfileString(subpath)
			out = append(out,
				`(deny file-write* (literal "`+escaped+`"))`,
				`(deny file-write* (subpath "`+escaped+`"))`,
			)
		}
		for _, name := range root.ProtectedMetadataNames {
			regex := seatbeltProtectedMetadataRegex(root.Root, name)
			if regex == "" {
				continue
			}
			out = append(out, `(deny file-write* (regex #"`+sandboxProfileRegex(regex)+`"))`)
		}
	}
	return out
}

func denyWriteRulesFromPaths(paths []string) []string {
	return denySeatbeltPathRules("file-write*", paths)
}

func denySeatbeltPathRules(action string, paths []string) []string {
	return denySeatbeltNormalizedPathRules(action, normalizeProfilePaths(paths))
}

func denySeatbeltNormalizedPathRules(action string, paths []string) []string {
	paths = dedupeStrings(paths)
	if len(paths) == 0 {
		return nil
	}
	out := make([]string, 0, len(paths)*2)
	for _, path := range paths {
		filters := []string{`(subpath "` + sandboxProfileString(path) + `")`}
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			filters = []string{`(literal "` + sandboxProfileString(path) + `")`}
		} else {
			filters = append(filters, `(literal "`+sandboxProfileString(path)+`")`)
		}
		for _, filter := range filters {
			out = append(out, "(deny "+action+" "+filter+")")
			if action == "file-read*" {
				out = append(out, "(deny file-write-unlink "+filter+")")
			}
		}
	}
	return out
}

func networkRuleForProfile(network NetworkPolicy) string {
	switch network.Mode {
	case NetworkAllow:
		return "(allow network*)"
	default:
		// Seatbelt has no private network namespace. Its localhost filters can
		// reach services on the host and host interfaces, so the restricted
		// profile must deny the entire network surface.
		return "(deny network*)"
	}
}

func sandboxProfileString(value string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\r", `\r`)
	return replacer.Replace(value)
}

func sandboxProfileRegex(value string) string {
	replacer := strings.NewReplacer(`"`, `\"`, "\n", `\n`, "\r", `\r`)
	return replacer.Replace(value)
}

func appendSeatbeltSubpathFilter(filters []string, root string) []string {
	root = strings.TrimSpace(root)
	if root == "" {
		return filters
	}
	return append(filters, `(subpath "`+sandboxProfileString(root)+`")`)
}

func macosPlatformReadRoots() []string {
	return []string{
		"/bin",
		"/sbin",
		"/Applications",
		"/Library/Apple/System/Library/Frameworks",
		"/Library/Apple/System/Library/PrivateFrameworks",
		"/Library/Apple/usr/lib",
		"/usr/bin",
		"/usr/sbin",
		"/usr/lib",
		"/usr/libexec",
		"/usr/share",
		// User-installed package trees (Homebrew on Apple Silicon, /usr/local on
		// Intel / older installs). Broadened from the bare lib dirs to the whole
		// tree so the interpreters in .../bin and their Cellar/opt dylibs are
		// readable; writes stay confined to the workspace.
		"/usr/local",
		"/opt/homebrew",
		// Xcode Command Line Tools toolchain. /usr/bin/git (and clang, make, etc.)
		// are thin stubs that resolve the real binary under the active developer
		// dir — usually /Library/Developer/CommandLineTools. Without read+exec here
		// the stub can't reach the real tool and fails with the misleading
		// "xcode-select: No developer tools were found" error.
		"/Library/Developer",
		"/etc",
		"/private/etc",
		"/var/db",
		"/private/var/db",
		"/System/Library",
		"/System/iOSSupport/System/Library/Frameworks",
		"/System/iOSSupport/System/Library/PrivateFrameworks",
		"/System/iOSSupport/System/Library/SubFrameworks",
		"/Library/Apple",
		"/Library/Preferences",
		"/dev",
	}
}

func seatbeltPlatformRuntimeRules() string {
	return strings.Join([]string{
		`(allow file-map-executable`,
		`  (subpath "/Library/Apple/System/Library/Frameworks")`,
		`  (subpath "/Library/Apple/System/Library/PrivateFrameworks")`,
		`  (subpath "/Library/Apple/usr/lib")`,
		`  (subpath "/System/Library/Extensions")`,
		`  (subpath "/System/Library/Frameworks")`,
		`  (subpath "/System/Library/PrivateFrameworks")`,
		`  (subpath "/System/Library/SubFrameworks")`,
		`  (subpath "/System/iOSSupport/System/Library/Frameworks")`,
		`  (subpath "/System/iOSSupport/System/Library/PrivateFrameworks")`,
		`  (subpath "/System/iOSSupport/System/Library/SubFrameworks")`,
		`  (subpath "/usr/lib")`,
		// User-installed tools load their own dylibs (e.g. node -> libnode/libuv,
		// python3 -> its framework) from these trees; without map-executable here
		// a Homebrew/usr-local binary fails to start even when it is on PATH.
		`  (subpath "/usr/local")`,
		`  (subpath "/opt/homebrew"))`,
		`(allow system-mac-syscall (mac-policy-name "vnguard"))`,
		`(allow system-mac-syscall (require-all (mac-policy-name "Sandbox") (mac-syscall-number 67)))`,
		`(allow file-read-metadata file-test-existence`,
		`  (literal "/etc")`,
		`  (literal "/tmp")`,
		`  (literal "/var")`,
		`  (literal "/private/etc/localtime"))`,
		`(allow file-read-metadata file-test-existence (path-ancestors "/System/Volumes/Data/private"))`,
		// realpath()/getcwd() lstat every ancestor of a path, so resolving a tool
		// under /opt/homebrew or /usr/local also stats the parent dirs (e.g. /opt,
		// /usr). The subpath read rules above cover the trees themselves but not
		// those ancestors, so without this a Homebrew python3 fails at startup with
		// "realpath: /opt/homebrew/bin/: Operation not permitted" even though it is
		// on PATH and readable. node only exec()s (kernel traversal) so it is not
		// affected — which is exactly how this asymmetry was diagnosed.
		`(allow file-read-metadata file-test-existence (path-ancestors "/opt/homebrew") (path-ancestors "/usr/local"))`,
		`(allow file-read* file-test-existence (literal "/"))`,
		`(allow system-fsctl (fsctl-command FSIOC_CAS_BSDFLAGS))`,
		`(allow file-read* file-test-existence`,
		`  (literal "/dev/autofs_nowait")`,
		`  (literal "/dev/random")`,
		`  (literal "/dev/urandom")`,
		`  (literal "/private/etc/master.passwd")`,
		`  (literal "/private/etc/passwd")`,
		`  (literal "/private/etc/protocols")`,
		`  (literal "/private/etc/services"))`,
		`(allow file-read* file-test-existence file-write-data`,
		`  (literal "/dev/null")`,
		`  (literal "/dev/zero"))`,
		`(allow file-read-data file-test-existence file-write-data (subpath "/dev/fd"))`,
		`(allow file-read* file-test-existence file-write-data file-ioctl (literal "/dev/dtracehelper"))`,
		`(allow file-read* file-test-existence file-write* (subpath "/tmp"))`,
		`(allow file-read* file-write* (subpath "/private/tmp"))`,
		`(allow file-read* file-write* (subpath "/var/tmp"))`,
		`(allow file-read* file-write* (subpath "/private/var/tmp"))`,
		`(allow file-read* file-test-existence`,
		`  (literal "/System/Library/CoreServices")`,
		`  (literal "/System/Library/CoreServices/.SystemVersionPlatform.plist")`,
		`  (literal "/System/Library/CoreServices/SystemVersion.plist"))`,
		`(allow file-read-metadata (subpath "/var"))`,
		`(allow file-read-metadata (subpath "/private/var"))`,
		`(allow network-outbound (literal "/private/var/run/syslog"))`,
		`(allow ipc-posix-shm-read* (ipc-posix-name "apple.shm.notification_center"))`,
		`(allow file-read* (literal "/private/var/db/eligibilityd/eligibility.plist"))`,
		`(allow file-read* (extension "com.apple.app-sandbox.read"))`,
		`(allow file-read* file-write* (extension "com.apple.app-sandbox.read-write"))`,
	}, "\n")
}

func nonEmptyStrings(values []string) []string {
	out := values[:0]
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			out = append(out, value)
		}
	}
	return out
}

func regexpQuoteMeta(value string) string {
	replacer := strings.NewReplacer(
		`\`, `\\`,
		`.`, `\.`,
		`+`, `\+`,
		`*`, `\*`,
		`?`, `\?`,
		`(`, `\(`,
		`)`, `\)`,
		`|`, `\|`,
		`[`, `\[`,
		`]`, `\]`,
		`{`, `\{`,
		`}`, `\}`,
		`^`, `\^`,
		`$`, `\$`,
	)
	return replacer.Replace(value)
}

// ScrubSensitiveEnv removes credential-bearing variables from a child
// environment. It is exported for callers that exec a host tool OUTSIDE the
// platform sandbox — notably format-on-write, which runs a project's formatter
// in-process — so that a formatter doing dynamic configuration evaluation
// cannot read API keys and tokens out of the inherited environment.
func ScrubSensitiveEnv(env []string) []string {
	return scrubSensitiveEnv(env)
}

func scrubSensitiveEnv(env []string, additionalKeys ...string) []string {
	// Secrets not covered by the provider catalog: cloud/VCS credentials and
	// providers Zero talks to through generic OpenAI-compatible endpoints.
	sensitiveKeys := []string{
		"COHERE_API_KEY",
		"PERPLEXITY_API_KEY",
		"ANYSCALE_API_KEY",
		"AZURE_OPENAI_API_KEY",
		"AWS_SECRET_ACCESS_KEY",
		"AWS_SESSION_TOKEN",
		"GITLAB_TOKEN",
		"GH_TOKEN",
		"ZERO_WEBSEARCH_API_KEY",
		"ZERO_DAEMON_REMOTE_TOKEN",
		// The file form of the same bridge token. TokenFromEnv accepts either,
		// so scrubbing only the inline variable left the pointer readable, and
		// the default sandbox posture is read-all. There is no fallback
		// location to guess at, the path comes from this variable alone, so
		// removing it closes the leak rather than half of it. Same reasoning as
		// GOOGLE_APPLICATION_CREDENTIALS below.
		"ZERO_DAEMON_REMOTE_TOKEN_FILE",
	}
	for _, descriptor := range providercatalog.All() {
		for _, key := range descriptor.AuthEnvVars {
			// AWS_PROFILE is a profile name, not a secret, and scrubbing it
			// protects nothing: the SDKs fall back to the default profile in
			// ~/.aws/credentials, which credentialDenyReadPaths blocks at the
			// filesystem level instead. GOOGLE_APPLICATION_CREDENTIALS IS
			// scrubbed: it points at a service-account key file at an
			// arbitrary path, so hiding the pointer complements the deny-read
			// rule on the file itself.
			if key == "AWS_PROFILE" {
				continue
			}
			sensitiveKeys = append(sensitiveKeys, key)
		}
	}
	sensitiveKeys = append(sensitiveKeys, normalizeSensitiveEnvKeys(additionalKeys)...)

	out := make([]string, 0, len(env))
	for _, kv := range env {
		key, _, ok := strings.Cut(kv, "=")
		if !ok {
			out = append(out, kv)
			continue
		}
		drop := isDynamicSensitiveEnvKey(key)
		for _, sensitive := range sensitiveKeys {
			if strings.EqualFold(key, sensitive) {
				drop = true
				break
			}
		}
		if !drop {
			out = append(out, kv)
		}
	}
	return out
}

func normalizeSensitiveEnvKeys(keys []string) []string {
	out := make([]string, 0, len(keys))
	seen := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		key = strings.TrimSpace(key)
		// A misconfigured value like "COMPANY_LLM_SECRET=..." would never
		// match a real env key during scrubbing; keep only the name part.
		if name, _, found := strings.Cut(key, "="); found {
			key = strings.TrimSpace(name)
		}
		folded := strings.ToUpper(key)
		if key == "" {
			continue
		}
		if _, ok := seen[folded]; ok {
			continue
		}
		seen[folded] = struct{}{}
		out = append(out, key)
	}
	return out
}

func isDynamicSensitiveEnvKey(key string) bool {
	const (
		prefix = "ZERO_OAUTH_"
		suffix = "_CLIENT_SECRET"
	)
	key = strings.ToUpper(strings.TrimSpace(key))
	return strings.HasPrefix(key, prefix) &&
		strings.HasSuffix(key, suffix) &&
		len(key) > len(prefix)+len(suffix)
}
