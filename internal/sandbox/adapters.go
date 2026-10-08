package sandbox

import (
	"strings"
)

type BackendOptions struct {
	GOOS             string
	LookupExecutable func(string) (string, error)
	DetectWSL        func() WSLInfo
}

type Backend struct {
	Name            BackendName `json:"name"`
	Available       bool        `json:"available"`
	Platform        string      `json:"platform,omitempty"`
	Fallback        bool        `json:"fallback"`
	CommandWrapping bool        `json:"commandWrapping"`
	NativeIsolation bool        `json:"nativeIsolation"`
	Executable      string      `json:"executable,omitempty"`
	// ExecutableArgsPrefix is prepended to a wrapped command's args before the
	// sandbox arguments. Non-empty only for the Windows self-dispatch helper,
	// where Executable is the running zero binary and this carries the hidden
	// subcommand token (e.g. "__windows-command-runner"). nil for every other
	// backend, so their serialized form is unchanged.
	ExecutableArgsPrefix []string `json:"executableArgsPrefix,omitempty"`
	Message              string   `json:"message,omitempty"`
}

type BackendPlan struct {
	Backend                 Backend             `json:"backend"`
	TargetBackend           BackendName         `json:"targetBackend"`
	WorkspaceRoot           string              `json:"workspaceRoot"`
	Policy                  Policy              `json:"policy"`
	PermissionProfile       PermissionProfile   `json:"permissionProfile"`
	CommandWrapped          bool                `json:"commandWrapped"`
	SandboxEnvMarkers       []string            `json:"sandboxEnvMarkers,omitempty"`
	EnforcementLevel        EnforcementLevel    `json:"enforcementLevel"`
	DowngradeReason         string              `json:"downgradeReason,omitempty"`
	SupportLevel            BackendSupportLevel `json:"supportLevel"`
	RequiresPlatformSandbox bool                `json:"requiresPlatformSandbox"`
	Capabilities            []BackendCapability `json:"capabilities"`
	Restrictions            []string            `json:"restrictions"`
	Warnings                []string            `json:"warnings,omitempty"`
}

type BackendCapability struct {
	Key    string           `json:"key"`
	Status CapabilityStatus `json:"status"`
	Detail string           `json:"detail"`
}

func SelectBackend(options BackendOptions) Backend {
	return NewSandboxManager(SandboxManagerOptions{
		GOOS:             options.GOOS,
		LookupExecutable: options.LookupExecutable,
		DetectWSL:        options.DetectWSL,
	}).Backend()
}

func TargetBackendForPlatform(goos string, wsl bool) BackendName {
	switch goos {
	case "darwin":
		return BackendMacOSSeatbelt
	case "linux":
		if wsl {
			return BackendLinuxBwrap
		}
		return BackendLinuxBwrap
	case "windows":
		return BackendWindowsRestrictedToken
	default:
		return BackendUnavailable
	}
}

func (backend Backend) TargetBackend() BackendName {
	if backend.Platform == "windows" {
		if backend.Name == BackendWindowsElevated || backend.Name == BackendWindowsRestrictedToken {
			return backend.Name
		}
		return BackendWindowsRestrictedToken
	}
	switch backend.Name {
	case BackendWSL:
		return BackendLinuxBwrap
	case BackendNone, BackendMacOSSeatbelt, BackendLinuxBwrap, BackendLinuxLandlock, BackendWindowsRestrictedToken, BackendWindowsElevated, BackendUnavailable:
		return backend.Name
	default:
		return TargetBackendForPlatform(backend.Platform, false)
	}
}

func nativeBackend(goos string, name BackendName, executable string, message string) Backend {
	return Backend{
		Name:            name,
		Available:       true,
		Platform:        goos,
		Fallback:        false,
		CommandWrapping: true,
		NativeIsolation: true,
		Executable:      executable,
		Message:         message,
	}
}

// wslBackend records WSL native isolation unavailability for diagnostics.
func wslBackend(goos string, info WSLInfo) Backend {
	msg := "native Linux sandbox unavailable under WSL"
	if info.IsWSL2 {
		msg = "native Linux sandbox unavailable or unreliable under WSL2"
	}
	return Backend{
		Name:            BackendWSL,
		Available:       false,
		Platform:        goos,
		Fallback:        true,
		CommandWrapping: false,
		NativeIsolation: false,
		Message:         msg,
	}
}

func unavailableBackend(goos string, message string) Backend {
	return Backend{
		Name:            BackendUnavailable,
		Available:       false,
		Platform:        goos,
		Fallback:        true,
		CommandWrapping: false,
		NativeIsolation: false,
		Message:         message,
	}
}

// BuildPlan renders the plan for introspection (`zero sandbox policy`). It has
// no command to describe, so the profile is built with no command context: the
// credential baseline resolves relative token overrides against THIS process's
// environment and working directory only. Execution goes through
// BuildCommandPlan instead, which passes the command's own dir and env and can
// therefore produce additional deny-if-exists entries. Callers exporting this
// plan should say which of the two they are showing.
func (backend Backend) BuildPlan(workspaceRoot string, policy Policy) BackendPlan {
	effectivePolicy := policy
	if effectivePolicy.Mode == "" {
		effectivePolicy = DefaultPolicy()
	}
	profile := PermissionProfileFromPolicy(workspaceRoot, effectivePolicy, nil)
	execRequest, _ := NewSandboxManager(SandboxManagerOptions{
		GOOS:    backend.Platform,
		Backend: backend,
	}).BuildExecutionRequest(SandboxManagerRequest{
		WorkspaceRoot: workspaceRoot,
		Policy:        effectivePolicy,
		Profile:       profile,
		Preference:    SandboxPreferenceAuto,
	})
	return execRequest.BackendPlan(effectivePolicy)
}

