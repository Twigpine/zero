package tui

import (
	"image/color"
	"os"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/colorprofile"
)

// defaultReducedMotion resolves reducedMotionEnabled from the live environment
// and the env-detected color profile, at model construction.
func defaultReducedMotion() bool {
	return reducedMotionEnabled(os.Getenv, colorprofile.Env(os.Environ()))
}

// reducedMotionEnabled reports whether animations (the streaming fade AND the
// animated spinner) should be replaced with static equivalents. It is an
// explicit accessibility/preference switch via ZERO_REDUCED_MOTION, and is also
// forced when there is no TTY (animation frames are meaningless to a pipe).
// Reduced motion never removes liveness: a steady glyph plus the advancing
// elapsed timer keeps the "still working" cue. It is one switch for all motion,
// whereas ZERO_NO_FADE only governs the streaming-text fade.
func reducedMotionEnabled(env func(string) string, profile colorprofile.Profile) bool {
	if v := strings.TrimSpace(env("ZERO_REDUCED_MOTION")); v != "" && v != "0" && !strings.EqualFold(v, "false") {
		return true
	}
	return profile == colorprofile.NoTTY
}

// Streaming-text age-based fade.
//
// While the assistant reply streams in, the latest line appears in the brand
// lime ("fresh") and gradually settles to the standard off-white ink over
// ~1.2 seconds. The effect mirrors the lime spinner glyph in the liveness
// block: it's a quiet visual signal that the model is still emitting, not a
// stylized "neon glow". Per-line granularity by design — per-rune would be
// more correct for single-token streams but is ~10x the work and visually
// indistinguishable for prose.
//
// The palette is pre-computed at init() using lipgloss.Blend1D, which
// interpolates in CIELAB. Lab is the right call here even though the two
// endpoints are both near-white: the path through L*a*b* is the same cost
// as the linear sRGB loop it replaces, and it's a free upgrade for any
// future wider-ramp theme tokens.

const (
	// streamingFadeSteps is the number of discrete buckets in the age→color
	// ramp. 12 is the sweet spot: smooth at 150ms cadence, cheap to look up,
	// no per-frame float math.
	streamingFadeSteps = 12

	// streamingFadeDuration is how long a freshly-streamed line takes to
	// settle from the brand-lime "fresh" color to the off-white "ink" base.
	// 1.2s is the minimum that reads as a deliberate fade instead of a
	// flicker — the eye registers the change and the text is already solid
	// before the user re-reads it.
	streamingFadeDuration = 1200 * time.Millisecond

	// streamingFadeTickInterval is the cadence at which we re-render the
	// fading text. Independent of the active-status animation tick — a slower
	// cadence
	// is enough for a smooth-feeling fade and keeps the per-frame work
	// cheap.
	streamingFadeTickInterval = 150 * time.Millisecond
)

// streamingFadeTickMsg re-renders the streaming-text fade on the next frame.
// The Update loop schedules the FOLLOWING tick in the case branch so the
// ticker self-perpetuates while fadeActive is true.
type streamingFadeTickMsg time.Time

// streamingFadePalette holds the 12-step ramp from fresh (accent) to settled
// (ink), pre-computed at init() via lipgloss.Blend1D (CIELAB interpolation).
// Index 0 = freshest, index N-1 = closest to settled. The styles are stored
// eagerly (one lipgloss.NewStyle call per step) so per-frame lookups are a
// struct-field access and a single Render call, not a hex parse.
var streamingFadePalette [streamingFadeSteps]lipgloss.Style

// init builds the palette once at package load (after zeroTheme's var init), so
// the fade tracks the active theme's accent→ink. Cheap; before any model exists.
func init() {
	rebuildStreamingFadePalette()
}

// rebuildStreamingFadePalette regenerates the fade ramp from the active theme.
// Called at init and again by applyTheme when /theme or startup detection swaps
// the palette, so the streaming fade matches dark vs light.
func rebuildStreamingFadePalette() {
	streamingFadePalette = buildStreamingFadePalette(
		streamingFadeSteps,
		zeroTheme.accentColor,
		zeroTheme.inkColor,
	)
}

