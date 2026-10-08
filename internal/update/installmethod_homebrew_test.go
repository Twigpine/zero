package update

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The path rule itself, checked for every target from any machine. Gating this
// on runtime.GOOS would skip it on Windows, leaving the Windows branch of the
// rule to be verified by nobody.
//
// The keg SHAPE only. Whether a real keg lives there is a separate question with
// a separate answer on disk — see TestOnlyAKegWithAReceiptIsHomebrew, which is
// the check that stops a user's own Cellar directory being classified.
func TestHomebrewKegShapePerTarget(t *testing.T) {
	cases := []struct {
		goos string
		path string
		want bool
	}{
		{"darwin", "/opt/homebrew/Cellar/zero/0.7.1/bin/zero", true},
		{"darwin", "/usr/local/Cellar/zero/0.7.1/bin/zero", true},
		{"linux", "/home/linuxbrew/.linuxbrew/Cellar/zero/0.7.1/bin/zero", true},
		{"darwin", "/usr/local/bin/zero", false},
		{"darwin", "/Users/someone/.local/bin/zero", false},
		{"linux", "/opt/cellar/bin/zero", false},          // lowercase is not a keg
		{"linux", "/opt/CellarX/bin/zero", false},         // segment must match exactly
		{"windows", `C:\Cellar\zero\bin\zero.exe`, false}, // Homebrew does not run here
		// User directories called Cellar. Too shallow to be a keg.
		{"linux", "/home/someone/Cellar/zero", false},
		{"darwin", "/Users/someone/Cellar/bin/zero", false},
		// The case the reviewer of #910 raised. It is one segment short of a keg,
		// so the depth rule alone already refuses it.
		{"linux", "/work/Cellar/tools/bin/zero", false},
		// One segment deeper it IS a keg by shape, and still not a keg. Nothing in
		// the path can tell the difference — only the receipt can, which is what
		// TestOnlyAKegWithAReceiptIsHomebrew covers.
		{"linux", "/work/Cellar/tools/1.0/bin/zero", true},
	}
	for _, testCase := range cases {
		if _, got := homebrewKeg(testCase.goos, testCase.path); got != testCase.want {
			t.Errorf("homebrewKeg(%q, %q) = %v, want %v", testCase.goos, testCase.path, got, testCase.want)
		}
	}
}

