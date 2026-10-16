package update

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func writeTestTarGz(t *testing.T, archivePath string, entries map[string]string) {
	t.Helper()
	file, err := os.Create(archivePath)
	if err != nil {
		t.Fatalf("Create archive: %v", err)
	}
	defer func() { _ = file.Close() }()
	gzipWriter := gzip.NewWriter(file)
	tarWriter := tar.NewWriter(gzipWriter)
	for name, content := range entries {
		header := &tar.Header{Name: name, Mode: 0o755, Size: int64(len(content))}
		if err := tarWriter.WriteHeader(header); err != nil {
			t.Fatalf("WriteHeader %s: %v", name, err)
		}
		if _, err := tarWriter.Write([]byte(content)); err != nil {
			t.Fatalf("Write %s: %v", name, err)
		}
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatalf("close tar writer: %v", err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatalf("close gzip writer: %v", err)
	}
}

func writeTestZip(t *testing.T, archivePath string, entries map[string]string) {
	t.Helper()
	file, err := os.Create(archivePath)
	if err != nil {
		t.Fatalf("Create archive: %v", err)
	}
	defer func() { _ = file.Close() }()
	zipWriter := zip.NewWriter(file)
	for name, content := range entries {
		writer, err := zipWriter.Create(name)
		if err != nil {
			t.Fatalf("Create entry %s: %v", name, err)
		}
		if _, err := writer.Write([]byte(content)); err != nil {
			t.Fatalf("Write %s: %v", name, err)
		}
	}
	if err := zipWriter.Close(); err != nil {
		t.Fatalf("close zip writer: %v", err)
	}
}

func TestExtractTarGzRoundTrip(t *testing.T) {
	dir := t.TempDir()
	archivePath := filepath.Join(dir, "archive.tar.gz")
	writeTestTarGz(t, archivePath, map[string]string{
		"zero":                 "main-binary",
		"helpers/zero-seccomp": "helper-binary",
	})

	destDir := filepath.Join(dir, "extracted")
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := extractArchive(archivePath, destDir); err != nil {
		t.Fatalf("extractArchive: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(destDir, "zero"))
	if err != nil {
		t.Fatalf("ReadFile zero: %v", err)
	}
	if string(data) != "main-binary" {
		t.Fatalf("zero content = %q", data)
	}
	data, err = os.ReadFile(filepath.Join(destDir, "helpers", "zero-seccomp"))
	if err != nil {
		t.Fatalf("ReadFile helpers/zero-seccomp: %v", err)
	}
	if string(data) != "helper-binary" {
		t.Fatalf("helpers/zero-seccomp content = %q", data)
	}
}

func TestExtractZipRoundTrip(t *testing.T) {
	dir := t.TempDir()
	archivePath := filepath.Join(dir, "archive.zip")
	writeTestZip(t, archivePath, map[string]string{
		"zero.exe": "main-binary",
	})

	destDir := filepath.Join(dir, "extracted")
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := extractArchive(archivePath, destDir); err != nil {
		t.Fatalf("extractArchive: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(destDir, "zero.exe"))
	if err != nil {
		t.Fatalf("ReadFile zero.exe: %v", err)
	}
	if string(data) != "main-binary" {
		t.Fatalf("zero.exe content = %q", data)
	}
}

func TestExtractTarGzRejectsPathTraversal(t *testing.T) {
	dir := t.TempDir()
	archivePath := filepath.Join(dir, "archive.tar.gz")
	writeTestTarGz(t, archivePath, map[string]string{
		"../escape": "malicious",
	})

	destDir := filepath.Join(dir, "extracted")
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := extractArchive(archivePath, destDir); err == nil {
		t.Fatal("expected extractArchive to reject a path-traversal entry")
	}
	if _, err := os.Stat(filepath.Join(dir, "escape")); err == nil {
		t.Fatal("path traversal entry was written outside the destination directory")
	}
}

func TestExtractZipRejectsPathTraversal(t *testing.T) {
	dir := t.TempDir()
	archivePath := filepath.Join(dir, "archive.zip")
	writeTestZip(t, archivePath, map[string]string{
		"../../escape.exe": "malicious",
	})

	destDir := filepath.Join(dir, "extracted")
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := extractArchive(archivePath, destDir); err == nil {
		t.Fatal("expected extractArchive to reject a path-traversal entry")
	}
}

// A zip entry with the Unix symlink mode bit set must be rejected, not
// silently written out as an ordinary file containing the link-target
// string — mirroring extractTarGz's rejection of non-regular tar entries.
func TestExtractZipRejectsSymlinkEntry(t *testing.T) {
	dir := t.TempDir()
	archivePath := filepath.Join(dir, "archive.zip")

	file, err := os.Create(archivePath)
	if err != nil {
		t.Fatalf("Create archive: %v", err)
	}
	zipWriter := zip.NewWriter(file)
	header := &zip.FileHeader{Name: "zero"}
	header.SetMode(os.ModeSymlink | 0o777)
	writer, err := zipWriter.CreateHeader(header)
	if err != nil {
		t.Fatalf("CreateHeader: %v", err)
	}
	if _, err := writer.Write([]byte("/some/other/path")); err != nil {
		t.Fatalf("Write symlink target: %v", err)
	}
	if err := zipWriter.Close(); err != nil {
		t.Fatalf("close zip writer: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close archive file: %v", err)
	}

	destDir := filepath.Join(dir, "extracted")
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := extractArchive(archivePath, destDir); err == nil {
		t.Fatal("expected extractArchive to reject a symlink entry")
	}
	if _, err := os.Lstat(filepath.Join(destDir, "zero")); err == nil {
		t.Fatal("symlink entry should not have been written to the destination")
	}
}

func TestFindByBasenameSearchesRecursively(t *testing.T) {
	dir := t.TempDir()
	nested := filepath.Join(dir, "helpers")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	wantPath := filepath.Join(nested, "zero-seccomp")
	if err := os.WriteFile(wantPath, []byte("helper"), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	found, err := findByBasename(dir, "zero-seccomp")
	if err != nil {
		t.Fatalf("findByBasename: %v", err)
	}
	if found != wantPath {
		t.Fatalf("findByBasename = %q, want %q", found, wantPath)
	}

	notFound, err := findByBasename(dir, "does-not-exist")
	if err != nil {
		t.Fatalf("findByBasename: %v", err)
	}
	if notFound != "" {
		t.Fatalf("findByBasename = %q, want empty", notFound)
	}
}

func symlinksSupported(t *testing.T) bool {
	dir := t.TempDir()
	err := os.Symlink("target", filepath.Join(dir, "link"))
	return err == nil
}

func TestExtractTarGzAllowsSafeSymlink(t *testing.T) {
	if !symlinksSupported(t) {
		t.Skip("symlinks not supported")
	}
	dir := t.TempDir()
	archivePath := filepath.Join(dir, "archive.tar.gz")

	file, err := os.Create(archivePath)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	gw := gzip.NewWriter(file)
	tw := tar.NewWriter(gw)

	h1 := &tar.Header{
		Name:     "link.txt",
		Typeflag: tar.TypeSymlink,
		Linkname: "target.txt",
	}
	if err := tw.WriteHeader(h1); err != nil {
		t.Fatalf("WriteHeader link: %v", err)
	}
	h2 := &tar.Header{
		Name:     "target.txt",
		Typeflag: tar.TypeReg,
		Mode:     0o644,
		Size:     12,
	}
	if err := tw.WriteHeader(h2); err != nil {
		t.Fatalf("WriteHeader target: %v", err)
	}
	if _, err := tw.Write([]byte("hello symbol")); err != nil {
		t.Fatalf("Write target: %v", err)
	}

	// Cover a linked subdirectory with a file extracted through the link.
	// Windows Root.Symlink needs the directory target to exist first so the
	// link is created as a directory junction rather than a file symlink.
	h3 := &tar.Header{
		Name:     "subdir",
		Typeflag: tar.TypeDir,
		Mode:     0o755,
	}
	if err := tw.WriteHeader(h3); err != nil {
		t.Fatalf("WriteHeader subdir: %v", err)
	}
	h4 := &tar.Header{
		Name:     "sublink",
		Typeflag: tar.TypeSymlink,
		Linkname: "subdir",
	}
	if err := tw.WriteHeader(h4); err != nil {
		t.Fatalf("WriteHeader sublink: %v", err)
	}
	h5 := &tar.Header{
		Name:     "sublink/nested.txt",
		Typeflag: tar.TypeReg,
		Mode:     0o644,
		Size:     6,
	}
	if err := tw.WriteHeader(h5); err != nil {
		t.Fatalf("WriteHeader nested: %v", err)
	}
	if _, err := tw.Write([]byte("nested")); err != nil {
		t.Fatalf("Write nested: %v", err)
	}

	tw.Close()
	gw.Close()
	file.Close()

	destDir := filepath.Join(dir, "extracted")
	if err := extractArchive(archivePath, destDir); err != nil {
		t.Fatalf("extractArchive: %v", err)
	}

	targetPath := filepath.Join(destDir, "link.txt")
	gotLink, err := os.Readlink(targetPath)
	if err != nil {
		t.Fatalf("Readlink: %v", err)
	}
	if gotLink != "target.txt" {
		t.Fatalf("link target = %q, want %q", gotLink, "target.txt")
	}

	nestedData, err := os.ReadFile(filepath.Join(destDir, "subdir", "nested.txt"))
	if err != nil {
		t.Fatalf("ReadFile nested: %v", err)
	}
	if string(nestedData) != "nested" {
		t.Fatalf("nestedData = %q, want %q", string(nestedData), "nested")
	}
}

func TestExtractTarGzRejectsEscapingSymlink(t *testing.T) {
	if !symlinksSupported(t) {
		t.Skip("symlinks not supported")
	}
	dir := t.TempDir()
	archivePath := filepath.Join(dir, "archive.tar.gz")

	file, err := os.Create(archivePath)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	gw := gzip.NewWriter(file)
	tw := tar.NewWriter(gw)

	h1 := &tar.Header{
		Name:     "link.txt",
		Typeflag: tar.TypeSymlink,
		Linkname: "../../../outside.txt",
	}
	if err := tw.WriteHeader(h1); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}

	tw.Close()
	gw.Close()
	file.Close()

	destDir := filepath.Join(dir, "extracted")
	if err := extractArchive(archivePath, destDir); err == nil {
		t.Fatal("expected error extracting escaping symlink")
	}
}

func TestExtractTarGzRejectsChainedSymlinkEscapingFile(t *testing.T) {
	if !symlinksSupported(t) {
		t.Skip("symlinks not supported")
	}
	dir := t.TempDir()
	destDir := filepath.Join(dir, "extracted")
	outsideDir := filepath.Join(dir, "outside")
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		t.Fatalf("Mkdir dest: %v", err)
	}
	if err := os.MkdirAll(outsideDir, 0o755); err != nil {
		t.Fatalf("Mkdir outside: %v", err)
	}
	// chain -> mid -> outsideDir. Lexical path destDir/chain/pwned.txt stays
	// under destDir; EvalSymlinks of the chain does not.
	if err := os.Symlink(outsideDir, filepath.Join(destDir, "mid")); err != nil {
		t.Fatalf("symlink mid: %v", err)
	}
	if err := os.Symlink("mid", filepath.Join(destDir, "chain")); err != nil {
		t.Fatalf("symlink chain: %v", err)
	}

	archivePath := filepath.Join(dir, "archive.tar.gz")
	writeTestTarGz(t, archivePath, map[string]string{
		"chain/pwned.txt": "escaped",
	})

	if err := extractArchive(archivePath, destDir); err == nil {
		t.Fatal("expected error extracting a file through a chained escaping symlink")
	}
	if _, err := os.Stat(filepath.Join(outsideDir, "pwned.txt")); !os.IsNotExist(err) {
		t.Fatalf("escaped file exists outside destDir: %v", err)
	}
}

type testTarEntry struct {
	name     string
	typeflag byte
	linkname string
	body     string
}

func writeTestTarGzEntries(t *testing.T, archivePath string, entries []testTarEntry) {
	t.Helper()
	file, err := os.Create(archivePath)
	if err != nil {
		t.Fatalf("Create archive: %v", err)
	}
	defer func() { _ = file.Close() }()
	gzipWriter := gzip.NewWriter(file)
	tarWriter := tar.NewWriter(gzipWriter)
	for _, entry := range entries {
		header := &tar.Header{
			Name:     entry.name,
			Typeflag: entry.typeflag,
			Linkname: entry.linkname,
			Mode:     0o644,
			Size:     int64(len(entry.body)),
		}
		if entry.typeflag == tar.TypeSymlink || entry.typeflag == tar.TypeDir {
			header.Size = 0
		}
		if err := tarWriter.WriteHeader(header); err != nil {
			t.Fatalf("WriteHeader %s: %v", entry.name, err)
		}
		if header.Size > 0 {
			if _, err := tarWriter.Write([]byte(entry.body)); err != nil {
				t.Fatalf("Write %s: %v", entry.name, err)
			}
		}
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatalf("close tar writer: %v", err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatalf("close gzip writer: %v", err)
	}
}

// A tar can plant d -> ., then d/s -> .. (physically destDir/s -> .. because
// d is already a link), then l -> d/s/missing, then a regular file named l.
// EvalSymlinks(l) is ENOENT; lexical Join would accept destDir/d/s/missing
// while open follows d and s and writes missing beside destDir.
func TestExtractTarGzRejectsDanglingSymlinkChainEscape(t *testing.T) {
	if !symlinksSupported(t) {
		t.Skip("symlinks not supported")
	}
	dir := t.TempDir()
	destDir := filepath.Join(dir, "extracted")
	archivePath := filepath.Join(dir, "archive.tar.gz")
	writeTestTarGzEntries(t, archivePath, []testTarEntry{
		{name: "d", typeflag: tar.TypeSymlink, linkname: "."},
		{name: "d/s", typeflag: tar.TypeSymlink, linkname: ".."},
		{name: "l", typeflag: tar.TypeSymlink, linkname: "d/s/missing"},
		{name: "l", typeflag: tar.TypeReg, body: "pwned"},
	})
	extractErr := extractArchive(archivePath, destDir)
	escaped := filepath.Join(dir, "missing")
	_, statErr := os.Stat(escaped)
	if !os.IsNotExist(statErr) {
		t.Fatalf("escaped file exists outside destDir: %v", statErr)
	}
	if extractErr == nil {
		t.Fatal("expected extractArchive to reject a dangling symlink chain that escapes destDir")
	}
}

func TestExtractTarGzAllowsSafeDanglingRelative(t *testing.T) {
	if !symlinksSupported(t) {
		t.Skip("symlinks not supported")
	}
	dir := t.TempDir()
	destDir := filepath.Join(dir, "extracted")
	archivePath := filepath.Join(dir, "archive.tar.gz")
	writeTestTarGzEntries(t, archivePath, []testTarEntry{
		{name: "l", typeflag: tar.TypeSymlink, linkname: "not-yet-there"},
		{name: "l", typeflag: tar.TypeReg, body: "safe-content"},
	})
	if err := extractArchive(archivePath, destDir); err != nil {
		t.Fatalf("extractArchive: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(destDir, "not-yet-there"))
	if err != nil {
		t.Fatalf("ReadFile not-yet-there: %v", err)
	}
	if string(data) != "safe-content" {
		t.Fatalf("not-yet-there content = %q", data)
	}
	if _, err := os.Stat(filepath.Join(dir, "not-yet-there")); err == nil {
		t.Fatal("safe dangling target was written outside destDir")
	}
}

// Tar entry d -> . followed by d/s -> .. creates destDir/s -> .. if the
// symlink entry's link-target check uses lexical filepath.Dir instead of the
// physical parent that will create it. The symlink entry itself must be
// rejected before an outbound link can be planted.
func TestExtractTarGzRejectsSymlinkParentEscape(t *testing.T) {
	if !symlinksSupported(t) {
		t.Skip("symlinks not supported")
	}
	dir := t.TempDir()
	destDir := filepath.Join(dir, "extracted")
	archivePath := filepath.Join(dir, "archive.tar.gz")
	writeTestTarGzEntries(t, archivePath, []testTarEntry{
		{name: "d", typeflag: tar.TypeSymlink, linkname: "."},
		{name: "d/s", typeflag: tar.TypeSymlink, linkname: ".."},
	})
	extractErr := extractArchive(archivePath, destDir)
	escaped := filepath.Join(destDir, "s")
	if _, err := os.Lstat(escaped); err == nil {
		t.Fatalf("outbound symlink %s was created", escaped)
	}
	if extractErr == nil {
		t.Fatal("expected extractArchive to reject symlink entry with escaping target through symlink parent")
	}
}

// An archive attempting to plant zero -> d/s/outside-file through a symlink parent
// must not create the target outside destDir and extractArchive must fail.
func TestExtractTarGzRejectsSymlinkThroughSymlinkParentOutsideTarget(t *testing.T) {
	if !symlinksSupported(t) {
		t.Skip("symlinks not supported")
	}
	dir := t.TempDir()
	destDir := filepath.Join(dir, "extracted")
	outsideDir := filepath.Join(dir, "outside")
	if err := os.MkdirAll(outsideDir, 0o755); err != nil {
		t.Fatalf("MkdirAll outside: %v", err)
	}
	outsideFile := filepath.Join(outsideDir, "pwned.txt")
	archivePath := filepath.Join(dir, "archive.tar.gz")
	writeTestTarGzEntries(t, archivePath, []testTarEntry{
		{name: "d", typeflag: tar.TypeSymlink, linkname: "."},
		{name: "d/s", typeflag: tar.TypeSymlink, linkname: ".."},
		{name: "zero", typeflag: tar.TypeSymlink, linkname: "d/s/outside/pwned.txt"},
	})
	extractErr := extractArchive(archivePath, destDir)
	if extractErr == nil {
		t.Fatal("expected extractArchive to fail on escaping symlink chain")
	}
	if _, err := os.Stat(outsideFile); !os.IsNotExist(err) {
		t.Fatalf("file outside destDir was accessed/created: %v", err)
	}
}

func TestExtractTarGzRejectsIntermediateDirSymlinkEscape(t *testing.T) {
	if !symlinksSupported(t) {
		t.Skip("symlinks not supported")
	}
	dir := t.TempDir()
	destDir := filepath.Join(dir, "extracted")
	outside := filepath.Join(dir, "archive.tar.gz.outside")
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	archivePath := filepath.Join(dir, "archive.tar.gz")
	writeTestTarGzEntries(t, archivePath, []testTarEntry{
		{name: "d", typeflag: tar.TypeSymlink, linkname: "."},
		{name: "d/a", typeflag: tar.TypeDir},
		{name: "d/a/zero", typeflag: tar.TypeSymlink, linkname: "../../archive.tar.gz.outside"},
	})
	if err := extractArchive(archivePath, destDir); err == nil {
		t.Fatal("expected reject of ../../ escape through intermediate dir symlink")
	}
	if _, err := os.Lstat(filepath.Join(destDir, "d", "a", "zero")); err == nil {
		t.Fatal("escaping link must not be created")
	}
}

func TestFollowUnderRootRejectsNinthLink(t *testing.T) {
	if !symlinksSupported(t) {
		t.Skip("symlinks not supported")
	}
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := os.Mkdir(filepath.Join(dir, "leaf"), 0o755); err != nil {
		t.Fatal(err)
	}
	prev := "leaf"
	for i := 0; i < maxRootSymlinks; i++ {
		name := fmt.Sprintf("l%d", i)
		if err := os.Symlink(prev, filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
		prev = name
	}
	if _, err := followUnderRoot(root, prev, 0); err != nil {
		t.Fatalf("eight links must resolve: %v", err)
	}
	ninth := "l8"
	if err := os.Symlink(prev, filepath.Join(dir, ninth)); err != nil {
		t.Fatal(err)
	}
	if _, err := followUnderRoot(root, ninth, 0); err == nil {
		t.Fatal("ninth link must be rejected")
	}
}
