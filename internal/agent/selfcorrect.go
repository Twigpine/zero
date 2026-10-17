package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Twigpine/zero/internal/lsp"
	"github.com/Twigpine/zero/internal/redaction"
	"github.com/Twigpine/zero/internal/trace"
	"github.com/Twigpine/zero/internal/verify"
)

// defaultSelfCorrectMaxAttempts bounds how many corrective rounds a single run
// will drive before giving up, so an unattended run can never loop forever.
const defaultSelfCorrectMaxAttempts = 3

// diagnosticsChecker returns the diagnostics for a single (absolute) file path.
// *lsp.Manager is adapted to this via lspDiagnosticsChecker; tests inject a fake.
type diagnosticsChecker interface {
	Check(ctx context.Context, path string) ([]lsp.Diagnostic, error)
}

// projectVerifier runs the workspace's verification plan once and reports it.
// The default implementation is DetectPlan + verify.Run; tests inject a fake.
type projectVerifier interface {
	Verify(ctx context.Context) (verify.Report, error)
}

// SelfCorrectConfig controls the post-edit verify-and-correct cycle.
type SelfCorrectConfig struct {
	Enabled      bool
	MaxAttempts  int
	IncludeTests bool
	IncludeLSP   bool
	Autonomy     string // low | medium | high
}

// Outcome reports what a self-correct round decided, for observability/tests.
type Outcome string

const (
	OutcomeDisabled   Outcome = "disabled"   // self-correct off, or nothing to check
	OutcomePassed     Outcome = "passed"     // verification passed; nothing to do
	OutcomeCorrecting Outcome = "correcting" // failed; feedback issued for the model to fix
	OutcomeReported   Outcome = "reported"   // failed; low autonomy, reported but no auto-fix
	OutcomeAborted    Outcome = "aborted"    // failed but the correction budget is exhausted
)

// CorrectionReport is the unified result of a post-edit verification pass.
type CorrectionReport struct {
	LSPDiagnostics map[string][]lsp.Diagnostic // path -> error diagnostics
	VerifyReport   verify.Report
	// InspectErrors holds non-degradable verification failures (an LSP
	// startup/sync error, an unreadable changed file, or plan detection failing).
	// These are distinct from diagnostics and must fail the pass rather than be
	// silently swallowed into a false "passed".
	InspectErrors []string
	Failed        bool
}

// SelfCorrector runs verification after a mutating edit and, when it fails,
// synthesizes corrective feedback for the model — bounded by a per-run attempt
// budget and gated by the autonomy ceiling. One instance per run (it holds the
// attempt counter). A nil *SelfCorrector is a no-op.
type SelfCorrector struct {
	workspaceRoot string
	checker       diagnosticsChecker
	verifier      projectVerifier
	cfg           SelfCorrectConfig
	attempts      int
}

// NewSelfCorrector builds a self-corrector. checker/verifier may be nil to
// disable that half of verification (e.g. no LSP server, or tests off).
func NewSelfCorrector(workspaceRoot string, checker diagnosticsChecker, verifier projectVerifier, cfg SelfCorrectConfig) *SelfCorrector {
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = defaultSelfCorrectMaxAttempts
	}
	return &SelfCorrector{workspaceRoot: workspaceRoot, checker: checker, verifier: verifier, cfg: cfg}
}

// AfterEdit runs a verification pass over the files a mutating tool changed and
// returns corrective feedback (empty when nothing actionable) plus the outcome.
// The loop appends any non-empty feedback to the conversation so the model can
// fix the problem on its next turn.
func (sc *SelfCorrector) AfterEdit(ctx context.Context, changedFiles []string) (string, Outcome) {
	if sc == nil || !sc.cfg.Enabled || len(changedFiles) == 0 {
		return "", OutcomeDisabled
	}

	report := sc.inspect(ctx, changedFiles)
	if !report.Failed {
		return "", OutcomePassed
	}

	// Low autonomy reports the failure but never starts an auto-fix round.
	if !autonomyAllowsAutoCorrect(sc.cfg.Autonomy) {
		return sc.feedback(report, false), OutcomeReported
	}

	if sc.attempts >= sc.cfg.MaxAttempts {
		return abortedSelfCorrectNotice(sc.cfg.MaxAttempts), OutcomeAborted
	}
	sc.attempts++
	return sc.feedback(report, true), OutcomeCorrecting
}

