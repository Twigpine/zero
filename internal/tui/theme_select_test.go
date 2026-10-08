package tui

import (
	"fmt"
	"image/color"
	"math"
	"strconv"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
)

func relLum(t *testing.T, hex string) float64 {
	t.Helper()
	h := strings.TrimPrefix(hex, "#")
	v, err := strconv.ParseUint(h, 16, 32)
	if err != nil || len(h) != 6 {
		t.Fatalf("bad hex %q", hex)
	}
	r := float64((v>>16)&0xff) / 255
	g := float64((v>>8)&0xff) / 255
	b := float64(v&0xff) / 255
	return 0.2126*r + 0.7152*g + 0.0722*b
}

// wcagRatio is the true WCAG 2.x contrast ratio (sRGB-linearized luminance), unlike
// relLum which is a cheap perceptual ordering. Used to assert AA (>=4.5) for the
// text-bearing theme tokens.
func wcagRatio(t *testing.T, fg, bg string) float64 {
	t.Helper()
	rel := func(hex string) float64 {
		h := strings.TrimPrefix(hex, "#")
		v, err := strconv.ParseUint(h, 16, 32)
		if err != nil || len(h) != 6 {
			t.Fatalf("bad hex %q", hex)
		}
		lin := func(c float64) float64 {
			c /= 255
			if c <= 0.03928 {
				return c / 12.92
			}
			return math.Pow((c+0.055)/1.055, 2.4)
		}
		return 0.2126*lin(float64((v>>16)&0xff)) + 0.7152*lin(float64((v>>8)&0xff)) + 0.0722*lin(float64(v&0xff))
	}
	l1, l2 := rel(fg), rel(bg)
	if l1 < l2 {
		l1, l2 = l2, l1
	}
	return (l1 + 0.05) / (l2 + 0.05)
}

// The word-level diff's brighter changed-span band must keep its text AA-readable
// and stay clearly distinct from the base add/del band, on both themes.
func TestDiffWordSpanContrast(t *testing.T) {
	for _, entry := range themeRegistry {
		name, pal := entry.Name, entry.Palette
		if r := wcagRatio(t, pal.addInk, pal.addBgWord); r < 4.5 {
			t.Errorf("%s: addInk on addBgWord %.2f < 4.5 (AA)", name, r)
		}
		if r := wcagRatio(t, pal.delInk, pal.delBgWord); r < 4.5 {
			t.Errorf("%s: delInk on delBgWord %.2f < 4.5 (AA)", name, r)
		}
		if sep := wcagRatio(t, pal.addBgWord, pal.addBg); sep < 1.2 {
			t.Errorf("%s: addBgWord vs addBg separation %.2f < 1.2 (span not distinct)", name, sep)
		}
		if sep := wcagRatio(t, pal.delBgWord, pal.delBg); sep < 1.2 {
			t.Errorf("%s: delBgWord vs delBg separation %.2f < 1.2 (span not distinct)", name, sep)
		}
	}
}

// The highlighted picker/autocomplete row must both stand out from the panel
// AND keep its label readable. Guards the regression this fixes: the light
// selBg (#e7f2cd) sat at 1.01 vs the panel (#ececed) — effectively invisible.
func TestSelectedRowBandIsVisibleAndReadable(t *testing.T) {
	for _, entry := range themeRegistry {
		name, pal := entry.Name, entry.Palette
		if r := wcagRatio(t, pal.ink, pal.selBg); r < 4.5 {
			t.Errorf("%s: ink on selBg contrast %.2f < 4.5 — selected-row label unreadable", name, r)
		}
		if sep := wcagRatio(t, pal.selBg, pal.panel); sep < 1.10 {
			t.Errorf("%s: selBg vs panel separation %.2f < 1.10 — selected row does not stand out", name, sep)
		}
	}
}

