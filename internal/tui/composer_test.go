package tui

import (
	"context"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/Twigpine/zero/internal/imageinput"
)

func TestComposerInsertNewlineAtCursor(t *testing.T) {
	state := composerState{text: "helloworld", cursor: 5}

	got := insertComposerText(state, "\n")

	if got.text != "hello\nworld" {
		t.Fatalf("text = %q, want %q", got.text, "hello\nworld")
	}
	if got.cursor != len([]rune("hello\n")) {
		t.Fatalf("cursor = %d, want %d", got.cursor, len([]rune("hello\n")))
	}
}

func TestComposerDeleteWordBeforeCursor(t *testing.T) {
	state := composerState{text: "alpha beta  gamma", cursor: len([]rune("alpha beta  gamma"))}

	got := deleteComposerWordBefore(state)

	if got.text != "alpha beta  " {
		t.Fatalf("text = %q, want %q", got.text, "alpha beta  ")
	}
	if got.cursor != len([]rune("alpha beta  ")) {
		t.Fatalf("cursor = %d, want %d", got.cursor, len([]rune("alpha beta  ")))
	}
}

func TestComposerDeleteWordBeforeSkipsTrailingSpace(t *testing.T) {
	state := composerState{text: "alpha beta  ", cursor: len([]rune("alpha beta  "))}

	got := deleteComposerWordBefore(state)

	if got.text != "alpha " {
		t.Fatalf("text = %q, want %q", got.text, "alpha ")
	}
	if got.cursor != len([]rune("alpha ")) {
		t.Fatalf("cursor = %d, want %d", got.cursor, len([]rune("alpha ")))
	}
}

func TestComposerDeleteWordAfterCursor(t *testing.T) {
	state := composerState{text: "alpha  beta gamma", cursor: len([]rune("alpha  "))}

	got := deleteComposerWordAfter(state)

	if got.text != "alpha  gamma" {
		t.Fatalf("text = %q, want %q", got.text, "alpha  gamma")
	}
	if got.cursor != len([]rune("alpha  ")) {
		t.Fatalf("cursor = %d, want %d", got.cursor, len([]rune("alpha  ")))
	}
}

func TestBackspaceAfterCompletedFileMentionRemovesWholeMention(t *testing.T) {
	tests := []struct {
		name       string
		start      string
		want       string
		wantCursor int
	}{
		{
			name:       "only mention",
			start:      "@docs/NPM_WRAPPER_SMOKE.md ",
			want:       "",
			wantCursor: 0,
		},
		{
			name:       "mention after prompt text",
			start:      "read @docs/NPM_WRAPPER_SMOKE.md ",
			want:       "read ",
			wantCursor: len([]rune("read ")),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := newModel(context.Background(), Options{})
			m.input.SetValue(tc.start)
			m.input.CursorEnd()

			updated, _ := m.Update(testKey(tea.KeyBackspace))
			next := updated.(model)

			if got := next.composerValue(); got != tc.want {
				t.Fatalf("composer value = %q, want %q", got, tc.want)
			}
			if got := next.currentComposerState().cursor; got != tc.wantCursor {
				t.Fatalf("cursor = %d, want %d", got, tc.wantCursor)
			}
			if next.suggestionsActive() {
				t.Fatal("completed mention deletion should not reopen the file picker")
			}
		})
	}
}

func TestBackspaceInsideActiveFileMentionStillEditsQuery(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, root, "docs/NPM_WRAPPER_SMOKE.md")
	m := newModel(context.Background(), Options{Cwd: root})
	m = typeRunes(t, m, "@docs")

	updated, _ := m.Update(testKey(tea.KeyBackspace))
	next := updated.(model)

	if got := next.composerValue(); got != "@doc" {
		t.Fatalf("composer value = %q, want active query edited", got)
	}
	if !next.suggestionsActive() || !next.suggestionsAreFiles {
		t.Fatal("active file query backspace should keep the file picker open")
	}
}

func TestSanitizeComposerPastePreservesNewlines(t *testing.T) {
	got := sanitizeComposerPaste("alpha\tbeta\x00\nsecond\r\nthird\x1b[31m")
	want := "alpha    beta\nsecond\nthird[31m"

	if got != want {
		t.Fatalf("sanitized paste = %q, want %q", got, want)
	}
}

