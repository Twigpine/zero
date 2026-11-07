package cli

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/Twigpine/zero/internal/config"
	zeroSandbox "github.com/Twigpine/zero/internal/sandbox"
)

type sandboxCommandOptions struct {
	json      bool
	confirm   bool
	effective bool
	reason    string
	path      string
}

const permissionProfileScopeNote = "permissionProfile is derived from this process's environment and working directory; a command run with its own environment can add further credential deny-read entries."

func runSandbox(args []string, stdout io.Writer, stderr io.Writer, deps appDeps) int {
	if len(args) == 0 {
		return writeExecUsageError(stderr, "sandbox subcommand required. Use `zero sandbox policy` or `zero sandbox grants list`.")
	}
	switch args[0] {
	case "-h", "--help", "help":
		if err := writeSandboxHelp(stdout); err != nil {
			return exitCrash
		}
		return exitSuccess
	case "policy":
		return runSandboxPolicy(args[1:], stdout, stderr, deps)
	case "setup":
		return runSandboxSetup(args[1:], stdout, stderr, deps)
	case "check":
		return runSandboxCheck(args[1:], stdout, stderr, deps)
	case "grants":
		return runSandboxGrants(args[1:], stdout, stderr, deps)
	default:
		return writeExecUsageError(stderr, fmt.Sprintf("unknown sandbox subcommand %q", args[0]))
	}
}

func runSandboxPolicy(args []string, stdout io.Writer, stderr io.Writer, deps appDeps) int {
	options, help, err := parseSandboxCommandOptions(args, sandboxCommandFlags{allowEffective: true})
	if err != nil {
		return writeExecUsageError(stderr, err.Error())
	}
	if help {
		if err := writeSandboxPolicyHelp(stdout); err != nil {
			return exitCrash
		}
		return exitSuccess
	}
	workspaceRoot, err := resolveWorkspaceRoot("", deps)
	if err != nil {
		return writeExecUsageError(stderr, err.Error())
	}
	store, err := deps.newSandboxStore()
	if err != nil {
		return writeAppError(stderr, err.Error(), exitCrash)
	}
	// Surface config resolution failures instead of silently falling back to
	// DefaultPolicy(), which would otherwise misreport the active sandbox posture.
	resolved, err := deps.resolveConfig(workspaceRoot, config.Overrides{})
	if err != nil {
		return writeAppError(stderr, err.Error(), exitProvider)
	}
	policy := applyConfiguredSandboxPolicy(zeroSandbox.DefaultPolicy(), resolved.Sandbox)
	backend := deps.selectSandboxBackend(zeroSandbox.BackendOptions{})
	plan := backend.BuildPlan(workspaceRoot, policy)
	if options.effective {
		// Compute the effective write roots exactly the way the engine does:
		// workspace root first, then the user-granted extras from the global
		// config. A stale config entry (e.g. a directory that no longer
		// exists) must not crash `zero sandbox policy --effective` — fall
		// back to the workspace root and surface the error visibly instead.
		writeRoots := []string{workspaceRoot}
		var writeRootsErr error
		if scope, scopeErr := zeroSandbox.NewScope(workspaceRoot, resolved.Sandbox.AdditionalWriteRoots); scopeErr != nil {
			writeRootsErr = scopeErr
			// NewScope only fails on extras, so a workspace-only scope cannot
			// fail; use it so the fallback renders the same symlink-resolved
			// workspace root as the success path.
			if fallback, fallbackErr := zeroSandbox.NewScope(workspaceRoot, nil); fallbackErr == nil {
				writeRoots = fallback.Roots()
			}
		} else {
			writeRoots = scope.Roots()
		}
		return runSandboxPolicyEffective(options, workspaceRoot, policy, backend, plan, store.FilePath(), writeRoots, writeRootsErr, stdout)
	}
	if options.json {
		// The profile here is built from Zero's own environment and working
		// directory, because that is all this command has: no command has been
		// chosen yet. A nested command carries its own cwd and env, and the
		// credential baseline resolves relative token overrides against those, so
		// an executed command can carry MORE denyReadIfExists entries than this
		// view lists. Say so in the payload rather than leaving a consumer to
		// assume it is the whole picture (see PermissionProfileFromPolicy).
		payload := struct {
			Policy                zeroSandbox.Policy      `json:"policy"`
			Backend               zeroSandbox.Backend     `json:"backend"`
			Plan                  zeroSandbox.BackendPlan `json:"plan"`
			GrantsPath            string                  `json:"grantsPath"`
			PermissionProfileNote string                  `json:"permissionProfileNote"`
		}{
			Policy:                policy,
			Backend:               backend,
			Plan:                  plan,
			GrantsPath:            store.FilePath(),
			PermissionProfileNote: permissionProfileScopeNote,
		}
		if err := writePrettyJSON(stdout, payload); err != nil {
			return exitCrash
		}
		return exitSuccess
	}
	if _, err := fmt.Fprintln(stdout, formatSandboxPolicy(workspaceRoot, policy, backend, plan, store.FilePath())); err != nil {
		return exitCrash
	}
	return exitSuccess
}