// inspect collects LSP error diagnostics for the changed files and runs the
// project verification plan, marking Failed on any LSP error or verify failure.
func (sc *SelfCorrector) inspect(ctx context.Context, changedFiles []string) CorrectionReport {
	report := CorrectionReport{LSPDiagnostics: map[string][]lsp.Diagnostic{}}

	if sc.cfg.IncludeLSP && sc.checker != nil {
		for _, file := range changedFiles {
			abs := sc.absPath(file)
			diags, err := sc.checker.Check(ctx, abs)
			if err != nil {
				// Manager.Check degrades missing servers / unsupported files to
				// (nil, nil), so a non-nil error here is a real failure (LSP
				// startup/sync, or an unreadable changed file). Surface it instead
				// of silently passing.
				report.InspectErrors = append(report.InspectErrors, fmt.Sprintf("%s: %v", file, err))
				report.Failed = true
				continue
			}
			if errs := lsp.FilterBySeverity(diags, lsp.SeverityError); len(errs) > 0 {
				report.LSPDiagnostics[file] = errs
				report.Failed = true
			}
		}
	}

	if sc.cfg.IncludeTests && sc.verifier != nil {
		verifySpan := trace.FromContext(ctx).Span(trace.SpanVerification)
		vr, err := sc.verifier.Verify(ctx)
		verifySpan.End()
		if err != nil {
			// "No plan / no tests" is not an error (DetectPlan returns an empty
			// plan), so a non-nil error means verification could not run at all —
			// don't let that masquerade as a pass.
			report.InspectErrors = append(report.InspectErrors, fmt.Sprintf("verification could not run: %v", err))
			report.Failed = true
		} else {
			report.VerifyReport = vr
			if !vr.OK {
				report.Failed = true
			}
		}
	}
	return report
}

// feedback synthesizes the model-facing message. corrective=false is the
// low-autonomy reporting variant.
func (sc *SelfCorrector) feedback(report CorrectionReport, corrective bool) string {
	var b strings.Builder
	if corrective {
		b.WriteString("Verification failed after your edit. Fix these and continue:\n")
	} else {
		b.WriteString("Verification failed after your edit (auto-correction is off at low autonomy; reporting only):\n")
	}

	for _, msg := range report.InspectErrors {
		fmt.Fprintf(&b, "could not verify after edit: %s\n", msg)
	}
	for file, diags := range report.LSPDiagnostics {
		b.WriteString(lsp.FormatDiagnostics(file, diags))
		b.WriteString("\n")
	}
	for _, result := range report.VerifyReport.Results {
		if result.Status == verify.StatusPass {
			continue
		}
		fmt.Fprintf(&b, "%s (%s): %s\n", result.Name, strings.Join(result.Command, " "), result.Status)
		for _, line := range verifyResultLines(result) {
			b.WriteString("  " + line + "\n")
		}
	}

	// Secret-scrub the whole message: verify/LSP output can echo args, paths, env.
	return strings.TrimRight(redaction.RedactString(b.String(), redaction.Options{}), "\n")
}

// verifyResultLines returns the most useful, already-trimmed output for a failing
// check: the captured summary lines if present, else a trimmed stderr/stdout/err.
func verifyResultLines(result verify.Result) []string {
	if result.OutputSummary != nil && len(result.OutputSummary.Lines) > 0 {
		return result.OutputSummary.Lines
	}
	for _, candidate := range []string{result.Stderr, result.Stdout, result.Error} {
		if trimmed := strings.TrimSpace(candidate); trimmed != "" {
			return strings.Split(trimmed, "\n")
		}
	}
	return nil
}

func (sc *SelfCorrector) absPath(path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(sc.workspaceRoot, path)
}

func autonomyAllowsAutoCorrect(autonomy string) bool {
	switch strings.ToLower(strings.TrimSpace(autonomy)) {
	case "medium", "high":
		return true
	default:
		// low / unset: conservative — report, don't auto-attempt.
		return false
	}
}

func abortedSelfCorrectNotice(maxAttempts int) string {
	return fmt.Sprintf("Verification is still failing after %d self-correction attempts; stopping auto-correction. Review the remaining failures and decide how to proceed.", maxAttempts)
}

// lspDiagnosticsChecker adapts *lsp.Manager to diagnosticsChecker by reading the
// file's current contents before asking the manager to check it.
type lspDiagnosticsChecker struct {
	manager *lsp.Manager
}

// NewLSPDiagnosticsChecker wraps an *lsp.Manager for use as a SelfCorrector
// checker. Returns nil when manager is nil so self-correct cleanly skips LSP.
func NewLSPDiagnosticsChecker(manager *lsp.Manager) diagnosticsChecker {
	if manager == nil {
		return nil
	}
	return lspDiagnosticsChecker{manager: manager}
}

func (c lspDiagnosticsChecker) Check(ctx context.Context, path string) ([]lsp.Diagnostic, error) {
	text, err := os.ReadFile(path)
	if err != nil {
		return nil, err // unreadable (e.g. deleted) -> let the caller decide
	}
	return c.manager.Check(ctx, path, string(text))
}

// detectPlanVerifier is the default projectVerifier: detect the plan once per
// run and execute it with verify.Run.
type detectPlanVerifier struct {
	root    string
	options verify.RunOptions
}

// NewProjectVerifier builds the default verifier rooted at workspaceRoot.
func NewProjectVerifier(workspaceRoot string) projectVerifier {
	return detectPlanVerifier{root: workspaceRoot}
}

func (v detectPlanVerifier) Verify(ctx context.Context) (verify.Report, error) {
	plan, err := verify.DetectPlan(v.root)
	if err != nil {
		return verify.Report{}, err
	}
	return verify.Run(ctx, plan, v.options), nil
}
