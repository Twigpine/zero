package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/config"
	"github.com/Twigpine/zero/internal/mcp"
	"github.com/Twigpine/zero/internal/tools"
	"github.com/Twigpine/zero/internal/tui"
	"github.com/Twigpine/zero/internal/zeroruntime"
)

// The orchestrate notice, wired at both surfaces.
//
// AgentOptions.OrchestrateAvailable was declared, documented and consumed by the
// reminder selector — and set by NEITHER production call site. So the tool was
// advertised in the tool list and the model was never told it existed, which is
// exactly what a user found by running the binary: the posture turned on and
// nothing orchestrated.
//
// A unit test on zeromaxingReminders PASSED throughout that whole period,
// because it passes the flag in itself. So these tests drive the two
// constructions instead — Run() to the wire for exec, and the captured
// tui.Options for the TUI — which is the only level at which the defect was
// visible. Invariant 1, for the second time in this feature.

// EXEC: the notice has to reach the actual request body.
func TestExecTellsTheModelAboutOrchestrate(t *testing.T) {
	clearProviderEnv(t)
	root := t.TempDir()
	configDir := filepath.Join(root, ".zero")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}

	var sent []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if sent == nil {
			sent = body
		}
		_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"content":"ok"}}]}`+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	providerConfig := `{
		"activeProvider": "local",
		"providers": [{
			"name": "local",
			"provider_kind": "openai-compatible",
			"base_url": "` + server.URL + `",
			"api_key": "sk-local",
			"model": "local-model"
		}]
	}`
	if err := os.WriteFile(filepath.Join(configDir, "config.json"), []byte(providerConfig), 0o600); err != nil {
		t.Fatal(err)
	}

	run := func(args ...string) string {
		sent = nil
		var stdout, stderr bytes.Buffer
		Run(append([]string{"exec", "--cwd", root}, args...), &stdout, &stderr)
		return string(sent)
	}

	// POSTURE ON, specialist tools registered: the model is TOLD.
	on := run("--reasoning-effort", "zeromaxing", "--auto", "high", "look at this")
	if !strings.Contains(on, "orchestrate tool") {
		t.Fatalf("the model was never told the orchestrate tool exists:\n%s", firstKB(on))
	}
	// ...and the tool really is advertised, so the notice is not a promise the
	// run cannot keep.
	if !advertisesOrchestrate(t, on) {
		t.Fatalf("the notice names a tool this run does not advertise:\n%s", firstKB(on))
	}

	// POSTURE OFF: neither the notice nor the tool. This is the direction that
	// makes the first half mean something — a notice emitted unconditionally
	// would satisfy the assertion above.
	off := run("--auto", "high", "look at this")
	if strings.Contains(off, "orchestrate") {
		t.Fatalf("orchestrate leaked into a posture-off run:\n%s", firstKB(off))
	}

	// POSTURE ON but at an autonomy that registers NO specialist tools: the
	// notice must not fire, or it promises a capability this run does not have.
	unavailable := run("--reasoning-effort", "zeromaxing", "--auto", "low", "look at this")
	if advertisesOrchestrate(t, unavailable) {
		t.Skip("this autonomy level registers orchestrate after all; the case cannot be exercised here")
	}
	if strings.Contains(unavailable, "orchestrate tool") {
		t.Fatalf("the notice fired for a run that does not hold the tool:\n%s", firstKB(unavailable))
	}
}

func advertisesOrchestrate(t *testing.T, body string) bool {
	t.Helper()
	var request struct {
		Tools []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	if json.Unmarshal([]byte(body), &request) != nil {
		return false
	}
	for _, tool := range request.Tools {
		if tool.Function.Name == "orchestrate" {
			return true
		}
	}
	return false
}

func firstKB(body string) string {
	if len(body) > 1200 {
		return body[:1200]
	}
	return body
}

// TUI: the same fact, at the only seam a test can reach. The TUI cannot be
// driven headlessly, so this asserts what it is HANDED — which is where the
// field was missing.
func TestTheTUIIsHandedTheOrchestrateAvailability(t *testing.T) {
	var stdout, stderr bytes.Buffer
	cwd := t.TempDir()
	setCLIUserConfigRoot(t)
	userConfigPath := filepath.Join(t.TempDir(), "zero", "config.json")
	var launched tui.Options

	exitCode := runWithDeps([]string{}, &stdout, &stderr, appDeps{
		getwd: func() (string, error) { return cwd, nil },
		resolveConfig: func(string, config.Overrides) (config.ResolvedConfig, error) {
			return config.ResolvedConfig{
				MaxTurns:       12,
				ActiveProvider: "local",
				Provider: config.ProviderProfile{
					Name: "local", ProviderKind: config.ProviderKindOpenAICompatible,
					BaseURL: "http://127.0.0.1/v1", Model: "m",
				},
			}, nil
		},
		newProvider:    func(config.ProviderProfile) (zeroruntime.Provider, error) { return nil, nil },
		userConfigPath: func() (string, error) { return userConfigPath, nil },
		registerMCPTools: func(context.Context, *tools.Registry, config.MCPConfig, mcp.RegisterOptions) (mcpToolRuntime, error) {
			return noopMCPRuntime{}, nil
		},
		runTUI: func(_ context.Context, options tui.Options) int {
			launched = options
			return 3
		},
	})
	if exitCode != 3 {
		t.Fatalf("exit = %d, want 3: %s", exitCode, stderr.String())
	}

	// The TUI always registers specialist tools, so it always holds orchestrate;
	// the flag must say so, or the notice can never fire in the surface where
	// the posture is most used.
	if !launched.AgentOptions.OrchestrateAvailable {
		t.Fatal("the TUI was handed OrchestrateAvailable=false while holding the tool, so the model is never told it exists")
	}
	// ...and it agrees with the registry it was handed, rather than being a
	// constant that happens to read true.
	if _, found := launched.AgentOptions.Registry.Get("orchestrate"); !found {
		t.Fatal("the TUI's registry does not hold orchestrate, so the flag is wrong in the other direction")
	}
}

// The derivation itself: false when the tool is absent, so the flag can never
// promise a capability a run does not have.
func TestOrchestrateAvailabilityFollowsTheRegistry(t *testing.T) {
	if orchestrateAvailable(nil) {
		t.Fatal("a nil registry reported the tool available")
	}
	empty := tools.NewRegistry()
	if orchestrateAvailable(empty) {
		t.Fatal("an empty registry reported the tool available")
	}
}
