package provideronboarding

import (
	"os/exec"
	"runtime"
	"strings"
	"testing"

	"github.com/Gitlawb/zero/internal/config"
	"github.com/Gitlawb/zero/internal/providercatalog"
)

func TestSetupCommandUsesCatalogEnvAndSetActive(t *testing.T) {
	groq, err := providercatalog.Require("groq")
	if err != nil {
		t.Fatalf("Require(groq) returned error: %v", err)
	}

	got := SetupCommand(groq, "fast", true)
	want := "zero providers add groq --name fast --api-key-env GROQ_API_KEY --set-active"
	if got != want {
		t.Fatalf("SetupCommand() = %q, want %q", got, want)
	}
}

func TestSetupCommandForOpenAICustomAndLocalProviders(t *testing.T) {
	tests := []struct {
		name       string
		catalogID  string
		profile    string
		setActive  bool
		want       string
		notWantArg string
	}{
		{
			name:      "openai",
			catalogID: "openai",
			profile:   "openai",
			want:      "zero providers add openai --name openai --api-key-env OPENAI_API_KEY",
		},
		{
			name:      "custom openai compatible",
			catalogID: "custom-openai-compatible",
			profile:   "custom",
			want:      "zero providers add custom-openai-compatible --name custom --api-key-env OPENAI_API_KEY",
		},
		{
			name:       "local provider",
			catalogID:  "ollama",
			profile:    "local",
			want:       "zero providers add ollama --name local",
			notWantArg: "--api-key-env",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			descriptor, err := providercatalog.Require(tt.catalogID)
			if err != nil {
				t.Fatalf("Require(%q) returned error: %v", tt.catalogID, err)
			}

			got := SetupCommand(descriptor, tt.profile, tt.setActive)
			if got != tt.want {
				t.Fatalf("SetupCommand() = %q, want %q", got, tt.want)
			}
			if tt.notWantArg != "" && strings.Contains(got, tt.notWantArg) {
				t.Fatalf("SetupCommand() = %q, did not want %q", got, tt.notWantArg)
			}
		})
	}
}

func TestUseAndCheckCommands(t *testing.T) {
	if got, want := UseCommand("fast"), "zero providers use fast"; got != want {
		t.Fatalf("UseCommand() = %q, want %q", got, want)
	}
	if got, want := CheckCommand("fast", false), "zero providers check fast"; got != want {
		t.Fatalf("CheckCommand(false) = %q, want %q", got, want)
	}
	if got, want := CheckCommand("fast", true), "zero providers check fast --connectivity"; got != want {
		t.Fatalf("CheckCommand(true) = %q, want %q", got, want)
	}
}

func TestMissingCredentialActionUsesCatalogEnvWithoutSecrets(t *testing.T) {
	profile := config.ProviderProfile{
		Name:      "fast",
		CatalogID: "groq",
	}

	action, ok := MissingCredentialAction(profile)
	if !ok {
		t.Fatalf("MissingCredentialAction() ok = false, want true")
	}
	if action.Label != "Set API key" {
		t.Fatalf("Label = %q, want %q", action.Label, "Set API key")
	}
	if action.Command != "set GROQ_API_KEY in your shell" {
		t.Fatalf("Command = %q, want shell-neutral env guidance", action.Command)
	}
	if !strings.Contains(action.Detail, "GROQ_API_KEY") {
		t.Fatalf("Detail = %q, want env var mention", action.Detail)
	}
	assertNoSecretLeak(t, []Action{action}, "sk-live-secret", "Bearer actual-token")
}

func TestMissingCredentialActionUsesBuiltInProviderDefaults(t *testing.T) {
	tests := []struct {
		name string
		kind config.ProviderKind
		env  string
	}{
		{name: "openai", kind: config.ProviderKindOpenAI, env: "OPENAI_API_KEY"},
		{name: "anthropic", kind: config.ProviderKindAnthropic, env: "ANTHROPIC_API_KEY"},
		{name: "google", kind: config.ProviderKindGoogle, env: "GEMINI_API_KEY"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			action, ok := MissingCredentialAction(config.ProviderProfile{
				Name:         tt.name,
				ProviderKind: tt.kind,
			})
			if !ok {
				t.Fatalf("MissingCredentialAction() ok = false, want true")
			}
			if !strings.Contains(action.Detail, tt.env) {
				t.Fatalf("Detail = %q, want env var %q", action.Detail, tt.env)
			}
			if !strings.Contains(action.Command, tt.env) {
				t.Fatalf("Command = %q, want env var %q", action.Command, tt.env)
			}
		})
	}
}

