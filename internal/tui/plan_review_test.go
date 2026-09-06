package tui

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/Gitlawb/zero/internal/agent"
	"github.com/Gitlawb/zero/internal/planmode"
	"github.com/Gitlawb/zero/internal/tools"
	"github.com/Gitlawb/zero/internal/zeroruntime"
)

func TestEditorCompletionPreservesRejectedWork(t *testing.T) {
	for _, kind := range []string{"conflict", "baseline", "storage"} {
		t.Run(kind, func(t *testing.T) {
			m := newPlanCommandTestModel(t, t.TempDir(), agent.PermissionModePlan)
			durable, err := planmode.WritePlan(m.cwd, m.activeSession.SessionID, "original")
			if err != nil {
				t.Fatal(err)
			}
			staged, finish, err := planmode.StageEditor(m.cwd, m.activeSession.SessionID)
			if err != nil {
				t.Fatal(err)
			}
			defer finish(false)
			if err := os.WriteFile(staged, []byte("saved user work"), 0600); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "conflict", "baseline":
				if _, err := planmode.WritePlan(m.cwd, m.activeSession.SessionID, "newer"); err != nil {
					t.Fatal(err)
				}
				if kind == "baseline" {
					if err := os.Remove(staged + ".basehash"); err != nil {
						t.Fatal(err)
					}
				}
			case "storage":
				if err := os.Remove(durable); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(durable, 0700); err != nil {
					t.Fatal(err)
				}
			}
			msg := finishPlanEditor(m.cwd, m.activeSession.SessionID, staged, finish, nil)
			if msg.err == nil {
				t.Fatal("expected rejected write-back")
			}
			recovery := filepath.Join(filepath.Dir(staged), "zero-recovered-"+strings.TrimPrefix(filepath.Base(staged), planmode.StagedPlanFilePrefix))
			if !strings.Contains(msg.err.Error(), recovery) {
				t.Fatalf("missing recovery location: %v", msg.err)
			}
			data, err := os.ReadFile(recovery)
			if err != nil || string(data) != "saved user work" {
				t.Fatalf("recovery = %q, %v", data, err)
			}
			for _, suffix := range []string{"", ".lock", ".basehash"} {
				if _, err := os.Stat(staged + suffix); !os.IsNotExist(err) {
					t.Fatalf("staging resource retained: %s, %v", suffix, err)
				}
			}
			if kind != "storage" {
				got, _, err := planmode.ReadPlan(m.cwd, m.activeSession.SessionID)
				if err != nil || got != "newer\n" {
					t.Fatalf("newer durable plan lost: %q, %v", got, err)
				}
				old := time.Now().Add(-24 * time.Hour)
				if err := os.Chtimes(recovery, old, old); err != nil {
					t.Fatal(err)
				}
				_, cleanup, err := planmode.StageForEditor(m.cwd, m.activeSession.SessionID)
				if err != nil {
					t.Fatal(err)
				}
				cleanup()
				if _, err := os.Stat(recovery); err != nil {
					t.Fatalf("later sweep removed recovery: %v", err)
				}
			}
			updated, _ := m.Update(msg)
			if !transcriptContains(updated.(model).transcript, recovery) {
				t.Fatal("recovery path was not surfaced")
			}
		})
	}
}

