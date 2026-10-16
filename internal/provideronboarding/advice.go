package provideronboarding

import (
	"strconv"
	"strings"
	"unicode"

	"github.com/Gitlawb/zero/internal/config"
	"github.com/Gitlawb/zero/internal/providercatalog"
)

type Action struct {
	Label   string
	Command string
	Detail  string
}

type ProviderState struct {
	Profile config.ProviderProfile
	Active  bool
}

func (state ProviderState) Actions() []Action {
	return ProviderActions(state.Profile, state.Active)
}

func SetupCommand(descriptor providercatalog.Descriptor, name string, setActive bool) string {
	return setupCommand(descriptor, name, "", setActive)
}

// SetupCommandWithModel is SetupCommand with an explicit --model. A local
// runtime serves whichever model the user loaded, so its catalog DefaultModel is
// only a placeholder: an adopt command that omits --model persists that
// placeholder and the first completion fails with an unknown-model response.
// An empty model falls back to SetupCommand's behaviour. Commands that cannot
// be represented safely across supported shells are omitted.
func SetupCommandWithModel(descriptor providercatalog.Descriptor, name string, model string, setActive bool) string {
	return setupCommand(descriptor, name, model, setActive)
}

func setupCommand(descriptor providercatalog.Descriptor, name string, model string, setActive bool) string {
	parts := []string{"zero", "providers", "add", strings.TrimSpace(descriptor.ID)}
	if name = strings.TrimSpace(name); name != "" {
		parts = append(parts, "--name", name)
	}
	if model = strings.TrimSpace(model); model != "" {
		// A separate operand beginning with '-' is rejected as an option.
		if strings.HasPrefix(model, "-") {
			parts = append(parts, "--model="+model)
		} else {
			parts = append(parts, "--model", model)
		}
	}
	if descriptor.RequiresAuth && len(descriptor.AuthEnvVars) > 0 {
		if env := strings.TrimSpace(descriptor.AuthEnvVars[0]); env != "" {
			parts = append(parts, "--api-key-env", env)
		}
	}
	if setActive {
		parts = append(parts, "--set-active")
	}
	return joinSetupCommand(parts)
}

func UseCommand(name string) string {
	parts := []string{"zero", "providers", "use"}
	if name = strings.TrimSpace(name); name != "" {
		parts = append(parts, name)
	}
	return joinCommand(parts)
}

func CheckCommand(name string, connectivity bool) string {
	parts := []string{"zero", "providers", "check"}
	if name = strings.TrimSpace(name); name != "" {
		parts = append(parts, name)
	}
	if connectivity {
		parts = append(parts, "--connectivity")
	}
	return joinCommand(parts)
}

func MissingCredentialAction(profile config.ProviderProfile) (Action, bool) {
	advice := credentialAdviceForProfile(profile)
	if !advice.requiresAuth || providerProfileHasCredential(profile) {
		return Action{}, false
	}

	detail := "Set an API key before using this provider."
	command := "set API_KEY in your shell"
	if advice.envVar != "" {
		detail = "Set " + advice.envVar + " to your provider API key before using this provider."
		command = "set " + advice.envVar + " in your shell"
	}
	return Action{
		Label:   "Set API key",
		Command: command,
		Detail:  detail,
	}, true
}

func ProviderActions(profile config.ProviderProfile, active bool) []Action {
	name := strings.TrimSpace(profile.Name)
	actions := make([]Action, 0, 3)
	if name != "" && !active {
		actions = append(actions, Action{
			Label:   "Use provider",
			Command: UseCommand(name),
			Detail:  "Make " + name + " the active provider.",
		})
	}
	if name != "" {
		actions = append(actions, Action{
			Label:   "Check provider",
			Command: CheckCommand(name, false),
			Detail:  "Validate the provider profile without probing network connectivity.",
		})
	}
	if action, ok := MissingCredentialAction(profile); ok {
		actions = append(actions, action)
	}
	return actions
}

type credentialAdvice struct {
	requiresAuth bool
	envVar       string
}

func credentialAdviceForProfile(profile config.ProviderProfile) credentialAdvice {
	profileEnv := strings.TrimSpace(profile.APIKeyEnv)
	if catalogID := strings.TrimSpace(profile.CatalogID); catalogID != "" {
		if descriptor, err := providercatalog.Require(catalogID); err == nil {
			return credentialAdvice{
				requiresAuth: descriptor.RequiresAuth,
				envVar:       firstNonEmpty(profileEnv, firstAuthEnvVar(descriptor)),
			}
		}
	}

	switch effectiveProviderKind(profile) {
	case config.ProviderKindOpenAI:
		return credentialAdvice{requiresAuth: true, envVar: firstNonEmpty(profileEnv, "OPENAI_API_KEY")}
	case config.ProviderKindAnthropic:
		return credentialAdvice{requiresAuth: true, envVar: firstNonEmpty(profileEnv, "ANTHROPIC_API_KEY")}
	case config.ProviderKindGoogle:
		return credentialAdvice{requiresAuth: true, envVar: firstNonEmpty(profileEnv, "GEMINI_API_KEY")}
	default:
		return credentialAdvice{requiresAuth: profileEnv != "", envVar: profileEnv}
	}
}

func effectiveProviderKind(profile config.ProviderProfile) config.ProviderKind {
	if kind := strings.TrimSpace(string(profile.ProviderKind)); kind != "" {
		return config.ProviderKind(strings.ToLower(kind))
	}
	if provider := strings.TrimSpace(profile.Provider); provider != "" {
		return config.ProviderKind(strings.ToLower(provider))
	}
	return ""
}

func providerProfileHasCredential(profile config.ProviderProfile) bool {
	return strings.TrimSpace(profile.APIKey) != "" || strings.TrimSpace(profile.AuthHeaderValue) != ""
}

func firstAuthEnvVar(descriptor providercatalog.Descriptor) string {
	for _, env := range descriptor.AuthEnvVars {
		if env = strings.TrimSpace(env); env != "" {
			return env
		}
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

// joinSetupCommand only emits arguments supported literally by POSIX shells,
// cmd.exe, and PowerShell. Shell-specific quoting cannot safely cover all three.
// Return no command when a value requires it; callers can offer interactive setup.
func joinSetupCommand(parts []string) string {
	quoted := make([]string, 0, len(parts))
	for _, part := range parts {
		if part = strings.TrimSpace(part); part == "" {
			continue
		}
		arg := setupCommandArg(part)
		if value, inlineModel := strings.CutPrefix(part, "--model="); inlineModel {
			// Validate the untrusted model separately from the fixed separator;
			// '=' must not become an allowed character in model IDs.
			arg = setupCommandArg(value)
			if arg != "" {
				arg = "--model=" + arg
			}
		}
		if arg == "" {
			return ""
		}
		quoted = append(quoted, arg)
	}
	return strings.Join(quoted, " ")
}

func setupCommandArg(value string) string {
	for i, r := range value {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			continue
		}
		switch r {
		case '-', '_', '.', '/', ':', ' ':
			continue
		case '@':
			// A leading @ starts splatting in PowerShell.
			if i > 0 {
				continue
			}
		}
		return ""
	}
	if value == "" || strings.Contains(value, " ") {
		return `"` + value + `"`
	}
	return value
}

func joinCommand(parts []string) string {
	quoted := make([]string, 0, len(parts))
	for _, part := range parts {
		if part = strings.TrimSpace(part); part != "" {
			quoted = append(quoted, commandArg(part))
		}
	}
	return strings.Join(quoted, " ")
}

func commandArg(value string) string {
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
