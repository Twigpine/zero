package sandbox

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const WindowsSandboxCommandRunnerName = "zero-windows-command-runner.exe"

const windowsCapabilitySIDSchemaVersion = 2

// Hidden subcommands the main zero binary answers to when it acts as its own
// Windows sandbox helper (self-dispatch). The "__" prefix can never collide with
// a real user subcommand; internal/cli/app.go routes these before normal CLI
// parsing.
const (
	WindowsCommandRunnerSubcommand = "__windows-command-runner"
	WindowsSandboxSetupSubcommand  = "__windows-sandbox-setup"
)

// WindowsSandboxHelper locates a Windows sandbox helper to launch. Name is the
// executable; ArgsPrefix is prepended to the helper's own arguments. For a
// standalone helper .exe (the release layout) ArgsPrefix is empty; for
// self-dispatch — the running zero binary acting as its own helper — Name is
// os.Executable() and ArgsPrefix carries the hidden subcommand token.
type WindowsSandboxHelper struct {
	Name       string
	ArgsPrefix []string
}

// Available reports whether a helper executable was resolved.
func (helper WindowsSandboxHelper) Available() bool {
	return strings.TrimSpace(helper.Name) != ""
}

// ResolveWindowsSandboxCommandRunner locates the command-runner helper.
func ResolveWindowsSandboxCommandRunner(lookup func(string) (string, error)) WindowsSandboxHelper {
	return resolveWindowsSandboxHelper(WindowsSandboxCommandRunnerName, WindowsCommandRunnerSubcommand, lookup)
}

// ResolveWindowsSandboxSetupHelper locates the one-time setup helper.
func ResolveWindowsSandboxSetupHelper(lookup func(string) (string, error)) WindowsSandboxHelper {
	return resolveWindowsSandboxHelper(WindowsSandboxSetupName, WindowsSandboxSetupSubcommand, lookup)
}

func findWindowsSandboxCommandRunner(lookup func(string) (string, error)) WindowsSandboxHelper {
	return ResolveWindowsSandboxCommandRunner(lookup)
}

func findWindowsSandboxSetupHelper(lookup func(string) (string, error)) WindowsSandboxHelper {
	return ResolveWindowsSandboxSetupHelper(lookup)
}

// osExecutable resolves the running binary's path. A package var so tests can
// pin it deterministically (self-dispatch resolution depends on it).
var osExecutable = os.Executable

// resolveWindowsSandboxHelper resolves a helper in three tiers, mirroring the
// Linux helper resolution (linux_helper.go): (1) a standalone .exe adjacent to
// the running binary — the release layout, kept first so a packaged helper still
// wins; (2) the same name on PATH; (3) SELF-DISPATCH — the running zero binary
// itself, invoked with the hidden subcommand. Tier 3 is what makes the sandbox
// reachable under `go run`/plain `go build` (no separate helper shipped),
// fixing the dev-only "command runner is not available" failure that hard-failed
// every bash command. Returns an empty helper only if os.Executable() fails.
func resolveWindowsSandboxHelper(name, subcommand string, lookup func(string) (string, error)) WindowsSandboxHelper {
	if exe, err := osExecutable(); err == nil {
		if candidate := filepath.Join(filepath.Dir(exe), name); regularFile(candidate) {
			return WindowsSandboxHelper{Name: candidate}
		}
	}
	if lookup != nil {
		if path, err := lookup(name); err == nil && path != "" {
			return WindowsSandboxHelper{Name: path}
		}
	}
	if exe, err := osExecutable(); err == nil && strings.TrimSpace(exe) != "" {
		return WindowsSandboxHelper{Name: exe, ArgsPrefix: []string{subcommand}}
	}
	return WindowsSandboxHelper{}
}

func regularFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

type WindowsSandboxLevel string

