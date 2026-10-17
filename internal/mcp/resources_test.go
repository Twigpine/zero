package mcp

import (
	"bytes"
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/tools"
)

func TestServeInitializeAdvertisesResourcesAndPrompts(t *testing.T) {
	registry := tools.NewRegistry()

	var input bytes.Buffer
	writeServerTestMessage(t, &input, rpcMessage{ID: 1, Method: "initialize"})

	var output bytes.Buffer
	if err := Serve(context.Background(), &input, &output, registry, ServeOptions{}); err != nil {
		t.Fatalf("Serve() error = %v", err)
	}

	var result struct {
		Capabilities map[string]any `json:"capabilities"`
	}
	decodeServerTestResult(t, readServerTestMessage(t, newMessageReader(&output)), &result)
	for _, capability := range []string{"tools", "resources", "prompts"} {
		if _, ok := result.Capabilities[capability]; !ok {
			t.Fatalf("initialize capabilities = %#v, want %s", result.Capabilities, capability)
		}
	}
}

func TestServeResourcesListReturnsInScopeFiles(t *testing.T) {
	workspace := t.TempDir()
	writeResourceFile(t, workspace, "main.go", "package main\n")
	writeResourceFile(t, workspace, "docs/readme.md", "# Title\n")
	// A binary file is still listed (read decides how to encode it).
	writeResourceFile(t, workspace, "logo.png", "\x89PNG\r\n\x1a\n")

	var input bytes.Buffer
	writeServerTestMessage(t, &input, rpcMessage{ID: 1, Method: "resources/list"})

	var output bytes.Buffer
	if err := Serve(context.Background(), &input, &output, tools.NewRegistry(), ServeOptions{WorkspaceRoot: workspace}); err != nil {
		t.Fatalf("Serve() error = %v", err)
	}

	var result struct {
		Resources []Resource `json:"resources"`
	}
	decodeServerTestResult(t, readServerTestMessage(t, newMessageReader(&output)), &result)

	names := map[string]Resource{}
	for _, resource := range result.Resources {
		names[resource.Name] = resource
	}
	if _, ok := names["main.go"]; !ok {
		t.Fatalf("resources = %#v, want main.go", result.Resources)
	}
	if _, ok := names["docs/readme.md"]; !ok {
		t.Fatalf("resources = %#v, want docs/readme.md", result.Resources)
	}
	main := names["main.go"]
	if !strings.HasPrefix(main.URI, "file://") {
		t.Fatalf("resource URI = %q, want file:// scheme", main.URI)
	}
	if main.MimeType == "" {
		t.Fatalf("resource mimeType empty for %#v", main)
	}
}

func TestServeResourcesListExcludesOutOfScope(t *testing.T) {
	workspace := t.TempDir()
	writeResourceFile(t, workspace, "inside.txt", "in\n")

	// A sibling directory outside the workspace root must never be enumerated.
	outside := t.TempDir()
	writeResourceFile(t, outside, "secret.txt", "leak\n")

	var input bytes.Buffer
	writeServerTestMessage(t, &input, rpcMessage{ID: 1, Method: "resources/list"})

	var output bytes.Buffer
	if err := Serve(context.Background(), &input, &output, tools.NewRegistry(), ServeOptions{WorkspaceRoot: workspace}); err != nil {
		t.Fatalf("Serve() error = %v", err)
	}

	var result struct {
		Resources []Resource `json:"resources"`
	}
	decodeServerTestResult(t, readServerTestMessage(t, newMessageReader(&output)), &result)
	outsideRoot, err := filepath.EvalSymlinks(outside)
	if err != nil {
		outsideRoot = filepath.Clean(outside)
	}
	for _, resource := range result.Resources {
		resourcePath, err := pathFromURI(resource.URI)
		if err != nil {
			t.Fatalf("resource URI %q did not decode: %v", resource.URI, err)
		}
		resourcePath, err = filepath.EvalSymlinks(resourcePath)
		if err != nil {
			resourcePath = filepath.Clean(resourcePath)
		}
		if strings.Contains(resource.URI, "secret.txt") || containedInRoot(outsideRoot, resourcePath) {
			t.Fatalf("resource %#v leaks out-of-scope path", resource)
		}
	}
}

func TestServeResourcesReadReturnsText(t *testing.T) {
	workspace := t.TempDir()
	writeResourceFile(t, workspace, "hello.txt", "hello world\n")

	uri := fileURI(filepath.Join(workspace, "hello.txt"))
	contents := readResource(t, workspace, uri)
	if len(contents) != 1 {
		t.Fatalf("contents = %#v, want one entry", contents)
	}
	if contents[0].Text != "hello world\n" {
		t.Fatalf("text = %q, want hello world", contents[0].Text)
	}
	if contents[0].Blob != "" {
		t.Fatalf("blob = %q, want empty for text file", contents[0].Blob)
	}
	if contents[0].URI != uri {
		t.Fatalf("uri = %q, want %q", contents[0].URI, uri)
	}
}

