package cli

import (
	"io/fs"
	"runtime"
	"testing"
)

func TestImportWarningNeverStatsForeignNetworkWorkspace(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("native Windows passive path")
	}
	oldStat, oldLocal := importWorkspaceStat, importWorkspaceLocal
	t.Cleanup(func() { importWorkspaceStat, importWorkspaceLocal = oldStat, oldLocal })
	calls := 0
	importWorkspaceStat = func(string) (fs.FileInfo, error) { calls++; return nil, fs.ErrNotExist }
	for _, pair := range [][2]string{{`\\server\one`, `\\server\two`}, {`Z:\one`, `Z:\two`}} {
		importWorkspaceLocal = func(string) bool { return false }
		if pathsEqualForOS(pair[0], pair[1], "windows") {
			t.Error("different paths compared equal")
		}
		if calls != 0 {
			t.Fatalf("passive network comparison made %d Stat calls", calls)
		}
	}
}
