package cli

import (
	"bytes"
	"strconv"
	"strings"
	"testing"
)

func TestProviderCatalogRuntimeProvidersShowOnboardingSetupHints(t *testing.T) {
	output := runProviderCatalogOnboarding(t)

	tests := []struct {
		id           string
		name         string
		transport    string
		defaultModel string
	}{
		{id: "openai", name: "OpenAI", transport: "openai", defaultModel: "gpt-4.1"},
		{id: "groq", name: "Groq", transport: "openai-compatible", defaultModel: "llama-3.3-70b-versatile"},
		{id: "longcat", name: "LongCat", transport: "openai-compatible", defaultModel: "LongCat-2.0"},
		{id: "fireworks", name: "Fireworks AI", transport: "openai-compatible", defaultModel: "accounts/fireworks/models/kimi-k2p7-code"},
		{id: "gmicloud", name: "GMI Cloud", transport: "openai-compatible", defaultModel: "Qwen/Qwen3.8-Max"},
		{id: "concentrate", name: "Concentrate", transport: "openai-compatible", defaultModel: "deepseek-v4-flash-0731"},
		{id: "ollama-cloud", name: "Ollama Cloud", transport: "openai-compatible", defaultModel: "qwen3-coder:480b"},
		{id: "ollama", name: "Ollama Local", transport: "openai-compatible", defaultModel: "llama3.1"},
	}
	for _, tt := range tests {
		t.Run(tt.id, func(t *testing.T) {
			block := providerCatalogOnboardingBlock(t, output, tt.id)
			nameLine := "name=" + tt.name
			if strings.Contains(tt.name, " ") {
				nameLine = "name=" + strconv.Quote(tt.name)
			}
			for _, want := range []string{
				"id=" + tt.id,
				nameLine,
				"transport=" + tt.transport,
				"defaultModel=" + tt.defaultModel,
				"setup: zero providers setup " + tt.id + " --set-active",
			} {
				if !strings.Contains(block, want) {
					t.Fatalf("expected %s catalog block to contain %q, got:\n%s", tt.id, want, block)
				}
			}
			if strings.Contains(block, "unsupported:") {
				t.Fatalf("runtime-supported provider %s should not show unsupported reason, got:\n%s", tt.id, block)
			}
		})
	}
}

func TestProviderCatalogLongCatRequiresAPIKey(t *testing.T) {
	output := runProviderCatalogOnboarding(t)
	block := providerCatalogOnboardingBlock(t, output, "longcat")

	for _, want := range []string{
		"requiresAuth=true",
		"authEnvVars=LONGCAT_API_KEY",
		"setup: zero providers setup longcat --set-active",
	} {
		if !strings.Contains(block, want) {
			t.Fatalf("expected LongCat catalog block to contain %q, got:\n%s", want, block)
		}
	}
}

func TestProviderCatalogOllamaCloudRequiresAPIKey(t *testing.T) {
	output := runProviderCatalogOnboarding(t)
	block := providerCatalogOnboardingBlock(t, output, "ollama-cloud")

	for _, want := range []string{
		"requiresAuth=true",
		"authEnvVars=OLLAMA_API_KEY",
		"setup: zero providers setup ollama-cloud --set-active",
	} {
		if !strings.Contains(block, want) {
			t.Fatalf("expected ollama-cloud catalog block to contain %q, got:\n%s", want, block)
		}
	}
}

func TestProviderCatalogLocalProviderOnboardingDoesNotRequireAuthEnv(t *testing.T) {
	output := runProviderCatalogOnboarding(t)
	block := providerCatalogOnboardingBlock(t, output, "ollama")

	if strings.Contains(block, "authEnvVars=") {
		t.Fatalf("local provider should not show an auth env var summary, got:\n%s", block)
	}
	if strings.Contains(block, "requiresAuth=true") {
		t.Fatalf("local provider should not require auth, got:\n%s", block)
	}
}

func TestProviderCatalogUnsupportedProvidersShowOnboardingReason(t *testing.T) {
	output := runProviderCatalogOnboarding(t)

	for _, id := range []string{"bedrock", "vertex"} {
		t.Run(id, func(t *testing.T) {
			block := providerCatalogOnboardingBlock(t, output, id)
			if !strings.Contains(block, "unsupported: native adapter not implemented yet") {
				t.Fatalf("expected %s catalog block to show unsupported reason, got:\n%s", id, block)
			}
			if strings.Contains(block, "setup: zero providers setup") {
				t.Fatalf("unsupported provider %s should not show setup hint, got:\n%s", id, block)
			}
		})
	}
}

func runProviderCatalogOnboarding(t *testing.T) string {
	t.Helper()

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := runWithDeps([]string{"providers", "catalog"}, &stdout, &stderr, providerCatalogDeps(t))
	if exitCode != exitSuccess {
		t.Fatalf("expected exit code %d, got %d: %s", exitSuccess, exitCode, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("expected empty stderr, got %q", stderr.String())
	}
	return stdout.String()
}

func providerCatalogOnboardingBlock(t *testing.T, output string, id string) string {
	t.Helper()

	lines := strings.Split(strings.TrimSuffix(output, "\n"), "\n")
	start := -1
	for index, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "id="+id) {
			start = index
			break
		}
	}
	if start == -1 {
		t.Fatalf("catalog block for provider %q not found in:\n%s", id, output)
	}
	end := len(lines)
	for index := start + 1; index < len(lines); index++ {
		if strings.HasPrefix(strings.TrimSpace(lines[index]), "id=") {
			end = index
			break
		}
	}
	return strings.Join(lines[start:end], "\n")
}
