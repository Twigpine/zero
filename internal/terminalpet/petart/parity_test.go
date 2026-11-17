package petart

import (
	"bufio"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// parityRecord is one reference frame exported from OpenClaude's engine by
// testdata/export_parity.ts.
type parityRecord struct {
	Kind   string   `json:"kind"`
	Form   Form     `json:"form"`
	Mood   Mood     `json:"mood"`
	T      float64  `json:"t"`
	Scale  float64  `json:"scale"`
	Rows   []string `json:"rows"`
	Height int      `json:"height"`
	// Supersample is 4 for the default sampling and 2 for animation frames.
	Supersample int    `json:"supersample"`
	Width       int    `json:"width"`
	RGBA        string `json:"rgba"`
}

func loadParity(t *testing.T) []parityRecord {
	t.Helper()
	file, err := os.Open(filepath.Join("testdata", "parity.jsonl.gz"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	reader, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	var records []parityRecord
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 1<<20), 1<<24)
	for scanner.Scan() {
		var record parityRecord
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			t.Fatal(err)
		}
		records = append(records, record)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return records
}

// The reference engine runs on a different language runtime. Both follow
// IEEE 754, but their sine and cosine may differ in the last bit, and a
// coordinate that lands exactly on a pixel boundary can then fall either
// way. These budgets allow for that and nothing more: a transcription
// mistake moves a whole shape and blows straight through them.
const (
	// maxFramePixelDiffs is the number of pixels one frame may differ by.
	maxFramePixelDiffs = 2
	// maxImageChannelDiff is the largest difference in any colour channel of
	// one image pixel. One supersample out of sixteen changing its colour
	// moves a channel by at most 255/16.
	maxImageChannelDiff = 16
	// maxImagePixelDiffs is how many pixels of one image may differ at all.
	maxImagePixelDiffs = 4
)

func TestFramesMatchReferenceEngine(t *testing.T) {
	seen := map[Form]map[Mood]bool{}
	frames, inexact := 0, 0
	for _, record := range loadParity(t) {
		if record.Kind != "frame" {
			continue
		}
		frames++
		if seen[record.Form] == nil {
			seen[record.Form] = map[Mood]bool{}
		}
		seen[record.Form][record.Mood] = true
		got := Render(record.Form, record.Mood, record.T, record.Scale)
		if got.Height != len(record.Rows) {
			t.Fatalf("%s/%s t=%v scale=%v: %d rows, reference has %d", record.Form, record.Mood, record.T, record.Scale, got.Height, len(record.Rows))
		}
		diffs := 0
		for y, want := range record.Rows {
			row := got.Row(y)
			if len(row) != len(want) {
				t.Fatalf("%s/%s t=%v scale=%v row %d: width %d, reference has %d", record.Form, record.Mood, record.T, record.Scale, y, len(row), len(want))
			}
			for x := range want {
				if row[x] != want[x] {
					diffs++
				}
			}
		}
		if diffs > 0 {
			inexact++
		}
		if diffs > maxFramePixelDiffs {
			t.Errorf("%s/%s t=%v scale=%v: %d pixels differ from the reference\n got:\n%s\nwant:\n%s",
				record.Form, record.Mood, record.T, record.Scale, diffs, joinRows(got.Rows()), joinRows(record.Rows))
		}
	}
	// The fixture must cover every character in every mood, or a new one
	// could ship without ever being compared.
	for _, form := range Forms() {
		for _, mood := range Moods() {
			if !seen[form][mood] {
				t.Errorf("no reference frame for %s/%s", form, mood)
			}
		}
	}
	t.Logf("%d reference frames, %d not pixel-exact", frames, inexact)
}

func TestImagesMatchReferenceEngine(t *testing.T) {
	images, inexact := 0, 0
	sampled := map[int]bool{}
	for _, record := range loadParity(t) {
		if record.Kind != "image" {
			continue
		}
		images++
		want, err := base64.StdEncoding.DecodeString(record.RGBA)
		if err != nil {
			t.Fatal(err)
		}
		if record.Supersample != defaultSupersample && record.Supersample != liveSupersample {
			t.Fatalf("%s/%s: unexpected supersample %d", record.Form, record.Mood, record.Supersample)
		}
		sampled[record.Supersample] = true
		got := imageAt(record.Form, record.Mood, record.T, record.Height, 0, record.Supersample)
		if got.Rect.Dx() != record.Width || got.Rect.Dy() != record.Height || len(got.Pix) != len(want) {
			t.Fatalf("%s/%s: image is %dx%d, reference is %dx%d", record.Form, record.Mood, got.Rect.Dx(), got.Rect.Dy(), record.Width, record.Height)
		}
		pixelDiffs, worst := 0, 0
		for offset := 0; offset < len(want); offset += 4 {
			differs := false
			for channel := 0; channel < 4; channel++ {
				delta := int(got.Pix[offset+channel]) - int(want[offset+channel])
				if delta < 0 {
					delta = -delta
				}
				if delta > 0 {
					differs = true
				}
				worst = max(worst, delta)
			}
			if differs {
				pixelDiffs++
			}
		}
		if pixelDiffs > 0 {
			inexact++
		}
		if worst > maxImageChannelDiff || pixelDiffs > maxImagePixelDiffs {
			t.Errorf("%s/%s t=%v: %d pixels differ from the reference, worst channel difference %d", record.Form, record.Mood, record.T, pixelDiffs, worst)
		}
	}
	// Both the default sampling and the lighter one animation frames use.
	if !sampled[defaultSupersample] || !sampled[liveSupersample] {
		t.Fatalf("reference images cover supersampling %v, want both %d and %d", sampled, defaultSupersample, liveSupersample)
	}
	t.Logf("%d reference images, %d not pixel-exact", images, inexact)
}

func joinRows(rows []string) string {
	out := ""
	for _, row := range rows {
		out += row + "\n"
	}
	return out
}
