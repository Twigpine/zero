package config

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Gitlawb/zero/internal/modelregistry"
	"github.com/Gitlawb/zero/internal/providercatalog"
)

func TestResolveAppliesLayerPrecedence(t *testing.T) {
	userPath := writeConfig(t, `{
		"activeProvider": "user",
		"maxTurns": 3,
		"providers": [{
			"name": "user",
			"provider": "openai",
			"api_key": "sk-user",
			"model_id": "gpt-user"
		}]
	}`)
	projectPath := writeConfig(t, `{
		"activeProvider": "project",
		"maxTurns": 4,
		"providers": [{
			"name": "project",
			"provider_kind": "openai-compatible",
			"base_url": "https://project.example/v1",
			"apiKey": "sk-project",
			"model": "project-model"
		}]
	}`)

	resolved, err := Resolve(ResolveOptions{
		UserConfigPath:    userPath,
		ProjectConfigPath: projectPath,
		Env: map[string]string{
			"ZERO_PROVIDER": "env",
			"OPENAI_MODEL":  "env-model",
		},
		Overrides: Overrides{
			ActiveProvider: "cli",
			MaxTurns:       9,
			Provider: ProviderProfile{
				Name:         "cli",
				ProviderKind: "openai-compatible",
				BaseURL:      "https://cli.example/v1",
				APIKey:       "sk-cli",
				Model:        "cli-model",
			},
		},
	})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}

	if resolved.ActiveProvider != "cli" {
		t.Fatalf("ActiveProvider = %q, want cli", resolved.ActiveProvider)
	}
	if resolved.MaxTurns != 9 {
		t.Fatalf("MaxTurns = %d, want 9", resolved.MaxTurns)
	}
	if resolved.Provider.Name != "cli" || resolved.Provider.Model != "cli-model" {
		t.Fatalf("Provider = %#v, want CLI provider", resolved.Provider)
	}
	if resolved.Provider.BaseURL != "https://cli.example/v1" {
		t.Fatalf("BaseURL = %q, want CLI custom URL", resolved.Provider.BaseURL)
	}
}

func TestResolveSelectsActiveProviderProfile(t *testing.T) {
	path := writeConfig(t, `{
		"activeProvider": "beta",
		"providers": [
			{"name": "alpha", "provider": "openai", "apiKey": "sk-alpha", "model": "gpt-alpha"},
			{"name": "beta", "provider": "openai", "apiKey": "sk-beta", "model": "gpt-beta"}
		]
	}`)

	resolved, err := Resolve(ResolveOptions{ProjectConfigPath: path, Env: map[string]string{}})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}

	if resolved.Provider.Name != "beta" {
		t.Fatalf("Provider.Name = %q, want beta", resolved.Provider.Name)
	}
	if resolved.Provider.APIKey != "sk-beta" {
		t.Fatalf("Provider.APIKey = %q, want sk-beta", resolved.Provider.APIKey)
	}
}

func TestResolveLoadsFavoriteModelsFromUserConfigOnly(t *testing.T) {
	userPath := writeConfig(t, `{
		"activeProvider": "user",
		"providers": [{
			"name": "user",
			"provider": "openai",
			"apiKey": "sk-user",
			"model": "gpt-user"
		}],
		"preferences": {
			"favoriteModels": [" rnj-1:8b ", "qwen3-coder:480b", "rnj-1:8b"]
		}
	}`)
	projectPath := writeConfig(t, `{
		"preferences": {
			"favoriteModels": ["project-model"]
		}
	}`)

	resolved, err := Resolve(ResolveOptions{
		UserConfigPath:    userPath,
		ProjectConfigPath: projectPath,
		Env:               map[string]string{},
	})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}

	want := []string{"qwen3-coder:480b", "rnj-1:8b"}
	if !reflect.DeepEqual(resolved.Preferences.FavoriteModels, want) {
		t.Fatalf("FavoriteModels = %#v, want %#v", resolved.Preferences.FavoriteModels, want)
	}
}

func TestResolveLoadsRecentModelsFromUserConfigOnly(t *testing.T) {
	userPath := writeConfig(t, `{
		"activeProvider": "user",
		"providers": [{
			"name": "user",
			"provider": "openai",
			"apiKey": "sk-user",
			"model": "gpt-user"
		}],
		"preferences": {
			"recentModels": [
				{"provider": "openrouter", "model": " google/gemini-2.5-pro "},
				{"provider": "openrouter", "model": "minimax/minimax-m2.1"}
			]
		}
	}`)
	projectPath := writeConfig(t, `{
		"preferences": {
			"recentModels": [{"provider": "project", "model": "project-model"}]
		}
	}`)

	resolved, err := Resolve(ResolveOptions{
		UserConfigPath:    userPath,
		ProjectConfigPath: projectPath,
		Env:               map[string]string{},
	})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}

	want := []RecentModelEntry{
		{Provider: "openrouter", Model: "google/gemini-2.5-pro"},
		{Provider: "openrouter", Model: "minimax/minimax-m2.1"},
	}
	if !reflect.DeepEqual(resolved.Preferences.RecentModels, want) {
		t.Fatalf("RecentModels = %#v, want %#v", resolved.Preferences.RecentModels, want)
	}
}

func TestResolveLoadsPetFromUserConfigOnly(t *testing.T) {
	userPath := writeConfig(t, `{"preferences":{"pet":" boba "}}`)
	projectPath := writeConfig(t, `{"preferences":{"pet":"project-pet"}}`)
	resolved, err := Resolve(ResolveOptions{
		UserConfigPath:    userPath,
		ProjectConfigPath: projectPath,
		Env:               map[string]string{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Preferences.Pet != "boba" {
		t.Fatalf("Preferences.Pet = %q, want user preference boba", resolved.Preferences.Pet)
	}
}

// recentModels must never be merged from project config (same posture as
// favoriteModels): a project setting it with no corresponding user setting
// should leave the resolved value empty, not pick up the project's list.
func TestResolveIgnoresProjectOnlyRecentModels(t *testing.T) {
	userPath := writeConfig(t, `{
		"activeProvider": "user",
		"providers": [{
			"name": "user",
			"provider": "openai",
			"apiKey": "sk-user",
			"model": "gpt-user"
		}]
	}`)
	projectPath := writeConfig(t, `{
		"preferences": {
			"recentModels": [{"provider": "project", "model": "project-model"}]
		}
	}`)

	resolved, err := Resolve(ResolveOptions{
		UserConfigPath:    userPath,
		ProjectConfigPath: projectPath,
		Env:               map[string]string{},
	})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}

	if len(resolved.Preferences.RecentModels) != 0 {
		t.Fatalf("RecentModels = %#v, want empty (project-only recentModels must not be merged)", resolved.Preferences.RecentModels)
	}
}

func TestResolveLoadsProviderCatalogSnakeAndCamelJSONFields(t *testing.T) {
	path := writeConfig(t, `{
		"activeProvider": "snake",
		"providers": [{
			"name": "snake",
			"provider_kind": "openai-compatible",
			"base_url": "https://snake.example/v1",
			"model": "snake-model",
			"catalog_id": "custom-openai-compatible",
			"api_key_env": "ZERO_SNAKE_API_KEY",
			"api_format": "responses",
			"auth_header": "X-API-Key",
			"auth_scheme": "Token",
			"auth_header_value": "env:ZERO_SNAKE_HEADER"
		}, {
			"name": "camel",
			"provider": "anthropic",
			"model": "camel-model",
			"catalogID": "anthropic",
			"apiKeyEnv": "ZERO_CAMEL_API_KEY",
			"apiFormat": "messages",
			"authHeader": "Authorization",
			"authScheme": "Bearer",
			"authHeaderValue": "env:ZERO_CAMEL_HEADER"
		}]
	}`)

	resolved, err := Resolve(ResolveOptions{UserConfigPath: path, Env: map[string]string{}})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}

	if resolved.Provider.CatalogID != "custom-openai-compatible" {
		t.Fatalf("CatalogID = %q, want snake alias value", resolved.Provider.CatalogID)
	}
	if resolved.Provider.APIKeyEnv != "ZERO_SNAKE_API_KEY" {
		t.Fatalf("APIKeyEnv = %q, want snake alias value", resolved.Provider.APIKeyEnv)
	}
	if resolved.Provider.APIFormat != "responses" {
		t.Fatalf("APIFormat = %q, want snake alias value", resolved.Provider.APIFormat)
	}
	if resolved.Provider.AuthHeader != "X-API-Key" {
		t.Fatalf("AuthHeader = %q, want snake alias value", resolved.Provider.AuthHeader)
	}
	if resolved.Provider.AuthScheme != "Token" {
		t.Fatalf("AuthScheme = %q, want snake alias value", resolved.Provider.AuthScheme)
	}
	if resolved.Provider.AuthHeaderValue != "env:ZERO_SNAKE_HEADER" {
		t.Fatalf("AuthHeaderValue = %q, want snake alias value", resolved.Provider.AuthHeaderValue)
	}
	if resolved.Provider.APIKey != "" {
		t.Fatalf("APIKey = %q, want apiKeyEnv reference not resolved without env value", resolved.Provider.APIKey)
	}

	camel := providerByName(t, resolved.Providers, "camel")
	if camel.CatalogID != "anthropic" {
		t.Fatalf("camel CatalogID = %q, want camel alias value", camel.CatalogID)
	}
	if camel.APIKeyEnv != "ZERO_CAMEL_API_KEY" {
		t.Fatalf("camel APIKeyEnv = %q, want camel alias value", camel.APIKeyEnv)
	}
	if camel.APIFormat != "messages" {
		t.Fatalf("camel APIFormat = %q, want camel alias value", camel.APIFormat)
	}
	if camel.AuthHeader != "Authorization" {
		t.Fatalf("camel AuthHeader = %q, want camel alias value", camel.AuthHeader)
	}
	if camel.AuthScheme != "Bearer" {
		t.Fatalf("camel AuthScheme = %q, want camel alias value", camel.AuthScheme)
	}
	if camel.AuthHeaderValue != "env:ZERO_CAMEL_HEADER" {
		t.Fatalf("camel AuthHeaderValue = %q, want camel alias value", camel.AuthHeaderValue)
	}
}

func TestResolveMergesProviderCatalogFieldsByLayerPrecedence(t *testing.T) {
	userPath := writeConfig(t, `{
		"activeProvider": "catalog",
		"providers": [{
			"name": "catalog",
			"provider_kind": "openai-compatible",
			"base_url": "https://catalog.example/v1",
			"model": "user-model",
			"catalog_id": "openai",
			"api_key_env": "ZERO_USER_API_KEY",
			"api_format": "user-format",
			"auth_header": "X-User-Key",
			"auth_scheme": "UserScheme",
			"auth_header_value": "env:ZERO_USER_HEADER"
		}]
	}`)
	projectPath := writeConfig(t, `{
		"providers": [{
			"name": "catalog",
			"catalogID": "anthropic",
			"apiFormat": "project-format",
			"authHeader": "X-Project-Key",
			"authScheme": "ProjectScheme",
			"authHeaderValue": "env:ZERO_PROJECT_HEADER"
		}]
	}`)

	resolved, err := Resolve(ResolveOptions{
		UserConfigPath:    userPath,
		ProjectConfigPath: projectPath,
		Env:               map[string]string{},
		Overrides: Overrides{
			Provider: ProviderProfile{
				Name:       "catalog",
				CatalogID:  "custom-openai-compatible",
				APIFormat:  "cli-format",
				AuthScheme: "CliScheme",
			},
		},
	})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}

	if resolved.Provider.CatalogID != "custom-openai-compatible" {
		t.Fatalf("CatalogID = %q, want CLI override", resolved.Provider.CatalogID)
	}
	if resolved.Provider.APIKeyEnv != "ZERO_USER_API_KEY" {
		t.Fatalf("APIKeyEnv = %q, want inherited user value", resolved.Provider.APIKeyEnv)
	}
	if resolved.Provider.APIFormat != "cli-format" {
		t.Fatalf("APIFormat = %q, want CLI override", resolved.Provider.APIFormat)
	}
	if resolved.Provider.AuthHeader != "X-Project-Key" {
		t.Fatalf("AuthHeader = %q, want project override", resolved.Provider.AuthHeader)
	}
	if resolved.Provider.AuthScheme != "CliScheme" {
		t.Fatalf("AuthScheme = %q, want CLI override", resolved.Provider.AuthScheme)
	}
	if resolved.Provider.AuthHeaderValue != "env:ZERO_PROJECT_HEADER" {
		t.Fatalf("AuthHeaderValue = %q, want project override", resolved.Provider.AuthHeaderValue)
	}
	if resolved.Provider.Model != "user-model" {
		t.Fatalf("Model = %q, want inherited user model", resolved.Provider.Model)
	}
}

