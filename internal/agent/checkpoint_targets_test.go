package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/Twigpine/zero/internal/tools"
)

func TestCheckpointTargets(t *testing.T) {
	root := t.TempDir()
	// Absolute headers must be compared against the physical root: t.TempDir
	// differs from it on macOS (/var -> /private/var) and via Windows short names.
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	if err := os.WriteFile(filepath.Join(root, "main.txt"), []byte("before\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	relativePatch := "*** Begin Patch\n*** Update File: main.txt\n@@\n-before\n+after\n*** End Patch\n"
	absolutePatch := "*** Begin Patch\n*** Update File: " + filepath.Join(root, "main.txt") + "\n@@\n-before\n+after\n*** End Patch\n"
	multiFilePatch := "*** Begin Patch\n*** Update File: main.txt\n@@\n-before\n+after\n*** Add File: notes.txt\n+new\n*** End Patch\n"
	jsonArgs, err := json.Marshal(map[string]any{"patch": relativePatch})
	if err != nil {
		t.Fatal(err)
	}

	registry := tools.NewRegistry()
	registry.Register(tools.NewScopedApplyPatchTool(root, nil))
	unbuiltinRegistry := tools.NewRegistry()
	unbuiltinRegistry.Register(&customApplyPatchTool{})

	for _, tc := range []struct {
		name     string
		registry *tools.Registry
		call     ToolCall
		want     []string
	}{
		{
			name: "freeform relative header",
			call: ToolCall{Name: "apply_patch", Arguments: relativePatch, Freeform: true},
			want: []string{"main.txt"},
		},
		{
			name: "freeform absolute header stays workspace-relative",
			call: ToolCall{Name: "apply_patch", Arguments: absolutePatch, Freeform: true},
			want: []string{"main.txt"},
		},
		{
			name: "freeform multi-file patch",
			call: ToolCall{Name: "apply_patch", Arguments: multiFilePatch, Freeform: true},
			want: []string{"main.txt", "notes.txt"},
		},
		{
			name: "json arguments",
			call: ToolCall{Name: "apply_patch", Arguments: string(jsonArgs)},
			want: []string{"main.txt"},
		},
		{
			name: "freeform traversal is rejected",
			call: ToolCall{Name: "apply_patch", Arguments: "*** Begin Patch\n*** Update File: ../escape.txt\n@@\n-x\n+y\n*** End Patch\n", Freeform: true},
			want: nil,
		},
		{
			name:     "freeform custom apply_patch is not prepared",
			registry: unbuiltinRegistry,
			call:     ToolCall{Name: "apply_patch", Arguments: relativePatch, Freeform: true},
			want:     nil,
		},
		{
			name: "unknown freeform tool",
			call: ToolCall{Name: "read_file", Arguments: "raw", Freeform: true},
			want: nil,
		},
		{
			name: "malformed json arguments",
			call: ToolCall{Name: "apply_patch", Arguments: "{"},
			want: nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg := registry
			if tc.registry != nil {
				reg = tc.registry
			}
			got := CheckpointTargets(root, reg, tc.call)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("CheckpointTargets() = %#v, want %#v", got, tc.want)
			}
		})
	}

	if got := CheckpointTargets(root, nil, ToolCall{Name: "apply_patch", Arguments: relativePatch}); got != nil {
		t.Fatalf("CheckpointTargets(nil registry) = %#v, want nil", got)
	}
}
