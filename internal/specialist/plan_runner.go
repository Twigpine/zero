package specialist

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Twigpine/zero/internal/measurements"
	"github.com/Twigpine/zero/internal/streamjson"
	"github.com/Twigpine/zero/internal/tools"
)

// PlanTaskContext is the per-RUN state a plan task inherits from its parent.
//
// It is captured at registration and does not change for the process's life —
// unlike the posture, which flips between runs and therefore lives behind a
// PostureGate pointer. Everything here is genuinely run-invariant (paths,
// workspace) or supplied per-call.
type PlanTaskContext struct {
	Executor Executor
	Cwd      string
	// PermissionMode / Depth describe the run issuing the plan, so a task
	// inherits exactly the parent's policy.
	//
	// ParentSessionID and ParentModel are DELIBERATELY NOT HERE. They looked
	// run-invariant and are not: the TUI builds this once per session while the
	// user can change model with /model between runs, so a value captured here
	// would be whatever was active at startup — and in practice both call sites
	// left them empty, so a plan task inherited no model at all. They arrive
	// per call on PlanTaskRequest instead, from the same tools.RunOptions the
	// Task tool reads.
	PermissionMode string
	Depth          int
	// SpecialistName is the read-only specialist each plan task runs as.
	SpecialistName string
	// PostureReasoningEffort is the effort the zeromaxing posture asks for, used
	// only as the fallback in planTaskReasoningEffort.
	//
	// PASSED IN rather than read from execprofile, and not for tidiness: this
	// package cannot import execprofile at all. execprofile imports agent, and
	// agent's own test binary imports specialist, so the reference compiled fine
	// and broke `go test ./internal/agent` with an import cycle. The caller
	// already knows the posture; it hands over the one value needed.
	PostureReasoningEffort string
}

// planTaskDescriptionPrefix labels a plan task's child session.
//
// ONE SPELLING, because worker_view reads it back to tell a plan task from a
// plain Task call. Two copies of a magic string is invariant 5 — the writer
// would be free to change without the reader noticing, and the only symptom
// would be every plan task quietly re-labelled as a direct delegation.
const planTaskDescriptionPrefix = "plan task "

