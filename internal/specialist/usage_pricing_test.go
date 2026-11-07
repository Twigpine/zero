package specialist

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/sessions"
	"github.com/Twigpine/zero/internal/streamjson"
)

func intPtr(n int) *int { return &n }

// A CHILD'S TURN MUST REACH ITS PARENT PRICEABLE, not merely counted.
//
// The child writes cachedInputTokens / cacheWriteTokens / reasoningTokens to its
// own session record precisely so a turn can be costed exactly. The stream-json
// event the PARENT reads carried only prompt/completion/total, so every rolled-up
// sub-agent turn was priced as if nothing had been cached.
//
// That is not a rounding error. A measured plan task had 49,280 of 49,894 prompt
// tokens served from cache — 98.8% — and plan tasks are the ideal cache case,
// re-sending one large stable prompt every turn.
func TestSummarizeStreamCarriesTheFieldsThatMakeATurnPriceable(t *testing.T) {
	// Two turns, as a real task produces: one call per turn, each with its own
	// cache split.
	summary := SummarizeStream([]streamjson.Event{
		{
			Type:         streamjson.EventUsage,
			PromptTokens: intPtr(30000), CompletionTokens: intPtr(500), TotalTokens: intPtr(30500),
			CachedInputTokens: intPtr(29000), CacheWriteTokens: intPtr(1000), ReasoningTokens: intPtr(120),
		},
		{
			Type:         streamjson.EventUsage,
			PromptTokens: intPtr(31000), CompletionTokens: intPtr(400), TotalTokens: intPtr(31400),
			CachedInputTokens: intPtr(30500), ReasoningTokens: intPtr(80),
		},
	}, 0)

	// SUMMED, not overwritten — each turn reports its own split.
	if got := summary.Usage.CachedInputTokens; got != 59500 {
		t.Errorf("cached input across turns: want 59500, got %d", got)
	}
	if got := summary.Usage.CacheWriteTokens; got != 1000 {
		t.Errorf("cache writes: want 1000, got %d", got)
	}
	if got := summary.Usage.ReasoningTokens; got != 200 {
		t.Errorf("reasoning: want 200, got %d", got)
	}
	// The pre-existing totals must be untouched by any of this.
	if got := summary.Usage.TotalTokens; got != 61900 {
		t.Errorf("total tokens changed: want 61900, got %d", got)
	}

	// GUARD THE GUARD: with the fields absent the sum must be zero, not garbage —
	// most providers report no cache at all and must stay priceable as uncached.
	bare := SummarizeStream([]streamjson.Event{
		{Type: streamjson.EventUsage, PromptTokens: intPtr(100), CompletionTokens: intPtr(10), TotalTokens: intPtr(110)},
	}, 0)
	if bare.Usage.CachedInputTokens != 0 || bare.Usage.CacheWriteTokens != 0 || bare.Usage.ReasoningTokens != 0 {
		t.Errorf("a provider reporting no cache produced non-zero splits: %+v", bare.Usage)
	}
	if bare.Usage.TotalTokens != 110 {
		t.Errorf("the no-cache case lost its total: %d", bare.Usage.TotalTokens)
	}
}

