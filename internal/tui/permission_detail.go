package tui

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Twigpine/zero/internal/agent"
	"github.com/Twigpine/zero/internal/specialist"
)

// What a permission prompt SHOWS about the thing it is approving.
//
// PermissionRequest has carried an Args map since it was written and no renderer
// has ever read it — verified across permission_prompt.go, transcript.go and
// rendering.go. So the card can name a tool and state a static reason, and
// nothing more. That is survivable for a one-file write, where the path is
// already surfaced as the grant scope, and it is not survivable for a plan: a
// twenty-task plan and a one-task plan produce the identical card.
//
// This is STEP 1 OF THREE. On its own it renders a plan into a decision surface
// that does not yet gate plans — orchestrate returns PermissionAllow, because
// today its tasks are read-only and a prompt that adds friction without adding
// safety trains click-through. Step 2 is worktree isolation; step 3 flips plan
// tasks to allow write tools with both as preconditions. The order is forced:
// an approval gate that cannot show the plan is the thing plan_tool.go argues
// against, so the renderer has to exist before the gate can be justified.
//
// UNTRUSTED INPUT. Every value here came from the model. It is truncated on
// RUNE boundaries, stripped of anything that could move the cursor, and bounded
// in row count — a card that a plan can make taller than the terminal is a card
// an attacker can use to push the options off screen.

// permissionDetailRenderer turns a tool's arguments into card lines.
type permissionDetailRenderer func(args map[string]any, width int) []string

// permissionDetailRenderers is keyed by tool name. A tool with no entry renders
// EXACTLY as before — that is what keeps every existing prompt byte-identical,
// and it is asserted rather than assumed.
var permissionDetailRenderers = map[string]permissionDetailRenderer{
	"orchestrate": planPermissionDetail,
}

// permissionDetailLines returns the tool-specific detail for a request, or
// nothing when the tool has no renderer.
func permissionDetailLines(request agent.PermissionRequest, width int) []string {
	render := permissionDetailRenderers[strings.TrimSpace(request.ToolName)]
	if render == nil || len(request.Args) == 0 {
		return nil
	}
	return render(request.Args, width)
}

// permissionDetailMaxRows bounds the detail. The options must stay on screen:
// a prompt whose choices have been pushed past the bottom is a prompt that can
// only be answered by the one key still visible.
const permissionDetailMaxRows = 12

// planPermissionDetail renders an orchestrate plan: what it will run, in what
// order, and what it may spend.
func planPermissionDetail(args map[string]any, width int) []string {
	room := maxInt(16, width-4)

	if saved := permissionString(args, "saved"); saved != "" {
		// A saved plan's tasks are on disk, not in the arguments — there is
		// nothing here to render, and inventing a summary would describe a plan
		// this card never saw.
		head := "saved plan: " + saved
		if specialist.PlanRunsInBackground(args) {
			head += " · background"
		}
		return []string{
			"  " + zeroTheme.muted.Render(truncateRunes(head, room)),
			"  " + zeroTheme.faint.Render(truncateRunes("run /plans show "+saved+" to read it", room)),
		}
	}

	rawTasks, _ := args["tasks"].([]any)
	lines := []string{}

	head := fmt.Sprintf("%d task(s)", len(rawTasks))
	if name := permissionString(args, "name"); name != "" {
		head = name + " · " + head
	}
	if budget := planBudgetSummary(args); budget != "" {
		head += " · " + budget
	}
	lines = append(lines, "  "+zeroTheme.ink.Render(truncateRunes(head, room)))

	if description := permissionString(args, "description"); description != "" {
		lines = append(lines, "  "+zeroTheme.muted.Render(truncateRunes(description, room)))
	}

	shown := 0
	for _, raw := range rawTasks {
		if shown >= permissionDetailMaxRows {
			break
		}
		task, ok := raw.(map[string]any)
		if !ok {
			// A malformed entry is SHOWN as malformed, not skipped. A task the
			// card silently drops is a task the user approved without seeing.
			lines = append(lines, "  "+zeroTheme.faint.Render("· (unreadable task entry)"))
			shown++
			continue
		}
		lines = append(lines, "  "+zeroTheme.faint.Render(truncateRunes(planTaskDetailLine(task), room)))
		shown++
	}
	if hidden := len(rawTasks) - shown; hidden > 0 {
		lines = append(lines, "  "+zeroTheme.faint.Render(fmt.Sprintf("… and %d more not shown", hidden)))
	}
	return lines
}

