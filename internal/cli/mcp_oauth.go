package cli

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/Twigpine/zero/internal/mcp"
	"github.com/Twigpine/zero/internal/redaction"
)

func runMCPOAuth(args []string, stdout io.Writer, stderr io.Writer, deps appDeps) int {
	if len(args) == 0 {
		return writeExecUsageError(stderr, "mcp oauth subcommand required. Use `zero mcp oauth status`.")
	}
	if args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		if err := writeMCPOAuthHelp(stdout); err != nil {
			return exitCrash
		}
		return exitSuccess
	}

	switch args[0] {
	case "login":
		return runMCPOAuthLogin(args[1:], stdout, stderr, deps)
	case "logout":
		return runMCPOAuthLogout(args[1:], stdout, stderr, deps)
	case "status":
		return runMCPOAuthStatus(args[1:], stdout, stderr, deps)
	default:
		return writeExecUsageError(stderr, fmt.Sprintf("unknown mcp oauth subcommand %q", args[0]))
	}
}

func runMCPOAuthLogin(args []string, stdout io.Writer, stderr io.Writer, deps appDeps) int {
	opts, positional, help, err := parseMCPPositionalCommand(args)
	if err != nil {
		return writeExecUsageError(stderr, err.Error())
	}
	if help {
		if err := writeMCPOAuthHelp(stdout); err != nil {
			return exitCrash
		}
		return exitSuccess
	}
	if opts.json {
		// login is an interactive flow (prints a URL, waits for the callback); a
		// machine-readable mode would be misleading, so reject it rather than
		// accepting the flag and ignoring it.
		return writeExecUsageError(stderr, "zero mcp oauth login does not support --json")
	}
	if len(positional) != 1 {
		return writeExecUsageError(stderr, "usage: zero mcp oauth login <server>")
	}
	serverName := positional[0]

	server, skip, err := resolveOAuthServer(deps, serverName)
	if err != nil {
		// When the named server was dropped because the workspace is untrusted, explain
		// that with the same one-line notice the other trust-gated MCP paths emit, so a
		// gated project OAuth server reads as "run zero trust", not a bare miss.
		emitTrustNotice(stderr, skip)
		return writeAppError(stderr, redaction.ErrorMessage(err, redaction.Options{}), exitCrash)
	}

	store, err := deps.newMCPTokenStore()
	if err != nil {
		return writeAppError(stderr, redaction.ErrorMessage(err, redaction.Options{}), exitCrash)
	}

	// The opener prints the authorization URL so headless environments can copy
	// it into a browser. The URL carries no token material.
	opener := func(authURL string) error {
		_, err := fmt.Fprintf(stdout, "Open this URL in your browser to authorize %s:\n\n  %s\n\nWaiting for the authorization callback...\n", serverName, authURL)
		return err
	}

	token, err := mcp.Login(context.Background(), mcp.LoginOptions{
		ServerName:  serverName,
		ServerURL:   server.URL,
		Config:      oauthConfigForServer(server),
		OpenBrowser: opener,
		Now:         deps.now,
	})
	if err != nil {
		return writeAppError(stderr, redaction.ErrorMessage(err, redaction.Options{}), exitCrash)
	}
	if err := store.SaveForServer(server, token); err != nil {
		return writeAppError(stderr, redaction.ErrorMessage(err, redaction.Options{}), exitCrash)
	}

	if _, err := fmt.Fprintf(stdout, "Stored OAuth credentials for %s.\n", serverName); err != nil {
		return exitCrash
	}
	return exitSuccess
}

func runMCPOAuthLogout(args []string, stdout io.Writer, stderr io.Writer, deps appDeps) int {
	options, positional, help, err := parseMCPPositionalCommand(args)
	if err != nil {
		return writeExecUsageError(stderr, err.Error())
	}
	if help {
		if err := writeMCPOAuthHelp(stdout); err != nil {
			return exitCrash
		}
		return exitSuccess
	}
	if len(positional) != 1 {
		return writeExecUsageError(stderr, "usage: zero mcp oauth logout <server>")
	}
	serverName := positional[0]

	store, err := deps.newMCPTokenStore()
	if err != nil {
		return writeAppError(stderr, redaction.ErrorMessage(err, redaction.Options{}), exitCrash)
	}
	removed, err := store.DeleteForServerName(serverName)
	if err != nil {
		return writeAppError(stderr, redaction.ErrorMessage(err, redaction.Options{}), exitCrash)
	}
	if options.json {
		payload := struct {
			ServerName string `json:"serverName"`
			Removed    bool   `json:"removed"`
		}{ServerName: serverName, Removed: removed}
		if err := writePrettyJSON(stdout, payload); err != nil {
			return exitCrash
		}
		return exitSuccess
	}
	if removed {
		if _, err := fmt.Fprintf(stdout, "Removed OAuth credentials for %s.\n", serverName); err != nil {
			return exitCrash
		}
	} else if _, err := fmt.Fprintf(stdout, "No OAuth credentials stored for %s.\n", serverName); err != nil {
		return exitCrash
	}
	return exitSuccess
}

