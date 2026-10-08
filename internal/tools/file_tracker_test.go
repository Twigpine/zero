package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFileTrackerRecordsAndReadsBackVersion(t *testing.T) {
	tracker := NewFileTracker()
	path := filepath.Join(t.TempDir(), "a.txt")
	content := []byte("hello world")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	tracker.Record(path, content, info)
	version, ok := tracker.Version(path)
	if !ok {
		t.Fatal("expected a recorded version")
	}
	if version.Hash != HashContent(content) {
		t.Fatalf("hash = %q, want %q", version.Hash, HashContent(content))
	}
	if version.Size != int64(len(content)) {
		t.Fatalf("size = %d, want %d", version.Size, len(content))
	}
	if version.MTime.IsZero() {
		t.Fatal("expected mtime to be recorded from stat info")
	}
}

func TestFileTrackerRecordHashMatchesRecord(t *testing.T) {
	tracker := NewFileTracker()
	content := []byte("streamed content")
	tracker.RecordHash("/repo/x.txt", HashContent(content), nil)
	if err := tracker.CheckConflict("/repo/x.txt", content); err != nil {
		t.Fatalf("recorded hash should match content, got %v", err)
	}
	if err := tracker.CheckConflict("/repo/x.txt", []byte("changed")); err != ErrFileChangedOnDisk {
		t.Fatalf("changed content should conflict, got %v", err)
	}
}

func TestCheckConflictAllowsUntrackedPath(t *testing.T) {
	tracker := NewFileTracker()
	// No Record call: a first-touch write has no baseline to conflict against.
	if err := tracker.CheckConflict("/nowhere/x.txt", []byte("anything")); err != nil {
		t.Fatalf("untracked path should not conflict, got %v", err)
	}
}

func TestCheckConflictAllowsMatchingContent(t *testing.T) {
	tracker := NewFileTracker()
	content := []byte("stable content")
	tracker.Record("/repo/x.txt", content, nil)
	if err := tracker.CheckConflict("/repo/x.txt", content); err != nil {
		t.Fatalf("unchanged content should not conflict, got %v", err)
	}
}

func TestCheckConflictBlocksDriftedContent(t *testing.T) {
	tracker := NewFileTracker()
	tracker.Record("/repo/x.txt", []byte("version one"), nil)
	if err := tracker.CheckConflict("/repo/x.txt", []byte("version two — changed underneath us")); err != ErrFileChangedOnDisk {
		t.Fatalf("drifted content should report ErrFileChangedOnDisk, got %v", err)
	}
}

func TestForgetClearsBaselineSoNextWriteIsAllowed(t *testing.T) {
	tracker := NewFileTracker()
	tracker.Record("/repo/x.txt", []byte("version one"), nil)
	tracker.Forget("/repo/x.txt")
	if _, ok := tracker.Version("/repo/x.txt"); ok {
		t.Fatal("Forget should drop the recorded version")
	}
	if err := tracker.CheckConflict("/repo/x.txt", []byte("version two")); err != nil {
		t.Fatalf("forgotten path should behave as untracked, got %v", err)
	}
}

func TestRecordOverwritesBaselineWithNewContent(t *testing.T) {
	tracker := NewFileTracker()
	tracker.Record("/repo/x.txt", []byte("old"), nil)
	tracker.Record("/repo/x.txt", []byte("new"), nil)
	// After re-recording, the new content is the baseline and matches.
	if err := tracker.CheckConflict("/repo/x.txt", []byte("new")); err != nil {
		t.Fatalf("re-recorded content should be the new baseline, got %v", err)
	}
	if err := tracker.CheckConflict("/repo/x.txt", []byte("old")); err != ErrFileChangedOnDisk {
		t.Fatal("the superseded content should now conflict")
	}
}

func TestNilFileTrackerIsANoop(t *testing.T) {
	var tracker *FileTracker
	tracker.Record("/repo/x.txt", []byte("x"), nil) // must not panic
	tracker.Forget("/repo/x.txt")
	if _, ok := tracker.Version("/repo/x.txt"); ok {
		t.Fatal("nil tracker should report no version")
	}
	if err := tracker.CheckConflict("/repo/x.txt", []byte("x")); err != nil {
		t.Fatalf("nil tracker should never conflict, got %v", err)
	}
}

func TestHashContentIsStableAndDistinguishing(t *testing.T) {
	// Store results of separate calls before comparing so the stability check is
	// not a same-expression comparison (staticcheck SA4000).
	first := HashContent([]byte("a"))
	second := HashContent([]byte("a"))
	if first != second {
		t.Fatal("hash must be stable for identical content")
	}
	if first == HashContent([]byte("b")) {
		t.Fatal("hash must differ for different content")
	}
}

