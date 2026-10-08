package terminalpet

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"strings"
	"sync"
	"time"
)

const (
	DisabledID         = "disabled"
	DefaultManifestURL = "https://petdex.dev/api/manifest/v2"
	DefaultRankingURL  = "https://petdex.dev/api/pets/search?sort=installed&limit=60&includeMeta=0"
	TrustedAssetHost   = "assets.petdex.dev"
	previewFrameCount  = 6
	atlasColumns       = 8
)

type State string

const (
	Idle      State = "idle"
	MoveRight State = "running-right"
	MoveLeft  State = "running-left"
	Waving    State = "waving"
	Jumping   State = "jumping"
	Running   State = "running"
	Waiting   State = "waiting"
	Review    State = "review"
	Failed    State = "failed"
)

type Entry struct {
	Slug           string `json:"slug"`
	DisplayName    string `json:"displayName"`
	Kind           string `json:"kind,omitempty"`
	SubmittedBy    string `json:"submittedBy,omitempty"`
	SpritesheetURL string `json:"spritesheet"`
	PetJSONURL     string `json:"petJson,omitempty"`
	AssetBase      string `json:"assetBase,omitempty"`
	SpriteVersion  int    `json:"spriteVersionNumber"`
	Local          bool   `json:"-"`
}

func (e Entry) Label() string {
	if name := strings.TrimSpace(e.DisplayName); name != "" {
		return name
	}
	return e.Slug
}

type Animation struct {
	frames          map[State][]image.Image
	durations       map[State][]time.Duration
	loopStarts      map[State]int
	clickAnimations []State
	pngMu           sync.Mutex
	pngCache        map[frameCacheKey][]byte
}

type atlasTrack struct {
	row          int
	count        int
	durations    []time.Duration
	repeat       int
	loop         bool
	fallbackIdle bool
}

type frameCacheKey struct {
	state State
	index int
}

func (a *Animation) Frame(state State, phase int) image.Image {
	frame, _ := a.frame(state, phase)
	return frame
}

func (a *Animation) FrameDelay(state State, phase int) time.Duration {
	if a == nil {
		return 0
	}
	frames := a.frames[state]
	if len(frames) == 0 {
		state = Idle
		frames = a.frames[state]
	}
	if len(frames) == 0 {
		return 0
	}
	durations := a.durations[state]
	if len(durations) != len(frames) {
		return 0
	}
	return durations[a.frameIndex(state, phase, len(frames))]
}

// PrimaryDuration reports how long a non-idle action plays before it settles
// into its fallback loop. A zero duration means the state loops from its first
// frame or has no configured playback boundary.
func (a *Animation) PrimaryDuration(state State) time.Duration {
	if a == nil {
		return 0
	}
	loopStart, ok := a.loopStarts[state]
	durations := a.durations[state]
	if !ok || loopStart == 0 || loopStart > len(durations) || len(durations) == 0 {
		return 0
	}
	if loopStart < 0 {
		loopStart = len(durations)
	}
	var total time.Duration
	for _, duration := range durations[:loopStart] {
		total += duration
	}
	return total
}

// ClickDuration reports the duration of an authored click action.
// The boolean is false for ordinary activity states and built-in interactions.
func (a *Animation) ClickDuration(state State) (time.Duration, bool) {
	if a == nil {
		return 0, false
	}
	isClick := false
	for _, candidate := range a.clickAnimations {
		if candidate == state {
			isClick = true
			break
		}
	}
	if !isClick {
		return 0, false
	}
	return a.PrimaryDuration(state), true
}

func (a *Animation) ClickAnimation(index int) (State, bool) {
	if a == nil || len(a.clickAnimations) == 0 {
		return "", false
	}
	if index < 0 {
		index = -index
	}
	return a.clickAnimations[index%len(a.clickAnimations)], true
}

func (a *Animation) setClickAnimations(names []string) {
	if a == nil {
		return
	}
	a.clickAnimations = a.clickAnimations[:0]
	seen := make(map[State]bool, len(names))
	for _, name := range names {
		state := State(strings.ToLower(strings.TrimSpace(name)))
		if state == "" || seen[state] || len(a.frames[state]) == 0 {
			continue
		}
		seen[state] = true
		a.clickAnimations = append(a.clickAnimations, state)
	}
}

