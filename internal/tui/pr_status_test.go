package tui

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestPrServiceDetectsGitHubPRAndDiffStats(t *testing.T) {
	runner := &fakePRRunner{results: map[string]fakePRResult{
		"gh pr view --json url,number,baseRefName": {
			output: `{"url":"https://github.com/org/repo/pull/1337","number":1337,"baseRefName":"main"}`,
		},
		"git merge-base origin/main HEAD": {
			output: "abc123\n",
		},
		"git diff --numstat --ignore-submodules=dirty abc123": {
			output: "42\t7\tinternal/tui/view.go\n",
		},
	}}
	service := NewPrService("/workspace/zero")
	service.run = runner.Run

	state := service.detect(context.Background())

	if state.Status != PrFound || state.Provider != ProviderGitHub || state.PrNumber != "1337" {
		t.Fatalf("unexpected PR state: %#v", state)
	}
	if state.PrURL != "https://github.com/org/repo/pull/1337" {
		t.Fatalf("PrURL = %q", state.PrURL)
	}
	if state.Additions != 42 || state.Deletions != 7 {
		t.Fatalf("diff stats = +%d -%d, want +42 -7", state.Additions, state.Deletions)
	}
	if runner.called("glab mr view --json web_url,iid,target_branch") {
		t.Fatal("GitLab fallback should not run when GitHub PR is found")
	}
}

func TestPrServiceFallsBackToGitLabMR(t *testing.T) {
	runner := &fakePRRunner{results: map[string]fakePRResult{
		"gh pr view --json url,number,baseRefName": {
			err: errors.New("no pull request found"),
		},
		"glab mr view --json web_url,iid,target_branch": {
			output: `{"web_url":"https://gitlab.com/org/repo/-/merge_requests/55","iid":55,"target_branch":"develop"}`,
		},
		"git merge-base origin/develop HEAD": {
			output: "def456\n",
		},
		"git diff --numstat --ignore-submodules=dirty def456": {
			output: "4\t5\ta.go\n5\t6\tb.go\n",
		},
	}}
	service := NewPrService("/workspace/zero")
	service.run = runner.Run

	state := service.detect(context.Background())

	if state.Status != PrFound || state.Provider != ProviderGitLab || state.PrNumber != "55" {
		t.Fatalf("unexpected MR state: %#v", state)
	}
	if state.Additions != 9 || state.Deletions != 11 {
		t.Fatalf("diff stats = +%d -%d, want +9 -11", state.Additions, state.Deletions)
	}
}

func TestGetLocalDiffStatsFallsBackToOriginBase(t *testing.T) {
	runner := &fakePRRunner{results: map[string]fakePRResult{
		"git merge-base origin/main HEAD": {
			err: errors.New("unknown revision"),
		},
		"git diff --numstat --ignore-submodules=dirty origin/main": {
			output: "3\t0\tREADME.md\n",
		},
	}}

	additions, deletions, err := getLocalDiffStats(context.Background(), "/workspace/zero", "main", runner.Run)
	if err != nil {
		t.Fatalf("getLocalDiffStats returned error: %v", err)
	}
	if additions != 3 || deletions != 0 {
		t.Fatalf("diff stats = +%d -%d, want +3 -0", additions, deletions)
	}
}

func TestParseGitNumStatSumsLocaleIndependentColumns(t *testing.T) {
	additions, deletions := parseGitNumStat("10\t2\tfile one.go\n-\t-\tassets/logo.png\n3\t4\tdir/file.go\n")
	if additions != 13 || deletions != 6 {
		t.Fatalf("diff stats = +%d -%d, want +13 -6", additions, deletions)
	}
}

func TestBuildPRSegments(t *testing.T) {
	state := PrState{
		Status:    PrFound,
		PrURL:     "https://github.com/org/repo/pull/1337",
		PrNumber:  "1337",
		Additions: 42,
		Deletions: 7,
	}

	segments := BuildPRSegments(state, false)
	gotText := ""
	for _, segment := range segments {
		gotText += segment.Text
		if segment.URL != state.PrURL {
			t.Fatalf("segment %#v should carry PR hyperlink", segment)
		}
	}
	if gotText != "+42 -7 #1337" {
		t.Fatalf("segments text = %q", gotText)
	}
}

func TestTitleBarShowsPRDiffStatsBesideBranch(t *testing.T) {
	m := limeTestModel()
	m.cwd = "/workspace/zero"
	m.gitBranch = "feat/title-polish"
	m.prState = PrState{
		Status:    PrFound,
		PrURL:     "https://github.com/org/repo/pull/219",
		PrNumber:  "219",
		Additions: 144,
		Deletions: 32,
	}

	rendered := m.titleBar(120)
	plain := plainRender(t, rendered)
	for _, want := range []string{" feat/title-polish", "+144", "-32", "#219", "/workspace/zero"} {
		if !strings.Contains(plain, want) {
			t.Fatalf("title bar = %q, missing %q", plain, want)
		}
	}
	if !strings.Contains(rendered, "\x1b]8;;"+m.prState.PrURL+"\x1b\\") {
		t.Fatalf("title bar should include OSC 8 PR hyperlink: %q", rendered)
	}

	footer := plainRender(t, m.footerView(120))
	for _, notWant := range []string{"+144", "-32", "#219"} {
		if strings.Contains(footer, notWant) {
			t.Fatalf("footer = %q, should not contain title-bar PR stat %q", footer, notWant)
		}
	}
}

type fakePRResult struct {
	output string
	err    error
}

type fakePRRunner struct {
	results map[string]fakePRResult
	calls   []string
}

func (runner *fakePRRunner) Run(ctx context.Context, cwd string, name string, args ...string) (string, error) {
	call := strings.TrimSpace(name + " " + strings.Join(args, " "))
	runner.calls = append(runner.calls, call)
	result, ok := runner.results[call]
	if !ok {
		return "", errors.New("unexpected command: " + call)
	}
	return result.output, result.err
}

func (runner *fakePRRunner) called(call string) bool {
	for _, actual := range runner.calls {
		if actual == call {
			return true
		}
	}
	return false
}
