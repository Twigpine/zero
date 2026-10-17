package tui

import (
	"context"
	"fmt"
	"github.com/Twigpine/zero/internal/terminalpet"
	"github.com/charmbracelet/x/ansi"
	"image"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/Twigpine/zero/internal/agent"
)

// These drive the REAL key handler rather than the pure cycle functions.
//
// The pure functions are easy to get right. The dangerous part is the offer
// state living on the model across keypresses: if any path fails to clear it, a
// later innocent ctrl+g commits full-auto mode with nobody having decided
// anything, and nothing would say so. The disarm is written as an unconditional
// clear at the top of the key handler with only shift+tab re-arming, and these
// exist to prove that inversion actually holds through the handler.

// newModel rather than a model literal: the key handler calls m.now() before any
// early return, so a literal leaves that hook nil and every keypress panics.
//
// The filename matters as much as the constructor. This file was originally
// permission_mode_arm_test.go, and Go read the trailing "_arm" as a GOARCH
// constraint and excluded it from every amd64 build. None of these tests ran,
// locally or in CI, so the panic never surfaced and the offer gate they exist to
// prove had no coverage at all. See TestPermissionModeOfferTestsActuallyRun.
func modelInMode(t *testing.T, mode agent.PermissionMode) model {
	t.Helper()
	m := newModel(context.Background(), Options{})
	m.permissionMode = mode
	return m
}

func armedModel(t *testing.T) model {
	t.Helper()
	armed := pressKey(t, modelInMode(t, agent.PermissionModeAsk), tea.Key{Code: tea.KeyTab, Mod: tea.ModShift})
	if !armed.unsafeArmed {
		t.Fatal("shift+tab from Ask did not raise the full-auto offer")
	}
	if armed.permissionMode != agent.PermissionModeAsk {
		t.Fatalf("mode changed to %s while merely offering full-auto", armed.permissionMode)
	}
	return armed
}

func pressKey(t *testing.T, m model, key tea.Key) model {
	t.Helper()
	next, _ := m.updateModel(tea.KeyPressMsg(key))
	got, ok := next.(model)
	if !ok {
		t.Fatalf("updateModel returned %T, want model", next)
	}
	return got
}

// The confirm key immediately after the offer commits full-auto. This is the
// feature working.
func TestConfirmKeyCommitsUnsafeRightAfterTheOffer(t *testing.T) {
	m := pressKey(t, armedModel(t), tea.Key{Code: 'g', Mod: tea.ModCtrl})
	if m.permissionMode != agent.PermissionModeFullAuto {
		t.Fatalf("mode = %s after confirming, want full-auto", m.permissionMode)
	}
	if m.unsafeArmed {
		t.Error("offer still live after being accepted")
	}
}

// THE ONE THAT MATTERS. Any other key in between must cancel the offer, so the
// confirm key afterwards does nothing. Each of these is a separate path through
// the handler, and every one of them has to clear.
func TestAnyOtherKeyCancelsTheUnsafeOffer(t *testing.T) {
	for name, key := range map[string]tea.Key{
		"printable":     {Code: 'a', Text: "a"},
		"space":         {Code: tea.KeySpace, Text: " "},
		"escape":        {Code: tea.KeyEscape},
		"enter":         {Code: tea.KeyEnter},
		"backspace":     {Code: tea.KeyBackspace},
		"plain tab":     {Code: tea.KeyTab},
		"up arrow":      {Code: tea.KeyUp},
		"unrelated ctl": {Code: 'b', Mod: tea.ModCtrl},
	} {
		t.Run(name, func(t *testing.T) {
			m := pressKey(t, armedModel(t), key)
			if m.unsafeArmed {
				t.Fatalf("%s left the full-auto offer live, so a later ctrl+g would commit it silently", name)
			}
			// And prove the consequence rather than trusting the flag.
			m = pressKey(t, m, tea.Key{Code: 'g', Mod: tea.ModCtrl})
			if m.permissionMode == agent.PermissionModeFullAuto {
				t.Fatalf("ctrl+g after %s entered full-auto mode with no live offer", name)
			}
		})
	}
}

