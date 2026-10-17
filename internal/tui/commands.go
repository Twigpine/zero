package tui

import (
	"strings"
)

type commandKind int

const (
	commandEmpty commandKind = iota
	commandPrompt
	commandHelp
	commandClear
	commandExit
	commandTools
	commandMCP
	commandPermissions
	commandPS
	commandStop
	commandAttach
	commandSandboxSetup
	commandProvider
	commandModel
	commandContext
	commandConfig
	commandDebug
	commandDoctor
	commandPlan
	commandSearch
	commandResume
	commandRename
	commandSpec
	commandInit
	commandCompact
	commandRewind
	commandEffort
	commandFast
	commandStyle
	commandTheme
	commandNotify
	commandTranscript
	commandBash
	commandImage
	commandAddDir
	commandSelfCorrect
	commandTurns
	commandProfile
	commandRetry
	commandEdit
	commandCopy
	commandExport
	commandNew
	commandBTW
	commandSkills
	commandLoop
	commandGoal
	commandVoice
	commandSTTModel
	commandPets
	commandUnknown
)

type commandGroup string

const (
	commandGroupSession commandGroup = "session"
	commandGroupModel   commandGroup = "model"
	commandGroupRuntime commandGroup = "runtime"
	commandGroupTools   commandGroup = "tools"
	commandGroupMeta    commandGroup = "meta"
)

type commandDefinition struct {
	name        string
	aliases     []string
	usage       string
	group       commandGroup
	description string
	kind        commandKind
}

type parsedCommand struct {
	kind commandKind
	text string
	name string
}

