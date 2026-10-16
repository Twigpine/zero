package cli

import (
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/Gitlawb/zero/internal/aimlapi"
	"github.com/Gitlawb/zero/internal/config"
	"github.com/Gitlawb/zero/internal/providercatalog"
	"github.com/Gitlawb/zero/internal/providerhealth"
	"github.com/Gitlawb/zero/internal/zerocommands"
)

type providerAddOptions struct {
	catalogID       string
	name            string
	model           string
	baseURL         string
	apiKeyEnv       string
	authHeader      string
	authScheme      string
	authHeaderValue string
	customHeaders   map[string]string
	setActive       bool
	json            bool
}

type providerCheckOptions struct {
	name         string
	json         bool
	connectivity bool
}

func runProvidersAdd(args []string, stdout io.Writer, stderr io.Writer, deps appDeps) int {
	options, help, err := parseProviderAddArgs(args)
	if err != nil {
		return writeExecUsageError(stderr, err.Error())
	}
	if help {
		if err := writeProvidersHelp(stdout); err != nil {
			return exitCrash
		}
		return exitSuccess
	}

	profile, err := providerProfileForAdd(options)
	if err != nil {
		return writeExecUsageError(stderr, err.Error())
	}
	configPath, err := deps.userConfigPath()
	if err != nil {
		return writeAppError(stderr, err.Error(), exitCrash)
	}
	if err := config.PreflightProviderWrite(configPath, profile.Name); err != nil {
		return writeAppError(stderr, err.Error(), exitCrash)
	}
	// Persist with the key moved into the encrypted credential store (capture flip);
	// the local profile keeps the key for the verification build below.
	//
	// Fail-soft capture: the preflight above rules out a validation failure
	// AFTER the store write, but a config write that fails for another reason
	// (permissions, disk full) still leaves a store entry with no apiKeyStored
	// marker. Atomic capture+publish for this path is #894's transaction, not
	// the OpenRouter-style PublishProviderCredential rollback this PR wires.
	cfg, err := config.UpsertProvider(configPath, config.SecureProviderProfile(profile, configPath), options.setActive)
	if err != nil {
		return writeAppError(stderr, err.Error(), exitCrash)
	}

	if options.json {
		if err := writePrettyJSON(stdout, map[string]any{
			"configPath":     configPath,
			"activeProvider": cfg.ActiveProvider,
			"provider":       zerocommands.ProviderSnapshotFromProfile(profile, cfg.ActiveProvider == profile.Name),
		}); err != nil {
			return exitCrash
		}
		return exitSuccess
	}
	if _, err := fmt.Fprintf(stdout, "Added provider %s to %s\n", profile.Name, configPath); err != nil {
		return exitCrash
	}
	return exitSuccess
}

func runProvidersCheck(args []string, stdout io.Writer, stderr io.Writer, deps appDeps) int {
	options, help, err := parseProviderCheckArgs(args)
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
	profile, err := selectProviderForCheck(resolved, options.name)
	if err != nil {
		return writeExecUsageError(stderr, err.Error())
	}
	var health providerhealth.Result
	if options.connectivity {
		ctx, stop := signalContext()
		defer stop()
		health = deps.probeProviderHealth(ctx, providerhealth.Options{
			Profile:      profile,
			Connectivity: true,
			UserAgent:    userAgent(),
		})
	} else {
		if err := validateProviderRuntimeReady(profile); err != nil {
			return writeAppError(stderr, providerCheckErrorMessage(err, profile), exitProvider)
		}
		if _, err := deps.newProvider(profile); err != nil {
			return writeAppError(stderr, err.Error(), exitProvider)
		}
	}

	snapshot := zerocommands.ProviderSnapshotFromProfile(profile, profile.Name == resolved.ActiveProvider)
	if options.json {
		payload := map[string]any{"provider": snapshot, "status": "ok"}
		if options.connectivity {
			payload["health"] = health
			payload["status"] = providerHealthStatusLabel(health.Status)
		}
		if nextActions := providerCheckNextActions(profile, options.connectivity, health); len(nextActions) > 0 {
			payload["nextActions"] = nextActions
		}
		if err := writePrettyJSON(stdout, payload); err != nil {
			return exitCrash
		}
		if options.connectivity && health.Status == providerhealth.StatusFail {
			return exitProvider
		}
		return exitSuccess
	}
	status := "ok"
	if options.connectivity {
		status = providerHealthStatusLabel(health.Status)
	}
	if _, err := fmt.Fprintf(stdout, "Provider check\nname: %s\nstatus: %s\nkind: %s\nmodel: %s\napi model: %s\n", snapshot.Name, status, snapshot.ProviderKind, snapshot.Model, snapshot.APIModel); err != nil {
		return exitCrash
	}
	if options.connectivity {
		if _, err := fmt.Fprintf(stdout, "connectivity: %s\n", health.Status); err != nil {
			return exitCrash
		}
		if check := health.PrimaryCheck(); check != nil {
			if _, err := fmt.Fprintf(stdout, "%s: %s\n", check.ID, check.Message); err != nil {
				return exitCrash
			}
		}
	}
	for _, nextAction := range providerCheckNextActions(profile, options.connectivity, health) {
		if _, err := fmt.Fprintf(stdout, "next: %s\n", nextAction); err != nil {
			return exitCrash
		}
	}
	if options.connectivity && health.Status == providerhealth.StatusFail {
		return exitProvider
	}
	return exitSuccess
}

