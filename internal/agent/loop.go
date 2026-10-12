package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/Gitlawb/zero/internal/execution"
	"github.com/Gitlawb/zero/internal/hooks"
	"github.com/Gitlawb/zero/internal/modelregistry"
	"github.com/Gitlawb/zero/internal/redaction"
	"github.com/Gitlawb/zero/internal/sandbox"
	"github.com/Gitlawb/zero/internal/streamjson"
	"github.com/Gitlawb/zero/internal/tools"
	"github.com/Gitlawb/zero/internal/trace"
	"github.com/Gitlawb/zero/internal/zeroruntime"
)

const maxTurnsAnswer = "Agent reached maximum number of turns without a final answer."
const maxTurnsFinalAnswerPrompt = "You have reached the tool-turn limit. Do not call tools. Give a concise final answer now: summarize what you completed, what you found, and any remaining blockers."

// maxStreamStallRetries bounds how many times a turn that timed out (idle/stall)
// OR hit a mid-stream transport abort (#973) with no answer text yet is re-issued
// on a fresh connection before giving up. Incomplete tool-call previews are
// allowed through the gate (they are never dispatched before the error return).
// The safety claim is only that visible answer prose is not duplicated on retry —
// not that every partial stream shape is excluded. Covers a stalled/dead pooled
// connection or a socket abort before tool dispatch.
//
// Set to 1 (not 2): each attempt can itself idle for the full stream timeout
// (~5min) before the stall is even detected, so 2 retries left an interactive
// session frozen for ~15min. One retry keeps the common single-hiccup recovery
// while bounding the worst case to ~2× the idle timeout. Mid-stream transport
// aborts share this bound (do not raise it for #973).
const maxStreamStallRetries = 1

const (
	toolResultMetaControl       = "control"
	toolResultControlSpecReview = "spec_review_required"
)

// ErrPermissionApprovalCanceled ends a run because the caller cancelled a
// permission prompt, rather than because anything failed.
//
// EXPORTED FOR THE SURFACES. A cancel is a normal ending and each surface says
// so its own way — the TUI unwinds, and ACP has a "cancelled" stop reason for
// exactly this. While it was unexported ACP could not distinguish it from a
// fault and reported it as an internal error, so declining a tool looked like
// the agent crashing.
var ErrPermissionApprovalCanceled = errors.New("permission approval cancelled")

// isImageRejectionError reports whether err is a provider 400 that rejects
// image/multimodal content. This is checked BEFORE the compaction-retry path
// so an image-rejection 400 doesn't loop endlessly (the user hit this with
// minimax-m3: text-only model, image attached, 400 loop until the process
// was killed).
func isImageRejectionError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	if !strings.Contains(msg, "400") {
		return false
	}
	for _, keyword := range []string{"image", "vision", "multimodal", "unsupported content type", "does not support"} {
		if strings.Contains(msg, keyword) {
			return true
		}
	}
	return false
}

type permissionRunState struct {
	mu       sync.Mutex
	cleanups []func()
}

func (state *permissionRunState) add(cleanup func()) {
	if state == nil || cleanup == nil {
		return
	}
	state.mu.Lock()
	state.cleanups = append(state.cleanups, cleanup)
	state.mu.Unlock()
}

func (state *permissionRunState) cleanup() {
	if state == nil {
		return
	}
	state.mu.Lock()
	cleanups := state.cleanups
	state.cleanups = nil
	state.mu.Unlock()
	for i := len(cleanups) - 1; i >= 0; i-- {
		cleanups[i]()
	}
}

// abortedToolResultNotice is the placeholder tool result recorded for a tool
// call that was advertised by the assistant turn but never executed because the
// repeated-failure guard halted the run first. It keeps every tool_use paired
// with a tool_result so the transcript stays valid for a strict provider replay.
const abortedToolResultNotice = "aborted: run halted by the repeated-failure guard"

// droppedToolCallNotice tells the model a tool call it attempted was malformed
// (missing a tool name) and dropped before execution, so it re-issues a valid
// call instead of assuming the call ran. It is surfaced both when a turn yields
// ONLY a dropped call and when a turn mixes valid calls with a dropped one.
const droppedToolCallNotice = "Your previous tool call was malformed (it was missing a tool name) and was not executed. " +
	"Re-issue the tool call with a valid tool name and JSON arguments, or reply with your final answer."

// escalationFailedNoticePrefix introduces the brief, user-role note recorded
// when a requested mid-run model switch could not be performed (the
// ModelSwitcher returned an error). The run continues on the current model;
// escalation is best-effort, never fatal.
const escalationFailedNoticePrefix = "Note: could not switch to the requested model"

// The system prompt (core coding-craft instructions + workspace context + safety
// confirmation policy) is assembled in system_prompt.go via buildSystemPrompt.

