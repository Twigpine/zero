package tui

import (
	"fmt"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/alecthomas/chroma/v2"
	"github.com/charmbracelet/colorprofile"
)

func TestStreamingCodeRendersHighlighted(t *testing.T) {
	m := model{
		streamingText: []byte("```go\nfunc main() {}\n```"),
		pending:       true,
	}
	out := m.interimBlock(80)
	if !strings.Contains(out, "\x1b[") {
		t.Fatalf("live code should be highlighted before commit, got:\n%s", out)
	}
	if !strings.Contains(plainRender(t, out), "func main() {}") {
		t.Fatalf("live code should keep the plain content, got:\n%s", out)
	}
}

func TestStreamingInlineCodeUsesFinalStyleWithoutReverseVideoFlash(t *testing.T) {
	previousProfile := lipgloss.Writer.Profile
	lipgloss.Writer.Profile = colorprofile.TrueColor
	defer func() { lipgloss.Writer.Profile = previousProfile }()

	m := model{
		streamingText: []byte("Use `calculator.go` and run `go test ./...`."),
		pending:       true,
	}
	out := m.interimBlock(90)
	if strings.Contains(out, markdownCodeStart) || strings.Contains(out, markdownCodeEnd) {
		t.Fatalf("streaming inline code must not emit reverse-video markers: %q", out)
	}
	for _, code := range []string{"calculator.go", "go test ./..."} {
		if want := inlineCodeStyle().Render(code); !strings.Contains(out, want) {
			t.Fatalf("streaming inline code %q should use its final style:\n%s", code, out)
		}
	}
}

func TestFinalBarePythonCodeRendersHighlighted(t *testing.T) {
	row := transcriptRow{
		kind:  rowAssistant,
		final: true,
		text: strings.Join([]string{
			"from datetime import datetime",
			"",
			"def print_current_time():",
			"    print(datetime.now().strftime(\"%Y-%m-%d %H:%M:%S\"))",
			"",
			"if __name__ == \"__main__\":",
			"    print_current_time()",
		}, "\n"),
	}
	out := renderAssistantRow(row, 90)
	if !strings.Contains(out, "\x1b[") {
		t.Fatalf("final bare Python code should be syntax-highlighted, got:\n%s", out)
	}
	plain := plainRender(t, out)
	if !strings.Contains(plain, "from datetime import datetime") ||
		!strings.Contains(plain, "def print_current_time():") ||
		!strings.Contains(plain, `if __name__ == "__main__":`) ||
		!strings.Contains(plain, "print_current_time()") {
		t.Fatalf("final bare Python code should keep content, got:\n%s", out)
	}
	for _, wantStyled := range []string{"from", "def", "if"} {
		if !strings.Contains(out, tokenStyle(chroma.Keyword).Render(wantStyled)) {
			t.Fatalf("final bare Python code should color keyword %q, got:\n%s", wantStyled, out)
		}
	}
}

func TestSelectablePaletteUsesItsSyntaxTheme(t *testing.T) {
	for _, entry := range themeRegistry {
		if entry.Name == string(themeDark) || entry.Name == string(themeLight) {
			continue // legacy aliases resolve to System and are not user-selectable
		}
		t.Run(entry.Name, func(t *testing.T) {
			_, theme := themeForMode(themeMode(entry.Name), entry.IsDark)
			if theme.codeTheme == nil {
				t.Fatalf("%s should select its palette syntax theme", entry.Name)
			}
			want := theme.codeTheme.keyword.foreground
			got := styleForegroundHex(t, tokenStyleForTheme(theme, chroma.Keyword))
			if got != want {
				t.Fatalf("keyword foreground = %q, want %q", got, want)
			}
		})
	}
}

func TestInvertedPaletteRetainsContrastSafeSyntaxFallback(t *testing.T) {
	_, theme := themeForMode("nord", false)
	if theme.codeTheme != nil {
		t.Fatal("an inverted dark palette must not use a mismatched dark syntax style")
	}
	got := styleForegroundHex(t, tokenStyleForTheme(theme, chroma.Keyword))
	want := styleForegroundHex(t, theme.accent)
	if got != want {
		t.Fatalf("fallback keyword foreground = %q, want palette accent %q", got, want)
	}
}

func TestDiffSyntaxKeepsThemeColorOverDiffSurface(t *testing.T) {
	previous := zeroTheme
	defer func() { zeroTheme = previous }()
	_, zeroTheme = themeForMode("nord", true)

	lines, ok := highlightCodeForPath([]string{"func main() {}"}, "main.go", 80, zeroTheme.addLine.GetBackground())
	if !ok || len(lines) != 1 {
		t.Fatalf("highlightCodeForPath = %#v, %t", lines, ok)
	}
	want := tokenStyle(chroma.Keyword).Background(zeroTheme.addLine.GetBackground()).Render("func")
	if !strings.Contains(lines[0], want) {
		t.Fatalf("diff syntax should retain themed foreground over the add surface:\n%s", lines[0])
	}
	if got := styleForegroundHex(t, tokenStyle(chroma.Keyword)); got != "#81a1c1" {
		t.Fatalf("Nord keyword color = %q, want #81a1c1", got)
	}
}

