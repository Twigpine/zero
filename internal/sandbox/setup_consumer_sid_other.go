//go:build !windows

package sandbox

// currentProcessSID has no meaning off Windows. Windows sandbox setup args are
// only ever built on Windows; this exists so the shared builder compiles.
func currentProcessSID() (string, error) {
	return "", nil
}
