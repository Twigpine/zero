//go:build windows

package execution

import (
	"errors"
	"strings"
	"testing"
)

func TestKillProcessTreeRetainsTreeErrorWhenRootFallbackSucceeds(t *testing.T) {
	treeErr := errors.New("taskkill failed")
	err := killProcessTree(42, func(int) error { return treeErr }, func(int) error { return nil })
	if !errors.Is(err, treeErr) {
		t.Fatalf("killProcessTree error = %v, want tree error", err)
	}
	if !strings.Contains(err.Error(), "root process killed directly") {
		t.Fatalf("killProcessTree error = %q, want fallback context", err)
	}
}

func TestKillProcessTreeJoinsTreeAndRootErrors(t *testing.T) {
	treeErr := errors.New("taskkill failed")
	rootErr := errors.New("root kill failed")
	err := killProcessTree(42, func(int) error { return treeErr }, func(int) error { return rootErr })
	if !errors.Is(err, treeErr) || !errors.Is(err, rootErr) {
		t.Fatalf("killProcessTree error = %v, want both failures", err)
	}
}
