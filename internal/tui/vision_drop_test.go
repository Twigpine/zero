package tui

import (
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/zeroruntime"
)

func TestVisionDropWarning(t *testing.T) {
	// No staged images: never warns, regardless of model.
	if got := (model{}).visionDropWarning(); got != "" {
		t.Fatalf("no images should give no warning, got %q", got)
	}

	// Staged images + a model with no vision support (empty model name qualifies):
	// warn immediately, naming the count.
	withImg := model{pendingImages: make([]zeroruntime.ImageBlock, 2)}
	warn := withImg.visionDropWarning()
	if !strings.Contains(warn, "will be dropped") || !strings.Contains(warn, "2 staged") {
		t.Fatalf("expected a drop warning naming the count, got %q", warn)
	}
}
