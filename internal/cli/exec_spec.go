package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/Gitlawb/zero/internal/agent"
	"github.com/Gitlawb/zero/internal/config"
	"github.com/Gitlawb/zero/internal/execution"
	"github.com/Gitlawb/zero/internal/modelregistry"
	"github.com/Gitlawb/zero/internal/notify"
	"github.com/Gitlawb/zero/internal/sandbox"
	"github.com/Gitlawb/zero/internal/sessions"
	"github.com/Gitlawb/zero/internal/specmode"
	"github.com/Gitlawb/zero/internal/streamjson"
	"github.com/Gitlawb/zero/internal/tools"
	"github.com/Gitlawb/zero/internal/usage"
	"github.com/Gitlawb/zero/internal/zeroruntime"
)

type execSpecDraftRun struct {
	options       execOptions
	stdout        io.Writer
	stderr        io.Writer
	deps          appDeps
	workspaceRoot string
	// trustRoot is the ORIGINAL launch directory (captured before any --worktree
	// reassignment), so a --use-spec run inside a --worktree of a trusted repo
	// still keys the trust check on the source repo, not the generated worktree path.
	trustRoot string
	// mcpSkip is the trust verdict from the workspace MCP registration in runExec
	// (which runs before this spec-draft path), so the spec-draft notice can report a
	// dropped project MCP layer instead of leaving it silent.
	mcpSkip            trustSkip
	registry           *tools.Registry
	modelRegistry      modelregistry.Registry
	resolved           config.ResolvedConfig
	runMetadata        execRunMetadata
	provider           zeroruntime.Provider
	sandboxEngine      *sandbox.Engine
	prompt             string
	sessionTitle       string
	images             []zeroruntime.ImageBlock
	reasoningEffort    string
	specPermissionMode agent.PermissionMode
	notifier           *notify.Notifier
	// profilePolicy carries the selected execution profile's escalation policy
	// (nil when none): the profile displaces resolved.MaxTurns before this
	// path branches off, so the spec-draft run needs the same safety net as
	// the main run.
	profilePolicy *agent.ProfilePolicy
}

type execSpecDraftInfo struct {
	SpecID         string
	SpecTitle      string
	SpecFilePath   string
	RelativePath   string
	DraftSessionID string
}

