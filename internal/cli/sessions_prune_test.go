package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Gitlawb/zero/internal/sessions"
)

// pruneCLIFixture is a session root holding one session last written in 2020
// and one written now, neither held open, plus deps whose user config path is
// the returned file (which does not exist until a test writes it).
func pruneCLIFixture(t *testing.T) (string, string, appDeps) {
	t.Helper()
	root := t.TempDir()
	for _, session := range []struct {
		id string
		at func() time.Time
	}{
		{"old-session", func() time.Time { return time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC) }},
		{"recent-session", time.Now},
	} {
		store := sessions.NewStore(sessions.StoreOptions{RootDir: root, Now: session.at})
		if _, err := store.Create(sessions.CreateInput{SessionID: session.id, Title: "title of " + session.id}); err != nil {
			t.Fatalf("create %s: %v", session.id, err)
		}
		store.Release(session.id)
	}
	configPath := filepath.Join(t.TempDir(), "config.json")
	deps := appDeps{
		newSessionStore: func() *sessions.Store { return sessions.NewStore(sessions.StoreOptions{RootDir: root}) },
		userConfigPath:  func() (string, error) { return configPath, nil },
	}
	return root, configPath, deps
}

func runPruneCLI(t *testing.T, deps appDeps, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := runWithDeps(append([]string{"sessions"}, args...), &stdout, &stderr, deps)
	return code, stdout.String(), stderr.String()
}

func pruneSessionExists(t *testing.T, root, id string) bool {
	t.Helper()
	_, err := os.Stat(filepath.Join(root, id))
	return err == nil
}

