package tui

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

func glowModel(t *testing.T) model {
	t.Helper()
	m := model{now: func() time.Time { return time.Unix(1000, 0) }}
	m.execProfileName = "zeromaxing"
	m.zeromaxing = 2 // ZeromaxingActive
	if !m.zeromaxingActive() {
		t.Fatal("setup: the posture must be active")
	}
	return m
}

// The chip is a LIVE WORD with NO BACKGROUND BOX. A badge signalled "on" by
// being a solid slab; this signals it by moving, and the footer keeps its calm.
//
// Asserted on the RENDERED FOOTER, not on the helper: an earlier version called
// zeromaxingGlowChip directly, and reverting the footer to plain text passed it.
func TestTheZeromaxingChipIsAnUnboxedLiveWord(t *testing.T) {
	m := glowModel(t)
	m.width, m.height = 100, 30

	footer := m.footerView(m.width)
	if !strings.Contains(ansi.Strip(footer), zeromaxingChipLabel) {
		t.Fatalf("the footer does not carry the posture label:\n%s", ansi.Strip(footer))
	}
	chip := m.zeromaxingGlowChip()
	if !strings.Contains(footer, chip) {
		t.Fatalf("the footer does not render the chip it was given:\n%s", ansi.Strip(footer))
	}
	// NO BACKGROUND FILL anywhere in the chip: the letters carry the signal.
	if strings.Contains(chip, "48;2;") {
		t.Fatalf("the posture chip still paints a background box: %q", chip)
	}
	// ...and it is genuinely multi-coloured rather than one flat colour.
	distinct := map[string]bool{}
	for _, field := range strings.Split(chip, "38;2;") {
		if end := strings.IndexByte(field, 'm'); end > 0 {
			distinct[field[:end]] = true
		}
	}
	if len(distinct) < 3 {
		t.Fatalf("the chip uses %d colours; the word is meant to be a spectrum: %q", len(distinct), chip)
	}
}

// Off means ABSENT, not dim. The footer is byte-identical without the posture,
// which is the rule every part of this feature follows.
func TestThePostureChipVanishesWhenOff(t *testing.T) {
	m := model{now: func() time.Time { return time.Unix(1000, 0) }}
	if got := m.zeromaxingGlowChip(); got != "" {
		t.Fatalf("with the posture off the chip must render nothing, got %q", got)
	}
}

// THE PULSE IS FREE, and that constraint shapes it: it advances on the spinner
// tick that is already running during a turn, and holds steady otherwise.
// ensureSpinnerTick schedules no timer on an idle session, and a chip is not a
// reason to break that.
func TestThePulseOnlyBreathesWhileThereIsWork(t *testing.T) {
	m := glowModel(t)

	// Idle: steady, at full brightness. A chip frozen on a dim frame would read
	// as a rendering fault.
	for _, ms := range []int64{0, 300, 700, 1100} {
		m.now = func() time.Time { return time.UnixMilli(ms) }
		if got := m.zeromaxingPulseGlyph(); got != "●" {
			t.Fatalf("idle at %dms rendered %q, want a steady full glyph", ms, got)
		}
	}

	// Pending: it moves.
	m.pending = true
	seen := map[string]bool{}
	for ms := int64(0); ms < zeromaxingPulsePeriod.Milliseconds(); ms += 100 {
		m.now = func() time.Time { return time.UnixMilli(ms) }
		seen[m.zeromaxingPulseGlyph()] = true
	}
	if len(seen) < 3 {
		t.Fatalf("the pulse only reached %d frames across a full period; it is not breathing", len(seen))
	}
}

// Reduced motion pins it, like every other animation here.
func TestReducedMotionStopsThePulse(t *testing.T) {
	m := glowModel(t)
	m.pending = true
	m.reducedMotion = true
	for _, ms := range []int64{0, 400, 900} {
		m.now = func() time.Time { return time.UnixMilli(ms) }
		if got := m.zeromaxingPulseGlyph(); got != "●" {
			t.Fatalf("reduced motion still animated: %q at %dms", got, ms)
		}
	}
}

