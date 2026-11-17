package terminalpet

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Twigpine/zero/internal/terminalpet/petart"
)

// communityEntries checks that a catalog leads with every built-in companion
// and returns the entries after them.
func communityEntries(t *testing.T, entries []Entry) []Entry {
	t.Helper()
	builtins := BuiltinEntries()
	if len(entries) < len(builtins) || !slices.Equal(entries[:len(builtins)], builtins) {
		t.Fatalf("catalog does not lead with the built-in companions: %v", entrySlugs(entries))
	}
	rest := entries[len(builtins):]
	for _, entry := range rest {
		if entry.Builtin {
			t.Fatalf("built-in companion %q listed after the community entries", entry.Slug)
		}
	}
	return rest
}

func TestBuiltinEntriesCannotCollideWithCatalogSlugs(t *testing.T) {
	entries := BuiltinEntries()
	if len(entries) != len(petart.Forms()) || entries[0].DisplayName != "Twig" {
		t.Fatalf("BuiltinEntries() = %v", entrySlugs(entries))
	}
	for _, entry := range entries {
		if !entry.Builtin || !entry.Local || entry.DisplayName == "" || entry.Kind == "" {
			t.Errorf("unexpected built-in entry: %#v", entry)
		}
		// A catalog or installed slug has to pass validateSlug, so a slug that
		// fails it can only ever be a built-in.
		if err := validateSlug(entry.Slug); err == nil {
			t.Errorf("built-in slug %q is a valid catalog slug", entry.Slug)
		}
	}
}

func TestResolveBuiltin(t *testing.T) {
	for _, input := range []string{"twig", "Twig", "  TWIG ", "zero:twig"} {
		if entry, ok := ResolveBuiltin(input); !ok || entry.Slug != "zero:twig" {
			t.Errorf("ResolveBuiltin(%q) = %#v, %v", input, entry, ok)
		}
	}
	for _, input := range []string{"", "boba", "zero:", "zero:boba", "zero:Twig extra"} {
		if entry, ok := ResolveBuiltin(input); ok {
			t.Errorf("ResolveBuiltin(%q) = %#v, want no match", input, entry)
		}
	}
}

func TestBuiltinCompanionsWorkWithoutNetworkOrDisk(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		t.Errorf("unexpected request for %s", request.URL.Path)
		http.NotFound(writer, request)
	}))
	root := t.TempDir()
	client := testClient(t, root, server)
	// Close it: anything that still tries the network now fails.
	server.Close()

	entries, err := client.Catalog(context.Background())
	if err == nil {
		t.Error("Catalog() hid the failure to load the community catalog")
	}
	if rest := communityEntries(t, entries); len(rest) != 0 {
		t.Errorf("offline catalog has community entries: %v", entrySlugs(rest))
	}
	installed, err := client.InstalledEntries()
	if err != nil || !slices.Equal(installed, BuiltinEntries()) {
		t.Fatalf("InstalledEntries() = %v, %v", entrySlugs(installed), err)
	}

	entry := installed[0]
	for name, load := range map[string]func() (*Animation, error){
		"Preview":       func() (*Animation, error) { return client.Preview(context.Background(), entry) },
		"Install":       func() (*Animation, error) { return client.Install(context.Background(), entry) },
		"LoadInstalled": func() (*Animation, error) { return client.LoadInstalled(entry.Slug) },
	} {
		animation, err := load()
		if err != nil || animation.Frame(Running, 3) == nil {
			t.Errorf("%s: animation=%v err=%v", name, animation, err)
		}
	}
	if got, err := client.InstalledEntry(entry.Slug); err != nil || got != entry {
		t.Errorf("InstalledEntry() = %#v, %v", got, err)
	}
	if _, err := client.LoadInstalled("zero:boba"); err == nil {
		t.Error("LoadInstalled accepted an unknown built-in")
	}
	// Choosing a built-in installs nothing: the client leaves the disk alone.
	if leftovers, _ := os.ReadDir(root); len(leftovers) != 0 {
		t.Errorf("built-in companions wrote to disk: %v", leftovers)
	}
}

func TestCatalogReportsUnreachableCommunityCatalogAlongsideInstalledPets(t *testing.T) {
	server := httptest.NewTLSServer(http.NotFoundHandler())
	root := t.TempDir()
	dir := filepath.Join(root, "pets", "installed", "kratos")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	metadata := `{"slug":"kratos","displayName":"Kratos","kind":"character","submittedBy":"tester","spriteVersionNumber":1}`
	if err := os.WriteFile(filepath.Join(dir, "source.json"), []byte(metadata), 0o600); err != nil {
		t.Fatal(err)
	}
	client := testClient(t, root, server)
	server.Close()

	entries, err := client.Catalog(context.Background())
	// Having something to list must not hide that the rest could not load.
	if err == nil {
		t.Error("Catalog() hid the failure to load the community catalog")
	}
	if rest := communityEntries(t, entries); len(rest) != 1 || rest[0].Slug != "kratos" || !rest[0].Local {
		t.Errorf("installed pet missing from the offline catalog: %v", entrySlugs(rest))
	}
}

