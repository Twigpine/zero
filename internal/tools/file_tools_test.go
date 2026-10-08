package tools

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/Gitlawb/zero/internal/sandbox"
)

func TestReadFileToolReadsLineRanges(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "notes.txt"), "alpha\nbeta\ngamma\ndelta")

	result := NewScopedReadFileTool(root, nil).Run(context.Background(), map[string]any{
		"path":       "notes.txt",
		"start_line": 2,
		"max_lines":  2,
	})

	if result.Status != StatusOK {
		t.Fatalf("expected ok status, got %s: %s", result.Status, result.Output)
	}
	for _, want := range []string{
		"File: notes.txt (lines 2-3 of 4)",
		"2→beta",
		"3→gamma",
	} {
		if !strings.Contains(result.Output, want) {
			t.Fatalf("expected output to contain %q, got %q", want, result.Output)
		}
	}
	if strings.Contains(result.Output, "alpha") || strings.Contains(result.Output, "delta") {
		t.Fatalf("line range leaked outside requested slice: %q", result.Output)
	}
}

func TestReadFileToolReadsCanonicalLineRange(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "notes.txt"), "alpha\nbeta\ngamma\ndelta")

	result := NewScopedReadFileTool(root, nil).Run(context.Background(), map[string]any{
		"path":   "notes.txt",
		"offset": 2,
		"limit":  2,
	})

	if result.Status != StatusOK {
		t.Fatalf("expected ok status, got %s: %s", result.Status, result.Output)
	}
	if result.Truncated {
		t.Fatalf("canonical limit defines the requested range and must not mark it truncated: %#v", result.Meta)
	}
	if !strings.Contains(result.Output, "2→beta") || !strings.Contains(result.Output, "3→gamma") {
		t.Fatalf("canonical range returned the wrong lines: %q", result.Output)
	}
	if strings.Contains(result.Output, "alpha") || strings.Contains(result.Output, "delta") {
		t.Fatalf("canonical range leaked outside requested slice: %q", result.Output)
	}
}

func TestReadFileToolCanonicalOffsetPastEnd(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "notes.txt"), "alpha\nbeta")

	result := NewScopedReadFileTool(root, nil).Run(context.Background(), map[string]any{
		"path": "notes.txt", "offset": 10,
	})

	if result.Status != StatusOK || !strings.Contains(result.Output, "offset 10 is past the end") {
		t.Fatalf("expected canonical out-of-range message, got status=%s output=%q", result.Status, result.Output)
	}
	if strings.Contains(result.Output, "start_line") {
		t.Fatalf("out-of-range message exposed legacy argument: %q", result.Output)
	}
}

func TestReadFileToolMixedLegacyRangesPreferLines(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "notes.txt"), "alpha\nbeta\ngamma\ndelta")

	result := NewScopedReadFileTool(root, nil).Run(context.Background(), map[string]any{
		"path":        "notes.txt",
		"start_line":  2,
		"end_line":    3,
		"max_lines":   2,
		"byte_offset": 0,
		"byte_limit":  readFileByteChunkMax,
	})

	if result.Status != StatusOK {
		t.Fatalf("mixed legacy range should recover, got %s: %s", result.Status, result.Output)
	}
	if !strings.Contains(result.Output, "2→beta") || !strings.Contains(result.Output, "3→gamma") {
		t.Fatalf("mixed legacy range did not prefer lines: %q", result.Output)
	}
}

func TestReadFileToolCombinesLegacyStartWithCanonicalLimit(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "notes.txt"), "alpha\nbeta\ngamma\ndelta")

	result := NewScopedReadFileTool(root, nil).Run(context.Background(), map[string]any{
		"path": "notes.txt", "start_line": 2, "limit": 2,
	})

	if result.Status != StatusOK || result.Truncated {
		t.Fatalf("mixed compatible range should be exact, got status=%s truncated=%v: %s", result.Status, result.Truncated, result.Output)
	}
	if !strings.Contains(result.Output, "2→beta") || !strings.Contains(result.Output, "3→gamma") {
		t.Fatalf("mixed compatible range returned the wrong lines: %q", result.Output)
	}
	if strings.Contains(result.Output, "alpha") || strings.Contains(result.Output, "delta") {
		t.Fatalf("mixed compatible range leaked outside requested slice: %q", result.Output)
	}
}

func TestFileToolSchemasExposeOnlyCanonicalArguments(t *testing.T) {
	readProperties := NewScopedReadFileTool(t.TempDir(), nil).Parameters().Properties
	for _, want := range []string{"path", "offset", "limit", "byte_offset", "byte_limit"} {
		if _, ok := readProperties[want]; !ok {
			t.Fatalf("read_file schema missing %q", want)
		}
	}
	for _, legacy := range []string{"start_line", "end_line", "max_lines"} {
		if _, ok := readProperties[legacy]; ok {
			t.Fatalf("read_file schema must not expose legacy argument %q", legacy)
		}
	}

	grepProperties := NewScopedGrepTool(t.TempDir(), nil).Parameters().Properties
	if _, ok := grepProperties["case_insensitive"]; !ok {
		t.Fatal("grep schema missing case_insensitive")
	}
	if _, ok := grepProperties["-i"]; ok {
		t.Fatal("grep schema must not expose the -i alias")
	}
}

