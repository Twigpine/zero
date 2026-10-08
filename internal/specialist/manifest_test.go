package specialist

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/Gitlawb/zero/internal/tools"
)

func TestParseMarkdownValidatesAndResolvesTools(t *testing.T) {
	manifest, err := ParseMarkdown(`---
name: code-review
description: Reviews a change
extends: worker
tools: [read-only, apply_patch]
unknown: kept
---
Review the diff.`)
	if err != nil {
		t.Fatalf("ParseMarkdown returned error: %v", err)
	}
	if manifest.Metadata.Name != "code-review" || manifest.SystemPrompt != "Review the diff." {
		t.Fatalf("unexpected manifest: %#v", manifest)
	}
	if manifest.Metadata.Extends != "worker" {
		t.Fatalf("Extends = %q, want worker", manifest.Metadata.Extends)
	}
	for _, want := range []string{"apply_patch", "glob", "grep", "list_directory", "read_file"} {
		if !contains(manifest.ResolvedTools, want) {
			t.Fatalf("resolved tools missing %q: %#v", want, manifest.ResolvedTools)
		}
	}
	if len(manifest.Warnings) != 1 || !strings.Contains(manifest.Warnings[0], "unknown") {
		t.Fatalf("expected unknown key warning, got %#v", manifest.Warnings)
	}
}

func TestParseMarkdownRejectsScalarTools(t *testing.T) {
	_, err := ParseMarkdown(`---
name: explorer
description: Explores code
tools: read-only
---
Explore.`)
	if err == nil || !strings.Contains(err.Error(), "tools must be an array") {
		t.Fatalf("expected tools array error, got %v", err)
	}
}

func TestParseMarkdownUsesDescriptionFallback(t *testing.T) {
	manifest, err := ParseMarkdown(`---
name: greeter
description: Greets the user
---`)
	if err != nil {
		t.Fatalf("ParseMarkdown returned error: %v", err)
	}
	if manifest.SystemPrompt != "Greets the user" {
		t.Fatalf("SystemPrompt = %q, want description fallback", manifest.SystemPrompt)
	}
	if len(manifest.Warnings) != 1 || !strings.Contains(manifest.Warnings[0], "description") {
		t.Fatalf("expected description fallback warning, got %#v", manifest.Warnings)
	}
	for _, want := range []string{"glob", "grep", "list_directory", "read_file"} {
		if !contains(manifest.ResolvedTools, want) {
			t.Fatalf("default resolved tools missing %q: %#v", want, manifest.ResolvedTools)
		}
	}
	if contains(manifest.ResolvedTools, "bash") || contains(manifest.ResolvedTools, "write_file") {
		t.Fatalf("default tools should be read-only, got %#v", manifest.ResolvedTools)
	}
}

func TestParseMarkdownValidatesModelAndReasoningEffort(t *testing.T) {
	manifest, err := ParseMarkdown(`---
name: reviewer
description: Reviews code
model: sonnet-4.5
reasoningEffort: HIGH
---
Review.`)
	if err != nil {
		t.Fatalf("ParseMarkdown returned error: %v", err)
	}
	if manifest.Metadata.Model != "claude-sonnet-4.5" {
		t.Fatalf("Model = %q, want canonical claude-sonnet-4.5", manifest.Metadata.Model)
	}
	if manifest.Metadata.ReasoningEffort != "high" {
		t.Fatalf("ReasoningEffort = %q, want high", manifest.Metadata.ReasoningEffort)
	}

	_, err = ParseMarkdown(`---
name: reviewer
description: Reviews code
model: fake-9000
---
Review.`)
	if err == nil || !strings.Contains(err.Error(), "unknown model") {
		t.Fatalf("expected unknown model error, got %v", err)
	}

	_, err = ParseMarkdown(`---
name: reviewer
description: Reviews code
reasoningEffort: IMPOSSIBLE
---
Review.`)
	if err == nil || !strings.Contains(err.Error(), "unknown reasoning effort") {
		t.Fatalf("expected unknown reasoning effort error, got %v", err)
	}
}

