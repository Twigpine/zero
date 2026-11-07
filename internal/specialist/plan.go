package specialist

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Twigpine/zero/internal/config"
)

// ZeroMaxing plan capture: a validated dependency graph, executed CONCURRENTLY.
//
// A plan declares a dependency graph and the executor runs it in topological
// order, up to budget.max_workers tasks at a time (maxPlanWorkers = 16). Tasks
// may be granted write tools by name, in which case the plan runs in an isolated
// git worktree — see RequiresIsolation and resolvePlanWorkspace.
//
// THE MEASUREMENT SURVIVED THE GATE IT WAS BUILT FOR. This header used to read
// "executed SEQUENTIALLY … one task at a time … if the recorded max_speedup says
// fan-out would not have paid for itself, Phase 3 is not built". That was a
// genuine evidence gate, and it was PASSED: a five-task diamond measured 40.0s
// at one worker against 27.0s at four (4643666), so concurrency was built —
// the gate did its job and opened. PlanReport.MaxSpeedup is still
// recorded and still reported — the number that justified fan-out is the same
// number that now says whether any given plan was worth fanning out — but it is
// no longer deciding whether to build it.
//
// Fan-out, pipeline and phase are ONE structure: fan-out is tasks with no
// dependencies, a pipeline is a chain, a phase is a label plus a barrier. The
// prototype grew five separate hooks for these and never invoked any of them.

// Plan is a validated, executable plan.
//
// Its fields are UNEXPORTED and ParsePlan is the only constructor. That is not
// stylistic: it makes skipping validation impossible rather than merely wrong.
// The prototype's validator was reachable, correct, and never called from the
// production path; a type that cannot be constructed any other way removes the
// choice.
type Plan struct {
	name        string
	description string
	tasks       []Task
	budget      Budget
	// order is the topological order Kahn's algorithm emitted during
	// validation. The executor reuses it rather than recomputing, so admission
	// and execution cannot disagree about the order or about acyclicity.
	order []string
}

// Task is one unit of a plan.
type Task struct {
	ID        string
	Prompt    string
	DependsOn []string
	// Tools is the read-only subset this task may use. Empty means "the parent's
	// full read-only grant". It can only ever NARROW: validated here and
	// intersected again at dispatch, so a validator bug cannot widen authority.
	Tools []string
	// Phase is a display and ordering label only. It carries no execution
	// semantics in Phase 2 — a barrier is expressible as dependencies.
	Phase string
	// Model is the CANONICAL registry id this task runs on, empty to inherit the
	// parent's. Stored canonical rather than as the caller wrote it, because the
	// id is what reaches the child's argv — see resolveTaskModel.
	Model string
}

// Budget bounds a plan. Every field is required to be sane at admission and
// MaxTokens is enforced again at dispatch.
type Budget struct {
	// MaxWorkers is how many tasks the plan may run at once, 1 to maxPlanWorkers.
	//
	// It is what the PLAN asked for, not what it will get: the machine's own
	// capacity may be lower, and the report says which number actually applied
	// rather than letting the request stand as if it were honoured.
	MaxWorkers int
	MaxTokens  int
	// MaxTokensPerTask bounds what ONE task may spend. Optional; 0 is unbounded,
	// which is every plan that does not ask for a cap.
	//
	// A PLAN-LEVEL BUDGET CANNOT STOP ONE TASK FROM EATING EVERYTHING, and that
	// is not theoretical: in a measured six-task plan against 200,000 tokens, a
	// single task spent 1,017,177 — five times the whole plan's budget — and the
	// cheapest one that finished spent 510,017. Nothing bounded them, because the
	// plan's own limit is only consulted between tasks.
	//
	// Kept separate from max_tokens rather than derived from it. A derived cap
	// (budget ÷ remaining tasks) changes the economics of every existing plan
	// without anyone choosing it, which is the same argument this codebase
	// already makes for auto_assign being opt-in.
	MaxTokensPerTask int
	MaxWall          time.Duration
	// MaxStall bounds how long a single task may emit NOTHING before it is
	// stopped. Distinct from MaxWall, which bounds the whole plan: a plan can
	// sit inside its wall budget while one task is wedged and the rest never
	// run. Zero means the default.
	MaxStall time.Duration
	// MaxRetries is how many EXTRA attempts a STALLED task gets. Resolved to its
	// effective value at parse time — 0 here means no retries, and an unset
	// max_retries has already become the default by the time anything reads it,
	// so nothing downstream re-derives it and 0 can never be mistaken for unset.
	//
	// Only stalls are retried. See runTaskWithRetries.
	MaxRetries int
}

// Limits are the caller-supplied hard caps a plan must fit inside.
type Limits struct {
	// MaxTasks bounds plan size. 0 means no bound.
	MaxTasks int
	// MaxTasksSource LABELS where MaxTasks came from, for the rejection message
	// only — "the \"medium\" plan size". Deliberately a phrase and not a second
	// number: a label cannot contradict the bound it describes, whereas a second
	// copy of the count would eventually disagree with the one being enforced
	// (invariant 5). Empty renders a generic message.
	MaxTasksSource string
	// MaxTokens is the ceiling a plan's own budget may not exceed.
	MaxTokens int
	// ParentTools is the grant the parent run holds. A task's Tools must be a
	// subset; anything outside it is rejected.
	//
	// EMPTY MEANS EMPTY, not "unset". The intersection below is unconditional:
	// a caller that does not supply this grants nothing, and every task is
	// rejected. That is deliberate — the previous "skip the check when the
	// list is empty" escape hatch made the rule inert at both production call
	// sites, which supplied no grant at all, and a narrower parent produced a
	// wider child. Fail closed (invariant 3): an unsupplied grant is a wiring
	// bug, and the run must stop rather than assume authority.
	ParentTools []string
	// CurrentDepth is the depth of the run issuing the plan. Its tasks run one
	// level deeper, so the check is against maxSpecialistDepth.
	CurrentDepth int
}

