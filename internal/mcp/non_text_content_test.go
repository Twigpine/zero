package mcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Gitlawb/zero/internal/imageinput"
	"github.com/Gitlawb/zero/internal/tools"
)

// tinyPNGBase64 is a 1x1 PNG. http.DetectContentType sniffs it as image/png.
const tinyPNGBase64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="

// paddedPNGBase64 is a decodable image/png of decoded length n. The 8-byte
// PNG magic is enough for http.DetectContentType; the rest is padding so
// tests can hit size caps without committing a multi-mebibyte fixture.
func paddedPNGBase64(n int) string {
	raw := make([]byte, n)
	copy(raw, "\x89PNG\r\n\x1a\n")
	return base64.StdEncoding.EncodeToString(raw)
}

// A server that returns an image WITHOUT payload still cannot be forwarded, so
// the delivered result must name the block rather than report "(empty MCP tool
// result)". The model then usually retries, which is the worst outcome (#823).
//
// Image blocks that DO carry data are forwarded on Result.Images (see the
// payload tests below). This case is the remaining drop path, and it has to be
// true of the DELIVERED result, so this drives registryTool.Run rather than
// the helper alone.
func TestAnImageOnlyResultSaysWhatItReturned(t *testing.T) {
	tool := registryTool{
		client: &nonTextClient{content: []Content{
			{Type: "image", MimeType: "image/png"},
		}},
		server: Server{Name: "shots"},
		remote: RemoteTool{Name: "screenshot"},
	}

	result := tool.Run(context.Background(), map[string]any{})

	if strings.Contains(result.Output, "(empty MCP tool result)") {
		t.Fatalf("an image-only result still reports empty, so the model will retry:\n%s", result.Output)
	}
	if !strings.Contains(result.Output, "image/png") {
		t.Errorf("the output does not name what the server returned:\n%s", result.Output)
	}
	// Naming the block is only half of it. Without the guidance the model still
	// retries, which is the expensive symptom, so the wording is pinned too.
	//
	// "cannot recover this payload" and not "will return the same thing": every
	// retry is a fresh call and the server may answer differently. What cannot
	// change is that Zero has nowhere to put a non-text block.
	if !strings.Contains(result.Output, "Retrying cannot recover this payload.") {
		t.Errorf("the output does not tell the model retrying is pointless:\n%s", result.Output)
	}
	if strings.Contains(result.Output, "will return the same thing") {
		t.Errorf("the output promises an identical response, which a fresh call cannot guarantee:\n%s", result.Output)
	}
	if result.Status != tools.StatusOK {
		t.Errorf("status = %v, want OK: the call succeeded, we just cannot forward the payload", result.Status)
	}
}

// The quieter half of the same bug: when a result carries text AND an image,
// the text arrives and the image vanishes with no mention at all.
func TestTextAlongsideAnImageStillReportsTheImage(t *testing.T) {
	tool := registryTool{
		client: &nonTextClient{content: []Content{
			{Type: "text", Text: "captured the page"},
			{Type: "image", MimeType: "image/png"},
		}},
		server: Server{Name: "shots"},
		remote: RemoteTool{Name: "screenshot"},
	}

	result := tool.Run(context.Background(), map[string]any{})

	if !strings.Contains(result.Output, "captured the page") {
		t.Errorf("the text block was lost:\n%s", result.Output)
	}
	if !strings.Contains(result.Output, "image/png") {
		t.Errorf("the dropped image was not mentioned:\n%s", result.Output)
	}
}

// A text-only result must be byte-for-byte what it was before: this change adds
// a line only when something was actually dropped.
func TestATextOnlyResultIsUnchanged(t *testing.T) {
	tool := registryTool{
		client: &nonTextClient{content: []Content{{Type: "text", Text: "plain answer"}}},
		server: Server{Name: "shots"},
		remote: RemoteTool{Name: "lookup"},
	}

	if got := tool.Run(context.Background(), map[string]any{}).Output; got != "plain answer" {
		t.Fatalf("output = %q, want exactly %q", got, "plain answer")
	}
}