func TestServeResourcesReadBinaryReturnsBlob(t *testing.T) {
	workspace := t.TempDir()
	raw := "\x89PNG\r\n\x1a\n\x00\x01\x02"
	writeResourceFile(t, workspace, "logo.png", raw)

	uri := fileURI(filepath.Join(workspace, "logo.png"))
	contents := readResource(t, workspace, uri)
	if len(contents) != 1 {
		t.Fatalf("contents = %#v, want one entry", contents)
	}
	if contents[0].Text != "" {
		t.Fatalf("text = %q, want empty for binary file", contents[0].Text)
	}
	decoded, err := base64.StdEncoding.DecodeString(contents[0].Blob)
	if err != nil {
		t.Fatalf("blob decode: %v", err)
	}
	if string(decoded) != raw {
		t.Fatalf("decoded blob = %q, want %q", decoded, raw)
	}
}

func TestServeResourcesReadRejectsTraversal(t *testing.T) {
	workspace := t.TempDir()
	writeResourceFile(t, workspace, "inside.txt", "in\n")

	traversal := "file://" + filepath.ToSlash(filepath.Join(workspace, "..", "..", "etc", "passwd"))
	message := readResourceMessage(t, workspace, traversal)
	if message.Error == nil {
		t.Fatalf("expected error for traversal, got result %s", string(message.Result))
	}
	if len(message.Result) != 0 {
		t.Fatalf("traversal returned contents: %s", string(message.Result))
	}
}

func TestServeResourcesReadRejectsInRootSymlinkEscape(t *testing.T) {
	workspace := t.TempDir()
	writeResourceFile(t, workspace, "inside.txt", "in\n")
	// A secret outside the workspace, reachable only via an in-workspace symlink.
	// Resolving symlinks before the scope check must keep it unreadable.
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("top secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(workspace, "link.txt")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unavailable on this platform: %v", err)
	}
	message := readResourceMessage(t, workspace, fileURI(link))
	if message.Error == nil {
		t.Fatalf("expected error for an in-root symlink escaping the workspace, got result %s", string(message.Result))
	}
	if len(message.Result) != 0 {
		t.Fatalf("symlink escape returned contents: %s", string(message.Result))
	}
}

func TestServeResourcesReadRejectsOutOfScopeAbsolute(t *testing.T) {
	workspace := t.TempDir()
	outside := t.TempDir()
	writeResourceFile(t, outside, "secret.txt", "leak\n")

	uri := fileURI(filepath.Join(outside, "secret.txt"))
	message := readResourceMessage(t, workspace, uri)
	if message.Error == nil {
		t.Fatalf("expected error for out-of-scope absolute path, got result %s", string(message.Result))
	}
	if len(message.Result) != 0 {
		t.Fatalf("out-of-scope read returned contents: %s", string(message.Result))
	}
	if strings.Contains(message.Error.Message, "leak") {
		t.Fatalf("error message leaked file contents: %q", message.Error.Message)
	}
}

func TestServeResourcesReadMissingFileErrors(t *testing.T) {
	workspace := t.TempDir()

	uri := fileURI(filepath.Join(workspace, "nope.txt"))
	message := readResourceMessage(t, workspace, uri)
	if message.Error == nil {
		t.Fatalf("expected error for missing file, got result %s", string(message.Result))
	}
}

func readResource(t *testing.T, workspace string, uri string) []ResourceContents {
	t.Helper()
	message := readResourceMessage(t, workspace, uri)
	if message.Error != nil {
		t.Fatalf("resources/read error = %#v", message.Error)
	}
	var result struct {
		Contents []ResourceContents `json:"contents"`
	}
	decodeServerTestResult(t, message, &result)
	return result.Contents
}

func readResourceMessage(t *testing.T, workspace string, uri string) rpcMessage {
	t.Helper()
	var input bytes.Buffer
	writeServerTestMessage(t, &input, rpcMessage{
		ID:     7,
		Method: "resources/read",
		Params: mustRaw(map[string]any{"uri": uri}),
	})
	var output bytes.Buffer
	if err := Serve(context.Background(), &input, &output, tools.NewRegistry(), ServeOptions{WorkspaceRoot: workspace}); err != nil {
		t.Fatalf("Serve() error = %v", err)
	}
	return readServerTestMessage(t, newMessageReader(&output))
}

func writeResourceFile(t *testing.T, root string, rel string, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
