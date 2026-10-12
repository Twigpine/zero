package cli

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Gitlawb/zero/internal/cron"
)

func TestFireJobClaimPreventsDoubleFire(t *testing.T) {
	now := time.Date(2026, 6, 9, 9, 0, 0, 0, time.UTC)
	nowFn := func() time.Time { return now }
	store := cron.NewStore(cron.StoreOptions{RootDir: t.TempDir(), Now: nowFn})
	due, _ := store.Add(cron.Job{Expr: "0 9 * * *", Prompt: "fire me", Status: cron.StatusActive, NextRunAt: now.Add(-time.Minute)})

	fx := &fakeExec{}
	// Two schedulers fire the same due job concurrently; the atomic claim must let
	// exactly one through, so the job never double-fires (M9).
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			job, _ := store.Get(due.ID)
			var out, errb bytes.Buffer
			fireJob(store, nowFn, job, &out, &errb, fx.run)
		}()
	}
	wg.Wait()

	if len(fx.calls) != 1 {
		t.Fatalf("claim must let exactly one scheduler fire the due job, got %d", len(fx.calls))
	}
	// And NextRunAt is advanced to the next slot (tomorrow 09:00), not left due.
	d, _ := store.Get(due.ID)
	if !d.NextRunAt.After(now) {
		t.Fatalf("NextRunAt must advance past now, got %s", d.NextRunAt)
	}
}

func TestFireJobOverlappingAdjacentSlots(t *testing.T) {
	for _, olderFirst := range []bool{false, true} {
		name := "newer completes first"
		if olderFirst {
			name = "older completes first"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			stores := []*cron.Store{
				cron.NewStore(cron.StoreOptions{RootDir: root}),
				cron.NewStore(cron.StoreOptions{RootDir: root}),
			}
			now := time.Date(2026, 6, 9, 9, 0, 0, 0, time.UTC)
			job, err := stores[0].Add(cron.Job{Expr: "* * * * *", Prompt: "fire me", NextRunAt: now, FireCount: 7})
			if err != nil {
				t.Fatal(err)
			}
			var release, done [2]chan struct{}
			for i := range stores {
				release[i], done[i] = make(chan struct{}), make(chan struct{})
				// Always release blocked runners, including on assertion failures.
				defer func() {
					select {
					case <-release[i]:
					default:
						close(release[i])
					}
					<-done[i]
				}()
				started := make(chan struct{})
				go func() {
					defer close(done[i])
					fireJob(stores[i], func() time.Time { return now.Add(time.Duration(i) * time.Minute) }, job, io.Discard, io.Discard,
						func(_ []string, _, _ io.Writer) int {
							close(started)
							<-release[i]
							return i // Include a failed execution in the accounting.
						})
				}()
				select {
				case <-started:
				case <-done[i]:
					t.Fatal("runner did not claim its slot")
				}
			}
			order := []int{1, 0}
			if olderFirst {
				order = []int{0, 1}
			}
			for n, i := range order {
				close(release[i])
				<-done[i]
				current, err := stores[0].Get(job.ID)
				if err != nil {
					t.Fatal(err)
				}
				if current.FireCount != 8+n || !current.NextRunAt.Equal(now.Add(2*time.Minute)) {
					t.Fatalf("after completion %d: FireCount=%d NextRunAt=%s; want count %d and next %s", n+1, current.FireCount, current.NextRunAt, 8+n, now.Add(2*time.Minute))
				}
				claimed, err := claimFire(stores[1], now.Add(time.Minute), job.ID)
				if err != nil || claimed {
					t.Fatalf("already claimed slot became available: claimed=%v err=%v", claimed, err)
				}
			}
			runs, err := stores[0].Runs(job.ID, 0)
			if err != nil || len(runs) != 2 {
				t.Fatalf("runs=%v err=%v", runs, err)
			}
			for n, i := range order {
				if !runs[n].At.Equal(now.Add(time.Duration(i)*time.Minute)) || runs[n].ExitCode != i {
					t.Fatalf("unexpected run record: %+v", runs[n])
				}
			}
		})
	}
}

func TestFireJobCompletionReadErrorDoesNotOverwriteState(t *testing.T) {
	root := t.TempDir()
	store := cron.NewStore(cron.StoreOptions{RootDir: root})
	now := time.Date(2026, 6, 9, 9, 0, 0, 0, time.UTC)
	job, err := store.Add(cron.Job{Expr: "* * * * *", Prompt: "fire me", NextRunAt: now})
	if err != nil {
		t.Fatal(err)
	}
	metadata := filepath.Join(root, job.ID, "metadata.json")
	var stdout, stderr bytes.Buffer
	fireJob(store, func() time.Time { return now }, job, &stdout, &stderr,
		func(_ []string, _, _ io.Writer) int {
			// A corrupt read must not cause completion to replace unknown newer state
			// with the stale pre-execution job. This works without permission tricks.
			if err := os.WriteFile(metadata, []byte("unreadable job"), 0o600); err != nil {
				t.Fatal(err)
			}
			return 0
		})
	data, err := os.ReadFile(metadata)
	if err != nil || string(data) != "unreadable job" {
		t.Fatalf("completion overwrote unreadable state: %q, err=%v", data, err)
	}
	if !strings.Contains(stderr.String(), "failed to persist job state") {
		t.Fatalf("missing persistence warning: %s", stderr.String())
	}
	if want := "fired " + job.ID + " -> exit 0 (next: unknown)\n"; stdout.String() != want {
		t.Fatalf("completion summary = %q; want %q", stdout.String(), want)
	}
	runs, err := store.Runs(job.ID, 0)
	if err != nil || len(runs) != 1 || runs[0].ExitCode != 0 {
		t.Fatalf("execution should still be recorded: runs=%v err=%v", runs, err)
	}
}
