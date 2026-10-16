//go:build !windows

package daemon

import "os"

func prepareStatusReplacement(*os.Root, string, string) error { return nil }
