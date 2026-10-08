package tui

import (
	"errors"
	"fmt"
	"image"
	"image/color"
	"io"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/Gitlawb/zero/internal/terminalpet"
	"github.com/charmbracelet/x/ansi"
)

type shortTerminalOutput struct {
	limit int
	err   error
}

func (o *shortTerminalOutput) Read([]byte) (int, error) { return 0, io.EOF }
func (o *shortTerminalOutput) Close() error             { return nil }
func (o *shortTerminalOutput) Fd() uintptr              { return 0 }
func (o *shortTerminalOutput) Write(value []byte) (int, error) {
	return min(o.limit, len(value)), o.err
}

func TestPetImageOutputAppendsScheduledImageAfterFrame(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "pet-output-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	frame := image.NewNRGBA(image.Rect(0, 0, 1, 1))
	frame.SetNRGBA(0, 0, color.NRGBA{R: 255, A: 255})
	animation, err := terminalpet.ThumbnailAnimation(frame)
	if err != nil {
		t.Fatal(err)
	}
	renderer := terminalpet.NewImageRenderer(terminalpet.ImageSupport{Protocol: terminalpet.ImageProtocolKitty})
	renderer.Set(&terminalpet.ImageDraw{ID: 7, Animation: animation, State: terminalpet.Idle, Columns: 4, Rows: 3})
	output := newPetImageOutput(file, renderer)
	if _, err := output.Write([]byte("bubbletea-frame")); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(file.Name())
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	if !strings.HasPrefix(got, terminalSyncStart+"bubbletea-frame") || !strings.Contains(got, "_Ga=T,t=d,f=100,c=4,r=3") {
		t.Fatalf("output did not append image after frame: %q", got)
	}
	syncStart := strings.Index(got, "\x1b[?2026h")
	frameAt := strings.Index(got, "bubbletea-frame")
	imageAt := strings.Index(got, "_Ga=T,t=d,f=100,c=4,r=3")
	syncEnd := strings.LastIndex(got, "\x1b[?2026l")
	if syncStart < 0 || frameAt < syncStart || imageAt < frameAt || syncEnd < imageAt {
		t.Fatalf("frame and image update were not synchronized together: %q", got)
	}
}

func TestPetImageOutputKeepsImageInsideBubbleTeaSynchronizedFrame(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "pet-output-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	frame := image.NewNRGBA(image.Rect(0, 0, 1, 1))
	animation, err := terminalpet.ThumbnailAnimation(frame)
	if err != nil {
		t.Fatal(err)
	}
	renderer := terminalpet.NewImageRenderer(terminalpet.ImageSupport{Protocol: terminalpet.ImageProtocolKitty})
	renderer.Set(&terminalpet.ImageDraw{ID: 7, Animation: animation, State: terminalpet.Idle, Columns: 4, Rows: 3})
	output := newPetImageOutput(file, renderer)
	bubbleTeaFrame := terminalSyncStart + "streamed-text-frame" + terminalSyncEnd
	if _, err := output.Write([]byte(bubbleTeaFrame)); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(file.Name())
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	if count := strings.Count(got, terminalSyncStart); count != 1 {
		t.Fatalf("got %d synchronized-output starts, want 1: %q", count, got)
	}
	if count := strings.Count(got, terminalSyncEnd); count != 1 {
		t.Fatalf("got %d synchronized-output ends, want 1: %q", count, got)
	}
	frameAt := strings.Index(got, "streamed-text-frame")
	imageAt := strings.Index(got, "_Ga=T,t=d,f=100,c=4,r=3")
	syncEnd := strings.Index(got, terminalSyncEnd)
	if frameAt < 0 || imageAt < frameAt || syncEnd < imageAt {
		t.Fatalf("pet update was presented outside the Bubble Tea frame: %q", got)
	}
}