// The pulse never falls off its frame table, whatever the clock says.
func TestThePulseNeverLeavesItsFrames(t *testing.T) {
	m := glowModel(t)
	m.pending = true
	valid := map[string]bool{}
	for _, frame := range zeromaxingGlowFrames {
		valid[frame] = true
	}
	valid["●"] = true
	for _, ms := range []int64{0, 1, 1399, 1400, 999999999, 1} {
		m.now = func() time.Time { return time.UnixMilli(ms) }
		if got := m.zeromaxingPulseGlyph(); !valid[got] {
			t.Fatalf("clock %dms produced %q, which is not a frame", ms, got)
		}
	}
}

// The posture is marked where it is OFFERED too, so what you are about to turn
// on looks like what you will see once it is on. Marked rather than merely
// coloured: the selected picker row already owns its background, so colour
// alone would be invisible exactly when you are looking at it.
func TestThePostureIsMarkedInTheEffortPicker(t *testing.T) {
	for _, name := range []string{"glm-5.2", "claude-sonnet-4.5", "gpt-4o"} {
		var postureLabel string
		for _, item := range (model{modelName: name}).newEffortPicker().items {
			if isZeromaxingOption(item.Value) {
				postureLabel = item.Label
			}
		}
		if postureLabel == "" {
			t.Fatalf("%s: the picker does not offer the posture at all", name)
		}
		if !strings.Contains(postureLabel, "◉") {
			t.Errorf("%s: the posture row is unmarked: %q", name, postureLabel)
		}
	}
}

// The marker is display only — the VALUE the picker hands to the command must
// stay the bare name, or selecting it would be refused as an unknown effort.
func TestTheMarkerDoesNotLeakIntoTheSelectedValue(t *testing.T) {
	m := model{modelName: "glm-5.2"}
	// Located by its LABEL, not its value: matching on the value would skip the
	// row entirely the moment the marker leaked into it, and the test would
	// pass by finding nothing.
	found := false
	for _, item := range m.newEffortPicker().items {
		if !strings.Contains(item.Label, "◉") {
			continue
		}
		found = true
		if item.Value != "zeromaxing" {
			t.Fatalf("the picker would send %q to the command, want the bare name", item.Value)
		}
		if _, out := m.handleEffortCommand(item.Value); strings.Contains(out, "Unknown reasoning effort") {
			t.Fatalf("the command refuses the value the picker offers: %s", out)
		}
	}
	if !found {
		t.Fatal("no marked row in the picker at all")
	}
}

// HOVER ON THE CHIP. It opens /effort when pressed, so it has to say so under
// the cursor — a clickable chip that looks identical to a label never teaches
// anyone it can be pressed.
func TestTheChipHighlightsUnderTheCursor(t *testing.T) {
	m := glowModel(t)
	m.width, m.height = 100, 30
	m.altScreen = true

	plain := m.zeromaxingGlowChip()
	m.hover = hoverTarget{kind: hoverZeromaxingChip}
	hovered := m.zeromaxingGlowChip()

	if plain == hovered {
		t.Fatal("the chip renders identically hovered and not, so hovering it says nothing")
	}
	if !strings.Contains(ansi.Strip(hovered), zeromaxingChipLabel) {
		t.Fatalf("the hovered chip lost its label: %q", ansi.Strip(hovered))
	}
}

// The chip's hit region is measured from the RENDERED footer, not assumed: the
// chips before it vary in width with the session's permission mode and effort.
func TestTheChipHitRegionTracksTheRenderedFooter(t *testing.T) {
	m := glowModel(t)
	m.width, m.height = 100, 30
	m.altScreen = true

	start, end, ok := m.zeromaxingChipSpan()
	if !ok {
		t.Fatal("the chip span could not be resolved from the footer")
	}
	if end <= start {
		t.Fatalf("empty chip span [%d,%d)", start, end)
	}
	row, ok := m.zeromaxingChipRow()
	if !ok {
		t.Fatal("the chip row could not be resolved")
	}

	inside := tea.MouseMotionMsg{X: start + 1, Y: row}
	if !m.zeromaxingChipAtMouse(inside) {
		t.Fatalf("a point inside the chip [%d,%d) on row %d did not hit", start, end, row)
	}
	for _, outside := range []tea.MouseMotionMsg{
		{X: maxInt(0, start-2), Y: row},
		{X: end + 2, Y: row},
		{X: start + 1, Y: maxInt(0, row-2)},
	} {
		if m.zeromaxingChipAtMouse(outside) {
			t.Fatalf("a point outside the chip (%d,%d) hit anyway", outside.X, outside.Y)
		}
	}
}

