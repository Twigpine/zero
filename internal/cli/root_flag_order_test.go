package cli

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Gitlawb/zero/internal/agent"
	"github.com/Gitlawb/zero/internal/config"
	"github.com/Gitlawb/zero/internal/sandbox"
	"github.com/Gitlawb/zero/internal/tui"
	"github.com/Gitlawb/zero/internal/zeroruntime"
)

// rootFlagOrderings is every ordering of the given flag groups, each group
// kept intact so a flag stays next to its value.
func rootFlagOrderings(groups [][]string) [][]string {
	if len(groups) == 0 {
		return [][]string{{}}
	}
	var orderings [][]string
	for index, group := range groups {
		rest := append(append([][]string{}, groups[:index]...), groups[index+1:]...)
		for _, tail := range rootFlagOrderings(rest) {
			orderings = append(orderings, append(append([]string{}, group...), tail...))
		}
	}
	return orderings
}

// ROOT FLAGS COMPOSE IN ANY ORDER. Each leading-flag splitter stops at the first
// token it does not own, so running them once in a fixed sequence made the
// order the operator wrote them in load-bearing: `zero --allow-escalation
// --theme auto` stranded `--theme auto` as an unknown command and exited with
// an argument error instead of launching. Every ordering of the three root
// flags, with --skip-permissions-unsafe absent or at any position, has to reach
// the TUI with all three applied.
func TestRootFlagsComposeInAnyOrder(t *testing.T) {
	extra := t.TempDir()
	resolvedExtra, err := filepath.EvalSymlinks(extra)
	if err != nil {
		t.Fatal(err)
	}
	groups := [][]string{{"--add-dir", extra}, {"--theme", "auto"}, {"--allow-escalation"}}
	cases := rootFlagOrderings(groups)
	cases = append(cases, rootFlagOrderings(append(groups, []string{"--skip-permissions-unsafe"}))...)
	if len(cases) != 6+24 {
		t.Fatalf("SETUP INVALID: %d orderings, want 30", len(cases))
	}
	for _, args := range cases {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			options := captureTUIOptions(t, args...)
			wantMode := agent.PermissionModeAsk
			for _, arg := range args {
				if arg == "--skip-permissions-unsafe" {
					wantMode = agent.PermissionModeUnsafe
				}
			}
			if options.PermissionMode != wantMode {
				t.Errorf("PermissionMode = %q, want %q", options.PermissionMode, wantMode)
			}
			if !options.AllowEscalation {
				t.Error("escalation not enabled")
			}
			if options.Theme != "auto" {
				t.Errorf("Theme = %q, want auto", options.Theme)
			}
			if options.AgentOptions.Sandbox == nil {
				t.Fatal("no sandbox engine on the launched options")
			}
			roots := options.AgentOptions.Sandbox.Scope().Roots()
			found := false
			for _, root := range roots {
				if root == resolvedExtra {
					found = true
				}
			}
			if !found {
				t.Errorf("scope roots = %v, want the --add-dir root %q", roots, resolvedExtra)
			}
		})
	}
}

// The last --theme wins even with another root flag between two of them, and a
// --theme written before --skip-permissions-unsafe reaches the TUI: the unsafe
// path used to launch with only the theme written after the flag, dropping one
// written before it.
func TestRootThemeLastOccurrenceWinsAcrossOtherFlags(t *testing.T) {
	for _, testCase := range []struct {
		args []string
		want string
	}{
		{[]string{"--theme", "light", "--allow-escalation", "--theme", "auto"}, "auto"},
		{[]string{"--theme", "auto", "--skip-permissions-unsafe"}, "auto"},
		{[]string{"--theme", "light", "--skip-permissions-unsafe", "--theme", "auto"}, "auto"},
	} {
		t.Run(strings.Join(testCase.args, " "), func(t *testing.T) {
			options := captureTUIOptions(t, testCase.args...)
			if options.Theme != testCase.want {
				t.Fatalf("Theme = %q, want %q", options.Theme, testCase.want)
			}
		})
	}
}