// planTaskDetailLine renders one task: its id, what it waits on, and the first
// line of its prompt.
func planTaskDetailLine(task map[string]any) string {
	id := permissionString(task, "id")
	if id == "" {
		id = "(no id)"
	}
	line := "· " + id
	if deps := permissionStrings(task, "depends_on"); len(deps) > 0 {
		sort.Strings(deps)
		line += " after " + strings.Join(deps, ",")
	}
	// The task's TOOLS are shown when it names any, because "which tools" is
	// half of what an approval is deciding.
	if granted := permissionStrings(task, "tools"); len(granted) > 0 {
		sort.Strings(granted)
		line += " [" + strings.Join(granted, " ") + "]"
	}
	if prompt := permissionString(task, "prompt"); prompt != "" {
		line += " — " + prompt
	}
	return line
}

// planBudgetSummary renders the bounds a plan asked for, naming the unbounded
// case rather than omitting it: "no token limit" is the thing worth seeing.
//
// DECODED BY THE PLAN'S OWN DECODER. This card used to read the raw arguments
// itself, accepting only JSON numbers, while admission also accepts a numeric
// string — so a plan asking for "500000" tokens was shown as having no token
// limit, and the user approved a plan described differently from the one that
// ran. A budget admission will refuse is shown as refused, with the reason.
func planBudgetSummary(args map[string]any) string {
	parts := []string{}
	if _, ok := args["budget"].(map[string]any); ok {
		budget, err := specialist.DecodePlanBudget(args)
		if err != nil {
			parts = append(parts, "invalid budget: "+sanitizeCardText(err.Error()))
		} else {
			if budget.MaxTokens > 0 {
				parts = append(parts, fmt.Sprintf("%d tokens", budget.MaxTokens))
			} else {
				parts = append(parts, "no token limit")
			}
			if budget.MaxWall > 0 {
				parts = append(parts, fmt.Sprintf("%ds wall", int(budget.MaxWall/time.Second)))
			}
		}
	}
	// A TOP-LEVEL directive, as the tool reads it. It was looked up inside the
	// budget object, where no call ever puts it, so the card never said a plan
	// would keep running after the turn ended.
	if specialist.PlanRunsInBackground(args) {
		parts = append(parts, "background")
	}
	return strings.Join(parts, ", ")
}

// permissionString reads a string argument and makes it SAFE TO DRAW: one line,
// no control characters, no escape sequences. The value came from the model, and
// a card is a place where a stray \r or an ANSI reset rewrites the screen around
// it.
func permissionString(args map[string]any, key string) string {
	raw, _ := args[key].(string)
	return sanitizeCardText(raw)
}

// sanitizeCardText collapses a value to a single printable line.
func sanitizeCardText(raw string) string {
	if raw == "" {
		return ""
	}
	if index := strings.IndexAny(raw, "\r\n"); index >= 0 {
		raw = raw[:index]
	}
	var out strings.Builder
	for _, r := range raw {
		switch {
		case r == '\t':
			out.WriteRune(' ')
		case r < 0x20 || r == 0x7f:
			// Control characters, including ESC — the one that starts an ANSI
			// sequence. Dropped rather than escaped: nothing here needs them.
			continue
		default:
			out.WriteRune(r)
		}
	}
	return strings.TrimSpace(out.String())
}

func permissionStrings(args map[string]any, key string) []string {
	raw, _ := args[key].([]any)
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		if text, ok := item.(string); ok {
			if clean := sanitizeCardText(text); clean != "" {
				out = append(out, clean)
			}
		}
	}
	return out
}
