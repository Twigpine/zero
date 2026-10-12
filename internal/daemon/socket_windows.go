//go:build windows

package daemon

import "os"

// hardenSocketFile is a no-op on Windows: AF_UNIX socket files do not honor POSIX
// mode bits, and the daemon places the socket under the per-user profile
// directory, which is already ACL-restricted to the owner. A dedicated restricted
// ACL (or a named pipe with an explicit DACL) is a documented follow-up.
func hardenSocketFile(string) error { return nil }

func hardenSocketFileRoot(*os.Root, string) error { return nil }
