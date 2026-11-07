package tui

import (
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// The zeromaxing posture is a LIVE WORD, not a filled box.
//
// It raises a cost multiplier — 480 turns per run, inherited by every sub-agent
// — so "is it on?" must be answerable from across the room. A badge did that by
// being a solid slab; this does it by MOVING, which the eye catches at least as
// well and which costs the footer none of its calm. There is no background
// fill: the letters carry the whole signal.
//
// It is also no longer amber, and that part was a meaning fix rather than a
// taste one — amber fills the PERMISSION badge, so it is this UI's colour for
// "something needs your attention", and a standing mode is not a caution.
//
// EVERY COLOUR COMES FROM THE PALETTE. The ramp is built in buildTheme from
// accent/green/blue/amber/red, so the word stays coherent on dracula, on a
// light theme, and on whatever is added next.
//
// THE PULSE IS FREE. It advances on the spinner tick that is already running
// while a turn is in flight, and holds a steady lit state when nothing is
// animating. ensureSpinnerTick deliberately schedules no timer on an idle
// session — "an idle plain session schedules no timer" — and a chip is not a
// reason to break that. So the glow breathes exactly while there is work to
// breathe with, and simply stays on otherwise.
//
// Reduced motion pins it to the steady state, like every other animation here.

// zeromaxingGlowFrames are the pulse's brightness steps, cycled on the spinner
// tick. Symmetric, so it breathes in and out rather than snapping back.
var zeromaxingGlowFrames = []string{"◦", "•", "●", "◉", "●", "•"}

// zeromaxingPulsePeriod is how long one full breath takes. Slow: this marks a
// standing state, not activity, and a fast blink beside a working spinner reads
// as a second thing happening.
const zeromaxingPulsePeriod = 1400 * time.Millisecond

// zeromaxingGlowChip renders the footer's posture indicator.
//
// Returns "" when the posture is off, so the footer is byte-identical without
// it — the same rule every other part of this feature follows.
func (m model) zeromaxingGlowChip() string {
	if !m.zeromaxingActive() {
		return ""
	}
	marker := m.zeromaxingPulseGlyph()
	// HOVER IS A FLOW, not a box and not a static rule. The chip is clickable —
	// it opens /effort — so it has to say so under the cursor, and a background
	// fill is the one thing this deliberately does not have. A band travelling
	// through the letters says "pressable" by moving, which is the same language
	// the rest of the chip already speaks.
	return m.zeromaxingSpectrumLabel(marker, m.hover.kind == hoverZeromaxingChip)
}

// zeromaxingSpectrumLabel paints each character its own hue.
//
// WIDTH IS UNCHANGED, which is what keeps the chip clickable: colour adds ANSI
// bytes and no cells, and zeromaxingChipSpan strips ANSI before locating the
// label, so the hit test sees the same plain string either way.
//
// The ramp ROTATES on the same tick the glyph breathes on, so the word walks
// its colours while a turn is in flight and holds a static rainbow when idle.
// That costs nothing: it reads the clock the spinner already schedules and adds
// no timer of its own — an idle session still schedules none, which is the rule
// the pulse was built around, and a footer that animated forever would be a
// timer nobody asked for.
func (m model) zeromaxingSpectrumLabel(marker string, hovered bool) string {
	ramp := zeroTheme.spectrum
	letters := []rune(zeromaxingChipLabel)
	// THE FLOW: a short band that travels left to right through the word and
	// wraps, so hovering reads as motion rather than as a second static state.
	// Only the band is underlined, which is what makes it a wave instead of a
	// rule under the whole label.
	head := -1
	if hovered {
		head = m.zeromaxingFlowHead(len(letters))
	}
	inBand := func(index int) bool {
		if head < 0 {
			return false
		}
		span := len(letters) + zeromaxingFlowBand
		distance := (index - head + span) % span
		return distance < zeromaxingFlowBand
	}
	paint := func(style lipgloss.Style, text string, lit bool) string {
		style = style.Bold(true)
		if lit {
			style = style.Underline(true)
		}
		return style.Render(text)
	}
	if len(ramp) == 0 {
		return paint(zeroTheme.accent, " "+marker+" "+zeromaxingChipLabel+" ", hovered)
	}
	offset := m.zeromaxingSpectrumOffset()
	var out strings.Builder
	out.WriteString(paint(zeroTheme.faint, " ", false))
	out.WriteString(paint(ramp[offset%len(ramp)], marker, false))
	out.WriteString(paint(zeroTheme.faint, " ", false))
	for index, letter := range letters {
		out.WriteString(paint(ramp[(index+offset)%len(ramp)], string(letter), inBand(index)))
	}
	out.WriteString(paint(zeroTheme.faint, " ", false))
	return out.String()
}

// zeromaxingChipAnimating reports that the chip needs frames of its own.
//
// ONLY WHILE HOVERED, and that bound is the whole justification: an animation
// that ran on an idle session would be a timer nobody asked for, which is the
// rule ensureSpinnerTick exists to keep. A cursor resting on a control is a
// direct interaction, and it stops the moment the cursor leaves.
//
// The resting word needs no tick: it walks its colours on the spinner that is
// already running during a turn, and holds still otherwise.
func (m model) zeromaxingChipAnimating() bool {
	return m.zeromaxingActive() && m.hover.kind == hoverZeromaxingChip && !m.reducedMotion
}

// zeromaxingFlowBand is how many letters the travelling highlight covers. Three
// reads as a moving band; one reads as a blinking character, and the whole word
// reads as a static underline.
const zeromaxingFlowBand = 3

// zeromaxingFlowPeriod is how long the band takes to cross the word once.
const zeromaxingFlowPeriod = 900 * time.Millisecond

// zeromaxingFlowHead is the band's leading letter.
//
// Wall clock, not a frame counter, so the band moves at the same speed whatever
// cadence the ticks arrive at — and it is pinned under reduced motion, like
// every other animation here.
func (m model) zeromaxingFlowHead(letters int) int {
	if m.reducedMotion || letters <= 0 {
		return 0
	}
	span := int64(letters + zeromaxingFlowBand)
	period := zeromaxingFlowPeriod.Milliseconds()
	if period <= 0 {
		return 0
	}
	step := m.now().UnixMilli() % period
	return int(step * span / period)
}

// zeromaxingSpectrumOffset rotates the ramp with the pulse. Pinned to 0 under
// reduced motion and when nothing is running, like every other animation here.
func (m model) zeromaxingSpectrumOffset() int {
	if m.reducedMotion || !m.pending {
		return 0
	}
	period := zeromaxingPulsePeriod.Milliseconds()
	if period <= 0 {
		return 0
	}
	step := m.now().UnixMilli() % period
	return int(step * int64(len(zeroTheme.spectrum)) / period)
}

// zeromaxingChipWidth is the chip's rendered cell width, used to hit-test it.
// Derived from the same string the renderer builds, so the two cannot drift.
// zeromaxingChipLeadColumns is the " ● " that precedes the label inside the
// badge, measured in COLUMNS. Three columns, five bytes — the distinction that
// put the chip's clickable span in the wrong place.
const zeromaxingChipLeadColumns = 3

func zeromaxingChipWidth() int {
	return lipgloss.Width(" ● " + zeromaxingChipLabel + " ")
}

// zeromaxingChipAtMouse reports whether the cursor is over the footer chip.
//
// The chip sits at the END of the footer's left run, so its span is measured
// from the rendered footer rather than assumed: the chips before it (permission
// mode, effort) vary in width with the session.
func (m model) zeromaxingChipAtMouse(msg tea.MouseMsg) bool {
	// A FAST PATH, not the enforcement: with the posture off the footer carries
	// no chip, so the span lookup below fails anyway. Removing this guard does
	// not make the chip hittable — it just renders the footer to find that out.
	if !m.zeromaxingActive() || !m.altScreen || m.height <= 0 {
		return false
	}
	row, ok := m.zeromaxingChipRow()
	if !ok || mouseY(msg) != row {
		return false
	}
	start, end, ok := m.zeromaxingChipSpan()
	if !ok {
		return false
	}
	x := mouseX(msg)
	return x >= start && x < end
}

// zeromaxingStatusRows returns the footer rows the STATUS LINE occupies, and the
// screen row the first of them sits on.
//
// THE SEARCH IS NARROWED TO THE STATUS LINE because the label is a word, and the
// footer is not only chips. footerView also renders the plan panel, the idle
// hints, a queued-message preview and THE COMPOSER — so scanning every footer row
// for "zeromaxing" meant typing the word into the composer put the composer's own
// row ahead of the chip's, and the hit test then answered with a row the chip is
// not on. A task prompt containing the word did the same through the plan panel.
// The chip renders in statusLine and nowhere else, so that is the only row range
// a hit test may consider.
//
// Derived from the SAME footer string the screen shows, and located by rendering
// the status line separately and taking that many rows off the end — the status
// line is the last thing footerView writes, on every branch.
func (m model) zeromaxingStatusRows() ([]string, int) {
	width := m.chatColumnWidth()
	footer := viewLines(m.footerView(width))
	status := viewLines(m.statusLine(width))
	if len(status) == 0 || len(status) > len(footer) {
		return nil, 0
	}
	return footer[len(footer)-len(status):], m.height - len(status)
}

// zeromaxingChipRow is the status row the chip renders on.
func (m model) zeromaxingChipRow() (int, bool) {
	rows, top := m.zeromaxingStatusRows()
	for index, line := range rows {
		if strings.Contains(ansiStripLine(line), zeromaxingChipLabel) {
			// Status rows sit at the bottom of the screen.
			return top + index, true
		}
	}
	return 0, false
}

// zeromaxingChipSpan is the chip's [start,end) column range on its row.
func (m model) zeromaxingChipSpan() (int, int, bool) {
	rows, _ := m.zeromaxingStatusRows()
	for _, line := range rows {
		plain := ansiStripLine(line)
		index := strings.Index(plain, zeromaxingChipLabel)
		if index < 0 {
			continue
		}
		// COLUMNS, NOT BYTES. strings.Index returns a byte offset, and this is a
		// screen coordinate: every multi-byte rune earlier on the footer row —
		// the "●" in the permission chip, an em dash, a non-ASCII branch or
		// directory name — pushed the byte offset past the real column and moved
		// the whole span right, so clicks landed beside the chip while the hover
		// highlight sat on it. lipgloss.Width measures what the terminal draws.
		//
		// The label is preceded by " ● " inside the badge: three COLUMNS, five
		// bytes, which is the same confusion in miniature.
		start := maxInt(0, lipgloss.Width(plain[:index])-zeromaxingChipLeadColumns)
		return start, start + zeromaxingChipWidth(), true
	}
	return 0, 0, false
}

// zeromaxingPulseGlyph picks the current breath frame. Steady when nothing is
// animating, so the chip never freezes mid-pulse on a dimmed frame — a half-lit
// chip on an idle session would read as a rendering fault.
func (m model) zeromaxingPulseGlyph() string {
	if m.reducedMotion || !m.pending {
		return "●"
	}
	step := m.now().UnixMilli() % zeromaxingPulsePeriod.Milliseconds()
	index := int(step * int64(len(zeromaxingGlowFrames)) / zeromaxingPulsePeriod.Milliseconds())
	if index < 0 || index >= len(zeromaxingGlowFrames) {
		return "●"
	}
	return zeromaxingGlowFrames[index]
}

// zeromaxingOptionValue is the posture's name as it appears in option lists.
// Declared here rather than reaching into execprofile so this file has no
// dependency beyond rendering.
const zeromaxingOptionValue = "zeromaxing"

// ansiStripLine removes styling so a rendered row can be measured in cells.
func ansiStripLine(line string) string { return ansi.Strip(line) }

// isZeromaxingOption reports whether a picker/palette entry is the posture.
func isZeromaxingOption(value string) bool {
	return strings.EqualFold(strings.TrimSpace(value), zeromaxingOptionValue)
}
