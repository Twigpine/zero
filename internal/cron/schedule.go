// Package cron implements a small, dependency-free standard 5-field cron parser
// and next-run evaluator for the `zero cron` scheduler.
package cron

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Schedule is a parsed 5-field cron expression. Field sets are bitsets over the
// field's value range. domStar/dowStar record whether the day-of-month /
// day-of-week field was "*", which selects the standard Vixie OR semantics.
type Schedule struct {
	minute  uint64 // bits 0-59
	hour    uint64 // bits 0-23
	dom     uint64 // bits 1-31
	month   uint64 // bits 1-12
	dow     uint64 // bits 0-6 (Sunday = 0)
	domStar bool
	dowStar bool
	expr    string
}

// full returns a bitset with every value in [min,max] set.
func full(min, max int) uint64 {
	var m uint64
	for n := min; n <= max; n++ {
		m |= 1 << uint(n)
	}
	return m
}

var monthNames = map[string]int{"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6, "jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12}
var dowNames = map[string]int{"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6}

// Parse parses a standard 5-field cron expression: minute hour day-of-month
// month day-of-week. Supports *, lists (a,b), ranges (a-b), steps (*/s, a-b/s,
// a/s), 3-letter month/weekday names, and 7 as Sunday.
func Parse(expr string) (Schedule, error) {
	fields := strings.Fields(strings.TrimSpace(expr))
	if len(fields) != 5 {
		return Schedule{}, fmt.Errorf("cron expression must have 5 fields (minute hour day-of-month month day-of-week), got %d", len(fields))
	}
	var s Schedule
	var err error
	if s.minute, _, err = parseField(fields[0], 0, 59, nil); err != nil {
		return Schedule{}, fmt.Errorf("minute field: %w", err)
	}
	if s.hour, _, err = parseField(fields[1], 0, 23, nil); err != nil {
		return Schedule{}, fmt.Errorf("hour field: %w", err)
	}
	if s.dom, s.domStar, err = parseField(fields[2], 1, 31, nil); err != nil {
		return Schedule{}, fmt.Errorf("day-of-month field: %w", err)
	}
	if s.month, _, err = parseField(fields[3], 1, 12, monthNames); err != nil {
		return Schedule{}, fmt.Errorf("month field: %w", err)
	}
	if s.dow, s.dowStar, err = parseField(fields[4], 0, 7, dowNames); err != nil {
		return Schedule{}, fmt.Errorf("day-of-week field: %w", err)
	}
	// Normalize 7 -> 0 (Sunday) in the dow bitset.
	if s.dow&(1<<7) != 0 {
		s.dow = (s.dow &^ (1 << 7)) | 1
	}
	s.expr = strings.Join(fields, " ")
	return s, nil
}

// parseField parses one cron field into a bitset over [min,max]. It returns the
// bitset, whether the field was exactly "*", and any error. names maps lowercase
// 3-letter names to numbers (nil if the field has no names).
func parseField(field string, min, max int, names map[string]int) (uint64, bool, error) {
	if field == "*" {
		return full(min, max), true, nil
	}
	var set uint64
	for _, item := range strings.Split(field, ",") {
		if item == "" {
			return 0, false, fmt.Errorf("empty list element in %q", field)
		}
		bitsForItem, err := parseItem(item, min, max, names)
		if err != nil {
			return 0, false, err
		}
		set |= bitsForItem
	}
	return set, false, nil
}

func parseItem(item string, min, max int, names map[string]int) (uint64, error) {
	rangePart := item
	step := 1
	hasStep := false
	if slash := strings.IndexByte(item, '/'); slash >= 0 {
		rangePart = item[:slash]
		stepStr := item[slash+1:]
		n, err := strconv.Atoi(stepStr)
		if err != nil || n <= 0 {
			return 0, fmt.Errorf("invalid step %q in %q", stepStr, item)
		}
		step = n
		hasStep = true
	}

	var lo, hi int
	switch {
	case rangePart == "*":
		lo, hi = min, max
	case strings.IndexByte(rangePart, '-') >= 0:
		dash := strings.IndexByte(rangePart, '-')
		var err error
		if lo, err = parseValue(rangePart[:dash], min, max, names); err != nil {
			return 0, err
		}
		if hi, err = parseValue(rangePart[dash+1:], min, max, names); err != nil {
			return 0, err
		}
		if lo > hi {
			return 0, fmt.Errorf("range start %d after end %d in %q", lo, hi, item)
		}
	default:
		v, err := parseValue(rangePart, min, max, names)
		if err != nil {
			return 0, err
		}
		lo = v
		// A bare "n/s" means n..max step s, for every s including 1
		// ("5/1" is minutes 5..59, not minute 5 alone); a bare "n"
		// with no step at all is just n.
		if hasStep {
			hi = max
		} else {
			hi = v
		}
	}

	var set uint64
	for n := lo; n <= hi; n += step {
		set |= 1 << uint(n)
	}
	return set, nil
}

func parseValue(tok string, min, max int, names map[string]int) (int, error) {
	tok = strings.TrimSpace(tok)
	if names != nil {
		if v, ok := names[strings.ToLower(tok)]; ok {
			return v, nil
		}
	}
	n, err := strconv.Atoi(tok)
	if err != nil {
		return 0, fmt.Errorf("invalid value %q (expected %d-%d or a name)", tok, min, max)
	}
	if n < min || n > max {
		return 0, fmt.Errorf("value %d out of range %d-%d", n, min, max)
	}
	return n, nil
}

// nextSearchYears bounds the forward search. It must exceed the worst-case gap
// between consecutive valid days for any field — notably Feb 29, whose gap can be
// 8 years across a century non-leap-year (e.g. 2096 -> 2104).
const nextSearchYears = 9

// Next returns the first scheduled instant strictly after `after`, evaluated in
// after's location. It returns the zero time.Time if no match occurs within
// nextSearchYears (an impossible schedule such as Feb 30). It is robust to DST
// gaps: a per-iteration forward-progress guard prevents stalling on a
// non-existent local instant (e.g. 02:30 on a spring-forward day).
func (s Schedule) Next(after time.Time) time.Time {
	loc := after.Location()
	t := after.Truncate(time.Minute).Add(time.Minute)
	yearCap := after.Year() + nextSearchYears
	for t.Year() <= yearCap {
		start := t
		switch {
		case !has(s.month, int(t.Month())):
			t = time.Date(t.Year(), t.Month()+1, 1, 0, 0, 0, 0, loc) // first of next month
		case !s.dayMatches(t):
			t = time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, loc) // next day 00:00
		case !has(s.hour, t.Hour()):
			// Advance to the next hour by ABSOLUTE addition from this hour's :00.
			// Using time.Date(..., Hour()+1, ...) can map back into a DST spring-
			// forward gap (02:00 -> 01:00) and stall; adding an hour always moves
			// forward in absolute time and correctly skips the missing hour.
			t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), 0, 0, 0, loc).Add(time.Hour)
		case !has(s.minute, t.Minute()):
			t = t.Add(time.Minute)
		default:
			// DST fall-back guard: on a fall-back day one wall-clock hour
			// occurs twice (01:00-01:59 EDT, then again as EST). If the match
			// is the SECOND occurrence of its wall-clock minute and `after`
			// is at or past the first occurrence, that minute has already had
			// its fire, so returning it would run the same scheduled minute
			// twice. Step past it and keep searching. This covers more than
			// a candidate identical to `after`: when the scheduler chains
			// Next calls (claimFire feeds each fire back in as `after`), a
			// stepped schedule such as "5/1" crosses into the repeated hour
			// at its first stepped minute of the second pass, which need not
			// equal the last fire's minute, and would otherwise re-run every
			// stepped minute of that hour.
			//
			// Only collapse when `after` sits exactly on the minute boundary
			// (its "last fire time" form). If `after` carries sub-minute
			// precision (e.g. 01:30:30 EDT), the first 01:30 fire already
			// preceded it, so the repeated 01:30 EST is the legitimate next
			// fire and must NOT be skipped — skipping it would violate the
			// strictly-after contract.
			if first, repeated := fallBackFirstOccurrence(t); repeated &&
				after.Truncate(time.Minute).Equal(after) && !after.Before(first) {
				t = t.Add(time.Minute)
			} else {
				return t
			}
		}
		// Forward-progress guard: if a wall-clock jump landed on a non-existent
		// local instant (DST gap) and did not advance, step a minute so the search
		// always terminates.
		if !t.After(start) {
			t = start.Add(time.Minute)
		}
	}
	return time.Time{}
}