// Every registered theme — not just the two built-ins — must clear WCAG AA on its
// text-bearing tokens against the panel and keep the muted>faint>faintest>panel
// gray ramp monotonic in its polarity (light-on-dark for dark themes, the inverse
// for light themes). Guards that a newly-added color palette can't ship illegible.
func TestAllThemesContrastAndHierarchy(t *testing.T) {
	for _, entry := range themeRegistry {
		pal := entry.Palette
		for _, tok := range []struct {
			name string
			fg   string
		}{
			{"ink", pal.ink}, {"muted", pal.muted}, {"faint", pal.faint},
			{"faintest", pal.faintest}, {"accent", pal.accent},
		} {
			if r := wcagRatio(t, tok.fg, pal.panel); r < 4.5 {
				t.Errorf("%s %s on panel %.2f < 4.5 (WCAG AA)", entry.Name, tok.name, r)
			}
		}
		if r := wcagRatio(t, pal.onAccent, pal.accent); r < 4.5 {
			t.Errorf("%s onAccent on accent %.2f < 4.5 (WCAG AA)", entry.Name, r)
		}
		// Gray ramp ordered ink -> muted -> faint -> faintest -> panel; luminance
		// rises toward the surface on light themes, falls on dark themes.
		chain := []float64{
			relLum(t, pal.ink), relLum(t, pal.muted), relLum(t, pal.faint),
			relLum(t, pal.faintest), relLum(t, pal.panel),
		}
		for i := 1; i < len(chain); i++ {
			ok := chain[i] > chain[i-1]
			if entry.IsDark {
				ok = chain[i] < chain[i-1]
			}
			if !ok {
				t.Errorf("%s hierarchy not monotonic toward surface at %d: %v", entry.Name, i, chain)
			}
		}
	}
}

// resolveThemeMode precedence: explicit flag > ZERO_THEME env > system.
func TestResolveThemeModePrecedence(t *testing.T) {
	cases := []struct {
		flag, env string
		want      themeMode
	}{
		{"light", "dark", themeSystem}, // retired preference migrates
		{"dark", "light", themeSystem}, // retired preference migrates
		{"", "light", themeSystem},     // retired env value migrates
		{"", "dark", themeSystem},      // retired env value migrates
		{"dracula", "dune", themeMode("dracula")},
		{"", "", themeSystem}, // default
		{"garbage", "also-bad", themeSystem},
		{"AUTO", "", themeSystem},
	}
	for _, c := range cases {
		if got := resolveThemeMode(c.flag, c.env); got != c.want {
			t.Errorf("resolveThemeMode(%q,%q) = %q, want %q", c.flag, c.env, got, c.want)
		}
	}
}

func TestLegacyThemeArgumentsRemainAcceptedOutsidePicker(t *testing.T) {
	for _, value := range []string{"dark", "light"} {
		if !ValidThemeArg(value) {
			t.Errorf("ValidThemeArg(%q) = false, want true for migration compatibility", value)
		}
		if validThemeMode(value) {
			t.Errorf("validThemeMode(%q) = true, retired values must stay out of the picker", value)
		}
	}
}

// applyTheme keeps system canvas-native and mirrors named palettes when their
// intended surface polarity differs from the terminal.
func TestApplyThemeResolution(t *testing.T) {
	defer applyTheme(themeDark, true) // restore the global default
	cases := []struct {
		mode    themeMode
		darkBg  bool
		want    themeMode
		wantInk string
	}{
		{themeSystem, true, themeSystem, ""},
		{themeAuto, false, themeSystem, ""},
		{themeDark, true, themeDark, darkPalette.ink},
		{themeDark, false, themeDark, invertPaletteColor(darkPalette.ink)},
		{themeLight, false, themeLight, lightPalette.ink},
		{themeLight, true, themeLight, invertPaletteColor(lightPalette.ink)},
	}
	for _, c := range cases {
		got := applyTheme(c.mode, c.darkBg)
		if got != c.want {
			t.Errorf("applyTheme(%q, darkBg=%v) = %q, want %q", c.mode, c.darkBg, got, c.want)
		}
		if c.wantInk == "" {
			if _, ok := zeroTheme.bgPanel.(lipgloss.NoColor); !ok {
				t.Errorf("applyTheme(%q,%v): system mode painted canvas with %T", c.mode, c.darkBg, zeroTheme.bgPanel)
			}
			continue
		}
		wantR, wantG, wantB, _ := lipgloss.Color(c.wantInk).RGBA()
		gotR, gotG, gotB, _ := zeroTheme.inkColor.RGBA()
		if gotR != wantR || gotG != wantG || gotB != wantB {
			t.Errorf("applyTheme(%q,%v): zeroTheme.inkColor not the %q ink", c.mode, c.darkBg, c.want)
		}
	}
}

