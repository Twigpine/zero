package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/Gitlawb/zero/internal/config"
	"github.com/Gitlawb/zero/internal/providermodelcatalog"
	"github.com/Gitlawb/zero/internal/providermodeldiscovery"
	"github.com/Gitlawb/zero/internal/providers"
)

type providerModelsOptions struct {
	name string
	json bool
}

// runProvidersModels lists the models a saved provider actually serves by probing
// its live model-discovery endpoint (e.g. an OpenAI-compatible `/v1/models`). It
// works for custom OpenAI-/Anthropic-compatible providers too: discovery runs off
// the profile's base URL + credentials, so a self-hosted endpoint serving a dozen
// models no longer needs a config object per model — configure the provider once,
// then run any listed model with `zero exec --model <id>` (Zero passes unknown
// model ids through to the provider).
func runProvidersModels(args []string, stdout io.Writer, stderr io.Writer, deps appDeps) int {
	options, help, err := parseProviderModelsArgs(args)
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

	ctx, stop := signalContext()
	defer stop()
	models, err := deps.discoverProviderModels(ctx, discoveryCredentialProfile(profile))
	if err != nil {
		return writeAppError(stderr, err.Error(), exitProvider)
	}

	// Apply provider-specific model filtering for catalog-backed profiles.
	// For example, opencode-go-anthropic-compatible only permits Qwen and
	// MiniMax model IDs; the raw /zen/go/v1/models endpoint returns many more.
	if profile.CatalogID != "" {
		filtered := make([]providermodeldiscovery.Model, 0, len(models))
		for _, model := range models {
			if providermodelcatalog.ModelIDAllowedForProvider(profile.CatalogID, model.ID) {
				filtered = append(filtered, model)
			}
		}
		models = filtered
	}

	if options.json {
		items := make([]map[string]any, 0, len(models))
		for _, model := range models {
			entry := map[string]any{"id": model.ID}
			if description := strings.TrimSpace(model.Description); description != "" {
				entry["description"] = description
			}
			if len(model.ReasoningEfforts) > 0 {
				entry["reasoning_efforts"] = append([]string{}, model.ReasoningEfforts...)
			}
			if effort := strings.TrimSpace(model.DefaultReasoningEffort); effort != "" {
				entry["default_reasoning_effort"] = effort
			}
			if len(model.ServiceTiers) > 0 {
				entry["service_tiers"] = append([]string{}, model.ServiceTiers...)
			}
			if tier := strings.TrimSpace(model.DefaultServiceTier); tier != "" {
				entry["default_service_tier"] = tier
			}
			items = append(items, entry)
		}
		payload := map[string]any{
			"provider": profile.Name,
			"count":    len(items),
			"models":   items,
		}
		if err := writePrettyJSON(stdout, payload); err != nil {
			return exitCrash
		}
		return exitSuccess
	}

	name := strings.TrimSpace(profile.Name)
	if name == "" {
		name = "provider"
	}
	if _, err := fmt.Fprintf(stdout, "Provider models (%s)\n", name); err != nil {
		return exitCrash
	}
	for _, model := range models {
		line := strings.TrimSpace(model.ID)
		if description := strings.TrimSpace(model.Description); description != "" {
			line += " — " + description
		}
		if _, err := fmt.Fprintln(stdout, line); err != nil {
			return exitCrash
		}
	}
	suffix := "s"
	if len(models) == 1 {
		suffix = ""
	}
	if _, err := fmt.Fprintf(stdout, "%d model%s discovered\n", len(models), suffix); err != nil {
		return exitCrash
	}
	if len(models) > 0 {
		if _, err := fmt.Fprintf(stdout, "next: zero exec %q --model %s\n", "hello", setupCommandArg(models[0].ID)); err != nil {
			return exitCrash
		}
	}
	return exitSuccess
}

// discoveryCredentialProfile resolves the profile's API key the same way the
// runtime does — inline, then the stored credential, then the configured env var —
// so a `providers models` probe authenticates exactly like a real request. Mirrors
// discoveredModelContextWindow's credential resolution.
func discoveryCredentialProfile(profile config.ProviderProfile) config.ProviderProfile {
	authed := profile
	if strings.TrimSpace(authed.APIKey) == "" {
		if store, err := config.ProviderKeyStore(); err == nil {
			authed = config.ApplyStoredAPIKey(authed, store)
		}
	}
	if strings.TrimSpace(authed.APIKey) == "" && strings.TrimSpace(authed.APIKeyEnv) != "" {
		authed.APIKey = strings.TrimSpace(os.Getenv(authed.APIKeyEnv))
	}
	return authed
}

// defaultDiscoverProviderModels is the production discovery hook: a live probe of
// the provider's model-listing endpoint with no curated-catalog merge or
// coding-model filtering, so a custom provider's full model list is returned.
func defaultDiscoverProviderModels(ctx context.Context, profile config.ProviderProfile) ([]providermodeldiscovery.Model, error) {
	resolver, loginKey := oauthLoginForProfile(profile)
	return providermodeldiscovery.Discover(ctx, profile, providermodeldiscovery.Options{
		OAuthResolver:        resolver,
		CodexAccountResolver: providers.CodexAccountResolverForLogin(loginKey),
		UserAgent:            userAgent(),
	})
}

func parseProviderModelsArgs(args []string) (providerModelsOptions, bool, error) {
	options := providerModelsOptions{}
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
	return options, false, nil
}
