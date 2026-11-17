// Package petart draws Zero's built-in terminal companions.
//
// Characters are drawn from simple shapes (ellipses, capsules, polygons) in a
// fixed 48x40 design space and rasterised at a chosen scale. Scale 1 is the
// full-size art; scale 0.5 is the 24x20 sprite that half-block cells show as
// 24 columns by 10 rows. Shapes scale cleanly, but faces do not: a two-pixel
// eye shrunk to one pixel reads as noise. So eyes, mouths and blush are fixed
// pixel stamps when the canvas is compact (scale below 1), positioned by the
// same design-space coordinates.
//
// Pixels are palette keys ('.' is transparent). Everything is pure: time comes
// in as an argument, so the same inputs always give the same frame.
//
// The engine is a port of the one OpenClaude ships, and the two are kept
// frame-compatible; see testdata/README.md.
package petart

import (
	"image/color"
	"math"
)

const (
	// DesignWidth and DesignHeight are the size of the design space.
	DesignWidth  = 48
	DesignHeight = 40

	transparent = '.'
	minRadius   = 0.55
)

// palette maps a pixel key to its colour.
var palette = map[byte]color.NRGBA{
	'K': hex(0x231815), // outline
	'B': hex(0x8f5d3d), // marten fur
	'b': hex(0xa8744c), // fur highlight
	'D': hex(0x5f3c29), // fur shadow, paws, boots
	'C': hex(0xf4dca6), // cream belly, horns
	'E': hex(0x17110f), // eye
	'W': hex(0xffffff), // eye glint
	'N': hex(0x3b2622), // nose, mouth
	'P': hex(0xee8f86), // blush, inner ear
	'G': hex(0x226633), // hood green
	'g': hex(0x4fae54), // tunic green
	'R': hex(0xd8423c), // feather, scarf, heart
	'S': hex(0xf0c8a0), // skin
	'H': hex(0x7a522e), // hair, wood
	'Y': hex(0xf2c14e), // gold trim
	'X': hex(0xc8cdd2), // steel
	'p': hex(0xb98af0), // light purple
	'Q': hex(0x7a46be), // robe purple
	'w': hex(0xf1f2f6), // white cloth, beard
	'n': hex(0x3a3d46), // dark grey
	'M': hex(0x5b606c), // ninja grey
	'V': hex(0x2f4a8c), // navy coat
	'd': hex(0xee6a4c), // dragon scales
	'o': hex(0xf58a66), // dragon highlight
	'F': hex(0xb83a2e), // dragon wing
	'O': hex(0xffa23a), // flame orange
	'A': hex(0xffe066), // spark yellow
	'u': hex(0xa5dcff), // visor blue
	'U': hex(0x3f78e0), // suit blue
	'T': hex(0x3ba99a), // octopus teal
	't': hex(0x7fd6c2), // octopus highlight
	'L': hex(0x1c1e23), // leather jacket
	'l': hex(0x565d6b), // leather sheen
	'h': hex(0xc3c6cc), // salt-and-pepper hair
	'a': hex(0x7c8089), // hair shadow
	'c': hex(0x5fe3ff), // chip glow
}

func hex(rgb uint32) color.NRGBA {
	return color.NRGBA{R: uint8(rgb >> 16), G: uint8(rgb >> 8), B: uint8(rgb), A: 0xff}
}

// Color reports the colour of a pixel key. The boolean is false for the
// transparent key and for anything that is not in the palette.
func Color(key byte) (color.NRGBA, bool) {
	value, ok := palette[key]
	return value, ok
}

type eyeStyle uint8

const (
	eyeOpen eyeStyle = iota
	eyeUp
	eyeDown
	eyeShut
	eyeHappy
)

type mouthStyle uint8

const (
	mouthFlat mouthStyle = iota
	mouthSmile
	mouthFrown
	mouthO
)

type point struct{ x, y float64 }

// canvas is a pixel buffer addressed in design-space coordinates.
type canvas struct {
	width, height int
	scale         float64
	buf           []byte
	// compact is true below scale 1: faces switch to fixed pixel stamps.
	compact bool
	// smooth is true from scale 2 up, where there are enough pixels to draw
	// faces as curves instead of pixel-art dots.
	smooth bool
	// block is how many device pixels a single design dot covers, so glints
	// and sparks stay visible above scale 1.
	block int
	// headroom is extra rows above the design space, in device pixels. The
	// design is drawn below them, so a hop has somewhere to go instead of
	// being cut off at the top.
	headroom int
}