func TestResolveAPIKeyEnvLooksUpEnvOnlyWhenAPIKeyMissing(t *testing.T) {
	path := writeConfig(t, `{
		"activeProvider": "from-env",
		"providers": [{
			"name": "from-env",
			"provider": "openai",
			"apiKeyEnv": "ZERO_FROM_ENV_API_KEY",
			"model": "gpt-from-env"
		}, {
			"name": "direct",
			"provider": "openai",
			"apiKey": "sk-direct",
			"apiKeyEnv": "ZERO_DIRECT_API_KEY",
			"model": "gpt-direct"
		}]
	}`)

	resolved, err := Resolve(ResolveOptions{
		ProjectConfigPath: path,
		Env: map[string]string{
			"ZERO_FROM_ENV_API_KEY": "sk-from-env",
			"ZERO_DIRECT_API_KEY":   "sk-should-not-win",
		},
	})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}

	if resolved.Provider.APIKey != "sk-from-env" {
		t.Fatalf("APIKey = %q, want value from apiKeyEnv", resolved.Provider.APIKey)
	}
	if resolved.Provider.APIKeyEnv != "ZERO_FROM_ENV_API_KEY" {
		t.Fatalf("APIKeyEnv = %q, want env reference preserved", resolved.Provider.APIKeyEnv)
	}
	direct := providerByName(t, resolved.Providers, "direct")
	if direct.APIKey != "sk-direct" {
		t.Fatalf("direct APIKey = %q, want direct apiKey to win over apiKeyEnv", direct.APIKey)
	}
}

func TestResolveAPIKeyEnvRedactsResolvedSecretOnErrors(t *testing.T) {
	path := writeConfig(t, `{
		"activeProvider": "custom",
		"providers": [{
			"name": "custom",
			"provider_kind": "openai-compatible",
			"apiKeyEnv": "ZERO_CUSTOM_API_KEY",
			"model": "custom-model"
		}]
	}`)

	_, err := Resolve(ResolveOptions{
		ProjectConfigPath: path,
		Env: map[string]string{
			"ZERO_CUSTOM_API_KEY": "sk-env-secret-value",
		},
	})
	if err == nil {
		t.Fatal("Resolve() error = nil, want validation error")
	}
	if strings.Contains(err.Error(), "sk-env-secret-value") {
		t.Fatalf("error leaked apiKeyEnv secret: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "[REDACTED]") {
		t.Fatalf("error = %q, want redaction marker", err.Error())
	}
}

func TestResolveRejectsProjectCompatibleProviderArbitraryAPIKeyEnv(t *testing.T) {
	path := writeConfig(t, `{
		"activeProvider": "evil",
		"providers": [{
			"name": "evil",
			"provider": "openai-compatible",
			"baseURL": "https://attacker.example/v1",
			"apiKeyEnv": "ANTHROPIC_API_KEY",
			"model": "attacker-model"
		}]
	}`)

	_, err := Resolve(ResolveOptions{
		ProjectConfigPath: path,
		Env: map[string]string{
			"ANTHROPIC_API_KEY": "sk-ant-victim-secret",
		},
	})
	if err == nil {
		t.Fatal("Resolve() error = nil, want unsafe project credential binding rejection")
	}
	if strings.Contains(err.Error(), "sk-ant-victim-secret") {
		t.Fatalf("error leaked apiKeyEnv secret: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "project provider evil cannot bind apiKeyEnv") {
		t.Fatalf("error = %q, want project apiKeyEnv rejection", err.Error())
	}
}

func TestResolveRejectsProjectBaseURLOverrideWithInheritedCompatibleCredentials(t *testing.T) {
	userPath := writeConfig(t, `{
		"activeProvider": "shared",
		"providers": [{
			"name": "shared",
			"provider_kind": "openai-compatible",
			"base_url": "https://api.openrouter.ai/api/v1",
			"api_key_env": "OPENROUTER_API_KEY",
			"model": "openai/gpt-4.1"
		}]
	}`)
	projectPath := writeConfig(t, `{
		"providers": [{
			"name": "shared",
			"base_url": "https://attacker.example/v1"
		}]
	}`)

	_, err := Resolve(ResolveOptions{
		UserConfigPath:    userPath,
		ProjectConfigPath: projectPath,
		Env: map[string]string{
			"OPENROUTER_API_KEY": "sk-openrouter-victim-secret",
		},
	})
	if err == nil {
		t.Fatal("Resolve() error = nil, want unsafe project baseURL override rejection")
	}
	if strings.Contains(err.Error(), "sk-openrouter-victim-secret") {
		t.Fatalf("error leaked apiKeyEnv secret: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "project provider shared cannot override baseURL") {
		t.Fatalf("error = %q, want project baseURL override rejection", err.Error())
	}
}

func TestResolveAllowsProjectCatalogAPIKeyEnvOnCatalogEndpoint(t *testing.T) {
	path := writeConfig(t, `{
		"activeProvider": "openrouter",
		"providers": [{
			"name": "openrouter",
			"catalogID": "openrouter",
			"apiKeyEnv": "OPENROUTER_API_KEY"
		}]
	}`)

	resolved, err := Resolve(ResolveOptions{
		ProjectConfigPath: path,
		Env: map[string]string{
			"OPENROUTER_API_KEY": "sk-openrouter",
		},
	})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if resolved.Provider.APIKey != "sk-openrouter" {
		t.Fatalf("APIKey = %q, want catalog env-resolved key", resolved.Provider.APIKey)
	}
	if resolved.Provider.BaseURL != "https://openrouter.ai/api/v1" {
		t.Fatalf("BaseURL = %q, want catalog default", resolved.Provider.BaseURL)
	}
}

func TestResolveReplacesMCPServerOverlayCollections(t *testing.T) {
	userPath := writeConfig(t, `{
		"mcp": {
			"servers": {
				"docs": {
					"type": "stdio",
					"command": "docs-mcp",
					"args": ["--user"],
					"env": {"ZERO_DOCS_TOKEN": "user-token"}
				}
			}
		}
	}`)
	projectPath := writeConfig(t, `{
		"mcpServers": {
			"docs": {
				"args": ["--project"],
				"env": {"ZERO_DOCS_PROJECT": "1"}
			},
			"web": {
				"type": "http",
				"url": "https://example.com/mcp",
				"headers": {"Authorization": "Bearer test-token"}
			}
		}
	}`)

	resolved, err := Resolve(ResolveOptions{
		UserConfigPath:    userPath,
		ProjectConfigPath: projectPath,
		Env:               map[string]string{},
	})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}

	docs := resolved.MCP.Servers["docs"]
	if docs.Command != "docs-mcp" {
		t.Fatalf("docs.Command = %q, want inherited user command", docs.Command)
	}
	if got := strings.Join(docs.Args, " "); got != "--project" {
		t.Fatalf("docs.Args = %q, want project args override", got)
	}
	if _, ok := docs.Env["ZERO_DOCS_TOKEN"]; ok || docs.Env["ZERO_DOCS_PROJECT"] != "1" {
		t.Fatalf("docs.Env = %#v, want ZERO_DOCS_TOKEN absent and ZERO_DOCS_PROJECT=1", docs.Env)
	}
	web := resolved.MCP.Servers["web"]
	if web.Type != "http" || web.URL != "https://example.com/mcp" {
		t.Fatalf("web server = %#v, want root mcpServers alias loaded", web)
	}
}

func TestResolveMCPServerLayersCannotReenableUserDisabled(t *testing.T) {
	userPath := writeConfig(t, `{
		"mcp": {
			"servers": {
				"docs": {
					"type": "stdio",
					"command": "docs-mcp",
					"args": ["--user"],
					"env": {"ZERO_DOCS_TOKEN": "user-token"},
					"disabled": true
				}
			}
		}
	}`)
	projectPath := writeConfig(t, `{
		"mcpServers": {
			"docs": {
				"args": [],
				"env": {},
				"disabled": false
			}
		}
	}`)

	resolved, err := Resolve(ResolveOptions{
		UserConfigPath:    userPath,
		ProjectConfigPath: projectPath,
		Env:               map[string]string{},
	})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}

	docs := resolved.MCP.Servers["docs"]
	// A user-level disable is sticky: project config must not re-enable it.
	if !docs.Disabled {
		t.Fatal("docs.Disabled = false, want user-level disable to remain sticky")
	}
	// Non-disable fields from the project layer still merge normally.
	if len(docs.Args) != 0 {
		t.Fatalf("docs.Args = %#v, want project layer to clear inherited args", docs.Args)
	}
	if len(docs.Env) != 0 {
		t.Fatalf("docs.Env = %#v, want project layer to clear inherited env", docs.Env)
	}
}

func TestMergeMCPStickyDisableReenable(t *testing.T) {
	disabled := MCPServerConfig{Disabled: true, disabledSet: true}

	// A lower-trust (project) layer must not lift a sticky user disable.
	project := MCPServerConfig{Disabled: false, disabledSet: true}
	if got := mergeMCPServer(disabled, project, false); !got.Disabled {
		t.Fatal("project layer re-enabled a sticky user disable")
	}

	// The user scope (explicit mcp enable command / CLI override) may re-enable.
	override := MCPServerConfig{Disabled: false, disabledSet: true}
	if got := mergeMCPServer(disabled, override, true); got.Disabled {
		t.Fatal("user-scope override must be able to re-enable")
	}

	// A user-scope disable stacked on a project attempt still sticks.
	if got := mergeMCPServer(disabled, project, false); !got.Disabled {
		t.Fatal("sticky disable must survive a project re-enable attempt")
	}

	// Baseline: a project layer may disable a default-enabled server when no
	// higher-trust scope has explicitly set disabled (the trust boundary must
	// not block legitimate project disables). Regression guard for the
	// disabledSet-before-check ordering bug.
	defaultEnabled := MCPServerConfig{Disabled: false, disabledSet: false}
	projectDisable := MCPServerConfig{Disabled: true, disabledSet: true}
	if got := mergeMCPServer(defaultEnabled, projectDisable, false); !got.Disabled {
		t.Fatal("project layer should be able to disable a default-enabled server")
	}

	// And an empty project layer must not flip an unconfigured server to disabled.
	if got := mergeMCPServer(MCPServerConfig{}, MCPServerConfig{}, false); got.Disabled {
		t.Fatal("an empty project layer must not disable an unconfigured server")
	}
}

// TestResolveMCPCannotReenableUserDisabled exercises the trust boundary at the
// actual ResolveMCP entry point: a project config (project scope) must not lift
// a disable the user set in their higher-trust user config.
func TestResolveMCPCannotReenableUserDisabled(t *testing.T) {
	userPath := writeConfig(t, `{
		"mcp": {
			"servers": {
				"docs": {
					"type": "stdio",
					"command": "docs-mcp",
					"disabled": true
				}
			}
		}
	}`)
	// Project config tries to re-enable it.
	projectPath := writeConfig(t, `{
		"mcpServers": {
			"docs": {
				"disabled": false
			}
		}
	}`)

	resolved, err := ResolveMCP(ResolveOptions{
		UserConfigPath:    userPath,
		ProjectConfigPath: projectPath,
	})
	if err != nil {
		t.Fatalf("ResolveMCP() error = %v", err)
	}

	docs, ok := resolved.Servers["docs"]
	if !ok {
		t.Fatal("docs server missing from resolved config")
	}
	if !docs.Disabled {
		t.Fatalf("docs.Disabled = false, want user-level disable to remain sticky across project re-enable")
	}
}

// TestResolveMCPUserLiftsProjectDisabled asserts the reverse direction is open:
// a user config (higher-trust scope) re-enabling a server the project config
// disabled must be permitted, since user > project in the trust hierarchy.
func TestResolveMCPUserLiftsProjectDisabled(t *testing.T) {
	// Project config disables the server.
	projectPath := writeConfig(t, `{
		"mcpServers": {
			"docs": {
				"type": "stdio",
				"command": "docs-mcp",
				"disabled": true
			}
		}
	}`)
	// User config re-enables it.
	userPath := writeConfig(t, `{
		"mcp": {
			"servers": {
				"docs": {
					"disabled": false
				}
			}
		}
	}`)

	resolved, err := ResolveMCP(ResolveOptions{
		UserConfigPath:    userPath,
		ProjectConfigPath: projectPath,
	})
	if err != nil {
		t.Fatalf("ResolveMCP() error = %v", err)
	}

	docs, ok := resolved.Servers["docs"]
	if !ok {
		t.Fatal("docs server missing from resolved config")
	}
	if docs.Disabled {
		t.Fatalf("docs.Disabled = true, want user scope to lift the project-level disable")
	}
}

func TestResolveRejectsDuplicateMCPRootAliases(t *testing.T) {
	path := writeConfig(t, `{
		"mcpServers": {
			"docs": {"type": "stdio", "command": "docs-mcp"}
		},
		"mcp_servers": {
			"docs": {"type": "stdio", "command": "other-docs-mcp"}
		}
	}`)

	_, err := Resolve(ResolveOptions{ProjectConfigPath: path, Env: map[string]string{}})
	if err == nil {
		t.Fatal("Resolve() error = nil, want duplicate MCP alias error")
	}
	if !strings.Contains(err.Error(), `defined in both mcpServers and mcp_servers`) {
		t.Fatalf("error = %q, want duplicate MCP alias message", err.Error())
	}
}

func TestResolveMCPDoesNotRunProviderCommand(t *testing.T) {
	path := writeConfig(t, `{
		"mcp": {
			"servers": {
				"docs": {"type": "stdio", "command": "docs-mcp"}
			}
		}
	}`)

	resolved, err := ResolveMCP(ResolveOptions{
		ProjectConfigPath: path,
		ProviderCommand:   "provider-command-that-should-not-run",
		Env:               map[string]string{},
	})
	if err != nil {
		t.Fatalf("ResolveMCP() error = %v", err)
	}
	if resolved.Servers["docs"].Command != "docs-mcp" {
		t.Fatalf("MCP docs server = %#v", resolved.Servers["docs"])
	}
}

func TestResolveIgnoresWhitespaceOnlyActiveProviderLayers(t *testing.T) {
	userPath := writeConfig(t, `{
		"activeProvider": "alpha",
		"providers": [
			{"name": "alpha", "provider": "openai", "apiKey": "sk-alpha", "model": "gpt-alpha"},
			{"name": "beta", "provider": "openai", "apiKey": "sk-beta", "model": "gpt-beta"}
		]
	}`)
	projectPath := writeConfig(t, `{"activeProvider": "   "}`)

	resolved, err := Resolve(ResolveOptions{
		UserConfigPath:    userPath,
		ProjectConfigPath: projectPath,
		Env:               map[string]string{},
		Overrides:         Overrides{ActiveProvider: "   "},
	})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if resolved.ActiveProvider != "alpha" {
		t.Fatalf("ActiveProvider = %q, want alpha", resolved.ActiveProvider)
	}
}

func TestResolveUsesOpenAIEnvFallback(t *testing.T) {
	resolved, err := Resolve(ResolveOptions{
		Env: map[string]string{
			"OPENAI_API_KEY":  "sk-env",
			"OPENAI_BASE_URL": "https://env.example/v1",
			"OPENAI_MODEL":    "env-model",
		},
	})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}

	if resolved.ActiveProvider != "openai" {
		t.Fatalf("ActiveProvider = %q, want openai", resolved.ActiveProvider)
	}
	if resolved.Provider.ProviderKind != ProviderKindOpenAICompatible {
		t.Fatalf("ProviderKind = %q, want openai-compatible", resolved.Provider.ProviderKind)
	}
	if resolved.Provider.APIKey != "sk-env" || resolved.Provider.Model != "env-model" {
		t.Fatalf("Provider = %#v, want env credentials/model", resolved.Provider)
	}
	if resolved.Provider.BaseURL != "https://env.example/v1" {
		t.Fatalf("BaseURL = %q, want env URL", resolved.Provider.BaseURL)
	}
}

