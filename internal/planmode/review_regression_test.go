package planmode

import (
	"errors"
	"github.com/Gitlawb/zero/internal/sandbox"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestCommitRejectsUnavailableBaseline(t *testing.T) {
	for _, kind := range []string{"missing", "invalid", "unreadable"} {
		t.Run(kind, func(t *testing.T) {
			isolatePlanStorage(t)
			workspace := t.TempDir()
			if _, err := WritePlan(workspace, "session", "original"); err != nil {
				t.Fatal(err)
			}
			staged, cleanup, err := StageForEditor(workspace, "session")
			if err != nil {
				t.Fatal(err)
			}
			defer cleanup()
			if err := os.WriteFile(staged, []byte("edited"), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := WritePlan(workspace, "session", "newer"); err != nil {
				t.Fatal(err)
			}
			baseline := staged + ".basehash"
			if err := os.Remove(baseline); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "invalid":
				if err := os.WriteFile(baseline, []byte("invalid"), 0600); err != nil {
					t.Fatal(err)
				}
			case "unreadable":
				if err := os.Mkdir(baseline, 0700); err != nil {
					t.Fatal(err)
				}
			}
			if err := CommitStagedEdit(workspace, "session", staged); err == nil {
				t.Fatal("accepted edit without valid baseline")
			}
			got, _, err := ReadPlan(workspace, "session")
			if err != nil || got != "newer\n" {
				t.Fatalf("durable = %q, %v", got, err)
			}
			data, err := os.ReadFile(staged)
			if err != nil || string(data) != "edited" {
				t.Fatalf("saved edit lost: %q, %v", data, err)
			}
		})
	}
}

func TestEditorBaselineCreationAndWriteFailures(t *testing.T) {
	sentinel := errors.New("baseline create failure")
	if err := writeEditorBaseline("plan", func() (*os.File, error) { return nil, sentinel }); !errors.Is(err, sentinel) {
		t.Fatalf("creation error = %v", err)
	}
	path := filepath.Join(t.TempDir(), "read-only-handle")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	var opened *os.File
	err := writeEditorBaseline("plan", func() (*os.File, error) {
		var err error
		opened, err = os.Open(path)
		return opened, err
	})
	if err == nil {
		t.Fatal("ignored baseline write failure")
	}
	if err := opened.Close(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("baseline handle not closed: %v", err)
	}
}

func TestCommitEditorOutcomes(t *testing.T) {
	for _, kind := range []string{"unchanged", "external", "edit", "clear"} {
		t.Run(kind, func(t *testing.T) {
			isolatePlanStorage(t)
			workspace := t.TempDir()
			if _, err := WritePlan(workspace, "session", "first\r\nsecond"); err != nil {
				t.Fatal(err)
			}
			staged, cleanup, err := StageForEditor(workspace, "session")
			if err != nil {
				t.Fatal(err)
			}
			defer cleanup()
			switch kind {
			case "external":
				_, err = WritePlan(workspace, "session", "newer")
			case "edit":
				err = os.WriteFile(staged, []byte("edited"), 0600)
			case "clear":
				err = os.WriteFile(staged, nil, 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			result, err := CommitStagedEditResult(workspace, "session", staged)
			if err != nil {
				t.Fatal(err)
			}
			if result.Edited != (kind == "edit" || kind == "clear") || result.Reload != (kind != "unchanged") {
				t.Fatalf("outcome for %s: %+v", kind, result)
			}
			if kind == "external" {
				got, _, err := ReadPlan(workspace, "session")
				if err != nil || strings.TrimSpace(got) != "newer" {
					t.Fatalf("external plan lost: %q, %v", got, err)
				}
			}
		})
	}
}

func TestStorageRejectsAllSandboxTempRoots(t *testing.T) {
	root := t.TempDir()
	first := filepath.Join(root, "temp")
	second := filepath.Join(root, "tmp")
	for _, dir := range []string{first, second} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if runtime.GOOS == "windows" {
		t.Setenv("TEMP", first)
		t.Setenv("TMP", second)
	} else {
		t.Setenv("TMPDIR", first)
	}
	tempDirMu.Lock()
	previous := tempDirFn
	tempDirFn = sandbox.DefaultTempWriteRoots
	tempDirMu.Unlock()
	t.Cleanup(func() { tempDirMu.Lock(); tempDirFn = previous; tempDirMu.Unlock() })
	workspace := filepath.Join(root, "workspace")
	for _, configRoot := range []string{first, second} {
		t.Run(filepath.Base(configRoot), func(t *testing.T) {
			setUserConfigHomeEnv(t, configRoot)
			if _, err := WritePlan(workspace, "session", "unsafe"); err == nil {
				t.Fatal("durable storage accepted a sandbox-writable root")
			}
			if _, cleanup, err := StageForEditor(workspace, "session"); err == nil {
				cleanup()
				t.Fatal("editor staging accepted a sandbox-writable root")
			}
		})
	}
	safe := filepath.Join(root, "safe")
	if runtime.GOOS != "windows" {
		var err error
		safe, err = os.MkdirTemp("/var/tmp", "zero-plan-private-*")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(safe) })
	}
	setUserConfigHomeEnv(t, safe)
	if _, err := WritePlan(workspace, "session", "safe"); err != nil {
		t.Fatalf("legitimate redirected config rejected: %v", err)
	}
	_, cleanup, err := StageForEditor(workspace, "session")
	if err != nil {
		t.Fatal(err)
	}
	cleanup()
	alias := filepath.Join(safe, "alias")
	if err := os.Symlink(second, alias); err != nil {
		t.Logf("symlink fixture unavailable: %v", err)
		return
	}
	setUserConfigHomeEnv(t, alias)
	if _, err := WritePlan(workspace, "session", "unsafe"); err == nil {
		t.Fatal("physical alias into writable temp root accepted")
	}
	if _, cleanup, err := StageForEditor(workspace, "session"); err == nil {
		cleanup()
		t.Fatal("physical staging alias accepted")
	}
}

func TestStageFailsAndCleansUpWithoutBaseline(t *testing.T) {
	for _, kind := range []string{"create", "write"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			writer := func(content string, create func() (*os.File, error)) error {
				if kind == "create" {
					return errors.New("injected baseline creation failure")
				}
				return writeEditorBaseline(content, func() (*os.File, error) {
					file, err := create()
					if err != nil {
						return nil, err
					}
					if err := file.Close(); err != nil {
						return nil, err
					}
					return file, nil
				})
			}
			path, finish, err := stageContentUnderBase(dir, "session", "plan", writer)
			if err == nil || path != "" || finish != nil {
				t.Fatalf("staging reported usable operation: %q, %v", path, err)
			}
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) != 0 {
				t.Fatalf("failed staging leaked resources: %v, %v", entries, err)
			}
		})
	}
}
