// Test seams: helpers only test code uses, kept out of the production binary.
package sandbox

import (
	"os"
	"path/filepath"
	"strings"
)

func FormatGrantList(grants []Grant) string {
	return FormatGrantListWithCommandPrefixes(grants, nil)
}

// PinProcessCredentialBaseDir re-pins the directory relative credential
// overrides resolve against, and restores it when the test ends.
//
// Production pins this once during package initialization, which is what makes
// it equal the directory the token stores resolved against when they opened. A
// test that chdirs to simulate a process running elsewhere has to move the pin
// with it — re-reading the working directory per call is exactly the drift this
// seam exists to keep out of the production path.
func PinProcessCredentialBaseDir(t interface{ Cleanup(func()) }, dir string) {
	previous := processCredentialBaseDir
	processCredentialBaseDir = filepath.Clean(dir)
	t.Cleanup(func() { processCredentialBaseDir = previous })
}

func DefaultPermissionProfile(workspaceRoot string) PermissionProfile {
	return PermissionProfileFromPolicy(workspaceRoot, DefaultPolicy(), nil)
}

// writeRoots returns the full ordered write-root list for command plans:
// the workspace root plus any granted extra roots. The single-root fallback
// only applies to engines built without a workspace root (NewEngine always
// builds a scope otherwise); it is kept as defense in depth.
func (engine *Engine) writeRoots(workspaceRoot string) []string {
	var roots []string
	if engine.scope != nil {
		roots = engine.scope.Roots()
	} else {
		roots = []string{workspaceRoot}
	}
	// Reflect the policy's AllowWrite roots in the OS backend write binds so a
	// sandboxed shell command may write where the policy grants writes. DenyWrite
	// is enforced at the policy gate, and on sandbox-exec additionally as an
	// explicit deny rule (see sandboxExecProfile).
	policy := engine.effectivePolicy(engine.policy)
	if extra := resolveWriteRootPaths(policy.AllowWrite); len(extra) > 0 {
		roots = dedupeStrings(append(roots, extra...))
	}
	return roots
}

func sandboxEnvironment(policy Policy, backend BackendName, workspaceRoot string) []string {
	return sandboxEnvironmentForCommand(nil, policy, backend, workspaceRoot)
}

func sandboxExecProfile(writeRoots []string, policy Policy, denialTag string) string {
	return seatbeltProfileFromPermissionProfile(seatbeltCompatibilityPermissionProfile(writeRoots, policy), policy, denialTag)
}

func seatbeltCompatibilityPermissionProfile(writeRoots []string, policy Policy) PermissionProfile {
	fs := FileSystemPolicy{
		Kind:                 FileSystemUnrestricted,
		ReadRoots:            []string{string(filepath.Separator)},
		IncludePlatformRoots: true,
		AllowTemp:            true,
	}
	if policy.EnforceWorkspace {
		fs.Kind = FileSystemRestricted
		fs.WriteRoots = make([]WritableRoot, 0, len(writeRoots))
		for _, root := range writeRoots {
			fs.WriteRoots = append(fs.WriteRoots, WritableRoot{Root: root})
		}
	}
	fs.DenyRead = normalizeProfilePaths(policy.DenyRead)
	credentials := finalizeCredentialDenyPaths(credentialDenyReadPaths(policy, "", os.Environ(), nil), fs.DenyRead)
	fs.DenyReadIfExists = credentials.Paths
	fs.MandatoryDenyReadPaths = credentials.MandatoryPaths
	fs.DenyReadCarveouts = credentials.Carveouts
	fs.EnsureDenyReadDirs = credentials.EnsureDirs
	fs.ProcessTrustedDenyReadFiles = credentials.ProcessTrustedFinalFiles
	fs.DenyWrite = normalizeProfilePaths(policy.DenyWrite)
	return PermissionProfile{
		FileSystem: fs,
		Network:    NetworkPolicy{Mode: policy.Network},
	}
}

// WindowsSandboxSetupPathForRunner derives the setup helper's path from a
// standalone command-runner path (the sibling .exe in the release layout).
// Retained for that layout; self-dispatch callers use
// ResolveWindowsSandboxSetupHelper instead.
func WindowsSandboxSetupPathForRunner(runnerPath string) string {
	if strings.TrimSpace(runnerPath) == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(runnerPath), WindowsSandboxSetupName)
}
