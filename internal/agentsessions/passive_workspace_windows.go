package agentsessions

import "golang.org/x/sys/windows"

func windowsDriveIsLocal(root string) bool {
	pointer, err := windows.UTF16PtrFromString(root)
	if err != nil {
		return false
	}
	switch windows.GetDriveType(pointer) {
	case windows.DRIVE_FIXED, windows.DRIVE_REMOVABLE, windows.DRIVE_CDROM, windows.DRIVE_RAMDISK:
		return true
	default:
		return false
	}
}
