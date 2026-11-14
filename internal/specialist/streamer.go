package specialist

import (
	"fmt"
	"strings"

	"github.com/Twigpine/zero/internal/streamjson"
	"github.com/Twigpine/zero/internal/tools"
)

type StreamResult struct {
	Events    []streamjson.Event
	RunID     string
	SessionID string
	Text      string
	// Tools is the DISTINCT tool names the child used, in first-use order.
	Tools []string
	// ToolCalls is how many tool calls the child made — every call, repeats
	// included. It is the figure the AGENTS panel counts live, so a summary that
	// reports a count must report this one, not len(Tools).
	ToolCalls int
	Errors    []string
	Status    string
	ExitCode  int
	Usage     StreamUsage
}

type StreamUsage struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	// CachedInputTokens, CacheWriteTokens and ReasoningTokens are what make a
	// child's turn PRICEABLE by its parent. Absent, every rolled-up sub-agent
	// turn was costed as if nothing had been cached — on a plan task, where the
	// same large prompt is re-sent each turn, that is the overwhelming majority
	// of the input.
	CachedInputTokens int
	CacheWriteTokens  int
	ReasoningTokens   int
	Events            int
}

func (usage StreamUsage) HasUsage() bool {
	return usage.Events > 0 || usage.PromptTokens != 0 || usage.CompletionTokens != 0 || usage.TotalTokens != 0
}

func (usage StreamUsage) EffectiveTotalTokens() int {
	return usage.TotalTokens
}

func SummarizeStream(events []streamjson.Event, processExitCode int) StreamResult {
	result := StreamResult{Events: append([]streamjson.Event(nil), events...), ExitCode: processExitCode}
	textParts := []string{}
	finalText := ""
	seenTools := map[string]bool{}
	for _, event := range events {
		if result.RunID == "" && strings.TrimSpace(event.RunID) != "" {
			result.RunID = strings.TrimSpace(event.RunID)
		}
		if result.SessionID == "" && strings.TrimSpace(event.SessionID) != "" {
			result.SessionID = strings.TrimSpace(event.SessionID)
		}
		switch event.Type {
		case streamjson.EventText:
			textParts = append(textParts, event.Delta)
		case streamjson.EventFinal:
			finalText = event.Text
		case streamjson.EventToolCall:
			result.ToolCalls++
			if name := strings.TrimSpace(event.Name); name != "" && !seenTools[name] {
				seenTools[name] = true
				result.Tools = append(result.Tools, name)
			}
		case streamjson.EventError:
			message := strings.TrimSpace(event.Message)
			if message == "" {
				message = strings.TrimSpace(event.Code)
			}
			if message != "" {
				result.Errors = append(result.Errors, message)
			}
		case streamjson.EventRunEnd:
			result.Status = strings.TrimSpace(event.Status)
			if event.ExitCode != nil {
				result.ExitCode = *event.ExitCode
			}
		case streamjson.EventUsage:
			result.Usage.Events++
			eventPromptTokens := 0
			eventCompletionTokens := 0
			if event.PromptTokens != nil {
				eventPromptTokens = *event.PromptTokens
				result.Usage.PromptTokens += eventPromptTokens
			}
			if event.CompletionTokens != nil {
				eventCompletionTokens = *event.CompletionTokens
				result.Usage.CompletionTokens += eventCompletionTokens
			}
			if event.TotalTokens != nil {
				result.Usage.TotalTokens += *event.TotalTokens
			} else {
				result.Usage.TotalTokens += eventPromptTokens + eventCompletionTokens
			}
			// Summed, not overwritten: a task makes one provider call per turn
			// and each reports its own cache split.
			if event.CachedInputTokens != nil {
				result.Usage.CachedInputTokens += *event.CachedInputTokens
			}
			if event.CacheWriteTokens != nil {
				result.Usage.CacheWriteTokens += *event.CacheWriteTokens
			}
			if event.ReasoningTokens != nil {
				result.Usage.ReasoningTokens += *event.ReasoningTokens
			}
		}
	}
	if finalText != "" {
		result.Text = strings.TrimSpace(finalText)
	} else {
		result.Text = strings.TrimSpace(strings.Join(textParts, ""))
	}
	return result
}

