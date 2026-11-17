//go:build windows

package sandbox

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Twigpine/zero/internal/execution"
)

// A REPORT THAT SAYS A CHILD LAUNCHED HAS TO MEAN THE CHILD COULD RUN.
//
// The report is published before ResumeThread so the inherited-pipe race is
// closed, and that ordering is right. But a failure between the publish and the
// resume reaps a process that executed nothing, and the report was left on disk
// saying true. AppliedEnforcementNotices gates on that report, so the operator
// would have been told a write-jail trade applied to a child that never became
// runnable. The record has to be unwound on that path, not only on the ones
// before it was written.
// AND IT HAS TO SAY SO, RATHER THAN SAY NOTHING. Removing the file leaves the
// parent reading absence, which is also what a normal cleanup leaves, so a
// reader that saw the publication during the window keeps its positive through
// completion. The retraction states the negative instead.
func TestResumeFailureRetractsTheLaunchReport(t *testing.T) {
	path := filepath.Join(t.TempDir(), "report.json")
	report, err := openWindowsExecutionReport(path)
	if err != nil {
		t.Fatal(err)
	}

	keep, err := publishThenResume(report, func() error { return errors.New("STATUS_ACCESS_DENIED") })
	if err == nil {
		t.Fatal("SETUP INVALID: the injected resume failure was not reported")
	}
	if !keep {
		t.Fatal("publishThenResume discarded the report after its resume failed, so the parent reads absence and a live reader keeps the launch it already saw")
	}
	report.close(keep)

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the retracted report is not readable: %v", err)
	}
	var decoded execution.AdapterReport
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("decode report %q: %v", data, err)
	}
	if decoded.ChildLaunched == nil {
		t.Fatalf("report = %s, want an explicit childLaunched false; silence does not revoke a launch a live reader already latched", data)
	}
	if *decoded.ChildLaunched {
		t.Fatalf("report = %s, want childLaunched false for a child that executed no instruction", data)
	}
}

// A retraction that cannot be written falls back to discarding the file, which
// is where this path was before: absence is weaker than an explicit false, and
// still better than a report left saying true.
func TestAnUnwritableRetractionDiscardsTheReport(t *testing.T) {
	path := filepath.Join(t.TempDir(), "report.json")
	report, err := openWindowsExecutionReport(path)
	if err != nil {
		t.Fatal(err)
	}

	keep, err := publishThenResume(report, func() error {
		// Close the handle underneath the retraction, so its write fails the
		// way a broken report file would.
		_ = report.file.Close()
		return errors.New("STATUS_ACCESS_DENIED")
	})
	if err == nil {
		t.Fatal("SETUP INVALID: the injected resume failure was not reported")
	}
	if keep {
		t.Fatal("publishThenResume kept a report it could not retract, so the file still says a child launched")
	}
	report.close(keep)

	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatalf("a report that could not be retracted survived (stat: %v); the parent would read a child as launched that executed no instruction", statErr)
	}
}

// And the ordinary path still publishes exactly what the parent relies on.
func TestSuccessfulResumeKeepsTheLaunchReport(t *testing.T) {
	path := filepath.Join(t.TempDir(), "report.json")
	report, err := openWindowsExecutionReport(path)
	if err != nil {
		t.Fatal(err)
	}

	keep, err := publishThenResume(report, func() error { return nil })
	if err != nil || !keep {
		t.Fatalf("publishThenResume = (%v, %v), want (true, nil)", keep, err)
	}
	report.close(keep)

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the launch report is gone after a successful resume: %v", err)
	}
	var decoded execution.AdapterReport
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("decode report: %v", err)
	}
	if decoded.ChildLaunched == nil || !*decoded.ChildLaunched {
		t.Fatalf("report = %s, want childLaunched true", data)
	}
}
