package doctor

import (
	"fmt"
	"os/exec"
	"runtime"
	"strings"

	"github.com/Twigpine/zero/internal/config"
	"github.com/Twigpine/zero/internal/lsp"
	"github.com/Twigpine/zero/internal/sandbox"
)

// sandboxBackendCheck reports whether the selected platform has a native
// sandbox backend ready. A missing backend is a WARN because setup may be
// optional for non-shell workflows, but sandboxed shell execution remains
// unavailable until a native backend is installed.
func sandboxBackendCheck(goos string, lookup func(string) (string, error), workspaceRoot string, sandboxConfig config.SandboxConfig) Check {
	if goos == "" {
		goos = runtime.GOOS
	}
	if lookup == nil {
		lookup = exec.LookPath
	}
	backend := sandbox.SelectBackend(sandbox.BackendOptions{GOOS: goos, LookupExecutable: lookup})
	if backend.Available {
		if setupCheck := windowsSandboxSetupCheck(goos, backend, workspaceRoot, sandboxConfig); setupCheck != nil {
			return *setupCheck
		}
		return check("sandbox.backend", "Sandbox backend", StatusPass, fmt.Sprintf("Native sandbox backend %s is available.", backend.Name), map[string]any{
			"backend":      string(backend.Name),
			"platform":     goos,
			"supportLevel": string(backend.SupportLevel()),
		})
	}
	remedy := sandboxRemedy(goos, backend)
	return check("sandbox.backend", "Sandbox backend", StatusWarn, sandboxBackendWarning(goos, backend), map[string]any{
		"backend":         string(backend.Name),
		"platform":        goos,
		"supportLevel":    string(backend.SupportLevel()),
		"downgradeReason": backend.DowngradeReason(sandbox.DefaultPolicy()),
		"remedy":          remedy,
	})
}

func sandboxBackendWarning(goos string, backend sandbox.Backend) string {
	if backend.Message != "" {
		return fmt.Sprintf("Native sandbox backend unavailable on %s: %s.", goos, backend.Message)
	}
	return fmt.Sprintf("Native sandbox backend unavailable on %s; sandboxed shell execution is unavailable.", goos)
}

// sandboxRemedy returns the platform-specific, actionable step to obtain a
// native sandbox backend or complete required setup.
func sandboxRemedy(goos string, backend sandbox.Backend) string {
	switch goos {
	case "linux":
		return "install the Linux sandbox helper or bubblewrap so native command wrapping is available"
	case "darwin":
		return "sandbox-exec ships with macOS; ensure /usr/bin is on PATH so `sandbox-exec` resolves"
	case "windows":
		if backend.Message != "" {
			return "install the Windows sandbox command runner and setup helper together, then run `zero sandbox setup`"
		}
		return "run `zero sandbox setup` to prepare the Windows native sandbox"
	default:
		return "no native sandbox adapter exists for " + goos + "; run inside Linux (bubblewrap) or macOS (sandbox-exec) for native isolation"
	}
}