func TestResolveUsesAnthropicEnvBaseURLAsCompatible(t *testing.T) {
	// ANTHROPIC_BASE_URL pointing at a proxy/gateway must resolve to an
	// anthropic-compatible provider (mirroring OPENAI_BASE_URL), not the "requires
	// official baseURL" rejection — issue #479. The env is user-controlled, so a
	// custom URL there is a deliberate gateway choice, unlike a project config.json.
	resolved, err := Resolve(ResolveOptions{
		Env: map[string]string{
			"ANTHROPIC_API_KEY":  "sk-ant-env",
			"ANTHROPIC_BASE_URL": "https://gateway.example/anthropic",
			"ANTHROPIC_MODEL":    "claude-custom",
		},
	})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if resolved.ActiveProvider != "anthropic" {
		t.Fatalf("ActiveProvider = %q, want anthropic", resolved.ActiveProvider)
	}
	if resolved.Provider.ProviderKind != ProviderKindAnthropicCompat {
		t.Fatalf("ProviderKind = %q, want anthropic-compatible", resolved.Provider.ProviderKind)
	}
	if resolved.Provider.BaseURL != "https://gateway.example/anthropic" {
		t.Fatalf("BaseURL = %q, want gateway URL", resolved.Provider.BaseURL)
	}
	if resolved.Provider.APIKey != "sk-ant-env" || resolved.Provider.Model != "claude-custom" {
		t.Fatalf("Provider = %#v, want env credentials/model", resolved.Provider)
	}
}

func TestResolveUsesOpenAIAPIKeyOnlyWithDefaultModel(t *testing.T) {
	resolved, err := Resolve(ResolveOptions{
		Env: map[string]string{
			"OPENAI_API_KEY": "sk-env",
		},
	})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}

	if resolved.ActiveProvider != "openai" {
		t.Fatalf("ActiveProvider = %q, want openai", resolved.ActiveProvider)
	}
	if resolved.Provider.ProviderKind != ProviderKindOpenAI {
		t.Fatalf("ProviderKind = %q, want openai", resolved.Provider.ProviderKind)
	}
	if resolved.Provider.Model != modelregistry.DefaultModelID {
		t.Fatalf("Model = %q, want registry default model", resolved.Provider.Model)
	}
}

func TestResolvePreservesOpenAIConfigModelWhenAPIKeyEnvIsSet(t *testing.T) {
	path := writeConfig(t, `{
		"activeProvider": "openai",
		"providers": [{
			"name": "openai",
			"provider": "openai",
			"model": "gpt-4.1-mini"
		}]
	}`)

	resolved, err := Resolve(ResolveOptions{
		ProjectConfigPath: path,
		Env: map[string]string{
			"OPENAI_API_KEY": "sk-env",
		},
	})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}

	if resolved.Provider.Model != "gpt-4.1-mini" {
		t.Fatalf("Model = %q, want configured model preserved", resolved.Provider.Model)
	}
	if resolved.Provider.APIKey != "sk-env" {
		t.Fatalf("APIKey = %q, want env key attached", resolved.Provider.APIKey)
	}
}

func TestResolveDoesNotDefaultOpenAICustomBaseURLModel(t *testing.T) {
	_, err := Resolve(ResolveOptions{
		Env: map[string]string{
			"OPENAI_API_KEY":  "sk-env",
			"OPENAI_BASE_URL": "https://gateway.example/v1",
		},
	})
	if err == nil {
		t.Fatal("expected Resolve() error for custom OpenAI-compatible env without model")
	}
	message := err.Error()
	if !strings.Contains(message, "provider openai requires model") {
		t.Fatalf("expected missing model error, got %q", message)
	}
	if strings.Contains(message, "sk-env") {
		t.Fatalf("error leaked API key: %q", message)
	}
}

func TestResolveUsesAnthropicEnvFallback(t *testing.T) {
	resolved, err := Resolve(ResolveOptions{
		Env: map[string]string{
			"ZERO_PROVIDER":     "anthropic",
			"ANTHROPIC_API_KEY": "sk-ant-env",
			"ANTHROPIC_MODEL":   "claude-sonnet-4.5",
			"OPENAI_API_KEY":    "sk-openai-env",
			"OPENAI_MODEL":      "gpt-4.1",
			"GEMINI_API_KEY":    "sk-google-env",
			"GEMINI_MODEL":      "gemini-2.5-flash",
		},
	})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}

	if resolved.ActiveProvider != "anthropic" {
		t.Fatalf("ActiveProvider = %q, want anthropic", resolved.ActiveProvider)
	}
	if resolved.Provider.ProviderKind != ProviderKindAnthropic {
		t.Fatalf("ProviderKind = %q, want anthropic", resolved.Provider.ProviderKind)
	}
	if resolved.Provider.APIKey != "sk-ant-env" || resolved.Provider.Model != "claude-sonnet-4.5" {
		t.Fatalf("Provider = %#v, want Anthropic env credentials/model", resolved.Provider)
	}
	if resolved.Provider.BaseURL != AnthropicBaseURL {
		t.Fatalf("BaseURL = %q, want official Anthropic URL", resolved.Provider.BaseURL)
	}
	if len(resolved.Providers) != 3 {
		t.Fatalf("Providers = %#v, want openai, anthropic, and google env profiles", resolved.Providers)
	}
}

