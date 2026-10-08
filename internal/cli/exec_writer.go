package cli

import (
	"encoding/json"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Gitlawb/zero/internal/agent"
	"github.com/Gitlawb/zero/internal/sessions"
	"github.com/Gitlawb/zero/internal/streamjson"
	"github.com/Gitlawb/zero/internal/tools"
)

const streamJSONToolResultOutputLimit = 10 * 1024

type execRunMetadata struct {
	Provider string
	Model    string
	APIModel string
}

type execEventWriter struct {
	stdout       io.Writer
	stderr       io.Writer
	format       execOutputFormat
	runID        string
	sessionID    string
	streamedText *strings.Builder
	err          error
}

func (writer *execEventWriter) runStart(cwd string, metadata execRunMetadata, permissionMode agent.PermissionMode) {
	switch writer.format {
	case execOutputJSON:
		writer.writeJSON(map[string]any{
			"type":            "run_start",
			"cwd":             cwd,
			"provider":        metadata.Provider,
			"model":           metadata.Model,
			"api_model":       metadata.APIModel,
			"permission_mode": string(permissionMode),
		})
	case execOutputStreamJSON:
		writer.writeStreamJSON(streamjson.Event{
			Type:      streamjson.EventRunStart,
			RunID:     writer.runID,
			SessionID: writer.sessionID,
			Cwd:       cwd,
			Provider:  metadata.Provider,
			Model:     metadata.Model,
			APIModel:  metadata.APIModel,
		})
	}
}

func (writer *execEventWriter) warning(message string) {
	if writer.format == execOutputJSON {
		writer.writeJSON(map[string]any{"type": "warning", "message": message})
		return
	}
	if writer.format == execOutputStreamJSON {
		writer.writeStreamJSON(streamjson.Event{Type: streamjson.EventWarning, RunID: writer.runID, Message: message})
		return
	}
	writer.writeStderr("[zero] WARNING: " + message + "\n")
}

func (writer *execEventWriter) text(delta string) {
	writer.streamedText.WriteString(delta)
	if writer.format == execOutputJSON {
		writer.writeJSON(map[string]any{"type": "text", "delta": delta})
		return
	}
	if writer.format == execOutputStreamJSON {
		writer.writeStreamJSON(streamjson.Event{Type: streamjson.EventText, RunID: writer.runID, Delta: delta})
		return
	}
	writer.writeStdout(delta)
}

func (writer *execEventWriter) reasoning(delta string) {
	if writer.format == execOutputJSON {
		writer.writeJSON(map[string]any{"type": "reasoning", "delta": delta})
		return
	}
	if writer.format == execOutputStreamJSON {
		writer.writeStreamJSON(streamjson.Event{Type: streamjson.EventReasoning, RunID: writer.runID, Delta: delta})
	}
}

func (writer *execEventWriter) toolCall(call agent.ToolCall, registry *tools.Registry) {
	if writer.format == execOutputJSON {
		writer.writeJSON(map[string]any{
			"type":      "tool_call",
			"id":        call.ID,
			"name":      call.Name,
			"arguments": call.Arguments,
		})
		return
	}
	if writer.format == execOutputStreamJSON {
		writer.writeStreamJSON(streamjson.Event{
			Type:       streamjson.EventToolCall,
			RunID:      writer.runID,
			ID:         call.ID,
			Name:       call.Name,
			Args:       parseToolCallArgs(call.Arguments),
			SideEffect: streamJSONSideEffect(call.Name, registry),
		})
		return
	}
	writer.writeStderr("[tool] " + call.Name + "\n")
}

func (writer *execEventWriter) checkpoint(event sessions.Event) {
	var payload sessions.CheckpointPayload
	if len(event.Payload) > 0 {
		_ = json.Unmarshal(event.Payload, &payload)
	}
	files := make([]string, 0, len(payload.Files))
	for _, f := range payload.Files {
		files = append(files, f.Path)
	}
	switch writer.format {
	case execOutputStreamJSON:
		writer.writeStreamJSON(streamjson.Event{
			Type:       streamjson.EventCheckpoint,
			RunID:      writer.runID,
			Checkpoint: &streamjson.CheckpointInfo{Sequence: event.Sequence, Tool: payload.Tool, Files: files},
		})
	case execOutputJSON:
		writer.writeJSON(map[string]any{
			"type":     "checkpoint",
			"sequence": event.Sequence,
			"tool":     payload.Tool,
			"files":    files,
		})
	}
	// Plain text mode stays silent: checkpoints are background safety, not output.
}

