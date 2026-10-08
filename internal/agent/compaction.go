package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/Gitlawb/zero/internal/trace"
	"github.com/Gitlawb/zero/internal/zeroruntime"
)

// Session compaction.
//
// When a running conversation approaches the model's context window, the oldest
// middle of the history is summarized into a single user-role note while the
// system prompt(s) and the most recent turns are kept verbatim. This keeps long
// sessions from blowing the context budget.
//
// Compact is a PURE function: it never talks to a provider. The actual LLM call
// is injected via CompactionOptions.Summarize so it stays trivially testable and
// the agent loop owns the provider wiring.

// defaultCompactionPreserveLast is how many trailing messages are kept verbatim
// when the caller does not specify a preserve count. Kept in sync with the replay
// reconstruction default in internal/sessions/replay.go.
const defaultCompactionPreserveLast = 6

// compactionTriggerRatio is the fraction of the context window at which
// proactive compaction kicks in (top of each turn). NOTE: this is speculative
// tuning — validate against the per-turn latency/compaction metrics (roadmap 5.2)
// before trusting it. Measured fixed overhead is ~7.4k tokens (not the ~16k the
// roadmap assumed), so context fills slower; if data shows 0.7 triggers redundant
// summarization, raise it back toward 0.8.
const compactionTriggerRatio = 0.7

// summaryLabel prefixes the injected summary so it is unmistakable in the
// transcript (and so tests can assert on it).
const summaryLabel = "[Summary of earlier conversation]"

// CompactionSummaryInstructions is the system prompt handed to both automatic
// and manually requested compaction summarizers.
const CompactionSummaryInstructions = "You are compacting a coding-assistant conversation to save context. " +
	"Write a dense, factual summary of the conversation so far. Preserve: the user's goals and explicit constraints; " +
	"decisions made and why; files created or modified (with paths) and key code changes; commands run and their important " +
	"results; and anything still in progress or unresolved. Omit pleasantries. Use terse bullet points. Do not invent details. " +
	"If the conversation already begins with an earlier summary block, treat its facts as established context and carry them " +
	"forward into the new summary — never drop earlier information."

// CompactionOptions configure a single Compact call.
type CompactionOptions struct {
	// PreserveLast is the number of trailing messages to keep verbatim. The
	// preserved suffix is widened (never shrunk) so it begins at a safe
	// user/assistant boundary. <= 0 falls back to defaultCompactionPreserveLast.
	PreserveLast int
	// Summarize turns the to-be-elided middle into a single dense summary. It is
	// injected so Compact stays pure and testable; the agent loop wires it to a
	// real provider call.
	Summarize func(toSummarize []zeroruntime.Message) (string, error)
	// taskState is a snapshot supplied by the running agent. Its immutable
	// objective is always preserved; mutable fields are admitted only when its
	// plan projection still matches the transcript.
	taskState *taskStateSnapshot
}

// CompactionResult is the metadata-bearing result returned by CompactMessages.
type CompactionResult struct {
	// Messages is the original conversation or the compacted replacement.
	Messages []zeroruntime.Message
	// RemovedCount is the number of original messages summarized away.
	RemovedCount int
	// PreservedCount is the number of original messages kept verbatim, including
	// leading system messages and the preserved suffix.
	PreservedCount int
	// SummaryText is the trimmed text returned by the summarizer. The injected
	// summary message also includes summaryLabel and any preserved structured
	// state needed by later compactions.
	SummaryText string
	// ProjectedChars is the text content sent to the summarizer after semantic
	// projection, excluding provider protocol framing.
	ProjectedChars int
	// Truncated reports whether projection dropped content to satisfy its
	// bounded brief or previous-summary limits.
	Truncated bool
	// Compacted reports whether Messages contains an injected summary.
	Compacted bool
}

// imageTokenEstimate is a flat per-image token cost. Vision models tokenize an
// image by its resolution (tiles), not its byte length, so the raw Data size is
// a poor proxy — counting len(Data)/4 would overcount by orders of magnitude.
// A fixed estimate keeps estimateTokens monotonic in the image count so an
// image-heavy context still trends toward compaction instead of reading as ~0.
// ~1k tokens is a representative mid-range cost across providers.
const imageTokenEstimate = 1000

