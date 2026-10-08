// sidebar.go retains compact data-formatting helpers used by the run-details
// surface. Conversation rendering is single-column.
package tui

import (
	"fmt"
	"image/color"
	"regexp"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/Gitlawb/zero/internal/tools"
)

// sidebar geometry. The sidebar takes ~30% of the width, clamped so it never
// crowds the chat on a narrow terminal nor sprawls on a wide one. A 1-cell
// divider sits between the two columns.
const (
	sidebarMinWidth  = 26
	sidebarMaxWidth  = 40
	sidebarMinColumn = 60 // below this total width the sidebar is suppressed
)

// sidebarWidth returns the sidebar column width for a given total width, or 0
// when the terminal is too narrow to justify a second column (the caller then
// renders the single-column chat at full width).
func sidebarWidth(total int) int {
	if total < sidebarMinColumn {
		return 0
	}
	return clamp(total*30/100, sidebarMinWidth, sidebarMaxWidth)
}

// sidebarActive is retained for the compact data helpers and legacy file-detail
// interactions. The persistent two-column layout is intentionally no longer
// rendered by transcriptView.
func (m model) sidebarActive() bool {
	return !m.sidebarHidden && m.sidebarAvailable()
}

// sidebarAvailable reports whether the two-column layout CAN render: only in
// alt-screen managed mode, with a measured height, on a wide-enough terminal,
// outside subchat/overlays, with real conversation. It ignores the user's Ctrl+B
// hide preference (sidebarHidden) so the toggle handler can tell whether Ctrl+B
// would have any visible effect. The subchat drill-in keeps its own single-column
// view, so the sidebar is suppressed there.
func (m model) sidebarAvailable() bool {
	if !m.altScreen || m.height <= 0 || m.subchat.active || m.transcriptDetailed {
		return false
	}
	if sidebarWidth(m.width) <= 0 {
		return false
	}
	// Only split once the chat column survives it: require the medium tier (>=80
	// cols). Between 60-79 the sidebar would starve the chat to ~30 cells, so the
	// layout commits to two healthy columns or stays cleanly single-column.
	if widthTier(m.width) < tierMedium {
		return false
	}
	// Full-screen overlays (setup, wizards, pickers) take over the chat column and
	// render at full width; suppress the second column while any is active so
	// their geometry and mouse hit-testing stay full-width as before.
	//
	// The `/` command palette is deliberately NOT in this list. It is not a
	// full-screen overlay: suggestionOverlay draws it via centerRenderedBlock at
	// minInt(width, suggestionPaletteMaxWidth) — a centred box capped at 76 cells
	// — floating over the chat column rather than replacing it. Suppressing the
	// sidebar for it collapsed the whole layout on every keystroke of `/`: the
	// plan panel dropped out of the sidebar and re-rendered inline at the bottom
	// and the transcript re-wrapped to full width, which is at its most jarring
	// mid-run with a live plan on screen. Since the sidebar already requires the
	// medium tier (>= 80 cells) the palette is always strictly narrower than the
	// chat column here, so the two never contend for the same cells.
	//
	// Mouse hit-testing is unaffected: sidebarLineAtMouse carries its own
	// suggestionsActive() guard, so clicks still go to the palette and not to the
	// sidebar rows underneath it.
	if m.setup.visible || m.helpOverlay || m.leaderHelpOverlay || m.providerWizard != nil || m.mcpAddWizard != nil ||
		m.mcpManager != nil || m.picker != nil || m.renamePrompt != nil {
		return false
	}
	// Home/welcome screen: stay single-column until there's real conversation, so
	// the empty home screen isn't split by an (empty) sidebar.
	if m.transcriptEmpty() {
		return false
	}
	// Auto-hide when the panel has nothing to show (no sub-agents and no active
	// plan): a fixed-width column of mostly empty space is wasted, so reclaim it
	// for the full-width chat. The panel returns the moment an agent spawns or a
	// plan starts. (Ctrl+B still force-hides it when there IS content.)
	if !m.sidebarHasContent() {
		return false
	}
	return true
}

// sidebarHasContent reports whether the context sidebar has anything worth a
// column: at least one agent (a specialist delegation or a swarm member) or a
// non-empty plan. Used to auto-hide the panel — and reclaim its width for the
// chat — during plain idle stretches with neither.
func (m model) sidebarHasContent() bool {
	if len(m.sidebarSpecialists()) > 0 || len(m.swarmSpawnedAgents()) > 0 {
		return true
	}
	if len(m.touchedFiles()) > 0 || m.liveEditingPath() != "" {
		// A live in-flight write counts before its result row exists, so the
		// FILES pulse for the session's first mutation isn't hidden.
		return true
	}
	return !m.plan.isEmpty()
}

