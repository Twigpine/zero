package petart

import (
	"bytes"
	"math"
	"regexp"
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

var sgrPattern = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func TestHalfBlockLines(t *testing.T) {
	frame := Frame{Width: 4, Height: 3, Pixels: []byte(
		"K.K." +
			"KK.." +
			"..R.",
	)}
	lines := HalfBlockLines(frame)
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2", len(lines))
	}
	// Cell by cell: both pixels, bottom only, top only, neither.
	if got := sgrPattern.ReplaceAllString(lines[0], ""); got != "▀▄▀ " {
		t.Errorf("first line glyphs = %q", got)
	}
	// The odd last row has no pixel beneath it.
	if got := sgrPattern.ReplaceAllString(lines[1], ""); got != "  ▀ " {
		t.Errorf("second line glyphs = %q", got)
	}
	const ink, red = "35;24;21", "216;66;60"
	want := "\x1b[0;38;2;" + ink + ";48;2;" + ink + "m▀" +
		"\x1b[0;38;2;" + ink + "m▄▀" +
		"\x1b[0m "
	if lines[0] != want {
		t.Errorf("first line = %q, want %q", lines[0], want)
	}
	// Leading blanks carry no styling, and a styled line ends reset so the
	// colour cannot leak into whatever is drawn after it.
	if want := "  \x1b[0;38;2;" + red + "m▀\x1b[0m "; lines[1] != want {
		t.Errorf("second line = %q, want %q", lines[1], want)
	}

	sprite := HalfBlockLines(Sprite(Twig, Idle, 0))
	if len(sprite) != SpriteRows {
		t.Fatalf("sprite has %d lines, want %d", len(sprite), SpriteRows)
	}
	for index, line := range sprite {
		glyphs := sgrPattern.ReplaceAllString(line, "")
		if count := len([]rune(glyphs)); count != SpriteColumns {
			t.Errorf("line %d is %d cells wide, want %d", index, count, SpriteColumns)
		}
		if strings.Contains(line, "\x1b[") && !strings.HasSuffix(strings.TrimRight(line, " "), "\x1b[0m") {
			t.Errorf("line %d does not end reset: %q", index, line)
		}
	}
}

func TestRenderWithHeadroomAddsEmptyRowsAndMirror(t *testing.T) {
	plain := Render(Merlin, Celebrate, 0.3, 0.375)
	roomy := RenderWithHeadroom(Merlin, Celebrate, 0.3, 0.375, 2)
	if roomy.Width != plain.Width || roomy.Height != plain.Height+2 {
		t.Fatalf("headroom frame is %dx%d, plain is %dx%d", roomy.Width, roomy.Height, plain.Width, plain.Height)
	}
	// Mid-hop the hat rises: without headroom it is cut at the top edge, with
	// headroom it is drawn.
	drawnAbove := func(f Frame, rows int) int {
		count := 0
		for _, key := range f.Pixels[:rows*f.Width] {
			if key != transparent {
				count++
			}
		}
		return count
	}
	if drawnAbove(roomy, 2) == 0 {
		t.Error("nothing was drawn in the headroom mid-hop")
	}
	if !bytes.Equal(roomy.Pixels[2*roomy.Width:], RenderWithHeadroom(Merlin, Celebrate, 0.3, 0.375, 2).Pixels[2*roomy.Width:]) {
		t.Error("RenderWithHeadroom is not deterministic")
	}
	if got := RenderWithHeadroom(Twig, Idle, 1.3, 0.5, 0); !bytes.Equal(got.Pixels, Sprite(Twig, Idle, 1.3).Pixels) {
		t.Error("zero headroom differs from Render")
	}

	mirrored := Render(Twig, Idle, 1.3, 0.5)
	mirrored.Mirror()
	original := Render(Twig, Idle, 1.3, 0.5)
	for y := 0; y < original.Height; y++ {
		for x := 0; x < original.Width; x++ {
			if mirrored.Pixels[y*original.Width+x] != original.Pixels[y*original.Width+original.Width-1-x] {
				t.Fatalf("Mirror is not a left-right flip at %d,%d", x, y)
			}
		}
	}
}

func TestHalfBlockLines256MatchesTheTrueColourLayout(t *testing.T) {
	frame := RenderWithHeadroom(Ember, Happy, 0.7, 0.375, 1)
	full, limited := HalfBlockLines(frame), HalfBlockLines256(frame)
	if len(full) != len(limited) {
		t.Fatalf("%d lines in true colour, %d in 256", len(full), len(limited))
	}
	for index := range full {
		if got, want := sgrPattern.ReplaceAllString(limited[index], ""), sgrPattern.ReplaceAllString(full[index], ""); got != want {
			t.Fatalf("line %d glyphs differ: %q, want %q", index, got, want)
		}
		if strings.Contains(limited[index], ";2;") {
			t.Fatalf("line %d of the 256-colour frame uses 24-bit colour: %q", index, limited[index])
		}
	}
	if !strings.Contains(strings.Join(limited, ""), "38;5;") {
		t.Fatal("no 256-colour codes in the frame")
	}
}

