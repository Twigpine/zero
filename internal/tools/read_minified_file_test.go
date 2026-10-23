package tools

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestReadToolsDescribeExploratoryAndExactUseWithoutRedundantRereads(t *testing.T) {
	root := t.TempDir()
	minified := strings.ToLower(NewScopedReadMinifiedFileTool(root, nil).Description())
	for _, want := range []string{
		"exploratory understanding",
		"use read_file directly for likely edit targets",
		"unless a new need for exact text or line numbers appears",
	} {
		if !strings.Contains(minified, want) {
			t.Fatalf("read_minified_file description missing %q: %s", want, minified)
		}
	}
	exact := strings.ToLower(NewScopedReadFileTool(root, nil).Description())
	for _, want := range []string{
		"use directly for small files likely to be edited",
		"prefer read_minified_file only for exploratory understanding",
	} {
		if !strings.Contains(exact, want) {
			t.Fatalf("read_file description missing %q: %s", want, exact)
		}
	}
}

func TestReadMinifiedFileStripsCommentsAndLineNumbers(t *testing.T) {
	dir := t.TempDir()
	src := "package demo\n\nimport \"fmt\"\n\n// secret doc comment\nfunc F() { fmt.Println(\"x\") }\n"
	if err := os.WriteFile(filepath.Join(dir, "f.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	res := NewScopedReadMinifiedFileTool(dir, nil).Run(context.Background(), map[string]any{"path": "f.go"})
	if res.Status != StatusOK {
		t.Fatalf("status %v: %s", res.Status, res.Output)
	}
	if strings.Contains(res.Output, "secret doc comment") {
		t.Errorf("comment leaked:\n%s", res.Output)
	}
	if !strings.Contains(res.Output, "func F()") {
		t.Errorf("code missing:\n%s", res.Output)
	}
	if strings.Contains(res.Output, " | ") {
		t.Errorf("minified output should carry NO line-number prefixes:\n%s", res.Output)
	}
	if !strings.Contains(res.Output, "minified go view") {
		t.Errorf("expected a minified header note:\n%s", res.Output)
	}
	for _, key := range []string{"mode", "compacted", "raw_bytes", "emitted_bytes", "estimated_tokens_saved"} {
		if res.Meta[key] == "" {
			t.Fatalf("expected compact-read metadata key %q, got %#v", key, res.Meta)
		}
	}
}

func TestReadMinifiedFileRejectsTraversal(t *testing.T) {
	dir := t.TempDir()
	res := NewScopedReadMinifiedFileTool(dir, nil).Run(context.Background(), map[string]any{"path": "../escape.go"})
	if res.Status == StatusOK {
		t.Fatalf("expected traversal rejection, got OK:\n%s", res.Output)
	}
}

func TestReadMinifiedFileSelectsSourceLineRangeBeforeMinifying(t *testing.T) {
	dir := t.TempDir()
	src := "package demo\n\nfunc One() int { return 1 }\n\nfunc Two() int { return 2 }\n"
	if err := os.WriteFile(filepath.Join(dir, "f.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	res := NewScopedReadMinifiedFileTool(dir, nil).Run(context.Background(), map[string]any{
		"path": "f.go", "offset": 5, "limit": 1,
	})
	if res.Status != StatusOK || !strings.Contains(res.Output, "func Two") || strings.Contains(res.Output, "func One") {
		t.Fatalf("unexpected ranged compact read: status=%s\n%s", res.Status, res.Output)
	}
}

func TestReadMinifiedFileRangesPreserveUnknownLexicalContext(t *testing.T) {
	tests := []struct {
		name string
		path string
		src  string
		want []string
	}{
		{
			name: "multiline string",
			path: "snippet.py",
			src:  "value = \"\"\"\n# literal text\nstill literal\n\"\"\"\nprint(value)\n",
			want: []string{"# literal text", "still literal"},
		},
		{
			name: "template literal",
			path: "snippet.js",
			src:  "const value = `\n// literal text\nstill literal\n`;\n",
			want: []string{"// literal text", "still literal"},
		},
		{
			name: "block comment",
			path: "snippet.c",
			src:  "/* open\ncomment body\n*/\nint live;\n",
			want: []string{"comment body", "*/"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, tc.path), []byte(tc.src), 0o644); err != nil {
				t.Fatal(err)
			}
			res := NewScopedReadMinifiedFileTool(dir, nil).Run(context.Background(), map[string]any{
				"path": tc.path, "offset": 2, "limit": 2,
			})
			if res.Status != StatusOK || res.Meta["compacted"] != "false" {
				t.Fatalf("ranged read must use conservative normalization: status=%s meta=%#v\n%s", res.Status, res.Meta, res.Output)
			}
			for _, want := range tc.want {
				if !strings.Contains(res.Output, want) {
					t.Fatalf("ranged read lost lexical content %q:\n%s", want, res.Output)
				}
			}
		})
	}
}

