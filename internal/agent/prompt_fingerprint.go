package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/Gitlawb/zero/internal/zeroruntime"
)

// promptSubstrings are the cacheable components of the prompt a run sends to a
// model. systemPrompt is the exact joined system-message content. The remaining
// fields retain useful diagnostic boundaries within that prompt and tool list.
type promptSubstrings struct {
	systemPrompt       string
	baseInstructions   string
	confirmationPolicy string
	projectContext     string
	skills             string
	tools              string
	schema             string
}

// buildPromptSubstrings builds the prompt and retains both its exact joined
// form and narrower diagnostic components. Callers that already built the
// prompt should use buildPromptSubstringsFromParts to avoid repeating workspace
// reads.
func buildPromptSubstrings(options Options, exposed []zeroruntime.ToolDefinition) promptSubstrings {
	return buildPromptSubstringsFromParts(buildSystemPromptParts(options), exposed)
}

func buildPromptSubstringsFromParts(parts systemPromptParts, exposed []zeroruntime.ToolDefinition) promptSubstrings {
	toolsSubstr, schemaSubstr := toolSubstrings(exposed)

	return promptSubstrings{
		systemPrompt:       parts.prompt,
		baseInstructions:   parts.baseInstructions,
		confirmationPolicy: parts.confirmationPolicy,
		projectContext:     parts.projectContext,
		skills:             parts.skills,
		tools:              toolsSubstr,
		schema:             schemaSubstr,
	}
}

// toolSubstrings preserves the emitted tool order. Names and descriptions are
// fingerprinted separately from schemas so either kind of drift is identifiable.
// Order is part of request identity: silently sorting here would hide a provider-
// visible reordering and could report a false cache hit.
func toolSubstrings(exposed []zeroruntime.ToolDefinition) (toolsSubstr, schemaSubstr string) {
	if len(exposed) == 0 {
		return "", ""
	}
	var toolsSB, schemaSB strings.Builder
	for _, def := range exposed {
		writeLengthPrefixed(&toolsSB, def.Name)
		writeLengthPrefixed(&toolsSB, def.Description)
		writeLengthPrefixed(&toolsSB, string(def.Type))
		// Parameters is rendered to a canonical JSON string per tool. The
		// schema-render cache in internal/agent (used by partitionToolsCached)
		// guarantees the same render is byte-identical across turns for the
		// same tool, so this substring is a stable hash input.
		writeLengthPrefixed(&schemaSB, def.Name)
		writeLengthPrefixed(&schemaSB, canonicalSchemaString(def.Parameters))
		if def.Format != nil {
			writeLengthPrefixed(&schemaSB, def.Format.Type)
			writeLengthPrefixed(&schemaSB, def.Format.Syntax)
			writeLengthPrefixed(&schemaSB, def.Format.Definition)
		}
	}
	return toolsSB.String(), schemaSB.String()
}

func writeLengthPrefixed(sb *strings.Builder, value string) {
	fmt.Fprintf(sb, "%d:", len(value))
	sb.WriteString(value)
}

// canonicalSchemaString renders a tool's parameter schema to a stable string.
// tools.ToolDef.Parameters is a map[string]any in the common case; Go's
// fmt.Sprintf("%v", m) iterates a map in random order, which would produce
// a different hash for the same Parameters value across calls and defeat
// the fingerprint. encoding/json marshals maps with keys sorted
// alphabetically, so json.Marshal is the primary stable render. The
// fallback for the rare non-JSON-compatible value (functions, channels,
// NaN/Inf floats, cyclic references) is a stable key-sorted stringifier
// that walks the value with sorted keys for maps and a leading
// "__non_json:" prefix so a future schema change is visible in the trace
// (a SchemaHash collision from the same value) rather than a silent hash
// drift.
//
// Note: SchemaHash is a stability signal, not a wire-identity signal. The
// bytes json.Marshal produces for a Go map[string]any may differ from
// the bytes the provider actually sends on the wire (provider encoders
// use their own JSON conventions; an Anthropic schema may serialize with
// different whitespace, key order, or number formatting). A consumer
// must NOT assume SchemaHash matches the provider's wire schema — only
// that two turns with the same Go-side Parameters produce the same
// SchemaHash. This is sufficient for the trace's purpose (drift
// detection) but not for a content-based equality check.
func canonicalSchemaString(params map[string]any) string {
	if len(params) == 0 {
		return ""
	}
	data, err := json.Marshal(params)
	if err != nil {
		return "__non_json:" + stableStringify(params)
	}
	return string(data)
}