func TestInvertedPalettePreservesSemanticHues(t *testing.T) {
	inverted := invertedPalette(nordPalette)
	for _, test := range []struct {
		name     string
		value    string
		dominant int
	}{
		{"success", inverted.green, 1},
		{"error", inverted.red, 0},
	} {
		r, g, b, _ := lipgloss.Color(test.value).RGBA()
		channels := []uint32{r, g, b}
		for index, channel := range channels {
			if index != test.dominant && channels[test.dominant] <= channel {
				t.Errorf("inverted %s color %s lost its semantic hue", test.name, test.value)
				break
			}
		}
	}
}

// The light palette must be a real dark-on-light set: distinct from dark, ink
// well-contrasted against the panel, accent readable, and the gray hierarchy
// (ink→faintest) ordered toward the surface so it still reads on white.
func TestLightPaletteContrastAndHierarchy(t *testing.T) {
	if lightPalette.ink == darkPalette.ink || lightPalette.panel == darkPalette.panel {
		t.Fatal("light palette must differ from dark")
	}
	inkL, panelL := relLum(t, lightPalette.ink), relLum(t, lightPalette.panel)
	if panelL-inkL < 0.5 {
		t.Errorf("light ink/panel contrast too low: panel=%.2f ink=%.2f", panelL, inkL)
	}
	// AUDIT-H5/H6/M: text-bearing tokens (incl. faint/faintest, which carry line
	// numbers, diff @@/+++/---, help text, placeholders, and the accent prompt glyph)
	// must meet WCAG AA (>=4.5) against the worst-case background (the panel) — a real
	// contrast ratio, not just a luminance ordering.
	for _, tok := range []struct {
		name   string
		fg, bg string
	}{
		{"dark muted", darkPalette.muted, darkPalette.panel},
		{"dark faint", darkPalette.faint, darkPalette.panel},
		{"dark faintest", darkPalette.faintest, darkPalette.panel},
		{"dark accent", darkPalette.accent, darkPalette.panel},
		{"light muted", lightPalette.muted, lightPalette.panel},
		{"light faint", lightPalette.faint, lightPalette.panel},
		{"light faintest", lightPalette.faintest, lightPalette.panel},
		{"light accent", lightPalette.accent, lightPalette.panel},
	} {
		if r := wcagRatio(t, tok.fg, tok.bg); r < 4.5 {
			t.Errorf("%s contrast %.2f < 4.5 (WCAG AA): %s on %s", tok.name, r, tok.fg, tok.bg)
		}
	}
	// Dark-on-light: ink darkest, then progressively lighter toward the surface.
	chain := []float64{
		relLum(t, lightPalette.ink),
		relLum(t, lightPalette.muted),
		relLum(t, lightPalette.faint),
		relLum(t, lightPalette.faintest),
		relLum(t, lightPalette.panel),
	}
	for i := 1; i < len(chain); i++ {
		if !(chain[i] > chain[i-1]) {
			t.Errorf("light hierarchy not monotonic toward surface at %d: %v", i, chain)
		}
	}
	// Dark theme keeps the inverse ordering (light-on-dark).
	dchain := []float64{
		relLum(t, darkPalette.ink),
		relLum(t, darkPalette.muted),
		relLum(t, darkPalette.faint),
		relLum(t, darkPalette.faintest),
		relLum(t, darkPalette.panel),
	}
	for i := 1; i < len(dchain); i++ {
		if !(dchain[i] < dchain[i-1]) {
			t.Errorf("dark hierarchy not monotonic toward surface at %d: %v", i, dchain)
		}
	}
}

