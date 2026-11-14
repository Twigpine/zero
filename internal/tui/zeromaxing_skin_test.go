package tui

import (
	"strings"
	"testing"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/Twigpine/zero/internal/agent"
)

// THE SKIN IS ADDITIVE OR ABSENT. Posture off, every header byte matches the
// plain renderer — the sidebar cannot know this file exists. Posture on, only
// the colours change: the stripped text, the width, and the count's column are
// identical to the plain header, so every layout and hit-test measurement is
// untouched.

func skinModel(active bool) model {
	m := model{now: func() time.Time { return time.Unix(1000, 0) }}
	if active {
		m.zeromaxing = agent.ZeromaxingActive
	}
	return m
}

func TestPostureOffHeadersAreByteIdentical(t *testing.T) {
	m := skinModel(false)
	if got, want := m.postureHeader("AGENTS", 40), sidebarHeader("AGENTS", 40); got != want {
		t.Fatalf("plain header changed with the posture off:\n got %q\nwant %q", got, want)
	}
	got := m.postureHeaderWithCount("PLAN", "2/5", zeroTheme.accent, 40)
	want := sidebarHeaderWithCount("PLAN", "2/5", zeroTheme.accent, 40)
	if got != want {
		t.Fatalf("counted header changed with the posture off:\n got %q\nwant %q", got, want)
	}
}

func TestPostureOnPaintsTheLabelAndNothingElse(t *testing.T) {
	m := skinModel(true)
	painted := m.postureHeaderWithCount("MODELS", "3", zeroTheme.muted, 40)
	plain := sidebarHeaderWithCount("MODELS", "3", zeroTheme.muted, 40)
	if painted == plain {
		t.Fatal("the active posture must repaint the header")
	}
	// ONLY the colours: stripped, the two lines are the same text in the same
	// columns, so width math and hit tests cannot tell them apart.
	if ansi.Strip(painted) != ansi.Strip(plain) {
		t.Fatalf("the skin moved text, not just colour:\n got %q\nwant %q",
			ansi.Strip(painted), ansi.Strip(plain))
	}
	if ansi.Strip(m.postureHeader("AGENTS", 40)) != "AGENTS" {
		t.Fatalf("plain text lost under the paint: %q", ansi.Strip(m.postureHeader("AGENTS", 40)))
	}
}

// Idle and reduced-motion renders are pinned: the same moment renders the same
// bytes, so an idle sidebar does not shimmer between frames.
func TestPostureSkinIsStableWhenIdle(t *testing.T) {
	m := skinModel(true)
	firstRender := m.postureHeader("FILES", 40)
	secondRender := m.postureHeader("FILES", 40)
	if firstRender != secondRender {
		t.Fatal("two idle renders of the same header differ")
	}
	m.reducedMotion = true
	m.pending = true
	first := m.postureHeader("FILES", 40)
	m.now = func() time.Time { return time.Unix(1000, 0).Add(300 * time.Millisecond) }
	if got := m.postureHeader("FILES", 40); got != first {
		t.Fatal("reduced motion must pin the ramp walk")
	}
}

// The walk is real: during a turn the ramp offset advances, so the same header
// renders differently as the clock moves — that is the "alive" part.
func TestPostureSkinWalksDuringATurn(t *testing.T) {
	m := skinModel(true)
	m.pending = true
	first := m.postureHeader("AGENTS", 40)
	m.now = func() time.Time { return time.Unix(1000, 0).Add(700 * time.Millisecond) }
	second := m.postureHeader("AGENTS", 40)
	if first == second {
		t.Fatal("the ramp did not walk while a turn was in flight")
	}
	if ansi.Strip(first) != ansi.Strip(second) {
		t.Fatal("the walk must move colours, never text")
	}
}

