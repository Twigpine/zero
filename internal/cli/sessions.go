package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/Gitlawb/zero/internal/redaction"
	"github.com/Gitlawb/zero/internal/sessions"
	"github.com/Gitlawb/zero/internal/zerocommands"
)

type sessionCommandOptions struct {
	json           bool
	kind           sessions.SessionKind
	sequence       int
	eventID        string
	excludeTarget  bool
	preserveLast   int
	maxPromptChars int
	olderThan      string
	dryRun         bool
}

func runSessions(args []string, stdout io.Writer, stderr io.Writer, deps appDeps) int {
	command, remaining, options, help, err := parseSessionsArgs(args)
	if err != nil {
		return writeExecUsageError(stderr, err.Error())
	}
	if help {
		if err := writeSessionsHelp(stdout); err != nil {
			return exitCrash
		}
		return exitSuccess
	}
	if err := validateSessionCommandFlags(command, options); err != nil {
		return writeExecUsageError(stderr, err.Error())
	}

	store := deps.newSessionStore()
	switch command {
	case "list":
		if len(remaining) != 0 {
			return writeExecUsageError(stderr, "sessions list does not accept positional arguments")
		}
		return runSessionsList(store, options, stdout, stderr)
	case "children":
		if len(remaining) != 1 {
			return writeExecUsageError(stderr, "sessions children requires a session id")
		}
		return runSessionsChildren(store, remaining[0], options, stdout, stderr)
	case "lineage":
		if len(remaining) != 1 {
			return writeExecUsageError(stderr, "sessions lineage requires a session id")
		}
		return runSessionsLineage(store, remaining[0], options, stdout, stderr)
	case "tree":
		if len(remaining) != 1 {
			return writeExecUsageError(stderr, "sessions tree requires a session id")
		}
		return runSessionsTree(store, remaining[0], options, stdout, stderr)
	case "rewind-plan":
		if len(remaining) != 1 {
			return writeExecUsageError(stderr, "sessions rewind-plan requires a session id")
		}
		return runSessionsRewindPlan(store, remaining[0], options, stdout, stderr)
	case "rewind":
		if len(remaining) != 1 {
			return writeExecUsageError(stderr, "sessions rewind requires a session id")
		}
		return runSessionsRewind(store, remaining[0], options, stdout, stderr)
	case "compact-plan":
		if len(remaining) != 1 {
			return writeExecUsageError(stderr, "sessions compact-plan requires a session id")
		}
		return runSessionsCompactPlan(store, remaining[0], options, stdout, stderr)
	case "prune":
		if len(remaining) != 0 {
			return writeExecUsageError(stderr, "sessions prune does not accept positional arguments")
		}
		return runSessionsPrune(store, options, stdout, stderr, deps)
	default:
		return writeExecUsageError(stderr, fmt.Sprintf("unknown sessions command %q", command))
	}
}

