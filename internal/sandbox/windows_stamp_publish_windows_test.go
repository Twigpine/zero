//go:build windows

package sandbox

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// A STAMP IS REPLACED, NEVER REWRITTEN IN PLACE.
//
// Commands and zero doctor read the stamp without the setup lock. The writer
// used to truncate the existing stamp at open time and write the new contents
// afterwards, and rollback deleted the stamp before recreating it, so a reader
// in either interval saw an empty, partial or missing stamp on a setup that was
// still valid, and a process that stopped there left it that way. Reported by
// @jatmn.

func stampPublishTestContents(label string) string {
	return label + strings.Repeat("0123456789abcdef", 4)
}

func stampEntries(t *testing.T, root string) (stamps int, replacements []string) {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("list the runtime root: %v", err)
	}
	name := windowsSandboxRuntimeStampName(testStampPlanHash)
	for _, entry := range entries {
		switch {
		case entry.Name() == name:
			stamps++
		case strings.HasPrefix(entry.Name(), name):
			replacements = append(replacements, entry.Name())
		}
	}
	return stamps, replacements
}

// Many republishes against a reader that never stops. Every read has to return
// one complete version or the other. Reads are counted, so a reader that never
// ran cannot pass this.
func TestStampRepublishIsNeverSeenPartialOrMissing(t *testing.T) {
	root := t.TempDir()
	directory := openStampTestDirectory(t, root)
	name := windowsSandboxRuntimeStampName(testStampPlanHash)
	path := filepath.Join(root, name)
	versions := []string{stampPublishTestContents("plan-a-"), stampPublishTestContents("plan-b-")}
	if err := writeWindowsRuntimeStampToDirectoryHandle(directory, name, versions[0]); err != nil {
		t.Fatalf("publish the first stamp: %v", err)
	}

	var (
		stop  atomic.Bool
		reads atomic.Int64
		mu    sync.Mutex
		bad   []string
		done  = make(chan struct{})
	)
	go func() {
		defer close(done)
		for !stop.Load() {
			data, err := readWindowsSandboxRuntimeStampFile(path)
			reads.Add(1)
			if err != nil || (string(data) != versions[0] && string(data) != versions[1]) {
				mu.Lock()
				if len(bad) < 5 {
					bad = append(bad, "read "+string(data)+" err="+errorText(err))
				}
				mu.Unlock()
			}
		}
	}()
	for index := 0; index < 300; index++ {
		if err := writeWindowsRuntimeStampToDirectoryHandle(directory, name, versions[index%2]); err != nil {
			stop.Store(true)
			<-done
			t.Fatalf("republish %d: %v", index, err)
		}
	}
	stop.Store(true)
	<-done

	if reads.Load() < 100 {
		t.Fatalf("SETUP INVALID: the reader only ran %d times, so it proves nothing about the window", reads.Load())
	}
	if len(bad) > 0 {
		t.Fatalf("a reader saw a stamp that was neither version (%d reads): %v", reads.Load(), bad)
	}
	if stamps, replacements := stampEntries(t, root); stamps != 1 || len(replacements) != 0 {
		t.Fatalf("the root holds %d stamps and leftover replacements %v, want exactly the stamp", stamps, replacements)
	}
}

func errorText(err error) string {
	if err == nil {
		return "nil"
	}
	return err.Error()
}

// The one point where a stopped process leaves two files behind: the
// replacement is complete and the stamp has not been replaced yet. The stamp has
// to still be the previous one, whole, so a process that dies here costs nothing.
func TestStampPublishLeavesThePreviousStampWholeUntilTheReplace(t *testing.T) {
	root := t.TempDir()
	directory := openStampTestDirectory(t, root)
	name := windowsSandboxRuntimeStampName(testStampPlanHash)
	path := filepath.Join(root, name)
	previous, next := stampPublishTestContents("previous-"), stampPublishTestContents("next-")
	if err := writeWindowsRuntimeStampToDirectoryHandle(directory, name, previous); err != nil {
		t.Fatalf("publish the previous stamp: %v", err)
	}

	reached := false
	windowsRuntimeStampPublishSeam = func() {
		reached = true
		data, err := readWindowsSandboxRuntimeStampFile(path)
		if err != nil {
			t.Errorf("mid-publish the stamp could not be read: %v", err)
			return
		}
		if string(data) != previous {
			t.Errorf("mid-publish the stamp reads %q, want the previous stamp whole", data)
		}
		if stamps, replacements := stampEntries(t, root); stamps != 1 || len(replacements) != 1 {
			t.Errorf("mid-publish the root holds %d stamps and replacements %v, want the stamp and one replacement", stamps, replacements)
		}
	}
	defer func() { windowsRuntimeStampPublishSeam = nil }()

	if err := writeWindowsRuntimeStampToDirectoryHandle(directory, name, next); err != nil {
		t.Fatalf("publish the next stamp: %v", err)
	}
	if !reached {
		t.Fatal("SETUP INVALID: the publish never reached the point between writing and replacing")
	}
	data, err := readWindowsSandboxRuntimeStampFile(path)
	if err != nil || string(data) != next {
		t.Fatalf("after the publish the stamp reads %q (%v), want the next stamp", data, err)
	}
	if stamps, replacements := stampEntries(t, root); stamps != 1 || len(replacements) != 0 {
		t.Fatalf("after the publish the root holds %d stamps and replacements %v", stamps, replacements)
	}
}

