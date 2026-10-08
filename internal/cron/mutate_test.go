package cron

import (
	"errors"
	"sync"
	"testing"
)

// Mutate must serialize the read-modify-write across concurrent callers via the
// cross-process lock: N concurrent FireCount++ must all land (final == N). Without
// the lock, racing read-modify-writes would lose updates (final < N).
func TestMutateIsAtomicUnderConcurrency(t *testing.T) {
	store := NewStore(StoreOptions{RootDir: t.TempDir()})
	job, err := store.Add(Job{Expr: "* * * * *", Prompt: "x"})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	const goroutines = 20
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, merr := store.Mutate(job.ID, func(current Job, readErr error) (Job, error) {
				if readErr != nil {
					return Job{}, readErr
				}
				current.FireCount++
				return current, nil
			})
			if merr != nil {
				t.Errorf("Mutate: %v", merr)
			}
		}()
	}
	close(start)
	wg.Wait()

	got, err := store.Get(job.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.FireCount != goroutines {
		t.Fatalf("FireCount = %d, want %d (lost updates → the lock is not serializing the read-modify-write)", got.FireCount, goroutines)
	}
}

// A job removed between fire and persist must abort Mutate (no resurrection).
func TestMutateRemovedJobReturnsNotFound(t *testing.T) {
	store := NewStore(StoreOptions{RootDir: t.TempDir()})
	job, err := store.Add(Job{Expr: "* * * * *", Prompt: "x"})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := store.Remove(job.ID); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	_, err = store.Mutate(job.ID, func(current Job, readErr error) (Job, error) {
		return current, nil
	})
	if !errors.Is(err, ErrJobNotFound) {
		t.Fatalf("Mutate on removed job = %v, want ErrJobNotFound", err)
	}
	// The stable advisory-lock file does not resurrect the removed job dir.
	if _, gerr := store.Get(job.ID); !errors.Is(gerr, ErrJobNotFound) {
		t.Fatalf("job should remain removed, Get = %v", gerr)
	}
}
