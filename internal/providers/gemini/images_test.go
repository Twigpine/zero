package gemini

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/Gitlawb/zero/internal/zeroruntime"
)

func TestGeminiPartTextOnlySerializationOmitsInlineData(t *testing.T) {
	t.Parallel()
	part := geminiPart{Text: "hello"}
	got, err := json.Marshal(part)
	if err != nil {
		t.Fatalf("Marshal returned error: %v", err)
	}
	if string(got) != `{"text":"hello"}` {
		t.Fatalf("text-only part = %s, want byte-identical {\"text\":\"hello\"}", got)
	}
}

func TestGeminiInlineDataSerialization(t *testing.T) {
	t.Parallel()
	part := geminiPart{InlineData: &geminiInlineData{MimeType: "image/png", Data: "QUJD"}}
	got, err := json.Marshal(part)
	if err != nil {
		t.Fatalf("Marshal returned error: %v", err)
	}
	if string(got) != `{"inlineData":{"mimeType":"image/png","data":"QUJD"}}` {
		t.Fatalf("inlineData part = %s, want mimeType+data", got)
	}
}

func TestMapMessagesTextOnlyUserUnchanged(t *testing.T) {
	t.Parallel()
	_, contents, err := mapMessages([]zeroruntime.Message{
		{Role: zeroruntime.MessageRoleUser, Content: "Read the file."},
	})
	if err != nil {
		t.Fatalf("mapMessages returned error: %v", err)
	}
	if len(contents) != 1 {
		t.Fatalf("contents = %#v, want one user content", contents)
	}
	parts := contents[0].Parts
	if len(parts) != 1 || parts[0].Text != "Read the file." || parts[0].InlineData != nil {
		t.Fatalf("parts = %#v, want single text part with no inlineData", parts)
	}
}

func TestMapMessagesImageAndTextUserTurn(t *testing.T) {
	t.Parallel()
	raw := []byte("ABC")
	_, contents, err := mapMessages([]zeroruntime.Message{
		{
			Role:    zeroruntime.MessageRoleUser,
			Content: "What is this?",
			Images:  []zeroruntime.ImageBlock{{MediaType: "image/png", Data: raw}},
		},
	})
	if err != nil {
		t.Fatalf("mapMessages returned error: %v", err)
	}
	if len(contents) != 1 || contents[0].Role != "user" {
		t.Fatalf("contents = %#v, want one user content", contents)
	}
	parts := contents[0].Parts
	if len(parts) != 2 {
		t.Fatalf("parts = %#v, want text part then inlineData part", parts)
	}
	if parts[0].Text != "What is this?" || parts[0].InlineData != nil {
		t.Fatalf("parts[0] = %#v, want leading text part", parts[0])
	}
	if parts[1].InlineData == nil {
		t.Fatalf("parts[1] = %#v, want inlineData part", parts[1])
	}
	if parts[1].InlineData.MimeType != "image/png" {
		t.Fatalf("mimeType = %q, want image/png", parts[1].InlineData.MimeType)
	}
	if parts[1].InlineData.Data != "QUJD" {
		t.Fatalf("data = %q, want base64 of raw bytes (QUJD)", parts[1].InlineData.Data)
	}
}

func TestMapMessagesImageOnlyUserTurn(t *testing.T) {
	t.Parallel()
	_, contents, err := mapMessages([]zeroruntime.Message{
		{
			Role:   zeroruntime.MessageRoleUser,
			Images: []zeroruntime.ImageBlock{{MediaType: "image/jpeg", Data: bytes.Repeat([]byte{0xFF}, 3)}},
		},
	})
	if err != nil {
		t.Fatalf("mapMessages returned error: %v", err)
	}
	if len(contents) != 1 || contents[0].Role != "user" {
		t.Fatalf("contents = %#v, want one user content for image-only turn", contents)
	}
	parts := contents[0].Parts
	if len(parts) != 1 {
		t.Fatalf("parts = %#v, want single inlineData part (no empty text part)", parts)
	}
	if parts[0].InlineData == nil || parts[0].InlineData.MimeType != "image/jpeg" {
		t.Fatalf("parts[0] = %#v, want image/jpeg inlineData", parts[0])
	}
	if parts[0].Text != "" {
		t.Fatalf("parts[0].Text = %q, want empty (no text part emitted)", parts[0].Text)
	}
	if parts[0].InlineData.Data != "////" {
		t.Fatalf("data = %q, want base64 of three 0xFF bytes (////)", parts[0].InlineData.Data)
	}
}
