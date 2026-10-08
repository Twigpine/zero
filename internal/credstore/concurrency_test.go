package credstore

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func fileStore(t *testing.T, dir string) *Store {
	t.Helper()
	store, err := New(Options{Dir: dir, Storage: "file"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return store
}

// THE REPRODUCTION, kept as the regression. Before the lock this reliably kept
// 1 of 100: every writer read the same map, added its own provider, and the
// last rename published a file missing all the others.
func TestConcurrentSetKeepsEveryKey(t *testing.T) {
	dir := t.TempDir()
	store := fileStore(t, dir)

	const n = 100
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := store.Set(fmt.Sprintf("p%03d", i), fmt.Sprintf("k%03d", i)); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("Set: %v", err)
	}

	got, err := store.Providers()
	if err != nil {
		t.Fatalf("Providers: %v", err)
	}
	if len(got) != n {
		t.Fatalf("kept %d of %d concurrent writes", len(got), n)
	}
	// ...and every value is the one its own writer wrote, not a neighbour's.
	for i := 0; i < n; i++ {
		key, ok, err := store.Get(fmt.Sprintf("p%03d", i))
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if !ok || key != fmt.Sprintf("k%03d", i) {
			t.Fatalf("p%03d = %q (present=%v); a writer's own value must survive", i, key, ok)
		}
	}
}

// DELETE RACES SET, and an unlocked delete loses the other writer's key.
//
// A first version of this only checked that keys nobody deleted survived — and
// every reader sees those, so it passed with the lock removed from Delete. The
// case that actually breaks is a Delete whose stale read predates a concurrent
// Set: it writes back a map that never contained the new key, and the Set is
// gone. So the assertion is on the SETS surviving, not on the bystanders.
func TestADeleteCannotClobberAConcurrentSet(t *testing.T) {
	dir := t.TempDir()
	store := fileStore(t, dir)

	// Throwaway keys for the deleters to remove, written first so a delete
	// always has something to do.
	const churn = 40
	for i := 0; i < churn; i++ {
		if err := store.Set(fmt.Sprintf("churn%02d", i), "v"); err != nil {
			t.Fatal(err)
		}
	}

	const adds = 60
	var wg sync.WaitGroup
	for i := 0; i < adds; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = store.Set(fmt.Sprintf("added%02d", i), "v")
		}(i)
	}
	for i := 0; i < churn; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _ = store.Delete(fmt.Sprintf("churn%02d", i))
		}(i)
	}
	wg.Wait()

	got, err := store.Providers()
	if err != nil {
		t.Fatal(err)
	}
	survivors := map[string]bool{}
	for _, name := range got {
		survivors[name] = true
	}
	missing := []string{}
	for i := 0; i < adds; i++ {
		name := fmt.Sprintf("added%02d", i)
		if !survivors[name] {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("%d of %d concurrent Set calls were clobbered by a racing Delete: %v",
			len(missing), adds, missing)
	}
	// ...and every churn key really was removed, so the deletes were not
	// no-ops that would make the check above vacuous.
	for i := 0; i < churn; i++ {
		if survivors[fmt.Sprintf("churn%02d", i)] {
			t.Fatalf("churn%02d survived; the deletes did nothing and this test proves nothing", i)
		}
	}
}

// ACROSS PROCESSES, which is the case that matters: a plan running with
// max_workers > 1 spawns child PROCESSES, and an in-process mutex would not
// have helped any of them. The race detector cannot see this one either — it is
// two OS processes — so it is driven for real.
func TestConcurrentSetAcrossProcesses(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns processes")
	}
	helper := os.Getenv("ZERO_CREDSTORE_HELPER_DIR")
	if helper != "" {
		// Child mode: write our slice of the keys and exit.
		store, err := New(Options{Dir: helper, Storage: "file"})
		if err != nil {
			os.Exit(3)
		}
		prefix := os.Getenv("ZERO_CREDSTORE_HELPER_PREFIX")
		for i := 0; i < 25; i++ {
			if err := store.Set(fmt.Sprintf("%s%02d", prefix, i), "v"); err != nil {
				os.Exit(4)
			}
		}
		os.Exit(0)
	}

	dir := t.TempDir()
	const children = 4
	var wg sync.WaitGroup
	failures := make(chan string, children)
	for c := 0; c < children; c++ {
		wg.Add(1)
		go func(c int) {
			defer wg.Done()
			cmd := exec.Command(os.Args[0], "-test.run=TestConcurrentSetAcrossProcesses", "-test.v")
			cmd.Env = append(os.Environ(),
				"ZERO_CREDSTORE_HELPER_DIR="+dir,
				fmt.Sprintf("ZERO_CREDSTORE_HELPER_PREFIX=c%d_", c),
			)
			if out, err := cmd.CombinedOutput(); err != nil {
				failures <- fmt.Sprintf("child %d: %v\n%s", c, err, out)
			}
		}(c)
	}
	wg.Wait()
	close(failures)
	for failure := range failures {
		t.Fatal(failure)
	}

	store := fileStore(t, dir)
	got, err := store.Providers()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != children*25 {
		t.Fatalf("kept %d of %d keys written by %d concurrent processes", len(got), children*25, children)
	}
}

// The lock lives BESIDE the data file, never on it. write publishes by rename,
// so a lock taken on the data file would be attached to an inode the next
// writer has already replaced — every writer would appear to hold it.
func TestTheLockIsNotTheDataFile(t *testing.T) {
	dir := t.TempDir()
	store := fileStore(t, dir)
	if store.lockPath() == store.file {
		t.Fatal("the lock is the data file; rename would carry it away")
	}
	if !strings.HasPrefix(filepath.Base(store.lockPath()), filepath.Base(store.file)) {
		t.Fatalf("lock %q is not beside the data file %q", store.lockPath(), store.file)
	}
	if err := store.Set("p", "k"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(store.lockPath()); err != nil {
		t.Fatalf("the lock file was not created: %v", err)
	}
	// ...and it survives a write, since the rename replaces only the data file.
	if err := store.Set("q", "k"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(store.lockPath()); err != nil {
		t.Fatalf("the lock file did not survive a write: %v", err)
	}
}

// The lock is a security boundary, so the path where it CANNOT be taken needs
// coverage too: an operation that could not lock must report that, never proceed
// unserialized and never report success. Replacing the credential directory with
// a regular file makes the lock's MkdirAll fail on every platform.
func TestOperationsFailWhenTheLockCannotBeAcquired(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "creds")
	store := fileStore(t, dir)
	// Where the store expects its directory, put a regular file.
	if err := os.WriteFile(dir, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := store.Set("openai", "sk-test"); err == nil {
		t.Error("Set reported success although the lock could not be acquired")
	}
	if _, err := store.Delete("openai"); err == nil {
		t.Error("Delete reported success although the lock could not be acquired")
	}
	if _, _, err := store.Get("openai"); err == nil {
		t.Error("Get reported success although the lock could not be acquired")
	}
	if _, err := store.Providers(); err == nil {
		t.Error("Providers reported success although the lock could not be acquired")
	}
	// And nothing may have been written through the failed path.
	if raw, err := os.ReadFile(dir); err != nil || string(raw) != "not a directory" {
		t.Errorf("the blocking file was modified: %q (err %v)", raw, err)
	}
}
