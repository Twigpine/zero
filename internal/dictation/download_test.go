package dictation

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Gitlawb/zero/internal/lockutil"
)

// Tiny tar.bz2 fixtures (generated in-repo): the engine has top/bin/sherpa-onnx-
// offline + server; the model has top/tokens.txt.
const (
	engineTarB64 = "QlpoOTFBWSZTWW+fEfAAAW1/hMGRAIBAA/+AAgAgRH9v3+AAAQCoMAEYAYA00NGjEZANABoYYA00NGjEZANABoYIpE0TRoTGkA2poGgxqepl1H6w5anW3tY2OggUi1AW/d29pO0GLUoOtnQsrGFNowIEh0cImtQ9gyHqYH7unm+/wZSLXsBuBDDCfHiahCsGEgGL9/pUG+RQZo76xlaDIGKWDErPKuyhXYOlzJMG3iNa56kiluctzUqgZ2A1AGwgcB3jtB5PB7pYYlDHH8OvOznM6B6MHPRqa7sTE1vwDBc0LjmG4ah5F+YYaXHTZpGmcemWW1yMh1adhdrB6A3nAQP8XckU4UJBvnxHwA=="
	engineSHA    = "97b5cb6a417705860f8940e1bf9ffec8cc60e8666a0d228cd60f1de7607d503d"
	modelTarB64  = "QlpoOTFBWSZTWTtAoX4AANp/hMOQBIBAAf+AAAIQBHoJ3mAAAQAIMAC5sG1SNMhNqGmQwIyeKEagam1DQ0AAABFRT0T0mg0aeoDQHpH7Nzrs24wsbVW8hMQHClNI0LuLhKMGY4sEQi6N2qQWP1id8o1KtprGd4hll4uQkMGA1nFMXAwohEZOnIMIPQKQl2k0GbSGwkC0lJb2IWOQ57ETYbmxFGspaRp1QyT6q4sKj855st7GzOGj5eIwmWt6wRajEQl/F3JFOFCQO0Chfg=="
	modelSHA     = "16c6b1dcb40f6b21c0de70bc1a8edab95375e4de90dac7c076fd2cc7a34fc59a"
)

// fakeReleaseServer serves the GitHub release API + asset bytes for the engine
// and model, exercising resolveAsset + downloadVerifyExtract against a real HTTP
// round-trip (with the real coder/websocket-free download path).
func fakeReleaseServer(t *testing.T, engineDigest, modelDigest string) *httptest.Server {
	t.Helper()
	engineBytes, _ := base64.StdEncoding.DecodeString(engineTarB64)
	modelBytes, _ := base64.StdEncoding.DecodeString(modelTarB64)
	mux := http.NewServeMux()
	base := ""
	release := func(assetName, digest, dlPath string) string {
		body, _ := json.Marshal(map[string]any{
			"tag_name": "test",
			"assets": []map[string]any{{
				"name":                 assetName,
				"browser_download_url": base + dlPath,
				"digest":               "sha256:" + digest,
				"size":                 123,
			}},
		})
		return string(body)
	}
	var srv *httptest.Server
	// Any release tag under this prefix resolves: asr-models → the model, every
	// other tag → the engine (so the default-version pinned-check test works too).
	mux.HandleFunc("/repos/k2-fsa/sherpa-onnx/releases/tags/", func(w http.ResponseWriter, r *http.Request) {
		base = srv.URL
		if strings.HasSuffix(r.URL.Path, "/asr-models") {
			fmt.Fprint(w, release(modelAssetName, modelDigest, "/model.tar.bz2"))
			return
		}
		fmt.Fprint(w, release("sherpa-onnx-test-linux-x64-shared-no-tts.tar.bz2", engineDigest, "/engine.tar.bz2"))
	})
	mux.HandleFunc("/engine.tar.bz2", func(w http.ResponseWriter, r *http.Request) { w.Write(engineBytes) })
	mux.HandleFunc("/model.tar.bz2", func(w http.ResponseWriter, r *http.Request) { w.Write(modelBytes) })
	srv = httptest.NewServer(mux)
	base = srv.URL
	t.Cleanup(srv.Close)
	return srv
}

func TestResolveEnginePathsHonorsTargetPlatform(t *testing.T) {
	// The engine binary name follows the TARGET platform, not the host: a Windows
	// engine has ".exe", a Linux one doesn't. Resolution must find each regardless
	// of the OS the test runs on (this is what makes the cross-platform download
	// test pass on Windows CI).
	for _, tc := range []struct {
		name    string
		target  bool
		binName string
	}{
		{"linux target", false, "sherpa-onnx-offline"},
		{"windows target", true, "sherpa-onnx-offline.exe"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			binDir := filepath.Join(dir, "bin")
			if err := os.MkdirAll(binDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(binDir, tc.binName), []byte("x"), 0o755); err != nil {
				t.Fatal(err)
			}
			bin, _ := resolveEnginePaths(dir, tc.target)
			if !fileExists(bin) {
				t.Errorf("resolveEnginePaths(target-windows=%v) = %q; not found", tc.target, bin)
			}
			if !strings.HasSuffix(bin, tc.binName) {
				t.Errorf("bin = %q, want suffix %q", bin, tc.binName)
			}
		})
	}
}

func TestEnsureLocalEngineDownloadsAndExtracts(t *testing.T) {
	srv := fakeReleaseServer(t, engineSHA, modelSHA)
	dest := t.TempDir()
	var stages []string
	comp, err := EnsureLocalEngine(context.Background(), DownloadOptions{
		DestRoot:      dest,
		EngineVersion: "test",
		APIBase:       srv.URL,
		platformKey:   "linux-amd64",
		skipPinned:    true,
		Progress:      func(s string) { stages = append(stages, s) },
	})
	if err != nil {
		t.Fatalf("EnsureLocalEngine: %v", err)
	}
	if !fileExists(comp.BinaryPath) || !strings.HasSuffix(comp.BinaryPath, "sherpa-onnx-offline") {
		t.Errorf("binary path wrong: %q", comp.BinaryPath)
	}
	if !fileExists(comp.ServerPath) {
		t.Errorf("server path missing: %q", comp.ServerPath)
	}
	if !fileExists(filepath.Join(comp.ModelPath, "tokens.txt")) {
		t.Errorf("model tokens.txt missing under %q", comp.ModelPath)
	}
	// The extracted binary keeps its exec bit — Unix only; Windows has no
	// executable permission bit, so os.FileMode never reports 0o100 there.
	if runtime.GOOS != "windows" {
		if info, err := os.Stat(comp.BinaryPath); err == nil && info.Mode()&0o100 == 0 {
			t.Error("engine binary should be executable")
		}
	}
	if len(stages) == 0 {
		t.Error("expected progress stages")
	}

	// Idempotent: a second call reuses the extracted engine + model and downloads
	// NOTHING (no progress stages) — the fix for re-downloading an already-present
	// engine whose binary lives in the tarball's flattened subdir.
	var stages2 []string
	comp2, err := EnsureLocalEngine(context.Background(), DownloadOptions{
		DestRoot: dest, EngineVersion: "test", APIBase: srv.URL, platformKey: "linux-amd64", skipPinned: true,
		Progress: func(s string) { stages2 = append(stages2, s) },
	})
	if err != nil || comp2.BinaryPath != comp.BinaryPath {
		t.Errorf("second call should reuse: %v / %q vs %q", err, comp2.BinaryPath, comp.BinaryPath)
	}
	if len(stages2) != 0 {
		t.Errorf("a fully-cached second call must not download/extract anything, got stages: %v", stages2)
	}
}

func TestEnsureLocalEngineRejectsChecksumMismatch(t *testing.T) {
	// Serve the engine with a wrong digest → verification must refuse to extract.
	srv := fakeReleaseServer(t, strings.Repeat("0", 64), modelSHA)
	_, err := EnsureLocalEngine(context.Background(), DownloadOptions{
		DestRoot: t.TempDir(), EngineVersion: "test", APIBase: srv.URL, platformKey: "linux-amd64", skipPinned: true,
	})
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("expected checksum-mismatch error, got %v", err)
	}
}

func TestEnsureLocalEnginePinnedCrossCheckRefusesChangedRelease(t *testing.T) {
	// Default version + a digest that doesn't match the pinned value → refuse,
	// even though the download itself would verify against the API digest.
	srv := fakeReleaseServer(t, engineSHA, modelSHA)
	_, err := EnsureLocalEngine(context.Background(), DownloadOptions{
		DestRoot:      t.TempDir(),
		EngineVersion: DefaultSherpaVersion, // triggers the pinned cross-check
		APIBase:       srv.URL,
		platformKey:   "linux-amd64",
		// skipPinned false: the pinned digest for linux-amd64 != engineSHA fixture.
	})
	if err == nil || !strings.Contains(err.Error(), "pinned known-good") {
		t.Fatalf("expected pinned cross-check refusal, got %v", err)
	}
}

func TestResolveAssetMissingDigestAllowed(t *testing.T) {
	// An older asset with no API digest resolves fine (verification against a
	// pinned value happens later); resolveAsset no longer refuses here.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"assets": []map[string]any{{"name": "x-linux-x64-shared-no-tts.tar.bz2", "browser_download_url": "http://x", "digest": ""}},
		})
	}))
	defer srv.Close()
	asset, err := resolveAsset(context.Background(), http.DefaultClient, srv.URL, "test", "x-", "linux-x64-shared-no-tts.tar.bz2")
	if err != nil {
		t.Fatalf("resolveAsset should tolerate a missing digest, got %v", err)
	}
	if asset.sha256 != "" {
		t.Errorf("expected empty digest, got %q", asset.sha256)
	}
}

func TestUnverifiableDownloadRefused(t *testing.T) {
	// No API digest AND no pinned digest → refuse to download/extract.
	srv := fakeReleaseServer(t, "", "") // both assets served with empty digest
	_, err := EnsureLocalEngine(context.Background(), DownloadOptions{
		DestRoot: t.TempDir(), EngineVersion: "test", APIBase: srv.URL, platformKey: "linux-amd64", skipPinned: true,
	})
	if err == nil || !strings.Contains(err.Error(), "unverifiable") {
		t.Fatalf("expected refusal of an unverifiable download, got %v", err)
	}
}

func TestProgressReaderReportsPercent(t *testing.T) {
	data := make([]byte, 1000)
	var last string
	pr := &progressReader{r: bytesReader(data), total: 1000, label: "Engine", report: func(s string) { last = s }, lastPct: -1}
	buf := make([]byte, 100)
	for {
		if _, err := pr.Read(buf); err != nil {
			break
		}
	}
	if !strings.Contains(last, "Engine") || !strings.Contains(last, "100%") {
		t.Errorf("expected a final 'Engine 100%%' report, got %q", last)
	}
}

func bytesReader(b []byte) *sliceReader { return &sliceReader{b: b} }

type sliceReader struct {
	b   []byte
	pos int
}

func (s *sliceReader) Read(p []byte) (int, error) {
	if s.pos >= len(s.b) {
		return 0, io.EOF
	}
	n := copy(p, s.b[s.pos:])
	s.pos += n
	return n, nil
}

func TestUnsupportedPlatformSetupError(t *testing.T) {
	_, err := EnsureLocalEngine(context.Background(), DownloadOptions{DestRoot: t.TempDir(), platformKey: "plan9-mips"})
	var setupErr *SetupError
	if !errors.As(err, &setupErr) {
		t.Fatalf("want *SetupError for unsupported platform, got %v", err)
	}
}

