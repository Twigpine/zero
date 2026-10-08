package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// editTrackerFixture writes a numbered file and returns its real path plus a
// tracker that has already recorded the whole content as the current version.
//
// EvalSymlinks matters: on macOS t.TempDir() hands back /var/..., the tools
// resolve it to /private/var/..., and a tracker keyed on the unresolved path
// silently misses every lookup — which makes an edit fail for the very reason
// this file is about, and for entirely the wrong cause.
func editTrackerFixture(t *testing.T, lines []string) (string, *FileTracker, Tool) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "index.go")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	tracker := NewFileTracker()
	content, _ := os.ReadFile(path)
	info, _ := os.Stat(path)
	tracker.Record(path, content, info)
	return path, tracker, NewScopedEditFileTool(root, nil)
}

func numberedLines(total int, special map[int]string) []string {
	lines := make([]string, 0, total)
	for i := 1; i <= total; i++ {
		if text, ok := special[i]; ok {
			lines = append(lines, text)
			continue
		}
		lines = append(lines, "filler")
	}
	return lines
}

func runTrackedEdit(t *testing.T, tool Tool, tracker *FileTracker, path, oldString, newString string) Result {
	t.Helper()
	return tool.(optionsAwareTool).RunWithOptions(context.Background(), map[string]any{
		"path": path, "old_string": oldString, "new_string": newString,
	}, RunOptions{FileTracker: tracker})
}

// ONE EDIT MUST NOT ERASE EVERY OTHER READ, and this is the run that made the
// point: a 371-line file read in three pieces (40-45, 85-260, 260-371), one
// two-line edit at line 92, and then six consecutive refusals to edit regions
// that HAD been read and that the edit never touched — until the
// repeated-failure guard halted the run. The error told the model to re-read,
// which its next successful edit would have undone again.
func TestASuccessfulEditKeepsTheReadsItDidNotDisturb(t *testing.T) {
	path, tracker, tool := editTrackerFixture(t, numberedLines(371, map[int]string{
		92:  "cmp := keyCmp(k, n.split)",
		200: "prio: l.prio, split: l.split,",
	}))
	tracker.RecordSeenRange(path, 40, 45, 371)
	tracker.RecordSeenRange(path, 85, 260, 371)
	tracker.RecordSeenRange(path, 260, 371, 371)

	if !tracker.SeenRange(path, 200, 200) {
		t.Fatal("setup: line 200 sits inside the 85-260 read and must start out seen")
	}

	first := runTrackedEdit(t, tool, tracker, path, "cmp := keyCmp(k, n.split)", "cmp := keyCmp(k, n.pivot)")
	if strings.Contains(first.Output, "Error") {
		t.Fatalf("the first edit failed, so the test never reaches its subject: %s", first.Output)
	}

	if !tracker.SeenRange(path, 200, 200) {
		t.Error("line 200 was read, the edit at line 92 did not touch it, and the file still has 371 lines — but it is no longer considered seen")
	}
	// The assertion that matters: the SECOND edit is what the run could never do.
	second := runTrackedEdit(t, tool, tracker, path, "prio: l.prio, split: l.split,", "prio: l.prio, pivot: l.pivot,")
	if strings.Contains(second.Output, "not been read exactly in this session") {
		t.Fatalf("a second edit into an already-read region was refused as unseen: %s", second.Output)
	}
	if strings.Contains(second.Output, "Error") {
		t.Fatalf("the second edit failed: %s", second.Output)
	}
}

// LINE NUMBERS MOVE WHEN AN EDIT CHANGES THE LINE COUNT, so a range after the
// edit has to be carried across SHIFTED, not left where it was.
func TestReadsAfterAnEditAreShiftedByTheLineDelta(t *testing.T) {
	path, tracker, tool := editTrackerFixture(t, numberedLines(100, map[int]string{
		10: "short",
		80: "target line",
	}))
	tracker.RecordSeenRange(path, 5, 20, 100)
	tracker.RecordSeenRange(path, 70, 90, 100)

	// Replace one line with three: everything after line 10 moves down by two.
	res := runTrackedEdit(t, tool, tracker, path, "short", "one\ntwo\nthree")
	if strings.Contains(res.Output, "Error") {
		t.Fatalf("setup edit failed: %s", res.Output)
	}

	// "target line" was line 80 and is now line 82. It was read either way.
	content, _ := os.ReadFile(path)
	moved := strings.Count(strings.SplitN(string(content), "target line", 2)[0], "\n") + 1
	if moved != 82 {
		t.Fatalf("fixture moved the target to line %d, expected 82", moved)
	}
	if !tracker.SeenRange(path, moved, moved) {
		t.Errorf("the read at lines 70-90 was not shifted with the content: line %d reads as unseen", moved)
	}
	second := runTrackedEdit(t, tool, tracker, path, "target line", "target changed")
	if strings.Contains(second.Output, "Error") {
		t.Fatalf("editing a line that was read and merely moved was refused: %s", second.Output)
	}
}