func TestPetImageOutputMapsPartialSynchronizedWriteToOriginalBytes(t *testing.T) {
	frame := image.NewNRGBA(image.Rect(0, 0, 1, 1))
	animation, err := terminalpet.ThumbnailAnimation(frame)
	if err != nil {
		t.Fatal(err)
	}
	renderer := terminalpet.NewImageRenderer(terminalpet.ImageSupport{Protocol: terminalpet.ImageProtocolKitty})
	renderer.Set(&terminalpet.ImageDraw{ID: 7, Animation: animation, State: terminalpet.Idle, Columns: 4, Rows: 3})
	bubbleTeaFrame := terminalSyncStart + "streamed-text-frame" + terminalSyncEnd
	prefixLength := strings.Index(bubbleTeaFrame, terminalSyncEnd)
	wantErr := errors.New("partial terminal write")
	sink := &shortTerminalOutput{limit: prefixLength + 3, err: wantErr}

	written, err := newPetImageOutput(sink, renderer).Write([]byte(bubbleTeaFrame))
	if !errors.Is(err, wantErr) {
		t.Fatalf("Write error = %v, want %v", err, wantErr)
	}
	if written != prefixLength {
		t.Fatalf("Write reported %d original bytes, want %d", written, prefixLength)
	}
}

func TestPetImageOutputReportsNilErrorShortWrite(t *testing.T) {
	frame := image.NewNRGBA(image.Rect(0, 0, 1, 1))
	animation, err := terminalpet.ThumbnailAnimation(frame)
	if err != nil {
		t.Fatal(err)
	}
	renderer := terminalpet.NewImageRenderer(terminalpet.ImageSupport{Protocol: terminalpet.ImageProtocolKitty})
	renderer.Set(&terminalpet.ImageDraw{ID: 7, Animation: animation, State: terminalpet.Idle, Columns: 4, Rows: 3})
	bubbleTeaFrame := terminalSyncStart + "streamed-text-frame" + terminalSyncEnd
	prefixLength := strings.Index(bubbleTeaFrame, terminalSyncEnd)
	sink := &shortTerminalOutput{limit: prefixLength + 3}

	written, err := newPetImageOutput(sink, renderer).Write([]byte(bubbleTeaFrame))
	if !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("Write error = %v, want %v", err, io.ErrShortWrite)
	}
	if written != prefixLength {
		t.Fatalf("Write reported %d original bytes, want %d", written, prefixLength)
	}
}

func TestPetImageOutputKeepsDraggedPlacementInsideStreamingFrame(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "pet-output-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	frame := image.NewNRGBA(image.Rect(0, 0, 1, 1))
	animation, err := terminalpet.ThumbnailAnimation(frame)
	if err != nil {
		t.Fatal(err)
	}
	renderer := terminalpet.NewImageRenderer(terminalpet.ImageSupport{Protocol: terminalpet.ImageProtocolKitty})
	draw := terminalpet.ImageDraw{ID: 7, Animation: animation, State: terminalpet.Idle, X: 2, Y: 3, Columns: 4, Rows: 3}
	renderer.Set(&draw)
	output := newPetImageOutput(file, renderer)
	if _, err := output.Write([]byte(terminalSyncStart + "initial-frame" + terminalSyncEnd)); err != nil {
		t.Fatal(err)
	}
	start, err := file.Seek(0, io.SeekCurrent)
	if err != nil {
		t.Fatal(err)
	}
	draw.X = 8
	draw.Y = 6
	renderer.Set(&draw)
	if _, err := output.Write([]byte(terminalSyncStart + "streamed-delta" + terminalSyncEnd)); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(file.Name())
	if err != nil {
		t.Fatal(err)
	}
	got := string(data[start:])
	if count := strings.Count(got, terminalSyncStart); count != 1 {
		t.Fatalf("got %d synchronized-output starts, want 1: %q", count, got)
	}
	if count := strings.Count(got, terminalSyncEnd); count != 1 {
		t.Fatalf("got %d synchronized-output ends, want 1: %q", count, got)
	}
	textAt := strings.Index(got, "streamed-delta")
	moveAt := strings.Index(got, "\x1b[7;9H\x1b_Ga=p")
	syncEnd := strings.Index(got, terminalSyncEnd)
	if textAt < 0 || moveAt < textAt || syncEnd < moveAt {
		t.Fatalf("dragged pet placement was presented outside the streaming frame: %q", got)
	}
	if strings.Contains(got, "_Ga=T") {
		t.Fatalf("dragging retransmitted image data instead of moving its placement: %q", got)
	}
}