func TestReadFileToolMarksTruncation(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "notes.txt"), "a\nb\nc\nd\ne")

	result := NewScopedReadFileTool(root, nil).Run(context.Background(), map[string]any{
		"path":      "notes.txt",
		"max_lines": 2,
	})

	if result.Status != StatusOK || !result.Truncated {
		t.Fatalf("expected ok+truncated, got status=%s truncated=%v", result.Status, result.Truncated)
	}
	// The cut must be visible in the rendered output, not just the Truncated flag,
	// and must tell the model where to continue.
	if !strings.Contains(result.Output, "[truncated:") || !strings.Contains(result.Output, "offset=3") {
		t.Fatalf("expected truncation marker pointing to the next line, got %q", result.Output)
	}
}

func TestReadFileToolAppliesByteBudget(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "large.txt"), strings.Repeat("0123456789abcdef\n", 9000))

	result := NewScopedReadFileTool(root, nil).Run(context.Background(), map[string]any{
		"path": "large.txt",
	})

	if result.Status != StatusOK || !result.Truncated {
		t.Fatalf("expected ok+truncated, got status=%s truncated=%v", result.Status, result.Truncated)
	}
	if !strings.Contains(result.Output, "output exceeded") || !strings.Contains(result.Output, "offset/limit") {
		t.Fatalf("expected byte-budget continuation hint, got %q", result.Output[len(result.Output)-200:])
	}
	if result.Meta["raw_bytes"] == "" || result.Meta["emitted_bytes"] == "" {
		t.Fatalf("expected output byte metadata, got %#v", result.Meta)
	}
}

func TestReadFileToolTracksWholeFileHashWhenReadingSmallRange(t *testing.T) {
	root := t.TempDir()
	var builder strings.Builder
	for i := 1; i <= 2000; i++ {
		builder.WriteString("line ")
		builder.WriteString(strings.Repeat("x", 16))
		builder.WriteString("\n")
	}
	content := builder.String()
	path := filepath.Join(root, "large.txt")
	writeTestFile(t, path, content)

	tracker := NewFileTracker()
	tool := NewScopedReadFileTool(root, nil).(optionsAwareTool)
	result := tool.RunWithOptions(context.Background(), map[string]any{
		"path":       "large.txt",
		"start_line": 1000,
		"max_lines":  2,
	}, RunOptions{FileTracker: tracker})

	if result.Status != StatusOK {
		t.Fatalf("expected ok status, got %s: %s", result.Status, result.Output)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	version, ok := tracker.Version(resolved)
	if !ok {
		t.Fatal("expected read_file to record a file version")
	}
	if version.Hash != HashContent([]byte(content)) {
		t.Fatalf("hash = %q, want %q", version.Hash, HashContent([]byte(content)))
	}
	if err := tracker.CheckConflict(resolved, []byte(content+"changed")); err != ErrFileChangedOnDisk {
		t.Fatalf("changed content should conflict, got %v", err)
	}
}

func TestReadFileToolRejectsOutsideWorkspace(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.txt")
	writeTestFile(t, outside, "secret")

	result := NewScopedReadFileTool(root, nil).Run(context.Background(), map[string]any{
		"path": outside,
	})

	if result.Status != StatusError {
		t.Fatalf("expected error status, got %s", result.Status)
	}
	if !strings.Contains(result.Output, "must stay inside the workspace") {
		t.Fatalf("expected workspace error, got %q", result.Output)
	}
}

func TestListDirectoryToolListsRecursivelyAndIgnoresJunk(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "src", "main.go"), "package main")
	writeTestFile(t, filepath.Join(root, "node_modules", "leftpad", "index.js"), "module.exports = 1")
	writeTestFile(t, filepath.Join(root, "README.md"), "# Zero")

	result := NewScopedListDirectoryTool(root, nil).Run(context.Background(), map[string]any{
		"path":      ".",
		"recursive": true,
		"max_depth": 2,
	})

	if result.Status != StatusOK {
		t.Fatalf("expected ok status, got %s: %s", result.Status, result.Output)
	}
	if !strings.Contains(result.Output, "src/") || !strings.Contains(result.Output, "main.go") {
		t.Fatalf("expected recursive source listing, got %q", result.Output)
	}
	if !strings.Contains(result.Output, "README.md") {
		t.Fatalf("expected README.md, got %q", result.Output)
	}
	if strings.Contains(result.Output, "node_modules") || strings.Contains(result.Output, "leftpad") {
		t.Fatalf("expected ignored junk directory, got %q", result.Output)
	}
}

