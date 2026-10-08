package update

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// npmPackageName is the published package name for the npm distribution of
// zero (see package.json). scripts/postinstall.mjs downloads the native
// binary into the same directory as package.json and leaves a
// ".zero-binary-version" marker file next to it — both are reliable signals
// that a given executable came from an npm install.
const npmPackageName = "@gitlawb/zero"

// InstallMethod identifies how the running zero binary was installed.
type InstallMethod string

const (
	InstallMethodNpm        InstallMethod = "npm"
	InstallMethodHomebrew   InstallMethod = "homebrew"
	InstallMethodStandalone InstallMethod = "standalone"
)

// homebrewCellar is the directory every Homebrew keg lives under:
// <prefix>/Cellar/<formula>/<version>/bin/<binary>.
//
// Matching the Cellar segment rather than the Homebrew PREFIX is deliberate.
// The prefix on Intel macOS is /usr/local, which is also where people put
// hand-installed binaries, so treating the prefix as the signal would classify
// an ordinary /usr/local/bin/zero as Homebrew-managed and refuse an update that
// works fine. Every keg is under Cellar and nothing else is, so this errs
// toward leaving self-update enabled, which is the recoverable direction.
//
// HOMEBREW_CELLAR needs no separate check: it defaults to <prefix>/Cellar and
// Homebrew does not support renaming it.
const homebrewCellar = "Cellar"

// DetectInstallMethod reports how the binary at executablePath was installed.
//
// Symlinks are resolved here rather than at the call sites. Homebrew links
// <prefix>/bin/zero to the keg, and Check did not resolve while Apply did, so
// the two disagreed about what a Homebrew install was: the check printed
// standalone guidance for an install the apply path would have handled
// differently.
func DetectInstallMethod(executablePath string) InstallMethod {
	if resolved, err := filepath.EvalSymlinks(executablePath); err == nil {
		executablePath = resolved
	}
	if isHomebrewPath(runtime.GOOS, executablePath) {
		return InstallMethodHomebrew
	}
	dir := filepath.Dir(executablePath)
	if _, err := os.Stat(filepath.Join(dir, ".zero-binary-version")); err == nil {
		return InstallMethodNpm
	}
	data, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return InstallMethodStandalone
	}
	var pkg struct {
		Name string          `json:"name"`
		OS   []string        `json:"os"`
		CPU  []string        `json:"cpu"`
		Bin  json.RawMessage `json:"bin"`
	}
	if err := json.Unmarshal(data, &pkg); err != nil {
		return InstallMethodStandalone
	}
	// The native platform package is inert and constrained to one OS/CPU. The
	// repository's wrapper package has the same name but carries a bin entry and
	// broad platform lists, so name alone would misclassify `go build -o zero`
	// from the repository root as an npm-managed install.
	if pkg.Name == npmPackageName && len(pkg.OS) == 1 && len(pkg.CPU) == 1 && len(pkg.Bin) == 0 {
		return InstallMethodNpm
	}
	return InstallMethodStandalone
}

// isHomebrewPath reports whether executablePath is inside a Homebrew keg.
//
// goos is a parameter rather than runtime.GOOS so the decision can be tested on
// every target from any machine. Gating it on the real GOOS made the one test
// that matters skip on Windows, which is how a platform rule ends up unverified
// on the platform it excludes.
//
// TWO CHECKS, CHEAP ONE FIRST. The path shape is a filter, not the answer: a
// user's own /work/Cellar/tools/bin/zero has exactly the shape a keg does, and
// classifying it as Homebrew would refuse an update that works. The keg is only
// confirmed by Homebrew's own receipt, which it writes into every keg and
// nothing else does.
//
// The receipt costs one Stat, and only on a path that already looks like a keg —
// so the version check that runs for ordinary installs still does no filesystem
// work here at all.
func isHomebrewPath(goos string, executablePath string) bool {
	keg, ok := homebrewKeg(goos, executablePath)
	if !ok {
		return false
	}
	return hasHomebrewReceipt(keg)
}

// homebrewKeg returns the <prefix>/Cellar/<formula>/<version> directory that
// executablePath sits inside, if its shape is a keg's.
//
// Deliberately not matched: the formula name, because a tap may name it
// something other than zero.
func homebrewKeg(goos string, executablePath string) (string, bool) {
	if goos == "windows" {
		// Homebrew does not run here, and a Windows path is far likelier to hold
		// an unrelated directory called Cellar than a keg.
		return "", false
	}
	// <prefix>/Cellar/<formula>/<version>/bin/<binary> — the two segments
	// Homebrew always inserts, then the bin directory and the binary itself.
	// Anything shorter cannot be a keg however it is named.
	segments := strings.Split(filepath.ToSlash(executablePath), "/")
	for index, segment := range segments {
		if segment != homebrewCellar {
			continue
		}
		if len(segments)-index < 5 {
			continue
		}
		return strings.Join(segments[:index+3], "/"), true
	}
	return "", false
}

// homebrewReceipt is the file Homebrew writes into every keg it installs. Its
// presence is what distinguishes a keg from a directory tree that merely looks
// like one.
const homebrewReceipt = "INSTALL_RECEIPT.json"

func hasHomebrewReceipt(kegDir string) bool {
	info, err := os.Stat(filepath.Join(kegDir, homebrewReceipt))
	return err == nil && !info.IsDir()
}
