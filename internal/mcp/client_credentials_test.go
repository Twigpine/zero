package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// fakeCredentialStore stands in for Zero's credential store. It records every
// lookup so a test can prove a server with no references never reaches it.
type fakeCredentialStore struct {
	values  map[string]string
	err     error
	lookups []string
}

func (store *fakeCredentialStore) Get(name string) (string, bool, error) {
	store.lookups = append(store.lookups, name)
	if store.err != nil {
		return "", false, store.err
	}
	value, ok := store.values[name]
	return value, ok, nil
}

func credentialHelperServer(t *testing.T, envFrom map[string]string, env map[string]string) Server {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	merged := map[string]string{"ZERO_MCP_ENV_HELPER": "1"}
	for key, value := range env {
		merged[key] = value
	}
	return Server{
		Name:    "memlawb",
		Type:    ServerTypeStdio,
		Command: executable,
		Args:    []string{"-test.run=TestMCPEnvHelperProcess", "--"},
		Env:     merged,
		EnvFrom: envFrom,
	}
}

func TestStdioCredentialReferencesReachChildEnvironment(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	store := &fakeCredentialStore{values: map[string]string{
		"memlawb-passphrase": "correct-horse-battery-staple",
		"memlawb-api-key":    "mk_live_fake",
	}}
	server := credentialHelperServer(t,
		map[string]string{"MEMLAWB_PASSPHRASE": "memlawb-passphrase", "MEMLAWB_API_KEY": "memlawb-api-key"},
		map[string]string{"MEMLAWB_URL": "https://memory.gitlawb.com"},
	)

	client, err := ConnectWithOptions(ctx, server, ConnectOptions{Credentials: store})
	if err != nil {
		t.Fatalf("ConnectWithOptions() error = %v", err)
	}
	defer client.Close()

	read := func(name string) string {
		result, err := client.CallTool(ctx, "env", map[string]any{"name": name})
		if err != nil {
			t.Fatalf("CallTool(%s) error = %v", name, err)
		}
		return TextContent(result.Content)
	}
	if got := read("MEMLAWB_PASSPHRASE"); got != "correct-horse-battery-staple" {
		t.Fatalf("child MEMLAWB_PASSPHRASE = %q, want the store's value", got)
	}
	if got := read("MEMLAWB_API_KEY"); got != "mk_live_fake" {
		t.Fatalf("child MEMLAWB_API_KEY = %q, want the store's value", got)
	}
	// Control: the verbatim env still arrives, so the assertions above are
	// reading a real child environment rather than an empty one.
	if got := read("MEMLAWB_URL"); got != "https://memory.gitlawb.com" {
		t.Fatalf("child MEMLAWB_URL = %q, want the verbatim env value", got)
	}
	// Control: an unset variable comes back empty, so "found" is not the
	// helper's default answer.
	if got := read("MEMLAWB_NOT_SET"); got != "" {
		t.Fatalf("unset variable = %q, want empty", got)
	}
}

func TestStdioMissingCredentialFailsConnectNamingOnlyTheCredential(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// The key resolves; the passphrase does not. A connect that leaked the
	// resolved environment into its error would carry the key's value.
	store := &fakeCredentialStore{values: map[string]string{"memlawb-api-key": "mk_live_leak_probe"}}
	server := credentialHelperServer(t,
		map[string]string{"MEMLAWB_PASSPHRASE": "memlawb-passphrase", "MEMLAWB_API_KEY": "memlawb-api-key"},
		nil,
	)

	client, err := ConnectWithOptions(ctx, server, ConnectOptions{Credentials: store})
	if err == nil {
		client.Close()
		t.Fatal("ConnectWithOptions() error = nil, want a missing-credential failure")
	}
	message := err.Error()
	if !strings.Contains(message, "memlawb-passphrase") {
		t.Fatalf("error = %q, want the missing credential named", message)
	}
	if !strings.Contains(message, "memlawb") {
		t.Fatalf("error = %q, want the server named", message)
	}
	if strings.Contains(message, "mk_live_leak_probe") {
		t.Fatalf("error leaked a credential value: %q", message)
	}
}

func TestStdioCredentialStoreErrorFailsConnect(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	store := &fakeCredentialStore{err: fmt.Errorf("keyring locked")}
	server := credentialHelperServer(t, map[string]string{"MEMLAWB_PASSPHRASE": "memlawb-passphrase"}, nil)

	client, err := ConnectWithOptions(ctx, server, ConnectOptions{Credentials: store})
	if err == nil {
		client.Close()
		t.Fatal("ConnectWithOptions() error = nil, want the store error surfaced")
	}
	if !strings.Contains(err.Error(), "memlawb-passphrase") || !strings.Contains(err.Error(), "keyring locked") {
		t.Fatalf("error = %q, want the credential name and the store error", err.Error())
	}
}

func TestStdioWithoutReferencesNeverReadsTheCredentialStore(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	store := &fakeCredentialStore{values: map[string]string{"memlawb-passphrase": "unused"}}
	server := credentialHelperServer(t, nil, nil)

	client, err := ConnectWithOptions(ctx, server, ConnectOptions{Credentials: store})
	if err != nil {
		t.Fatalf("ConnectWithOptions() error = %v", err)
	}
	defer client.Close()
	if len(store.lookups) != 0 {
		t.Fatalf("credential lookups = %v, want none for a server with no references", store.lookups)
	}
}

// TestMCPEnvHelperProcess is a stdio MCP server whose only tool reports one
// environment variable of the child process, so a test can see exactly what the
// spawn path handed it.
func TestMCPEnvHelperProcess(t *testing.T) {
	if os.Getenv("ZERO_MCP_ENV_HELPER") != "1" {
		return
	}
	reader := newMessageReader(os.Stdin)
	writer := newMessageWriter(os.Stdout)
	for {
		message, err := reader.read()
		if err != nil {
			os.Exit(0)
		}
		if message.Method == "notifications/initialized" {
			continue
		}
		switch message.Method {
		case "initialize":
			_ = writer.write(rpcMessage{
				JSONRPC: "2.0",
				ID:      message.ID,
				Result: mustRaw(map[string]any{
					"protocolVersion": "2024-11-05",
					"capabilities":    map[string]any{"tools": map[string]any{}},
					"serverInfo":      map[string]any{"name": "test-env", "version": "1.0.0"},
				}),
			})
		case "tools/list":
			_ = writer.write(rpcMessage{
				JSONRPC: "2.0",
				ID:      message.ID,
				Result: mustRaw(map[string]any{
					"tools": []map[string]any{{
						"name":        "env",
						"description": "Report one environment variable",
						"inputSchema": map[string]any{"type": "object", "properties": map[string]any{"name": map[string]any{"type": "string"}}},
					}},
				}),
			})
		case "tools/call":
			var params struct {
				Arguments map[string]any `json:"arguments"`
			}
			_ = json.Unmarshal(message.Params, &params)
			name, _ := params.Arguments["name"].(string)
			_ = writer.write(rpcMessage{
				JSONRPC: "2.0",
				ID:      message.ID,
				Result: mustRaw(map[string]any{
					"content": []map[string]any{{"type": "text", "text": os.Getenv(name)}},
				}),
			})
		default:
			_ = writer.write(rpcMessage{JSONRPC: "2.0", ID: message.ID, Error: &rpcError{Code: -32601, Message: "method not found"}})
		}
	}
}