func TestPetImageOutputFlushesMovedPlacementWithoutTextChange(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "pet-output-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	frame := image.NewNRGBA(image.Rect(0, 0, 1, 1))
	animation, err := terminalpet.ThumbnailAnimation(frame)
	if err != nil {
		t.Fatal(err)
	}
	renderer := terminalpet.NewImageRenderer(terminalpet.ImageSupport{Protocol: terminalpet.ImageProtocolKitty})
	draw := terminalpet.ImageDraw{ID: 7, Animation: animation, State: terminalpet.Idle, X: 2, Y: 3, Columns: 4, Rows: 3}
	renderer.Set(&draw)
	output := newPetImageOutput(file, renderer)
	if _, err := output.Write([]byte(terminalSyncStart + "initial-frame" + terminalSyncEnd)); err != nil {
		t.Fatal(err)
	}
	start, err := file.Seek(0, io.SeekCurrent)
	if err != nil {
		t.Fatal(err)
	}
	draw.X = 8
	draw.Y = 6
	renderer.Set(&draw)
	if _, err := output.Write([]byte(terminalSyncStart + terminalSyncEnd)); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(file.Name())
	if err != nil {
		t.Fatal(err)
	}
	got := string(data[start:])
	moveAt := strings.Index(got, "\x1b[7;9H\x1b_Ga=p")
	syncEnd := strings.Index(got, terminalSyncEnd)
	if moveAt < 0 || syncEnd < moveAt {
		t.Fatalf("idle drag flush did not place the pet inside its empty frame: %q", got)
	}
	if strings.Contains(got, "_Ga=T") {
		t.Fatalf("idle drag flush retransmitted image data: %q", got)
	}
}

func TestPetImageOutputRetransmitsAfterFullScreenClear(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "pet-output-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	frame := image.NewNRGBA(image.Rect(0, 0, 1, 1))
	animation, err := terminalpet.ThumbnailAnimation(frame)
	if err != nil {
		t.Fatal(err)
	}
	renderer := terminalpet.NewImageRenderer(terminalpet.ImageSupport{Protocol: terminalpet.ImageProtocolKitty})
	renderer.Set(&terminalpet.ImageDraw{ID: 7, Animation: animation, State: terminalpet.Idle, X: 2, Y: 3, Columns: 4, Rows: 3})
	output := newPetImageOutput(file, renderer)
	if _, err := output.Write([]byte(terminalSyncStart + "initial-frame" + terminalSyncEnd)); err != nil {
		t.Fatal(err)
	}
	start, err := file.Seek(0, io.SeekCurrent)
	if err != nil {
		t.Fatal(err)
	}
	clearFrame := terminalSyncStart + ansi.EraseEntireScreen + "resized-frame" + terminalSyncEnd
	if _, err := output.Write([]byte(clearFrame)); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(file.Name())
	if err != nil {
		t.Fatal(err)
	}
	got := string(data[start:])
	clearAt := strings.Index(got, ansi.EraseEntireScreen)
	transmitAt := strings.Index(got, "_Ga=T,t=d,f=100,c=4,r=3")
	syncEnd := strings.Index(got, terminalSyncEnd)
	if clearAt < 0 || transmitAt < clearAt || syncEnd < transmitAt {
		t.Fatalf("pet was not restored inside the full-screen redraw: %q", got)
	}
}

