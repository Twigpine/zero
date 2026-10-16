package cli

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode"

	"github.com/Gitlawb/zero/internal/config"
	"github.com/Gitlawb/zero/internal/providercatalog"
	"github.com/Gitlawb/zero/internal/provideronboarding"
	"github.com/Gitlawb/zero/internal/redaction"
)

type providerUseOptions struct {
	name string
	json bool
}

type providerSetupOptions struct {
	catalogID string
	name      string
	model     string
	baseURL   string
	apiKeyEnv string
	setActive bool
	json      bool
}

type providerSetupPlan struct {
	CatalogID    string `json:"catalogID"`
	Name         string `json:"name"`
	AddCommand   string `json:"addCommand"`
	CheckCommand string `json:"checkCommand"`
	UseCommand   string `json:"useCommand"`
	EnvVar       string `json:"envVar"`
}

type providerRepairOptions struct {
	name string
	json bool
}

func runProvidersUse(args []string, stdout io.Writer, stderr io.Writer, deps appDeps) int {
	options, help, err := parseProviderUseArgs(args)
	if err != nil {
		return writeExecUsageError(stderr, err.Error())
	}
	if help {
		if err := writeProvidersHelp(stdout); err != nil {
			return exitCrash
		}
		return exitSuccess
	}

	configPath, err := deps.userConfigPath()
	if err != nil {
		return writeAppError(stderr, err.Error(), exitCrash)
	}
	// SetActiveProvider only ever matches profiles persisted in config.json
	// (see config.ProviderPersisted), but a provider can be visible in
	// `zero providers list`/the TUI picker purely because Resolve()
	// synthesized it in-memory from an ambient env var (e.g. OPENAI_API_KEY)
	// without ever writing a row to disk. Without this check, switching to
	// that provider by name always fails with a confusing "not found" even
	// though it is genuinely usable this session (issue #707).
	persisted, err := config.ProviderPersisted(configPath, options.name)
	if err != nil {
		return writeAppError(stderr, err.Error(), exitCrash)
	}
	if !persisted {
		if exit, handled := reportUnpersistedProviderUse(stdout, stderr, deps, options, configPath); handled {
			return exit
		}
	}
	cfg, err := config.SetActiveProvider(configPath, options.name)
	if err != nil {
		return writeAppError(stderr, err.Error(), exitCrash)
	}

	override := activeProviderEnvOverride(deps.getenv, cfg.ActiveProvider)
	if options.json {
		payload := map[string]any{
			"activeProvider": cfg.ActiveProvider,
			"configPath":     configPath,
		}
		if override != "" {
			// A JSON consumer must not read this as an effective switch either.
			payload["effectiveProvider"] = override
			payload["overriddenByEnv"] = config.ActiveProviderEnv
		}
		if err := writePrettyJSON(stdout, payload); err != nil {
			return exitCrash
		}
		return exitSuccess
	}
	if _, err := fmt.Fprintf(stdout, "Active provider set to %s\nnext: %s\n", cfg.ActiveProvider, providerCheckCommand(cfg.ActiveProvider, false)); err != nil {
		return exitCrash
	}
	if override != "" {
		if _, err := fmt.Fprintf(stderr, "Note: %s=%s is set and overrides config.json, so %s stays the active provider until you unset %s.\n", config.ActiveProviderEnv, override, override, config.ActiveProviderEnv); err != nil {
			return exitCrash
		}
	}
	return exitSuccess
}

// activeProviderEnvOverride returns the ZERO_PROVIDER value when it is set and
// names a DIFFERENT provider than the one just selected, meaning the saved
// `providers use` selection will NOT be the effective active provider until the
// env var is unset. applyEnv (resolver.go) makes ZERO_PROVIDER win over
// config.json unconditionally, so reporting the write as a plain success reads as
// a switch that silently has no effect (issue #721). Empty when nothing overrides
// (including when getenv is nil, e.g. a test that did not inject the environment).
func activeProviderEnvOverride(getenv func(string) string, selected string) string {
	if getenv == nil {
		return ""
	}
	override := strings.TrimSpace(getenv(config.ActiveProviderEnv))
	if override == "" || config.SameProviderIdentity(override, selected) {
		return ""
	}
	return override
}