// chatColumnWidth is the full conversation width. All frame and geometry
// callers route through this so transcript, composer, and mouse hit-testing
// share one layout.
func (m model) chatColumnWidth() int {
	return chatWidth(m.width)
}

// transcriptGutter is the left indent applied to transcript body rows. Keep this
// at zero so content starts at the chat edge and tool/code blocks can use the
// full available width.
func transcriptGutter(columnWidth int) int {
	return 0
}

// transcriptContentWidth is the wrap width for transcript body rows.
func transcriptContentWidth(columnWidth int) int {
	cw := columnWidth - transcriptGutter(columnWidth)
	if cw < 24 {
		return columnWidth
	}
	return cw
}

// sidebarSpecialists returns the specialist delegations worth surfacing in the
// AGENTS panel, EXCLUDING failed tool-misroutes: when a model calls a swarm (or
// any other) tool name as if it were a specialist, the lookup fails with
// "specialist <name> not found". That was never a real sub-agent, so it would
// otherwise pile up bogus "× swarm_send" rows. Real specialists (e.g. "worker")
// and genuine run failures still show.
func (m model) sidebarSpecialists() []specialistInfo {
	all := m.specialists.all()
	out := all[:0:0]
	for _, a := range all {
		if a.status == specialistError && strings.Contains(strings.ToLower(a.errorMsg), "not found") {
			continue
		}
		// Linger a finished specialist for sidebarAgentLinger (a fading ✓), then
		// drop it — a smooth exit rather than an abrupt pop.
		if a.status != specialistRunning && !a.completedAt.IsZero() &&
			m.now().Sub(a.completedAt) >= sidebarAgentLinger {
			continue
		}
		out = append(out, a)
	}
	return out
}

// sidebarHasAgents reports whether the session has agent activity worth keeping
// alive for the compact run-details surface.
func (m model) sidebarHasAgents() bool {
	return len(m.sidebarSpecialists())+len(m.swarmSpawnedAgents()) > 0
}

// sidebarAgentHeader renders the AGENTS section header with the total count of
// active agents — specialist delegations plus swarm/team members.
func (m model) sidebarAgentHeader(width int) string {
	n := len(m.sidebarSpecialists()) + len(m.swarmSpawnedAgents())
	if n == 0 {
		return sidebarHeader("AGENTS", width)
	}
	return sidebarHeaderWithCount("AGENTS", fmt.Sprintf("%d", n), zeroTheme.muted, width)
}

// swarmSpawnRe extracts a member id from a swarm_spawn tool result, whose text
// is "Spawned <type> as task <id> on team <team>." (internal/swarm/tools.go).
var swarmSpawnRe = regexp.MustCompile(`as task (\S+) on team`)

// swarmAgent is one spawned swarm/team member surfaced in the sidebar: the
// stable id recovered from the spawn result, plus a human display name derived
// from the spawn call's task briefing (falling back to the id).
type swarmAgent struct {
	id         string    // e.g. "subagent-1" — the dedup key and fallback name
	name       string    // the task briefing (argHint of the call row), or id when empty
	state      string    // latest reported state (running/done/failed/…), "" until a report lands
	sessionID  string    // member's durable child session id (from swarm_collect), "" until known
	finishing  bool      // done/failed but still lingering before removal (smooth exit)
	finishedAt time.Time // when first seen finished (zero until the spinner tick stamps it)
}