func TestPetImageOutputClearsKittyImageAfterLeavingAltScreen(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "pet-output-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	frame := image.NewNRGBA(image.Rect(0, 0, 1, 1))
	animation, err := terminalpet.ThumbnailAnimation(frame)
	if err != nil {
		t.Fatal(err)
	}
	renderer := terminalpet.NewImageRenderer(terminalpet.ImageSupport{Protocol: terminalpet.ImageProtocolKitty})
	renderer.Set(&terminalpet.ImageDraw{ID: 7, Animation: animation, State: terminalpet.Idle, Columns: 4, Rows: 3})
	output := newPetImageOutput(file, renderer)
	if _, err := output.Write([]byte("frame")); err != nil {
		t.Fatal(err)
	}
	if _, err := output.Write([]byte("\x1b[?1049l")); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(file.Name())
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	deleteAt := strings.LastIndex(got, "_Ga=d,d=I,i=7,q=2")
	exitAt := strings.LastIndex(got, "\x1b[?1049l")
	if deleteAt < 0 || exitAt < 0 || deleteAt < exitAt {
		t.Fatalf("Kitty image was not cleared after alt-screen exit: %q", got)
	}
	syncStart := strings.LastIndex(got[:deleteAt], "\x1b[?2026h")
	syncEnd := strings.Index(got[deleteAt:], "\x1b[?2026l")
	if syncStart < exitAt || syncEnd < 0 {
		t.Fatalf("Kitty image cleanup was not synchronized after alt-screen exit: %q", got)
	}
}