func providerHealthStatusLabel(status providerhealth.Status) string {
	switch status {
	case providerhealth.StatusFail:
		return "fail"
	case providerhealth.StatusWarn:
		return "warn"
	default:
		return "ok"
	}
}

func providerCheckErrorMessage(err error, profile config.ProviderProfile) string {
	message := err.Error()
	if nextAction := providerCheckMissingKeyNextAction(profile); nextAction != "" {
		message += "\nnext: " + nextAction
	}
	return message
}

func providerCheckNextActions(profile config.ProviderProfile, connectivity bool, health providerhealth.Result) []string {
	name := providerCheckName(profile)
	if !connectivity {
		return []string{fmt.Sprintf("run zero providers check %s --connectivity", name)}
	}
	switch health.Status {
	case providerhealth.StatusFail:
		return []string{fmt.Sprintf("verify the API key, base URL, and model, then rerun zero providers check %s --connectivity", name)}
	case providerhealth.StatusWarn:
		return []string{fmt.Sprintf("review the warning, then rerun zero providers check %s --connectivity", name)}
	default:
		model := strings.TrimSpace(profile.Model)
		if model == "" {
			return []string{"provider is ready"}
		}
		return []string{fmt.Sprintf("run zero exec %q --model %s", "hello", model)}
	}
}

func providerCheckMissingKeyNextAction(profile config.ProviderProfile) string {
	if providerProfileHasCredential(profile) {
		return ""
	}
	apiKeyEnv := providerCheckAPIKeyEnv(profile)
	if apiKeyEnv == "" {
		return ""
	}
	return fmt.Sprintf("set %s and rerun zero providers check %s", apiKeyEnv, providerCheckName(profile))
}

func providerCheckAPIKeyEnv(profile config.ProviderProfile) string {
	if apiKeyEnv := strings.TrimSpace(profile.APIKeyEnv); apiKeyEnv != "" {
		return apiKeyEnv
	}
	if strings.TrimSpace(profile.CatalogID) == "" {
		return ""
	}
	descriptor, err := providercatalog.Require(profile.CatalogID)
	if err != nil || !descriptor.RequiresAuth || !providercatalog.RuntimeSupported(descriptor) {
		return ""
	}
	for _, envVar := range descriptor.AuthEnvVars {
		if envVar = strings.TrimSpace(envVar); envVar != "" {
			return envVar
		}
	}
	return ""
}

func providerCheckName(profile config.ProviderProfile) string {
	if name := strings.TrimSpace(profile.Name); name != "" {
		return name
	}
	if catalogID := strings.TrimSpace(profile.CatalogID); catalogID != "" {
		return catalogID
	}
	if provider := strings.TrimSpace(profile.Provider); provider != "" {
		return provider
	}
	if kind := strings.TrimSpace(string(profile.ProviderKind)); kind != "" {
		return kind
	}
	return "provider"
}

