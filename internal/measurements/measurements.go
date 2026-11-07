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
	"encoding/json"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"
)

// Measurement is one timing a command reported: what was measured, and how long
// it took in seconds.
type Measurement struct {
	Name    string
	Package string
	Test    string
	Seconds float64
}

type measurementID struct {
	Package string
	Test    string
}

func (m Measurement) identity() measurementID {
	if m.Test != "" {
		return measurementID{Package: m.Package, Test: m.Test}
	}
	return measurementID{Package: m.Name}
}

// Conflict is a number an answer states that the session never recorded.
type Conflict struct {
	Name string
	// Claimed is the value the answer gave, in seconds.
	Claimed float64
	// Recorded is every value this session actually observed for Name UNDER THE
	// SAME RUN, sorted.
	Recorded []float64
	// Run is the command those values came from.
	Run Run
}

// Run is the command a set of timings came from.
//
// TIMINGS FROM DIFFERENT COMMANDS ARE DIFFERENT MEASUREMENTS. Everything used to
// pool into map[name][]seconds, so a claim about an ordinary run was satisfied by
// a value only the -race run ever produced — and -race is routinely several times
// slower, which is exactly the size of discrepancy this package exists to catch.
// The pooling made the check agree with a number the stated command never
// printed, silently, which is the failure mode that is hardest to notice.
//
// A zero Run is a legitimate value meaning "this caller does not distinguish
// runs"; everything it records and asks about lives in one group, which is how
// this behaved before provenance existed.
type Run struct {
	Command string
	Args    []string
	Dir     string
}

// key identifies the run for grouping. Args are joined with a separator that
// cannot appear inside a single argument boundary ambiguously, so ["a b"] and
// ["a","b"] are different runs rather than the same one.
func (r Run) key() string {
	var b strings.Builder
	writeKeyField := func(value string) {
		b.WriteString(strconv.Itoa(len(value)))
		b.WriteByte(':')
		b.WriteString(value)
	}
	writeKeyField(r.Dir)
	writeKeyField(r.Command)
	b.WriteString(strconv.Itoa(len(r.Args)))
	b.WriteByte(':')
	for _, arg := range r.Args {
		writeKeyField(arg)
	}
	return b.String()
}

// snapshot severs the caller's ownership of argument backing storage. A Run is
// retained as provenance, so it must remain the same identity that produced the
// key even when a command builder reuses its argument slice for a later run.
func (r Run) snapshot() Run {
	r.Args = append([]string(nil), r.Args...)
	return r
}

// quoteRunPart keeps ordinary command lines readable while making argument
// boundaries unambiguous whenever whitespace, quotes, shell expansion, or
// control bytes would otherwise collapse two different argv values into the
// same label.
func quoteRunPart(part string) string {
	if part == "" {
		return strconv.Quote(part)
	}
	for _, r := range part {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case strings.ContainsRune("_./-:@%+=,", r):
		default:
			return strconv.Quote(part)
		}
	}
	return part
}

// Label renders the run for a reader. Empty for the zero Run, so a caller that
// does not distinguish runs gets the same wording it always had.
func (r Run) Label() string {
	if r.Command == "" && len(r.Args) == 0 && r.Dir == "" {
		return ""
	}
	parts := make([]string, 0, len(r.Args)+1)
	if r.Command != "" {
		parts = append(parts, quoteRunPart(r.Command))
	}
	for _, arg := range r.Args {
		parts = append(parts, quoteRunPart(arg))
	}
	label := strings.Join(parts, " ")
	if r.Dir != "" {
		if label != "" {
			label += " "
		}
		label += "(dir " + quoteRunPart(r.Dir) + ")"
	}
	return label
}

// ── one authoritative duration token ────────────────────────────────────────
//
// THREE UNANCHORED SEARCHES WERE THE ROOT CAUSE. Each regex hunted for its own
// suffix with no shared left boundary, so a failed outer match simply restarted
// inside the same token: ".86s" was read as 86s, ".5m" as 300s, "1,200ms" as
// 0.2s, and the valid Go compounds "1m10ms" and "1h1m500ms" fell through to their
// millisecond tails as 0.01s and 0.5s. Every one of those turns an honest claim
// into a fabricated correction, which is the single failure this package exists
// to prevent. Reported by @jatmn.
//
// A token is now recognised WHOLE or not at all, and one scanner answers for both
// callers — parseClaimedDuration and the clause scan — so the two can no longer
// disagree about what counts as a duration.

// durationUnits are ordered longest-first: "ms" has to be tried before "m", or
// every millisecond figure reads as minutes.
var durationUnits = []struct {
	suffix  string
	seconds float64
}{
	{"ms", 0.001},
	{"h", 3600},
	{"m", 60},
	{"s", 1},
}

