package specialist

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/Twigpine/zero/internal/config"
	"github.com/Twigpine/zero/internal/streamjson"
	"github.com/Twigpine/zero/internal/tools"
)

// OrchestrateToolName is the single spelling of the plan-capture tool.
const OrchestrateToolName = "orchestrate"

// OrchestrateTool accepts a structured PLAN as tool arguments, validates it,
// records it as session events, and executes it SEQUENTIALLY through the same
// specialist path a Task call uses.
//
// It is DEFERRED unless the zeromaxing posture is active. That is the whole
// mechanism behind Phase 2's overriding constraint: with the posture off the
// tool is registered but never advertised, so the advertised tool set, the
// tool-definition bytes and the assembled prompt are byte-identical to a build
// without the feature (proved by
// TestPostureOffPrefixUnchangedByRegisteringTheTool in internal/agent).
//
// Phase 2 is read-only and sequential by construction, not by convention: plan
// tasks may not request mutating tools, and Budget.MaxWorkers must be 1. Both
// are rejected at parse time rather than coerced, so the fields stay meaningful
// for a later phase instead of quietly lying.
type OrchestrateTool struct {
	// PostureActive reports whether the zeromaxing posture is on. nil means off
	// — a caller that never wires it gets today's behaviour, which is the
	// fail-safe direction for a tool that spends budget.
	PostureActive func() bool
	// RunTask executes one plan task. nil makes the tool refuse rather than
	// pretend it ran a plan.
	RunTask PlanRunner
	// Recorder receives plan lifecycle events; nil disables recording without
	// affecting execution.
	Recorder PlanRecorder
	// ParentTools is this run's tool grant. A task may narrow it, never widen.
	ParentTools []string
	// Depth is the nesting depth of the run holding this tool, used for the
	// admission-time headroom check.
	Depth int
	// Size is the configured plan-size tier. The zero value is the default tier,
	// so a caller that never wires it gets the same ceiling as before.
	Size config.PlanSize
	// Launch runs a plan in the BACKGROUND, and nil means background plans are
	// unavailable in this run — which is the honest default.
	//
	// THE SEAM IS A LAUNCHER, not a context, and that is deliberate. A tool that
	// held a context would have to hold one that outlives the tool call, and the
	// only thing that legitimately knows how long a background plan may live is
	// the surface that owns the session. So the surface supplies the launcher,
	// keeps the context, and can drain or cancel on exit; the tool only asks to
	// be run. It also makes headless exec's refusal fall out for free: that
	// process exits when the turn ends, so it supplies no launcher and a
	// background plan there is refused rather than silently orphaned.
	//
	// It reports false when it will not run the work, so the tool can say so
	// instead of returning a run id for a plan nobody started.
	Launch func(run func(ctx context.Context)) bool
	// Isolate prepares a worktree for a plan that may write. nil means this run
	// cannot isolate, and a plan requiring it is REFUSED rather than run in the
	// parent's tree — see resolvePlanWorkspace.
	Isolate PlanIsolator
	// DiscoverModels reports what the active provider can serve, for
	// auto_assign. nil means auto-assignment is unavailable and a plan asking
	// for it is told so rather than silently running without it.
	DiscoverModels ModelDiscoverer
	// ProbeModel asks whether the provider will actually RUN a model, as opposed
	// to merely listing it. nil skips proving entirely, which is the behaviour
	// every plan had before this existed.
	ProbeModel ModelProber
	// ProbeIdentity names the live provider/profile/endpoint whose answers
	// ProbeModel returns. It is resolved for every plan because the TUI can
	// switch providers without rebuilding this long-lived tool. Empty retains a
	// private default namespace for callers that have no provider concept.
	ProbeIdentity func() string
	// probes remembers verdicts for the life of the process, so the cost is one
	// trivial request per model per session rather than per plan.
	probes probeCache
	// ModelPrefs is what the user has said about which models plans may use:
	// per-role pins and an exclusion list. Empty leaves the automatic choice
	// alone, which is the behaviour for anyone who never configures it.
	ModelPrefs ModelPreferences
	// RequirePlanKeyword makes a plan refusable unless the turn's own user text
	// asks for one. Default false: enabling it silently would start refusing
	// plans for everyone whose phrasing does not match. See plan_keyword.go.
	RequirePlanKeyword bool
	// ContextWindows reports the window of the model a task will run on, so its
	// dependency briefing can be sized to what it can actually read. nil keeps
	// the fixed caps, which is the behaviour every plan had before it existed —
	// and the honest default for a provider that reports no window.
	ContextWindows ContextWindowFunc
	// ExtraReadRoots reports the paths the parent may read BEYOND its workspace —
	// its request_permissions grants — at DISPATCH time, so a grant that landed
	// mid-turn reaches the tasks dispatched after it. Every task gets read access
	// to these, so a plan can audit a granted external path. nil hands tasks
	// nothing beyond their workspace, the behaviour before this existed.
	ExtraReadRoots func() []string
	// Plans locates saved plans. Both directories empty means saved plans are
	// simply unavailable — the tool refuses a `saved` reference with a reason
	// rather than searching nothing and reporting "not found", which would read
	// as "you never saved it".
	Plans PlanPaths
}

const (
	// defaultPlanMaxTokens is 0: NO ceiling on what a plan may request.
	//
	// It was 200_000, and a six-task chain asking for exactly that spent
	// 469,555 — the check ran only between tasks, so the last task dispatched
	// overshot without limit and the plan was cut short anyway. Capping the
	// REQUEST while not bounding the SPEND is the worst of both: heavy work
	// cannot finish and the number means nothing. Spend is metered and
	// reported either way; a caller that wants a bound still sets max_tokens.
	defaultPlanMaxTokens = 0
)

// planSchemaBound is a schema minimum/maximum. tools.PropertySchema takes *int
// so that "unbounded" and "bounded at zero" stay different things — a plain int
// would make max_retries: 0, which is a real and meaningful setting, mean the
// same as declaring no bound at all.
func planSchemaBound(n int) *int { return &n }

func (tool *OrchestrateTool) Name() string { return OrchestrateToolName }

func (tool *OrchestrateTool) Description() string {
	return "Execute a structured plan of sub-agent tasks in dependency order. " +
		"Tasks inherit the parent's sandbox and tool grant; write tools are grantable to a task by name. " +
		"Independent tasks run in parallel up to budget.max_workers; declare dependencies with depends_on and a task never starts before what it waits on has finished. " +
		// The either/or the schema cannot express, and the shape worth
		// encouraging: a plan is only worth more than reading the code yourself
		// when its tasks are genuinely independent.
		"Supply EITHER tasks and budget, OR saved with the name of a stored plan — never neither. " +
		"Use it when a question splits into parts that can be answered independently and then combined; " +
		"a single lookup is faster done directly."
}

// Deferred hides the tool unless the posture is active — as a SECOND layer.
//
// It is not the primary mechanism, and the brief's original design assumed it
// was. Deferral only hides anything when it is ACTIVE, which needs a configured
// threshold, enough eligible tools AND a runnable tool_search loader; when it is
// inactive — an ordinary configuration — every visible tool is exposed eagerly.
// An unsafe-mode session with deferral off would therefore have advertised this
// tool with the posture off, breaking the additivity constraint. Safety()
// returning PermissionDeny is what actually enforces it, because ToolAdvertised
// short-circuits on Deny in every permission mode and runs BEFORE the deferral
// machinery. This stays as defence in depth and is asserted, not assumed.
func (tool *OrchestrateTool) Deferred() bool {
	return !tool.postureActive()
}

