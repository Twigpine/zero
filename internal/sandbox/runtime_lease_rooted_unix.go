//go:build !windows

package sandbox

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// acquireRuntimeLeaseRootedUnix creates and locks the lease for root without
// resolving an owned component by name.
//
// The lease is the FIRST thing setup writes, so it is the write a redirection has
// to be caught before, not after. It is a sibling of the runtime root rather than
// one of its owned components, which is why the alias guard never looked at it at
// all: it walks the components, and the lease is not one of them.
//
// Only the components above the leaf are created here. The leaf belongs to
// provisioning, which records it for rollback, and creating it here would mean
// two owners for one directory.
func acquireRuntimeLeaseRootedUnix(root string) (*sandboxRuntimeLease, []windowsCreatedRuntimeDir, error) {
	base, components, owned := windowsSandboxRuntimeOwnedTail(root)
	if !owned || len(components) == 0 {
		// Fail rather than fall back to the pathname walk: the walk is the defect.
		return nil, nil, fmt.Errorf("acquire sandbox runtime lease for %s: %w", root, errRuntimeTailNotOwned)
	}
	if runtimeLeasePreCreateBarrier != nil {
		runtimeLeasePreCreateBarrier()
	}
	// The base is the operator's, and may legitimately be a redirected or linked
	// temp location, so it is created and opened by name exactly as before.
	// Everything below it is Zero's and is addressed by descriptor.
	if err := os.MkdirAll(base, 0o700); err != nil {
		return nil, nil, fmt.Errorf("create sandbox runtime base: %w", err)
	}

	// The lease sits beside the leaf, so the deepest directory needed here is the
	// leaf's parent.
	tail := make([]string, 0, len(components)-1)
	current := base
	for _, component := range components[:len(components)-1] {
		current = filepath.Join(current, component)
		tail = append(tail, current)
	}

	// THE WHOLE ACQUISITION RETRIES, NOT JUST THE LOCK. A contender that waited
	// on the lease while cleanup held it exclusively can wake on an object cleanup
	// has since unlinked, and by then cleanup has compensated the tail this run
	// created as well. Re-taking the lock alone would lock a fresh file beside a
	// leaf that is gone, so a stale lock sends the caller back through tail
	// creation. The ledger accumulates across attempts: a directory this run made
	// on an earlier pass and cleanup did not remove is still this run's to
	// compensate.
	var created []windowsCreatedRuntimeDir
	for attempt := 1; ; attempt++ {
		made, parent, err := createRuntimeTailRetainingFD(base, tail)
		created = appendCreatedRuntimeDirs(created, made)
		if err != nil {
			return nil, created, err
		}
		handle, madeLease, err := acquireSharedRuntimeLeaseAtFD(parent, filepath.Base(sandboxRuntimeLeasePath(root)))
		_ = unix.Close(parent)
		if err == nil {
			return &sandboxRuntimeLease{handle: handle, root: root, createdFile: madeLease}, created, nil
		}
		if errors.Is(err, errRuntimeLeaseReplaced) && attempt < runtimeLeaseAcquireAttempts {
			continue
		}
		return nil, created, fmt.Errorf("acquire sandbox runtime lease: %w", err)
	}
}

// acquireSharedRuntimeLeaseAtFD opens the lease relative to a verified parent and
// takes the shared lock on it.
func acquireSharedRuntimeLeaseAtFD(parent int, name string) (runtimeLeaseHandle, bool, error) {
	file, created, err := openRuntimeLeaseAtFD(parent, name)
	if err != nil {
		return runtimeLeaseHandle{}, false, err
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_SH); err != nil {
		// The lock is the last step, and until now the created fact went out only
		// alongside a lease object. Failing here returned no object, so this
		// invocation's own lease file stayed on disk with nothing recording it,
		// and its directory could never be compensated afterwards.
		err = undoCreatedRuntimeLeaseAtFD(created, parent, name, err)
		_ = file.Close()
		return runtimeLeaseHandle{}, false, err
	}
	// THE LOCK IS ON AN INODE, THE COORDINATION IS ON A NAME. Flock blocks while
	// cleanup holds the exclusive lock, and cleanup unlinks the lease name before
	// it lets go. A waiter then acquires the old, unlinked inode and returns a
	// lease that coordinates with nobody: the next cleanup creates a fresh file at
	// the name, locks it unopposed, and reports the root unused while this holder
	// is still running in it. So the lock is only a lease once the locked object
	// is still what the name resolves to. Reported by @gnanam1990.
	current, err := runtimeLeaseIsCurrentAtFD(parent, name, int(file.Fd()))
	if err != nil {
		_ = file.Close()
		return runtimeLeaseHandle{}, false, err
	}
	if !current {
		// Not undone by name, whatever created says: the name is no longer this
		// object's, so an unlink there would take somebody else's lease.
		_ = file.Close()
		return runtimeLeaseHandle{}, false, errRuntimeLeaseReplaced
	}
	return runtimeLeaseHandle{file: file}, created, nil
}

