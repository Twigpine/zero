package sandbox

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/Gitlawb/zero/internal/remotetoken"
)

// errWindowsSandboxNotInitialized is returned only to a caller that explicitly
// REQUIRED the sandbox (--sandbox require) on Windows before `zero sandbox setup`
// has run. The default (auto) preference degrades instead — see BuildExecutionRequest.
var errWindowsSandboxNotInitialized = errors.New(
	"the Windows sandbox is not initialized — run `zero sandbox setup` from an elevated (Administrator) terminal, " +
		"or drop `--sandbox require` to run with workspace path-confinement and per-command approval")

// windowsSetupDowngradeReason is surfaced when Windows falls back to the
// in-process policy gate because the OS sandbox has not been set up.
const windowsSetupDowngradeReason = "Windows OS sandbox inactive — commands run with workspace path-confinement and " +
	"per-command approval only. Run `zero sandbox setup` from an elevated (Administrator) terminal for full filesystem and network isolation."

// windowsUnelevatedDowngradeReason is surfaced when Windows runs the sandbox in
// the unelevated tier: the restricted-token write-jail is enforced (the command
// runner applies the workspace ACLs itself, which needs no Administrator
// rights), but the WFP network filters are Administrator-only and absent.
const windowsUnelevatedDowngradeReason = "Windows sandbox running unelevated — the filesystem write-jail is enforced, but network " +
	"isolation still relies on per-command approval. Run `zero sandbox setup` from an elevated (Administrator) terminal for OS-level network enforcement."

// windowsSandboxInitialized reports whether the per-host Windows sandbox setup
// marker exists. A missing marker is the common fresh-install state, so the
// caller degrades rather than bricking the command. Indirected through a var so
// tests can drive both the initialized and not-initialized paths.
var windowsSandboxInitialized = func() bool {
	home, err := ResolveWindowsSandboxHome(nil)
	if err != nil || home == "" {
		return false
	}
	_, statErr := os.Stat(WindowsSandboxSetupMarkerPath(home))
	return statErr == nil
}

type SandboxPreference string

const (
	SandboxPreferenceAuto    SandboxPreference = "auto"
	SandboxPreferenceRequire SandboxPreference = "require"
	SandboxPreferenceForbid  SandboxPreference = "forbid"
)

type SandboxManagerOptions struct {
	GOOS             string
	LookupExecutable func(string) (string, error)
	DetectWSL        func() WSLInfo
	Backend          Backend
}

type SandboxManager struct {
	goos    string
	backend Backend
}

type SandboxManagerRequest struct {
	WorkspaceRoot     string
	Command           CommandSpec
	Policy            Policy
	Scope             *Scope
	Profile           PermissionProfile
	Preference        SandboxPreference
	ValidateExecution bool
}

type SandboxExecutionRequest struct {
	Command                 CommandSpec         `json:"command"`
	WorkspaceRoot           string              `json:"workspaceRoot"`
	PermissionProfile       PermissionProfile   `json:"permissionProfile"`
	Backend                 Backend             `json:"backend"`
	TargetBackend           BackendName         `json:"targetBackend"`
	CommandWrapped          bool                `json:"commandWrapped"`
	SandboxEnvMarkers       []string            `json:"sandboxEnvMarkers,omitempty"`
	EnforcementLevel        EnforcementLevel    `json:"enforcementLevel"`
	DowngradeReason         string              `json:"downgradeReason,omitempty"`
	SupportLevel            BackendSupportLevel `json:"supportLevel"`
	RequiresPlatformSandbox bool                `json:"requiresPlatformSandbox"`
}

func NewSandboxManager(options SandboxManagerOptions) SandboxManager {
	backend := options.Backend
	if backend.Name != "" && backend.Platform == "" {
		backend.Platform = platformForBackendName(backend.Name)
	}
	goos := options.GOOS
	if goos == "" {
		goos = backend.Platform
	}
	if goos == "" {
		goos = runtime.GOOS
	}
	if backend.Name == "" {
		backend = selectPlatformBackend(goos, options.LookupExecutable, options.DetectWSL)
	}
	if backend.Platform == "" {
		backend.Platform = goos
	}
	backend = inferBackendCapabilities(backend)
	return SandboxManager{goos: goos, backend: backend}
}