// planIDPattern is an ALLOW-LIST. Enumerating permitted characters rather than
// forbidden ones is this repo's standing rule for classification: every
// deny-list here has leaked (git -C, then git -c, then --exec-path).
var planIDPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// writeToolMarkers name the mutating capabilities Phase 2 tasks may not have.
// Matched as an allow-list would be inverted, so this list is checked AGAINST a
// read-only allow-list below rather than used as the primary gate.
var planReadOnlyTools = map[string]bool{
	"read_file":          true,
	"read_minified_file": true,
	"list_directory":     true,
	"grep":               true,
	"glob":               true,
	"lsp_navigate":       true,
	// update_plan is DELIBERATELY ABSENT, and it was here.
	//
	// TWO REASONS, and the second was measured rather than reasoned.
	//
	// It is not read-only. It REPLACES the parent's whole plan list on every
	// call, so N tasks each calling it race and the last one wins — and a plan
	// task is an investigator that reports to the parent, which owns the list.
	// Nothing about the job needs it.
	//
	// And it was the single reason a plan child carried the confirmation
	// policy. update_plan declares readOnlySafety but sets no capabilities, so
	// CapabilitiesOf reports EffectUnknown — the fail-closed default — while its
	// safety says read-only. runCanMutate reads the CAPABILITY, so one
	// unclassified tool in an otherwise read-only grant made the whole child
	// look mutating, and ~2,500 tokens of policy it can never need rode along on
	// every task of every plan.
	//
	// That mismatch is a PRE-EXISTING MAIN DEFECT and is filed as one rather
	// than fixed here: the same capability drives concurrency eligibility, and
	// promoting update_plan to EffectReadOnly would make it eligible for
	// parallel execution it is not safe for. Removing it from this grant fixes
	// the plan path without touching a classification that means something else
	// somewhere else.
}

// Name, Description, Tasks and Budget expose a validated plan for execution and
// reporting. Read-only accessors: a Plan cannot be mutated after ParsePlan.
func (p Plan) Name() string        { return p.name }
func (p Plan) Description() string { return p.description }
func (p Plan) Budget() Budget      { return p.budget }

// Tasks returns a copy so a caller cannot mutate a validated plan.
func (p Plan) Tasks() []Task {
	out := make([]Task, len(p.tasks))
	for index, task := range p.tasks {
		// DEEP, because copy() duplicates the structs and shares their slices.
		// Task.Tools is the VALIDATED GRANT: with the backing array shared,
		// plan.Tasks()[0].Tools[0] = "bash" rewrote the admitted plan in place,
		// from outside, after every check had passed — the widening this whole
		// file exists to make impossible. DependsOn is the same hazard aimed at
		// execution order rather than authority.
		task.DependsOn = append([]string(nil), task.DependsOn...)
		task.Tools = append([]string(nil), task.Tools...)
		out[index] = task
	}
	return out
}

// Order returns the validated topological order.
func (p Plan) Order() []string {
	out := make([]string, len(p.order))
	copy(out, p.order)
	return out
}

// TaskCount is THE counting function. Both admission and the executor call it.
//
// It is a length, not a text scan. The prototype counted source text —
// strings.Count(body, "agent(") in admission and a regex in the compiler — so
// `agent ("x")` with one space counted as zero and executed anyway. Counting a
// parsed structure removes the class rather than fixing the regex.
func (p Plan) TaskCount() int { return len(p.tasks) }

// Args renders a validated plan back into the argument shape ParsePlan accepts.
//
// THE ROUND TRIP IS THE POINT. Saving a plan means saving something that can be
// run again, and the only thing that can be run is what ParsePlan admits — so a
// saved plan is stored as ARGS and re-admitted on load, rather than as a
// serialised Plan that would enter execution having skipped the one constructor
// that validates. The prototype's validator was reachable, correct and never
// called on the production path; a stored object that deserialises straight into
// an executable is the same hole with a filesystem in front of it.
//
// Budget fields that were resolved to defaults are written as the values in
// force, so a plan saved today runs the same way after a default changes.
func (p Plan) Args() map[string]any {
	tasks := make([]any, 0, len(p.tasks))
	for _, task := range p.tasks {
		entry := map[string]any{"id": task.ID, "prompt": task.Prompt}
		if len(task.DependsOn) > 0 {
			entry["depends_on"] = stringsToAny(task.DependsOn)
		}
		if len(task.Tools) > 0 {
			entry["tools"] = stringsToAny(task.Tools)
		}
		if task.Phase != "" {
			entry["phase"] = task.Phase
		}
		// EMITTED HERE OR LOST ENTIRELY. Args() is the round trip behind
		// /plans save, /plans show, resume and the staged _resume plan; a model
		// on the struct but not in this map means the plan reruns on the parent's
		// model with nothing anywhere reporting the change.
		if task.Model != "" {
			entry["model"] = task.Model
		}
		tasks = append(tasks, entry)
	}
	budget := map[string]any{
		"max_workers": p.budget.MaxWorkers,
		"max_retries": p.budget.MaxRetries,
	}
	if p.budget.MaxTokens > 0 {
		budget["max_tokens"] = p.budget.MaxTokens
	}
	if p.budget.MaxTokensPerTask > 0 {
		budget["max_tokens_per_task"] = p.budget.MaxTokensPerTask
	}
	if p.budget.MaxWall > 0 {
		budget["max_wall_seconds"] = int(p.budget.MaxWall.Seconds())
	}
	if p.budget.MaxStall > 0 {
		budget["max_stall_seconds"] = int(p.budget.MaxStall.Seconds())
	}
	args := map[string]any{"tasks": tasks, "budget": budget}
	if p.name != "" {
		args["name"] = p.name
	}
	if p.description != "" {
		args["description"] = p.description
	}
	return args
}

func stringsToAny(in []string) []any {
	out := make([]any, len(in))
	for i, s := range in {
		out[i] = s
	}
	return out
}

