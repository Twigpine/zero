package cron

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/Gitlawb/zero/internal/fsutil"
)

func TestAppendRunRetainsNewestThousand(t *testing.T) {
	store := newTestStore(t)
	job, err := store.Add(Job{Expr: "* * * * *", Prompt: "x"})
	if err != nil {
		t.Fatal(err)
	}
	// Seed an oversized legacy log, then cross the boundary again after compaction.
	var history bytes.Buffer
	for i := 0; i < 1207; i++ {
		if err := json.NewEncoder(&history).Encode(RunRecord{ExitCode: i}); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(store.jobDir(job.ID), "runs.jsonl")
	if err := os.WriteFile(path, history.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, next := range []int{1207, 1208} {
		if err := store.AppendRun(job.ID, RunRecord{ExitCode: next}); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		lines := bytes.Split(bytes.TrimSpace(data), []byte("\n"))
		if len(lines) != 1000 {
			t.Fatalf("retained %d records, want 1000", len(lines))
		}
		for i, line := range lines {
			var rec RunRecord
			if err := json.Unmarshal(line, &rec); err != nil {
				t.Fatal(err)
			}
			if rec.ExitCode != next-999+i {
				t.Fatalf("record %d = %d, want %d", i, rec.ExitCode, next-999+i)
			}
		}
	}
}

// AppendRun on a job removed mid-run must NOT resurrect its directory (which the
// old unconditional MkdirAll did, leaving an orphaned runs.jsonl with no
// metadata.json).
func TestAppendRunDoesNotResurrectRemovedJob(t *testing.T) {
	store := NewStore(StoreOptions{RootDir: t.TempDir()})
	job, err := store.Add(Job{Expr: "* * * * *", Prompt: "x"})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := store.Remove(job.ID); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	if err := store.AppendRun(job.ID, RunRecord{JobID: job.ID, At: store.now()}); err != nil {
		t.Fatalf("AppendRun after Remove should be a no-op, got: %v", err)
	}
	if _, err := os.Stat(store.jobDir(job.ID)); !os.IsNotExist(err) {
		t.Fatalf("AppendRun resurrected the removed job directory (stat err = %v)", err)
	}
	runs, err := store.Runs(job.ID, 0)
	if err != nil {
		t.Fatalf("Runs: %v", err)
	}
	if len(runs) != 0 {
		t.Fatalf("expected no runs for a removed job, got %d", len(runs))
	}
}

func TestRunsTail(t *testing.T) {
	store := newTestStore(t)
	job, err := store.Add(Job{Prompt: "x"})
	if err != nil {
		t.Fatal(err)
	}
	var history bytes.Buffer
	// A forward scan would fail on this old oversized line. A recent-window
	// reader must stop before reaching it, rather than merely cap its output.
	history.WriteString(strings.Repeat("x", 1024*1024+1) + "\n")
	for i := 0; i < 1103; i++ {
		if err := json.NewEncoder(&history).Encode(RunRecord{ExitCode: i, SessionTitle: strings.Repeat("界", 1500)}); err != nil {
			t.Fatal(err)
		}
	}
	history.WriteString("bad json\n\n{\"exitCode\":1103}") // no final newline
	path := filepath.Join(store.jobDir(job.ID), "runs.jsonl")
	if err := os.WriteFile(path, history.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, limit := range []int{1, 7, 999, 1000, 1001, 0, -1} {
		runs, err := store.Runs(job.ID, limit)
		want := limit
		if want <= 0 || want > 1000 {
			want = 1000
		}
		if err != nil || len(runs) != want {
			t.Fatalf("limit %d: got %d records, err %v", limit, len(runs), err)
		}
		for i, run := range runs {
			if run.ExitCode != 1104-want+i {
				t.Fatalf("limit %d, record %d = %d", limit, i, run.ExitCode)
			}
			if run.ExitCode != 1103 && run.SessionTitle != strings.Repeat("界", 1500) {
				t.Fatal("record spanning read blocks was corrupted")
			}
		}
	}
}

func TestAppendRunFailurePreservesHistory(t *testing.T) {
	store := newTestStore(t)
	job, err := store.Add(Job{Prompt: "x"})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(store.jobDir(job.ID), "runs.jsonl")
	for _, data := range []string{"{\"exitCode\":7}\n", strings.Repeat("x", 1024*1024)} {
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		rec := RunRecord{ExitCode: 8, Error: strings.Repeat("x", 1024*1024)}
		if err := store.AppendRun(job.ID, rec); err == nil {
			t.Fatal("expected oversized new record error")
		}
		got, err := os.ReadFile(path)
		if err != nil || string(got) != data {
			t.Fatalf("failed append changed history: %v", err)
		}
	}
}

func TestAppendRunWithOversizedHistory(t *testing.T) {
	store := newTestStore(t)
	job, err := store.Add(Job{Prompt: "x"})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(store.jobDir(job.ID), "runs.jsonl")
	// An oversized unterminated legacy record must not block future outcomes.
	if err := os.WriteFile(path, []byte(strings.Repeat("x", maxRunBytes)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendRun(job.ID, RunRecord{ExitCode: 42}); err != nil {
		t.Fatal(err)
	}
	runs, err := store.Runs(job.ID, 1)
	if err != nil || len(runs) != 1 || runs[0].ExitCode != 42 {
		t.Fatalf("new outcome lost: %+v, %v", runs, err)
	}
}

func TestRunsSkipOversizedLines(t *testing.T) {
	store := newTestStore(t)
	job, err := store.Add(Job{Prompt: "x"})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(store.jobDir(job.ID), "runs.jsonl")
	for _, size := range []int{maxRunBytes - 1, maxRunBytes, maxRunBytes + 4096} {
		// Valid JSON plus trailing whitespace catches accidental decoding of a
		// prefix after dropping only the oversized suffix. The exact boundary
		// also distinguishes an oversized line from the largest allowed line.
		line := `{"exitCode":99}`
		line += strings.Repeat(" ", size-len(line))
		data := line + "\n{\"exitCode\":7}\n" + line + "\n{\"exitCode\":42}\n" + line
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		runs, err := store.Runs(job.ID, 10)
		want := []int{7, 42}
		if size < maxRunBytes {
			want = []int{99, 7, 99, 42, 99}
		}
		if err != nil || len(runs) != len(want) {
			t.Fatalf("size %d: got %+v, err %v; want %v", size, runs, err, want)
		}
		for i, rec := range runs {
			if rec.ExitCode != want[i] {
				t.Fatalf("size %d: record %d = %d, want %d", size, i, rec.ExitCode, want[i])
			}
		}
	}
}

// An outside reader holding runs.jsonl open makes the Windows replace fail;
// the outcome must still be recorded rather than dropped.
func TestAppendRunWhileHistoryHeldOpen(t *testing.T) {
	store := newTestStore(t)
	job, err := store.Add(Job{Prompt: "x"})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(store.jobDir(job.ID), "runs.jsonl")
	if err := os.WriteFile(path, []byte("{\"exitCode\":7}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	held, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	if err := store.AppendRun(job.ID, RunRecord{ExitCode: 42}); err != nil {
		t.Fatalf("AppendRun with history held open: %v", err)
	}
	runs, err := store.Runs(job.ID, 10)
	if err != nil || len(runs) != 2 || runs[0].ExitCode != 7 || runs[1].ExitCode != 42 {
		t.Fatalf("outcome lost while history held open: %+v, %v", runs, err)
	}
}

// Only a sharing or lock violation falls back to appending. Any other replace
// failure, including a persistent access denial, and a committed replacement
// whose backup cleanup failed must leave the log untouched, so history can
// neither grow past retention nor record an outcome twice.
func TestAppendRunFallbackOnlyForSharingViolation(t *testing.T) {
	const seed = "{\"exitCode\":7}\n"
	for _, tc := range []struct {
		name   string
		err    error
		append bool
	}{
		{"sharing violation", syscall.Errno(32), runtime.GOOS == "windows"},
		{"access denied", os.ErrPermission, false},
		{"other", errors.New("boom"), false},
		{"committed", &fsutil.CommittedReplacementCleanupError{Cause: syscall.Errno(32)}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newTestStore(t)
			job, err := store.Add(Job{Prompt: "x"})
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(store.jobDir(job.ID), "runs.jsonl")
			if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
				t.Fatal(err)
			}
			store.replace = func(string, string) error { return tc.err }
			err = store.AppendRun(job.ID, RunRecord{ExitCode: 42})
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if tc.append {
				if err != nil || !strings.HasPrefix(string(data), seed) || !strings.Contains(string(data), "\"exitCode\":42") {
					t.Fatalf("sharing violation dropped the outcome: err %v, log %q", err, data)
				}
				return
			}
			if !errors.Is(err, tc.err) {
				t.Fatalf("AppendRun error = %v, want %v", err, tc.err)
			}
			if string(data) != seed {
				t.Fatalf("non-transient replace failure changed the log: %q", data)
			}
		})
	}
}

// The fallback append must start a fresh line even when the log ends in an
// unterminated record, so neither record is corrupted.
func TestAppendRunLineAfterUnterminatedTail(t *testing.T) {
	store := newTestStore(t)
	job, err := store.Add(Job{Prompt: "x"})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(store.jobDir(job.ID), "runs.jsonl")
	if err := os.WriteFile(path, []byte("{\"exitCode\":7}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := appendRunLine(path, []byte("{\"exitCode\":42}")); err != nil {
		t.Fatal(err)
	}
	runs, err := store.Runs(job.ID, 10)
	if err != nil || len(runs) != 2 || runs[0].ExitCode != 7 || runs[1].ExitCode != 42 {
		t.Fatalf("fallback append corrupted history: %+v, %v", runs, err)
	}
}

func TestRunHistoryConcurrentStores(t *testing.T) {
	store := newTestStore(t)
	job, err := store.Add(Job{Prompt: "x"})
	if err != nil {
		t.Fatal(err)
	}
	var history bytes.Buffer
	for i := 0; i < 1000; i++ {
		if err := json.NewEncoder(&history).Encode(RunRecord{ExitCode: -1}); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(store.jobDir(job.ID), "runs.jsonl"), history.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Go(func() {
			other := NewStore(StoreOptions{RootDir: store.root})
			if err := other.AppendRun(job.ID, RunRecord{ExitCode: i}); err != nil {
				t.Error(err)
				return
			}
			runs, err := other.Runs(job.ID, 1000)
			if err != nil || len(runs) != 1000 {
				t.Errorf("inconsistent history: %d records, err %v", len(runs), err)
			}
		})
	}
	wg.Wait()
	runs, err := store.Runs(job.ID, 12)
	if err != nil || len(runs) != 12 {
		t.Fatalf("final tail: %d records, err %v", len(runs), err)
	}
	seen := make(map[int]bool)
	for _, run := range runs {
		if run.ExitCode < 0 || run.ExitCode >= 12 || seen[run.ExitCode] {
			t.Fatalf("lost or duplicate concurrent append: %+v", runs)
		}
		seen[run.ExitCode] = true
	}
}