// tokenLeftBoundary reports whether a duration may BEGIN at index.
//
// A digit, letter, dot or comma before the figure means this is the middle of
// something else — the fractional tail of ".86s", the grouped remainder of
// "1,200ms", or an identifier. None of those is a duration this package can read,
// and reading part of one is how a truthful sentence acquired a number it never
// contained.
func tokenLeftBoundary(text string, index int) bool {
	if index == 0 {
		return true
	}
	previous, _ := utf8.DecodeLastRuneInString(text[:index])
	switch {
	case previous >= '0' && previous <= '9':
		return false
	case previous >= 'a' && previous <= 'z', previous >= 'A' && previous <= 'Z':
		return false
	case previous == '.', previous == ',':
		return false
	case previous == '+', previous == '-', previous == '−':
		// A signed number is a delta or another numeric expression, not an
		// unsigned elapsed-time claim. Starting at the digit would discard the
		// sign and turn "improved by -4.20s" into a claim that the test took
		// positive 4.20 seconds.
		return false
	default:
		return true
	}
}

// tokenRightBoundary reports whether a duration may END at index.
func tokenRightBoundary(text string, index int) bool {
	if index >= len(text) {
		return true
	}
	switch next := text[index]; {
	case next >= '0' && next <= '9':
		return false
	case next >= 'a' && next <= 'z', next >= 'A' && next <= 'Z':
		return false
	case next == '.':
		// A sentence-ending dot is a boundary; a decimal point is not. Only a
		// digit after it makes it part of a number.
		return index+1 >= len(text) || text[index+1] < '0' || text[index+1] > '9'
	default:
		return true
	}
}

// scanNumber reads a plain or decimal figure at index, returning its end offset.
func scanNumber(text string, index int) int {
	at := index
	for at < len(text) && text[at] >= '0' && text[at] <= '9' {
		at++
	}
	if at == index {
		return index
	}
	if at < len(text) && text[at] == '.' {
		fraction := at + 1
		for fraction < len(text) && text[fraction] >= '0' && text[fraction] <= '9' {
			fraction++
		}
		if fraction > at+1 {
			at = fraction
		}
	}
	return at
}

// durationTokenAt reads a complete duration token beginning at index.
//
// Every component must be a figure followed by a unit, and the whole run must sit
// between token boundaries. A partial parse yields nothing rather than a
// second-best reading: an unsupported form is not evidence, and guessing at one
// is exactly how a fabricated number reaches the model.
func durationTokenAt(text string, index int) (end int, seconds float64, unitCount int, ok bool) {
	if !tokenLeftBoundary(text, index) {
		return 0, 0, 0, false
	}
	at := index
	for {
		numberEnd := scanNumber(text, at)
		if numberEnd == at {
			break
		}
		matched := false
		for _, unit := range durationUnits {
			if !strings.HasPrefix(text[numberEnd:], unit.suffix) {
				continue
			}
			value, err := strconv.ParseFloat(text[at:numberEnd], 64)
			if err != nil {
				return 0, 0, 0, false
			}
			seconds += value * unit.seconds
			at = numberEnd + len(unit.suffix)
			unitCount++
			matched = true
			break
		}
		if !matched {
			// A figure with no unit, or an unsupported one. The token is not a
			// duration, and no prefix of it is either.
			return 0, 0, 0, false
		}
	}
	if unitCount == 0 || !tokenRightBoundary(text, at) {
		return 0, 0, 0, false
	}
	return at, seconds, unitCount, true
}

// nextDurationToken finds the next complete duration token at or after start.
func nextDurationToken(text string, start int) (begin, end int, seconds float64, unitCount int, ok bool) {
	for at := start; at < len(text); at++ {
		if text[at] < '0' || text[at] > '9' {
			continue
		}
		if finish, value, units, valid := durationTokenAt(text, at); valid {
			return at, finish, value, units, true
		}
		// Skip the rest of this numeric run rather than re-entering it one digit
		// later, which is precisely how ".86s" became "86s".
		for at < len(text) && ((text[at] >= '0' && text[at] <= '9') || text[at] == '.' || text[at] == ',') {
			at++
		}
	}
	return 0, 0, 0, 0, false
}

// ParseGoTest pulls timings only from structured `go test -json` result events.
//
// Plain `go test -v` output is deliberately not accepted. The test process and
// the Go runner share that text stream, so a test can print a line shaped like
// `--- PASS: TestName (99.00s)` before the runner prints the real result. Under
// -json, test stdout is wrapped in an "output" event; admitting only
// pass/fail/skip events establishes which values the runner produced. Malformed
// or ordinary-text lines are silence, not evidence.
func ParseGoTest(text string) []Measurement {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	type event struct {
		Action  string   `json:"Action"`
		Package string   `json:"Package"`
		Test    string   `json:"Test"`
		Elapsed *float64 `json:"Elapsed"`
	}
	var out []Measurement
	for _, line := range strings.Split(text, "\n") {
		var item event
		if err := json.Unmarshal([]byte(line), &item); err != nil || item.Elapsed == nil {
			continue
		}
		switch item.Action {
		case "pass", "fail", "skip":
		default:
			continue
		}
		item.Test = strings.TrimSpace(item.Test)
		item.Package = strings.TrimSpace(item.Package)
		name := item.Test
		if name == "" {
			name = item.Package
		}
		if name == "" || math.IsNaN(*item.Elapsed) || math.IsInf(*item.Elapsed, 0) || *item.Elapsed < 0 {
			continue
		}
		out = append(out, Measurement{Name: name, Package: item.Package, Test: item.Test, Seconds: *item.Elapsed})
	}
	return out
}

