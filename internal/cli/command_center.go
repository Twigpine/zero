package cli

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/Gitlawb/zero/internal/config"
	"github.com/Gitlawb/zero/internal/modelregistry"
	"github.com/Gitlawb/zero/internal/zerocommands"
)

type commandCenterOptions struct {
	json              bool
	provider          string
	transport         string
	includeDeprecated bool
}

type configSummary = zerocommands.ConfigSnapshot
type providerSummary = zerocommands.ProviderSnapshot
type modelSummary = zerocommands.ModelSnapshot
type providerCatalogSummary = zerocommands.ProviderCatalogSnapshot

func runConfig(args []string, stdout io.Writer, stderr io.Writer, deps appDeps) int {
	options, help, err := parseCommandCenterArgs(args, false, false)
	if err != nil {
		return writeExecUsageError(stderr, err.Error())
	}
	if help {
		if err := writeConfigHelp(stdout); err != nil {
			return exitCrash
		}
		return exitSuccess
	}

	resolved, exitCode := resolveCommandCenterConfig(stderr, deps)
	if exitCode != exitSuccess {
		return exitCode
	}
	summary := summarizeConfig(resolved)
	if options.json {
		if err := writePrettyJSON(stdout, summary); err != nil {
			return exitCrash
		}
		return exitSuccess
	}
	if _, err := fmt.Fprintln(stdout, formatConfigSummary(summary)); err != nil {
		return exitCrash
	}
	return exitSuccess
}

func runProviders(args []string, stdout io.Writer, stderr io.Writer, deps appDeps) int {
	command := "list"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		command = strings.ToLower(strings.TrimSpace(args[0]))
		args = args[1:]
	}
	if command == "help" {
		if err := writeProvidersHelp(stdout); err != nil {
			return exitCrash
		}
		return exitSuccess
	}
	if command == "add" {
		return runProvidersAdd(args, stdout, stderr, deps)
	}
	if command == "check" {
		return runProvidersCheck(args, stdout, stderr, deps)
	}
	if command == "use" {
		return runProvidersUse(args, stdout, stderr, deps)
	}
	if command == "remove" || command == "rm" {
		return runProvidersRemove(args, stdout, stderr, deps)
	}
	if command == "rename" {
		return runProvidersRename(args, stdout, stderr, deps)
	}
	if command == "setup" {
		return runProvidersSetup(args, stdout, stderr, deps)
	}
	if command == "detect" {
		return runProvidersDetect(args, stdout, stderr, deps)
	}
	if command == "models" {
		return runProvidersModels(args, stdout, stderr, deps)
	}
	if command != "list" && command != "current" && command != "catalog" {
		return writeExecUsageError(stderr, fmt.Sprintf("unknown providers command %q", command))
	}
	options, help, err := parseCommandCenterArgs(args, false, command == "catalog")
	if err != nil {
		return writeExecUsageError(stderr, err.Error())
	}
	if help {
		if err := writeProvidersHelp(stdout); err != nil {
			return exitCrash
		}
		return exitSuccess
	}
	if command == "catalog" {
		catalog, err := listProviderCatalogSummaries(options)
		if err != nil {
			return writeExecUsageError(stderr, err.Error())
		}
		if options.json {
			if err := writePrettyJSON(stdout, map[string]any{"providers": catalog}); err != nil {
				return exitCrash
			}
			return exitSuccess
		}
		if _, err := fmt.Fprintln(stdout, formatProviderCatalogSummaries(catalog)); err != nil {
			return exitCrash
		}
		return exitSuccess
	}

	resolved, exitCode := resolveCommandCenterConfig(stderr, deps)
	if exitCode != exitSuccess {
		return exitCode
	}
	summary := summarizeConfig(resolved)
	providers := summary.Providers
	if command == "current" {
		providers = []providerSummary{}
		for _, provider := range summary.Providers {
			if provider.Active {
				providers = append(providers, provider)
				break
			}
		}
	}
	if options.json {
		if command == "current" {
			var provider any
			if len(providers) > 0 {
				provider = providers[0]
			}
			if err := writePrettyJSON(stdout, map[string]any{"provider": provider}); err != nil {
				return exitCrash
			}
			return exitSuccess
		}
		if err := writePrettyJSON(stdout, map[string]any{"providers": providers}); err != nil {
			return exitCrash
		}
		return exitSuccess
	}
	if _, err := fmt.Fprintln(stdout, formatProviderSummaries(command, providers)); err != nil {
		return exitCrash
	}
	return exitSuccess
}