// NewPlanRunner adapts Executor.Run into a PlanRunner.
//
// LIFETIME, deliberately: the returned closure captures only run-INVARIANT
// state — the executor, the workspace, the parent's identity and policy. It
// captures NO context. The ctx it uses is the one ExecutePlan hands it per
// task, which is the tool call's own context, so a cancelled run cancels the
// task in flight. Capturing a context at construction is precisely how the
// prototype's background goroutine kept running after cancellation, and a
// runner that outlived its plan would do it again.
//
// The runner does not outlive the plan in any meaningful sense either: it is
// synchronous, returns before ExecutePlan moves to the next task, and holds no
// goroutine of its own.
func NewPlanRunner(planCtx PlanTaskContext) PlanRunner {
	return func(ctx context.Context, req PlanTaskRequest) (TaskResult, error) {
		task, grantedTools := req.Task, req.Tools
		if ctx == nil {
			ctx = context.Background()
		}
		// Honour cancellation BEFORE launching a child: a cancelled plan must
		// not spend another task's budget.
		if err := ctx.Err(); err != nil {
			return TaskResult{Outcome: TaskFailed, Err: err.Error()}, err
		}

		// THE STALL WATCHDOG. Its clock resets on every event the child emits,
		// so a task that is working — however slowly — is never stopped; only
		// silence counts. The context it cancels is this task's alone, so a
		// wedged task does not take the plan with it.
		watchdog := newStallWatchdog(req.StallTimeout, nil)
		// Zero leaves the production rule in place; only a test sets it.
		watchdog.poll = req.StallPoll
		// Held until the executor launches the child (OnLaunch below).
		watchdog.holdUntilLaunch()
		taskCtx, cancelTask := context.WithCancel(ctx)
		defer cancelTask()
		stopWatchdog := watchdog.watch(taskCtx, cancelTask)
		defer stopWatchdog()

		// A WRITE TASK THAT CALLED NO TOOL DID NOT DO THE WORK, and the plan must
		// not record it as success. Counted from the child's own stream, which is
		// the only evidence the parent has of what the child actually did.
		toolCalls := &atomic.Int64{}
		// THE METER READS THE STREAM, so a budget can be enforced while the task
		// is running rather than after it has finished.
		//
		// Every usage event a child emits is one provider call it has already
		// been billed for. Waiting for the child to exit before counting them is
		// what let a measured plan spend 3,091,618 against a 200,000 budget: four
		// tasks in flight, none of them bounded, and the first number landing
		// after all four were done.
		taskTokens := &atomic.Int64{}
		// SEEDED with what earlier attempts of this task spent: the per-task cap
		// bounds the TASK, and a fresh zero here handed every retry the whole
		// cap again.
		taskTokens.Store(int64(req.TaskTokensSpent))
		overspent := &atomic.Value{}
		// THE TASK'S OWN NUMBERS, read from the commands it actually ran. Every
		// tool result a child emits streams through here, which is the only place
		// the parent ever sees what a task's commands printed.
		measured := measurements.NewLedger()
		// WHAT THIS TASK ACTUALLY CHANGED, accumulated from the child's own tool
		// results. The handoff below is built from this rather than from prose,
		// which matters twice: it costs nothing, and it cannot be invented — a
		// file is on this list because a tool reported changing it.
		var changedMu sync.Mutex
		changedFiles := map[string]bool{}
		counted := func(event streamjson.Event) {
			if event.Type == streamjson.EventToolCall {
				toolCalls.Add(1)
			}
			if event.Type == streamjson.EventToolResult {
				// WITH THE CALL THAT PRODUCED IT. The stream carries the tool,
				// its arguments and the directory, so a timing is recorded
				// against the command that printed it rather than pooled with
				// every other command's — which is what let a claim about one run
				// be satisfied by a number a different run produced.
				measured.Record(runForEvent(event), event.Output)
				if len(event.ChangedFiles) > 0 {
					changedMu.Lock()
					for _, file := range event.ChangedFiles {
						changedFiles[file] = true
					}
					changedMu.Unlock()
				}
			}
			if event.Type == streamjson.EventUsage && event.TotalTokens != nil {
				spent := *event.TotalTokens
				task := taskTokens.Add(int64(spent))
				// EVERY EVENT REACHES THE PLAN METER, before either limit is
				// judged. The add used to sit inside the second case's condition,
				// and a switch stops at the first true case — so the event that
				// tripped the per-task cap, and every event after it, was billed
				// to the task but never to the plan, and downstream dispatch was
				// gated on an undercount.
				poolTotal := req.Spend.add(spent, req.WaitsOnOtherTasks)
				// BOTH LIMITS, and the task's own is checked first so its message
				// names the cap the caller actually set for it.
				switch {
				case req.MaxTaskTokens > 0 && task > int64(req.MaxTaskTokens):
					overspent.Store(fmt.Sprintf(
						"task %s stopped after %d tokens: budget.max_tokens_per_task is %d",
						task_ID(req), task, req.MaxTaskTokens))
					cancelTask()
				case req.Spend.overPool(poolTotal, req.WaitsOnOtherTasks):
					// THE CEILING IS THIS TASK'S, not the plan's. A task others
					// depend on stops before the whole budget is gone, so their
					// work still has something to run on — see planSpendCeiling.
					overspent.Store(fmt.Sprintf(
						"task %s stopped: the plan's token budget ran out while it was running",
						task_ID(req)))
					cancelTask()
				}
			}
			if req.Progress != nil {
				req.Progress(event)
			}
		}

		started := time.Now()
		manifest := planTaskManifest(planCtx.SpecialistName, task.Model,
			planTaskReasoningEffort(task.Model, req.ParentReasoningEffort, planCtx.PostureReasoningEffort),
			grantedTools)
		if override := strings.TrimSpace(req.SystemPrompt); override != "" {
			manifest.SystemPrompt = override
		}
		res, err := planCtx.Executor.Run(taskCtx, TaskParameters{
			Name:        planCtx.SpecialistName,
			Prompt:      task.Prompt,
			Description: planTaskDescriptionPrefix + task.ID,
			Manifest:    &manifest,
		}, TaskRunOptions{
			// Per-call, from the tool's RunOptions — see PlanTaskRequest.
			ParentSessionID:       req.ParentSessionID,
			ParentModel:           req.ParentModel,
			ParentReasoningEffort: req.ParentReasoningEffort,
			ToolCallID:            req.ParentToolCallID,
			CurrentDepth:          planCtx.Depth,
			// The plan's own workspace when it has one, the parent's otherwise.
			// A write-capable plan runs in an isolated worktree, and this is
			// the line that puts its children there.
			Cwd:            planTaskCwd(planCtx.Cwd, req.Cwd),
			PermissionMode: planCtx.PermissionMode,
			// THE PLAN'S SCRATCHPAD, so a task handed a truncated excerpt can
			// open the whole answer its dependency wrote. Read-only: the
			// executor is the sole writer there.
			ExtraReadRoots: req.ReadRoots,
			// The parent's request_permissions READ grants, on their own read-only
			// flag, so a plan auditing a granted external path can read it without
			// gaining write access (ExtraReadRoots above is emitted as --add-dir).
			ReadOnlyRoots: req.ReadOnlyRoots,
			// THE SECOND HALF of the same defect. The Task tool forwards its
			// caller's progress callback here (task_tool.go); this path built
			// the same struct and omitted the field, so a plan task's child
			// streamed to nobody. Same class as finding 7 (ParentModel): a
			// second construction path that does not carry what the first one
			// did. Fixed with the tool-side half, not separately.
			Progress: watchedProgress(watchdog, counted),
			// THE CHILD'S SILENCE STARTS AT LAUNCH. The watchdog was armed when
			// this runner began, so everything the executor did first —
			// manifest, admission, args, prompt file, start accounting — was
			// charged as the child's silence, and a slow launch could be killed
			// as "stalled" before the child existed.
			OnLaunch: watchdog.touch,
			// THE RUNG MUST MATCH THE GRANT, and for a long time it did not.
			//
			// This said "explicitly NOT MemberAutonomy: Phase 2 tasks are
			// read-only", which was true when every plan task was. Write-capable
			// plans then arrived with a grant, an approval prompt and a worktree
			// — and nobody came back here. specialistAutonomy maps every
			// non-unsafe parent to "low", so the child was handed write_file in
			// its manifest and launched at read-only autonomy, which never
			// advertises it. The model, wanting a tool it could not see,
			// narrated the write and leaked fragments of the call into its text
			// ("belwrite_file"). Zero tool calls, no file, and until the guard
			// above it also reported success.
			//
			// DERIVED FROM THE GRANT, like the prompt: a read-only task stays at
			// "low" exactly as before, so nothing about an ordinary plan changes.
			// member-auto is the rung built for this — it advertises in-workspace
			// write/edit and sandboxed shell while the sandbox still gates every
			// call, and it normalizes to Auto everywhere except advertisement, so
			// authority is not widened beyond what an interactive auto agent
			// already has. For a plan task the workspace IS the isolated
			// worktree, which is what makes that bound meaningful here.
			MemberAutonomy: grantsPlanWriteTool(grantedTools),
		})
		result := TaskResult{
			ID: task.ID,
			// What it actually ran on, carried back so the report can say so.
			Model:     task.Model,
			Duration:  time.Since(started),
			SessionID: res.SessionID,
			// Carried so the executor can tell a task IT killed from one the OS
			// killed — see TaskResult.Signal.
			Signal: res.Signal,
			// The id travels in SessionID, not in the prose. See
			// WithoutSessionIDLine: a plan task's output is quoted into the
			// report, into the dependency briefing every downstream task reads,
			// and into the panel — so the line BuildFinalResult prepends for a
			// Task caller surfaces a raw child session id inside a user-facing
			// answer, three times over.
			Output: WithoutSessionIDLine(res.Result.Output, res.SessionID),
			// The meter the plan budget is spent from. A task whose stream
			// reported no usage costs 0 here, which is honest — but it means a
			// provider that never reports usage cannot be budget-bounded by
			// token count; MaxWall is the backstop in that case.
			Tokens: res.TotalTokens,
		}
		// CHECKED BEFORE THE WATCHDOG. Both cancel the same context, so a task
		// stopped for budget looks exactly like a stalled one from the outside —
		// and a stall is RETRYABLE. Retrying a task that was stopped for spending
		// too much is the worst possible response to it.
		// CHECKED AFTER THE CHILD HAS FINISHED, against the output it is handing
		// back. Only conflicts are recorded, so an honest task carries nothing.
		// ACROSS RUNS: the child's output summarises everything it ran, so there
		// is no single command to hold each number to, and holding it to one
		// would accuse the child of inventing a figure another of its own
		// commands really printed.
		for _, conflict := range measured.ConflictsAcrossRuns(result.Output) {
			result.MeasurementConflicts = append(result.MeasurementConflicts,
				fmt.Sprintf("%s: reported %s, but this task's own commands printed %s",
					conflict.Name, formatConflictSeconds(conflict.Claimed), formatRecorded(conflict.Recorded)))
		}
		if reason, ok := overspent.Load().(string); ok && reason != "" {
			result.Outcome = TaskCancelled
			result.Err = reason
			// A CUT-SHORT TASK MUST HAND SOMETHING ON.
			//
			// withDependencyBriefing already carries a cancelled task's output to
			// its dependents, labelled INCOMPLETE — so the machinery for a
			// follow-up task to pick this work up exists. It was fed nothing: a
			// task killed mid-edit-loop has written no prose, so Output was empty
			// and a measured run lost 858,231 tokens of real work with no record
			// of what it had touched.
			//
			// NOT A MODEL CALL. The child's process is already gone by the time the
			// meter trips, so a summary turn would mean resuming a task that just
			// overspent — paying more for the report than the report is worth, and
			// asking for prose from an agent whose last act was being stopped. The
			// file list is free, exact, and is the thing a continuation actually
			// needs: where the work got to.
			changedMu.Lock()
			handoff := planTaskHandoff(changedFiles, result.Output)
			changedMu.Unlock()
			result.Output = handoff
			return result, nil
		}
		if watchdog.didFire() {
			// A stall and a user cancellation both surface as a context error;
			// they are not the same event and must not read the same.
			//
			// Stalled is what makes the task RETRYABLE, and it is set here rather
			// than inferred by the executor from the message: matching on error
			// text to decide whether to spend another child is exactly the shape
			// invariant 9 warns about, one layer up from errors.Is.
			result.Outcome = TaskFailed
			result.Stalled = true
			result.Err = stallError(task.ID, watchdog.timeout, 1).Error()
			return result, nil
		}
		if err != nil {
			result.Outcome = TaskFailed
			result.Err = err.Error()
			result.ModelRejected = modelLookedUnusable(result, task, toolCalls.Load())
			return result, err
		}
		if res.Result.Status == tools.StatusError {
			// The child ran but its task FAILED. Surfacing it as success would
			// let a plan report work that did not happen.
			result.Outcome = TaskFailed
			result.Err = res.Result.Output
			result.ModelRejected = modelLookedUnusable(result, task, toolCalls.Load())
			// A DECLINE IS NOT A WRONG ANSWER. The child exits with this code when
			// it stopped with work unfinished — it said it could not, rather than
			// doing it badly — and that is worth one more attempt in a way a wrong
			// answer never is.
			result.Declined = res.ExitCode == childExitIncomplete
			// THE PROVIDER FAILED, NOT THE TASK. Same structural signal, same
			// reason it is worth another attempt: nothing about the work produced
			// this, so repeating the work is not what repeating the request buys.
			result.ProviderFailed = res.ExitCode == childExitProvider
			return result, nil
		}
		// THE LAST CHECK, and the one a plausible answer gets past. A task granted
		// write tools that emitted not one tool call cannot have written anything,
		// whatever its output says. A real run produced exactly this: seventeen
		// completion tokens reading "Creating notes.md now.", no tool call, and a
		// plan reporting succeeded 1.
		//
		// Only for tasks that CAN write, because only there is the inference
		// airtight — a file cannot appear without a tool call, while a read-only
		// task answering from its own prompt is unusual rather than impossible.
		// The task prompt already asks every task to start with a tool call; this
		// makes the write half enforceable instead of merely requested.
		if grantsPlanWriteTool(grantedTools) && toolCalls.Load() == 0 {
			result.Outcome = TaskFailed
			result.Err = "task " + task.ID + " was granted write tools and finished without calling a single one, " +
				"so it changed nothing — its output describes work that did not happen. " +
				"Re-run it, or state the change the task must make unambiguously."
			return result, nil
		}
		result.Outcome = TaskSucceeded
		return result, nil
	}
}

