//go:build windows

package observability

import (
	"errors"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

type fileRenameInformation struct {
	replaceIfExists uint32
	rootDirectory   windows.Handle
	fileNameLength  uint32
	fileName        [1]uint16
}

func renameNoReplace(root *os.Root, oldname, newname string) (returnErr error) {
	directory, err := root.Open(".")
	if err != nil {
		return err
	}
	defer directory.Close()
	raw, err := directory.SyscallConn()
	if err != nil {
		return err
	}
	if err := raw.Control(func(rawHandle uintptr) {
		rootHandle := windows.Handle(rawHandle)
		oldObjectName, nameErr := windows.NewNTUnicodeString(oldname)
		if nameErr != nil {
			returnErr = nameErr
			return
		}
		attributes := &windows.OBJECT_ATTRIBUTES{
			Length:        uint32(unsafe.Sizeof(windows.OBJECT_ATTRIBUTES{})),
			RootDirectory: rootHandle,
			ObjectName:    oldObjectName,
			Attributes:    windows.OBJ_CASE_INSENSITIVE | windows.OBJ_DONT_REPARSE,
		}
		var source windows.Handle
		var status windows.IO_STATUS_BLOCK
		returnErr = windows.NtCreateFile(
			&source,
			windows.DELETE|windows.SYNCHRONIZE,
			attributes,
			&status,
			nil,
			0,
			windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
			windows.FILE_OPEN,
			windows.FILE_NON_DIRECTORY_FILE|windows.FILE_SYNCHRONOUS_IO_NONALERT|windows.FILE_OPEN_REPARSE_POINT,
			0,
			0,
		)
		if returnErr != nil {
			return
		}
		defer windows.CloseHandle(source)

		newName, nameErr := windows.UTF16FromString(newname)
		if nameErr != nil {
			returnErr = nameErr
			return
		}
		nameBytes := (len(newName) - 1) * 2
		var layout fileRenameInformation
		bufferSize := int(unsafe.Offsetof(layout.fileName)) + nameBytes
		buffer := make([]byte, bufferSize)
		info := (*fileRenameInformation)(unsafe.Pointer(&buffer[0]))
		info.rootDirectory = rootHandle
		info.fileNameLength = uint32(nameBytes)
		copy((*[windows.MAX_LONG_PATH]uint16)(unsafe.Pointer(&info.fileName[0]))[:nameBytes/2:nameBytes/2], newName)
		returnErr = windows.NtSetInformationFile(source, &status, &buffer[0], uint32(bufferSize), windows.FileRenameInformation)
	}); err != nil {
		return err
	}
	if errors.Is(returnErr, windows.STATUS_OBJECT_NAME_COLLISION) || errors.Is(returnErr, windows.STATUS_OBJECT_NAME_EXISTS) {
		return os.ErrExist
	}
	return returnErr
}
