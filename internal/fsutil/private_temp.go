package fsutil

import (
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// privateCreationObserver observes the object immediately after the creation
// syscall, before any metadata changes or content writes. Tests are serial.
var privateCreationObserver func(string)

// CreatePrivateTemp creates an owner-only staging file, suppressing effective
// inherited grants in the creation syscall. The caller must close and remove it.
func CreatePrivateTemp(dir, pattern string) (*os.File, error) {
	var file *os.File
	_, err := createPrivateTemp(dir, pattern, func(path string) error {
		var err error
		file, err = createPrivateFile(path)
		return err
	})
	return file, err
}

// CreatePrivateTempDir isolates formatter rewrites and auxiliary files as well
// as the initial copy. The caller must remove the directory after the child exits.
func CreatePrivateTempDir(dir, pattern string) (string, error) {
	return createPrivateTemp(dir, pattern, createPrivateDir)
}

func createPrivateTemp(dir, pattern string, create func(string) error) (string, error) {
	if strings.ContainsAny(pattern, `/\`) {
		return "", errors.New("fsutil: invalid temporary pattern")
	}
	prefix, suffix := pattern, ""
	if index := strings.LastIndexByte(pattern, '*'); index >= 0 {
		prefix, suffix = pattern[:index], pattern[index+1:]
	}
	for range 10000 {
		name := filepath.Join(dir, prefix+rand.Text()+suffix)
		err := create(name)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		if privateCreationObserver != nil {
			privateCreationObserver(name)
		}
		return name, nil
	}
	return "", errors.New("fsutil: temporary name collisions")
}
