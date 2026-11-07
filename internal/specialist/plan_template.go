package specialist

import (
	"fmt"
	"sort"
	"strings"
)

// Plan shapes a model can ask for by name instead of composing from scratch.
//
// THE FAILURE THIS REMOVES. The model deciding to run a plan is not the problem
// — system_prompt.go already tells it when to delegate, and a measured run
// spawned a background sub-agent with the words "sub-agent", "spawn" and
// "parallel" appearing nowhere in the prompt. The problem is what happens next:
// it emits plan JSON that admission refuses, and a turn and a model call are
// spent learning a rule. A real run emitted budget.max_tokens_per_task: 5000 and
// was refused for sitting below the floor.
//
// SO THIS IS A GENERATOR, NOT A CLASSIFIER. Nothing here decides WHETHER to
// plan. It only turns "audit this" into a task graph that admission accepts.
//
// DETERMINISTIC, AND THAT IS THE MULTI-PROVIDER ARGUMENT. An LLM generator needs
// reliable structured output, which across this catalogue means JSON mode on
// some providers, forced tool-use on others and prose-with-fences on the rest —
// twenty-one of its entries are OpenAI-compatible gateways whose capabilities
// are whatever the upstream vendor supports that day. A template needs nothing
// from the provider, so it behaves identically on all of them, including a local
// Ollama that reports no capabilities at all.
//
// EVERY TEMPLATE IS RE-VALIDATED THROUGH ParsePlan before it runs. These emit
// the same args a model would; they are not a second admission path.

// PlanTemplate is one named shape.
type PlanTemplate struct {
	// Name is what a caller asks for.
	Name string
	// WhenToUse is one line, for the tool description. Templates a model cannot
	// tell apart are templates it picks at random.
	WhenToUse string
	// Params are the values it needs, in the order a caller would give them.
	Params []string
	// build emits orchestrate arguments. Never called with a missing param —
	// BuildTemplatePlan checks first, so this cannot silently produce a plan
	// with an empty subject.
	build func(params map[string]string) map[string]any
}

// planTemplates are the shapes that recur. DELIBERATELY FEW: a template exists
// here when its task graph is genuinely reusable, and inventing one per phrasing
// would leave a list nobody can choose from.
var planTemplates = []PlanTemplate{
	{
		Name:      "audit",
		WhenToUse: "Examine one subject from several independent angles, then verify the findings before reporting them.",
		Params:    []string{"subject"},
		build: func(params map[string]string) map[string]any {
			subject := params["subject"]
			return map[string]any{
				"name":        "audit",
				"description": "Hostile audit of " + subject,
				"tasks": []any{
					templateTask("correctness", "find", "Find CORRECTNESS defects in "+subject+
						". Report each as file:line, what is wrong, and a concrete reproduction. Prove it from the code; do not report what you suspect.", nil),
					templateTask("safety", "find", "Find SAFETY and authority defects in "+subject+
						": missing checks, trust placed in untrusted input, anything that fails open. Report each as file:line with the path that reaches it.", nil),
					templateTask("tests", "find", "Find what TESTS "+subject+
						", and name any test that asserts nothing meaningful. A behaviour with no test is itself a finding.", nil),
					templateTask("refute", "verify", "Try to REFUTE every finding above. Default to refuted when uncertain. "+
						"For each: open the cited file:line and check the claim is what the code actually does. Report VERIFIED with the line that proves it, or REFUTED with the line that disproves it.",
						[]string{"correctness", "safety", "tests"}),
					templateTask("report", "answer", "Report the findings that SURVIVED refutation, most severe first, each with its file:line and reproduction. "+
						"Say plainly how many were refuted and dropped — a reader who is not told the count cannot tell a clean audit from a shallow one.",
						[]string{"refute"}),
				},
				"budget": map[string]any{"max_workers": float64(3)},
			}
		},
	},
	{
		Name:      "compare",
		WhenToUse: "Establish how two things differ, by examining each on its own before comparing them.",
		Params:    []string{"before", "after"},
		build: func(params map[string]string) map[string]any {
			before, after := params["before"], params["after"]
			return map[string]any{
				"name":        "compare",
				"description": "Compare " + before + " against " + after,
				"tasks": []any{
					// EACH SIDE EXAMINED BLIND. A single task told to compare
					// reads one side, forms a view, and reads the second looking
					// for confirmation of it.
					templateTask("left", "examine", "Describe "+before+" on its own terms: what it does, how, and with what limits. Cite file:line. Do NOT compare it to anything.", nil),
					templateTask("right", "examine", "Describe "+after+" on its own terms: what it does, how, and with what limits. Cite file:line. Do NOT compare it to anything.", nil),
					templateTask("diff", "answer", "Using the two descriptions above, state how "+before+" and "+after+
						" differ, and where they agree. Where the two descriptions conflict, say which you believe and open the code to settle it — do not average them.",
						[]string{"left", "right"}),
				},
				"budget": map[string]any{"max_workers": float64(2)},
			}
		},
	},
	{
		Name:      "sweep",
		WhenToUse: "Ask the same question of several targets independently, then combine the answers.",
		Params:    []string{"question", "targets"},
		build: func(params map[string]string) map[string]any {
			// A VARIABLE TASK COUNT is the shape a model most often gets wrong by
			// hand: it has to invent one id per target, keep them unique, and
			// list every one of them in the synthesis task's depends_on. Getting
			// any of that wrong is a refusal at admission.
			targets := splitTemplateList(params["targets"])
			tasks := make([]any, 0, len(targets)+1)
			ids := make([]string, 0, len(targets))
			for index, target := range targets {
				id := templateTaskID("t", index, target)
				ids = append(ids, id)
				tasks = append(tasks, templateTask(id, "examine",
					params["question"]+"\n\nAnswer this for exactly one target: "+target+
						". Cite file:line. If the answer is 'this does not apply here', say so plainly rather than stretching for one.", nil))
			}
			tasks = append(tasks, templateTask("combine", "answer",
				"Answer the question across every target above: "+params["question"]+
					"\n\nState what holds everywhere, what holds only somewhere — naming which — and what contradicts. A target whose answer was empty is itself a result; do not drop it.",
				ids))
			return map[string]any{
				"name":        "sweep",
				"description": params["question"],
				"tasks":       tasks,
				"budget":      map[string]any{"max_workers": float64(templateWorkers(len(targets)))},
			}
		},
	},
}

