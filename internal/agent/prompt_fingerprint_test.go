package agent

import (
	"strings"
	"testing"

	"github.com/Gitlawb/zero/internal/zeroruntime"
)

// TestComputePrefixFingerprintStableAcrossCalls asserts the headline property
// the trace is meant to expose: two ComputePrefixFingerprint calls over the
// same Options and the same exposed tool list produce byte-identical hashes.
// This is the regression catch for "did anyone introduce non-determinism into
// the prompt prefix."
func TestComputePrefixFingerprintStableAcrossCalls(t *testing.T) {
	opts := Options{Cwd: t.TempDir(), SystemPrompt: "core"}
	exposed := []zeroruntime.ToolDefinition{
		{Name: "read_file", Parameters: map[string]any{"type": "object"}},
		{Name: "grep", Parameters: map[string]any{"type": "object"}},
	}
	first := ComputePrefixFingerprint(opts, exposed)
	second := ComputePrefixFingerprint(opts, exposed)
	if first != second {
		t.Fatalf("fingerprint must be stable across calls with the same input:\n  first=%+v\n second=%+v", first, second)
	}
	if first.CompletePrefixHash == "" {
		t.Fatalf("CompletePrefixHash must be non-empty for a non-trivial prompt")
	}
}

// Tool order and descriptions are provider-visible and therefore part of the
// cacheable prefix identity.
func TestComputePrefixFingerprintToolsHashMatchesEmittedOrderAndDescriptions(t *testing.T) {
	opts := Options{Cwd: t.TempDir(), SystemPrompt: "core"}
	a := []zeroruntime.ToolDefinition{
		{Name: "read_file", Description: "read", Parameters: map[string]any{"schema": "schema-read"}},
		{Name: "grep", Description: "search", Parameters: map[string]any{"schema": "schema-grep"}},
	}
	b := []zeroruntime.ToolDefinition{
		{Name: "grep", Description: "search", Parameters: map[string]any{"schema": "schema-grep"}},
		{Name: "read_file", Description: "read", Parameters: map[string]any{"schema": "schema-read"}},
	}
	fpA := ComputePrefixFingerprint(opts, a)
	fpB := ComputePrefixFingerprint(opts, b)
	if fpA.ToolsHash == fpB.ToolsHash {
		t.Fatalf("ToolsHash must change when emitted order changes: a=%s b=%s", fpA.ToolsHash, fpB.ToolsHash)
	}
	c := append([]zeroruntime.ToolDefinition(nil), a...)
	c[0].Description = "read a file"
	fpC := ComputePrefixFingerprint(opts, c)
	if fpA.ToolsHash == fpC.ToolsHash {
		t.Fatalf("ToolsHash must change when a description changes: a=%s c=%s", fpA.ToolsHash, fpC.ToolsHash)
	}
}

// TestComputePrefixFingerprintSchemaChangeChangesSchemaHash asserts that a
// change to a tool's Parameters schema is visible in the SchemaHash. A schema
// edit that should be cache-stable (e.g. field reorder, doc tweak) will move
// the hash and surface in the trace; a schema edit that legitimately
// invalidates the cache also moves the hash, which is the right behavior.
func TestComputePrefixFingerprintSchemaChangeChangesSchemaHash(t *testing.T) {
	opts := Options{Cwd: t.TempDir(), SystemPrompt: "core"}
	a := []zeroruntime.ToolDefinition{
		{Name: "read_file", Parameters: map[string]any{"version": "v1"}},
	}
	b := []zeroruntime.ToolDefinition{
		{Name: "read_file", Parameters: map[string]any{"version": "v2"}},
	}
	fpA := ComputePrefixFingerprint(opts, a)
	fpB := ComputePrefixFingerprint(opts, b)
	if fpA.SchemaHash == fpB.SchemaHash {
		t.Fatalf("SchemaHash must change when tool Parameters change: a=%s b=%s", fpA.SchemaHash, fpB.SchemaHash)
	}
}

func TestComputePrefixFingerprintIncludesToolTypeAndFormat(t *testing.T) {
	opts := Options{Cwd: t.TempDir(), SystemPrompt: "core"}
	base := zeroruntime.ToolDefinition{Name: "apply_patch", Description: "Patch files"}
	baseFingerprint := ComputePrefixFingerprint(opts, []zeroruntime.ToolDefinition{base})

	typed := base
	typed.Type = zeroruntime.ToolDefinitionFreeform
	if got := ComputePrefixFingerprint(opts, []zeroruntime.ToolDefinition{typed}); got.ToolsHash == baseFingerprint.ToolsHash {
		t.Fatal("ToolsHash must change when tool Type changes")
	}

	formatCases := []struct {
		name   string
		format zeroruntime.ToolDefinitionFormat
	}{
		{name: "type", format: zeroruntime.ToolDefinitionFormat{Type: "grammar"}},
		{name: "syntax", format: zeroruntime.ToolDefinitionFormat{Syntax: "lark"}},
		{name: "definition", format: zeroruntime.ToolDefinitionFormat{Definition: "start: PATCH"}},
		{name: "empty", format: zeroruntime.ToolDefinitionFormat{}},
	}
	for _, test := range formatCases {
		t.Run(test.name, func(t *testing.T) {
			withFormat := base
			withFormat.Format = &test.format
			got := ComputePrefixFingerprint(opts, []zeroruntime.ToolDefinition{withFormat})
			if got.SchemaHash == baseFingerprint.SchemaHash {
				t.Fatalf("SchemaHash must change for %s format input", test.name)
			}
		})
	}
}