func TestInlineCodeAndShellCommandsUseTheActiveSyntaxPalette(t *testing.T) {
	previous := zeroTheme
	defer func() { zeroTheme = previous }()
	_, zeroTheme = themeForMode("nord", true)

	inline := styleAssistantMarkdownLine(renderMarkdownInline("Run `gofmt` before committing."), zeroTheme.ink)
	if want := inlineCodeStyle().Render("gofmt"); !strings.Contains(inline, want) {
		t.Fatalf("inline code should use the active palette:\n%s", inline)
	}

	command, ok := highlightShellCommand("gofmt -w calculator.go && go run . divide 9 2")
	if !ok {
		t.Fatal("bash command should have a cached syntax lexer")
	}
	if plain := ansiPattern.ReplaceAllString(command, ""); plain != "gofmt -w calculator.go && go run . divide 9 2" {
		t.Fatalf("command highlight changed visible command to %q", plain)
	}
	if !strings.Contains(command, "\x1b[") {
		t.Fatalf("shell command should be syntax styled, got %q", command)
	}
	for token, style := range map[string]lipgloss.Style{
		"gofmt": tokenStyle(chroma.NameFunction),
		"-w":    tokenStyle(chroma.NameAttribute),
		"9":     tokenStyle(chroma.LiteralNumber),
	} {
		if want := style.Render(token); !strings.Contains(command, want) {
			t.Fatalf("shell token %q should use its semantic style:\n%s", token, command)
		}
	}
}

func styleForegroundHex(t *testing.T, style lipgloss.Style) string {
	t.Helper()
	foreground := style.GetForeground()
	if foreground == nil {
		t.Fatal("style has no foreground")
	}
	r, g, b, _ := foreground.RGBA()
	return fmt.Sprintf("#%02x%02x%02x", r>>8, g>>8, b>>8)
}

func TestBareFencedCodeInfersLanguage(t *testing.T) {
	out := strings.Join(renderAssistantMarkdownText("```\nfrom datetime import datetime\nprint(datetime.now())\n```", 90, 90, true), "\n")
	if !strings.Contains(out, "\x1b[") {
		t.Fatalf("bare fenced Python code should infer language and highlight, got:\n%s", out)
	}
}

func TestPlainProseDoesNotTriggerBareCodeHighlight(t *testing.T) {
	out := strings.Join(renderAssistantMarkdownText("from here we continue with the explanation", 90, 90, true), "\n")
	if strings.Contains(out, "\x1b[") {
		t.Fatalf("plain prose should not be treated as bare code, got:\n%s", out)
	}
}

func TestBareCodeHighlightRequiresBlockSignal(t *testing.T) {
	out := strings.Join(renderAssistantMarkdownText("for these reasons:\nreturn later with a decision", 90, 90, true), "\n")
	if strings.Contains(out, "\x1b[") {
		t.Fatalf("ordinary prose should not be highlighted as bare code, got:\n%s", out)
	}
}

func TestStreamingMarkdownStablePrefixUsesRenderCache(t *testing.T) {
	defaultRenderCache.clear()
	text := "Here is the script:\n```go\nfunc main() {}\n```\nDone."
	_ = renderStreamingAssistantMarkdownText(text, 90, 90)
	before := defaultRenderCache.stats()
	_ = renderStreamingAssistantMarkdownText(text, 90, 90)
	after := defaultRenderCache.stats()
	if after.Hits <= before.Hits {
		t.Fatalf("streaming stable markdown should reuse highlighted cache, before=%+v after=%+v", before, after)
	}
}

func TestStreamingBuffersOpenFencedCodeBlock(t *testing.T) {
	open := model{
		streamingText: []byte("Here is the script:\n```python\nfrom datetime import datetime\nprint(datetime.now())"),
		pending:       true,
	}
	openOut := plainRender(t, open.interimBlock(90))
	if !strings.Contains(openOut, "Here is the script:") {
		t.Fatalf("streaming prose before code should remain visible, got:\n%s", openOut)
	}
	if strings.Contains(openOut, "datetime") || strings.Contains(openOut, "print(") {
		t.Fatalf("open fenced code should be buffered until the closing fence, got:\n%s", openOut)
	}

	closed := model{
		streamingText: []byte(string(open.streamingText) + "\n```"),
		pending:       true,
	}
	closedOut := closed.interimBlock(90)
	closedPlain := plainRender(t, closedOut)
	if !strings.Contains(closedPlain, "from datetime import datetime") || !strings.Contains(closedPlain, "print(datetime.now())") {
		t.Fatalf("closed fenced code should appear as one block, got:\n%s", closedOut)
	}
	if !strings.Contains(closedOut, "\x1b[") {
		t.Fatalf("closed streaming code should be highlighted, got:\n%s", closedOut)
	}
}

// highlightCode must fall back (ok=false) on a missing/unknown language so the
// caller renders the block plain — never worse than today — and must preserve
// the line structure of a known language.
func TestHighlightCodeFallbackAndLineCount(t *testing.T) {
	if _, ok := highlightCode([]string{"x := 1"}, "", 80); ok {
		t.Error("empty language must fall back (ok=false) so the caller renders plain")
	}
	if _, ok := highlightCode([]string{"x"}, "definitely-not-a-language", 80); ok {
		t.Error("unknown language must fall back")
	}
	out, ok := highlightCode([]string{"package main", "func main() {}"}, "go", 80)
	if !ok {
		t.Fatal("go must have a lexer")
	}
	if len(out) != 2 {
		t.Fatalf("expected 2 highlighted lines (structure preserved), got %d: %#v", len(out), out)
	}
}

// A line longer than the measure wraps at the token level (never loses content).
func TestHighlightCodeWraps(t *testing.T) {
	long := "x := 1 + 2 + 3 + 4 + 5 + 6 + 7 + 8 + 9 + 10 + 11 + 12"
	out, ok := highlightCode([]string{long}, "go", 20)
	if !ok {
		t.Fatal("go must have a lexer")
	}
	if len(out) < 2 {
		t.Fatalf("a long line should wrap into multiple rows, got %d", len(out))
	}
}
