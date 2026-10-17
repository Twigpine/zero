package tui

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/Twigpine/zero/internal/agent"
)

// typeRunes feeds each rune of s through Update as an individual key press,
// exercising the same recompute-after-input path the real loop uses.
func typeRunes(t *testing.T, m model, s string) model {
	t.Helper()
	for _, r := range s {
		updated, _ := m.Update(testKeyText(string(r)))
		m = updated.(model)
	}
	return m
}

func suggestionNames(m model) []string {
	names := make([]string, 0, len(m.suggestions))
	for _, s := range m.suggestions {
		names = append(names, s.Name)
	}
	return names
}

func contains(names []string, want string) bool {
	for _, name := range names {
		if name == want {
			return true
		}
	}
	return false
}

func TestSuggestionsSurfaceMatchingCommands(t *testing.T) {
	m := newModel(context.Background(), Options{})
	m = typeRunes(t, m, "/mo")

	if !m.suggestionsActive() {
		t.Fatal("expected suggestions active after typing /mo")
	}
	names := suggestionNames(m)
	if !contains(names, "/model") {
		t.Fatalf("expected /model in suggestions, got %v", names)
	}
}

func TestSuggestionsMatchAliasButListCanonical(t *testing.T) {
	m := newModel(context.Background(), Options{})
	m = typeRunes(t, m, "/find") // alias of /search

	names := suggestionNames(m)
	if !contains(names, "/search") {
		t.Fatalf("expected alias /find to surface canonical /search, got %v", names)
	}
}

func TestSuggestionsInactiveWithoutSlashOrToken(t *testing.T) {
	m := newModel(context.Background(), Options{})

	m1 := typeRunes(t, m, "hello")
	if m1.suggestionsActive() {
		t.Fatal("plain text should not surface suggestions")
	}

	// A slash followed by a space (an argument has started) drops suggestions.
	m2 := typeRunes(t, m, "/model ")
	if m2.suggestionsActive() {
		t.Fatal("suggestions should clear once an argument is typed")
	}
}

func TestTabCyclesSuggestions(t *testing.T) {
	m := newModel(context.Background(), Options{})
	m = typeRunes(t, m, "/s") // ambiguous prefix with several matches
	start := m.suggestionIdx

	updated, _ := m.Update(testKey(tea.KeyTab))
	m = updated.(model)
	if m.suggestionIdx == start {
		t.Fatal("Tab should advance the selected suggestion")
	}

	// Tab past the end wraps to 0.
	for i := 0; i < len(m.suggestions); i++ {
		updated, _ = m.Update(testKey(tea.KeyTab))
		m = updated.(model)
	}
	if m.suggestionIdx != m.suggestionIdx%len(m.suggestions) {
		t.Fatal("selection index out of range after cycling")
	}
}

func TestUpDownMoveSuggestions(t *testing.T) {
	m := newModel(context.Background(), Options{})
	m = typeRunes(t, m, "/s") // ambiguous prefix with several matches

	updated, _ := m.Update(testKey(tea.KeyDown))
	m = updated.(model)
	if m.suggestionIdx != 1 {
		t.Fatalf("Down should select index 1, got %d", m.suggestionIdx)
	}
	// Up from index 0 wraps to the last suggestion.
	updated, _ = m.Update(testKey(tea.KeyUp))
	m = updated.(model)
	updated, _ = m.Update(testKey(tea.KeyUp))
	m = updated.(model)
	if m.suggestionIdx != len(m.suggestions)-1 {
		t.Fatalf("Up past the top should wrap to last (%d), got %d", len(m.suggestions)-1, m.suggestionIdx)
	}
}

func TestMouseWheelMovesSuggestions(t *testing.T) {
	m := newModel(context.Background(), Options{})
	m = typeRunes(t, m, "/")

	updated, _ := m.Update(testMouseWheel(tea.MouseWheelDown, 0, 0))
	m = updated.(model)
	if m.suggestionIdx != 1 {
		t.Fatalf("wheel down should select index 1, got %d", m.suggestionIdx)
	}

	updated, _ = m.Update(testMouseWheel(tea.MouseWheelUp, 0, 0))
	m = updated.(model)
	if m.suggestionIdx != 0 {
		t.Fatalf("wheel up should select index 0, got %d", m.suggestionIdx)
	}
}

