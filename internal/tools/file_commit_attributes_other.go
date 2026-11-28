//go:build !windows

package tools

import "os"

// Only Windows keeps the presentation of a file in attributes that a rename
// leaves behind. Elsewhere there is nothing here to carry.

func carriedFileAttributes(os.FileInfo) *uint32 { return nil }

func applyFileAttributes(*os.File, *uint32) error { return nil }
