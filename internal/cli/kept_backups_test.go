package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Gitlawb/zero/internal/daemon/remote"
	"github.com/Gitlawb/zero/internal/dictation"
)

// plantKeptTxn writes the transaction marker both install sites read: the kind
// that site writes, the destination the copy was set aside for, and the sequence
// its directory name carries. Written here by hand rather than through either
// package, so the command is exercised against the on-disk format an operator
// actually has, not against a helper that could drift with it.
func plantKeptTxn(t *testing.T, dir, kind, dest string, seq int64) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(map[string]any{"kind": kind, "dest": dest, "seq": seq})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "txn"), payload, 0o600); err != nil {
		t.Fatal(err)
	}
}

// plantSTTKept writes a Kept backup of a dictation install: the engine copy an
// operator has to be able to find, because it is the only offline one.
func plantSTTKept(t *testing.T, root, dest string, seq int64, content string) string {
	t.Helper()
	name := fmt.Sprintf("%s.kept-%020d-seq", dest, seq)
	dir := filepath.Join(root, name)
	plantKeptTxn(t, dir, "dictation-promote", dest, seq)
	if err := os.MkdirAll(filepath.Join(dir, "install"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "install", "engine"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return name
}

// plantBundleKept writes a Kept backup of a bundle extract: a tree the client
// that uploaded it can send again, which is why the two sites cost differently.
func plantBundleKept(t *testing.T, dir, linkID string, seq int64, content string) string {
	t.Helper()
	name := fmt.Sprintf(".kept-%020d-seq", seq)
	path := filepath.Join(dir, name)
	plantKeptTxn(t, path, "bundle-extract", linkID, seq)
	if err := os.MkdirAll(filepath.Join(path, "backup"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "backup", "a.txt"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return name
}

func keptDeps(userConfigPath string) appDeps {
	return appDeps{userConfigPath: func() (string, error) { return userConfigPath, nil }}
}

// The listing is the only way a retained copy is found again: recovery never
// enumerates the Kept prefix and nothing reclaims one on its own. It has to name
// the site, because the same command reads two of them and the cost of keeping a
// copy is not the same at both.
func TestKeptBackupsListPrintsBothSites(t *testing.T) {
	userConfigPath := filepath.Join(t.TempDir(), "zero", "config.json")
	sttRoot := filepath.Join(filepath.Dir(userConfigPath), "stt")
	bundleDir := t.TempDir()

	sttName := plantSTTKept(t, sttRoot, "engine-a", 1, "engine-bytes")
	bundleName := plantBundleKept(t, bundleDir, "proj-1", 7, "tree")
	// Kept grammar, nothing attributing it: recovery's residue, which the
	// operator has to see beside the real backups to reclaim it by hand.
	unownedName := fmt.Sprintf(".kept-%020d-seq", 8)
	if err := os.MkdirAll(filepath.Join(bundleDir, unownedName), 0o700); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if code := runWithDeps([]string{"kept-backups", "list", "--bundle-dir", bundleDir}, &stdout, &stderr, keptDeps(userConfigPath)); code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}

	lines := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
		fields := strings.Fields(line)
		if len(fields) > 1 {
			lines[fields[1]] = line
		}
	}
	stt, ok := lines[sttName]
	if !ok {
		t.Fatalf("the dictation kept backup is missing from:\n%s", stdout.String())
	}
	for _, want := range []string{"stt", "dest=engine-a", "seq=1", "bytes="} {
		if !strings.Contains(stt, want) {
			t.Errorf("dictation line %q is missing %q", stt, want)
		}
	}
	if strings.Contains(stt, "bytes=0") {
		t.Errorf("dictation line %q reports no bytes for a copy that holds some", stt)
	}
	bundle, ok := lines[bundleName]
	if !ok {
		t.Fatalf("the bundle kept backup is missing from:\n%s", stdout.String())
	}
	for _, want := range []string{"bundle", "dest=proj-1", "seq=7", "bytes="} {
		if !strings.Contains(bundle, want) {
			t.Errorf("bundle line %q is missing %q", bundle, want)
		}
	}
	unowned, ok := lines[unownedName]
	if !ok {
		t.Fatalf("the unowned entry is missing from:\n%s", stdout.String())
	}
	if !strings.Contains(unowned, "unowned") {
		t.Errorf("unowned line %q does not say so", unowned)
	}
}

// Which site a removal lands on is decided by --bundle-dir alone. Getting that
// wrong deletes a copy at one site while the operator watches the other, so the
// name has to resolve at the site the flag names and nowhere else.
func TestKeptBackupsRemoveNamesTheSite(t *testing.T) {
	userConfigPath := filepath.Join(t.TempDir(), "zero", "config.json")
	sttRoot := filepath.Join(filepath.Dir(userConfigPath), "stt")
	bundleDir := t.TempDir()
	sttName := plantSTTKept(t, sttRoot, "engine-a", 1, "engine-bytes")
	bundleName := plantBundleKept(t, bundleDir, "proj-1", 7, "tree")

	var stdout, stderr bytes.Buffer
	if code := runWithDeps([]string{"kept-backups", "remove"}, &stdout, &stderr, keptDeps(userConfigPath)); code != exitUsage {
		t.Fatalf("remove with no name: exit = %d, want %d (stderr %s)", code, exitUsage, stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := runWithDeps([]string{"kept-backups", "remove", sttName}, &stdout, &stderr, keptDeps(userConfigPath)); code != 0 {
		t.Fatalf("remove at the dictation root: exit = %d, stderr = %s", code, stderr.String())
	}
	if _, err := os.Stat(filepath.Join(sttRoot, sttName)); !os.IsNotExist(err) {
		t.Errorf("the dictation kept backup should be gone, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(bundleDir, bundleName)); err != nil {
		t.Errorf("a removal with no --bundle-dir must not touch the bundle site: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	if code := runWithDeps([]string{"kept-backups", "remove", bundleName, "--bundle-dir", bundleDir}, &stdout, &stderr, keptDeps(userConfigPath)); code != 0 {
		t.Fatalf("remove at the bundle dir: exit = %d, stderr = %s", code, stderr.String())
	}
	if _, err := os.Stat(filepath.Join(bundleDir, bundleName)); !os.IsNotExist(err) {
		t.Errorf("the bundle kept backup should be gone, got %v", err)
	}
}

// The two sites cost differently and only the usage text says so, so a run that
// stops printing it leaves the operator deciding blind.
func TestKeptBackupsUsageNamesTheCostOfEachSite(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := runWithDeps([]string{"kept-backups", "-h"}, &stdout, &stderr, keptDeps("")); code != exitSuccess {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
	for _, want := range []string{"only offline copy", "upload"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("usage is missing %q:\n%s", want, stdout.String())
		}
	}
}

// The reclaim command is the only path a retained copy has off the disk, so a
// command that never appears in help or completions is a reclaim path that
// exists but cannot be found.
func TestKeptBackupsIsDiscoverable(t *testing.T) {
	var out bytes.Buffer
	if code := Run([]string{"--help"}, &out, &out); code != 0 {
		t.Fatalf("--help exit = %d", code)
	}
	if !strings.Contains(out.String(), "kept-backups") {
		t.Error("kept-backups is missing from the command list")
	}
	var comp bytes.Buffer
	if code := Run([]string{"completions", "bash"}, &comp, &comp); code != 0 {
		t.Fatalf("completions exit = %d", code)
	}
	if !strings.Contains(comp.String(), "kept-backups") {
		t.Error("kept-backups is missing from the completion tree")
	}
}

// The refusal and usage paths, over the command's own entry point. The two tests
// above only ever drive a removal that succeeds, which is why replacing the
// remove path's error handling with a discard left this package green: a refusal
// would have printed "Removed" and exited zero with nothing to say so. Every row
// here asserts the exit code, what did and did not reach stdout, and for the
// refusals that the copy is still on disk afterwards.
func TestKeptBackupsRefusalsAndUsage(t *testing.T) {
	// plantUnowned writes a directory carrying the Kept grammar and no marker:
	// recovery's residue, the entry the usage text promises remove refuses.
	plantUnowned := func(t *testing.T, dir string, seq int64) string {
		t.Helper()
		name := fmt.Sprintf(".kept-%020d-seq", seq)
		if err := os.MkdirAll(filepath.Join(dir, name, "backup"), 0o700); err != nil {
			t.Fatal(err)
		}
		return name
	}

	for _, tc := range []struct {
		name string
		// arrange plants whatever the row needs and returns the argument list.
		arrange  func(t *testing.T, sttRoot, bundleDir string) []string
		wantCode int
		wantOut  string
		// denyOut must not appear on stdout. "Removed" is the whole point on the
		// refusal rows: a refusal that prints it is a removal as far as the
		// operator can tell.
		denyOut string
		wantErr string
		// check runs after the command, for the rows whose claim is on disk.
		check func(t *testing.T, sttRoot, bundleDir string)
	}{
		{
			name: "remove refuses an unowned entry at the dictation root",
			arrange: func(t *testing.T, sttRoot, _ string) []string {
				return []string{"kept-backups", "remove", plantUnowned(t, sttRoot, 3)}
			},
			wantCode: exitCrash,
			denyOut:  "Removed",
			wantErr:  "[zero]",
			check: func(t *testing.T, sttRoot, _ string) {
				if _, err := os.Stat(filepath.Join(sttRoot, fmt.Sprintf(".kept-%020d-seq", 3))); err != nil {
					t.Errorf("a refused removal must leave the copy on disk: %v", err)
				}
			},
		},
		{
			name: "remove refuses an unowned entry at the bundle dir",
			arrange: func(t *testing.T, _, bundleDir string) []string {
				return []string{"kept-backups", "remove", plantUnowned(t, bundleDir, 4), "--bundle-dir", bundleDir}
			},
			wantCode: exitCrash,
			denyOut:  "Removed",
			wantErr:  "[zero]",
			check: func(t *testing.T, _, bundleDir string) {
				if _, err := os.Stat(filepath.Join(bundleDir, fmt.Sprintf(".kept-%020d-seq", 4))); err != nil {
					t.Errorf("a refused removal must leave the copy on disk: %v", err)
				}
			},
		},
		{
			// The flag can sit before the subcommand and can carry its value
			// with an '=', and both forms have to reach the same site: an
			// operator who names the bundle dir and is silently answered from
			// the dictation root is being shown the wrong disk.
			name: "remove lands on the bundle site through the equals form",
			arrange: func(t *testing.T, sttRoot, bundleDir string) []string {
				plantSTTKept(t, sttRoot, "engine-a", 1, "engine-bytes")
				name := plantBundleKept(t, bundleDir, "proj-1", 7, "tree")
				return []string{"kept-backups", "--bundle-dir=" + bundleDir, "remove", name}
			},
			wantCode: exitSuccess,
			wantOut:  "Removed",
			check: func(t *testing.T, sttRoot, bundleDir string) {
				if _, err := os.Stat(filepath.Join(bundleDir, fmt.Sprintf(".kept-%020d-seq", 7))); !os.IsNotExist(err) {
					t.Errorf("the bundle copy should be gone, got %v", err)
				}
				if _, err := os.Stat(filepath.Join(sttRoot, fmt.Sprintf("engine-a.kept-%020d-seq", 1))); err != nil {
					t.Errorf("a removal at the bundle site must not touch the dictation root: %v", err)
				}
			},
		},
		{
			name: "list lands on the bundle site through the equals form",
			arrange: func(t *testing.T, _, bundleDir string) []string {
				plantBundleKept(t, bundleDir, "proj-1", 7, "tree")
				return []string{"kept-backups", "--bundle-dir=" + bundleDir, "list"}
			},
			wantCode: exitSuccess,
			wantOut:  "bundle .kept-",
			denyOut:  "No kept backups.",
		},
		{
			name: "--bundle-dir with no value",
			arrange: func(t *testing.T, _, _ string) []string {
				return []string{"kept-backups", "list", "--bundle-dir"}
			},
			wantCode: exitUsage,
			wantErr:  "needs a directory",
		},
		{
			name: "unknown subcommand",
			arrange: func(t *testing.T, _, _ string) []string {
				return []string{"kept-backups", "purge"}
			},
			wantCode: exitUsage,
			wantErr:  "unknown subcommand",
		},
		{
			name: "no subcommand at all",
			arrange: func(t *testing.T, _, _ string) []string {
				return []string{"kept-backups"}
			},
			wantCode: exitUsage,
			wantErr:  "Usage:",
		},
		{
			name: "list with a stray argument",
			arrange: func(t *testing.T, _, _ string) []string {
				return []string{"kept-backups", "list", "proj-1"}
			},
			wantCode: exitUsage,
			wantErr:  "unexpected argument",
		},
		{
			name: "remove with two names",
			arrange: func(t *testing.T, _, _ string) []string {
				return []string{"kept-backups", "remove", "a", "b"}
			},
			wantCode: exitUsage,
			wantErr:  "usage: zero kept-backups remove",
		},
		{
			// An empty root has to say so. Printing nothing at all reads as a
			// command that did not run, and this listing is the only way a
			// retained copy is found again.
			name: "an empty root reports no backups",
			arrange: func(t *testing.T, _, bundleDir string) []string {
				return []string{"kept-backups", "list", "--bundle-dir", bundleDir}
			},
			wantCode: exitSuccess,
			wantOut:  "No kept backups.",
		},
		{
			// The dictation root does not exist yet before the first install,
			// which is a listing of nothing rather than an error.
			name: "a dictation root that was never created reports no backups",
			arrange: func(t *testing.T, sttRoot, _ string) []string {
				if err := os.RemoveAll(sttRoot); err != nil {
					t.Fatal(err)
				}
				return []string{"kept-backups", "list"}
			},
			wantCode: exitSuccess,
			wantOut:  "No kept backups.",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			userConfigPath := filepath.Join(t.TempDir(), "zero", "config.json")
			sttRoot := filepath.Join(filepath.Dir(userConfigPath), "stt")
			if err := os.MkdirAll(sttRoot, 0o700); err != nil {
				t.Fatal(err)
			}
			bundleDir := t.TempDir()
			args := tc.arrange(t, sttRoot, bundleDir)

			var stdout, stderr bytes.Buffer
			code := runWithDeps(args, &stdout, &stderr, keptDeps(userConfigPath))
			if code != tc.wantCode {
				t.Errorf("exit = %d, want %d (stdout %q, stderr %q)", code, tc.wantCode, stdout.String(), stderr.String())
			}
			if tc.wantOut != "" && !strings.Contains(stdout.String(), tc.wantOut) {
				t.Errorf("stdout %q is missing %q", stdout.String(), tc.wantOut)
			}
			if tc.denyOut != "" && strings.Contains(stdout.String(), tc.denyOut) {
				t.Errorf("stdout %q must not contain %q", stdout.String(), tc.denyOut)
			}
			if tc.wantErr != "" && !strings.Contains(stderr.String(), tc.wantErr) {
				t.Errorf("stderr %q is missing %q", stderr.String(), tc.wantErr)
			}
			if tc.check != nil {
				tc.check(t, sttRoot, bundleDir)
			}
		})
	}
}

// The listing is the only place a retained copy is visible, so a listing cut
// short must not report success: an operator who sees a clean exit believes
// they have seen every copy that exists.
func TestKeptBackupsListFailsOnAShortWrite(t *testing.T) {
	userConfigPath := filepath.Join(t.TempDir(), "config.json")
	root := filepath.Join(filepath.Dir(userConfigPath), "stt")
	plantSTTKept(t, root, "engine-a", 1, "the only offline copy")

	var stderr bytes.Buffer
	if code := runWithDeps([]string{"kept-backups", "list"}, shortWriter{}, &stderr, keptDeps(userConfigPath)); code == exitSuccess {
		t.Error("a listing that could not be written must not exit successfully")
	}
}

// shortWriter fails every write, standing in for a closed pipe or a full disk.
type shortWriter struct{}

func (shortWriter) Write([]byte) (int, error) { return 0, errors.New("short write") }

// A removal that completed and then failed to release its lock is not a removal
// that did not happen. Reporting it as a crash sends the operator to retry a
// copy that is already gone, and the retry answers with "nothing attributes this
// name", which describes an entry nobody can find because it no longer exists.
func TestKeptBackupsRemoveReportsACompletedRemovalThatCouldNotUnlock(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"dictation", fmt.Errorf("%w: engine-a: injected", dictation.ErrInstalledNotReleased)},
		{"bundle", fmt.Errorf("%w: proj-1: injected", remote.ErrCommittedNotReleased)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, warning := keptRemovalOutcome(tc.err)
			if code != exitSuccess {
				t.Errorf("exit = %d, want %d: the copy is gone", code, exitSuccess)
			}
			if warning == "" {
				t.Error("a cleanup failure the operator cannot see is one nobody fixes")
			}
		})
	}
	if code, _ := keptRemovalOutcome(errors.New("nothing attributes this name")); code == exitSuccess {
		t.Error("a removal that did not happen must not report success")
	}
	if code, warning := keptRemovalOutcome(nil); code != exitSuccess || warning != "" {
		t.Errorf("a clean removal = (%d, %q), want (%d, \"\")", code, warning, exitSuccess)
	}
}
