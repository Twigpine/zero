package agentsessions

import (
	"runtime"
	"strings"
)

// PassiveWorkspaceIsLocal allows alias/loadability checks only for a local
// Windows drive. Foreign UNC, device, relative and remote mapped-drive paths
// must not cause filesystem/network I/O while merely listing imported history.
// Explicitly selecting a session for activation has its own validation path.
func PassiveWorkspaceIsLocal(value string) bool {
	if runtime.GOOS != "windows" {
		return true
	}
	return passiveWindowsWorkspaceIsLocal(value, windowsDriveIsLocal)
}

func passiveWindowsWorkspaceIsLocal(value string, localDrive func(string) bool) bool {
	value = strings.ReplaceAll(value, "/", `\`)
	value = strings.TrimPrefix(value, `\\?\`)
	// Extended UNC/device namespaces do not become drive-rooted paths after
	// removing the extended-path introducer, so they are refused here as well.
	if len(value) < 3 || value[1] != ':' || value[2] != '\\' ||
		!strings.ContainsRune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ", rune(value[0])) {
		return false
	}
	return localDrive(value[:3])
}