// ApproxTextTokens estimates the token count of text WITHOUT a real tokenizer.
// The BPE tokenizers zero targets fold a run's leading space into the following
// token rather than emitting whitespace as its own token, so naive len/4
// overcounts real text by ~15-20% (measured: a 24.8k-char prompt counted 5.2k
// real tokens where len/4 said 6.2k). Counting NON-whitespace bytes / 4 tracks
// the provider's actual count closely (validated against live usage) while
// staying allocation- and dependency-free. zero still receives the exact count
// back as usage on every request; this estimate is only for the pre-request
// context budget preview and the compaction threshold.
func ApproxTextTokens(value string) int {
	nonSpace := 0
	for i := 0; i < len(value); i++ {
		switch value[i] {
		case ' ', '\t', '\n', '\r', '\f', '\v':
		default:
			nonSpace++
		}
	}
	return nonSpace / 4
}

// estimateTokens is a cheap, dependency-free token estimate (see ApproxTextTokens)
// across message content plus tool call names/arguments and a flat per-image
// cost. It deliberately uses no real tokenizer; it only needs to be monotonic
// and roughly proportional so the loop can decide when to compact.
func estimateTokens(messages []zeroruntime.Message) int {
	total := 0
	for _, message := range messages {
		total += ApproxTextTokens(message.Content)
		for _, call := range message.ToolCalls {
			total += ApproxTextTokens(call.Name)
			total += ApproxTextTokens(call.Arguments)
			total += 4 // small per-call overhead
		}
		total += len(message.Images) * imageTokenEstimate
		total += 4 // per-message overhead
	}
	return total
}

// estimateToolDefTokens approximates the input-token cost of the tool definitions
// sent with every request (~4 chars/token, matching estimateTokens). Compaction
// must include them: they ride on every turn, so ignoring them under-counts the
// real context and can let it blow past the model limit while the message-only
// estimate still looks under threshold.
func estimateToolDefTokens(tools []zeroruntime.ToolDefinition) int {
	total := 0
	for _, tool := range tools {
		total += ApproxTextTokens(tool.Name)
		total += ApproxTextTokens(tool.Description)
		total += ApproxTextTokens(string(tool.Type))
		if len(tool.Parameters) > 0 {
			if encoded, err := json.Marshal(tool.Parameters); err == nil {
				total += ApproxTextTokens(string(encoded))
			}
		}
		if tool.Format != nil {
			total += ApproxTextTokens(tool.Format.Type)
			total += ApproxTextTokens(tool.Format.Syntax)
			total += ApproxTextTokens(tool.Format.Definition)
		}
		total += 4 // per-tool overhead
	}
	return total
}

// compactionThreshold is the estimated-token level at which proactive
// compaction triggers for a given context window.
func compactionThreshold(contextWindow int) int {
	if contextWindow <= 0 {
		return 0
	}
	return int(float64(contextWindow) * compactionTriggerRatio)
}

// Compact summarizes the oldest middle of a conversation, keeping the leading
// system message(s) and the most recent turns verbatim. The result is:
//
//	[system..., summaryAsUser, preservedSuffix...]
//
// Rules:
//   - Leading system messages stay at the head untouched.
//   - The preserved suffix is widened backward so it never begins with a
//     tool/tool_result message (provider APIs reject a dangling tool result
//     with no preceding assistant tool call).
//   - The summary is injected as a single user-role message labeled with
//     summaryLabel.
//   - If there is nothing to summarize (too few messages once system and the
//     preserved suffix are removed), the input is returned unchanged.
//
// Compact is pure: it performs no provider I/O. A Summarize error is returned to
// the caller, which decides how to recover.
func Compact(messages []zeroruntime.Message, opts CompactionOptions) ([]zeroruntime.Message, error) {
	result, err := CompactMessages(messages, opts)
	if err != nil {
		return nil, err
	}
	return result.Messages, nil
}