// Ctrl+V must never insert TEXT: bracketed paste is the only text path, so
// letting Bubble's own Ctrl+V binding also read the clipboard would double-paste.
// It may return a command, but that command is the image probe below, never a
// text paste, so the composer contents are unchanged either way.
func TestCtrlVDoesNotPasteTextIntoComposer(t *testing.T) {
	m := newModel(context.Background(), Options{})
	m.input.SetValue("hello")
	m.input.CursorEnd()

	updated, _ := m.Update(testKeyCtrl('v'))
	next := updated.(model)

	if got := next.composerValue(); got != "hello" {
		t.Fatalf("composer value after ctrl+v = %q, want unchanged", got)
	}
}

// Ctrl+V probes the clipboard for an image (#534). A clipboard holding a
// screenshot produces no bracketed paste at all, so without this the empty-paste
// image probe in routePaste never runs and Ctrl+V does nothing, which is exactly
// what users reported: right-click paste attached the image, Ctrl+V did not.
func TestCtrlVProbesClipboardForImage(t *testing.T) {
	// Stubbed BEFORE Update, so the command Update returns is the one that runs.
	// Asserting only that some command came back, then running a freshly built
	// probe, would pass even if the key never reached the image route at all.
	original := readClipboardImage
	t.Cleanup(func() { readClipboardImage = original })
	readClipboardImage = func() ([]byte, string, error) {
		return []byte("png-bytes"), "image/png", nil
	}

	// Both modifiers: macOS reports Command as ModSuper, so a handler matching
	// only ModCtrl leaves Cmd+V doing nothing on the platform where screenshots
	// are most often on the clipboard.
	for name, key := range map[string]tea.KeyPressMsg{
		"ctrl+v": testKeyCtrl('v'),
		"cmd+v":  testKeyPressMod('v', tea.ModSuper),
	} {
		t.Run(name, func(t *testing.T) {
			m := newModel(context.Background(), Options{})
			_, cmd := m.Update(key)
			if cmd == nil {
				t.Fatal("no command issued; the clipboard image probe never ran")
			}
			image, ok := execCmd(cmd).(clipboardImageMsg)
			if !ok {
				t.Fatal("the command issued was not the clipboard image probe")
			}
			if string(image.data) != "png-bytes" || image.mediaType != "image/png" {
				t.Fatalf("unexpected image message: %+v", image)
			}
		})
	}
}

// With no image on the clipboard the probe stays silent: Ctrl+V while copying
// text must not emit a notice or disturb the composer. Driven through Update
// rather than by calling the probe directly, so this covers the real key route
// and not just the command in isolation.
func TestClipboardImageProbeSilentWithoutImage(t *testing.T) {
	original := readClipboardImage
	t.Cleanup(func() { readClipboardImage = original })
	readClipboardImage = func() ([]byte, string, error) { return nil, "", nil }

	m := newModel(context.Background(), Options{})
	_, cmd := m.Update(testKeyCtrl('v'))
	if msg := execCmd(cmd); msg != nil {
		t.Fatalf("probe emitted %T with no image on the clipboard, want no message", msg)
	}
}

func TestPastedMultilineComposerContentRendersAsPreview(t *testing.T) {
	paste := strings.Join([]string{
		"Create a book library dashboard page with the Bootstrap theme.",
		"Include cards, search, filters, and progress bars.",
		"Make the grid responsive across desktop and mobile.",
		"Keep the sidebar readable.",
	}, "\n")
	m := newModel(context.Background(), Options{})

	updated, _ := m.Update(testPaste(paste))
	next := updated.(model)

	if got := next.composerValue(); got != paste {
		t.Fatalf("composer value should preserve full paste = %q, want %q", got, paste)
	}
	view := plainRender(t, next.composerBox(96))
	for _, want := range []string{"[Create a book library dashboard page", "4 lines"} {
		if !strings.Contains(view, want) {
			t.Fatalf("composer preview missing %q in:\n%s", want, view)
		}
	}
	if strings.Contains(view, "Include cards") || strings.Contains(view, "Keep the sidebar") {
		t.Fatalf("composer preview should not render full pasted content:\n%s", view)
	}
}