// swarmSpawnedAgents derives the swarm/team members from the transcript's
// swarm_spawn rows — the swarm roster lives in the CLI runtime, not the TUI
// model, so members are recovered from the tool stream. Each member pairs a
// swarm_spawn CALL row (whose detail = the task briefing, via argHint) with the
// next swarm_spawn RESULT row (whose text yields the id, via swarmSpawnRe), so
// the sidebar can name the agent by what it was asked to do rather than an
// opaque "subagent-N". Only spawns that produced a result row (success) are
// returned, and the list is deduped by id.
func (m model) swarmSpawnedAgents() []swarmAgent {
	seen := map[string]bool{}
	var agents []swarmAgent
	// pendingTask holds the task from the most recent unmatched swarm_spawn call
	// row, to be paired with the next swarm_spawn result row.
	pendingTask := ""
	havePending := false
	for _, row := range m.transcript {
		// Scope to the current run's spawns so finished members from an earlier
		// turn don't reappear when a later run keeps members visible (below).
		if row.tool != "swarm_spawn" || row.runID != m.activeRunID {
			continue
		}
		switch row.kind {
		case rowToolCall:
			pendingTask = strings.TrimSpace(row.detail)
			havePending = true
		case rowToolResult:
			match := swarmSpawnRe.FindStringSubmatch(row.detail)
			if match == nil {
				continue
			}
			id := match[1]
			task := ""
			if havePending {
				task = pendingTask
			}
			pendingTask = ""
			havePending = false
			if seen[id] {
				continue
			}
			seen[id] = true
			name := shortTaskName(task)
			if name == "" {
				name = id
			}
			agents = append(agents, swarmAgent{id: id, name: name, sessionID: m.swarmSessionMap[id]})
		}
	}
	// Finished members stay in the panel (✓, still clickable) while the run is in
	// flight, so the user can drill into what each subagent did even after it
	// completes mid-turn. Only once the turn ends do they LINGER briefly with a
	// fading ✓ then drop — a smooth exit, not an abrupt pop. Members not yet in a
	// status report (just spawned) stay live. The done-time is stamped by the
	// spinner tick (stampSwarmDone) for the post-turn fade.
	if status := m.swarmMemberStatus(); len(status) > 0 {
		live := agents[:0:0]
		for _, a := range agents {
			a.state = status[a.id]
			switch a.state {
			case "done", "failed", "completed", "cancelled":
				a.finishing = true
				// A member drops once ITS OWN task completes — not when the whole turn
				// ends: fade out over the linger window from when it was first seen
				// finished (stamped each tick by stampSwarmDone), then remove. This
				// holds whether or not the overall run is still in flight, mirroring
				// how finished specialists drop (sidebarSpecialists).
				doneAt, stamped := m.swarmDoneAt[a.id]
				if stamped && m.now().Sub(doneAt) >= sidebarAgentLinger {
					continue // past the linger window — remove
				}
				a.finishedAt = doneAt
				live = append(live, a)
			default:
				live = append(live, a)
			}
		}
		agents = live
	}
	return agents
}

// swarmStatusRe matches one member line of a swarm_status result, e.g.
// "– teammate-1 [done] (cyan) <task>" → captures the id and the status word.
var swarmStatusRe = regexp.MustCompile(`(?m)^\s*[-–—]?\s*(\S+)\s+\[([a-zA-Z]+)\]`)

// swarmMemberStatus parses the LATEST swarm roster report in the transcript into
// id → status (lowercased). Both swarm_status and swarm_collect results list
// every member with its "[state]", so either one refreshes the live roster; the
// last report in transcript order wins. Empty when no report has run yet. This
// is what lets a swarm_collect that runs while members are still working keep the
// AGENTS panel populated instead of clearing it.
//
// Scoped to the active run, exactly like the spawn rows in swarmSpawnedAgents: a
// prior run's status/collect (whose task ids can repeat) must not mark a current
// member done/failed and drop or fade it.
func (m model) swarmMemberStatus() map[string]string {
	status := map[string]string{}
	for _, row := range m.transcript {
		if row.kind != rowToolResult || row.runID != m.activeRunID {
			continue
		}
		if row.tool != "swarm_status" && row.tool != "swarm_collect" {
			continue
		}
		latest := map[string]string{}
		for _, mt := range swarmStatusRe.FindAllStringSubmatch(row.detail, -1) {
			latest[mt[1]] = strings.ToLower(mt[2])
		}
		if len(latest) > 0 {
			status = latest
		}
	}
	return status
}

// sidebarAgentLinger is how long a finished agent (specialist or swarm member)
// stays in the AGENTS panel with a fading ✓ before it's removed, so the exit
// reads as "done" rather than an abrupt pop.
const sidebarAgentLinger = 1500 * time.Millisecond

// stampSwarmDone records the first time each swarm member is seen finished, so
// swarmSpawnedAgents can linger it for sidebarAgentLinger before dropping it. It
// only adds entries (never clears) and is called from the spinner tick while the
// sidebar holds agents. Mutates the (always-initialised) swarmDoneAt map.
func (m *model) stampSwarmDone() {
	for id, s := range m.swarmMemberStatus() {
		switch s {
		case "done", "failed", "completed", "cancelled":
			if _, seen := m.swarmDoneAt[id]; !seen {
				m.swarmDoneAt[id] = m.now()
			}
		}
	}
}