func runSandboxSetup(args []string, stdout io.Writer, stderr io.Writer, deps appDeps) int {
	options, help, err := parseSandboxCommandOptions(args, sandboxCommandFlags{})
	if err != nil {
		return writeExecUsageError(stderr, err.Error())
	}
	if help {
		if err := writeSandboxSetupHelp(stdout); err != nil {
			return exitCrash
		}
		return exitSuccess
	}
	workspaceRoot, err := resolveWorkspaceRoot("", deps)
	if err != nil {
		return writeExecUsageError(stderr, err.Error())
	}
	resolved, err := deps.resolveConfig(workspaceRoot, config.Overrides{})
	if err != nil {
		return writeAppError(stderr, err.Error(), exitProvider)
	}
	policy := applyConfiguredSandboxPolicy(zeroSandbox.DefaultPolicy(), resolved.Sandbox)
	backend := deps.selectSandboxBackend(zeroSandbox.BackendOptions{})
	if backend.Platform != "windows" {
		message := "No native sandbox setup is required for " + displayPlatform(backend.Platform)
		return writeSandboxSetupResult(stdout, options.json, sandboxSetupResult{
			Platform: displayPlatform(backend.Platform),
			Backend:  backend.Name,
			Ran:      false,
			Message:  message,
		})
	}
	if backend.Name != zeroSandbox.BackendWindowsRestrictedToken || !backend.Available || backend.Executable == "" {
		message := "Windows sandbox setup helper is not available"
		if strings.TrimSpace(backend.Message) != "" {
			message += ": " + backend.Message
		}
		return writeAppError(stderr, message, exitProvider)
	}
	scope, err := zeroSandbox.NewScope(workspaceRoot, resolved.Sandbox.AdditionalWriteRoots)
	if err != nil {
		return writeExecUsageError(stderr, err.Error())
	}
	profile := zeroSandbox.PermissionProfileFromPolicy(workspaceRoot, policy, scope)
	// Resolve the setup helper the same way the runner is resolved: a standalone
	// .exe when shipped (release), else self-dispatch via the running zero binary
	// (dev / plain build). This mirrors the backend's command-runner resolution
	// so `zero sandbox setup` works in every layout, not just release.
	setupHelper := zeroSandbox.ResolveWindowsSandboxSetupHelper(nil)
	if !setupHelper.Available() {
		return writeAppError(stderr, "Windows sandbox setup helper is not available", exitProvider)
	}
	setupArgs, err := zeroSandbox.BuildWindowsSandboxSetupArgs(zeroSandbox.WindowsSandboxSetupArgsOptions{
		CommandCWD:        workspaceRoot,
		WorkspaceRoots:    []string{workspaceRoot},
		PermissionProfile: profile,
	})
	if err != nil {
		return writeAppError(stderr, err.Error(), exitCrash)
	}
	setupArgs = append(append([]string{}, setupHelper.ArgsPrefix...), setupArgs...)
	if err := deps.runSandboxSetupHelper(setupHelper.Name, setupArgs, stdout, stderr); err != nil {
		return writeAppError(stderr, "Windows sandbox setup failed: "+err.Error(), exitProvider)
	}
	return writeSandboxSetupResult(stdout, options.json, sandboxSetupResult{
		Platform: "windows",
		Backend:  backend.Name,
		Helper:   setupHelper.Name,
		Ran:      true,
		Message:  "Windows sandbox setup complete",
	})
}

