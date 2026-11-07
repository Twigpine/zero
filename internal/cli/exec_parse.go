package cli

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/Twigpine/zero/internal/sessions"
	"github.com/Twigpine/zero/internal/specialist"
)

func parseExecArgs(args []string) (execOptions, bool, error) {
	options := execOptions{inputFormat: execInputText, outputFormat: execOutputText, autonomy: "low"}
	if len(args) == 0 {
		return options, false, execUsageError{"Prompt required. Use `zero exec \"prompt\"` or `zero exec --file prompt.txt`."}
	}

	for index := 0; index < len(args); index++ {
		arg := args[index]
		switch {
		case arg == "-h" || arg == "--help" || arg == "help":
			return options, true, nil
		case arg == "--permission-mode":
			value, next, err := nextFlagValue(args, index, arg)
			if err != nil {
				return options, false, err
			}
			options.permissionMode = strings.TrimSpace(value)
			index = next
		case strings.HasPrefix(arg, "--permission-mode="):
			value, err := requiredInlineFlagValue(arg, "--permission-mode")
			if err != nil {
				return options, false, err
			}
			options.permissionMode = value
		case arg == "--skip-permissions-unsafe":
			options.skipPermissionsUnsafe = true
		case arg == "--list-tools":
			options.listTools = true
		case arg == "--allow-escalation":
			options.allowEscalation = true
		case arg == "--self-correct":
			options.selfCorrect = true
		case arg == "--no-notify":
			options.noNotify = true
		case arg == "--no-completion-gate":
			options.noCompletionGate = true
		case arg == "--trace":
			value, next, err := nextFlagValue(args, index, arg)
			if err != nil {
				return options, false, err
			}
			options.tracePath = value
			index = next
		case strings.HasPrefix(arg, "--trace="):
			options.tracePath = strings.TrimSpace(strings.TrimPrefix(arg, "--trace="))
		case arg == "--notify":
			value, next, err := nextFlagValue(args, index, arg)
			if err != nil {
				return options, false, err
			}
			options.notifyMode = strings.TrimSpace(value)
			index = next
		case strings.HasPrefix(arg, "--notify="):
			value, err := requiredInlineFlagValue(arg, "--notify")
			if err != nil {
				return options, false, err
			}
			options.notifyMode = value
		case arg == "--auto":
			value, next, err := nextFlagValue(args, index, arg)
			if err != nil {
				return options, false, err
			}
			options.autonomy = value
			index = next
		case strings.HasPrefix(arg, "--auto="):
			options.autonomy = strings.TrimSpace(strings.TrimPrefix(arg, "--auto="))
		case arg == "--enabled-tools":
			value, next, err := nextFlagValue(args, index, arg)
			if err != nil {
				return options, false, err
			}
			options.enabledTools = parseToolList(value)
			index = next
		case strings.HasPrefix(arg, "--enabled-tools="):
			options.enabledTools = parseToolList(strings.TrimSpace(strings.TrimPrefix(arg, "--enabled-tools=")))
		case arg == "--disabled-tools":
			value, next, err := nextFlagValue(args, index, arg)
			if err != nil {
				return options, false, err
			}
			options.disabledTools = parseToolList(value)
			index = next
		case strings.HasPrefix(arg, "--disabled-tools="):
			options.disabledTools = parseToolList(strings.TrimSpace(strings.TrimPrefix(arg, "--disabled-tools=")))
		case arg == "-f" || arg == "--file":
			value, next, err := nextFlagValue(args, index, arg)
			if err != nil {
				return options, false, err
			}
			options.file = value
			index = next
		case strings.HasPrefix(arg, "--file="):
			options.file = strings.TrimSpace(strings.TrimPrefix(arg, "--file="))
		case arg == "--image":
			value, next, err := nextFlagValue(args, index, arg)
			if err != nil {
				return options, false, err
			}
			options.imagePaths = append(options.imagePaths, value)
			index = next
		case strings.HasPrefix(arg, "--image="):
			value, err := requiredInlineFlagValue(arg, "--image")
			if err != nil {
				return options, false, err
			}
			options.imagePaths = append(options.imagePaths, value)
		case arg == "--add-dir":
			value, next, err := nextFlagValue(args, index, arg)
			if err != nil {
				return options, false, err
			}
			options.addDirs = append(options.addDirs, value)
			index = next
		case strings.HasPrefix(arg, "--add-dir="):
			value, err := requiredInlineFlagValue(arg, "--add-dir")
			if err != nil {
				return options, false, err
			}
			options.addDirs = append(options.addDirs, value)
		case arg == "--mode":
			value, next, err := nextFlagValue(args, index, arg)
			if err != nil {
				return options, false, err
			}
			options.mode = strings.TrimSpace(value)
			index = next
		case strings.HasPrefix(arg, "--mode="):
			value, err := requiredInlineFlagValue(arg, "--mode")
			if err != nil {
				return options, false, err
			}
			options.mode = value
		case arg == "-m" || arg == "--model":
			value, next, err := nextFlagValue(args, index, arg)
			if err != nil {
				return options, false, err
			}
			options.model = value
			index = next
		case strings.HasPrefix(arg, "--model="):
			options.model = strings.TrimSpace(strings.TrimPrefix(arg, "--model="))
		case arg == "--profile":
			value, next, err := nextFlagValue(args, index, arg)
			if err != nil {
				return options, false, err
			}
			options.modelProfile = strings.TrimSpace(value)
			index = next
		case strings.HasPrefix(arg, "--profile="):
			options.modelProfile = strings.TrimSpace(strings.TrimPrefix(arg, "--profile="))
		case arg == "--exec-profile":
			value, next, err := nextFlagValue(args, index, arg)
			if err != nil {
				return options, false, err
			}
			options.execProfile = strings.TrimSpace(value)
			index = next
		case strings.HasPrefix(arg, "--exec-profile="):
			value, err := requiredInlineFlagValue(arg, "--exec-profile")
			if err != nil {
				return options, false, err
			}
			options.execProfile = value
		case arg == "-r" || arg == "--reasoning-effort":
			value, next, err := nextFlagValue(args, index, arg)
			if err != nil {
				return options, false, err
			}
			options.reasoningEffort = strings.TrimSpace(value)
			index = next
		case strings.HasPrefix(arg, "--reasoning-effort="):
			options.reasoningEffort = strings.TrimSpace(strings.TrimPrefix(arg, "--reasoning-effort="))
		case arg == "--use-spec":
			options.useSpec = true
		case arg == "--plan":
			options.plan = true
		case arg == "--spec-model":
			value, next, err := nextFlagValue(args, index, arg)
			if err != nil {
				return options, false, err
			}
			options.specModel = strings.TrimSpace(value)
			index = next
		case strings.HasPrefix(arg, "--spec-model="):
			value, err := requiredInlineFlagValue(arg, "--spec-model")
			if err != nil {
				return options, false, err
			}
			options.specModel = value
		case arg == "--spec-reasoning-effort":
			value, next, err := nextFlagValue(args, index, arg)
			if err != nil {
				return options, false, err
			}
			options.specReasoningEffort = strings.TrimSpace(value)
			index = next
		case strings.HasPrefix(arg, "--spec-reasoning-effort="):
			value, err := requiredInlineFlagValue(arg, "--spec-reasoning-effort")
			if err != nil {
				return options, false, err
			}
			options.specReasoningEffort = value
		case arg == "--max-turns":
			value, next, err := nextFlagValue(args, index, arg)
			if err != nil {
				return options, false, err
			}
			maxTurns, err := parseExecMaxTurns(value)
			if err != nil {
				return options, false, err
			}
			options.maxTurns = maxTurns
			index = next
		case strings.HasPrefix(arg, "--max-turns="):
			maxTurns, err := parseExecMaxTurns(strings.TrimSpace(strings.TrimPrefix(arg, "--max-turns=")))
			if err != nil {
				return options, false, err
			}
			options.maxTurns = maxTurns
		case arg == "-C" || arg == "--cwd":
			value, next, err := nextFlagValue(args, index, arg)
			if err != nil {
				return options, false, err
			}
			options.cwd = value
			index = next
		case strings.HasPrefix(arg, "--cwd="):
			options.cwd = strings.TrimSpace(strings.TrimPrefix(arg, "--cwd="))
		case arg == "-i" || arg == "--input-format":
			value, next, err := nextFlagValue(args, index, arg)
			if err != nil {
				return options, false, err
			}
			format, err := parseExecInputFormat(value)
			if err != nil {
				return options, false, err
			}
			options.inputFormat = format
			index = next
		case strings.HasPrefix(arg, "--input-format="):
			format, err := parseExecInputFormat(strings.TrimSpace(strings.TrimPrefix(arg, "--input-format=")))
			if err != nil {
				return options, false, err
			}
			options.inputFormat = format
		case arg == "-o" || arg == "--output-format":
			value, next, err := nextFlagValue(args, index, arg)
			if err != nil {
				return options, false, err
			}
			format, err := parseExecOutputFormat(value)
			if err != nil {
				return options, false, err
			}
			options.outputFormat = format
			index = next
		case strings.HasPrefix(arg, "--output-format="):
			format, err := parseExecOutputFormat(strings.TrimSpace(strings.TrimPrefix(arg, "--output-format=")))
			if err != nil {
				return options, false, err
			}
			options.outputFormat = format
		case arg == "-p" || arg == "--prompt":
			value, next, err := nextFlagValue(args, index, arg)
			if err != nil {
				return options, false, err
			}
			options.promptParts = append(options.promptParts, value)
			index = next
		case strings.HasPrefix(arg, "--prompt="):
			options.promptParts = append(options.promptParts, strings.TrimSpace(strings.TrimPrefix(arg, "--prompt=")))
		case strings.HasPrefix(arg, "-p="):
			options.promptParts = append(options.promptParts, strings.TrimSpace(strings.TrimPrefix(arg, "-p=")))
		case arg == "--resume":
			if index+1 < len(args) && !strings.HasPrefix(args[index+1], "-") && strings.TrimSpace(args[index+1]) != "" {
				options.resume = strings.TrimSpace(args[index+1])
				index++
			} else {
				options.resumeLatest = true
			}
		case strings.HasPrefix(arg, "--resume="):
			value := strings.TrimSpace(strings.TrimPrefix(arg, "--resume="))
			if value == "" {
				options.resumeLatest = true
			} else {
				options.resume = value
			}
		case arg == "--fork":
			value, next, err := nextFlagValue(args, index, arg)
			if err != nil {
				return options, false, err
			}
			options.fork = value
			index = next
		case strings.HasPrefix(arg, "--fork="):
			options.fork = strings.TrimSpace(strings.TrimPrefix(arg, "--fork="))
		case arg == "--calling-session-id":
			value, next, err := nextFlagValue(args, index, arg)
			if err != nil {
				return options, false, err
			}
			options.callingSessionID = strings.TrimSpace(value)
			index = next
		case strings.HasPrefix(arg, "--calling-session-id="):
			value, err := requiredInlineFlagValue(arg, "--calling-session-id")
			if err != nil {
				return options, false, err
			}
			options.callingSessionID = value
		case arg == "--calling-tool-use-id":
			value, next, err := nextFlagValue(args, index, arg)
			if err != nil {
				return options, false, err
			}
			options.callingToolUseID = strings.TrimSpace(value)
			index = next
		case strings.HasPrefix(arg, "--calling-tool-use-id="):
			value, err := requiredInlineFlagValue(arg, "--calling-tool-use-id")
			if err != nil {
				return options, false, err
			}
			options.callingToolUseID = value
		case arg == "--tag":
			value, next, err := nextFlagValue(args, index, arg)
			if err != nil {
				return options, false, err
			}
			options.tag = strings.TrimSpace(value)
			index = next
		case strings.HasPrefix(arg, "--tag="):
			value, err := requiredInlineFlagValue(arg, "--tag")
			if err != nil {
				return options, false, err
			}
			options.tag = value
		case arg == "--depth":
			value, next, err := nextFlagValue(args, index, arg)
			if err != nil {
				return options, false, err
			}
			depth, err := parseExecDepth(value)
			if err != nil {
				return options, false, err
			}
			options.depth = depth
			index = next
		case strings.HasPrefix(arg, "--depth="):
			depth, err := parseExecDepth(strings.TrimSpace(strings.TrimPrefix(arg, "--depth=")))
			if err != nil {
				return options, false, err
			}
			options.depth = depth
		case arg == "--session-title":
			value, next, err := nextFlagValue(args, index, arg)
			if err != nil {
				return options, false, err
			}
			options.sessionTitle = strings.TrimSpace(value)
			index = next
		case strings.HasPrefix(arg, "--session-title="):
			value, err := requiredInlineFlagValue(arg, "--session-title")
			if err != nil {
				return options, false, err
			}
			options.sessionTitle = value
		case arg == "--init-session-id":
			value, next, err := nextFlagValue(args, index, arg)
			if err != nil {
				return options, false, err
			}
			options.initSessionID = strings.TrimSpace(value)
			index = next
		case strings.HasPrefix(arg, "--init-session-id="):
			value, err := requiredInlineFlagValue(arg, "--init-session-id")
			if err != nil {
				return options, false, err
			}
			options.initSessionID = value
		case arg == "-w" || arg == "--worktree":
			options.worktree = true
			if index+1 < len(args) && !flagValueLooksLikeOption(strings.TrimSpace(args[index+1])) && strings.TrimSpace(args[index+1]) != "" {
				options.worktreeName = strings.TrimSpace(args[index+1])
				index++
			}
		case strings.HasPrefix(arg, "--worktree="):
			options.worktree = true
			options.worktreeName = strings.TrimSpace(strings.TrimPrefix(arg, "--worktree="))
		case arg == "--worktree-dir":
			value, next, err := nextFlagValue(args, index, arg)
			if err != nil {
				return options, false, err
			}
			options.worktreeDir = value
			index = next
		case strings.HasPrefix(arg, "--worktree-dir="):
			options.worktreeDir = strings.TrimSpace(strings.TrimPrefix(arg, "--worktree-dir="))
		case arg == "--":
			options.promptParts = append(options.promptParts, args[index+1:]...)
			index = len(args)
		case strings.HasPrefix(arg, "-"):
			return options, false, execUsageError{fmt.Sprintf("unknown exec flag %q", arg)}
		default:
			options.promptParts = append(options.promptParts, arg)
		}
	}

	if options.noNotify && options.notifyMode != "" {
		return options, false, execUsageError{"Use either --notify or --no-notify, not both."}
	}
	if options.notifyMode != "" {
		switch options.notifyMode {
		case "off", "bell", "notify", "both":
		default:
			return options, false, execUsageError{fmt.Sprintf("invalid --notify %q. Expected off, bell, notify, or both.", options.notifyMode)}
		}
	}
	if (options.resume != "" || options.resumeLatest) && options.fork != "" {
		return options, false, execUsageError{"Use either --resume or --fork, not both."}
	}
	if options.useSpec && (options.resume != "" || options.resumeLatest || options.fork != "") {
		return options, false, execUsageError{"--use-spec cannot be combined with --resume or --fork."}
	}
	if options.useSpec && strings.EqualFold(strings.TrimSpace(options.tag), specialist.SessionTagSpecialist) {
		return options, false, execUsageError{"--use-spec cannot be used inside a specialist child session."}
	}
	if options.useSpec && options.selfCorrect {
		// The spec-draft (planning) path never wires the post-edit self-correct loop,
		// so accepting the flag here would silently ignore it. Reject the combination
		// rather than pretend it took effect.
		return options, false, execUsageError{"--self-correct cannot be combined with --use-spec."}
	}
	if options.useSpec && options.noCompletionGate {
		// Same reasoning as --self-correct above: the spec-draft path never consults
		// the completion gate, so the flag would be silently ignored.
		return options, false, execUsageError{"--no-completion-gate cannot be combined with --use-spec."}
	}
	if !options.useSpec && options.specModel != "" {
		return options, false, execUsageError{"--spec-model requires --use-spec."}
	}
	if !options.useSpec && options.specReasoningEffort != "" {
		return options, false, execUsageError{"--spec-reasoning-effort requires --use-spec."}
	}
	// Plan mode may be entered via --plan or --permission-mode plan. Combination
	// guards must key off both: otherwise --permission-mode plan bypasses the
	// worktree/unsafe/use-spec rejects that --plan alone enforces, and worktree
	// prep mutates the filesystem before the read-only mode is assigned.
	planMode := options.plan || strings.EqualFold(strings.TrimSpace(options.permissionMode), "plan")
	if planMode && options.useSpec {
		return options, false, execUsageError{"Use either --plan or --use-spec, not both."}
	}
	if planMode && options.skipPermissionsUnsafe {
		return options, false, execUsageError{"Use either --plan or --skip-permissions-unsafe, not both."}
	}
	if planMode && options.worktree {
		// Worktree prep (copying/branching the workspace) runs before the plan
		// permission mode is assigned, so it would happen even under plan mode's
		// read-only, no-side-effects promise. Reject the combination outright
		// rather than let a mutation slip in ahead of the mode gate.
		return options, false, execUsageError{"--plan cannot be combined with --worktree."}
	}
	if options.plan && options.permissionMode != "" && strings.ToLower(options.permissionMode) != "plan" {
		return options, false, execUsageError{"--plan cannot be combined with --permission-mode=" + options.permissionMode + "."}
	}
	if options.initSessionID != "" && (options.resume != "" || options.resumeLatest) {
		return options, false, execUsageError{"Use --init-session-id only when creating or forking a session."}
	}
	if options.worktree && options.fork != "" {
		return options, false, execUsageError{"--fork cannot be used with --worktree. Forked sessions must continue in the source session workspace."}
	}
	if options.worktreeDir != "" && !options.worktree {
		return options, false, execUsageError{"--worktree-dir requires --worktree."}
	}
	if options.initSessionID != "" && !sessions.ValidSessionID(options.initSessionID) {
		return options, false, execUsageError{fmt.Sprintf("invalid --init-session-id %q", options.initSessionID)}
	}
	if options.inputFormat == execInputStreamJSON && strings.TrimSpace(strings.Join(options.promptParts, " ")) != "" {
		return options, false, execUsageError{"Stream-json input does not accept positional prompt text. Pipe JSONL or use --file."}
	}
	if !options.listTools && options.file == "" && options.inputFormat != execInputStreamJSON && strings.TrimSpace(strings.Join(options.promptParts, " ")) == "" {
		return options, false, execUsageError{"Prompt required. Use `zero exec \"prompt\"` or `zero exec --file prompt.txt`."}
	}
	return options, false, nil
}

