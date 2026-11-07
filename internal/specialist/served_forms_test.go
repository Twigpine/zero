package specialist

import (
	"context"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/tools"
)

// A MODEL SPELLED DIFFERENTLY IS THE SAME MODEL, and the provider-mismatch guard
// must not read a spelling difference as a different provider.
//
// The served map held raw discovery ids while the guard probed it with the
// SESSION's model. A session on "sonnet 4.5" against a provider listing
// "claude-sonnet-4.5" missed, and a miss reads as "this list belongs to a
// different provider" — so auto-assignment silently switched off, or refused the
// plan outright when it had been asked for explicitly. Reproduced before the fix
// on both an alias and an Ollama ":latest" tag.
func TestAnAliasNamedSessionModelIsNotMistakenForAnotherProvider(t *testing.T) {
	for name, fixture := range map[string]struct {
		session    string
		discovered []string
	}{
		"registry alias": {"sonnet 4.5", []string{"claude-sonnet-4.5", "claude-haiku-4.5", "claude-opus-4.1"}},
		"ollama tag":     {"glm-5.2", []string{"glm-5.2:latest", "kimi-k2.6:latest", "qwen3.5:397b"}},
		"tagged session": {"glm-5.2:latest", []string{"glm-5.2", "kimi-k2.6", "qwen3.5:397b"}},
	} {
		t.Run(name, func(t *testing.T) {
			models := make([]DiscoveredModel, 0, len(fixture.discovered))
			for index, id := range fixture.discovered {
				models = append(models, DiscoveredModel{ID: id, ToolCall: true, InputCost: float64(index + 1)})
			}
			tool := &OrchestrateTool{
				DiscoverModels: func(context.Context) ([]DiscoveredModel, error) { return models, nil },
				ModelPrefs:     ModelPreferences{AutoAssign: true},
			}
			args := map[string]any{"tasks": []any{
				map[string]any{"id": "a", "prompt": "list the files"},
				map[string]any{"id": "b", "prompt": "audit it and judge whether it holds"},
			}}

			notes, err := tool.autoAssignModels(context.Background(), args, tools.RunOptions{Model: fixture.session})
			if err != nil {
				t.Fatalf("a spelling difference refused the whole plan: %v", err)
			}
			if joined := strings.Join(notes, " "); strings.Contains(joined, "different provider") {
				t.Fatalf("the mismatch guard fired on the right provider: %s", joined)
			}
			assigned := 0
			for _, entry := range args["tasks"].([]any) {
				if strings.TrimSpace(planString(entry.(map[string]any), "model")) != "" {
					assigned++
				}
			}
			if assigned == 0 {
				t.Errorf("auto-assignment silently switched itself off: %v", notes)
			}
		})
	}
}

// THE GUARD MUST STILL FIRE ON A GENUINELY FOREIGN LIST. Widening the comparison
// to every spelling is worthless if it also stops catching the case it was
// written for.
func TestAGenuinelyForeignModelListStillTripsTheGuard(t *testing.T) {
	tool := &OrchestrateTool{
		DiscoverModels: func(context.Context) ([]DiscoveredModel, error) {
			return []DiscoveredModel{
				{ID: "deepseek-v4-flash", ToolCall: true, InputCost: 1},
				{ID: "qwen3.5:397b", ToolCall: true, InputCost: 9},
			}, nil
		},
		ModelPrefs: ModelPreferences{AutoAssign: true},
	}
	args := map[string]any{"tasks": []any{map[string]any{"id": "a", "prompt": "list"}}}
	notes, _ := tool.autoAssignModels(context.Background(), args, tools.RunOptions{Model: "grok-4.5"})
	if !strings.Contains(strings.Join(notes, " "), "different provider") {
		t.Errorf("an Ollama list on an xAI session was accepted: %v", notes)
	}
}

// A pin written in either spelling must survive, for the same reason.
func TestAPinIsHonouredWhicheverSpellingItUses(t *testing.T) {
	served := servedModels([]DiscoveredModel{{ID: "claude-sonnet-4.5"}, {ID: "glm-5.2:latest"}})
	for _, pin := range []string{"claude-sonnet-4.5", "sonnet 4.5", "glm-5.2", "glm-5.2:latest"} {
		if !servedContains(served, pin) {
			t.Errorf("pin %q was treated as unserved", pin)
		}
	}
	if servedContains(served, "grok-4.5") {
		t.Error("an unserved model was accepted")
	}
}