func parseSessionsArgs(args []string) (string, []string, sessionCommandOptions, bool, error) {
	options := sessionCommandOptions{}
	command := "list"
	commandExplicit := false
	remaining := []string{}
	for index := 0; index < len(args); index++ {
		arg := args[index]
		switch arg {
		case "-h", "--help", "help":
			return command, remaining, options, true, nil
		case "--json":
			options.json = true
		case "--exclude-target":
			options.excludeTarget = true
		case "--dry-run":
			options.dryRun = true
		case "--kind":
			value, next, err := nextFlagValue(args, index, arg)
			if err != nil {
				return command, remaining, options, false, err
			}
			kind, err := parseSessionKindFlag(value)
			if err != nil {
				return command, remaining, options, false, err
			}
			options.kind = kind
			index = next
		default:
			switch {
			case strings.HasPrefix(arg, "--kind="):
				kind, err := parseSessionKindFlag(strings.TrimPrefix(arg, "--kind="))
				if err != nil {
					return command, remaining, options, false, err
				}
				options.kind = kind
				continue
			case arg == "--sequence":
				value, next, err := nextFlagValue(args, index, arg)
				if err != nil {
					return command, remaining, options, false, err
				}
				sequence, err := parsePositiveIntFlag(arg, value)
				if err != nil {
					return command, remaining, options, false, err
				}
				options.sequence = sequence
				index = next
				continue
			case strings.HasPrefix(arg, "--sequence="):
				sequence, err := parsePositiveIntFlag("--sequence", strings.TrimSpace(strings.TrimPrefix(arg, "--sequence=")))
				if err != nil {
					return command, remaining, options, false, err
				}
				options.sequence = sequence
				continue
			case arg == "--event":
				value, next, err := nextFlagValue(args, index, arg)
				if err != nil {
					return command, remaining, options, false, err
				}
				eventID, err := parseNonEmptySessionsFlag("--event", value)
				if err != nil {
					return command, remaining, options, false, err
				}
				options.eventID = eventID
				index = next
				continue
			case strings.HasPrefix(arg, "--event="):
				eventID, err := parseNonEmptySessionsFlag("--event", strings.TrimPrefix(arg, "--event="))
				if err != nil {
					return command, remaining, options, false, err
				}
				options.eventID = eventID
				continue
			case arg == "--preserve-last":
				value, next, err := nextFlagValue(args, index, arg)
				if err != nil {
					return command, remaining, options, false, err
				}
				preserveLast, err := parsePositiveIntFlag(arg, value)
				if err != nil {
					return command, remaining, options, false, err
				}
				options.preserveLast = preserveLast
				index = next
				continue
			case strings.HasPrefix(arg, "--preserve-last="):
				preserveLast, err := parsePositiveIntFlag("--preserve-last", strings.TrimSpace(strings.TrimPrefix(arg, "--preserve-last=")))
				if err != nil {
					return command, remaining, options, false, err
				}
				options.preserveLast = preserveLast
				continue
			case arg == "--older-than":
				value, next, err := nextFlagValue(args, index, arg)
				if err != nil {
					return command, remaining, options, false, err
				}
				options.olderThan = value
				index = next
				continue
			case strings.HasPrefix(arg, "--older-than="):
				value, err := parseNonEmptySessionsFlag("--older-than", strings.TrimPrefix(arg, "--older-than="))
				if err != nil {
					return command, remaining, options, false, err
				}
				options.olderThan = value
				continue
			case arg == "--max-prompt-chars":
				value, next, err := nextFlagValue(args, index, arg)
				if err != nil {
					return command, remaining, options, false, err
				}
				maxPromptChars, err := parsePositiveIntFlag(arg, value)
				if err != nil {
					return command, remaining, options, false, err
				}
				options.maxPromptChars = maxPromptChars
				index = next
				continue
			case strings.HasPrefix(arg, "--max-prompt-chars="):
				maxPromptChars, err := parsePositiveIntFlag("--max-prompt-chars", strings.TrimSpace(strings.TrimPrefix(arg, "--max-prompt-chars=")))
				if err != nil {
					return command, remaining, options, false, err
				}
				options.maxPromptChars = maxPromptChars
				continue
			}
			if strings.HasPrefix(arg, "-") {
				return command, remaining, options, false, execUsageError{fmt.Sprintf("unknown sessions flag %q", arg)}
			}
			if !commandExplicit && len(remaining) == 0 && isSessionsCommand(arg) {
				command = arg
				commandExplicit = true
				continue
			}
			if !commandExplicit && len(remaining) == 0 {
				return command, remaining, options, false, execUsageError{fmt.Sprintf("unknown sessions command %q", arg)}
			}
			remaining = append(remaining, arg)
		}
	}
	return command, remaining, options, false, nil
}

func parseNonEmptySessionsFlag(flag string, value string) (string, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "", execUsageError{flag + " requires a value"}
	}
	return trimmed, nil
}

func parseSessionKindFlag(value string) (sessions.SessionKind, error) {
	kind := sessions.SessionKind(strings.ToLower(strings.TrimSpace(value)))
	switch kind {
	case sessions.SessionKindFork, sessions.SessionKindChild, sessions.SessionKindSide, sessions.SessionKindSpecDraft, sessions.SessionKindSpecImpl:
		return kind, nil
	default:
		return "", execUsageError{fmt.Sprintf("invalid --kind %q. Expected fork, child, side, spec-draft, or spec-impl.", value)}
	}
}

func isSessionsCommand(command string) bool {
	switch command {
	case "list", "children", "lineage", "tree", "rewind-plan", "rewind", "compact-plan", "prune":
		return true
	default:
		return false
	}
}