func Run(ctx context.Context, prompt string, provider Provider, options Options) (result Result, err error) {
	if provider == nil {
		return Result{}, errors.New("agent provider is required")
	}

	// Tracing is opt-in. When a recorder is wired, thread it into ctx so the
	// providerio seam and reconnect helper can reach it via trace.FromContext,
	// mark the run's start, and wrap OnUsage so token counters accumulate for
	// free alongside the existing per-request plumbing. nil recorder leaves the
	// loop byte-identical: FromContext returns nil, the no-op stamps cost
	// nothing, and the OnUsage wrapper is not installed.
	if options.Trace != nil {
		ctx = trace.WithContext(ctx, options.Trace)
		options.Trace.Start()
		originalOnUsage := options.OnUsage
		options.OnUsage = func(usage Usage) {
			if originalOnUsage != nil {
				originalOnUsage(usage)
			}
			options.Trace.Counter(trace.CounterInputTokens, int64(usage.InputTokens))
			options.Trace.Counter(trace.CounterCachedInputTokens, int64(usage.CachedInputTokens))
			options.Trace.Counter(trace.CounterCacheWriteTokens, int64(usage.CacheWriteTokens))
			options.Trace.Counter(trace.CounterOutputTokens, int64(usage.OutputTokens))
		}
	}

	// Establish the run's turn session — the seam an optimized provider session
	// (connection reuse, prewarm, native compaction) plugs into without touching
	// this loop. Default: wrap the passed provider so the session's Stream IS
	// provider.StreamCompletion, byte-identical to calling it directly. Opened
	// before dispatchSessionStart so a failed open is a clean run-start error
	// with no session hooks fired and nothing to unwind.
	turnSessions := options.TurnSessionProvider
	if turnSessions == nil {
		turnSessions = zeroruntime.NewProviderTurnSessionProvider(provider, zeroruntime.ProviderCapabilities{})
	}
	session, sessionErr := turnSessions.OpenTurnSession(ctx)
	if sessionErr != nil {
		return Result{}, fmt.Errorf("agent: open turn session: %w", sessionErr)
	}
	// Closed via closure (not `defer session.Close()`) so after a mid-run model
	// swap the CURRENT session is the one closed at teardown — the swap closes
	// the old one inline. Close errors are advisory and never fail the run.
	defer func() { _ = session.Close() }()
	// Best-effort prewarm: the default adapter no-ops; a real session may prime
	// a connection. Failure is ignored by contract (never fatal).
	_ = session.Prewarm(ctx)
	// Route ALL provider I/O — per-turn streams, mid-stream retries, the
	// compaction summarizer, and the final-answer request — through the session
	// by reassigning the loop's provider local. Every existing call site reads
	// this local, so no signatures change anywhere downstream.
	provider = sessionProvider{session: session}

	maxTurns := options.MaxTurns
	if maxTurns <= 0 {
		maxTurns = 12
	}

	registry := options.Registry
	if registry == nil {
		registry = tools.NewRegistry()
	}

	permissionMode := options.PermissionMode
	if permissionMode == "" {
		permissionMode = PermissionModeAuto
	}
	runPermissions := &permissionRunState{}
	options.runPermissions = runPermissions
	defer runPermissions.cleanup()

	promptParts := buildSystemPromptParts(options)
	planner := newContextPlanner(contextPlannerConfig{
		contextWindow:  options.ContextWindow,
		promptCacheKey: options.SessionID,
		serviceTier:    options.ServiceTier,
		promptParts:    promptParts,
	})
	messages := zeroruntime.SeedMessagesWithImages(promptParts.prompt, prompt, options.Images)

	guards := newGuardState()
	task := newTaskState(prompt, options.Trace)
	onPermission := options.OnPermission
	options.OnPermission = func(event PermissionEvent) {
		task.observe(taskStateEvent{kind: taskStateEventPermission, permission: event})
		if onPermission != nil {
			onPermission(event)
		}
	}
	compactor := newCompactionState(options, task)
	defer func() {
		// A final transcript comparison is observational. It records drift but
		// never rewrites the result or changes execution after the fact.
		task.observePlanParity(result.Messages)
		// Tool-result observations are intentionally coalesced; this final event
		// guarantees their aggregate counts are present even on an early return.
		task.emit()
	}()
	// The execution-profile posture controller. nil Options.Profile (every
	// existing caller) makes every observe/decide call a no-op, keeping the
	// loop byte-identical for unprofiled runs.
	posture := newProfileController(options.Profile)
	// applyPosture applies at most one posture escalation when an armed trigger
	// has fired: raise the hoisted turn ceiling, restore effort and the
	// completion gate, stamp the counter. Shared by the end-of-turn tail and
	// the completion-gate uncertain path (which continues the loop before the
	// tail runs). No-op forever after the first escalation and for unprofiled
	// runs.
	applyPosture := func() {
		if target, fired := posture.maybeEscalate(); fired {
			if target.MaxTurns > maxTurns {
				maxTurns = target.MaxTurns
			}
			if target.ReasoningEffort != "" {
				options.ReasoningEffort = target.ReasoningEffort
			} else if target.RestoreDefaultEffort {
				options.ReasoningEffort = ""
			}
			if target.RestoreCompletionGate {
				options.RequireCompletionSignal = true
			}
			options.Trace.Counter(trace.CounterPostureEscalations, 1)
		}
	}

	// Background post-edit diagnostics: files changed by mutating tools are
	// checked off the tool-call critical path and any errors are appended as a
	// nudge before the NEXT request (see async_diagnostics.go). nil when no
	// FileDiagnostics callback is wired; every method no-ops on nil.
	postEditDiagnostics := newAsyncDiagnostics(options.FileDiagnostics, options.Cwd)

	// loaded tracks deferred-eligible tools the model has pulled via tool_search
	// during THIS run. It is consulted by partitionTools each turn to expose a
	// loaded tool's full schema; it lives only for the run (v1 within-run scope).
	loaded := map[string]bool{}

	// The feature-gated completion policy keeps local completion decisions and its
	// bounded follow-up state out of the central loop. Self-correcting profiles
	// opt into the one permitted task-grounded semantic check.
	completionPolicy := newCompletionPolicy(options.SelfCorrect != nil)

	// toolDefCache memoizes each tool's rendered JSON-schema definition across
	// turns (a tool's advertised schema is stable for the run), so partitionTools
	// doesn't re-run the recursive schema→map conversion for every tool every turn.
	toolDefCache := map[string]zeroruntime.ToolDefinition{}

	result = Result{Messages: copyMessages(messages)}
	dispatchSessionStart(ctx, options)
	defer func() {
		// Use a context detached from ctx's cancellation: on Esc/Ctrl+C the run
		// context is already canceled by the time this defer runs, and a canceled
		// parent would make Hooks.Dispatch's context.WithTimeout derive an
		// already-expired context, so the sessionEnd hook would never actually run.
		dispatchSessionEnd(context.WithoutCancel(ctx), options, result, err)
	}()
	for turn := 0; turn < maxTurns; turn++ {
		result.Turns = turn + 1

		// Deliver background post-edit diagnostics from the previous turn's edits
		// BEFORE compaction so the nudge is part of the request being budgeted.
		// A brief wait at most (asyncDiagnosticsDrainTimeout); an unfinished check
		// simply delivers on a later turn.
		if nudge := postEditDiagnostics.drain(ctx); nudge != "" {
			messages = append(messages, zeroruntime.Message{
				Role:    zeroruntime.MessageRoleUser,
				Content: nudge,
			})
		}

		// Build the per-turn tool list first so proactive compaction can include
		// the tool-definition tokens (they ride on every request) in its estimate.
		// partitionTools depends only on registry/permissions/options/loaded, not on
		// the messages, so computing it before compaction is safe.
		toolPartitionSpan := options.Trace.Span(trace.SpanToolPartition)
		exposed, _ := partitionToolsCached(registry, permissionMode, options, loaded, toolDefCache)
		toolPartitionSpan.End()

		// PROACTIVE compaction: if the history is approaching the model's
		// context window, summarize the oldest middle before building the
		// request. A no-op when ContextWindow == 0 (compaction disabled).
		compactionSpan := options.Trace.Span(trace.SpanCompaction)
		messages = compactor.maybeCompact(ctx, provider, messages, exposed)
		compactionSpan.End()
		plan := planner.Plan(messages, exposed, options.ReasoningEffort)
		request := plan.Request
		recordContextPlan(options, plan)

		// A transient upstream disconnect on the initial connect is retried with
		// backoff (before any content is forwarded, so no OnText is duplicated);
		// a context-limit / image-rejection / cancellation is NOT retried here —
		// those fall through to their own handlers below.
		stream, err := streamWithReconnect(ctx, provider, request, reconnectNoticeFor(options))
		if err != nil {
			if isImageRejectionError(err) {
				result.Messages = copyMessages(messages)
				return result, fmt.Errorf("model %s rejected the image: %s. The model may not support image input — try switching to a vision-capable model (claude, gpt-4o, gemini)", options.Model, err.Error())
			}
			// REACTIVE compaction: a context-limit failure on the call itself
			// can be recovered by compacting once and retrying the same turn.
			if compacted, retried, retryErr := compactor.recover(ctx, provider, messages, request.Tools, err.Error()); retried {
				messages = compacted
				if retryErr != nil {
					result.Messages = copyMessages(messages)
					return result, retryErr
				}
				// Rebuild from the compacted messages but reuse the SAME active-mode
				// partition computed for this turn: it depends
				// on registry+loaded, not on the messages, so they stay valid after
				// compaction. Using the bare toolDefinitions here would route through an
				// empty-loaded partition, re-hiding every already-loaded deferred tool.
				retryPlan := planner.Plan(messages, exposed, options.ReasoningEffort)
				request = retryPlan.Request
				recordContextPlan(options, retryPlan)
				// Pre-content connect after a context-limit compaction: route through the
				// reconnect helper so a transient upstream hiccup here doesn't fail the
				// whole run and re-burn every token (AUDIT-L1).
				stream, err = streamWithReconnect(ctx, provider, request, reconnectNoticeFor(options))
			}
			if err != nil {
				result.Messages = copyMessages(messages)
				return result, err
			}
		}

		// forwardedVisibleText flags forwarded final PROSE (OnText) specifically —
		// the only output whose re-stream on a stall retry would visibly DUPLICATE
		// answer text the user already read. Transient working indicators
		// (reasoning previews, tool-call "writing…" previews) are deliberately NOT
		// counted: a turn that streamed only those then stalled — the common
		// gpt-5.x / ollama "froze mid-write_file" case — is safe to re-issue, since
		// the TUI resets the tool-call preview on the next tool-call-start and a
		// stalled turn is never appended to `messages` (it returns on the error
		// before the append), so the retry re-sends clean context with no
		// conversation-state duplication.
		forwardedVisibleText := false
		forwardingOpts := zeroruntime.CollectOptions{OnUsage: options.OnUsage}
		// Install text/reasoning forwarding handlers whenever EITHER a user
		// callback OR a trace recorder is set. A headless traced run (e.g. `zero
		// exec --trace`) sets Trace but no OnText/OnReasoning; without these
		// handlers the stream is still collected, but FirstTokenAt would never
		// stamp and the trace would lose its TTFT signal. The trace recorder's
		// stamp methods are nil-safe, but we guard on `trace != nil` so a run with
		// a user callback but no recorder still works. forwardedVisibleText stays
		// tied to the USER callback only, preserving the stall-retry semantics
		// (a trace-only handler forwards nothing the user would see duplicated).
		rec := options.Trace
		onText := options.OnText
		if onText != nil || rec != nil {
			forwardingOpts.OnText = func(s string) {
				if rec != nil {
					rec.StampFirstVisibleEvent()
					rec.StampFirstToken()
				}
				if onText != nil {
					// Zero-length chunks are not committed answer prose; counting
					// them would block an eligible mid-stream abort retry.
					if s != "" {
						forwardedVisibleText = true
					}
					onText(s)
				}
			}
		}
		onReasoning := options.OnReasoning
		if onReasoning != nil || rec != nil {
			forwardingOpts.OnReasoning = func(s string) {
				if rec != nil {
					rec.StampFirstToken()
				}
				if onReasoning != nil {
					onReasoning(s)
				}
			}
		}
		if options.OnToolCallStart != nil {
			forwardingOpts.OnToolCallStart = func(id, name string) { options.OnToolCallStart(id, name) }
		}
		if options.OnToolCallDelta != nil {
			forwardingOpts.OnToolCallDelta = func(id, fragment string) { options.OnToolCallDelta(id, fragment) }
		}
		// recoverStreamError applies the same non-stall recovery the initial stream
		// gets to ANY collected error — including one from a reissued (stall-retry)
		// stream: an image rejection gets the friendly wrapping, and a context limit
		// gets one compaction + reactive reissue (omitting visible callbacks, since
		// any pre-error output was already forwarded). It returns the possibly-updated
		// collected and a non-nil stop error when the run must end now.
		recoverStreamError := func(collected zeroruntime.CollectedStream) (zeroruntime.CollectedStream, error) {
			if isImageRejectionError(errors.New(collected.Error)) {
				return collected, fmt.Errorf("model %s rejected the image: %s. The model may not support image input — try switching to a vision-capable model (claude, gpt-4o, gemini)", options.Model, collected.Error)
			}
			// REACTIVE compaction: the streamed error may also be a context limit
			// (some providers surface it mid-stream). Compact and retry once.
			if compacted, retried, retryErr := compactor.recover(ctx, provider, messages, request.Tools, collected.Error); retried {
				messages = compacted
				if retryErr != nil {
					return collected, retryErr
				}
				// Reuse the SAME active-mode partition (exposed) from this turn rather
				// than the bare toolDefinitions: exposed depends on registry+loaded (not
				// the messages), so it stays valid after compaction.
				retryPlan := planner.Plan(messages, exposed, options.ReasoningEffort)
				retryRequest := retryPlan.Request
				recordContextPlan(options, retryPlan)
				retryStream, retryStreamErr := streamWithReconnect(ctx, provider, retryRequest, reconnectNoticeFor(options))
				if retryStreamErr != nil {
					return collected, retryStreamErr
				}
				genSpan := options.Trace.Span(trace.SpanGeneration)
				collected = zeroruntime.CollectStreamWithOptions(ctx, retryStream, zeroruntime.CollectOptions{
					OnUsage: options.OnUsage,
				})
				genSpan.End()
			}
			return collected, nil
		}

		generationSpan := options.Trace.Span(trace.SpanGeneration)
		collected := zeroruntime.CollectStreamWithOptions(ctx, stream, forwardingOpts)
		generationSpan.End()
		if collected.Error != "" {
			updated, stop := recoverStreamError(collected)
			collected = updated
			if stop != nil {
				result.Messages = copyMessages(messages)
				return result, stop
			}
		}
		// Check ctx first: on cancellation helpers.go sets collected.Error to
		// ctx.Err().Error(), so returning errors.New(collected.Error) would lose
		// the wrapped sentinel and break errors.Is(err, context.Canceled).
		if ctx.Err() != nil {
			result.Messages = copyMessages(messages)
			return result, ctx.Err()
		}
		// A stream idle/stall timeout OR mid-stream transport abort (#973 — wsarecv /
		// connection reset / unexpected EOF / connection closed / broken pipe) is
		// safely re-issued when the turn committed NO answer text — no forwarded
		// visible prose (forwardedVisibleText) and no collected final text
		// (collected.Text). The abort gate matches midStreamAbortNeedles only, not
		// the full shouldReconnect list: connect-phase timeouts and connection
		// refused are NOT retried here (a slow healthy server must not cost a
		// second prefill). This covers three cases:
		//   1. Nothing streamed at all before the connection died (the original
		//      macOS stale-pooled-connection hang past the response-header timeout).
		//   2. The model streamed transient reasoning and began a tool call (e.g. a
		//      large write_file) then froze mid-arguments — the common gpt-5.x /
		//      ollama heartbeat-pause stall. That partial tool call is NEVER executed
		//      (a turn with collected.Error returns before dispatch below) and NEVER
		//      appended to `messages`, so a retry re-issues clean context; the only
		//      re-render is transient previews, not duplicated answer text. This is
		//      why the gate no longer excludes collected.ToolCalls: an incomplete
		//      tool call from a timed-out stream is discard-and-retry, not output.
		//   3. Mid-stream socket abort after connect succeeded (Windows WSAECONNABORTED /
		//      wsarecv, connection reset by peer). Same no-tool-executed safety: the
		//      error returns before tool dispatch, so a bounded retry is safe.
		// A turn that forwarded real prose is NOT retried (it would duplicate visible
		// answer text) and falls through to the error return below. Capped +
		// exponential backoff, with a user-visible notice per attempt.
		for attempt := 1; attempt <= maxStreamStallRetries &&
			(isStreamTimeoutError(collected.Error) || isMidStreamTransportAbort(collected.Error)) &&
			!forwardedVisibleText &&
			collected.Text == ""; attempt++ {
			var notify reconnectNotifier
			if isStreamTimeoutError(collected.Error) {
				notify = stallRetryNoticeFor(options)
			} else {
				notify = reconnectNoticeFor(options)
				// Count this post-connect reissue even when the replacement
				// connect succeeds immediately. streamWithReconnect only
				// increments reconnect_count after a failed connect attempt.
				options.Trace.Counter(trace.CounterReconnectCount, 1)
			}
			if notify != nil {
				notify(attempt, maxStreamStallRetries)
			}
			if err := sleepWithContext(ctx, backoffFor(attempt)); err != nil {
				result.Messages = copyMessages(messages)
				return result, err
			}
			retryPlan := planner.Plan(messages, exposed, options.ReasoningEffort)
			retryRequest := retryPlan.Request
			recordContextPlan(options, retryPlan)
			retryStream, retryErr := streamWithReconnect(ctx, provider, retryRequest, reconnectNoticeFor(options))
			if retryErr != nil {
				result.Messages = copyMessages(messages)
				if ctx.Err() != nil {
					return result, ctx.Err()
				}
				return result, retryErr
			}
			stallGenSpan := options.Trace.Span(trace.SpanGeneration)
			collected = zeroruntime.CollectStreamWithOptions(ctx, retryStream, forwardingOpts)
			stallGenSpan.End()
		}
		// Recheck ctx after a retried CollectStream: helpers.go stores
		// ctx.Err().Error() in collected.Error, and errors.New of that string
		// drops the context.Canceled sentinel ACP branches on.
		if ctx.Err() != nil {
			result.Messages = copyMessages(messages)
			return result, ctx.Err()
		}
		if collected.Error != "" {
			// Route a reissued stream's non-stall error through the SAME recovery as
			// the initial stream (image-rejection wrapping / context-limit compaction)
			// rather than returning it raw.
			updated, stop := recoverStreamError(collected)
			collected = updated
			if stop != nil {
				result.Messages = copyMessages(messages)
				return result, stop
			}
			if ctx.Err() != nil {
				result.Messages = copyMessages(messages)
				return result, ctx.Err()
			}
			if collected.Error != "" {
				result.Messages = copyMessages(messages)
				return result, errors.New(collected.Error)
			}
		}

		// Calibrate the compaction token estimator against the provider's real
		// prompt-token count for the request we just sent, so later turns trigger
		// compaction near true capacity instead of ~15% early on code-heavy history.
		// Recompute the estimate HERE (not at request-build time): a reactive
		// compaction may have replaced `messages` with a smaller set and re-sent, so
		// this reflects the request that actually produced collected.Usage. The
		// assistant reply is appended below, after this, so `messages` is still the
		// sent request.
		compactor.calibrate(estimateTokens(messages)+estimateToolDefTokens(exposed), collected.Usage.InputTokens)

		// Carry the turn's terminal stop reason so a final answer cut off at the
		// output token cap (or by a content filter) is reported as truncated. A
		// tool-call turn normalizes to "" and clears any prior reason.
		result.FinishReason = collected.FinishReason

		messages = append(messages, zeroruntime.Message{
			Role:      zeroruntime.MessageRoleAssistant,
			Content:   collected.Text,
			ToolCalls: historySafeToolCalls(collected.ToolCalls),
			// Preserve thinking blocks so the next turn can replay them; providers
			// that use extended thinking reject tool conversations that drop them.
			Reasoning: collected.ReasoningBlocks,
		})

		if len(collected.ToolCalls) == 0 {
			// The model intended a tool call but it was malformed and dropped.
			// Tell it to retry rather than silently treating text as the answer.
			// This path is handled before the no-output guard so a dropped-call
			// turn is never counted as a runaway empty turn.
			if collected.DroppedToolCalls > 0 {
				messages = append(messages, zeroruntime.Message{
					Role:    zeroruntime.MessageRoleUser,
					Content: droppedToolCallNotice,
				})
				continue
			}
			// No-output guard: a turn with visible text is a real final answer.
			// A truly-empty turn (no text, no tool calls, no dropped calls) is
			// counted toward the runaway cap so we stop before burning maxTurns.
			if guards.observeTurn(collected) {
				result.FinalAnswer = noOutputStopAnswer(result.Turns)
				result.Messages = copyMessages(messages)
				return result, nil
			}
			if strings.TrimSpace(collected.Text) == "" {
				// Empty-but-under-cap turn: nudge the model to make progress
				// rather than treating the empty response as a final answer.
				messages = append(messages, zeroruntime.Message{
					Role: zeroruntime.MessageRoleUser,
					Content: "Your previous response had no visible output and no tool calls. " +
						"Continue the task by using a tool or reply with your final answer.",
				})
				continue
			}
			// Completion gate (headless): a turn with text but no tool call is the
			// model's final answer ONLY when the work is actually done. Default off
			// (RequireCompletionSignal), so interactive runs stay byte-identical.
			if options.RequireCompletionSignal {
				completionContext := task.completionContext(messages, guards.pendingPlanItems())
				evaluation := completionPolicy.evaluate(collected.Text, completionContext)
				task.observe(taskStateEvent{kind: taskStateEventCompletion, completion: evaluation})
				switch evaluation.Decision {
				case CompletionIncomplete:
					result.Incomplete = true
					result.IncompleteReason = evaluation.Reason
					result.FinalAnswer = collected.Text
					result.Messages = copyMessages(messages)
					return result, nil
				case CompletionUncertain:
					// Observe the uncertainty signal and apply any escalation NOW:
					// this branch continues the turn loop before the end-of-turn
					// act point, so deferring would skip escalation entirely.
					posture.observeUncertain()
					applyPosture()
					switch evaluation.Action {
					case completionActionContinue:
						options.Trace.Counter(trace.CounterCompletionNudges, 1)
						messages = append(messages, zeroruntime.Message{
							Role:    zeroruntime.MessageRoleUser,
							Content: continueNudge(evaluation.Reason),
						})
					case completionActionSemanticCheck:
						options.Trace.Counter(trace.CounterAcceptanceChecks, 1)
						messages = append(messages, zeroruntime.Message{
							Role:    zeroruntime.MessageRoleUser,
							Content: acceptanceVerificationNudge(completionContext.Objective),
						})
					}
					continue
				case CompletionComplete:
					// Local evidence is sufficient; proceed to final diagnostics.
				}
			} else {
				task.observe(taskStateEvent{kind: taskStateEventCompletion, completion: completionEvaluation{Decision: CompletionComplete}})
			}
			// Finalization diagnostics gate: edits from this run may still have
			// checks in flight — the per-turn drain waits only briefly and defers
			// to "a later turn", but a final answer means there is no later turn.
			// Wait out the full inline-era budget once; an introduced error gives
			// the model one more turn to see (and fix) it instead of being lost.
			// Free for runs that never edited: an idle collector returns "".
			if nudge := postEditDiagnostics.drainFinal(ctx); nudge != "" {
				messages = append(messages, zeroruntime.Message{
					Role:    zeroruntime.MessageRoleUser,
					Content: nudge,
				})
				continue
			}
			result.FinalAnswer = collected.Text
			result.Messages = copyMessages(messages)
			return result, nil
		}

		// A turn with tool calls is progress: update guard counters before
		// executing so the empty-turn counter resets and plan-tracking signals
		// stay current.
		guards.observeTurn(collected)
		for _, call := range collected.ToolCalls {
			if call.Name == planToolName {
				task.observe(taskStateEvent{kind: taskStateEventPlan, arguments: call.Arguments})
			}
		}

		failureHint := ""
		// turnRequestedModel records the FIRST mid-run escalation target requested
		// during this turn's tool batch. The actual provider switch happens once,
		// after the batch, so every tool_result is recorded first and at most one
		// switch occurs per turn.
		turnRequestedModel := ""
		// changedFilesThisBatch aggregates every file the turn's mutating tools
		// touched, so post-edit self-correction runs ONCE over the union after the
		// batch — one AfterEdit call keeps the per-run attempt budget accurate and
		// avoids verifying an intermediate edit a later call in the same turn already
		// superseded. Its feedback is appended after the loop so every advertised
		// tool call keeps its tool_result contiguous (a user message interleaved
		// between tool_results breaks strict provider replay) — same after-batch
		// rationale as turnRequestedModel above.
		var changedFilesThisBatch []string
		// Images produced by tools this turn, held until every tool_result is
		// recorded. They travel as user messages, and a user message between two
		// tool_results breaks strict provider replay — Anthropic coalesces them
		// into one user block list and requires the tool_result blocks first, so
		// Images ride a following USER message rather than the tool result
		// above. Every provider drops images on a tool-role message —
		// Anthropic's tool_result content is a string, Gemini's is a
		// functionResponse, and OpenAI guards its image parts to the user role
		// — so attaching them there would silently deliver nothing. A separate
		// message also keeps the one-tool-result-per-tool-call pairing intact,
		// which the providers validate. Vision-gate evaluation is deferred until
		// after any model switch this turn resolves, so images can reach an
		// escalated vision-capable model.
		var toolResultsWithImages []ToolResult
		buildToolImageMessages := func() []zeroruntime.Message {
			var out []zeroruntime.Message
			for _, tr := range toolResultsWithImages {
				if imageMessage, ok := toolResultImageMessage(tr, options); ok {
					out = append(out, imageMessage)
				}
			}
			return out
		}
		// Parallel read-ahead state: results for calls[precomputedStart:precomputedEnd]
		// executed concurrently, consumed strictly in order below.
		var precomputed []precomputedToolResult
		precomputedStart, precomputedEnd := 0, 0
		for index, call := range collected.ToolCalls {
			// When this call starts a consecutive run of >= 2 auto-allowed read-only
			// calls, execute the whole run concurrently now (see parallel_tools.go).
			// The scan happens lazily at the run's first index — never ahead of a
			// pending mutating call — so read-after-write ordering is preserved.
			if index >= precomputedEnd {
				// Capability-safe consecutive run (ReadOnly+ThreadSafe, no
				// resource-key conflicts). See extendParallelRun.
				runEnd := extendParallelRun(registry, collected.ToolCalls, index, options)
				if runEnd-index >= 2 {
					batchSpan := options.Trace.Span(trace.SpanToolExecution)
					precomputed = executeParallelReadBatch(ctx, registry, collected.ToolCalls, index, runEnd, permissionMode, options)
					batchSpan.End()
					precomputedStart, precomputedEnd = index, runEnd
				}
			}
			if options.OnToolCall != nil {
				options.OnToolCall(call)
			}
			options.Trace.StampFirstUsefulAction()
			var toolResult ToolResult
			var abortErr error
			if index >= precomputedStart && index < precomputedEnd {
				toolResult, abortErr = precomputed[index-precomputedStart].result, precomputed[index-precomputedStart].abortErr
			} else {
				toolSpan := options.Trace.Span(trace.SpanToolExecution)
				toolResult, abortErr = executeToolCall(ctx, registry, call, permissionMode, options)
				toolSpan.End()
			}
			options.Trace.Counter(trace.CounterToolCalls, 1)
			recordOutputBudgetTrace(options.Trace, toolResult)
			task.observe(taskStateEvent{kind: taskStateEventToolResult, arguments: call.Arguments, toolResult: toolResult})
			if options.OnToolResult != nil {
				options.OnToolResult(toolResult)
			}
			// Union the deferred tools this result asked to load into the per-run
			// set BEFORE any abort/stop/guard branch, so a load that coincides with
			// a turn-ending result is still recorded for the next turn's partition.
			for _, name := range toolResult.LoadedTools {
				loaded[name] = true
			}
			if turnRequestedModel == "" && toolResult.RequestedModel != "" {
				turnRequestedModel = toolResult.RequestedModel
			}
			messages = append(messages, zeroruntime.Message{
				Role:               zeroruntime.MessageRoleTool,
				Content:            toolResult.ModelOutput(),
				ToolCallID:         toolResult.ToolCallID,
				ToolCallProviderID: call.ProviderCallID,
				IsError:            toolResult.Status == tools.StatusError,
				ChangedFiles:       append([]string(nil), toolResult.ChangedFiles...),
			})
			if len(toolResult.Images) > 0 {
				toolResultsWithImages = append(toolResultsWithImages, toolResult)
			}

			// A tool may demand the run ABORT — a canceled/timed-out ask_user prompt
			// returns context.Canceled rather than fabricating a headless answer. Stop
			// promptly with that error (or run-context cancellation), keeping messages
			// valid by closing out the still-advertised calls first.
			if abortErr == nil && ctx.Err() != nil {
				abortErr = ctx.Err()
			}
			if abortErr != nil {
				messages = appendAbortedToolResults(messages, collected.ToolCalls[index+1:])
				messages = append(messages, buildToolImageMessages()...)
				result.Messages = copyMessages(messages)
				return result, abortErr
			}
			if stopReason := stopReasonFromToolResult(toolResult); stopReason != "" {
				messages = appendAbortedToolResults(messages, collected.ToolCalls[index+1:])
				messages = append(messages, buildToolImageMessages()...)
				result.FinalAnswer = toolResult.ModelOutput()
				result.StopReason = stopReason
				result.Messages = copyMessages(messages)
				return result, nil
			}

			// Repeated-failure guard: if a tool keeps failing the same way, hint
			// once (with its schema) then halt — so no model loops on a bad call.
			// Only RETRIABLE failures (bad arguments / execution errors) drive it:
			// policy refusals (disabled tool, permission denial, sandbox block)
			// aren't fixed by reformatting the call, so a "match this schema" hint
			// would misdirect the model toward JSON shape or blocked behavior.
			retriableFailure := isRetriableToolError(toolResult)
			outcome := guards.observeToolResult(call.Name, retriableFailure, toolResult.ModelOutput())
			posture.observeToolOutcome(outcome, toolResult)
			if outcome.Stop {
				// The assistant message advertised EVERY collected tool call, but
				// the guard halts mid-turn so the calls after this one never run.
				// Append an aborted placeholder result for each remaining call so
				// every tool_use has a matching tool_result and the recorded
				// messages stay valid for a strict provider replay (Anthropic
				// rejects a tool_use with no answering tool_result).
				messages = appendAbortedToolResults(messages, collected.ToolCalls[index+1:])
				messages = append(messages, buildToolImageMessages()...)
				result.FinalAnswer = toolFailureStopAnswer(call.Name, outcome.Count)
				result.Messages = copyMessages(messages)
				return result, nil
			}
			if outcome.InjectHint && failureHint == "" {
				failureHint = toolFailureHint(call.Name, toolSchemaJSON(registry, call.Name), toolResult.ModelOutput())
			}

			// Collect the files this successful mutating tool changed. Self-correct
			// verification runs once over the union after the batch; background
			// diagnostics start NOW so the language server analyzes while the rest
			// of the batch executes. A read-only tool (no ChangedFiles) never
			// contributes.
			if toolResult.Status == tools.StatusOK && len(toolResult.ChangedFiles) > 0 {
				changedFilesThisBatch = append(changedFilesThisBatch, toolResult.ChangedFiles...)
				postEditDiagnostics.enqueue(ctx, toolResult.ChangedFiles)
			}
		}

		// Run post-edit self-correction once over the union of files this turn
		// changed, then append any feedback after every tool_result is recorded so
		// the assistant's tool_results stay contiguous (a user message between
		// tool_results breaks strict provider replay). nil SelfCorrect is a no-op.
		if options.SelfCorrect != nil && len(changedFilesThisBatch) > 0 {
			feedback, selfCorrectOutcome := options.SelfCorrect.AfterEdit(ctx, dedupeStrings(changedFilesThisBatch))
			posture.observeSelfCorrect(selfCorrectOutcome)
			task.observe(taskStateEvent{kind: taskStateEventVerification, verification: selfCorrectOutcome})
			if feedback != "" {
				messages = append(messages, zeroruntime.Message{
					Role:    zeroruntime.MessageRoleUser,
					Content: feedback,
				})
			}
		}

		// Mid-run model escalation: if a tool asked to switch models this turn and
		// a switcher is wired, rebuild the provider on the new model for the rest
		// of the run. At most one switch per turn (first request wins). A switcher
		// error is NON-FATAL — record a brief note and continue on the current
		// model. nil switcher ⇒ requests are ignored entirely (escalation off).
		if turnRequestedModel != "" && (options.ModelSessionSwitcher != nil || options.ModelSwitcher != nil) {
			// Resolve the escalated model's session source. The target-aware
			// ModelSessionSwitcher is preferred: its TurnSessionProvider carries an
			// optimized session (and capabilities) across the swap. The legacy
			// ModelSwitcher fallback returns a bare Provider, wrapped in the
			// default no-op session — identical to pre-seam behavior.
			var newSessions zeroruntime.TurnSessionProvider
			var switchErr error
			if options.ModelSessionSwitcher != nil {
				newSessions, switchErr = options.ModelSessionSwitcher(ctx, turnRequestedModel)
			} else {
				var newProvider Provider
				newProvider, switchErr = options.ModelSwitcher(ctx, turnRequestedModel)
				if switchErr == nil && newProvider != nil {
					newSessions = zeroruntime.NewProviderTurnSessionProvider(newProvider, zeroruntime.ProviderCapabilities{})
				}
			}
			if switchErr != nil {
				messages = append(messages, zeroruntime.Message{
					Role:    zeroruntime.MessageRoleUser,
					Content: escalationFailedNoticePrefix + " (" + turnRequestedModel + "): " + switchErr.Error() + ". Continuing on " + options.Model + ".",
				})
			} else if newSessions != nil {
				// Open the new session and close the old one, so the next turn's
				// streams and compaction use the new model. For the default adapter
				// the open never errors and Close is a no-op, so this stays
				// byte-identical to a direct provider reassignment; the guarded open
				// and prewarm matter once a session holds real state.
				newSession, openErr := newSessions.OpenTurnSession(ctx)
				if openErr != nil {
					messages = append(messages, zeroruntime.Message{
						Role:    zeroruntime.MessageRoleUser,
						Content: escalationFailedNoticePrefix + " (" + turnRequestedModel + "): " + openErr.Error() + ". Continuing on " + options.Model + ".",
					})
				} else {
					_ = session.Close()
					session = newSession
					_ = session.Prewarm(ctx)
					provider = sessionProvider{session: session}
					options.Model = turnRequestedModel
					// Service tiers are model capabilities. A successful escalation may
					// target a model that does not support the tier selected before the
					// switch, so let the destination use its default.
					options.ServiceTier = ""
					planner.config.serviceTier = ""
					options.Trace.Counter(trace.CounterModelSwitches, 1)
					// Re-derive the context window for the new model so compaction and
					// the OnContext budget report track the model actually in force —
					// without this a switch to a smaller-window model can overflow the
					// target, and a switch to a larger one over-compacts. An unknown
					// model (<= 0) keeps the original window. The compactor also
					// re-resolves its dedicated summarizer against the new model.
					window := 0
					if options.ContextWindowFor != nil {
						window = options.ContextWindowFor(turnRequestedModel)
					}
					if window > 0 {
						options.ContextWindow = window
						planner.config.contextWindow = window
					}
					compactor.switchModel(turnRequestedModel, window)
				}
			}
		}

		// Every tool_result, self-correct notice, and model-switch update for this
		// turn is now in place; evaluate and append image messages against the
		// effective (potentially escalated) model.
		messages = append(messages, buildToolImageMessages()...)
		toolResultsWithImages = nil

		// A turn can mix valid tool calls with a dropped (nameless) one. The valid
		// calls executed above; surface the dropped call too so it is never
		// silently ignored just because the turn also did real work. This is
		// independent of (and additive to) the failure-hint / plan-reminder nudges.
		if collected.DroppedToolCalls > 0 {
			messages = append(messages, zeroruntime.Message{
				Role:    zeroruntime.MessageRoleUser,
				Content: droppedToolCallNotice,
			})
		}

		// A repeated-failure hint (schema + exact error) takes priority over the
		// planning reminders — fixing the failing call matters more than plan
		// hygiene. Both are light, one-shot, user-role nudges.
		if failureHint != "" {
			messages = append(messages, zeroruntime.Message{
				Role:    zeroruntime.MessageRoleUser,
				Content: failureHint,
			})
		} else if reminder := guards.progressReminder(); reminder != "" {
			messages = append(messages, zeroruntime.Message{
				Role:    zeroruntime.MessageRoleUser,
				Content: reminder,
			})
		} else if reminder := guards.planReminder(result.Turns); reminder != "" {
			messages = append(messages, zeroruntime.Message{
				Role:    zeroruntime.MessageRoleUser,
				Content: reminder,
			})
		}

		// One-shot posture escalation: when an armed profile trigger fired this
		// turn, restore the stricter knob values the profile displaced. Mutates
		// only per-turn-read policy (the hoisted turn ceiling, reasoning effort,
		// completion gate); never messages, model, or session. No-op forever
		// after the first escalation and for every unprofiled run.
		applyPosture()
	}

	if ctx.Err() != nil {
		result.Messages = copyMessages(messages)
		return result, ctx.Err()
	}
	// Reaching here means the loop hit the maxTurns ceiling — the agent was cut off
	// mid-run, not a natural completion (a finished run returns via the no-tool-call
	// success path before this). Under the headless completion gate that is INCOMPLETE,
	// not success, so a run that loops to the turn limit isn't reported as done.
	//
	// The final-answer call is one more model request, so diagnostics from the
	// LAST turn's edits get a drain too — with the finalization budget, since
	// there is no later turn to defer to — otherwise an error introduced by
	// the final edit would go unreported in the summary.
	if nudge := postEditDiagnostics.drainFinal(ctx); nudge != "" {
		messages = append(messages, zeroruntime.Message{
			Role:    zeroruntime.MessageRoleUser,
			Content: nudge,
		})
	}
	finalExposed, _ := partitionToolsCached(registry, permissionMode, options, loaded, toolDefCache)
	if answer, finalMessages, finishReason := finalAnswerAfterMaxTurns(ctx, provider, planner, messages, finalExposed, options); strings.TrimSpace(answer) != "" {
		result.FinalAnswer = answer
		result.FinishReason = finishReason
		result.Messages = copyMessages(finalMessages)
		if options.RequireCompletionSignal {
			result.Incomplete = true
			result.IncompleteReason = "reached the max-turns limit without completing"
		}
		return result, nil
	}

	result.FinalAnswer = maxTurnsAnswer
	result.Messages = copyMessages(messages)
	if options.RequireCompletionSignal {
		result.Incomplete = true
		result.IncompleteReason = "reached the max-turns limit without a final answer"
	}
	return result, nil
}

