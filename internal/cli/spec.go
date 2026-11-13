package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/Twigpine/zero/internal/redaction"
	"github.com/Twigpine/zero/internal/sessions"
	"github.com/Twigpine/zero/internal/specmode"
)

type specCommandOptions struct {
	json            bool
	comment         string
	commentProvided bool
	reason          string
	reasonProvided  bool
}

type specCommandResult struct {
	Status                  string `json:"status"`
	SpecID                  string `json:"specId,omitempty"`
	SpecFilePath            string `json:"specFilePath,omitempty"`
	DraftSessionID          string `json:"draftSessionId,omitempty"`
	ImplementationSessionID string `json:"implementationSessionId,omitempty"`
	Message                 string `json:"message,omitempty"`
	Next                    string `json:"next,omitempty"`
}

func runSpec(args []string, stdout io.Writer, stderr io.Writer, deps appDeps) int {
	command, target, options, help, err := parseSpecArgs(args)
	if err != nil {
		return writeExecUsageError(stderr, err.Error())
	}
	if help {
		if err := writeSpecHelp(stdout); err != nil {
			return exitCrash
		}
		return exitSuccess
	}

	store := deps.newSessionStore()
	draft, err := resolveSpecReviewTarget(store, target)
	if err != nil {
		return writeExecUsageError(stderr, err.Error())
	}
	switch command {
	case "show":
		return runSpecShow(store, draft, options, stdout, stderr)
	case "approve":
		return runSpecApprove(store, draft, options, stdout, stderr)
	case "reject":
		return runSpecReject(store, draft, options, stdout, stderr)
	default:
		return writeExecUsageError(stderr, fmt.Sprintf("unknown spec command %q", command))
	}
}

func parseSpecArgs(args []string) (string, string, specCommandOptions, bool, error) {
	options := specCommandOptions{}
	if len(args) == 0 {
		return "", "", options, false, execUsageError{"spec command required. Use `zero spec show <spec>`."}
	}
	command := ""
	target := ""
	for index := 0; index < len(args); index++ {
		arg := args[index]
		switch {
		case arg == "-h" || arg == "--help" || arg == "help":
			return command, target, options, true, nil
		case arg == "--json":
			options.json = true
		case arg == "--comment":
			value, next, err := nextFlagValue(args, index, arg)
			if err != nil {
				return command, target, options, false, err
			}
			options.comment = value
			options.commentProvided = true
			index = next
		case strings.HasPrefix(arg, "--comment="):
			value, err := requiredInlineFlagValue(arg, "--comment")
			if err != nil {
				return command, target, options, false, err
			}
			options.comment = value
			options.commentProvided = true
		case arg == "--reason":
			value, next, err := nextFlagValue(args, index, arg)
			if err != nil {
				return command, target, options, false, err
			}
			options.reason = value
			options.reasonProvided = true
			index = next
		case strings.HasPrefix(arg, "--reason="):
			value, err := requiredInlineFlagValue(arg, "--reason")
			if err != nil {
				return command, target, options, false, err
			}
			options.reason = value
			options.reasonProvided = true
		case strings.HasPrefix(arg, "-"):
			return command, target, options, false, execUsageError{fmt.Sprintf("unknown spec flag %q", arg)}
		default:
			if command == "" {
				command = arg
				continue
			}
			if target == "" {
				target = arg
				continue
			}
			return command, target, options, false, execUsageError{fmt.Sprintf("unexpected spec argument %q", arg)}
		}
	}
	command = strings.TrimSpace(command)
	switch command {
	case "show", "approve", "reject":
	default:
		return command, target, options, false, execUsageError{fmt.Sprintf("unknown spec command %q", command)}
	}
	if strings.TrimSpace(target) == "" {
		return command, target, options, false, execUsageError{fmt.Sprintf("zero spec %s requires a spec id or draft session id", command)}
	}
	if options.commentProvided && command != "approve" {
		return command, target, options, false, execUsageError{"--comment is only valid for zero spec approve"}
	}
	if options.reasonProvided && command != "reject" {
		return command, target, options, false, execUsageError{"--reason is only valid for zero spec reject"}
	}
	return command, strings.TrimSpace(target), options, false, nil
}

