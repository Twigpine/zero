package agentsessions

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestScanHeadReadsFarLessThanTheWholeFile is the test that keeps `sessions
// discover` usable. The live corpus is 439 MB across 1,266 files with a single
// 73 MB transcript in it; an indexer that reads whole files turns a listing into
// a coffee break.
//
// It asserts on BYTES READ rather than on elapsed time, so it fails for the
// right reason on a slow machine and cannot be silenced by faster hardware.
func TestScanHeadReadsFarLessThanTheWholeFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "huge.jsonl")

	// A transcript whose metadata is where it really is (line 3) followed by a
	// great deal of conversation, mimicking a long session.
	bulk := strings.Repeat("x", 200<<10)
	lines := []string{
		`{"type":"mode","mode":"default"}`,
		`{"type":"queue-operation","operation":"enqueue"}`,
		`{"type":"user","cwd":"/Users/someone/proj","sessionId":"huge","message":{"role":"user","content":"go"}}`,
	}
	for i := 0; i < 200; i++ {
		lines = append(lines, `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"`+bulk+`"}]}}`)
	}
	writeFile(t, path, strings.Join(lines, "\n")+"\n")

	fileSize := fileSizeOf(t, path)
	if fileSize < 32<<20 {
		t.Fatalf("fixture is only %d bytes; it must dwarf the head budget to prove anything", fileSize)
	}

	read, _, err := scanHeadSnapshot("", path, defaultHeadLimit, func([]byte, bool) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	if read > defaultHeadLimit.MaxBytes {
		t.Errorf("scanHead read %d bytes, over its own %d-byte budget", read, defaultHeadLimit.MaxBytes)
	}
	if read >= fileSize/8 {
		t.Errorf("scanHead read %d of %d bytes — discovery is reading the transcript, not indexing it", read, fileSize)
	}

	// And the point of the budget: the metadata is still found.
	session, ok := indexFamily1Transcript("claude-code", "", path)
	if !ok || session.Cwd != "/Users/someone/proj" {
		t.Fatalf("indexing a large transcript failed: ok=%v session=%+v", ok, session)
	}
}

// TestAnOversizedFirstRecordDoesNotStarveTheScan pins the defect the live
// corpus exposed: three real sessions there open with a ~334 KB
// queue-operation record. At the original 256 KiB budget the scan spent
// everything on that one line and never reached the record carrying cwd, so the
// sessions vanished from discovery with no error anywhere.
func TestAnOversizedFirstRecordDoesNotStarveTheScan(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fat-head.jsonl")
	writeFile(t, path, strings.Join([]string{
		`{"type":"queue-operation","operation":"enqueue","content":"` + strings.Repeat("q", 334<<10) + `"}`,
		`{"type":"mode","mode":"default"}`,
		`{"type":"user","cwd":"/Users/someone/proj","sessionId":"fat-head","message":{"role":"user","content":"still here"}}`,
	}, "\n")+"\n")

	session, ok := indexFamily1Transcript("claude-code", "", path)
	if !ok {
		t.Fatal("a session whose first record is huge was dropped from discovery")
	}
	if session.Cwd != "/Users/someone/proj" {
		t.Errorf("Cwd = %q, want the record after the oversized one to be reached", session.Cwd)
	}
}

