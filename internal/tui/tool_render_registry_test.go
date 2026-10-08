package tui

import (
	"strings"
	"testing"
)

func TestDefaultToolBodyRegistrySelectsCoreRenderers(t *testing.T) {
	registry := newDefaultToolBodyRegistry()
	opts := cardRenderOptions{bodyCap: cardBodyMaxLines}

	tests := []struct {
		name   string
		hint   string
		arg    string
		detail string
		want   []string
	}{
		{
			name: "edit_file",
			detail: strings.Join([]string{
				"--- a/app.go",
				"+++ b/app.go",
				"@@ -1 +1 @@",
				"-old",
				"+new",
			}, "\n"),
			want: []string{"(+1 -1)", "-1", "+1", "new"},
		},
		{
			name: "apply_patch",
			detail: strings.Join([]string{
				"--- a/app.go",
				"+++ b/app.go",
				"@@ -1 +1 @@",
				"-old",
				"+new",
			}, "\n"),
			want: []string{"(+1 -1)", "-1", "+1", "new"},
		},
		{
			name:   "read_file",
			hint:   "README.md",
			detail: "File: README.md\n\n7→# Zero",
			want:   []string{"Read", "README.md"},
		},
		{
			name:   "bash",
			hint:   "go test ./internal/tui",
			detail: "stdout:\nok\nexit_code: 0",
			want:   []string{"ok"},
		},
		{
			name:   "grep",
			arg:    "func render",
			detail: "internal/tui/rendering.go:41: func render()",
			want:   []string{"Search", "func render"},
		},
		{
			name:   "unknown_tool",
			detail: "raw output",
			want:   []string{"raw output"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := registry.render(toolBodyRequest{
				name:   tt.name,
				hint:   tt.hint,
				arg:    tt.arg,
				detail: normalizeToolCardDetail(tt.detail),
				width:  96,
				opts:   opts,
			})
			got := plainRender(t, strings.Join(append(append([]string{}, body.lines...), body.headTag, body.footer), "\n"))
			for _, want := range tt.want {
				if !strings.Contains(got, want) {
					t.Fatalf("%s body = %q, missing %q", tt.name, got, want)
				}
			}
			if tt.name == "read_file" && strings.Contains(got, "# Zero") {
				t.Fatalf("read_file body = %q, must not expose read contents", got)
			}
			if tt.name == "grep" && strings.Contains(got, "internal/tui/rendering.go:41") {
				t.Fatalf("grep body = %q, must not expose raw search matches", got)
			}
		})
	}
}

func TestToolBodyRegistryReplacementIsScopedToOneTool(t *testing.T) {
	registry := newDefaultToolBodyRegistry()
	registry.register("grep", toolBodyRendererFunc(func(req toolBodyRequest) cardBody {
		return cardBody{lines: []string{zeroTheme.onPanel(zeroTheme.ink).Render("replacement grep body")}}
	}))

	opts := cardRenderOptions{bodyCap: cardBodyMaxLines}
	grepBody := registry.render(toolBodyRequest{
		name:   "grep",
		detail: "internal/tui/rendering.go:41: func render()",
		width:  96,
		opts:   opts,
	})
	if got := plainRender(t, strings.Join(grepBody.lines, "\n")); !strings.Contains(got, "replacement grep body") {
		t.Fatalf("grep replacement body = %q, want replacement", got)
	}

	bashBody := registry.render(toolBodyRequest{
		name:   "bash",
		hint:   "go test ./internal/tui",
		detail: normalizeToolCardDetail("stdout:\nok\nexit_code: 0"),
		width:  96,
		opts:   opts,
	})
	got := plainRender(t, strings.Join(append(append([]string{}, bashBody.lines...), bashBody.footer), "\n"))
	if strings.Contains(got, "replacement grep body") {
		t.Fatalf("bash body = %q, must not use grep replacement", got)
	}
	if !strings.Contains(got, "ok") || strings.Contains(got, "exit 0") {
		t.Fatalf("bash body = %q, want original bash renderer", got)
	}
}

func TestToolBodyRegistryTrimsRegisteredNames(t *testing.T) {
	registry := newToolBodyRegistry(unknownToolBodyRenderer{})
	registry.register(" grep ", toolBodyRendererFunc(func(req toolBodyRequest) cardBody {
		return cardBody{lines: []string{zeroTheme.onPanel(zeroTheme.ink).Render("trimmed grep body")}}
	}))

	body := registry.render(toolBodyRequest{
		name:   "grep",
		detail: "internal/tui/rendering.go:41: func render()",
		width:  96,
		opts:   cardRenderOptions{bodyCap: cardBodyMaxLines},
	})

	if got := plainRender(t, strings.Join(body.lines, "\n")); !strings.Contains(got, "trimmed grep body") {
		t.Fatalf("grep body = %q, want trimmed registered renderer", got)
	}
}
