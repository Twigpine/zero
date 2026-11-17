// Package execution defines the platform-neutral command execution contract.
// Frontends and tools use these types without depending on native sandbox or
// process-management details.
package execution

import (
	"errors"
	"fmt"
	"strings"
)

// PolicyVersion is the version of the approval policy that recorded grants were
// evaluated under. Bump it whenever a change narrows what may be approved: the
// grant store then backs the file up, keeps deny grants, invalidates prior
// approvals, and tells the user once, instead of silently honoring approvals
// that the current rules would have refused.
//
// v2 tightened command-prefix validation so a launcher spelled with a version
// or an executable extension ("python3.11", "python.exe") can no longer be
// approved as an ordinary command.
const PolicyVersion = 2

type Origin string

const (
	OriginInteractiveCommand Origin = "interactive_command"
	OriginHook               Origin = "hook"
	OriginPlugin             Origin = "plugin"
	OriginMCPServer          Origin = "mcp_server"
	OriginBackgroundTask     Origin = "background_task"
)

type Mode string

const (
	ModeCaptured    Mode = "captured"
	ModeInteractive Mode = "interactive"
	ModeDurable     Mode = "durable"
)

type Command struct {
	Name string   `json:"name"`
	Args []string `json:"args,omitempty"`
	Env  []string `json:"env,omitempty"`
}

type CapabilityKind string

const (
	CapabilityRead               CapabilityKind = "filesystem_read"
	CapabilityWorkspaceWrite     CapabilityKind = "workspace_write"
	CapabilityProtectedMetadata  CapabilityKind = "protected_metadata"
	CapabilityExternalNetwork    CapabilityKind = "external_network"
	CapabilityIsolatedLoopback   CapabilityKind = "isolated_loopback"
	CapabilityHostVisibleBinding CapabilityKind = "host_visible_binding"
	CapabilityProcessSpawn       CapabilityKind = "process_spawn"
	CapabilityProcessControl     CapabilityKind = "process_control"
	CapabilityEnvironment        CapabilityKind = "environment"
	CapabilityUnrestricted       CapabilityKind = "unrestricted"
)

type Capability struct {
	Kind  CapabilityKind `json:"kind"`
	Scope string         `json:"scope,omitempty"`
}

type ApprovalContext struct {
	SessionID     string `json:"sessionId,omitempty"`
	PolicyVersion int    `json:"policyVersion"`
}

type Request struct {
	Origin           Origin          `json:"origin"`
	Mode             Mode            `json:"mode"`
	Command          Command         `json:"command"`
	WorkingDirectory string          `json:"workingDirectory"`
	WorkspaceRoots   []string        `json:"workspaceRoots"`
	Capabilities     []Capability    `json:"capabilities,omitempty"`
	Approval         ApprovalContext `json:"approval"`
}

func (request Request) Validate() error {
	if !validOrigin(request.Origin) {
		return errors.New("execution request requires a valid origin")
	}
	if request.Mode != ModeCaptured && request.Mode != ModeInteractive && request.Mode != ModeDurable {
		return errors.New("execution request requires a valid mode")
	}
	if strings.TrimSpace(request.Command.Name) == "" {
		return errors.New("execution request requires a command")
	}
	if strings.TrimSpace(request.WorkingDirectory) == "" {
		return errors.New("execution request requires a working directory")
	}
	if len(request.WorkspaceRoots) == 0 {
		return errors.New("execution request requires at least one workspace root")
	}
	for _, root := range request.WorkspaceRoots {
		if strings.TrimSpace(root) == "" {
			return errors.New("execution request contains an empty workspace root")
		}
	}
	for _, capability := range request.Capabilities {
		if !validCapabilityKind(capability.Kind) {
			return fmt.Errorf("execution request contains invalid capability %q", capability.Kind)
		}
	}
	return nil
}

type State string

const (
	StateRequested        State = "requested"
	StateEvaluated        State = "evaluated"
	StateAwaitingApproval State = "awaiting_approval"
	StatePrepared         State = "prepared"
	StateRunning          State = "running"
	StateRetained         State = "retained"
	StateCompleted        State = "completed"
	StateDenied           State = "denied"
	StateFailed           State = "failed"
	StateCancelled        State = "cancelled"
)

