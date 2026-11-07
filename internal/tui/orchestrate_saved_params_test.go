package tui

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/specialist"
)

// A parameterised plan is refused HERE, before a turn is spent.
//
// expandPlanParams refuses it too, but only at admission — after the command has
// dispatched a prompt, the model has composed an orchestrate call and the run has
// begun. The bundled research plan is five child agents, so the difference is
// five sub-agent runs against a placeholder.
func writeParamPlan(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func mustPlanArgs(t *testing.T, body string) map[string]any {
	t.Helper()
	var args map[string]any
	if err := json.Unmarshal([]byte(body), &args); err != nil {
		t.Fatal(err)
	}
	return args
}

const oneParamPlan = `{"name":"research","tasks":[{"id":"a","prompt":"Find where ${subject} is defined"}],"budget":{"max_workers":1}}`

func TestRunningAParameterisedPlanWithNoValueRefusesBeforeStartingATurn(t *testing.T) {
	m, paths := savedPlanModel(t)
	writeParamPlan(t, paths.ProjectDir, "research", oneParamPlan)

	updated, cmd := m.runSavedPlan("research", false)
	if cmd != nil {
		t.Fatal("a plan missing its parameter started a turn: the whole point is that it costs nothing")
	}
	rendered := transcriptText(updated.(model).transcript)
	if !strings.Contains(rendered, "status: warning") {
		t.Fatalf("must refuse: %s", rendered)
	}
	// It has to name the parameter AND show the syntax — a refusal the user
	// cannot act on just moves the dead end earlier.
	if !strings.Contains(rendered, "subject") {
		t.Fatalf("the refusal must name the parameter: %s", rendered)
	}
	if !strings.Contains(rendered, "/plans run research <subject>") {
		t.Fatalf("the refusal must show how to supply it: %s", rendered)
	}
}

// The other direction: words handed to a plan that takes none must be refused,
// not dropped. A user who typed them meant them.
func TestRunningAPlainPlanWithExtraWordsIsRefusedRatherThanIgnored(t *testing.T) {
	m, paths := savedPlanModel(t)
	writeParamPlan(t, paths.ProjectDir, "sweep",
		`{"name":"sweep","tasks":[{"id":"a","prompt":"look around"}],"budget":{"max_workers":1}}`)

	updated, cmd := m.runSavedPlan("sweep the retry watchdog", false)
	if cmd != nil {
		t.Fatal("extra words were silently dropped and the plan ran anyway")
	}
	rendered := transcriptText(updated.(model).transcript)
	if !strings.Contains(rendered, "takes no parameters") {
		t.Fatalf("must say why: %s", rendered)
	}
}

// A single parameter is resolved by the COMMAND, not by the model: everything
// after the name is the value, which is a total mapping, so the substituted plan
// is a function of what was typed rather than of what a model inferred.
func TestASingleParameterIsResolvedFromTheCommandLine(t *testing.T) {
	stored := specialist.SavedPlan{Name: "research", Args: mustPlanArgs(t, oneParamPlan)}

	arg, err := savedPlanParamsArg(stored, "the retry watchdog", "run")
	if err != nil {
		t.Fatalf("supplying the parameter must succeed: %v", err)
	}
	if !strings.Contains(arg, `{"subject":"the retry watchdog"}`) {
		t.Fatalf("the params object must carry the typed value verbatim: %s", arg)
	}
}

// A percent sign in the subject must survive. The instruction is built by
// concatenation for exactly this reason: a value interpolated through Sprintf
// comes out as "%!s(MISSING)" and the plan researches a formatting artefact.
func TestAParameterContainingAPercentSignSurvivesIntoTheInstruction(t *testing.T) {
	m, paths := savedPlanModel(t)
	writeParamPlan(t, paths.ProjectDir, "research", oneParamPlan)

	updated, _ := m.runSavedPlan("research why cache hit rate dropped 40% today", false)
	rendered := transcriptText(updated.(model).transcript)
	if strings.Contains(rendered, "MISSING") || strings.Contains(rendered, "%!") {
		t.Fatalf("the percent sign was interpreted as a verb: %s", rendered)
	}
	// ASSERTED POSITIVELY. "no %!" is satisfied by an instruction that dropped the
	// parameter altogether, which is the failure this is supposed to catch.
	if !strings.Contains(rendered, `{"subject":"why cache hit rate dropped 40% today"}`) {
		t.Fatalf("the subject did not reach the instruction verbatim: %s", rendered)
	}
	// And the plan is still named, so the params did not displace the reference.
	if !strings.Contains(rendered, `"research"`) {
		t.Fatalf("the instruction no longer names the plan: %s", rendered)
	}
}

// Several parameters have no total mapping from one line, so the model splits it
// — and is told the exact key set so it fills those and invents none.
func TestSeveralParametersNameTheExactKeySet(t *testing.T) {
	stored := specialist.SavedPlan{Name: "compare", Args: mustPlanArgs(t,
		`{"name":"compare","tasks":[{"id":"a","prompt":"compare ${before} against ${after}"}],"budget":{"max_workers":1}}`)}

	arg, err := savedPlanParamsArg(stored, "v1 and v2", "run")
	if err != nil {
		t.Fatalf("supplying text for several parameters must succeed: %v", err)
	}
	for _, want := range []string{`"after"`, `"before"`, "v1 and v2"} {
		if !strings.Contains(arg, want) {
			t.Fatalf("instruction must carry %s: %s", want, arg)
		}
	}
}

// A plan name is compared exactly, so the split that separates it from its
// parameters must not lowercase it the way the verb split does.
func TestAPlanNameKeepsItsCaseWhenSplitFromItsParameters(t *testing.T) {
	name, rest := splitPlanTarget("  Sweep   the retry watchdog  ")
	if name != "Sweep" {
		t.Fatalf("name was rewritten to %q: /plans run Sweep would report no such plan", name)
	}
	if rest != "the retry watchdog" {
		t.Fatalf("parameter text was %q", rest)
	}
}
