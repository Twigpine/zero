package tui

import (
	"context"
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/Twigpine/zero/internal/agent"
	"github.com/Twigpine/zero/internal/sandbox"
	"github.com/Twigpine/zero/internal/tools"
	"github.com/Twigpine/zero/internal/zeroruntime"
)

func pendingPermissionModel(t *testing.T, decide func(agent.PermissionDecision)) model {
	t.Helper()
	return pendingPermissionModelWithRequest(t, testPromptPermissionRequest(), decide)
}

func pendingPermissionModelWithRequest(t *testing.T, request agent.PermissionRequest, decide func(agent.PermissionDecision)) model {
	t.Helper()
	m := newModel(context.Background(), Options{})
	m.pending = true
	m.activeRunID = 7
	updated, _ := m.Update(permissionRequestMsg{
		runID:   7,
		request: request,
		decide:  decide,
	})
	next := updated.(model)
	if next.pendingPermission == nil {
		t.Fatal("setup: expected a pending permission prompt")
	}
	return next
}

func TestPermissionCursorDefaultsToAllowOnce(t *testing.T) {
	m := pendingPermissionModel(t, func(agent.PermissionDecision) {})
	if m.pendingPermission.cursor != 0 {
		t.Fatalf("default cursor = %d, want 0 (approve)", m.pendingPermission.cursor)
	}
}

func TestPermissionOptionsEmptyDecisionsUseRecoverableFallback(t *testing.T) {
	options := permissionOptions(agent.PermissionRequest{})
	if len(options) != 2 {
		t.Fatalf("fallback options = %#v, want allow and deny only", options)
	}
	if options[0].choice != permissionDecisionAllow || options[1].choice != permissionDecisionDeny {
		t.Fatalf("fallback options = %#v, want allow then deny", options)
	}
}

func TestPermissionOptionsExposeApprovalCancelWhenSupplied(t *testing.T) {
	request := agent.PermissionRequest{
		ToolName: "bash",
		AvailableDecisions: []agent.PermissionDecisionAction{
			agent.PermissionDecisionAllow,
			agent.PermissionDecisionAllowForSession,
			agent.PermissionDecisionDeny,
			agent.PermissionDecisionCancel,
		},
	}
	options := permissionOptions(request)
	if len(options) != 4 {
		t.Fatalf("options = %#v, want four supplied choices", options)
	}
	if options[2].choice != permissionDecisionDeny || options[2].hotkey != "d" {
		t.Fatalf("recoverable deny option = %#v, want deny on d", options[2])
	}
	if options[3].choice != permissionDecisionCancel || options[3].hotkey != "n" {
		t.Fatalf("cancel option = %#v, want cancel on n", options[3])
	}

	card, _ := renderFocusedPermissionPrompt(request, 3, false, "", 80)
	got := plainRender(t, card)
	for _, want := range []string{"continue without running it", "[d]", "tell Zero what to do differently", "[n]"} {
		if !strings.Contains(got, want) {
			t.Fatalf("permission card = %q, missing %q", got, want)
		}
	}
}

func TestPermissionOptionsExposeCommandPrefixApproval(t *testing.T) {
	request := agent.PermissionRequest{
		ToolName:      "bash",
		CommandPrefix: []string{"git", "status"},
		AvailableDecisions: []agent.PermissionDecisionAction{
			agent.PermissionDecisionAllow,
			agent.PermissionDecisionAllowPrefix,
			agent.PermissionDecisionCancel,
		},
	}
	options := permissionOptions(request)
	if len(options) != 3 || options[1].choice != permissionDecisionAllowPrefix || options[1].hotkey != "p" {
		t.Fatalf("prefix option = %#v, want p hotkey in supplied order", options)
	}
	card, _ := renderFocusedPermissionPrompt(request, 1, false, "", 100)
	got := plainRender(t, card)
	for _, want := range []string{"allow `git status` in this session", "[p]"} {
		if !strings.Contains(got, want) {
			t.Fatalf("permission card = %q, missing %q", got, want)
		}
	}
}

