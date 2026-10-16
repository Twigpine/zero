package tools

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
)

// countingCancelContext returns context.Canceled from Err() once it has been
// called more than "remaining" times, letting a test deterministically
// cancel a loop after a fixed number of iterations instead of relying on
// timing. A context.WithTimeout test would be flaky under a loaded or
// shared CI runner, exactly the kind of test PR #464 was criticized for
// elsewhere (TestCollectRespectsDeadlineUnderContinuousOutput).
type countingCancelContext struct {
	context.Context
	remaining int
}

func (c *countingCancelContext) Err() error {
	c.remaining--
	if c.remaining < 0 {
		return context.Canceled
	}
	return nil
}

// buildLargeSearchTree creates n files, each containing a grep-matchable
// line, so a walk that does NOT respect cancellation would have plenty of
// work left to do (and matches to find) after the very first entry.
func buildLargeSearchTree(t *testing.T, n int) string {
	t.Helper()
	root := t.TempDir()
	for i := 0; i < n; i++ {
		path := filepath.Join(root, "file"+strconv.Itoa(i)+".txt")
		if err := os.WriteFile(path, []byte("needle\n"), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}
	return root
}

// A cancelled run must stop the filesystem walk promptly instead of visiting
// every remaining entry to completion. Before this fix, grep's Run/
// RunWithSandbox discarded their context entirely, so cancelling the run
// (Ctrl+C / /exit) had no effect on an in-flight, unscoped search: the walk
// ran to completion regardless, and — because the TUI's exit path waits for
// the cancelled run's own response before it can quit — the whole
// application was stuck until the walk finished on its own.
func TestGrepStopsWalkOnCancelledContext(t *testing.T) {
	root := buildLargeSearchTree(t, 500)
	tool := NewScopedGrepTool(root, nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled before the walk starts

	result := tool.Run(ctx, map[string]any{"pattern": "needle"})
	if result.Status != StatusError {
		t.Fatalf("status = %q, want error", result.Status)
	}
	if result.Output != "Error: grep cancelled." {
		t.Fatalf("output = %q, want the cancellation message", result.Output)
	}
}

func TestGrepRunWithSandboxStopsWalkOnCancelledContext(t *testing.T) {
	root := buildLargeSearchTree(t, 500)
	tool := NewScopedGrepTool(root, nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	sandboxed := tool.(sandboxAwareTool)
	result := sandboxed.RunWithSandbox(ctx, map[string]any{"pattern": "needle"}, nil)
	if result.Status != StatusError {
		t.Fatalf("status = %q, want error", result.Status)
	}
	if result.Output != "Error: grep cancelled." {
		t.Fatalf("output = %q, want the cancellation message", result.Output)
	}
}

// grep with a live (non-cancelled) context must still work normally: this
// fix only short-circuits on cancellation, it does not change matching.
func TestGrepStillMatchesWithLiveContext(t *testing.T) {
	root := buildLargeSearchTree(t, 3)
	tool := NewScopedGrepTool(root, nil)

	result := tool.Run(context.Background(), map[string]any{"pattern": "needle"})
	if result.Status != StatusOK {
		t.Fatalf("status = %q, want ok; output=%q", result.Status, result.Output)
	}
}

// Before this fix, only grep's walk checked ctx: once the walk had discovered
// candidate files, match scanning could run to completion regardless of
// cancellation. These tests cover both phases directly.
// The walker must stop mid-walk once ctx is cancelled, not only when the
// context was already cancelled before WalkDir starts.
func TestGrepWalkStopsMidWalkOnCancelledContext(t *testing.T) {
	root := buildLargeSearchTree(t, 50)
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}

	const allowed = 5
	ctx := &countingCancelContext{Context: context.Background(), remaining: allowed}
	files := 0
	err = walkGrepFiles(ctx, resolvedRoot, root, nil, readExcluder{}, func(string) error {
		files++
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if files >= 50 {
		t.Fatalf("files = %d; walk should stop mid-traversal instead of visiting every entry", files)
	}
}

func TestGrepScanStopsMidFileOnCancelledContext(t *testing.T) {
	root := t.TempDir()
	var body string
	for i := 0; i < 50; i++ {
		body += "needle\n"
	}
	file := filepath.Join(root, "file.txt")
	writeTestFile(t, file, body)
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	compiled := regexp.MustCompile("needle")

	const allowed = 5
	ctx := &countingCancelContext{Context: context.Background(), remaining: allowed}
	matches := 0
	err = scanGrepFile(ctx, resolvedRoot, false, file, readExcluder{}, presenceGrepLineMatcher(compiled), func(grepMatch) bool {
		matches++
		return true
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if matches != allowed {
		t.Fatalf("matches = %d, want exactly %d collected before cancellation stopped the loop", matches, allowed)
	}
}

func TestScanGlobStopsMidWalkOnCancelledContext(t *testing.T) {
	root := buildLargeSearchTree(t, 50)
	matcher, err := compileGlob("**/*.txt")
	if err != nil {
		t.Fatalf("compileGlob: %v", err)
	}

	const allowed = 5
	ctx := &countingCancelContext{Context: context.Background(), remaining: allowed}
	matches, err := scanGlob(ctx, root, root, matcher, false, readExcluder{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if len(matches) >= 50 {
		t.Fatalf("matches = %d; walk should stop mid-traversal instead of visiting every entry", len(matches))
	}
}

func TestGlobStopsWalkOnCancelledContext(t *testing.T) {
	root := buildLargeSearchTree(t, 500)
	tool := NewScopedGlobTool(root, nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result := tool.Run(ctx, map[string]any{"pattern": "**/*.txt"})
	if result.Status != StatusError {
		t.Fatalf("status = %q, want error", result.Status)
	}
	if result.Output != "Error: glob cancelled." {
		t.Fatalf("output = %q, want the cancellation message", result.Output)
	}
}

func TestGlobRunWithSandboxStopsWalkOnCancelledContext(t *testing.T) {
	root := buildLargeSearchTree(t, 500)
	tool := NewScopedGlobTool(root, nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	sandboxed := tool.(sandboxAwareTool)
	result := sandboxed.RunWithSandbox(ctx, map[string]any{"pattern": "**/*.txt"}, nil)
	if result.Status != StatusError {
		t.Fatalf("status = %q, want error", result.Status)
	}
	if result.Output != "Error: glob cancelled." {
		t.Fatalf("output = %q, want the cancellation message", result.Output)
	}
}

func TestGlobStillMatchesWithLiveContext(t *testing.T) {
	root := buildLargeSearchTree(t, 3)
	tool := NewScopedGlobTool(root, nil)

	result := tool.Run(context.Background(), map[string]any{"pattern": "**/*.txt"})
	if result.Status != StatusOK {
		t.Fatalf("status = %q, want ok; output=%q", result.Status, result.Output)
	}
}