func recordOutputBudgetTrace(recorder *trace.Recorder, result ToolResult) {
	if recorder == nil || result.Meta["output_budget_category"] == "" {
		return
	}
	if result.Outcome.Finalized() {
		diagnostics := result.Outcome.Diagnostics
		recorder.EmitOutputBudget(trace.OutputBudgetEvent{
			Tool:                    result.Name,
			Category:                diagnostics.Category,
			OriginalBytes:           diagnostics.OriginalBytes,
			RetainedBytes:           diagnostics.ModelBytes,
			EstimatedOriginalTokens: diagnostics.EstimatedOriginalTokens,
			EstimatedRetainedTokens: diagnostics.EstimatedModelTokens,
			Truncated:               diagnostics.Truncated,
			Reason:                  diagnostics.Reason,
			SpillCreated:            result.Outcome.Artifact != nil,
		})
		return
	}
	parseInt := func(key string) int {
		value, _ := strconv.Atoi(result.Meta[key])
		return value
	}
	spillCreated, _ := strconv.ParseBool(result.Meta["output_budget_spill_created"])
	recorder.EmitOutputBudget(trace.OutputBudgetEvent{
		Tool:                    result.Name,
		Category:                result.Meta["output_budget_category"],
		OriginalBytes:           parseInt("output_budget_original_bytes"),
		RetainedBytes:           parseInt("output_budget_retained_bytes"),
		EstimatedOriginalTokens: parseInt("output_budget_estimated_original_tokens"),
		EstimatedRetainedTokens: parseInt("output_budget_estimated_retained_tokens"),
		Truncated:               result.Truncated,
		Reason:                  result.Meta["output_budget_reason"],
		SpillCreated:            spillCreated,
	})
}

func recordContextPlan(options Options, plan contextPlan) {
	recordContextPlanTrace(options.Trace, plan)
	if options.OnContext != nil {
		options.OnContext(plan.Breakdown)
	}
}

func recordContextPlanTrace(recorder *trace.Recorder, plan contextPlan) {
	if recorder == nil {
		return
	}
	fingerprint := plan.PrefixFingerprint
	recorder.EmitPrefixHash(trace.PrefixHash{
		SystemPromptHash:       fingerprint.SystemPromptHash,
		BaseInstructionsHash:   fingerprint.BaseInstructionsHash,
		ConfirmationPolicyHash: fingerprint.ConfirmationPolicyHash,
		ProjectContextHash:     fingerprint.ProjectContextHash,
		SkillsHash:             fingerprint.SkillsHash,
		ToolsHash:              fingerprint.ToolsHash,
		SchemaHash:             fingerprint.SchemaHash,
		CompletePrefixHash:     fingerprint.CompletePrefixHash,
		InvalidationReason:     plan.Breakdown.PrefixInvalidationReason,
	})
}

func finalAnswerAfterMaxTurns(ctx context.Context, provider Provider, planner *contextPlanner, messages []zeroruntime.Message, toolDefs []zeroruntime.ToolDefinition, options Options) (string, []zeroruntime.Message, string) {
	finalMessages := copyMessages(messages)
	finalMessages = append(finalMessages, zeroruntime.Message{
		Role:    zeroruntime.MessageRoleUser,
		Content: maxTurnsFinalAnswerPrompt,
	})
	// The max-turns final-answer call is a pre-content connect, often after a long
	// autonomous/cron run — route it through the reconnect helper so a single
	// transient hiccup doesn't drop the final summary (AUDIT-L1).
	plan := planner.Plan(finalMessages, toolDefs, options.ReasoningEffort)
	recordContextPlan(options, plan)
	stream, err := streamWithReconnect(ctx, provider, plan.Request, reconnectNoticeFor(options))
	if err != nil {
		return "", messages, ""
	}
	finalGenSpan := options.Trace.Span(trace.SpanGeneration)
	collected := zeroruntime.CollectStreamWithOptions(ctx, stream, zeroruntime.CollectOptions{
		OnText:          options.OnText,
		OnReasoning:     options.OnReasoning,
		OnUsage:         options.OnUsage,
		OnToolCallStart: options.OnToolCallStart,
		OnToolCallDelta: options.OnToolCallDelta,
	})
	finalGenSpan.End()
	if ctx.Err() != nil || collected.Error != "" || strings.TrimSpace(collected.Text) == "" {
		return "", messages, ""
	}
	finalMessages = append(finalMessages, zeroruntime.Message{
		Role:    zeroruntime.MessageRoleAssistant,
		Content: collected.Text,
	})
	return collected.Text, finalMessages, collected.FinishReason
}

func historySafeToolCalls(calls []ToolCall) []ToolCall {
	if len(calls) == 0 {
		return nil
	}
	safe := make([]ToolCall, len(calls))
	for i, call := range calls {
		safe[i] = call
		if call.Freeform {
			continue
		}
		args := strings.TrimSpace(call.Arguments)
		switch {
		case args == "":
			safe[i].Arguments = "{}"
		case json.Valid([]byte(args)):
			// already a single valid JSON value — keep as-is
		default:
			// Recover the first object for the concatenated case so the replayed
			// transcript matches what executeToolCall actually ran; otherwise (real
			// corruption) fall back to an empty object.
			if first, ok := recoverableToolArguments(args); ok {
				safe[i].Arguments = first
			} else {
				safe[i].Arguments = "{}"
			}
		}
	}
	return safe
}

// toolSchemaJSON renders a tool's parameter schema as readable JSON for the
// repeated-failure corrective hint, so the model can see exactly what arguments
// the tool expects. Returns "{}" if the tool or schema is unavailable.
func toolSchemaJSON(registry *tools.Registry, name string) string {
	tool, ok := registry.Get(name)
	if !ok {
		return "{}"
	}
	data, err := json.MarshalIndent(schemaToRuntimeMap(tool.Parameters()), "", "  ")
	if err != nil {
		return "{}"
	}
	return string(data)
}

// executeToolCall runs one tool call and returns its result. The second return
// is a non-nil ABORT error only when the call demands the whole run stop (a
// canceled/timed-out ask_user prompt) rather than continuing — every ordinary
// success or tool error returns a nil abort error.
// recoverableToolArguments inspects a tool call's raw JSON arguments. It returns
// the raw text of the FIRST JSON value when the payload is one value optionally
// followed by additional WHOLE JSON values (the weak-model concatenated case, e.g.
// `{A}{B}`). ok is false when the payload is genuinely malformed: a bad/truncated
// first value, OR trailing NON-JSON garbage after a valid first value (e.g.
// `{"x":1}xyz`) — that case must still error, not be silently accepted. Sharing
// this between dispatch and replay-history keeps them consistent: only a cleanly
// recoverable first object is ever used.
func recoverableToolArguments(arguments string) (first string, ok bool) {
	dec := json.NewDecoder(strings.NewReader(arguments))
	var head json.RawMessage
	if err := dec.Decode(&head); err != nil {
		return "", false
	}
	// The remainder must be only whole JSON values (or nothing); any decode error
	// other than EOF means trailing garbage — reject so corruption still surfaces.
	for {
		var rest json.RawMessage
		if err := dec.Decode(&rest); err != nil {
			if errors.Is(err, io.EOF) {
				return strings.TrimSpace(string(head)), true
			}
			return "", false
		}
	}
}

// decodeToolArguments decodes a tool call's JSON arguments into v. It tolerates a
// weaker model that packs MULTIPLE concatenated top-level JSON objects into one
// arguments string (`{A}{B}`) — the cause of "invalid character '{' after top-level
// value" failures with small models like minimax-m3 — by decoding the FIRST object
// and ignoring the trailing WHOLE JSON values, so the primary intended call runs.
// A genuinely malformed payload (bad/truncated first object, or non-JSON trailing
// garbage) still returns the real parse error; a standard single-object payload
// decodes exactly as before.
func decodeToolArguments(arguments string, v any) error {
	if first, ok := recoverableToolArguments(arguments); ok {
		return json.Unmarshal([]byte(first), v)
	}
	// Not cleanly recoverable — surface the genuine parse error.
	return json.Unmarshal([]byte(arguments), v)
}