// TestComputePrefixFingerprintSystemPromptChangeChangesBaseHash asserts the
// most common drift path: a system-prompt edit (e.g. an agent.md update, a
// model addendum change) must move the base-instructions hash, which moves
// the complete-prefix hash, which is what the trace is for.
func TestComputePrefixFingerprintSystemPromptChangeChangesBaseHash(t *testing.T) {
	optsA := Options{Cwd: t.TempDir(), SystemPrompt: "core v1"}
	optsB := Options{Cwd: t.TempDir(), SystemPrompt: "core v2"}
	exposed := []zeroruntime.ToolDefinition{{Name: "noop", Parameters: map[string]any{}}}
	fpA := ComputePrefixFingerprint(optsA, exposed)
	fpB := ComputePrefixFingerprint(optsB, exposed)
	if fpA.BaseInstructionsHash == fpB.BaseInstructionsHash {
		t.Fatalf("BaseInstructionsHash must change when SystemPrompt changes: a=%s b=%s", fpA.BaseInstructionsHash, fpB.BaseInstructionsHash)
	}
	if fpA.CompletePrefixHash == fpB.CompletePrefixHash {
		t.Fatalf("CompletePrefixHash must change when any sub-hash changes: a=%s b=%s", fpA.CompletePrefixHash, fpB.CompletePrefixHash)
	}
}

// CompletePrefixHash represents the exact joined system prompt and the ordered
// tool definitions. Narrower system-section hashes remain diagnostic only.
func TestComputePrefixFingerprintCompleteIsAggregateOfSubHashes(t *testing.T) {
	opts := Options{Cwd: t.TempDir(), SystemPrompt: "core"}
	exposed := []zeroruntime.ToolDefinition{{Name: "read_file", Parameters: map[string]any{"k": "v"}}}
	fp := ComputePrefixFingerprint(opts, exposed)
	// Manually compute what the canonical join should be.
	expected := sha256hex(strings.Join([]string{
		fp.SystemPromptHash,
		fp.ToolsHash,
		fp.SchemaHash,
	}, "|"))
	if fp.CompletePrefixHash != expected {
		t.Fatalf("CompletePrefixHash must equal sha256hex of the exact system, tools, and schema hashes:\n  got:      %s\n  expected: %s", fp.CompletePrefixHash, expected)
	}
}

func TestComputePrefixFingerprintCoversEverySystemPromptSection(t *testing.T) {
	base := Options{Cwd: t.TempDir(), SystemPrompt: "core", Model: "gpt-5"}
	changed := base
	changed.ResponseStyle = "concise"

	fpBase := ComputePrefixFingerprint(base, nil)
	fpChanged := ComputePrefixFingerprint(changed, nil)
	if fpBase.SystemPromptHash == fpChanged.SystemPromptHash {
		t.Fatal("SystemPromptHash must change when any rendered system section changes")
	}
	if fpBase.CompletePrefixHash == fpChanged.CompletePrefixHash {
		t.Fatal("CompletePrefixHash must include the exact full system prompt")
	}
}

func TestComputePrefixFingerprintCoversTransientSystemPrompt(t *testing.T) {
	base := Options{Cwd: t.TempDir(), SystemPrompt: "core"}
	changed := base
	changed.TransientSystemPrompt = "turn-only guidance"

	fpBase := ComputePrefixFingerprint(base, nil)
	fpChanged := ComputePrefixFingerprint(changed, nil)
	if fpBase.SystemPromptHash == fpChanged.SystemPromptHash {
		t.Fatal("SystemPromptHash must change for transient system guidance")
	}
	if fpBase.CompletePrefixHash == fpChanged.CompletePrefixHash {
		t.Fatal("CompletePrefixHash must cover transient system guidance")
	}
}

