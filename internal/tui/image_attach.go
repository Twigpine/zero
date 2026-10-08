package tui

import (
	"bytes"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/Gitlawb/zero/internal/imageinput"
	"github.com/Gitlawb/zero/internal/modelregistry"
	"github.com/Gitlawb/zero/internal/terminalpet"
	"github.com/Gitlawb/zero/internal/zeroruntime"
	_ "golang.org/x/image/webp"
)

const attachmentThumbnailMaxPixels = 16 << 20

// droppableImageExts are the image extensions a dragged-and-dropped file may
// carry (matched case-insensitively); PDFs are recognized separately.
var droppableImageExts = map[string]bool{
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true,
}

// droppedAttachmentPath recognizes a single drag-dropped (or pasted) file path
// that points at an existing image or PDF, returning the cleaned path. Terminals
// deliver a dropped file as its path with spaces/special chars backslash-escaped
// (or the whole path quoted); this undoes that so "Screenshot 2026 at 1.png"
// resolves. ok is false for anything that is not a single existing image/PDF
// file, so normal text pastes and real slash-commands are left untouched.
func droppedAttachmentPath(content, cwd string) (string, bool) {
	s := strings.TrimSpace(content)
	if s == "" || strings.ContainsAny(s, "\n\r") {
		return "", false // empty or multi-line: not a single dropped file
	}
	if unq, quoted := stripMatchingQuotes(s); quoted {
		s = unq // a quoted path is literal — do not unescape inside it
	} else {
		s = unescapeDroppedPath(s)
	}
	if s == "" {
		return "", false
	}
	resolved := s
	if !filepath.IsAbs(resolved) {
		resolved = filepath.Join(cwd, resolved)
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.Mode().IsRegular() {
		return "", false
	}
	ext := strings.ToLower(filepath.Ext(s))
	if droppableImageExts[ext] || ext == ".pdf" || imageinput.LooksLikeDocumentFile(s, cwd) {
		return s, true
	}
	return "", false
}

// unescapeDroppedPath drops a backslash before any following byte, undoing the
// terminal's drag-drop escaping ("\ " -> " ", "\(" -> "(", "\\" -> "\").
func unescapeDroppedPath(s string) string {
	if runtime.GOOS == "windows" {
		// On Windows the backslash is the path separator, not a drag-drop escape;
		// stripping it would corrupt real paths (C:\Users\… -> C:Users…). Dropped
		// paths there arrive quoted (handled by stripMatchingQuotes) or plain.
		return s
	}
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			i++
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// stripMatchingQuotes removes a single pair of surrounding ' or " quotes.
func stripMatchingQuotes(s string) (string, bool) {
	if len(s) >= 2 {
		q := s[0]
		if (q == '"' || q == '\'') && s[len(s)-1] == q {
			return s[1 : len(s)-1], true
		}
	}
	return s, false
}

// modelSupportsVisionTUI reports whether the active model can accept image
// input. It checks three sources in order:
//  1. The curated model registry (catalog authority + name heuristic)
//  2. The discovered model list from models.dev (if the live model picker
//     fetched it) — this carries InputModalities from models.dev, which
//     includes "image" for vision-capable models
//  3. Falls back to the name heuristic for unknown models
func (m model) modelSupportsVisionTUI() bool {
	trimmed := strings.TrimSpace(m.modelName)
	if trimmed == "" {
		return false
	}
	// The curated catalog is authoritative only when it knows the model.
	if entry, known := m.modelCatalog.Resolve(trimmed); known {
		return entry.Supports(modelregistry.ModelCapabilityVision)
	}
	// Check the discovered model list (from models.dev) for InputModalities
	// containing "image". This covers custom/ollama/cloud models not in the
	// curated catalog — models.dev knows their capabilities.
	for _, models := range m.modelPickerLiveByProvider {
		for _, dm := range models {
			if strings.EqualFold(strings.TrimSpace(dm.ID), trimmed) {
				// A provider's authenticated listing may only establish which models
				// are available, without repeating modalities. Treat an empty list as
				// unknown rather than as an explicit image-input denial, so the
				// curated registry/name capability fallback remains available while
				// models.dev metadata is temporarily unavailable.
				if len(dm.InputModalities) == 0 {
					continue
				}
				for _, modality := range dm.InputModalities {
					if strings.EqualFold(strings.TrimSpace(modality), "image") {
						return true
					}
				}
				return false // found the model in discovered list, no image modality
			}
		}
	}
	// Fall back to the name heuristic for models not in the catalog or
	// discovered list.
	return modelregistry.VisionCapableByName(trimmed)
}

// attachClipboardImage attaches an image read from the OS clipboard (a
// screenshot paste). Runs through the same vision gate + size cap as
// /image <path>, but the bytes come from the clipboard instead of a file.
func (m model) attachClipboardImage(data []byte, mediaType string) model {
	if !m.modelSupportsVisionTUI() {
		name := m.modelName
		if name == "" {
			name = "the active model"
		}
		return m.appendImageNotice("Model " + name + " does not support image input; clipboard image refused.")
	}
	if len(data) > imageinput.MaxImageBytes {
		return m.appendImageNotice("Clipboard image is larger than the 10 MiB limit.")
	}
	m.pendingImages = append(m.pendingImages, zeroruntime.ImageBlock{
		MediaType: mediaType,
		Data:      data,
	})
	m.pendingImageLabels = append(m.pendingImageLabels, "clipboard")
	m.refreshPendingImageThumbnail()
	return m
}

// handleImageCommand processes "/image <path>" and "/image clear". A bare
// "/image" prints usage. PDFs are routed to the document path (text layer always
// attaches; pages rasterize to images only for vision models with a rasterizer).
// Image files attach only to vision models. Attachment failures (missing file,
// unsupported type, oversize) surface as an inline notice and attach nothing.
func (m model) handleImageCommand(arg string) model {
	trimmed := strings.TrimSpace(arg)
	switch {
	case trimmed == "":
		return m.appendImageNotice("Usage: /image <path>  (image or PDF; or /image clear)")
	case strings.EqualFold(trimmed, "clear"):
		m.pendingImages = nil
		m.pendingImageLabels = nil
		m.pendingImageThumbnails = nil
		m.pendingDocuments = nil
		return m.appendImageNotice("Cleared pending attachments.")
	}

	// A PDF carries a text layer every model can read, so it is not gated on
	// vision the way a raw image is; the optional rasterized pages are. Route by
	// the ".pdf" hint OR a content sniff so a real PDF whose name lacks the
	// extension still reaches the document path rather than the vision-only image
	// path. The cheap header sniff runs before the vision gate.
	if imageinput.IsProbablyDocumentPath(trimmed) || imageinput.LooksLikeDocumentFile(trimmed, m.cwd) {
		return m.handleDocumentAttach(trimmed)
	}

	if !m.modelSupportsVisionTUI() {
		name := m.modelName
		if name == "" {
			name = "the active model"
		}
		return m.appendImageNotice("Model " + name + " does not support image input; attachment refused.")
	}

	block, err := imageinput.LoadFile(trimmed, m.cwd)
	if err != nil {
		return m.appendImageNotice(err.Error())
	}

	m.pendingImages = append(m.pendingImages, block)
	m.pendingImageLabels = append(m.pendingImageLabels, filepath.Base(trimmed))
	m.refreshPendingImageThumbnail()
	// No "attached" system message: the composer attachment chip ([Image #N]) is
	// the confirmation, matching the compact attach UX.
	return m
}

// pendingDocument is a PDF staged by /image for the next user turn: its extracted
// text layer (prepended to the prompt at submit time) and a display label.
type pendingDocument struct {
	label string
	text  string
}

// handleDocumentAttach loads a PDF through imageinput.LoadDocument. The text
// layer is staged for every model; when the active model supports vision and a
// rasterizer is available, the rendered pages are staged through the existing
// pending-image pipeline too. A scanned PDF with no text (and no rasterizer)
// surfaces LoadDocument's explicit "no extractable text" notice and attaches
// nothing.
func (m model) handleDocumentAttach(path string) model {
	doc, err := imageinput.LoadDocument(path, m.cwd, imageinput.DocumentOptions{
		Vision: m.modelSupportsVisionTUI(),
	})
	if err != nil {
		return m.appendImageNotice(err.Error())
	}

	label := filepath.Base(path)
	if strings.TrimSpace(doc.Text) != "" {
		m.pendingDocuments = append(m.pendingDocuments, pendingDocument{label: label, text: doc.Text})
	}
	for _, block := range doc.Images {
		m.pendingImages = append(m.pendingImages, block)
		m.pendingImageLabels = append(m.pendingImageLabels, label)
	}
	m.refreshPendingImageThumbnail()
	// The composer attachment chip ([Doc #N] / [Image #N]) is the confirmation; no
	// "attached" system message.
	return m
}

// consumePendingDocuments returns the staged document text formatted as a prompt
// preamble and clears the pending documents. The preamble names each document so
// the model can attribute the text; an empty result means nothing was staged.
func (m *model) consumePendingDocuments() string {
	if len(m.pendingDocuments) == 0 {
		return ""
	}
	var b strings.Builder
	for _, doc := range m.pendingDocuments {
		b.WriteString("Attached document: ")
		b.WriteString(doc.label)
		b.WriteString("\n")
		b.WriteString(doc.text)
		b.WriteString("\n\n")
	}
	m.pendingDocuments = nil
	return b.String()
}

// appendImageNotice appends an image-related notice to the transcript. Image
// errors (vision gate refusal, oversize, unsupported type) render with a red
// error border + red text so they stand out from ordinary grey system notes.
func (m model) appendImageNotice(text string) model {
	row := transcriptRow{
		kind: rowError,
		text: text,
	}
	m.transcript = appendTranscriptRow(m.transcript, row)
	return m
}

// removeLastAttachment drops the rightmost pending attachment chip and reports
// whether anything was removed. Documents render after images, so a staged
// document is removed before images; image pops keep pendingImages and
// pendingImageLabels in lockstep.
func (m model) removeLastAttachment() (model, bool) {
	if n := len(m.pendingDocuments); n > 0 {
		m.pendingDocuments = m.pendingDocuments[:n-1]
		return m, true
	}
	if n := len(m.pendingImageLabels); n > 0 {
		m.pendingImageLabels = m.pendingImageLabels[:n-1]
		if len(m.pendingImages) > 0 {
			m.pendingImages = m.pendingImages[:len(m.pendingImages)-1]
		}
		m.refreshPendingImageThumbnail()
		return m, true
	}
	return m, false
}

// refreshPendingImageThumbnail prepares only the first pending image for the
// optional inline terminal preview. Decode failures deliberately leave the
// attachment intact: the text chip remains the portable fallback and the model
// still receives the original bytes.
func (m *model) refreshPendingImageThumbnail() {
	m.pendingImageThumbnails = nil
	if len(m.pendingImages) == 0 {
		return
	}
	count := min(len(m.pendingImages), attachmentPreviewMaxImages)
	m.pendingImageThumbnails = make([]*terminalpet.Animation, count)
	for index := 0; index < count; index++ {
		preview, err := attachmentThumbnail(m.pendingImages[index])
		if err == nil {
			m.pendingImageThumbnails[index] = preview
		}
	}
}

func attachmentThumbnail(block zeroruntime.ImageBlock) (*terminalpet.Animation, error) {
	config, _, err := image.DecodeConfig(bytes.NewReader(block.Data))
	if err != nil {
		return nil, err
	}
	if config.Width < 1 || config.Height < 1 || config.Width > attachmentThumbnailMaxPixels/config.Height {
		return nil, fmt.Errorf("image is too large for an inline thumbnail")
	}
	decoded, _, err := image.Decode(bytes.NewReader(block.Data))
	if err != nil {
		return nil, err
	}
	return terminalpet.ThumbnailAnimation(decoded)
}

// renderAttachmentChips builds the pending-attachment row from both staged images
// and staged documents, e.g. "[Image #1] [Image #2] [Doc #1]". Returns "" when
// nothing is staged. Numbered (not named) so a long screenshot path never shows
// in the composer.
// visionDropWarning returns a one-line notice when images are staged but the
// (now active) model can't accept them, so switching to a non-vision model warns
// the user immediately at switch time instead of silently dropping the images at
// submit. Empty when there is nothing staged or the model supports vision.
func (m model) visionDropWarning() string {
	if len(m.pendingImages) == 0 || m.modelSupportsVisionTUI() {
		return ""
	}
	return fmt.Sprintf("⚠ %d staged image(s) will be dropped — %s has no vision support.",
		len(m.pendingImages), displayValue(m.modelName, "the active model"))
}

func renderAttachmentChips(imageLabels []string, docs []pendingDocument) string {
	chips := make([]string, 0, len(imageLabels)+len(docs))
	for i := range imageLabels {
		chips = append(chips, fmt.Sprintf("[Image #%d]", i+1))
	}
	for i := range docs {
		chips = append(chips, fmt.Sprintf("[Doc #%d]", i+1))
	}
	return strings.Join(chips, " ")
}