func TestResolveRejectsAnthropicOfficialProviderCustomBaseURL(t *testing.T) {
	path := writeConfig(t, `{
		"activeProvider": "anthropic",
		"providers": [{
			"name": "anthropic",
			"provider": "anthropic",
			"baseURL": "https://attacker.example/anthropic",
			"model": "claude-sonnet-4.5"
		}]
	}`)

	_, err := Resolve(ResolveOptions{
		ProjectConfigPath: path,
		Env: map[string]string{
			"ANTHROPIC_API_KEY": "sk-ant-secret",
		},
	})
	if err == nil {
		t.Fatal("Resolve() error = nil, want custom Anthropic baseURL rejection")
	}
	if !strings.Contains(err.Error(), "anthropic provider anthropic requires official baseURL") {
		t.Fatalf("error = %q, want official Anthropic baseURL message", err.Error())
	}
	if strings.Contains(err.Error(), "sk-ant-secret") {
		t.Fatalf("error leaked Anthropic API key: %q", err.Error())
	}
}

func TestResolveUsesAnthropicEnvFallbackWithCustomProfile(t *testing.T) {
	path := writeConfig(t, `{
		"activeProvider": "claude-prod",
		"providers": [{
			"name": "claude-prod",
			"provider_kind": "anthropic",
			"description": "production Claude"
		}]
	}`)

	resolved, err := Resolve(ResolveOptions{
		ProjectConfigPath: path,
		Env: map[string]string{
			"ZERO_PROVIDER":     "claude-prod",
			"ANTHROPIC_API_KEY": "sk-ant-env",
			"ANTHROPIC_MODEL":   "claude-sonnet-4.5",
		},
	})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}

	if resolved.ActiveProvider != "claude-prod" {
		t.Fatalf("ActiveProvider = %q, want claude-prod", resolved.ActiveProvider)
	}
	if resolved.Provider.ProviderKind != ProviderKindAnthropic {
		t.Fatalf("ProviderKind = %q, want anthropic", resolved.Provider.ProviderKind)
	}
	if resolved.Provider.APIKey != "sk-ant-env" || resolved.Provider.Model != "claude-sonnet-4.5" {
		t.Fatalf("Provider = %#v, want Anthropic env credentials/model on custom profile", resolved.Provider)
	}
	if resolved.Provider.BaseURL != AnthropicBaseURL {
		t.Fatalf("BaseURL = %q, want official Anthropic URL", resolved.Provider.BaseURL)
	}
	if resolved.Provider.Description != "production Claude" {
		t.Fatalf("Description = %q, want existing custom profile metadata preserved", resolved.Provider.Description)
	}
	if len(resolved.Providers) != 1 {
		t.Fatalf("Providers = %#v, want only custom Anthropic profile", resolved.Providers)
	}
}

func TestResolveUsesGoogleEnvFallbackAliases(t *testing.T) {
	resolved, err := Resolve(ResolveOptions{
		Env: map[string]string{
			"ZERO_PROVIDER":  "google",
			"GOOGLE_API_KEY": "sk-google-env",
			"GOOGLE_MODEL":   "gemini-2.5-pro",
		},
	})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}

	if resolved.ActiveProvider != "google" {
		t.Fatalf("ActiveProvider = %q, want google", resolved.ActiveProvider)
	}
	if resolved.Provider.ProviderKind != ProviderKindGoogle {
		t.Fatalf("ProviderKind = %q, want google", resolved.Provider.ProviderKind)
	}
	if resolved.Provider.APIKey != "sk-google-env" || resolved.Provider.Model != "gemini-2.5-pro" {
		t.Fatalf("Provider = %#v, want Google env credentials/model", resolved.Provider)
	}
	if resolved.Provider.BaseURL != GoogleBaseURL {
		t.Fatalf("BaseURL = %q, want official Google URL", resolved.Provider.BaseURL)
	}
}

func TestResolveRejectsGoogleOfficialProviderCustomBaseURL(t *testing.T) {
	path := writeConfig(t, `{
		"activeProvider": "google",
		"providers": [{
			"name": "google",
			"provider": "google",
			"baseURL": "https://attacker.example/google",
			"model": "gemini-2.5-pro"
		}]
	}`)

	_, err := Resolve(ResolveOptions{
		ProjectConfigPath: path,
		Env: map[string]string{
			"GOOGLE_API_KEY": "sk-google-secret",
		},
	})
	if err == nil {
		t.Fatal("Resolve() error = nil, want custom Google baseURL rejection")
	}
	if !strings.Contains(err.Error(), "google provider google requires official baseURL") {
		t.Fatalf("error = %q, want official Google baseURL message", err.Error())
	}
	if strings.Contains(err.Error(), "sk-google-secret") {
		t.Fatalf("error leaked Google API key: %q", err.Error())
	}
}

func TestResolveProviderCommandOverridesEnvProviderFields(t *testing.T) {
	command := writeCommand(t, commandScript{
		Stdout: `{"name":"cmd","provider":"openai","apiKey":"sk-command","model":"gpt-command"}`,
	})

	resolved, err := Resolve(ResolveOptions{
		ProviderCommand: command,
		Env: map[string]string{
			"OPENAI_API_KEY": "sk-env",
			"OPENAI_MODEL":   "gpt-env",
		},
	})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}

	if resolved.ActiveProvider != "cmd" {
		t.Fatalf("ActiveProvider = %q, want cmd", resolved.ActiveProvider)
	}
	if resolved.Provider.APIKey != "sk-command" {
		t.Fatalf("APIKey = %q, want provider command key", resolved.Provider.APIKey)
	}
	if resolved.Provider.Model != "gpt-command" {
		t.Fatalf("Model = %q, want provider command model", resolved.Provider.Model)
	}
}

func TestResolveAllowsNoConfiguredProviders(t *testing.T) {
	resolved, err := Resolve(ResolveOptions{Env: map[string]string{}})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}

	if resolved.ActiveProvider != "" {
		t.Fatalf("ActiveProvider = %q, want empty", resolved.ActiveProvider)
	}
	if len(resolved.Providers) != 0 {
		t.Fatalf("Providers = %#v, want empty", resolved.Providers)
	}
	if HasProviderProfile(resolved.Provider) {
		t.Fatalf("Provider = %#v, want zero value", resolved.Provider)
	}
	if resolved.MaxTurns != defaultMaxTurns {
		t.Fatalf("MaxTurns = %d, want default %d", resolved.MaxTurns, defaultMaxTurns)
	}
}

func TestResolveRejectsActiveProviderWithoutConfiguredProfiles(t *testing.T) {
	path := writeConfig(t, `{"activeProvider":"ghost","providers":[]}`)

	resolved, err := Resolve(ResolveOptions{ProjectConfigPath: path, Env: map[string]string{}})
	if err == nil {
		t.Fatal("Resolve() error = nil, want missing active provider error")
	}
	if !strings.Contains(err.Error(), `active provider "ghost" not found`) {
		t.Fatalf("error = %q, want active provider missing message", err.Error())
	}
	if !errors.Is(err, ErrNoActiveProvider) {
		t.Fatalf("error = %v, want errors.Is(err, ErrNoActiveProvider)", err)
	}
	if resolved.ActiveProvider != "" {
		t.Fatalf("ActiveProvider = %q, want empty", resolved.ActiveProvider)
	}
	if len(resolved.Providers) != 0 {
		t.Fatalf("Providers = %#v, want empty", resolved.Providers)
	}
	if HasProviderProfile(resolved.Provider) {
		t.Fatalf("Provider = %#v, want zero value", resolved.Provider)
	}
	if resolved.MaxTurns != 0 {
		t.Fatalf("MaxTurns = %d, want zero on failed resolve", resolved.MaxTurns)
	}
}

func TestResolveKeepsNormalizedProvidersWhenNoneMarkedActive(t *testing.T) {
	// Multiple providers configured (e.g. via `zero provider add`) but
	// activeProvider is blank/stale — a caller like the interactive TUI still
	// needs the normalized list to fall back to an already-usable provider
	// instead of forcing a full re-onboarding wizard.
	path := writeConfig(t, `{"providers":[
		{"name":"work","provider_kind":"openai","apiKey":"sk-test","model":"gpt-test"},
		{"name":"other","provider_kind":"openai","apiKey":"sk-other","model":"gpt-test"}
	]}`)

	resolved, err := Resolve(ResolveOptions{ProjectConfigPath: path, Env: map[string]string{}})
	if !errors.Is(err, ErrNoActiveProvider) {
		t.Fatalf("error = %v, want errors.Is(err, ErrNoActiveProvider)", err)
	}
	if len(resolved.Providers) != 2 {
		t.Fatalf("Providers = %#v, want the 2 normalized profiles preserved despite the error", resolved.Providers)
	}
}

func TestResolveTrimsProviderProfileAliasesBeforeFallback(t *testing.T) {
	path := writeConfig(t, `{
		"activeProvider": "custom",
		"providers": [{
			"name": "custom",
			"provider_kind": "openai-compatible",
			"baseURL": "   ",
			"base_url": "https://custom.example/v1",
			"apiKey": "   ",
			"api_key": "sk-custom",
			"model": "   ",
			"model_id": "custom-model"
		}]
	}`)

	resolved, err := Resolve(ResolveOptions{ProjectConfigPath: path, Env: map[string]string{}})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if resolved.Provider.BaseURL != "https://custom.example/v1" {
		t.Fatalf("BaseURL = %q, want alias fallback", resolved.Provider.BaseURL)
	}
	if resolved.Provider.APIKey != "sk-custom" {
		t.Fatalf("APIKey = %q, want alias fallback", resolved.Provider.APIKey)
	}
	if resolved.Provider.Model != "custom-model" {
		t.Fatalf("Model = %q, want alias fallback", resolved.Provider.Model)
	}
}

func TestResolveNormalizesOfficialOpenAIBaseURL(t *testing.T) {
	path := writeConfig(t, `{
		"activeProvider": "openai",
		"providers": [{
			"name": "openai",
			"provider": "openai",
			"baseURL": "openai",
			"apiKey": "sk-official",
			"model": "gpt-4.1"
		}]
	}`)

	resolved, err := Resolve(ResolveOptions{ProjectConfigPath: path, Env: map[string]string{}})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}

	if resolved.Provider.ProviderKind != ProviderKindOpenAI {
		t.Fatalf("ProviderKind = %q, want openai", resolved.Provider.ProviderKind)
	}
	if resolved.Provider.BaseURL != OpenAIBaseURL {
		t.Fatalf("BaseURL = %q, want %q", resolved.Provider.BaseURL, OpenAIBaseURL)
	}
}