func TestGlobToolFindsMatchesWithLimit(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "a.go"), "package zero")
	writeTestFile(t, filepath.Join(root, "nested", "b.go"), "package nested")
	writeTestFile(t, filepath.Join(root, "nested", "c.txt"), "text")

	result := NewScopedGlobTool(root, nil).Run(context.Background(), map[string]any{
		"pattern": "**/*.go",
		"limit":   1,
	})

	if result.Status != StatusOK {
		t.Fatalf("expected ok status, got %s: %s", result.Status, result.Output)
	}
	if result.Truncated != true {
		t.Fatalf("expected truncated result")
	}
	if !strings.Contains(result.Output, "[truncated: showing first 1 of 2 matches") {
		t.Fatalf("expected visible glob truncation marker, got %q", result.Output)
	}
	matchedPaths := regexp.MustCompile(`(?m)^[^\n]*\.go\b`).FindAllString(result.Output, -1)
	if got := len(matchedPaths); got != 1 {
		t.Fatalf("expected exactly one go match, got %d in %q", got, result.Output)
	}
}

func TestGlobToolCanIncludeDirectoryMatches(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "src"), 0o755); err != nil {
		t.Fatal(err)
	}

	result := NewScopedGlobTool(root, nil).Run(context.Background(), map[string]any{
		"pattern":      "src",
		"include_dirs": true,
	})

	if result.Status != StatusOK {
		t.Fatalf("expected ok status, got %s: %s", result.Status, result.Output)
	}
	if strings.TrimSpace(result.Output) != "src" {
		t.Fatalf("expected src directory match, got %q", result.Output)
	}
}

func TestGrepToolSearchesContent(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "cmd", "main.go"), "package main\nfunc main() {}\n")
	writeTestFile(t, filepath.Join(root, "README.md"), "main docs\n")

	result := NewScopedGrepTool(root, nil).Run(context.Background(), map[string]any{
		"pattern":    "func main",
		"path":       ".",
		"glob":       "**/*.go",
		"head_limit": 5,
	})

	if result.Status != StatusOK {
		t.Fatalf("expected ok status, got %s: %s", result.Status, result.Output)
	}
	if !strings.Contains(result.Output, "cmd/main.go:2: func main() {}") {
		t.Fatalf("expected formatted grep result, got %q", result.Output)
	}
	if strings.Contains(result.Output, "README.md") {
		t.Fatalf("glob filter leaked README match: %q", result.Output)
	}
}

func TestGrepToolMakesHeadLimitTruncationVisible(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "notes.txt"), "needle 1\nneedle 2\nneedle 3\n")

	result := NewScopedGrepTool(root, nil).Run(context.Background(), map[string]any{
		"pattern":    "needle",
		"path":       ".",
		"head_limit": 2,
	})

	if result.Status != StatusOK || !result.Truncated {
		t.Fatalf("expected ok+truncated, got status=%s truncated=%v output=%q", result.Status, result.Truncated, result.Output)
	}
	if !strings.Contains(result.Output, "[truncated: showing first 2 matches") {
		t.Fatalf("expected visible grep truncation marker, got %q", result.Output)
	}
	if strings.Contains(result.Output, " of 3 matches") {
		t.Fatalf("content truncation marker must not claim exact total, got %q", result.Output)
	}
	if result.Meta["truncation_reason"] != "head_limit" {
		t.Fatalf("expected head_limit truncation reason, got %v", result.Meta)
	}
}

func TestGrepToolStopsAfterHeadLimitInContentMode(t *testing.T) {
	root := t.TempDir()
	var builder strings.Builder
	for i := 0; i < 100; i++ {
		builder.WriteString("needle ")
		builder.WriteString(strings.Repeat("x", 8))
		builder.WriteString("\n")
	}
	writeTestFile(t, filepath.Join(root, "notes.txt"), builder.String())

	result := NewScopedGrepTool(root, nil).Run(context.Background(), map[string]any{
		"pattern":    "needle",
		"path":       ".",
		"head_limit": 3,
	})

	if result.Status != StatusOK || !result.Truncated {
		t.Fatalf("expected ok+truncated, got status=%s truncated=%v output=%q", result.Status, result.Truncated, result.Output)
	}
	if !strings.Contains(result.Output, "[truncated: showing first 3 matches") {
		t.Fatalf("expected visible grep truncation marker, got %q", result.Output)
	}
	if strings.Contains(result.Output, " of 100 matches") {
		t.Fatalf("content truncation marker must not claim exact total, got %q", result.Output)
	}
	if strings.Contains(result.Output, "notes.txt:4:") {
		t.Fatalf("head_limit leaked fourth result: %q", result.Output)
	}
	if result.Meta["truncation_reason"] != "head_limit" {
		t.Fatalf("expected head_limit truncation reason, got %v", result.Meta)
	}
}

