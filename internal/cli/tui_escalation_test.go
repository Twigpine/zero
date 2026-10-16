package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/Gitlawb/zero/internal/tui"
)

// captureTUIOptions runs the root command with the TUI launch intercepted, and
// returns the options the interactive session would have started with.
func captureTUIOptions(t *testing.T, args ...string) tui.Options {
	t.Helper()
	isolateCLIUserState(t)
	var captured tui.Options
	var stdout, stderr bytes.Buffer
	workspace := t.TempDir()
	exitCode := runWithDeps(args, &stdout, &stderr, appDeps{
		getwd: func() (string, error) { return workspace, nil },
		runTUI: func(_ context.Context, options tui.Options) int {
			captured = options
			return exitSuccess
		},
	})
	if exitCode != exitSuccess {
		t.Fatalf("exitCode = %d stdout=%s stderr=%s", exitCode, stdout.String(), stderr.String())
	}
	return captured
}

func hasEscalateModel(options tui.Options) bool {
	if options.AgentOptions.Registry == nil {
		return false
	}
	_, registered := options.AgentOptions.Registry.Get("escalate_model")
	return registered
}

// ESCALATION IS OFF UNLESS THE OPERATOR ASKS FOR IT.
//
// Escalation moves a run onto a different model, changing what it costs and which
// provider sees the conversation, so the interactive surface answers this the same
// conservative way `zero exec --allow-escalation` already does.
func TestInteractiveTUIRegistersEscalateModelOnlyWithTheFlag(t *testing.T) {
	if hasEscalateModel(captureTUIOptions(t)) {
		t.Error("escalate_model was registered without --allow-escalation")
	}
	if !hasEscalateModel(captureTUIOptions(t, "--allow-escalation")) {
		t.Error("--allow-escalation did not register escalate_model")
	}
}

// THE TOOL AND THE SWITCHERS ARE ONE FEATURE, NOT TWO.
//
// This is the failure the whole change exists to avoid. The agent loop performs a
// switch only when a switcher is wired, so registering escalate_model without
// wiring one ships a tool the model can call and the loop will silently ignore:
// the run reports an escalation that never happened. The reverse is merely dead
// code. Both halves ride on the same flag and this asserts they cannot be
// separated, whichever half a future change touches.
func TestInteractiveTUIEscalationToolAndSwitchersAreWiredTogether(t *testing.T) {
	for _, testCase := range []struct {
		name string
		args []string
		want bool
	}{
		{name: "default", args: nil, want: false},
		{name: "flagged", args: []string{"--allow-escalation"}, want: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			options := captureTUIOptions(t, testCase.args...)
			if got := hasEscalateModel(options); got != testCase.want {
				t.Fatalf("escalate_model registered = %v, want %v", got, testCase.want)
			}
			if options.AllowEscalation != testCase.want {
				t.Errorf("AllowEscalation = %v, want %v: the tool is registered on one gate and the switchers on another, so they can drift apart",
					options.AllowEscalation, testCase.want)
			}
		})
	}
}

// The flag is accepted on either side of --skip-permissions-unsafe, like the
// other root flags that may appear there.
func TestInteractiveTUIAcceptsAllowEscalationAroundUnsafe(t *testing.T) {
	for _, args := range [][]string{
		{"--allow-escalation", "--skip-permissions-unsafe"},
		{"--skip-permissions-unsafe", "--allow-escalation"},
	} {
		options := captureTUIOptions(t, args...)
		if !options.AllowEscalation {
			t.Errorf("%v did not enable escalation", args)
		}
		if !hasEscalateModel(options) {
			t.Errorf("%v did not register escalate_model", args)
		}
	}
}

// An =value form is a loud error rather than a silent enable, so a mistyped
// --allow-escalation=false cannot turn the feature ON.
func TestAllowEscalationRejectsAValue(t *testing.T) {
	var stdout, stderr bytes.Buffer
	exitCode := runWithDeps([]string{"--allow-escalation=false"}, &stdout, &stderr, appDeps{
		getwd:  func() (string, error) { return t.TempDir(), nil },
		runTUI: func(context.Context, tui.Options) int { return exitSuccess },
	})
	if exitCode == exitSuccess {
		t.Fatal("--allow-escalation=false was accepted; a mistyped disable must not silently enable escalation")
	}
	if !strings.Contains(stderr.String(), "--allow-escalation takes no value") {
		t.Errorf("stderr does not explain the flag: %s", stderr.String())
	}
}

func TestRootHelpDocumentsAllowEscalation(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if exitCode := runWithDeps([]string{"--help"}, &stdout, &stderr, appDeps{}); exitCode != exitSuccess {
		t.Fatalf("exitCode = %d stderr=%s", exitCode, stderr.String())
	}
	if !strings.Contains(stdout.String(), "--allow-escalation") {
		t.Error("the root help does not mention --allow-escalation, so the flag is undiscoverable")
	}
}