// THE COMPOSER'S TOP BORDER is the always-on surface: rainbow gradient with
// the posture, byte-identical lineStrong rule without it.
func TestComposerTopWearsTheGradientOnlyWithThePosture(t *testing.T) {
	off := skinModel(false)
	plain := zeroTheme.lineStrong.Render("╭" + strings.Repeat("─", 38) + "╮")
	if got := off.postureComposerTop(40); got != plain {
		t.Fatalf("posture off must render the plain rule byte-identically:\n got %q\nwant %q", got, plain)
	}
	on := skinModel(true)
	painted := on.postureComposerTop(40)
	if painted == plain {
		t.Fatal("the active posture must paint the composer's top border")
	}
	if ansi.Strip(painted) != "╭"+strings.Repeat("─", 38)+"╮" {
		t.Fatalf("the gradient changed the rule's characters: %q", ansi.Strip(painted))
	}
}

// "Working" ripples through the SPECTRUM while the posture is on, and through
// the plain brand ramp when it is off — colour only, same clock, no new timer.
func TestWorkingRipplesTheSpectrumOnlyWithThePosture(t *testing.T) {
	off := skinModel(false)
	offSample := rippleText("Working", off.postureRipplePalette(), 0, 6)
	plainSample := rippleText("Working", ripplePalette(), 0, 6)
	if offSample != plainSample {
		t.Fatal("posture off must ripple the plain brand palette byte-identically")
	}
	on := skinModel(true)
	onSample := rippleText("Working", on.postureRipplePalette(), 0, 6)
	if onSample == plainSample {
		t.Fatal("the active posture must ripple the spectrum")
	}
	if ansi.Strip(onSample) != "Working" {
		t.Fatalf("the spectrum ripple changed the word: %q", ansi.Strip(onSample))
	}
}

// THE ANIMATION KNOWS WHAT KIND OF WORK IS IN FLIGHT. Idle, thinking, writing
// and orchestrating each get their own look; orchestrating outranks the rest
// because fanned-out sub-agents are the story.
func TestPostureActivityStates(t *testing.T) {
	m := skinModel(true)
	if got := m.postureActivity(); got != "" {
		t.Fatalf("idle activity = %q, want empty", got)
	}
	m.pending = true
	if got := m.postureActivity(); got != "thinking" {
		t.Fatalf("pending activity = %q, want thinking", got)
	}
	m.orchestrate.tasks = []orchestrateTask{{id: "t", status: orchestrateRunning}}
	if got := m.postureActivity(); got != "orchestrating" {
		t.Fatalf("with a running plan task = %q, want orchestrating", got)
	}
}

// Each state shapes the "Working" ripple differently; posture off is exactly
// the historical wavelength so the plain ripple is byte-identical.
func TestPostureRippleWaveLenByState(t *testing.T) {
	off := skinModel(false)
	if got := off.postureRippleWaveLen(); got != 6 {
		t.Fatalf("posture off wavelength = %d, want the historical 6", got)
	}
	on := skinModel(true)
	on.pending = true
	thinking := on.postureRippleWaveLen()
	on.orchestrate.tasks = []orchestrateTask{{id: "t", status: orchestrateRunning}}
	orchestrating := on.postureRippleWaveLen()
	if thinking == 6 || thinking == orchestrating {
		t.Fatalf("states must differ: thinking=%d orchestrating=%d", thinking, orchestrating)
	}
}

// Writing races, thinking drifts: at the same wall-clock moment the two states
// paint the bar differently, because their gradients travel at different speeds.
func TestComposerBarSpeedDiffersByState(t *testing.T) {
	m := skinModel(true)
	m.pending = true
	m.now = func() time.Time { return time.Unix(1000, 0).Add(350 * time.Millisecond) }
	thinking := m.postureComposerTop(60)
	writing := m
	writing.streamingText = []byte("streaming text")
	if got := writing.postureActivity(); got != "writing" {
		t.Fatalf("setup: streaming text must classify as writing, got %q", got)
	}
	if writing.postureComposerTop(60) == thinking {
		t.Fatal("thinking and writing painted the bar identically at the same moment")
	}
}

