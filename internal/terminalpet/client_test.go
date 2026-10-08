package terminalpet

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCatalogPreviewInstallAndOfflineReload(t *testing.T) {
	preview := encodedPNG(t, 24*previewFrameCount, 26)
	atlas := encodedPNG(t, 24*atlasColumns, 26*11)
	var assetBase atomic.Pointer[string]
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/manifest":
			base := assetBase.Load()
			if base == nil {
				http.Error(writer, "server not ready", http.StatusServiceUnavailable)
				return
			}
			manifest := compactManifest{
				Version:   2,
				AssetBase: *base,
				Fields:    []string{"slug", "displayName", "kind", "submittedBy", "spritesheet", "petJson", "zip", "spriteVersionNumber"},
			}
			row := []any{"boba", "Boba", "animal", "tester", "pets/boba/sprite.webp", "pets/boba/petjson.json", "pets/boba/archive.zip", 2}
			encoded, _ := json.Marshal(row)
			manifest.Pets = []json.RawMessage{encoded}
			_ = json.NewEncoder(writer).Encode(manifest)
		case "/pets/boba/preview.webp":
			_, _ = writer.Write(preview)
		case "/pets/boba/sprite.webp":
			_, _ = writer.Write(atlas)
		case "/pets/boba/petjson.json":
			_, _ = writer.Write([]byte(`{"displayName":"Boba","description":"A calm blue-screen gremlin."}`))
		case "/ranking":
			_, _ = writer.Write([]byte(`{"pets":[{"slug":"boba"}],"nextCursor":null}`))
		default:
			http.NotFound(writer, request)
		}
	}))
	serverURL := server.URL
	assetBase.Store(&serverURL)
	defer server.Close()

	root := t.TempDir()
	client := testClient(t, root, server)
	entries, err := client.Catalog(context.Background())
	if err != nil || len(entries) != 1 {
		t.Fatalf("Catalog() = %#v, %v", entries, err)
	}
	entry := entries[0]
	if entry.Slug != "boba" || entry.SpriteVersion != 2 || entry.AssetBase != server.URL {
		t.Fatalf("unexpected catalog entry: %#v", entry)
	}
	if entry.SpritesheetURL != server.URL+"/pets/boba/sprite.webp" {
		t.Fatalf("relative spritesheet was not resolved: %q", entry.SpritesheetURL)
	}
	previewAnimation, err := client.Preview(context.Background(), entry)
	if err != nil || previewAnimation.Frame(Idle, 5) == nil {
		t.Fatalf("Preview() animation=%v err=%v", previewAnimation, err)
	}
	installed, err := client.Install(context.Background(), entry)
	if err != nil || installed.Frame(Running, 3) == nil {
		t.Fatalf("Install() animation=%v err=%v", installed, err)
	}
	if _, err := os.Stat(filepath.Join(root, "pets", "installed", "boba", "source.json")); err != nil {
		t.Fatalf("installed source marker: %v", err)
	}
	server.Close()
	offlineEntries, err := client.Catalog(context.Background())
	if err != nil || len(offlineEntries) != 1 || !offlineEntries[0].Local {
		t.Fatalf("offline Catalog() = %#v, %v", offlineEntries, err)
	}
	if _, err := client.LoadInstalled("boba"); err != nil {
		t.Fatalf("LoadInstalled: %v", err)
	}
}

