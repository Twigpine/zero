package update

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// extractArchive extracts a .tar.gz or .zip release archive into destDir.
func extractArchive(archivePath string, destDir string) error {
	if strings.HasSuffix(archivePath, ".zip") {
		return extractZip(archivePath, destDir)
	}
	return extractTarGz(archivePath, destDir)
}

func extractTarGz(archivePath string, destDir string) error {
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return err
	}
	destRoot, err := os.OpenRoot(destDir)
	if err != nil {
		return err
	}
	defer func() {
		_ = destRoot.Close()
	}()

	file, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer func() {
		_ = file.Close()
	}()
	gzipReader, err := gzip.NewReader(file)
	if err != nil {
		return err
	}
	defer func() {
		_ = gzipReader.Close()
	}()
	tarReader := tar.NewReader(gzipReader)
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		cleanName, err := cleanEntryPath(header.Name)
		if err != nil {
			return err
		}
		if cleanName == "." {
			continue
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if err := destRoot.MkdirAll(cleanName, 0o755); err != nil {
				return fmt.Errorf("archive directory entry %s escapes destination: %w", header.Name, err)
			}
		case tar.TypeSymlink:
			parent := filepath.Dir(cleanName)
			if parent != "." {
				if err := destRoot.MkdirAll(parent, 0o755); err != nil {
					return fmt.Errorf("archive symlink parent %s escapes destination: %w", parent, err)
				}
			}
			if err := validateSymlinkTarget(destRoot, parent, header.Linkname); err != nil {
				return fmt.Errorf("archive symlink target escapes destination: %s -> %s: %w", header.Name, header.Linkname, err)
			}
			_ = destRoot.Remove(cleanName)
			if err := destRoot.Symlink(header.Linkname, cleanName); err != nil {
				return err
			}
		case tar.TypeReg:
			parent := filepath.Dir(cleanName)
			if parent != "." {
				if err := destRoot.MkdirAll(parent, 0o755); err != nil {
					return fmt.Errorf("archive file parent %s escapes destination: %w", parent, err)
				}
			}
			if err := writeExtractedFile(destRoot, cleanName, tarReader, fs.FileMode(header.Mode)); err != nil {
				return err
			}
		default:
			// Release archives only ever contain regular files and directories;
			// reject anything else (symlinks, devices) rather than silently skip it.
			return fmt.Errorf("unsupported archive entry type for %s", header.Name)
		}
	}
}

func extractZip(archivePath string, destDir string) error {
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return err
	}
	destRoot, err := os.OpenRoot(destDir)
	if err != nil {
		return err
	}
	defer func() {
		_ = destRoot.Close()
	}()

	reader, err := zip.OpenReader(archivePath)
	if err != nil {
		return err
	}
	defer func() {
		_ = reader.Close()
	}()
	for _, entry := range reader.File {
		cleanName, err := cleanEntryPath(entry.Name)
		if err != nil {
			return err
		}
		if cleanName == "." {
			continue
		}
		if entry.FileInfo().IsDir() {
			if err := destRoot.MkdirAll(cleanName, 0o755); err != nil {
				return fmt.Errorf("archive directory entry %s escapes destination: %w", entry.Name, err)
			}
			continue
		}
		// Release archives only ever contain regular files and directories;
		// reject anything else (symlinks, devices) rather than silently write
		// the link-target string (or other special content) out as an
		// ordinary file. Unlike extractTarGz, zip symlinks stay rejected: the
		// .zip path is the Windows release archive format, where
		// filepath.IsAbs does not reliably reject a slash-rooted target like
		// "/some/other/path" (Windows absolute paths need a drive letter or
		// UNC prefix), and there is no current use case for symlinks in a
		// Windows archive. The npm shim symlinks this feature exists for are
		// packaged in the Unix/macOS .tar.gz archives, handled above.
		if !entry.Mode().IsRegular() {
			return fmt.Errorf("unsupported archive entry type for %s", entry.Name)
		}
		parent := filepath.Dir(cleanName)
		if parent != "." {
			if err := destRoot.MkdirAll(parent, 0o755); err != nil {
				return fmt.Errorf("archive file parent %s escapes destination: %w", parent, err)
			}
		}
		if err := func() error {
			entryReader, err := entry.Open()
			if err != nil {
				return err
			}
			defer func() {
				_ = entryReader.Close()
			}()
			return writeExtractedFile(destRoot, cleanName, entryReader, entry.Mode())
		}(); err != nil {
			return err
		}
	}
	return nil
}

