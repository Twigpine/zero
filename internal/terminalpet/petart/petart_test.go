package petart

import (
	"bytes"
	"strings"
	"testing"
)

func TestJSRoundRoundsHalfTowardPositiveInfinity(t *testing.T) {
	// math.Round would give -1, -2 and -3 for the negative halves.
	for value, want := range map[float64]float64{
		-0.5: 0, -1.5: -1, -2.5: -2, 0.5: 1, 2.5: 3, 1.4: 1, -1.6: -2, 0: 0,
	} {
		if got := jsRound(value); got != want {
			t.Errorf("jsRound(%v) = %v, want %v", value, got, want)
		}
	}
}

func TestFormsHaveNamesAndParse(t *testing.T) {
	forms := Forms()
	if len(forms) != 9 || forms[0] != Twig {
		t.Fatalf("Forms() = %v, want nine forms starting with twig", forms)
	}
	names := map[string]bool{}
	for _, form := range forms {
		if !form.Valid() || form.Name() == "" || form.Kind() == "" {
			t.Errorf("%q: valid=%v name=%q kind=%q", form, form.Valid(), form.Name(), form.Kind())
		}
		if names[form.Name()] {
			t.Errorf("duplicate name %q", form.Name())
		}
		names[form.Name()] = true
		for _, input := range []string{string(form), form.Name(), "  " + strings.ToUpper(form.Name()) + " "} {
			if got, ok := ParseForm(input); !ok || got != form {
				t.Errorf("ParseForm(%q) = %q, %v; want %q", input, got, ok, form)
			}
		}
	}
	for _, input := range []string{"", "boba", "twig2"} {
		if got, ok := ParseForm(input); ok {
			t.Errorf("ParseForm(%q) = %q, want no match", input, got)
		}
	}
	unknown := Form("boba")
	if unknown.Valid() || unknown.Name() != "" || unknown.Kind() != "" {
		t.Errorf("unknown form reports valid=%v name=%q kind=%q", unknown.Valid(), unknown.Name(), unknown.Kind())
	}
}

func TestSpriteIsPalettePixelsAtSpriteSize(t *testing.T) {
	for _, form := range Forms() {
		for _, mood := range Moods() {
			for _, seconds := range []float64{0, 0.37, 1.3, 2.9, 6.1} {
				frame := Sprite(form, mood, seconds)
				if frame.Width != SpriteColumns || frame.Height != SpriteRows*2 || len(frame.Pixels) != frame.Width*frame.Height {
					t.Fatalf("%s/%s: frame is %dx%d with %d pixels", form, mood, frame.Width, frame.Height, len(frame.Pixels))
				}
				drawn := 0
				for _, key := range frame.Pixels {
					if key == transparent {
						continue
					}
					drawn++
					if _, ok := Color(key); !ok {
						t.Fatalf("%s/%s t=%v: pixel %q is not in the palette", form, mood, seconds, key)
					}
				}
				if drawn < 100 {
					t.Errorf("%s/%s t=%v: only %d pixels drawn", form, mood, seconds, drawn)
				}
			}
		}
	}
}

func TestRenderIsDeterministicAndMoodsDiffer(t *testing.T) {
	first := Sprite(Twig, Idle, 1.3)
	if second := Sprite(Twig, Idle, 1.3); !bytes.Equal(first.Pixels, second.Pixels) {
		t.Fatal("the same inputs drew two different frames")
	}
	for _, mood := range Moods() {
		if mood == Idle {
			continue
		}
		if other := Sprite(Twig, mood, 1.3); bytes.Equal(first.Pixels, other.Pixels) {
			t.Errorf("%s draws the same frame as idle", mood)
		}
	}
	if full := Render(Twig, Idle, 0.5, 1); full.Width != DesignWidth || full.Height != DesignHeight {
		t.Errorf("full scale is %dx%d, want %dx%d", full.Width, full.Height, DesignWidth, DesignHeight)
	}
}

func TestUnknownFormAndMoodDoNotPanic(t *testing.T) {
	frame := Sprite("boba", Idle, 0)
	for _, key := range frame.Pixels {
		if key != transparent {
			t.Fatal("an unknown form drew pixels")
		}
	}
	if frame.Width != SpriteColumns || frame.Height != SpriteRows*2 {
		t.Errorf("unknown form frame is %dx%d", frame.Width, frame.Height)
	}
	// An unknown mood stands still like idle, without a badge.
	if Mood("boba").Valid() {
		t.Error("unknown mood reports valid")
	}
	if got, want := Sprite(Twig, "boba", 1.3), Sprite(Twig, Idle, 1.3); !bytes.Equal(got.Pixels, want.Pixels) {
		t.Error("an unknown mood does not fall back to idle")
	}
	if img := Image(Twig, Idle, 0, 0); img.Rect.Dx() != 0 || img.Rect.Dy() != 0 {
		t.Errorf("zero-height image is %v", img.Rect)
	}
}

func TestMoodFrameDelays(t *testing.T) {
	if Idle.FrameDelay() <= Working.FrameDelay() || Sleeping.FrameDelay() <= Idle.FrameDelay() {
		t.Errorf("calm moods should redraw less often: idle %v, working %v, sleeping %v", Idle.FrameDelay(), Working.FrameDelay(), Sleeping.FrameDelay())
	}
	for _, mood := range Moods() {
		if !mood.Valid() || mood.FrameDelay() <= 0 {
			t.Errorf("%s: valid=%v delay=%v", mood, mood.Valid(), mood.FrameDelay())
		}
	}
}

func TestImageKeepsShapeAndTransparency(t *testing.T) {
	for _, height := range []int{40, 75, 120} {
		img := Image(Ember, Happy, 0.4, height)
		if img.Rect.Dy() != height || img.Rect.Dx() != height*DesignWidth/DesignHeight {
			t.Fatalf("height %d: image is %dx%d", height, img.Rect.Dx(), img.Rect.Dy())
		}
		opaque, clear, soft := 0, 0, 0
		for offset := 3; offset < len(img.Pix); offset += 4 {
			switch img.Pix[offset] {
			case 0:
				clear++
			case 255:
				opaque++
			default:
				soft++
			}
		}
		// A body, an empty background, and anti-aliased edges plus shadow.
		if opaque == 0 || clear == 0 || soft == 0 {
			t.Errorf("height %d: %d opaque, %d clear, %d soft pixels", height, opaque, clear, soft)
		}
		if corner := img.NRGBAAt(0, 0); corner.A != 0 {
			t.Errorf("height %d: top-left corner is not transparent: %v", height, corner)
		}
	}
}
