package tui

import (
	"strings"

	"github.com/Twigpine/zero/internal/redaction"
)

type commandStatus string

const (
	commandStatusOK      commandStatus = "ok"
	commandStatusWarning commandStatus = "warning"
	commandStatusBlocked commandStatus = "blocked"
	commandStatusInfo    commandStatus = "info"
)

type commandOutput struct {
	Title    string
	Status   commandStatus
	Sections []commandSection
	Hints    []string
}

type commandSection struct {
	Title  string
	Fields []commandField
	Lines  []string
	Rows   []commandRow
	Hints  []string
}

type commandField struct {
	Key   string
	Value string
}

type commandRow struct {
	Text string
}

type commandCard struct {
	Title    string
	Summary  []string
	Sections []commandCardSection
	Actions  []string
}

type commandCardSection struct {
	Title  string
	Fields []commandField
	Lines  []string
	Rows   []commandRow
}

const commandCardTranscriptPrefix = "\x00command-card\x00"

func renderCommandCardTranscript(card commandCard) string {
	return commandCardTranscriptPrefix + renderCommandCard(card)
}

func commandCardTranscriptPayload(text string) (string, bool) {
	if !strings.HasPrefix(text, commandCardTranscriptPrefix) {
		return text, false
	}
	return strings.TrimPrefix(text, commandCardTranscriptPrefix), true
}

// planCardTranscriptPrefix tags the plan-step detail card so it renders through
// renderPlanCardRow — a minimal variant (dim grey border, calm status-tinted
// title, no loud lime headers) — instead of the standard command-card styling.
const planCardTranscriptPrefix = "\x00plan-card\x00"

// renderPlanCard formats a plan-step detail card with the same structured text
// as a command card, but tagged so it renders minimally.
func renderPlanCard(output commandOutput) string {
	return planCardTranscriptPrefix + formatCommandOutput(output)
}

func planCardTranscriptPayload(text string) (string, bool) {
	if !strings.HasPrefix(text, planCardTranscriptPrefix) {
		return text, false
	}
	return strings.TrimPrefix(text, planCardTranscriptPrefix), true
}

func renderCommandCard(card commandCard) string {
	lines := []string{}

	title := compactCommandOutputText(card.Title)
	if title == "" {
		title = "Zero"
	}
	lines = append(lines, title)

	if summary := compactCommandCardList(card.Summary); len(summary) > 0 {
		lines = append(lines, strings.Join(summary, " | "))
	}

	for _, section := range card.Sections {
		lines = append(lines, formatCommandCardSection(section)...)
	}

	if actions := compactCommandCardList(card.Actions); len(actions) > 0 {
		lines = append(lines, "actions: "+strings.Join(actions, " | "))
	}

	return strings.Join(lines, "\n")
}

func formatCommandCardSection(section commandCardSection) []string {
	lines := []string{}

	if title := compactCommandOutputText(section.Title); title != "" {
		lines = append(lines, title)
	}

	keyWidth := 0
	fields := make([]commandField, 0, len(section.Fields))
	for _, field := range section.Fields {
		key := compactCommandOutputText(field.Key)
		value := compactCommandOutputText(field.Value)
		if key == "" || value == "" {
			continue
		}
		if len(key) > keyWidth {
			keyWidth = len(key)
		}
		fields = append(fields, commandField{Key: key, Value: value})
	}
	for _, field := range fields {
		lines = append(lines, "  "+field.Key+strings.Repeat(" ", keyWidth-len(field.Key)+2)+field.Value)
	}

	for _, line := range section.Lines {
		if text := compactCommandOutputText(line); text != "" {
			lines = append(lines, "  "+text)
		}
	}
	for _, row := range section.Rows {
		if text := compactCommandOutputText(row.Text); text != "" {
			lines = append(lines, "  - "+text)
		}
	}

	return lines
}

func compactCommandCardList(values []string) []string {
	compacted := make([]string, 0, len(values))
	for _, value := range values {
		if text := compactCommandOutputText(value); text != "" {
			compacted = append(compacted, text)
		}
	}
	return compacted
}

func formatCommandOutput(output commandOutput) string {
	lines := []string{}

	status := normalizeCommandStatus(output.Status)
	title := compactCommandOutputText(output.Title)
	if title == "" {
		lines = append(lines, "Zero")
	} else {
		lines = append(lines, title)
	}
	lines = append(lines, "status: "+string(status))

	for _, section := range output.Sections {
		lines = append(lines, formatCommandSection(section)...)
	}
	for _, hint := range output.Hints {
		if text := compactCommandOutputText(hint); text != "" {
			lines = append(lines, "hint: "+text)
		}
	}

	return strings.Join(lines, "\n")
}

func formatCommandSection(section commandSection) []string {
	lines := []string{}

	if title := compactCommandOutputText(section.Title); title != "" {
		lines = append(lines, title)
	}
	for _, field := range section.Fields {
		key := compactCommandOutputText(field.Key)
		value := compactCommandOutputText(field.Value)
		if key == "" || value == "" {
			continue
		}
		lines = append(lines, "  "+key+": "+value)
	}
	for _, line := range section.Lines {
		if text := compactCommandOutputText(line); text != "" {
			lines = append(lines, "  "+text)
		}
	}
	for _, row := range section.Rows {
		if text := compactCommandOutputText(row.Text); text != "" {
			lines = append(lines, "  - "+text)
		}
	}
	for _, hint := range section.Hints {
		if text := compactCommandOutputText(hint); text != "" {
			lines = append(lines, "  hint: "+text)
		}
	}

	return lines
}

func normalizeCommandStatus(status commandStatus) commandStatus {
	switch status {
	case commandStatusOK, commandStatusWarning, commandStatusBlocked, commandStatusInfo:
		return status
	default:
		return commandStatusInfo
	}
}

func compactCommandOutputText(text string) string {
	text = strings.Join(strings.Fields(text), " ")
	return redaction.RedactString(text, redaction.Options{})
}

func renderCommandOutput(output commandOutput) string {
	// Route every command-info screen (/tools, /permissions, /context, /config,
	// /sessions, doctor, …) through the styled command-card renderer so it gets
	// real visual hierarchy (accent group headers, two-tone command rows) instead
	// of the flat dim-grey system note. The structured text is unchanged — the
	// prefix just selects the card renderer in renderRowModeUncached.
	return commandCardTranscriptPrefix + formatCommandOutput(output)
}

func commandBullet(value string) string {
	return "- " + compactCommandOutputText(value)
}

func boolText(value bool) string {
	if value {
		return "yes"
	}
	return "no"
}