func TestReadMinifiedFileRangeInsideGoRawStringIsConservative(t *testing.T) {
	dir := t.TempDir()
	src := "package demo\nvar value = `first\nSECRET-MARKER-ONE\n// literal line\nlast`\n"
	if err := os.WriteFile(filepath.Join(dir, "f.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	res := NewScopedReadMinifiedFileTool(dir, nil).Run(context.Background(), map[string]any{
		"path": "f.go", "offset": 3, "limit": 2,
	})
	if res.Status != StatusOK || res.Meta["compacted"] != "false" {
		t.Fatalf("raw-string range must use conservative normalization: status=%s meta=%#v\n%s", res.Status, res.Meta, res.Output)
	}
	for _, want := range []string{"SECRET-MARKER-ONE", "// literal line"} {
		if !strings.Contains(res.Output, want) {
			t.Fatalf("raw-string range lost or rewrote %q:\n%s", want, res.Output)
		}
	}
	if strings.Contains(res.Output, "SECRET - MARKER - ONE") {
		t.Fatalf("raw-string contents were parsed as Go code:\n%s", res.Output)
	}
}

func TestReadMinifiedFileCanonicalOffsetPastEnd(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "f.go"), []byte("package demo\nvar value = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	res := NewScopedReadMinifiedFileTool(dir, nil).Run(context.Background(), map[string]any{
		"path": "f.go", "offset": 10,
	})
	if res.Status != StatusOK || !strings.Contains(res.Output, "offset 10 is past the end") {
		t.Fatalf("expected canonical out-of-range response, got status=%s output=%q", res.Status, res.Output)
	}
}

func TestReadMinifiedFileEmptyOffsetPastEnd(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "empty.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	res := NewScopedReadMinifiedFileTool(dir, nil).Run(context.Background(), map[string]any{
		"path": "empty.txt", "offset": 2,
	})
	if res.Status != StatusOK || !strings.Contains(res.Output, "offset 2 is past the end of the file, which has 1 lines") {
		t.Fatalf("expected canonical empty-file out-of-range response, got status=%s output=%q", res.Status, res.Output)
	}
}

func TestReadMinifiedFileNULInClippedLineTail(t *testing.T) {
	dir := t.TempDir()
	line := append(bytes.Repeat([]byte("a"), readMinifiedMaxLineRunes*4), 0)
	line = append(line, []byte("tail\nsecond\n")...)
	line = append(line, bytes.Repeat([]byte("padding\n"), 80000)...)
	if err := os.WriteFile(filepath.Join(dir, "binary.txt"), line, 0o644); err != nil {
		t.Fatal(err)
	}

	tool := NewScopedReadMinifiedFileTool(dir, nil).(optionsAwareTool)
	result := tool.RunWithOptions(context.Background(), map[string]any{"path": "binary.txt", "limit": 2}, RunOptions{})
	if result.Status != StatusOK || result.Meta["binary"] != "true" {
		t.Fatalf("expected clipped NUL to classify as binary: status=%s meta=%#v output=%q", result.Status, result.Meta, result.Output)
	}
}