var commandDefinitions = []commandDefinition{
	{
		name:        "/provider",
		usage:       "/provider [add|status]",
		group:       commandGroupModel,
		description: "Manage providers: activate, add, edit, delete.",
		kind:        commandProvider,
	},
	{
		name:        "/model",
		usage:       "/model [list|id]",
		group:       commandGroupModel,
		description: "Show or switch the active model.",
		kind:        commandModel,
	},
	{
		name:        "/stt-model",
		usage:       "/stt-model",
		group:       commandGroupModel,
		description: "Choose the speech-to-text (dictation) model.",
		kind:        commandSTTModel,
	},
	{
		name:        "/voice",
		usage:       "/voice",
		group:       commandGroupRuntime,
		description: "Toggle voice mode (" + voiceCaptureUsage + ").",
		kind:        commandVoice,
	},
	{
		name:        "/plan",
		usage:       "/plan [status|on|off]",
		group:       commandGroupSession,
		description: "Show plan status, or enter/exit read-only planning mode.",
		kind:        commandPlan,
	},
	{
		name:        "/permissions",
		usage:       "/permissions",
		group:       commandGroupRuntime,
		description: "Show the active permission mode and sandbox grants.",
		kind:        commandPermissions,
	},
	{
		name:        "/ps",
		usage:       "/ps",
		group:       commandGroupRuntime,
		description: "List running background terminal sessions.",
		kind:        commandPS,
	},
	{
		name:        "/stop",
		usage:       "/stop [session_id]",
		group:       commandGroupRuntime,
		description: "Stop running background terminal sessions.",
		kind:        commandStop,
	},
	{
		name:        "/attach",
		usage:       "/attach [session_id]",
		group:       commandGroupRuntime,
		description: "Type directly into a running interactive terminal session.",
		kind:        commandAttach,
	},
	{
		name:        "/sandbox-setup",
		usage:       "/sandbox-setup",
		group:       commandGroupRuntime,
		description: "Run native sandbox setup for this platform.",
		kind:        commandSandboxSetup,
	},
	{
		name:        "/tools",
		usage:       "/tools",
		group:       commandGroupTools,
		description: "List registered tools.",
		kind:        commandTools,
	},
	{
		name:        "/skills",
		usage:       "/skills",
		group:       commandGroupTools,
		description: "List installed skills; run one directly with /<skill-name> [args].",
		kind:        commandSkills,
	},
	{
		name:        "/context",
		usage:       "/context",
		group:       commandGroupSession,
		description: "Show current workspace and runtime context.",
		kind:        commandContext,
	},
	{
		name:        "/image",
		usage:       "/image <path> | clear",
		group:       commandGroupSession,
		description: "Attach a local image (vision models) or PDF (text layer for any model) to the next message. /image clear removes pending attachments.",
		kind:        commandImage,
	},
	{
		name:        "/add-dir",
		usage:       "/add-dir [path]",
		group:       commandGroupRuntime,
		description: "Grant write access to a directory outside the workspace for this session; bare form lists current write roots.",
		kind:        commandAddDir,
	},
	{
		name:        "/clear",
		usage:       "/clear",
		group:       commandGroupMeta,
		description: "Clear the visible transcript.",
		kind:        commandClear,
	},
	{
		name:        "/new",
		usage:       "/new",
		group:       commandGroupSession,
		description: "Start a fresh session; the current one stays resumable via /resume.",
		kind:        commandNew,
	},
	{
		name:        "/btw",
		usage:       "/btw [question]",
		group:       commandGroupSession,
		description: "Open an isolated side conversation; run /btw again to return.",
		kind:        commandBTW,
	},
	{
		name:        "/search",
		aliases:     []string{"/find"},
		usage:       "/search <query>",
		group:       commandGroupTools,
		description: "Search local session events. Requires a query argument.",
		kind:        commandSearch,
	},
	{
		name:        "/mcp",
		aliases:     []string{"/mcp-status"},
		usage:       "/mcp",
		group:       commandGroupTools,
		description: "Show MCP server status.",
		kind:        commandMCP,
	},
	{
		name:        "/resume",
		aliases:     []string{"/sessions"},
		usage:       "/resume [id]",
		group:       commandGroupSession,
		description: "List recent sessions or show resume guidance.",
		kind:        commandResume,
	},
	{
		name:        "/rename",
		usage:       "/rename [title]",
		group:       commandGroupSession,
		description: "Rename the current session (no arg opens an editor).",
		kind:        commandRename,
	},
	{
		name:        "/spec",
		usage:       "/spec <task>",
		group:       commandGroupSession,
		description: "Draft an implementation spec for review before editing.",
		kind:        commandSpec,
	},
	{
		name:        "/init",
		usage:       "/init",
		group:       commandGroupSession,
		description: "Investigate the repo and generate an AGENTS.md for the agent.",
		kind:        commandInit,
	},
	{
		name:        "/compact",
		usage:       "/compact [status|now]",
		group:       commandGroupSession,
		description: "Compact the transcript now, or show compaction state (/compact status).",
		kind:        commandCompact,
	},
	{
		name:        "/transcript",
		usage:       "/transcript",
		group:       commandGroupSession,
		description: "Toggle the detailed transcript view.",
		kind:        commandTranscript,
	},
	{
		name:        "/rewind",
		aliases:     []string{"/undo"},
		usage:       "/rewind [latest|<sequence>]",
		group:       commandGroupSession,
		description: "Restore workspace files to a checkpoint and truncate the session.",
		kind:        commandRewind,
	},
	{
		name:        "/effort",
		usage:       "/effort [list|level|auto]",
		group:       commandGroupModel,
		description: "Show or set reasoning effort for supported models.",
		kind:        commandEffort,
	},
	{
		name:        "/fast",
		usage:       "/fast",
		group:       commandGroupModel,
		description: "Toggle fast mode for supported ChatGPT subscription models.",
		kind:        commandFast,
	},
	{
		name:        "/style",
		usage:       "/style [list|balanced|concise|explanatory|review]",
		group:       commandGroupSession,
		description: "Show or set the response style preference.",
		kind:        commandStyle,
	},
	{
		name:        "/selfcorrect",
		aliases:     []string{"/sc"},
		usage:       "/selfcorrect [status|on|off|tests|full|lsp]",
		group:       commandGroupSession,
		description: "Show or set post-edit self-correction depth (LSP-only default; on/tests/full add the project test plan; off/lsp disable tests, LSP-only).",
		kind:        commandSelfCorrect,
	},
	{
		name:        "/turns",
		usage:       "/turns [n]",
		group:       commandGroupSession,
		description: "Show or set the per-run tool-turn budget for this session (raise it for long multi-step tasks).",
		kind:        commandTurns,
	},
	{
		name:        "/profile",
		usage:       "/profile [status|balanced|fast|thorough]",
		group:       commandGroupSession,
		description: "Show or switch the execution profile for the next run (loop posture: turn budget, effort, self-correction, escalation; model selection is unchanged).",
		kind:        commandProfile,
	},
	{
		name:        "/retry",
		usage:       "/retry",
		group:       commandGroupSession,
		description: "Resend your last prompt.",
		kind:        commandRetry,
	},
	{
		name:        "/edit",
		usage:       "/edit",
		group:       commandGroupSession,
		description: "Recall your last prompt into the composer to edit and resend.",
		kind:        commandEdit,
	},
	{
		name:        "/copy",
		usage:       "/copy",
		group:       commandGroupSession,
		description: "Copy the last answer to the clipboard.",
		kind:        commandCopy,
	},
	{
		name:        "/export",
		usage:       "/export [path]",
		group:       commandGroupSession,
		description: "Write the conversation transcript to a file.",
		kind:        commandExport,
	},
	{
		name:        "/loop",
		usage:       "/loop [interval] <prompt|/command> | /loop list | /loop stop [id|all]",
		group:       commandGroupSession,
		description: "Repeat a prompt or command on an interval (e.g. /loop 5m /babysit-prs), or self-paced when no interval is given.",
		kind:        commandLoop,
	},
	{
		name:        "/goal",
		usage:       "/goal [--tokens N] <objective> | status | pause | resume | edit | clear",
		group:       commandGroupSession,
		description: "Create and pursue one persistent objective for this session.",
		kind:        commandGoal,
	},
	{
		name:        "/help",
		usage:       "/help",
		group:       commandGroupMeta,
		description: "Show available commands.",
		kind:        commandHelp,
	},
	{
		name:        "/pets",
		aliases:     []string{"/pet"},
		usage:       "/pets [name|off]",
		group:       commandGroupMeta,
		description: "Choose, preview, or hide a terminal companion.",
		kind:        commandPets,
	},
	{
		name:        "/doctor",
		aliases:     []string{"/health"},
		usage:       "/doctor [fix|--connectivity]",
		group:       commandGroupRuntime,
		description: "Show diagnostics, open safe fixes, or run provider connectivity checks.",
		kind:        commandDoctor,
	},
	{
		name:        "/config",
		usage:       "/config [recaps on|off]",
		group:       commandGroupRuntime,
		description: "Show active configuration; toggle settings (recaps on|off).",
		kind:        commandConfig,
	},
	{
		name:        "/debug",
		aliases:     []string{"/debug-mode"},
		usage:       "/debug",
		group:       commandGroupRuntime,
		description: "Show debug mode status.",
		kind:        commandDebug,
	},
	{
		name:        "/theme",
		usage:       "/theme [list|auto|name]",
		group:       commandGroupSession,
		description: "Pick a color theme (no arg opens the picker; auto detects the terminal background).",
		kind:        commandTheme,
	},
	{
		name:        "/notify",
		usage:       "/notify [list|off|bell|notify|both [unfocused|always|focused]]",
		group:       commandGroupSession,
		description: "Pick when Zero alerts you (completion and needs-input). No arg opens the picker.",
		kind:        commandNotify,
	},
	{
		name:        "/exit",
		aliases:     []string{"/quit"},
		usage:       "/exit",
		group:       commandGroupMeta,
		description: "Exit Zero.",
		kind:        commandExit,
	},
}

