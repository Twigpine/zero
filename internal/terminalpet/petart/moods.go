package petart

import (
	"math"
	"time"
)

// Mood is what a companion is doing: it sets the body language and the small
// badge drawn beside the head.
type Mood string

const (
	Idle      Mood = "idle"
	Hello     Mood = "hello"
	Thinking  Mood = "thinking"
	Working   Mood = "working"
	Waiting   Mood = "waiting"
	Happy     Mood = "happy"
	Celebrate Mood = "celebrate"
	Dance     Mood = "dance"
	Sad       Mood = "sad"
	Sleeping  Mood = "sleeping"
	// Action is a wind-up, a lunge toward the prompt and a pleased finish.
	Action Mood = "action"
)

var moods = []Mood{Idle, Hello, Thinking, Working, Waiting, Happy, Celebrate, Dance, Sad, Sleeping, Action}

// Moods returns every mood.
func Moods() []Mood {
	return append([]Mood(nil), moods...)
}

// Valid reports whether m is a known mood.
func (m Mood) Valid() bool {
	for _, mood := range moods {
		if mood == m {
			return true
		}
	}
	return false
}

// FrameDelay is how often a mood needs a new frame: calm moods redraw less
// often.
func (m Mood) FrameDelay() time.Duration {
	switch m {
	case Idle:
		return 250 * time.Millisecond
	case Sleeping:
		return 400 * time.Millisecond
	default:
		return 100 * time.Millisecond
	}
}

const (
	// ActionCycle is the length of the Action pose cycle.
	ActionCycle = 2600 * time.Millisecond

	actionCycleSeconds = 2.6
	actionWindup       = 0.18
	actionRelease      = 0.86
)

// noise is deterministic per-index noise for particles. There is no random
// source: frames must be reproducible.
func noise(i float64) float64 {
	v := math.Sin(i*127.1+311.7) * 43758.5453
	return v - math.Floor(v)
}

// sign01 maps a boolean to 1 or 0.
func sign01(on bool) float64 {
	if on {
		return 1
	}
	return 0
}

func poseFor(mood Mood, t float64) (p pose, flip bool) {
	p.sway = math.Sin(t * 1.8)
	blink := math.Mod(t, 3.4) < 0.14
	blinkEye := eyeOpen
	if blink {
		blinkEye = eyeShut
	}
	hop := func(rate, height float64) {
		j := math.Abs(math.Sin(t * rate))
		p.jump = j * height
		// Squash on landing, stretch at the top of the hop.
		p.sq = 0.55 - j*1.1
		p.hy = p.sq * 1.5
	}
	switch mood {
	case Hello:
		hop(4, 1.5)
		p.wave = true
		p.eye = blinkEye
		p.mouth = mouthSmile
		p.sway = math.Sin(t * 6)
	case Thinking:
		p.hx = -1
		p.hy = -1 + math.Sin(t*1.5)*0.5
		p.eye = eyeUp
		p.ex = sign01(math.Sin(t*0.9) > 0)
		p.sway = math.Sin(t * 0.9)
	case Working:
		p.hy = math.Abs(math.Sin(t*9)) * 1.3
		p.eye = eyeDown
		p.arm = math.Sin(t*18) * 1.6
		p.sway = math.Sin(t*4) * 0.5
	case Waiting:
		p.hx = jsRound(math.Sin(t * 2.2))
		p.hy = math.Sin(t*3) * 0.6
		p.eye = blinkEye
		p.arm = math.Max(0, math.Sin(t*8)) * 1.2
	case Happy:
		hop(5, 4.5)
		p.eye = eyeHappy
		p.sway = math.Sin(t * 10)
		p.mouth = mouthSmile
	case Celebrate:
		hop(5.5, 5.5)
		p.eye = eyeHappy
		p.mouth = mouthSmile
		p.sway = math.Sin(t * 12)
		p.arm = -3
		flip = int(math.Floor((t*5.5)/math.Pi))%2 == 1
	case Dance:
		beat := t * 6
		p.hx = math.Sin(beat) * 2
		p.hy = math.Abs(math.Sin(beat)) * 1.5
		p.sq = math.Abs(math.Sin(beat)) * 0.4
		p.arm = math.Sin(beat) * 3
		p.eye = eyeHappy
		p.mouth = mouthSmile
		p.sway = math.Sin(beat) * 1.5
		flip = int(math.Floor(beat/(math.Pi*4)))%2 == 1
	case Sad:
		if math.Mod(t, 1.6) < 0.5 {
			p.hx = math.Sin(t * 26)
		}
		p.hy = 1.5
		p.eye = eyeShut
		p.droop = 1
		p.sway = 0
		p.mouth = mouthFrown
	case Sleeping:
		p.sq = 0.8 + math.Sin(t*1.2)*0.25
		p.hy = 4 + math.Sin(t*1.2)*0.9
		p.eye = eyeShut
		p.sway = 0
		p.droop = 0.5
		p.mouth = mouthO
	case Action:
		u := math.Mod(t, actionCycleSeconds) / actionCycleSeconds
		switch {
		case u < actionWindup:
			// Wind up: lean back, arm raised.
			p.hx = 1.5 * (u / actionWindup)
			p.wave = true
			p.sq = 0.4 * (u / actionWindup)
		case u < actionRelease:
			// Release: lunge toward the prompt and point.
			p.hx = -1.5
			p.point = true
			p.mouth = mouthO
			p.sq = -0.2
		default:
			p.mouth = mouthSmile
			p.eye = eyeHappy
		}
	default: // Idle
		look := math.Mod(t, 6.2)
		p.hy = math.Sin(t*2.2) * 0.6
		p.sq = math.Sin(t*2.2) * 0.12
		p.eye = blinkEye
		switch {
		case look > 4.3 && look < 4.9:
			p.ex = -1
		case look > 4.9 && look < 5.5:
			p.ex = 1
		}
		p.twitch = sign01(math.Mod(t, 4.7) < 0.16)
	}
	return p, flip
}