func parseProviderAddArgs(args []string) (providerAddOptions, bool, error) {
	options := providerAddOptions{}
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
		case arg == "--auth-header":
			value, next, err := nextFlagValue(args, index, arg)
			if err != nil {
				return options, false, err
			}
			options.authHeader = value
			index = next
		case strings.HasPrefix(arg, "--auth-header="):
			value, err := requiredInlineFlagValue(arg, "--auth-header")
			if err != nil {
				return options, false, err
			}
			options.authHeader = value
		case arg == "--auth-scheme":
			value, next, err := nextFlagValue(args, index, arg)
			if err != nil {
				return options, false, err
			}
			options.authScheme = value
			index = next
		case strings.HasPrefix(arg, "--auth-scheme="):
			options.authScheme = strings.TrimSpace(strings.TrimPrefix(arg, "--auth-scheme="))
		case arg == "--auth-header-value":
			value, next, err := nextFlagValue(args, index, arg)
			if err != nil {
				return options, false, err
			}
			options.authHeaderValue = value
			index = next
		case strings.HasPrefix(arg, "--auth-header-value="):
			value, err := requiredInlineFlagValue(arg, "--auth-header-value")
			if err != nil {
				return options, false, err
			}
			options.authHeaderValue = value
		case arg == "--header":
			value, next, err := nextFlagValue(args, index, arg)
			if err != nil {
				return options, false, err
			}
			if err := addCustomHeader(&options, value); err != nil {
				return options, false, err
			}
			index = next
		case strings.HasPrefix(arg, "--header="):
			value, err := requiredInlineFlagValue(arg, "--header")
			if err != nil {
				return options, false, err
			}
			if err := addCustomHeader(&options, value); err != nil {
				return options, false, err
			}
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

func parseProviderCheckArgs(args []string) (providerCheckOptions, bool, error) {
	options := providerCheckOptions{}
	for index := 0; index < len(args); index++ {
		arg := args[index]
		switch {
		case arg == "-h" || arg == "--help" || arg == "help":
			return options, true, nil
		case arg == "--json":
			options.json = true
		case arg == "--connectivity":
			options.connectivity = true
		case strings.HasPrefix(arg, "-"):
			return options, false, execUsageError{fmt.Sprintf("unknown flag %q", arg)}
		default:
			if options.name != "" {
				return options, false, execUsageError{fmt.Sprintf("unexpected argument %q", arg)}
			}
			options.name = arg
		}
	}
	return options, false, nil
}

func addCustomHeader(options *providerAddOptions, value string) error {
	key, headerValue, ok := strings.Cut(value, "=")
	key = strings.TrimSpace(key)
	if !ok || key == "" {
		return execUsageError{fmt.Sprintf("invalid header %q; want key=value", value)}
	}
	if options.customHeaders == nil {
		options.customHeaders = map[string]string{}
	}
	options.customHeaders[key] = strings.TrimSpace(headerValue)
	return nil
}

func providerProfileForAdd(options providerAddOptions) (config.ProviderProfile, error) {
	descriptor, err := providercatalog.Require(options.catalogID)
	if err != nil {
		return config.ProviderProfile{}, err
	}
	if !providercatalog.RuntimeSupported(descriptor) {
		return config.ProviderProfile{}, fmt.Errorf("provider %q uses transport %q: %s", descriptor.ID, descriptor.Transport, providercatalog.RuntimeUnsupportedReason(descriptor))
	}
	name := strings.TrimSpace(options.name)
	if name == "" {
		name = descriptor.ID
	}
	apiKeyEnv := strings.TrimSpace(options.apiKeyEnv)
	if apiKeyEnv == "" && len(descriptor.AuthEnvVars) > 0 {
		apiKeyEnv = descriptor.AuthEnvVars[0]
	}
	baseURL := firstNonEmptyCLI(options.baseURL, descriptor.DefaultBaseURL)
	var catalogHeaders map[string]string
	if sameProviderSetupBaseURL(baseURL, descriptor.DefaultBaseURL) {
		catalogHeaders = descriptor.CustomHeaders
		if strings.EqualFold(strings.TrimSpace(descriptor.ID), "aimlapi") {
			catalogHeaders = aimlapi.WithResolvedPartnerHeader(catalogHeaders)
		}
	}
	// Discovery fallback can pass the nonempty catalog placeholder through
	// either CLI setup path. Never persist it as an Atomic Chat model.
	if descriptor.ID == "atomic-chat-local" {
		model := strings.TrimSpace(options.model)
		if model == "" || model == "local-model" {
			return config.ProviderProfile{}, fmt.Errorf("provider %q serves a locally loaded model; pass --model <id> (run `zero providers detect` to see the served model)", descriptor.ID)
		}
	}
	profile := config.ProviderProfile{
		Name:            name,
		ProviderKind:    providerKindForDescriptor(descriptor),
		CatalogID:       descriptor.ID,
		BaseURL:         baseURL,
		APIKeyEnv:       apiKeyEnv,
		APIFormat:       firstAPIFormat(descriptor),
		AuthHeader:      strings.TrimSpace(options.authHeader),
		AuthScheme:      normalizeAuthScheme(options.authScheme),
		AuthHeaderValue: strings.TrimSpace(options.authHeaderValue),
		// Catalog attribution belongs only to the catalog endpoint. Explicit
		// user headers still apply to an override/staging/proxy endpoint.
		CustomHeaders: mergeProviderHeaders(catalogHeaders, options.customHeaders),
		Model:         firstNonEmptyCLI(options.model, descriptor.DefaultModel),
	}
	return profile, nil
}

func sameProviderSetupBaseURL(left string, right string) bool {
	return strings.EqualFold(
		strings.TrimRight(strings.TrimSpace(left), "/"),
		strings.TrimRight(strings.TrimSpace(right), "/"),
	)
}

func selectProviderForCheck(resolved config.ResolvedConfig, name string) (config.ProviderProfile, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		if config.HasProviderProfile(resolved.Provider) {
			return resolved.Provider, nil
		}
		return config.ProviderProfile{}, fmt.Errorf("no active provider configured")
	}
	for _, profile := range resolved.Providers {
		if profile.Name == name {
			return profile, nil
		}
	}
	return config.ProviderProfile{}, fmt.Errorf("provider %q not found", name)
}

