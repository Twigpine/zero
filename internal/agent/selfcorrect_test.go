package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/lsp"
	"github.com/Twigpine/zero/internal/verify"
)

type fakeVerifier struct {
	reports []verify.Report
	calls   int
}

func (f *fakeVerifier) Verify(context.Context) (verify.Report, error) {
	report := verify.Report{OK: true}
	switch {
	case f.calls < len(f.reports):
		report = f.reports[f.calls]
	case len(f.reports) > 0:
		report = f.reports[len(f.reports)-1]
	}
	f.calls++
	return report, nil
}

type fakeChecker struct {
	byPath map[string][]lsp.Diagnostic
}

func (f fakeChecker) Check(_ context.Context, path string) ([]lsp.Diagnostic, error) {
	return f.byPath[path], nil
}

type erroringChecker struct{ err error }

func (e erroringChecker) Check(context.Context, string) ([]lsp.Diagnostic, error) {
	return nil, e.err
}

type erroringVerifier struct{ err error }

func (e erroringVerifier) Verify(context.Context) (verify.Report, error) {
	return verify.Report{}, e.err
}

func TestSelfCorrectSurfacesCheckerErrorInsteadOfPassing(t *testing.T) {
	// Manager.Check degrades missing servers to (nil, nil); a real error (LSP
	// startup/sync failure or unreadable file) must fail the pass, not be swallowed
	// into OutcomePassed.
	sc := NewSelfCorrector("/root", erroringChecker{err: errors.New("lsp server crashed")}, nil, SelfCorrectConfig{
		Enabled:    true,
		IncludeLSP: true,
		Autonomy:   "high",
	})
	feedback, outcome := sc.AfterEdit(context.Background(), []string{"a.go"})
	if outcome != OutcomeCorrecting {
		t.Fatalf("outcome = %q, want %q (a checker error must not pass)", outcome, OutcomeCorrecting)
	}
	if !strings.Contains(feedback, "lsp server crashed") {
		t.Fatalf("feedback should surface the checker error, got: %q", feedback)
	}
}

func TestSelfCorrectSurfacesVerifierErrorInsteadOfPassing(t *testing.T) {
	// DetectPlan returns an empty plan (not an error) when no tests exist, so a
	// non-nil Verify error means verification could not run — it must fail the pass.
	sc := NewSelfCorrector("/root", nil, erroringVerifier{err: errors.New("plan detection failed")}, SelfCorrectConfig{
		Enabled:      true,
		IncludeTests: true,
		Autonomy:     "high",
	})
	feedback, outcome := sc.AfterEdit(context.Background(), []string{"a.go"})
	if outcome != OutcomeCorrecting {
		t.Fatalf("outcome = %q, want %q (a verifier error must not pass)", outcome, OutcomeCorrecting)
	}
	if !strings.Contains(feedback, "plan detection failed") {
		t.Fatalf("feedback should surface the verifier error, got: %q", feedback)
	}
}

func TestLSPDiagnosticsCheckerPropagatesReadError(t *testing.T) {
	// A read failure (e.g. the edit deleted the file) must propagate so callers
	// can decide how to handle it, rather than being swallowed as "no diagnostics".
	// manager is never reached on a read error, so a nil manager is fine here.
	c := lspDiagnosticsChecker{}
	if _, err := c.Check(context.Background(), "/nonexistent/zero-selfcorrect/missing.go"); err == nil {
		t.Fatal("expected Check to propagate the file-read error, got nil")
	}
}

func failingReport(stderr string) verify.Report {
	return verify.Report{
		OK: false,
		Results: []verify.Result{{
			Name:    "go test",
			Command: []string{"go", "test", "./..."},
			Status:  verify.StatusFail,
			Stderr:  stderr,
		}},
	}
}

func newTestCorrector(checker diagnosticsChecker, verifier projectVerifier, autonomy string, maxAttempts int) *SelfCorrector {
	return NewSelfCorrector("", checker, verifier, SelfCorrectConfig{
		Enabled:      true,
		IncludeLSP:   checker != nil,
		IncludeTests: verifier != nil,
		Autonomy:     autonomy,
		MaxAttempts:  maxAttempts,
	})
}

func TestSelfCorrectPassesWithNoFailures(t *testing.T) {
	v := &fakeVerifier{reports: []verify.Report{{OK: true}}}
	sc := newTestCorrector(nil, v, "high", 3)
	if fb, oc := sc.AfterEdit(context.Background(), []string{"main.go"}); fb != "" || oc != OutcomePassed {
		t.Fatalf("expected passed/no-feedback, got %q / %s", fb, oc)
	}
}

func TestSelfCorrectFailsThenPasses(t *testing.T) {
	v := &fakeVerifier{reports: []verify.Report{failingReport("--- FAIL: TestX"), {OK: true}}}
	sc := newTestCorrector(nil, v, "high", 3)

	fb, oc := sc.AfterEdit(context.Background(), []string{"main.go"})
	if oc != OutcomeCorrecting || !strings.Contains(fb, "Fix these and continue") || !strings.Contains(fb, "go test") {
		t.Fatalf("round 1 = %q / %s", fb, oc)
	}
	fb2, oc2 := sc.AfterEdit(context.Background(), []string{"main.go"})
	if oc2 != OutcomePassed || fb2 != "" {
		t.Fatalf("round 2 should pass cleanly, got %q / %s", fb2, oc2)
	}
}