// DeferralEligible keeps this tool counting toward the DeferThreshold even when
// it un-defers, so turning the posture on cannot deactivate deferral for other
// tools and force-expose them.
func (tool *OrchestrateTool) DeferralEligible() bool { return true }

func (tool *OrchestrateTool) postureActive() bool {
	return tool != nil && tool.PostureActive != nil && tool.PostureActive()
}

// StreamsChildProgress declares that a plan's tasks are child agent runs whose
// stream-json events should reach the parent's UI, exactly as a Task
// sub-agent's do. Declaring it is what un-gates the progress path — the loop
// used to key on the tool name, so this tool ran invisibly.
func (tool *OrchestrateTool) StreamsChildProgress() bool { return true }

func (tool *OrchestrateTool) Parameters() tools.Schema {
	return tools.Schema{
		Type: "object",
		Properties: map[string]tools.PropertySchema{
			"name":        {Type: "string", Description: "Short label for the plan."},
			"description": {Type: "string", Description: "What the plan is for."},
			"tasks": {
				Type: "array",
				// THE SHAPE IS DECLARED, not described in prose.
				//
				// This was a bare array with a paragraph of English, so a model had
				// to compose nested objects from a sentence — and did it wrong twice
				// in one session. It read "an optional read-only tool subset", which
				// names no tool at all, and wrote tools: ["read-only"], taking the
				// adjective for a value. Admission then refused the plan and recited
				// the legal names in the error: the right list, arriving after the
				// failure instead of before it.
				//
				// The enum comes from PlanGrantableToolNames, which is the list
				// admission itself validates against and was already exported so
				// "the grant and the validator cannot come to disagree". The schema
				// was the one caller not using it.
				Items: &tools.PropertySchema{
					Type: "object",
					Properties: map[string]tools.PropertySchema{
						"id":     {Type: "string", Description: "Short identifier, unique within the plan: letters, digits, hyphen, underscore."},
						"prompt": {Type: "string", Description: "What this task must do, and what it must return."},
						"depends_on": {
							Type:        "array",
							Description: "Ids of tasks that must finish first. A dependent receives their results in its prompt.",
							Items:       &tools.PropertySchema{Type: "string"},
						},
						"tools": {
							Type: "array",
							Description: "Tools this task may use, narrowing the run's own grant — it can never widen it. " +
								"Omit for the read-only default. Naming a write tool makes the plan write-capable, which " +
								"runs it in an isolated worktree and asks for approval first.",
							Items: &tools.PropertySchema{Type: "string", Enum: PlanGrantableToolNames()},
						},
						"model": {Type: "string", Description: "Model this task runs on. Omit to inherit the session's."},
						"phase": {Type: "string", Description: "Display label only; it carries no execution meaning."},
					},
					Required: []string{"id", "prompt"},
				},
				Description: "The plan's tasks. Each has an id, a prompt, optional depends_on ids, an optional tool subset, and an optional phase label. " +
					"A task may also name a model to run on — any model this provider serves, by id or alias; omit it to inherit " +
					"this session's model. Use it to spend where it matters: a cheaper model for mechanical scanning, a stronger " +
					"one for the tasks that judge or decide. A name the provider cannot serve fails when the task runs.\n\n" +
					// WRITING THE TASKS IS THE LEVER, and it sits upstream of everything
					// else this tool does. Routing cannot rescue a badly split plan and
					// verification cannot rescue a task that never asked a real question.
					// A measured run produced ten tasks whose prompts all opened with the
					// same wrapper — ten paraphrases of one job rather than ten jobs.
					"Writing good tasks matters more than how many you write:\n" +
					"- A task should be a whole question someone could answer alone and be judged right or wrong about. " +
					"\"Trace how X is computed and quote file:line for each step\" is a task; \"look at X\" is not.\n" +
					"- Split by SUBJECT, never by phrasing. Two tasks that would read the same files are one task.\n" +
					"- Say what the task must return. A task whose output cannot be checked cannot be verified by anything downstream.\n" +
					"- Use depends_on for real dependencies only. A dependent receives its dependencies' results in its prompt, " +
					"so chain a task that must build on findings — and leave independent work independent so it runs in parallel.\n" +
					"- Do not split one modest job into pieces to look thorough. Fewer, larger, genuinely independent tasks beat many small ones: " +
					"each task pays for its own context.\n" +
					"- Be honest about what a task can do. A task that reads and reports is not the same as one that decides; " +
					"give the deciding work to a task that says it is deciding, and name a stronger model for it.\n\n" +
					// A CONVENTION, NOT MACHINERY. Nothing here parses verdicts or
					// filters claims — this teaches a shape the plan author can adopt
					// with the tasks they already write.
					//
					// Earned by measurement rather than argument: two runs of the same
					// audit on this repo, one ending in a verify task and one not. The
					// verified run dropped FIVE overclaims the other passed through to
					// its report, including an inference the unverified run stated as
					// fact. Fan-out multiplies whatever a finder produces, including its
					// mistakes; nothing else in this tool attacks a claim once made.
					"If a plan produces CLAIMS, end it in verification:\n" +
					"- Finder tasks gather claims with file:line evidence.\n" +
					"- A verify task depends on the finders, so its briefing carries their claims, and attacks every one.\n" +
					"- A synthesis task depends on the verifier and reports only what survived.\n\n" +
					"A verify task's prompt must tell it to: try to REFUTE each claim from the code first, and confirm only " +
					"when the code forces it; default to refuted when uncertain, but quote what the code actually says — a " +
					"guessed refutation is worth no more than a guessed confirmation; judge each claim independently, treating " +
					"the finder's confidence as no evidence at all; and re-read the cited locations itself, because the " +
					"briefing carries a claim, not a trace.\n\n" +
					// TWO GAPS THAT LOOK ALIKE AND ARE NOT, and merging them is how the
					// second one disappears.
					//
					// A measured run built against a specification and reported a clean
					// list of requirement conflicts while filing NOT ONE relaxation —
					// though it had lowered a one-million-record bound to ten thousand,
					// cut a sixty-second soak to five, and excluded a latency class from
					// its own measurements. All three were real, all three were
					// defensible, and all three reached the reader as cells inside a
					// results table rather than as gaps. A reader who trusted the report
					// never learned what had not been built.
					//
					// The distinction is not stylistic: a conflict is a question the
					// spec failed to answer, and a relaxation is an answer the work
					// failed to reach. One is resolved by deciding; the other is
					// outstanding until someone builds it.
					"If a plan BUILDS to a specification, keep two kinds of gap apart and report them separately:\n" +
					"- A CONFLICT is the spec disagreeing with itself — two requirements that cannot both hold. " +
					"It is resolved by choosing a reading, and the report names the reading chosen.\n" +
					"- A RELAXATION is the work coming in under the spec — a bound lowered, a case left unhandled, " +
					"a measurement taken under easier conditions than were asked for. Nothing disagreed; less was built.\n" +
					"Give relaxations a task or a section of their own, and never only a cell in a results table. " +
					"Listed beside conflicts they read as questions already settled rather than as work still owed, " +
					"and a relaxation nobody wrote down is indistinguishable from a requirement nobody noticed. " +
					"Report each one even when it was the right call: the judgement is the reader's to make, and they can only make it if they are told.",
			},
			"params": {
				Type: "object",
				Description: "Values for a saved plan's ${placeholders}, as name/value pairs. Only usable with `saved`. " +
					"A plan's prompts may contain ${name} where the target varies — a scope, a directory, a release — and these fill them in, " +
					"so one reviewed plan runs against many targets instead of being copied and edited per target. " +
					"Substituted into task prompts and the plan description only: never into tools, ids, depends_on, model or budget, " +
					"which are authority and graph and stay as they were saved. " +
					"A placeholder with no value is refused, and so is a value matching no placeholder.",
			},
			"template": {
				Type: "string",
				Description: "Build the plan from a named SHAPE instead of writing tasks by hand: " +
					"audit (examine one subject from several independent angles, then verify the findings), " +
					"compare (describe two things separately, then contrast them), " +
					"sweep (ask one question of several targets, then combine the answers — it emits one task per target plus a synthesis, so keep the target list inside this run's plan-size tier). " +
					"Supply its values in params. Mutually exclusive with tasks and saved. " +
					"Prefer this when the shape fits: the graph, the ids, the dependencies and the budget all come out valid, " +
					"so the call is not refused and retried.",
			},
			"saved": {
				Type: "string",
				Description: "Run a plan saved earlier, by name, instead of supplying tasks. " +
					"Mutually exclusive with tasks/budget/name/description: a saved plan runs as it was saved.",
			},
			"auto_assign": {
				Type: "boolean",
				Description: "Pick a model per task automatically from what this provider serves: the cheapest tool-calling model " +
					"for tasks that scan or read, a mid-priced one for tasks that change code, and the strongest for tasks that " +
					"judge or verify. Off by default. A task that names its own model is never overridden, and the result reports " +
					"what was chosen.",
			},
			"background": {
				Type: "boolean",
				Description: "Run the plan in the background and report when it finishes, instead of holding this turn. " +
					"Only available in the interactive TUI; a headless run exits when the turn ends, so it refuses this.",
			},
			"budget": {
				Type: "object",
				// DECLARED for the same reason tasks is: a model composing this from
				// prose has to invent the field names, and inventing one is how a
				// plan fails at admission instead of running.
				Properties: map[string]tools.PropertySchema{
					// DECLARED BOUNDS, not merely described ones. Every number here is
					// already enforced at admission, and prose saying "1 to 16" is
					// advice to the same model that has to guess the field names — so
					// a run emitted max_tokens_per_task: 5000, was refused for being
					// below the floor, and spent a whole turn learning a rule the
					// schema could have carried. A bound in the schema is checked by
					// the provider before the call is ever made.
					//
					// EACH ONE MIRRORS ITS ENFORCEMENT CONSTANT rather than repeating
					// the literal, because two spellings of one limit drift.
					"max_workers":         {Type: "integer", Minimum: planSchemaBound(1), Maximum: planSchemaBound(maxPlanWorkers), Description: "How many tasks may run at once, 1 to 16. 1 is sequential."},
					"max_tokens":          {Type: "integer", Minimum: planSchemaBound(minimumPlausibleTaskTokens), Description: "Bound on the WHOLE plan, shared by every task. DO NOT SET THIS unless the user asked for a spending limit. You cannot estimate what a plan costs, and a number guessed too low does not save money — it stops tasks mid-work and skips the ones that had not started, so the plan spends its budget and returns nothing. Omitted, the plan runs unbounded within this run's own ceiling with a wall-clock backstop, and spend is still metered and reported. If the user did ask for a limit, prefer max_tokens_per_task."},
					"max_tokens_per_task": {Type: "integer", Minimum: planSchemaBound(minimumPlausibleTaskTokens), Description: "Optional bound on ONE task, and the right knob for budgeting per sub-agent. Use it ALONE: with max_tokens also set, each task is limited by its share of the total long before its own cap applies, so the cap does nothing and the plan is refused."},
					"max_wall_seconds":    {Type: "integer", Minimum: planSchemaBound(1), Description: "Optional wall-clock bound on the whole plan."},
					"max_stall_seconds":   {Type: "integer", Minimum: planSchemaBound(int(minStallTimeout.Seconds())), Description: "How long ONE task may emit nothing before it is stopped. Default 180."},
					"max_retries":         {Type: "integer", Minimum: planSchemaBound(0), Maximum: planSchemaBound(maxPlanRetries), Description: "Extra attempts a STALLED task gets, 0 to 3. Default 1."},
				},
				Required:    []string{"max_workers"},
				Description: "Required. max_workers (1-16) is how many tasks may run at once; 1 is sequential and is the right answer unless the tasks are genuinely independent. The machine's own capacity may be lower and the report says which number applied. max_tokens and max_wall_seconds are optional bounds; omit them to run unbounded — spend is reported either way. max_stall_seconds bounds how long ONE task may emit nothing (default 180); it resets on every event, so a slow-but-working task is never stopped. max_retries (0-3, default 1) is how many extra attempts a STALLED task gets; a task that failed with a real error is never retried. max_tokens_per_task bounds what ONE task may spend, and is the right knob for budgeting per sub-agent — use it ALONE, without max_tokens, or each task is limited by its share of the total instead and the plan is refused as incoherent. Sizing, from measured runs: a task that traces or audits a large repo costs 510k-1,017k tokens, so budget about 1M each; one reasoning over what its dependencies already found costs far less. max_tokens is the TOTAL across every task, not per task — max_tokens_per_task is the per-task one, and it may not exceed the total. A cap BELOW what a task needs saves nothing — a plan capping tasks at 200k lost all six of them between 213k and 259k and still spent 1,437,049 tokens, finishing none. A capped task is told its budget and asked to write a partial answer before reaching it, so tight is survivable and too-tight is not. Omit max_tokens unless the user asked for a spending limit: guessing it low is how a plan spends everything and returns nothing. A budget far below what its task count needs is warned about at admission and left to you. When a plan runs out, tasks in flight are STOPPED MID-RUN — they keep what they had already found, and it reaches both the report and any task depending on them — while tasks not yet started never run at all. So the cost of guessing low is the questions at the END of the plan, which is usually the synthesis you wanted. If you cannot estimate, omit max_tokens and use max_wall_seconds instead.",
			},
		},
		// EMPTY, and the either/or lives in the DESCRIPTION instead.
		//
		// `saved` supplies tasks and budget, so neither is unconditionally
		// required any more — but dropping them from Required emits no
		// "required" key at all, and the model is then handed a tool whose every
		// argument looks optional. tools.Schema has no oneOf, so the constraint
		// that is actually true — "tasks and budget, or saved, never neither" —
		// cannot be spelled here. It is spelled in Description, which is the
		// thing a model reads for intent, and enforced in ParsePlan, which is
		// the only place that can see the resolved arguments.
		Required:             []string{},
		AdditionalProperties: false,
	}
}