func parseExecDepth(value string) (int, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return 0, execUsageError{"--depth requires a value"}
	}
	depth, err := strconv.Atoi(trimmed)
	if err != nil || depth < 0 {
		return 0, execUsageError{fmt.Sprintf("invalid --depth %q. Expected a non-negative integer.", value)}
	}
	return depth, nil
}

func parseExecMaxTurns(value string) (int, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return 0, execUsageError{"--max-turns requires a value"}
	}
	maxTurns, err := strconv.Atoi(trimmed)
	if err != nil || maxTurns <= 0 {
		return 0, execUsageError{fmt.Sprintf("invalid --max-turns %q. Expected a positive integer.", value)}
	}
	return maxTurns, nil
}

func nextFlagValue(args []string, index int, flag string) (string, int, error) {
	if index+1 >= len(args) {
		return "", index, execUsageError{fmt.Sprintf("%s requires a value", flag)}
	}
	next := strings.TrimSpace(args[index+1])
	if next == "" || flagValueLooksLikeOption(next) {
		return "", index, execUsageError{fmt.Sprintf("%s requires a value", flag)}
	}

	// Validate specific flag values to prevent consuming positional prompt text
	switch flag {
	case "--auto":
		switch strings.ToLower(next) {
		case "low", "medium", "high", "member":
		default:
			return "", index, execUsageError{fmt.Sprintf("Invalid autonomy level %q. Expected low, medium, or high.", next)}
		}
	case "--notify":
		switch strings.ToLower(next) {
		case "off", "bell", "notify", "both":
		default:
			return "", index, execUsageError{fmt.Sprintf("invalid --notify %q. Expected off, bell, notify, or both.", next)}
		}
	case "-o", "--output":
		switch strings.ToLower(next) {
		case "text", "json", "stream-json", "debug":
		default:
			return "", index, execUsageError{fmt.Sprintf("invalid output format %q. Expected text, json, stream-json, or debug.", next)}
		}
	case "-i", "--input":
		switch strings.ToLower(next) {
		case "text", "stream-json":
		default:
			return "", index, execUsageError{fmt.Sprintf("Invalid input format %q. Expected text or stream-json.", next)}
		}
	case "--reasoning-effort", "--spec-reasoning-effort":
		switch strings.ToLower(next) {
		case "low", "medium", "high":
		default:
			return "", index, execUsageError{fmt.Sprintf("invalid %s %q. Expected low, medium, or high.", flag, next)}
		}
	case "--depth":
		if _, err := strconv.Atoi(next); err != nil {
			return "", index, execUsageError{fmt.Sprintf("invalid --depth %q. Expected a non-negative integer.", next)}
		}
	case "--max-turns":
		if _, err := strconv.Atoi(next); err != nil {
			return "", index, execUsageError{fmt.Sprintf("invalid --max-turns %q. Expected a positive integer.", next)}
		}
	}

	return next, index + 1, nil
}

func requiredInlineFlagValue(arg string, flag string) (string, error) {
	value := strings.TrimSpace(strings.TrimPrefix(arg, flag+"="))
	if value == "" {
		return "", execUsageError{fmt.Sprintf("%s requires a value", flag)}
	}
	return value, nil
}

func flagValueLooksLikeOption(value string) bool {
	if !strings.HasPrefix(value, "-") {
		return false
	}
	if _, err := strconv.ParseFloat(value, 64); err == nil {
		return false
	}
	return true
}

func parseExecOutputFormat(value string) (execOutputFormat, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", string(execOutputText):
		return execOutputText, nil
	case string(execOutputJSON):
		return execOutputJSON, nil
	case string(execOutputStreamJSON), "debug":
		return execOutputStreamJSON, nil
	default:
		return "", execUsageError{fmt.Sprintf("invalid output format %q. Expected text, json, stream-json, or debug.", value)}
	}
}

func parseExecInputFormat(value string) (execInputFormat, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", string(execInputText):
		return execInputText, nil
	case string(execInputStreamJSON):
		return execInputStreamJSON, nil
	default:
		return "", execUsageError{fmt.Sprintf("Invalid input format %q. Expected text or stream-json.", value)}
	}
}