// sidebarAgentHit marks a rendered agent line (by its index within the agent
// lines block) that is clickable, carrying the member session to drill into.
type sidebarAgentHit struct {
	lineOffset int
	sessionID  string
	title      string
}

// sidebarAgentLines renders one line per active agent. Specialist delegations
// show a live status glyph (• running, ✓ done, ✗ error) plus a "↳ <tool>" working
// line; swarm/team members (from swarm_spawn) show a ready dot and their id.
// Returns nil when there are none (the caller shows a placeholder).
func (m model) sidebarAgentLines(width int) []string {
	lines, _ := m.sidebarAgentRows(width)
	return lines
}

// sidebarAgentRows renders the agent lines and, alongside, records which lines
// are clickable swarm members (those whose member session is known), so a click
// in the sidebar can drill into the member's subchat. lineOffset indexes the
// returned lines slice.
func (m model) sidebarAgentRows(width int) ([]string, []sidebarAgentHit) {
	specialists := m.sidebarSpecialists()
	swarm := m.swarmSpawnedAgents()
	if len(specialists) == 0 && len(swarm) == 0 {
		return nil, nil
	}
	room := maxInt(4, width-3)
	var lines []string
	var hits []sidebarAgentHit
	for _, a := range specialists {
		var icon string
		switch a.status {
		case specialistRunning:
			// A working specialist spins (same glyph its transcript card uses) so
			// the sidebar reads "this one is busy" at a glance; the tick is kept
			// alive by sidebarHasAgents. Static "•" stays for idle/parked members.
			icon = zeroTheme.accent.Render(m.spinnerGlyph())
		case specialistError:
			icon = zeroTheme.red.Render("✗")
		default: // completed
			icon = zeroTheme.green.Render("✓")
		}
		name := strings.TrimSpace(a.name)
		if name == "" {
			name = "agent"
		}
		nameStyle := zeroTheme.ink
		// As a finished specialist nears the end of its linger, dim the whole row
		// toward faint so its removal reads as a fade-out rather than a pop.
		if a.status != specialistRunning && m.agentExitFading(a.completedAt) {
			glyph := "✓"
			if a.status == specialistError {
				glyph = "✗"
			}
			icon = zeroTheme.faint.Render(glyph)
			nameStyle = zeroTheme.faint
		}
		lines = append(lines, " "+icon+" "+nameStyle.Render(truncateStep(name, room)))
		if a.status != specialistRunning {
			continue
		}
		// Live working detail for a running subagent: current tool + arg hint,
		// falling back to the running tool count.
		detail := strings.TrimSpace(a.currentTool)
		if d := strings.TrimSpace(a.currentDetail); d != "" {
			if detail != "" {
				detail += " " + d
			} else {
				detail = d
			}
		}
		if detail == "" && a.toolCount > 0 {
			detail = fmt.Sprintf("%d tools", a.toolCount)
		}
		if detail != "" {
			lines = append(lines, "   "+zeroTheme.faint.Render("↳ "+truncateStep(detail, maxInt(2, room-2))))
		}
	}
	// Swarm/team members: a live member's whole task-name carries a mild, slow cool
	// pulse (NOT a per-letter ripple). A finished member instead shows a green ✓
	// that dims toward faint over its linger — a smooth exit before removal.
	style := m.swarmNameStyle()
	for _, a := range swarm {
		// A member with a known session is clickable: record the hit at the index
		// this line will occupy before appending it.
		if a.sessionID != "" {
			hits = append(hits, sidebarAgentHit{lineOffset: len(lines), sessionID: a.sessionID, title: a.name})
		}
		if a.finishing {
			icon := zeroTheme.green.Render("✓")
			nameStyle := zeroTheme.muted
			if m.agentExitFading(a.finishedAt) {
				icon = zeroTheme.faint.Render("✓")
				nameStyle = zeroTheme.faint
			}
			lines = append(lines, " "+icon+" "+nameStyle.Render(truncateStep(a.name, room)))
			continue
		}
		// A non-running state (pending/handoff/…) is shown faintly after the task
		// so the panel reports the member's actual status; running is left implied
		// by the live pulse to keep the common case clean.
		nameRoom := room
		suffix := ""
		if st := strings.TrimSpace(a.state); st != "" && st != "running" {
			suffix = " " + zeroTheme.faint.Render(st)
			nameRoom = maxInt(4, room-len(st)-1)
		}
		lines = append(lines, " "+zeroTheme.accent.Render("•")+" "+style.Render(truncateStep(a.name, nameRoom))+suffix)
	}
	return lines, hits
}

