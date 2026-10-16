package tui

import (
	"strings"

	"github.com/Gitlawb/zero/internal/terminalpet"
)

const (
	attachmentImageID             = uint32(0xC0E0)
	attachmentPreviewCols         = 10
	attachmentPreviewRows         = 4
	attachmentPreviewHeightPixels = 64
	attachmentPreviewMaxImages    = 4
)

// attachmentThumbnailVisible deliberately has a conservative layout gate. The
// image is an enhancement to the existing attachment chips, not a reason to
// crowd a narrow or modal surface.
func (m model) attachmentThumbnailVisible(width int) bool {
	if len(m.attachmentRenderers) == 0 || !m.altScreen ||
		m.height < attachmentPreviewRows+10 || width < attachmentPreviewCols+28 {
		return false
	}
	if m.pendingPermission != nil || m.pendingAskUser != nil || m.pendingSpecReview != nil ||
		m.providerWizard != nil || m.mcpAddWizard != nil || m.mcpManager != nil || m.picker != nil ||
		m.sttKeyPrompt != nil || m.renamePrompt != nil || m.terminalAttach != nil || m.setup.visible || m.helpOverlay || m.leaderHelpOverlay {
		return false
	}
	for index := 0; index < m.attachmentThumbnailSlots(width); index++ {
		if index < len(m.pendingImageThumbnails) && m.pendingImageThumbnails[index] != nil &&
			index < len(m.attachmentRenderers) && m.attachmentRenderers[index].Support().Supported() {
			return true
		}
	}
	return false
}

func (m model) attachmentThumbnailSlots(width int) int {
	innerWidth := maxInt(1, width-m.petComposerReservedColumns(width)-4)
	slots := maxInt(1, innerWidth/(attachmentPreviewCols+1))
	return min(attachmentPreviewMaxImages, min(slots, len(m.attachmentRenderers)))
}

func (m model) attachmentThumbnailLines(width int) []string {
	left := strings.Repeat(" ", attachmentPreviewCols+1)
	lines := make([]string, attachmentPreviewRows)
	for index := range lines {
		lines[index] = left
	}
	for index, line := range lines {
		lines[index] = fitStyledLine(line, width)
	}
	return lines
}

func (m model) attachmentThumbnailSupplementalChips() string {
	if len(m.pendingImageLabels) <= 1 && len(m.pendingDocuments) == 0 {
		return ""
	}
	return renderAttachmentChips(m.pendingImageLabels, m.pendingDocuments)
}

func (m model) attachmentComposerPrefixRows(width int) int {
	if m.attachmentThumbnailVisible(width) {
		rows := attachmentPreviewRows
		if m.attachmentThumbnailSupplementalChips() != "" {
			rows++
		}
		return rows
	}
	if renderAttachmentChips(m.pendingImageLabels, m.pendingDocuments) != "" {
		return 1
	}
	return 0
}

// attachmentImageDraw places one gallery thumbnail inside the reserved top rows
// of the composer. The position comes from the same footer layout used for mouse
// input, so it follows sidebar changes and terminal resizes instead of relying on
// a brittle absolute bottom-right calculation.
func (m model) attachmentImageDraw(index int) *terminalpet.ImageDraw {
	width := m.chatColumnWidth()
	if !m.attachmentThumbnailVisible(width) || index < 0 || index >= m.attachmentThumbnailSlots(width) ||
		index >= len(m.pendingImageThumbnails) || m.pendingImageThumbnails[index] == nil {
		return nil
	}
	frame := m.scrollableTranscriptFrame(m.pinnedTitleBar(width), m.footerView(width))
	if frame.composerRect.height < attachmentPreviewRows+2 {
		return nil
	}
	return &terminalpet.ImageDraw{
		ID:           attachmentImageID + uint32(index),
		Animation:    m.pendingImageThumbnails[index],
		State:        terminalpet.Idle,
		X:            frame.composerRect.x + 2 + index*(attachmentPreviewCols+1),
		Y:            frame.composerRect.y + 1,
		Columns:      attachmentPreviewCols,
		Rows:         attachmentPreviewRows,
		HeightPixels: attachmentPreviewHeightPixels,
	}
}

func attachmentPreviewImageIDs() []uint32 {
	ids := make([]uint32, attachmentPreviewMaxImages)
	for index := range ids {
		ids[index] = attachmentImageID + uint32(index)
	}
	return ids
}