func TestGrepContentScanStopsAfterHeadLimitPlusOne(t *testing.T) {
	root := t.TempDir()
	var builder strings.Builder
	for i := 0; i < 100; i++ {
		builder.WriteString("needle\n")
	}
	writeTestFile(t, filepath.Join(root, "notes.txt"), builder.String())
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}

	matcherCalls := 0
	collector := &grepContentCollector{headLimit: 3}
	err = scanGrepMatches(context.Background(), resolvedRoot, root, nil, readExcluder{}, false, func([]byte) (int, bool) {
		matcherCalls++
		return 1, true
	}, collector.collect)
	if err != nil {
		t.Fatalf("scanGrepMatches: %v", err)
	}
	if matcherCalls != 4 {
		t.Fatalf("matcher calls = %d, want head_limit+1", matcherCalls)
	}
	if !collector.truncated {
		t.Fatal("collector should mark content results truncated")
	}
}

// A glob is matched relative to the search directory, not the workspace root, so
// `path=subdir glob=*.go` finds subdir/a.go (as "a.go"). Previously the glob was
// matched against the workspace-relative "subdir/a.go", so a non-recursive "*.go"
// found nothing.
func TestGrepGlobMatchesRelativeToSearchDir(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "subdir", "a.go"), "package sub\nfunc target() {}\n")

	result := NewScopedGrepTool(root, nil).Run(context.Background(), map[string]any{
		"pattern": "func target",
		"path":    "subdir",
		"glob":    "*.go",
	})

	if result.Status != StatusOK {
		t.Fatalf("expected ok status, got %s: %s", result.Status, result.Output)
	}
	if !strings.Contains(result.Output, "subdir/a.go:2: func target() {}") {
		t.Fatalf("expected subdir match with non-recursive glob, got %q", result.Output)
	}
}

func TestGrepToolSupportsFilesAndCountModes(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "a.txt"), "needle\nneedle\n")
	writeTestFile(t, filepath.Join(root, "b.txt"), "needle\n")

	files := NewScopedGrepTool(root, nil).Run(context.Background(), map[string]any{
		"pattern":     "needle",
		"output_mode": "files_with_matches",
	})
	if files.Status != StatusOK {
		t.Fatalf("expected files result, got %s: %s", files.Status, files.Output)
	}
	if !strings.Contains(files.Output, "a.txt") || !strings.Contains(files.Output, "b.txt") {
		t.Fatalf("expected both files, got %q", files.Output)
	}

	count := NewScopedGrepTool(root, nil).Run(context.Background(), map[string]any{
		"pattern":     "needle",
		"output_mode": "count",
	})
	if count.Status != StatusOK {
		t.Fatalf("expected count result, got %s: %s", count.Status, count.Output)
	}
	if count.Output != "3 matches found" {
		t.Fatalf("expected count output, got %q", count.Output)
	}
}

func TestGrepCountModeCountsMultipleHitsPerLine(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "notes.txt"), "needle needle\nneedle\n")

	count := NewScopedGrepTool(root, nil).Run(context.Background(), map[string]any{
		"pattern":     "needle",
		"output_mode": "count",
	})
	if count.Status != StatusOK {
		t.Fatalf("expected count result, got %s: %s", count.Status, count.Output)
	}
	if count.Output != "3 matches found" {
		t.Fatalf("expected count output, got %q", count.Output)
	}
}

// Finding 1: grep must not follow an in-workspace symlink that points to a file
// OUTSIDE the workspace (confinement bypass). The symlinked secret must not be
// searched or returned, mirroring read_file's EvalSymlinks confinement.
func TestGrepDoesNotFollowSymlinkOutsideWorkspace(t *testing.T) {
	root := t.TempDir()
	outsideDir := t.TempDir()
	secret := filepath.Join(outsideDir, "secret.txt")
	writeTestFile(t, secret, "needle leaked from outside\n")
	writeTestFile(t, filepath.Join(root, "keep.txt"), "needle inside\n")

	link := filepath.Join(root, "escape.txt")
	if err := os.Symlink(secret, link); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}

	res := NewScopedGrepTool(root, nil).Run(context.Background(), map[string]any{
		"pattern":     "needle",
		"output_mode": "content",
	})
	if res.Status != StatusOK {
		t.Fatalf("status=%s output=%s", res.Status, res.Output)
	}
	if strings.Contains(res.Output, "leaked from outside") || strings.Contains(res.Output, "escape.txt") {
		t.Fatalf("grep followed symlink outside workspace, leaked:\n%s", res.Output)
	}
	if !strings.Contains(res.Output, "keep.txt") {
		t.Fatalf("expected in-workspace match, got:\n%s", res.Output)
	}
}

