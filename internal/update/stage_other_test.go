//go:build !windows

package update

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// stageAtPath drives the production staging primitive — createStagingFileAt,
// the one createStagedBinary calls — against path by binding a descriptor to
// its parent first. The tests below go through it rather than a path-taking
// helper of their own so that weakening the real open flags fails them.
func stageAtPath(path string) (*os.File, error) {
	parent, err := os.Open(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	defer func() { _ = parent.Close() }()
	return createStagingFileAt(parent, filepath.Base(path), path)
}

// TestCreateStagingFileRefusesPrecreatedHardLink is the regression test for
// #742: a lower-privileged attacker who can write in the installation
// directory pre-creates the staging path as a hard link to another file the
// (possibly elevated) updater can write. Staging must fail instead of opening
// and truncating through that link.
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

	if _, err := stageAtPath(staged); err == nil {
		t.Fatal("staging succeeded through a pre-existing hard link, want error")
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
// variant of the same #742 primitive: the staging path pre-created as a
// symlink to another writable file.
func TestCreateStagingFileRefusesPrecreatedSymlink(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "victim")
	if err := os.WriteFile(victim, []byte("do not touch"), 0o644); err != nil {
		t.Fatalf("WriteFile victim: %v", err)
	}
	staged := filepath.Join(dir, "staged")
	if err := os.Symlink(victim, staged); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	if _, err := stageAtPath(staged); err == nil {
		t.Fatal("staging succeeded through a pre-existing symlink, want error")
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

	file, err := stageAtPath(path)
	if err != nil {
		t.Fatalf("stageAtPath: %v", err)
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
			file, err := stageAtPath(path)
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
		t.Fatalf("concurrent staging winners = %d, want exactly 1", winners)
	}
}