func TestRecordCreatedTracksNewFilesInOrder(t *testing.T) {
	tracker := NewFileTracker()
	tracker.RecordCreated("/repo/b.txt")
	tracker.RecordCreated("/repo/a.txt")
	got := tracker.CreatedFiles()
	want := []string{"/repo/b.txt", "/repo/a.txt"}
	if len(got) != len(want) {
		t.Fatalf("CreatedFiles() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("CreatedFiles() = %v, want %v", got, want)
		}
	}
}

func TestRecordCreatedDeduplicates(t *testing.T) {
	tracker := NewFileTracker()
	tracker.RecordCreated("/repo/a.txt")
	tracker.RecordCreated("/repo/a.txt")
	if got := tracker.CreatedFiles(); len(got) != 1 {
		t.Fatalf("CreatedFiles() = %v, want a single entry", got)
	}
}

func TestNilFileTrackerCreatedFilesIsANoop(t *testing.T) {
	var tracker *FileTracker
	tracker.RecordCreated("/repo/a.txt") // must not panic
	if got := tracker.CreatedFiles(); got != nil {
		t.Fatalf("CreatedFiles() on nil tracker = %v, want nil", got)
	}
}

// THE SPAN IS DERIVED IN BOUNDED MEMORY, whatever the file size.
//
// changedLineSpan used to split both versions into []string, so it allocated one
// string header per line for each — work scaling with the SIZE OF THE FILE rather
// than the size of the edit, and it ran before RecordEdit had even checked whether
// there was an observation to update. @jatmn measured 32,036,640 bytes for two
// 2 MB versions of a million short lines.
//
// The cost was not only memory. edit_file writes the updated bytes BEFORE calling
// RecordEdit, so an out-of-memory kill lands after the user's file has changed and
// before the tracker baseline catches up — file and record disagree, silently.
//
// This asserts an absolute ceiling rather than a ratio, so a future rewrite that
// reintroduces per-line slices fails here rather than merely getting slower.
func TestTheChangedSpanDoesNotAllocatePerLine(t *testing.T) {
	before := strings.Repeat("x\n", 200_000)
	after := before[:len(before)-2] + "y\n"

	spanBytes := testing.Benchmark(func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			changedLineSpan(before, after)
		}
	}).AllocedBytesPerOp()
	if spanBytes > 4096 {
		t.Errorf("changedLineSpan allocated %d bytes for a %d-line pair; per-line slices are back", spanBytes, 200_000)
	}

	content := []byte(before)
	countBytes := testing.Benchmark(func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			countLines(content)
		}
	}).AllocedBytesPerOp()
	if countBytes > 4096 {
		t.Errorf("countLines allocated %d bytes to return a number", countBytes)
	}
}

// AND IT GIVES THE SAME ANSWERS THE LINE-ARRAY VERSION DID. The rewrite is only
// safe if it is behaviour-preserving, so the replaced implementation is kept here
// as an oracle and both are asked the same questions. This caught 206 mismatches
// in the first attempt at the byte scan — empty versions and partially shared
// trailing lines, neither of which the hand-written cases below would have found.
func TestTheByteScanAgreesWithTheLineSplit(t *testing.T) {
	oracle := func(before, after string) (int, int, int) {
		split := func(text string) []string {
			if text == "" {
				return nil
			}
			return strings.Split(text, "\n")
		}
		beforeLines, afterLines := split(before), split(after)
		prefix := 0
		for prefix < len(beforeLines) && prefix < len(afterLines) && beforeLines[prefix] == afterLines[prefix] {
			prefix++
		}
		suffix := 0
		for suffix < len(beforeLines)-prefix && suffix < len(afterLines)-prefix &&
			beforeLines[len(beforeLines)-1-suffix] == afterLines[len(afterLines)-1-suffix] {
			suffix++
		}
		return prefix + 1, len(beforeLines) - suffix, len(afterLines) - len(beforeLines)
	}

	versions := []string{
		"", "a", "\n", "a\n", "a\nb", "a\nb\n", "a\nb\nc", "a\nb\nc\n",
		"\n\n", "x\n\ny", "a\nb\nc\nd\ne", "same\nsame\nsame\n", "ab\nc", "b\nc",
	}
	for _, before := range versions {
		for _, after := range versions {
			wantFirst, wantLast, wantDelta := oracle(before, after)
			gotFirst, gotLast, gotDelta := changedLineSpan(before, after)
			if wantFirst != gotFirst || wantLast != gotLast || wantDelta != gotDelta {
				t.Errorf("changedLineSpan(%q, %q) = (%d, %d, %d), want (%d, %d, %d)",
					before, after, gotFirst, gotLast, gotDelta, wantFirst, wantLast, wantDelta)
			}
		}
	}
}