// Ledger is every timing this run observed, and which conflicts it has already
// raised.
//
// A nil Ledger is a working no-op, so a caller that does not want the check
// holds nil and still calls every method unconditionally.
type Ledger struct {
	mu sync.Mutex
	// observed is keyed by run FIRST, so a lookup can only ever see the values
	// the asked-about command produced. Making that structural rather than a
	// filter applied at read time means a future caller cannot reintroduce the
	// pooling by forgetting to pass the run.
	observed map[string]map[measurementID][]float64
	runs     map[string]Run
	// raised keys on the name AND the value that was wrong, not the name alone.
	// Keying on the name switched the check off for that name permanently: after
	// one bad 4.20s, a later, differently wrong 9.90s for the same test was
	// silent. The point of the dedupe is that re-reading the SAME answer says
	// nothing twice, which per-value still gives, while a new wrong number is a
	// new thing to say.
	raised map[raisedKey]bool
}

// raisedKey identifies one conflict already reported: which name, and which
// claimed value. The value is rounded to milliseconds so that re-stating the
// same number in a different precision is still the same conflict.
type raisedKey struct {
	run string
	// acrossRuns separates the two entry points' bookkeeping. A cross-run report
	// is not about any single run, so keying it on one was wrong twice over: the
	// run it picked came from map iteration, which Go randomises, so the SAME
	// conflict was reported again on a later pass one time in four — and the
	// dedupe exists precisely so a correction fed back to the model cannot loop.
	acrossRuns   bool
	name         string
	claimedMilli int64
}

func newRaisedKey(run Run, name string, claimed float64) raisedKey {
	return raisedKey{run: run.key(), name: name, claimedMilli: claimedMilli(claimed)}
}

// newAcrossRunsKey identifies a conflict raised against every run at once.
// Independent of which run the report happens to quote, and in its own namespace
// so the two entry points cannot suppress each other's reports — they answer
// different questions, and a caller uses one or the other.
func newAcrossRunsKey(name string, claimed float64) raisedKey {
	return raisedKey{acrossRuns: true, name: name, claimedMilli: claimedMilli(claimed)}
}

func claimedMilli(claimed float64) int64 {
	return int64(math.Round(claimed * 1000))
}

func NewLedger() *Ledger {
	return &Ledger{
		observed: map[string]map[measurementID][]float64{},
		runs:     map[string]Run{},
		raised:   map[raisedKey]bool{},
	}
}

// ensureMaps makes a zero-value Ledger behave like a constructed one.
//
// ONE PLACE, NOT THREE. The nil receiver is handled separately, but a Ledger
// that was DECLARED rather than built by NewLedger reached the map writes with
// nil maps and panicked. The first attempt at this fixed the two maps Record
// touches and missed `raised`, which only Conflicts and ConflictsAcrossRuns
// write — and they write it exclusively on the contradiction path, so a test
// asking an agreeing claim never reached it. Reported by @jatmn.
//
// Every caller that can write a map calls this, and the field list lives beside
// NewLedger's, so a fourth map added later is a compile-time-visible omission in
// one spot rather than a panic in whichever entry point forgot it.
//
// Callers hold l.mu; this only fills nil fields, so a constructed Ledger pays
// three nil checks and nothing else.
func (l *Ledger) ensureMaps() {
	if l.observed == nil {
		l.observed = map[string]map[measurementID][]float64{}
	}
	if l.runs == nil {
		l.runs = map[string]Run{}
	}
	if l.raised == nil {
		l.raised = map[raisedKey]bool{}
	}
}

