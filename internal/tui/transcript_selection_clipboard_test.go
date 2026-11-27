package tui

import (
	"encoding/base64"
	"errors"
	"io"
	"strings"
	"testing"
	"unicode/utf16"
)

func decodeUTF16LEBase64(t *testing.T, b64 string) string {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatalf("DecodeString failed: %v", err)
	}
	if len(raw)%2 != 0 {
		t.Fatalf("expected even byte length for UTF-16LE, got %d", len(raw))
	}
	u16 := make([]uint16, len(raw)/2)
	for i := range u16 {
		u16[i] = uint16(raw[i*2]) | uint16(raw[i*2+1])<<8
	}
	return string(utf16.Decode(u16))
}

func TestPowershellSetClipboardCommandRoundTrip(t *testing.T) {
	// Windows CreateProcessW limits the full command line to 32,767 characters.
	// 8,192 multi-byte Japanese runes would exceed that limit if embedded in
	// -EncodedCommand, so the command line must stay fixed-size and pass the
	// Base64 payload over stdin.
	const windowsMaxCommandLine = 32767

	samples := []string{
		"",
		"hello world",
		"文字化けチェック：日本語の表示テスト",
		"emoji 🚀 and quotes 'single' \"double\" $var `backtick`\nline2\tindented",
		strings.Repeat("日", 8192),
	}

	var fixedEncoded string
	for i, want := range samples {
		cmd := buildWSLClipboardCommand("powershell.exe", want)
		if len(cmd.Args) != 5 || cmd.Args[1] != "-NoProfile" || cmd.Args[2] != "-NonInteractive" || cmd.Args[3] != "-EncodedCommand" {
			t.Fatalf("unexpected PowerShell args: %q", cmd.Args)
		}
		if cmdLineLen := len(strings.Join(cmd.Args, " ")); cmdLineLen >= windowsMaxCommandLine {
			t.Fatalf("sample %d command line length %d exceeds Windows limit %d", i, cmdLineLen, windowsMaxCommandLine)
		}

		encoded := cmd.Args[4]
		if i == 0 {
			fixedEncoded = encoded
		} else if encoded != fixedEncoded {
			t.Fatalf("sample %d changed -EncodedCommand; want fixed-size command", i)
		}

		script := decodeUTF16LEBase64(t, encoded)
		if script != powershellSetClipboardScript {
			t.Fatalf("unexpected PowerShell script: %q", script)
		}

		stdinBytes, err := io.ReadAll(cmd.Stdin)
		if err != nil {
			t.Fatalf("reading cmd.Stdin failed: %v", err)
		}
		for _, b := range stdinBytes {
			if b > 0x7f {
				t.Fatalf("non-ASCII byte 0x%02x in stdin payload", b)
			}
		}

		gotBytes, err := base64.StdEncoding.DecodeString(string(stdinBytes))
		if err != nil {
			t.Fatalf("stdin base64 decode failed: %v", err)
		}
		if string(gotBytes) != want {
			t.Fatalf("round-trip mismatch (len=%d): got len=%d, want len=%d", len(want), len(gotBytes), len(want))
		}
	}
}

func TestWriteClipboardWSLPrefersPowershellAndFallsBack(t *testing.T) {
	origIsWSL := isWSLFunc
	origWriteWSL := writeWSLFunc
	origWriteAll := clipboardWriteAll
	t.Cleanup(func() {
		isWSLFunc = origIsWSL
		writeWSLFunc = origWriteWSL
		clipboardWriteAll = origWriteAll
	})

	t.Run("WSL success skips atotto/clipboard", func(t *testing.T) {
		isWSLFunc = func() bool { return true }
		var wslGot string
		writeWSLFunc = func(text string) error {
			wslGot = text
			return nil
		}
		clipboardWriteAll = func(string) error {
			t.Fatal("clipboardWriteAll should not be called when WSL clipboard succeeds")
			return nil
		}

		for _, sample := range []string{"日本語テスト", strings.Repeat("日", 8192)} {
			if err := writeClipboard(sample); err != nil {
				t.Fatalf("writeClipboard returned error: %v", err)
			}
			if wslGot != sample {
				t.Fatalf("WSL clipboard mismatch (len=%d): got len=%d", len(sample), len(wslGot))
			}
		}
	})

	t.Run("WSL failure falls back to atotto/clipboard", func(t *testing.T) {
		isWSLFunc = func() bool { return true }
		writeWSLFunc = func(string) error {
			return errors.New("powershell.exe not found")
		}
		var fallbackGot string
		clipboardWriteAll = func(text string) error {
			fallbackGot = text
			return nil
		}

		if err := writeClipboard("fallback text"); err != nil {
			t.Fatalf("writeClipboard returned error: %v", err)
		}
		if fallbackGot != "fallback text" {
			t.Fatalf("fallback clipboard got %q, want %q", fallbackGot, "fallback text")
		}
	})

	t.Run("non-WSL uses atotto/clipboard directly", func(t *testing.T) {
		isWSLFunc = func() bool { return false }
		writeWSLFunc = func(string) error {
			t.Fatal("writeWSLFunc should not be called outside WSL")
			return nil
		}
		var got string
		clipboardWriteAll = func(text string) error {
			got = text
			return nil
		}

		if err := writeClipboard("native text"); err != nil {
			t.Fatalf("writeClipboard returned error: %v", err)
		}
		if got != "native text" {
			t.Fatalf("native clipboard got %q, want %q", got, "native text")
		}
	})
}
