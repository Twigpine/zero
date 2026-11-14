package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The plan-size tier, proved END TO END: a .zero/config.json setting has to
// reach the ceiling a plan is actually rejected against.
//
// A test on registerSpecialistTools would pass while `zero exec` never read the
// setting at all — that is the exact shape of this feature's recurring defect,
// a field correctly threaded through everything except the one call site that
// populates it. So this drives Run(), writes a real config file, and asserts on
// what the model is told.
func TestExecReadsThePlanSizeTierFromProjectConfig(t *testing.T) {
	clearProviderEnv(t)
	root := t.TempDir()
	configDir := filepath.Join(root, ".zero")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}

	// BOTH probes are plans that get REJECTED, at DIFFERENT ceilings. An admitted
	// plan would spawn real child processes — six of them — so the control here
	// cannot be "and this one is admitted"; it is "and this one is rejected at
	// the other tier's number", which isolates the configured value just as well
	// and executes nothing.
	planArgs := func(n int) string {
		tasks := make([]map[string]any, 0, n)
		for i := 0; i < n; i++ {
			tasks = append(tasks, map[string]any{"id": "t" + strings.Repeat("z", i), "prompt": "look"})
		}
		encoded, err := json.Marshal(map[string]any{
			"name":   "sweep",
			"tasks":  tasks,
			"budget": map[string]any{"max_workers": 1},
		})
		if err != nil {
			t.Fatal(err)
		}
		return string(encoded)
	}

	var plan string
	var toolOutput string
	turn := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		turn++
		if turn == 1 {
			call, _ := json.Marshal(map[string]any{
				"choices": []any{map[string]any{"delta": map[string]any{
					"tool_calls": []any{map[string]any{
						"index": 0, "id": "call_1", "type": "function",
						"function": map[string]any{"name": "orchestrate", "arguments": plan},
					}},
				}}},
			})
			_, _ = io.WriteString(w, "data: "+string(call)+"\n\n")
			_, _ = io.WriteString(w, `data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`+"\n\n")
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
			return
		}
		// The second turn carries the tool result back. That message is what the
		// model would have been told, and it is the assertion target.
		if messages, ok := body["messages"].([]any); ok {
			for _, raw := range messages {
				message, _ := raw.(map[string]any)
				if message["role"] == "tool" {
					if content, ok := message["content"].(string); ok {
						toolOutput += content
					}
				}
			}
		}
		_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"content":"done"}}]}`+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	providerConfig := func(planSize string) string {
		profiles := ""
		if planSize != "" {
			profiles = `"profiles": {"planSize": "` + planSize + `"},`
		}
		return `{
			` + profiles + `
			"activeProvider": "local",
			"providers": [{
				"name": "local",
				"provider_kind": "openai-compatible",
				"base_url": "` + server.URL + `",
				"api_key": "sk-local",
				"model": "local-model"
			}]
		}`
	}

	run := func(planSize string, taskCount int) string {
		turn = 0
		toolOutput = ""
		plan = planArgs(taskCount)
		if err := os.WriteFile(filepath.Join(configDir, "config.json"), []byte(providerConfig(planSize)), 0o600); err != nil {
			t.Fatal(err)
		}
		var stdout, stderr bytes.Buffer
		Run([]string{"exec", "--cwd", root, "--reasoning-effort", "zeromaxing", "--auto", "high", "sweep it"}, &stdout, &stderr)
		return toolOutput
	}

	// small: a six-task plan is refused at 5, and the message names the tier that
	// refused it plus how to raise it.
	small := run("small", 6)
	for _, want := range []string{`"small" plan size`, "planSize", "exceeds the limit of 5"} {
		if !strings.Contains(small, want) {
			t.Fatalf("under planSize=small the tool output must contain %q; got:\n%s", want, small)
		}
	}

	// THE OTHER DIRECTION, which is what makes the first half mean anything: with
	// no setting the ceiling is the DEFAULT tier's 20, not small's 5. Without
	// this, a run that ignored config entirely and always used one hard-coded
	// number would satisfy the assertion above.
	unset := run("", 25)
	for _, want := range []string{`"medium" plan size`, "exceeds the limit of 20"} {
		if !strings.Contains(unset, want) {
			t.Fatalf("with no planSize set the ceiling must be the default tier's; want %q, got:\n%s", want, unset)
		}
	}
	if strings.Contains(unset, "limit of 5") {
		t.Fatalf("an unset planSize applied small's ceiling:\n%s", unset)
	}
}