func validateSessionCommandFlags(command string, options sessionCommandOptions) error {
	if options.kind != "" && command != "list" {
		return execUsageError{"--kind is only valid for sessions list"}
	}
	hasRewindFlag := options.sequence > 0 || strings.TrimSpace(options.eventID) != "" || options.excludeTarget
	if hasRewindFlag && command != "rewind-plan" && command != "rewind" {
		return execUsageError{"--sequence, --event, and --exclude-target are only valid for sessions rewind-plan and rewind"}
	}
	hasCompactionFlag := options.preserveLast > 0 || options.maxPromptChars > 0
	if hasCompactionFlag && command != "compact-plan" {
		return execUsageError{"--preserve-last and --max-prompt-chars are only valid for sessions compact-plan"}
	}
	if (options.olderThan != "" || options.dryRun) && command != "prune" {
		return execUsageError{"--older-than and --dry-run are only valid for sessions prune"}
	}
	return nil
}

func runSessionsList(store *sessions.Store, options sessionCommandOptions, stdout io.Writer, stderr io.Writer) int {
	items, err := store.List()
	if err != nil {
		return writeAppError(stderr, err.Error(), exitCrash)
	}
	items = filterSessionsByKind(items, options.kind)
	if options.json {
		if err := writePrettyJSON(stdout, redaction.RedactValue(zerocommands.SessionSnapshots(items), redaction.Options{})); err != nil {
			return exitCrash
		}
		return exitSuccess
	}
	if _, err := fmt.Fprintln(stdout, formatSessionSnapshotsList(zerocommands.SessionSnapshots(items))); err != nil {
		return exitCrash
	}
	return exitSuccess
}

func filterSessionsByKind(items []sessions.Metadata, kind sessions.SessionKind) []sessions.Metadata {
	if kind == "" {
		return items
	}
	filtered := make([]sessions.Metadata, 0, len(items))
	for _, item := range items {
		if item.SessionKind == kind {
			filtered = append(filtered, item)
		}
	}
	return filtered
}

func runSessionsChildren(store *sessions.Store, sessionID string, options sessionCommandOptions, stdout io.Writer, stderr io.Writer) int {
	items, err := store.ListChildren(sessionID)
	if err != nil {
		return writeSessionCommandError(stderr, err)
	}
	if options.json {
		if err := writePrettyJSON(stdout, redaction.RedactValue(zerocommands.SessionSnapshots(items), redaction.Options{})); err != nil {
			return exitCrash
		}
		return exitSuccess
	}
	if _, err := fmt.Fprintln(stdout, formatSessionSnapshotsList(zerocommands.SessionSnapshots(items))); err != nil {
		return exitCrash
	}
	return exitSuccess
}

func runSessionsLineage(store *sessions.Store, sessionID string, options sessionCommandOptions, stdout io.Writer, stderr io.Writer) int {
	lineage, err := store.Lineage(sessionID)
	if err != nil {
		return writeSessionCommandError(stderr, err)
	}
	if options.json {
		if err := writePrettyJSON(stdout, redaction.RedactValue(zerocommands.SessionSnapshots(lineage), redaction.Options{})); err != nil {
			return exitCrash
		}
		return exitSuccess
	}
	ids := make([]string, 0, len(lineage))
	for _, session := range zerocommands.SessionSnapshots(lineage) {
		ids = append(ids, redact(session.SessionID))
	}
	if _, err := fmt.Fprintln(stdout, strings.Join(ids, " -> ")); err != nil {
		return exitCrash
	}
	return exitSuccess
}

func runSessionsTree(store *sessions.Store, sessionID string, options sessionCommandOptions, stdout io.Writer, stderr io.Writer) int {
	tree, err := store.Tree(sessionID)
	if err != nil {
		return writeSessionCommandError(stderr, err)
	}
	if options.json {
		if err := writePrettyJSON(stdout, redaction.RedactValue(zerocommands.SessionTreeSnapshotFromNode(tree), redaction.Options{})); err != nil {
			return exitCrash
		}
		return exitSuccess
	}
	if _, err := fmt.Fprint(stdout, formatSessionSnapshotTree(zerocommands.SessionTreeSnapshotFromNode(tree))); err != nil {
		return exitCrash
	}
	return exitSuccess
}

