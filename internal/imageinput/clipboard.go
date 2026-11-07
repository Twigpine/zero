package imageinput

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/Twigpine/zero/internal/zeroruntime"
)

// ReadClipboardImage returns the raw image bytes and media type from the OS
// clipboard, or (nil, "", nil) when the clipboard has no image. Called when
// text clipboard is empty (the user pasted a screenshot). The media type is
// sniffed from the bytes, not trusted from the clipboard.
// ErrClipboardImageUnreadable reports that the clipboard holds an image the host
// has no way to extract. It is deliberately distinct from "no image on the
// clipboard", which is an ordinary no-op: this one is actionable, and silence is
// the wrong response to it.
var ErrClipboardImageUnreadable = errors.New(
	"the clipboard holds an image but no helper on this host can read it; on macOS install pngpaste (brew install pngpaste) or a Python with pyobjc")

func ReadClipboardImage() ([]byte, string, error) {
	data, err := readClipboardImageBytes()
	if err != nil {
		return nil, "", err
	}
	if data == nil {
		return nil, "", nil
	}
	// Sniff the media type from the bytes — don't trust the clipboard's claim.
	sniffLen := len(data)
	if sniffLen > 512 {
		sniffLen = 512
	}
	mediaType := zeroruntime.NormalizeImageMediaType(http.DetectContentType(data[:sniffLen]))
	if mediaType == "" {
		return nil, "", fmt.Errorf("clipboard image is not a supported type (allowed: png, jpeg, gif, webp)")
	}
	return data, mediaType, nil
}

// readClipboardImageBytes calls the platform-specific clipboard tool to extract
// image bytes. Returns (nil, nil) when no image is present.
func readClipboardImageBytes() ([]byte, error) {
	switch runtime.GOOS {
	case "windows":
		return readClipboardImageWindows()
	case "darwin":
		return readClipboardImageDarwin()
	case "linux":
		return readClipboardImageLinux()
	default:
		return nil, nil
	}
}

// readClipboardImageWindows uses PowerShell to check for and read a clipboard
// image. The image is saved as PNG to a temp file, read back, and the temp file
// deleted. Returns (nil, nil) when no image is on the clipboard.
func readClipboardImageWindows() ([]byte, error) {
	// Check if the clipboard contains an image.
	check := `Add-Type -AssemblyName System.Windows.Forms; Add-Type -AssemblyName System.Drawing; [System.Windows.Forms.Clipboard]::ContainsImage()`
	out, err := exec.Command("powershell", "-NoProfile", "-Command", check).Output()
	if err != nil {
		return nil, nil // clipboard not available, treat as no image
	}
	if strings.TrimSpace(string(out)) != "True" {
		return nil, nil
	}
	// Save the clipboard image as PNG to a temp file, then read the bytes.
	// PowerShell stdout can't reliably emit raw binary — $ms.ToArray() prints
	// a .NET byte array as space-separated text, not raw bytes. A temp file
	// is the correct binary-safe path.
	tmpFile, err := os.CreateTemp("", "zero-clipboard-*.png")
	if err != nil {
		return nil, nil
	}
	tmpPath := tmpFile.Name()
	tmpFile.Close()
	defer os.Remove(tmpPath)

	// Stay on -Command: -File is subject to script execution policy (default
	// Restricted on Windows clients) and Windows PowerShell decodes BOM-less
	// UTF-8 .ps1 files as ANSI. Doubling single quotes is all the escaping the
	// single-quoted PowerShell literal needs.
	escapedTmpPath := strings.ReplaceAll(tmpPath, "'", "''")
	script := `Add-Type -AssemblyName System.Windows.Forms; Add-Type -AssemblyName System.Drawing; $img = [System.Windows.Forms.Clipboard]::GetImage(); if ($img -ne $null) { $img.Save('` + escapedTmpPath + `', [System.Drawing.Imaging.ImageFormat]::Png) }`
	cmd := exec.Command("powershell", "-NoProfile", "-Command", script)
	if err := cmd.Run(); err != nil {
		return nil, nil
	}
	data, err := os.ReadFile(tmpPath)
	if err != nil || len(data) == 0 {
		return nil, nil
	}
	return data, nil
}

