package cli

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"

	"github.com/Gitlawb/zero/internal/daemon/remote"
	"github.com/Gitlawb/zero/internal/dictation"
	"github.com/Gitlawb/zero/internal/redaction"
)

// runKeptBackups implements `zero kept-backups`, the only way a copy recovery
// retained ever leaves the disk. Recovery moves a copy it will not restore and
// cannot prove superseded under a Kept prefix its own scan never enumerates, and
// nothing reclaims one on its own, so without this command retention is one-way.
//
//	zero kept-backups list [--bundle-dir <dir>]             list what is retained
//	zero kept-backups remove <name> [--bundle-dir <dir>]    remove one by name
func runKeptBackups(args []string, stdout io.Writer, stderr io.Writer, deps appDeps) int {
	rest, bundleDir, err := splitBundleDirFlag(args)
	if err != nil {
		if _, werr := fmt.Fprintf(stderr, "zero kept-backups: %s\n\n", err); werr != nil {
			return exitCrash
		}
		writeKeptBackupsUsage(stderr)
		return exitUsage
	}
	if len(rest) == 0 {
		writeKeptBackupsUsage(stderr)
		return exitUsage
	}
	switch rest[0] {
	case "list", "ls":
		return keptBackupsList(rest[1:], bundleDir, stdout, stderr, deps)
	case "remove", "rm":
		return keptBackupsRemove(rest[1:], bundleDir, stdout, stderr, deps)
	case "-h", "--help", "help":
		// Explicit help is a success path, matching the other subcommands: usage
		// to stdout, exit 0. Only the error paths below write it to stderr.
		writeKeptBackupsUsage(stdout)
		return exitSuccess
	default:
		if _, err := fmt.Fprintf(stderr, "zero kept-backups: unknown subcommand %q\n\n", rest[0]); err != nil {
			return exitCrash
		}
		writeKeptBackupsUsage(stderr)
		return exitUsage
	}
}

// splitBundleDirFlag pulls --bundle-dir out of the argument list wherever it
// appears, so it can sit before or after the subcommand and its name argument.
// The daemon has no config key for the bundle dir; it is a serve-remote flag, so
// the operator has to be able to name the same directory here.
func splitBundleDirFlag(args []string) (rest []string, bundleDir string, err error) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--bundle-dir":
			if i+1 >= len(args) {
				return nil, "", errors.New("--bundle-dir needs a directory")
			}
			bundleDir = args[i+1]
			i++
		case len(arg) > len("--bundle-dir=") && arg[:len("--bundle-dir=")] == "--bundle-dir=":
			bundleDir = arg[len("--bundle-dir="):]
		default:
			rest = append(rest, arg)
		}
	}
	return rest, bundleDir, nil
}

// sttKeptRoot is where the dictation installs live: the same tree the rest of the
// TUI downloads into, derived from userConfigPath rather than the default config
// dir so an overridden config root does not leave this command reading a
// directory nothing writes to.
func sttKeptRoot(deps appDeps) (string, error) {
	path, err := deps.userConfigPath()
	if err != nil {
		return "", err
	}
	if path == "" {
		return "", errors.New("no user config path, so the dictation install root cannot be resolved")
	}
	return filepath.Join(filepath.Dir(path), "stt"), nil
}

func keptBackupsList(args []string, bundleDir string, stdout io.Writer, stderr io.Writer, deps appDeps) int {
	if len(args) > 0 {
		if _, err := fmt.Fprintf(stderr, "zero kept-backups list: unexpected argument %q\n", args[0]); err != nil {
			return exitCrash
		}
		return exitUsage
	}
	root, err := sttKeptRoot(deps)
	if err != nil {
		return writeAppError(stderr, redaction.ErrorMessage(err, redaction.Options{}), exitCrash)
	}
	sttBackups, err := dictation.ListKeptBackups(root)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return writeAppError(stderr, redaction.ErrorMessage(err, redaction.Options{}), exitCrash)
	}
	found, err := writeKeptBackups(stdout, "stt", sttKeptLines(sttBackups))
	if err != nil {
		return exitCrash
	}
	if bundleDir != "" {
		bundleBackups, err := remote.ListKeptBackups(bundleDir)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return writeAppError(stderr, redaction.ErrorMessage(err, redaction.Options{}), exitCrash)
		}
		bundleFound, err := writeKeptBackups(stdout, "bundle", bundleKeptLines(bundleBackups))
		if err != nil {
			return exitCrash
		}
		found = bundleFound || found
	}
	if !found {
		if _, err := fmt.Fprintln(stdout, "No kept backups."); err != nil {
			return exitCrash
		}
	}
	return exitSuccess
}

// keptBackupLine is one retained copy as this command prints it. The two sites
// return their own KeptBackup types, and flattening them here is what keeps the
// output one format rather than two that drift.
type keptBackupLine struct {
	name  string
	dest  string
	seq   int64
	bytes int64
	owned bool
}