// Record reads any timings out of a command's output and remembers them against
// the run that produced it. Returns how many it took, which is what a test
// asserts on.
//
// Pass the command actually executed. A zero Run says this caller does not
// distinguish runs, which is a legitimate answer — but it is now said out loud at
// the call site rather than being the only thing the type could express.
func (l *Ledger) Record(run Run, text string) int {
	if l == nil {
		return 0
	}
	found := ParseGoTest(text)
	if len(found) == 0 {
		return 0
	}
	run = run.snapshot()
	l.mu.Lock()
	defer l.mu.Unlock()
	key := run.key()
	l.ensureMaps()
	byName := l.observed[key]
	if byName == nil {
		byName = map[measurementID][]float64{}
		l.observed[key] = byName
		l.runs[key] = run
	}
	for _, m := range found {
		id := m.identity()
		byName[id] = append(byName[id], m.Seconds)
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

func measurementDisplayNames(observed map[measurementID][]float64) (map[measurementID]string, map[string][]float64) {
	testOwners := map[string]int{}
	for id := range observed {
		if id.Test != "" {
			testOwners[id.Test]++
		}
	}
	candidates := make(map[measurementID]string, len(observed))
	owners := make(map[string]int, len(observed))
	for id := range observed {
		name := id.Package
		if id.Test != "" {
			name = id.Test
			if testOwners[id.Test] > 1 {
				name = id.Package + "." + id.Test
			}
		}
		if name == "" {
			continue
		}
		candidates[id] = name
		owners[name]++
	}
	names := make(map[measurementID]string, len(candidates))
	known := make(map[string][]float64, len(candidates))
	for id, name := range candidates {
		// Package results and package-qualified test results occupy distinct
		// identities but can render to the same text (package "example/a.TestFoo"
		// versus TestFoo in package "example/a"). Such a claim has no unique
		// owner, so fail silent rather than pooling values or emitting two
		// incompatible corrections.
		if owners[name] != 1 {
			continue
		}
		names[id] = name
		known[name] = append(known[name], observed[id]...)
	}
	return names, known
}

// Conflicts reports numbers in claim that contradict what this session recorded.
//
// ONLY NAMES THE LEDGER ALREADY KNOWS are considered, and only when the claim
// puts a duration next to one on the same line. An answer that mentions a test
// without timing it, or that reports something never measured here, produces
// nothing — this check exists to catch a number that DISAGREES with the
// transcript, not to demand that every number have one.
//
// Each name AND CLAIMED VALUE is reported at most once per Ledger. A second pass
// over the same answer is silent, so the caller can feed a correction back to the
// model without the possibility of a loop — while a differently wrong number for
// the same name is a new thing to say and is reported.
func (l *Ledger) Conflicts(run Run, claim string) []Conflict {
	if l == nil || strings.TrimSpace(claim) == "" {
		return nil
	}
	l.mu.Lock()
	l.ensureMaps()
	defer l.mu.Unlock()

	// ONLY THIS RUN'S VALUES. A claim about `go test ./...` is not answered by a
	// number that only `go test -race ./...` ever printed.
	key := run.key()
	observed := l.observed[key]
	if len(observed) == 0 {
		return nil
	}
	// Use the immutable provenance retained at Record time. A command builder
	// may reuse the caller's argv backing array after this method returns; a
	// later Nudge must still name the command that produced these observations.
	attributedRun, ok := l.runs[key]
	if !ok {
		attributedRun = run.snapshot()
	}
	names, known := measurementDisplayNames(observed)
	var out []Conflict
	for id, recorded := range observed {
		name := names[id]
		if name == "" {
			continue
		}
		if len(recorded) == 0 {
			continue
		}
		// EVERY MENTION, not the first that parsed. An agreeing occurrence used to
		// return before any later one was examined, so a fabricated second value
		// went unseen — and the per-value dedupe below exists precisely because
		// two different wrong numbers for one name are two findings.
		// ONE FINDING PER DISTINCT VALUE, within this call as well as across
		// calls. The ledger's dedupe is keyed on the value for a reason: the
		// same wrong number said twice is one thing to correct, while two
		// different wrong numbers are two.
		seenThisCall := map[int64]bool{}
		for _, claimed := range claimedSecondsAllFor(claim, name, known) {
			if l.raised[newRaisedKey(run, name, claimed)] || seenThisCall[claimedMilli(claimed)] {
				continue
			}
			seenThisCall[claimedMilli(claimed)] = true
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
			out = append(out, Conflict{Name: name, Claimed: claimed, Recorded: values, Run: attributedRun})
		}
	}
	// Deterministic order: this text reaches a model, and a set that reshuffles
	// between identical runs is a diff nobody can read.
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	for _, conflict := range out {
		l.raised[newRaisedKey(run, conflict.Name, conflict.Claimed)] = true
	}
	return out
}

// firstDurationEnd returns the offset just past the first duration token in the
// clause, or len(clause) when there is none.
func firstDurationEnd(clause string) int {
	if _, end, _, _, ok := nextDurationToken(clause, 0); ok {
		return end
	}
	return len(clause)
}

// claimedSecondsAllFor returns EVERY value the claim attributes to name, in the
// order they appear.
//
// THE SCALAR CONTRACT WAS THE DEFECT. Returning at the first occurrence that
// parsed meant an agreeing mention shielded every later one, so
// "TestFoo took 1.00s; TestFoo later took 9.00s" produced no conflict against a
// recorded 1s — the fabricated 9s was never examined. That also contradicted the
// ledger's per-VALUE dedupe, which exists precisely so two different wrong
// numbers for one name are two findings.
//
// Comparison and dedupe belong to the caller, which is why this returns values
// rather than a verdict: "later" is not a special case, and neither are two wrong
// values, repeated equivalent spellings, or more than two mentions. Reported by
// @jatmn.
func claimedSecondsAllFor(claim, name string, known map[string][]float64) []float64 {
	var values []float64
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
			clause := line[end:clauseEnd(line, end, known)]
			// TWO DURATIONS IN ONE CLAUSE MEANS OWNERSHIP IS UNCLEAR, so the
			// clause yields nothing.
			//
			// Position is not ownership. "nearest duration" was standing in for
			// "the duration asserted as this test's result", and the two part
			// company the moment a budget is stated first: with 0.86s recorded,
			// "TestQuick stayed under the 10s timeout and completed in 0.86s" was
			// reported as claiming 10s — a completely truthful sentence receiving
			// the exact fabricated correction this package exists to prevent.
			//
			// Deliberately NOT a "timeout" keyword exception: the same structure
			// arrives as deadlines, limits, budgets, targets and baselines, and a
			// word list would reopen the class at the next synonym. An ambiguous
			// clause is silent, which fails toward a miss rather than an
			// accusation. Reported by @jatmn.
			if _, _, _, _, second := nextDurationToken(clause, firstDurationEnd(clause)); second {
				continue
			}
			if value, ok := parseClaimedDuration(clause); ok {
				values = append(values, value)
			}
		}
	}
	return values
}

