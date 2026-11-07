package tui

// The MODELS section: the live mix of models the session's sub-agents are
// running on. The AGENTS rows say what each agent does; nothing said what the
// fleet as a whole is running ON — with per-role auto-assignment the answer
// stopped being "the session's model", and the only way to see the routing work
// was to expand agents one by one.
//
// A separate template over separate data, deliberately: every renderer here is
// a pure function of ([]modelMixEntry, width), so the section is testable
// without a model and reusable by any surface that has the entries.
//
// ABSENT UNTIL IT SAYS SOMETHING. The section renders only when at least one
// agent runs on a known model — a plain session (posture off, no assignments)
// never grows a new section, and the sidebar's layout stays exactly what it
// was. Same contract as ACTIVITY.

import (
	"hash/fnv"
	"sort"
	"strconv"
	"strings"

	"charm.land/lipgloss/v2"
)

// modelMixEntry is one model's slice of the fleet.
type modelMixEntry struct {
	model   string
	total   int
	working int
	// inherited marks the "no model recorded" bucket: agents on whatever the
	// session runs. Rendered last, in muted, under the session's own model name
	// when known.
	inherited bool
}

// modelMixEntries groups the visible agents by the model they run on.
// Agents with no recorded model fold into one inherited bucket labelled with
// the session's model (or "session model" when even that is unknown).
// Sorted by size descending, then name, with the inherited bucket pinned last —
// the routed models are the news; the default is the backdrop.
func modelMixEntries(agents []specialistInfo, sessionModel string) []modelMixEntry {
	if len(agents) == 0 {
		return nil
	}
	byModel := map[string]*modelMixEntry{}
	order := []string{}
	inherited := modelMixEntry{inherited: true}
	for _, agent := range agents {
		name := strings.TrimSpace(agent.model)
		if name == "" {
			inherited.total++
			if agent.status == specialistRunning {
				inherited.working++
			}
			continue
		}
		entry, ok := byModel[name]
		if !ok {
			entry = &modelMixEntry{model: name}
			byModel[name] = entry
			order = append(order, name)
		}
		entry.total++
		if agent.status == specialistRunning {
			entry.working++
		}
	}
	out := make([]modelMixEntry, 0, len(order)+1)
	for _, name := range order {
		out = append(out, *byModel[name])
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].total != out[j].total {
			return out[i].total > out[j].total
		}
		return out[i].model < out[j].model
	})
	if inherited.total > 0 {
		inherited.model = strings.TrimSpace(sessionModel)
		if inherited.model == "" {
			inherited.model = "session model"
		}
		out = append(out, inherited)
	}
	return out
}

// modelMixDiversified reports whether the entries say anything the AGENTS
// header does not: at least one agent on a KNOWN model. All-inherited is the
// old world and renders nothing.
func modelMixDiversified(entries []modelMixEntry) bool {
	for _, entry := range entries {
		if !entry.inherited && entry.total > 0 {
			return true
		}
	}
	return false
}

// modelMixPalette is the cycle of styles a model's segments and dot draw in.
// Assignment is by NAME HASH, not position — a model keeps its colour while
// counts shift the sort order around it, so the bar never reshuffles under the
// reader's eyes. The inherited bucket is always muted and never hashes.
func modelMixPalette() []lipgloss.Style {
	return []lipgloss.Style{zeroTheme.accent, zeroTheme.blue, zeroTheme.amber, zeroTheme.green}
}

func modelMixStyle(entry modelMixEntry) lipgloss.Style {
	if entry.inherited {
		return zeroTheme.muted
	}
	palette := modelMixPalette()
	hash := fnv.New32a()
	hash.Write([]byte(entry.model))
	return palette[int(hash.Sum32())%len(palette)]
}

// modelMixBar renders the one-line proportional mix: each model a run of ▰
// cells sized to its share of the fleet, every model guaranteed at least one
// cell so a single agent on a distinct model is never rounded away. Empty when
// nothing fits or nothing is diversified.
func modelMixBar(entries []modelMixEntry, width int) string {
	total := 0
	for _, entry := range entries {
		total += entry.total
	}
	if total == 0 || width < len(entries) {
		return ""
	}
	cells := make([]int, len(entries))
	used := 0
	for i, entry := range entries {
		cells[i] = maxInt(1, entry.total*width/total)
		used += cells[i]
	}
	// Rounding overshoot comes out of the largest slice; undershoot goes into it.
	// One pass each way is enough: len(entries) ≤ width by the guard above.
	for used > width {
		largest := 0
		for i := range cells {
			if cells[i] > cells[largest] {
				largest = i
			}
		}
		if cells[largest] <= 1 {
			break
		}
		cells[largest]--
		used--
	}
	for used < width {
		largest := 0
		for i := range cells {
			if cells[i] > cells[largest] {
				largest = i
			}
		}
		cells[largest]++
		used++
	}
	var bar strings.Builder
	for i, entry := range entries {
		bar.WriteString(modelMixStyle(entry).Render(strings.Repeat("▰", cells[i])))
	}
	return bar.String()
}

// modelMixRows renders one line per model: a coloured dot, the model's name,
// and its count — with "·N live" while any of its agents still works, in the
// model's own colour so a glance pairs the row with its bar segment. The
// inherited bucket draws an open dot: those agents were not routed anywhere.
func modelMixRows(entries []modelMixEntry, width int) []string {
	rows := make([]string, 0, len(entries))
	for _, entry := range entries {
		style := modelMixStyle(entry)
		dot := style.Render("●")
		if entry.inherited {
			dot = zeroTheme.faint.Render("○")
		}
		count := "×" + strconv.Itoa(entry.total)
		if entry.working > 0 {
			count += " ·" + strconv.Itoa(entry.working) + " live"
		}
		countRendered := style.Render(count)
		name := zeroTheme.ink.Render(truncateStep(entry.model,
			maxInt(1, width-2-lipgloss.Width(dot)-lipgloss.Width(countRendered)-1)))
		gap := width - 1 - lipgloss.Width(dot) - 1 - lipgloss.Width(name) - lipgloss.Width(countRendered)
		if gap < 1 {
			rows = append(rows, " "+dot+" "+name)
			continue
		}
		rows = append(rows, " "+dot+" "+name+strings.Repeat(" ", gap)+countRendered)
	}
	return rows
}

// sidebarModelLines assembles the MODELS section for the sidebar: header with
// the distinct-model count, the mix bar, then a row per model. Empty — no
// header, no blank — when the fleet is not diversified, so the section only
// exists while the routing is actually routing.
func (m model) sidebarModelLines(width int) []string {
	entries := modelMixEntries(m.sidebarSpecialists(), m.modelName)
	if !modelMixDiversified(entries) {
		return nil
	}
	distinct := 0
	anyWorking := false
	for _, entry := range entries {
		if !entry.inherited {
			distinct++
		}
		if entry.working > 0 {
			anyWorking = true
		}
	}
	countStyle := zeroTheme.muted
	if anyWorking {
		countStyle = zeroTheme.accent
	}
	lines := []string{m.postureHeaderWithCount("MODELS", strconv.Itoa(distinct), countStyle, width)}
	if bar := modelMixBar(entries, maxInt(0, width-2)); bar != "" {
		lines = append(lines, " "+bar)
	}
	lines = append(lines, modelMixRows(entries, width)...)
	return lines
}
