package cli

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/config"
	"github.com/Twigpine/zero/internal/sandbox"
)

func sandboxCheckDeps(t *testing.T) (appDeps, string) {
	t.Helper()
	store := newSandboxTestStore(t)
	root := t.TempDir()
	return appDeps{
		getwd:           func() (string, error) { return root, nil },
		newSandboxStore: func() (*sandbox.GrantStore, error) { return store, nil },
		resolveConfig: func(string, config.Overrides) (config.ResolvedConfig, error) {
			return config.ResolvedConfig{}, nil
		},
		selectSandboxBackend: func(sandbox.BackendOptions) sandbox.Backend {
			return sandbox.Backend{Name: sandbox.BackendUnavailable, Platform: "windows", Fallback: true}
		},
	}, root
}

func TestRunSandboxCheckJSONDeniesOutOfWorkspaceWrite(t *testing.T) {
	deps, _ := sandboxCheckDeps(t)
	// An absolute path outside the workspace, portable across OSes: a Unix literal
	// like /etc/passwd is not absolute on Windows (it would be joined into the
	// workspace and allowed), so build one under a non-temp test dir instead.
	outside := filepath.Join(tempDirOutsideDefaultTemp(t), "outside.txt")
	var stdout, stderr bytes.Buffer
	exitCode := runWithDeps([]string{"sandbox", "check", "write_file", "--side-effect", "write", "--path", outside, "--json"}, &stdout, &stderr, deps)
	if exitCode != exitSuccess {
		t.Fatalf("check exit=%d stderr=%s", exitCode, stderr.String())
	}
	var payload struct {
		Tool string `json:"tool"`
		Plan struct {
			Policy struct {
				EffectiveMode string `json:"effectiveMode"`
			} `json:"policy"`
			Backend struct {
				Name string `json:"name"`
			} `json:"backend"`
		} `json:"plan"`
		Decision struct {
			Action string `json:"action"`
			Risk   struct {
				Level string `json:"level"`
			} `json:"risk"`
			Block *struct {
				Code string `json:"code"`
			} `json:"block"`
		} `json:"decision"`
		Grant struct {
			ToolName string `json:"toolName"`
			Matched  bool   `json:"matched"`
		} `json:"grant"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("decode check JSON: %v\n%s", err, stdout.String())
	}
	if payload.Tool != "write_file" {
		t.Fatalf("tool = %q", payload.Tool)
	}
	if payload.Plan.Policy.EffectiveMode != string(sandbox.ModeEnforce) {
		t.Fatalf("effectiveMode = %q, want enforce", payload.Plan.Policy.EffectiveMode)
	}
	if payload.Plan.Backend.Name != string(sandbox.BackendUnavailable) {
		t.Fatalf("backend = %q", payload.Plan.Backend.Name)
	}
	if payload.Decision.Action != string(sandbox.ActionPrompt) {
		t.Fatalf("expected prompt for out-of-workspace write, got %q\n%s", payload.Decision.Action, stdout.String())
	}
	if payload.Decision.Block == nil {
		t.Fatalf("expected a block for the out-of-workspace write")
	}
	if payload.Grant.ToolName != "write_file" || payload.Grant.Matched {
		t.Fatalf("expected an unmatched grant for write_file, got %#v", payload.Grant)
	}
}

func TestRunSandboxCheckTextRendersDecisionAndPlan(t *testing.T) {
	deps, _ := sandboxCheckDeps(t)
	var stdout, stderr bytes.Buffer
	exitCode := runWithDeps([]string{"sandbox", "check", "read_file", "--side-effect", "read", "--path", "notes.txt"}, &stdout, &stderr, deps)
	if exitCode != exitSuccess {
		t.Fatalf("check exit=%d stderr=%s", exitCode, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{
		"Sandbox check: read_file",
		"decision: ",
		"risk: ",
		"policy: mode=enforce",
		"backend: unavailable",
		"grant: no grant matched this action",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("check text missing %q:\n%s", want, out)
		}
	}
}

func TestRunSandboxCheckRequiresTool(t *testing.T) {
	deps, _ := sandboxCheckDeps(t)
	var stdout, stderr bytes.Buffer
	exitCode := runWithDeps([]string{"sandbox", "check"}, &stdout, &stderr, deps)
	if exitCode == exitSuccess {
		t.Fatalf("expected a usage error when no tool is given, got success: %s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "tool name is required") {
		t.Fatalf("expected 'tool name is required', got %q", stderr.String())
	}
}

func TestRunSandboxCheckRejectsInvalidFlags(t *testing.T) {
	deps, _ := sandboxCheckDeps(t)
	for _, args := range [][]string{
		{"sandbox", "check", "read_file", "--side-effect", "wrtie"},
	} {
		var stdout, stderr bytes.Buffer
		if code := runWithDeps(args, &stdout, &stderr, deps); code == exitSuccess {
			t.Fatalf("args %v: expected a usage error, got success: %s", args, stdout.String())
		}
		if !strings.Contains(stderr.String(), "invalid --") {
			t.Fatalf("args %v: expected an 'invalid --…' usage error, got %q", args, stderr.String())
		}
	}
}

func TestRunSandboxCheckMatchedGrantRedactsReason(t *testing.T) {
	store := newSandboxTestStore(t)
	root := t.TempDir()
	deps := appDeps{
		getwd:           func() (string, error) { return root, nil },
		newSandboxStore: func() (*sandbox.GrantStore, error) { return store, nil },
		resolveConfig: func(string, config.Overrides) (config.ResolvedConfig, error) {
			return config.ResolvedConfig{}, nil
		},
		selectSandboxBackend: func(sandbox.BackendOptions) sandbox.Backend {
			return sandbox.Backend{Name: sandbox.BackendUnavailable}
		},
	}
	// Seed a tool-wide allow grant whose reason embeds a secret; the matched-grant
	// snapshot must redact it.
	if _, err := store.Grant(sandbox.GrantInput{
		ToolName: "read_file",
		Decision: sandbox.GrantAllow,
		Reason:   "approved with sk-test-secret1234567890",
	}); err != nil {
		t.Fatalf("seed grant: %v", err)
	}

	var stdout, stderr bytes.Buffer
	exitCode := runWithDeps([]string{"sandbox", "check", "read_file", "--json"}, &stdout, &stderr, deps)
	if exitCode != exitSuccess {
		t.Fatalf("check exit=%d stderr=%s", exitCode, stderr.String())
	}
	if strings.Contains(stdout.String(), "sk-test-secret1234567890") {
		t.Fatalf("grant reason leaked a secret into the snapshot:\n%s", stdout.String())
	}
	var payload struct {
		Grant struct {
			Matched bool `json:"matched"`
			Grant   *struct {
				Reason string `json:"reason"`
			} `json:"grant"`
		} `json:"grant"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("decode check JSON: %v\n%s", err, stdout.String())
	}
	if !payload.Grant.Matched || payload.Grant.Grant == nil {
		t.Fatalf("expected a matched grant, got %#v", payload.Grant)
	}
	if !strings.Contains(payload.Grant.Grant.Reason, "REDACTED") {
		t.Fatalf("expected the reason to be redacted, got %q", payload.Grant.Grant.Reason)
	}
}