func TestBuiltinTracksOpenWithEyesOpen(t *testing.T) {
	// The first frame is shown whenever a track starts, and is the only one
	// shown under reduced motion. The idle and waiting moods blink at time
	// zero, so a track that started there would greet every state change with
	// shut eyes and leave a still companion looking asleep.
	animation := BuiltinAnimation(petart.Twig)
	for state, mood := range map[State]petart.Mood{Idle: petart.Idle, Waiting: petart.Waiting} {
		first := animation.Frame(state, 0).(*builtinFrame).draw()
		blink := petart.ImageWithHeadroom(petart.Twig, mood, 0, builtinArtHeight, builtinHeadroom)
		open := petart.ImageWithHeadroom(petart.Twig, mood, builtinBlinkSkip.Seconds(), builtinArtHeight, builtinHeadroom)
		if bytes.Equal(blink.Pix, open.Pix) {
			t.Fatalf("%s: the mood no longer blinks at time zero, so this test proves nothing", state)
		}
		if bytes.Equal(first.Pix, blink.Pix) {
			t.Errorf("%s opens on the blink frame", state)
		}
		if !bytes.Equal(first.Pix, open.Pix) {
			t.Errorf("%s does not open just after the blink", state)
		}
	}
}

func TestBuiltinFramePicturesAreNotRetainedAfterEncoding(t *testing.T) {
	animation := newBuiltinAnimation(petart.Kage)
	if _, _, err := animation.framePNG(Running, 2); err != nil {
		t.Fatal(err)
	}
	// One drawn frame is about 160 KB and a companion has hundreds. The
	// renderer needs only the PNG it encoded.
	if frame := animation.Frame(Running, 2).(*builtinFrame); frame.pixels != nil {
		t.Error("encoding a frame kept its drawn picture")
	}
	if got := drawnFrame(animation.Frame(Running, 2)); got.Bounds() != image.Rect(0, 0, 192, 208) {
		t.Errorf("drawnFrame bounds = %v", got.Bounds())
	}
	// A frame that is already encoded is served from the cache without being
	// drawn again. Drawing one allocates its 160 KB picture, so a repeat
	// request that allocates almost nothing proves no draw happened.
	if allocs := testing.AllocsPerRun(5, func() {
		if _, _, err := animation.framePNG(Running, 2); err != nil {
			t.Fatal(err)
		}
	}); allocs > 2 {
		t.Errorf("a cached frame allocated %.0f times per request: it was drawn again", allocs)
	}
	plain := image.NewNRGBA(image.Rect(0, 0, 2, 2))
	if drawnFrame(plain) != image.Image(plain) {
		t.Error("drawnFrame changed a frame that is not built in")
	}
}

func TestInstalledPetNamedLikeABuiltinIsNotShadowed(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "pets", "installed", "twig")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	metadata := `{"slug":"twig","displayName":"Twig","kind":"bird","submittedBy":"tester","spriteVersionNumber":1}`
	if err := os.WriteFile(filepath.Join(dir, "source.json"), []byte(metadata), 0o600); err != nil {
		t.Fatal(err)
	}
	entries, err := NewClient(root).InstalledEntries()
	if err != nil {
		t.Fatal(err)
	}
	rest := communityEntries(t, entries)
	if len(rest) != 1 || rest[0].Slug != "twig" || rest[0].Builtin || rest[0].Kind != "bird" {
		t.Fatalf("installed pet was lost or replaced: %#v", rest)
	}
	if entries[0].Slug != "zero:twig" || !entries[0].Builtin {
		t.Fatalf("built-in Twig is missing: %#v", entries[0])
	}
}

