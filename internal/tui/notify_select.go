package tui

import (
	"strings"

	"github.com/Gitlawb/zero/internal/config"
	"github.com/Gitlawb/zero/internal/notify"
)

// notifyChoice is one row in the /notify picker: a (mode, focusMode) pair and
// the label the user reads.
type notifyChoice struct {
	label     string
	mode      string
	focusMode string
}

// notifyChoiceSubtitle renders the human explanation shown as the picker row's
// Meta text: "<what the mode does> — <when the focus fires>".
func (c notifyChoice) subtitle() string {
	modeDescriptions := map[string]string{
		string(notify.ModeOff):    "silent",
		string(notify.ModeBell):   "terminal bell only",
		string(notify.ModeNotify): "desktop notification only",
		string(notify.ModeBoth):   "terminal bell + desktop notification",
	}
	focusDescriptions := map[string]string{
		string(notify.FocusUnfocused): "only when the terminal is in the background",
		string(notify.FocusAlways):    "every time",
		string(notify.FocusFocused):   "only while the terminal is focused",
	}
	return modeDescriptions[c.mode] + " — " + focusDescriptions[c.focusMode]
}

// notifyPickerChoices enumerates the FULL mode x focus space (4 modes x 3
// focus modes = 12 rows), the way newThemePicker enumerates every theme. The
// earlier 4-row curated list could not represent the other 8 valid pairs, so
// opening the picker on one of them fell through to row 0 and Enter silently
// committed a different setting than the user's current one (maintainer
// review, PR #1001). Every valid pair must have a row so Enter always keeps
// (or explicitly changes) the user's actual setting.
func notifyPickerChoices() []notifyChoice {
	modes := []string{string(notify.ModeBoth), string(notify.ModeBell), string(notify.ModeNotify), string(notify.ModeOff)}
	foci := []string{string(notify.FocusUnfocused), string(notify.FocusAlways), string(notify.FocusFocused)}
	choices := make([]notifyChoice, 0, len(modes)*len(foci))
	for _, mode := range modes {
		for _, focus := range foci {
			choices = append(choices, notifyChoice{
				label:     mode + " · " + focus,
				mode:      mode,
				focusMode: focus,
			})
		}
	}
	return choices
}

// handleNotifyCommand implements /notify [list|off|bell|notify|both [focus]].
// Bare `/notify` opens the picker at the dispatch layer.
//
// Two contracts share this handler and must stay separate (maintainer review,
// PR #1001):
//
//   - LIVE policy (m.notifyMode/m.notifyFocusMode, m.notifier): what the
//     current session uses, initialized by newModel from the RESOLVED pair
//     (user config after project precedence). A mode-only change preserves the
//     live focus so a project's focus rule keeps applying to the session, and
//     the change reaches the notifier immediately.
//   - PERSISTED policy (the user's own config file): a partial write seeds
//     omitted fields from config.UserNotify — the user's global choice —
//     never from the resolved view, so a project's value cannot leak into the
//     global file and a blank stays blank.
//
// Mirrors handleThemeCommand.
func (m model) handleNotifyCommand(args string) (model, string) {
	tokens := strings.Fields(strings.TrimSpace(args))
	if len(tokens) == 0 {
		return m, m.notifyStateText()
	}
	if tokens[0] == "list" {
		if len(tokens) != 1 {
			return m, "Notify\nToo many arguments: " + args + " (usage: /notify list)"
		}
		return m, m.notifyStateText()
	}
	if len(tokens) > 2 {
		return m, "Notify\nToo many arguments: " + args + " (usage: /notify <off|bell|notify|both> [unfocused|always|focused])"
	}
	mode := strings.ToLower(strings.TrimSpace(tokens[0]))
	if !isValidNotifyMode(mode) {
		return m, "Notify\nUnknown mode: " + tokens[0] + " (expected off, bell, notify, or both; run /notify with no argument to pick from the list)"
	}
	// LIVE focus: an explicit token wins; otherwise preserve the in-session
	// value (which started as the resolved pair, project precedence included).
	liveFocus := m.notifyFocusMode
	focusExplicit := false
	if len(tokens) > 1 {
		focus := strings.ToLower(strings.TrimSpace(tokens[1]))
		if !isValidNotifyFocusMode(focus) {
			return m, "Notify\nUnknown focus mode: " + tokens[1] + " (expected unfocused, always, or focused)"
		}
		liveFocus = focus
		focusExplicit = true
	}
	m.notifyMode = mode
	m.notifyConfiguredMode = mode
	m.notifyFocusMode = liveFocus
	// Apply to the live notifier so the change takes effect on the next
	// notification in this session, not just after a restart. A blank
	// focusMode is fine here: the notifier treats blank as "unfocused".
	if m.notifier != nil {
		m.notifier.Configure(notify.Config{
			Mode:      notify.Mode(mode),
			FocusMode: notify.FocusMode(liveFocus),
		})
	}
	lines := []string{
		"Notify",
		"active mode: " + effectiveModeLabel(mode, m.notifyConfiguredMode) + ", focus: " + effectiveFocusLabel(liveFocus),
	}
	if note := m.persistNotifyPreference(mode, liveFocus, focusExplicit); note != "" {
		lines = append(lines, note)
	}
	return m, strings.Join(lines, "\n")
}