type sandboxSetupResult struct {
	Platform string                  `json:"platform"`
	Backend  zeroSandbox.BackendName `json:"backend"`
	Helper   string                  `json:"helper,omitempty"`
	Ran      bool                    `json:"ran"`
	Message  string                  `json:"message"`
}

func writeSandboxSetupResult(stdout io.Writer, asJSON bool, result sandboxSetupResult) int {
	if asJSON {
		if err := writePrettyJSON(stdout, result); err != nil {
			return exitCrash
		}
		return exitSuccess
	}
	if _, err := fmt.Fprintln(stdout, result.Message); err != nil {
		return exitCrash
	}
	if result.Helper != "" {
		if _, err := fmt.Fprintln(stdout, "helper: "+result.Helper); err != nil {
			return exitCrash
		}
	}
	return exitSuccess
}

func displayPlatform(platform string) string {
	if strings.TrimSpace(platform) == "" {
		return "this platform"
	}
	return platform
}

// sandboxGuards is the resolved set of pre-execution safety guards the engine
// applies for the effective policy. There is no layered/merged config today, so
// the "effective" view is the fully-resolved DefaultPolicy plus the platform
// backend and the always-on static guards.
type sandboxGuards struct {
	InteractiveCommand bool `json:"interactiveCommand"`
	Network            bool `json:"network"`
	Workspace          bool `json:"workspace"`
}

func resolveSandboxGuards(policy zeroSandbox.Policy) sandboxGuards {
	return sandboxGuards{
		// Interactive-command detection is a static pre-exec guard that always
		// runs in the bash tool regardless of policy toggles.
		InteractiveCommand: true,
		Network:            policy.Network == zeroSandbox.NetworkDeny,
		Workspace:          policy.EnforceWorkspace,
	}
}

func runSandboxPolicyEffective(options sandboxCommandOptions, workspaceRoot string, policy zeroSandbox.Policy, backend zeroSandbox.Backend, plan zeroSandbox.BackendPlan, grantsPath string, writeRoots []string, writeRootsErr error, stdout io.Writer) int {
	guards := resolveSandboxGuards(policy)
	if options.json {
		payload := struct {
			WorkspaceRoot string   `json:"workspaceRoot"`
			WriteRoots    []string `json:"writeRoots"`
			// WriteRootsError carries the fail-soft scope construction error so
			// JSON consumers see the same signal as the text write_roots_error
			// line: a stale sandbox.additionalWriteRoots entry means the real
			// entrypoints would refuse to launch, not run workspace-only.
			WriteRootsError       string                  `json:"writeRootsError,omitempty"`
			Policy                zeroSandbox.Policy      `json:"policy"`
			Backend               zeroSandbox.Backend     `json:"backend"`
			Plan                  zeroSandbox.BackendPlan `json:"plan"`
			Guards                sandboxGuards           `json:"guards"`
			GrantsPath            string                  `json:"grantsPath"`
			PermissionProfileNote string                  `json:"permissionProfileNote"`
		}{
			WorkspaceRoot:         workspaceRoot,
			WriteRoots:            writeRoots,
			Policy:                policy,
			Backend:               backend,
			Plan:                  plan,
			Guards:                guards,
			GrantsPath:            grantsPath,
			PermissionProfileNote: permissionProfileScopeNote,
		}
		if writeRootsErr != nil {
			payload.WriteRootsError = writeRootsErr.Error()
		}
		if err := writePrettyJSON(stdout, payload); err != nil {
			return exitCrash
		}
		return exitSuccess
	}
	if _, err := fmt.Fprintln(stdout, formatEffectiveSandboxPolicy(workspaceRoot, policy, backend, plan, guards, grantsPath, writeRoots, writeRootsErr)); err != nil {
		return exitCrash
	}
	return exitSuccess
}

