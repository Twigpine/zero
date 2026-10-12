package daemon

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Gitlawb/zero/internal/fsutil"
	"github.com/Gitlawb/zero/internal/privatedir"
)

func TestWriteStatusFilePreservesPreviousDocumentWhenReplaceFails(t *testing.T) {
	dir := t.TempDir()
	secureStatusTestDir(t, dir)
	path := filepath.Join(dir, "daemon.status")
	previous := []byte(`{"pid":7,"socket":"old.sock","version":1,"startedAt":"2026-08-01T00:00:00Z"}`)
	if err := os.WriteFile(path, previous, 0o600); err != nil {
		t.Fatal(err)
	}

	replaceErr := errors.New("injected replacement failure")
	server := &Server{
		startedAt: time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC),
		opts: ServerOptions{
			Paths:   Paths{Status: path},
			Version: 2,
			replaceStatusFile: func(root *os.Root, src, dst string) error {
				staged, err := root.ReadFile(src)
				if err != nil {
					t.Fatalf("read staged status: %v", err)
				}
				var decoded StatusFile
				if err := json.Unmarshal(staged, &decoded); err != nil {
					t.Fatalf("staged status is incomplete JSON: %v", err)
				}
				current, err := root.ReadFile(dst)
				if err != nil {
					t.Fatalf("read previous status at commit boundary: %v", err)
				}
				if string(current) != string(previous) {
					t.Fatalf("previous status changed before commit: %q", current)
				}
				return replaceErr
			},
		},
	}

	err := server.writeStatusFile()
	if !errors.Is(err, replaceErr) {
		t.Fatalf("writeStatusFile error = %v, want injected replacement failure", err)
	}
	current, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read preserved status: %v", err)
	}
	if string(current) != string(previous) {
		t.Fatalf("status after failed publication = %q, want previous document", current)
	}
	assertNoStatusTemps(t, dir)
}

func TestWriteStatusFilePublishesCompleteRestrictedDocument(t *testing.T) {
	dir := t.TempDir()
	secureStatusTestDir(t, dir)
	path := filepath.Join(dir, "daemon.status")
	if err := os.WriteFile(path, []byte(`{"pid":7}`), 0o644); err != nil {
		t.Fatal(err)
	}

	startedAt := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	parentSynced := false
	server := &Server{
		startedAt: startedAt,
		opts: ServerOptions{
			Paths:   Paths{Socket: filepath.Join(dir, "daemon.sock"), Status: path},
			Version: 3,
			syncStatusParent: func(root *os.Root) error {
				if _, err := root.Stat("."); err != nil {
					t.Fatalf("stat bound status parent: %v", err)
				}
				parentSynced = true
				return nil
			},
		},
	}

	if err := server.writeStatusFile(); err != nil {
		t.Fatalf("writeStatusFile: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var status StatusFile
	if err := json.Unmarshal(data, &status); err != nil {
		t.Fatalf("published status is invalid JSON: %v", err)
	}
	if status.PID != os.Getpid() || status.Socket != server.opts.Paths.Socket || status.Version != 3 || !status.StartedAt.Equal(startedAt) {
		t.Fatalf("published status = %+v", status)
	}
	if !parentSynced {
		t.Fatal("status parent directory was not synced")
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Fatalf("status mode = %04o, want 0600", got)
		}
	}
	assertNoStatusTemps(t, dir)
}

func TestWriteStatusFileReaderSeesCompleteDocumentsDuringPublication(t *testing.T) {
	dir := t.TempDir()
	secureStatusTestDir(t, dir)
	path := filepath.Join(dir, "daemon.status")
	previous := StatusFile{
		PID:       7,
		Socket:    filepath.Join(dir, "old.sock"),
		Version:   1,
		StartedAt: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
	}
	previousData, err := json.Marshal(previous)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, previousData, 0o600); err != nil {
		t.Fatal(err)
	}

	replacementReady := make(chan struct{})
	publish := make(chan struct{})
	writeDone := make(chan error, 1)
	server := &Server{
		startedAt: time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC),
		opts: ServerOptions{
			Paths:   Paths{Socket: filepath.Join(dir, "new.sock"), Status: path},
			Version: 2,
			replaceStatusFile: func(root *os.Root, src, dst string) error {
				close(replacementReady)
				<-publish
				return fsutil.RenameWithRetry(src, dst, root.Rename)
			},
		},
	}
	go func() {
		writeDone <- server.writeStatusFile()
	}()

	<-replacementReady
	for range 100 {
		status := readStatusDocument(t, path)
		if status != previous {
			t.Fatalf("status before commit = %+v, want previous document %+v", status, previous)
		}
	}
	close(publish)
	if err := <-writeDone; err != nil {
		t.Fatalf("writeStatusFile: %v", err)
	}

	status := readStatusDocument(t, path)
	if status.PID != os.Getpid() || status.Socket != server.opts.Paths.Socket || status.Version != 2 || !status.StartedAt.Equal(server.startedAt) {
		t.Fatalf("status after commit = %+v", status)
	}
	assertNoStatusTemps(t, dir)
}

