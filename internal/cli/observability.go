package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/Gitlawb/zero/internal/config"
	"github.com/Gitlawb/zero/internal/doctor"
	"github.com/Gitlawb/zero/internal/providerhealth"
	zsearch "github.com/Gitlawb/zero/internal/search"
	"github.com/Gitlawb/zero/internal/sessions"
)

type doctorOptions struct {
	json         bool
	connectivity bool
}

func runDoctor(args []string, stdout io.Writer, stderr io.Writer, deps appDeps) int {
	options, help, err := parseDoctorArgs(args)
	if err != nil {
		return writeExecUsageError(stderr, err.Error())
	}
	if help {
		if err := writeDoctorHelp(stdout); err != nil {
			return exitCrash
		}
		return exitSuccess
	}

	workspaceRoot, err := resolveWorkspaceRoot("", deps)
	if err != nil {
		return writeExecUsageError(stderr, err.Error())
	}

	// Resolve config paths for the validation check even if resolution itself
	// fails on malformed JSON — that failure is exactly what config.validation
	// is meant to report, so it must not abort the run.
	var userConfig, projectConfig string
	if resolveOptions, optErr := config.DefaultResolveOptions(workspaceRoot); optErr == nil {
		userConfig = resolveOptions.UserConfigPath
		projectConfig = resolveOptions.ProjectConfigPath
	}
	if path, pathErr := deps.userConfigPath(); pathErr == nil {
		userConfig = path
	}

	var provider config.ProviderProfile
	var sandboxConfig config.SandboxConfig
	var configResolveErr error
	if resolved, resolveErr := deps.resolveConfig(workspaceRoot, config.Overrides{}); resolveErr == nil {
		provider = resolved.Provider
		sandboxConfig = resolved.Sandbox
	} else {
		configResolveErr = resolveErr
	}
	var health *providerhealth.Result
	if options.connectivity && config.HasProviderProfile(provider) {
		ctx, stop := signalContext()
		defer stop()
		probe := deps.probeProviderHealth(ctx, providerhealth.Options{
			Profile:      provider,
			Connectivity: true,
			UserAgent:    userAgent(),
		})
		health = &probe
	}

	report := doctor.Run(doctor.Options{
		Now:            deps.now,
		Runtime:        "go",
		UserConfig:     userConfig,
		ProjectConfig:  projectConfig,
		Provider:       provider,
		ResolveError:   configResolveErr,
		WorkspaceRoot:  workspaceRoot,
		Sandbox:        sandboxConfig,
		Connectivity:   options.connectivity,
		ProviderHealth: health,
	})
	if options.json {
		if err := writePrettyJSON(stdout, report); err != nil {
			return exitCrash
		}
		if !report.OK {
			return exitProvider
		}
		return exitSuccess
	}
	if _, err := fmt.Fprintln(stdout, doctor.Format(report)); err != nil {
		return exitCrash
	}
	if !report.OK {
		return exitProvider
	}
	return exitSuccess
}

type searchOptions struct {
	query        string
	json         bool
	limit        int
	contextChars int
	sessionID    string
	eventType    sessions.EventType
	reindex      bool
}

func runSearch(args []string, stdout io.Writer, stderr io.Writer, deps appDeps) int {
	options, help, err := parseSearchArgs(args)
	if err != nil {
		return writeExecUsageError(stderr, err.Error())
	}
	if help {
		if err := writeSearchHelp(stdout); err != nil {
			return exitCrash
		}
		return exitSuccess
	}
	if strings.TrimSpace(options.query) == "" {
		return writeExecUsageError(stderr, "search query required. Use `zero search <query>`.")
	}

	result, err := zsearch.Sessions(options.query, zsearch.Options{
		Store:        deps.newSessionStore(),
		Limit:        options.limit,
		ContextChars: options.contextChars,
		SessionID:    options.sessionID,
		Type:         options.eventType,
		Reindex:      options.reindex,
		Now:          deps.now,
	})
	if err != nil {
		return writeAppError(stderr, err.Error(), exitCrash)
	}
	if options.json {
		if err := writePrettyJSON(stdout, zsearch.RedactResult(result)); err != nil {
			return exitCrash
		}
		return exitSuccess
	}
	if _, err := fmt.Fprintln(stdout, zsearch.FormatResult(result)); err != nil {
		return exitCrash
	}
	return exitSuccess
}

func parseDoctorArgs(args []string) (doctorOptions, bool, error) {
	options := doctorOptions{}
	for _, arg := range args {
		switch arg {
		case "-h", "--help", "help":
			return options, true, nil
		case "--json":
			options.json = true
		case "--connectivity":
			options.connectivity = true
		default:
			return options, false, execUsageError{fmt.Sprintf("unknown doctor flag %q", arg)}
		}
	}
	return options, false, nil
}

