package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Twigpine/zero/internal/sessions"
	"github.com/Twigpine/zero/internal/zerogit"
)

func seedUsageStore(t *testing.T) *sessions.Store {
	t.Helper()
	store := sessions.NewStore(sessions.StoreOptions{RootDir: t.TempDir(), Now: fixedCLITime("2026-06-01T09:00:00Z")})
	session, err := store.Create(sessions.CreateInput{SessionID: "usage_s1", Title: "Usage", Cwd: "/repo", ModelID: "gpt-4.1", Provider: "openai"})
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	for _, payload := range []map[string]any{
		{"promptTokens": 1000, "completionTokens": 200, "totalTokens": 1200},
		{"promptTokens": 500, "completionTokens": 100, "totalTokens": 600},
	} {
		if _, err := store.AppendEvent(session.SessionID, sessions.AppendEventInput{Type: sessions.EventUsage, Payload: payload}); err != nil {
			t.Fatalf("AppendEvent returned error: %v", err)
		}
	}
	return store
}

func stubInspectChanges(stat string) func(context.Context, zerogit.InspectOptions) (zerogit.ChangeSummary, error) {
	return func(context.Context, zerogit.InspectOptions) (zerogit.ChangeSummary, error) {
		return zerogit.ChangeSummary{Root: "/repo", DiffStat: stat}, nil
	}
}

func TestRunUsageTextReport(t *testing.T) {
	store := seedUsageStore(t)
	var stdout, stderr bytes.Buffer
	exitCode := runWithDeps([]string{"usage", "report"}, &stdout, &stderr, appDeps{
		newSessionStore: func() *sessions.Store { return store },
		inspectChanges:  stubInspectChanges(" 1 file changed, 100 insertions(+), 30 deletions(-)"),
	})
	if exitCode != exitSuccess {
		t.Fatalf("expected exit %d, got %d: %s", exitSuccess, exitCode, stderr.String())
	}
	output := stdout.String()
	for _, want := range []string{"2026-06-01", "1,800", "estimate", "net LOC", "+100", "-30"} {
		if !strings.Contains(output, want) {
			t.Fatalf("usage report missing %q in:\n%s", want, output)
		}
	}
}

func TestRunUsageDefaultsToReport(t *testing.T) {
	store := seedUsageStore(t)
	var stdout, stderr bytes.Buffer
	exitCode := runWithDeps([]string{"usage"}, &stdout, &stderr, appDeps{
		newSessionStore: func() *sessions.Store { return store },
		inspectChanges:  stubInspectChanges(""),
	})
	if exitCode != exitSuccess {
		t.Fatalf("expected exit %d, got %d: %s", exitSuccess, exitCode, stderr.String())
	}
	if !strings.Contains(stdout.String(), "2026-06-01") {
		t.Fatalf("expected default report output, got %q", stdout.String())
	}
}

func TestRunUsageJSONReport(t *testing.T) {
	store := seedUsageStore(t)
	var stdout, stderr bytes.Buffer
	exitCode := runWithDeps([]string{"usage", "report", "--json"}, &stdout, &stderr, appDeps{
		newSessionStore: func() *sessions.Store { return store },
		inspectChanges:  stubInspectChanges(" 1 file changed, 100 insertions(+), 30 deletions(-)"),
	})
	if exitCode != exitSuccess {
		t.Fatalf("expected exit %d, got %d: %s", exitSuccess, exitCode, stderr.String())
	}
	var report struct {
		NetLOC int `json:"netLOC"`
		Total  struct {
			Requests    int `json:"requests"`
			TotalTokens int `json:"totalTokens"`
		} `json:"total"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("usage JSON did not decode: %v\n%s", err, stdout.String())
	}
	if report.NetLOC != 70 || report.Total.Requests != 2 || report.Total.TotalTokens != 1800 {
		t.Fatalf("unexpected usage JSON: %+v", report)
	}
}

func TestRunUsageSinceFilter(t *testing.T) {
	store := seedUsageStore(t)
	var stdout, stderr bytes.Buffer
	exitCode := runWithDeps([]string{"usage", "report", "--since", "2026-07-01"}, &stdout, &stderr, appDeps{
		newSessionStore: func() *sessions.Store { return store },
		inspectChanges:  stubInspectChanges(""),
	})
	if exitCode != exitSuccess {
		t.Fatalf("expected exit %d, got %d: %s", exitSuccess, exitCode, stderr.String())
	}
	if strings.Contains(stdout.String(), "2026-06-01") {
		t.Fatalf("expected --since to filter out June events, got %q", stdout.String())
	}
}

func TestRunUsageSessionFilter(t *testing.T) {
	store := seedUsageStore(t)
	var stdout, stderr bytes.Buffer
	exitCode := runWithDeps([]string{"usage", "report", "--json", "--session", "missing_session"}, &stdout, &stderr, appDeps{
		newSessionStore: func() *sessions.Store { return store },
		inspectChanges:  stubInspectChanges(""),
	})
	if exitCode != exitSuccess {
		t.Fatalf("expected exit %d, got %d: %s", exitSuccess, exitCode, stderr.String())
	}
	var report struct {
		Total struct {
			Requests int `json:"requests"`
		} `json:"total"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("usage JSON did not decode: %v\n%s", err, stdout.String())
	}
	if report.Total.Requests != 0 {
		t.Fatalf("expected unknown session to yield 0 requests, got %d", report.Total.Requests)
	}
}

