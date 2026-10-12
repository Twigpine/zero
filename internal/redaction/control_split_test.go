package redaction

import (
	"fmt"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"unicode"
)

const (
	awsKey       = "AKIAIOSFODNN7EXAMPLE"
	anthropicKey = "sk-ant-api03-aaaaaaaaaaaaaaaaaaaaaaaa0123456789ABCD"
	githubKey    = "ghp_cccccccccccccccccccccccccccccccccccc"
	openaiKey    = "sk-proj-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// rejoin drops every character a reader would never see, which is what a
// terminal, a log viewer or a copy-paste does to this output. If the secret is
// back after that, the redaction did not happen.
func rejoin(value string) string {
	return strings.Map(func(r rune) rune {
		if splitSecretSeparator(r) {
			return -1
		}
		return r
	}, value)
}

// AN INVISIBLE BYTE INSIDE A KEY MUST NOT END THE MATCH.
//
// Every shape matcher describes a contiguous body, so one NUL or ESC in the
// middle of a key ended the match and both fragments were emitted. Dropping the
// byte — which is what the next reader does, because it was never displayed —
// put the original key back. Reported in #969 for NUL and ESC; this covers the
// classes those two belong to, because every member behaves the same way.
func TestRedactStringRedactsSecretsSplitByInvisibleCharacters(t *testing.T) {
	separators := map[string]string{
		"NUL":               "\x00",
		"ESC":               "\x1b",
		"BEL":               "\x07",
		"backspace":         "\x08",
		"vertical tab":      "\x0b",
		"form feed":         "\x0c",
		"DEL":               "\x7f",
		"C1 NEL":            "\u0085",
		"C1 CSI":            "\u009b",
		"zero width space":  "\u200b",
		"zero width joiner": "\u200d",
		"word joiner":       "\u2060",
		"byte order mark":   "\ufeff",
		"soft hyphen":       "\u00ad",
	}
	// Every entry has to BE a separator, or its leg passes vacuously: inserting an
	// ordinary character into a key also stops the secret matching, and the rejoin
	// below would then find nothing. An escape mangled on the way into this file is
	// exactly how that happens.
	for name, separator := range separators {
		if runes := []rune(separator); len(runes) != 1 || !splitSecretSeparator(runes[0]) {
			t.Fatalf("SETUP INVALID: %s is %q, which is not a single invisible separator", name, separator)
		}
	}
	secrets := map[string]string{"aws": awsKey, "anthropic": anthropicKey, "github": githubKey, "openai": openaiKey}
	for secretName, secret := range secrets {
		// The premise: the unsplit form already redacts, so a failure below is
		// about the separator and not about the shape being unknown.
		if out := RedactString(secret, Options{}); strings.Contains(out, secret) {
			t.Fatalf("SETUP INVALID: the unsplit %s key is not redacted at all: %q", secretName, out)
		}
		for sepName, separator := range separators {
			for _, at := range []int{1, len(secret) / 2, len(secret) - 1} {
				split := secret[:at] + separator + secret[at:]
				out := RedactString(split, Options{})
				if got := rejoin(out); strings.Contains(got, secret) {
					t.Errorf("%s key split by %s at byte %d survives: %q rejoins to %q", secretName, sepName, at, out, got)
				}
			}
		}
	}
}

// The replacement has to cover the ORIGINAL bytes, separators included. Leaving
// the separator behind would emit `[REDACTED]\x00[REDACTED]` and tell a reader
// there were two secrets, and leaving either fragment behind is the leak itself.
func TestRedactStringCoversTheSeparatorsInsideASplitSecret(t *testing.T) {
	for _, testCase := range []struct {
		name  string
		value string
		want  string
	}{
		{"one separator", "key=" + awsKey[:8] + "\x00" + awsKey[8:], "key=[REDACTED]"},
		{"several separators", "key=" + strings.Join(strings.Split(awsKey, ""), "\x1b"), "key=[REDACTED]"},
		{"adjacent separators", "key=" + awsKey[:8] + "\x00\x1b\u200b" + awsKey[8:], "key=[REDACTED]"},
		{"two split secrets", awsKey[:8] + "\x00" + awsKey[8:] + " and " + githubKey[:8] + "\x00" + githubKey[8:], "[REDACTED] and [REDACTED]"},
		{"separator before the secret", "\x00" + awsKey, "\x00[REDACTED]"},
		{"separator after the secret", awsKey + "\x00", "[REDACTED]\x00"},
		{"text around it survives", "before " + awsKey[:4] + "\x00" + awsKey[4:] + " after", "before [REDACTED] after"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got := RedactString(testCase.value, Options{}); got != testCase.want {
				t.Errorf("RedactString(...) = %q, want %q", got, testCase.want)
			}
		})
	}
}