// CompactMessages summarizes the oldest middle of a conversation and returns
// both the replacement messages and UI/session-friendly metadata about what
// changed. It uses the same compaction rules as Compact.
func CompactMessages(messages []zeroruntime.Message, opts CompactionOptions) (CompactionResult, error) {
	preserveLast := opts.PreserveLast
	if preserveLast <= 0 {
		preserveLast = defaultCompactionPreserveLast
	}
	if opts.Summarize == nil {
		return CompactionResult{}, errors.New("compaction requires a Summarize function")
	}

	// Leading system messages are kept verbatim at the head.
	systemEnd := 0
	for systemEnd < len(messages) && messages[systemEnd].Role == zeroruntime.MessageRoleSystem {
		systemEnd++
	}

	// Naive boundary: keep the last preserveLast messages. Then widen the suffix
	// backward to a safe boundary so it never starts on a tool result.
	boundary := len(messages) - preserveLast
	if boundary < systemEnd {
		boundary = systemEnd
	}
	boundary = safeSuffixBoundary(messages, systemEnd, boundary)

	middle := messages[systemEnd:boundary]
	if len(middle) == 0 {
		// Nothing to summarize once system + preserved suffix are removed.
		return CompactionResult{
			Messages:       messages,
			PreservedCount: len(messages),
		}, nil
	}

	summaryResult, err := SummarizeCompactionMessages(middle, opts.Summarize)
	if err != nil {
		return CompactionResult{}, err
	}
	summary := summaryResult.SummaryText

	// Preserve structured state (active plan + loaded skills) from the elided
	// middle verbatim, so it is not lost or paraphrased away by the prose summary.
	content := appendPreservedState(summaryLabel+"\n"+summary, middle, opts.taskState)

	compacted := make([]zeroruntime.Message, 0, systemEnd+1+(len(messages)-boundary))
	compacted = append(compacted, messages[:systemEnd]...)
	compacted = append(compacted, zeroruntime.Message{
		Role:    zeroruntime.MessageRoleUser,
		Content: content,
	})
	compacted = append(compacted, messages[boundary:]...)
	return CompactionResult{
		Messages:       compacted,
		RemovedCount:   len(middle),
		PreservedCount: len(messages) - len(middle),
		SummaryText:    summary,
		ProjectedChars: summaryResult.ProjectedChars,
		Truncated:      summaryResult.Truncated,
		Compacted:      true,
	}, nil
}

// CompactionSummaryResult describes the shared summarization input and output.
type CompactionSummaryResult struct {
	SummaryText    string
	ProjectedChars int
	Truncated      bool
}

// SummarizeCompactionMessages applies the shared semantic projection before
// invoking a caller-supplied summarizer. Automatic context-pressure compaction
// and manual session compaction both use this path.
func SummarizeCompactionMessages(messages []zeroruntime.Message, summarize func([]zeroruntime.Message) (string, error)) (CompactionSummaryResult, error) {
	if summarize == nil {
		return CompactionSummaryResult{}, errors.New("compaction requires a Summarize function")
	}
	projection := projectCompactionInput(messages)
	summaryInput := projection.messages
	if len(summaryInput) == 0 {
		summaryInput = messages
	}
	summary, err := summarize(summaryInput)
	if err != nil {
		return CompactionSummaryResult{}, err
	}
	return CompactionSummaryResult{
		SummaryText:    strings.TrimSpace(summary),
		ProjectedChars: compactionMessageChars(summaryInput),
		Truncated:      projection.truncated,
	}, nil
}

func compactionMessageChars(messages []zeroruntime.Message) int {
	total := 0
	for _, message := range messages {
		total += len(message.Content)
		for _, call := range message.ToolCalls {
			total += len(call.Name) + len(call.Arguments)
		}
	}
	return total
}

// safeSuffixBoundary walks the preserve boundary backward (toward systemEnd) so
// the preserved suffix begins on a user or assistant message rather than a
// tool/tool_result message. A tool result with no preceding assistant tool call
// is rejected by provider APIs, so the boundary must land on a safe turn start.
// It never moves the boundary forward (the suffix only grows), and never crosses
// systemEnd.
func safeSuffixBoundary(messages []zeroruntime.Message, systemEnd int, boundary int) int {
	// Walk back so the preserved suffix begins with an assistant message. The
	// summary is injected as a user-role message, so a user- or tool-led suffix
	// would create consecutive same-role turns that strict providers (Anthropic)
	// reject. Stopping on an assistant keeps user/assistant alternation valid;
	// if no assistant exists above systemEnd, boundary lands at systemEnd and the
	// middle is empty, so Compact no-ops (no summary is injected).
	for boundary > systemEnd && messages[boundary].Role != zeroruntime.MessageRoleAssistant {
		boundary--
	}
	return boundary
}

