package background

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestManagerRegistersListsAndKillsTask(t *testing.T) {
	now := sequenceClock(
		time.Date(2026, 6, 7, 9, 0, 0, 0, time.UTC),
		time.Date(2026, 6, 7, 9, 0, 1, 0, time.UTC),
		time.Date(2026, 6, 7, 9, 0, 2, 0, time.UTC),
	)
	killed := []int{}
	manager, err := NewManagerWithOptions(ManagerOptions{
		RootDir: t.TempDir(),
		Now:     now,
		KillProcess: func(pid int) error {
			killed = append(killed, pid)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("NewManagerWithOptions returned error: %v", err)
	}

	outputFile, err := manager.Register(RegisterInput{
		TaskID:         "task_1",
		Type:           "specialist",
		SpecialistName: "worker",
		Description:    "Read the release notes",
		ParentID:       "parent",
	})
	if err != nil {
		t.Fatalf("Register returned error: %v", err)
	}
	if outputFile != filepath.Join(manager.RootDir(), "task_1.ndjson") {
		t.Fatalf("output file = %q", outputFile)
	}
	info, err := os.Stat(outputFile)
	if err != nil {
		t.Fatalf("expected output file to exist: %v", err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("output file permissions = %v, want 0600", info.Mode().Perm())
	}
	if err := manager.SetPID("task_1", 1234); err != nil {
		t.Fatalf("SetPID returned error: %v", err)
	}
	if _, err := manager.Register(RegisterInput{TaskID: "task_2", Type: "specialist", SpecialistName: "explorer", ParentID: "parent", PID: 5678}); err != nil {
		t.Fatalf("Register second task returned error: %v", err)
	}
	if _, err := manager.Register(RegisterInput{TaskID: "other", Type: "specialist", ParentID: "different-parent"}); err != nil {
		t.Fatalf("Register other task returned error: %v", err)
	}

	task, ok := manager.Get("task_1")
	if !ok {
		t.Fatalf("Get did not find task")
	}
	if task.ID != "task_1" || task.PID != 1234 || task.Status != StatusRunning || task.SpecialistName != "worker" {
		t.Fatalf("unexpected task: %#v", task)
	}

	parentTasks := manager.ListByParent("parent")
	gotIDs := []string{}
	for _, task := range parentTasks {
		gotIDs = append(gotIDs, task.ID)
	}
	if !reflect.DeepEqual(gotIDs, []string{"task_2", "task_1"}) {
		t.Fatalf("ListByParent ids = %#v, want newest first", gotIDs)
	}

	if err := manager.Kill("task_1"); err != nil {
		t.Fatalf("Kill returned error: %v", err)
	}
	if !reflect.DeepEqual(killed, []int{1234}) {
		t.Fatalf("killed pids = %#v", killed)
	}
	task, ok = manager.Get("task_1")
	if !ok || task.Status != StatusKilled || task.ExitCode != -1 || task.CompletedAt.IsZero() {
		t.Fatalf("killed task status was not recorded: %#v", task)
	}
}

func TestManagerPersistsAndLoadsTasks(t *testing.T) {
	root := t.TempDir()
	now := sequenceClock(
		time.Date(2026, 6, 7, 10, 0, 0, 0, time.UTC),
		time.Date(2026, 6, 7, 10, 0, 1, 0, time.UTC),
	)
	manager, err := NewManagerWithOptions(ManagerOptions{RootDir: root, Now: now})
	if err != nil {
		t.Fatalf("NewManagerWithOptions returned error: %v", err)
	}
	outputFile, err := manager.Register(RegisterInput{
		TaskID:         "task_1",
		Type:           "specialist",
		SpecialistName: "worker",
		Description:    "Persist metadata",
		ParentID:       "parent",
	})
	if err != nil {
		t.Fatalf("Register returned error: %v", err)
	}
	if err := manager.SetPID("task_1", 4321); err != nil {
		t.Fatalf("SetPID returned error: %v", err)
	}
	if err := manager.UpdateStatus("task_1", StatusCompleted, 0); err != nil {
		t.Fatalf("UpdateStatus returned error: %v", err)
	}

	reloaded, err := NewManager(root)
	if err != nil {
		t.Fatalf("NewManager reload returned error: %v", err)
	}
	task, ok := reloaded.Get("task_1")
	if !ok {
		t.Fatal("reloaded manager did not find task")
	}
	if task.ID != "task_1" ||
		task.Type != "specialist" ||
		task.SpecialistName != "worker" ||
		task.Description != "Persist metadata" ||
		task.ParentID != "parent" ||
		task.PID != 4321 ||
		task.Status != StatusCompleted ||
		task.ExitCode != 0 ||
		task.OutputFile != outputFile ||
		task.StartedAt.IsZero() ||
		task.CompletedAt.IsZero() {
		t.Fatalf("reloaded task = %#v", task)
	}
	parentTasks := reloaded.ListByParent("parent")
	if len(parentTasks) != 1 || parentTasks[0].ID != "task_1" {
		t.Fatalf("reloaded parent tasks = %#v", parentTasks)
	}
}

func TestManagerPersistsKilledStatus(t *testing.T) {
	root := t.TempDir()
	manager, err := NewManagerWithOptions(ManagerOptions{
		RootDir: root,
		KillProcess: func(pid int) error {
			return nil
		},
	})
	if err != nil {
		t.Fatalf("NewManagerWithOptions returned error: %v", err)
	}
	if _, err := manager.Register(RegisterInput{TaskID: "task", Type: "specialist", PID: 42}); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}
	if err := manager.Kill("task"); err != nil {
		t.Fatalf("Kill returned error: %v", err)
	}

	reloaded, err := NewManager(root)
	if err != nil {
		t.Fatalf("NewManager reload returned error: %v", err)
	}
	task, ok := reloaded.Get("task")
	if !ok || task.Status != StatusKilled || task.ExitCode != -1 || task.CompletedAt.IsZero() {
		t.Fatalf("reloaded killed task = %#v", task)
	}
}

func TestManagerSkipsInvalidPersistedMetadata(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "task.json"), []byte(`{"id":"other","type":"specialist","status":"running","outputFile":"task.ndjson"}`+"\n"), 0o600); err != nil {
		t.Fatalf("write metadata: %v", err)
	}

	manager, err := NewManager(root)

	if err != nil {
		t.Fatalf("NewManager returned error for invalid metadata: %v", err)
	}
	if _, ok := manager.Get("task"); ok {
		t.Fatal("manager loaded invalid task metadata")
	}
	warnings := strings.Join(manager.LoadWarnings(), "\n")
	if !strings.Contains(warnings, "does not match file id") {
		t.Fatalf("load warnings = %q, want id mismatch warning", warnings)
	}
}

