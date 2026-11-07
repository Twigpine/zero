//go:build windows

package sandbox

// acquireRuntimeLeaseForPlatform routes Windows through the rooted, no-follow
// acquisition. See acquireRuntimeLeaseRooted for why the pathname walk it
// replaces was unsafe.
func acquireRuntimeLeaseForPlatform(root string) (*sandboxRuntimeLease, []windowsCreatedRuntimeDir, error) {
	return acquireRuntimeLeaseRooted(root)
}
