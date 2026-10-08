package terminalpet

import (
	"image"
	"testing"
	"time"
)

func TestClickDurationEndsOnceAnimationWithoutGenericHold(t *testing.T) {
	action := State("custom-action")
	animation := &Animation{
		durations: map[State][]time.Duration{
			action: {200 * time.Millisecond, 100 * time.Millisecond, 300 * time.Millisecond, 200 * time.Millisecond},
		},
		loopStarts:      map[State]int{action: -1},
		clickAnimations: []State{action},
	}

	if got := animation.PrimaryDuration(action); got != 800*time.Millisecond {
		t.Fatalf("once-action duration = %s, want 800ms", got)
	}
	if got, ok := animation.ClickDuration(action); !ok || got != 800*time.Millisecond {
		t.Fatalf("click duration = %s, %t; want 800ms, true", got, ok)
	}
	if got, ok := animation.ClickDuration(Running); ok || got != 0 {
		t.Fatalf("ordinary activity duration = %s, %t; want 0, false", got, ok)
	}
}

func TestAtlasAnimationRejectsMismatchedOverrideDurations(t *testing.T) {
	sheet := image.NewNRGBA(image.Rect(0, 0, 192, 26*9))
	_, err := atlasAnimation(sheet, 1, map[State]atlasTrack{
		Idle: {row: 0, count: 2, durations: []time.Duration{100 * time.Millisecond}},
	})
	if err == nil {
		t.Fatal("atlasAnimation accepted mismatched override durations")
	}
}
