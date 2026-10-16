package repomap

import (
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestScanBuildsDeterministicSnapshot(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "go.mod", "module example.test/repo\n")
	writeFile(t, root, "README.md", "# Example\n")
	writeFile(t, root, "package.json", `{"name":"example"}`)
	writeFile(t, root, "cmd/zero/main.go", "package main\n")
	writeFile(t, root, "internal/app/app.go", "package app\n")
	writeFile(t, root, "web/app.ts", "export const app = true\n")
	writeFile(t, root, "zero.exe", "ignored binary")
	writeFile(t, root, ".git/config", "[core]\n")
	writeFile(t, root, ".zero/state.json", "{}")
	writeFile(t, root, "node_modules/pkg/index.js", "ignored")
	writeFile(t, root, "vendor/lib/lib.go", "ignored")
	writeFile(t, root, "dist/bundle.js", "ignored")
	writeFile(t, root, "coverage/out.txt", "ignored")
	writeFile(t, root, ".next/cache.js", "ignored")
	writeFile(t, root, ".cache/blob", "ignored")

	got, err := Scan(root, Options{MaxDepth: DefaultMaxDepth})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	if got.Root != filepath.Clean(root) {
		t.Fatalf("Root=%q want %q", got.Root, filepath.Clean(root))
	}
	wantFiles := []string{
		"README.md",
		"cmd/zero/main.go",
		"go.mod",
		"internal/app/app.go",
		"package.json",
		"web/app.ts",
	}
	if !reflect.DeepEqual(pathsOf(got.Files), wantFiles) {
		t.Fatalf("Files=%v want %v", pathsOf(got.Files), wantFiles)
	}
	if got.FileCount != len(wantFiles) {
		t.Fatalf("FileCount=%d want %d", got.FileCount, len(wantFiles))
	}
	if got.DirectoryCount != 5 {
		t.Fatalf("DirectoryCount=%d want 5", got.DirectoryCount)
	}
	assertCounts(t, "LanguageCounts", got.LanguageCounts, []Count{
		{Name: "Go", Count: 2},
		{Name: "JSON", Count: 1},
		{Name: "Markdown", Count: 1},
		{Name: "TypeScript", Count: 1},
	})
	assertCounts(t, "ExtensionCounts", got.ExtensionCounts, []Count{
		{Name: ".go", Count: 2},
		{Name: ".json", Count: 1},
		{Name: ".md", Count: 1},
		{Name: ".mod", Count: 1},
		{Name: ".ts", Count: 1},
	})
	wantImportant := []string{"README.md", "go.mod", "package.json"}
	if !reflect.DeepEqual(got.ImportantFiles, wantImportant) {
		t.Fatalf("ImportantFiles=%v want %v", got.ImportantFiles, wantImportant)
	}
	wantTree := []string{
		".",
		"README.md",
		"cmd/",
		"  zero/",
		"    main.go",
		"go.mod",
		"internal/",
		"  app/",
		"    app.go",
		"package.json",
		"web/",
		"  app.ts",
	}
	if !reflect.DeepEqual(got.Tree, wantTree) {
		t.Fatalf("Tree=%v want %v", got.Tree, wantTree)
	}
}

func TestScanDoesNotFollowSymlinkedDirectories(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "main.go", "package main\n")
	// Not named "target": outside a git work tree Scan skips that name as Cargo build
	// output, and the real directory must be scanned for this test to exercise the link.
	target := filepath.Join(root, "real-dir")
	writeFile(t, target, "hidden.go", "package hidden\n")
	link := filepath.Join(root, "linked")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	got, err := Scan(root, Options{MaxDepth: DefaultMaxDepth})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	want := []string{"main.go", "real-dir/hidden.go"}
	if !reflect.DeepEqual(pathsOf(got.Files), want) {
		t.Fatalf("Files=%v want %v", pathsOf(got.Files), want)
	}
	if contains(pathsOf(got.Files), "linked/hidden.go") || contains(pathsOf(got.Files), "linked") {
		t.Fatalf("symlinked directory was included: %v", got.Files)
	}
}

func TestScanSkipsGitMetadataFile(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, ".git", "gitdir: ../.git/worktrees/zero\n")
	writeFile(t, root, "main.go", "package main\n")

	got, err := Scan(root, Options{MaxDepth: DefaultMaxDepth})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	want := []string{"main.go"}
	if !reflect.DeepEqual(pathsOf(got.Files), want) {
		t.Fatalf("Files=%v want %v", pathsOf(got.Files), want)
	}
}

func TestScanReturnsFilesystemErrorsWithTruncatedSnapshot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "missing")

	got, err := Scan(root, Options{MaxDepth: DefaultMaxDepth})
	if err == nil {
		t.Fatal("Scan missing root error=nil, want error")
	}
	if got.Root != filepath.Clean(root) {
		t.Fatalf("Root=%q want %q", got.Root, filepath.Clean(root))
	}
	if !got.Truncated {
		t.Fatal("Truncated=false want true")
	}
}

