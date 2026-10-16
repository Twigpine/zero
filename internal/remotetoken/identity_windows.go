//go:build windows

package remotetoken

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

func fileIdentity(os.FileInfo) (string, bool) { return "", false }

func openFileIdentity(file *os.File) (string, bool) {
	if file == nil {
		return "", false
	}
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(file.Fd()), &info); err != nil {
		return "", false
	}
	return fmt.Sprintf("windows:%08x:%08x%08x", info.VolumeSerialNumber, info.FileIndexHigh, info.FileIndexLow), true
}