// Badges sit in the top-right corner, beside the head. Small sprites get
// hand-placed pixel stamps; full-size art gets drawn shapes.
//
// Stamp coordinates are design units; at scale 0.5 every 2 units is one
// pixel, so x 36..46 is the sprite's right-hand 6 columns.
func compactBadge(g *canvas, mood Mood, t float64) {
	switch mood {
	case Thinking:
		n := int(math.Floor(t*2.5)) % 4
		if n >= 1 {
			g.stamp(36, 10, "w")
		}
		if n >= 2 {
			g.stamp(40, 6, "w")
		}
		if n >= 3 {
			g.stamp(42, 0, "ww", "ww")
		}
	case Working:
		if blinks(t, 8) {
			g.stamp(40, 0, ".A.", "AAA", ".A.")
		} else {
			g.stamp(40, 0, "A.A", ".A.", "A.A")
		}
	case Waiting:
		bounce := 0.0
		if jsRound(math.Sin(t*7)) > 0 {
			bounce = 2
		}
		g.stamp(44, bounce, "A", "A", ".", "A")
	case Happy:
		g.stamp(40, float64(4-(int(math.Floor(t*5))%3)*2), "R.R", "RRR", ".R.")
	case Hello:
		g.stamp(36, 0, "w...w", "ww...", "w.w.w")
	case Dance:
		bounce := jsRound(math.Sin(t*6)) * 2
		g.stamp(38, 2+bounce, ".p", ".p", "pp")
		g.stamp(43, 4-bounce, ".u", ".u", "uu")
	case Sad:
		g.stamp(38, math.Floor(math.Mod(t*9, 8))*2, "u", "u")
	case Sleeping:
		rise := math.Floor(math.Mod(t*1.2, 1)*3) * 2
		g.stamp(38, 8-rise, "ww.", ".w.", ".ww")
	}
}