// The rollup written into the PARENT session is where BuildReport reads from, so
// the fields have to survive that hop too — carrying them up the stream and then
// dropping them at the rollup would fix nothing.
//
// Drives appendSpecialistUsageRollup itself. Re-implementing the payload in the
// test would assert the shape I happened to write rather than the shape the code
// writes, which is how a field gets carried three layers and dropped at the
// fourth without a single test noticing.
func TestTheUsageRollupWritesThePricingFields(t *testing.T) {
	store := sessions.NewStore(sessions.StoreOptions{RootDir: t.TempDir()})
	parent, err := store.Create(sessions.CreateInput{
		SessionID: "zero_00000000000000_0000000000000000000_1",
		Cwd:       t.TempDir(),
	})
	if err != nil {
		t.Fatalf("create parent session: %v", err)
	}
	input := specialistAccountingInput{
		ParentSessionID: parent.SessionID,
		ChildSessionID:  "specialist_00000000000000000000000a",
		SpecialistName:  "explorer",
		Model:           "grok-4.3",
	}
	summary := StreamResult{RunID: "run-1", Usage: StreamUsage{
		PromptTokens: 30000, CompletionTokens: 500, TotalTokens: 30500,
		CachedInputTokens: 29000, CacheWriteTokens: 1000, ReasoningTokens: 120, Events: 1,
	}}

	rolledUp, err := appendSpecialistUsageRollup(store, input, summary)
	if err != nil {
		t.Fatalf("rollup: %v", err)
	}
	if !rolledUp {
		t.Fatal("the rollup did not record anything")
	}

	events, err := store.ReadEvents(parent.SessionID)
	if err != nil {
		t.Fatalf("read parent events: %v", err)
	}
	var usage map[string]any
	for _, event := range events {
		if event.Type != sessions.EventUsage {
			continue
		}
		if err := json.Unmarshal(event.Payload, &usage); err != nil {
			t.Fatalf("decode usage payload: %v", err)
		}
	}
	if usage == nil {
		t.Fatalf("no usage event reached the parent session: %+v", events)
	}
	// These exact keys are what internal/usage reads back to price a turn.
	for key, want := range map[string]float64{
		"cachedInputTokens": 29000, "cacheWriteTokens": 1000, "reasoningTokens": 120,
	} {
		got, ok := usage[key]
		if !ok {
			t.Errorf("the rollup omits %q, so BuildReport prices this turn as uncached", key)
			continue
		}
		if number, ok := got.(float64); !ok || number != want {
			t.Errorf("%s: want %v, got %v", key, want, got)
		}
	}
}

// ROUTING IS NOT FREE and is not part of any task's total, so a plan that does
// not name its cost reports less than it spent — every run, invisibly.
func TestTheRoutingCallReportsWhatItSpent(t *testing.T) {
	notes := []string{"routed by grok-4.5 (4210 tokens)", "a: routed → grok-4.3"}
	summary := autoAssignSummary(notes)
	if !strings.Contains(summary, "4210 tokens") {
		t.Errorf("the router's own spend is invisible in the plan's report:\n%s", summary)
	}
}

// SPEND SURVIVES FAILURE, in the session record as well as in the plan.
//
// The error branch of Executor.Run passed a hard-coded false for usageRolledUp
// and appended no usage event at all, so a child killed mid-run had every token
// it had already spent vanish from its parent's record — `zero usage` under-
// reported by exactly that much. The plan's own budget was already correct;
// this is the other ledger.
func TestAChildThatFailedStillRollsUpWhatItSpent(t *testing.T) {
	store := sessions.NewStore(sessions.StoreOptions{RootDir: t.TempDir()})
	parent, err := store.Create(sessions.CreateInput{
		SessionID: "zero_00000000000000_0000000000000001111_1",
		Cwd:       t.TempDir(),
	})
	if err != nil {
		t.Fatalf("create parent: %v", err)
	}

	spent := 4200
	exec := Executor{
		BinaryPath:   "/bin/true",
		SessionStore: store,
		NewSessionID: func() (string, error) { return "specialist_0000000000000000000000ff", nil },
		Load: func(LoadOptions) (LoadResult, error) {
			return LoadResult{Specialists: []Manifest{resumeTestManifest()}}, nil
		},
		RunChild: func(_ context.Context, _ string, _ []string, _ func(streamjson.Event)) (ChildRunResult, error) {
			// Spent real tokens, then died — a kill, an OOM, a broken pipe.
			return ChildRunResult{
				Started:  true,
				ExitCode: -1,
				Events: []streamjson.Event{{
					Type: streamjson.EventUsage, TotalTokens: &spent,
					PromptTokens: intPtr(4000), CompletionTokens: intPtr(200),
				}},
			}, errors.New("the child was killed")
		},
	}

	result, runErr := exec.Run(context.Background(),
		TaskParameters{Name: "explorer", Prompt: "work"},
		TaskRunOptions{Cwd: t.TempDir(), ParentSessionID: parent.SessionID})
	if runErr == nil {
		t.Fatal("setup: this test needs the failing branch")
	}
	// Already true before this fix, and asserted so it stays true.
	if result.TotalTokens != spent {
		t.Errorf("the plan's own accounting lost the tokens: want %d, got %d", spent, result.TotalTokens)
	}

	events, err := store.ReadEvents(parent.SessionID)
	if err != nil {
		t.Fatalf("read parent events: %v", err)
	}
	found := false
	for _, event := range events {
		if event.Type == sessions.EventUsage {
			found = true
		}
	}
	if !found {
		t.Errorf("a failed child's spend never reached the parent's session record: %d events", len(events))
	}
}
