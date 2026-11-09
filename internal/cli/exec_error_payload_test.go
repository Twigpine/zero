package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/Twigpine/zero/internal/config"
	"github.com/Twigpine/zero/internal/sandbox"
	"github.com/Twigpine/zero/internal/sessions"
	"github.com/Twigpine/zero/internal/zeroruntime"
)

// streamErrorExecProvider fails every request with one provider error event.
type streamErrorExecProvider struct {
	event zeroruntime.StreamEvent
}

func (provider streamErrorExecProvider) StreamCompletion(ctx context.Context, _ zeroruntime.CompletionRequest) (<-chan zeroruntime.StreamEvent, error) {
	ch := make(chan zeroruntime.StreamEvent, 1)
	ch <- provider.event
	close(ch)
	return ch, nil
}

func providerErrorExecDeps(t *testing.T, store *sessions.Store, workspaceRoot string) appDeps {
	t.Helper()
	return appDeps{
		getwd: func() (string, error) { return workspaceRoot, nil },
		resolveConfig: func(_ string, _ config.Overrides) (config.ResolvedConfig, error) {
			return config.ResolvedConfig{
				ActiveProvider: "test",
				Provider: config.ProviderProfile{
					Name:         "test",
					ProviderKind: config.ProviderKindOpenAICompatible,
					BaseURL:      "http://127.0.0.1/v1",
					Model:        "test-model",
				},
				MaxTurns: 3,
			}, nil
		},
		resolveMCPConfig: func(string, bool) (config.MCPConfig, error) { return config.MCPConfig{}, nil },
		newProvider: func(config.ProviderProfile) (zeroruntime.Provider, error) {
			return streamErrorExecProvider{event: zeroruntime.StreamEvent{
				Type:       zeroruntime.StreamEventError,
				Error:      "provider request error: model not found",
				StatusCode: 404,
				Cause:      "upstream said: no such model",
			}}, nil
		},
		newSessionStore: func() *sessions.Store { return store },
		newSandboxStore: func() (*sandbox.GrantStore, error) {
			return sandbox.NewGrantStore(sandbox.StoreOptions{FilePath: t.TempDir() + "/grants.json"})
		},
	}
}

// recordedProviderError returns the payload of the one error event the run
// recorded, failing unless there is exactly one session with exactly one.
func recordedProviderError(t *testing.T, store *sessions.Store) map[string]any {
	t.Helper()
	items, err := store.List()
	if err != nil || len(items) != 1 {
		t.Fatalf("sessions = %d, %v; want one", len(items), err)
	}
	events, err := store.ReadEvents(items[0].SessionID)
	if err != nil {
		t.Fatalf("ReadEvents returned error: %v", err)
	}
	var found []map[string]any
	for _, event := range events {
		if event.Type != sessions.EventError {
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatalf("unmarshal error payload: %v", err)
		}
		found = append(found, payload)
	}
	if len(found) != 1 {
		t.Fatalf("recorded %d error events, want 1: %#v", len(found), found)
	}
	return found[0]
}

func assertProviderErrorDetail(t *testing.T, payload map[string]any) {
	t.Helper()
	if got, want := payload["message"], "provider request error: model not found"; got != want {
		t.Errorf("message = %#v, want %q", got, want)
	}
	if got, want := payload["statusCode"], float64(404); got != want {
		t.Errorf("statusCode = %#v, want %v: the run recorded only the flattened message", got, want)
	}
	if got, want := payload["cause"], "upstream said: no such model"; got != want {
		t.Errorf("cause = %#v, want %q: the run recorded only the flattened message", got, want)
	}
}

// A provider error reaches the exec session log with its HTTP status and the
// upstream cause, not only the flattened message (#674).
func TestRunExecRecordsTheProviderErrorStatusAndCause(t *testing.T) {
	isolateCLIUserState(t)
	workspaceRoot := t.TempDir()
	// exec records through the default store, which isolateCLIUserState has
	// moved under the test's temp root; this opens the same one.
	store := sessions.NewStore(sessions.StoreOptions{})
	var stdout, stderr bytes.Buffer

	exitCode := runWithDeps([]string{"exec", "--cwd", workspaceRoot, "--init-session-id", "provider_error_run", "hello"}, &stdout, &stderr,
		providerErrorExecDeps(t, store, workspaceRoot))

	if exitCode != exitProvider {
		t.Fatalf("exit = %d, want %d; stderr=%s", exitCode, exitProvider, stderr.String())
	}
	assertProviderErrorDetail(t, recordedProviderError(t, store))
}

// The spec draft run records its provider error through its own call site, so
// it is checked on its own.
func TestRunExecUseSpecRecordsTheProviderErrorStatusAndCause(t *testing.T) {
	isolateCLIUserState(t)
	workspaceRoot := t.TempDir()
	store := sessions.NewStore(sessions.StoreOptions{RootDir: t.TempDir()})
	var stdout, stderr bytes.Buffer

	exitCode := runWithDeps([]string{"exec", "--cwd", workspaceRoot, "--use-spec", "-o", "json", "plan the review flow"}, &stdout, &stderr,
		providerErrorExecDeps(t, store, workspaceRoot))

	if exitCode != exitProvider {
		t.Fatalf("exit = %d, want %d; stderr=%s stdout=%s", exitCode, exitProvider, stderr.String(), stdout.String())
	}
	assertProviderErrorDetail(t, recordedProviderError(t, store))
}