func TestEnterRunsCommandSuggestion(t *testing.T) {
	m := newModel(context.Background(), Options{})
	m = typeRunes(t, m, "/he") // selects /help

	updated, cmd := m.Update(testKey(tea.KeyEnter))
	m = updated.(model)

	if cmd != nil {
		t.Fatal("Enter on a command suggestion should not start an agent run")
	}
	if got := m.input.Value(); got != "" {
		t.Fatalf("Enter on a command suggestion should clear input, got %q", got)
	}
	if m.suggestionsActive() {
		t.Fatal("running a command suggestion should dismiss the overlay")
	}
	if !transcriptContains(m.transcript, "Commands") {
		t.Fatal("running /help from suggestions should append help output")
	}
}

func TestEnterPrefillsCommandSuggestionRequiringInput(t *testing.T) {
	m := newModel(context.Background(), Options{})
	m = typeRunes(t, m, "/sp") // selects /spec

	updated, cmd := m.Update(testKey(tea.KeyEnter))
	m = updated.(model)

	if cmd != nil {
		t.Fatal("Enter on an argument command suggestion should not start an agent run")
	}
	if got := m.input.Value(); got != "/spec" {
		t.Fatalf("Enter should prefill command for arguments, got %q", got)
	}
	if got := plainRender(t, m.composerLine(96)); !strings.Contains(got, "/spec") || !strings.Contains(got, "[task]") {
		t.Fatalf("prefilled command should show argument hint, got %q", got)
	}

	updated, cmd = m.Update(testKeyText("fix"))
	m = updated.(model)
	if cmd != nil {
		t.Fatal("typing the argument should not start an agent run")
	}
	if got := m.input.Value(); got != "/spec fix" {
		t.Fatalf("typing after the hint should insert one argument separator, got %q", got)
	}
	for range "fix" {
		updated, cmd = m.Update(testKey(tea.KeyBackspace))
		m = updated.(model)
		if cmd != nil {
			t.Fatal("backspacing the argument should not start an agent run")
		}
	}
	if got := plainRender(t, m.composerLine(96)); !strings.Contains(got, "/spec [task]") || strings.Contains(got, "/spec  [task]") {
		t.Fatalf("empty argument command should render one visual separator, got %q", got)
	}
	updated, cmd = m.Update(testKeyText("x"))
	m = updated.(model)
	if cmd != nil {
		t.Fatal("typing after deleting the argument should not start an agent run")
	}
	if got := m.input.Value(); got != "/spec x" {
		t.Fatalf("typing after deleting the argument should keep one separator, got %q", got)
	}
	if m.suggestionsActive() {
		t.Fatal("prefilling a command suggestion should dismiss the overlay")
	}
	if transcriptContains(m.transcript, "usage: /spec") {
		t.Fatal("prefilling /spec should not run the command without a task")
	}
}

func TestCommandSuggestionFooterReflectsInsertAction(t *testing.T) {
	m := newModel(context.Background(), Options{})
	m.width, m.height = 96, 30
	m = typeRunes(t, m, "/sp")

	plain := plainRender(t, m.View())
	if !strings.Contains(plain, "Enter insert") {
		t.Fatalf("required-argument command footer should say Enter insert, got %q", plain)
	}
	if strings.Contains(plain, "Enter run") {
		t.Fatalf("required-argument command footer should not say Enter run, got %q", plain)
	}
}

func TestTabCompletesAfterSelection(t *testing.T) {
	m := newModel(context.Background(), Options{})
	m = typeRunes(t, m, "/s") // ambiguous prefix (stop, search, spec, …)

	// With multiple matches, Tab cycles the highlighted suggestion rather than
	// completing the input, so the typed prefix stays put.
	updated, _ := m.Update(testKey(tea.KeyTab))
	m = updated.(model)
	if m.input.Value() != "/s" {
		t.Fatalf("Tab should cycle, not yet complete; input=%q", m.input.Value())
	}
}

