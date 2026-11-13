//go:build !windows

package sandbox

// acquireRuntimeLeaseForPlatform creates and locks the lease through the same
// rooted no-follow boundary the Windows side uses.
//
// A PRE-CHECK IS NOT A BOUNDARY. This used to call refuseAliasedRuntimeComponents
// and then os.MkdirAll plus a pathname lease open. The guard answers about an
// ABSENT component by saying there is nothing to alias, which is exactly the
// state a fresh fallback root is in, and both calls that followed resolve the
// name again. A link planted in between was therefore created through and
// written into, and the next check could only report it after the writes.
//
// The alias guard is gone from this path rather than kept alongside: retaining it
// would suggest the two together are the protection, when the descent is the
// protection and the guard is the thing that could not be one. Other callers
// still use it where a pathname really is all there is.
func acquireRuntimeLeaseForPlatform(root string) (*sandboxRuntimeLease, []windowsCreatedRuntimeDir, error) {
	return acquireRuntimeLeaseRootedUnix(root)
}