func formatEffectiveSandboxPolicy(workspaceRoot string, policy zeroSandbox.Policy, backend zeroSandbox.Backend, plan zeroSandbox.BackendPlan, guards sandboxGuards, grantsPath string, writeRoots []string, writeRootsErr error) string {
	lines := []string{
		"Zero effective sandbox policy",
		"root: " + workspaceRoot,
		"mode: " + string(policy.Mode),
		"network: " + string(policy.Network),
		"enforce_workspace: " + fmt.Sprintf("%t", policy.EnforceWorkspace),
		"write_roots: " + strings.Join(writeRoots, ", "),
		"permission_profile_note: " + permissionProfileScopeNote,
	}
	if writeRootsErr != nil {
		// Fail soft, visibly: a stale sandbox.additionalWriteRoots entry must
		// not hide the rest of the status output.
		lines = append(lines, "write_roots_error: "+writeRootsErr.Error())
	}
	lines = append(lines,
		"backend: "+string(backend.Name),
		"target_backend: "+string(plan.TargetBackend),
		"support_level: "+string(plan.SupportLevel),
		"command_wrapped: "+fmt.Sprintf("%t", plan.CommandWrapped),
		"enforcement_level: "+string(plan.EnforcementLevel),
		"requires_platform_sandbox: "+fmt.Sprintf("%t", plan.RequiresPlatformSandbox),
		"interactive_command_guard: "+enabledLabel(guards.InteractiveCommand),
		"network_guard: "+enabledLabel(guards.Network),
		"workspace_guard: "+enabledLabel(guards.Workspace),
		"grants: "+grantsPath,
	)
	if plan.DowngradeReason != "" {
		lines = append(lines, "downgrade_reason: "+plan.DowngradeReason)
	}
	if backend.Platform != "" {
		lines = append(lines, "backend_platform: "+backend.Platform)
	}
	for _, restriction := range plan.Restrictions {
		lines = append(lines, "restriction: "+restriction)
	}
	for _, warning := range plan.Warnings {
		lines = append(lines, "warning: "+warning)
	}
	return strings.Join(lines, "\n")
}

func enabledLabel(enabled bool) string {
	if enabled {
		return "enabled"
	}
	return "disabled"
}

func runSandboxGrants(args []string, stdout io.Writer, stderr io.Writer, deps appDeps) int {
	if len(args) == 0 {
		return writeExecUsageError(stderr, "sandbox grants subcommand required. Use `zero sandbox grants list`.")
	}
	switch args[0] {
	case "-h", "--help", "help":
		if err := writeSandboxGrantsHelp(stdout); err != nil {
			return exitCrash
		}
		return exitSuccess
	case "list":
		options, help, err := parseSandboxCommandOptions(args[1:], sandboxCommandFlags{})
		if err != nil {
			return writeExecUsageError(stderr, err.Error())
		}
		if help {
			if err := writeSandboxGrantsHelp(stdout); err != nil {
				return exitCrash
			}
			return exitSuccess
		}
		store, err := deps.newSandboxStore()
		if err != nil {
			return writeAppError(stderr, err.Error(), exitCrash)
		}
		grants, err := store.List()
		if err != nil {
			return writeAppError(stderr, err.Error(), exitCrash)
		}
		prefixes, err := store.ListCommandPrefixes()
		if err != nil {
			return writeAppError(stderr, err.Error(), exitCrash)
		}
		if options.json {
			if err := writePrettyJSON(stdout, struct {
				Grants          []zeroSandbox.Grant              `json:"grants"`
				CommandPrefixes []zeroSandbox.CommandPrefixGrant `json:"commandPrefixes,omitempty"`
			}{Grants: grants, CommandPrefixes: prefixes}); err != nil {
				return exitCrash
			}
			return exitSuccess
		}
		if _, err := fmt.Fprintln(stdout, zeroSandbox.FormatGrantListWithCommandPrefixes(grants, prefixes)); err != nil {
			return exitCrash
		}
		return exitSuccess
	case "allow", "deny":
		return runSandboxGrantSet(args[0], args[1:], stdout, stderr, deps)
	case "revoke":
		return runSandboxGrantRevoke(args[1:], stdout, stderr, deps)
	case "clear":
		return runSandboxGrantClear(args[1:], stdout, stderr, deps)
	default:
		return writeExecUsageError(stderr, fmt.Sprintf("unknown sandbox grants subcommand %q", args[0]))
	}
}