func TestEscDismissesCommandSuggestionsAndClearsInput(t *testing.T) {
	m := newModel(context.Background(), Options{})
	m = typeRunes(t, m, "/mo")

	updated, _ := m.Update(testKey(tea.KeyEsc))
	m = updated.(model)

	if m.suggestionsActive() {
		t.Fatal("Esc should dismiss the suggestion overlay")
	}
	if m.input.Value() != "" {
		t.Fatalf("Esc should clear slash command input, got %q", m.input.Value())
	}
}

func TestEscWithoutSuggestionsClearsInputAsBefore(t *testing.T) {
	m := newModel(context.Background(), Options{})
	m = typeRunes(t, m, "hello") // no suggestions

	updated, _ := m.Update(testKey(tea.KeyEsc))
	m = updated.(model)
	if m.input.Value() != "" {
		t.Fatalf("Esc with no suggestions should clear input, got %q", m.input.Value())
	}
}

func TestEnterWithNoSuggestionStillSubmits(t *testing.T) {
	m := newModel(context.Background(), Options{})
	m.input.SetValue("hello zero") // plain prompt, no suggestions

	updated, _ := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)

	if next.input.Value() != "" {
		t.Fatal("Enter should submit (and clear) a plain prompt")
	}
	if !transcriptContains(next.transcript, "hello zero") {
		t.Fatal("submitted prompt should appear in the transcript")
	}
}

func TestSuggestionsSuppressedDuringModals(t *testing.T) {
	m := newModel(context.Background(), Options{})
	request := agent.AskUserRequest{Questions: []agent.AskUserQuestion{{Question: "name?"}}}
	m.pendingAskUser = &pendingAskUserPrompt{
		request: request,
		answer:  func([]string) {},
		states:  newAskUserStates(request.Questions),
	}
	// Typing while a questionnaire is active feeds the answer field; no overlay.
	m = typeRunes(t, m, "/mo")
	if m.suggestionsActive() {
		t.Fatal("suggestions must stay suppressed while a questionnaire is active")
	}
}

func TestSuggestionsSuppressedDuringSpecReview(t *testing.T) {
	m := newModel(context.Background(), Options{})
	m.suggestions = []commandSuggestion{{Name: "/model", Desc: "Pick a model."}}
	m.pendingSpecReview = &pendingSpecReviewPrompt{SpecID: "spec-1", SpecFilePath: ".zero/specs/spec-1.md"}

	if m.suggestionsActive() {
		t.Fatal("stale suggestions must stay suppressed while spec review is active")
	}

	m = typeRunes(t, m, "/mo")
	if m.suggestionsActive() {
		t.Fatal("new suggestions must stay suppressed while spec review is active")
	}
}

func TestSuggestionOverlayRenders(t *testing.T) {
	m := newModel(context.Background(), Options{})
	m.width, m.height = 96, 30
	m = typeRunes(t, m, "/mo")

	view := m.View()
	plain := plainRender(t, view)
	if !strings.Contains(plain, "model") || !strings.Contains(plain, "mode") {
		t.Fatal("view should render the suggestion overlay")
	}
	if strings.Contains(plain, "/model") {
		t.Fatalf("suggestion overlay should display command names without slash prefixes, got %q", plain)
	}
	for _, want := range []string{"╭── Commands", "╰", "search > mo", "↑/↓ move", "Enter run", "Esc close"} {
		if !strings.Contains(plain, want) {
			t.Fatalf("suggestion overlay should include %q in %q", want, plain)
		}
	}
}