func TestSelfCorrectAbortsAtMaxAttempts(t *testing.T) {
	v := &fakeVerifier{reports: []verify.Report{failingReport("boom")}} // always fails
	sc := newTestCorrector(nil, v, "high", 2)

	for i := 1; i <= 2; i++ {
		if _, oc := sc.AfterEdit(context.Background(), []string{"main.go"}); oc != OutcomeCorrecting {
			t.Fatalf("attempt %d outcome = %s, want correcting", i, oc)
		}
	}
	fb, oc := sc.AfterEdit(context.Background(), []string{"main.go"})
	if oc != OutcomeAborted {
		t.Fatalf("third round outcome = %s, want aborted", oc)
	}
	if !strings.Contains(fb, "stopping auto-correction") {
		t.Fatalf("abort notice = %q", fb)
	}
	if sc.attempts != 2 {
		t.Fatalf("correction attempts exceeded the ceiling: %d", sc.attempts)
	}
}

func TestSelfCorrectSkipsWhenNoChangedFiles(t *testing.T) {
	v := &fakeVerifier{reports: []verify.Report{failingReport("x")}}
	sc := newTestCorrector(nil, v, "high", 3)
	if fb, oc := sc.AfterEdit(context.Background(), nil); fb != "" || oc != OutcomeDisabled {
		t.Fatalf("read-only (no changes) should skip, got %q / %s", fb, oc)
	}
	if v.calls != 0 {
		t.Fatal("verification must not run when nothing changed")
	}
}

func TestSelfCorrectLowAutonomyReportsButDoesNotAttempt(t *testing.T) {
	v := &fakeVerifier{reports: []verify.Report{failingReport("FAIL")}}
	sc := newTestCorrector(nil, v, "low", 3)
	fb, oc := sc.AfterEdit(context.Background(), []string{"main.go"})
	if oc != OutcomeReported {
		t.Fatalf("low autonomy outcome = %s, want reported", oc)
	}
	if !strings.Contains(fb, "reporting only") {
		t.Fatalf("expected a reporting-only message, got %q", fb)
	}
	if sc.attempts != 0 {
		t.Fatalf("low autonomy must not consume a correction attempt, got %d", sc.attempts)
	}
}

func TestSelfCorrectLSPOnlyFailureFeedsDiagnostics(t *testing.T) {
	checker := fakeChecker{byPath: map[string][]lsp.Diagnostic{
		"main.go": {{
			Severity: lsp.SeverityError,
			Message:  "undefined: foo",
			Range:    lsp.Range{Start: lsp.Position{Line: 4, Character: 1}},
		}},
	}}
	v := &fakeVerifier{reports: []verify.Report{{OK: true}}} // tests pass; only LSP fails
	sc := newTestCorrector(checker, v, "high", 3)

	fb, oc := sc.AfterEdit(context.Background(), []string{"main.go"})
	if oc != OutcomeCorrecting {
		t.Fatalf("LSP error should drive a correction, got %s", oc)
	}
	if !strings.Contains(fb, "main.go:5:2: error: undefined: foo") {
		t.Fatalf("feedback missing formatted diagnostic: %q", fb)
	}
}

func TestSelfCorrectRedactsSecretsInFeedback(t *testing.T) {
	secret := "ghp_abcdefghijklmnopqrstuvwxyz0123456789"
	v := &fakeVerifier{reports: []verify.Report{failingReport("auth failed with " + secret)}}
	sc := newTestCorrector(nil, v, "high", 3)
	fb, _ := sc.AfterEdit(context.Background(), []string{"main.go"})
	if strings.Contains(fb, secret) {
		t.Fatalf("secret leaked into self-correct feedback: %q", fb)
	}
}

func TestSelfCorrectDisabledIsNoop(t *testing.T) {
	v := &fakeVerifier{reports: []verify.Report{failingReport("x")}}
	sc := NewSelfCorrector("", nil, v, SelfCorrectConfig{Enabled: false, IncludeTests: true, Autonomy: "high"})
	if fb, oc := sc.AfterEdit(context.Background(), []string{"main.go"}); fb != "" || oc != OutcomeDisabled {
		t.Fatalf("disabled corrector should no-op, got %q / %s", fb, oc)
	}
	if v.calls != 0 {
		t.Fatal("disabled corrector must not verify")
	}
}

func TestNilSelfCorrectorIsSafe(t *testing.T) {
	var sc *SelfCorrector
	if fb, oc := sc.AfterEdit(context.Background(), []string{"main.go"}); fb != "" || oc != OutcomeDisabled {
		t.Fatalf("nil corrector should no-op, got %q / %s", fb, oc)
	}
}
