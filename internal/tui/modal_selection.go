package tui

import tea "charm.land/bubbletea/v2"

// moveModalSelection moves the highlight on an open selection surface by delta
// (−1 previous / +1 next), matching arrow-key behavior for that surface.
// handled is false when no list/selector modal owns the keys (caller may fall
// through to plan toggle, composer, etc.).
//
// Surfaces: permission options, ask_user option lists, provider/MCP wizards and
// managers, command pickers, slash/file autocomplete. Does not scroll the
// transcript or drive composer history — those stay arrow-only.
func (m model) moveModalSelection(delta int) (model, tea.Cmd, bool) {
	if delta == 0 {
		return m, nil, false
	}
	if m.pendingPermission != nil {
		return m.movePermissionCursor(delta), nil, true
	}
	if m.pendingAskUser != nil {
		// moveAskUserCursor is a no-op in free-text mode (parity with arrows).
		return m.moveAskUserCursor(delta), nil, true
	}
	if m.providerWizard != nil {
		m.burstCount = 0
		next, cmd := m.handleProviderWizardKey(syntheticArrowKey(delta))
		return next, cmd, true
	}
	if m.mcpAddWizard != nil {
		m.burstCount = 0
		next, cmd := m.handleMCPAddWizardKey(syntheticArrowKey(delta))
		return next, cmd, true
	}
	if m.mcpManager != nil {
		m.burstCount = 0
		next, cmd := m.handleMCPManagerKey(syntheticArrowKey(delta))
		return next, cmd, true
	}
	if m.picker != nil {
		if m.modelPickerIsLoading() {
			return m, nil, true
		}
		m, cmd := m.pickerMoved(delta)
		return m, cmd, true
	}
	if m.suggestionsActive() {
		m.moveSuggestion(delta)
		return m, nil, true
	}
	return m, nil, false
}

// syntheticArrowKey builds a KeyUp (delta < 0) or KeyDown (delta > 0) message so
// wizard/manager handlers that only match arrow keys can share one code path
// with Ctrl+P / Ctrl+N.
func syntheticArrowKey(delta int) tea.KeyPressMsg {
	code := tea.KeyDown
	if delta < 0 {
		code = tea.KeyUp
	}
	return tea.KeyPressMsg(tea.Key{Code: code})
}
