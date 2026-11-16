package petart

import (
	"math"
	"strings"
)

// Form identifies a built-in companion.
type Form string

const (
	Twig    Form = "twig"
	Robin   Form = "robin"
	Nova    Form = "nova"
	Inky    Form = "inky"
	Merlin  Form = "merlin"
	Kage    Form = "kage"
	Ember   Form = "ember"
	Corsair Form = "corsair"
	Keynote Form = "keynote"
)

type formSpec struct {
	form Form
	name string
	kind string
	draw func(g *canvas, p pose, t float64)
}

// formSpecs lists every built-in companion in the order they are offered.
var formSpecs = []formSpec{
	{Twig, "Twig", "Pine marten", drawTwig},
	{Robin, "Robin", "Archer", drawRobin},
	{Nova, "Nova", "Astronaut", drawNova},
	{Inky, "Inky", "Octopus", drawInky},
	{Merlin, "Merlin", "Wizard", drawMerlin},
	{Kage, "Kage", "Ninja", drawKage},
	{Ember, "Ember", "Dragon", drawEmber},
	{Corsair, "Corsair", "Pirate captain", drawCorsair},
	{Keynote, "Keynote", "Stage presenter", drawKeynote},
}

// Forms returns every built-in companion in the order they are offered.
func Forms() []Form {
	forms := make([]Form, len(formSpecs))
	for index, spec := range formSpecs {
		forms[index] = spec.form
	}
	return forms
}

func (f Form) spec() (formSpec, bool) {
	for _, spec := range formSpecs {
		if spec.form == f {
			return spec, true
		}
	}
	return formSpec{}, false
}

// Valid reports whether f is a built-in companion.
func (f Form) Valid() bool {
	_, ok := f.spec()
	return ok
}

// Name is the name shown to users, or the empty string for an unknown form.
func (f Form) Name() string {
	spec, _ := f.spec()
	return spec.name
}

// Kind is a short description of the character, such as "Pine marten".
func (f Form) Kind() string {
	spec, _ := f.spec()
	return spec.kind
}

// ParseForm resolves a built-in companion from its id or its display name,
// ignoring case and surrounding space.
func ParseForm(input string) (Form, bool) {
	wanted := strings.ToLower(strings.TrimSpace(input))
	for _, spec := range formSpecs {
		if string(spec.form) == wanted || strings.ToLower(spec.name) == wanted {
			return spec.form, true
		}
	}
	return "", false
}

// pose is the body language a mood gives a character. Every character reads
// the same pose, so one set of moods drives all of them.
type pose struct {
	// jump is the height of a hop in design pixels (positive is up).
	jump float64
	// hx and hy offset the head.
	hx, hy float64
	// sq squashes (positive) or stretches (negative) the body.
	sq  float64
	eye eyeStyle
	// ex is a glance: eyes shift left (-1) or right (1).
	ex float64
	// sway moves tails, scarves and wings, -1 to 1 or a little beyond.
	sway float64
	// arm pumps the arms while working.
	arm   float64
	wave  bool
	point bool
	// droop lowers ears and shoulders when sad or sleepy, 0 to 1.
	droop float64
	mouth mouthStyle
	// twitch flicks an ear at idle.
	twitch float64
}

// outfit dresses the shared human body.
type outfit struct {
	skin, cloth, sleeve, hand, legs, boots byte
	// plain hides the mouth and blush (masks, beards).
	plain bool
	// patch covers the left eye.
	patch bool
	back  func(g *canvas, J float64, p pose, t float64)
	hat   func(g *canvas, hx, hy, t float64)
	chest func(g *canvas, J, t float64)
	front func(g *canvas, J, t float64, p pose)
}

