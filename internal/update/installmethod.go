package update

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// npmPackageName is the published package name for the npm distribution of
// zero, and must equal the name in the repository's package.json
// (TestNpmPackageNameMatchesPackageJSON holds the two together).
// scripts/postinstall.mjs downloads the native binary into the same directory
// as package.json and leaves a ".zero-binary-version" marker file next to it —
// both are reliable signals that a given executable came from an npm install.
const npmPackageName = "@twigpine/zero"

// legacyNpmPackageName is the name zero was published under on npm up to
// 0.9.0. The 0.10.0 release was published once more under it, so that 0.9.0's
// `zero upgrade` and anyone still installing the old name land on a build that
// says where the package went (docs/NPM_PACKAGING.md). Nothing is published
// under it after that, which is why an install of it is told to switch instead
// of offered an update.
const legacyNpmPackageName = "@gitlawb/zero"

// InstallMethod identifies how the running zero binary was installed.
type InstallMethod string

const (
	InstallMethodNpm InstallMethod = "npm"
	// InstallMethodNpmLegacy is an npm install of legacyNpmPackageName.
	InstallMethodNpmLegacy  InstallMethod = "npm-legacy"
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
	pkg, hasManifest := readNpmManifest(dir)
	if _, err := os.Stat(filepath.Join(dir, ".zero-binary-version")); err == nil {
		// The first-run downloader put this binary next to the wrapper's own
		// package.json, which names the package it was installed as.
		if hasManifest && pkg.Name == legacyNpmPackageName {
			return InstallMethodNpmLegacy
		}
		return InstallMethodNpm
	}
	if !hasManifest {
		return InstallMethodStandalone
	}
	// The native platform package is inert and constrained to one OS/CPU. The
	// repository's wrapper package has the same name but carries a bin entry and
	// broad platform lists, so name alone would misclassify `go build -o zero`
	// from the repository root as an npm-managed install.
	if len(pkg.OS) != 1 || len(pkg.CPU) != 1 || len(pkg.Bin) != 0 {
		return InstallMethodStandalone
	}
	switch pkg.Name {
	case npmPackageName:
		return InstallMethodNpm
	case legacyNpmPackageName:
		return InstallMethodNpmLegacy
	}
	return InstallMethodStandalone
}

type npmManifest struct {
	Name string          `json:"name"`
	OS   []string        `json:"os"`
	CPU  []string        `json:"cpu"`
	Bin  json.RawMessage `json:"bin"`
}

// readNpmManifest reads the package.json in dir, reporting false when there is
// none or it does not parse.
func readNpmManifest(dir string) (npmManifest, bool) {
	data, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return npmManifest{}, false
	}
	var pkg npmManifest
	if err := json.Unmarshal(data, &pkg); err != nil {
		return npmManifest{}, false
	}
	return pkg, true
}

// legacyPackageSwitchMessage tells someone running the build published under
// legacyNpmPackageName how to move to npmPackageName. bin/zero.js prints the
// same note.
//
// There is no in-place migration. Both packages own the `zero` bin, so npm
// refuses to install the new one over the old (EEXIST), and forcing it lets a
// later uninstall of the old package delete the shared bin. Uninstalling first
// is the only safe order, and it has to be the user's own step: the running
// binary is inside the package it would remove.
func legacyPackageSwitchMessage() string {
	return "Zero has moved to " + npmPackageName + " on npm, and " + legacyNpmPackageName + " gets no further releases. To keep getting updates, quit zero and run:\n" +
		"  npm uninstall -g " + legacyNpmPackageName + "\n" +
		"  npm install -g " + npmPackageName + "\n" +
		"Uninstall first: both packages provide the `zero` command. With bun, pnpm or yarn, use its global remove and add commands instead."
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
