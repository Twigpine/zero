package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/Gitlawb/zero/internal/cron"
	"github.com/Gitlawb/zero/internal/streamjson"
)

// execRunner runs a `zero exec ...` invocation and returns its exit code. The
// default is cli.Run; tests inject a fake.
type execRunner func(args []string, stdout, stderr io.Writer) int

// cronRun implements `zero cron run [--once] [--catch-up] [id...]`.
func cronRun(store *cron.Store, now func() time.Time, args []string, stdout io.Writer, stderr io.Writer, exec execRunner) int {
	once, catchUp := false, false
	var ids []string
	for _, a := range args {
		switch {
		case a == "--once":
			once = true
		case a == "--catch-up":
			catchUp = true
		case strings.HasPrefix(a, "-"):
			fmt.Fprintf(stderr, "Unknown cron run flag: %s\n", a)
			return exitUsage
		default:
			ids = append(ids, a)
		}
	}

	selected := func(j cron.Job) bool {
		if j.Status != cron.StatusActive {
			return false
		}
		if len(ids) == 0 {
			return true
		}
		return contains(ids, j.ID)
	}

	fireDue := func() {
		jobs, err := store.List()
		if err != nil {
			fmt.Fprintln(stderr, "warning:", err.Error()) // jobs still valid; never fatal
		}
		for _, j := range jobs {
			if !selected(j) || j.NextRunAt.After(now()) {
				continue
			}
			fireJob(store, now, j, stdout, stderr, exec)
		}
	}

	if once {
		// --once fires every currently-due job once and exits, so --catch-up is a
		// no-op when combined with --once. Intended for use under an external
		// scheduler (system cron / launchd).
		fireDue()
		return exitSuccess
	}

	// Forever-mode startup: unless --catch-up, push STRICTLY-overdue jobs to their
	// next future slot (skip the backlog) without firing.
	if !catchUp {
		reconcileOverdue(store, now, ids, stderr)
	}

	ctx, stop := signalContext()
	defer stop()
	fireDue() // fire anything already due before the first tick
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			fmt.Fprintln(stdout, "cron scheduler stopped.")
			return exitSuccess
		case <-ticker.C:
			fireDue()
		}
	}
}

// contains reports whether ss contains want.
func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// reconcileOverdue (forever-mode startup, non --catch-up) reschedules jobs that
// are STRICTLY overdue (NextRunAt before the current minute) to their next future
// slot without firing the backlog. A job due within the current minute is left
// for the fireDue pass so it still fires now. List errors are warnings, never
// fatal (jobs remains valid).
func reconcileOverdue(store *cron.Store, now func() time.Time, ids []string, stderr io.Writer) {
	jobs, err := store.List()
	if err != nil {
		fmt.Fprintln(stderr, "warning:", err.Error())
	}
	nowMin := now().Truncate(time.Minute)
	for _, j := range jobs {
		if j.Status != cron.StatusActive {
			continue
		}
		if len(ids) > 0 && !contains(ids, j.ID) {
			continue
		}
		if !j.NextRunAt.Before(nowMin) {
			continue // not strictly overdue
		}
		if sched, perr := cron.Parse(j.Expr); perr == nil {
			if nxt := sched.Next(now()); !nxt.IsZero() {
				j.NextRunAt = nxt
				if err := store.Update(j); err != nil {
					fmt.Fprintf(stderr, "warning: failed to reschedule %s: %v\n", j.ID, err)
				}
			}
		}
	}
}