// human is the shared body for the human-shaped characters and Nova. Every
// character faces left, toward the prompt.
func human(g *canvas, p pose, t float64, o outfit) {
	J := -p.jump
	hx := 21 + p.hx
	hy := 16 + J + p.hy
	s := p.sq
	if o.back != nil {
		o.back(g, J, p, t)
	}
	g.ell(17, 37+J, 3, 1.8, o.boots)
	g.ell(26, 37+J, 3, 1.8, o.boots)
	g.capsule(18, 33+J, 18, 35+J, 1.8, o.legs)
	g.capsule(25, 33+J, 25, 35+J, 1.8, o.legs)
	g.ell(21.5, 29.5+J+s*2, 7+s*1.5, 6-s*1.5, o.cloth)
	if o.chest != nil {
		o.chest(g, J+s*2, t)
	}
	hand := nearHand(p, t, J)
	switch {
	case p.point:
		g.capsule(14, 27+J, 7, 26+J, 1.7, o.sleeve)
		g.ell(hand.x, hand.y, 1.6, 1.6, o.hand)
	case p.wave:
		g.capsule(14, 27+J, 9, hand.y+1, 1.7, o.sleeve)
		g.ell(hand.x, hand.y, 1.6, 1.6, o.hand)
	default:
		g.capsule(14, 27+J, 12.5, hand.y-1.5, 1.7, o.sleeve)
		g.ell(hand.x, hand.y, 1.5, 1.5, o.hand)
	}
	g.capsule(29, 27+J, 30.5, 31+J-p.arm, 1.7, o.sleeve)
	g.ell(30.5, 32.5+J-p.arm, 1.5, 1.5, o.hand)
	g.ell(hx, hy, 10.5, 9.5, o.skin)
	if o.hat != nil {
		o.hat(g, hx, hy, t)
	}
	if !o.plain {
		g.blush(hx-8, hy+3.5)
		g.blush(hx+5.5, hy+3.5)
		g.mouth(hx-2.5, hy+5, p.mouth)
	}
	if !o.patch {
		g.eye(hx-6+p.ex, hy+0.5, p.eye)
	}
	g.eye(hx+3+p.ex, hy+0.5, p.eye)
	if o.front != nil {
		o.front(g, J, t, p)
	}
}

// nearHand is where human draws the hand on the prompt side.
func nearHand(p pose, t, J float64) point {
	switch {
	case p.point:
		return point{5.5, 26 + J}
	case p.wave:
		return point{8.5, 20 + J + math.Sin(t*10)*2}
	default:
		return point{12.5, 32.5 + J + p.arm}
	}
}

// frontLimb is a paw or claw on the prompt side: it points, waves or rests.
func frontLimb(g *canvas, p pose, t, J float64, key byte) {
	switch {
	case p.point:
		g.capsule(13, 28+J, 6, 26+J, 1.8, key)
	case p.wave:
		g.capsule(13, 28+J, 8, 22+J+math.Sin(t*10)*2, 1.8, key)
	default:
		g.ell(13, 29.5+J+p.arm, 2, 3, key)
	}
}

// blinks reports whether a light that flashes rate times a second is on.
func blinks(t, rate float64) bool {
	return int(math.Floor(t*rate))%2 != 0
}

func drawTwig(g *canvas, p pose, t float64) {
	J := -p.jump
	hx := 20 + p.hx
	hy := 16 + J + p.hy
	s := p.sq
	sw := p.sway * 2
	ed := p.droop * 2
	// Tail first, behind the body: a long curl with a cream tip.
	g.capsule(29, 33+J, 37, 31+J, 3.2, 'D')
	g.capsule(37, 31+J, 41+sw*0.5, 23+J, 3.6, 'B')
	g.capsule(41+sw*0.5, 23+J, 38.5+sw, 16+J, 3.4, 'B')
	g.ell(38.5+sw, 14.5+J, 3, 3, 'C')
	g.ell(16, 37+J, 3.2, 1.8, 'D')
	g.ell(26, 37+J, 3.2, 1.8, 'D')
	g.ell(21, 30+J+s*2, 8+s*2, 7-s*2, 'B')
	g.ell(19.5, 31+J+s*2, 5+s, 5-s*1.5, 'C')
	frontLimb(g, p, t, J, 'D')
	g.ell(28, 29.5+J-p.arm, 2, 3, 'D')
	g.ell(hx-8, hy-8+ed, 3.6, 4, 'B')
	g.ell(hx-8, hy-7.5+ed, 1.8, 2.2, 'P')
	g.ell(hx+8+p.twitch, hy-8+ed-p.twitch, 3.6, 4, 'B')
	g.ell(hx+8+p.twitch, hy-7.5+ed-p.twitch, 1.8, 2.2, 'P')
	g.ell(hx, hy, 11, 9.5, 'B')
	g.ell(hx-3, hy-5, 6, 2.6, 'b')
	g.ell(hx-3, hy+4.8, 6.2, 4, 'C')
	g.ell(hx-4.5, hy+2.6, 1.6, 1.1, 'N')
	g.mouth(hx-5.5, hy+5.5, p.mouth)
	g.blush(hx+6.5, hy+3.8)
	g.eye(hx-7+p.ex, hy-0.5, p.eye)
	g.eye(hx+2.5+p.ex, hy-0.5, p.eye)
}

