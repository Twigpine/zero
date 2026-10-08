package tui

import (
	"errors"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/Gitlawb/zero/internal/config"
	"github.com/Gitlawb/zero/internal/dictation"
)

// sttKeyPromptState is the inline API-key prompt shown when a cloud dictation
// provider is chosen. On submit the key is saved to the credential store and the
// model selection is finalized. When optional is set a key is already on file, so
// Esc/empty-Enter keeps it and still applies the model (this is the proactive path
// for replacing a key that turned out to be invalid — you don't have to trigger a
// failed recording first).
type sttKeyPromptState struct {
	provider   string // "groq" / "openai"
	label      string // "Groq" / "OpenAI"
	modelValue string // the picker value to finalize, e.g. "groq:whisper-large-v3-turbo"
	input      string
	optional   bool // a key already resolves; entering a new one just replaces it
}

// maybeOfferKeyOnAuthError, when err is a cloud AuthError, reopens the API-key
// prompt for the currently-selected cloud provider (preserving its model) so the
// user can fix a missing/invalid key and retry. Returns handled=false for any
// other error (or when no key can be saved / the provider is local).
func (m model) maybeOfferKeyOnAuthError(err error) (model, bool) {
	var authErr *dictation.AuthError
	if !errors.As(err, &authErr) || m.dictation.saveKey == nil {
		return m, false
	}
	provider := string(m.dictation.cfg.Provider)
	if provider == "" || m.dictation.cfg.STTProvider() == config.STTProviderLocal {
		return m, false
	}
	value := provider + sttValueSep + m.dictation.cfg.Model
	m = m.appendSystemNotice(dictationErrorText(err) + " — paste your " + titleCase(provider) + " API key below to fix it and retry.")
	// The existing key is invalid, so a new one is required here (Esc cancels).
	return m.openSTTKeyPrompt(provider, value, false), true
}

// openSTTKeyPrompt begins collecting the API key for a cloud provider. optional
// marks that a key already resolves, so the prompt can be dismissed to keep it.
func (m model) openSTTKeyPrompt(provider, modelValue string, optional bool) model {
	m.sttKeyPrompt = &sttKeyPromptState{
		provider:   provider,
		label:      titleCase(provider),
		modelValue: modelValue,
		optional:   optional,
	}
	m.clearSuggestions()
	return m
}

// handleSTTKeyPromptKey captures typing for the API-key prompt. It fully owns
// the keystroke while the prompt is open (masked input, Enter saves, Esc cancels).
func (m model) handleSTTKeyPromptKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	p := m.sttKeyPrompt
	switch {
	case keyIs(msg, tea.KeyEsc), keyCtrl(msg, 'c'):
		if p.optional {
			// A key is already on file — keep it and apply the model selection.
			return m.finalizeSTTModelFromKey(p, "Kept your saved "+p.label+" API key. ")
		}
		m.sttKeyPrompt = nil
		return m.showTransientNoticeInline("Cancelled — no "+p.label+" API key saved.", transientNoticeInfo), nil
	case keyIs(msg, tea.KeyEnter):
		return m.submitSTTKey()
	case keyBackspace(msg):
		if r := []rune(p.input); len(r) > 0 {
			p.input = string(r[:len(r)-1])
		}
		return m, nil
	case keyCtrl(msg, 'u'):
		p.input = ""
		return m, nil
	default:
		if t := keyText(msg); t != "" && !keyAlt(msg) && !keyHasMod(msg, tea.ModCtrl) {
			p.input += t
		}
		return m, nil
	}
}

// submitSTTKey saves the entered key and finalizes the pending model selection.
func (m model) submitSTTKey() (tea.Model, tea.Cmd) {
	p := m.sttKeyPrompt
	key := strings.TrimSpace(p.input)
	if key == "" {
		if p.optional {
			// Nothing typed but a key is already on file — keep it and apply the model.
			return m.finalizeSTTModelFromKey(p, "Kept your saved "+p.label+" API key. ")
		}
		m.sttKeyPrompt = nil
		return m.showTransientNoticeInline("No key entered — nothing saved.", transientNoticeInfo), nil
	}
	if m.dictation.saveKey != nil {
		if err := m.dictation.saveKey(p.provider, key); err != nil {
			m.sttKeyPrompt = nil
			return m.appendSystemNotice("Couldn't save the " + p.label + " API key: " + err.Error()), nil
		}
	}
	return m.finalizeSTTModelFromKey(p, "Saved your "+p.label+" API key. ")
}

// finalizeSTTModelFromKey closes the prompt and commits the pending cloud model
// selection, prefixing the confirmation with a key-status note.
func (m model) finalizeSTTModelFromKey(p *sttKeyPromptState, prefix string) (tea.Model, tea.Cmd) {
	m.sttKeyPrompt = nil
	provider, modelID, _ := strings.Cut(p.modelValue, sttValueSep)
	kind := config.STTProviderKind(provider)
	m.dictation.cfg.Provider = kind
	m.dictation.cfg.Model = modelID
	if _, err := config.SetSTTModel(m.userConfigPath, kind, modelID); err != nil {
		return m.appendSystemNotice(prefix + "But couldn't save the model: " + err.Error()), nil
	}
	label := p.label
	if modelID != "" {
		label += " " + modelID
	}
	return m.showTransientNoticeInline(prefix+"Dictation model: "+label+". Run /voice; "+voiceCaptureUsage+".", transientNoticeSuccess), nil
}

// sttKeyPromptOverlay renders the API-key prompt as a centered modal, matching
// the picker's box styling. The key is masked as it is typed.
func (m model) sttKeyPromptOverlay(width int) string {
	if m.sttKeyPrompt == nil {
		return ""
	}
	p := m.sttKeyPrompt
	overlayWidth := minInt(width, pickerOverlayMaxWidth)
	if overlayWidth < pickerOverlayMinWidth {
		overlayWidth = width
	}
	// Input line mirrors the provider wizard's credential step (renderCredentialStep):
	// an "api key > " prompt, cursor-before-placeholder when empty, masked key with a
	// trailing cursor once typing starts — so both key prompts read the same.
	value := zeroTheme.accent.Render("▌") + zeroTheme.faint.Render("paste key here")
	if p.input != "" {
		value = zeroTheme.ink.Render(maskedProviderWizardKey(p.input)) + zeroTheme.accent.Render("▌")
	}
	input := zeroTheme.userPrompt.Render("api key > ") + value
	intro := p.label + " isn't set up yet. Paste or type your " + p.label + " API key:"
	footer := "⏎ save · Esc cancel · stored in Zero's credential store"
	if p.optional {
		// A key already resolves — this pass is for replacing an invalid/old one.
		intro = p.label + " already has an API key. Paste a new one to replace it, or press Esc to keep it:"
		footer = "⏎ replace · Esc keep saved key · stored in Zero's credential store"
	}
	lines := []string{
		zeroTheme.ink.Render(intro),
		"",
		input,
		"",
		zeroTheme.faint.Render(footer),
	}
	return centerRenderedBlock(styledBlockFillTitle(overlayWidth, p.label+" API key", lines, zeroTheme.lineStrong, lipgloss.NewStyle()), width)
}