func TestReadMinifiedFileClippedPageRemainsValidUTF8(t *testing.T) {
	dir := t.TempDir()
	line := strings.Repeat("界", readMinifiedMaxLoadBytes/3+1)
	if err := os.WriteFile(filepath.Join(dir, "unicode.txt"), []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := NewScopedReadMinifiedFileTool(dir, nil).(optionsAwareTool)
	result := tool.RunWithOptions(context.Background(), map[string]any{"path": "unicode.txt", "limit": 1}, RunOptions{})
	if result.Status != StatusOK || !utf8.ValidString(result.Output) {
		t.Fatalf("expected valid UTF-8 output: status=%s valid=%v", result.Status, utf8.ValidString(result.Output))
	}
	if result.Meta["partial_load"] != "true" || result.Meta["clamped_lines"] != "1" {
		t.Fatalf("expected streamed clipped page: meta=%#v output=%q", result.Meta, result.Output)
	}
}

func TestReadMinifiedFileExactEndHasNoContinuation(t *testing.T) {
	dir := t.TempDir()
	line := strings.Repeat("x", readMinifiedMaxWindowBytes+1)
	if err := os.WriteFile(filepath.Join(dir, "exact.txt"), []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := NewScopedReadMinifiedFileTool(dir, nil).(optionsAwareTool)
	result := tool.RunWithOptions(context.Background(), map[string]any{"path": "exact.txt", "limit": 1}, RunOptions{})
	if result.Status != StatusOK || !result.Truncated || strings.Contains(result.Output, "more source lines remain") {
		t.Fatalf("exact-end read should only report line clipping: status=%s truncated=%v output=%q", result.Status, result.Truncated, result.Output)
	}
}

func TestReadMinifiedFileDeepOffsetReportsPrefixScan(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "deep.txt"), []byte(strings.Repeat("line\n", 150000)), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := NewScopedReadMinifiedFileTool(dir, nil).(optionsAwareTool)
	result := tool.RunWithOptions(context.Background(), map[string]any{
		"path": "deep.txt", "offset": 100000, "limit": 1,
	}, RunOptions{})
	if result.Status != StatusOK || result.Meta["partial_load"] != "true" {
		t.Fatalf("expected streamed deep read: status=%s meta=%#v", result.Status, result.Meta)
	}
	if result.Meta["prefix_lines_skipped"] != "99999" || result.Meta["prefix_bytes_scanned"] == "" {
		t.Fatalf("expected prefix scan metadata, got %#v", result.Meta)
	}
	if !strings.Contains(result.Output, "skipped 99999 source line(s)") {
		t.Fatalf("expected deep-offset load note, got %q", result.Output)
	}
}

func TestReadMinifiedFileMaximumLimitDoesNotOverflow(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("alpha\nbeta\ngamma\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	const maxInt = int(^uint(0) >> 1)

	res := NewScopedReadMinifiedFileTool(dir, nil).Run(context.Background(), map[string]any{
		"path": "notes.txt", "offset": 2, "limit": maxInt,
	})
	if res.Status != StatusOK {
		t.Fatalf("maximum limit should return the remaining range without panicking: status=%s output=%q", res.Status, res.Output)
	}
	if !strings.Contains(res.Output, "beta") || !strings.Contains(res.Output, "gamma") || strings.Contains(res.Output, "alpha") {
		t.Fatalf("maximum limit returned the wrong range: %q", res.Output)
	}
}

func TestReadMinifiedFileAppliesByteBudget(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "large.txt"), []byte(strings.Repeat("0123456789abcdef\n", 9000)), 0o644); err != nil {
		t.Fatal(err)
	}

	res := NewScopedReadMinifiedFileTool(dir, nil).Run(context.Background(), map[string]any{"path": "large.txt", "limit": 9000})
	if res.Status != StatusOK || !res.Truncated {
		t.Fatalf("expected ok+truncated, got status=%s truncated=%v", res.Status, res.Truncated)
	}
	if !strings.Contains(res.Output, "output exceeded") || !strings.Contains(res.Output, "read_file") {
		t.Fatalf("expected byte-budget continuation hint, got %q", res.Output[len(res.Output)-200:])
	}
	if res.Meta["truncated"] != "true" {
		t.Fatalf("expected truncation metadata, got %#v", res.Meta)
	}
}

func TestLoadMinifiedFileStreamsOnlyRequestedPrefix(t *testing.T) {
	data := bytes.Repeat([]byte("first line\n"), readMinifiedMaxLoadBytes)
	reader := &countingReadSeeker{ReadSeeker: bytes.NewReader(data)}

	loaded, err := loadMinifiedFile(context.Background(), reader, int64(len(data)), 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.partial || string(loaded.window.content) != "first line" {
		t.Fatalf("loaded=%+v", loaded)
	}
	if reader.bytesRead > 64*1024 {
		t.Fatalf("one-line streamed read consumed %d bytes", reader.bytesRead)
	}
}

func TestReadMinifiedFileHonorsExplicitLimitAcrossLoadModes(t *testing.T) {
	tests := []struct {
		name    string
		line    string
		lines   int
		partial bool
	}{
		{name: "full load", line: "x\n", lines: 5001},
		{name: "streamed", line: strings.Repeat("x", 100) + "\n", lines: 5201, partial: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "many.txt")
			if err := os.WriteFile(path, []byte(strings.Repeat(tc.line, tc.lines)), 0o644); err != nil {
				t.Fatal(err)
			}
			tool := NewScopedReadMinifiedFileTool(dir, nil).(optionsAwareTool)
			result := tool.RunWithOptions(context.Background(), map[string]any{
				"path": "many.txt", "limit": 5000,
			}, RunOptions{})
			if result.Status != StatusOK {
				t.Fatalf("status=%s output=%q", result.Status, result.Output)
			}
			if result.Meta["raw_lines"] != "5000" {
				t.Fatalf("raw_lines=%q want 5000", result.Meta["raw_lines"])
			}
			if !strings.Contains(result.Output, "offset=5001") {
				t.Fatalf("missing continuation from requested range: %q", result.Output)
			}
			if got := result.Meta["partial_load"] == "true"; got != tc.partial {
				t.Fatalf("partial_load=%v want %v", got, tc.partial)
			}
		})
	}
}

