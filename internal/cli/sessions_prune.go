package cli

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/Gitlawb/zero/internal/config"
	"github.com/Gitlawb/zero/internal/redaction"
	"github.com/Gitlawb/zero/internal/sessions"
)

// maxPruneDays keeps a day count from overflowing time.Duration.
const maxPruneDays = 36500

// runSessionsPrune removes sessions last updated before a cutoff, or with
// --dry-run lists what would go and why. It runs only when asked (#971).
func runSessionsPrune(store *sessions.Store, options sessionCommandOptions, stdout io.Writer, stderr io.Writer, deps appDeps) int {
	olderThan, err := pruneCutoff(options, deps)
	if err != nil {
		return writeExecUsageError(stderr, err.Error())
	}
	report, err := store.Prune(sessions.PruneOptions{OlderThan: olderThan, DryRun: options.dryRun})
	if err != nil {
		return writeSessionCommandError(stderr, err)
	}
	if options.json {
		if err := writePrettyJSON(stdout, redaction.RedactValue(report, redaction.Options{})); err != nil {
			return exitCrash
		}
	} else if _, err := fmt.Fprint(stdout, formatPruneReport(report)); err != nil {
		return exitCrash
	}
	if len(report.Failed) > 0 {
		return exitCrash
	}
	return exitSuccess
}

// pruneCutoff is --older-than, or else sessions.retentionDays from the user's
// own config, and never below sessions.MinimumPruneAge. Project config has no
// say in it: see config.SessionsConfig.
func pruneCutoff(options sessionCommandOptions, deps appDeps) (time.Duration, error) {
	var age time.Duration
	if options.olderThan != "" {
		parsed, err := parsePruneAge(options.olderThan)
		if err != nil {
			return 0, err
		}
		age = parsed
	} else {
		days := 0
		if deps.userConfigPath != nil {
			path, err := deps.userConfigPath()
			if err != nil {
				return 0, fmt.Errorf("sessions prune could not find your config: %w", err)
			}
			settings, err := config.ReadSessionsConfig(path)
			if err != nil {
				return 0, fmt.Errorf("sessions prune could not read sessions.retentionDays: %w", err)
			}
			days = settings.RetentionDays
		}
		if days == 0 {
			return 0, execUsageError{"sessions prune needs a cutoff: pass --older-than (for example --older-than 30d) or set sessions.retentionDays in your user config"}
		}
		if days > maxPruneDays {
			return 0, execUsageError{fmt.Sprintf("sessions.retentionDays %d is more than %d", days, maxPruneDays)}
		}
		age = time.Duration(days) * 24 * time.Hour
	}
	if age < sessions.MinimumPruneAge {
		return 0, execUsageError{fmt.Sprintf("sessions prune does not remove anything updated in the last %d hours; use --older-than 1d or more", int(sessions.MinimumPruneAge.Hours()))}
	}
	return age, nil
}

// parsePruneAge accepts a whole number of days ("30d") or a Go duration
// ("720h").
func parsePruneAge(value string) (time.Duration, error) {
	invalid := execUsageError{fmt.Sprintf("invalid --older-than %q: expected days like 30d or a duration like 720h", value)}
	trimmed := strings.TrimSpace(value)
	if days, ok := strings.CutSuffix(trimmed, "d"); ok {
		count, err := strconv.Atoi(days)
		if err != nil || count <= 0 || count > maxPruneDays {
			return 0, invalid
		}
		return time.Duration(count) * 24 * time.Hour, nil
	}
	age, err := time.ParseDuration(trimmed)
	if err != nil || age <= 0 {
		return 0, invalid
	}
	return age, nil
}

func formatPruneReport(report sessions.PruneReport) string {
	var out strings.Builder
	verb := "Removed"
	if report.DryRun {
		verb = "Would remove"
	}
	if len(report.Removed) == 0 {
		fmt.Fprintf(&out, "No sessions to remove: nothing last updated before %s can go.\n", report.Cutoff)
	} else {
		var total int64
		for _, entry := range report.Removed {
			total += entry.Bytes
		}
		fmt.Fprintf(&out, "%s %d %s last updated before %s (%s):\n", verb, len(report.Removed), plural(len(report.Removed), "session", "sessions"), report.Cutoff, formatPruneBytes(total))
		for _, entry := range report.Removed {
			fmt.Fprintf(&out, "  %s\n", formatPruneEntry(entry, redact(entry.Title)))
		}
	}
	if len(report.Kept) > 0 {
		fmt.Fprintf(&out, "Kept %d old enough to remove:\n", len(report.Kept))
		for _, entry := range report.Kept {
			fmt.Fprintf(&out, "  %s\n", formatPruneEntry(entry, entry.Reason))
		}
	}
	if len(report.Failed) > 0 {
		failed := "Could not remove"
		if report.DryRun {
			failed = "Could not check"
		}
		fmt.Fprintf(&out, "%s %d:\n", failed, len(report.Failed))
		for _, entry := range report.Failed {
			fmt.Fprintf(&out, "  %s\n", formatPruneEntry(entry, redact(entry.Reason)))
		}
	}
	if report.DryRun {
		out.WriteString("Dry run: nothing was removed.\n")
	}
	return out.String()
}

func formatPruneEntry(entry sessions.PruneEntry, note string) string {
	updated := entry.UpdatedAt
	if len(updated) >= len("2006-01-02") {
		updated = updated[:len("2006-01-02")]
	}
	line := redact(entry.SessionID) + "  " + updated
	if note = strings.TrimSpace(note); note != "" {
		line += "  " + note
	}
	return line
}

func formatPruneBytes(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	value, suffix := float64(bytes)/unit, "KB"
	for _, next := range []string{"MB", "GB", "TB"} {
		if value < unit {
			break
		}
		value, suffix = value/unit, next
	}
	return fmt.Sprintf("%.1f %s", value, suffix)
}

func plural(count int, one, many string) string {
	if count == 1 {
		return one
	}
	return many
}