// clauseEnd returns the offset in line at which this name's clause stops.
//
// Three bounds, whichever comes first:
//
//   - the next RECORDED name, as a whole token. Every recorded name is
//     considered, including the one being searched for: a second mention starts
//     a second clause, and the caller's loop visits it on its own turn.
//   - the next name-SHAPED token, recorded or not. Cutting only at names the
//     ledger knows left "TestFoo passed; TestUnrecorded took 4.20s" fabricating
//     a conflict for TestFoo, because the neighbour holding the number was never
//     recorded and so never bounded anything. Whether a number belongs to this
//     name cannot depend on whether some OTHER name happened to be measured.
//   - a clause separator. "and", a semicolon or a comma end the clause as surely
//     as a new name does, and catch the neighbours that are not name-shaped.
//
// All three only ever SHORTEN the search, so each can cost a detection but none
// can invent one — the right direction for a check whose worst failure is
// accusing a correct number of being wrong.
func clauseEnd(line string, from int, known map[string][]float64) int {
	cut := len(line)
	consider := func(at int) {
		if at >= 0 && at < cut {
			cut = at
		}
	}
	for other := range known {
		if other == "" {
			continue
		}
		for start := from; start < len(line); {
			index := strings.Index(line[start:], other)
			if index < 0 {
				break
			}
			absolute := start + index
			start = absolute + 1
			if !nameBoundary(line, absolute, absolute+len(other)) {
				continue
			}
			consider(absolute)
			break
		}
	}
	if at := nextNameShaped(line, from); at >= 0 {
		consider(at)
	}
	for _, separator := range clauseSeparators {
		index := strings.Index(line[from:], separator)
		if index < 0 {
			continue
		}
		// A SEPARATOR BREAKS THE CLAUSE ONLY WHEN A NEW SUBJECT FOLLOWS IT.
		// Punctuation alone does not decide: "TestFoo (9.99s)",
		// "TestFoo passed - 9.99s" and "TestFoo passed, 9.99s" are all ways of
		// stating THIS test's own number, and bounding at the punctuation would
		// cut the number off from the name it belongs to and silence a real
		// fabrication. What makes it a break is a subject named after it —
		// "TestFoo passed (the suite took 34.249s)" — so the test is whether any
		// word appears between the separator and the next duration.
		after := line[from+index+len(separator):]
		// A CONJUNCTION AFTER A THRESHOLD DOES NOT PROVE NEW OWNERSHIP. Keeping the
		// threshold and following result in one clause lets the ambiguity guard
		// above refuse "under 10s and completed in 0.86s" instead of charging 10s
		// to the test. A plain result followed by another subject still ends here.
		if separator == " and " {
			before := line[from : from+index]
			_, _, _, _, afterDuration := nextDurationToken(after, 0)
			if durationHasThresholdContext(before) && afterDuration {
				continue
			}
		}
		if !separatorBreaksClause(after) {
			continue
		}
		consider(from + index)
	}
	consider(sentenceEnd(line, from))
	return cut
}

// durationHasThresholdContext recognizes the bounded threshold grammar this
// parser supports. The relationship is local to the duration: a comparative
// immediately before it or a threshold noun immediately after it. Merely
// finding one of these words elsewhere in the sentence is not enough.
func durationHasThresholdContext(text string) bool {
	begin, end, _, _, ok := nextDurationToken(text, 0)
	if !ok {
		return false
	}
	words := func(value string) []string {
		return strings.FieldsFunc(strings.ToLower(value), func(r rune) bool {
			return (r < 'a' || r > 'z') && (r < '0' || r > '9')
		})
	}
	before := words(text[:begin])
	after := words(text[end:])
	if len(after) > 0 {
		switch after[0] {
		case "timeout", "deadline", "budget", "limit", "target", "threshold":
			return true
		}
	}
	if len(before) > 0 {
		switch before[len(before)-1] {
		case "under", "within", "below":
			return true
		}
	}
	return len(before) >= 2 && before[len(before)-2] == "at" && before[len(before)-1] == "most"
}