func platformForBackendName(name BackendName) string {
	switch name {
	case BackendMacOSSeatbelt:
		return "darwin"
	case BackendLinuxBwrap, BackendLinuxLandlock, BackendWSL:
		return "linux"
	case BackendWindowsRestrictedToken, BackendWindowsElevated:
		return "windows"
	default:
		return ""
	}
}

func (manager SandboxManager) Backend() Backend {
	return manager.backend
}

// isExecutable checks whether a file is executable. On Unix, this checks the
// execute permission bits. On Windows, Go's os.FileMode does not set execute
// bits, so we check for common executable extensions instead.
func isExecutable(fi os.FileInfo) bool {
	if !fi.Mode().IsRegular() {
		return false
	}
	if runtime.GOOS == "windows" {
		name := strings.ToLower(fi.Name())
		return strings.HasSuffix(name, ".exe") ||
			strings.HasSuffix(name, ".com") ||
			strings.HasSuffix(name, ".bat") ||
			strings.HasSuffix(name, ".cmd")
	}
	return fi.Mode()&0o111 != 0
}

// lookupExecutable checks whether a named binary exists and is executable,
// using os.Stat instead of exec.LookPath. exec.LookPath internally uses the
// faccessat2 syscall, which is blocked by Android's seccomp filter (SIGSYS).
func lookupExecutable(name string) (string, error) {
	if !strings.Contains(name, string(os.PathSeparator)) {
		path := os.Getenv("PATH")
		for _, dir := range strings.Split(path, string(os.PathListSeparator)) {
			if dir == "" {
				continue // skip empty entries (e.g. trailing colon)
			}
			candidate := filepath.Join(dir, name)
			if fi, err := os.Stat(candidate); err == nil {
				if isExecutable(fi) {
					return candidate, nil
				}
			}
		}
		return "", errors.New("executable file not found in $PATH")
	}
	fi, err := os.Stat(name)
	if err != nil {
		return "", err
	}
	if isExecutable(fi) {
		return name, nil
	}
	return "", errors.New("executable file not found")
}

func selectPlatformBackend(goos string, lookup func(string) (string, error), detect func() WSLInfo) Backend {
	if lookup == nil {
		lookup = lookupExecutable
	}
	if detect == nil {
		detect = detectWSL
	}
	switch goos {
	case "linux":
		if helper := resolveLinuxSandboxHelper(lookup); helper.Name != "" {
			if _, bwrapErr := lookup("bwrap"); bwrapErr != nil {
				return unavailableBackend(goos, "bubblewrap is not installed")
			}
			backend := nativeBackend(goos, BackendLinuxBwrap, helper.Name, "Linux sandbox helper available")
			backend.ExecutableArgsPrefix = helper.ArgsPrefix
			return backend
		}
		if info := detect(); info.IsWSL {
			return wslBackend(goos, info)
		}
		return unavailableBackend(goos, "Linux sandbox helper is not available")
	case "darwin":
		if path, err := lookup("sandbox-exec"); err == nil && path != "" {
			return nativeBackend(goos, BackendMacOSSeatbelt, path, "macOS Seatbelt backend available")
		}
		return unavailableBackend(goos, "sandbox-exec is not available")
	case "windows":
		runner := findWindowsSandboxCommandRunner(lookup)
		setup := findWindowsSandboxSetupHelper(lookup)
		if runner.Available() && setup.Available() {
			backend := nativeBackend(goos, BackendWindowsRestrictedToken, runner.Name, "Windows sandbox command runner and setup helper available")
			backend.ExecutableArgsPrefix = runner.ArgsPrefix
			return backend
		}
		if runner.Available() {
			return unavailableBackend(goos, "Windows sandbox setup helper is not available")
		}
		return unavailableBackend(goos, "Windows sandbox command runner is not available")
	default:
		return unavailableBackend(goos, "no platform sandbox adapter is available for "+goos)
	}
}