func runSandboxGrantSet(command string, args []string, stdout io.Writer, stderr io.Writer, deps appDeps) int {
	options, positional, help, err := parseSandboxPositionalOptions(args)
	if err != nil {
		return writeExecUsageError(stderr, err.Error())
	}
	if help {
		if err := writeSandboxGrantSetHelp(stdout); err != nil {
			return exitCrash
		}
		return exitSuccess
	}
	if len(positional) != 1 {
		return writeExecUsageError(stderr, "usage: zero sandbox grants "+command+" <tool> [--path file] [--reason text] [--json]")
	}
	decision := zeroSandbox.GrantAllow
	if command == "deny" {
		decision = zeroSandbox.GrantDeny
	}
	store, err := deps.newSandboxStore()
	if err != nil {
		return writeAppError(stderr, err.Error(), exitCrash)
	}
	input := zeroSandbox.GrantInput{
		ToolName: positional[0],
		Decision: decision,
		Reason:   options.reason,
	}
	if options.path != "" {
		// --path persists an exact-file grant. Resolve to an absolute path so it
		// matches how scopes are stored (and how `revoke --path` looks them up).
		abs, absErr := filepath.Abs(options.path)
		if absErr != nil {
			return writeExecUsageError(stderr, absErr.Error())
		}
		input.Scope = abs
		input.ScopeKind = zeroSandbox.ScopeFile
	}
	grant, err := store.Grant(input)
	if err != nil {
		return writeExecUsageError(stderr, err.Error())
	}
	if options.json {
		if err := writePrettyJSON(stdout, struct {
			Grant zeroSandbox.Grant `json:"grant"`
		}{Grant: grant}); err != nil {
			return exitCrash
		}
		return exitSuccess
	}
	if _, err := fmt.Fprintf(stdout, "Sandbox grant saved: %s [%s]\n", grant.ToolName, grant.Decision); err != nil {
		return exitCrash
	}
	return exitSuccess
}

func runSandboxGrantRevoke(args []string, stdout io.Writer, stderr io.Writer, deps appDeps) int {
	options, positional, help, err := parseSandboxPositionalOptions(args)
	if err != nil {
		return writeExecUsageError(stderr, err.Error())
	}
	if help {
		if err := writeSandboxGrantRevokeHelp(stdout); err != nil {
			return exitCrash
		}
		return exitSuccess
	}
	if len(positional) != 1 {
		return writeExecUsageError(stderr, "usage: zero sandbox grants revoke <tool> [--path file] [--json]")
	}
	store, err := deps.newSandboxStore()
	if err != nil {
		return writeAppError(stderr, err.Error(), exitCrash)
	}
	// With --path, revoke only the grant scoped to that exact file/dir; otherwise
	// revoke every grant for the tool (the pre-existing behavior).
	var revoked int
	if options.path != "" {
		revoked, err = store.RevokePath(positional[0], options.path)
	} else {
		revoked, err = store.Revoke(positional[0])
	}
	if err != nil {
		return writeExecUsageError(stderr, err.Error())
	}
	if options.json {
		if err := writePrettyJSON(stdout, struct {
			Revoked int `json:"revoked"`
		}{Revoked: revoked}); err != nil {
			return exitCrash
		}
		return exitSuccess
	}
	if _, err := fmt.Fprintf(stdout, "Sandbox grants revoked: %d\n", revoked); err != nil {
		return exitCrash
	}
	return exitSuccess
}

