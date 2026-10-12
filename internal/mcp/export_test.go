package mcp

import "github.com/Gitlawb/zero/internal/zeroruntime"

// DroppedContentSummary describes the blocks that were not forwarded, e.g.
// "1 audio/wav block" or "2 resource blocks, 1 image/png block". It returns ""
// when every block is text or an image that ImageBlocks successfully
// forwarded, so a caller adds nothing to the ordinary case.
func DroppedContentSummary(content []Content) string {
	_, disp, _ := forwardImages(content)
	return droppedContentNote(content, disp, dispDropped, dispBudgetExceeded, dispUninspected)
}

// ImageBlocks converts MCP image content into the same ImageBlock channel
// capture tools already use.
func ImageBlocks(content []Content) []zeroruntime.ImageBlock {
	images, _, _ := forwardImages(content)
	return images
}