// ParsePlan is the ONLY way to obtain a Plan. It parses and validates in one
// step; there is no path from tool arguments to an executable plan that skips
// it. Every check rejects by default.
func ParsePlan(args map[string]any, limits Limits) (Plan, error) {
	plan := Plan{
		name:        planString(args, "name"),
		description: planString(args, "description"),
	}

	rawTasks, err := planTaskList(args)
	if err != nil {
		return Plan{}, err
	}
	if len(rawTasks) == 0 {
		return Plan{}, fmt.Errorf("plan requires at least one task")
	}
	if limits.MaxTasks > 0 && len(rawTasks) > limits.MaxTasks {
		return Plan{}, planTooLargeError(len(rawTasks), limits)
	}

	// DEPTH, at admission. A plan runs inside a child at limits.CurrentDepth, so
	// its tasks are one level deeper. Checking here — with the remaining
	// headroom named — turns an opaque mid-plan failure into a rejection the
	// caller can act on.
	taskDepth := limits.CurrentDepth + 1
	if taskDepth >= maxSpecialistDepth {
		return Plan{}, fmt.Errorf(
			"a plan's tasks would run at depth %d, which reaches the maximum nesting depth of %d; this run is already at depth %d, leaving no headroom for plan tasks",
			taskDepth, maxSpecialistDepth, limits.CurrentDepth)
	}

	seen := map[string]bool{}
	// UNIQUE IGNORING CASE, too. A task id becomes the scratchpad filename, and
	// on a case-insensitive filesystem "Alpha" and "alpha" are one file — one
	// task's answer silently served as the other's. They would also read as the
	// same task to a person scanning the event log or the plan panel, so the
	// plan is refused rather than the ids quietly disambiguated.
	folded := map[string]string{}
	for index, raw := range rawTasks {
		task, err := planTask(raw, index)
		if err != nil {
			return Plan{}, err
		}
		if seen[task.ID] {
			return Plan{}, fmt.Errorf("task id %q appears more than once; ids must be unique", task.ID)
		}
		if prior, clash := folded[strings.ToLower(task.ID)]; clash {
			return Plan{}, fmt.Errorf("task ids %q and %q differ only in case; ids must be unique ignoring case", prior, task.ID)
		}
		seen[task.ID] = true
		folded[strings.ToLower(task.ID)] = task.ID
		if err := validateTaskTools(task, limits); err != nil {
			return Plan{}, err
		}
		plan.tasks = append(plan.tasks, task)
	}

	// Every DependsOn must resolve. An unknown edge is REJECTED, never skipped:
	// skipping it would silently execute a task whose stated precondition never
	// ran, which is worse than refusing the plan.
	for _, task := range plan.tasks {
		for _, dep := range task.DependsOn {
			if !seen[dep] {
				return Plan{}, fmt.Errorf("task %q depends on %q, which is not a task in this plan", task.ID, dep)
			}
			if dep == task.ID {
				return Plan{}, fmt.Errorf("task %q depends on itself", task.ID)
			}
		}
	}

	order, cycle := topologicalOrder(plan.tasks)
	if cycle != nil {
		return Plan{}, fmt.Errorf("plan has a dependency cycle involving: %s", strings.Join(cycle, ", "))
	}
	plan.order = order

	budget, err := planBudget(args, limits)
	if err != nil {
		return Plan{}, err
	}
	if err := refuseImplausibleBudget(budget, len(plan.tasks), limits); err != nil {
		return Plan{}, err
	}
	if err := refuseUnreachablePerTaskCap(budget, len(plan.tasks)); err != nil {
		return Plan{}, err
	}
	plan.budget = budget
	return plan, nil
}

// minimumPlausibleTaskTokens is the floor below which a per-task share of the
// budget cannot buy a task at all.
//
// DELIBERATELY AN ORDER OF MAGNITUDE BELOW WHAT ANYTHING REALLY COSTS. In a
// measured six-task plan the CHEAPEST task that completed spent 510,017 tokens;
// the dearest spent 1,017,177. This is not a typical cost and must never be read
// as one — it is the point below which the arithmetic is impossible, so the only
// plans it can refuse are plans that were never going to finish.
//
// A task pays for its whole prompt on every turn, so even a single lookup
// answering in two turns costs tens of thousands. 50k buys a very small task and
// nothing more.
const minimumPlausibleTaskTokens = 50_000

// typicalHeavyTaskTokens is what a task that reads and traces a large repo
// actually costs, measured across several runs: 510,017 for the cheapest that
// finished, 1,017,177 for the dearest, and finders repeatedly cut short between
// 700,000 and 790,000 while still working.
//
// SET AT THE TOP OF THAT RANGE, not the middle. This number only decides when to
// WARN, and the two errors are not equal: warning on a plan that would have been
// fine costs a sentence the author can ignore, while staying quiet on a plan
// that cannot finish costs the entire run — which has now happened four times.
//
// It is NOT a floor and nothing is refused for being below it. It is the number
// that tells a plan author their budget is an order of magnitude out while they
// can still change it — the gap the floor deliberately cannot cover, because a
// floor strict enough to catch this would refuse legitimate plans of small
// tasks.
const typicalHeavyTaskTokens = 1_000_000

// typicalDownstreamTaskTokens is what a task that works from its dependencies'
// findings costs, measured: 62,783 for a sweep and 86,986 for a verify in the
// same plan whose finders were spending 136,000 to 200,000 apiece.
//
// AN ORDER OF MAGNITUDE CHEAPER, because it reads a briefing rather than a
// repository. Estimating every task at the heavy figure warned on plans that
// were correctly sized, which teaches an author to ignore the warning — the one
// outcome worse than not having it.
const typicalDownstreamTaskTokens = 150_000

