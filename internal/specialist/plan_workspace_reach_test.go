package specialist

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// THE MEASURED FAILURE, REPRODUCED. A six-task plan started from a directory
// that was a git repo with one tracked file; one task asked for exec_command, so
// every task was moved into a worktree of that directory; the five reading a
// different tree were refused by the sandbox and three died reporting "I
// cannot". This is that shape, in miniature.
func reachPlan(t *testing.T, prompt string, tools []string) Plan {
	t.Helper()
	anyTools := make([]any, 0, len(tools))
	for _, name := range tools {
		anyTools = append(anyTools, name)
	}
	return mustParsePlan(t, map[string]any{
		"name": "swarm-session-id-leak",
		"tasks": []any{
			map[string]any{"id": "finder", "prompt": prompt, "tools": anyTools},
			map[string]any{"id": "runner", "prompt": "run the tests", "tools": []any{"read_file", "exec_command"}},
		},
		"budget": map[string]any{"max_workers": float64(1)},
	}, Limits{MaxTasks: 20, ParentTools: []string{"read_file", "grep", "glob", "exec_command"}})
}

func isolatedWorkspace(path string) PlanWorkspace {
	return PlanWorkspace{Path: path, Isolated: true, Describe: "worktree", Release: func() {}}
}

func TestAPlanIsRefusedWhenItsWorktreeCannotReachThePathsItNames(t *testing.T) {
	// The tree the tasks were pointed at.
	realTree := t.TempDir()
	target := filepath.Join(realTree, "internal", "swarm", "tools.go")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("package swarm\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The worktree the plan actually got: a different tree, with one file.
	worktree := t.TempDir()
	if err := os.WriteFile(filepath.Join(worktree, "gauntlet_test.txt"), []byte("test"), 0o600); err != nil {
		t.Fatal(err)
	}

	plan := reachPlan(t, "Read "+target+" and report every render site.", []string{"read_file", "grep"})
	unreachable := unreachablePlanPaths(plan, isolatedWorkspace(worktree))
	if len(unreachable) == 0 {
		t.Fatal("a plan naming a path outside its own worktree was allowed to dispatch")
	}
	if unreachable[0] != target {
		t.Fatalf("named %q, want %q", unreachable[0], target)
	}

	// The refusal must be ACTIONABLE: name the path, name why it isolated, and
	// name both ways out.
	err := refuseUnreachableWorkspace(plan, isolatedWorkspace(worktree), unreachable)
	text := err.Error()
	for _, want := range []string{target, worktree, "exec_command", "runner", "read-only"} {
		if !strings.Contains(text, want) {
			t.Errorf("the refusal never mentions %q:\n%s", want, text)
		}
	}
}

// FAILS TOWARD RUNNING THE PLAN. Each of these must NOT fire — a false refusal
// blocks work that would have succeeded.
func TestTheReachGuardStaysQuietWithoutUnambiguousEvidence(t *testing.T) {
	worktree := t.TempDir()
	// A real file in a real tree that is NOT the worktree, so the only thing
	// keeping the guard quiet in the non-isolated cases is the isolation check.
	elsewhere := t.TempDir()
	outsideAndReal := filepath.Join(elsewhere, "outside.go")
	if err := os.WriteFile(outsideAndReal, []byte("package outside\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(worktree, "internal", "app.go")
	if err := os.MkdirAll(filepath.Dir(inside), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(inside, []byte("package app\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name      string
		prompt    string
		workspace PlanWorkspace
	}{
		{
			// The overwhelmingly common case: no isolation, so the parent's tree
			// is the workspace and nothing was ever relocated.
			//
			// THE PATH MUST EXIST AND BE OUTSIDE, or this case proves nothing: an
			// earlier version pointed at a path that was not there, so the
			// existence check filtered it and the isolation check was never
			// reached — a mutation removing that check passed cleanly.
			name:      "a plan that never isolated",
			prompt:    "Read " + outsideAndReal + " for context.",
			workspace: PlanWorkspace{},
		},
		{
			// Same, with a path recorded but isolation false: only the Isolated
			// flag stands between this and a refusal.
			name:      "a workspace that is named but not isolated",
			prompt:    "Read " + outsideAndReal + " for context.",
			workspace: PlanWorkspace{Path: worktree, Isolated: false},
		},
		{
			name:      "a path inside the worktree",
			prompt:    "Read " + inside + " carefully.",
			workspace: isolatedWorkspace(worktree),
		},
		{
			// Somebody else's error. A path that is not there would fail for its
			// own reasons and this guard must not claim it.
			name:      "a path that does not exist",
			prompt:    "Read /Users/nobody/dev/ghost/internal/thing.go",
			workspace: isolatedWorkspace(worktree),
		},
		{
			// Prose is full of slash-shaped things that are not paths.
			name:      "a URL path and an endpoint",
			prompt:    "See https://ollama.com/settings and the /v1/pay endpoint, then read app.go",
			workspace: isolatedWorkspace(worktree),
		},
		{
			name:      "relative paths resolve against the workspace",
			prompt:    "Read internal/app.go and internal/swarm/tools.go",
			workspace: isolatedWorkspace(worktree),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan := reachPlan(t, tc.prompt, []string{"read_file", "grep"})
			if got := unreachablePlanPaths(plan, tc.workspace); len(got) > 0 {
				t.Fatalf("refused a plan that would have worked: %v", got)
			}
		})
	}
}

// A path at the end of a sentence arrives with punctuation attached; missing it
// is safe but cheap to avoid.
func TestAPathFollowedByPunctuationIsStillFound(t *testing.T) {
	realTree := t.TempDir()
	target := filepath.Join(realTree, "streamer.go")
	if err := os.WriteFile(target, []byte("package x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	worktree := t.TempDir()

	for _, suffix := range []string{".", ",", ")", "'", `"`, ";"} {
		plan := reachPlan(t, "Open "+target+suffix+" and report.", []string{"read_file"})
		got := unreachablePlanPaths(plan, isolatedWorkspace(worktree))
		if len(got) != 1 || got[0] != target {
			t.Errorf("suffix %q: got %v, want [%s]", suffix, got, target)
		}
	}
}

// "/a/b" does not contain "/a/bc". A bare prefix check gets this wrong and would
// wave through a plan pointed at a sibling directory.
func TestContainmentIsSeparatorAware(t *testing.T) {
	for _, tc := range []struct {
		root, path string
		want       bool
	}{
		{"/a/b", "/a/b", true},
		{"/a/b", "/a/b/c", true},
		{"/a/b", "/a/bc", false},
		{"/a/b", "/a/bc/d", false},
		{"/a/b/", "/a/b/c", true},
		{"", "/a", false},
		{"/a", "", false},
	} {
		if got := pathInsideRoot(tc.root, tc.path); got != tc.want {
			t.Errorf("pathInsideRoot(%q, %q) = %v, want %v", tc.root, tc.path, got, tc.want)
		}
	}
}

// The refusal must name the tool that caused isolation, because one task's grant
// silently decides the workspace for every other task — the surprising part.
func TestTheTriggeringTaskAndToolAreNamed(t *testing.T) {
	plan := reachPlan(t, "read something", []string{"read_file", "grep"})
	taskID, tool := planIsolationTrigger(plan)
	if taskID != "runner" || tool != "exec_command" {
		t.Fatalf("trigger = (%q, %q), want (\"runner\", \"exec_command\")", taskID, tool)
	}

	// A wholly read-only plan has no trigger to name.
	readOnly := mustParsePlan(t, map[string]any{
		"name":   "p",
		"tasks":  []any{map[string]any{"id": "a", "prompt": "look", "tools": []any{"read_file"}}},
		"budget": map[string]any{"max_workers": float64(1)},
	}, Limits{MaxTasks: 20, ParentTools: PlanReadOnlyToolNames()})
	if id, name := planIsolationTrigger(readOnly); id != "" || name != "" {
		t.Fatalf("a read-only plan reported a trigger (%q, %q)", id, name)
	}
}

// END TO END through resolvePlanWorkspace: the refusal must arrive from the one
// function both call sites use, and the worktree must not be left on disk.
func TestResolvePlanWorkspaceRefusesAndAbortsTheWorktree(t *testing.T) {
	realTree := t.TempDir()
	target := filepath.Join(realTree, "tools.go")
	if err := os.WriteFile(target, []byte("package x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	worktree := t.TempDir()

	released, aborted := false, false
	isolate := func(context.Context, string) (PlanWorkspace, error) {
		return PlanWorkspace{
			Path: worktree, Isolated: true, Describe: "worktree",
			Release: func() { released = true },
			Abort:   func() { aborted = true },
		}, nil
	}
	plan := reachPlan(t, "Read "+target+" and report.", []string{"read_file"})

	got, err := resolvePlanWorkspace(context.Background(), plan, isolate)
	if err == nil {
		t.Fatal("the plan was allowed to run in a worktree it cannot read from")
	}
	if got.Path != "" {
		t.Fatalf("a refused resolution still handed back a workspace: %+v", got)
	}
	if released {
		t.Fatal("admission failure used successful-retention cleanup")
	}
	if !aborted {
		t.Fatal("the aborted worktree lease was not released: one locked directory per attempt")
	}
	if !strings.Contains(err.Error(), target) {
		t.Fatalf("the refusal does not name the unreachable path: %v", err)
	}
}

// A plan whose worktree DOES serve it must still run, and must keep its
// workspace — the guard is a check, not a new refusal path for ordinary plans.
func TestAWorktreeThatServesThePlanIsStillAccepted(t *testing.T) {
	worktree := t.TempDir()
	inside := filepath.Join(worktree, "tools.go")
	if err := os.WriteFile(inside, []byte("package x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	isolate := func(context.Context, string) (PlanWorkspace, error) {
		return PlanWorkspace{Path: worktree, Isolated: true, Describe: "worktree", Release: func() {}}, nil
	}
	plan := reachPlan(t, "Read "+inside+" and report.", []string{"read_file"})

	got, err := resolvePlanWorkspace(context.Background(), plan, isolate)
	if err != nil {
		t.Fatalf("a usable worktree was refused: %v", err)
	}
	if got.Path != worktree || !got.Isolated {
		t.Fatalf("workspace = %+v, want the worktree", got)
	}
}

// The pre-existing refusals must survive: this adds a check, it does not replace
// the ones that were already there.
func TestTheExistingWorkspaceRefusalsStillFire(t *testing.T) {
	writePlan := reachPlan(t, "read something", []string{"read_file"})

	if _, err := resolvePlanWorkspace(context.Background(), writePlan, nil); err == nil {
		t.Fatal("a write-capable plan ran with no isolator")
	}
	dishonest := func(context.Context, string) (PlanWorkspace, error) {
		return PlanWorkspace{Path: "", Isolated: false}, nil
	}
	if _, err := resolvePlanWorkspace(context.Background(), writePlan, dishonest); err == nil {
		t.Fatal("an isolator returning no isolation was believed")
	}
	failing := func(context.Context, string) (PlanWorkspace, error) {
		return PlanWorkspace{}, errors.New("no git here")
	}
	if _, err := resolvePlanWorkspace(context.Background(), writePlan, failing); err == nil {
		t.Fatal("a failed isolator was ignored")
	}
}

// THE 04:48 RUN, REPRODUCED. Same plan as the earlier failure, but its targets
// were named RELATIVELY — "internal/specialist/", "internal/swarm/" — plus a
// commit sha. Not one absolute path, so the check above found nothing and six
// tasks were dispatched into a worktree holding one file.
func bareWorktree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "gauntlet_test.txt"), []byte("test"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A worktree always has a .git entry; it must not count as content.
	if err := os.WriteFile(filepath.Join(dir, ".git"), []byte("gitdir: /elsewhere\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestAPlanIsRefusedWhenABareWorktreeHoldsNothingItNames(t *testing.T) {
	worktree := bareWorktree(t)
	plan := reachPlan(t,
		"Trace session_id emission. Read internal/specialist/streamer.go and internal/swarm/launcher_specialist.go, then check commit 5400fa46.",
		[]string{"read_file", "grep", "glob"})

	named, bare := planNamesNothingPresent(plan, isolatedWorkspace(worktree))
	if !bare {
		t.Fatal("a plan naming only paths absent from a bare worktree was allowed to dispatch")
	}
	if len(named) < 2 {
		t.Fatalf("expected the paths it named, got %v", named)
	}
	err := refuseBareWorkspace(plan, isolatedWorkspace(worktree), named)
	for _, want := range []string{"internal/specialist/streamer.go", worktree, "exec_command", "read-only"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal never mentions %q:\n%s", want, err.Error())
		}
	}
}

// THE CONJUNCTION IS THE SAFETY. Each of these breaks exactly one condition and
// must therefore NOT refuse — a relative path is weak evidence on its own.
func TestTheBareWorkspaceGuardNeedsEveryConditionAtOnce(t *testing.T) {
	populated := t.TempDir()
	for _, rel := range []string{"internal/specialist/streamer.go", "cmd/zero/main.go", "README.md", "go.mod"} {
		full := filepath.Join(populated, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	bare := bareWorktree(t)

	// Bare by entry count — one visible directory — yet holding a file the plan
	// names. Only the resolving-path clause keeps the guard quiet here.
	bareButServing := t.TempDir()
	served := filepath.Join(bareButServing, "src", "app.go")
	if err := os.MkdirAll(filepath.Dir(served), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(served, []byte("package app"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !workspaceIsBare(bareButServing) {
		t.Fatal("setup: this tree must read as bare or the case proves nothing")
	}

	for _, tc := range []struct {
		name      string
		prompt    string
		workspace PlanWorkspace
	}{
		{
			// A scaffolding plan: the files do not exist YET, but the worktree is
			// a real checkout. This is the false refusal most worth avoiding.
			name:      "new files in a populated worktree",
			prompt:    "Create internal/brand/new.go and internal/brand/new_test.go.",
			workspace: isolatedWorkspace(populated),
		},
		{
			// One token resolves, so the worktree IS serving the plan.
			//
			// THE TREE MUST BE BARE HERE, or this case proves nothing: pointed at
			// a populated tree it was excluded by the bareness check before the
			// resolving-path clause was reached, and a mutation deleting that
			// clause passed cleanly. Bare AND one path present is the only shape
			// that exercises it.
			name:      "one named path is present in an otherwise bare tree",
			prompt:    "Read src/app.go and internal/ghost/absent.go.",
			workspace: isolatedWorkspace(bareButServing),
		},
		{
			name:      "every named path is present",
			prompt:    "Read internal/specialist/streamer.go and cmd/zero/main.go.",
			workspace: isolatedWorkspace(populated),
		},
		{
			name:      "only one path-shaped token",
			prompt:    "Read internal/specialist/streamer.go and report.",
			workspace: isolatedWorkspace(bare),
		},
		{
			name:      "the plan never isolated",
			prompt:    "Read internal/specialist/streamer.go and internal/swarm/tools.go.",
			workspace: PlanWorkspace{Path: bare, Isolated: false},
		},
		{
			// Prose with slashes is not a path list.
			name:      "and/or, read/write, input/output",
			prompt:    "Decide and/or report, covering read/write and input/output behaviour.",
			workspace: isolatedWorkspace(bare),
		},
		{
			// A URL must not contribute its path.
			name:      "a URL and a host path",
			prompt:    "See https://ollama.com/settings/keys and https://github.com/Twigpine/zero/pull/829.",
			workspace: isolatedWorkspace(bare),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan := reachPlan(t, tc.prompt, []string{"read_file", "grep"})
			if named, refused := planNamesNothingPresent(plan, tc.workspace); refused {
				t.Fatalf("refused a plan that would have worked, on %v", named)
			}
		})
	}
}

// The token rule itself, since it is what keeps prose out.
func TestOnlyPathShapedTokensCount(t *testing.T) {
	for token, want := range map[string]bool{
		"internal/specialist/":    true,
		"internal/swarm/tools.go": true,
		"a/b/c":                   true,
		"main.go":                 false, // no slash at all
		"and/or":                  false,
		"read/write":              false,
		"input/output":            false,
		"cmd/zero":                false, // two bare segments: safe to miss
		"internal/app.go":         true,
		"":                        false,
		"/absolute/path.go":       false, // the other check owns absolutes
	} {
		if got := pathShapedToken(token); got != want {
			t.Errorf("pathShapedToken(%q) = %v, want %v", token, got, want)
		}
	}
}

// A worktree's .git must not make it look occupied, and a real checkout must
// never read as bare.
func TestBarenessIgnoresDotEntries(t *testing.T) {
	if !workspaceIsBare(bareWorktree(t)) {
		t.Fatal("a worktree with one file and .git did not read as bare")
	}
	populated := t.TempDir()
	for _, name := range []string{"a", "b", "c", "d"} {
		if err := os.WriteFile(filepath.Join(populated, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if workspaceIsBare(populated) {
		t.Fatal("a populated tree read as bare")
	}
	// UNREADABLE IS NOT BARE: a filesystem error must not become a refusal.
	if workspaceIsBare(filepath.Join(t.TempDir(), "nope")) {
		t.Fatal("an unreadable directory read as bare")
	}
}

// END TO END through the one function both call sites use, and the worktree must
// not be left behind.
func TestResolvePlanWorkspaceRefusesABareWorktree(t *testing.T) {
	worktree := bareWorktree(t)
	released := false
	isolate := func(context.Context, string) (PlanWorkspace, error) {
		return PlanWorkspace{
			Path: worktree, Isolated: true, Describe: "worktree",
			Release: func() { released = true },
		}, nil
	}
	plan := reachPlan(t,
		"Read internal/specialist/streamer.go and internal/swarm/tools.go and report.",
		[]string{"read_file", "grep"})

	if _, err := resolvePlanWorkspace(context.Background(), plan, isolate); err == nil {
		t.Fatal("the plan was allowed to search an empty worktree")
	} else if !strings.Contains(err.Error(), "internal/specialist/streamer.go") {
		t.Fatalf("the refusal does not name what it could not find: %v", err)
	}
	if !released {
		t.Fatal("the worktree was left on disk")
	}
}

// `name` IS OPTIONAL ON THE ORCHESTRATE TOOL, so a refusal that interpolated
// Plan.Name() rendered `plan "" would run in an isolated worktree` — naming
// nothing, and reading like a fault in the reader's own plan. Seen verbatim.
func TestAnUnnamedPlanIsStillDescribedInTheRefusal(t *testing.T) {
	worktree := bareWorktree(t)
	unnamed := mustParsePlan(t, map[string]any{
		"tasks": []any{
			map[string]any{"id": "a", "prompt": "read internal/specialist/streamer.go and internal/swarm/tools.go", "tools": []any{"read_file"}},
			map[string]any{"id": "b", "prompt": "run it", "tools": []any{"read_file", "exec_command"}},
		},
		"budget": map[string]any{"max_workers": float64(1)},
	}, Limits{MaxTasks: 20, ParentTools: []string{"read_file", "exec_command"}})

	if unnamed.Name() != "" {
		t.Skip("plans now require a name; this case cannot arise")
	}
	named, bare := planNamesNothingPresent(unnamed, isolatedWorkspace(worktree))
	if !bare {
		t.Fatal("setup: the guard did not fire, so there is no message to check")
	}
	text := refuseBareWorkspace(unnamed, isolatedWorkspace(worktree), named).Error()
	if strings.Contains(text, `plan ""`) {
		t.Fatalf("the refusal names nothing:\n%s", text)
	}
	if !strings.Contains(text, "unnamed") || !strings.Contains(text, "2-task") {
		t.Fatalf("an unnamed plan is not described by what it is:\n%s", text)
	}
	// And a named plan still says its name.
	namedPlan := reachPlan(t, "read internal/specialist/streamer.go and internal/swarm/tools.go", []string{"read_file"})
	got, _ := planNamesNothingPresent(namedPlan, isolatedWorkspace(worktree))
	if text := refuseBareWorkspace(namedPlan, isolatedWorkspace(worktree), got).Error(); !strings.Contains(text, `plan "swarm-session-id-leak"`) {
		t.Fatalf("a named plan lost its name:\n%s", text)
	}
}

// AN EXACT HIT CAN BE A WINDOWS QUIRK. NTFS ignores trailing dots, so
// Lstat("streamer.go.") succeeds for "streamer.go" and the sentence's full stop
// came back as part of the path — the Windows CI failure this closes. The
// same-file preference must NOT rewrite a POSIX file genuinely named with a
// trailing dot, which these cases pin.
func TestExistingPathPrefersTheCanonicalSpelling(t *testing.T) {
	if runtime.GOOS == "windows" {
		// The dotted names below are not creatable on NTFS; the quirk itself is
		// covered by TestAPathFollowedByPunctuationIsStillFound on the Windows
		// runner.
		t.Skip("trailing-dot filenames are not creatable on Windows")
	}
	dir := t.TempDir()
	plain := filepath.Join(dir, "plain.go")
	if err := os.WriteFile(plain, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A genuinely dot-suffixed file, alone: its exact spelling must survive.
	dottedOnly := filepath.Join(dir, "only.")
	if err := os.WriteFile(dottedOnly, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, ok := existingPath(dottedOnly); !ok || got != dottedOnly {
		t.Fatalf("a real dotted filename was rewritten: got %q ok=%v", got, ok)
	}
	// Both spellings exist as DIFFERENT files: the exact one wins.
	pairBase := filepath.Join(dir, "pair")
	pairDotted := pairBase + "."
	for _, path := range []string{pairBase, pairDotted} {
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if got, ok := existingPath(pairDotted); !ok || got != pairDotted {
		t.Fatalf("a distinct dotted sibling was rewritten: got %q ok=%v", got, ok)
	}
	// The missing-with-punctuation case keeps its trim.
	if got, ok := existingPath(plain + "."); !ok || got != plain {
		t.Fatalf("trailing punctuation must still trim to the real file: got %q ok=%v", got, ok)
	}
}
