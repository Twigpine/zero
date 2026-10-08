package tui

import (
	"strings"
)

type toolBodyRequest struct {
	name   string
	hint   string
	arg    string
	detail string
	width  int
	opts   cardRenderOptions
	failed bool
}

type toolBodyRenderer interface {
	renderToolBody(toolBodyRequest) cardBody
}

type toolBodyRendererFunc func(toolBodyRequest) cardBody

func (fn toolBodyRendererFunc) renderToolBody(req toolBodyRequest) cardBody {
	return fn(req)
}

type toolBodyRegistry struct {
	renderers map[string]toolBodyRenderer
	fallback  toolBodyRenderer
}

var defaultToolBodyRegistry = newDefaultToolBodyRegistry()

func newDefaultToolBodyRegistry() *toolBodyRegistry {
	fallback := unknownToolBodyRenderer{}
	registry := newToolBodyRegistry(fallback)

	diffOrFallback := diffFirstToolBodyRenderer{next: fallback}
	registry.register("edit_file", diffOrFallback)
	registry.register("apply_patch", diffOrFallback)
	// write_file's card-only Display.Preview is a synthesized all-additions diff
	// (the new file's head), so render it through the same diff path.
	registry.register("write_file", diffOrFallback)
	exploreRenderer := diffFirstToolBodyRenderer{next: toolBodyRendererFunc(func(req toolBodyRequest) cardBody {
		return exploreCardBody(req.name, req.hint, req.arg, req.detail, req.width, req.opts, req.failed)
	})}
	registry.register("read_file", exploreRenderer)
	registry.register("read_minified_file", exploreRenderer)
	registry.register("list_directory", exploreRenderer)
	registry.register("glob", exploreRenderer)
	registry.register("bash", diffFirstToolBodyRenderer{next: toolBodyRendererFunc(func(req toolBodyRequest) cardBody {
		return bashCardBody(req.hint, req.detail, req.width, req.opts)
	})})
	registry.register("exec_command", diffFirstToolBodyRenderer{next: toolBodyRendererFunc(func(req toolBodyRequest) cardBody {
		return execCommandCardBody(req.hint, req.detail, req.width, req.opts)
	})})
	registry.register("write_stdin", diffFirstToolBodyRenderer{next: toolBodyRendererFunc(func(req toolBodyRequest) cardBody {
		return execCommandCardBody("", req.detail, req.width, req.opts)
	})})
	registry.register("grep", exploreRenderer)
	localControlRenderer := diffFirstToolBodyRenderer{next: toolBodyRendererFunc(func(req toolBodyRequest) cardBody {
		return localControlCardBody(req.name, req.hint, req.detail, req.width, req.opts, req.failed)
	})}
	for _, name := range []string{
		"browser_install", "browser_launch", "browser_connect", "browser_open", "browser_snapshot",
		"browser_click", "browser_type", "browser_press", "browser_action",
		"desktop_windows", "desktop_snapshot", "desktop_action",
		"terminal_session", "capture_artifact",
	} {
		registry.register(name, localControlRenderer)
	}
	return registry
}

func newToolBodyRegistry(fallback toolBodyRenderer) *toolBodyRegistry {
	if fallback == nil {
		fallback = unknownToolBodyRenderer{}
	}
	return &toolBodyRegistry{
		renderers: map[string]toolBodyRenderer{},
		fallback:  fallback,
	}
}

func (registry *toolBodyRegistry) register(name string, renderer toolBodyRenderer) {
	name = strings.TrimSpace(name)
	if registry == nil || name == "" || renderer == nil {
		return
	}
	if registry.renderers == nil {
		registry.renderers = map[string]toolBodyRenderer{}
	}
	registry.renderers[name] = renderer
}

func (registry *toolBodyRegistry) render(req toolBodyRequest) cardBody {
	req.detail = normalizeToolCardDetail(req.detail)
	if strings.TrimSpace(req.detail) == "" {
		return cardBody{}
	}
	return registry.rendererFor(req.name).renderToolBody(req)
}

func (registry *toolBodyRegistry) rendererFor(name string) toolBodyRenderer {
	if registry != nil {
		if renderer, ok := registry.renderers[name]; ok && renderer != nil {
			return renderer
		}
		if registry.fallback != nil {
			return registry.fallback
		}
	}
	return unknownToolBodyRenderer{}
}

func normalizeToolCardDetail(detail string) string {
	detail = strings.TrimRight(strings.ReplaceAll(detail, "\r\n", "\n"), "\n")
	// Terminal tab stops are unknowable from here and break the width math
	// (lipgloss measures \t as one cell, the terminal expands it further), so
	// card bodies render tabs as a fixed indent.
	return strings.ReplaceAll(detail, "\t", "    ")
}

type diffFirstToolBodyRenderer struct {
	next toolBodyRenderer
}

func (renderer diffFirstToolBodyRenderer) renderToolBody(req toolBodyRequest) cardBody {
	if looksLikeDiff(req.detail) {
		return diffCardBody(req.detail, req.width, req.opts)
	}
	if renderer.next == nil {
		return unknownToolBodyRenderer{}.renderToolBody(req)
	}
	return renderer.next.renderToolBody(req)
}

type unknownToolBodyRenderer struct{}

func (unknownToolBodyRenderer) renderToolBody(req toolBodyRequest) cardBody {
	if looksLikeDiff(req.detail) {
		return diffCardBody(req.detail, req.width, req.opts)
	}
	return genericCardBody(req.detail, req.opts)
}