// ORCHESTRATING GETS THE SCANNER: a bright band sweeping the bar. It exists
// only in that state, never under reduced motion, and never changes the runes.
func TestOrchestratingScannerSweepsTheBar(t *testing.T) {
	m := skinModel(true)
	m.pending = true
	// Absolute milliseconds, so the sweep's phase is exactly the stated offset
	// into its 1800ms period rather than wherever an arbitrary epoch lands.
	m.now = func() time.Time { return time.UnixMilli(450) }
	plainState := m.postureComposerTop(60)
	m.orchestrate.tasks = []orchestrateTask{{id: "t", status: orchestrateRunning}}
	withScanner := m.postureComposerTop(60)
	if withScanner == plainState {
		t.Fatal("orchestrating must paint the scanner over the bar")
	}
	if ansi.Strip(withScanner) != ansi.Strip(plainState) {
		t.Fatal("the scanner changed the bar's characters")
	}
	// The eye moves: rising through the first half of the period…
	first := m.postureScanHead(60)
	m.now = func() time.Time { return time.UnixMilli(900) }
	second := m.postureScanHead(60)
	if first < 0 || second <= first {
		t.Fatalf("the scanner is not sweeping forward: head %d then %d", first, second)
	}
	// …and bouncing back through the second half.
	m.now = func() time.Time { return time.UnixMilli(1350) }
	if got := m.postureScanHead(60); got >= second {
		t.Fatalf("the scanner did not bounce: %d after peak %d", got, second)
	}
	m.reducedMotion = true
	if m.postureScanHead(60) != -1 {
		t.Fatal("reduced motion must disable the scanner")
	}
}

// THE RAIL: the chat|sidebar divider becomes a full-height gradient while the
// posture is on — same three cells, so column math is untouched — and stays
// the quiet single-hue rule byte-identically when it is off.
func TestTheDividerBecomesTheRailOnlyWithThePosture(t *testing.T) {
	off := skinModel(false)
	plain := " " + zeroTheme.line.Render("│") + " "
	if got := off.postureDivider(0, 40); got != plain {
		t.Fatalf("posture off divider changed:\n got %q\nwant %q", got, plain)
	}
	on := skinModel(true)
	top := on.postureDivider(0, 40)
	middle := on.postureDivider(20, 40)
	if top == plain {
		t.Fatal("the active posture must paint the rail")
	}
	// The sweep is MIRRORED — both ends share the edge hue, the middle burns
	// brightest — so the gradient shows between an end and the centre.
	if top == middle {
		t.Fatal("the rail must be a gradient: its end and its middle are the same hue")
	}
	for _, cell := range []string{top, middle} {
		if ansi.Strip(cell) != " │ " {
			t.Fatalf("the rail changed the divider's cells: %q", ansi.Strip(cell))
		}
	}
}

// THE WHOLE BOX WEARS IT: sides and bottom rule paint with the gradient, not
// just the lid — and every surface is byte-identical with the posture off.
func TestTheFullComposerBoxWearsTheGradient(t *testing.T) {
	off := skinModel(false)
	if got, want := off.postureComposerSide(false), zeroTheme.lineStrong.Render("│ "); got != want {
		t.Fatalf("posture off left wall changed: %q", got)
	}
	if got, want := off.postureComposerSide(true), zeroTheme.lineStrong.Render(" │"); got != want {
		t.Fatalf("posture off right wall changed: %q", got)
	}
	if got, want := off.postureBoxRule("╰──╯", 0, 4), zeroTheme.lineStrong.Render("╰──╯"); got != want {
		t.Fatalf("posture off bottom rule changed: %q", got)
	}
	on := skinModel(true)
	if on.postureComposerSide(false) == off.postureComposerSide(false) {
		t.Fatal("the active posture must paint the walls")
	}
	bottom := on.postureBoxRule("╰──────╯", 0, 8)
	if bottom == off.postureBoxRule("╰──────╯", 0, 8) {
		t.Fatal("the active posture must paint the bottom rule")
	}
	if ansi.Strip(bottom) != "╰──────╯" {
		t.Fatalf("the paint changed the bottom rule's characters: %q", ansi.Strip(bottom))
	}
}