func TestDroppedContentSummaryNamesTheBlocks(t *testing.T) {
	tests := []struct {
		name    string
		content []Content
		want    string
	}{
		{
			name:    "nothing dropped",
			content: []Content{{Type: "text", Text: "hi"}},
			want:    "",
		},
		{
			name:    "no content at all",
			content: nil,
			want:    "",
		},
		{
			name:    "one image with a mime type",
			content: []Content{{Type: "image", MimeType: "image/png"}},
			want:    "1 image/png block",
		},
		{
			// A server may omit mimeType. Fall back to the block type rather than
			// inventing one or printing an empty pair of slashes.
			name:    "one image without a mime type",
			content: []Content{{Type: "image"}},
			want:    "1 image block",
		},
		{
			name: "several of the same kind are counted, not repeated",
			content: []Content{
				{Type: "image", MimeType: "image/png"},
				{Type: "image", MimeType: "image/png"},
			},
			want: "2 image/png blocks",
		},
		{
			// Order follows first appearance so the message is stable to read and
			// to assert on.
			name: "mixed kinds",
			content: []Content{
				{Type: "text", Text: "ignored here"},
				{Type: "resource"},
				{Type: "audio", MimeType: "audio/wav"},
				{Type: "resource"},
			},
			want: "2 resource blocks, 1 audio/wav block",
		},
		{
			name:    "successfully forwarded image is not named as dropped",
			content: []Content{{Type: "image", MimeType: "image/png", Data: tinyPNGBase64}},
			want:    "",
		},
		{
			name:    "malformed image data is still named",
			content: []Content{{Type: "image", MimeType: "image/png", Data: "%%%not-base64%%%"}},
			want:    "1 image/png block",
		},
		{
			name: "forwarded image plus audio names only the audio",
			content: []Content{
				{Type: "image", MimeType: "image/png", Data: tinyPNGBase64},
				{Type: "audio", MimeType: "audio/wav"},
			},
			want: "1 audio/wav block",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := DroppedContentSummary(test.content); got != test.want {
				t.Fatalf("DroppedContentSummary() = %q, want %q", got, test.want)
			}
		})
	}
}

type nonTextClient struct {
	content []Content
}

func (client *nonTextClient) ListTools(context.Context) ([]RemoteTool, error) { return nil, nil }

func (client *nonTextClient) CallTool(context.Context, string, map[string]any) (CallToolResult, error) {
	return CallToolResult{Content: client.content}, nil
}

func (client *nonTextClient) Close() error { return nil }

func TestAnImageWithPayloadIsForwarded(t *testing.T) {
	tool := registryTool{
		client: &nonTextClient{content: []Content{
			{Type: "image", MimeType: "image/png", Data: tinyPNGBase64},
		}},
		server: Server{Name: "shots"},
		remote: RemoteTool{Name: "screenshot"},
	}

	result := tool.Run(context.Background(), map[string]any{})

	if result.Output != "[image returned by tool]" {
		t.Fatalf("image-only Output = %q, want [image returned by tool] so the tool_result is not an empty body", result.Output)
	}
	if strings.Contains(result.Output, "cannot forward") {
		t.Fatalf("a forwarded image is still described as unforwardable:\n%s", result.Output)
	}
	if len(result.Images) != 1 {
		t.Fatalf("Images len = %d, want 1", len(result.Images))
	}
	if result.Images[0].MediaType != "image/png" {
		t.Errorf("MediaType = %q, want image/png", result.Images[0].MediaType)
	}
	if len(result.Images[0].Data) == 0 {
		t.Fatal("forwarded image has empty Data")
	}
	if DroppedContentSummary([]Content{{Type: "image", MimeType: "image/png", Data: tinyPNGBase64}}) != "" {
		t.Fatal("drop summary named a successfully forwarded image")
	}
	if result.Status != tools.StatusOK {
		t.Errorf("status = %v, want OK", result.Status)
	}
}

func TestAudioIsStillDroppedAndNamed(t *testing.T) {
	tool := registryTool{
		client: &nonTextClient{content: []Content{
			{Type: "audio", MimeType: "audio/wav"},
		}},
		server: Server{Name: "shots"},
		remote: RemoteTool{Name: "clip"},
	}

	result := tool.Run(context.Background(), map[string]any{})

	if len(result.Images) != 0 {
		t.Fatalf("audio was forwarded as an image: %#v", result.Images)
	}
	if !strings.Contains(result.Output, "audio/wav") {
		t.Errorf("the output does not name the dropped audio:\n%s", result.Output)
	}
	if !strings.Contains(result.Output, "cannot forward yet") {
		t.Errorf("the output does not say the audio cannot be forwarded:\n%s", result.Output)
	}
}