func (tool *OrchestrateTool) Safety() tools.Safety {
	// PermissionDeny when the posture is off. This — not Deferred() — is what
	// actually enforces the additivity constraint: ToolVisible consults
	// ToolAdvertised, which short-circuits on PermissionDeny for EVERY
	// permission mode, and it runs BEFORE the deferral machinery. Deferred()
	// alone is insufficient because deferral only activates when a usable
	// tool_search loader is registered and the eligible count clears the
	// threshold; when it is inactive every visible tool is exposed eagerly, so
	// an unsafe-mode session with deferral off would have advertised this tool
	// with the posture off. Deferred() below is now belt-and-braces.
	permission := tools.PermissionDeny
	if tool.postureActive() {
		// PermissionAllow, not PermissionPrompt, and this is deliberate.
		//
		// WHY NOT PROMPT: the approval surface renders "permission: orchestrate
		// prompt" plus this static Reason and nothing else. PermissionRequest
		// carries an Args map, but no renderer reads it — verified across
		// permission_prompt.go, transcript.go and rendering.go. So the user
		// would be asked to approve a plan without being shown its task count,
		// its prompts, its budget or its graph. A dialog that cannot show what
		// it is approving is not a gate; it trains click-through, which is
		// worse than no dialog because it also erodes the prompts that DO carry
		// information.
		//
		// WHAT BOUNDS IT INSTEAD, all enforced rather than advisory:
		//   - tasks are READ-ONLY, rejected at validation (validateTaskTools)
		//     and intersected again at dispatch (planToolGrant)
		//   - a budget is REQUIRED and enforced at dispatch, not just validated
		//   - the posture itself was explicit user consent: this tool does not
		//     exist until someone types /effort zeromaxing
		//
		// PHASE 3 MUST REVISIT THIS. The moment plan tasks can WRITE, an
		// approval gate becomes necessary — and it needs a real renderer that
		// shows the plan first. Inheriting Allow without that renderer would be
		// inheriting this reasoning without its precondition.
		permission = tools.PermissionAllow
	}
	return tools.Safety{
		SideEffect: tools.SideEffectShell,
		Permission: permission,
		// Not "read-only": this Reason is rendered on the approval card, and
		// PermissionForArgs only prompts when argsCanWrite is true — so every
		// card a user actually sees is asking about a plan that CAN write.
		// Describing it as read-only there was precisely backwards.
		Reason: "Runs a plan of specialist sub-agents under the parent's sandbox and tool grant; tasks may hold write tools granted by name.",
		// Irrelevant while Permission is Allow (auto advertises Allow tools
		// anyway) and equally irrelevant while it is Deny (ToolAdvertised
		// short-circuits on Deny before reading this). Left false so the field
		// never becomes the thing holding the gate open.
		AdvertiseInAuto: false,
	}
}

