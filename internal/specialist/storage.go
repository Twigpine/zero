package specialist

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/Twigpine/zero/internal/fsutil"
)

type Storage struct {
	paths            Paths
	writeReplacement func(string, string) error
}

type CreateInput struct {
	Name            string
	Description     string
	SystemPrompt    string
	Extends         string
	Model           string
	ReasoningEffort string
	Tools           []string
	Location        Location
	Overwrite       bool
}

type DeleteInput struct {
	Name     string
	Location Location
}

func NewStorage(paths Paths) *Storage {
	return &Storage{paths: paths}
}

func (storage *Storage) Create(input CreateInput) (Manifest, error) {
	location := normalizeWritableLocation(input.Location)
	path, err := storage.path(input.Name, location)
	if err != nil {
		return Manifest{}, err
	}
	systemPrompt := strings.TrimSpace(input.SystemPrompt)
	if systemPrompt == "" && strings.TrimSpace(input.Extends) == "" {
		systemPrompt = strings.TrimSpace(input.Description)
	}
	manifest := Manifest{
		Metadata: Metadata{
			Name:            strings.TrimSpace(input.Name),
			Description:     strings.TrimSpace(input.Description),
			Extends:         strings.TrimSpace(input.Extends),
			Model:           strings.TrimSpace(input.Model),
			ReasoningEffort: strings.TrimSpace(input.ReasoningEffort),
			Tools:           trimStringList(input.Tools),
		},
		SystemPrompt: systemPrompt,
		Location:     location,
		FilePath:     path,
	}
	if err := Validate(&manifest); err != nil {
		return Manifest{}, err
	}
	if strings.TrimSpace(manifest.SystemPrompt) == "" && manifest.Metadata.Extends == "" {
		return Manifest{}, fmt.Errorf("specialist %q requires a system prompt", manifest.Metadata.Name)
	}
	content := FormatMarkdown(manifest)
	dir := filepath.Dir(path)
	specialistFilesMu.Lock()
	defer specialistFilesMu.Unlock()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Manifest{}, fmt.Errorf("create specialist directory: %w", err)
	}
	if input.Overwrite {
		writeReplacement := storage.writeReplacement
		if writeReplacement == nil {
			writeReplacement = writeSpecialistReplacement
		}
		if err := writeReplacement(path, content); err != nil {
			var cleanupErr *fsutil.CommittedReplacementCleanupError
			if errors.As(err, &cleanupErr) {
				manifest.Warnings = append(manifest.Warnings, fmt.Sprintf("specialist was updated, but replacement backup %s could not be removed: %v", cleanupErr.BackupPath, cleanupErr.Cause))
				return manifest, nil
			}
			return Manifest{}, err
		}
		return manifest, nil
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if os.IsExist(err) {
			return Manifest{}, fmt.Errorf("specialist already exists: %s", manifest.Metadata.Name)
		}
		return Manifest{}, fmt.Errorf("write specialist file: %w", err)
	}
	if _, err := file.WriteString(content); err != nil {
		_ = file.Close()
		return Manifest{}, fmt.Errorf("write specialist file: %w", err)
	}
	if err := file.Close(); err != nil {
		return Manifest{}, fmt.Errorf("close specialist file: %w", err)
	}
	return manifest, nil
}

func writeSpecialistReplacement(path string, content string) error {
	return writeSpecialistReplacementWith(path, content, nil, syncSpecialistDir)
}

