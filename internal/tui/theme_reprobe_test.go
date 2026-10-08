package tui

import (
	"context"
	"testing"
)

// System no longer guesses a light or dark canvas from the terminal. The legacy
// auto argument is accepted for old configs and scripts, but resolves to the
// terminal-native system preference without issuing a background-color query.
func TestThemeAutoAliasUsesSystemWithoutBackgroundProbe(t *testing.T) {
	defer applyTheme(themeDark, true)
	m := newModel(context.Background(), Options{ModelName: "gpt-4"})
	m.input.SetValue("/theme auto")
	updated, cmd := m.handleSubmit()
	next := updated.(model)
	if cmd == nil {
		t.Fatal("/theme auto should schedule a transient confirmation expiry")
	}
	if next.themeMode != themeSystem {
		t.Fatalf("/theme auto mode = %q, want system", next.themeMode)
	}
	if next.transientNotice.text != "Theme: System" {
		t.Fatalf("/theme auto notice = %q", next.transientNotice.text)
	}
}
