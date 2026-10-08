package terminalpet

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"image"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	xdraw "golang.org/x/image/draw"

	"github.com/Gitlawb/zero/internal/installtxn"
)

const kittyChunkSize = 4096

type ImageProtocol uint8

const (
	ImageProtocolNone ImageProtocol = iota
	ImageProtocolKitty
	ImageProtocolKittyLocalFile
	ImageProtocolSixel
)

type ImageSupport struct {
	Protocol ImageProtocol
	Reason   string
}

func (s ImageSupport) Supported() bool {
	return s.Protocol != ImageProtocolNone
}

func DetectImageSupport(getenv func(string) string) ImageSupport {
	if strings.TrimSpace(getenv("TMUX")) != "" || strings.TrimSpace(getenv("TMUX_PANE")) != "" {
		return ImageSupport{Reason: "Terminal companions are disabled in tmux because terminal images are not reliably pane-local."}
	}
	if strings.TrimSpace(getenv("ZELLIJ")) != "" || strings.TrimSpace(getenv("ZELLIJ_SESSION_NAME")) != "" || strings.TrimSpace(getenv("ZELLIJ_VERSION")) != "" {
		return ImageSupport{Reason: "Terminal companions are disabled in Zellij because terminal images are not reliably pane-local."}
	}
	if strings.TrimSpace(getenv("KITTY_WINDOW_ID")) != "" || strings.TrimSpace(getenv("WEZTERM_EXECUTABLE")) != "" || strings.TrimSpace(getenv("WEZTERM_VERSION")) != "" {
		return ImageSupport{Protocol: ImageProtocolKitty}
	}
	term := strings.ToLower(getenv("TERM"))
	program := strings.ToLower(getenv("TERM_PROGRAM"))
	if strings.Contains(program, "iterm") {
		if dottedVersionAtLeast(getenv("TERM_PROGRAM_VERSION"), 3, 6, 0) {
			return ImageSupport{Protocol: ImageProtocolKittyLocalFile}
		}
		return ImageSupport{Reason: "Terminal companions require iTerm2 3.6 or newer."}
	}
	if strings.Contains(term, "ghostty") || strings.Contains(program, "ghostty") || strings.TrimSpace(getenv("GHOSTTY_RESOURCES_DIR")) != "" ||
		strings.Contains(term, "kitty") || strings.Contains(program, "kitty") || strings.Contains(term, "wezterm") || strings.Contains(program, "wezterm") {
		return ImageSupport{Protocol: ImageProtocolKitty}
	}
	if strings.TrimSpace(getenv("WT_SESSION")) != "" || strings.Contains(term, "sixel") || strings.Contains(term, "mlterm") || strings.Contains(term, "foot") {
		return ImageSupport{Protocol: ImageProtocolSixel}
	}
	return ImageSupport{Reason: "Terminal companions need Kitty graphics or Sixel image support."}
}

type ImageDraw struct {
	ID           uint32
	Animation    *Animation
	State        State
	Phase        int
	X            int
	Y            int
	OffsetX      int
	OffsetY      int
	Columns      int
	Rows         int
	HeightPixels int
}

type imageDrawKey struct {
	protocol  ImageProtocol
	id        uint32
	animation *Animation
	frame     frameCacheKey
	x         int
	y         int
	offsetX   int
	offsetY   int
	columns   int
	rows      int
	height    int
}

type ImageRenderer struct {
	mu      sync.Mutex
	support ImageSupport
	cache   string
	desired *ImageDraw
	last    *imageDrawKey
}

func NewImageRenderer(support ImageSupport) *ImageRenderer {
	return &ImageRenderer{support: support}
}

func NewImageRendererWithCache(support ImageSupport, cacheDir string) *ImageRenderer {
	return &ImageRenderer{support: support, cache: strings.TrimSpace(cacheDir)}
}

func (r *ImageRenderer) Support() ImageSupport {
	if r == nil {
		return ImageSupport{Reason: "Terminal image rendering is unavailable."}
	}
	return r.support
}

func (r *ImageRenderer) Set(draw *ImageDraw) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if draw == nil {
		r.desired = nil
		return
	}
	copyValue := *draw
	r.desired = &copyValue
}

func (r *ImageRenderer) Invalidate() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.last = nil
}