// sidebarAgentSelectables returns the clickable swarm-member lines with their
// ABSOLUTE index inside the rendered sidebar (the AGENTS header occupies index 0,
// so agent rows start at index 1). Recomputed on demand by the mouse hit-test —
// View cannot persist a registry on the value-receiver model — mirroring
// transcriptLineAtMouse.
func (m model) sidebarAgentSelectables(width int) []sidebarAgentHit {
	_, hits := m.sidebarAgentRows(width)
	for i := range hits {
		hits[i].lineOffset++ // shift past the AGENTS header at sidebar index 0
	}
	return hits
}

// agentExitFading reports whether a finished agent is in the later half of its
// linger window (sidebarAgentLinger), so its row dims toward faint just before
// it's removed. A zero finishedAt (not yet stamped) is not fading.
func (m model) agentExitFading(finishedAt time.Time) bool {
	return !finishedAt.IsZero() && m.now().Sub(finishedAt) >= sidebarAgentLinger/2
}

// swarmNameStyle returns a gentle, whole-line cool tint for a live swarm member
// name: mostly a calm blue, easing through a dimmer shade on a slow cycle — a
// mild breathe, not a flashing per-letter ripple. Static blue under reduced
// motion (or when the animation clock isn't advancing).
func (m model) swarmNameStyle() lipgloss.Style {
	if m.reducedMotion {
		return zeroTheme.blue
	}
	styles := swarmPulseStyles()
	if len(styles) == 0 {
		return zeroTheme.blue
	}
	// Slow ping-pong through the subtle ramp for a smooth, slight breathe: the
	// index eases up then back down so the colour never jumps (no flicker).
	n := len(styles)
	period := 2 * (n - 1)
	if period <= 0 {
		return styles[0]
	}
	p := (m.spinnerPhase / 5) % period
	idx := p
	if idx >= n {
		idx = period - p
	}
	return styles[idx]
}

// swarmPulseStyles is a short, SUBTLE ramp from the cool blue toward a slightly
// dimmer shade — only the bright portion of a blue→muted blend, so the dim end
// stays bluish (a slight shift, not a blue→grey swing). Smooth gradient = no
// flicker. Returns nil when the theme has no parseable colours (static fallback).
func swarmPulseStyles() []lipgloss.Style {
	fg := zeroTheme.blue.GetForeground()
	dim := zeroTheme.muted.GetForeground()
	if fg == nil || dim == nil {
		return nil
	}
	blend := lipgloss.Blend1D(12, fg, dim)
	if len(blend) > 5 {
		blend = blend[:5] // brightest portion: blue → slightly dimmed blue
	}
	out := make([]lipgloss.Style, len(blend))
	for i, c := range blend {
		r, g, b, a := c.RGBA()
		out[i] = lipgloss.NewStyle().Foreground(color.RGBA{R: uint8(r >> 8), G: uint8(g >> 8), B: uint8(b >> 8), A: uint8(a >> 8)})
	}
	return out
}

// shortTaskName condenses a task briefing into a 1-2 word agent name: the first
// significant word (usually the verb) plus the next non-filler word, so a member
// reads as e.g. "Explore repository" instead of the full one-line briefing.
func shortTaskName(task string) string {
	task = strings.TrimSpace(task)
	if task == "" {
		return ""
	}
	picked := make([]string, 0, 2)
	for _, w := range strings.Fields(task) {
		clean := strings.Trim(w, ".,:;!?\"'`()[]{}")
		if clean == "" {
			continue
		}
		if len(picked) > 0 && nameFillerWords[strings.ToLower(clean)] {
			continue
		}
		picked = append(picked, clean)
		if len(picked) == 2 {
			break
		}
	}
	if len(picked) == 0 {
		return task
	}
	return strings.Join(picked, " ")
}

// nameFillerWords are skipped (after the first word) when condensing a task into
// a short agent name, so "Review the current branch" → "Review current".
var nameFillerWords = map[string]bool{
	"the": true, "a": true, "an": true, "to": true, "of": true, "for": true,
	"and": true, "or": true, "on": true, "in": true, "with": true, "any": true,
	"all": true, "its": true, "this": true, "that": true, "into": true, "from": true,
}