// With the posture off there is no chip, so nothing can hover or click it.
func TestTheChipIsNotHittableWhenThePostureIsOff(t *testing.T) {
	m := model{now: func() time.Time { return time.Unix(1000, 0) }}
	m.width, m.height = 100, 30
	m.altScreen = true
	if m.zeromaxingChipAtMouse(tea.MouseMotionMsg{X: 5, Y: 29}) {
		t.Fatal("the chip is hittable with the posture off")
	}
}

// Clicking it opens the effort picker — where the posture can be turned off or
// changed.
func TestClickingTheChipOpensTheEffortPicker(t *testing.T) {
	m := glowModel(t)
	m.width, m.height = 100, 30
	m.altScreen = true
	m.modelName = "glm-5.2"

	row, ok := m.zeromaxingChipRow()
	if !ok {
		t.Fatal("the chip row could not be resolved")
	}
	start, _, _ := m.zeromaxingChipSpan()

	updated, _ := m.Update(tea.MouseClickMsg{X: start + 1, Y: row, Button: tea.MouseLeft})
	m = updated.(model)
	if m.picker == nil || m.picker.kind != pickerEffort {
		t.Fatalf("clicking the chip did not open the effort picker: %#v", m.picker)
	}
}

// ...but not mid-turn: /effort refuses while a run is in flight, and a dialog
// that cannot be acted on is worse than none.
func TestClickingTheChipMidTurnOpensNothing(t *testing.T) {
	m := glowModel(t)
	m.width, m.height = 100, 30
	m.altScreen = true
	m.pending = true

	row, _ := m.zeromaxingChipRow()
	start, _, _ := m.zeromaxingChipSpan()
	updated, _ := m.Update(tea.MouseClickMsg{X: start + 1, Y: row, Button: tea.MouseLeft})
	if updated.(model).picker != nil {
		t.Fatal("a picker opened mid-turn, where it cannot be acted on")
	}
}

// NOT AMBER, and the reason is meaning rather than taste: amber fills the
// PERMISSION badge, so it is this UI's colour for "something needs your
// attention". The posture is a standing mode, not a caution.
func TestThePostureChipDoesNotWearThePermissionColour(t *testing.T) {
	m := glowModel(t)
	m.width, m.height = 100, 30

	chip := m.zeromaxingGlowChip()
	permission := zeroTheme.permBadge.Render(" PERMISSION ")
	if fill := backgroundSequence(permission); fill != "" && strings.Contains(chip, fill) {
		t.Fatal("the posture chip wears the permission badge's colour; a mode must not read as a caution")
	}
	if backgroundSequence(chip) != "" {
		t.Fatalf("the posture chip paints a background box: %q", chip)
	}
}

// backgroundSequence extracts the first background-colour escape from a
// rendered string, so two styles can be compared without hard-coding either.
func backgroundSequence(rendered string) string {
	index := strings.Index(rendered, "48;2;")
	if index < 0 {
		return ""
	}
	end := strings.IndexByte(rendered[index:], 'm')
	if end < 0 {
		return ""
	}
	return rendered[index : index+end]
}

// HOVER IS AN UNDERLINE, not a box. The chip opens /effort, so it must say it
// is pressable — and a background fill is the one thing this deliberately does
// not have, so it cannot be the hover signal either.
func TestTheHoveredChipUnderlinesWithoutABox(t *testing.T) {
	m := glowModel(t)
	m.width, m.height = 100, 30

	resting := m.zeromaxingGlowChip()
	m.hover = hoverTarget{kind: hoverZeromaxingChip}
	hovered := m.zeromaxingGlowChip()

	if resting == hovered {
		t.Fatal("the chip renders identically hovered and not, so hovering it says nothing")
	}
	if !strings.Contains(hovered, "\x1b[4") && !strings.Contains(hovered, ";4m") && !strings.Contains(hovered, "4;") {
		t.Fatalf("the hovered chip carries no underline: %q", hovered)
	}
	if backgroundSequence(hovered) != "" {
		t.Fatalf("the hovered chip painted a background box: %q", hovered)
	}
	if !strings.Contains(ansi.Strip(hovered), zeromaxingChipLabel) {
		t.Fatalf("the hovered chip lost its label: %q", ansi.Strip(hovered))
	}
}