func TestManagerMigratesLegacyOutputPathWhenCanonicalOutputExists(t *testing.T) {
	root := t.TempDir()
	const taskID = "legacy_task"
	canonicalOutput := filepath.Join(root, taskID+".ndjson")
	if err := os.WriteFile(canonicalOutput, []byte("legacy output\n"), 0o600); err != nil {
		t.Fatalf("write canonical output: %v", err)
	}
	legacyOutput := filepath.Join(t.TempDir(), "zero", "background", taskID+".ndjson")
	metadata, err := json.Marshal(Task{
		ID:         taskID,
		Type:       "specialist",
		Status:     StatusCompleted,
		OutputFile: legacyOutput,
		StartedAt:  time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("marshal legacy metadata: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, taskID+".json"), append(metadata, '\n'), 0o600); err != nil {
		t.Fatalf("write legacy metadata: %v", err)
	}

	manager, err := NewManager(root)
	if err != nil {
		t.Fatalf("NewManager returned error: %v", err)
	}
	task, ok := manager.Get(taskID)
	if !ok {
		t.Fatal("manager did not load migrated task")
	}
	if task.OutputFile != canonicalOutput {
		t.Fatalf("migrated output file = %q, want %q", task.OutputFile, canonicalOutput)
	}
	if warnings := manager.LoadWarnings(); len(warnings) != 0 {
		t.Fatalf("migration produced load warnings: %#v", warnings)
	}

	persisted, err := os.ReadFile(filepath.Join(root, taskID+".json"))
	if err != nil {
		t.Fatalf("read migrated metadata: %v", err)
	}
	var saved Task
	if err := json.Unmarshal(persisted, &saved); err != nil {
		t.Fatalf("decode migrated metadata: %v", err)
	}
	if saved.OutputFile != canonicalOutput {
		t.Fatalf("persisted output file = %q, want %q", saved.OutputFile, canonicalOutput)
	}
}

func TestManagerRejectsUnrelatedAbsoluteOutputPathDuringMigration(t *testing.T) {
	root := t.TempDir()
	const taskID = "legacy_task"
	if err := os.WriteFile(filepath.Join(root, taskID+".ndjson"), []byte("output\n"), 0o600); err != nil {
		t.Fatalf("write canonical output: %v", err)
	}
	metadata, err := json.Marshal(Task{
		ID:         taskID,
		Type:       "specialist",
		Status:     StatusCompleted,
		OutputFile: filepath.Join(t.TempDir(), taskID+".ndjson"),
		StartedAt:  time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("marshal metadata: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, taskID+".json"), append(metadata, '\n'), 0o600); err != nil {
		t.Fatalf("write metadata: %v", err)
	}

	manager, err := NewManager(root)
	if err != nil {
		t.Fatalf("NewManager returned error: %v", err)
	}
	if _, ok := manager.Get(taskID); ok {
		t.Fatal("manager loaded task with an unrelated absolute output path")
	}
	if warnings := strings.Join(manager.LoadWarnings(), "\n"); !strings.Contains(warnings, "output file must be inside") {
		t.Fatalf("load warnings = %q, want containment rejection", warnings)
	}
}

func TestManagerMarksReloadedRunningTaskNonKillable(t *testing.T) {
	root := t.TempDir()
	manager, err := NewManager(root)
	if err != nil {
		t.Fatalf("NewManager returned error: %v", err)
	}
	if _, err := manager.Register(RegisterInput{TaskID: "task", Type: "specialist", PID: 42}); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}
	killed := []int{}
	reloaded, err := NewManagerWithOptions(ManagerOptions{
		RootDir: root,
		KillProcess: func(pid int) error {
			killed = append(killed, pid)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("NewManager reload returned error: %v", err)
	}
	task, ok := reloaded.Get("task")
	if !ok {
		t.Fatal("reloaded manager did not find task")
	}
	if task.Status != StatusError || task.PID != 0 || task.ExitCode != -1 || task.CompletedAt.IsZero() {
		t.Fatalf("reloaded running task was not orphaned: %#v", task)
	}
	if err := reloaded.Kill("task"); err == nil || !strings.Contains(err.Error(), "is error") {
		t.Fatalf("Kill reloaded running task error = %v", err)
	}
	if len(killed) != 0 {
		t.Fatalf("Kill signaled stale pids: %#v", killed)
	}

	reloadedAgain, err := NewManager(root)
	if err != nil {
		t.Fatalf("second reload returned error: %v", err)
	}
	task, ok = reloadedAgain.Get("task")
	if !ok || task.Status != StatusError || task.PID != 0 {
		t.Fatalf("repaired task metadata did not persist: %#v", task)
	}
}

func TestManagerRejectsUnsafeTaskIDsAndOutputPaths(t *testing.T) {
	manager, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewManager returned error: %v", err)
	}

	for _, taskID := range []string{"", "../escape", "bad/name", `bad\name`} {
		if _, err := manager.Register(RegisterInput{TaskID: taskID, Type: "specialist"}); err == nil {
			t.Fatalf("Register accepted unsafe task id %q", taskID)
		}
	}
	if _, err := manager.Register(RegisterInput{TaskID: "safe", Type: "specialist", OutputFile: "../escape.ndjson"}); err == nil || !strings.Contains(err.Error(), "inside") {
		t.Fatalf("Register unsafe output file error = %v", err)
	}
}

func TestManagerRejectsDuplicateAndMissingTasks(t *testing.T) {
	manager, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewManager returned error: %v", err)
	}
	if _, err := manager.Register(RegisterInput{TaskID: "task", Type: "specialist"}); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}
	if _, err := manager.Register(RegisterInput{TaskID: "task", Type: "specialist"}); err == nil || !strings.Contains(err.Error(), "already registered") {
		t.Fatalf("duplicate Register error = %v", err)
	}
	if err := manager.SetPID("missing", 1); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("missing SetPID error = %v", err)
	}
	if err := manager.UpdateStatus("missing", StatusCompleted, 0); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("missing UpdateStatus error = %v", err)
	}
	if err := manager.Kill("missing"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("missing Kill error = %v", err)
	}
}