func executeToolCall(ctx context.Context, registry *tools.Registry, call ToolCall, permissionMode PermissionMode, options Options) (ToolResult, error) {
	tool, toolFound := registry.Get(call.Name)
	args := map[string]any{}
	if call.Freeform {
		if !toolFound || !tools.IsBuiltInApplyPatch(tool) {
			return ToolResult{
				ToolCallID: call.ID,
				Name:       call.Name,
				Status:     tools.StatusError,
				Output:     "Error: Unsupported freeform tool call for " + call.Name + ".",
			}, nil
		}
		prepared, err := tools.PrepareFreeformApplyPatchArguments(tool, call.Arguments)
		if err != nil {
			return ToolResult{
				ToolCallID: call.ID,
				Name:       call.Name,
				Status:     tools.StatusError,
				Output:     "Error: Invalid freeform apply_patch input: " + err.Error(),
			}, nil
		}
		args = prepared
	} else if call.Arguments != "" {
		if err := decodeToolArguments(call.Arguments, &args); err != nil {
			return ToolResult{
				ToolCallID: call.ID,
				Name:       call.Name,
				Status:     tools.StatusError,
				Output:     "Error: Failed to parse arguments for " + call.Name + ": " + err.Error(),
			}, nil
		}
	}
	if isShellCommandTool(call.Name) {
		normalizeNoopAdditionalPermissions(args, options.Cwd, options.Sandbox)
	}
	// tool_search is the gateway to the allowlisted deferred tools, so a non-empty
	// EnabledTools allowlist that omits it must NOT reject the call — otherwise the
	// discovery tool exposed to the model rejects at dispatch time (an inescapable
	// dead-end). The allowlist is exempted; an explicit DisabledTools entry for
	// tool_search is still honored (only the allowlist is exempted, not the denylist).
	toolSearchAllowed := call.Name == tools.ToolSearchToolName && !containsToolName(options.DisabledTools, tools.ToolSearchToolName)
	if !toolSearchAllowed && !ToolAllowedByFilters(call.Name, options.EnabledTools, options.DisabledTools) {
		return ToolResult{
			ToolCallID:   call.ID,
			Name:         call.Name,
			Status:       tools.StatusError,
			Output:       `Error: Tool "` + call.Name + `" is not enabled for this run.`,
			DenialReason: DenialFiltered,
		}, nil
	}
	if (permissionMode == PermissionModeSpecDraft || permissionMode == PermissionModePlan) && toolFound && !ToolAdvertised(tool, permissionMode) {
		modeName := string(permissionMode)
		return ToolResult{
			ToolCallID:   call.ID,
			Name:         call.Name,
			Status:       tools.StatusError,
			Output:       `Error: Tool "` + call.Name + `" is not available in ` + modeName + ` mode.`,
			DenialReason: DenialFiltered,
		}, nil
	}
	if toolFound {
		if rejecter, ok := tool.(tools.PrePermissionRejecter); ok {
			if result, rejected := rejecter.RejectBeforePermission(args); rejected {
				return toolResultFromPrePermissionReject(call, result), nil
			}
		}
	}

	// A shell command that sets sandbox_permissions: with_additional_permissions
	// must carry a valid additional_permissions payload; inlineAdditionalPermissionsProfile
	// is the single source of truth for that shape, and buildPermissionEvent (below)
	// calls it again to render the prompt's scope text. Check it here, before any
	// permission prompt is shown: a malformed payload can never be satisfied no
	// matter what the user decides, so surfacing it as a plain tool error (which the
	// model can see and retry) is both clearer and avoids presenting a normal-looking
	// prompt whose "allow" path is guaranteed to fail with a confusing denial.
	if toolFound && isShellCommandTool(call.Name) {
		if _, _, err := inlineAdditionalPermissionsProfile(args, options.Cwd); err != nil {
			return ToolResult{
				ToolCallID: call.ID,
				Name:       call.Name,
				Status:     tools.StatusError,
				Output:     "Error: " + err.Error(),
			}, nil
		}
	}

	// ask_user is intercepted in the loop (like permissions) so the question can
	// be routed to an interactive front-end instead of blocking inside the tool.
	// When no front-end is wired up it degrades to the tool's own graceful Run().
	if call.Name == "ask_user" {
		return executeAskUser(ctx, registry, call, args, permissionMode, options)
	}
	if call.Name == tools.RequestPermissionsToolName {
		return executeRequestPermissions(ctx, call, args, permissionMode, options)
	}

	permissionGranted := permissionMode == PermissionModeUnsafe
	if toolFound && effectivePermission(tool, args) == tools.PermissionAllow {
		permissionGranted = true
	}

	decisionReason := ""
	var decisionAction PermissionDecisionAction
	var decisionCommandPrefix []string
	var permissionCleanups []func()
	defer func() {
		for i := len(permissionCleanups) - 1; i >= 0; i-- {
			if permissionCleanups[i] != nil {
				permissionCleanups[i]()
			}
		}
	}()
	if toolFound && !permissionGranted {
		if grant, ok, session := matchCommandPrefix(call.Name, args, options); ok {
			permissionGranted = true
			if session {
				decisionAction = PermissionDecisionAllowPrefix
				decisionReason = "session command prefix approval matched"
			} else {
				decisionAction = PermissionDecisionAlwaysAllowPrefix
				decisionReason = "persistent command prefix approval matched"
			}
			decisionCommandPrefix = grant.Prefix
		}
	}
	// An explicit additional-permissions request is an elevation only when it
	// expands the current effective policy. If the same capability was already
	// approved for this session, reuse that grant across equivalent commands
	// instead of prompting again merely because their command prefixes differ.
	if toolFound && !permissionGranted && isShellCommandTool(call.Name) && options.Sandbox != nil {
		if profile, requested, profileErr := inlineAdditionalPermissionsProfile(args, options.Cwd); profileErr == nil && requested && options.Sandbox.CoversRequestPermissions(profile) {
			permissionGranted = true
			decisionAction = PermissionDecisionAllowForSession
			decisionReason = "requested sandbox capabilities already granted for this session"
		}
	}

	var preflightDecision *sandbox.Decision
	if toolFound && options.Sandbox != nil {
		decision := options.Sandbox.Evaluate(ctx, sandboxRequest(call.Name, tool, args, permissionGranted, permissionMode, options))
		preflightDecision = &decision
	}

	if toolFound && options.OnPermissionRequest != nil && shouldRequestPermission(tool, args, permissionGranted, preflightDecision) {
		requestEvent, ok := buildPermissionEvent(call, tool, args, permissionGranted, permissionMode, options, preflightDecision)
		if !ok {
			requestEvent = fallbackPermissionEvent(call, tool, args, permissionMode, options)
		}
		request := permissionRequestFromEvent(requestEvent, args, options)
		decision, err := requestPermission(ctx, request, options)
		if err != nil {
			decision = PermissionDecision{Action: PermissionDecisionDeny, Reason: err.Error()}
		}
		decision.Action = normalizePermissionDecisionAction(decision.Action)
		decisionReason = strings.TrimSpace(decision.Reason)
		decisionAction = decision.Action
		switch decision.Action {
		case PermissionDecisionAllow, PermissionDecisionAllowStrict:
			permissionGranted = true
			requestEvent.DecisionAction = decision.Action
			cleanup, err := grantNetworkForSandboxPrompt(requestEvent, sandbox.PermissionGrantScopeTurn, options)
			if err != nil {
				reason := "failed to grant network permission: " + err.Error()
				emitDeniedPermission(options, call, requestEvent, reason)
				return deniedPermissionResult(call, reason, requestEvent), nil
			}
			permissionCleanups = append(permissionCleanups, cleanup)
			cleanup, err = grantInlineAdditionalPermissions(args, sandbox.PermissionGrantScopeTurn, options)
			if err != nil {
				reason := "failed to grant additional permissions: " + err.Error()
				emitDeniedPermission(options, call, requestEvent, reason)
				return deniedPermissionResult(call, reason, requestEvent), nil
			}
			permissionCleanups = append(permissionCleanups, cleanup)
			cleanup, err = grantFilesystemForSandboxPrompt(requestEvent, sandbox.PermissionGrantScopeTurn, options)
			if err != nil {
				reason := "failed to grant filesystem permission: " + err.Error()
				emitDeniedPermission(options, call, requestEvent, reason)
				return deniedPermissionResult(call, reason, requestEvent), nil
			}
			permissionCleanups = append(permissionCleanups, cleanup)
		case PermissionDecisionAllowForSession:
			permissionGranted = true
			requestEvent.DecisionAction = decision.Action
			if shellCommandAdditionalPermissionsRequested(args) {
				cleanup, err := grantInlineAdditionalPermissions(args, sandbox.PermissionGrantScopeSession, options)
				if err != nil {
					reason := "failed to grant session additional permissions: " + err.Error()
					emitDeniedPermission(options, call, requestEvent, reason)
					return deniedPermissionResult(call, reason, requestEvent), nil
				}
				permissionCleanups = append(permissionCleanups, cleanup)
			} else if networkSandboxPrompt(requestEvent) {
				cleanup, err := grantNetworkForSandboxPrompt(requestEvent, sandbox.PermissionGrantScopeSession, options)
				if err != nil {
					reason := "failed to grant session network permission: " + err.Error()
					emitDeniedPermission(options, call, requestEvent, reason)
					return deniedPermissionResult(call, reason, requestEvent), nil
				}
				permissionCleanups = append(permissionCleanups, cleanup)
			} else if filesystemSandboxPrompt(requestEvent) {
				cleanup, err := grantFilesystemForSandboxPrompt(requestEvent, sandbox.PermissionGrantScopeSession, options)
				if err != nil {
					reason := "failed to grant session filesystem permission: " + err.Error()
					emitDeniedPermission(options, call, requestEvent, reason)
					return deniedPermissionResult(call, reason, requestEvent), nil
				}
				permissionCleanups = append(permissionCleanups, cleanup)
			} else if options.Sandbox != nil {
				// The current call stays allowed if recording the session grant
				// fails; the user is simply prompted again for a later match.
				if grant, err := persistSessionPermissionGrant(call.Name, args, decisionReason, options); err == nil {
					requestEvent.GrantMatched = true
					requestEvent.Grant = &grant
				}
			}
		case PermissionDecisionAllowPrefix:
			if len(request.CommandPrefix) == 0 {
				emitDeniedPermission(options, call, requestEvent, decisionReason)
				return deniedPermissionResult(call, decisionReason, requestEvent), nil
			}
			permissionGranted = true
			requestEvent.DecisionAction = decision.Action
			decisionCommandPrefix = append([]string(nil), request.CommandPrefix...)
			if options.Sandbox != nil && len(decisionCommandPrefix) > 0 {
				options.Sandbox.GrantCommandPrefixForSession(call.Name, decisionCommandPrefix)
			}
			cleanup, err := grantNetworkForSandboxPrompt(requestEvent, sandbox.PermissionGrantScopeTurn, options)
			if err != nil {
				reason := "failed to grant network permission: " + err.Error()
				emitDeniedPermission(options, call, requestEvent, reason)
				return deniedPermissionResult(call, reason, requestEvent), nil
			}
			permissionCleanups = append(permissionCleanups, cleanup)
		case PermissionDecisionAlwaysAllowPrefix:
			if len(request.CommandPrefix) == 0 {
				emitDeniedPermission(options, call, requestEvent, decisionReason)
				return deniedPermissionResult(call, decisionReason, requestEvent), nil
			}
			permissionGranted = true
			requestEvent.DecisionAction = decision.Action
			decisionCommandPrefix = append([]string(nil), request.CommandPrefix...)
			if options.Sandbox != nil && len(decisionCommandPrefix) > 0 {
				if grant, err := persistCommandPrefixGrant(call.Name, decisionCommandPrefix, decisionReason, options); err == nil {
					decisionCommandPrefix = append([]string(nil), grant.Prefix...)
				}
			}
			cleanup, err := grantNetworkForSandboxPrompt(requestEvent, sandbox.PermissionGrantScopeTurn, options)
			if err != nil {
				reason := "failed to grant network permission: " + err.Error()
				emitDeniedPermission(options, call, requestEvent, reason)
				return deniedPermissionResult(call, reason, requestEvent), nil
			}
			permissionCleanups = append(permissionCleanups, cleanup)
		case PermissionDecisionAlwaysAllow:
			permissionGranted = true
			requestEvent.DecisionAction = decision.Action
			// "Always allow" also persists a grant so FUTURE calls skip the prompt.
			// With no sandbox engine there is nowhere to persist it, and persistence
			// can also fail — neither must deny what the user explicitly allowed, so
			// honor the allow for THIS call regardless and just skip remembering the
			// grant (the user is re-prompted next time rather than denied now). This
			// call's permission event is built from the sandbox decision below
			// (buildPermissionEvent reads decision.GrantMatched/Grant), so the
			// persisted grant does not need to be recorded on requestEvent here.
			if options.Sandbox != nil {
				if grant, err := persistPermissionGrant(call.Name, args, decisionReason, options); err == nil {
					requestEvent.GrantMatched = true
					requestEvent.Grant = &grant
				}
			}
		case PermissionDecisionCancel:
			emitCanceledPermission(options, call, requestEvent, decisionReason)
			return canceledPermissionResult(call, decisionReason, requestEvent), fmt.Errorf("%w for %s", ErrPermissionApprovalCanceled, call.Name)
		default:
			emitDeniedPermission(options, call, requestEvent, decisionReason)
			return deniedPermissionResult(call, decisionReason, requestEvent), nil
		}
	}

	// beforeTool hooks may veto the call before it runs (a non-zero exit blocks).
	if toolFound {
		if outcome, blocked := dispatchBeforeTool(ctx, options, call, args); blocked {
			return blockedByHookResult(call, outcome), nil
		}
	}
	args = shellExecutionArgsForApproval(call.Name, args, decisionAction, options)

	// Task tool: wire progress callback so the TUI sees live tool-call events
	// from the specialist child process.
	var progressCallback func(streamjson.Event)
	if call.Name == "Task" && options.OnToolProgress != nil {
		toolCallID := call.ID
		onProgress := options.OnToolProgress
		progressCallback = func(event streamjson.Event) {
			onProgress(toolCallID, event)
		}
	}

	result := registry.RunWithOptions(ctx, call.Name, args, tools.RunOptions{
		PermissionGranted: permissionGranted,
		PermissionMode:    string(permissionMode),
		Autonomy:          options.Autonomy,
		Sandbox:           options.Sandbox,
		ToolCallID:        call.ID,
		SessionID:         options.SessionID,
		Model:             options.Model,
		ReasoningEffort:   options.ReasoningEffort,
		Depth:             options.Depth,
		Cwd:               options.Cwd,
		// Per-session file version tracker so write_file/edit_file refuse to clobber
		// a file that changed on disk outside Zero since it was last read.
		FileTracker:                options.FileTracker,
		DeferFileObservationCommit: true,
		// Post-edit diagnostics are NOT forwarded to the tools: they used to run
		// synchronously inside edit_file/write_file and block every mutating tool
		// call on the language server (≥300ms debounce floor, 10s cap). The loop
		// now checks changed files in the background and nudges the model before
		// its next request instead (see async_diagnostics.go). The tools' inline
		// mechanism (tools.RunOptions.Diagnostics) stays for direct API callers.
		// Forward the run's operator tool filters so a filter-aware tool
		// (tool_search) never discloses or loads an operator-hidden deferred tool.
		EnabledTools:  options.EnabledTools,
		DisabledTools: options.DisabledTools,
		Progress:      progressCallback,
		// The sandbox decision (if any) is returned synchronously on the Result and
		// used here for permission event building.
	})
	if retryResult, directResult, retried, action, reason, prefix, abortErr := maybeRetryUnsandboxedAfterSandboxRestriction(ctx, registry, call, tool, args, result, permissionMode, options, progressCallback); retried || directResult != nil || abortErr != nil {
		if directResult != nil {
			return *directResult, abortErr
		}
		result = retryResult
		permissionGranted = true
		decisionAction = action
		decisionReason = reason
		if len(prefix) > 0 {
			decisionCommandPrefix = append([]string(nil), prefix...)
		}
	}
	sandboxDecision := result.SandboxDecision
	if toolFound && options.OnPermission != nil {
		if event, ok := buildPermissionEvent(call, tool, args, permissionGranted, permissionMode, options, sandboxDecision); ok {
			event.DecisionReason = decisionReason
			if decisionAction != "" {
				event.DecisionAction = decisionAction
			}
			if len(decisionCommandPrefix) > 0 {
				event.CommandPrefix = append([]string(nil), decisionCommandPrefix...)
			}
			options.OnPermission(event)
		}
	}
	// afterTool hooks run once the tool has executed; their output (e.g. a
	// formatter or vet result) is surfaced back to the model on the result.
	if toolFound {
		if feedback := dispatchAfterTool(ctx, options, call, args, result); feedback != "" {
			var didRedact bool
			result.Output, didRedact = appendHookFeedback(result.Output, feedback)
			if didRedact {
				result.Redacted = true
			}
			result = registry.RebudgetAfterHook(call.Name, args, result)
		}
	}
	result = registry.CommitFileObservation(result, options.FileTracker)
	// Stamp the sandbox risk classification for this EXECUTED call so run-policy
	// observers can see the risk of an allowed mutation. Prefer the preflight
	// decision's classification when the sandbox evaluated the call; otherwise
	// run the same pure classifier the permission path uses. Denied/canceled
	// results returned earlier keep the zero value.
	executedRisk := sandbox.Risk{}
	if preflightDecision != nil {
		executedRisk = preflightDecision.Risk
	} else if toolFound {
		// Unknown-tool results fall through here with a nil tool; they carry a
		// denial and keep the zero risk value.
		executedRisk = sandbox.Classify(sandboxRequest(call.Name, tool, args, permissionGranted, permissionMode, options))
	}

	// Secret scrubbing happens at the registry boundary (the single point both
	// the agent loop and the MCP server pass through), so result.Output is
	// already redacted here and result.Redacted reflects whether it changed.
	return ToolResult{
		Risk:            executedRisk,
		ToolCallID:      call.ID,
		Name:            call.Name,
		Status:          result.Status,
		Output:          result.ModelOutput(),
		Truncated:       result.Truncated,
		Meta:            result.Meta,
		Images:          result.Images,
		Redacted:        result.Redacted,
		ChangedFiles:    result.ChangedFiles,
		FileDiffs:       result.FileDiffs,
		ChangeSummaries: result.ChangeSummaries,
		Display:         result.HumanDisplay(),
		Outcome:         result.Outcome,
		LoadedTools:     loadedToolsFromResult(result.Meta),
		// A tool may signal a mid-run model escalation by carrying the target id
		// in Meta["escalate_to_model"]. Lift it into the typed loop-level field;
		// the Run turn loop performs the actual provider switch. Empty for every
		// ordinary tool result.
		RequestedModel: result.Meta["escalate_to_model"],
	}, nil
}

const sandboxNamespaceLimitedReason = "sandbox output is limited to the sandbox PID namespace; host/global state requires approval"

func maybeRetryUnsandboxedAfterSandboxRestriction(ctx context.Context, registry *tools.Registry, call ToolCall, tool tools.Tool, args map[string]any, result tools.Result, permissionMode PermissionMode, options Options, progressCallback func(streamjson.Event)) (tools.Result, *ToolResult, bool, PermissionDecisionAction, string, []string, error) {
	if retryResult, directResult, retried, action, reason, abortErr := maybeRetryWithNetworkAfterSandboxDenial(ctx, registry, call, tool, args, result, permissionMode, options, progressCallback); retried || directResult != nil || abortErr != nil {
		return retryResult, directResult, retried, action, reason, nil, abortErr
	}
	if !sandboxRestrictedShellRetryCandidate(call, args, result, options) {
		return result, nil, false, "", "", nil, nil
	}
	requestEvent := sandboxRestrictionRetryEvent(call, tool, args, permissionMode, options, result)
	request := permissionRequestFromEvent(requestEvent, args, options)
	if permissionMode == PermissionModeUnsafe {
		retryArgs := unsandboxedRetryArgs(args)
		retry := runToolForUnsandboxedRetry(ctx, registry, call.Name, call.ID, retryArgs, permissionMode, options, progressCallback)
		return retry, nil, true, PermissionDecisionAllow, "unsafe permission mode permits unsandboxed retry", nil, nil
	}
	decision, err := requestPermission(ctx, request, options)
	if err != nil {
		decision = PermissionDecision{Action: PermissionDecisionDeny, Reason: err.Error()}
	}
	decision.Action = normalizePermissionDecisionAction(decision.Action)
	reason := strings.TrimSpace(decision.Reason)
	if reason == "" {
		reason = request.Reason
	}
	switch decision.Action {
	case PermissionDecisionAllow, PermissionDecisionAllowStrict, PermissionDecisionAllowForSession, PermissionDecisionAllowPrefix, PermissionDecisionAlwaysAllowPrefix:
		prefix := []string(nil)
		if decision.Action == PermissionDecisionAllowPrefix || decision.Action == PermissionDecisionAlwaysAllowPrefix {
			if len(request.CommandPrefix) == 0 {
				emitDeniedPermission(options, call, requestEvent, reason)
				denied := deniedPermissionResult(call, reason, requestEvent)
				return result, &denied, true, decision.Action, reason, nil, nil
			}
			prefix = append([]string(nil), request.CommandPrefix...)
			if options.Sandbox != nil {
				if decision.Action == PermissionDecisionAlwaysAllowPrefix {
					if grant, err := persistCommandPrefixGrant(call.Name, prefix, reason, options); err == nil {
						prefix = append([]string(nil), grant.Prefix...)
					}
				} else {
					options.Sandbox.GrantCommandPrefixForSession(call.Name, prefix)
				}
			}
		}
		retryArgs := unsandboxedRetryArgs(args)
		retry := runToolForUnsandboxedRetry(ctx, registry, call.Name, call.ID, retryArgs, permissionMode, options, progressCallback)
		return retry, nil, true, decision.Action, reason, prefix, nil
	case PermissionDecisionCancel:
		emitCanceledPermission(options, call, requestEvent, reason)
		canceled := canceledPermissionResult(call, reason, requestEvent)
		return result, &canceled, true, decision.Action, reason, nil, fmt.Errorf("%w for %s", ErrPermissionApprovalCanceled, call.Name)
	default:
		emitDeniedPermission(options, call, requestEvent, reason)
		denied := deniedPermissionResult(call, reason, requestEvent)
		return result, &denied, true, decision.Action, reason, nil, nil
	}
}