// TestALineTooLongToKeepIsSkippedNotFatal covers a single record larger than
// MaxLineBytes. bufio.Scanner would fail the entire scan here; the records
// after it must still be read.
func TestALineTooLongToKeepIsSkippedNotFatal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "long-line.jsonl")
	writeFile(t, path, strings.Join([]string{
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"` + strings.Repeat("y", 128<<10) + `"}]}}`,
		`{"type":"user","cwd":"/Users/someone/proj","sessionId":"long-line","message":{"role":"user","content":"after the wall"}}`,
	}, "\n")+"\n")

	session, ok := indexFamily1Transcript("claude-code", "", path)
	if !ok || session.Cwd != "/Users/someone/proj" {
		t.Fatalf("a record past an over-long line was not read: ok=%v session=%+v", ok, session)
	}
}

func TestScanHeadStopsWhenTheVisitorIsDone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stop.jsonl")
	lines := make([]string, 0, 100)
	for i := 0; i < 100; i++ {
		lines = append(lines, `{"type":"user","content":"`+strings.Repeat("z", 4096)+`"}`)
	}
	writeFile(t, path, strings.Join(lines, "\n")+"\n")

	seen := 0
	read, _, err := scanHeadSnapshot("", path, defaultHeadLimit, func([]byte, bool) bool {
		seen++
		return seen < 2
	})
	if err != nil {
		t.Fatal(err)
	}
	if seen != 2 {
		t.Errorf("visited %d lines, want to stop after 2", seen)
	}
	if read > 64<<10 {
		t.Errorf("read %d bytes after an early stop; the reader buffer should bound this", read)
	}
}

func TestScanHeadHonoursItsLineBudget(t *testing.T) {
	path := filepath.Join(t.TempDir(), "many.jsonl")
	lines := make([]string, 0, 500)
	for i := 0; i < 500; i++ {
		lines = append(lines, `{"type":"noise"}`)
	}
	writeFile(t, path, strings.Join(lines, "\n")+"\n")

	seen := 0
	if _, _, err := scanHeadSnapshot("", path, defaultHeadLimit, func([]byte, bool) bool { seen++; return true }); err != nil {
		t.Fatal(err)
	}
	if seen != defaultHeadLimit.MaxLines {
		t.Errorf("visited %d lines, want exactly MaxLines=%d", seen, defaultHeadLimit.MaxLines)
	}
}

func TestScanHeadOnAMissingFileIsAnError(t *testing.T) {
	// Unlike globbing, an unreadable file that discovery has already decided
	// exists is worth reporting to the caller, which drops that one entry.
	if _, _, err := scanHeadSnapshot("", filepath.Join(t.TempDir(), "absent.jsonl"), defaultHeadLimit, func([]byte, bool) bool { return true }); err == nil {
		t.Error("scanHead on a missing file returned no error")
	}
}

func TestStreamTailLinesReadsEverythingWithinItsBudget(t *testing.T) {
	path := filepath.Join(t.TempDir(), "all.jsonl")
	lines := make([]string, 0, 300)
	for i := 0; i < 300; i++ {
		lines = append(lines, `{"type":"message","n":`+itoa(i)+`}`)
	}
	writeFile(t, path, strings.Join(lines, "\n")+"\n")

	seen := 0
	if _, err := streamTailLinesAt("", path, 64<<10, importByteLimit, func([]byte, bool) bool { seen++; return true }); err != nil {
		t.Fatal(err)
	}
	if seen != 300 {
		t.Errorf("streamTailLinesAt visited %d lines, want all 300 within the byte budget", seen)
	}
}

func TestStreamTailLinesToleratesAMissingTrailingNewline(t *testing.T) {
	path := filepath.Join(t.TempDir(), "no-newline.jsonl")
	// A live transcript is appended to constantly; the last record frequently
	// has no terminator yet.
	writeFile(t, path, `{"type":"a"}`+"\n"+`{"type":"b"}`)

	seen := 0
	if _, err := streamTailLinesAt("", path, 64<<10, importByteLimit, func([]byte, bool) bool { seen++; return true }); err != nil {
		t.Fatal(err)
	}
	if seen != 2 {
		t.Errorf("visited %d lines, want 2 — the unterminated final record must not be lost", seen)
	}
}

func TestStreamTailLinesBoundsTheReadAndDropsAPartialLeadingRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "large.jsonl")
	writeFile(t, path, strings.Repeat("x", 100)+"\nsecond\nthird\n")

	var got []string
	omitted, err := streamTailLinesAt("", path, 64<<10, 20, func(line []byte, truncated bool) bool {
		if truncated {
			t.Fatal("short tail record was reported truncated")
		}
		got = append(got, string(line))
		return true
	})
	if err != nil {
		t.Fatal(err)
	}
	if !omitted {
		t.Fatal("bounded tail read did not disclose that an older prefix was skipped")
	}
	if strings.Join(got, ",") != "second,third" {
		t.Fatalf("tail records = %v, want only complete records second and third", got)
	}
}

func TestStreamTailLinesDoesNotReadPastCapturedLiveExtent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "live.jsonl")
	writeFile(t, path, "first\n")

	var got []string
	appended := false
	_, err := streamTailLinesAt("", path, 64<<10, 32<<20, func(line []byte, truncated bool) bool {
		if truncated {
			t.Fatal("short live record was reported truncated")
		}
		got = append(got, string(line))
		if !appended {
			appended = true
			file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := file.WriteString("appended-after-stat\n"); err != nil {
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
		}
		return true
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "first" {
		t.Fatalf("captured tail records = %v, want only the pre-stat extent", got)
	}
}

// THE LINE TERMINATOR IS NOT CONTENT. A record whose content exactly fills the
// per-line cap has been read in full, and reporting it truncated made the import
// path emit "could not be read" for records it had in fact read — a false alarm
// in the one signal that exists to be trusted. CRLF made it one byte worse,
// since both bytes were counted.
func TestARecordThatExactlyFillsTheCapIsNotTruncated(t *testing.T) {
	const keep = 64
	for _, eol := range []string{"\n", "\r\n"} {
		for _, size := range []int{keep - 1, keep, keep + 1} {
			path := filepath.Join(t.TempDir(), "x.jsonl")
			if err := os.WriteFile(path, append([]byte(strings.Repeat("a", size)), []byte(eol)...), 0o644); err != nil {
				t.Fatal(err)
			}
			var truncated bool
			if _, err := streamTailLinesAt("", path, keep, importByteLimit, func(_ []byte, wasTruncated bool) bool {
				truncated = truncated || wasTruncated
				return true
			}); err != nil {
				t.Fatal(err)
			}
			if want := size > keep; truncated != want {
				t.Errorf("content=%d cap=%d eol=%q reported truncated=%v, want %v", size, keep, eol, truncated, want)
			}
		}
	}
}

// EOF IS THE ONLY CLEAN STOP. Any other read error used to end the scan and
// return success, so a session indexed off however many bytes arrived before an
// I/O failure was indistinguishable from one indexed off a whole file.
func TestScanHeadReportsAReadFailure(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := scanHeadSnapshot("", filepath.Join(dir, "gone.jsonl"), defaultHeadLimit, func([]byte, bool) bool { return true }); err == nil {
		t.Error("scanning a missing transcript reported success")
	}
	// A directory opens but cannot be read as a file: a read error that is not EOF.
	if _, _, err := scanHeadSnapshot("", dir, defaultHeadLimit, func([]byte, bool) bool { return true }); err == nil {
		t.Error("scanning a directory reported success; a non-EOF read error was swallowed")
	}
}

// CONTAINMENT HOLDS AT OPEN TIME, not merely at glob time. globTranscripts
// refuses a symlink wearing a transcript extension, but that verdict describes
// the tree when it was taken — anything can replace the entry before the open,
// and os.Open would follow it out of the store.
func TestScanHeadRefusesAPathOutsideTheRoot(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.jsonl")
	if err := os.WriteFile(outside, []byte(`{"type":"user","cwd":"/w"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := scanHeadSnapshot(root, outside, defaultHeadLimit, func([]byte, bool) bool { return true }); err == nil {
		t.Errorf("scanHead read %q from outside the store root %q", outside, root)
	}
}