// /theme switches the active theme live and shows state with no arg.
func TestHandleThemeCommand(t *testing.T) {
	defer applyTheme(themeDark, true)
	m := model{themeMode: themeSystem, hasDarkBg: true}

	m, out := m.handleThemeCommand("dracula")
	if m.themeMode != themeMode("dracula") {
		t.Fatalf("after /theme dracula, mode = %q", m.themeMode)
	}
	if got, want := colorHex(t, zeroTheme.inkColor), draculaPalette.ink; got != want {
		t.Errorf("/theme dracula ink = %s, want %s", got, want)
	}
	if !strings.Contains(out, "dracula") {
		t.Errorf("output should confirm dracula: %q", out)
	}

	before := m.themeMode
	m, removed := m.handleThemeCommand("dark")
	if m.themeMode != before || !strings.Contains(removed, "Unknown theme") {
		t.Fatalf("retired dark theme should not be selectable: mode=%q output=%q", m.themeMode, removed)
	}

	_, state := m.handleThemeCommand("")
	if !strings.Contains(state, "active theme") {
		t.Errorf("no-arg /theme should show state: %q", state)
	}
	if _, bad := m.handleThemeCommand("solarized"); !strings.Contains(bad, "Unknown theme") {
		t.Errorf("invalid theme should error: %q", bad)
	}
}

func TestNewThemePresetsWired(t *testing.T) {
	neon, ok := lookupTheme("neon")
	if !ok {
		t.Fatal("theme 'neon' is not registered")
	}
	if !neon.IsDark {
		t.Error("theme 'neon' should be marked as dark")
	}

	dune, ok := lookupTheme("dune")
	if !ok {
		t.Fatal("theme 'dune' is not registered")
	}
	if dune.IsDark {
		t.Error("theme 'dune' should be marked as light")
	}

	for _, name := range []string{"neon", "dune"} {
		if !validThemeMode(name) {
			t.Errorf("%q should be a valid --theme/ZERO_THEME value", name)
		}
	}

	if !contains(themeModes, "neon") || !contains(themeModes, "dune") {
		t.Errorf("themeModes = %v, want it to include neon and dune (the /theme picker list)", themeModes)
	}
}

// The --theme flag and ZERO_THEME both resolve through resolveThemeMode, and
// applyTheme must actually swap zeroTheme to the resolved preset, adapting only
// its contrast direction when the terminal has the opposite polarity.
func TestNewThemePresetsResolveThroughCLIAndEnvPath(t *testing.T) {
	defer applyTheme(themeDark, true)

	if got := resolveThemeMode("dune", ""); got != themeMode("dune") {
		t.Fatalf(`resolveThemeMode("dune", "") = %q, want "dune"`, got)
	}
	if got := resolveThemeMode("", "neon"); got != themeMode("neon") {
		t.Fatalf(`resolveThemeMode("", "neon") = %q, want "neon"`, got)
	}

	applyTheme(themeMode("dune"), true)
	if got, want := colorHex(t, zeroTheme.inkColor), invertPaletteColor(dunePalette.ink); got != want {
		t.Errorf("applying dune ink = %s, want %s", got, want)
	}

	applyTheme(themeMode("neon"), true)
	if got, want := colorHex(t, zeroTheme.inkColor), neonPalette.ink; got != want {
		t.Errorf("applying neon ink = %s, want %s", got, want)
	}
}