func maybeRetryWithNetworkAfterSandboxDenial(ctx context.Context, registry *tools.Registry, call ToolCall, tool tools.Tool, args map[string]any, result tools.Result, permissionMode PermissionMode, options Options, progressCallback func(streamjson.Event)) (tools.Result, *ToolResult, bool, PermissionDecisionAction, string, error) {
	if !sandboxDeniedNetworkRetryCandidate(call, args, result, options) {
		return result, nil, false, "", "", nil
	}
	requestEvent := sandboxDeniedNetworkRetryEvent(call, tool, args, permissionMode, options, result)
	request := permissionRequestFromEvent(requestEvent, args, options)
	if permissionMode == PermissionModeUnsafe {
		retry := runToolForNetworkRetry(ctx, registry, call.Name, call.ID, args, permissionMode, options, progressCallback)
		return retry, nil, true, PermissionDecisionAllow, "unsafe permission mode permits sandbox network retry", nil
	}
	decision, err := requestPermission(ctx, request, options)
	if err != nil {
		decision = PermissionDecision{Action: PermissionDecisionDeny, Reason: err.Error()}
	}
	decision.Action = normalizePermissionDecisionAction(decision.Action)
	reason := strings.TrimSpace(decision.Reason)
	if reason == "" {
		reason = request.Reason
	}
	switch decision.Action {
	case PermissionDecisionAllow, PermissionDecisionAllowStrict, PermissionDecisionAllowForSession:
		scope := sandbox.PermissionGrantScopeTurn
		if decision.Action == PermissionDecisionAllowForSession {
			scope = sandbox.PermissionGrantScopeSession
		}
		cleanup, err := grantNetworkForSandboxPrompt(requestEvent, scope, options)
		if err != nil {
			reason := "failed to grant network permission: " + err.Error()
			emitDeniedPermission(options, call, requestEvent, reason)
			denied := deniedPermissionResult(call, reason, requestEvent)
			return result, &denied, true, decision.Action, reason, nil
		}
		if cleanup != nil {
			defer cleanup()
		}
		retry := runToolForNetworkRetry(ctx, registry, call.Name, call.ID, args, permissionMode, options, progressCallback)
		return retry, nil, true, decision.Action, reason, nil
	case PermissionDecisionCancel:
		emitCanceledPermission(options, call, requestEvent, reason)
		canceled := canceledPermissionResult(call, reason, requestEvent)
		return result, &canceled, true, decision.Action, reason, fmt.Errorf("%w for %s", ErrPermissionApprovalCanceled, call.Name)
	default:
		emitDeniedPermission(options, call, requestEvent, reason)
		denied := deniedPermissionResult(call, reason, requestEvent)
		return result, &denied, true, decision.Action, reason, nil
	}
}

func sandboxDeniedNetworkRetryCandidate(call ToolCall, args map[string]any, result tools.Result, options Options) bool {
	if options.Sandbox == nil || !isShellCommandTool(call.Name) {
		return false
	}
	if result.ExecutionOutcome != nil {
		denial := result.ExecutionOutcome.Denial
		if denial == nil || denial.Capability.Kind != execution.CapabilityExternalNetwork {
			return false
		}
	} else {
		if result.Meta[tools.SandboxLikelyDeniedMeta] != "true" || result.Meta[tools.SandboxDenialKindMeta] != tools.SandboxDenialKindNetwork {
			return false
		}
	}
	if value, _ := args["sandbox_permissions"].(string); strings.TrimSpace(value) == string(tools.SandboxPermissionsRequireEscalated) {
		return false
	}
	return true
}

func sandboxDeniedNetworkRetryEvent(call ToolCall, tool tools.Tool, args map[string]any, permissionMode PermissionMode, options Options, result tools.Result) PermissionEvent {
	risk := sandbox.Classify(sandbox.Request{
		ToolName:          call.Name,
		SideEffect:        sandbox.SideEffect(tool.Safety().SideEffect),
		Permission:        sandbox.Permission(tool.Safety().Permission),
		PermissionGranted: false,
		PermissionMode:    sandbox.PermissionMode(permissionMode),
		Args:              args,
		Reason:            sandbox.ReasonNetworkBlocked,
	})
	risk = ensureRiskCategory(risk, "network", sandbox.RiskCritical)
	reason := sandbox.ReasonNetworkBlocked
	if keyword := strings.TrimSpace(result.Meta[tools.SandboxDenialKeywordMeta]); keyword != "" {
		reason += " (" + keyword + ")"
	}
	return PermissionEvent{
		ToolCallID:     call.ID,
		ToolName:       call.Name,
		Action:         PermissionActionPrompt,
		Permission:     string(tools.PermissionPrompt),
		PermissionMode: permissionMode,
		Autonomy:       options.Autonomy,
		SideEffect:     string(tools.SideEffectShell),
		Reason:         sandbox.ReasonNetworkBlocked,
		DecisionReason: reason,
		Scope:          permissionScope(call.Name, args),
		Risk:           risk,
		CommandPrefix:  proposedCommandPrefix(call.Name, args),
	}
}

func sandboxRestrictedShellRetryCandidate(call ToolCall, args map[string]any, result tools.Result, options Options) bool {
	if options.Sandbox == nil || !isShellCommandTool(call.Name) {
		return false
	}
	if !options.Sandbox.UnsandboxedExecutionAllowed() {
		return false
	}
	if value, _ := args["sandbox_permissions"].(string); strings.TrimSpace(value) == string(tools.SandboxPermissionsRequireEscalated) {
		return false
	}
	return sandboxDeniedShellResult(result) || sandboxNamespaceLimitedShellResult(result)
}

func sandboxDeniedShellResult(result tools.Result) bool {
	if result.ExecutionOutcome != nil {
		// Only a typed platform denial that explicitly requests unrestricted
		// recovery may enter the legacy retry path. Narrow capability denials
		// remain on their exact capability path.
		denial := result.ExecutionOutcome.Denial
		return denial != nil &&
			denial.Source == execution.DenialSourcePlatformSandbox &&
			denial.Capability.Kind == execution.CapabilityUnrestricted &&
			denial.Recoverable &&
			denial.NextAction == execution.DenialNextActionRequestApproval
	}
	return result.Status == tools.StatusError && result.Meta[tools.SandboxLikelyDeniedMeta] == "true"
}

func sandboxNamespaceLimitedShellResult(result tools.Result) bool {
	if result.Status != tools.StatusOK {
		return false
	}
	if result.Meta["sandbox_wrapped"] != "true" {
		return false
	}
	if result.Meta["sandbox_backend"] != string(sandbox.BackendLinuxBwrap) && result.Meta["sandbox_target_backend"] != string(sandbox.BackendLinuxBwrap) {
		return false
	}
	output := strings.ToLower(result.Output)
	return strings.Contains(output, "bwrap ") &&
		strings.Contains(output, "--unshare-pid") &&
		strings.Contains(output, "-- /bin/sh -c")
}

func sandboxRestrictionRetryEvent(call ToolCall, tool tools.Tool, args map[string]any, permissionMode PermissionMode, options Options, result tools.Result) PermissionEvent {
	reason := sandboxRestrictionRetryReason(result)
	if keyword := strings.TrimSpace(result.Meta[tools.SandboxDenialKeywordMeta]); keyword != "" {
		reason += " (" + keyword + ")"
	}
	risk := sandbox.Classify(sandbox.Request{
		ToolName:          call.Name,
		SideEffect:        sandbox.SideEffect(tool.Safety().SideEffect),
		Permission:        sandbox.Permission(tool.Safety().Permission),
		PermissionGranted: false,
		PermissionMode:    sandbox.PermissionMode(permissionMode),
		Args:              args,
		Reason:            reason,
	})
	return PermissionEvent{
		ToolCallID:     call.ID,
		ToolName:       call.Name,
		Action:         PermissionActionPrompt,
		Permission:     string(tools.PermissionPrompt),
		PermissionMode: permissionMode,
		Autonomy:       options.Autonomy,
		SideEffect:     string(tools.SideEffectShell),
		Reason:         reason,
		Scope:          permissionScope(call.Name, args),
		Risk:           risk,
		CommandPrefix:  proposedCommandPrefix(call.Name, args),
	}
}

func sandboxRestrictionRetryReason(result tools.Result) string {
	if sandboxDeniedShellResult(result) {
		if reason := strings.TrimSpace(result.Meta[tools.SandboxDenialReasonMeta]); reason != "" {
			return reason
		}
		return "sandbox blocked command execution"
	}
	if sandboxNamespaceLimitedShellResult(result) {
		return sandboxNamespaceLimitedReason
	}
	return "sandbox blocked command execution"
}

func runToolForNetworkRetry(ctx context.Context, registry *tools.Registry, name string, toolCallID string, args map[string]any, permissionMode PermissionMode, options Options, progressCallback func(streamjson.Event)) tools.Result {
	return registry.RunWithOptions(ctx, name, cloneArgs(args), tools.RunOptions{
		PermissionGranted:          true,
		PermissionMode:             string(permissionMode),
		Autonomy:                   options.Autonomy,
		Sandbox:                    options.Sandbox,
		ToolCallID:                 toolCallID,
		SessionID:                  options.SessionID,
		Model:                      options.Model,
		ReasoningEffort:            options.ReasoningEffort,
		Depth:                      options.Depth,
		Cwd:                        options.Cwd,
		FileTracker:                options.FileTracker,
		DeferFileObservationCommit: true,
		EnabledTools:               options.EnabledTools,
		DisabledTools:              options.DisabledTools,
		Progress:                   progressCallback,
	})
}

func unsandboxedRetryArgs(args map[string]any) map[string]any {
	retryArgs := cloneArgs(args)
	if retryArgs == nil {
		retryArgs = map[string]any{}
	}
	retryArgs["sandbox_permissions"] = string(tools.SandboxPermissionsRequireEscalated)
	return retryArgs
}

func runToolForUnsandboxedRetry(ctx context.Context, registry *tools.Registry, name string, toolCallID string, args map[string]any, permissionMode PermissionMode, options Options, progressCallback func(streamjson.Event)) tools.Result {
	return registry.RunWithOptions(ctx, name, args, tools.RunOptions{
		PermissionGranted:          true,
		PermissionMode:             string(permissionMode),
		Autonomy:                   options.Autonomy,
		Sandbox:                    options.Sandbox,
		ToolCallID:                 toolCallID,
		SessionID:                  options.SessionID,
		Model:                      options.Model,
		ReasoningEffort:            options.ReasoningEffort,
		Depth:                      options.Depth,
		Cwd:                        options.Cwd,
		FileTracker:                options.FileTracker,
		DeferFileObservationCommit: true,
		EnabledTools:               options.EnabledTools,
		DisabledTools:              options.DisabledTools,
		Progress:                   progressCallback,
	})
}

func toolResultFromPrePermissionReject(call ToolCall, result tools.Result) ToolResult {
	// PrePermissionRejecter runs before Registry.RunWithOptions, so it must
	// explicitly cross the same transcript/redaction boundary before its result
	// can be forwarded through ACP.
	result = tools.ScrubResultSecrets(result)
	output, outputRedacted := scrubInterceptedOutput(result.Output)
	display := result.Display
	summary, summaryRedacted := scrubInterceptedOutput(display.Summary)
	display.Summary = summary

	meta := result.Meta
	metaRedacted := false
	if len(result.Meta) > 0 {
		meta = make(map[string]string, len(result.Meta))
		for key, value := range result.Meta {
			scrubbed := redaction.RedactString(value, redaction.Options{})
			if scrubbed != value {
				metaRedacted = true
			}
			meta[key] = scrubbed
		}
	}

	return ToolResult{
		ToolCallID:      call.ID,
		Name:            call.Name,
		Status:          result.Status,
		Output:          output,
		Truncated:       result.Truncated,
		Meta:            meta,
		Redacted:        result.Redacted || outputRedacted || summaryRedacted || metaRedacted,
		ChangedFiles:    result.ChangedFiles,
		FileDiffs:       result.FileDiffs,
		ChangeSummaries: result.ChangeSummaries,
		Display:         display,
		LoadedTools:     loadedToolsFromResult(meta),
		RequestedModel:  meta["escalate_to_model"],
	}
}

// hooksSuppressed reports whether advisory (non-veto) hooks must not run for
// this run's permission mode. Plan mode promises a read-only turn, but
// sessionStart/sessionEnd/afterTool hooks execute configured host commands
// outside the advertised-tool and sandbox gates, so dispatching them would let
// merely starting a plan session or finishing a read mutate the workspace.
//
// beforeTool is intentionally NOT suppressed: a non-zero exit is a deny gate,
// and skipping it fails open (operators who block secret-file reads via
// beforeTool would lose that protection under /plan on). See dispatchBeforeTool.
//
// Spec-draft keeps the existing trust-gated hook model: project hooks still
// fire when the workspace (or its worktree trust root) is trusted. That is
// intentional; trust inheritance for --use-spec --worktree is covered by
// TestExecSpecWorktreeInheritsTrustEndToEnd.
func hooksSuppressed(options Options) bool {
	return options.PermissionMode == PermissionModePlan
}

// dispatchBeforeTool runs configured beforeTool hooks for a tool call. A hook
// that exits non-zero vetoes the call: the returned bool is true and the tool
// must not run. A nil dispatcher (no hooks wired) is a no-op.
//
// Unlike advisory hooks, beforeTool still runs under plan mode. Suppressing it
// would fail open: a project policy that blocks reads of secrets via
// beforeTool would silently stop applying the moment permission mode is plan.
func dispatchBeforeTool(ctx context.Context, options Options, call ToolCall, args map[string]any) (hooks.DispatchOutcome, bool) {
	if options.Hooks == nil {
		return hooks.DispatchOutcome{}, false
	}
	outcome := options.Hooks.Dispatch(ctx, hooks.DispatchInput{
		Event:      hooks.EventBeforeTool,
		ToolName:   call.Name,
		ToolCallID: call.ID,
		Payload: map[string]any{
			"event":      string(hooks.EventBeforeTool),
			"tool":       call.Name,
			"toolCallId": call.ID,
			"sessionId":  options.SessionID,
			"cwd":        options.Cwd,
			"args":       args,
		},
	})
	return outcome, outcome.Blocked
}

// dispatchAfterTool runs configured afterTool hooks once a tool has executed and
// returns any advisory output (e.g. a formatter or vet result) to surface back
// to the model. afterTool hooks never block. A nil dispatcher is a no-op.
func dispatchAfterTool(ctx context.Context, options Options, call ToolCall, args map[string]any, result tools.Result) string {
	if options.Hooks == nil || hooksSuppressed(options) {
		return ""
	}
	outcome := options.Hooks.Dispatch(ctx, hooks.DispatchInput{
		Event:      hooks.EventAfterTool,
		ToolName:   call.Name,
		ToolCallID: call.ID,
		Payload: map[string]any{
			"event":        string(hooks.EventAfterTool),
			"tool":         call.Name,
			"toolCallId":   call.ID,
			"sessionId":    options.SessionID,
			"cwd":          options.Cwd,
			"status":       string(result.Status),
			"changedFiles": result.ChangedFiles,
		},
	})
	return strings.TrimSpace(strings.Join(outcome.Messages, "\n"))
}

// dispatchSessionStart runs configured sessionStart hooks once before the first
// model turn. Lifecycle hooks are advisory: dispatcher failures are audited but
// never block the run.
func dispatchSessionStart(ctx context.Context, options Options) {
	if options.Hooks == nil || hooksSuppressed(options) {
		return
	}
	options.Hooks.Dispatch(ctx, hooks.DispatchInput{
		Event: hooks.EventSessionStart,
		Payload: map[string]any{
			"event":        string(hooks.EventSessionStart),
			"sessionId":    options.SessionID,
			"cwd":          options.Cwd,
			"provider":     options.ProviderName,
			"model":        options.Model,
			"depth":        options.Depth,
			"tag":          options.Tag,
			"sessionTitle": options.SessionTitle,
		},
	})
}

// dispatchSessionEnd runs configured sessionEnd hooks once when the agent run
// exits, including early error returns. Lifecycle hooks are advisory.
func dispatchSessionEnd(ctx context.Context, options Options, result Result, runErr error) {
	if options.Hooks == nil || hooksSuppressed(options) {
		return
	}
	payload := map[string]any{
		"event":        string(hooks.EventSessionEnd),
		"sessionId":    options.SessionID,
		"cwd":          options.Cwd,
		"provider":     options.ProviderName,
		"model":        options.Model,
		"turns":        result.Turns,
		"stopReason":   string(result.StopReason),
		"finishReason": result.FinishReason,
		"incomplete":   result.Incomplete,
	}
	if runErr != nil {
		payload["error"] = runErr.Error()
	}
	options.Hooks.Dispatch(ctx, hooks.DispatchInput{
		Event:   hooks.EventSessionEnd,
		Payload: payload,
	})
}

// blockedByHookResult is the tool result for a call vetoed by a beforeTool hook.
func blockedByHookResult(call ToolCall, outcome hooks.DispatchOutcome) ToolResult {
	// The hook reason (its stdout/stderr) is model-visible here, an intercepted
	// path that bypasses the registry's output redaction boundary — so scrub it
	// like every other string that crosses into the transcript, and flag Redacted
	// when scrubbing changed it (matching the registry's contract).
	scrubbed := redaction.RedactString(outcome.Reason, redaction.Options{})
	redacted := scrubbed != outcome.Reason
	reason := strings.TrimSpace(scrubbed)
	if reason == "" {
		reason = "blocked by a beforeTool hook"
	}
	message := fmt.Sprintf("Error: %q was blocked by hook %q: %s", call.Name, outcome.BlockedBy, reason)
	return ToolResult{
		ToolCallID:   call.ID,
		Name:         call.Name,
		Status:       tools.StatusError,
		Output:       message,
		Redacted:     redacted,
		DenialReason: DenialHookBlocked,
	}
}

// appendHookFeedback appends afterTool hook output to a tool result's output,
// scrubbed for secrets like every other string crossing the tool boundary. The
// returned bool reports whether scrubbing changed the feedback, so the caller can
// set ToolResult.Redacted to match the registry's redaction contract.
func appendHookFeedback(output string, feedback string) (string, bool) {
	scrubbed := redaction.RedactString(feedback, redaction.Options{})
	redacted := scrubbed != feedback
	if strings.TrimSpace(scrubbed) == "" {
		return output, redacted
	}
	if strings.TrimSpace(output) == "" {
		return "Hook output:\n" + scrubbed, redacted
	}
	return output + "\n\nHook output:\n" + scrubbed, redacted
}

