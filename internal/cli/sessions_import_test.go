package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/agentsessions"
	"github.com/Twigpine/zero/internal/sessions"
	"github.com/Twigpine/zero/internal/zerocommands"
)

func writeImportFixture(t *testing.T, path string, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func isolateAgentSessionRoots(t *testing.T, home string) {
	t.Helper()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("APPDATA", filepath.Join(home, "AppData", "Roaming"))
	t.Setenv("LOCALAPPDATA", filepath.Join(home, "AppData", "Local"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
}

func importUserRecord(t *testing.T, cwd, sessionID string) string {
	t.Helper()
	record, err := json.Marshal(map[string]any{
		"type":      "user",
		"cwd":       cwd,
		"sessionId": sessionID,
		"message":   map[string]any{"role": "user", "content": "hello"},
	})
	if err != nil {
		t.Fatalf("marshal import fixture: %v", err)
	}
	return string(record) + "\n"
}

func TestSessionListSanitizesPersistedModelMetadata(t *testing.T) {
	secret := "sk-ant-api03-" + strings.Repeat("A", 24)
	line := formatSessionSnapshotLine(zerocommands.SessionSnapshot{
		SessionID: "session-1",
		ModelID:   "claude\x1b[2K-opus\n" + secret,
		Tag:       "imported:codex:filename\x1b[2K\n" + secret,
	})
	if strings.Contains(line, "\x1b") || strings.Contains(line, secret) {
		t.Fatalf("unsafe model metadata reached the session list: %q", line)
	}
	if !strings.Contains(line, "model=claude-opus [REDACTED]") {
		t.Fatalf("session list lost safe model text: %q", line)
	}
	if !strings.Contains(line, "tag=imported:codex:filename [REDACTED]") {
		t.Fatalf("session list did not sanitize the raw imported provenance tag: %q", line)
	}
}

// THE HUMAN-READABLE SUMMARY IS ANOTHER PRODUCT'S BYTES ON A TERMINAL. The
// --json branch above it is structurally escaped and redacted; this branch
// printed the title and the cwd exactly as the foreign store wrote them, so an
// escape repainted the block and a title — usually the user's first prompt, and
// so the likeliest place for a pasted key — was shown verbatim. The listing
// alongside it already sanitized every field it drew, which is what made the
// omission easy to miss.
func TestImportSummarySanitizesTheTitleAndCwdItPrints(t *testing.T) {
	home := t.TempDir()
	writeImportFixture(t, filepath.Join(home, ".claude", "projects", "-w", "hostile.jsonl"),
		strings.Join([]string{
			`{"type":"user","cwd":"/w/\u001b[2Kmoved/proj","sessionId":"hostile","message":{"role":"user","content":"hi"}}`,
			`{"type":"ai-title","aiTitle":"rotate the key\nsk-ant-api03-AAAAAAAAAAAAAAAAAAAAAAAA","sessionId":"hostile"}`,
		}, "\n")+"\n")
	// Every root the discovery code resolves, not only the redirect variable:
	// claudeCodeRoot falls back to HOME, and the other three adapters have no
	// redirect at all, so leaving HOME alone would index the developer's own
	// transcripts.
	isolateAgentSessionRoots(t, home)

	store := sessions.NewStore(sessions.StoreOptions{RootDir: filepath.Join(t.TempDir(), "sessions")})
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	if code := runSessionsImport(store, "claude-code:hostile", sessionCommandOptions{}, stdout, stderr); code != exitSuccess {
		t.Fatalf("import exited %d: %s", code, stderr.String())
	}
	out := stdout.String()

	// Exact lines. "contains no escape" would pass for output that dropped the
	// fields entirely, and the point is that they stay readable.
	for _, want := range []string{
		"  title:        rotate the key [REDACTED]\n",
		"  cwd:          /w/moved/proj\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("import summary is missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "\x1b") {
		t.Errorf("a terminal escape reached the import summary:\n%q", out)
	}
	if strings.Contains(out, "sk-ant-api03-") {
		t.Errorf("a credential-shaped title reached the import summary:\n%q", out)
	}
}

// The warning is built from the recorded cwd, which is the same foreign bytes
// one sentence later — CodeRabbit named it separately for that reason. Called
// directly rather than through the command so the assertion is on the sentence
// itself and cannot be satisfied by an earlier guard declining the input.
func TestImportWorkspaceWarningSanitizesTheRecordedPath(t *testing.T) {
	warning := importWorkspaceWarning("/elsewhere/\x1b[31mred\x1b[0m/proj")
	const want = "Note: this session ran in /elsewhere/red/proj, not the current directory.\n" +
		"      Paths mentioned in it refer to that tree."
	if warning != want {
		t.Errorf("warning = %q, want %q", warning, want)
	}

	// The comparison behind it still runs on the recorded path, so a session
	// imported from this very directory stays silent.
	working, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if got := importWorkspaceWarning(working); got != "" {
		t.Errorf("a session recorded in the current directory warned anyway: %q", got)
	}
}

func TestImportWorkspaceWarningUsesWindowsCaseInsensitivePaths(t *testing.T) {
	if got := importWorkspaceWarningForOS(`C:\Work\Temp\..\Project`, `c:\work\project`, "windows"); got != "" {
		t.Fatalf("Windows-equivalent paths produced a warning: %q", got)
	}
	if got := importWorkspaceWarningForOS(`/Work/Other`, `/work/project`, "windows"); got == "" {
		t.Fatal("different Windows paths produced no warning")
	}
	if got := importWorkspaceWarningForOS(`C:`, `C:\`, "windows"); got == "" {
		t.Fatal("drive-relative and drive-rooted Windows paths compared equal")
	}
	if got := importWorkspaceWarningForOS(`\\server\share`, `\server\share`, "windows"); got == "" {
		t.Fatal("UNC and root-relative Windows paths compared equal")
	}
}

func TestImportWorkspaceWarningRecognizesNativeDirectoryAliases(t *testing.T) {
	realDir := t.TempDir()
	alias := filepath.Join(t.TempDir(), "project-alias")
	if err := os.Symlink(realDir, alias); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if got := importWorkspaceWarningForOS(realDir, alias, runtime.GOOS); got != "" {
		t.Fatalf("same native directory through symlink produced warning: %q", got)
	}
	if got := importWorkspaceWarningForOS(realDir, t.TempDir(), runtime.GOOS); got == "" {
		t.Fatal("distinct native directories produced no warning")
	}
}

func TestRunSessionsDiscoverFiltersAgentAndWritesJSON(t *testing.T) {
	home := t.TempDir()
	workspace := filepath.Join(home, "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	writeImportFixture(t, filepath.Join(home, ".claude", "projects", "-workspace", "claude.jsonl"),
		importUserRecord(t, workspace, "claude"))
	isolateAgentSessionRoots(t, home)
	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(workspace); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(previous) })

	for _, test := range []struct {
		agent string
		want  int
	}{{agent: "claude-code", want: 1}, {agent: "codex", want: 0}} {
		stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
		code := runSessionsDiscover(sessionCommandOptions{agent: test.agent, json: true}, stdout, stderr)
		if code != exitSuccess {
			t.Fatalf("discover --agent %s exited %d: %s", test.agent, code, stderr.String())
		}
		var found []discoveredSnapshot
		if err := json.Unmarshal(stdout.Bytes(), &found); err != nil {
			t.Fatalf("discover JSON did not decode: %v\n%s", err, stdout.String())
		}
		if len(found) != test.want {
			t.Fatalf("discover --agent %s returned %d rows, want %d: %+v", test.agent, len(found), test.want, found)
		}
	}
}

func TestSessionJSONCommandsNormalizeBidiFormatCharacters(t *testing.T) {
	home := t.TempDir()
	workspace := filepath.Join(home, "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	record, err := json.Marshal(map[string]any{
		"type": "user", "cwd": workspace, "gitBranch": "fix/\u202ebidi", "sessionId": "bidi",
		"message": map[string]any{"role": "user", "model": "model-\u202eunsafe", "content": "deploy \u202egnp.txt.exe"},
	})
	if err != nil {
		t.Fatal(err)
	}
	writeImportFixture(t, filepath.Join(home, ".claude", "projects", "-workspace", "bidi.jsonl"), string(record)+"\n")
	isolateAgentSessionRoots(t, home)
	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(workspace); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(previous) })

	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	if code := runSessionsDiscover(sessionCommandOptions{json: true}, stdout, stderr); code != exitSuccess {
		t.Fatalf("discover exited %d: %s", code, stderr.String())
	}
	if strings.ContainsRune(stdout.String(), '\u202e') {
		t.Fatalf("discover JSON retained a terminal-active format character: %q", stdout.String())
	}
	var discovered []discoveredSnapshot
	if err := json.Unmarshal(stdout.Bytes(), &discovered); err != nil || len(discovered) != 1 {
		t.Fatalf("decode discover JSON: rows=%d err=%v output=%s", len(discovered), err, stdout.String())
	}
	if discovered[0].Title != "deploy gnp.txt.exe" || discovered[0].GitBranch != "fix/bidi" || discovered[0].ModelID != "model-unsafe" {
		t.Fatalf("discover JSON lost safe text while normalizing: %+v", discovered[0])
	}

	store := sessions.NewStore(sessions.StoreOptions{RootDir: filepath.Join(t.TempDir(), "sessions")})
	stdout.Reset()
	stderr.Reset()
	if code := runSessionsImport(store, "claude-code:bidi", sessionCommandOptions{json: true}, stdout, stderr); code != exitSuccess {
		t.Fatalf("import exited %d: %s", code, stderr.String())
	}
	if strings.ContainsRune(stdout.String(), '\u202e') {
		t.Fatalf("import JSON retained a terminal-active format character: %q", stdout.String())
	}
	var imported struct {
		Title  string             `json:"title"`
		Source discoveredSnapshot `json:"source"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &imported); err != nil {
		t.Fatalf("decode import JSON: %v\n%s", err, stdout.String())
	}
	if imported.Title != "deploy gnp.txt.exe" || imported.Source.GitBranch != "fix/bidi" || imported.Source.ModelID != "model-unsafe" {
		t.Fatalf("import JSON lost safe text while normalizing: %+v", imported)
	}
}

func TestRunSessionsImportRejectsEmptyTranslationsWithoutDurableState(t *testing.T) {
	home := t.TempDir()
	writeImportFixture(t, filepath.Join(home, ".claude", "projects", "-w", "empty.jsonl"),
		`{"type":"user","cwd":"/w","sessionId":"empty","message":{"role":"user","content":""}}`+"\n")
	isolateAgentSessionRoots(t, home)
	store := sessions.NewStore(sessions.StoreOptions{RootDir: filepath.Join(t.TempDir(), "sessions")})

	for attempt := 1; attempt <= 2; attempt++ {
		stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
		if code := runSessionsImport(store, "claude-code:empty", sessionCommandOptions{}, stdout, stderr); code != exitCrash {
			t.Fatalf("attempt %d exited %d, want %d; stdout=%q stderr=%q", attempt, code, exitCrash, stdout.String(), stderr.String())
		}
		if !strings.Contains(stderr.String(), "no importable content") {
			t.Fatalf("attempt %d did not explain the empty translation: %q", attempt, stderr.String())
		}
		metas, err := store.List()
		if err != nil {
			t.Fatal(err)
		}
		if len(metas) != 0 {
			t.Fatalf("attempt %d left durable empty sessions: %+v", attempt, metas)
		}
	}
}

func TestRunSessionsImportReportsUsageAndReadFailures(t *testing.T) {
	home := t.TempDir()
	isolateAgentSessionRoots(t, home)
	store := sessions.NewStore(sessions.StoreOptions{RootDir: filepath.Join(t.TempDir(), "sessions")})

	for _, test := range []struct {
		ref      string
		wantCode int
	}{{ref: "unknown:id", wantCode: exitUsage}, {ref: "claude-code:missing", wantCode: exitCrash}} {
		stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
		if code := runSessionsImport(store, test.ref, sessionCommandOptions{}, stdout, stderr); code != test.wantCode {
			t.Fatalf("import %s exited %d, want %d; stderr=%q", test.ref, code, test.wantCode, stderr.String())
		}
		if stderr.Len() == 0 {
			t.Fatalf("import %s failed without an error message", test.ref)
		}
	}
}

// A SESSION IMPORTED BY AN EARLIER BUILD STILL HOLDS THE RAW BYTES. Import only
// began sanitizing what it stores at this change, so the summary has to clean
// its own output rather than trust the record — and through runSessionsImport
// that is unobservable, because the session it just wrote is already clean. This
// supplies the record an older build left behind.
func TestTheImportSummaryCleansAStoredTitleAndCwdItDidNotWrite(t *testing.T) {
	result := agentsessions.ImportResult{
		Session: sessions.Metadata{
			SessionID: "zero_1",
			Title:     "rotate the key\nsk-ant-api03-AAAAAAAAAAAAAAAAAAAAAAAA",
			Cwd:       "/w/\x1b[2Kmoved/proj",
		},
		Events: 3,
		Source: agentsessions.ForeignSession{Agent: "claude-code", ID: "abc\x1b[2Kdef"},
	}
	got := importSummaryLines(result)
	want := []string{
		"Imported claude-code session abcdef",
		"",
		"  zero session: zero_1",
		"  title:        rotate the key [REDACTED]",
		"  cwd:          /w/moved/proj",
		"  events:       3",
	}
	if len(got) != len(want) {
		t.Fatalf("summary has %d lines, want %d: %q", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// An empty title and cwd still read as "(none)" rather than as a blank column —
// sanitizing must not have swallowed the placeholder.
func TestTheImportSummaryStillSaysNoneForAnEmptyTitleAndCwd(t *testing.T) {
	got := importSummaryLines(agentsessions.ImportResult{
		Session: sessions.Metadata{SessionID: "zero_1"},
		Source:  agentsessions.ForeignSession{Agent: "codex", ID: "x"},
	})
	for _, want := range []string{"  title:        (none)", "  cwd:          (none)"} {
		found := false
		for _, line := range got {
			if line == want {
				found = true
			}
		}
		if !found {
			t.Errorf("summary is missing %q: %q", want, got)
		}
	}
}