func (writer *execEventWriter) toolResult(result agent.ToolResult) {
	modelOutput := result.ModelOutput()
	display := result.HumanDisplay()
	if writer.format == execOutputJSON {
		payload := map[string]any{
			"type":         "tool_result",
			"tool_call_id": result.ToolCallID,
			"name":         result.Name,
			"status":       string(result.Status),
			"output":       modelOutput,
		}
		if len(result.Meta) > 0 {
			payload["meta"] = result.Meta
		}
		if result.Truncated {
			payload["truncated"] = true
		}
		if result.Redacted {
			payload["redacted"] = true
		}
		if len(result.ChangedFiles) > 0 {
			payload["changed_files"] = result.ChangedFiles
		}
		if display.Summary != "" || display.Kind != "" {
			payload["display"] = map[string]string{"summary": display.Summary, "kind": display.Kind}
		}
		writer.writeJSON(payload)
		return
	}
	if writer.format == execOutputStreamJSON {
		output, surfaceTruncated := truncateForStreamJSONOutput(modelOutput)
		truncated := result.Truncated || surfaceTruncated
		event := streamjson.Event{
			Type:         streamjson.EventToolResult,
			RunID:        writer.runID,
			ID:           result.ToolCallID,
			Name:         result.Name,
			Status:       string(result.Status),
			Output:       output,
			Truncated:    &truncated,
			ChangedFiles: result.ChangedFiles,
			Meta:         result.Meta,
		}
		if result.Redacted {
			redacted := true
			event.Redacted = &redacted
		}
		if display.Summary != "" || display.Kind != "" {
			event.Display = &streamjson.Display{Summary: display.Summary, Kind: display.Kind}
		}
		writer.writeStreamJSON(event)
		return
	}
	writer.writeStderr("[result] " + truncateForStatus(modelOutput) + "\n")
}

func (writer *execEventWriter) permission(event agent.PermissionEvent) {
	if writer.format == execOutputJSON {
		payload := map[string]any{
			"type":               "permission",
			"tool_call_id":       event.ToolCallID,
			"name":               event.ToolName,
			"action":             string(event.Action),
			"permission":         event.Permission,
			"permission_granted": event.PermissionGranted,
			"permission_mode":    string(event.PermissionMode),
			"autonomy":           event.Autonomy,
			"side_effect":        event.SideEffect,
			"reason":             event.Reason,
			"decision_reason":    event.DecisionReason,
			"risk":               event.Risk,
		}
		if event.Block != nil {
			payload["block"] = event.Block
		}
		if event.GrantMatched {
			payload["grant_matched"] = true
		}
		if event.Grant != nil {
			payload["grant"] = event.Grant
		}
		writer.writeJSON(payload)
		return
	}
	if writer.format == execOutputStreamJSON {
		risk := event.Risk
		permissionGranted := event.PermissionGranted
		writer.writeStreamJSON(streamjson.Event{
			Type:              streamJSONPermissionEventType(event),
			RunID:             writer.runID,
			SessionID:         writer.sessionID,
			ID:                event.ToolCallID,
			Name:              event.ToolName,
			Action:            string(event.Action),
			Permission:        event.Permission,
			PermissionGranted: &permissionGranted,
			PermissionMode:    string(event.PermissionMode),
			Autonomy:          event.Autonomy,
			SideEffect:        event.SideEffect,
			Reason:            event.Reason,
			DecisionReason:    event.DecisionReason,
			Risk:              &risk,
			Block:             event.Block,
			GrantMatched:      event.GrantMatched,
			Grant:             event.Grant,
		})
		return
	}
	if event.Action != agent.PermissionActionAllow {
		writer.writeStderr("[permission] " + event.ToolName + " " + string(event.Action) + ": " + event.Reason + "\n")
	}
}

func streamJSONPermissionEventType(event agent.PermissionEvent) streamjson.EventType {
	if event.Action == agent.PermissionActionPrompt {
		return streamjson.EventPermissionRequest
	}
	if event.Action == agent.PermissionActionAllow || event.Action == agent.PermissionActionDeny || event.Action == agent.PermissionActionCancel {
		return streamjson.EventPermissionDecision
	}
	return streamjson.EventPermission
}

func (writer *execEventWriter) usage(usage agent.Usage) {
	if writer.format == execOutputJSON {
		writer.writeJSON(map[string]any{
			"type":                "usage",
			"prompt_tokens":       usage.PromptTokens,
			"completion_tokens":   usage.CompletionTokens,
			"cached_input_tokens": usage.CachedInputTokens,
			"cache_write_tokens":  usage.CacheWriteTokens,
			"total_tokens":        usage.TotalTokens(),
		})
		return
	}
	if writer.format == execOutputStreamJSON {
		promptTokens := usage.EffectiveInputTokens()
		completionTokens := usage.EffectiveOutputTokens()
		cachedInputTokens := usage.CachedInputTokens
		cacheWriteTokens := usage.CacheWriteTokens
		totalTokens := usage.TotalTokens()
		writer.writeStreamJSON(streamjson.Event{
			Type:              streamjson.EventUsage,
			RunID:             writer.runID,
			PromptTokens:      &promptTokens,
			CompletionTokens:  &completionTokens,
			CachedInputTokens: &cachedInputTokens,
			CacheWriteTokens:  &cacheWriteTokens,
			TotalTokens:       &totalTokens,
		})
	}
}

