package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunServeMCPListsReadOnlyToolsByDefault(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := runWithDeps([]string{"serve", "--mcp"}, &stdout, &stderr, appDeps{
		getwd: func() (string, error) {
			return t.TempDir(), nil
		},
		stdin: bytes.NewReader(serveMCPInput(t)),
	})

	if exitCode != exitSuccess {
		t.Fatalf("expected exit code 0, got %d; stderr=%q", exitCode, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("expected empty stderr, got %q", stderr.String())
	}
	output := stdout.String()
	for _, want := range []string{"read_file", "list_directory", "glob", "grep"} {
		if !strings.Contains(output, want) {
			t.Fatalf("expected MCP output to contain %q, got %q", want, output)
		}
	}
	for _, unwanted := range []string{"write_file", "apply_patch", "bash", "web_fetch"} {
		if strings.Contains(output, unwanted) {
			t.Fatalf("did not expect default MCP output to contain %q: %q", unwanted, output)
		}
	}
}

func TestRunServeMCPAllowsUnsafeToolsWithExplicitFlag(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := runWithDeps([]string{"serve", "--mcp", "--allow-unsafe-tools"}, &stdout, &stderr, appDeps{
		getwd: func() (string, error) {
			return t.TempDir(), nil
		},
		stdin: bytes.NewReader(serveMCPInput(t)),
	})

	if exitCode != exitSuccess {
		t.Fatalf("expected exit code 0, got %d; stderr=%q", exitCode, stderr.String())
	}
	output := stdout.String()
	for _, want := range []string{"read_file", "write_file", "apply_patch", "bash", "web_fetch"} {
		if !strings.Contains(output, want) {
			t.Fatalf("expected unsafe MCP output to contain %q, got %q", want, output)
		}
	}
	if !strings.Contains(stderr.String(), "Unsafe MCP server tools enabled") {
		t.Fatalf("expected unsafe warning on stderr, got %q", stderr.String())
	}
}

func TestRunServeRequiresMCPMode(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := runWithDeps([]string{"serve"}, &stdout, &stderr, appDeps{})

	if exitCode != exitUsage {
		t.Fatalf("expected exit code %d, got %d", exitUsage, exitCode)
	}
	if stdout.Len() != 0 {
		t.Fatalf("expected empty stdout, got %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "serve requires --mcp") {
		t.Fatalf("expected usage error, got %q", stderr.String())
	}
}

func TestRunServeMCPWiresWorkspaceRootAndAddDirIntoResources(t *testing.T) {
	workspace := t.TempDir()
	extra := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "workspace.txt"), []byte("ws\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(extra, "extra.txt"), []byte("ex\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := runWithDeps(
		[]string{"serve", "--mcp", "-C", workspace, "--add-dir", extra},
		&stdout,
		&stderr,
		appDeps{stdin: bytes.NewReader(serveMCPResourcesInput(t))},
	)
	if exitCode != exitSuccess {
		t.Fatalf("exit=%d stderr=%q", exitCode, stderr.String())
	}
	output := stdout.String()
	if !strings.Contains(output, "workspace.txt") {
		t.Fatalf("expected workspace resource, got %q", output)
	}
	if !strings.Contains(output, "extra.txt") {
		t.Fatalf("expected --add-dir resource, got %q", output)
	}
}

func TestRunServeMCPRejectsMissingAddDir(t *testing.T) {
	workspace := t.TempDir()
	missing := filepath.Join(workspace, "does-not-exist")
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := runWithDeps(
		[]string{"serve", "--mcp", "-C", workspace, "--add-dir", missing},
		&stdout,
		&stderr,
		appDeps{stdin: bytes.NewReader(nil)},
	)
	if exitCode != exitUsage {
		t.Fatalf("exit=%d want %d stderr=%q", exitCode, exitUsage, stderr.String())
	}
	if !strings.Contains(stderr.String(), "--add-dir") {
		t.Fatalf("expected --add-dir error, got %q", stderr.String())
	}
}

func TestParseServeArgsCollectsAddDirs(t *testing.T) {
	options, help, err := parseServeArgs([]string{"--mcp", "--add-dir", "/one", "--add-dir=/two", "-C", "/ws"})
	if err != nil {
		t.Fatal(err)
	}
	if help {
		t.Fatal("unexpected help")
	}
	if !options.mcp || options.cwd != "/ws" {
		t.Fatalf("options=%#v", options)
	}
	if len(options.addDirs) != 2 || options.addDirs[0] != "/one" || options.addDirs[1] != "/two" {
		t.Fatalf("addDirs=%v", options.addDirs)
	}
}

func TestBuildServeScopeNilWithoutExtras(t *testing.T) {
	scope, err := buildServeScope(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if scope != nil {
		t.Fatalf("expected nil scope, got %#v", scope)
	}
}

func TestBuildServeScopeKeepsLexicalPaths(t *testing.T) {
	base := t.TempDir()
	realWorkspace := filepath.Join(base, "real-workspace")
	realExtra := filepath.Join(base, "real-extra")
	for _, dir := range []string{realWorkspace, realExtra} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	linkWorkspace := filepath.Join(base, "link-workspace")
	linkExtra := filepath.Join(base, "link-extra")
	// SKIPPED, NOT FAILED, WHEN THE PLATFORM WILL NOT MAKE THE LINK. Creating a
	// symlink on Windows needs SeCreateSymbolicLinkPrivilege, which an ordinary
	// developer account does not hold, so this reported a failure with nothing
	// wrong in the code: "A required privilege is not held by the client". CI
	// runners are elevated, so it only ever showed up locally, where it cost a
	// round of triage before being ruled environmental. The sibling tests that
	// need a link already answer this way.
	if err := os.Symlink(realWorkspace, linkWorkspace); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if err := os.Symlink(realExtra, linkExtra); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	scope, err := buildServeScope(linkWorkspace, []string{linkExtra})
	if err != nil {
		t.Fatal(err)
	}
	if scope == nil {
		t.Fatal("expected non-nil scope")
	}
	roots := scope.Roots()
	wantWorkspace, err := filepath.Abs(linkWorkspace)
	if err != nil {
		t.Fatal(err)
	}
	wantExtra, err := filepath.Abs(linkExtra)
	if err != nil {
		t.Fatal(err)
	}
	if len(roots) != 2 || roots[0] != wantWorkspace || roots[1] != wantExtra {
		t.Fatalf("roots=%v want [%q %q]", roots, wantWorkspace, wantExtra)
	}
}

func serveMCPInput(t *testing.T) []byte {
	t.Helper()

	var input bytes.Buffer
	writeServeMCPMessage(t, &input, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params":  map[string]any{},
	})
	writeServeMCPMessage(t, &input, map[string]any{
		"jsonrpc": "2.0",
		"method":  "notifications/initialized",
		"params":  map[string]any{},
	})
	writeServeMCPMessage(t, &input, map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "tools/list",
		"params":  map[string]any{},
	})
	return input.Bytes()
}

func serveMCPResourcesInput(t *testing.T) []byte {
	t.Helper()

	var input bytes.Buffer
	writeServeMCPMessage(t, &input, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params":  map[string]any{},
	})
	writeServeMCPMessage(t, &input, map[string]any{
		"jsonrpc": "2.0",
		"method":  "notifications/initialized",
		"params":  map[string]any{},
	})
	writeServeMCPMessage(t, &input, map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "resources/list",
		"params":  map[string]any{},
	})
	return input.Bytes()
}

func writeServeMCPMessage(t *testing.T, buffer *bytes.Buffer, message map[string]any) {
	t.Helper()
	body, err := json.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintf(buffer, "Content-Length: %d\r\n\r\n%s", len(body), body); err != nil {
		t.Fatal(err)
	}
}
