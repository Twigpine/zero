package redaction

import (
	"strings"
	"testing"
)

// A GREEDY BODY SWALLOWS THE NEXT CREDENTIAL'S TRIGGER, SO THE END-CHAIN
// NEVER SEES IT.
//
// The anchored chain re-binds a redacted span at its END; it can only catch a
// credential that begins exactly where the previous one stops. That is true
// for fixed-length bodies (AKIA{16} stops at the next key) and false for
// unbounded ones: github's body class [A-Za-z0-9] happily eats the literal
// "ghp" of the token glued behind it, so the first match ends past the second
// trigger and the plain chain has nothing left to match. Every unbounded shape
// below leaked its second credential before the swallowed-trigger cut.
func TestRedactStringRedactsCredentialsSwallowedByAGluedRun(t *testing.T) {
	// Assembled so no secret-shaped literal appears in source.
	// gitleaks:allow -- synthetic redaction fixtures below
	firstGitHub := "ghp_" + strings.Repeat("a", 36)
	secondGitHub := "ghp_" + strings.Repeat("b", 36)

	t.Run("two github tokens glued end to end", func(t *testing.T) {
		got := RedactString(firstGitHub+secondGitHub, Options{})
		if got != RedactedSecret+RedactedSecret {
			t.Fatalf("second token survived a glued github pair: got %q", got)
		}
	})

	t.Run("two JWTs glued end to end", func(t *testing.T) {
		got := RedactString(jwtToken+otherJWT, Options{})
		if got != RedactedSecret+RedactedSecret {
			t.Fatalf("second token survived a glued JWT pair: got %q", got)
		}
	})

	t.Run("third github token in the same run", func(t *testing.T) {
		thirdGitHub := "ghp_" + strings.Repeat("c", 36)
		got := RedactString(firstGitHub+secondGitHub+thirdGitHub, Options{})
		if got != strings.Repeat(RedactedSecret, 3) {
			t.Fatalf("glued github run left residue between markers: got %q", got)
		}
	})
}

// THE SWALLOWED TRIGGER CAN BELONG TO A DIFFERENT SHAPE. The chained run from
// a cut must try every shape at each boundary, not only the one that matched
// the swallowed span.
//
// Two outcomes are both fully redacted and the tests accept either. A github
// body stops at the hyphen an anthropic trigger carries, so the cut recovers
// the boundary and each key becomes its own marker. An anthropic body class
// spans [A-Za-z0-9_-], wide enough to cover the next trigger and the
// credential behind it whole, so the single match already consumed them and
// the requirement is only that nothing of either key survives.
func TestRedactStringRedactsACrossShapedCredentialGluedOntoARun(t *testing.T) {
	// gitleaks:allow -- synthetic redaction fixtures below
	github := "ghp_" + strings.Repeat("a", 36)
	anthropic := "sk-ant-" + strings.Repeat("b", 20)
	secondAnthropic := "sk-ant-" + strings.Repeat("d", 20)

	// keptOut fails when any trigger or credential-shaped residue survives the
	// redaction, the one property a glued run can never relax.
	keptOut := func(t *testing.T, got string, pieces ...string) {
		t.Helper()
		for _, mark := range []string{"sk-ant-", "ghp_"} {
			if strings.Contains(got, mark) {
				t.Fatalf("a trigger survived a glued run: %q in %q", mark, got)
			}
		}
		for _, piece := range pieces {
			if strings.Contains(got, piece) {
				t.Fatalf("a credential survived a glued run: parts of %q… in %q", piece[:8], got)
			}
		}
	}

	t.Run("anthropic glued behind github", func(t *testing.T) {
		value := github + anthropic
		got := RedactString(value, Options{})
		if got != RedactedSecret+RedactedSecret {
			t.Fatalf("anthropic key survived behind a github key: got %q", got)
		}
		keptOut(t, got, github, anthropic)
	})

	t.Run("anthropic glued behind anthropic, then github", func(t *testing.T) {
		value := anthropic + secondAnthropic + github
		got := RedactString(value, Options{})
		if got == value {
			t.Fatal("SETUP INVALID: the glued run is not redacted at all")
		}
		keptOut(t, got, anthropic, secondAnthropic, github)
	})

	t.Run("github glued behind anthropic", func(t *testing.T) {
		value := anthropic + github
		got := RedactString(value, Options{})
		if got == value {
			t.Fatal("SETUP INVALID: the glued run is not redacted at all")
		}
		keptOut(t, got, anthropic, github)
	})
}

