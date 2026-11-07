package cli

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/Twigpine/zero/internal/sandbox"
)

// THE TEST BINARY ANSWERS THE SANDBOX'S SELF-DISPATCH LIKE THE REAL BINARY.
// The sandbox launches os.Executable() with a hidden helper subcommand; under a
// test that runs this binary as the zero CLI, that is this binary. Unrouted, the
// "helper" ran the entire package suite instead, and the sandboxed command that
// launched it hung until killed.
//
// Each hidden subcommand, given no helper arguments, must come straight back
// with the helper's own usage failure — never a test run.
func TestTheTestBinaryRoutesSandboxSelfDispatch(t *testing.T) {
	if os.Getenv("ZERO_TEST_SELF_DISPATCH_CHILD") == "1" {
		// Reached only when routing is broken and the child is running the
		// suite; skip so the failure is the parent's timeout, not a recursion.
		t.Skip("self-dispatch child")
	}
	for _, subcommand := range []string{
		sandbox.WindowsCommandRunnerSubcommand,
		sandbox.WindowsSandboxSetupSubcommand,
		sandbox.LinuxSandboxHelperSubcommand,
	} {
		t.Run(subcommand, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], subcommand)
			cmd.Env = append(os.Environ(), "ZERO_TEST_SELF_DISPATCH_CHILD=1")
			output, err := cmd.CombinedOutput()
			if ctx.Err() != nil {
				t.Fatalf("%s did not return: the test binary ran its suite instead of the helper", subcommand)
			}
			if err == nil {
				t.Fatalf("%s with no helper arguments succeeded; expected the helper's usage failure:\n%s", subcommand, output)
			}
			if text := string(output); strings.Contains(text, "=== RUN") || strings.Contains(text, "\nPASS") || strings.Contains(text, "--- SKIP") {
				t.Fatalf("%s ran tests instead of the helper:\n%s", subcommand, text)
			}
		})
	}
}