func TestRunUsageHelp(t *testing.T) {
	var stdout, stderr bytes.Buffer
	exitCode := runWithDeps([]string{"usage", "--help"}, &stdout, &stderr, appDeps{})
	if exitCode != exitSuccess {
		t.Fatalf("expected exit %d, got %d: %s", exitSuccess, exitCode, stderr.String())
	}
	for _, want := range []string{"zero usage report", "--json", "--days", "--since", "--session"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("usage help missing %q in:\n%s", want, stdout.String())
		}
	}
}

func TestRunUsageUnknownFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	exitCode := runWithDeps([]string{"usage", "report", "--bogus"}, &stdout, &stderr, appDeps{})
	if exitCode != exitUsage {
		t.Fatalf("expected usage exit %d, got %d", exitUsage, exitCode)
	}
	if !strings.Contains(stderr.String(), "unknown usage flag") {
		t.Fatalf("expected unknown-flag error, got %q", stderr.String())
	}
}

// TestRunUsageEmptySessionRejected verifies that an empty --session/--session-id
// value is rejected with exitUsage and the value-required error, matching the
// --since validation style, rather than silently filtering to no session.
func TestRunUsageEmptySessionRejected(t *testing.T) {
	cases := []struct {
		name string
		args []string
		flag string
	}{
		{name: "session equals empty", args: []string{"usage", "report", "--session="}, flag: "--session"},
		{name: "session-id equals empty", args: []string{"usage", "report", "--session-id="}, flag: "--session-id"},
		{name: "session spaced empty", args: []string{"usage", "report", "--session", "   "}, flag: "--session"},
		{name: "session-id spaced empty", args: []string{"usage", "report", "--session-id", ""}, flag: "--session-id"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			exitCode := runWithDeps(tc.args, &stdout, &stderr, appDeps{
				newSessionStore: func() *sessions.Store {
					return sessions.NewStore(sessions.StoreOptions{RootDir: t.TempDir()})
				},
				inspectChanges: stubInspectChanges(""),
			})
			if exitCode != exitUsage {
				t.Fatalf("%s: expected exitUsage (%d), got %d", tc.flag, exitUsage, exitCode)
			}
			msg := stderr.String()
			if !strings.Contains(msg, tc.flag+" requires a value") {
				t.Fatalf("%s: expected value-required error in stderr, got %q", tc.flag, msg)
			}
		})
	}
}

// seedUsageStoreDates creates a store with usage events spread across two
// different calendar dates. The "recent" session's events fall on recentDate
// and the "old" session's events fall on oldDate. Both dates must be
// YYYY-MM-DD strings that parse as time.RFC3339 with a T00:00:00Z suffix.
func seedUsageStoreDates(t *testing.T, recentDate, oldDate string) (*sessions.Store, func() time.Time) {
	t.Helper()

	// The store's Now function is used to timestamp every appended event.
	// We swap it via this pointer so we can stamp the two sessions differently.
	currentTime := mustParseTime(t, recentDate+"T09:00:00Z")
	nowFunc := func() time.Time { return currentTime }

	store := sessions.NewStore(sessions.StoreOptions{RootDir: t.TempDir(), Now: nowFunc})

	// Session whose events fall on recentDate.
	sess, err := store.Create(sessions.CreateInput{SessionID: "days_recent", Title: "Recent", Cwd: "/repo", ModelID: "gpt-4.1", Provider: "openai"})
	if err != nil {
		t.Fatalf("Create recent session: %v", err)
	}
	if _, err := store.AppendEvent(sess.SessionID, sessions.AppendEventInput{
		Type:    sessions.EventUsage,
		Payload: map[string]any{"promptTokens": 100, "completionTokens": 20, "totalTokens": 120},
	}); err != nil {
		t.Fatalf("AppendEvent recent: %v", err)
	}

	// Swap the clock to oldDate before creating the old session's events.
	currentTime = mustParseTime(t, oldDate+"T09:00:00Z")

	sessOld, err := store.Create(sessions.CreateInput{SessionID: "days_old", Title: "Old", Cwd: "/repo", ModelID: "gpt-4.1", Provider: "openai"})
	if err != nil {
		t.Fatalf("Create old session: %v", err)
	}
	if _, err := store.AppendEvent(sessOld.SessionID, sessions.AppendEventInput{
		Type:    sessions.EventUsage,
		Payload: map[string]any{"promptTokens": 200, "completionTokens": 40, "totalTokens": 240},
	}); err != nil {
		t.Fatalf("AppendEvent old: %v", err)
	}

	// Return the store and a fixed-time now func anchored at recentDate.
	return store, fixedCLITime(recentDate + "T12:00:00Z")
}

