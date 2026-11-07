package tui

// The zeromaxing SKIN: while the posture is on, the workspace wears it.
//
// The footer chip already marks the posture — but only the footer, and a
// posture that multiplies every sub-agent's budget deserves to be visible
// wherever the work is. So the always-on surfaces take the skin's ELECTRIC
// GRADIENT (blue → brand accent) while the posture is active: the composer's
// top border, the "Working" ripple, and the sidebar's section headers —
// AGENTS, PLAN, FILES, MODELS, ACTIVITY, TASK — each walking its colours on
// the clock the chip already breathes on while a turn runs, holding a static
// gradient when idle.
//
// THE SAME RULES THE CHIP LIVES BY, inherited wholesale:
//   - Posture off renders BYTE-IDENTICAL output to before this file existed —
//     the off path delegates to the plain renderers, it does not reimplement
//     them.
//   - Every colour is blended from named theme styles (postureSkinRamp), so
//     the skin stays coherent on every theme and recolours with /theme.
//   - No timer of its own: the walk reads zeromaxingSpectrumOffset, which
//     advances on the spinner tick that already runs during a turn and pins to
//     0 when idle or under reduced motion.
//   - Width is unchanged: colour adds ANSI bytes and no cells, so every layout
//     and hit-test measurement sees the same plain text.

import (
	"image/color"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
)

// postureSkinRamp is the skin's own gradient: theme blue blended into the
// brand accent — an ELECTRIC two-tone sweep, deliberately not the chip's
// full six-hue spectrum. Rainbowing every surface read as a flag rather than
// a mode; two hues read as voltage. Blended from named theme styles only
// (the same construction as swarmPulseStyles), so it recolours with /theme
// and no hex literal appears here. Falls back to a plain accent ramp when the
// theme's colours cannot be read.
func postureSkinRamp() []lipgloss.Style {
	from := zeroTheme.blue.GetForeground()
	to := zeroTheme.accent.GetForeground()
	if from == nil || to == nil {
		return []lipgloss.Style{zeroTheme.accent}
	}
	blend := lipgloss.Blend1D(8, from, to)
	out := make([]lipgloss.Style, 0, len(blend))
	for _, c := range blend {
		r, g, b, a := c.RGBA()
		out = append(out, lipgloss.NewStyle().Foreground(color.RGBA{
			R: uint8(r >> 8), G: uint8(g >> 8), B: uint8(b >> 8), A: uint8(a >> 8)}))
	}
	return out
}

// postureSkinActive reports whether the skin should paint: the posture is on
// and a ramp exists.
func (m model) postureSkinActive() bool {
	return m.zeromaxingActive() && len(postureSkinRamp()) > 0
}

// spectrumHeaderLabel paints an uppercase section label letter-by-letter from
// the skin's gradient, bold like the plain header it replaces. The offset
// walks during a turn and is pinned when idle — the same clock as the footer
// chip.
func (m model) spectrumHeaderLabel(label string) string {
	ramp := postureSkinRamp()
	offset := m.zeromaxingSpectrumOffset()
	var out strings.Builder
	for index, letter := range []rune(strings.ToUpper(label)) {
		out.WriteString(ramp[(index+offset)%len(ramp)].Bold(true).Render(string(letter)))
	}
	return out.String()
}

// postureHeader is sidebarHeader with the skin: plain muted-bold normally,
// gradient-painted while the posture is on.
func (m model) postureHeader(label string, width int) string {
	if !m.postureSkinActive() {
		return sidebarHeader(label, width)
	}
	return m.spectrumHeaderLabel(label)
}

// postureHeaderWithCount is sidebarHeaderWithCount with the skin. The count
// keeps its own state colour either way — the skin dresses the label, never
// the data.
func (m model) postureHeaderWithCount(label, count string, countStyle lipgloss.Style, width int) string {
	if !m.postureSkinActive() {
		return sidebarHeaderWithCount(label, count, countStyle, width)
	}
	return sidebarHeaderAlign(m.spectrumHeaderLabel(label), count, countStyle, width)
}

// postureComposerTop is the composer box's top border: the plain lineStrong
// rule normally, and the electric gradient while the posture is on — the one
// element of this skin that is on screen in every session, sidebar or not,
// from the first frame. Each ramp hue owns a band of the rule rather than
// cycling per character, so it reads as one gradient sweep; the offset walks
// it during a turn on the chip's own clock and pins it when idle.
//
// The characters are untouched — same runes, same width — so composerRect,
// mouse hit-testing and every layout measurement see the identical rule.
// postureGradientHue maps a column to a ramp hue with a MIRRORED sweep: blue
// at both edges rising to the accent in the middle, ping-ponging instead of
// wrapping. The modular wrap it replaces produced a hard seam where the ramp
// snapped from accent back to blue — beside the box's own blue border that
// read as "the gradient stops halfway". A mirror has no seam: every cell is
// unmistakably painted, and the offset slides the bright peak along the run.
func postureGradientHue(ramp []lipgloss.Style, position, span, offset int) lipgloss.Style {
	if len(ramp) == 0 {
		return zeroTheme.accent
	}
	steps := 2 * len(ramp)
	p := (position*steps/maxInt(1, span) + offset) % steps
	if p < 0 {
		p += steps
	}
	if p >= len(ramp) {
		p = steps - 1 - p
	}
	return ramp[p]
}