// Finding 2: when the workspace root itself lives under a symlink (e.g. macOS
// /tmp -> /private/tmp), match paths must be clean workspace-relative paths with
// NO leading "../" — because the walked paths are EvalSymlinks-resolved while the
// root was previously only Abs-normalized.
func TestGrepReturnsCleanRelativePathsUnderSymlinkedRoot(t *testing.T) {
	realDir := t.TempDir()
	writeTestFile(t, filepath.Join(realDir, "pkg", "main.go"), "func main() {}\n")

	linkRoot := filepath.Join(t.TempDir(), "ws")
	if err := os.Symlink(realDir, linkRoot); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}

	res := NewScopedGrepTool(linkRoot, nil).Run(context.Background(), map[string]any{
		"pattern":     "func main",
		"output_mode": "content",
	})
	if res.Status != StatusOK {
		t.Fatalf("status=%s output=%s", res.Status, res.Output)
	}
	if strings.Contains(res.Output, "../") || strings.HasPrefix(strings.TrimSpace(res.Output), "/") {
		t.Fatalf("expected clean workspace-relative path, got:\n%s", res.Output)
	}
	if !strings.Contains(res.Output, "pkg/main.go:1: func main") {
		t.Fatalf("expected pkg/main.go match, got:\n%s", res.Output)
	}

	// files_with_matches mode must likewise be clean-relative.
	res = NewScopedGrepTool(linkRoot, nil).Run(context.Background(), map[string]any{
		"pattern":     "func main",
		"output_mode": "files_with_matches",
	})
	if strings.TrimSpace(res.Output) != "pkg/main.go" {
		t.Fatalf("expected pkg/main.go, got %q", res.Output)
	}
}

func TestScopedToolsAllowExtraRootWrites(t *testing.T) {
	workspace := t.TempDir()
	extra := tempDirOutsideDefaultTemp(t)
	scope, err := sandbox.NewScope(workspace, []string{extra})
	if err != nil {
		t.Fatalf("NewScope: %v", err)
	}
	target := filepath.Join(extra, "saved.txt")

	res := NewScopedWriteFileTool(workspace, scope).Run(context.Background(), map[string]any{
		"path":    target,
		"content": "hello",
	})
	if res.Status != StatusOK {
		t.Fatalf("scoped write_file status=%s output=%s", res.Status, res.Output)
	}
	read := NewScopedReadFileTool(workspace, scope).Run(context.Background(), map[string]any{"path": target})
	if read.Status != StatusOK || !strings.Contains(read.Output, "hello") {
		t.Fatalf("scoped read_file status=%s output=%s", read.Status, read.Output)
	}
}