func parseSearchArgs(args []string) (searchOptions, bool, error) {
	options := searchOptions{}
	queryParts := []string{}
	for index := 0; index < len(args); index++ {
		arg := args[index]
		switch {
		case arg == "-h" || arg == "--help" || arg == "help":
			return options, true, nil
		case arg == "--json":
			options.json = true
		case arg == "--reindex":
			options.reindex = true
		case arg == "--limit":
			value, next, err := nextFlagValue(args, index, arg)
			if err != nil {
				return options, false, err
			}
			limit, err := parsePositiveOrZeroInt(value, "--limit")
			if err != nil {
				return options, false, err
			}
			options.limit = limit
			index = next
		case strings.HasPrefix(arg, "--limit="):
			limit, err := parsePositiveOrZeroInt(strings.TrimSpace(strings.TrimPrefix(arg, "--limit=")), "--limit")
			if err != nil {
				return options, false, err
			}
			options.limit = limit
		case arg == "--context-chars" || arg == "--context":
			value, next, err := nextFlagValue(args, index, arg)
			if err != nil {
				return options, false, err
			}
			contextChars, err := parsePositiveOrZeroInt(value, arg)
			if err != nil {
				return options, false, err
			}
			options.contextChars = contextChars
			index = next
		case strings.HasPrefix(arg, "--context-chars="):
			contextChars, err := parsePositiveOrZeroInt(strings.TrimSpace(strings.TrimPrefix(arg, "--context-chars=")), "--context-chars")
			if err != nil {
				return options, false, err
			}
			options.contextChars = contextChars
		case strings.HasPrefix(arg, "--context="):
			contextChars, err := parsePositiveOrZeroInt(strings.TrimSpace(strings.TrimPrefix(arg, "--context=")), "--context")
			if err != nil {
				return options, false, err
			}
			options.contextChars = contextChars
		case arg == "--session-id" || arg == "--session":
			value, next, err := nextFlagValue(args, index, arg)
			if err != nil {
				return options, false, err
			}
			options.sessionID = value
			index = next
		case strings.HasPrefix(arg, "--session-id="):
			options.sessionID = strings.TrimSpace(strings.TrimPrefix(arg, "--session-id="))
		case strings.HasPrefix(arg, "--session="):
			options.sessionID = strings.TrimSpace(strings.TrimPrefix(arg, "--session="))
		case arg == "--type":
			value, next, err := nextFlagValue(args, index, arg)
			if err != nil {
				return options, false, err
			}
			options.eventType = sessions.EventType(value)
			index = next
		case strings.HasPrefix(arg, "--type="):
			options.eventType = sessions.EventType(strings.TrimSpace(strings.TrimPrefix(arg, "--type=")))
		case strings.HasPrefix(arg, "-"):
			return options, false, execUsageError{fmt.Sprintf("unknown search flag %q", arg)}
		default:
			queryParts = append(queryParts, arg)
		}
	}
	options.query = strings.TrimSpace(strings.Join(queryParts, " "))
	return options, false, nil
}

func parsePositiveOrZeroInt(value string, label string) (int, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return 0, execUsageError{fmt.Sprintf("%s requires a value", label)}
	}
	parsed, err := strconv.Atoi(trimmed)
	if err != nil || parsed < 0 {
		return 0, execUsageError{fmt.Sprintf("invalid %s %q. Expected a non-negative integer.", label, value)}
	}
	return parsed, nil
}

func writePrettyJSON(w io.Writer, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	if _, err := w.Write(append(data, '\n')); err != nil {
		return err
	}
	return nil
}

func writeDoctorHelp(w io.Writer) error {
	_, err := fmt.Fprint(w, `Usage:
  zero doctor [flags]

Runs Go backend health checks for config and provider setup.

Flags:
      --json            Print JSON report
      --connectivity    Include provider endpoint connectivity probe when available
  -h, --help            Show this help
`)
	return err
}

func writeSearchHelp(w io.Writer) error {
	_, err := fmt.Fprint(w, `Usage:
  zero search [flags] <query>

Searches persisted local Zero session events.

Flags:
      --json                 Print JSON results
      --limit <number>       Maximum hits to return
      --context-chars <n>    Characters of context around each match
      --session-id <id>      Search one session
      --type <event-type>    Filter by session event type
      --reindex              Rebuild cached search indexes
  -h, --help                 Show this help
`)
	return err
}
