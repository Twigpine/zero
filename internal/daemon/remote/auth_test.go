package remote

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/Gitlawb/zero/internal/remotetoken"
)

func TestTokenAuthenticator(t *testing.T) {
	if _, err := NewTokenAuthenticator("  "); err == nil {
		t.Fatal("empty token must be rejected (fail closed)")
	}
	a, err := NewTokenAuthenticator("s3cret")
	if err != nil {
		t.Fatalf("NewTokenAuthenticator: %v", err)
	}
	if err := a.Authenticate("s3cret"); err != nil {
		t.Fatalf("matching token should authenticate: %v", err)
	}
	if err := a.Authenticate("wrong"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("mismatch err = %v, want ErrUnauthorized", err)
	}
	if err := a.Authenticate(""); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("empty presented token err = %v, want ErrUnauthorized", err)
	}
}

func TestNewAuthenticatorFromEnvPinsOpenedFileIdentityAcrossReplacement(t *testing.T) {
	t.Setenv(EnvToken, "")
	t.Setenv(EnvTokenFileResolved, "")
	t.Setenv(EnvTokenFileIdentity, "")
	dir := t.TempDir()
	token := filepath.Join(dir, "token")
	alias := filepath.Join(dir, "startup-token-alias")
	if err := os.WriteFile(token, []byte("old-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(token, alias); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	t.Setenv(EnvTokenFile, token)
	auth, err := NewAuthenticatorFromEnv()
	if err != nil {
		t.Fatalf("NewAuthenticatorFromEnv: %v", err)
	}
	if err := auth.Authenticate("old-secret"); err != nil {
		t.Fatalf("startup token rejected: %v", err)
	}
	replacement := filepath.Join(dir, "replacement")
	if err := os.WriteFile(replacement, []byte("new-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, token); err != nil {
		t.Fatal(err)
	}
	aliasFile, err := os.Open(alias)
	if err != nil {
		t.Fatal(err)
	}
	want, ok := remotetoken.IdentityOfFile(aliasFile)
	aliasFile.Close()
	if !ok || os.Getenv(EnvTokenFileIdentity) != want {
		t.Fatalf("startup identity = %q, want alias identity %q", os.Getenv(EnvTokenFileIdentity), want)
	}
	if err := auth.Authenticate("new-secret"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("running authenticator accepted replacement token: %v", err)
	}

	restarted, err := NewAuthenticatorFromEnv()
	if err != nil {
		t.Fatalf("restart NewAuthenticatorFromEnv: %v", err)
	}
	if err := restarted.Authenticate("new-secret"); err != nil {
		t.Fatalf("replacement token rejected after restart: %v", err)
	}
	if err := restarted.Authenticate("old-secret"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("restart retained retired authenticator credential: %v", err)
	}
	if err := auth.Authenticate("old-secret"); err != nil {
		t.Fatalf("running authenticator stopped matching its pinned credential: %v", err)
	}
	if got := os.Getenv(EnvTokenFileIdentity); got == want {
		t.Fatal("restart retained old file identity")
	}
}

func TestTokenFromEnv(t *testing.T) {
	// Clear both so the no-config path errors.
	t.Setenv(EnvToken, "")
	t.Setenv(EnvTokenFile, "")
	if _, err := TokenFromEnv(); err == nil {
		t.Fatal("TokenFromEnv with neither var set must error")
	}
	// Direct env.
	t.Setenv(EnvToken, "from-env")
	if tok, err := TokenFromEnv(); err != nil || tok != "from-env" {
		t.Fatalf("TokenFromEnv(env) = %q, %v", tok, err)
	}
	// File (env takes precedence, so clear env first).
	t.Setenv(EnvToken, "")
	file := filepath.Join(t.TempDir(), "tok")
	if err := os.WriteFile(file, []byte("  from-file\n"), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}
	t.Setenv(EnvTokenFile, file)
	if tok, err := TokenFromEnv(); err != nil || tok != "from-file" {
		t.Fatalf("TokenFromEnv(file) = %q, %v", tok, err)
	}
	// Empty file fails closed.
	if err := os.WriteFile(file, []byte("   \n"), 0o600); err != nil {
		t.Fatalf("rewrite token file: %v", err)
	}
	if _, err := TokenFromEnv(); err == nil {
		t.Fatal("empty token file must error")
	}
}

// TestTokenFromEnvTrustsAStaleInheritedResolvedMarker documents the shape
// TokenFromFreshEnv exists to avoid: SourceFromEnv (behind TokenFromEnv) binds
// EnvTokenFileResolved to whatever EnvTokenFile is set to right now, without
// proving it is THAT value's resolved identity. A process that inherits a
// resolved marker left over from an unrelated prior daemon (same shell,
// different invocation) reads the stale target once EnvTokenFile is repointed.
// This is the vulnerable baseline; TestTokenFromFreshEnvIgnoresAStaleResolvedMarker
// is the fix.
func TestTokenFromEnvTrustsAStaleInheritedResolvedMarker(t *testing.T) {
	t.Setenv(EnvToken, "")
	oldToken := filepath.Join(t.TempDir(), "old-token")
	if err := os.WriteFile(oldToken, []byte("old-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	newDir := t.TempDir()
	newToken := filepath.Join(newDir, "new-token")
	if err := os.WriteFile(newToken, []byte("new-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The shell still carries a resolved marker for the OLD token from an
	// unrelated prior serve-remote invocation.
	t.Setenv(EnvTokenFileResolved, oldToken)
	// The operator has since pointed EnvTokenFile at the new token.
	t.Setenv(EnvTokenFile, newToken)

	tok, err := TokenFromEnv()
	if err != nil {
		t.Fatalf("TokenFromEnv: %v", err)
	}
	if tok != "old-secret" {
		t.Fatalf("TokenFromEnv = %q, want it to demonstrate the stale-marker bug by reading old-secret (got the correct new-secret — has the underlying contract changed?)", tok)
	}
}

// TestTokenFromFreshEnvIgnoresAStaleResolvedMarker is the fix: a caller that
// has not run CanonicalizeTokenFileEnv — a one-shot client such as `zero daemon
// link` or a remote dial — must always resolve EnvTokenFile fresh rather than
// trust an inherited resolved marker that could belong to a different
// configured value.
func TestTokenFromFreshEnvIgnoresAStaleResolvedMarker(t *testing.T) {
	t.Setenv(EnvToken, "")
	oldToken := filepath.Join(t.TempDir(), "old-token")
	if err := os.WriteFile(oldToken, []byte("old-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	newDir := t.TempDir()
	newToken := filepath.Join(newDir, "new-token")
	if err := os.WriteFile(newToken, []byte("new-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvTokenFileResolved, oldToken)
	t.Setenv(EnvTokenFile, newToken)

	tok, err := TokenFromFreshEnv()
	if err != nil {
		t.Fatalf("TokenFromFreshEnv: %v", err)
	}
	if tok != "new-secret" {
		t.Fatalf("TokenFromFreshEnv = %q, want new-secret (the currently configured file, not the stale marker)", tok)
	}

	// Inline precedence is preserved: an inline token still wins over both the
	// configured file and any resolved marker.
	t.Setenv(EnvToken, "inline-secret")
	if tok, err := TokenFromFreshEnv(); err != nil || tok != "inline-secret" {
		t.Fatalf("TokenFromFreshEnv(inline) = %q, %v", tok, err)
	}
	t.Setenv(EnvToken, "")

	// No configured file at all still errors, matching TokenFromEnv.
	t.Setenv(EnvTokenFile, "")
	t.Setenv(EnvTokenFileResolved, "")
	if _, err := TokenFromFreshEnv(); err == nil {
		t.Fatal("TokenFromFreshEnv with neither var set must error")
	}
}

// TestSelectedFilePathPreservesFilenameWhitespace pins the pointer as a
// pathname rather than a trimmed word. Trimming it made this boundary select
// "<dir>/bridge-token" while the operator had named "<dir>/bridge-token " —
// which fails the daemon start at best, and at worst reads and protects a
// different file than the one holding the bearer token.
//
// remotetoken.SelectedFilePath is the single selector behind both SourceFromEnv
// and ResolveSource, so this covers the pathname rule for every consumer of the
// contract rather than one boundary's copy of it.
func TestSelectedFilePathPreservesFilenameWhitespace(t *testing.T) {
	t.Setenv(EnvToken, "")
	for _, configured := range []string{"/srv/zero/bridge-token ", " /srv/zero/bridge-token", "/srv/zero/ token "} {
		t.Setenv(EnvTokenFile, configured)
		if got := remotetoken.SelectedFilePath(); got != configured {
			t.Fatalf("SelectedFilePath() = %q, want the configured pathname %q", got, configured)
		}
	}
	for _, blank := range []string{"", "   ", "\t\n"} {
		t.Setenv(EnvTokenFile, blank)
		if got := remotetoken.SelectedFilePath(); got != "" {
			t.Fatalf("SelectedFilePath() = %q for a blank value, want unset", got)
		}
	}
	// An inline token wins: the file pointer is not selected at all, so nothing
	// downstream protects or reads a file the bridge never authenticates against.
	t.Setenv(EnvToken, "inline-secret")
	t.Setenv(EnvTokenFile, "/srv/zero/bridge-token ")
	if got := remotetoken.SelectedFilePath(); got != "" {
		t.Fatalf("SelectedFilePath() = %q while an inline token is set, want unset", got)
	}
}

// TestCanonicalizeTokenFileEnvKeepsTrailingSpaceFilename covers the same rule at
// the daemon boundary: the file the bridge authenticates against must survive
// canonicalization byte for byte.
func TestCanonicalizeTokenFileEnvKeepsTrailingSpaceFilename(t *testing.T) {
	t.Setenv(EnvTokenFileResolved, "")
	if runtime.GOOS == "windows" {
		t.Skip("Windows filenames cannot end in a space")
	}
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	token := filepath.Join(base, "bridge-token ")
	if err := os.WriteFile(token, []byte("from-file\n"), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}
	t.Chdir(base)
	t.Setenv(EnvToken, "")
	t.Setenv(EnvTokenFile, "bridge-token ")

	if err := CanonicalizeTokenFileEnv(); err != nil {
		t.Fatalf("CanonicalizeTokenFileEnv: %v", err)
	}
	if got := os.Getenv(EnvTokenFile); got != token {
		t.Fatalf("%s = %q, want %q", EnvTokenFile, got, token)
	}
	t.Chdir(t.TempDir())
	if tok, err := TokenFromEnv(); err != nil || tok != "from-file" {
		t.Fatalf("TokenFromEnv after canonicalization = %q, %v", tok, err)
	}
}

// TestCanonicalizeTokenFileEnv pins the configured absolute spelling and the
// resolved startup object for every worker.
func TestCanonicalizeTokenFileEnv(t *testing.T) {
	t.Setenv(EnvTokenFileResolved, "")
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	token := filepath.Join(base, "tok")
	if err := os.WriteFile(token, []byte("from-file\n"), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}

	t.Run("relative value becomes absolute", func(t *testing.T) {
		// A worker resolves the inherited value against its own session directory,
		// so a relative value must not survive the daemon boundary.
		t.Chdir(base)
		t.Setenv(EnvToken, "")
		t.Setenv(EnvTokenFile, "tok")
		if err := CanonicalizeTokenFileEnv(); err != nil {
			t.Fatalf("CanonicalizeTokenFileEnv: %v", err)
		}
		if got := os.Getenv(EnvTokenFile); got != token {
			t.Fatalf("%s = %q, want %q", EnvTokenFile, got, token)
		}
		// Workers run from session directories, so prove the selected path remains
		// pinned after crossing that boundary rather than merely inspecting the env.
		t.Chdir(t.TempDir())
		if tok, err := TokenFromEnv(); err != nil || tok != "from-file" {
			t.Fatalf("TokenFromEnv from a worker directory after canonicalization = %q, %v", tok, err)
		}
	})

	t.Run("symlinked pathname retains configured and resolved identities", func(t *testing.T) {
		link := filepath.Join(base, "tok-link")
		if err := os.Symlink(token, link); err != nil {
			t.Skipf("symlink unsupported: %v", err)
		}
		t.Setenv(EnvToken, "")
		t.Setenv(EnvTokenFile, link)
		if err := CanonicalizeTokenFileEnv(); err != nil {
			t.Fatalf("CanonicalizeTokenFileEnv: %v", err)
		}
		if got := os.Getenv(EnvTokenFile); got != link {
			t.Fatalf("%s = %q, want configured path %q", EnvTokenFile, got, link)
		}
		if got := os.Getenv(EnvTokenFileResolved); got != token {
			t.Fatalf("%s = %q, want resolved target %q", EnvTokenFileResolved, got, token)
		}

		replacement := filepath.Join(base, "replacement-token")
		if err := os.WriteFile(replacement, []byte("replacement\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(link); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(replacement, link); err != nil {
			t.Fatal(err)
		}
		if tok, err := TokenFromEnv(); err != nil || tok != "from-file" {
			t.Fatalf("TokenFromEnv after symlink replacement = %q, %v, want startup-pinned token", tok, err)
		}
		if err := CanonicalizeTokenFileEnv(); err != nil {
			t.Fatalf("CanonicalizeTokenFileEnv after restart: %v", err)
		}
		if tok, err := TokenFromEnv(); err != nil || tok != "replacement" {
			t.Fatalf("TokenFromEnv after simulated restart = %q, %v, want replacement target", tok, err)
		}
	})

	t.Run("an inline token keeps precedence over a dangling pointer", func(t *testing.T) {
		// TokenFromEnv prefers EnvToken, so an unused (even dangling) file pointer
		// must neither fail the start nor be rewritten.
		dangling := filepath.Join(base, "missing", "tok")
		t.Setenv(EnvToken, "from-env")
		t.Setenv(EnvTokenFile, dangling)
		if err := CanonicalizeTokenFileEnv(); err != nil {
			t.Fatalf("CanonicalizeTokenFileEnv with an inline token: %v", err)
		}
		if got := os.Getenv(EnvTokenFile); got != dangling {
			t.Fatalf("%s = %q, want it left alone", EnvTokenFile, got)
		}
		if tok, err := TokenFromEnv(); err != nil || tok != "from-env" {
			t.Fatalf("TokenFromEnv = %q, %v, want the inline token", tok, err)
		}
	})

	t.Run("a selected but unreadable pointer fails closed", func(t *testing.T) {
		t.Setenv(EnvToken, "")
		t.Setenv(EnvTokenFile, filepath.Join(base, "missing", "tok"))
		if err := CanonicalizeTokenFileEnv(); err == nil {
			t.Fatal("a selected token file that cannot be resolved must error")
		}
	})

	t.Run("no pointer is a no-op", func(t *testing.T) {
		t.Setenv(EnvToken, "")
		t.Setenv(EnvTokenFile, "")
		if err := CanonicalizeTokenFileEnv(); err != nil {
			t.Fatalf("CanonicalizeTokenFileEnv without a pointer: %v", err)
		}
		if got := os.Getenv(EnvTokenFile); got != "" {
			t.Fatalf("%s = %q, want empty", EnvTokenFile, got)
		}
	})
}

func TestServerTLSConfigRequiresCertKey(t *testing.T) {
	if _, err := ServerTLSConfig("", ""); err == nil {
		t.Fatal("ServerTLSConfig must require a cert and key (TLS mandatory)")
	}
	if _, err := ServerTLSConfig("/nope/cert.pem", "/nope/key.pem"); err == nil {
		t.Fatal("ServerTLSConfig must error on unreadable cert/key")
	}
}