func TestTextAndImageKeepsTextAndForwardsImage(t *testing.T) {
	tool := registryTool{
		client: &nonTextClient{content: []Content{
			{Type: "text", Text: "captured the page"},
			{Type: "image", MimeType: "image/png", Data: tinyPNGBase64},
		}},
		server: Server{Name: "shots"},
		remote: RemoteTool{Name: "screenshot"},
	}

	result := tool.Run(context.Background(), map[string]any{})

	if !strings.Contains(result.Output, "captured the page") {
		t.Errorf("the text block was lost:\n%s", result.Output)
	}
	if strings.Contains(result.Output, "cannot forward") {
		t.Errorf("a forwarded image is still described as unforwardable:\n%s", result.Output)
	}
	if strings.Contains(result.Output, "[image forwarded]") {
		t.Errorf("text+image result substituted a placeholder over the text:\n%s", result.Output)
	}
	if len(result.Images) != 1 {
		t.Fatalf("Images len = %d, want 1", len(result.Images))
	}
	if result.Images[0].MediaType != "image/png" {
		t.Errorf("MediaType = %q, want image/png", result.Images[0].MediaType)
	}
}

func TestMalformedImageDataDoesNotPanic(t *testing.T) {
	tool := registryTool{
		client: &nonTextClient{content: []Content{
			{Type: "image", MimeType: "image/png", Data: "%%%not-base64%%%"},
		}},
		server: Server{Name: "shots"},
		remote: RemoteTool{Name: "screenshot"},
	}

	result := tool.Run(context.Background(), map[string]any{})

	if len(result.Images) != 0 {
		t.Fatalf("malformed image was forwarded: %#v", result.Images)
	}
	if !strings.Contains(result.Output, "image/png") {
		t.Errorf("malformed image was not named in the drop summary:\n%s", result.Output)
	}
	if strings.Contains(result.Output, "(empty MCP tool result)") {
		t.Errorf("malformed image still reported as empty:\n%s", result.Output)
	}
}

func TestImageContentJSONDecodesDataAndStaysCompatibleWithoutIt(t *testing.T) {
	var withData CallToolResult
	raw := []byte(`{"content":[{"type":"image","mimeType":"image/png","data":"` + tinyPNGBase64 + `"}]}`)
	if err := json.Unmarshal(raw, &withData); err != nil {
		t.Fatalf("unmarshal image content: %v", err)
	}
	if len(withData.Content) != 1 {
		t.Fatalf("content len = %d, want 1", len(withData.Content))
	}
	if withData.Content[0].Type != "image" || withData.Content[0].MimeType != "image/png" {
		t.Fatalf("decoded fields = %+v", withData.Content[0])
	}
	if withData.Content[0].Data != tinyPNGBase64 {
		t.Fatalf("data = %q, want tiny PNG base64", withData.Content[0].Data)
	}

	var withoutData CallToolResult
	if err := json.Unmarshal([]byte(`{"content":[{"type":"image","mimeType":"image/png"}]}`), &withoutData); err != nil {
		t.Fatalf("unmarshal image content without data: %v", err)
	}
	if withoutData.Content[0].Data != "" {
		t.Fatalf("absent data decoded as %q, want empty", withoutData.Content[0].Data)
	}
}

func TestAnExactlyMaxImageBytesPaddedPNGIsForwarded(t *testing.T) {
	payload := paddedPNGBase64(imageinput.MaxImageBytes)
	if got := base64.StdEncoding.DecodedLen(len(payload)); got <= imageinput.MaxImageBytes {
		t.Fatalf("fixture DecodedLen = %d, want > %d so the old bound would reject it", got, imageinput.MaxImageBytes)
	}
	tool := registryTool{
		client: &nonTextClient{content: []Content{
			{Type: "image", MimeType: "image/png", Data: payload},
		}},
		server: Server{Name: "shots"},
		remote: RemoteTool{Name: "screenshot"},
	}

	result := tool.Run(context.Background(), map[string]any{})
	if len(result.Images) != 1 {
		t.Fatalf("at-limit padded PNG was dropped: images=%d output=%q", len(result.Images), result.Output)
	}
	if got := len(result.Images[0].Data); got != imageinput.MaxImageBytes {
		t.Fatalf("forwarded size = %d, want %d", got, imageinput.MaxImageBytes)
	}
}