func runProvidersSetup(args []string, stdout io.Writer, stderr io.Writer, deps appDeps) int {
	options, help, err := parseProviderSetupArgs(args)
	if err != nil {
		return writeExecUsageError(stderr, err.Error())
	}
	if help {
		if err := writeProvidersHelp(stdout); err != nil {
			return exitCrash
		}
		return exitSuccess
	}

	profile, err := providerProfileForAdd(providerAddOptions{
		catalogID: options.catalogID,
		name:      options.name,
		model:     options.model,
		baseURL:   options.baseURL,
		apiKeyEnv: options.apiKeyEnv,
		setActive: options.setActive,
	})
	if err != nil {
		return writeExecUsageError(stderr, err.Error())
	}
	descriptor, err := providercatalog.Require(profile.CatalogID)
	if err != nil {
		return writeExecUsageError(stderr, err.Error())
	}
	plan := providerSetupPlan{
		CatalogID:    profile.CatalogID,
		Name:         profile.Name,
		AddCommand:   providerSetupAddCommand(options, profile),
		CheckCommand: providerCheckCommand(profile.Name, true),
		EnvVar:       providerSetupEnvVar(descriptor, profile),
	}
	if !options.setActive {
		plan.UseCommand = providerUseCommand(profile.Name)
	}

	if options.json {
		if err := writePrettyJSON(stdout, plan); err != nil {
			return exitCrash
		}
		return exitSuccess
	}
	if _, err := fmt.Fprintln(stdout, formatProviderSetupPlan(plan)); err != nil {
		return exitCrash
	}
	return exitSuccess
}

func parseProviderUseArgs(args []string) (providerUseOptions, bool, error) {
	options := providerUseOptions{}
	for index := 0; index < len(args); index++ {
		arg := args[index]
		switch {
		case arg == "-h" || arg == "--help" || arg == "help":
			return options, true, nil
		case arg == "--json":
			options.json = true
		case strings.HasPrefix(arg, "-"):
			return options, false, execUsageError{fmt.Sprintf("unknown flag %q", arg)}
		default:
			if options.name != "" {
				return options, false, execUsageError{fmt.Sprintf("unexpected argument %q", arg)}
			}
			options.name = arg
		}
	}
	if strings.TrimSpace(options.name) == "" {
		return options, false, execUsageError{"provider name is required"}
	}
	return options, false, nil
}

func parseProviderSetupArgs(args []string) (providerSetupOptions, bool, error) {
	options := providerSetupOptions{}
	for index := 0; index < len(args); index++ {
		arg := args[index]
		switch {
		case arg == "-h" || arg == "--help" || arg == "help":
			return options, true, nil
		case arg == "--json":
			options.json = true
		case arg == "--set-active":
			options.setActive = true
		case arg == "--name":
			value, next, err := nextFlagValue(args, index, arg)
			if err != nil {
				return options, false, err
			}
			options.name = value
			index = next
		case strings.HasPrefix(arg, "--name="):
			value, err := requiredInlineFlagValue(arg, "--name")
			if err != nil {
				return options, false, err
			}
			options.name = value
		case arg == "--model":
			value, next, err := nextFlagValue(args, index, arg)
			if err != nil {
				return options, false, err
			}
			options.model = value
			index = next
		case strings.HasPrefix(arg, "--model="):
			value, err := requiredInlineFlagValue(arg, "--model")
			if err != nil {
				return options, false, err
			}
			options.model = value
		case arg == "--base-url":
			value, next, err := nextFlagValue(args, index, arg)
			if err != nil {
				return options, false, err
			}
			options.baseURL = value
			index = next
		case strings.HasPrefix(arg, "--base-url="):
			value, err := requiredInlineFlagValue(arg, "--base-url")
			if err != nil {
				return options, false, err
			}
			options.baseURL = value
		case arg == "--api-key-env":
			value, next, err := nextFlagValue(args, index, arg)
			if err != nil {
				return options, false, err
			}
			options.apiKeyEnv = value
			index = next
		case strings.HasPrefix(arg, "--api-key-env="):
			value, err := requiredInlineFlagValue(arg, "--api-key-env")
			if err != nil {
				return options, false, err
			}
			options.apiKeyEnv = value
		case strings.HasPrefix(arg, "-"):
			return options, false, execUsageError{fmt.Sprintf("unknown flag %q", arg)}
		default:
			if options.catalogID != "" {
				return options, false, execUsageError{fmt.Sprintf("unexpected argument %q", arg)}
			}
			options.catalogID = arg
		}
	}
	if strings.TrimSpace(options.catalogID) == "" {
		return options, false, execUsageError{"provider catalog id is required"}
	}
	return options, false, nil
}