func TestManagerDoesNotMarkKilledWhenKillFails(t *testing.T) {
	manager, err := NewManagerWithOptions(ManagerOptions{
		RootDir: t.TempDir(),
		KillProcess: func(pid int) error {
			return errors.New("denied")
		},
	})
	if err != nil {
		t.Fatalf("NewManagerWithOptions returned error: %v", err)
	}
	if _, err := manager.Register(RegisterInput{TaskID: "task", Type: "specialist", PID: 42}); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}
	if err := manager.Kill("task"); err == nil || !strings.Contains(err.Error(), "denied") {
		t.Fatalf("Kill error = %v", err)
	}
	task, ok := manager.Get("task")
	if !ok || task.Status != StatusRunning || !task.CompletedAt.IsZero() {
		t.Fatalf("failed kill mutated task: %#v", task)
	}
}

func TestManagerKillDoesNotClobberCompletedTask(t *testing.T) {
	now := sequenceClock(
		time.Date(2026, 6, 7, 9, 0, 0, 0, time.UTC),
		time.Date(2026, 6, 7, 9, 0, 1, 0, time.UTC),
	)
	var manager *Manager
	manager, err := NewManagerWithOptions(ManagerOptions{
		RootDir: t.TempDir(),
		Now:     now,
		KillProcess: func(pid int) error {
			return manager.UpdateStatus("task", StatusCompleted, 0)
		},
	})
	if err != nil {
		t.Fatalf("NewManagerWithOptions returned error: %v", err)
	}
	if _, err := manager.Register(RegisterInput{TaskID: "task", Type: "specialist", PID: 42}); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}
	if err := manager.Kill("task"); err != nil {
		t.Fatalf("Kill returned error: %v", err)
	}
	task, ok := manager.Get("task")
	if !ok || task.Status != StatusCompleted || task.ExitCode != 0 {
		t.Fatalf("Kill clobbered completed task: %#v", task)
	}
}