func validateProviderRuntimeReady(profile config.ProviderProfile) error {
	// A stored OAuth login (e.g. `zero auth chatgpt`) is a credential too: a
	// keyless token-login profile must pass the readiness check instead of being
	// told to set an API key it will never have.
	if profile.CatalogID != "" {
		descriptor, err := providercatalog.Require(profile.CatalogID)
		if err != nil {
			return err
		}
		if !providercatalog.RuntimeSupported(descriptor) {
			return fmt.Errorf("provider %q uses transport %q: %s", descriptor.ID, descriptor.Transport, providercatalog.RuntimeUnsupportedReason(descriptor))
		}
	}
	if providerHasOAuthLogin(profile, oauthLoggedInProviders()) {
		return nil
	}
	// Delegates to the single source of truth shared with the TUI and the
	// setup wizard, so `providers check` never disagrees with them about
	// whether a saved provider (including a no-auth custom endpoint) is
	// actually missing a credential.
	if envVar, missing := profile.MissingCredentialEnv(); missing {
		if envVar != "" {
			return fmt.Errorf("provider %s requires API key; set %s", profile.Name, envVar)
		}
		return fmt.Errorf("provider %s requires API key", profile.Name)
	}
	return nil
}

func providerProfileHasCredential(profile config.ProviderProfile) bool {
	return profile.HasConfiguredCredential()
}

func providerKindForDescriptor(descriptor providercatalog.Descriptor) config.ProviderKind {
	switch descriptor.Transport {
	case providercatalog.TransportOpenAI:
		return config.ProviderKindOpenAI
	case providercatalog.TransportAnthropic:
		return config.ProviderKindAnthropic
	case providercatalog.TransportAnthropicCompatible:
		return config.ProviderKindAnthropicCompat
	case providercatalog.TransportGoogle:
		return config.ProviderKindGoogle
	case providercatalog.TransportOpenAICompatible:
		return config.ProviderKindOpenAICompatible
	default:
		return config.ProviderKind(strings.ToLower(string(descriptor.Transport)))
	}
}

func firstAPIFormat(descriptor providercatalog.Descriptor) string {
	if descriptor.Transport == providercatalog.TransportOpenAI || descriptor.Transport == providercatalog.TransportOpenAICompatible {
		return string(providercatalog.APIFormatOpenAIChatCompletions)
	}
	if len(descriptor.SupportedAPIFormats) == 0 {
		return ""
	}
	return string(descriptor.SupportedAPIFormats[0])
}

func firstNonEmptyCLI(values ...string) string {
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			return value
		}
	}
	return ""
}

func normalizeAuthScheme(value string) string {
	value = strings.TrimSpace(value)
	if strings.EqualFold(value, "none") || strings.EqualFold(value, "raw") {
		return ""
	}
	return value
}

func copyProviderHeaders(headers map[string]string) map[string]string {
	if headers == nil {
		return nil
	}
	copied := make(map[string]string, len(headers))
	for key, value := range headers {
		copied[key] = value
	}
	return copied
}

func mergeProviderHeaders(base map[string]string, overrides map[string]string) map[string]string {
	merged := copyProviderHeaders(base)
	if len(overrides) == 0 {
		return merged
	}
	if merged == nil {
		merged = map[string]string{}
	}
	for key, value := range overrides {
		// HTTP header names are case-insensitive and request construction
		// canonicalizes them (http.Header.Set), so an override that differs from a
		// base key only in case must replace that key's value in place — otherwise
		// both survive as distinct map entries and race non-deterministically at
		// build time. Keep the existing spelling (the catalog's, which the resolver
		// re-merges verbatim); only a genuinely new header is added as typed.
		if existing, ok := headerKeyFold(merged, key); ok {
			merged[existing] = value
			continue
		}
		merged[key] = value
	}
	return merged
}

// headerKeyFold returns the key in headers whose canonical HTTP form matches key,
// so a case-insensitive override lands on the existing entry instead of adding a
// colliding duplicate.
func headerKeyFold(headers map[string]string, key string) (string, bool) {
	if _, ok := headers[key]; ok {
		return key, true
	}
	canonical := http.CanonicalHeaderKey(key)
	for existing := range headers {
		if http.CanonicalHeaderKey(existing) == canonical {
			return existing, true
		}
	}
	return "", false
}