// PermanentlyDenied reports that no arguments can make this tool callable while
// the posture is off. The posture is a session state, not a parameter, and
// ArgsPermissioner cannot express that — see tools.PermanentDenier.
func (tool *OrchestrateTool) PermanentlyDenied() bool { return !tool.postureActive() }

// PermissionForArgs is what makes a WRITE-CAPABLE plan ask, and a read-only one
// not.
//
// Safety() cannot decide this: it sees no arguments, so it can only describe the
// tool, and "orchestrate may write" is a property of the PLAN. A read-only plan
// keeps PermissionAllow — prompting for it would add friction with no safety to
// show for it, which is the click-through argument Safety() records. A plan that
// can write is a different decision and gets a different answer.
//
// The card that answers it can now show the plan (the permission detail
// renderer) and the work lands in a worktree of the plan's own (the isolator).
// Prompting before either existed would have been asking a user to approve
// something the screen could not describe.
// RefusesPersistentPermission reports that an approval for this tool must never
// be remembered.
//
// A read-only plan does not prompt at all, so EVERY prompt this tool produces is
// for a plan that can write — and a plan that can write can be given bash, for
// which the permission layer already refuses to persist an approval. Letting
// "always allow orchestrate" be remembered would be a strictly broader standing
// grant than the one that refusal exists to prevent, and it would disable the
// approval gate permanently with a single keystroke.
//
// The plan is still approvable for THIS call and for the session; only the
// permanent form is withheld, which is exactly how bash behaves.
func (tool *OrchestrateTool) RefusesPersistentPermission() bool { return true }

func (tool *OrchestrateTool) PermissionForArgs(args map[string]any) tools.Permission {
	if !tool.postureActive() {
		return tools.PermissionDeny
	}
	if tool.argsCanWrite(args) {
		return tools.PermissionPrompt
	}
	return tools.PermissionAllow
}

// argsCanWrite reports whether the arguments describe a plan that could change
// something.
//
// It reads the ARGUMENTS rather than a parsed Plan because the permission
// decision happens before parsing, and it errs toward PROMPTING: a saved plan
// whose tasks live on disk, or arguments this cannot read, are treated as
// possibly-writing. A wrong guess in that direction costs one prompt; the other
// direction runs write tasks without asking.
func (tool *OrchestrateTool) argsCanWrite(args map[string]any) bool {
	if planString(args, "saved") != "" {
		// The tasks are in a file this has not opened. Ask.
		return true
	}
	rawTasks, ok := args["tasks"].([]any)
	if !ok {
		return false
	}
	for _, raw := range rawTasks {
		task, ok := raw.(map[string]any)
		if !ok {
			// Unreadable entry: ask rather than assume it is harmless.
			return true
		}
		for _, name := range planStrings(task, "tools") {
			if !planReadOnlyTools[name] {
				return true
			}
		}
	}
	return false
}

func (tool *OrchestrateTool) Run(ctx context.Context, args map[string]any) tools.Result {
	return tool.RunWithOptions(ctx, args, tools.RunOptions{})
}

// runnerForCall attaches everything that belongs to THIS tool call — the
// progress callback and the parent's identity — to every task request.
//
// PER CALL, not captured at construction: RunTask is built once at
// registration and holds only run-invariant state, while the progress callback
// belongs to one tool call. Capturing it in NewPlanRunner is precisely the
// mistake the runner's own doc comment warns about.
//
// The callback is shared by every task rather than keyed per task, because the
// loop's callback carries only the parent's tool-call id and has no room for a
// sub-key. THE CONSEQUENCE, now that plans can be concurrent: with more than one
// worker a consumer CANNOT attribute an event to a task, and the TUI stops
// trying rather than attributing every child to whichever task started last.
// Threading the child's identity through the loop's callback is what would fix
// it properly. Previously this read: MaxWorkers is validated to be 1,
// so exactly one task is in flight at any moment and the consumer can attribute
// events to the task it last saw dispatched — which is no longer true.
// moment two tasks can run at once — see the note on the TUI plan recorder.
func (tool *OrchestrateTool) runnerForCall(options tools.RunOptions) PlanRunner {
	run := tool.RunTask
	if run == nil {
		return nil
	}
	recorder := tool.Recorder
	return func(ctx context.Context, req PlanTaskRequest) (TaskResult, error) {
		// PER TASK, not per call. The loop's own callback carries the parent's
		// tool-call id and nothing else, so every task's events look alike to a
		// consumer; routing them through the recorder with the task id attached
		// is what lets a display tell them apart. The recorder falls back to
		// doing nothing when a surface has no live UI, which is the headless
		// case.
		taskID := req.Task.ID
		callerProgress := options.Progress
		req.Progress = func(event streamjson.Event) {
			// BOTH, and they are not alternatives. The recorder gets the event
			// WITH the task id so a display can route it; the caller's own
			// callback still fires because that is the contract a plan task's
			// child streams under — the same one a Task sub-agent's child does,
			// and restoring it was a fix in its own right. Sending only to the
			// recorder would have quietly undone that for every caller without
			// one, which is the headless case.
			planTaskProgress(recorder, taskID, event)
			if callerProgress != nil {
				callerProgress(event)
			}
		}
		// The parent's identity, read from the same RunOptions fields the Task
		// tool reads (task_tool.go). A plan task inherits the model its parent
		// is running on; without this it fell back to the child's own config,
		// so switching model with /model or --model left plan tasks running
		// somewhere else entirely.
		req.ParentSessionID = options.SessionID
		req.ParentModel = options.Model
		req.ParentReasoningEffort = options.ReasoningEffort
		req.ParentToolCallID = options.ToolCallID
		return run(ctx, req)
	}
}

