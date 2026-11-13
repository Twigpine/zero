package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/agent"
	"github.com/Twigpine/zero/internal/config"
)

func TestParseExecArgsRecognizesPlanFlag(t *testing.T) {
	options, _, err := parseExecArgs([]string{"--plan", "draft a plan"})
	if err != nil {
		t.Fatalf("parseExecArgs: %v", err)
	}
	if !options.plan {
		t.Fatal("expected --plan to set options.plan")
	}
}

func TestParseExecArgsRejectsPlanWithUseSpec(t *testing.T) {
	_, _, err := parseExecArgs([]string{"--plan", "--use-spec", "draft a plan"})
	if err == nil || !strings.Contains(err.Error(), "not both") {
		t.Fatalf("expected --plan/--use-spec validation, got %v", err)
	}
}

func TestParseExecArgsRejectsPlanWithSkipPermissionsUnsafe(t *testing.T) {
	_, _, err := parseExecArgs([]string{"--plan", "--skip-permissions-unsafe", "draft a plan"})
	if err == nil || !strings.Contains(err.Error(), "not both") {
		t.Fatalf("expected --plan/--skip-permissions-unsafe validation, got %v", err)
	}
}

// TestParseExecArgsRejectsPlanWithWorktree guards against worktree
// preparation (a filesystem mutation) running ahead of the plan mode gate:
// options.worktree is processed in runExec before the plan permission mode is
// assigned, so the combination must be rejected during option validation,
// before any worktree prep can occur.
func TestParseExecArgsRejectsPlanWithWorktree(t *testing.T) {
	_, _, err := parseExecArgs([]string{"--plan", "--worktree", "draft a plan"})
	if err == nil || !strings.Contains(err.Error(), "--worktree") {
		t.Fatalf("expected --plan/--worktree validation, got %v", err)
	}
}

func TestParseExecArgsRejectsPlanWithNonPlanPermissionMode(t *testing.T) {
	_, _, err := parseExecArgs([]string{"--plan", "--permission-mode=ask", "draft a plan"})
	if err == nil || !strings.Contains(err.Error(), "--permission-mode") {
		t.Fatalf("expected --plan/--permission-mode validation, got %v", err)
	}

	options, _, err := parseExecArgs([]string{"--plan", "--permission-mode=plan", "draft a plan"})
	if err != nil {
		t.Fatalf("expected --plan with --permission-mode=plan to succeed, got %v", err)
	}
	if !options.plan || options.permissionMode != "plan" {
		t.Fatalf("expected options.plan=true and permissionMode=plan, got plan=%v mode=%q", options.plan, options.permissionMode)
	}
}

// TestParseExecArgsPermissionModePlanSharesCombinationGuards ensures the
// combination rejects that key off --plan also fire for --permission-mode plan,
// which reaches PermissionModePlan without setting options.plan.
func TestParseExecArgsPermissionModePlanSharesCombinationGuards(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"worktree", []string{"--permission-mode", "plan", "--worktree", "draft a plan"}, "--worktree"},
		{"use-spec", []string{"--permission-mode=plan", "--use-spec", "draft a plan"}, "not both"},
		{"skip-permissions-unsafe", []string{"--permission-mode", "plan", "--skip-permissions-unsafe", "draft a plan"}, "not both"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := parseExecArgs(tc.args)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected --permission-mode plan / %s validation containing %q, got %v", tc.name, tc.want, err)
			}
		})
	}

	// Bare --permission-mode plan (no conflicting flags) still parses.
	options, _, err := parseExecArgs([]string{"--permission-mode", "plan", "draft a plan"})
	if err != nil {
		t.Fatalf("expected bare --permission-mode plan to succeed, got %v", err)
	}
	if options.permissionMode != "plan" || options.plan {
		t.Fatalf("expected permissionMode=plan and plan=false, got mode=%q plan=%v", options.permissionMode, options.plan)
	}
}

// TestRunExecPlanHidesWriteAndShellToolsFromListing drives the real --plan
// flag through runExec (via --list-tools, so no provider is needed) and
// confirms write_file and bash — advertised under every other mode covered by
// TestRunExecListToolsAppliesModeBeforeListing-style tests — are hidden,
// mirroring the TUI /plan on gating end to end from the CLI entry point.
func TestRunExecPlanHidesWriteAndShellToolsFromListing(t *testing.T) {
	cwd := t.TempDir()
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := runWithDeps([]string{"exec", "--plan", "--list-tools"}, &stdout, &stderr, appDeps{
		getwd: func() (string, error) {
			return cwd, nil
		},
		resolveConfig: func(string, config.Overrides) (config.ResolvedConfig, error) {
			return execResolvedConfig(), nil
		},
	})

	if exitCode != exitSuccess {
		t.Fatalf("expected exit code %d, got %d: %s", exitSuccess, exitCode, stderr.String())
	}
	listing := stdout.String()
	if !strings.Contains(listing, "Tools visible to model") {
		t.Fatalf("expected --plan --list-tools to list tools, got %q", listing)
	}
	if !strings.Contains(listing, "read_file") {
		t.Fatalf("expected plan mode to still list read_file, got %q", listing)
	}
	for _, hidden := range []string{"write_file", "edit_file", "apply_patch", "bash"} {
		if strings.Contains(listing, hidden) {
			t.Fatalf("expected --plan to hide %q from the tool listing, got %q", hidden, listing)
		}
	}
}

// TestRunExecPermissionModePlanHidesWriteAndShellToolsFromListing covers the
// --permission-mode plan entry path, which does not set options.plan and must
// still hide write and shell tools the same way --plan does.
func TestRunExecPermissionModePlanHidesWriteAndShellToolsFromListing(t *testing.T) {
	cwd := t.TempDir()
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := runWithDeps([]string{"exec", "--permission-mode", "plan", "--list-tools"}, &stdout, &stderr, appDeps{
		getwd: func() (string, error) {
			return cwd, nil
		},
		resolveConfig: func(string, config.Overrides) (config.ResolvedConfig, error) {
			return execResolvedConfig(), nil
		},
	})

	if exitCode != exitSuccess {
		t.Fatalf("expected exit code %d, got %d: %s", exitSuccess, exitCode, stderr.String())
	}
	listing := stdout.String()
	if !strings.Contains(listing, "Tools visible to model") {
		t.Fatalf("expected --permission-mode plan --list-tools to list tools, got %q", listing)
	}
	if !strings.Contains(listing, "read_file") {
		t.Fatalf("expected plan mode to still list read_file, got %q", listing)
	}
	for _, hidden := range []string{"write_file", "edit_file", "apply_patch", "bash"} {
		if strings.Contains(listing, hidden) {
			t.Fatalf("expected --permission-mode plan to hide %q from the tool listing, got %q", hidden, listing)
		}
	}
}

func TestResolveExecPermissionModePlanOverride(t *testing.T) {
	options := execOptions{autonomy: "low", plan: true}
	mode, err := resolveExecPermissionMode(options)
	if err != nil {
		t.Fatalf("resolveExecPermissionMode: %v", err)
	}
	// resolveExecPermissionMode itself only resolves --auto; the --plan override
	// is applied by the caller (runExec) afterward, same as --use-spec. This
	// pins the precondition: --plan must not interfere with autonomy resolution.
	if mode != agent.PermissionModeAuto {
		t.Fatalf("resolveExecPermissionMode with --plan = %q, want auto (override applied by the caller)", mode)
	}
}