// buildStreamingFadePalette interpolates `steps` colors from fresh to base
// in CIELAB (via lipgloss.Blend1D, which uses go-colorful internally).
// Exposed for tests so they can build a 1-step or 3-step palette without
// the package-init side effect.
func buildStreamingFadePalette(steps int, fresh, base color.Color) [streamingFadeSteps]lipgloss.Style {
	var palette [streamingFadeSteps]lipgloss.Style
	if steps < 1 {
		steps = 1
	}
	if steps > streamingFadeSteps {
		steps = streamingFadeSteps
	}
	if fresh == nil || base == nil {
		// Theme didn't have a parseable color literal. Fall back to a
		// no-op palette so streamed text stays in the base color
		// rather than going neon.
		for i := 0; i < steps; i++ {
			palette[i] = lipgloss.NewStyle().Foreground(base)
		}
		return palette
	}
	blend := lipgloss.Blend1D(steps, fresh, base)
	for i, c := range blend {
		// Convert the Blend1D output to a concrete color.RGBA so
		// GetForeground() returns a stable stdlib type and tests can
		// type-assert on it. Blend1D returns a colorful.Color (the
		// go-colorful CIELAB type) which satisfies color.Color but is
		// its own package — depending on it in our public surface
		// would force a go-colorful import on every consumer.
		r, g, b, a := c.RGBA()
		palette[i] = lipgloss.NewStyle().Foreground(color.RGBA{R: uint8(r >> 8), G: uint8(g >> 8), B: uint8(b >> 8), A: uint8(a >> 8)})
	}
	return palette
}

// streamingFadeTick returns the next tea.Tick command. The Update loop
// schedules the FOLLOWING tick in the streamingFadeTickMsg case so the
// ticker self-perpetuates while fadeActive is true (and stops cleanly when
// the stream ends, since the case short-circuits to nil).
func streamingFadeTick() tea.Cmd {
	return tea.Tick(streamingFadeTickInterval, func(t time.Time) tea.Msg {
		return streamingFadeTickMsg(t)
	})
}

// ageDimLine returns `line` styled with the color bucket corresponding to
// `bornAt`'s age relative to `now`. When `bornAt.IsZero()` (no age recorded
// — test fixtures, or a stream that just started), returns `line` styled
// with `base` so direct-fixture tests and the very first frame render
// identically to the pre-fade behavior.
//
// Per-line, not per-rune, by design: cheaper, visually equivalent for
// streaming prose, and survives soft-wrap without grapheme-segmentation
// code. The cost is one palette lookup and one Render call per visible line
// per frame.
func ageDimLine(line string, bornAt, now time.Time, base lipgloss.Style) string {
	if bornAt.IsZero() {
		return base.Render(line)
	}
	age := now.Sub(bornAt)
	if age < 0 {
		// Clock skew (e.g. the test fixture hand-codes bornAt in the
		// future). Treat as freshest.
		age = 0
	}
	if age >= streamingFadeDuration {
		return base.Render(line)
	}
	// Map age in [0, dimDuration) to bucket in [0, steps). With 12 steps
	// over 1.2s the bucket width is exactly 100ms, so age = 0 → bucket 0,
	// age = 99ms → bucket 0, age = 100ms → bucket 1, …
	bucket := int(age * time.Duration(streamingFadeSteps) / streamingFadeDuration)
	if bucket >= streamingFadeSteps {
		// Floating-point edge: age = dimDuration - 1ns could still land
		// here after the int truncation. Clamp.
		bucket = streamingFadeSteps - 1
	}
	return streamingFadePalette[bucket].Render(line)
}