func newCanvas(scale float64, headroom int) *canvas {
	width := int(jsRound(DesignWidth * scale))
	height := int(jsRound(DesignHeight*scale)) + headroom
	buf := make([]byte, width*height)
	for index := range buf {
		buf[index] = transparent
	}
	return &canvas{
		width: width, height: height, scale: scale, buf: buf, headroom: headroom,
		compact: scale < 1,
		smooth:  scale >= 2,
		block:   max(1, int(jsRound(scale))),
	}
}

// jsRound rounds half toward positive infinity, the rule the reference engine
// uses. math.Round rounds half away from zero, which differs for negatives.
func jsRound(value float64) float64 {
	return math.Floor(value + 0.5)
}

func (c *canvas) put(x, y int, key byte) {
	y += c.headroom
	if x >= 0 && x < c.width && y >= 0 && y < c.height {
		c.buf[y*c.width+x] = key
	}
}

// The shape tests below decide which pixels are drawn. Each product is
// converted explicitly so the compiler cannot fuse it into a multiply-add:
// fused arithmetic rounds differently, and frames must be the same on every
// architecture.

func (c *canvas) fillEllipse(cx, cy, rx, ry float64, key byte) {
	for y := int(math.Floor(cy - ry)); y <= int(math.Ceil(cy+ry)); y++ {
		for x := int(math.Floor(cx - rx)); x <= int(math.Ceil(cx+rx)); x++ {
			dx := (float64(x) + 0.5 - cx) / rx
			dy := (float64(y) + 0.5 - cy) / ry
			if float64(dx*dx)+float64(dy*dy) <= 1 {
				c.put(x, y, key)
			}
		}
	}
}

func (c *canvas) fillCapsule(x1, y1, x2, y2, radius float64, key byte) {
	vx := x2 - x1
	vy := y2 - y1
	len2 := float64(vx*vx) + float64(vy*vy)
	if len2 == 0 {
		len2 = 1
	}
	limit := float64(radius * radius)
	for y := int(math.Floor(math.Min(y1, y2) - radius)); y <= int(math.Ceil(math.Max(y1, y2)+radius)); y++ {
		for x := int(math.Floor(math.Min(x1, x2) - radius)); x <= int(math.Ceil(math.Max(x1, x2)+radius)); x++ {
			px := float64(x) + 0.5 - x1
			py := float64(y) + 0.5 - y1
			k := math.Max(0, math.Min(1, (float64(px*vx)+float64(py*vy))/len2))
			ex := px - float64(k*vx)
			ey := py - float64(k*vy)
			if float64(ex*ex)+float64(ey*ey) <= limit {
				c.put(x, y, key)
			}
		}
	}
}

func (c *canvas) fillPolygon(points []point, key byte) {
	if len(points) == 0 {
		return
	}
	minX, maxX := points[0].x, points[0].x
	minY, maxY := points[0].y, points[0].y
	for _, p := range points[1:] {
		minX, maxX = math.Min(minX, p.x), math.Max(maxX, p.x)
		minY, maxY = math.Min(minY, p.y), math.Max(maxY, p.y)
	}
	for y := int(math.Floor(minY)); y <= int(math.Ceil(maxY)); y++ {
		for x := int(math.Floor(minX)); x <= int(math.Ceil(maxX)); x++ {
			px := float64(x) + 0.5
			py := float64(y) + 0.5
			inside := false
			for i, j := 0, len(points)-1; i < len(points); j, i = i, i+1 {
				a, b := points[i], points[j]
				if (a.y > py) != (b.y > py) && px < float64((b.x-a.x)*(py-a.y))/(b.y-a.y)+a.x {
					inside = !inside
				}
			}
			if inside {
				c.put(x, y, key)
			}
		}
	}
}

// stampAt writes a small pixel pattern with its top-left at a device pixel.
func (c *canvas) stampAt(x, y int, pattern ...string) {
	for row, line := range pattern {
		for col := 0; col < len(line); col++ {
			if line[col] != transparent {
				c.put(x+col, y+row, line[col])
			}
		}
	}
}