func providerSetupAddCommand(options providerSetupOptions, profile config.ProviderProfile) string {
	parts := []string{"zero", "providers", "add", profile.CatalogID}
	if strings.TrimSpace(options.name) != "" {
		parts = append(parts, "--name", options.name)
	}
	if strings.TrimSpace(options.model) != "" {
		parts = append(parts, "--model", options.model)
	}
	if strings.TrimSpace(options.baseURL) != "" {
		parts = append(parts, "--base-url", options.baseURL)
	}
	if apiKeyEnv := firstNonEmptyCLI(options.apiKeyEnv, profile.APIKeyEnv); apiKeyEnv != "" {
		parts = append(parts, "--api-key-env", apiKeyEnv)
	}
	if options.setActive {
		parts = append(parts, "--set-active")
	}
	return providerCommand(parts...)
}

func providerSetupEnvVar(descriptor providercatalog.Descriptor, profile config.ProviderProfile) string {
	if !descriptor.RequiresAuth || descriptor.Local {
		return ""
	}
	if envVar := strings.TrimSpace(profile.APIKeyEnv); envVar != "" {
		return envVar
	}
	for _, envVar := range descriptor.AuthEnvVars {
		if envVar = strings.TrimSpace(envVar); envVar != "" {
			return envVar
		}
	}
	return ""
}

func formatProviderSetupPlan(plan providerSetupPlan) string {
	lines := []string{"Provider setup plan"}
	if plan.EnvVar != "" {
		lines = append(lines, "Set "+plan.EnvVar+" to your API key before running connectivity checks.")
	}
	lines = append(lines, "Next commands:", "  "+plan.AddCommand, "  "+plan.CheckCommand)
	if plan.UseCommand != "" {
		lines = append(lines, "  "+plan.UseCommand)
	}
	return strings.Join(lines, "\n")
}

func providerUseCommand(name string) string {
	return provideronboarding.UseCommand(name)
}

func providerCheckCommand(name string, connectivity bool) string {
	return provideronboarding.CheckCommand(name, connectivity)
}

func providerCommand(parts ...string) string {
	quoted := make([]string, 0, len(parts))
	for _, part := range parts {
		if part = strings.TrimSpace(part); part != "" {
			quoted = append(quoted, providerCommandArg(part))
		}
	}
	return strings.Join(quoted, " ")
}

func providerCommandArg(value string) string {
	if value == "" {
		return strconv.Quote(value)
	}
	for _, r := range value {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			continue
		}
		switch r {
		case '-', '_', '.', '/', ':', '@':
			continue
		default:
			return strconv.Quote(value)
		}
	}
	return value
}

type providerNamesOptions struct {
	names []string
	json  bool
}

func parseProviderNamesArgs(args []string, want int, usage string) (providerNamesOptions, bool, error) {
	options := providerNamesOptions{}
	for _, arg := range args {
		switch {
		case arg == "-h" || arg == "--help" || arg == "help":
			return options, true, nil
		case arg == "--json":
			options.json = true
		case strings.HasPrefix(arg, "-"):
			return options, false, execUsageError{fmt.Sprintf("unknown flag %q", arg)}
		default:
			options.names = append(options.names, arg)
		}
	}
	if len(options.names) != want {
		return options, false, execUsageError{usage}
	}
	return options, false, nil
}

func runProvidersRepairConfig(args []string, stdout io.Writer, stderr io.Writer, deps appDeps) int {
	options, help, err := parseProviderRepairArgs(args)
	if err != nil {
		return writeExecUsageError(stderr, err.Error())
	}
	if help {
		if err := writeProvidersHelp(stdout); err != nil {
			return exitCrash
		}
		return exitSuccess
	}
	configPath, err := deps.userConfigPath()
	if err != nil {
		return writeAppError(stderr, redaction.ErrorMessage(err, redaction.Options{}), exitCrash)
	}
	// repaired comes from the repair itself. Re-deriving the defaulting rules
	// here reported activeProvider as the repaired name whenever that value
	// already belonged to a different row and the fallback had actually named
	// this one — the command said "Named legacy provider Groq" about a row it
	// had named "openai".
	_, repaired, err := config.RepairUnnamedProvider(configPath, options.name)
	if err != nil {
		return writeAppError(stderr, redaction.ErrorMessage(err, redaction.Options{}), exitCrash)
	}
	if options.json {
		if err := writePrettyJSON(stdout, map[string]any{"repairedProvider": repaired, "configPath": configPath}); err != nil {
			return exitCrash
		}
		return exitSuccess
	}
	if _, err := fmt.Fprintf(stdout, "Named legacy provider %s in %s\n", repaired, configPath); err != nil {
		return exitCrash
	}
	return exitSuccess
}

