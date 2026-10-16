// Package testutil provides small, shared helpers for tests across the
// codebase.
package testutil

import (
	"testing"
	"time"
)

// WaitFor polls cond until it returns true, or fails the test if it does not
// become true within a generous deadline. what names the condition being
// waited for so a timeout failure message is actionable.
func WaitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