func TestPermissionPromptMapsEscalatedSandboxReason(t *testing.T) {
	request := agent.PermissionRequest{
		ToolName:   "exec_command",
		SideEffect: "shell",
		Reason:     sandbox.ReasonEscalatedSandboxRequired,
		AvailableDecisions: []agent.PermissionDecisionAction{
			agent.PermissionDecisionAllow,
			agent.PermissionDecisionDeny,
		},
	}
	card, _ := renderFocusedPermissionPrompt(request, 0, false, "", 96)
	got := plainRender(t, card)
	if !strings.Contains(got, "This command needs to run outside the sandbox.") {
		t.Fatalf("permission card = %q, missing user-facing sandbox reason", got)
	}
	if strings.Contains(got, sandbox.ReasonEscalatedSandboxRequired) {
		t.Fatalf("permission card leaked internal sandbox reason: %q", got)
	}
}

func TestPermissionPromptWrapsLongReasonWithoutTruncating(t *testing.T) {
	request := agent.PermissionRequest{
		ToolName:   "list_directory",
		SideEffect: "read",
		Reason:     "Reading /home/dev/projects/a-very-long-directory-name/with/nested/configuration requires access outside the workspace.",
		AvailableDecisions: []agent.PermissionDecisionAction{
			agent.PermissionDecisionAllow,
			agent.PermissionDecisionDeny,
		},
	}

	card, _ := renderFocusedPermissionPrompt(request, 0, false, "", 60)
	got := plainRender(t, card)
	for _, want := range []string{"Reading", "/home/dev/projects", "requires access outside the workspace."} {
		if !strings.Contains(got, want) {
			t.Fatalf("permission card truncated reason; missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "…") {
		t.Fatalf("permission card must wrap the decision reason instead of truncating it:\n%s", got)
	}
}

func TestPermissionOptionsExposePersistentCommandPrefixApproval(t *testing.T) {
	request := agent.PermissionRequest{
		ToolName:      "bash",
		CommandPrefix: []string{"git", "status"},
		AvailableDecisions: []agent.PermissionDecisionAction{
			agent.PermissionDecisionAllow,
			agent.PermissionDecisionAllowPrefix,
			agent.PermissionDecisionAlwaysAllowPrefix,
			agent.PermissionDecisionCancel,
		},
	}
	options := permissionOptions(request)
	if len(options) != 4 || options[2].choice != permissionDecisionAlwaysAllowPrefix || options[2].hotkey != "y" {
		t.Fatalf("persistent prefix option = %#v, want y hotkey in supplied order", options)
	}
	card, _ := renderFocusedPermissionPrompt(request, 2, false, "", 100)
	got := plainRender(t, card)
	for _, want := range []string{"always allow `git status`", "[y]"} {
		if !strings.Contains(got, want) {
			t.Fatalf("permission card = %q, missing %q", got, want)
		}
	}
}

func TestPermissionOptionsCanExposePatchCancelWithoutRecoverableDeny(t *testing.T) {
	request := agent.PermissionRequest{
		ToolName: "apply_patch",
		AvailableDecisions: []agent.PermissionDecisionAction{
			agent.PermissionDecisionAllow,
			agent.PermissionDecisionAllowForSession,
			agent.PermissionDecisionCancel,
		},
	}
	card, _ := renderFocusedPermissionPrompt(request, 2, false, "", 80)
	got := plainRender(t, card)
	if !strings.Contains(got, "tell Zero what to do differently") || !strings.Contains(got, "[n]") {
		t.Fatalf("permission card = %q, missing cancel option", got)
	}
	if strings.Contains(got, "continue without running it") || strings.Contains(got, "[d]") {
		t.Fatalf("apply_patch approval must not show recoverable deny, got %q", got)
	}
}

func TestRequestPermissionsPromptUsesGrantLabelsAndEscDenies(t *testing.T) {
	request := agent.PermissionRequest{
		ToolName: tools.RequestPermissionsToolName,
		Action:   agent.PermissionActionPrompt,
		Reason:   "Need write access.",
		Scope:    "write /tmp/project",
		AvailableDecisions: []agent.PermissionDecisionAction{
			agent.PermissionDecisionAllow,
			agent.PermissionDecisionAllowStrict,
			agent.PermissionDecisionAllowForSession,
			agent.PermissionDecisionDeny,
		},
	}
	card, _ := renderFocusedPermissionPrompt(request, 1, false, "", 96)
	got := plainRender(t, card)
	for _, want := range []string{
		"Grant requested permissions?",
		"permissions: write /tmp/project",
		"Grant for this task",
		"Grant for this task and ask model to review commands",
		"Grant for this session",
		"Continue without permissions",
		"[esc] continue without permissions",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("request_permissions card = %q, missing %q", got, want)
		}
	}

	var resolved agent.PermissionDecision
	m := pendingPermissionModelWithRequest(t, request, func(d agent.PermissionDecision) {
		resolved = d
	})
	updated, _ := m.Update(testKey(tea.KeyEsc))
	m = updated.(model)
	if resolved.Action != agent.PermissionDecisionDeny {
		t.Fatalf("Esc should continue without permissions, got %#v", resolved)
	}
	if m.pendingPermission != nil {
		t.Fatal("request_permissions prompt should clear after Esc")
	}
}

func TestPermissionCursorMovesAndEnterConfirms(t *testing.T) {
	decisions := []permissionDecision{}
	m := pendingPermissionModel(t, func(d agent.PermissionDecision) {
		decisions = append(decisions, permissionDecision(d.Action))
	})
	// 0 -> down 1 -> down 2 -> up 1 (session).
	for _, key := range []rune{tea.KeyDown, tea.KeyDown, tea.KeyUp} {
		updated, _ := m.Update(testKey(key))
		m = updated.(model)
	}
	if m.pendingPermission == nil || m.pendingPermission.cursor != 1 {
		t.Fatalf("cursor after down,down,up = %v, want 1 (session)", m.pendingPermission)
	}
	updated, _ := m.Update(testKey(tea.KeyEnter))
	m = updated.(model)
	if len(decisions) != 1 || decisions[0] != permissionDecisionAllowForSession {
		t.Fatalf("enter on cursor 1 should resolve session allow, got %#v", decisions)
	}
	if m.pendingPermission != nil {
		t.Fatal("prompt should clear after confirm")
	}
}

func TestPermissionCursorWrapsWithUp(t *testing.T) {
	m := pendingPermissionModel(t, func(agent.PermissionDecision) {})
	updated, _ := m.Update(testKey(tea.KeyUp)) // 0 wraps to last (deny)
	m = updated.(model)
	if want := len(permissionOptions(m.pendingPermission.request)) - 1; m.pendingPermission.cursor != want {
		t.Fatalf("Up from 0 should wrap to %d, got %d", want, m.pendingPermission.cursor)
	}
}

func TestPermissionHotkeysStillResolveDirectly(t *testing.T) {
	got := []permissionDecision{}
	m := pendingPermissionModel(t, func(d agent.PermissionDecision) {
		got = append(got, permissionDecision(d.Action))
	})
	if _, cmd := m.Update(testKeyText("d")); cmd != nil { // hotkey ignores the cursor
		t.Fatal("'d' should resolve synchronously")
	}
	if len(got) != 1 || got[0] != permissionDecisionDeny {
		t.Fatalf("'d' should resolve deny directly, got %#v", got)
	}
}

// feedbackRequest is a bash prompt whose decision set includes Cancel, so the
// "tell Zero what to do differently" row (and its [n] hotkey) is present.
func feedbackRequest() agent.PermissionRequest {
	request := testPromptPermissionRequest()
	request.ToolName = "bash"
	request.AvailableDecisions = []agent.PermissionDecisionAction{
		agent.PermissionDecisionAllow,
		agent.PermissionDecisionDeny,
		agent.PermissionDecisionCancel,
	}
	return request
}

// The [n] "tell Zero what to do differently" hotkey no longer resolves cancel
// immediately: it opens the inline feedback field and sends nothing yet.
func TestPermissionTellDifferentlyOpensFeedbackField(t *testing.T) {
	var got []agent.PermissionDecision
	m := pendingPermissionModelWithRequest(t, feedbackRequest(), func(d agent.PermissionDecision) {
		got = append(got, d)
	})
	next, _ := m.Update(testKeyText("n"))
	nm := next.(model)
	if nm.pendingPermission == nil || !nm.pendingPermission.typing {
		t.Fatalf("'n' should open the feedback field, got pending=%#v", nm.pendingPermission)
	}
	if len(got) != 0 {
		t.Fatalf("'n' must not resolve anything yet, got %#v", got)
	}
}

// Typing an instruction and pressing Enter sends a Deny whose Reason is the
// text, so the agent surfaces it to the model as the tool result.
func TestPermissionFeedbackSubmitSendsDenyWithText(t *testing.T) {
	var got []agent.PermissionDecision
	m := pendingPermissionModelWithRequest(t, feedbackRequest(), func(d agent.PermissionDecision) {
		got = append(got, d)
	})
	next, _ := m.Update(testKeyText("n"))
	nm := typeRunes(t, next.(model), "use apply_patch instead")
	after, _ := nm.Update(testKey(tea.KeyEnter))
	am := after.(model)

	if len(got) != 1 {
		t.Fatalf("Enter should resolve exactly one decision, got %#v", got)
	}
	if got[0].Action != agent.PermissionDecisionDeny {
		t.Fatalf("feedback should resolve as Deny, got %s", got[0].Action)
	}
	if got[0].Reason != "use apply_patch instead" {
		t.Fatalf("Deny reason should carry the typed text, got %q", got[0].Reason)
	}
	if am.pendingPermission != nil {
		t.Fatal("prompt should be dismissed after submitting feedback")
	}
}

// Enter with an empty field falls back to a plain cancel (the option's prior
// meaning), so opening the field and changing your mind is safe.
func TestPermissionFeedbackEmptySubmitCancels(t *testing.T) {
	var got []agent.PermissionDecision
	m := pendingPermissionModelWithRequest(t, feedbackRequest(), func(d agent.PermissionDecision) {
		got = append(got, d)
	})
	next, _ := m.Update(testKeyText("n"))
	after, _ := next.(model).Update(testKey(tea.KeyEnter))
	_ = after
	if len(got) != 1 || got[0].Action != agent.PermissionDecisionCancel {
		t.Fatalf("empty feedback should resolve Cancel, got %#v", got)
	}
}

// Esc in the feedback field returns to the option list without resolving.
func TestPermissionFeedbackEscReturnsToOptions(t *testing.T) {
	var got []agent.PermissionDecision
	m := pendingPermissionModelWithRequest(t, feedbackRequest(), func(d agent.PermissionDecision) {
		got = append(got, d)
	})
	next, _ := m.Update(testKeyText("n"))
	after, _ := typeRunes(t, next.(model), "half a thought").Update(testKey(tea.KeyEsc))
	am := after.(model)
	if am.pendingPermission == nil || am.pendingPermission.typing {
		t.Fatalf("Esc should return to the option list, got pending=%#v", am.pendingPermission)
	}
	if len(got) != 0 {
		t.Fatalf("Esc must not resolve anything, got %#v", got)
	}
}

func TestPermissionRenderEmitsHighlightedClickableOffsets(t *testing.T) {
	request := agent.PermissionRequest{ToolName: "write_file", AvailableDecisions: testAllPermissionDecisions()}
	card, offsets := renderFocusedPermissionPrompt(request, 2, false, "", 60) // cursor on future approval
	if len(offsets) != len(permissionOptions(request)) {
		t.Fatalf("offsets = %d, want %d", len(offsets), len(permissionOptions(request)))
	}
	lines := strings.Split(plainRender(t, card), "\n")
	future := offsets[2]
	if future < 0 || future >= len(lines) || !strings.Contains(lines[future], "always") {
		t.Fatalf("offset[2] (%d) should point at the future line; lines=%#v", future, lines)
	}
	if !strings.Contains(lines[future], "▸") {
		t.Fatalf("the highlighted (cursor) option line should carry ▸, got %q", lines[future])
	}
}

func TestPermissionRenderShowsNetworkTargetAndHostScopedAlways(t *testing.T) {
	request := agent.PermissionRequest{
		ToolName:           "web_fetch",
		SideEffect:         "network",
		Scope:              "example.com",
		AvailableDecisions: testAllPermissionDecisions(),
	}
	card, _ := renderFocusedPermissionPrompt(request, 1, false, "", 72)
	got := plainRender(t, card)
	for _, want := range []string{"target: example.com", "allow this host for this conversation", "[s]", "allow this host in the future", "[y]"} {
		if !strings.Contains(got, want) {
			t.Fatalf("permission card = %q, missing %q", got, want)
		}
	}
	if strings.Contains(got, "scope: example.com") {
		t.Fatalf("network prompt should render target label, got %q", got)
	}
}

// TestPermissionCursorCtrlU ensures Ctrl+U moves the permission cursor UP
// (toward the first option). Regression: the original implementation moved
// the cursor DOWN on Ctrl+U and UP on Ctrl+D.
func TestPermissionCursorCtrlU(t *testing.T) {
	m := pendingPermissionModel(t, func(agent.PermissionDecision) {})
	m.pendingPermission.cursor = 2 // middle option

	updated, _ := m.Update(testKeyCtrl('u'))
	next := updated.(model)
	if next.pendingPermission.cursor != 1 {
		t.Fatalf("Ctrl+U moved cursor to %d, want 1 (one step up)", next.pendingPermission.cursor)
	}
}

// TestPermissionCursorCtrlD ensures Ctrl+D moves the permission cursor DOWN
// (toward the last option). Regression: the original implementation moved
// the cursor UP on Ctrl+D and DOWN on Ctrl+U.
func TestPermissionCursorCtrlD(t *testing.T) {
	m := pendingPermissionModel(t, func(agent.PermissionDecision) {})
	m.pendingPermission.cursor = 0 // first option

	updated, _ := m.Update(testKeyCtrl('d'))
	next := updated.(model)
	if next.pendingPermission.cursor != 1 {
		t.Fatalf("Ctrl+D moved cursor to %d, want 1 (one step down)", next.pendingPermission.cursor)
	}
}

// TestShiftUpComposerGuard ensures Shift+Up does NOT scroll the transcript
// when the composer has text, so it falls through to the input's own line
// navigation.
func TestShiftUpComposerGuard(t *testing.T) {
	m := mouseTestModel()
	// Add enough transcript rows so scrolling is valid.
	for i := 0; i < 20; i++ {
		m.transcript = appendRow(m.transcript, rowAssistant, "line")
	}
	m.input.SetValue("some text")
	m.chatScrollOffset = 5

	updated, cmd := m.Update(testKeyShift(tea.KeyUp))
	next := updated.(model)
	_ = cmd
	if got := next.chatScrollOffset; got != 5 {
		t.Fatalf("Shift+Up with non-empty composer scrolled offset to %d, want 5 (unchanged)", got)
	}
}

// TestShiftDownComposerGuard ensures Shift+Down does NOT scroll the transcript
// when the composer has text, falling through to the input's navigation.
func TestShiftDownComposerGuard(t *testing.T) {
	m := mouseTestModel()
	// Add enough transcript rows so scrolling is valid.
	for i := 0; i < 20; i++ {
		m.transcript = appendRow(m.transcript, rowAssistant, "line")
	}
	m.input.SetValue("some text")
	m.chatScrollOffset = 3

	updated, cmd := m.Update(testKeyShift(tea.KeyDown))
	next := updated.(model)
	_ = cmd
	if got := next.chatScrollOffset; got != 3 {
		t.Fatalf("Shift+Down with non-empty composer scrolled offset to %d, want 3 (unchanged)", got)
	}
}

// The highlighted permission option must use the selected-row tint, not the
// brand chip. zeroTheme.badge is the accent-filled chip for short labels
// (" 0 ", " ASK ", " SPEC REVIEW "); using it for a whole row painted a
// full-brightness accent slab across a card whose palette is deliberately amber
// (warning), and skipped the card tint every other line composes onto. selBg is
// the tint tuned for a highlighted row against a panel, and onSel is what every
// other selectable list in the TUI uses.
func TestFocusedPermissionSelectedRowUsesSelectionTintNotBrandChip(t *testing.T) {
	request := agent.PermissionRequest{ToolName: "exec_command", SideEffect: "shell"}
	card, _ := renderFocusedPermissionPrompt(request, 0, false, "", 70)

	var selected string
	for _, line := range strings.Split(card, "\n") {
		if strings.Contains(ansiPattern.ReplaceAllString(line, ""), "▸ ") {
			selected = line
			break
		}
	}
	if selected == "" {
		t.Fatal("no highlighted option row rendered")
	}

	accentBg := backgroundCode(darkPalette.accent)
	selBg := backgroundCode(darkPalette.selBg)
	if strings.Contains(selected, accentBg) {
		t.Errorf("selected row is filled with the brand accent %s (zeroTheme.badge); want the selection tint:\n%q", darkPalette.accent, selected)
	}
	if !strings.Contains(selected, selBg) {
		t.Errorf("selected row should carry the selection tint %s:\n%q", darkPalette.selBg, selected)
	}

	// The PERMISSION chip keeps its amber fill — the card must still read as a
	// warning surface, so this fix must not flatten it.
	if !strings.Contains(card, backgroundCode(darkPalette.amber)) {
		t.Errorf("PERMISSION badge lost its amber fill:\n%q", card)
	}

	// The card BODY carries no warm permBg wash any more: it matches the other
	// prompt cards (ask_user, spec) whose bodies are transparent. Warning identity
	// comes from the amber badge + border, not a full-body tint that clashes on
	// cool themes.
	if strings.Contains(card, backgroundCode(darkPalette.permBg)) {
		t.Errorf("permission card body still tinted with permBg %s; want a transparent body:\n%q", darkPalette.permBg, card)
	}
}

// backgroundCode renders the SGR truecolor background sequence for a #rrggbb
// palette entry, so assertions compare against the palette rather than
// hardcoded numbers that drift when a theme is retuned.
func backgroundCode(hex string) string {
	var r, g, b int
	fmt.Sscanf(hex, "#%02x%02x%02x", &r, &g, &b)
	return fmt.Sprintf("48;2;%d;%d;%d", r, g, b)
}

// A focused permission prompt suppresses the composer: keys drive the card, so a
// "describe a task for zero…" box below it is inert and misleading — and once the
// feedback field is open, the shared input must not echo in both places.
func TestComposerSuppressedDuringPermissionPrompt(t *testing.T) {
	m := pendingPermissionModelWithRequest(t, feedbackRequest(), func(agent.PermissionDecision) {})
	m.width, m.height = 96, 30

	view := plainRender(t, m.View())
	if strings.Contains(view, "describe a task for zero") {
		t.Errorf("composer must be hidden while a permission prompt is focused:\n%s", view)
	}

	// While typing feedback, the text lives on the card, not in a second box.
	next, _ := m.Update(testKeyText("n"))
	typed := typeRunes(t, next.(model), "use apply_patch")
	tv := plainRender(t, typed.View())
	if strings.Contains(tv, "describe a task for zero") {
		t.Errorf("composer must stay hidden in feedback mode:\n%s", tv)
	}
	if strings.Count(tv, "use apply_patch") != 1 {
		t.Errorf("feedback text should appear exactly once (on the card), got %d occurrences:\n%s", strings.Count(tv, "use apply_patch"), tv)
	}
}

// Entering feedback mode must not lose a composer draft (a half-typed or queued
// next-turn message). The draft is restored whether feedback is submitted or
// cancelled; only the feedback text (delivered via the decision Reason) is
// consumed.
func TestPermissionFeedbackPreservesComposerDraftOnSubmit(t *testing.T) {
	var got []agent.PermissionDecision
	m := pendingPermissionModelWithRequest(t, feedbackRequest(), func(d agent.PermissionDecision) {
		got = append(got, d)
	})
	m.input.SetValue("my queued next message")

	next, _ := m.Update(testKeyText("n"))
	nm := next.(model)
	if nm.input.Value() != "" {
		t.Fatalf("feedback field should start empty, got %q", nm.input.Value())
	}
	nm = typeRunes(t, nm, "use apply_patch")
	after, _ := nm.Update(testKey(tea.KeyEnter))
	am := after.(model)

	if len(got) != 1 {
		t.Fatalf("expected exactly one permission decision, got %#v", got)
	}
	if got[0].Reason != "use apply_patch" {
		t.Fatalf("feedback text lost: %q", got[0].Reason)
	}
	if am.input.Value() != "my queued next message" {
		t.Fatalf("composer draft not restored after submit, got %q", am.input.Value())
	}
}

func TestPermissionFeedbackPreservesComposerDraftOnCancel(t *testing.T) {
	m := pendingPermissionModelWithRequest(t, feedbackRequest(), func(agent.PermissionDecision) {})
	m.input.SetValue("my queued next message")

	next, _ := m.Update(testKeyText("n"))
	nm := typeRunes(t, next.(model), "half a thought")
	after, _ := nm.Update(testKey(tea.KeyEsc))
	am := after.(model)

	if am.pendingPermission == nil || am.pendingPermission.typing {
		t.Fatal("Esc should return to the option list")
	}
	if am.input.Value() != "my queued next message" {
		t.Fatalf("composer draft not restored after cancel, got %q", am.input.Value())
	}
}

// Backspace on an empty permission feedback field must edit the feedback text,
// never drop a staged attachment. The keyBackspace case runs before the typing
// branch and, on an empty composer, would otherwise remove the last staged
// image/doc — which savedDraft does not restore.
func TestPermissionFeedbackBackspaceKeepsStagedAttachment(t *testing.T) {
	m := pendingPermissionModelWithRequest(t, feedbackRequest(), func(agent.PermissionDecision) {})
	m.pendingImages = []zeroruntime.ImageBlock{{MediaType: "image/png"}}
	m.pendingImageLabels = []string{"diagram.png"}

	next, _ := m.Update(testKeyText("n")) // open feedback field (clears composer text)
	after, _ := next.(model).Update(testKey(tea.KeyBackspace))
	am := after.(model)

	if len(am.pendingImageLabels) != 1 || len(am.pendingImages) != 1 {
		t.Fatalf("Backspace in feedback mode dropped the staged attachment: labels=%v imgs=%d", am.pendingImageLabels, len(am.pendingImages))
	}
	if !am.pendingPermission.typing {
		t.Fatal("Backspace should stay in the feedback field, not exit it")
	}
}

// No permission option row is clickable while the feedback field is open: the
// renderer registers zero clickable offsets in typing mode, so a stray click
// (Allow included) has no row to land on. This is the primary safety; the
// !typing guard on the click resolver in handleTranscriptSelectionMouse is the
// explicit second layer for if this early-return is ever refactored away.
func TestPermissionFeedbackRendersNoClickableOptionsWhileTyping(t *testing.T) {
	request := feedbackRequest()

	// Option mode: rows are clickable.
	_, optOffsets := renderFocusedPermissionPrompt(request, 0, false, "", 80)
	if len(optOffsets) == 0 {
		t.Fatal("precondition: option rows should be clickable in option mode")
	}
	// Feedback mode: no clickable rows.
	_, typingOffsets := renderFocusedPermissionPrompt(request, 0, true, "some feedback", 80)
	if typingOffsets != nil {
		t.Fatalf("feedback mode must register no clickable option offsets, got %#v", typingOffsets)
	}
}