func TestBuiltinAnimationCoversEveryState(t *testing.T) {
	animation := BuiltinAnimation(petart.Twig)
	if animation == nil || BuiltinAnimation(petart.Twig) != animation {
		t.Fatal("BuiltinAnimation should return one shared animation per form")
	}
	if BuiltinAnimation("boba") != nil {
		t.Fatal("BuiltinAnimation returned an animation for an unknown form")
	}
	frameSize := image.Rect(0, 0, 192, 208)
	for _, state := range []State{Idle, MoveRight, MoveLeft, Waving, Jumping, Running, Waiting, Review, Failed, Dance, Lunge, Thinking, Sleeping} {
		frame := animation.Frame(state, 0)
		// The frame must match a pet atlas frame, which the layout assumes.
		if frame == nil || frame.Bounds() != frameSize {
			t.Fatalf("%s: frame = %v", state, frame)
		}
		if animation.FrameDelay(state, 0) <= 0 {
			t.Errorf("%s: no frame delay", state)
		}
	}
	// Loops never settle; one-shot states play for their length and then idle.
	for _, state := range []State{Idle, Running, Waiting, MoveLeft, MoveRight, Thinking, Sleeping} {
		if got := animation.PrimaryDuration(state); got != 0 {
			t.Errorf("%s should loop, PrimaryDuration = %v", state, got)
		}
	}
	for state, want := range map[State]time.Duration{
		Review: 1900 * time.Millisecond, Failed: 1800 * time.Millisecond, Waving: 2400 * time.Millisecond,
		Jumping: 2300 * time.Millisecond, Dance: 4200 * time.Millisecond, Lunge: 2600 * time.Millisecond,
	} {
		if got := animation.PrimaryDuration(state); got != want {
			t.Errorf("%s: PrimaryDuration = %v, want %v", state, got, want)
		}
	}
	// After a one-shot state the frames are the idle loop's own.
	settled := int(1900*time.Millisecond/(100*time.Millisecond)) + 2
	if animation.Frame(Review, settled) != animation.Frame(Idle, 2) {
		t.Error("Review does not settle into the idle loop")
	}

	var clicks []State
	for index := 0; index < 5; index++ {
		state, ok := animation.ClickAnimation(index)
		if !ok {
			t.Fatal("built-in companions should have click animations")
		}
		clicks = append(clicks, state)
	}
	if want := []State{Waving, Jumping, Dance, Lunge, Waving}; !slices.Equal(clicks, want) {
		t.Errorf("click animations = %v, want %v", clicks, want)
	}
	if duration, ok := animation.ClickDuration(Dance); !ok || duration != 4200*time.Millisecond {
		t.Errorf("ClickDuration(Dance) = %v, %v", duration, ok)
	}
}

func TestBuiltinFramesDrawTheCharacterWithHeadroom(t *testing.T) {
	animation := BuiltinAnimation(petart.Merlin)
	data, _, err := animation.framePNG(Idle, 0)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Bounds() != image.Rect(0, 0, 192, 208) {
		t.Fatalf("frame PNG is %v", decoded.Bounds())
	}
	if again, _, _ := animation.framePNG(Idle, 0); &again[0] != &data[0] {
		t.Error("frame PNG was encoded twice")
	}
	opaqueRows := func(img image.Image) (top, bottom int) {
		top, bottom = -1, -1
		for y := 0; y < img.Bounds().Dy(); y++ {
			for x := 0; x < img.Bounds().Dx(); x++ {
				if _, _, _, alpha := img.At(x, y).RGBA(); alpha == 0xffff {
					if top < 0 {
						top = y
					}
					bottom = y
					break
				}
			}
		}
		return top, bottom
	}
	top, bottom := opaqueRows(decoded)
	// Standing, the character leaves the headroom empty and reaches the floor.
	if top < 30 || bottom < 195 {
		t.Errorf("idle frame spans rows %d..%d, want headroom above and feet near the bottom", top, bottom)
	}
	// Mid-hop the hat rises into the headroom instead of being cut off at
	// the top edge: the picture's first row stays clear.
	highest := 208
	for phase := 0; phase < 23; phase++ {
		hop := animation.Frame(Jumping, phase)
		if top, _ := opaqueRows(hop); top < highest {
			highest = top
		}
	}
	if highest >= top || highest <= 0 {
		t.Errorf("highest hop reaches row %d (standing %d): want it inside the headroom, clear of the top edge", highest, top)
	}

	// Dragging right shows the mirror image of dragging left.
	left, right := animation.Frame(MoveLeft, 3), animation.Frame(MoveRight, 3)
	for _, at := range []image.Point{{40, 120}, {96, 150}, {150, 100}, {20, 190}} {
		if left.At(at.X, at.Y) != right.At(191-at.X, at.Y) {
			t.Errorf("MoveRight is not MoveLeft mirrored at %v", at)
		}
	}
}

