package providermodeldiscovery

import (
	"errors"
	"testing"
)

func TestRedactErrorWithholdsCredentialDamagedByEscapeSequence(t *testing.T) {
	const secret = "opaque-subscription-bearer"
	// A secret whose own digit reads as a CSI parameter is damaged the same way.
	if err := RedactError(errors.New("expired opaque-\x1b[9subscription now"), "opaque-9subscription"); !errors.Is(err, errWithheldDiscoveryDetails) {
		t.Fatalf("digit-led damaged secret was not withheld: %q", err)
	}
	for _, message := range []string{
		"expired opaque-\x1b[subscription-bearer now",
		"expired opaque-\x1b[9subscription-bearer now",
		"expired opaque-\x1b[?25subscription-bearer now",
		"expired opaque-\x1b]subscription-bearer now",
	} {
		err := RedactError(errors.New(message), secret)
		if !errors.Is(err, errWithheldDiscoveryDetails) {
			t.Fatalf("RedactError(%q) = %q, want withheld", message, err)
		}
	}
}

func TestRedactErrorKeepsVisibleGapsAndOrdinaryText(t *testing.T) {
	const secret = "opaque-subscription-bearer"
	// A whole sequence between the halves leaves a visible gap in both
	// readings, so neither holds the secret and the text is kept as shown.
	if got := RedactError(errors.New("expired opaque-\x1b7subscription-bearer now"), secret).Error(); got != "expired opaque-7subscription-bearer now" {
		t.Fatalf("whole sequence split: got %q", got)
	}
	const ordinary = "upstream returned 503:\tservice\nunavailable \x1b[31mretry\x1b[0m"
	if got := RedactError(errors.New(ordinary), secret).Error(); got != "upstream returned 503:\tservice\nunavailable retry" {
		t.Fatalf("ordinary error text changed: %q", got)
	}
}

func TestRedactErrorDropsDefaultIgnorableSplits(t *testing.T) {
	const secret = "opaque-subscription-bearer"
	for _, split := range []string{"͏", "️", "᠋", "\U000E0100"} {
		got := RedactError(errors.New("expired opaque-"+split+"subscription-bearer now"), secret).Error()
		if got != "expired [REDACTED] now" {
			t.Fatalf("split %U: got %q", []rune(split)[0], got)
		}
	}
}
