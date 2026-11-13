package mcp

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/tools"
)

func TestServePromptsListReturnsCuratedSet(t *testing.T) {
	var input bytes.Buffer
	writeServerTestMessage(t, &input, rpcMessage{ID: 1, Method: "prompts/list"})

	var output bytes.Buffer
	if err := Serve(context.Background(), &input, &output, tools.NewRegistry(), ServeOptions{}); err != nil {
		t.Fatalf("Serve() error = %v", err)
	}

	var result struct {
		Prompts []Prompt `json:"prompts"`
	}
	decodeServerTestResult(t, readServerTestMessage(t, newMessageReader(&output)), &result)
	if len(result.Prompts) == 0 {
		t.Fatalf("prompts/list returned no prompts")
	}
	for _, prompt := range result.Prompts {
		if strings.TrimSpace(prompt.Name) == "" {
			t.Fatalf("prompt has empty name: %#v", prompt)
		}
		if strings.TrimSpace(prompt.Description) == "" {
			t.Fatalf("prompt %q has empty description", prompt.Name)
		}
	}
	if _, ok := curatedPrompt("code_review"); !ok {
		t.Fatalf("curated set missing code_review prompt")
	}
}

func TestServePromptsGetSubstitutesArguments(t *testing.T) {
	template, ok := curatedPrompt("code_review")
	if !ok {
		t.Fatalf("code_review prompt not registered")
	}
	if len(template.prompt.Arguments) == 0 {
		t.Fatalf("code_review prompt has no arguments to substitute")
	}
	argName := template.prompt.Arguments[0].Name

	var input bytes.Buffer
	writeServerTestMessage(t, &input, rpcMessage{
		ID:     1,
		Method: "prompts/get",
		Params: mustRaw(map[string]any{
			"name":      "code_review",
			"arguments": map[string]any{argName: "ZERO-SENTINEL-VALUE"},
		}),
	})

	var output bytes.Buffer
	if err := Serve(context.Background(), &input, &output, tools.NewRegistry(), ServeOptions{}); err != nil {
		t.Fatalf("Serve() error = %v", err)
	}

	var result struct {
		Description string          `json:"description"`
		Messages    []PromptMessage `json:"messages"`
	}
	decodeServerTestResult(t, readServerTestMessage(t, newMessageReader(&output)), &result)
	if result.Description == "" {
		t.Fatalf("prompts/get description empty")
	}
	if len(result.Messages) == 0 {
		t.Fatalf("prompts/get returned no messages")
	}

	var rendered strings.Builder
	for _, message := range result.Messages {
		if message.Role == "" {
			t.Fatalf("message has empty role: %#v", message)
		}
		if message.Content.Type != "text" {
			t.Fatalf("message content type = %q, want text", message.Content.Type)
		}
		rendered.WriteString(message.Content.Text)
	}
	if !strings.Contains(rendered.String(), "ZERO-SENTINEL-VALUE") {
		t.Fatalf("rendered prompt did not substitute argument: %q", rendered.String())
	}
	if strings.Contains(rendered.String(), "{{"+argName+"}}") {
		t.Fatalf("rendered prompt still contains placeholder for %q", argName)
	}
}

func TestSubstituteArgsPreservesPlaceholdersInUserInput(t *testing.T) {
	// A supplied value containing {{...}} (a pasted template/CI snippet) must
	// survive verbatim — only TEMPLATE placeholders are replaced.
	out := substituteArgs("Review this: {{diff}}", map[string]string{
		"diff": "run: ${{ secrets.TOKEN }}\nvalue: {{ .Values.x }}",
	})
	if !strings.Contains(out, "${{ secrets.TOKEN }}") || !strings.Contains(out, "{{ .Values.x }}") {
		t.Fatalf("user-supplied {{...}} was mangled: %q", out)
	}
	// An unfilled template placeholder still renders blank (no raw syntax leaks).
	if blank := substituteArgs("a {{missing}} b", map[string]string{}); strings.Contains(blank, "{{") {
		t.Fatalf("unfilled template placeholder leaked raw syntax: %q", blank)
	}
}

func TestServePromptsGetMissingRequiredArgErrors(t *testing.T) {
	// code_review.diff is required; omitting it must error rather than silently
	// rendering a blank, honoring the contract prompts/list advertises.
	var input bytes.Buffer
	writeServerTestMessage(t, &input, rpcMessage{
		ID:     1,
		Method: "prompts/get",
		Params: mustRaw(map[string]any{"name": "code_review", "arguments": map[string]any{}}),
	})

	var output bytes.Buffer
	if err := Serve(context.Background(), &input, &output, tools.NewRegistry(), ServeOptions{}); err != nil {
		t.Fatalf("Serve() error = %v", err)
	}
	message := readServerTestMessage(t, newMessageReader(&output))
	if message.Error == nil {
		t.Fatalf("expected error for missing required argument, got result %s", string(message.Result))
	}
}

func TestServePromptsGetUnknownErrors(t *testing.T) {
	var input bytes.Buffer
	writeServerTestMessage(t, &input, rpcMessage{
		ID:     1,
		Method: "prompts/get",
		Params: mustRaw(map[string]any{"name": "does_not_exist"}),
	})

	var output bytes.Buffer
	if err := Serve(context.Background(), &input, &output, tools.NewRegistry(), ServeOptions{}); err != nil {
		t.Fatalf("Serve() error = %v", err)
	}
	message := readServerTestMessage(t, newMessageReader(&output))
	if message.Error == nil {
		t.Fatalf("expected error for unknown prompt, got result %s", string(message.Result))
	}
}

func TestServePromptsGetMissingNameErrors(t *testing.T) {
	var input bytes.Buffer
	writeServerTestMessage(t, &input, rpcMessage{
		ID:     1,
		Method: "prompts/get",
		Params: mustRaw(map[string]any{}),
	})

	var output bytes.Buffer
	if err := Serve(context.Background(), &input, &output, tools.NewRegistry(), ServeOptions{}); err != nil {
		t.Fatalf("Serve() error = %v", err)
	}
	message := readServerTestMessage(t, newMessageReader(&output))
	if message.Error == nil || message.Error.Code != jsonRPCInvalidParams {
		t.Fatalf("error = %#v, want invalid params", message.Error)
	}
}