// NOTHING ELSE MOVES. The compaction exists to find a match; it must never
// reach text that has no secret in it, and text with no invisible character at
// all must take exactly the path it took before.
func TestRedactStringLeavesOrdinaryTextWithControlCharactersAlone(t *testing.T) {
	for _, value := range []string{
		"a plain sentence",
		"a sentence\x00with a NUL in it",
		"\x1b[31mcolored output\x1b[0m",
		"tab\tseparated\tcolumns",
		"line one\nline two\r\nline three",
		"zero\u200bwidth\u200bspaces",
		"", "\x00", "\x00\x00\x00",
		"not-a-secret-just-a-long-kebab-case-identifier-here",
		"sk-some-kebab-case-value-without-any-digits-at-all",
	} {
		if got := RedactString(value, Options{}); got != value {
			t.Errorf("RedactString(%q) = %q, want it unchanged", value, got)
		}
	}
}

// The openai filter that keeps kebab-case identifiers out of the redactor has
// to apply on the split path too, or working around the false positive would
// become a matter of inserting a control byte.
func TestSplitPathKeepsTheOpenAIKebabCaseFilter(t *testing.T) {
	kebab := "sk-some-kebab-case-value-without-digits"
	split := kebab[:6] + "\x00" + kebab[6:]
	if got := RedactString(split, Options{}); got != split {
		t.Errorf("a kebab-case identifier split by a NUL was redacted: %q", got)
	}
	// ... while a real key of the same family, split the same way, is not spared.
	realKey := openaiKey[:6] + "\x00" + openaiKey[6:]
	if got := RedactString(realKey, Options{}); strings.Contains(rejoin(got), openaiKey) {
		t.Errorf("a real openai key split by a NUL survived: %q", got)
	}
}

// TAB, NEWLINE AND CARRIAGE RETURN ARE OUT OF SCOPE ON PURPOSE, and this says
// so in a place that fails if someone changes it without meaning to. They are
// real text structure: stripping them would let one match span a line break and
// replace unrelated lines, and a credential broken across a line is visible to
// whoever reads it rather than hidden from them.
func TestLineStructureIsNotTreatedAsAnInvisibleSeparator(t *testing.T) {
	for name, separator := range map[string]string{"tab": "\t", "newline": "\n", "carriage return": "\r"} {
		if splitSecretSeparator([]rune(separator)[0]) {
			t.Errorf("%s is treated as an invisible separator; if that is now wanted, this test and the note in control_split.go both need to change", name)
		}
		split := awsKey[:8] + separator + awsKey[8:]
		if got := RedactString(split, Options{}); got != split {
			t.Errorf("%s split was redacted: %q. That may be an improvement, but it is a scope change this test is here to make deliberate", name, got)
		}
	}
}

