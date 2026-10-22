package sandbox

import (
	"fmt"
	"os"
	"strings"
)

// EnvSandboxed marks a process that zero has already wrapped in a sandbox: every
// wrapped command carries ZERO_SANDBOXED=1 in its environment. When such a
// process spawns another command through the engine, the re-entrancy guard
// returns a pass-through plan instead of double-wrapping it; nested platform
// wrappers fail, and a second sandbox wrapper would be redundant. Unset by
// default.
const EnvSandboxed = "ZERO_SANDBOXED"

// EnvSandboxBackend records which backend wrapped the command. sandboxEnvironment
// always sets it alongside EnvSandboxed, so it serves as a corroborating marker:
// the re-entrancy guard requires BOTH, raising the provenance bar above a single
// ambient flag (a stray or hand-exported ZERO_SANDBOXED=1 with no backend marker
// no longer forces an unsandboxed pass-through).
const EnvSandboxBackend = "ZERO_SANDBOX_BACKEND"

// IsAlreadySandboxed reports whether the current process is already running
// inside a zero-created sandbox. It requires BOTH correlated markers that
// sandboxEnvironment sets together — EnvSandboxed == "1" AND a non-empty
// EnvSandboxBackend — so a single user-set/inherited ZERO_SANDBOXED=1 cannot by
// itself disable wrapping. zero sets both only on genuinely wrapped commands;
// pass-through (direct) plans set neither.
func IsAlreadySandboxed() bool {
	return os.Getenv(EnvSandboxed) == "1" && strings.TrimSpace(os.Getenv(EnvSandboxBackend)) != ""
}

type SideEffect string
type Permission string
type PermissionMode string
type PolicyMode string
type NetworkMode string
type Action string
type RiskLevel string
type BlockCode string
type GrantDecision string
type BackendName string
type BackendSupportLevel string
type CapabilityStatus string
type EnforcementLevel string

const (
	SideEffectRead           SideEffect = "read"
	SideEffectWrite          SideEffect = "write"
	SideEffectShell          SideEffect = "shell"
	SideEffectNetwork        SideEffect = "network"
	SideEffectLocalControl   SideEffect = "local_control"
	SideEffectLocalBrowser   SideEffect = "local_browser"
	SideEffectLocalDesktop   SideEffect = "local_desktop"
	SideEffectLocalTerminal  SideEffect = "local_terminal"
	SideEffectOutOfWorkspace SideEffect = "out_of_workspace"
	// SideEffectNone marks a control-only tool that performs no read/write/
	// shell/network effect (e.g. escalate_model). It must be recognized so it is
	// not normalized to out_of_workspace and falsely classified as critical.
	SideEffectNone SideEffect = "none"
)

const (
	PermissionAllow  Permission = "allow"
	PermissionPrompt Permission = "prompt"
	PermissionDeny   Permission = "deny"
)

const (
	PermissionModeAuto PermissionMode = "auto"
	PermissionModeAsk  PermissionMode = "ask"
	PermissionFullAuto PermissionMode = "full-auto"
	// Deprecated: use PermissionFullAuto. Kept as an alias so code landing
	// independently of this branch still compiles after a merge.
	PermissionUnsafe = PermissionFullAuto
)

const (
	ModeDisabled PolicyMode = "disabled"
	ModeEnforce  PolicyMode = "enforce"
)

const (
	NetworkDeny  NetworkMode = "deny"
	NetworkAllow NetworkMode = "allow"
)

const (
	ActionAllow  Action = "allow"
	ActionPrompt Action = "prompt"
	ActionDeny   Action = "deny"
)

const (
	RiskLow      RiskLevel = "low"
	RiskMedium   RiskLevel = "medium"
	RiskHigh     RiskLevel = "high"
	RiskCritical RiskLevel = "critical"
)

const (
	BlockContextCanceled    BlockCode = "context_canceled"
	BlockDeniedPermission   BlockCode = "denied_permission"
	BlockOutsideWorkspace   BlockCode = "outside_workspace"
	BlockSymlinkTraversal   BlockCode = "symlink_traversal"
	BlockNetwork            BlockCode = "network"
	BlockDestructiveCommand BlockCode = "destructive_command"
	BlockPersistentDeny     BlockCode = "persistent_deny"
	// BlockDenied is the catch-all for a denied decision that carries no more
	// specific block code.
	BlockDenied BlockCode = "denied"
)

const ReasonNetworkBlocked = "network access requires approval"
const ReasonEscalatedSandboxRequired = "unsandboxed shell command requires approval"

const (
	GrantAllow GrantDecision = "allow"
	GrantDeny  GrantDecision = "deny"
)

const (
	BackendNone                   BackendName = "none"
	BackendMacOSSeatbelt          BackendName = "macos-seatbelt"
	BackendLinuxBwrap             BackendName = "linux-bwrap"
	BackendLinuxLandlock          BackendName = "linux-landlock"
	BackendWindowsRestrictedToken BackendName = "windows-restricted-token"
	BackendWindowsElevated        BackendName = "windows-elevated"
	BackendUnavailable            BackendName = "unavailable"
	// BackendWSL records that WSL was detected but native Linux sandbox wrapping
	// is unavailable or unreliable in that environment.
	BackendWSL BackendName = "wsl"
)

const (
	BackendSupportNative      BackendSupportLevel = "native"
	BackendSupportUnavailable BackendSupportLevel = "unavailable"
)

const (
	CapabilityNative      CapabilityStatus = "native"
	CapabilityPreflight   CapabilityStatus = "preflight"
	CapabilityUnavailable CapabilityStatus = "unavailable"
	CapabilityDisabled    CapabilityStatus = "disabled"
)

