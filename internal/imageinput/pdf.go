package imageinput

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Gitlawb/zero/internal/zeroruntime"
	"github.com/ledongthuc/pdf"
)

// Dependency posture (see stage 12): the DEFAULT build extracts a PDF's text
// layer in pure Go via github.com/ledongthuc/pdf (BSD-licensed, no CGO, no
// transitive deps), so ZERO stays a single static cross-compilable binary with
// no runtime dependencies. Rasterizing pages to images for vision models needs
// real font/graphics rendering, which no maintained pure-Go library does well;
// that path is OPTIONAL and uses the poppler tools (pdftotext / pdftoppm) only
// when they are already on PATH -- the same "external tool the user may have"
// posture as the LSP language servers. When poppler is absent, extraction
// silently degrades to the pure-Go text layer; absence is never an error.

// MaxDocumentBytes is the per-document raw-file cap (32 MiB). PDFs are routinely
// larger than the image cap, but we still bound the file before it is read into
// memory or handed to a parser so an unbounded file never reaches a provider.
const MaxDocumentBytes = 32 << 20

// MaxDocumentTextBytes caps the EXTRACTED text we hand to a model (256 KiB).
// Unlike the raw-file cap (which rejects), an over-cap text layer is truncated
// with documentTruncatedMarker so a large-but-valid spec is still partially
// usable instead of refused outright.
const MaxDocumentTextBytes = 256 << 10

// documentTruncatedMarker is appended to capped text so the agent (and the user)
// can tell extraction was cut short rather than the document simply ending.
const documentTruncatedMarker = "\n\n[... document text truncated at the size limit ...]"

// defaultMaxRasterPages bounds how many pages the optional vision path renders,
// so a long PDF cannot blow up the context window. Callers may override it via
// DocumentOptions.MaxPages.
const defaultMaxRasterPages = 10

// popplerTimeout bounds each external poppler invocation so a wedged or
// pathological binary cannot hang the CLI/TUI.
const popplerTimeout = 30 * time.Second

// pdfMagic is the leading signature of every PDF stream. Detection keys on these
// bytes, never on the file extension alone.
var pdfMagic = []byte("%PDF-")

// Document is the result of ingesting a PDF: the extracted text layer (always
// populated when a text layer exists) plus, on the optional vision path, one
// ImageBlock per rendered page. Pages is the page count the parser reported;
// Truncated is set when Text was capped at MaxDocumentTextBytes.
type Document struct {
	Text      string
	Images    []zeroruntime.ImageBlock
	Pages     int
	Truncated bool
}

// DocumentOptions tunes how a PDF is ingested.
type DocumentOptions struct {
	// Vision asks for page rasterization (ImageBlocks) in addition to text, for a
	// vision-capable model. It is best-effort: when no rasterizer is available the
	// load degrades to the text layer rather than erroring.
	Vision bool
	// MaxPages bounds how many pages the vision path renders. Zero means
	// defaultMaxRasterPages.
	MaxPages int

	// disableExternalTools forces the pure-Go path even if poppler is installed.
	// It exists so tests are deterministic on any host; it is intentionally
	// unexported and not part of the public surface.
	disableExternalTools bool
}

// isPDF reports whether data begins with the PDF magic signature ("%PDF-"). It
// is the authoritative check: the file extension is only a hint.
func isPDF(data []byte) bool {
	return bytes.HasPrefix(data, pdfMagic)
}

// IsProbablyDocumentPath reports whether a path looks like a document ZERO can
// ingest (currently: a ".pdf" extension, case-insensitive). It is only a routing
// hint for input surfaces deciding whether to call LoadDocument vs LoadFile;
// LoadDocument re-verifies the real content via magic bytes.
func IsProbablyDocumentPath(path string) bool {
	return strings.EqualFold(filepath.Ext(path), ".pdf")
}