func runExecSpecDraft(run execSpecDraftRun) int {
	store := run.deps.newSessionStore()
	draftSession, err := store.Create(sessions.CreateInput{
		SessionID:          run.options.initSessionID,
		SessionKind:        sessions.SessionKindSpecDraft,
		Title:              run.sessionTitle,
		Cwd:                run.workspaceRoot,
		ModelID:            run.resolved.Provider.Model,
		Provider:           run.runMetadata.Provider,
		SpecDraftModelID:   run.resolved.Provider.Model,
		SpecDraftReasoning: run.reasoningEffort,
	})
	if err != nil {
		return writeExecFormatUsageError(run.stdout, run.stderr, run.options.outputFormat, err.Error())
	}
	runID, err := streamjson.CreateRunID(run.deps.now())
	if err != nil {
		return writeAppError(run.stderr, "failed to create run id: "+err.Error(), exitCrash)
	}
	writer := execEventWriter{
		stdout:       run.stdout,
		stderr:       run.stderr,
		format:       run.options.outputFormat,
		runID:        runID,
		sessionID:    draftSession.SessionID,
		streamedText: &strings.Builder{},
	}
	writer.runStart(run.workspaceRoot, run.runMetadata, run.specPermissionMode)
	if writer.err != nil {
		return exitCrash
	}

	preparedSession := sessions.PreparedExec{
		Mode:    sessions.ModeNew,
		Session: draftSession,
		Store:   store,
	}
	sessionRecorder := execSessionRecorder{prepared: preparedSession}
	// Surface a best-effort session-recording failure once, on every exit path.
	defer sessionRecorder.warnIfRecordingFailed(run.stderr)
	sessionRecorder.append(sessions.EventMessage, map[string]any{
		"role":    "user",
		"content": run.prompt,
	})

	var draftInfo execSpecDraftInfo
	runCtx, stopSignals := signalContext()
	defer stopSignals()
	// The spec-draft path activates no plugins, so the plugin skip is omitted. Trust
	// keys on run.trustRoot (the original launch dir), not run.workspaceRoot, which
	// may be a --worktree path; this keeps a --use-spec --worktree run of a trusted
	// repo trusted. Emit at most one notice when project hooks or the project MCP
	// layer (registered earlier in runExec, carried in run.mcpSkip) were dropped.
	hookDispatcher, hookSkip := newHookDispatcher(run.workspaceRoot, run.trustRoot, execution.NewRunner(run.sandboxEngine))
	emitTrustNotice(run.stderr, hookSkip, run.mcpSkip)
	result, err := agent.Run(runCtx, run.prompt, run.provider, agent.Options{
		MaxTurns:      run.resolved.MaxTurns,
		ContextWindow: resolveAgentContextWindow(runCtx, run.modelRegistry, run.resolved.Provider),
		SessionID:     draftSession.SessionID,
		SessionTitle:  run.sessionTitle,
		ProviderName:  run.resolved.Provider.Name,
		Model:         run.resolved.Provider.Model,
		SupportsVision: func(modelID string) bool {
			return modelregistry.SupportsVision(run.modelRegistry, modelID)
		},
		ReasoningEffort: run.reasoningEffort,
		Profile:         run.profilePolicy,
		Cwd:             run.workspaceRoot,
		SystemPrompt:    specmode.DraftSystemPrompt,
		Images:          run.images,
		Registry:        run.registry,
		PermissionMode:  agent.PermissionModeSpecDraft,
		Autonomy:        "low",
		Sandbox:         run.sandboxEngine,
		FileTracker:     tools.NewFileTracker(),
		Hooks:           hookDispatcher,
		EnabledTools:    run.options.enabledTools,
		DisabledTools:   run.options.disabledTools,
		OnText:          writer.text,
		OnToolCall: func(call agent.ToolCall) {
			writer.toolCall(call, run.registry)
			sessionRecorder.append(sessions.EventToolCall, map[string]any{
				"id":        call.ID,
				"name":      call.Name,
				"arguments": call.Arguments,
			})
			if checkpoint, ok := sessionRecorder.captureCheckpoint(run.workspaceRoot, call); ok {
				writer.checkpoint(checkpoint)
			}
		},
		OnPermission: func(event agent.PermissionEvent) {
			writer.permission(event)
			sessionRecorder.append(sessionPermissionEventType(event), event)
		},
		OnToolResult: func(result agent.ToolResult) {
			writer.toolResult(result)
			if info, ok := execSpecDraftInfoFromToolResult(result); ok {
				draftInfo = info
			}
			payload := map[string]any{
				"toolCallId": result.ToolCallID,
				"name":       result.Name,
				"status":     string(result.Status),
				"output":     result.Output,
			}
			if len(result.Meta) > 0 {
				payload["meta"] = result.Meta
			}
			if result.Redacted {
				payload["redacted"] = true
			}
			if len(result.ChangedFiles) > 0 {
				payload["changedFiles"] = result.ChangedFiles
			}
			sessionRecorder.append(sessions.EventToolResult, payload)
		},
		OnUsage: func(u agent.Usage) {
			writer.usage(u)
			sessionRecorder.append(sessions.EventUsage, usage.EventUsagePayload(u))
		},
	})
	run.notifier.Notify(notify.Completion, notify.DefaultMessage(notify.Completion))
	if writer.err != nil {
		return exitCrash
	}
	if err != nil {
		if errors.Is(err, context.Canceled) || runCtx.Err() != nil {
			sessionRecorder.append(sessions.EventError, map[string]any{"message": "interrupted"})
			if run.options.outputFormat == execOutputStreamJSON {
				writer.errorEvent("interrupted", "run cancelled by signal", false)
				writer.runEnd("interrupted", exitInterrupted)
				if writer.err != nil {
					return exitCrash
				}
			} else {
				fmt.Fprintln(run.stderr, "Interrupted.")
			}
			return exitInterrupted
		}
		sessionRecorder.append(sessions.EventError, map[string]any{"message": err.Error()})
		return writeExecSpecDraftProviderError(&writer, run.options.outputFormat, err.Error())
	}
	if result.StopReason != agent.StopReasonSpecReviewRequired || draftInfo.SpecID == "" || draftInfo.SpecFilePath == "" {
		message := "spec draft ended without submit_spec"
		sessionRecorder.append(sessions.EventError, map[string]any{"message": message})
		return writeExecSpecDraftProviderError(&writer, run.options.outputFormat, message)
	}
	draftInfo.DraftSessionID = draftSession.SessionID
	if _, _, err := store.RecordSpec(draftSession.SessionID, sessions.RecordSpecInput{
		SpecID:             draftInfo.SpecID,
		SpecFilePath:       draftInfo.SpecFilePath,
		SpecStatus:         sessions.SpecStatusDraft,
		SpecDraftModelID:   run.resolved.Provider.Model,
		SpecDraftReasoning: run.reasoningEffort,
	}); err != nil {
		return writeAppError(run.stderr, err.Error(), exitCrash)
	}

	if emittedTerminal := writer.specReviewRequired(draftInfo); !emittedTerminal {
		writer.runEnd(string(agent.StopReasonSpecReviewRequired), exitSuccess)
	}
	if writer.err != nil {
		return exitCrash
	}
	return exitSuccess
}