// streamingLineBornAt looks up the bornAt for a visual line in the streaming
// block. `lineAges` is keyed to LOGICAL lines (one entry per `\n` in
// `m.streamingText`); the markdown renderer may wrap a single logical line
// into multiple VISUAL lines, so we have to disambiguate.
//
// `visualIndex` is the index into the rendered `lines` slice; `visualCount`
// is the total visual-line count. `lastActivity` is the timestamp of the
// most recent delta and is used for the in-progress last visual line — the
// user can see exactly where the model is currently typing, so this is the
// "freshest" line by definition.
//
// The mapping rule:
//   - The last VISUAL line uses `lastActivity` (always freshest).
//   - All other visual lines use the corresponding entry in `lineAges`.
//     A single logical line that wrapped into N visual lines uses the same
//     `lineAges[k]` for all N of them — they're the same age, just wrapped.
//
// Returns time.Time{} when no age can be determined (test fixtures that
// pre-populate streamingText without lineAges); ageDimLine short-circuits
// that case to the base color.
func streamingLineBornAt(visualIndex, visualCount int, lineAges []time.Time, lastActivity time.Time) time.Time {
	if visualCount == 0 {
		return time.Time{}
	}
	if visualIndex == visualCount-1 {
		// The in-progress last line is always freshest by construction.
		return lastActivity
	}
	// Map visual index to logical index. We don't have the wrap map here
	// (the markdown renderer doesn't return one), so we use a simple
	// approximation: assume 1 visual line per logical line, which is
	// exact for lines that don't wrap and conservative for lines that do.
	// The visual line numbering is dominated by lines that don't wrap,
	// so a one-line-at-a-time mapping is correct for the majority case.
	//
	// For wrapped lines (a single logical line that produced multiple
	// visual lines), the second-and-later visual lines fall out of the
	// `visualIndex >= len(lineAges)` branch. Clamp to the last known
	// logical age so wrapped middle lines keep fading in step with their
	// siblings instead of snapping to the zero time (= base ink).
	if visualIndex < 0 {
		return time.Time{}
	}
	if visualIndex >= len(lineAges) {
		if len(lineAges) == 0 {
			return time.Time{}
		}
		return lineAges[len(lineAges)-1]
	}
	return lineAges[visualIndex]
}

// recordStreamingDelta updates the fade state after a streaming delta
// arrives. Appends a time.Time entry to lineAges for every newline in the
// delta (so the new line that just started has its own age) and updates
// lastActivity to now (so the in-progress last line stays fresh).
//
// Called from the agentTextMsg branch; the same now-time is used for every
// entry appended in one delta, which is fine — deltas are typically <1ms.
func (m *model) recordStreamingDelta(delta string) {
	now := m.now()
	if m.lineAges == nil {
		m.lineAges = []time.Time{now}
	} else {
		// Update the trailing entry's age to `now` so the line that's
		// still being filled stays fresh. The first append below (for
		// any newline in the delta) re-bumps it.
		m.lineAges[len(m.lineAges)-1] = now
	}
	for _, r := range delta {
		if r == '\n' {
			m.lineAges = append(m.lineAges, now)
		}
	}
	m.lastStreamActivity = now
}

// resetStreamingFade clears the fade state. Called on stream end (so the
// next turn starts from a clean slate) and on cancel (so the partial
// stream doesn't leave dangling state).
func (m *model) resetStreamingFade() {
	m.fadeActive = false
	m.lineAges = nil
	m.lastStreamActivity = time.Time{}
}

// styleStreamingLine applies the fade palette to one visual line of prose in the
// streaming block. Already-styled lines (markdown/code/table highlighting) are
// returned unchanged so live colors match committed colors instead of snapping at
// turn end.
func (m model) styleStreamingLine(line string, visualIndex, visualCount int) string {
	// Inline markdown uses private SGR markers while it is being parsed. They
	// include reverse-video for code spans, which must be converted to the
	// palette-aware final style before the line reaches the terminal. Otherwise
	// a streaming frame briefly flashes the terminal's reverse background.
	if hasMarkdownDisplayControls(line) {
		return styleAssistantMarkdownLine(line, zeroTheme.ink)
	}
	if strings.Contains(line, "\x1b") {
		return line
	}
	if !m.fadeActive || m.lineAges == nil {
		return zeroTheme.ink.Render(line)
	}
	bornAt := streamingLineBornAt(visualIndex, visualCount, m.lineAges, m.lastStreamActivity)
	return ageDimLine(line, bornAt, m.now(), zeroTheme.ink)
}