func TestInstallUsesPetMetadataSpriteVersionWhenManifestIsStale(t *testing.T) {
	atlas := encodedPNG(t, 24*atlasColumns, 26*11)
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/pets/luffy/sprite.webp":
			_, _ = writer.Write(atlas)
		case "/pets/luffy/petjson.json":
			_, _ = writer.Write([]byte(`{
				"spriteVersionNumber":2,
				"atlasRows":11,
				"interactions":{"click":{"animations":["bajrang-gun"],"mode":"cycle"}},
				"animations":{"bajrang-gun":{"sourceRowIndex":3,"frameCount":8,"timingMs":[100,110,120,130,140,150,160,320]}}
			}`))
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	client := testClient(t, t.TempDir(), server)
	entry := Entry{
		Slug:           "luffy",
		DisplayName:    "Luffy Gear 5",
		SpritesheetURL: server.URL + "/pets/luffy/sprite.webp",
		PetJSONURL:     server.URL + "/pets/luffy/petjson.json",
		SpriteVersion:  1,
	}
	animation, err := client.Install(context.Background(), entry)
	if err != nil {
		t.Fatalf("Install() with corrected pet metadata: %v", err)
	}
	if animation.Frame(Review, 0) == nil {
		t.Fatal("Install() did not load the version 2 review row")
	}
	customState := State("bajrang-gun")
	if _, key := animation.frame(customState, 7); key.index != 7 {
		t.Fatalf("custom waving final frame index = %d, want 7", key.index)
	}
	if got := animation.FrameDelay(customState, 7); got != 320*time.Millisecond {
		t.Fatalf("custom waving final frame delay = %s, want 320ms", got)
	}
	if got := animation.PrimaryDuration(customState); got != 1230*time.Millisecond {
		t.Fatalf("custom waving duration = %s, want unchanged 1.23s", got)
	}
	if got := animation.FrameDelay(customState, 8); got != 1680*time.Millisecond {
		t.Fatalf("custom one-shot fallback delay = %s, want first idle frame at 1.68s", got)
	}
	if got, ok := animation.ClickAnimation(0); !ok || got != customState {
		t.Fatalf("click animation = %q, %v; want %q", got, ok, customState)
	}
	installed, err := client.InstalledEntry(entry.Slug)
	if err != nil {
		t.Fatal(err)
	}
	if installed.SpriteVersion != 2 {
		t.Fatalf("persisted sprite version = %d, want 2", installed.SpriteVersion)
	}
	if _, err := client.LoadInstalled(entry.Slug); err != nil {
		t.Fatalf("LoadInstalled() with corrected sprite version: %v", err)
	}
}

func TestPetMetadataAllowsSeveralNamedAnimationsOnOneRow(t *testing.T) {
	row := 0
	document := petMetadata{Animations: map[string]petAnimationMetadata{
		"blink": {SourceRowIndex: &row, FrameCount: 4, TimingMS: []int{100, 100, 100, 100}},
		"idle":  {SourceRow: json.RawMessage(`"idle"`), FrameCount: 6, TimingMS: []int{200, 200, 200, 200, 200, 400}},
	}}
	tracks, err := document.atlasTracks()
	if err != nil {
		t.Fatal(err)
	}
	if got := tracks[Idle].count; got != 6 {
		t.Fatalf("idle frame count = %d, want six", got)
	}
	if got := tracks[State("blink")].count; got != 4 {
		t.Fatalf("blink frame count = %d, want four", got)
	}
}

func TestPetMetadataAlwaysLoopsIdleAnimation(t *testing.T) {
	row := 0
	document := petMetadata{Animations: map[string]petAnimationMetadata{
		"idle": {
			SourceRowIndex: &row,
			FrameCount:     2,
			TimingMS:       []int{100, 200},
			Playback:       "once",
		},
	}}
	tracks, err := document.atlasTracks()
	if err != nil {
		t.Fatal(err)
	}
	idle := tracks[Idle]
	if !idle.loop || idle.fallbackIdle {
		t.Fatalf("idle track = %#v, want looping without fallback", idle)
	}
}

func TestPetMetadataAcceptsNamedAndNumericSourceRows(t *testing.T) {
	document := petMetadata{Animations: map[string]petAnimationMetadata{
		"dash":  {SourceRow: json.RawMessage(`"running-right"`), FrameCount: 8},
		"skill": {SourceRow: json.RawMessage(`3`), FrameCount: 8},
	}}
	tracks, err := document.atlasTracks()
	if err != nil {
		t.Fatal(err)
	}
	if tracks[State("dash")].row != 1 || tracks[State("skill")].row != 3 {
		t.Fatalf("normalized rows = dash:%d skill:%d", tracks[State("dash")].row, tracks[State("skill")].row)
	}
}