func runSandboxGrantClear(args []string, stdout io.Writer, stderr io.Writer, deps appDeps) int {
	options, help, err := parseSandboxCommandOptions(args, sandboxCommandFlags{allowConfirm: true})
	if err != nil {
		return writeExecUsageError(stderr, err.Error())
	}
	if help {
		if err := writeSandboxGrantClearHelp(stdout); err != nil {
			return exitCrash
		}
		return exitSuccess
	}
	if !options.confirm {
		return writeExecUsageError(stderr, "zero sandbox grants clear requires --confirm")
	}
	store, err := deps.newSandboxStore()
	if err != nil {
		return writeAppError(stderr, err.Error(), exitCrash)
	}
	cleared, err := store.Clear()
	if err != nil {
		return writeAppError(stderr, err.Error(), exitCrash)
	}
	if options.json {
		if err := writePrettyJSON(stdout, struct {
			Cleared int `json:"cleared"`
		}{Cleared: cleared}); err != nil {
			return exitCrash
		}
		return exitSuccess
	}
	if _, err := fmt.Fprintf(stdout, "Sandbox grants cleared: %d\n", cleared); err != nil {
		return exitCrash
	}
	return exitSuccess
}

type sandboxCommandFlags struct {
	allowConfirm   bool
	allowEffective bool
}

func parseSandboxCommandOptions(args []string, flags sandboxCommandFlags) (sandboxCommandOptions, bool, error) {
	options := sandboxCommandOptions{}
	for index := 0; index < len(args); index++ {
		arg := args[index]
		switch {
		case arg == "-h" || arg == "--help" || arg == "help":
			return options, true, nil
		case arg == "--json":
			options.json = true
		case flags.allowConfirm && arg == "--confirm":
			options.confirm = true
		case flags.allowEffective && arg == "--effective":
			options.effective = true
		case strings.HasPrefix(arg, "-"):
			return options, false, execUsageError{fmt.Sprintf("unknown sandbox flag %q", arg)}
		default:
			return options, false, execUsageError{fmt.Sprintf("unexpected sandbox argument %q", arg)}
		}
	}
	return options, false, nil
}

func parseSandboxPositionalOptions(args []string) (sandboxCommandOptions, []string, bool, error) {
	options := sandboxCommandOptions{}
	positional := []string{}
	for index := 0; index < len(args); index++ {
		arg := args[index]
		switch {
		case arg == "-h" || arg == "--help" || arg == "help":
			return options, positional, true, nil
		case arg == "--json":
			options.json = true
		case arg == "--reason":
			value, next, err := nextFlagValue(args, index, arg)
			if err != nil {
				return options, positional, false, err
			}
			options.reason = value
			index = next
		case strings.HasPrefix(arg, "--reason="):
			options.reason = strings.TrimSpace(strings.TrimPrefix(arg, "--reason="))
		case arg == "--path":
			value, next, err := nextFlagValue(args, index, arg)
			if err != nil {
				return options, positional, false, err
			}
			// An explicit but empty --path is a user error, not "tool-wide": treating
			// it as unset would silently widen an allow to the whole tool, or make a
			// revoke drop every grant for the tool. Fail closed.
			if strings.TrimSpace(value) == "" {
				return options, positional, false, execUsageError{"--path requires a non-empty file path"}
			}
			options.path = strings.TrimSpace(value)
			index = next
		case strings.HasPrefix(arg, "--path="):
			value := strings.TrimSpace(strings.TrimPrefix(arg, "--path="))
			if value == "" {
				return options, positional, false, execUsageError{"--path requires a non-empty file path"}
			}
			options.path = value
		case strings.HasPrefix(arg, "-"):
			return options, positional, false, execUsageError{fmt.Sprintf("unknown sandbox grants flag %q", arg)}
		default:
			positional = append(positional, strings.TrimSpace(arg))
		}
	}
	return options, positional, false, nil
}

