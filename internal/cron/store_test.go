package cron

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestStoreGetDistinguishesNotFoundFromReadError(t *testing.T) {
	s := newTestStore(t)

	// A genuinely absent job → ErrJobNotFound.
	if _, err := s.Get("job_missing"); !errors.Is(err, ErrJobNotFound) {
		t.Fatalf("missing job Get err = %v, want ErrJobNotFound", err)
	}

	// A job whose metadata.json can't be read (here it's a directory, so ReadFile
	// fails with a non-ENOENT error) must NOT be reported as not-found — otherwise
	// cron_run mislabels a transient IO failure as "job removed during run".
	id := "job_unreadable"
	if err := os.MkdirAll(filepath.Join(s.jobDir(id), "metadata.json"), 0o755); err != nil {
		t.Fatalf("mkdir metadata.json-as-dir: %v", err)
	}
	_, err := s.Get(id)
	if err == nil {
		t.Fatal("expected an error reading a directory as metadata.json")
	}
	if errors.Is(err, ErrJobNotFound) {
		t.Fatalf("a read error must not be reported as ErrJobNotFound: %v", err)
	}
}

func newTestStore(t *testing.T) *Store {
	t.Helper()
	return NewStore(StoreOptions{RootDir: t.TempDir(), Now: func() time.Time { return time.Unix(1000, 0).UTC() }})
}

func TestStoreAddListGetRemove(t *testing.T) {
	s := newTestStore(t)
	job := Job{Expr: "0 9 * * *", Prompt: "hi", Status: StatusActive, NextRunAt: time.Unix(2000, 0).UTC()}
	added, err := s.Add(job)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if added.ID == "" || added.CreatedAt.IsZero() {
		t.Fatalf("Add must assign ID + CreatedAt, got %+v", added)
	}
	list, err := s.List()
	if err != nil || len(list) != 1 || list[0].ID != added.ID {
		t.Fatalf("List=%v err=%v", list, err)
	}
	got, err := s.Get(added.ID)
	if err != nil || got.Prompt != "hi" || got.Expr != "0 9 * * *" {
		t.Fatalf("Get=%+v err=%v", got, err)
	}
	got.Status = StatusPaused
	if err := s.Update(got); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if reread, _ := s.Get(added.ID); reread.Status != StatusPaused {
		t.Fatalf("Update not persisted, status=%q", reread.Status)
	}
	if err := s.Remove(added.ID); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if list, _ := s.List(); len(list) != 0 {
		t.Fatalf("expected empty after remove, got %v", list)
	}
	if _, err := s.Get(added.ID); err == nil {
		t.Fatal("Get of removed id must error")
	}
}

func TestStoreAddReservesUniqueIDsAcrossConcurrentStores(t *testing.T) {
	root := t.TempDir()
	now := func() time.Time { return time.Unix(1000, 0).UTC() }
	const adds = 32

	start := make(chan struct{})
	results := make(chan Job, adds)
	errs := make(chan error, adds)
	var wg sync.WaitGroup
	for i := 0; i < adds; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			store := NewStore(StoreOptions{RootDir: root, Now: now})
			<-start
			job, err := store.Add(Job{Expr: "* * * * *", Prompt: fmt.Sprintf("job-%d", index)})
			if err != nil {
				errs <- err
				return
			}
			results <- job
		}(i)
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)

	for err := range errs {
		t.Errorf("concurrent Add: %v", err)
	}
	seen := make(map[string]struct{}, adds)
	reader := NewStore(StoreOptions{RootDir: root, Now: now})
	for job := range results {
		if _, duplicate := seen[job.ID]; duplicate {
			t.Errorf("concurrent Add returned duplicate ID %q", job.ID)
		}
		seen[job.ID] = struct{}{}
		persisted, err := reader.Get(job.ID)
		if err != nil {
			t.Errorf("Get(%q): %v", job.ID, err)
			continue
		}
		if persisted.Prompt != job.Prompt {
			t.Errorf("Get(%q).Prompt = %q, want %q", job.ID, persisted.Prompt, job.Prompt)
		}
	}
	if len(seen) != adds {
		t.Fatalf("unique jobs = %d, want %d", len(seen), adds)
	}
}

// TestAddDoesNotLeakReservationToConcurrentRemove reproduces the audit
// finding that Remove could observe and delete an in-flight Add's bare
// reservation directory (Mkdir'd but not yet holding metadata.json) and
// report success — after which Add's writeJob would silently resurrect the
// directory and persist the job anyway. Add now holds the same per-ID lock
// Remove takes, from reservation through initial persistence, so a
// concurrent Remove for that id must wait until the reservation is either
// committed or torn down, never observing the bare reservation in between.
func TestAddDoesNotLeakReservationToConcurrentRemove(t *testing.T) {
	store := newTestStore(t)

	// Simulate the in-flight window Add now holds the lock across: the id is
	// reserved (its directory exists) but metadata.json has not been written.
	id, unlock, err := store.reserveID()
	if err != nil {
		t.Fatalf("reserveID: %v", err)
	}
	if _, statErr := os.Stat(store.jobDir(id)); statErr != nil {
		t.Fatalf("reservation directory missing: %v", statErr)
	}

	removeErr := make(chan error, 1)
	go func() {
		removeErr <- store.Remove(id)
	}()

	// Remove cannot proceed past the shared per-ID lock we're holding, so it's
	// safe to commit the job here, before releasing it: if Remove could race
	// ahead of this write, it would delete the bare reservation instead of
	// waiting to observe (and remove) the committed job.
	job := Job{ID: id, Expr: "* * * * *", Prompt: "job", Status: StatusActive}
	if err := store.writeJob(job); err != nil {
		t.Fatalf("writeJob: %v", err)
	}
	unlock()

	if err := <-removeErr; err != nil {
		t.Fatalf("Remove of the committed job: %v", err)
	}
	if _, err := os.Stat(store.jobDir(id)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("job directory should be gone after Remove: %v", err)
	}
	if _, err := store.Get(id); !errors.Is(err, ErrJobNotFound) {
		t.Fatalf("Get after Remove = %v, want ErrJobNotFound", err)
	}
}

