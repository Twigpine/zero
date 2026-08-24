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
	return r.Dir + "\x00" + r.Command + "\x00" + strings.Join(r.Args, "\x00")
}

// Label renders the run for a reader. Empty for the zero Run, so a caller that
// does not distinguish runs gets the same wording it always had.
func (r Run) Label() string {
	if r.Command == "" && len(r.Args) == 0 {
		return ""
	}
	return strings.TrimSpace(r.Command + " " + strings.Join(r.Args, " "))
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
	// seconds remainder. Every part is optional, so "2m", "1h10m0s", "1h30s" and
	// a bare "2h" all parse — a match is only accepted when an hour or minute
	// part is present, which is what separates this from the plain-seconds form.
	//
	// HOURS COUNT for the same reason minutes did. Without them "1h10m0s" matched
	// only its minute remainder and read as 600s, so a truthful restatement of a
	// recorded 4200s was reported as a conflict — the fabricated accusation this
	// package exists to avoid, one unit further up.
	//
	// THE WHOLE NUMBER, INCLUDING ITS FRACTION. An integer-only minute component
	// could not match "1.5m" at its start, so the leftmost match began at the
	// remainder instead and "1.5m" read as 5 minutes — and "0.5m", half a minute,
	// read as five. compoundPart already parses with ParseFloat, so the fraction
	// only had to be allowed into the capture for the match to start where the
	// number does.
	claimedMinuteDuration = regexp.MustCompile(`([0-9]+(?:\.[0-9]+)?)m(?:([0-9]+(?:\.[0-9]+)?)s)?\b`)
	// The hour form, kept as its OWN pattern rather than an optional prefix on the
	// minute one: every part optional makes the whole expression matchable by the
	// EMPTY string, which regexp then finds at offset 0 ahead of any real
	// duration — "1h10m0s" read as 0s that way, which is worse than the bug being
	// fixed. Minutes and seconds are optional here, so "2h", "1h30s" and
	// "1h10m0s" all parse.
	// Decimal components here for the same reason: "1.5h" read as 5 hours and
	// "10.25h" as 25.
	claimedHourDuration = regexp.MustCompile(`([0-9]+(?:\.[0-9]+)?)h(?:([0-9]+(?:\.[0-9]+)?)m)?(?:([0-9]+(?:\.[0-9]+)?)s)?\b`)
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
	mu sync.Mutex
	// observed is keyed by run FIRST, so a lookup can only ever see the values
	// the asked-about command produced. Making that structural rather than a
	// filter applied at read time means a future caller cannot reintroduce the
	// pooling by forgetting to pass the run.
	observed map[string]map[string][]float64
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
		observed: map[string]map[string][]float64{},
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
		l.observed = map[string]map[string][]float64{}
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
	l.mu.Lock()
	defer l.mu.Unlock()
	key := run.key()
	l.ensureMaps()
	byName := l.observed[key]
	if byName == nil {
		byName = map[string][]float64{}
		l.observed[key] = byName
		l.runs[key] = run
	}
	for _, m := range found {
		byName[m.Name] = append(byName[m.Name], m.Seconds)
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
	observed := l.observed[run.key()]
	if len(observed) == 0 {
		return nil
	}
	var out []Conflict
	for name, recorded := range observed {
		if len(recorded) == 0 {
			continue
		}
		claimed, ok := claimedSecondsFor(claim, name, observed)
		if !ok {
			continue
		}
		if l.raised[newRaisedKey(run, name, claimed)] {
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
		out = append(out, Conflict{Name: name, Claimed: claimed, Recorded: values, Run: run})
	}
	// Deterministic order: this text reaches a model, and a set that reshuffles
	// between identical runs is a diff nobody can read.
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	for _, conflict := range out {
		l.raised[newRaisedKey(run, conflict.Name, conflict.Claimed)] = true
	}
	return out
}

// claimedSecondsFor finds the duration an answer puts beside a name, searching
// this name's own clause on each line it appears on. Same line only: a number
// three paragraphs away is not this name's timing, and pairing them would invent
// a disagreement rather than find one.
//
// THE CLAUSE ENDS WHERE THE NEXT NAME BEGINS. Searching the whole remainder of
// the line let one name borrow another's number:
//
//	recorded: TestFoo 0.10s, TestBar 4.20s
//	claim:    "TestFoo passed; TestBar took 4.20s"
//	  -> [{Name:TestFoo Claimed:4.2 Recorded:[0.1]}]
//
// Every word of that claim is true. TestFoo reached past its own clause, took the
// number belonging to TestBar, and was told it had invented it — the same failure
// as reading a package total as a test's own timing, arrived at through the name
// binding rather than the pattern order. Cutting at the next known name is the
// bound, and the ledger is what knows those names, so they are passed in.
func claimedSecondsFor(claim, name string, known map[string][]float64) (float64, bool) {
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
			if value, ok := parseClaimedDuration(line[end:clauseEnd(line, end, known)]); ok {
				return value, true
			}
		}
	}
	return 0, false
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
		if !separatorBreaksClause(line[from+index+len(separator):]) {
			continue
		}
		consider(from + index)
	}
	consider(sentenceEnd(line, from))
	return cut
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
	start, stop := -1, -1
	take := func(match []int) {
		if match == nil {
			return
		}
		if start < 0 || match[0] < start {
			start, stop = match[0], match[1]
		}
	}
	take(claimedDuration.FindStringSubmatchIndex(text))
	if match := claimedMinuteDuration.FindStringSubmatchIndex(text); match != nil && !bareUnitIsAmbiguous(text, match, 2) {
		take(match)
	}
	if match := claimedHourDuration.FindStringSubmatchIndex(text); match != nil && !bareUnitIsAmbiguous(text, match, 2, 3) {
		take(match)
	}
	return start, stop
}

