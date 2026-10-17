//go:build windows

package sandbox

import "errors"

var errNoRootedStampWriter = errors.New("no rooted stamp writer on this platform")

func writeRuntimeStampThroughHandle(root string, planHash string) error {
	return writeWindowsRuntimeStampThroughHandle(root, planHash)
}
