package terminalpet

import (
	"image"
	"image/color"
	"strings"
	"sync"
	"time"

	"github.com/Twigpine/zero/internal/terminalpet/petart"
)

// Built-in companions ship inside Zero: they are drawn by code, so they need
// no download and work offline.
//
// Their slugs carry a prefix that validateSlug rejects. A catalog entry or an
// installed directory can therefore never be mistaken for a built-in in a
// list of companions, even when a community pet has the same name.
const builtinSlugPrefix = "zero:"

// Extra states the built-in companions have. Dance and Lunge play when the
// companion is clicked. Thinking and Sleeping are agent states: pets without
// those animations show Running and Idle instead (see Animation.Has), and a
// community pet whose pet.json defines them gets them too.
const (
	Dance    State = "dance"
	Lunge    State = "lunge"
	Thinking State = "thinking"
	Sleeping State = "sleeping"
)

const (
	// TextColumns and TextRows are the terminal cells a built-in companion
	// fills when it is drawn as text, for terminals that cannot show images.
	TextColumns = 18
	TextRows    = 8

	// textScale draws the 48x40 design at 18x15 pixels. Half-block cells
	// stack two pixels each, and one empty pixel row on top leaves room for
	// a hop: 18x16 pixels in 18x8 cells.
	textScale    = 0.375
	textHeadroom = 1
)

// TextColors is the colour depth text frames are drawn for.
type TextColors uint8

const (
	// TextTrueColor draws with the art's exact 24-bit colours.
	TextTrueColor TextColors = iota
	// Text256 draws with a fixed 256-colour palette, the nearest colour to
	// each art colour by CIEDE2000, which keeps hues a generic conversion
	// would lose.
	Text256
)

const (
	// Built-in frames match the 192x208 frame of a pet atlas, so every code
	// path that sizes or places a companion treats both kinds the same way.
	// The character fills the full width; the rows above it are headroom for
	// hops.
	builtinFrameWidth  = 192
	builtinFrameHeight = 208
	builtinArtHeight   = builtinFrameWidth * petart.DesignHeight / petart.DesignWidth
	builtinHeadroom    = builtinFrameHeight - builtinArtHeight

	builtinAuthor = "Twigpine"

	// builtinBlinkSkip is long enough to clear the blink and the ear twitch
	// that the idle and waiting moods play at time zero.
	builtinBlinkSkip = 200 * time.Millisecond
)

// BuiltinSlug is the slug stored in config for a built-in companion.
func BuiltinSlug(form petart.Form) string {
	return builtinSlugPrefix + string(form)
}

func builtinForm(slug string) (petart.Form, bool) {
	name, ok := strings.CutPrefix(slug, builtinSlugPrefix)
	if !ok {
		return "", false
	}
	form := petart.Form(name)
	return form, form.Valid()
}

func builtinEntry(form petart.Form) Entry {
	return Entry{
		Slug:        BuiltinSlug(form),
		DisplayName: form.Name(),
		Kind:        form.Kind(),
		SubmittedBy: builtinAuthor,
		Local:       true,
		Builtin:     true,
	}
}

// BuiltinEntries lists the built-in companions in the order they are offered.
func BuiltinEntries() []Entry {
	forms := petart.Forms()
	entries := make([]Entry, len(forms))
	for index, form := range forms {
		entries[index] = builtinEntry(form)
	}
	return entries
}

// ResolveBuiltin finds a built-in companion by slug, id or display name,
// ignoring case and surrounding space.
func ResolveBuiltin(name string) (Entry, bool) {
	name = strings.ToLower(strings.TrimSpace(name))
	if form, ok := builtinForm(name); ok {
		return builtinEntry(form), true
	}
	if form, ok := petart.ParseForm(name); ok {
		return builtinEntry(form), true
	}
	return Entry{}, false
}

// builtinTrack describes how one state of a built-in companion is drawn.
type builtinTrack struct {
	mood   petart.Mood
	length time.Duration
	// start is the time of the first frame on the mood's clock. Moods that
	// blink do so at time zero, and the first frame is the one shown whenever
	// the track starts and the only one shown under reduced motion, so those
	// tracks start just after the blink.
	start time.Duration
	// loop repeats the track forever. Otherwise it plays once and settles
	// into the idle loop.
	loop bool
	// mirror faces the character the other way.
	mirror bool
}

var (
	// Track lengths follow the main motion of each mood (the idle breath,
	// the working bob, a whole number of hops), so loops close with little or
	// no visible jump and one-shot moods end with the feet on the ground.
	builtinTracks = map[State]builtinTrack{
		Idle:      {mood: petart.Idle, start: builtinBlinkSkip, length: 5750 * time.Millisecond, loop: true},
		Running:   {mood: petart.Working, length: 3100 * time.Millisecond, loop: true},
		Waiting:   {mood: petart.Waiting, start: builtinBlinkSkip, length: 2900 * time.Millisecond, loop: true},
		Thinking:  {mood: petart.Thinking, length: 6400 * time.Millisecond, loop: true},
		Sleeping:  {mood: petart.Sleeping, length: 5200 * time.Millisecond, loop: true},
		Review:    {mood: petart.Happy, length: 1900 * time.Millisecond},
		Failed:    {mood: petart.Sad, length: 1800 * time.Millisecond},
		Waving:    {mood: petart.Hello, length: 2400 * time.Millisecond},
		Jumping:   {mood: petart.Celebrate, length: 2300 * time.Millisecond},
		MoveLeft:  {mood: petart.Happy, length: 1300 * time.Millisecond, loop: true},
		MoveRight: {mood: petart.Happy, length: 1300 * time.Millisecond, loop: true, mirror: true},
		Dance:     {mood: petart.Dance, length: 4200 * time.Millisecond},
		Lunge:     {mood: petart.Action, length: petart.ActionCycle},
	}
	builtinClickStates = []string{string(Waving), string(Jumping), string(Dance), string(Lunge)}
)

