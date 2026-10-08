package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Gitlawb/zero/internal/agent"
	"github.com/Gitlawb/zero/internal/tools"
)

// escalate_model is a control-only tool (SideEffectNone). The stream-json tool
// call must report sideEffect "none", not "unknown", so automation sees the
// promised value.
func TestStreamJSONSideEffectReportsNoneForControlTool(t *testing.T) {
	registry := tools.NewRegistry()
	registry.Register(tools.NewEscalateModelTool())
	if got := streamJSONSideEffect("escalate_model", registry); got != "none" {
		t.Fatalf("streamJSONSideEffect(escalate_model) = %q, want none", got)
	}
}

func TestExecWriterPropagatesToolResultTruncation(t *testing.T) {
	for _, format := range []execOutputFormat{execOutputJSON, execOutputStreamJSON} {
		t.Run(string(format), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			writer := execEventWriter{
				stdout:       &stdout,
				stderr:       &stderr,
				format:       format,
				runID:        "run_budget",
				streamedText: &strings.Builder{},
			}
			writer.toolResult(agent.ToolResult{
				ToolCallID: "call_budget",
				Name:       "read_file",
				Status:     tools.StatusOK,
				Output:     "bounded output",
				Truncated:  true,
			})
			if writer.err != nil {
				t.Fatalf("toolResult: %v", writer.err)
			}
			var payload map[string]any
			if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &payload); err != nil {
				t.Fatalf("decode output %q: %v", stdout.String(), err)
			}
			if payload["truncated"] != true {
				t.Fatalf("truncated = %#v, want true; payload=%#v", payload["truncated"], payload)
			}
		})
	}
}

func TestExecWriterPropagatesCacheUsage(t *testing.T) {
	for _, format := range []execOutputFormat{execOutputJSON, execOutputStreamJSON} {
		t.Run(string(format), func(t *testing.T) {
			var stdout bytes.Buffer
			writer := execEventWriter{
				stdout:       &stdout,
				format:       format,
				runID:        "run_usage",
				streamedText: &strings.Builder{},
			}
			writer.usage(agent.Usage{
				InputTokens:       100,
				OutputTokens:      20,
				PromptTokens:      100,
				CompletionTokens:  20,
				CachedInputTokens: 60,
				CacheWriteTokens:  10,
			})
			if writer.err != nil {
				t.Fatalf("usage: %v", writer.err)
			}
			var payload map[string]any
			if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &payload); err != nil {
				t.Fatalf("decode output %q: %v", stdout.String(), err)
			}
			cachedKey, writeKey := "cached_input_tokens", "cache_write_tokens"
			if format == execOutputStreamJSON {
				cachedKey, writeKey = "cachedInputTokens", "cacheWriteTokens"
			}
			if payload[cachedKey] != float64(60) || payload[writeKey] != float64(10) {
				t.Fatalf("cache usage missing from payload: %#v", payload)
			}
		})
	}
}
