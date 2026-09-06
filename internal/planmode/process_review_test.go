package planmode

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Gitlawb/zero/internal/lockutil"
)

func TestPlanWriterHelperProcess(t *testing.T) {
	if os.Getenv("ZERO_PLAN_WRITER_HELPER") != "1" {
		return
	}
	args := os.Args[len(os.Args)-4:]
	workspace, staged, action, tempRoot := args[0], args[1], args[2], args[3]
	SetTempDirForTest(t, tempRoot)
	fmt.Println("ready")
	if _, err := bufio.NewReader(os.Stdin).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	var err error
	if action == "write" {
		_, err = WritePlan(workspace, "session", "direct writer")
	} else {
		err = CommitStagedEdit(workspace, "session", staged)
	}
	if err != nil {
		fmt.Println("rejected:", err)
		return
	}
	fmt.Println("accepted")
}

type planWriterProcess struct {
	cmd    *exec.Cmd
	start  func()
	done   chan error
	output *bytes.Buffer
}

func startPlanWriterProcess(t *testing.T, workspace, staged, action, tempRoot string) planWriterProcess {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestPlanWriterHelperProcess$", "--", workspace, staged, action, tempRoot)
	cmd.Env = []string{"ZERO_PLAN_WRITER_HELPER=1", "TMPDIR=" + tempRoot, "TEMP=" + tempRoot, "TMP=" + tempRoot}
	for _, key := range []string{"HOME", "USERPROFILE", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "AppData", "LocalAppData"} {
		cmd.Env = append(cmd.Env, key+"="+os.Getenv(key))
	}
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waiting := false
	waited := make(chan struct{})
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		if waiting {
			<-waited
		} else {
			_ = cmd.Wait()
		}
	})
	reader := bufio.NewReader(output)
	line, err := reader.ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "ready" {
		t.Fatalf("helper ready = %q, %v, stderr %s", line, err, stderr.String())
	}
	result := planWriterProcess{cmd: cmd, done: make(chan error, 1), output: new(bytes.Buffer)}
	result.start = func() {
		waiting = true
		if _, err := input.Write([]byte("go\n")); err != nil {
			t.Fatal(err)
		}
		_ = input.Close()
		go func() {
			defer close(waited)
			_, readErr := result.output.ReadFrom(reader)
			waitErr := cmd.Wait()
			if readErr != nil {
				result.done <- readErr
				return
			}
			result.done <- waitErr
		}()
	}
	return result
}

func waitPlanWriter(t *testing.T, child planWriterProcess) string {
	t.Helper()
	select {
	case err := <-child.done:
		if err != nil {
			t.Fatalf("helper failed: %v, %s", err, child.output.String())
		}
		return child.output.String()
	case <-time.After(10 * time.Second):
		t.Fatal("plan writer did not finish")
	}
	return ""
}

func holdPlanWriterLock(t *testing.T, workspace string) *lockutil.FileLock {
	t.Helper()
	path, err := PlanFilePath(workspace, "session")
	if err != nil {
		t.Fatal(err)
	}
	base, _, err := planStorageBase(workspace)
	if err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(base, filepath.Base(filepath.Dir(path))+"-"+filepath.Base(path)+".lock")
	lock, err := lockutil.TryAcquireFileLockAt(filepath.Dir(base), lockPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lock.Release() })
	return lock
}

func TestEditorProcessesShareDurableAcceptanceLock(t *testing.T) {
	cfg := isolatePlanStorage(t)
	workspace := t.TempDir()
	if _, err := WritePlan(workspace, "session", "original"); err != nil {
		t.Fatal(err)
	}
	lock := holdPlanWriterLock(t, workspace)
	var children []planWriterProcess
	for i := 0; i < 4; i++ {
		staged, cleanup, err := StageForEditor(workspace, "session")
		if err != nil {
			t.Fatal(err)
		}
		defer cleanup()
		if err := os.WriteFile(staged, []byte(fmt.Sprintf("edit %d", i)), 0600); err != nil {
			t.Fatal(err)
		}
		children = append(children, startPlanWriterProcess(t, workspace, staged, "edit", filepath.Join(filepath.Dir(cfg), "tmp")))
	}
	for _, child := range children {
		child.start()
	}
	select {
	case <-children[0].done:
		t.Fatal("editor replaced the durable plan while another process held its lock")
	case <-time.After(100 * time.Millisecond):
	}
	if err := lock.Release(); err != nil {
		t.Fatal(err)
	}
	accepted := 0
	for _, child := range children {
		if strings.Contains(waitPlanWriter(t, child), "accepted") {
			accepted++
		}
	}
	if accepted != 1 {
		t.Fatalf("accepted %d competing editors from one baseline, want 1", accepted)
	}
}

func TestDirectWriterAndEditorShareDurableAcceptanceLock(t *testing.T) {
	cfg := isolatePlanStorage(t)
	workspace := t.TempDir()
	if _, err := WritePlan(workspace, "session", "original"); err != nil {
		t.Fatal(err)
	}
	staged, cleanup, err := StageForEditor(workspace, "session")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if err := os.WriteFile(staged, []byte("stale editor"), 0600); err != nil {
		t.Fatal(err)
	}
	lock := holdPlanWriterLock(t, workspace)
	writer := startPlanWriterProcess(t, workspace, "", "write", filepath.Join(filepath.Dir(cfg), "tmp"))
	writer.start()
	select {
	case <-writer.done:
		t.Fatal("WritePlan replaced durable state while another process held its lock")
	case <-time.After(100 * time.Millisecond):
	}
	if err := lock.Release(); err != nil {
		t.Fatal(err)
	}
	if output := waitPlanWriter(t, writer); !strings.Contains(output, "accepted") {
		t.Fatalf("direct write failed: %s", output)
	}
	editor := startPlanWriterProcess(t, workspace, staged, "edit", filepath.Join(filepath.Dir(cfg), "tmp"))
	editor.start()
	if output := waitPlanWriter(t, editor); !strings.Contains(output, "rejected:") {
		t.Fatalf("stale editor accepted: %s", output)
	}
	got, _, err := ReadPlan(workspace, "session")
	if err != nil || got != "direct writer\n" {
		t.Fatalf("durable plan = %q, %v", got, err)
	}
}
