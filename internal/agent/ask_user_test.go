package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/tools"
	"github.com/Twigpine/zero/internal/zeroruntime"
)

// providerCallingAskUserThenAnswer returns a mock provider whose first turn calls
// the ask_user tool and whose second turn returns a plain-text final answer.
func providerCallingAskUserThenAnswer(arguments string, answer string) *mockProvider {
	return &mockProvider{
		turns: [][]zeroruntime.StreamEvent{
			{
				{Type: zeroruntime.StreamEventToolCallStart, ToolCallID: "call-1", ToolName: "ask_user"},
				{Type: zeroruntime.StreamEventToolCallDelta, ToolCallID: "call-1", ArgumentsFragment: arguments},
				{Type: zeroruntime.StreamEventToolCallEnd, ToolCallID: "call-1"},
				{Type: zeroruntime.StreamEventDone},
			},
			{
				{Type: zeroruntime.StreamEventText, Content: answer},
				{Type: zeroruntime.StreamEventDone},
			},
		},
	}
}

func registryWithAskUser() *tools.Registry {
	registry := tools.NewRegistry()
	registry.Register(tools.NewAskUserTool())
	return registry
}

func TestRunAskUserReturnsHandlerAnswers(t *testing.T) {
	registry := registryWithAskUser()
	args := `{"questions":[{"question":"Which framework?","options":["React","Vue"]},{"question":"TypeScript?"}]}`
	provider := providerCallingAskUserThenAnswer(args, "thanks")

	var requests []AskUserRequest
	result, err := Run(context.Background(), "clarify", provider, Options{
		Registry: registry,
		OnAskUser: func(_ context.Context, request AskUserRequest) (AskUserResponse, error) {
			requests = append(requests, request)
			return AskUserResponse{Answers: []string{"React", "yes"}}, nil
		},
	})

	if err != nil {
		t.Fatal(err)
	}
	if result.FinalAnswer != "thanks" {
		t.Fatalf("expected final answer from second turn, got %q", result.FinalAnswer)
	}
	if len(requests) != 1 {
		t.Fatalf("expected one ask_user request, got %#v", requests)
	}
	request := requests[0]
	if len(request.Questions) != 2 {
		t.Fatalf("expected two parsed questions, got %#v", request.Questions)
	}
	if request.Questions[0].Question != "Which framework?" || len(request.Questions[0].Options) != 2 {
		t.Fatalf("unexpected first question: %#v", request.Questions[0])
	}
	if request.Questions[1].Question != "TypeScript?" {
		t.Fatalf("unexpected second question: %#v", request.Questions[1])
	}

	// The answers must be threaded back to the model as the tool result.
	toolMessage := provider.requests[1].Messages[len(provider.requests[1].Messages)-1]
	if toolMessage.Role != zeroruntime.MessageRoleTool || toolMessage.ToolCallID != "call-1" {
		t.Fatalf("expected tool result message for call-1, got %#v", toolMessage)
	}
	for _, want := range []string{"Which framework?", "React", "TypeScript?", "yes"} {
		if !strings.Contains(toolMessage.Content, want) {
			t.Fatalf("expected tool result to contain %q, got %q", want, toolMessage.Content)
		}
	}
}

func TestRunAskUserWithoutHandlerDegradesGracefully(t *testing.T) {
	registry := registryWithAskUser()
	args := `{"questions":[{"question":"Which framework?"}]}`
	provider := providerCallingAskUserThenAnswer(args, "proceeding with React")

	// No OnAskUser handler: the loop must NOT hang, and must hand the model a
	// result telling it to proceed with its best assumption.
	result, err := Run(context.Background(), "clarify", provider, Options{
		Registry: registry,
	})

	if err != nil {
		t.Fatal(err)
	}
	if result.FinalAnswer != "proceeding with React" {
		t.Fatalf("expected loop to continue to a final answer, got %q", result.FinalAnswer)
	}
	if len(provider.requests) != 2 {
		t.Fatalf("expected the loop to continue (2 turns), got %d", len(provider.requests))
	}
	toolMessage := provider.requests[1].Messages[len(provider.requests[1].Messages)-1]
	if toolMessage.ToolCallID != "call-1" {
		t.Fatalf("expected tool result for call-1, got %#v", toolMessage)
	}
	if !strings.Contains(strings.ToLower(toolMessage.Content), "no interactive user") {
		t.Fatalf("expected no-interactive-user message, got %q", toolMessage.Content)
	}
	if !strings.Contains(strings.ToLower(toolMessage.Content), "assumption") {
		t.Fatalf("expected guidance to proceed with assumptions, got %q", toolMessage.Content)
	}
}