// THE GUARD STILL GUARDS. Carrying reads across must not credit the model with
// content it never saw — that is the whole reason the check exists.
func TestAnUnreadRegionIsStillRefusedAfterAnEdit(t *testing.T) {
	path, tracker, tool := editTrackerFixture(t, numberedLines(300, map[int]string{
		50:  "seen line",
		250: "never read line",
	}))
	// Only 40-60 is read. Line 250 is not.
	tracker.RecordSeenRange(path, 40, 60, 300)

	if res := runTrackedEdit(t, tool, tracker, path, "seen line", "seen line edited"); strings.Contains(res.Output, "Error") {
		t.Fatalf("setup edit failed: %s", res.Output)
	}
	res := runTrackedEdit(t, tool, tracker, path, "never read line", "sneaky")
	if !strings.Contains(res.Output, "not been read exactly in this session") {
		t.Fatalf("an edit into a region that was NEVER read was allowed: %s", res.Output)
	}
}

// A RANGE THE EDIT SPANS IS SPLIT, NOT DROPPED — and getting this wrong is not
// hypothetical: dropping on overlap was my first attempt, and it left the
// original defect exactly in place. A read almost always CONTAINS the line it
// is about to edit, because containing it is why it was read.
//
// So the flanks survive and only the rewritten lines stop being known.
func TestARangeTheEditSpansIsSplitAroundIt(t *testing.T) {
	path, tracker, tool := editTrackerFixture(t, numberedLines(100, map[int]string{
		20: "alpha",
		21: "beta",
		22: "gamma",
	}))
	tracker.RecordSeenRange(path, 15, 30, 100)

	// Replace a three-line span with one line, entirely inside the read range.
	if res := runTrackedEdit(t, tool, tracker, path, "alpha\nbeta\ngamma", "merged"); strings.Contains(res.Output, "Error") {
		t.Fatalf("setup edit failed: %s", res.Output)
	}

	// Before the edit: still known, still at the same line numbers.
	if !tracker.SeenRange(path, 15, 19) {
		t.Error("lines 15-19 sit before the edit and did not move, but were forgotten")
	}
	// After it: still known, shifted up by the two lines the edit removed.
	if !tracker.SeenRange(path, 21, 28) {
		t.Error("lines 23-30 were read and merely moved to 21-28, but were forgotten")
	}
	// The rewritten span itself is no longer described by that read.
	if tracker.SeenRange(path, 15, 30) {
		t.Error("the whole original range still reads as seen, so the model is credited with content the edit replaced")
	}
}

// AN EXTERNAL CHANGE STILL INVALIDATES EVERYTHING. RecordEdit is only for edits
// we made; a file changed behind our back is exactly the case the blanket drop
// is right for, and it must keep working.
func TestAnExternalChangeStillDropsEveryRead(t *testing.T) {
	path, tracker, _ := editTrackerFixture(t, numberedLines(100, map[int]string{50: "hello"}))
	tracker.RecordSeenRange(path, 40, 60, 100)
	if !tracker.SeenRange(path, 50, 50) {
		t.Fatal("setup: line 50 should be seen")
	}

	// Something outside Zero rewrites the file.
	changed := strings.Repeat("different\n", 100)
	if err := os.WriteFile(path, []byte(changed), 0o644); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	tracker.Record(path, []byte(changed), info)

	if tracker.SeenRange(path, 50, 50) {
		t.Error("an external rewrite left the old read ranges in place")
	}
}