func (backend Backend) restrictions(policy Policy) []string {
	effectivePolicy := policy
	if effectivePolicy.Mode == "" {
		effectivePolicy = DefaultPolicy()
	}
	restrictions := []string{}
	if effectivePolicy.EnforceWorkspace {
		restrictions = append(restrictions, "filesystem writes must stay inside workspace")
	}
	if NormalizeNetworkMode(effectivePolicy.Network) == NetworkDeny {
		if backend.Name == BackendWindowsRestrictedToken && backend.NativeIsolation {
			restrictions = append(restrictions, "Windows WFP filters block outbound network for sandbox identities")
		} else {
			restrictions = append(restrictions, "network access denied unless a future adapter grants it explicitly")
		}
	}
	if backend.Name == BackendUnavailable {
		platform := backend.Platform
		if platform == "" {
			platform = "this platform"
		}
		restrictions = append(restrictions, "native process isolation unavailable on "+platform)
		restrictions = append(restrictions, "shell commands are not wrapped by a native platform sandbox")
	} else if backend.Available {
		restrictions = append(restrictions, "shell commands are wrapped through "+string(backend.Name)+" when launched by the sandbox engine")
	}
	return restrictions
}

func (backend Backend) SupportLevel() BackendSupportLevel {
	if backend.Available && backend.NativeIsolation && backend.CommandWrapping {
		return BackendSupportNative
	}
	return BackendSupportUnavailable
}

func (backend Backend) EnforcementLevel(policy Policy) EnforcementLevel {
	if policy.Mode == "" {
		policy = DefaultPolicy()
	}
	if policy.Mode == ModeDisabled {
		return EnforcementDisabled
	}
	if backend.SupportLevel() == BackendSupportNative {
		return EnforcementNative
	}
	return EnforcementDegraded
}

func (backend Backend) DowngradeReason(policy Policy) string {
	if policy.Mode == "" {
		policy = DefaultPolicy()
	}
	if policy.Mode == ModeDisabled {
		return "sandbox disabled"
	}
	if backend.SupportLevel() == BackendSupportNative {
		return ""
	}
	if strings.TrimSpace(backend.Message) != "" {
		return backend.Message
	}
	platform := backend.Platform
	if platform == "" {
		platform = "this platform"
	}
	return "native sandbox unavailable on " + platform
}

func (backend Backend) SandboxEnvMarkers(policy Policy) []string {
	if policy.Mode == "" {
		policy = DefaultPolicy()
	}
	if policy.Mode == ModeDisabled {
		return nil
	}
	if (!backend.CommandWrapping || !backend.Available) && backend.Name != BackendWSL {
		return nil
	}
	name := backend.Name
	if name == "" {
		name = BackendUnavailable
	}
	return []string{
		EnvSandboxed + "=1",
		EnvSandboxBackend + "=" + string(name),
		"ZERO_SANDBOX_NETWORK=" + string(policy.Network),
	}
}

func (backend Backend) Warnings() []string {
	if backend.SupportLevel() == BackendSupportNative {
		return nil
	}
	platform := backend.Platform
	if platform == "" {
		platform = "this platform"
	}
	warnings := []string{
		"native process isolation unavailable on " + platform,
		"shell commands are not wrapped by a native platform sandbox",
	}
	if backend.Platform == "windows" {
		warnings[0] = "Windows sandbox command runner is not available"
	}
	return warnings
}

func (backend Backend) Capabilities(policy Policy) []BackendCapability {
	if policy.Mode == "" {
		policy = DefaultPolicy()
	}
	networkGuard := BackendCapability{
		Key:    "network_guard",
		Status: policyCapabilityStatus(policy.Mode, policy.Network == NetworkDeny),
		Detail: "network-capable tool requests are denied before execution",
	}
	if policy.Mode != ModeDisabled && policy.Network == NetworkDeny && backend.Name == BackendWindowsRestrictedToken && backend.NativeIsolation {
		networkGuard.Status = CapabilityNative
		networkGuard.Detail = "Windows WFP filters block outbound network for sandbox identities"
	}
	capabilities := []BackendCapability{
		{
			Key:    "permission_review",
			Status: policyCapabilityStatus(policy.Mode, true),
			Detail: "tool requests are reviewed before execution",
		},
		{
			Key:    "workspace_write_guard",
			Status: policyCapabilityStatus(policy.Mode, policy.EnforceWorkspace),
			Detail: "filesystem writes are checked against the workspace root before execution",
		},
		networkGuard,
	}
	nativeIsolation := BackendCapability{
		Key:    "native_process_isolation",
		Status: CapabilityUnavailable,
		Detail: "no native process sandbox is active for this platform",
	}
	if backend.NativeIsolation {
		nativeIsolation.Status = CapabilityNative
		nativeIsolation.Detail = "tool subprocesses can run inside " + string(backend.Name)
	} else if backend.Platform == "windows" {
		nativeIsolation.Detail = "Windows sandbox command runner is not available"
	}
	commandWrapping := BackendCapability{
		Key:    "command_wrapping",
		Status: CapabilityUnavailable,
		Detail: "shell commands are not wrapped by a native platform sandbox",
	}
	if backend.CommandWrapping {
		commandWrapping.Status = CapabilityNative
		commandWrapping.Detail = "shell commands can be launched through " + string(backend.Name)
	}
	return append(capabilities, nativeIsolation, commandWrapping)
}

func policyCapabilityStatus(mode PolicyMode, enabled bool) CapabilityStatus {
	if mode == ModeDisabled || !enabled {
		return CapabilityDisabled
	}
	return CapabilityPreflight
}