func TestReadMinifiedFileDefaultLimitMatchesLoadModes(t *testing.T) {
	content := strings.Repeat("x\n", 2001) + strings.Repeat("padding\n", 80000)
	for _, partial := range []bool{false, true} {
		t.Run(map[bool]string{false: "full load", true: "streamed"}[partial], func(t *testing.T) {
			dir := t.TempDir()
			data := content
			if !partial {
				data = strings.Repeat("x\n", 2001)
			}
			if err := os.WriteFile(filepath.Join(dir, "many.txt"), []byte(data), 0o644); err != nil {
				t.Fatal(err)
			}
			tool := NewScopedReadMinifiedFileTool(dir, nil).(optionsAwareTool)
			result := tool.RunWithOptions(context.Background(), map[string]any{
				"path": "many.txt",
			}, RunOptions{})
			if result.Status != StatusOK || result.Meta["raw_lines"] != "2000" {
				t.Fatalf("status=%s raw_lines=%q meta=%#v", result.Status, result.Meta["raw_lines"], result.Meta)
			}
			if !strings.Contains(result.Output, "offset=2001") {
				t.Fatalf("missing default continuation: %q", result.Output[len(result.Output)-200:])
			}
		})
	}
}

func TestReadMinifiedFileBlankLineAdvancesContinuation(t *testing.T) {
	for _, partial := range []bool{false, true} {
		t.Run(map[bool]string{false: "full load", true: "streamed"}[partial], func(t *testing.T) {
			dir := t.TempDir()
			data := "\nreachable\n"
			if partial {
				data += strings.Repeat("padding\n", 80000)
			}
			if err := os.WriteFile(filepath.Join(dir, "blank.txt"), []byte(data), 0o644); err != nil {
				t.Fatal(err)
			}
			tool := NewScopedReadMinifiedFileTool(dir, nil).(optionsAwareTool)
			result := tool.RunWithOptions(context.Background(), map[string]any{
				"path": "blank.txt", "offset": 1, "limit": 1,
			}, RunOptions{})
			if result.Status != StatusOK || result.Meta["source_lines_emitted"] != "1" {
				t.Fatalf("status=%s meta=%#v output=%q", result.Status, result.Meta, result.Output)
			}
			if !strings.Contains(result.Output, "offset=2") {
				t.Fatalf("missing blank-line continuation: %q", result.Output)
			}
		})
	}
}

func TestReadMinifiedFileReportsSourceByteBudgetContinuation(t *testing.T) {
	dir := t.TempDir()
	line := strings.Repeat("x", 2000) + "\n"
	if err := os.WriteFile(filepath.Join(dir, "wide.txt"), []byte(strings.Repeat(line, 400)), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := NewScopedReadMinifiedFileTool(dir, nil).(optionsAwareTool)
	result := tool.RunWithOptions(context.Background(), map[string]any{
		"path": "wide.txt", "limit": 400,
	}, RunOptions{})
	if result.Status != StatusOK || !result.Truncated {
		t.Fatalf("status=%s truncated=%v", result.Status, result.Truncated)
	}
	if result.Meta["truncation_reason"] != readMinifiedTruncByteBudget {
		t.Fatalf("meta=%#v", result.Meta)
	}
	emitted, err := strconv.Atoi(result.Meta["source_lines_emitted"])
	if err != nil || emitted < 1 || emitted >= 400 {
		t.Fatalf("source_lines_emitted=%q err=%v", result.Meta["source_lines_emitted"], err)
	}
	want := "offset=" + strconv.Itoa(emitted+1)
	if !strings.Contains(result.Output, want) {
		t.Fatalf("missing continuation %q: %q", want, result.Output)
	}
}

func TestLoadMinifiedFileHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	reader := bytes.NewReader(bytes.Repeat([]byte("line\n"), readMinifiedMaxLoadBytes))

	_, err := loadMinifiedFile(ctx, reader, int64(reader.Len()), 1, 1)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v want context.Canceled", err)
	}
}

func TestReadFileLineWindowPropagatesPeekError(t *testing.T) {
	wantErr := errors.New("look-ahead failed")
	reader := &peekErrorReader{wantErr: wantErr}
	_, err := readFileLineWindow(context.Background(), reader, 1, 1)
	if !errors.Is(err, wantErr) {
		t.Fatalf("err=%v want %v", err, wantErr)
	}
}

