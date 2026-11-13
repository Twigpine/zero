package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// goosGoarchTokens are the filename suffixes Go treats as an implicit build
// constraint. A file named foo_arm_test.go is compiled ONLY on GOARCH=arm, with
// no error and no warning anywhere.
//
// Not exhaustive by design: this lists the tokens plausible as the tail of an
// English identifier, which is exactly when the mistake gets made. "arm" is the
// one that bit us; "js", "wasm" and "plan9" are the same shape.
var goosGoarchTokens = map[string]bool{
	"aix": true, "android": true, "darwin": true, "dragonfly": true,
	"freebsd": true, "hurd": true, "illumos": true, "ios": true, "js": true,
	"linux": true, "nacl": true, "netbsd": true, "openbsd": true, "plan9": true,
	"solaris": true, "wasip1": true, "windows": true, "zos": true,

	"386": true, "amd64": true, "arm": true, "arm64": true, "loong64": true,
	"mips": true, "mips64": true, "mips64le": true, "mipsle": true,
	"ppc64": true, "ppc64le": true, "riscv": true, "riscv64": true,
	"s390x": true, "sparc64": true, "wasm": true,
}

// A test file whose name ends in a GOOS/GOARCH token is silently excluded from
// every build on every other platform. It does not fail, it does not warn, and
// CI stays green because the file may as well not exist.
//
// This happened here: permission_mode_arm_test.go was read as GOARCH=arm and
// never ran, so the shift+tab full-auto offer gate had zero coverage while
// appearing to have five tests. Two of them panicked the moment they were made
// to run.
//
// A file that genuinely is platform-specific carries an explicit //go:build
// line, so requiring that tag is what separates intent from accident.
func TestNoTestFileIsHiddenByItsFilename(t *testing.T) {
	root := repoRootForFilenameCheck(t)

	var hidden []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", "node_modules", "vendor", "testdata", "dist", "bin":
				return filepath.SkipDir
			}
			return nil
		}
		name := entry.Name()
		if !strings.HasSuffix(name, "_test.go") {
			return nil
		}
		base := strings.TrimSuffix(name, "_test.go")
		parts := strings.Split(base, "_")
		if len(parts) < 2 {
			return nil
		}
		if !goosGoarchTokens[parts[len(parts)-1]] {
			return nil
		}
		tagged, readErr := hasExplicitBuildTag(path)
		if readErr != nil {
			return readErr
		}
		if tagged {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			rel = path
		}
		hidden = append(hidden, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatalf("walking the repository: %v", err)
	}

	if len(hidden) > 0 {
		t.Fatalf("these test files end in a GOOS/GOARCH token, so Go excludes them from every other platform and their tests never run:\n  %s\n"+
			"Rename the file, or add an explicit //go:build line if the constraint is intended.",
			strings.Join(hidden, "\n  "))
	}
}

// hasExplicitBuildTag reports whether the file declares a //go:build line, which
// is how a deliberately platform-scoped file states that it means it.
func hasExplicitBuildTag(path string) (bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "//go:build") {
			return true, nil
		}
		// Constraints must precede the package clause, so there is nothing left
		// to find once it appears.
		if strings.HasPrefix(trimmed, "package ") {
			return false, nil
		}
	}
	return false, nil
}

// repoRootForFilenameCheck walks up from this package to the module root, so the
// check covers the whole repository rather than one package.
func repoRootForFilenameCheck(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("working directory: %v", err)
	}
	for {
		if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod found above the test package, cannot locate the repository root")
		}
		dir = parent
	}
}
