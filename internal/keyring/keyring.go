// Package keyring provides a small, dependency-free secret store backed by the
// operating system's native credential tooling: the `security` keychain CLI on
// macOS and `secret-tool` (libsecret) on Linux. It stores a single secret string
// per (service, account). Windows and other platforms report unsupported.
//
// It shells out to the OS tools rather than taking a third-party dependency.
// On both macOS and Linux the secret is passed over stdin, never the argument
// vector, so it is not exposed via the process list. On macOS the write goes
// through `security -i` (interactive mode), whose command parser is line-based
// with a fixed 4096-byte line buffer, so Set rejects secrets containing
// newlines or exceeding that budget rather than silently corrupting them;
// Linux's secret-tool has no such restriction. MaxSecretLen reports that budget
// so a caller holding a larger value can split it across entries instead of
// being turned away.
package keyring

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// commandTimeout bounds a single keyring tool invocation.
const commandTimeout = 10 * time.Second

// ErrUnsupported is returned when no OS keyring backend is available for the
// current platform.
var ErrUnsupported = errors.New("keyring: no OS keyring backend on this platform")

// runner executes name with args and optional stdin, returning stdout. It is the
// single seam tests replace to drive the platform command logic without touching
// a real keychain.
type runner func(ctx context.Context, name string, stdin []byte, args ...string) ([]byte, error)

// Keyring is an OS-native secret store.
type Keyring struct {
	run  runner
	goos string
}

// New returns a Keyring for the current platform.
func New() *Keyring {
	return &Keyring{run: execRunner, goos: runtime.GOOS}
}

// Available reports whether this platform has a supported keyring backend. The
// backing tool (`secret-tool` on Linux) must also be installed; a missing tool
// surfaces as an error from Get/Set/Delete.
func (k *Keyring) Available() bool {
	switch k.goos {
	case "darwin", "linux":
		return true
	default:
		return false
	}
}

// Set stores secret under (service, account), replacing any existing value.
func (k *Keyring) Set(service, account, secret string) error {
	if err := validate(service, account); err != nil {
		return err
	}
	switch k.goos {
	case "darwin":
		// The secret must stay out of the argument vector (visible to every
		// process via `ps`), but a trailing -w prompt is not a substitute:
		// security prompts with getpass(3), which reads from /dev/tty whenever
		// the process has a controlling terminal (ignoring a piped stdin) and
		// truncates at getpass's small fixed buffer otherwise. Instead drive
		// `security -i`: interactive mode reads whole commands from stdin, so
		// argv is just ["-i"] and the secret rides inside the command line.
		// That parser is line-based with a fixed 4096-byte buffer, so reject
		// newlines and oversized payloads up front; an overlong line would be
		// split and executed as two garbage commands, silently corrupting the
		// stored value.
		for _, s := range []string{service, account, secret} {
			if strings.ContainsAny(s, "\r\n") {
				return wrap("set", errors.New("service, account, and secret must not contain newlines on macOS (security -i is line-based)"))
			}
		}
		// -U updates the item if it already exists rather than failing.
		line := securityAddLine(service, account, secret)
		if len(line) > securityMaxLine {
			return wrap("set", fmt.Errorf("secret too large for the macOS security tool's %d-byte command line; use the file backend instead", securityMaxLine))
		}
		_, err := k.exec([]byte(line), "security", "-i")
		return wrap("set", err)
	case "linux":
		// secret-tool reads the secret from stdin, keeping it out of the argv.
		_, err := k.exec([]byte(secret), "secret-tool", "store", "--label", "zero", "service", service, "account", account)
		return wrap("set", err)
	default:
		return ErrUnsupported
	}
}

// Get returns the secret stored under (service, account). The bool is false when
// no entry exists (which is not an error).
func (k *Keyring) Get(service, account string) (string, bool, error) {
	if err := validate(service, account); err != nil {
		return "", false, err
	}
	switch k.goos {
	case "darwin":
		out, err := k.exec(nil, "security", "find-generic-password", "-s", service, "-a", account, "-w")
		if err != nil {
			if isNotFound(err, securityNotFoundExit) {
				return "", false, nil
			}
			return "", false, wrap("get", err)
		}
		return strings.TrimRight(string(out), "\r\n"), true, nil
	case "linux":
		out, err := k.exec(nil, "secret-tool", "lookup", "service", service, "account", account)
		if err != nil {
			if isNotFound(err, secretToolNotFoundExit) {
				return "", false, nil
			}
			return "", false, wrap("get", err)
		}
		value := strings.TrimRight(string(out), "\r\n")
		if value == "" {
			return "", false, nil
		}
		return value, true, nil
	default:
		return "", false, ErrUnsupported
	}
}