func TestPastedLongSingleLineComposerContentRendersWrappedLineCount(t *testing.T) {
	paste := strings.TrimSpace(strings.Repeat("word ", 37))
	m := newModel(context.Background(), Options{})
	m.width = 44

	updated, _ := m.Update(testPaste(paste))
	next := updated.(model)

	if got := next.composerValue(); got != paste {
		t.Fatalf("composer value should preserve full paste = %q, want %q", got, paste)
	}
	view := plainRender(t, next.composerBox(44))
	for _, want := range []string{"[word word word", "5 lines"} {
		if !strings.Contains(view, want) {
			t.Fatalf("composer preview missing %q in:\n%s", want, view)
		}
	}
	for _, unwanted := range []string{"1 line", "chars"} {
		if strings.Contains(view, unwanted) {
			t.Fatalf("composer preview should use wrapped line count, found %q in:\n%s", unwanted, view)
		}
	}
}

func TestBackspaceAfterPastedPreviewDeletesWholePaste(t *testing.T) {
	paste := "first line\nsecond line\nthird line"
	m := newModel(context.Background(), Options{})
	updated, _ := m.Update(testPaste(paste))
	next := updated.(model)

	updated, _ = next.Update(testKey(tea.KeyBackspace))
	next = updated.(model)
	if got := next.composerValue(); got != "" {
		t.Fatalf("composer value after deleting paste preview = %q, want empty", got)
	}
	if len(next.composerPastePreviews) != 0 {
		t.Fatal("paste preview should clear after deleting pasted block")
	}
}

func TestAltBackspaceAfterPastedPreviewDoesNotLeakPaste(t *testing.T) {
	paste := "first line\nsecond line\nthird line"
	m := newModel(context.Background(), Options{})
	m.input.SetValue("prefix ")
	m.input.CursorEnd()

	updated, _ := m.Update(testPaste(paste))
	next := updated.(model)
	updated, _ = next.Update(testKeyAlt(tea.KeyBackspace))
	next = updated.(model)

	if got := next.composerValue(); got != "prefix " {
		t.Fatalf("composer value after alt+backspace = %q, want prefix only", got)
	}
	view := plainRender(t, next.composerBox(96))
	if strings.Contains(view, "second line") || strings.Contains(view, "third line") {
		t.Fatalf("alt+backspace should not leak hidden pasted content:\n%s", view)
	}
}

func TestBackspaceAfterTypedTextKeepsPastedPreviewCollapsed(t *testing.T) {
	paste := "first line\nsecond line\nthird line"
	m := newModel(context.Background(), Options{})

	updated, _ := m.Update(testPaste(paste))
	next := updated.(model)
	updated, _ = next.Update(testKeyText("x"))
	next = updated.(model)
	updated, _ = next.Update(testKey(tea.KeyBackspace))
	next = updated.(model)

	if got := next.composerValue(); got != paste {
		t.Fatalf("composer value after deleting typed suffix = %q, want original paste", got)
	}
	view := plainRender(t, next.composerBox(96))
	for _, want := range []string{"[first line", "3 lines"} {
		if !strings.Contains(view, want) {
			t.Fatalf("composer preview missing %q after deleting typed suffix:\n%s", want, view)
		}
	}
	if strings.Contains(view, "second line") || strings.Contains(view, "third line") {
		t.Fatalf("backspace after typed suffix should keep paste collapsed:\n%s", view)
	}
}

func TestPastingTwiceKeepsBothComposerPreviews(t *testing.T) {
	firstPaste := strings.Join([]string{
		"Create a dashboard.",
		"Use cards and filters.",
		"Keep it responsive.",
	}, "\n")
	secondPaste := strings.Join([]string{
		"Log line one",
		"Log line two",
		"Log line three",
		"Log line four",
	}, "\n")
	m := newModel(context.Background(), Options{})

	updated, _ := m.Update(testPaste(firstPaste))
	next := updated.(model)
	updated, _ = next.Update(testKeyCtrl('j'))
	next = updated.(model)
	updated, _ = next.Update(testPaste(secondPaste))
	next = updated.(model)

	if got := next.composerValue(); got != firstPaste+"\n"+secondPaste {
		t.Fatalf("composer value should preserve both pastes = %q", got)
	}
	view := plainRender(t, next.composerBox(120))
	for _, want := range []string{"[Create a dashboard.", "3 lines", "[Log line one", "4 lines, paste 2"} {
		if !strings.Contains(view, want) {
			t.Fatalf("composer preview missing %q in:\n%s", want, view)
		}
	}
	for _, unwanted := range []string{"Use cards and filters", "Log line two"} {
		if strings.Contains(view, unwanted) {
			t.Fatalf("composer preview should keep both pasted blocks compact, found %q in:\n%s", unwanted, view)
		}
	}
}