// runtimeLeaseIsCurrentAtFD reports whether the locked lease is still the object
// the lease name resolves to under parent.
//
// Both answers come from the descriptor side: the locked file's own identity and
// the directory entry read relative to the retained parent, no-follow. A missing
// entry, a different inode at the name, or a locked inode with no links left all
// mean the coordination moved on while this call was waiting.
func runtimeLeaseIsCurrentAtFD(parent int, name string, fd int) (bool, error) {
	var locked, entry unix.Stat_t
	if err := unix.Fstat(fd, &locked); err != nil {
		return false, fmt.Errorf("inspect the locked sandbox runtime lease %s: %w", name, err)
	}
	if locked.Nlink == 0 {
		return false, nil
	}
	if err := unix.Fstatat(parent, name, &entry, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return false, nil
		}
		return false, fmt.Errorf("inspect the sandbox runtime lease entry %s: %w", name, err)
	}
	return locked.Dev == entry.Dev && locked.Ino == entry.Ino, nil
}

// undoCreatedRuntimeLeaseAtFD removes a lease file this call created, when a
// later step means no lease object will be returned to carry that fact.
//
// Relative to the verified parent descriptor, and only when this call is the one
// that created the file: a lease that was already there belongs to whoever is
// holding it.
func undoCreatedRuntimeLeaseAtFD(created bool, parent int, name string, cause error) error {
	if !created {
		return cause
	}
	if undoErr := unix.Unlinkat(parent, name, 0); undoErr != nil {
		return fmt.Errorf("%w; and the lease file this run created could not be removed: %w", cause, undoErr)
	}
	return cause
}

// openRuntimeLeaseAtFD opens or creates the lease under parent and proves it is
// an ordinary file.
//
// O_NOFOLLOW so a symlink planted at the lease name is an ELOOP rather than an
// open of its target, and a regular-file check besides, because O_NOFOLLOW says
// nothing about a fifo or a device a local user can also create there. Both
// holders have to end up on the same object or the lock protects nothing.
func openRuntimeLeaseAtFD(parent int, name string) (*os.File, bool, error) {
	const flags = unix.O_RDWR | unix.O_NOFOLLOW | unix.O_CLOEXEC
	// O_EXCL FIRST, so the create is what reports the create. A Stat beforehand
	// answers about a moment that has passed by the time the open runs, and
	// compensation would then delete a lease another process had just made.
	created := true
	fd, err := unix.Openat(parent, name, flags|unix.O_CREAT|unix.O_EXCL, 0o600)
	if errors.Is(err, unix.EEXIST) {
		created = false
		fd, err = unix.Openat(parent, name, flags, 0)
	}
	if err != nil {
		if errors.Is(err, unix.ELOOP) {
			return nil, false, fmt.Errorf("refusing to use the sandbox runtime lease at %s: it is a link, so the holders would lock different objects: %w", name, err)
		}
		return nil, false, err
	}
	// Every failure from here on has already created the file when created is
	// true, and returns no lease object to carry that fact, so each one undoes
	// its own creation rather than leaving an unrecorded artifact behind.
	if runtimeCreationFailure != nil {
		if injected := runtimeCreationFailure(name); injected != nil {
			injected = undoCreatedRuntimeLeaseAtFD(created, parent, name, injected)
			_ = unix.Close(fd)
			return nil, false, injected
		}
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		err = undoCreatedRuntimeLeaseAtFD(created, parent, name, fmt.Errorf("inspect the sandbox runtime lease %s: %w", name, err))
		_ = unix.Close(fd)
		return nil, false, err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		err := undoCreatedRuntimeLeaseAtFD(created, parent, name, fmt.Errorf("refusing to use the sandbox runtime lease at %s: it is not an ordinary file (mode %#o)", name, stat.Mode&unix.S_IFMT))
		_ = unix.Close(fd)
		return nil, false, err
	}
	if uid := unix.Getuid(); uid >= 0 && stat.Uid != uint32(uid) {
		err := undoCreatedRuntimeLeaseAtFD(created, parent, name, fmt.Errorf("refusing to use the sandbox runtime lease at %s: it is owned by uid %d, not %d", name, stat.Uid, uid))
		_ = unix.Close(fd)
		return nil, false, err
	}
	file := os.NewFile(uintptr(fd), name)
	if file == nil {
		err := undoCreatedRuntimeLeaseAtFD(created, parent, name, fmt.Errorf("wrap the sandbox runtime lease handle for %s", name))
		_ = unix.Close(fd)
		return nil, false, err
	}
	return file, created, nil
}