func TestExtendedThemeContrastInvariants(t *testing.T) {
	// Skip validation for old built-in themes if they have established, non-compliant palettes,
	// but enforce strict compliance on the newly introduced 'neon' and 'dune' themes.
	for _, entry := range themeRegistry {
		if entry.Name != "neon" && entry.Name != "dune" {
			continue
		}
		name, pal := entry.Name, entry.Palette

		// Finding 1: Permission and status/success surfaces
		if r := wcagRatio(t, pal.amber, pal.permBg); r < 4.5 {
			t.Errorf("%s: amber on permBg contrast %.2f < 4.5", name, r)
		}
		if r := wcagRatio(t, pal.onAccent, pal.amber); r < 4.5 {
			t.Errorf("%s: onAccent on amber contrast %.2f < 4.5", name, r)
		}
		if r := wcagRatio(t, pal.green, pal.panel); r < 4.5 {
			t.Errorf("%s: green on panel contrast %.2f < 4.5", name, r)
		}
		if r := wcagRatio(t, pal.amber, pal.panel); r < 4.5 {
			t.Errorf("%s: amber on panel contrast %.2f < 4.5", name, r)
		}
		if r := wcagRatio(t, pal.red, pal.panel); r < 4.5 {
			t.Errorf("%s: red on panel contrast %.2f < 4.5", name, r)
		}

		// Finding 2: Selected row secondary text
		if r := wcagRatio(t, pal.faint, pal.selBg); r < 4.5 {
			t.Errorf("%s: faint on selBg contrast %.2f < 4.5", name, r)
		}
		if r := wcagRatio(t, pal.faintest, pal.selBg); r < 4.5 {
			t.Errorf("%s: faintest on selBg contrast %.2f < 4.5", name, r)
		}

		// Finding 3: Diff gutter pairings
		if r := wcagRatio(t, pal.faintest, pal.addBg); r < 4.5 {
			t.Errorf("%s: faintest on addBg contrast %.2f < 4.5", name, r)
		}
		if r := wcagRatio(t, pal.faintest, pal.delBg); r < 4.5 {
			t.Errorf("%s: faintest on delBg contrast %.2f < 4.5", name, r)
		}
		if r := wcagRatio(t, pal.green, pal.addBg); r < 4.5 {
			t.Errorf("%s: green on addBg contrast %.2f < 4.5", name, r)
		}
		if r := wcagRatio(t, pal.red, pal.delBg); r < 4.5 {
			t.Errorf("%s: red on delBg contrast %.2f < 4.5", name, r)
		}
	}
}

// hexChannels splits a #rrggbb token into its 8-bit channels.
func hexChannels(t *testing.T, hexColor string) (int, int, int) {
	t.Helper()
	h := strings.TrimPrefix(hexColor, "#")
	v, err := strconv.ParseUint(h, 16, 32)
	if err != nil || len(h) != 6 {
		t.Fatalf("bad hex %q", hexColor)
	}
	return int((v >> 16) & 0xff), int((v >> 8) & 0xff), int(v & 0xff)
}

// xterm256Hex returns the nearest xterm-256 color (the 6x6x6 cube plus the
// 24-step grayscale ramp, by squared RGB distance): how a terminal without
// truecolor support downsamples the palette's hex tokens before rendering.
func xterm256Hex(t *testing.T, hexColor string) string {
	t.Helper()
	r, g, b := hexChannels(t, hexColor)
	levels := []int{0, 95, 135, 175, 215, 255}
	bestR, bestG, bestB := 0, 0, 0
	bestDistance := math.MaxFloat64
	try := func(cr, cg, cb int) {
		d := float64((r-cr)*(r-cr) + (g-cg)*(g-cg) + (b-cb)*(b-cb))
		if d < bestDistance {
			bestDistance, bestR, bestG, bestB = d, cr, cg, cb
		}
	}
	for _, cr := range levels {
		for _, cg := range levels {
			for _, cb := range levels {
				try(cr, cg, cb)
			}
		}
	}
	for i := 0; i < 24; i++ {
		gray := 8 + 10*i
		try(gray, gray, gray)
	}
	return fmt.Sprintf("#%02x%02x%02x", bestR, bestG, bestB)
}

