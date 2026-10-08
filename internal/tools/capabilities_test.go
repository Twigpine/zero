package tools

import (
	"context"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// plainTool implements Tool without CapabilityProvider — fail-closed default.
type plainTool struct {
	name string
}

func (t plainTool) Name() string        { return t.name }
func (t plainTool) Description() string { return "plain" }
func (t plainTool) Parameters() Schema  { return Schema{Type: "object"} }
func (t plainTool) Safety() Safety      { return readOnlySafety("plain") }
func (t plainTool) Run(context.Context, map[string]any) Result {
	return okResult("ok")
}

func TestCapabilitiesOfDefaultsUnknown(t *testing.T) {
	caps := CapabilitiesOf(plainTool{name: "mystery"})
	if caps.Effect != EffectUnknown {
		t.Fatalf("Effect = %v, want Unknown", caps.Effect)
	}
	if caps.ThreadSafe {
		t.Fatal("ThreadSafe must be false for unclassified tools")
	}
}

func TestCapabilitiesOfNilTool(t *testing.T) {
	caps := CapabilitiesOf(nil)
	if caps.Effect != EffectUnknown || caps.ThreadSafe {
		t.Fatalf("nil tool = %+v, want fail-closed unknown", caps)
	}
}

func TestUnknownNotThreadSafeEvenIfProviderLies(t *testing.T) {
	tool := baseTool{
		name: "liar",
		capabilities: ToolCapabilities{
			Effect:     EffectUnknown,
			ThreadSafe: true, // invalid — Capabilities() must clear it
		},
	}
	caps := tool.Capabilities()
	if caps.ThreadSafe {
		t.Fatal("Capabilities() must force ThreadSafe=false for EffectUnknown")
	}
}

func TestNormalizeClearsThreadSafeOnMutators(t *testing.T) {
	tool := baseTool{
		name: "writer",
		capabilities: ToolCapabilities{
			Effect:     EffectWorkspaceWrite,
			ThreadSafe: true, // must be cleared on read
		},
	}
	if tool.Capabilities().ThreadSafe {
		t.Fatal("WorkspaceWrite must never surface ThreadSafe=true via Capabilities()")
	}
	// Wrap with Run so CapabilitiesOf's CapabilityProvider path is exercised.
	wrapped := deferredCapableTool{baseTool: tool}
	if CapabilitiesOf(wrapped).ThreadSafe {
		t.Fatal("CapabilitiesOf must normalize mutator ThreadSafe away")
	}
}

func TestValidateRejectsThreadSafeUnknown(t *testing.T) {
	problems := ValidateCapabilities("x", ToolCapabilities{Effect: EffectUnknown, ThreadSafe: true})
	if len(problems) == 0 {
		t.Fatal("expected validation problems for ThreadSafe+Unknown")
	}
}

func TestValidateRejectsThreadSafeMutators(t *testing.T) {
	for _, effect := range []EffectClass{EffectWorkspaceWrite, EffectExternalWrite, EffectInteractive, EffectProcessRead} {
		problems := ValidateCapabilities("m", ToolCapabilities{Effect: effect, ThreadSafe: true})
		if len(problems) == 0 {
			t.Fatalf("effect %s: expected ThreadSafe rejection", effect)
		}
	}
}

func TestEveryBuiltinHasExplicitMetadata(t *testing.T) {
	problems := ValidateBuiltinCatalog(t.TempDir())
	if len(problems) > 0 {
		t.Fatalf("builtin metadata coverage failed:\n  %s", strings.Join(problems, "\n  "))
	}
}

func TestBuiltinCatalogNoUnknown(t *testing.T) {
	counts := map[EffectClass]int{}
	for _, tool := range BuiltinCatalog(t.TempDir()) {
		caps := CapabilitiesOf(tool)
		if caps.Effect == EffectUnknown {
			t.Errorf("tool %q is EffectUnknown", tool.Name())
		}
		counts[caps.Effect]++
	}
	if counts[EffectUnknown] != 0 {
		t.Fatalf("Unknown built-ins = %d, want 0", counts[EffectUnknown])
	}
	t.Logf("classification counts: %+v", counts)
}

func TestRegistryCapabilitiesLookup(t *testing.T) {
	reg := NewRegistry()
	reg.Register(NewScopedReadFileTool(t.TempDir(), nil))
	caps := reg.Capabilities("read_file")
	if caps.Effect != EffectReadOnly {
		t.Fatalf("read_file effect = %v, want ReadOnly", caps.Effect)
	}
	// Missing tool → fail-closed.
	missing := reg.Capabilities("does_not_exist")
	if missing.Effect != EffectUnknown || missing.ThreadSafe {
		t.Fatalf("missing tool = %+v", missing)
	}
}

func TestFileResourceKeysNormalized(t *testing.T) {
	keys := fileResourceKeys(map[string]any{"path": `./src/../src/main.go`})
	if len(keys) != 1 {
		t.Fatalf("keys = %v", keys)
	}
	want := ResourceKeyFile + NormalizeResourcePath("./src/../src/main.go")
	if keys[0] != want {
		t.Fatalf("key = %q, want %q", keys[0], want)
	}
	// Missing args → nil, no panic.
	if fileResourceKeys(nil) != nil {
		t.Fatal("nil args should yield nil keys")
	}
	if fileResourceKeys(map[string]any{}) != nil {
		t.Fatal("empty args should yield nil keys")
	}
}

func TestResourcePathNormalizePlatform(t *testing.T) {
	// Unix-style clean
	got := NormalizeResourcePath("a//b/../c")
	if got != "a/c" && got != filepath.ToSlash(filepath.Clean("a//b/../c")) {
		// After ToSlash clean should be a/c
		if NormalizeResourcePath("a//b/../c") != "a/c" {
			t.Fatalf("unix clean = %q", got)
		}
	}
	if runtime.GOOS == "windows" {
		// Case-insensitive
		if NormalizeResourcePath(`C:\Foo\Bar`) != NormalizeResourcePath(`c:\foo\bar`) {
			t.Fatalf("windows case fold failed: %q vs %q",
				NormalizeResourcePath(`C:\Foo\Bar`), NormalizeResourcePath(`c:\foo\bar`))
		}
	}
	// Empty / URL-shaped
	if NormalizeResourcePath("") != "" {
		t.Fatal("empty path")
	}
	if NormalizeResourcePath("https://user:pass@host/path") != "" {
		t.Fatal("URL must not become a file resource path")
	}
}

func TestEndpointResourceKeysStripSecrets(t *testing.T) {
	keys := endpointResourceKeys(map[string]any{
		"url": "https://user:secret@api.example.com:443/v1?token=abc",
	})
	if len(keys) != 1 {
		t.Fatalf("keys = %v", keys)
	}
	if strings.Contains(keys[0], "secret") || strings.Contains(keys[0], "token") || strings.Contains(keys[0], "user") {
		t.Fatalf("endpoint key leaked secret material: %q", keys[0])
	}
	if !strings.Contains(keys[0], "api.example.com") {
		t.Fatalf("endpoint key missing host: %q", keys[0])
	}
}

func TestEndpointResourceKeysAtInQueryDoesNotStealHost(t *testing.T) {
	// Path/query must be stripped BEFORE userinfo so ?x=@bad cannot become the host.
	keys := endpointResourceKeys(map[string]any{
		"url": "https://api.example.com/v1?token=@secret",
	})
	if len(keys) != 1 || keys[0] != ResourceKeyEndpoint+"api.example.com" {
		t.Fatalf("keys = %v, want endpoint:api.example.com", keys)
	}
}

func TestProcessResourceKeysNumericSessionID(t *testing.T) {
	// write_stdin declares session_id as integer; JSON decodes as float64.
	keys := processResourceKeys(map[string]any{"session_id": float64(42)})
	if len(keys) != 1 || keys[0] != ResourceKeyProcess+"42" {
		t.Fatalf("keys = %v, want process:42", keys)
	}
}

func TestProcessResourceKeysSessionArg(t *testing.T) {
	// terminal_session requires "session", not session_id.
	keys := processResourceKeys(map[string]any{"session": "term-7"})
	if len(keys) != 1 || keys[0] != ResourceKeyProcess+"term-7" {
		t.Fatalf("keys = %v, want process:term-7", keys)
	}
}

func TestApplyPatchResourceKeysFromDiffAndCwd(t *testing.T) {
	patch := "--- a/old.go\n+++ b/new.go\n@@ -1 +1 @@\n-x\n+y\n"
	keys := applyPatchResourceKeys(map[string]any{
		"patch": patch,
		"cwd":   "pkg",
	})
	// Diff paths must be joined under cwd so the same basename under different
	// cwds does not collide for the future conflict planner.
	want := map[string]bool{
		ResourceKeyDirectory + "pkg":   true,
		ResourceKeyFile + "pkg/old.go": true,
		ResourceKeyFile + "pkg/new.go": true,
	}
	for _, k := range keys {
		if !want[k] {
			t.Fatalf("unexpected key %q in %v", k, keys)
		}
		delete(want, k)
	}
	if len(want) != 0 {
		t.Fatalf("missing keys %v from %v", want, keys)
	}
}

func TestApplyPatchResourceKeysFromStructuredPatchAndCwd(t *testing.T) {
	patch := "*** Begin Patch\n*** Update File: old.go\n*** Move to: new.go\n@@\n-old\n+new\n*** End Patch\n"
	keys := applyPatchResourceKeys(map[string]any{"patch": patch, "cwd": "pkg"})
	want := map[string]bool{
		ResourceKeyDirectory + "pkg":   true,
		ResourceKeyFile + "pkg/old.go": true,
		ResourceKeyFile + "pkg/new.go": true,
	}
	for _, key := range keys {
		if !want[key] {
			t.Fatalf("unexpected key %q in %v", key, keys)
		}
		delete(want, key)
	}
	if len(want) != 0 {
		t.Fatalf("missing keys %v from %v", want, keys)
	}
}

func TestDeclaredMutatorThreadSafeIsReportedByCatalogValidation(t *testing.T) {
	// Runtime CapabilitiesOf clears ThreadSafe on mutators; catalog validation
	// must still see the raw declaration so miswired constructors fail the gate.
	tool := deferredCapableTool{baseTool: baseTool{
		name: "bad_writer",
		capabilities: ToolCapabilities{
			Effect:     EffectWorkspaceWrite,
			ThreadSafe: true,
		},
	}}
	declared := declaredCapabilitiesOf(tool)
	if !declared.ThreadSafe || declared.Effect != EffectWorkspaceWrite {
		t.Fatalf("declared = %+v, want raw WorkspaceWrite+ThreadSafe", declared)
	}
	problems := ValidateCapabilities(tool.Name(), declared)
	if len(problems) == 0 {
		t.Fatal("expected ValidateCapabilities to reject raw WorkspaceWrite+ThreadSafe")
	}
	// Runtime view stays safe.
	if CapabilitiesOf(tool).ThreadSafe {
		t.Fatal("runtime CapabilitiesOf must still clear ThreadSafe")
	}
}

func TestApplyPatchResourceKeysFallbackWorkspace(t *testing.T) {
	keys := applyPatchResourceKeys(map[string]any{"patch": "not a real diff"})
	if len(keys) != 1 || keys[0] != ResourceKeyWorkspace+"root" {
		t.Fatalf("keys = %v, want workspace:root fallback", keys)
	}
}

func TestWorkspaceWriteClassifications(t *testing.T) {
	root := t.TempDir()
	for _, tool := range []Tool{
		NewScopedWriteFileTool(root, nil),
		NewScopedEditFileTool(root, nil),
		NewScopedApplyPatchTool(root, nil),
		NewScopedBashTool(root, nil),
	} {
		if got := CapabilitiesOf(tool).Effect; got != EffectWorkspaceWrite {
			t.Errorf("%s effect = %v, want WorkspaceWrite", tool.Name(), got)
		}
		if CapabilitiesOf(tool).ThreadSafe {
			t.Errorf("%s must not be ThreadSafe", tool.Name())
		}
	}
}

func TestInteractiveClassifications(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"ask_user", "exec_command", "write_stdin", "update_plan", "escalate_model", "request_permissions", "browser_open", "terminal_session", "desktop_action"} {
		var found Tool
		for _, tool := range BuiltinCatalog(root) {
			if tool.Name() == name {
				found = tool
				break
			}
		}
		if found == nil {
			t.Fatalf("tool %q not in catalog", name)
		}
		if got := CapabilitiesOf(found).Effect; got != EffectInteractive {
			t.Errorf("%s effect = %v, want Interactive", name, got)
		}
	}
}

