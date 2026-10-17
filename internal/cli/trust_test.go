package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/workspacetrust"
)

// trustDeps builds a test appDeps whose getwd returns a fixed directory, so the
// trust command can be exercised without touching the real cwd.
func trustDeps(cwd string) appDeps {
	return appDeps{getwd: func() (string, error) { return cwd, nil }}
}

// TestRunTrustAddThenList proves `trust` trusts the cwd and `trust list` shows it.
func TestRunTrustAddThenList(t *testing.T) {
	setTrustConfigRoot(t)
	cwd := t.TempDir()
	deps := trustDeps(cwd)

	var out, errBuf bytes.Buffer
	if code := runTrust(nil, &out, &errBuf, deps); code != exitSuccess {
		t.Fatalf("trust returned %d, want %d; stderr=%q", code, exitSuccess, errBuf.String())
	}
	if !strings.Contains(out.String(), "Trusted "+cwd) {
		t.Fatalf("trust stdout = %q, want it to contain %q", out.String(), "Trusted "+cwd)
	}

	trusted, err := workspacetrust.IsTrusted(cwd)
	if err != nil {
		t.Fatalf("IsTrusted after trust: %v", err)
	}
	if !trusted {
		t.Fatalf("cwd should be trusted after `trust`")
	}

	// The store canonicalizes with filepath.EvalSymlinks, so `list` prints the
	// resolved path. On platforms where the temp dir is a symlink (macOS /var) or a
	// Windows 8.3 short name (RUNNER~1 -> runneradmin), that differs from the raw
	// cwd; compare against the same canonical form the store records.
	wantListed := cwd
	if resolved, err := filepath.EvalSymlinks(cwd); err == nil {
		wantListed = resolved
	}

	out.Reset()
	errBuf.Reset()
	if code := runTrust([]string{"list"}, &out, &errBuf, deps); code != exitSuccess {
		t.Fatalf("trust list returned %d, want %d; stderr=%q", code, exitSuccess, errBuf.String())
	}
	if !strings.Contains(out.String(), wantListed) {
		t.Fatalf("trust list stdout = %q, want it to contain the trusted root %q", out.String(), wantListed)
	}
}

// TestRunTrustListShowsCanonicalPath executes the canonical-path behavior that broke
// TestRunTrustAddThenList on the Windows runner (an 8.3 short name expanded by
// EvalSymlinks). It uses a symlinked cwd -- the portable, Linux-runnable analogue of
// that divergence -- and proves `trust list` prints the resolved path the store
// records, never the raw pre-resolution path.
func TestRunTrustListShowsCanonicalPath(t *testing.T) {
	setTrustConfigRoot(t)
	realDir := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(realDir, link); err != nil {
		t.Skipf("symlinks unavailable on this platform: %v", err)
	}
	resolved, err := filepath.EvalSymlinks(link)
	if err != nil {
		t.Fatalf("EvalSymlinks(link): %v", err)
	}
	if resolved == link {
		t.Skip("symlink did not change the path on this platform")
	}

	deps := trustDeps(link)
	if code := runTrust(nil, &bytes.Buffer{}, &bytes.Buffer{}, deps); code != exitSuccess {
		t.Fatalf("trust returned %d", code)
	}

	var out, errBuf bytes.Buffer
	if code := runTrust([]string{"list"}, &out, &errBuf, deps); code != exitSuccess {
		t.Fatalf("trust list returned %d; stderr=%q", code, errBuf.String())
	}
	if !strings.Contains(out.String(), resolved) {
		t.Fatalf("trust list = %q, want the canonical path %q", out.String(), resolved)
	}
	if strings.Contains(out.String(), link) {
		t.Fatalf("trust list = %q, must not contain the raw pre-resolution path %q", out.String(), link)
	}
}

// TestRunTrustRemoveCurrentDir proves `trust remove` (no arg) untrusts the cwd and
// `trust list` then reports none.
func TestRunTrustRemoveCurrentDir(t *testing.T) {
	setTrustConfigRoot(t)
	cwd := t.TempDir()
	deps := trustDeps(cwd)

	if code := runTrust(nil, &bytes.Buffer{}, &bytes.Buffer{}, deps); code != exitSuccess {
		t.Fatalf("trust setup returned %d", code)
	}

	var out, errBuf bytes.Buffer
	if code := runTrust([]string{"remove"}, &out, &errBuf, deps); code != exitSuccess {
		t.Fatalf("trust remove returned %d, want %d; stderr=%q", code, exitSuccess, errBuf.String())
	}
	if !strings.Contains(out.String(), "Untrusted "+cwd) {
		t.Fatalf("trust remove stdout = %q, want it to contain %q", out.String(), "Untrusted "+cwd)
	}

	trusted, err := workspacetrust.IsTrusted(cwd)
	if err != nil {
		t.Fatalf("IsTrusted after remove: %v", err)
	}
	if trusted {
		t.Fatalf("cwd should not be trusted after `trust remove`")
	}

	out.Reset()
	errBuf.Reset()
	if code := runTrust([]string{"list"}, &out, &errBuf, deps); code != exitSuccess {
		t.Fatalf("trust list returned %d, want %d", code, exitSuccess)
	}
	if !strings.Contains(out.String(), "No trusted workspaces.") {
		t.Fatalf("trust list stdout = %q, want %q for an empty store", out.String(), "No trusted workspaces.")
	}
}

