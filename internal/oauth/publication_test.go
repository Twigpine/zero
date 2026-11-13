package oauth

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFileBlobWriteSyncsFileAndDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oauth-tokens.json")
	blob := fileBlob{path: path}
	origFile, origDir := syncOAuthFileFn, syncOAuthDirFn
	t.Cleanup(func() {
		syncOAuthFileFn, syncOAuthDirFn = origFile, origDir
	})
	fileSynced, dirSynced := false, false
	syncOAuthFileFn = func(file *os.File) error {
		fileSynced = true
		return origFile(file)
	}
	syncOAuthDirFn = func(dir string) error {
		dirSynced = true
		return origDir(dir)
	}

	if err := blob.write([]byte("payload")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if !fileSynced {
		t.Fatal("token blob was not synced before rename")
	}
	if !dirSynced {
		t.Fatal("token store directory was not synced after rename")
	}
	data, ok, err := blob.read()
	if err != nil || !ok || !bytes.Equal(data, []byte("payload")) {
		t.Fatalf("read = %q ok=%v err=%v", data, ok, err)
	}
}

// TestFileBlobWriteDirectorySyncFailureLeavesPublishedBlob pins the post-rename
// path: the directory barrier runs after the atomic replace, so a barrier
// failure reports an error but must not hide the already-published token.
func TestFileBlobWriteDirectorySyncFailureLeavesPublishedBlob(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oauth-tokens.json")
	blob := fileBlob{path: path}
	if err := blob.write([]byte("old")); err != nil {
		t.Fatalf("seed write: %v", err)
	}

	origDir := syncOAuthDirFn
	t.Cleanup(func() { syncOAuthDirFn = origDir })
	syncErr := errors.New("directory sync failed")
	syncOAuthDirFn = func(string) error { return syncErr }

	err := blob.write([]byte("new"))
	if !errors.Is(err, syncErr) {
		t.Fatalf("write error = %v, want it to wrap %v", err, syncErr)
	}
	data, ok, err := blob.read()
	if err != nil || !ok || !bytes.Equal(data, []byte("new")) {
		t.Fatalf("published blob was not retained: data=%q ok=%v err=%v", data, ok, err)
	}
}

func TestFileBlobWriteSyncFailurePreservesPrevious(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oauth-tokens.json")
	blob := fileBlob{path: path}
	if err := blob.write([]byte("old")); err != nil {
		t.Fatalf("seed write: %v", err)
	}

	origFile := syncOAuthFileFn
	t.Cleanup(func() { syncOAuthFileFn = origFile })
	syncErr := errors.New("sync failed")
	syncOAuthFileFn = func(*os.File) error { return syncErr }

	err := blob.write([]byte("new"))
	if !errors.Is(err, syncErr) {
		t.Fatalf("write error = %v, want it to wrap %v", err, syncErr)
	}
	data, ok, err := blob.read()
	if err != nil || !ok || !bytes.Equal(data, []byte("old")) {
		t.Fatalf("previous blob was not preserved: data=%q ok=%v err=%v", data, ok, err)
	}
	entries, err := os.ReadDir(PublicationDir(path))
	if err != nil {
		t.Fatalf("ReadDir publication dir: %v", err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "publish-") {
			t.Fatalf("failed write left publication temp %s", entry.Name())
		}
	}
}
