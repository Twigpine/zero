package tui

import (
	"context"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/Twigpine/zero/internal/agent"
	"github.com/Twigpine/zero/internal/skills"
)

// newSkillTestModel builds a model with an injected skills loader (and the
// matching palette metadata), no filesystem involved.
func newSkillTestModel(t *testing.T, installed ...skills.Skill) model {
	t.Helper()
	infos := make([]agent.SkillInfo, 0, len(installed))
	for _, s := range installed {
		infos = append(infos, agent.SkillInfo{Name: s.Name, Description: s.Description})
	}
	return newModel(context.Background(), Options{
		Cwd:          t.TempDir(),
		LoadSkills:   func() []skills.Skill { return installed },
		AgentOptions: agent.Options{Skills: infos},
	})
}

func submitInput(t *testing.T, m model, input string) model {
	t.Helper()
	m.input.SetValue(input)
	updated, _ := m.Update(testKey(tea.KeyEnter))
	return updated.(model)
}

func TestSkillCommandInvokesAndSubmits(t *testing.T) {
	m := newSkillTestModel(t, skills.Skill{
		Name:        "deploy-checks",
		Description: "Run the pre-deploy verification suite.",
		Content:     "Run every verification step and summarize failures.",
		Path:        "/skills/deploy-checks/SKILL.md",
	})

	next := submitInput(t, m, "/deploy-checks ship v2 to production")

	if transcriptContains(next.transcript, "unknown command") {
		t.Fatalf("an installed skill must not be 'unknown', got %#v", next.transcript)
	}
	if !transcriptContains(next.transcript, "Run every verification step and summarize failures.") {
		t.Fatalf("skill body should be inlined into the prompt, got %#v", next.transcript)
	}
	if !transcriptContains(next.transcript, "ship v2 to production") {
		t.Fatalf("typed args should follow the skill body, got %#v", next.transcript)
	}
}

func TestSkillCommandWithoutArgs(t *testing.T) {
	m := newSkillTestModel(t, skills.Skill{Name: "reviewer", Content: "Review the diff for correctness."})

	next := submitInput(t, m, "/reviewer")

	if !transcriptContains(next.transcript, "Review the diff for correctness.") {
		t.Fatalf("bare skill invocation should submit the body, got %#v", next.transcript)
	}
	// A bare invocation carries the ask-first note: instructions with no target
	// must make the model ask ("which PR?") instead of improvising one.
	if !transcriptContains(next.transcript, "ask for them first") {
		t.Fatalf("bare invocation should carry the clarify-first note, got %#v", next.transcript)
	}
}

// An invocation WITH a request must not carry the ask-first note — the user
// already said what to apply the skill to.
func TestSkillCommandWithArgsHasNoClarifyNote(t *testing.T) {
	m := newSkillTestModel(t, skills.Skill{Name: "reviewer", Content: "Review the diff."})

	next := submitInput(t, m, "/reviewer PR 484 reconnect changes")

	if transcriptContains(next.transcript, "ask for them first") {
		t.Fatalf("args invocation must not carry the clarify note, got %#v", next.transcript)
	}
}

// A skill's frontmatter name is free-form; invocation matches its lowercased
// slash form.
func TestSkillCommandNameIsCaseInsensitive(t *testing.T) {
	m := newSkillTestModel(t, skills.Skill{Name: "Deploy-Checks", Content: "Check things."})

	next := submitInput(t, m, "/deploy-checks")

	if !transcriptContains(next.transcript, "Check things.") {
		t.Fatalf("uppercase frontmatter name should be invocable lowercased, got %#v", next.transcript)
	}
}

// Precedence: a user command (.zero/commands) shadows a same-named skill.
func TestUserCommandShadowsSkill(t *testing.T) {
	root := t.TempDir()
	writeUserCommand(t, root, "greet.md", "Say hello from the user command.")
	m := newModel(context.Background(), Options{
		Cwd:        root,
		LoadSkills: func() []skills.Skill { return []skills.Skill{{Name: "greet", Content: "Skill body must not run."}} },
	})

	next := submitInput(t, m, "/greet")

	if !transcriptContains(next.transcript, "Say hello from the user command.") {
		t.Fatalf("user command should win the name collision, got %#v", next.transcript)
	}
	if transcriptContains(next.transcript, "Skill body must not run.") {
		t.Fatalf("shadowed skill must not run, got %#v", next.transcript)
	}
}

