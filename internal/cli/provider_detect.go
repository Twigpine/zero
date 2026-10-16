package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/Gitlawb/zero/internal/config"
	"github.com/Gitlawb/zero/internal/provideronboarding"
)

type providerDetectOptions struct {
	json bool
}

// providerDetectAction is the JSON/text shape of a single suggested next step.
type providerDetectAction struct {
	Label   string `json:"label"`
	Command string `json:"command"`
	Detail  string `json:"detail,omitempty"`
}

type providerDetectRuntime struct {
	CatalogID string               `json:"catalogID"`
	Name      string               `json:"name"`
	BaseURL   string               `json:"baseURL"`
	Models    []string             `json:"models,omitempty"`
	Action    providerDetectAction `json:"action"`
}

type providerDetectProvider struct {
	Name    string                 `json:"name"`
	Active  bool                   `json:"active"`
	Actions []providerDetectAction `json:"actions,omitempty"`
}

type providerDetectReport struct {
	DetectedRuntimes []providerDetectRuntime  `json:"detectedRuntimes"`
	Providers        []providerDetectProvider `json:"providers"`
}

// runProvidersDetect probes the machine for running local, OpenAI-compatible
// model runtimes (Ollama, LM Studio, Atomic Chat) and prints a no-key adopt command for each
// one it finds, followed by the next-step actions for every already-configured
// provider. It is the onboarding-advice surface — "what can I do right now?" —
// and never errors on a machine with nothing running locally (it just reports an
// empty detected list).
func runProvidersDetect(args []string, stdout io.Writer, stderr io.Writer, deps appDeps) int {
	options, help, err := parseProviderDetectArgs(args)
	if err != nil {
		return writeExecUsageError(stderr, err.Error())
	}
	if help {
		if err := writeProvidersHelp(stdout); err != nil {
			return exitCrash
		}
		return exitSuccess
	}

	resolved, exitCode := resolveCommandCenterConfig(stderr, deps)
	if exitCode != exitSuccess {
		return exitCode
	}

	ctx, stop := signalContext()
	defer stop()
	detected := deps.detectLocalRuntimes(ctx, provideronboarding.LocalDetectOptions{})

	report := buildProviderDetectReport(resolved, detected)

	if options.json {
		if err := writePrettyJSON(stdout, report); err != nil {
			return exitCrash
		}
		return exitSuccess
	}
	if _, err := fmt.Fprintln(stdout, formatProviderDetectReport(report)); err != nil {
		return exitCrash
	}
	return exitSuccess
}

func parseProviderDetectArgs(args []string) (providerDetectOptions, bool, error) {
	options := providerDetectOptions{}
	for _, arg := range args {
		switch arg {
		case "-h", "--help", "help":
			return options, true, nil
		case "--json":
			options.json = true
		default:
			return options, false, execUsageError{fmt.Sprintf("unexpected argument %q", arg)}
		}
	}
	return options, false, nil
}

func buildProviderDetectReport(resolved config.ResolvedConfig, detected []provideronboarding.DetectedLocalRuntime) providerDetectReport {
	report := providerDetectReport{
		DetectedRuntimes: make([]providerDetectRuntime, 0, len(detected)),
		Providers:        make([]providerDetectProvider, 0, len(resolved.Providers)),
	}
	for _, runtime := range detected {
		report.DetectedRuntimes = append(report.DetectedRuntimes, providerDetectRuntime{
			CatalogID: runtime.CatalogID,
			Name:      runtime.Name,
			BaseURL:   runtime.BaseURL,
			Models:    runtime.Models,
			Action:    providerDetectActionFrom(runtime.SetupAction()),
		})
	}
	for _, profile := range resolved.Providers {
		active := strings.TrimSpace(profile.Name) != "" && profile.Name == resolved.ActiveProvider
		state := provideronboarding.ProviderState{Profile: profile, Active: active}
		actions := state.Actions()
		entry := providerDetectProvider{
			Name:    profile.Name,
			Active:  active,
			Actions: make([]providerDetectAction, 0, len(actions)),
		}
		for _, action := range actions {
			entry.Actions = append(entry.Actions, providerDetectActionFrom(action))
		}
		report.Providers = append(report.Providers, entry)
	}
	return report
}

func providerDetectActionFrom(action provideronboarding.Action) providerDetectAction {
	return providerDetectAction{Label: action.Label, Command: action.Command, Detail: action.Detail}
}

func formatProviderDetectReport(report providerDetectReport) string {
	lines := []string{"Detected local runtimes:"}
	if len(report.DetectedRuntimes) == 0 {
		lines = append(lines, "  (none running on default ports)")
	}
	for _, runtime := range report.DetectedRuntimes {
		name := strings.TrimSpace(runtime.Name)
		if name == "" {
			name = runtime.CatalogID
		}
		lines = append(lines, "  "+name+" — "+runtime.BaseURL)
		if len(runtime.Models) > 0 {
			lines = append(lines, "    models: "+strings.Join(runtime.Models, ", "))
		}
		if command := strings.TrimSpace(runtime.Action.Command); command != "" {
			lines = append(lines, "    "+runtime.Action.Label+": "+command)
		} else if detail := strings.TrimSpace(runtime.Action.Detail); detail != "" {
			lines = append(lines, "    "+runtime.Action.Label+": "+detail)
		}
	}

	lines = append(lines, "", "Configured providers:")
	if len(report.Providers) == 0 {
		lines = append(lines, "  (none configured — run `zero setup`)")
	}
	for _, provider := range report.Providers {
		name := strings.TrimSpace(provider.Name)
		if name == "" {
			name = "(unnamed)"
		}
		marker := ""
		if provider.Active {
			marker = " (active)"
		}
		lines = append(lines, "  "+name+marker)
		if len(provider.Actions) == 0 {
			lines = append(lines, "    (ready)")
		}
		for _, action := range provider.Actions {
			line := "    " + action.Label
			if command := strings.TrimSpace(action.Command); command != "" {
				line += ": " + command
			}
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "\n")
}