// effectiveFocusLabel renders a focus value for the state line: blank means the
// built-in "unfocused" default, so say so instead of showing an empty string.
func effectiveFocusLabel(focus string) string {
	if strings.TrimSpace(focus) == "" {
		return "unfocused (default)"
	}
	return focus
}

// effectiveModeLabel renders the notify mode for state lines: when the mode in
// effect came from the built-in default rather than an explicit choice (stored
// or in-session), label it "(default)" so the TUI agrees with the CLI's
// `mode: (default)` for the same configuration (maintainer review, PR #1001).
// A mode the user actually chose — including a previously stored "both" —
// renders bare.
func effectiveModeLabel(mode, configuredMode string) string {
	if strings.TrimSpace(mode) == strings.TrimSpace(configuredMode) {
		return mode
	}
	return mode + " (default)"
}

// persistNotifyPreference writes the choice to user config so it survives a
// restart, as ONE serialized read-merge-write (config.UpdateNotify): the lock
// covers reading the stored block and replacing the file, so a concurrent
// partial update cannot interleave (maintainer review, PR #1001).
//
// Persist contract: an explicit pair replaces both fields. A mode-only change
// preserves the focus stored in the USER'S OWN file (blank stays blank) — the
// merge leaves current.FocusMode untouched rather than pre-reading it, which
// would reopen the stale-preservation window the lock is there to close. Best-
// effort: returns a short note to surface on failure, or "" on success / when
// there is no config path (e.g. tests).
func (m model) persistNotifyPreference(mode string, focus string, focusExplicit bool) string {
	if strings.TrimSpace(m.userConfigPath) == "" {
		return ""
	}
	_, err := config.UpdateNotify(m.userConfigPath, func(current config.NotifyConfig) config.NotifyConfig {
		current.Mode = mode
		if focusExplicit {
			current.FocusMode = focus
		}
		return current
	})
	if err != nil {
		return "note: could not save notify preference (" + err.Error() + ")"
	}
	return ""
}

// notifyStateText renders the /notify state view: the LIVE session policy
// (model fields, initialized from the resolved pair and updated by /notify) —
// not the stored file, which can legitimately disagree when a project config
// overrides notify for this session (maintainer review, PR #1001).
func (m model) notifyStateText() string {
	sections := []commandSection{{
		Title: "State",
		Lines: []string{
			"active mode: " + effectiveModeLabel(m.notifyMode, m.notifyConfiguredMode),
			"active focus: " + effectiveFocusLabel(m.notifyFocusMode),
		},
	}}
	rows := make([]string, 0, 12)
	for _, c := range notifyPickerChoices() {
		rows = append(rows, c.label)
	}
	sections = append(sections, commandSection{
		Title: "Available",
		Lines: rows,
	})
	return renderCommandOutput(commandOutput{
		Title:    "Notify",
		Status:   commandStatusOK,
		Sections: sections,
		Hints:    []string{"run /notify with no argument to open the picker, or /notify <mode> [focus] to change directly"},
	})
}

// isValidNotifyMode reports whether s names one of the four notification modes.
func isValidNotifyMode(s string) bool {
	switch s {
	case string(notify.ModeOff), string(notify.ModeBell), string(notify.ModeNotify), string(notify.ModeBoth):
		return true
	}
	return false
}

// isValidNotifyFocusMode reports whether s names one of the three focus modes.
func isValidNotifyFocusMode(s string) bool {
	switch s {
	case string(notify.FocusUnfocused), string(notify.FocusAlways), string(notify.FocusFocused):
		return true
	}
	return false
}
