package tui

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/Gitlawb/zero/internal/agent"
	"github.com/Gitlawb/zero/internal/planmode"
	"github.com/Gitlawb/zero/internal/tools"
	"github.com/Gitlawb/zero/internal/zeroruntime"
)

func TestPlanPublicationFailureKeepsConsumersAligned(t *testing.T) {
	for _, failure := range []string{"conflict", "lock", "initial-read", "unreadable", "success", "clear"} {
		for _, delivery := range []string{"live", "BTW", "completion"} {
			hidden := delivery == "BTW"
			name := failure + "-" + delivery
			t.Run(name, func(t *testing.T) {
				m := newPlanCommandTestModel(t, t.TempDir(), agent.PermissionModePlan)
				m.activeSession.SessionID = ""
				var err error
				m, err = m.ensureActiveSession("plan publication")
				if err != nil {
					t.Fatal(err)
				}
				initial := []tools.PlanItem{{ID: "1", Content: "original", Status: "pending"}}
				tool, _ := m.registry.Get("update_plan")
				shared := tool.(interface {
					currentPlanReader
					planFileReloader
				})
				shared.SetPlan(initial)
				m.plan.updateFromItems(initial, m.now())
				path, err := planmode.WritePlan(m.cwd, m.activeSession.SessionID, formatPlanItems(initial))
				if err != nil {
					t.Fatal(err)
				}
				if failure == "initial-read" || failure == "unreadable" {
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
					if err := os.Mkdir(path, 0700); err != nil {
						t.Fatal(err)
					}
				}
				want := initial
				if failure == "conflict" {
					want = []tools.PlanItem{{ID: "1", Content: "accepted elsewhere", Status: "completed"}}
				}
				args := `{"plan":[{"content":"candidate update","status":"pending"}]}`
				if failure == "success" {
					want = []tools.PlanItem{{ID: "1", Content: "candidate update", Status: "pending"}}
				}
				if failure == "clear" {
					args = `{"plan":[]}`
					want = nil
				}
				provider := &scriptedProvider{scripts: [][]zeroruntime.StreamEvent{
					{
						{Type: zeroruntime.StreamEventToolCallStart, ToolCallID: "plan", ToolName: "update_plan"},
						{Type: zeroruntime.StreamEventToolCallDelta, ToolCallID: "plan", ArgumentsFragment: args},
						{Type: zeroruntime.StreamEventToolCallEnd, ToolCallID: "plan"},
						{Type: zeroruntime.StreamEventDone},
					},
					{{Type: zeroruntime.StreamEventText, Content: "done"}, {Type: zeroruntime.StreamEventDone}},
				}}
				provider.beforeCall = func(index int) {
					if index != 0 {
						return
					}
					switch failure {
					case "conflict":
						_, err = planmode.WritePlan(m.cwd, m.activeSession.SessionID, formatPlanItems(want))
					case "lock":
						base := filepath.Dir(filepath.Dir(path))
						lock := filepath.Join(base, filepath.Base(filepath.Dir(path))+"-"+filepath.Base(path)+".lock")
						err = os.Remove(lock)
						if err == nil {
							err = os.Mkdir(lock, 0700)
						}
					case "initial-read":
						err = os.Remove(path)
						if err == nil {
							_, err = planmode.WritePlan(m.cwd, m.activeSession.SessionID, formatPlanItems(initial))
						}
					}
					if err != nil {
						t.Fatal(err)
					}
				}
				m.provider = provider
				var messages []tea.Msg
				m.runtimeMessageSink = func(msg tea.Msg) { messages = append(messages, msg) }
				if delivery == "completion" {
					m.runtimeMessageSink = nil
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				m = m.beginRun(cancel)
				cmd := m.runAgent(m.activeRunID, ctx, "update the plan", nil)
				visible := m
				sideItems := []tools.PlanItem{{ID: "1", Content: "side plan", Status: "pending"}}
				if hidden {
					visible, _ = m.handleBTWCommand("")
					if !visible.btw.active {
						t.Fatal("BTW did not open")
					}
					shared.SetPlan(sideItems)
					visible.plan.updateFromItems(sideItems, visible.now())
				}
				response := cmd()
				for _, msg := range append(messages, response) {
					updated, _ := visible.Update(msg)
					visible = updated.(model)
				}
				if hidden {
					if !planItemsEqual(shared.CurrentPlan(), sideItems) {
						t.Errorf("parent publication changed the side tool: %#v", shared.CurrentPlan())
					}
					if !planItemsEqual(visible.btw.parentPlanItems, want) {
						t.Errorf("captured parent plan = %#v, want %#v", visible.btw.parentPlanItems, want)
					}
					visible, _ = visible.leaveBTW()
				}
				if !planItemsEqual(shared.CurrentPlan(), want) {
					t.Errorf("tool accepted rejected publication: %#v, want %#v", shared.CurrentPlan(), want)
				}
				if len(visible.plan.steps) != len(want) {
					t.Errorf("panel accepted rejected publication: %#v", visible.plan.steps)
				}
				for i, step := range visible.plan.steps {
					if i < len(want) && (step.content != want[i].Content || step.status != want[i].Status || step.notes != want[i].Notes) {
						t.Errorf("panel step = %#v, want %#v", step, want[i])
					}
				}
				if !strings.HasSuffix(visible.planText(), formatPlanItems(want)) {
					t.Errorf("status differs from accepted plan: %q", visible.planText())
				}
				content, _, err := planmode.ReadPlan(m.cwd, m.activeSession.SessionID)
				if failure == "unreadable" {
					if err == nil || !strings.Contains(visible.planText(), "plan file read error:") {
						t.Error("unreadable storage was presented as a successful durable read")
					}
				} else if err != nil || !planItemsEqual(parsePlanFileLines(content), want) {
					t.Errorf("durable plan = %q, %v", content, err)
				}
				var rejected bool
				for _, request := range provider.requests {
					for _, message := range request.Messages {
						if message.ToolCallID == "plan" && message.IsError {
							rejected = true
							stage := "write"
							if failure == "initial-read" || failure == "unreadable" {
								stage = "read"
							}
							if !strings.HasPrefix(message.Content, "plan file "+stage+" error:") {
								t.Errorf("publication failure reported the wrong operation: %q, want %s error", message.Content, stage)
							}
						}
					}
				}
				wantRejected := failure != "success" && failure != "clear"
				if rejected != wantRejected {
					t.Errorf("model error result = %v, want %v for durable publication", rejected, wantRejected)
				}
			})
		}
	}
}

func TestPlanPublicationRetriesFromAcceptedConflictValue(t *testing.T) {
	m := newPlanCommandTestModel(t, t.TempDir(), agent.PermissionModePlan)
	if _, err := planmode.WritePlan(m.cwd, m.activeSession.SessionID, "1. [pending] original"); err != nil {
		t.Fatal(err)
	}
	p := newPlanPublication(m.cwd, m.activeSession.SessionID, nil)
	if _, err := planmode.WritePlan(m.cwd, m.activeSession.SessionID, "1. [completed] external"); err != nil {
		t.Fatal(err)
	}
	args := map[string]any{"plan": []any{map[string]any{"content": "retry", "status": "pending"}}}
	if result := p.Run(context.Background(), args); result.Status != tools.StatusError {
		t.Fatalf("conflicting update = %#v", result)
	}
	if got := p.CurrentPlan(); len(got) != 1 || got[0].Content != "external" {
		t.Fatalf("run accepted rejected candidate: %#v", got)
	}
	if result := p.Run(context.Background(), args); result.Status != tools.StatusOK {
		t.Fatalf("retry against accepted baseline = %#v", result)
	}
	content, _, err := planmode.ReadPlan(m.cwd, m.activeSession.SessionID)
	if err != nil || !planItemsEqual(parsePlanFileLines(content), p.CurrentPlan()) {
		t.Fatalf("retry left different durable and run plans: %q, %v", content, err)
	}
}

func TestCancelledParentPlanMessageCannotReplaceBTWSnapshot(t *testing.T) {
	m := newPlanCommandTestModel(t, t.TempDir(), agent.PermissionModePlan)
	m.activeSession.SessionID = ""
	var err error
	m, err = m.ensureActiveSession("parent cancellation")
	if err != nil {
		t.Fatal(err)
	}
	tool, _ := m.registry.Get("update_plan")
	accepted := []tools.PlanItem{{Content: "accepted", Status: "pending"}}
	tool.(planFileReloader).SetPlan(accepted)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m = m.beginRun(cancel)
	runID := m.activeRunID
	side, _ := m.handleBTWCommand("")
	side.btw.parent.cancelRun()
	if ctx.Err() == nil {
		t.Fatal("parent was not cancelled")
	}
	updated, _ := side.Update(planUpdateMsg{runID: runID, items: []tools.PlanItem{{Content: "stale"}}, syncTool: true})
	side = updated.(model)
	if !planItemsEqual(side.btw.parentPlanItems, accepted) {
		t.Fatalf("cancelled parent replaced accepted BTW snapshot: %#v", side.btw.parentPlanItems)
	}
	if len(tool.(currentPlanReader).CurrentPlan()) != 0 {
		t.Fatal("cancelled parent changed the side tool")
	}
}