func (a *Animation) frame(state State, phase int) (image.Image, frameCacheKey) {
	if a == nil {
		return nil, frameCacheKey{}
	}
	frames := a.frames[state]
	if len(frames) == 0 {
		state = Idle
		frames = a.frames[Idle]
	}
	if len(frames) == 0 {
		return nil, frameCacheKey{}
	}
	index := a.frameIndex(state, phase, len(frames))
	return frames[index], frameCacheKey{state: state, index: index}
}

func (a *Animation) frameIndex(state State, phase, frameCount int) int {
	if frameCount < 1 {
		return 0
	}
	if phase < 0 {
		phase = -phase
	}
	loopStart, configured := a.loopStarts[state]
	if !configured {
		return phase % frameCount
	}
	if phase < frameCount {
		return phase
	}
	if loopStart >= 0 && loopStart < frameCount {
		return loopStart + (phase-loopStart)%(frameCount-loopStart)
	}
	return frameCount - 1
}

func (a *Animation) framePNG(state State, phase int) ([]byte, frameCacheKey, error) {
	frame, key := a.frame(state, phase)
	if frame == nil {
		return nil, key, fmt.Errorf("pet animation has no frames")
	}
	a.pngMu.Lock()
	defer a.pngMu.Unlock()
	if cached := a.pngCache[key]; len(cached) > 0 {
		return cached, key, nil
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, frame); err != nil {
		return nil, key, fmt.Errorf("encode pet frame: %w", err)
	}
	if a.pngCache == nil {
		a.pngCache = make(map[frameCacheKey][]byte)
	}
	value := append([]byte(nil), encoded.Bytes()...)
	a.pngCache[key] = value
	return value, key, nil
}

func PreviewAnimation(sheet image.Image) (*Animation, error) {
	if sheet == nil {
		return nil, fmt.Errorf("preview image is empty")
	}
	bounds := sheet.Bounds()
	if bounds.Dx() < previewFrameCount || bounds.Dx()%previewFrameCount != 0 || bounds.Dy() < 1 {
		return nil, fmt.Errorf("preview must contain %d equal-width frames", previewFrameCount)
	}
	frames := splitRow(sheet, previewFrameCount, 0, bounds.Dy())
	return &Animation{
		frames:    map[State][]image.Image{Idle: frames},
		durations: map[State][]time.Duration{Idle: repeatedDurations(len(frames), 140*time.Millisecond)},
	}, nil
}

func ThumbnailAnimation(imageValue image.Image) (*Animation, error) {
	if imageValue == nil || imageValue.Bounds().Empty() {
		return nil, fmt.Errorf("thumbnail image is empty")
	}
	return &Animation{frames: map[State][]image.Image{Idle: {imageValue}}}, nil
}

func AtlasAnimation(sheet image.Image, spriteVersion int) (*Animation, error) {
	return atlasAnimation(sheet, spriteVersion, nil)
}