// THE WIDTH IS UNCHANGED, which is what keeps the chip clickable. Colour adds
// ANSI bytes and no cells, and the span finder strips ANSI before locating the
// label — so a hovered chip must still be hit-testable at the same span.
func TestTheSpectrumDoesNotMoveTheChipsHitRegion(t *testing.T) {
	m := glowModel(t)
	m.width, m.height = 100, 30
	m.altScreen = true

	plainStart, plainEnd, ok := m.zeromaxingChipSpan()
	if !ok {
		t.Fatal("the chip span could not be located while resting")
	}
	m.hover = hoverTarget{kind: hoverZeromaxingChip}
	hoverStart, hoverEnd, ok := m.zeromaxingChipSpan()
	if !ok {
		t.Fatal("the chip span could not be located while hovered; the spectrum broke the hit test")
	}
	if plainStart != hoverStart || plainEnd != hoverEnd {
		t.Fatalf("the hit region moved on hover: resting [%d,%d) hovered [%d,%d)",
			plainStart, plainEnd, hoverStart, hoverEnd)
	}
	if ansi.StringWidth(m.zeromaxingGlowChip()) != zeromaxingChipWidth() {
		t.Fatalf("the hovered chip is %d cells wide, want %d",
			ansi.StringWidth(m.zeromaxingGlowChip()), zeromaxingChipWidth())
	}
}

// The shimmer rides the pulse the chip already has, so it costs no timer — and
// it holds STILL when nothing is running or motion is reduced, like every other
// animation here.
func TestTheSpectrumHoldsStillWhenNothingIsRunning(t *testing.T) {
	m := glowModel(t)
	if got := m.zeromaxingSpectrumOffset(); got != 0 {
		t.Fatalf("offset = %d on an idle session; the shimmer must hold still", got)
	}
	m.pending = true
	m.reducedMotion = true
	if got := m.zeromaxingSpectrumOffset(); got != 0 {
		t.Fatalf("offset = %d under reduced motion; it must hold still", got)
	}

	m.reducedMotion = false
	seen := map[int]bool{}
	for step := 0; step < int(zeromaxingPulsePeriod.Milliseconds()); step += 50 {
		at := time.Unix(0, 0).Add(time.Duration(step) * time.Millisecond)
		moving := m
		moving.now = func() time.Time { return at }
		offset := moving.zeromaxingSpectrumOffset()
		if offset < 0 || offset >= len(zeroTheme.spectrum) {
			t.Fatalf("offset %d is outside the ramp of %d", offset, len(zeroTheme.spectrum))
		}
		seen[offset] = true
	}
	if len(seen) < 2 {
		t.Fatalf("the shimmer never advanced across a full period: %v", seen)
	}
}

// LOWERCASE, like every other footer label beside it. Shouting was the badge's
// job and the badge is gone; the word earns attention by moving now.
func TestThePostureLabelIsLowercase(t *testing.T) {
	if zeromaxingChipLabel != strings.ToLower(zeromaxingChipLabel) {
		t.Fatalf("the footer label is %q; it must be lowercase like the labels beside it", zeromaxingChipLabel)
	}
	m := glowModel(t)
	m.width, m.height = 100, 30
	if !strings.Contains(ansiStripLine(m.footerView(m.width)), zeromaxingChipLabel) {
		t.Fatalf("the footer lost the label:\n%s", ansiStripLine(m.footerView(m.width)))
	}
}

// bandPositions reports which letters carry the travelling underline.
func bandPositions(t *testing.T, m model) string {
	t.Helper()
	rendered := m.zeromaxingGlowChip()
	out := ""
	for _, letter := range zeromaxingChipLabel {
		marker := "."
		for _, run := range strings.Split(rendered, "\x1b[") {
			if strings.HasSuffix(run, string(letter)) && strings.Contains(run, ";4;") {
				marker = "^"
			}
		}
		out += marker
	}
	return out
}

