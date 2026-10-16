package tui

import (
	"fmt"
	"os"
	"testing"

	"github.com/Gitlawb/zero/internal/testutil"
)

// isolatedUserDirsRoot is where TestMain points every per-user directory.
var isolatedUserDirsRoot string

// TestMain keeps the whole package off the developer's real home, config and
// cache directories. newModel loads the user's slash commands from the config
// dir, so without this every test sees (and could write to) real user state.
func TestMain(m *testing.M) {
	root, err := os.MkdirTemp("", "zero-tui-test-home-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "tui tests: create isolated user dirs:", err)
		os.Exit(1)
	}
	isolatedUserDirsRoot = root
	restore, err := testutil.IsolateUserDirs(root)
	if err != nil {
		fmt.Fprintln(os.Stderr, "tui tests: isolate user dirs:", err)
		_ = os.RemoveAll(root)
		os.Exit(1)
	}
	code := m.Run()
	if err := restore(); err != nil {
		fmt.Fprintln(os.Stderr, "tui tests: restore user dirs:", err)
		code = 1
	}
	if err := os.RemoveAll(root); err != nil {
		fmt.Fprintln(os.Stderr, "tui tests: remove isolated user dirs:", err)
		code = 1
	}
	os.Exit(code)
}
