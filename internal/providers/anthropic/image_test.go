package anthropic

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Gitlawb/zero/internal/zeroruntime"
)

// captureRequestBody runs one StreamCompletion against a stub server that
// records the decoded JSON request body, then returns it for assertions.
func captureRequestBody(t *testing.T, request zeroruntime.CompletionRequest) map[string]any {
	t.Helper()
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		writeSSEEvent(w, "message_stop", `{"type":"message_stop"}`)
	}))
	t.Cleanup(server.Close)

	provider, err := New(Options{APIKey: "sk-ant", BaseURL: server.URL, Model: "claude-test"})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	stream, err := provider.StreamCompletion(context.Background(), request)
	if err != nil {
		t.Fatalf("StreamCompletion returned error: %v", err)
	}
	drain(stream)
	if gotBody == nil {
		t.Fatal("server did not capture a request body")
	}
	return gotBody
}

// firstUserContentBlocks returns the content blocks of the first user message.
func firstUserContentBlocks(t *testing.T, body map[string]any) []any {
	t.Helper()
	messages, ok := body["messages"].([]any)
	if !ok || len(messages) == 0 {
		t.Fatalf("messages = %#v, want at least one", body["messages"])
	}
	user, ok := messages[0].(map[string]any)
	if !ok || user["role"] != "user" {
		t.Fatalf("first message = %#v, want a user message", messages[0])
	}
	blocks, ok := user["content"].([]any)
	if !ok {
		t.Fatalf("user content = %#v, want a blocks array", user["content"])
	}
	return blocks
}

// TestUserTextOnlyTurnUnchanged pins the text-only wire shape: a single
// user message whose content is a one-element text-block array.
func TestUserTextOnlyTurnUnchanged(t *testing.T) {
	t.Parallel()
	body := captureRequestBody(t, zeroruntime.CompletionRequest{
		Messages: []zeroruntime.Message{
			{Role: zeroruntime.MessageRoleUser, Content: "Describe this."},
		},
	})
	blocks := firstUserContentBlocks(t, body)
	if len(blocks) != 1 {
		t.Fatalf("text-only blocks = %#v, want exactly one", blocks)
	}
	text := blocks[0].(map[string]any)
	if text["type"] != "text" || text["text"] != "Describe this." {
		t.Fatalf("text-only block = %#v, want a text block", text)
	}
}

// TestUserImagePlusTextTurn asserts a text block followed by one image
// source block carrying base64 of the RAW bytes.
func TestUserImagePlusTextTurn(t *testing.T) {
	t.Parallel()
	raw := []byte{0x89, 0x50, 0x4e, 0x47, 0x01, 0x02}
	body := captureRequestBody(t, zeroruntime.CompletionRequest{
		Messages: []zeroruntime.Message{
			{
				Role:    zeroruntime.MessageRoleUser,
				Content: "Describe this.",
				Images:  []zeroruntime.ImageBlock{{MediaType: "image/png", Data: raw}},
			},
		},
	})
	blocks := firstUserContentBlocks(t, body)
	if len(blocks) != 2 {
		t.Fatalf("image+text blocks = %#v, want text then image", blocks)
	}
	text := blocks[0].(map[string]any)
	if text["type"] != "text" || text["text"] != "Describe this." {
		t.Fatalf("first block = %#v, want a text block", text)
	}
	image := blocks[1].(map[string]any)
	if image["type"] != "image" {
		t.Fatalf("second block = %#v, want an image block", image)
	}
	source := image["source"].(map[string]any)
	if source["type"] != "base64" || source["media_type"] != "image/png" {
		t.Fatalf("image source = %#v, want base64 png", source)
	}
	if source["data"] != base64.StdEncoding.EncodeToString(raw) {
		t.Fatalf("image data = %v, want base64 of raw bytes", source["data"])
	}
}

// TestUserImageOnlyTurnEmits asserts an image-only user turn (empty Content)
// still produces a user message with a single image block.
func TestUserImageOnlyTurnEmits(t *testing.T) {
	t.Parallel()
	raw := []byte{0xff, 0xd8, 0xff, 0xe0}
	body := captureRequestBody(t, zeroruntime.CompletionRequest{
		Messages: []zeroruntime.Message{
			{
				Role:   zeroruntime.MessageRoleUser,
				Images: []zeroruntime.ImageBlock{{MediaType: "image/jpeg", Data: raw}},
			},
		},
	})
	messages := body["messages"].([]any)
	if len(messages) != 1 {
		t.Fatalf("messages = %#v, want exactly one user turn for image-only", messages)
	}
	blocks := firstUserContentBlocks(t, body)
	if len(blocks) != 1 {
		t.Fatalf("image-only blocks = %#v, want a single image block", blocks)
	}
	image := blocks[0].(map[string]any)
	if image["type"] != "image" {
		t.Fatalf("image-only block = %#v, want an image block", image)
	}
	source := image["source"].(map[string]any)
	if source["media_type"] != "image/jpeg" || source["data"] != base64.StdEncoding.EncodeToString(raw) {
		t.Fatalf("image-only source = %#v, want base64 jpeg of raw bytes", source)
	}
}