// separatorBreaksClause reports whether the text after a separator names a new
// subject, rather than simply carrying the preceding name's own number.
//
// A word beside the next duration means something else is being talked about. No
// word — just the number — means the punctuation was only presentation, which is
// how a table, a bullet list or an aside states one test's timing.
//
// BESIDE, NOT BEFORE. The scan used to look only at what came ahead of the
// number, so a subject that trailed its own figure was invisible and the
// separator read as presentation:
//
//	recorded: TestFoo 0.10s
//	claim:    "TestFoo passed; 4.20s was the whole suite."
//	  -> [{Name:TestFoo Claimed:4.2 Recorded:[0.1]}]
//
// Every word of that claim is true and 4.20s belongs to the suite named directly
// after it. Which side of the figure its subject sits on says nothing about who
// owns it, so both sides are scanned.
//
// The trailing scan stops at the end of the figure's OWN segment, or ordinary
// layouts would break themselves: the words after "| 4.20s |" belong to the next
// cell, and the ones after ", 9.99s." to the next sentence.
func separatorBreaksClause(after string) bool {
	start, stop := nextDurationSpan(after)
	if start < 0 {
		// No duration after the separator at all, so there is no figure for a
		// word to sit beside: any word is a new subject.
		start, stop = len(after), len(after)
	}
	if containsLetter(after[:start]) {
		return true
	}
	// A TRAILING WORD KEEPS THE CLAUSE AMBIGUOUS, and that stays deliberate.
	//
	// @jatmn is right that this misses a real fabrication: "TestFoo passed, 9.90s
	// elapsed" reports nothing where the same sentence without "elapsed" is
	// caught, because containsLetter cannot tell a noun phrase that OWNS the
	// figure from a word that merely DESCRIBES it.
	//
	// I tried the fix he suggested first — recognise a subject rather than any
	// letter, using the same measurement-name layer the clause bound uses — and it
	// reopened the case this check exists for. "TestFoo passed; 4.20s was the
	// whole suite." and five siblings went straight back to charging the suite's
	// figure to the test, which is a FALSE ACCUSATION where the current behaviour
	// is only a miss. Measured, not reasoned: all six of the following-subject
	// tests failed.
	//
	// "the whole suite" and "elapsed" are both ordinary words. Separating them by
	// vocabulary is the qualifier allowlist he explicitly ruled out, and it would
	// reopen the same class at the next synonym. So the clause stays ambiguous,
	// which fails toward silence — this file's own comments say a miss is cheaper
	// than a fabricated correction, and that ordering has not changed. Closing the
	// miss needs an ownership model that reads structure rather than words, and I
	// do not have one that survives the six cases above.
	tail := after[stop:]
	return containsLetter(tail[:segmentEnd(tail)])
}

// segmentEnd returns the offset at which text stops belonging to the segment it
// starts — the first clause separator or sentence terminator, or the whole
// string when it holds neither.
func segmentEnd(text string) int {
	end := len(text)
	for _, separator := range clauseSeparators {
		if index := strings.Index(text, separator); index >= 0 && index < end {
			end = index
		}
	}
	if index := sentenceEnd(text, 0); index >= 0 && index < end {
		end = index
	}
	return end
}

func containsLetter(text string) bool {
	for index := 0; index < len(text); index++ {
		if c := text[index]; (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') {
			return true
		}
	}
	return false
}

// nextDurationSpan returns the bounds of the first duration in text that this
// package would actually read, or -1 when there is none.
//
// EVERY FORM THE PARSER READS, AND NO FORM IT REFUSES. Without the hour form this
// scan could not locate "9h" as a duration, so it read the "h" as the first
// letter of a new subject and turned the punctuation into a clause boundary,
// cutting a test's own number off from its name — "TestVerySlow - 9h",
// "…passed - 9h", "…(9h)" and "…: 9h" were all missed that way. The ambiguous
// bare unit below is the same requirement from the other side: a token
// parseClaimedDuration will not read is not a duration here either, or the two
// again disagree about where a clause ends.
//
// Only the leftmost match of each form is considered, so an ambiguous "5m"
// hides any later minute figure. That can only push the span later, which
// lengthens the scan and shortens the clause — the direction every bound here
// takes.
func nextDurationSpan(text string) (int, int) {
	// THE SAME SCANNER THE PARSER USES. These were separate heuristics, so a token
	// the parser refused could still bound a clause and vice versa — the two
	// disagreeing about what a duration is was its own defect class.
	begin, end, _, units, ok := nextDurationToken(text, 0)
	if !ok {
		return -1, -1
	}
	if units == 1 && bareUnitFollowedByWord(text, begin, end) {
		return -1, -1
	}
	return begin, end
}

// clauseSeparators end a measurement clause without starting a new name.
//
// CLAUSE PUNCTUATION IS A CLOSED SET, which is what makes enumerating it
// finishable — unlike the ways a person can phrase an admission. Walking the
// shapes rather than guessing found five that leaked, not one: the plain hyphen
// everyone types, both typographic dashes, a parenthetical and a pipe. Each let
// a following clause's number be charged to the name in front of it.
var clauseSeparators = []string{
	";", ":", ",", "(", "|", "—", "–",
	" - ", " -- ",
	" and ", " but ", " while ", " whereas ", " though ",
}

// sentenceEnd returns the offset of the first sentence terminator at or after
// from, or -1.
//
// A FULL STOP ENDS A CLAUSE. The separator list had none, so when the next
// sentence's subject was an ordinary noun phrase — not a recorded name and not
// name-shaped — nothing bounded the clause and its number was charged to the
// previous sentence's test:
//
//	"TestNested is green. The full run took 34.249s."
//	  -> [{Name:TestNested Claimed:34.249 Recorded:[0.03]}]
//
// Both sentences are true, both numbers were really measured, and the writer
// attached each to the right subject. Accusing a correct report is the failure
// that gets a tripwire switched off.
//
// A decimal point is not a terminator, and neither is the dot inside an import
// path: a terminator is followed by whitespace or the end of the line, and never
// sits between two digits.
func sentenceEnd(line string, from int) int {
	for index := from; index < len(line); index++ {
		switch line[index] {
		case '.', '!', '?':
		default:
			continue
		}
		if line[index] == '.' && index > 0 && index+1 < len(line) &&
			isDigitByte(line[index-1]) && isDigitByte(line[index+1]) {
			continue
		}
		if index+1 >= len(line) || line[index+1] == ' ' || line[index+1] == '\t' {
			return index
		}
	}
	return -1
}