func TestResolveAcceptsOfficialAnthropicAndGoogleProfiles(t *testing.T) {
	path := writeConfig(t, `{
		"activeProvider": "claude",
		"providers": [
			{"name": "claude", "provider": "anthropic", "apiKey": "sk-ant", "model": "claude-sonnet-4.5"},
			{"name": "gemini", "provider": "google", "apiKey": "sk-google", "model": "gemini-2.5-flash"}
		]
	}`)

	resolved, err := Resolve(ResolveOptions{ProjectConfigPath: path, Env: map[string]string{}})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}

	if resolved.Provider.ProviderKind != ProviderKindAnthropic {
		t.Fatalf("ProviderKind = %q, want anthropic", resolved.Provider.ProviderKind)
	}
	if resolved.Provider.BaseURL != AnthropicBaseURL {
		t.Fatalf("BaseURL = %q, want default Anthropic URL", resolved.Provider.BaseURL)
	}
	if resolved.Providers[1].ProviderKind != ProviderKindGoogle {
		t.Fatalf("Google ProviderKind = %q, want google", resolved.Providers[1].ProviderKind)
	}
	if resolved.Providers[1].BaseURL != GoogleBaseURL {
		t.Fatalf("Google BaseURL = %q, want default Google URL", resolved.Providers[1].BaseURL)
	}
}

func TestResolveRejectsAnthropicCompatibleOfficialHostWithPath(t *testing.T) {
	path := writeConfig(t, `{
		"activeProvider": "claude-compatible",
		"providers": [{
			"name": "claude-compatible",
			"provider_kind": "anthropic-compatible",
			"baseURL": "https://api.anthropic.com/v1",
			"apiKey": "sk-ant",
			"model": "custom-claude"
		}]
	}`)

	_, err := Resolve(ResolveOptions{ProjectConfigPath: path, Env: map[string]string{}})
	if err == nil {
		t.Fatal("Resolve() error = nil, want official Anthropic host rejection")
	}
	if !strings.Contains(err.Error(), "requires custom baseURL") {
		t.Fatalf("error = %q, want custom baseURL message", err.Error())
	}
}

func TestResolveAppliesProviderCatalogDefaults(t *testing.T) {
	path := writeConfig(t, `{
		"activeProvider": "mini",
		"providers": [{
			"name": "mini",
			"catalog_id": "minimax"
		}]
	}`)

	resolved, err := Resolve(ResolveOptions{
		ProjectConfigPath: path,
		Env: map[string]string{
			"MINIMAX_API_KEY": "sk-mini",
		},
	})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}

	if resolved.Provider.CatalogID != "minimax" {
		t.Fatalf("CatalogID = %q, want minimax", resolved.Provider.CatalogID)
	}
	if resolved.Provider.ProviderKind != ProviderKindAnthropicCompat {
		t.Fatalf("ProviderKind = %q, want %q", resolved.Provider.ProviderKind, ProviderKindAnthropicCompat)
	}
	if resolved.Provider.BaseURL != "https://api.minimax.io/anthropic" {
		t.Fatalf("BaseURL = %q, want MiniMax default", resolved.Provider.BaseURL)
	}
	if resolved.Provider.Model != "MiniMax-M3" {
		t.Fatalf("Model = %q, want MiniMax-M3", resolved.Provider.Model)
	}
	if resolved.Provider.APIKeyEnv != "MINIMAX_API_KEY" {
		t.Fatalf("APIKeyEnv = %q, want MINIMAX_API_KEY", resolved.Provider.APIKeyEnv)
	}
	if resolved.Provider.APIKey != "sk-mini" {
		t.Fatalf("APIKey = %q, want env-resolved secret", resolved.Provider.APIKey)
	}
}

func TestResolveAppliesMiniMaxCNCatalogDefaults(t *testing.T) {
	path := writeConfig(t, `{
		"activeProvider": "mini-cn",
		"providers": [{
			"name": "mini-cn",
			"catalog_id": "minimaxi-cn"
		}]
	}`)

	resolved, err := Resolve(ResolveOptions{
		ProjectConfigPath: path,
		Env: map[string]string{
			"MINIMAXI_API_KEY": "sk-cn-mini",
		},
	})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}

	if resolved.Provider.CatalogID != "minimaxi-cn" {
		t.Fatalf("CatalogID = %q, want minimaxi-cn", resolved.Provider.CatalogID)
	}
	if resolved.Provider.ProviderKind != ProviderKindAnthropicCompat {
		t.Fatalf("ProviderKind = %q, want %q", resolved.Provider.ProviderKind, ProviderKindAnthropicCompat)
	}
	if resolved.Provider.BaseURL != "https://api.minimaxi.com/anthropic" {
		t.Fatalf("BaseURL = %q, want MiniMax CN default", resolved.Provider.BaseURL)
	}
	if resolved.Provider.Model != "MiniMax-M3" {
		t.Fatalf("Model = %q, want MiniMax-M3", resolved.Provider.Model)
	}
	if resolved.Provider.APIKeyEnv != "MINIMAXI_API_KEY" {
		t.Fatalf("APIKeyEnv = %q, want MINIMAXI_API_KEY", resolved.Provider.APIKeyEnv)
	}
	if resolved.Provider.APIKey != "sk-cn-mini" {
		t.Fatalf("APIKey = %q, want env-resolved secret", resolved.Provider.APIKey)
	}
}

func TestResolveAppliesZaiCNCatalogDefaults(t *testing.T) {
	// The "zai-cn" catalog entry preserves the legacy open.bigmodel.cn endpoint
	// for users who still target the China hosting; "zai" itself now points at
	// api.z.ai (international). The two env-vars are intentionally distinct
	// (ZHIPU_API_KEY for China, ZAI_API_KEY for international) so an account on
	// one side cannot be silently used against the other.
	path := writeConfig(t, `{
		"activeProvider": "zai-cn",
		"providers": [{
			"name": "zai-cn",
			"catalog_id": "zai-cn"
		}]
	}`)

	resolved, err := Resolve(ResolveOptions{
		ProjectConfigPath: path,
		Env: map[string]string{
			"ZHIPU_API_KEY": "sk-zhipu-cn",
		},
	})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}

	if resolved.Provider.CatalogID != "zai-cn" {
		t.Fatalf("CatalogID = %q, want zai-cn", resolved.Provider.CatalogID)
	}
	if resolved.Provider.ProviderKind != ProviderKindOpenAICompatible {
		t.Fatalf("ProviderKind = %q, want %q", resolved.Provider.ProviderKind, ProviderKindOpenAICompatible)
	}
	if resolved.Provider.BaseURL != "https://open.bigmodel.cn/api/paas/v4" {
		t.Fatalf("BaseURL = %q, want Z.ai China default", resolved.Provider.BaseURL)
	}
	if resolved.Provider.Model != "glm-4.5" {
		t.Fatalf("Model = %q, want glm-4.5 (China default)", resolved.Provider.Model)
	}
	if resolved.Provider.APIKeyEnv != "ZHIPU_API_KEY" {
		t.Fatalf("APIKeyEnv = %q, want ZHIPU_API_KEY", resolved.Provider.APIKeyEnv)
	}
	if resolved.Provider.APIKey != "sk-zhipu-cn" {
		t.Fatalf("APIKey = %q, want env-resolved secret", resolved.Provider.APIKey)
	}
}

func TestResolveAppliesZaiInternationalCatalogDefaults(t *testing.T) {
	// "zai" now resolves to the international endpoint; the catalog default
	// model stays glm-4.5 (live discovery upgrades to glm-5.2 at runtime).
	path := writeConfig(t, `{
		"activeProvider": "zai-intl",
		"providers": [{
			"name": "zai-intl",
			"catalog_id": "zai"
		}]
	}`)

	resolved, err := Resolve(ResolveOptions{
		ProjectConfigPath: path,
		Env: map[string]string{
			"ZAI_API_KEY": "sk-zai-intl",
		},
	})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}

	if resolved.Provider.CatalogID != "zai" {
		t.Fatalf("CatalogID = %q, want zai", resolved.Provider.CatalogID)
	}
	if resolved.Provider.ProviderKind != ProviderKindOpenAICompatible {
		t.Fatalf("ProviderKind = %q, want %q", resolved.Provider.ProviderKind, ProviderKindOpenAICompatible)
	}
	if resolved.Provider.BaseURL != "https://api.z.ai/api/paas/v4" {
		t.Fatalf("BaseURL = %q, want Z.ai international default", resolved.Provider.BaseURL)
	}
	if resolved.Provider.Model != "glm-4.5" {
		t.Fatalf("Model = %q, want glm-4.5 (preserved default; live discovery upgrades to glm-5.2 at runtime)", resolved.Provider.Model)
	}
	if resolved.Provider.APIKeyEnv != "ZAI_API_KEY" {
		t.Fatalf("APIKeyEnv = %q, want ZAI_API_KEY", resolved.Provider.APIKeyEnv)
	}
	if resolved.Provider.APIKey != "sk-zai-intl" {
		t.Fatalf("APIKey = %q, want env-resolved secret", resolved.Provider.APIKey)
	}
}

func TestResolveProviderCatalogAliasAndLocalNoAuth(t *testing.T) {
	path := writeConfig(t, `{
		"activeProvider": "local",
		"providers": [{
			"name": "local",
			"catalogID": "lm-studio"
		}]
	}`)

	resolved, err := Resolve(ResolveOptions{ProjectConfigPath: path, Env: map[string]string{}})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}

	if resolved.Provider.CatalogID != "lmstudio" {
		t.Fatalf("CatalogID = %q, want lmstudio", resolved.Provider.CatalogID)
	}
	if resolved.Provider.ProviderKind != ProviderKindOpenAICompatible {
		t.Fatalf("ProviderKind = %q, want openai-compatible", resolved.Provider.ProviderKind)
	}
	if resolved.Provider.APIKey != "" {
		t.Fatalf("APIKey = %q, want empty for local provider", resolved.Provider.APIKey)
	}
}

func TestResolveProviderProfileExtendedJSONAliases(t *testing.T) {
	path := writeConfig(t, `{
		"activeProvider": "custom",
		"providers": [{
			"name": "custom",
			"providerKind": "openai-compatible",
			"catalogID": "custom-openai-compatible",
			"base_url": "https://custom.example/v1",
			"api_key_env": "CUSTOM_KEY",
			"api_format": "responses",
			"auth_header": "X-API-Key",
			"auth_scheme": "raw",
			"auth_header_value": "header-secret",
			"custom_headers": {"X-Zero": "1"},
			"model_id": "custom-model",
			"parse_think_tags": true
		}]
	}`)

	resolved, err := Resolve(ResolveOptions{
		UserConfigPath: path,
		Env:            map[string]string{"CUSTOM_KEY": "sk-custom-env"},
	})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}

	profile := resolved.Provider
	if profile.CatalogID != "custom-openai-compatible" || profile.APIKeyEnv != "CUSTOM_KEY" {
		t.Fatalf("profile aliases not loaded: %#v", profile)
	}
	if profile.APIKey != "sk-custom-env" {
		t.Fatalf("APIKey = %q, want env-resolved key", profile.APIKey)
	}
	if profile.APIFormat != "responses" || profile.AuthHeader != "X-API-Key" || profile.AuthScheme != "raw" || profile.AuthHeaderValue != "header-secret" {
		t.Fatalf("extended provider fields not loaded: %#v", profile)
	}
	if profile.CustomHeaders["X-Zero"] != "1" {
		t.Fatalf("CustomHeaders = %#v, want X-Zero header", profile.CustomHeaders)
	}
	if profile.ParseThinkTags == nil || !*profile.ParseThinkTags {
		t.Fatalf("ParseThinkTags = %#v, want true", profile.ParseThinkTags)
	}
}

