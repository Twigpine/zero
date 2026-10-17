package tui

import (
	"testing"

	"github.com/Twigpine/zero/internal/zeroruntime"
)

func TestRemoveLastAttachment(t *testing.T) {
	m := model{
		pendingImages:      []zeroruntime.ImageBlock{{MediaType: "image/png"}, {MediaType: "image/png"}},
		pendingImageLabels: []string{"a.png", "b.png"},
		pendingDocuments:   []pendingDocument{{label: "spec.pdf"}},
	}

	// Documents render last, so a staged doc is removed first.
	m, ok := m.removeLastAttachment()
	if !ok || len(m.pendingDocuments) != 0 {
		t.Fatalf("doc should be removed first: ok=%v docs=%d", ok, len(m.pendingDocuments))
	}
	// Then the last image (images + labels stay in lockstep).
	m, ok = m.removeLastAttachment()
	if !ok || len(m.pendingImages) != 1 || len(m.pendingImageLabels) != 1 || m.pendingImageLabels[0] != "a.png" {
		t.Fatalf("last image should be removed: ok=%v imgs=%d labels=%v", ok, len(m.pendingImages), m.pendingImageLabels)
	}
	// Remove the final image.
	m, ok = m.removeLastAttachment()
	if !ok || len(m.pendingImages) != 0 || len(m.pendingImageLabels) != 0 {
		t.Fatalf("all images should be removed: ok=%v imgs=%d", ok, len(m.pendingImages))
	}
	// Nothing left.
	if _, ok := m.removeLastAttachment(); ok {
		t.Fatal("removeLastAttachment on an empty set must report false")
	}
}