// THE IMPORT READ IS CONTAINED TOO, and it matters more here than in the index.
// scanHead only builds a picker row; this path reads a transcript's actual
// content and writes it into the user's own Zero session, so a symlink swapped
// in after the glob would copy whatever it points at into their store.
func TestStreamTailLinesRefusesAPathOutsideTheRoot(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "elsewhere.jsonl")
	if err := os.WriteFile(outside, []byte(`{"type":"user"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	seen := 0
	_, err := streamTailLinesAt(root, outside, 1<<20, importByteLimit, func([]byte, bool) bool { seen++; return true })
	if err == nil {
		t.Errorf("streamTailLinesAt read %q from outside the store root %q", outside, root)
	}
	if seen != 0 {
		t.Errorf("streamTailLinesAt handed the caller %d lines from outside the root", seen)
	}
}

func TestScanHeadSnapshotRejectsMutationDuringDiscovery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "changing.jsonl")
	if err := os.WriteFile(path, []byte("first\nsecond\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mutated := false
	_, _, err := scanHeadSnapshot("", path, defaultHeadLimit, func(_ []byte, _ bool) bool {
		if !mutated {
			mutated = true
			file, openErr := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
			if openErr != nil {
				t.Fatal(openErr)
			}
			if _, writeErr := file.WriteString("replacement\n"); writeErr != nil {
				t.Fatal(writeErr)
			}
			if closeErr := file.Close(); closeErr != nil {
				t.Fatal(closeErr)
			}
		}
		return true
	})
	if err == nil || !strings.Contains(err.Error(), "changed during discovery") {
		t.Fatalf("scanHeadSnapshot error = %v, want mutation rejection", err)
	}
}