// resolveSavedPlan swaps a `saved` reference for the stored plan's arguments.
//
// Anything the caller supplied ALONGSIDE `saved` is refused rather than merged.
// A half-overridden plan is not the plan that was saved, and silently letting
// one field through would mean "run the sweep plan" ran something else — the
// name would still be right in the transcript.
func (tool *OrchestrateTool) resolveSavedPlan(args map[string]any) (map[string]any, error) {
	name := planString(args, "saved")
	if name == "" {
		return args, nil
	}
	for _, field := range []string{"tasks", "budget", "name", "description"} {
		if _, present := args[field]; present {
			return nil, fmt.Errorf(
				"a saved plan is run as it was saved: remove %q, or supply the plan inline instead of naming one", field)
		}
	}
	// params is NOT in that list, and must not be: it does not override the
	// stored plan, it fills the holes the plan itself declared. A plan with no
	// ${placeholder} takes none, and supplying one is refused below.
	params, err := planParamsFromArgs(args)
	if err != nil {
		return nil, err
	}
	if tool.Plans.ProjectDir == "" && tool.Plans.UserDir == "" {
		return nil, fmt.Errorf("saved plans are not available in this run")
	}
	stored, err := FindSavedPlan(tool.Plans, name)
	if err != nil {
		return nil, err
	}
	// EXECUTION DIRECTIVES SURVIVE THE SWAP; plan content does not.
	//
	// The refusal above exists because a half-overridden plan is not the plan
	// that was saved. These two are not plan content: they say HOW to run it,
	// not WHAT to run, and neither appears in a stored plan's args. Returning
	// the stored map wholesale dropped them silently — `{"saved":"sweep",
	// "background":true}` parsed, passed the refusal list, and then ran in the
	// foreground because the flag was read from the map that had replaced it.
	for _, directive := range []string{"background", "auto_assign"} {
		if value, present := args[directive]; present {
			stored.Args[directive] = value
		}
	}
	// EXPANDED BEFORE ParsePlan, so admission validates the plan that will run
	// rather than a template of it: a substituted prompt still faces the same
	// tool-grant narrowing, depth cap and budget checks. Returns a copy, so the
	// stored plan is unchanged for the next run.
	return expandPlanParams(stored.Args, params)
}

// limits supplies the hard caps a plan must fit inside, DERIVED — there is no
// override field.
//
// There was one: a `Limits *Limits` that nothing ever set, in production or in
// a test, so `if tool.Limits != nil` could not be true. It was found sweeping
// for siblings of the OrchestrateAvailable defect and it is the same family —
// a knob a reader would trust, sitting next to Size, which is the knob that
// actually works. Deleted rather than wired: nothing needs it, because every
// value it could have overridden is already derived from something real.
//
// The task ceiling comes from the CONFIGURED TIER rather than a constant here.
// It was a hard-coded 20 with no way to move it: too many for a metered
// provider, too few for a real sweep, and discoverable only by reading this
// file. PlanSize.MaxTasks resolves an unset or unrecognised tier to the default,
// so the zero value is exactly the old ceiling.
func (tool *OrchestrateTool) limits(options tools.RunOptions) Limits {
	size := tool.Size
	if !size.Valid() {
		size = config.DefaultPlanSize
	}
	limits := Limits{
		MaxTasks:       size.MaxTasks(),
		MaxTasksSource: fmt.Sprintf("the %q plan size", size),
		MaxTokens:      defaultPlanMaxTokens,
		CurrentDepth:   tool.Depth,
		ParentTools:    tool.ParentTools,
	}
	return limits
}