func TestCatalogRejectsUntrustedAssetHost(t *testing.T) {
	client := NewClient(t.TempDir())
	manifest := compactManifest{
		Version:   2,
		AssetBase: "https://evil.example",
		Fields:    []string{"slug", "displayName", "kind", "submittedBy", "spritesheet", "petJson", "spriteVersionNumber"},
	}
	row, _ := json.Marshal([]any{"boba", "Boba", "animal", nil, "https://evil.example/pets/boba/sprite.webp", "", 2})
	manifest.Pets = []json.RawMessage{row}
	data, _ := json.Marshal(manifest)
	if _, err := client.decodeCatalog(data); err == nil {
		t.Fatal("decodeCatalog accepted an untrusted asset host")
	}
}

func TestCatalogSkipsMalformedRowWhenValidRowsRemain(t *testing.T) {
	client := NewClient(t.TempDir())
	manifest := compactManifest{
		Version:   2,
		AssetBase: "https://assets.petdex.dev",
		Fields:    []string{"slug", "displayName", "kind", "submittedBy", "spritesheet", "petJson", "spriteVersionNumber"},
	}
	invalid, _ := json.Marshal([]any{"broken slug", "Broken", "creature", "tester", "pets/broken/sprite.webp", "", 2})
	valid, _ := json.Marshal([]any{"boba", "Boba", "creature", "tester", "pets/boba/sprite.webp", "", 2})
	manifest.Pets = []json.RawMessage{invalid, valid}
	data, _ := json.Marshal(manifest)

	entries, err := client.decodeCatalog(data)
	if err != nil {
		t.Fatal(err)
	}
	if got := entrySlugs(entries); !slices.Equal(got, []string{"boba"}) {
		t.Fatalf("catalog entries = %v, want [boba]", got)
	}
}

func TestCatalogOrdersRankedPetsBeforeAlphabeticalRemainder(t *testing.T) {
	manifest := compactManifest{
		Version:   2,
		AssetBase: "",
		Fields:    []string{"slug", "displayName", "kind", "submittedBy", "spritesheet", "petJson", "zip", "spriteVersionNumber"},
	}
	for _, row := range [][]any{
		{"alpha", "Alpha", "creature", "tester", "pets/alpha/sprite.webp", "pets/alpha/pet.json", nil, 1},
		{"beta", "Beta", "creature", "tester", "pets/beta/sprite.webp", "pets/beta/pet.json", nil, 1},
		{"zeta", "Zeta", "creature", "tester", "pets/zeta/sprite.webp", "pets/zeta/pet.json", nil, 1},
	} {
		encoded, _ := json.Marshal(row)
		manifest.Pets = append(manifest.Pets, encoded)
	}
	var rankingFails atomic.Bool
	var assetBase atomic.Pointer[string]
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/manifest":
			base := assetBase.Load()
			if base == nil {
				http.Error(writer, "server not ready", http.StatusServiceUnavailable)
				return
			}
			manifest.AssetBase = *base
			_ = json.NewEncoder(writer).Encode(manifest)
		case "/ranking":
			if rankingFails.Load() {
				http.Error(writer, "unavailable", http.StatusServiceUnavailable)
				return
			}
			_, _ = writer.Write([]byte(`{"pets":[{"slug":"zeta"},{"slug":"alpha"}],"nextCursor":60}`))
		default:
			http.NotFound(writer, request)
		}
	}))
	serverURL := server.URL
	assetBase.Store(&serverURL)
	defer server.Close()

	client := testClient(t, t.TempDir(), server)
	entries, err := client.Catalog(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := entrySlugs(entries); !slices.Equal(got, []string{"zeta", "alpha", "beta"}) {
		t.Fatalf("ranked catalog = %v", got)
	}

	rankingFails.Store(true)
	fallback := testClient(t, t.TempDir(), server)
	entries, err = fallback.Catalog(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := entrySlugs(entries); !slices.Equal(got, []string{"alpha", "beta", "zeta"}) {
		t.Fatalf("fallback catalog = %v", got)
	}
}

func TestAssetPathAndSlugValidation(t *testing.T) {
	client := NewClient(t.TempDir())
	for _, raw := range []string{
		"https://assets.petdex.dev/other/sprite.webp",
		"https://assets.petdex.dev/pets/%2e%2e/secret",
		"https://user@assets.petdex.dev/pets/boba/sprite.webp",
	} {
		if _, err := client.trustedAssetURL(raw); err == nil {
			t.Errorf("trustedAssetURL(%q) unexpectedly succeeded", raw)
		}
	}
	for _, slug := range []string{"../boba", "/boba", "Boba", "boba/other", ""} {
		if err := validateSlug(slug); err == nil {
			t.Errorf("validateSlug(%q) unexpectedly succeeded", slug)
		}
	}
}

func TestAssetRedirectToUntrustedHostIsRejectedBeforeFollowing(t *testing.T) {
	redirect := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, "https://example.com/pets/boba/sprite.webp", http.StatusFound)
	}))
	defer redirect.Close()
	parsed, _ := url.Parse(redirect.URL)
	client := NewClient(t.TempDir())
	client.HTTPClient = redirect.Client()
	client.TrustedHosts = map[string]bool{parsed.Hostname(): true}
	client.TrustedAssetHosts = map[string]bool{parsed.Hostname(): true}
	if _, err := client.fetchAsset(context.Background(), redirect.URL+"/pets/boba/sprite.webp", maxSpriteBytes); err == nil {
		t.Fatal("fetchAsset followed a redirect to an untrusted host")
	}
}