func TestManagerMarkExitedDoesNotClobberKilledTask(t *testing.T) {
	manager, err := NewManagerWithOptions(ManagerOptions{
		RootDir: t.TempDir(),
		KillProcess: func(pid int) error {
			return nil
		},
	})
	if err != nil {
		t.Fatalf("NewManagerWithOptions returned error: %v", err)
	}
	if _, err := manager.Register(RegisterInput{TaskID: "task", Type: "specialist", PID: 42}); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}
	if err := manager.Kill("task"); err != nil {
		t.Fatalf("Kill returned error: %v", err)
	}
	if err := manager.MarkExited("task", StatusError, 1); err != nil {
		t.Fatalf("MarkExited returned error: %v", err)
	}
	task, ok := manager.Get("task")
	if !ok || task.Status != StatusKilled || task.ExitCode != -1 {
		t.Fatalf("MarkExited clobbered killed task: %#v", task)
	}
}

func TestManagerKillRunningStopsOnlyRunningTasks(t *testing.T) {
	now := sequenceClock(
		time.Date(2026, 6, 7, 9, 0, 0, 0, time.UTC),
		time.Date(2026, 6, 7, 9, 0, 1, 0, time.UTC),
		time.Date(2026, 6, 7, 9, 0, 2, 0, time.UTC),
	)
	killed := []int{}
	manager, err := NewManagerWithOptions(ManagerOptions{
		RootDir: t.TempDir(),
		Now:     now,
		KillProcess: func(pid int) error {
			killed = append(killed, pid)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("NewManagerWithOptions returned error: %v", err)
	}
	if _, err := manager.Register(RegisterInput{TaskID: "running_1", Type: "specialist", PID: 11}); err != nil {
		t.Fatalf("Register running_1 returned error: %v", err)
	}
	if _, err := manager.Register(RegisterInput{TaskID: "done", Type: "specialist", PID: 22}); err != nil {
		t.Fatalf("Register done returned error: %v", err)
	}
	if err := manager.UpdateStatus("done", StatusCompleted, 0); err != nil {
		t.Fatalf("UpdateStatus returned error: %v", err)
	}
	if _, err := manager.Register(RegisterInput{TaskID: "running_2", Type: "specialist", PID: 33}); err != nil {
		t.Fatalf("Register running_2 returned error: %v", err)
	}

	if err := manager.KillRunning(); err != nil {
		t.Fatalf("KillRunning returned error: %v", err)
	}

	if !reflect.DeepEqual(killed, []int{33, 11}) {
		t.Fatalf("killed pids = %#v", killed)
	}
	for _, id := range []string{"running_1", "running_2"} {
		task, ok := manager.Get(id)
		if !ok || task.Status != StatusKilled {
			t.Fatalf("%s status = %#v", id, task)
		}
	}
	done, ok := manager.Get("done")
	if !ok || done.Status != StatusCompleted {
		t.Fatalf("completed task was changed: %#v", done)
	}
}

func TestDefaultRootHonorsXDGDataHome(t *testing.T) {
	got := DefaultRoot(map[string]string{
		"XDG_DATA_HOME": "/tmp/zero-data",
		"HOME":          "/home/example",
	})
	want := filepath.Join("/tmp/zero-data", "zero", "background")
	if got != want {
		t.Fatalf("DefaultRoot = %q, want %q", got, want)
	}
}

// TestManagerKillStaysKilledWhenExitWaiterReapsDuringTerminate reproduces the
// lifecycle race created by a blocking terminateProcess: while Kill waits for the
// child to exit, the background Wait-goroutine reaps it and calls MarkExited. The
// task must stay "killed" (the user's intent), not be clobbered to "error".
func TestManagerKillStaysKilledWhenExitWaiterReapsDuringTerminate(t *testing.T) {
	now := sequenceClock(
		time.Date(2026, 6, 8, 9, 0, 0, 0, time.UTC),
		time.Date(2026, 6, 8, 9, 0, 1, 0, time.UTC),
		time.Date(2026, 6, 8, 9, 0, 2, 0, time.UTC),
	)
	var manager *Manager
	var err error
	manager, err = NewManagerWithOptions(ManagerOptions{
		RootDir: t.TempDir(),
		Now:     now,
		KillProcess: func(int) error {
			// terminateProcess blocks until exit; simulate the Wait-goroutine
			// reaping the child and marking it exited mid-kill.
			_ = manager.MarkExited("task_1", StatusError, -1)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("NewManagerWithOptions returned error: %v", err)
	}
	if _, err := manager.Register(RegisterInput{TaskID: "task_1", Type: "specialist", SpecialistName: "worker", ParentID: "p", PID: 4242}); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}

	if err := manager.Kill("task_1"); err != nil {
		t.Fatalf("Kill returned error: %v", err)
	}
	task, ok := manager.Get("task_1")
	if !ok {
		t.Fatal("task_1 not found after Kill")
	}
	if task.Status != StatusKilled {
		t.Fatalf("status = %q, want %q — the exit waiter must not clobber a user-initiated kill", task.Status, StatusKilled)
	}
}

func TestMarkKilledIfStillRunningReportsWhetherItMarked(t *testing.T) {
	manager, err := NewManagerWithOptions(ManagerOptions{
		RootDir: t.TempDir(),
		Now:     sequenceClock(time.Date(2026, 6, 8, 9, 0, 0, 0, time.UTC)),
	})
	if err != nil {
		t.Fatalf("NewManagerWithOptions returned error: %v", err)
	}
	if _, err := manager.Register(RegisterInput{TaskID: "t", Type: "specialist", PID: 7}); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}

	marked, err := manager.markKilledIfStillRunning("t", 7)
	if err != nil || !marked {
		t.Fatalf("first call: marked=%v err=%v, want true/nil", marked, err)
	}
	// Already killed (not running) → must report not-marked so Kill skips signaling
	// a possibly-stale pid.
	marked, err = manager.markKilledIfStillRunning("t", 7)
	if err != nil || marked {
		t.Fatalf("second call: marked=%v err=%v, want false/nil", marked, err)
	}
}

func sequenceClock(times ...time.Time) func() time.Time {
	index := 0
	return func() time.Time {
		if index >= len(times) {
			return times[len(times)-1]
		}
		next := times[index]
		index++
		return next
	}
}
