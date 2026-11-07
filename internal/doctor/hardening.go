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
		SandboxHome:       sandboxHome,
		CommandCWD:        workspaceRoot,
		WorkspaceRoots:    []string{workspaceRoot},
		PermissionProfile: profile,
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
