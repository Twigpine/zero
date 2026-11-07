package specialist

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// Model-based routing: ask the provider's strongest model which model should run
// each task.
//
// WHY THE KEYWORD CLASSIFIER IS NOT ENOUGH. classifyTaskRole matches verbs —
// "searching" is cheap, "auditing" is strong — and a verb is a proxy for
// difficulty, not a measurement of it. A hard architectural question phrased as
// "find every place X happens" is billed as a scan; a trivial "review this typo"
// gets the flagship. The proxy was worth having because it costs nothing. It is
// still a proxy.
//
// A model reading the actual task text can judge what the work needs, which is
// the thing the verb was standing in for. It costs one extra call on an
// expensive model before the plan starts, which is why this is OPT-IN and why a
// plan too small to benefit skips it.
//
// EVERY FAILURE FALLS BACK TO THE CLASSIFIER. A router that errors, times out,
// returns malformed JSON, names a task that does not exist or a model the
// provider does not serve must never be able to stop a plan from running: the
// worst outcome is the routing we already had.

// routerMinimumTasks is the plan size below which routing is not worth its own
// call. Two tasks cannot differ enough in difficulty to justify spending a
// frontier-model round trip deciding between them.
const routerMinimumTasks = 3

// routedAssignment is one task-to-model decision from the router.
type routedAssignment struct {
	ID     string `json:"id"`
	Model  string `json:"model"`
	Reason string `json:"reason"`
}

type routerResponse struct {
	Assignments []routedAssignment `json:"assignments"`
}

// routerModel picks who does the routing.
//
// THE SESSION'S OWN MODEL COMES FIRST, and that is the whole point. A user who
// picked a model with /model has already said which one they trust to think:
// routing on "whatever discovery priced highest" ignores that and can hand the
// decision to a model they deliberately did not choose — on one account the
// priciest thing was a build preview, on another it was a video model.
//
// Order: an explicitly configured router, then the session's model, then the
// strongest tier as a last resort. Each is checked against what the provider
// actually serves, so a stale name falls through instead of failing the call.
func routerModel(prefs ModelPreferences, tiers modelTiers, served map[string]bool, sessionModel string) string {
	for _, candidate := range []string{strings.TrimSpace(prefs.Router), strings.TrimSpace(sessionModel)} {
		if candidate == "" {
			continue
		}
		if len(served) == 0 || servedContains(served, candidate) {
			return candidate
		}
	}
	return tiers.strong
}