func parseProviderRepairArgs(args []string) (providerRepairOptions, bool, error) {
	options := providerRepairOptions{}
	for index := 0; index < len(args); index++ {
		arg := args[index]
		switch {
		case arg == "-h" || arg == "--help" || arg == "help":
			return options, true, nil
		case arg == "--json":
			options.json = true
		case arg == "--name":
			value, next, err := nextFlagValue(args, index, arg)
			if err != nil {
				return options, false, err
			}
			options.name = value
			index = next
		case strings.HasPrefix(arg, "--name="):
			value, err := requiredInlineFlagValue(arg, "--name")
			if err != nil {
				return options, false, err
			}
			options.name = value
		case strings.HasPrefix(arg, "-"):
			return options, false, execUsageError{fmt.Sprintf("unknown flag %q", arg)}
		default:
			return options, false, execUsageError{fmt.Sprintf("unexpected argument %q", arg)}
		}
	}
	return options, false, nil
}

// runProvidersRemove deletes a saved provider profile and its stored API key.
// The OAuth token (if any) is kept — logins outlive profiles so re-adding the
// provider needs no new browser round-trip; `zero auth logout <name>` removes it.
func runProvidersRemove(args []string, stdout io.Writer, stderr io.Writer, deps appDeps) int {
	options, help, err := parseProviderNamesArgs(args, 1, "usage: zero providers remove <name>")
	if err != nil {
		return writeExecUsageError(stderr, err.Error())
	}
	if help {
		if err := writeProvidersHelp(stdout); err != nil {
			return exitCrash
		}
		return exitSuccess
	}
	configPath, err := deps.userConfigPath()
	if err != nil {
		return writeAppError(stderr, err.Error(), exitCrash)
	}
	name := options.names[0]
	// RemoveProvider only ever matches profiles persisted in config.json (see
	// config.ProviderPersisted), but a provider can be visible in
	// `zero providers list`/the TUI picker purely because Resolve()
	// synthesized it in-memory from an ambient env var (e.g. OPENAI_API_KEY)
	// without ever writing a row to disk. Without this check, deleting that
	// provider by name always fails with a confusing "not found" even though
	// it is genuinely visible/usable this session (issue #707).
	persisted, err := config.ProviderPersisted(configPath, name)
	if err != nil {
		return writeAppError(stderr, err.Error(), exitCrash)
	}
	if !persisted {
		if exit, handled := reportUnpersistedProviderRemove(stdout, stderr, deps, name, options.json, configPath); handled {
			return exit
		}
	}
	// ProviderPersisted above answers a credential-identity question, but
	// RemoveProvider targets a row by its exact spelling. Bridge the two, so
	// `zero providers remove work` against a sole saved "WORK" row removes it
	// instead of failing "not found" right after the persisted check passed.
	name, err = resolveProviderMutationName(configPath, name, deps)
	if err != nil {
		return writeAppError(stderr, redaction.ErrorMessage(err, redaction.Options{}), exitCrash)
	}
	cfg, err := config.RemoveProvider(configPath, name)
	if err != nil {
		return writeAppError(stderr, err.Error(), exitCrash)
	}
	// Provider credentials are user-scoped, so delete from the same default user
	// store runtime lookup uses. A surviving case variant that still claims the
	// credential keeps it (see config.CredentialKeyRetained); a survivor that
	// never claimed it does not.
	keyRemoved, keyErr := false, error(nil)
	if !config.CredentialKeyRetained(cfg.Providers, name) {
		keyRemoved, keyErr = config.ForgetProviderKey(name)
	}
	if options.json {
		payload := map[string]any{
			"removed":        name,
			"keyRemoved":     keyRemoved,
			"activeProvider": cfg.ActiveProvider,
			"configPath":     configPath,
		}
		if keyErr != nil {
			// A lingering secret must not read as a clean removal.
			payload["keyError"] = redaction.ErrorMessage(keyErr, redaction.Options{})
		}
		if err := writePrettyJSON(stdout, payload); err != nil {
			return exitCrash
		}
		if keyErr != nil {
			return exitCrash
		}
		return exitSuccess
	}
	if _, err := fmt.Fprintf(stdout, "Removed provider %s\n", name); err != nil {
		return exitCrash
	}
	if keyErr != nil {
		if _, err := fmt.Fprintf(stderr, "warning: its stored API key could not be deleted and remains in the credential store: %s\n", redaction.ErrorMessage(keyErr, redaction.Options{})); err != nil {
			return exitCrash
		}
		return exitCrash
	} else if keyRemoved {
		if _, err := fmt.Fprintln(stdout, "Deleted its stored API key."); err != nil {
			return exitCrash
		}
	}
	if active := strings.TrimSpace(cfg.ActiveProvider); active != "" {
		if _, err := fmt.Fprintf(stdout, "Active provider: %s\n", active); err != nil {
			return exitCrash
		}
	} else {
		if _, err := fmt.Fprintln(stdout, "No providers remain — run zero setup to add one."); err != nil {
			return exitCrash
		}
	}
	return exitSuccess
}

