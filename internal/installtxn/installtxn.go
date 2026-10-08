// Package installtxn provides the cross-process filesystem transaction used by
// plugin and skill installation. Callers stage content before taking the lock,
// then commit the content swap and lockfile update together while holding it.
package installtxn

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const lockFileName = ".zero-install.lock"

// workspacePrefix names the per-transaction workspaces created inside an install
// root. Dot-prefixed so it is never mistaken for an installed plugin or skill.
const workspacePrefix = ".zero-install-txn-"

// markerFileName records, inside a workspace, which install the backup beside it
// belongs to. Without it a workspace left by a killed process holds a tree
// nothing can attribute, and so nothing can put back.
const markerFileName = ".zero-install-txn"

// markerMagic is the first line of that marker. Ownership has to be proven
// rather than inferred from filesystem shape: a user authored skill directory
// may legitimately be named with the workspace prefix and hold a previous
// directory, and recovery acting on one would destroy installed content. No
// ordinary content carries this line by accident.
const markerMagic = "zero-install-txn v1"

// Lock takes the per-install-root cross-process lock. It blocks until any other
// installer or remover using dir has completed.
func Lock(dir string) (func(), error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create install dir: %w", err)
	}
	return lockFile(filepath.Join(dir, lockFileName))
}

// StageDir creates an install workspace on the target filesystem. Content must
// be built and validated in the returned stage directory before CommitDir is
// called. cleanup is always safe to call.
func StageDir(dir string) (stage string, cleanup func(), err error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", func() {}, fmt.Errorf("create install dir: %w", err)
	}
	workspace, err := os.MkdirTemp(dir, workspacePrefix)
	if err != nil {
		return "", func() {}, fmt.Errorf("create install staging dir: %w", err)
	}
	return filepath.Join(workspace, "staged"), func() { cleanupWorkspace(workspace) }, nil
}

// CommitDir replaces target with staged and runs publish while retaining the
// previous target. If either the swap or publish fails, the previous target is
// restored (or the new target is removed for a first install).
//
// The caller must hold the install-root lock returned by Lock.
func CommitDir(target string, staged string, publish func() error) error {
	workspace := filepath.Dir(staged)
	backup := filepath.Join(workspace, "previous")
	hadPrevious := false
	if _, err := os.Stat(target); err == nil {
		// Record the target before moving its tree. The two renames below cannot
		// be made atomic, so a process killed between them leaves the only copy
		// in the backup, and without this nothing could tell which install it is.
		if err := os.WriteFile(filepath.Join(workspace, markerFileName), []byte(markerMagic+"\ntarget "+filepath.Base(target)+"\n"), 0o600); err != nil {
			return fmt.Errorf("record install target: %w", err)
		}
		if err := os.Rename(target, backup); err != nil {
			return fmt.Errorf("retain previous install: %w", err)
		}
		hadPrevious = true
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect previous install: %w", err)
	}

	if err := os.Rename(staged, target); err != nil {
		if hadPrevious {
			if restoreErr := os.Rename(backup, target); restoreErr != nil {
				return errors.Join(fmt.Errorf("publish staged install: %w", err), fmt.Errorf("restore previous install: %w", restoreErr))
			}
		}
		return fmt.Errorf("publish staged install: %w", err)
	}
	if err := publish(); err != nil {
		return rollback(target, backup, hadPrevious, err)
	}
	if hadPrevious {
		_ = os.RemoveAll(backup)
	}
	cleanupWorkspace(workspace)
	return nil
}

// RemoveDir removes target and runs publish while retaining the target until
// publish succeeds. A publish failure restores the directory.
//
// The caller must hold the install-root lock returned by Lock.
func RemoveDir(target string, publish func() error) error {
	workspace, err := os.MkdirTemp(filepath.Dir(target), workspacePrefix)
	if err != nil {
		return fmt.Errorf("create removal staging dir: %w", err)
	}
	defer cleanupWorkspace(workspace)
	backup := filepath.Join(workspace, "previous")
	if err := os.Rename(target, backup); err != nil {
		return fmt.Errorf("retain removed install: %w", err)
	}
	if err := publish(); err != nil {
		if restoreErr := os.Rename(backup, target); restoreErr != nil {
			return errors.Join(err, fmt.Errorf("restore removed install: %w", restoreErr))
		}
		return err
	}
	_ = os.RemoveAll(backup)
	return nil
}

// Phase names which tree in an interrupted workspace the caller's published
// metadata records.
type Phase int

const (
	// PhaseUnknown means the metadata matches neither tree. Recovery refuses to
	// act and reports the workspace as unresolved.
	PhaseUnknown Phase = iota
	// PhaseCommitted means the live target is what the metadata records: the
	// publish committed, and the backup beside it is superseded.
	PhaseCommitted
	// PhasePrePublish means the metadata still records the retained backup: the
	// tree swap landed but the publish never did.
	PhasePrePublish
)