// LooksLikeDocumentFile reports whether the file at path is a PDF by content,
// reading only its leading magic bytes. It lets input surfaces route a real PDF
// to LoadDocument even when its name lacks a ".pdf" extension, so detection is
// content-based rather than extension-only. It opens nothing it cannot stat as a
// regular file and never reads more than the magic prefix; any I/O error (missing
// file, permission, non-regular) simply reports false and lets the normal path
// surface a precise error. Relative paths resolve against workspaceRoot.
func LooksLikeDocumentFile(path string, workspaceRoot string) bool {
	resolved := path
	if !filepath.IsAbs(resolved) {
		resolved = filepath.Join(workspaceRoot, resolved)
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	file, err := os.Open(resolved)
	if err != nil {
		return false
	}
	defer file.Close()

	header := make([]byte, len(pdfMagic))
	n, err := io.ReadFull(file, header)
	if err != nil && n < len(pdfMagic) {
		return false
	}
	return isPDF(header)
}

// LoadDocument reads the PDF at path (resolved against workspaceRoot when
// relative), enforces the per-document size cap, and extracts its text layer.
// With opts.Vision and an available rasterizer it also renders the first N pages
// to ImageBlocks. The file is identified by magic bytes, not its extension, so a
// ".pdf"-named non-PDF is rejected with a clear error. A PDF with no text layer
// and no rasterization/OCR available returns an explicit "no extractable text"
// error rather than a silent empty success. Errors are plain (callers wrap them
// into surface-specific notice text).
func LoadDocument(path string, workspaceRoot string, opts DocumentOptions) (Document, error) {
	data, err := readDocumentBytes(path, workspaceRoot)
	if err != nil {
		return Document{}, err
	}
	if !isPDF(data) {
		return Document{}, fmt.Errorf("%s is not a PDF (expected a %%PDF- file)", path)
	}

	useExternal := !opts.disableExternalTools

	// Vision path (optional): render pages to images via poppler when available.
	// Failures here are non-fatal -- we still return the text layer below.
	var images []zeroruntime.ImageBlock
	if opts.Vision && useExternal {
		if rendered, rerr := rasterizeWithPoppler(data, opts.maxPages()); rerr == nil {
			images = rendered
		}
	}

	// Text path. Prefer poppler's pdftotext when present (it handles more font
	// encodings); otherwise use the pure-Go extractor. Either way, absence of the
	// external tool is not an error.
	text, pages := "", 0
	if useExternal {
		if t, ok := extractTextWithPoppler(data); ok {
			text = t
			// pdftotext does not report a page count, so derive it from the pure-Go
			// reader (cheap structural read, no text extraction) to keep
			// Document.Pages correct regardless of which text path wins.
			pages = pdfPageCount(data)
		}
	}
	if strings.TrimSpace(text) == "" {
		t, p, terr := extractTextPureGo(data)
		if terr != nil {
			// Only surface the pure-Go error when we have nothing else (no poppler
			// text and no rasterized pages) to offer.
			if len(images) == 0 {
				return Document{}, terr
			}
		} else {
			text, pages = t, p
		}
	}

	text, truncated := capDocumentText(text)

	// Scanned-PDF guard: no text layer AND no rendered pages means we have nothing
	// the model can use. Say so explicitly instead of returning empty success.
	if strings.TrimSpace(text) == "" && len(images) == 0 {
		return Document{}, fmt.Errorf("%s has no extractable text; OCR is not available (install poppler's pdftotext/pdftoppm for image-only PDFs)", path)
	}

	return Document{Text: text, Images: images, Pages: pages, Truncated: truncated}, nil
}

// readDocumentBytes resolves path against workspaceRoot, rejects missing,
// non-regular, and oversized files (mirroring LoadFile), and returns the raw
// bytes with a hard bound so an unbounded source can never allocate without
// limit.
func readDocumentBytes(path string, workspaceRoot string) ([]byte, error) {
	resolved := path
	if !filepath.IsAbs(resolved) {
		resolved = filepath.Join(workspaceRoot, resolved)
	}

	info, err := os.Stat(resolved)
	if err != nil {
		return nil, fmt.Errorf("document file not found: %s", path)
	}
	// Reject non-regular files (directories, FIFOs, devices) before os.Open so a
	// writerless FIFO can never block the read forever -- same guard as LoadFile.
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("document file must be a regular file: %s", path)
	}
	if info.Size() > MaxDocumentBytes {
		return nil, fmt.Errorf("document %s is larger than the 32 MiB limit", path)
	}

	file, err := os.Open(resolved)
	if err != nil {
		return nil, fmt.Errorf("cannot open document %s: %w", path, err)
	}
	defer file.Close()

	// LimitReader of cap+1: at most one byte past the cap is buffered, so the cap
	// is the real bound regardless of any stat/read race or a growing file.
	data, err := io.ReadAll(io.LimitReader(file, MaxDocumentBytes+1))
	if err != nil {
		return nil, fmt.Errorf("cannot read document %s: %w", path, err)
	}
	if len(data) > MaxDocumentBytes {
		return nil, fmt.Errorf("document %s is larger than the 32 MiB limit", path)
	}
	return data, nil
}

