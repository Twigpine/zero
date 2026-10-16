// Package workspaceseed builds a compact, deterministic workspace context seed
// for future agent prompt/session wiring.
package workspaceseed

import (
	"fmt"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Gitlawb/zero/internal/workspaceindex"
)

const (
	defaultMaxLayoutEntries = 16
	defaultMaxRenderLines   = 12
	defaultRenderWidth      = 100
)

var detectedProjectFileOrder = []string{
	"README.md",
	"go.mod",
	"package.json",
	"AGENTS.md",
	"docs/INSTALL.md",
	"docs/STREAM_JSON_PROTOCOL.md",
}

// Input is the pure builder input. Callers provide paths and git metadata; the
// builder never shells out.
type Input struct {
	CWD              string
	GitBranch        string
	GitDirty         *bool
	GitSummary       string
	Paths            []string
	MaxLayoutEntries int
}

// GitInfo is metadata supplied by integration code that already knows git
// state. BuildFromWorkspace does not invoke git.
type GitInfo struct {
	Branch  string
	Dirty   *bool
	Summary string
}

// Seed is the compact workspace context model ready for rendering.
type Seed struct {
	CWD          string
	GitBranch    string
	GitSummary   string
	Layout       []string
	ProjectFiles []string
	MemoryFiles  []string
	Truncated    bool
}

// RenderOptions controls text output budgets.
type RenderOptions struct {
	MaxLines int
	Width    int
}

// Build creates a deterministic seed from simple, injectable inputs.
func Build(input Input) Seed {
	cwd := strings.TrimSpace(input.CWD)
	if cwd != "" {
		cwd = filepath.Clean(cwd)
	}
	paths := normalizePaths(cwd, input.Paths)
	layout, truncated := topLevelLayout(paths, input.MaxLayoutEntries)
	return Seed{
		CWD:          cwd,
		GitBranch:    strings.TrimSpace(input.GitBranch),
		GitSummary:   gitSummary(input.GitDirty, input.GitSummary),
		Layout:       layout,
		ProjectFiles: detectedProjectFiles(paths),
		MemoryFiles:  memoryFiles(paths),
		Truncated:    truncated,
	}
}

// BuildFromWorkspace scans the local filesystem with workspaceindex and builds
// a seed from the resulting file list. Its only git operation is the read-only
// ignored-path lookup inside workspaceindex.Scan; branch and dirty state come
// from the caller's GitInfo.
func BuildFromWorkspace(root string, git GitInfo) (Seed, error) {
	summary, err := workspaceindex.Scan(root, workspaceindex.Options{
		MaxFiles: workspaceindex.DefaultMaxFiles,
		MaxDepth: workspaceindex.DefaultMaxDepth,
	})
	if err != nil {
		return Seed{}, err
	}
	paths := make([]string, 0, len(summary.Files))
	for _, file := range summary.Files {
		paths = append(paths, file.Path)
	}
	return Build(Input{
		CWD:        summary.Root,
		GitBranch:  git.Branch,
		GitDirty:   git.Dirty,
		GitSummary: git.Summary,
		Paths:      paths,
	}), nil
}

// Render formats a seed for prompt/session context while respecting simple line
// and rune-width budgets.
func Render(seed Seed, options RenderOptions) string {
	maxLines := options.MaxLines
	if maxLines <= 0 {
		maxLines = defaultMaxRenderLines
	}
	width := options.Width
	if width <= 0 {
		width = defaultRenderWidth
	}

	lines := []string{"Workspace context seed"}
	lines = append(lines, "cwd: "+safeCWD(seed.CWD))
	if seed.GitBranch != "" || seed.GitSummary != "" {
		lines = append(lines, "git: "+formatGit(seed))
	}
	lines = append(lines, "layout: "+formatList(seed.Layout))
	lines = append(lines, "project files: "+formatList(seed.ProjectFiles))
	lines = append(lines, "memory hints: "+formatList(seed.MemoryFiles))
	if seed.Truncated {
		lines = append(lines, "layout truncated: true")
	}

	clipped := false
	if len(lines) > maxLines {
		lines = append([]string{}, lines[:maxLines]...)
		lines[len(lines)-1] = "..."
		clipped = true
	}
	for i, line := range lines {
		next, didClip := clampLine(line, width)
		lines[i] = next
		clipped = clipped || didClip
	}
	if clipped && !containsEllipsis(lines) {
		lines[len(lines)-1] = clampLineNoFlag("...", width)
	}
	return strings.Join(lines, "\n")
}