func (manager SandboxManager) BuildExecutionRequest(request SandboxManagerRequest) (SandboxExecutionRequest, error) {
	policy := request.Policy
	if policy.Mode == "" {
		policy = DefaultPolicy()
	}
	preference := request.Preference
	if preference == "" {
		preference = SandboxPreferenceAuto
	}
	profile := request.Profile
	if permissionProfileUnset(profile) {
		profile = PermissionProfileFromPolicy(request.WorkspaceRoot, policy, request.Scope)
	}
	requiresPlatformSandbox := profile.RequiresPlatformSandbox() && preference != SandboxPreferenceForbid
	backend := manager.backend
	enforcementLevel := backend.EnforcementLevel(policy)
	if preference == SandboxPreferenceForbid || policy.Mode == ModeDisabled || !requiresPlatformSandbox {
		enforcementLevel = EnforcementDisabled
	}
	if request.ValidateExecution && preference == SandboxPreferenceRequire && backend.SupportLevel() != BackendSupportNative {
		return SandboxExecutionRequest{}, nativeSandboxUnavailableError(backend)
	}
	protectedCredentials := protectedCredentialPaths()
	protectedCredentialNeedsNative := policy.Mode != ModeDisabled && len(protectedCredentials) > 0
	if request.ValidateExecution && preference != SandboxPreferenceForbid && backend.SupportLevel() != BackendSupportNative &&
		(policyHasExplicitDeny(policy) || protectedCredentialNeedsNative) {
		return SandboxExecutionRequest{}, errors.New("native sandbox unavailable: configured deny rules or protected credentials cannot be enforced")
	}
	if request.ValidateExecution && preference != SandboxPreferenceForbid && policy.Mode != ModeDisabled &&
		manager.goos == "darwin" && len(protectedCredentials) > 0 {
		return SandboxExecutionRequest{}, errors.New("macOS Seatbelt cannot protect a file-backed remote token from inode aliases across its lifecycle; sandboxed shell commands require ZERO_DAEMON_REMOTE_TOKEN")
	}
	if request.ValidateExecution && preference != SandboxPreferenceForbid && policy.Mode != ModeDisabled &&
		manager.goos == "windows" && len(protectedCredentials) > 0 {
		return SandboxExecutionRequest{}, errors.New("the Windows sandbox enforces writes through ACLs and has no filesystem deny-read rule, so it cannot keep a file-backed remote token out of a sandboxed shell; sandboxed shell commands require ZERO_DAEMON_REMOTE_TOKEN")
	}
	if request.ValidateExecution && preference != SandboxPreferenceForbid && policy.Mode != ModeDisabled && manager.goos == "linux" {
		if credential, changed := protectedCredentialStartupIdentityChanged(protectedCredentials); changed {
			return SandboxExecutionRequest{}, fmt.Errorf("bubblewrap cannot protect the remote token file %q because it no longer names the token object opened at daemon startup; restart the daemon or restore the original token file", credential)
		}
		if credential, ok := protectedCredentialLinkableIntoLinuxShellRoot(profile, protectedCredentials); ok {
			return SandboxExecutionRequest{}, fmt.Errorf("bubblewrap cannot protect the remote token file %q from hard-link aliases in a shell-accessible root: a /dev/null bind covers one pathname, not the inode; use ZERO_DAEMON_REMOTE_TOKEN, place the file on a separate filesystem, or remove that root from the sandbox", credential)
		}
	}
	// Windows: the FULL OS sandbox needs a one-time elevated `zero sandbox setup`
	// (it applies WFP network filters + workspace ACLs and writes a marker).
	// Without it, a restricted-filesystem profile can still run in the UNELEVATED
	// tier: the command runner applies the workspace ACL plan itself (DACL edits
	// on user-owned roots need no Administrator rights) and wraps the command in
	// the write-restricted token, so the filesystem write-jail holds. Only the
	// WFP network filters are Administrator-only, so network stays with the
	// in-process approval gate at that tier. A profile that needs the sandbox
	// solely for network isolation gains nothing from the unelevated tier and
	// DEGRADES to the policy gate as before. A strict caller (--sandbox require)
	// still gets a clear error pointing at setup.
	windowsNeedsSetup := false
	if manager.goos == "windows" && requiresPlatformSandbox && enforcementLevel == EnforcementNative && !windowsSandboxInitialized() {
		if preference == SandboxPreferenceRequire {
			return SandboxExecutionRequest{}, errWindowsSandboxNotInitialized
		}
		windowsNeedsSetup = true
		if profile.FileSystem.Kind == FileSystemRestricted {
			enforcementLevel = EnforcementUnelevated
		} else {
			enforcementLevel = EnforcementDegraded
		}
	}
	targetBackend := manager.targetBackend(preference, policy, requiresPlatformSandbox)
	downgradeReason := ""
	if requiresPlatformSandbox && enforcementLevel == EnforcementDegraded {
		downgradeReason = backend.DowngradeReason(policy)
		if windowsNeedsSetup {
			downgradeReason = windowsSetupDowngradeReason
		}
	}
	if enforcementLevel == EnforcementUnelevated {
		downgradeReason = windowsUnelevatedDowngradeReason
	}
	wrapped := backend.CommandWrapping && backend.Available &&
		(enforcementLevel == EnforcementNative || enforcementLevel == EnforcementUnelevated)
	markers := backend.SandboxEnvMarkers(policy)
	if !wrapped && backend.Name != BackendWSL {
		markers = nil
	}
	return SandboxExecutionRequest{
		Command:                 request.Command,
		WorkspaceRoot:           request.WorkspaceRoot,
		PermissionProfile:       profile,
		Backend:                 backend,
		TargetBackend:           targetBackend,
		CommandWrapped:          wrapped,
		SandboxEnvMarkers:       markers,
		EnforcementLevel:        enforcementLevel,
		DowngradeReason:         downgradeReason,
		SupportLevel:            backend.SupportLevel(),
		RequiresPlatformSandbox: requiresPlatformSandbox,
	}, nil
}