func atlasAnimation(sheet image.Image, spriteVersion int, overrides map[State]atlasTrack) (*Animation, error) {
	if sheet == nil {
		return nil, fmt.Errorf("spritesheet is empty")
	}
	rows := 9
	if spriteVersion == 2 {
		rows = 11
	} else if spriteVersion != 0 && spriteVersion != 1 {
		return nil, fmt.Errorf("unsupported sprite version %d", spriteVersion)
	}
	bounds := sheet.Bounds()
	if bounds.Dx()%atlasColumns != 0 || bounds.Dy()%rows != 0 {
		return nil, fmt.Errorf("spritesheet must be an 8x%d grid", rows)
	}
	frameWidth := bounds.Dx() / atlasColumns
	frameHeight := bounds.Dy() / rows
	if frameWidth < 1 || frameHeight < 1 || frameWidth*208 != frameHeight*192 {
		return nil, fmt.Errorf("spritesheet frames must use the 192x208 aspect ratio")
	}
	stateRows := map[State]atlasTrack{
		Idle:      {row: 0, count: 6, durations: durations(1680, 660, 660, 840, 840, 1920), loop: true},
		MoveRight: {row: 1, count: 8, durations: finalFrameDurations(8, 120, 220), repeat: 3, fallbackIdle: true},
		MoveLeft:  {row: 2, count: 8, durations: finalFrameDurations(8, 120, 220), repeat: 3, fallbackIdle: true},
		Waving:    {row: 3, count: 4, durations: finalFrameDurations(4, 140, 280), repeat: 3, fallbackIdle: true},
		Jumping:   {row: 4, count: 5, durations: finalFrameDurations(5, 140, 280), repeat: 3, fallbackIdle: true},
		Failed:    {row: 5, count: 8, durations: finalFrameDurations(8, 140, 240), repeat: 3, fallbackIdle: true},
		Waiting:   {row: 6, count: 6, durations: finalFrameDurations(6, 150, 260), repeat: 3, fallbackIdle: true},
		Running:   {row: 7, count: 6, durations: finalFrameDurations(6, 120, 220), repeat: 3, fallbackIdle: true},
		Review:    {row: 8, count: 6, durations: finalFrameDurations(6, 150, 280), repeat: 3, fallbackIdle: true},
	}
	for state, override := range overrides {
		stateRows[state] = override
	}
	rawFrames := make(map[State][]image.Image, len(stateRows))
	for state, spec := range stateRows {
		if spec.row < 0 || spec.row >= rows || spec.count < 1 || spec.count > atlasColumns {
			return nil, fmt.Errorf("invalid %s animation geometry", state)
		}
		if len(spec.durations) == 0 {
			spec.durations = repeatedDurations(spec.count, 180*time.Millisecond)
			stateRows[state] = spec
		} else if len(spec.durations) != spec.count {
			return nil, fmt.Errorf("%s animation has %d durations for %d frames", state, len(spec.durations), spec.count)
		}
		rawFrames[state] = splitRow(sheet, atlasColumns, spec.row*frameHeight, frameHeight)[:spec.count]
	}
	idleFrames := rawFrames[Idle]
	idleDurations := stateRows[Idle].durations
	frames := make(map[State][]image.Image, len(stateRows))
	frameDurations := make(map[State][]time.Duration, len(stateRows))
	loopStarts := make(map[State]int, len(stateRows))
	for state, spec := range stateRows {
		repeat := max(1, spec.repeat)
		for range repeat {
			frames[state] = append(frames[state], rawFrames[state]...)
			if len(spec.durations) == spec.count {
				frameDurations[state] = append(frameDurations[state], spec.durations...)
			}
		}
		switch {
		case spec.fallbackIdle && state != Idle:
			loopStarts[state] = len(frames[state])
			frames[state] = append(frames[state], idleFrames...)
			frameDurations[state] = append(frameDurations[state], idleDurations...)
		case spec.loop:
			loopStarts[state] = 0
		default:
			loopStarts[state] = -1
		}
	}
	return &Animation{frames: frames, durations: frameDurations, loopStarts: loopStarts}, nil
}

func durations(milliseconds ...int) []time.Duration {
	result := make([]time.Duration, len(milliseconds))
	for index, value := range milliseconds {
		result[index] = time.Duration(value) * time.Millisecond
	}
	return result
}

func repeatedDurations(count int, duration time.Duration) []time.Duration {
	result := make([]time.Duration, count)
	for index := range result {
		result[index] = duration
	}
	return result
}

func finalFrameDurations(count, regularMilliseconds, finalMilliseconds int) []time.Duration {
	result := repeatedDurations(count, time.Duration(regularMilliseconds)*time.Millisecond)
	if len(result) > 0 {
		result[len(result)-1] = time.Duration(finalMilliseconds) * time.Millisecond
	}
	return result
}

func splitRow(sheet image.Image, count, y, height int) []image.Image {
	bounds := sheet.Bounds()
	width := bounds.Dx() / count
	frames := make([]image.Image, 0, count)
	for index := 0; index < count; index++ {
		frames = append(frames, cropImage{
			source: sheet,
			bounds: image.Rect(0, 0, width, height),
			offset: image.Pt(bounds.Min.X+index*width, bounds.Min.Y+y),
		})
	}
	return frames
}

type cropImage struct {
	source image.Image
	bounds image.Rectangle
	offset image.Point
}

func (c cropImage) ColorModel() color.Model { return c.source.ColorModel() }
func (c cropImage) Bounds() image.Rectangle { return c.bounds }
func (c cropImage) At(x, y int) color.Color {
	return c.source.At(x+c.offset.X, y+c.offset.Y)
}
