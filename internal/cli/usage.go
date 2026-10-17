package cli

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/Twigpine/zero/internal/modelregistry"
	"github.com/Twigpine/zero/internal/sessions"
	"github.com/Twigpine/zero/internal/usage"
	"github.com/Twigpine/zero/internal/zerogit"
)

type usageOptions struct {
	json      bool
	days      int
	since     string
	sessionID string
}

// usageEventSet keeps the persisted events paired with the session metadata they
// were read alongside, so cost reconstruction can resolve each event's owning
// model id during aggregation.
type usageEventSet struct {
	events  []sessions.Event
	meta    []sessions.Metadata
	skipped int
}

// collectUsageData reads every persisted session's events (optionally limited to
// a single session id) and returns them flattened alongside the matching session
// metadata. It mirrors runSearch's persisted-event traversal over the injected
// session store. Sessions whose event log cannot be read are skipped (and
// counted) so one corrupt session can't abort the whole report.
func collectUsageData(store *sessions.Store, sessionFilter string) (usageEventSet, error) {
	metadata, err := store.List()
	if err != nil {
		return usageEventSet{}, err
	}
	set := usageEventSet{}
	for _, meta := range metadata {
		if sessionFilter != "" && meta.SessionID != sessionFilter {
			continue
		}
		sessionEvents, err := store.ReadEvents(meta.SessionID)
		if err != nil {
			set.skipped++
			continue
		}
		set.meta = append(set.meta, meta)
		set.events = append(set.events, sessionEvents...)
	}
	return set, nil
}

// filterEventsSince drops events whose UTC calendar date precedes the inclusive
// lower bound. An empty since returns the events unchanged.
func filterEventsSince(events []sessions.Event, since string) []sessions.Event {
	if since == "" {
		return events
	}
	filtered := make([]sessions.Event, 0, len(events))
	for _, event := range events {
		if eventUTCDate(event.CreatedAt) >= since {
			filtered = append(filtered, event)
		}
	}
	return filtered
}

// eventUTCDate maps an RFC3339 timestamp to its UTC calendar date (YYYY-MM-DD)
// so the --since/--days cutoff compares against the same UTC day the report
// buckets under. On a parse failure it falls back to the leading-10 slice.
func eventUTCDate(createdAt string) string {
	if parsed, err := time.Parse(time.RFC3339, createdAt); err == nil {
		return parsed.UTC().Format("2006-01-02")
	}
	if len(createdAt) >= 10 {
		return createdAt[:10]
	}
	return createdAt
}

func runUsage(args []string, stdout io.Writer, stderr io.Writer, deps appDeps) int {
	subcommand := "report"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		subcommand = strings.ToLower(strings.TrimSpace(args[0]))
		args = args[1:]
	}
	if subcommand == "help" {
		if err := writeUsageHelp(stdout); err != nil {
			return exitCrash
		}
		return exitSuccess
	}
	if subcommand != "report" {
		return writeExecUsageError(stderr, fmt.Sprintf("unknown usage command %q", subcommand))
	}

	options, help, err := parseUsageArgs(args)
	if err != nil {
		return writeExecUsageError(stderr, err.Error())
	}
	if help {
		if err := writeUsageHelp(stdout); err != nil {
			return exitCrash
		}
		return exitSuccess
	}

	set, err := collectUsageData(deps.newSessionStore(), options.sessionID)
	if err != nil {
		return writeAppError(stderr, err.Error(), exitCrash)
	}
	since := usageSinceCutoff(options, deps)
	events := filterEventsSince(set.events, since)

	// The net-LOC column is best-effort garnish on a token report: outside a
	// git repository (or on any git failure) it degrades to zero instead of
	// aborting the entire report.
	diff := zerogit.DiffStat{}
	if workspaceRoot, err := resolveWorkspaceRoot("", deps); err == nil {
		if summary, err := deps.inspectChanges(context.Background(), zerogit.InspectOptions{Cwd: workspaceRoot}); err == nil {
			// The --stat summary line ("N files changed, A insertions(+), B
			// deletions(-)") carries no secret-bearing tokens, so parsing the
			// already-redacted DiffStat returned by zerogit.Inspect is safe.
			diff = zerogit.ParseDiffStat(summary.DiffStat)
		}
	}

	registry, err := modelregistry.DefaultRegistry()
	if err != nil {
		return writeAppError(stderr, err.Error(), exitCrash)
	}
	report, err := usage.BuildReport(events, set.meta, &registry, diff.NetLOC())
	if err != nil {
		return writeAppError(stderr, err.Error(), exitCrash)
	}

	if options.json {
		if err := writePrettyJSON(stdout, report); err != nil {
			return exitCrash
		}
		return exitSuccess
	}
	if _, err := fmt.Fprintln(stdout, FormatReport(report, diff.Insertions, diff.Deletions)); err != nil {
		return exitCrash
	}
	return exitSuccess
}

// usageSinceCutoff resolves the inclusive lower-bound date (YYYY-MM-DD). An
// explicit --since wins; otherwise --days N derives a cutoff relative to the
// injected clock. An empty result means "no lower bound".
func usageSinceCutoff(options usageOptions, deps appDeps) string {
	if strings.TrimSpace(options.since) != "" {
		return options.since
	}
	if options.days > 0 {
		cutoff := deps.now().UTC().AddDate(0, 0, -(options.days - 1))
		return cutoff.Format("2006-01-02")
	}
	return ""
}

