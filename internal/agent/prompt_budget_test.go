package agent

import (
	"strings"
	"testing"

	"github.com/Gitlawb/zero/internal/tools"
	"github.com/Gitlawb/zero/internal/zeroruntime"
)

// These ceilings are a deliberate ratchet on Zero's fixed per-turn overhead — the
// system prompt and the eager tool schemas that ride on EVERY request. They are
// set ~10% above the current measured cost, using ApproxTextTokens (the same
// estimate the compaction loop and /context use, ~non-whitespace-bytes/4). A change
// that pushes either past its ceiling must be justified (and the ceiling raised
// deliberately) or trimmed — the per-turn floor should not creep up silently.
//
// Measured baselines (2026-07): base system prompt ~3160 tokens; the tools a normal
// (auto permission-mode) interactive turn sends ~3230 tokens. The tool figure is
// the auto-mode advertised set, including exec_command: auto mode must expose both
// process creation and write_stdin process interaction or the latter is unusable
// without a session id. Higher-risk tools that do not opt into auto remain excluded.
const (
	maxBaseSystemPromptTokens = 3500
	// Raised deliberately from 3550 on 2026-08-08. view_image (#843) took the set
	// to 3588, and #867 trimmed it back to 3578: still 28 tokens over.
	//
	// The overrun is a rounding error rather than a regression. view_image costs
	// 82 tokens, the second cheapest tool in the set, against exec_command's 909
	// and request_permissions' 397; it is simply the one that crossed the line.
	// Raised rather than paid down because the alternative was trimming a
	// description to claw back 28 tokens, degrading a tool's usability to satisfy
	// a line drawn against a 2026-07 measurement that predates two tools.
	//
	// The ratchet did its job: it caught the creep and forced a decision instead
	// of a drift. That is the point of it, so keep raising it deliberately rather
	// than reflexively.
	//
	// THIS COUNTS THE SCHEMAS ONLY, NOT THE HOST'S SHELL GUIDANCE. Until
	// 2026-09-07 it counted both, and that made it a different measurement on
	// every machine: exec_command appends shell guidance whose size depends on
	// which shell was detected, so the same tree measured 3498 on Linux, ~3600 on
	// a Windows host with pwsh 7, and 3653 on Windows PowerShell 5.1, which is
	// stock Windows. The 5.1 case has been over this ceiling since view_image
	// landed, so `go test ./...` failed for anyone on a stock Windows box while
	// CI stayed green, because the runners have pwsh 7 and take the cheaper
	// branch. A budget that passes or fails on which PowerShell happens to be
	// installed is not ratcheting anything.
	maxEagerToolSchemaTokens = 3650
	// The host-specific half, ratcheted on its own so it cannot creep either.
	// Measured 2026-09-07: Windows PowerShell 5.1 is the most expensive host at
	// 155 tokens (722 chars), pwsh 7 is 110, cmd.exe is 87, and every POSIX host
	// is 0 because no guidance is appended there. Set above the worst case, not
	// above whatever this machine happens to be.
	maxExecCommandShellGuidanceTokens = 200
)

func TestSystemPromptTokenBudget(t *testing.T) {
	// Minimal render: a model is set (so the session block renders) but no Cwd, so
	// the workspace map, project guidelines, and repo map are excluded — this is the
	// fixed base every session pays regardless of the workspace it runs in.
	prompt := buildSystemPrompt(Options{Model: "claude-opus-4-8"})
	got := ApproxTextTokens(prompt)
	t.Logf("base system prompt: %d tokens (%d bytes)", got, len(prompt))
	if got > maxBaseSystemPromptTokens {
		t.Fatalf("base system prompt is %d tokens, over the %d ceiling — trim it or raise the ceiling deliberately", got, maxBaseSystemPromptTokens)
	}
}

func TestEagerToolSchemaTokenBudget(t *testing.T) {
	registry := tools.NewRegistry()
	for _, tool := range tools.CoreToolsScoped(t.TempDir(), nil) {
		registry.Register(tool)
	}
	// Options{} keeps DeferThreshold at 0, so deferral is inactive and every core
	// tool is exposed eagerly — exactly what a plugin-free session sends each turn.
	exposed, _ := partitionTools(registry, PermissionModeAuto, Options{}, map[string]bool{})
	total := estimateToolDefTokens(exposed)
	// exec_command's description carries this host's shell guidance, and that
	// guidance is the one part of the eager set that is not the same everywhere.
	// Charged separately below rather than folded in here, or this ceiling would
	// mean a different thing on every machine. See the constants above.
	//
	// Removed from the description BEFORE estimating rather than subtracted from
	// the total afterwards. ApproxTextTokens is an integer division, so
	// floor((schema+guidance)/4) - floor(guidance/4) comes out one token high
	// whenever the two remainders add past four, and which side of that line a
	// host lands on depends on the length of its guidance: exactly the
	// host-dependence this number is supposed to be rid of.
	guidance := tools.HostExecCommandShellGuidance()
	schemas := make([]zeroruntime.ToolDefinition, len(exposed))
	copy(schemas, exposed)
	stripped := 0
	for index := range schemas {
		without := strings.TrimSuffix(schemas[index].Description, guidance)
		if without != schemas[index].Description {
			stripped++
		}
		schemas[index].Description = without
	}
	if guidance != "" && stripped != 1 {
		t.Fatalf("SETUP INVALID: this host's shell guidance came off %d descriptions, want exactly exec_command's; internal/tools pins it as that description's tail", stripped)
	}
	got := estimateToolDefTokens(schemas)
	guidanceTokens := ApproxTextTokens(guidance)
	t.Logf("eager core tool schemas: %d tokens across %d tools (%d total, %d host shell guidance)", got, len(exposed), total, guidanceTokens)
	if got > maxEagerToolSchemaTokens {
		// NOT "defer a tool": this test pins DeferThreshold at 0, so marking a
		// tool deferred leaves it exposed here and changes nothing. Deferral is
		// still worth doing for real sessions, it just cannot move this number.
		// The levers that do are a smaller schema, one fewer core tool, or a
		// deliberate raise.
		t.Fatalf("eager tool schemas are %d tokens, over the %d ceiling — trim a schema, drop a core tool, or raise the ceiling deliberately (deferring will NOT help: this test disables deferral)", got, maxEagerToolSchemaTokens)
	}
	if guidanceTokens > maxExecCommandShellGuidanceTokens {
		t.Fatalf("this host's exec_command shell guidance is %d tokens, over the %d ceiling — trim the guidance in internal/tools/shell_runtime.go or raise the ceiling deliberately", guidanceTokens, maxExecCommandShellGuidanceTokens)
	}
}

func TestAgentAdvertisesBothEditTools(t *testing.T) {
	registry := tools.NewRegistry()
	for _, tool := range tools.CoreToolsScoped(t.TempDir(), nil) {
		registry.Register(tool)
	}
	exposed, _ := partitionTools(registry, PermissionModeAsk, Options{}, map[string]bool{})
	names := make(map[string]bool, len(exposed))
	for _, definition := range exposed {
		names[definition.Name] = true
	}
	if !names["apply_patch"] {
		t.Fatal("agent must retain apply_patch for multi-hunk changes")
	}
	if !names["edit_file"] {
		t.Fatal("agent must receive edit_file for targeted exact replacements")
	}
}