func sttKeptLines(backups []dictation.KeptBackup) []keptBackupLine {
	lines := make([]keptBackupLine, 0, len(backups))
	for _, b := range backups {
		lines = append(lines, keptBackupLine{name: filepath.Base(b.Path), dest: b.Dest, seq: b.Seq, bytes: b.Bytes, owned: b.Owned})
	}
	return lines
}

func bundleKeptLines(backups []remote.KeptBackup) []keptBackupLine {
	lines := make([]keptBackupLine, 0, len(backups))
	for _, b := range backups {
		lines = append(lines, keptBackupLine{name: filepath.Base(b.Path), dest: b.Dest, seq: b.Seq, bytes: b.Bytes, owned: b.Owned})
	}
	return lines
}

// writeKeptBackups prints one line per retained copy. The name comes first after
// the site because it is exactly what `remove` takes; the destination is the
// install or link the copy was set aside for, and an entry nothing on disk
// attributes says so instead of borrowing a destination from its own name.
func writeKeptBackups(stdout io.Writer, site string, lines []keptBackupLine) (bool, error) {
	for _, line := range lines {
		dest := line.dest
		if dest == "" {
			dest = "-"
		}
		suffix := ""
		if !line.owned {
			suffix = " unowned"
		}
		if _, err := fmt.Fprintf(stdout, "%s %s dest=%s seq=%d bytes=%d%s\n", site, line.name, dest, line.seq, line.bytes, suffix); err != nil {
			// A listing cut short is not a listing. Reporting success here tells
			// an operator they have seen every retained copy when they have not,
			// and this command is the only place those copies are visible.
			return false, err
		}
	}
	return len(lines) > 0, nil
}

func keptBackupsRemove(args []string, bundleDir string, stdout io.Writer, stderr io.Writer, deps appDeps) int {
	if len(args) != 1 {
		if _, err := fmt.Fprintln(stderr, "usage: zero kept-backups remove <name> [--bundle-dir <dir>]"); err != nil {
			return exitCrash
		}
		return exitUsage
	}
	name := args[0]
	root := bundleDir
	remove := func() error { return remote.RemoveKeptBackup(bundleDir, name) }
	if bundleDir == "" {
		sttRoot, err := sttKeptRoot(deps)
		if err != nil {
			return writeAppError(stderr, redaction.ErrorMessage(err, redaction.Options{}), exitCrash)
		}
		root = sttRoot
		remove = func() error { return dictation.RemoveKeptBackup(sttRoot, name) }
	}
	code, warning := keptRemovalOutcome(remove())
	if code != exitSuccess {
		return writeAppError(stderr, warning, code)
	}
	if _, err := fmt.Fprintf(stdout, "Removed %s from %s\n", name, root); err != nil {
		return exitCrash
	}
	if warning != "" {
		if _, err := fmt.Fprintln(stderr, "zero kept-backups: "+warning); err != nil {
			return exitCrash
		}
	}
	return exitSuccess
}

// keptRemovalOutcome decides what a removal's error means for the operator. A
// lock that could not be released after the copy was already gone is a cleanup
// failure, not a removal that did not happen: reporting it as a crash sends the
// operator to retry a name that no longer exists, and the retry then says
// nothing attributes it, which reads as a different fault entirely. The copy is
// gone either way, so the command succeeds and says what else went wrong.
func keptRemovalOutcome(err error) (code int, warning string) {
	switch {
	case err == nil:
		return exitSuccess, ""
	case errors.Is(err, dictation.ErrInstalledNotReleased), errors.Is(err, remote.ErrCommittedNotReleased):
		return exitSuccess, redaction.ErrorMessage(err, redaction.Options{})
	default:
		return exitCrash, redaction.ErrorMessage(err, redaction.Options{})
	}
}

func writeKeptBackupsUsage(w io.Writer) {
	_, _ = fmt.Fprint(w, `Usage:
  zero kept-backups list [--bundle-dir <dir>]           List retained copies
  zero kept-backups remove <name> [--bundle-dir <dir>]  Remove one by name

Recovery never deletes a copy it cannot prove was superseded; it moves that copy
under a kept- name and leaves it there. Nothing reclaims one on its own, so this
command is how retained copies leave the disk.

Without --bundle-dir both subcommands work on the dictation install root. With
it, remove works on that daemon bundle dir instead, and list adds the bundle dir
to the dictation listing, so each line names the site it came from. Weigh the two differently: a
dictation kept backup is the only offline copy of an engine or a model, while a
bundle kept backup is a work tree the client that sent it can upload again.

Entries marked unowned carry the kept- name with nothing on disk attributing
them. They are reported so they can be found, and remove refuses them; check
what they hold and remove those by hand.
`)
}