// NO SEAM. The mirrored sweep means no column snaps from the accent back to
// blue mid-bar — the defect that read as "only half the box is coloured". The
// hue ramps up to the middle and back down: both edges share a hue, the middle
// differs, and adjacent columns never jump more than one ramp step.
func TestTheGradientHasNoSeam(t *testing.T) {
	ramp := postureSkinRamp()
	if len(ramp) < 2 {
		t.Skip("theme built no ramp to sweep")
	}
	hueIndex := map[string]int{}
	for i, style := range ramp {
		hueIndex[style.Render("x")] = i
	}
	span := 60
	last := -1
	for col := 0; col < span; col++ {
		rendered := postureGradientHue(ramp, col, span, 0).Render("x")
		index, ok := hueIndex[rendered]
		if !ok {
			t.Fatalf("column %d rendered a hue not in the ramp", col)
		}
		if last >= 0 {
			delta := index - last
			if delta < -1 || delta > 1 {
				t.Fatalf("hue jumped %d ramp steps at column %d — that is a seam", delta, col)
			}
		}
		last = index
	}
	edge := postureGradientHue(ramp, 0, span, 0).Render("x")
	if postureGradientHue(ramp, span-1, span, 0).Render("x") != edge {
		t.Fatal("a mirrored sweep must end on the hue it started with")
	}
	if postureGradientHue(ramp, span/2, span, 0).Render("x") == edge {
		t.Fatal("the middle must burn a different hue than the edges")
	}
}

// THE PLAN BAR JOINS THE SKIN: same layout and semantic colours, but the
// settled blocks speak ▰ and the pending track glows the gradient — and with
// the posture off it is the historical bar byte-for-byte.
func TestThePlanBarWearsTheSkinOnlyWithThePosture(t *testing.T) {
	state := orchestratePanelState{tasks: []orchestrateTask{
		{id: "a", status: orchestrateDone},
		{id: "b", status: orchestrateRunning},
		{id: "c", status: orchestratePending},
		{id: "d", status: orchestratePending},
	}}
	off := skinModel(false)
	if got, want := off.posturePlanProgressBar(state, 36), sidebarProgressBar(state, 36); got != want {
		t.Fatalf("posture off plan bar changed:\n got %q\nwant %q", got, want)
	}
	on := skinModel(true)
	skinned := on.posturePlanProgressBar(state, 36)
	if skinned == sidebarProgressBar(state, 36) {
		t.Fatal("the active posture must dress the plan bar")
	}
	plain := ansi.Strip(skinned)
	if !strings.Contains(plain, "▰") || !strings.Contains(plain, "▱") {
		t.Fatalf("the skinned bar must speak the ▰/▱ language: %q", plain)
	}
	if !strings.Contains(plain, "1/4") {
		t.Fatalf("the skinned bar lost its count: %q", plain)
	}
	if lipgloss.Width(plain) != lipgloss.Width(ansi.Strip(sidebarProgressBar(state, 36))) {
		t.Fatal("the skin changed the bar's width")
	}
}

// THE TODO CHECKLIST GETS A BAR TOO. The orchestrate plan always had one; the
// update_plan checklist showed only "0/4" in its header, so a session mid-plan
// read as barless. Posture on, the checklist's first line is the skinned bar;
// posture off, the checklist renders exactly as it always has.
func TestTheTodoPlanGetsABarUnderThePosture(t *testing.T) {
	steps := []planStep{
		{content: "one", status: "completed"},
		{content: "two", status: "in_progress"},
		{content: "three", status: "pending"},
		{content: "four", status: "pending"},
	}
	off := skinModel(false)
	off.plan.steps = steps
	for _, line := range off.updatePlanStepLines(36) {
		if strings.Contains(ansi.Strip(line), "▰") || strings.Contains(ansi.Strip(line), "1/4") {
			t.Fatalf("posture off must not grow a bar: %q", ansi.Strip(line))
		}
	}
	on := skinModel(true)
	on.plan.steps = steps
	lines := on.updatePlanStepLines(36)
	if len(lines) != len(steps)+1 {
		t.Fatalf("expected the bar plus %d steps, got %d lines", len(steps), len(lines))
	}
	first := ansi.Strip(lines[0])
	if !strings.Contains(first, "▰") || !strings.Contains(first, "1/4") {
		t.Fatalf("the checklist's first line must be the skinned bar with its count: %q", first)
	}
}