// A file read in FULL stays read in full: an edit of ours does not make that
// untrue, and re-reading a whole file after every edit is the cost this avoids.
func TestAWhollyReadFileStaysWhollyRead(t *testing.T) {
	path, tracker, tool := editTrackerFixture(t, numberedLines(40, map[int]string{5: "one"}))
	tracker.RecordSeenRange(path, 1, 40, 40)
	if !tracker.SeenWhole(path) {
		t.Fatal("setup: the file should read as wholly seen")
	}

	if res := runTrackedEdit(t, tool, tracker, path, "one", "one\ntwo"); strings.Contains(res.Output, "Error") {
		t.Fatalf("edit failed: %s", res.Output)
	}
	if !tracker.SeenWhole(path) {
		t.Error("a wholly-read file stopped being wholly read after an edit")
	}
}

// The span helpers, directly: these decide what survives, so their edges are
// worth pinning independently of the tool.
func TestChangedSpanHelpers(t *testing.T) {
	t.Run("a single line replaced in place", func(t *testing.T) {
		first, last, delta := changedLineSpan("a\nb\nc", "a\nB\nc")
		if first != 2 || last != 2 || delta != 0 {
			t.Fatalf("got (%d,%d,%d), want (2,2,0)", first, last, delta)
		}
	})
	t.Run("one line becomes three", func(t *testing.T) {
		first, last, delta := changedLineSpan("a\nb\nc", "a\nx\ny\nz\nc")
		if first != 2 || last != 2 || delta != 2 {
			t.Fatalf("got (%d,%d,%d), want (2,2,2)", first, last, delta)
		}
	})
	t.Run("lines removed", func(t *testing.T) {
		first, last, delta := changedLineSpan("a\nb\nc\nd", "a\nd")
		if first != 2 || last != 3 || delta != -2 {
			t.Fatalf("got (%d,%d,%d), want (2,3,-2)", first, last, delta)
		}
	})
	t.Run("bytes", func(t *testing.T) {
		first, last, delta := changedByteSpan([]byte("abcdef"), []byte("abXYef"))
		if first != 2 || last != 4 || delta != 0 {
			t.Fatalf("got (%d,%d,%d), want (2,4,0)", first, last, delta)
		}
	})
}

// A file read whole IN PIECES must stay seen-whole after an edit. SeenWhole
// derives its answer from the ranges precisely because the raw flag is only set
// by a single covering read; RecordEdit branching on the flag instead sent this
// case down the split path, and the write_file refusal the derived answer exists
// to prevent came back within one edit.
func TestAFileReadWholeInPiecesStaysWholeAfterAnEdit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "chunked.go")
	content := "l1\nl2\nl3\nl4\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	tracker := NewFileTracker()
	info, _ := os.Stat(path)
	tracker.Record(path, []byte(content), info)
	// Two reads that together cover the file, neither covering it alone.
	tracker.RecordSeenRange(path, 1, 2, 4)
	tracker.RecordSeenRange(path, 3, 4, 4)
	if !tracker.SeenWhole(path) {
		t.Fatal("two chunked reads did not add up to seen-whole")
	}

	updated := "l1\nl2X\nl3\nl4\n"
	if err := os.WriteFile(path, []byte(updated), 0o600); err != nil {
		t.Fatal(err)
	}
	newInfo, _ := os.Stat(path)
	tracker.RecordEdit(path, []byte(content), []byte(updated), newInfo)

	if !tracker.SeenWhole(path) {
		t.Error("a file read whole in two chunks stopped being seen whole after one edit")
	}
}

// observation.total is compared against read ranges, so it has to be the line
// count a READER would give. strings.Split leaves an empty final element for
// content ending in a newline, which counted one line too many — and since
// SeenWhole asks whether the ranges cover 1..total, that made full coverage
// unreachable for a file that had just been read in full.
func TestCountLinesMatchesWhatAReaderWouldSay(t *testing.T) {
	for _, testCase := range []struct {
		content string
		want    int
	}{
		{"a\nb\nc\n", 3}, // the common case: trailing newline opens no line
		{"a\nb\nc", 3},
		{"\n", 1},
		{"", 0},
	} {
		if got := countLines([]byte(testCase.content)); got != testCase.want {
			t.Errorf("countLines(%q) = %d, want %d", testCase.content, got, testCase.want)
		}
	}
}