// routerPrompt asks for a model per task and nothing else.
//
// The candidate list carries CAPABILITY ORDER (least to most capable) rather than
// prices: what matters is which model is stronger than which, and a router given
// raw prices starts optimising cost against a budget it cannot see. Size is shown
// where the id names it — "20B" vs "120B" is a capability signal the router
// should weigh, not a budget one — but prices are still withheld for that reason.
func routerPrompt(tasks []routableTask, candidates []DiscoveredModel, guidance string) string {
	var b strings.Builder
	b.WriteString("You are choosing which model should run each task of a plan. ")
	b.WriteString("Judge what each task actually requires: how much it must read, whether it must decide ")
	b.WriteString("something or merely report what it found, and how costly a wrong answer would be.\n\n")

	// THE LIST IS A SPECTRUM, NOT A PAIR, and saying so is the whole difference.
	//
	// This once read "mechanical lookups belong on the cheapest model, work that
	// judges belongs on the strongest" — a binary instruction. A router given
	// nineteen models obeyed it exactly and used two of them, always the ends,
	// while every mid-priced model on the account sat unused. The guidance has to
	// name the middle and say that most work lives there, or the middle may as
	// well not be on the list.
	b.WriteString("Match the model to the work, using the whole range:\n")
	b.WriteString("  - Trivial mechanical work — listing files, grepping a constant, reading one short file: the cheapest capable model.\n")
	b.WriteString("  - Ordinary work needing care but not deep reasoning — reading a file and explaining it, tracing a call path, ")
	b.WriteString("summarising findings: a MIDDLE model. Most tasks belong here.\n")
	b.WriteString("  - Work that decides something where being wrong is costly — auditing, judging correctness, proving a guarantee, ")
	b.WriteString("weighing alternatives: the most capable model.\n\n")
	b.WriteString("Do not answer with only the cheapest and the most capable. A middle model exists to be used, ")
	b.WriteString("and reaching for an extreme when the work sits between them is the mistake to avoid.\n\n")

	// THE OPERATOR'S ADVICE OUTRANKS THE GENERAL RULE, and is placed after it so
	// it reads as the more specific instruction rather than as background.
	//
	// It is ADDED, never a replacement. A router prompt has to end with a JSON
	// contract and a candidate list the caller validates against; letting this
	// replace the whole thing would let one config typo produce a router whose
	// output nothing can parse — and the failure would look like the model being
	// stupid rather than the prompt being broken.
	if advice := strings.TrimSpace(guidance); advice != "" {
		b.WriteString("The operator of this machine adds, and this outranks the general rule above:\n")
		b.WriteString("  " + advice + "\n\n")
	}

	b.WriteString("Available models, least to most capable:\n")
	for index, model := range candidates {
		fmt.Fprintf(&b, "  %d. %s", index+1, model.ID)
		if desc := strings.TrimSpace(model.Description); desc != "" {
			fmt.Fprintf(&b, " — %s", desc)
		}
		// The size when the id names one: a router can read "20B" vs "120B" and
		// match the light model to a scan, the heavy one to a judgement. Absent
		// for a cloud id that carries no size — never invented.
		if label := ModelSizeLabel(model.ID); label != "" {
			fmt.Fprintf(&b, " [%s]", label)
		}
		if model.Reasoning {
			b.WriteString(" [reasoning]")
		}
		b.WriteString("\n")
	}

	b.WriteString("\nTasks:\n")
	for _, task := range tasks {
		fmt.Fprintf(&b, "  id %q: %s\n", task.ID, task.Prompt)
		if len(task.Tools) > 0 {
			fmt.Fprintf(&b, "    tools: %s\n", strings.Join(task.Tools, ", "))
		}
	}

	b.WriteString("\nReply with JSON only, no prose and no code fence:\n")
	b.WriteString(`{"assignments":[{"id":"<task id>","model":"<model id exactly as listed>","reason":"<a few words>"}]}`)
	b.WriteString("\nEvery task must appear exactly once. Use only model ids from the list above.")
	return b.String()
}

// routerSystemPrompt houses the router, and deliberately not the plan-task prompt.
//
// The plan-task prompt tells its reader "You have read-only tools: USE THEM.
// Start with a tool call, not prose" — correct for a task that must go and look,
// and the exact opposite of what the router is asked to do one message later:
// answer from the list in front of it, in JSON, with no prose. A model handed
// both obeys one of them, and which one is a coin toss.
//
// It keeps its grant regardless. The child is refused outright for holding no
// tools, so the router carries one it is told not to reach for.
const routerSystemPrompt = "You are a routing classifier. You are given a list of models and a list of tasks, " +
	"and you decide which model should run each task.\n\n" +
	"Everything you need is in the message. Do not read files, do not search, do not call any tool — " +
	"the answer is a judgement about the text in front of you, not something to go and look up.\n\n" +
	"Reply with the requested JSON object and nothing else: no explanation before it, no summary after it, " +
	"no code fence around it."

// routableTask is the slice of a task the router is shown. Deliberately not the
// whole Task: dependencies and phases describe ORDER, and a router shown them
// starts reasoning about scheduling instead of difficulty.
type routableTask struct {
	ID     string
	Prompt string
	Tools  []string
}