func writeSpecialistReplacementWith(path string, content string, rename func(string, string) error, syncDir func(string) error) (err error) {
	temp, err := os.CreateTemp(filepath.Dir(path), ".specialist-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary specialist file: %w", err)
	}
	tempPath := temp.Name()
	// The temporary file never survives this function, on any path: a successful
	// replace has already consumed it, and every failure — including the Windows
	// partial-replace recoveries in fsutil — discards it here. Recovery from a
	// failed overwrite is therefore always about the ORIGINAL (which fsutil either
	// rolls back or names in its error), never about this file, and no error text
	// should tell an operator to go looking for it.
	defer func() {
		_ = temp.Close()
		_ = os.Remove(tempPath)
	}()

	if err := temp.Chmod(0o600); err != nil {
		return fmt.Errorf("set temporary specialist file permissions: %w", err)
	}
	info, err := os.Lstat(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("inspect specialist file: %w", err)
	}
	if err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing to overwrite symlink specialist file: %s", path)
		}
		if runtime.GOOS != "windows" {
			// The old in-place overwrite retained manually configured Unix modes.
			// Preserve that behavior when publishing a replacement inode.
			if err := temp.Chmod(info.Mode().Perm()); err != nil {
				return fmt.Errorf("preserve specialist file permissions: %w", err)
			}
		}
	}
	if _, err := temp.WriteString(content); err != nil {
		return fmt.Errorf("write temporary specialist file: %w", err)
	}
	if err := temp.Sync(); err != nil {
		return fmt.Errorf("sync temporary specialist file: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close temporary specialist file: %w", err)
	}

	// On Unix, the same-directory rename publishes the complete file atomically.
	// On Windows, ReplaceFileW is used to preserve the destination DACL rather
	// than publishing the temporary file's inherited DACL. ReplaceFileW can
	// briefly leave the destination name absent; specialistFilesMu prevents
	// Zero-managed loads in this process from observing that window, but external
	// processes and editors are not synchronized.
	if err := fsutil.ReplaceWithRetry(tempPath, path, rename); err != nil {
		return fmt.Errorf("replace specialist file: %w", err)
	}
	// The replacement is committed at this point. As in the sessions store,
	// opening/fsyncing the parent directory only improves crash durability and
	// must not turn a successful overwrite into an API failure.
	_ = syncDir(filepath.Dir(path))
	return nil
}

func syncSpecialistDir(path string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	dir, err := os.Open(path)
	if err != nil {
		return nil
	}
	if err := dir.Sync(); err != nil {
		_ = dir.Close()
		return err
	}
	return dir.Close()
}

func (storage *Storage) Delete(input DeleteInput) (string, error) {
	location := normalizeWritableLocation(input.Location)
	path, err := storage.path(input.Name, location)
	if err != nil {
		return "", err
	}
	specialistFilesMu.Lock()
	defer specialistFilesMu.Unlock()
	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("specialist not found: %s", strings.TrimSpace(input.Name))
		}
		return "", fmt.Errorf("delete specialist file: %w", err)
	}
	return path, nil
}

func (storage *Storage) Path(name string, location Location) (string, error) {
	return storage.path(name, normalizeWritableLocation(location))
}

func FormatMarkdown(manifest Manifest) string {
	var builder strings.Builder
	builder.WriteString("---\n")
	writeMetadataString(&builder, "name", manifest.Metadata.Name)
	writeMetadataString(&builder, "description", manifest.Metadata.Description)
	writeMetadataString(&builder, "extends", manifest.Metadata.Extends)
	writeMetadataString(&builder, "model", manifest.Metadata.Model)
	writeMetadataString(&builder, "reasoningEffort", manifest.Metadata.ReasoningEffort)
	if len(manifest.Metadata.Tools) > 0 {
		builder.WriteString("tools:\n")
		for _, tool := range manifest.Metadata.Tools {
			tool = strings.TrimSpace(tool)
			if tool == "" {
				continue
			}
			builder.WriteString("  - ")
			builder.WriteString(strconv.Quote(tool))
			builder.WriteByte('\n')
		}
	}
	builder.WriteString("---\n\n")
	builder.WriteString(strings.TrimSpace(manifest.SystemPrompt))
	builder.WriteByte('\n')
	return builder.String()
}

func (storage *Storage) path(name string, location Location) (string, error) {
	name = strings.TrimSpace(name)
	if !namePattern.MatchString(name) {
		return "", fmt.Errorf("invalid specialist name %q: use lowercase letters, numbers, and dashes", name)
	}
	dir, err := storage.dir(location)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, name+".md"), nil
}

func (storage *Storage) dir(location Location) (string, error) {
	switch location {
	case LocationUser:
		if strings.TrimSpace(storage.paths.UserDir) == "" {
			return "", fmt.Errorf("user specialist directory is not configured")
		}
		return storage.paths.UserDir, nil
	case LocationProject:
		if strings.TrimSpace(storage.paths.ProjectDir) == "" {
			return "", fmt.Errorf("project specialist directory is not configured")
		}
		return storage.paths.ProjectDir, nil
	default:
		return "", fmt.Errorf("invalid specialist location %q", location)
	}
}

func normalizeWritableLocation(location Location) Location {
	if location == "" {
		return LocationUser
	}
	return location
}

func trimStringList(values []string) []string {
	out := []string{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			out = append(out, value)
		}
	}
	return out
}

func writeMetadataString(builder *strings.Builder, key string, value string) {
	value = strings.TrimSpace(value)
	if value == "" {
		return
	}
	builder.WriteString(key)
	builder.WriteString(": ")
	builder.WriteString(strconv.Quote(value))
	builder.WriteByte('\n')
}