func TestModifiedEnterInsertsNewlineWithoutSubmitting(t *testing.T) {
	tests := []struct {
		name string
		key  tea.Msg
	}{
		{name: "alt enter", key: testKeyAlt(tea.KeyEnter)},
		{name: "shift enter", key: testKeyShift(tea.KeyEnter)},
		{name: "ctrl j", key: testKeyCtrl('j')},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := newModel(context.Background(), Options{Provider: &fakeProvider{}, ProviderName: "test", ModelName: "test-model"})
			m.input.SetValue("first")
			m.input.CursorEnd()

			updated, cmd := m.Update(tc.key)
			next := updated.(model)

			if cmd != nil {
				t.Fatal("modified Enter should not launch a run")
			}
			if next.pending {
				t.Fatal("modified Enter should leave the model idle")
			}
			if got := next.composerValue(); got != "first\n" {
				t.Fatalf("input = %q, want %q", got, "first\n")
			}
		})
	}
}

func TestMultilineComposerEditingDoesNotFallBackToFlatInput(t *testing.T) {
	m := newModel(context.Background(), Options{})
	m.setComposerState(composerState{text: "alpha\nbeta gamma", cursor: len([]rune("alpha\nbeta"))})

	updated, _ := m.Update(testKeyCtrl('u'))
	next := updated.(model)
	if got := next.composerValue(); got != "alpha\n gamma" {
		t.Fatalf("ctrl+u composer value = %q, want current line prefix removed", got)
	}
	if !next.composerActive {
		t.Fatal("ctrl+u should keep multiline composer state active")
	}

	updated, _ = next.Update(testKeyCtrl('k'))
	next = updated.(model)
	if got := next.composerValue(); got != "alpha\n" {
		t.Fatalf("ctrl+k composer value = %q, want current line suffix removed", got)
	}
	if !next.composerActive {
		t.Fatal("ctrl+k should keep multiline composer state active")
	}
}

func TestMultilineComposerAcceptsSpaceKey(t *testing.T) {
	m := newModel(context.Background(), Options{})
	m.setComposerState(composerState{text: "alpha\nbetagamma", cursor: len([]rune("alpha\nbeta"))})

	updated, _ := m.Update(testKey(tea.KeySpace))
	next := updated.(model)

	if got := next.composerValue(); got != "alpha\nbeta gamma" {
		t.Fatalf("composer value = %q, want space inserted in multiline state", got)
	}
	if !next.composerActive {
		t.Fatal("space insertion should keep multiline composer state active")
	}
}

func TestWrappedComposerArrowKeysMoveByVisualLine(t *testing.T) {
	text := "Create a book library dashboard page with cards, filters, charts, and responsive behavior."
	m := newModel(context.Background(), Options{})
	m.width = 44
	m.input.SetValue(text)
	m.input.CursorEnd()
	startCursor := len([]rune(text))

	updated, _ := m.Update(testKey(tea.KeyUp))
	next := updated.(model)
	if got := next.composerValue(); got != text {
		t.Fatalf("composer value = %q, want unchanged text %q", got, text)
	}
	upCursor := next.currentComposerState().cursor
	if upCursor >= startCursor {
		t.Fatalf("up cursor = %d, want before end cursor %d", upCursor, startCursor)
	}

	updated, _ = next.Update(testKey(tea.KeyDown))
	next = updated.(model)
	if got := next.currentComposerState().cursor; got != startCursor {
		t.Fatalf("down cursor = %d, want restored end cursor %d", got, startCursor)
	}
}

