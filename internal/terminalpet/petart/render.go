package petart

import (
	"image"
	"math"
)

const (
	// SpriteScale draws the 48x40 design at half size: 24x20 pixels, which
	// half-block cells show as 24 columns by 10 rows.
	SpriteScale = 0.5
	// SpriteColumns and SpriteRows are the terminal cells the sprite fills.
	SpriteColumns = 24
	SpriteRows    = 10

	// outlineInk is the share of an image outline that is outline ink. The
	// rest comes from the fill it wraps, so every part gets a darker shade of
	// itself instead of flat black.
	outlineInk = 0.62
	// defaultSupersample is the block of samples averaged into each image
	// pixel, per side.
	defaultSupersample = 4
	// liveSupersample is the lighter sampling used for animation frames.
	liveSupersample = 2
)

// Frame is one drawn frame as palette keys, '.' where nothing is drawn.
type Frame struct {
	Width, Height int
	// Pixels holds Width*Height keys, row by row.
	Pixels []byte
}

// Row returns one row of the frame as a string of palette keys.
func (f Frame) Row(y int) string {
	return string(f.Pixels[y*f.Width : (y+1)*f.Width])
}

// Rows returns the frame as one string of palette keys per row.
func (f Frame) Rows() []string {
	rows := make([]string, f.Height)
	for y := range rows {
		rows[y] = f.Row(y)
	}
	return rows
}

// draw paints a character and its mood onto a new canvas, without outlines.
// headroom adds empty device rows above the design space.
func draw(form Form, mood Mood, seconds, scale float64, headroom int) *canvas {
	g := newCanvas(scale, headroom)
	spec, ok := form.spec()
	if !ok {
		return g
	}
	p, flip := poseFor(mood, seconds)
	spec.draw(g, p, seconds)
	if flip {
		g.flip()
	}
	drawMoodExtras(g, mood, seconds)
	return g
}

// Render draws one frame of a companion at the given scale. seconds is time on
// the mood's own clock. An unknown form gives an empty frame.
func Render(form Form, mood Mood, seconds, scale float64) Frame {
	g := draw(form, mood, seconds, scale, 0)
	// Keep the outline about one design pixel thick at any size.
	pixels, _ := g.finish(max(1, int(jsRound(scale))))
	return Frame{Width: g.width, Height: g.height, Pixels: pixels}
}

// Sprite draws the 24x20 frame used for half-block cells.
func Sprite(form Form, mood Mood, seconds float64) Frame {
	return Render(form, mood, seconds, SpriteScale)
}

// Image draws one frame as a picture height pixels tall (the width keeps the
// 48:40 shape), for terminals that show images. Edges are anti-aliased,
// outlines take a darker shade of the colour they wrap, and a soft shadow
// sits under the feet. The picture is transparent where nothing is drawn.
//
// The art is made of shapes, so it is drawn directly at the target size,
// supersampled and averaged down, instead of enlarging the small sprite.
func Image(form Form, mood Mood, seconds float64, height int) *image.NRGBA {
	return imageAt(form, mood, seconds, height, 0, defaultSupersample)
}

// ImageWithHeadroom is Image with headroom extra transparent rows above the
// character, so the picture is height+headroom pixels tall. The design space
// has no room above the head, and a hopping character would otherwise lose
// its hat to the top edge. It samples less densely than Image, which is
// enough for frames that are on screen for a tenth of a second.
func ImageWithHeadroom(form Form, mood Mood, seconds float64, height, headroom int) *image.NRGBA {
	return imageAt(form, mood, seconds, height, max(0, headroom), liveSupersample)
}