func resolveSpecReviewTarget(store *sessions.Store, target string) (sessions.Metadata, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return sessions.Metadata{}, fmt.Errorf("spec id or draft session id is required")
	}
	if sessions.ValidSessionID(target) {
		session, err := store.Get(target)
		if err != nil {
			return sessions.Metadata{}, err
		}
		if session != nil {
			if session.SpecID == "" || session.SpecFilePath == "" {
				return sessions.Metadata{}, fmt.Errorf("zero session has no recorded spec: %s", redact(target))
			}
			return *session, nil
		}
	}
	items, err := store.List()
	if err != nil {
		return sessions.Metadata{}, err
	}
	matches := []sessions.Metadata{}
	draftMatches := []sessions.Metadata{}
	for _, item := range items {
		if item.SpecID == target && item.SpecFilePath != "" {
			matches = append(matches, item)
			if item.SessionKind == sessions.SessionKindSpecDraft {
				draftMatches = append(draftMatches, item)
			}
		}
	}
	if len(draftMatches) == 1 {
		return draftMatches[0], nil
	}
	if len(draftMatches) > 1 {
		return sessions.Metadata{}, fmt.Errorf("zero spec id is ambiguous: %s; use the draft session id", redact(target))
	}
	if len(matches) == 0 {
		return sessions.Metadata{}, fmt.Errorf("zero spec not found: %s", redact(target))
	}
	if len(matches) > 1 {
		return sessions.Metadata{}, fmt.Errorf("zero spec id is ambiguous: %s; use the draft session id", redact(target))
	}
	return matches[0], nil
}

func runSpecShow(store *sessions.Store, draft sessions.Metadata, options specCommandOptions, stdout io.Writer, stderr io.Writer) int {
	body, path, err := specmode.LoadSpecFile(draft.Cwd, draft.SpecFilePath)
	if err != nil {
		return writeAppError(stderr, err.Error(), exitCrash)
	}
	if options.json {
		payload := map[string]any{
			"specId":         draft.SpecID,
			"specFilePath":   path,
			"draftSessionId": draft.SessionID,
			"specStatus":     draft.SpecStatus,
			"content":        body,
		}
		if err := writePrettyJSON(stdout, redaction.RedactValue(payload, redaction.Options{})); err != nil {
			return exitCrash
		}
		return exitSuccess
	}
	if _, err := fmt.Fprintln(stdout, body); err != nil {
		return exitCrash
	}
	return exitSuccess
}

func runSpecApprove(store *sessions.Store, draft sessions.Metadata, options specCommandOptions, stdout io.Writer, stderr io.Writer) int {
	if draft.SessionKind != sessions.SessionKindSpecDraft {
		return writeExecUsageError(stderr, "zero spec approve requires a spec-draft session")
	}
	if draft.SpecStatus == sessions.SpecStatusRejected {
		return writeExecUsageError(stderr, "zero spec approve cannot approve a rejected spec")
	}
	if draft.SpecStatus == sessions.SpecStatusApproved && draft.SpecImplSessionID != "" {
		return writeSpecResult(stdout, options, specCommandResult{
			Status:                  string(sessions.SpecStatusApproved),
			SpecID:                  draft.SpecID,
			SpecFilePath:            draft.SpecFilePath,
			DraftSessionID:          draft.SessionID,
			ImplementationSessionID: draft.SpecImplSessionID,
			Message:                 "Spec already approved.",
			Next:                    "zero exec --resume " + draft.SpecImplSessionID + ` "Start implementation"`,
		})
	}
	body, path, err := specmode.LoadSpecFile(draft.Cwd, draft.SpecFilePath)
	if err != nil {
		return writeAppError(stderr, err.Error(), exitCrash)
	}
	prompt := specmode.ImplementationPrompt(body, path, draft.SessionID, options.comment)
	impl, _, err := store.EnsureSpecImplementation(sessions.EnsureSpecImplementationInput{
		Title:               specImplementationTitle(draft),
		Cwd:                 draft.Cwd,
		ModelID:             draft.ModelID,
		Provider:            draft.Provider,
		RootSessionID:       firstNonEmptyString(draft.RootSessionID, draft.SessionID),
		SpecID:              draft.SpecID,
		SpecFilePath:        path,
		SpecDraftModelID:    draft.SpecDraftModelID,
		SpecDraftReasoning:  draft.SpecDraftReasoning,
		SpecUserComment:     options.comment,
		SpecSourceSessionID: draft.SessionID,
		Prompt:              prompt,
	})
	if err != nil {
		return writeAppError(stderr, err.Error(), exitCrash)
	}
	updated, _, err := store.RecordSpec(draft.SessionID, sessions.RecordSpecInput{
		SpecID:              draft.SpecID,
		SpecFilePath:        path,
		SpecStatus:          sessions.SpecStatusApproved,
		SpecDraftModelID:    draft.SpecDraftModelID,
		SpecDraftReasoning:  draft.SpecDraftReasoning,
		SpecUserComment:     options.comment,
		SpecImplSessionID:   impl.SessionID,
		SpecSourceSessionID: draft.SessionID,
	})
	if err != nil {
		return writeAppError(stderr, err.Error(), exitCrash)
	}
	return writeSpecResult(stdout, options, specCommandResult{
		Status:                  string(updated.SpecStatus),
		SpecID:                  updated.SpecID,
		SpecFilePath:            updated.SpecFilePath,
		DraftSessionID:          updated.SessionID,
		ImplementationSessionID: impl.SessionID,
		Message:                 "Spec approved.",
		Next:                    "zero exec --resume " + impl.SessionID + ` "Start implementation"`,
	})
}

