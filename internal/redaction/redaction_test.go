package redaction

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestRedactStringRedactsAdjacentAWSKeys(t *testing.T) {
	// Two AWS keys glued end to end have no word boundary between them, so a
	// plain \b-anchored pattern only matches the first. Both must be redacted.
	// gitleaks:allow -- synthetic redaction fixtures below
	key1 := "AKIAIOSFODNN7EXAMPLE"
	key2 := "ASIAIOSFODNN7EXAMPLE"
	input := key1 + key2
	if got := RedactString(input, Options{}); got != RedactedSecret+RedactedSecret {
		t.Fatalf("adjacent AWS keys not both redacted: got %q", got)
	}

	// Three glued keys chain as well.
	input3 := key1 + key2 + key1
	if got := RedactString(input3, Options{}); got != RedactedSecret+RedactedSecret+RedactedSecret {
		t.Fatalf("three adjacent AWS keys not all redacted: got %q", got)
	}

	// A mid-word AKIA that does not abut a redacted span still must not match.
	if got := RedactString("prefixAKIAIOSFODNN7EXAMPLE", Options{}); got != "prefixAKIAIOSFODNN7EXAMPLE" {
		t.Fatalf("mid-word AKIA should not be redacted: got %q", got)
	}
}

func TestRedactStringCoversCommonSecretShapes(t *testing.T) {
	input := strings.Join([]string{
		`{"apiKey":"sk-proj-abcdefghijklmnopqrstuvwxyz"}`,
		"authorization: Bearer ghp_abcdefghijklmnopqrstuvwxyz123456",
		"https://zero:super-secret@example.test/path?token=glpat-abcdefghijklmnopqrstuvwxyz",
		"-----BEGIN PRIVATE KEY-----\nabc123\n-----END PRIVATE KEY-----",
	}, "\n")

	got := RedactString(input, Options{ExtraSecretValues: []string{"super-secret"}})

	for _, leaked := range []string{
		"sk-proj-abcdefghijklmnopqrstuvwxyz",
		"ghp_abcdefghijklmnopqrstuvwxyz123456",
		"super-secret",
		"glpat-abcdefghijklmnopqrstuvwxyz",
		"abc123",
	} {
		if strings.Contains(got, leaked) {
			t.Fatalf("redacted string leaked %q in %q", leaked, got)
		}
	}
	if count := strings.Count(got, RedactedSecret); count < 5 {
		t.Fatalf("expected multiple redaction markers, got %d in %q", count, got)
	}
}

func TestRedactValueHandlesSensitiveKeysAndCycles(t *testing.T) {
	type node struct {
		Name     string
		Password string
		Next     *node
	}
	root := &node{Name: "root", Password: "open-sesame"}
	root.Next = root

	got := RedactValue(root, Options{})
	asMap, ok := got.(map[string]any)
	if !ok {
		t.Fatalf("expected map output, got %T", got)
	}
	if asMap["Password"] != RedactedSecret {
		t.Fatalf("expected sensitive key redacted, got %#v", asMap["Password"])
	}
	if asMap["Next"] != CircularReference {
		t.Fatalf("expected circular reference marker, got %#v", asMap["Next"])
	}
}

func TestRedactErrorRedactsMessageStackAndFields(t *testing.T) {
	err := withFieldsError{
		err:    errors.New("request failed with api_key=sk-test-secret1234567890"),
		Token:  "ghp_abcdefghijklmnopqrstuvwxyz123456",
		Detail: "safe",
	}

	got := RedactError(err, Options{})

	if strings.Contains(got.Message, "sk-test-secret") {
		t.Fatalf("message leaked secret: %#v", got)
	}
	if got.Fields["Token"] != RedactedSecret {
		t.Fatalf("token field was not redacted: %#v", got.Fields)
	}
	if got.Fields["Detail"] != "safe" {
		t.Fatalf("non-sensitive field changed: %#v", got.Fields)
	}
}

type withFieldsError struct {
	err    error
	Token  string
	Detail string
}

func (err withFieldsError) Error() string {
	return err.err.Error()
}

func TestRedactValueSharedPointerIsNotMistakenForCycle(t *testing.T) {
	// Two sibling fields referencing the SAME object form a DAG, not a cycle.
	// Both must redact normally; the second must not collapse to CircularReference.
	type leaf struct{ Name string }
	type root struct {
		A *leaf
		B *leaf
	}
	shared := &leaf{Name: "shared"}
	out := RedactValue(root{A: shared, B: shared}, Options{})
	m, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("expected map, got %T", out)
	}
	for _, field := range []string{"A", "B"} {
		sub, ok := m[field].(map[string]any)
		if !ok {
			t.Fatalf("field %s = %#v, want a redacted leaf (sibling DAG must not be %q)", field, m[field], CircularReference)
		}
		if sub["Name"] != "shared" {
			t.Fatalf("field %s Name = %v, want \"shared\"", field, sub["Name"])
		}
	}
}

func TestRedactValueTrueCycleStillDetected(t *testing.T) {
	type node struct {
		Name string
		Next *node
	}
	a := &node{Name: "a"}
	a.Next = a // genuine self-cycle
	out := RedactValue(a, Options{})
	if !containsCircular(out) {
		t.Fatalf("expected a CircularReference marker somewhere in %#v", out)
	}
}

func containsCircular(v any) bool {
	switch t := v.(type) {
	case string:
		return t == CircularReference
	case map[string]any:
		for _, val := range t {
			if containsCircular(val) {
				return true
			}
		}
	case []any:
		for _, val := range t {
			if containsCircular(val) {
				return true
			}
		}
	}
	return false
}

func TestRedactStringLongAdjacentKeyRunCompletesPromptly(t *testing.T) {
	// A long run of glued AWS keys must not blow up the span-chaining loop in
	// redactAdjacent: the outer loop is bounded to the original match count,
	// so appended spans are never re-chained. On the old code this input
	// grows the span list exponentially and never finishes.
	// The key is assembled at runtime so the literal never appears in source.
	// gitleaks:allow -- synthetic redaction fixture below
	key := "AKIA" + strings.Repeat("A", 16)
	const count = 64
	input := strings.Repeat(key, count)
	start := time.Now()
	got := RedactString(input, Options{})
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("RedactString took %v on %d glued keys", elapsed, count)
	}
	if want := strings.Repeat(RedactedSecret, count); got != want {
		t.Fatalf("expected %d redaction markers, got %d", count, strings.Count(got, RedactedSecret))
	}
}