func (r *ImageRenderer) Render(writer io.Writer) error {
	if r == nil || writer == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.support.Supported() {
		return nil
	}

	var desiredKey *imageDrawKey
	var pngBytes []byte
	if r.desired != nil && r.desired.Animation != nil {
		var frameKey frameCacheKey
		var err error
		pngBytes, frameKey, err = r.desired.Animation.framePNG(r.desired.State, r.desired.Phase)
		if err != nil {
			return err
		}
		desiredKey = &imageDrawKey{
			protocol: r.support.Protocol, id: r.desired.ID, animation: r.desired.Animation, frame: frameKey,
			x: r.desired.X, y: r.desired.Y, offsetX: r.desired.OffsetX, offsetY: r.desired.OffsetY,
			columns: r.desired.Columns, rows: r.desired.Rows,
			height: r.desired.HeightPixels,
		}
	}
	if imageKeysEqual(r.last, desiredKey) {
		return nil
	}
	if kittyImageContentEqual(r.last, desiredKey) {
		if _, err := fmt.Fprintf(writer, "\x1b[s\x1b[%d;%dH%s\x1b[u", desiredKey.y+1, desiredKey.x+1, kittyPlace(desiredKey.id, desiredKey.columns, desiredKey.rows, desiredKey.offsetX, desiredKey.offsetY)); err != nil {
			return err
		}
		copyKey := *desiredKey
		r.last = &copyKey
		return nil
	}
	if r.last != nil {
		if err := clearRenderedImage(writer, *r.last); err != nil {
			return err
		}
	}
	if desiredKey == nil {
		r.last = nil
		return nil
	}

	if _, err := fmt.Fprintf(writer, "\x1b[s\x1b[%d;%dH", desiredKey.y+1, desiredKey.x+1); err != nil {
		return err
	}
	switch r.support.Protocol {
	case ImageProtocolKitty:
		if _, err := io.WriteString(writer, kittyTransmitPNG(pngBytes, desiredKey.id, desiredKey.columns, desiredKey.rows, desiredKey.offsetX, desiredKey.offsetY)); err != nil {
			return err
		}
	case ImageProtocolKittyLocalFile:
		path, err := cachePNGFrame(r.cache, pngBytes)
		if err != nil {
			return err
		}
		if _, err := io.WriteString(writer, kittyTransmitPNGFile(path, desiredKey.id, desiredKey.columns, desiredKey.rows, desiredKey.offsetX, desiredKey.offsetY)); err != nil {
			return err
		}
	case ImageProtocolSixel:
		frame := r.desired.Animation.Frame(r.desired.State, r.desired.Phase)
		sixel, err := renderSixel(frame, r.desired.HeightPixels)
		if err != nil {
			return err
		}
		if _, err := writer.Write(sixel); err != nil {
			return err
		}
	default:
		return nil
	}
	if _, err := io.WriteString(writer, "\x1b[u"); err != nil {
		return err
	}
	copyKey := *desiredKey
	r.last = &copyKey
	return nil
}

func (r *ImageRenderer) Clear(writer io.Writer) error {
	r.Set(nil)
	return r.Render(writer)
}

// DeleteImages removes known application-owned Kitty image placements even when
// this renderer no longer considers them active. This is intended for process
// shutdown and recovery from a previous interrupted render.
func (r *ImageRenderer) DeleteImages(writer io.Writer, ids ...uint32) error {
	if r == nil || writer == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.desired = nil
	r.last = nil
	if r.support.Protocol != ImageProtocolKitty && r.support.Protocol != ImageProtocolKittyLocalFile {
		return nil
	}
	for _, id := range ids {
		if _, err := io.WriteString(writer, kittyDelete(id)); err != nil {
			return err
		}
	}
	return nil
}

func imageKeysEqual(left, right *imageDrawKey) bool {
	if left == nil || right == nil {
		return left == right
	}
	return *left == *right
}

func kittyImageContentEqual(left, right *imageDrawKey) bool {
	if left == nil || right == nil || left.protocol != right.protocol {
		return false
	}
	if left.protocol != ImageProtocolKitty && left.protocol != ImageProtocolKittyLocalFile {
		return false
	}
	leftValue, rightValue := *left, *right
	leftValue.x, leftValue.y = 0, 0
	leftValue.offsetX, leftValue.offsetY = 0, 0
	rightValue.x, rightValue.y = 0, 0
	rightValue.offsetX, rightValue.offsetY = 0, 0
	return leftValue == rightValue
}

func kittyDelete(id uint32) string {
	return fmt.Sprintf("\x1b_Ga=d,d=I,i=%d,q=2;\x1b\\", id)
}

func clearRenderedImage(writer io.Writer, key imageDrawKey) (err error) {
	if key.protocol == ImageProtocolKitty || key.protocol == ImageProtocolKittyLocalFile {
		_, err := io.WriteString(writer, kittyDelete(key.id))
		return err
	}
	if key.protocol != ImageProtocolSixel {
		return nil
	}
	// Sixel is the only protocol here that erases by PAINTING CHARACTERS. Kitty
	// hands the terminal an id and the placement goes away; a sixel is already in
	// the cell grid, so the only way to remove it is to write over it. That makes
	// this the one path where the caller's SGR state matters, and it was not
	// being normalised.
	//
	// The spaces below are ordinary characters, so they carry whatever background
	// the TUI last set. Every erase therefore stamped a filled block in the
	// ambient colour, and since a drag erases and repaints on every pixel, that
	// block was continuously refreshed over the interface. It stayed invisible on
	// a terminal whose ambient background matched its theme, which is why it
	// never appeared on the Kitty-protocol terminals — ghostty, kitty, WezTerm,
	// iTerm — that do not reach this code at all. Windows Terminal is the only
	// mainstream terminal that lands here.
	//
	// DECSC/DECRC rather than CSI s/u: this now has to put back the SGR state as
	// well as the cursor, and only DECSC saves attributes. Restoring them matters
	// because the erase is emitted mid-frame, between styled writes the TUI has
	// already started.
	if _, err = io.WriteString(writer, "\x1b7\x1b[0m"); err != nil {
		return err
	}
	defer func() {
		if _, restoreErr := io.WriteString(writer, "\x1b8"); err == nil {
			err = restoreErr
		}
	}()
	for row := 0; row < key.rows; row++ {
		if _, err = fmt.Fprintf(writer, "\x1b[%d;%dH%s", key.y+row+1, key.x+1, strings.Repeat(" ", key.columns)); err != nil {
			return err
		}
	}
	return nil
}

