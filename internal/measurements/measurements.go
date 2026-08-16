// Package measurements keeps a run's own timings so a report cannot invent them.
//
// THE FAILURE THIS EXISTS FOR. A measured run finished a benchmark and reported
// a table of test timings that no command in the session had produced. The same
// test read 0.86s in one paste and 4.20s in the next with nothing said about the
// difference, a -race overhead moved from +3.7% to +133% between two tellings of
// the same result, and the column summed to an exact total no real transcript
// lands on. A prompt rule — "re-run every command before you paste it" — is the
// obvious answer and the weak one: a model that is willing to write numbers it
// did not measure is equally willing to say it re-ran them.
//
// The harness is not. Every command's output passed through this process and was
// written to the session log, so the run's real numbers are already here. This
// package reads them back and compares them against what the answer claims,
// which is the one check a model cannot satisfy by asserting harder.
//
// DELIBERATELY LOOSE. Timings vary between runs for honest reasons — a loaded
// machine, a warm cache, a different -count. The tolerance below is a 50% band,
// which lets ordinary variation through and catches 0.86s reported as 4.20s. The
// cost of a false positive is high (a tripwire that cries wolf is turned off, and
// then it catches nothing), and the cost of a false negative is one uncaught
// number, so this errs firmly toward silence.
package measurements