func TestPetImageOutputFinalCleanupRepeatsAllKittyDeletes(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "pet-output-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	frame := image.NewNRGBA(image.Rect(0, 0, 1, 1))
	animation, err := terminalpet.ThumbnailAnimation(frame)
	if err != nil {
		t.Fatal(err)
	}
	renderer := terminalpet.NewImageRenderer(terminalpet.ImageSupport{Protocol: terminalpet.ImageProtocolKitty})
	renderer.Set(&terminalpet.ImageDraw{ID: petAmbientImageID, Animation: animation, State: terminalpet.Idle, Columns: 4, Rows: 3})
	output := newPetImageOutput(file, renderer)
	if _, err := output.Write([]byte("frame")); err != nil {
		t.Fatal(err)
	}
	if _, err := output.Write([]byte("\x1b[?1049l")); err != nil {
		t.Fatal(err)
	}
	cleanupStart, err := file.Seek(0, io.SeekCurrent)
	if err != nil {
		t.Fatal(err)
	}
	if err := output.clearImage(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(file.Name())
	if err != nil {
		t.Fatal(err)
	}
	cleanup := string(data[cleanupStart:])
	if !strings.Contains(cleanup, ansi.ResetModeMouseExtSgrPixel) {
		t.Fatalf("final cleanup did not reset pixel mouse mode: %q", cleanup)
	}
	for _, id := range []uint32{petAmbientImageID, petPreviewImageID} {
		want := fmt.Sprintf("_Ga=d,d=I,i=%d,q=2", id)
		if !strings.Contains(cleanup, want) {
			t.Errorf("final cleanup did not repeat Kitty delete for image %d: %q", id, cleanup)
		}
	}
}

func TestPetImageOutputClearsSixelBeforeLeavingAltScreen(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "pet-output-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	frame := image.NewNRGBA(image.Rect(0, 0, 1, 1))
	animation, err := terminalpet.ThumbnailAnimation(frame)
	if err != nil {
		t.Fatal(err)
	}
	renderer := terminalpet.NewImageRenderer(terminalpet.ImageSupport{Protocol: terminalpet.ImageProtocolSixel})
	renderer.Set(&terminalpet.ImageDraw{ID: 7, Animation: animation, State: terminalpet.Idle, X: 2, Y: 3, Columns: 4, Rows: 3, HeightPixels: 12})
	output := newPetImageOutput(file, renderer)
	if _, err := output.Write([]byte("frame")); err != nil {
		t.Fatal(err)
	}
	if _, err := output.Write([]byte("\x1b[?1049l")); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(file.Name())
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	clearAt := strings.LastIndex(got, "\x1b[4;3H    ")
	exitAt := strings.LastIndex(got, "\x1b[?1049l")
	if clearAt < 0 || exitAt < 0 || clearAt > exitAt {
		t.Fatalf("Sixel cell area was not cleared before alt-screen exit: %q", got)
	}
}

func TestPetImageOutputDoesNotCloseAnExistingSynchronizedFrame(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "pet-output-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	animation, err := terminalpet.ThumbnailAnimation(image.NewNRGBA(image.Rect(0, 0, 1, 1)))
	if err != nil {
		t.Fatal(err)
	}
	renderer := terminalpet.NewImageRenderer(terminalpet.ImageSupport{Protocol: terminalpet.ImageProtocolKitty})
	renderer.Set(&terminalpet.ImageDraw{ID: 7, Animation: animation, State: terminalpet.Idle, Columns: 4, Rows: 3})
	if _, err := newPetImageOutput(file, renderer).Write([]byte(terminalSyncStart + "partial-frame")); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(file.Name())
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	if strings.Count(got, terminalSyncStart) != 1 || strings.Contains(got, terminalSyncEnd) {
		t.Fatalf("pet output changed caller-owned synchronized frame boundaries: %q", got)
	}
	if strings.Index(got, "_Ga=T") < strings.Index(got, "partial-frame") {
		t.Fatalf("pet image was not appended to the open frame: %q", got)
	}
}

func TestPetImageOutputReportsUnsynchronizedShortWrite(t *testing.T) {
	animation, err := terminalpet.ThumbnailAnimation(image.NewNRGBA(image.Rect(0, 0, 1, 1)))
	if err != nil {
		t.Fatal(err)
	}
	renderer := terminalpet.NewImageRenderer(terminalpet.ImageSupport{Protocol: terminalpet.ImageProtocolKitty})
	renderer.Set(&terminalpet.ImageDraw{ID: 7, Animation: animation, State: terminalpet.Idle, Columns: 4, Rows: 3})
	sink := &shortTerminalOutput{limit: len(terminalSyncStart)}
	written, err := newPetImageOutput(sink, renderer).Write([]byte("long-frame-payload"))
	if !errors.Is(err, io.ErrShortWrite) || written != len(terminalSyncStart) {
		t.Fatalf("Write() = %d, %v; want %d, %v", written, err, len(terminalSyncStart), io.ErrShortWrite)
	}
}

func TestPetImageOutputKeepsTextAliveAfterRendererFailure(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "pet-output-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	animation, err := terminalpet.ThumbnailAnimation(image.NewNRGBA(image.Rect(0, 0, 1, 1)))
	if err != nil {
		t.Fatal(err)
	}
	renderer := terminalpet.NewImageRenderer(terminalpet.ImageSupport{Protocol: terminalpet.ImageProtocolKittyLocalFile})
	renderer.Set(&terminalpet.ImageDraw{ID: 7, Animation: animation, State: terminalpet.Idle, Columns: 4, Rows: 3})
	output := newPetImageOutput(file, renderer)
	for _, frame := range []string{"first-frame", "second-frame"} {
		if written, err := output.Write([]byte(frame)); err != nil || written != len(frame) {
			t.Fatalf("Write(%q) = %d, %v", frame, written, err)
		}
	}
	data, err := os.ReadFile(file.Name())
	if err != nil {
		t.Fatal(err)
	}
	if got := string(data); got != "first-framesecond-frame" {
		t.Fatalf("text output after renderer failure = %q", got)
	}
}

func TestPetImageOutputSerializesWriteAndCleanup(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "pet-output-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	animation, err := terminalpet.ThumbnailAnimation(image.NewNRGBA(image.Rect(0, 0, 1, 1)))
	if err != nil {
		t.Fatal(err)
	}
	renderer := terminalpet.NewImageRenderer(terminalpet.ImageSupport{Protocol: terminalpet.ImageProtocolKitty})
	renderer.Set(&terminalpet.ImageDraw{ID: petAmbientImageID, Animation: animation, State: terminalpet.Idle, Columns: 4, Rows: 3})
	output := newPetImageOutput(file, renderer)
	var wait sync.WaitGroup
	errorsSeen := make(chan error, 9)
	for range 8 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, err := output.Write([]byte("frame"))
			errorsSeen <- err
		}()
	}
	wait.Add(1)
	go func() {
		defer wait.Done()
		errorsSeen <- output.clearImage()
	}()
	wait.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		if err != nil {
			t.Fatal(err)
		}
	}
}