const (
	EnforcementNative EnforcementLevel = "native"
	// EnforcementUnelevated is the Windows-only middle tier used when the
	// elevated `zero sandbox setup` has not run: commands still wrap through the
	// command runner with a write-restricted token and workspace ACLs (which the
	// runner can apply without Administrator rights), but the WFP network
	// filters are absent, so network isolation stays with the in-process
	// approval gate.
	EnforcementUnelevated EnforcementLevel = "unelevated"
	EnforcementDegraded   EnforcementLevel = "degraded"
	EnforcementDisabled   EnforcementLevel = "disabled"
)

type Policy struct {
	Mode             PolicyMode  `json:"mode"`
	Network          NetworkMode `json:"network"`
	EnforceWorkspace bool        `json:"enforceWorkspace"`
	// BlockUnixSockets, when true on the Linux helper backend, installs a
	// best-effort seccomp filter in the inner helper stage that denies AF_UNIX
	// socket creation. It is an extra hardening layer over the native sandbox and
	// is ignored on non-Linux backends.
	BlockUnixSockets bool `json:"blockUnixSockets,omitempty"`
	// MonitorDenials, when true on macOS, tags the sandbox-exec profile's denials
	// and tails `log stream` for them so blocked operations can be surfaced back to
	// the agent. Off by default: it starts a `log stream` subprocess per command and
	// appends a <sandbox_blocks> note to the command's stderr, so it is opt-in.
	// Ignored on non-macOS backends, and a no-op where the OS does not deliver
	// seatbelt denials to the unified log.
	MonitorDenials bool `json:"monitorDenials,omitempty"`
	// AllowRead/DenyRead/AllowWrite/DenyWrite are fine-grained path lists layered
	// ON TOP of the workspace + Scope guards; they never bypass the symlink /
	// out-of-workspace protections. Each entry is home-expanded, made absolute, and
	// symlink-resolved (an entry that does not exist is dropped). All default empty,
	// so an unconfigured policy behaves exactly as before. Semantics:
	//
	//   - Read: a path readable under the base workspace/Scope guard is denied if it
	//     falls under a DenyRead entry, UNLESS a more-specific AllowRead entry (one
	//     nested inside that DenyRead entry) re-includes it. AllowRead only
	//     re-includes within a DenyRead carve-out; it never extends reads beyond the
	//     workspace.
	//   - Write: DenyWrite wins over everything; otherwise a path writable under the
	//     workspace/Scope guard is allowed; otherwise an absolute path under an
	//     AllowWrite root is allowed; otherwise it is denied. AllowWrite roots are
	//     also reflected in the OS backend write binds, and on sandbox-exec DenyWrite
	//     entries are emitted as explicit deny rules so the precedence holds for
	//     shell commands too.
	AllowRead  []string `json:"allowRead,omitempty"`
	DenyRead   []string `json:"denyRead,omitempty"`
	AllowWrite []string `json:"allowWrite,omitempty"`
	DenyWrite  []string `json:"denyWrite,omitempty"`
}

type Request struct {
	WorkspaceRoot     string         `json:"workspaceRoot,omitempty"`
	ToolName          string         `json:"toolName"`
	SideEffect        SideEffect     `json:"sideEffect"`
	Permission        Permission     `json:"permission"`
	PermissionGranted bool           `json:"permissionGranted,omitempty"`
	PermissionMode    PermissionMode `json:"permissionMode"`
	Args              map[string]any `json:"args,omitempty"`
	Reason            string         `json:"reason,omitempty"`
}

type Decision struct {
	Action       Action `json:"action"`
	Reason       string `json:"reason,omitempty"`
	Risk         Risk   `json:"risk"`
	GrantMatched bool   `json:"grantMatched,omitempty"`
	Grant        *Grant `json:"grant,omitempty"`
	Block        *Block `json:"block,omitempty"`
	// AutoAllowed marks an allow that the sandbox itself authorized without a user
	// prompt or persistent grant, such as a workspace-write file mutation or an
	// opted-in sandboxed shell command. Enforcement points treat it like a
	// grant-authorized allow so a prompt tool runs without a separately-recorded
	// PermissionGranted.
	AutoAllowed bool `json:"autoAllowed,omitempty"`
}

type Risk struct {
	Level      RiskLevel `json:"level"`
	Categories []string  `json:"categories,omitempty"`
	Reason     string    `json:"reason,omitempty"`
}

type Block struct {
	Code        BlockCode `json:"code"`
	ToolName    string    `json:"toolName,omitempty"`
	Action      Action    `json:"action"`
	Risk        Risk      `json:"risk"`
	Path        string    `json:"path,omitempty"`
	Reason      string    `json:"reason"`
	Recoverable bool      `json:"recoverable"`
}

func (block Block) Error() string {
	if block.Path != "" {
		return fmt.Sprintf("Sandbox block [%s] for %s at %s: %s", block.Code, block.ToolName, block.Path, block.Reason)
	}
	return fmt.Sprintf("Sandbox block [%s] for %s: %s", block.Code, block.ToolName, block.Reason)
}

func (decision Decision) ErrorString() string {
	if decision.Block != nil {
		return decision.Block.Error()
	}
	if decision.Reason != "" {
		return "Sandbox decision: " + decision.Reason
	}
	return "Sandbox decision denied."
}

func DefaultPolicy() Policy {
	return Policy{
		Mode:             ModeEnforce,
		Network:          NetworkDeny,
		EnforceWorkspace: true,
	}
}