// Rollback restoring the previous stamp goes through the same single step. In
// the middle of the restore, the stamp being rolled back is still there and
// whole; it used to be deleted first, which is the state a stopped rollback
// left behind.
func TestStampRestoreNeverLeavesTheStampMissing(t *testing.T) {
	root, identity := runtimeRootForCompensation(t)
	directory := openStampTestDirectory(t, root)
	name := windowsSandboxRuntimeStampName(testStampPlanHash)
	path := filepath.Join(root, name)
	rolledBack, prior := stampPublishTestContents("this-run-"), stampPublishTestContents("before-this-run-")
	if err := writeWindowsRuntimeStampToDirectoryHandle(directory, name, rolledBack); err != nil {
		t.Fatalf("publish this run's stamp: %v", err)
	}

	reached := false
	windowsRuntimeStampPublishSeam = func() {
		reached = true
		data, err := readWindowsSandboxRuntimeStampFile(path)
		if err != nil {
			t.Errorf("mid-restore the stamp is gone: %v", err)
			return
		}
		if string(data) != rolledBack {
			t.Errorf("mid-restore the stamp reads %q, want this run's stamp whole", data)
		}
	}
	defer func() { windowsRuntimeStampPublishSeam = nil }()

	if err := compensateRuntimeStampBound(root, identity, name, []byte(prior), true); err != nil {
		t.Fatalf("restore the previous stamp: %v", err)
	}
	if !reached {
		t.Fatal("SETUP INVALID: the restore never went through the publish")
	}
	data, err := readWindowsSandboxRuntimeStampFile(path)
	if err != nil || string(data) != prior {
		t.Fatalf("after the restore the stamp reads %q (%v), want the previous stamp", data, err)
	}
}

// The validation read holds the stamp with FILE_SHARE_DELETE, so a publish
// replaces it while it is open. The held reader keeps the version it opened.
func TestStampPublishReplacesAStampTheValidatorHasOpen(t *testing.T) {
	root := t.TempDir()
	directory := openStampTestDirectory(t, root)
	name := windowsSandboxRuntimeStampName(testStampPlanHash)
	path := filepath.Join(root, name)
	previous, next := stampPublishTestContents("previous-"), stampPublishTestContents("next-")
	if err := writeWindowsRuntimeStampToDirectoryHandle(directory, name, previous); err != nil {
		t.Fatalf("publish the previous stamp: %v", err)
	}
	restore := shortenStampRenameRetry(t, 2)
	defer restore()

	held, err := openWindowsSandboxRuntimeStampFile(path)
	if err != nil {
		t.Fatalf("open the stamp the way validation does: %v", err)
	}
	defer func() { _ = held.Close() }()
	if err := writeWindowsRuntimeStampToDirectoryHandle(directory, name, next); err != nil {
		t.Fatalf("a publish could not replace a stamp the validator had open: %v", err)
	}
	data, err := readWindowsSandboxRuntimeStampFile(path)
	if err != nil || string(data) != next {
		t.Fatalf("after the publish the stamp reads %q (%v), want the next stamp", data, err)
	}
}