// fallBackFirstOccurrence reports whether t is the second occurrence of an
// ambiguous wall-clock minute (a DST fall-back repeat) and, if so, returns
// the first occurrence. time.Date resolves an ambiguous wall time to its
// first occurrence, so re-dating t's wall fields lands on the first pass; an
// unambiguous t re-dates to itself.
func fallBackFirstOccurrence(t time.Time) (time.Time, bool) {
	y, mo, d := t.Date()
	first := time.Date(y, mo, d, t.Hour(), t.Minute(), 0, 0, t.Location())
	if first.Before(t) && sameWallClockMinute(first, t) {
		return first, true
	}
	return time.Time{}, false
}

// sameWallClockMinute reports whether two instants share the same local
// year/month/day/hour/minute (wall-clock representation). Used to detect a DST
// fall-back repeat of the same scheduled minute.
func sameWallClockMinute(a, b time.Time) bool {
	ay, am, ad := a.Date()
	by, bm, bd := b.Date()
	return ay == by && am == bm && ad == bd && a.Hour() == b.Hour() && a.Minute() == b.Minute()
}

func has(set uint64, n int) bool { return set&(1<<uint(n)) != 0 }

// dayMatches applies standard Vixie day-of-month / day-of-week semantics: when
// both fields are restricted (neither was "*"), a day matches if EITHER matches;
// otherwise only the restricted field constrains.
func (s Schedule) dayMatches(t time.Time) bool {
	domMatch := has(s.dom, t.Day())
	dowMatch := has(s.dow, int(t.Weekday()))
	switch {
	case s.domStar && s.dowStar:
		return true
	case s.domStar:
		return dowMatch
	case s.dowStar:
		return domMatch
	default:
		return domMatch || dowMatch
	}
}