func (writer *execEventWriter) final(answer string) {
	if writer.format == execOutputJSON {
		writer.writeJSON(map[string]any{"type": "final", "text": answer})
		writer.writeJSON(map[string]any{"type": "done", "exit_code": exitSuccess})
		return
	}
	if writer.format == execOutputStreamJSON {
		writer.writeStreamJSON(streamjson.Event{Type: streamjson.EventFinal, RunID: writer.runID, Text: answer})
		return
	}

	if writer.streamedText.Len() == 0 && answer != "" {
		writer.writeStdout(answer)
		writer.streamedText.WriteString(answer)
	}
	if writer.streamedText.Len() > 0 && !strings.HasSuffix(writer.streamedText.String(), "\n") {
		writer.writeStdout("\n")
	}
}

func (writer *execEventWriter) errorEvent(code string, message string, recoverable bool) {
	if writer.format == execOutputStreamJSON {
		writer.writeStreamJSON(streamjson.Event{
			Type:        streamjson.EventError,
			RunID:       writer.runID,
			Code:        code,
			Message:     message,
			Recoverable: &recoverable,
		})
		return
	}
	if writer.format == execOutputJSON {
		writer.writeJSON(map[string]any{"type": "error", "code": code, "message": message})
		return
	}
	writer.writeStderr("[zero] " + message + "\n")
}

func (writer *execEventWriter) runEnd(status string, exitCode int) {
	if writer.format != execOutputStreamJSON {
		return
	}
	writer.writeStreamJSON(streamjson.Event{
		Type:     streamjson.EventRunEnd,
		RunID:    writer.runID,
		Status:   status,
		ExitCode: &exitCode,
	})
}

func (writer *execEventWriter) writeStreamJSON(event streamjson.Event) {
	if writer.err != nil {
		return
	}
	line, err := streamjson.FormatEvent(event)
	if err != nil {
		writer.err = err
		return
	}
	writer.writeStdout(line + "\n")
}

func (writer *execEventWriter) writeJSON(payload map[string]any) {
	if writer.err != nil {
		return
	}
	writer.err = writeJSONLine(writer.stdout, payload)
}

func (writer *execEventWriter) writeStdout(value string) {
	if writer.err != nil {
		return
	}
	_, writer.err = io.WriteString(writer.stdout, value)
}

func (writer *execEventWriter) writeStderr(value string) {
	if writer.err != nil {
		return
	}
	_, writer.err = io.WriteString(writer.stderr, value)
}

func writeJSONLine(w io.Writer, payload map[string]any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := w.Write(append(data, '\n')); err != nil {
		return err
	}
	return nil
}

func parseToolCallArgs(arguments string) any {
	if strings.TrimSpace(arguments) == "" {
		return nil
	}
	var args any
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return arguments
	}
	return args
}

func streamJSONSideEffect(name string, registry *tools.Registry) string {
	tool, ok := registry.Get(name)
	if !ok {
		return "unknown"
	}
	switch tool.Safety().SideEffect {
	case tools.SideEffectRead:
		return "read"
	case tools.SideEffectWrite:
		return "write"
	case tools.SideEffectShell:
		return "shell"
	case tools.SideEffectNetwork:
		return "network"
	case tools.SideEffectNone:
		return "none"
	default:
		return "unknown"
	}
}

func truncateForStatus(value string) string {
	compact := strings.Join(strings.Fields(value), " ")
	if len(compact) > 200 {
		return cutRuneBoundary(compact, 200) + "..."
	}
	return compact
}

func truncateForStreamJSONOutput(value string) (string, bool) {
	if len(value) <= streamJSONToolResultOutputLimit {
		return value, false
	}
	// Rune-boundary cut: stream-json is a machine-readable protocol and a
	// mid-rune byte slice would emit invalid UTF-8 into it.
	return cutRuneBoundary(value, streamJSONToolResultOutputLimit) + "\n[truncated]", true
}

// cutRuneBoundary truncates s to at most n bytes without splitting a UTF-8
// rune (the cut lands on the last rune boundary at or before n).
func cutRuneBoundary(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func writeStreamJSONError(stdout io.Writer, code string, message string, recoverable bool, exitCode int) int {
	runID, err := streamjson.CreateRunID(timeNow())
	if err != nil {
		return exitCrash
	}
	writer := execEventWriter{
		stdout: stdout,
		format: execOutputStreamJSON,
		runID:  runID,
	}
	writer.errorEvent(code, message, recoverable)
	writer.runEnd("error", exitCode)
	if writer.err != nil {
		return exitCrash
	}
	return exitCode
}

func timeNow() time.Time {
	return time.Now()
}
