//go:build !unix

package planmode

import "fmt"

func mkfifoForTest(path string) error {
	return fmt.Errorf("mkfifo not available on this platform")
}
