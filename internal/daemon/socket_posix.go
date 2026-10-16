//go:build !windows

package daemon

import "os"

// hardenSocketFile restricts the control socket to its owner (0600) so no other
// local user can connect. Combined with the 0700 parent directory this makes the
// control plane owner-only, as required by the security model.
func hardenSocketFile(path string) error {
	return os.Chmod(path, 0o600)
}

func hardenSocketFileRoot(root *os.Root, name string) error {
	return root.Chmod(name, 0o600)
}