// An empty skill body is a real, named match — surface the problem instead of
// falling through to a misleading "unknown command".
func TestSkillCommandEmptyBody(t *testing.T) {
	m := newSkillTestModel(t, skills.Skill{Name: "hollow", Path: "/skills/hollow/SKILL.md"})

	next := submitInput(t, m, "/hollow")

	if transcriptContains(next.transcript, "unknown command") {
		t.Fatalf("an empty skill must not be 'unknown', got %#v", next.transcript)
	}
	if !transcriptContains(next.transcript, "empty SKILL.md body") {
		t.Fatalf("empty-body error should be surfaced, got %#v", next.transcript)
	}
}

func TestUnknownSlashStillUnknownWithSkillsInstalled(t *testing.T) {
	m := newSkillTestModel(t, skills.Skill{Name: "reviewer", Content: "Review."})

	next := submitInput(t, m, "/definitelynotaskill")

	if !transcriptContains(next.transcript, "unknown command") {
		t.Fatalf("non-matching name should still be unknown, got %#v", next.transcript)
	}
}

func TestSkillSlashName(t *testing.T) {
	cases := map[string]string{
		"deploy-checks":  "deploy-checks",
		"Deploy-Checks":  "deploy-checks",
		"  pdf_tools  ":  "pdf_tools",
		"v2.migrate":     "v2.migrate",
		"has space":      "",
		"emoji✨":         "",
		"":               "",
		"slash/inside":   "",
		"tab\tseparated": "",
	}
	for in, want := range cases {
		if got := skillSlashName(in); got != want {
			t.Errorf("skillSlashName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSkillAppearsInAutocomplete(t *testing.T) {
	m := newSkillTestModel(t, skills.Skill{Name: "deploy-checks", Description: "Run the pre-deploy suite.", Content: "x"})

	got := m.matchCommandSuggestions("/dep")
	found := false
	for _, s := range got {
		if s.Name == "/deploy-checks" {
			found = true
			if s.Desc != "Run the pre-deploy suite. (skill)" {
				t.Fatalf("skill suggestion should be labeled, got %q", s.Desc)
			}
		}
	}
	if !found {
		t.Fatalf("skill /deploy-checks should appear for '/dep', got %#v", got)
	}
}

// A skill shadowed by a builtin (or its alias) or a user command would be dead
// at dispatch time, so the palette must not advertise it.
func TestShadowedSkillNotSuggested(t *testing.T) {
	root := t.TempDir()
	writeUserCommand(t, root, "deploy.md", "User deploy.")
	installed := []skills.Skill{
		{Name: "help", Content: "x"},     // builtin collision
		{Name: "sessions", Content: "x"}, // builtin ALIAS collision (/resume alias)
		{Name: "deploy", Content: "x"},   // user-command collision
	}
	infos := make([]agent.SkillInfo, 0, len(installed))
	for _, s := range installed {
		infos = append(infos, agent.SkillInfo{Name: s.Name})
	}
	m := newModel(context.Background(), Options{
		Cwd:          root,
		LoadSkills:   func() []skills.Skill { return installed },
		AgentOptions: agent.Options{Skills: infos},
	})

	// Assert on the advertised NAME with a skill-marked Desc, not on the exact
	// fallback Desc format — the invariant is that no skill row carries a
	// builtin/user-command name, regardless of description wording.
	for _, token := range []string{"/hel", "/ses", "/dep"} {
		for _, s := range m.matchSkillSuggestions(token) {
			if s.Name == "/help" || s.Name == "/sessions" || s.Name == "/deploy" {
				t.Fatalf("shadowed skill advertised for %q: %#v", token, s)
			}
		}
	}
}

// Invalid slash shapes (spaces etc.) and a bare "/" must not surface skills.
func TestSkillSuggestionEdgeCases(t *testing.T) {
	m := newSkillTestModel(t, skills.Skill{Name: "has space", Description: "not invocable", Content: "x"})

	if got := m.matchSkillSuggestions("/has"); len(got) != 0 {
		t.Fatalf("invalid slash-shape skill must not be suggested, got %#v", got)
	}
	if got := m.matchSkillSuggestions("/"); got != nil {
		t.Fatalf("bare '/' should not list skills, got %#v", got)
	}
}

func TestSkillsCommandResolves(t *testing.T) {
	command, ok := resolveCommand("/skills")
	if !ok || command.kind != commandSkills {
		t.Fatalf("resolveCommand(/skills) = %#v, %v", command, ok)
	}
}

// With skills installed, /skills opens a searchable picker (like /model).
func TestSkillsCommandOpensPicker(t *testing.T) {
	m := newSkillTestModel(t,
		skills.Skill{Name: "deploy-checks", Description: "Run the pre-deploy suite.", Content: "x"},
		skills.Skill{Name: "has space", Description: "tool-only skill", Content: "x"},
	)

	next := submitInput(t, m, "/skills")

	if next.picker == nil || next.picker.kind != pickerSkill {
		t.Fatalf("/skills should open the skill picker, got %#v", next.picker)
	}
	labels := map[string]string{}
	for _, item := range next.picker.items {
		labels[item.Label] = item.Value
	}
	if labels["/deploy-checks"] != "deploy-checks" {
		t.Fatalf("slash-invocable skill should show its slash form, got %#v", labels)
	}
	// A non-slash-shaped name is still selectable (bare label, exact-name value).
	if labels["has space"] != "has space" {
		t.Fatalf("non-slash skill should be listed bare but selectable, got %#v", labels)
	}
}

// Enter on a picker row fills the composer with "/name " so the user can add
// their request (which PR? which file?) before submitting — it must NOT fire
// the skill with no request attached.
func TestSkillPickerEnterFillsComposer(t *testing.T) {
	m := newSkillTestModel(t, skills.Skill{Name: "reviewer", Content: "Review the diff for correctness."})

	next := submitInput(t, m, "/skills")
	if next.picker == nil {
		t.Fatal("picker should be open")
	}
	updated, _ := next.Update(testKey(tea.KeyEnter))
	after := updated.(model)

	if after.picker != nil {
		t.Fatal("picker should close on Enter")
	}
	if got := after.input.Value(); got != "/reviewer " {
		t.Fatalf("composer should be filled with the invocation, got %q", got)
	}
	if transcriptContains(after.transcript, "Review the diff for correctness.") {
		t.Fatalf("skill must not run before the user submits, got %#v", after.transcript)
	}

	// A second Enter submits the (bare) invocation and runs the skill.
	final, _ := after.Update(testKey(tea.KeyEnter))
	done := final.(model)
	if !transcriptContains(done.transcript, "Review the diff for correctness.") {
		t.Fatalf("submitting the filled composer should run the skill, got %#v", done.transcript)
	}
}

// Picker selection is by exact name, so a skill shadowed by a builtin (dead on
// the slash path) is still runnable from the picker.
func TestSkillPickerRunsBuiltinShadowedSkill(t *testing.T) {
	m := newSkillTestModel(t, skills.Skill{Name: "help", Description: "shadowed", Content: "Shadowed skill body."})

	next := submitInput(t, m, "/skills")
	if next.picker == nil {
		t.Fatal("picker should be open")
	}
	updated, _ := next.Update(testKey(tea.KeyEnter))
	after := updated.(model)

	if !transcriptContains(after.transcript, "Shadowed skill body.") {
		t.Fatalf("picker must run the shadowed skill directly, got %#v", after.transcript)
	}
}

func TestSkillsCommandEmptyState(t *testing.T) {
	m := newModel(context.Background(), Options{Cwd: t.TempDir()})

	next := submitInput(t, m, "/skills")

	if !transcriptContains(next.transcript, "No skills installed.") {
		t.Fatalf("empty state should say no skills, got %#v", next.transcript)
	}
}

// Run-state guards: an expanded skill prompt must not race an active run — it
// queues (as the EXPANDED prompt, since the queue flush submits literal text).
func TestSkillInvocationQueuedWhileRunPending(t *testing.T) {
	m := newSkillTestModel(t, skills.Skill{Name: "reviewer", Content: "Review the diff."})
	m.pending = true

	next, _, handled := m.handleSkillCommand("/reviewer focus on tests")
	if !handled {
		t.Fatal("skill must be handled even while a run is pending")
	}
	if next.queuedMessage == "" || !strings.Contains(next.queuedMessage, "Review the diff.") {
		t.Fatalf("expanded prompt should be queued, got %q", next.queuedMessage)
	}
	if !strings.Contains(next.queuedMessage, "focus on tests") {
		t.Fatalf("queued prompt should carry the args, got %q", next.queuedMessage)
	}
}

func TestSkillInvocationDroppedWhileExiting(t *testing.T) {
	m := newSkillTestModel(t, skills.Skill{Name: "reviewer", Content: "Review."})
	m.exiting = true

	next, cmd, handled := m.handleSkillCommand("/reviewer")
	if !handled || cmd != nil || next.queuedMessage != "" {
		t.Fatalf("exiting must swallow the invocation: handled=%v cmd=%v queued=%q", handled, cmd, next.queuedMessage)
	}
}

func TestSkillInvocationWarnsDuringCompaction(t *testing.T) {
	m := newSkillTestModel(t, skills.Skill{Name: "reviewer", Content: "Review."})
	m.compactInFlight = true

	next, _, handled := m.handleSkillCommand("/reviewer")
	if !handled {
		t.Fatal("skill must be handled during compaction")
	}
	if !transcriptContains(next.transcript, "Compaction is running") {
		t.Fatalf("compaction warning expected, got %#v", next.transcript)
	}
	// The typed invocation is restored for an easy re-submit (the dispatch path
	// cleared the composer before the guard could fire).
	if got := next.input.Value(); got != "/reviewer" {
		t.Fatalf("typed invocation should be restored into the composer, got %q", got)
	}
}

// The same guard now protects user commands (previously they could start a
// second concurrent run).
func TestUserCommandQueuedWhileRunPending(t *testing.T) {
	root := t.TempDir()
	writeUserCommand(t, root, "greet.md", "Say hello to $1.")
	m := newModel(context.Background(), Options{Cwd: root})
	m.pending = true

	next, _, handled := m.handleUserCommand("/greet world")
	if !handled {
		t.Fatal("user command must be handled while pending")
	}
	if !strings.Contains(next.queuedMessage, "Say hello to world.") {
		t.Fatalf("expanded user-command prompt should be queued, got %q", next.queuedMessage)
	}
}

// Two skills whose names collide after lowercasing map to one slash name; only
// the one dispatch will run may be advertised.
func TestCaseCollidingSkillsAdvertisedOnce(t *testing.T) {
	m := newSkillTestModel(t,
		skills.Skill{Name: "Deploy", Description: "wins", Content: "a"},
		skills.Skill{Name: "deploy", Description: "loses", Content: "b"},
	)

	rows := 0
	for _, s := range m.matchSkillSuggestions("/dep") {
		if s.Name == "/deploy" {
			rows++
			if !strings.Contains(s.Desc, "wins") {
				t.Fatalf("advertised row must describe the skill dispatch runs, got %q", s.Desc)
			}
		}
	}
	if rows != 1 {
		t.Fatalf("case-colliding skills must yield exactly one /deploy row, got %d", rows)
	}
}

// The palette reads through the loader, so a skill installed mid-session (the
// loader's next result) appears without a restart.
func TestSkillPaletteReadsThroughLoader(t *testing.T) {
	installed := []skills.Skill{}
	m := newModel(context.Background(), Options{
		Cwd:        t.TempDir(),
		LoadSkills: func() []skills.Skill { return installed },
	})
	if got := m.matchSkillSuggestions("/lat"); len(got) != 0 {
		t.Fatalf("no skills installed yet, got %#v", got)
	}
	installed = append(installed, skills.Skill{Name: "late-arrival", Description: "installed mid-session", Content: "x"})
	got := m.matchSkillSuggestions("/lat")
	if len(got) != 1 || got[0].Name != "/late-arrival" {
		t.Fatalf("mid-session skill should appear via the loader, got %#v", got)
	}
}
