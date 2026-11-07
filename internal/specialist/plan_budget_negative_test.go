package specialist

import (
	"strings"
	"testing"
)

// planInt returns 0 for both an absent key and a present-but-negative one, so a
// bare `seconds > 0` read a model-supplied -60 as "unset" and ran the plan
// unbounded with no error. Every other numeric in the budget refuses a
// negative; a timeout the caller asked for and silently did not get is the
// worst version of that inconsistency.
func TestBudgetRejectsNegativeTimeouts(t *testing.T) {
	for field, value := range map[string]float64{
		"max_wall_seconds":  -60,
		"max_stall_seconds": -60,
	} {
		t.Run(field, func(t *testing.T) {
			_, err := ParsePlan(map[string]any{
				"name":   "p",
				"tasks":  []any{map[string]any{"id": "a", "prompt": "x"}},
				"budget": map[string]any{"max_workers": float64(1), field: value},
			}, Limits{MaxTasks: 20, ParentTools: []string{"read_file"}})
			if err == nil {
				t.Fatalf("a negative %s was accepted and silently ignored", field)
			}
			if !strings.Contains(err.Error(), field) {
				t.Errorf("error should name %s, got %v", field, err)
			}
		})
	}
}

// Absent and zero must keep their existing MEANINGS — the fix must not turn
// "unset" into an error, and it must not turn it into a bound either.
//
// Asserting only that ParsePlan returned no error would pass just as happily if
// admission started assigning some non-zero timeout to a plan that asked for
// none: the plan would be accepted and then killed by a clock nobody set. Both
// spellings of "no timeout" have to survive as zero, which is what every reader
// downstream treats as unbounded.
func TestBudgetStillAcceptsAbsentAndZeroTimeouts(t *testing.T) {
	for name, budget := range map[string]map[string]any{
		"absent": {"max_workers": float64(1)},
		"zero":   {"max_workers": float64(1), "max_wall_seconds": float64(0), "max_stall_seconds": float64(0)},
	} {
		t.Run(name, func(t *testing.T) {
			plan, err := ParsePlan(map[string]any{
				"name":   "p",
				"tasks":  []any{map[string]any{"id": "a", "prompt": "x"}},
				"budget": budget,
			}, Limits{MaxTasks: 20, ParentTools: []string{"read_file"}})
			if err != nil {
				t.Fatalf("%s timeouts should parse: %v", name, err)
			}
			if got := plan.Budget().MaxWall; got != 0 {
				t.Errorf("MaxWall = %v for %s timeouts; unset must stay unset or the plan is bounded by a clock nobody asked for", got, name)
			}
			if got := plan.Budget().MaxStall; got != 0 {
				t.Errorf("MaxStall = %v for %s timeouts; unset must stay unset", got, name)
			}
		})
	}
}

// A TIMEOUT TOO LARGE TO EXPRESS MUST BE REFUSED, because wrapping fails OPEN.
//
// seconds * time.Second is int64 nanoseconds, so past roughly 292 years the
// multiply wraps and comes back NEGATIVE. A negative wall budget does not read
// as "a very long time" anywhere — planWallBudget's caller only acts when the
// value is positive, so the plan that asked for the longest possible timeout was
// given no timeout at all. Every other unusable value in this file is rejected;
// this one was accepted and inverted.
func TestABudgetTimeoutTooLargeToRepresentIsRefused(t *testing.T) {
	for _, field := range []string{"max_wall_seconds", "max_stall_seconds"} {
		t.Run(field, func(t *testing.T) {
			// Comfortably past maxRepresentableSeconds, and the shape a model
			// writes when it means "do not stop me".
			budget := okBudget()
			budget[field] = float64(1e18)

			_, err := ParsePlan(map[string]any{
				"tasks":  []any{task("a", "x")},
				"budget": budget,
			}, readOnlyLimits())
			if err == nil {
				t.Fatalf("%s = 1e18 was admitted; it wraps to a negative duration, which reads as no bound", field)
			}
			if !strings.Contains(err.Error(), field) {
				t.Fatalf("the refusal does not name the field the caller has to fix: %v", err)
			}
		})
	}
}

// ...and the largest value that DOES fit is still accepted, so the guard bounds
// only what it must.
func TestTheLargestRepresentableWallBudgetIsStillAccepted(t *testing.T) {
	budget := okBudget()
	budget["max_wall_seconds"] = float64(maxRepresentableSeconds)

	plan, err := ParsePlan(map[string]any{
		"tasks":  []any{task("a", "x")},
		"budget": budget,
	}, readOnlyLimits())
	if err != nil {
		t.Fatalf("the largest representable wall budget was refused: %v", err)
	}
	if got := plan.Budget().MaxWall; got <= 0 {
		t.Fatalf("MaxWall = %v; a value that fits must stay positive", got)
	}
}