func TestApplyCatalogDescriptorFoldsCustomHeadersCaseInsensitively(t *testing.T) {
	t.Setenv("AIMLAPI_PARTNER_ID", "part_env")
	descriptor, err := providercatalog.Require("aimlapi")
	if err != nil {
		t.Fatal(err)
	}
	profile := ProviderProfile{
		BaseURL: descriptor.DefaultBaseURL,
		CustomHeaders: map[string]string{
			"x-aimlapi-partner-id": "part_override",
		},
	}

	applyCatalogDescriptor(&profile, descriptor, true)

	if got := profile.CustomHeaders["X-AIMLAPI-Partner-ID"]; got != "part_override" {
		t.Fatalf("canonical header value = %q, want override", got)
	}
	if _, duplicate := profile.CustomHeaders["x-aimlapi-partner-id"]; duplicate {
		t.Fatalf("case-colliding header survived merge: %#v", profile.CustomHeaders)
	}
}

func TestApplyCatalogDescriptorUsesAimlapiPartnerEnvironmentOverride(t *testing.T) {
	t.Setenv("AIMLAPI_PARTNER_ID", "part_env")
	descriptor, err := providercatalog.Require("aimlapi")
	if err != nil {
		t.Fatal(err)
	}
	profile := ProviderProfile{BaseURL: descriptor.DefaultBaseURL}

	applyCatalogDescriptor(&profile, descriptor, true)

	if got := profile.CustomHeaders["X-AIMLAPI-Partner-ID"]; got != "part_env" {
		t.Fatalf("partner header = %q, want part_env", got)
	}
}

func TestApplyCatalogDescriptorStripsAimlapiAttributionFromRetargetedProfile(t *testing.T) {
	descriptor, err := providercatalog.Require("aimlapi")
	if err != nil {
		t.Fatal(err)
	}
	profile := ProviderProfile{
		BaseURL: "https://proxy.example.test/v1",
		CustomHeaders: map[string]string{
			"x-aimlapi-partner-id":          "persisted-partner",
			"X-AIMLAPI-Integration-Repo":    "Gitlawb/zero",
			"X-AIMLAPI-Integration-Version": "zero",
			"X-Environment":                 "staging",
		},
	}

	applyCatalogDescriptor(&profile, descriptor, true)

	for key := range profile.CustomHeaders {
		if strings.HasPrefix(strings.ToLower(key), "x-aimlapi-") {
			t.Fatalf("catalog attribution survived retargeting: %#v", profile.CustomHeaders)
		}
	}
	if profile.CustomHeaders["X-Environment"] != "staging" {
		t.Fatalf("user header was removed: %#v", profile.CustomHeaders)
	}
}

func TestResolveProviderProfileParseThinkTagsFalseAlias(t *testing.T) {
	path := writeConfig(t, `{
		"activeProvider": "custom",
		"providers": [{
			"name": "custom",
			"provider_kind": "openai-compatible",
			"base_url": "https://custom.example/v1",
			"model_id": "custom-model",
			"parse_think_tags": false
		}]
	}`)

	resolved, err := Resolve(ResolveOptions{UserConfigPath: path, Env: map[string]string{}})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if resolved.Provider.ParseThinkTags == nil || *resolved.Provider.ParseThinkTags {
		t.Fatalf("ParseThinkTags = %#v, want explicit false", resolved.Provider.ParseThinkTags)
	}
}

func TestResolveRejectsUnknownProviderCatalogID(t *testing.T) {
	path := writeConfig(t, `{
		"activeProvider": "bad",
		"providers": [{
			"name": "bad",
			"catalog_id": "missing",
			"apiKey": "sk-secret-value"
		}]
	}`)

	_, err := Resolve(ResolveOptions{ProjectConfigPath: path, Env: map[string]string{}})
	if err == nil {
		t.Fatal("Resolve() error = nil, want unknown catalog ID error")
	}
	if !strings.Contains(err.Error(), `unknown provider "missing"`) {
		t.Fatalf("error = %q, want unknown catalog ID", err.Error())
	}
	if strings.Contains(err.Error(), "sk-secret-value") {
		t.Fatalf("error leaked secret: %q", err.Error())
	}
}

func TestResolveRejectsCatalogOnlyProviderRuntime(t *testing.T) {
	path := writeConfig(t, `{
		"activeProvider": "bedrock",
		"providers": [{
			"name": "bedrock",
			"catalog_id": "bedrock"
		}]
	}`)

	_, err := Resolve(ResolveOptions{ProjectConfigPath: path, Env: map[string]string{}})
	if err == nil {
		t.Fatal("Resolve() error = nil, want catalog-only runtime rejection")
	}
	if !strings.Contains(err.Error(), "native adapter not implemented yet") {
		t.Fatalf("error = %q, want native adapter unsupported message", err.Error())
	}
}

func TestResolveRejectsOpenAICompatibleWithoutBaseURL(t *testing.T) {
	path := writeConfig(t, `{
		"activeProvider": "custom",
		"providers": [{
			"name": "custom",
			"provider_kind": "openai-compatible",
			"apiKey": "sk-custom",
			"model": "custom-model"
		}]
	}`)

	_, err := Resolve(ResolveOptions{ProjectConfigPath: path, Env: map[string]string{}})
	if err == nil {
		t.Fatal("Resolve() error = nil, want validation error")
	}
	if !strings.Contains(err.Error(), "openai-compatible provider custom requires baseURL") {
		t.Fatalf("error = %q, want missing baseURL message", err.Error())
	}
}

func TestResolveRejectsUnknownProviderKind(t *testing.T) {
	path := writeConfig(t, `{
		"activeProvider": "bad",
		"providers": [{
			"name": "bad",
			"provider": "bedrock",
			"apiKey": "sk-bad",
			"model": "model"
		}]
	}`)

	_, err := Resolve(ResolveOptions{ProjectConfigPath: path, Env: map[string]string{}})
	if err == nil {
		t.Fatal("Resolve() error = nil, want validation error")
	}
	if !strings.Contains(err.Error(), `unknown provider kind "bedrock"`) {
		t.Fatalf("error = %q, want unknown provider kind", err.Error())
	}
}

func TestResolveRedactsSecretsFromErrors(t *testing.T) {
	path := writeConfig(t, `{
		"activeProvider": "custom",
		"providers": [{
			"name": "custom",
			"provider_kind": "openai-compatible",
			"apiKey": "sk-secret-value",
			"model": "custom-model"
		}]
	}`)

	_, err := Resolve(ResolveOptions{ProjectConfigPath: path, Env: map[string]string{}})
	if err == nil {
		t.Fatal("Resolve() error = nil, want validation error")
	}
	if strings.Contains(err.Error(), "sk-secret-value") {
		t.Fatalf("error leaked secret: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "[REDACTED]") {
		t.Fatalf("error = %q, want redaction marker", err.Error())
	}
}

func TestResolveSandboxBlockUnixSocketsFromFile(t *testing.T) {
	path := writeConfig(t, `{
		"sandbox": {"blockUnixSockets": true},
		"providers": [{
			"name": "openai",
			"provider": "openai",
			"api_key": "sk-test",
			"model": "gpt-test"
		}]
	}`)

	resolved, err := Resolve(ResolveOptions{ProjectConfigPath: path, Env: map[string]string{}})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if !resolved.Sandbox.BlockUnixSockets {
		t.Fatal("resolved.Sandbox.BlockUnixSockets = false, want true")
	}
}

func TestResolveSandboxNetworkProjectConfigCannotWeaken(t *testing.T) {
	// 1. Project config tries to set network to "allow" (should be ignored).
	userPath := writeConfig(t, `{}`)
	projectPath := writeConfig(t, `{
		"sandbox": {"network": "allow"}
	}`)

	resolved, err := Resolve(ResolveOptions{
		UserConfigPath:    userPath,
		ProjectConfigPath: projectPath,
		Env:               map[string]string{},
	})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if resolved.Sandbox.Network == "allow" {
		t.Fatal("resolved.Sandbox.Network = allow, want empty/deny to prevent project config from weakening sandbox")
	}

	// 2. User config sets network to "allow", project config sets it to "deny" (tightening is allowed).
	userPath2 := writeConfig(t, `{
		"sandbox": {"network": "allow"}
	}`)
	projectPath2 := writeConfig(t, `{
		"sandbox": {"network": "deny"}
	}`)
	resolved2, err := Resolve(ResolveOptions{
		UserConfigPath:    userPath2,
		ProjectConfigPath: projectPath2,
		Env:               map[string]string{},
	})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if resolved2.Sandbox.Network != "deny" {
		t.Fatalf("resolved.Sandbox.Network = %q, want deny (project config allowed to tighten)", resolved2.Sandbox.Network)
	}
}

func TestResolveSandboxEnabledUserConfigDisables(t *testing.T) {
	userPath := writeConfig(t, `{"sandbox": {"enabled": false}}`)
	resolved, err := Resolve(ResolveOptions{UserConfigPath: userPath, Env: map[string]string{}})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if resolved.Sandbox.Enabled == nil || *resolved.Sandbox.Enabled {
		t.Fatalf("user config enabled:false must resolve to a false pointer, got %v", resolved.Sandbox.Enabled)
	}
}

func TestResolveSandboxDisableIgnoredFromProjectConfig(t *testing.T) {
	// Security: a cloned repo's project config must NOT be able to disable the
	// sandbox that constrains it. Only global config / CLI can turn it off.
	userPath := writeConfig(t, `{}`)
	projectPath := writeConfig(t, `{"sandbox": {"enabled": false}}`)
	resolved, err := Resolve(ResolveOptions{UserConfigPath: userPath, ProjectConfigPath: projectPath, Env: map[string]string{}})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if resolved.Sandbox.Enabled != nil {
		t.Fatalf("project config enabled:false must be ignored, got %v — a repo could disable the sandbox", *resolved.Sandbox.Enabled)
	}
}

func TestResolveSandboxEnabledIgnoredFromProviderCommand(t *testing.T) {
	// Security: a provider command is an arbitrary executable named in config, and
	// its stdout is parsed into a full FileConfig — so it must NOT be able to reach
	// Sandbox.Enabled, in either direction. Same rule as project config: only global
	// config / CLI may turn the sandbox off.
	for _, tc := range []struct {
		name  string
		value string
	}{
		{name: "cannot disable", value: "false"},
		{name: "cannot enable", value: "true"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			command := writeCommand(t, commandScript{
				Stdout: `{"activeProvider":"cmd","providers":[{"name":"cmd","provider":"openai","apiKey":"sk-command","model":"gpt-command"}],"sandbox":{"enabled":` + tc.value + `}}`,
			})
			resolved, err := Resolve(ResolveOptions{ProviderCommand: command, Env: map[string]string{}})
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			// The provider itself must still be applied — only the sandbox toggle is dropped.
			if resolved.ActiveProvider != "cmd" {
				t.Fatalf("ActiveProvider = %q, want cmd (provider command must still apply)", resolved.ActiveProvider)
			}
			if resolved.Sandbox.Enabled != nil {
				t.Fatalf("provider command set sandbox.enabled=%s, got %v — a provider command could toggle the sandbox", tc.value, *resolved.Sandbox.Enabled)
			}
		})
	}
}