func drawRobin(g *canvas, p pose, t float64) {
	human(g, p, t, outfit{
		skin: 'S', cloth: 'g', sleeve: 'G', hand: 'S', legs: 'H', boots: 'D',
		hat: func(g *canvas, hx, hy, _ float64) {
			g.ell(hx+8.5, hy, 2.4, 4.5, 'H')
			g.poly('G',
				point{hx - 12, hy - 4},
				point{hx - 9, hy - 10},
				point{hx, hy - 12.5},
				point{hx + 8, hy - 11},
				point{hx + 13, hy - 6},
				point{hx + 13, hy - 4},
			)
			g.poly('G',
				point{hx - 11, hy - 4},
				point{hx - 17, hy - 3},
				point{hx - 9, hy - 7},
			)
			g.capsule(hx-6, hy-9, hx+2, hy-10.5, 0.8, 'g')
			g.capsule(hx+7, hy-9, hx+13, hy-15, 1.1, 'R')
		},
		chest: func(g *canvas, J, _ float64) {
			g.capsule(15.5, 32+J, 27.5, 32+J, 0.8, 'H')
			g.dot(21, 32+J, 'Y')
			g.dot(22, 32+J, 'Y')
		},
		front: func(g *canvas, J, _ float64, _ pose) {
			g.capsule(7, 23+J, 4.5, 30+J, 0.7, 'H')
			g.capsule(4.5, 30+J, 7, 37+J, 0.7, 'H')
			g.capsule(7.5, 23+J, 7.5, 37+J, 0.3, 'X')
		},
	})
}

func drawMerlin(g *canvas, p pose, t float64) {
	human(g, p, t, outfit{
		skin: 'S', cloth: 'Q', sleeve: 'Q', hand: 'S', legs: 'Q', boots: 'D',
		plain: true,
		hat: func(g *canvas, hx, hy, _ float64) {
			g.ell(hx-1.5, hy+7.5, 7, 5, 'w')
			g.poly('Q',
				point{hx - 11, hy - 4.5},
				point{hx + 11, hy - 4.5},
				point{hx + 3, hy - 16},
			)
			g.capsule(hx-13, hy-4.5, hx+13, hy-4.5, 1.3, 'Q')
			g.capsule(hx-9, hy-6.5, hx+9, hy-6.5, 0.6, 'Y')
			g.dot(hx+1, hy-11, 'A')
			g.dot(hx+4, hy-9, 'A')
			g.ell(hx-4, hy+3.5, 3, 1.2, 'w')
			g.ell(hx+2, hy+3.5, 3, 1.2, 'w')
		},
		chest: func(g *canvas, J, _ float64) {
			g.ell(21, 27+J, 4, 3.5, 'w')
			g.capsule(15, 34+J, 28, 34+J, 0.7, 'p')
		},
		front: func(g *canvas, J, t float64, _ pose) {
			g.capsule(7.5, 15+J, 7.5, 37+J, 0.8, 'H')
			g.ell(7.5, 12.5+J, 2.3, 2.3, 'A')
			if blinks(t, 4) {
				g.dot(4, 9+J, 'A')
				g.dot(11, 10+J, 'A')
			}
		},
	})
}

func drawKage(g *canvas, p pose, t float64) {
	human(g, p, t, outfit{
		skin: 'M', cloth: 'M', sleeve: 'n', hand: 'n', legs: 'n', boots: 'n',
		plain: true,
		back: func(g *canvas, J float64, p pose, t float64) {
			g.capsule(31, 13+J, 37, 6+J, 0.8, 'X')
			g.capsule(27, 25+J, 36+p.sway, 23+J+math.Sin(t*5), 1.5, 'R')
			g.capsule(27, 26+J, 34+p.sway, 28+J+math.Cos(t*5), 1.2, 'R')
		},
		hat: func(g *canvas, hx, hy, _ float64) {
			g.ell(hx-1.5, hy+0.5, 8.5, 3.4, 'S')
			g.capsule(hx-6, hy-6, hx+5, hy-6.5, 0.5, 'n')
		},
		chest: func(g *canvas, J, _ float64) {
			g.capsule(15, 25+J, 28, 25+J, 1.6, 'R')
			g.capsule(16, 32+J, 27, 32+J, 0.7, 'n')
		},
	})
}