// Malformed UTF-8 must not move the span mapping, which works in the source's
// own byte offsets. A decoder that measured an invalid byte as the three bytes
// of U+FFFD would shift every later span.
// The exact output is asserted, not just the absence of the key. An invalid
// byte measured as three would still cover the secret while moving the
// replacement's edges onto the bytes around it, which reads as a pass if the
// test only asks whether the secret is gone.
func TestSplitRedactionHandlesMalformedUTF8(t *testing.T) {
	for _, testCase := range []struct {
		name  string
		value string
		want  string
	}{
		{"invalid byte before the secret", "\xff" + awsKey[:8] + "\x00" + awsKey[8:], "\xff[REDACTED]"},
		// An invalid byte is NOT one of the separators: it decodes to U+FFFD and
		// is drawn, so a key broken by one is visible to whoever reads it, the
		// same argument that keeps tab and newline out. It is kept in the
		// compacted text and so it still ends the match.
		{"invalid byte inside the split", awsKey[:8] + "\x00\xff" + awsKey[8:], awsKey[:8] + "\x00\xff" + awsKey[8:]},
		{"invalid byte after the secret", awsKey[:8] + "\x00" + awsKey[8:] + "\xfe", "[REDACTED]\xfe"},
		{"invalid bytes on both sides", "\xff\xfe" + awsKey[:8] + "\x00" + awsKey[8:] + "\xfd\xfc", "\xff\xfe[REDACTED]\xfd\xfc"},
		{"invalid byte then text then the secret", "\xffpre " + awsKey[:8] + "\x00" + awsKey[8:] + " post", "\xffpre [REDACTED] post"},
		{"only invalid bytes", "\xff\xfe\x00\xff", "\xff\xfe\x00\xff"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			got := RedactString(testCase.value, Options{})
			if got != testCase.want {
				t.Errorf("RedactString(%q) = %q, want %q", testCase.value, got, testCase.want)
			}
			if strings.Contains(rejoin(got), awsKey) {
				t.Errorf("the key survived: %q", got)
			}
		})
	}
}

// The compaction is only a lens for finding the match. A string with no
// separator at all must come back byte-identical, including its invalid bytes.
func TestCompactionDoesNotRewriteTextItDoesNotRedact(t *testing.T) {
	for _, value := range []string{"plain", "with\xffinvalid", "tab\there", "\xef\xbb\xbfbom at the front"} {
		if got := redactControlSplitSecrets(value, RedactedSecret); got != value {
			t.Errorf("redactControlSplitSecrets(%q) = %q, want it unchanged", value, got)
		}
	}
}

// The separators the tests below need by name, built from their code points so
// no escape in this file can be mangled into the character it stands for.
var (
	nulSeparator  = string(rune(0x00))
	escSeparator  = string(rune(0x1b))
	zwspSeparator = string(rune(0x200b))
)

const (
	slackKey  = "xoxb-EXAMPLE-NOT-A-REAL-TOKEN-AAAAAAAAAA"
	googleKey = "AIzaSyD-0123456789abcdefghijklmnopqrstuv"
	jwtToken  = "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dBjftJeZ4CVPmB92K27uhbUJU1p1r0W1gFWFOEjXkPY"
	otherJWT  = "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiI5ODc2NTQzMjEwIn0.QWxpY2VCb2JDYXJvbERhdmVFdmVGcmFua0dyYWNlSA"
)

// splitInTheMiddle puts one separator inside the body of a secret.
func splitInTheMiddle(secret, separator string) string {
	half := len(secret) / 2
	return secret[:half] + separator + secret[half:]
}