// readClipboardImageDarwin uses osascript to check for and read a clipboard
// image. Returns (nil, nil) when no image is present.
// darwinClipboardInfo reports what the pasteboard is currently offering, and
// darwinClipboardExtract pulls the PNG bytes out of it.
//
// Both are vars so the two-step logic below can be driven in a test. The step
// that matters, "the clipboard holds an image and nothing here can extract it",
// only happens when the first succeeds and the second fails, which cannot be
// arranged by stubbing the caller: a test that substitutes ReadClipboardImage
// asserts the plumbing around this function rather than the decision inside it,
// and stays green with that decision reverted.
var (
	darwinClipboardInfo = func() (string, error) {
		out, err := exec.Command("sh", "-c", `osascript -e 'clipboard info'`).Output()
		return string(out), err
	}
	darwinClipboardExtract = func() ([]byte, error) {
		// pngpaste if available, falling back to a Python one-liner. Neither
		// ships with macOS: pngpaste is Homebrew-only and Apple dropped the
		// bundled PyObjC, so a stock Mac fails here every time.
		cmd := exec.Command("sh", "-c", `pngpaste - 2>/dev/null || python3 -c "
import AppKit, sys
pb = AppKit.NSPasteboard.generalPasteboard()
data = pb.dataForType_(AppKit.NSPasteboardTypePNG)
if data:
    sys.stdout.buffer.write(data.bytes())
"`)
		var stdout bytes.Buffer
		cmd.Stdout = &stdout
		if err := cmd.Run(); err != nil {
			return nil, err
		}
		return stdout.Bytes(), nil
	}
)

// darwinClipboardOffersImage reports whether the pasteboard info line names an
// image class.
func darwinClipboardOffersImage(info string) bool {
	for _, class := range []string{"PNG", "JPEG", "TIFF", "GIF"} {
		if strings.Contains(info, class) {
			return true
		}
	}
	return false
}

func readClipboardImageDarwin() ([]byte, error) {
	info, err := darwinClipboardInfo()
	if err != nil {
		return nil, nil
	}
	if !darwinClipboardOffersImage(info) {
		return nil, nil
	}
	// Past this point the clipboard is KNOWN to hold an image, because the probe
	// above said so. A failure here is therefore not "no image", it is "there is
	// an image and nothing on this host can extract it". Reporting that as
	// absence is what left users staring at a paste that silently did nothing.
	data, err := darwinClipboardExtract()
	if err != nil {
		return nil, ErrClipboardImageUnreadable
	}
	if len(data) == 0 {
		return nil, ErrClipboardImageUnreadable
	}
	return data, nil
}

// readClipboardImageLinux tries wl-paste (Wayland) then xclip (X11) to read
// clipboard image bytes. Returns (nil, nil) when no image or no tool available.
//
// The MIME type comes from the clipboard (wl-paste --list-types / xclip
// TARGETS), so it is NEVER interpolated into a shell — every command runs via
// exec.Command(prog, args...) with the type passed as a discrete argument.
// (A hostile clipboard offerer could otherwise register a target like
// "image/png; rm -rf ~" that passes the "image/" prefix check.)
func readClipboardImageLinux() ([]byte, error) {
	// Try Wayland first.
	if types, err := runClipboardStdout("wl-paste", "--list-types"); err == nil {
		for _, t := range imageMIMETypes(types) {
			if data, err := runClipboardStdout("wl-paste", "--type", t); err == nil && len(data) > 0 {
				return data, nil
			}
		}
	}
	// Fall back to X11 xclip.
	if types, err := runClipboardStdout("xclip", "-selection", "clipboard", "-t", "TARGETS", "-o"); err == nil {
		for _, t := range imageMIMETypes(types) {
			if data, err := runClipboardStdout("xclip", "-selection", "clipboard", "-t", t, "-o"); err == nil && len(data) > 0 {
				return data, nil
			}
		}
	}
	return nil, nil
}

// runClipboardStdout runs a clipboard helper and returns only its stdout.
// Stderr is discarded (the helpers are noisy when the clipboard is empty or the
// tool is missing); a missing tool surfaces as the command error, treated as
// "no image" by the callers.
func runClipboardStdout(name string, args ...string) ([]byte, error) {
	cmd := exec.Command(name, args...)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	return stdout.Bytes(), nil
}

// imageMIMETypes extracts the "image/*" lines from a newline-separated type
// list, each safe to pass as a discrete argument (no shell).
func imageMIMETypes(list []byte) []string {
	var out []string
	for _, line := range strings.Split(string(list), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "image/") {
			out = append(out, line)
		}
	}
	return out
}
