package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/Gitlawb/zero/internal/config"
	"github.com/Gitlawb/zero/internal/oauth"
	"github.com/Gitlawb/zero/internal/providercatalog"
	"github.com/Gitlawb/zero/internal/provideroauth"
	"github.com/Gitlawb/zero/internal/redaction"
)

// ensureLoginProviderProfile makes a freshly stored OAuth login visible as a
// provider: without a profile in config.json the login shows up nowhere — not in
// `zero providers list`, not in the TUI picker — and `zero providers use <name>`
// fails "not found", so the user's working active provider looks broken while
// the new login looks lost. Returns user-facing guidance lines ("" when the
// login has no catalog entry to scaffold from). Best-effort by design: the
// token is already stored, so a profile-write failure degrades to a warning
// line instead of failing a login that succeeded.
func ensureLoginProviderProfile(deps appDeps, provider string) string {
	if _, ok := providercatalog.Get(provider); !ok {
		// Custom OAuth server without a catalog entry — no endpoint/model defaults
		// to scaffold a profile from; the user wires their own profile.
		return ""
	}
	configPath, err := deps.userConfigPath()
	if err != nil {
		return "warning: login saved, but no provider profile was written: " + err.Error()
	}
	ensured, err := config.EnsureCatalogProvider(configPath, provider)
	if err != nil {
		return "warning: login saved, but no provider profile was written: " + err.Error()
	}
	// Both sides are persisted provider names, so this is a provider-identity
	// question and uses the credential store's rule rather than EqualFold.
	active := config.SameProviderIdentity(ensured.Active, ensured.Name)
	switch {
	case ensured.Created && active:
		return fmt.Sprintf("Added provider %q to your config and set it active.", ensured.Name)
	case ensured.Created:
		return fmt.Sprintf("Added provider %q to your config; the active provider is still %q.\nSwitch with: zero providers use %s", ensured.Name, ensured.Active, ensured.Name)
	case active:
		return fmt.Sprintf("Provider %q is configured and active.", ensured.Name)
	default:
		return fmt.Sprintf("Provider %q is already configured.\nSwitch with: zero providers use %s", ensured.Name, ensured.Name)
	}
}

// runAuth dispatches `zero auth <command>` for provider OAuth login. It is
// additive and independent of `zero mcp oauth` (MCP server auth), which is
// unchanged.
func runAuth(args []string, stdout io.Writer, stderr io.Writer, deps appDeps) int {
	if len(args) == 0 {
		return writeExecUsageError(stderr, "auth subcommand required. Use `zero auth status`.")
	}
	if args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		if err := writeAuthHelp(stdout); err != nil {
			return exitCrash
		}
		return exitSuccess
	}
	switch args[0] {
	case "login":
		return runAuthLogin(args[1:], stdout, stderr, deps)
	case "logout":
		return runAuthLogout(args[1:], stdout, stderr, deps)
	case "status":
		return runAuthStatus(args[1:], stdout, stderr, deps)
	case "refresh":
		return runAuthRefresh(args[1:], stdout, stderr, deps)
	case "reset":
		return runAuthReset(args[1:], stdout, stderr, deps)
	case "openrouter":
		return runAuthOpenRouter(args[1:], stdout, stderr, deps)
	case "chatgpt":
		return runAuthChatGPT(args[1:], stdout, stderr, deps)
	default:
		return writeExecUsageError(stderr, fmt.Sprintf("unknown auth subcommand %q", args[0]))
	}
}