func TestParseMarkdownRejectsInvalidNamesUnknownToolsAndDuplicateKeys(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{
			name: "invalid name",
			content: `---
name: ../escape
description: Bad name
---
Prompt.`,
			want: "invalid specialist name",
		},
		{
			name: "unknown tool",
			content: `---
name: explorer
description: Explores code
tools: [web_crawl]
---
Prompt.`,
			want: "unknown tool or category",
		},
		{
			name: "forbidden generation tool",
			content: `---
name: generator
description: Generates specialists
tools: [GenerateSpecialist]
---
Prompt.`,
			want: "forbidden specialist tool",
		},
		{
			name: "duplicate key",
			content: `---
name: explorer
name: explorer-two
description: Explores code
---
Prompt.`,
			want: "duplicate frontmatter key",
		},
		{
			name: "malformed frontmatter",
			content: `---
name explorer
description: Explores code
---
Prompt.`,
			want: "invalid frontmatter line",
		},
		{
			name: "empty prompt",
			content: `---
name: empty
description:
---`,
			want: "system prompt cannot be empty",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseMarkdown(tc.content)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected %q error, got %v", tc.want, err)
			}
		})
	}
}

func TestParseMarkdownInlineListHonorsQuotedCommas(t *testing.T) {
	_, err := ParseMarkdown(`---
name: quoted
description: Quoted list
tools: ["read-only", "grep,glob"]
---
Prompt.`)
	if err == nil || !strings.Contains(err.Error(), `unknown tool or category "grep,glob"`) {
		t.Fatalf("expected quoted comma to stay in one token, got %v", err)
	}
}

func TestLoadMergesBuiltinsUserAndProjectByPrecedence(t *testing.T) {
	root := t.TempDir()
	userDir := filepath.Join(root, "user")
	projectDir := filepath.Join(root, "project")
	writeManifest(t, filepath.Join(userDir, "explorer.md"), `---
name: explorer
description: User explorer override
tools: [read-only]
---
User prompt.`)
	writeManifest(t, filepath.Join(projectDir, "worker.md"), `---
name: worker
description: Project worker override
tools: [execute]
---
Project prompt.`)
	writeManifest(t, filepath.Join(projectDir, "conflict.md"), `---
name: conflict
description: Project conflict
tools: [execute]
---
Project conflict prompt.`)
	writeManifest(t, filepath.Join(userDir, "conflict.md"), `---
name: conflict
description: User conflict
tools: [read-only]
---
User conflict prompt.`)

	result, err := Load(LoadOptions{Paths: Paths{UserDir: userDir, ProjectDir: projectDir}})
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	explorer, ok := Find(result, "explorer")
	if !ok {
		t.Fatal("explorer not found")
	}
	if explorer.Location != LocationUser || explorer.SystemPrompt != "User prompt." {
		t.Fatalf("unexpected explorer override: %#v", explorer)
	}
	worker, ok := Find(result, "worker")
	if !ok {
		t.Fatal("worker not found")
	}
	if worker.Location != LocationProject || !contains(worker.ResolvedTools, "bash") {
		t.Fatalf("unexpected worker override: %#v", worker)
	}
	conflict, ok := Find(result, "conflict")
	if !ok {
		t.Fatal("conflict not found")
	}
	if conflict.Location != LocationUser || conflict.SystemPrompt != "User conflict prompt." || contains(conflict.ResolvedTools, "bash") {
		t.Fatalf("user manifest should win same-name conflict, got %#v", conflict)
	}
}

func TestLoadResolvesExtendsChainsAndChildOverrides(t *testing.T) {
	root := t.TempDir()
	userDir := filepath.Join(root, "user")
	writeManifest(t, filepath.Join(userDir, "base.md"), `---
name: base
description: Base specialist
tools: [execute]
---
Base prompt.`)
	writeManifest(t, filepath.Join(userDir, "reviewer.md"), `---
name: reviewer
description: Reviews code
extends: base
---
Reviewer prompt.`)
	writeManifest(t, filepath.Join(userDir, "strict-reviewer.md"), `---
name: strict-reviewer
description: Reviews strictly
extends: reviewer
tools: [read-only]
---
Strict prompt.`)

	result, err := Load(LoadOptions{Paths: Paths{UserDir: userDir}})
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	reviewer, ok := Find(result, "reviewer")
	if !ok {
		t.Fatal("reviewer not found")
	}
	if reviewer.SystemPrompt != "Base prompt.\n\nReviewer prompt." {
		t.Fatalf("reviewer prompt = %q", reviewer.SystemPrompt)
	}
	if reviewer.Metadata.Extends != "base" || !contains(reviewer.ResolvedTools, "bash") {
		t.Fatalf("reviewer did not inherit base metadata/tools: %#v", reviewer)
	}

	strict, ok := Find(result, "strict-reviewer")
	if !ok {
		t.Fatal("strict-reviewer not found")
	}
	if strict.SystemPrompt != "Base prompt.\n\nReviewer prompt.\n\nStrict prompt." {
		t.Fatalf("strict prompt = %q", strict.SystemPrompt)
	}
	if strict.Metadata.Extends != "reviewer" || contains(strict.ResolvedTools, "bash") || !contains(strict.ResolvedTools, "read_file") {
		t.Fatalf("strict reviewer should override inherited tools with read-only: %#v", strict)
	}
}

