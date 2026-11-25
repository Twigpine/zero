//go:build !windows

package agentsessions

func windowsDriveIsLocal(string) bool { return false }