func TestSuggestionOverlayStaysVisibleWhenTranscriptScrolled(t *testing.T) {
	m := newModel(context.Background(), Options{})
	m.width, m.height = 96, 32
	m.altScreen = true
	m.headerPrinted = true
	for i := 0; i < 20; i++ {
		m.transcript = appendRow(m.transcript, rowAssistant, "message "+string(rune('A'+i)))
	}
	m.chatScrollOffset = 12
	m = typeRunes(t, m, "/")

	plain := plainRender(t, m.View())
	if !strings.Contains(plain, "Commands") || !strings.Contains(plain, "search >") {
		t.Fatalf("suggestion overlay should stay visible above composer while transcript is scrolled, got %q", plain)
	}
	lines := strings.Split(plain, "\n")
	if got := len(lines); got != m.height {
		t.Fatalf("alt-screen view should keep terminal height, got %d lines want %d", got, m.height)
	}
	paletteLine := -1
	composerLine := -1
	for index, line := range lines {
		switch {
		case strings.Contains(line, "Commands"):
			paletteLine = index
		case strings.Contains(line, "no model"):
			// The composer rule shows the model; the mode ("auto-approve") is now on
			// the status line below it, so locate the composer by its model label.
			composerLine = index
		}
	}
	if paletteLine < 0 || composerLine < 0 {
		t.Fatalf("expected palette and composer in view, palette=%d composer=%d view=%q", paletteLine, composerLine, plain)
	}
	if paletteLine >= composerLine-3 {
		t.Fatalf("palette should be centered over chat, not anchored to composer; palette line %d composer line %d", paletteLine, composerLine)
	}
}

func TestOverlayViewportLinesPreservesTextOutsidePanel(t *testing.T) {
	lines := []string{
		"left edge text stays visible after panel",
		"second row text stays visible after panel",
		"third row text stays visible after panel",
	}
	overlay := strings.Join([]string{
		"          ╭── Files ─╮",
		"          │ row      │",
		"          ╰──────────╯",
	}, "\n")

	got := overlayViewportLines(append([]string(nil), lines...), overlay, 48)
	plain := plainRender(t, strings.Join(got, "\n"))
	if !strings.Contains(plain, "left edge ╭── Files ─╮") {
		t.Fatalf("overlay should preserve text left of the panel, got %q", plain)
	}
	if !strings.Contains(plain, "visible after panel") {
		t.Fatalf("overlay should preserve text right of the panel, got %q", plain)
	}
}

func TestSuggestionOverlayCapsRowsWithoutMoreText(t *testing.T) {
	m := newModel(context.Background(), Options{})
	m.width, m.height = 96, 30
	m = typeRunes(t, m, "/")

	plain := plainRender(t, m.View())
	if strings.Contains(plain, "more") {
		t.Fatalf("bare slash palette should not render a more-count row, got %q", plain)
	}
	if !strings.Contains(plain, "│ ❯ provider") || !strings.Contains(plain, "│   ps") {
		t.Fatalf("top of palette should render first visible command window, got %q", plain)
	}
	if strings.Contains(plain, "compact") {
		t.Fatalf("top of palette should cap hidden commands without rendering them, got %q", plain)
	}

	for range suggestionPaletteMaxVisible + 1 {
		updated, _ := m.Update(testKey(tea.KeyDown))
		m = updated.(model)
	}
	plain = plainRender(t, m.View())
	if strings.Contains(plain, "more") {
		t.Fatalf("scrolled palette should not render a more-count row, got %q", plain)
	}
	selected := strings.TrimPrefix(m.suggestions[m.suggestionIdx].Name, "/")
	if !strings.Contains(plain, "│ ❯ "+selected) || strings.Contains(plain, "│ ❯ provider") || strings.Contains(plain, "│   provider") {
		t.Fatalf("scrolled palette should move the visible command window, got %q", plain)
	}
}

