package tui

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/Gitlawb/zero/internal/terminalpet"
	"github.com/Gitlawb/zero/internal/zeroruntime"
)

func previewImageBlock(t *testing.T) zeroruntime.ImageBlock {
	t.Helper()
	frame := image.NewNRGBA(image.Rect(0, 0, 8, 6))
	for y := 0; y < 6; y++ {
		for x := 0; x < 8; x++ {
			frame.Set(x, y, color.NRGBA{R: 60, G: 170, B: 220, A: 255})
		}
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, frame); err != nil {
		t.Fatal(err)
	}
	return zeroruntime.ImageBlock{MediaType: "image/png", Data: encoded.Bytes()}
}

func TestAttachmentThumbnailRendersInsideComposerWhenSupported(t *testing.T) {
	m := newModel(context.Background(), Options{})
	m.width, m.height = 100, 30
	m.altScreen = true
	m.headerPrinted = true
	m.attachmentRenderers = []*terminalpet.ImageRenderer{terminalpet.NewImageRenderer(terminalpet.ImageSupport{Protocol: terminalpet.ImageProtocolKitty})}
	m.pendingImages = []zeroruntime.ImageBlock{previewImageBlock(t)}
	m.pendingImageLabels = []string{"diagram.png"}
	m.refreshPendingImageThumbnail()
	if len(m.pendingImageThumbnails) != 1 || m.pendingImageThumbnails[0] == nil {
		t.Fatal("expected staged image thumbnail")
	}

	view := m.View()
	plain := plainRender(t, view.Content)
	if strings.Contains(plain, "[Image #1]") {
		t.Fatalf("supported terminal should use thumbnail strip, got:\n%s", plain)
	}
	if strings.Contains(plain, "Image #1 attached") {
		t.Fatalf("thumbnail strip should not duplicate a text attachment label, got:\n%s", plain)
	}

	var output bytes.Buffer
	if err := m.attachmentRenderers[0].Render(&output); err != nil {
		t.Fatal(err)
	}
	if got := output.String(); !strings.Contains(got, "_Ga=T,t=d,f=100,c=10,r=4") {
		t.Fatalf("thumbnail did not schedule a compact Kitty image: %q", got)
	}
}

func TestAttachmentThumbnailKeepsAdditionalImagesVisibleWhenSupported(t *testing.T) {
	m := newModel(context.Background(), Options{})
	m.width, m.height = 100, 30
	m.altScreen = true
	m.attachmentRenderers = []*terminalpet.ImageRenderer{
		terminalpet.NewImageRenderer(terminalpet.ImageSupport{Protocol: terminalpet.ImageProtocolKitty}),
		terminalpet.NewImageRenderer(terminalpet.ImageSupport{Protocol: terminalpet.ImageProtocolKitty}),
	}
	m.pendingImages = []zeroruntime.ImageBlock{previewImageBlock(t), previewImageBlock(t)}
	m.pendingImageLabels = []string{"first.png", "second.png"}
	m.refreshPendingImageThumbnail()

	plain := plainRender(t, m.composerBox(m.width))
	if !strings.Contains(plain, "[Image #2]") {
		t.Fatalf("a supported terminal must keep a second staged image visible, got:\n%s", plain)
	}
	_ = m.View()
	for index, renderer := range m.attachmentRenderers {
		var output bytes.Buffer
		if err := renderer.Render(&output); err != nil {
			t.Fatal(err)
		}
		id := "i=" + strconv.FormatUint(uint64(attachmentImageID+uint32(index)), 10)
		if !strings.Contains(output.String(), id) {
			t.Fatalf("thumbnail %d was not rendered, got %q", index+1, output.String())
		}
	}
}

func TestAttachmentThumbnailFallsBackToChipsWithoutTerminalGraphics(t *testing.T) {
	m := newModel(context.Background(), Options{})
	m.width, m.height = 100, 30
	m.altScreen = true
	m.pendingImages = []zeroruntime.ImageBlock{previewImageBlock(t)}
	m.pendingImageLabels = []string{"diagram.png"}
	m.refreshPendingImageThumbnail()

	plain := plainRender(t, m.composerBox(m.width))
	if !strings.Contains(plain, "[Image #1]") {
		t.Fatalf("unsupported terminal should retain text attachment chip, got:\n%s", plain)
	}
	if strings.Contains(plain, "Image #1 attached") {
		t.Fatalf("unsupported terminal should not reserve thumbnail rows, got:\n%s", plain)
	}
}

func TestAttachmentThumbnailClearsWithPendingImage(t *testing.T) {
	m := newModel(context.Background(), Options{})
	m.width, m.height = 100, 30
	m.altScreen = true
	m.headerPrinted = true
	m.attachmentRenderers = []*terminalpet.ImageRenderer{terminalpet.NewImageRenderer(terminalpet.ImageSupport{Protocol: terminalpet.ImageProtocolKitty})}
	m.pendingImages = []zeroruntime.ImageBlock{previewImageBlock(t)}
	m.pendingImageLabels = []string{"diagram.png"}
	m.refreshPendingImageThumbnail()
	_ = m.View()
	var first bytes.Buffer
	if err := m.attachmentRenderers[0].Render(&first); err != nil {
		t.Fatal(err)
	}

	m.pendingImages = nil
	m.pendingImageLabels = nil
	m.pendingImageThumbnails = nil
	_ = m.View()
	var cleared bytes.Buffer
	if err := m.attachmentRenderers[0].Render(&cleared); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cleared.String(), "a=d,d=I,i=49376") {
		t.Fatalf("clearing pending image should delete its Kitty placement, got %q", cleared.String())
	}
}

func TestPetOutputCompositesAttachmentAndPetInOneFrame(t *testing.T) {
	animation, err := terminalpet.ThumbnailAnimation(image.NewNRGBA(image.Rect(0, 0, 2, 2)))
	if err != nil {
		t.Fatal(err)
	}
	pet := terminalpet.NewImageRenderer(terminalpet.ImageSupport{Protocol: terminalpet.ImageProtocolKitty})
	attachment := terminalpet.NewImageRenderer(terminalpet.ImageSupport{Protocol: terminalpet.ImageProtocolKitty})
	pet.Set(&terminalpet.ImageDraw{ID: petAmbientImageID, Animation: animation, State: terminalpet.Idle, Columns: 4, Rows: 3})
	attachment.Set(&terminalpet.ImageDraw{ID: attachmentImageID, Animation: animation, State: terminalpet.Idle, Columns: 10, Rows: 4})
	sink, err := os.CreateTemp(t.TempDir(), "image-output-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sink.Close() })
	if _, err := newPetImageOutput(sink, pet, attachment).Write([]byte(terminalSyncStart + "frame" + terminalSyncEnd)); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(sink.Name())
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	if strings.Count(got, terminalSyncStart) != 1 || strings.Count(got, terminalSyncEnd) != 1 {
		t.Fatalf("graphics should share Bubble Tea's synchronized frame: %q", got)
	}
	for _, id := range []string{"i=49374", "i=49376"} {
		if !strings.Contains(got, id) {
			t.Fatalf("missing image placement %s: %q", id, got)
		}
	}
}
