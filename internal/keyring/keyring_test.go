package keyring

import (
	"context"
	"os/exec"
	"strings"
	"testing"
)

// fakeExit is a not-found exit error: it satisfies the ExitCode() seam used by
// isNotFound without spawning a real process.
type fakeExit struct{ code int }

func (e fakeExit) Error() string { return "exit status" }
func (e fakeExit) ExitCode() int { return e.code }

// fakeKeyring is an in-memory simulation of the OS tools driven through the
// runner seam. It records the last stdin so tests can assert the secret never
// travels via argv on Linux.
type fakeKeyring struct {
	goos      string
	data      map[string]string
	lastStdin string
	lastArgs  []string
}

func newFake(goos string) *fakeKeyring {
	return &fakeKeyring{goos: goos, data: map[string]string{}}
}

func (f *fakeKeyring) keyring() *Keyring { return &Keyring{run: f.run, goos: f.goos} }

func flagValue(args []string, flag string) string {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == flag {
			return args[i+1]
		}
	}
	return ""
}

func attrValue(args []string, attr string) string {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == attr {
			return args[i+1]
		}
	}
	return ""
}

func key(service, account string) string { return service + "\x00" + account }

// splitSecurityLine mirrors split_line in Apple's SecurityTool so the fake
// parses `security -i` command lines the way the real tool does: whitespace
// separates arguments, double or single quotes group one, and a backslash
// escapes the next character both inside and outside quotes.
func splitSecurityLine(line string) []string {
	var args []string
	var cur strings.Builder
	inArg, escaped := false, false
	quote := byte(0)
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case escaped:
			cur.WriteByte(c)
			escaped = false
		case c == '\\':
			escaped = true
			inArg = true
		case quote != 0:
			if c != quote {
				cur.WriteByte(c)
				continue
			}
			args = append(args, cur.String())
			cur.Reset()
			inArg, quote = false, 0
		case c == '"' || c == '\'':
			quote = c
			inArg = true
		case c == ' ' || c == '\t':
			if inArg {
				args = append(args, cur.String())
				cur.Reset()
				inArg = false
			}
		default:
			cur.WriteByte(c)
			inArg = true
		}
	}
	if inArg {
		args = append(args, cur.String())
	}
	return args
}

func (f *fakeKeyring) run(_ context.Context, name string, stdin []byte, args ...string) ([]byte, error) {
	f.lastStdin = string(stdin)
	f.lastArgs = append([]string{name}, args...)
	if len(args) == 0 {
		return nil, fakeExit{1}
	}
	switch f.goos {
	case "darwin":
		cmdArgs := args
		if args[0] == "-i" {
			// Interactive mode: the command arrives as one line on stdin.
			line, _, _ := strings.Cut(string(stdin), "\n")
			cmdArgs = splitSecurityLine(line)
			if len(cmdArgs) == 0 {
				return nil, fakeExit{1}
			}
		}
		svc, acct := flagValue(cmdArgs, "-s"), flagValue(cmdArgs, "-a")
		switch cmdArgs[0] {
		case "add-generic-password":
			f.data[key(svc, acct)] = flagValue(cmdArgs, "-w")
			return nil, nil
		case "find-generic-password":
			if v, ok := f.data[key(svc, acct)]; ok {
				return []byte(v + "\n"), nil // security prints a trailing newline
			}
			return nil, fakeExit{44}
		case "delete-generic-password":
			if _, ok := f.data[key(svc, acct)]; ok {
				delete(f.data, key(svc, acct))
				return nil, nil
			}
			return nil, fakeExit{44}
		}
	case "linux":
		svc, acct := attrValue(args, "service"), attrValue(args, "account")
		switch args[0] {
		case "store":
			f.data[key(svc, acct)] = string(stdin)
			return nil, nil
		case "lookup":
			if v, ok := f.data[key(svc, acct)]; ok {
				return []byte(v), nil // secret-tool prints no trailing newline
			}
			return nil, fakeExit{1}
		case "clear":
			delete(f.data, key(svc, acct))
			return nil, nil
		}
	}
	return nil, fakeExit{1}
}

