package tui

import (
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/agent"
)

func TestExpandSpecialistMention(t *testing.T) {
	specs := []agent.SpecialistInfo{
		{Name: "explorer", WhenToUse: "Explore."},
		{Name: "worker", WhenToUse: "Work."},
	}

	t.Run("leading mention expands to a delegation directive", func(t *testing.T) {
		got, ok := expandSpecialistMention("@explorer where is the retry logic?", specs)
		if !ok {
			t.Fatal("expected expansion")
		}
		if !strings.Contains(got, "explorer") ||
			!strings.Contains(got, "Task tool") ||
			!strings.Contains(got, "where is the retry logic?") {
			t.Fatalf("unexpected directive: %q", got)
		}
	})

	t.Run("name match is case-insensitive", func(t *testing.T) {
		if _, ok := expandSpecialistMention("@Explorer do the thing", specs); !ok {
			t.Fatal("expected case-insensitive match")
		}
	})

	for name, in := range map[string]string{
		"plain prompt":            "where is the retry logic?",
		"unknown specialist":      "@nobody do X",
		"mid-message file ref":    "summarize @internal/x.go",
		"mention without a task":  "@explorer",
		"mention with only space": "@explorer   ",
	} {
		t.Run("no expansion: "+name, func(t *testing.T) {
			if got, ok := expandSpecialistMention(in, specs); ok {
				t.Fatalf("expected no expansion for %q, got %q", in, got)
			}
		})
	}
}

func TestLeadingSpecialistSuggestions(t *testing.T) {
	m := model{agentOptions: agent.Options{Specialists: []agent.SpecialistInfo{
		{Name: "explorer", WhenToUse: "Explore."},
		{Name: "worker", WhenToUse: "Work."},
	}}}

	// A leading "@exp" suggests the explorer specialist.
	q := extractPathQuery("@exp", 4)
	if q == nil {
		t.Fatal("expected a path query for @exp")
	}
	got := m.leadingSpecialistSuggestions("@exp", q)
	if len(got) != 1 || got[0].Name != "@explorer" {
		t.Fatalf("expected [@explorer], got %#v", got)
	}

	// A mid-message "@token" is a file reference, not a specialist mention.
	value := "look at @wor"
	q2 := extractPathQuery(value, len([]rune(value)))
	if q2 == nil {
		t.Fatal("expected a path query for mid-message @")
	}
	if got := m.leadingSpecialistSuggestions(value, q2); got != nil {
		t.Fatalf("mid-message @ must not suggest specialists, got %#v", got)
	}
}