// A SEPARATOR IN FRONT OF A KEY IS A DELIMITER, ONE INSIDE IT IS FILLER.
//
// #969 asks for one thing: the split form is redacted the same as the unsplit
// form. That is the whole assertion here, run over every kind of byte a key can
// follow. It catches the two ways of getting it wrong from opposite sides. Drop
// the separators and then ask the pattern for a word boundary, and a key written
// after a NUL stops redacting the moment a second separator lands inside it,
// even though the unsplit key after that same NUL redacts today. Ignore the
// boundary instead, and a key glued to the end of a word starts redacting when
// it is split, although the unsplit form never did.
func TestSplitRedactionMatchesTheUnsplitVerdictAtEveryLeadingBoundary(t *testing.T) {
	leaders := map[string]string{
		"start of string": "",
		"space":           "before ",
		"quote":           `{"k":"`,
		"equals":          "key=",
		"NUL":             "prefix" + nulSeparator,
		"ESC":             "prefix" + escSeparator,
		"zero width":      "prefix" + zwspSeparator,
		"letter":          "prefix",
		"digit":           "7",
		"underscore":      "field_",
	}
	secrets := map[string]string{"aws": awsKey, "github": githubKey, "openai": openaiKey, "slack": slackKey, "google": googleKey}
	for secretName, secret := range secrets {
		for leaderName, leader := range leaders {
			unsplit := leader + secret
			split := leader + splitInTheMiddle(secret, escSeparator)
			wantRedacted := !strings.Contains(RedactString(unsplit, Options{}), secret)
			out := RedactString(split, Options{})
			gotRedacted := !strings.Contains(rejoin(out), secret)
			if gotRedacted == wantRedacted {
				continue
			}
			if wantRedacted {
				t.Errorf("%s key after a %s leader: the unsplit form redacts and the split form does not: %q rejoins to %q",
					secretName, leaderName, out, rejoin(out))
				continue
			}
			t.Errorf("%s key after a %s leader: the unsplit form is left alone and the split form is redacted: %q",
				secretName, leaderName, out)
		}
	}
}

// A MATCH MUST NOT REACH OUT OF ONE CREDENTIAL AND INTO THE NEXT.
//
// The JWT shapes end on a run of body characters with no trailing boundary, so
// a matcher reading a copy with the separators removed can start in the key in
// front and run through the header of the JWT behind it, replacing both with one
// marker and eating the delimiter between them. The contiguous pass claims whole
// credentials before this one runs, which is what keeps them apart.
func TestSplitRedactionKeepsNeighbouringCredentialsApart(t *testing.T) {
	separators := map[string]string{"NUL": nulSeparator, "ESC": escSeparator, "zero width": zwspSeparator}
	leaders := map[string]string{
		"jwt":       jwtToken,
		"github":    githubKey,
		"anthropic": anthropicKey,
		"openai":    openaiKey,
		"slack":     slackKey,
		"google":    googleKey,
		"aws":       awsKey,
	}
	for leaderName, leader := range leaders {
		for sepName, separator := range separators {
			value := leader + separator + otherJWT
			want := RedactedSecret + separator + RedactedSecret
			if got := RedactString(value, Options{}); got != want {
				t.Errorf("a %s key %s a JWT: RedactString(...) = %q, want %q", leaderName, sepName, got, want)
			}
		}
	}
}

// splitThroughout puts a separator a quarter of the way into every dot-separated
// segment of a token. Early, so the part in front is too short to be a
// credential by itself and the strict pass cannot claim it; a split whose front
// part is complete alone is the documented limit, not what these tests are about.
func splitThroughout(secret, separator string) string {
	parts := strings.Split(secret, ".")
	for i, part := range parts {
		quarter := len(part) / 4
		parts[i] = part[:quarter] + separator + part[quarter:]
	}
	return strings.Join(parts, ".")
}

