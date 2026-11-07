package tools

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// A LINE BREAK ENDS THE LINE IT TRAILS; IT DOES NOT OPEN THE ONE AFTER IT.
//
// The scan used to hand the matcher an empty line past the file's real content
// at EOF, so every pattern that matches an empty string — the anchored ones ^$
// and \b among them — reported a match on a line no reader sees, numbered one
// past the real content, and count mode charged that phantom to its total.
// read_file and the file tracker's countLines both report "x\n" as one line
// and an empty file as none; the grep scan now stops with them.
func TestGrepStopsAtTheLineBreaksTheFileHas(t *testing.T) {
	for name, testCase := range map[string]struct {
		content string
		pattern string
		want    string
	}{
		"a trailing newline closes the line":          {"x\n", `^$`, "content:No matches found."},
		"an unterminated last line stands alone":      {"x", `^$`, "content:No matches found."},
		"an empty file has no lines":                  {"", `^$`, "content:No matches found."},
		"the middle empty line is the only empty one": {"x\n\nz\n", `^$`, "content:a.txt:2: "},
		"dot star does not run past the break":        {"x\n", ".*", "content:a.txt:1: x"},
		"count stops at the trailing break":           {"x\n", `^$`, "count:0 matches found"},
		"count charges only real lines":               {"x\n\nz\n", `^$`, "count:1 matches found"},
		"count takes the trailing empty lines":        {"z\n\n\n", `^$`, "count:2 matches found"},
		"an empty file counts nothing":                {"", `^$`, "count:0 matches found"},
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			writeTestFile(t, filepath.Join(root, "a.txt"), testCase.content)

			mode, want, found := strings.Cut(testCase.want, ":")
			if !found {
				t.Fatalf("SETUP INVALID: %s carries no mode", testCase.want)
			}

			result := NewScopedGrepTool(root, nil).Run(context.Background(), map[string]any{
				"pattern":     testCase.pattern,
				"output_mode": mode,
			})
			if result.Status != StatusOK {
				t.Fatalf("grep status = %s: %s", result.Status, result.Output)
			}
			if result.Output != want {
				t.Fatalf("grep = %q, want %q", result.Output, want)
			}
		})
	}
}

// THE LINE NUMBERS THE SCAN REPORTS ARE THE LINE NUMBERS read_file USES, and
// neither of them runs past the breaks the content carries. The pattern also
// matches an empty line, so the line the scan used to invent at EOF would
// appear right beside the real one: numbering a real line wrong in the same
// file would fail the read side first, and the phantom would fail as a match
// the content does not carry.
func TestGrepLineNumbersAgreeWithReadFile(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "a.txt"), "one\nneedle two\nthree\n")

	read := NewScopedReadFileTool(root, nil).Run(context.Background(), map[string]any{
		"path": "a.txt",
	})
	if read.Status != StatusOK || !strings.Contains(read.Output, "2→needle two") {
		t.Fatalf("SETUP INVALID: read_file does not number the needle line 2: %s", read.Output)
	}
	result := NewScopedGrepTool(root, nil).Run(context.Background(), map[string]any{
		"pattern": "needle|^$",
	})
	if result.Status != StatusOK {
		t.Fatalf("grep status = %s: %s", result.Status, result.Output)
	}
	if result.Output != "a.txt:2: needle two" {
		t.Fatalf("grep = %q, want exactly the line read_file numbers 2; an extra empty match is a line the content does not have, so the scan opened one past the trailing break", result.Output)
	}
}