// isRetriableToolError reports whether a failed tool result is one the model can
// plausibly fix by changing its next call (argument or execution failure), as
// opposed to a policy refusal (disabled tool, permission denial, sandbox
// block) that no reformatting will satisfy. Only retriable failures should
// drive the repeated-failure schema hint / stop.
func isRetriableToolError(result ToolResult) bool {
	if result.Status != tools.StatusError {
		return false
	}
	// A categorized denial (filtered / permission / sandbox) is a policy decision,
	// not a transient failure — never retry it. This is robust to message wording
	// (the text checks below remain as a fallback for results lacking the field).
	if result.DenialReason != DenialNone {
		return false
	}
	if result.Meta["permission_action"] == string(PermissionActionDeny) {
		return false
	}
	switch {
	case strings.Contains(result.Output, "is not enabled for this run"),
		strings.Contains(result.Output, "Permission denied for "),
		strings.Contains(result.Output, "Permission required for "),
		strings.Contains(result.Output, "Sandbox block"),
		strings.Contains(result.Output, "Sandbox approval required for "):
		return false
	}
	return true
}

// scrubInterceptedOutput mirrors the registry's scrubResultSecrets boundary for
// the loop-intercepted paths (ask_user answers, task child final answers) that
// build a ToolResult.Output directly instead of going through
// registry.RunWithOptions. RedactString substitutes "[REDACTED]" inline; the
// returned bool reports whether anything was scrubbed so the caller can set
// ToolResult.Redacted, keeping these paths consistent with every other tool
// result the model and transcript see.
func scrubInterceptedOutput(output string) (string, bool) {
	scrubbed := redaction.RedactString(output, redaction.Options{})
	return scrubbed, scrubbed != output
}

// executeAskUser routes an ask_user call to the interactive front-end via
// options.OnAskUser, mirroring the async permission flow. If no handler is set,
// or the handler errors, it falls back to the tool's own graceful result so the
// loop never blocks forever waiting on a user who isn't there.
// executeAskUser returns (result, abortErr). abortErr is non-nil ONLY when the
// prompt was canceled/timed out and the run must stop with that error; every
// other path (success, bad args, headless degradation) returns a nil abortErr.
func executeAskUser(ctx context.Context, registry *tools.Registry, call ToolCall, args map[string]any, permissionMode PermissionMode, options Options) (ToolResult, error) {
	questions, err := tools.ParseAskUserQuestions(args)
	if err != nil {
		return ToolResult{
			ToolCallID: call.ID,
			Name:       call.Name,
			Status:     tools.StatusError,
			Output:     "Error: Invalid arguments for ask_user: " + err.Error(),
		}, nil
	}

	if options.OnAskUser == nil {
		return askUserFallbackResult(ctx, registry, call, args, permissionMode, options), nil
	}

	header, _ := args["header"].(string)
	request := AskUserRequest{
		ToolCallID: call.ID,
		Header:     strings.TrimSpace(header),
		Questions:  toAgentAskUserQuestions(questions),
	}
	response, err := options.OnAskUser(ctx, request)
	if err != nil {
		// A canceled / timed-out prompt must ABORT the run (return the error), not
		// fabricate a headless answer that keeps mutating the transcript after the UI
		// asked to stop. The error result carries the reason; the abort error stops
		// the loop even when the run context itself was not canceled.
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return ToolResult{
				ToolCallID: call.ID,
				Name:       call.Name,
				Status:     tools.StatusError,
				Output:     "Error: ask_user canceled: " + err.Error(),
			}, err
		}
		// Genuine handler unavailability (no interactive surface / non-cancel error):
		// degrade to the same headless path as a missing handler.
		return askUserFallbackResult(ctx, registry, call, args, permissionMode, options), nil
	}

	// Scrub the formatted answers through the same redaction boundary the
	// registry applies to tool output, so a secret in a user's answer never lands
	// in the transcript unredacted.
	output, redacted := scrubInterceptedOutput(tools.FormatAskUserAnswers(questions, response.Answers))
	return ToolResult{
		ToolCallID: call.ID,
		Name:       call.Name,
		Status:     tools.StatusOK,
		Output:     output,
		Redacted:   redacted,
	}, nil
}

// askUserFallbackResult runs the registered ask_user tool (or returns the shared
// graceful message) so the no-interactive-user path matches every OTHER tool: it
// goes through registry.RunWithOptions with the same run context (ToolCallID,
// session/model/depth/cwd, sandbox/permission) and copies the full result fields
// (Meta/ChangedFiles/Display), instead of the bare registry.Run that dropped them.
func askUserFallbackResult(ctx context.Context, registry *tools.Registry, call ToolCall, args map[string]any, permissionMode PermissionMode, options Options) ToolResult {
	if _, ok := registry.Get(call.Name); ok {
		result := registry.RunWithOptions(ctx, call.Name, args, tools.RunOptions{
			// ask_user is read-only (PermissionAllow); no prompt/grant is required.
			PermissionGranted: true,
			PermissionMode:    string(permissionMode),
			Autonomy:          options.Autonomy,
			Sandbox:           options.Sandbox,
			ToolCallID:        call.ID,
			SessionID:         options.SessionID,
			Model:             options.Model,
			ReasoningEffort:   options.ReasoningEffort,
			Depth:             options.Depth,
			Cwd:               options.Cwd,
		})
		return ToolResult{
			ToolCallID:      call.ID,
			Name:            call.Name,
			Status:          result.Status,
			Output:          result.ModelOutput(),
			Truncated:       result.Truncated,
			Meta:            result.Meta,
			Redacted:        result.Redacted,
			ChangedFiles:    result.ChangedFiles,
			FileDiffs:       result.FileDiffs,
			ChangeSummaries: result.ChangeSummaries,
			Display:         result.HumanDisplay(),
			Outcome:         result.Outcome,
		}
	}
	return ToolResult{
		ToolCallID: call.ID,
		Name:       call.Name,
		Status:     tools.StatusOK,
		Output:     tools.AskUserNonInteractiveMessage(),
	}
}

func toAgentAskUserQuestions(questions []tools.AskUserQuestion) []AskUserQuestion {
	converted := make([]AskUserQuestion, len(questions))
	for index, question := range questions {
		converted[index] = AskUserQuestion{
			Question:           question.Question,
			Header:             question.Header,
			Options:            append([]string{}, question.Options...),
			OptionDescriptions: append([]string{}, question.OptionDescriptions...),
			Recommended:        question.Recommended,
			MultiSelect:        question.MultiSelect,
		}
	}
	return converted
}

func sandboxRequest(toolName string, tool tools.Tool, args map[string]any, permissionGranted bool, permissionMode PermissionMode, options Options) sandbox.Request {
	safety := tool.Safety()
	return sandbox.Request{
		WorkspaceRoot:     "",
		ToolName:          toolName,
		SideEffect:        sandbox.SideEffect(safety.SideEffect),
		Permission:        sandbox.Permission(safety.Permission),
		PermissionGranted: permissionGranted,
		PermissionMode:    sandbox.PermissionMode(permissionMode),
		Args:              args,
		Reason:            safety.Reason,
	}
}

// effectivePermission returns the permission for THIS specific call. A tool that
// implements tools.ArgsPermissioner may relax its static permission for arguments
// it can prove are safe (the Task tool auto-allows a read-only sub-agent); every
// other tool falls back to its static Safety().Permission unchanged.
func effectivePermission(tool tools.Tool, args map[string]any) tools.Permission {
	if p, ok := tool.(tools.ArgsPermissioner); ok {
		return p.PermissionForArgs(args)
	}
	return tool.Safety().Permission
}

func shouldRequestPermission(tool tools.Tool, args map[string]any, permissionGranted bool, decision *sandbox.Decision) bool {
	if decision != nil && decision.Action == sandbox.ActionPrompt {
		return true
	}
	if tool.Safety().Permission != tools.PermissionPrompt {
		return false
	}
	if !permissionGranted && shellCommandAdditionalPermissionsRequested(args) {
		return true
	}
	if decision != nil {
		if sandboxDecisionRequiresExplicitPermission(decision) {
			return true
		}
		if permissionGranted {
			return false
		}
		return decision.Action == sandbox.ActionPrompt
	}
	if permissionGranted {
		return false
	}
	return true
}

func sandboxDecisionRequiresExplicitPermission(decision *sandbox.Decision) bool {
	return decision != nil && decision.Action == sandbox.ActionPrompt && decision.Reason == sandbox.ReasonNetworkBlocked
}

func requestPermission(ctx context.Context, request PermissionRequest, options Options) (PermissionDecision, error) {
	if options.OnPermissionRequest == nil {
		return PermissionDecision{Action: PermissionDecisionDeny, Reason: request.Reason}, nil
	}
	permSpan := options.Trace.Span(trace.SpanPermissionWait)
	decision, err := options.OnPermissionRequest(ctx, request)
	permSpan.End()
	return decision, err
}

func normalizePermissionDecisionAction(action PermissionDecisionAction) PermissionDecisionAction {
	switch action {
	case PermissionDecisionAllow, PermissionDecisionAllowStrict, PermissionDecisionAllowForSession, PermissionDecisionAllowPrefix, PermissionDecisionAlwaysAllowPrefix, PermissionDecisionAlwaysAllow, PermissionDecisionCancel:
		return action
	default:
		return PermissionDecisionDeny
	}
}

type requestPermissionsArgs struct {
	EnvironmentID string                           `json:"environment_id"`
	Reason        string                           `json:"reason"`
	Permissions   sandbox.RequestPermissionProfile `json:"permissions"`
}

func executeRequestPermissions(ctx context.Context, call ToolCall, args map[string]any, permissionMode PermissionMode, options Options) (ToolResult, error) {
	// request_permissions is dispatched by name above, before the registry-based
	// ToolAdvertised gate runs, so a read-only mode's registry omitting this tool
	// (rather than registering it as denied) must not fall through to a real
	// grant. Deny it here unconditionally for spec-draft/plan, independent of
	// whether the caller's registry happens to contain the tool.
	if permissionMode == PermissionModeSpecDraft || permissionMode == PermissionModePlan {
		return ToolResult{
			ToolCallID:   call.ID,
			Name:         call.Name,
			Status:       tools.StatusError,
			Output:       `Error: Tool "` + call.Name + `" is not available in ` + string(permissionMode) + ` mode.`,
			DenialReason: DenialFiltered,
		}, nil
	}
	parsed, err := parseRequestPermissionsArgs(args)
	if err != nil {
		return ToolResult{
			ToolCallID: call.ID,
			Name:       call.Name,
			Status:     tools.StatusError,
			Output:     "Error: Invalid arguments for request_permissions: " + err.Error(),
		}, nil
	}
	normalized, err := sandbox.NormalizeRequestPermissionProfile(parsed.Permissions, options.Cwd)
	if err != nil {
		return ToolResult{
			ToolCallID: call.ID,
			Name:       call.Name,
			Status:     tools.StatusError,
			Output:     "Error: Invalid permissions for request_permissions: " + err.Error(),
		}, nil
	}
	if normalized.Empty() {
		return ToolResult{
			ToolCallID: call.ID,
			Name:       call.Name,
			Status:     tools.StatusError,
			Output:     "Error: request_permissions requires at least one permission.",
		}, nil
	}
	grantProfile, err := sandbox.RequestPermissionGrantProfile(normalized)
	if err != nil {
		return ToolResult{
			ToolCallID: call.ID,
			Name:       call.Name,
			Status:     tools.StatusError,
			Output:     "Error: Invalid permissions for request_permissions: " + err.Error(),
		}, nil
	}

	request := requestPermissionsPrompt(call, parsed, grantProfile, permissionMode, options)
	if options.OnPermissionRequest == nil {
		response := sandbox.RequestPermissionsResponse{
			Permissions: sandbox.RequestPermissionProfile{},
			Scope:       sandbox.PermissionGrantScopeTurn,
		}
		return requestPermissionsResult(call, response, false), nil
	}

	decision, err := requestPermission(ctx, request, options)
	if err != nil {
		decision = PermissionDecision{Action: PermissionDecisionDeny, Reason: err.Error()}
	}
	decision.Action = normalizePermissionDecisionAction(decision.Action)
	decisionReason := strings.TrimSpace(decision.Reason)
	if decisionReason == "" {
		decisionReason = request.Reason
	}

	response := sandbox.RequestPermissionsResponse{
		Permissions: sandbox.RequestPermissionProfile{},
		Scope:       sandbox.PermissionGrantScopeTurn,
	}
	permissionGranted := false
	switch decision.Action {
	case PermissionDecisionAllow:
		response.Permissions = grantProfile
		response.Scope = sandbox.PermissionGrantScopeTurn
		permissionGranted = true
	case PermissionDecisionAllowStrict:
		response.Permissions = grantProfile
		response.Scope = sandbox.PermissionGrantScopeTurn
		response.StrictAutoReview = true
		permissionGranted = true
	case PermissionDecisionAllowForSession:
		response.Permissions = grantProfile
		response.Scope = sandbox.PermissionGrantScopeSession
		permissionGranted = true
	case PermissionDecisionCancel:
		decision.Action = PermissionDecisionDeny
		decisionReason = firstNonEmptyString(decisionReason, "continued without granting permissions")
	default:
		decision.Action = PermissionDecisionDeny
		decisionReason = firstNonEmptyString(decisionReason, "continued without granting permissions")
	}

	if permissionGranted {
		if options.Sandbox == nil {
			return ToolResult{
				ToolCallID: call.ID,
				Name:       call.Name,
				Status:     tools.StatusError,
				Output:     "Error: request_permissions cannot grant permissions because the sandbox engine is not configured.",
			}, nil
		}
		cleanup, err := options.Sandbox.GrantRequestPermissions(response.Permissions, response.Scope)
		if err != nil {
			return ToolResult{
				ToolCallID: call.ID,
				Name:       call.Name,
				Status:     tools.StatusError,
				Output:     "Error: Failed to grant requested permissions: " + err.Error(),
			}, nil
		}
		if response.Scope == sandbox.PermissionGrantScopeTurn && options.runPermissions != nil {
			options.runPermissions.add(cleanup)
		}
	}

	emitRequestPermissionsDecision(options, call, request, decision.Action, decisionReason, permissionGranted)
	return requestPermissionsResult(call, response, false), nil
}

func parseRequestPermissionsArgs(args map[string]any) (requestPermissionsArgs, error) {
	data, err := json.Marshal(args)
	if err != nil {
		return requestPermissionsArgs{}, err
	}
	var parsed requestPermissionsArgs
	if err := json.Unmarshal(data, &parsed); err != nil {
		return requestPermissionsArgs{}, err
	}
	if parsed.EnvironmentID == "" {
		if raw, ok := args["environmentId"].(string); ok {
			parsed.EnvironmentID = raw
		}
	}
	return parsed, nil
}

func requestPermissionsPrompt(call ToolCall, parsed requestPermissionsArgs, profile sandbox.RequestPermissionProfile, permissionMode PermissionMode, options Options) PermissionRequest {
	autonomy := options.Autonomy
	reason := strings.TrimSpace(parsed.Reason)
	if reason == "" {
		reason = "The agent is requesting additional permissions."
	}
	return PermissionRequest{
		ToolCallID:     call.ID,
		ToolName:       tools.RequestPermissionsToolName,
		Action:         PermissionActionPrompt,
		Permission:     string(tools.PermissionPrompt),
		PermissionMode: permissionMode,
		Autonomy:       autonomy,
		SideEffect:     "permissions",
		Reason:         reason,
		Scope:          requestPermissionsScope(profile),
		Args: map[string]any{
			"environment_id": parsed.EnvironmentID,
			"reason":         parsed.Reason,
			"permissions":    profile,
		},
		AvailableDecisions: []PermissionDecisionAction{
			PermissionDecisionAllow,
			PermissionDecisionAllowStrict,
			PermissionDecisionAllowForSession,
			PermissionDecisionDeny,
		},
	}
}

func requestPermissionsScope(profile sandbox.RequestPermissionProfile) string {
	parts := []string{}
	if profile.Network != nil && profile.Network.Enabled != nil && *profile.Network.Enabled {
		parts = append(parts, "network all outbound access")
	}
	if profile.FileSystem != nil {
		for _, path := range profile.FileSystem.Read {
			parts = append(parts, "read "+path)
		}
		for _, path := range profile.FileSystem.Write {
			parts = append(parts, "write "+path)
		}
		for _, path := range profile.FileSystem.DenyRead {
			parts = append(parts, "deny read "+path)
		}
	}
	return strings.Join(parts, ", ")
}

func emitRequestPermissionsDecision(options Options, call ToolCall, request PermissionRequest, action PermissionDecisionAction, reason string, granted bool) {
	if options.OnPermission == nil {
		return
	}
	eventAction := PermissionActionDeny
	if granted {
		eventAction = PermissionActionAllow
	}
	options.OnPermission(PermissionEvent{
		ToolCallID:        call.ID,
		ToolName:          call.Name,
		Action:            eventAction,
		DecisionAction:    action,
		Permission:        request.Permission,
		PermissionGranted: granted,
		PermissionMode:    request.PermissionMode,
		Autonomy:          request.Autonomy,
		SideEffect:        request.SideEffect,
		Reason:            reason,
		Scope:             request.Scope,
		DecisionReason:    reason,
	})
}

