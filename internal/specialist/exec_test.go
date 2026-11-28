package specialist

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestBuildArgsCreatesFreshSpecialistExecInvocation(t *testing.T) {
	executor := Executor{
		NewSessionID: func() (string, error) { return "child_session", nil },
	}
	manifest := Manifest{
		Metadata: Metadata{
			Name:            "reviewer",
			Description:     "Reviews code",
			Model:           "claude-sonnet-4.5",
			ReasoningEffort: "high",
		},
		SystemPrompt:  "Review carefully.",
		ResolvedTools: []string{"grep", "read_file"},
	}

	result, err := executor.BuildArgs(BuildArgsInput{
		Manifest:              manifest,
		Prompt:                "Review this patch",
		ParentSessionID:       "parent_session",
		ParentToolUseID:       "toolu_123",
		ParentModel:           "gpt-4.1",
		ParentReasoningEffort: "medium",
		CurrentDepth:          1,
		Description:           "Auth diff",
	})
	if err != nil {
		t.Fatalf("BuildArgs returned error: %v", err)
	}
	if result.SessionID != "child_session" || result.PromptFile != "" {
		t.Fatalf("unexpected result metadata: %#v", result)
	}
	wantArgs := []string{
		"exec", "--init-session-id", "child_session",
		result.Args[3],
		"--model", "claude-sonnet-4.5",
		"--reasoning-effort", "high",
		"--auto", "low", // no PermissionMode set -> fail-safe low (not unsafe high)
		"--output-format", "stream-json",
		"--enabled-tools", "grep,read_file",
		"--depth", "2",
		"--tag", "specialist",
		"--calling-session-id", "parent_session",
		"--calling-tool-use-id", "toolu_123",
		"--session-title", "reviewer: Auth diff",
	}
	if !reflect.DeepEqual(result.Args, wantArgs) {
		t.Fatalf("args mismatch\ngot:  %#v\nwant: %#v", result.Args, wantArgs)
	}
	prompt := result.Args[3]
	for _, want := range []string{"Specialist: reviewer", "Task description: Auth diff", "Review carefully.", "Review this patch"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("wrapped prompt missing %q:\n%s", want, prompt)
		}
	}
}

func TestSpecialistAutonomyByPermissionMode(t *testing.T) {
	cases := map[string]string{
		"": "low", // fail-safe: an unset mode does NOT inherit unsafe autonomy
		// Both spellings of the most permissive mode. The value arrives here as a
		// raw string, so the agent package's deprecated Go alias does not reach
		// it: when this table listed only "unsafe", renaming the mode to
		// "full-auto" silently dropped every full-auto parent's specialists to
		// read-only "low" and nothing failed.
		"full-auto":  "high",
		"unsafe":     "high",
		"auto":       "low",
		"ask":        "low",
		"spec-draft": "low",
		"whatever":   "low",
	}
	for mode, want := range cases {
		if got := specialistAutonomy(mode); got != want {
			t.Errorf("specialistAutonomy(%q) = %q, want %q", mode, got, want)
		}
	}
}

func TestMemberAwareAutonomy(t *testing.T) {
	// A non-unsafe member runs at "member" (write/edit + sandboxed shell), a plain
	// specialist at "low" (read-only), and an unsafe parent at "high" either way.
	cases := []struct {
		mode   string
		member bool
		want   string
	}{
		{"auto", false, "low"},
		{"ask", false, "low"},
		{"auto", true, "member"},
		{"ask", true, "member"},
		{"", true, "member"},     // member, fail-safe non-unsafe, still write-capable
		{"unsafe", true, "high"}, // unsafe parent keeps full autonomy
		{"unsafe", false, "high"},
		{"full-auto", true, "high"}, // the current spelling must behave identically
		{"full-auto", false, "high"},
	}
	for _, c := range cases {
		if got := memberAwareAutonomy(c.mode, c.member); got != c.want {
			t.Errorf("memberAwareAutonomy(%q, %v) = %q, want %q", c.mode, c.member, got, c.want)
		}
	}
}

