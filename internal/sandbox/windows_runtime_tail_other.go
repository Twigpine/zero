//go:build !windows

package sandbox

import (
	"errors"
	"os"
)

// errNoRootedStampWriter marks the platforms with no rooted traversal. The
// runtime stamp is a Windows concept; the code that writes it is shared only so
// its tests run everywhere.
var errNoRootedStampWriter = errors.New("no rooted stamp writer on this platform")

func writeRuntimeStampThroughHandle(string, string) error {
	return errNoRootedStampWriter
}

// readWindowsSandboxRuntimeStampFile is a plain read off Windows, where no
// publish contends with it.
func readWindowsSandboxRuntimeStampFile(path string) ([]byte, error) {
	return os.ReadFile(path)
}