// PlanTemplates lists the shapes, sorted, for a tool description.
func PlanTemplates() []PlanTemplate {
	out := append([]PlanTemplate(nil), planTemplates...)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// BuildTemplatePlan emits the orchestrate arguments for one template.
//
// FAILS CLOSED IN BOTH DIRECTIONS, exactly as expandPlanParams does for saved
// plans: a missing value is refused rather than substituted as empty prose, and
// a value the template does not take is refused rather than dropped — a caller
// who supplied it meant it, and ignoring it runs a different plan than the one
// asked for while reporting success.
func BuildTemplatePlan(name string, params map[string]string) (map[string]any, error) {
	template, found := findPlanTemplate(name)
	if !found {
		return nil, fmt.Errorf("no plan template named %q; the templates are %s",
			name, strings.Join(planTemplateNames(), ", "))
	}
	var missing []string
	for _, param := range template.Params {
		if strings.TrimSpace(params[param]) == "" {
			missing = append(missing, param)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("the %q template needs %s: %s",
			name, pluralParams(missing), strings.Join(quoteAll(missing), ", "))
	}
	known := map[string]bool{}
	for _, param := range template.Params {
		known[param] = true
	}
	var unused []string
	for supplied := range params {
		if !known[supplied] {
			unused = append(unused, supplied)
		}
	}
	if len(unused) > 0 {
		sort.Strings(unused)
		return nil, fmt.Errorf("the %q template takes no %s: %s (it takes %s)",
			name, pluralParams(unused), strings.Join(quoteAll(unused), ", "),
			strings.Join(quoteAll(template.Params), ", "))
	}
	if template.Name == "sweep" && len(splitTemplateList(params["targets"])) == 0 {
		return nil, fmt.Errorf("the %q template needs at least one non-empty target", name)
	}
	return template.build(params), nil
}

func findPlanTemplate(name string) (PlanTemplate, bool) {
	wanted := strings.ToLower(strings.TrimSpace(name))
	for _, template := range planTemplates {
		if template.Name == wanted {
			return template, true
		}
	}
	return PlanTemplate{}, false
}

func planTemplateNames() []string {
	out := make([]string, 0, len(planTemplates))
	for _, template := range PlanTemplates() {
		out = append(out, template.Name)
	}
	return out
}

func templateTask(id, phase, prompt string, dependsOn []string) map[string]any {
	task := map[string]any{"id": id, "phase": phase, "prompt": prompt}
	if len(dependsOn) > 0 {
		deps := make([]any, 0, len(dependsOn))
		for _, dep := range dependsOn {
			deps = append(deps, dep)
		}
		task["depends_on"] = deps
	}
	return task
}

// templateTaskID builds an id from a target that is ALREADY UNIQUE by its index,
// so two targets that sanitise to the same string cannot collide — a collision
// would silently drop one target's task and its dependency edge.
func templateTaskID(prefix string, index int, target string) string {
	var cleaned strings.Builder
	for _, r := range strings.ToLower(target) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			cleaned.WriteRune(r)
		case r == '_' || r == '-':
			cleaned.WriteRune('_')
		}
		if cleaned.Len() >= 20 {
			break
		}
	}
	id := fmt.Sprintf("%s%d", prefix, index+1)
	if cleaned.Len() > 0 {
		id += "_" + cleaned.String()
	}
	return id
}

// splitTemplateList reads a comma-separated list, dropping blanks so a trailing
// comma does not become a task with no target.
func splitTemplateList(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// templateWorkers keeps a generated plan inside the range max_workers accepts,
// so a sweep over thirty targets is refused by nothing. Never below 1: a plan
// with zero workers cannot run and the schema forbids it.
func templateWorkers(targets int) int {
	switch {
	case targets < 1:
		return 1
	case targets > maxPlanWorkers:
		return maxPlanWorkers
	default:
		return targets
	}
}
