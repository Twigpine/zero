package acp

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/Twigpine/zero/internal/agentsessions"
	"github.com/Twigpine/zero/internal/sessions"
)

func TestSessionListDoesNotResolveForeignNetworkWorkspace(t *testing.T) {
	deps := testDeps(t)
	cwd := filepath.Join(t.TempDir(), "foreign")
	if _, err := deps.Store.Create(sessions.CreateInput{Cwd: cwd, Tag: agentsessions.ImportTag("codex", "foreign")}); err != nil {
		t.Fatal(err)
	}
	old := passiveImportedWorkspaceLocal
	t.Cleanup(func() { passiveImportedWorkspaceLocal = old })
	passiveImportedWorkspaceLocal = func(path string) bool { return path != cwd }
	calls := 0
	deps.ResolveWorkspaceRoot = func(path string) (string, error) {
		if path == cwd {
			calls++
		}
		return path, nil
	}
	a := &Agent{deps: deps}
	for _, filter := range []string{"", t.TempDir()} {
		params, _ := json.Marshal(ListSessionsParams{Cwd: filter})
		got, err := a.handleSessionList(context.Background(), params)
		if err != nil {
			t.Fatal(err)
		}
		if len(got.(ListSessionsResult).Sessions) != 0 || calls != 0 {
			t.Fatalf("passive list probed foreign root: %+v calls=%d", got, calls)
		}
	}
}
