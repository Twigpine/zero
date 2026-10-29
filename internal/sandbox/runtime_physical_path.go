//go:build !windows

package sandbox

// physicalSandboxPath resolves path to the spelling the filesystem itself uses.
//
// Off Windows that is what canonicalSandboxWorkspaceRoot already does:
// filepath.EvalSymlinks follows every symlink, and there is no junction to
// follow. Two aliases remain unresolved and are handled by the identity walk in
// runtimeRootWithinWorkspace instead: a differing case on a case-insensitive
// volume, which EvalSymlinks preserves, and a bind mount, which no userspace
// path API resolves because the kernel deliberately presents it as a real path.
func physicalSandboxPath(path string) string {
	return canonicalSandboxWorkspaceRoot(path)
}
