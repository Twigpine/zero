package sandbox

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestApplyPatchPathBlockOnlyRejectsRelativeTraversal(t *testing.T) {
	root := t.TempDir()
	inside := filepath.Join(root, "main.js")
	structured := func(path string) string {
		return strings.Join([]string{"*** Begin Patch", "*** Update File: " + path, "@@", "-a", "+b", "*** End Patch"}, "\n")
	}
	for name, patch := range map[string]string{
		"absolute inside workspace": structured(inside),
		"relative":                  structured("main.js"),
		"decorated markers":         "*** Begin Patch ***\n*** Update File: main.js\n@@\n-a\n+b\n*** End Patch ***",
		"no-space marker":           "***Begin Patch\n*** Update File: main.js\n@@\n-a\n+b\n***End Patch",
	} {
		request := Request{ToolName: "apply_patch", WorkspaceRoot: root, SideEffect: SideEffectWrite, Args: map[string]any{"patch": patch}}
		if block := applyPatchPathBlock(request); block != nil {
			t.Fatalf("%s: unexpected block %+v", name, block)
		}
	}
	for _, path := range []string{"../escape.js", ".."} {
		for name, patch := range map[string]string{"canonical": structured(path), "no-space": "***Begin Patch\n*** Update File: " + path + "\n@@\n-a\n+b\n***End Patch"} {
			request := Request{ToolName: "apply_patch", WorkspaceRoot: root, SideEffect: SideEffectWrite, Args: map[string]any{"patch": patch}}
			block := applyPatchPathBlock(request)
			if block == nil || block.Code != BlockOutsideWorkspace {
				t.Fatalf("%s %q must be blocked as traversal, got %+v", name, path, block)
			}
		}
	}
}

// Every marker spelling the tool applies must be classified as structured at
// the sandbox boundary too; otherwise the boundary scans the patch as a unified
// diff, extracts no targets, and validates nothing (fail-open).
func TestStructuredPatchClassifierMatchesToolSpellings(t *testing.T) {
	for _, header := range []string{"*** Begin Patch", "*** Begin Patch ***", "***Begin Patch", "  *** Begin Patch  ", "\ufeff*** Begin Patch"} {
		patch := header + "\n*** Update File: main.js\n@@\n-a\n+b\n*** End Patch"
		if !IsStructuredPatch(patch) {
			t.Fatalf("%q must classify as a structured patch", header)
		}
		if paths := applyPatchPaths(patch); len(paths) != 1 || paths[0] != "main.js" {
			t.Fatalf("%q: sandbox must extract the structured target, got %v", header, paths)
		}
	}
	for _, header := range []string{"--- a/x", "Begin Patch", "*** Begin Patchwork", "*** Update File: x"} {
		if IsStructuredPatch(header + "\n-a\n+b") {
			t.Fatalf("%q must not classify as a structured patch", header)
		}
	}
	if StructuredPatchMarker("*** End Patch ***") != "end" || StructuredPatchMarker("***End Patch") != "end" {
		t.Fatal("decorated end markers must classify as end")
	}
}

func TestApplyPatchRequestPathsCarryAbsolutePathsToScopeValidation(t *testing.T) {
	root := t.TempDir()
	// NewScope also grants the system temp dir, so a sibling t.TempDir() is
	// legitimately in scope; pick a path under the filesystem root instead.
	outside, err := filepath.Abs(filepath.Join(string(filepath.Separator), "zero-outside-workspace-test", "escape.js"))
	if err != nil {
		t.Fatal(err)
	}
	scope, err := NewScope(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(root, "main.js")
	structured := func(header, footer, path string) string {
		return strings.Join([]string{header, "*** Update File: " + path, "@@", "-a", "+b", footer}, "\n")
	}
	for name, spelling := range map[string][2]string{"canonical": {"*** Begin Patch", "*** End Patch"}, "no-space": {"***Begin Patch", "***End Patch"}} {
		// Failure path: the exact path the boundary parsed must be denied by the
		// scope. structuredPatchHeaderPaths normalises separators to "/", so the
		// parsed form is compared in slash form and then validated as-is.
		paths := applyPatchRequestPaths(map[string]any{"patch": structured(spelling[0], spelling[1], outside)})
		if len(paths) != 1 || paths[0] != filepath.ToSlash(outside) {
			t.Fatalf("%s: absolute patch path must reach scope validation unchanged, got %v", name, paths)
		}
		if block := scope.validate(paths[0]); block == nil || block.Code != BlockOutsideWorkspace {
			t.Fatalf("%s: scope must deny the parsed outside path %q, got %+v", name, paths[0], block)
		}
		// Success path: the parsed inside path must be accepted by the scope.
		paths = applyPatchRequestPaths(map[string]any{"patch": structured(spelling[0], spelling[1], inside)})
		if len(paths) != 1 || paths[0] != filepath.ToSlash(inside) {
			t.Fatalf("%s: inside patch path must reach scope validation unchanged, got %v", name, paths)
		}
		if block := scope.validate(paths[0]); block != nil {
			t.Fatalf("%s: scope must accept the parsed inside path %q, got %+v", name, paths[0], block)
		}
	}
}