func TestLoadRejectsMissingExtendsBaseAndCycles(t *testing.T) {
	t.Run("missing base", func(t *testing.T) {
		userDir := filepath.Join(t.TempDir(), "user")
		writeManifest(t, filepath.Join(userDir, "child.md"), `---
name: child
description: Child specialist
extends: ghost
---
Child prompt.`)

		_, err := Load(LoadOptions{Paths: Paths{UserDir: userDir}})
		if err == nil || !strings.Contains(err.Error(), `base specialist "ghost" for "child" not found`) {
			t.Fatalf("expected missing base error, got %v", err)
		}
	})

	t.Run("cycle", func(t *testing.T) {
		userDir := filepath.Join(t.TempDir(), "user")
		writeManifest(t, filepath.Join(userDir, "a.md"), `---
name: a
description: A specialist
extends: b
---
A prompt.`)
		writeManifest(t, filepath.Join(userDir, "b.md"), `---
name: b
description: B specialist
extends: a
---
B prompt.`)

		_, err := Load(LoadOptions{Paths: Paths{UserDir: userDir}})
		if err == nil || !strings.Contains(err.Error(), "cycle detected in specialist extends chain") {
			t.Fatalf("expected cycle error, got %v", err)
		}
	})
}

func TestLoadSkipsBadFilesAndSymlinksWithWarnings(t *testing.T) {
	root := t.TempDir()
	userDir := filepath.Join(root, "user")
	writeManifest(t, filepath.Join(userDir, "valid.md"), `---
name: valid
description: Valid specialist
---
Prompt.`)
	writeManifest(t, filepath.Join(userDir, "bad.md"), `---
name: bad
tools: [missing]
---
Prompt.`)
	target := filepath.Join(userDir, "valid.md")
	link := filepath.Join(userDir, "linked.md")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unavailable on this platform: %v", err)
	}

	result, err := Load(LoadOptions{Paths: Paths{UserDir: userDir}})
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if _, ok := Find(result, "valid"); !ok {
		t.Fatalf("valid manifest should still load: %#v", result)
	}
	if _, ok := Find(result, "bad"); ok {
		t.Fatalf("bad manifest should be skipped: %#v", result)
	}
	warnings := strings.Join(result.Warnings, "\n")
	if !strings.Contains(warnings, "skipped invalid specialist manifest") || !strings.Contains(warnings, "skipped symlink specialist manifest") {
		t.Fatalf("expected invalid-file and symlink warnings, got %#v", result.Warnings)
	}
}

func TestKnownToolNamesMatchCoreRegistry(t *testing.T) {
	// web_search is only registered when a search backend is configured; set one so
	// CoreToolsScoped(, nil) exposes the full set this list is meant to mirror.
	t.Setenv("ZERO_WEBSEARCH_BASE_URL", "https://search.example/api")
	core := tools.CoreToolsScoped(t.TempDir(), nil)
	got := make([]string, 0, len(knownToolNames))
	for name := range knownToolNames {
		got = append(got, name)
	}
	want := make([]string, 0, len(core))
	for _, tool := range core {
		want = append(want, tool.Name())
	}
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("knownToolNames drifted from core registry\ngot:  %#v\nwant: %#v", got, want)
	}
	for category, names := range toolCategories {
		for _, name := range names {
			if !knownToolNames[name] {
				t.Fatalf("category %q references unknown tool %q", category, name)
			}
		}
	}
}

func TestFormatListUsesSpecialistTerminology(t *testing.T) {
	result := LoadResult{Specialists: []Manifest{{
		Metadata: Metadata{Name: "worker", Description: "Does work", Extends: "base"},
		Location: LocationBuiltin,
		FilePath: "(builtin)",
	}}}
	output := FormatList(result)
	if !strings.Contains(output, "Zero Specialists") || !strings.Contains(output, "worker [builtin]") {
		t.Fatalf("unexpected list output: %s", output)
	}
	show := FormatShow(result.Specialists[0])
	if !strings.Contains(show, "extends: base") {
		t.Fatalf("show output missing extends: %s", show)
	}
}

func writeManifest(t *testing.T, path string, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("create manifest dir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