func isDigitByte(b byte) bool { return b >= '0' && b <= '9' }

// nextNameShaped returns the offset of the next token that looks like a name
// `go test` would print a timing for — a Test, Benchmark, Fuzz or Example
// function — or -1.
//
// SHAPE, not membership. The ledger only knows what this session measured, and a
// claim may name a test that was never run; that name still ends the clause it
// starts, because the number after it belongs to it and not to the name before.
//
// THE PREFIXES ARE CAPITALISED, and the first version's were not. Nothing here
// lowercases the claim — unlike the guardrails, this package compares against
// recorded names and must preserve their case — so "test" never matched
// "TestUnrecorded" and this function returned -1 for every realistic input. The
// tests that were supposed to cover it all contained a comma or a semicolon, so
// the clause-separator bound caught them and the dead branch looked alive. A
// bleed with no separator at all went straight through.
//
// The character after the prefix must be uppercase, a digit or an underscore,
// which is how `go test` spells these names and what separates TestFoo from the
// ordinary words "test", "testing" and "tested".
func nextNameShaped(line string, from int) int {
	for index := from; index < len(line); index++ {
		if index > from && !isNameSeparator(line[index-1]) {
			continue
		}
		rest := line[index:]
		for _, prefix := range []string{"Test", "Benchmark", "Fuzz", "Example"} {
			if len(rest) <= len(prefix) || !strings.HasPrefix(rest, prefix) {
				continue
			}
			if next := rest[len(prefix)]; !isNameContinuation(next) {
				continue
			}
			return index
		}
		if packagePathAt(line, index) {
			return index
		}
	}
	return -1
}

// packagePathAt reports whether a package path begins at index.
//
// A PACKAGE IS A MEASUREMENT SUBJECT TOO, and it used to be recognised only when
// that exact package had already been recorded. So a truthful
// "github.com/x/first passed github.com/x/unrecorded took 4.20s" charged the
// second package's figure backwards to the first, because the unrecorded
// neighbour did not bound the clause — while an unrecorded TEST-shaped name
// always did. Whether a neighbouring subject ends a clause cannot depend on
// whether that neighbour happened to produce a parseable timing. Reported by
// @jatmn.
//
// Deliberately narrow: at least one slash, and every segment made of the
// characters an import path may contain. Prose rarely looks like this, and the
// consequence of a miss is the pre-existing behaviour rather than a new one.
func packagePathAt(line string, index int) bool {
	end := index
	slashes := 0
	for end < len(line) {
		c := line[end]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '.', c == '-', c == '_':
		case c == '/':
			slashes++
		default:
			goto done
		}
		end++
	}
done:
	if slashes == 0 || end == index {
		return false
	}
	// A trailing dot is sentence punctuation, not part of the path.
	segment := line[index:end]
	return !strings.HasPrefix(segment, "/") && !strings.HasSuffix(segment, "/")
}

