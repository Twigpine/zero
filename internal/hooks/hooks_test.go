package hooks

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestResolvePathsUsesXDGLocations(t *testing.T) {
	dir := t.TempDir()
	paths, err := ResolvePaths(ResolvePathOptions{
		Cwd: dir,
		Env: map[string]string{
			"XDG_CONFIG_HOME": filepath.Join(dir, "config"),
			"XDG_DATA_HOME":   filepath.Join(dir, "data"),
		},
	})
	if err != nil {
		t.Fatalf("ResolvePaths returned error: %v", err)
	}
	if paths.UserConfigPath != filepath.Join(dir, "config", "zero", "hooks.json") {
		t.Fatalf("user path = %q", paths.UserConfigPath)
	}
	if paths.ProjectConfigPath != filepath.Join(dir, ".zero", "hooks.json") {
		t.Fatalf("project path = %q", paths.ProjectConfigPath)
	}
	if paths.AuditPath != filepath.Join(dir, "data", "zero", "hooks", "audit.jsonl") {
		t.Fatalf("audit path = %q", paths.AuditPath)
	}
}

func TestLoadConfigLayersProjectOverridesAndDiagnostics(t *testing.T) {
	dir := t.TempDir()
	userConfigPath := filepath.Join(dir, "user-hooks.json")
	projectConfigPath := filepath.Join(dir, "project-hooks.json")
	writeHookJSON(t, userConfigPath, map[string]any{
		"enabled": true,
		"hooks": []any{
			map[string]any{
				"id":      "zero.format",
				"name":    "Format after edits",
				"event":   "afterTool",
				"matcher": "edit_file",
				"command": "bun",
				"args":    []string{"run", "format"},
			},
			map[string]any{
				"id":      "zero.audit",
				"event":   "sessionEnd",
				"command": "node",
				"args":    []string{"audit.mjs"},
			},
		},
	})
	writeHookJSON(t, projectConfigPath, map[string]any{
		"hooks": []any{map[string]any{
			"id":      "zero.format",
			"event":   "afterTool",
			"matcher": "write_file",
			"command": "bun",
			"args":    []string{"run", "lint"},
			"enabled": false,
		}},
	})

	result, err := LoadConfig(LoadOptions{UserConfigPath: userConfigPath, ProjectConfigPath: projectConfigPath})
	if err != nil {
		t.Fatalf("LoadConfig returned error: %v", err)
	}
	if !result.Config.Enabled {
		t.Fatalf("config should be enabled")
	}
	if got := []string{result.Config.Hooks[0].ID, result.Config.Hooks[1].ID}; !reflect.DeepEqual(got, []string{"zero.audit", "zero.format"}) {
		t.Fatalf("hook order = %#v", got)
	}
	format := result.Config.Hooks[1]
	if format.Enabled || format.Matcher != "write_file" || !reflect.DeepEqual(format.Args, []string{"run", "lint"}) {
		t.Fatalf("project override not applied: %#v", format)
	}
	if !hasHookDiagnostic(result.Diagnostics, DiagnosticDuplicate, "zero.format", "") {
		t.Fatalf("missing duplicate diagnostic: %#v", result.Diagnostics)
	}
}

func TestLoadConfigExcludeProjectSkipsProjectLayer(t *testing.T) {
	userConfig := map[string]any{
		"enabled": true,
		"hooks": []any{map[string]any{
			"id":      "zero.user",
			"event":   "beforeTool",
			"command": "node",
			"args":    []string{"user.mjs"},
			"enabled": true,
		}},
	}
	projectConfig := map[string]any{
		"enabled": true,
		"hooks": []any{map[string]any{
			"id":      "zero.project",
			"event":   "beforeTool",
			"command": "node",
			"args":    []string{"project.mjs"},
			"enabled": true,
		}},
	}

	for _, tc := range []struct {
		name           string
		excludeProject bool
		wantIDs        []string
	}{
		{name: "include project (default)", excludeProject: false, wantIDs: []string{"zero.project", "zero.user"}},
		{name: "exclude project", excludeProject: true, wantIDs: []string{"zero.user"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			userConfigPath := filepath.Join(dir, "user-hooks.json")
			projectConfigPath := filepath.Join(dir, "project-hooks.json")
			writeHookJSON(t, userConfigPath, userConfig)
			writeHookJSON(t, projectConfigPath, projectConfig)

			result, err := LoadConfig(LoadOptions{
				UserConfigPath:    userConfigPath,
				ProjectConfigPath: projectConfigPath,
				ExcludeProject:    tc.excludeProject,
			})
			if err != nil {
				t.Fatalf("LoadConfig returned error: %v", err)
			}
			if got := hookIDs(result.Config.Hooks); !reflect.DeepEqual(got, tc.wantIDs) {
				t.Fatalf("hook ids = %#v, want %#v", got, tc.wantIDs)
			}
		})
	}
}