import (
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Measurement is one timing a command reported: what was measured, and how long
// it took in seconds.
type Measurement struct {
	Name    string
	Seconds float64
}

// Conflict is a number an answer states that the session never recorded.
type Conflict struct {
	Name string
	// Claimed is the value the answer gave, in seconds.
	Claimed float64
	// Recorded is every value this session actually observed for Name, sorted.
	Recorded []float64
}

var (
	// `ok  	github.com/x/y	8.337s` and its FAIL twin. Not anchored at the end:
	// a coverage or cached suffix may follow.
	goTestPackageLine = regexp.MustCompile(`(?m)^(?:ok|FAIL)\s+(\S+)\s+([0-9]+(?:\.[0-9]+)?)s(?:\s|$)`)
	// `--- PASS: TestFoo (0.30s)`, at any indentation, including subtests.
	goTestCaseLine = regexp.MustCompile(`(?m)^\s*--- (?:PASS|FAIL|SKIP):\s+(\S+)\s+\(([0-9]+(?:\.[0-9]+)?)s\)`)
	// A duration as an answer would write it, in seconds or milliseconds.
	claimedDuration = regexp.MustCompile(`([0-9]+(?:\.[0-9]+)?)\s*(ms|s)\b`)
	// The compound Go duration form, tried FIRST: "1m10s" must not be read as its
	// seconds remainder. The seconds group is optional so a bare "2m" also parses.
	claimedMinuteDuration = regexp.MustCompile(`([0-9]+)m(?:([0-9]+(?:\.[0-9]+)?)s)?\b`)
)

// ParseGoTest pulls every timing out of `go test` output.
//
// Two shapes only — the per-package result line and the per-case `--- PASS`
// line. Benchmarks report ns/op rather than a duration and are NOT read here:
// guessing at a unit would put wrong numbers in the ledger, and a ledger that is
// itself unreliable is worse than none.
func ParseGoTest(text string) []Measurement {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	var out []Measurement
	for _, pattern := range []*regexp.Regexp{goTestPackageLine, goTestCaseLine} {
		for _, match := range pattern.FindAllStringSubmatch(text, -1) {
			seconds, err := strconv.ParseFloat(match[2], 64)
			if err != nil {
				continue
			}
			name := strings.TrimSpace(match[1])
			if name == "" {
				continue
			}
			out = append(out, Measurement{Name: name, Seconds: seconds})
		}
	}
	return out
}

// Ledger is every timing this run observed, and which conflicts it has already
// raised.
//
// A nil Ledger is a working no-op, so a caller that does not want the check
// holds nil and still calls every method unconditionally.
type Ledger struct {
	mu       sync.Mutex
	observed map[string][]float64
	raised   map[string]bool
}

func NewLedger() *Ledger {
	return &Ledger{observed: map[string][]float64{}, raised: map[string]bool{}}
}

// Record reads any timings out of a command's output and remembers them.
// Returns how many it took, which is what a test asserts on.
func (l *Ledger) Record(text string) int {
	if l == nil {
		return 0
	}
	found := ParseGoTest(text)
	if len(found) == 0 {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, m := range found {
		l.observed[m.Name] = append(l.observed[m.Name], m.Seconds)
	}
	return len(found)
}

// tolerance reports whether two timings are close enough to be the same result
// measured twice. A 50% band with a small absolute floor: ordinary variation and
// sub-centisecond jitter pass, a fivefold difference does not.
func tolerance(a, b float64) bool {
	spread := math.Max(math.Abs(a), math.Abs(b)) * 0.5
	if spread < 0.05 {
		spread = 0.05
	}
	return math.Abs(a-b) <= spread
}

// Conflicts reports numbers in claim that contradict what this session recorded.
//
// ONLY NAMES THE LEDGER ALREADY KNOWS are considered, and only when the claim
// puts a duration next to one on the same line. An answer that mentions a test
// without timing it, or that reports something never measured here, produces
// nothing — this check exists to catch a number that DISAGREES with the
// transcript, not to demand that every number have one.
//
// Each name is reported at most once per Ledger. A second pass over the same
// answer is silent, so the caller can feed a correction back to the model
// without the possibility of a loop.
func (l *Ledger) Conflicts(claim string) []Conflict {
	if l == nil || strings.TrimSpace(claim) == "" {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	var out []Conflict
	for name, recorded := range l.observed {
		if l.raised[name] || len(recorded) == 0 {
			continue
		}
		claimed, ok := claimedSecondsFor(claim, name)
		if !ok {
			continue
		}
		agrees := false
		for _, seen := range recorded {
			if tolerance(claimed, seen) {
				agrees = true
				break
			}
		}
		if agrees {
			continue
		}
		values := append([]float64(nil), recorded...)
		sort.Float64s(values)
		out = append(out, Conflict{Name: name, Claimed: claimed, Recorded: values})
	}
	// Deterministic order: this text reaches a model, and a set that reshuffles
	// between identical runs is a diff nobody can read.
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	for _, conflict := range out {
		l.raised[conflict.Name] = true
	}
	return out
}

// claimedSecondsFor finds the duration an answer puts beside a name, searching
// the remainder of each line the name appears on. Same line only: a number three
// paragraphs away is not this name's timing, and pairing them would invent a
// disagreement rather than find one.
func claimedSecondsFor(claim, name string) (float64, bool) {
	for _, line := range strings.Split(claim, "\n") {
		for start := 0; start < len(line); {
			index := strings.Index(line[start:], name)
			if index < 0 {
				break
			}
			absolute := start + index
			end := absolute + len(name)
			start = absolute + 1
			if !nameBoundary(line, absolute, end) {
				continue
			}
			if value, ok := parseClaimedDuration(line[end:]); ok {
				return value, true
			}
		}
	}
	return 0, false
}

// nameBoundary reports whether line[from:to] is a whole token rather than the
// head of a longer one.
//
// A raw substring search made an HONEST report look like a fabrication whenever
// one recorded name is a prefix of another, which `go test -v` guarantees: it
// prints the parent above every subtest and the ledger records both, so a
// truthful "TestParent/subcase took 0.02s" matched the ledger entry for
// TestParent and was told it invented the number. Package names collide the same
// way with no subtests at all — internal/agent is a prefix of internal/agentinit,
// and this repo has several such pairs. A tripwire that accuses honest reports is
// one that gets switched off, and then it catches nothing.
//
// "/" and "." continue a name rather than ending it, so TestParent does not match
// inside TestParent/subcase and internal/agent does not match inside
// internal/agentinit.
func nameBoundary(line string, from, to int) bool {
	continues := func(b byte) bool {
		switch {
		case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9':
			return true
		case b == '_', b == '/', b == '.', b == '-':
			return true
		}
		return false
	}
	if from > 0 && continues(line[from-1]) {
		return false
	}
	if to < len(line) && continues(line[to]) {
		return false
	}
	return true
}

// parseClaimedDuration reads the FIRST duration in tail, in seconds.
//
// MINUTES COUNT. The pattern was ms-or-s only, so "1m10s" failed on "1m", the
// scan moved on, and "10s" won: a truthful restatement of a recorded 70 seconds
// was reported as a conflict, and the nudge then quoted 10s back at the model — a
// number its answer never contained. Anything over a minute is ordinary in this
// repo's own suite.
//
// POSITION DECIDES, NOT PATTERN ORDER. Trying the minute form over the whole
// tail first reached past a nearer seconds figure to claim a later one:
//
//	"took 0.86s (package total 1m20s)"  ->  80, not 0.86
//
// The claim being checked is the test's own 0.86s; 1m20s is the package total
// that happens to trail it. Reading the far number as the claim invented a
// conflict against a number the model got right, which is the one failure this
// package must never produce. Both patterns are located, and the minute form
// wins only when it starts no later than the seconds form.
func parseClaimedDuration(tail string) (float64, bool) {
	minute := claimedMinuteDuration.FindStringSubmatchIndex(tail)
	plain := claimedDuration.FindStringSubmatchIndex(tail)
	switch {
	case minute == nil && plain == nil:
		return 0, false
	case minute != nil && (plain == nil || minute[0] <= plain[0]):
		minutes, err := strconv.ParseFloat(tail[minute[2]:minute[3]], 64)
		if err != nil {
			return 0, false
		}
		seconds := 0.0
		// Group 2 is optional: "1m" alone leaves it unset, which regexp reports
		// as index -1 rather than an empty span.
		if minute[4] >= 0 {
			parsed, secErr := strconv.ParseFloat(tail[minute[4]:minute[5]], 64)
			if secErr != nil {
				return 0, false
			}
			seconds = parsed
		}
		return minutes*60 + seconds, true
	}
	value, err := strconv.ParseFloat(tail[plain[2]:plain[3]], 64)
	if err != nil {
		return 0, false
	}
	if tail[plain[4]:plain[5]] == "ms" {
		value /= 1000
	}
	return value, true
}

// Nudge renders conflicts as the correction a model is asked to act on. Empty
// when there is nothing to say, so the caller can test the string itself.
func Nudge(conflicts []Conflict) string {
	if len(conflicts) == 0 {
		return ""
	}
	var b strings.Builder
	if len(conflicts) == 1 {
		b.WriteString("One number in your answer does not match what this session recorded.\n")
	} else {
		b.WriteString("Some numbers in your answer do not match what this session recorded.\n")
	}
	for _, conflict := range conflicts {
		b.WriteString("  - ")
		b.WriteString(conflict.Name)
		b.WriteString(": your answer says ")
		b.WriteString(formatSeconds(conflict.Claimed))
		b.WriteString("; the commands actually run in this session reported ")
		for i, value := range conflict.Recorded {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(formatSeconds(value))
		}
		b.WriteString(".\n")
	}
	b.WriteString("Re-run the command and report what it prints. If both numbers are real, give both and say what changed between them — " +
		"do not replace one with the other silently. If the number was not measured in this session, say so plainly instead of stating it as a result.")
	return b.String()
}

func formatSeconds(value float64) string {
	return strconv.FormatFloat(value, 'f', -1, 64) + "s"
}