func parseCommand(input string) parsedCommand {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		return parsedCommand{kind: commandEmpty}
	}

	// "!cmd" is a shell escape (the footer advertises "! bash"): run it directly.
	if strings.HasPrefix(trimmed, "!") {
		return parsedCommand{kind: commandBash, text: strings.TrimSpace(trimmed[1:])}
	}

	if strings.HasPrefix(trimmed, "/") {
		name, args := splitCommand(trimmed)
		command, ok := resolveCommand(name)
		if ok {
			return parsedCommand{kind: command.kind, name: command.name, text: args}
		}
		return parsedCommand{kind: commandUnknown, text: trimmed}
	}

	return parsedCommand{kind: commandPrompt, text: trimmed}
}

func splitCommand(input string) (string, string) {
	parts := strings.Fields(input)
	if len(parts) == 0 {
		return "", ""
	}

	name := parts[0]
	args := strings.TrimSpace(input[len(name):])
	return strings.ToLower(name), args
}

func resolveCommand(name string) (commandDefinition, bool) {
	normalized := strings.ToLower(strings.TrimSpace(name))
	for _, command := range commandDefinitions {
		if normalized == command.name {
			return command, true
		}
		for _, alias := range command.aliases {
			if normalized == alias {
				return command, true
			}
		}
	}
	return commandDefinition{}, false
}