func TestScopedToolsAllowReadOnlyRootsWithoutWrite(t *testing.T) {
	workspace := t.TempDir()
	extra := tempDirOutsideDefaultTemp(t)
	scope, err := sandbox.NewScope(workspace, nil)
	if err != nil {
		t.Fatalf("NewScope: %v", err)
	}
	if _, err := scope.AddRead(extra); err != nil {
		t.Fatalf("AddRead: %v", err)
	}
	target := filepath.Join(extra, "readable.txt")
	if err := os.WriteFile(target, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	read := NewScopedReadFileTool(workspace, scope).Run(context.Background(), map[string]any{"path": target})
	if read.Status != StatusOK || !strings.Contains(read.Output, "hello") {
		t.Fatalf("read-only root read_file status=%s output=%s", read.Status, read.Output)
	}
	write := NewScopedWriteFileTool(workspace, scope).Run(context.Background(), map[string]any{
		"path":      filepath.Join(extra, "created.txt"),
		"content":   "no",
		"overwrite": true,
	})
	if write.Status != StatusError {
		t.Fatalf("read-only root write_file status=%s output=%s, want error", write.Status, write.Output)
	}
}

func TestScopedToolsKeepRelativePathsInWorkspace(t *testing.T) {
	workspace := t.TempDir()
	extra := tempDirOutsideDefaultTemp(t)
	scope, err := sandbox.NewScope(workspace, []string{extra})
	if err != nil {
		t.Fatalf("NewScope: %v", err)
	}
	res := NewScopedWriteFileTool(workspace, scope).Run(context.Background(), map[string]any{
		"path":    "rel.txt",
		"content": "workspace",
	})
	if res.Status != StatusOK {
		t.Fatalf("relative write status=%s output=%s", res.Status, res.Output)
	}
	if _, err := os.Stat(filepath.Join(workspace, "rel.txt")); err != nil {
		t.Fatalf("relative path must land in workspace: %v", err)
	}
}

func TestScopedGlobReturnsAbsoluteMatchesForExtraRoot(t *testing.T) {
	workspace := t.TempDir()
	extra := tempDirOutsideDefaultTemp(t)
	scope, err := sandbox.NewScope(workspace, []string{extra})
	if err != nil {
		t.Fatalf("NewScope: %v", err)
	}
	// Same relative name in both roots: a bare "report.go" match from the extra
	// root would resolve back into the workspace and hit the wrong file.
	writeTestFile(t, filepath.Join(workspace, "report.go"), "package ws")
	writeTestFile(t, filepath.Join(extra, "report.go"), "package extra")

	tool := NewScopedGlobTool(workspace, scope)

	// Globbing the extra root (absolute cwd) must emit absolute matches.
	extraRes := tool.Run(context.Background(), map[string]any{
		"pattern": "**/*.go",
		"cwd":     extra,
	})
	if extraRes.Status != StatusOK {
		t.Fatalf("extra-root glob status=%s output=%s", extraRes.Status, extraRes.Output)
	}
	// Matches report the symlink-resolved root (macOS /var -> /private/var).
	resolvedExtra, err := filepath.EvalSymlinks(extra)
	if err != nil {
		t.Fatalf("EvalSymlinks(extra): %v", err)
	}
	wantAbs := filepath.ToSlash(filepath.Join(resolvedExtra, "report.go"))
	if strings.TrimSpace(extraRes.Output) != wantAbs {
		t.Fatalf("extra-root glob output=%q, want absolute %q", extraRes.Output, wantAbs)
	}

	// Globbing the workspace (default cwd) keeps matches workspace-relative.
	wsRes := tool.Run(context.Background(), map[string]any{"pattern": "**/*.go"})
	if wsRes.Status != StatusOK {
		t.Fatalf("workspace glob status=%s output=%s", wsRes.Status, wsRes.Output)
	}
	if strings.TrimSpace(wsRes.Output) != "report.go" {
		t.Fatalf("workspace glob output=%q, want relative %q", wsRes.Output, "report.go")
	}
}

func TestScopedGrepReturnsAbsoluteMatchesForExtraRoot(t *testing.T) {
	workspace := t.TempDir()
	extra := tempDirOutsideDefaultTemp(t)
	scope, err := sandbox.NewScope(workspace, []string{extra})
	if err != nil {
		t.Fatalf("NewScope: %v", err)
	}
	// Same relative name in both roots: a bare "report.go" match from the extra
	// root would resolve back into the workspace and hit the wrong file.
	writeTestFile(t, filepath.Join(workspace, "report.go"), "needle in workspace")
	writeTestFile(t, filepath.Join(extra, "report.go"), "needle in extra")

	tool := NewScopedGrepTool(workspace, scope)

	// Grepping the extra root (absolute path) must emit absolute file paths.
	extraRes := tool.Run(context.Background(), map[string]any{
		"pattern": "needle",
		"path":    extra,
	})
	if extraRes.Status != StatusOK {
		t.Fatalf("extra-root grep status=%s output=%s", extraRes.Status, extraRes.Output)
	}
	// Matches report the symlink-resolved root (macOS /var -> /private/var).
	resolvedExtra, err := filepath.EvalSymlinks(extra)
	if err != nil {
		t.Fatalf("EvalSymlinks(extra): %v", err)
	}
	wantAbs := filepath.ToSlash(filepath.Join(resolvedExtra, "report.go"))
	if !strings.HasPrefix(strings.TrimSpace(extraRes.Output), wantAbs+":") {
		t.Fatalf("extra-root grep output=%q, want absolute path %q", extraRes.Output, wantAbs)
	}

	// files_with_matches mode must also report the absolute path.
	filesRes := tool.Run(context.Background(), map[string]any{
		"pattern":     "needle",
		"path":        extra,
		"output_mode": "files_with_matches",
	})
	if strings.TrimSpace(filesRes.Output) != wantAbs {
		t.Fatalf("extra-root files_with_matches output=%q, want absolute %q", filesRes.Output, wantAbs)
	}

	// Grepping the workspace (default path) keeps matches workspace-relative.
	wsRes := tool.Run(context.Background(), map[string]any{"pattern": "needle"})
	if wsRes.Status != StatusOK {
		t.Fatalf("workspace grep status=%s output=%s", wsRes.Status, wsRes.Output)
	}
	if !strings.HasPrefix(strings.TrimSpace(wsRes.Output), "report.go:") {
		t.Fatalf("workspace grep output=%q, want workspace-relative %q", wsRes.Output, "report.go:")
	}
}

func TestUnscopedWriteRefusesInRootSymlinkTraversal(t *testing.T) {
	workspace := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workspace, "subdir"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	link := filepath.Join(workspace, "link")
	if err := os.Symlink(filepath.Join(workspace, "subdir"), link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	res := NewScopedWriteFileTool(workspace, nil).Run(context.Background(), map[string]any{
		"path":    filepath.Join(link, "x.txt"),
		"content": "nope",
	})
	if res.Status == StatusOK {
		t.Fatalf("write through in-root symlink must fail (fail-closed write targets), got OK: %s", res.Output)
	}
	if _, err := os.Stat(filepath.Join(workspace, "subdir", "x.txt")); err == nil {
		t.Fatal("file must not be created through the symlink")
	}
}

func TestScopedWriteThroughSymlinkIntoGrantedRoot(t *testing.T) {
	workspace := t.TempDir()
	extra := tempDirOutsideDefaultTemp(t)
	link := filepath.Join(workspace, "into-extra")
	if err := os.Symlink(extra, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	scope, err := sandbox.NewScope(workspace, []string{extra})
	if err != nil {
		t.Fatalf("NewScope: %v", err)
	}
	// Final target inside a granted root: allowed (matches sandbox.Scope's
	// documented widening — the true write location is granted).
	res := NewScopedWriteFileTool(workspace, scope).Run(context.Background(), map[string]any{
		"path":    filepath.Join(link, "ok.txt"),
		"content": "granted",
	})
	if res.Status != StatusOK {
		t.Fatalf("write through symlink into granted root: status=%s output=%s", res.Status, res.Output)
	}
	// A symlink escaping a granted root to ungated territory stays denied.
	escape := filepath.Join(extra, "out")
	if err := os.Symlink(filepath.Join(tempDirOutsideDefaultTemp(t), "target"), escape); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	res = NewScopedWriteFileTool(workspace, scope).Run(context.Background(), map[string]any{
		"path":    filepath.Join(escape, "leak.txt"),
		"content": "nope",
	})
	if res.Status == StatusOK {
		t.Fatalf("write through escaping symlink must fail, got OK: %s", res.Output)
	}
}

func TestUnscopedToolsStillRejectOutsideWrites(t *testing.T) {
	workspace := t.TempDir()
	outside := filepath.Join(t.TempDir(), "escape.txt")
	res := NewScopedWriteFileTool(workspace, nil).Run(context.Background(), map[string]any{
		"path":    outside,
		"content": "nope",
	})
	if res.Status == StatusOK {
		t.Fatalf("unscoped write outside workspace must fail, got OK: %s", res.Output)
	}
}

func writeTestFile(t *testing.T, path string, content string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestGrepSkipsAlwaysExcludedDirectories(t *testing.T) {
	root := t.TempDir()
	mustWrite := func(rel, body string) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite("keep.txt", "needle here")
	mustWrite(".git/config", "needle here")
	mustWrite(".zero/state.json", "needle here")
	mustWrite("node_modules/pkg/index.js", "needle here")
	mustWrite("vendor/pkg/lib.go", "needle here")
	mustWrite(".worktrees/branch/main.go", "needle here")

	res := NewScopedGrepTool(root, nil).Run(context.Background(), map[string]any{
		"pattern":     "needle",
		"output_mode": "files_with_matches",
	})
	if res.Status != StatusOK {
		t.Fatalf("status=%s output=%s", res.Status, res.Output)
	}
	for _, forbidden := range []string{".git", ".zero", "node_modules", "vendor", ".worktrees"} {
		if strings.Contains(res.Output, forbidden) {
			t.Fatalf("grep must not descend into excluded dir %q, got:\n%s", forbidden, res.Output)
		}
	}
	if !strings.Contains(res.Output, "keep.txt") {
		t.Fatalf("expected keep.txt in results, got:\n%s", res.Output)
	}
}

func TestScopedWriteRefusesSameRootSymlinkTraversal(t *testing.T) {
	workspace := t.TempDir()
	extra := tempDirOutsideDefaultTemp(t)
	if err := os.MkdirAll(filepath.Join(extra, "subdir"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	link := filepath.Join(extra, "link")
	if err := os.Symlink(filepath.Join(extra, "subdir"), link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	scope, err := sandbox.NewScope(workspace, []string{extra})
	if err != nil {
		t.Fatalf("NewScope: %v", err)
	}
	res := NewScopedWriteFileTool(workspace, scope).Run(context.Background(), map[string]any{
		"path":    filepath.Join(link, "x.txt"),
		"content": "nope",
	})
	if res.Status == StatusOK {
		t.Fatalf("scoped write through same-root symlink must fail, got OK: %s", res.Output)
	}
	if _, err := os.Stat(filepath.Join(extra, "subdir", "x.txt")); err == nil {
		t.Fatal("file must not be created through the symlink")
	}
}

func TestScopedWriteReportsAbsolutePathForExtraRoot(t *testing.T) {
	workspace := t.TempDir()
	extra := tempDirOutsideDefaultTemp(t)
	scope, err := sandbox.NewScope(workspace, []string{extra})
	if err != nil {
		t.Fatalf("NewScope: %v", err)
	}
	target := filepath.Join(extra, "abs.txt")
	res := NewScopedWriteFileTool(workspace, scope).Run(context.Background(), map[string]any{
		"path":    target,
		"content": "x",
	})
	if res.Status != StatusOK {
		t.Fatalf("status=%s output=%s", res.Status, res.Output)
	}
	for _, changed := range res.ChangedFiles {
		if !filepath.IsAbs(changed) {
			t.Fatalf("ChangedFiles=%v — extra-root entries must be absolute, got relative %q", res.ChangedFiles, changed)
		}
	}
	if len(res.ChangedFiles) == 0 {
		t.Fatal("expected ChangedFiles to record the extra-root write")
	}
}

func TestGrepSkipsBinaryLikeFiles(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "keep.txt"), "needle here")
	writeTestFile(t, filepath.Join(root, "image.png"), "needle hidden")
	writeTestFile(t, filepath.Join(root, "archive.zip"), "needle hidden")

	res := NewScopedGrepTool(root, nil).Run(context.Background(), map[string]any{
		"pattern":     "needle",
		"output_mode": "files_with_matches",
	})
	if res.Status != StatusOK {
		t.Fatalf("status=%s output=%s", res.Status, res.Output)
	}
	if strings.Contains(res.Output, "image.png") || strings.Contains(res.Output, "archive.zip") {
		t.Fatalf("grep must skip binary-like files, got:\n%s", res.Output)
	}
	if strings.TrimSpace(res.Output) != "keep.txt" {
		t.Fatalf("expected only keep.txt, got:\n%s", res.Output)
	}

	direct := NewScopedGrepTool(root, nil).Run(context.Background(), map[string]any{
		"pattern": "needle",
		"path":    "image.png",
	})
	if direct.Status != StatusOK {
		t.Fatalf("direct status=%s output=%s", direct.Status, direct.Output)
	}
	if strings.Contains(direct.Output, "image.png") || strings.Contains(direct.Output, "needle hidden") {
		t.Fatalf("direct binary-like grep should be skipped, got:\n%s", direct.Output)
	}
}

func TestGlobSkipsWorkspaceExcludedDirectoriesAndBinaryFiles(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "keep.txt"), "keep")
	writeTestFile(t, filepath.Join(root, ".zero", "state.json"), "{}")
	writeTestFile(t, filepath.Join(root, "vendor", "pkg", "lib.go"), "package lib")
	writeTestFile(t, filepath.Join(root, ".worktrees", "branch", "main.go"), "package main")
	writeTestFile(t, filepath.Join(root, "image.png"), "binary")

	res := NewScopedGlobTool(root, nil).Run(context.Background(), map[string]any{
		"pattern": "**/*",
		"limit":   20,
	})
	if res.Status != StatusOK {
		t.Fatalf("status=%s output=%s", res.Status, res.Output)
	}
	for _, forbidden := range []string{".zero", "vendor", ".worktrees", "image.png"} {
		if strings.Contains(res.Output, forbidden) {
			t.Fatalf("glob must skip %q, got:\n%s", forbidden, res.Output)
		}
	}
	if strings.TrimSpace(res.Output) != "keep.txt" {
		t.Fatalf("expected only keep.txt, got:\n%s", res.Output)
	}
}

// A backwards range (end_line < start_line) is a caller-arithmetic slip, not a
// fatal error: read_file recovers by reading just start_line and surfaces a note
// explaining the adjustment, matching how an end_line past EOF is auto-clamped.
// It must never hard-error the way it used to.
func TestReadFileToolRecoversBackwardsRange(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "notes.txt"), "alpha\nbeta\ngamma\ndelta\nepsilon")

	result := NewReadFileTool(root).Run(context.Background(), map[string]any{
		"path":       "notes.txt",
		"start_line": 3,
		"end_line":   2, // backwards
	})

	if result.Status != StatusOK {
		t.Fatalf("backwards range must recover, not error; got %s: %s", result.Status, result.Output)
	}
	if strings.Contains(result.Output, "must be greater than or equal to start_line") {
		t.Fatalf("backwards range still hard-errors: %q", result.Output)
	}
	for _, want := range []string{
		"end_line 2 was before start_line 3", // the note
		"only line 3 was read",
		"3→gamma",
	} {
		if !strings.Contains(result.Output, want) {
			t.Fatalf("expected %q in output, got %q", want, result.Output)
		}
	}
	// It read exactly start_line, nothing adjacent.
	if strings.Contains(result.Output, "beta") || strings.Contains(result.Output, "delta") {
		t.Fatalf("recovery read the wrong lines: %q", result.Output)
	}
}

// A valid range with end_line >= start_line is byte-for-byte unchanged: the
// recovery path must not touch normal reads (no stray note, exact same window).
func TestReadFileToolValidRangeUnchangedByRecovery(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "notes.txt"), "alpha\nbeta\ngamma\ndelta\nepsilon")

	result := NewReadFileTool(root).Run(context.Background(), map[string]any{
		"path":       "notes.txt",
		"start_line": 2,
		"end_line":   4,
	})
	if result.Status != StatusOK {
		t.Fatalf("valid range failed: %s: %s", result.Status, result.Output)
	}
	// Byte-for-byte: header, blank separator, the exact selected lines, and no
	// recovery note or trailing content. An altered header/separator or a stray
	// out-of-range line would fail this where a substring check would not.
	const want = "File: notes.txt (lines 2-4 of 5)\n\n2→beta\n3→gamma\n4→delta"
	if result.Output != want {
		t.Fatalf("valid-range output changed by the recovery path:\n got: %q\nwant: %q", result.Output, want)
	}
}