// Input is not only keys. The offer state lives on the model across EVERY
// message, so a path that does not clear it is a path to full-auto: pasting and
// then pressing ctrl+g for its ordinary meaning used to enter the mode with
// nobody having accepted anything.
//
// These are separate dispatch cases from the key handler, which is exactly why
// a key-only table missed them.
func TestPasteAndMouseCancelTheUnsafeOffer(t *testing.T) {
	for name, msg := range map[string]tea.Msg{
		"paste":        tea.PasteMsg{Content: "hello"},
		"mouse click":  tea.MouseClickMsg{},
		"mouse wheel":  tea.MouseWheelMsg{},
		"mouse releas": tea.MouseReleaseMsg{},
	} {
		t.Run(name, func(t *testing.T) {
			next, _ := armedModel(t).updateModel(msg)
			m := next.(model)
			if m.unsafeArmed {
				t.Fatalf("%s left the full-auto offer live, so a later ctrl+g would commit it silently", name)
			}
			m = pressKey(t, m, tea.Key{Code: 'g', Mod: tea.ModCtrl})
			if m.permissionMode == agent.PermissionModeFullAuto {
				t.Fatalf("ctrl+g after %s entered full-auto mode with no live offer", name)
			}
		})
	}
}

// Motion is the deliberate exception. Terminals stream it while tracking is on,
// so cancelling on a twitch would retract the offer before it could be accepted
// and make the gate unusable rather than safe.
func TestMouseMotionAloneDoesNotCancelTheOffer(t *testing.T) {
	next, _ := armedModel(t).updateModel(tea.MouseMotionMsg{})
	if !next.(model).unsafeArmed {
		t.Fatal("passive mouse motion withdrew the full-auto offer, so moving the mouse makes the confirm key unreachable")
	}
}

// The confirm key with no offer at all must be inert, so it cannot be used as a
// standalone shortcut into full-auto.
func TestConfirmKeyAloneNeverEntersUnsafe(t *testing.T) {
	for _, start := range []agent.PermissionMode{agent.PermissionModeAuto, agent.PermissionModeAsk} {
		m := modelInMode(t, start)
		for press := 0; press < 3; press++ {
			m = pressKey(t, m, tea.Key{Code: 'g', Mod: tea.ModCtrl})
			if m.permissionMode == agent.PermissionModeFullAuto {
				t.Fatalf("ctrl+g alone from %s reached full-auto on press %d", start, press+1)
			}
		}
	}
}

// Repeated shift+tab must never commit unsafe, only offer and then decline.
// Someone holding the key down must not end up with prompts disabled.
func TestHoldingShiftTabNeverCommitsUnsafe(t *testing.T) {
	m := modelInMode(t, agent.PermissionModeAuto)
	for press := 0; press < 10; press++ {
		m = pressKey(t, m, tea.Key{Code: tea.KeyTab, Mod: tea.ModShift})
		if m.permissionMode == agent.PermissionModeFullAuto {
			t.Fatalf("shift+tab alone reached full-auto on press %d", press+1)
		}
	}
}

// The offer must be visible, name the key, and not claim the mode has changed.
func TestOfferLabelNamesTheConfirmKey(t *testing.T) {
	label, _ := armedModel(t).modeLabel()
	if label == "full-auto" {
		t.Fatal("the offer renders as though full-auto is already active")
	}
	for _, want := range []string{"full-auto", "ctrl+g"} {
		if !strings.Contains(label, want) {
			t.Errorf("offer label %q does not mention %q", label, want)
		}
	}
}

// AN OFFER THAT IS NOT ON SCREEN MUST NOT BE ACCEPTABLE.
//
// The offer lives in the left chip, and several statuses used to take that chip
// over wholesale: an active recording, a transcription, a model download. Arming
// while the mic was live therefore rendered REC while ctrl+g still committed
// full-auto, so the user was asked to confirm something the footer never put in
// front of them, and the permission prompts went away. Reported by @jatmn.
//
// The invariant is one implication, asserted through the REAL renderer and the
// REAL handler: if the mode became full-auto, the frame the user saw before
// pressing the key contained the offer. Matrixed over the states that contend
// for the chip and the width tiers, because the two branches of statusLine
// reach the chip by different routes.
func TestConfirmOnlyCommitsAnOfferTheFooterShowed(t *testing.T) {
	for _, width := range []int{40, 96, 160} {
		for _, state := range []struct {
			name  string
			apply func(model) model
		}{
			{"idle", func(m model) model { return m }},
			{"recording", func(m model) model {
				m.dictation.phase = dictRecording
				return m
			}},
			{"transcribing", func(m model) model {
				m.dictation.phase = dictTranscribing
				return m
			}},
			{"downloading a model", func(m model) model {
				m.dictation.downloading = true
				m.dictation.downloadStatus = "downloading 42%"
				return m
			}},
		} {
			t.Run(fmt.Sprintf("%s/%d", state.name, width), func(t *testing.T) {
				armed := state.apply(armedModel(t))
				armed.width = width

				shown := armed.statusLine(width)
				confirmed := pressKey(t, armed, tea.Key{Code: 'g', Mod: tea.ModCtrl})

				if confirmed.permissionMode == agent.PermissionModeFullAuto &&
					!strings.Contains(shown, "ctrl+g") {
					t.Fatalf("ctrl+g committed full-auto while the footer showed %q, "+
						"so the offer was accepted without ever being displayed", shown)
				}
				// The feature still has to work where the offer IS shown, or a fix
				// that simply made confirmation unreachable would pass the above.
				if strings.Contains(shown, "ctrl+g") &&
					confirmed.permissionMode != agent.PermissionModeFullAuto {
					t.Fatalf("the footer offered full-auto (%q) but ctrl+g did not commit it: mode=%s",
						shown, confirmed.permissionMode)
				}
			})
		}
	}
}

