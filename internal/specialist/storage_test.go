package specialist

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Twigpine/zero/internal/fsutil"
)

func TestStorageCreateWritesValidManifestAndDeleteRemovesIt(t *testing.T) {
	userDir := filepath.Join(t.TempDir(), "user")
	storage := NewStorage(Paths{UserDir: userDir})

	manifest, err := storage.Create(CreateInput{
		Name:         "triage",
		Description:  "Triage failures",
		SystemPrompt: "Find the likely failure area.",
		Tools:        []string{"read-only", "plan"},
	})
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	if manifest.FilePath != filepath.Join(userDir, "triage.md") || manifest.Location != LocationUser {
		t.Fatalf("unexpected manifest path/location: %#v", manifest)
	}
	data, err := os.ReadFile(manifest.FilePath)
	if err != nil {
		t.Fatalf("read created manifest: %v", err)
	}
	loaded, err := ParseMarkdown(string(data))
	if err != nil {
		t.Fatalf("created manifest did not parse: %v\n%s", err, string(data))
	}
	if loaded.Metadata.Name != "triage" || !contains(loaded.ResolvedTools, "update_plan") {
		t.Fatalf("unexpected loaded manifest: %#v", loaded)
	}

	deleted, err := storage.Delete(DeleteInput{Name: "triage"})
	if err != nil {
		t.Fatalf("Delete returned error: %v", err)
	}
	if deleted != manifest.FilePath {
		t.Fatalf("deleted path = %q, want %q", deleted, manifest.FilePath)
	}
	if _, err := os.Stat(manifest.FilePath); !os.IsNotExist(err) {
		t.Fatalf("expected file deleted, stat err=%v", err)
	}
}

func TestStorageRejectsUnsafeNamesAndDuplicates(t *testing.T) {
	storage := NewStorage(Paths{UserDir: t.TempDir()})
	if _, err := storage.Create(CreateInput{Name: "../escape", Description: "Escape"}); err == nil || !strings.Contains(err.Error(), "invalid specialist name") {
		t.Fatalf("unsafe Create error = %v", err)
	}
	if _, err := storage.Create(CreateInput{Name: "safe", Description: "Safe"}); err != nil {
		t.Fatalf("Create safe returned error: %v", err)
	}
	if _, err := storage.Create(CreateInput{Name: "safe", Description: "Safe"}); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("duplicate Create error = %v", err)
	}
}

func TestStorageCreateForceRejectsSymlink(t *testing.T) {
	root := t.TempDir()
	userDir := filepath.Join(root, "user")
	if err := os.MkdirAll(userDir, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "target.md")
	if err := os.WriteFile(target, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(userDir, "safe.md")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	storage := NewStorage(Paths{UserDir: userDir})

	_, err := storage.Create(CreateInput{Name: "safe", Description: "Safe", Overwrite: true})

	if err == nil || !strings.Contains(err.Error(), "refusing to overwrite symlink") {
		t.Fatalf("symlink overwrite error = %v", err)
	}
	data, readErr := os.ReadFile(target)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(data) != "outside" {
		t.Fatalf("symlink target was modified: %q", string(data))
	}
	assertNoTemporarySpecialistFiles(t, userDir)
}

func TestStorageCreateForceReplacesFileAndPreservesMode(t *testing.T) {
	userDir := t.TempDir()
	path := filepath.Join(userDir, "safe.md")
	if err := os.WriteFile(path, []byte("old content"), 0o644); err != nil {
		t.Fatal(err)
	}
	storage := NewStorage(Paths{UserDir: userDir})

	manifest, err := storage.Create(CreateInput{
		Name:         "safe",
		Description:  "Safe",
		SystemPrompt: "new content",
		Overwrite:    true,
	})
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(data), FormatMarkdown(manifest); got != want {
		t.Fatalf("file content = %q, want %q", got, want)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); runtime.GOOS != "windows" && got != 0o644 {
		t.Fatalf("file permissions = %o, want the existing mode 644", got)
	}
	assertNoTemporarySpecialistFiles(t, userDir)
}

func TestStorageCreateForceSerializesConcurrentLoad(t *testing.T) {
	userDir := t.TempDir()
	storage := NewStorage(Paths{UserDir: userDir})
	if _, err := storage.Create(CreateInput{Name: "safe", Description: "old"}); err != nil {
		t.Fatal(err)
	}

	gapOpen := make(chan struct{})
	finishReplacement := make(chan struct{})
	storage.writeReplacement = func(path, content string) error {
		backup := path + ".test-backup"
		if err := os.Rename(path, backup); err != nil {
			return err
		}
		close(gapOpen)
		<-finishReplacement
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			return err
		}
		return os.Remove(backup)
	}

	writeDone := make(chan error, 1)
	go func() {
		_, err := storage.Create(CreateInput{Name: "safe", Description: "new", Overwrite: true})
		writeDone <- err
	}()
	<-gapOpen
	defer func() {
		select {
		case <-finishReplacement:
		default:
			close(finishReplacement)
		}
	}()

	loadDone := make(chan LoadResult, 1)
	loadErr := make(chan error, 1)
	loadStarted := make(chan struct{})
	go func() {
		close(loadStarted)
		result, err := Load(LoadOptions{Paths: Paths{UserDir: userDir}})
		if err != nil {
			loadErr <- err
			return
		}
		loadDone <- result
	}()
	<-loadStarted
	select {
	case err := <-loadErr:
		t.Fatalf("Load returned during replacement: %v", err)
	case result := <-loadDone:
		t.Fatalf("Load returned during replacement gap: %#v", result.Specialists)
	case <-time.After(25 * time.Millisecond):
	}

	close(finishReplacement)
	if err := <-writeDone; err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	select {
	case err := <-loadErr:
		t.Fatal(err)
	case result := <-loadDone:
		manifest, ok := Find(result, "safe")
		if !ok || manifest.Metadata.Description != "new" {
			t.Fatalf("loaded specialist = %#v, want replacement", manifest)
		}
	case <-time.After(time.Second):
		t.Fatal("Load did not resume after replacement")
	}
}