func TestAnOversizedImageIsDroppedAndNamed(t *testing.T) {
	tool := registryTool{
		client: &nonTextClient{content: []Content{
			{Type: "image", MimeType: "image/png", Data: paddedPNGBase64(imageinput.MaxImageBytes + 1)},
		}},
		server: Server{Name: "shots"},
		remote: RemoteTool{Name: "screenshot"},
	}

	result := tool.Run(context.Background(), map[string]any{})

	if len(result.Images) != 0 {
		t.Fatalf("oversized image was forwarded: %#v", result.Images)
	}
	if !strings.Contains(result.Output, "image/png") {
		t.Errorf("oversized image was not named in the drop summary:\n%s", result.Output)
	}
	if !strings.Contains(result.Output, "cannot forward yet") {
		t.Errorf("individually oversized image should stay unforwardable, not a budget skip:\n%s", result.Output)
	}
	if strings.Contains(result.Output, "image budget") {
		t.Errorf("individually oversized image was described as a budget skip:\n%s", result.Output)
	}
}

func TestAggregateImageBudgetForwardsTheFirstAndNamesTheRest(t *testing.T) {
	// Each payload is under the per-image cap; together they exceed the
	// aggregate MaxImageBytes budget for one result. Identical bytes are
	// deliberate: DroppedContentSummary must name the second even though
	// imageBlockFromContent would accept it in isolation.
	payload := paddedPNGBase64(imageinput.MaxImageBytes/2 + 1)
	tool := registryTool{
		client: &nonTextClient{content: []Content{
			{Type: "image", MimeType: "image/png", Data: payload},
			{Type: "image", MimeType: "image/png", Data: payload},
		}},
		server: Server{Name: "shots"},
		remote: RemoteTool{Name: "screenshot"},
	}

	result := tool.Run(context.Background(), map[string]any{})

	if len(result.Images) != 1 {
		t.Fatalf("Images len = %d, want 1 (first fits the aggregate budget)", len(result.Images))
	}
	if got := len(result.Images[0].Data); got != imageinput.MaxImageBytes/2+1 {
		t.Errorf("forwarded image size = %d, want %d", got, imageinput.MaxImageBytes/2+1)
	}
	if !strings.Contains(result.Output, "[image returned by tool]") {
		t.Errorf("the forwarded first image has no placeholder:\n%s", result.Output)
	}
	if !strings.Contains(result.Output, "image/png") {
		t.Errorf("the dropped second image was not named:\n%s", result.Output)
	}
	if !strings.Contains(result.Output, "which exceeded this result's remaining image budget") {
		t.Errorf("the dropped second image is not described as a budget exceeded:\n%s", result.Output)
	}
	if !strings.Contains(result.Output, "Retrying with fewer images can recover this payload.") {
		t.Errorf("the output does not tell the model a retry can recover the payload:\n%s", result.Output)
	}
	if strings.Contains(result.Output, "cannot forward yet") {
		t.Errorf("a budget-exceeded image is described as unforwardable:\n%s", result.Output)
	}
	if strings.Contains(result.Output, "Retrying cannot recover this payload.") {
		t.Errorf("a budget-exceeded image is described as unrecoverable:\n%s", result.Output)
	}
}