// task_ID names the task for a message, tolerating an unnamed one.
func task_ID(req PlanTaskRequest) string {
	if id := strings.TrimSpace(req.Task.ID); id != "" {
		return strconv.Quote(id)
	}
	return "(unnamed)"
}

// modelLookedUnusable reports that a task died without the assigned model ever
// producing anything — the signature of a provider refusing the MODEL rather
// than the work failing.
//
// A FLAG, not a message match, for the same reason Stalled is one: choosing to
// spend another child by looking for a phrase in an error is the class of bug
// that silently disabled every stall retry in the prototype. Providers word this
// differently every time — "does not exist or your team does not have access to
// it", "Multi Agent requests are not allowed on chat completions" — and the next
// provider will word it a fourth way.
//
// The structural facts are provider-independent:
//
//   - A model was ASSIGNED. With none the task already ran on the parent's, so
//     there is nothing to fall back to.
//   - The child did NO WORK: no tool call, no token. This is what separates "the
//     provider refused this model" from "the work failed": a task that reasoned,
//     answered and got it wrong has spent tokens, and re-running it elsewhere
//     would be a second opinion nobody asked for.
//
// MEASURED FROM THE CHILD, NOT FROM Result.Output, and that distinction is the
// whole defect this signal was written to catch. On a failed child
// BuildFinalResult returns a DIAGNOSTIC as the output — "Subagent failed (exit
// 3)\nerrors: provider request error: ..." — so Output is never empty on exactly
// the path that matters, and an emptiness test there can never fire. It did not:
// two tasks died on a refused model with the fallback sitting right there. Tool
// calls and tokens come from the child's own stream and describe the child.
//
// THE DECISION TO RETRY IS NOT MADE HERE, and that placement is the point. This
// file's own retry loop lives in the executor because the executor owns the
// budget, the wall deadline, cancellation and the record — a retry hidden in the
// runner spends a child none of them can see, under-reports the plan's duration
// and leaves Attempts saying one when two ran. This classifies; runTaskWithRetries
// decides.
func modelLookedUnusable(result TaskResult, task Task, toolCalls int64) bool {
	if strings.TrimSpace(task.Model) == "" {
		return false
	}
	return result.Tokens == 0 && toolCalls == 0
}