func (tool *OrchestrateTool) RunWithOptions(ctx context.Context, args map[string]any, options tools.RunOptions) tools.Result {
	// The posture gate, again at the point of USE. Safety() already hides the
	// tool, but a model can call a tool it was never advertised — the registry
	// dispatches by name — so refusing here is what makes "only under the
	// posture" a rule rather than a display convention.
	if !tool.postureActive() {
		return tools.Result{
			Status: tools.StatusError,
			Output: "Error: orchestrate is only available under the zeromaxing posture. Turn it on with /effort zeromaxing.",
		}
	}
	// THE USER MUST HAVE ASKED, when this session requires it. Checked BEFORE the
	// saved-plan lookup and before auto-assignment: both touch the disk or the
	// provider, and a refused plan must cost neither. See plan_keyword.go for why
	// this is an admission check rather than a line in the prompt.
	if tool.RequirePlanKeyword && !planRequestedByUser(options.UserMessage) {
		return tools.Result{Status: tools.StatusError, Output: "Error: " + planKeywordRefusal().Error()}
	}
	// A SAVED plan is loaded into the same argument shape and then validated by
	// the same constructor. It is not a second way to run a plan: by the time
	// ParsePlan sees it, a stored plan and a model-supplied one are
	// indistinguishable, so the tier, the depth check, the read-only rule and
	// the parent-grant intersection all apply to it unchanged.
	// A TEMPLATE IS RESOLVED FIRST, and its output then travels the ordinary
	// path: ParsePlan validates it, the grant is intersected, the budget is
	// checked. It is a way to WRITE the arguments, never a second admission
	// route — which is the rule every part of this feature follows, because its
	// whole defect history is second call paths carrying less than the first.
	args, err := resolveTemplatePlan(args)
	if err != nil {
		return tools.Result{Status: tools.StatusError, Output: "Error: " + err.Error()}
	}
	args, err = tool.resolveSavedPlan(args)
	if err != nil {
		return tools.Result{Status: tools.StatusError, Output: "Error: " + err.Error()}
	}
	// AUTO-ASSIGNMENT RUNS BEFORE THE CONSTRUCTOR, on the arguments.
	//
	// Not inside ParsePlan: that has six call sites, four of them TUI verb and
	// render paths through savedPlanLimits(), and a provider probe on those would
	// block the interface, make parsing non-deterministic, and give a saved plan
	// different models every time it was re-admitted. Here it happens once, on
	// the dispatch path, where a network call is already expected.
	//
	// Working on the args rather than the parsed plan is what makes an assigned
	// model indistinguishable from a hand-written one: same validation, same
	// Args() round trip into a saved plan, same resume.
	// VALIDATE BEFORE SPENDING ANYTHING. Auto-assignment lists the provider's
	// models and, when routing is on, spends a call on a frontier model before a
	// single task runs — and it was doing that for plans that were then rejected
	// outright: a duplicate task id, a cycle, one task over the size tier. A
	// model that proposes an oversized plan twice paid for routing twice and got
	// nothing either time.
	//
	// ParsePlan is pure — no I/O, no network — so running it first costs
	// microseconds against a provider round trip. Assignment can only ADD a model
	// field to tasks that already parsed, so nothing it does can rescue a plan
	// that failed here, and the real parse below still has the final word.
	if _, err := ParsePlan(args, tool.limits(options)); err != nil {
		return tools.Result{Status: tools.StatusError, Output: "Error: " + err.Error()}
	}
	// THE VERIFIER, BEFORE ASSIGNMENT so the appended task is routed and pinned
	// exactly like a hand-written one — the verify role is precisely the one the
	// user's strongest pin exists for. After the pre-parse, so a plan that was
	// going to be refused is refused on what its author wrote.
	verifyNote := tool.appendVerifyStage(args, options)
	assignNotes, routerTokens, autoErr := tool.autoAssignModelsCosting(ctx, args, options)
	if autoErr != nil {
		return tools.Result{Status: tools.StatusError, Output: "Error: " + autoErr.Error()}
	}
	// ParsePlan validates as part of parsing; there is no other constructor, so
	// this call cannot be bypassed by any argument shape.
	plan, err := ParsePlan(args, tool.limits(options))
	if err != nil {
		return tools.Result{Status: tools.StatusError, Output: "Error: " + err.Error()}
	}
	if tool.RunTask == nil {
		return tools.Result{Status: tools.StatusError, Output: "Error: orchestrate has no task runner wired."}
	}
	// ONE PLAN AT A TIME, on this path too.
	//
	// The surface that displays a plan can hold exactly one, and the card table
	// pairing tasks with rows is keyed by task id — unique within a plan, not
	// between two. The TUI already refused a second plan on the path a USER
	// drives; this is the path the MODEL drives, and it is the reachable one:
	// a background plan returns immediately by design, so the very next tool
	// call lands while it is still running.
	//
	// Checked AFTER parsing, so an invalid plan is still reported as invalid
	// rather than blamed on the plan already running — and BEFORE admission, so
	// a refused plan leaves no record of having started.
	if running, busy := runningPlanOn(tool.Recorder); busy {
		return tools.Result{
			Status: tools.StatusError,
			Output: fmt.Sprintf(
				"Error: plan %q is still running, and a session shows one plan at a time. "+
					"Wait for it to finish — its result arrives on a later turn if it is a background plan — "+
					"or stop it with /plans stop, then run this one.", running),
		}
	}

	// BACKGROUND, when asked for and when this surface can carry one.
	//
	// The runner is built HERE, on the tool-call goroutine, because it captures
	// the call's RunOptions — the parent's model, effort and session id. Only
	// the CONTEXT comes from the launcher; capturing a per-call context in a
	// goroutine is the prototype's defect, and capturing per-call VALUES is
	// exactly what must happen.
	if planBool(args, "background") {
		if tool.Launch == nil {
			return tools.Result{
				Status: tools.StatusError,
				Output: "Error: background plans are not available in this run — a headless run exits when the turn ends, " +
					"so a plan launched into the background could never report. Run it in the foreground, or use the interactive TUI.",
			}
		}
		// THE SAME WORKSPACE RULE. Resolved here rather than inside the
		// goroutine so a plan that cannot be isolated is refused NOW, with a
		// reason the model reads, instead of failing invisibly on a later turn.
		// Applying the rule to only one of the two dispatch paths is the defect
		// this feature has produced three times.
		workspace, err := resolvePlanWorkspace(ctx, plan, tool.Isolate)
		if err != nil {
			return tools.Result{Status: tools.StatusError, Output: "Error: " + err.Error()}
		}
		run := tool.runnerForCall(options)
		parentTools := tool.ParentTools
		recorder := tool.Recorder
		launched := tool.Launch(func(backgroundCtx context.Context) {
			defer workspace.Release()
			recordPlanAdmitted(recorder, plan)
			report := ExecutePlanIn(backgroundCtx, plan, workspace, parentTools, run, recorder, tool.execOptionsFor(plan, routerTokens)...)
			recordPlanCompleted(recorder, plan, report)
		})
		if !launched {
			workspace.Abort()
		}
		if !launched {
			return tools.Result{
				Status: tools.StatusError,
				Output: "Error: the plan was not started — this session is shutting down, or a background plan is already running.",
			}
		}
		// NOT StatusOK-with-a-summary: there is no result yet, and reporting one
		// would be reporting work that has not happened.
		return tools.Result{
			Status: tools.StatusOK,
			Output: fmt.Sprintf(
				"Plan %q started in the background with %d tasks. It is NOT finished — its result will arrive on a later turn. "+
					"Carry on with other work; do not wait for it and do not report it as done.",
				plan.Name(), plan.TaskCount()),
			Meta: map[string]string{"plan_status": "background"},
		}
	}

	// WHERE IT RUNS. A read-only plan runs where the parent runs; one that can
	// write gets a tree of its own or does not run at all. Resolved BEFORE the
	// admission is recorded, so a refused plan leaves no record of having
	// started.
	workspace, err := resolvePlanWorkspace(ctx, plan, tool.Isolate)
	if err != nil {
		return tools.Result{Status: tools.StatusError, Output: "Error: " + err.Error()}
	}
	defer workspace.Release()

	recordPlanAdmitted(tool.Recorder, plan)
	// SAID BEFORE IT RUNS, not deduced from the wreckage afterwards.
	budgetWarning := warnBudgetLooksLow(plan.Budget(), plan.Tasks())
	// SAID BEFORE THE FIRST TASK, not only in the output afterwards. The estimate
	// was computed correctly on five consecutive runs of the same plan and read
	// by nobody, because it rode the result — which arrives once the run is
	// already over. A warning delivered with the corpse is not a warning.
	if budgetWarning != "" {
		planPreflight(tool.Recorder, "budget may be too low: "+budgetWarning)
	}
	report := ExecutePlanIn(ctx, plan, workspace, tool.ParentTools, tool.runnerForCall(options), tool.Recorder, tool.execOptionsFor(plan, routerTokens)...)
	recordPlanCompleted(tool.Recorder, plan, report)

	result := tools.Result{
		Status: tools.StatusOK,
		Output: report.Summary() + planBudgetWarning(budgetWarning) + planWorkspaceNote(workspace) +
			autoAssignSummary(assignNotes) + verifyStageSummary(verifyNote),
		Meta: map[string]string{
			"plan_status": string(report.Status),
			"max_speedup": strconv.FormatFloat(report.MaxSpeedup, 'f', 2, 64),
		},
	}
	// A plan that did not fully succeed must NOT report OK. This repo has
	// repeatedly reported failure as success; in a plan, nineteen of twenty
	// tasks failing surfacing as a clean result is the same defect at scale.
	//
	// The ONE exception is a task the author did not write: when the appended
	// verifier is the only thing that did not succeed, every task that was asked
	// for is done, and calling that "error" makes the orchestrating model re-run
	// a plan whose work already completed. The report still counts the failure
	// and the caller is told the claims went unverified — the verdict on the
	// author's plan is simply left to the author's tasks.
	if report.Status != PlanCompleted {
		if onlyTheAppendedVerifierFailed(report, verifyNote) {
			result.Output += verifyStageUnverifiedNote(verifyNote)
		} else {
			result.Status = tools.StatusError
		}
	}
	return result
}