// renderContextSidebar builds the sidebar block: exactly height lines, each
// exactly width cells (after fitStyledLine + padding). Sections render top to
// bottom — FILES, PLAN — with the token readout pinned to the bottom line. Each
// section header is a faint uppercase label; items use ink/muted. Empty
// sections render a quiet placeholder rather than vanishing so the layout stays
// stable.
func (m model) renderContextSidebar(width, height int) []string {
	if width <= 0 || height <= 0 {
		return nil
	}

	var lines []string
	add := func(s string) { lines = append(lines, s) }

	// AGENTS section — spawned subagents and their live working detail.
	add(m.sidebarAgentHeader(width))
	agentLines := m.sidebarAgentLines(width)
	if len(agentLines) == 0 {
		add(sidebarPlaceholder("no agents spawned", width))
	} else {
		lines = append(lines, agentLines...)
	}

	// PLAN section.
	add("")
	add(m.sidebarPlanHeader(width))
	planLines := m.sidebarPlanLines(width)
	if len(planLines) == 0 {
		add(sidebarPlaceholder("no active plan", width))
	} else {
		lines = append(lines, planLines...)
	}

	// FILES section: the files this session has touched (files_panel.go).
	// Rendered BELOW the plan steps so it never shifts sidebarPlanSelectables'
	// click offsets; its own hits (sidebarFileSelectables) account for the
	// sections above it.
	add("")
	add(m.sidebarFilesHeader(width))
	fileLines, _ := m.sidebarFileLines(width)
	if len(fileLines) == 0 {
		add(sidebarPlaceholder("no files touched", width))
	} else {
		lines = append(lines, fileLines...)
	}

	// ACTIVITY section: recent completed work + a live "generating…" pulse. Shown
	// BELOW the plan steps so it never shifts sidebarPlanSelectables' click offsets,
	// and budgeted (height-1 minus what's used) so it clips ITSELF from the bottom
	// rather than letting the end-truncation eat into the plan. Absent when empty.
	if activityLines := m.sidebarActivityLines(width, maxInt(0, height-1-len(lines))); len(activityLines) > 0 {
		add("")
		add(sidebarHeader("ACTIVITY", width))
		lines = append(lines, activityLines...)
	}

	// Token readout pinned to the bottom.
	tokenLine := m.sidebarTokenLine(width)
	// Reserve the bottom row for tokens; pad the gap so it sits at the floor.
	for len(lines) < height-1 {
		add("")
	}
	if len(lines) > height-1 {
		lines = lines[:height-1]
	}
	add(tokenLine)

	// Hover highlight: resolved by STABLE IDENTITY (sessionID / stepIndex), not a
	// cached line offset — see hoveredSidebarLineOffset. A row whose identity no
	// longer resolves (it disappeared since the hover was last set from a real
	// mouse motion) simply doesn't highlight, rather than a coincidentally-matching
	// unrelated row lighting up.
	if lineOffset, ok := m.hoveredSidebarLineOffset(width); ok && lineOffset >= 0 && lineOffset < len(lines) {
		lines[lineOffset] = zeroTheme.hover.Render(ansi.Strip(lines[lineOffset]))
	}

	// Normalize every row to exactly width cells.
	for i := range lines {
		lines[i] = padStyledLine(lines[i], width)
	}
	for len(lines) < height {
		lines = append(lines, strings.Repeat(" ", width))
	}
	if len(lines) > height {
		lines = lines[:height]
	}
	return lines
}

// hoveredSidebarLineOffset resolves the hovered sidebar row's CURRENT line offset
// fresh on every call, by re-matching m.hover's stable identity (sessionID for an
// agent, stepIndex for a plan step) against a freshly computed hit list — never a
// cached index. Returns false when the hover isn't sidebar-scoped, or when the
// identity no longer resolves (that row disappeared since the hover was last set
// by a real mouse motion — a linger window elapsing, a plan step completing —
// with no intervening motion to re-target it).
func (m model) hoveredSidebarLineOffset(width int) (int, bool) {
	switch m.hover.kind {
	case hoverSidebarAgent:
		for _, hit := range m.sidebarAgentSelectables(width) {
			if hit.sessionID == m.hover.sessionID {
				return hit.lineOffset, true
			}
		}
	case hoverPlanStep:
		for _, hit := range m.sidebarPlanSelectables(width) {
			if hit.stepIndex == m.hover.stepIndex {
				return hit.lineOffset, true
			}
		}
	case hoverFileRow:
		for _, hit := range m.sidebarFileSelectables(width) {
			if hit.path == m.hover.filePath {
				return hit.lineOffset, true
			}
		}
	}
	return 0, false
}

// sidebarHeader renders a bold-muted uppercase section label. Bold muted (vs the
// faint body items and placeholders) gives the header enough weight to read as a
// section heading rather than more filler. The width arg is unused — kept so it
// shares a signature with sidebarHeaderWithCount.
func sidebarHeader(label string, _ int) string {
	return zeroTheme.muted.Bold(true).Render(strings.ToUpper(label))
}