// runAuthOpenRouter runs OpenRouter's browser PKCE login and saves the freshly
// minted API key into the user's provider credential store. Unlike `auth login`
// (which stores an OAuth bearer token), OpenRouter's flow mints a normal API key;
// persist it immediately so the provider is usable after the command completes.
func runAuthOpenRouter(args []string, stdout io.Writer, stderr io.Writer, deps appDeps) int {
	for _, a := range args {
		if a == "-h" || a == "--help" || a == "help" {
			_ = writeAuthHelp(stdout)
			return exitSuccess
		}
	}
	// openrouter takes no positional args or flags; reject the unexpected so a
	// typo/unsupported flag fails fast instead of silently running the login.
	if len(args) > 0 {
		return writeExecUsageError(stderr, fmt.Sprintf("zero auth openrouter takes no arguments (got %q)", args[0]))
	}
	// Check the config BEFORE the browser flow, as every other auth entry point
	// does. Finding out the file is unusable only after the user has completed a
	// PKCE round trip wastes their work and mints a key Zero then cannot store.
	configPath, err := deps.userConfigPath()
	if err != nil {
		return writeAppError(stderr, redaction.ErrorMessage(err, redaction.Options{}), exitCrash)
	}
	if err := config.PreflightCatalogProviderLogin(configPath, "openrouter"); err != nil {
		return writeAppError(stderr, redaction.ErrorMessage(err, redaction.Options{}), exitCrash)
	}
	key, err := deps.openRouterLogin(context.Background(), provideroauth.OpenRouterOptions{
		Out:        stdout,
		HTTPClient: &http.Client{Timeout: 30 * time.Second},
		// ZERO_OPENROUTER_BASE_URL overrides the endpoint (self-hosted gateway or tests).
		BaseURL: strings.TrimSpace(os.Getenv("ZERO_OPENROUTER_BASE_URL")),
	})
	if err != nil {
		return writeAppError(stderr, redaction.ErrorMessage(err, redaction.Options{}), exitCrash)
	}
	key = strings.TrimSpace(key)
	line, err := saveOpenRouterProviderKey(deps, key)
	if err != nil {
		// The key is real and the user paid for it with a browser round trip, so
		// print it rather than losing it. But nothing was persisted, so this is a
		// failure: exiting 0 here reported success for a command that left the
		// provider unusable, and a script would have carried on.
		if _, writeErr := fmt.Fprintf(stdout, "\nOpenRouter login complete — new API key minted, but Zero could not save it.\nUse it manually, e.g.:\n  export OPENROUTER_API_KEY=%s\n", key); writeErr != nil {
			return exitCrash
		}
		return writeAppError(stderr, "could not save the OpenRouter API key: "+redaction.ErrorMessage(err, redaction.Options{}), exitCrash)
	}
	if _, err := fmt.Fprintf(stdout, "\nOpenRouter login complete — new API key saved.\n%s\n", line); err != nil {
		return exitCrash
	}
	return exitSuccess
}

func saveOpenRouterProviderKey(deps appDeps, key string) (string, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return "", fmt.Errorf("OpenRouter returned an empty API key")
	}
	configPath, err := deps.userConfigPath()
	if err != nil {
		return "", err
	}
	// Validate the persisted config BEFORE EnsureCatalogProvider's lookup, which
	// matches case-insensitively and would happily return one of a pair of legacy
	// duplicate rows — whose shared credential the capture below then overwrites
	// for a config that can never be published.
	if err := config.PreflightUserConfig(configPath); err != nil {
		return "", err
	}
	ensured, err := config.EnsureCatalogProvider(configPath, "openrouter")
	if err != nil {
		return "", err
	}
	// One operation owns validate → capture → publish, and restores the previous
	// stored key if publication is rejected, so a failed login never costs the
	// user the key they were already working with.
	if err := config.PublishProviderCredential(configPath, ensured.Name, key); err != nil {
		return "", err
	}
	active := config.SameProviderIdentity(ensured.Active, ensured.Name)
	switch {
	case ensured.Created && active:
		return fmt.Sprintf("Added provider %q to your config and set it active.", ensured.Name), nil
	case ensured.Created:
		return fmt.Sprintf("Added provider %q to your config; the active provider is still %q.\nSwitch with: zero providers use %s", ensured.Name, ensured.Active, ensured.Name), nil
	case active:
		return fmt.Sprintf("Provider %q is configured, active, and ready to use.", ensured.Name), nil
	default:
		return fmt.Sprintf("Provider %q is configured with the new key.\nSwitch with: zero providers use %s", ensured.Name, ensured.Name), nil
	}
}