func drawCorsair(g *canvas, p pose, t float64) {
	human(g, p, t, outfit{
		skin: 'S', cloth: 'V', sleeve: 'V', hand: 'S', legs: 'n', boots: 'D',
		patch: true,
		hat: func(g *canvas, hx, hy, _ float64) {
			g.ell(hx+8.5, hy, 2.4, 4.5, 'H')
			g.poly('V',
				point{hx - 14, hy - 4.5},
				point{hx - 9, hy - 11},
				point{hx, hy - 8.5},
				point{hx + 9, hy - 12},
				point{hx + 14, hy - 4.5},
			)
			g.capsule(hx-13, hy-4.5, hx+13, hy-4.5, 0.9, 'Y')
			g.ell(hx, hy-7, 1.4, 1.2, 'w')
			g.ell(hx-6, hy+0.5, 2.5, 2.5, 'K')
			g.capsule(hx-10, hy-2.5, hx+9, hy-3.5, 0.4, 'K')
		},
		chest: func(g *canvas, J, _ float64) {
			g.ell(21.5, 29+J, 2.4, 4.5, 'w')
			g.dot(18, 28+J, 'Y')
			g.dot(18, 31+J, 'Y')
			g.dot(25, 28+J, 'Y')
			g.dot(25, 31+J, 'Y')
		},
	})
}

func drawEmber(g *canvas, p pose, t float64) {
	J := -p.jump
	hx := 20 + p.hx
	hy := 16 + J + p.hy
	s := p.sq
	sw := p.sway * 2
	flap := math.Sin(t*6) * 2
	g.poly('F',
		point{26, 24 + J},
		point{39, 15 + J - flap},
		point{41, 21 + J - flap*0.5},
		point{35, 27 + J},
	)
	g.capsule(29, 33+J, 38, 34+J, 2.6, 'F')
	g.capsule(38, 34+J, 43+sw, 28+J, 2, 'F')
	g.poly('F',
		point{40 + sw, 28 + J},
		point{46.5 + sw, 27 + J},
		point{43.5 + sw, 21 + J},
	)
	g.poly('F',
		point{24, 23 + J},
		point{28, 20 + J},
		point{29, 25 + J},
	)
	g.poly('F',
		point{27, 27 + J},
		point{32, 25 + J},
		point{31, 30 + J},
	)
	g.ell(16, 37+J, 3.2, 1.8, 'F')
	g.ell(26, 37+J, 3.2, 1.8, 'F')
	g.ell(21, 30+J+s*2, 8+s*2, 7-s*2, 'd')
	g.ell(19.5, 31.5+J+s*2, 5+s, 4.5-s*1.5, 'C')
	frontLimb(g, p, t, J, 'F')
	g.ell(28, 29.5+J-p.arm, 2, 3, 'F')
	g.poly('C',
		point{hx - 10, hy - 6},
		point{hx - 12, hy - 14},
		point{hx - 5, hy - 8},
	)
	g.poly('C',
		point{hx + 10, hy - 6},
		point{hx + 12, hy - 14},
		point{hx + 5, hy - 8},
	)
	g.ell(hx, hy, 11, 9.5, 'd')
	g.ell(hx-3, hy-5, 6, 2.6, 'o')
	g.ell(hx-4, hy+4.8, 6.5, 3.8, 'o')
	g.ell(hx-6.5, hy+3.8, 0.6, 0.5, 'F')
	g.ell(hx-2.5, hy+3.8, 0.6, 0.5, 'F')
	g.mouth(hx-5.5, hy+6, p.mouth)
	g.eye(hx-7+p.ex, hy-0.5, p.eye)
	g.eye(hx+2.5+p.ex, hy-0.5, p.eye)
}

func drawNova(g *canvas, p pose, t float64) {
	human(g, p, t, outfit{
		skin: 'w', cloth: 'w', sleeve: 'X', hand: 'U', legs: 'X', boots: 'U',
		hat: func(g *canvas, hx, hy, t float64) {
			g.capsule(hx+5, hy-9, hx+8, hy-14, 0.6, 'X')
			beacon := byte('O')
			if blinks(t, 3) {
				beacon = 'A'
			}
			g.ell(hx+8.5, hy-14.5, 1.5, 1.5, beacon)
			g.ell(hx, hy, 11.5, 10.5, 'w')
			g.ell(hx-1.5, hy+0.8, 8.6, 6.6, 'u')
			g.capsule(hx+2, hy-4, hx+4.5, hy-2.5, 0.5, 'W')
		},
		chest: func(g *canvas, J, t float64) {
			g.ell(21.5, 29.5+J, 3.2, 2.6, 'U')
			light := byte('u')
			if blinks(t, 3) {
				light = 'A'
			}
			g.dot(21, 29+J, light)
			g.dot(22, 29+J, 'u')
		},
	})
}