func runSessionsRewindPlan(store *sessions.Store, sessionID string, options sessionCommandOptions, stdout io.Writer, stderr io.Writer) int {
	plan, err := store.PlanRewind(sessionID, sessions.RewindOptions{
		TargetSequence: options.sequence,
		TargetEventID:  options.eventID,
		KeepTarget:     !options.excludeTarget,
	})
	if err != nil {
		return writeSessionCommandError(stderr, err)
	}
	if options.json {
		if err := writePrettyJSON(stdout, redaction.RedactValue(plan, redaction.Options{})); err != nil {
			return exitCrash
		}
		return exitSuccess
	}
	if _, err := fmt.Fprintln(stdout, formatRewindPlan(plan)); err != nil {
		return exitCrash
	}
	return exitSuccess
}

func runSessionsRewind(store *sessions.Store, sessionID string, options sessionCommandOptions, stdout io.Writer, stderr io.Writer) int {
	plan, err := store.PlanRewind(sessionID, sessions.RewindOptions{
		TargetSequence: options.sequence,
		TargetEventID:  options.eventID,
		KeepTarget:     !options.excludeTarget,
	})
	if err != nil {
		return writeSessionCommandError(stderr, err)
	}
	session, err := store.Get(sessionID)
	if err != nil {
		return writeSessionCommandError(stderr, err)
	}
	if session == nil {
		return writeExecUsageError(stderr, "Zero session not found: "+redact(sessionID))
	}
	workspaceRoot := strings.TrimSpace(session.Cwd)
	if workspaceRoot == "" {
		return writeExecUsageError(stderr, "session has no recorded workspace (cwd); cannot restore files")
	}
	// Honor --exclude-target: ApplyRewind keeps events THROUGH the given sequence,
	// so when the target event itself must be dropped (KeepTarget=false), apply
	// through the sequence BEFORE it — matching what rewind-plan reports.
	keepThrough := plan.TargetSequence
	if !plan.KeepTarget {
		keepThrough = plan.TargetSequence - 1
	}
	report, err := store.ApplyRewind(sessionID, workspaceRoot, keepThrough)
	if err != nil {
		return writeSessionCommandError(stderr, err)
	}
	if options.json {
		if err := writePrettyJSON(stdout, redaction.RedactValue(report, redaction.Options{})); err != nil {
			return exitCrash
		}
		return exitSuccess
	}
	if _, err := fmt.Fprintf(stdout, "Rewound %s to sequence %d: %d file(s) restored, %d deleted, %d skipped.\n",
		redact(sessionID), keepThrough, report.FilesRestored, report.FilesDeleted, len(report.Skipped)); err != nil {
		return exitCrash
	}
	return exitSuccess
}

func runSessionsCompactPlan(store *sessions.Store, sessionID string, options sessionCommandOptions, stdout io.Writer, stderr io.Writer) int {
	plan, err := store.PlanCompaction(sessionID, sessions.CompactionOptions{
		PreserveLast:   options.preserveLast,
		MaxPromptChars: options.maxPromptChars,
	})
	if err != nil {
		return writeSessionCommandError(stderr, err)
	}
	if options.json {
		if err := writePrettyJSON(stdout, redaction.RedactValue(plan, redaction.Options{})); err != nil {
			return exitCrash
		}
		return exitSuccess
	}
	if _, err := fmt.Fprintln(stdout, formatCompactionPlan(plan)); err != nil {
		return exitCrash
	}
	return exitSuccess
}

func writeSessionCommandError(stderr io.Writer, err error) int {
	message := strings.TrimPrefix(err.Error(), "zero session")
	if message != err.Error() {
		message = "Zero session" + message
	}
	if strings.Contains(message, "not found") || strings.Contains(message, "invalid zero session id") {
		return writeExecUsageError(stderr, message)
	}
	return writeAppError(stderr, message, exitCrash)
}

func formatRewindPlan(plan sessions.RewindPlan) string {
	return strings.Join([]string{
		"Zero session rewind plan",
		"session: " + redact(plan.SessionID),
		"target: " + redact(plan.TargetEventID),
		fmt.Sprintf("kept: %d", plan.KeptCount),
		fmt.Sprintf("dropped: %d", plan.DroppedCount),
	}, "\n")
}

