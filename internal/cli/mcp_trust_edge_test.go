package cli

// Edge-case coverage for the MCP trust gate on the oauth-login and check paths:
// the error-return contract of the gated resolveOAuthServer, the trust-store-read-error
// notice variant, and the accepted (advisory) notice over-emission on the not-oauth path.

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/config"
)

// writeProjectMCPConfig drops a ./.zero/config.json under dir declaring one MCP server,
// so projectMCPConfigExists() reads it as present.
func writeProjectMCPConfig(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".zero"), 0o700); err != nil {
		t.Fatal(err)
	}
	body := `{"mcp":{"servers":{"proj":{"type":"stdio","command":"proj-cmd"}}}}`
	if err := os.WriteFile(filepath.Join(dir, ".zero", "config.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// breakTrustStore makes the trust store unreadable (creates trust.json as a directory),
// so workspacetrust.IsTrusted returns an error and resolveTrust fails closed with
// trustCheckErrored=true.
func breakTrustStore(t *testing.T) {
	t.Helper()
	configRoot := setTrustConfigRoot(t)
	if err := os.MkdirAll(filepath.Join(configRoot, "zero", "trust.json"), 0o700); err != nil {
		t.Fatal(err)
	}
}

// TestResolveOAuthServerValidationErrorReturnsZeroSkip locks the new 3-value contract on
// the pre-resolve error path: an invalid server name errors before trust is resolved, so
// the returned skip is zero (nothing was excluded yet).
func TestResolveOAuthServerValidationErrorReturnsZeroSkip(t *testing.T) {
	deps := appDeps{getwd: func() (string, error) { return t.TempDir(), nil }}
	_, skip, err := resolveOAuthServer(deps, "bad name with spaces")
	if err == nil {
		t.Fatal("invalid server name must error")
	}
	if skip.excludedProjectConfig || skip.trustCheckErrored {
		t.Fatalf("a validation error must return a zero skip, got %+v", skip)
	}
}

// TestResolveOAuthServerGetwdErrorPropagates covers the getwd error path of the gated
// resolver.
func TestResolveOAuthServerGetwdErrorPropagates(t *testing.T) {
	deps := appDeps{getwd: func() (string, error) { return "", errors.New("boom") }}
	_, _, err := resolveOAuthServer(deps, "remote")
	if err == nil || !strings.Contains(err.Error(), "failed to resolve workspace") {
		t.Fatalf("getwd error must propagate, got %v", err)
	}
}

// TestRunMCPOAuthLoginStoreErrorNotice proves the store-read-error branch: when the trust
// store cannot be read, login fails closed AND the notice names the store error (the
// "could not be read" variant) rather than the plain untrusted wording.
func TestRunMCPOAuthLoginStoreErrorNotice(t *testing.T) {
	breakTrustStore(t)
	cwd := t.TempDir()
	writeProjectMCPConfig(t, cwd)
	deps := appDeps{
		getwd: func() (string, error) { return cwd, nil },
		resolveMCPConfig: func(_ string, excludeProject bool) (config.MCPConfig, error) {
			servers := map[string]config.MCPServerConfig{}
			if !excludeProject {
				servers["proj"] = config.MCPServerConfig{Type: "http", URL: "https://x.invalid", Auth: "oauth"}
			}
			return config.MCPConfig{Servers: servers}, nil
		},
	}
	var out, errBuf bytes.Buffer
	if code := runWithDeps([]string{"mcp", "oauth", "login", "proj"}, &out, &errBuf, deps); code == exitSuccess {
		t.Fatal("login must fail closed on a trust-store read error")
	}
	if got := errBuf.String(); !strings.Contains(got, "could not be read") || !strings.Contains(got, "not configured") {
		t.Fatalf("store-error login must emit the store-errored notice AND not-configured, got %q", got)
	}
}

// TestRunMCPCheckStoreErrorNotice is the mcp-check sibling of the store-error case.
func TestRunMCPCheckStoreErrorNotice(t *testing.T) {
	breakTrustStore(t)
	cwd := t.TempDir()
	writeProjectMCPConfig(t, cwd)
	deps := appDeps{
		getwd: func() (string, error) { return cwd, nil },
		resolveMCPConfig: func(_ string, excludeProject bool) (config.MCPConfig, error) {
			servers := map[string]config.MCPServerConfig{}
			if !excludeProject {
				servers["proj"] = config.MCPServerConfig{Type: "stdio", Command: "proj-cmd"}
			}
			return config.MCPConfig{Servers: servers}, nil
		},
	}
	var out, errBuf bytes.Buffer
	if code := runWithDeps([]string{"mcp", "check", "proj"}, &out, &errBuf, deps); code == exitSuccess {
		t.Fatal("mcp check must fail closed on a trust-store read error")
	}
	if got := errBuf.String(); !strings.Contains(got, "could not be read") {
		t.Fatalf("mcp check store-error must emit the store-errored notice, got %q", got)
	}
}

// TestRunMCPOAuthLoginNoticeCoFiresWithNonOAuthError documents accepted behavior: in an
// untrusted workspace with project MCP config on disk, a login that fails because the
// named USER-config server is not OAuth still co-emits the trust notice (project MCP was
// excluded this run, which is true). The notice is advisory and keyed on "project config
// was dropped", not on "this server was the dropped one" — the same coarse scoping the
// mcp-check path uses. If this behavior changes intentionally, update this test.
func TestRunMCPOAuthLoginNoticeCoFiresWithNonOAuthError(t *testing.T) {
	setTrustConfigRoot(t) // untrusted
	cwd := t.TempDir()
	writeProjectMCPConfig(t, cwd)
	deps := appDeps{
		getwd: func() (string, error) { return cwd, nil },
		// "foo" is a user-config server (survives the gate) that does not declare oauth.
		resolveMCPConfig: func(_ string, _ bool) (config.MCPConfig, error) {
			return config.MCPConfig{Servers: map[string]config.MCPServerConfig{
				"foo": {Type: "http", URL: "https://foo.invalid"},
			}}, nil
		},
	}
	var out, errBuf bytes.Buffer
	if code := runWithDeps([]string{"mcp", "oauth", "login", "foo"}, &out, &errBuf, deps); code == exitSuccess {
		t.Fatal("login on a non-oauth server must fail")
	}
	got := errBuf.String()
	if !strings.Contains(got, "oauth") {
		t.Fatalf("want the not-oauth error, got %q", got)
	}
	if !strings.Contains(got, "zero trust") {
		t.Fatalf("expected the advisory trust notice to co-fire (accepted behavior), got %q", got)
	}
}

// TestResolveOAuthServerResolveConfigErrorPropagates covers the resolveMCPConfig-error
// return of the gated resolver: the error propagates with the computed skip.
func TestResolveOAuthServerResolveConfigErrorPropagates(t *testing.T) {
	setTrustConfigRoot(t)
	deps := appDeps{
		getwd: func() (string, error) { return t.TempDir(), nil },
		resolveMCPConfig: func(_ string, _ bool) (config.MCPConfig, error) {
			return config.MCPConfig{}, errors.New("resolve boom")
		},
	}
	_, _, err := resolveOAuthServer(deps, "remote")
	if err == nil || !strings.Contains(err.Error(), "resolve boom") {
		t.Fatalf("resolveMCPConfig error must propagate, got %v", err)
	}
}

// TestResolveOAuthServerNormalizeErrorPropagates covers the NormalizeConfig-error return:
// a malformed server (stdio with no command) fails normalization.
func TestResolveOAuthServerNormalizeErrorPropagates(t *testing.T) {
	setTrustConfigRoot(t)
	deps := appDeps{
		getwd: func() (string, error) { return t.TempDir(), nil },
		resolveMCPConfig: func(_ string, _ bool) (config.MCPConfig, error) {
			return config.MCPConfig{Servers: map[string]config.MCPServerConfig{
				"remote": {Type: "stdio"}, // no command -> NormalizeConfig rejects
			}}, nil
		},
	}
	_, _, err := resolveOAuthServer(deps, "remote")
	if err == nil || !strings.Contains(err.Error(), "requires command") {
		t.Fatalf("NormalizeConfig error must propagate, got %v", err)
	}
}

// TestRunMCPCheckDisabledServer covers the found-but-disabled branch of runMCPCheck.
func TestRunMCPCheckDisabledServer(t *testing.T) {
	setTrustConfigRoot(t)
	deps := appDeps{
		getwd: func() (string, error) { return t.TempDir(), nil },
		resolveMCPConfig: func(_ string, _ bool) (config.MCPConfig, error) {
			return config.MCPConfig{Servers: map[string]config.MCPServerConfig{
				"proj": {Type: "stdio", Command: "c", Disabled: true},
			}}, nil
		},
	}
	var out, errBuf bytes.Buffer
	if code := runWithDeps([]string{"mcp", "check", "proj"}, &out, &errBuf, deps); code == exitSuccess {
		t.Fatal("mcp check on a disabled server must fail")
	}
	if got := errBuf.String(); !strings.Contains(got, "is disabled") {
		t.Fatalf("want disabled error, got %q", got)
	}
}

// TestRunMCPCheckNormalizeError covers the found-but-malformed branch of runMCPCheck: the
// scoped NormalizeConfig fails before any spawn.
func TestRunMCPCheckNormalizeError(t *testing.T) {
	setTrustConfigRoot(t)
	deps := appDeps{
		getwd: func() (string, error) { return t.TempDir(), nil },
		resolveMCPConfig: func(_ string, _ bool) (config.MCPConfig, error) {
			return config.MCPConfig{Servers: map[string]config.MCPServerConfig{
				"proj": {Type: "stdio"}, // no command
			}}, nil
		},
	}
	var out, errBuf bytes.Buffer
	if code := runWithDeps([]string{"mcp", "check", "proj"}, &out, &errBuf, deps); code == exitSuccess {
		t.Fatal("mcp check on a malformed server must fail")
	}
	if got := errBuf.String(); !strings.Contains(got, "requires command") {
		t.Fatalf("want normalize error, got %q", got)
	}
}

// TestRunMCPCheckReportsUnreachableServer covers the case the command exists
// for: a server that fails to start.
//
// RegisterTools is best-effort by design, so a connect failure is recorded on
// the runtime and registration returns nil. runMCPCheck used to inspect only
// that nil error and then print "is reachable", which told the user their
// broken server was fine. The command is spawned for real here rather than
// stubbed, because the whole failure mode was a mismatch between what
// registration returns and what it records.
func TestRunMCPCheckReportsUnreachableServer(t *testing.T) {
	setTrustConfigRoot(t)
	deps := appDeps{
		getwd: func() (string, error) { return t.TempDir(), nil },
		resolveMCPConfig: func(_ string, _ bool) (config.MCPConfig, error) {
			return config.MCPConfig{Servers: map[string]config.MCPServerConfig{
				"broken": {Type: "stdio", Command: "zero-definitely-not-a-real-binary-xyz"},
			}}, nil
		},
	}
	var out, errBuf bytes.Buffer
	if code := runWithDeps([]string{"mcp", "check", "broken"}, &out, &errBuf, deps); code == exitSuccess {
		t.Fatalf("mcp check must fail for a server that did not start; stdout=%q stderr=%q", out.String(), errBuf.String())
	}
	if got := out.String(); strings.Contains(got, "is reachable") {
		t.Fatalf("mcp check reported a failed server as reachable: %q", got)
	}
	if got := errBuf.String(); !strings.Contains(got, "not reachable") {
		t.Fatalf("want an unreachable error naming the server, got %q", got)
	}
}

// The --json contract for an unreachable server, which the text case above does
// not touch.
//
// The JSON branch is a separate return path with its own payload, so a caller
// parsing it would otherwise be relying on a shape nothing pins: reverting the
// check leaves it emitting status "ok" with a tool count, which is valid JSON
// and completely wrong.
func TestRunMCPCheckReportsUnreachableServerJSON(t *testing.T) {
	setTrustConfigRoot(t)
	deps := appDeps{
		getwd: func() (string, error) { return t.TempDir(), nil },
		resolveMCPConfig: func(_ string, _ bool) (config.MCPConfig, error) {
			return config.MCPConfig{Servers: map[string]config.MCPServerConfig{
				"broken": {Type: "stdio", Command: "zero-definitely-not-a-real-binary-xyz"},
			}}, nil
		},
	}
	var out, errBuf bytes.Buffer
	if code := runWithDeps([]string{"mcp", "check", "broken", "--json"}, &out, &errBuf, deps); code == exitSuccess {
		t.Fatalf("mcp check --json must fail for a server that did not start; stdout=%q stderr=%q", out.String(), errBuf.String())
	}
	var payload struct {
		ServerName string `json:"serverName"`
		Status     string `json:"status"`
		Error      string `json:"error"`
		ToolCount  int    `json:"toolCount"`
	}
	if err := json.Unmarshal(out.Bytes(), &payload); err != nil {
		t.Fatalf("decode mcp check --json output %q: %v", out.String(), err)
	}
	if payload.ServerName != "broken" {
		t.Fatalf("serverName = %q, want the server that was checked", payload.ServerName)
	}
	if payload.Status != "unreachable" {
		t.Fatalf("status = %q, want %q", payload.Status, "unreachable")
	}
	if !strings.Contains(payload.Error, "not reachable") {
		t.Fatalf("error = %q, want it to say the server is not reachable", payload.Error)
	}
	// A reachable payload carries a tool count; an unreachable one must not
	// imply zero tools were found, which reads as a server that simply exposes
	// none.
	if payload.ToolCount != 0 {
		t.Fatalf("toolCount = %d, want it absent from an unreachable payload", payload.ToolCount)
	}
}