func drawInky(g *canvas, p pose, t float64) {
	J := -p.jump
	hx := 22 + p.hx
	hy := 17 + J + p.hy
	s := p.sq
	for i := 0; i < 5; i++ {
		bx := float64(10 + i*6)
		tx := bx + math.Sin(t*3+float64(i)*1.3)*1.8
		g.capsule(bx, 28+J+s, tx, 36+J, 2, 'T')
		g.ell(tx, 37+J, 1.6, 1.2, 't')
	}
	if p.point {
		g.capsule(10, 26+J, 4, 24+J, 2, 'T')
	} else if p.wave {
		wy := math.Sin(t*10) * 2
		g.capsule(10, 24+J, 4.5, 16+J+wy, 1.8, 'T')
		g.ell(4.5, 15+J+wy, 1.6, 1.6, 't')
	}
	g.ell(hx, hy+2+s*2, 13+s, 12-s*2, 'T')
	g.ell(hx-4, hy-4.5+s*2, 6.5, 3.2, 't')
	g.ell(hx+7.5, hy-4+s*2, 1.6, 1.2, 't')
	ey := hy + 3 + s*2
	g.blush(hx-10, ey+3.5)
	g.blush(hx+6.5, ey+3.5)
	g.mouth(hx-3, ey+4, p.mouth)
	g.eye(hx-7+p.ex, ey, p.eye)
	g.eye(hx+3+p.ex, ey, p.eye)
}

// drawKeynote is the stage presenter: salt-and-pepper hair, a shiny black
// leather jacket over a dark tee, and a glowing chip in hand.
func drawKeynote(g *canvas, p pose, t float64) {
	human(g, p, t, outfit{
		skin: 'S', cloth: 'L', sleeve: 'L', hand: 'S', legs: 'n', boots: 'L',
		hat: func(g *canvas, hx, hy, _ float64) {
			// Short hair, swept back, with the hairline above the brows.
			g.ell(hx+1, hy-6.6, 10.4, 4.4, 'h')
			g.ell(hx+5.5, hy-5.8, 6.4, 4.4, 'h')
			g.ell(hx+9.4, hy-2, 1.5, 3, 'a')
			g.capsule(hx-7.5, hy-4.6, hx-1, hy-7.2, 0.9, 'h')
			// Tufts along the hairline so it reads as hair, not a cap.
			g.ell(hx-8.2, hy-3.6, 1.9, 1.6, 'h')
			g.ell(hx-5.2, hy-4.3, 2.1, 1.7, 'h')
			g.ell(hx-2, hy-4.7, 1.9, 1.5, 'h')
			g.capsule(hx-2, hy-9, hx+7, hy-8.6, 0.4, 'w')
			g.capsule(hx+2, hy-6.4, hx+9, hy-5.4, 0.35, 'a')
		},
		chest: func(g *canvas, J, t float64) {
			// Collar, dark tee in the open jacket, lapels, a drifting sheen.
			g.poly('L',
				point{16.5, 23.5 + J},
				point{19.5, 24.5 + J},
				point{18.5, 27 + J},
			)
			g.poly('L',
				point{26.5, 23.5 + J},
				point{23.5, 24.5 + J},
				point{24.5, 27 + J},
			)
			g.poly('n',
				point{18.5, 24.5 + J},
				point{24.5, 24.5 + J},
				point{22.6, 33 + J},
				point{20.4, 33 + J},
			)
			g.capsule(18.4, 24.8+J, 20.2, 31.5+J, 0.5, 'l')
			g.capsule(24.6, 24.8+J, 22.8, 31.5+J, 0.5, 'l')
			glint := math.Sin(t*1.3) * 1.2
			g.capsule(15.6, 26.5+J+glint, 16.6, 30.5+J+glint, 0.45, 'l')
			g.capsule(27.4, 26.5+J-glint, 26.6, 30+J-glint, 0.4, 'l')
		},
		front: func(g *canvas, J, t float64, p pose) {
			// The chip in the near hand, with pins and a soft pulsing glow.
			// It leaves the hand while the character points.
			if p.point {
				return
			}
			pulse := 0.35 + 0.25*math.Abs(math.Sin(t*3))
			// Follow the near hand, which human raises while waving.
			hand := nearHand(p, t, J)
			cx := hand.x - 0.7
			cy := hand.y + 1.9
			g.ell(cx, cy, 2.5+pulse, 2.1+pulse, 'c')
			for _, dx := range []float64{-1.1, 0, 1.1} {
				g.capsule(cx+dx, cy-1.9, cx+dx, cy+1.9, 0.22, 'X')
			}
			g.poly('n',
				point{cx - 1.7, cy - 1.3},
				point{cx + 1.7, cy - 1.3},
				point{cx + 1.7, cy + 1.3},
				point{cx - 1.7, cy + 1.3},
			)
			g.ell(cx, cy, 0.8, 0.55, 'c')
		},
	})
}
