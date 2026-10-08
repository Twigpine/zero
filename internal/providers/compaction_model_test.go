package providers

import (
	"context"
	"errors"
	"testing"

	"github.com/Gitlawb/zero/internal/config"
	"github.com/Gitlawb/zero/internal/zeroruntime"
)

func TestCompactionModelIDResolutionOrder(t *testing.T) {
	anthropic := config.ProviderProfile{
		Name:         "anthropic",
		ProviderKind: config.ProviderKindAnthropic,
		Model:        "claude-sonnet-4.5",
	}

	t.Setenv(CompactionModelEnv, "my-env-model")
	if got := CompactionModelID(anthropic, "my-config-model"); got != "my-env-model" {
		t.Fatalf("env must win, got %q", got)
	}

	t.Setenv(CompactionModelEnv, "main")
	if got := CompactionModelID(anthropic, "my-config-model"); got != "" {
		t.Fatalf("env 'main' must force the main model, got %q", got)
	}

	t.Setenv(CompactionModelEnv, "")
	if got := CompactionModelID(anthropic, "my-config-model"); got != "my-config-model" {
		t.Fatalf("config preference must apply when env unset, got %q", got)
	}
	if got := CompactionModelID(anthropic, "main"); got != "" {
		t.Fatalf("config 'main' must force the main model, got %q", got)
	}
}

func TestCompactionModelIDCuratedDefaults(t *testing.T) {
	t.Setenv(CompactionModelEnv, "")

	anthropic := config.ProviderProfile{Name: "anthropic", ProviderKind: config.ProviderKindAnthropic, Model: "claude-sonnet-4.5"}
	if got := CompactionModelID(anthropic, ""); got != defaultAnthropicCompactionModel {
		t.Fatalf("anthropic default = %q, want %q", got, defaultAnthropicCompactionModel)
	}

	google := config.ProviderProfile{Name: "google", ProviderKind: config.ProviderKindGoogle, Model: "gemini-2.5-pro"}
	if got := CompactionModelID(google, ""); got != defaultGoogleCompactionModel {
		t.Fatalf("google default = %q, want %q", got, defaultGoogleCompactionModel)
	}

	openai := config.ProviderProfile{Name: "openai", ProviderKind: config.ProviderKindOpenAI, Model: "gpt-4.1"}
	if got := CompactionModelID(openai, ""); got != defaultOpenAICompactionModel {
		t.Fatalf("openai default = %q, want %q", got, defaultOpenAICompactionModel)
	}

	// Custom/compatible endpoints: catalog unknowable, no default.
	compatible := config.ProviderProfile{
		Name:         "opengateway",
		ProviderKind: config.ProviderKindOpenAICompatible,
		BaseURL:      "https://opengateway.gitlawb.com/v1",
		Model:        "some-model",
	}
	if got := CompactionModelID(compatible, ""); got != "" {
		t.Fatalf("openai-compatible must have no default, got %q", got)
	}
}

func TestCompactionModelIDSkipsWhenMainIsAlreadyCheap(t *testing.T) {
	t.Setenv(CompactionModelEnv, "")
	haiku := config.ProviderProfile{
		Name:         "anthropic",
		ProviderKind: config.ProviderKindAnthropic,
		Model:        defaultAnthropicCompactionModel,
	}
	if got := CompactionModelID(haiku, ""); got != "" {
		t.Fatalf("already-cheap session must not get a dedicated summarizer, got %q", got)
	}
}

func TestCompactionSummarizerFactoryTracksMainModel(t *testing.T) {
	t.Setenv(CompactionModelEnv, "")
	base := config.ProviderProfile{Name: "anthropic", ProviderKind: config.ProviderKindAnthropic, Model: defaultAnthropicCompactionModel}
	var built []string
	newProvider := func(profile config.ProviderProfile) (zeroruntime.Provider, error) {
		built = append(built, profile.Model)
		return nil, errors.New("not needed")
	}
	factory := CompactionSummarizerFactory(base, "", newProvider)
	if factory == nil {
		t.Fatal("factory must exist when a provider builder exists")
	}
	// Main model is the cheap default: no dedicated summarizer, nothing built.
	if provider, err := factory(context.Background(), defaultAnthropicCompactionModel); provider != nil || err != nil || len(built) != 0 {
		t.Fatalf("cheap main model must yield (nil, nil) without building, got %v %v built=%v", provider, err, built)
	}
	// After escalating to an expensive model the cheap default is built.
	if _, err := factory(context.Background(), "claude-sonnet-4-5"); err == nil || len(built) != 1 || built[0] != defaultAnthropicCompactionModel {
		t.Fatalf("escalated main model must build the cheap summarizer, built=%v err=%v", built, err)
	}
	// An empty main model falls back to the base profile's model — the cheap
	// default here, so again nothing is built.
	built = nil
	if provider, err := factory(context.Background(), ""); provider != nil || err != nil || len(built) != 0 {
		t.Fatalf("empty main model must use the base profile, got %v %v built=%v", provider, err, built)
	}
	// The explicit "main" preference disables the dedicated summarizer.
	if provider, err := CompactionSummarizerFactory(base, "main", newProvider)(context.Background(), "claude-sonnet-4-5"); provider != nil || err != nil {
		t.Fatalf("preference main must disable the summarizer, got %v %v", provider, err)
	}
	if CompactionSummarizerFactory(base, "", nil) != nil {
		t.Fatal("no provider builder means no factory")
	}
}