// tryAcquireExclusiveRuntimeLeaseRootedUnix is cleanup's acquisition, resolved
// the way acquisition resolves it.
//
// It opens and never creates the tree: a runtime root that is not there has no
// lease to take, and cleanup rebuilding it in order to lock it would be inventing
// the thing it is about to remove.
func tryAcquireExclusiveRuntimeLeaseRootedUnix(root string) (runtimeLeaseHandle, bool, error) {
	base, components, owned := windowsSandboxRuntimeOwnedTail(root)
	if !owned || len(components) == 0 {
		return runtimeLeaseHandle{}, false, fmt.Errorf("open the sandbox runtime lease parent for %s: %w", root, errRuntimeTailNotOwned)
	}
	parent, err := unix.Open(base, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return runtimeLeaseHandle{}, false, fmt.Errorf("open sandbox runtime base %s: %w", base, err)
	}
	path := base
	for _, name := range components[:len(components)-1] {
		path = filepath.Join(path, name)
		child, openErr := unix.Openat(parent, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if openErr != nil {
			_ = unix.Close(parent)
			return runtimeLeaseHandle{}, false, fmt.Errorf("open sandbox runtime component %s: %w", path, openErr)
		}
		if err := refuseForeignRuntimeDirectory(child, path); err != nil {
			_ = unix.Close(child)
			_ = unix.Close(parent)
			return runtimeLeaseHandle{}, false, err
		}
		_ = unix.Close(parent)
		parent = child
	}
	defer func() { _ = unix.Close(parent) }()

	name := filepath.Base(sandboxRuntimeLeasePath(root))
	for attempt := 1; ; attempt++ {
		file, created, err := openRuntimeLeaseAtFD(parent, name)
		if err != nil {
			return runtimeLeaseHandle{}, false, err
		}
		if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
			if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
				// Somebody holds it, so it is not this call's to remove, whatever the
				// open reported. A file this call created cannot reach here anyway.
				_ = file.Close()
				return runtimeLeaseHandle{}, true, nil
			}
			err = undoCreatedRuntimeLeaseAtFD(created, parent, name, err)
			_ = file.Close()
			return runtimeLeaseHandle{}, false, err
		}
		// The exclusive side has the same window in miniature: between its open
		// and its lock another cleanup can unlink and replace the name, and an
		// exclusive lock on the old inode would then remove a tree a holder of the
		// new one is entering. Same check, same answer: only the current object
		// counts, and a stale one is reopened rather than reasoned about.
		current, err := runtimeLeaseIsCurrentAtFD(parent, name, int(file.Fd()))
		if err != nil {
			_ = file.Close()
			return runtimeLeaseHandle{}, false, err
		}
		if !current {
			_ = file.Close()
			if attempt < runtimeLeaseAcquireAttempts {
				continue
			}
			return runtimeLeaseHandle{}, false, errRuntimeLeaseReplaced
		}
		return runtimeLeaseHandle{file: file}, false, nil
	}
}