// TWO SPLIT CREDENTIALS OF ONE SHAPE ARE TWO CREDENTIALS.
//
// Matches of one pattern never overlap, so when the first one's body runs on
// through the separator between them, the second cannot start a match of its
// own. Whole neighbours are safe because the strict pass claims them first;
// these are both split. A JWT body stops at the next token's first dot and a
// classic GitHub body at the next token's underscore, which left the rest of the
// second credential in the clear, and the bodies that swallow the next key whole
// joined both keys and the separator between them into one replacement.
func TestSplitRedactionKeepsTwoSplitCredentialsOfOneShapeApart(t *testing.T) {
	separators := map[string]string{"NUL": nulSeparator, "ESC": escSeparator, "zero width": zwspSeparator}
	pairs := map[string][2]string{
		"jwt":       {jwtToken, otherJWT},
		"github":    {githubKey, "ghp_ffffffffffffffffffffffffffffffffffff"},
		"openai":    {openaiKey, "sk-proj-dddddddddddddddddddddddddddddddd"},
		"anthropic": {anthropicKey, "sk-ant-api03-eeeeeeeeeeeeeeeeeeeeeeee9876543210WXYZ"},
		"slack":     {slackKey, "xoxb-ANOTHER-NOT-REAL-TOKEN-BBBBBBBBBBBB"},
	}
	for name, pair := range pairs {
		for sepName, separator := range separators {
			value := splitThroughout(pair[0], escSeparator) + separator + splitThroughout(pair[1], escSeparator)
			want := RedactedSecret + separator + RedactedSecret
			if got := RedactString(value, Options{}); got != want {
				t.Errorf("two split %s credentials with %s between them: RedactString(...) = %q, want %q", name, sepName, got, want)
			}
		}
	}
}

// The search for where the next credential begins is bounded, so it has to be
// spent on positions where one can begin. Here the second token's header is split
// at every character inside the part the first match swallowed, which puts dozens
// of separator runs between the end of that match and the real boundary.
func TestSplitRedactionFindsTheNextCredentialPastItsOwnSplits(t *testing.T) {
	parts := strings.Split(otherJWT, ".")
	header := strings.Join(strings.Split(parts[0], ""), escSeparator)
	second := header + "." + strings.Join(parts[1:], ".")
	value := splitThroughout(jwtToken, escSeparator) + nulSeparator + second
	want := RedactedSecret + nulSeparator + RedactedSecret
	if got := RedactString(value, Options{}); got != want {
		t.Errorf("RedactString(...) = %q, want %q", got, want)
	}
}

// And a chain of them leaks nothing, however the markers fall.
func TestSplitRedactionLeavesNothingOfAChainOfSplitJWTs(t *testing.T) {
	chain := splitThroughout(jwtToken, escSeparator) + nulSeparator +
		splitThroughout(otherJWT, escSeparator) + nulSeparator +
		splitThroughout(jwtToken, zwspSeparator)
	out := rejoin(RedactString(chain, Options{}))
	for _, segment := range append(strings.Split(jwtToken, "."), strings.Split(otherJWT, ".")...) {
		if strings.Contains(out, segment) {
			t.Errorf("a JWT segment survived a chain of three split tokens: %q in %q", segment, out)
		}
	}
}

// A CHAIN OF SPLIT CREDENTIALS COSTS WHAT ITS LENGTH COSTS.
//
// The run-on search once followed a chain to its end from every match inside it,
// which is quadratic in the chain: a megabyte of split JWTs back to back
// allocated seventeen gigabytes and ran for ten minutes. Measured as allocation,
// which that walk multiplies with the chain and a linear pass does not.
func TestSplitRedactionStaysLinearOnAChainOfSplitJWTs(t *testing.T) {
	unit := splitThroughout(jwtToken, escSeparator) + nulSeparator
	chain := strings.Repeat(unit, (64<<10)/len(unit))
	signature := strings.Split(jwtToken, ".")[2]
	if strings.Contains(rejoin(RedactString(chain, Options{})), signature) {
		t.Fatal("SETUP INVALID: the chain is not redacted, so the run-on search never ran")
	}
	limit := int64(200 * len(chain))
	if used := redactAllocBytes(chain); used > limit {
		t.Errorf("redacting a %d byte chain of split JWTs allocated %d bytes, over the %d byte limit", len(chain), used, limit)
	}
}