// TestReserveIDSerializesOnPerIDLock covers the other half of the audit
// finding: a second Add attempting the SAME candidate id while the first
// reservation is still held must block on the shared per-ID lock (not just
// race the directory Mkdir), so it never ends up writing metadata.json for
// an id whose first writer is still in flight. It moves on to the next
// candidate id instead.
func TestReserveIDSerializesOnPerIDLock(t *testing.T) {
	store := newTestStore(t)

	id, unlock, err := store.reserveID()
	if err != nil {
		t.Fatalf("first reserveID: %v", err)
	}

	type reservation struct {
		id  string
		err error
	}
	second := make(chan reservation, 1)
	go func() {
		id2, unlock2, err := store.reserveID()
		if err == nil {
			unlock2()
		}
		second <- reservation{id: id2, err: err}
	}()

	// The second reserveID cannot observe an id until this lock is released:
	// it contends on the same per-ID lock file before ever attempting Mkdir.
	unlock()

	got := <-second
	if got.err != nil {
		t.Fatalf("second reserveID: %v", got.err)
	}
	if got.id == id {
		t.Fatalf("second reserveID returned the first id %q while its reservation was still held", got.id)
	}
}

func TestWriteReservedJobRemovesReservationOnFailure(t *testing.T) {
	store := newTestStore(t)
	job := Job{ID: "reserved", Expr: "* * * * *", Prompt: "job"}
	if err := os.MkdirAll(filepath.Join(store.jobDir(job.ID), "metadata.json.tmp"), 0o700); err != nil {
		t.Fatal(err)
	}

	if err := store.writeReservedJob(job); err == nil {
		t.Fatal("writeReservedJob error = nil, want metadata write failure")
	}
	if _, err := os.Stat(store.jobDir(job.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed reservation still exists: %v", err)
	}
}

func TestStoreAppendRun(t *testing.T) {
	s := newTestStore(t)
	job, _ := s.Add(Job{Expr: "* * * * *", Prompt: "x", Status: StatusActive})
	for i := 0; i < 3; i++ {
		if err := s.AppendRun(job.ID, RunRecord{JobID: job.ID, At: time.Unix(int64(i), 0).UTC(), ExitCode: i}); err != nil {
			t.Fatalf("AppendRun: %v", err)
		}
	}
	runs, err := s.Runs(job.ID, 0)
	if err != nil || len(runs) != 3 || runs[2].ExitCode != 2 {
		t.Fatalf("Runs=%v err=%v", runs, err)
	}
}

func TestDefaultRootHonorsXDG(t *testing.T) {
	root := DefaultRoot(map[string]string{"XDG_DATA_HOME": "/tmp/xdg"})
	if root != filepath.Join("/tmp/xdg", "zero", "cron") {
		t.Fatalf("DefaultRoot=%q", root)
	}
}

func TestDefaultRootEmptyHomeFallsBackToUserHome(t *testing.T) {
	// No XDG_DATA_HOME and no HOME: must NOT produce a relative ".local/share"
	// under the caller's cwd (the bug). It falls back to the OS user home.
	root := DefaultRoot(map[string]string{})
	if !filepath.IsAbs(root) {
		t.Fatalf("DefaultRoot with empty env must be absolute, got %q", root)
	}
	if strings.HasPrefix(root, ".local") || strings.HasPrefix(root, filepath.Join(".local", "share")) {
		t.Fatalf("DefaultRoot leaked a relative .local/share path: %q", root)
	}
	if filepath.Base(root) != "cron" || filepath.Base(filepath.Dir(root)) != "zero" {
		t.Fatalf("DefaultRoot tail = %q, want .../zero/cron", root)
	}
}

func TestStoreRejectsUnsafeID(t *testing.T) {
	root := t.TempDir()
	s := NewStore(StoreOptions{RootDir: root, Now: func() time.Time { return time.Unix(0, 0).UTC() }})
	sibling := filepath.Join(filepath.Dir(root), "victim")
	if err := os.MkdirAll(sibling, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"../victim", "a/b", "..", ""} {
		if err := s.Remove(id); err == nil {
			t.Fatalf("Remove(%q) must be rejected", id)
		}
		if _, err := s.Get(id); err == nil {
			t.Fatalf("Get(%q) must be rejected", id)
		}
		if _, err := s.Runs(id, 0); err == nil {
			t.Fatalf("Runs(%q) must be rejected", id)
		}
	}
	if _, err := os.Stat(sibling); err != nil {
		t.Fatalf("traversal must not delete a sibling directory: %v", err)
	}
}

func TestListSurfacesCorruptJob(t *testing.T) {
	s := newTestStore(t)
	good, _ := s.Add(Job{Expr: "0 9 * * *", Prompt: "ok", Status: StatusActive})
	bad, _ := s.Add(Job{Expr: "0 9 * * *", Prompt: "bad", Status: StatusActive})
	if err := os.WriteFile(filepath.Join(s.jobDir(bad.ID), "metadata.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	jobs, err := s.List()
	if err == nil {
		t.Fatal("List should surface a corrupt job via a non-nil (warning) error")
	}
	if len(jobs) != 1 || jobs[0].ID != good.ID {
		t.Fatalf("good job must still list despite a corrupt sibling, got %+v", jobs)
	}
}
