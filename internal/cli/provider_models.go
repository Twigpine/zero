package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/Twigpine/zero/internal/config"
	"github.com/Twigpine/zero/internal/providercatalog"
	"github.com/Twigpine/zero/internal/providermodelcatalog"
	"github.com/Twigpine/zero/internal/providermodeldiscovery"
	"github.com/Twigpine/zero/internal/providers"
	"github.com/Twigpine/zero/internal/specialist"
)

type providerModelsOptions struct {
	name string
	json bool
	// verify asks the provider to actually RUN each model, rather than trusting
	// that listing it means it works.
	verify bool
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

	// PROVED, not merely listed. /v1/models is an advertisement: every model that
	// has broken a real plan appeared on it looking ordinary — one refused at
	// chat completions, one belonging to a provider the session had switched away
	// from, two that fail every task they touch. Only a real request settles it.
	verdicts := map[string]specialist.ModelProbeResult{}
	if options.verify {
		workspaceRoot, wderr := deps.getwd()
		if wderr != nil {
			return writeAppError(stderr, "resolve working directory: "+wderr.Error(), exitCrash)
		}
		probe := planModelProber(workspaceRoot, profile, deps.newProvider)
		if probe == nil {
			return writeAppError(stderr, "this build cannot open provider connections, so nothing can be proved", exitCrash)
		}
		results := make([]specialist.ModelProbeResult, len(models))
		var wait sync.WaitGroup
		// CONCURRENT: nineteen models one after another is a minute of staring at
		// nothing, and they are independent.
		for index, model := range models {
			wait.Add(1)
			go func(index int, id string) {
				defer wait.Done()
				results[index] = probe(ctx, id)
			}(index, model.ID)
		}
		wait.Wait()
		for index, model := range models {
			verdicts[model.ID] = results[index]
		}
	}

	if options.json {
		items := make([]map[string]any, 0, len(models))
		for _, model := range models {
			entry := map[string]any{"id": model.ID}
			if description := strings.TrimSpace(model.Description); description != "" {
				entry["description"] = description
			}
			if verdict, ok := verdicts[model.ID]; ok {
				entry["status"] = probeStatusWord(verdict.Verdict)
				if reason := strings.TrimSpace(verdict.Reason); reason != "" {
					entry["details"] = reason
				}
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
	var serves, refuses, unreachable int
	for _, model := range models {
		line := strings.TrimSpace(model.ID)
		// The size where the id names one, so the directory reads as light/medium/
		// heavy at a glance — the classification routing uses, made visible. Absent
		// for a cloud id that carries none.
		if label := specialist.ModelSizeLabel(model.ID); label != "" {
			line += " [" + label + "]"
		}
		if verdict, ok := verdicts[model.ID]; ok {
			// The VERDICT LEADS, because it is the reason to run this at all: a
			// reader scanning for what is wrong should not have to reach the end
			// of each line to find out.
			switch verdict.Verdict {
			case specialist.ProbeServes:
				serves++
				line = "ok       " + line
			case specialist.ProbeRefuses:
				refuses++
				line = "REFUSES  " + line
			default:
				unreachable++
				line = "?        " + line
			}
			if verdict.Verdict != specialist.ProbeServes {
				if reason := strings.TrimSpace(verdict.Reason); reason != "" {
					line += "  — " + firstProbeLine(reason)
				}
			}
		} else if description := strings.TrimSpace(model.Description); description != "" {
			line += " — " + description
		}
		if _, err := fmt.Fprintln(stdout, line); err != nil {
			return exitCrash
		}
	}
	if options.verify {
		if _, err := fmt.Fprintf(stdout, "\n%d serve, %d refuse, %d could not be reached.\n",
			serves, refuses, unreachable); err != nil {
			return exitCrash
		}
		if refuses > 0 {
			// The point of --verify, stated plainly: these are the ids that would
			// otherwise be found mid-plan, one dead task at a time.
			if _, err := fmt.Fprintln(stdout, "A model that REFUSES is listed by this provider but will not run. "+
				"Plans skip these automatically — nothing needs excluding by hand."); err != nil {
				return exitCrash
			}
		}
		if unreachable > 0 {
			if _, err := fmt.Fprintln(stdout, "A model that could not be reached is left in place: "+
				"that is a fact about the connection, not about the model."); err != nil {
				return exitCrash
			}
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

// livePlanProvider answers WHICH provider this plan's models should be listed
// from, at the moment the plan runs rather than at the moment the tool was built.
//
// THE CAPTURED PROFILE GOES STALE. The orchestrate tool is wired once, at
// startup, from the provider active then; the TUI's /model picker can switch
// provider mid-session, and switchProviderModel re-points the parent client and
// exports ZERO_PROVIDER so spawned children follow. Nothing re-pointed this. The
// result was a session running xai/grok-4.5 whose plan was handed nineteen Ollama
// model ids — the router assigned them, the children were dispatched with them,
// and the provider rejected every one.
//
// So it re-resolves through the SAME path a freshly spawned child takes:
// config plus ZERO_PROVIDER. Discovery and child execution now read one source of
// truth, which is stronger than keeping two in sync — they cannot drift apart
// again because there is no longer a second place to update.
//
// THE CAPTURED PROFILE WINS WHEN THE NAME MATCHES, and that is not an
// optimisation. It carries what re-resolution cannot see: --provider, --base-url
// and an inline --api-key are applied by the caller ON TOP of Resolve, so a
// headless run that never switches must keep exactly the profile it was given.
// Only a genuine change of provider — the one case that broke — takes the new one.
func livePlanProvider(workspaceRoot string, captured config.ProviderProfile) config.ProviderProfile {
	options, err := config.DefaultResolveOptions(workspaceRoot)
	if err != nil {
		return captured
	}
	// Nothing to re-resolve FROM, which is not the same as a failure and is why
	// the error branches cannot catch it.
	//
	// With no config file on either path, no provider command and no
	// ZERO_PROVIDER, config.Resolve still SUCCEEDS: it falls back to a built-in
	// default provider. That default is not a decision anyone made. Treating it
	// as a provider switch discards a captured profile carrying --provider,
	// --base-url and an inline --api-key, and points discovery at the default
	// endpoint with none of the caller's credentials — this function's own
	// failure mode arriving from the other direction. A headless run on a machine
	// with no config file, a fresh CI container being the obvious one, hits it
	// every time.
	//
	// Read off the OPTIONS rather than by stat-ing again: DefaultResolveOptions
	// already leaves a path empty when the file is absent, so this uses the
	// answer it computed instead of asking a second time and risking a different
	// one.
	//
	// ZERO_PROVIDER is deliberately NOT part of this test, though it looks like
	// it belongs. With no config file there are no profiles to select from, so a
	// ZERO_PROVIDER naming one resolves to ErrNoActiveProvider and the error
	// branch below already returns the captured profile. Adding it here would
	// read as a meaningful case while changing no outcome.
	if strings.TrimSpace(options.UserConfigPath) == "" &&
		strings.TrimSpace(options.ProjectConfigPath) == "" &&
		strings.TrimSpace(options.ProviderCommand) == "" {
		return captured
	}
	// Env deliberately left nil: config.Resolve then reads the live process
	// environment, which is where switchProviderModel wrote ZERO_PROVIDER.
	resolved, err := config.Resolve(options)
	if err != nil {
		return captured
	}
	if strings.TrimSpace(resolved.Provider.Name) == "" ||
		strings.EqualFold(strings.TrimSpace(resolved.Provider.Name), strings.TrimSpace(captured.Name)) {
		return captured
	}
	return resolved.Provider
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
		case arg == "--verify":
			options.verify = true
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

// planModelDiscoverer adapts provider discovery for the orchestrate tool's
// auto_assign.
//
// THE ADAPTER LIVES HERE, not in specialist, and that is the point of the
// narrower type. internal/specialist runs on the child-execution path; importing
// config and providercatalog to describe a model would drag the whole provider
// stack in behind it. The surface that already holds a provider profile does the
// translation and hands over the four facts a plan actually chooses between.
func planModelDiscoverer(workspaceRoot string, profile config.ProviderProfile) specialist.ModelDiscoverer {
	return func(ctx context.Context) ([]specialist.DiscoveredModel, error) {
		live := livePlanProvider(workspaceRoot, profile)
		found, err := defaultDiscoverProviderModels(ctx, discoveryCredentialProfile(live))
		if err != nil {
			return nil, err
		}
		// The LIVE profile's catalog id, not the captured one: discovery already
		// follows an in-session provider switch, and filtering against the
		// launch-time provider would reintroduce the disagreement
		// livePlanProvider exists to remove.
		return planModelsFromDiscovered(live.CatalogID, found), nil
	}
}

// planModelsFromDiscovered narrows a provider's list to the models a plan may
// actually be assigned, and translates them into specialist's own type.
//
// FILTERED THROUGH THE SAME ALLOW-LIST THE CLIENT WILL APPLY.
// defaultDiscoverProviderModels deliberately returns a provider's whole list —
// right for `providers models`, which exists to show what is there, and which
// applies this same filter itself before printing. A plan does not display these,
// it RUNS them, and providers.New refuses a model outside its provider's scope
// (validateModelAllowedForProvider). So the unfiltered list handed the router
// candidates that could never be dispatched, and every task assigned one died at
// client creation — the same shape as assigning from the wrong provider's list
// entirely, and just as invisible until dispatch.
func planModelsFromDiscovered(catalogID string, found []providermodeldiscovery.Model) []specialist.DiscoveredModel {
	catalogID = strings.TrimSpace(catalogID)
	out := make([]specialist.DiscoveredModel, 0, len(found))
	for _, model := range found {
		if !providermodelcatalog.ModelIDAllowedForProvider(catalogID, model.ID) {
			continue
		}
		out = append(out, specialist.DiscoveredModel{
			ID:               model.ID,
			Description:      model.Description,
			ToolCall:         model.ToolCall,
			Reasoning:        model.Reasoning,
			InputCost:        model.InputCost,
			OutputCost:       model.OutputCost,
			OutputModalities: model.OutputModalities,
		})
	}
	return out
}

// planModelPreferences carries the configured per-role pins and exclusions into
// specialist's own narrow type, so that package keeps its distance from config.
func planModelPreferences(cfg config.PlanModelsConfig) specialist.ModelPreferences {
	return specialist.ModelPreferences{
		Scan:           cfg.Scan,
		Implement:      cfg.Implement,
		Verify:         cfg.Verify,
		Exclude:        cfg.Exclude,
		Router:         cfg.Router,
		RouterGuidance: cfg.RouterGuidance,
		AutoAssign:     cfg.AutoAssign,
		MinSize:        cfg.MinSize,
		TopModels:      cfg.TopModels,
	}
}

// planContextWindows reports the context window of the model a plan task will
// run on, 0 when unknown.
//
// FROM THE CATALOGUE, NOT FROM DISCOVERY. The window is static metadata about a
// model, so asking the provider for it over the network per task would pay a
// round trip for an answer that cannot change — and most of this catalogue's
// gateways do not report a window at all, in which case 0 is the honest answer
// and the plan keeps its fixed briefing caps.
//
// AN EMPTY MODEL ID MEANS "the model this run is using". A plan task that names
// no model inherits the parent's, and only this side of the seam knows what that
// is — internal/specialist deliberately does not.
func planContextWindows(profile config.ProviderProfile, sessionModel string) specialist.ContextWindowFunc {
	var once sync.Once
	windows := map[string]int{}
	return func(modelID string) int {
		once.Do(func() {
			descriptor, err := providercatalog.Require(profile.CatalogID)
			if err != nil {
				return
			}
			for _, model := range providermodelcatalog.Models(descriptor) {
				if model.ContextWindow > 0 {
					windows[model.ID] = model.ContextWindow
				}
			}
		})
		if strings.TrimSpace(modelID) == "" {
			modelID = sessionModel
		}
		return windows[strings.TrimSpace(modelID)]
	}
}