func imageAt(form Form, mood Mood, seconds float64, height, headroom, supersample int) *image.NRGBA {
	if height <= 0 {
		return image.NewNRGBA(image.Rectangle{})
	}
	ss := max(1, supersample)
	scale := (float64(height) / DesignHeight) * float64(ss)
	g := draw(form, mood, seconds, scale, headroom*ss)
	pixels, source := g.finish(max(1, int(jsRound(scale*0.8))))
	srcWidth := g.width
	outWidth := srcWidth / ss
	outHeight := g.height / ss
	out := image.NewNRGBA(image.Rect(0, 0, outWidth, outHeight))
	ink := palette['K']
	samples := float64(ss * ss)
	for y := 0; y < outHeight; y++ {
		for x := 0; x < outWidth; x++ {
			var r, gr, b float64
			count := 0
			for sy := 0; sy < ss; sy++ {
				for sx := 0; sx < ss; sx++ {
					index := (y*ss+sy)*srcWidth + x*ss + sx
					key := pixels[index]
					if key == transparent {
						continue
					}
					rgb := palette[key]
					cr, cg, cb := float64(rgb.R), float64(rgb.G), float64(rgb.B)
					if key == 'K' {
						if from := source[index]; from >= 0 {
							if fill, ok := palette[pixels[from]]; ok {
								cr = float64(float64(ink.R)*outlineInk) + float64(float64(fill.R)*(1-outlineInk))
								cg = float64(float64(ink.G)*outlineInk) + float64(float64(fill.G)*(1-outlineInk))
								cb = float64(float64(ink.B)*outlineInk) + float64(float64(fill.B)*(1-outlineInk))
							}
						}
					}
					r += cr
					gr += cg
					b += cb
					count++
				}
			}
			if count == 0 {
				continue
			}
			n := float64(count)
			offset := out.PixOffset(x, y)
			out.Pix[offset] = uint8(jsRound(r / n))
			out.Pix[offset+1] = uint8(jsRound(gr / n))
			out.Pix[offset+2] = uint8(jsRound(b / n))
			out.Pix[offset+3] = uint8(jsRound(255 * n / samples))
		}
	}
	addGroundShadow(out, mood, seconds, headroom)
	return out
}

// addGroundShadow draws a soft shadow under the feet, behind the character.
// It stays on the ground and shrinks as the character jumps.
func addGroundShadow(img *image.NRGBA, mood Mood, seconds float64, headroom int) {
	width := float64(img.Rect.Dx())
	// The design space starts below the headroom.
	height := float64(img.Rect.Dy() - headroom)
	p, flip := poseFor(mood, seconds)
	lift := math.Min(0.55, p.jump/12)
	centre := 21.5
	if flip {
		centre = DesignWidth - 21.5
	}
	cx := (centre / DesignWidth) * width
	cy := (38.6/DesignHeight)*height + float64(headroom)
	rx := (12.5 / DesignWidth) * width * (1 - lift)
	ry := (1.6 / DesignHeight) * height * (1 - lift*0.6)
	strength := 0.32 * (1 - lift)
	for y := int(math.Floor(cy - ry)); y <= int(math.Ceil(cy+ry)); y++ {
		if y < 0 || y >= img.Rect.Dy() {
			continue
		}
		for x := int(math.Floor(cx - rx)); x <= int(math.Ceil(cx+rx)); x++ {
			if x < 0 || x >= img.Rect.Dx() {
				continue
			}
			dx := (float64(x) + 0.5 - cx) / rx
			dy := (float64(y) + 0.5 - cy) / ry
			d := float64(dx*dx) + float64(dy*dy)
			if d >= 1 {
				continue
			}
			shadowAlpha := strength * (1 - d) * (1 - d)
			offset := img.PixOffset(x, y)
			alpha := float64(img.Pix[offset+3]) / 255
			// Composite the character over the shadow, which is black.
			outAlpha := alpha + float64(shadowAlpha*(1-alpha))
			if outAlpha <= 0 {
				continue
			}
			for channel := 0; channel < 3; channel++ {
				img.Pix[offset+channel] = uint8(jsRound(float64(img.Pix[offset+channel]) * alpha / outAlpha))
			}
			img.Pix[offset+3] = uint8(jsRound(outAlpha * 255))
		}
	}
}