// runProvidersRename renames a saved provider profile, migrating its stored
// API key and the activeProvider pointer along with it (config.RenameProvider).
func runProvidersRename(args []string, stdout io.Writer, stderr io.Writer, deps appDeps) int {
	options, help, err := parseProviderNamesArgs(args, 2, "usage: zero providers rename <old> <new>")
	if err != nil {
		return writeExecUsageError(stderr, err.Error())
	}
	if help {
		if err := writeProvidersHelp(stdout); err != nil {
			return exitCrash
		}
		return exitSuccess
	}
	configPath, err := deps.userConfigPath()
	if err != nil {
		return writeAppError(stderr, err.Error(), exitCrash)
	}
	oldName := options.names[0]
	persisted, err := config.ProviderPersisted(configPath, oldName)
	if err != nil {
		return writeAppError(stderr, err.Error(), exitCrash)
	}
	if !persisted {
		if exit, handled := reportUnpersistedProviderRename(stdout, stderr, deps, oldName, options.json, configPath); handled {
			return exit
		}
	}
	// Same bridge as remove: the persisted check matches credential identity
	// while RenameProvider matches the row's exact spelling.
	oldName, err = resolveProviderMutationName(configPath, oldName, deps)
	if err != nil {
		return writeAppError(stderr, redaction.ErrorMessage(err, redaction.Options{}), exitCrash)
	}
	cfg, err := config.RenameProvider(configPath, oldName, options.names[1])
	if err != nil {
		return writeAppError(stderr, err.Error(), exitCrash)
	}
	if options.json {
		if err := writePrettyJSON(stdout, map[string]any{
			"renamed":        map[string]string{"from": oldName, "to": options.names[1]},
			"activeProvider": cfg.ActiveProvider,
			"configPath":     configPath,
		}); err != nil {
			return exitCrash
		}
		return exitSuccess
	}
	if _, err := fmt.Fprintf(stdout, "Renamed provider %s to %s\n", oldName, options.names[1]); err != nil {
		return exitCrash
	}
	return exitSuccess
}

// A spelling copied from providers list can identify a project/env row, not an
// alias for a user row. Exact persisted requests stay usable for repairing an
// otherwise invalid config; only a fallback needs cross-layer resolution.
func resolveProviderMutationName(path, name string, deps appDeps) (string, error) {
	persisted, err := config.ResolvePersistedProviderName(path, name)
	if err != nil || persisted == strings.TrimSpace(name) {
		return persisted, err
	}
	root, err := resolveWorkspaceRoot("", deps)
	if err != nil {
		return "", err
	}
	options, err := config.DefaultResolveOptions(root)
	if err != nil {
		return "", err
	}
	// Use the same user file the mutation will write, including injected paths.
	options.UserConfigPath = path
	names, err := config.ResolveProviderSourceNames(options)
	if err != nil {
		return "", err
	}
	row, lookup := config.LookupProviderName(names, name)
	if lookup == config.ProviderNameExact {
		owner, err := config.ProviderRowOwnershipAt(path, names, row)
		if err != nil {
			return "", err
		}
		if !owner.UserBacked {
			return "", fmt.Errorf("%s; change that provider in its project configuration or environment instead", owner.Reason)
		}
	}
	return persisted, nil
}