// isContextLimitError reports whether a provider error string looks like a
// context-window / prompt-too-long error from a common provider. Matching is
// substring-based and case-insensitive so it tolerates phrasing differences
// across OpenAI, Anthropic, and Google providers.
func isContextLimitError(message string) bool {
	lowered := strings.ToLower(strings.TrimSpace(message))
	if lowered == "" {
		return false
	}
	needles := []string{
		"context length",
		"context window",
		"context_length_exceeded",
		"maximum context",
		"context limit",
		"prompt is too long",
		"too many tokens",
		"reduce the length of the messages",
		"exceeds the maximum",
		"input is too long",
	}
	for _, needle := range needles {
		if strings.Contains(lowered, needle) {
			return true
		}
	}
	return false
}

// isStreamTimeoutError reports whether a streamed error is a provider stream
// idle/stall timeout (providerio surfaces these as "idle timeout after …" /
// "no output for …" / "stream stalled"). Such a turn produced nothing, so when
// no content was forwarded yet it can be safely re-issued on a fresh connection
// (see the stall-retry in the agent loop). Substring + case-insensitive to
// tolerate the prefixed "provider stream error: …" wrapping.
func isStreamTimeoutError(message string) bool {
	lowered := strings.ToLower(strings.TrimSpace(message))
	if lowered == "" {
		return false
	}
	for _, needle := range []string{"idle timeout after", "no output for", "stream stalled"} {
		if strings.Contains(lowered, needle) {
			return true
		}
	}
	return false
}

// compactionState carries the per-run state the agent loop needs to compact a
// conversation safely. It is created once per Run and is a no-op whenever
// options.ContextWindow <= 0.
type compactionState struct {
	enabled      bool
	threshold    int
	preserveLast int
	// lowWaterMark is the estimated token size at (or below) which we will NOT
	// proactively compact again. It is the size right after the last compaction;
	// the loop only compacts when the history has grown past it AND is over the
	// threshold. This prevents compacting on every turn once near the limit.
	lowWaterMark int
	// reactiveAttempted guards the reactive path so it fires at most once per
	// run. Without this a provider that keeps returning context-limit errors
	// (even after compaction) could loop indefinitely; one attempt then the
	// original error surfaces.
	reactiveAttempted bool
	// onUsage forwards the summarizer call's token usage for accounting/budgeting.
	// OnText is deliberately NOT forwarded (compaction stays invisible to the user),
	// but its token COST must still be counted so usage reports and budgets include it.
	onUsage func(Usage)
	task    *taskState
	planner *contextPlanner
	trace   *trace.Recorder

	// calibrationRatio scales the raw byte/4 token estimate toward the provider's
	// real prompt-token count. ApproxTextTokens over-counts code-heavy content by
	// ~15-20%, which would trip compaction early (at ~60% of true capacity). It
	// starts at 1.0 and converges via an EMA as each turn reports actual usage, so
	// later turns compact nearer to real capacity. Zero is treated as 1.0.
	calibrationRatio float64

	// summarizerFactory lazily builds the dedicated (cheap) summarizer provider
	// (Options.Summarizer) for the current main model. summarizerProvider
	// memoizes the built provider and summarizerResolved records that the
	// factory ran (it may legitimately return no provider: the main model is
	// already the cheap one). summarizerBroken is the sticky fallback: once a
	// build or call fails, the run stays on the main provider until the next
	// model switch — a misconfigured cheap model must never break (or
	// repeatedly delay) compaction.
	summarizerFactory  func(ctx context.Context, mainModelID string) (Provider, error)
	summarizerProvider Provider
	summarizerResolved bool
	summarizerBroken   bool
	// window is the context window the threshold was derived from. The
	// reactive free-prune stage compares against it (the limit the provider
	// enforces), not against the 70% proactive threshold — a pruned history
	// between the two would still succeed on retry.
	window int
	// mainModel is the model the run currently streams on; the summarizer
	// choice is made against it and remade after a mid-run switch.
	mainModel string
}

// calibrate folds one turn's (rawEstimate, actualPromptTokens) sample into the
// running calibration ratio. A single sample is clamped to a sane band so an
// outlier (a huge cache-read turn, a provider-overhead spike) can't skew the
// estimate enough to disable or thrash compaction.
func (state *compactionState) calibrate(rawEstimate int, actualPromptTokens int) {
	if !state.enabled || rawEstimate <= 0 || actualPromptTokens <= 0 {
		return
	}
	sample := float64(actualPromptTokens) / float64(rawEstimate)
	if sample < 0.5 {
		sample = 0.5
	} else if sample > 2.0 {
		sample = 2.0
	}
	if state.calibrationRatio <= 0 {
		state.calibrationRatio = 1.0
	}
	const alpha = 0.3 // weight on the newest sample; smooths jitter across turns
	state.calibrationRatio = state.calibrationRatio*(1-alpha) + sample*alpha
}

