package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/tools"
	"github.com/Twigpine/zero/internal/zeroruntime"
)

// THE ADDITIVITY PROOF.
//
// ZeroMaxing Phase 2 adds an `orchestrate` tool. The constraint proved here is
// that REGISTERING THAT TOOL changes nothing while the posture is OFF: same
// advertised tool set, same tool-definition bytes, same assembled system
// prompt, therefore the same token count and the same provider request.
//
// Stated narrowly on purpose. "With the posture off, Zero is byte-identical to
// a build without the feature" is the tempting phrasing and it is not true:
// system_prompt.go drops the ~5 KB confirmation policy for any run that cannot
// mutate, and runCanMutate is evaluated regardless of posture. An all-read-only
// run — `zero exec --enabled-tools read_file,grep`, or a read-only specialist
// child — therefore gets a smaller prompt than a build without the feature,
// posture off. That drop is deliberate and fails closed; it is simply not part
// of what this test proves, and the comparison below could not catch it anyway
// since both registries take the same branch.
//
// The proof is a CONTROLLED COMPARISON rather than a frozen hash of the whole
// prefix. Two registries are built that differ in exactly one thing — whether
// the orchestrate tool is registered — and the posture-off output of both must
// be byte-identical. That states the real property ("registering this tool
// changes nothing while the posture is off"), it exercises the Deferred()
// mechanism that enforces it, and it cannot rot: there is no constant to
// regenerate and nothing to keep in step by hand.
//
// A frozen whole-prefix hash was tried first and is the wrong instrument here:
// the assembled prompt embeds the working directory and the operating system,
// so the constant would be machine- and platform-specific and every CI runner
// would "fail" honestly-unchanged code. The definition-bytes hash below keeps
// the frozen-golden idea where it IS portable.

// fixtureCwd is a fixed synthetic path. The prompt embeds the working
// directory, so a real t.TempDir() would make any hash unstable across runs.
const fixtureCwd = "/zero/fixture/workspace"

// baseFixtureRegistry is the control: a fixed, representative tool set with NO
// orchestrate tool. Deliberately not the full core set — that would make the
// golden hash churn on every unrelated tool change and train the reader to
// regenerate it.
func baseFixtureRegistry() *tools.Registry {
	registry := tools.NewRegistry()
	registry.Register(tools.NewScopedReadFileTool(fixtureCwd, nil))
	registry.Register(tools.NewScopedGrepTool(fixtureCwd, nil))
	registry.Register(tools.NewScopedGlobTool(fixtureCwd, nil))
	registry.Register(tools.NewScopedListDirectoryTool(fixtureCwd, nil))
	return registry
}

// postureOffPrefix returns the three things a user pays for, for a posture-OFF
// run against the given registry: the advertised tool names in emitted order,
// the full tool-definition bytes, and the assembled system prompt.
func postureOffPrefix(t *testing.T, registry *tools.Registry, mode PermissionMode) (names []string, definitions string, prompt string) {
	t.Helper()
	options := Options{
		Cwd:        fixtureCwd,
		Registry:   registry,
		SessionID:  "session-posture-off-identity",
		Zeromaxing: ZeromaxingOff, // THE POINT: the posture is off.
		// Deferral ACTIVE. Without this the partition never consults Deferred()
		// at all, and this test would pass on the permission gate alone — which
		// it originally did, so a Deferred() regression went undetected by it.
		DeferThreshold: 1,
	}
	exposed, _ := partitionTools(registry, mode, options, nil)
	for _, definition := range exposed {
		names = append(names, definition.Name)
	}
	encoded, err := json.Marshal(exposed)
	if err != nil {
		t.Fatalf("marshal tool definitions: %v", err)
	}
	return names, string(encoded), buildSystemPromptParts(options).prompt
}

// (a) THE PROOF. Registering the orchestrate tool must change NOTHING that a
// posture-off run sends: not the advertised set, not the definition bytes, not
// the prompt. If this fails, the feature has stopped being additive and the
// diff is what is wrong.
func TestPostureOffPrefixUnchangedByRegisteringTheTool(t *testing.T) {
	// BOTH permission modes. Under auto the permission gate would hide the tool
	// even if Deferred() were broken, so auto alone proves nothing about the
	// mechanism; unsafe is where Deferred() is the ONLY thing standing between
	// the tool and the advertised set.
	for _, mode := range []PermissionMode{PermissionModeAuto, PermissionModeUnsafe} {
		t.Run(string(mode), func(t *testing.T) { assertPostureOffPrefixUnchanged(t, mode) })
	}
}

