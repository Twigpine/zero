package agent

import (
	"testing"

	"github.com/Twigpine/zero/internal/tools"
)

func TestPartitionToolsActiveAppendsLoadedToolAfterEagerBlock(t *testing.T) {
	// Cache-stability property: loading a deferred tool must APPEND it after the
	// always-eager tools, not sort it into the middle of them — otherwise the tool
	// definitions (the provider's cacheable prefix) shift on every load. The old
	// behaviour alpha-sorted the whole array, so "mcp__srv__alpha" jumped ahead of
	// "read_file".
	root := t.TempDir()
	registry := tools.NewRegistry()
	registry.Register(tools.NewScopedReadFileTool(root, nil)) // non-deferred, "read_file"
	registry.Register(fakeToolSearchTool{})
	registry.Register(fakeDeferredTool{name: "mcp__srv__alpha", desc: "alpha"})
	registry.Register(fakeDeferredTool{name: "mcp__srv__beta", desc: "beta"})

	exposed, _ := partitionTools(registry, PermissionModeAuto, Options{DeferThreshold: 2}, map[string]bool{"mcp__srv__alpha": true})

	pos := func(name string) int {
		for i := range exposed {
			if exposed[i].Name == name {
				return i
			}
		}
		return -1
	}
	readPos, searchPos, alphaPos := pos("read_file"), pos("tool_search"), pos("mcp__srv__alpha")
	if readPos < 0 || searchPos < 0 || alphaPos < 0 {
		t.Fatalf("expected read_file, tool_search and loaded alpha all exposed, got %#v", exposed)
	}
	// The eager tools must precede the loaded deferred tool (appended, not inserted).
	if readPos >= alphaPos || searchPos >= alphaPos {
		t.Fatalf("loaded deferred tool must be appended after the eager block; got read=%d search=%d alpha=%d", readPos, searchPos, alphaPos)
	}
	// beta was never loaded — it stays hidden.
	if pos("mcp__srv__beta") != -1 {
		t.Fatalf("unloaded deferred tool must stay hidden, got %#v", exposed)
	}
}
