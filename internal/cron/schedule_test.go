package cron

import "testing"

func bits(ns ...int) uint64 {
	var m uint64
	for _, n := range ns {
		m |= 1 << uint(n)
	}
	return m
}

func TestParseValidFields(t *testing.T) {
	cases := []struct {
		expr                        string
		minute, hour, dom, mon, dow uint64
		domStar, dowStar            bool
	}{
		{"* * * * *", full(0, 59), full(0, 23), full(1, 31), full(1, 12), full(0, 6), true, true},
		{"0 9 * * *", bits(0), bits(9), full(1, 31), full(1, 12), full(0, 6), true, true},
		{"*/15 * * * *", bits(0, 15, 30, 45), full(0, 23), full(1, 31), full(1, 12), full(0, 6), true, true},
		{"0 0,12 * * *", bits(0), bits(0, 12), full(1, 31), full(1, 12), full(0, 6), true, true},
		{"0 9-17 * * 1-5", bits(0), bits(9, 10, 11, 12, 13, 14, 15, 16, 17), full(1, 31), full(1, 12), bits(1, 2, 3, 4, 5), true, false},
		{"0 0 1 */3 *", bits(0), bits(0), bits(1), bits(1, 4, 7, 10), full(0, 6), false, true},
		{"0 0 * * SUN", bits(0), bits(0), full(1, 31), full(1, 12), bits(0), true, false},
		{"0 0 * JAN,DEC *", bits(0), bits(0), full(1, 31), bits(1, 12), full(0, 6), true, true},
		{"0 0 * * 7", bits(0), bits(0), full(1, 31), full(1, 12), bits(0), true, false}, // 7 => Sunday
		// A bare "n/1" is n..max step 1, not n alone: the step's presence,
		// not its size, is what extends the range.
		{"5/1 * * * *", full(5, 59), full(0, 23), full(1, 31), full(1, 12), full(0, 6), true, true},
		{"0 9/1 * * *", bits(0), full(9, 23), full(1, 31), full(1, 12), full(0, 6), true, true},
		{"0 0 1/1 * *", bits(0), bits(0), full(1, 31), full(1, 12), full(0, 6), false, true},
		{"0 0 * 1/1 *", bits(0), bits(0), full(1, 31), full(1, 12), full(0, 6), true, true},
		{"5/2 * * * *", bits(5, 7, 9, 11, 13, 15, 17, 19, 21, 23, 25, 27, 29, 31, 33, 35, 37, 39, 41, 43, 45, 47, 49, 51, 53, 55, 57, 59), full(0, 23), full(1, 31), full(1, 12), full(0, 6), true, true},
	}
	for _, c := range cases {
		s, err := Parse(c.expr)
		if err != nil {
			t.Fatalf("Parse(%q) error: %v", c.expr, err)
		}
		if s.minute != c.minute || s.hour != c.hour || s.dom != c.dom || s.month != c.mon || s.dow != c.dow {
			t.Fatalf("Parse(%q) sets minute=%b hour=%b dom=%b mon=%b dow=%b", c.expr, s.minute, s.hour, s.dom, s.month, s.dow)
		}
		if s.domStar != c.domStar || s.dowStar != c.dowStar {
			t.Fatalf("Parse(%q) star flags dom=%v dow=%v want %v/%v", c.expr, s.domStar, s.dowStar, c.domStar, c.dowStar)
		}
		if s.String() != c.expr {
			t.Fatalf("String()=%q want %q", s.String(), c.expr)
		}
	}
}

func TestParseErrors(t *testing.T) {
	bad := []string{
		"", "* * * *", "* * * * * *", // wrong field count
		"60 * * * *", "* 24 * * *", "* * 0 * *", "* * 32 * *", "* * * 0 *", "* * * 13 *", "* * * * 8", // out of range
		"*/0 * * * *", "5-2 * * * *", "a * * * *", "* * * FOO *", "1- * * * *", "1,,2 * * * *",
	}
	for _, expr := range bad {
		if _, err := Parse(expr); err == nil {
			t.Fatalf("Parse(%q) expected error", expr)
		}
	}
}