// TestPalette256IsTheNearestColourByEye recomputes each palette256 entry: the
// xterm colour from 16 to 255 with the smallest CIEDE2000 difference.
func TestPalette256IsTheNearestColourByEye(t *testing.T) {
	if len(palette256) != len(palette) {
		t.Fatalf("palette256 has %d colours, palette has %d", len(palette256), len(palette))
	}
	for key, c := range palette {
		got, ok := palette256[key]
		if !ok {
			t.Fatalf("%q has no 256-colour value", key)
		}
		want, best := 0, math.Inf(1)
		target := cieLab(float64(c.R), float64(c.G), float64(c.B))
		for index := 16; index < 256; index++ {
			r, g, b := xterm256(index)
			if d := ciede2000(target, cieLab(r, g, b)); d < best {
				want, best = index, d
			}
		}
		if int(got) != want {
			t.Errorf("%q #%02x%02x%02x: palette256 has %d, nearest by CIEDE2000 is %d", key, c.R, c.G, c.B, got, want)
		}
	}
}

func xterm256(index int) (float64, float64, float64) {
	if index >= 232 {
		v := float64(8 + (index-232)*10)
		return v, v, v
	}
	index -= 16
	level := func(v int) float64 {
		if v == 0 {
			return 0
		}
		return float64(55 + v*40)
	}
	return level(index / 36), level(index / 6 % 6), level(index % 6)
}

type lab struct{ l, a, b float64 }

func cieLab(r, g, b float64) lab {
	linear := func(c float64) float64 {
		c /= 255
		if c <= 0.04045 {
			return c / 12.92
		}
		return math.Pow((c+0.055)/1.055, 2.4)
	}
	r, g, b = linear(r), linear(g), linear(b)
	x := (0.4124*r + 0.3576*g + 0.1805*b) / 0.95047
	y := 0.2126*r + 0.7152*g + 0.0722*b
	z := (0.0193*r + 0.1192*g + 0.9505*b) / 1.08883
	f := func(t float64) float64 {
		if t > 0.008856 {
			return math.Cbrt(t)
		}
		return 7.787*t + 16.0/116
	}
	fx, fy, fz := f(x), f(y), f(z)
	return lab{116*fy - 16, 500 * (fx - fy), 200 * (fy - fz)}
}

func ciede2000(c1, c2 lab) float64 {
	rad := math.Pi / 180
	cAvg := (math.Hypot(c1.a, c1.b) + math.Hypot(c2.a, c2.b)) / 2
	g := 0.5 * (1 - math.Sqrt(math.Pow(cAvg, 7)/(math.Pow(cAvg, 7)+math.Pow(25, 7))))
	a1, a2 := (1+g)*c1.a, (1+g)*c2.a
	cp1, cp2 := math.Hypot(a1, c1.b), math.Hypot(a2, c2.b)
	hue := func(b, a float64) float64 {
		h := math.Atan2(b, a) / rad
		if h < 0 {
			h += 360
		}
		return h
	}
	h1, h2 := hue(c1.b, a1), hue(c2.b, a2)
	dh := 0.0
	if cp1*cp2 != 0 {
		dh = h2 - h1
		if dh > 180 {
			dh -= 360
		} else if dh < -180 {
			dh += 360
		}
	}
	dL, dC := c2.l-c1.l, cp2-cp1
	dH := 2 * math.Sqrt(cp1*cp2) * math.Sin(dh/2*rad)
	lAvg, cpAvg := (c1.l+c2.l)/2, (cp1+cp2)/2
	hAvg := h1 + h2
	if cp1*cp2 != 0 {
		switch {
		case math.Abs(h1-h2) <= 180:
			hAvg = (h1 + h2) / 2
		case h1+h2 < 360:
			hAvg = (h1 + h2 + 360) / 2
		default:
			hAvg = (h1 + h2 - 360) / 2
		}
	}
	tt := 1 - 0.17*math.Cos((hAvg-30)*rad) + 0.24*math.Cos(2*hAvg*rad) + 0.32*math.Cos((3*hAvg+6)*rad) - 0.2*math.Cos((4*hAvg-63)*rad)
	hueOffset := (hAvg - 275) / 25
	theta := 30 * math.Exp(-hueOffset*hueOffset)
	rc := 2 * math.Sqrt(math.Pow(cpAvg, 7)/(math.Pow(cpAvg, 7)+math.Pow(25, 7)))
	lightness := (lAvg - 50) * (lAvg - 50)
	sl := 1 + 0.015*lightness/math.Sqrt(20+lightness)
	sc := 1 + 0.045*cpAvg
	sh := 1 + 0.015*cpAvg*tt
	rt := -math.Sin(2*theta*rad) * rc
	l, c, h := dL/sl, dC/sc, dH/sh
	return math.Sqrt(l*l + c*c + h*h + rt*c*h)
}
