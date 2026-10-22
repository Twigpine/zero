//go:build !windows

package cli

import (
	"errors"
	"os/exec"
	"syscall"
	"testing"
)

// A SIGNALED CHILD MUST NOT LOOK LIKE ONE THAT CHOSE TO EXIT 255.
//
// `sandbox exec` exists to report the child's own status faithfully, and
// exec.ExitError cannot represent signal termination: ExitCode() answers -1, and
// the top level hands that to os.Exit, which truncates it to 255. So a child
// killed by SIGTERM was indistinguishable from an ordinary exit of 255, and a
// harness comparing statuses could not tell a refusal from a kill.
//
// Driven with a real subprocess and a real signal rather than a synthetic
// WaitStatus, because the mapping is only correct if the OS agrees with it.
func TestSignaledChildReportsTheConventionalStatus(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		script string
		want   int
	}{
		{name: "SIGTERM", script: "kill -TERM $$; sleep 5", want: 128 + int(syscall.SIGTERM)},
		{name: "SIGINT", script: "kill -INT $$; sleep 5", want: 128 + int(syscall.SIGINT)},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			err := exec.Command("/bin/sh", "-c", testCase.script).Run()
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) {
				t.Fatalf("SETUP INVALID: the child did not end with an ExitError: %v", err)
			}
			// The behaviour being corrected: the integer alone cannot say this.
			if code := exitErr.ExitCode(); code != -1 {
				t.Fatalf("SETUP INVALID: a signaled child reported exit code %d, expected -1 on this platform", code)
			}
			status, signaled := signaledExitStatus(exitErr.ProcessState)
			if !signaled {
				t.Fatal("a signaled child was not recognised as signaled, so it would be reported as exit 255")
			}
			if status != testCase.want {
				t.Fatalf("status = %d, want %d (128 + signal), which is what a shell reports", status, testCase.want)
			}
		})
	}
}

// An ordinary non-zero exit is untouched: it has a real code and must keep it.
func TestOrdinaryExitIsNotTreatedAsSignaled(t *testing.T) {
	err := exec.Command("/bin/sh", "-c", "exit 3").Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("SETUP INVALID: the child did not end with an ExitError: %v", err)
	}
	if _, signaled := signaledExitStatus(exitErr.ProcessState); signaled {
		t.Fatal("an ordinary exit was reported as signaled, which would rewrite its status")
	}
	if code := exitErr.ExitCode(); code != 3 {
		t.Fatalf("exit code = %d, want the child's own 3", code)
	}
}