// extractTextPureGo extracts the full text layer with the pure-Go parser. The
// ledongthuc/pdf parser panics (not errors) on some malformed structures, so the
// whole call is wrapped in a recover: a bad PDF becomes a clean error, never a
// crash that escapes the package. It returns the joined text and the page count.
func extractTextPureGo(data []byte) (text string, pages int, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			text, pages = "", 0
			err = fmt.Errorf("could not parse PDF (malformed or unsupported): %v", rec)
		}
	}()

	reader, rerr := pdf.NewReader(bytes.NewReader(data), int64(len(data)))
	if rerr != nil {
		return "", 0, fmt.Errorf("could not parse PDF: %w", rerr)
	}
	pages = reader.NumPage()

	var buf strings.Builder
	plain, perr := reader.GetPlainText()
	if perr != nil {
		return "", pages, fmt.Errorf("could not extract PDF text: %w", perr)
	}
	if _, cerr := io.Copy(&buf, plain); cerr != nil {
		return "", pages, fmt.Errorf("could not read PDF text: %w", cerr)
	}
	return strings.TrimSpace(buf.String()), pages, nil
}

// pdfPageCount returns the page count via the pure-Go reader without extracting
// any text. It backs Document.Pages on the poppler text path (pdftotext does not
// report a count). Like extractTextPureGo it recovers from the parser's panics on
// malformed input and reports 0 rather than crashing -- the page count is
// informational, so an unreadable structure simply yields 0.
func pdfPageCount(data []byte) (pages int) {
	defer func() {
		if recover() != nil {
			pages = 0
		}
	}()
	reader, err := pdf.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return 0
	}
	return reader.NumPage()
}

// capDocumentText truncates text to MaxDocumentTextBytes on a UTF-8 rune
// boundary and appends documentTruncatedMarker when it had to cut. The second
// return reports whether truncation happened. The marker is counted against the
// cap so the returned string never exceeds MaxDocumentTextBytes.
func capDocumentText(text string) (string, bool) {
	if len(text) <= MaxDocumentTextBytes {
		return text, false
	}
	// Reserve room for the marker so the final payload (text + marker) stays at or
	// under the advertised cap. If the marker alone would not fit, cut to nothing
	// and return just the marker.
	cut := MaxDocumentTextBytes - len(documentTruncatedMarker)
	if cut < 0 {
		cut = 0
	}
	// Back up to a rune boundary so we never split a multi-byte character.
	for cut > 0 && !utf8RuneStart(text[cut]) {
		cut--
	}
	return text[:cut] + documentTruncatedMarker, true
}

// utf8RuneStart reports whether b can start a UTF-8 rune (i.e. it is not a
// continuation byte 0b10xxxxxx). Kept local to avoid pulling in unicode/utf8
// for a single bit test.
func utf8RuneStart(b byte) bool {
	return b&0xC0 != 0x80
}

func (o DocumentOptions) maxPages() int {
	if o.MaxPages > 0 {
		return o.MaxPages
	}
	return defaultMaxRasterPages
}

// --- Optional external poppler path -----------------------------------------