// autoAssignModels fills in a model per task when the plan asked for it,
// returning one note per assignment for the result.
//
// OFF UNLESS ASKED. planBool reports false for a missing key, and that is the
// wanted default: turning it on by default would change which model every
// existing plan runs on — and what it costs — without anyone choosing that.
//
// The two unavailable cases are told apart deliberately. A plan that asked for
// auto-assignment on a run that CANNOT do it is refused, because silently
// running without it is the thing the user asked not to happen. A provider that
// answers but offers nothing usable is NOT an error: every task simply inherits,
// which is exactly the behaviour before this existed.
// autoAssignModels assigns per-task models and reports what it did.
//
// Kept at two returns for the seven call sites that only care about the notes.
// The production path needs a third thing — what routing COST — and gets it from
// autoAssignModelsCosting below; editing seven tests to thread a value they do
// not assert on would be seven tests changed to keep compiling.

// autoAssignModelsCosting is autoAssignModels, also reporting the tokens the
// routing call spent — which no task performed and which therefore reached
// neither the plan's budget nor its reported total.
func (tool *OrchestrateTool) autoAssignModelsCosting(ctx context.Context, args map[string]any, options tools.RunOptions) ([]string, int, error) {
	// DECLARED UP HERE so every early return can report it. Each of those
	// returns precedes the routing call, so they honestly report 0: nothing was
	// spent because nothing was routed.
	var routerTokens int
	// THE ARGUMENT WINS IN BOTH DIRECTIONS. A plan that supplies auto_assign is
	// believed — true or false — and only a plan that says nothing falls back to
	// what the user configured. Without the presence check a configured default
	// of true could never be turned off for a single plan, because an absent
	// argument and an explicit false look identical.
	requested, supplied := planBoolSet(args, "auto_assign")
	if !supplied {
		requested = tool.ModelPrefs.AutoAssign
	}
	if !requested {
		return nil, routerTokens, nil
	}
	// ASKED FOR vs CONFIGURED, and they must fail differently.
	//
	// A plan that SAYS auto_assign wants it: running silently without it is the
	// thing that request exists to prevent, so an unavailable run is refused.
	// A CONFIGURED default is a standing preference, not a demand — refusing
	// every plan because a models endpoint blinked would make one setting break
	// all planning, offline or behind a proxy. There it degrades: nothing is
	// assigned, the reason is reported, and the plan runs on the session's model
	// exactly as it did before the setting existed.
	if tool.DiscoverModels == nil {
		if !supplied {
			return []string{"none — this run cannot list the provider's models, so every task kept this session's model"}, routerTokens, nil
		}
		return nil, routerTokens, fmt.Errorf(
			"auto_assign is not available in this run — it needs a provider that can list its models. " +
				"Name a model per task instead, or omit auto_assign to inherit this session's model")
	}
	raw, ok := args["tasks"].([]any)
	if !ok {
		// A saved plan or a malformed one: leave it entirely to ParsePlan, which
		// owns every message about task shape.
		return nil, routerTokens, nil
	}
	// SAY WHAT IS HAPPENING. Everything from here to admission is invisible
	// otherwise, and it is the slowest part of a plan's start.
	planPreflight(tool.Recorder, "listing this provider's models…")
	defer planPreflight(tool.Recorder, "")
	models, err := tool.DiscoverModels(ctx)
	if err != nil {
		if !supplied {
			return []string{"none — could not list the provider's models (" + err.Error() +
				"), so every task kept this session's model"}, routerTokens, nil
		}
		return nil, routerTokens, fmt.Errorf("auto_assign could not list the provider's models: %w", err)
	}
	// PROVED BEFORE ANYTHING IS BUILT FROM IT. Tiers, the served set and the
	// router's candidate list are all derived from this slice, so a model that
	// cannot run has to be gone before any of them exist — filtering later would
	// leave it in the tiers it was already ranked into.
	if tool.ProbeModel != nil {
		planPreflight(tool.Recorder, "checking which models this provider will run…")
	}
	probeIdentity := ""
	if tool.ProbeIdentity != nil {
		probeIdentity = tool.ProbeIdentity()
	}
	models, probeNotes := proveModels(ctx, models, tool.ProbeModel, &tool.probes, probeIdentity)

	tiers := buildModelTiers(models, tool.ModelPrefs)
	served := servedModels(models)

	// THE SESSION'S OWN MODEL MUST BE IN THE LIST, or the list is not this
	// provider's and nothing built from it can be trusted.
	//
	// Discovery and child execution resolve the provider by different routes, and
	// when those routes disagree this is the only place that can notice. A real
	// run proved how bad the disagreement is: the session was xai/grok-4.5, the
	// discovered list was nineteen Ollama models, and because grok-4.5 was absent
	// from it even routerModel's "the session's model comes first" rule rejected
	// the session's model — so the ROUTER ITSELF ran on qwen3.5:397b and the
	// provider refused it. Four tasks died at dispatch on models that never
	// existed for them.
	//
	// Every downstream guard was defeated by the same root fact. The served-set
	// check passed, because the wrong list was internally consistent. Refusing
	// here turns a plan-wide failure into the no-op it should always have been:
	// every task keeps the session's model, which is exactly what it did before
	// this feature existed.
	//
	// Guarded on a NON-EMPTY served set: an empty one means discovery told us
	// nothing, which is the existing fail-open and not evidence of disagreement.
	if parent := strings.TrimSpace(options.Model); parent != "" && len(served) > 0 && !servedContains(served, parent) {
		mismatch := fmt.Sprintf(
			"none — this session runs on %q, which is not among the %d model(s) discovered, "+
				"so that list belongs to a different provider and was ignored "+
				"(every task kept this session's model)", parent, len(models))
		if !supplied {
			return []string{mismatch}, routerTokens, nil
		}
		// Asked for explicitly: the same evidence, raised rather than reported,
		// because a plan that demanded auto-assignment must not quietly not get it.
		return nil, routerTokens, fmt.Errorf("auto_assign refused: %s", mismatch)
	}

	// THE ROUTER READS THE TASKS; the classifier only reads their verbs. It runs
	// once per plan, on the strongest model available, and every way it can fail
	// falls back to the classifier — a routing decision is worth having, never
	// worth stopping a plan for.
	// The router needs a grant only so the child is not refused for holding
	// none; it reads nothing. Parent identity is attached by runnerForCall.
	routerGrant, _ := planToolGrant(Task{}, tool.ParentTools)
	router := routerModel(tool.ModelPrefs, tiers, served, options.Model)
	if strings.TrimSpace(router) != "" && len(routableTasks(raw)) >= routerMinimumTasks {
		planPreflight(tool.Recorder, "asking "+router+" which model each task needs…")
	}
	routed, routerSpent, routeErr := routeTaskModels(ctx, tool.runnerForCall(options), PlanTaskRequest{Tools: routerGrant},
		router, routableTasks(raw), eligibleForRouting(models, tool.ModelPrefs), tool.ModelPrefs.RouterGuidance)
	// THE HOISTED COUNTER, so the number the plan is CHARGED is the same one the
	// headline PRINTS. Two spellings of one quantity is how a report and a budget
	// drift apart — and this one already drifted once, silently, every run.
	routerTokens = routerSpent

	tasks, notes := assignModelsToTaskArgs(raw, tiers, tool.ModelPrefs, served, routed)
	// SAID, NOT SILENT. A model disappearing from routing with no explanation is
	// indistinguishable from a bug, and these are precisely the ids the user has
	// been adding to planModels.exclude by hand after each plan died on one.
	for _, dropped := range probeNotes {
		notes = append(notes, "not offered — "+dropped)
	}
	args["tasks"] = tasks
	if len(routed) > 0 {
		// NAME THE ROUTER. A per-task model that appeared from nowhere is
		// indistinguishable from the classifier's guess, and the whole reason to
		// pay for a routing call is being able to see whose judgement it was.
		// NAME THE PRICE WITH THE NAME. Routing spends a frontier-model call
		// before a single task runs, and it is not part of any task's total —
		// so without this line the plan's reported spend is under its real spend
		// by exactly this much, every run, invisibly.
		headline := "routed by " + router
		if routerTokens > 0 {
			headline += fmt.Sprintf(" (%d tokens)", routerTokens)
		}
		notes = append([]string{headline}, notes...)
	}
	if routeErr != nil {
		// Reported, not raised. The plan ran with classifier routing, and a user
		// who configured a router deserves to know it did not answer.
		notes = append(notes, "router unavailable ("+routeErr.Error()+"); roles chosen from task wording instead")
	}
	if len(notes) == 0 {
		// ASKED FOR AND DID NOTHING IS A RESULT, and it has to say WHICH nothing.
		//
		// One message covered two unrelated causes and blamed the wrong one: a
		// run with nineteen perfectly good models reported "none were usable"
		// when the truth was that no task looked like scan, implement or verify,
		// so nothing was assigned. That sent the reader hunting through provider
		// capabilities for a problem that was in the task wording.
		if tiers == (modelTiers{}) {
			notes = append(notes, fmt.Sprintf(
				"none — %d model(s) discovered, none of them usable for a plan task "+
					"(every task kept this session's model)", len(models)))
		} else {
			notes = append(notes, fmt.Sprintf(
				"none — %d model(s) available, but no task called for a different one "+
					"(every task kept this session's model)", len(models)))
		}
	}
	return notes, routerTokens, nil
}