func formatSandboxPolicy(workspaceRoot string, policy zeroSandbox.Policy, backend zeroSandbox.Backend, plan zeroSandbox.BackendPlan, grantsPath string) string {
	lines := []string{
		"Zero sandbox policy",
		"root: " + workspaceRoot,
		"mode: " + string(policy.Mode),
		"network: " + string(policy.Network),
		"backend: " + string(backend.Name),
		"target_backend: " + string(plan.TargetBackend),
		"support_level: " + string(plan.SupportLevel),
		"command_wrapped: " + fmt.Sprintf("%t", plan.CommandWrapped),
		"enforcement_level: " + string(plan.EnforcementLevel),
		"requires_platform_sandbox: " + fmt.Sprintf("%t", plan.RequiresPlatformSandbox),
	}
	if plan.DowngradeReason != "" {
		lines = append(lines, "downgrade_reason: "+plan.DowngradeReason)
	}
	if backend.Platform != "" {
		lines = append(lines, "backend_platform: "+backend.Platform)
	}
	lines = append(lines,
		"backend_available: "+fmt.Sprintf("%t", backend.Available),
		"backend_fallback: "+fmt.Sprintf("%t", backend.Fallback),
		"backend_command_wrapping: "+fmt.Sprintf("%t", backend.CommandWrapping),
		"backend_native_isolation: "+fmt.Sprintf("%t", backend.NativeIsolation),
		"grants: "+grantsPath,
	)
	if backend.Message != "" {
		lines = append(lines, "backend_message: "+backend.Message)
	}
	for _, warning := range plan.Warnings {
		lines = append(lines, "warning: "+warning)
	}
	return strings.Join(lines, "\n")
}

func writeSandboxHelp(w io.Writer) error {
	_, err := fmt.Fprint(w, `Usage:
  zero sandbox <command>

Commands:
  policy      Inspect active sandbox policy and platform backend
  setup       Run native platform sandbox setup
  check       Evaluate the sandbox decision for a hypothetical tool action
  grants      Manage persistent sandbox grants

`)
	return err
}

func writeSandboxPolicyHelp(w io.Writer) error {
	_, err := fmt.Fprint(w, `Usage:
  zero sandbox policy [flags]

Flags:
      --effective         Print the resolved effective policy (merged config + guards)
      --json              Print JSON output
  -h, --help              Show this help
`)
	return err
}

func writeSandboxSetupHelp(w io.Writer) error {
	_, err := fmt.Fprint(w, `Usage:
  zero sandbox setup [flags]

Runs native platform sandbox setup when the selected backend requires it.

Flags:
      --json              Print JSON output
  -h, --help              Show this help
`)
	return err
}

func writeSandboxGrantsHelp(w io.Writer) error {
	_, err := fmt.Fprint(w, `Usage:
  zero sandbox grants <command>

Commands:
  list        List persistent sandbox grants
  allow       Persistently allow a tool
  deny        Persistently deny a tool
  revoke      Revoke a tool grant
  clear       Clear all sandbox grants
`)
	return err
}

func writeSandboxGrantSetHelp(w io.Writer) error {
	_, err := fmt.Fprint(w, `Usage:
  zero sandbox grants allow <tool> [flags]
  zero sandbox grants deny <tool> [flags]

Flags:
      --reason <text>     Human-readable reason for the grant
      --path <path>       Scope the grant to one exact file (default: tool-wide)
      --json              Print JSON output
  -h, --help              Show this help
`)
	return err
}

func writeSandboxGrantRevokeHelp(w io.Writer) error {
	_, err := fmt.Fprint(w, `Usage:
  zero sandbox grants revoke <tool> [flags]

Flags:
      --path <path>       Revoke only the grant scoped to this exact file/dir
                          (default: revoke every grant for the tool)
      --json              Print JSON output
  -h, --help              Show this help
`)
	return err
}

func writeSandboxGrantClearHelp(w io.Writer) error {
	_, err := fmt.Fprint(w, `Usage:
  zero sandbox grants clear --confirm [flags]

Flags:
      --confirm           Confirm removal of all sandbox grants
      --json              Print JSON output
  -h, --help              Show this help
`)
	return err
}