// runAuthChatGPT runs the ChatGPT (Codex) browser PKCE login, persists the
// bearer + chatgpt-account-id claim under the "chatgpt" provider key, and
// prints a status block. The bearer routes to chatgpt.com/backend-api/codex
// for ChatGPT Plus/Pro/Business/Enterprise subscribers; a successful login
// makes the agent use the chatgpt catalog entry with the OAuth bearer.
func runAuthChatGPT(args []string, stdout io.Writer, stderr io.Writer, deps appDeps) int {
	for _, a := range args {
		if a == "-h" || a == "--help" || a == "help" {
			_ = writeAuthHelp(stdout)
			return exitSuccess
		}
	}
	if len(args) > 0 {
		return writeExecUsageError(stderr, fmt.Sprintf("zero auth chatgpt takes no arguments (got %q)", args[0]))
	}
	configPath, err := deps.userConfigPath()
	if err != nil {
		return writeAppError(stderr, redaction.ErrorMessage(err, redaction.Options{}), exitCrash)
	}
	const provider = "chatgpt"
	if err := config.PreflightCatalogProviderLogin(configPath, provider); err != nil {
		return writeAppError(stderr, redaction.ErrorMessage(err, redaction.Options{}), exitCrash)
	}

	// Build the same env map the oauth engine reads so the chatgpt preset is
	// opted into (the preset is off by default to keep third-party OAuth
	// client identities out of the default credential path). The env is
	// layered: process env first, then ZERO_OAUTH_ALLOW_PRESETS=1.
	env := map[string]string{}
	for _, kv := range os.Environ() {
		if eq := strings.IndexByte(kv, '='); eq > 0 {
			env[kv[:eq]] = kv[eq+1:]
		}
	}
	env["ZERO_OAUTH_ALLOW_PRESETS"] = "1"

	token, err := deps.chatGPTLogin(context.Background(), provideroauth.ChatGPTOptions{
		Env:        env,
		HTTPClient: &http.Client{Timeout: 60 * time.Second},
		Out:        stdout,
		// Don't auto-open a browser — print the URL to stdout and let the
		// user click it. (Same posture as runAuthOpenRouter; the headless
		// sandbox context makes launching a browser a worse default than
		// printing the URL.)
		OpenBrowser: func(string) error { return nil },
	})
	if err != nil {
		return writeAppError(stderr, redaction.ErrorMessage(err, redaction.Options{}), exitCrash)
	}

	// Persist via the oauth manager's store so the same path
	// zero auth status / zero auth refresh / TokenResolver use is hit.
	// We bypass Manager.Login because the account-id extraction happens
	// inside provideroauth.ChatGPTLogin; the manager would not pick up
	// the customized Token.Account field.
	store, err := oauth.NewStore(oauth.StoreOptions{Now: deps.now})
	if err != nil {
		return writeAppError(stderr, redaction.ErrorMessage(err, redaction.Options{}), exitCrash)
	}
	if err := config.PreflightCatalogProviderLogin(configPath, provider); err != nil {
		return writeAppError(stderr, redaction.ErrorMessage(err, redaction.Options{}), exitCrash)
	}
	if err := store.Save(oauth.ProviderKey(provider), token); err != nil {
		return writeAppError(stderr, redaction.ErrorMessage(err, redaction.Options{}), exitCrash)
	}
	statuses, err := oauthFormatChatGPTStatus(token)
	if err != nil {
		return writeAppError(stderr, redaction.ErrorMessage(err, redaction.Options{}), exitCrash)
	}
	if _, err := fmt.Fprint(stdout, statuses); err != nil {
		return exitCrash
	}
	if line := ensureLoginProviderProfile(deps, "chatgpt"); line != "" {
		if _, err := fmt.Fprintf(stdout, "\n%s\n", line); err != nil {
			return exitCrash
		}
	}
	return exitSuccess
}

// oauthFormatChatGPTStatus formats the saved ChatGPT token into the same
// shape `zero auth status` prints, so the user sees a consistent view.
func oauthFormatChatGPTStatus(token oauth.Token) (string, error) {
	store, err := oauth.NewStore(oauth.StoreOptions{})
	if err != nil {
		return "", err
	}
	statuses, err := store.Status("provider:chatgpt")
	if err != nil {
		return "", err
	}
	if len(statuses) == 0 {
		// Fallback: the token was just saved but the status query came up
		// empty (e.g. an OS keyring backend that doesn't enumerate). The
		// user still has a successful login; tell them what was saved
		// without the formatted status block.
		accountLine := ""
		if strings.TrimSpace(token.Account) != "" {
			accountLine = fmt.Sprintf("ChatGPT account id: %s\n", token.Account)
		}
		return fmt.Sprintf("ChatGPT login complete.\n%s", accountLine), nil
	}
	return oauth.FormatStatuses(statuses), nil
}

// authArgs is the parsed form of an auth subcommand's arguments.
type authArgs struct {
	positional []string
	json       bool
	device     bool
	watch      bool
	confirm    bool
	scopes     []string
	help       bool
}