func (c *canvas) ell(cx, cy, rx, ry float64, key byte) {
	c.fillEllipse(cx*c.scale, cy*c.scale, math.Max(minRadius, rx*c.scale), math.Max(minRadius, ry*c.scale), key)
}

func (c *canvas) capsule(x1, y1, x2, y2, radius float64, key byte) {
	c.fillCapsule(x1*c.scale, y1*c.scale, x2*c.scale, y2*c.scale, math.Max(minRadius, radius*c.scale), key)
}

func (c *canvas) poly(key byte, points ...point) {
	scaled := make([]point, len(points))
	for index, p := range points {
		scaled[index] = point{p.x * c.scale, p.y * c.scale}
	}
	c.fillPolygon(scaled, key)
}

// dot draws one design pixel at a design-space point.
func (c *canvas) dot(x, y float64, key byte) {
	px := int(math.Floor(x * c.scale))
	py := int(math.Floor(y * c.scale))
	for dy := 0; dy < c.block; dy++ {
		for dx := 0; dx < c.block; dx++ {
			c.put(px+dx, py+dy, key)
		}
	}
}

// row draws a run of design pixels to the right of a design-space point.
func (c *canvas) row(x, y float64, count int, key byte) {
	px := int(math.Floor(x * c.scale))
	py := int(math.Floor(y * c.scale))
	for dy := 0; dy < c.block; dy++ {
		for index := 0; index < count*c.block; index++ {
			c.put(px+index, py+dy, key)
		}
	}
}

// stamp writes an unscaled pixel pattern with its top-left at a design-space
// point.
func (c *canvas) stamp(x, y float64, pattern ...string) {
	c.stampAt(int(math.Floor(x*c.scale)), int(math.Floor(y*c.scale)), pattern...)
}

func (c *canvas) eye(x, y float64, style eyeStyle) {
	if c.compact {
		ex := int(jsRound(x*c.scale)) - 1
		ey := int(jsRound(y*c.scale)) - 1
		switch style {
		case eyeShut, eyeDown:
			c.stampAt(ex, ey+1, "EE")
		case eyeHappy:
			c.stampAt(ex, ey, ".E.", "E.E")
		case eyeUp:
			c.stampAt(ex, ey-1, "WE", "EE")
		default:
			c.stampAt(ex, ey, "WE", "EE")
		}
		return
	}
	if c.smooth {
		switch style {
		case eyeShut:
			// A soft downward curve, like a closed, relaxed eye.
			c.capsule(x-1.9, y+0.5, x, y+1.3, 0.45, 'E')
			c.capsule(x, y+1.3, x+1.9, y+0.5, 0.45, 'E')
		case eyeHappy:
			c.capsule(x-1.9, y+1.2, x, y-0.3, 0.5, 'E')
			c.capsule(x, y-0.3, x+1.9, y+1.2, 0.5, 'E')
		case eyeDown:
			c.ell(x, y+1, 1.9, 1.6, 'E')
			c.ell(x-0.8, y+0.4, 0.6, 0.5, 'W')
		default:
			dy := 0.0
			if style == eyeUp {
				dy = -1
			}
			c.ell(x, y+dy, 1.9, 2.5, 'E')
			c.ell(x-0.75, y+dy-1.1, 0.75, 0.85, 'W')
			c.ell(x+0.8, y+dy+1.1, 0.35, 0.35, 'W')
		}
		return
	}
	switch style {
	case eyeShut:
		c.row(x-2, y+1, 4, 'E')
	case eyeHappy:
		c.dot(x-2, y+1, 'E')
		c.dot(x-1, y, 'E')
		c.dot(x, y, 'E')
		c.dot(x+1, y+1, 'E')
	case eyeDown:
		c.ell(x, y+1, 1.9, 1.7, 'E')
		c.dot(x-1, y, 'W')
	default:
		dy := 0.0
		if style == eyeUp {
			dy = -1
		}
		c.ell(x, y+dy, 1.9, 2.5, 'E')
		c.dot(x-1, y+dy-2, 'W')
		c.dot(x-1, y+dy-1, 'W')
	}
}