func parseUsageArgs(args []string) (usageOptions, bool, error) {
	options := usageOptions{}
	for index := 0; index < len(args); index++ {
		arg := args[index]
		switch {
		case arg == "-h" || arg == "--help" || arg == "help":
			return options, true, nil
		case arg == "--json":
			options.json = true
		case arg == "--days":
			value, next, err := nextFlagValue(args, index, arg)
			if err != nil {
				return options, false, err
			}
			days, err := parsePositiveOrZeroInt(value, "--days")
			if err != nil {
				return options, false, err
			}
			options.days = days
			index = next
		case strings.HasPrefix(arg, "--days="):
			days, err := parsePositiveOrZeroInt(strings.TrimSpace(strings.TrimPrefix(arg, "--days=")), "--days")
			if err != nil {
				return options, false, err
			}
			options.days = days
		case arg == "--since":
			value, next, err := nextFlagValue(args, index, arg)
			if err != nil {
				return options, false, err
			}
			if _, parseErr := time.Parse("2006-01-02", value); parseErr != nil {
				return options, false, execUsageError{fmt.Sprintf("invalid --since %q: expected YYYY-MM-DD", value)}
			}
			options.since = value
			index = next
		case strings.HasPrefix(arg, "--since="):
			value := strings.TrimSpace(strings.TrimPrefix(arg, "--since="))
			if _, parseErr := time.Parse("2006-01-02", value); parseErr != nil {
				return options, false, execUsageError{fmt.Sprintf("invalid --since %q: expected YYYY-MM-DD", value)}
			}
			options.since = value
		case arg == "--session" || arg == "--session-id":
			value, next, err := nextFlagValue(args, index, arg)
			if err != nil {
				return options, false, err
			}
			if strings.TrimSpace(value) == "" {
				return options, false, execUsageError{fmt.Sprintf("%s requires a value", arg)}
			}
			options.sessionID = strings.TrimSpace(value)
			index = next
		case strings.HasPrefix(arg, "--session="):
			value := strings.TrimSpace(strings.TrimPrefix(arg, "--session="))
			if value == "" {
				return options, false, execUsageError{"--session requires a value"}
			}
			options.sessionID = value
		case strings.HasPrefix(arg, "--session-id="):
			value := strings.TrimSpace(strings.TrimPrefix(arg, "--session-id="))
			if value == "" {
				return options, false, execUsageError{"--session-id requires a value"}
			}
			options.sessionID = value
		default:
			return options, false, execUsageError{fmt.Sprintf("unknown usage flag %q", arg)}
		}
	}
	return options, false, nil
}

// FormatReport renders a usage Report as a per-day table plus totals and net-LOC
// efficiency ratios. Cost and net-LOC are labeled as estimates: persisted usage
// events carry no model id or cost (cost is reconstructed from session
// metadata), and net-LOC is a working-tree diff proxy. The insertions/deletions
// counts come straight from the parsed working-tree --stat.
func FormatReport(report usage.Report, insertions int, deletions int) string {
	var builder strings.Builder
	builder.WriteString("Usage report (cost is a reconstructed estimate)\n\n")
	fmt.Fprintf(&builder, "%-12s %10s %14s %14s\n", "date", "requests", "tokens", "est. cost")
	for _, bucket := range report.Buckets {
		fmt.Fprintf(&builder, "%-12s %10d %14s %14s\n",
			bucket.Date, bucket.Requests, groupThousands(bucket.TotalTokens), formatUSD(bucket.TotalCost))
	}
	fmt.Fprintf(&builder, "\n%-12s %10d %14s %14s\n",
		"total", report.Total.Requests, groupThousands(report.Total.TotalTokens), formatUSD(report.Total.TotalCost))

	fmt.Fprintf(&builder, "\nnet LOC (estimate): +%d / -%d = %d\n",
		insertions, deletions, report.NetLOC)
	if report.NetLOCPositive {
		fmt.Fprintf(&builder, "tokens per net LOC: %.1f\n", report.TokensPerNetLOC)
		fmt.Fprintf(&builder, "est. cost per net LOC: %s\n", formatUSD(report.CostPerNetLOC))
	} else {
		builder.WriteString("tokens per net LOC: n/a (net LOC <= 0)\n")
		builder.WriteString("est. cost per net LOC: n/a (net LOC <= 0)\n")
	}
	return strings.TrimRight(builder.String(), "\n")
}

func formatUSD(value float64) string {
	formatted, err := modelregistry.FormatCostUSD(value)
	if err != nil {
		return "$0.0000"
	}
	return formatted
}

// groupThousands renders an integer with comma thousands separators, preserving
// a leading minus sign.
func groupThousands(value int) string {
	sign := ""
	if value < 0 {
		sign = "-"
		value = -value
	}
	digits := fmt.Sprintf("%d", value)
	if len(digits) <= 3 {
		return sign + digits
	}
	var out []byte
	prefix := len(digits) % 3
	if prefix == 0 {
		prefix = 3
	}
	out = append(out, digits[:prefix]...)
	for index := prefix; index < len(digits); index += 3 {
		out = append(out, ',')
		out = append(out, digits[index:index+3]...)
	}
	return sign + string(out)
}

func writeUsageHelp(w io.Writer) error {
	_, err := fmt.Fprint(w, `Usage:
  zero usage report [flags]

Summarizes token usage and reconstructed (estimated) cost from persisted local
Zero session usage events, plus a working-tree net-LOC efficiency estimate.

Flags:
      --json                 Print JSON report
      --days <number>        Only include the most recent N days
      --since <YYYY-MM-DD>    Only include events on or after this date
      --session <id>         Limit the report to one session
  -h, --help                 Show this help

Cost is reconstructed from session model metadata and is an estimate; net LOC
is a working-tree diff proxy and is labeled as an estimate.
`)
	return err
}