func fullBadge(g *canvas, mood Mood, t float64) {
	switch mood {
	case Thinking:
		n := int(math.Floor(t*2.5)) % 4
		if n >= 1 {
			g.ell(36.5, 9.5, 1, 1, 'w')
		}
		if n >= 2 {
			g.ell(39.5, 6.5, 1.5, 1.5, 'w')
		}
		if n >= 3 {
			g.ell(43.5, 3.5, 2.2, 2.2, 'w')
		}
	case Working:
		if blinks(t, 8) {
			g.capsule(41, 4.5, 46, 4.5, 0.6, 'A')
			g.capsule(43.5, 2, 43.5, 7, 0.6, 'A')
		} else {
			g.capsule(41.5, 2.5, 45.5, 6.5, 0.6, 'A')
			g.capsule(45.5, 2.5, 41.5, 6.5, 0.6, 'A')
		}
	case Waiting:
		b := jsRound(math.Sin(t * 7))
		g.capsule(43.5, 2+b, 43.5, 6+b, 1.1, 'A')
		g.ell(43.5, 9.5+b, 1.2, 1.2, 'A')
	case Happy:
		y := 6 - math.Mod(t*5, 4)
		g.ell(42, y, 1.7, 1.7, 'R')
		g.ell(45, y, 1.7, 1.7, 'R')
		g.poly('R',
			point{40.3, y + 0.6},
			point{46.7, y + 0.6},
			point{43.5, y + 4.2},
		)
	case Hello:
		y := 3 + jsRound(math.Sin(t*6))
		g.capsule(38.5, y, 38.5, y+5, 0.5, 'w')
		g.capsule(39, y+3, 41, y+3, 0.5, 'w')
		g.capsule(41.5, y+3, 41.5, y+5, 0.5, 'w')
		g.dot(44, y, 'A')
		g.capsule(44.5, y+2, 44.5, y+5, 0.5, 'w')
	case Dance:
		note := func(x, y float64, key byte) {
			g.ell(x, y+4, 1.5, 1.2, key)
			g.capsule(x+1, y, x+1, y+4, 0.5, key)
			g.capsule(x+1, y, x+3, y+1, 0.5, key)
		}
		note(38, 3+math.Sin(t*6)*1.5, 'p')
		note(43, 7+math.Cos(t*6)*1.5, 'u')
	case Sad:
		g.ell(40.5, 3+math.Mod(t*9, 8), 1, 1.7, 'u')
	case Sleeping:
		k := math.Mod(t*1.2, 1)
		zee := func(x, y, size float64) {
			const w = 0.45
			g.capsule(x, y, x+size-1, y, w, 'w')
			g.capsule(x+size-1, y, x, y+size-1, w, 'w')
			g.capsule(x, y+size-1, x+size-1, y+size-1, w, 'w')
		}
		zee(37, 9-k*3, 3)
		zee(41, 5-k*3, 4)
	}
}

// particles belong to the scene, not the corner badge.
func particles(g *canvas, mood Mood, t float64) {
	switch {
	case mood == Celebrate:
		colours := [...]byte{'R', 'A', 'g', 'u', 'p', 'O'}
		for i := 0; i < 16; i++ {
			n := float64(i)
			x := noise(n)*47 + math.Sin(t*3+n)*1.5
			y := math.Mod(noise(n+40)*40+t*(14+noise(n+80)*12), 40)
			// Tiny round flakes: one pixel on the small sprite, discs at full size.
			g.ell(x, y, 0.6, 0.45+0.25*math.Abs(math.Sin(t*6+n)), colours[i%len(colours)])
		}
	case mood == Working:
		for i := 0; i < 4; i++ {
			n := float64(i)
			k := math.Mod(t*2.2+noise(n), 1)
			x := 11.0
			if i%2 != 0 {
				x = 31
			}
			key := byte('O')
			if k < 0.5 {
				key = 'A'
			}
			g.dot(x+(noise(n+5)-0.5)*5, 31-k*9, key)
		}
	case mood == Happy && math.Abs(math.Sin(t*5)) < 0.25:
		// Landing dust.
		g.dot(9, 37, 'X')
		g.dot(33, 37, 'X')
	}
}

func drawMoodExtras(g *canvas, mood Mood, t float64) {
	particles(g, mood, t)
	if g.compact {
		compactBadge(g, mood, t)
	} else {
		fullBadge(g, mood, t)
	}
}