func formatCompactionPlan(plan sessions.CompactionPlan) string {
	lines := []string{
		"Zero session compaction plan",
		"session: " + redact(plan.SessionID),
		fmt.Sprintf("compactable: %d", plan.CompactableCount),
		fmt.Sprintf("preserved: %d", plan.PreservedCount),
		fmt.Sprintf("prompt chars: %d", plan.PromptChars),
	}
	if plan.Truncated {
		lines = append(lines, "truncated: true")
	}
	return strings.Join(lines, "\n")
}

func formatSessionSnapshotsList(items []zerocommands.SessionSnapshot) string {
	if len(items) == 0 {
		return "No Zero sessions found."
	}
	lines := []string{fmt.Sprintf("Zero sessions (%d):", len(items))}
	for _, session := range items {
		lines = append(lines, "  "+formatSessionSnapshotLine(session))
	}
	return strings.Join(lines, "\n")
}

func formatSessionSnapshotTree(node zerocommands.SessionTreeSnapshot) string {
	lines := []string{"Zero session tree:"}
	appendSessionSnapshotTree(&lines, node, "")
	return strings.Join(lines, "\n") + "\n"
}

func appendSessionSnapshotTree(lines *[]string, node zerocommands.SessionTreeSnapshot, prefix string) {
	*lines = append(*lines, prefix+formatSessionSnapshotLine(node.Session))
	for _, child := range node.Children {
		appendSessionSnapshotTree(lines, child, prefix+"  ")
	}
}

func formatSessionSnapshotLine(session zerocommands.SessionSnapshot) string {
	parts := []string{"- " + redact(session.SessionID)}
	if session.Kind != "" {
		parts = append(parts, "["+redact(session.Kind)+"]")
	}
	if session.Title != "" {
		parts = append(parts, redact(session.Title))
	}
	details := []string{}
	if session.AgentName != "" {
		details = append(details, "agent="+redact(session.AgentName))
	}
	if session.TaskID != "" {
		details = append(details, "task="+redact(session.TaskID))
	}
	if session.SpecStatus != "" {
		details = append(details, "spec="+redact(session.SpecStatus))
	}
	if session.SpecID != "" {
		details = append(details, "spec_id="+redact(session.SpecID))
	}
	if session.Tag != "" {
		details = append(details, "tag="+redact(session.Tag))
	}
	if session.Depth > 0 {
		details = append(details, fmt.Sprintf("depth=%d", session.Depth))
	}
	if session.ParentSessionID != "" {
		details = append(details, "parent="+redact(session.ParentSessionID))
	}
	if session.ModelID != "" {
		details = append(details, "model="+redact(session.ModelID))
	}
	if len(details) > 0 {
		parts = append(parts, "("+strings.Join(details, ", ")+")")
	}
	return strings.Join(parts, " ")
}

func redact(value string) string {
	return redaction.RedactString(value, redaction.Options{})
}

func writeSessionsHelp(w io.Writer) error {
	_, err := fmt.Fprint(w, `Usage:
  zero sessions <command> [flags]

Commands:
  list                  List local Zero sessions
  children <id>         List direct child sessions for a parent session
  lineage <id>          Print the root-to-session lineage path
  tree <id>             Print a child-session tree
  rewind-plan <id>      Preview events kept and dropped by a rewind
  rewind <id>           Restore workspace files and truncate the log to a checkpoint
  compact-plan <id>     Preview events compacted and preserved by compaction
  prune                 Remove sessions last updated before a cutoff

Flags:
      --json            Print JSON output
      --kind <kind>     Filter list by fork, child, side, spec-draft, or spec-impl
      --sequence <n>    Rewind target sequence (rewind-plan, rewind)
      --event <id>      Rewind target event id (rewind-plan, rewind)
      --exclude-target  Drop the target event (rewind-plan, rewind)
      --preserve-last <n> Keep recent events in compact-plan
      --max-prompt-chars <n> Limit compact-plan summary prompt
      --older-than <age>  Prune cutoff, as days (30d) or a duration (720h); at least 1d.
                        Without it, prune uses sessions.retentionDays from your user config.
      --dry-run         List what prune would remove, and why it keeps the rest
  -h, --help            Show this help

prune only runs when you run it. It never removes a session another Zero process
has open, or an ancestor of a session it keeps, and --dry-run shows why.
`)
	return err
}
