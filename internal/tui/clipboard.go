package tui

import (
	tea "charm.land/bubbletea/v2"

	"github.com/Gitlawb/zero/internal/imageinput"
	"github.com/atotto/clipboard"
)

// clipboardReadMsg carries the result of an async OS-clipboard read back to
// Update. It drives the right-click "paste" path.
type clipboardReadMsg struct {
	content string
	err     error
}

// clipboardImageMsg carries image bytes read from the OS clipboard (a
// screenshot). Sent when the text clipboard is empty — the user pasted an
// image, not text. data is nil when no image was found.
type clipboardImageMsg struct {
	data      []byte
	mediaType string
	err       error
}

// readClipboardImage is the OS clipboard image reader. A var so tests can
// substitute a stub instead of depending on the developer's real clipboard.
var readClipboardImage = imageinput.ReadClipboardImage

// readClipboardImageCmd reads the OS clipboard for image content off the
// Update goroutine. Returns a clipboardImageMsg with the bytes, or nil (no
// command) if there is no image — the caller treats nil as a silent no-op.
func readClipboardImageCmd() tea.Cmd {
	return func() tea.Msg {
		data, mediaType, err := readClipboardImage()
		if err != nil {
			return clipboardImageMsg{err: err}
		}
		if data == nil {
			return nil // no image — silent no-op
		}
		return clipboardImageMsg{data: data, mediaType: mediaType}
	}
}

// pasteFromClipboardCmd reads the OS clipboard off the Update goroutine (it
// shells out to pbpaste/xclip/etc.) and delivers the text as a clipboardReadMsg.
// A right-click pastes straight from here — no menu.
func pasteFromClipboardCmd() tea.Cmd {
	return func() tea.Msg {
		content, err := clipboard.ReadAll()
		return clipboardReadMsg{content: content, err: err}
	}
}

// routePaste inserts pasted text into whichever input surface is focused. It is
// shared by the terminal bracketed-paste handler (tea.PasteMsg) and the
// right-click paste (clipboardReadMsg) so a bracketed paste and a right-click
// paste behave identically. Surfaces with no editable text field (a permission/
// spec prompt, the MCP manager, or the detailed transcript) swallow the paste.
// Searchable pickers consume pasted text through the same filtering path as
// typed characters. The session rename editor accepts text only; empty content
// is a no-op there rather than an image probe.
func (m model) routePaste(content string) (tea.Model, tea.Cmd) {
	// A paste is a deliberate action, same as a keypress or click — it means
	// the user moved on to something else, so it disarms a stale Esc
	// cancel-confirmation. tea.PasteMsg/clipboardReadMsg aren't
	// tea.KeyPressMsg, so they don't go through that generic keypress hook;
	// do it once here, before any early return, so both the bracketed-paste
	// and right-click-paste paths are covered uniformly.
	m = m.disarmCancelConfirmation()
	// Same immediate-input transition as the keypress hook: text entering the
	// composer must land the caret in its solid-while-typing state right away
	// and refresh the typing timestamp. Without this, a paste right after the
	// blink phase hid the caret leaves the newly populated composer caret-less
	// until the next 530ms tick, and the stale timestamp makes that tick
	// toggle instead of going solid.
	m.lastCharTime = m.now()
	m.composerCursorVisible = true
	if m.renamePrompt != nil {
		if content == "" {
			return m, nil
		}
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(tea.PasteMsg{Content: sanitizeComposerInput(content)})
		return m, cmd
	}
	if content == "" {
		// Empty text clipboard — the user may have pasted a screenshot.
		// Probe the OS clipboard for image content asynchronously.
		return m, readClipboardImageCmd()
	}
	// Setup and the ask_user questionnaire share the main text input.
	if m.setup.visible {
		return m.handleSetupPaste(tea.PasteMsg{Content: content})
	}
	if m.pendingAskUser != nil {
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(tea.PasteMsg{Content: content})
		return m, cmd
	}
	if m.providerWizard != nil {
		return m.handleProviderWizardPaste(content)
	}
	if m.picker != nil {
		if m.modelPickerIsLoading() {
			return m, nil
		}
		m.picker.appendQuery([]rune(sanitizeComposerInput(content)))
		if m.picker.kind == pickerPet {
			return m.schedulePetPreview()
		}
		return m, nil
	}
	if m.transcriptDetailed || m.pendingSpecReview != nil || m.pendingPermission != nil || m.mcpAddWizard != nil || m.mcpManager != nil {
		return m, nil
	}
	// A drag-dropped image/PDF arrives as a (backslash-escaped) file path. Attach
	// it as an image instead of inserting the raw path — otherwise a "/Users/…"
	// path would be submitted as an unknown slash-command.
	if path, ok := droppedAttachmentPath(content, m.cwd); ok {
		return m.handleImageCommand(path), nil
	}
	state := m.currentComposerState()
	m = m.applyComposerText(state, content, true)
	m.recomputeSuggestions()
	return m, nil
}
