package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Gitlawb/zero/internal/lsp"
)

type recordingDiagnosticsChecker struct {
	called bool
	text   string
}

func (checker *recordingDiagnosticsChecker) Check(_ context.Context, _ string, text string) ([]lsp.Diagnostic, error) {
	checker.called = true
	checker.text = text
	return nil, nil
}

// Diagnostics are model-facing: absolute paths would leak the local username
// and directory layout into the prompt and session transcript on every edit.
func TestDiagnosticsDisplayPath(t *testing.T) {
	root := filepath.Join("/Users", "someone", "project")
	cases := []struct {
		root, abs, want string
	}{
		{root, filepath.Join(root, "internal", "a.go"), filepath.Join("internal", "a.go")},
		{root, filepath.Join("/etc", "other.go"), "other.go"}, // outside root -> base name only
		{"", filepath.Join("/home", "user", "x.go"), "x.go"},  // no root -> base name only
	}
	for _, c := range cases {
		if got := diagnosticsDisplayPath(c.root, c.abs); got != c.want {
			t.Errorf("diagnosticsDisplayPath(%q, %q) = %q, want %q", c.root, c.abs, got, c.want)
		}
	}
}

func TestFileDiagnosticsSwapDoesNotSendTokenToLSP(t *testing.T) {
	for _, aliasKind := range []string{"symlink", "hardlink"} {
		t.Run(aliasKind, func(t *testing.T) {
			dir := t.TempDir()
			token := filepath.Join(dir, "bridge-token")
			target := filepath.Join(dir, "ordinary.go")
			const secret = "diagnostics-swap-secret"
			if err := os.WriteFile(token, []byte(secret), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(target, []byte("package ordinary\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			t.Setenv("ZERO_DAEMON_REMOTE_TOKEN", "")
			t.Setenv("ZERO_DAEMON_REMOTE_TOKEN_FILE", token)
			t.Setenv("ZERO_INTERNAL_DAEMON_REMOTE_TOKEN_FILE_RESOLVED", "")

			// This swap occurs after the mutating tool would have completed its
			// rooted write and immediately before its diagnostics callback reads.
			if err := os.Remove(target); err != nil {
				t.Fatal(err)
			}
			var err error
			if aliasKind == "symlink" {
				err = os.Symlink(token, target)
			} else {
				err = os.Link(token, target)
			}
			if err != nil {
				t.Skipf("%s unavailable: %v", aliasKind, err)
			}
			checker := &recordingDiagnosticsChecker{}
			output := newFileDiagnostics(checker, dir)(context.Background(), target)
			if checker.called || strings.Contains(checker.text, secret) || strings.Contains(output, secret) {
				t.Fatalf("token reached LSP/output: called=%v text=%q output=%q", checker.called, checker.text, output)
			}
			got, readErr := os.ReadFile(token)
			if readErr != nil || string(got) != secret {
				t.Fatalf("token changed: content=%q err=%v", got, readErr)
			}
		})
	}
}

func TestFileDiagnosticsOrdinaryPositiveControlReachesLSP(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "ordinary.go")
	token := filepath.Join(dir, "bridge-token")
	const ordinary = "package ordinary\n"
	if err := os.WriteFile(target, []byte(ordinary), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(token, []byte("positive-control-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ZERO_DAEMON_REMOTE_TOKEN", "")
	t.Setenv("ZERO_DAEMON_REMOTE_TOKEN_FILE", token)
	t.Setenv("ZERO_INTERNAL_DAEMON_REMOTE_TOKEN_FILE_RESOLVED", "")
	checker := &recordingDiagnosticsChecker{}
	_ = newFileDiagnostics(checker, dir)(context.Background(), target)
	if !checker.called || checker.text != ordinary {
		t.Fatalf("ordinary diagnostics input: called=%v text=%q", checker.called, checker.text)
	}
}