func stagedTree(t *testing.T, dir, content string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "engine"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestPromoteStagedDirReplacesAPreviousInstall(t *testing.T) {
	root := t.TempDir()
	dest := filepath.Join(root, "engine-dir")
	stagedTree(t, dest, "old")
	stage := stagedTree(t, filepath.Join(root, "stage"), "new")

	if err := promoteStagedDir(lockFor(t, dest), stage, dest, "engine", nil); err != nil {
		t.Fatalf("promote: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dest, "engine"))
	if err != nil || string(got) != "new" {
		t.Fatalf("engine = %q, err %v, want %q", got, err, "new")
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".previous-") {
			t.Errorf("the set-aside install should be cleaned up, found %s", e.Name())
		}
	}
}

func TestPromoteStagedDirWorksWithNoPreviousInstall(t *testing.T) {
	root := t.TempDir()
	dest := filepath.Join(root, "engine-dir")
	stage := stagedTree(t, filepath.Join(root, "stage"), "new")

	if err := promoteStagedDir(lockFor(t, dest), stage, dest, "engine", nil); err != nil {
		t.Fatalf("promote: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dest, "engine"))
	if err != nil || string(got) != "new" {
		t.Fatalf("engine = %q, err %v, want %q", got, err, "new")
	}
}

// The old code deleted the previous install before it had anything to put in
// its place, so a failed promotion left the user with no engine at all.
func TestPromoteStagedDirKeepsThePreviousInstallWhenPromotionFails(t *testing.T) {
	root := t.TempDir()
	dest := filepath.Join(root, "engine-dir")
	stagedTree(t, dest, "old")
	stage := stagedTree(t, filepath.Join(root, "stage"), "new")

	// Fail only the promotion, so the restore that follows it still runs. The
	// set-aside rename goes through the same seam now, and it has to stay real
	// or there is no previous install to put back.
	real := holderFS.rename
	holderFS.rename = func(from, to string) error {
		if from == stage {
			return errors.New("injected promotion failure")
		}
		return real(from, to)
	}
	t.Cleanup(func() { holderFS.rename = real })

	if err := promoteStagedDir(lockFor(t, dest), stage, dest, "engine", nil); err == nil {
		t.Fatal("a failed promotion must be reported")
	}
	got, err := os.ReadFile(filepath.Join(dest, "engine"))
	if err != nil {
		t.Fatalf("the previous install was not restored: %v", err)
	}
	if string(got) != "old" {
		t.Fatalf("engine = %q, want the previous %q", got, "old")
	}
}

// If the restore fails too, the only remaining copy must survive the cleanup.
func TestPromoteStagedDirKeepsTheSetAsideCopyWhenRestoreAlsoFails(t *testing.T) {
	root := t.TempDir()
	dest := filepath.Join(root, "engine-dir")
	stagedTree(t, dest, "old")
	stage := stagedTree(t, filepath.Join(root, "stage"), "new")

	// The promotion and the restore are the two renames into dest; the set-aside
	// that fills the holder stays real, or there is no retained copy to assert on.
	real := holderFS.rename
	holderFS.rename = func(from, to string) error {
		if to == dest {
			return errors.New("injected rename failure")
		}
		return real(from, to)
	}
	t.Cleanup(func() { holderFS.rename = real })

	err := promoteStagedDir(lockFor(t, dest), stage, dest, "engine", nil)
	if err == nil {
		t.Fatal("a failed promotion must be reported")
	}
	if !strings.Contains(err.Error(), "previous install left in") {
		t.Errorf("the error should name the retained copy, got: %v", err)
	}
	entries, readErr := os.ReadDir(root)
	if readErr != nil {
		t.Fatal(readErr)
	}
	found := false
	for _, e := range entries {
		if !strings.Contains(e.Name(), ".previous-") {
			continue
		}
		got, readErr := os.ReadFile(filepath.Join(root, e.Name(), "install", "engine"))
		if readErr == nil && string(got) == "old" {
			found = true
		}
	}
	if !found {
		t.Error("the previous install was deleted along with the holder dir")
	}
}

// A process stop between the two renames in promoteStagedDir leaves destDir
// absent and the only usable install in a .previous-* holder. Nothing else knows
// about that holder, so without a repair the engine is re-downloaded and a host
// that cannot reach the network stays without dictation despite having a copy.
func TestRestoreInterruptedPromotionPutsTheInstallBack(t *testing.T) {
	root := t.TempDir()
	dest := filepath.Join(root, "engine-1.2.3-linux-x64")
	holder := plantHolder(t, dest, 1, "kept", false)

	restoreInterruptedPromotion(lockFor(t, dest), dest, testPublished, nil)

	got, err := os.ReadFile(filepath.Join(dest, "engine"))
	if err != nil {
		t.Fatalf("the interrupted promotion was not restored: %v", err)
	}
	if string(got) != "kept" {
		t.Fatalf("restored engine = %q, want %q", got, "kept")
	}
	if _, err := os.Stat(holder); !os.IsNotExist(err) {
		t.Errorf("the holder should be cleared after a restore, got %v", err)
	}
}

// A promotion that published its install but could not remove the holder leaves
// a complete copy of the OLD install beside a live one, and the commit flag
// inside it is the evidence that a publish actually landed over it. Only that
// flag licenses the delete: a copy with no flag may still be the last usable
// one, so it is parked rather than reaped, and a destination that merely EXISTS
// proves nothing about either.
func TestRestoreInterruptedPromotionDeletesOnlyCommittedHolders(t *testing.T) {
	root := t.TempDir()
	dest := filepath.Join(root, "engine-1.2.3-linux-x64")
	stagedTree(t, dest, "new")
	superseded := plantHolder(t, dest, 100, "old", true)
	uncommitted := plantHolder(t, dest, 50, "older", false)
	// One handle for both passes: the lock is per open file description, so a
	// second acquire in this process contends with the first rather than nests.
	txn := lockFor(t, dest)

	var reported []string
	restoreInterruptedPromotion(txn, dest, testPublished, reporterFor(&reported))

	// The live install is never touched. This is the assertion that matters most.
	got, err := os.ReadFile(filepath.Join(dest, "engine"))
	if err != nil || string(got) != "new" {
		t.Fatalf("the live install must be left alone: got %q err %v", got, err)
	}
	if _, err := os.Stat(superseded); !os.IsNotExist(err) {
		t.Errorf("a copy proven superseded by its commit flag should be reaped, got %v", err)
	}
	parked := keptName(t, dest, uncommitted)
	if _, err := os.Stat(uncommitted); !os.IsNotExist(err) {
		t.Errorf("the uncommitted copy should have left the scanned prefix, got %v", err)
	}
	kept, err := os.ReadFile(filepath.Join(parked, "install", "engine"))
	if err != nil || string(kept) != "older" {
		t.Fatalf("the uncommitted copy must survive the park intact: %q err %v", kept, err)
	}
	assertReports(t, reported, parked)

	// No memory: the parked copy is under a prefix the scan does not read, so a
	// second pass has nothing left to decide.
	reported = nil
	restoreInterruptedPromotion(txn, dest, testPublished, reporterFor(&reported))
	if _, err := os.Stat(filepath.Join(parked, "install", "engine")); err != nil {
		t.Errorf("the second pass must leave the parked copy alone: %v", err)
	}
	if len(reported) != 0 {
		t.Errorf("a pass with nothing beside the destination should report nothing, got %v", reported)
	}
}

// A copy parked by recovery is permanent: a later install that publishes over
// the destination sets the CURRENT install aside, never a Kept backup, so it can
// never write the commit flag that would license removing one. Nothing but the
// operator takes a Kept backup off disk.
func TestRestoreInterruptedPromotionKeepsAParkedCopyAfterALaterInstall(t *testing.T) {
	root := t.TempDir()
	dest := filepath.Join(root, "engine-1.2.3-linux-x64")
	stagedTree(t, dest, "new")
	uncommitted := plantHolder(t, dest, 50, "older", false)
	txn := lockFor(t, dest)

	restoreInterruptedPromotion(txn, dest, testPublished, nil)
	parked := keptName(t, dest, uncommitted)
	if _, err := os.Stat(filepath.Join(parked, "install", "engine")); err != nil {
		t.Fatalf("seeding a parked copy: %v", err)
	}

	// A real later install over the same destination.
	stage := stagedTree(t, filepath.Join(root, "stage"), "newest")
	if err := promoteStagedDir(txn, stage, dest, "engine", nil); err != nil {
		t.Fatalf("the later install failed: %v", err)
	}
	var reported []string
	restoreInterruptedPromotion(txn, dest, testPublished, reporterFor(&reported))

	// Silence is the assertion that the Kept prefix is outside the scan: a pass
	// that enumerated it would have to rule on the copy, and every ruling it
	// could reach (park, delete, restore) says so in the report.
	if len(reported) != 0 {
		t.Errorf("recovery must not enumerate the Kept prefix, it reported %v", reported)
	}
	kept, err := os.ReadFile(filepath.Join(parked, "install", "engine"))
	if err != nil || string(kept) != "older" {
		t.Errorf("a Kept backup must survive a later successful install: %q err %v", kept, err)
	}
	live, err := os.ReadFile(filepath.Join(dest, "engine"))
	if err != nil || string(live) != "newest" {
		t.Errorf("the later install should be live: %q err %v", live, err)
	}
}

// A destDir that is merely NOT EMPTY is no evidence a promotion published there.
// An empty husk and a half-populated one are the same thing to recovery, and a
// copy beside one may be the last usable install there is. So the destination is
// not what wins: the usable copy is restored over the husk, and the husk itself
// is set aside rather than deleted, which is how an offline caller stops being
// stuck beside an install it cannot use.
func TestRestoreInterruptedPromotionReplacesADestThatIsNotAUsableInstall(t *testing.T) {
	for _, tc := range []struct {
		name    string
		seed    func(t *testing.T, dest string)
		usable  usability
		content string
	}{
		{
			name:    "empty husk",
			seed:    func(t *testing.T, dest string) {},
			usable:  func(dir string) (bool, error) { bin, _ := resolveEnginePaths(dir, false); return fileExistsErr(bin) },
			content: "bin/sherpa-onnx-offline",
		},
		{
			name: "non-empty but no engine binary",
			seed: func(t *testing.T, dest string) {
				t.Helper()
				plantUnusableDest(t, dest)
			},
			usable:  func(dir string) (bool, error) { bin, _ := resolveEnginePaths(dir, false); return fileExistsErr(bin) },
			content: "bin/sherpa-onnx-offline",
		},
		{
			name: "model dir without tokens.txt",
			seed: func(t *testing.T, dest string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(dest, "something.onnx"), []byte("x"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			usable:  dirHasModel,
			content: "tokens.txt",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			dest := filepath.Join(root, "engine-1.2.3-linux-x64")
			if err := os.MkdirAll(dest, 0o755); err != nil {
				t.Fatal(err)
			}
			tc.seed(t, dest)
			// The copy has to pass the case's own predicate, which is the whole
			// point: recovery applies the CALLER's predicate to a candidate, not
			// a structural guess about what an install looks like.
			holder := plantHolder(t, dest, 100, "the only copy", false)
			install := filepath.Join(holder, "install")
			if err := os.MkdirAll(filepath.Dir(filepath.Join(install, tc.content)), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(install, tc.content), []byte("x"), 0o755); err != nil {
				t.Fatal(err)
			}

			restoreInterruptedPromotion(lockFor(t, dest), dest, tc.usable, nil)

			if ok, err := tc.usable(dest); !ok || err != nil {
				t.Errorf("the usable copy should be live at %s", dest)
			}
			got, err := os.ReadFile(filepath.Join(dest, "engine"))
			if err != nil || string(got) != "the only copy" {
				t.Errorf("the destination should hold the copy: got %q err %v", got, err)
			}
			if _, err := os.Stat(holder); !os.IsNotExist(err) {
				t.Errorf("the restored copy's holder should be cleared, got %v", err)
			}
		})
	}
}

// A holder never replaces a destination that holds a USABLE install: that
// install is the published one and the copy beside it is the leftover. Against
// an unusable destination the ruling inverts, and the husk moves aside rather
// than being deleted.
func TestRestoreInterruptedPromotionKeepsAUsableDestAndReplacesAnUnusableOne(t *testing.T) {
	for _, tc := range []struct {
		name      string
		live      string
		committed bool
	}{
		{name: "empty dest", live: ""},
		{name: "populated dest", live: "live", committed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			dest := filepath.Join(root, "engine-1.2.3-linux-x64")
			if err := os.MkdirAll(dest, 0o755); err != nil {
				t.Fatal(err)
			}
			if tc.live != "" {
				if err := os.WriteFile(filepath.Join(dest, "engine"), []byte(tc.live), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			holder := plantHolder(t, dest, 1, "stale", tc.committed)

			restoreInterruptedPromotion(lockFor(t, dest), dest, testPublished, nil)

			got, err := os.ReadFile(filepath.Join(dest, "engine"))
			if tc.live == "" {
				// The husk was unusable, so the copy beside it wins and the husk
				// is set aside under a Kept name rather than dropped.
				if err != nil || string(got) != "stale" {
					t.Fatalf("the usable copy should have replaced the husk: %q err %v", got, err)
				}
				if _, err := os.Stat(holder); !os.IsNotExist(err) {
					t.Errorf("the restored copy's holder should be cleared, got %v", err)
				}
				return
			}
			if err != nil || string(got) != tc.live {
				t.Fatalf("engine = %q, err %v, want the live %q", got, err, tc.live)
			}
			// Its commit flag proves the live install published over it, which
			// is the only thing that licenses the delete.
			if _, err := os.Stat(holder); !os.IsNotExist(err) {
				t.Errorf("a live dest should reap the copy it provably superseded, got %v", err)
			}
		})
	}
}

// An interrupted promotion of the MODEL leaves it in a holder exactly as it does
// for the engine, and the model side is what an offline user loses if nothing
// puts it back: with no network there is no download to fall back to.
func TestEnsureLocalEngineRestoresAnInterruptedModelPromotion(t *testing.T) {
	srv := fakeReleaseServer(t, engineSHA, modelSHA)
	dest := t.TempDir()
	opts := DownloadOptions{
		DestRoot: dest, EngineVersion: "test", APIBase: srv.URL, platformKey: "linux-amd64", skipPinned: true,
	}
	comp, err := EnsureLocalEngine(context.Background(), opts)
	if err != nil {
		t.Fatalf("seeding the install: %v", err)
	}

	// Stop the world where promoteStagedDir has moved the model aside but has
	// not yet published the staged copy.
	modelDir := filepath.Dir(comp.ModelPath)
	holder := fmt.Sprintf("%s%s%020d%s", modelDir, holderSuffix, 1, holderSeqSuffix)
	if err := os.MkdirAll(holder, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeHolderMarker(holder, txnMarker{Kind: holderMarkerKind, Dest: filepath.Base(modelDir), Seq: 1}); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(modelDir, filepath.Join(holder, "install")); err != nil {
		t.Fatal(err)
	}

	offline := offlineAPIBase(t)
	got, err := EnsureLocalEngine(context.Background(), DownloadOptions{
		DestRoot: dest, EngineVersion: "test", APIBase: offline, platformKey: "linux-amd64", skipPinned: true,
	})
	if err != nil {
		t.Fatalf("the model set aside by an interrupted promotion was not restored: %v", err)
	}
	if !fileExists(filepath.Join(got.ModelPath, "tokens.txt")) {
		t.Errorf("model tokens.txt missing under %q", got.ModelPath)
	}
	if _, err := os.Stat(holder); !os.IsNotExist(err) {
		t.Errorf("the holder should be cleared after a restore, got %v", err)
	}
}

// offlineAPIBase returns an API base nothing is listening on, so any attempt to
// resolve a release asset fails instead of silently downloading.
func offlineAPIBase(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.NewServeMux())
	url := srv.URL
	srv.Close()
	return url
}

// lockFor takes the Install lock the promotion path now requires, for the
// destination the test is about to drive. Released in cleanup, and release is
// idempotent, so a test that goes on to call EnsureLocalEngine releases it
// early rather than waiting out its own lock.
func lockFor(t *testing.T, destDir string) *destTxn {
	t.Helper()
	txn, err := lockDestination(context.Background(), filepath.Dir(destDir), filepath.Base(destDir))
	if err != nil {
		t.Fatalf("locking %s: %v", destDir, err)
	}
	t.Cleanup(func() { _ = txn.release() })
	return txn
}

// testPublished is the "is this a real install" predicate these tests use: the
// fixtures write an "engine" file, so its presence is what publication means here.
func testPublished(dir string) (bool, error) {
	_, err := os.Stat(filepath.Join(dir, "engine"))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// plantHolder writes an install into a holder named and marked the way
// promoteStagedDir writes one, so recovery sees the same shape it does in
// production: the name alone is not ownership, and a fixture without the marker
// would test a directory recovery is supposed to leave alone. committed plants
// the flag the publish writes, which is the only evidence that licenses removing
// the copy, so a fixture has to say which of the two it is.
func plantHolder(t *testing.T, destDir string, seq int64, content string, committed bool) string {
	t.Helper()
	holder := fmt.Sprintf("%s%s%020d%s", destDir, holderSuffix, seq, holderSeqSuffix)
	install := filepath.Join(holder, "install")
	if err := os.MkdirAll(install, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(install, "engine"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeHolderMarker(holder, txnMarker{Kind: holderMarkerKind, Dest: filepath.Base(destDir), Seq: seq}); err != nil {
		t.Fatal(err)
	}
	if committed {
		if err := writeCommitFlag(holder); err != nil {
			t.Fatal(err)
		}
	}
	return holder
}

// keptName is the name a parked holder takes: the same sequence under the Kept
// prefix, which is what makes a retained copy findable by the operator and
// invisible to the next scan.
func keptName(t *testing.T, destDir, holder string) string {
	t.Helper()
	seq, ok := holderStamp(destDir, holder)
	if !ok {
		t.Fatalf("holder name %q carries no sequence", filepath.Base(holder))
	}
	return fmt.Sprintf("%s%s%020d%s", destDir, keptSuffix, seq, holderSeqSuffix)
}

// plantUnowned creates a directory carrying the exact holder name grammar and no
// marker: the shape recovery must retain in place rather than restore from or
// reap, since nothing on disk attributes it.
func plantUnowned(t *testing.T, destDir string, seq int64, content string) string {
	t.Helper()
	holder := fmt.Sprintf("%s%s%020d%s", destDir, holderSuffix, seq, holderSeqSuffix)
	install := filepath.Join(holder, "install")
	if err := os.MkdirAll(install, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(install, "engine"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return holder
}

// plantUnusableDest leaves a directory at destDir that testPublished rejects:
// present, non-empty, and holding no install. It is the husk the review's P2
// chain ends at, and the state recovery has to be able to leave.
func plantUnusableDest(t *testing.T, destDir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(destDir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(destDir, "bin", "README"), []byte("partial"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// reporterFor collects the recovery report so a retain assertion can check the
// copy was named. A copy recovery keeps and never names is one an operator
// cannot find.
func reporterFor(reported *[]string) func(string) {
	return func(m string) { *reported = append(*reported, m) }
}

// assertReports fails unless some report line mentions each wanted fragment.
func assertReports(t *testing.T, reported []string, want ...string) {
	t.Helper()
	for _, fragment := range want {
		if !slices.ContainsFunc(reported, func(m string) bool { return strings.Contains(m, fragment) }) {
			t.Errorf("the report should name %q, got %v", fragment, reported)
		}
	}
}

// A cleanup that could not finish leaves an old holder behind; a later
// interrupted promotion adds a second one. Recovery has to put back the newer
// install, and Glob's lexical order is no evidence of which that is.
func TestRestoreInterruptedPromotionPrefersTheNewestHolder(t *testing.T) {
	root := t.TempDir()
	dest := filepath.Join(root, "engine-1.2.3-linux-x64")
	stale := plantHolder(t, dest, 100, "stale", false)
	current := plantHolder(t, dest, 200, "current", false)

	restoreInterruptedPromotion(lockFor(t, dest), dest, testPublished, nil)

	got, err := os.ReadFile(filepath.Join(dest, "engine"))
	if err != nil || string(got) != "current" {
		t.Fatalf("restored engine = %q (err %v), want the newest holder's %q", got, err, "current")
	}
	if _, err := os.Stat(current); !os.IsNotExist(err) {
		t.Errorf("the restored holder should be cleared, got %v", err)
	}
	// The loser is kept rather than deleted on a guess, and it moves under the
	// Kept prefix so the next pass has nothing left to rule on.
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("the older holder should have left the scanned prefix, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(keptName(t, dest, stale), "install", "engine")); err != nil {
		t.Errorf("the older holder must be kept intact under the Kept prefix: %v", err)
	}
}

// interruptPromotion drives the REAL promotion into the state a process killed
// between its two renames leaves: destDir absent, the only install in a holder
// promoteStagedDir named. Both renames fail, so nothing is put back in process
// and the holder is retained rather than cleaned up.
func interruptPromotion(t *testing.T, txn *destTxn, destDir, label string) {
	t.Helper()
	stage := destDir + ".incoming"
	if err := os.MkdirAll(stage, 0o755); err != nil {
		t.Fatal(err)
	}
	real := holderFS.rename
	holderFS.rename = func(from, to string) error {
		if to == destDir {
			return errors.New("injected rename failure")
		}
		return real(from, to)
	}
	err := promoteStagedDir(txn, stage, destDir, label, nil)
	holderFS.rename = real
	if err == nil {
		t.Fatalf("a promotion whose publish and restore both fail must report an error")
	}
	if _, statErr := os.Lstat(destDir); !os.IsNotExist(statErr) {
		t.Fatalf("the interrupted promotion should leave %s absent, got %v", destDir, statErr)
	}
}

// holdersFor lists the holders promoteStagedDir left beside destDir.
func holdersFor(t *testing.T, destDir string) []string {
	t.Helper()
	holders, err := filepath.Glob(destDir + holderSuffix + "*")
	if err != nil {
		t.Fatal(err)
	}
	return holders
}

// End to end over the real transaction, for BOTH consumers of it. The engine and
// the model are installed for real, one of them is interrupted mid-promotion by
// the real promoteStagedDir, and the next start has to put it back with no
// network to fall back on. Nothing plants a holder by hand, so the name recovery
// orders by is the one promoteStagedDir writes -- the half a hand-built fixture
// cannot check.
func TestEnsureLocalEngineRecoversARealInterruptedPromotionOffline(t *testing.T) {
	for _, tc := range []struct {
		name  string
		label string
		// dirFor picks the install this case interrupts, given a finished setup.
		dirFor func(t *testing.T, dest string, comp EngineComponents) string
	}{
		{
			name:  "engine",
			label: "Engine",
			dirFor: func(t *testing.T, dest string, comp EngineComponents) string {
				t.Helper()
				matches, err := filepath.Glob(filepath.Join(dest, "engine-*"))
				if err != nil || len(matches) != 1 {
					t.Fatalf("want exactly one engine dir under %s, got %v (err %v)", dest, matches, err)
				}
				return matches[0]
			},
		},
		{
			name:  "model",
			label: "Model",
			dirFor: func(t *testing.T, dest string, comp EngineComponents) string {
				t.Helper()
				return filepath.Dir(comp.ModelPath)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := fakeReleaseServer(t, engineSHA, modelSHA)
			dest := t.TempDir()
			comp, err := EnsureLocalEngine(context.Background(), DownloadOptions{
				DestRoot: dest, EngineVersion: "test", APIBase: srv.URL, platformKey: "linux-amd64", skipPinned: true,
			})
			if err != nil {
				t.Fatalf("seeding the install: %v", err)
			}

			target := tc.dirFor(t, dest, comp)
			targetTxn := lockFor(t, target)
			interruptPromotion(t, targetTxn, target, tc.label)
			// Released before EnsureLocalEngine below, which takes the same
			// lock: the wait is cross-process but flock conflicts between two
			// opens in one process too.
			targetTxn.release()
			holders := holdersFor(t, target)
			if len(holders) != 1 {
				t.Fatalf("want exactly one holder beside %s, got %v", target, holders)
			}
			// The name promoteStagedDir wrote must be one recovery can order by.
			// Restoring a lone holder works either way, so without this the two
			// halves could drift apart and only a second holder would show it.
			if _, ok := holderStamp(target, holders[0]); !ok {
				t.Fatalf("recovery cannot order the name promoteStagedDir wrote: %q", holders[0])
			}

			// No network: if the holder is not found there is nothing to fall
			// back on, which is the failure the offline user actually sees.
			got, err := EnsureLocalEngine(context.Background(), DownloadOptions{
				DestRoot: dest, EngineVersion: "test", APIBase: offlineAPIBase(t), platformKey: "linux-amd64", skipPinned: true,
			})
			if err != nil {
				t.Fatalf("the interrupted %s promotion was not recovered offline: %v", tc.name, err)
			}
			if !fileExists(got.BinaryPath) {
				t.Errorf("engine binary missing after recovery: %q", got.BinaryPath)
			}
			if !fileExists(filepath.Join(got.ModelPath, "tokens.txt")) {
				t.Errorf("model tokens.txt missing after recovery under %q", got.ModelPath)
			}
			if holders := holdersFor(t, target); len(holders) != 0 {
				t.Errorf("the holder should be cleared after recovery, got %v", holders)
			}
		})
	}
}

// A path is not a pattern. An install root containing a glob metacharacter -- a
// '[' is the one that silently matches nothing -- must not cost a user the
// install recovery exists to put back.
func TestRestoreInterruptedPromotionFindsHoldersUnderAnAwkwardPath(t *testing.T) {
	dirNames := []string{"plain", "wei[rd", "br]ack"}
	if runtime.GOOS != "windows" {
		// Windows refuses these two in a filename outright, so there is no such
		// path to defend there. The bracket cases above still cover the
		// metacharacter that matches nothing instead of failing.
		dirNames = append(dirNames, "sta*r", "que?ry")
	}
	for _, dirName := range dirNames {
		t.Run(dirName, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), dirName)
			dest := filepath.Join(root, "engine-1.2.3-linux-x64")
			install := filepath.Join(plantHolder(t, dest, 100, "kept", false), "install")
			if _, err := os.Stat(install); err != nil {
				t.Fatal(err)
			}

			restoreInterruptedPromotion(lockFor(t, dest), dest, testPublished, nil)

			got, err := os.ReadFile(filepath.Join(dest, "engine"))
			if err != nil || string(got) != "kept" {
				t.Fatalf("the install was not restored under %q: got %q err %v", dirName, got, err)
			}
		})
	}
}

// A backward clock leaves a STALE holder carrying a LARGER stamp. Recovery must
// still put back the install that was actually live last. Nothing is deleted
// either way at this site, so the failure is a stale restore, and the negative
// half of the assertion pins that the loser is kept.
func TestRestoreInterruptedPromotionPrefersTheRealNewerInstallOverAFutureStampedHolder(t *testing.T) {
	const farFuture = int64(4_000_000_000_000_000_000)

	root := t.TempDir()
	dest := filepath.Join(root, "engine-1.2.3-linux-x64")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dest, "engine"), []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	stale := plantHolder(t, dest, farFuture, "stale", false)

	// The real transaction sets "new" aside and never publishes.
	txn := lockFor(t, dest)
	interruptPromotion(t, txn, dest, "engine")

	restoreInterruptedPromotion(txn, dest, testPublished, nil)

	got, err := os.ReadFile(filepath.Join(dest, "engine"))
	if err != nil || string(got) != "new" {
		t.Errorf("restored %q (err %v), want the install that was live last, %q", got, err, "new")
	}
	// Whichever holder won, the one that lost is kept, under the Kept prefix.
	if _, err := os.Stat(filepath.Join(keptName(t, dest, stale), "install", "engine")); err != nil {
		t.Errorf("a holder that lost the ordering must be kept, not deleted: %v", err)
	}
}

// The allocator and the parser must agree, and the sequence must seed above a
// name a released binary wrote.
func TestHolderNamesAllocateInOrderAndParse(t *testing.T) {
	t.Run("counts up and parses", func(t *testing.T) {
		root := t.TempDir()
		dest := filepath.Join(root, "engine-1.2.3-linux-x64")
		for want := int64(1); want <= 2; want++ {
			seq, err := nextHolderSeq(dest)
			if err != nil {
				t.Fatal(err)
			}
			path, err := createSequencedHolder(dest, seq)
			if err != nil {
				t.Fatal(err)
			}
			stamp, ok := holderStamp(dest, path)
			if !ok {
				t.Fatalf("the allocator wrote a name recovery cannot order: %q", filepath.Base(path))
			}
			if stamp != want {
				t.Errorf("stamp = %d, want %d", stamp, want)
			}
		}
	})

	// No released version of this package ever wrote a holder: v0.8.0 staged
	// under .stage-* and published with a plain rename. A nanosecond-shaped name
	// beside an install is therefore something this code did not write, and
	// counting it would let any such directory dictate the sequence, up to the
	// maximum that refuses installs outright.
	t.Run("ignores a nanosecond-shaped name it did not write", func(t *testing.T) {
		root := t.TempDir()
		dest := filepath.Join(root, "engine-1.2.3-linux-x64")
		const nano = int64(1_700_000_000_000_000_000)
		var planted []string
		for _, suffix := range []string{"x7Kq3", "12345"} {
			path := fmt.Sprintf("%s%s%020d-%s", dest, holderSuffix, nano, suffix)
			if err := os.MkdirAll(path, 0o700); err != nil {
				t.Fatal(err)
			}
			planted = append(planted, path)
		}
		seq, err := nextHolderSeq(dest)
		if err != nil {
			t.Fatal(err)
		}
		if seq != 1 {
			t.Errorf("next sequence = %d, want 1: neither planted name is one this code wrote", seq)
		}
		for _, path := range planted {
			if _, err := os.Stat(path); err != nil {
				t.Errorf("a name this code did not write must be left alone: %v", err)
			}
		}
	})
}

func TestCreateSequencedHolderSkipsAnOccupiedNumber(t *testing.T) {
	root := t.TempDir()
	dest := filepath.Join(root, "engine-1.2.3-linux-x64")
	taken := fmt.Sprintf("%s%s%020d%s", dest, holderSuffix, 1, holderSeqSuffix)
	if err := os.MkdirAll(taken, 0o700); err != nil {
		t.Fatal(err)
	}

	got, err := createSequencedHolder(dest, 1)
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("%s%s%020d%s", dest, holderSuffix, 2, holderSeqSuffix)
	if got != want {
		t.Errorf("claimed %q, want %q", got, want)
	}
}

func TestNextHolderSeqRefusesOverflow(t *testing.T) {
	root := t.TempDir()
	dest := filepath.Join(root, "engine-1.2.3-linux-x64")
	if err := os.MkdirAll(fmt.Sprintf("%s%s%020d%s", dest, holderSuffix, int64(math.MaxInt64), holderSeqSuffix), 0o700); err != nil {
		t.Fatal(err)
	}
	if seq, err := nextHolderSeq(dest); err == nil {
		t.Errorf("nextHolderSeq returned %d, want an error rather than a wrapped value", seq)
	}
}

func TestCreateSequencedHolderRefusesOutOfRange(t *testing.T) {
	root := t.TempDir()
	dest := filepath.Join(root, "engine-1.2.3-linux-x64")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range []int64{0, -1, math.MinInt64} {
		if _, err := createSequencedHolder(dest, n); err == nil {
			t.Errorf("createSequencedHolder(%d) succeeded, want an error", n)
		}
	}
	if holders := holdersFor(t, dest); len(holders) != 0 {
		t.Errorf("a refused allocation must create nothing, got %v", holders)
	}
}

// This site holds no lock, so exclusive creation is the only thing arbitrating.
func TestHolderSeqAllocatesDistinctValuesConcurrently(t *testing.T) {
	root := t.TempDir()
	dest := filepath.Join(root, "engine-1.2.3-linux-x64")
	// 128, not a smaller number: against a check-then-create allocator this
	// detects the duplicate in every run, where 16 caught it about half the time.
	const writers = 128

	var wg sync.WaitGroup
	paths := make([]string, writers)
	errs := make([]error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			seq, err := nextHolderSeq(dest)
			if err != nil {
				errs[i] = err
				return
			}
			paths[i], errs[i] = createSequencedHolder(dest, seq)
		}(i)
	}
	wg.Wait()

	seen := map[string]bool{}
	stamps := map[int64]bool{}
	for i, path := range paths {
		if errs[i] != nil {
			t.Fatalf("writer %d: %v", i, errs[i])
		}
		if seen[path] {
			t.Errorf("two writers claimed the same name %q", path)
		}
		seen[path] = true
		stamp, ok := holderStamp(dest, path)
		if !ok {
			t.Errorf("writer %d wrote an unorderable name %q", i, filepath.Base(path))
			continue
		}
		if stamps[stamp] {
			t.Errorf("two writers claimed sequence %d", stamp)
		}
		stamps[stamp] = true
	}
}

// The holder wraps a complete previous install for as long as it exists, so it
// keeps the owner-only mode it has always had. os.Mkdir takes
// a mode where os.MkdirTemp did not, so the allocator has to name it.
func TestHolderKeepsOwnerOnlyPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Go permission bits do not map to Windows ACLs")
	}
	root := t.TempDir()
	dest := filepath.Join(root, "engine-1.2.3-linux-x64")
	path, err := createSequencedHolder(dest, 1)
	if err != nil {
		t.Fatal(err)
	}
	requireUmaskAllowsWiderThan0700(t, root)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Errorf("holder mode = %o, want 0700", got)
	}
}

// requireUmaskAllowsWiderThan0700 skips when the process umask would strip the
// group and other bits anyway. Without it this assertion is vacuous under a
// umask of 077: a wrongly widened 0o755 comes back as 0700 and passes.
func requireUmaskAllowsWiderThan0700(t *testing.T, parent string) {
	t.Helper()
	control := filepath.Join(parent, ".umask-control")
	if err := os.Mkdir(control, 0o755); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(control) }()
	info, err := os.Stat(control)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Skipf("umask masks a 0755 request down to %o, so this assertion cannot tell 0700 from a widened mode", info.Mode().Perm())
	}
}

// Recovery restores only from a holder it can prove it wrote for THIS install.
// Both decoys carry content that would land at the destination if the name alone
// were the attribution, and both sort FIRST lexically, so only the ownership
// rule can produce the wanted answer.
func TestRestoreInterruptedPromotionRestoresOnlyFromAnOwnedHolder(t *testing.T) {
	root := t.TempDir()
	dest := filepath.Join(root, "engine-1.2.3-linux-x64")
	// A user's own directory that happens to start like a holder name.
	collide := dest + holderSuffix + "aaa"
	// A name this code would have written, with nothing behind it saying it did.
	unmarked := fmt.Sprintf("%s%s%020d%s", dest, holderSuffix, 900, holderSeqSuffix)
	for _, decoy := range []string{collide, unmarked} {
		if err := os.MkdirAll(filepath.Join(decoy, "install"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(decoy, "install", "engine"), []byte("not ours"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	owned := plantHolder(t, dest, 100, "ours", false)

	restoreInterruptedPromotion(lockFor(t, dest), dest, testPublished, nil)

	got, err := os.ReadFile(filepath.Join(dest, "engine"))
	if err != nil || string(got) != "ours" {
		t.Fatalf("recovery restored from a holder it cannot prove it wrote: got %q err %v", got, err)
	}
	if _, err := os.Stat(owned); !os.IsNotExist(err) {
		t.Errorf("the restored holder should be cleared, got %v", err)
	}
	// Neither decoy is recovery's to move or delete.
	for _, decoy := range []string{collide, unmarked} {
		if _, err := os.Stat(filepath.Join(decoy, "install", "engine")); err != nil {
			t.Errorf("%s must be left exactly as found: %v", filepath.Base(decoy), err)
		}
	}
}

// A holder can be there without an install in it: the promotion creates the
// holder first, so a stop before the rename leaves an empty one. Recovery must
// step over it and keep looking rather than treating it as the newest word on
// what to restore.
func TestRestoreInterruptedPromotionSkipsAHolderWithNoInstall(t *testing.T) {
	root := t.TempDir()
	dest := filepath.Join(root, "engine-1.2.3-linux-x64")
	usable := plantHolder(t, dest, 100, "kept", false)
	// Newer AND owned, so ordering reaches it first and it is a real candidate,
	// but it holds nothing: exactly what a stop between the allocation and the
	// set-aside rename leaves.
	empty := fmt.Sprintf("%s%s%020d%s", dest, holderSuffix, 200, holderSeqSuffix)
	if err := os.MkdirAll(empty, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeHolderMarker(empty, txnMarker{Kind: holderMarkerKind, Dest: filepath.Base(dest), Seq: 200}); err != nil {
		t.Fatal(err)
	}

	restoreInterruptedPromotion(lockFor(t, dest), dest, testPublished, nil)

	got, err := os.ReadFile(filepath.Join(dest, "engine"))
	if err != nil || string(got) != "kept" {
		t.Fatalf("the usable holder should be restored: got %q err %v", got, err)
	}
	if _, err := os.Stat(usable); !os.IsNotExist(err) {
		t.Errorf("the restored holder should be cleared, got %v", err)
	}
	// A holder that is provably this code's and provably holds nothing costs
	// nothing to remove, and leaving it is what accumulates scratch forever.
	if _, err := os.Stat(empty); !os.IsNotExist(err) {
		t.Errorf("an owned holder with no install should be removed, got %v", err)
	}
}

// ---- filesystem seam -------------------------------------------------------

// injectFault swaps one field of holderFS for a wrapper that fails the call
// whose arguments satisfy match and passes every other call through to the real
// primitive. The call ordinal is appended to the arguments handed to match, so
// nthCall can select by call order without a second counter, and it is counted
// with an atomic because the racing scenarios drive one seam from two
// goroutines. The whole struct is restored in t.Cleanup, so a test that injects
// twice unwinds in reverse.
func injectFault(t *testing.T, step string, match func(args ...string) bool, err error) {
	t.Helper()
	var calls atomic.Int64
	real := holderFS
	t.Cleanup(func() { holderFS = real })
	fire := func(args ...string) bool {
		n := calls.Add(1)
		if match == nil {
			return true
		}
		return match(append(args, strconv.FormatInt(n, 10))...)
	}
	switch step {
	case "rename":
		holderFS.rename = func(from, to string) error {
			if fire(from, to) {
				return err
			}
			return real.rename(from, to)
		}
	case "removeAll":
		holderFS.removeAll = func(path string) error {
			if fire(path) {
				return err
			}
			return real.removeAll(path)
		}
	case "stat":
		holderFS.stat = func(name string) (os.FileInfo, error) {
			if fire(name) {
				return nil, err
			}
			return real.stat(name)
		}
	case "lstat":
		holderFS.lstat = func(name string) (os.FileInfo, error) {
			if fire(name) {
				return nil, err
			}
			return real.lstat(name)
		}
	case "readDir":
		holderFS.readDir = func(name string) ([]os.DirEntry, error) {
			if fire(name) {
				return nil, err
			}
			return real.readDir(name)
		}
	case "readFile":
		holderFS.readFile = func(name string) ([]byte, error) {
			if fire(name) {
				return nil, err
			}
			return real.readFile(name)
		}
	case "mkdir":
		holderFS.mkdir = func(name string, perm os.FileMode) error {
			if fire(name) {
				return err
			}
			return real.mkdir(name, perm)
		}
	case "writeFile":
		holderFS.writeFile = func(name string, data []byte, perm os.FileMode) error {
			if fire(name) {
				return err
			}
			return real.writeFile(name, data, perm)
		}
	case "create":
		holderFS.create = func(name string, flag int, perm os.FileMode) (*os.File, error) {
			if fire(name) {
				return nil, err
			}
			return real.create(name, flag, perm)
		}
	case "createTemp":
		holderFS.createTemp = func(dir, pattern string) (*os.File, error) {
			if fire(dir, pattern) {
				return nil, err
			}
			return real.createTemp(dir, pattern)
		}
	default:
		t.Fatalf("injectFault: unknown step %q", step)
	}
}

// nthCall selects the nth call through an injected seam. It reads the ordinal
// injectFault appends to the arguments rather than counting for itself, so the
// two share one counter and a concurrent test has one place to be correct.
func nthCall(n int) func(args ...string) bool {
	return func(args ...string) bool {
		return len(args) > 0 && args[len(args)-1] == strconv.Itoa(n)
	}
}

// blockStep holds every call to step until the returned release runs, which is
// how a test parks one transaction mid-step and lets another reach the same
// destination. Releasing twice is safe, so a test can release on the happy path
// and still defer it.
func blockStep(t *testing.T, step string) (release func()) {
	t.Helper()
	gate := make(chan struct{})
	var once sync.Once
	release = func() { once.Do(func() { close(gate) }) }
	t.Cleanup(release)
	injectFaultGate(t, step, gate)
	return release
}

// injectFaultGate is blockStep's half of the swap, split out so the wrapper it
// installs sits beside the fault wrappers above.
func injectFaultGate(t *testing.T, step string, gate <-chan struct{}) {
	t.Helper()
	real := holderFS
	t.Cleanup(func() { holderFS = real })
	switch step {
	case "rename":
		holderFS.rename = func(from, to string) error {
			<-gate
			return real.rename(from, to)
		}
	case "removeAll":
		holderFS.removeAll = func(path string) error {
			<-gate
			return real.removeAll(path)
		}
	case "stat":
		holderFS.stat = func(name string) (os.FileInfo, error) {
			<-gate
			return real.stat(name)
		}
	default:
		t.Fatalf("blockStep: unknown step %q", step)
	}
}

// The seam has to be the real filesystem until a test swaps a field. A nil field
// is a panic in whatever production path reaches it first, and a field that no
// longer behaves like its os function makes every test above it prove nothing.
func TestHolderFSDefaultsAreTheRealPrimitives(t *testing.T) {
	for name, missing := range map[string]bool{
		"rename":     holderFS.rename == nil,
		"removeAll":  holderFS.removeAll == nil,
		"stat":       holderFS.stat == nil,
		"lstat":      holderFS.lstat == nil,
		"readDir":    holderFS.readDir == nil,
		"readFile":   holderFS.readFile == nil,
		"mkdir":      holderFS.mkdir == nil,
		"writeFile":  holderFS.writeFile == nil,
		"create":     holderFS.create == nil,
		"createTemp": holderFS.createTemp == nil,
	} {
		if missing {
			t.Errorf("holderFS.%s is nil", name)
		}
	}

	root := t.TempDir()
	dir := filepath.Join(root, "one")
	if err := holderFS.mkdir(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	file := filepath.Join(dir, "a.txt")
	if err := holderFS.writeFile(file, []byte("v0"), 0o600); err != nil {
		t.Fatalf("writeFile: %v", err)
	}
	if _, err := holderFS.stat(file); err != nil {
		t.Fatalf("stat: %v", err)
	}
	if _, err := holderFS.lstat(file); err != nil {
		t.Fatalf("lstat: %v", err)
	}
	if got, err := holderFS.readFile(file); err != nil || string(got) != "v0" {
		t.Fatalf("readFile = %q, %v, want %q", got, err, "v0")
	}
	entries, err := holderFS.readDir(dir)
	if err != nil || len(entries) != 1 || entries[0].Name() != "a.txt" {
		t.Fatalf("readDir = %v, %v, want one entry a.txt", entries, err)
	}
	tmp, err := holderFS.createTemp(dir, "seam-*")
	if err != nil {
		t.Fatalf("createTemp: %v", err)
	}
	tmpName := tmp.Name()
	if err := tmp.Close(); err != nil {
		t.Fatalf("close temp: %v", err)
	}
	opened, err := holderFS.create(tmpName, os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := opened.Close(); err != nil {
		t.Fatalf("close opened: %v", err)
	}
	moved := filepath.Join(root, "two")
	if err := holderFS.rename(dir, moved); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if _, err := holderFS.stat(dir); !os.IsNotExist(err) {
		t.Fatalf("stat of the old name = %v, want not-exist", err)
	}
	if err := holderFS.removeAll(moved); err != nil {
		t.Fatalf("removeAll: %v", err)
	}
	if _, err := holderFS.stat(moved); !os.IsNotExist(err) {
		t.Fatalf("stat after removeAll = %v, want not-exist", err)
	}
}

// The matrix needs both selections: "fail the rename whose source is seq 2's
// set-aside copy" is an argument match, and "fail the second remove" is an
// ordinal one.
// An ordinal alone cannot express the first, and an argument match cannot
// express a step that runs twice on the same path.
func TestInjectFaultMatchesByArgumentAndByOrdinal(t *testing.T) {
	injected := errors.New("injected seam failure")
	root := t.TempDir()

	t.Run("by argument", func(t *testing.T) {
		injectFault(t, "rename", func(args ...string) bool {
			return strings.HasSuffix(args[0], filepath.Join("seq", "backup"))
		}, injected)
		for _, name := range []string{"one", "seq", "three"} {
			from := filepath.Join(root, name, "backup")
			if err := os.MkdirAll(from, 0o700); err != nil {
				t.Fatal(err)
			}
			err := holderFS.rename(from, filepath.Join(root, name, "moved"))
			if name == "seq" {
				if !errors.Is(err, injected) {
					t.Fatalf("rename of %s = %v, want the injected failure", from, err)
				}
				continue
			}
			if err != nil {
				t.Fatalf("rename of %s = %v, want it to pass through", from, err)
			}
		}
	})

	t.Run("by ordinal", func(t *testing.T) {
		injectFault(t, "removeAll", nthCall(2), injected)
		for i := 1; i <= 3; i++ {
			dir := filepath.Join(root, fmt.Sprintf("remove-%d", i))
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			err := holderFS.removeAll(dir)
			if i == 2 {
				if !errors.Is(err, injected) {
					t.Fatalf("removeAll call %d = %v, want the injected failure", i, err)
				}
				if _, statErr := os.Stat(dir); statErr != nil {
					t.Fatalf("the failed call must not have removed %s: %v", dir, statErr)
				}
				continue
			}
			if err != nil {
				t.Fatalf("removeAll call %d = %v, want it to pass through", i, err)
			}
			if _, statErr := os.Stat(dir); !os.IsNotExist(statErr) {
				t.Fatalf("removeAll call %d left %s behind: %v", i, dir, statErr)
			}
		}
	})

	if got, want := reflect.ValueOf(holderFS.removeAll).Pointer(), reflect.ValueOf(os.RemoveAll).Pointer(); got != want {
		t.Fatal("holderFS.removeAll was not restored after the subtest's cleanup")
	}
	if got, want := reflect.ValueOf(holderFS.rename).Pointer(), reflect.ValueOf(os.Rename).Pointer(); got != want {
		t.Fatal("holderFS.rename was not restored after the subtest's cleanup")
	}

	// One seam, two goroutines: the ordinal must come from a counter that is
	// safe to increment concurrently, or the racing rows this seam exists for
	// report a race instead of a result.
	t.Run("concurrently", func(t *testing.T) {
		injectFault(t, "stat", nthCall(1), injected)
		start := make(chan struct{})
		errs := make([]error, 2)
		var wg sync.WaitGroup
		for i := range errs {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				_, errs[i] = holderFS.stat(root)
			}(i)
		}
		close(start)
		wg.Wait()
		failed := 0
		for _, err := range errs {
			if errors.Is(err, injected) {
				failed++
			} else if err != nil {
				t.Fatalf("the call that was not selected = %v, want it to pass through", err)
			}
		}
		if failed != 1 {
			t.Fatalf("%d of 2 concurrent calls failed, want exactly 1", failed)
		}
	})
}

// A blocked step has to stay blocked: the racing rows park one transaction
// inside a step and only then let the second one reach the same destination.
func TestBlockStepHoldsTheCallUntilRelease(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "blocked")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	release := blockStep(t, "removeAll")
	done := make(chan error, 1)
	go func() { done <- holderFS.removeAll(dir) }()
	select {
	case err := <-done:
		t.Fatalf("removeAll returned %v before release", err)
	case <-time.After(20 * time.Millisecond):
	}
	release()
	release()
	if err := <-done; err != nil {
		t.Fatalf("removeAll after release: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("the released call did not run: %v", err)
	}
}

// ---- the per-destination Install lock ---------------------------------------

// shortenInstallLockWait cuts the wait budget so a test can observe an expiry
// without spending the production two minutes on it.
func shortenInstallLockWait(t *testing.T, d time.Duration) {
	t.Helper()
	real := installLockWait
	installLockWait = d
	t.Cleanup(func() { installLockWait = real })
}

// gateRename holds the one rename from -> to until the returned release runs and
// then fails it. blockStep gates every call to a step, which cannot tell a
// promotion's set-aside rename from the publish that follows it, and this test
// needs the transaction parked precisely between the two.
func gateRename(t *testing.T, from, to string, fail error) (reached <-chan struct{}, release func()) {
	t.Helper()
	gate := make(chan struct{})
	hit := make(chan struct{})
	var once, announced sync.Once
	release = func() { once.Do(func() { close(gate) }) }
	real := holderFS
	t.Cleanup(release)
	t.Cleanup(func() { holderFS = real })
	holderFS.rename = func(f, to2 string) error {
		if f != from || to2 != to {
			return real.rename(f, to2)
		}
		announced.Do(func() { close(hit) })
		<-gate
		return fail
	}
	return hit, release
}

// A promotion moves a whole install out of the way and puts it back, so running
// one without the destination's Install lock is what lets a second process
// delete the copy this one is about to roll back to. A handle for a DIFFERENT
// destination is the same defect wearing a lock, which is why destDir stays an
// explicit parameter.
func TestPromoteStagedDirRefusesWithoutADestinationLock(t *testing.T) {
	root := t.TempDir()
	dest := filepath.Join(root, "model-a")
	stagedTree(t, dest, "old")
	stage := stagedTree(t, filepath.Join(root, "stage"), "new")

	if err := promoteStagedDir(nil, stage, dest, "engine", nil); err == nil {
		t.Error("a promotion with no Install lock must be refused")
	}
	if err := promoteStagedDir(lockFor(t, filepath.Join(root, "model-b")), stage, dest, "engine", nil); err == nil {
		t.Error("a promotion holding another destination's Install lock must be refused")
	}
	got, err := os.ReadFile(filepath.Join(dest, "engine"))
	if err != nil || string(got) != "old" {
		t.Errorf("the destination was touched without its lock: %q err %v", got, err)
	}
	got, err = os.ReadFile(filepath.Join(stage, "engine"))
	if err != nil || string(got) != "new" {
		t.Errorf("the staged copy was touched without the lock: %q err %v", got, err)
	}
}

// Recovery restores and deletes whole installs, so it fails closed the same way,
// and it says so: it has no error to return and a silent skip reads as "there
// was nothing to recover".
func TestRestoreInterruptedPromotionRefusesWithoutADestinationLock(t *testing.T) {
	root := t.TempDir()
	dest := filepath.Join(root, "engine-1.2.3-linux-x64")
	holder := plantHolder(t, dest, 100, "the only copy", false)

	var reported []string
	restoreInterruptedPromotion(nil, dest, testPublished, func(s string) { reported = append(reported, s) })
	if len(reported) == 0 {
		t.Error("a recovery pass that refuses to run must say so")
	}
	restoreInterruptedPromotion(lockFor(t, filepath.Join(root, "engine-other")), dest, testPublished, nil)

	if _, err := os.Lstat(dest); !os.IsNotExist(err) {
		t.Errorf("recovery ran without the destination's lock: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(holder, "install", "engine"))
	if err != nil || string(got) != "the only copy" {
		t.Errorf("the holder was touched without the lock: %q err %v", got, err)
	}
}

// The download half fails closed too, or a caller that forgot the lock still
// reaches promoteStagedDir with a full extraction behind it.
func TestDownloadVerifyExtractRefusesWithoutADestinationLock(t *testing.T) {
	srv := fakeReleaseServer(t, engineSHA, modelSHA)
	root := t.TempDir()
	dest := filepath.Join(root, "engine-test-linux-amd64")
	// A real asset the download would otherwise install, so the refusal is what
	// keeps the destination empty rather than a download that was going to fail.
	asset, err := resolveAsset(context.Background(), http.DefaultClient, srv.URL, "test", "sherpa-onnx-", engineAssetSuffix["linux-amd64"])
	if err != nil {
		t.Fatal(err)
	}

	var requests atomic.Int64
	client := &http.Client{Transport: countingTransport(&requests)}
	if err := downloadVerifyExtract(context.Background(), client, asset, "", false, "Engine", dest, nil, func(string) {}); err == nil {
		t.Fatal("a download with no Install lock must be refused")
	}
	// Refused before the transfer, not after it: the promotion at the end would
	// refuse too, and then a caller that forgot the lock still pays for the
	// whole download every start.
	if got := requests.Load(); got != 0 {
		t.Errorf("%d asset request(s) were made without the Install lock, want 0", got)
	}
	if _, err := os.Lstat(dest); !os.IsNotExist(err) {
		t.Errorf("the destination was installed to without its lock: %v", err)
	}
}

// countingTransport counts the asset transfers a client performs, which is how a
// test tells "refused before the download" from "refused after it".
func countingTransport(n *atomic.Int64) http.RoundTripper {
	return roundTripFunc(func(r *http.Request) (*http.Response, error) {
		n.Add(1)
		return http.DefaultTransport.RoundTrip(r)
	})
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// The lock is the cross-process one, so a second open of the same lock file has
// to conflict, and a caller has to wait the holder out rather than fail on the
// first contended try.
func TestLockDestinationSerializesAcrossProcesses(t *testing.T) {
	root := t.TempDir()
	lockDir := filepath.Join(root, installLockDir)
	if err := os.MkdirAll(lockDir, 0o700); err != nil {
		t.Fatal(err)
	}
	held, err := lockutil.TryAcquireFileLockAt(root, filepath.Join(lockDir, "engine-a.lock"))
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := lockDestination(ctx, root, "engine-a"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lockDestination = %v, want the context error while another holder has the lock", err)
	}
	if waited := time.Since(start); waited < 150*time.Millisecond {
		t.Errorf("lockDestination gave up after %v; it must wait the holder out", waited)
	}

	if err := held.Release(); err != nil {
		t.Fatal(err)
	}
	txn, err := lockDestination(context.Background(), root, "engine-a")
	if err != nil {
		t.Fatalf("the lock was not free after the holder released it: %v", err)
	}
	txn.release()
}

// A destination name is one path component. Anything else would put the lock
// file somewhere other than beside its peers, so two callers for the same
// destination could hold different inodes and both proceed.
func TestLockDestinationRefusesADestinationThatIsNotABaseName(t *testing.T) {
	root := t.TempDir()
	for _, dest := range []string{"", ".", "..", "a/b", filepath.Join("..", "escape")} {
		if _, err := lockDestination(context.Background(), root, dest); err == nil {
			t.Errorf("lockDestination(%q) must be refused", dest)
		}
	}
}

// The likeliest reason a caller finds the destination locked is that another
// process is installing the very thing it wants. Failing on the first contended
// try turns a normal race into a user-visible install failure.
func TestEnsureLocalEngineWaitsForAConcurrentInstall(t *testing.T) {
	srv := fakeReleaseServer(t, engineSHA, modelSHA)
	root := t.TempDir()
	txn := lockFor(t, filepath.Join(root, "engine-test-linux-amd64"))
	go func() {
		time.Sleep(150 * time.Millisecond)
		txn.release()
	}()

	comp, err := EnsureLocalEngine(context.Background(), DownloadOptions{
		DestRoot: root, EngineVersion: "test", APIBase: srv.URL, platformKey: "linux-amd64", skipPinned: true,
	})
	if err != nil {
		t.Fatalf("an install that waited out a concurrent one must succeed: %v", err)
	}
	if !fileExists(comp.BinaryPath) {
		t.Errorf("engine binary missing at %q", comp.BinaryPath)
	}
}

// The review's P2: a promotion parked between its two renames has the only copy
// of the install in a holder and destDir absent. A recovery pass that runs in
// that window restores the holder and removes it, and the promotion's rollback
// then has nothing to put back. The lock is what makes that window unreachable.
func TestRecoveryDoesNotDeleteALiveRollbackSource(t *testing.T) {
	root := t.TempDir()
	dest := filepath.Join(root, "engine-1.2.3-linux-x64")
	stagedTree(t, dest, "live")
	stage := stagedTree(t, filepath.Join(root, "stage"), "new")

	reached, release := gateRename(t, stage, dest, errors.New("injected publish failure"))
	txn := lockFor(t, dest)
	promoted := make(chan error, 1)
	go func() { promoted <- promoteStagedDir(txn, stage, dest, "engine", nil) }()
	select {
	case <-reached:
	case <-time.After(10 * time.Second):
		t.Fatal("the promotion never reached its publish rename")
	}
	if _, err := os.Lstat(dest); !os.IsNotExist(err) {
		t.Fatalf("the parked promotion should leave %s absent, got %v", dest, err)
	}
	holders := holdersFor(t, dest)
	if len(holders) != 1 {
		t.Fatalf("want exactly one holder beside %s, got %v", dest, holders)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	err := withDestinationLock(ctx, root, filepath.Base(dest), testPublished, func(rec *destTxn) error {
		restoreInterruptedPromotion(rec, dest, testPublished, nil)
		return nil
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("recovery = %v, want the context error while a promotion holds the destination", err)
	}
	got, readErr := os.ReadFile(filepath.Join(holders[0], "install", "engine"))
	if readErr != nil || string(got) != "live" {
		t.Fatalf("recovery took the copy the promotion still needs: %q err %v", got, readErr)
	}

	release()
	if err := <-promoted; err == nil {
		t.Fatal("the injected publish failure must be reported")
	}
	got, readErr = os.ReadFile(filepath.Join(dest, "engine"))
	if readErr != nil || string(got) != "live" {
		t.Fatalf("the rollback did not put the previous install back: %q err %v", got, readErr)
	}
	txn.release()
}

// The two destinations are locked one after the other. Taking both up front
// would let a model install someone else is running block the engine step, which
// is shared and usually already done.
func TestEnsureLocalEngineLocksEngineAndModelSeparately(t *testing.T) {
	shortenInstallLockWait(t, 200*time.Millisecond)
	srv := fakeReleaseServer(t, engineSHA, modelSHA)
	root := t.TempDir()
	modelTxn := lockFor(t, filepath.Join(root, "model-moonshine-tiny-en-int8"))

	_, err := EnsureLocalEngine(context.Background(), DownloadOptions{
		DestRoot: root, EngineVersion: "test", APIBase: srv.URL, platformKey: "linux-amd64", skipPinned: true,
	})
	if !errors.Is(err, errInstallInProgress) {
		t.Fatalf("EnsureLocalEngine = %v, want the model step to report an install in progress", err)
	}
	bin, _ := resolveEnginePaths(filepath.Join(root, "engine-test-linux-amd64"), false)
	if !fileExists(bin) {
		t.Errorf("the engine step must not wait on the model's lock: %s missing", bin)
	}
	modelTxn.release()
}

// The lock directory sits beside the installs and holds nothing anyone else
// needs to read.
func TestLockDirectoryIsOwnerOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Go permission bits do not map to Windows ACLs")
	}
	root := t.TempDir()
	requireUmaskAllowsWiderThan0700(t, root)
	lockFor(t, filepath.Join(root, "engine-a"))

	info, err := os.Stat(filepath.Join(root, installLockDir))
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Errorf("%s mode = %o, want 0700", installLockDir, got)
	}
}

// DestRoot is created lazily by the download that follows, and lockutil opens
// the root with O_DIRECTORY, so a first run has no root to lock in.
func TestLockDestinationCreatesTheDestRootFirst(t *testing.T) {
	root := filepath.Join(t.TempDir(), "stt", "nested")
	txn, err := lockDestination(context.Background(), root, "engine-a")
	if err != nil {
		t.Fatalf("lockDestination on a DestRoot that does not exist yet: %v", err)
	}
	defer txn.release()
	if _, err := os.Stat(filepath.Join(root, installLockDir, "engine-a.lock")); err != nil {
		t.Errorf("the lock file should sit under the created root: %v", err)
	}
}

// A wait that runs out is not an install failure. The other process was almost
// certainly installing the same thing, so the destination is re-checked before
// anyone is told anything went wrong, and what comes back when it really is
// missing is a named outcome the caller can report rather than a raw timeout.
func TestEnsureLocalEngineTreatsAnExpiredWaitAsBenign(t *testing.T) {
	shortenInstallLockWait(t, 100*time.Millisecond)

	t.Run("a usable destination means the other install finished", func(t *testing.T) {
		root := t.TempDir()
		engineDir := filepath.Join(root, "engine-test-linux-amd64")
		bin, server := enginePaths(engineDir, false)
		if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
			t.Fatal(err)
		}
		for _, p := range []string{bin, server} {
			if err := os.WriteFile(p, []byte("planted"), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		modelDir := filepath.Join(root, "model-moonshine-tiny-en-int8")
		if err := os.MkdirAll(modelDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(modelDir, "tokens.txt"), []byte("planted"), 0o644); err != nil {
			t.Fatal(err)
		}
		txn := lockFor(t, engineDir)
		defer txn.release()

		// Nothing is listening on the API base, so any download attempt fails
		// outright rather than quietly re-installing what is already there.
		comp, err := EnsureLocalEngine(context.Background(), DownloadOptions{
			DestRoot: root, EngineVersion: "test", APIBase: offlineAPIBase(t), platformKey: "linux-amd64", skipPinned: true,
		})
		if err != nil {
			t.Fatalf("an expired wait over a usable destination must read as installed: %v", err)
		}
		if comp.BinaryPath != bin {
			t.Errorf("BinaryPath = %q, want the install already at the destination %q", comp.BinaryPath, bin)
		}
	})

	t.Run("an unusable destination is a named in-progress outcome", func(t *testing.T) {
		root := t.TempDir()
		engineDir := filepath.Join(root, "engine-test-linux-amd64")
		txn := lockFor(t, engineDir)
		defer txn.release()

		_, err := EnsureLocalEngine(context.Background(), DownloadOptions{
			DestRoot: root, EngineVersion: "test", APIBase: offlineAPIBase(t), platformKey: "linux-amd64", skipPinned: true,
		})
		if !errors.Is(err, errInstallInProgress) {
			t.Fatalf("EnsureLocalEngine = %v, want an error satisfying errInstallInProgress", err)
		}
		if _, err := os.Lstat(engineDir); !os.IsNotExist(err) {
			t.Errorf("nothing should have been created for a destination that was never locked: %v", err)
		}
	})
}

// The grammar is the first ownership filter: a name that does not read back as
// one createSequencedHolder wrote is not this code's to move or delete. Loose
// parsing is what let a sibling merely NAMED like a holder be attributed to the
// install, which is the review's P3 finding on this site.
func TestHolderStampRequiresTheExactGrammar(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "engine-1.2.3-linux-x64")
	base := filepath.Base(dest)
	for _, tc := range []struct {
		name string
		want int64
		ok   bool
	}{
		{base + ".previous-00000000000000000042-seq", 42, true},
		{base + ".kept-00000000000000000042-seq", 42, true},
		{base + ".previous-42-seq", 0, false},
		{base + ".previous-00000000000000000042-x7Kq3", 0, false},
		{base + ".previous-1234567890", 0, false},
		{base + ".previous-00000000000000000042-seq-extra", 0, false},
		{base + ".previous-0000000000000000004a-seq", 0, false},
		{base + ".previous-notes", 0, false},
		{base + ".previous-", 0, false},
		{base + ".kept-42-seq", 0, false},
		{"other-install.previous-00000000000000000042-seq", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := holderStamp(dest, filepath.Join(filepath.Dir(dest), tc.name))
			if ok != tc.ok || got != tc.want {
				t.Errorf("holderStamp(%q) = (%d, %v), want (%d, %v)", tc.name, got, ok, tc.want, tc.ok)
			}
		})
	}
}

// The allocator counts the Kept prefix as well as the scanned one. Recovery
// parks a holder by renaming it under .kept-, and a sequence that stopped
// counting there would hand the next promotion a number a parked copy already
// carries, so the park would collide or the ordering would repeat.
func TestNextHolderSeqCountsKeptNames(t *testing.T) {
	root := t.TempDir()
	dest := filepath.Join(root, "engine-1.2.3-linux-x64")
	kept := fmt.Sprintf("%s%s%020d%s", dest, keptSuffix, 9, holderSeqSuffix)
	if err := os.MkdirAll(kept, 0o700); err != nil {
		t.Fatal(err)
	}

	seq, err := nextHolderSeq(dest)
	if err != nil {
		t.Fatal(err)
	}
	if seq != 10 {
		t.Errorf("nextHolderSeq = %d, want 10: a parked copy already holds sequence 9", seq)
	}
}

// The review's P3 reproduction. A directory whose NAME collides with the holder
// prefix is not this install's copy, and attributing one by name is what put a
// user's unrelated directory in reach of recovery's moves and deletes.
func TestOwnedHoldersBesideIgnoresAPrefixCollidingSibling(t *testing.T) {
	root := t.TempDir()
	dest := filepath.Join(root, "model-a")
	other := filepath.Join(root, "model-b")

	mine := fmt.Sprintf("%s%s%020d%s", dest, holderSuffix, 1, holderSeqSuffix)
	makeDir(t, mine)
	if err := writeHolderMarker(mine, txnMarker{Kind: holderMarkerKind, Dest: "model-a", Seq: 1}); err != nil {
		t.Fatal(err)
	}
	// Named for model-a, marked for model-b: evidence it belongs to another
	// destination's transaction, so model-a's pass has no claim on it at all.
	foreign := fmt.Sprintf("%s%s%020d%s", dest, holderSuffix, 2, holderSeqSuffix)
	makeDir(t, foreign)
	if err := writeHolderMarker(foreign, txnMarker{Kind: holderMarkerKind, Dest: filepath.Base(other), Seq: 2}); err != nil {
		t.Fatal(err)
	}
	// A name the grammar rejects: a user's own directory that happens to start
	// the same way.
	collide := dest + holderSuffix + "mine"
	makeDir(t, collide)
	// The grammar passes and no marker backs it: retained and reported, never
	// restored and never deleted.
	unmarked := fmt.Sprintf("%s%s%020d%s", dest, holderSuffix, 3, holderSeqSuffix)
	makeDir(t, unmarked)

	owned, unowned, _, _ := ownedHoldersBeside(dest)

	gotOwned := make([]string, 0, len(owned))
	for _, c := range owned {
		gotOwned = append(gotOwned, c.path)
	}
	if want := []string{mine}; !slices.Equal(gotOwned, want) {
		t.Errorf("owned = %v, want %v", gotOwned, want)
	}
	if want := []string{unmarked}; !slices.Equal(unowned, want) {
		t.Errorf("unowned = %v, want %v", unowned, want)
	}
	for _, path := range []string{foreign, collide} {
		if slices.Contains(gotOwned, path) || slices.Contains(unowned, path) {
			t.Errorf("%s is not this destination's to classify", filepath.Base(path))
		}
	}
	if len(owned) == 1 && owned[0].seq != 1 {
		t.Errorf("owned seq = %d, want 1", owned[0].seq)
	}
}

// makeDir creates one directory the fixtures need, with the mode the allocator
// gives a holder.
func makeDir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
}

// Parking is how a copy recovery will not restore and cannot prove superseded
// leaves the scan without leaving the disk: the same directory under a prefix
// the scan does not enumerate, keeping its sequence so the operator can still
// name it.
func TestParkKeptHolderMovesTheCopyUnderTheKeptPrefix(t *testing.T) {
	root := t.TempDir()
	dest := filepath.Join(root, "engine-1.2.3-linux-x64")
	holder := plantHolder(t, dest, 7, "kept", false)

	if err := parkKeptHolder(holder); err != nil {
		t.Fatalf("park: %v", err)
	}
	kept := fmt.Sprintf("%s%s%020d%s", dest, keptSuffix, 7, holderSeqSuffix)
	got, err := os.ReadFile(filepath.Join(kept, "install", "engine"))
	if err != nil || string(got) != "kept" {
		t.Fatalf("parked copy = %q (err %v), want %q under %s", got, err, "kept", filepath.Base(kept))
	}
	if _, err := os.Stat(holder); !os.IsNotExist(err) {
		t.Errorf("the holder should be gone from the scanned prefix, got %v", err)
	}
	owned, unowned, _, _ := ownedHoldersBeside(dest)
	if len(owned) != 0 || len(unowned) != 0 {
		t.Errorf("a parked copy must leave the scan: owned %v unowned %v", owned, unowned)
	}
}

// A park that would have to clear something first is a park onto a copy that is
// not ours to remove, so it fails and leaves both where they are. Only a
// clobbering implementation (remove the destination, then rename) can lose the
// occupant.
func TestParkKeptHolderDoesNotClobber(t *testing.T) {
	root := t.TempDir()
	dest := filepath.Join(root, "engine-1.2.3-linux-x64")
	holder := plantHolder(t, dest, 7, "new", false)
	occupied := fmt.Sprintf("%s%s%020d%s", dest, keptSuffix, 7, holderSeqSuffix)
	makeDir(t, filepath.Join(occupied, "install"))
	if err := os.WriteFile(filepath.Join(occupied, "install", "engine"), []byte("older"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := parkKeptHolder(holder); err == nil {
		t.Error("parking onto an occupied Kept name must fail")
	}
	got, err := os.ReadFile(filepath.Join(occupied, "install", "engine"))
	if err != nil || string(got) != "older" {
		t.Errorf("the occupant was clobbered: %q err %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(holder, "install", "engine")); err != nil {
		t.Errorf("a failed park must leave the holder where it is: %v", err)
	}
}

// soleHolder returns the one holder beside destDir, failing when there is not
// exactly one.
func soleHolder(t *testing.T, destDir string) string {
	t.Helper()
	holders := holdersFor(t, destDir)
	if len(holders) != 1 {
		t.Fatalf("want exactly one holder beside %s, got %v", destDir, holders)
	}
	return holders[0]
}

// hasFile reports whether path exists.
func hasFile(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Lstat(path)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return err == nil
}

// The marker goes in before the first destructive rename. A holder that takes
// the only copy of an install before it can be proven ours is one recovery has
// to leave in place forever, since nothing on disk says who wrote it.
func TestPromoteStagedDirWritesTheMarkerBeforeSettingAside(t *testing.T) {
	root := t.TempDir()
	dest := filepath.Join(root, "engine-1.2.3-linux-x64")
	stagedTree(t, dest, "old")
	stage := stagedTree(t, filepath.Join(root, "stage"), "new")

	injectFault(t, "rename", func(args ...string) bool {
		return filepath.Base(args[1]) == "install"
	}, errors.New("injected set-aside failure"))
	// The holder is cleaned up on this failure path, which is right and would
	// also erase what this test is asserting on.
	injectFault(t, "removeAll", nil, errors.New("injected cleanup failure"))

	if err := promoteStagedDir(lockFor(t, dest), stage, dest, "engine", nil); err == nil {
		t.Fatal("a failed set-aside must be reported")
	}
	holder := soleHolder(t, dest)
	m, err := readHolderMarker(holder)
	if err != nil {
		t.Fatalf("the holder was filled before it carried a marker: %v", err)
	}
	seq, ok := holderStamp(dest, holder)
	if !ok {
		t.Fatalf("the allocator wrote a name the grammar rejects: %q", filepath.Base(holder))
	}
	if m.Kind != holderMarkerKind || m.Dest != filepath.Base(dest) || m.Seq != seq {
		t.Errorf("marker = %+v, want kind %q dest %q seq %d", m, holderMarkerKind, filepath.Base(dest), seq)
	}
	if hasFile(t, filepath.Join(holder, "install")) {
		t.Error("the set-aside failed, so nothing should have moved into the holder")
	}
	got, err := os.ReadFile(filepath.Join(dest, "engine"))
	if err != nil || string(got) != "old" {
		t.Errorf("the previous install must be untouched: %q err %v", got, err)
	}
}

// The same ordering on recovery's own set-aside. Only promoteStagedDir's copy of
// this window was pinned, and the recovery path writes its own: a husk moved
// into a holder before the marker lands is the only copy of whatever was at the
// destination, sitting in a directory nothing on disk attributes, which recovery
// can then neither restore from nor ever reclaim.
func TestSetAsideUnusableDestWritesTheMarkerBeforeMovingTheHusk(t *testing.T) {
	root := t.TempDir()
	dest := filepath.Join(root, "engine-1.2.3-linux-x64")
	// A destination that exists and holds no usable install, plus a usable copy
	// beside it: the one state that reaches the set-aside, since the husk moves
	// only after a candidate has been selected.
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dest, "husk"), []byte("partial"), 0o644); err != nil {
		t.Fatal(err)
	}
	candidate := plantHolder(t, dest, 1, "the copy", false)

	injectFault(t, "rename", func(args ...string) bool {
		return args[0] == dest
	}, errors.New("injected set-aside failure"))
	// The holder is removed on this failure path, which is right and would also
	// erase what this test is asserting on.
	injectFault(t, "removeAll", nil, errors.New("injected cleanup failure"))

	restoreInterruptedPromotion(lockFor(t, dest), dest, testPublished, nil)

	var husk string
	for _, h := range holdersFor(t, dest) {
		if h != candidate {
			if husk != "" {
				t.Fatalf("want one holder beside the candidate, got %v", holdersFor(t, dest))
			}
			husk = h
		}
	}
	if husk == "" {
		t.Fatal("the set-aside allocated no holder, so this test proves nothing about the order")
	}
	m, err := readHolderMarker(husk)
	if err != nil {
		t.Fatalf("the holder was moved into before it carried a marker: %v", err)
	}
	seq, ok := holderStamp(dest, husk)
	if !ok {
		t.Fatalf("the allocator wrote a name the grammar rejects: %q", filepath.Base(husk))
	}
	if m.Kind != holderMarkerKind || m.Dest != filepath.Base(dest) || m.Seq != seq {
		t.Errorf("marker = %+v, want kind %q dest %q seq %d", m, holderMarkerKind, filepath.Base(dest), seq)
	}
	if hasFile(t, filepath.Join(husk, "install")) {
		t.Error("the set-aside failed, so nothing should have moved into the holder")
	}
	if got, err := os.ReadFile(filepath.Join(dest, "husk")); err != nil || string(got) != "partial" {
		t.Errorf("the destination must be left exactly as found: %q err %v", got, err)
	}
}

// A torn marker write must leave no marker at all: the atomic rename is what
// makes "missing" and "ours" the only two answers a crash can produce, so a
// partial file can never be read as ownership.
func TestPromoteStagedDirMarkerWriteIsAtomic(t *testing.T) {
	root := t.TempDir()
	dest := filepath.Join(root, "engine-1.2.3-linux-x64")
	stagedTree(t, dest, "old")
	stage := stagedTree(t, filepath.Join(root, "stage"), "new")

	injectFault(t, "rename", func(args ...string) bool {
		return filepath.Base(args[1]) == holderMarkerFile
	}, errors.New("injected marker publish failure"))
	injectFault(t, "removeAll", nil, errors.New("injected cleanup failure"))

	if err := promoteStagedDir(lockFor(t, dest), stage, dest, "engine", nil); err == nil {
		t.Fatal("a failed marker write must be reported")
	}
	holder := soleHolder(t, dest)
	if hasFile(t, filepath.Join(holder, holderMarkerFile)) {
		t.Error("a marker that was never published must not be readable")
	}
	if _, err := readHolderMarker(holder); !errors.Is(err, errMarkerMissing) {
		t.Errorf("readHolderMarker = %v, want errMarkerMissing", err)
	}
	got, err := os.ReadFile(filepath.Join(dest, "engine"))
	if err != nil || string(got) != "old" {
		t.Errorf("the previous install must be untouched: %q err %v", got, err)
	}
}

// The commit flag is what proves to a later pass that the copy in the holder was
// superseded by a publish that actually landed. It goes in after the publish and
// before the cleanup, so a crash in that window costs a retained copy rather
// than the install.
func TestPromoteStagedDirCreatesTheCommitFlagAfterPublish(t *testing.T) {
	root := t.TempDir()
	dest := filepath.Join(root, "engine-1.2.3-linux-x64")
	stagedTree(t, dest, "old")
	stage := stagedTree(t, filepath.Join(root, "stage"), "new")

	// Fail only the final cleanup, so the holder the flag was written into is
	// still there to assert on.
	injectFault(t, "removeAll", nil, errors.New("injected cleanup failure"))

	if err := promoteStagedDir(lockFor(t, dest), stage, dest, "engine", nil); err != nil {
		t.Fatalf("promote: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dest, "engine"))
	if err != nil || string(got) != "new" {
		t.Fatalf("engine = %q err %v, want %q", got, err, "new")
	}
	holder := soleHolder(t, dest)
	if !hasFile(t, filepath.Join(holder, committedFile)) {
		t.Error("a published promotion must mark the copy it superseded")
	}
	if kept, err := os.ReadFile(filepath.Join(holder, "install", "engine")); err != nil || string(kept) != "old" {
		t.Errorf("the superseded copy = %q err %v, want %q", kept, err, "old")
	}
}

// A publish that never happened must leave no evidence that it did. The flag is
// the only thing licensing a later pass to delete the copy in the holder, so a
// flag beside a copy that was never superseded authorizes deleting the only
// install the user has. Both renames into the destination fail, which is the
// state a crash mid-publish leaves: the holder retained, holding everything.
func TestPromoteStagedDirPublishFailureLeavesNoCommitFlag(t *testing.T) {
	root := t.TempDir()
	dest := filepath.Join(root, "engine-1.2.3-linux-x64")
	stagedTree(t, dest, "old")
	stage := stagedTree(t, filepath.Join(root, "stage"), "new")

	injectFault(t, "rename", func(args ...string) bool {
		return args[1] == dest
	}, errors.New("injected publish failure"))

	if err := promoteStagedDir(lockFor(t, dest), stage, dest, "engine", nil); err == nil {
		t.Fatal("a failed publish must be reported")
	}
	if _, err := os.Lstat(dest); !os.IsNotExist(err) {
		t.Fatalf("neither rename into %s succeeded, so it should be absent, got %v", dest, err)
	}
	holder := soleHolder(t, dest)
	if kept, err := os.ReadFile(filepath.Join(holder, "install", "engine")); err != nil || string(kept) != "old" {
		t.Fatalf("the only copy = %q err %v, want it retained as %q", kept, err, "old")
	}
	if found := findNamed(t, root, committedFile); len(found) != 0 {
		t.Errorf("a promotion that never published must leave no commit flag, found %v", found)
	}
}

// A commit flag that cannot be written leaves the superseded copy on disk. The
// alternative is deleting a whole install on the word of a step that just
// failed, and the report is what tells the user where the copy went.
func TestPromoteStagedDirFailedCommitFlagKeepsTheHolder(t *testing.T) {
	root := t.TempDir()
	dest := filepath.Join(root, "engine-1.2.3-linux-x64")
	stagedTree(t, dest, "old")
	stage := stagedTree(t, filepath.Join(root, "stage"), "new")

	injectFault(t, "create", func(args ...string) bool {
		return filepath.Base(args[0]) == committedFile
	}, errors.New("injected commit flag failure"))
	var reported []string
	report := func(line string) { reported = append(reported, line) }

	if err := promoteStagedDir(lockFor(t, dest), stage, dest, "engine", report); err != nil {
		t.Fatalf("a published install must not be reported as a failure: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dest, "engine"))
	if err != nil || string(got) != "new" {
		t.Fatalf("engine = %q err %v, want the published %q", got, err, "new")
	}
	holder := soleHolder(t, dest)
	if kept, err := os.ReadFile(filepath.Join(holder, "install", "engine")); err != nil || string(kept) != "old" {
		t.Errorf("the superseded copy = %q err %v, want it retained as %q", kept, err, "old")
	}
	if hasFile(t, filepath.Join(holder, committedFile)) {
		t.Error("the flag write failed, so no flag should exist")
	}
	if !slices.ContainsFunc(reported, func(line string) bool { return strings.Contains(line, holder) }) {
		t.Errorf("the report must name the retained copy, got %v", reported)
	}
}

// findNamed lists every path under root whose base name is name.
func findNamed(t *testing.T, root, name string) []string {
	t.Helper()
	var found []string
	if err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Name() == name {
			found = append(found, path)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return found
}

// ---- recovery reconciliation ----------------------------------------------

// Which copy is NEWEST and which copy is USABLE are different questions, and the
// review's P2 is what happens when the code answers only the first: a stop
// between the holder's creation and the set-aside leaves a newer holder whose
// install directory is there and empty, and restoring from it publishes nothing
// over a destination that has nothing either. The caller's predicate is what
// decides, and it has to reach every candidate, not just the winner.
func TestRestoreInterruptedPromotionAppliesThePredicateToEveryHolder(t *testing.T) {
	root := t.TempDir()
	dest := filepath.Join(root, "engine-1.2.3-linux-x64")
	usable := plantHolder(t, dest, 1, "the only copy", false)
	// Newer, owned, and holding an install directory with nothing in it: the
	// exact shape a Stat-only check reads as the copy to restore.
	newer := plantHolder(t, dest, 2, "", false)
	if err := os.Remove(filepath.Join(newer, "install", "engine")); err != nil {
		t.Fatal(err)
	}

	var reported []string
	restoreInterruptedPromotion(lockFor(t, dest), dest, testPublished, reporterFor(&reported))

	got, err := os.ReadFile(filepath.Join(dest, "engine"))
	if err != nil || string(got) != "the only copy" {
		t.Fatalf("recovery published a copy its own caller cannot use: %q err %v", got, err)
	}
	if _, err := os.Stat(usable); !os.IsNotExist(err) {
		t.Errorf("the restored copy's holder should be cleared, got %v", err)
	}
	parked := keptName(t, dest, newer)
	if _, err := os.Stat(parked); err != nil {
		t.Errorf("the unusable copy should be kept under the Kept prefix: %v", err)
	}
	assertReports(t, reported, parked)
}

// Falling back to an older copy after the newest one could not be restored is
// what manufactures the provenance loss: an older tree lands at the destination
// and the next pass reads it as evidence that the newer copy was superseded. So
// a failed restore stops the destination, keeps every copy, and says so.
func TestRestoreInterruptedPromotionStopsWhenTheNewestRestoreFails(t *testing.T) {
	root := t.TempDir()
	dest := filepath.Join(root, "engine-1.2.3-linux-x64")
	older := plantHolder(t, dest, 1, "older", false)
	newest := plantHolder(t, dest, 2, "newest", false)
	txn := lockFor(t, dest)

	// The fault belongs to the two failing passes only, so the third pass runs
	// against a real filesystem and shows the retry the report promises.
	func() {
		saved := holderFS
		defer func() { holderFS = saved }()
		injectFault(t, "rename", func(args ...string) bool {
			return args[0] == filepath.Join(newest, "install")
		}, errors.New("injected restore failure"))

		for pass := 1; pass <= 2; pass++ {
			var reported []string
			restoreInterruptedPromotion(txn, dest, testPublished, reporterFor(&reported))
			if _, err := os.Lstat(dest); !os.IsNotExist(err) {
				t.Fatalf("pass %d: an older copy was installed after the newest failed: %v", pass, err)
			}
			for _, holder := range []string{older, newest} {
				if _, err := os.Stat(filepath.Join(holder, "install", "engine")); err != nil {
					t.Errorf("pass %d: every copy must be kept where it is: %v", pass, err)
				}
			}
			assertReports(t, reported, newest)
		}
	}()

	restoreInterruptedPromotion(txn, dest, testPublished, nil)
	got, err := os.ReadFile(filepath.Join(dest, "engine"))
	if err != nil || string(got) != "newest" {
		t.Fatalf("the pass after the fault cleared should restore the newest copy: %q err %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(keptName(t, dest, older), "install", "engine")); err != nil {
		t.Errorf("the older copy must be kept: %v", err)
	}
}

// The review's P2 chain end to end: recovery leaves a partial tree at the
// destination, the caller's predicate rejects it, and an offline start then has
// no engine while a usable copy sits beside it. The exit is to set the husk
// aside and restore the copy, which costs one Kept backup and leaves the user
// with a working install instead of a permanent stuck state.
func TestRestoreInterruptedPromotionExitsTheStuckState(t *testing.T) {
	root := t.TempDir()
	dest := filepath.Join(root, "engine-1.2.3-linux-x64")
	plantUnusableDest(t, dest)
	holder := plantHolder(t, dest, 1, "the only copy", false)
	txn := lockFor(t, dest)

	var reported []string
	restoreInterruptedPromotion(txn, dest, testPublished, reporterFor(&reported))

	got, err := os.ReadFile(filepath.Join(dest, "engine"))
	if err != nil || string(got) != "the only copy" {
		t.Fatalf("the usable copy should be live at the destination: %q err %v", got, err)
	}
	if _, err := os.Stat(holder); !os.IsNotExist(err) {
		t.Errorf("the restored copy's holder should be cleared, got %v", err)
	}
	// The husk moves into a FRESH sequenced holder, above the one the copy came
	// out of, so holders never nest and the operator can name it.
	husk := filepath.Join(keptName(t, dest, fmt.Sprintf("%s%s%020d%s", dest, holderSuffix, 2, holderSeqSuffix)), "install")
	if _, err := os.Stat(filepath.Join(husk, "bin", "README")); err != nil {
		t.Errorf("the husk must be kept, not deleted: %v", err)
	}
	assertReports(t, reported, filepath.Dir(husk))

	// No memory: the destination is usable now and the husk is under the Kept
	// prefix, so there is nothing left to rule on.
	reported = nil
	restoreInterruptedPromotion(txn, dest, testPublished, reporterFor(&reported))
	if got, err := os.ReadFile(filepath.Join(dest, "engine")); err != nil || string(got) != "the only copy" {
		t.Errorf("the second pass changed the live install: %q err %v", got, err)
	}
	if len(reported) != 0 {
		t.Errorf("the second pass should have nothing to report, got %v", reported)
	}

	// A later real install sets the now-usable destination aside as an ordinary
	// holder and the Kept husk is still where recovery put it.
	stage := stagedTree(t, filepath.Join(root, "stage"), "newest")
	if err := promoteStagedDir(txn, stage, dest, "engine", nil); err != nil {
		t.Fatalf("the later install failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(husk, "bin", "README")); err != nil {
		t.Errorf("a later install must not disturb a Kept backup: %v", err)
	}
}

// An unusable destination with nothing usable beside it is not a state recovery
// can improve, and moving the husk out of the way anyway would leave the caller
// with no destination at all. So the destination is left exactly as found and
// only the report changes.
func TestRestoreInterruptedPromotionLeavesAnUnusableDestinationWithNoCandidateAlone(t *testing.T) {
	root := t.TempDir()
	dest := filepath.Join(root, "engine-1.2.3-linux-x64")
	plantUnusableDest(t, dest)
	// Owned, uncommitted, and holding an install nothing can use.
	unusable := plantHolder(t, dest, 1, "", false)
	if err := os.Remove(filepath.Join(unusable, "install", "engine")); err != nil {
		t.Fatal(err)
	}
	unowned := plantUnowned(t, dest, 2, "not ours")
	txn := lockFor(t, dest)

	for pass := 1; pass <= 2; pass++ {
		var reported []string
		restoreInterruptedPromotion(txn, dest, testPublished, reporterFor(&reported))

		if _, err := os.Stat(filepath.Join(dest, "bin", "README")); err != nil {
			t.Fatalf("pass %d: the destination must be left exactly as found: %v", pass, err)
		}
		if ok, _ := testPublished(dest); ok {
			t.Fatalf("pass %d: nothing was restored, so the destination cannot have become usable", pass)
		}
		parked := keptName(t, dest, unusable)
		if _, err := os.Stat(filepath.Join(parked, "install")); err != nil {
			t.Errorf("pass %d: the unusable copy should be kept under the Kept prefix: %v", pass, err)
		}
		if _, err := os.Stat(filepath.Join(unowned, "install", "engine")); err != nil {
			t.Errorf("pass %d: an unowned directory is never moved: %v", pass, err)
		}
		if pass == 1 {
			assertReports(t, reported, parked, unowned, dest)
		} else {
			assertReports(t, reported, unowned, dest)
		}
	}
}

// The husk moves only after a candidate is chosen, and it moves BACK when the
// restore fails. Parking it first would strand the destination's own contents
// under a prefix recovery never enumerates, which is the one outcome worse than
// the stuck state this branch exists to exit.
func TestRestoreInterruptedPromotionPutsTheHuskBackWhenTheRestoreFails(t *testing.T) {
	root := t.TempDir()
	dest := filepath.Join(root, "engine-1.2.3-linux-x64")
	plantUnusableDest(t, dest)
	holder := plantHolder(t, dest, 1, "the only copy", false)
	txn := lockFor(t, dest)

	func() {
		saved := holderFS
		defer func() { holderFS = saved }()
		injectFault(t, "rename", func(args ...string) bool {
			return args[0] == filepath.Join(holder, "install")
		}, errors.New("injected restore failure"))

		for pass := 1; pass <= 2; pass++ {
			var reported []string
			restoreInterruptedPromotion(txn, dest, testPublished, reporterFor(&reported))

			if _, err := os.Stat(filepath.Join(dest, "bin", "README")); err != nil {
				t.Fatalf("pass %d: the destination must be left exactly as found: %v", pass, err)
			}
			if _, err := os.Stat(filepath.Join(holder, "install", "engine")); err != nil {
				t.Errorf("pass %d: the copy must stay at its own name: %v", pass, err)
			}
			kept, err := filepath.Glob(dest + keptSuffix + "*")
			if err != nil {
				t.Fatal(err)
			}
			if len(kept) != 0 {
				t.Errorf("pass %d: a failed restore must leave no Kept backup, got %v", pass, kept)
			}
			assertReports(t, reported, holder)
		}
	}()

	restoreInterruptedPromotion(txn, dest, testPublished, nil)
	got, err := os.ReadFile(filepath.Join(dest, "engine"))
	if err != nil || string(got) != "the only copy" {
		t.Fatalf("the pass after the fault cleared should restore the copy: %q err %v", got, err)
	}
	kept, err := filepath.Glob(dest + keptSuffix + "*")
	if err != nil {
		t.Fatal(err)
	}
	if len(kept) != 1 {
		t.Fatalf("the husk should be kept exactly once, got %v", kept)
	}
	if _, err := os.Stat(filepath.Join(kept[0], "install", "bin", "README")); err != nil {
		t.Errorf("the husk must be kept intact: %v", err)
	}
}

// A committed copy is only provably superseded by a destination that is present
// AND usable. With no such destination it is the last usable copy there is, so
// it is the fallback selection rather than something to reclaim.
func TestRestoreInterruptedPromotionRestoresACommittedHolderOverAnUnusableDestination(t *testing.T) {
	for _, tc := range []struct {
		name     string
		seedDest func(t *testing.T, dest string)
		wantHusk bool
	}{
		{
			name:     "unusable destination",
			seedDest: plantUnusableDest,
			wantHusk: true,
		},
		{
			name:     "absent destination",
			seedDest: func(t *testing.T, dest string) {},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			dest := filepath.Join(root, "engine-1.2.3-linux-x64")
			tc.seedDest(t, dest)
			holder := plantHolder(t, dest, 1, "the only copy", true)

			restoreInterruptedPromotion(lockFor(t, dest), dest, testPublished, nil)

			got, err := os.ReadFile(filepath.Join(dest, "engine"))
			if err != nil || string(got) != "the only copy" {
				t.Fatalf("the last usable copy should be live: %q err %v", got, err)
			}
			if _, err := os.Stat(holder); !os.IsNotExist(err) {
				t.Errorf("the restored copy's holder should be cleared, got %v", err)
			}
			kept, err := filepath.Glob(dest + keptSuffix + "*")
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantHusk {
				if len(kept) != 1 {
					t.Fatalf("the husk should be kept exactly once, got %v", kept)
				}
				if _, err := os.Stat(filepath.Join(kept[0], "install", "bin", "README")); err != nil {
					t.Errorf("the husk must be kept intact: %v", err)
				}
				return
			}
			if len(kept) != 0 {
				t.Errorf("nothing was set aside, so there is no Kept backup to write: %v", kept)
			}
		})
	}
}

// A directory that merely starts like a holder name was never this install's,
// so it is neither acted on nor reported on this install's account. Reporting it
// would be as wrong as moving it: it tells the operator a copy of their install
// is somewhere it is not.
func TestRestoreInterruptedPromotionIgnoresAPrefixCollidingSibling(t *testing.T) {
	root := t.TempDir()
	dest := filepath.Join(root, "engine-1.2.3-linux-x64")
	stagedTree(t, dest, "live")
	sibling := dest + holderSuffix + "notes"
	if err := os.MkdirAll(sibling, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sibling, "note"), []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	committed := plantHolder(t, dest, 1, "old", true)

	var reported []string
	restoreInterruptedPromotion(lockFor(t, dest), dest, testPublished, reporterFor(&reported))

	if got, err := os.ReadFile(filepath.Join(sibling, "note")); err != nil || string(got) != "mine" {
		t.Errorf("a prefix-colliding sibling must be left exactly as found: %q err %v", got, err)
	}
	if _, err := os.Stat(committed); !os.IsNotExist(err) {
		t.Errorf("the superseded copy should be reaped, got %v", err)
	}
	for _, m := range reported {
		if strings.Contains(m, sibling) {
			t.Errorf("a sibling that is not this install's must not be reported on its account: %q", m)
		}
	}
}

// Unusable and unreadable are different answers. Unusable is durable and the
// candidate is skipped and kept; unreadable is a filesystem fault, and deciding
// anything on it would turn a transient error into a permanent ruling. So a
// candidate that cannot be read stops the destination with everything intact.
func TestRestoreInterruptedPromotionStopsOnAnUnreadableHolder(t *testing.T) {
	for _, tc := range []struct {
		name string
		// seed makes the holder's install unreadable without touching the holder
		// itself: chmod the holder and the MARKER stops being readable too, the
		// directory is classified unowned, and the branch under test is never
		// reached.
		seed func(t *testing.T, holder string)
		// portable rows run everywhere. The chmod rows do not, and this
		// distinction is the axis the no-fallback rule rests on, so at least one
		// row has to hold on the Windows leg and under root.
		portable bool
	}{
		{
			name:     "the install cannot be listed at the seam",
			portable: true,
			seed: func(t *testing.T, holder string) {
				injectFault(t, "readDir", func(args ...string) bool {
					return args[0] == filepath.Join(holder, "install")
				}, os.ErrPermission)
			},
		},
		{
			name: "install is a symlink through an unreadable directory",
			seed: func(t *testing.T, holder string) {
				t.Helper()
				locked := filepath.Join(holder, "locked")
				if err := os.Rename(filepath.Join(holder, "install"), locked); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(locked, "inner"), filepath.Join(holder, "install")); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(locked, 0o000); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })
			},
		},
		{
			name: "install itself cannot be read",
			seed: func(t *testing.T, holder string) {
				t.Helper()
				install := filepath.Join(holder, "install")
				if err := os.Chmod(install, 0o000); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(install, 0o700) })
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !tc.portable {
				if os.Geteuid() == 0 {
					t.Skip("root ignores the directory permissions this row relies on")
				}
				if runtime.GOOS == "windows" {
					t.Skip("POSIX directory permissions")
				}
			}
			root := t.TempDir()
			dest := filepath.Join(root, "engine-1.2.3-linux-x64")
			// A second usable owned holder sits below the unreadable one, and it
			// is what makes "stopped" distinguishable from "ran out of
			// candidates": with the unreadable holder as the only candidate,
			// turning the stop into a continue leaves every assertion below
			// true, because there is nothing left for the pass to publish or
			// park either way.
			older := plantHolder(t, dest, 1, "the older copy", false)
			holder := plantHolder(t, dest, 2, "the only copy", false)
			tc.seed(t, holder)

			var reported []string
			// The delete is recorded rather than inferred from the disk: a
			// holder whose own contents cannot be read cannot be removed
			// either, so a pass that classified it as empty and ASKED for the
			// delete leaves exactly the same directory behind as one that
			// never touched it.
			removed := recordRemoveAll(t)
			restoreInterruptedPromotion(lockFor(t, dest), dest, testPublished, reporterFor(&reported))

			if _, err := os.Lstat(dest); !os.IsNotExist(err) {
				t.Errorf("nothing readable was found, so nothing may be published: %v", err)
			}
			if _, err := os.Lstat(holder); err != nil {
				t.Errorf("a copy that could not be read must be left where it is: %v", err)
			}
			if slices.Contains(*removed, holder) {
				t.Errorf("a copy that could not be read must never be handed to a delete: %v", *removed)
			}
			// The older copy is the claim the fixture exists for: a pass that
			// carried on past the unreadable holder would publish this one, or
			// park it on the way, and either is a ruling made on a fault.
			if got, err := os.ReadFile(filepath.Join(older, "install", "engine")); err != nil || string(got) != "the older copy" {
				t.Errorf("the older copy must be left exactly as found: %q err %v", got, err)
			}
			if slices.Contains(*removed, older) {
				t.Errorf("the older copy must never be handed to a delete: %v", *removed)
			}
			assertNoOtherEntries(t, root, filepath.Base(older), filepath.Base(holder), installLockDir)
			assertReports(t, reported, holder)
		})
	}
}

// A directory carrying the exact generated name with no marker behind it is one
// this code cannot claim. Restoring from it would publish a stranger's tree as
// the user's install, and deleting it would destroy something that was never
// ours, so it is kept where it is and named.
func TestRestoreInterruptedPromotionRetainsUnownedHolders(t *testing.T) {
	root := t.TempDir()
	dest := filepath.Join(root, "engine-1.2.3-linux-x64")
	unowned := plantUnowned(t, dest, 1, "not ours")
	txn := lockFor(t, dest)

	for pass := 1; pass <= 2; pass++ {
		var reported []string
		restoreInterruptedPromotion(txn, dest, testPublished, reporterFor(&reported))

		if _, err := os.Lstat(dest); !os.IsNotExist(err) {
			t.Errorf("pass %d: an unowned directory must never be restored: %v", pass, err)
		}
		if got, err := os.ReadFile(filepath.Join(unowned, "install", "engine")); err != nil || string(got) != "not ours" {
			t.Errorf("pass %d: an unowned directory must be left exactly as found: %q err %v", pass, got, err)
		}
		assertReports(t, reported, unowned)
	}
}

// The predicate is what tells a published install from a directory that merely
// exists. Without one there is no such evidence, and every ruling recovery could
// make rests on it, so a caller that supplies none gets no mutation at all.
func TestRestoreInterruptedPromotionNilPredicateNeverDeletesOrRestores(t *testing.T) {
	root := t.TempDir()
	dest := filepath.Join(root, "engine-1.2.3-linux-x64")
	stagedTree(t, dest, "live")
	committed := plantHolder(t, dest, 1, "old", true)

	restoreInterruptedPromotion(lockFor(t, dest), dest, nil, nil)

	if got, err := os.ReadFile(filepath.Join(dest, "engine")); err != nil || string(got) != "live" {
		t.Errorf("the destination must be left alone: %q err %v", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(committed, "install", "engine")); err != nil || string(got) != "old" {
		t.Errorf("with no predicate nothing is proven superseded: %q err %v", got, err)
	}
	assertNoOtherEntries(t, root, filepath.Base(dest), filepath.Base(committed), installLockDir)
}

// recordRemoveAll collects every path recovery asks the seam to delete, and
// passes each call through. It is how a test tells "the delete was refused by
// the filesystem" from "the delete was never asked for", which the directory
// left on disk cannot show.
func recordRemoveAll(t *testing.T) *[]string {
	t.Helper()
	var paths []string
	real := holderFS
	t.Cleanup(func() { holderFS = real })
	holderFS.removeAll = func(path string) error {
		paths = append(paths, path)
		return real.removeAll(path)
	}
	return &paths
}

// ---- the reclaim surface ---------------------------------------------------

// plantKeptHolder writes what recovery leaves under the Kept prefix: a holder,
// named and marked the way promoteStagedDir writes one, renamed to the Kept name
// recovery parks it under. Going through the real helpers is what keeps the
// fixture from testing a shape production never writes.
func plantKeptHolder(t *testing.T, destDir string, seq int64, content string) string {
	t.Helper()
	holder := plantHolder(t, destDir, seq, content, false)
	kept := keptName(t, destDir, holder)
	if err := os.Rename(holder, kept); err != nil {
		t.Fatal(err)
	}
	return kept
}

// keptHolderBytes is what the listing should report for a backup
// plantKeptHolder wrote: the marker and the one file in the copy. Summed from
// the two names the fixture created rather than by walking, so the assertion is
// not the implementation restated.
func keptHolderBytes(t *testing.T, kept, content string) int64 {
	t.Helper()
	info, err := os.Stat(filepath.Join(kept, holderMarkerFile))
	if err != nil {
		t.Fatal(err)
	}
	return info.Size() + int64(len(content))
}

func findKept(t *testing.T, list []KeptBackup, path string) KeptBackup {
	t.Helper()
	for _, b := range list {
		if b.Path == path {
			return b
		}
	}
	t.Fatalf("%s is missing from the listing %v", path, list)
	return KeptBackup{}
}

// A Kept backup an operator cannot find is one that can never be reclaimed, and
// this listing is the only thing that finds them: recovery deliberately never
// enumerates the prefix. Here the copy is the only offline copy of an engine or
// a model, so a directory nothing attributes is reported unowned rather than
// dropped: the operator has to be able to see recovery's residue.
func TestListKeptBackupsReportsDestSeqAndSize(t *testing.T) {
	root := t.TempDir()
	engine := filepath.Join(root, "engine-a")
	model := filepath.Join(root, "model-b")
	first := plantKeptHolder(t, engine, 1, "engine-bytes")
	second := plantKeptHolder(t, model, 2, "model")

	// Kept grammar, nothing attributing it: the shape a crash between the mkdir
	// and the marker write leaves, parked by a later pass.
	bare := fmt.Sprintf("%s%s%020d%s", engine, keptSuffix, 3, holderSeqSuffix)
	if err := os.Mkdir(bare, 0o755); err != nil {
		t.Fatal(err)
	}
	// A marker naming another destination is evidence this copy belongs to a
	// transaction that is not the one the name claims.
	skewed := plantKeptHolder(t, engine, 4, "skew")
	if err := writeHolderMarker(skewed, txnMarker{Kind: holderMarkerKind, Dest: "engine-z", Seq: 4}); err != nil {
		t.Fatal(err)
	}
	// A sibling that fails the grammar was never a holder and is not listed.
	if err := os.Mkdir(engine+keptSuffix+"notanumber"+holderSeqSuffix, 0o755); err != nil {
		t.Fatal(err)
	}

	list, err := ListKeptBackups(root)
	if err != nil {
		t.Fatalf("ListKeptBackups: %v", err)
	}
	if len(list) != 4 {
		t.Fatalf("listed %d kept backups, want 4: %v", len(list), list)
	}

	got := findKept(t, list, first)
	if !got.Owned || got.Dest != "engine-a" || got.Seq != 1 || got.Bytes != keptHolderBytes(t, first, "engine-bytes") {
		t.Errorf("first = %+v, want owned engine-a seq 1 bytes %d", got, keptHolderBytes(t, first, "engine-bytes"))
	}
	got = findKept(t, list, second)
	if !got.Owned || got.Dest != "model-b" || got.Seq != 2 || got.Bytes != keptHolderBytes(t, second, "model") {
		t.Errorf("second = %+v, want owned model-b seq 2 bytes %d", got, keptHolderBytes(t, second, "model"))
	}
	for _, path := range []string{bare, skewed} {
		got := findKept(t, list, path)
		if got.Owned {
			t.Errorf("%s is attributed by nothing on disk and must be listed unowned, got %+v", path, got)
		}
		// The destination has to be empty too: naming one off the directory's
		// own name tells the operator this copy belongs to an install the
		// marker does not support.
		if got.Dest != "" {
			t.Errorf("%s: unowned entries carry no destination, got %q", path, got.Dest)
		}
	}
}

// Removal carries the same ownership proof recovery's own deletes carry. This is
// the one thing that deletes a Kept backup here, and the copy it deletes may be
// the only offline copy of an engine, so an entry nothing attributes survives it
// and the operator is told to remove that one by hand.
func TestRemoveKeptBackupRefusesAnUnownedEntry(t *testing.T) {
	t.Run("no marker", func(t *testing.T) {
		root := t.TempDir()
		kept := plantKeptHolder(t, filepath.Join(root, "engine-a"), 1, "engine")
		if err := os.Remove(filepath.Join(kept, holderMarkerFile)); err != nil {
			t.Fatal(err)
		}
		if err := RemoveKeptBackup(root, filepath.Base(kept)); err == nil {
			t.Fatal("a directory with no marker must not be removed")
		}
		if _, err := os.Stat(filepath.Join(kept, "install", "engine")); err != nil {
			t.Errorf("the copy must be left intact: %v", err)
		}
	})
	t.Run("marker disagrees with the sequence", func(t *testing.T) {
		root := t.TempDir()
		kept := plantKeptHolder(t, filepath.Join(root, "engine-a"), 1, "engine")
		if err := writeHolderMarker(kept, txnMarker{Kind: holderMarkerKind, Dest: "engine-a", Seq: 42}); err != nil {
			t.Fatal(err)
		}
		if err := RemoveKeptBackup(root, filepath.Base(kept)); err == nil {
			t.Fatal("a marker for another sequence must not license a removal")
		}
		if _, err := os.Stat(filepath.Join(kept, "install", "engine")); err != nil {
			t.Errorf("the copy must be left intact: %v", err)
		}
	})
	t.Run("marker disagrees with the destination", func(t *testing.T) {
		root := t.TempDir()
		kept := plantKeptHolder(t, filepath.Join(root, "engine-a"), 1, "engine")
		if err := writeHolderMarker(kept, txnMarker{Kind: holderMarkerKind, Dest: "engine-z", Seq: 1}); err != nil {
			t.Fatal(err)
		}
		if err := RemoveKeptBackup(root, filepath.Base(kept)); err == nil {
			t.Fatal("a marker for another destination must not license a removal")
		}
		if _, err := os.Stat(filepath.Join(kept, "install", "engine")); err != nil {
			t.Errorf("the copy must be left intact: %v", err)
		}
	})
}

// The lock is what separates a Kept backup from a copy a live install is about
// to roll back to: on disk the two are the same directory, and removing one
// mid-promotion takes the only copy of the install.
func TestRemoveKeptBackupRefusesWhileTheDestinationIsLocked(t *testing.T) {
	root := t.TempDir()
	kept := plantKeptHolder(t, filepath.Join(root, "engine-a"), 1, "engine")
	lockDir := filepath.Join(root, installLockDir)
	if err := os.MkdirAll(lockDir, 0o700); err != nil {
		t.Fatal(err)
	}
	held, err := lockutil.TryAcquireFileLockAt(root, filepath.Join(lockDir, "engine-a.lock"))
	if err != nil {
		t.Fatalf("take the lock as the other install would: %v", err)
	}
	if err := RemoveKeptBackup(root, filepath.Base(kept)); err == nil {
		_ = held.Release()
		t.Fatal("a removal must not run while another process holds the destination")
	}
	if _, err := os.Stat(filepath.Join(kept, "install", "engine")); err != nil {
		t.Errorf("the copy must be left intact: %v", err)
	}
	if err := held.Release(); err != nil {
		t.Fatal(err)
	}
	if err := RemoveKeptBackup(root, filepath.Base(kept)); err != nil {
		t.Fatalf("the same removal must go through once the lock is free: %v", err)
	}
	if _, err := os.Stat(kept); !os.IsNotExist(err) {
		t.Errorf("the copy should be gone, got %v", err)
	}
}

// The attribution runs twice: once before the lock and once under it. The first
// check happens with nothing excluding a live promotion, so a directory that
// stopped being this install's in between must not be deleted on the strength of
// that reading. Both halves of the second check are driven here, because
// deleting the whole block leaves the package green.
func TestRemoveKeptBackupRefusesWhenAttributionChangesUnderTheLock(t *testing.T) {
	t.Run("the marker goes unreadable", func(t *testing.T) {
		root := t.TempDir()
		kept := plantKeptHolder(t, filepath.Join(root, "engine-a"), 1, "engine")
		marker := filepath.Join(kept, holderMarkerFile)

		var read atomic.Bool
		injectFault(t, "readFile", func(args ...string) bool {
			// The pre-lock check reads the marker first; the re-read under the
			// lock is the one this fault is for.
			return args[0] == marker && !read.CompareAndSwap(false, true)
		}, errors.New("injected marker read failure"))

		err := RemoveKeptBackup(root, filepath.Base(kept))
		if err == nil || !strings.Contains(err.Error(), "no longer attributable") {
			t.Fatalf("RemoveKeptBackup = %v, want a refusal naming the attribution that changed", err)
		}
		if _, err := os.Stat(filepath.Join(kept, "install", "engine")); err != nil {
			t.Errorf("the copy must be left intact: %v", err)
		}
	})
	t.Run("the marker names another install", func(t *testing.T) {
		root := t.TempDir()
		kept := plantKeptHolder(t, filepath.Join(root, "engine-a"), 1, "engine")
		marker := filepath.Join(kept, holderMarkerFile)
		// A readable marker for a different install is the other half: the
		// re-read succeeds and answers with a destination the first one did not,
		// which is what a directory reused by another promotion looks like from
		// here.
		other, err := json.Marshal(txnMarker{Kind: holderMarkerKind, Dest: "engine-z", Seq: 1})
		if err != nil {
			t.Fatal(err)
		}

		real := holderFS
		t.Cleanup(func() { holderFS = real })
		var read atomic.Bool
		holderFS.readFile = func(name string) ([]byte, error) {
			if name != marker || read.CompareAndSwap(false, true) {
				return real.readFile(name)
			}
			return other, nil
		}

		if err := RemoveKeptBackup(root, filepath.Base(kept)); err == nil || !strings.Contains(err.Error(), "no longer attributable") {
			t.Fatalf("RemoveKeptBackup = %v, want a refusal naming the attribution that changed", err)
		}
		if _, err := os.Stat(filepath.Join(kept, "install", "engine")); err != nil {
			t.Errorf("the copy must be left intact: %v", err)
		}
	})
}

// The command takes a name, never a path. The Kept grammar here is a
// destination's own name followed by the suffix, so a name carrying a separator
// still passes it: "../x.kept-...-seq" parses as destination "../x". Only the
// base-name check keeps that from joining to a directory outside the root, and
// it has to run before any filesystem call.
func TestRemoveKeptBackupRejectsANameThatIsNotABaseName(t *testing.T) {
	root := t.TempDir()
	suffix := fmt.Sprintf("%s%020d%s", keptSuffix, 1, holderSeqSuffix)
	for _, step := range []string{"removeAll", "stat", "readFile", "mkdir"} {
		injectFault(t, step, func(args ...string) bool {
			t.Errorf("a rejected name must not reach the filesystem, got %s(%v)", step, args)
			return false
		}, nil)
	}
	names := []string{
		"../x" + suffix,
		"a/b" + suffix,
		filepath.Join(root, "engine-a") + suffix,
		"." + suffix,
		".." + suffix,
		"", ".", "..",
	}
	for _, name := range names {
		if err := RemoveKeptBackup(root, name); err == nil {
			t.Errorf("RemoveKeptBackup(%q) = nil, want a refusal", name)
		}
	}
}

// The scanned prefix is recovery's, not the operator's: a holder under it may
// belong to a promotion running right now, and recovery has a disposition for it
// either way. Only the Kept prefix is this command's to touch.
func TestRemoveKeptBackupNeverTouchesTheScannedPrefix(t *testing.T) {
	root := t.TempDir()
	holder := plantHolder(t, filepath.Join(root, "engine-a"), 1, "engine", false)
	if err := RemoveKeptBackup(root, filepath.Base(holder)); err == nil {
		t.Fatal("a holder under the scanned prefix must not be removed by name")
	}
	if _, err := os.Stat(filepath.Join(holder, "install", "engine")); err != nil {
		t.Errorf("the copy must be left intact: %v", err)
	}
}

// A marker this pass could not read is not a marker proving the holder belongs
// to someone else. Treating the two the same lets recovery walk past the copy
// whose state it failed to establish and install an older one, which is the
// provenance loss the whole no-fallback rule exists to prevent.
func TestRestoreInterruptedPromotionStopsWhenAMarkerCannotBeRead(t *testing.T) {
	root := t.TempDir()
	destDir := filepath.Join(root, "engine-a")
	newest := plantHolder(t, destDir, 2, "v2", false)
	older := plantHolder(t, destDir, 1, "v1", false)
	txn := lockFor(t, destDir)

	injectFault(t, "readFile", func(args ...string) bool {
		return strings.HasPrefix(args[0], newest)
	}, errors.New("injected marker read failure"))

	var reported []string
	restoreInterruptedPromotion(txn, destDir, testPublished, reporterFor(&reported))

	if _, err := os.Lstat(destDir); !os.IsNotExist(err) {
		t.Fatalf("recovery installed a copy while one holder's marker was unreadable: %v", err)
	}
	for _, holder := range []string{newest, older} {
		if _, err := os.Stat(filepath.Join(holder, "install", "engine")); err != nil {
			t.Errorf("every copy must be retained until the unreadable one can be read: %v", err)
		}
	}
	if len(reported) == 0 {
		t.Error("an unreadable marker must be reported")
	}
}

// A handle kept past its release must be refused. Reporting the lock as still
// held after it was given up is the fail-open direction: the guard exists so a
// call site that forgets the lifecycle is stopped, not trusted.
func TestDestTxnDoesNotClaimALockItReleased(t *testing.T) {
	root := t.TempDir()
	destDir := filepath.Join(root, "engine-a")
	txn, err := lockDestination(context.Background(), root, "engine-a")
	if err != nil {
		t.Fatal(err)
	}
	if !txn.holds(destDir) {
		t.Fatal("a fresh handle must hold its own destination")
	}
	txn.release()
	if txn.holds(destDir) {
		t.Error("a released handle must not report that it still holds the destination")
	}
}

// ---- a probe that could not run ---------------------------------------------

// snapshotTree records every path under root with the size of each file, so a
// test can assert nothing moved, was parked, or was removed. Names alone would
// miss a copy restored over a destination that already existed.
func snapshotTree(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		if d.IsDir() {
			// The lock directory is the mechanism, not install state: it is
			// created by taking a lock at all, so counting it would report a
			// change on a pass that touched nothing.
			if rel == installLockDir {
				return filepath.SkipDir
			}
			out = append(out, rel+"/")
			return nil
		}
		info, infoErr := d.Info()
		if infoErr != nil {
			return infoErr
		}
		out = append(out, fmt.Sprintf("%s %d", rel, info.Size()))
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
	slices.Sort(out)
	return out
}

// plantEngineHolder plants an interrupted promotion whose copy is a real engine
// layout: bin/ holds the executables and lib/ is the second directory the
// tarball ships, which keeps flattenSingleChild from redirecting the probe to a
// path the fault does not name.
func plantEngineHolder(t *testing.T, destDir string, seq int64) string {
	t.Helper()
	holder := plantHolder(t, destDir, seq, "engine copy", false)
	install := filepath.Join(holder, "install")
	if err := os.MkdirAll(filepath.Join(install, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(install, "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	bin, server := enginePaths(install, false)
	for _, path := range []string{bin, server} {
		if err := os.WriteFile(path, []byte("x"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return holder
}

// A scan that never ran is not a scan that found nothing. With the parent
// unreadable there is no evidence about any copy beside the destination, so
// EnsureLocalEngine has to stop before its idempotency check rather than read
// the empty result as "nothing to recover" and download a replacement over an
// install that is sitting right there.
func TestEnsureLocalEngineStopsWhenTheHolderScanCannotBeRead(t *testing.T) {
	root := t.TempDir()
	engineDir := filepath.Join(root, "engine-test-linux-amd64")
	holder := plantEngineHolder(t, engineDir, 100)
	modelDir := filepath.Join(root, "model-moonshine-tiny-en-int8")
	if err := os.MkdirAll(modelDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(modelDir, "tokens.txt"), []byte("t"), 0o644); err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, "the release API must not be reached on unknown state", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	injected := errors.New("injected parent listing failure")
	faulted := true
	injectFault(t, "readDir", func(args ...string) bool { return faulted && args[0] == root }, injected)
	before := snapshotTree(t, root)

	_, err := EnsureLocalEngine(context.Background(), DownloadOptions{
		DestRoot: root, EngineVersion: "test", APIBase: srv.URL, platformKey: "linux-amd64", skipPinned: true,
	})
	if !errors.Is(err, injected) {
		t.Fatalf("EnsureLocalEngine = %v, want the scan failure", err)
	}
	if n := requests.Load(); n != 0 {
		t.Errorf("the release API was contacted %d times on a state nothing could read", n)
	}
	if got := snapshotTree(t, root); !slices.Equal(got, before) {
		t.Errorf("nothing may move while the scan is unreadable:\nbefore %v\nafter  %v", before, got)
	}

	// Same disk, fault removed: the holder is what the install is, and putting
	// it back must need no download at all.
	faulted = false
	comp, err := EnsureLocalEngine(context.Background(), DownloadOptions{
		DestRoot: root, EngineVersion: "test", APIBase: srv.URL, platformKey: "linux-amd64", skipPinned: true,
	})
	if err != nil {
		t.Fatalf("the retry from the unchanged state should succeed: %v", err)
	}
	if n := requests.Load(); n != 0 {
		t.Errorf("the recovered install needed no download, but the API was contacted %d times", n)
	}
	if !fileExists(comp.BinaryPath) {
		t.Errorf("the restored engine binary is missing at %q", comp.BinaryPath)
	}
	if _, err := os.Stat(holder); !os.IsNotExist(err) {
		t.Errorf("the restored holder should be cleared, got %v", err)
	}
}

// The usability predicate is the whole basis of recovery's rulings, so a probe
// that could not run must stop the destination. Reading a permission error as
// "this copy is not usable" would skip the newest copy, park it, and restore an
// older one over the destination, which is the provenance loss the no-fallback
// rule exists to prevent.
func TestRestoreInterruptedPromotionStopsWhenTheProbeCannotRun(t *testing.T) {
	injected := errors.New("injected probe failure")
	for _, tc := range []struct {
		name string
		// plant writes one holder's copy and returns the path the probe reads,
		// which is the path the fault names.
		plant     func(t *testing.T, destDir string, seq int64) (holder, probed string)
		step      string
		published usability
		seedDest  func(t *testing.T, destDir string)
	}{
		{
			name: "the engine executable cannot be stat-ed",
			step: "stat",
			plant: func(t *testing.T, destDir string, seq int64) (string, string) {
				t.Helper()
				holder := plantEngineHolder(t, destDir, seq)
				bin, _ := enginePaths(filepath.Join(holder, "install"), false)
				return holder, bin
			},
			published: func(dir string) (bool, error) {
				bin, _ := resolveEnginePaths(dir, false)
				return fileExistsErr(bin)
			},
			seedDest: plantUnusableDest,
		},
		{
			name: "the model's nested directory cannot be listed",
			step: "readDir",
			plant: func(t *testing.T, destDir string, seq int64) (string, string) {
				t.Helper()
				holder := plantHolder(t, destDir, seq, "model copy", false)
				nested := filepath.Join(holder, "install", "moonshine")
				if err := os.MkdirAll(nested, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(nested, "tokens.txt"), []byte("t"), 0o644); err != nil {
					t.Fatal(err)
				}
				return holder, nested
			},
			published: dirHasModel,
			seedDest: func(t *testing.T, destDir string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(destDir, "something.onnx"), []byte("x"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			dest := filepath.Join(root, "engine-1.2.3-linux-x64")
			if err := os.MkdirAll(dest, 0o755); err != nil {
				t.Fatal(err)
			}
			tc.seedDest(t, dest)
			older, _ := tc.plant(t, dest, 100)
			newer, probed := tc.plant(t, dest, 200)

			faulted := true
			injectFault(t, tc.step, func(args ...string) bool { return faulted && args[0] == probed }, injected)
			before := snapshotTree(t, root)
			var reported []string

			txn := lockFor(t, dest)
			restoreInterruptedPromotion(txn, dest, tc.published, reporterFor(&reported))

			if got := snapshotTree(t, root); !slices.Equal(got, before) {
				t.Errorf("neither copy may move while a probe cannot run:\nbefore %v\nafter  %v", before, got)
			}
			assertReports(t, reported, injected.Error())
			if _, err := os.Stat(filepath.Join(older, "install")); err != nil {
				t.Errorf("the older copy must be left exactly where it is: %v", err)
			}

			// Same disk, fault removed: the newest copy is the one restored.
			faulted = false
			txn.release()
			restoreInterruptedPromotion(lockFor(t, dest), dest, tc.published, nil)

			if ok, err := tc.published(dest); !ok || err != nil {
				t.Errorf("the retry should restore a usable install at %s: ok=%v err=%v", dest, ok, err)
			}
			if _, err := os.Stat(newer); !os.IsNotExist(err) {
				t.Errorf("the restored holder should be cleared, got %v", err)
			}
		})
	}
}

// The destination's own probe is what decides whether every copy beside it is
// superseded or still the last one of something. A probe that could not run
// answers neither, so the destination stops rather than being taken apart on the
// strength of a fault.
func TestRestoreInterruptedPromotionStopsWhenTheDestinationProbeCannotRun(t *testing.T) {
	root := t.TempDir()
	dest := filepath.Join(root, "engine-1.2.3-linux-x64")
	if err := os.MkdirAll(filepath.Join(dest, "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dest, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	destBin, _ := enginePaths(dest, false)
	if err := os.WriteFile(destBin, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	holder := plantEngineHolder(t, dest, 100)
	published := usability(func(dir string) (bool, error) {
		bin, _ := resolveEnginePaths(dir, false)
		return fileExistsErr(bin)
	})

	injected := errors.New("injected destination probe failure")
	faulted := true
	injectFault(t, "stat", func(args ...string) bool { return faulted && args[0] == destBin }, injected)
	txn := lockFor(t, dest)
	before := snapshotTree(t, root)
	var reported []string

	restoreInterruptedPromotion(txn, dest, published, reporterFor(&reported))

	if got := snapshotTree(t, root); !slices.Equal(got, before) {
		t.Errorf("nothing may move while the destination cannot be probed:\nbefore %v\nafter  %v", before, got)
	}
	assertReports(t, reported, injected.Error())

	// Same disk, fault removed: the destination is the published install, so
	// the copy beside it is kept rather than restored over it.
	faulted = false
	restoreInterruptedPromotion(txn, dest, published, nil)

	if ok, err := published(dest); !ok || err != nil {
		t.Errorf("the published destination must stay live: ok=%v err=%v", ok, err)
	}
	if _, err := os.Stat(filepath.Join(keptName(t, dest, holder), "install")); err != nil {
		t.Errorf("the copy beside it should be kept under the Kept prefix: %v", err)
	}
}

// Waiting out another process is only harmless if the destination is usable
// afterwards. A recheck that could not run is no evidence of that, so the wait
// and the probe are both reported instead of the caller being told the install
// it never made is done.
func TestWithDestinationLockDoesNotReportSuccessOnAnUnreadableDestination(t *testing.T) {
	shortenInstallLockWait(t, 100*time.Millisecond)
	root := t.TempDir()
	dest := filepath.Join(root, "engine-1.2.3-linux-x64")
	lockFor(t, dest)
	injected := errors.New("injected recheck failure")
	ran := false

	err := withDestinationLock(context.Background(), root, filepath.Base(dest),
		func(dir string) (bool, error) { return false, injected },
		func(*destTxn) error {
			ran = true
			return nil
		})

	if !errors.Is(err, errInstallInProgress) || !errors.Is(err, injected) {
		t.Errorf("withDestinationLock = %v, want both the wait and the failed recheck", err)
	}
	if ran {
		t.Error("the callback must not run without the lock")
	}
}

// ---- the engine identity ----------------------------------------------------

// A tag that is already one safe path component has to encode to itself, byte
// for byte. Every engine on disk today was named that way, and an encoding that
// renamed even one of them would strand a cache the user already paid to
// download.
func TestEngineDestNameKeepsASafeTagAsItIs(t *testing.T) {
	for _, tag := range []string{
		DefaultSherpaVersion, "latest", "v1.13.3", "v1.13.3-rc.1", "1.2.3", "asr-models", "a_b.c-d",
	} {
		want := "engine-" + tag + "-linux-amd64"
		if got := engineDestName(tag, "linux-amd64"); got != want {
			t.Errorf("engineDestName(%q) = %q, want %q", tag, got, want)
		}
	}
}

// A tag that is not a safe component still has to name one destination, and two
// different tags must never name the same one: the name is what the Install
// lock, the holder markers, and the ownership checks all key off, so a collision
// would have two releases sharing one install and one lock.
func TestEngineDestNameEncodesAnUnsafeTagWithoutColliding(t *testing.T) {
	tags := []string{"release/v1", "release\\v1", "release:v1", "release v1", "release_v1", "../v1", "release/v2"}
	seen := map[string]string{}
	for _, tag := range tags {
		name := engineDestName(tag, "linux-amd64")
		if !isInstallDestName(name) {
			t.Errorf("engineDestName(%q) = %q, which is not an install destination name", tag, name)
		}
		if other, ok := seen[name]; ok {
			t.Errorf("engineDestName(%q) and engineDestName(%q) both = %q", other, tag, name)
		}
		seen[name] = tag
	}
}

// plantEngineTree writes the engine layout a released tarball extracts to, so a
// directory planted by a test is one the publication probe accepts.
func plantEngineTree(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	bin, server := enginePaths(dir, false)
	for _, path := range []string{bin, server} {
		if err := os.WriteFile(path, []byte("x"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

// plantDefaultModel writes the model the default options resolve to, so a test
// about the engine never reaches the model download.
func plantDefaultModel(t *testing.T, root string) {
	t.Helper()
	modelDir := filepath.Join(root, "model-moonshine-tiny-en-int8")
	if err := os.MkdirAll(modelDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(modelDir, "tokens.txt"), []byte("t"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Released versions joined the raw tag to the root, so a tag with a separator in
// it installed into a nested path. That cache is a real engine and the only copy
// on that disk: the encoded name must not download a second one beside it or
// leave it orphaned.
func TestEnsureLocalEngineReusesAnEngineInstalledUnderARawTagPath(t *testing.T) {
	root := t.TempDir()
	legacy := filepath.Join(root, "engine-release/v1-linux-amd64")
	plantEngineTree(t, legacy)
	plantDefaultModel(t, root)
	var requests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, "an engine that is already on disk must not be downloaded again", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	comp, err := EnsureLocalEngine(context.Background(), DownloadOptions{
		DestRoot: root, EngineVersion: "release/v1", APIBase: srv.URL, platformKey: "linux-amd64", skipPinned: true,
	})
	if err != nil {
		t.Fatalf("EnsureLocalEngine = %v, want the engine already on disk", err)
	}
	if n := requests.Load(); n != 0 {
		t.Errorf("the release API was contacted %d times for an engine that is already extracted", n)
	}
	wantBin, _ := enginePaths(legacy, false)
	if comp.BinaryPath != wantBin {
		t.Errorf("BinaryPath = %q, want the engine already on disk at %q", comp.BinaryPath, wantBin)
	}
	if _, err := os.Stat(filepath.Join(root, engineDestName("release/v1", "linux-amd64"))); !os.IsNotExist(err) {
		t.Errorf("nothing should have been installed under the encoded name, got %v", err)
	}
}

// The tag is documented as any release tag, and one with a separator in it was
// installable before the destination name became a lock key. It has to stay
// installable: the encoding is what keeps the name one component, not a refusal.
func TestEnsureLocalEngineInstallsATagThatIsNotAPathComponent(t *testing.T) {
	srv := fakeReleaseServer(t, engineSHA, modelSHA)
	root := t.TempDir()
	comp, err := EnsureLocalEngine(context.Background(), DownloadOptions{
		DestRoot: root, EngineVersion: "release/v1", APIBase: srv.URL, platformKey: "linux-amd64", skipPinned: true,
	})
	if err != nil {
		t.Fatalf("EnsureLocalEngine = %v, want a tag with a separator to install", err)
	}
	if !fileExists(comp.BinaryPath) {
		t.Errorf("the engine binary is missing at %q", comp.BinaryPath)
	}
	want := filepath.Join(root, engineDestName("release/v1", "linux-amd64"))
	if !strings.HasPrefix(comp.BinaryPath, want+string(os.PathSeparator)) {
		t.Errorf("BinaryPath = %q, want it under the encoded destination %q", comp.BinaryPath, want)
	}
}

// ---- the compatibility lookup for the pre-encoding layout --------------------

// The compat lookup joins the RAW tag, and filepath.Join cleans, so a tag
// carrying ".." resolves to a sibling of DestRoot entirely outside the install
// root. Whatever sits there would be adopted as the engine with no lock and no
// digest behind it, and its bin/ is what the dictation runner then executes.
func TestEnsureLocalEngineRefusesACompatPathOutsideTheRoot(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "stt")
	outside := filepath.Join(parent, "outside-linux-amd64")
	plantEngineTree(t, outside)
	plantDefaultModel(t, root)

	comp, err := EnsureLocalEngine(context.Background(), DownloadOptions{
		DestRoot: root, EngineVersion: "../../../outside", APIBase: offlineAPIBase(t),
		platformKey: "linux-amd64", skipPinned: true,
	})
	if err == nil {
		t.Fatalf("a tree outside %s must not be adopted, got BinaryPath %q", root, comp.BinaryPath)
	}
	if strings.HasPrefix(comp.BinaryPath, outside) {
		t.Errorf("BinaryPath = %q, which is outside the install root %s", comp.BinaryPath, root)
	}
}

// os.Stat follows a link, so a symlink sitting at the compat path adopts
// whatever it points at just as the ".." tag does. The path stays inside
// DestRoot, so containment alone does not cover it: what is found there has to
// be a real directory.
func TestEnsureLocalEngineRefusesACompatPathThatIsASymlink(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "stt")
	outside := filepath.Join(parent, "elsewhere")
	plantEngineTree(t, outside)
	plantDefaultModel(t, root)
	// The pre-encoding layout for "rel/v1": DestRoot/engine-rel/v1-linux-amd64.
	if err := os.MkdirAll(filepath.Join(root, "engine-rel"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "engine-rel", "v1-linux-amd64")); err != nil {
		t.Skipf("os.Symlink unsupported on this platform: %v", err)
	}

	comp, err := EnsureLocalEngine(context.Background(), DownloadOptions{
		DestRoot: root, EngineVersion: "rel/v1", APIBase: offlineAPIBase(t),
		platformKey: "linux-amd64", skipPinned: true,
	})
	if err == nil {
		t.Fatalf("a symlinked compat path must not be adopted, got BinaryPath %q", comp.BinaryPath)
	}
	if strings.HasPrefix(comp.BinaryPath, outside) {
		t.Errorf("BinaryPath = %q, which is the link's target outside %s", comp.BinaryPath, root)
	}
}

// The picker's badge probes the legacy raw-tag layout too, and the tag is a
// config value: joined unvetted, "../../../escape" resolves the probe OUTSIDE the
// install root, where a matching binary would mark the engine downloaded.
// compatEngineDir is the same containment the install path applies.
func TestEngineDownloadedRefusesARawTagPathOutsideTheRoot(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "stt")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	// "engine-../../../escape-<key>" cleans to <parent>/escape-<key> — the
	// "engine-.." element absorbs the first "..", so escaping takes three: a
	// tree planted there is exactly what an unvetted join would find and
	// believe.
	outside := filepath.Join(parent, "escape-"+platformKey())
	plantEngineTree(t, outside)
	// A legitimately nested legacy path must still be found — containment is
	// the gate, not "the tag had a separator".
	nested := filepath.Join(root, "engine-rel", "v1-"+platformKey())
	plantEngineTree(t, nested)
	// plantEngineTree writes the unix binary names; the badge resolves the
	// host's, so add the names the probe looks for on this platform.
	for _, dir := range []string{outside, nested} {
		bin, server := enginePaths(dir, runtime.GOOS == "windows")
		for _, path := range []string{bin, server} {
			if err := os.WriteFile(path, []byte("x"), 0o755); err != nil {
				t.Fatal(err)
			}
		}
	}

	if EngineDownloaded(root, "../../../escape") {
		t.Errorf("a raw tag that resolves outside %s must not mark the engine downloaded", root)
	}
	if !EngineDownloaded(root, "rel/v1") {
		t.Errorf("a raw tag that resolves inside %s must still mark the engine downloaded", root)
	}
}

// The compat lookup is an optimization, not a gate: nothing is installed when it
// hits. A probe that cannot run there says nothing about the encoded
// destination, so failing the whole call on it turns a stray at the old path
// into an install failure the user can only clear by editing config.
func TestEnsureLocalEngineInstallsWhenTheCompatProbeCannotRun(t *testing.T) {
	for _, tc := range []struct {
		name string
		tag  string
		seed func(t *testing.T, root string)
	}{
		{
			// A NUL cannot be in a path at all, so every stat of the raw path
			// fails with EINVAL, permanently.
			name: "the tag cannot name a path",
			tag:  "v1\x00bad",
		},
		{
			name: "the compat path cannot be read",
			tag:  "rel/v1",
			seed: func(t *testing.T, root string) {
				if err := os.MkdirAll(filepath.Join(root, "engine-rel"), 0o755); err != nil {
					t.Fatal(err)
				}
				unreadable := filepath.Join(root, "engine-rel", "v1-linux-amd64")
				if err := os.Mkdir(unreadable, 0o000); err != nil {
					t.Fatal(err)
				}
				// Put the bit back so the temp dir can be torn down.
				t.Cleanup(func() { _ = os.Chmod(unreadable, 0o755) })
			},
		},
		{
			name: "a stray file sits at the compat path",
			tag:  "rel/v1",
			seed: func(t *testing.T, root string) {
				if err := os.MkdirAll(filepath.Join(root, "engine-rel"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(root, "engine-rel", "v1-linux-amd64"), []byte("x"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			// The real destination already holds a working engine, so the only
			// thing that can fail this call is the probe of the old layout.
			engineDir := filepath.Join(root, engineDestName(tc.tag, "linux-amd64"))
			plantEngineTree(t, engineDir)
			plantDefaultModel(t, root)
			if tc.seed != nil {
				tc.seed(t, root)
			}

			comp, err := EnsureLocalEngine(context.Background(), DownloadOptions{
				DestRoot: root, EngineVersion: tc.tag, APIBase: offlineAPIBase(t),
				platformKey: "linux-amd64", skipPinned: true,
			})
			if err != nil {
				t.Fatalf("EnsureLocalEngine = %v, want the engine already at %s", err, engineDir)
			}
			wantBin, _ := enginePaths(engineDir, false)
			if comp.BinaryPath != wantBin {
				t.Errorf("BinaryPath = %q, want the engine at the encoded destination %q", comp.BinaryPath, wantBin)
			}
		})
	}
}

// The engine probe resolves paths before it answers, and a probe that could
// not run is not "not installed": a stat that fails durably on something other
// than "not there", or the flatten listing that cannot list the tree at all,
// is the probe saying nothing — and answering anyway is what licenses
// downloading over a tree nobody could read and reaping it. The faults go
// through the seam rather than a self-referential link or an unreadable
// directory, because the platforms this runs on disagree about whether either
// fixture produces the error the contract needs — a symlink loop stats as
// absent on Windows, not as a failure.
func TestEnsureLocalEngineStopsWhenTheInstalledEngineCannotBeProbed(t *testing.T) {
	for _, tc := range []struct {
		name string
		seed func(t *testing.T, engineDir string)
	}{
		{
			// The binary's own stat fails on something that is not absence.
			name: "the binary cannot be probed",
			seed: func(t *testing.T, engineDir string) {
				bin, _ := enginePaths(engineDir, false)
				injectFault(t, "stat", func(args ...string) bool {
					return args[0] == bin
				}, errors.New("injected probe failure"))
			},
		},
		{
			// The binary absent is a real answer; the flatten listing that then
			// fails is not — it is what the un-flattened paths were silently
			// standing in for.
			name: "the tree cannot be listed",
			seed: func(t *testing.T, engineDir string) {
				injectFault(t, "readDir", func(args ...string) bool {
					return args[0] == engineDir
				}, errors.New("injected listing failure"))
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			engineDir := filepath.Join(root, "engine-test-linux-amd64")
			lib := filepath.Join(engineDir, "lib")
			if err := os.MkdirAll(lib, 0o755); err != nil {
				t.Fatal(err)
			}
			kept := filepath.Join(lib, "libsherpa.so")
			if err := os.WriteFile(kept, []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
			tc.seed(t, engineDir)
			plantDefaultModel(t, root)
			// A server that WOULD serve a working engine, so nothing but the
			// refusal itself can stop the download.
			srv := fakeReleaseServer(t, engineSHA, modelSHA)

			if _, err := EnsureLocalEngine(context.Background(), DownloadOptions{
				DestRoot: root, EngineVersion: "test", APIBase: srv.URL,
				platformKey: "linux-amd64", skipPinned: true,
			}); err == nil {
				t.Fatal("a probe that could not run must not be answered with an install over the tree it could not read")
			}
			if _, err := os.Stat(kept); err != nil {
				t.Errorf("the existing tree must survive a probe that could not run: %v", err)
			}
		})
	}
}

// A holder marker this pass could not read leaves recovery with no ruling on
// the copy beside the destination. Reporting it and returning nil hands the
// caller a clean run, and the caller then downloads over the very transaction
// whose state nobody established.
func TestRestoreInterruptedPromotionStopsTheCallerOnAnUnreadableMarker(t *testing.T) {
	root := t.TempDir()
	destDir := filepath.Join(root, "engine-a")
	holder := plantHolder(t, destDir, 1, "v1", false)
	txn := lockFor(t, destDir)

	injectFault(t, "readFile", func(args ...string) bool {
		return strings.HasPrefix(args[0], holder)
	}, errors.New("injected marker read failure"))

	var reported []string
	if err := restoreInterruptedPromotion(txn, destDir, testPublished, reporterFor(&reported)); err == nil {
		t.Fatal("a marker that could not be read must stop the caller, not report a clean run")
	}
	if _, err := os.Stat(filepath.Join(holder, "install", "engine")); err != nil {
		t.Errorf("the copy must be left intact: %v", err)
	}
}

// A destination name the filesystem accepts can still have a lock file it
// refuses: the name is under NAME_MAX and "<name>.lock" is not. The failure
// then names lockutil and a path the user never chose, so the tag has to be
// bounded where it is set instead.
func TestEnsureLocalEngineRefusesATagWhoseLockFileWouldNotFit(t *testing.T) {
	t.Run("at the bound", func(t *testing.T) {
		root := t.TempDir()
		tag := strings.Repeat("a", maxInstallTagBytes)
		plantEngineTree(t, filepath.Join(root, engineDestName(tag, "linux-amd64")))
		plantDefaultModel(t, root)

		if _, err := EnsureLocalEngine(context.Background(), DownloadOptions{
			DestRoot: root, EngineVersion: tag, APIBase: offlineAPIBase(t),
			platformKey: "linux-amd64", skipPinned: true,
		}); err != nil {
			t.Fatalf("a tag at the bound must install: %v", err)
		}
	})
	t.Run("one byte over", func(t *testing.T) {
		root := t.TempDir()
		tag := strings.Repeat("a", maxInstallTagBytes+1)
		plantDefaultModel(t, root)

		_, err := EnsureLocalEngine(context.Background(), DownloadOptions{
			DestRoot: root, EngineVersion: tag, APIBase: offlineAPIBase(t),
			platformKey: "linux-amd64", skipPinned: true,
		})
		if err == nil {
			t.Fatal("a tag whose lock file cannot be created must be refused")
		}
		if strings.Contains(err.Error(), "lockutil") || strings.Contains(err.Error(), "file name too long") {
			t.Errorf("err = %v, want the tag named rather than the lock file it happened to break", err)
		}
		if !strings.Contains(err.Error(), fmt.Sprint(len(tag))) {
			t.Errorf("err = %v, want the tag length named", err)
		}
	})
}

// ---- the install lock's release ---------------------------------------------

// failInstallRelease makes the Install lock fail its release while the lock
// itself is really taken and really let go. Release is the last step of every
// operation that holds it, so it is the one cleanup failure that can follow a
// mutation that already landed.
func failInstallRelease(t *testing.T, err error) {
	t.Helper()
	real := acquireInstallLock
	t.Cleanup(func() { acquireInstallLock = real })
	acquireInstallLock = func(root, path string) (installFileLock, error) {
		lock, aerr := real(root, path)
		if aerr != nil {
			return nil, aerr
		}
		return failingRelease{lock: lock, err: err}, nil
	}
}

// failingRelease still releases the real lock, so the operations after the one
// under test are not left waiting on a lock this test never gives back.
type failingRelease struct {
	lock installFileLock
	err  error
}

func (f failingRelease) Release() error {
	_ = f.lock.Release()
	return f.err
}

// Release joins the platform unlock and the close, so a failure can mean the
// lock file is still held. An install that swallowed it would report a clean
// run over a lock nobody let go of, and one reported as a plain failure would
// send the caller back to download an engine that is already installed.
func TestEnsureLocalEngineReportsAnInstallItCouldNotUnlock(t *testing.T) {
	root := t.TempDir()
	engineDir := filepath.Join(root, "engine-test-linux-amd64")
	plantEngineTree(t, engineDir)
	plantDefaultModel(t, root)
	failInstallRelease(t, errors.New("injected release failure"))

	_, err := EnsureLocalEngine(context.Background(), DownloadOptions{
		DestRoot: root, EngineVersion: "test", APIBase: offlineAPIBase(t), platformKey: "linux-amd64", skipPinned: true,
	})
	if err == nil {
		t.Fatal("an install that could not release its lock must not report a clean run")
	}
	if !errors.Is(err, errInstalledNotReleased) {
		t.Fatalf("err = %v, want it to say the destination is installed", err)
	}
	if !strings.Contains(err.Error(), "injected release failure") {
		t.Errorf("err = %v, want the release failure named", err)
	}
	bin, _ := enginePaths(engineDir, false)
	if !fileExists(bin) {
		t.Errorf("the engine itself is installed, so %q must still be there", bin)
	}
}

// The primary cause is what the operation failed on; the release failure is a
// second fact about the lock. Folding the first into the second would leave the
// caller with a lock error and no reason for the install itself.
func TestWithDestinationLockKeepsThePrimaryCauseWhenTheReleaseAlsoFails(t *testing.T) {
	root := t.TempDir()
	failInstallRelease(t, errors.New("injected release failure"))
	injected := errors.New("injected install failure")

	err := withDestinationLock(context.Background(), root, "engine-a", testPublished, func(*destTxn) error {
		return injected
	})
	if !errors.Is(err, injected) {
		t.Fatalf("err = %v, want the install failure kept as the cause", err)
	}
	if !strings.Contains(err.Error(), "injected release failure") {
		t.Errorf("err = %v, want the release failure carried too", err)
	}
	if errors.Is(err, errInstalledNotReleased) {
		t.Errorf("err = %v, but nothing was installed", err)
	}
}

// The removal is the mutation here, and it has already happened by the time the
// lock is released. A bare release error reads as "nothing was removed", which
// sends an operator back to a command whose work is done.
func TestRemoveKeptBackupReportsARemovalItCouldNotUnlock(t *testing.T) {
	t.Run("the removal landed", func(t *testing.T) {
		root := t.TempDir()
		destDir := filepath.Join(root, "engine-a")
		kept := plantKeptHolder(t, destDir, 3, "engine copy")
		failInstallRelease(t, errors.New("injected release failure"))

		err := RemoveKeptBackup(root, filepath.Base(kept))
		if err == nil {
			t.Fatal("a removal that could not release its lock must not report a clean run")
		}
		if !errors.Is(err, errInstalledNotReleased) {
			t.Fatalf("err = %v, want it to say the removal landed", err)
		}
		if _, serr := os.Stat(kept); !os.IsNotExist(serr) {
			t.Errorf("the removal itself landed, so the backup should be gone: %v", serr)
		}
	})

	t.Run("the removal failed", func(t *testing.T) {
		root := t.TempDir()
		destDir := filepath.Join(root, "engine-a")
		kept := plantKeptHolder(t, destDir, 3, "engine copy")
		injectFault(t, "removeAll", func(args ...string) bool { return args[0] == kept }, errors.New("injected remove failure"))
		failInstallRelease(t, errors.New("injected release failure"))

		err := RemoveKeptBackup(root, filepath.Base(kept))
		if err == nil {
			t.Fatal("a removal that failed must be reported")
		}
		if !strings.Contains(err.Error(), "injected remove failure") {
			t.Errorf("err = %v, want the removal failure kept as the cause", err)
		}
		if !strings.Contains(err.Error(), "injected release failure") {
			t.Errorf("err = %v, want the release failure carried too", err)
		}
		if errors.Is(err, errInstalledNotReleased) {
			t.Errorf("err = %v, but nothing was removed", err)
		}
	})
}
