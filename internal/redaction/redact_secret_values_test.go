package redaction

import "testing"

func TestRedactSecretValuesRemovesOnlyTheKnownValues(t *testing.T) {
	const key = "aiml-0123456789abcdef0123456789abcdef"
	nul := string(rune(0))
	zeroWidthSpace := string(rune(0x200b))
	for _, tc := range []struct {
		name  string
		value string
		want  string
		// shaped marks text RedactString would rewrite by shape alone, so the
		// case shows that RedactSecretValues leaves shapes to RedactString.
		shaped bool
	}{
		{name: "whole", value: "invalid key " + key + " for this account", want: "invalid key " + RedactedSecret + " for this account"},
		{name: "bearer form", value: "got Bearer " + key, want: "got Bearer " + RedactedSecret},
		{name: "split by NUL", value: "invalid key " + key[:9] + nul + key[9:], want: "invalid key " + RedactedSecret},
		{name: "split by a zero-width space", value: "value " + key[:20] + zeroWidthSpace + key[20:] + ".", want: "value " + RedactedSecret + "."},
		{name: "password assignment", value: "password=hunter2", want: "password=hunter2", shaped: true},
		{name: "another bearer token", value: "Authorization: Bearer some-other-token", want: "Authorization: Bearer some-other-token", shaped: true},
		{name: "openai shaped key", value: "use sk-proj-abcdefghijklmnopqrstuvwxyz0123456789", want: "use sk-proj-abcdefghijklmnopqrstuvwxyz0123456789", shaped: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := RedactSecretValues(tc.value, key); got != tc.want {
				t.Fatalf("RedactSecretValues(%q) = %q, want %q", tc.value, got, tc.want)
			}
			if tc.shaped && RedactString(tc.value, Options{}) == tc.value {
				t.Fatalf("RedactString(%q) left it unchanged, so this case does not show the shape patterns are off", tc.value)
			}
		})
	}

	for _, secrets := range [][]string{nil, {""}, {"  "}} {
		if got := RedactSecretValues("token "+key, secrets...); got != "token "+key {
			t.Fatalf("RedactSecretValues with secrets %q = %q, want the text unchanged", secrets, got)
		}
	}
}