// AND THE CLICKS STILL LAND. The bar is one inserted line; every step's click
// offset must move down with it, or selecting a step opens its neighbour.
func TestPlanStepClicksSurviveTheTodoBar(t *testing.T) {
	m := sidebarTestModel()
	m.zeromaxing = agent.ZeromaxingActive
	m.plan.steps = []planStep{
		{content: "first step body", status: "in_progress"},
		{content: "second step body", status: "pending"},
	}
	width := sidebarWidth(m.width)
	if m.todoPlanBar(width) == "" {
		t.Fatal("setup: the bar must render for this check to mean anything")
	}
	rendered := m.renderContextSidebar(width, 30)
	hits := m.sidebarPlanSelectables(width)
	if len(hits) != 2 {
		t.Fatalf("expected 2 step hits, got %d", len(hits))
	}
	for i, hit := range hits {
		if hit.lineOffset >= len(rendered) {
			t.Fatalf("hit %d offset %d beyond the rendered sidebar", i, hit.lineOffset)
		}
		row := ansi.Strip(rendered[hit.lineOffset])
		want := m.plan.steps[hit.stepIndex].content
		if !strings.Contains(row, truncateStep(want, width)) && !strings.Contains(row, want[:8]) {
			t.Fatalf("hit %d points at %q, not step %q — the bar shifted the clicks", i, row, want)
		}
	}
}

// THE BAR BELONGS TO THE SKIN, NOT TO THE STOPLIGHT. Done work fills with the
// electric gradient — never the loud solid green that sat beside the skin as a
// third colour — the running head burns accent, and failure stays findable but
// CALM: dimmed red, not the shout.
func TestThePlanBarFillsWithTheGradientNotGreen(t *testing.T) {
	state := orchestratePanelState{tasks: []orchestrateTask{
		{id: "a", status: orchestrateDone},
		{id: "b", status: orchestrateDone},
		{id: "c", status: orchestrateFailed},
		{id: "d", status: orchestrateRunning},
		{id: "e", status: orchestratePending},
	}}
	on := skinModel(true)
	bar := on.posturePlanProgressBar(state, 36)
	if strings.Contains(bar, zeroTheme.green.Render("▰")) {
		t.Fatal("done cells still render solid green — the exact colour being replaced")
	}
	if strings.Contains(bar, zeroTheme.red.Render("▰")) {
		t.Fatal("failed cells still shout in full red")
	}
	if !strings.Contains(bar, zeroTheme.red.Faint(true).Render("▰")) {
		t.Fatal("failure must stay findable: no calm-red cell in the bar")
	}
	if !strings.Contains(bar, zeroTheme.accent.Bold(true).Render("▰")) {
		t.Fatal("the running head must burn accent at the fill's edge")
	}
	ramp := postureSkinRamp()
	foundGradient := false
	for i := 0; i < 36; i++ {
		if strings.Contains(bar, postureGradientHue(ramp, i, 28, 0).Render("▰")) {
			foundGradient = true
			break
		}
	}
	if !foundGradient {
		t.Fatal("done cells must pour the gradient into the bar")
	}
	// The plain bar is untouched: posture off still renders the historical
	// semantic colours byte-for-byte.
	off := skinModel(false)
	if off.posturePlanProgressBar(state, 36) != sidebarProgressBar(state, 36) {
		t.Fatal("posture off must keep the historical bar byte-identically")
	}
}