// posturePaintRule paints each rune of a border segment by its COLUMN through
// the mirrored gradient, so segments drawn separately (the bottom rule around
// its model label, the two corners) still join into one continuous sweep.
func (m model) posturePaintRule(text string, startCol, span int) string {
	ramp := postureSkinRamp()
	offset := m.postureBarOffset()
	var out strings.Builder
	for index, r := range []rune(text) {
		out.WriteString(postureGradientHue(ramp, startCol+index, span, offset).Render(string(r)))
	}
	return out.String()
}

func (m model) postureComposerTop(width int) string {
	plain := "╭" + strings.Repeat("─", maxInt(0, width-2)) + "╮"
	if !m.postureSkinActive() {
		return zeroTheme.lineStrong.Render(plain)
	}
	runes := []rune(plain)
	scanHead := m.postureScanHead(len(runes))
	if scanHead < 0 {
		return m.posturePaintRule(plain, 0, len(runes))
	}
	ramp := postureSkinRamp()
	offset := m.postureBarOffset()
	var out strings.Builder
	for index, r := range runes {
		hue := postureGradientHue(ramp, index, len(runes), offset)
		if index >= scanHead && index < scanHead+postureScanBand {
			// The scanner burns over the gradient in the brand accent, bold — the
			// brightest thing on the bar, unmistakably a moving eye.
			hue = zeroTheme.accent.Bold(true)
		}
		out.WriteString(hue.Render(string(r)))
	}
	return out.String()
}

// postureComposerSide paints one row of the box's side walls. The walls take
// the gradient's EDGE hue — the same colour the corners carry — so the whole
// frame reads as one lit box rather than a coloured lid on a grey crate.
func (m model) postureComposerSide(right bool) string {
	if !m.postureSkinActive() {
		if right {
			return zeroTheme.lineStrong.Render(" │")
		}
		return zeroTheme.lineStrong.Render("│ ")
	}
	hue := postureGradientHue(postureSkinRamp(), 0, 1, m.postureBarOffset())
	if right {
		return hue.Render(" │")
	}
	return hue.Render("│ ")
}

// postureBoxRule paints a bottom-rule segment: plain lineStrong normally, the
// column-accurate gradient while the posture is on.
func (m model) postureBoxRule(text string, startCol, span int) string {
	if !m.postureSkinActive() {
		return zeroTheme.lineStrong.Render(text)
	}
	return m.posturePaintRule(text, startCol, span)
}

// postureRipplePalette is the working line's colour ramp: the brand dim→lime
// ripple normally, the skin's gradient while the posture is on — so "Working"
// breathes the mode's colours exactly when the session spends at zeromaxing scale.
// Same length contract as ripplePalette (rippleText handles any), same clock,
// no new timer.
func (m model) postureRipplePalette() []lipgloss.Style {
	if !m.postureSkinActive() {
		return ripplePalette()
	}
	return postureSkinRamp()
}

// postureActivity is the skin's read of WHAT KIND of work is in flight, so the
// animations can differ by state instead of one look for everything:
//
//	""             — idle, everything static
//	"thinking"     — reasoning / waiting on the model
//	"writing"      — the answer is streaming
//	"orchestrating"— a plan has tasks running RIGHT NOW
//
// Orchestrating outranks the other two: while sub-agents run, that is the story.
// Derived entirely from state the model already tracks — no new bookkeeping.
func (m model) postureActivity() string {
	if !m.pending {
		return ""
	}
	if _, _, _, _, running := m.orchestrate.counts(); running > 0 {
		return "orchestrating"
	}
	return m.workingActivity()
}

// postureRippleWaveLen shapes the "Working" ripple by state: a broad slow
// breath while thinking, a tight fast flow while writing, a mid pulse while
// orchestrating. Posture off returns exactly the historical 6, so the plain
// ripple is byte-identical.
func (m model) postureRippleWaveLen() int {
	if !m.postureSkinActive() {
		return 6
	}
	switch m.postureActivity() {
	case "writing":
		return 3
	case "orchestrating":
		return 4
	case "thinking":
		return 10
	default:
		return 6
	}
}

// postureBarPeriod is how fast the composer bar's gradient travels, by state:
// thinking drifts, writing races, orchestrating holds the thinking drift and
// adds the scanner instead (postureScanHead) — speed marks throughput, the
// scanner marks fan-out.
func (m model) postureBarPeriod() time.Duration {
	switch m.postureActivity() {
	case "writing":
		return 700 * time.Millisecond
	case "thinking", "orchestrating":
		return 2800 * time.Millisecond
	default:
		return 0
	}
}

