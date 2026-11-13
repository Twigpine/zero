package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Twigpine/zero/internal/lsp"
)

// fileDiagnosticsTimeout bounds one inline post-edit diagnostics check so a
// slow or wedged language server can never hang a tool call; on timeout the
// edit simply reports without a diagnostics block.
const fileDiagnosticsTimeout = 10 * time.Second

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
	return func(ctx context.Context, absPath string) string {
		text, err := os.ReadFile(absPath)
		if err != nil {
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
