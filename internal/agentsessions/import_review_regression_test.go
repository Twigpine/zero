package agentsessions

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/sessions"
	"github.com/Twigpine/zero/internal/tools"
)

func TestImportedANSISecretsAreRedactedAtEveryEventSurface(t *testing.T) {
	secret := "sk-ant-api03-" + strings.Repeat("aB3x", 20)
	for _, escape := range []string{"\x1b[01;31m\x1b[K", "\x1b]8;;https://example.invalid\a", "\x1b[0m"} {
		value := secret[:6] + escape + secret[6:] + "\nnormal\ttext"
		identities := &importCallIdentities{}
		arguments, _ := json.Marshal(map[string]string{"command": value, "nested": value})
		events := []sessions.AppendEventInput{messageEvent("user", value), toolCallEvent(identities, "Bash", "id", string(arguments)), toolResultEvent(identities, "Bash", "id", tools.StatusOK, value)}
		store := sessions.NewStore(sessions.StoreOptions{RootDir: filepath.Join(t.TempDir(), "sessions")})
		meta, err := store.Create(sessions.CreateInput{})
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range events {
			if _, err := store.AppendEvent(meta.SessionID, event); err != nil {
				t.Fatal(err)
			}
		}
		persisted, err := store.ReadEvents(meta.SessionID)
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range persisted {
			body := string(event.Payload)
			if !strings.Contains(body, "[REDACTED]") || strings.Contains(body, secret[12:]) || strings.Contains(body, "[01;31m") {
				t.Errorf("import secret survived persisted event: %s", body)
			}
		}
		prepared, err := sessions.PrepareExec(sessions.PrepareExecOptions{Store: store, Resume: meta.SessionID})
		if err != nil {
			t.Fatal(err)
		}
		prompt := sessions.FormatExecPrompt("continue", prepared)
		if strings.Contains(prompt, secret[12:]) || strings.Contains(prompt, "[01;31m") {
			t.Error("secret reached resumed provider context")
		}
		if got := redact("first\nsecond\tcolumn"); got != "first\nsecond\tcolumn" {
			t.Errorf("transcript layout changed: %q", got)
		}
	}
}

func TestPendingCommandAndSearchAreNotCompletedActivity(t *testing.T) {
	for _, args := range []string{`{"command":"dangerous command"}`, `{"pattern":"needle"}`, "bare script"} {
		for _, status := range []tools.Status{"", tools.StatusError, tools.StatusOK} {
			log := newActivityLog("/w")
			log.observeCall("id", "Bash", args)
			if status != "" {
				log.observeResult("id", "Bash", status, "permission denied")
			}
			summary := joinedSummary(t, log.summaryEvents())
			completed := strings.Contains(summary, "Commands run:") || strings.Contains(summary, "Searched for:")
			if completed != (status == tools.StatusOK) {
				t.Errorf("status=%q args=%s completed=%v: %s", status, args, completed, summary)
			}
		}
	}
}

func TestPiRedirectsCoverDiscoveryAndImport(t *testing.T) {
	for _, kind := range []string{"agent", "sessions", "precedence"} {
		t.Run(kind, func(t *testing.T) {
			fixture := t.TempDir()
			source := writePiSession(t, fixture)
			redirect := filepath.Join(fixture, ".pi", "agent")
			vars := map[string]string{"PI_CODING_AGENT_DIR": redirect}
			if kind != "agent" {
				vars["PI_CODING_AGENT_SESSION_DIR"] = filepath.Join(redirect, "sessions")
				delete(vars, "PI_CODING_AGENT_DIR")
				if kind == "precedence" {
					vars["PI_CODING_AGENT_DIR"] = filepath.Join(fixture, "empty-agent")
				}
			}
			adapter := Pi(testEnv(t.TempDir(), vars))
			found, err := adapter.Discover("/w")
			if err != nil || len(found) != 1 {
				t.Fatalf("redirect discovery: %v, %d sessions", err, len(found))
			}
			store := sessions.NewStore(sessions.StoreOptions{RootDir: filepath.Join(t.TempDir(), "store")})
			result, err := Import(store, adapter, transcriptID(source), ReadOptions{})
			if err != nil || result.Events == 0 {
				t.Fatalf("redirect import: %+v %v", result, err)
			}
		})
	}
}