type OutcomeKind string

const (
	OutcomeRunning             OutcomeKind = "running"
	OutcomeSuccess             OutcomeKind = "success"
	OutcomePolicyDenied        OutcomeKind = "policy_denied"
	OutcomeEnforcementDenied   OutcomeKind = "enforcement_denied"
	OutcomeApplicationFailure  OutcomeKind = "application_failure"
	OutcomeSandboxSetupFailure OutcomeKind = "sandbox_setup_failure"
	OutcomeExecutableNotFound  OutcomeKind = "executable_not_found"
	OutcomeCancelled           OutcomeKind = "cancelled"
	OutcomeTimedOut            OutcomeKind = "timed_out"
	OutcomeEnforcementDegraded OutcomeKind = "enforcement_degraded"
)

type Exit struct {
	Code   int    `json:"code"`
	Signal string `json:"signal,omitempty"`
}

type DenialSource string

const (
	DenialSourceConfiguredPolicy DenialSource = "configured_policy"
	DenialSourcePlatformSandbox  DenialSource = "platform_sandbox"
	DenialSourceApproval         DenialSource = "approval"
)

type DenialNextAction string

const (
	DenialNextActionNone            DenialNextAction = "none"
	DenialNextActionRequestApproval DenialNextAction = "request_approval"
	DenialNextActionChangeCommand   DenialNextAction = "change_command"
	DenialNextActionEnableSandbox   DenialNextAction = "enable_sandbox"
)

type Denial struct {
	Capability  Capability       `json:"capability"`
	Source      DenialSource     `json:"source"`
	Reason      string           `json:"reason"`
	Recoverable bool             `json:"recoverable"`
	NextAction  DenialNextAction `json:"nextAction"`
}

type Enforcement struct {
	Backend         string `json:"backend,omitempty"`
	Level           string `json:"level,omitempty"`
	Degraded        bool   `json:"degraded,omitempty"`
	DowngradeReason string `json:"downgradeReason,omitempty"`
	// Notices are least-privilege disclosures about the enforcement actually
	// applied to THIS command, as opposed to the diagnostic views produced by
	// `zero sandbox policy` and `zero sandbox check`. A trade an operator only
	// discovers by running a separate diagnostic command is not disclosed.
	Notices []string `json:"notices,omitempty"`
}

type Outcome struct {
	State       State       `json:"state"`
	Kind        OutcomeKind `json:"kind"`
	ProcessID   string      `json:"processId,omitempty"`
	Exit        *Exit       `json:"exit,omitempty"`
	Denial      *Denial     `json:"denial,omitempty"`
	Enforcement Enforcement `json:"enforcement"`
	// Launched records whether an OS process was actually created, observed at
	// the boundary that calls Run rather than inferred afterwards.
	//
	// OutcomeKind is not a launch-state field, and reading it as one is wrong in
	// both directions. A child that ran and then produced an unreadable adapter
	// report is rewritten to a setup failure, so inference drops a disclosure that
	// did apply; a context already cancelled before os.StartProcess yields a
	// cancellation, so inference claims reduced enforcement for a child that never
	// existed. Report decoding can fail after launch without rewriting history,
	// and cancellation happens on either side of Start.
	Launched bool     `json:"launched,omitempty"`
	Changes  []Change `json:"changes,omitempty"`
}

// AdapterReport is the structured, machine-readable result emitted by a
// platform enforcement adapter. It is separate from stdout and stderr so
// command text cannot impersonate a policy decision.
type AdapterReport struct {
	Denial *Denial `json:"denial,omitempty"`
	// ChildLaunched is the adapter's authoritative statement that the REQUESTED
	// process started, for a plan where the command the runner starts is not that
	// process.
	//
	// A wrapped plan starts a helper, and the helper creates the sandboxed child
	// only after validating the setup marker, applying ACLs, checking the network
	// policy, building capability SIDs and minting the restricted token. Any of
	// those can fail with the helper already running, so the runner's own
	// exec.Cmd.Process tells it the WRAPPER started and nothing about the child.
	// Only the adapter sees that transition, so only the adapter may report it.
	//
	// nil means the adapter does not speak to this, and the runner keeps its own
	// observation. That is correct for every direct, unwrapped command, where the
	// process the runner starts IS the requested one.
	ChildLaunched *bool `json:"childLaunched,omitempty"`
}