func windowsSandboxSetupCheck(goos string, backend sandbox.Backend, workspaceRoot string, sandboxConfig config.SandboxConfig) *Check {
	if goos != "windows" || backend.Name != sandbox.BackendWindowsRestrictedToken {
		return nil
	}
	workspaceRoot = strings.TrimSpace(workspaceRoot)
	if workspaceRoot == "" {
		return nil
	}
	scope, err := sandbox.NewScope(workspaceRoot, sandboxConfig.AdditionalWriteRoots)
	if err != nil {
		result := check("sandbox.backend", "Sandbox backend", StatusWarn, "Windows sandbox setup could not be checked because configured write roots are invalid: "+err.Error(), map[string]any{
			"backend":      string(backend.Name),
			"platform":     goos,
			"supportLevel": string(backend.SupportLevel()),
			"setupStatus":  "invalid-config",
			"remedy":       "fix sandbox.additionalWriteRoots, then run `zero sandbox setup`",
		})
		return &result
	}
	sandboxHome, err := sandbox.ResolveWindowsSandboxHome(nil)
	if err != nil {
		result := check("sandbox.backend", "Sandbox backend", StatusWarn, "Windows sandbox setup could not be checked: "+err.Error(), map[string]any{
			"backend":      string(backend.Name),
			"platform":     goos,
			"supportLevel": string(backend.SupportLevel()),
			"setupStatus":  "unknown",
			"remedy":       "run `zero sandbox setup` after fixing the sandbox home path",
		})
		return &result
	}
	profile := sandbox.PermissionProfileFromPolicy(workspaceRoot, doctorSandboxPolicy(sandboxConfig), scope)
	setupConfig := sandbox.WindowsSandboxSetupConfig{
		SandboxHome:    sandboxHome,
		CommandCWD:     workspaceRoot,
		WorkspaceRoots: []string{workspaceRoot},
		// Same augmentation the setup args and the command plan apply, so doctor
		// fingerprints what a real command fingerprints. Safe here: doctor runs in
		// the operator's shell, not behind the sandbox's TEMP redirection.
		PermissionProfile: sandbox.WindowsSandboxProfileWithRuntimeRoots(profile, []string{workspaceRoot}),
		// Same opt-in a command would resolve, so doctor reports the principal
		// mismatch as out-of-date setup instead of passing a check the next command
		// will fail.
		PrincipalOptIn: sandbox.WindowsSandboxPrincipalOptIn(nil),
	}
	if err := sandbox.ValidateWindowsSandboxSetupMarker(setupConfig); err != nil {
		result := check("sandbox.backend", "Sandbox backend", StatusWarn, fmt.Sprintf("Native sandbox backend %s is installed, but Windows sandbox setup is missing or out of date: %v.", backend.Name, err), map[string]any{
			"backend":      string(backend.Name),
			"platform":     goos,
			"supportLevel": string(backend.SupportLevel()),
			"setupStatus":  "missing-or-out-of-date",
			"remedy":       "run `zero sandbox setup` to prepare the Windows native sandbox",
		})
		return &result
	}
	// Setup is valid and the opt-in is set, so report which principal a command
	// will actually run as.
	//
	// This used to warn that the principal stood down under the DEFAULT
	// network-deny policy, which was true while network denial could only be
	// expressed on the restricted token: a principal token cannot carry the
	// synthetic offline-marker SID the WFP filters matched. Dual-role provisioning
	// removed that limit by giving each workspace an offline account whose real
	// group membership the same filters match, so deny mode now runs on a
	// principal exactly as allow mode does. Leaving the warning would have doctor
	// telling operators their reads are unconfined while they are confined.
	//
	// Naming the role rather than just saying "active" is the useful part. The two
	// accounts differ only in that membership, so an operator debugging network
	// behaviour needs to know which one this workspace picks.
	if setupConfig.PrincipalOptIn {
		// Asked of the shared rule rather than re-derived here, so doctor and the
		// runtime cannot disagree about which account a command runs as. Two
		// copies drifting is what produced the wrong report this replaces.
		role := sandbox.WindowsSandboxPrincipalRoleForNetwork(profile.Network.Mode)
		// SETUP IS CURRENT IS NOT THE SAME AS THE PRINCIPAL IS LIVE, and this used
		// to claim the second from the first. Marker validation compares serialized
		// plans and hashes; it names no account. Delete the selected account or its
		// secret, or drop the offline account out of ZeroSandboxOffline, and the
		// marker still validates while the runtime either falls back to the weaker
		// restricted-token path or fails outright. Doctor asserting read
		// confinement in that state is worse than saying nothing, because it is the
		// surface an operator checks precisely when they are unsure.
		//
		// Verifying liveness needs the account, a usable secret and (for offline)
		// the group membership, all Windows-only live queries this package cannot
		// make. So the claim is narrowed to what the marker actually proves, and
		// the role is still named because that part IS derived rather than assumed
		// and is what an operator debugging network behaviour needs.
		result := check("sandbox.principal", "Sandbox principal", StatusPass,
			fmt.Sprintf("Sandbox principal setup is current; a command in this workspace selects the %s principal. Setup state does not confirm the account is usable.", role), map[string]any{
				"backend":  string(backend.Name),
				"platform": goos,
				"optIn":    true,
				// Deliberately not "active": nothing here checked the account, the
				// secret or the group membership.
				"setupCurrent": true,
				"role":         role,
				"networkMode":  string(profile.Network.Mode),
			})
		return &result
	}
	return nil
}

func doctorSandboxPolicy(cfg config.SandboxConfig) sandbox.Policy {
	policy := sandbox.DefaultPolicy()
	switch sandbox.NetworkMode(cfg.Network) {
	case sandbox.NetworkAllow, sandbox.NetworkDeny:
		policy.Network = sandbox.NetworkMode(cfg.Network)
	}
	policy.MonitorDenials = cfg.MonitorDenials
	return policy
}

// lspServersCheck reports which language servers ZERO would use are present on
// PATH. Missing servers are not a failure — ZERO degrades to text-only edits for
// those languages — so the worst status is WARN, and each missing server gets an
// actionable install command keyed by its binary name. Only the tier-1 servers
// (lsp.CoreServerBinaries) drive the status: the long-tail servers configured
// for breadth are listed informationally under missingOptional, because warning
// about a missing zls on a machine with no Zig code would be permanent noise.
func lspServersCheck(lookup func(string) (string, error)) Check {
	if lookup == nil {
		lookup = exec.LookPath
	}
	core := map[string]bool{}
	for _, binary := range lsp.CoreServerBinaries() {
		core[binary] = true
	}
	present := map[string]any{}
	missing := map[string]any{}
	missingOptional := map[string]any{}
	for _, binary := range lsp.ServerBinaries() {
		if _, err := lookup(binary); err == nil {
			present[binary] = "on PATH"
			continue
		}
		if core[binary] {
			missing[binary] = lspRemedy(binary)
		} else {
			missingOptional[binary] = lspRemedy(binary)
		}
	}
	details := map[string]any{"present": present}
	if len(missingOptional) > 0 {
		details["missingOptional"] = missingOptional
	}
	if len(missing) == 0 {
		message := "All core language servers are available on PATH."
		if len(missingOptional) > 0 {
			message = fmt.Sprintf("All core language servers are available on PATH (%d optional server(s) not installed).", len(missingOptional))
		}
		return check("lsp.servers", "LSP servers", StatusPass, message, details)
	}
	details["missing"] = missing
	return check("lsp.servers", "LSP servers", StatusWarn, fmt.Sprintf("%d language server(s) missing from PATH; affected files degrade to text-only edits.", len(missing)), details)
}

// lspRemedy returns an actionable install command for a missing language-server
// binary. It is provider/tooling neutral and names the ecosystem's standard
// installer.
func lspRemedy(binary string) string {
	switch binary {
	case "gopls":
		return "install with `go install golang.org/x/tools/gopls@latest` (ensure $GOBIN is on PATH)"
	case "typescript-language-server":
		return "install with `npm install -g typescript typescript-language-server`"
	case "pyright-langserver":
		return "install with `npm install -g pyright` (or `pipx install pyright`)"
	case "rust-analyzer":
		return "install with `rustup component add rust-analyzer`"
	default:
		return "install the " + binary + " language server and ensure it is on PATH"
	}
}