// fireJob claims one job, runs it via the exec runner, and records the outcome.
// Separate scheduler processes can have adjacent slots executing concurrently.
func fireJob(store *cron.Store, now func() time.Time, job cron.Job, stdout io.Writer, stderr io.Writer, exec execRunner) {
	fired := now()
	// Atomically claim this fire before running it so concurrent schedulers (or
	// --once overlapping the forever loop) can't both execute the same due job: the
	// winner advances NextRunAt under the per-job lock, and a loser sees the
	// advanced time and skips. Only the claim writes schedule state; completion
	// must not overwrite a later slot's claim with this run's stale job copy.
	claimed, claimErr := claimFire(store, fired, job.ID)
	if claimErr != nil {
		if errors.Is(claimErr, cron.ErrJobNotFound) {
			return // removed before we could claim
		}
		fmt.Fprintf(stderr, "warning: could not claim job %s: %v\n", job.ID, claimErr)
		return
	}
	if !claimed {
		return // another scheduler already claimed this slot, or the job was paused
	}

	args := []string{"exec", "--output-format", "stream-json", "--session-title", "cron:" + job.ID}
	if job.Cwd != "" {
		args = append(args, "--cwd", job.Cwd)
	}
	if job.Model != "" {
		args = append(args, "--model", job.Model)
	}
	// Inline --prompt= form: a bare "--prompt" "<value>" makes exec reject a
	// dash-leading prompt as a misplaced flag; the =VALUE form is taken verbatim.
	args = append(args, "--prompt="+job.Prompt)

	var outBuf, errBuf strings.Builder
	code := exec(args, &outBuf, &errBuf)

	rec := cron.RunRecord{JobID: job.ID, At: fired, ExitCode: code, SessionTitle: "cron:" + job.ID}
	if code != 0 {
		// The job runs with --output-format stream-json, so a failure is reported as
		// an `error` event on STDOUT, not stderr. Prefer that message; fall back to
		// stderr when stdout carries no error event.
		detail := strings.TrimSpace(errBuf.String())
		if streamErr := extractStreamJSONError(outBuf.String()); streamErr != "" {
			detail = streamErr
		}
		rec.Error = cronTruncate(detail, 500)
	}

	// Explain schedules that claimFire paused because they cannot advance.
	if sched, perr := cron.Parse(job.Expr); perr != nil {
		if rec.Error == "" {
			rec.Error = "invalid schedule; job paused: " + perr.Error()
		}
	} else if nxt := sched.Next(fired.Truncate(time.Minute)); nxt.IsZero() {
		if rec.Error == "" {
			rec.Error = "schedule no longer fires; job paused"
		}
	}
	// Count this completion against current state under the cross-process lock.
	// The claim already advanced or paused the schedule. Leave that state alone:
	// another scheduler may have claimed a later slot or a user may have edited it.
	persisted, err := store.Mutate(job.ID, func(current cron.Job, readErr error) (cron.Job, error) {
		if readErr != nil {
			// Never replace unknown newer state with the pre-execution snapshot.
			return current, readErr
		}
		current.FireCount++
		return current, nil
	})
	if errors.Is(err, cron.ErrJobNotFound) {
		// Genuinely removed mid-run: don't recreate it (no run record, no persist).
		fmt.Fprintf(stdout, "fired %s -> exit %d (job removed during run)\n", job.ID, code)
		return
	}
	// Record the fire. AppendRun takes the per-job lock and bails if the job's
	// metadata is gone, so a Remove landing here cannot resurrect a deleted job's
	// directory with an orphaned runs.jsonl.
	if aerr := store.AppendRun(job.ID, rec); aerr != nil {
		fmt.Fprintf(stderr, "warning: failed to record run for %s: %v\n", job.ID, aerr)
	}
	next := "unknown"
	if err != nil {
		fmt.Fprintf(stderr, "warning: failed to persist job state for %s: %v\n", job.ID, err)
	} else {
		next = formatCronTime(persisted.NextRunAt)
	}
	fmt.Fprintf(stdout, "fired %s -> exit %d (next: %s)\n", job.ID, code, next)
}

// claimFire atomically claims a due job's fire under the per-job lock and reports
// whether THIS caller should run it. It returns false only when the job was paused
// externally or another scheduler already advanced NextRunAt past `fired` (so a
// valid recurring job never double-fires). A valid due job has its NextRunAt
// advanced here to claim the slot; an invalid or exhausted schedule is paused
// here and the caller still fires once (preserving single-scheduler behavior).
func claimFire(store *cron.Store, fired time.Time, id string) (bool, error) {
	claimed := true
	_, err := store.Mutate(id, func(current cron.Job, readErr error) (cron.Job, error) {
		if readErr != nil {
			return current, readErr
		}
		if current.Status != cron.StatusActive {
			claimed = false // paused/other — not ours to fire
			return current, nil
		}
		if current.NextRunAt.After(fired) {
			claimed = false // another scheduler already advanced this slot
			return current, nil
		}
		// Advance to the next slot to claim the fire. Compute it from the minute-
		// aligned fire instant so the schedule's DST fall-back collapse guard (which
		// only engages for a minute-aligned `after`) actually fires, instead of
		// double-firing the repeated wall-clock hour (AUDIT-M4).
		if sched, perr := cron.Parse(current.Expr); perr == nil {
			if nxt := sched.Next(fired.Truncate(time.Minute)); !nxt.IsZero() {
				current.NextRunAt = nxt // advance to claim; concurrent schedulers now skip
				return current, nil
			}
		}
		// Unparseable or unadvanceable schedule (an impossible spec whose Next is
		// zero): pause the job inside this same locked claim so a concurrent scheduler
		// sees a non-active job and does NOT also fire it. The winner still fires once.
		// Without this, both callers leave
		// NextRunAt unchanged, both see the job due, and both fire the same slot
		// (AUDIT-M5).
		current.Status = cron.StatusPaused
		return current, nil
	})
	return claimed, err
}

func cronTruncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	// Cut on a UTF-8 rune boundary so a persisted run-error excerpt can't end in
	// a split multi-byte rune (invalid UTF-8 in the cron record).
	return cutRuneBoundary(s, max) + "…"
}

// extractStreamJSONError scans a stream-json output stream for the message of an
// `error` event (the last one wins). Under --output-format stream-json the
// failure detail rides on stdout, so this recovers it for the run record.
func extractStreamJSONError(output string) string {
	found := ""
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var event struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		}
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			continue
		}
		if event.Type == string(streamjson.EventError) {
			if message := strings.TrimSpace(event.Message); message != "" {
				found = message
			}
		}
	}
	return found
}