// A MATCH THE FILTER REJECTS MUST NOT HIDE THE KEY IT RAN INTO.
//
// The OpenAI shape is filtered, because kebab-case with no digit in it is prose
// and not a key. A rejected match still spans the text it matched, and its body
// runs on through a separator into whatever follows, so a split key after a
// kebab-case name was rejected along with the name and printed in full.
func TestSplitRedactionDoesNotLetARejectedNeighbourHideASplitKey(t *testing.T) {
	const prose = "sk-some-kebab-case-name-here-ok"
	const key = "sk-ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefgh"
	if !strings.Contains(RedactString(prose, Options{}), prose) {
		t.Fatal("SETUP INVALID: the prose is redacted on its own, so it is not a rejected match")
	}
	if strings.Contains(RedactString(key, Options{}), key) {
		t.Fatal("SETUP INVALID: the key is not redacted even whole, so it is not a credential")
	}
	value := prose + nulSeparator + splitInTheMiddle(key, escSeparator)
	want := prose + nulSeparator + RedactedSecret
	if got := RedactString(value, Options{}); got != want {
		t.Errorf("RedactString(...) = %q, want %q", got, want)
	}
}

// ORDINARY TEXT AFTER A KEY IS NOT PART OF THE KEY. Removing the separator
// between them joins the word behind it onto the end of the key, and an
// unbounded shape then carries the replacement over text that was never secret.
func TestSplitRedactionLeavesTextBehindAKeyAlone(t *testing.T) {
	for _, testCase := range []struct {
		name  string
		value string
		want  string
	}{
		{"word", openaiKey + nulSeparator + "ordinary prose here", RedactedSecret + nulSeparator + "ordinary prose here"},
		{"escape then word", openaiKey + escSeparator + "ordinary", RedactedSecret + escSeparator + "ordinary"},
		{"zero width then word", githubKey + zwspSeparator + "ordinary", RedactedSecret + zwspSeparator + "ordinary"},
		{"digits", awsKey + nulSeparator + "0123456789", RedactedSecret + nulSeparator + "0123456789"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got := RedactString(testCase.value, Options{}); got != testCase.want {
				t.Errorf("RedactString(...) = %q, want %q", got, testCase.want)
			}
		})
	}
}

// THE OFFSET TABLE IS SIZED BY THE MATCHES, NOT BY THE TEXT IT SEARCHED.
//
// RedactString is handed whole command output, verification stdout and
// notification bodies, none of which is bounded before it runs, and an index
// carrying a start and an end for every source byte costs sixteen bytes per byte
// of input on a 64-bit build. Measured against the same text with no separator
// in it, so the regex work and the output copy cancel out and what is left is
// what the split path added.
func TestSplitRedactionDoesNotIndexEverySourceByte(t *testing.T) {
	const line = "plain log output line with no secret in it "
	filler := strings.Repeat(line, (1<<20)/len(line))
	split := filler + " " + splitInTheMiddle(awsKey, escSeparator)
	plain := filler + " " + awsKey
	if got := RedactString(split, Options{}); !strings.HasSuffix(got, RedactedSecret) {
		t.Fatalf("SETUP INVALID: the split key in the large input is not redacted, so nothing indexes it: %q", got[len(got)-64:])
	}
	// Four times the input still fails an index of two ints per source byte,
	// which needs sixteen, and clears the copy this path actually makes.
	limit := int64(4 * len(split))
	overhead := redactAllocBytes(split) - redactAllocBytes(plain)
	if overhead > limit {
		t.Errorf("redacting %d bytes with a split key allocated %d bytes more than the same text without one, over the %d byte limit",
			len(split), overhead, limit)
	}
}

// redactAllocBytes is the smallest number of bytes any of three RedactString
// calls allocated. Smallest rather than mean because a garbage collection or an
// unrelated goroutine can only ever add to the figure.
func redactAllocBytes(value string) int64 {
	smallest := int64(-1)
	for i := 0; i < 3; i++ {
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		out := RedactString(value, Options{})
		runtime.ReadMemStats(&after)
		if len(out) == 0 {
			panic("RedactString returned nothing")
		}
		used := int64(after.TotalAlloc - before.TotalAlloc)
		if smallest < 0 || used < smallest {
			smallest = used
		}
	}
	return smallest
}

