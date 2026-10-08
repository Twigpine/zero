//go:build windows

package update

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// TestCreateStagingFileRefusesPrecreatedHardLink is the regression test for
// #742: a lower-privileged attacker who can write in the installation
// directory pre-creates the staging path as a hard link to another file the
// (possibly elevated) updater can write. createStagingFile must fail instead
// of opening and truncating through that link.
func TestCreateStagingFileRefusesPrecreatedHardLink(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "victim")
	if err := os.WriteFile(victim, []byte("do not touch"), 0o644); err != nil {
		t.Fatalf("WriteFile victim: %v", err)
	}
	staged := filepath.Join(dir, "staged")
	if err := os.Link(victim, staged); err != nil {
		t.Fatalf("Link: %v", err)
	}

	if _, err := createStagingFile(staged); err == nil {
		t.Fatal("createStagingFile succeeded through a pre-existing hard link, want error")
	}

	data, err := os.ReadFile(victim)
	if err != nil {
		t.Fatalf("ReadFile victim: %v", err)
	}
	if string(data) != "do not touch" {
		t.Fatalf("victim content = %q, want unchanged", data)
	}
}

// TestCreateStagingFileRefusesPrecreatedSymlink covers the reparse-point
// variant of the same #742 primitive. Creating a file symlink on Windows
// needs SeCreateSymbolicLinkPrivilege (admin) or Developer Mode; skip rather
// than fail where that isn't available.
func TestCreateStagingFileRefusesPrecreatedSymlink(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "victim")
	if err := os.WriteFile(victim, []byte("do not touch"), 0o644); err != nil {
		t.Fatalf("WriteFile victim: %v", err)
	}
	staged := filepath.Join(dir, "staged")
	if err := os.Symlink(victim, staged); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	if _, err := createStagingFile(staged); err == nil {
		t.Fatal("createStagingFile succeeded through a pre-existing symlink, want error")
	}

	data, err := os.ReadFile(victim)
	if err != nil {
		t.Fatalf("ReadFile victim: %v", err)
	}
	if string(data) != "do not touch" {
		t.Fatalf("victim content = %q, want unchanged", data)
	}
}

// TestCreateStagingFileSucceedsForFreshPath is the control: a path with
// nothing pre-existing must still work normally.
func TestCreateStagingFileSucceedsForFreshPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "staged")

	file, err := createStagingFile(path)
	if err != nil {
		t.Fatalf("createStagingFile: %v", err)
	}
	if _, err := file.WriteString("payload"); err != nil {
		t.Fatalf("WriteString: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != "payload" {
		t.Fatalf("content = %q, want %q", data, "payload")
	}
}

// TestCreateStagingFileConcurrentRaceOnlyOneWinner exercises a race on a
// single fixed path (installBinary normally avoids this by randomizing the
// name, but the exclusive-creation guarantee must hold regardless): exactly
// one concurrent caller may create the file, and the rest must fail cleanly
// rather than silently truncate the winner's content.
func TestCreateStagingFileConcurrentRaceOnlyOneWinner(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "staged")

	const attempts = 16
	var wg sync.WaitGroup
	successes := make([]bool, attempts)
	for i := range attempts {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			file, err := createStagingFile(path)
			if err != nil {
				return
			}
			defer func() { _ = file.Close() }()
			successes[i] = true
		}(i)
	}
	wg.Wait()

	winners := 0
	for _, ok := range successes {
		if ok {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("concurrent createStagingFile winners = %d, want exactly 1", winners)
	}
}