// refuseUnreachablePerTaskCap rejects a per-task cap the plan's own total can
// never let a task reach.
//
// THE TWO NUMBERS FIGHT AND THE SMALLER ONE ALWAYS WINS, silently. A run asked
// for 1,000,000 per task with a 500,000 total across seven tasks: every task was
// bounded by its share of the total long before its own cap mattered, so the cap
// the author set had no effect at all. Four finders were stopped between 6,688
// and 219,698 tokens — nowhere near the million they had been given — and the
// author reasonably concluded the per-task limit was broken. It was not; it was
// unreachable.
//
// Refused rather than resolved, because either resolution would be a lie: taking
// the total silently ignores the cap, and taking the cap silently spends past a
// total the author set. Only they can say which they meant.
func refuseUnreachablePerTaskCap(budget Budget, taskCount int) error {
	if budget.MaxTokens <= 0 || budget.MaxTokensPerTask <= 0 || taskCount <= 0 {
		return nil
	}
	// OVERFLOW FAILS OPEN, so it is ruled on first — the same hazard
	// refuseUnrepresentableSeconds closes for timeouts. taskCount * cap past
	// math.MaxInt wraps NEGATIVE, and every max_tokens is >= a negative number,
	// so the plan that needed the most was the one waved through. A product
	// that does not fit in an int exceeds every representable max_tokens, so it
	// is the unreachable case by definition.
	if budget.MaxTokensPerTask > math.MaxInt/taskCount {
		return fmt.Errorf(
			"budget.max_tokens is %d but max_tokens_per_task is %d across %d tasks, which needs more than %d — "+
				"every task would be stopped by its share of the total long before its own cap applied, "+
				"so the cap would do nothing. Lower max_tokens_per_task, or OMIT max_tokens and let "+
				"max_tokens_per_task bound each task on its own",
			budget.MaxTokens, budget.MaxTokensPerTask, taskCount, math.MaxInt)
	}
	needed := taskCount * budget.MaxTokensPerTask
	if budget.MaxTokens >= needed {
		return nil
	}
	return fmt.Errorf(
		"budget.max_tokens is %d but max_tokens_per_task is %d across %d tasks, which needs %d — "+
			"every task would be stopped by its share of the total long before its own cap applied, "+
			"so the cap would do nothing. Raise max_tokens to at least %d, or OMIT it and let "+
			"max_tokens_per_task bound each task on its own",
		budget.MaxTokens, budget.MaxTokensPerTask, taskCount, needed, needed)
}

// likelyPlanTokens estimates a plan's cost, weighting each task by whether it
// reads a repository or reads its dependencies' findings.
func likelyPlanTokens(tasks []Task) int {
	likely := 0
	for _, task := range tasks {
		if len(task.DependsOn) > 0 {
			likely += typicalDownstreamTaskTokens
			continue
		}
		likely += typicalHeavyTaskTokens
	}
	return likely
}

// warnBudgetLooksLow returns a warning when a plan's budget is far below what
// its task count usually costs, or "" when it is not.
//
// THE FLOOR CATCHES THE IMPOSSIBLE; THIS CATCHES THE MERELY WRONG, and the band
// between them is where every real failure has been. A seven-task audit was
// admitted with 500,000 tokens — over the 350,000 floor, and about an eighth of
// what those tasks went on to spend. Four finders consumed it between them, the
// verify, sweep and synthesis tasks never ran, and the run produced no report at
// all. Nothing anywhere told the author the number was wrong until it was.
func warnBudgetLooksLow(budget Budget, tasks []Task) string {
	taskCount := len(tasks)
	if budget.MaxTokens <= 0 || taskCount <= 0 {
		return ""
	}
	// WEIGHTED BY POSITION, via the same estimator the refusal uses: two
	// estimates of one number would eventually disagree about whether a plan is
	// tight or hopeless.
	likely := likelyPlanTokens(tasks)
	if budget.MaxTokens >= likely {
		return ""
	}
	return fmt.Sprintf(
		"budget.max_tokens is %d for %d tasks. A task that reads and traces a repo has measured 510k-1,017k, and one "+
			"working from its dependencies' findings far less, so this plan may need nearer %d. Below that, tasks are "+
			"stopped mid-run and later ones may never start — omit max_tokens to run unbounded within this run's own "+
			"ceiling if you cannot estimate it",
		budget.MaxTokens, taskCount, likely)
}

// refuseImplausibleBudget rejects a plan whose budget cannot cover its own tasks,
// BEFORE anything is spent.
//
// Neither of the two things that happen without it is a good outcome. A measured
// run admitted six tasks against 200,000 tokens, spent 3,091,618 — fifteen times
// over — and still returned an incomplete answer, because the two tasks that had
// not started yet were skipped once the meter finally caught up. The user paid
// for the overrun AND lost a third of the audit.
//
// Killing a task mid-flight (the other repair) at least stops the bleeding, but
// bills for everything spent before the knife. Refusing here costs nothing at
// all, and is the only response that can still produce a COMPLETE answer: the
// caller raises the number or asks for fewer tasks and runs a plan that finishes.
//
// Silent on an unset budget. Unbounded is a deliberate choice — see planBudget —
// and this must not turn it back into a required field by the back door.
func refuseImplausibleBudget(budget Budget, taskCount int, limits Limits) error {
	if budget.MaxTokens <= 0 || taskCount <= 0 {
		return nil
	}
	needed := taskCount * minimumPlausibleTaskTokens
	if budget.MaxTokens >= needed {
		return nil
	}
	// THE ADVICE MUST BE FOLLOWABLE. When this run's own ceiling is below what
	// the plan would need, "raise max_tokens" is advice the next call cannot
	// take — the ceiling refuses it — and the caller loops between two errors
	// that each blame the other. The plan is simply too big for this run, and
	// saying so is the only thing that ends the loop.
	if limits.MaxTokens > 0 && limits.MaxTokens < needed {
		// "Ask for at most 0 tasks" is not advice. When the run's ceiling cannot
		// fund even one task, no plan fits and the caller needs to hear that
		// rather than a number to aim at.
		if affordable := limits.MaxTokens / minimumPlausibleTaskTokens; affordable >= 1 {
			return fmt.Errorf(
				"%d tasks need about %d tokens and this run is capped at %d — the plan is too big for this run. "+
					"Ask for at most %d task(s), or split the work across runs",
				taskCount, needed, limits.MaxTokens, affordable)
		}
		return fmt.Errorf(
			"this run is capped at %d tokens, which cannot fund a single plan task (they rarely cost less than %d). "+
				"Do the work directly instead of planning it, or raise this run's ceiling",
			limits.MaxTokens, minimumPlausibleTaskTokens)
	}
	return fmt.Errorf(
		"budget.max_tokens is %d for %d tasks — about %d per task, and a plan task rarely costs less than %d "+
			"(a measured six-task plan spent 3,091,618). Raise max_tokens to at least %d, or ask for fewer tasks. "+
			"Omitting max_tokens runs unbounded within this run's own ceiling",
		budget.MaxTokens, taskCount, budget.MaxTokens/taskCount, minimumPlausibleTaskTokens, needed)
}

