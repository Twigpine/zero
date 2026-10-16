// Package imageinput is the single shared loader for local image files used by
// every input surface (CLI exec --image, TUI /image). It reads a file, sniffs +
// normalizes its media type against the allow-list, enforces the per-image size
// cap, and returns a raw-bytes ImageBlock. Keeping it here means the CLI and TUI
// never duplicate the read/sniff/normalize/cap logic.
package imageinput

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"

	"github.com/Gitlawb/zero/internal/sandbox"
	"github.com/Gitlawb/zero/internal/zeroruntime"
)

// MaxImageBytes is the per-image decoded-size cap (10 MiB). Bytes above this are
// rejected at every input boundary so an unbounded request body never reaches a
// provider.
const MaxImageBytes = 10 << 20

// LoadFile reads the image at path (resolved against workspaceRoot when
// relative), validates its type and size, and returns a raw-bytes ImageBlock.
// Errors are plain (callers wrap them into surface-specific usage/notice text).
func LoadFile(path string, workspaceRoot string) (zeroruntime.ImageBlock, error) {
	resolved := path
	if !filepath.IsAbs(resolved) {
		resolved = filepath.Join(workspaceRoot, resolved)
	}

	// Reject oversized files via Stat BEFORE reading them into memory, so a huge
	// file never allocates a multi-gigabyte buffer just to be discarded by the
	// post-read cap. A missing file surfaces the same "not found" notice as a
	// failed read.
	info, err := os.Stat(resolved)
	if err != nil {
		return zeroruntime.ImageBlock{}, fmt.Errorf("image file not found: %s", path)
	}
	// Reject non-regular files (directories, FIFOs, devices) up front. os.Stat
	// follows symlinks, so a symlink to a regular file still passes. This guards
	// against os.Open blocking forever on a writerless FIFO (an --image/ /image
	// path pointing at a named pipe would otherwise hang the process/UI).
	if !info.Mode().IsRegular() {
		return zeroruntime.ImageBlock{}, fmt.Errorf("image file must be a regular file: %s", path)
	}
	if info.Size() > MaxImageBytes {
		return zeroruntime.ImageBlock{}, fmt.Errorf("image %s is larger than the 10 MiB limit", path)
	}

	// Bounded read: the os.Stat above is only a fast-path hint (a non-regular
	// file reports a misleading size, and the file can grow between Stat and the
	// read). A LimitReader of MaxImageBytes+1 is the real bound — at most one byte
	// past the cap is ever buffered, so an oversized or unbounded source (e.g. a
	// FIFO) can never allocate a multi-gigabyte buffer just to be discarded.
	file, err := os.Open(resolved)
	if err != nil {
		// Keep the real cause: a permission or I/O failure reported as "not
		// found" sends users hunting for a file that exists.
		return zeroruntime.ImageBlock{}, fmt.Errorf("cannot open image %s: %w", path, err)
	}
	defer file.Close()

	return LoadOpenFile(file, path, workspaceRoot)
}

// LoadOpenFile validates and consumes the supplied handle without reopening its
// pathname. The caller owns the handle and any workspace confinement; LoadFile
// intentionally permits user-selected images outside the workspace.
func LoadOpenFile(file *os.File, path, workspaceRoot string) (zeroruntime.ImageBlock, error) {
	info, err := file.Stat()
	if err != nil {
		return zeroruntime.ImageBlock{}, fmt.Errorf("cannot stat image %s: %w", path, err)
	}
	exclusions := sandbox.ProtectedCredentialExclusions(workspaceRoot)
	if exclusions.FileHandleExcluded(path, file, info) {
		return zeroruntime.ImageBlock{}, fmt.Errorf("%s holds the remote bridge token and is never readable", path)
	}
	if !info.Mode().IsRegular() {
		return zeroruntime.ImageBlock{}, fmt.Errorf("image file must be a regular file: %s", path)
	}
	if info.Size() > MaxImageBytes {
		return zeroruntime.ImageBlock{}, fmt.Errorf("image %s is larger than the 10 MiB limit", path)
	}

	data, err := io.ReadAll(io.LimitReader(file, MaxImageBytes+1))
	if err != nil {
		return zeroruntime.ImageBlock{}, fmt.Errorf("cannot read image %s: %w", path, err)
	}

	// The LimitReader yields at most MaxImageBytes+1 bytes; more than the cap means
	// the source was oversized (caught here regardless of any stat/read race).
	if len(data) > MaxImageBytes {
		return zeroruntime.ImageBlock{}, fmt.Errorf("image %s is larger than the 10 MiB limit", path)
	}

	sniffLen := len(data)
	if sniffLen > 512 {
		sniffLen = 512
	}
	mediaType := zeroruntime.NormalizeImageMediaType(http.DetectContentType(data[:sniffLen]))
	if mediaType == "" {
		return zeroruntime.ImageBlock{}, fmt.Errorf("unsupported image type for %s (allowed: png, jpeg, gif, webp)", path)
	}

	return zeroruntime.ImageBlock{MediaType: mediaType, Data: data}, nil
}