func TestImagePayloadsAreDecodedOnceAndNotPastTheBudget(t *testing.T) {
	orig := decodeImageBase64
	t.Cleanup(func() { decodeImageBase64 = orig })
	var n int
	decodeImageBase64 = func(s string) ([]byte, error) {
		n++
		return orig(s)
	}

	// Two images fill the 10 MiB aggregate exactly, so remaining hits 0 and
	// the later candidates must not be decoded at all.
	half := imageinput.MaxImageBytes / 2
	payload := paddedPNGBase64(half)
	content := []Content{
		{Type: "image", MimeType: "image/png", Data: payload},
		{Type: "image", MimeType: "image/png", Data: payload},
		{Type: "image", MimeType: "image/png", Data: payload},
		{Type: "image", MimeType: "image/png", Data: "not-even-valid-base64!"},
		{Type: "image", MimeType: "image/png", Data: ""},
		{Type: "audio", MimeType: "audio/wav"},
	}

	n = 0
	images := ImageBlocks(content)
	if n != 2 {
		t.Fatalf("ImageBlocks decoded %d payloads, want 2 (budget fills after two %d-byte images)", n, half)
	}
	if len(images) != 2 {
		t.Fatalf("ImageBlocks len = %d, want 2", len(images))
	}
	n = 0
	if got := DroppedContentSummary(content); got != "3 image/png blocks, 1 audio/wav block" {
		t.Fatalf("DroppedContentSummary() = %q, want the three skipped images and the audio", got)
	}
	if n != 2 {
		t.Fatalf("DroppedContentSummary decoded %d payloads, want 2 (same single pass as ImageBlocks)", n)
	}

	n = 0
	result := registryTool{
		client: &nonTextClient{content: content},
		server: Server{Name: "shots"},
		remote: RemoteTool{Name: "screenshot"},
	}.Run(context.Background(), map[string]any{})
	if n != 2 {
		t.Fatalf("Run decoded %d payloads, want 2 (one pass; drop note must not decode again)", n)
	}
	if len(result.Images) != 2 {
		t.Fatalf("Images len = %d, want 2", len(result.Images))
	}
	if !strings.Contains(result.Output, "[image returned by tool]") {
		t.Errorf("forwarded images have no placeholder:\n%s", result.Output)
	}
	if !strings.Contains(result.Output, "image/png") {
		t.Errorf("skipped images were not named:\n%s", result.Output)
	}
	if !strings.Contains(result.Output, "which were not inspected because the aggregate image budget was reached") {
		t.Errorf("uninspected images are not described correctly:\n%s", result.Output)
	}
	if strings.Contains(result.Output, "Retrying with fewer images can recover this payload.") {
		t.Errorf("uninspected images must not make unsupported recovery claims:\n%s", result.Output)
	}
	if !strings.Contains(result.Output, "audio/wav") {
		t.Errorf("audio was not named:\n%s", result.Output)
	}
	if !strings.Contains(result.Output, "cannot forward yet") {
		t.Errorf("audio is not described as unforwardable:\n%s", result.Output)
	}
	if strings.Contains(result.Output, "(empty MCP tool result)") {
		t.Errorf("forwarded images still reported as empty:\n%s", result.Output)
	}

	n = 0
	one := []Content{{Type: "image", MimeType: "image/png", Data: payload}}
	oneResult := registryTool{
		client: &nonTextClient{content: one},
		server: Server{Name: "shots"},
		remote: RemoteTool{Name: "screenshot"},
	}.Run(context.Background(), map[string]any{})
	if n != 1 {
		t.Fatalf("one image decoded %d times, want 1", n)
	}
	if len(oneResult.Images) != 1 {
		t.Fatalf("one-image Images len = %d, want 1", len(oneResult.Images))
	}
	if oneResult.Output != "[image returned by tool]" {
		t.Fatalf("one-image Output = %q, want [image returned by tool]", oneResult.Output)
	}
	if strings.Contains(oneResult.Output, "cannot forward") {
		t.Fatalf("a forwarded image is still described as unforwardable:\n%s", oneResult.Output)
	}
}

func TestImageBudgetNonZeroResidueAllowsSmallerLaterImage(t *testing.T) {
	// First image: 8 MiB (fits, 2 MiB left)
	// Second image: 3 MiB (exceeds remaining 2 MiB, budgetExceeded)
	// Third image: 1 MiB (fits in remaining 2 MiB, forwarded, 1 MiB left)
	img8 := paddedPNGBase64(8 * 1024 * 1024)
	img3 := paddedPNGBase64(3 * 1024 * 1024)
	img1 := paddedPNGBase64(1 * 1024 * 1024)

	content := []Content{
		{Type: "image", MimeType: "image/png", Data: img8},
		{Type: "image", MimeType: "image/png", Data: img3},
		{Type: "image", MimeType: "image/png", Data: img1},
	}

	images, disp, _ := forwardImages(content)
	if len(images) != 2 {
		t.Fatalf("forwardImages len = %d, want 2 (8 MiB + 1 MiB)", len(images))
	}
	if disp[0] != dispForwarded || disp[1] != dispBudgetExceeded || disp[2] != dispForwarded {
		t.Fatalf("dispositions = %v, want [forwarded, budgetExceeded, forwarded]", disp)
	}

	result := registryTool{
		client: &nonTextClient{content: content},
		server: Server{Name: "shots"},
		remote: RemoteTool{Name: "screenshot"},
	}.Run(context.Background(), map[string]any{})

	if len(result.Images) != 2 {
		t.Fatalf("result Images len = %d, want 2", len(result.Images))
	}
	if !strings.Contains(result.Output, "exceeded this result's remaining image budget") {
		t.Fatalf("expected remaining budget notice in output:\n%s", result.Output)
	}
	if !strings.Contains(result.Output, "Retrying with fewer images can recover this payload.") {
		t.Fatalf("expected retry recovery guidance for validated exceeded image:\n%s", result.Output)
	}
}

