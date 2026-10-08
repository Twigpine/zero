package tui

import (
	"regexp"
	"strings"
)

// planUpdateItem is the persisted, transcript-facing form of one update_plan
// item. It is parsed from the tool result rather than the live plan state so a
// resumed session reflows exactly like the original one.
type planUpdateItem struct {
	status  string
	content string
	notes   string
}

var planUpdateLinePattern = regexp.MustCompile(`^\s*\d+\.\s+\[([^\]]+)\]\s*(.+?)\s*$`)

// parsePlanUpdateItems understands the stable output contract of update_plan:
// a Current Plan header, numbered status lines, and optional indented Notes.
// It returns false for malformed output so the caller can retain the ordinary
// tool-result card instead of dropping an unexpected diagnostic.
func parsePlanUpdateItems(detail string) ([]planUpdateItem, bool) {
	lines := strings.Split(strings.ReplaceAll(detail, "\r\n", "\n"), "\n")
	items := make([]planUpdateItem, 0, len(lines))
	foundHeader := false
	for _, line := range lines {
		if !foundHeader {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" {
				continue
			}
			if trimmed == "Current Plan:" {
				foundHeader = true
				continue
			}
			return nil, false
		}
		if match := planUpdateLinePattern.FindStringSubmatch(line); len(match) == 3 {
			status := strings.ToLower(strings.TrimSpace(match[1]))
			switch status {
			case "pending", "in_progress", "completed", "failed":
			default:
				return nil, false
			}
			items = append(items, planUpdateItem{
				status:  status,
				content: strings.TrimSpace(match[2]),
			})
			continue
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		trimmed := strings.TrimSpace(line)
		if len(items) > 0 &&
			(strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t")) &&
			strings.HasPrefix(trimmed, "Notes:") {
			items[len(items)-1].notes = strings.TrimSpace(strings.TrimPrefix(trimmed, "Notes:"))
			continue
		}
		return nil, false
	}
	return items, foundHeader && len(items) > 0
}

// renderPlanUpdateCard mirrors the transcript-first plan layout: a quiet
// heading followed by a compact, wrapped checklist. Plans therefore remain
// where they happened in the work history instead of occupying a permanent
// footer slot above the composer.
func renderPlanUpdateCard(detail string, width int) (string, bool) {
	items, ok := parsePlanUpdateItems(detail)
	if !ok {
		return "", false
	}
	if width < 8 {
		width = 8
	}

	lines := []string{zeroTheme.faint.Render("• ") + zeroTheme.ink.Bold(true).Render("Updated Plan")}
	for index, item := range items {
		firstPrefix := "    "
		if index == 0 {
			firstPrefix = "  └ "
		}
		lines = append(lines, renderPlanUpdateItem(item, firstPrefix, width)...)
	}
	return strings.Join(lines, "\n"), true
}

func renderPlanUpdateItem(item planUpdateItem, prefix string, width int) []string {
	marker, style := planUpdateItemStyle(item.status)
	contentWidth := maxInt(1, width-len([]rune(prefix))-len([]rune(marker)))
	wrapped := wrapPlainText(item.content, contentWidth)
	if len(wrapped) == 0 {
		wrapped = []string{"(unnamed step)"}
	}

	lines := make([]string, 0, len(wrapped)+1)
	lines = append(lines, prefix+style.Render(marker+wrapped[0]))
	continuation := strings.Repeat(" ", len([]rune(prefix))+len([]rune(marker)))
	for _, line := range wrapped[1:] {
		lines = append(lines, continuation+style.Render(line))
	}
	if note := strings.TrimSpace(item.notes); note != "" {
		notePrefix := strings.Repeat(" ", len([]rune(prefix))) + "  "
		noteWidth := maxInt(1, width-len([]rune(notePrefix)))
		for _, line := range wrapPlainText(note, noteWidth) {
			lines = append(lines, notePrefix+zeroTheme.faint.Italic(true).Render(line))
		}
	}
	return lines
}

func planUpdateItemStyle(status string) (string, interface{ Render(...string) string }) {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "completed":
		return "✔ ", zeroTheme.faint.Strikethrough(true)
	case "in_progress":
		return "□ ", zeroTheme.accent.Bold(true)
	case "failed":
		return "✗ ", zeroTheme.red
	default:
		return "□ ", zeroTheme.faint
	}
}