func parseAuthArgs(sub string, args []string) (authArgs, error) {
	var parsed authArgs
	addScope := func(scope string) error {
		scope = strings.TrimSpace(scope)
		if scope == "" {
			return fmt.Errorf("--scope requires a non-empty value")
		}
		parsed.scopes = append(parsed.scopes, scope)
		return nil
	}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "-h" || arg == "--help" || arg == "help":
			parsed.help = true
		case arg == "--json":
			parsed.json = true
		case arg == "--device":
			parsed.device = true
		case arg == "--watch":
			parsed.watch = true
		case arg == "--confirm":
			parsed.confirm = true
		case arg == "--scope":
			if i+1 >= len(args) {
				return authArgs{}, fmt.Errorf("--scope requires a value")
			}
			i++
			if err := addScope(args[i]); err != nil {
				return authArgs{}, err
			}
		case strings.HasPrefix(arg, "--scope="):
			if err := addScope(strings.TrimPrefix(arg, "--scope=")); err != nil {
				return authArgs{}, err
			}
		case strings.HasPrefix(arg, "-"):
			return authArgs{}, fmt.Errorf("unknown flag %q", arg)
		default:
			parsed.positional = append(parsed.positional, arg)
		}
	}
	if parsed.help {
		return parsed, nil // help short-circuits flag validation
	}
	if err := validateAuthFlags(sub, parsed); err != nil {
		return authArgs{}, err
	}
	return parsed, nil
}

// validateAuthFlags rejects flags a subcommand does not accept, so an ambiguous
// invocation fails fast instead of silently ignoring a flag.
func validateAuthFlags(sub string, a authArgs) error {
	allowed := map[string]map[string]bool{
		"login":   {"device": true, "scope": true},
		"logout":  {"json": true},
		"status":  {"json": true},
		"refresh": {"watch": true},
		"reset":   {"json": true, "confirm": true},
	}[sub]
	bad := func(name string) error { return fmt.Errorf("zero auth %s does not accept %s", sub, name) }
	if a.json && !allowed["json"] {
		return bad("--json")
	}
	if a.device && !allowed["device"] {
		return bad("--device")
	}
	if a.watch && !allowed["watch"] {
		return bad("--watch")
	}
	if a.confirm && !allowed["confirm"] {
		return bad("--confirm")
	}
	if len(a.scopes) > 0 && !allowed["scope"] {
		return bad("--scope")
	}
	return nil
}

// newAuthManager builds an oauth.Manager backed by the file store, printing the
// authorization URL / device code to stdout. The store path honors
// ZERO_OAUTH_TOKENS_PATH (env), so callers/tests can redirect it. Setting
// ZERO_OAUTH_STORAGE=encrypted-file selects the AES-256-GCM encrypted-at-rest
// backend (a per-user secret is created beside the token file).
func newAuthManager(deps appDeps, out io.Writer, beforeSave func() error) (*oauth.Manager, error) {
	// Validate ZERO_OAUTH_STORAGE up front: a mistyped value must fail fast rather
	// than silently change the backend. Empty = default (plaintext 0600 file);
	// "encrypted-file" = AES-256-GCM; "keyring" = the OS keyring.
	storage := strings.ToLower(strings.TrimSpace(os.Getenv("ZERO_OAUTH_STORAGE")))
	switch storage {
	case "", "file", "encrypted-file", "keyring":
	default:
		return nil, fmt.Errorf("invalid ZERO_OAUTH_STORAGE %q (supported: file, encrypted-file, keyring)", storage)
	}
	store, err := oauth.NewStore(oauth.StoreOptions{
		Now:     deps.now,
		Storage: storage,
	})
	if err != nil {
		return nil, err
	}
	return oauth.NewManager(oauth.ManagerOptions{
		Store:      store,
		HTTPClient: &http.Client{Timeout: 30 * time.Second},
		Now:        deps.now,
		Out:        out,
		// The opener prints the URL so headless shells can copy it; the URL
		// carries no token material. A real browser launch is intentionally not
		// performed (the sandbox/headless contexts make printing the safer default).
		OpenBrowser: func(string) error { return nil },
		// `zero auth login <preset>` (e.g. xai) should resolve the baked-in preset
		// without the operator exporting ZERO_OAUTH_ALLOW_PRESETS first.
		AllowPresets: true,
		BeforeSave:   beforeSave,
	})
}

