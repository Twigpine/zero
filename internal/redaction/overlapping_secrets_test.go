package redaction

import (
	"strings"
	"testing"
)

// A SECRET MUST NOT SURVIVE BECAUSE ANOTHER SECRET WAS REPLACED FIRST.
//
// Extra secret values are replaced one after another with ReplaceAll. When one
// configured value is a prefix of another, replacing the SHORT one first eats
// the head of the long one, and what is left of the long value no longer matches
// anything: the tail of a real credential is printed. The partial replacement
// also destroys the token shape, so the pattern passes further down cannot
// recover it either.
//
// Callers collect these values out of maps (MCP headers and env), and Go
// randomizes map iteration, so before the fix which of the two happened was
// decided fresh on every run. A test that fed them in one fixed order would have
// been green about half the time for the wrong reason, so both orders are
// asserted here.
func TestOverlappingSecretsAreRedactedWhicheverOrderTheyArrive(t *testing.T) {
	const short = "abcdefgh"
	const long = "abcdefghXYZ"
	message := "connect failed for tenant: " + long

	for _, testCase := range []struct {
		name   string
		values []string
	}{
		{name: "short value first", values: []string{short, long}},
		{name: "long value first", values: []string{long, short}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			got := RedactString(message, Options{ExtraSecretValues: testCase.values})
			if strings.Contains(got, "XYZ") {
				t.Errorf("the tail of the longer secret survived: %q", got)
			}
			if strings.Contains(got, short) {
				t.Errorf("the shorter secret survived: %q", got)
			}
			// The diagnostic half of the message has to be left alone, or
			// redaction has simply destroyed the error instead of cleaning it.
			if !strings.Contains(got, "connect failed for tenant:") {
				t.Errorf("redaction ate the diagnostic text: %q", got)
			}
		})
	}
}

// Order-independence has to hold for the OUTPUT too, not just for the absence of
// the secret. Two runs that redact the same message with the same values in a
// different order must produce the same string, or the panel and the transcript
// disagree about what a failure looked like depending on map iteration.
func TestOverlappingSecretRedactionIsOrderIndependent(t *testing.T) {
	values := []string{"tok_abcdefgh", "tok_abcdefghijkl", "hdr_abcdefgh"}
	message := "auth failed: tok_abcdefghijkl rejected by hdr_abcdefgh"

	first := RedactString(message, Options{ExtraSecretValues: values})
	shuffled := []string{values[2], values[0], values[1]}
	second := RedactString(message, Options{ExtraSecretValues: shuffled})

	if first != second {
		t.Errorf("redaction depends on the order the caller collected its secrets:\n  %q\nvs\n  %q", first, second)
	}
	for _, secret := range values {
		if strings.Contains(first, secret) {
			t.Errorf("secret %q survived: %q", secret, first)
		}
	}
}

// A duplicate must not change the result. Callers append from several sources
// and the same value can arrive twice; replacing it a second time finds nothing
// and must stay harmless.
func TestDuplicateSecretValuesAreHarmless(t *testing.T) {
	const secret = "sk-duplicate-value"
	once := RedactString("failed: "+secret, Options{ExtraSecretValues: []string{secret}})
	twice := RedactString("failed: "+secret, Options{ExtraSecretValues: []string{secret, secret}})
	if once != twice {
		t.Errorf("a repeated secret changed the result:\n  %q\nvs\n  %q", once, twice)
	}
}

// The empty-value guard has to survive the reordering. A blank entry must not
// become a replacement that matches everywhere.
func TestBlankSecretValuesAreIgnored(t *testing.T) {
	got := RedactString("connection refused", Options{ExtraSecretValues: []string{"", "   ", "\t"}})
	if got != "connection refused" {
		t.Errorf("a blank secret value altered the message: %q", got)
	}
}