// planTaskCwd prefers the plan's own workspace over the parent's. An empty
// override is the read-only case and means "wherever the parent runs".
func planTaskCwd(parentCwd, override string) string {
	if strings.TrimSpace(override) != "" {
		return override
	}
	return parentCwd
}

// planTaskManifest builds the inline manifest a plan task runs under. The tool
// list is the ALREADY-INTERSECTED grant ExecutePlan computed, so this cannot
// widen it — it only carries it.
// planManifestFilePath marks a manifest authored by the plan path. It is the
// provenance autoTaskModel keys on: a plan task's model was already decided by
// the plan tool's own assignment, and must not be re-decided at dispatch.
const planManifestFilePath = "(plan)"

func planTaskManifest(name, model, reasoningEffort string, grantedTools []string) Manifest {
	if strings.TrimSpace(name) == "" {
		name = "explorer"
	}
	// DERIVED FROM THE GRANT, never carried beside it. The prompt below used to
	// say "you have read-only tools … do not attempt to modify anything" for
	// every task, including a write-capable plan the user had approved and the
	// executor had isolated in a worktree. That task was handed write_file and
	// told in the same sentence not to use it; a model that obeys produces a
	// plan which reports success and changed nothing, and one that disobeys was
	// never being steered in the first place.
	//
	// A writeCapable bool threaded down alongside grantedTools would be the same
	// defect waiting: two statements of one fact, free to drift apart. The grant
	// IS the fact, and it is already an argument.
	description := "Read-only plan task."
	if grantsPlanWriteTool(grantedTools) {
		description = "Plan task with write access."
	}
	return Manifest{
		Metadata: Metadata{
			Name:            name,
			Description:     description,
			Model:           model,
			ReasoningEffort: reasoningEffort,
			Tools:           grantedTools,
		},
		// IT MUST SAY "USE THEM". The first version said "You have read-only
		// tools" and stopped there, which states a fact and asks for nothing. A
		// task told to "find every definition and quote the file:line" can
		// answer that from its own weights, and a real run did: 260 seconds of
		// generation with ZERO tool calls on the first task of a fifteen-task
		// plan. The stall watchdog cannot catch it either — it keys on silence,
		// and a model writing prose is not silent.
		//
		// So the instruction is now an obligation with a named failure: search
		// before answering, and say you could not find it rather than produce
		// something plausible. A plan task exists to go and look; a plan task
		// that reasons from memory is worse than no task, because its output
		// reads exactly like the one that looked.
		SystemPrompt: planTaskSystemPrompt(grantedTools),
		Location:     LocationBuiltin,
		FilePath:     planManifestFilePath,
		// AUTHORITATIVE, not a hint: this is the already-intersected grant, and
		// an empty one must refuse the child rather than expand to the default
		// read-only category. ExecutePlan refuses before reaching here, so this
		// is the second layer.
		ResolvedTools: grantedTools,
		ToolsResolved: true,
	}
}