func TestImportedActivityKeepsOutcomeUncertaintyThroughResume(t *testing.T) {
	for _, vendor := range []string{"family1", "pi", "codex", "codex-script"} {
		for _, outcome := range []string{"pending", "denied", "success"} {
			t.Run(vendor+"/"+outcome, func(t *testing.T) {
				var lines []string
				for _, call := range []struct{ id, name, arguments string }{
					{"command", "Bash", `{"command":"synthetic-command"}`},
					{"search", "Grep", `{"pattern":"synthetic-needle"}`},
				} {
					switch vendor {
					case "family1":
						pair := claudeToolLines(call.id, call.name, call.arguments, "permission denied", outcome == "denied")
						lines = append(lines, pair[0])
						if outcome != "pending" {
							lines = append(lines, pair[1])
						}
					case "pi":
						lines = append(lines, `{"type":"message","message":{"role":"assistant","content":[{"type":"toolCall","id":"`+call.id+`","name":"`+call.name+`","arguments":`+call.arguments+`}]}}`)
						if outcome != "pending" {
							failed := "false"
							if outcome == "denied" {
								failed = "true"
							}
							lines = append(lines, `{"type":"message","message":{"role":"toolResult","toolCallId":"`+call.id+`","toolName":"`+call.name+`","content":[{"type":"text","text":"permission denied"}],"isError":`+failed+`}}`)
						}
					default:
						kind, arguments := "function_call", call.arguments
						field := "arguments"
						if vendor == "codex-script" && call.id == "command" {
							kind, field, arguments = "custom_tool_call", "input", "synthetic-command"
						}
						encoded, _ := json.Marshal(arguments)
						lines = append(lines, `{"type":"response_item","payload":{"type":"`+kind+`","call_id":"`+call.id+`","name":"`+call.name+`","`+field+`":`+string(encoded)+`}}`)
						if outcome != "pending" {
							lines = append(lines, `{"type":"response_item","payload":{"type":"`+kind+`_output","call_id":"`+call.id+`","output":"permission denied"}}`)
						}
					}
				}
				file, err := os.Open(writeTranscript(t, lines...))
				if err != nil {
					t.Fatal(err)
				}
				defer file.Close()
				translate := translateFamily1
				if vendor == "pi" {
					translate = translatePi
				}
				if strings.HasPrefix(vendor, "codex") {
					translate = translateCodex
				}
				events, err := translate(file, ReadOptions{Cwd: t.TempDir()})
				if err != nil {
					t.Fatal(err)
				}
				summary := joinedSummary(t, events)
				completed := outcome == "success" && !strings.HasPrefix(vendor, "codex")
				for _, label := range []string{"Commands run:", "Searched for:"} {
					if strings.Contains(summary, label) != completed {
						t.Fatalf("unexpected completion claim %q: %s", label, summary)
					}
				}
				for _, request := range []string{"synthetic-command", "synthetic-needle"} {
					if !strings.Contains(summary, request) {
						t.Fatalf("request context lost: %s", summary)
					}
				}
				store := sessions.NewStore(sessions.StoreOptions{RootDir: filepath.Join(t.TempDir(), "sessions")})
				meta, err := store.Create(sessions.CreateInput{})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { store.Release(meta.SessionID) })
				if _, err := store.AppendEvents(meta.SessionID, events); err != nil {
					t.Fatal(err)
				}
				prepared, err := sessions.PrepareExec(sessions.PrepareExecOptions{Store: store, Resume: meta.SessionID})
				if err != nil {
					t.Fatal(err)
				}
				prompt := sessions.FormatExecPrompt("continue", prepared)
				if !completed && (strings.Contains(prompt, "Commands run:") || strings.Contains(prompt, "Searched for:")) {
					t.Fatalf("resume promoted an unconfirmed request: %s", prompt)
				}
				if !strings.Contains(prompt, "synthetic-command") || !strings.Contains(prompt, "synthetic-needle") {
					t.Fatalf("resume lost request context: %s", prompt)
				}
			})
		}
	}
}
