//go:build !darwin && !linux && !windows

package observability

import (
	"errors"
	"os"
)

func renameNoReplace(*os.Root, string, string) error {
	return errors.ErrUnsupported
}
