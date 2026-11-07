package config

import (
	"os"
	"path/filepath"
	"testing"
)

func writeConfigs(t *testing.T, user, project string) ResolvedConfig {
	t.Helper()
	dir := t.TempDir()
	userPath := filepath.Join(dir, "user.json")
	projectPath := filepath.Join(dir, "project.json")
	if err := os.WriteFile(userPath, []byte(user), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(projectPath, []byte(project), 0o600); err != nil {
		t.Fatal(err)
	}
	resolved, err := Resolve(ResolveOptions{UserConfigPath: userPath, ProjectConfigPath: projectPath})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	return resolved
}

const someProvider = `"providers":[{"name":"p","provider_kind":"openai-compatible","catalogID":"xai",` +
	`"baseURL":"https://api.x.ai/v1","apiFormat":"chat-completions","model":"grok-4.5","apiKey":"k"}],"activeProvider":"p"`

// BOTH FLAGS DEFAULT OFF, and that is what keeps a build carrying them
// byte-identical to one without them.
func TestTheNewProfileFlagsDefaultOff(t *testing.T) {
	resolved := writeConfigs(t, `{`+someProvider+`}`, `{}`)
	if resolved.Profiles.RequirePlanKeyword {
		t.Error("RequirePlanKeyword defaulted on; it would refuse plans for everyone whose phrasing does not match")
	}
	if resolved.Profiles.Memory {
		t.Error("Memory defaulted on; registering its tools changes the advertised tool set for every run")
	}
}

// USER CONFIG MAY SET EITHER. It is the higher-trust source.
func TestUserConfigMaySetBothFlags(t *testing.T) {
	resolved := writeConfigs(t,
		`{`+someProvider+`,"profiles":{"requirePlanKeyword":true,"memory":true}}`, `{}`)
	if !resolved.Profiles.RequirePlanKeyword {
		t.Error("user config could not enable the plan-keyword gate")
	}
	if !resolved.Profiles.Memory {
		t.Error("user config could not enable memory")
	}
}

// A PROJECT MAY TIGHTEN A SAFETY GATE. Asking for one is not a privilege
// escalation — it is a repo asking to be treated more carefully.
func TestProjectConfigMayEnableThePlanKeywordGate(t *testing.T) {
	resolved := writeConfigs(t, `{`+someProvider+`}`, `{"profiles":{"requirePlanKeyword":true}}`)
	if !resolved.Profiles.RequirePlanKeyword {
		t.Error("a project could not ask for the plan-keyword gate")
	}
}

// ...AND MAY NOT HAND ITSELF A WRITE PRIMITIVE. memory_write writes into the
// workspace, and project config is not trust-gated: a cloned repo must not be
// able to give the agent a new way to write into it.
func TestProjectConfigCannotEnableMemory(t *testing.T) {
	resolved := writeConfigs(t, `{`+someProvider+`}`, `{"profiles":{"memory":true}}`)
	if resolved.Profiles.Memory {
		t.Error("a cloned repo enabled the memory write tool for whoever opened it")
	}
}

// A project cannot switch the gate back OFF either — that direction is a
// downgrade, and presence-only means an attempted false is simply not there.
func TestProjectConfigCannotDisableThePlanKeywordGate(t *testing.T) {
	resolved := writeConfigs(t,
		`{`+someProvider+`,"profiles":{"requirePlanKeyword":true}}`,
		`{"profiles":{"requirePlanKeyword":false}}`)
	if !resolved.Profiles.RequirePlanKeyword {
		t.Error("a project turned the user's safety gate off")
	}
}
