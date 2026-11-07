package tui

import (
	"github.com/Twigpine/zero/internal/sessions"
)

// subchatState manages the drill-in view for a specialist's child session.
// When active, the transcript body swaps to show the child session's events
// instead of the parent's. ArrowUp/Esc pops back to the parent view.
type subchatState struct {
	// active is true when the transcript is showing a child session.
	active bool
	// childSessionID is the session being viewed.
	childSessionID string
	// childSessionTitle is the display title for the subchat nav bar.
	childSessionTitle string
	// parentScrollOffset preserves the chat scroll position so popping back
	// returns to the same view.
	parentScrollOffset int
	// childRows are the rehydrated transcript rows from the child session.
	childRows []transcriptRow
}

// enter loads the child session's events and activates the subchat view.
// It returns an error message string if the session could not be loaded.
func (s *subchatState) enter(store *sessions.Store, childSessionID, title string, parentScrollOffset int) string {
	if store == nil || childSessionID == "" {
		return "No session store available."
	}
	events, err := store.ReadEvents(childSessionID)
	if err != nil {
		return "Could not load subagent session: " + err.Error()
	}
	s.active = true
	s.childSessionID = childSessionID
	s.childSessionTitle = title
	s.parentScrollOffset = parentScrollOffset
	s.childRows = transcriptRowsFromSessionEvents(events)
	return ""
}

// exit deactivates the subchat view and returns the saved parent scroll offset.
func (s *subchatState) exit() int {
	offset := s.parentScrollOffset
	s.active = false
	s.childSessionID = ""
	s.childSessionTitle = ""
	s.parentScrollOffset = 0
	s.childRows = nil
	return offset
}

// renderSubchatNavBar renders the navigation bar shown at the top of the
// subchat view, telling the user how to get back to the main chat.
func renderSubchatNavBar(title string, width int) string {
	nav := "← Back to main chat (ArrowUp/Esc)"
	if title != "" {
		nav += "  ·  " + truncateRunes(title, width-40)
	}
	return zeroTheme.accent.Render(nav)
}