// ChildLaunched reports whether this outcome describes a process that actually
// started, from the recorded fact rather than from the terminal outcome kind.
func (outcome Outcome) ChildLaunched() bool {
	return outcome.Launched
}

// AppliedEnforcementNotices returns the least-privilege disclosures that are
// true of what actually happened.
//
// ONE DECISION, AT THE BOUNDARY WHERE THE OUTCOME IS KNOWN. Enforcement.Notices
// is planned: it describes the shape the command was PREPARED to run under, and
// planning is not proof that anything ran. Every consumer that copied the field
// straight out therefore made the completed-enforcement claim for commands that
// never launched, telling an operator the write jail had been traded away for a
// child that failed before it existed.
//
// Keeping this on Outcome rather than repeating an outcome-kind switch in hooks,
// plugins and tools is the point: a new pre-launch outcome kind has to be
// classified once, here, instead of being silently disclosed by whichever
// consumer was not updated.
func (outcome Outcome) AppliedEnforcementNotices() []string {
	if !outcome.ChildLaunched() {
		return nil
	}
	if len(outcome.Enforcement.Notices) == 0 {
		return nil
	}
	return append([]string(nil), outcome.Enforcement.Notices...)
}

func (outcome Outcome) Validate() error {
	if outcome.State == "" || outcome.Kind == "" {
		return errors.New("execution outcome requires state and kind")
	}
	switch outcome.State {
	case StateRetained, StateRunning:
		if outcome.Kind != OutcomeRunning || strings.TrimSpace(outcome.ProcessID) == "" {
			return errors.New("running execution outcome requires a process id")
		}
	case StateCompleted:
		if outcome.Kind != OutcomeSuccess || outcome.Exit == nil || outcome.Exit.Code != 0 {
			return errors.New("completed execution outcome requires a successful exit")
		}
	case StateDenied:
		if (outcome.Kind != OutcomePolicyDenied && outcome.Kind != OutcomeEnforcementDenied) || outcome.Denial == nil {
			return errors.New("denied execution outcome requires structured denial details")
		}
		if err := outcome.Denial.Validate(); err != nil {
			return err
		}
	case StateFailed:
		if outcome.Kind == OutcomeRunning || outcome.Kind == OutcomeSuccess || outcome.Exit == nil {
			return errors.New("failed execution outcome requires a failure kind and exit")
		}
	case StateCancelled:
		if outcome.Kind != OutcomeCancelled {
			return errors.New("cancelled execution outcome requires cancelled kind")
		}
	default:
		return fmt.Errorf("execution outcome state %q is not terminal or externally observable", outcome.State)
	}
	return nil
}

func (denial Denial) Validate() error {
	if !validCapabilityKind(denial.Capability.Kind) {
		return errors.New("execution denial requires a valid capability")
	}
	if denial.Source == "" || strings.TrimSpace(denial.Reason) == "" || denial.NextAction == "" {
		return errors.New("execution denial requires source, reason, and next action")
	}
	return nil
}

func validOrigin(origin Origin) bool {
	switch origin {
	case OriginInteractiveCommand, OriginHook, OriginPlugin, OriginMCPServer, OriginBackgroundTask:
		return true
	default:
		return false
	}
}

func validCapabilityKind(kind CapabilityKind) bool {
	switch kind {
	case CapabilityRead, CapabilityWorkspaceWrite, CapabilityProtectedMetadata,
		CapabilityExternalNetwork, CapabilityIsolatedLoopback, CapabilityHostVisibleBinding,
		CapabilityProcessSpawn, CapabilityProcessControl, CapabilityEnvironment, CapabilityUnrestricted:
		return true
	default:
		return false
	}
}