// THE HOVER FLOWS. A band travels through the letters and wraps, so hovering
// reads as motion rather than as a second static state — which is what an
// underline across the whole word would have been.
func TestTheHoverBandTravelsThroughTheWord(t *testing.T) {
	m := glowModel(t)
	m.width, m.height = 100, 30
	m.hover = hoverTarget{kind: hoverZeromaxingChip}

	seen := map[string]bool{}
	lit := 0
	for step := 0; step < int(zeromaxingFlowPeriod.Milliseconds()); step += 60 {
		at := time.Unix(0, 0).Add(time.Duration(step) * time.Millisecond)
		frame := m
		frame.now = func() time.Time { return at }
		positions := bandPositions(t, frame)
		seen[positions] = true
		if count := strings.Count(positions, "^"); count > zeromaxingFlowBand {
			t.Fatalf("the band covers %d letters, want at most %d: %s", count, zeromaxingFlowBand, positions)
		}
		if strings.Count(positions, "^") > 0 {
			lit++
		}
	}
	if len(seen) < 3 {
		t.Fatalf("the band never moved across a full period: %v", seen)
	}
	if lit == 0 {
		t.Fatal("no letter was ever lit; the hover produces no band at all")
	}
}

// RESTING carries no band: the flow is what hovering ADDS, and a resting chip
// that already flowed would make the hover say nothing.
func TestTheRestingWordCarriesNoBand(t *testing.T) {
	m := glowModel(t)
	m.width, m.height = 100, 30
	if positions := bandPositions(t, m); strings.Contains(positions, "^") {
		t.Fatalf("the resting word is banded: %s", positions)
	}
}

// THE TICK IS BOUNDED BY THE HOVER. An animation that ran on an idle session
// would be a timer nobody asked for, which is the rule ensureSpinnerTick exists
// to keep — so it starts when the cursor arrives and stops when it leaves.
func TestTheChipOnlyAsksForFramesWhileHovered(t *testing.T) {
	m := glowModel(t)
	if m.zeromaxingChipAnimating() {
		t.Fatal("the chip asks for frames while nothing hovers it")
	}
	m.hover = hoverTarget{kind: hoverZeromaxingChip}
	if !m.zeromaxingChipAnimating() {
		t.Fatal("the hovered chip asks for no frames, so its band cannot move")
	}
	m.reducedMotion = true
	if m.zeromaxingChipAnimating() {
		t.Fatal("reduced motion must stop the chip asking for frames")
	}
	m.reducedMotion = false
	m.zeromaxing = 0
	if m.zeromaxingChipAnimating() {
		t.Fatal("a chip that is not shown must not ask for frames")
	}
}

// Reduced motion pins the band, like every other animation here.
func TestReducedMotionStopsTheFlow(t *testing.T) {
	m := glowModel(t)
	m.hover = hoverTarget{kind: hoverZeromaxingChip}
	m.reducedMotion = true
	first := bandPositions(t, m)
	later := m
	later.now = func() time.Time { return time.Unix(0, 0).Add(500 * time.Millisecond) }
	if second := bandPositions(t, later); first != second {
		t.Fatalf("the band moved under reduced motion: %s then %s", first, second)
	}
}

// THE SCHEDULER MUST CONSULT IT. Asserting zeromaxingChipAnimating alone passes
// against an ensureSpinnerTick that ignores it entirely — the predicate would be
// right and the band would still never move. Sixth instance of that shape today,
// so this drives the scheduler.
func TestTheHoverActuallyStartsTheTickLoop(t *testing.T) {
	idle := glowModel(t)
	if cmd := idle.ensureSpinnerTick(); cmd != nil {
		t.Fatal("an idle session with nothing hovered scheduled a timer")
	}

	hovered := glowModel(t)
	hovered.hover = hoverTarget{kind: hoverZeromaxingChip}
	if cmd := hovered.ensureSpinnerTick(); cmd == nil {
		t.Fatal("hovering the chip scheduled no tick, so its band can never move")
	}
	// ...and it does not schedule a SECOND one while the loop already runs.
	running := glowModel(t)
	running.hover = hoverTarget{kind: hoverZeromaxingChip}
	running.spinnerTicking = true
	if cmd := running.ensureSpinnerTick(); cmd != nil {
		t.Fatal("a second timer was scheduled while the loop was already alive")
	}
}