// providerResolvedByName reports whether name matches a provider in a
// resolved provider list — used to tell a genuinely unknown name (a typo)
// apart from a real, env-derived provider that just has no config.json row.
func providerResolvedByName(providers []config.ProviderProfile, name string) bool {
	name = strings.TrimSpace(name)
	for _, provider := range providers {
		if config.SameProviderIdentity(provider.Name, name) {
			return true
		}
	}
	return false
}

// reportUnpersistedProviderUse handles `zero providers use <name>` for a
// provider that is not persisted in config.json. If it's not resolvable at
// all (an unknown/misspelled name), it returns handled=false so the caller
// falls through to SetActiveProvider's real "not found" error. If it IS
// resolvable — an env-derived profile (e.g. ambient OPENAI_API_KEY) —
// SetActiveProvider would only ever fail "not found" against it, so this
// reports the situation plainly instead of that confusing error (issue
// #707).
func reportUnpersistedProviderUse(stdout, stderr io.Writer, deps appDeps, options providerUseOptions, configPath string) (int, bool) {
	resolved, exitCode := resolveProviderManagementConfig(stderr, deps)
	if exitCode != exitSuccess {
		// resolveProviderManagementConfig already wrote its own error to stderr;
		// stop here instead of letting the caller try SetActiveProvider too.
		return exitCode, true
	}
	if !providerResolvedByName(resolved.Providers, options.name) {
		return exitCode, false
	}
	message := fmt.Sprintf(
		"Provider %q is not saved in config.json (likely set via an environment variable), so there is no saved profile to switch to.\nIt is available whenever its environment variable is set, but is only active when selected (for example via ZERO_PROVIDER); unset its environment variable to stop Zero from detecting it automatically.",
		options.name,
	)
	if options.json {
		if err := writePrettyJSON(stdout, map[string]any{
			"activeProvider": resolved.ActiveProvider,
			"configPath":     configPath,
			"persisted":      false,
			"message":        message,
		}); err != nil {
			return exitCrash, true
		}
		return exitSuccess, true
	}
	if _, err := fmt.Fprintln(stdout, message); err != nil {
		return exitCrash, true
	}
	return exitSuccess, true
}

// reportUnpersistedProviderRemove handles `zero providers remove <name>` for
// a provider that is not persisted in config.json, mirroring the TUI
// provider manager's delete handling (internal/tui/provider_manager.go). If
// name isn't resolvable at all, it returns handled=false so the caller falls
// through to RemoveProvider's real "not found" error.
func reportUnpersistedProviderRemove(stdout, stderr io.Writer, deps appDeps, name string, jsonOutput bool, configPath string) (int, bool) {
	resolved, exitCode := resolveProviderManagementConfig(stderr, deps)
	if exitCode != exitSuccess {
		return exitCode, true
	}
	if !providerResolvedByName(resolved.Providers, name) {
		return exitCode, false
	}
	message := fmt.Sprintf(
		"Provider %q is not saved in config.json (likely set via an environment variable) — nothing to remove there.\nUnset its environment variable to stop Zero from detecting it automatically.",
		name,
	)
	if jsonOutput {
		if err := writePrettyJSON(stdout, map[string]any{
			"removed":        "",
			"keyRemoved":     false,
			"activeProvider": resolved.ActiveProvider,
			"configPath":     configPath,
			"persisted":      false,
			"message":        message,
		}); err != nil {
			return exitCrash, true
		}
		return exitSuccess, true
	}
	if _, err := fmt.Fprintln(stdout, message); err != nil {
		return exitCrash, true
	}
	return exitSuccess, true
}

func reportUnpersistedProviderRename(stdout, stderr io.Writer, deps appDeps, name string, jsonOutput bool, configPath string) (int, bool) {
	resolved, exitCode := resolveProviderManagementConfig(stderr, deps)
	if exitCode != exitSuccess {
		return exitCode, true
	}
	if !providerResolvedByName(resolved.Providers, name) {
		return exitCode, false
	}
	message := fmt.Sprintf(
		"Provider %q is not saved in config.json (likely set via an environment variable), so there is no saved profile to rename.",
		name,
	)
	if jsonOutput {
		if err := writePrettyJSON(stdout, map[string]any{
			"renamed":    nil,
			"configPath": configPath,
			"persisted":  false,
			"message":    message,
		}); err != nil {
			return exitCrash, true
		}
		return exitSuccess, true
	}
	if _, err := fmt.Fprintln(stdout, message); err != nil {
		return exitCrash, true
	}
	return exitSuccess, true
}