func TestRunAskUserHandlerErrorDegradesGracefully(t *testing.T) {
	registry := registryWithAskUser()
	args := `{"questions":[{"question":"Which framework?"}]}`
	provider := providerCallingAskUserThenAnswer(args, "done")

	result, err := Run(context.Background(), "clarify", provider, Options{
		Registry: registry,
		OnAskUser: func(_ context.Context, _ AskUserRequest) (AskUserResponse, error) {
			return AskUserResponse{}, errors.New("user cancelled")
		},
	})

	if err != nil {
		t.Fatal(err)
	}
	if result.FinalAnswer != "done" {
		t.Fatalf("expected loop to continue after handler error, got %q", result.FinalAnswer)
	}
	toolMessage := provider.requests[1].Messages[len(provider.requests[1].Messages)-1]
	if !strings.Contains(strings.ToLower(toolMessage.Content), "no interactive user") {
		t.Fatalf("expected graceful degradation message after handler error, got %q", toolMessage.Content)
	}
}

func TestRunAskUserCancellationAbortsRun(t *testing.T) {
	registry := registryWithAskUser()
	args := `{"questions":[{"question":"Which framework?"}]}`
	provider := providerCallingAskUserThenAnswer(args, "done")
	var toolResults []ToolResult

	result, err := Run(context.Background(), "clarify", provider, Options{
		Registry:     registry,
		OnToolResult: func(result ToolResult) { toolResults = append(toolResults, result) },
		OnAskUser: func(_ context.Context, _ AskUserRequest) (AskUserResponse, error) {
			return AskUserResponse{}, context.Canceled
		},
	})

	// A canceled prompt must abort with that error — NOT fabricate a headless
	// answer and run on to a final answer.
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected run to abort with context.Canceled, got err=%v answer=%q", err, result.FinalAnswer)
	}
	// The model must not be asked for a follow-up turn after cancellation.
	if len(provider.requests) != 1 {
		t.Fatalf("expected the run to stop after the canceled ask_user (1 turn), got %d", len(provider.requests))
	}
	if len(toolResults) != 1 || toolResults[0].ToolCallID == "" {
		t.Fatalf("cancellation must emit exactly one result for the call, got %#v", toolResults)
	}
	// The recorded tool result must reflect cancellation, not a synthetic answer.
	var toolMsg string
	for _, m := range result.Messages {
		if m.Role == zeroruntime.MessageRoleTool {
			toolMsg = m.Content
		}
	}
	if !strings.Contains(strings.ToLower(toolMsg), "canceled") {
		t.Fatalf("expected a canceled tool result, got %q", toolMsg)
	}
	if strings.Contains(strings.ToLower(toolMsg), "no interactive user") {
		t.Fatalf("must not fabricate a headless answer on cancellation, got %q", toolMsg)
	}
}

func TestRunAskUserRedactsSecretsInAnswers(t *testing.T) {
	registry := registryWithAskUser()
	secret := "sk-ant-api03-ABCDEFGHIJKLMNOP1234567890"
	args := `{"questions":[{"question":"Paste your key"}]}`
	provider := providerCallingAskUserThenAnswer(args, "done")

	var captured ToolResult
	_, err := Run(context.Background(), "clarify", provider, Options{
		Registry:     registry,
		OnToolResult: func(r ToolResult) { captured = r },
		OnAskUser: func(_ context.Context, _ AskUserRequest) (AskUserResponse, error) {
			return AskUserResponse{Answers: []string{"my key is " + secret}}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(captured.Output, secret) {
		t.Fatalf("secret leaked into ask_user tool result: %q", captured.Output)
	}
	if !captured.Redacted {
		t.Error("expected Redacted=true when a secret was scrubbed from an ask_user answer")
	}
	// The redacted answer must also not reach the model.
	for _, m := range provider.requests[1].Messages {
		if strings.Contains(m.Content, secret) {
			t.Fatalf("secret leaked into model message: %q", m.Content)
		}
	}
}

func TestRunAskUserRejectsMissingQuestions(t *testing.T) {
	registry := registryWithAskUser()
	provider := providerCallingAskUserThenAnswer(`{"questions":[]}`, "done")

	result, err := Run(context.Background(), "clarify", provider, Options{
		Registry: registry,
		OnAskUser: func(_ context.Context, _ AskUserRequest) (AskUserResponse, error) {
			t.Fatal("handler should not be invoked when questions are missing")
			return AskUserResponse{}, nil
		},
	})

	if err != nil {
		t.Fatal(err)
	}
	if result.FinalAnswer != "done" {
		t.Fatalf("expected loop to continue, got %q", result.FinalAnswer)
	}
	toolMessage := provider.requests[1].Messages[len(provider.requests[1].Messages)-1]
	if !strings.Contains(strings.ToLower(toolMessage.Content), "at least one question") {
		t.Fatalf("expected invalid-arguments message, got %q", toolMessage.Content)
	}
}