func TestBuildArgsMemberAutonomyEmitsMember(t *testing.T) {
	executor := Executor{NewSessionID: func() (string, error) { return "child", nil }}
	manifest := Manifest{Metadata: Metadata{Name: "subagent"}, SystemPrompt: "x", ResolvedTools: []string{"read_file", "write_file"}}

	// A non-unsafe member → --auto member (write-capable), not the read-only low.
	res, err := executor.BuildArgs(BuildArgsInput{Manifest: manifest, Prompt: "p", PermissionMode: "auto", MemberAutonomy: true})
	if err != nil {
		t.Fatalf("BuildArgs: %v", err)
	}
	if !containsSequence(res.Args, []string{"--auto", "member"}) {
		t.Fatalf("non-unsafe member must yield --auto member, got %v", res.Args)
	}
	// --permission-mode auto would short-circuit resolveExecPermissionMode and
	// strip the member rung; member children must rely on --auto alone.
	if containsSequence(res.Args, []string{"--permission-mode"}) {
		t.Fatalf("non-plan member must not emit --permission-mode (would override --auto member), got %v", res.Args)
	}

	// Without the member flag, the same parent stays --auto low (unchanged).
	plain, err := executor.BuildArgs(BuildArgsInput{Manifest: manifest, Prompt: "p", PermissionMode: "auto"})
	if err != nil {
		t.Fatalf("BuildArgs(plain): %v", err)
	}
	if !containsSequence(plain.Args, []string{"--auto", "low"}) || containsSequence(plain.Args, []string{"--auto", "member"}) {
		t.Fatalf("a plain specialist must stay --auto low, got %v", plain.Args)
	}
	if containsSequence(plain.Args, []string{"--permission-mode"}) {
		t.Fatalf("auto parent must not emit --permission-mode, got %v", plain.Args)
	}

	// An unsafe member still runs --auto high, never downgraded to member.
	unsafe, err := executor.BuildArgs(BuildArgsInput{Manifest: manifest, Prompt: "p", PermissionMode: "unsafe", MemberAutonomy: true})
	if err != nil {
		t.Fatalf("BuildArgs(unsafe member): %v", err)
	}
	if !containsSequence(unsafe.Args, []string{"--auto", "high"}) {
		t.Fatalf("unsafe member must yield --auto high, got %v", unsafe.Args)
	}
	if containsSequence(unsafe.Args, []string{"--permission-mode"}) {
		t.Fatalf("unsafe parent must not emit --permission-mode, got %v", unsafe.Args)
	}
}

// TestBuildArgsPropagatesPermissionModeOnlyForPlanAndSpecDraft pins that
// --permission-mode is emitted only when --auto cannot express the mode
// (plan/spec-draft). Emitting it for auto/ask/member would either strip swarm
// member write tools or widen headless ask children past --auto low.
func TestBuildArgsPropagatesPermissionModeOnlyForPlanAndSpecDraft(t *testing.T) {
	executor := Executor{NewSessionID: func() (string, error) { return "child", nil }}
	manifest := Manifest{Metadata: Metadata{Name: "worker"}, SystemPrompt: "x", ResolvedTools: []string{"read_file", "write_file", "bash"}}

	for _, mode := range []string{"auto", "ask", "unsafe", "member", ""} {
		res, err := executor.BuildArgs(BuildArgsInput{Manifest: manifest, Prompt: "p", PermissionMode: mode})
		if err != nil {
			t.Fatalf("BuildArgs(%q): %v", mode, err)
		}
		if containsSequence(res.Args, []string{"--permission-mode"}) {
			t.Fatalf("mode %q must not emit --permission-mode, got %v", mode, res.Args)
		}
	}

	for _, mode := range []string{"plan", "spec-draft"} {
		res, err := executor.BuildArgs(BuildArgsInput{Manifest: manifest, Prompt: "p", PermissionMode: mode})
		if err != nil {
			t.Fatalf("BuildArgs(%q): %v", mode, err)
		}
		if !containsSequence(res.Args, []string{"--permission-mode", mode}) {
			t.Fatalf("mode %q must emit --permission-mode %s, got %v", mode, mode, res.Args)
		}
		if !containsSequence(res.Args, []string{"--auto", "low"}) {
			t.Fatalf("mode %q must still emit --auto low, got %v", mode, res.Args)
		}
	}

	// Resume path uses the same rule.
	resume, err := executor.BuildResumeArgs(BuildResumeArgsInput{
		SessionID: "child_session", Prompt: "p", Manifest: manifest, PermissionMode: "ask",
	})
	if err != nil {
		t.Fatalf("BuildResumeArgs(ask): %v", err)
	}
	if containsSequence(resume.Args, []string{"--permission-mode"}) {
		t.Fatalf("resume ask must not emit --permission-mode, got %v", resume.Args)
	}
	planResume, err := executor.BuildResumeArgs(BuildResumeArgsInput{
		SessionID: "child_session", Prompt: "p", Manifest: manifest, PermissionMode: "plan",
	})
	if err != nil {
		t.Fatalf("BuildResumeArgs(plan): %v", err)
	}
	if !containsSequence(planResume.Args, []string{"--permission-mode", "plan"}) {
		t.Fatalf("resume plan must emit --permission-mode plan, got %v", planResume.Args)
	}
}