func runAuthLogin(args []string, stdout io.Writer, stderr io.Writer, deps appDeps) int {
	parsed, err := parseAuthArgs("login", args)
	if err != nil {
		return writeExecUsageError(stderr, err.Error())
	}
	if parsed.help {
		_ = writeAuthHelp(stdout)
		return exitSuccess
	}
	if len(parsed.positional) != 1 {
		return writeExecUsageError(stderr, "usage: zero auth login <provider> [--device] [--scope <scope>]")
	}
	provider := strings.ToLower(strings.TrimSpace(parsed.positional[0]))
	// ChatGPT (Codex) requires a fixed redirect_uri (http://localhost:1455/
	// auth/callback) and mandatory authorize params (id_token_add_organizations,
	// codex_cli_simplified_flow, originator) that the generic loopback flow
	// cannot supply. Route it to the dedicated ChatGPT login so
	// `zero auth login chatgpt` behaves identically to `zero auth chatgpt`.
	if strings.EqualFold(provider, "chatgpt") {
		if parsed.device {
			return writeExecUsageError(stderr, "ChatGPT login does not support --device (it is loopback-only)")
		}
		if len(parsed.scopes) > 0 {
			return writeExecUsageError(stderr, "ChatGPT login does not support --scope (the required scopes are fixed by the Codex client registration)")
		}
		return runAuthChatGPT(nil, stdout, stderr, deps)
	}
	configPath, err := deps.userConfigPath()
	if err != nil {
		return writeAppError(stderr, redaction.ErrorMessage(err, redaction.Options{}), exitCrash)
	}
	if err := config.PreflightCatalogProviderLogin(configPath, provider); err != nil {
		return writeAppError(stderr, redaction.ErrorMessage(err, redaction.Options{}), exitCrash)
	}
	manager, err := newAuthManager(deps, stdout, func() error {
		return config.PreflightCatalogProviderLogin(configPath, provider)
	})
	if err != nil {
		return writeAppError(stderr, redaction.ErrorMessage(err, redaction.Options{}), exitCrash)
	}
	status, err := manager.Login(context.Background(), oauth.LoginOptions{
		Provider:    provider,
		Device:      parsed.device,
		ExtraScopes: parsed.scopes,
	})
	if err != nil {
		return writeAppError(stderr, redaction.ErrorMessage(err, redaction.Options{}), exitCrash)
	}
	if _, err := fmt.Fprintf(stdout, "Logged in to %s.\n%s\n", parsed.positional[0], oauth.FormatStatuses([]oauth.Status{status})); err != nil {
		return exitCrash
	}
	if line := ensureLoginProviderProfile(deps, provider); line != "" {
		if _, err := fmt.Fprintln(stdout, line); err != nil {
			return exitCrash
		}
	}
	return exitSuccess
}