// planTaskSystemPrompt builds the child's contract from the tools it was
// ACTUALLY granted.
//
// It used to be one literal that told every task "You have read-only tools" and
// "do not attempt to modify anything" — including a task that named write_file
// or bash, which ParsePlan permits by design ("A TASK MAY NOW NAME A WRITE
// TOOL, and only by naming it"). A child instructed not to modify anything will
// not use the write tool it was granted, so the grant, the approval prompt it
// triggered, and the worktree prepared for it all bought nothing.
//
// The read-only wording stays exactly as it was for the read-only case, which
// is the overwhelming majority and the one whose phrasing was tuned.
func planTaskSystemPrompt(grantedTools []string) string {
	// The two rules below are the plan-task half of the posture's evidence
	// contract (agent.ZeromaxingEvidenceNotice), which a plan task does NOT
	// receive: the contract rides the loop's posture reminders, and a task runs as
	// a child process that is never handed --exec-profile, so it starts with the
	// posture off. The rest of that contract was already here in substance —
	// claims backed by what was read, quoted file:line, an honest "not found" —
	// and these are what was missing.
	//
	// Stated here rather than shared with that constant, because this package
	// CANNOT import agent: agent's own test binary imports specialist, so the
	// reference would compile and then break `go test ./internal/agent` with a
	// cycle — the same trap PostureReasoningEffort above exists to avoid. The
	// framing differs anyway: that notice addresses an agent being pushed back on
	// by a person, this addresses a task writing into a report nobody re-derives.
	// TestAPlanTaskCarriesTheEvidenceRules pins the rules, not the wording.
	const investigate = "USE THEM. Search and read the actual files before you answer — do not rely on memory or " +
		"inference about what the code probably says. Start with a tool call, not with prose. " +
		"Every claim you make must be backed by something you read in this run, quoted with its " +
		"file:line. If you cannot find something, say so plainly; an honest \"not found\" is worth " +
		"more than a plausible guess, and a guess is indistinguishable from a finding once it " +
		"reaches the plan's report. A passing test is not proof that a property holds: when you " +
		"rest a claim on one, name the test and say in one sentence what it would still pass with. " +
		"Any number you report — a timing, a count, a percentage — must come from a command you ran " +
		"in this run; if you did not run it, say so rather than stating a figure the plan's report " +
		"will carry as measured. "
	if !grantsPlanWriteTool(grantedTools) {
		return "You are executing one task of a larger plan. You have read-only tools: " + investigate +
			"Complete exactly the task described and report what you found; do not attempt to modify anything."
	}
	return "You are executing one task of a larger plan. " + investigate +
		"You have been granted tools that CHANGE things, and only the ones named in your task. " +
		"Make exactly the change the task describes and nothing beyond it: no drive-by fixes, no " +
		"reformatting, no edits to files the task did not name. Report what you changed, with file:line."
}

