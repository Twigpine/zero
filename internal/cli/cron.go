package cli

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/Twigpine/zero/internal/cron"
)

// runCron is the dispatch entry for `zero cron`.
func runCron(args []string, stdout io.Writer, stderr io.Writer, deps appDeps) int {
	now := time.Now
	if deps.now != nil {
		now = deps.now
	}
	store := cron.NewStore(cron.StoreOptions{Now: now})
	return runCronWith(store, now, args, stdout, stderr)
}

// runCronWith is the testable core (store + clock injected). `run` is handled in
// cron_run.go (Task 6).
func runCronWith(store *cron.Store, now func() time.Time, args []string, stdout io.Writer, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "Usage: zero cron <add|list|rm|pause|resume|run> ...")
		return exitUsage
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "-h", "--help", "help":
		writeCronHelp(stdout)
		return exitSuccess
	case "add":
		return cronAdd(store, now, rest, stdout, stderr)
	case "list", "ls":
		return cronList(store, now, stdout, stderr)
	case "rm", "remove":
		return cronSimple(store, rest, stdout, stderr, func(id string) error { return store.Remove(id) }, "Removed")
	case "pause":
		return cronSetStatus(store, rest, stdout, stderr, cron.StatusPaused, "Paused")
	case "resume":
		return cronResume(store, now, rest, stdout, stderr)
	case "run":
		return cronRun(store, now, rest, stdout, stderr, Run)
	default:
		fmt.Fprintf(stderr, "Unknown cron subcommand: %s\n", sub)
		return exitUsage
	}
}

func cronAdd(store *cron.Store, now func() time.Time, args []string, stdout io.Writer, stderr io.Writer) int {
	var expr, prompt, recipe, cwd, model string
	runNow := false
	var positional []string
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "--prompt":
			v, n, err := nextFlagValue(args, i, a)
			if err != nil {
				fmt.Fprintln(stderr, err.Error())
				return exitUsage
			}
			prompt, i = v, n
		case strings.HasPrefix(a, "--prompt="):
			prompt = strings.TrimSpace(strings.TrimPrefix(a, "--prompt="))
		case a == "--recipe":
			v, n, err := nextFlagValue(args, i, a)
			if err != nil {
				fmt.Fprintln(stderr, err.Error())
				return exitUsage
			}
			recipe, i = v, n
		case strings.HasPrefix(a, "--recipe="):
			recipe = strings.TrimSpace(strings.TrimPrefix(a, "--recipe="))
		case a == "--cwd":
			v, n, err := nextFlagValue(args, i, a)
			if err != nil {
				fmt.Fprintln(stderr, err.Error())
				return exitUsage
			}
			cwd, i = v, n
		case strings.HasPrefix(a, "--cwd="):
			cwd = strings.TrimSpace(strings.TrimPrefix(a, "--cwd="))
		case a == "--model":
			v, n, err := nextFlagValue(args, i, a)
			if err != nil {
				fmt.Fprintln(stderr, err.Error())
				return exitUsage
			}
			model, i = v, n
		case strings.HasPrefix(a, "--model="):
			model = strings.TrimSpace(strings.TrimPrefix(a, "--model="))
		case a == "--run-now":
			runNow = true
		case strings.HasPrefix(a, "-"):
			fmt.Fprintf(stderr, "Unknown cron add flag: %s\n", a)
			return exitUsage
		default:
			positional = append(positional, a)
		}
	}

	if len(positional) > 1 {
		fmt.Fprintf(stderr, "Unexpected extra arguments: %s\n", strings.Join(positional[1:], " "))
		return exitUsage
	}
	// Consume an explicit positional expression BEFORE applying recipe defaults,
	// so `cron add "0 9 * * *" --recipe X` keeps the user's schedule instead of
	// silently dropping it in favor of the recipe's expr.
	if len(positional) == 1 && expr == "" {
		expr = positional[0]
	}
	if recipe != "" {
		r, ok := cron.Recipe(recipe)
		if !ok {
			fmt.Fprintf(stderr, "Unknown recipe %q. Available: %s\n", recipe, recipeIDs())
			return exitUsage
		}
		if expr == "" {
			expr = r.Expr
		}
		if prompt == "" {
			prompt = r.Prompt
		}
	}
	if expr == "" {
		fmt.Fprintln(stderr, "A cron expression is required (e.g. `zero cron add \"0 9 * * *\" --prompt ...`).")
		return exitUsage
	}
	schedule, err := cron.Parse(expr)
	if err != nil {
		fmt.Fprintln(stderr, err.Error())
		return exitUsage
	}
	if prompt == "" {
		home, _ := os.UserHomeDir()
		jobCwd := cwd
		if jobCwd == "" {
			jobCwd, _ = os.Getwd()
		}
		prompt, err = cron.ResolveLoopPrompt(jobCwd, home)
		if err != nil {
			fmt.Fprintln(stderr, err.Error())
			return exitCrash
		}
	}
	// Reject an impossible schedule (e.g. "0 0 30 2 *") regardless of --run-now,
	// so a job that can never advance is never persisted (it would otherwise
	// re-fire every tick under `cron run`).
	next := schedule.Next(now())
	if next.IsZero() {
		fmt.Fprintln(stderr, "That schedule never fires.")
		return exitUsage
	}
	if runNow {
		next = now()
	}
	job, err := store.Add(cron.Job{Expr: expr, Prompt: prompt, Cwd: cwd, Model: model, Status: cron.StatusActive, NextRunAt: next})
	if err != nil {
		fmt.Fprintln(stderr, err.Error())
		return exitCrash
	}
	fmt.Fprintf(stdout, "Added cron job %s (%s); next run %s.\n", job.ID, expr, formatCronTime(job.NextRunAt))
	fmt.Fprintln(stdout, "Note: the prompt is stored in plaintext on disk — do not embed secrets.")
	return exitSuccess
}

