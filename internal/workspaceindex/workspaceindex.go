// Package workspaceindex provides shared deterministic workspace scanning
// primitives for repo intelligence, tools, and context accounting.
package workspaceindex

import (
	"errors"
	"io/fs"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

const (
	DefaultMaxFiles = 2000
	DefaultMaxDepth = 6
)

var errStopWalk = errors.New("workspace index scan stopped")

type Options struct {
	MaxFiles int
	// MaxDepth is measured as path separators below the root. Zero means root
	// files only; negative values use DefaultMaxDepth.
	MaxDepth            int
	MaxBytesPerFileName int
}

type Summary struct {
	Root            string
	Files           []File
	LanguageCounts  map[string]int
	ExtensionCounts map[string]int
	TotalFiles      int
	DirectoryCount  int
	MaxDepth        int
	Truncated       bool
}

type File struct {
	Path      string
	Name      string
	Ext       string
	Language  string
	Size      int64
	Depth     int
	Important bool
}

func Scan(root string, options Options) (Summary, error) {
	cleanRoot, err := filepath.Abs(strings.TrimSpace(root))
	if err != nil {
		return Summary{}, err
	}
	cleanRoot = filepath.Clean(cleanRoot)

	maxFiles := options.MaxFiles
	if maxFiles <= 0 {
		maxFiles = DefaultMaxFiles
	}
	maxDepth := options.MaxDepth
	if maxDepth < 0 {
		maxDepth = DefaultMaxDepth
	}

	// Inside a git work tree, git's own ignore rules decide what else to skip; outside one
	// (or when git is unavailable or too old), a fixed list of build/cache directory names
	// stands in. Both apply to Scan only: ShouldSkipDir is shared with glob, grep,
	// list_directory and path autocomplete, which must keep seeing these directories.
	ignored, hasIgnored := gitIgnoredPaths(cleanRoot)
	skipScanDir := func(rel, name string) bool {
		if hasIgnored {
			return ignored[rel]
		}
		return isScanOnlySkipDir(name)
	}

	files := []File{}
	dirs := map[string]struct{}{}
	maxDepthSeen := 0
	truncated := false
	walkErr := filepath.WalkDir(cleanRoot, func(current string, entry fs.DirEntry, walkErr error) error {
		if handled, err := HandleWalkError(cleanRoot, current, entry, walkErr, &truncated); handled {
			return err
		}
		if current == cleanRoot {
			return nil
		}

		rel, relErr := filepath.Rel(cleanRoot, current)
		if relErr != nil {
			truncated = true
			return relErr
		}
		rel = filepath.ToSlash(rel)

		if entry.IsDir() {
			if ShouldSkipDir(entry.Name()) || isSymlink(entry) || skipScanDir(rel, entry.Name()) {
				return filepath.SkipDir
			}
			depth := pathDepth(rel)
			if depth > maxDepth {
				truncated = true
				return filepath.SkipDir
			}
			if depth > maxDepthSeen {
				maxDepthSeen = depth
			}
			dirs[rel] = struct{}{}
			return nil
		}

		if isSymlink(entry) || ShouldSkipFile(rel) || (hasIgnored && ignored[rel]) {
			return nil
		}
		depth := FileDepth(rel)
		if depth > maxDepth {
			truncated = true
			return nil
		}
		if depth > maxDepthSeen {
			maxDepthSeen = depth
		}
		if options.MaxBytesPerFileName > 0 && len(rel) > options.MaxBytesPerFileName {
			truncated = true
			return nil
		}
		if len(files) >= maxFiles {
			truncated = true
			return errStopWalk
		}

		size := int64(0)
		if info, err := entry.Info(); err == nil {
			size = info.Size()
		}
		ext := strings.ToLower(path.Ext(rel))
		files = append(files, File{
			Path:      rel,
			Name:      path.Base(rel),
			Ext:       ext,
			Language:  LanguageForPath(rel),
			Size:      size,
			Depth:     depth,
			Important: IsImportantPath(rel),
		})
		return nil
	})

	SortFiles(files)
	summary := Summary{
		Root:            cleanRoot,
		Files:           files,
		LanguageCounts:  languageCounts(files),
		ExtensionCounts: extensionCounts(files),
		TotalFiles:      len(files),
		DirectoryCount:  len(dirs),
		MaxDepth:        maxDepthSeen,
		Truncated:       truncated,
	}
	if walkErr != nil && !errors.Is(walkErr, errStopWalk) {
		return summary, walkErr
	}
	return summary, nil
}

func HandleWalkError(cleanRoot string, current string, entry fs.DirEntry, walkErr error, truncated *bool) (bool, error) {
	if walkErr == nil {
		return false, nil
	}
	*truncated = true
	if current == cleanRoot {
		return true, walkErr
	}
	if entry != nil && entry.IsDir() {
		return true, filepath.SkipDir
	}
	return true, nil
}

func ShouldSkipDir(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case ".cache", ".git", ".next", ".worktrees", ".zero", "build", "coverage", "dist", "node_modules", "vendor":
		return true
	default:
		return false
	}
}