func TestReadOnlyClassifications(t *testing.T) {
	root := t.TempDir()
	// PR6 audited concurrent-safe pure reads (mutex-guarded FileTracker,
	// independent scan I/O, independent HTTP). Others stay sequential until
	// their shared state is proven safe under concurrent Run.
	threadSafeReads := map[string]bool{
		"read_file":          true,
		"read_minified_file": true,
		"list_directory":     true,
		"glob":               true,
		"grep":               true,
		"skill":              true,
		"web_fetch":          true,
	}
	for _, name := range []string{"read_file", "read_minified_file", "list_directory", "glob", "grep", "lsp_navigate", "skill", "web_fetch", "web_search", "tool_search"} {
		var found Tool
		for _, tool := range BuiltinCatalog(root) {
			if tool.Name() == name {
				found = tool
				break
			}
		}
		if found == nil {
			t.Fatalf("tool %q not in catalog", name)
		}
		caps := CapabilitiesOf(found)
		if caps.Effect != EffectReadOnly {
			t.Errorf("%s effect = %v, want ReadOnly", name, caps.Effect)
		}
		wantSafe := threadSafeReads[name]
		if caps.ThreadSafe != wantSafe {
			t.Errorf("%s ThreadSafe=%v, want %v", name, caps.ThreadSafe, wantSafe)
		}
	}
}