func runAuthLogout(args []string, stdout io.Writer, stderr io.Writer, deps appDeps) int {
	parsed, err := parseAuthArgs("logout", args)
	if err != nil {
		return writeExecUsageError(stderr, err.Error())
	}
	if parsed.help {
		_ = writeAuthHelp(stdout)
		return exitSuccess
	}
	if len(parsed.positional) != 1 {
		return writeExecUsageError(stderr, "usage: zero auth logout <provider>")
	}
	provider := parsed.positional[0]
	configPath, err := deps.userConfigPath()
	if err != nil {
		return writeAppError(stderr, redaction.ErrorMessage(err, redaction.Options{}), exitCrash)
	}
	if err := config.PreflightUserConfig(configPath); err != nil {
		return writeAppError(stderr, redaction.ErrorMessage(err, redaction.Options{}), exitCrash)
	}
	credentialCandidates, configProvider, err := config.ProviderCredentialCandidates(configPath, provider)
	if err != nil {
		return writeAppError(stderr, redaction.ErrorMessage(err, redaction.Options{}), exitCrash)
	}
	manager, err := newAuthManager(deps, stdout, nil)
	if err != nil {
		return writeAppError(stderr, redaction.ErrorMessage(err, redaction.Options{}), exitCrash)
	}
	// Delete every candidate the runtime bearer resolver would have tried
	// (ProviderProfile.OAuthLoginCandidates: profile name, then catalog id) —
	// not just the spelling the user typed. A profile saved as
	// {name:"my-xai", catalogId:"xai"} logged in via `zero auth login xai`
	// stores its token under "xai"; `zero auth logout my-xai` must delete that
	// too, or the login silently survives.
	//
	// The catalog id joins that set only when the resolved profile is the ONLY
	// row claiming it. Catalog ids are shared by design — stored-key "work-xai",
	// stored-key "xai", and keyless "personal-xai" can all carry catalogId
	// "xai" — so expanding unconditionally made `logout work-xai` delete the
	// "xai" token and the "xai" profile's API key while clearing only
	// work-xai's marker. When the id is shared, the user has to name the
	// profile whose credential they mean.
	removed := false
	for _, candidate := range credentialCandidates {
		candidateRemoved, err := manager.Logout(candidate)
		if err != nil {
			return writeAppError(stderr, redaction.ErrorMessage(err, redaction.Options{}), exitCrash)
		}
		removed = removed || candidateRemoved
	}
	// Also drop the addressed row's stored API keys and marker so `auth logout`
	// clears the whole credential (OAuth token AND key), not just the OAuth side.
	// ProviderCredentialCandidates includes a catalog alias only when no sibling
	// provider or dictation configuration claims it.
	//
	// Clear the marker before deleting the shared credential. The reverse order
	// can leave apiKeyStored:true with no secret behind it when config publication
	// fails. Resolve catalog aliases first, but mutate the exact persisted row.
	keyRemoved := false
	if configProvider != "" {
		row, match, resolveErr := config.ResolvePersistedProviderIdentity(configPath, configProvider)
		if resolveErr != nil || match == config.PersistedIdentityNone || match == config.PersistedIdentityAmbiguous {
			if resolveErr == nil {
				resolveErr = fmt.Errorf("provider %q no longer resolves to a persisted row", configProvider)
			}
			return writeAppError(stderr, redaction.ErrorMessage(resolveErr, redaction.Options{}), exitCrash)
		}
		catalogExclusive, exclusiveErr := config.CatalogIdentityExclusive(configPath, row.CatalogID, configProvider)
		if exclusiveErr != nil {
			return writeAppError(stderr, redaction.ErrorMessage(exclusiveErr, redaction.Options{}), exitCrash)
		}
		keyCandidates := []string{configProvider}
		if catalogExclusive && !config.SameProviderIdentity(row.CatalogID, configProvider) {
			keyCandidates = append(keyCandidates, row.CatalogID)
		}
		if _, clearErr := config.ClearProviderKeyStoredCaseVariants(configPath, configProvider); clearErr != nil {
			return writeAppError(stderr, redaction.ErrorMessage(clearErr, redaction.Options{}), exitCrash)
		}
		keyStore, keyErr := config.ProviderKeyStoreForConfigPath(configPath)
		if keyErr != nil {
			return writeAppError(stderr, redaction.ErrorMessage(keyErr, redaction.Options{}), exitCrash)
		}
		for _, candidate := range keyCandidates {
			candidateRemoved, candidateErr := keyStore.Delete(candidate)
			if candidateErr != nil {
				return writeAppError(stderr, redaction.ErrorMessage(candidateErr, redaction.Options{}), exitCrash)
			}
			keyRemoved = keyRemoved || candidateRemoved
		}
	}
	removed = removed || keyRemoved
	if parsed.json {
		payload := struct {
			Provider string `json:"provider"`
			Removed  bool   `json:"removed"`
		}{Provider: provider, Removed: removed}
		if err := writePrettyJSON(stdout, payload); err != nil {
			return exitCrash
		}
		return exitSuccess
	}
	msg := fmt.Sprintf("No stored credential for %s.\n", provider)
	if removed {
		msg = fmt.Sprintf("Logged out of %s.\n", provider)
	}
	if _, err := fmt.Fprint(stdout, msg); err != nil {
		return exitCrash
	}
	return exitSuccess
}

func runAuthStatus(args []string, stdout io.Writer, stderr io.Writer, deps appDeps) int {
	parsed, err := parseAuthArgs("status", args)
	if err != nil {
		return writeExecUsageError(stderr, err.Error())
	}
	if parsed.help {
		_ = writeAuthHelp(stdout)
		return exitSuccess
	}
	if len(parsed.positional) > 1 {
		return writeExecUsageError(stderr, "usage: zero auth status [provider]")
	}
	var credentialCandidates []string
	if len(parsed.positional) == 1 {
		configPath, err := deps.userConfigPath()
		if err != nil {
			return writeAppError(stderr, err.Error(), exitCrash)
		}
		credentialCandidates, _, err = config.ProviderCredentialCandidates(configPath, parsed.positional[0])
		if err != nil {
			return writeAppError(stderr, redaction.ErrorMessage(err, redaction.Options{}), exitCrash)
		}
	}
	manager, err := newAuthManager(deps, stdout, nil)
	if err != nil {
		return writeAppError(stderr, redaction.ErrorMessage(err, redaction.Options{}), exitCrash)
	}
	statuses, err := manager.StatusAll()
	if err != nil {
		return writeAppError(stderr, redaction.ErrorMessage(err, redaction.Options{}), exitCrash)
	}
	if len(parsed.positional) == 1 {
		statuses = filterAuthStatuses(statuses, credentialCandidates)
	}
	if parsed.json {
		payload := struct {
			Logins []oauth.Status `json:"logins"`
		}{Logins: statuses}
		if err := writePrettyJSON(stdout, payload); err != nil {
			return exitCrash
		}
		return exitSuccess
	}
	if _, err := fmt.Fprintln(stdout, oauth.FormatStatuses(statuses)); err != nil {
		return exitCrash
	}
	return exitSuccess
}