// topologicalOrder runs Kahn's algorithm. It returns the emitted order, or the
// ids still carrying unmet dependencies when the queue drains early — which is
// exactly the set involved in a cycle.
//
// There was no cycle detection anywhere in this tree to reuse. Audit U24: a
// cyclic page tree hangs forever precisely because nothing checks.
func topologicalOrder(tasks []Task) (order []string, cycle []string) {
	indegree := map[string]int{}
	dependents := map[string][]string{}
	for _, task := range tasks {
		if _, ok := indegree[task.ID]; !ok {
			indegree[task.ID] = 0
		}
		for _, dep := range task.DependsOn {
			indegree[task.ID]++
			dependents[dep] = append(dependents[dep], task.ID)
		}
	}

	// Seed with every zero-indegree node, in declaration order so the emitted
	// order is deterministic for a given plan.
	ready := []string{}
	for _, task := range tasks {
		if indegree[task.ID] == 0 {
			ready = append(ready, task.ID)
		}
	}
	for len(ready) > 0 {
		id := ready[0]
		ready = ready[1:]
		order = append(order, id)
		next := append([]string(nil), dependents[id]...)
		sort.Strings(next)
		for _, dependent := range next {
			indegree[dependent]--
			if indegree[dependent] == 0 {
				ready = append(ready, dependent)
			}
		}
	}
	if len(order) == len(tasks) {
		return order, nil
	}
	// The queue drained early: everything still carrying an indegree is part of
	// a cycle or downstream of one. Name them — an unnamed "cycle detected" is
	// not actionable on a twenty-task plan.
	for _, task := range tasks {
		if indegree[task.ID] > 0 {
			cycle = append(cycle, task.ID)
		}
	}
	sort.Strings(cycle)
	return nil, cycle
}

// validateTaskTools enforces both Phase 2 rules: read-only, and never wider
// than the parent's grant. Enforced AGAIN at dispatch — see planToolGrant.
func validateTaskTools(task Task, limits Limits) error {
	parent := map[string]bool{}
	for _, name := range limits.ParentTools {
		parent[name] = true
	}
	for _, name := range task.Tools {
		// A TASK MAY NOW NAME A WRITE TOOL, and only by naming it.
		//
		// The read-only allow-list used to be the whole rule. It is now the
		// DEFAULT — a task that names nothing still inherits read-only tools and
		// nothing else (planToolGrant) — and a task that wants to change
		// something has to say which tool, by name. Writing is opted into per
		// task, never inherited.
		//
		// What still bounds it: the tool must be on the GRANTABLE allow-list —
		// read-only, or one of the few write tools a plan may name — and it must
		// be one the PARENT holds (below). A plan containing any such task
		// cannot run outside an isolated worktree (Plan.RequiresIsolation) or
		// without an approval that shows it (PermissionForArgs). Those two are
		// why this line could be relaxed at all.
		if !planReadOnlyTools[name] && !planWriteTools[name] {
			return fmt.Errorf("task %q requests tool %q, which a plan task may never hold; it may use %s, or name one of %s to write",
				task.ID, name, strings.Join(sortedReadOnlyTools(), ", "), strings.Join(PlanWriteToolNames(), ", "))
		}
		// UNCONDITIONAL. Guarding this on len(limits.ParentTools) > 0 is what
		// made the rule inert: neither production call site supplied a grant,
		// so the check never ran and a task could name any read-only tool the
		// parent did not hold.
		if !parent[name] {
			return fmt.Errorf("task %q requests tool %q, which this run does not hold; a task may narrow the parent's grant, never widen it",
				task.ID, name)
		}
	}
	return nil
}

// planTooLargeError names the ceiling AND how to move it.
//
// The old message was "plan has 24 tasks, which exceeds the limit of 20" — a
// number with no origin and no remedy, so the only way to act on it was to read
// the source. The ceiling is configurable now, and a bound the user cannot
// discover is a bound they will work around by splitting the plan instead.
func planTooLargeError(count int, limits Limits) error {
	source := strings.TrimSpace(limits.MaxTasksSource)
	if source == "" {
		return fmt.Errorf("plan has %d tasks, which exceeds the limit of %d", count, limits.MaxTasks)
	}
	return fmt.Errorf(
		"plan has %d tasks, which exceeds the limit of %d set by %s; raise it with \"profiles\": {\"planSize\": \"%s\"} in .zero/config.json, or split the plan",
		count, limits.MaxTasks, source, config.PlanSizeLarge)
}