func TestMissingCredentialActionSkipsLocalAndCredentialedProfiles(t *testing.T) {
	tests := []struct {
		name    string
		profile config.ProviderProfile
	}{
		{
			name: "local catalog provider",
			profile: config.ProviderProfile{
				Name:      "local",
				CatalogID: "ollama",
			},
		},
		{
			name: "api key is present",
			profile: config.ProviderProfile{
				Name:         "openai",
				ProviderKind: config.ProviderKindOpenAI,
				APIKey:       "sk-live-secret",
			},
		},
		{
			name: "auth header value is present",
			profile: config.ProviderProfile{
				Name:            "custom",
				CatalogID:       "custom-openai-compatible",
				AuthHeaderValue: "Bearer actual-token",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			action, ok := MissingCredentialAction(tt.profile)
			if ok {
				t.Fatalf("MissingCredentialAction() = (%#v, true), want false", action)
			}
		})
	}
}

func TestProviderActionsIncludesExpectedActions(t *testing.T) {
	profile := config.ProviderProfile{
		Name:      "fast",
		CatalogID: "groq",
	}

	actions := ProviderActions(profile, false)
	wantCommands := []string{
		"zero providers use fast",
		"zero providers check fast",
		"set GROQ_API_KEY in your shell",
	}
	if len(actions) != len(wantCommands) {
		t.Fatalf("ProviderActions() returned %d actions, want %d: %#v", len(actions), len(wantCommands), actions)
	}
	for index, want := range wantCommands {
		if actions[index].Command != want {
			t.Fatalf("actions[%d].Command = %q, want %q", index, actions[index].Command, want)
		}
	}

	activeActions := ProviderActions(profile, true)
	for _, action := range activeActions {
		if action.Command == "zero providers use fast" {
			t.Fatalf("ProviderActions(active) included use action: %#v", activeActions)
		}
	}
}

func TestProviderActionsDoNotLeakStoredSecrets(t *testing.T) {
	const apiKey = "sk-live-secret"
	const headerValue = "Bearer actual-token"
	profile := config.ProviderProfile{
		Name:            "secure",
		ProviderKind:    config.ProviderKindOpenAI,
		APIKey:          apiKey,
		AuthHeaderValue: headerValue,
	}

	actions := ProviderActions(profile, false)
	assertNoSecretLeak(t, actions, apiKey, headerValue)
}

func assertNoSecretLeak(t *testing.T, actions []Action, secrets ...string) {
	t.Helper()
	for _, action := range actions {
		for _, secret := range secrets {
			if secret == "" {
				continue
			}
			if strings.Contains(action.Label, secret) || strings.Contains(action.Command, secret) || strings.Contains(action.Detail, secret) {
				t.Fatalf("action leaked secret %q: %#v", secret, action)
			}
		}
	}
}

func TestSetupCommandWithModelShellRoundTrip(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell round-trip; portable command boundary is tested on every OS")
	}
	for _, id := range []string{"my loaded model", "unsloth/Qwen3-GGUF", "model:latest", "user/model@revision"} {
		command := SetupCommandWithModel(providercatalog.Descriptor{ID: "atomic-chat-local"}, "Atomic Chat Local", id, true)
		out, err := exec.Command("sh", "-c", `zero() { printf '%s\n' "$@"; }; `+command).CombinedOutput()
		want := "providers\nadd\natomic-chat-local\n--name\nAtomic Chat Local\n--model\n" + id + "\n--set-active\n"
		if err != nil || string(out) != want {
			t.Fatalf("argument round-trip: command=%q output=%q err=%v", command, out, err)
		}
	}
}

func TestSetupCommandWithModelOmitsUnsafeArguments(t *testing.T) {
	for _, value := range []string{"x&calc", `a"&calc&"b`, "$(id)", "`id`", "%PATH%", "!PATH!", "a;b", "a|b", "a^b", "a>b", "a\nb", "@args", "a\u201db", "-x&calc", `-a"&calc&"b`, "-$(id)", "-%PATH%", "-model=value"} {
		for _, field := range []string{"model", "name"} {
			name, model := "local", "loaded/model"
			if field == "model" {
				model = value
			} else {
				name = value
			}
			if got := SetupCommandWithModel(providercatalog.Descriptor{ID: "atomic-chat-local"}, name, model, true); got != "" {
				t.Fatalf("unsafe %s %q emitted as a shell command: %q", field, value, got)
			}
		}
	}
}