func TestLoadConfigPreservesUserDisabledStateWhenProjectOmitsEnabled(t *testing.T) {
	dir := t.TempDir()
	userConfigPath := filepath.Join(dir, "user-hooks.json")
	projectConfigPath := filepath.Join(dir, "project-hooks.json")
	writeHookJSON(t, userConfigPath, map[string]any{
		"enabled": false,
		"hooks": []any{map[string]any{
			"id":      "zero.format",
			"event":   "afterTool",
			"matcher": "edit_file",
			"command": "bun",
			"args":    []string{"run", "format"},
			"enabled": false,
		}},
	})
	writeHookJSON(t, projectConfigPath, map[string]any{
		"hooks": []any{map[string]any{
			"id":      "zero.format",
			"event":   "afterTool",
			"matcher": "write_file",
			"command": "bun",
			"args":    []string{"run", "lint"},
		}},
	})

	result, err := LoadConfig(LoadOptions{UserConfigPath: userConfigPath, ProjectConfigPath: projectConfigPath})
	if err != nil {
		t.Fatalf("LoadConfig returned error: %v", err)
	}
	if result.Config.Enabled {
		t.Fatalf("project layer without enabled should preserve user disabled config: %#v", result.Config)
	}
	if len(result.Config.Hooks) != 1 {
		t.Fatalf("expected one hook, got %#v", result.Config.Hooks)
	}
	format := result.Config.Hooks[0]
	if format.Enabled || format.Matcher != "write_file" || !reflect.DeepEqual(format.Args, []string{"run", "lint"}) {
		t.Fatalf("project override should preserve user-disabled hook state: %#v", format)
	}
}

func TestLoadConfigRejectsMatchersOnLifecycleHooks(t *testing.T) {
	for _, event := range []string{"sessionStart", "sessionEnd"} {
		t.Run(event, func(t *testing.T) {
			dir := t.TempDir()
			projectConfigPath := filepath.Join(dir, "hooks.json")
			writeHookJSON(t, projectConfigPath, map[string]any{
				"hooks": []any{map[string]any{
					"id":      "zero.lifecycle",
					"event":   event,
					"matcher": "bash",
					"command": "node",
				}},
			})

			result, err := LoadConfig(LoadOptions{
				UserConfigPath:    filepath.Join(dir, "missing-user-hooks.json"),
				ProjectConfigPath: projectConfigPath,
			})
			if err != nil {
				t.Fatalf("LoadConfig returned error: %v", err)
			}
			if len(result.Config.Hooks) != 0 {
				t.Fatalf("expected invalid hooks to be skipped: %#v", result.Config.Hooks)
			}
			if !hasHookDiagnostic(result.Diagnostics, DiagnosticSchema, "", "hooks.0.matcher") {
				t.Fatalf("missing matcher diagnostic: %#v", result.Diagnostics)
			}
		})
	}
}

func TestLoadConfigAcceptsMatchersOnSpecialistHooks(t *testing.T) {
	dir := t.TempDir()
	projectConfigPath := filepath.Join(dir, "hooks.json")
	writeHookJSON(t, projectConfigPath, map[string]any{
		"hooks": []any{map[string]any{
			"id":      "zero.explorer-start",
			"event":   "specialistStart",
			"matcher": "explorer",
			"command": "node",
		}},
	})

	result, err := LoadConfig(LoadOptions{
		UserConfigPath:    filepath.Join(dir, "missing-user-hooks.json"),
		ProjectConfigPath: projectConfigPath,
	})
	if err != nil {
		t.Fatalf("LoadConfig returned error: %v", err)
	}
	if len(result.Config.Hooks) != 1 || result.Config.Hooks[0].Matcher != "explorer" {
		t.Fatalf("specialist matcher hook = %#v", result.Config.Hooks)
	}
	if got := hookIDs(Select(result.Config, SelectInput{Event: EventSpecialistStart, ToolName: "explorer"})); !reflect.DeepEqual(got, []string{"zero.explorer-start"}) {
		t.Fatalf("matched selection = %#v", got)
	}
	if got := Select(result.Config, SelectInput{Event: EventSpecialistStart, ToolName: "worker"}); len(got) != 0 {
		t.Fatalf("unmatched specialist should be empty, got %#v", got)
	}
}