// calibratedTokens applies the learned ratio to a raw estimate. Before any sample
// arrives (ratio unset) it returns the raw estimate unchanged.
func (state *compactionState) calibratedTokens(raw int) int {
	if state.calibrationRatio <= 0 {
		return raw
	}
	return int(float64(raw) * state.calibrationRatio)
}

func newCompactionState(options Options, task *taskState) *compactionState {
	state := &compactionState{
		enabled:           options.ContextWindow > 0,
		threshold:         compactionThreshold(options.ContextWindow),
		window:            options.ContextWindow,
		mainModel:         options.Model,
		preserveLast:      options.CompactionPreserveLast,
		onUsage:           options.OnUsage,
		task:              task,
		planner:           newContextPlanner(contextPlannerConfig{contextWindow: options.ContextWindow, serviceTier: options.ServiceTier}),
		trace:             options.Trace,
		summarizerFactory: options.Summarizer,
	}
	return state
}

// updateWindow re-derives the compaction threshold after a mid-run model
// switch. The low-water mark is reset: it was measured against the old
// threshold, and keeping it could suppress a compaction the smaller window
// now requires. <= 0 (unknown model) leaves everything unchanged.
func (state *compactionState) updateWindow(window int) {
	if window <= 0 {
		return
	}
	state.enabled = true
	state.window = window
	state.threshold = compactionThreshold(window)
	state.lowWaterMark = 0
	if state.planner != nil {
		state.planner.config.contextWindow = window
	}
}

// switchModel records a mid-run model switch: the window is re-derived
// (window <= 0 keeps the old one) and the summarizer choice is forgotten so
// the next compaction re-resolves it against the new main model. A run that
// started on the cheap model (no dedicated summarizer) and escalated to an
// expensive one therefore regains cheap summaries — the exact workflow a
// dedicated summarizer exists for.
func (state *compactionState) switchModel(modelID string, window int) {
	state.mainModel = modelID
	state.updateWindow(window)
	state.summarizerProvider = nil
	state.summarizerResolved = false
	state.summarizerBroken = false
}

// summarizeProvider returns the provider summarization calls should use and
// whether it is the dedicated summarizer: that one when it applies to the
// current main model and is healthy, else the main provider. The factory runs
// at most once per main model; a build error marks the summarizer broken for
// that model. Callers branch on the boolean rather than comparing Provider
// values — interface comparison panics on a non-comparable dynamic type.
func (state *compactionState) summarizeProvider(ctx context.Context, main Provider) (Provider, bool) {
	if state.summarizerFactory == nil || state.summarizerBroken {
		return main, false
	}
	if !state.summarizerResolved {
		state.summarizerResolved = true
		built, err := state.summarizerFactory(ctx, state.mainModel)
		if err != nil {
			state.summarizerBroken = true
			return main, false
		}
		state.summarizerProvider = built
	}
	if state.summarizerProvider == nil {
		return main, false
	}
	return state.summarizerProvider, true
}

// fitsWindow reports whether a calibrated size is safely under the limit the
// provider enforces. The proactive threshold (70% of the window) is a trigger
// for the paid summarizer, not the hard limit; the reactive path compares
// against the window itself, with a small margin for estimate error, so a
// history pruned to, say, 85% of the window takes the free retry.
func (state *compactionState) fitsWindow(size int) bool {
	if state.window <= 0 {
		return size <= state.threshold
	}
	return size <= state.window-state.window/20
}