func TestCommandPaletteStaysOpenForNoMatches(t *testing.T) {
	m := newModel(context.Background(), Options{})
	m.width, m.height = 96, 30
	m = typeRunes(t, m, "/,")

	if !m.suggestionsActive() {
		t.Fatal("command palette should stay active for a no-match slash query")
	}
	if len(m.suggestions) != 0 {
		t.Fatalf("expected no command matches, got %v", suggestionNames(m))
	}
	plain := plainRender(t, m.View())
	for _, want := range []string{"search > ,", "no matching commands"} {
		if !strings.Contains(plain, want) {
			t.Fatalf("no-match palette should include %q in %q", want, plain)
		}
	}
	if strings.Contains(plain, "/,") {
		t.Fatalf("slash query should stay inside palette display, got %q", plain)
	}
}

func TestEnterOnNoMatchCommandPaletteDoesNotSubmit(t *testing.T) {
	m := newModel(context.Background(), Options{})
	m = typeRunes(t, m, "/,")

	updated, cmd := m.Update(testKey(tea.KeyEnter))
	m = updated.(model)

	if cmd != nil {
		t.Fatal("Enter on no-match command palette should not start a command")
	}
	if m.input.Value() != "/," {
		t.Fatalf("Enter on no-match command palette should preserve search input, got %q", m.input.Value())
	}
	if !m.suggestionsActive() {
		t.Fatal("Enter on no-match command palette should keep palette open")
	}
	if transcriptContains(m.transcript, "unknown command") {
		t.Fatal("Enter on no-match command palette should not submit an unknown command")
	}
}

func TestFilePaletteStaysOpenForNoMatches(t *testing.T) {
	m := newModel(context.Background(), Options{Cwd: t.TempDir()})
	m.width, m.height = 96, 30
	m = typeRunes(t, m, "@missing")

	if !m.suggestionsActive() {
		t.Fatal("file palette should stay active for a no-match @ query")
	}
	if len(m.suggestions) != 0 {
		t.Fatalf("expected no file matches, got %v", suggestionNames(m))
	}
	plain := plainRender(t, m.View())
	for _, want := range []string{"Files", "search > missing", "no matching files"} {
		if !strings.Contains(plain, want) {
			t.Fatalf("no-match file palette should include %q in %q", want, plain)
		}
	}
	if strings.Contains(plain, "@missing") {
		t.Fatalf("bare @ query should stay inside palette display without @ prefix, got %q", plain)
	}
}

func TestEscDismissesFilePaletteAndRemovesTrailingToken(t *testing.T) {
	m := newModel(context.Background(), Options{Cwd: t.TempDir()})
	m = typeRunes(t, m, "read @missing")

	updated, _ := m.Update(testKey(tea.KeyEsc))
	m = updated.(model)

	if m.suggestionsActive() {
		t.Fatal("Esc should dismiss the file palette")
	}
	if got := m.input.Value(); got != "read " {
		t.Fatalf("Esc should remove only the trailing @ token, got %q", got)
	}

	m = typeRunes(t, m, "x")
	if got := m.composerValue(); got != "read x" {
		t.Fatalf("Esc should keep composer state synced after removing @ token, got %q", got)
	}

	m = newModel(context.Background(), Options{Cwd: t.TempDir()})
	m = typeRunes(t, m, "compare @old with @new")
	m.input.SetCursor(len([]rune("compare @old")))
	m.recomputeSuggestions()

	updated, _ = m.Update(testKey(tea.KeyEsc))
	m = updated.(model)

	if m.suggestionsActive() {
		t.Fatal("Esc should dismiss the cursor-local file palette")
	}
	if got := m.input.Value(); got != "compare  with @new" {
		t.Fatalf("Esc should remove only the cursor-local @ token, got %q", got)
	}
	if got := m.input.Position(); got != len([]rune("compare ")) {
		t.Fatalf("cursor after cursor-local removal = %d, want %d", got, len([]rune("compare ")))
	}
	if got := m.composerValue(); got != "compare  with @new" {
		t.Fatalf("Esc should keep composer synced after cursor-local removal, got %q", got)
	}
}

