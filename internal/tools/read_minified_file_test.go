package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

	res := NewScopedReadMinifiedFileTool(dir, nil).Run(context.Background(), map[string]any{"path": "large.txt"})
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
