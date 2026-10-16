package cli

import (
	"bytes"
	"context"
	"errors"
	"github.com/Gitlawb/zero/internal/config"
	"github.com/Gitlawb/zero/internal/mcp"
	"github.com/Gitlawb/zero/internal/tools"
	"github.com/Gitlawb/zero/internal/tui"
	"github.com/Gitlawb/zero/internal/zeroruntime"
	"strings"
	"testing"
)

type rpmExecProvider struct{ calls int }

func (p *rpmExecProvider) StreamCompletion(context.Context, zeroruntime.CompletionRequest) (<-chan zeroruntime.StreamEvent, error) {
	p.calls++
	ch := make(chan zeroruntime.StreamEvent, 4)
	if p.calls == 1 {
		ch <- zeroruntime.StreamEvent{Type: zeroruntime.StreamEventToolCallStart, ToolCallID: "fixture", ToolName: "read_file"}
		ch <- zeroruntime.StreamEvent{Type: zeroruntime.StreamEventToolCallDelta, ToolCallID: "fixture", ArgumentsFragment: `{"path":"missing-fixture.txt"}`}
		ch <- zeroruntime.StreamEvent{Type: zeroruntime.StreamEventToolCallEnd, ToolCallID: "fixture"}
	} else {
		ch <- zeroruntime.StreamEvent{Type: zeroruntime.StreamEventText, Content: "unexpected second completion"}
	}
	ch <- zeroruntime.StreamEvent{Type: zeroruntime.StreamEventDone}
	close(ch)
	return ch, nil
}
func rpmTestDeps(t *testing.T, p *rpmExecProvider) appDeps {
	t.Helper()
	root := t.TempDir()
	for _, k := range []string{"HOME", "USERPROFILE", "APPDATA", "LOCALAPPDATA", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME"} {
		t.Setenv(k, root)
	}
	return appDeps{getwd: func() (string, error) { return root, nil }, resolveConfig: func(string, config.Overrides) (config.ResolvedConfig, error) {
		c := execResolvedConfig()
		c.ModelRPM = map[string]int{c.Provider.Model: 1}
		return c, nil
	}, newProvider: func(config.ProviderProfile) (zeroruntime.Provider, error) { return p, nil }, registerMCPTools: func(context.Context, *tools.Registry, config.MCPConfig, mcp.RegisterOptions) (mcpToolRuntime, error) {
		return noopMCPRuntime{}, nil
	}}
}
func TestRunExecRPMRefusesSecondCompletion(t *testing.T) {
	p := &rpmExecProvider{}
	deps := rpmTestDeps(t, p)
	var out, err bytes.Buffer
	code := runWithDeps([]string{"exec", "fixture"}, &out, &err, deps)
	if code != exitProvider || p.calls != 1 || !strings.Contains(err.String(), "Local modelRPM cap reached") {
		t.Fatalf("exit=%d calls=%d stdout=%s stderr=%s", code, p.calls, out.String(), err.String())
	}
}
func TestInteractiveRPMFactorySharesWindow(t *testing.T) {
	p := &rpmExecProvider{}
	deps := rpmTestDeps(t, p)
	launched := false
	deps.runTUI = func(ctx context.Context, o tui.Options) int {
		launched = true
		if o.ModelRPM == nil {
			t.Fatal("TUI escalation lacks limiter")
		}
		for i := 0; i < 2; i++ {
			ss := o.NewTurnSessionProvider(o.ProviderProfile, p)
			s, e := ss.OpenTurnSession(ctx)
			if e != nil {
				t.Fatal(e)
			}
			_, e = s.Stream(ctx, zeroruntime.CompletionRequest{})
			var hit *zeroruntime.RPMLimitError
			if (i == 0 && e != nil) || (i == 1 && !errors.As(e, &hit)) {
				t.Fatalf("run=%d err=%v", i, e)
			}
		}
		return exitSuccess
	}
	var out, err bytes.Buffer
	code := runWithDeps(nil, &out, &err, deps)
	if !launched || code != exitSuccess || p.calls != 1 {
		t.Fatalf("launched=%v exit=%d calls=%d stderr=%s", launched, code, p.calls, err.String())
	}
}
