package tui

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/Twigpine/zero/internal/agent"
	"github.com/Twigpine/zero/internal/peermsg"
)

// peerDeliveriesForOfferTest is one production message of every peer type. The
// guard test below fails if a peer*Msg type exists that is not a key here.
func peerDeliveriesForOfferTest() map[string]tea.Msg {
	inbound := peermsg.InboundMessage{
		ID:      "msg-1",
		From:    peermsg.Peer{Identity: peermsg.Identity{SessionID: "other-session", Name: "other"}},
		Body:    "please look at the failing build",
		Summary: "failing build",
	}
	needsApproval := inbound
	needsApproval.ID = "msg-2"
	needsApproval.RequiresApproval = true
	return map[string]tea.Msg{
		"peerMessageMsg":         peerMessageMsg{message: needsApproval},
		"peerStatusMsg":          peerStatusMsg{event: peermsg.StatusEvent{MessageID: "msg-3", Peer: inbound.From, Status: peermsg.DeliveryDelivered}},
		"peerHeldReleasedMsg":    peerHeldReleasedMsg{message: inbound},
		"peerDecisionMsg":        peerDecisionMsg{message: needsApproval, allow: false},
		"peerRuntimeErrorMsg":    peerRuntimeErrorMsg{err: errors.New("socket closed")},
		"peerReceiptErrorMsg":    peerReceiptErrorMsg{err: errors.New("receipt not written")},
		"peerApprovalExpiredMsg": peerApprovalExpiredMsg{messageID: "msg-2"},
	}
}

// CROSS-SESSION TRAFFIC ENDS A FULL-AUTO OFFER.
//
// shift+tab offers full-auto and the very next ctrl+g confirms it. Everything
// else that changes what the user is looking at cancels the offer, so that
// ctrl+g cannot turn permission prompts off several events after the
// confirmation was shown. Paste, blur, mouse, clipboard and dictation did that;
// peer delivery did not, so an approval prompt from another session could arrive
// between the offer and an unrelated ctrl+g and full-auto was still entered.
//
// Every peer message type goes through Update as production delivers it. The
// offer has to be gone, ctrl+g has to leave the mode alone, and a fresh
// shift+tab has to be able to offer again, so the test cannot pass by the mode
// cycle having become unreachable. Reported by @jatmn.
func TestPeerDeliveryCancelsTheFullAutoOffer(t *testing.T) {
	deliveries := peerDeliveriesForOfferTest()
	names := make([]string, 0, len(deliveries))
	for name := range deliveries {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			m := armFullAutoOffer(t)
			modeBefore := m.permissionMode

			updated, _ := m.Update(deliveries[name])
			m = updated.(model)
			if m.unsafeArmed {
				t.Errorf("the offer survived %s, so an unrelated ctrl+g can still enter full-auto", name)
			}

			updated, _ = m.Update(testKeyCtrl('g'))
			m = updated.(model)
			if m.permissionMode == agent.PermissionModeFullAuto {
				t.Fatalf("full-auto entered after %s, with no fresh offer in front of the keypress", name)
			}
			if m.permissionMode != modeBefore {
				t.Errorf("permission mode moved from %q to %q without an offer", modeBefore, m.permissionMode)
			}

			// A message that needs approval really did raise its prompt, which is
			// the user-visible transition this whole rule is about. The prompt is
			// modal, so it has to go away before anything can be offered. Expiry is
			// the production path that closes it without a keypress.
			if name == "peerMessageMsg" {
				if m.peerPendingApproval == nil {
					t.Fatalf("SETUP INVALID: the approval-requiring message raised no prompt, so this leg drove nothing")
				}
				updated, _ = m.Update(peerApprovalExpiredMsg{messageID: m.peerPendingApproval.ID})
				m = updated.(model)
			}

			// The offer is cancelled, not broken: asking again offers again, and
			// that offer confirms.
			updated, _ = m.Update(testKeyShift(tea.KeyTab))
			m = updated.(model)
			if !m.unsafeArmed {
				t.Fatalf("a fresh shift+tab after %s did not offer full-auto again (mode %q)", name, m.permissionMode)
			}
			updated, _ = m.Update(testKeyCtrl('g'))
			if got := updated.(model).permissionMode; got != agent.PermissionModeFullAuto {
				t.Errorf("ctrl+g straight after the fresh offer gave mode %q, want full-auto", got)
			}
		})
	}
}

// And the offer still works when nothing intervenes, or every leg above would
// pass on a model that could never enter full-auto at all.
func TestTheFullAutoOfferStillConfirmsWithNothingInBetween(t *testing.T) {
	m := armFullAutoOffer(t)
	updated, _ := m.Update(testKeyCtrl('g'))
	if got := updated.(model).permissionMode; got != agent.PermissionModeFullAuto {
		t.Fatalf("SETUP INVALID: ctrl+g straight after the offer gave mode %q, so the cancellation legs prove nothing", got)
	}
}

// A message that is not peer traffic must not be swept up by the family check.
func TestOrdinaryMessagesAreNotPeerDeliveries(t *testing.T) {
	for _, msg := range []tea.Msg{composerBlinkMsg{}, tea.WindowSizeMsg{Width: 96, Height: 30}, agentRowMsg{}, nil} {
		if isPeerDelivery(msg) {
			t.Errorf("%T is classified as peer delivery", msg)
		}
	}
}

// THE FAMILY IS CLOSED, AND THIS KEEPS IT CLOSED. isPeerDelivery is a list, and
// what it decides is a permission boundary. A peer message type added later and
// not added there would keep an offer alive across traffic the user can see, and
// nothing would fail. This reads the package source for every peer*Msg type and
// requires each one to be classified and to have a leg in the test above.
func TestEveryPeerMessageTypeEndsTheFullAutoOffer(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	shape := regexp.MustCompile(`^peer[A-Z][A-Za-z]*Msg$`)
	var declared []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range file.Decls {
			general, ok := decl.(*ast.GenDecl)
			if !ok || general.Tok != token.TYPE {
				continue
			}
			for _, spec := range general.Specs {
				if typeName := spec.(*ast.TypeSpec).Name.Name; shape.MatchString(typeName) {
					declared = append(declared, typeName)
				}
			}
		}
	}
	sort.Strings(declared)
	if len(declared) == 0 {
		t.Fatal("SETUP INVALID: no peer message types were found in the package source")
	}

	deliveries := peerDeliveriesForOfferTest()
	for _, name := range declared {
		msg, covered := deliveries[name]
		if !covered {
			t.Errorf("%s is a peer message type with no leg in TestPeerDeliveryCancelsTheFullAutoOffer; add it there and to isPeerDelivery", name)
			continue
		}
		if !isPeerDelivery(msg) {
			t.Errorf("%s is not classified by isPeerDelivery, so it would leave a full-auto offer armed", name)
		}
	}
	if len(deliveries) != len(declared) {
		t.Errorf("the test table names %d peer types and the package declares %d: %v", len(deliveries), len(declared), declared)
	}
}
