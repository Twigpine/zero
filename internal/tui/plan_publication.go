package tui

import (
	"context"

	"github.com/Gitlawb/zero/internal/planmode"
	"github.com/Gitlawb/zero/internal/tools"
)

// planPublication is private to one TUI run. A candidate must reach durable
// storage before the tool reports success or the live session sees it. Keeping
// candidates here also prevents a hidden parent from changing the BTW tool.
type planPublication struct {
	tools.Tool
	workspace   string
	sessionID   string
	baseline    string
	baselineErr error
	accepted    []tools.PlanItem
	updated     bool
}

func newPlanPublication(workspace, sessionID string, initial []tools.PlanItem) *planPublication {
	baseline, exists, err := planmode.ReadPlan(workspace, sessionID)
	if err == nil && exists {
		initial = tools.CanonicalizePlanItems(parsePlanFileLines(baseline))
	}
	return &planPublication{
		Tool: tools.NewUpdatePlanTool(), workspace: workspace, sessionID: sessionID,
		baseline: baseline, baselineErr: err, accepted: append([]tools.PlanItem{}, initial...),
	}
}

func (p *planPublication) CurrentPlan() []tools.PlanItem {
	return append([]tools.PlanItem{}, p.accepted...)
}

func (p *planPublication) Run(ctx context.Context, args map[string]any) tools.Result {
	result := p.Tool.Run(ctx, args)
	if result.Status != tools.StatusOK {
		return result
	}
	content := formatPlanFile(result.PlanSnapshot)
	err := p.baselineErr
	stage := "read"
	if err == nil && p.sessionID != "" {
		stage = "write"
		_, err = planmode.WritePlanIfUnchanged(ctx, p.workspace, p.sessionID, content, p.baseline)
	}
	p.updated = true
	if err != nil {
		// Another writer may have won the baseline comparison. Hydrate its
		// accepted value, never the rejected candidate. If storage is unreadable,
		// retain the last accepted snapshot and surface the failure. A cancelled
		// run must not hydrate a later session or editor's state.
		if ctx.Err() == nil {
			if current, _, readErr := planmode.ReadPlan(p.workspace, p.sessionID); readErr == nil {
				p.accepted = tools.CanonicalizePlanItems(parsePlanFileLines(current))
				p.baseline, p.baselineErr = current, nil
			}
		}
		return tools.Result{Status: tools.StatusError, Output: "plan file " + stage + " error: " + err.Error() + "; update not accepted"}
	}
	p.accepted = append([]tools.PlanItem{}, result.PlanSnapshot...)
	p.baseline = content
	return result
}

func (p *planPublication) update(runID int) *planUpdateMsg {
	if p == nil || !p.updated {
		return nil
	}
	return &planUpdateMsg{runID: runID, items: p.CurrentPlan(), syncTool: true}
}

// applyPlanUpdate runs only on the TUI goroutine after the run-ID check. Hidden
// parent messages omit syncTool: the shared tool belongs to the visible side
// session until leaveBTW restores the parent's accepted durable snapshot.
func (m *model) applyPlanUpdate(msg planUpdateMsg) {
	if msg.syncTool {
		if tool, ok := m.registry.Get("update_plan"); ok {
			if reloader, ok := tool.(planFileReloader); ok {
				reloader.SetPlan(msg.items)
			}
		}
	}
	m.plan.updateFromItems(msg.items, m.now())
}
