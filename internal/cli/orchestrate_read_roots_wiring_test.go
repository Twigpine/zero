package cli

import (
	"os"
	"strings"
	"testing"
)

// Both surfaces that launch plan tasks must wire ExtraReadRoots, or a
// request_permissions READ grant never reaches those tasks and a plan auditing a
// granted external path fails "outside the workspace" in every finder.
//
// This is the unwired-feature guard: an option that is built but never passed
// does nothing, and mutation testing cannot catch it because deleting an unused
// wiring breaks no test. So this asserts the wiring is present at the source, in
// both the TUI (app.go) and headless (exec.go) paths, beside the write grant it
// mirrors.
func TestBothPathsWireExtraReadRootsBesideWrite(t *testing.T) {
	for _, file := range []string{"app.go", "exec.go"} {
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		if !strings.Contains(string(src), "ExtraReadRoots:") {
			t.Fatalf("%s does not wire ExtraReadRoots — a plan there cannot read a granted path", file)
		}
	}
}
