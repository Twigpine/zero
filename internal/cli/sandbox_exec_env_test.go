package cli

import (
	"context"
	"os"
	"runtime"
	"strings"
	"testing"

	zeroSandbox "github.com/Twigpine/zero/internal/sandbox"
)

// envPrinterPlan returns a plan whose child prints its own environment, so the
// assertion is about what the child received rather than about a field value.
func envPrinterPlan(t *testing.T, env []string) zeroSandbox.CommandPlan {
	t.Helper()
	plan := zeroSandbox.CommandPlan{Dir: t.TempDir(), Env: env}
	if runtime.GOOS == "windows" {
		plan.Name = "cmd.exe"
		plan.Args = []string{"/c", "set"}
		return plan
	}
	plan.Name = "/bin/sh"
	plan.Args = []string{"-c", "env"}
	return plan
}

// A PLAN THAT SPECIFIES NO VARIABLES MUST NOT INHERIT EVERY VARIABLE.
//
// exec.Cmd reads a nil Env as "inherit this process's entire environment", which
// is a different statement from "run with no variables". The plan owns its
// environment: directCommandEnv and scrubSensitiveEnv return a slice they built,
// and that slice is non-nil with length zero when every entry was sensitive.
// Testing its length collapsed the two states and turned the strictest possible
// answer into the loosest one.
//
// Not reachable through `zero sandbox exec` today, because the child environment
// is os.Environ() and an environment holding only sensitive keys has no
// %AppData%, so config resolution fails before the planner runs. Pinned anyway,
// because the next caller that hands the plan a deliberately narrow environment
// would silently inherit everything instead.
func TestAnEmptyPlannedEnvironmentDoesNotInherit(t *testing.T) {
	const marker = "ZZ_SANDBOX_ENV_MARKER"
	t.Setenv(marker, "leaked-value")

	var out strings.Builder
	// Specified, and deliberately empty.
	code := runSandboxPlannedCommand(context.Background(), envPrinterPlan(t, []string{}), &out, os.Stderr)
	if code != 0 {
		t.Fatalf("child exited %d: %s", code, out.String())
	}
	if strings.Contains(out.String(), marker) {
		t.Fatalf("a plan specifying no environment leaked the caller's %s to the child:\n%s", marker, out.String())
	}
}

// And a nil environment still inherits, which is the meaning the planner relies
// on when it did not build one. Without this the fix above could be "never pass
// the environment", which would break every ordinary command.
func TestANilPlannedEnvironmentStillInherits(t *testing.T) {
	const marker = "ZZ_SANDBOX_ENV_MARKER"
	t.Setenv(marker, "inherited-value")

	var out strings.Builder
	code := runSandboxPlannedCommand(context.Background(), envPrinterPlan(t, nil), &out, os.Stderr)
	if code != 0 {
		t.Fatalf("child exited %d: %s", code, out.String())
	}
	if !strings.Contains(out.String(), marker) {
		t.Fatalf("a nil planned environment stopped inheriting, which changes the meaning the planner relies on:\n%s", out.String())
	}
}
