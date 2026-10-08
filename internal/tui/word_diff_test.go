package tui

import (
	"fmt"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/alecthomas/chroma/v2"
)

func TestChangedSpan(t *testing.T) {
	cases := []struct {
		a, b               string
		p, aEnd, bEnd      int
		wantAMid, wantBMid string
	}{
		{"abXcd", "abYcd", 2, 3, 3, "X", "Y"},
		{"90", "300", 0, 1, 2, "9", "30"},
		{"same", "same", 4, 4, 4, "", ""},
		{"foo()", "foobar()", 3, 3, 6, "", "bar"},
	}
	for _, c := range cases {
		p, aEnd, bEnd := changedSpan([]rune(c.a), []rune(c.b))
		if p != c.p || aEnd != c.aEnd || bEnd != c.bEnd {
			t.Errorf("changedSpan(%q,%q) = (%d,%d,%d), want (%d,%d,%d)", c.a, c.b, p, aEnd, bEnd, c.p, c.aEnd, c.bEnd)
		}
		if got := string([]rune(c.a)[p:aEnd]); got != c.wantAMid {
			t.Errorf("a mid = %q, want %q", got, c.wantAMid)
		}
		if got := string([]rune(c.b)[p:bEnd]); got != c.wantBMid {
			t.Errorf("b mid = %q, want %q", got, c.wantBMid)
		}
	}
}

// A single-token change word-highlights; a near-rewrite falls back to whole-line.
func TestWordDiffPairGating(t *testing.T) {
	if _, _, ok := renderWordDiffPair(1, 1, "const x = 90", "const x = 300", 40, true); !ok {
		t.Error("a small single-token change should word-diff")
	}
	if _, _, ok := renderWordDiffPair(1, 1, "alpha beta gamma", "totally different line", 40, true); ok {
		t.Error("a near-rewrite should fall back to whole-line tinting")
	}
}

func TestIsolatedReplacementDetection(t *testing.T) {
	// one "-" then one "+" => isolated
	iso := []string{"@@", "-old", "+new", " ctx"}
	if !isIsolatedReplacement(iso, 1) {
		t.Error("a lone -/+ pair should be isolated")
	}
	// block: two dels then two adds => not isolated
	block := []string{"@@", "-a", "-b", "+c", "+d"}
	if isIsolatedReplacement(block, 1) {
		t.Error("a multi-line del block should not be treated as isolated")
	}
}

// The whole diff body still renders and the changed token lands on the brighter
// word background (the dark addBgWord/delBgWord), not the base band.
func TestDiffBodyWordHighlightRenders(t *testing.T) {
	d := "--- a/x\n+++ b/x\n@@ -1,1 +1,1 @@\n-timeout = 90\n+timeout = 300"
	body := diffCardBody(d, 70, cardRenderOptions{bodyCap: 20})
	joined := strings.Join(body.lines, "\n")
	// addBgWord #2e654d -> "46;101;77"; delBgWord #502d30 -> "80;45;48"
	if !strings.Contains(joined, "46;101;77") || !strings.Contains(joined, "80;45;48") {
		t.Errorf("expected changed spans on the brighter word bg, got:\n%s", joined)
	}
}

func TestMixedDiffSyntaxHighlightsCodeOnBothSides(t *testing.T) {
	previous := zeroTheme
	defer func() { zeroTheme = previous }()
	_, zeroTheme = themeForMode("nord", true)

	diff := strings.Join([]string{
		"--- a/main.go",
		"+++ b/main.go",
		"@@ -1,5 +1,8 @@",
		" package main",
		" ",
		"-import \"old\"",
		"+import \"fmt\"",
		" ",
		" func greeting(name string) string {",
		"+\tif name == \"\" {",
		"+\t\tname = \"friend\"",
		"+\t}",
		" \treturn fmt.Sprintf(\"Hello, %s!\", name)",
		" }",
	}, "\n")
	body := diffCardBody(diff, 100, cardRenderOptions{bodyCap: 20})
	joined := strings.Join(body.lines, "\n")

	addedKeyword := tokenStyle(chroma.Keyword).Background(zeroTheme.addLine.GetBackground()).Render("if")
	if !strings.Contains(joined, addedKeyword) {
		t.Fatalf("mixed diff added code must retain syntax colors, got:\n%s", joined)
	}
	contextKeyword := tokenStyle(chroma.Keyword).Render("func")
	if !strings.Contains(joined, contextKeyword) {
		t.Fatalf("mixed diff context code must retain syntax colors, got:\n%s", joined)
	}
}

func TestSystemThemeKeepsSyntaxOnAnAddedLineBand(t *testing.T) {
	previous := zeroTheme
	defer func() { zeroTheme = previous }()
	zeroTheme = buildSystemTheme()

	diff := strings.Join([]string{
		"--- /dev/null",
		"+++ b/main.go",
		"@@ -0,0 +1,2 @@",
		"+package main",
		"+func main() {}",
	}, "\n")
	body := diffCardBody(diff, 80, cardRenderOptions{bodyCap: 20})
	joined := strings.Join(body.lines, "\n")
	if !styleHasLocalBackground(zeroTheme.addLine) {
		t.Fatal("system-theme added code must use a local line background")
	}
	if want := tokenStyle(chroma.Keyword).Background(zeroTheme.addLine.GetBackground()).Render("package"); !strings.Contains(joined, want) {
		t.Fatalf("system-theme added code must retain syntax highlighting, got:\n%s", joined)
	}
	if !styleHasLocalBackground(zeroTheme.onSel(zeroTheme.ink)) {
		t.Fatal("system-theme selected rows must use a local selection background")
	}
}

func TestSystemThemeUsesMutedLocalSurfaceColors(t *testing.T) {
	theme := buildSystemTheme()
	for _, test := range []struct {
		name  string
		style lipgloss.Style
		want  string
	}{
		{"added diff", theme.addLine, "#212922"},
		{"deleted diff", theme.delLine, "#3c170f"},
		{"selection", theme.onSel(theme.ink), "#2b2f2d"},
	} {
		if got := styleBackgroundHex(t, test.style); got != test.want {
			t.Errorf("%s background = %s, want %s", test.name, got, test.want)
		}
	}
}

func styleHasLocalBackground(style lipgloss.Style) bool {
	background := style.GetBackground()
	if background == nil {
		return false
	}
	_, noColor := background.(lipgloss.NoColor)
	return !noColor
}

func styleBackgroundHex(t *testing.T, style lipgloss.Style) string {
	t.Helper()
	background := style.GetBackground()
	if background == nil {
		t.Fatal("style has no background")
	}
	r, g, b, _ := background.RGBA()
	return fmt.Sprintf("#%02x%02x%02x", r>>8, g>>8, b>>8)
}
