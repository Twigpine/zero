package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/Gitlawb/zero/internal/config"
)

// runConfigNotify implements `zero config notify`: with no flags it prints the
// stored mode/focusMode; --mode/--focus update them via the same
// config.SetNotify writer the TUI /notify command uses, so all surfaces stay
// in lockstep; --reset blanks both fields so the TUI's effective default
// applies again (an unconfigured headless run stays silent).
//
// The command manages a user preference, so it talks ONLY to the user's own
// config file (config.UserNotify / config.SetNotify) and never runs the full
// config resolution: resolving providers would fail with ErrNoActiveProvider
// for a brand-new user, locking them out of setting notifications before they
// have even configured a provider (CodeRabbit review, PR #1001). The display
// therefore reports the USER'S stored values — a project config that
// overrides notify for one repo is not shown here, by the same logic the
// maintainer applied to the write path.
func runConfigNotify(args []string, stdout io.Writer, stderr io.Writer, deps appDeps) int {
	options, help, err := parseConfigNotifyArgs(args)
	if err != nil {
		return writeExecUsageError(stderr, err.Error())
	}
	if help {
		if err := writeConfigNotifyHelp(stdout); err != nil {
			return exitCrash
		}
		return exitSuccess
	}

	configPath, err := deps.userConfigPath()
	if err != nil {
		return writeAppError(stderr, err.Error(), exitCrash)
	}

	if options.mode != "" || options.focus != "" || options.reset {
		// One serialized read-merge-write transaction: the lock covers
		// reading the stored block, applying only the explicit fields, and
		// replacing the file, so two concurrent partial updates (e.g.
		// --mode off and --focus always from two terminals) cannot lose
		// each other's change (maintainer review, PR #1001). Omitted
		// fields preserve the values stored in the user's OWN file — blank
		// stays blank; --reset is the only path that clears both fields.
		_, err := config.UpdateNotify(configPath, func(current config.NotifyConfig) config.NotifyConfig {
			if options.reset {
				return config.NotifyConfig{}
			}
			if options.mode != "" {
				current.Mode = options.mode
			}
			if options.focus != "" {
				current.FocusMode = options.focus
			}
			return current
		})
		if err != nil {
			return writeAppError(stderr, err.Error(), exitUsage)
		}
	}

	// Report the stored values (blank renders as "(default)").
	current, err := config.UserNotify(configPath)
	if err != nil {
		return writeAppError(stderr, err.Error(), exitUsage)
	}
	if options.json {
		if err := writePrettyJSON(stdout, map[string]any{
			"mode":      current.Mode,
			"focusMode": current.FocusMode,
		}); err != nil {
			return exitCrash
		}
		return exitSuccess
	}
	lines := []string{
		"Notify",
		"mode:      " + displayCLIValue(current.Mode, "(default)"),
		"focusMode: " + displayCLIValue(current.FocusMode, "(default)"),
	}
	if _, err := fmt.Fprintln(stdout, strings.Join(lines, "\n")); err != nil {
		return exitCrash
	}
	return exitSuccess
}

type configNotifyOptions struct {
	mode  string
	focus string
	reset bool
	json  bool
}

func parseConfigNotifyArgs(args []string) (configNotifyOptions, bool, error) {
	options := configNotifyOptions{}
	for index := 0; index < len(args); index++ {
		arg := args[index]
		switch {
		case arg == "-h" || arg == "--help" || arg == "help":
			return options, true, nil
		case arg == "--json":
			options.json = true
		case arg == "--reset":
			options.reset = true
		case arg == "--mode":
			value, next, err := nextFlagValue(args, index, arg)
			if err != nil {
				return options, false, err
			}
			options.mode = value
			index = next
		case strings.HasPrefix(arg, "--mode="):
			value, err := requiredInlineFlagValue(arg, "--mode")
			if err != nil {
				return options, false, err
			}
			options.mode = value
		case arg == "--focus":
			value, next, err := nextFlagValue(args, index, arg)
			if err != nil {
				return options, false, err
			}
			options.focus = value
			index = next
		case strings.HasPrefix(arg, "--focus="):
			value, err := requiredInlineFlagValue(arg, "--focus")
			if err != nil {
				return options, false, err
			}
			options.focus = value
		case strings.HasPrefix(arg, "-"):
			return options, false, execUsageError{fmt.Sprintf("unknown flag %q", arg)}
		default:
			return options, false, execUsageError{fmt.Sprintf("unexpected argument %q", arg)}
		}
	}
	return options, false, nil
}

func writeConfigNotifyHelp(w io.Writer) error {
	_, err := fmt.Fprint(w, "Usage:\n"+
		"  zero config notify [flags]\n"+
		"\n"+
		"Print or update the stored global notification preference.\n"+
		"\n"+
		"The preference controls BOTH notification kinds: the turn-completion\n"+
		"(\"Zero: ready\") alert and the needs-input alert. mode off silences both;\n"+
		"the focus mode (unfocused, always, focused) applies to both.\n"+
		"\n"+
		"When run with no flag, prints the stored mode and focusMode; a field you\n"+
		"never set shows as (default) — the TUI alerts with bell + notification,\n"+
		"firing only when the terminal is unfocused, while an unconfigured\n"+
		"headless run stays silent. Omitted flags preserve the values stored in\n"+
		"YOUR config file; --reset clears both so the TUI's effective default\n"+
		"applies again. If --reset is combined with --mode/--focus, --reset wins\n"+
		"and the explicit values are ignored.\n"+
		"\n"+
		"Examples:\n"+
		"  zero config notify\n"+
		"  zero config notify --json\n"+
		"  zero config notify --mode both --focus unfocused\n"+
		"  zero config notify --mode off\n"+
		"  zero config notify --reset         # clear the stored preference\n"+
		"\n"+
		"Flags:\n"+
		"      --mode <off|bell|notify|both>       Notification mechanism (both kinds)\n"+
		"      --focus <unfocused|always|focused>  When the alert fires\n"+
		"      --reset                             Clear the stored preference so the TUI effective default applies\n"+
		"      --json                              Machine-readable output\n"+
		"  -h, --help                              Show this help\n")
	return err
}