// maybeCompact runs proactive compaction at the top of a turn. It returns the
// (possibly compacted) message slice. It is a no-op when compaction is disabled,
// when the history is under threshold, or when the history has not grown past
// the low-water mark since the last compaction (the infinite-loop guard).
func (state *compactionState) maybeCompact(
	ctx context.Context,
	provider Provider,
	messages []zeroruntime.Message,
	tools []zeroruntime.ToolDefinition,
) []zeroruntime.Message {
	if !state.enabled {
		return messages
	}
	// Tool definitions are part of every request's input, so count them alongside
	// the messages; both the threshold check and the shrink check below use the
	// same term so they stay consistent.
	toolTokens := estimateToolDefTokens(tools)
	size := state.calibratedTokens(estimateTokens(messages) + toolTokens)
	if size <= state.threshold {
		return messages
	}
	// Only compact when the history has grown past where we last left it. This
	// stops the loop from re-summarizing an already-compacted history every turn
	// when it sits just over the threshold.
	if state.lowWaterMark > 0 && size <= state.lowWaterMark {
		return messages
	}

	// CHEAP FIRST STAGE: reclaim context at zero token/latency cost by pruning
	// the bodies of old, large tool results (the model has already acted on
	// them). If that brings us back under threshold, skip the paid summarizer
	// entirely and preserve recent turns verbatim.
	if pruned, reclaimed := pruneSupersededReadResults(messages); reclaimed > 0 {
		messages = pruned
		size = state.calibratedTokens(estimateTokens(messages) + toolTokens)
		if size <= state.threshold {
			state.lowWaterMark = size
			return messages
		}
	}
	if pruned, reclaimed := pruneStaleToolOutput(messages, state.preserveLast); reclaimed > 0 {
		messages = pruned
		size = state.calibratedTokens(estimateTokens(messages) + toolTokens)
		if size <= state.threshold {
			state.lowWaterMark = size
			return messages
		}
	}

	compacted, err := Compact(messages, CompactionOptions{
		PreserveLast: state.preserveLast,
		Summarize:    state.summarizeClosure(ctx, provider),
		taskState:    state.task.snapshotForCompaction(messages),
	})
	if err != nil {
		// Summarizer failed: keep the original history. The reactive path (or a
		// later turn) can try again; we never drop messages on failure here.
		return messages
	}
	newSize := state.calibratedTokens(estimateTokens(compacted) + toolTokens)
	if newSize >= size {
		// Compaction did not actually shrink anything (e.g. nothing to
		// summarize). Leave the history untouched and don't churn next turn.
		state.lowWaterMark = size
		return messages
	}
	// Only count a compaction when it actually shrank the history, so the
	// compaction counter reflects real context reductions rather than paid
	// no-ops that left the token budget untouched.
	if r := trace.FromContext(ctx); r != nil {
		r.Counter(trace.CounterCompactionCount, 1)
	}
	state.lowWaterMark = newSize
	return compacted
}

// recover runs reactive compaction after a provider/stream error. It compacts
// at most once per run when the error looks like a context-limit error and the
// history can actually be shrunk. The returned booleans are (compacted, retried) and the
// error is non-nil only when compaction itself failed (so the loop should give
// up). When retried is false the caller keeps its original error.
func (state *compactionState) recover(
	ctx context.Context,
	provider Provider,
	messages []zeroruntime.Message,
	tools []zeroruntime.ToolDefinition,
	errorMessage string,
) (compacted []zeroruntime.Message, retried bool, err error) {
	if !state.enabled {
		// Compaction disabled (ContextWindow==0): stay a strict no-op so a
		// context-limit error never triggers an unexpected summarization call.
		return messages, false, nil
	}
	if !isContextLimitError(errorMessage) {
		return messages, false, nil
	}

	// CHEAP FIRST STAGE (mirrors maybeCompact): a context-limit failure can
	// often be fixed for free by dropping superseded reads and pruning old
	// tool-result bodies. It runs regardless of the one-shot reactive budget
	// (it costs nothing and cannot loop: a second pass finds nothing left to
	// prune). The free retry is taken only when the pruned history fits the
	// window the provider enforces — otherwise a retry would fail the same
	// way, so fall through to the paid summarizer in this same call.
	toolTokens := estimateToolDefTokens(tools)
	prunedAny := false
	if pruned, reclaimed := pruneSupersededReadResults(messages); reclaimed > 0 {
		messages, prunedAny = pruned, true
	}
	if pruned, reclaimed := pruneStaleToolOutput(messages, state.preserveLast); reclaimed > 0 {
		messages, prunedAny = pruned, true
	}
	if prunedAny {
		size := state.calibratedTokens(estimateTokens(messages) + toolTokens)
		if state.fitsWindow(size) {
			state.lowWaterMark = size
			return messages, true, nil
		}
	}

	if state.reactiveAttempted {
		return messages, false, nil
	}

	result, compactErr := Compact(messages, CompactionOptions{
		PreserveLast: state.preserveLast,
		Summarize:    state.summarizeClosure(ctx, provider),
		taskState:    state.task.snapshotForCompaction(messages),
	})
	if compactErr != nil {
		// A genuine compaction attempt was made (and failed): the budget is spent
		// so the loop gives up rather than retrying a failing summarizer forever.
		state.reactiveAttempted = true
		return messages, true, compactErr
	}
	if estimateTokens(result) >= estimateTokens(messages) {
		// Nothing to compact; the retry would just fail again. Signal "not
		// retried" so the caller surfaces the original context-limit error. Do NOT
		// consume the one-shot budget here: a no-op recover (history too small to
		// shrink) must not disable a later recovery once the history has grown.
		return messages, false, nil
	}
	// Success: a real compaction shrank the history and we will retry. Consume the
	// one-shot budget now so a provider that keeps returning context-limit errors
	// after a successful compaction can't loop forever. Store the low-water mark in
	// the SAME combined (messages + tool-defs) domain maybeCompact uses, so the
	// proactive shrink-guard compares like with like. Count it now that the shrink
	// is confirmed, so the counter mirrors maybeCompact's real-reduction policy.
	if r := trace.FromContext(ctx); r != nil {
		r.Counter(trace.CounterCompactionCount, 1)
	}
	state.reactiveAttempted = true
	state.lowWaterMark = state.calibratedTokens(estimateTokens(result) + estimateToolDefTokens(tools))
	return result, true, nil
}