func TestWriteStatusFileContinuesAfterDirectorySyncWarning(t *testing.T) {
	dir := t.TempDir()
	secureStatusTestDir(t, dir)
	path := filepath.Join(dir, "daemon.status")
	if err := os.WriteFile(path, []byte(`{"pid":7}`), 0o600); err != nil {
		t.Fatal(err)
	}

	syncErr := errors.New("injected directory sync failure")
	var logs []string
	server := &Server{
		startedAt: time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC),
		opts: ServerOptions{
			Paths:            Paths{Socket: filepath.Join(dir, "daemon.sock"), Status: path},
			Version:          4,
			Log:              func(message string) { logs = append(logs, message) },
			syncStatusParent: func(*os.Root) error { return syncErr },
		},
	}

	if err := server.writeStatusFile(); err != nil {
		t.Fatalf("writeStatusFile returned a post-commit warning as failure: %v", err)
	}
	status := readStatusDocument(t, path)
	if status.Version != 4 {
		t.Fatalf("published status = %+v, want version 4", status)
	}
	if len(logs) != 1 || !strings.Contains(logs[0], syncErr.Error()) {
		t.Fatalf("logs = %q, want directory sync warning", logs)
	}
	assertNoStatusTemps(t, dir)
}

func TestWriteStatusFileContinuesAfterCommittedReplacementWarning(t *testing.T) {
	dir := t.TempDir()
	secureStatusTestDir(t, dir)
	path := filepath.Join(dir, "daemon.status")
	if err := os.WriteFile(path, []byte(`{"pid":7}`), 0o600); err != nil {
		t.Fatal(err)
	}

	cleanupErr := errors.New("injected backup cleanup failure")
	parentSynced := false
	var logs []string
	server := &Server{
		startedAt: time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC),
		opts: ServerOptions{
			Paths:   Paths{Socket: filepath.Join(dir, "daemon.sock"), Status: path},
			Version: 5,
			Log:     func(message string) { logs = append(logs, message) },
			replaceStatusFile: func(root *os.Root, src, dst string) error {
				if err := fsutil.RenameWithRetry(src, dst, root.Rename); err != nil {
					return err
				}
				return &fsutil.CommittedReplacementCleanupError{
					BackupPath: filepath.Join(dir, ".zero-replace-backup"),
					Cause:      cleanupErr,
				}
			},
			syncStatusParent: func(*os.Root) error {
				parentSynced = true
				return nil
			},
		},
	}

	if err := server.writeStatusFile(); err != nil {
		t.Fatalf("writeStatusFile returned a post-commit warning as failure: %v", err)
	}
	if !parentSynced {
		t.Fatal("status directory was not synced after committed replacement warning")
	}
	status := readStatusDocument(t, path)
	if status.Version != 5 {
		t.Fatalf("published status = %+v, want version 5", status)
	}
	if len(logs) != 1 || !strings.Contains(logs[0], cleanupErr.Error()) {
		t.Fatalf("logs = %q, want committed cleanup warning", logs)
	}
	assertNoStatusTemps(t, dir)
}

func TestWriteStatusFileContinuesAfterCommittedTempCleanupWarning(t *testing.T) {
	dir := t.TempDir()
	secureStatusTestDir(t, dir)
	path := filepath.Join(dir, "daemon.status")
	if err := os.WriteFile(path, []byte(`{"pid":7}`), 0o600); err != nil {
		t.Fatal(err)
	}

	var tempName string
	var logs []string
	server := &Server{
		startedAt: time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC),
		opts: ServerOptions{
			Paths:   Paths{Socket: filepath.Join(dir, "daemon.sock"), Status: path},
			Version: 6,
			Log:     func(message string) { logs = append(logs, message) },
			replaceStatusFile: func(root *os.Root, src, dst string) error {
				if err := fsutil.RenameWithRetry(src, dst, root.Rename); err != nil {
					return err
				}
				tempName = src
				if err := root.Mkdir(src, 0o700); err != nil {
					t.Fatalf("recreate staged path as directory: %v", err)
				}
				keep, err := root.OpenFile(filepath.Join(src, "keep"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
				if err != nil {
					t.Fatalf("make staged directory non-empty: %v", err)
				}
				if err := keep.Close(); err != nil {
					t.Fatalf("close staged directory marker: %v", err)
				}
				return nil
			},
		},
	}

	if err := server.writeStatusFile(); err != nil {
		t.Fatalf("writeStatusFile returned a post-commit warning as failure: %v", err)
	}
	status := readStatusDocument(t, path)
	if status.Version != 6 {
		t.Fatalf("published status = %+v, want version 6", status)
	}
	if len(logs) != 1 || !strings.Contains(logs[0], "remove temporary status file") {
		t.Fatalf("logs = %q, want temporary cleanup warning", logs)
	}
	if err := os.Remove(filepath.Join(dir, tempName, "keep")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, tempName)); err != nil {
		t.Fatal(err)
	}
	assertNoStatusTemps(t, dir)
}

