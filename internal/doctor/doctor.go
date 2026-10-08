package doctor

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/Gitlawb/zero/internal/config"
	"github.com/Gitlawb/zero/internal/modelregistry"
	"github.com/Gitlawb/zero/internal/oauth"
	"github.com/Gitlawb/zero/internal/providerhealth"
	"github.com/Gitlawb/zero/internal/redaction"
)

type Status string

const (
	StatusPass Status = "pass"
	StatusWarn Status = "warn"
	StatusFail Status = "fail"
)

type Check struct {
	ID      string         `json:"id"`
	Label   string         `json:"label"`
	Status  Status         `json:"status"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

type Report struct {
	GeneratedAt string  `json:"generatedAt"`
	OK          bool    `json:"ok"`
	Checks      []Check `json:"checks"`
}

type Options struct {
	Now            func() time.Time
	Runtime        string
	UserConfig     string
	ProjectConfig  string
	Provider       config.ProviderProfile
	WorkspaceRoot  string
	Sandbox        config.SandboxConfig
	Connectivity   bool
	ProviderHealth *providerhealth.Result
	// GOOS overrides the platform used to resolve the sandbox backend. Empty
	// means runtime.GOOS. Tests set it to assert platform-specific remedies.
	GOOS string
	// LookupExecutable resolves a binary on PATH for the sandbox-backend and
	// LSP-server checks. Nil means exec.LookPath; tests inject a stub so the
	// checks are deterministic regardless of the host's installed tooling.
	LookupExecutable func(string) (string, error)
}

func Run(options Options) Report {
	now := options.Now
	if now == nil {
		now = time.Now
	}
	checks := []Check{
		runtimeCheck(options.Runtime),
		configFilesCheck(options.UserConfig, options.ProjectConfig),
		configValidationCheck(options.UserConfig, options.ProjectConfig),
	}
	providerCheck := providerConfigCheck(options.Provider)
	checks = append(checks, providerCheck)
	modelCheck := providerModelCheck(options.Provider)
	checks = append(checks, modelCheck)
	checks = append(checks, connectivityCheck(options.Provider, options.Connectivity, modelCheck.Status, options.ProviderHealth))
	checks = append(checks, sandboxBackendCheck(options.GOOS, options.LookupExecutable, options.WorkspaceRoot, options.Sandbox))
	checks = append(checks, lspServersCheck(options.LookupExecutable))

	report := Report{
		GeneratedAt: now().UTC().Format(time.RFC3339),
		OK:          true,
		Checks:      checks,
	}
	for _, check := range checks {
		if check.Status == StatusFail {
			report.OK = false
			break
		}
	}
	return report
}

func (report Report) Check(id string) *Check {
	for index := range report.Checks {
		if report.Checks[index].ID == id {
			return &report.Checks[index]
		}
	}
	return nil
}

func Format(report Report) string {
	lines := []string{
		fmt.Sprintf("Zero doctor report (%s)", redaction.RedactString(report.GeneratedAt, redaction.Options{})),
		fmt.Sprintf("Overall: %s", passFail(report.OK)),
	}
	for _, check := range report.Checks {
		lines = append(lines, fmt.Sprintf("[%s] %s - %s", check.Status, redaction.RedactString(check.ID, redaction.Options{}), redaction.RedactString(check.Message, redaction.Options{})))
		if details := formatDetails(check.Details); details != "" {
			lines = append(lines, "  "+details)
		}
	}
	return strings.Join(lines, "\n")
}

func runtimeCheck(runtime string) Check {
	runtime = strings.TrimSpace(runtime)
	if runtime == "" {
		runtime = "go"
	}
	return check("runtime.go", "Go runtime", StatusPass, fmt.Sprintf("Zero Go runtime is available (%s).", runtime), map[string]any{"runtime": runtime})
}

func configFilesCheck(userPath string, projectPath string) Check {
	details := map[string]any{}
	if strings.TrimSpace(userPath) != "" {
		details["userConfigPath"] = userPath
	}
	if strings.TrimSpace(projectPath) != "" {
		details["projectConfigPath"] = projectPath
	}
	if len(details) == 0 {
		return check("config.files", "Config files", StatusWarn, "No explicit Zero config files were inspected.", nil)
	}
	return check("config.files", "Config files", StatusPass, "Zero config file inputs are available for inspection.", details)
}

func providerConfigCheck(profile config.ProviderProfile) Check {
	if emptyProviderProfile(profile) {
		return check("provider.config", "Provider config", StatusFail, "No LLM provider is configured.", map[string]any{"help": "Set a provider in config or environment."})
	}
	// Report credential PRESENCE, never the value. Reported under a non-sensitive
	// key ("authConfigured"): "apiKey" and "credentialConfigured" are themselves
	// sensitive after camelCase normalization, so check()'s redaction would scrub
	// the indicator to [REDACTED] — making "set"/"not set" invisible.
	// HasConfiguredCredential is the shared definition of
	// "key-authed" (inline key, raw auth header, or a key in the encrypted
	// credential store), matching ProviderSnapshot.APIKeySet — checking only the
	// inline fields made doctor and `zero providers list` disagree about the
	// same stored-key profile. A stored OAuth login counts too: a keyless
	// token-login provider (e.g. ChatGPT) is fully able to make requests and
	// must not fail the one tool meant to verify setup.
	credential := "not set"
	switch {
	case profile.HasConfiguredCredential():
		credential = "set"
	case providerHasStoredOAuthLogin(profile):
		credential = "oauth login"
	}
	details := map[string]any{
		"name":           profile.Name,
		"provider":       profile.ProviderKind,
		"baseURL":        profile.BaseURL,
		"model":          profile.Model,
		"authConfigured": credential,
	}
	// A remote provider with no credential cannot make a request, so doctor must NOT
	// report it as healthy — otherwise "Overall: pass" gives a false all-clear for the
	// one tool meant to verify keys. Keyless local providers (loopback base_url, e.g.
	// Ollama/LM Studio) legitimately need no key, so they stay a pass. (AUDIT-H9)
	if credential == "not set" && !localProviderBaseURL(profile.BaseURL) {
		return check("provider.config", "Provider config", StatusFail,
			fmt.Sprintf("No API key configured for %s. Run `zero auth` or `zero setup`, or set the provider's API key environment variable.", providerName(profile)),
			details)
	}
	return check("provider.config", "Provider config", StatusPass, fmt.Sprintf("Provider config loaded for %s.", providerName(profile)), details)
}

// providerHasStoredOAuthLogin reports whether the token store holds a login for
// any of the profile's candidates — the same candidate set and normalized-key
// matching the runtime resolver uses. Errors degrade to false.
func providerHasStoredOAuthLogin(profile config.ProviderProfile) bool {
	candidates := profile.OAuthLoginCandidates()
	if len(candidates) == 0 {
		return false
	}
	store, err := oauth.NewStore(oauth.StoreOptions{})
	if err != nil {
		return false
	}
	_, _, ok := oauth.FirstStored(store, candidates)
	return ok
}

// localProviderBaseURL reports whether the configured base_url is a loopback host
// (a keyless local provider like Ollama/LM Studio), which needs no API key.
func localProviderBaseURL(baseURL string) bool {
	u := strings.TrimSpace(baseURL)
	if u == "" {
		return false
	}
	if parsed, err := url.Parse(u); err == nil && parsed.Host != "" {
		host := strings.TrimSuffix(strings.ToLower(parsed.Hostname()), ".")
		if host == "localhost" || strings.HasSuffix(host, ".localhost") {
			return true
		}
		if addr, err := netip.ParseAddr(host); err == nil {
			return addr.IsLoopback()
		}
	}
	return false
}

func providerModelCheck(profile config.ProviderProfile) Check {
	if emptyProviderProfile(profile) {
		return check("provider.model", "Provider model", StatusWarn, "Model validity was skipped because provider config is unavailable.", nil)
	}
	if strings.TrimSpace(profile.Model) == "" {
		return check("provider.model", "Provider model", StatusFail, "Provider model is required.", map[string]any{"provider": providerName(profile)})
	}
	registry, err := modelregistry.DefaultRegistry()
	if err != nil {
		return check("provider.model", "Provider model", StatusFail, "Model registry could not be loaded: "+err.Error(), nil)
	}
	model, err := registry.Require(profile.Model)
	if err != nil {
		if profile.ProviderKind == config.ProviderKindOpenAICompatible || profile.ProviderKind == config.ProviderKindAnthropicCompat {
			return check("provider.model", "Provider model", StatusWarn, fmt.Sprintf("Custom %s model was not found in the Zero registry; runtime will pass it through to the configured provider. Run `zero doctor --connectivity` to validate the endpoint and auth.", profile.ProviderKind), map[string]any{"model": profile.Model, "provider": providerName(profile)})
		}
		return check("provider.model", "Provider model", StatusFail, "Provider model is invalid: "+err.Error(), map[string]any{"model": profile.Model})
	}
	if !model.AllowsProvider(toModelProvider(profile)) {
		return check("provider.model", "Provider model", StatusFail, fmt.Sprintf("Model %s is not available for provider %s.", model.ID, providerName(profile)), map[string]any{"model": model.ID, "provider": providerName(profile)})
	}
	return check("provider.model", "Provider model", StatusPass, fmt.Sprintf("Model %s resolves to %s.", model.ID, model.Provider), map[string]any{
		"modelId":      model.ID,
		"apiModel":     model.APIModel,
		"provider":     model.Provider,
		"capabilities": model.Capabilities,
	})
}

func connectivityCheck(profile config.ProviderProfile, enabled bool, modelStatus Status, health *providerhealth.Result) Check {
	if !enabled {
		if emptyProviderProfile(profile) || modelStatus == StatusFail {
			return check("provider.connectivity", "Provider connectivity", StatusWarn, "Connectivity check was skipped because provider runtime did not resolve.", nil)
		}
		return check("provider.connectivity", "Provider connectivity", StatusWarn, "Connectivity probe skipped. Run `zero doctor --connectivity` to probe the provider endpoint.", map[string]any{"baseURL": profile.BaseURL})
	}
	if health != nil {
		if providerCheck := health.PrimaryCheck(); providerCheck != nil {
			return check("provider.connectivity", "Provider connectivity", doctorStatus(providerCheck.Status), providerCheck.Message, providerCheck.Details)
		}
		return check("provider.connectivity", "Provider connectivity", doctorStatus(health.Status), "Provider health probe completed without a connectivity check.", map[string]any{"healthStatus": health.Status})
	}
	if emptyProviderProfile(profile) || modelStatus == StatusFail {
		return check("provider.connectivity", "Provider connectivity", StatusWarn, "Connectivity check was skipped because provider runtime did not resolve.", nil)
	}
	return check("provider.connectivity", "Provider connectivity", StatusWarn, "Connectivity probing is not wired in the Go doctor backend yet.", map[string]any{"baseURL": profile.BaseURL})
}

func doctorStatus(status providerhealth.Status) Status {
	switch status {
	case providerhealth.StatusPass:
		return StatusPass
	case providerhealth.StatusFail:
		return StatusFail
	default:
		return StatusWarn
	}
}

func emptyProviderProfile(profile config.ProviderProfile) bool {
	return !config.HasProviderProfile(profile)
}

func check(id string, label string, status Status, message string, details map[string]any) Check {
	redacted := redaction.RedactValue(map[string]any{
		"id":      id,
		"label":   label,
		"status":  string(status),
		"message": message,
		"details": details,
	}, redaction.Options{}).(map[string]any)
	out := Check{
		ID:      redacted["id"].(string),
		Label:   redacted["label"].(string),
		Status:  Status(redacted["status"].(string)),
		Message: redacted["message"].(string),
	}
	if detailsValue, ok := redacted["details"].(map[string]any); ok && len(detailsValue) > 0 {
		out.Details = detailsValue
	}
	return out
}

func providerName(profile config.ProviderProfile) string {
	if strings.TrimSpace(profile.Name) != "" {
		return strings.TrimSpace(profile.Name)
	}
	if strings.TrimSpace(string(profile.ProviderKind)) != "" {
		return strings.TrimSpace(string(profile.ProviderKind))
	}
	return strings.TrimSpace(profile.Provider)
}

func toModelProvider(profile config.ProviderProfile) modelregistry.ProviderKind {
	switch profile.ProviderKind {
	case config.ProviderKindAnthropic, config.ProviderKindAnthropicCompat:
		return modelregistry.ProviderAnthropic
	case config.ProviderKindGoogle:
		return modelregistry.ProviderGoogle
	case config.ProviderKindOpenAICompatible:
		return modelregistry.ProviderOpenAICompatible
	default:
		return modelregistry.ProviderOpenAI
	}
}

func formatDetails(details map[string]any) string {
	if len(details) == 0 {
		return ""
	}
	keys := make([]string, 0, len(details))
	for key := range details {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(details))
	for _, key := range keys {
		value := details[key]
		parts = append(parts, fmt.Sprintf("%s: %s", redaction.RedactString(key, redaction.Options{}), formatDetailValue(redaction.RedactValue(value, redaction.Options{}))))
	}
	return strings.Join(parts, " | ")
}

// formatDetailValue renders a detail value for human reading. A nested map (e.g. the
// lsp.servers missing-tools list or config.validation results) was previously printed
// with %v, leaking Go's `map[k:v ...]` syntax into user output; render maps as sorted
// "k: v" entries and slices as a comma list instead. (AUDIT-H8)
func formatDetailValue(value any) string {
	if value == nil {
		return ""
	}
	rv := reflect.ValueOf(value)
	switch rv.Kind() {
	case reflect.Map:
		entries := make([]string, 0, rv.Len())
		for _, k := range rv.MapKeys() {
			entries = append(entries, fmt.Sprintf("%v: %v", k.Interface(), rv.MapIndex(k).Interface()))
		}
		sort.Strings(entries)
		return strings.Join(entries, "; ")
	case reflect.Slice, reflect.Array:
		if rv.Type().Elem().Kind() == reflect.Uint8 { // []byte
			return fmt.Sprintf("%v", value)
		}
		parts := make([]string, 0, rv.Len())
		for i := 0; i < rv.Len(); i++ {
			parts = append(parts, fmt.Sprintf("%v", rv.Index(i).Interface()))
		}
		return strings.Join(parts, ", ")
	case reflect.String:
		return rv.String()
	default:
		return fmt.Sprintf("%v", value)
	}
}

func configValidationCheck(userPath string, projectPath string) Check {
	paths := make([]string, 0, 2)
	for _, path := range []string{userPath, projectPath} {
		if strings.TrimSpace(path) != "" {
			paths = append(paths, path)
		}
	}
	if len(paths) == 0 {
		return check("config.validation", "Config validation", StatusWarn, "No Zero config files were available to validate.", nil)
	}

	status := StatusPass
	issueCount := 0
	details := map[string]any{}
	for _, path := range paths {
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			// A genuinely-missing path is configFilesCheck's job to report, and a
			// "missing" path must stay a skip — so a not-exist read error is ignored
			// here. Any OTHER read error (permissions, is-a-directory) means a
			// present-but-unreadable config that would otherwise silently pass
			// validation, so surface it as a failing per-path detail.
			if os.IsNotExist(readErr) {
				continue
			}
			details[path] = map[string]any{"error": "unreadable: " + readErr.Error()}
			status = StatusFail
			issueCount++
			continue
		}
		if line, col, ok := jsonParsePosition(data); ok {
			_, issues := config.ValidateBytes(data)
			errMsg := ""
			if len(issues) > 0 {
				errMsg = issues[0].Message
			}
			details[path] = map[string]any{"line": line, "col": col, "error": errMsg}
			status = StatusFail
			issueCount++
			continue
		}
		_, issues := config.ValidateBytes(data)
		if len(issues) == 0 {
			continue
		}
		messages := make([]string, 0, len(issues))
		for _, issue := range issues {
			messages = append(messages, issue.Message)
		}
		details[path] = map[string]any{"issues": messages}
		status = StatusFail
		issueCount += len(issues)
	}

	if status == StatusPass {
		return check("config.validation", "Config validation", StatusPass, "Zero config files parsed and validated successfully.", nil)
	}
	return check("config.validation", "Config validation", StatusFail, fmt.Sprintf("Zero config validation found %d issue(s).", issueCount), details)
}

// jsonParsePosition reports whether data fails to parse as JSON and, if so, the
// 1-based line/col of the failure using the concrete json error offset.
//
// The probe is a config.FileConfig (not `any`) so that a structurally-valid
// document with a wrong field type (e.g. {"maxTurns":"twelve"}) surfaces a
// *json.UnmarshalTypeError carrying the offset. FileConfig.UnmarshalJSON returns
// the underlying json error unchanged, preserving that offset. Documents that
// are structurally valid AND type-correct unmarshal cleanly (ok=false) and fall
// through to the semantic ValidateBytes branch.
func jsonParsePosition(data []byte) (int, int, bool) {
	var probe config.FileConfig
	err := json.Unmarshal(data, &probe)
	if err == nil {
		return 0, 0, false
	}
	var syntaxErr *json.SyntaxError
	if errors.As(err, &syntaxErr) {
		line, col := offsetToLineCol(data, syntaxErr.Offset)
		return line, col, true
	}
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) {
		line, col := offsetToLineCol(data, typeErr.Offset)
		return line, col, true
	}
	// Non-positional JSON error (e.g. unexpected EOF without offset): do not
	// fabricate a (1,1) position. Returning ok=false routes the error through the
	// no-position ValidateBytes path so it is reported without a fake line/col.
	return 0, 0, false
}

func passFail(ok bool) string {
	if ok {
		return "pass"
	}
	return "fail"
}

// offsetToLineCol converts a byte offset (as reported by *json.SyntaxError /
// *json.UnmarshalTypeError) into a 1-based line and column. Offsets out of range
// are clamped into [0, len(data)].
func offsetToLineCol(data []byte, offset int64) (int, int) {
	if offset < 0 {
		offset = 0
	}
	if offset > int64(len(data)) {
		offset = int64(len(data))
	}
	line := 1
	col := 1
	for index := int64(0); index < offset; index++ {
		if data[index] == '\n' {
			line++
			col = 1
			continue
		}
		col++
	}
	return line, col
}