func formatGroupedCommandHelp() string {
	lines := []string{"Commands", "status: info"}
	for _, group := range commandGroupOrder() {
		groupLines := commandHelpLinesForGroup(group)
		if len(groupLines) == 0 {
			continue
		}
		lines = append(lines, commandGroupTitle(group))
		lines = append(lines, groupLines...)
	}
	lines = append(lines, "hint: submit plain text to ask Zero")
	return strings.Join(lines, "\n")
}

func commandHelpLinesForGroup(group commandGroup) []string {
	lines := []string{}
	for _, command := range commandDefinitions {
		if command.group != group {
			continue
		}
		lines = append(lines, "  "+formatCommandHelpLine(command))
	}
	return lines
}

func formatCommandHelpLine(command commandDefinition) string {
	label := command.usage
	if len(command.aliases) > 0 {
		label += " (" + strings.Join(command.aliases, ", ") + ")"
	}
	return label + " - " + command.description
}

func commandSelectionRequiresInput(name string) bool {
	command, ok := resolveCommand(name)
	return ok && commandUsageRequiresInput(command.usage)
}

func commandRequiredInputHint(name string) string {
	command, ok := resolveCommand(name)
	if !ok {
		return ""
	}
	placeholder := commandUsageRequiredPlaceholder(command.usage)
	if placeholder == "" {
		return ""
	}
	return "[" + placeholder + "]"
}

func commandUsageRequiresInput(usage string) bool {
	return commandUsageRequiredPlaceholder(usage) != ""
}

func commandUsageRequiredPlaceholder(usage string) string {
	optionalDepth := 0
	var placeholder strings.Builder
	inPlaceholder := false
	for _, char := range usage {
		if inPlaceholder {
			if char == '>' {
				return strings.TrimSpace(placeholder.String())
			}
			placeholder.WriteRune(char)
			continue
		}
		switch char {
		case '[':
			optionalDepth++
		case ']':
			if optionalDepth > 0 {
				optionalDepth--
			}
		case '<':
			if optionalDepth == 0 {
				inPlaceholder = true
			}
		}
	}
	return ""
}

func commandGroupOrder() []commandGroup {
	return []commandGroup{
		commandGroupModel,
		commandGroupSession,
		commandGroupRuntime,
		commandGroupTools,
		commandGroupMeta,
	}
}

func commandGroupTitle(group commandGroup) string {
	switch group {
	case commandGroupModel:
		return "Model"
	case commandGroupSession:
		return "Session"
	case commandGroupRuntime:
		return "Runtime"
	case commandGroupTools:
		return "Tools"
	case commandGroupMeta:
		return "Meta"
	default:
		return string(group)
	}
}