// Hex-level AA does not guarantee the rendered pairs hold on a 256-color
// terminal, which quantizes every token to its nearest xterm entry first.
// Guard the pairs that regressed: Dune's selected-row affordances (accent
// caret/favorite star and blue local-model dot over selBg via onSel) and
// Neon's diff bands, whose previous values all quantized to the same grays.
func TestExtendedThemeANSI256Contrast(t *testing.T) {
	palettes := map[string]palette{}
	for _, entry := range themeRegistry {
		palettes[entry.Name] = entry.Palette
	}
	q := func(hexColor string) string { return xterm256Hex(t, hexColor) }

	dune := palettes["dune"]
	for _, pair := range []struct{ name, fg, bg string }{
		{"accent on selBg", dune.accent, dune.selBg},
		{"blue on selBg", dune.blue, dune.selBg},
		{"faintest on selBg", dune.faintest, dune.selBg},
		{"ink on selBg", dune.ink, dune.selBg},
	} {
		if r := wcagRatio(t, q(pair.fg), q(pair.bg)); r < 4.5 {
			t.Errorf("dune: %s = %.2f < 4.5 after xterm-256 quantization (%s on %s)", pair.name, r, q(pair.fg), q(pair.bg))
		}
	}

	neon := palettes["neon"]
	greenish := func(hexColor string) bool {
		r, g, b := hexChannels(t, hexColor)
		return g > r && g > b
	}
	reddish := func(hexColor string) bool {
		r, g, b := hexChannels(t, hexColor)
		return r > g && r > b
	}
	if q(neon.addBg) == q(neon.delBg) || !greenish(q(neon.addBg)) || !reddish(q(neon.delBg)) {
		t.Errorf("neon: add/del row bands lose their green/red identity after quantization: addBg %s -> %s, delBg %s -> %s",
			neon.addBg, q(neon.addBg), neon.delBg, q(neon.delBg))
	}
	if q(neon.addBgWord) == q(neon.delBgWord) || !greenish(q(neon.addBgWord)) || !reddish(q(neon.delBgWord)) {
		t.Errorf("neon: word-span bands lose their green/red identity after quantization: addBgWord %s -> %s, delBgWord %s -> %s",
			neon.addBgWord, q(neon.addBgWord), neon.delBgWord, q(neon.delBgWord))
	}
	if q(neon.addBgWord) == q(neon.addBg) {
		t.Errorf("neon: changed span is indistinguishable from its add row after quantization (both %s)", q(neon.addBg))
	}
	if q(neon.delBgWord) == q(neon.delBg) {
		t.Errorf("neon: changed span is indistinguishable from its del row after quantization (both %s)", q(neon.delBg))
	}
	if r := wcagRatio(t, q(neon.green), q(neon.addBg)); r < 4.5 {
		t.Errorf("neon: green on addBg = %.2f < 4.5 after quantization", r)
	}
	if r := wcagRatio(t, q(neon.red), q(neon.delBg)); r < 4.5 {
		t.Errorf("neon: red on delBg = %.2f < 4.5 after quantization", r)
	}
	// The two rendered diff-content pairs a 256-color terminal actually
	// shows: line numbers (faintest on addBg/delBg) and highlighted changed
	// spans (addInk/delInk on their word bands).
	for _, pair := range []struct{ name, fg, bg string }{
		{"faintest on addBg", neon.faintest, neon.addBg},
		{"faintest on delBg", neon.faintest, neon.delBg},
		{"addInk on addBgWord", neon.addInk, neon.addBgWord},
		{"delInk on delBgWord", neon.delInk, neon.delBgWord},
	} {
		if r := wcagRatio(t, q(pair.fg), q(pair.bg)); r < 4.5 {
			t.Errorf("neon: %s = %.2f < 4.5 after quantization (%s on %s)", pair.name, r, q(pair.fg), q(pair.bg))
		}
	}
	// Error-card borders are non-text UI components: they need 3:1 against
	// the panel (WCAG 1.4.11) to be distinguishable at all, in both truecolor
	// and 256-color renderings.
	if r := wcagRatio(t, neon.cardErr, neon.panel); r < 3.0 {
		t.Errorf("neon: cardErr border on panel = %.2f < 3.0", r)
	}
	if r := wcagRatio(t, q(neon.cardErr), q(neon.panel)); r < 3.0 {
		t.Errorf("neon: cardErr border on panel = %.2f < 3.0 after quantization", r)
	}
}

func colorHex(t *testing.T, value color.Color) string {
	t.Helper()
	r, g, b, _ := value.RGBA()
	return fmt.Sprintf("#%02x%02x%02x", r>>8, g>>8, b>>8)
}
