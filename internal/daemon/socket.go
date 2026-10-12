package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Gitlawb/zero/internal/privatedir"
)

// maxUnixSocketPath bounds the socket path to the smallest platform sun_path
// limit so we surface a clear error instead of a cryptic bind failure. macOS
// allows 104 bytes incl. the NUL (so 103 chars); Linux allows 108. 103 is the
// safe cross-platform ceiling.
const maxUnixSocketPath = 103

func secureCustomRuntimeParents(paths Paths) error {
	parents := []struct {
		name string
		path string
	}{
		{name: "socket", path: filepath.Dir(paths.Socket)},
		{name: "lock", path: filepath.Dir(paths.Lock)},
		{name: "status", path: filepath.Dir(paths.Status)},
	}
	seen := make(map[string]struct{}, len(parents))
	for _, parent := range parents {
		absolute, err := filepath.Abs(parent.path)
		if err != nil {
			return fmt.Errorf("daemon: resolve %s directory: %w", parent.name, err)
		}
		if _, ok := seen[absolute]; ok {
			continue
		}
		seen[absolute] = struct{}{}
		if err := os.MkdirAll(absolute, 0o700); err != nil {
			return fmt.Errorf("daemon: create %s directory: %w", parent.name, err)
		}
		root, err := os.OpenRoot(absolute)
		if err != nil {
			return fmt.Errorf("daemon: open custom %s directory: %w", parent.name, err)
		}
		validateErr := validateStatusRoot(root)
		closeErr := root.Close()
		if validateErr != nil {
			return fmt.Errorf("daemon: validate custom %s directory: %w", parent.name, validateErr)
		}
		if closeErr != nil {
			return fmt.Errorf("daemon: close custom %s directory: %w", parent.name, closeErr)
		}
	}
	return nil
}

// OpenRuntimeLog secures the default runtime root before acquiring the
// detached daemon's log descriptor. Custom endpoint layouts remain supported,
// but their existing parent permissions are caller-owned and left untouched.
func OpenRuntimeLog(paths Paths) (*os.File, string, error) {
	logPath := filepath.Join(filepath.Dir(paths.Socket), "daemon.log")
	root, isDefault, err := openDefaultRuntimeRoot(paths)
	if err != nil {
		return nil, logPath, err
	}
	if isDefault {
		defer root.Close()
		file, err := openRootAppendRegular(root, filepath.Base(logPath))
		if err != nil {
			return nil, logPath, fmt.Errorf("daemon: open runtime log: %w", err)
		}
		return file, logPath, nil
	}

	parent := filepath.Dir(logPath)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return nil, logPath, fmt.Errorf("daemon: create custom log directory: %w", err)
	}
	info, err := os.Lstat(logPath)
	if err == nil && !info.Mode().IsRegular() {
		return nil, logPath, fmt.Errorf("daemon: custom runtime log is not a regular file")
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, logPath, fmt.Errorf("daemon: inspect custom runtime log: %w", err)
	}
	file, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, logPath, fmt.Errorf("daemon: open custom runtime log: %w", err)
	}
	return file, logPath, nil
}

func openDefaultRuntimeRoot(paths Paths) (*os.Root, bool, error) {
	defaults, err := DefaultPaths()
	if err != nil {
		// A caller-supplied layout remains usable even when the process has no
		// resolvable home directory. Production default paths can only reach this
		// helper after DefaultPaths has already succeeded.
		return nil, false, nil
	}
	matches, err := samePaths(paths, defaults)
	if err != nil {
		return nil, false, err
	}
	if !matches {
		return nil, false, nil
	}
	root, err := privatedir.Open(filepath.Dir(defaults.Socket))
	if err != nil {
		return nil, false, fmt.Errorf("daemon: secure default runtime directory: %w", err)
	}
	return root, true, nil
}

func samePaths(left, right Paths) (bool, error) {
	pairs := [][2]string{
		{left.Socket, right.Socket},
		{left.Lock, right.Lock},
		{left.Status, right.Status},
	}
	for _, pair := range pairs {
		leftAbs, err := filepath.Abs(pair[0])
		if err != nil {
			return false, fmt.Errorf("daemon: resolve runtime path: %w", err)
		}
		rightAbs, err := filepath.Abs(pair[1])
		if err != nil {
			return false, fmt.Errorf("daemon: resolve default runtime path: %w", err)
		}
		if filepath.Clean(leftAbs) != filepath.Clean(rightAbs) {
			return false, nil
		}
	}
	return true, nil
}

func runtimeRootStillNamesPath(root *os.Root, path string) error {
	opened, err := root.Stat(".")
	if err != nil {
		return fmt.Errorf("inspect bound runtime directory: %w", err)
	}
	current, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect runtime directory entry: %w", err)
	}
	if current.Mode()&os.ModeSymlink != 0 || !os.SameFile(opened, current) {
		return fmt.Errorf("daemon: default runtime directory changed after it was secured")
	}
	return nil
}

func captureBoundSocketRoot(path string) (*os.Root, string, error) {
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return nil, "", fmt.Errorf("open bound socket directory: %w", err)
	}
	name := filepath.Base(path)
	info, err := root.Lstat(name)
	if err != nil {
		_ = root.Close()
		return nil, "", fmt.Errorf("inspect bound socket: %w", err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		_ = root.Close()
		return nil, "", fmt.Errorf("bound socket path is not a socket")
	}
	return root, name, nil
}

func openRootAppendRegular(root *os.Root, name string) (*os.File, error) {
	info, err := root.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY|os.O_APPEND, 0o600)
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("runtime log is not a regular file")
	}
	file, err := root.OpenFile(name, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	opened, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	if !opened.Mode().IsRegular() {
		_ = file.Close()
		return nil, fmt.Errorf("runtime log changed while opening")
	}
	return file, nil
}

// checkSocketPathLength rejects an over-long unix socket path before bind.
func checkSocketPathLength(socketPath string) error {
	if len(socketPath) > maxUnixSocketPath {
		return fmt.Errorf("daemon: socket path too long (%d > %d): %s", len(socketPath), maxUnixSocketPath, socketPath)
	}
	return nil
}