// A REPLACEMENT ALREADY ON THE PAGE IS A BARRIER. The default marker is spelled
// with brackets, which no credential body admits, so it stops a match by
// itself. A caller is free to supply one made of ordinary word characters, and
// then the text left by the contiguous pass would read as the middle of a key
// and carry the replacement out over both delimiters and the text behind them.
func TestSplitRedactionStopsAtACallerSuppliedReplacement(t *testing.T) {
	options := Options{Replacement: "REDACTED"}
	value := "sk-aaaaa" + nulSeparator + awsKey + nulSeparator + "bbbbbbbbbbbbbbb"
	want := "sk-aaaaa" + nulSeparator + "REDACTED" + nulSeparator + "bbbbbbbbbbbbbbb"
	if got := RedactString(value, options); got != want {
		t.Errorf("RedactString(...) = %q, want %q", got, want)
	}
}

// A REJECTED MATCH MUST NOT SHADOW THE REAL KEY BEHIND IT. Raised by CodeRabbit
// on #1067. With the separators removed before matching, the leftmost AWS match
// in "xAKIA<NUL>AKIAIOSF<SOH>ODNN7EXAMPLE" began at the decoy AKIA, was rejected
// for the "x" in front of it, and had already consumed the start of the real key,
// so the real key was never tried. "ask-" in front of a split OpenAI key does the
// same through "sk-", which is the ordinary-prose version of it. Each input is
// checked against its unsplit counterpart, which main already redacts.
func TestSplitRedactionDoesNotLetARejectedMatchShadowTheRealKey(t *testing.T) {
	soh := string(rune(0x01))
	for _, testCase := range []struct {
		name    string
		split   string
		unsplit string
	}{
		{"decoy AKIA before a split key", "xAKIA" + nulSeparator + "AKIAIOSFODNN7E" + soh + "XAMPLE", "xAKIA" + nulSeparator + awsKey},
		{"ask- before a split OpenAI key", "ask-" + nulSeparator + splitInTheMiddle(openaiKey, soh), "ask-" + nulSeparator + openaiKey},
		{"task- before a split OpenAI key", "task-" + escSeparator + splitInTheMiddle(openaiKey, zwspSeparator), "task-" + escSeparator + openaiKey},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			want := RedactString(testCase.unsplit, Options{})
			if strings.Contains(want, awsKey) || strings.Contains(want, openaiKey) {
				t.Fatalf("SETUP INVALID: main does not redact the unsplit form either: %q", want)
			}
			if got := RedactString(testCase.split, Options{}); got != want {
				t.Errorf("RedactString(split) = %q, want the unsplit form's %q", got, want)
			}
		})
	}
}

// AN UNBOUNDED BODY MUST NOT TAKE A NEIGHBOUR'S PREFIX WITH IT. With gaps allowed
// between body characters, a split key's body runs on across a separator into
// whatever follows. When that is a JWT, it stops only at the JWT's first dot, and
// if the shapes were applied one after another the JWT shape would then find its
// header gone and leave the payload and signature in the clear. The shapes are
// matched against the same text and their union replaced, so it cannot.
func TestSplitRedactionKeepsANeighbourWhosePrefixASplitBodyWouldSwallow(t *testing.T) {
	cut := len(jwtToken) / 2
	splitJWT := jwtToken[:cut] + nulSeparator + jwtToken[cut:]
	for _, testCase := range []struct {
		name  string
		value string
	}{
		{"split Anthropic key, a word, then a split JWT", splitInTheMiddle(anthropicKey, zwspSeparator) + escSeparator + "build" + escSeparator + splitJWT},
		{"split GitHub key, then a split JWT", splitInTheMiddle(githubKey, nulSeparator) + escSeparator + splitJWT},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			got := rejoin(RedactString(testCase.value, Options{}))
			for _, piece := range strings.Split(jwtToken, ".") {
				if strings.Contains(got, piece) {
					t.Errorf("part of the JWT survives: %q in %q", piece, got)
				}
			}
		})
	}
}