// THE CUT MUST NOT WEAKEN WHAT ALREADY WORKED.
//
// A trigger spelled inside a matched span that starts no credential there
// leaves the span whole: the bytes before it do not re-validate as their
// shape, and the run chained from it stays inside the span. The end-chaining
// for fixed-length bodies and every single-credential behavior the suite
// already pins must be untouched.
func TestRedactStringSwallowedTriggerCutKeepsExistingBehavior(t *testing.T) {
	// gitleaks:allow -- synthetic redaction fixtures below
	github := "ghp_" + strings.Repeat("a", 36)

	t.Run("two fixed-length AWS keys still chain at the end", func(t *testing.T) {
		got := RedactString("AKIAIOSFODNN7EXAMPLE"+"ASIAIOSFODNN7EXAMPLE", Options{})
		if got != RedactedSecret+RedactedSecret {
			t.Fatalf("glued AWS keys regressed: got %q", got)
		}
	})

	t.Run("an interior trigger spelling starts no credential of its own", func(t *testing.T) {
		// A JWT payload can contain "eyJ" away from a segment start. The strict
		// form needs the segment to open with it and the loose form needs the
		// three-segment skeleton, so a payload fragment is prose to both and the
		// token stays one marker.
		token := "eyJhbGciOiJIUzI1NiJ9." + "aaaaeyJaaaaaa" + ".QWxpY2VCb2JDYXJvbA"
		if got := RedactString(token, Options{}); got != RedactedSecret {
			t.Fatalf("a JWT with an interior trigger spelling changed shape: got %q", got)
		}
	})

	t.Run("a single token is one marker, not two", func(t *testing.T) {
		got := RedactString(github, Options{})
		if got != RedactedSecret {
			t.Fatalf("a lone token changed shape: got %q", got)
		}
	})
}

// A SPAM OF TRIGGER SPELLINGS INSIDE ONE BODY MUST COST WHAT THE SPAN COSTS.
//
// Every trigger occurrence inside a matched span is a cut candidate, but only
// the first maxTriggerOccurrences are examined, so the scan stays linear in
// the span: a body stuffed with lookalikes must not walk the span once per
// candidate. Measured as allocation against the same bar the split path's
// linearity test uses, the one an index-per-byte or a walk-per-credential
// explosion offends.
func TestRedactStringTriggerLookalikeSpamStaysBounded(t *testing.T) {
	// No digit appears anywhere in the fixture, so the openai shape's filter
	// rejects it as kebab-case prose and the anthropic shape still matches it
	// whole: every "sk-ant-" inside the span is a cut candidate, and none
	// validates because the run chained from any of them reaches only to the
	// span's own end.
	// gitleaks:allow -- synthetic redaction fixture below
	value := "sk-ant-" + strings.Repeat("sk-ant-", (64<<10)/len("sk-ant-"))
	if got := RedactString(value, Options{}); got != RedactedSecret {
		t.Fatalf("SETUP INVALID: the lookalike span was not redacted whole: got %q", got)
	}
	// Sixty-four candidates, each re-checked against the span, cost a small
	// constant multiple of the span. A walk over every occurrence quadruples
	// the cost when the span doubles, and the split path measured that walk
	// at seventeen gigabytes on a one-megabyte chain.
	limit := int64(200 * len(value))
	if used := redactAllocBytes(value); used > limit {
		t.Fatalf("redacting a %d byte lookalike span allocated %d bytes, over the %d byte limit", len(value), used, limit)
	}
}