func TestSessionsPruneDryRunListsWithoutRemoving(t *testing.T) {
	root, _, deps := pruneCLIFixture(t)
	code, stdout, stderr := runPruneCLI(t, deps, "prune", "--older-than", "30d", "--dry-run")
	if code != exitSuccess {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	for _, want := range []string{"Would remove 1 session", "old-session", "Dry run: nothing was removed."} {
		if !strings.Contains(stdout, want) {
			t.Errorf("output does not contain %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, "recent-session") {
		t.Errorf("the recent session was listed:\n%s", stdout)
	}
	if !pruneSessionExists(t, root, "old-session") {
		t.Fatal("a dry run removed the session")
	}
}

// A dry run removes nothing, so a session it could not check is not reported
// as one it could not remove.
func TestSessionsPruneDryRunReportsFailuresAsUnchecked(t *testing.T) {
	failed := []sessions.PruneEntry{{SessionID: "old-session", UpdatedAt: "2020-01-01T00:00:00Z", Reason: "the session disappeared while pruning"}}
	dryRun := formatPruneReport(sessions.PruneReport{Cutoff: "2026-08-27T00:00:00Z", DryRun: true, Failed: failed})
	if !strings.Contains(dryRun, "Could not check 1:") || strings.Contains(dryRun, "Could not remove") {
		t.Errorf("dry run report:\n%s", dryRun)
	}
	run := formatPruneReport(sessions.PruneReport{Cutoff: "2026-08-27T00:00:00Z", Failed: failed})
	if !strings.Contains(run, "Could not remove 1:") {
		t.Errorf("report:\n%s", run)
	}
}

func TestSessionsPruneRemovesOnlyOldSessions(t *testing.T) {
	root, _, deps := pruneCLIFixture(t)
	code, stdout, stderr := runPruneCLI(t, deps, "prune", "--older-than=30d")
	if code != exitSuccess {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	if !strings.Contains(stdout, "Removed 1 session") || !strings.Contains(stdout, "old-session") {
		t.Fatalf("output does not report the removal:\n%s", stdout)
	}
	if pruneSessionExists(t, root, "old-session") {
		t.Fatal("the old session is still there")
	}
	if !pruneSessionExists(t, root, "recent-session") {
		t.Fatal("the recent session was removed")
	}
}

func TestSessionsPruneReportsJSON(t *testing.T) {
	_, _, deps := pruneCLIFixture(t)
	code, stdout, stderr := runPruneCLI(t, deps, "prune", "--older-than", "720h", "--json")
	if code != exitSuccess {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	var report sessions.PruneReport
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("output is not a prune report: %v\n%s", err, stdout)
	}
	if len(report.Removed) != 1 || report.Removed[0].SessionID != "old-session" || report.DryRun {
		t.Fatalf("report = %+v", report)
	}
}

func TestSessionsPruneRefusesABadOrShortCutoff(t *testing.T) {
	root, _, deps := pruneCLIFixture(t)
	for _, tc := range []struct {
		value string
		want  string
	}{
		{"12h", "last 24 hours"},
		{"0d", "invalid --older-than"},
		{"soon", "invalid --older-than"},
		{"-5d", "invalid --older-than"},
	} {
		code, _, stderr := runPruneCLI(t, deps, "prune", "--older-than="+tc.value)
		if code != exitUsage || !strings.Contains(stderr, tc.want) {
			t.Errorf("--older-than %s: exit %d, stderr %q, want a usage error naming %q", tc.value, code, stderr, tc.want)
		}
	}
	if !pruneSessionExists(t, root, "old-session") {
		t.Fatal("a refused prune removed a session")
	}
}

func TestSessionsPruneNeedsACutoff(t *testing.T) {
	root, _, deps := pruneCLIFixture(t)
	code, _, stderr := runPruneCLI(t, deps, "prune")
	if code != exitUsage || !strings.Contains(stderr, "needs a cutoff") {
		t.Fatalf("exit %d, stderr %q, want a usage error asking for a cutoff", code, stderr)
	}
	if !pruneSessionExists(t, root, "old-session") {
		t.Fatal("prune without a cutoff removed a session")
	}
}

func TestSessionsPruneUsesTheUserRetentionSetting(t *testing.T) {
	root, configPath, deps := pruneCLIFixture(t)
	if err := os.WriteFile(configPath, []byte(`{"sessions":{"retentionDays":30}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runPruneCLI(t, deps, "prune")
	if code != exitSuccess {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	if !strings.Contains(stdout, "Removed 1 session") || pruneSessionExists(t, root, "old-session") {
		t.Fatalf("the user retention setting was not applied:\n%s", stdout)
	}
}

// A REPOSITORY CANNOT CHOOSE WHAT PRUNE DELETES. Sessions are the user's, not
// the project's, so sessions.retentionDays in a workspace's .zero/config.json
// is not a cutoff, even when prune runs from inside that workspace.
func TestSessionsPruneIgnoresRetentionInProjectConfig(t *testing.T) {
	root, _, deps := pruneCLIFixture(t)
	workspace := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workspace, ".zero"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, ".zero", "config.json"), []byte(`{"sessions":{"retentionDays":1}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	deps.getwd = func() (string, error) { return workspace, nil }
	t.Chdir(workspace)

	code, _, stderr := runPruneCLI(t, deps, "prune")
	if code != exitUsage || !strings.Contains(stderr, "needs a cutoff") {
		t.Fatalf("exit %d, stderr %q: the project's retention setting was used", code, stderr)
	}
	if !pruneSessionExists(t, root, "old-session") {
		t.Fatal("a project config setting removed a session")
	}
}

func TestSessionsPruneFlagsBelongToPrune(t *testing.T) {
	_, _, deps := pruneCLIFixture(t)
	for _, args := range [][]string{{"list", "--dry-run"}, {"list", "--older-than", "30d"}} {
		code, _, stderr := runPruneCLI(t, deps, args...)
		if code != exitUsage || !strings.Contains(stderr, "only valid for sessions prune") {
			t.Errorf("%v: exit %d, stderr %q", args, code, stderr)
		}
	}
}

// An empty cutoff is a usage error, never a fall back to sessions.retentionDays.
func TestSessionsPruneRejectsAnEmptyCutoff(t *testing.T) {
	root, configPath, deps := pruneCLIFixture(t)
	if err := os.WriteFile(configPath, []byte(`{"sessions":{"retentionDays":1}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"prune", "--older-than", ""},
		{"prune", "--older-than", "   "},
		{"prune", "--older-than="},
		{"list", "--older-than", ""},
	} {
		code, _, stderr := runPruneCLI(t, deps, args...)
		if code != exitUsage || !strings.Contains(stderr, "--older-than requires a value") {
			t.Errorf("%q: exit %d, stderr %q, want a usage error", args, code, stderr)
		}
	}
	if !pruneSessionExists(t, root, "old-session") {
		t.Fatal("an empty cutoff fell back to the retention setting and removed a session")
	}
}