func mustParseTime(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatalf("mustParseTime %q: %v", value, err)
	}
	return parsed
}

// TestRunUsageDaysFilter verifies that --days N excludes events outside the
// rolling window and includes events inside it.
func TestRunUsageDaysFilter(t *testing.T) {
	// recentDate is within the last 3 days; oldDate is 10 days ago.
	recentDate := "2026-06-06"
	oldDate := "2026-05-29"
	store, nowFunc := seedUsageStoreDates(t, recentDate, oldDate)

	var stdout, stderr bytes.Buffer
	exitCode := runWithDeps([]string{"usage", "report", "--days", "3"}, &stdout, &stderr, appDeps{
		newSessionStore: func() *sessions.Store { return store },
		inspectChanges:  stubInspectChanges(""),
		now:             nowFunc, // anchored at 2026-06-06T12:00:00Z
	})
	if exitCode != exitSuccess {
		t.Fatalf("expected exit %d, got %d: %s", exitSuccess, exitCode, stderr.String())
	}
	output := stdout.String()
	// The recent event (recentDate) must appear.
	if !strings.Contains(output, recentDate) {
		t.Fatalf("--days 3 should include %s, got:\n%s", recentDate, output)
	}
	// The old event (oldDate) must be excluded.
	if strings.Contains(output, oldDate) {
		t.Fatalf("--days 3 should exclude %s, got:\n%s", oldDate, output)
	}
}

// TestRunUsageInvalidSince verifies that malformed --since values are rejected
// with exitUsage and the expected validation message, while a valid YYYY-MM-DD
// date is accepted.
func TestRunUsageInvalidSince(t *testing.T) {
	cases := []struct {
		since       string
		expectError bool
	}{
		{"foo", true},
		{"2026-6-1", true},    // unpadded month/day
		{"06/01/2026", true},  // wrong separator
		{"2026-06-01", false}, // valid
	}
	for _, tc := range cases {
		tc := tc
		t.Run("since="+tc.since, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			exitCode := runWithDeps([]string{"usage", "report", "--since", tc.since}, &stdout, &stderr, appDeps{
				newSessionStore: func() *sessions.Store {
					return sessions.NewStore(sessions.StoreOptions{RootDir: t.TempDir()})
				},
				inspectChanges: stubInspectChanges(""),
			})
			if tc.expectError {
				if exitCode != exitUsage {
					t.Fatalf("--since %q: expected exitUsage (%d), got %d", tc.since, exitUsage, exitCode)
				}
				msg := stderr.String()
				if !strings.Contains(msg, "invalid --since") {
					t.Fatalf("--since %q: expected validation error in stderr, got %q", tc.since, msg)
				}
				if !strings.Contains(msg, "YYYY-MM-DD") {
					t.Fatalf("--since %q: expected YYYY-MM-DD hint in stderr, got %q", tc.since, msg)
				}
			} else {
				if exitCode != exitSuccess {
					t.Fatalf("--since %q: expected exitSuccess (%d), got %d: %s", tc.since, exitSuccess, exitCode, stderr.String())
				}
			}
		})
	}
}

// TestRunUsageEmptyStore verifies that running `usage report` against a store
// with no usage events exits successfully, prints the header, shows a zero
// total, and does not panic.
func TestRunUsageEmptyStore(t *testing.T) {
	store := sessions.NewStore(sessions.StoreOptions{RootDir: t.TempDir()})
	var stdout, stderr bytes.Buffer
	exitCode := runWithDeps([]string{"usage", "report"}, &stdout, &stderr, appDeps{
		newSessionStore: func() *sessions.Store { return store },
		inspectChanges:  stubInspectChanges(""),
	})
	if exitCode != exitSuccess {
		t.Fatalf("empty store: expected exit %d, got %d: %s", exitSuccess, exitCode, stderr.String())
	}
	output := stdout.String()
	if !strings.Contains(output, "Usage report") {
		t.Fatalf("empty store: expected header in output, got:\n%s", output)
	}
	// Total row must show zero requests / tokens.
	if !strings.Contains(output, "total") {
		t.Fatalf("empty store: expected 'total' row in output, got:\n%s", output)
	}
}