func TestScopedScanResourceKeys(t *testing.T) {
	// Workspace-wide scan → no keys (do not false-conflict every concurrent scan).
	if keys := scopedScanResourceKeys(nil); keys != nil {
		t.Fatalf("nil args = %v, want nil", keys)
	}
	if keys := scopedScanResourceKeys(map[string]any{}); keys != nil {
		t.Fatalf("empty args = %v, want nil", keys)
	}
	if keys := scopedScanResourceKeys(map[string]any{"cwd": "."}); keys != nil {
		t.Fatalf("cwd=. = %v, want nil", keys)
	}
	if keys := scopedScanResourceKeys(map[string]any{"path": "."}); keys != nil {
		t.Fatalf("path=. = %v, want nil", keys)
	}
	// Scoped path → directory: key (glob uses cwd; grep uses path).
	keys := scopedScanResourceKeys(map[string]any{"cwd": "internal/tools"})
	if len(keys) != 1 || keys[0] != ResourceKeyDirectory+NormalizeResourcePath("internal/tools") {
		t.Fatalf("cwd scoped = %v", keys)
	}
	keys = scopedScanResourceKeys(map[string]any{"path": "./pkg/../pkg/api"})
	if len(keys) != 1 || keys[0] != ResourceKeyDirectory+NormalizeResourcePath("./pkg/../pkg/api") {
		t.Fatalf("path scoped = %v", keys)
	}
}

