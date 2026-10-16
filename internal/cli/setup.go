package cli

import (
	"fmt"
	"io"
	"net"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"

	"github.com/Gitlawb/zero/internal/config"
	"github.com/Gitlawb/zero/internal/oauth"
	"github.com/Gitlawb/zero/internal/providercatalog"
	"github.com/Gitlawb/zero/internal/providerhealth"
	"github.com/Gitlawb/zero/internal/provideronboarding"
	"github.com/Gitlawb/zero/internal/tui"
)

type setupOptions struct {
	catalogID string
	name      string
	model     string
	baseURL   string
	apiKeyEnv string
	json      bool
	// verify runs a live connectivity probe after saving the provider and reports
	// a specific, fixable error on failure (wrong base URL, bad key, model not
	// found) — the first-run "paste key -> working" confirmation.
	verify bool
}

func runSetup(args []string, stdout io.Writer, stderr io.Writer, deps appDeps) int {
	options, help, err := parseSetupArgs(args)
	if err != nil {
		return writeExecUsageError(stderr, err.Error())
	}
	if help {
		if err := writeSetupHelp(stdout); err != nil {
			return exitCrash
		}
		return exitSuccess
	}
	if strings.TrimSpace(options.catalogID) == "" {
		return runInteractiveTUIWithSetup(stderr, deps, "", nil, "", true, false)
	}

	result, err := saveSetupProvider(deps, tui.SetupSelection{
		CatalogID: options.catalogID,
		Model:     options.model,
	}, setupSaveOptions{
		name:      options.name,
		baseURL:   options.baseURL,
		apiKeyEnv: options.apiKeyEnv,
	})
	if err != nil {
		return writeAppError(stderr, err.Error(), exitCrash)
	}

	var verification *setupVerification
	if options.verify {
		verified, probeErr := verifySetupProvider(deps, result.Provider)
		verification = &verified
		if probeErr != nil {
			// A failed probe is reported as a specific, fixable provider error — the
			// profile is already saved, so the message tells the user the one thing to
			// change and re-run, never a stack trace.
			if options.json {
				if err := writePrettyJSON(stdout, setupJSONPayload(result, verification)); err != nil {
					return exitCrash
				}
			} else {
				if _, err := fmt.Fprintln(stdout, formatSetupComplete(result)); err != nil {
					return exitCrash
				}
			}
			return writeAppError(stderr, "setup verification failed: "+probeErr.Error(), exitProvider)
		}
	}

	if options.json {
		if err := writePrettyJSON(stdout, setupJSONPayload(result, verification)); err != nil {
			return exitCrash
		}
		return exitSuccess
	}
	output := formatSetupComplete(result)
	if verification != nil && verification.Ran && verification.OK {
		output += "\nverified: " + verification.Summary
	}
	if _, err := fmt.Fprintln(stdout, output); err != nil {
		return exitCrash
	}
	return exitSuccess
}

// setupVerification is the outcome of the optional first-run connectivity probe.
// Ran distinguishes "the probe ran and passed" (OK) from "no probe was wired so
// nothing was verified", so a skipped probe is never reported as verified.
type setupVerification struct {
	Ran     bool
	OK      bool
	Summary string
}

// verifySetupProvider runs a live connectivity probe against the just-saved
// provider and classifies any failure into a specific, fixable message. When no
// probe is wired it reports a skipped (not-run) state rather than success, so it
// never blocks setup in a context that cannot probe and never claims a provider
// is verified when nothing was checked.
func verifySetupProvider(deps appDeps, profile config.ProviderProfile) (setupVerification, error) {
	if deps.probeProviderHealth == nil {
		return setupVerification{Ran: false, Summary: "probe unavailable; skipped"}, nil
	}
	if profile.APIKeyStored && strings.TrimSpace(profile.APIKey) == "" {
		configPath, err := deps.userConfigPath()
		if err != nil {
			return setupVerification{Ran: true, Summary: "stored api key unavailable"}, err
		}
		store, err := config.ProviderKeyStoreAt(filepath.Dir(configPath))
		if err != nil {
			return setupVerification{Ran: true, Summary: "stored api key unavailable"}, err
		}
		key, ok, err := store.Get(strings.TrimSpace(profile.Name))
		if err != nil {
			return setupVerification{Ran: true, Summary: "stored api key unavailable"}, fmt.Errorf("load stored API key: %w", err)
		}
		if ok && strings.TrimSpace(key) != "" {
			profile.APIKey = key
		}
	}
	// Distinguish "no key configured" from "key rejected": probing a remote provider
	// with no credential yields a generic "the provider rejected the API key", which
	// misleads a user who simply hasn't exported a key yet. Keyless local providers
	// (loopback base_url) legitimately need no key, so they still probe. A profile may
	// carry its key indirectly via APIKeyEnv (the setup result isn't env-resolved yet),
	// so treat a populated env var as having a credential. (AUDIT-M1)
	if !profileHasCredential(profile) && !baseURLIsLoopback(profile.BaseURL) {
		name := strings.TrimSpace(profile.Name)
		if name == "" {
			name = "this provider"
		}
		return setupVerification{Ran: true, OK: false, Summary: "no api key"},
			fmt.Errorf("no API key found for %s — set its API key (export the provider's API key env var, or pass --api-key-env) or run `zero auth`, then re-run setup", name)
	}
	ctx, stop := signalContext()
	defer stop()
	result := deps.probeProviderHealth(ctx, providerhealth.Options{
		Profile:      profile,
		Connectivity: true,
		UserAgent:    userAgent(),
	})
	if probeErr, failed := provideronboarding.ClassifySetupProbe(result); failed {
		return setupVerification{Ran: true, OK: false, Summary: string(probeErr.Class)}, probeErr
	}
	summary := "provider endpoint reachable"
	if check := result.PrimaryCheck(); check != nil && strings.TrimSpace(check.Message) != "" {
		summary = strings.TrimSpace(check.Message)
	}
	return setupVerification{Ran: true, OK: true, Summary: summary}, nil
}