const (
	WindowsSandboxLevelRestrictedToken WindowsSandboxLevel = "restricted-token"
	// WindowsSandboxLevelUnelevated is the restricted-token tier without the
	// elevated setup: the runner applies the workspace ACL plan itself before
	// launching (no Administrator rights needed) and skips the elevated setup
	// marker check. Network is NOT enforced at this level — the WFP filters need
	// the elevated setup — so callers must keep network behind the approval gate.
	WindowsSandboxLevelUnelevated WindowsSandboxLevel = "unelevated"
	WindowsSandboxLevelElevated   WindowsSandboxLevel = "elevated"
	WindowsSandboxLevelDisabled   WindowsSandboxLevel = "disabled"
)

// WindowsShellArgs returns cmd.exe's argv (after the executable name) for
// running commandText as a shell command on Windows: no wrapping quotes and
// no /S, so commandText reaches cmd.exe's own /C remainder parsing exactly as
// written. See WindowsShellCommandLine for why that matters. Used both as
// CommandSpec.Args for the plain (unwrapped) path and, via
// windowsShellCommandLineFromArgs, to recognize this same shape once it has
// round-tripped through the sandboxed runner's CLI argv.
func WindowsShellArgs(commandText string) []string {
	return []string{"/d", "/c", commandText}
}

// WindowsShellCommandLine builds the raw command line a Windows CreateProcess
// call should receive to run commandText as a shell command: "cmd.exe /d /c "
// followed by commandText completely unescaped.
//
// cmd.exe's own /C remainder parsing is not CommandLineToArgvW-compatible: it
// applies its own quote-stripping heuristic (documented in `cmd /?`) whenever
// the remainder starts with a literal quote, and that heuristic does not
// understand backslash-escaped inner quotes the way a normal argv consumer
// would. A command like `python -c "print(15 / 3)"` passed through the
// standard exec.Cmd Args mechanism gets wrapped in an outer pair of quotes
// (because it contains spaces) with its own quotes escaped as \" — exactly
// the shape that heuristic corrupts, stripping the outer quotes but leaving
// the literal backslashes behind. Passing commandText through raw, exactly as
// the model wrote it, means cmd.exe parses it the same way it would if typed
// directly at an interactive prompt: a leading quoted executable path like
// `"C:\Program Files\foo.exe" arg` is preserved correctly by cmd.exe's own
// quote-preserving special case, and a command with no leading quote at all,
// like the python example above, is never touched by the stripping heuristic
// in the first place.
func WindowsShellCommandLine(commandText string) string {
	return "cmd.exe /d /c " + commandText
}

// windowsShellCommandLineFromArgs reports whether args is exactly
// ["cmd.exe"]+WindowsShellArgs(text) for some text and, if so, returns
// WindowsShellCommandLine's result for that text. The sandboxed runner path
// only has the deserialized argv (WindowsSandboxCommandConfig.Command) to
// work from, not the original commandText, so it recovers the raw command
// line by recognizing the shape instead.
func windowsShellCommandLineFromArgs(args []string) (string, bool) {
	if len(args) != 4 {
		return "", false
	}
	if !strings.EqualFold(args[0], "cmd.exe") || args[1] != "/d" || args[2] != "/c" {
		return "", false
	}
	return WindowsShellCommandLine(args[3]), true
}

type WindowsSandboxCommandArgsOptions struct {
	SandboxHome       string
	CommandCWD        string
	WorkspaceRoots    []string
	PermissionProfile PermissionProfile
	Env               []string
	SandboxLevel      WindowsSandboxLevel
	Command           []string
}

type WindowsSandboxCommandConfig struct {
	SandboxHome       string
	CommandCWD        string
	WorkspaceRoots    []string
	PermissionProfile PermissionProfile
	Env               map[string]string
	SandboxLevel      WindowsSandboxLevel
	Command           []string
}

