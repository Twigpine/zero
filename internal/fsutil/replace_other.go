//go:build !windows

package fsutil

import "os"

// replaceExisting publishes src over dst. rename(2) already replaces the
// destination atomically within one filesystem on Unix, and it neither creates
// nor consults an ACL, so there is nothing extra to preserve here.
func replaceExisting(src, dst string) error {
	return os.Rename(src, dst)
}

// publishExclusive publishes src as a new dst without replacing anything.
// link(2) is atomic and fails with EEXIST when dst exists, so a destination
// that appeared concurrently is refused rather than overwritten. The caller
// removes the now-duplicated temporary afterwards.
func publishExclusive(src, dst string) error {
	return os.Link(src, dst)
}