func (c *canvas) mouth(x, y float64, style mouthStyle) {
	if c.compact {
		mx := int(jsRound(x * c.scale))
		my := int(jsRound(y * c.scale))
		if style == mouthSmile {
			c.stampAt(mx, my, "NN")
		} else {
			c.stampAt(mx, my, "N")
		}
		return
	}
	if c.smooth {
		switch style {
		case mouthSmile:
			c.capsule(x-1, y+0.1, x+0.5, y+1, 0.4, 'N')
			c.capsule(x+0.5, y+1, x+2, y+0.1, 0.4, 'N')
		case mouthFrown:
			c.capsule(x-1, y+1, x+0.5, y+0.2, 0.4, 'N')
			c.capsule(x+0.5, y+0.2, x+2, y+1, 0.4, 'N')
		case mouthO:
			c.ell(x+0.5, y+0.7, 0.85, 0.95, 'N')
		default:
			c.capsule(x-0.2, y+0.4, x+1.3, y+0.4, 0.35, 'N')
		}
		return
	}
	switch style {
	case mouthSmile:
		c.dot(x-1, y, 'N')
		c.dot(x, y+1, 'N')
		c.dot(x+1, y+1, 'N')
		c.dot(x+2, y, 'N')
	case mouthFrown:
		c.dot(x-1, y+1, 'N')
		c.dot(x, y, 'N')
		c.dot(x+1, y, 'N')
		c.dot(x+2, y+1, 'N')
	case mouthO:
		c.row(x, y, 2, 'N')
		c.row(x, y+1, 2, 'N')
	default:
		c.row(x, y, 2, 'N')
	}
}

func (c *canvas) blush(x, y float64) {
	if c.compact {
		c.stampAt(int(jsRound(x*c.scale)), int(jsRound(y*c.scale)), "P")
		return
	}
	c.ell(x, y, 1.4, 0.9, 'P')
}

// flip mirrors the pixels drawn so far left to right.
func (c *canvas) flip() {
	for y := 0; y < c.height; y++ {
		line := c.buf[y*c.width : (y+1)*c.width]
		for left, right := 0, len(line)-1; left < right; left, right = left+1, right-1 {
			line[left], line[right] = line[right], line[left]
		}
	}
}

// finish outlines every shape, outline pixels thick, and returns the frame.
// For each outline pixel, source holds the index of the nearest drawn pixel
// (and -1 everywhere else), so an outline can take that pixel's colour.
//
// The outline is every empty pixel within reach of a drawn one. A two-pass
// chamfer distance transform (3 per straight step, 4 per diagonal) finds them
// in linear time and keeps thick outlines round.
func (c *canvas) finish(outline int) (pixels []byte, source []int32) {
	const far = 1 << 30
	width, height := c.width, c.height
	dist := make([]int32, width*height)
	near := make([]int32, width*height)
	for index := range dist {
		if c.buf[index] == transparent {
			dist[index] = far
			near[index] = -1
		} else {
			near[index] = int32(index)
		}
	}
	relax := func(i, j int, step int32) {
		if d := dist[j] + step; d < dist[i] {
			dist[i] = d
			near[i] = near[j]
		}
	}
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			i := y*width + x
			if dist[i] == 0 {
				continue
			}
			if x > 0 {
				relax(i, i-1, 3)
			}
			if y > 0 {
				relax(i, i-width, 3)
				if x > 0 {
					relax(i, i-width-1, 4)
				}
				if x < width-1 {
					relax(i, i-width+1, 4)
				}
			}
		}
	}
	for y := height - 1; y >= 0; y-- {
		for x := width - 1; x >= 0; x-- {
			i := y*width + x
			if dist[i] == 0 {
				continue
			}
			if x < width-1 {
				relax(i, i+1, 3)
			}
			if y < height-1 {
				relax(i, i+width, 3)
				if x < width-1 {
					relax(i, i+width+1, 4)
				}
				if x > 0 {
					relax(i, i+width-1, 4)
				}
			}
		}
	}
	limit := int32(outline * 3)
	pixels = make([]byte, width*height)
	source = make([]int32, width*height)
	for index := range pixels {
		source[index] = -1
		switch d := dist[index]; {
		case d == 0:
			pixels[index] = c.buf[index]
		case d <= limit:
			pixels[index] = 'K'
			source[index] = near[index]
		default:
			pixels[index] = transparent
		}
	}
	return pixels, source
}