func runAuthRefresh(args []string, stdout io.Writer, stderr io.Writer, deps appDeps) int {
	parsed, err := parseAuthArgs("refresh", args)
	if err != nil {
		return writeExecUsageError(stderr, err.Error())
	}
	if parsed.help {
		_ = writeAuthHelp(stdout)
		return exitSuccess
	}
	if len(parsed.positional) != 1 {
		return writeExecUsageError(stderr, "usage: zero auth refresh <provider>")
	}
	provider := parsed.positional[0]
	configPath, err := deps.userConfigPath()
	if err != nil {
		return writeAppError(stderr, err.Error(), exitCrash)
	}
	credentialCandidates, _, err := config.ProviderCredentialCandidates(configPath, provider)
	if err != nil {
		return writeAppError(stderr, redaction.ErrorMessage(err, redaction.Options{}), exitCrash)
	}
	if len(credentialCandidates) == 0 {
		return writeAppError(stderr, redaction.ErrorMessage(fmt.Errorf("provider identity %q resolved to no credential candidates", provider), redaction.Options{}), exitCrash)
	}
	manager, err := newAuthManager(deps, stdout, nil)
	if err != nil {
		return writeAppError(stderr, redaction.ErrorMessage(err, redaction.Options{}), exitCrash)
	}
	if parsed.watch {
		return runAuthRefreshWatch(manager, credentialCandidates, provider, stdout, stderr)
	}
	var refreshErr error
	for _, candidate := range credentialCandidates {
		_, refreshErr = manager.Handle401(context.Background(), oauth.ProviderKey(candidate))
		if refreshErr == nil {
			break
		}
		if !errors.Is(refreshErr, oauth.ErrNoToken) {
			break
		}
	}
	if refreshErr != nil {
		return writeAppError(stderr, redaction.ErrorMessage(refreshErr, redaction.Options{}), exitCrash)
	}
	if _, err := fmt.Fprintf(stdout, "Refreshed OAuth token for %s.\n", provider); err != nil {
		return exitCrash
	}
	return exitSuccess
}

// runAuthRefreshWatch keeps a provider's token fresh in the foreground until
// interrupted. This is the opt-in proactive-refresh scheduler surface (for a
// long-running external process that reads the token file). It validates a
// refreshable token exists first, then schedules refreshes before each expiry.
func runAuthRefreshWatch(manager *oauth.Manager, candidates []string, provider string, stdout io.Writer, stderr io.Writer) int {
	ctx, stop := signalContext()
	defer stop()
	// Validate (and refresh-if-needed) once up front so a missing/non-refreshable
	// token fails fast instead of silently watching nothing.
	key := ""
	var freshErr error
	for _, candidate := range candidates {
		candidateKey := oauth.ProviderKey(candidate)
		_, freshErr = manager.GetFresh(ctx, candidateKey)
		if freshErr == nil {
			key = candidateKey
			break
		}
		if !errors.Is(freshErr, oauth.ErrNoToken) {
			break
		}
	}
	if freshErr != nil {
		return writeAppError(stderr, redaction.ErrorMessage(freshErr, redaction.Options{}), exitCrash)
	}
	scheduler := oauth.NewRefreshScheduler()
	scheduler.Start(ctx, manager, key)
	defer scheduler.Stop()
	if _, err := fmt.Fprintf(stdout, "Watching %s — refreshing before expiry. Press Ctrl+C to stop.\n", provider); err != nil {
		return exitCrash
	}
	<-ctx.Done()
	return exitSuccess
}