func TestBuiltinTextFramesFitTheTextSlot(t *testing.T) {
	sgr := regexp.MustCompile(`\x1b\[[0-9;]*m`)
	states := []State{Idle, MoveRight, MoveLeft, Waving, Jumping, Running, Waiting, Review, Failed, Dance, Lunge, Thinking, Sleeping}
	for _, form := range petart.Forms() {
		animation := BuiltinAnimation(form)
		if !animation.HasText() {
			t.Fatalf("%s has no text form", form)
		}
		for _, state := range states {
			for _, phase := range []int{0, 3, 17} {
				for _, colors := range []TextColors{TextTrueColor, Text256} {
					lines, ok := animation.TextFrame(state, phase, colors)
					if !ok || len(lines) != TextRows {
						t.Fatalf("%s/%s phase %d: %d text lines, ok=%v", form, state, phase, len(lines), ok)
					}
					drawn := 0
					for index, line := range lines {
						cells := []rune(sgr.ReplaceAllString(line, ""))
						if len(cells) != TextColumns {
							t.Fatalf("%s/%s line %d is %d cells wide, want %d", form, state, index, len(cells), TextColumns)
						}
						for _, cell := range cells {
							if cell != ' ' {
								drawn++
							}
						}
					}
					if drawn < 40 {
						t.Errorf("%s/%s phase %d: only %d cells drawn", form, state, phase, drawn)
					}
				}
			}
		}
	}

	// Each colour depth uses its own codes, for the same picture.
	truecolor, _ := BuiltinAnimation(petart.Twig).TextFrame(Idle, 0, TextTrueColor)
	limited, _ := BuiltinAnimation(petart.Twig).TextFrame(Idle, 0, Text256)
	if !strings.Contains(strings.Join(truecolor, ""), ";2;") || strings.Contains(strings.Join(limited, ""), ";2;") ||
		!strings.Contains(strings.Join(limited, ""), "38;5;") {
		t.Error("text frames do not use the colour codes of their depth")
	}
	if sgr.ReplaceAllString(strings.Join(truecolor, "\n"), "") != sgr.ReplaceAllString(strings.Join(limited, "\n"), "") {
		t.Error("the 256-colour frame draws a different picture")
	}

	animation := BuiltinAnimation(petart.Twig)
	first, _ := animation.TextFrame(Running, 2, TextTrueColor)
	again, _ := animation.TextFrame(Running, 2, TextTrueColor)
	if &first[0] != &again[0] {
		t.Error("a text frame was drawn twice")
	}
	// The top pixel row is headroom: tall parts such as Robin's feather reach
	// it, and a hop rises into it instead of being cut off. A cell whose top
	// pixel is drawn is an upper half block, so Merlin standing has none on
	// the first line and has some at the top of a hop.
	if standing, _ := BuiltinAnimation(petart.Merlin).TextFrame(Idle, 0, TextTrueColor); strings.Contains(sgr.ReplaceAllString(standing[0], ""), "▀") {
		t.Errorf("Merlin standing fills the headroom: %q", sgr.ReplaceAllString(standing[0], ""))
	}
	hopped := false
	for phase := 0; phase < 23 && !hopped; phase++ {
		frame, _ := BuiltinAnimation(petart.Merlin).TextFrame(Jumping, phase, TextTrueColor)
		hopped = strings.Contains(sgr.ReplaceAllString(frame[0], ""), "▀")
	}
	if !hopped {
		t.Error("Merlin never rises into the headroom while celebrating")
	}
	// Dragging right shows the mirror image of dragging left.
	left, _ := animation.TextFrame(MoveLeft, 3, TextTrueColor)
	right, _ := animation.TextFrame(MoveRight, 3, TextTrueColor)
	if strings.Join(left, "\n") == strings.Join(right, "\n") {
		t.Error("MoveRight text is not mirrored")
	}
}

func TestPictureAnimationsHaveNoTextOrExtraStates(t *testing.T) {
	sheet, err := png.Decode(bytes.NewReader(encodedPNG(t, 24*atlasColumns, 26*9)))
	if err != nil {
		t.Fatal(err)
	}
	atlas, err := AtlasAnimation(sheet, 1)
	if err != nil {
		t.Fatal(err)
	}
	if atlas.HasText() {
		t.Error("an atlas pet reports a text form")
	}
	if lines, ok := atlas.TextFrame(Idle, 0, TextTrueColor); ok || lines != nil {
		t.Errorf("TextFrame for an atlas pet = %v, %v", lines, ok)
	}
	// Agents fall back to Running and Idle for pets without these states.
	if atlas.Has(Thinking) || atlas.Has(Sleeping) || !atlas.Has(Running) || !atlas.Has(Idle) {
		t.Errorf("atlas Has: thinking=%v sleeping=%v running=%v idle=%v",
			atlas.Has(Thinking), atlas.Has(Sleeping), atlas.Has(Running), atlas.Has(Idle))
	}
	var none *Animation
	if none.Has(Idle) || none.HasText() {
		t.Error("a nil animation reports frames")
	}
	if !BuiltinAnimation(petart.Ember).Has(Thinking) || !BuiltinAnimation(petart.Ember).Has(Sleeping) {
		t.Error("built-in companions lack the thinking and sleeping states")
	}
}