// popplerAvailable reports whether a poppler binary is resolvable on PATH.
func popplerAvailable(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

// extractTextWithPoppler runs `pdftotext - -` (read stdin, write stdout) when
// pdftotext is on PATH. The bool is false when the tool is absent or failed, so
// the caller can fall back to the pure-Go extractor. Absence is never an error.
func extractTextWithPoppler(data []byte) (string, bool) {
	if !popplerAvailable("pdftotext") {
		return "", false
	}
	ctx, cancel := context.WithTimeout(context.Background(), popplerTimeout)
	defer cancel()

	// "-layout" keeps the visual column layout; the trailing "- -" reads the PDF
	// from stdin and writes UTF-8 text to stdout.
	cmd := exec.CommandContext(ctx, "pdftotext", "-layout", "-enc", "UTF-8", "-", "-")
	cmd.Stdin = bytes.NewReader(data)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", false
	}
	return strings.TrimSpace(stdout.String()), true
}

// rasterizeWithPoppler renders the first maxPages pages to PNG via pdftoppm and
// returns them as normalized ImageBlocks (reusing the image allow-list, sniff,
// and per-image cap). It returns an error when pdftoppm is absent or rendering
// produced nothing; the caller treats that as "no rasterization available" and
// keeps the text layer.
func rasterizeWithPoppler(data []byte, maxPages int) ([]zeroruntime.ImageBlock, error) {
	if !popplerAvailable("pdftoppm") {
		return nil, fmt.Errorf("pdftoppm not available")
	}
	if maxPages <= 0 {
		maxPages = defaultMaxRasterPages
	}

	dir, err := os.MkdirTemp("", "zero-pdf-raster-")
	if err != nil {
		return nil, fmt.Errorf("cannot create temp dir for rasterization: %w", err)
	}
	defer os.RemoveAll(dir)

	ctx, cancel := context.WithTimeout(context.Background(), popplerTimeout)
	defer cancel()

	prefix := filepath.Join(dir, "page")
	// -png: PNG output; -r 150: 150 DPI (legible without huge files);
	// -f 1 / -l N: render only the first N pages so context can't blow up.
	cmd := exec.CommandContext(ctx, "pdftoppm", "-png", "-r", "150", "-f", "1", "-l", fmt.Sprintf("%d", maxPages), "-", prefix)
	cmd.Stdin = bytes.NewReader(data)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("pdftoppm failed: %w", err)
	}

	entries, err := filepath.Glob(prefix + "*.png")
	if err != nil {
		return nil, fmt.Errorf("cannot list rendered pages: %w", err)
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("rasterization produced no pages")
	}
	// Glob order is lexical; pdftoppm zero-pads page numbers, so a numeric-aware
	// sort keeps page 10 after page 9 rather than after page 1.
	sort.Strings(entries)

	images := make([]zeroruntime.ImageBlock, 0, len(entries))
	for _, name := range entries {
		if len(images) >= maxPages {
			break
		}
		block, err := loadRenderedPage(name)
		if err != nil {
			// Skip a single unreadable page rather than failing the whole render.
			continue
		}
		images = append(images, block)
	}
	if len(images) == 0 {
		return nil, fmt.Errorf("no rendered pages could be loaded")
	}
	return images, nil
}

// loadRenderedPage reads one rendered PNG page, enforces the per-image cap, and
// normalizes its media type through the same allow-list LoadFile uses, so
// rasterized pages flow through the existing image pipeline unchanged.
func loadRenderedPage(name string) (zeroruntime.ImageBlock, error) {
	info, err := os.Stat(name)
	if err != nil {
		return zeroruntime.ImageBlock{}, err
	}
	if info.Size() > MaxImageBytes {
		return zeroruntime.ImageBlock{}, fmt.Errorf("rendered page %s exceeds the per-image limit", filepath.Base(name))
	}
	data, err := os.ReadFile(name)
	if err != nil {
		return zeroruntime.ImageBlock{}, err
	}
	if len(data) > MaxImageBytes {
		return zeroruntime.ImageBlock{}, fmt.Errorf("rendered page %s exceeds the per-image limit", filepath.Base(name))
	}
	sniffLen := len(data)
	if sniffLen > 512 {
		sniffLen = 512
	}
	mediaType := zeroruntime.NormalizeImageMediaType(http.DetectContentType(data[:sniffLen]))
	if mediaType == "" {
		return zeroruntime.ImageBlock{}, fmt.Errorf("rendered page %s has an unsupported image type", filepath.Base(name))
	}
	return zeroruntime.ImageBlock{MediaType: mediaType, Data: data}, nil
}