func TestExtractPathQueryUsesCursorPosition(t *testing.T) {
	query := extractPathQuery("read @internal/file.go after", len([]rune("read @internal/file")))
	if query == nil {
		t.Fatal("expected path query at cursor")
	}
	if query.Query != "internal/file.go" || query.StartIndex != len([]rune("read ")) || query.EndIndex != len([]rune("read @internal/file.go")) {
		t.Fatalf("query = %#v", query)
	}
	if got := extractPathQuery("read @internal/file.go after", len([]rune("read @internal/file.go after"))); got != nil {
		t.Fatalf("cursor after mention should not be in path context, got %#v", got)
	}
}

func TestCompletePathQueryReplacesActiveMentionOnly(t *testing.T) {
	text := "compare @old and @ne"
	cursor := len([]rune(text))
	got, gotCursor := completePathQuery(text, cursor, "@new/file.go")
	want := "compare @old and @new/file.go "
	if got != want {
		t.Fatalf("completePathQuery text = %q, want %q", got, want)
	}
	if gotCursor != len([]rune(want)) {
		t.Fatalf("completePathQuery cursor = %d, want %d", gotCursor, len([]rune(want)))
	}
}

func TestFilePaletteDisplaysFilenamesAndPaths(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, root, "cmd/server/main.go")
	mustWriteFile(t, root, "main.go")

	m := newModel(context.Background(), Options{Cwd: root})
	m.width, m.height = 96, 30
	m = typeRunes(t, m, "@main")

	plain := plainRender(t, m.View())
	if strings.Contains(plain, "@main") {
		t.Fatalf("file palette should not render @ prefixes, got %q", plain)
	}
	if !strings.Contains(plain, "main.go") || !strings.Contains(plain, "cmd/server") {
		t.Fatalf("file palette should show filename plus parent path, got %q", plain)
	}
	if got := suggestionNames(m)[0]; got != "@main.go" {
		t.Fatalf("basename prefix match should rank before nested path match, got first suggestion %q from %v", got, suggestionNames(m))
	}
}

func TestFileSuggestionsIncludeDirectories(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "internal", "tui"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, root, "internal/tui/model.go")

	got := suggestionTokens(fileSuggestions(root, "internal"))
	if !contains(got, "@internal/") {
		t.Fatalf("expected directory suggestion, got %v", got)
	}
}

func TestEnterOnDirectorySuggestionKeepsFilePaletteOpen(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, root, "internal/tui/model.go")
	mustWriteFile(t, root, "internal/agent/loop.go")

	m := newModel(context.Background(), Options{Cwd: root})
	m = typeRunes(t, m, "@int")
	if got := suggestionNames(m)[0]; got != "@internal/" {
		t.Fatalf("expected internal directory first, got %q from %v", got, suggestionNames(m))
	}

	updated, cmd := m.Update(testKey(tea.KeyEnter))
	m = updated.(model)

	if cmd != nil {
		t.Fatal("directory completion should not submit a prompt")
	}
	if got := m.input.Value(); got != "@internal/" {
		t.Fatalf("directory completion should keep path active without trailing space, got %q", got)
	}
	if !m.suggestionsActive() || !m.suggestionsAreFiles {
		t.Fatal("directory completion should keep the file palette open")
	}
	if names := suggestionNames(m); !contains(names, "@internal/tui/") || !contains(names, "@internal/agent/") {
		t.Fatalf("directory completion should drill into that path, got %v", names)
	}
}

func TestEnterOnFileSuggestionClosesFilePalette(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, root, "internal/tui/model.go")

	m := newModel(context.Background(), Options{Cwd: root})
	m = typeRunes(t, m, "@model")
	if got := suggestionNames(m)[0]; got != "@internal/tui/model.go" {
		t.Fatalf("expected file suggestion first, got %q from %v", got, suggestionNames(m))
	}

	updated, cmd := m.Update(testKey(tea.KeyEnter))
	m = updated.(model)

	if cmd != nil {
		t.Fatal("file completion should not submit a prompt")
	}
	if got := m.input.Value(); got != "@internal/tui/model.go " {
		t.Fatalf("file completion should add trailing space, got %q", got)
	}
	if m.suggestionsActive() {
		t.Fatal("file completion should close the file palette")
	}
}