// grantsPlanWriteTool reports whether a grant contains any tool that can change
// something, using the same allow-list ParsePlan validates against so the two
// cannot drift.
func grantsPlanWriteTool(grantedTools []string) bool {
	for _, name := range grantedTools {
		if planWriteTools[name] {
			return true
		}
	}
	return false
}

// planTaskReasoningEffort decides what effort a task runs at, and exists because
// appendModelArgs inherits the parent's ONLY when the manifest names no model:
//
//	if reasoningEffort == "" && manifest.Metadata.Model == "" {
//	    reasoningEffort = parentReasoningEffort
//	}
//
// That rule is right in general — an effort tier is meaningful only against the
// model it was chosen for — and it means naming a model on a task silently drops
// the posture's raised effort, which is most of what the posture IS. A plan that
// pointed its hardest task at a stronger model would get that model thinking
// less than the tasks around it.
//
// So the effort is stated explicitly, and ONLY when the task names a model:
// leaving it empty otherwise keeps the untouched path byte-identical to what it
// was, which is what the additivity guarantee rests on.
//
// The parent's effort is the first choice because it is already clamped to the
// parent's model and reflects any /effort the user set. It is EMPTY exactly when
// the posture could not raise it — a model whose tiers the catalog cannot vouch
// for — and that user is the one most likely to point a task somewhere else, so
// falling through to the posture's own effort serves precisely the case that
// would otherwise be served worst.
//
// FORWARDED ONLY FOR A MODEL THE REGISTRY KNOWS, and that qualifier was learned
// the hard way. This used to reason "the child re-clamps via
// forwardedReasoningEffort, so forwarding an unsupported tier is safe" — true
// while every nameable model was curated. Once uncurated models were allowed
// through, that clamp stopped applying to them:
//
//	entry, ok := registry.Get(modelID)
//	if !ok { return requested }   // unknown model: forwarded verbatim
//
// so "high" went straight to the provider and a real run died three times over
// with `Model grok-build-0.1 does not support parameter reasoningEffort`.
//
// For a model nobody can vouch for, the honest answer is to say nothing and let
// the provider apply its own default. Sending a parameter it may not accept, to
// buy an effort level we cannot confirm it has, trades a working task for a
// guess.
func planTaskReasoningEffort(model, parentReasoningEffort, postureReasoningEffort string) string {
	if strings.TrimSpace(model) == "" {
		return ""
	}
	if !modelTakesExplicitEffort(model) {
		return ""
	}
	if effort := strings.TrimSpace(parentReasoningEffort); effort != "" {
		return effort
	}
	return strings.TrimSpace(postureReasoningEffort)
}