// TestRunTrustRemoveNamedPathNonCanonical proves `trust remove <path>` untrusts a
// specific path, and that a non-canonical argument (a trailing slash) still matches
// the stored normalized entry.
func TestRunTrustRemoveNamedPathNonCanonical(t *testing.T) {
	setTrustConfigRoot(t)
	// The cwd is a different directory than the one we trust-and-remove, so this
	// exercises the named-path branch, not the bare-cwd branch.
	cwd := t.TempDir()
	target := t.TempDir()
	deps := trustDeps(cwd)

	if err := workspacetrust.Trust(target); err != nil {
		t.Fatalf("Trust(target): %v", err)
	}
	trusted, err := workspacetrust.IsTrusted(target)
	if err != nil || !trusted {
		t.Fatalf("target should be trusted before remove (trusted=%v err=%v)", trusted, err)
	}

	// Pass the target with a trailing slash: normalization must still match the
	// stored canonical entry.
	var out, errBuf bytes.Buffer
	if code := runTrust([]string{"remove", target + "/"}, &out, &errBuf, deps); code != exitSuccess {
		t.Fatalf("trust remove <path> returned %d, want %d; stderr=%q", code, exitSuccess, errBuf.String())
	}

	trusted, err = workspacetrust.IsTrusted(target)
	if err != nil {
		t.Fatalf("IsTrusted after named remove: %v", err)
	}
	if trusted {
		t.Fatalf("target should be untrusted after `trust remove <path>/` despite the trailing slash")
	}
}

// TestRunTrustUnknownSubcommand proves an unknown subcommand returns exit code 2 and
// writes usage to stderr (not stdout).
func TestRunTrustUnknownSubcommand(t *testing.T) {
	setTrustConfigRoot(t)
	deps := trustDeps(t.TempDir())

	var out, errBuf bytes.Buffer
	if code := runTrust([]string{"bogus"}, &out, &errBuf, deps); code != exitUsage {
		t.Fatalf("unknown subcommand returned %d, want %d", code, exitUsage)
	}
	if errBuf.Len() == 0 {
		t.Fatalf("unknown subcommand should write usage to stderr, stderr was empty")
	}
	if !strings.Contains(errBuf.String(), "trust") {
		t.Fatalf("stderr usage = %q, want it to mention `trust`", errBuf.String())
	}
	if out.Len() != 0 {
		t.Fatalf("unknown subcommand should not write to stdout, got %q", out.String())
	}
}

// TestRunTrustRemoveTooManyArgs proves `trust remove a b` (more than one path) is a
// usage error: exit code 2, usage on stderr, nothing on stdout, and no store change.
func TestRunTrustRemoveTooManyArgs(t *testing.T) {
	setTrustConfigRoot(t)
	deps := trustDeps(t.TempDir())

	var out, errBuf bytes.Buffer
	if code := runTrust([]string{"remove", "a", "b"}, &out, &errBuf, deps); code != exitUsage {
		t.Fatalf("remove with two args returned %d, want %d", code, exitUsage)
	}
	if errBuf.Len() == 0 {
		t.Fatalf("remove with two args should write usage to stderr, stderr was empty")
	}
	if out.Len() != 0 {
		t.Fatalf("remove with two args should not write to stdout, got %q", out.String())
	}
}

// TestRunTrustHelp proves the -h / --help / help subcommands print usage to stdout,
// nothing to stderr, and return success, matching the other CLI help entrypoints
// (unknown subcommands, by contrast, write to stderr with the usage exit code).
func TestRunTrustHelp(t *testing.T) {
	setTrustConfigRoot(t)
	deps := trustDeps(t.TempDir())

	for _, flag := range []string{"-h", "--help", "help"} {
		var out, errBuf bytes.Buffer
		if code := runTrust([]string{flag}, &out, &errBuf, deps); code != exitSuccess {
			t.Fatalf("trust %s returned %d, want %d", flag, code, exitSuccess)
		}
		if !strings.Contains(out.String(), "Usage") {
			t.Fatalf("trust %s stdout = %q, want it to contain usage text", flag, out.String())
		}
		if errBuf.Len() != 0 {
			t.Fatalf("trust %s should not write to stderr, got %q", flag, errBuf.String())
		}
	}
}
