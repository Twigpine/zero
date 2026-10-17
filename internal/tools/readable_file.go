package tools

import (
	"errors"
	"os"
)

func openReadableRegularFile(path string) (*os.File, error) {
	file, err := openReadableFile(path)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, errors.New("not a regular file")
	}
	return file, nil
}
