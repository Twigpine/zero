package tools

import "strings"

// EffectClass classifies a tool's side effects for concurrency safety.
// Concurrent execution (a later PR) must only consider tools that are both
// ThreadSafe and EffectReadOnly; this package only declares metadata.
// ThreadSafe is force-cleared for every non-ReadOnly effect at read time
// (see normalizeCapabilities).
type EffectClass int

const (
	// EffectUnknown is the fail-closed default for unclassified tools.
	// Never concurrency-eligible; always serialized.
	EffectUnknown EffectClass = iota
	// EffectReadOnly reads local or remote state without mutating it.
	// The only effect class that may set ThreadSafe=true after audit.
	EffectReadOnly
	// EffectProcessRead observes a retained process, PTY, or terminal.
	EffectProcessRead
	// EffectWorkspaceWrite mutates the local repository or workspace.
	// Includes one-shot shell commands that can rewrite the workspace.
	EffectWorkspaceWrite
	// EffectExternalWrite mutates state outside the local workspace.
	EffectExternalWrite
	// EffectInteractive uses browser, PTY/terminal, prompts, or other
	// session-bound interaction where ordering matters.
	EffectInteractive
)

// String returns a stable diagnostic name for the effect class.
func (e EffectClass) String() string {
	switch e {
	case EffectUnknown:
		return "unknown"
	case EffectReadOnly:
		return "read_only"
	case EffectProcessRead:
		return "process_read"
	case EffectWorkspaceWrite:
		return "workspace_write"
	case EffectExternalWrite:
		return "external_write"
	case EffectInteractive:
		return "interactive"
	default:
		return "invalid"
	}
}

// Valid reports whether e is one of the defined effect classes.
func (e EffectClass) Valid() bool {
	return e >= EffectUnknown && e <= EffectInteractive
}

// ToolCapabilities is the concurrency-safety contract for a tool.
// ResourceKeys, when set, returns deterministic conflict keys for a call's
// arguments. Returning no keys does NOT make a tool parallel-safe.
type ToolCapabilities struct {
	Effect       EffectClass
	ThreadSafe   bool
	ResourceKeys func(args map[string]any) []string
}

// CapabilityProvider is the optional interface tools implement to declare
// effect metadata. Tools that do not implement it are treated as EffectUnknown
// and not thread-safe (fail-closed).
type CapabilityProvider interface {
	Capabilities() ToolCapabilities
}

// UnknownCapabilities is the fail-closed default for tools without metadata.
func UnknownCapabilities() ToolCapabilities {
	return ToolCapabilities{
		Effect:     EffectUnknown,
		ThreadSafe: false,
	}
}

// normalizeCapabilities is the single read path for capability metadata.
// ThreadSafe is cleared unless Effect is a valid EffectReadOnly — mutators,
// interactive tools, process readers, unknown, and invalid effects must never
// appear concurrency-eligible to a future batch planner that only checks
// ThreadSafe && Effect.
func normalizeCapabilities(caps ToolCapabilities) ToolCapabilities {
	if !caps.Effect.Valid() || caps.Effect != EffectReadOnly {
		caps.ThreadSafe = false
	}
	return caps
}

// CapabilitiesOf returns a tool's declared capabilities, or the fail-closed
// unknown default when the tool does not implement CapabilityProvider.
func CapabilitiesOf(tool Tool) ToolCapabilities {
	if tool == nil {
		return UnknownCapabilities()
	}
	if provider, ok := tool.(CapabilityProvider); ok {
		return normalizeCapabilities(provider.Capabilities())
	}
	return UnknownCapabilities()
}

// (baseTool).Capabilities exposes the capabilities field set at construction,
// normalized so ThreadSafe cannot surface on non-ReadOnly effects.
// Tools that embed baseTool inherit this method.
func (tool baseTool) Capabilities() ToolCapabilities {
	return normalizeCapabilities(tool.capabilities)
}

// declaredCapabilities returns the constructor-set metadata without runtime
// normalization. Catalog validation uses this so a mis-declared
// ThreadSafe=true on a mutator is still reported even though CapabilitiesOf
// clears it for callers.
func (tool baseTool) declaredCapabilities() ToolCapabilities {
	return tool.capabilities
}

// declaredCapabilityProvider is implemented by tools that can report their
// raw constructor metadata (baseTool embedders).
type declaredCapabilityProvider interface {
	declaredCapabilities() ToolCapabilities
}

// Registry.Capabilities looks up a registered tool's capabilities by name.
// Unknown tools return the fail-closed default (not an error) so callers can
// treat missing tools as non-concurrent without panicking.
func (registry *Registry) Capabilities(name string) ToolCapabilities {
	if registry == nil {
		return UnknownCapabilities()
	}
	tool, ok := registry.Get(name)
	if !ok {
		return UnknownCapabilities()
	}
	return CapabilitiesOf(tool)
}

// ValidateCapabilities checks a single capability record for consistency.
// It returns a non-empty slice of human-readable problems when invalid.
// Note: CapabilitiesOf already normalizes ThreadSafe away for non-ReadOnly
// effects; validation still rejects the raw combination so catalog tests
// catch mis-declared constructors.
func ValidateCapabilities(name string, caps ToolCapabilities) []string {
	var problems []string
	name = strings.TrimSpace(name)
	if name == "" {
		name = "<unnamed>"
	}
	if !caps.Effect.Valid() {
		problems = append(problems, name+": invalid effect value")
	}
	if caps.Effect == EffectUnknown && caps.ThreadSafe {
		problems = append(problems, name+": ThreadSafe=true is forbidden with EffectUnknown")
	}
	// Mutating / interactive / process-read effects are never concurrency-eligible.
	switch caps.Effect {
	case EffectWorkspaceWrite, EffectExternalWrite, EffectInteractive, EffectProcessRead:
		if caps.ThreadSafe {
			problems = append(problems, name+": ThreadSafe=true is not permitted for effect "+caps.Effect.String())
		}
	}
	return problems
}