func TestImageCountBudgetPreservesTextAndSkipsFurtherDecoding(t *testing.T) {
	content := []Content{{Type: "image", Data: "invalid"}}
	for range 17 {
		content = append(content, Content{Type: "image", MimeType: "image/png", Data: tinyPNGBase64})
	}
	content = append(content, Content{Type: "text", Text: "trailing text"})
	previous := decodeImageBase64
	decodes := 0
	decodeImageBase64 = func(s string) ([]byte, error) {
		decodes++
		return previous(s)
	}
	t.Cleanup(func() { decodeImageBase64 = previous })
	result := registryTool{
		client: &nonTextClient{content: content},
		server: Server{Name: "shots"}, remote: RemoteTool{Name: "screenshot"},
	}.Run(context.Background(), map[string]any{})
	if len(result.Images) != 16 || decodes != 17 {
		t.Fatalf("forwarded %d images with %d decodes, want 16 images and 17 attempts including invalid input", len(result.Images), decodes)
	}
	if !strings.Contains(result.Output, "trailing text") || !strings.Contains(result.Output, "1 image/png block, which was not inspected") {
		t.Fatalf("missing text or count-budget notice: %s", result.Output)
	}
}

func TestImageInspectionBudgetBoundsRejectedCandidates(t *testing.T) {
	// Use the decoder seam to exercise accounting without allocating a large
	// payload for every rejected candidate.
	previous := decodeImageBase64
	decodes := 0
	large, _ := previous(paddedPNGBase64(8 * 1024 * 1024))
	rejected, _ := previous(paddedPNGBase64(3 * 1024 * 1024))
	decodeImageBase64 = func(string) ([]byte, error) {
		decodes++
		if decodes == 1 {
			return large, nil
		}
		return rejected, nil
	}
	t.Cleanup(func() { decodeImageBase64 = previous })
	content := make([]Content, 40)
	for i := range content {
		content[i] = Content{Type: "image", MimeType: "image/png", Data: tinyPNGBase64}
	}
	content = append(content, Content{Type: "text", Text: "trailing text"})
	images, disp, _ := forwardImages(content)
	if decodes != 32 || len(images) != 1 {
		t.Fatalf("got %d decodes and %d forwarded images, want 32 and 1", decodes, len(images))
	}
	for i := 1; i < 40; i++ {
		want := dispBudgetExceeded
		if i >= 32 {
			want = dispUninspected
		}
		if disp[i] != want {
			t.Errorf("disposition[%d] = %v, want %v", i, disp[i], want)
		}
	}
	if disp[40] != dispText {
		t.Fatal("inspection limit discarded trailing text")
	}
}

func BenchmarkForwardImagesFourHalfBudget(b *testing.B) {
	payload := paddedPNGBase64(imageinput.MaxImageBytes / 2)
	content := []Content{
		{Type: "image", MimeType: "image/png", Data: payload},
		{Type: "image", MimeType: "image/png", Data: payload},
		{Type: "image", MimeType: "image/png", Data: payload},
		{Type: "image", MimeType: "image/png", Data: payload},
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _, _ = forwardImages(content)
	}
}