func BuildWindowsSandboxCommandArgs(options WindowsSandboxCommandArgsOptions) ([]string, error) {
	commandCWD := strings.TrimSpace(options.CommandCWD)
	if commandCWD == "" {
		return nil, errors.New("windows sandbox command runner requires command cwd")
	}
	if len(options.Command) == 0 {
		return nil, errors.New("windows sandbox command runner requires command")
	}
	sandboxHome := strings.TrimSpace(options.SandboxHome)
	if sandboxHome == "" {
		var err error
		sandboxHome, err = ResolveWindowsSandboxHome(nil)
		if err != nil {
			return nil, err
		}
	}
	level := options.SandboxLevel
	if level == "" {
		level = WindowsSandboxLevelRestrictedToken
	}
	if !validWindowsSandboxLevel(level) {
		return nil, fmt.Errorf("unsupported windows sandbox level %q", level)
	}
	workspaceRoots := trimNonEmptyStrings(options.WorkspaceRoots)
	if len(workspaceRoots) == 0 {
		workspaceRoots = []string{commandCWD}
	}
	profileJSON, err := json.Marshal(options.PermissionProfile)
	if err != nil {
		return nil, fmt.Errorf("marshal windows sandbox permission profile: %w", err)
	}
	envJSON, err := json.Marshal(envListToMap(options.Env))
	if err != nil {
		return nil, fmt.Errorf("marshal windows sandbox environment: %w", err)
	}
	args := []string{
		"--sandbox-home", sandboxHome,
		"--command-cwd", commandCWD,
		"--permission-profile", string(profileJSON),
		"--env-json", string(envJSON),
		"--windows-sandbox-level", string(level),
	}
	for _, root := range workspaceRoots {
		args = append(args, "--workspace-root", root)
	}
	args = append(args, "--")
	args = append(args, options.Command...)
	return args, nil
}

func ParseWindowsSandboxCommandArgs(args []string) (WindowsSandboxCommandConfig, error) {
	var config WindowsSandboxCommandConfig
	var profileJSON string
	var envJSON string
	for index := 0; index < len(args); index++ {
		arg := args[index]
		switch arg {
		case "--":
			config.Command = cloneStrings(args[index+1:])
			index = len(args)
		case "--command-cwd":
			value, next, err := nextWindowsSandboxFlagValue(args, index)
			if err != nil {
				return WindowsSandboxCommandConfig{}, err
			}
			config.CommandCWD = strings.TrimSpace(value)
			index = next
		case "--sandbox-home":
			value, next, err := nextWindowsSandboxFlagValue(args, index)
			if err != nil {
				return WindowsSandboxCommandConfig{}, err
			}
			config.SandboxHome = strings.TrimSpace(value)
			index = next
		case "--workspace-root":
			value, next, err := nextWindowsSandboxFlagValue(args, index)
			if err != nil {
				return WindowsSandboxCommandConfig{}, err
			}
			if root := strings.TrimSpace(value); root != "" {
				config.WorkspaceRoots = append(config.WorkspaceRoots, root)
			}
			index = next
		case "--permission-profile":
			value, next, err := nextWindowsSandboxFlagValue(args, index)
			if err != nil {
				return WindowsSandboxCommandConfig{}, err
			}
			profileJSON = strings.TrimSpace(value)
			index = next
		case "--env-json":
			value, next, err := nextWindowsSandboxFlagValue(args, index)
			if err != nil {
				return WindowsSandboxCommandConfig{}, err
			}
			envJSON = strings.TrimSpace(value)
			index = next
		case "--windows-sandbox-level":
			value, next, err := nextWindowsSandboxFlagValue(args, index)
			if err != nil {
				return WindowsSandboxCommandConfig{}, err
			}
			config.SandboxLevel = WindowsSandboxLevel(strings.TrimSpace(value))
			index = next
		default:
			return WindowsSandboxCommandConfig{}, fmt.Errorf("unknown windows sandbox runner flag %q", arg)
		}
	}
	if config.CommandCWD == "" {
		return WindowsSandboxCommandConfig{}, errors.New("missing --command-cwd")
	}
	if config.SandboxHome == "" {
		return WindowsSandboxCommandConfig{}, errors.New("missing --sandbox-home")
	}
	if len(config.WorkspaceRoots) == 0 {
		config.WorkspaceRoots = []string{config.CommandCWD}
	}
	if profileJSON == "" {
		return WindowsSandboxCommandConfig{}, errors.New("missing --permission-profile")
	}
	if err := json.Unmarshal([]byte(profileJSON), &config.PermissionProfile); err != nil {
		return WindowsSandboxCommandConfig{}, fmt.Errorf("invalid --permission-profile: %w", err)
	}
	if envJSON == "" {
		return WindowsSandboxCommandConfig{}, errors.New("missing --env-json")
	}
	if err := json.Unmarshal([]byte(envJSON), &config.Env); err != nil {
		return WindowsSandboxCommandConfig{}, fmt.Errorf("invalid --env-json: %w", err)
	}
	if config.SandboxLevel == "" {
		config.SandboxLevel = WindowsSandboxLevelRestrictedToken
	}
	if !validWindowsSandboxLevel(config.SandboxLevel) {
		return WindowsSandboxCommandConfig{}, fmt.Errorf("unsupported windows sandbox level %q", config.SandboxLevel)
	}
	if len(config.Command) == 0 {
		return WindowsSandboxCommandConfig{}, errors.New("missing command after --")
	}
	return config, nil
}

