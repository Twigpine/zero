package tui

import (
	"encoding/base64"
	"errors"
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
	samples := []string{
		"",
		"hello world",
		"文字化けチェック：日本語の表示テスト",
		"emoji 🚀 and quotes 'single' \"double\" $var `backtick`\nline2\tindented",
	}

	for _, want := range samples {
		encoded := powershellSetClipboardCommand(want)
		script := decodeUTF16LEBase64(t, encoded)

		const prefix = "Set-Clipboard -Value ([System.Text.Encoding]::UTF8.GetString([System.Convert]::FromBase64String('"
		const suffix = "')))"
		if !strings.HasPrefix(script, prefix) || !strings.HasSuffix(script, suffix) {
			t.Fatalf("unexpected PowerShell script format: %q", script)
		}
		payloadB64 := strings.TrimSuffix(strings.TrimPrefix(script, prefix), suffix)
		gotBytes, err := base64.StdEncoding.DecodeString(payloadB64)
		if err != nil {
			t.Fatalf("inner base64 decode failed: %v", err)
		}
		if string(gotBytes) != want {
			t.Fatalf("round-trip mismatch: got %q, want %q", string(gotBytes), want)
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

		if err := writeClipboard("日本語テスト"); err != nil {
			t.Fatalf("writeClipboard returned error: %v", err)
		}
		if wslGot != "日本語テスト" {
			t.Fatalf("WSL clipboard got %q, want %q", wslGot, "日本語テスト")
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
