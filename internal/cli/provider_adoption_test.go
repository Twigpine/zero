package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Gitlawb/zero/internal/provideronboarding"
	"mvdan.cc/sh/v3/shell"
)

func TestLocalAdoptionSavesAndReloadsEligibleModel(t *testing.T) {
	const wantModel = "-loaded chat model"
	requests := make(chan string, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"data":[{"id":"text-embedding-local"},{"id":%q}]}`, wantModel)
		case "/v1/chat/completions":
			var body struct {
				Model string `json:"model"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode completion request: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if r.Header.Get("Authorization") != "" {
				t.Error("local adoption unexpectedly sent authorization")
			}
			select {
			case requests <- body.Model:
			default:
				t.Error("unexpected extra completion requests")
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"adoption ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	detected := provideronboarding.DetectLocalRuntimes(context.Background(), provideronboarding.LocalDetectOptions{
		HTTPClient: server.Client(),
		Candidates: []provideronboarding.LocalRuntime{{CatalogID: "atomic-chat-local", Name: "Atomic Chat Local", DefaultModel: "local-model", BaseURL: server.URL + "/v1"}},
	})
	if len(detected) != 1 {
		t.Fatalf("expected one detected runtime: %+v", detected)
	}
	args, err := shell.Fields(detected[0].SetupAction().Command, func(string) string { return "" })
	if err != nil || len(args) < 4 {
		t.Fatalf("invalid adoption command: args=%q err=%v", args, err)
	}
	root := t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		childArgs := append([]string{"-test.run=^TestLocalAdoptionCLIProcess$", "--"}, args...)
		cmd := exec.CommandContext(ctx, os.Args[0], childArgs...)
		cmd.Dir = root
		cmd.Env = []string{
			"ZERO_TEST_LOCAL_ADOPTION=1", "ZERO_CRED_STORAGE=encrypted-file",
			"HOME=" + root, "USERPROFILE=" + root,
			"XDG_CONFIG_HOME=" + filepath.Join(root, "config"),
			"XDG_CACHE_HOME=" + filepath.Join(root, "cache"),
			"XDG_STATE_HOME=" + filepath.Join(root, "state"),
			"APPDATA=" + filepath.Join(root, "config"),
			"LOCALAPPDATA=" + filepath.Join(root, "cache"),
		}
		for _, key := range []string{"PATH", "SystemRoot", "WINDIR", "TMPDIR", "TEMP", "TMP"} {
			if value, ok := os.LookupEnv(key); ok {
				cmd.Env = append(cmd.Env, key+"="+value)
			}
		}
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("CLI %q failed: %v\n%s", args, err, output)
		}
		return string(output)
	}
	// The mock uses a random port; only override the endpoint, retaining all
	// generated arguments for the real add parser and persistence path.
	run(append(args[1:], "--base-url", server.URL+"/v1")...)
	if output := run("exec", "--cwd", root, "Reply with adoption ok"); !strings.Contains(output, "adoption ok") {
		t.Fatalf("completion output = %q", output)
	}
	select {
	case model := <-requests:
		if model != wantModel {
			t.Fatalf("fresh process sent model %q, want eligible model %q", model, wantModel)
		}
	default:
		t.Fatal("fresh process did not send a completion request")
	}
}

func TestLocalAdoptionCLIProcess(t *testing.T) {
	if os.Getenv("ZERO_TEST_LOCAL_ADOPTION") != "1" {
		return
	}
	for i, arg := range os.Args {
		if arg == "--" {
			os.Exit(Run(os.Args[i+1:], os.Stdout, os.Stderr))
		}
	}
	t.Fatal("missing helper argument separator")
}
