// Package repomap scans a workspace into a compact, deterministic repository map.
package repomap

import (
	"sort"
	"strings"

	"github.com/Twigpine/zero/internal/workspaceindex"
)

const (
	DefaultMaxFiles = workspaceindex.DefaultMaxFiles
	DefaultMaxDepth = workspaceindex.DefaultMaxDepth
)

type Options struct {
	MaxFiles int
	// MaxDepth is measured as path separators below the root. Zero means root
	// files only; negative values use DefaultMaxDepth.
	MaxDepth            int
	MaxBytesPerFileName int
}

type Snapshot struct {
	Root            string   `json:"root"`
	Files           []File   `json:"files"`
	FileCount       int      `json:"fileCount"`
	DirectoryCount  int      `json:"directoryCount"`
	MaxDepth        int      `json:"maxDepth"`
	LanguageCounts  []Count  `json:"languages"`
	ExtensionCounts []Count  `json:"extensions"`
	ImportantFiles  []string `json:"importantFiles"`
	Tree            []string `json:"tree"`
	Truncated       bool     `json:"truncated,omitempty"`
}

// RepoMap is kept as a semantic alias for call sites/tests that use the product
// term instead of the storage term.
type RepoMap = Snapshot

type File struct {
	Path      string `json:"path"`
	Language  string `json:"language,omitempty"`
	Extension string `json:"extension,omitempty"`
	Depth     int    `json:"depth,omitempty"`
}

type Count struct {
	Name  string `json:"name"`
	Count int    `json:"fileCount"`
}

func Scan(root string, options Options) (Snapshot, error) {
	summary, err := workspaceindex.Scan(root, workspaceindex.Options{
		MaxFiles:            options.MaxFiles,
		MaxDepth:            options.MaxDepth,
		MaxBytesPerFileName: options.MaxBytesPerFileName,
	})
	files := make([]File, 0, len(summary.Files))
	for _, file := range summary.Files {
		files = append(files, File{
			Path:      file.Path,
			Language:  file.Language,
			Extension: file.Ext,
			Depth:     file.Depth,
		})
	}
	sortFiles(files)
	snapshot := Snapshot{
		Root:           summary.Root,
		Files:          files,
		FileCount:      summary.TotalFiles,
		DirectoryCount: summary.DirectoryCount,
		MaxDepth:       summary.MaxDepth,
		Truncated:      summary.Truncated,
	}
	snapshot.LanguageCounts = sortedCounts(summary.LanguageCounts)
	snapshot.ExtensionCounts = sortedCounts(summary.ExtensionCounts)
	snapshot.ImportantFiles = importantFilePaths(files)
	snapshot.Tree = buildTree(files)
	return snapshot, err
}

func fileDepth(rel string) int {
	return workspaceindex.FileDepth(rel)
}

func languageForExt(ext string) string {
	return workspaceindex.LanguageForPath("file" + ext)
}

func countLanguages(files []File) []Count {
	counts := map[string]int{}
	for _, file := range files {
		if file.Language != "" {
			counts[file.Language]++
		}
	}
	return sortedCounts(counts)
}

func countExtensions(files []File) []Count {
	counts := map[string]int{}
	for _, file := range files {
		if file.Extension != "" {
			counts[file.Extension]++
		}
	}
	return sortedCounts(counts)
}

func sortedCounts(counts map[string]int) []Count {
	result := make([]Count, 0, len(counts))
	for name, count := range counts {
		if name == "" || count <= 0 {
			continue
		}
		result = append(result, Count{Name: name, Count: count})
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Count != result[j].Count {
			return result[i].Count > result[j].Count
		}
		return result[i].Name < result[j].Name
	})
	return result
}

func importantFilePaths(files []File) []string {
	type important struct {
		path     string
		priority int
	}
	values := []important{}
	for _, file := range files {
		if priority, ok := importantPriority(file.Path); ok {
			values = append(values, important{path: file.Path, priority: priority})
		}
	}
	sort.Slice(values, func(i, j int) bool {
		if values[i].priority != values[j].priority {
			return values[i].priority < values[j].priority
		}
		return values[i].path < values[j].path
	})
	paths := make([]string, 0, len(values))
	for _, value := range values {
		paths = append(paths, value.path)
	}
	return paths
}

func importantPriority(file string) (int, bool) {
	return workspaceindex.ImportantPriority(file)
}

func buildTree(files []File) []string {
	root := newTreeNode()
	for _, file := range files {
		insertFile(root, file.Path)
	}
	lines := []string{"."}
	appendTreeLines(&lines, root, 0)
	return lines
}

type treeNode struct {
	dirs  map[string]*treeNode
	files map[string]struct{}
}

func newTreeNode() *treeNode {
	return &treeNode{dirs: map[string]*treeNode{}, files: map[string]struct{}{}}
}

func insertFile(root *treeNode, rel string) {
	parts := strings.Split(rel, "/")
	if len(parts) == 0 {
		return
	}
	node := root
	for _, part := range parts[:len(parts)-1] {
		if part == "" {
			continue
		}
		if node.dirs[part] == nil {
			node.dirs[part] = newTreeNode()
		}
		node = node.dirs[part]
	}
	name := parts[len(parts)-1]
	if name != "" {
		node.files[name] = struct{}{}
	}
}

func appendTreeLines(lines *[]string, node *treeNode, depth int) {
	for _, entry := range sortedTreeEntries(node) {
		prefix := strings.Repeat("  ", depth)
		if entry.dir {
			*lines = append(*lines, prefix+entry.name+"/")
			appendTreeLines(lines, node.dirs[entry.name], depth+1)
			continue
		}
		*lines = append(*lines, prefix+entry.name)
	}
}

type treeEntry struct {
	name string
	dir  bool
}

func sortedTreeEntries(node *treeNode) []treeEntry {
	entries := make([]treeEntry, 0, len(node.dirs)+len(node.files))
	for name := range node.dirs {
		entries = append(entries, treeEntry{name: name, dir: true})
	}
	for name := range node.files {
		entries = append(entries, treeEntry{name: name})
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].name != entries[j].name {
			return entries[i].name < entries[j].name
		}
		return entries[i].dir && !entries[j].dir
	})
	return entries
}

func sortFiles(files []File) {
	sort.Slice(files, func(i, j int) bool {
		return files[i].Path < files[j].Path
	})
}
