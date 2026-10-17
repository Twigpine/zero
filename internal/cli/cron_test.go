package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/Twigpine/zero/internal/cron"
)

func testCronStore(t *testing.T) *cron.Store {
	t.Helper()
	return cron.NewStore(cron.StoreOptions{RootDir: t.TempDir(), Now: func() time.Time { return time.Date(2026, 6, 9, 8, 0, 0, 0, time.UTC) }})
}

func TestCronAddListRemove(t *testing.T) {
	store := testCronStore(t)
	var out, errb bytes.Buffer
	now := func() time.Time { return time.Date(2026, 6, 9, 8, 0, 0, 0, time.UTC) }

	if code := runCronWith(store, now, []string{"add", "0 9 * * *", "--prompt", "daily"}, &out, &errb); code != 0 {
		t.Fatalf("add exit=%d err=%s", code, errb.String())
	}
	jobs, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(jobs) != 1 || jobs[0].Expr != "0 9 * * *" || jobs[0].Prompt != "daily" {
		t.Fatalf("job not stored: %+v", jobs)
	}
	if jobs[0].NextRunAt.IsZero() {
		t.Fatal("NextRunAt should be set on add")
	}

	out.Reset()
	if code := runCronWith(store, now, []string{"list"}, &out, &errb); code != 0 {
		t.Fatalf("list exit=%d", code)
	}
	if !strings.Contains(out.String(), jobs[0].ID) || !strings.Contains(out.String(), "0 9 * * *") {
		t.Fatalf("list output missing job:\n%s", out.String())
	}

	if code := runCronWith(store, now, []string{"pause", jobs[0].ID}, &out, &errb); code != 0 {
		t.Fatalf("pause exit=%d", code)
	}
	j, err := store.Get(jobs[0].ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if j.Status != cron.StatusPaused {
		t.Fatalf("pause did not persist, status=%q", j.Status)
	}

	if code := runCronWith(store, now, []string{"rm", jobs[0].ID}, &out, &errb); code != 0 {
		t.Fatalf("rm exit=%d", code)
	}
	jobs, err = store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(jobs) != 0 {
		t.Fatalf("expected removed, got %v", jobs)
	}
}

func TestCronAddRejectsBadExpr(t *testing.T) {
	store := testCronStore(t)
	var out, errb bytes.Buffer
	now := func() time.Time { return time.Now() }
	if code := runCronWith(store, now, []string{"add", "99 * * * *", "--prompt", "x"}, &out, &errb); code == 0 {
		t.Fatal("expected non-zero exit for invalid cron expr")
	}
	if !strings.Contains(errb.String(), "minute") {
		t.Fatalf("error should name the bad field, got %q", errb.String())
	}
}

func TestCronAddRecipe(t *testing.T) {
	store := testCronStore(t)
	var out, errb bytes.Buffer
	now := func() time.Time { return time.Date(2026, 6, 9, 8, 0, 0, 0, time.UTC) }
	if code := runCronWith(store, now, []string{"add", "--recipe", "git-recap"}, &out, &errb); code != 0 {
		t.Fatalf("add --recipe exit=%d err=%s", code, errb.String())
	}
	jobs, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(jobs) != 1 || jobs[0].Expr != "*/30 * * * *" {
		t.Fatalf("recipe job not stored: %+v", jobs)
	}
}

func TestCronAddExplicitExprOverridesRecipe(t *testing.T) {
	store := testCronStore(t)
	var out, errb bytes.Buffer
	now := func() time.Time { return time.Date(2026, 6, 9, 8, 0, 0, 0, time.UTC) }
	// An explicit positional schedule must win over the recipe's default expr
	// instead of being silently dropped.
	if code := runCronWith(store, now, []string{"add", "0 9 * * *", "--recipe", "git-recap"}, &out, &errb); code != 0 {
		t.Fatalf("add exit=%d err=%s", code, errb.String())
	}
	jobs, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(jobs) != 1 || jobs[0].Expr != "0 9 * * *" {
		t.Fatalf("explicit expr should win over recipe, got: %+v", jobs)
	}
	// The recipe's prompt must actually propagate — assert the exact recipe text,
	// not just non-empty (which a default prompt would also satisfy).
	recipe, ok := cron.Recipe("git-recap")
	if !ok {
		t.Fatal("git-recap recipe missing")
	}
	if jobs[0].Prompt != recipe.Prompt {
		t.Fatalf("recipe prompt not propagated: got %q want %q", jobs[0].Prompt, recipe.Prompt)
	}
}

func TestCronAddRejectsExtraArgs(t *testing.T) {
	store := testCronStore(t)
	now := func() time.Time { return time.Date(2026, 6, 9, 8, 0, 0, 0, time.UTC) }
	var out, errb bytes.Buffer
	if code := runCronWith(store, now, []string{"add", "0 9 * * *", "extra", "--prompt", "x"}, &out, &errb); code == 0 {
		t.Fatal("expected non-zero exit for extra positional args")
	}
	jobs, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(jobs) != 0 {
		t.Fatalf("no job should be stored on a usage error, got %v", jobs)
	}
}

func TestCronResumeRejectsImpossibleSchedule(t *testing.T) {
	store := testCronStore(t)
	now := func() time.Time { return time.Date(2026, 6, 9, 8, 0, 0, 0, time.UTC) }
	job, err := store.Add(cron.Job{Expr: "0 0 30 2 *", Prompt: "x", Status: cron.StatusPaused})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	var out, errb bytes.Buffer
	if code := runCronWith(store, now, []string{"resume", job.ID}, &out, &errb); code == 0 {
		t.Fatal("resume of an impossible schedule must be rejected")
	}
	j, err := store.Get(job.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if j.Status != cron.StatusPaused {
		t.Fatalf("job must remain paused after a rejected resume, got %q", j.Status)
	}
}