// sessionIDLinePrefix heads the line BuildFinalResult prepends to a successful
// subagent's output, so the parent can continue that child by id.
const sessionIDLinePrefix = "session_id: "

// WithoutSessionIDLine removes that line for a caller that already holds the id
// structurally and whose output a PERSON reads.
//
// A PLAN TASK IS BOTH. ExecResult.SessionID carries the id to the plan runner
// already, so the line adds nothing there — and a plan task's output is not a
// tool result the parent model consumes on its own. It is quoted into the report
// under "result:", pasted into the dependency briefing every downstream task
// reads, and rendered in the plan panel, so the line surfaced a raw child
// session id in the middle of a user-facing answer three separate ways. Nothing
// can continue a plan task's child by that id in any case: the plan owns its
// children's lifetimes.
//
// KEYED ON THE ID WE KNOW, never on the pattern. It strips the first line only
// when that line is exactly this prefix followed by the id the caller was handed,
// so it cannot cut a line of the child's own prose that happens to begin the same
// way — the fuzzy-match rule that this repo has broken more than once.
func WithoutSessionIDLine(output, sessionID string) string {
	if sessionID == "" {
		return output
	}
	line := sessionIDLinePrefix + sessionID
	rest, ok := strings.CutPrefix(output, line)
	if !ok {
		return output
	}
	if rest != "" && !strings.HasPrefix(rest, "\n") {
		return output
	}
	return strings.TrimLeft(rest, "\n")
}

func BuildFinalResult(events []streamjson.Event, stderrOutput string, processExitCode int, signalDesc string) tools.Result {
	summary := SummarizeStream(events, processExitCode)
	hasErrors := len(summary.Errors) > 0 || summary.ExitCode != 0
	if summary.Status != "" && summary.Status != "success" && summary.Status != "ok" {
		hasErrors = true
	}
	// A captured kill signal must always surface, even if a late run_end reported a
	// clean exit just before the child was killed during teardown.
	if strings.TrimSpace(signalDesc) != "" {
		hasErrors = true
	}
	if !hasErrors {
		output := summary.Text
		if summary.SessionID != "" {
			output = sessionIDLinePrefix + summary.SessionID + "\n" + output
		}
		return tools.Result{Status: tools.StatusOK, Output: strings.TrimSpace(output)}
	}

	var builder strings.Builder
	if signal := strings.TrimSpace(signalDesc); signal != "" {
		// The child was terminated by a signal (exit code -1) rather than exiting.
		// Surface the signal instead of an opaque "exit -1", and list the common
		// causes evenhandedly — this branch also covers timeouts and cancellations,
		// so don't assert OOM.
		fmt.Fprintf(&builder, "Subagent terminated by a signal (%s) — it was killed before it finished. Common causes: an out-of-memory kill, a timeout, or cancellation; check the signal to tell which. If you were running many sub-agents in parallel, reduce the concurrency.\n", signal)
	} else {
		fmt.Fprintf(&builder, "Subagent failed (exit %d)\n", summary.ExitCode)
	}
	if len(summary.Errors) > 0 {
		fmt.Fprintf(&builder, "errors: %s\n", strings.Join(summary.Errors, "; "))
	}
	if stderr := strings.TrimSpace(stderrOutput); stderr != "" {
		fmt.Fprintf(&builder, "stderr:\n%s\n", stderr)
	}
	if len(summary.Tools) > 0 {
		fmt.Fprintf(&builder, "tools executed: %s\n", strings.Join(summary.Tools, ", "))
	}
	if text := strings.TrimSpace(summary.Text); text != "" {
		fmt.Fprintf(&builder, "\n%s\n", text)
	}
	return tools.Result{Status: tools.StatusError, Output: strings.TrimSpace(builder.String())}
}
