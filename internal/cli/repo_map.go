package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/Twigpine/zero/internal/repomap"
)

const defaultRepoMapOutputFiles = 40

type repoMapOptions struct {
	json         bool
	query        string
	outputFiles  int
	scanMaxFiles int
	maxDepth     int
}

type repoMapReport struct {
	Root           string                 `json:"root"`
	Summary        repoMapSummary         `json:"summary"`
	Languages      []repomap.Count        `json:"languages"`
	Extensions     []repomap.Count        `json:"extensions"`
	ImportantFiles []string               `json:"importantFiles,omitempty"`
	Files          []repomap.File         `json:"files"`
	Matches        []repomap.SearchResult `json:"matches,omitempty"`
}

type repoMapSummary struct {
	FileCount      int  `json:"fileCount"`
	DirectoryCount int  `json:"directoryCount"`
	MaxDepth       int  `json:"maxDepth"`
	Truncated      bool `json:"truncated,omitempty"`
}

func runRepoMap(args []string, stdout io.Writer, stderr io.Writer, deps appDeps) int {
	options, help, err := parseRepoMapArgs(args)
	if err != nil {
		return writeExecUsageError(stderr, err.Error())
	}
	if help {
		if err := writeRepoMapHelp(stdout); err != nil {
			return exitCrash
		}
		return exitSuccess
	}

	workspaceRoot, err := resolveWorkspaceRoot("", deps)
	if err != nil {
		return writeExecUsageError(stderr, err.Error())
	}
	snapshot, err := repomap.Scan(workspaceRoot, repomap.Options{
		MaxFiles: options.scanMaxFiles,
		MaxDepth: options.maxDepth,
	})
	if err != nil {
		return writeAppError(stderr, err.Error(), exitCrash)
	}

	report := buildRepoMapReport(snapshot, options)
	if options.json {
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(report); err != nil {
			return exitCrash
		}
		return exitSuccess
	}
	if options.query != "" {
		if _, err := fmt.Fprint(stdout, formatRepoMapMatches(options.query, report.Matches)); err != nil {
			return exitCrash
		}
		return exitSuccess
	}
	if _, err := fmt.Fprint(stdout, formatRepoMapSummary(report)); err != nil {
		return exitCrash
	}
	return exitSuccess
}

func parseRepoMapArgs(args []string) (repoMapOptions, bool, error) {
	options := repoMapOptions{
		outputFiles:  defaultRepoMapOutputFiles,
		scanMaxFiles: repomap.DefaultMaxFiles,
		maxDepth:     repomap.DefaultMaxDepth,
	}
	for index := 0; index < len(args); index++ {
		arg := args[index]
		switch {
		case arg == "-h" || arg == "--help" || arg == "help":
			return options, true, nil
		case arg == "--json":
			options.json = true
		case arg == "--query":
			value, next, err := nextFlagValue(args, index, arg)
			if err != nil {
				return options, false, err
			}
			options.query = value
			index = next
		case strings.HasPrefix(arg, "--query="):
			options.query = strings.TrimSpace(strings.TrimPrefix(arg, "--query="))
		case arg == "--max-files":
			value, next, err := nextFlagValue(args, index, arg)
			if err != nil {
				return options, false, err
			}
			parsed, err := parsePositiveRepoMapInt("--max-files", value)
			if err != nil {
				return options, false, err
			}
			options.outputFiles = parsed
			index = next
		case strings.HasPrefix(arg, "--max-files="):
			parsed, err := parsePositiveRepoMapInt("--max-files", strings.TrimPrefix(arg, "--max-files="))
			if err != nil {
				return options, false, err
			}
			options.outputFiles = parsed
		case arg == "--scan-max-files":
			value, next, err := nextFlagValue(args, index, arg)
			if err != nil {
				return options, false, err
			}
			parsed, err := parsePositiveRepoMapInt("--scan-max-files", value)
			if err != nil {
				return options, false, err
			}
			options.scanMaxFiles = parsed
			index = next
		case strings.HasPrefix(arg, "--scan-max-files="):
			parsed, err := parsePositiveRepoMapInt("--scan-max-files", strings.TrimPrefix(arg, "--scan-max-files="))
			if err != nil {
				return options, false, err
			}
			options.scanMaxFiles = parsed
		case arg == "--max-depth":
			value, next, err := nextFlagValue(args, index, arg)
			if err != nil {
				return options, false, err
			}
			parsed, err := parseNonNegativeRepoMapInt("--max-depth", value)
			if err != nil {
				return options, false, err
			}
			options.maxDepth = parsed
			index = next
		case strings.HasPrefix(arg, "--max-depth="):
			parsed, err := parseNonNegativeRepoMapInt("--max-depth", strings.TrimPrefix(arg, "--max-depth="))
			if err != nil {
				return options, false, err
			}
			options.maxDepth = parsed
		case strings.HasPrefix(arg, "-"):
			return options, false, execUsageError{fmt.Sprintf("unknown repo-map flag %q", arg)}
		default:
			return options, false, execUsageError{fmt.Sprintf("unexpected repo-map argument %q", arg)}
		}
	}
	return options, false, nil
}