func TestEditorCompletionCarriesAuthorshipAndCanonicalStatus(t *testing.T) {
	for _, kind := range []string{"unchanged", "external", "edit", "clear"} {
		t.Run(kind, func(t *testing.T) {
			m := newPlanCommandTestModel(t, t.TempDir(), agent.PermissionModePlan)
			var err error
			m.activeSession.SessionID = ""
			m, err = m.ensureActiveSession("editor outcomes")
			if err != nil {
				t.Fatal(err)
			}
			tool, _ := m.registry.Get("update_plan")
			planTool := tool.(interface {
				currentPlanReader
				planFileReloader
			})
			before := []tools.PlanItem{{Content: "first\r\nsecond", Status: "in_progress", Notes: "note\r\ncontinued"}}
			planTool.SetPlan(before)
			before = planTool.CurrentPlan()
			if _, err := planmode.WritePlan(m.cwd, m.activeSession.SessionID, formatPlanItems(before)); err != nil {
				t.Fatal(err)
			}
			staged, finish, err := planmode.StageEditor(m.cwd, m.activeSession.SessionID)
			if err != nil {
				t.Fatal(err)
			}
			defer finish(false)
			switch kind {
			case "external":
				_, err = planmode.WritePlan(m.cwd, m.activeSession.SessionID, "1. [done] newer")
			case "edit":
				err = os.WriteFile(staged, []byte("1. [in_progress] first edit\n2. [in_progress] second edit\n   Notes: keep\n3. [done] finished"), 0600)
			case "clear":
				err = os.WriteFile(staged, nil, 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			count := len(m.sessionEvents)
			msg := finishPlanEditor(m.cwd, m.activeSession.SessionID, staged, finish, nil)
			if msg.err != nil {
				t.Fatal(msg.err)
			}
			updated, _ := m.Update(msg)
			next := updated.(model)
			wantEvents := 0
			if kind == "edit" || kind == "clear" {
				wantEvents = 1
			}
			if got := len(next.sessionEvents) - count; got != wantEvents {
				t.Fatalf("authored events = %d, want %d", got, wantEvents)
			}
			if kind == "unchanged" && !reflect.DeepEqual(planTool.CurrentPlan(), before) {
				t.Fatalf("no-op changed accepted plan: %#v", planTool.CurrentPlan())
			}
			if kind == "edit" || kind == "external" {
				canonical := formatPlanItems(planTool.CurrentPlan())
				if !strings.HasSuffix(next.planText(), canonical) {
					t.Fatalf("status disagrees with plan: %q, want %q", next.planText(), canonical)
				}
			}
			if _, err := os.Stat(staged); !os.IsNotExist(err) {
				t.Fatalf("successful editor leaked staging: %v", err)
			}
		})
	}
}

func TestCancelledPlanPublicationCannotOverwriteEditor(t *testing.T) {
	m := newPlanCommandTestModel(t, t.TempDir(), agent.PermissionModePlan)
	m.activeSession.SessionID = ""
	var err error
	m, err = m.ensureActiveSession("delayed plan publication")
	if err != nil {
		t.Fatal(err)
	}
	m.provider = &scriptedProvider{scripts: [][]zeroruntime.StreamEvent{{
		{Type: zeroruntime.StreamEventToolCallStart, ToolCallID: "plan", ToolName: "update_plan"},
		{Type: zeroruntime.StreamEventToolCallDelta, ToolCallID: "plan", ArgumentsFragment: `{"plan":[{"content":"obsolete","status":"pending"}]}`},
		{Type: zeroruntime.StreamEventToolCallEnd, ToolCallID: "plan"},
		{Type: zeroruntime.StreamEventDone},
	}}}
	if _, err := planmode.WritePlan(m.cwd, m.activeSession.SessionID, "original"); err != nil {
		t.Fatal(err)
	}
	held := make(chan planUpdateMsg, 1)
	release := make(chan struct{})
	m.runtimeMessageSink = func(msg tea.Msg) {
		if plan, ok := msg.(planUpdateMsg); ok {
			held <- plan
			<-release
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m = m.beginRun(cancel)
	cmd := m.runAgent(m.activeRunID, ctx, "plan", nil)
	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()
	var delayed planUpdateMsg
	select {
	case delayed = <-held:
	case <-time.After(10 * time.Second):
		close(release)
		t.Fatal("result callback did not reach publication")
	}
	m.cancelRun()
	staged, finish, err := planmode.StageEditor(m.cwd, m.activeSession.SessionID)
	if err != nil {
		close(release)
		t.Fatal(err)
	}
	if err := os.WriteFile(staged, []byte("1. [pending] accepted edit"), 0600); err != nil {
		close(release)
		finish(false)
		t.Fatal(err)
	}
	msg := finishPlanEditor(m.cwd, m.activeSession.SessionID, staged, finish, nil)
	if msg.err != nil {
		close(release)
		t.Fatal(msg.err)
	}
	updated, _ := m.Update(msg)
	m = updated.(model)
	close(release)
	var response tea.Msg
	select {
	case response = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("cancelled run did not finish")
	}
	updated, _ = m.Update(delayed)
	m = updated.(model)
	updated, _ = m.Update(response)
	m = updated.(model)
	content, _, err := planmode.ReadPlan(m.cwd, m.activeSession.SessionID)
	if err != nil || content != "1. [pending] accepted edit\n" {
		t.Fatalf("obsolete callback replaced editor: %q, %v", content, err)
	}
	if m.plan.isEmpty() || !strings.Contains(m.formatPlanDraft(), "accepted edit") {
		t.Fatalf("accepted UI plan lost: %s", m.formatPlanDraft())
	}
	if len(m.flushRunIDs) != 0 {
		t.Fatal("cancelled run events did not drain")
	}
}

func TestEditorRecoveryCollisionKeepsBothFiles(t *testing.T) {
	m := newPlanCommandTestModel(t, t.TempDir(), agent.PermissionModePlan)
	if _, err := planmode.WritePlan(m.cwd, m.activeSession.SessionID, "original"); err != nil {
		t.Fatal(err)
	}
	staged, finish, err := planmode.StageEditor(m.cwd, m.activeSession.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer finish(false)
	if err := os.WriteFile(staged, []byte("saved edit"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := planmode.WritePlan(m.cwd, m.activeSession.SessionID, "newer"); err != nil {
		t.Fatal(err)
	}
	recovery := filepath.Join(filepath.Dir(staged), "zero-recovered-"+strings.TrimPrefix(filepath.Base(staged), planmode.StagedPlanFilePrefix))
	if err := os.WriteFile(recovery, []byte("existing recovery"), 0600); err != nil {
		t.Fatal(err)
	}
	msg := finishPlanEditor(m.cwd, m.activeSession.SessionID, staged, finish, nil)
	if msg.err == nil || !strings.Contains(msg.err.Error(), staged) {
		t.Fatalf("missing original recovery location: %v", msg.err)
	}
	for path, want := range map[string]string{staged: "saved edit", recovery: "existing recovery"} {
		got, err := os.ReadFile(path)
		if err != nil || string(got) != want {
			t.Fatalf("recovery content = %q, want %q: %v", got, want, err)
		}
	}
	if _, err := os.Stat(staged + ".lock"); !os.IsNotExist(err) {
		t.Fatalf("staging lock retained: %v", err)
	}
}
