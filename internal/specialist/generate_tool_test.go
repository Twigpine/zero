package specialist

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/tools"
)

func TestGenerateToolCreatesSpecialist(t *testing.T) {
	projectDir := filepath.Join(t.TempDir(), "project")
	tool := NewGenerateTool(NewStorage(Paths{ProjectDir: projectDir}))

	result := tool.Run(context.Background(), map[string]any{
		"description":   "API review helper",
		"name":          "api-review",
		"system_prompt": "Review API diffs.",
		"tools":         []any{"read-only", "plan"},
	})

	if result.Status != tools.StatusOK {
		t.Fatalf("GenerateSpecialist status = %s output=%s", result.Status, result.Output)
	}
	path := filepath.Join(projectDir, "api-review.md")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("expected generated specialist file: %v", err)
	}
	if !strings.Contains(string(data), `name: "api-review"`) || !strings.Contains(string(data), "Review API diffs.") {
		t.Fatalf("unexpected generated file:\n%s", string(data))
	}
	if result.Meta["path"] != path || result.Meta["name"] != "api-review" {
		t.Fatalf("unexpected result meta: %#v", result.Meta)
	}
	if result.Meta["location"] != string(LocationProject) {
		t.Fatalf("location = %q, want project", result.Meta["location"])
	}
}

func TestGenerateToolResultAppendsWarnings(t *testing.T) {
	manifest := Manifest{
		Metadata: Metadata{Name: "api-review"},
		Location: LocationProject,
		FilePath: "/project/.zero/specialists/api-review.md",
		Warnings: []string{"specialist was updated, but replacement backup /project/.zero/specialists/.api-review.bak could not be removed: access denied"},
	}

	result := generateToolResult(manifest)

	want := "specialist: api-review\nlocation: project\npath: /project/.zero/specialists/api-review.md\nwarning: " + manifest.Warnings[0]
	if result.Status != tools.StatusOK || result.Output != want {
		t.Fatalf("result = %#v, want output %q", result, want)
	}
}

func TestGenerateToolResultNormalOutputUnchanged(t *testing.T) {
	manifest := Manifest{
		Metadata: Metadata{Name: "api-review"},
		Location: LocationProject,
		FilePath: "/project/.zero/specialists/api-review.md",
	}

	result := generateToolResult(manifest)

	want := "specialist: api-review\nlocation: project\npath: /project/.zero/specialists/api-review.md"
	if result.Output != want {
		t.Fatalf("output = %q, want %q", result.Output, want)
	}
}

func TestGenerateToolDerivesNameAndDefaultPrompt(t *testing.T) {
	projectDir := filepath.Join(t.TempDir(), "project")
	tool := NewGenerateTool(NewStorage(Paths{ProjectDir: projectDir}))

	result := tool.Run(context.Background(), map[string]any{"description": "Security Audit Helper"})

	if result.Status != tools.StatusOK {
		t.Fatalf("GenerateSpecialist status = %s output=%s", result.Status, result.Output)
	}
	data, err := os.ReadFile(filepath.Join(projectDir, "security-audit-helper.md"))
	if err != nil {
		t.Fatalf("expected generated specialist file: %v", err)
	}
	if !strings.Contains(string(data), "Purpose: Security Audit Helper") {
		t.Fatalf("default prompt missing purpose:\n%s", string(data))
	}
}

func TestGenerateToolRejectsInvalidLocation(t *testing.T) {
	tool := NewGenerateTool(NewStorage(Paths{ProjectDir: t.TempDir()}))

	result := tool.Run(context.Background(), map[string]any{
		"description": "Bad location",
		"location":    "remote",
	})

	if result.Status != tools.StatusError || !strings.Contains(result.Output, "location must be project") {
		t.Fatalf("invalid location result = %#v", result)
	}
}

func TestGenerateToolRejectsUserLocation(t *testing.T) {
	tool := NewGenerateTool(NewStorage(Paths{ProjectDir: t.TempDir()}))

	result := tool.Run(context.Background(), map[string]any{
		"description": "User-scoped profile",
		"location":    "user",
	})

	if result.Status != tools.StatusError || !strings.Contains(result.Output, "location must be project") {
		t.Fatalf("user location result = %#v", result)
	}
}