func TestValidateBuiltinCatalogRejectsUnknownRaw(t *testing.T) {
	// Prove the gate is not a rubber stamp: raw Unknown+ThreadSafe is rejected.
	problems := ValidateCapabilities("ghost", ToolCapabilities{Effect: EffectUnknown, ThreadSafe: true})
	if len(problems) == 0 {
		t.Fatal("expected ValidateCapabilities to reject Unknown+ThreadSafe")
	}
}

func TestCaptureArtifactIsWorkspaceWrite(t *testing.T) {
	for _, tool := range NewLocalControlArtifactTools(LocalControlArtifactOptions{}) {
		if CapabilitiesOf(tool).Effect != EffectWorkspaceWrite {
			t.Fatalf("%s effect = %v, want WorkspaceWrite", tool.Name(), CapabilitiesOf(tool).Effect)
		}
	}
}

func TestCapabilitiesLookupRace(t *testing.T) {
	reg := NewRegistry()
	for _, tool := range BuiltinCatalog(t.TempDir()) {
		reg.Register(tool)
	}
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for _, tool := range reg.All() {
				_ = reg.Capabilities(tool.Name())
				_ = CapabilitiesOf(tool)
				caps := CapabilitiesOf(tool)
				if caps.ResourceKeys != nil {
					_ = caps.ResourceKeys(map[string]any{"path": "a/b.go", "url": "https://example.com"})
				}
			}
		}()
	}
	wg.Wait()
}

