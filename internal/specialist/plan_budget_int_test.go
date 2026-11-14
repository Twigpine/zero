package specialist

import (
	"context"
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/Twigpine/zero/internal/tools"
)

// budgetIntCase is one integer budget field: a value admission accepts, the
// Budget field it lands in, and the smallest fraction that the old unchecked
// int() conversion turned into a DIFFERENT, still-admitted budget.
type budgetIntCase struct {
	field    string
	valid    int
	fraction float64
	read     func(Budget) int
}

func budgetIntCases() []budgetIntCase {
	return []budgetIntCase{
		{"max_workers", 4, 1.9, func(b Budget) int { return b.MaxWorkers }},
		{"max_tokens", 2_000_000, 500_000.5, func(b Budget) int { return b.MaxTokens }},
		{"max_tokens_per_task", 100_000, 100_000.5, func(b Budget) int { return b.MaxTokensPerTask }},
		// 0.9 truncated to 0, which DISABLES the wall bound.
		{"max_wall_seconds", 600, 0.9, func(b Budget) int { return int(b.MaxWall / time.Second) }},
		{"max_stall_seconds", 60, 60.5, func(b Budget) int { return int(b.MaxStall / time.Second) }},
		// 0.9 truncated to an EXPLICIT zero: no retries, against the default.
		{"max_retries", 2, 0.9, func(b Budget) int { return b.MaxRetries }},
	}
}

func budgetWith(field string, value any) map[string]any {
	budget := map[string]any{"max_workers": float64(1)}
	budget[field] = value
	return budget
}

// A PRESENT VALUE THAT IS NOT A WHOLE NUMBER IS REFUSED, for every integer
// field — never truncated to a different budget, never read as absent.
func TestEveryIntegerBudgetFieldRefusesANonWholeValue(t *testing.T) {
	for _, tc := range budgetIntCases() {
		for name, value := range map[string]any{
			"fraction":           tc.fraction,
			"fractional string":  "12.5",
			"non-numeric string": "lots",
			"null":               nil,
			"bool":               true,
			"object":             map[string]any{"n": float64(1)},
			"NaN":                math.NaN(),
			"+Inf":               math.Inf(1),
			"2^63":               math.Pow(2, 63),
		} {
			t.Run(tc.field+"/"+name, func(t *testing.T) {
				plan, err := ParsePlan(planArgs([]any{task("a", "x")}, budgetWith(tc.field, value)), readOnlyLimits())
				if err == nil {
					t.Fatalf("%s=%v was admitted as %d", tc.field, value, tc.read(plan.Budget()))
				}
				if !strings.Contains(err.Error(), "budget."+tc.field) {
					t.Errorf("the refusal does not name budget.%s: %v", tc.field, err)
				}
			})
		}
	}
}

// A WHOLE NUMBER IS ACCEPTED IN EVERY SPELLING a provider sends it in — a JSON
// number, a whole float, or a numeric string (the repo's intArg rule) — and a
// saved plan's Args() carries the bound forward instead of losing it.
func TestEveryIntegerBudgetFieldAcceptsAWholeNumberAndKeepsItInArgs(t *testing.T) {
	for _, tc := range budgetIntCases() {
		for name, value := range map[string]any{
			"number":          float64(tc.valid),
			"int":             tc.valid,
			"string":          " " + itoa(tc.valid) + " ",
			"whole float str": itoa(tc.valid) + ".0",
		} {
			t.Run(tc.field+"/"+name, func(t *testing.T) {
				plan, err := ParsePlan(planArgs([]any{task("a", "x")}, budgetWith(tc.field, value)), readOnlyLimits())
				if err != nil {
					t.Fatalf("%s=%#v was refused: %v", tc.field, value, err)
				}
				if got := tc.read(plan.Budget()); got != tc.valid {
					t.Fatalf("%s=%#v decoded to %d, want %d", tc.field, value, got, tc.valid)
				}
				// The saved form: Args() through JSON and back through ParsePlan.
				encoded, err := json.Marshal(plan.Args())
				if err != nil {
					t.Fatal(err)
				}
				var decoded map[string]any
				if err := json.Unmarshal(encoded, &decoded); err != nil {
					t.Fatal(err)
				}
				restored, err := ParsePlan(decoded, readOnlyLimits())
				if err != nil {
					t.Fatalf("the saved plan did not re-admit: %v", err)
				}
				if got := tc.read(restored.Budget()); got != tc.valid {
					t.Errorf("the saved plan lost %s: %d, want %d", tc.field, got, tc.valid)
				}
			})
		}
	}
}

func itoa(n int) string {
	encoded, _ := json.Marshal(n)
	return string(encoded)
}

// ABSENT AND EXPLICIT ZERO KEEP THEIR SEPARATE MEANINGS through the new decoder:
// an absent max_retries is the default, an explicit "0" is no retries, and a
// string zero for a timeout is still "no bound" exactly like a numeric zero.
func TestIntegerBudgetDecodingKeepsAbsentDistinctFromZero(t *testing.T) {
	absent, err := ParsePlan(planArgs([]any{task("a", "x")}, map[string]any{"max_workers": "1"}), readOnlyLimits())
	if err != nil {
		t.Fatalf("absent fields were refused: %v", err)
	}
	if got := absent.Budget().MaxRetries; got != defaultPlanRetries {
		t.Errorf("absent max_retries = %d, want the default %d", got, defaultPlanRetries)
	}
	zero, err := ParsePlan(planArgs([]any{task("a", "x")}, map[string]any{
		"max_workers": "1", "max_retries": "0", "max_wall_seconds": "0", "max_stall_seconds": float64(0),
	}), readOnlyLimits())
	if err != nil {
		t.Fatalf("explicit zeros were refused: %v", err)
	}
	if got := zero.Budget(); got.MaxRetries != 0 || got.MaxWall != 0 || got.MaxStall != 0 {
		t.Errorf("explicit zeros = %+v, want retries 0 and no timeouts", got)
	}
}

// END TO END: A STRING BOUND REACHES THE RUNNER. Through the tool a model
// drives, max_tokens_per_task "100000" used to arrive as MaxTaskTokens 0 —
// which the request struct defines as unbounded — with no error anywhere.
func TestAStringPerTaskCapReachesTheRunner(t *testing.T) {
	var got []int
	tool := &OrchestrateTool{
		PostureActive: func() bool { return true },
		ParentTools:   []string{"read_file"},
		RunTask: func(_ context.Context, req PlanTaskRequest) (TaskResult, error) {
			got = append(got, req.MaxTaskTokens)
			return TaskResult{Outcome: TaskSucceeded, Output: "ok"}, nil
		},
	}
	result := tool.RunWithOptions(context.Background(), map[string]any{
		"name":   "p",
		"budget": map[string]any{"max_workers": "1", "max_tokens_per_task": "100000"},
		"tasks":  []any{map[string]any{"id": "a", "prompt": "x"}},
	}, tools.RunOptions{Model: "m"})
	if result.Status == tools.StatusError {
		t.Fatalf("plan refused: %s", result.Output)
	}
	if len(got) != 1 || got[0] != 100_000 {
		t.Fatalf("the runner saw MaxTaskTokens %v, want [100000]", got)
	}
}