func setupJSONPayload(result tui.SetupResult, verification *setupVerification) map[string]any {
	payload := map[string]any{
		"configPath": result.ConfigPath,
		"provider":   result.Provider.Name,
		"model":      result.Provider.Model,
		"catalogID":  result.Provider.CatalogID,
	}
	if verification != nil {
		// Only emit the machine-readable verified flag when a probe actually ran, so
		// a skipped probe is never reported as a passing verification.
		if verification.Ran {
			payload["verified"] = verification.OK
		}
		if verification.Summary != "" {
			payload["verifyStatus"] = verification.Summary
		}
	}
	return payload
}

func parseSetupArgs(args []string) (setupOptions, bool, error) {
	options := setupOptions{}
	for index := 0; index < len(args); index++ {
		arg := args[index]
		switch {
		case arg == "-h" || arg == "--help" || arg == "help":
			return options, true, nil
		case arg == "--json":
			options.json = true
		case arg == "--verify":
			options.verify = true
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
	return options, false, nil
}

type setupSaveOptions struct {
	name      string
	baseURL   string
	apiKeyEnv string
}

func saveSetupProvider(deps appDeps, selection tui.SetupSelection, options setupSaveOptions) (tui.SetupResult, error) {
	profile, err := providerProfileForAdd(providerAddOptions{
		catalogID: selection.CatalogID,
		name:      firstNonEmptyCLI(options.name, selection.Name),
		model:     firstNonEmptyCLI(selection.Model),
		baseURL:   firstNonEmptyCLI(options.baseURL, selection.BaseURL),
		apiKeyEnv: options.apiKeyEnv,
		setActive: true,
	})
	if err != nil {
		return tui.SetupResult{}, err
	}
	if apiKey := strings.TrimSpace(selection.APIKey); apiKey != "" {
		profile.APIKey = apiKey
		profile.APIKeyEnv = ""
	}
	configPath, err := deps.userConfigPath()
	if err != nil {
		return tui.SetupResult{}, err
	}
	committed, err := config.CommitProviderProfile(configPath, config.ProviderCommit{
		Profile:   profile,
		SetActive: true,
	})
	if err != nil {
		return tui.SetupResult{}, err
	}
	return tui.SetupResult{ConfigPath: configPath, Provider: committed.Persisted}, nil
}

func setupProviderOptions() []tui.SetupProviderOption {
	descriptors := providercatalog.All()
	options := make([]tui.SetupProviderOption, 0, len(descriptors))
	for _, descriptor := range descriptors {
		if !providercatalog.RuntimeSupported(descriptor) {
			continue
		}
		options = append(options, tui.SetupProviderOption{
			ID:           descriptor.ID,
			Name:         descriptor.Name,
			DefaultModel: descriptor.DefaultModel,
			EnvVar:       setupProviderEnvVar(descriptor),
			RequiresAuth: descriptor.RequiresAuth,
			Local:        descriptor.Local,
			Recommended:  descriptor.Recommended,
		})
	}
	return options
}

func setupProviderEnvVar(descriptor providercatalog.Descriptor) string {
	for _, envVar := range descriptor.AuthEnvVars {
		if envVar = strings.TrimSpace(envVar); envVar != "" {
			return envVar
		}
	}
	return ""
}

func setupRequired(resolved config.ResolvedConfig) bool {
	if !config.HasProviderProfile(resolved.Provider) {
		return true
	}
	if _, missing := setupMissingCredentialEnv(resolved.Provider); !missing {
		return false
	}
	// A stored OAuth login (e.g. `zero auth login xai`) is a credential too, even
	// though the profile has no inline key / env var — so a logged-in provider
	// must not trigger onboarding.
	return !providerHasOAuthLogin(resolved.Provider, oauthLoggedInProviders())
}

// providerHasOAuthLogin reports whether a stored OAuth login exists for the
// provider. It uses the same ProviderProfile.OAuthLoginCandidates the runtime
// resolver does, so the "authenticated in the UI" gate and the "authenticated at
// runtime" resolver can never diverge. This is only consulted for a profile with
// no usable API-key credential (setupRequired returns early otherwise), where
// the candidate set is permissive (profile name + catalog ID).
func providerHasOAuthLogin(profile config.ProviderProfile, oauthLogins map[string]bool) bool {
	for _, name := range profile.OAuthLoginCandidates() {
		// Store keys are normalized to lower case (oauth.ProviderKey), so the
		// presence check must compare the same way.
		if oauthLogins[strings.ToLower(strings.TrimSpace(name))] {
			return true
		}
	}
	return false
}

// oauthLoggedInProviders returns the set of provider names that have a stored
// OAuth token, so credential checks recognize an OAuth login (not just inline
// keys / env vars). Errors degrade to an empty set (no logins).
func oauthLoggedInProviders() map[string]bool {
	out := map[string]bool{}
	store, err := oauth.NewStore(oauth.StoreOptions{})
	if err != nil {
		return out
	}
	statuses, err := store.Status(oauth.KeyPrefixProvider)
	if err != nil {
		return out
	}
	for _, status := range statuses {
		if status.HasToken {
			out[strings.ToLower(strings.TrimPrefix(status.Key, oauth.KeyPrefixProvider))] = true
		}
	}
	return out
}

// firstUsableProvider returns the saved provider best suited to run without
// onboarding: the first usable (inline credential, stored OAuth login, or
// no-auth/local) non-local provider, else the first usable local one. It lets
// the CLI fall back to an already-configured login when the active provider
// happens to lack a credential, instead of re-running onboarding every launch.
func firstUsableProvider(providers []config.ProviderProfile) (config.ProviderProfile, bool) {
	logins := oauthLoggedInProviders()
	var localFallback config.ProviderProfile
	haveLocal := false
	for _, profile := range providers {
		if !config.HasProviderProfile(profile) {
			continue
		}
		// A profile whose catalog entry no longer resolves AND that has no explicit
		// BaseURL has no endpoint to talk to, so it cannot become a working
		// provider — skip it rather than picking a fallback that fails at first use.
		// (A stale CatalogID with a BaseURL still works as a custom endpoint.)
		if catalogID := strings.TrimSpace(profile.CatalogID); catalogID != "" && strings.TrimSpace(profile.BaseURL) == "" {
			if _, err := providercatalog.Require(catalogID); err != nil {
				continue
			}
		}
		// A stored OAuth login (e.g. `zero auth login xai`) is a credential too, even
		// when the profile has no inline key / env var — mirrors setupRequired and
		// usableSavedProviders so this fallback doesn't force onboarding for a
		// provider the user is already authenticated with.
		if _, missing := setupMissingCredentialEnv(profile); missing && !providerHasOAuthLogin(profile, logins) {
			continue
		}
		if providerProfileIsLocal(profile) {
			if !haveLocal {
				localFallback = profile
				haveLocal = true
			}
			continue
		}
		return profile, true
	}
	if haveLocal {
		return localFallback, true
	}
	return config.ProviderProfile{}, false
}

// usableSavedProviders filters configured providers to those the user can actually
// use: an inline/stored/env key, an auth header, a stored OAuth login, or a no-auth
// local provider. This keeps /model from listing providers that are merely present
// in config.json but never authenticated (e.g. a default openai entry with no key).
func usableSavedProviders(providers []config.ProviderProfile) []config.ProviderProfile {
	logins := oauthLoggedInProviders()
	store, storeErr := config.ProviderKeyStore()
	usable := make([]config.ProviderProfile, 0, len(providers))
	for _, profile := range providers {
		if !config.HasProviderProfile(profile) {
			continue
		}
		// A provider whose only credential is the APIKeyStored marker counts as usable
		// only when the key is actually retrievable — the keyring/file entry may have
		// been deleted, leaving a stale marker that would otherwise list it in /model.
		if profile.APIKeyStored && strings.TrimSpace(profile.APIKey) == "" {
			if storeErr == nil {
				if key, ok, err := store.Get(profile.Name); err == nil && ok && strings.TrimSpace(key) != "" {
					usable = append(usable, profile)
					continue
				}
			}
			// Marker present but key missing/unreadable: fall through and judge the
			// profile on its other signals (env var, OAuth, local) with the marker off.
			withoutMarker := profile
			withoutMarker.APIKeyStored = false
			if _, missing := setupMissingCredentialEnv(withoutMarker); !missing {
				usable = append(usable, profile)
			} else if providerHasOAuthLogin(profile, logins) {
				usable = append(usable, profile)
			}
			continue
		}
		if _, missing := setupMissingCredentialEnv(profile); !missing {
			usable = append(usable, profile)
			continue
		}
		if providerHasOAuthLogin(profile, logins) {
			usable = append(usable, profile)
		}
	}
	return usable
}

// providerProfileIsLocal reports whether a provider points at a local endpoint
// (a loopback URL or a catalog entry flagged Local), so the fallback prefers a
// remote keyed provider that is more likely reachable.
func providerProfileIsLocal(profile config.ProviderProfile) bool {
	if catalogID := strings.TrimSpace(profile.CatalogID); catalogID != "" {
		if descriptor, err := providercatalog.Require(catalogID); err == nil && descriptor.Local {
			return true
		}
	}
	base := strings.TrimSpace(profile.BaseURL)
	if base == "" {
		return false
	}
	parsed, err := url.Parse(base)
	if err != nil {
		return false
	}
	switch strings.ToLower(parsed.Hostname()) {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	// Also treat explicit private-network IPs (192.168.x.y, 10.x.y.z,
	// 172.16-31.x.y) as local — the user is pointing at their own LAN box.
	if ip := net.ParseIP(parsed.Hostname()); ip != nil && ip.IsPrivate() {
		return true
	}
	return false
}

func formatSetupComplete(result tui.SetupResult) string {
	lines := []string{"Zero setup complete"}
	if result.Provider.Name != "" {
		lines = append(lines, "provider: "+result.Provider.Name)
	}
	if result.Provider.Model != "" {
		lines = append(lines, "model: "+result.Provider.Model)
	}
	if result.ConfigPath != "" {
		lines = append(lines, "config: "+result.ConfigPath)
	}
	if envVar, ok := setupMissingCredentialEnv(result.Provider); ok {
		if envVar != "" {
			lines = append(lines, "next: set "+envVar+" in your shell")
		} else {
			lines = append(lines, "next: set provider credentials in your shell")
		}
	}
	lines = append(lines, "next: "+setupCheckCommand(result.Provider.Name), "next: zero")
	lines = append(lines, "try this: "+setupTryThisExample(result.Provider))
	return strings.Join(lines, "\n")
}

// setupTryThisExample returns a concrete one-line headless run the user can paste
// to confirm a real completion, parameterized by the model just configured. This
// is the "end the wizard in a working state" example from the first-run spec.
func setupTryThisExample(profile config.ProviderProfile) string {
	example := `zero exec "say hello in one short sentence"`
	if model := strings.TrimSpace(profile.Model); model != "" {
		example += " --model " + setupCommandArg(model)
	}
	return example
}

// setupMissingCredentialEnv reports the environment variable a saved profile
// is expected to read an API key from, and whether that expectation is
// unmet. It delegates to config.ProviderProfile.MissingCredentialEnv, the
// single source of truth shared with the TUI's provider status/wizard
// surfaces and the CLI's `providers check` readiness gate, so all three never
// disagree about whether a saved provider is missing a credential.
func setupMissingCredentialEnv(profile config.ProviderProfile) (string, bool) {
	return profile.MissingCredentialEnv()
}

func setupCheckCommand(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "zero providers check --connectivity"
	}
	return "zero providers check " + setupCommandArg(name) + " --connectivity"
}

func setupCommandArg(value string) string {
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

func writeSetupHelp(w io.Writer) error {
	_, err := fmt.Fprint(w, `Usage:
  zero setup [provider] [flags]

Guides first-run Zero setup. Without a provider, opens the setup UI. With a
provider catalog id, writes that provider as the active provider.

Examples:
  zero setup
  zero setup openai --api-key-env OPENAI_API_KEY
  zero setup ollama
  zero setup ollama-cloud

Flags:
      --name <name>             Provider profile name
      --model <model>           Override the default model
      --base-url <url>          Override provider base URL
      --api-key-env <name>      Store an API key environment variable name
      --verify                  Probe the provider after saving and report a fixable error on failure
      --json                    Print machine-readable setup result
  -h, --help                    Show this help
`)
	return err
}