func TestMCPResultUninspectedImageLimitingReasonNotes(t *testing.T) {
	t.Run("forwarded count limit reached", func(t *testing.T) {
		content := make([]Content, 20)
		for i := range content {
			content[i] = Content{Type: "image", MimeType: "image/png", Data: tinyPNGBase64}
		}
		result := registryTool{
			client: &nonTextClient{content: content},
			server: Server{Name: "shots"},
			remote: RemoteTool{Name: "screenshot"},
		}.Run(context.Background(), map[string]any{})
		if len(result.Images) != 16 {
			t.Fatalf("expected 16 forwarded images, got %d", len(result.Images))
		}
		if !strings.Contains(result.Output, "which were not inspected because the maximum forwarded image count was reached.") {
			t.Fatalf("expected maximum forwarded image count message, got:\n%s", result.Output)
		}
	})

	t.Run("inspection limit reached", func(t *testing.T) {
		previous := decodeImageBase64
		large := make([]byte, 1024)
		rejected := make([]byte, imageinput.MaxImageBytes+1)
		decodes := 0
		decodeImageBase64 = func(s string) ([]byte, error) {
			decodes++
			if decodes == 1 {
				return large, nil
			}
			return rejected, nil
		}
		t.Cleanup(func() { decodeImageBase64 = previous })
		content := make([]Content, 35)
		for i := range content {
			content[i] = Content{Type: "image", MimeType: "image/png", Data: tinyPNGBase64}
		}
		result := registryTool{
			client: &nonTextClient{content: content},
			server: Server{Name: "shots"},
			remote: RemoteTool{Name: "screenshot"},
		}.Run(context.Background(), map[string]any{})
		if !strings.Contains(result.Output, "which were not inspected because the maximum image inspection limit was reached.") {
			t.Fatalf("expected maximum image inspection limit message, got:\n%s", result.Output)
		}
	})
}

// The pre-decode encoded-length cap is the decode-DoS guard: it keeps a
// hostile server from making Zero decode a payload of any size (up to the
// inspection cap times per result) before the post-decode backstop drops it.
// Removing the cap changes nothing that is forwarded, so no existing test
// notices; this pins the guard on the decode hook instead. A payload one
// byte over the encoded cap must be rejected without the decoder ever being
// called.
func TestOverCapEncodedPayloadIsRejectedBeforeDecoding(t *testing.T) {
	previous := decodeImageBase64
	decodes := 0
	decodeImageBase64 = func(s string) ([]byte, error) {
		decodes++
		return previous(s)
	}
	t.Cleanup(func() { decodeImageBase64 = previous })

	// An at-limit PNG would decode to exactly MaxImageBytes and forward; a
	// single extra base64 character pushes it one byte over the encoded cap,
	// so only the pre-decode guard can be responsible for rejecting it.
	payload := paddedPNGBase64(imageinput.MaxImageBytes) + "A"
	content := []Content{{Type: "image", MimeType: "image/png", Data: payload}}

	if images := ImageBlocks(content); len(images) != 0 {
		t.Fatalf("ImageBlocks forwarded %d images from an over-cap payload, want 0", len(images))
	}
	if decodes != 0 {
		t.Fatalf("decoder ran %d times, want 0: the pre-decode cap must reject before any decode work", decodes)
	}
	if got := DroppedContentSummary(content); got != "1 image/png block" {
		t.Fatalf("DroppedContentSummary() = %q, want the over-cap payload named as dropped", got)
	}
}

// The media type is sniffed from the decoded bytes, never taken from the
// block's declared mimeType: a hostile server must not be able to relabel
// HTML or a script as image/png and have it forwarded to the provider as an
// image. Every other fixture pairs real PNG bytes with image/png, so
// sniffing and trusting the declared type agree there and no test can tell
// them apart; this fixture makes them disagree.
func TestDeclaredMimeTypeDisagreeingWithSniffedBytesIsDropped(t *testing.T) {
	html := []byte("<!doctype html><html><body><script>alert(1)</script></body></html>")
	tool := registryTool{
		client: &nonTextClient{content: []Content{
			{Type: "image", MimeType: "image/png", Data: base64.StdEncoding.EncodeToString(html)},
		}},
		server: Server{Name: "shots"},
		remote: RemoteTool{Name: "screenshot"},
	}

	result := tool.Run(context.Background(), map[string]any{})

	if len(result.Images) != 0 {
		t.Fatalf("forwarded %d images from an HTML payload declared as image/png, want 0", len(result.Images))
	}
	if strings.Contains(result.Output, "[image returned by tool]") {
		t.Errorf("a non-image payload declared as image/png is still announced as a returned image:\n%s", result.Output)
	}
	if !strings.Contains(result.Output, "image/png") {
		t.Errorf("the dropped block is not named in the drop summary:\n%s", result.Output)
	}
}