func TestGenerateToolOverwriteAndToolFormats(t *testing.T) {
	projectDir := filepath.Join(t.TempDir(), "project")
	tool := NewGenerateTool(NewStorage(Paths{ProjectDir: projectDir}))

	result := tool.Run(context.Background(), map[string]any{
		"description":   "API review",
		"name":          "api-review",
		"system_prompt": "First prompt.",
		"tools":         "read-only, plan",
	})
	if result.Status != tools.StatusOK {
		t.Fatalf("first create status = %s output=%s", result.Status, result.Output)
	}
	path := filepath.Join(projectDir, "api-review.md")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read generated file: %v", err)
	}
	for _, want := range []string{`"read-only"`, `"plan"`, "First prompt."} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("generated file missing %q:\n%s", want, string(data))
		}
	}

	result = tool.Run(context.Background(), map[string]any{
		"description":   "API review",
		"name":          "api-review",
		"system_prompt": "Second prompt.",
	})
	if result.Status != tools.StatusError || !strings.Contains(result.Output, "already exists") {
		t.Fatalf("duplicate create result = %#v", result)
	}

	result = tool.Run(context.Background(), map[string]any{
		"description":   "API review",
		"name":          "api-review",
		"system_prompt": "Second prompt.",
		"tools":         []string{"execute"},
		"overwrite":     true,
	})
	if result.Status != tools.StatusOK {
		t.Fatalf("overwrite status = %s output=%s", result.Status, result.Output)
	}
	data, err = os.ReadFile(path)
	if err != nil {
		t.Fatalf("read overwritten file: %v", err)
	}
	for _, want := range []string{`"execute"`, "Second prompt."} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("overwritten file missing %q:\n%s", want, string(data))
		}
	}
}

func TestGenerateToolValidationEdges(t *testing.T) {
	tests := []struct {
		name string
		args map[string]any
		want string
	}{
		{
			name: "empty description",
			args: map[string]any{"description": "   "},
			want: "requires description",
		},
		{
			name: "invalid name",
			args: map[string]any{"description": "Bad name", "name": "../escape"},
			want: "invalid specialist name",
		},
		{
			name: "non string tools",
			args: map[string]any{"description": "Bad tools", "tools": []any{"read-only", 12}},
			want: "parameter tools must be an array of strings",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tool := NewGenerateTool(NewStorage(Paths{ProjectDir: t.TempDir()}))

			result := tool.Run(context.Background(), tc.args)

			if result.Status != tools.StatusError || !strings.Contains(result.Output, tc.want) {
				t.Fatalf("result = %#v, want error containing %q", result, tc.want)
			}
		})
	}
}

func TestGenerateToolUsesFallbackSlugForSymbolsOnlyDescription(t *testing.T) {
	projectDir := filepath.Join(t.TempDir(), "project")
	tool := NewGenerateTool(NewStorage(Paths{ProjectDir: projectDir}))

	result := tool.Run(context.Background(), map[string]any{"description": "123 !!!"})

	if result.Status != tools.StatusOK {
		t.Fatalf("GenerateSpecialist status = %s output=%s", result.Status, result.Output)
	}
	if result.Meta["name"] != "specialist" {
		t.Fatalf("name = %q, want specialist", result.Meta["name"])
	}
	if _, err := os.Stat(filepath.Join(projectDir, "specialist.md")); err != nil {
		t.Fatalf("expected fallback specialist file: %v", err)
	}
}

func TestGenerateToolRejectsNilStorage(t *testing.T) {
	tool := NewGenerateTool(nil)

	result := tool.Run(context.Background(), map[string]any{"description": "Nil storage"})

	if result.Status != tools.StatusError || !strings.Contains(result.Output, "storage is not configured") {
		t.Fatalf("nil storage result = %#v", result)
	}
}