// sidebarHeaderWithCount renders a bold-muted section label with a right-aligned
// count (e.g. "PLAN   2/5") rendered in countStyle, so a section can colour its
// count by state — accent while in-flight, green when complete.
func sidebarHeaderWithCount(label, count string, countStyle lipgloss.Style, width int) string {
	left := zeroTheme.muted.Bold(true).Render(strings.ToUpper(label))
	right := countStyle.Render(count)
	gap := width - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		return left
	}
	return left + strings.Repeat(" ", gap) + right
}

// sidebarPlaceholder renders a quiet placeholder line for an empty section.
func sidebarPlaceholder(text string, width int) string {
	return " " + zeroTheme.faint.Render(truncateRunes(text, maxInt(1, width-1)))
}

// sidebarPlanHeader renders the PLAN section header with the done/total count.
func (m model) sidebarPlanHeader(width int) string {
	state := m.plan
	if state.isEmpty() {
		return sidebarHeader("PLAN", width)
	}
	total := len(state.steps)
	done := 0
	for _, step := range state.steps {
		if step.status == "completed" || step.status == "failed" {
			done++
		}
	}
	// Stateful count: green once every step is done, accent while in-flight.
	countStyle := zeroTheme.accent
	if done == total {
		countStyle = zeroTheme.green
	}
	return sidebarHeaderWithCount("PLAN", fmt.Sprintf("%d/%d", done, total), countStyle, width)
}

// sidebarPlanLines renders the plan step list for the sidebar using the same
// status glyphs as the pinned panel (✓ done, • in-progress, ○ pending, ✗
// failed), reading m.plan directly so it stays in sync. Returns nil for an
// empty plan (the caller then shows a placeholder).
func (m model) sidebarPlanLines(width int) []string {
	state := m.plan
	if state.isEmpty() {
		return nil
	}
	room := maxInt(4, width-3)
	lines := make([]string, 0, len(state.steps))
	for _, step := range state.steps {
		var icon, body string
		switch step.status {
		case "completed":
			icon = zeroTheme.green.Render("✓")
			body = zeroTheme.muted.Render(truncateStep(step.content, room))
		case "in_progress":
			icon = zeroTheme.accent.Render("•")
			body = zeroTheme.ink.Render(truncateStep(step.content, room))
		case "failed":
			icon = zeroTheme.red.Render("✗")
			body = zeroTheme.muted.Render(truncateStep(step.content, room))
		default: // pending
			icon = zeroTheme.faint.Render("○")
			body = zeroTheme.faint.Render(truncateStep(step.content, room))
		}
		lines = append(lines, " "+icon+" "+body)
	}
	return lines
}

// maxSidebarActivityLines caps the ACTIVITY feed so it stays a glanceable tail,
// not a scrolling log.
const maxSidebarActivityLines = 5

// maxSidebarActivityScan bounds how many trailing transcript rows the activity
// feed inspects per render, so a sparse-work transcript stays O(window).
const maxSidebarActivityScan = 200

// sidebarActivityLines builds the ACTIVITY feed: a live, phase-aware pulse atop
// the most recent completed work (files written, commands run). The quiet-run
// warning still replaces the phase label when needed. It scans the transcript
// BACKWARD and stops after the cap, so the cost is bounded by the cap — never a
// full-transcript walk per frame. budget is the rows available before the token
// floor; the feed clips itself to it.
func (m model) sidebarActivityLines(width, budget int) []string {
	if budget <= 0 {
		return nil
	}
	room := maxInt(4, width-3)
	limit := minInt(maxSidebarActivityLines, budget)
	var work []string
	// Bound the rows inspected per render so a long, work-sparse transcript can't
	// turn this hot path into a full O(transcript) walk; recent work sits near the
	// end, so a window comfortably finds the latest items.
	scanned := 0
	for i := len(m.transcript) - 1; i >= 0 && len(work) < limit && scanned < maxSidebarActivityScan; i-- {
		scanned++
		row := m.transcript[i]
		if row.kind != rowToolResult || !isPlanWorkTool(row.tool) {
			continue
		}
		glyph := zeroTheme.green.Render("✓")
		if row.status == tools.StatusError {
			glyph = zeroTheme.red.Render("✗")
		}
		work = append(work, " "+glyph+" "+zeroTheme.muted.Render(truncateStep(m.activitySummary(row), room)))
	}
	live := ""
	if m.activeRunID != 0 {
		label := m.workingActivity()
		if hint := m.quietGenerationHint(); hint != "" {
			label = hint
		}
		live = " " + zeroTheme.accent.Render("›") + " " + zeroTheme.faint.Render(truncateStep(label, room))
	}
	lines := make([]string, 0, len(work)+1)
	if live != "" {
		lines = append(lines, live)
	}
	lines = append(lines, work...)
	if len(lines) > budget {
		lines = lines[:budget]
	}
	return lines
}