// isScanOnlySkipDir names build and cache directories Scan skips when it cannot ask git
// what is ignored. It is deliberately separate from ShouldSkipDir: a tracked package named
// "target" or a question about a dependency inside .venv must stay visible to the tools.
func isScanOnlySkipDir(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "target", "__pycache__", ".venv", "venv", ".pytest_cache", ".terraform", ".mypy_cache", ".ruff_cache":
		return true
	default:
		return false
	}
}

func ShouldSkipFile(rel string) bool {
	base := strings.ToLower(path.Base(filepath.ToSlash(rel)))
	switch base {
	case ".git", ".ds_store":
		return true
	default:
		return LooksBinaryPath(rel)
	}
}

func LooksBinaryPath(file string) bool {
	switch strings.ToLower(path.Ext(filepath.ToSlash(file))) {
	case ".a", ".avif", ".bmp", ".class", ".dll", ".dylib", ".exe", ".gif", ".gz", ".ico", ".jar", ".jpeg", ".jpg", ".o", ".pdf", ".png", ".so", ".tar", ".tgz", ".webp", ".zip":
		return true
	default:
		return false
	}
}

func LanguageForPath(file string) string {
	switch strings.TrimPrefix(strings.ToLower(path.Ext(filepath.ToSlash(file))), ".") {
	case "bash", "sh", "zsh":
		return "Shell"
	case "c", "h":
		return "C"
	case "cc", "cpp", "cxx", "hh", "hpp":
		return "C++"
	case "cs":
		return "C#"
	case "css":
		return "CSS"
	case "dart":
		return "Dart"
	case "ex", "exs":
		return "Elixir"
	case "go":
		return "Go"
	case "html", "htm":
		return "HTML"
	case "java":
		return "Java"
	case "js", "jsx", "mjs", "cjs":
		return "JavaScript"
	case "json":
		return "JSON"
	case "kt", "kts":
		return "Kotlin"
	case "lua":
		return "Lua"
	case "md", "markdown":
		return "Markdown"
	case "php":
		return "PHP"
	case "proto":
		return "Protobuf"
	case "py":
		return "Python"
	case "rb":
		return "Ruby"
	case "rs":
		return "Rust"
	case "sass", "scss":
		return "SCSS"
	case "sql":
		return "SQL"
	case "svelte":
		return "Svelte"
	case "swift":
		return "Swift"
	case "tf":
		return "Terraform"
	case "toml":
		return "TOML"
	case "ts", "tsx":
		return "TypeScript"
	case "vue":
		return "Vue"
	case "yaml", "yml":
		return "YAML"
	default:
		return ""
	}
}

func IsImportantPath(file string) bool {
	_, ok := ImportantPriority(file)
	return ok
}

func ImportantPriority(file string) (int, bool) {
	base := strings.ToLower(path.Base(filepath.ToSlash(file)))
	switch base {
	case "agents.md":
		return 10, true
	case "zero.md":
		return 20, true
	case "readme.md":
		return 30, true
	case "contributing.md":
		return 40, true
	case "go.mod":
		return 50, true
	case "go.sum":
		return 55, true
	case "package.json":
		return 60, true
	case "cargo.toml":
		return 70, true
	case "pyproject.toml":
		return 80, true
	case "requirements.txt":
		return 90, true
	case "makefile":
		return 100, true
	case "dockerfile", "docker-compose.yml", "docker-compose.yaml":
		return 110, true
	default:
		return 0, false
	}
}

func PathDepth(rel string) int {
	normalized := normalizeSeparators(rel)
	if normalized == "" || normalized == "." {
		return 0
	}
	return strings.Count(normalized, "/") + 1
}

func FileDepth(rel string) int {
	normalized := normalizeSeparators(rel)
	if normalized == "" || !strings.Contains(normalized, "/") {
		return 0
	}
	return strings.Count(normalized, "/")
}

func normalizeSeparators(rel string) string {
	return strings.ReplaceAll(filepath.ToSlash(rel), "\\", "/")
}

func SortFiles(files []File) {
	sort.Slice(files, func(i, j int) bool {
		return files[i].Path < files[j].Path
	})
}

func isSymlink(entry fs.DirEntry) bool {
	return entry.Type()&fs.ModeSymlink != 0
}

func pathDepth(rel string) int {
	return PathDepth(rel)
}

func languageCounts(files []File) map[string]int {
	counts := map[string]int{}
	for _, file := range files {
		if file.Language != "" {
			counts[file.Language]++
		}
	}
	return counts
}

func extensionCounts(files []File) map[string]int {
	counts := map[string]int{}
	for _, file := range files {
		if file.Ext != "" {
			counts[file.Ext]++
		}
	}
	return counts
}
