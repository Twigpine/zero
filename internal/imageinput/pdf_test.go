package imageinput

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const minimalPDFTextChunkSize = 80

// buildMinimalPDF assembles a tiny, single-page PDF whose content stream draws
// the given text. It computes a real cross-reference table and trailer so a
// pure-Go PDF parser (ledongthuc/pdf) accepts it. Generating the fixture in-test
// keeps the repo free of opaque binary blobs while still exercising the real
// text-extraction path on real PDF bytes.
func buildMinimalPDF(text string) []byte {
	var buf bytes.Buffer
	offsets := make([]int, 0, 8)
	startObj := func() { offsets = append(offsets, buf.Len()) }

	buf.WriteString("%PDF-1.4\n")

	startObj() // object 1: catalog
	buf.WriteString("1 0 obj\n<< /Type /Catalog /Pages 2 0 R >>\nendobj\n")

	startObj() // object 2: page tree
	buf.WriteString("2 0 obj\n<< /Type /Pages /Kids [3 0 R] /Count 1 >>\nendobj\n")

	pageHeight := minimalPDFPageHeight(text)
	startObj() // object 3: page
	buf.WriteString("3 0 obj\n<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 " + strconv.Itoa(pageHeight) + "] /Resources << /Font << /F1 5 0 R >> >> /Contents 4 0 R >>\nendobj\n")

	content := minimalPDFTextContent(text, pageHeight-92)
	startObj() // object 4: content stream
	buf.WriteString("4 0 obj\n<< /Length " + strconv.Itoa(len(content)) + " >>\nstream\n")
	buf.WriteString(content)
	buf.WriteString("\nendstream\nendobj\n")

	startObj() // object 5: font
	buf.WriteString("5 0 obj\n<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>\nendobj\n")

	xrefStart := buf.Len()
	buf.WriteString("xref\n")
	buf.WriteString("0 " + strconv.Itoa(len(offsets)+1) + "\n")
	buf.WriteString("0000000000 65535 f \n")
	for _, off := range offsets {
		fmt.Fprintf(&buf, "%010d 00000 n \n", off)
	}
	buf.WriteString("trailer\n<< /Size " + strconv.Itoa(len(offsets)+1) + " /Root 1 0 R >>\n")
	buf.WriteString("startxref\n" + strconv.Itoa(xrefStart) + "\n%%EOF\n")
	return buf.Bytes()
}

func minimalPDFPageHeight(text string) int {
	lines := (len(text) + minimalPDFTextChunkSize - 1) / minimalPDFTextChunkSize
	if lines < 1 {
		lines = 1
	}
	height := 184 + lines*10
	if height < 792 {
		return 792
	}
	return height
}

func minimalPDFTextContent(text string, startY int) string {
	var content strings.Builder
	content.WriteString("BT /F1 8 Tf 10 TL 72 ")
	content.WriteString(strconv.Itoa(startY))
	content.WriteString(" Td ")
	for index := 0; len(text) > 0; index++ {
		if index > 0 {
			content.WriteString(" T* ")
		}
		chunk := text
		if len(chunk) > minimalPDFTextChunkSize {
			cut := minimalPDFTextChunkSize
			for cut > 0 && !utf8RuneStart(chunk[cut]) {
				cut--
			}
			if cut == 0 {
				cut = minimalPDFTextChunkSize
			}
			chunk = text[:cut]
		}
		content.WriteString("(")
		content.WriteString(escapePDFLiteral(chunk))
		content.WriteString(") Tj")
		text = text[len(chunk):]
	}
	content.WriteString(" ET")
	return content.String()
}