func assertPostureOffPrefixUnchanged(t *testing.T, mode PermissionMode) {
	t.Helper()
	withoutNames, withoutDefs, withoutPrompt := postureOffPrefix(t, baseFixtureRegistry(), mode)

	withTool := baseFixtureRegistry()
	registerPhase2ToolForTest(withTool)
	withNames, withDefs, withPrompt := postureOffPrefix(t, withTool, mode)

	if !reflect.DeepEqual(withoutNames, withNames) {
		t.Fatalf("ADVERTISED TOOL SET CHANGED with the posture off:\n without: %v\n with:    %v",
			withoutNames, withNames)
	}
	if withoutDefs != withDefs {
		t.Fatalf("TOOL DEFINITION BYTES CHANGED with the posture off (%d -> %d bytes)",
			len(withoutDefs), len(withDefs))
	}
	if withoutPrompt != withPrompt {
		t.Fatalf("ASSEMBLED PROMPT CHANGED with the posture off (%d -> %d bytes)",
			len(withoutPrompt), len(withPrompt))
	}
	// Guard against the test passing because the tool was never registered at
	// all: it must be present in the registry, just not advertised.
	if _, ok := withTool.Get(phase2ToolName); !ok {
		t.Fatalf("fixture did not register %q, so this test proves nothing", phase2ToolName)
	}
}

// The same claim in terms a reader can check without diffing: with the posture
// off the tool is registered but not advertised, and its name appears nowhere
// in what the provider receives.
func TestPostureOffDoesNotAdvertiseThePhase2Tool(t *testing.T) {
	registry := baseFixtureRegistry()
	registerPhase2ToolForTest(registry)
	names, definitions, prompt := postureOffPrefix(t, registry, PermissionModeUnsafe)

	for _, name := range names {
		if name == phase2ToolName {
			t.Fatalf("posture off must not advertise %q, got %v", phase2ToolName, names)
		}
	}
	if strings.Contains(definitions, phase2ToolName) {
		t.Fatalf("posture off must not send %q in the tool definitions", phase2ToolName)
	}
	if strings.Contains(prompt, phase2ToolName) {
		t.Fatalf("posture off must not name %q in the system prompt", phase2ToolName)
	}
}

// ...and the run-level counterpart: a posture-off conversation carries no
// posture or orchestration text at all.
func TestPostureOffRunCarriesNoPostureText(t *testing.T) {
	root := t.TempDir()
	registry := tools.NewRegistry()
	registry.Register(tools.NewScopedReadFileTool(root, nil))
	registerPhase2ToolForTest(registry)
	provider := &mockProvider{turns: [][]zeroruntime.StreamEvent{
		{{Type: zeroruntime.StreamEventText, Content: "done"}, {Type: zeroruntime.StreamEventDone}},
	}}
	if _, err := Run(context.Background(), "hello", provider, Options{
		Cwd: root, Registry: registry, SessionID: "session-posture-off-run",
	}); err != nil {
		t.Fatal(err)
	}
	var rendered strings.Builder
	for _, message := range provider.requests[0].Messages {
		rendered.WriteString(message.Content)
	}
	if strings.Contains(rendered.String(), phase2ToolName) {
		t.Fatalf("a posture-off run must never name %q:\n%s", phase2ToolName, rendered.String())
	}
	for _, forbidden := range []string{
		ZeromaxingEnterNotice, ZeromaxingBudgetNotice, ZeromaxingStillOnNotice, ZeromaxingExitNotice,
	} {
		if strings.Contains(rendered.String(), forbidden) {
			t.Fatalf("a posture-off run must carry no posture text, found %q", forbidden)
		}
	}
}

// A frozen golden over the TOOL DEFINITION BYTES only. Definitions carry no
// environment (no cwd, no OS), so unlike the whole prefix this IS portable and
// a moved hash means a real schema change in the fixture's tools.
//
// MOVED AGAIN AFTER THE CURRENT MAIN REFRESH. Main's tool descriptions now
// distinguish exact reads from minified exploratory reads while keeping the
// same posture-off exposure contract. Before that, #867 reshaped read_file's
// schema (the canonical
// path/offset/limit contract) and reworded its description; before that, #838
// reworded glob, grep, list_directory and read_file. Both are schema changes on
// main's side rather than posture leaks from this branch, and the distinction was
// proved rather than assumed: the first request body of a posture-off run was
// compared byte for byte against a binary built from that same main, across
// --auto low/medium/high/member and --use-spec, and all five were identical.
// The absolute byte counts moved with main's new wording (33549 -> 31851 on
// --auto low); the DIFFERENCE between the two binaries stayed zero, which is
// the thing this guard exists to hold.
const postureOffDefinitionsFingerprint = "35a12ffe0079ffb18a3342df5e0c3499836d2f2811b94707ae6c337cf5087197"

func TestPostureOffToolDefinitionsMatchGolden(t *testing.T) {
	registry := baseFixtureRegistry()
	registerPhase2ToolForTest(registry)
	_, definitions, _ := postureOffPrefix(t, registry, PermissionModeUnsafe)
	sum := sha256.Sum256([]byte(definitions))
	got := hex.EncodeToString(sum[:])
	if postureOffDefinitionsFingerprint == "PLACEHOLDER" {
		t.Fatalf("golden not captured yet; observed %s", got)
	}
	if got != postureOffDefinitionsFingerprint {
		t.Fatalf("posture-off tool definitions changed:\n got  %s\n want %s\n%s",
			got, postureOffDefinitionsFingerprint, definitions)
	}
}
