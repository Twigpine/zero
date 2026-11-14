package cli

import (
	"os"
	"testing"

	"github.com/Twigpine/zero/internal/sandbox"
)

// TestMain lets this package's test binary stand in for the zero binary as its
// own sandbox helper.
//
// THE SANDBOX SELF-DISPATCHES. With no standalone helper beside the running
// executable, both sandboxes launch os.Executable() with a hidden subcommand
// (__windows-command-runner, __windows-sandbox-setup, __sandbox-helper). In a
// test that runs this binary as the zero CLI — TestLocalAdoptionSavesAndReloads-
// EligibleModel does — os.Executable() is the test binary. Without this routing
// it treated the hidden subcommand as nothing at all and ran the WHOLE package
// suite as the "helper", so every sandboxed command in that child hung until it
// was killed. cmd/zero/main.go and runWithDeps route these before anything else;
// this does the same, in the same order.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case sandbox.LinuxSandboxHelperSubcommand:
			os.Exit(sandbox.RunLinuxSandboxHelper(os.Args[1:], os.Stderr))
		case sandbox.WindowsCommandRunnerSubcommand, sandbox.WindowsSandboxSetupSubcommand:
			os.Exit(runWithDeps(os.Args[1:], os.Stdout, os.Stderr, appDeps{}))
		}
	}
	os.Exit(m.Run())
}