func requestPermissionsResult(call ToolCall, response sandbox.RequestPermissionsResponse, redacted bool) ToolResult {
	data, err := json.Marshal(response)
	if err != nil {
		return ToolResult{
			ToolCallID: call.ID,
			Name:       call.Name,
			Status:     tools.StatusError,
			Output:     "Error: Failed to serialize request_permissions response: " + err.Error(),
		}
	}
	output, didRedact := scrubInterceptedOutput(string(data))
	return ToolResult{
		ToolCallID: call.ID,
		Name:       call.Name,
		Status:     tools.StatusOK,
		Output:     output,
		Redacted:   redacted || didRedact,
	}
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func persistPermissionGrant(toolName string, args map[string]any, reason string, options Options) (sandbox.Grant, error) {
	if options.Sandbox == nil {
		return sandbox.Grant{}, errors.New("sandbox engine is not configured")
	}
	// Scope the grant to exactly what the permission card showed (the file or
	// directory the call touches); engine.Grant anchors it to the workspace. A
	// call with no path-like argument yields an empty scope — a tool-wide grant.
	scope, kind := sandbox.DeriveScope(toolName, args)
	return options.Sandbox.Grant(sandbox.GrantInput{
		ToolName:  toolName,
		Decision:  sandbox.GrantAllow,
		Reason:    reason,
		Scope:     scope,
		ScopeKind: kind,
	})
}

func persistSessionPermissionGrant(toolName string, args map[string]any, reason string, options Options) (sandbox.Grant, error) {
	if options.Sandbox == nil {
		return sandbox.Grant{}, errors.New("sandbox engine is not configured")
	}
	scope, kind := sandbox.DeriveScope(toolName, args)
	return options.Sandbox.GrantForSession(sandbox.GrantInput{
		ToolName:  toolName,
		Decision:  sandbox.GrantAllow,
		Reason:    reason,
		Scope:     scope,
		ScopeKind: kind,
	})
}

func persistCommandPrefixGrant(toolName string, prefix []string, reason string, options Options) (sandbox.CommandPrefixGrant, error) {
	if options.Sandbox == nil {
		return sandbox.CommandPrefixGrant{}, errors.New("sandbox engine is not configured")
	}
	return options.Sandbox.GrantCommandPrefix(sandbox.CommandPrefixInput{
		ToolName: toolName,
		Prefix:   prefix,
		Reason:   reason,
	})
}

func emitDeniedPermission(options Options, call ToolCall, requestEvent PermissionEvent, reason string) {
	if options.OnPermission == nil {
		return
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = requestEvent.Reason
	}
	event := requestEvent
	event.ToolCallID = call.ID
	event.ToolName = call.Name
	event.Action = PermissionActionDeny
	event.DecisionAction = PermissionDecisionDeny
	event.PermissionGranted = false
	event.DecisionReason = reason
	options.OnPermission(event)
}

func emitCanceledPermission(options Options, call ToolCall, requestEvent PermissionEvent, reason string) {
	if options.OnPermission == nil {
		return
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "cancelled in TUI"
	}
	event := requestEvent
	event.ToolCallID = call.ID
	event.ToolName = call.Name
	event.Action = PermissionActionCancel
	event.DecisionAction = PermissionDecisionCancel
	event.PermissionGranted = false
	event.DecisionReason = reason
	event.Reason = reason
	options.OnPermission(event)
}

func deniedPermissionResult(call ToolCall, reason string, requestEvent PermissionEvent) ToolResult {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = requestEvent.Reason
	}
	if reason == "" {
		reason = "tool requires approval before execution"
	}
	event := requestEvent
	event.Action = PermissionActionDeny
	event.DecisionAction = PermissionDecisionDeny
	event.PermissionGranted = false
	event.DecisionReason = reason
	if event.Risk.Level == "" {
		event.Risk = sandbox.Risk{Level: sandbox.RiskMedium, Reason: reason}
	}
	if requestEvent.ToolName == "" {
		event.ToolName = call.Name
	}
	// A denial driven by a sandbox block is categorized distinctly from a
	// plain approval-declined so a surface can tell policy from user choice.
	denial := DenialPermissionDenied
	if requestEvent.Block != nil {
		denial = DenialSandboxBlock
	}
	return ToolResult{
		ToolCallID:   call.ID,
		Name:         call.Name,
		Status:       tools.StatusError,
		Output:       "Error: Permission denied for " + call.Name + ": " + reason,
		DenialReason: denial,
		Meta: map[string]string{
			"permission_action": string(event.Action),
		},
	}
}

func canceledPermissionResult(call ToolCall, reason string, requestEvent PermissionEvent) ToolResult {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "cancelled in TUI"
	}
	event := requestEvent
	event.Action = PermissionActionCancel
	event.DecisionAction = PermissionDecisionCancel
	event.PermissionGranted = false
	event.DecisionReason = reason
	event.Reason = reason
	if requestEvent.ToolName == "" {
		event.ToolName = call.Name
	}
	return ToolResult{
		ToolCallID:   call.ID,
		Name:         call.Name,
		Status:       tools.StatusError,
		Output:       "Error: Permission approval cancelled for " + call.Name + ": " + reason,
		DenialReason: DenialApprovalCanceled,
		Meta: map[string]string{
			"permission_action": string(event.Action),
		},
	}
}

// permissionScope returns a concise, human-readable description of what a tool
// call will actually touch -- a file path, directory, working dir, or network
// host lifted from its arguments -- so the permission card and persisted
// decision can show the user exactly what "allow" covers. It is empty when the
// tool exposes no scoped argument (the grant is then plainly tool-wide). The
// first matching key wins, ordered most-specific (a concrete file) first.
func permissionScope(toolName string, args map[string]any) string {
	// sandbox.DeriveScope is the single source of truth for which arguments carry
	// a scope, shared with grant persistence and matching so the card display can
	// never diverge from what an "always allow" actually covers.
	raw, _ := sandbox.DeriveScope(toolName, args)
	if raw == "" {
		return ""
	}
	return truncateScope(raw)
}

func truncateScope(value string) string {
	const maxScopeRunes = 80
	runes := []rune(value)
	if len(runes) <= maxScopeRunes {
		return value
	}
	return string(runes[:maxScopeRunes-1]) + "…"
}

func buildPermissionEvent(call ToolCall, tool tools.Tool, args map[string]any, permissionGranted bool, permissionMode PermissionMode, options Options, decision *sandbox.Decision) (PermissionEvent, bool) {
	safety := tool.Safety()
	var action PermissionAction
	reason := safety.Reason
	risk := sandbox.Classify(sandbox.Request{
		WorkspaceRoot:     "",
		ToolName:          call.Name,
		SideEffect:        sandbox.SideEffect(safety.SideEffect),
		Permission:        sandbox.Permission(safety.Permission),
		PermissionGranted: permissionGranted,
		PermissionMode:    sandbox.PermissionMode(permissionMode),
		Args:              args,
		Reason:            safety.Reason,
	})
	var block *sandbox.Block
	grantMatched := false
	var grant *sandbox.Grant

	if decision != nil {
		action = permissionActionFromSandbox(decision.Action)
		if decision.Reason != "" {
			reason = decision.Reason
		}
		risk = decision.Risk
		block = decision.Block
		grantMatched = decision.GrantMatched
		grant = decision.Grant
	} else {
		switch safety.Permission {
		case tools.PermissionDeny:
			action = PermissionActionDeny
		case tools.PermissionPrompt:
			if permissionGranted {
				action = PermissionActionAllow
			} else {
				action = PermissionActionPrompt
			}
		default:
			return PermissionEvent{}, false
		}
	}

	// A command that explicitly requests additional sandbox permissions
	// (sandbox_permissions: with_additional_permissions) is an ELEVATION the user
	// must consent to, even when the base command's sandbox decision was Allow.
	// shouldRequestPermission blocks on OnPermissionRequest for exactly this case,
	// so the event MUST be a prompt — otherwise it carried Action=allow while the
	// loop waited on a decision, and a UI that renders only prompts (the TUI)
	// dropped it, deadlocking the run. Keep this consistent with
	// shouldRequestPermission's own condition.
	if action == PermissionActionAllow && !permissionGranted && shellCommandAdditionalPermissionsRequested(args) {
		action = PermissionActionPrompt
	}

	if safety.Permission == tools.PermissionAllow && action == PermissionActionAllow && !grantMatched && block == nil {
		return PermissionEvent{}, false
	}

	scopeText := permissionScope(call.Name, args)
	if profile, ok, err := inlineAdditionalPermissionsProfile(args, options.Cwd); ok && err == nil {
		scopeText = requestPermissionsScope(profile)
	}
	decisionReason := strings.TrimSpace(reason)
	reason = userFacingPermissionReason(call.Name, args, reason, safety.Reason)

	return PermissionEvent{
		ToolCallID:        call.ID,
		ToolName:          call.Name,
		Action:            action,
		DecisionAction:    grantDecisionAction(grant),
		Permission:        string(safety.Permission),
		PermissionGranted: permissionGranted,
		PermissionMode:    permissionMode,
		Autonomy:          options.Autonomy,
		SideEffect:        string(safety.SideEffect),
		Reason:            reason,
		DecisionReason:    decisionReason,
		Scope:             scopeText,
		Risk:              risk,
		Block:             block,
		GrantMatched:      grantMatched,
		Grant:             grant,
		CommandPrefix:     proposedCommandPrefix(call.Name, args),
	}, true
}

func userFacingPermissionReason(toolName string, args map[string]any, reason string, fallback string) string {
	reason = strings.TrimSpace(reason)
	if !isShellCommandTool(toolName) {
		return reason
	}

	// Policy and sandbox explanations are authoritative. The tool's static
	// safety copy is only an internal fallback; presenting it as the reason for
	// every ordinary shell approval makes prompts generic and obscures the cases
	// where policy has an actionable explanation.
	fallback = strings.TrimSpace(fallback)
	if reason != "" && reason != fallback && reason != "tool requires approval before execution" && reason != sandbox.ReasonEscalatedSandboxRequired {
		return reason
	}

	if shellCommandRequiresEscalated(args) {
		if justification, ok := firstStringArg(args, "justification"); ok {
			return justification
		}
		return "This command needs to run outside the sandbox."
	}

	// Ordinary shell approvals need no invented explanation. The raw fallback is
	// retained separately as DecisionReason for traces and policy auditing.
	return ""
}

func grantDecisionAction(grant *sandbox.Grant) PermissionDecisionAction {
	if grant == nil {
		return ""
	}
	if grant.Session {
		return PermissionDecisionAllowForSession
	}
	if grant.Decision == sandbox.GrantAllow {
		return PermissionDecisionAlwaysAllow
	}
	return ""
}

func fallbackPermissionEvent(call ToolCall, tool tools.Tool, args map[string]any, permissionMode PermissionMode, options Options) PermissionEvent {
	event, _ := buildPermissionEvent(call, tool, args, false, permissionMode, options, nil)
	return event
}

func permissionRequestFromEvent(event PermissionEvent, args map[string]any, options Options) PermissionRequest {
	return PermissionRequest{
		ToolCallID:         event.ToolCallID,
		ToolName:           event.ToolName,
		Action:             event.Action,
		Permission:         event.Permission,
		PermissionMode:     event.PermissionMode,
		Autonomy:           event.Autonomy,
		SideEffect:         event.SideEffect,
		Reason:             event.Reason,
		Scope:              event.Scope,
		Risk:               event.Risk,
		Args:               cloneArgs(args),
		Block:              event.Block,
		GrantMatched:       event.GrantMatched,
		Grant:              event.Grant,
		CommandPrefix:      append([]string(nil), event.CommandPrefix...),
		AvailableDecisions: availablePermissionDecisions(event, args, options),
		// Computed from the SAME conditions shellExecutionArgsForApproval uses,
		// so the prompt cannot claim an escalation that will not happen or stay
		// silent about one that will. Keeping the two in step matters more than
		// the wording: a label that overstates gets ignored, and one that
		// understates is the bug being fixed.
		PrefixApprovalEscalates: shellPrefixApprovalEscalates(event.ToolName, args, options),
	}
}

// shellPrefixApprovalEscalates reports whether approving a command prefix would
// also take this command out of the sandbox.
//
// Deliberately mirrors shellExecutionArgsForApproval's guards rather than
// re-deriving them: same tool test, same sandbox availability test, and the same
// two opt-outs for a call that already asked for additional permissions or
// escalation. Those already carry their own disclosure, so labelling them again
// would be noise.
func shellPrefixApprovalEscalates(toolName string, args map[string]any, options Options) bool {
	if !isShellCommandTool(toolName) {
		return false
	}
	if options.Sandbox == nil || !options.Sandbox.UnsandboxedExecutionAllowed() {
		return false
	}
	if shellCommandAdditionalPermissionsRequested(args) || shellCommandRequiresEscalated(args) {
		return false
	}
	return true
}

func availablePermissionDecisions(event PermissionEvent, args map[string]any, options Options) []PermissionDecisionAction {
	if event.Action != PermissionActionPrompt {
		return nil
	}
	decisions := []PermissionDecisionAction{PermissionDecisionAllow}
	inlineAdditionalPermissions := shellCommandAdditionalPermissionsRequested(args)
	if options.Sandbox != nil {
		decisions = append(decisions, PermissionDecisionAllowForSession)
		if isShellCommandTool(event.ToolName) && len(event.CommandPrefix) > 0 && !networkSandboxPrompt(event) && !inlineAdditionalPermissions {
			decisions = append(decisions, PermissionDecisionAllowPrefix)
			if options.Sandbox.CanPersistGrants() {
				decisions = append(decisions, PermissionDecisionAlwaysAllowPrefix)
			}
		}
		if options.Sandbox.CanPersistGrants() && permissionSupportsPersistentDecision(event.ToolName) && !filesystemSandboxPrompt(event) && !inlineAdditionalPermissions {
			decisions = append(decisions, PermissionDecisionAlwaysAllow)
		}
	}
	switch {
	case isShellCommandTool(event.ToolName):
		decisions = append(decisions, PermissionDecisionDeny, PermissionDecisionCancel)
	case event.ToolName == "apply_patch":
		decisions = append(decisions, PermissionDecisionCancel)
	default:
		decisions = append(decisions, PermissionDecisionDeny)
	}
	return decisions
}

func networkSandboxPrompt(event PermissionEvent) bool {
	return isShellCommandTool(event.ToolName) &&
		event.Reason == sandbox.ReasonNetworkBlocked &&
		sandbox.HasRiskCategory(event.Risk, "network")
}

func ensureRiskCategory(risk sandbox.Risk, category string, level sandbox.RiskLevel) sandbox.Risk {
	if !sandbox.HasRiskCategory(risk, category) {
		risk.Categories = append(risk.Categories, category)
		sort.Strings(risk.Categories)
	}
	risk.Level = level
	if len(risk.Categories) > 0 {
		risk.Reason = fmt.Sprintf("%s risk: %s", risk.Level, strings.Join(risk.Categories, ", "))
	}
	return risk
}

func shellCommandAdditionalPermissionsRequested(args map[string]any) bool {
	raw, ok := args["sandbox_permissions"]
	if !ok || raw == nil {
		return false
	}
	value, ok := raw.(string)
	if !ok {
		value = fmt.Sprint(raw)
	}
	return strings.TrimSpace(value) == string(tools.SandboxPermissionsWithAdditionalPermissions)
}

// normalizeNoopAdditionalPermissions treats a provider-expanded empty or
// already-covered additional_permissions object as if it had been omitted.
// Some strict-schema providers materialize every optional property and may add
// a workspace read that the default sandbox already grants; rejecting either
// no-op makes the model retry an otherwise valid command. Requests that broaden
// access and malformed payloads are left untouched so validation stays closed.
func normalizeNoopAdditionalPermissions(args map[string]any, basePath string, engine *sandbox.Engine) {
	raw, exists := args["additional_permissions"]
	if !exists {
		return
	}
	// An explicit null is the field omitted, not a request: drop the key so the
	// rest of the pipeline sees the same args a model that omitted it would send.
	// sandbox_permissions is deliberately left alone — a flagged null must reach
	// the same "no additional_permissions object was provided" error as a flagged
	// absent field rather than being silently downgraded to a different outcome.
	if raw == nil {
		delete(args, "additional_permissions")
		return
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return
	}
	var profile sandbox.RequestPermissionProfile
	if err := json.Unmarshal(data, &profile); err != nil {
		return
	}
	normalized, err := sandbox.NormalizeRequestPermissionProfile(profile, basePath)
	if err != nil {
		return
	}
	noop := normalized.Empty()
	if !noop && engine != nil {
		if grantProfile, grantErr := sandbox.RequestPermissionGrantProfile(normalized); grantErr == nil {
			noop = engine.CoversRequestPermissions(grantProfile)
		}
	}
	if !noop {
		return
	}
	delete(args, "additional_permissions")
	if rawMode, ok := args["sandbox_permissions"].(string); ok &&
		strings.TrimSpace(rawMode) == string(tools.SandboxPermissionsWithAdditionalPermissions) {
		args["sandbox_permissions"] = string(tools.SandboxPermissionsUseDefault)
	}
}

// omitAdditionalPermissionsHint is the one instruction that ends the retry loop
// in #845/#847. Every additional_permissions rejection must carry it: the model
// reaches these errors by attaching a permission object to a command that needs
// none, so "omit it and retry" is the answer in practice, and an error that
// only explains how to build a VALID permission object sends the model round
// the loop again with a different invalid shape.
const omitAdditionalPermissionsHint = "If this command does not need extra sandbox access — ordinary " +
	"read-only commands such as `git branch --show-current` do not — omit both sandbox_permissions and " +
	"additional_permissions entirely and retry; that is the fix in almost every case."

func inlineAdditionalPermissionsProfile(args map[string]any, basePath string) (sandbox.RequestPermissionProfile, bool, error) {
	if !shellCommandAdditionalPermissionsRequested(args) {
		// An explicit null grants nothing and carries no flag, so it is
		// indistinguishable from an omitted field; rejecting it would be a pure
		// false positive against providers that materialize every optional
		// property.
		if raw, exists := args["additional_permissions"]; exists && raw != nil {
			return sandbox.RequestPermissionProfile{}, false, fmt.Errorf(
				"additional_permissions was provided without sandbox_permissions set to %q, so it cannot be applied. %s "+
					"Only if the command genuinely needs file or network access the sandbox denies, resend with "+
					`sandbox_permissions: %q AND a non-empty additional_permissions, for example {"network": {"enabled": true}}`,
				tools.SandboxPermissionsWithAdditionalPermissions,
				omitAdditionalPermissionsHint,
				tools.SandboxPermissionsWithAdditionalPermissions)
		}
		return sandbox.RequestPermissionProfile{}, false, nil
	}
	raw, ok := args["additional_permissions"]
	if !ok || raw == nil {
		return sandbox.RequestPermissionProfile{}, true, fmt.Errorf(
			"sandbox_permissions was set to %q but no additional_permissions object was provided. %s "+
				`Otherwise include one, for example additional_permissions: {"network": {"enabled": true}} or `+
				`{"file_system": {"write": ["/path"]}}`,
			tools.SandboxPermissionsWithAdditionalPermissions,
			omitAdditionalPermissionsHint)
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return sandbox.RequestPermissionProfile{}, true, err
	}
	var profile sandbox.RequestPermissionProfile
	if err := json.Unmarshal(data, &profile); err != nil {
		return sandbox.RequestPermissionProfile{}, true, err
	}
	normalized, err := sandbox.NormalizeRequestPermissionProfile(profile, basePath)
	if err != nil {
		return sandbox.RequestPermissionProfile{}, true, err
	}
	if normalized.Empty() {
		return sandbox.RequestPermissionProfile{}, true, fmt.Errorf(
			"additional_permissions granted nothing, so it cannot be applied. %s "+
				`Otherwise include a real permission, for example {"network": {"enabled": true}} or `+
				`{"file_system": {"write": ["/path"]}}`,
			omitAdditionalPermissionsHint)
	}
	grantProfile, err := sandbox.RequestPermissionGrantProfile(normalized)
	if err != nil {
		return sandbox.RequestPermissionProfile{}, true, err
	}
	return grantProfile, true, nil
}