// Reconciler classifies one interrupted workspace for the install named name.
// target is the live install path, backup the retained tree beside it. An error
// means the classification could not be made and recovery must not act.
type Reconciler func(name string, target string, backup string) (Phase, error)

// Recover resolves the workspaces an earlier run was killed inside, which is
// what a process killed mid-commit leaves: a tree retained in a workspace
// nothing else reads. How far that commit got cannot be read off the
// filesystem. CommitDir swaps the trees and only then publishes, so a kill
// between the two leaves exactly what a kill after both leaves, and a phase bit
// written after the publish only inverts which side of the write the ambiguity
// falls on. Only the caller knows what its published metadata records, so
// reconcile is asked which of the two trees that is, and its answer decides:
// the live target stands and the backup beside it is retired, or the backup is
// the truthful tree and goes back over the target. A nil reconciler means the
// caller publishes no metadata beside the tree, so a live target is the
// committed one. With nothing at the target there is no choice to make and the
// backup, the only copy in existence, is put back.
//
// The returned error means an attributable transaction was left unresolved,
// never that there was nothing to do. Malformed, legacy, and unattributable
// workspaces are skipped without error, since they may be somebody's content or
// somebody else's transaction, but a workspace we own and could not resolve is
// reported. Every workspace is processed before returning, so one unresolved
// transaction does not strand the rest.
//
// The caller must hold the install-root lock returned by Lock, and EVERY caller
// that takes that lock must call this first and abort on its error before it
// reads the lockfile, inspects the target, installs over it, or reports a
// successful removal. Recovering only on the install path is worse than not
// recovering at all: a removal would then report success while the backup it
// never saw stayed on disk, and the next install would publish it again,
// reinstating something the user deleted. Recovery is deliberately an explicit
// call rather than a side effect of Lock, matching how the other staged-swap
// transactions in this repo invoke their repair pass.
func Recover(dir string, reconcile Reconciler) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		// A recovery set that was never enumerated is not an empty one, and the
		// caller must not go on to install over or report the removal of a tree
		// that may still be owed a restore.
		return fmt.Errorf("enumerate install workspaces: %w", err)
	}
	var unresolved []error
	// Gather first, act second. A pass that acts as it walks cannot see that two
	// workspaces claim one install, and acting on both in turn publishes one over
	// the other and deletes the rest.
	type claim struct {
		workspace string
		name      string
		target    string
		backup    string
	}
	var claims []claim
	claimants := map[string]int{}
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), workspacePrefix) {
			continue
		}
		workspace := filepath.Join(dir, entry.Name())
		recorded, ok, err := markerTarget(workspace)
		if err != nil {
			// A marker we could not read is not a marker that is absent. Skipping
			// silently would tell the caller there was nothing to do while the only
			// copy of an install stays in the workspace.
			unresolved = append(unresolved, fmt.Errorf("read the transaction marker in %s: %w", workspace, err))
			continue
		}
		if !ok {
			continue
		}
		name, target, ok := recoverableTarget(dir, recorded)
		if !ok {
			continue
		}
		backup := filepath.Join(workspace, "previous")
		info, err := os.Lstat(backup)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				// Mid-transaction: the trees never moved, so there is nothing to
				// put back and the workspace belongs to whoever made it.
				continue
			}
			unresolved = append(unresolved, fmt.Errorf("inspect the retained install in %s: %w", workspace, err))
			continue
		}
		if !info.IsDir() {
			// Only a directory is something this package set aside. Renaming a
			// symlink into the install root publishes whatever it points at, from
			// anywhere on the filesystem.
			unresolved = append(unresolved, fmt.Errorf("the retained install in %s is not a directory, so recovery will not publish it", workspace))
			continue
		}
		claims = append(claims, claim{workspace: workspace, name: name, target: target, backup: backup})
		claimants[name]++
	}
	for _, c := range claims {
		if claimants[c.name] > 1 {
			// Nothing ranks two claims on one install, and publishing them in turn
			// destroys every tree but the last.
			unresolved = append(unresolved, fmt.Errorf("install %s is claimed by more than one interrupted transaction, including %s: resolve them by hand", c.name, c.workspace))
			continue
		}
		if err := recoverWorkspace(c.workspace, c.target, c.backup, c.name, reconcile); err != nil {
			unresolved = append(unresolved, err)
		}
	}
	return errors.Join(unresolved...)
}