func escapePDFLiteral(text string) string {
	text = strings.ReplaceAll(text, `\`, `\\`)
	text = strings.ReplaceAll(text, `(`, `\(`)
	text = strings.ReplaceAll(text, `)`, `\)`)
	text = strings.ReplaceAll(text, "\r", `\r`)
	text = strings.ReplaceAll(text, "\n", `\n`)
	return text
}

func TestIsPDF(t *testing.T) {
	if !isPDF(buildMinimalPDF("hi")) {
		t.Fatal("isPDF should accept real %PDF- bytes")
	}
	if !isPDF([]byte("%PDF-1.7\n...")) {
		t.Fatal("isPDF should accept a bare %PDF- magic prefix")
	}
	if isPDF([]byte("not a pdf at all")) {
		t.Fatal("isPDF should reject non-PDF bytes")
	}
	if isPDF(nil) {
		t.Fatal("isPDF should reject empty input")
	}
	if isPDF([]byte("%PDF")) {
		t.Fatal("isPDF should require the trailing dash of %PDF-")
	}
}

func TestLoadDocumentTextExtraction(t *testing.T) {
	root := t.TempDir()
	want := "Hello ZERO PDF"
	if err := os.WriteFile(filepath.Join(root, "doc.pdf"), buildMinimalPDF(want), 0o644); err != nil {
		t.Fatalf("write pdf: %v", err)
	}

	doc, err := LoadDocument("doc.pdf", root, DocumentOptions{})
	if err != nil {
		t.Fatalf("LoadDocument: %v", err)
	}
	if !strings.Contains(doc.Text, want) {
		t.Fatalf("extracted text %q should contain %q", doc.Text, want)
	}
	if len(doc.Images) != 0 {
		t.Fatalf("text-only extraction should not return images, got %d", len(doc.Images))
	}
	if doc.Pages != 1 {
		t.Fatalf("Pages = %d, want 1", doc.Pages)
	}
}

// A .pdf-named file that is not actually a PDF must be rejected with a clear
// error rather than silently treated as a document (extension is never trusted
// over magic bytes).
func TestLoadDocumentRejectsFakePDF(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "fake.pdf"), []byte("this is plainly not a PDF document"), 0o644); err != nil {
		t.Fatalf("write fake: %v", err)
	}
	_, err := LoadDocument("fake.pdf", root, DocumentOptions{})
	if err == nil {
		t.Fatal("expected error for a .pdf-named non-PDF file")
	}
	if !strings.Contains(err.Error(), "not a PDF") {
		t.Fatalf("error %q should explain the file is not a PDF", err.Error())
	}
}

func TestLoadDocumentMissing(t *testing.T) {
	root := t.TempDir()
	_, err := LoadDocument("nope.pdf", root, DocumentOptions{})
	if err == nil {
		t.Fatal("expected error for a missing file")
	}
	if !strings.Contains(err.Error(), "nope.pdf") {
		t.Fatalf("error %q should name the path", err.Error())
	}
}

func TestLoadDocumentRejectsNonRegular(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "adir.pdf"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	_, err := LoadDocument("adir.pdf", root, DocumentOptions{})
	if err == nil {
		t.Fatal("expected error for a non-regular file")
	}
	if !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("error %q should mention regular file", err.Error())
	}
}

// The per-document byte cap is enforced before parsing, mirroring the image cap,
// so an unbounded file never reaches the parser or a provider.
func TestLoadDocumentOversizeRejected(t *testing.T) {
	root := t.TempDir()
	big := make([]byte, MaxDocumentBytes+1)
	copy(big, []byte("%PDF-1.4\n"))
	if err := os.WriteFile(filepath.Join(root, "big.pdf"), big, 0o644); err != nil {
		t.Fatalf("write big: %v", err)
	}
	_, err := LoadDocument("big.pdf", root, DocumentOptions{})
	if err == nil {
		t.Fatal("expected error for an oversize document")
	}
	if !strings.Contains(err.Error(), "limit") {
		t.Fatalf("error %q should mention the size limit", err.Error())
	}
}

// Extracted text longer than the cap is truncated (with a marker) rather than
// rejected: a large but valid spec/doc should still be partially usable.
func TestLoadDocumentTruncatesLongText(t *testing.T) {
	root := t.TempDir()
	// Many short lines so the *extracted text* (not the file) exceeds the cap.
	var body strings.Builder
	line := "The quick brown fox jumps over the lazy dog. "
	for body.Len() < MaxDocumentTextBytes+65536 {
		body.WriteString(line)
	}
	if err := os.WriteFile(filepath.Join(root, "long.pdf"), buildMinimalPDF(body.String()), 0o644); err != nil {
		t.Fatalf("write long: %v", err)
	}
	doc, err := LoadDocument("long.pdf", root, DocumentOptions{})
	if err != nil {
		t.Fatalf("LoadDocument: %v", err)
	}
	if len(doc.Text) > MaxDocumentTextBytes {
		t.Fatalf("capped text length %d exceeds cap %d (marker must be counted against the cap)", len(doc.Text), MaxDocumentTextBytes)
	}
	if !doc.Truncated {
		t.Fatalf("Truncated should be set when extracted text is capped; extracted len=%d pages=%d", len(doc.Text), doc.Pages)
	}
	if !strings.Contains(doc.Text, documentTruncatedMarker) {
		t.Fatal("truncated text should carry the truncation marker")
	}
}

// A PDF with no extractable text layer and no rasterization/OCR available must
// surface the explicit "no extractable text" message, never a silent empty
// success.
func TestLoadDocumentNoTextNoRaster(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "scan.pdf"), buildEmptyTextPDF(), 0o644); err != nil {
		t.Fatalf("write scan: %v", err)
	}
	// Force the pure-Go path with no external rasterizer so the no-text branch is
	// deterministic regardless of what is installed on the test host.
	_, err := LoadDocument("scan.pdf", root, DocumentOptions{disableExternalTools: true})
	if err == nil {
		t.Fatal("expected an error for a PDF with no extractable text and no raster")
	}
	if !strings.Contains(err.Error(), "no extractable text") {
		t.Fatalf("error %q should explain there is no extractable text", err.Error())
	}
	if !strings.Contains(err.Error(), "OCR") {
		t.Fatalf("error %q should mention OCR is unavailable", err.Error())
	}
}

// buildEmptyTextPDF is a valid single-page PDF with an empty content stream:
// structurally a PDF, but with no text layer to extract.
func buildEmptyTextPDF() []byte {
	var buf bytes.Buffer
	offsets := make([]int, 0, 8)
	startObj := func() { offsets = append(offsets, buf.Len()) }

	buf.WriteString("%PDF-1.4\n")
	startObj()
	buf.WriteString("1 0 obj\n<< /Type /Catalog /Pages 2 0 R >>\nendobj\n")
	startObj()
	buf.WriteString("2 0 obj\n<< /Type /Pages /Kids [3 0 R] /Count 1 >>\nendobj\n")
	startObj()
	buf.WriteString("3 0 obj\n<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << >> /Contents 4 0 R >>\nendobj\n")
	startObj()
	buf.WriteString("4 0 obj\n<< /Length 0 >>\nstream\n\nendstream\nendobj\n")

	xrefStart := buf.Len()
	buf.WriteString("xref\n")
	buf.WriteString("0 " + strconv.Itoa(len(offsets)+1) + "\n")
	buf.WriteString("0000000000 65535 f \n")
	for _, off := range offsets {
		fmt.Fprintf(&buf, "%010d 00000 n \n", off)
	}
	buf.WriteString("trailer\n<< /Size " + strconv.Itoa(len(offsets)+1) + " /Root 1 0 R >>\n")
	buf.WriteString("startxref\n" + strconv.Itoa(xrefStart) + "\n%%EOF\n")
	return buf.Bytes()
}

// Malformed PDF bytes that pass the header check but break the parser must be
// turned into a clean error, never a panic that escapes the package.
func TestLoadDocumentMalformedDoesNotPanic(t *testing.T) {
	root := t.TempDir()
	bad := []byte("%PDF-1.4\nthis header is valid but the body and xref are garbage\nstartxref\n9\n%%EOF\n")
	if err := os.WriteFile(filepath.Join(root, "bad.pdf"), bad, 0o644); err != nil {
		t.Fatalf("write bad: %v", err)
	}
	_, err := LoadDocument("bad.pdf", root, DocumentOptions{disableExternalTools: true})
	if err == nil {
		t.Fatal("expected an error for malformed PDF bytes")
	}
}

// When the external poppler tools are absent (or disabled), extraction falls
// back to the pure-Go text path and still succeeds; absence is never an error.
func TestLoadDocumentFallsBackToPureGo(t *testing.T) {
	root := t.TempDir()
	want := "Pure Go fallback text"
	if err := os.WriteFile(filepath.Join(root, "doc.pdf"), buildMinimalPDF(want), 0o644); err != nil {
		t.Fatalf("write pdf: %v", err)
	}
	doc, err := LoadDocument("doc.pdf", root, DocumentOptions{disableExternalTools: true})
	if err != nil {
		t.Fatalf("LoadDocument (pure-Go): %v", err)
	}
	if !strings.Contains(doc.Text, want) {
		t.Fatalf("pure-Go text %q should contain %q", doc.Text, want)
	}
}

// Vision-mode extraction without an available rasterizer must not error: it
// degrades to the text layer (a vision model can still read the text block).
func TestLoadDocumentVisionWithoutRasterizerUsesText(t *testing.T) {
	root := t.TempDir()
	want := "Vision degrade to text"
	if err := os.WriteFile(filepath.Join(root, "doc.pdf"), buildMinimalPDF(want), 0o644); err != nil {
		t.Fatalf("write pdf: %v", err)
	}
	doc, err := LoadDocument("doc.pdf", root, DocumentOptions{Vision: true, disableExternalTools: true})
	if err != nil {
		t.Fatalf("LoadDocument (vision, no raster): %v", err)
	}
	if len(doc.Images) != 0 {
		t.Fatalf("no rasterizer available, expected 0 images, got %d", len(doc.Images))
	}
	if !strings.Contains(doc.Text, want) {
		t.Fatalf("vision-without-raster should keep text, got %q", doc.Text)
	}
}

// capDocumentText must keep the final payload (text + marker) at or under the
// advertised cap: the marker is counted against MaxDocumentTextBytes, not added
// on top of it.
func TestCapDocumentTextRespectsCap(t *testing.T) {
	// Text comfortably over the cap so truncation triggers.
	over := strings.Repeat("a", MaxDocumentTextBytes+1024)
	got, truncated := capDocumentText(over)
	if !truncated {
		t.Fatal("expected truncation for over-cap text")
	}
	if len(got) > MaxDocumentTextBytes {
		t.Fatalf("capped text length %d exceeds cap %d", len(got), MaxDocumentTextBytes)
	}
	if !strings.HasSuffix(got, documentTruncatedMarker) {
		t.Fatal("capped text should end with the truncation marker")
	}

	// At-or-under the cap is returned unchanged with no marker.
	under := strings.Repeat("b", MaxDocumentTextBytes)
	got, truncated = capDocumentText(under)
	if truncated {
		t.Fatal("text exactly at the cap must not be truncated")
	}
	if got != under {
		t.Fatal("at-cap text must be returned unchanged")
	}
}

// pdfPageCount must report the real page count from PDF bytes (this is what
// backs Document.Pages on the poppler text path, where pdftotext gives no count)
// and must return 0 -- not panic -- on garbage.
func TestPDFPageCount(t *testing.T) {
	if got := pdfPageCount(buildMinimalPDF("one page")); got != 1 {
		t.Fatalf("pdfPageCount = %d, want 1", got)
	}
	if got := pdfPageCount([]byte("not a pdf at all")); got != 0 {
		t.Fatalf("pdfPageCount on garbage = %d, want 0", got)
	}
}

// LooksLikeDocumentFile sniffs PDF content by magic bytes, so a real PDF with no
// ".pdf" extension is still recognized while a non-PDF (even named .pdf) is not.
func TestLooksLikeDocumentFile(t *testing.T) {
	root := t.TempDir()
	// A real PDF named without a .pdf extension.
	if err := os.WriteFile(filepath.Join(root, "spec"), buildMinimalPDF("hi"), 0o644); err != nil {
		t.Fatalf("write pdf: %v", err)
	}
	// A non-PDF named with a .pdf extension.
	if err := os.WriteFile(filepath.Join(root, "fake.pdf"), []byte("not a pdf"), 0o644); err != nil {
		t.Fatalf("write fake: %v", err)
	}
	// A directory must not be treated as a document.
	if err := os.Mkdir(filepath.Join(root, "adir"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	if !LooksLikeDocumentFile("spec", root) {
		t.Fatal("a real PDF without a .pdf extension should be recognized by content")
	}
	if LooksLikeDocumentFile("fake.pdf", root) {
		t.Fatal("a .pdf-named non-PDF must not be recognized as a document")
	}
	if LooksLikeDocumentFile("nope", root) {
		t.Fatal("a missing file must not be recognized as a document")
	}
	if LooksLikeDocumentFile("adir", root) {
		t.Fatal("a directory must not be recognized as a document")
	}
}

func TestIsProbablyDocumentPath(t *testing.T) {
	cases := map[string]bool{
		"report.pdf":      true,
		"REPORT.PDF":      true,
		"a/b/spec.Pdf":    true,
		"image.png":       false,
		"notes.txt":       false,
		"noext":           false,
		"archive.pdf.zip": false,
	}
	for path, want := range cases {
		if got := IsProbablyDocumentPath(path); got != want {
			t.Fatalf("IsProbablyDocumentPath(%q) = %v, want %v", path, got, want)
		}
	}
}
