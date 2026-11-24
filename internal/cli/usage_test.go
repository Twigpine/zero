package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Twigpine/zero/internal/sessions"
	"github.com/Twigpine/zero/internal/usage"
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
func TestUsageReportIgnoresRawResolvedModelInStoredEvents(t *testing.T) {
	isolateCLIUserState(t)

	plainStore := sessions.NewStore(sessions.StoreOptions{RootDir: t.TempDir(), Now: fixedCLITime("2026-06-01T09:00:00Z")})
	rawStore := sessions.NewStore(sessions.StoreOptions{RootDir: t.TempDir(), Now: fixedCLITime("2026-06-01T09:00:00Z")})
	for _, store := range []*sessions.Store{plainStore, rawStore} {
		sess, err := store.Create(sessions.CreateInput{SessionID: "guard_s1", Title: "Guard", Cwd: "/repo", ModelID: "gpt-4.1", Provider: "openai"})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		payload := map[string]any{"promptTokens": 1000, "completionTokens": 200, "totalTokens": 1200}
		if store == rawStore {
			payload["resolvedModel"] = "\x1b[31mx\u0085"
		}
		if _, err := store.AppendEvent(sess.SessionID, sessions.AppendEventInput{Type: sessions.EventUsage, Payload: payload}); err != nil {
			t.Fatalf("AppendEvent: %v", err)
		}
	}

	var plainText, plainJSON bytes.Buffer
	var plainStderr bytes.Buffer
	plainExit := runWithDeps([]string{"usage", "report"}, &plainText, &plainStderr, appDeps{
		newSessionStore: func() *sessions.Store { return plainStore },
		inspectChanges:  stubInspectChanges(""),
	})
	var rawText, rawJSON bytes.Buffer
	var rawStderr bytes.Buffer
	rawExit := runWithDeps([]string{"usage", "report"}, &rawText, &rawStderr, appDeps{
		newSessionStore: func() *sessions.Store { return rawStore },
		inspectChanges:  stubInspectChanges(""),
	})
	if plainExit != exitSuccess || rawExit != exitSuccess {
		t.Fatalf("exit codes plain=%d raw=%d stderr plain=%q raw=%q", plainExit, rawExit, plainStderr.String(), rawStderr.String())
	}
	rawJSONExit := runWithDeps([]string{"usage", "report", "--json"}, &rawJSON, &rawStderr, appDeps{
		newSessionStore: func() *sessions.Store { return rawStore },
		inspectChanges:  stubInspectChanges(""),
	})
	runWithDeps([]string{"usage", "report", "--json"}, &plainJSON, &plainStderr, appDeps{
		newSessionStore: func() *sessions.Store { return plainStore },
		inspectChanges:  stubInspectChanges(""),
	})
	if rawJSONExit != exitSuccess {
		t.Fatalf("json report exit %d: %s", rawJSONExit, rawStderr.String())
	}

	for _, out := range []string{rawText.String(), rawJSON.String()} {
		if strings.Contains(out, "\x1b") || strings.Contains(out, `\u001b`) || strings.Contains(out, "\u0085") {
			t.Fatalf("output contains control/format escape: %q", out)
		}
	}

	var plainRep, rawRep struct {
		Total   json.RawMessage `json:"total"`
		Buckets []any           `json:"buckets"`
	}
	if err := json.Unmarshal(plainJSON.Bytes(), &plainRep); err != nil {
		t.Fatalf("plain json: %v", err)
	}
	if err := json.Unmarshal(rawJSON.Bytes(), &rawRep); err != nil {
		t.Fatalf("raw json: %v", err)
	}
	if !bytes.Equal(plainRep.Total, rawRep.Total) {
		t.Fatalf("total differs: plain %s raw %s", plainRep.Total, rawRep.Total)
	}
	plainBuckets, _ := json.Marshal(plainRep.Buckets)
	rawBuckets, _ := json.Marshal(rawRep.Buckets)
	if !bytes.Equal(plainBuckets, rawBuckets) {
		t.Fatalf("buckets differ: plain %s raw %s", plainBuckets, rawBuckets)
	}
}

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