func policyHasExplicitDeny(policy Policy) bool {
	return len(normalizeProfilePaths(policy.DenyRead)) > 0 || len(normalizeProfilePaths(policy.DenyWrite)) > 0
}

// Windows deliberately has no placement exception either: credentialDenyReadPaths
// returns nothing there (the ACL model has no read-deny rule — see #662), so a
// wrapped shell would run with the token readable under every pathname. The
// in-process tool gate still applies on Windows, which is what keeps read_file
// and friends covered while the shell boundary stays refused.
//
// macOS deliberately has no placement exception for file-backed tokens.
// Seatbelt can deny configured and resolved pathnames, but not every existing or
// future inode alias, and a restart authorizes the configured pathname again.
// BuildExecutionRequest therefore rejects shell execution whenever file-backed
// remote authentication is selected. In-process tools remain protected.

// protectedCredentialStartupIdentityChanged rejects sequential token rotation:
// masking a replacement pathname does not hide the still-authorized startup
// object after it has been renamed elsewhere. This is an admission check, not a
// lock over concurrent rotation and sandbox startup.
func protectedCredentialStartupIdentityChanged(protected []string) (string, bool) {
	source, selected := remotetoken.SourceFromEnv()
	if !selected || source.Identity == "" {
		return "", false
	}
	for _, path := range protected {
		file, err := os.Open(path)
		if err != nil {
			return path, true
		}
		identity, ok := remotetoken.IdentityOfFile(file)
		closeErr := file.Close()
		if !ok || closeErr != nil || identity != source.Identity {
			return path, true
		}
	}
	return "", false
}