func TestResolveAssetURLRejectsPathsOutsideCatalogRoots(t *testing.T) {
	client := NewClient(t.TempDir())
	for _, reference := range []string{"../secret.png", "/other/sprite.webp", `pets\\boba\\sprite.webp`} {
		if _, err := client.resolveAssetURL("https://assets.petdex.dev", reference); err == nil {
			t.Errorf("resolveAssetURL(%q) unexpectedly succeeded", reference)
		}
	}
}

func TestFetchAssetRejectsOversizedDownload(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte("12345"))
	}))
	defer server.Close()
	client := testClient(t, t.TempDir(), server)
	if _, err := client.fetchAsset(context.Background(), server.URL+"/pets/boba/sprite.webp", 4); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized fetch error = %v", err)
	}
}

func TestDecodeImageRejectsOversizedDimensions(t *testing.T) {
	data := encodedPNG(t, maxImageSide+1, 1)
	if _, err := decodeImage(data); err == nil || !strings.Contains(err.Error(), "dimensions") {
		t.Fatalf("oversized image error = %v", err)
	}
}

func TestAtlasAnimationRejectsWrongGeometry(t *testing.T) {
	bad := image.NewNRGBA(image.Rect(0, 0, 192, 285))
	if _, err := AtlasAnimation(bad, 2); err == nil {
		t.Fatal("AtlasAnimation accepted a non-grid image")
	}
	if _, err := AtlasAnimation(image.NewNRGBA(image.Rect(0, 0, 192, 286)), 3); err == nil {
		t.Fatal("AtlasAnimation accepted an unknown sprite version")
	}
}

func TestAtlasAnimationUsesPerStateFrameCounts(t *testing.T) {
	atlas := image.NewNRGBA(image.Rect(0, 0, 192, 26*9))
	for column := 0; column < 6; column++ {
		atlas.SetNRGBA(column*24, 0, color.NRGBA{R: 255, A: 255})
	}
	animation, err := AtlasAnimation(atlas, 1)
	if err != nil {
		t.Fatal(err)
	}
	// Idle has six frames: phase 6 wraps to frame zero instead of entering the
	// two unused atlas columns, which are commonly transparent.
	_, _, _, alpha := animation.Frame(Idle, 6).At(0, 0).RGBA()
	if alpha == 0 {
		t.Fatal("idle phase 6 selected an unused transparent atlas column")
	}
}