func runAuthReset(args []string, stdout io.Writer, stderr io.Writer, deps appDeps) int {
	parsed, err := parseAuthArgs("reset", args)
	if err != nil {
		return writeExecUsageError(stderr, err.Error())
	}
	if parsed.help {
		_ = writeAuthHelp(stdout)
		return exitSuccess
	}
	if len(parsed.positional) > 0 {
		return writeExecUsageError(stderr, fmt.Sprintf("zero auth reset takes no arguments (got %q)", parsed.positional[0]))
	}
	if !parsed.confirm {
		return writeExecUsageError(stderr, "zero auth reset clears all stored OAuth logins and shared MCP tokens; rerun with --confirm to reset")
	}
	manager, err := newAuthManager(deps, stdout, nil)
	if err != nil {
		return writeAppError(stderr, redaction.ErrorMessage(err, redaction.Options{}), exitCrash)
	}
	if err := manager.Reset(); err != nil {
		return writeAppError(stderr, redaction.ErrorMessage(err, redaction.Options{}), exitCrash)
	}
	if parsed.json {
		payload := struct {
			Reset bool `json:"reset"`
		}{Reset: true}
		if err := writePrettyJSON(stdout, payload); err != nil {
			return exitCrash
		}
		return exitSuccess
	}
	if _, err := fmt.Fprintln(stdout, "Reset OAuth token store."); err != nil {
		return exitCrash
	}
	return exitSuccess
}

func filterAuthStatuses(statuses []oauth.Status, candidates []string) []oauth.Status {
	wanted := make(map[string]struct{}, len(candidates))
	for _, candidate := range candidates {
		wanted[oauth.ProviderKey(candidate)] = struct{}{}
	}
	filtered := make([]oauth.Status, 0, len(candidates))
	for _, st := range statuses {
		if _, ok := wanted[st.Key]; ok {
			filtered = append(filtered, st)
		}
	}
	return filtered
}

func writeAuthHelp(w io.Writer) error {
	_, err := fmt.Fprint(w, `Usage:
  zero auth <command>

Commands:
  login <provider> [--device] [--scope <scope>]   Log in to a provider via OAuth
  logout <provider>                               Delete a provider's stored login
  status [provider]                               Show login presence/expiry (never the token)
  refresh <provider> [--watch]                    Force a token refresh (--watch keeps it fresh)
  reset --confirm [--json]                        Clear all OAuth logins and shared MCP tokens (also repairs corrupted stores)
  openrouter                                      Log in to OpenRouter in the browser; mints an API key
  chatgpt                                         Log in to ChatGPT in the browser (Codex backend, ChatGPT Plus/Pro)

A provider is any OAuth 2.0 / OIDC server. "openrouter" ('zero auth openrouter')
works out of the box. "xai" ('zero auth login xai') uses a built-in preset that is
off by default — enable it with ZERO_OAUTH_ALLOW_PRESETS=1, or set the
ZERO_OAUTH_XAI_* vars yourself. "chatgpt" ('zero auth login chatgpt' or
'zero auth chatgpt') uses a fixed-port loopback flow against the Codex backend.
Any preset field is overridable via the env vars below. For a custom provider named <name>, set:
  ZERO_OAUTH_<NAME>_CLIENT_ID       (required)
  ZERO_OAUTH_<NAME>_CLIENT_SECRET   (optional)
  ZERO_OAUTH_<NAME>_AUTHORIZE_URL   ZERO_OAUTH_<NAME>_TOKEN_URL
  ZERO_OAUTH_<NAME>_DEVICE_URL      ZERO_OAUTH_<NAME>_ISSUER_URL (for discovery)
  ZERO_OAUTH_<NAME>_SCOPES          ZERO_OAUTH_<NAME>_FLOW (loopback|device)
Endpoint URLs must be https (loopback exempt).

Storage: tokens are written 0600 under $XDG_CONFIG_HOME/zero (override with
ZERO_OAUTH_TOKENS_PATH). Set ZERO_OAUTH_STORAGE=encrypted-file to encrypt them at
rest with AES-256-GCM (a per-user secret beside the file), or
ZERO_OAUTH_STORAGE=keyring to use the OS keyring (macOS Keychain / Linux
secret-tool). MCP server tokens share the same store.

Flags:
      --device   Use the device-code flow (headless/SSH; no browser)
      --scope    Add an OAuth scope (repeatable)
      --watch    Keep the token fresh in the foreground (refresh only)
      --json     Print result as JSON (status/logout/reset)
  -h, --help     Show this help
`)
	return err
}