func runSpecReject(store *sessions.Store, draft sessions.Metadata, options specCommandOptions, stdout io.Writer, stderr io.Writer) int {
	if draft.SessionKind != sessions.SessionKindSpecDraft {
		return writeExecUsageError(stderr, "zero spec reject requires a spec-draft session")
	}
	if draft.SpecStatus == sessions.SpecStatusApproved && draft.SpecImplSessionID != "" {
		return writeExecUsageError(stderr, "zero spec reject cannot reject an approved spec with an implementation session")
	}
	updated, _, err := store.RecordSpec(draft.SessionID, sessions.RecordSpecInput{
		SpecID:              draft.SpecID,
		SpecFilePath:        draft.SpecFilePath,
		SpecStatus:          sessions.SpecStatusRejected,
		SpecDraftModelID:    draft.SpecDraftModelID,
		SpecDraftReasoning:  draft.SpecDraftReasoning,
		SpecRejectReason:    options.reason,
		SpecSourceSessionID: draft.SessionID,
	})
	if err != nil {
		return writeAppError(stderr, err.Error(), exitCrash)
	}
	return writeSpecResult(stdout, options, specCommandResult{
		Status:         string(updated.SpecStatus),
		SpecID:         updated.SpecID,
		SpecFilePath:   updated.SpecFilePath,
		DraftSessionID: updated.SessionID,
		Message:        "Spec rejected.",
		Next:           "Run zero exec --use-spec again with a revised task.",
	})
}

func writeSpecResult(stdout io.Writer, options specCommandOptions, result specCommandResult) int {
	if options.json {
		if err := writePrettyJSON(stdout, redaction.RedactValue(result, redaction.Options{})); err != nil {
			return exitCrash
		}
		return exitSuccess
	}
	lines := []string{result.Message}
	if result.SpecID != "" {
		lines = append(lines, "  spec: "+redact(result.SpecID))
	}
	if result.SpecFilePath != "" {
		lines = append(lines, "  path: "+redact(result.SpecFilePath))
	}
	if result.DraftSessionID != "" {
		lines = append(lines, "  draft session: "+redact(result.DraftSessionID))
	}
	if result.ImplementationSessionID != "" {
		lines = append(lines, "  implementation session: "+redact(result.ImplementationSessionID))
	}
	if result.Next != "" {
		lines = append(lines, "Next: "+redact(result.Next))
	}
	if _, err := fmt.Fprintln(stdout, strings.Join(lines, "\n")); err != nil {
		return exitCrash
	}
	return exitSuccess
}

func specImplementationTitle(draft sessions.Metadata) string {
	title := strings.TrimSpace(draft.Title)
	if title == "" {
		title = strings.TrimSpace(draft.SpecID)
	}
	if title == "" {
		return "Spec implementation"
	}
	return title + " implementation"
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func writeSpecHelp(w io.Writer) error {
	_, err := fmt.Fprint(w, `Usage:
  zero spec show <spec-id|draft-session-id> [--json]
  zero spec approve <spec-id|draft-session-id> [--comment <text>] [--json]
  zero spec reject <spec-id|draft-session-id> [--reason <text>] [--json]

Commands:
  show      Print the saved draft spec
  approve   Mark a draft approved and create a spec implementation session
  reject    Mark a draft rejected

Flags:
      --json            Print JSON output
      --comment <text>  Approval note to include in the implementation prompt
      --reason <text>   Rejection reason
  -h, --help            Show this help
`)
	return err
}
