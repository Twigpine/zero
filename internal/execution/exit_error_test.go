package execution

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"testing"
)

func TestAsPureExitError(t *testing.T) {
	first := &exec.ExitError{}
	second := &exec.ExitError{}
	var nilExit *exec.ExitError
	tests := []struct {
		name string
		err  error
		want *exec.ExitError
		ok   bool
	}{
		{name: "nil"},
		{name: "direct", err: first, want: first, ok: true},
		{name: "joined", err: errors.Join(first, second), want: first, ok: true},
		{name: "nested joins", err: errors.Join(errors.Join(first, second), &exec.ExitError{}), want: first, ok: true},
		{name: "join with nil", err: errors.Join(first, nil), want: first, ok: true},
		{name: "ordinary error", err: errors.New("start failed")},
		{name: "mixed join", err: errors.Join(first, context.Canceled)},
		{name: "nested mixed join", err: errors.Join(first, errors.Join(second, context.DeadlineExceeded))},
		{name: "wrapped exit", err: fmt.Errorf("cleanup failed: %w", first)},
		{name: "join containing wrapped exit", err: errors.Join(first, fmt.Errorf("wrapped: %w", second))},
		{name: "typed nil exit", err: nilExit},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, ok := AsPureExitError(test.err)
			if got != test.want || ok != test.ok {
				t.Fatalf("AsPureExitError(%v) = (%p, %v), want (%p, %v)", test.err, got, ok, test.want, test.ok)
			}
		})
	}
}