// A CREDENTIAL GLUED ONTO A SPLIT ONE GETS ITS BOUNDARY FROM THE REPLACEMENT.
// Contiguous shapes cascade: once an AWS key is replaced, a JWT written straight
// after it has the "]" in front of it and matches. A split key is replaced by
// the second pass, so the JWT behind it needs a pass of its own after that one,
// and the JWT is often whole, which means its region may hold no separator at
// all by then. The second case adds a whole key elsewhere so that the strict
// pass has already cut the text into regions.
func TestSplitRedactionCascadesIntoACredentialGluedOntoASplitOne(t *testing.T) {
	csi := string(rune(0x9b))
	for _, testCase := range []struct {
		name  string
		value string
	}{
		{"whole JWT glued onto a split AWS key", splitInTheMiddle(awsKey, escSeparator) + jwtToken},
		{"same, with a whole key elsewhere in the text", "user?q=" + "ASIAIOSFODNN7EXA" + csi + "MPLE" + jwtToken + "'" + awsKey + string(rune(0x7f)) + "ok"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			got := rejoin(RedactString(testCase.value, Options{}))
			for _, piece := range strings.Split(jwtToken, ".") {
				if strings.Contains(got, piece) {
					t.Errorf("part of the JWT survives: %q in %q", piece, got)
				}
			}
		})
	}
}

// THE CLASS AND THE PREDICATE ARE ONE FACT. splitSeparatorClass is generated
// from the same tables as splitSecretSeparator; this walks every code point and
// fails on the first one they disagree about.
func TestSplitSeparatorClassAgreesWithThePredicate(t *testing.T) {
	class := regexp.MustCompile("^" + splitSeparatorClass + "$")
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if r >= 0xd800 && r <= 0xdfff {
			continue // surrogates are not valid in a Go string
		}
		if got, want := class.MatchString(string(r)), splitSecretSeparator(r); got != want {
			t.Fatalf("U+%04X: class matches = %v, predicate says %v", r, got, want)
		}
	}
}

// ON TEXT WITH NO SEPARATOR, EVERY GAP-TOLERANT SHAPE IS ITS ORIGINAL. The
// rewrite only adds optional gaps, so a contiguous key has to produce exactly the
// same match; anything else means the rewrite changed what a shape is.
func TestGapTolerantShapesMatchContiguousKeysLikeTheOriginals(t *testing.T) {
	inputs := []string{
		"key=" + awsKey + " and " + githubKey,
		"x" + awsKey,
		anthropicKey + "." + openaiKey,
		jwtToken + " then " + slackKey + ", " + googleKey,
		"sk-this-is-kebab-case-prose-not-a-key",
		"github_pat_11ABCDEFG0123456789_abcdefghijklmnopqrstuvwxyzABCDEFGH/glpat-abcdefghij0123456789",
	}
	originals := append([]*regexp.Regexp{openaiKeyPattern}, textSecretPatterns...)
	tolerant := append([]*regexp.Regexp{splitOpenAIPattern}, splitTextPatterns...)
	if len(originals) != len(tolerant) {
		t.Fatalf("SETUP INVALID: %d original shapes and %d gap-tolerant ones", len(originals), len(tolerant))
	}
	for i := range originals {
		for _, input := range inputs {
			want := fmt.Sprint(originals[i].FindAllStringIndex(input, -1))
			if got := fmt.Sprint(tolerant[i].FindAllStringIndex(input, -1)); got != want {
				t.Errorf("shape %s on %q: gap-tolerant matched %s, original %s", originals[i], input, got, want)
			}
		}
	}
}