func cronList(store *cron.Store, now func() time.Time, stdout io.Writer, stderr io.Writer) int {
	jobs, err := store.List()
	if err != nil {
		// A corrupt job is surfaced as a warning; the listable jobs are still shown.
		fmt.Fprintln(stderr, "warning:", err.Error())
	}
	if len(jobs) == 0 {
		fmt.Fprintln(stdout, "No scheduled jobs.")
		return exitSuccess
	}
	sort.Slice(jobs, func(i, j int) bool {
		ai, aj := jobs[i].Status == cron.StatusActive, jobs[j].Status == cron.StatusActive
		if ai != aj {
			return ai // active first
		}
		if !jobs[i].NextRunAt.Equal(jobs[j].NextRunAt) {
			return jobs[i].NextRunAt.Before(jobs[j].NextRunAt)
		}
		return jobs[i].ID < jobs[j].ID
	})
	for _, j := range jobs {
		fmt.Fprintf(stdout, "%s · %s · %s (next: %s) · #%d · %s\n",
			j.ID, j.Status, j.Expr, formatCronTime(j.NextRunAt), j.FireCount, promptExcerpt(j.Prompt))
	}
	return exitSuccess
}

func cronSimple(store *cron.Store, args []string, stdout io.Writer, stderr io.Writer, fn func(string) error, verb string) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "An id is required.")
		return exitUsage
	}
	if err := fn(args[0]); err != nil {
		fmt.Fprintln(stderr, err.Error())
		return exitUsage
	}
	fmt.Fprintf(stdout, "%s cron job %s.\n", verb, args[0])
	return exitSuccess
}

func cronSetStatus(store *cron.Store, args []string, stdout io.Writer, stderr io.Writer, status string, verb string) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "An id is required.")
		return exitUsage
	}
	job, err := store.Get(args[0])
	if err != nil {
		fmt.Fprintln(stderr, err.Error())
		return exitUsage
	}
	job.Status = status
	if err := store.Update(job); err != nil {
		fmt.Fprintln(stderr, err.Error())
		return exitCrash
	}
	fmt.Fprintf(stdout, "%s cron job %s.\n", verb, job.ID)
	return exitSuccess
}

// cronResume sets a job active and recomputes NextRunAt.
func cronResume(store *cron.Store, now func() time.Time, args []string, stdout io.Writer, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "An id is required.")
		return exitUsage
	}
	job, err := store.Get(args[0])
	if err != nil {
		fmt.Fprintln(stderr, err.Error())
		return exitUsage
	}
	job.Status = cron.StatusActive
	// Don't reactivate a job whose schedule can't advance (corrupt expr or an
	// impossible spec) — it would leave a stale/zero NextRunAt for the runner.
	sched, perr := cron.Parse(job.Expr)
	if perr != nil {
		fmt.Fprintln(stderr, perr.Error())
		return exitUsage
	}
	next := sched.Next(now())
	if next.IsZero() {
		fmt.Fprintln(stderr, "That schedule never fires.")
		return exitUsage
	}
	job.NextRunAt = next
	if err := store.Update(job); err != nil {
		fmt.Fprintln(stderr, err.Error())
		return exitCrash
	}
	fmt.Fprintf(stdout, "Resumed cron job %s (next run %s).\n", job.ID, next.Format(time.RFC3339))
	return exitSuccess
}

func recipeIDs() string {
	var ids []string
	for _, r := range cron.Recipes() {
		ids = append(ids, r.ID)
	}
	return strings.Join(ids, ", ")
}

func formatCronTime(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return t.UTC().Format("2006-01-02 15:04 MST")
}

func promptExcerpt(p string) string {
	p = strings.TrimSpace(strings.ReplaceAll(p, "\n", " "))
	if len(p) > 48 {
		return cutRuneBoundary(p, 47) + "…"
	}
	return p
}

func writeCronHelp(w io.Writer) {
	fmt.Fprint(w, `zero cron — schedule agent jobs (foreground, file-backed)

Usage:
  zero cron add <cron-expr> [--prompt P | --recipe R] [--cwd D] [--model M] [--run-now]
  zero cron list
  zero cron pause <id> | resume <id> | rm <id>
  zero cron run [--once] [--catch-up] [id...]

Cron expression: standard 5 fields "minute hour day-of-month month day-of-week".
`)
}
