package terminalpet

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"os"
	"strings"
	"testing"
)

func TestDetectImageSupportUsesKittyGraphicsInGhostty(t *testing.T) {
	env := map[string]string{
		"TERM":         "xterm-ghostty",
		"TERM_PROGRAM": "ghostty",
	}
	support := DetectImageSupport(func(name string) string { return env[name] })
	if support.Protocol != ImageProtocolKitty {
		t.Fatalf("Ghostty protocol = %v, want Kitty", support.Protocol)
	}
}

func TestDetectImageSupportRejectsTmuxBeforeTerminalProtocol(t *testing.T) {
	env := map[string]string{"TMUX": "/tmp/tmux-1000/default,1,0", "TERM": "xterm-ghostty"}
	support := DetectImageSupport(func(name string) string { return env[name] })
	if support.Supported() || !strings.Contains(strings.ToLower(support.Reason), "tmux") {
		t.Fatalf("tmux support = %#v, want unsupported tmux reason", support)
	}
}

func TestDetectImageSupportUsesSixelInWindowsTerminal(t *testing.T) {
	env := map[string]string{"WT_SESSION": "session-id", "TERM": "xterm-256color"}
	support := DetectImageSupport(func(name string) string { return env[name] })
	if support.Protocol != ImageProtocolSixel {
		t.Fatalf("Windows Terminal protocol = %v, want Sixel", support.Protocol)
	}
}

func TestDetectImageSupportUsesLocalKittyFilesInNewIterm(t *testing.T) {
	env := map[string]string{"TERM_PROGRAM": "iTerm.app", "TERM_PROGRAM_VERSION": "3.6.1"}
	support := DetectImageSupport(func(name string) string { return env[name] })
	if support.Protocol != ImageProtocolKittyLocalFile {
		t.Fatalf("iTerm2 protocol = %v, want Kitty local file", support.Protocol)
	}
	env["TERM_PROGRAM_VERSION"] = "3.5.9"
	support = DetectImageSupport(func(name string) string { return env[name] })
	if support.Supported() || !strings.Contains(support.Reason, "3.6") {
		t.Fatalf("old iTerm2 support = %#v, want version error", support)
	}
}

func TestImageRendererTransmitsPNGAndDeletesItWhenCleared(t *testing.T) {
	frame := image.NewNRGBA(image.Rect(0, 0, 2, 2))
	frame.SetNRGBA(0, 0, color.NRGBA{R: 240, G: 80, B: 120, A: 255})
	animation, err := ThumbnailAnimation(frame)
	if err != nil {
		t.Fatal(err)
	}
	renderer := NewImageRenderer(ImageSupport{Protocol: ImageProtocolKitty})
	renderer.Set(&ImageDraw{
		ID:        0xC0DF,
		Animation: animation,
		State:     Idle,
		Columns:   11,
		Rows:      6,
		X:         20,
		Y:         10,
	})

	var output bytes.Buffer
	if err := renderer.Render(&output); err != nil {
		t.Fatal(err)
	}
	got := output.String()
	for _, want := range []string{"\x1b[11;21H", "_Ga=T,t=d,f=100,c=11,r=6,q=2,C=1,i=49375,p=49375", "iVBOR"} {
		if !strings.Contains(got, want) {
			t.Fatalf("Kitty output does not contain %q: %q", want, got)
		}
	}

	output.Reset()
	renderer.Set(nil)
	if err := renderer.Render(&output); err != nil {
		t.Fatal(err)
	}
	if got := output.String(); !strings.Contains(got, "_Ga=d,d=I,i=49375,q=2") {
		t.Fatalf("clear output did not delete image: %q", got)
	}
}

func TestImageRendererMovesExistingKittyPlacementWithoutRetransmitting(t *testing.T) {
	frame := image.NewNRGBA(image.Rect(0, 0, 2, 2))
	frame.SetNRGBA(0, 0, color.NRGBA{R: 240, A: 255})
	animation, err := ThumbnailAnimation(frame)
	if err != nil {
		t.Fatal(err)
	}
	renderer := NewImageRenderer(ImageSupport{Protocol: ImageProtocolKitty})
	draw := &ImageDraw{ID: 31, Animation: animation, State: Idle, Columns: 4, Rows: 3, X: 2, Y: 3}
	renderer.Set(draw)
	var output bytes.Buffer
	if err := renderer.Render(&output); err != nil {
		t.Fatal(err)
	}

	output.Reset()
	draw.X, draw.Y = 18, 9
	draw.OffsetX, draw.OffsetY = 3, 7
	renderer.Set(draw)
	if err := renderer.Render(&output); err != nil {
		t.Fatal(err)
	}
	got := output.String()
	if !strings.Contains(got, "\x1b[10;19H") || !strings.Contains(got, "_Ga=p,i=31,p=31,c=4,r=3,X=3,Y=7,q=2,C=1;") {
		t.Fatalf("placement-only move missing: %q", got)
	}
	if strings.Contains(got, "a=T") || strings.Contains(got, "iVBOR") || strings.Contains(got, "a=d") {
		t.Fatalf("placement-only move retransmitted or deleted image data: %q", got)
	}
}

func TestImageRendererEmitsSixelForSixelTerminal(t *testing.T) {
	frame := image.NewNRGBA(image.Rect(0, 0, 2, 2))
	frame.SetNRGBA(0, 0, color.NRGBA{R: 255, A: 255})
	animation, err := ThumbnailAnimation(frame)
	if err != nil {
		t.Fatal(err)
	}
	renderer := NewImageRenderer(ImageSupport{Protocol: ImageProtocolSixel})
	renderer.Set(&ImageDraw{ID: 9, Animation: animation, State: Idle, Columns: 4, Rows: 3, HeightPixels: 12})
	var output bytes.Buffer
	if err := renderer.Render(&output); err != nil {
		t.Fatal(err)
	}
	got := output.String()
	if !strings.Contains(got, "\x1bP9;1;0q") || !strings.HasSuffix(got, "\x1b\\\x1b[u") {
		t.Fatalf("Sixel output is missing its image payload: %q", got)
	}
	if strings.Contains(got, "_Ga=T") {
		t.Fatalf("Sixel renderer emitted Kitty graphics: %q", got)
	}
}

func TestImageRendererUsesLocalPNGForIterm(t *testing.T) {
	frame := image.NewNRGBA(image.Rect(0, 0, 1, 1))
	frame.SetNRGBA(0, 0, color.NRGBA{G: 255, A: 255})
	animation, err := ThumbnailAnimation(frame)
	if err != nil {
		t.Fatal(err)
	}
	renderer := NewImageRendererWithCache(ImageSupport{Protocol: ImageProtocolKittyLocalFile}, t.TempDir())
	renderer.Set(&ImageDraw{ID: 11, Animation: animation, State: Idle, Columns: 4, Rows: 3})
	var output bytes.Buffer
	if err := renderer.Render(&output); err != nil {
		t.Fatal(err)
	}
	got := output.String()
	prefix := "_Ga=T,t=f,f=100,c=4,r=3,q=2,C=1,i=11,p=11;"
	start := strings.Index(got, prefix)
	if start < 0 {
		t.Fatalf("local-file command missing: %q", got)
	}
	payload := got[start+len(prefix):]
	payload = strings.SplitN(payload, "\x1b\\", 2)[0]
	pathBytes, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		t.Fatalf("decode local-file payload: %v", err)
	}
	if _, err := os.Stat(string(pathBytes)); err != nil {
		t.Fatalf("cached PNG path is unavailable: %v", err)
	}
}
