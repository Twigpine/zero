//go:build windows

package sandbox

import (
	"fmt"

	"golang.org/x/sys/windows"
)

// runtimeDirIdentity identifies the directory currently at path, EAGERLY.
//
// os.SameFile cannot be used for this on Windows. A Windows fileStat loads its
// volume serial and file index lazily, by PATHNAME, at comparison time, so an
// identity captured before a replacement and compared afterwards reports the
// substitute as the same object and the original as a different one: exactly
// backwards, and silently. Measured, not reasoned about.
//
// Reading the identity through a handle at capture time removes the lazy step.
// Opened with FILE_FLAG_OPEN_REPARSE_POINT so a link substituted at the final
// component is identified as the link it is rather than followed.
func runtimeDirIdentity(path string) (string, bool) {
	utf16Path, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return "", false
	}
	handle, err := windows.CreateFile(
		utf16Path,
		windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return "", false
	}
	defer windows.CloseHandle(handle)
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return "", false
	}
	return fmt.Sprintf("%d:%d:%d", info.VolumeSerialNumber, info.FileIndexHigh, info.FileIndexLow), true
}