func windowsRestrictedTokenCommandPlan(execRequest SandboxExecutionRequest, policy Policy) (CommandPlan, error) {
	// Reject DenyRead before provisioning either restricted-token runner level.
	// Both elevated and unelevated build the same fully restricted narrow-SID
	// token for DenyRead profiles, which cannot load ordinary system binaries.
	if err := windowsDenyReadRestrictedTokenUnsupportedProfile(execRequest.PermissionProfile); err != nil {
		return CommandPlan{}, err
	}
	spec := execRequest.Command
	var sandboxHomeEnv map[string]string
	if spec.Env != nil {
		sandboxHomeEnv = envListToMap(spec.Env)
	}
	sandboxHome, err := ResolveWindowsSandboxHome(sandboxHomeEnv)
	if err != nil {
		return CommandPlan{}, err
	}
	childEnv := sandboxEnvironmentForCommandWithSensitiveEnv(spec.Env, policy, BackendWindowsRestrictedToken, execRequest.WorkspaceRoot, spec.sensitiveEnvKeys)
	childEnv = sandboxRuntimeEnvironment(childEnv, execRequest.PermissionProfile.Runtime)
	// The unelevated enforcement tier maps to the runner's unelevated level: same
	// restricted token, but the runner applies the workspace ACL plan itself instead
	// of requiring the elevated setup marker.
	level := WindowsSandboxLevelRestrictedToken
	if execRequest.EnforcementLevel == EnforcementUnelevated {
		level = WindowsSandboxLevelUnelevated
	}
	args, err := BuildWindowsSandboxCommandArgs(WindowsSandboxCommandArgsOptions{
		SandboxHome:       sandboxHome,
		CommandCWD:        spec.Dir,
		WorkspaceRoots:    []string{execRequest.WorkspaceRoot},
		PermissionProfile: execRequest.PermissionProfile,
		Env:               childEnv,
		SandboxLevel:      level,
		Command:           append([]string{spec.Name}, spec.Args...),
	})
	if err != nil {
		return CommandPlan{}, err
	}
	// Prepend the helper's args prefix: for self-dispatch this is the hidden
	// subcommand token (Name is the running zero binary), so the launched command
	// is `zero __windows-command-runner <sandbox args>`. Empty for a standalone
	// helper .exe, where args are passed unchanged.
	fullArgs := append(append([]string{}, execRequest.Backend.ExecutableArgsPrefix...), args...)
	return withSandboxExecutionMetadata(CommandPlan{
		Backend:           execRequest.Backend,
		TargetBackend:     execRequest.TargetBackend,
		WorkspaceRoot:     execRequest.WorkspaceRoot,
		Policy:            policy,
		Wrapped:           true,
		SandboxEnvMarkers: execRequest.SandboxEnvMarkers,
		EnforcementLevel:  execRequest.EnforcementLevel,
		Name:              execRequest.Backend.Executable,
		Args:              fullArgs,
		Dir:               spec.Dir,
		Env:               childEnv,
		SandboxDir:        spec.Dir,
	}, execRequest), nil
}

