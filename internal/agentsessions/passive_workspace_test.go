package agentsessions

import "testing"

func TestPassiveForeignWindowsWorkspacesNeverProbeNetworkRoots(t *testing.T) {
	for _, value := range []string{`\\server\share\project`, `\\?\UNC\server\share\project`, `\\.\device`, `C:relative`, `\relative`, `relative`} {
		calls := 0
		if passiveWindowsWorkspaceIsLocal(value, func(string) bool { calls++; return true }) || calls != 0 {
			t.Errorf("non-drive path probed: %q (%d)", value, calls)
		}
	}
	for _, value := range []string{`Z:\project`, `\\?\Z:\project`} {
		calls := 0
		if passiveWindowsWorkspaceIsLocal(value, func(root string) bool {
			calls++
			if root != `Z:\` {
				t.Errorf("non-root drive query: %q", root)
			}
			return false
		}) || calls != 1 {
			t.Errorf("remote mapped drive accepted: %q", value)
		}
	}
	for _, value := range []string{`C:\project`, `\\?\C:\project`} {
		if !passiveWindowsWorkspaceIsLocal(value, func(root string) bool { return root == `C:\` }) {
			t.Errorf("local drive refused: %q", value)
		}
	}
}