func kittyTransmitPNGFile(path string, id uint32, columns, rows, offsetX, offsetY int) string {
	payload := base64.StdEncoding.EncodeToString([]byte(path))
	return fmt.Sprintf("\x1b_Ga=T,t=f,f=100,c=%d,r=%d%s,q=2,C=1,i=%d,p=%d;%s\x1b\\", columns, rows, kittyOffsetControl(offsetX, offsetY), id, id, payload)
}

func kittyPlace(id uint32, columns, rows, offsetX, offsetY int) string {
	return fmt.Sprintf("\x1b_Ga=p,i=%d,p=%d,c=%d,r=%d%s,q=2,C=1;\x1b\\", id, id, columns, rows, kittyOffsetControl(offsetX, offsetY))
}

func kittyOffsetControl(offsetX, offsetY int) string {
	if offsetX == 0 && offsetY == 0 {
		return ""
	}
	return fmt.Sprintf(",X=%d,Y=%d", offsetX, offsetY)
}

func cachePNGFrame(cacheDir string, pngBytes []byte) (string, error) {
	if cacheDir == "" {
		return "", fmt.Errorf("terminal pet image cache is unavailable")
	}
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		return "", fmt.Errorf("create terminal pet image cache: %w", err)
	}
	digest := sha256.Sum256(pngBytes)
	path := filepath.Join(cacheDir, fmt.Sprintf("%x.png", digest))
	if _, err := os.Stat(path); err != nil {
		if !os.IsNotExist(err) {
			return "", fmt.Errorf("inspect cached terminal pet frame: %w", err)
		}
		if err := installtxn.WriteFileAtomically(path, pngBytes, 0o600); err != nil {
			return "", fmt.Errorf("cache terminal pet frame: %w", err)
		}
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve cached terminal pet frame: %w", err)
	}
	return absolute, nil
}

func dottedVersionAtLeast(value string, major, minor, patch int) bool {
	parts := strings.Split(strings.TrimSpace(value), ".")
	if len(parts) == 0 || len(parts) > 3 {
		return false
	}
	parsed := [3]int{}
	for index, part := range parts {
		digits := strings.TrimLeftFunc(part, func(value rune) bool { return value < '0' || value > '9' })
		end := 0
		for end < len(digits) && digits[end] >= '0' && digits[end] <= '9' {
			end++
		}
		if end == 0 {
			return false
		}
		number, err := strconv.Atoi(digits[:end])
		if err != nil {
			return false
		}
		parsed[index] = number
	}
	want := [3]int{major, minor, patch}
	for index := range parsed {
		if parsed[index] != want[index] {
			return parsed[index] > want[index]
		}
	}
	return true
}

func renderSixel(frame image.Image, heightPixels int) ([]byte, error) {
	if frame == nil || frame.Bounds().Empty() {
		return nil, fmt.Errorf("pet animation has no frame to encode")
	}
	if heightPixels < 1 {
		heightPixels = 75
	}
	bounds := frame.Bounds()
	widthPixels := max(1, bounds.Dx()*heightPixels/bounds.Dy())
	scaled := image.NewNRGBA(image.Rect(0, 0, widthPixels, heightPixels))
	xdraw.CatmullRom.Scale(scaled, scaled.Bounds(), frame, bounds, xdraw.Over, nil)
	return encodeSixel(scaled)
}

func kittyTransmitPNG(pngBytes []byte, id uint32, columns, rows, offsetX, offsetY int) string {
	payload := base64.StdEncoding.EncodeToString(pngBytes)
	var output strings.Builder
	for offset := 0; offset < len(payload); offset += kittyChunkSize {
		end := min(offset+kittyChunkSize, len(payload))
		more := 0
		if end < len(payload) {
			more = 1
		}
		if offset == 0 {
			fmt.Fprintf(&output, "\x1b_Ga=T,t=d,f=100,c=%d,r=%d%s,q=2,C=1,i=%d,p=%d,m=%d;%s\x1b\\", columns, rows, kittyOffsetControl(offsetX, offsetY), id, id, more, payload[offset:end])
		} else {
			fmt.Fprintf(&output, "\x1b_Gm=%d;%s\x1b\\", more, payload[offset:end])
		}
	}
	return output.String()
}