// planBudgetWarning surfaces a budget that looks an order of magnitude low.
//
// Carried to the OUTPUT rather than raised as an error: the plan may still be
// exactly what the author wanted, and refusing a number that is merely unusual
// would make the tool unusable on cheap plans of small tasks. It is said so the
// next call can be different.
func planBudgetWarning(warning string) string {
	if strings.TrimSpace(warning) == "" {
		return ""
	}
	return "\n\nNote: " + warning + ".\n"
}

// planWorkspaceNote says WHERE a write-capable plan did its work.
//
// PlanWorkspace.Describe exists "for the approval card and the run" and nothing
// read it, so a plan that wrote files reported what it did and never where. The
// work lands in a git worktree that is deliberately NOT removed afterwards —
// "a plan that wrote produced work nobody has reviewed; deleting it would delete
// the only copy" — which makes an unnamed location the difference between work
// the user can review and work they cannot find.
//
// Empty for a read-only plan, which runs in the parent's own tree and has
// nothing to disclose.
func planWorkspaceNote(workspace PlanWorkspace) string {
	if !workspace.Isolated {
		return ""
	}
	where := strings.TrimSpace(workspace.Describe)
	if where == "" {
		where = strings.TrimSpace(workspace.Path)
	}
	if where == "" {
		return ""
	}
	return "\n\nThis plan wrote in an isolated workspace: " + where +
		"\nIt is left in place for review; nothing was written to the parent tree."
}

// autoAssignSummary reports what auto-assignment chose.
//
// REPORTED, not merely applied. A feature that silently changes which model
// each task runs on — and therefore what the plan costs — has to say what it
// did, or the user cannot tell a plan that ran as they expected from one that
// did not. Empty when nothing was assigned, so the untouched path is unchanged.
func autoAssignSummary(notes []string) string {
	if len(notes) == 0 {
		return ""
	}
	return "\n\nModels assigned automatically:\n  " + strings.Join(notes, "\n  ")
}

// resolveTemplatePlan swaps a `template` reference for the arguments it builds.
//
// REFUSED ALONGSIDE tasks OR saved, rather than merged. A template plus
// hand-written tasks is neither the template nor the tasks, and picking one
// silently would run something the caller did not describe — the same reasoning
// that makes resolveSavedPlan refuse a half-overridden saved plan.
func resolveTemplatePlan(args map[string]any) (map[string]any, error) {
	name := planString(args, "template")
	if name == "" {
		return args, nil
	}
	for _, field := range []string{"tasks", "saved"} {
		if _, present := args[field]; present {
			return nil, fmt.Errorf(
				"a template builds the plan for you: remove %q, or drop `template` and supply the plan yourself", field)
		}
	}
	params, err := planParamsFromArgs(args)
	if err != nil {
		return nil, err
	}
	built, err := BuildTemplatePlan(name, params)
	if err != nil {
		return nil, err
	}
	// EXECUTION DIRECTIVES SURVIVE, plan content does not — the same two the
	// saved-plan path carries across, for the same reason: they say HOW to run
	// it, not WHAT to run, and dropping them silently ran a background plan in
	// the foreground once already.
	for _, directive := range []string{"background", "auto_assign"} {
		if value, present := args[directive]; present {
			built[directive] = value
		}
	}
	return built, nil
}

// execOptionsFor builds the execution options for one plan.
//
// ONE BUILDER FOR BOTH CALL SITES. A plan runs in the foreground or the
// background from two different places, and an option wired to one of them makes
// a feature that works or not depending on which the caller happened to pick —
// the shape TestBothExecutionPathsChargeRouterSpend already pins for spend.
func (tool *OrchestrateTool) execOptionsFor(plan Plan, routerTokens int) []ExecOption {
	options := []ExecOption{WithPreSpentTokens(routerTokens)}
	if tool.ContextWindows != nil {
		options = append(options, WithContextWindows(tool.ContextWindows))
	}
	// Called HERE, at dispatch — not captured at construction — so a
	// request_permissions grant that landed earlier this turn is included. Empty
	// is fine: WithReadRoots adds nothing, and the tasks keep their workspace-only
	// reads.
	if tool.ExtraReadRoots != nil {
		if roots := tool.ExtraReadRoots(); len(roots) > 0 {
			options = append(options, WithReadRoots(roots))
		}
	}
	// ONLY WHEN A BRIEFING CAN EXIST. The scratchpad's whole job is to make a
	// TRUNCATED dependency briefing reachable, and a plan whose tasks depend on
	// nothing never writes one — so a directory would be created, populated and
	// deleted for no reader.
	if planHasDependencies(plan) {
		options = append(options, WithScratchpad())
	}
	return options
}

func planHasDependencies(plan Plan) bool {
	for _, task := range plan.Tasks() {
		if len(task.DependsOn) > 0 {
			return true
		}
	}
	return false
}