// THE ROOT FLAG REACHES EXEC. A --allow-escalation written before the
// subcommand is forwarded the way --add-dir is, so `zero --allow-escalation -p
// "..."` runs with the opt-in the help text promises instead of silently
// without it. The negative control is the same run without the flag, which
// must not advertise escalate_model.
func TestRootAllowEscalationForwardsIntoExec(t *testing.T) {
	for _, testCase := range []struct {
		name string
		args []string
	}{
		{"exec subcommand", []string{"--allow-escalation", "exec", "say hi"}},
		{"prompt flag", []string{"--allow-escalation", "-p", "say hi"}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if !execAdvertisesEscalateModel(t, testCase.args) {
				t.Fatalf("%v ran without escalate_model advertised: the root flag was dropped before exec", testCase.args)
			}
			if execAdvertisesEscalateModel(t, testCase.args[1:]) {
				t.Fatalf("%v advertised escalate_model without the flag", testCase.args[1:])
			}
		})
	}
}

// Everywhere else the flag is rejected loudly rather than discarded, the way
// --add-dir is: help and version run no agent and could only ignore it.
func TestRootAllowEscalationIsRejectedWhereNothingConsumesIt(t *testing.T) {
	for _, args := range [][]string{
		{"--allow-escalation", "version"},
		{"--allow-escalation", "help"},
		{"--allow-escalation", "completions", "bash"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			launched := false
			exitCode := runWithDeps(args, &stdout, &stderr, appDeps{
				getwd:  func() (string, error) { return t.TempDir(), nil },
				runTUI: func(context.Context, tui.Options) int { launched = true; return exitSuccess },
			})
			if exitCode == exitSuccess || launched {
				t.Fatalf("exit = %d launched = %v: the flag was discarded instead of rejected", exitCode, launched)
			}
			if !strings.Contains(stderr.String(), "--allow-escalation is only supported for the interactive TUI and exec") {
				t.Fatalf("stderr does not name the flag: %s", stderr.String())
			}
		})
	}
}

// execAdvertisesEscalateModel dispatches args through the root command with a
// provider that records the tools advertised on the first request, and reports
// whether escalate_model was among them.
func execAdvertisesEscalateModel(t *testing.T, args []string) bool {
	t.Helper()
	isolateCLIUserState(t)
	provider := &toolListingProvider{}
	var stdout, stderr bytes.Buffer
	exitCode := runWithDeps(args, &stdout, &stderr, appDeps{
		getwd: func() (string, error) { return t.TempDir(), nil },
		resolveConfig: func(string, config.Overrides) (config.ResolvedConfig, error) {
			return execResolvedConfig(), nil
		},
		newProvider: func(config.ProviderProfile) (zeroruntime.Provider, error) {
			return provider, nil
		},
		newSandboxStore: func() (*sandbox.GrantStore, error) {
			return sandbox.NewGrantStore(sandbox.StoreOptions{FilePath: filepath.Join(t.TempDir(), "sandbox-grants.json")})
		},
	})
	if exitCode != exitSuccess {
		t.Fatalf("%v: exit = %d, stderr = %s", args, exitCode, stderr.String())
	}
	if provider.requests == 0 {
		t.Fatalf("SETUP INVALID: %v never reached the provider", args)
	}
	for _, name := range provider.toolNames {
		if name == "escalate_model" {
			return true
		}
	}
	return false
}

// toolListingProvider answers every request with a one-line text reply and
// keeps the tool names advertised on the first one.
type toolListingProvider struct {
	requests  int
	toolNames []string
}

func (provider *toolListingProvider) StreamCompletion(_ context.Context, request zeroruntime.CompletionRequest) (<-chan zeroruntime.StreamEvent, error) {
	provider.requests++
	if provider.requests == 1 {
		for _, tool := range request.Tools {
			provider.toolNames = append(provider.toolNames, tool.Name)
		}
	}
	ch := make(chan zeroruntime.StreamEvent, 2)
	ch <- zeroruntime.StreamEvent{Type: zeroruntime.StreamEventText, Content: "hi"}
	ch <- zeroruntime.StreamEvent{Type: zeroruntime.StreamEventDone}
	close(ch)
	return ch, nil
}
