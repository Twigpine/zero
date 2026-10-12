//go:build windows

package cron

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// Compacting runs.jsonl publishes a temporary file carrying the job directory's
// inherited DACL. A rename would replace a restrictive DACL applied to the log
// itself, so AppendRun must keep the log's own descriptor.
func TestAppendRunPreservesHistoryDACL(t *testing.T) {
	store := newTestStore(t)
	job, err := store.Add(Job{Prompt: "x"})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(store.jobDir(job.ID), "runs.jsonl")
	if err := os.WriteFile(path, []byte("{\"exitCode\":7}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A protected DACL granting only the owner: distinct from whatever the
	// temporary file inherits from the directory.
	restricted, err := windows.SecurityDescriptorFromString("D:P(A;;FA;;;OW)")
	if err != nil {
		t.Skipf("cannot build a test security descriptor: %v", err)
	}
	dacl, _, err := restricted.DACL()
	if err != nil {
		t.Skipf("cannot read the test DACL: %v", err)
	}
	if err := windows.SetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, dacl, nil,
	); err != nil {
		t.Skipf("cannot apply a restrictive DACL on this filesystem: %v", err)
	}
	want := describeDACL(t, path)
	if inherited := describeDACL(t, store.jobDir(job.ID)); strings.Contains(inherited, want) {
		t.Skip("the job directory already carries the same DACL; this filesystem cannot show the difference")
	}

	if err := store.AppendRun(job.ID, RunRecord{ExitCode: 42}); err != nil {
		t.Fatal(err)
	}
	if got := describeDACL(t, path); got != want {
		t.Fatalf("DACL after AppendRun = %q, want the log's own %q", got, want)
	}
	runs, err := store.Runs(job.ID, 10)
	if err != nil || len(runs) != 2 || runs[1].ExitCode != 42 {
		t.Fatalf("outcome not published by replacement: %+v, %v", runs, err)
	}
}

func describeDACL(t *testing.T, path string) string {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Skipf("cannot read the security descriptor of %s: %v", path, err)
	}
	text := sd.String()
	if index := strings.Index(text, "D:"); index >= 0 {
		return text[index:]
	}
	return text
}
