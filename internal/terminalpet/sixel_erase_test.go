package terminalpet

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// A sixel erase paints characters, so it must not inherit the caller's colours.
//
// This is the black rectangle reported on Windows Terminal. The erase writes
// literal spaces over the cells the image occupied, and those spaces are drawn
// with whatever SGR was last set; without a reset every erase stamps a filled
// block in the ambient background. A drag erases and repaints on every pixel, so
// the block is continuously refreshed over the interface.
//
// It never showed on ghostty, kitty, WezTerm or iTerm because those resolve to
// the Kitty protocol and delete by image id, never reaching this path. Windows
// Terminal is the only mainstream terminal that does, which is why the platform
// looked at fault when the real cause is an untested code path.
func TestSixelEraseDoesNotPaintWithTheCallersColours(t *testing.T) {
	var out bytes.Buffer
	key := imageDrawKey{protocol: ImageProtocolSixel, x: 40, y: 2, columns: 6, rows: 3}
	if err := clearRenderedImage(&out, key); err != nil {
		t.Fatalf("clearRenderedImage: %v", err)
	}
	got := out.String()

	reset := strings.Index(got, "\x1b[0m")
	if reset < 0 {
		t.Fatalf("the erase never resets SGR, so it paints a block in the ambient background: %q", got)
	}
	// Before the first space, not merely present somewhere.
	if firstSpace := strings.Index(got, strings.Repeat(" ", key.columns)); firstSpace >= 0 && reset > firstSpace {
		t.Errorf("SGR is reset only after the first row is painted, so that row still carries the caller's colours: %q", got)
	}
	// DECSC/DECRC, because only those restore the attributes this now clobbers.
	// CSI s/u save the cursor alone and would leave the TUI drawing in the reset
	// state for the remainder of the frame.
	if !strings.HasPrefix(got, "\x1b7") {
		t.Errorf("erase must open with DECSC to save cursor AND attributes, got %q", got)
	}
	if !strings.HasSuffix(got, "\x1b8") {
		t.Errorf("erase must close with DECRC to restore them, got %q", got)
	}
}

// The erase still covers exactly the cells the image claimed, or the fix above
// would be satisfied by an erase that does nothing.
func TestSixelEraseStillCoversEveryClaimedCell(t *testing.T) {
	var out bytes.Buffer
	key := imageDrawKey{protocol: ImageProtocolSixel, x: 40, y: 2, columns: 6, rows: 3}
	if err := clearRenderedImage(&out, key); err != nil {
		t.Fatalf("clearRenderedImage: %v", err)
	}
	got := out.String()
	for row := 0; row < key.rows; row++ {
		position := fmt.Sprintf("\x1b[%d;%dH", key.y+row+1, key.x+1)
		erase := position + strings.Repeat(" ", key.columns)
		if !strings.Contains(got, erase) {
			t.Fatalf("erase does not position row %d at %q: %q", row, position, got)
		}
	}
	if strings.Count(got, strings.Repeat(" ", key.columns)) != key.rows {
		t.Errorf("erase covered %d rows of %d columns, want %d rows: %q",
			strings.Count(got, strings.Repeat(" ", key.columns)), key.columns, key.rows, got)
	}
}

func TestSixelEraseRestoresTerminalStateAfterRowWriteFailure(t *testing.T) {
	rowErr := errors.New("row write failed")
	writer := &failOnceWriter{failOnWrite: 2, err: rowErr}
	key := imageDrawKey{protocol: ImageProtocolSixel, x: 4, y: 2, columns: 3, rows: 2}

	err := clearRenderedImage(writer, key)
	if !errors.Is(err, rowErr) {
		t.Fatalf("clearRenderedImage error = %v, want primary row error", err)
	}
	if !strings.HasSuffix(writer.String(), "\x1b8") {
		t.Fatalf("terminal state was not restored after row failure: %q", writer.String())
	}
}

type failOnceWriter struct {
	bytes.Buffer
	writes      int
	failOnWrite int
	err         error
}

func (w *failOnceWriter) Write(value []byte) (int, error) {
	w.writes++
	if w.writes == w.failOnWrite {
		return 0, w.err
	}
	return w.Buffer.Write(value)
}

// Kitty must NOT gain the character-painting erase: it deletes by image id, and
// writing spaces there would erase cells the terminal is about to redraw itself.
func TestKittyEraseStillDeletesByImageID(t *testing.T) {
	for _, protocol := range []ImageProtocol{ImageProtocolKitty, ImageProtocolKittyLocalFile} {
		var out bytes.Buffer
		if err := clearRenderedImage(&out, imageDrawKey{protocol: protocol, id: 7, columns: 6, rows: 3}); err != nil {
			t.Fatalf("clearRenderedImage: %v", err)
		}
		if got := out.String(); strings.Contains(got, "   ") || !strings.Contains(got, "\x1b_G") {
			t.Errorf("protocol %v should delete by id, not paint cells: %q", protocol, got)
		}
	}
}