// protectedCredentialLinkableIntoLinuxShellRoot reports a mandatory token that a
// shell command could reach through a second directory entry for the same inode,
// and the offending pathname.
//
// Bubblewrap masks each selected pathname. A READ root containing that masked
// spelling does not expose another alias by itself; an existing alias is proven
// separately by the link count. A FUTURE alias requires a shell-writable root on
// the same filesystem. Keeping those classes separate makes the documented
// separate-filesystem placement usable even when the normal read root is "/".
func protectedCredentialLinkableIntoLinuxShellRoot(profile PermissionProfile, protected []string) (string, bool) {
	if len(protected) == 0 {
		return "", false
	}
	if profile.FileSystem.Kind == FileSystemUnrestricted {
		return protected[0], true
	}
	if profile.FileSystem.Kind != FileSystemRestricted {
		return "", false
	}
	writeRoots := make([]string, 0, len(profile.FileSystem.WriteRoots)+len(sandboxWritableSubpaths))
	for _, root := range profile.FileSystem.WriteRoots {
		writeRoots = append(writeRoots, normalizeProfilePath(root.Root))
	}
	if profile.FileSystem.AllowTemp {
		writeRoots = append(writeRoots, normalizeProfilePaths(sandboxWritableSubpaths)...)
	}
	for _, credential := range protected {
		credential = filepath.Clean(credential)
		if credential == "." || credential == "" {
			continue
		}
		// An absent token has no inode to alias; the lexical pathname rule is
		// what reserves it through rotation. Any OTHER inspection failure leaves
		// the alias question unanswered, and an unanswered question about a
		// bearer token is a refusal.
		count, err := pathHardLinkCount(credential)
		switch {
		case err == nil && count > 1:
			return credential, true
		case err != nil && !errors.Is(err, fs.ErrNotExist):
			return credential, true
		}
		for _, root := range writeRoots {
			if root == "" {
				continue
			}
			if pathWithinRoot(root, credential) {
				return credential, true
			}
			// An uninspectable root is not proof of a separate filesystem: it is
			// a root a hard link might still be creatable in.
			if shared, known := pathsShareFilesystem(root, credential); !known || shared {
				return credential, true
			}
		}
	}
	return "", false
}

func (manager SandboxManager) BuildCommandPlan(request SandboxManagerRequest) (CommandPlan, error) {
	execRequest, err := manager.BuildExecutionRequest(request)
	if err != nil {
		return CommandPlan{}, err
	}
	policy := request.Policy
	if policy.Mode == "" {
		policy = DefaultPolicy()
	}
	return buildPlatformCommandPlan(execRequest, policy)
}

func (manager SandboxManager) targetBackend(preference SandboxPreference, policy Policy, requiresPlatformSandbox bool) BackendName {
	if preference == SandboxPreferenceForbid || policy.Mode == ModeDisabled || !requiresPlatformSandbox {
		return BackendNone
	}
	if manager.backend.Name == BackendWSL {
		return BackendLinuxBwrap
	}
	return manager.backend.TargetBackend()
}

func (request SandboxExecutionRequest) BackendPlan(policy Policy) BackendPlan {
	return BackendPlan{
		Backend:                 request.Backend,
		TargetBackend:           request.TargetBackend,
		WorkspaceRoot:           request.WorkspaceRoot,
		Policy:                  policy,
		PermissionProfile:       request.PermissionProfile,
		CommandWrapped:          request.CommandWrapped,
		SandboxEnvMarkers:       request.SandboxEnvMarkers,
		EnforcementLevel:        request.EnforcementLevel,
		DowngradeReason:         request.DowngradeReason,
		SupportLevel:            request.SupportLevel,
		RequiresPlatformSandbox: request.RequiresPlatformSandbox,
		Capabilities:            request.Backend.Capabilities(policy),
		Restrictions:            request.Backend.restrictions(policy),
		Warnings:                request.Backend.Warnings(),
	}
}

func permissionProfileUnset(profile PermissionProfile) bool {
	return profile.FileSystem.Kind == "" && profile.Network.Mode == ""
}

func inferBackendCapabilities(backend Backend) Backend {
	if backend.Available && backend.Executable != "" {
		switch backend.Name {
		case BackendLinuxBwrap, BackendMacOSSeatbelt, BackendWindowsRestrictedToken:
			backend.CommandWrapping = true
			backend.NativeIsolation = true
		}
	}
	return backend
}