func execSpecDraftInfoFromToolResult(result agent.ToolResult) (execSpecDraftInfo, bool) {
	if result.Name != specmode.SubmitToolName || result.Meta["control"] != specmode.ControlSpecReviewRequired {
		return execSpecDraftInfo{}, false
	}
	return execSpecDraftInfo{
		SpecID:       strings.TrimSpace(result.Meta["specId"]),
		SpecTitle:    strings.TrimSpace(result.Meta["specTitle"]),
		SpecFilePath: strings.TrimSpace(result.Meta["specFilePath"]),
		RelativePath: strings.TrimSpace(result.Meta["relativePath"]),
	}, true
}

func writeExecSpecDraftProviderError(writer *execEventWriter, format execOutputFormat, message string) int {
	if format == execOutputStreamJSON {
		writer.errorEvent("provider_error", message, false)
		writer.runEnd("error", exitProvider)
		if writer.err != nil {
			return exitCrash
		}
		return exitProvider
	}
	if format == execOutputJSON {
		writer.errorEvent("provider_error", message, false)
		writer.writeJSON(map[string]any{"type": "done", "exit_code": exitProvider})
		if writer.err != nil {
			return exitCrash
		}
		return exitProvider
	}
	writer.errorEvent("provider_error", message, false)
	if writer.err != nil {
		return exitCrash
	}
	return exitProvider
}

func (writer *execEventWriter) specReviewRequired(info execSpecDraftInfo) bool {
	summary := formatExecSpecDraftSummary(info)
	switch writer.format {
	case execOutputJSON:
		writer.writeJSON(map[string]any{
			"type":             string(agent.StopReasonSpecReviewRequired),
			"status":           string(sessions.SpecStatusDraft),
			"stop_reason":      string(agent.StopReasonSpecReviewRequired),
			"spec_id":          info.SpecID,
			"spec_title":       info.SpecTitle,
			"spec_file_path":   info.SpecFilePath,
			"relative_path":    info.RelativePath,
			"draft_session_id": info.DraftSessionID,
		})
		writer.writeJSON(map[string]any{"type": "done", "exit_code": exitSuccess})
		return true
	case execOutputStreamJSON:
		writer.writeStreamJSON(streamjson.Event{
			Type:  streamjson.EventFinal,
			RunID: writer.runID,
			Text:  summary,
			Meta: map[string]string{
				"status":         string(sessions.SpecStatusDraft),
				"stopReason":     string(agent.StopReasonSpecReviewRequired),
				"specId":         info.SpecID,
				"specTitle":      info.SpecTitle,
				"specFilePath":   info.SpecFilePath,
				"relativePath":   info.RelativePath,
				"draftSessionId": info.DraftSessionID,
			},
		})
		return false
	default:
		if writer.streamedText.Len() > 0 && !strings.HasSuffix(writer.streamedText.String(), "\n") {
			writer.writeStdout("\n")
		}
		writer.writeStdout(summary + "\n")
		return true
	}
}

func formatExecSpecDraftSummary(info execSpecDraftInfo) string {
	path := strings.TrimSpace(info.RelativePath)
	if path == "" {
		path = info.SpecFilePath
	}
	lines := []string{
		"Spec draft saved for review.",
		"  spec: " + redact(info.SpecID),
		"  path: " + redact(path),
		"  draft session: " + redact(info.DraftSessionID),
		"Next: zero spec show " + redact(info.SpecID) + "; zero spec approve " + redact(info.SpecID),
	}
	return strings.Join(lines, "\n")
}
