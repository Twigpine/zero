package agentinit

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Twigpine/zero/internal/repoinfo"
)

func intPtr(n int) *int { return &n }

func TestFormatFactsRendersKnownFields(t *testing.T) {
	info := repoinfo.Info{
		PrimaryLanguage: "Go",
		LanguageCount:   3,
		Languages: []repoinfo.LangStat{
			{Name: "Go"}, {Name: "TypeScript"}, {Name: "Shell"},
		},
		FileCount:       820,
		LOCEstimate:     210000,
		MaxDepth:        6,
		WorkspaceType:   "go-modules",
		BuildTools:      []string{"go", "make"},
		TestTools:       []string{"go test"},
		CICD:            []string{"github-actions"},
		HasGit:          true,
		Branch:          "main",
		Contributors90d: intPtr(4),
	}
	got := FormatFacts(info)
	for _, want := range []string{
		"Primary language: Go (of 3 detected)",
		"Languages: Go, TypeScript, Shell",
		"~820 files, ~210000 LOC",
		"Build tools: go, make",
		"Test tools: go test",
		"CI/CD: github-actions",
		"Git: yes, branch main, 4 contributors",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("FormatFacts missing %q in:\n%s", want, got)
		}
	}
}

func TestFormatFactsOmitsEmpty(t *testing.T) {
	got := FormatFacts(repoinfo.Info{FileCount: 1})
	for _, unwanted := range []string{"Primary language", "Build tools", "CI/CD", "Git:", "Workspace"} {
		if strings.Contains(got, unwanted) {
			t.Fatalf("FormatFacts should omit %q for an empty Info, got:\n%s", unwanted, got)
		}
	}
}

func TestBuildPromptEmbedsFactsAndInstructions(t *testing.T) {
	// BuildPrompt runs repoinfo.Collect on a real dir; t.TempDir() is not a git
	// repo, so collection fails soft and the prompt explains it will investigate.
	prompt := BuildPrompt(context.Background(), t.TempDir(), time.Now())
	if !strings.Contains(prompt, "Generate an AGENTS.md") {
		t.Fatal("prompt should state the goal")
	}
	if !strings.Contains(prompt, "write a concise") && !strings.Contains(prompt, "AGENTS.md at the repo root") {
		t.Fatalf("prompt should include the write instructions, got:\n%s", prompt)
	}
	// Non-git temp dir → facts unavailable branch.
	if !strings.Contains(prompt, "unavailable") && !strings.Contains(prompt, "Pre-computed repository facts") {
		t.Fatalf("prompt should include a facts block (or its unavailable fallback), got:\n%s", prompt)
	}
}