func upsertEnvList(env []string, values ...string) []string {
	out := cloneStrings(env)
	for _, value := range values {
		key, _, ok := strings.Cut(value, "=")
		if !ok || strings.TrimSpace(key) == "" {
			continue
		}
		replaced := false
		for index, existing := range out {
			existingKey, _, existingOK := strings.Cut(existing, "=")
			if existingOK && strings.EqualFold(existingKey, key) {
				out[index] = value
				replaced = true
			}
		}
		if !replaced {
			out = append(out, value)
		}
	}
	return out
}

func envListToMap(env []string) map[string]string {
	out := map[string]string{}
	for _, value := range env {
		key, envValue, ok := strings.Cut(value, "=")
		if ok && strings.TrimSpace(key) != "" {
			out[key] = envValue
		}
	}
	return out
}

func trimNonEmptyStrings(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func nextWindowsSandboxFlagValue(args []string, index int) (string, int, error) {
	if index+1 >= len(args) {
		return "", index, fmt.Errorf("missing value for %s", args[index])
	}
	value := args[index+1]
	if value == "--" || strings.HasPrefix(value, "--") {
		return "", index, fmt.Errorf("missing value for %s", args[index])
	}
	return value, index + 1, nil
}

func validWindowsSandboxLevel(level WindowsSandboxLevel) bool {
	switch level {
	case WindowsSandboxLevelRestrictedToken, WindowsSandboxLevelUnelevated, WindowsSandboxLevelElevated, WindowsSandboxLevelDisabled:
		return true
	default:
		return false
	}
}

type WindowsCapabilitySIDs struct {
	SchemaVersion      int               `json:"schemaVersion"`
	ReadOnly           string            `json:"readOnly"`
	WorkspaceByRoot    map[string]string `json:"workspaceByRoot,omitempty"`
	WritableRootByPath map[string]string `json:"writableRootByPath,omitempty"`
	// Offline is a synthetic SID that carries NO filesystem ACL. The persistent
	// WFP outbound-block filter is scoped to it, so a command's network is gated
	// purely by whether its restricted token carries this SID: offline (deny)
	// commands include it and are blocked; online (approved) commands omit it and
	// reach the network — both still write-jailed by the capability SIDs.
	Offline string `json:"offline,omitempty"`
}

func ResolveWindowsSandboxHome(env map[string]string) (string, error) {
	if override := strings.TrimSpace(envValue(env, "ZERO_WINDOWS_SANDBOX_HOME")); override != "" {
		if filepath.IsAbs(override) {
			return filepath.Clean(override), nil
		}
		return filepath.Abs(override)
	}
	grantPath, err := ResolveGrantPath(env)
	if err != nil {
		return "", err
	}
	return filepath.Dir(grantPath), nil
}

func WindowsCapabilitySIDPath(sandboxHome string) string {
	return filepath.Join(filepath.Clean(sandboxHome), "windows-cap-sids.json")
}

func LoadOrCreateWindowsCapabilitySIDs(sandboxHome string) (WindowsCapabilitySIDs, error) {
	sandboxHome = strings.TrimSpace(sandboxHome)
	if sandboxHome == "" {
		return WindowsCapabilitySIDs{}, errors.New("windows sandbox home is required")
	}
	path := WindowsCapabilitySIDPath(sandboxHome)
	if bytes, err := os.ReadFile(path); err == nil {
		var caps WindowsCapabilitySIDs
		if err := json.Unmarshal(bytes, &caps); err != nil {
			return WindowsCapabilitySIDs{}, fmt.Errorf("parse windows capability SIDs %s: %w", path, err)
		}
		normalizeWindowsCapabilitySIDs(&caps)
		if caps.ReadOnly != "" {
			// Back-compat: an older (schema 1) file has no offline-marker SID.
			// Mint one and persist so the setup helper and the runner agree on a
			// single value for the WFP filter scope across processes.
			if caps.Offline == "" {
				caps.Offline = randomWindowsCapabilitySID()
				caps.SchemaVersion = windowsCapabilitySIDSchemaVersion
				if err := saveWindowsCapabilitySIDs(path, caps); err != nil {
					return WindowsCapabilitySIDs{}, err
				}
			}
			return caps, nil
		}
	} else if !os.IsNotExist(err) {
		return WindowsCapabilitySIDs{}, fmt.Errorf("read windows capability SIDs %s: %w", path, err)
	}
	caps := WindowsCapabilitySIDs{
		SchemaVersion:      windowsCapabilitySIDSchemaVersion,
		ReadOnly:           randomWindowsCapabilitySID(),
		Offline:            randomWindowsCapabilitySID(),
		WorkspaceByRoot:    map[string]string{},
		WritableRootByPath: map[string]string{},
	}
	if err := saveWindowsCapabilitySIDs(path, caps); err != nil {
		return WindowsCapabilitySIDs{}, err
	}
	return caps, nil
}

// WindowsOfflineMarkerSID returns the sandbox home's offline-marker SID (minting
// and persisting it on first use). The persistent WFP block filter is scoped to
// this SID; a deny-mode command's token carries it, an allow-mode command's does
// not.
func WindowsOfflineMarkerSID(sandboxHome string) (string, error) {
	caps, err := LoadOrCreateWindowsCapabilitySIDs(sandboxHome)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(caps.Offline) == "" {
		return "", errors.New("windows sandbox offline-marker SID is missing")
	}
	return caps.Offline, nil
}

// windowsRuntimeTokenSIDs composes the restricting-SID set for the sandbox's
// restricted token. Both modes carry the write-capability SIDs (so the workspace
// write-jail holds in either case); DENY additionally carries the offline-marker
// SID that the persistent WFP block filter matches, so an offline command has no
// network while an approved online command does.
func windowsRuntimeTokenSIDs(capabilitySIDs []string, offlineSID string, mode NetworkMode) []string {
	out := append([]string(nil), capabilitySIDs...)
	if mode == NetworkDeny && strings.TrimSpace(offlineSID) != "" {
		out = append(out, offlineSID)
	}
	return out
}

func WindowsWorkspaceCapabilitySID(sandboxHome string, root string) (string, error) {
	caps, err := LoadOrCreateWindowsCapabilitySIDs(sandboxHome)
	if err != nil {
		return "", err
	}
	key := windowsCapabilityPathKey(root)
	if key == "" {
		return "", errors.New("workspace root is required")
	}
	if sid := caps.WorkspaceByRoot[key]; sid != "" {
		return sid, nil
	}
	if caps.WorkspaceByRoot == nil {
		caps.WorkspaceByRoot = map[string]string{}
	}
	sid := randomWindowsCapabilitySID()
	caps.WorkspaceByRoot[key] = sid
	return sid, saveWindowsCapabilitySIDs(WindowsCapabilitySIDPath(sandboxHome), caps)
}

func WindowsWritableRootCapabilitySID(sandboxHome string, root string) (string, error) {
	caps, err := LoadOrCreateWindowsCapabilitySIDs(sandboxHome)
	if err != nil {
		return "", err
	}
	key := windowsCapabilityPathKey(root)
	if key == "" {
		return "", errors.New("writable root is required")
	}
	if sid := caps.WritableRootByPath[key]; sid != "" {
		return sid, nil
	}
	if caps.WritableRootByPath == nil {
		caps.WritableRootByPath = map[string]string{}
	}
	sid := randomWindowsCapabilitySID()
	caps.WritableRootByPath[key] = sid
	return sid, saveWindowsCapabilitySIDs(WindowsCapabilitySIDPath(sandboxHome), caps)
}

func WindowsCapabilitySIDsForConfig(config WindowsSandboxCommandConfig) ([]string, error) {
	if config.PermissionProfile.FileSystem.Kind != FileSystemRestricted {
		return nil, errors.New("windows sandbox requires a restricted filesystem permission profile")
	}
	if len(config.PermissionProfile.FileSystem.WriteRoots) == 0 {
		caps, err := LoadOrCreateWindowsCapabilitySIDs(config.SandboxHome)
		if err != nil {
			return nil, err
		}
		return []string{caps.ReadOnly}, nil
	}
	out := make([]string, 0, len(config.PermissionProfile.FileSystem.WriteRoots))
	for _, root := range config.PermissionProfile.FileSystem.WriteRoots {
		path := strings.TrimSpace(root.Root)
		if path == "" {
			continue
		}
		var (
			sid string
			err error
		)
		if windowsRootMatchesWorkspace(path, config.WorkspaceRoots) {
			sid, err = WindowsWorkspaceCapabilitySID(config.SandboxHome, path)
		} else {
			sid, err = WindowsWritableRootCapabilitySID(config.SandboxHome, path)
		}
		if err != nil {
			return nil, err
		}
		out = append(out, sid)
	}
	if len(out) == 0 {
		return nil, errors.New("windows sandbox has no writable capability roots")
	}
	return out, nil
}

func normalizeWindowsCapabilitySIDs(caps *WindowsCapabilitySIDs) {
	if caps.SchemaVersion == 0 {
		caps.SchemaVersion = windowsCapabilitySIDSchemaVersion
	}
	if caps.WorkspaceByRoot == nil {
		caps.WorkspaceByRoot = map[string]string{}
	}
	if caps.WritableRootByPath == nil {
		caps.WritableRootByPath = map[string]string{}
	}
}

func saveWindowsCapabilitySIDs(path string, caps WindowsCapabilitySIDs) error {
	normalizeWindowsCapabilitySIDs(&caps)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create windows capability SID dir: %w", err)
	}
	bytes, err := json.MarshalIndent(caps, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal windows capability SIDs: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".windows-cap-sids-*.tmp")
	if err != nil {
		return fmt.Errorf("create windows capability SID temp file: %w", err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(bytes); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("write windows capability SID temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("close windows capability SID temp file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("replace windows capability SID file: %w", err)
	}
	return nil
}

func windowsCapabilityPathKey(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	path = filepath.Clean(path)
	return strings.ToLower(strings.ReplaceAll(path, "/", `\`))
}

func windowsRootMatchesWorkspace(root string, workspaceRoots []string) bool {
	rootKey := windowsCapabilityPathKey(root)
	if rootKey == "" {
		return false
	}
	for _, workspaceRoot := range workspaceRoots {
		if windowsCapabilityPathKey(workspaceRoot) == rootKey {
			return true
		}
	}
	return false
}

func randomWindowsCapabilitySID() string {
	var words [4]uint32
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		panic("generate windows capability SID entropy: " + err.Error())
	}
	for index := range words {
		words[index] = binary.LittleEndian.Uint32(bytes[index*4 : index*4+4])
		if words[index] == 0 {
			words[index] = uint32(index + 1)
		}
	}
	return fmt.Sprintf("S-1-5-21-%d-%d-%d-%d", words[0], words[1], words[2], words[3])
}