func TestResolveSandboxEnabledCLIOverride(t *testing.T) {
	// A CLI/programmatic override (applied after every config source) must be able
	// to disable the sandbox, even with no config file.
	disabled := false
	resolved, err := Resolve(ResolveOptions{Env: map[string]string{}, Overrides: Overrides{Sandbox: SandboxConfig{Enabled: &disabled}}})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if resolved.Sandbox.Enabled == nil || *resolved.Sandbox.Enabled {
		t.Fatalf("CLI override enabled:false must disable the sandbox, got %v", resolved.Sandbox.Enabled)
	}
	// An explicit true override must win over a lower-precedence global-config false.
	userPath := writeConfig(t, `{"sandbox": {"enabled": false}}`)
	enabled := true
	resolved2, err := Resolve(ResolveOptions{UserConfigPath: userPath, Env: map[string]string{}, Overrides: Overrides{Sandbox: SandboxConfig{Enabled: &enabled}}})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if resolved2.Sandbox.Enabled == nil || !*resolved2.Sandbox.Enabled {
		t.Fatalf("CLI override enabled:true must win over config false, got %v", resolved2.Sandbox.Enabled)
	}
}

func TestResolveNotifyValid(t *testing.T) {
	path := writeConfig(t, `{"notify":{"mode":"both","focusMode":"always"}}`)
	resolved, err := Resolve(ResolveOptions{UserConfigPath: path, Env: map[string]string{}})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if resolved.Notify.Mode != "both" || resolved.Notify.FocusMode != "always" {
		t.Fatalf("got %+v", resolved.Notify)
	}
}

func TestResolveNotifyInvalidMode(t *testing.T) {
	path := writeConfig(t, `{"notify":{"mode":"buzz"}}`)
	if _, err := Resolve(ResolveOptions{UserConfigPath: path, Env: map[string]string{}}); err == nil {
		t.Fatal("expected error for invalid notify.mode")
	}
}

func TestResolveNotifyInvalidFocusMode(t *testing.T) {
	path := writeConfig(t, `{"notify":{"focusMode":"sideways"}}`)
	if _, err := Resolve(ResolveOptions{UserConfigPath: path, Env: map[string]string{}}); err == nil {
		t.Fatal("expected error for invalid notify.focusMode")
	}
}

func TestResolveNotifyDefaultEmpty(t *testing.T) {
	path := writeConfig(t, `{}`)
	resolved, err := Resolve(ResolveOptions{UserConfigPath: path, Env: map[string]string{}})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if resolved.Notify.Mode != "" || resolved.Notify.FocusMode != "" {
		t.Fatalf("unset notify should be empty, got %+v", resolved.Notify)
	}
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "zero.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func providerByName(t *testing.T, providers []ProviderProfile, name string) ProviderProfile {
	t.Helper()

	for _, provider := range providers {
		if provider.Name == name {
			return provider
		}
	}
	t.Fatalf("provider %q not found in %#v", name, providers)
	return ProviderProfile{}
}

func TestResolveDefaultsDeferThresholdWhenUnset(t *testing.T) {
	path := writeConfig(t, `{
		"activeProvider": "p",
		"providers": [{"name": "p", "provider": "openai", "apiKey": "sk", "model": "m"}]
	}`)

	resolved, err := Resolve(ResolveOptions{ProjectConfigPath: path, Env: map[string]string{}})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if resolved.Tools.DeferThreshold != defaultDeferThreshold {
		t.Fatalf("Tools.DeferThreshold = %d, want %d", resolved.Tools.DeferThreshold, defaultDeferThreshold)
	}
}

// The default must stay low so a typical single MCP server's toolset defers
// instead of shipping every full schema each turn (the token-economy fix). This
// guards against a regression silently bumping it back up.
func TestDefaultDeferThresholdStaysLowForTokenEconomy(t *testing.T) {
	if defaultDeferThreshold <= 0 || defaultDeferThreshold > 4 {
		t.Fatalf("defaultDeferThreshold = %d, want a small positive value (<=4) so small MCP sets defer", defaultDeferThreshold)
	}
}

func TestResolveDeferThresholdFromFile(t *testing.T) {
	path := writeConfig(t, `{
		"activeProvider": "p",
		"providers": [{"name": "p", "provider": "openai", "apiKey": "sk", "model": "m"}],
		"tools": {"deferThreshold": 4}
	}`)

	resolved, err := Resolve(ResolveOptions{ProjectConfigPath: path, Env: map[string]string{}})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if resolved.Tools.DeferThreshold != 4 {
		t.Fatalf("Tools.DeferThreshold = %d, want 4", resolved.Tools.DeferThreshold)
	}
}

func TestResolveDeferThresholdZeroDisablesViaFile(t *testing.T) {
	// 0 is a valid, meaningful value (disabled). It must survive Resolve and NOT
	// be re-defaulted to 10, so a user can explicitly disable deferral.
	path := writeConfig(t, `{
		"activeProvider": "p",
		"providers": [{"name": "p", "provider": "openai", "apiKey": "sk", "model": "m"}],
		"tools": {"deferThreshold": 0}
	}`)

	resolved, err := Resolve(ResolveOptions{ProjectConfigPath: path, Env: map[string]string{}})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if resolved.Tools.DeferThreshold != 0 {
		t.Fatalf("Tools.DeferThreshold = %d, want 0 (explicit disable preserved)", resolved.Tools.DeferThreshold)
	}
}

func TestResolveDeferThresholdOverrideWins(t *testing.T) {
	path := writeConfig(t, `{
		"activeProvider": "p",
		"providers": [{"name": "p", "provider": "openai", "apiKey": "sk", "model": "m"}],
		"tools": {"deferThreshold": 4}
	}`)

	resolved, err := Resolve(ResolveOptions{
		ProjectConfigPath: path,
		Env:               map[string]string{},
		Overrides:         Overrides{Tools: ToolsConfig{DeferThreshold: 20}},
	})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if resolved.Tools.DeferThreshold != 20 {
		t.Fatalf("Tools.DeferThreshold = %d, want 20 (override wins)", resolved.Tools.DeferThreshold)
	}
}

func TestResolveToolsOverrideDisablesDeferralOverNonZeroBase(t *testing.T) {
	// A programmatic Override built via ToolsOverride(0) carries the presence flag,
	// so it must override an explicit non-zero base threshold down to 0 (disabled).
	// A bare ToolsConfig{DeferThreshold: 0} could not do this — it is indistinguishable
	// from "unset" — which is exactly the trap ToolsOverride exists to avoid.
	path := writeConfig(t, `{
		"activeProvider": "p",
		"providers": [{"name": "p", "provider": "openai", "apiKey": "sk", "model": "m"}],
		"tools": {"deferThreshold": 4}
	}`)

	resolved, err := Resolve(ResolveOptions{
		ProjectConfigPath: path,
		Env:               map[string]string{},
		Overrides:         Overrides{Tools: ToolsOverride(0)},
	})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if resolved.Tools.DeferThreshold != 0 {
		t.Fatalf("Tools.DeferThreshold = %d, want 0 (ToolsOverride(0) disables deferral)", resolved.Tools.DeferThreshold)
	}
}

func TestResolveToolsOverrideSetsNonZeroOverNonZeroBase(t *testing.T) {
	// ToolsOverride(7) over an explicit non-zero base must win with the new value.
	path := writeConfig(t, `{
		"activeProvider": "p",
		"providers": [{"name": "p", "provider": "openai", "apiKey": "sk", "model": "m"}],
		"tools": {"deferThreshold": 4}
	}`)

	resolved, err := Resolve(ResolveOptions{
		ProjectConfigPath: path,
		Env:               map[string]string{},
		Overrides:         Overrides{Tools: ToolsOverride(7)},
	})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if resolved.Tools.DeferThreshold != 7 {
		t.Fatalf("Tools.DeferThreshold = %d, want 7 (ToolsOverride(7) wins)", resolved.Tools.DeferThreshold)
	}
}

func TestApplyOverridesToolsOverrideZeroOverridesNonZero(t *testing.T) {
	// Unit-level check on applyOverrides directly: ToolsOverride(0) must flip an
	// explicit non-zero base to 0 (deferThresholdSet honored), while a bare
	// non-zero ToolsConfig still overrides via the != 0 branch.
	cfg := FileConfig{Tools: ToolsOverride(4)}
	applyOverrides(&cfg, Overrides{Tools: ToolsOverride(0)})
	if cfg.Tools.DeferThreshold != 0 {
		t.Fatalf("applyOverrides ToolsOverride(0): DeferThreshold = %d, want 0", cfg.Tools.DeferThreshold)
	}
	if !cfg.Tools.deferThresholdSet {
		t.Fatal("applyOverrides ToolsOverride(0): deferThresholdSet = false, want true")
	}

	cfg = FileConfig{Tools: ToolsOverride(4)}
	applyOverrides(&cfg, Overrides{Tools: ToolsConfig{DeferThreshold: 5}})
	if cfg.Tools.DeferThreshold != 5 {
		t.Fatalf("applyOverrides bare non-zero: DeferThreshold = %d, want 5", cfg.Tools.DeferThreshold)
	}
}

func TestResolveRejectsNegativeDeferThreshold(t *testing.T) {
	path := writeConfig(t, `{
		"activeProvider": "p",
		"providers": [{"name": "p", "provider": "openai", "apiKey": "sk", "model": "m"}],
		"tools": {"deferThreshold": -1}
	}`)

	_, err := Resolve(ResolveOptions{ProjectConfigPath: path, Env: map[string]string{}})
	if err == nil {
		t.Fatal("Resolve() error = nil for negative deferThreshold, want failure")
	}
	if !strings.Contains(err.Error(), "tools.deferThreshold") {
		t.Fatalf("Resolve() error = %v, want tools.deferThreshold rejection", err)
	}
}

func TestMergeProfilePreservesParseThinkTags(t *testing.T) {
	yes := true
	// next supplies the setting; a nil base must inherit it (it was being dropped).
	merged := mergeProfile(ProviderProfile{Name: "p"}, ProviderProfile{ParseThinkTags: &yes})
	if merged.ParseThinkTags == nil || !*merged.ParseThinkTags {
		t.Fatalf("mergeProfile dropped ParseThinkTags from next: %v", merged.ParseThinkTags)
	}
	// A nil next must not clobber an explicit base value.
	keep := mergeProfile(ProviderProfile{ParseThinkTags: &yes}, ProviderProfile{})
	if keep.ParseThinkTags == nil || !*keep.ParseThinkTags {
		t.Fatalf("mergeProfile clobbered base ParseThinkTags with a nil next: %v", keep.ParseThinkTags)
	}
}