var (
	builtinAnimationsMu sync.Mutex
	builtinAnimations   = map[petart.Form]*Animation{}
)

// BuiltinAnimation returns the animation for a built-in companion, or nil for
// an unknown form. The animation is shared by every caller. Its frames are
// drawn when they are shown, and what is kept is the encoded PNG, not the
// much larger picture it was made from.
func BuiltinAnimation(form petart.Form) *Animation {
	if !form.Valid() {
		return nil
	}
	builtinAnimationsMu.Lock()
	defer builtinAnimationsMu.Unlock()
	if animation := builtinAnimations[form]; animation != nil {
		return animation
	}
	animation := newBuiltinAnimation(form)
	builtinAnimations[form] = animation
	return animation
}

func newBuiltinAnimation(form petart.Form) *Animation {
	frames := make(map[State][]image.Image, len(builtinTracks))
	frameDurations := make(map[State][]time.Duration, len(builtinTracks))
	loopStarts := make(map[State]int, len(builtinTracks))
	trackFrames := func(track builtinTrack) ([]image.Image, []time.Duration) {
		delay := track.mood.FrameDelay()
		count := max(1, int(track.length/delay))
		images := make([]image.Image, count)
		for index := range images {
			images[index] = &builtinFrame{
				form: form, mood: track.mood, mirror: track.mirror,
				seconds: (track.start + time.Duration(index)*delay).Seconds(),
			}
		}
		return images, repeatedDurations(count, delay)
	}
	idleFrames, idleDurations := trackFrames(builtinTracks[Idle])
	for state, track := range builtinTracks {
		if state == Idle {
			frames[state], frameDurations[state], loopStarts[state] = idleFrames, idleDurations, 0
			continue
		}
		images, delays := trackFrames(track)
		if track.loop {
			frames[state], frameDurations[state], loopStarts[state] = images, delays, 0
			continue
		}
		// Play once, then carry on with the idle loop, as atlas pets do.
		loopStarts[state] = len(images)
		frames[state] = append(images, idleFrames...)
		frameDurations[state] = append(delays, idleDurations...)
	}
	animation := &Animation{frames: frames, durations: frameDurations, loopStarts: loopStarts}
	animation.setClickAnimations(builtinClickStates)
	return animation
}

// builtinFrame is one frame of a built-in companion, drawn on demand. The
// animation's frame tables are fixed when it is built, so they can be read
// without locking; only the drawing itself is deferred.
type builtinFrame struct {
	form    petart.Form
	mood    petart.Mood
	seconds float64
	mirror  bool

	mu     sync.Mutex
	pixels *image.NRGBA
	text   [2][]string
}

// draw paints the frame and keeps nothing. Callers cache what they make from
// it: framePNG keeps the encoded PNG, so a frame shown again is not redrawn.
func (f *builtinFrame) draw() *image.NRGBA {
	picture := petart.ImageWithHeadroom(f.form, f.mood, f.seconds, builtinArtHeight, builtinHeadroom)
	if f.mirror {
		mirrorNRGBA(picture)
	}
	return picture
}

// rendered backs pixel-by-pixel reads through At, which would otherwise
// redraw the frame for every pixel.
func (f *builtinFrame) rendered() *image.NRGBA {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pixels == nil {
		f.pixels = f.draw()
	}
	return f.pixels
}

// textLines draws the frame as terminal text, once per colour depth, and
// keeps it: a frame of text is a few kilobytes, and text frames are redrawn
// on every repaint.
func (f *builtinFrame) textLines(colors TextColors) []string {
	slot := 0
	if colors == Text256 {
		slot = 1
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.text[slot] == nil {
		frame := petart.RenderWithHeadroom(f.form, f.mood, f.seconds, textScale, textHeadroom)
		if f.mirror {
			frame.Mirror()
		}
		if slot == 1 {
			f.text[slot] = petart.HalfBlockLines256(frame)
		} else {
			f.text[slot] = petart.HalfBlockLines(frame)
		}
	}
	return f.text[slot]
}

// drawnFrame returns a picture an encoder can read directly: built-in frames
// are drawn now, and every other frame is returned as it is.
func drawnFrame(frame image.Image) image.Image {
	if builtin, ok := frame.(*builtinFrame); ok {
		return builtin.draw()
	}
	return frame
}

func (f *builtinFrame) ColorModel() color.Model { return color.NRGBAModel }

func (f *builtinFrame) Bounds() image.Rectangle {
	return image.Rect(0, 0, builtinFrameWidth, builtinFrameHeight)
}

func (f *builtinFrame) At(x, y int) color.Color { return f.rendered().NRGBAAt(x, y) }

// mirrorNRGBA flips an image left to right in place.
func mirrorNRGBA(img *image.NRGBA) {
	width := img.Rect.Dx()
	for y := 0; y < img.Rect.Dy(); y++ {
		row := img.Pix[y*img.Stride : y*img.Stride+width*4]
		for left, right := 0, width-1; left < right; left, right = left+1, right-1 {
			l, r := row[left*4:left*4+4], row[right*4:right*4+4]
			l[0], l[1], l[2], l[3], r[0], r[1], r[2], r[3] = r[0], r[1], r[2], r[3], l[0], l[1], l[2], l[3]
		}
	}
}
