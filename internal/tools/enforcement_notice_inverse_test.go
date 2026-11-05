package tools

import "testing"

// WithoutEnforcementNotices exists for readers of stored results, which hold
// the text with the disclosure composed in AND the same notices as a typed
// field. The property that matters is that composing after it always yields
// exactly what composing once would have: whether the stored text had been
// decorated or not, the reader ends up with one disclosure.
func TestWithoutEnforcementNoticesUndoesExactlyOneComposition(t *testing.T) {
	notices := []string{"least-privilege notice: read access was narrowed", "network access was denied"}
	for _, base := range []string{"the command output", "", "   ", "two\n\nparagraphs of output", "least-privilege notice: quoted in the output itself"} {
		once := WithEnforcementNotices(base, notices)

		// Stored decorated, the shape every writer produces.
		if got := WithEnforcementNotices(WithoutEnforcementNotices(once, notices), notices); got != once {
			t.Errorf("decorated %q: recomposed to %q, want %q", base, got, once)
		}
		// Stored undecorated: nothing to take off, and still one disclosure.
		if got := WithEnforcementNotices(WithoutEnforcementNotices(base, notices), notices); got != once {
			t.Errorf("undecorated %q: recomposed to %q, want %q", base, got, once)
		}
	}

	// No notices, or only blank ones, means there was never a composition.
	for _, none := range [][]string{nil, {}, {"", "  "}} {
		if got := WithoutEnforcementNotices("plain output", none); got != "plain output" {
			t.Errorf("notices %q changed undecorated text to %q", none, got)
		}
	}

	// Only a leading copy is a composition. The same words further down are the
	// command's own output and stay where they are.
	text := "first line\n\n" + notices[0] + "\n" + notices[1] + "\n\ntail"
	if got := WithoutEnforcementNotices(text, notices); got != text {
		t.Errorf("a notice quoted inside the output was removed: %q", got)
	}
}