// postureBarOffset walks the composer gradient at the state's own speed.
// Pinned to 0 when idle and under reduced motion, like every animation here.
func (m model) postureBarOffset() int {
	period := m.postureBarPeriod()
	if m.reducedMotion || period <= 0 {
		return 0
	}
	step := m.now().UnixMilli() % period.Milliseconds()
	return int(step * int64(len(postureSkinRamp())) / period.Milliseconds())
}

// postureScanPeriod is one full scanner crossing, edge to edge and back.
const postureScanPeriod = 1800 * time.Millisecond

// postureScanBand is the scanner's width in cells.
const postureScanBand = 5

// postureScanHead is the scanner's leading cell on the composer bar while
// ORCHESTRATING: a bright band sweeping left-right-left — the Cylon eye that
// says "sub-agents are fanned out and being watched". Returns -1 (no scanner)
// in every other state, when idle, and under reduced motion.
func (m model) postureScanHead(width int) int {
	if width <= postureScanBand || m.reducedMotion || m.postureActivity() != "orchestrating" {
		return -1
	}
	travel := width - postureScanBand
	period := postureScanPeriod.Milliseconds()
	step := m.now().UnixMilli() % period
	// Triangle wave: 0 → travel in the first half, back in the second.
	half := period / 2
	if step <= half {
		return int(int64(travel) * step / half)
	}
	return int(int64(travel) * (period - step) / half)
}

// postureDivider paints one row of the chat|sidebar divider: the plain quiet
// rule normally, and — while the posture is on — a cell of the ELECTRIC RAIL,
// a vertical gradient running the full height of the terminal, walking on the
// bar's clock. Same three cells (" │ ") either way, so column math and every
// hit test are untouched.
func (m model) postureDivider(row, rows int) string {
	if !m.postureSkinActive() {
		return " " + zeroTheme.line.Render("│") + " "
	}
	hue := postureGradientHue(postureSkinRamp(), row, rows, m.postureBarOffset())
	return " " + hue.Render("│") + " "
}

// todoPlanBar gives the update_plan checklist a progress bar of its own,
// UNDER THE POSTURE ONLY. The orchestrate plan has always had a bar; the todo
// checklist showed only "0/4" in its header, so a session mid-plan read as
// barless. The steps map onto the same settled/running/failed/pending shape
// the orchestrate bar draws, and the render reuses posturePlanProgressBar so
// the two bars cannot drift apart. Posture off returns "" — the checklist
// renders exactly as it always has.
func (m model) todoPlanBar(width int) string {
	if !m.postureSkinActive() || m.plan.isEmpty() {
		return ""
	}
	tasks := make([]orchestrateTask, 0, len(m.plan.steps))
	for _, step := range m.plan.steps {
		status := orchestratePending
		switch step.status {
		case "completed":
			status = orchestrateDone
		case "in_progress":
			status = orchestrateRunning
		case "failed":
			status = orchestrateFailed
		}
		tasks = append(tasks, orchestrateTask{status: status})
	}
	return m.posturePlanProgressBar(orchestratePanelState{tasks: tasks}, width)
}

// posturePlanProgressBar is the sidebar's plan bar wearing the skin: an ENERGY
// FILL. Done work pours the electric gradient into the bar cell by cell — the
// filled run is one continuous blue→accent sweep, positioned so it joins up as
// the plan advances — the running head burns accent-bold at the fill's edge,
// and the pending track sits quiet in faint ▱. The historical solid green
// blocks read as a third loud colour beside the skin and were the thing being
// asked about; the gradient IS the skin, so the bar finally belongs to it.
//
// Failure stays red — that is data — but CALM red, dimmed a step: a defect
// must be findable at a glance without shouting over the whole column.
// Posture off delegates to the plain renderer byte-for-byte.
func (m model) posturePlanProgressBar(state orchestratePanelState, width int) string {
	if !m.postureSkinActive() {
		return sidebarProgressBar(state, width)
	}
	ramp := postureSkinRamp()
	offset := m.postureBarOffset()
	return sidebarProgressBarWith(state, width, progressBarSkin{
		settled: "▰",
		running: "▰",
		paint: func(kind string, startCell, n, cells int) string {
			var out strings.Builder
			for i := 0; i < n; i++ {
				var hue lipgloss.Style
				switch kind {
				case "done":
					hue = postureGradientHue(ramp, startCell+i, cells, offset)
				case "failed":
					hue = zeroTheme.red.Faint(true)
				case "skipped":
					hue = zeroTheme.muted
				default: // running — the bright head of the fill
					hue = zeroTheme.accent.Bold(true)
				}
				out.WriteString(hue.Render("▰"))
			}
			return out.String()
		},
		pending: func(_, n, _ int) string {
			return zeroTheme.faint.Render(strings.Repeat("▱", n))
		},
	})
}