func TestChildPermissionModeFlag(t *testing.T) {
	cases := map[string]string{
		"":           "",
		"auto":       "",
		"ask":        "",
		"unsafe":     "",
		"member":     "",
		"plan":       "plan",
		"spec-draft": "spec-draft",
		"  plan  ":   "plan",
	}
	for in, want := range cases {
		if got := childPermissionModeFlag(in); got != want {
			t.Errorf("childPermissionModeFlag(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBuildArgsAutonomyHonorsPermissionMode(t *testing.T) {
	executor := Executor{NewSessionID: func() (string, error) { return "child", nil }}
	manifest := Manifest{Metadata: Metadata{Name: "reviewer"}, SystemPrompt: "x", ResolvedTools: []string{"read_file"}}

	// A non-unsafe parent mode yields a non-unsafe child (--auto low), so a
	// swarm member never gains more authority than a non-unsafe orchestrator.
	res, err := executor.BuildArgs(BuildArgsInput{Manifest: manifest, Prompt: "p", PermissionMode: "auto"})
	if err != nil {
		t.Fatalf("BuildArgs: %v", err)
	}
	if !containsSequence(res.Args, []string{"--auto", "low"}) {
		t.Fatalf("non-unsafe parent must yield --auto low, got %v", res.Args)
	}
	if containsSequence(res.Args, []string{"--auto", "high"}) {
		t.Fatalf("non-unsafe parent must NOT yield --auto high: %v", res.Args)
	}

	// Only an explicit unsafe parent keeps --auto high.
	out, err := executor.BuildArgs(BuildArgsInput{Manifest: manifest, Prompt: "p", PermissionMode: "unsafe"})
	if err != nil {
		t.Fatalf("BuildArgs(unsafe): %v", err)
	}
	if !containsSequence(out.Args, []string{"--auto", "high"}) {
		t.Fatalf("unsafe parent must yield --auto high, got %v", out.Args)
	}

	// An unset mode is fail-safe low (the foot-gun fix): a caller that forgets to
	// wire PermissionMode does NOT silently get an unsafe child.
	empty, err := executor.BuildArgs(BuildArgsInput{Manifest: manifest, Prompt: "p", PermissionMode: ""})
	if err != nil {
		t.Fatalf("BuildArgs(empty): %v", err)
	}
	if !containsSequence(empty.Args, []string{"--auto", "low"}) {
		t.Fatalf("empty mode must yield --auto low (fail-safe), got %v", empty.Args)
	}
}

func TestBuildResumeArgsAutonomyHonorsPermissionMode(t *testing.T) {
	executor := Executor{}
	manifest := Manifest{Metadata: Metadata{Name: "reviewer"}, ResolvedTools: []string{"read_file"}}
	res, err := executor.BuildResumeArgs(BuildResumeArgsInput{
		SessionID: "child_session", Prompt: "p", Manifest: manifest, PermissionMode: "auto",
	})
	if err != nil {
		t.Fatalf("BuildResumeArgs: %v", err)
	}
	if !containsSequence(res.Args, []string{"--auto", "low"}) || containsSequence(res.Args, []string{"--auto", "high"}) {
		t.Fatalf("resume non-unsafe parent must yield --auto low, got %v", res.Args)
	}
}

func TestBuildArgsSessionTitleFallsBackToNameWithoutDescription(t *testing.T) {
	executor := Executor{NewSessionID: func() (string, error) { return "child", nil }}
	manifest := Manifest{
		Metadata:      Metadata{Name: "reviewer", Description: "Reviews code"},
		SystemPrompt:  "Review carefully.",
		ResolvedTools: []string{"read_file"},
	}

	// Description is optional. When omitted, the session title must still carry
	// the specialist name so AgentName (derived from the title) is non-empty and
	// the session remains resumable.
	result, err := executor.BuildArgs(BuildArgsInput{
		Manifest: manifest,
		Prompt:   "Do the thing",
	})
	if err != nil {
		t.Fatalf("BuildArgs returned error: %v", err)
	}
	if !containsSequence(result.Args, []string{"--session-title", "reviewer"}) {
		t.Fatalf("args missing name-only session title: %#v", result.Args)
	}
	for index, arg := range result.Args {
		if arg == "--session-title" && index+1 < len(result.Args) {
			if got := result.Args[index+1]; got != "reviewer" {
				t.Fatalf("session title = %q, want %q", got, "reviewer")
			}
		}
	}
}

func TestBuildArgsInheritsParentModelAndReasoning(t *testing.T) {
	executor := Executor{NewSessionID: func() (string, error) { return "child", nil }}
	manifest := Manifest{
		Metadata:      Metadata{Name: "worker", Description: "Works"},
		SystemPrompt:  "Do work.",
		ResolvedTools: []string{"read_file"},
	}

	result, err := executor.BuildArgs(BuildArgsInput{
		Manifest:              manifest,
		Prompt:                "Do the thing",
		ParentModel:           "gpt-4.1",
		ParentReasoningEffort: "medium",
	})
	if err != nil {
		t.Fatalf("BuildArgs returned error: %v", err)
	}
	if !containsSequence(result.Args, []string{"--model", "gpt-4.1"}) {
		t.Fatalf("args missing inherited model: %#v", result.Args)
	}
	if !containsSequence(result.Args, []string{"--reasoning-effort", "medium"}) {
		t.Fatalf("args missing inherited reasoning effort: %#v", result.Args)
	}
}

func TestBuildArgsDefaultsToReadOnlyToolAllowlist(t *testing.T) {
	executor := Executor{NewSessionID: func() (string, error) { return "child", nil }}

	result, err := executor.BuildArgs(BuildArgsInput{
		Manifest: Manifest{
			Metadata:     Metadata{Name: "worker", Description: "Works"},
			SystemPrompt: "Do work.",
		},
		Prompt: "Do the thing",
	})
	if err != nil {
		t.Fatalf("BuildArgs returned error: %v", err)
	}
	if !containsSequence(result.Args, []string{"--enabled-tools", "glob,grep,list_directory,read_file,read_minified_file,view_image"}) {
		t.Fatalf("args missing default read-only allowlist: %#v", result.Args)
	}
}

func TestBuildArgsWritesLargePromptFile(t *testing.T) {
	root := t.TempDir()
	var writtenPrompt string
	executor := Executor{
		NewSessionID:      func() (string, error) { return "child", nil },
		PromptFileMaxSize: 16,
		WritePromptFile: func(prompt string) (string, error) {
			writtenPrompt = prompt
			path := filepath.Join(root, "prompt.md")
			return path, os.WriteFile(path, []byte(prompt), 0o600)
		},
	}

	result, err := executor.BuildArgs(BuildArgsInput{
		Manifest: Manifest{
			Metadata:     Metadata{Name: "worker", Description: "Works"},
			SystemPrompt: strings.Repeat("system ", 10),
		},
		Prompt: "Do the large thing",
	})
	if err != nil {
		t.Fatalf("BuildArgs returned error: %v", err)
	}
	if result.PromptFile != filepath.Join(root, "prompt.md") {
		t.Fatalf("PromptFile = %q", result.PromptFile)
	}
	if !reflect.DeepEqual(result.Args[:5], []string{"exec", "--init-session-id", "child", "--file", result.PromptFile}) {
		t.Fatalf("prompt file args = %#v", result.Args[:5])
	}
	if !strings.Contains(writtenPrompt, "Do the large thing") {
		t.Fatalf("written prompt missing task: %s", writtenPrompt)
	}
}

func TestBuildResumeArgsUsesExistingSession(t *testing.T) {
	result, err := (Executor{}).BuildResumeArgs(BuildResumeArgsInput{
		SessionID:    "child_session",
		Prompt:       "Follow up",
		CurrentDepth: 2,
	})
	if err != nil {
		t.Fatalf("BuildResumeArgs returned error: %v", err)
	}
	wantPrefix := []string{"exec", "--resume", "child_session"}
	if !reflect.DeepEqual(result.Args[:3], wantPrefix) {
		t.Fatalf("resume args prefix = %#v", result.Args[:3])
	}
	if !containsSequence(result.Args, []string{"--auto", "low"}) || // no PermissionMode -> fail-safe low
		!containsSequence(result.Args, []string{"--output-format", "stream-json"}) ||
		!containsSequence(result.Args, []string{"--depth", "3"}) ||
		!containsSequence(result.Args, []string{"--tag", "specialist"}) {
		t.Fatalf("resume args missing required flags: %#v", result.Args)
	}
	if !strings.Contains(result.Args[3], "Follow-up Instructions") || !strings.Contains(result.Args[3], "Follow up") {
		t.Fatalf("resume prompt not wrapped correctly: %s", result.Args[3])
	}
}

func TestBuildArgsRejectsInvalidInputs(t *testing.T) {
	tests := []struct {
		name  string
		input BuildArgsInput
		want  string
	}{
		{name: "negative depth", input: BuildArgsInput{Prompt: "hi", CurrentDepth: -1}, want: "depth"},
		{name: "empty prompt", input: BuildArgsInput{CurrentDepth: 0}, want: "prompt"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := (Executor{}).BuildArgs(tc.input)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("BuildArgs error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestRunRejectsDepthExceedingMax(t *testing.T) {
	_, err := (Executor{}).Run(context.Background(), TaskParameters{
		Prompt: "hi",
	}, TaskRunOptions{CurrentDepth: maxSpecialistDepth + 1})
	if err == nil || !strings.Contains(err.Error(), "depth") {
		t.Fatalf("Run error = %v, want depth error", err)
	}
}

// TestRunRejectsDepthAtMax covers the boundary: a parent already AT the cap
// must be rejected too, since this Run call would launch a child one level
// past it (--depth CurrentDepth+1). Only checking ">" here would let that
// child start before the guard ever caught it. The guard sits before the
// fresh/resume branch in Run, so both call shapes must be proven to reject
// here rather than falling through to runFresh/runResume.
func TestRunRejectsDepthAtMax(t *testing.T) {
	tests := []struct {
		name   string
		params TaskParameters
	}{
		{name: "fresh", params: TaskParameters{Prompt: "hi"}},
		{name: "resume", params: TaskParameters{Prompt: "hi", Resume: "child_session"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := (Executor{}).Run(context.Background(), tc.params, TaskRunOptions{CurrentDepth: maxSpecialistDepth})
			if err == nil || !strings.Contains(err.Error(), "depth") {
				t.Fatalf("Run error = %v, want depth error", err)
			}
		})
	}
}

func TestBuildArgsRejectsInvalidSessionIDs(t *testing.T) {
	_, err := (Executor{NewSessionID: func() (string, error) { return "../escape", nil }}).BuildArgs(BuildArgsInput{
		Prompt: "hi",
	})
	if err == nil || !strings.Contains(err.Error(), "invalid specialist session id") {
		t.Fatalf("BuildArgs error = %v", err)
	}

	_, err = (Executor{}).BuildResumeArgs(BuildResumeArgsInput{
		SessionID: "../escape",
		Prompt:    "hi",
	})
	if err == nil || !strings.Contains(err.Error(), "invalid resume session id") {
		t.Fatalf("BuildResumeArgs error = %v", err)
	}
}

func TestBuildArgsNormalizesGeneratedSessionID(t *testing.T) {
	result, err := (Executor{
		NewSessionID: func() (string, error) { return " child_session ", nil },
	}).BuildArgs(BuildArgsInput{Prompt: "hi"})
	if err != nil {
		t.Fatalf("BuildArgs returned error: %v", err)
	}
	if result.SessionID != "child_session" {
		t.Fatalf("SessionID = %q, want child_session", result.SessionID)
	}
	if !containsSequence(result.Args, []string{"--init-session-id", "child_session"}) {
		t.Fatalf("args missing normalized session id: %#v", result.Args)
	}
}

func containsSequence(values []string, sequence []string) bool {
	for index := 0; index+len(sequence) <= len(values); index++ {
		if reflect.DeepEqual(values[index:index+len(sequence)], sequence) {
			return true
		}
	}
	return false
}
