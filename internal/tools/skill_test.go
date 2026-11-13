package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeSkillFile(t *testing.T, dir string, name string, content string) {
	t.Helper()
	skillDir := filepath.Join(dir, name)
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", skillDir, err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatalf("write SKILL.md: %v", err)
	}
}

func TestSkillToolIsReadOnly(t *testing.T) {
	tool := NewSkillTool(t.TempDir())
	if tool.Name() != "skill" {
		t.Fatalf("Name = %q, want skill", tool.Name())
	}
	if tool.Safety().SideEffect != SideEffectRead {
		t.Fatalf("SideEffect = %s, want read", tool.Safety().SideEffect)
	}
	if tool.Safety().Permission != PermissionAllow {
		t.Fatalf("Permission = %s, want allow", tool.Safety().Permission)
	}
	if tool.Parameters().Type != "object" {
		t.Fatalf("schema type = %s, want object", tool.Parameters().Type)
	}
}

func TestSkillToolReturnsContentForKnownSkill(t *testing.T) {
	dir := t.TempDir()
	writeSkillFile(t, dir, "confirmation-policy", "---\nname: confirmation-policy\ndescription: ask first\n---\n\n# Confirmation Policy\n\nAsk before risky actions.")

	tool := NewSkillTool(dir)
	result := tool.Run(context.Background(), map[string]any{"name": "confirmation-policy"})
	if result.Status != StatusOK {
		t.Fatalf("Status = %s, want ok (output: %s)", result.Status, result.Output)
	}
	if !strings.Contains(result.Output, "Ask before risky actions.") {
		t.Fatalf("Output missing skill body: %q", result.Output)
	}
}

func TestSkillToolAcceptsSkillAlias(t *testing.T) {
	dir := t.TempDir()
	writeSkillFile(t, dir, "demo", "body of demo")

	tool := NewSkillTool(dir)
	result := tool.Run(context.Background(), map[string]any{"skill": "demo"})
	if result.Status != StatusOK {
		t.Fatalf("Status = %s, want ok (output: %s)", result.Status, result.Output)
	}
	if !strings.Contains(result.Output, "body of demo") {
		t.Fatalf("Output = %q", result.Output)
	}
}

func TestSkillToolUnknownSkillErrorsAndListsAvailable(t *testing.T) {
	dir := t.TempDir()
	writeSkillFile(t, dir, "alpha", "a")
	writeSkillFile(t, dir, "beta", "b")

	tool := NewSkillTool(dir)
	result := tool.Run(context.Background(), map[string]any{"name": "missing"})
	if result.Status != StatusError {
		t.Fatalf("Status = %s, want error", result.Status)
	}
	if !strings.Contains(result.Output, "alpha") || !strings.Contains(result.Output, "beta") {
		t.Fatalf("error should list available skills, got: %q", result.Output)
	}
}

func TestSkillToolMissingNameErrors(t *testing.T) {
	tool := NewSkillTool(t.TempDir())
	result := tool.Run(context.Background(), map[string]any{})
	if result.Status != StatusError {
		t.Fatalf("Status = %s, want error", result.Status)
	}
}

func TestSkillToolNoSkillsAvailable(t *testing.T) {
	tool := NewSkillTool(filepath.Join(t.TempDir(), "missing"))
	result := tool.Run(context.Background(), map[string]any{"name": "anything"})
	if result.Status != StatusError {
		t.Fatalf("Status = %s, want error", result.Status)
	}
	if !strings.Contains(strings.ToLower(result.Output), "no skills") {
		t.Fatalf("expected a no-skills message, got: %q", result.Output)
	}
}

// A skill that ships files beside its SKILL.md (#584) is no use if the model
// cannot find them, so the output names their directory and lists them.
func TestSkillToolPointsAtTheSkillsBundledFiles(t *testing.T) {
	dir := t.TempDir()
	writeSkillFile(t, dir, "pdf", "---\nname: pdf\n---\nRun scripts/fill.py.")
	script := filepath.Join(dir, "pdf", "scripts", "fill.py")
	if err := os.MkdirAll(filepath.Dir(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(script, []byte("print('fill')\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	result := NewSkillTool(dir).Run(context.Background(), map[string]any{"name": "pdf"})
	if result.Status != StatusOK {
		t.Fatalf("Status = %s (output: %s)", result.Status, result.Output)
	}
	for _, want := range []string{"Run scripts/fill.py.", filepath.Join(dir, "pdf"), "- scripts/fill.py"} {
		if !strings.Contains(result.Output, want) {
			t.Fatalf("output does not contain %q:\n%s", want, result.Output)
		}
	}
}