func sortedReadOnlyTools() []string {
	names := make([]string, 0, len(planReadOnlyTools))
	for name := range planReadOnlyTools {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// PlanWriteToolNames is the sorted set of MUTATING tools a plan task may hold,
// and only ever by naming one explicitly.
//
// An ALLOW-LIST, deliberately, and a short one. The alternative — "anything the
// parent holds that is not read-only" — would hand a plan task every future
// tool the moment it is registered, including ones nobody considered when this
// was written. Every deny-list in this repository has leaked.
//
// Kept narrow on purpose: editing files and running commands is what a
// write-capable plan is for. Network, browser and process-retention tools are
// not on it, and adding one is a decision, not a configuration.
func PlanWriteToolNames() []string {
	names := make([]string, 0, len(planWriteTools))
	for name := range planWriteTools {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

var planWriteTools = map[string]bool{
	"write_file":   true,
	"edit_file":    true,
	"apply_patch":  true,
	"bash":         true,
	"exec_command": true,
}

// PlanGrantableToolNames is every tool a plan task may hold by any route: the
// read-only default plus the write tools that must be named. ONE list for the
// caller building a parent grant, so the grant and the validator cannot come to
// disagree about what is grantable.
func PlanGrantableToolNames() []string {
	names := append(PlanReadOnlyToolNames(), PlanWriteToolNames()...)
	sort.Strings(names)
	return names
}

// PlanReadOnlyToolNames is the sorted set of tools a plan task may ever hold.
//
// Exported so a caller building Limits.ParentTools intersects against the SAME
// list this package validates against, rather than maintaining its own copy —
// two duplicated lists drift (invariant 5). Nothing outside this set can be
// granted, so a caller need only consider these names.
func PlanReadOnlyToolNames() []string { return sortedReadOnlyTools() }

// DecodePlanBudget decodes a plan call's budget exactly as admission will, for a
// surface that must show what a plan asks for BEFORE it runs — the approval
// card. A second, laxer decoder there once read a numeric-string max_tokens as
// absent and told the user "no token limit" about a plan that would enforce
// one. No run limits are applied: the card shows the request, and admission
// still judges it against the run's ceiling.
func DecodePlanBudget(args map[string]any) (Budget, error) {
	return planBudget(args, Limits{})
}

// PlanRunsInBackground reports whether a plan call asks to run in the
// background, read the way the tool reads it: a top-level boolean.
func PlanRunsInBackground(args map[string]any) bool {
	return planBool(args, "background")
}

func planBudget(args map[string]any, limits Limits) (Budget, error) {
	raw, ok := args["budget"].(map[string]any)
	if !ok {
		return Budget{}, fmt.Errorf("plan requires a budget object with max_workers")
	}
	// EVERY INTEGER FIELD IS DECODED ONCE, by planIntField, before any rule
	// reads it. A present value that is not a whole number is refused here
	// rather than truncated or read as absent: max_wall_seconds 0.9 truncated
	// to 0 and disabled the wall bound, and max_tokens "500000" read as absent
	// and erased the very spending limit it was sent to set.
	ints := map[string]int{}
	present := map[string]bool{}
	for _, key := range planBudgetIntFields {
		value, set, err := planIntField(raw, key)
		if err != nil {
			return Budget{}, fmt.Errorf("budget.%w", err)
		}
		ints[key], present[key] = value, set
	}
	budget := Budget{
		MaxWorkers:       ints["max_workers"],
		MaxTokens:        ints["max_tokens"],
		MaxTokensPerTask: ints["max_tokens_per_task"],
	}
	// Rejected rather than read as "unset". An absent key and a present-but-
	// negative one both used to decode to 0, so a bare `seconds > 0` let a
	// model-supplied -60 vanish and the plan run unbounded with no error. Every
	// other numeric here refuses a negative — max_retries and max_tokens
	// explicitly, max_stall_seconds even refuses positive values under its floor
	// — so silently accepting this one was the odd case out, and the failure is
	// the worst kind: a budget the caller asked for that is not applied.
	if seconds, set := ints["max_wall_seconds"], present["max_wall_seconds"]; set {
		if seconds < 0 {
			return Budget{}, fmt.Errorf("budget.max_wall_seconds must not be negative; %d was requested", seconds)
		}
		if err := refuseUnrepresentableSeconds("max_wall_seconds", seconds); err != nil {
			return Budget{}, err
		}
		if seconds > 0 {
			budget.MaxWall = time.Duration(seconds) * time.Second
		}
	}
	if seconds, set := ints["max_stall_seconds"], present["max_stall_seconds"]; set {
		if seconds < 0 {
			return Budget{}, fmt.Errorf("budget.max_stall_seconds must not be negative; %d was requested", seconds)
		}
		if err := refuseUnrepresentableSeconds("max_stall_seconds", seconds); err != nil {
			return Budget{}, err
		}
		stall := time.Duration(seconds) * time.Second
		if seconds > 0 && stall < minStallTimeout {
			return Budget{}, fmt.Errorf(
				"budget.max_stall_seconds must be at least %d: below that the watchdog fires on ordinary think-time and becomes a random task-killer",
				int(minStallTimeout.Seconds()))
		}
		if seconds > 0 {
			budget.MaxStall = stall
		}
	}
	// max_retries needs PRESENCE, not a value: an explicit 0 means "do not retry
	// this plan" and an absent key means "use the default", and the value alone
	// cannot tell them apart — both are 0. An unset value that reads as an
	// explicit one is invariant 2, the defect that made an empty tool grant
	// expand to the full read-only category.
	budget.MaxRetries = defaultPlanRetries
	if retries, set := ints["max_retries"], present["max_retries"]; set {
		if retries < 0 || retries > maxPlanRetries {
			return Budget{}, fmt.Errorf(
				"budget.max_retries must be between 0 and %d; a task that stalls repeatedly is a provider or network condition, not something more attempts will fix",
				maxPlanRetries)
		}
		budget.MaxRetries = retries
	}
	// MaxWorkers is a RANGE now, and still rejected rather than coerced outside
	// it. The rule it replaces — "must be exactly 1" — existed because the
	// executor was sequential and a caller that asked for 8 and silently got 1
	// would have been told nothing. That reasoning is unchanged; only the
	// executor changed, so the bound moved rather than dissolved.
	//
	// The ceiling is absolute and small. A plan is one tool call, and every task
	// it runs is a child process inheriting a 480-turn budget under this
	// posture; sixteen of those at once is already a great deal of machine and
	// a great deal of money. A plan asking for more is refused, not trimmed,
	// because a trimmed number is a number nobody can reason about afterwards.
	if budget.MaxWorkers < 1 || budget.MaxWorkers > maxPlanWorkers {
		return Budget{}, fmt.Errorf(
			"budget.max_workers must be between 1 and %d; %d was requested. "+
				"1 runs the plan sequentially, which is the right answer unless its tasks are genuinely independent",
			maxPlanWorkers, budget.MaxWorkers)
	}
	// max_tokens is OPTIONAL and unbounded by default.
	//
	// It was required, and capped at 200k. Both went, because the bound did not
	// work and got in the way of real work. It was checked only BETWEEN tasks:
	// a task was dispatched whenever any budget remained and then spent
	// whatever it spent, so a six-task chain asking for the 200k maximum
	// actually spent 469,555 — 2.3x over — and the last task was cut anyway.
	// A number that neither bounds spend nor lets a heavy plan finish is worse
	// than no number, because it reads like a guarantee.
	//
	// Spend is still METERED and reported (PlanReport.TokensUsed, the
	// plan_completed event, the panel), so what a plan cost is always visible.
	// A caller that wants a bound sets max_tokens and gets the same
	// dispatch-time behaviour as before.
	if budget.MaxTokens < 0 {
		return Budget{}, fmt.Errorf("budget.max_tokens must not be negative")
	}
	if limits.MaxTokens > 0 && budget.MaxTokens > limits.MaxTokens {
		return Budget{}, fmt.Errorf("budget.max_tokens %d exceeds the limit of %d for this run", budget.MaxTokens, limits.MaxTokens)
	}
	if budget.MaxTokensPerTask < 0 {
		return Budget{}, fmt.Errorf("budget.max_tokens_per_task must not be negative")
	}
	// A CAP BELOW WHAT ANY TASK COSTS KILLS EVERY TASK, and the plan pays in
	// full for nothing. Measured: a plan capping tasks at 200,000 lost all six
	// between 213k and 259k and cost 1,437,049 tokens with no completed work.
	//
	// Checked against the same floor as the plan budget, and for the same reason
	// — it is the point below which the arithmetic is impossible, not a typical
	// cost. A cap above it can still be tight; that is what the landing-strip
	// notice in withTokenBudgetNotice is for.
	if budget.MaxTokensPerTask > 0 && budget.MaxTokensPerTask < minimumPlausibleTaskTokens {
		return Budget{}, fmt.Errorf(
			"budget.max_tokens_per_task is %d, and a plan task rarely costs less than %d — every task would be "+
				"cut short and the plan would pay for all of them and finish nothing. Raise it, or omit it to leave tasks unbounded",
			budget.MaxTokensPerTask, minimumPlausibleTaskTokens)
	}
	// A per-task cap ABOVE the plan's own budget bounds nothing — the plan limit
	// would always bite first — and reads like a guarantee that no task will
	// exceed it. Refused rather than clamped, for the same reason max_workers is:
	// a trimmed number is one nobody can reason about afterwards.
	if budget.MaxTokens > 0 && budget.MaxTokensPerTask > budget.MaxTokens {
		return Budget{}, fmt.Errorf(
			"budget.max_tokens_per_task %d exceeds budget.max_tokens %d — a per-task cap above the plan's own budget bounds nothing",
			budget.MaxTokensPerTask, budget.MaxTokens)
	}
	return budget, nil
}

func planTask(raw any, index int) (Task, error) {
	fields, ok := raw.(map[string]any)
	if !ok {
		return Task{}, fmt.Errorf("task at position %d is not an object", index)
	}
	// STRICT for the two lists that carry AUTHORITY AND ORDER. planStrings drops
	// an entry it cannot read, which is fine for a display label and wrong here:
	// "depends_on": [42] decoded to no dependency at all, so the task was admitted
	// as dependency-free and ran BEFORE the precondition it declared — silently,
	// with nothing to notice. That defeats this file's own rule that an unknown
	// edge is rejected and never skipped. The same argument covers "tools", where
	// a dropped entry quietly narrows a grant the caller believed they asked for.
	dependsOn, err := planStringsStrict(fields, "depends_on")
	if err != nil {
		return Task{}, fmt.Errorf("task at position %d: %w", index, err)
	}
	tools, err := planStringsStrict(fields, "tools")
	if err != nil {
		return Task{}, fmt.Errorf("task at position %d: %w", index, err)
	}
	task := Task{
		ID:        planString(fields, "id"),
		Prompt:    planString(fields, "prompt"),
		DependsOn: dependsOn,
		Tools:     tools,
		Phase:     planString(fields, "phase"),
	}
	// REFUSED AT ADMISSION, not degraded at dispatch. The manifest loader already
	// treats an unknown model as fatal, so falling back to the parent's here
	// would soften an existing rejection — and a plan that asked for a stronger
	// model on its verify stage, silently ran on a weaker one, and still reported
	// success is indistinguishable from one that worked.
	model, modelErr := resolveTaskModel(planString(fields, "model"))
	if modelErr != nil {
		return Task{}, fmt.Errorf("task at position %d: %w", index, modelErr)
	}
	task.Model = model
	if task.ID == "" {
		return Task{}, fmt.Errorf("task at position %d has no id", index)
	}
	if !planIDPattern.MatchString(task.ID) {
		return Task{}, fmt.Errorf("task id %q must use only letters, digits, hyphen and underscore", task.ID)
	}
	if task.Prompt == "" {
		return Task{}, fmt.Errorf("task %q has no prompt", task.ID)
	}
	return task, nil
}

func planTaskList(args map[string]any) ([]any, error) {
	raw, ok := args["tasks"]
	if !ok {
		return nil, fmt.Errorf("plan requires a tasks array")
	}
	list, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("plan tasks must be an array")
	}
	return list, nil
}

func planString(args map[string]any, key string) string {
	if args == nil {
		return ""
	}
	value, _ := args[key].(string)
	return strings.TrimSpace(value)
}

// planStringsStrict is planStrings for lists whose entries MUST be readable.
//
// Its lenient twin skips what it cannot decode, which suits a label nobody acts
// on. It does not suit a dependency edge or a tool name: a skipped entry there
// is not a missing label, it is a precondition that stopped existing or an
// authority the caller thinks they narrowed. Refused with the offending value
// named, so the author can see WHICH entry was wrong rather than diffing what
// they sent against what ran.
//
// THE CONTAINER IS STRICT TOO, not only its entries. An ABSENT key is the
// documented "none" (no dependencies; the inherited read-only grant), and an
// empty array says the same thing explicitly. A key that is PRESENT but is not
// an array — a bare string, an object, a number, or null — is refused: reading
// "depends_on": "setup" as no dependencies ran the task before the
// precondition it named, and a malformed "tools" became the default grant
// rather than the narrowing the caller wrote. Only omission means omission.
func planStringsStrict(args map[string]any, key string) ([]string, error) {
	value, present := args[key]
	if !present {
		return nil, nil
	}
	raw, ok := value.([]any)
	if !ok {
		if value == nil {
			return nil, fmt.Errorf("%s must be an array of strings, got null; omit it to declare none", key)
		}
		return nil, fmt.Errorf("%s must be an array of strings, got %T; omit it to declare none", key, value)
	}
	out := make([]string, 0, len(raw))
	for index, item := range raw {
		text, ok := item.(string)
		if !ok {
			return nil, fmt.Errorf("%s[%d] must be a string, got %T", key, index, item)
		}
		trimmed := strings.TrimSpace(text)
		if trimmed == "" {
			return nil, fmt.Errorf("%s[%d] is empty", key, index)
		}
		out = append(out, trimmed)
	}
	return out, nil
}

func planStrings(args map[string]any, key string) []string {
	raw, ok := args[key].([]any)
	if !ok {
		return nil
	}
	out := []string{}
	for _, item := range raw {
		if text, ok := item.(string); ok {
			if trimmed := strings.TrimSpace(text); trimmed != "" {
				out = append(out, trimmed)
			}
		}
	}
	return out
}

// planBudgetIntFields is every integer-valued budget field. planBudget decodes
// each through planIntField before applying any rule, so a field added here
// cannot be read by a second, laxer conversion.
var planBudgetIntFields = []string{
	"max_workers",
	"max_tokens",
	"max_tokens_per_task",
	"max_wall_seconds",
	"max_stall_seconds",
	"max_retries",
}

// planInt is planIntField for a reader that only needs a best-effort value
// BEFORE admission (the verify stage sizes itself from the raw budget). An
// absent or undecodable value reads as 0; ParsePlan, which decodes the same
// field with the same function, still refuses the undecodable one.
func planInt(args map[string]any, key string) int {
	value, _, _ := planIntField(args, key)
	return value
}

// planIntField is THE integer decoder for plan arguments. It reports the value,
// whether the key was PRESENT (an explicit 0 and an absent key mean different
// things for several budget fields), and an error for a present value that is
// not a whole number.
//
// Accepted: an int, the float64 a JSON number decodes to when it is finite,
// whole and representable, and a numeric STRING holding such a number. The
// string case follows the repo's own tool-argument rule (intArg in
// internal/tools/args.go: "Some models send numbers as strings"), because
// nothing between the provider and this parser coerces arguments to the
// declared schema, and reading "500000" as absent silently erased a spending
// limit.
//
// Refused, never truncated or treated as absent: a fraction (0.9 would become
// 0 and disable a bound; 1.9 workers would become 1), NaN, ±Inf, a value out
// of int range, a non-numeric string, null, and any other type. Only omission
// means omission.
func planIntField(args map[string]any, key string) (int, bool, error) {
	value, present := args[key]
	if !present {
		return 0, false, nil
	}
	invalid := func(got any) (int, bool, error) {
		if got == nil {
			return 0, false, fmt.Errorf("%s must be a whole number, got null; omit it to leave it unset", key)
		}
		return 0, false, fmt.Errorf("%s must be a whole number, got %v", key, got)
	}
	switch typed := value.(type) {
	case int:
		return typed, true, nil
	case float64:
		number, ok := planWholeFloat(typed)
		if !ok {
			return invalid(typed)
		}
		return number, true, nil
	case string:
		trimmed := strings.TrimSpace(typed)
		if number, err := strconv.Atoi(trimmed); err == nil {
			return number, true, nil
		}
		parsed, err := strconv.ParseFloat(trimmed, 64)
		if err != nil {
			return invalid(fmt.Sprintf("%q", typed))
		}
		number, ok := planWholeFloat(parsed)
		if !ok {
			return invalid(fmt.Sprintf("%q", typed))
		}
		return number, true, nil
	default:
		return invalid(value)
	}
}

// planWholeFloat converts f to an int only when it is finite, integral and in
// range, before any implementation-defined float-to-int conversion can run.
// The upper comparison is >= because float64(math.MaxInt) rounds UP to 2^63,
// which is itself out of range.
func planWholeFloat(f float64) (int, bool) {
	if math.IsNaN(f) || math.IsInf(f, 0) || math.Trunc(f) != f ||
		f >= float64(math.MaxInt) || f < float64(math.MinInt) {
		return 0, false
	}
	return int(f), true
}

// planBoolSet reports a boolean argument AND whether it was supplied at all.
//
// planBool cannot distinguish false from absent, which is fine when absent means
// off — and wrong the moment a default can come from somewhere else. A plan
// saying auto_assign:false must be able to override a config that turned it on,
// and that requires knowing the difference.
func planBoolSet(args map[string]any, key string) (bool, bool) {
	value, ok := args[key].(bool)
	return value, ok
}

// planBool reads a boolean argument. Absent, or any other type, is false — a
// flag that changes where a plan RUNS must be opted into explicitly, never
// inferred from a value that happened to be there.
func planBool(args map[string]any, key string) bool {
	value, _ := args[key].(bool)
	return value
}

// maxRepresentableSeconds is the largest whole number of seconds that survives
// conversion to a time.Duration, which counts NANOSECONDS in an int64.
const maxRepresentableSeconds = int64(math.MaxInt64) / int64(time.Second)

// refuseUnrepresentableSeconds rejects a timeout too large to express as a
// time.Duration.
//
// SILENTLY WRAPPING IS THE HAZARD, and it fails OPEN. seconds * time.Second
// overflows int64 past ~292 years and comes back NEGATIVE, and a negative wall
// budget reads as "no bound at all" at the dispatch check — so a plan asking for
// an absurdly long timeout got an unlimited one instead, which is the opposite
// of what every other bound in this file does with a value it cannot honour.
// The negative and the zero cases are already refused above; this closes the
// third way a number stops meaning what it says.
func refuseUnrepresentableSeconds(field string, seconds int) error {
	if int64(seconds) > maxRepresentableSeconds {
		return fmt.Errorf(
			"budget.%s must be at most %d seconds; %d cannot be represented as a duration and would wrap to a negative timeout, which reads as no timeout at all",
			field, maxRepresentableSeconds, seconds)
	}
	return nil
}