func TestWriteStatusFileBindsDirectoryDuringAncestorSwap(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "live")
	movedDir := filepath.Join(parent, "moved")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	secureStatusTestDir(t, dir)
	path := filepath.Join(dir, "daemon.status")
	if err := os.WriteFile(path, []byte(`{"pid":7}`), 0o600); err != nil {
		t.Fatal(err)
	}

	substitute := []byte(`{"pid":999,"socket":"substitute"}`)
	var swapErr error
	server := &Server{
		startedAt: time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC),
		opts: ServerOptions{
			Paths:   Paths{Socket: filepath.Join(dir, "daemon.sock"), Status: path},
			Version: 6,
			beforeStatusReplace: func() {
				swapErr = os.Rename(dir, movedDir)
				if swapErr != nil {
					if runtime.GOOS == "windows" {
						return
					}
					t.Fatalf("move bound status directory: %v", swapErr)
				}
				if err := os.Mkdir(dir, 0o700); err != nil {
					t.Fatalf("create substitute status directory: %v", err)
				}
				if err := os.WriteFile(path, substitute, 0o600); err != nil {
					t.Fatalf("write substitute status: %v", err)
				}
			},
		},
	}

	if err := server.writeStatusFile(); err != nil {
		t.Fatalf("writeStatusFile: %v", err)
	}
	if swapErr != nil {
		if runtime.GOOS != "windows" {
			t.Fatalf("unexpected directory-swap error: %v", swapErr)
		}
		status := readStatusDocument(t, path)
		if status.Version != 6 {
			t.Fatalf("status after blocked swap = %+v, want version 6", status)
		}
		if _, err := os.Lstat(movedDir); !os.IsNotExist(err) {
			t.Fatalf("moved directory exists after blocked Windows swap: %v", err)
		}
		assertNoStatusTemps(t, dir)
		return
	}
	if got, err := os.ReadFile(path); err != nil {
		t.Fatal(err)
	} else if string(got) != string(substitute) {
		t.Fatalf("substitute destination changed: %q", got)
	}
	status := readStatusDocument(t, filepath.Join(movedDir, "daemon.status"))
	if status.Version != 6 {
		t.Fatalf("bound status = %+v, want version 6", status)
	}
	assertNoStatusTemps(t, dir)
	assertNoStatusTemps(t, movedDir)
}

func TestWriteStatusFileAllowsReadOnlyCustomStatusDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows directory access is governed by DACLs, not Unix mode bits")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "daemon.status")

	server := &Server{
		startedAt: time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC),
		opts: ServerOptions{
			Paths:   Paths{Status: path},
			Version: 6,
		},
	}
	err := server.writeStatusFile()
	if err != nil {
		t.Fatalf("writeStatusFile in 0755 directory: %v", err)
	}
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("status file not created in read-only custom directory: %v", err)
	}
	assertNoStatusTemps(t, dir)
}

func TestWriteStatusFileRejectsWritableStatusDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows directory access is governed by DACLs, not Unix mode bits")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "daemon.status")
	server := &Server{
		startedAt: time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC),
		opts:      ServerOptions{Paths: Paths{Status: path}, Version: 6},
	}
	err := server.writeStatusFile()
	if err == nil || !strings.Contains(err.Error(), "no group/other write access") {
		t.Fatalf("writeStatusFile error = %v, want writable-directory rejection", err)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("status file created in writable directory: %v", err)
	}
	assertNoStatusTemps(t, dir)
}

func TestPrivateDirHardensBroadCurrentUserStatusDirectory(t *testing.T) {
	dir := t.TempDir()
	broadenStatusTestDirPlatform(t, dir)
	if err := privatedir.Ensure(dir); err != nil {
		t.Fatalf("privatedir.Ensure: %v", err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	if err := validateStatusRoot(root); err != nil {
		t.Fatalf("validate hardened status root: %v", err)
	}
}

func readStatusDocument(t *testing.T, path string) StatusFile {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read status: %v", err)
	}
	var status StatusFile
	if err := json.Unmarshal(data, &status); err != nil {
		t.Fatalf("status is incomplete JSON: %v (content %q)", err, data)
	}
	return status
}

func assertNoStatusTemps(t *testing.T, dir string) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, statusTempPattern))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("temporary status files remain: %v", matches)
	}
}

func secureStatusTestDir(t *testing.T, dir string) {
	t.Helper()
	secureStatusTestDirPlatform(t, dir)
}