// isNameContinuation reports whether b continues a go test name rather than
// ending the word.
func isNameContinuation(b byte) bool {
	return (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9') || b == '_'
}

func isNameSeparator(b byte) bool {
	switch b {
	case ' ', '\t', ';', ',', '(', ')', '[', ']', '`', '"', '\'', '-', '*':
		return true
	}
	return false
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
	continues := func(r rune) bool {
		// Test names are Go identifiers and may contain Unicode letters and
		// digits. Conservatively treating every non-ASCII rune (including invalid
		// UTF-8 decoded as RuneError) as a continuation makes an uncertain match
		// fail silent rather than attributing a longer foreign name to an ASCII
		// prefix.
		if r >= utf8.RuneSelf {
			return true
		}
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return true
		case r == '_', r == '/', r == '.', r == '-', r == '#':
			return true
		}
		return false
	}
	if from > 0 {
		before, _ := utf8.DecodeLastRuneInString(line[:from])
		if continues(before) {
			return false
		}
	}
	if to < len(line) {
		after, _ := utf8.DecodeRuneInString(line[to:])
		if continues(after) {
			return false
		}
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
//
// AND POSITION IS ONLY WORTH DECIDING BETWEEN REAL DURATIONS. A bare "5m" or "9h"
// carrying a word — "handled 5m rows" — may be a count rather than a duration,
// and winning on position is exactly how it took precedence over the timing
// beside it. bareUnitIsAmbiguous holds that shape back, and its clause then
// reports nothing at all rather than a guess.
func parseClaimedDuration(tail string) (float64, bool) {
	begin, end, seconds, units, ok := nextDurationToken(tail, 0)
	if !ok {
		return 0, false
	}
	// AMBIGUITY IS STILL SILENCE, NOT A SECOND-BEST READING. A bare "5m" or "9h"
	// with a word directly after it may be a count rather than a duration
	// ("handled 5m rows"), and reaching past it to a later figure would answer the
	// same question by guessing.
	if units == 1 && bareUnitFollowedByWord(tail, begin, end) {
		return 0, false
	}
	return seconds, true
}

// bareUnitFollowedByWord reports whether a single-component token is the
// ambiguous count shape: one figure, one unit letter, and a word beside it. A
// compound form cannot be a count, and a bare figure with nothing after it has no
// noun to be counting.
func bareUnitFollowedByWord(text string, begin, end int) bool {
	if end-begin == 0 {
		return false
	}
	switch text[end-1] {
	case 'm', 'h':
	default:
		return false
	}
	at := end
	for at < len(text) && text[at] == ' ' {
		at++
	}
	if at == end || at >= len(text) {
		return false
	}
	letter := text[at]
	return (letter >= 'a' && letter <= 'z') || (letter >= 'A' && letter <= 'Z')
}

// sortedRunKeys walks the recorded runs in an order that does not depend on how
// the map was built.
//
// THE RUN QUOTED IS CHOSEN DETERMINISTICALLY. Map iteration order is randomised
// per range, and a merged sighting keeps the first run that recorded the name, so
// taking whichever came first made the nudge name a different command between
// identical passes — and this text reaches a model, where a message that
// reshuffles is the same problem the sorted output exists to avoid. The lowest
// run key wins, which is stable.
//
// NOTHING DOWNSTREAM CAN SEE THIS ORDER TODAY, and that is why it is a named
// function. A name recorded by several runs has its label dropped as untrue of
// any one of them, and a name recorded by one run has a single candidate, so the
// choice is forced either way and every assertion about it passes whatever this
// returns — removing the sort entirely leaves the suite green. A bound with no
// live witness is the shape that rots, so it is tested here directly instead of
// being certified by accident.
func sortedRunKeys(observed map[string]map[measurementID][]float64) []string {
	keys := make([]string, 0, len(observed))
	for key := range observed {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// ConflictsAcrossRuns reports numbers in claim that contradict EVERY run this
// session recorded.
//
// For a caller that cannot say which run a claim is about — the agent loop
// checking a final answer that may summarise several commands — this is the
// honest question to ask. A value the model could have read off any of them is
// not evidence of invention, and accusing it of inventing a number one of the
// commands really printed is the failure this package must never produce.
//
// Callers that DO know the command should use Conflicts, which holds the claim
// to that run's own numbers: a claim about an ordinary run is not answered by a
// value only `go test -race` printed. The difference between the two is not a
// convenience, it is how much the caller actually knows, so it is two functions
// rather than a flag.
//
// The Conflict reports the run whose values it quotes, picking the run with a
// recording for that name so the nudge names a command the model can repeat.
func (l *Ledger) ConflictsAcrossRuns(claim string) []Conflict {
	if l == nil || strings.TrimSpace(claim) == "" {
		return nil
	}
	l.mu.Lock()
	l.ensureMaps()
	defer l.mu.Unlock()

	// Names are gathered across runs first, so a name recorded by two commands
	// is considered once against everything either of them saw.
	type sighting struct {
		values []float64
		run    Run
		runs   int
	}
	keys := sortedRunKeys(l.observed)

	merged := map[measurementID]*sighting{}
	for _, key := range keys {
		for id, values := range l.observed[key] {
			seen := merged[id]
			if seen == nil {
				seen = &sighting{run: l.runs[key]}
				merged[id] = seen
			}
			seen.values = append(seen.values, values...)
			seen.runs++
		}
	}
	mergedValues := make(map[measurementID][]float64, len(merged))
	for id, seen := range merged {
		mergedValues[id] = seen.values
	}
	displayNames, known := measurementDisplayNames(mergedValues)

	var out []Conflict
	for id, seen := range merged {
		name := displayNames[id]
		if name == "" {
			continue
		}
		// EVERY MENTION, for the same reason as the per-run path above.
		seenThisCall := map[int64]bool{}
		for _, claimed := range claimedSecondsAllFor(claim, name, known) {
			if l.raised[newAcrossRunsKey(name, claimed)] || seenThisCall[claimedMilli(claimed)] {
				continue
			}
			seenThisCall[claimedMilli(claimed)] = true
			agrees := false
			for _, value := range seen.values {
				if tolerance(claimed, value) {
					agrees = true
					break
				}
			}
			if agrees {
				continue
			}
			values := append([]float64(nil), seen.values...)
			sort.Float64s(values)
			// NAME A COMMAND ONLY WHEN ONE COMMAND PRINTED ALL OF THESE. Merging two
			// runs' values and then labelling the union with one of them said that
			// command reported a number it never printed:
			//
			//	`go test ./a` in this session reported 0.1s, 0.2s
			//
			// where 0.2s came only from ./b. Choosing the run deterministically fixed
			// the reshuffling and left the attribution just as untrue. With more than
			// one run behind the values the label is dropped, and Nudge falls back to
			// naming the session rather than a command.
			attributed := seen.run
			if seen.runs > 1 {
				attributed = Run{}
			}
			out = append(out, Conflict{Name: name, Claimed: claimed, Recorded: values, Run: attributed})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	for _, conflict := range out {
		l.raised[newAcrossRunsKey(conflict.Name, conflict.Claimed)] = true
	}
	return out
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
		b.WriteString("; ")
		if label := conflict.Run.Label(); label != "" {
			b.WriteString("`")
			b.WriteString(label)
			b.WriteString("` in this session reported ")
		} else {
			b.WriteString("the commands actually run in this session reported ")
		}
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