func normalizePaths(cwd string, values []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		rel := normalizePath(cwd, value)
		if rel == "" {
			continue
		}
		if _, ok := seen[rel]; ok {
			continue
		}
		seen[rel] = struct{}{}
		out = append(out, rel)
	}
	sort.Strings(out)
	return out
}

func normalizePath(cwd string, value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	value = strings.ReplaceAll(value, "\\", "/")
	if filepath.IsAbs(value) {
		if cwd == "" {
			return ""
		}
		rel, err := filepath.Rel(filepath.Clean(cwd), filepath.Clean(value))
		if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return ""
		}
		value = rel
	}
	value = filepath.ToSlash(value)
	if isAbsoluteLikePath(value) {
		return ""
	}
	value = path.Clean(value)
	if isAbsoluteLikePath(value) {
		return ""
	}
	for strings.HasPrefix(value, "./") {
		value = strings.TrimPrefix(value, "./")
	}
	if value == "." || value == ".." || strings.HasPrefix(value, "../") {
		return ""
	}
	return value
}

func isAbsoluteLikePath(value string) bool {
	if strings.HasPrefix(value, "/") {
		return true
	}
	if len(value) >= 2 && value[1] == ':' {
		drive := value[0]
		return (drive >= 'A' && drive <= 'Z') || (drive >= 'a' && drive <= 'z')
	}
	return false
}

func topLevelLayout(paths []string, maxEntries int) ([]string, bool) {
	if maxEntries <= 0 {
		maxEntries = defaultMaxLayoutEntries
	}
	seen := map[string]struct{}{}
	for _, rel := range paths {
		first, _, hasSlash := strings.Cut(rel, "/")
		if first == "" {
			continue
		}
		entry := first
		if hasSlash {
			entry += "/"
		}
		seen[entry] = struct{}{}
	}
	layout := make([]string, 0, len(seen))
	for entry := range seen {
		layout = append(layout, entry)
	}
	sort.Strings(layout)
	if len(layout) > maxEntries {
		return append([]string{}, layout[:maxEntries]...), true
	}
	return layout, false
}

func detectedProjectFiles(paths []string) []string {
	present := presentSet(paths)
	out := []string{}
	for _, candidate := range detectedProjectFileOrder {
		if present[candidate] {
			out = append(out, candidate)
		}
	}
	return out
}

func memoryFiles(paths []string) []string {
	out := []string{}
	for _, candidate := range []string{"AGENTS.md", "ZERO.md"} {
		for _, rel := range paths {
			if rel == candidate {
				out = append(out, candidate)
				break
			}
		}
	}
	return out
}

func presentSet(paths []string) map[string]bool {
	present := map[string]bool{}
	for _, rel := range paths {
		present[rel] = true
	}
	return present
}

func gitSummary(dirty *bool, summary string) string {
	summary = strings.TrimSpace(summary)
	if dirty == nil {
		return summary
	}
	if !*dirty {
		if summary == "" {
			return "clean"
		}
		return summary
	}
	if summary == "" {
		return "dirty"
	}
	if strings.HasPrefix(strings.ToLower(summary), "dirty") {
		return summary
	}
	return "dirty: " + summary
}

func safeCWD(cwd string) string {
	cwd = strings.TrimSpace(cwd)
	if cwd == "" {
		return "."
	}
	base := filepath.Base(filepath.Clean(cwd))
	if base == "" || base == "." || base == string(filepath.Separator) {
		return "."
	}
	return base
}

func formatGit(seed Seed) string {
	switch {
	case seed.GitBranch != "" && seed.GitSummary != "":
		return fmt.Sprintf("%s (%s)", seed.GitBranch, seed.GitSummary)
	case seed.GitBranch != "":
		return seed.GitBranch
	default:
		return seed.GitSummary
	}
}

func formatList(values []string) string {
	if len(values) == 0 {
		return "none"
	}
	return strings.Join(values, ", ")
}

func clampLine(line string, width int) (string, bool) {
	if width <= 0 || len([]rune(line)) <= width {
		return line, false
	}
	return clampLineNoFlag(line, width), true
}

func clampLineNoFlag(line string, width int) string {
	if width <= 0 {
		return ""
	}
	runes := []rune(line)
	if len(runes) <= width {
		return line
	}
	if width <= 3 {
		return string(runes[:width])
	}
	return strings.TrimRight(string(runes[:width-3]), " ") + "..."
}

func containsEllipsis(lines []string) bool {
	for _, line := range lines {
		if strings.Contains(line, "...") {
			return true
		}
	}
	return false
}