// recoverWorkspace resolves one attributable workspace whose backup is present.
func recoverWorkspace(workspace string, target string, backup string, name string, reconcile Reconciler) error {
	info, err := os.Lstat(target)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			// A target we could not probe is not a target that is absent. Reading
			// it as absent enters the restore branch, the one branch that moves a
			// tree, on the strength of a question that never got an answer. No test
			// injects this on Linux: the names that used to reach here and fail are
			// now refused by recoverableTarget, and a permission fault on the root
			// stops at the marker read above. It is kept for the platforms that can
			// still fail this call, Windows reserved device names among them.
			return fmt.Errorf("inspect install %s: %w", name, err)
		}
		// Nothing at the target, so the swap never finished and the backup is the
		// only copy there is. There is no second tree to weigh it against, and
		// asking could only produce an answer that throws it away.
		if err := os.Rename(backup, target); err != nil {
			return fmt.Errorf("restore interrupted install %s: %w", name, err)
		}
		cleanupWorkspace(workspace)
		return nil
	}
	if !info.IsDir() {
		// An entry at the target is not an install at the target. A dangling
		// symlink satisfies Lstat, and reading it as a live install retires the
		// backup beside it, which is the only real copy there is.
		return fmt.Errorf("install %s is not a directory, so recovery cannot tell what it is: resolve %s by hand", name, workspace)
	}
	phase := PhaseCommitted
	if reconcile != nil {
		classified, err := reconcile(name, target, backup)
		if err != nil {
			// A caller that could not classify is not permission to fall back on
			// filesystem shape, which is the inference this protocol exists to
			// remove.
			return fmt.Errorf("classify interrupted install %s: %w", name, err)
		}
		phase = classified
	}
	switch phase {
	case PhaseCommitted:
		// The backup holds what the committed publish replaced. Keeping it made a
		// removal reversible by accident: the removal deleted the live target, and
		// the next recovery then read the absent target as an interrupted swap and
		// published the stale tree again. Only the workspace goes, never the live
		// install. This is the one place os.RemoveAll is right over
		// cleanupWorkspace, which refuses a workspace holding a previous precisely
		// because it cannot tell a superseded backup from one still owed a
		// restore.
		//
		// The backup goes before the workspace around it, because os.RemoveAll
		// walks the workspace in name order and so unlinks the marker first. A
		// removal that then failed inside the backup left a workspace holding a
		// tree nothing could attribute, which the next pass reads as somebody
		// else's content and stays silent about. Clearing the backup first means
		// whatever survives a failure is still ours and is still reported.
		if err := os.RemoveAll(backup); err != nil {
			return fmt.Errorf("retire superseded install %s: %w", name, err)
		}
		if err := os.RemoveAll(workspace); err != nil {
			return fmt.Errorf("retire superseded install %s: %w", name, err)
		}
		return nil
	case PhasePrePublish:
		return restoreOverTarget(workspace, target, backup, name)
	default:
		// Neither tree is the one the metadata records, so recovery cannot tell
		// which one the user is owed and either guess risks destroying the other.
		return fmt.Errorf("interrupted install %s matches neither the live target nor the retained backup: resolve %s by hand", name, workspace)
	}
}

// restoreOverTarget puts the backup back over a target that is still there,
// which is what the metadata recording the backup means: the tree swap landed
// but the publish never did, and recovery cannot publish forward because it does
// not know the source the interrupted install was writing.
func restoreOverTarget(workspace string, target string, backup string, name string) error {
	// Same ordering rollback uses. Removing the superseded tree in place would
	// leave a husk at the target while the backup was still the only complete
	// copy, and a kill in that window is unrecoverable. With the renames in this
	// order every instant has either a whole tree at the target or nothing there
	// and the backup intact, which is exactly what this pass can tell apart.
	failed := filepath.Join(workspace, "failed")
	// An earlier rollback can have left a tree here. The workspace is proven ours
	// by its marker, so clearing it is safe, and leaving it is not: the rename
	// below fails for as long as it is there, and because every caller aborts on a
	// recovery error that wedges every install and removal in this root.
	if err := os.RemoveAll(failed); err != nil {
		return fmt.Errorf("clear the superseded install set aside in %s: %w", workspace, err)
	}
	if err := os.Rename(target, failed); err != nil {
		return fmt.Errorf("set aside superseded install %s: %w", name, err)
	}
	if err := os.Rename(backup, target); err != nil {
		return fmt.Errorf("restore interrupted install %s: %w", name, err)
	}
	if err := os.RemoveAll(failed); err != nil {
		return fmt.Errorf("remove superseded install %s: %w", name, err)
	}
	cleanupWorkspace(workspace)
	return nil
}

