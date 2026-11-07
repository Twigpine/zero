package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/Twigpine/zero/internal/config"
	"github.com/Twigpine/zero/internal/contextreport"
	"github.com/Twigpine/zero/internal/modelregistry"
)

type contextOptions struct {
	json bool
}

func runContext(args []string, stdout io.Writer, stderr io.Writer, deps appDeps) int {
	options, help, err := parseContextArgs(args)
	if err != nil {
		return writeExecUsageError(stderr, err.Error())
	}
	if help {
		if err := writeContextHelp(stdout); err != nil {
			return exitCrash
		}
		return exitSuccess
	}

	workspaceRoot, err := resolveWorkspaceRoot("", deps)
	if err != nil {
		return writeExecUsageError(stderr, err.Error())
	}
	resolved, err := deps.resolveConfig(workspaceRoot, config.Overrides{})
	if err != nil {
		return writeAppError(stderr, err.Error(), exitProvider)
	}

	modelRegistry, err := modelregistry.DefaultRegistry()
	if err != nil {
		return writeAppError(stderr, err.Error(), exitCrash)
	}
	report, err := contextreport.Build(contextreport.Options{
		WorkspaceRoot:  workspaceRoot,
		Provider:       resolved.Provider,
		Registry:       newCoreRegistry(workspaceRoot),
		ContextWindow:  modelContextWindow(modelRegistry, resolved.Provider.Model),
		DeferThreshold: resolved.Tools.DeferThreshold,
	})
	if err != nil {
		return writeAppError(stderr, err.Error(), exitCrash)
	}
	if options.json {
		if err := writePrettyJSON(stdout, report); err != nil {
			return exitCrash
		}
		return exitSuccess
	}
	if _, err := fmt.Fprintln(stdout, contextreport.Format(report)); err != nil {
		return exitCrash
	}
	return exitSuccess
}

func parseContextArgs(args []string) (contextOptions, bool, error) {
	options := contextOptions{}
	for _, arg := range args {
		switch {
		case arg == "-h" || arg == "--help" || arg == "help":
			return options, true, nil
		case arg == "--json":
			options.json = true
		case strings.HasPrefix(arg, "-"):
			return options, false, execUsageError{fmt.Sprintf("unknown context flag %q", arg)}
		default:
			return options, false, execUsageError{fmt.Sprintf("unexpected context argument %q", arg)}
		}
	}
	return options, false, nil
}

func writeContextHelp(w io.Writer) error {
	_, err := fmt.Fprint(w, `Usage:
  zero context [flags]

Reports the current workspace context budget.

Flags:
      --json      Print JSON report
  -h, --help      Show this help
`)
	return err
}