// THE OFFER IS CONFIRMABLE ONLY WHERE THE FOOTER SHOWS ITS KEY.
//
// The footer reserves columns for a docked pet before it renders the status
// line, so at 24 and 30 columns the offer came out as `  ● full-aut…` and
// `  ● full-auto? ctr…`, no confirmation key in either, while ctrl+g still
// entered full-auto. The contract, checked through the complete View: either
// the rendered footer carries the key and ctrl+g confirms, or it does not and
// ctrl+g cannot. Wider and pet-free layouts are the controls, so the rule
// cannot pass by refusing confirmation everywhere.
func TestOfferConfirmsOnlyWhereTheFooterShowsItsKey(t *testing.T) {
	for _, testCase := range []struct {
		name        string
		width       int
		pet         bool
		wantVisible bool
	}{
		{"24 columns with a docked pet", 24, true, false},
		{"30 columns with a docked pet", 30, true, false},
		{"30 columns without a pet", 30, false, true},
		{"96 columns with a docked pet", 96, true, true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			m := newModel(context.Background(), Options{AltScreen: true})
			m.permissionMode = agent.PermissionModeAsk
			m.width, m.height = testCase.width, 24
			if testCase.pet {
				frame := image.NewNRGBA(image.Rect(0, 0, 12, 12))
				m.petAnimation, _ = terminalpet.ThumbnailAnimation(frame)
				m.petID = "boba"
				m.petRenderer = terminalpet.NewImageRenderer(terminalpet.ImageSupport{Protocol: terminalpet.ImageProtocolKitty})
				if m.petComposerReservedColumns(m.width) == 0 {
					t.Fatal("SETUP INVALID: the docked pet reserved no footer columns")
				}
			}
			armed := pressKey(t, m, tea.Key{Code: tea.KeyTab, Mod: tea.ModShift})
			view := ansi.Strip(armed.View().Content)
			visible := strings.Contains(view, "ctrl+g")
			if visible != testCase.wantVisible {
				t.Fatalf("footer key visible = %v, want %v; footer read:\n%s", visible, testCase.wantVisible, view)
			}
			confirmed := pressKey(t, armed, tea.Key{Code: 'g', Mod: tea.ModCtrl})
			entered := confirmed.permissionMode == agent.PermissionModeFullAuto
			if visible && !entered {
				t.Fatalf("the footer showed the confirmation key but ctrl+g did not enter full-auto (mode %s)", confirmed.permissionMode)
			}
			if !visible && entered {
				t.Fatal("ctrl+g entered full-auto on an offer the footer never showed the key for")
			}
		})
	}
}

// And an offer that was visible when raised is withdrawn when the terminal
// shrinks under it, rather than staying confirmable behind a truncated chip.
func TestOfferIsWithdrawnWhenTheTerminalShrinksUnderIt(t *testing.T) {
	m := newModel(context.Background(), Options{AltScreen: true})
	m.permissionMode = agent.PermissionModeAsk
	m.width, m.height = 96, 24
	armed := pressKey(t, m, tea.Key{Code: tea.KeyTab, Mod: tea.ModShift})
	if !armed.unsafeArmed {
		t.Fatal("SETUP INVALID: shift+tab at 96 columns did not raise the offer")
	}
	next, _ := armed.updateModel(tea.WindowSizeMsg{Width: 12, Height: 24})
	shrunk := next.(model)
	if shrunk.unsafeArmed {
		t.Fatal("the offer stayed live after shrinking to a width that cannot show its key")
	}
	if confirmed := pressKey(t, shrunk, tea.Key{Code: 'g', Mod: tea.ModCtrl}); confirmed.permissionMode == agent.PermissionModeFullAuto {
		t.Fatal("ctrl+g entered full-auto after the offer was withdrawn")
	}
}