func grantInlineAdditionalPermissions(args map[string]any, scope sandbox.PermissionGrantScope, options Options) (func(), error) {
	profile, ok, err := inlineAdditionalPermissionsProfile(args, options.Cwd)
	if err != nil || !ok {
		return nil, err
	}
	if options.Sandbox == nil {
		return nil, errors.New("sandbox engine is not configured")
	}
	return options.Sandbox.GrantRequestPermissions(profile, scope)
}

func grantNetworkForSandboxPrompt(event PermissionEvent, scope sandbox.PermissionGrantScope, options Options) (func(), error) {
	if !networkSandboxPrompt(event) || options.Sandbox == nil {
		return nil, nil
	}
	enabled := true
	return options.Sandbox.GrantRequestPermissions(sandbox.RequestPermissionProfile{
		Network: &sandbox.NetworkPermissions{Enabled: &enabled},
	}, scope)
}

func filesystemSandboxPrompt(event PermissionEvent) bool {
	return event.Block != nil &&
		event.Block.Code == sandbox.BlockOutsideWorkspace &&
		event.Block.Recoverable &&
		strings.TrimSpace(event.Block.Path) != ""
}

func grantFilesystemForSandboxPrompt(event PermissionEvent, scope sandbox.PermissionGrantScope, options Options) (func(), error) {
	if !filesystemSandboxPrompt(event) || options.Sandbox == nil {
		return nil, nil
	}
	path := event.Block.Path
	fs := sandbox.FileSystemPermissions{}
	switch sandbox.SideEffect(event.SideEffect) {
	case sandbox.SideEffectRead:
		fs.Read = []string{path}
	case sandbox.SideEffectWrite, sandbox.SideEffectOutOfWorkspace:
		fs.Write = []string{path}
	default:
		return nil, nil
	}
	return options.Sandbox.GrantRequestPermissions(sandbox.RequestPermissionProfile{
		FileSystem: &fs,
	}, scope)
}

func permissionSupportsPersistentDecision(toolName string) bool {
	switch toolName {
	case "bash", "exec_command", "write_stdin", "apply_patch":
		return false
	case "browser_install", "browser_launch", "browser_connect", "browser_open", "browser_snapshot", "browser_click", "browser_type", "browser_press", "browser_action", "desktop_windows", "desktop_snapshot", "desktop_action", "terminal_session", "capture_artifact":
		return false
	default:
		return true
	}
}

func cloneArgs(args map[string]any) map[string]any {
	if len(args) == 0 {
		return nil
	}
	copied := make(map[string]any, len(args))
	for key, value := range args {
		copied[key] = value
	}
	return copied
}

func permissionActionFromSandbox(action sandbox.Action) PermissionAction {
	switch action {
	case sandbox.ActionAllow:
		return PermissionActionAllow
	case sandbox.ActionDeny:
		return PermissionActionDeny
	default:
		return PermissionActionPrompt
	}
}

// partitionToolsCached is partitionTools with an optional per-tool definition
// cache. The partitioning itself (visibility, deferral, ordering) is recomputed
// every call — it must be, because a tool's deferred state can flip mid-run (e.g.
// swarm tools un-defer once a swarm is active). Only the expensive part —
// rendering each tool's JSON-schema parameters — is memoized by tool name, since a
// tool's advertised name/description/schema is stable for the run. defCache nil
// disables caching (used by tests and the plain partitionTools entrypoint).
func partitionToolsCached(registry *tools.Registry, permissionMode PermissionMode, options Options, loaded map[string]bool, defCache map[string]zeroruntime.ToolDefinition) ([]zeroruntime.ToolDefinition, string) {
	registeredTools := registry.All()

	visible := make([]tools.Tool, 0, len(registeredTools))
	eligible := 0
	for _, tool := range registeredTools {
		if !ToolVisible(tool, permissionMode, options.EnabledTools, options.DisabledTools) {
			continue
		}
		visible = append(visible, tool)
		// Count by deferral-ELIGIBILITY, not current deferred state: a tool that
		// un-defers at runtime (e.g. swarm coordination tools once a swarm is
		// active) still counts, so it can't drop `eligible` below the threshold
		// and force-expose every other deferred tool. Whether a tool is actually
		// hidden is decided by IsDeferred in the exposure loop below.
		if tools.IsDeferralEligible(tool) {
			eligible++
		}
	}

	// Deferral may activate only when tool_search is actually runnable; otherwise
	// the loop would hide deferred tools behind a loader the dispatch gate rejects
	// — an inescapable dead-end. "Runnable" mirrors executeToolCall's gate:
	// registered, model-visible, not in DisabledTools, and advertised in the
	// current permission mode (e.g. not spec-draft, where tool_search is not
	// advertised). The
	// EnabledTools allowlist is intentionally NOT checked here — tool_search is
	// exempt from the allowlist at dispatch, so an allowlist that omits it must
	// not disable deferral.
	loader, loaderFound := registry.Get(tools.ToolSearchToolName)
	loaderUsable := loaderFound &&
		tools.ModelVisible(loader) &&
		!containsToolName(options.DisabledTools, tools.ToolSearchToolName) &&
		ToolAdvertised(loader, permissionMode)

	active := options.DeferThreshold > 0 && eligible >= options.DeferThreshold && loaderUsable

	// INACTIVE: every visible tool eager (except tool_search), alpha-sorted. With no
	// deferral there is no mid-session loading, so this is byte-stable across turns
	// and byte-identical to the pre-deferral output.
	if !active {
		definitions := make([]zeroruntime.ToolDefinition, 0, len(visible))
		for _, tool := range visible {
			if tool.Name() == tools.ToolSearchToolName {
				continue
			}
			definitions = append(definitions, cachedRuntimeToolDefinition(defCache, tool))
		}
		sort.Slice(definitions, func(left int, right int) bool {
			return definitions[left].Name < definitions[right].Name
		})
		return definitions, ""
	}

	// ACTIVE: lay the tools array out so its cacheable prefix does NOT shift when a
	// deferred tool loads mid-session. The tools block is part of the provider's
	// cached prefix; if a load reorders it, the cache invalidates from that point
	// through the system prompt and messages — on EVERY load. Previously the whole
	// array was alpha-sorted each turn, so a newly loaded tool was INSERTED into the
	// middle and shifted every later definition. Instead we build three append-only
	// regions:
	//   1. non-deferred tools, alpha-sorted — always present, identical every turn;
	//   2. tool_search — always present, right after the stable block;
	//   3. loaded deferred tools, alpha-sorted — APPENDED, so an unloaded->loaded
	//      transition grows the tail instead of inserting into the eager block.
	// tool_search's compact catalog also stays byte-stable as tools load, so only
	// the appended full-schema tail changes.
	eager := make([]zeroruntime.ToolDefinition, 0, len(visible))
	loadedTail := make([]zeroruntime.ToolDefinition, 0)
	var deferredCatalog []tools.Tool
	for _, tool := range visible {
		name := tool.Name()
		if name == tools.ToolSearchToolName {
			continue // added explicitly between the eager block and the loaded tail
		}
		if tools.IsDeferred(tool) {
			deferredCatalog = append(deferredCatalog, tool)
			if loaded[name] {
				loadedTail = append(loadedTail, cachedRuntimeToolDefinition(defCache, tool))
			}
			continue
		}
		eager = append(eager, cachedRuntimeToolDefinition(defCache, tool))
	}
	sort.Slice(eager, func(left int, right int) bool {
		return eager[left].Name < eager[right].Name
	})
	sort.Slice(loadedTail, func(left int, right int) bool {
		return loadedTail[left].Name < loadedTail[right].Name
	})

	// Keep the catalog byte-stable as tools load. tool_search already redirects
	// requests for an exposed tool back to the current tool list, so retaining
	// loaded names costs only compact catalog lines and avoids invalidating the
	// provider cache at the loader definition on every load.
	discovery := tools.BuildToolSearchDescription(deferredCatalog)

	// tool_search is guaranteed runnable on the active path, so it is ALWAYS exposed
	// — even when a non-empty EnabledTools allowlist omits it (the operator
	// allowlisted the deferred tools, not the loader). It sits right after the stable
	// eager block and carries the stable deferred-tool catalog.
	description := loader.Description()
	if discovery != "" {
		description = discovery
	}
	definitions := make([]zeroruntime.ToolDefinition, 0, len(eager)+1+len(loadedTail))
	definitions = append(definitions, eager...)
	definitions = append(definitions, zeroruntime.ToolDefinition{
		Name:        loader.Name(),
		Description: description,
		Parameters:  schemaToRuntimeMap(loader.Parameters()),
	})
	definitions = append(definitions, loadedTail...)

	return definitions, discovery
}

// cachedRuntimeToolDefinition returns the tool's rendered definition, reusing a
// cached render when defCache holds one for this tool name. A tool's advertised
// definition is stable across a run, so caching skips the recursive schema→map
// conversion (schemaToRuntimeMap) that would otherwise run for every tool on every
// turn. tool_search is excluded by its callers (its description is dynamic), so it
// never poisons the cache. A nil cache computes fresh.
func cachedRuntimeToolDefinition(defCache map[string]zeroruntime.ToolDefinition, tool tools.Tool) zeroruntime.ToolDefinition {
	if defCache == nil {
		return runtimeToolDefinition(tool)
	}
	if def, ok := defCache[tool.Name()]; ok {
		return def
	}
	def := runtimeToolDefinition(tool)
	defCache[tool.Name()] = def
	return def
}

// runtimeToolDefinition renders a tool's advertised definition (name, description,
// JSON-schema parameters) as sent to the provider.
func runtimeToolDefinition(tool tools.Tool) zeroruntime.ToolDefinition {
	definition := zeroruntime.ToolDefinition{
		Name:        tool.Name(),
		Description: tool.Description(),
		Parameters:  schemaToRuntimeMap(tool.Parameters()),
	}
	if tools.IsBuiltInApplyPatch(tool) {
		definition.Type = zeroruntime.ToolDefinitionFreeform
	}
	return definition
}

func ToolVisible(tool tools.Tool, permissionMode PermissionMode, enabledTools []string, disabledTools []string) bool {
	return tools.ModelVisible(tool) &&
		ToolAllowedByFilters(tool.Name(), enabledTools, disabledTools) &&
		ToolAdvertised(tool, permissionMode)
}

func ToolAllowedByFilters(name string, enabledTools []string, disabledTools []string) bool {
	if len(enabledTools) > 0 {
		if !containsToolName(enabledTools, name) {
			return false
		}
	}
	if containsToolName(disabledTools, name) {
		return false
	}
	return true
}

func containsToolName(names []string, name string) bool {
	for _, candidate := range names {
		if candidate == name {
			return true
		}
	}
	return false
}

func schemaToRuntimeMap(schema tools.Schema) map[string]any {
	parameters := map[string]any{
		"type":                 schema.Type,
		"additionalProperties": schema.AdditionalProperties,
	}

	if len(schema.Required) > 0 {
		parameters["required"] = append([]string{}, schema.Required...)
	}

	if len(schema.Properties) > 0 {
		properties := make(map[string]any, len(schema.Properties))
		for name, property := range schema.Properties {
			properties[name] = propertyToRuntimeMap(property)
		}
		parameters["properties"] = properties
	}

	return parameters
}

func propertyToRuntimeMap(property tools.PropertySchema) map[string]any {
	schema := map[string]any{
		"type": property.Type,
	}
	if property.Description != "" {
		schema["description"] = property.Description
	}
	if len(property.Enum) > 0 {
		schema["enum"] = append([]string{}, property.Enum...)
	}
	if property.Default != nil {
		schema["default"] = property.Default
	}
	if property.Items != nil {
		schema["items"] = propertyToRuntimeMap(*property.Items)
	}
	if property.Minimum != nil {
		schema["minimum"] = *property.Minimum
	}
	if property.Maximum != nil {
		schema["maximum"] = *property.Maximum
	}
	if len(property.Properties) > 0 {
		properties := make(map[string]any, len(property.Properties))
		for name, nested := range property.Properties {
			properties[name] = propertyToRuntimeMap(nested)
		}
		schema["properties"] = properties
	}
	if len(property.Required) > 0 {
		schema["required"] = append([]string{}, property.Required...)
	}
	return schema
}

func ToolAdvertised(tool tools.Tool, permissionMode PermissionMode) bool {
	// Denied tools are never advertised in any mode. Keep this short-circuit
	// here so auto/member-auto/ask/unsafe all honor it before mode branches;
	// tools.ToolAdvertisedForPermissionMode repeats it for the tool_search path
	// which never enters this function.
	if tool.Safety().Permission == tools.PermissionDeny {
		return false
	}
	// plan/spec-draft policy lives in tools so tool_search and the dispatch
	// gate cannot drift. PermissionDeny was already checked above.
	if permissionMode == PermissionModeSpecDraft || permissionMode == PermissionModePlan {
		return tools.ToolAdvertisedForPermissionMode(tool, string(permissionMode))
	}
	if permissionMode == PermissionModeAuto {
		return tool.Safety().Permission == tools.PermissionAllow || tool.Safety().AdvertiseInAuto
	}
	if permissionMode == PermissionModeMemberAuto {
		// Like Auto, plus the in-workspace mutators a headless member needs to
		// build. The sandbox engine still decides at call time: in-workspace writes
		// and sandbox-backed shell auto-allow, while out-of-workspace writes,
		// network, and destructive commands prompt → denied headless. So this
		// advertises capability without widening sandbox authority.
		if tool.Safety().Permission == tools.PermissionAllow || tool.Safety().AdvertiseInAuto {
			return true
		}
		switch tool.Safety().SideEffect {
		case tools.SideEffectWrite, tools.SideEffectShell:
			return true
		}
		return false
	}
	return true
}

func stopReasonFromToolResult(result ToolResult) StopReason {
	if result.Meta == nil {
		return ""
	}
	if result.Meta[toolResultMetaControl] == toolResultControlSpecReview {
		return StopReasonSpecReviewRequired
	}
	return ""
}

// loadedToolsFromResult extracts the deferred-tool names a tool (tool_search)
// asked the loop to expose next turn from Meta["load_tools"] (comma-separated).
// It trims each name and drops empties; returns nil when the key is absent or
// yields no names, so an ordinary result keeps a nil LoadedTools. Mirrors the
// Meta-driven control signal read by stopReasonFromToolResult.
func loadedToolsFromResult(meta map[string]string) []string {
	raw := meta["load_tools"]
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var names []string
	for _, part := range strings.Split(raw, ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			names = append(names, trimmed)
		}
	}
	return names
}

// appendAbortedToolResults adds a placeholder tool-result message for each of
// the given (unexecuted) tool calls, so every advertised tool_use keeps a
// matching tool_result when the loop halts a turn before all calls have run.
func appendAbortedToolResults(messages []Message, remaining []ToolCall) []Message {
	for _, call := range remaining {
		messages = append(messages, zeroruntime.Message{
			Role:               zeroruntime.MessageRoleTool,
			Content:            abortedToolResultNotice,
			ToolCallID:         call.ID,
			ToolCallProviderID: call.ProviderCallID,
			IsError:            true,
		})
	}
	return messages
}

// dedupeStrings returns values with duplicates removed, preserving first-seen
// order so the deduped list (e.g. a turn's changed files) stays deterministic.
func dedupeStrings(values []string) []string {
	seen := make(map[string]bool, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

func copyMessages(messages []Message) []Message {
	copied := make([]Message, len(messages))
	for index, message := range messages {
		copied[index] = message
		if message.ToolCalls != nil {
			copied[index].ToolCalls = append([]ToolCall{}, message.ToolCalls...)
		}
		if message.Reasoning != nil {
			copied[index].Reasoning = append([]zeroruntime.ReasoningBlock{}, message.Reasoning...)
		}
		if message.ChangedFiles != nil {
			copied[index].ChangedFiles = append([]string(nil), message.ChangedFiles...)
		}
		// Deep-copy image attachments (slice AND each Data byte slice) so the
		// raw image bytes are never aliased across history/request/result copies.
		copied[index].Images = zeroruntime.CloneImageBlocks(message.Images)
	}
	return copied
}

// toolResultImageMessage builds the user message that carries a tool's images
// to the model, or reports false when the tool produced none.
//
// The text names the tool so the model can tell which call an image came from
// when several ran in one turn — the images arrive detached from their tool
// result, so nothing else associates them. If the effective model cannot
// accept images, the tool's text result is left unchanged and this follow-up
// is a notice with no image parts, matching the CLI/TUI vision gate.
func toolResultImageMessage(result ToolResult, options Options) (zeroruntime.Message, bool) {
	images := make([]zeroruntime.ImageBlock, 0, len(result.Images))
	for _, image := range result.Images {
		if len(image.Data) == 0 {
			continue
		}
		mediaType := zeroruntime.NormalizeImageMediaType(image.MediaType)
		if mediaType == "" {
			// Outside the provider allow-list. Dropping it beats sending bytes a
			// provider will reject and failing the whole turn.
			continue
		}
		images = append(images, zeroruntime.ImageBlock{MediaType: mediaType, Data: image.Data})
	}
	if len(images) == 0 {
		return zeroruntime.Message{}, false
	}
	label := result.Name
	if label == "" {
		label = "tool"
	}
	if !modelAcceptsToolImages(options) {
		return zeroruntime.Message{
			Role:    zeroruntime.MessageRoleUser,
			Content: "Image output from " + label + " was not sent because the current model does not support image input. Switch to a vision-capable model before retrying image delivery, including any images skipped by the tool's image budget.",
		}, true
	}
	return zeroruntime.Message{
		Role:    zeroruntime.MessageRoleUser,
		Content: "Image output from " + label + ":",
		Images:  images,
	}, true
}

func modelAcceptsToolImages(options Options) bool {
	if options.SupportsVision != nil {
		return options.SupportsVision(options.Model)
	}
	registry, err := modelregistry.DefaultRegistry()
	if err != nil {
		return modelregistry.VisionCapableByName(options.Model)
	}
	return modelregistry.SupportsVision(registry, options.Model)
}
