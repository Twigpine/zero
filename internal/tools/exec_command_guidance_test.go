package tools

import (
	"runtime"
	"strings"
	"testing"
)

// THE BUDGET TEST REMOVES THIS TEXT, SO IT HAS TO BE EXACTLY WHAT IS ADDED.
//
// internal/agent's eager-schema ratchet charges the tool schemas and this
// host's shell guidance separately, and it separates them by stripping
// HostExecCommandShellGuidance off exec_command's description before
// estimating. That is only valid while the guidance is appended verbatim as the
// tail of that description, and only while a platform that reports no guidance
// really appends none. If the two drift apart the ratchet keeps reporting a
// number, just not the one it claims.
//
// Checked for all three platforms rather than for the running one: the
// appending is itself conditional on GOOS, so a host-only check is the shape of
// bug this pins. Off Windows it would pass while reporting nothing at all.
func TestExecCommandDescriptionMatchesReportedGuidance(t *testing.T) {
	for _, goos := range []string{"windows", "linux", "darwin"} {
		t.Run(goos, func(t *testing.T) {
			description := execCommandDescription(goos)
			guidance := hostExecCommandShellGuidance(goos)

			if goos == "windows" {
				if guidance == "" {
					t.Fatal("exec_command appends shell guidance on Windows, but the accessor reports none; the budget would charge it to the schemas")
				}
				if !strings.HasSuffix(description, guidance) {
					t.Fatalf("the guidance is not the tail of the description, so removing it from the schema total is wrong.\nguidance: %q\ndescription tail: %q",
						guidance, description[max(0, len(description)-len(guidance)-40):])
				}
			} else if guidance != "" {
				t.Fatalf("no shell guidance is appended off Windows, but the accessor reported %q", guidance)
			}

			// Whatever guidance THIS platform would produce, none of it may be
			// left in the description once the reported guidance is removed.
			// Off Windows the accessor reports nothing, so this is the check
			// that the description really carries nothing: drop the GOOS
			// condition in execCommandDescription and a POSIX host starts
			// appending its own guidance (about 150 characters on darwin) with
			// the ratchet still charging every byte of it to the schemas.
			platformGuidance := shellGuidanceForGOOS(goos)
			if platformGuidance == "" {
				t.Fatalf("SETUP INVALID: %s produces no shell guidance at all, so the check below cannot fail", goos)
			}
			if remainder := strings.TrimSuffix(description, guidance); strings.Contains(remainder, platformGuidance) {
				t.Fatalf("exec_command's description still carries %s shell guidance after removing the %d characters the accessor reports, so the ratchet charges the rest to the tool schemas.\nremainder: %q",
					goos, len(guidance), remainder)
			}
		})
	}
}

// And the running host agrees with the table above, so the exported accessor
// and the real constructor cannot drift away from the platform contract.
func TestHostExecCommandShellGuidanceMatchesTheDescription(t *testing.T) {
	tool := NewExecCommandTool(t.TempDir(), nil)
	if got, want := tool.Description(), execCommandDescription(runtime.GOOS); got != want {
		t.Fatalf("the constructed tool's description is not what the platform contract says.\ngot:  %q\nwant: %q", got, want)
	}
	if got, want := HostExecCommandShellGuidance(), hostExecCommandShellGuidance(runtime.GOOS); got != want {
		t.Fatalf("the exported accessor disagrees with the platform contract.\ngot:  %q\nwant: %q", got, want)
	}
}

// And the guidance a Windows host gets is never empty, whichever shell was
// detected, since an empty one would mean the model gets no syntax rule at all.
func TestWindowsShellGuidanceIsNeverEmpty(t *testing.T) {
	for _, shell := range []shellRuntime{
		{GOOS: "windows", Executable: `C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`, Kind: shellKindPowerShell, Syntax: "PowerShell"},
		{GOOS: "windows", Executable: `C:\Program Files\PowerShell\7\pwsh.exe`, Kind: shellKindPowerShell, Syntax: "PowerShell"},
		{GOOS: "windows", Executable: `C:\Windows\System32\cmd.exe`, Kind: shellKindCmd, Syntax: "cmd.exe"},
	} {
		if guidance := shellGuidanceForRuntime(shell); strings.TrimSpace(guidance) == "" {
			t.Errorf("%s produced no shell guidance", shell.Executable)
		}
	}
}
