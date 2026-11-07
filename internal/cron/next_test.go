package cron

import (
	"testing"
	"time"
)

func mustParse(t *testing.T, expr string) Schedule {
	t.Helper()
	s, err := Parse(expr)
	if err != nil {
		t.Fatalf("Parse(%q): %v", expr, err)
	}
	return s
}

func TestNext(t *testing.T) {
	utc := time.UTC
	cases := []struct {
		expr  string
		after time.Time
		want  time.Time
	}{
		// every minute -> next minute
		{"* * * * *", time.Date(2026, 6, 9, 10, 0, 30, 0, utc), time.Date(2026, 6, 9, 10, 1, 0, 0, utc)},
		// every 15 min
		{"*/15 * * * *", time.Date(2026, 6, 9, 10, 7, 0, 0, utc), time.Date(2026, 6, 9, 10, 15, 0, 0, utc)},
		// hourly at :00 crossing the hour
		{"0 * * * *", time.Date(2026, 6, 9, 10, 30, 0, 0, utc), time.Date(2026, 6, 9, 11, 0, 0, 0, utc)},
		// daily 09:00 -> next day when already past
		{"0 9 * * *", time.Date(2026, 6, 9, 10, 0, 0, 0, utc), time.Date(2026, 6, 10, 9, 0, 0, 0, utc)},
		// month rollover: Jan 31 23:59 -> next match Feb? "0 0 1 * *" first of month
		{"0 0 1 * *", time.Date(2026, 1, 31, 23, 59, 0, 0, utc), time.Date(2026, 2, 1, 0, 0, 0, 0, utc)},
		// year rollover
		{"0 0 1 1 *", time.Date(2026, 6, 9, 0, 0, 0, 0, utc), time.Date(2027, 1, 1, 0, 0, 0, 0, utc)},
		// weekday: next Monday 00:00
		{"0 0 * * MON", time.Date(2026, 6, 9, 12, 0, 0, 0, utc), time.Date(2026, 6, 15, 0, 0, 0, 0, utc)}, // 2026-06-09 is a Tue
		// DOM/DOW OR: fires on the 1st OR any Monday
		{"0 0 1 * MON", time.Date(2026, 6, 9, 0, 0, 0, 0, utc), time.Date(2026, 6, 15, 0, 0, 0, 0, utc)}, // next Monday before next 1st
		// bare "n/1" runs every minute from n on: 10:05:30 -> 10:06, not 11:05
		{"5/1 * * * *", time.Date(2026, 6, 9, 10, 5, 30, 0, utc), time.Date(2026, 6, 9, 10, 6, 0, 0, utc)},
		// ... and wraps to the next hour's first stepped minute (11:05)
		{"5/1 * * * *", time.Date(2026, 6, 9, 10, 59, 30, 0, utc), time.Date(2026, 6, 9, 11, 5, 0, 0, utc)},
	}
	for _, c := range cases {
		got := mustParse(t, c.expr).Next(c.after)
		if !got.Equal(c.want) {
			t.Fatalf("Next(%q, %v) = %v want %v", c.expr, c.after, got, c.want)
		}
	}
}

func TestNextImpossibleReturnsZero(t *testing.T) {
	// Feb 30 never occurs.
	got := mustParse(t, "0 0 30 2 *").Next(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	if !got.IsZero() {
		t.Fatalf("impossible schedule should return zero time, got %v", got)
	}
}

func TestNextIsStrictlyAfter(t *testing.T) {
	// Exactly on a match -> returns the NEXT one, not the same instant.
	at := time.Date(2026, 6, 9, 10, 0, 0, 0, time.UTC)
	got := mustParse(t, "0 * * * *").Next(at)
	if !got.After(at) {
		t.Fatalf("Next must be strictly after input; got %v for %v", got, at)
	}
}

func TestNextDSTSpringForwardTerminates(t *testing.T) {
	// 2026-03-08 is the US spring-forward day: 02:00-02:59 local does not exist in
	// America/New_York. A "30 2 * * *" schedule must not hang (the old hour-advance
	// stalled here); it should skip the missing 02:30 and fire the next day.
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("tzdata unavailable")
	}
	after := time.Date(2026, 3, 8, 0, 0, 0, 0, ny)
	done := make(chan time.Time, 1)
	go func() { done <- mustParse(t, "30 2 * * *").Next(after) }()
	select {
	case got := <-done:
		want := time.Date(2026, 3, 9, 2, 30, 0, 0, ny) // missing 03-08 02:30 -> next day
		if !got.Equal(want) {
			t.Fatalf("DST: Next = %v want %v", got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Next did not terminate on a DST spring-forward gap (infinite loop)")
	}
}

func TestNextDSTFallBackDoesNotRepeatHour(t *testing.T) {
	// 2026-11-01 is the US fall-back day in America/New_York: at 02:00 EDT the
	// clock falls back to 01:00 EST, so 01:30 occurs twice. A daily "30 1" job
	// must fire once that day — Next called with the first (EDT) fire must skip the
	// repeated 01:30 EST and return the NEXT day's 01:30, not the same-day repeat.
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("tzdata unavailable")
	}
	// 05:30 UTC == 01:30 EDT (offset -4), the first of the two 01:30s.
	firstFireEDT := time.Date(2026, 11, 1, 5, 30, 0, 0, time.UTC).In(ny)
	if h := firstFireEDT.Hour(); h != 1 {
		t.Fatalf("setup: expected 01:30 EDT, got hour %d (%s)", h, firstFireEDT)
	}

	got := mustParse(t, "30 1 * * *").Next(firstFireEDT)
	want := time.Date(2026, 11, 2, 1, 30, 0, 0, ny)
	if !got.Equal(want) {
		t.Fatalf("fall-back: Next = %v, want %v (must skip the repeated 01:30 EST)", got, want)
	}
}

func TestNextDSTFallBackReturnsRepeatedMinuteWhenAfterHasSubMinutePrecision(t *testing.T) {
	// The fall-back collapse must only apply when `after` is exactly on the minute
	// boundary (its "last fire time" form). If `after` carries sub-minute precision,
	// the first 01:30 fire already preceded it, so the repeated 01:30 EST is the
	// legitimate next fire and must be returned (strictly-after contract).
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("tzdata unavailable")
	}
	// 05:30:30 UTC == 01:30:30 EDT — 30s past the first 01:30 fire (05:30:00 UTC),
	// which is therefore no longer eligible.
	afterEDT := time.Date(2026, 11, 1, 5, 30, 30, 0, time.UTC).In(ny)

	got := mustParse(t, "30 1 * * *").Next(afterEDT)
	// 06:30:00 UTC == 01:30:00 EST, the repeated minute and the next eligible fire.
	want := time.Date(2026, 11, 1, 6, 30, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("sub-minute after: Next = %v, want %v (must return the repeated 01:30 EST, not skip to the next day)", got.UTC(), want)
	}
}

func TestNextLeapYearCenturyGap(t *testing.T) {
	// 2100 is NOT a leap year, so the next Feb 29 after 2096 is 2104 (an 8-year
	// gap). The search window must be wide enough to find it (not report zero).
	got := mustParse(t, "0 0 29 2 *").Next(time.Date(2096, 3, 1, 0, 0, 0, 0, time.UTC))
	want := time.Date(2104, 2, 29, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("leap-gap: Next = %v want %v", got, want)
	}
}