// THE REGRESSION THE REVIEWER ASKED FOR, and the reason the receipt check
// exists: a directory tree a user built themselves can have exactly a keg's
// shape. Classifying it as Homebrew refuses a `zero upgrade` that would have
// worked perfectly, on a machine Homebrew has never touched.
//
// Homebrew writes INSTALL_RECEIPT.json into every keg it installs and nothing
// else does, so its presence is what separates the two.
func TestOnlyAKegWithAReceiptIsHomebrew(t *testing.T) {
	// An explicit target rather than runtime.GOOS, so the receipt rule is checked
	// on every platform including the one it is switched off for. isHomebrewPath
	// already takes the target, so the skip bought nothing and cost the ability to
	// catch a break anywhere but CI. Two tests broke exactly that way when the
	// receipt requirement landed: they build a keg with no receipt, and only
	// macOS and Linux ever ran them.
	const target = "darwin"
	root := t.TempDir()

	// Same shape, same depth. The only difference is the receipt.
	real := filepath.Join(root, "prefix", "Cellar", "zero", "0.7.1")
	fake := filepath.Join(root, "work", "Cellar", "tools", "0.7.1")
	for _, keg := range []string{real, fake} {
		if err := os.MkdirAll(filepath.Join(keg, "bin"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(keg, "bin", "zero"), []byte("binary"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(real, "INSTALL_RECEIPT.json"), []byte(`{"source":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	if !isHomebrewPath(target, filepath.Join(real, "bin", "zero")) {
		t.Error("a keg with a receipt was not recognised as Homebrew")
	}
	if isHomebrewPath(target, filepath.Join(fake, "bin", "zero")) {
		t.Error("a user's own Cellar-shaped directory was classified as Homebrew; `zero upgrade` would refuse for nothing")
	}
}

// A receipt that is a DIRECTORY is not a receipt. Cheap to get wrong with a
// bare Stat, and the failure would be silent.
func TestADirectoryNamedLikeTheReceiptIsNotOne(t *testing.T) {
	// Explicit target, same reason as above: the rule is worth checking wherever
	// the tests run, not only where the feature is switched on.
	const target = "darwin"
	keg := filepath.Join(t.TempDir(), "prefix", "Cellar", "zero", "0.7.1")
	if err := os.MkdirAll(filepath.Join(keg, "INSTALL_RECEIPT.json"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(keg, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if isHomebrewPath(target, filepath.Join(keg, "bin", "zero")) {
		t.Error("a directory named INSTALL_RECEIPT.json was accepted as a receipt")
	}
}

// A Homebrew install is a keg under <prefix>/Cellar with <prefix>/bin/zero
// linked to it. Detection has to survive both spellings, because Check passes
// the unresolved path and Apply passes the resolved one.
func TestDetectInstallMethodRecognisesAHomebrewKeg(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("DetectInstallMethod is GOOS-gated; the rule itself is covered by TestIsHomebrewPathPerTarget")
	}
	prefix := t.TempDir()
	keg := filepath.Join(prefix, "Cellar", "zero", "0.7.1", "bin")
	if err := os.MkdirAll(keg, 0o755); err != nil {
		t.Fatal(err)
	}
	// The receipt is what separates a keg from any directory that happens to sit
	// under a path containing "Cellar". Without it detection correctly answers
	// standalone, which is what this test would otherwise be asserting against.
	if err := os.WriteFile(filepath.Join(filepath.Dir(keg), homebrewReceipt), []byte(`{"source":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(keg, "zero")
	if err := os.WriteFile(binary, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := DetectInstallMethod(binary); got != InstallMethodHomebrew {
		t.Errorf("keg path: got %q, want %q", got, InstallMethodHomebrew)
	}

	linkDir := filepath.Join(prefix, "bin")
	if err := os.MkdirAll(linkDir, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(linkDir, "zero")
	if err := os.Symlink(binary, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	// The path Check sees. Without symlink resolution this reads as standalone
	// and the guidance tells a Homebrew user to run `zero upgrade`.
	if got := DetectInstallMethod(link); got != InstallMethodHomebrew {
		t.Errorf("linked path: got %q, want %q", got, InstallMethodHomebrew)
	}
}

// The failure that matters more than a missed detection: refusing to update an
// install Homebrew has never heard of. /usr/local is a Homebrew prefix on Intel
// macOS and an ordinary install location everywhere.
func TestDetectInstallMethodLeavesOrdinaryInstallsAlone(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{
		filepath.Join(root, "usr", "local", "bin"),
		filepath.Join(root, "home", "user", ".local", "bin"),
		filepath.Join(root, "opt", "homebrewish", "bin"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		binary := filepath.Join(dir, "zero")
		if err := os.WriteFile(binary, []byte("binary"), 0o755); err != nil {
			t.Fatal(err)
		}
		if got := DetectInstallMethod(binary); got != InstallMethodStandalone {
			t.Errorf("%s: got %q, want %q", dir, got, InstallMethodStandalone)
		}
	}
}

// An npm install must not start reading as Homebrew now that a second check
// runs first.
func TestDetectInstallMethodStillRecognisesNpm(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "zero")
	if err := os.WriteFile(binary, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".zero-binary-version"), []byte("0.7.1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := DetectInstallMethod(binary); got != InstallMethodNpm {
		t.Errorf("got %q, want %q", got, InstallMethodNpm)
	}
}

func TestUpgradeGuidanceSendsHomebrewUsersToBrew(t *testing.T) {
	guidance := upgradeGuidance(AssetCheck{}, "", InstallMethodHomebrew)
	if !strings.Contains(guidance, "brew upgrade zero") {
		t.Errorf("guidance does not name the command that works: %q", guidance)
	}
	if strings.Contains(guidance, "Run `zero upgrade`") {
		t.Errorf("guidance still offers the command that refuses: %q", guidance)
	}
}

// A custom source flag must not talk a Homebrew user back into `zero upgrade`.
func TestUpgradeGuidanceIgnoresSourceFlagForHomebrew(t *testing.T) {
	guidance := upgradeGuidance(AssetCheck{}, "--source", InstallMethodHomebrew)
	if !strings.Contains(guidance, "brew upgrade zero") {
		t.Errorf("source flag changed the Homebrew answer: %q", guidance)
	}
}

// A CROSS-TARGET check outranks the install method, on purpose.
//
// Homebrew is a property of the binary on THIS machine. When the check was asked
// about a different target, the answer is about that other machine, and
// `brew upgrade zero` would change this one instead. Pinned as a test because it
// reads like an ordering bug until you see which question is being answered.
func TestUpgradeGuidanceKeepsCrossTargetAnswerForHomebrew(t *testing.T) {
	local := localReleaseTarget()
	other := "linux-arm64"
	if local == other {
		other = "macos-x64"
	}
	target, err := ResolveTarget(other)
	if err != nil {
		t.Fatalf("resolve target: %v", err)
	}
	asset := AssetCheck{Platform: target.Platform, Arch: target.Arch}

	guidance := upgradeGuidance(asset, "", InstallMethodHomebrew)
	if strings.Contains(guidance, "brew upgrade zero") {
		t.Errorf("a question about %s was answered with a command that changes this machine: %q", other, guidance)
	}
	if !strings.Contains(guidance, other) {
		t.Errorf("cross-target guidance does not name the target asked about: %q", guidance)
	}
}

// localReleaseEndpoint builds a data: release whose assets match THIS platform.
//
// Apply runs Check first, and Check fails outright when the release carries no
// archive for the running target. An empty asset list therefore made the
// Homebrew test pass on the error it was looking for and fail on the message,
// which is a fixture that proves nothing about the branch it was written for.
func localReleaseEndpoint(t *testing.T, version string) string {
	t.Helper()
	target := localReleaseTarget()
	if target == "" {
		t.Skip("no published release target for this platform")
	}
	extension := ".tar.gz"
	if strings.HasPrefix(target, "windows") {
		extension = ".zip"
	}
	archive := "zero-v" + version + "-" + target + extension
	payload := url.QueryEscape(`{"tag_name":"v` + version + `","html_url":"https://example.test/release","assets":[` +
		`{"name":"` + archive + `","browser_download_url":"https://example.test/` + archive + `"},` +
		`{"name":"` + archive + `.sha256","browser_download_url":"https://example.test/` + archive + `.sha256"}]}`)
	return "data:application/json," + payload
}

// Guards the helper above on EVERY platform, including the ones where the
// Homebrew test that uses it has to skip. The fixture broke on macOS while
// passing everywhere it was not exercised, so the fixture gets its own check.
func TestLocalReleaseEndpointIsAcceptedByCheck(t *testing.T) {
	result, err := Check(context.Background(), Options{
		CurrentVersion: "0.1.0",
		Endpoint:       localReleaseEndpoint(t, "0.7.0"),
	})
	if err != nil {
		t.Fatalf("Check rejected the fixture release for this platform: %v", err)
	}
	if !result.UpdateAvailable {
		t.Errorf("fixture release 0.7.0 should be newer than 0.1.0")
	}
}

// Apply must refuse a Homebrew keg outright: no download, no write, and an error
// that names the command which does work.
func TestApplyRefusesToUpdateAHomebrewKeg(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("DetectInstallMethod is GOOS-gated; see TestIsHomebrewPathPerTarget")
	}
	prefix := t.TempDir()
	keg := filepath.Join(prefix, "Cellar", "zero", "0.7.0", "bin")
	if err := os.MkdirAll(keg, 0o755); err != nil {
		t.Fatal(err)
	}
	// Without the receipt this is not a keg, Apply proceeds to the download, and
	// the assertion below fails on a network error instead of on the refusal.
	// Without the receipt this is not a keg, Apply proceeds to the download, and
	// the assertion below fails on a network error instead of on the refusal.
	if err := os.WriteFile(filepath.Join(filepath.Dir(keg), homebrewReceipt), []byte(`{"source":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(keg, "zero")
	original := []byte("original binary")
	if err := os.WriteFile(binary, original, 0o755); err != nil {
		t.Fatal(err)
	}

	restore := currentExecutable
	currentExecutable = func() (string, error) { return binary, nil }
	t.Cleanup(func() { currentExecutable = restore })

	_, err := Apply(context.Background(), Options{
		CurrentVersion: "0.1.0",
		Endpoint:       localReleaseEndpoint(t, "0.7.0"),
	})
	if err == nil {
		t.Fatal("Apply updated a Homebrew keg instead of refusing")
	}
	if !strings.Contains(err.Error(), "brew upgrade zero") {
		t.Errorf("refusal does not name the command that works: %v", err)
	}
	after, readErr := os.ReadFile(binary)
	if readErr != nil {
		t.Fatalf("read binary: %v", readErr)
	}
	if string(after) != string(original) {
		t.Error("Apply rewrote the keg binary it claimed to refuse")
	}
}