func TestEffectStringAndValid(t *testing.T) {
	for _, e := range []EffectClass{EffectUnknown, EffectReadOnly, EffectProcessRead, EffectWorkspaceWrite, EffectExternalWrite, EffectInteractive} {
		if !e.Valid() {
			t.Fatalf("%v should be valid", e)
		}
		if e.String() == "invalid" || e.String() == "" {
			t.Fatalf("String for %d", e)
		}
	}
	if EffectClass(99).Valid() {
		t.Fatal("invalid class reported valid")
	}
}

// deferredCapableTool models a deferred-eligible built-in that still carries
// explicit capabilities (MCP tools do not implement CapabilityProvider and
// stay Unknown by design).
type deferredCapableTool struct {
	baseTool
}

func (t deferredCapableTool) Deferred() bool { return true }

func (t deferredCapableTool) Run(context.Context, map[string]any) Result {
	return okResult("ok")
}

func TestDeferredToolPreservesCapabilities(t *testing.T) {
	tool := deferredCapableTool{baseTool: baseTool{
		name: "deferred_reader",
		capabilities: ToolCapabilities{
			Effect:       EffectReadOnly,
			ThreadSafe:   false,
			ResourceKeys: fileResourceKeys,
		},
	}}
	if !IsDeferred(tool) {
		t.Fatal("expected deferred")
	}
	reg := NewRegistry()
	reg.Register(tool)
	got, ok := reg.Get("deferred_reader")
	if !ok {
		t.Fatal("missing registration")
	}
	caps := CapabilitiesOf(got)
	if caps.Effect != EffectReadOnly {
		t.Fatalf("effect = %v after deferred register", caps.Effect)
	}
	keys := caps.ResourceKeys(map[string]any{"path": "pkg/a.go"})
	if len(keys) != 1 || keys[0] != ResourceKeyFile+NormalizeResourcePath("pkg/a.go") {
		t.Fatalf("resource keys = %v", keys)
	}
}

func TestPluginAndMCPStyleDefaultUnknown(t *testing.T) {
	// Plugin and MCP adapters do not implement CapabilityProvider; they must
	// remain fail-closed (serialized) until they opt into a trusted contract.
	for _, name := range []string{"mcp_github_create_issue", "plugin_custom_tool"} {
		caps := CapabilitiesOf(plainTool{name: name})
		if caps.Effect != EffectUnknown || caps.ThreadSafe {
			t.Fatalf("%s = %+v, want Unknown/not-thread-safe", name, caps)
		}
	}
}

func TestSchemasUnchangedForCoreTools(t *testing.T) {
	// Metadata must not alter tool names or parameter shapes.
	root := t.TempDir()
	read := NewScopedReadFileTool(root, nil)
	if read.Name() != "read_file" {
		t.Fatalf("name = %q", read.Name())
	}
	params := read.Parameters()
	if params.Type != "object" {
		t.Fatalf("params type = %q", params.Type)
	}
	if _, ok := params.Properties["path"]; !ok {
		t.Fatal("path property missing")
	}
	// Safety path unchanged.
	if read.Safety().SideEffect != SideEffectRead {
		t.Fatalf("safety side effect changed: %v", read.Safety().SideEffect)
	}
}
