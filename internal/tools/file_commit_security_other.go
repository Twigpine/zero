//go:build !windows

package tools

import "os"

// fileSecurity is the access control an overwrite carries from the file it
// replaces. Only Windows has anything to carry here: elsewhere the permission
// bits are the access control this package preserves, and the staged file is
// given them directly.
type fileSecurity struct{}

func captureFileSecurity(*os.File) (*fileSecurity, error) { return nil, nil }

func applyFileSecurity(*os.File, *fileSecurity) error { return nil }