// A reader that holds the stamp WITHOUT FILE_SHARE_DELETE, such as an older
// Zero reading it with os.ReadFile, blocks the replace. A short hold is waited
// out; a hold past the bound fails the publish with the previous stamp whole
// and nothing left behind.
func TestStampPublishAgainstAReaderWithoutShareDelete(t *testing.T) {
	root := t.TempDir()
	directory := openStampTestDirectory(t, root)
	name := windowsSandboxRuntimeStampName(testStampPlanHash)
	path := filepath.Join(root, name)
	previous, next := stampPublishTestContents("previous-"), stampPublishTestContents("next-")
	if err := writeWindowsRuntimeStampToDirectoryHandle(directory, name, previous); err != nil {
		t.Fatalf("publish the previous stamp: %v", err)
	}

	t.Run("held past the bound", func(t *testing.T) {
		restore := shortenStampRenameRetry(t, 3)
		defer restore()
		held, err := os.Open(path)
		if err != nil {
			t.Fatalf("hold the stamp open: %v", err)
		}
		err = writeWindowsRuntimeStampToDirectoryHandle(directory, name, next)
		_ = held.Close()
		if err == nil {
			t.Fatal("a publish replaced a stamp held without share-delete, so this does not stage the blocked replace")
		}
		if !strings.Contains(err.Error(), "publish sandbox runtime setup stamp") {
			t.Fatalf("failed somewhere other than the replace: %v", err)
		}
		data, readErr := readWindowsSandboxRuntimeStampFile(path)
		if readErr != nil || string(data) != previous {
			t.Fatalf("after the failed publish the stamp reads %q (%v), want the previous stamp whole", data, readErr)
		}
		if stamps, replacements := stampEntries(t, root); stamps != 1 || len(replacements) != 0 {
			t.Fatalf("the failed publish left %d stamps and replacements %v behind", stamps, replacements)
		}
	})

	t.Run("released within the bound", func(t *testing.T) {
		held, err := os.Open(path)
		if err != nil {
			t.Fatalf("hold the stamp open: %v", err)
		}
		released := make(chan struct{})
		go func() {
			time.Sleep(100 * time.Millisecond)
			_ = held.Close()
			close(released)
		}()
		err = writeWindowsRuntimeStampToDirectoryHandle(directory, name, next)
		<-released
		if err != nil {
			t.Fatalf("a publish did not wait out a reader that let go within the bound: %v", err)
		}
		data, readErr := readWindowsSandboxRuntimeStampFile(path)
		if readErr != nil || string(data) != next {
			t.Fatalf("after the publish the stamp reads %q (%v), want the next stamp", data, readErr)
		}
	})
}

func shortenStampRenameRetry(t *testing.T, attempts int) func() {
	t.Helper()
	previousAttempts, previousRetry := windowsRuntimeStampRenameAttempts, windowsRuntimeStampRenameRetry
	windowsRuntimeStampRenameAttempts, windowsRuntimeStampRenameRetry = attempts, time.Millisecond
	return func() {
		windowsRuntimeStampRenameAttempts, windowsRuntimeStampRenameRetry = previousAttempts, previousRetry
	}
}

// A repeat setup of the same plan while a command is validating: the command
// holds the stamp open through the validator, the setup republishes it, and
// neither fails. The validator is called here rather than the reader underneath
// it, so this also pins that validation reads with FILE_SHARE_DELETE at all.
func TestValidationAndARepeatSetupDoNotBlockEachOther(t *testing.T) {
	root := t.TempDir()
	directory := openStampTestDirectory(t, root)
	name := windowsSandboxRuntimeStampName(testStampPlanHash)
	if err := writeWindowsRuntimeStampToDirectoryHandle(directory, name, testStampPlanHash); err != nil {
		t.Fatalf("publish the stamp: %v", err)
	}
	restore := shortenStampRenameRetry(t, 3)
	defer restore()

	reached := false
	windowsRuntimeStampReadSeam = func() {
		if reached {
			return
		}
		reached = true
		if err := writeWindowsRuntimeStampToDirectoryHandle(directory, name, testStampPlanHash); err != nil {
			t.Errorf("a repeat setup could not replace the stamp while validation had it open: %v", err)
		}
	}
	defer func() { windowsRuntimeStampReadSeam = nil }()

	profile := PermissionProfile{Runtime: &SandboxRuntime{Root: root}}
	if err := validateWindowsSandboxRuntimeStamp(profile, testStampPlanHash); err != nil {
		t.Fatalf("validation during a repeat setup failed: %v", err)
	}
	if !reached {
		t.Fatal("validation did not read the stamp through the share-delete reader")
	}
	if err := validateWindowsSandboxRuntimeStamp(profile, testStampPlanHash); err != nil {
		t.Fatalf("validation after the repeat setup failed: %v", err)
	}
}