func TestKeyringGetSurfacesNonNotFoundError(t *testing.T) {
	// On macOS only exit 44 (errSecItemNotFound) means "no entry"; any other
	// non-zero exit is a real failure that must surface, not be masked as absent.
	k := &Keyring{
		goos: "darwin",
		run: func(_ context.Context, _ string, _ []byte, _ ...string) ([]byte, error) {
			return nil, fakeExit{1}
		},
	}
	if _, ok, err := k.Get("zero", "tokens"); err == nil || ok {
		t.Fatalf("a non-44 exit must surface as an error, got ok=%v err=%v", ok, err)
	}
}

func TestKeyringRoundTripDarwin(t *testing.T) {
	k := newFake("darwin").keyring()
	if err := k.Set("zero", "tokens", "blob-AAA"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, ok, err := k.Get("zero", "tokens")
	if err != nil || !ok {
		t.Fatalf("Get: ok=%v err=%v", ok, err)
	}
	if got != "blob-AAA" {
		t.Fatalf("Get = %q, want blob-AAA", got)
	}
	existed, err := k.Delete("zero", "tokens")
	if err != nil || !existed {
		t.Fatalf("Delete: existed=%v err=%v", existed, err)
	}
	if _, ok, _ := k.Get("zero", "tokens"); ok {
		t.Fatal("token should be gone after delete")
	}
}

func TestKeyringRoundTripDarwinUsesStdin(t *testing.T) {
	f := newFake("darwin")
	k := f.keyring()
	if err := k.Set("zero", "tokens", "blob-CCC"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	// The secret must travel via stdin, never the argument vector: the write
	// goes through `security -i`, whose whole command line (secret included)
	// arrives on stdin while argv carries only the -i flag.
	wantStdin := "add-generic-password -U -s \"zero\" -a \"tokens\" -w \"blob-CCC\"\n"
	if f.lastStdin != wantStdin {
		t.Fatalf("secret not sent via stdin correctly: stdin=%q, want=%q", f.lastStdin, wantStdin)
	}
	wantArgs := []string{"security", "-i"}
	if len(f.lastArgs) != len(wantArgs) || f.lastArgs[0] != wantArgs[0] || f.lastArgs[1] != wantArgs[1] {
		t.Fatalf("argv = %v, want %v", f.lastArgs, wantArgs)
	}
	got, ok, err := k.Get("zero", "tokens")
	if err != nil || !ok || got != "blob-CCC" {
		t.Fatalf("Get = %q ok=%v err=%v", got, ok, err)
	}
	existed, err := k.Delete("zero", "tokens")
	if err != nil || !existed {
		t.Fatalf("Delete: existed=%v err=%v", existed, err)
	}
}

// TestKeyringSetRejectsMultilineSecretOnDarwin guards against silently
// truncating a secret containing a newline: `security -i` reads one command
// per line, so an embedded newline would split the write into two garbage
// commands. Set must reject this before ever invoking security, not store a
// corrupted value.
func TestKeyringSetRejectsMultilineSecretOnDarwin(t *testing.T) {
	for _, secret := range []string{"line1\nline2", "line1\r\nline2", "trailing\n"} {
		f := newFake("darwin")
		k := f.keyring()
		if err := k.Set("zero", "tokens", secret); err == nil {
			t.Fatalf("Set(%q) = nil error, want rejection of the embedded newline", secret)
		}
		if f.lastArgs != nil {
			t.Fatalf("Set(%q) should be rejected before invoking security, got args=%v", secret, f.lastArgs)
		}
	}
}

// TestKeyringDarwinQuotesSpecialCharacters proves the quoting survives the
// real tool's parser: the fake tokenizes stdin with a faithful mirror of
// SecurityTool's split_line, so a secret full of quotes, backslashes, and
// spaces must round-trip unchanged.
func TestKeyringDarwinQuotesSpecialCharacters(t *testing.T) {
	for _, secret := range []string{
		`spa ced`,
		`quo"te`,
		`back\slash`,
		`sin'gle`,
		`mi"x'ed \" \\ end\`,
		` leading and trailing `,
	} {
		f := newFake("darwin")
		k := f.keyring()
		if err := k.Set("zero", "tokens", secret); err != nil {
			t.Fatalf("Set(%q): %v", secret, err)
		}
		got, ok, err := k.Get("zero", "tokens")
		if err != nil || !ok || got != secret {
			t.Fatalf("Get after Set(%q) = %q ok=%v err=%v", secret, got, ok, err)
		}
	}
}

// TestKeyringSetRejectsOversizedSecretOnDarwin guards the `security -i` line
// budget: MAX_LINE_LEN is 4096 bytes, and an overlong line would be split and
// executed as two garbage commands. A payload comfortably under the budget
// must succeed; one over it must be rejected before invoking security.
func TestKeyringSetRejectsOversizedSecretOnDarwin(t *testing.T) {
	f := newFake("darwin")
	k := f.keyring()
	if err := k.Set("zero", "tokens", strings.Repeat("a", 4000)); err != nil {
		t.Fatalf("Set(4000 bytes): %v", err)
	}
	f = newFake("darwin")
	k = f.keyring()
	if err := k.Set("zero", "tokens", strings.Repeat("a", 5000)); err == nil {
		t.Fatal("Set(5000 bytes) = nil error, want rejection of the oversized line")
	}
	if f.lastArgs != nil {
		t.Fatalf("oversized Set should be rejected before invoking security, got args=%v", f.lastArgs)
	}
}

// TestKeyringSetAllowsMultilineSecretOnLinux confirms the newline restriction
// is macOS-specific: secret-tool reads the whole stdin payload as the secret,
// with no line-based prompt to corrupt an embedded newline.
func TestKeyringSetAllowsMultilineSecretOnLinux(t *testing.T) {
	f := newFake("linux")
	k := f.keyring()
	secret := "line1\nline2"
	if err := k.Set("zero", "tokens", secret); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, ok, err := k.Get("zero", "tokens")
	if err != nil || !ok || got != secret {
		t.Fatalf("Get = %q ok=%v err=%v, want %q", got, ok, err, secret)
	}
}

func TestKeyringRoundTripLinuxUsesStdin(t *testing.T) {
	f := newFake("linux")
	k := f.keyring()
	if err := k.Set("zero", "tokens", "blob-BBB"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	// The secret must travel via stdin, never the argument vector.
	if f.lastStdin != "blob-BBB" {
		t.Fatalf("secret not sent via stdin: stdin=%q", f.lastStdin)
	}
	for _, a := range f.lastArgs {
		if strings.Contains(a, "blob-BBB") {
			t.Fatalf("secret leaked into argv: %v", f.lastArgs)
		}
	}
	got, ok, err := k.Get("zero", "tokens")
	if err != nil || !ok || got != "blob-BBB" {
		t.Fatalf("Get = %q ok=%v err=%v", got, ok, err)
	}
	existed, err := k.Delete("zero", "tokens")
	if err != nil || !existed {
		t.Fatalf("Delete: existed=%v err=%v", existed, err)
	}
}

func TestKeyringGetMissingIsNotError(t *testing.T) {
	for _, goos := range []string{"darwin", "linux"} {
		k := newFake(goos).keyring()
		if _, ok, err := k.Get("zero", "absent"); err != nil || ok {
			t.Fatalf("[%s] Get(absent) = ok=%v err=%v, want false/nil", goos, ok, err)
		}
		if existed, err := k.Delete("zero", "absent"); err != nil || existed {
			t.Fatalf("[%s] Delete(absent) = existed=%v err=%v, want false/nil", goos, existed, err)
		}
	}
}

func TestKeyringUnsupportedPlatform(t *testing.T) {
	k := &Keyring{run: newFake("windows").run, goos: "windows"}
	if k.Available() {
		t.Fatal("windows should report unavailable")
	}
	if err := k.Set("zero", "tokens", "x"); err == nil {
		t.Fatal("Set on unsupported platform should error")
	}
	if _, _, err := k.Get("zero", "tokens"); err == nil {
		t.Fatal("Get on unsupported platform should error")
	}
	if _, err := k.Delete("zero", "tokens"); err == nil {
		t.Fatal("Delete on unsupported platform should error")
	}
}

func TestKeyringValidation(t *testing.T) {
	k := newFake("darwin").keyring()
	if err := k.Set("", "a", "s"); err == nil {
		t.Fatal("empty service should error")
	}
	if err := k.Set("svc", "", "s"); err == nil {
		t.Fatal("empty account should error")
	}
}

func TestKeyringMissingBinaryError(t *testing.T) {
	// A missing tool surfaces as a wrapped, descriptive error (not not-found).
	k := &Keyring{goos: "linux", run: func(context.Context, string, []byte, ...string) ([]byte, error) {
		return nil, &exec.Error{Name: "secret-tool", Err: exec.ErrNotFound}
	}}
	if err := k.Set("zero", "tokens", "x"); err == nil || !strings.Contains(err.Error(), "secret-tool") {
		t.Fatalf("missing-binary Set error = %v, want mention of secret-tool", err)
	}
	// A missing binary on Get must not be misread as not-found.
	if _, ok, err := k.Get("zero", "tokens"); err == nil || ok {
		t.Fatalf("missing-binary Get = ok=%v err=%v, want error", ok, err)
	}
}

func TestAvailable(t *testing.T) {
	if !(newFake("darwin").keyring().Available()) {
		t.Fatal("darwin should be available")
	}
	if !(newFake("linux").keyring().Available()) {
		t.Fatal("linux should be available")
	}
}

// TestMaxSecretLenMatchesTheDarwinSetBoundary pins the budget to the boundary
// Set actually enforces. A caller that splits an oversized value depends on the
// two agreeing exactly: one byte of drift either wastes an entry or produces a
// chunk security silently splits into two garbage commands.
func TestMaxSecretLenMatchesTheDarwinSetBoundary(t *testing.T) {
	k := newFake("darwin").keyring()
	budget, bounded := k.MaxSecretLen("zero", "oauth-tokens")
	if !bounded {
		t.Fatal("MaxSecretLen reported darwin as unbounded")
	}
	if err := newFake("darwin").keyring().Set("zero", "oauth-tokens", strings.Repeat("a", budget)); err != nil {
		t.Errorf("Set(budget=%d bytes): %v, want acceptance at the boundary", budget, err)
	}
	if err := newFake("darwin").keyring().Set("zero", "oauth-tokens", strings.Repeat("a", budget+1)); err == nil {
		t.Errorf("Set(%d bytes) succeeded, want rejection one byte past the budget", budget+1)
	}
}

// TestMaxSecretLenShrinksWithTheAccountName covers why the budget takes the
// account: on macOS the account and the secret share one command line, so a
// longer account name leaves less room for the secret.
func TestMaxSecretLenShrinksWithTheAccountName(t *testing.T) {
	k := newFake("darwin").keyring()
	short, _ := k.MaxSecretLen("zero", "oauth-tokens")
	long, _ := k.MaxSecretLen("zero", "oauth-tokens.a.63")
	if want := short - len(".a.63"); long != want {
		t.Fatalf("MaxSecretLen for the longer account = %d, want %d", long, want)
	}
	if err := newFake("darwin").keyring().Set("zero", "oauth-tokens.a.63", strings.Repeat("a", long+1)); err == nil {
		t.Error("Set one byte past the longer account's budget succeeded")
	}
}

// TestMaxSecretLenUnboundedOffDarwin keeps the limit platform-specific:
// secret-tool reads the secret from stdin, so there is no command line to fill.
func TestMaxSecretLenUnboundedOffDarwin(t *testing.T) {
	for _, goos := range []string{"linux", "windows"} {
		if n, bounded := newFake(goos).keyring().MaxSecretLen("zero", "oauth-tokens"); bounded || n != 0 {
			t.Errorf("MaxSecretLen on %s = (%d, %v), want (0, false)", goos, n, bounded)
		}
	}
}