func TestStorageCreateReturnsManifestWarningAfterCommittedCleanupFailure(t *testing.T) {
	userDir := t.TempDir()
	backupPath := filepath.Join(userDir, ".zero-replace-old.backup")
	cleanupErr := &os.PathError{Op: "remove", Path: backupPath, Err: syscall.Errno(32)}
	storage := NewStorage(Paths{UserDir: userDir})
	storage.writeReplacement = func(path, content string) error {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			return err
		}
		return &fsutil.CommittedReplacementCleanupError{BackupPath: backupPath, Cause: cleanupErr}
	}

	manifest, err := storage.Create(CreateInput{Name: "safe", Description: "Safe", Overwrite: true})
	if err != nil {
		t.Fatalf("Create returned error after committed replacement: %v", err)
	}
	if len(manifest.Warnings) != 1 || !strings.Contains(manifest.Warnings[0], backupPath) || !strings.Contains(manifest.Warnings[0], cleanupErr.Error()) {
		t.Fatalf("warnings = %#v, want backup path and cleanup problem", manifest.Warnings)
	}
	if data, readErr := os.ReadFile(manifest.FilePath); readErr != nil || string(data) != FormatMarkdown(manifest) {
		t.Fatalf("committed specialist content = %q, error = %v", data, readErr)
	}
}

func TestWriteSpecialistReplacementRetriesTransientWindowsRename(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("rename retries are Windows-specific")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "safe.md")
	if err := os.WriteFile(path, []byte("old content"), 0o600); err != nil {
		t.Fatal(err)
	}
	attempts := 0
	err := writeSpecialistReplacementWith(path, "new content", func(src, dst string) error {
		attempts++
		if attempts == 1 {
			return syscall.Errno(32) // ERROR_SHARING_VIOLATION
		}
		return os.Rename(src, dst)
	}, func(string) error { return nil })
	if err != nil {
		t.Fatalf("writeSpecialistReplacementWith returned error: %v", err)
	}
	if attempts != 2 {
		t.Fatalf("rename attempts = %d, want 2", attempts)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(data); got != "new content" {
		t.Fatalf("file content = %q, want %q", got, "new content")
	}
	assertNoTemporarySpecialistFiles(t, dir)
}

func TestWriteSpecialistReplacementIgnoresDirectorySyncErrorAfterCommit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "safe.md")
	syncErr := errors.New("sync failed")
	called := false
	err := writeSpecialistReplacementWith(path, "new content", nil, func(got string) error {
		called = true
		if got != dir {
			t.Fatalf("sync directory = %q, want %q", got, dir)
		}
		return syncErr
	})
	if err != nil {
		t.Fatalf("committed replacement returned sync error: %v", err)
	}
	if !called {
		t.Fatal("directory sync was not attempted")
	}
	if data, readErr := os.ReadFile(path); readErr != nil || string(data) != "new content" {
		t.Fatalf("committed content = %q, error = %v", data, readErr)
	}
	assertNoTemporarySpecialistFiles(t, dir)
}

func TestSyncSpecialistDirIgnoresOpenFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory sync is not attempted on Windows")
	}
	missing := filepath.Join(t.TempDir(), "missing")
	if err := syncSpecialistDir(missing); err != nil {
		t.Fatalf("directory open failure should be best-effort, got: %v", err)
	}
}

// TestWriteSpecialistReplacementKeepsTheOriginalAfterFailedReplace pins the caller
// side of Windows partial-failure rollback: fsutil restores the original at dst,
// reports the ReplaceFileW error, and leaves src for this function's deferred
// temporary-file cleanup.
func TestWriteSpecialistReplacementKeepsTheOriginalAfterFailedReplace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "safe.md")
	if err := os.WriteFile(path, []byte("old content"), 0o600); err != nil {
		t.Fatal(err)
	}
	replaceErr := errors.New("ReplaceFileW could not rename the replacement")
	err := writeSpecialistReplacementWith(path, "new content", func(_, _ string) error {
		return replaceErr
	}, func(string) error { return nil })

	if !errors.Is(err, replaceErr) {
		t.Fatalf("writeSpecialistReplacementWith error = %v, want it to report %v", err, replaceErr)
	}
	data, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("the original destination was lost after failed replacement: %v", readErr)
	}
	if got := string(data); got != "old content" {
		t.Fatalf("file content = %q, want the original bytes", got)
	}
	assertNoTemporarySpecialistFiles(t, dir)
}

func assertNoTemporarySpecialistFiles(t *testing.T, dir string) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, ".specialist-*.tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("temporary specialist files remain: %v", matches)
	}
}