func runMCPOAuthStatus(args []string, stdout io.Writer, stderr io.Writer, deps appDeps) int {
	options, positional, help, err := parseMCPPositionalCommand(args)
	if err != nil {
		return writeExecUsageError(stderr, err.Error())
	}
	if help {
		if err := writeMCPOAuthHelp(stdout); err != nil {
			return exitCrash
		}
		return exitSuccess
	}
	if len(positional) > 1 {
		return writeExecUsageError(stderr, "usage: zero mcp oauth status [server]")
	}

	store, err := deps.newMCPTokenStore()
	if err != nil {
		return writeAppError(stderr, redaction.ErrorMessage(err, redaction.Options{}), exitCrash)
	}
	statuses, err := store.Status()
	if err != nil {
		return writeAppError(stderr, redaction.ErrorMessage(err, redaction.Options{}), exitCrash)
	}
	if len(positional) == 1 {
		statuses = filterTokenStatuses(statuses, positional[0])
	}

	if options.json {
		// TokenStatus is redaction-safe by construction: it carries presence and
		// expiry metadata but never the access or refresh token, so it is emitted
		// directly. Routing time.Time through reflective redaction would mangle the
		// expiry timestamp.
		payload := struct {
			Tokens []mcp.TokenStatus `json:"tokens"`
		}{Tokens: statuses}
		if err := writePrettyJSON(stdout, payload); err != nil {
			return exitCrash
		}
		return exitSuccess
	}
	if _, err := fmt.Fprintln(stdout, mcp.FormatTokenStatuses(statuses)); err != nil {
		return exitCrash
	}
	return exitSuccess
}

func filterTokenStatuses(statuses []mcp.TokenStatus, serverName string) []mcp.TokenStatus {
	filtered := make([]mcp.TokenStatus, 0, 1)
	for _, status := range statuses {
		if status.ServerName == serverName {
			filtered = append(filtered, status)
		}
	}
	return filtered
}

// resolveOAuthServer loads the workspace MCP config and returns the named server
// after verifying it declares OAuth authentication. It also returns a trustSkip so the
// caller can emit the workspace-trust notice when a project server was dropped.
//
// Login is gated behind workspace trust (fail-closed) like the stdio spawn paths: it
// loads a project-scoped MCP server and drives its OAuth flow (endpoint discovery,
// dynamic client registration, code exchange, token persist) against the server's
// configured URL. That is a project-scoped server being activated and contacted, so an
// untrusted clone's ./.zero/config.json OAuth server must be dropped rather than loaded —
// leaving it ungated would let a cloned repo initiate outbound OAuth I/O and persist a
// token. There is no --worktree reassignment on this command path, so trustRoot == cwd.
func resolveOAuthServer(deps appDeps, serverName string) (mcp.Server, trustSkip, error) {
	if err := mcp.ValidateServerName(serverName); err != nil {
		return mcp.Server{}, trustSkip{}, err
	}
	cwd, err := deps.getwd()
	if err != nil {
		return mcp.Server{}, trustSkip{}, fmt.Errorf("failed to resolve workspace: %w", err)
	}
	excludeProject, trustCheckErrored := resolveTrust(cwd)
	skip := trustSkip{
		excludedProjectConfig: excludeProject && projectMCPConfigExists(cwd),
		trustCheckErrored:     trustCheckErrored,
	}
	cfg, err := deps.resolveMCPConfig(cwd, excludeProject)
	if err != nil {
		return mcp.Server{}, skip, err
	}
	servers, err := mcp.NormalizeConfig(cfg)
	if err != nil {
		return mcp.Server{}, skip, err
	}
	for _, server := range servers {
		if server.Name != serverName {
			continue
		}
		if !strings.EqualFold(server.Auth, mcp.ServerAuthOAuth) {
			return mcp.Server{}, skip, fmt.Errorf("MCP server %q does not declare auth: \"oauth\"", serverName)
		}
		return server, skip, nil
	}
	return mcp.Server{}, skip, fmt.Errorf("MCP server %q is not configured", serverName)
}

func oauthConfigForServer(server mcp.Server) mcp.OAuthConfig {
	if server.OAuth != nil {
		return *server.OAuth
	}
	return mcp.OAuthConfig{}
}

func writeMCPOAuthHelp(w io.Writer) error {
	_, err := fmt.Fprint(w, `Usage:
  zero mcp oauth <command>

Commands:
  login <server>     Run the OAuth flow and store credentials for a server
  logout <server>    Delete stored OAuth credentials for a server
  status [server]    Show credential presence and expiry (never the token)

Flags:
      --json    Print command result as JSON
  -h, --help    Show this help
`)
	return err
}