// routeTaskModels asks the router model for an assignment per task and returns
// the ones that survive validation.
//
// A partial answer is USABLE: any task the router named validly is routed, and
// anything it missed or got wrong falls through to the classifier. Discarding
// the whole response because one entry was bad would throw away good decisions
// to punish a bad one.
func routeTaskModels(
	ctx context.Context,
	run PlanRunner,
	req PlanTaskRequest,
	model string,
	tasks []routableTask,
	candidates []DiscoveredModel,
	guidance string,
) (map[string]string, int, error) {
	if run == nil || strings.TrimSpace(model) == "" || len(tasks) < routerMinimumTasks || len(candidates) == 0 {
		return nil, 0, nil
	}
	req.Task = Task{
		ID:     "plan-model-router",
		Prompt: routerPrompt(tasks, candidates, guidance),
		Model:  model,
	}
	req.SystemPrompt = routerSystemPrompt
	result, err := run(ctx, req)
	// SPENT WHETHER OR NOT IT ANSWERED. The routing call runs on the strongest
	// model over a prompt listing every candidate and every task; a router that
	// errored or replied with nonsense still cost that, and returning zero on
	// those paths would hide the runs most worth knowing about.
	spent := result.Tokens
	if err != nil {
		return nil, spent, err
	}
	if result.Outcome != TaskSucceeded {
		return nil, spent, fmt.Errorf("router task did not succeed: %s", result.Err)
	}

	decoded, err := decodeRouterResponse(result.Output)
	if err != nil {
		return nil, spent, err
	}
	valid := map[string]bool{}
	for _, task := range tasks {
		valid[task.ID] = true
	}
	// THE ANSWER IS BOUND TO WHAT WAS OFFERED, not merely to what the provider
	// serves, and the difference is a bypass rather than a nicety.
	//
	// This checked the SERVED set — every id discovery returned. But candidates
	// have already been filtered: an excluded id, a video model, one that cannot
	// call tools. All of those are still served, so a router naming one was
	// accepted and dispatched, and a user's own planModels.exclude was silently
	// overridden by the model it was written to avoid. Reproduced with three
	// tasks routed onto an excluded id.
	//
	// Candidates are themselves drawn from discovery, so membership here is
	// strictly stronger than the served check and subsumes it.
	offered := make(map[string]bool, len(candidates))
	for _, model := range candidates {
		if id := strings.TrimSpace(model.ID); id != "" {
			offered[id] = true
		}
	}
	out := map[string]string{}
	for _, assignment := range decoded.Assignments {
		id := strings.TrimSpace(assignment.ID)
		chosen := strings.TrimSpace(assignment.Model)
		if !valid[id] || chosen == "" {
			continue
		}
		// Not offered means the router invented a name or reached past its list;
		// applying it would fail the task at dispatch, or worse, succeed on a
		// model the plan had already ruled out.
		if !offered[chosen] {
			continue
		}
		out[id] = chosen
	}
	return out, spent, nil
}

// decodeRouterResponse pulls the JSON object out of a model's reply.
//
// Models wrap JSON in prose and code fences however firmly they are asked not
// to, so the object is located by braces rather than by trusting the whole reply
// to parse. A reply with no object at all is an error, not an empty result: the
// difference between "the router said nothing useful" and "the router named no
// tasks" matters to the caller deciding whether to report a failure.
func decodeRouterResponse(output string) (routerResponse, error) {
	start := strings.Index(output, "{")
	end := strings.LastIndex(output, "}")
	if start < 0 || end <= start {
		return routerResponse{}, fmt.Errorf("router reply contained no JSON object")
	}
	var decoded routerResponse
	if err := json.Unmarshal([]byte(output[start:end+1]), &decoded); err != nil {
		return routerResponse{}, fmt.Errorf("router reply is not valid JSON: %w", err)
	}
	return decoded, nil
}

// routableTasks projects the raw task arguments into what the router is shown,
// skipping any that already name a model — an explicit choice is not up for
// reconsideration.
func routableTasks(raw []any) []routableTask {
	out := make([]routableTask, 0, len(raw))
	for _, entry := range raw {
		fields, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		if strings.TrimSpace(planString(fields, "model")) != "" {
			continue
		}
		id := strings.TrimSpace(planString(fields, "id"))
		prompt := strings.TrimSpace(planString(fields, "prompt"))
		if id == "" || prompt == "" {
			continue
		}
		out = append(out, routableTask{ID: id, Prompt: prompt, Tools: planStrings(fields, "tools")})
	}
	return out
}

// eligibleForRouting is the candidate list the router may choose from: the same
// models the tiers were built from, in the same order.
//
// The ROUTER MUST NOT SEE A WIDER SET THAN THE TIERS. Offering it a model the
// tier logic excluded — a video model, an excluded id, one that cannot call
// tools — lets the router pick something the fallback path has already judged
// unusable.
//
// This is a NARROWING, not the enforcement. It once read as though the served-set
// check downstream would drop such a choice; it would not, because every excluded
// model is still served. routeTaskModels now validates against this list itself.
func eligibleForRouting(models []DiscoveredModel, prefs ModelPreferences) []DiscoveredModel {
	return rankedEligibleModels(models, prefs)
}