// Delete removes the entry under (service, account), reporting whether one
// existed.
func (k *Keyring) Delete(service, account string) (bool, error) {
	if err := validate(service, account); err != nil {
		return false, err
	}
	switch k.goos {
	case "darwin":
		_, err := k.exec(nil, "security", "delete-generic-password", "-s", service, "-a", account)
		if err != nil {
			if isNotFound(err, securityNotFoundExit) {
				return false, nil
			}
			return false, wrap("delete", err)
		}
		return true, nil
	case "linux":
		// `secret-tool clear` always exits 0, so probe existence first.
		_, existed, err := k.Get(service, account)
		if err != nil {
			return false, err
		}
		if _, err := k.exec(nil, "secret-tool", "clear", "service", service, "account", account); err != nil {
			return false, wrap("delete", err)
		}
		return existed, nil
	default:
		return false, ErrUnsupported
	}
}

// MaxSecretLen reports the largest secret, in bytes, that Set accepts under
// (service, account) on this platform. ok is false when the backend imposes no
// practical limit, which is the case for Linux's secret-tool: it reads the
// secret from stdin rather than a command line. On macOS the secret shares the
// `security -i` command line with the service and account, so the budget
// depends on both and a caller that splits an oversized value must size each
// part against the exact account name it will be stored under.
//
// The budget assumes the secret needs no quote escaping. securityQuote only
// expands `\` and `"`, neither of which appears in base64 or hex, so a caller
// storing encoded data gets the exact figure.
func (k *Keyring) MaxSecretLen(service, account string) (int, bool) {
	if k.goos != "darwin" {
		return 0, false
	}
	// Measuring the real command with an empty secret counts the surrounding
	// quotes too, so a secret of exactly this length lands on securityMaxLine.
	budget := securityMaxLine - len(securityAddLine(service, account, ""))
	if budget < 0 {
		budget = 0
	}
	return budget, true
}

// securityMaxLine is the most `security -i` can read as one command: its line
// buffer is MAX_LINE_LEN (4096) bytes in Apple's SecurityTool, one of which the
// terminating NUL consumes.
const securityMaxLine = 4095

// securityAddLine renders the `security -i` command that stores secret under
// (service, account). Set writes it and MaxSecretLen measures it, so the two
// cannot disagree about how much of the line the overhead consumes.
func securityAddLine(service, account, secret string) string {
	return "add-generic-password -U -s " + securityQuote(service) + " -a " + securityQuote(account) + " -w " + securityQuote(secret) + "\n"
}

// securityQuote wraps s for one argument of a `security -i` command line. The
// tool's parser (split_line in Apple's SecurityTool) treats a backslash inside
// double quotes as escaping the next character and the matching quote as the
// argument terminator, so those two characters are all that needs escaping.
func securityQuote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

func (k *Keyring) exec(stdin []byte, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	return k.run(ctx, name, stdin, args...)
}

func execRunner(ctx context.Context, name string, stdin []byte, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if len(stdin) > 0 {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		return out.Bytes(), &runError{err: err, stderr: strings.TrimSpace(errBuf.String())}
	}
	return out.Bytes(), nil
}

// runError carries a tool failure with its stderr and preserves the underlying
// error for errors.As (so exit-code / not-found detection still works).
type runError struct {
	err    error
	stderr string
}

func (e *runError) Error() string {
	if e.stderr != "" {
		return e.stderr
	}
	return e.err.Error()
}

func (e *runError) Unwrap() error { return e.err }

// Not-found exit codes for the OS tools: macOS `security` exits 44
// (errSecItemNotFound) when no matching item exists; `secret-tool` exits 1 when a
// lookup finds nothing. Any other non-zero exit is a real failure.
const (
	securityNotFoundExit   = 44
	secretToolNotFoundExit = 1
)

// isNotFound reports whether err is a tool exit whose code is one of the given
// "no such entry" codes (as opposed to a missing binary or a genuine failure,
// which must not be masked). It matches on the ExitCode behavior (satisfied by
// *exec.ExitError) so the logic is testable without spawning a real process.
func isNotFound(err error, codes ...int) bool {
	var coder interface{ ExitCode() int }
	if !errors.As(err, &coder) {
		return false
	}
	code := coder.ExitCode()
	for _, c := range codes {
		if code == c {
			return true
		}
	}
	return false
}

// wrap adds operation context to a tool error, leaving nil untouched.
func wrap(op string, err error) error {
	if err == nil {
		return nil
	}
	var execErr *exec.Error
	if errors.As(err, &execErr) {
		return fmt.Errorf("keyring: %s: %q not found (install the OS keyring tool): %w", op, execErr.Name, err)
	}
	return fmt.Errorf("keyring: %s: %w", op, err)
}

func validate(service, account string) error {
	if strings.TrimSpace(service) == "" {
		return errors.New("keyring: service is required")
	}
	if strings.TrimSpace(account) == "" {
		return errors.New("keyring: account is required")
	}
	return nil
}