// stableStringify renders v to a deterministic string. Maps have their keys
// sorted alphabetically; slices are walked in order; primitives use their
// natural Go format. The function is recursive but bounded by the size of
// the input, so a cyclic reference is not reachable here (the json.Marshal
// fallback is hit when json.Marshal itself returns an error, which for the
// common case is a non-JSON-compatible value, not a cycle — cycles are
// rare in tool schemas and acceptable to mis-render in the fallback path
// since the trace's contract is "the hash is stable for the same input,"
// not "the fallback is lossless"). Used only when json.Marshal fails.
func stableStringify(v any) string {
	var sb strings.Builder
	writeStable(&sb, v)
	return sb.String()
}

func writeStable(sb *strings.Builder, v any) {
	switch x := v.(type) {
	case nil:
		sb.WriteString("null")
	case bool:
		if x {
			sb.WriteString("true")
		} else {
			sb.WriteString("false")
		}
	case string:
		sb.WriteByte('"')
		sb.WriteString(x)
		sb.WriteByte('"')
	case float64:
		// Use %g for compact, deterministic float rendering. NaN and Inf are
		// not JSON-encodable (which is why we are in the fallback path) and
		// render as "NaN" / "+Inf" / "-Inf" — distinct, stable strings.
		fmt.Fprintf(sb, "%g", x)
	case float32:
		fmt.Fprintf(sb, "%g", float64(x))
	case int:
		fmt.Fprintf(sb, "%d", x)
	case int64:
		fmt.Fprintf(sb, "%d", x)
	case int32:
		fmt.Fprintf(sb, "%d", x)
	case uint:
		fmt.Fprintf(sb, "%d", x)
	case uint64:
		fmt.Fprintf(sb, "%d", x)
	case uint32:
		fmt.Fprintf(sb, "%d", x)
	case map[string]any:
		sb.WriteByte('{')
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for i, k := range keys {
			if i > 0 {
				sb.WriteByte(',')
			}
			sb.WriteByte('"')
			sb.WriteString(k)
			sb.WriteString(`":`)
			writeStable(sb, x[k])
		}
		sb.WriteByte('}')
	case []any:
		sb.WriteByte('[')
		for i, item := range x {
			if i > 0 {
				sb.WriteByte(',')
			}
			writeStable(sb, item)
		}
		sb.WriteByte(']')
	default:
		// Last-resort: include the type so distinct values produce distinct
		// strings even if their default format collides. Without the type
		// tag, fmt.Sprintf("%v", x) for two different types could produce
		// the same bytes (rare, but the type prefix makes it impossible).
		fmt.Fprintf(sb, "<%T:%v>", x, x)
	}
}

// ComputePrefixFingerprint builds and fingerprints a prompt for callers that do
// not already have one. The agent loop uses computePrefixFingerprint with the
// prompt parts it built once at run start, ensuring tracing observes the exact
// request content without repeating workspace I/O.
func ComputePrefixFingerprint(options Options, exposed []zeroruntime.ToolDefinition) prefixFingerprint {
	return computePrefixFingerprint(buildPromptSubstrings(options, exposed))
}

func computePrefixFingerprint(subs promptSubstrings) prefixFingerprint {
	systemPrompt := sha256hex(subs.systemPrompt)
	base := sha256hex(subs.baseInstructions)
	policy := sha256hex(subs.confirmationPolicy)
	project := sha256hex(subs.projectContext)
	skills := sha256hex(subs.skills)
	toolsH := sha256hex(subs.tools)
	schema := sha256hex(subs.schema)
	complete := sha256hex(strings.Join([]string{
		systemPrompt, toolsH, schema,
	}, "|"))
	return prefixFingerprint{
		SystemPromptHash:       systemPrompt,
		BaseInstructionsHash:   base,
		ConfirmationPolicyHash: policy,
		ProjectContextHash:     project,
		SkillsHash:             skills,
		ToolsHash:              toolsH,
		SchemaHash:             schema,
		CompletePrefixHash:     complete,
	}
}

// prefixFingerprint is the agent-side shape of a prompt-prefix fingerprint. It
// is converted to a trace.PrefixHash at the loop boundary (see EmitPrefixHash
// in loop.go) so the trace package does not need to import this one. The field
// names match the trace.PrefixHash JSON tags 1:1.
type prefixFingerprint struct {
	SystemPromptHash       string
	BaseInstructionsHash   string
	ConfirmationPolicyHash string
	ProjectContextHash     string
	SkillsHash             string
	ToolsHash              string
	SchemaHash             string
	CompletePrefixHash     string
}

// sha256hex returns the hex-encoded SHA-256 of s. Empty input produces the
// hash of the empty string, which is a constant; callers that want to
// distinguish "absent" from "empty" should check s before calling.
func sha256hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