// activitySummary renders one ACTIVITY line: a command's command-line (recovered
// from its call row) for bash/exec, else the tool result's first line with the
// "tool result: <tool> <status> " prefix stripped (e.g. "Created styles.css
// (1045 lines).").
func (m model) activitySummary(row transcriptRow) string {
	if isPlanCommandTool(row.tool) {
		if cmd := m.activityCommandForRow(row.id, row.runID); cmd != "" {
			return row.tool + " · " + cmd
		}
		return row.tool
	}
	text := strings.TrimSpace(strings.SplitN(row.text, "\n", 2)[0])
	status := row.status
	if status == "" {
		status = tools.StatusOK
	}
	text = strings.TrimPrefix(text, fmt.Sprintf("tool result: %s %s ", row.tool, status))
	if strings.TrimSpace(text) == "" {
		return row.tool
	}
	return text
}

// activityCommandForRow recovers a command tool's command-line from its paired
// call row (whose arg hint carries the command), matched by BOTH id and runID so
// a reused tool-call id from a later run can't attribute the wrong command.
func (m model) activityCommandForRow(id string, runID int) string {
	if id == "" {
		return ""
	}
	for i := len(m.transcript) - 1; i >= 0; i-- {
		row := m.transcript[i]
		if row.kind == rowToolCall && row.id == id && row.runID == runID {
			return row.arg
		}
	}
	return ""
}

// sidebarTokenLine renders the bottom token/context readout. It prefers the
// live context-fill figure (last request's input tokens) and falls back to the
// session's cumulative token count.
func (m model) sidebarTokenLine(width int) string {
	label := m.sidebarTokenText()
	if label == "" {
		label = "0 tokens"
	}
	// Append the graded context-fill % — the at-a-glance "how full is the window"
	// the compaction trigger reasons about. Reserve its width so the token label
	// truncates around it rather than overflowing.
	chip := ""
	if pct, _, _, style, ok := m.contextFillPercent(); ok {
		chip = zeroTheme.faint.Render(" · ") + style.Render(fmt.Sprintf("%d%%", pct))
	}
	budget := maxInt(1, width-1-lipgloss.Width(chip))
	return " " + zeroTheme.faint.Render(truncateRunes(label, budget)) + chip
}

// sidebarTokenText computes the token figure shown at the sidebar floor from
// the latest provider step's token footprint.
func (m model) sidebarTokenText() string {
	if m.usageTracker == nil {
		return ""
	}
	summary := m.usageTracker.Summary()
	used := m.latestUsageTokens(summary)
	if used <= 0 {
		return ""
	}
	if window := m.modelContextWindow(m.modelName); window > 0 {
		return fmt.Sprintf("%s / %s tokens", humanCount(used), humanCount(window))
	}
	return humanCount(used) + " tokens"
}

// joinColumns splices a chat block and a sidebar block side-by-side, one
// divider cell between them, into total-width rows. Both blocks are normalized
// to their column widths and to the same row count first, so every joined row
// is exactly chatWidth + 1 + sidebarWidth cells and the columns stay aligned.
func joinColumns(chat []string, sidebar []string, chatW, sidebarW int) []string {
	rows := len(chat)
	if len(sidebar) > rows {
		rows = len(sidebar)
	}
	// A cell of air on each side of the rule (" │ ") so the columns don't butt
	// flush against it. The chat side gets its gutter from the leading space; the
	// sidebar side from the trailing space (plus items' own leading inset, which
	// nests them under the flush section headers). Budgeted by chatColumnWidth(-3).
	divider := " " + zeroTheme.line.Render("│") + " "
	out := make([]string, rows)
	for i := 0; i < rows; i++ {
		left := ""
		if i < len(chat) {
			left = chat[i]
		}
		right := ""
		if i < len(sidebar) {
			right = sidebar[i]
		}
		left = padStyledLine(left, chatW)
		right = padStyledLine(right, sidebarW)
		out[i] = left + divider + right
	}
	return out
}