func TestConfigStorePersistsUpdates(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "hooks.json")
	store, err := NewConfigStore(StoreOptions{ConfigPath: configPath})
	if err != nil {
		t.Fatalf("NewConfigStore returned error: %v", err)
	}

	_, err = store.Upsert(Definition{
		ID:      "zero.preflight",
		Name:    "Preflight",
		Event:   EventBeforeTool,
		Matcher: "bash",
		Command: "node",
		Args:    []string{"hooks/preflight.mjs"},
	})
	if err != nil {
		t.Fatalf("Upsert returned error: %v", err)
	}
	config, err := store.List()
	if err != nil {
		t.Fatalf("List returned error: %v", err)
	}
	if !config.Hooks[0].Enabled {
		t.Fatalf("new hooks should default to enabled: %#v", config.Hooks[0])
	}
	upserted, err := store.Upsert(Definition{
		ID:      "zero.explicit",
		Event:   EventAfterTool,
		Command: "node",
		Enabled: false,
	})
	if err != nil {
		t.Fatalf("Upsert with zero-value Enabled returned error: %v", err)
	}
	if !upserted.Enabled {
		t.Fatalf("Upsert should default zero-value Enabled to true; use SetEnabled to disable: %#v", upserted)
	}
	changed, err := store.SetEnabled("zero.preflight", false)
	if err != nil || !changed {
		t.Fatalf("SetEnabled changed=%v err=%v", changed, err)
	}

	config, err = store.List()
	if err != nil {
		t.Fatalf("List returned error: %v", err)
	}
	preflight := findHook(config.Hooks, "zero.preflight")
	if preflight == nil || preflight.Enabled || preflight.Matcher != "bash" {
		t.Fatalf("unexpected stored hook: %#v", preflight)
	}
	removed, err := store.Remove("zero.preflight")
	if err != nil || !removed {
		t.Fatalf("Remove removed=%v err=%v", removed, err)
	}
	config, err = store.List()
	if err != nil {
		t.Fatalf("List returned error: %v", err)
	}
	if findHook(config.Hooks, "zero.preflight") != nil {
		t.Fatalf("expected zero.preflight to be removed, got %#v", config.Hooks)
	}
}

func TestConfigStoreListReadsOnlyStorePath(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "hooks.json")
	writeHookJSON(t, configPath, map[string]any{
		"hooks": []any{map[string]any{
			"id":      "zero.real",
			"event":   "beforeTool",
			"command": "node",
		}},
	})
	writeHookJSON(t, configPath+".user-missing", map[string]any{
		"hooks": []any{map[string]any{
			"id":      "zero.fake",
			"event":   "afterTool",
			"command": "node",
		}},
	})
	store, err := NewConfigStore(StoreOptions{ConfigPath: configPath})
	if err != nil {
		t.Fatalf("NewConfigStore returned error: %v", err)
	}

	config, err := store.List()
	if err != nil {
		t.Fatalf("List returned error: %v", err)
	}
	if got := hookIDs(config.Hooks); !reflect.DeepEqual(got, []string{"zero.real"}) {
		t.Fatalf("hook ids = %#v", got)
	}
}

func TestSelectMatchesEnabledHooksByEventAndWildcard(t *testing.T) {
	config := Config{Enabled: true, Hooks: []Definition{
		{ID: "zero.reads", Event: EventBeforeTool, Matcher: "read_*", Command: "node", Enabled: true},
		{ID: "zero.shell", Event: EventBeforeTool, Matcher: "bash", Command: "node", Enabled: false},
		{ID: "zero.done", Event: EventSessionEnd, Command: "node", Enabled: true},
		{ID: "zero.shell-edit", Event: EventBeforeTool, Matcher: "shell_*_edit", Command: "node", Enabled: true},
	}}

	if got := hookIDs(Select(config, SelectInput{Event: EventBeforeTool, ToolName: "read_file"})); !reflect.DeepEqual(got, []string{"zero.reads"}) {
		t.Fatalf("read selection = %#v", got)
	}
	if got := hookIDs(Select(config, SelectInput{Event: EventBeforeTool, ToolName: "shell_safe_edit"})); !reflect.DeepEqual(got, []string{"zero.shell-edit"}) {
		t.Fatalf("shell edit selection = %#v", got)
	}
	if got := Select(config, SelectInput{Event: EventBeforeTool, ToolName: "shell_safe_view"}); len(got) != 0 {
		t.Fatalf("unexpected selection: %#v", got)
	}
}