func TestFormatReportAndRunUsage(t *testing.T) {
	const reportHeader = "resolved models (reported by provider; cost uses the requested model)"

	t.Run("a-no-resolved-block", func(t *testing.T) {
		// Same shape as TestRunUsageTextReport / seedUsageStore (2× gpt-4.1 events, net LOC 70).
		report := usage.Report{
			Buckets: []usage.DayBucket{{
				Date: "2026-06-01", Requests: 2, InputTokens: 1500, OutputTokens: 300,
				TotalTokens: 1800, TotalCost: 0.0054,
			}},
			Total: usage.Totals{
				Requests: 2, InputTokens: 1500, OutputTokens: 300, TotalTokens: 1800, TotalCost: 0.0054,
			},
			NetLOC:          70,
			NetLOCPositive:  true,
			TokensPerNetLOC: 1800.0 / 70,
			CostPerNetLOC:   0.0054 / 70,
			LOCEstimated:    true,
			CostEstimated:   true,
		}
		const want = "" +
			"Usage report (cost is a reconstructed estimate)\n" +
			"\n" +
			"date           requests         tokens      est. cost\n" +
			"2026-06-01            2          1,800      $0.005400\n" +
			"\n" +
			"total                 2          1,800      $0.005400\n" +
			"\n" +
			"net LOC (estimate): +100 / -30 = 70\n" +
			"tokens per net LOC: 25.7\n" +
			"est. cost per net LOC: $0.000077"
		got := FormatReport(report, 100, 30)
		if got != want {
			t.Fatalf("FormatReport =\n%q\nwant\n%q", got, want)
		}
	})

	t.Run("b-resolved-block-placement", func(t *testing.T) {
		report := usage.Report{
			Buckets: []usage.DayBucket{{Date: "2026-06-01", Requests: 3, TotalTokens: 66}},
			Total:   usage.Totals{Requests: 3, TotalTokens: 66},
			NetLOC:  10, NetLOCPositive: true, TokensPerNetLOC: 6.6, LOCEstimated: true, CostEstimated: true,
			ResolvedModels: []usage.ResolvedModelUsage{
				{RequestedModel: "a-model", ResolvedModel: "z", Requests: 1, TotalTokens: 33},
				{RequestedModel: "gpt-test", ResolvedModel: "deepseek/a", Requests: 1, TotalTokens: 22},
				{RequestedModel: "gpt-test", ResolvedModel: "x/y", Requests: 1, TotalTokens: 11},
			},
		}
		text := FormatReport(report, 5, 5)
		lines := strings.Split(text, "\n")
		totalIdx, headerIdx, netLOCIdx := -1, -1, -1
		for i, line := range lines {
			if strings.HasPrefix(line, "total") && totalIdx < 0 {
				totalIdx = i
			}
			if line == reportHeader {
				headerIdx = i
			}
			if strings.HasPrefix(line, "net LOC") {
				netLOCIdx = i
			}
		}
		if totalIdx < 0 || headerIdx < 0 || netLOCIdx < 0 {
			t.Fatalf("could not find total/header/net LOC in:\n%s", text)
		}
		if totalIdx >= headerIdx || headerIdx >= netLOCIdx {
			t.Fatalf("block order wrong: total@%d header@%d netLOC@%d", totalIdx, headerIdx, netLOCIdx)
		}
		wantLines := []string{
			"  requests 1, tokens 33: a-model -> z",
			"  requests 1, tokens 22: gpt-test -> deepseek/a",
			"  requests 1, tokens 11: gpt-test -> x/y",
		}
		block := lines[headerIdx+1 : headerIdx+1+len(wantLines)]
		for i := range wantLines {
			if block[i] != wantLines[i] {
				t.Fatalf("line %d = %q, want %q", i, block[i], wantLines[i])
			}
		}
	})

	t.Run("c-unknown-requested", func(t *testing.T) {
		report := usage.Report{
			Buckets:        []usage.DayBucket{{Date: "2026-06-01", Requests: 1, TotalTokens: 9}},
			Total:          usage.Totals{Requests: 1, TotalTokens: 9},
			NetLOC:         1,
			LOCEstimated:   true,
			CostEstimated:  true,
			ResolvedModels: []usage.ResolvedModelUsage{{RequestedModel: "", ResolvedModel: "x", Requests: 1, TotalTokens: 9}},
		}
		text := FormatReport(report, 0, 0)
		if !strings.Contains(text, "  requests 1, tokens 9: (unknown) -> x") {
			t.Fatalf("missing (unknown) line in:\n%s", text)
		}
	})

	t.Run("d-e2e-clean-attribution", func(t *testing.T) {
		isolateCLIUserState(t)

		store := sessions.NewStore(sessions.StoreOptions{RootDir: t.TempDir(), Now: fixedCLITime("2026-06-01T09:00:00Z")})
		sess, err := store.Create(sessions.CreateInput{SessionID: "attr_s1", Title: "Attr", Cwd: "/repo", ModelID: "gpt-4.1", Provider: "openai"})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		payload := map[string]any{
			"promptTokens": 1000, "completionTokens": 200, "totalTokens": 1200,
			"resolvedModel": "\x1b[31mx\u0085",
		}
		if _, err := store.AppendEvent(sess.SessionID, sessions.AppendEventInput{Type: sessions.EventUsage, Payload: payload}); err != nil {
			t.Fatalf("AppendEvent: %v", err)
		}

		var textOut, jsonOut bytes.Buffer
		var stderr bytes.Buffer
		deps := appDeps{
			newSessionStore: func() *sessions.Store { return store },
			inspectChanges:  stubInspectChanges(""),
		}
		if code := runWithDeps([]string{"usage", "report"}, &textOut, &stderr, deps); code != exitSuccess {
			t.Fatalf("text report exit %d: %s", code, stderr.String())
		}
		if code := runWithDeps([]string{"usage", "report", "--json"}, &jsonOut, &stderr, deps); code != exitSuccess {
			t.Fatalf("json report exit %d: %s", code, stderr.String())
		}
		text := textOut.String()
		wantLine := "  requests 1, tokens 1,200: gpt-4.1 -> [31mx"
		// The whole row, not a prefix: a row that continues past the expected
		// text (an unsanitized tail) must not pass.
		foundRow := false
		for _, row := range strings.Split(text, "\n") {
			if row == wantLine {
				foundRow = true
			}
		}
		if !foundRow {
			t.Fatalf("text has no row equal to %q:\n%s", wantLine, text)
		}
		var rep struct {
			ResolvedModels []struct {
				ResolvedModel string `json:"resolvedModel"`
			} `json:"resolvedModels"`
		}
		if err := json.Unmarshal(jsonOut.Bytes(), &rep); err != nil {
			t.Fatalf("json decode: %v\n%s", err, jsonOut.String())
		}
		if len(rep.ResolvedModels) != 1 || rep.ResolvedModels[0].ResolvedModel != "[31mx" {
			t.Fatalf("resolvedModels = %+v, want [31mx", rep.ResolvedModels)
		}
		for _, out := range []string{text, jsonOut.String()} {
			if strings.Contains(out, "\x1b") || strings.Contains(out, `\u001b`) || strings.Contains(out, "\u0085") {
				t.Fatalf("output contains forbidden escape: %q", out)
			}
		}
	})

	t.Run("e-plain-store-no-block", func(t *testing.T) {
		isolateCLIUserState(t)

		store := sessions.NewStore(sessions.StoreOptions{RootDir: t.TempDir(), Now: fixedCLITime("2026-06-01T09:00:00Z")})
		sess, err := store.Create(sessions.CreateInput{SessionID: "plain_s1", Title: "Plain", Cwd: "/repo", ModelID: "gpt-4.1", Provider: "openai"})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		payload := map[string]any{"promptTokens": 1000, "completionTokens": 200, "totalTokens": 1200}
		if _, err := store.AppendEvent(sess.SessionID, sessions.AppendEventInput{Type: sessions.EventUsage, Payload: payload}); err != nil {
			t.Fatalf("AppendEvent: %v", err)
		}
		var textOut, jsonOut bytes.Buffer
		var stderr bytes.Buffer
		deps := appDeps{
			newSessionStore: func() *sessions.Store { return store },
			inspectChanges:  stubInspectChanges(""),
		}
		if code := runWithDeps([]string{"usage", "report"}, &textOut, &stderr, deps); code != exitSuccess {
			t.Fatalf("text report exit %d: %s", code, stderr.String())
		}
		if code := runWithDeps([]string{"usage", "report", "--json"}, &jsonOut, &stderr, deps); code != exitSuccess {
			t.Fatalf("json report exit %d: %s", code, stderr.String())
		}
		if strings.Contains(textOut.String(), "resolved models") {
			t.Fatalf("text shows resolved block:\n%s", textOut.String())
		}
		var keys map[string]json.RawMessage
		if err := json.Unmarshal(jsonOut.Bytes(), &keys); err != nil {
			t.Fatalf("json decode: %v", err)
		}
		if _, ok := keys["resolvedModels"]; ok {
			t.Fatalf("json has resolvedModels key: %s", jsonOut.String())
		}
	})
}

// The " -> " separator is printable, so it survives sanitizing. Two different
// (requested, resolved) pairs must not render as the same row.
func TestFormatReportQuotesModelIDsThatContainTheSeparator(t *testing.T) {
	render := func(requested, resolved string) string {
		return FormatReport(usage.Report{
			ResolvedModels: []usage.ResolvedModelUsage{{RequestedModel: requested, ResolvedModel: resolved, Requests: 1, TotalTokens: 5}},
		}, 0, 0)
	}
	a := render("router", "alias -> leaf")
	b := render("router -> alias", "leaf")
	if a == b {
		t.Fatalf("two different pairs render the same text:\n%s", a)
	}
	if !strings.Contains(a, `router -> "alias -> leaf"`) || !strings.Contains(b, `"router -> alias" -> leaf`) {
		t.Fatalf("ids that contain the separator are not quoted:\n%s\n%s", a, b)
	}
	if plain := render("router", "leaf"); !strings.Contains(plain, "  requests 1, tokens 5: router -> leaf") {
		t.Fatalf("plain ids must stay unquoted:\n%s", plain)
	}
}