func TestFileSuggestionCompletionKeepsPastePreviewCollapsed(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, root, "docs/guide.md")
	paste := "Create a book library dashboard page with the Bootstrap theme. " + strings.Repeat("SECRET_TAIL ", 20)
	m := newModel(context.Background(), Options{Cwd: root})
	m.width = 44

	updated, _ := m.Update(testPaste(paste))
	m = updated.(model)
	updated, _ = m.Update(testKey(tea.KeySpace))
	m = updated.(model)
	updated, _ = m.Update(testKeyText("@guide"))
	m = updated.(model)

	if !m.suggestionsActive() || !m.suggestionsAreFiles {
		t.Fatalf("expected file suggestions after @guide, got suggestions=%v files=%v", m.suggestionsActive(), m.suggestionsAreFiles)
	}
	updated, cmd := m.Update(testKey(tea.KeyEnter))
	m = updated.(model)

	if cmd != nil {
		t.Fatal("file completion should not submit a prompt")
	}
	if got := m.composerValue(); got != paste+" @docs/guide.md " {
		t.Fatalf("composer value after file completion = %q", got)
	}
	view := plainRender(t, m.composerBox(96))
	for _, want := range []string{"[Create a book library dashboard page", "lines", "@docs/guide.md"} {
		if !strings.Contains(view, want) {
			t.Fatalf("file completion view missing %q:\n%s", want, view)
		}
	}
	if strings.Contains(view, "SECRET_TAIL") {
		t.Fatalf("file completion should keep pasted content collapsed:\n%s", view)
	}
}

func TestFileSuggestionDismissKeepsPastePreviewCollapsed(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, root, "docs/guide.md")
	paste := "Create a book library dashboard page with the Bootstrap theme. " + strings.Repeat("SECRET_TAIL ", 20)
	m := newModel(context.Background(), Options{Cwd: root})
	m.width = 44

	updated, _ := m.Update(testPaste(paste))
	m = updated.(model)
	updated, _ = m.Update(testKey(tea.KeySpace))
	m = updated.(model)
	updated, _ = m.Update(testKeyText("@guide"))
	m = updated.(model)

	if !m.suggestionsActive() || !m.suggestionsAreFiles {
		t.Fatalf("expected file suggestions after @guide, got suggestions=%v files=%v", m.suggestionsActive(), m.suggestionsAreFiles)
	}
	updated, _ = m.Update(testKey(tea.KeyEsc))
	m = updated.(model)

	if got := m.composerValue(); got != paste+" " {
		t.Fatalf("composer value after dismissing file suggestion = %q", got)
	}
	if m.suggestionsActive() {
		t.Fatal("file suggestion dismiss should close the file palette")
	}
	view := plainRender(t, m.composerBox(96))
	for _, want := range []string{"[Create a book library dashboard page", "lines"} {
		if !strings.Contains(view, want) {
			t.Fatalf("file dismiss view missing %q:\n%s", want, view)
		}
	}
	if strings.Contains(view, "SECRET_TAIL") || strings.Contains(view, "@guide") {
		t.Fatalf("file dismiss should remove the query and keep pasted content collapsed:\n%s", view)
	}
}