func TestAtlasAnimationRunningRepeatsThreeTimesThenLoopsIdle(t *testing.T) {
	atlas := image.NewNRGBA(image.Rect(0, 0, 192, 26*9))
	for x := 0; x < 6*24; x++ {
		for y := 0; y < 26; y++ {
			atlas.SetNRGBA(x, y, color.NRGBA{R: 255, A: 255})
			atlas.SetNRGBA(x, 7*26+y, color.NRGBA{B: 255, A: 255})
		}
	}
	animation, err := AtlasAnimation(atlas, 1)
	if err != nil {
		t.Fatal(err)
	}

	// The six-frame running row is authored to play three times. The next
	// frame is idle, and subsequent phases loop only the appended idle row.
	running := color.NRGBAModel.Convert(animation.Frame(Running, 17).At(0, 0)).(color.NRGBA)
	settled := color.NRGBAModel.Convert(animation.Frame(Running, 18).At(0, 0)).(color.NRGBA)
	looped := color.NRGBAModel.Convert(animation.Frame(Running, 24).At(0, 0)).(color.NRGBA)
	if running.B != 255 || settled.R != 255 || looped.R != 255 {
		t.Fatalf("running transition colors = running:%#v settled:%#v looped:%#v", running, settled, looped)
	}
	if got := animation.FrameDelay(Running, 18); got != 1680*time.Millisecond {
		t.Fatalf("first fallback idle delay = %s, want 1.68s", got)
	}
	if got := animation.PrimaryDuration(Waving); got != 2100*time.Millisecond {
		t.Fatalf("waving primary duration = %s, want 2.1s", got)
	}
	if got := animation.PrimaryDuration(Jumping); got != 2520*time.Millisecond {
		t.Fatalf("jumping primary duration = %s, want 2.52s", got)
	}
}

func TestAtlasAnimationSuppliesDefaultTimingForMetadataWithoutTiming(t *testing.T) {
	atlas := image.NewNRGBA(image.Rect(0, 0, 192, 26*9))
	animation, err := atlasAnimation(atlas, 1, map[State]atlasTrack{
		State("dash"): {row: 1, count: 3, fallbackIdle: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := animation.FrameDelay(State("dash"), 0); got <= 0 {
		t.Fatalf("metadata animation delay = %s, want a positive default", got)
	}
}

func TestAtlasAnimationIncludesInteractiveStates(t *testing.T) {
	atlas := image.NewNRGBA(image.Rect(0, 0, 192, 26*9))
	animation, err := AtlasAnimation(atlas, 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []State{MoveRight, MoveLeft, Waving, Jumping} {
		if frame := animation.Frame(state, 0); frame == nil {
			t.Fatalf("interactive state %q has no frame", state)
		}
	}
}

func testClient(t *testing.T, root string, server *httptest.Server) *Client {
	t.Helper()
	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	host := parsed.Hostname()
	client := NewClient(root)
	client.ManifestURL = server.URL + "/manifest"
	client.RankingURL = server.URL + "/ranking"
	client.HTTPClient = server.Client()
	client.TrustedHosts = map[string]bool{host: true}
	client.TrustedAssetHosts = map[string]bool{host: true}
	return client
}

func entrySlugs(entries []Entry) []string {
	result := make([]string, len(entries))
	for index, entry := range entries {
		result[index] = entry.Slug
	}
	return result
}

func encodedPNG(t *testing.T, width, height int) []byte {
	t.Helper()
	value := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			value.SetNRGBA(x, y, color.NRGBA{R: uint8(x), G: uint8(y), B: 180, A: 255})
		}
	}
	var output bytes.Buffer
	if err := png.Encode(&output, value); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}