func writeExtractedFile(destRoot *os.Root, name string, source io.Reader, mode fs.FileMode) error {
	perm := mode.Perm()
	if perm == 0 {
		perm = 0o644
	}
	out, err := destRoot.OpenFile(name, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, source)
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

// cleanEntryPath resolves an archive entry name, rejecting absolute paths or
// entries that would escape the destination via "..".
func cleanEntryPath(name string) (string, error) {
	cleanName := filepath.Clean(strings.ReplaceAll(name, "\\", "/"))
	if cleanName == "." {
		return ".", nil
	}
	if filepath.IsAbs(cleanName) || strings.HasPrefix(name, "/") || strings.HasPrefix(name, "\\") ||
		cleanName == ".." || strings.HasPrefix(cleanName, ".."+string(os.PathSeparator)) || strings.HasPrefix(cleanName, "../") ||
		filepath.VolumeName(cleanName) != "" || strings.Contains(cleanName, ":") {
		return "", fmt.Errorf("archive entry escapes destination: %s", name)
	}
	return cleanName, nil
}

func validateSymlinkTarget(root *os.Root, parentRel, linkTarget string) error {
	if filepath.IsAbs(linkTarget) || strings.HasPrefix(linkTarget, "/") || strings.HasPrefix(linkTarget, "\\") ||
		filepath.VolumeName(linkTarget) != "" || strings.Contains(linkTarget, ":") {
		return fmt.Errorf("archive symlink has absolute or invalid target: %s", linkTarget)
	}
	base := parentRel
	if base == "" {
		base = "."
	}
	resolvedBase, err := followUnderRoot(root, base, 0)
	if err != nil {
		return err
	}
	_, err = walkUnderRoot(root, resolvedBase, linkTarget, 0)
	return err
}

const maxRootSymlinks = 8

func followUnderRoot(root *os.Root, rel string, depth int) (string, error) {
	if rel == "" || rel == "." {
		return ".", nil
	}
	return walkUnderRoot(root, ".", rel, depth)
}

func walkUnderRoot(root *os.Root, base, linkTarget string, depth int) (string, error) {
	if depth > maxRootSymlinks {
		return "", fmt.Errorf("archive symlink nest exceeds limit")
	}
	current := base
	if current == "" {
		current = "."
	}
	for _, part := range strings.Split(strings.ReplaceAll(linkTarget, "\\", "/"), "/") {
		if part == "" || part == "." {
			continue
		}
		if part == ".." {
			if current == "." {
				return "", fmt.Errorf("archive symlink target escapes destination")
			}
			current = filepath.Dir(current)
			if current == "" {
				current = "."
			}
			continue
		}
		next := part
		if current != "." {
			next = filepath.Join(current, part)
		}
		info, err := root.Lstat(next)
		if err != nil {
			if os.IsNotExist(err) {
				current = next
				continue
			}
			return "", err
		}
		if info.Mode()&os.ModeSymlink == 0 {
			current = next
			continue
		}
		tgt, err := root.Readlink(next)
		if err != nil {
			return "", err
		}
		if filepath.IsAbs(tgt) || strings.HasPrefix(tgt, "/") || strings.HasPrefix(tgt, "\\") ||
			filepath.VolumeName(tgt) != "" || strings.Contains(tgt, ":") {
			return "", fmt.Errorf("archive symlink %s has absolute target: %s", next, tgt)
		}
		followed, err := walkUnderRoot(root, current, tgt, depth+1)
		if err != nil {
			return "", err
		}
		current = followed
	}
	return current, nil
}

// findByBasename recursively searches root for the first regular file whose
// basename matches name, mirroring scripts/postinstall.mjs's lookup so
// helper binaries nested under archive subdirectories (e.g. helpers/) are
// still found.
func findByBasename(root string, name string) (string, error) {
	destRoot, err := os.OpenRoot(root)
	if err != nil {
		return "", err
	}
	defer destRoot.Close()
	var found string
	err = fs.WalkDir(destRoot.FS(), ".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if found != "" {
			return fs.SkipAll
		}
		if entry.Type().IsRegular() && entry.Name() == name {
			if path == "." {
				found = filepath.Join(root, name)
			} else {
				found = filepath.Join(root, filepath.FromSlash(path))
			}
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return found, nil
}