func TestLoadMinifiedFileBoundsGrownSmallFile(t *testing.T) {
	data := bytes.Repeat([]byte("line\n"), readMinifiedMaxLoadBytes)
	reader := &countingReadSeeker{ReadSeeker: bytes.NewReader(data)}
	loaded, err := loadMinifiedFile(context.Background(), reader, readMinifiedMaxLoadBytes-1, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.partial {
		t.Fatalf("expected streamed fallback after overflow probe: %+v", loaded)
	}
	if reader.bytesRead > 2*(readMinifiedMaxLoadBytes+1)+64*1024 {
		t.Fatalf("grown file consumed %d bytes", reader.bytesRead)
	}
}

func TestLoadMinifiedFileBoundsHugeSingleLine(t *testing.T) {
	data := bytes.Repeat([]byte("x"), 2*readMinifiedMaxWindowBytes)
	reader := &countingReadSeeker{ReadSeeker: bytes.NewReader(data)}
	loaded, err := loadMinifiedFile(context.Background(), reader, int64(len(data)), 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.partial || !loaded.window.hitByteLimit {
		t.Fatalf("expected bounded partial window: %+v", loaded)
	}
	if reader.bytesRead > readMinifiedMaxWindowBytes+1+64*1024 {
		t.Fatalf("single-line stream consumed %d bytes", reader.bytesRead)
	}
}

func TestReadFileLineWindowRecognizesExactPageEOF(t *testing.T) {
	data := bytes.Repeat([]byte("x\n"), readMinifiedMaxWindowBytes/2)
	window, err := readFileLineWindow(context.Background(), bytes.NewReader(data), 1, int(^uint(0)>>1))
	if err != nil {
		t.Fatal(err)
	}
	if window.hitByteLimit || window.emitted != readMinifiedMaxWindowBytes/2 || window.totalLines != window.emitted {
		t.Fatalf("window=%+v", window)
	}
}

func TestReadMinifiedFileShortCircuitsNULInBoundedContent(t *testing.T) {
	tests := []struct {
		name    string
		content []byte
	}{
		{name: "full load", content: append(bytes.Repeat([]byte("text "), 100), append([]byte{0}, []byte("tail")...)...)},
		{name: "streamed page", content: append(bytes.Repeat([]byte("text "), 120), append([]byte{0}, append([]byte("\n"), bytes.Repeat([]byte("padding\n"), 80000)...)...)...)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "binary.txt"), tc.content, 0o644); err != nil {
				t.Fatal(err)
			}
			tool := NewScopedReadMinifiedFileTool(dir, nil).(optionsAwareTool)
			result := tool.RunWithOptions(context.Background(), map[string]any{
				"path": "binary.txt",
			}, RunOptions{})
			if result.Status != StatusOK || result.Meta["binary"] != "true" || strings.Contains(result.Output, "text text") {
				t.Fatalf("expected bounded binary short-circuit: status=%s meta=%#v output=%q", result.Status, result.Meta, result.Output)
			}
		})
	}
}

func TestReadMinifiedFilePartialLoadDoesNotRecordWholeFileVersion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "large.txt")
	if err := os.WriteFile(path, bytes.Repeat([]byte("line\n"), readMinifiedMaxLoadBytes), 0o644); err != nil {
		t.Fatal(err)
	}
	tracker := NewFileTracker()
	tool := NewScopedReadMinifiedFileTool(dir, nil).(optionsAwareTool)
	result := tool.RunWithOptions(context.Background(), map[string]any{
		"path": "large.txt", "limit": 1,
	}, RunOptions{FileTracker: tracker})
	if result.Status != StatusOK {
		t.Fatalf("status=%s output=%q", result.Status, result.Output)
	}
	if _, tracked := tracker.Version(path); tracked {
		t.Fatal("partial read recorded a whole-file version")
	}
}

func TestReadFileLineWindowPropagatesCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := readFileLineWindow(ctx, strings.NewReader("line\n"), 1, 1)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v want context.Canceled", err)
	}
}

type countingReadSeeker struct {
	io.ReadSeeker
	bytesRead int
}

func (reader *countingReadSeeker) Read(buffer []byte) (int, error) {
	n, err := reader.ReadSeeker.Read(buffer)
	reader.bytesRead += n
	return n, err
}

type peekErrorReader struct {
	returned bool
	wantErr  error
}

func (reader *peekErrorReader) Read(buffer []byte) (int, error) {
	if !reader.returned {
		reader.returned = true
		n := copy(buffer, "line\n")
		return n, nil
	}
	return 0, reader.wantErr
}
