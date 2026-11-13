package agent

import (
	"context"
	"testing"

	"github.com/Twigpine/zero/internal/tools"
)

// argsPermTool implements tools.ArgsPermissioner; plainTool does not.
type argsPermTool struct{ perm tools.Permission }

func (argsPermTool) Name() string                                        { return "args" }
func (argsPermTool) Description() string                                 { return "" }
func (argsPermTool) Parameters() tools.Schema                            { return tools.Schema{} }
func (argsPermTool) Safety() tools.Safety                                { return tools.Safety{Permission: tools.PermissionPrompt} }
func (argsPermTool) Run(context.Context, map[string]any) tools.Result    { return tools.Result{} }
func (t argsPermTool) PermissionForArgs(map[string]any) tools.Permission { return t.perm }

type plainTool struct{ perm tools.Permission }

func (plainTool) Name() string                                     { return "plain" }
func (plainTool) Description() string                              { return "" }
func (plainTool) Parameters() tools.Schema                         { return tools.Schema{} }
func (t plainTool) Safety() tools.Safety                           { return tools.Safety{Permission: t.perm} }
func (plainTool) Run(context.Context, map[string]any) tools.Result { return tools.Result{} }

func TestEffectivePermission(t *testing.T) {
	// A tool implementing ArgsPermissioner refines its permission per call.
	if got := effectivePermission(argsPermTool{perm: tools.PermissionAllow}, nil); got != tools.PermissionAllow {
		t.Fatalf("args-aware tool: got %q, want allow", got)
	}
	if got := effectivePermission(argsPermTool{perm: tools.PermissionPrompt}, nil); got != tools.PermissionPrompt {
		t.Fatalf("args-aware tool: got %q, want prompt", got)
	}
	// A tool WITHOUT the interface falls back to its static Safety().Permission —
	// the args-aware path must never change a normal tool's behavior.
	if got := effectivePermission(plainTool{perm: tools.PermissionAllow}, nil); got != tools.PermissionAllow {
		t.Fatalf("plain tool: got %q, want its static allow", got)
	}
	if got := effectivePermission(plainTool{perm: tools.PermissionPrompt}, nil); got != tools.PermissionPrompt {
		t.Fatalf("plain tool: got %q, want its static prompt", got)
	}
}