func TestSpecialistHookEventsLoadAndSelect(t *testing.T) {
	dir := t.TempDir()
	projectConfigPath := filepath.Join(dir, "hooks.json")
	writeHookJSON(t, projectConfigPath, map[string]any{
		"hooks": []any{
			map[string]any{
				"id":      "zero.specialist-start",
				"event":   "specialistStart",
				"command": "node",
			},
			map[string]any{
				"id":      "zero.specialist-stop",
				"event":   "specialistStop",
				"command": "node",
			},
		},
	})

	result, err := LoadConfig(LoadOptions{
		UserConfigPath:    filepath.Join(dir, "missing-user-hooks.json"),
		ProjectConfigPath: projectConfigPath,
	})
	if err != nil {
		t.Fatalf("LoadConfig returned error: %v", err)
	}
	if got := hookIDs(result.Config.Hooks); !reflect.DeepEqual(got, []string{"zero.specialist-start", "zero.specialist-stop"}) {
		t.Fatalf("hook ids = %#v", got)
	}
	if got := hookIDs(Select(result.Config, SelectInput{Event: EventSpecialistStart})); !reflect.DeepEqual(got, []string{"zero.specialist-start"}) {
		t.Fatalf("specialistStart selection = %#v", got)
	}
	if got := hookIDs(Select(result.Config, SelectInput{Event: EventSpecialistStop})); !reflect.DeepEqual(got, []string{"zero.specialist-stop"}) {
		t.Fatalf("specialistStop selection = %#v", got)
	}
}

func TestAuditStoreAppendsAndSkipsMalformedLines(t *testing.T) {
	auditPath := filepath.Join(t.TempDir(), "audit.jsonl")
	if err := os.WriteFile(auditPath, []byte(strings.Join([]string{
		`{"sequence":1,"createdAt":"2026-06-04T00:00:00Z","type":"hook_execution_started","hookId":"zero.seed","event":"sessionStart"}`,
		"{not-json",
		`{"sequence":2,"createdAt":"2026-06-04T00:00:01Z","type":"hook_execution_completed","hookId":"zero.seed","event":"sessionStart","status":"completed"}`,
		"",
	}, "\n")), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := NewAuditStore(AuditStoreOptions{
		AuditPath: auditPath,
		Now:       func() time.Time { return time.Date(2026, 6, 4, 0, 0, 2, 0, time.UTC) },
	})
	if err != nil {
		t.Fatalf("NewAuditStore returned error: %v", err)
	}

	events, err := store.ReadEvents()
	if err != nil {
		t.Fatalf("ReadEvents returned error: %v", err)
	}
	if got := []int{events[0].Sequence, events[1].Sequence}; !reflect.DeepEqual(got, []int{1, 2}) {
		t.Fatalf("initial sequences = %#v", got)
	}

	appended, err := store.AppendStarted(AppendStartedInput{
		HookID:   "zero.preflight",
		Event:    EventBeforeTool,
		Matcher:  "bash",
		Commands: []AuditCommand{{Command: "node", Args: []string{"hooks/preflight.mjs"}}},
	})
	if err != nil {
		t.Fatalf("AppendStarted returned error: %v", err)
	}
	if appended.Sequence != 3 || appended.CreatedAt != "2026-06-04T00:00:02Z" {
		t.Fatalf("unexpected appended event: %#v", appended)
	}
}

func writeHookJSON(t *testing.T, path string, value any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func hookIDs(definitions []Definition) []string {
	ids := make([]string, 0, len(definitions))
	for _, definition := range definitions {
		ids = append(ids, definition.ID)
	}
	return ids
}

func findHook(definitions []Definition, id string) *Definition {
	for index := range definitions {
		if definitions[index].ID == id {
			return &definitions[index]
		}
	}
	return nil
}

func hasHookDiagnostic(diagnostics []Diagnostic, kind DiagnosticKind, hookID string, fieldPath string) bool {
	for _, diagnostic := range diagnostics {
		if diagnostic.Kind != kind {
			continue
		}
		if hookID != "" && diagnostic.HookID != hookID {
			continue
		}
		if fieldPath != "" && diagnostic.FieldPath != fieldPath {
			continue
		}
		return true
	}
	return false
}