// summarizeClosure builds a Summarize function backed by a focused, tool-less
// provider call. The summary stream intentionally does NOT forward OnText (so
// compaction stays invisible on the user-facing surface), but it DOES forward
// OnUsage so the summarizer's token cost is still counted by usage/budgeting.
// The call goes to the dedicated (cheap) summarizer when one applies; if that
// call fails the summarizer is marked broken for the current model and the
// same slice is retried once on the main provider, so the caller sees at most
// one (main-provider-quality) error.
func (state *compactionState) summarizeClosure(ctx context.Context, provider Provider) func([]zeroruntime.Message) (string, error) {
	if state.planner == nil {
		state.planner = newContextPlanner(contextPlannerConfig{})
	}
	return func(toSummarize []zeroruntime.Message) (string, error) {
		chosen, dedicated := state.summarizeProvider(ctx, provider)
		summary, err := summarizeWithPlanner(ctx, chosen, toSummarize, state.onUsage, state.planner, state.trace)
		if err == nil || !dedicated {
			return summary, err
		}
		state.summarizerBroken = true
		return summarizeWithPlanner(ctx, provider, toSummarize, state.onUsage, state.planner, state.trace)
	}
}

// summarizeWithFallback summarizes messages in a single provider call. If that
// call fails with a context-limit error — the slice to summarize is itself too
// large for the model's input window — it splits the slice in half, summarizes
// each half recursively, and joins the partial summaries. This keeps compaction
// working when the elided middle is bigger than the summarizer's own context.
// Non-context-limit errors (and a single message that still won't fit) surface
// to the caller unchanged.
func summarizeWithFallback(ctx context.Context, provider Provider, messages []zeroruntime.Message, onUsage func(Usage)) (string, error) {
	return summarizeWithPlanner(ctx, provider, messages, onUsage, newContextPlanner(contextPlannerConfig{}), nil)
}

func summarizeWithPlanner(ctx context.Context, provider Provider, messages []zeroruntime.Message, onUsage func(Usage), planner *contextPlanner, recorder *trace.Recorder) (string, error) {
	summary, err := summarizeMessagesOnce(ctx, provider, messages, onUsage, planner, recorder)
	if err == nil {
		return summary, nil
	}
	if len(messages) < 2 || !isContextLimitError(err.Error()) {
		return "", err
	}

	mid := len(messages) / 2
	left, leftErr := summarizeWithPlanner(ctx, provider, messages[:mid], onUsage, planner, recorder)
	if leftErr != nil {
		return "", leftErr
	}
	right, rightErr := summarizeWithPlanner(ctx, provider, messages[mid:], onUsage, planner, recorder)
	if rightErr != nil {
		return "", rightErr
	}

	// Re-summarize the two partial summaries into ONE so the persisted summary
	// stays a single, further-summarizable unit — not an ever-growing concatenated
	// blob that a later compaction (which can't split a single message) would fail
	// on. If even the combined partials don't fit (extreme), fall back to the
	// joined text: still better than failing, and each half is already compacted.
	combined := strings.TrimSpace(left + "\n\n" + right)
	reduced, reduceErr := summarizeMessagesOnce(ctx, provider, []zeroruntime.Message{
		{Role: zeroruntime.MessageRoleUser, Content: combined},
	}, onUsage, planner, recorder)
	if reduceErr != nil {
		if isContextLimitError(reduceErr.Error()) {
			// Even the two combined partials don't fit (extreme): fall back to the
			// joined text — still better than failing, and each half is already
			// compacted.
			return combined, nil
		}
		// A non-context failure (auth/network/provider) must surface unchanged, per
		// this function's contract — don't mask it behind the joined fallback.
		return "", reduceErr
	}
	return reduced, nil
}