// formatConflictSeconds and formatRecorded render a measurement for a reader of
// the plan's report, where the numbers sit beside prose rather than in a table.
func formatConflictSeconds(value float64) string {
	return strconv.FormatFloat(value, 'f', -1, 64) + "s"
}

func formatRecorded(values []float64) string {
	parts := make([]string, 0, len(values))
	for _, value := range values {
		parts = append(parts, formatConflictSeconds(value))
	}
	return strings.Join(parts, ", ")
}

// planTaskHandoff describes where a cut-short task got to, so a follow-up task
// does not start blind.
//
// APPENDED TO WHATEVER THE TASK DID SAY, never replacing it: a task that wrote
// prose before it was stopped has already said something worth keeping, and the
// file list is an addition to that rather than a substitute for it.
func planTaskHandoff(changed map[string]bool, existing string) string {
	if len(changed) == 0 {
		return existing
	}
	files := make([]string, 0, len(changed))
	for file := range changed {
		files = append(files, file)
	}
	sort.Strings(files)

	var b strings.Builder
	if trimmed := strings.TrimSpace(existing); trimmed != "" {
		b.WriteString(trimmed)
		b.WriteString("\n\n")
	}
	b.WriteString("This task was stopped before it finished. It had already changed these files:\n")
	for _, file := range files {
		b.WriteString("- ")
		b.WriteString(file)
		b.WriteString("\n")
	}
	b.WriteString("Their contents on disk are where the work got to — read them before continuing, " +
		"because they may be mid-change and need not compile.")
	return b.String()
}

// runForEvent describes the command a tool result came from, for the
// measurements ledger. Args is rendered rather than structured because the
// ledger only ever compares and displays it — two calls differing in their
// arguments must land in different runs, and that is all this has to guarantee.
func runForEvent(event streamjson.Event) measurements.Run {
	run := measurements.Run{Command: event.Name, Dir: event.Cwd}
	if event.Args != nil {
		run.Args = []string{fmt.Sprint(event.Args)}
	}
	return run
}