func TestHandleWalkErrorKeepsScanningAfterUnreadableSubdir(t *testing.T) {
	truncated := false
	handled, err := handleWalkError("root", "root/blocked", fakeDirEntry{dir: true}, os.ErrPermission, &truncated)
	if !handled {
		t.Fatal("handled=false want true")
	}
	if err != filepath.SkipDir {
		t.Fatalf("err=%v want filepath.SkipDir", err)
	}
	if !truncated {
		t.Fatal("truncated=false want true")
	}
}

func TestHandleWalkErrorReturnsRootErrors(t *testing.T) {
	truncated := false
	handled, err := handleWalkError("root", "root", nil, os.ErrNotExist, &truncated)
	if !handled {
		t.Fatal("handled=false want true")
	}
	if err == nil {
		t.Fatal("err=nil want root error")
	}
	if !truncated {
		t.Fatal("truncated=false want true")
	}
}

func TestScanHonorsTraversalCaps(t *testing.T) {
	t.Run("max files keeps the first deterministic files", func(t *testing.T) {
		root := t.TempDir()
		writeFile(t, root, "a.go", "package a\n")
		writeFile(t, root, "b.go", "package b\n")
		writeFile(t, root, "c.go", "package c\n")

		got, err := Scan(root, Options{MaxFiles: 2})
		if err != nil {
			t.Fatalf("Scan: %v", err)
		}
		want := []string{"a.go", "b.go"}
		if !reflect.DeepEqual(pathsOf(got.Files), want) {
			t.Fatalf("Files=%v want %v", pathsOf(got.Files), want)
		}
		if !got.Truncated {
			t.Fatal("Truncated=false want true")
		}
	})

	t.Run("max depth stops descent below the cap", func(t *testing.T) {
		root := t.TempDir()
		writeFile(t, root, "top.go", "package top\n")
		writeFile(t, root, "pkg/one.go", "package pkg\n")
		writeFile(t, root, "pkg/deep/two.go", "package deep\n")

		got, err := Scan(root, Options{MaxDepth: 1})
		if err != nil {
			t.Fatalf("Scan: %v", err)
		}
		want := []string{"pkg/one.go", "top.go"}
		if !reflect.DeepEqual(pathsOf(got.Files), want) {
			t.Fatalf("Files=%v want %v", pathsOf(got.Files), want)
		}
		if got.DirectoryCount != 1 {
			t.Fatalf("DirectoryCount=%d want 1", got.DirectoryCount)
		}
		if !got.Truncated {
			t.Fatal("Truncated=false want true")
		}
	})

	t.Run("zero max depth includes root files only", func(t *testing.T) {
		root := t.TempDir()
		writeFile(t, root, "top.go", "package top\n")
		writeFile(t, root, "pkg/one.go", "package pkg\n")

		got, err := Scan(root, Options{MaxDepth: 0})
		if err != nil {
			t.Fatalf("Scan: %v", err)
		}
		want := []string{"top.go"}
		if !reflect.DeepEqual(pathsOf(got.Files), want) {
			t.Fatalf("Files=%v want %v", pathsOf(got.Files), want)
		}
		if !got.Truncated {
			t.Fatal("Truncated=false want true")
		}
	})

	t.Run("max bytes per file name skips overlong entries", func(t *testing.T) {
		root := t.TempDir()
		writeFile(t, root, "short.go", "package short\n")
		writeFile(t, root, "longname.go", "package longname\n")

		got, err := Scan(root, Options{MaxBytesPerFileName: len("short.go")})
		if err != nil {
			t.Fatalf("Scan: %v", err)
		}
		want := []string{"short.go"}
		if !reflect.DeepEqual(pathsOf(got.Files), want) {
			t.Fatalf("Files=%v want %v", pathsOf(got.Files), want)
		}
		if !got.Truncated {
			t.Fatal("Truncated=false want true")
		}
	})
}

func writeFile(t *testing.T, root, rel, contents string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("WriteFile(%q): %v", path, err)
	}
}

func assertCounts(t *testing.T, name string, got, want []Count) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s=%v want %v", name, got, want)
	}
}

func pathsOf(files []File) []string {
	paths := make([]string, 0, len(files))
	for _, file := range files {
		paths = append(paths, file.Path)
	}
	return paths
}

func contains(values []string, needle string) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
}

type fakeDirEntry struct {
	dir bool
}

func (entry fakeDirEntry) Name() string {
	return "fake"
}

func (entry fakeDirEntry) IsDir() bool {
	return entry.dir
}

func (entry fakeDirEntry) Type() fs.FileMode {
	if entry.dir {
		return fs.ModeDir
	}
	return 0
}

func (entry fakeDirEntry) Info() (fs.FileInfo, error) {
	return nil, os.ErrInvalid
}