func runModels(args []string, stdout io.Writer, stderr io.Writer) int {
	if len(args) > 0 && (args[0] == "list" || args[0] == "ls") {
		args = args[1:]
	}
	options, help, err := parseCommandCenterArgs(args, true, false)
	if err != nil {
		return writeExecUsageError(stderr, err.Error())
	}
	if help {
		if err := writeModelsHelp(stdout); err != nil {
			return exitCrash
		}
		return exitSuccess
	}

	registry, err := modelregistry.DefaultRegistry()
	if err != nil {
		return writeAppError(stderr, err.Error(), exitCrash)
	}
	models, err := listModelSummaries(registry, options)
	if err != nil {
		return writeExecUsageError(stderr, err.Error())
	}
	if options.json {
		if err := writePrettyJSON(stdout, map[string]any{"models": models}); err != nil {
			return exitCrash
		}
		return exitSuccess
	}
	if _, err := fmt.Fprintln(stdout, formatModelSummaries(models)); err != nil {
		return exitCrash
	}
	return exitSuccess
}

func resolveCommandCenterConfig(stderr io.Writer, deps appDeps) (config.ResolvedConfig, int) {
	workspaceRoot, err := resolveWorkspaceRoot("", deps)
	if err != nil {
		return config.ResolvedConfig{}, writeExecUsageError(stderr, err.Error())
	}
	resolved, err := deps.resolveConfig(workspaceRoot, config.Overrides{})
	if err != nil {
		return config.ResolvedConfig{}, writeAppError(stderr, err.Error(), exitProvider)
	}
	return resolved, exitSuccess
}

func parseCommandCenterArgs(args []string, allowModelFilters bool, allowProviderCatalogFilters bool) (commandCenterOptions, bool, error) {
	options := commandCenterOptions{}
	for index := 0; index < len(args); index++ {
		arg := args[index]
		switch {
		case arg == "-h" || arg == "--help" || arg == "help":
			return options, true, nil
		case arg == "--json":
			options.json = true
		case allowModelFilters && arg == "--include-deprecated":
			options.includeDeprecated = true
		case allowModelFilters && arg == "--provider":
			value, next, err := nextFlagValue(args, index, arg)
			if err != nil {
				return options, false, err
			}
			options.provider = value
			index = next
		case allowModelFilters && strings.HasPrefix(arg, "--provider="):
			value, err := requiredInlineFlagValue(arg, "--provider")
			if err != nil {
				return options, false, err
			}
			options.provider = value
		case allowProviderCatalogFilters && arg == "--transport":
			value, next, err := nextFlagValue(args, index, arg)
			if err != nil {
				return options, false, err
			}
			options.transport = value
			index = next
		case allowProviderCatalogFilters && strings.HasPrefix(arg, "--transport="):
			value, err := requiredInlineFlagValue(arg, "--transport")
			if err != nil {
				return options, false, err
			}
			options.transport = value
		case strings.HasPrefix(arg, "-"):
			return options, false, execUsageError{fmt.Sprintf("unknown flag %q", arg)}
		default:
			return options, false, execUsageError{fmt.Sprintf("unexpected argument %q", arg)}
		}
	}
	return options, false, nil
}

func summarizeConfig(resolved config.ResolvedConfig) configSummary {
	summary := zerocommands.ConfigSnapshotFromResolved(resolved)
	// Mark keyless profiles that authenticate via a stored OAuth login (e.g.
	// ChatGPT) so the list shows "oauth login" instead of "api key: not set" —
	// the same candidate matching the runtime resolver uses.
	logins := oauthLoggedInProviders()
	if len(logins) > 0 {
		for index := range summary.Providers {
			for _, profile := range resolved.Providers {
				if profile.Name == summary.Providers[index].Name {
					summary.Providers[index].OAuthLogin = providerHasOAuthLogin(profile, logins)
					break
				}
			}
		}
	}
	sort.SliceStable(summary.Providers, func(i int, j int) bool {
		if summary.Providers[i].Active != summary.Providers[j].Active {
			return summary.Providers[i].Active
		}
		return summary.Providers[i].Name < summary.Providers[j].Name
	})
	return summary
}

