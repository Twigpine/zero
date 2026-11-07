package cli

import (
	"reflect"
	"testing"

	"github.com/Twigpine/zero/internal/config"
)

func TestProviderSensitiveEnvKeys(t *testing.T) {
	resolved := config.ResolvedConfig{
		Providers: []config.ProviderProfile{
			{Name: "custom", APIKeyEnv: " COMPANY_LLM_SECRET "},
			{Name: "catalog", APIKeyEnv: "OPENAI_API_KEY"},
			{Name: "inline"},
		},
		Provider: config.ProviderProfile{Name: "active", APIKeyEnv: "ACTIVE_PROVIDER_SECRET"},
	}
	want := []string{"COMPANY_LLM_SECRET", "OPENAI_API_KEY", "ACTIVE_PROVIDER_SECRET"}
	if got := providerSensitiveEnvKeys(resolved); !reflect.DeepEqual(got, want) {
		t.Fatalf("providerSensitiveEnvKeys() = %v, want %v", got, want)
	}
}