// TestBuildPromptSubstringsDefaultOptions asserts the invariants of the
// substrings helper for default Options: which substrings are non-empty,
// which are empty, and that the substring-to-hash round-trip is lossless.
func TestBuildPromptSubstringsDefaultOptions(t *testing.T) {
	opts := Options{
		Cwd:          t.TempDir(),
		SystemPrompt: "test core",
	}
	subs := buildPromptSubstrings(opts, nil)
	// Invariants the trace depends on (default Options):
	//   1. baseInstructions substring equals the core system prompt bytes.
	//   2. confirmationPolicy substring is non-empty (the embedded policy
	//      is always present, post TrimSpace).
	//   3. skills, tools, and schema substrings are empty (no skills or
	//      tools configured for default Options).
	if subs.baseInstructions != "test core" {
		t.Fatalf("baseInstructions substring must equal the core system prompt: got %q want %q", subs.baseInstructions, "test core")
	}
	if subs.systemPrompt != buildSystemPrompt(opts) {
		t.Fatal("systemPrompt substring must equal the exact joined system prompt")
	}
	if subs.confirmationPolicy == "" {
		t.Fatalf("confirmationPolicy substring must be non-empty (the embedded policy is always present)")
	}
	if subs.skills != "" {
		t.Fatalf("skills substring must be empty for default Options: got %q", subs.skills)
	}
	// Round-trip: the corresponding fingerprint hashes must equal
	// sha256hex of the substrings, with no truncation or reformatting
	// between the two layers.
	fp := ComputePrefixFingerprint(opts, nil)
	if fp.SystemPromptHash != sha256hex(subs.systemPrompt) {
		t.Fatalf("SystemPromptHash in fingerprint must equal sha256hex of the joined prompt")
	}
	if fp.BaseInstructionsHash != sha256hex(subs.baseInstructions) {
		t.Fatalf("BaseInstructionsHash in fingerprint must equal sha256hex of the substring")
	}
	if fp.ConfirmationPolicyHash != sha256hex(subs.confirmationPolicy) {
		t.Fatalf("ConfirmationPolicyHash in fingerprint must equal sha256hex of the substring")
	}
}

// TestCanonicalSchemaStringStableForMapParameters asserts the headline
// property the trace needs: the same map[string]any produces the same
// canonical string across calls, even though Go's fmt.Sprintf("%v", m)
// iterates a map in random order. json.Marshal sorts map keys
// alphabetically, which is the fix.
func TestCanonicalSchemaStringStableForMapParameters(t *testing.T) {
	params := map[string]any{
		"description": "read a file",
		"properties": map[string]any{
			"path": map[string]any{"type": "string"},
		},
		"required": []any{"path"},
		"type":     "object",
	}
	first := canonicalSchemaString(params)
	second := canonicalSchemaString(params)
	if first != second {
		t.Fatalf("canonicalSchemaString must be stable across calls for the same Parameters:\n  first=%s\n second=%s", first, second)
	}
	if first == "" {
		t.Fatal("canonicalSchemaString must produce a non-empty string for a non-empty map")
	}
	// Two maps with the same key/value pairs (different declaration
	// order in source) must produce identical canonical strings. This
	// is the property that defeats Go's map iteration randomization.
	reordered := map[string]any{
		"type":     "object",
		"required": []any{"path"},
		"properties": map[string]any{
			"path": map[string]any{"type": "string"},
		},
		"description": "read a file",
	}
	if canonicalSchemaString(params) != canonicalSchemaString(reordered) {
		t.Fatal("canonicalSchemaString must produce identical output for maps with the same key/value pairs in different declaration orders")
	}
}

// TestCanonicalSchemaStringFallbackStableForNonJSONValue exercises the
// non-JSON fallback in canonicalSchemaString: when Parameters contains a
// value json.Marshal cannot encode (here, a channel), the function must
// fall back to a key-sorted stringification that is stable across calls
// and across map-iteration order. The previous fallback used
// fmt.Sprintf("%v", params), which iterates the map in random order and
// produced a different hash for the same value across calls — defeating
// the fingerprint. The current fallback is stable by construction.
func TestCanonicalSchemaStringFallbackStableForNonJSONValue(t *testing.T) {
	ch := make(chan int) // channels are not JSON-encodable
	params := map[string]any{
		"description": "a tool with a non-JSON value",
		"channel":     ch,
		"z_first":     1,
		"a_second":    2,
	}
	first := canonicalSchemaString(params)
	second := canonicalSchemaString(params)
	if first == "" {
		t.Fatal("canonicalSchemaString must produce a non-empty string for the fallback path")
	}
	if first != second {
		t.Fatalf("canonicalSchemaString fallback must be stable across calls:\n  first=%s\n second=%s", first, second)
	}
	// A re-ordered map (same key/value pairs, different declaration order)
	// must produce the same fallback string. This is the property the
	// key-sorted stringifier provides that fmt.Sprintf("%v", m) does not.
	reordered := map[string]any{
		"a_second":    2,
		"z_first":     1,
		"channel":     ch,
		"description": "a tool with a non-JSON value",
	}
	if canonicalSchemaString(params) != canonicalSchemaString(reordered) {
		t.Fatalf("canonicalSchemaString fallback must produce identical output for maps with the same key/value pairs in different declaration orders:\n  first=%s\n second=%s", first, canonicalSchemaString(reordered))
	}
	// The fallback must include the "__non_json:" prefix so a consumer
	// can tell the bytes are not a JSON-marshaled schema (a hash collision
	// between a json.Marshal result and a stableStringify result is
	// possible in theory; the prefix makes the source observable).
	if !strings.HasPrefix(first, "__non_json:") {
		t.Fatalf("canonicalSchemaString fallback must start with the __non_json: prefix, got: %s", first)
	}
}