func listModelSummaries(registry modelregistry.Registry, options commandCenterOptions) ([]modelSummary, error) {
	summaries, err := zerocommands.ModelSnapshots(registry, zerocommands.ModelSnapshotOptions{
		Provider:          modelregistry.ProviderKind(strings.TrimSpace(strings.ToLower(options.provider))),
		IncludeDeprecated: options.includeDeprecated,
	})
	if err != nil {
		return nil, execUsageError{err.Error()}
	}
	sort.SliceStable(summaries, func(i int, j int) bool {
		if summaries[i].Provider == summaries[j].Provider {
			return summaries[i].ID < summaries[j].ID
		}
		return summaries[i].Provider < summaries[j].Provider
	})
	return summaries, nil
}

func listProviderCatalogSummaries(options commandCenterOptions) ([]providerCatalogSummary, error) {
	summaries, err := zerocommands.ProviderCatalogSnapshots(zerocommands.ProviderCatalogSnapshotOptions{
		Transport: options.transport,
	})
	if err != nil {
		return nil, err
	}
	sort.SliceStable(summaries, func(i int, j int) bool {
		if summaries[i].Recommended != summaries[j].Recommended {
			return summaries[i].Recommended
		}
		return summaries[i].ID < summaries[j].ID
	})
	return summaries, nil
}

func formatConfigSummary(summary configSummary) string {
	lines := []string{
		"Config",
		"runtime: " + summary.Runtime,
		"active provider: " + displayCLIValue(summary.ActiveProvider, "none"),
		fmt.Sprintf("max turns: %d", summary.MaxTurns),
		"providers:",
	}
	if len(summary.Providers) == 0 {
		lines = append(lines, "  (none)")
	}
	for _, provider := range summary.Providers {
		lines = append(lines, "  "+formatProviderLine(provider))
	}
	return strings.Join(lines, "\n")
}

func formatProviderSummaries(command string, providers []providerSummary) string {
	title := "Providers"
	if command == "current" {
		title = "Provider"
	}
	lines := []string{title}
	if len(providers) == 0 {
		lines = append(lines, "  (none)")
		return strings.Join(lines, "\n")
	}
	for _, provider := range providers {
		if command == "current" {
			lines = append(lines,
				"name: "+displayCLIValue(provider.Name, "none"),
				"kind: "+displayCLIValue(provider.ProviderKind, "unknown"),
				"model: "+displayCLIValue(provider.Model, "none"),
				"api model: "+displayCLIValue(provider.APIModel, "unknown"),
				"base url: "+displayCLIValue(provider.BaseURL, "default"),
				"api key: "+providerCredentialState(provider),
			)
			if provider.Message != "" {
				lines = append(lines, "status: "+provider.Status+" - "+provider.Message)
			}
			continue
		}
		lines = append(lines, "  "+formatProviderLine(provider))
	}
	return strings.Join(lines, "\n")
}

func formatProviderLine(provider providerSummary) string {
	marker := " "
	if provider.Active {
		marker = "*"
	}
	line := fmt.Sprintf("%s %s [%s] model=%s apiModel=%s api key: %s", marker, displayCLIValue(provider.Name, "none"), displayCLIValue(provider.ProviderKind, "unknown"), displayCLIValue(provider.Model, "none"), displayCLIValue(provider.APIModel, "unknown"), providerCredentialState(provider))
	if provider.Message != "" {
		line += " (" + provider.Status + ": " + provider.Message + ")"
	}
	return line
}

func formatProviderCatalogSummaries(providers []providerCatalogSummary) string {
	lines := []string{"Provider Catalog"}
	if len(providers) == 0 {
		lines = append(lines, "  (none)")
		return strings.Join(lines, "\n")
	}
	for _, provider := range providers {
		lines = append(lines, formatProviderCatalogLine(provider))
	}
	return strings.Join(lines, "\n")
}

func formatProviderCatalogLine(provider providerCatalogSummary) string {
	name := formatProviderCatalogValue(provider.Name, "unknown")
	prefix := "  id="
	if provider.Recommended {
		prefix = "  ★ id="
		name += " (recommended)"
	}
	lines := []string{
		fmt.Sprintf("%s%s name=%s",
			prefix,
			formatProviderCatalogValue(provider.ID, "unknown"),
			name,
		),
		fmt.Sprintf("    transport=%s defaultModel=%s",
			formatProviderCatalogValue(provider.Transport, "unknown"),
			formatProviderCatalogValue(provider.DefaultModel, "none"),
		),
		"    defaultBaseURL=" + formatProviderCatalogValue(provider.DefaultBaseURL, "none"),
	}
	if provider.RequiresAuth {
		lines = append(lines, "    authEnvVars="+formatProviderCatalogValue(strings.Join(provider.AuthEnvVars, ","), "none"))
	}
	lines = append(lines, fmt.Sprintf("    requiresAuth=%t local=%t runtimeSupported=%t",
		provider.RequiresAuth,
		provider.Local,
		provider.RuntimeSupported,
	))
	if provider.RuntimeSupported {
		setup := "    setup: zero providers setup " + displayCLIValue(provider.ID, "unknown") + " --set-active"
		if provider.ID == "atomic-chat-local" {
			setup = "    setup: run zero setup to select a loaded model, or zero providers detect to get an add command"
		}
		lines = append(lines, setup)
	} else {
		lines = append(lines, "    unsupported: "+displayCLIValue(provider.RuntimeUnsupportedReason, "unknown"))
	}
	return strings.Join(lines, "\n")
}