func TestBackspaceAfterCompletedFileSuggestionKeepsPastePreviewCollapsed(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, root, "docs/guide.md")
	paste := "Create a book library dashboard page with the Bootstrap theme. " + strings.Repeat("SECRET_TAIL ", 20)
	m := newModel(context.Background(), Options{Cwd: root})
	m.width = 44

	updated, _ := m.Update(testPaste(paste))
	m = updated.(model)
	updated, _ = m.Update(testKey(tea.KeySpace))
	m = updated.(model)
	updated, _ = m.Update(testKeyText("@guide"))
	m = updated.(model)
	updated, _ = m.Update(testKey(tea.KeyEnter))
	m = updated.(model)

	if got := m.composerValue(); got != paste+" @docs/guide.md " {
		t.Fatalf("composer value after file completion = %q", got)
	}
	updated, _ = m.Update(testKey(tea.KeyBackspace))
	m = updated.(model)

	if got := m.composerValue(); got != paste+" " {
		t.Fatalf("composer value after deleting completed file mention = %q", got)
	}
	view := plainRender(t, m.composerBox(96))
	for _, want := range []string{"[Create a book library dashboard page", "lines"} {
		if !strings.Contains(view, want) {
			t.Fatalf("backspace after file completion view missing %q:\n%s", want, view)
		}
	}
	for _, unwanted := range []string{"SECRET_TAIL", "@docs/guide.md"} {
		if strings.Contains(view, unwanted) {
			t.Fatalf("backspace after file completion should keep pasted content collapsed and remove mention:\n%s", view)
		}
	}
}

func TestFileSuggestionsMatchesAndSkipsVCSDirs(t *testing.T) {
	root := t.TempDir()
	mustWrite := func(rel string) {
		t.Helper()
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite("cmd/server/main.go")
	mustWrite(".git/config")               // hidden VCS dir: must be skipped
	mustWrite("node_modules/dep/index.js") // dependency dir: must be skipped

	got := suggestionTokens(fileSuggestions(root, "main"))
	if !contains(got, "@cmd/server/main.go") {
		t.Fatalf("expected @cmd/server/main.go in %v", got)
	}

	all := suggestionTokens(fileSuggestions(root, ""))
	for _, name := range all {
		if strings.Contains(name, ".git/") || strings.Contains(name, "node_modules/") {
			t.Fatalf("walk must skip VCS/dependency dirs, got %q", name)
		}
	}
}

func TestFileSuggestionsUseWorkspaceIndexSkipRules(t *testing.T) {
	root := t.TempDir()
	mustWrite := func(rel string) {
		t.Helper()
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite("internal/keep.go")
	mustWrite("build/generated.go") // workspaceindex.ShouldSkipDir skips build
	mustWrite("assets/logo.png")    // workspaceindex.ShouldSkipFile skips binary assets

	got := suggestionTokens(fileSuggestions(root, ""))
	if !contains(got, "@internal/keep.go") {
		t.Fatalf("expected normal source file in suggestions, got %v", got)
	}
	for _, skipped := range []string{"@build/generated.go", "@assets/logo.png"} {
		if contains(got, skipped) {
			t.Fatalf("file suggestions must use workspaceindex skip rules; found %s in %v", skipped, got)
		}
	}
}

// TestFileSuggestionsBoundCountsDirectories proves the walk budget counts
// directory entries (not just files): with a tiny budget, a match that sits
// behind many directories is never reached, so the per-keystroke walk stays
// bounded in directory-heavy trees.
func TestFileSuggestionsBoundCountsDirectories(t *testing.T) {
	root := t.TempDir()
	// Many empty directories sort before "zzz" lexically, so WalkDir visits them
	// first and exhausts the budget before reaching the matching file.
	for i := 0; i < 50; i++ {
		if err := os.MkdirAll(filepath.Join(root, "dir"+string(rune('a'+i%26))+string(rune('0'+i/26))), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	deep := filepath.Join(root, "zzz")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(deep, "needle.go"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Budget smaller than the directory count: must bail before the match.
	if got := suggestionTokens(fileSuggestionsBounded(root, "needle", 5)); contains(got, "@zzz/needle.go") {
		t.Fatalf("walk should have hit the budget before the deep match, got %v", got)
	}
	// Ample budget: the match is reachable.
	if got := suggestionTokens(fileSuggestionsBounded(root, "needle", maxFileWalk)); !contains(got, "@zzz/needle.go") {
		t.Fatalf("with an ample budget the match should be found, got %v", got)
	}
}

func suggestionTokens(s []commandSuggestion) []string {
	names := make([]string, 0, len(s))
	for _, c := range s {
		names = append(names, c.Name)
	}
	return names
}

func mustWriteFile(t *testing.T, root, rel string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}