func TestComposerTerminalWordKeybindings(t *testing.T) {
	tests := []struct {
		name       string
		start      string
		cursor     int
		key        tea.Msg
		want       string
		wantCursor int
	}{
		{
			name:       "alt backspace skips trailing spaces",
			start:      "alpha beta  ",
			cursor:     len([]rune("alpha beta  ")),
			key:        testKeyAlt(tea.KeyBackspace),
			want:       "alpha ",
			wantCursor: len([]rune("alpha ")),
		},
		{
			name:       "ctrl w skips trailing spaces",
			start:      "alpha beta  ",
			cursor:     len([]rune("alpha beta  ")),
			key:        testKeyCtrl('w'),
			want:       "alpha ",
			wantCursor: len([]rune("alpha ")),
		},
		{
			name:       "alt b moves back a word",
			start:      "alpha beta gamma",
			cursor:     len([]rune("alpha beta gamma")),
			key:        testKeyAltText("b"),
			want:       "alpha beta gamma",
			wantCursor: len([]rune("alpha beta ")),
		},
		{
			name:       "ctrl left moves back a word",
			start:      "alpha beta gamma",
			cursor:     len([]rune("alpha beta gamma")),
			key:        testKeyPressMod(tea.KeyLeft, tea.ModCtrl),
			want:       "alpha beta gamma",
			wantCursor: len([]rune("alpha beta ")),
		},
		{
			name:       "alt f moves forward a word",
			start:      "alpha beta gamma",
			cursor:     len([]rune("alpha ")),
			key:        testKeyAltText("f"),
			want:       "alpha beta gamma",
			wantCursor: len([]rune("alpha beta")),
		},
		{
			name:       "ctrl right moves forward a word",
			start:      "alpha beta gamma",
			cursor:     len([]rune("alpha ")),
			key:        testKeyPressMod(tea.KeyRight, tea.ModCtrl),
			want:       "alpha beta gamma",
			wantCursor: len([]rune("alpha beta")),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := newModel(context.Background(), Options{})
			m.input.SetValue(tc.start)
			m.input.SetCursor(tc.cursor)

			updated, _ := m.Update(tc.key)
			next := updated.(model)

			if got := next.composerValue(); got != tc.want {
				t.Fatalf("composer value = %q, want %q", got, tc.want)
			}
			if got := next.currentComposerState().cursor; got != tc.wantCursor {
				t.Fatalf("cursor = %d, want %d", got, tc.wantCursor)
			}
		})
	}
}

// A clipboard that holds an image the host cannot extract must say so. This is
// the macOS case: neither pngpaste nor PyObjC ships with the OS, so the reader
// fails on a stock Mac, and reporting that as "no image" meant Ctrl+V did
// nothing at all with no explanation. Silence is the wrong answer to a
// condition the user can act on.
func TestClipboardImageUnreadableSurfacesToUser(t *testing.T) {
	original := readClipboardImage
	t.Cleanup(func() { readClipboardImage = original })
	readClipboardImage = func() ([]byte, string, error) {
		return nil, "", imageinput.ErrClipboardImageUnreadable
	}

	m := newModel(context.Background(), Options{})
	_, cmd := m.Update(testKeyCtrl('v'))
	if cmd == nil {
		t.Fatal("no command issued; the clipboard image probe never ran")
	}
	msg := execCmd(cmd)
	image, ok := msg.(clipboardImageMsg)
	if !ok {
		t.Fatalf("probe returned %T, want clipboardImageMsg carrying the failure", msg)
	}
	if image.err == nil {
		t.Fatal("the unreadable-clipboard failure was swallowed; the user is told nothing")
	}
	// The message has to name the remedy, since the whole point is that the user
	// can fix this by installing something.
	if !strings.Contains(image.err.Error(), "pngpaste") {
		t.Fatalf("error = %q, want it to name what to install", image.err.Error())
	}

	// And it must reach the transcript as an error row rather than stopping at
	// the message, which is where the old silent no-op ended.
	updated, _ := m.Update(image)
	next, ok := updated.(model)
	if !ok {
		t.Fatalf("Update returned %T, want model", updated)
	}
	found := false
	for _, row := range next.transcript {
		if row.kind == rowError && strings.Contains(row.text, "Clipboard image read failed") {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("the failure never reached the transcript; the user still sees nothing")
	}
}
