package agent

import (
	"context"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/Gitlawb/zero/internal/lsp"
	"github.com/Gitlawb/zero/internal/tools"
)

// fileDiagnosticsTimeout bounds one inline post-edit diagnostics check so a
// slow or wedged language server can never hang a tool call; on timeout the
// edit simply reports without a diagnostics block.
const fileDiagnosticsTimeout = 10 * time.Second

type fileDiagnosticsChecker interface {
	Check(context.Context, string, string) ([]lsp.Diagnostic, error)
}

// NewFileDiagnostics adapts an *lsp.Manager to the per-edit inline diagnostics
// callback (tools.RunOptions.Diagnostics): it reads the just-written file,
// checks it against the file's language server, and formats error-severity
// diagnostics for the model. Warnings and hints are excluded — nagging about
// style on every edit is noise, while a type error the edit just introduced is
// exactly what the model should see before its next step. Diagnostics are
// rendered with workspace-relative paths: the absolute path would put the
// local username/home directory into the model prompt and session transcript
// on every edit. Returns nil when manager is nil, disabling inline diagnostics
// entirely.
func NewFileDiagnostics(manager *lsp.Manager, workspaceRoot string) func(context.Context, string) string {
	if manager == nil {
		return nil
	}
	return newFileDiagnostics(manager, workspaceRoot)
}

// newFileDiagnostics accepts the narrow operation used here so race tests can
// observe exactly what would be sent to LSP without installing process-global
// hooks or starting a language server.
func newFileDiagnostics(manager fileDiagnosticsChecker, workspaceRoot string) func(context.Context, string) string {
	if manager == nil {
		return nil
	}
	return func(ctx context.Context, absPath string) string {
		file, _, err := tools.ProtectedReadOpen(absPath, workspaceRoot)
		if err != nil {
			return ""
		}
		text, readErr := io.ReadAll(file)
		closeErr := file.Close()
		if readErr != nil || closeErr != nil {
			return ""
		}
		checkCtx, cancel := context.WithTimeout(ctx, fileDiagnosticsTimeout)
		defer cancel()
		diagnostics, err := manager.Check(checkCtx, absPath, string(text))
		if err != nil {
			return ""
		}
		errors := lsp.FilterBySeverity(diagnostics, lsp.SeverityError)
		if len(errors) == 0 {
			return ""
		}
		return lsp.FormatDiagnostics(diagnosticsDisplayPath(workspaceRoot, absPath), errors)
	}
}

// diagnosticsDisplayPath renders absPath relative to the workspace root for
// model-facing output, falling back to the file's base name when the path is
// outside the workspace (a bare name still identifies the file without
// exposing the directory layout).
func diagnosticsDisplayPath(workspaceRoot, absPath string) string {
	if workspaceRoot != "" {
		if rel, err := filepath.Rel(workspaceRoot, absPath); err == nil && !strings.HasPrefix(rel, "..") {
			return rel
		}
	}
	return filepath.Base(absPath)
}