func formatModelSummaries(models []modelSummary) string {
	lines := []string{"Models"}
	if len(models) == 0 {
		lines = append(lines, "  (none)")
		return strings.Join(lines, "\n")
	}
	for _, model := range models {
		lines = append(lines, fmt.Sprintf("  %s [%s] ctx=%d out=%d - %s", model.ID, model.Provider, model.ContextWindow, model.MaxOutputTokens, model.DisplayName))
	}
	return strings.Join(lines, "\n")
}

func apiKeyState(set bool) string {
	if set {
		return "set"
	}
	return "not set"
}

// providerCredentialState renders the credential column: a configured key wins,
// then a stored OAuth login, then "not set".
func providerCredentialState(provider providerSummary) string {
	if !provider.APIKeySet && provider.OAuthLogin {
		return "oauth login"
	}
	return apiKeyState(provider.APIKeySet)
}

func displayCLIValue(value string, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func formatProviderCatalogValue(value string, fallback string) string {
	value = displayCLIValue(value, fallback)
	for _, r := range value {
		if unicode.IsSpace(r) || unicode.IsControl(r) || r == '"' {
			return strconv.Quote(value)
		}
	}
	return value
}

func writeConfigHelp(w io.Writer) error {
	_, err := fmt.Fprint(w, `Usage:
  zero config [flags]

Inspects resolved Go configuration without printing secrets.

Flags:
      --json      Print JSON summary
  -h, --help      Show this help
`)
	return err
}

func writeModelsHelp(w io.Writer) error {
	_, err := fmt.Fprint(w, `Usage:
  zero models list [flags]

Lists Zero model registry entries.

Flags:
      --json                  Print JSON model list
      --provider <provider>   Filter by openai, anthropic, google, or openai-compatible
      --include-deprecated    Include deprecated models
  -h, --help                  Show this help
`)
	return err
}

func writeProvidersHelp(w io.Writer) error {
	_, err := fmt.Fprint(w, `Usage:
  zero providers current [flags]
  zero providers list [flags]
  zero providers catalog [flags]
  zero providers add <catalog-id> [flags]
  zero providers check [name] [flags]
  zero providers use <name> [flags]
  zero providers remove <name> [flags]
  zero providers rename <old> <new> [flags]
  zero providers setup <catalog-id> [flags]
  zero providers detect [flags]
  zero providers models [name] [flags]

Inspects resolved provider profiles and provider catalog descriptors without printing secrets.
Detect probes for running local runtimes (Ollama, LM Studio, Atomic Chat) and prints adopt commands plus per-provider next steps.
Models probes a provider's live model-listing endpoint (e.g. an OpenAI-compatible /v1/models) and lists the models it serves — including custom OpenAI-/Anthropic-compatible endpoints — so a self-hosted provider needs no per-model config.

Flags:
      --json                    Print JSON summary
      --connectivity            Probe provider endpoint for providers check
      --transport <transport>   Filter catalog descriptors by transport

Add flags:
      --name <name>             Saved provider profile name
      --model <model>           Override catalog default model
      --base-url <url>          Override catalog default base URL
      --api-key-env <name>      Environment variable that contains the API key
      --auth-header <header>    Custom API-key header name
      --auth-scheme <scheme>    Auth scheme prefix, for example Bearer or Token
      --auth-header-value <v>   Exact auth header value; stored in config
      --header <key=value>      Custom provider header; repeatable
      --set-active              Make the added provider active

Setup flags:
      --name <name>             Planned provider profile name
      --model <model>           Planned model override
      --base-url <url>          Planned base URL override
      --api-key-env <name>      Planned API key environment variable
      --set-active              Include --set-active in the add command
  -h, --help                    Show this help
`)
	return err
}