// markerTarget reads the install name a workspace records, and reports whether
// the workspace is one of ours at all. The prefix alone does not prove that: it
// is a public dot prefixed name and the skill loader enumerates dot prefixed
// directories, so a user authored skill can carry it and hold the same entries a
// workspace does. Only the magic and version first line is evidence no ordinary
// content produces by accident, so anything else is somebody's content and is
// left alone.
func markerTarget(workspace string) (string, bool, error) {
	path := filepath.Join(workspace, markerFileName)
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", false, nil
		}
		return "", false, err
	}
	if !info.Mode().IsRegular() {
		return "", false, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		// Present but unreadable is not absent, and the difference decides whether
		// a retained tree is put back or stranded where nothing reads it.
		return "", false, err
	}
	magic, rest, ok := strings.Cut(string(data), "\n")
	if !ok || magic != markerMagic {
		return "", false, nil
	}
	line, _, ok := strings.Cut(rest, "\n")
	if !ok {
		return "", false, nil
	}
	name, ok := strings.CutPrefix(line, "target ")
	if !ok {
		return "", false, nil
	}
	return name, true, nil
}

// recoverableTarget resolves a recorded target name to a path directly inside
// dir, and returns the normalized name alongside it. Both come back because the
// caller needs them to agree: the path is resolved from the trimmed name, so
// asking the reconciler about the raw one would split the decision across two
// values, and a lookup that missed on the untrimmed name would read as a publish
// that never ran and replace the committed target with the tree it superseded.
//
// A name that is not a single path element could name anything on the
// filesystem, so it is refused rather than restored over. A name carrying the
// workspace prefix is refused for the same reason: it names another
// transaction, not an install, and restoring over one that is still in flight
// would destroy it.
func recoverableTarget(dir string, name string) (string, string, bool) {
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." || name != filepath.Base(name) {
		return "", "", false
	}
	if strings.HasPrefix(name, workspacePrefix) {
		return "", "", false
	}
	// A name no filesystem can resolve is not an install to recover. Left to reach
	// Lstat it answers with an error that is not not-exist, which would be reported
	// as an unresolved transaction on every pass forever, with no way for a caller
	// to clear it.
	if strings.ContainsRune(name, 0) || len(name) > 255 {
		return "", "", false
	}
	return name, filepath.Join(dir, name), true
}

func rollback(target string, backup string, hadPrevious bool, cause error) error {
	if !hadPrevious {
		// A first install has no backup to protect and recorded no target, so
		// recovery never looks here and deleting in place costs nothing.
		if err := os.RemoveAll(target); err != nil {
			return errors.Join(cause, fmt.Errorf("remove failed install: %w", err))
		}
		return cause
	}
	// Move the failed install aside before restoring rather than deleting it in
	// place. A process killed partway through an in-place delete would leave a
	// half removed tree at the target while the backup was still the only
	// complete copy, and recovery reads a target that is there as a committed
	// publish and retires the backup beside it. With the renames in this order
	// every instant of the rollback has either a whole tree at the target or
	// nothing there and the backup intact, which is exactly what recovery's two
	// branches are able to tell apart.
	failed := filepath.Join(filepath.Dir(backup), "failed")
	if err := os.Rename(target, failed); err != nil {
		// The move aside can fail too, and then the failed install stays live at
		// the target while the backup is still the only copy of what it replaced.
		// A caller with no reconciler reads a tree at the target as a committed
		// publish, so it would retire that backup, and one that has a reconciler
		// cannot classify a tree its own publish never recorded. Dropping the
		// marker leaves a workspace nothing can attribute, which recovery already
		// leaves alone, and the copy is still there to rescue by hand.
		_ = os.Remove(filepath.Join(filepath.Dir(backup), markerFileName))
		return errors.Join(cause, fmt.Errorf("remove failed install: %w", err))
	}
	if err := os.Rename(backup, target); err != nil {
		return errors.Join(cause, fmt.Errorf("restore previous install: %w", err))
	}
	_ = os.RemoveAll(failed)
	return cause
}

// cleanupWorkspace never removes a retained previous install. If rollback was
// unable to restore it (for example because Windows still has a target file
// open), preserving the workspace is safer than turning a recoverable error
// into data loss.
func cleanupWorkspace(workspace string) {
	if _, err := os.Stat(filepath.Join(workspace, "previous")); err == nil {
		return
	}
	_ = os.RemoveAll(workspace)
}

// WriteFileAtomically publishes data by renaming a complete sibling temporary
// file over path. The caller is responsible for any surrounding transaction
// lock.
func WriteFileAtomically(path string, data []byte, perm os.FileMode) error {
	temp, err := os.CreateTemp(filepath.Dir(path), ".zero-lockfile-")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer func() { _ = os.Remove(tempPath) }()
	if err := temp.Chmod(perm); err != nil {
		_ = temp.Close()
		return err
	}
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return replaceFile(tempPath, path)
}