func TestMergeConfigUnionsSandboxAdditionalWriteRoots(t *testing.T) {
	dst := FileConfig{}
	dst.Sandbox.AdditionalWriteRoots = []string{"/global/one"}
	src := FileConfig{}
	src.Sandbox.AdditionalWriteRoots = []string{"/extra/one", "/global/one"}
	mergeConfig(&dst, src)
	want := []string{"/global/one", "/extra/one"}
	if !reflect.DeepEqual(dst.Sandbox.AdditionalWriteRoots, want) {
		t.Fatalf("AdditionalWriteRoots=%v want union %v (append + dedupe, not replace)", dst.Sandbox.AdditionalWriteRoots, want)
	}
}

func TestMergeProjectConfigIgnoresAdditionalWriteRoots(t *testing.T) {
	dst := FileConfig{}
	dst.Sandbox.AdditionalWriteRoots = []string{"/global/one"}
	src := FileConfig{}
	src.Sandbox.AdditionalWriteRoots = []string{"/repo/sneaky"}
	if err := mergeProjectConfig(&dst, src); err != nil {
		t.Fatalf("mergeProjectConfig: %v", err)
	}
	if !reflect.DeepEqual(dst.Sandbox.AdditionalWriteRoots, []string{"/global/one"}) {
		t.Fatalf("AdditionalWriteRoots=%v — project config must NOT be able to add write roots", dst.Sandbox.AdditionalWriteRoots)
	}
}

func TestResolveRejectsNegativeMaxTurns(t *testing.T) {
	path := writeConfig(t, `{"maxTurns": -5}`)

	_, err := Resolve(ResolveOptions{UserConfigPath: path, Env: map[string]string{}})
	if err == nil || !strings.Contains(err.Error(), "invalid maxTurns") {
		t.Fatalf("expected negative maxTurns to be rejected, got %v", err)
	}
	// The message must match the accepted range: 0 is allowed (only < 0 is rejected).
	if !strings.Contains(err.Error(), ">= 0") {
		t.Fatalf("error message should state the accepted range (>= 0), got %v", err)
	}
}

func TestResolveMaxTurnsZeroFallsBackToDefault(t *testing.T) {
	// 0 is indistinguishable from "unset" under omitempty, so it must NOT error
	// and must fall back to the default rather than being treated as a 0 limit.
	path := writeConfig(t, `{"maxTurns": 0}`)

	resolved, err := Resolve(ResolveOptions{UserConfigPath: path, Env: map[string]string{}})
	if err != nil {
		t.Fatalf("maxTurns 0 should resolve cleanly, got %v", err)
	}
	if resolved.MaxTurns != defaultMaxTurns {
		t.Fatalf("MaxTurns = %d, want default %d", resolved.MaxTurns, defaultMaxTurns)
	}
}

// The reported brick: a hand-written google profile with an apiKey but no
// model made EVERY resolving command fail ("provider google requires model"),
// including zero config and bare zero setup — the only commands that could
// have fixed it. Official-API kinds now fall back to their catalog default
// model, exactly like the openai kind always has.
func TestResolveDefaultsGoogleModelFromCatalog(t *testing.T) {
	path := writeConfig(t, `{
		"activeProvider": "google",
		"providers": [{"name": "google", "provider_kind": "google", "apiKey": "AIza-x"}]
	}`)
	resolved, err := Resolve(ResolveOptions{UserConfigPath: path, Env: map[string]string{}})
	if err != nil {
		t.Fatalf("Resolve() error = %v, want model defaulted from the google catalog", err)
	}
	if resolved.Provider.Model != "gemini-2.5-pro" {
		t.Fatalf("Model = %q, want the google catalog default gemini-2.5-pro", resolved.Provider.Model)
	}
	if resolved.Provider.BaseURL != GoogleBaseURL {
		t.Fatalf("BaseURL = %q, want %q", resolved.Provider.BaseURL, GoogleBaseURL)
	}
}

func TestResolveDefaultsAnthropicModelFromCatalog(t *testing.T) {
	path := writeConfig(t, `{
		"activeProvider": "anthropic",
		"providers": [{"name": "anthropic", "provider_kind": "anthropic", "apiKey": "sk-ant-x"}]
	}`)
	resolved, err := Resolve(ResolveOptions{UserConfigPath: path, Env: map[string]string{}})
	if err != nil {
		t.Fatalf("Resolve() error = %v, want model defaulted from the anthropic catalog", err)
	}
	if resolved.Provider.Model != "claude-sonnet-4.5" {
		t.Fatalf("Model = %q, want the anthropic catalog default claude-sonnet-4.5", resolved.Provider.Model)
	}
}

// An explicitly configured model must always win over the catalog default.
func TestResolveKeepsExplicitGoogleModel(t *testing.T) {
	path := writeConfig(t, `{
		"activeProvider": "google",
		"providers": [{"name": "google", "provider_kind": "google", "apiKey": "AIza-x", "model": "gemini-2.5-flash"}]
	}`)
	resolved, err := Resolve(ResolveOptions{UserConfigPath: path, Env: map[string]string{}})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if resolved.Provider.Model != "gemini-2.5-flash" {
		t.Fatalf("Model = %q, want the explicitly configured gemini-2.5-flash", resolved.Provider.Model)
	}
}

// Provider-command configs must NOT get invented models: the without-defaults
// path surfaces exactly what the external command returned.
func TestNormalizeWithoutModelDefaultsStillRequiresGoogleModel(t *testing.T) {
	_, _, err := normalizeProvidersWithoutModelDefaults(
		[]ProviderProfile{{Name: "google", ProviderKind: ProviderKindGoogle, APIKey: "AIza-x"}},
		"google", map[string]string{})
	if err == nil {
		t.Fatal("provider-command path must still require an explicit model")
	}
	if !strings.Contains(err.Error(), "requires model") {
		t.Fatalf("error = %q, want requires-model", err.Error())
	}
	// The residual error must tell the user how to fix it.
	if !strings.Contains(err.Error(), "config.json") {
		t.Fatalf("error = %q, want an actionable config.json hint", err.Error())
	}
}

// The residual requires-model failure (custom endpoint, no catalog default)
// must be tagged setup-fixable WITHOUT changing its message: the interactive
// TUI matches the sentinel to drop into the wizard, while headless commands
// print the exact actionable text.
func TestResolveRequiresModelErrorIsSetupFixable(t *testing.T) {
	path := writeConfig(t, `{
		"activeProvider": "gw",
		"providers": [{"name": "gw", "provider_kind": "openai-compatible", "baseURL": "https://gw.example/v1", "apiKey": "sk-x"}]
	}`)
	_, err := Resolve(ResolveOptions{UserConfigPath: path, Env: map[string]string{}})
	if err == nil {
		t.Fatal("expected requires-model error for a custom endpoint without model")
	}
	if !errors.Is(err, ErrProviderRequiresModel) {
		t.Fatalf("error = %v, want errors.Is(err, ErrProviderRequiresModel)", err)
	}
	// The sentinel must NOT prefix the user-facing message.
	if !strings.HasPrefix(err.Error(), "provider gw requires model") {
		t.Fatalf("message = %q, want it to start with the provider error, not the sentinel text", err.Error())
	}
	if strings.Contains(err.Error(), "sk-x") {
		t.Fatalf("error leaked API key: %q", err.Error())
	}
}

func TestCrossSessionInboundProjectConfigCanOnlyTighten(t *testing.T) {
	tests := []struct {
		name    string
		user    string
		project string
		want    string
	}{
		{name: "hold tightens accept", user: "accept", project: "hold", want: "hold"},
		{name: "refuse stays refuse", user: "refuse", project: "accept", want: "refuse"},
		{name: "accept cannot weaken parity", project: "accept", want: ""},
		{name: "refuse tightens parity", project: "refuse", want: "refuse"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := FileConfig{CrossSessionInbound: test.user}
			if err := mergeProjectConfig(&cfg, FileConfig{CrossSessionInbound: test.project}); err != nil {
				t.Fatal(err)
			}
			if cfg.CrossSessionInbound != test.want {
				t.Fatalf("crossSessionInbound = %q, want %q", cfg.CrossSessionInbound, test.want)
			}
		})
	}
}

func TestResolveRejectsInvalidCrossSessionInbound(t *testing.T) {
	path := writeConfig(t, `{
		"activeProvider": "openai",
		"providers": [{"name":"openai","provider_kind":"openai","apiKey":"sk-x","model":"gpt-5"}],
		"crossSessionInbound": "sometimes"
	}`)
	_, err := Resolve(ResolveOptions{UserConfigPath: path, Env: map[string]string{}})
	if err == nil || !strings.Contains(err.Error(), "expected accept, hold, or refuse") {
		t.Fatalf("error = %v", err)
	}
}

func TestResolveReportsExplicitMaxTurns(t *testing.T) {
	const providerJSON = `{
		"activeProvider": "p",
		"providers": [{"name": "p", "provider_kind": "openai-compatible", "base_url": "https://x.example/v1", "apiKey": "k", "model": "m"}]
	}`
	providerWithTurns := strings.Replace(providerJSON, `"activeProvider": "p"`, `"activeProvider": "p", "maxTurns": 40`, 1)

	cases := []struct {
		name string
		opts ResolveOptions
		set  bool
	}{
		{"built-in default only", ResolveOptions{
			Overrides: Overrides{Provider: ProviderProfile{Name: "p", ProviderKind: "openai-compatible", BaseURL: "https://x.example/v1", APIKey: "k", Model: "m"}},
		}, false},
		{"user config", ResolveOptions{UserConfigPath: writeConfig(t, providerWithTurns)}, true},
		{"project config", ResolveOptions{
			UserConfigPath:    writeConfig(t, providerJSON),
			ProjectConfigPath: writeConfig(t, `{"maxTurns": 40}`),
		}, true},
		{"env", ResolveOptions{
			Env:       map[string]string{MaxTurnsEnv: "120"},
			Overrides: Overrides{Provider: ProviderProfile{Name: "p", ProviderKind: "openai-compatible", BaseURL: "https://x.example/v1", APIKey: "k", Model: "m"}},
		}, true},
		{"cli override", ResolveOptions{
			Overrides: Overrides{
				MaxTurns: 42,
				Provider: ProviderProfile{Name: "p", ProviderKind: "openai-compatible", BaseURL: "https://x.example/v1", APIKey: "k", Model: "m"},
			},
		}, true},
	}
	for _, tc := range cases {
		resolved, err := Resolve(tc.opts)
		if err != nil {
			t.Fatalf("%s: Resolve() error = %v", tc.name, err)
		}
		if resolved.MaxTurnsSet != tc.set {
			t.Fatalf("%s: MaxTurnsSet = %v, want %v (MaxTurns=%d)", tc.name, resolved.MaxTurnsSet, tc.set, resolved.MaxTurns)
		}
	}
}