// summarizeMessagesOnce performs a single tool-less summarization call.
func summarizeMessagesOnce(ctx context.Context, provider Provider, messages []zeroruntime.Message, onUsage func(Usage), planner *contextPlanner, recorder *trace.Recorder) (string, error) {
	requestMessages := []zeroruntime.Message{
		{Role: zeroruntime.MessageRoleSystem, Content: CompactionSummaryInstructions},
		{Role: zeroruntime.MessageRoleUser, Content: "Summarize this conversation:\n\n" + renderTranscript(messages)},
	}
	parts := systemPromptParts{prompt: CompactionSummaryInstructions, baseInstructions: CompactionSummaryInstructions}
	plan := planner.planWithPromptParts(requestMessages, nil, "", parts)
	recordContextPlanTrace(recorder, plan)
	stream, err := provider.StreamCompletion(ctx, plan.Request)
	if err != nil {
		return "", err
	}
	// Forward OnUsage (token accounting) but not OnText (keep compaction invisible
	// to the user); a nil onUsage is a no-op.
	collected := zeroruntime.CollectStreamWithOptions(ctx, stream, zeroruntime.CollectOptions{OnUsage: onUsage})
	if collected.Error != "" {
		return "", errors.New(collected.Error)
	}
	summary := strings.TrimSpace(collected.Text)
	if summary == "" {
		return "", errors.New("summarizer returned no text")
	}
	return summary, nil
}

// summaryToolResultClamp bounds each tool-result body in the summarizer input.
// Tool outputs dominate the elided middle's bulk (build logs, file reads,
// search results), yet the summary instructions only need their gist — the
// model that ACTED on the full output already turned it into decisions the
// surrounding messages record. 2,000 chars (~500 tokens) keeps the useful
// head+tail of a result while cutting the summarizer input by 5-10x on
// tool-heavy sessions; the summarizer's own cost is the price being cut.
const summaryToolResultClamp = 2000

// summaryToolArgsClamp bounds each tool-call argument blob in the transcript:
// a write_file call carries the whole file body in its arguments.
const summaryToolArgsClamp = 500

// clampForSummary head+tail-truncates text for the summarizer transcript,
// snapping to rune boundaries so the clamp never splits a UTF-8 sequence.
func clampForSummary(text string, max int) string {
	if max <= 0 || len(text) <= max {
		return text
	}
	head := max * 3 / 4
	tail := max - head
	for head > 0 && !utf8.RuneStart(text[head]) {
		head--
	}
	start := len(text) - tail
	for start < len(text) && !utf8.RuneStart(text[start]) {
		start++
	}
	return text[:head] + "\n…[" + strconv.Itoa(len(text)-head-(len(text)-start)) + " chars omitted for summarization]…\n" + text[start:]
}

// renderTranscript flattens messages into a plain-text transcript for the
// summarizer. Secret scrubbing already happened upstream at the tool boundary.
// Tool-result bodies and tool-call arguments are clamped (see the constants
// above): the summary needs what was done and decided, not the raw payloads.
func renderTranscript(messages []zeroruntime.Message) string {
	lines := make([]string, 0, len(messages))
	for _, message := range messages {
		switch message.Role {
		case zeroruntime.MessageRoleAssistant:
			line := "assistant: " + message.Content
			if len(message.ToolCalls) > 0 {
				calls := make([]string, 0, len(message.ToolCalls))
				for _, call := range message.ToolCalls {
					calls = append(calls, call.Name+"("+clampForSummary(call.Arguments, summaryToolArgsClamp)+")")
				}
				line += "\n[tool calls: " + strings.Join(calls, "; ") + "]"
			}
			lines = append(lines, line)
		case zeroruntime.MessageRoleTool:
			lines = append(lines, "tool result: "+clampForSummary(message.Content, summaryToolResultClamp))
		default:
			lines = append(lines, string(message.Role)+": "+message.Content)
		}
	}
	return strings.Join(lines, "\n\n")
}