func parsePositiveRepoMapInt(name string, value string) (int, error) {
	parsed, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || parsed <= 0 {
		return 0, execUsageError{fmt.Sprintf("invalid %s: %s", name, value)}
	}
	return parsed, nil
}

func parseNonNegativeRepoMapInt(name string, value string) (int, error) {
	parsed, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || parsed < 0 {
		return 0, execUsageError{fmt.Sprintf("invalid %s: %s", name, value)}
	}
	return parsed, nil
}

func buildRepoMapReport(snapshot repomap.Snapshot, options repoMapOptions) repoMapReport {
	files := limitedRepoMapFiles(snapshot.Files, options.outputFiles)
	var matches []repomap.SearchResult
	if options.query != "" {
		matches = repomap.Search(snapshot, options.query, options.outputFiles)
		files = files[:0]
		// repomap.Search returns paths from snapshot.Files, so repoMapFileByPath
		// should always find match.Path. If it does not, Search and the snapshot
		// are inconsistent; skip the stale match instead of emitting a partial
		// file record with fabricated metadata.
		for _, match := range matches {
			if file, ok := repoMapFileByPath(snapshot.Files, match.Path); ok {
				files = append(files, file)
			}
		}
	}
	return repoMapReport{
		Root: snapshot.Root,
		Summary: repoMapSummary{
			FileCount:      snapshot.FileCount,
			DirectoryCount: snapshot.DirectoryCount,
			MaxDepth:       snapshot.MaxDepth,
			Truncated:      snapshot.Truncated,
		},
		Languages:      snapshot.LanguageCounts,
		Extensions:     snapshot.ExtensionCounts,
		ImportantFiles: snapshot.ImportantFiles,
		Files:          files,
		Matches:        matches,
	}
}

func limitedRepoMapFiles(files []repomap.File, limit int) []repomap.File {
	if limit <= 0 || len(files) <= limit {
		return append([]repomap.File{}, files...)
	}
	return append([]repomap.File{}, files[:limit]...)
}

func repoMapFileByPath(files []repomap.File, path string) (repomap.File, bool) {
	for _, file := range files {
		if file.Path == path {
			return file, true
		}
	}
	return repomap.File{}, false
}

func formatRepoMapSummary(report repoMapReport) string {
	var b strings.Builder
	fmt.Fprintln(&b, "Repo map")
	fmt.Fprintf(&b, "Root: %s\n", report.Root)
	fmt.Fprintf(&b, "Files: %d  Directories: %d  Max depth: %d\n", report.Summary.FileCount, report.Summary.DirectoryCount, report.Summary.MaxDepth)
	if report.Summary.Truncated {
		fmt.Fprintln(&b, "Truncated: true")
	}
	fmt.Fprintf(&b, "Languages: %s\n", formatRepoMapCounts(report.Languages))
	fmt.Fprintf(&b, "Important files: %s\n", formatRepoMapStrings(report.ImportantFiles))
	fmt.Fprintln(&b, "Files:")
	for _, file := range report.Files {
		fmt.Fprintf(&b, "  %s", file.Path)
		if file.Language != "" {
			fmt.Fprintf(&b, "  %s", file.Language)
		}
		b.WriteByte('\n')
	}
	return b.String()
}

func formatRepoMapMatches(query string, matches []repomap.SearchResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Repo map matches for %q\n", query)
	fmt.Fprintln(&b, "Rank  Score  Reason          Path")
	for index, match := range matches {
		fmt.Fprintf(&b, "%-5d %-6d %-15s %s\n", index+1, match.Score, match.Reason, match.Path)
	}
	if len(matches) == 0 {
		fmt.Fprintln(&b, "No matches.")
	}
	return b.String()
}

func formatRepoMapCounts(counts []repomap.Count) string {
	if len(counts) == 0 {
		return "none"
	}
	parts := make([]string, 0, len(counts))
	for _, count := range counts {
		parts = append(parts, fmt.Sprintf("%s=%d", count.Name, count.Count))
	}
	return strings.Join(parts, ", ")
}

func formatRepoMapStrings(values []string) string {
	if len(values) == 0 {
		return "none"
	}
	return strings.Join(values, ", ")
}

func writeRepoMapHelp(w io.Writer) error {
	_, err := fmt.Fprint(w, `Usage:
  zero repo-map [flags]

Builds a deterministic repository map for agent context and local inspection.

Flags:
      --json                Print JSON report
      --query <text>        Print ranked file matches for a query
      --max-files <number>  Limit files or query matches shown
      --scan-max-files <n>  Limit files scanned before truncation
      --max-depth <number>  Limit scan depth
  -h, --help                Show this help
`)
	return err
}