// bareUnitIsAmbiguous reports whether a compound-duration match is a bare "5m"
// or "5h" — a figure and a unit letter with no smaller component — carrying a
// word directly after it. subUnits names the smaller components' capture groups.
//
// AN "m" IS NOT ALWAYS MINUTES. "TestParseCorpus handled 5m rows in 0.86s" is an
// ordinary sentence, and the minute pattern reads its count of rows as five
// minutes. Position decides between the forms, so the count wins over the
// truthful 0.86s that follows it and the report is accused of stating 300s — a
// number its answer never contained, which is the one failure this package must
// never produce. A compound form ("1m10s", "2h30m") cannot be a count, and a bare
// figure with nothing after it ("took 2m", "| 2m |", "(9h)") has no noun to be
// counting, so the bare-plus-word shape is the whole of the ambiguity.
//
// AMBIGUOUS MEANS SILENT, NOT GUESSED. An ambiguous token is not evidence, so its
// clause yields no duration rather than a second-choice one — reaching past it to
// a later figure would decide the same question by guessing. The cost is real and
// stated plainly: "TestSlow took 2m to finish" is now unreadable, and a figure
// fabricated in that spelling goes uncaught. This package errs toward silence by
// design, and a missed number costs one detection while a false accusation costs
// the tripwire.
func bareUnitIsAmbiguous(text string, match []int, subUnits ...int) bool {
	for _, group := range subUnits {
		if match[group*2] >= 0 {
			return false
		}
	}
	for index := match[1]; index < len(text); index++ {
		switch c := text[index]; {
		case c == ' ' || c == '\t':
		case (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z'):
			return true
		default:
			return false
		}
	}
	return false
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
	}
	return -1
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

// startsFirst reports whether match begins no later than every other candidate.
// Ties go to the caller's match, which is how the hour form wins over the minute
// form inside "1h10m0s" — that string starts a minute match at "10m" only
// because the hour part came first.
func startsFirst(match []int, others ...[]int) bool {
	for _, other := range others {
		if other != nil && other[0] < match[0] {
			return false
		}
	}
	return true
}

// compoundPart reads one optional group of a compound duration match. An unset
// group is reported by regexp as index -1 rather than an empty span, which is
// how "1m" is told from "0m".
func compoundPart(tail string, match []int, group int) (float64, bool) {
	start, end := match[group*2], match[group*2+1]
	if start < 0 {
		return 0, false
	}
	value, err := strconv.ParseFloat(tail[start:end], 64)
	if err != nil {
		return 0, false
	}
	return value, true
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
	minute := claimedMinuteDuration.FindStringSubmatchIndex(tail)
	hour := claimedHourDuration.FindStringSubmatchIndex(tail)
	plain := claimedDuration.FindStringSubmatchIndex(tail)
	switch {
	case minute == nil && hour == nil && plain == nil:
		return 0, false
	case hour != nil && startsFirst(hour, minute, plain):
		if bareUnitIsAmbiguous(tail, hour, 2, 3) {
			return 0, false
		}
		hours, _ := compoundPart(tail, hour, 1)
		minutesPart, _ := compoundPart(tail, hour, 2)
		secondsPart, _ := compoundPart(tail, hour, 3)
		return hours*3600 + minutesPart*60 + secondsPart, true
	case minute != nil && (plain == nil || minute[0] <= plain[0]):
		if bareUnitIsAmbiguous(tail, minute, 2) {
			return 0, false
		}
		minutes, _ := compoundPart(tail, minute, 1)
		seconds, _ := compoundPart(tail, minute, 2)
		return minutes*60 + seconds, true
	}
	// Reachable only when the plain form matched — the guard above returns when
	// all three are nil, and each compound branch handles the cases where it
	// starts first. That is an argument, not a check, and this file has already
	// paid once for a fall-through whose precondition was only implied.
	if plain == nil {
		return 0, false
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
func sortedRunKeys(observed map[string]map[string][]float64) []string {
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

	merged := map[string]*sighting{}
	names := map[string][]float64{}
	for _, key := range keys {
		for name, values := range l.observed[key] {
			seen := merged[name]
			if seen == nil {
				seen = &sighting{run: l.runs[key]}
				merged[name] = seen
			}
			seen.values = append(seen.values, values...)
			seen.runs++
			names[name] = append(names[name], values...)
		}
	}

	var out []Conflict
	for name, seen := range merged {
		claimed, ok := claimedSecondsFor(claim, name, names)
		if !ok {
			continue
		}
		if l.raised[newAcrossRunsKey(name, claimed)] {
			continue
		}
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
