package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Gitlawb/zero/internal/agent"
	"github.com/Gitlawb/zero/internal/remotetoken"
	"github.com/Gitlawb/zero/internal/sessions"
	"github.com/Gitlawb/zero/internal/tools"
)

func TestExecCheckpointBeforeDeniedCredentialMutation(t *testing.T) {
	root, data := t.TempDir(), t.TempDir()
	const secret = "checkpoint-bearer-must-not-be-copied"
	token := filepath.Join(root, "token")
	if err := os.WriteFile(token, []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(remotetoken.EnvToken, "")
	t.Setenv(remotetoken.EnvTokenFile, token)
	t.Setenv(remotetoken.EnvTokenFileResolved, "")
	t.Setenv(remotetoken.EnvTokenFileIdentity, "")
	store := sessions.NewStore(sessions.StoreOptions{RootDir: data})
	session, err := store.Create(sessions.CreateInput{SessionID: "credential-checkpoint"})
	if err != nil {
		t.Fatal(err)
	}
	recorder := execSessionRecorder{prepared: sessions.PreparedExec{Store: store, Session: session}}
	registry := tools.NewRegistry()
	registry.Register(tools.NewScopedWriteFileTool(root, nil))
	registry.Register(tools.NewScopedEditFileTool(root, nil))
	for _, name := range []string{"write_file", "edit_file"} {
		args := map[string]any{"path": "token", "content": "replacement", "overwrite": true, "old_string": secret, "new_string": "replacement"}
		encoded, err := json.Marshal(args)
		if err != nil {
			t.Fatal(err)
		}
		if _, captured := recorder.captureCheckpoint(root, agent.ToolCall{Name: name, Arguments: string(encoded)}); captured {
			t.Fatalf("%s checkpoint captured protected credential", name)
		}
		result := registry.RunWithOptions(context.Background(), name, args, tools.RunOptions{PermissionGranted: true})
		if result.Status == tools.StatusOK {
			t.Fatalf("%s mutation was allowed", name)
		}
		if !strings.Contains(result.Output, "remote bridge token") {
			t.Fatalf("%s failed outside credential protection: %s", name, result.Output)
		}
	}
	// Read every persisted file through the ordinary read tool: a leaked blob
	// has a fresh inode and would otherwise evade the credential exclusion.
	reader := tools.NewRegistry()
	reader.Register(tools.NewScopedReadFileTool(data, nil))
	err = filepath.WalkDir(data, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		result := reader.Run(context.Background(), "read_file", map[string]any{"path": path})
		if result.Status != tools.StatusOK {
			t.Fatalf("read persisted file: %+v", result)
		}
		if strings.Contains(result.Output, secret) {
			t.Fatalf("checkpoint leaked credential through %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
