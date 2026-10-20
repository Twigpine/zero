package skills

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// A skill is a directory, not just its SKILL.md: the instructions often point at
// scripts, references or assets that sit beside them (the agentskills.io
// layout). Install copies that directory, and the skill tool says where it is.

// Install refuses a skill tree larger than this. A skill is a handful of
// scripts and documents; a source that is really a whole repository or a pile
// of binaries should fail loudly instead of filling the skills directory.
var (
	maxSkillTreeFiles = 1000
	maxSkillTreeBytes = int64(32 << 20)
)

// bundledFileListLimit caps how many bundled files the skill tool lists, so a
// large skill does not crowd the instructions out of the tool output.
const bundledFileListLimit = 50

// skipSkillEntry reports whether a skill tree entry is left out of an install,
// a tree hash and a listing alike. Hidden entries cover VCS metadata (.git,
// which a cloned source always has) and local clutter such as .DS_Store or
// .env; they are not part of what a skill ships.
func skipSkillEntry(name string) bool {
	return strings.HasPrefix(name, ".")
}

// copySkillTree copies the skill directory src into dst, which must exist.
// Regular files are copied with normalized permissions (0o755 when the source
// is executable, 0o644 otherwise), so a script stays runnable without carrying
// over group- or world-writable bits. Symlinks are not followed or copied:
// their relative paths come back in skipped, so the caller can say so. Other
// non-regular entries (devices, sockets, pipes) are left out.
func copySkillTree(src, dst string) (skipped []string, err error) {
	files := 0
	var total int64
	err = filepath.WalkDir(src, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == src {
			return nil
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if skipSkillEntry(entry.Name()) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Type()&fs.ModeSymlink != 0 {
			skipped = append(skipped, filepath.ToSlash(rel))
			return nil
		}
		target := filepath.Join(dst, rel)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		files++
		total += info.Size()
		if files > maxSkillTreeFiles {
			return fmt.Errorf("skill has more than %d files; a skill directory should hold only what the skill uses", maxSkillTreeFiles)
		}
		if total > maxSkillTreeBytes {
			return fmt.Errorf("skill is larger than %d MiB; a skill directory should hold only what the skill uses", maxSkillTreeBytes>>20)
		}
		mode := os.FileMode(0o644)
		if info.Mode().Perm()&0o111 != 0 {
			mode = 0o755
		}
		return copySkillFile(path, target, mode)
	})
	if err != nil {
		return nil, err
	}
	return skipped, nil
}

func copySkillFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	// The create mode is filtered by the umask; set it outright so an
	// executable script is still executable wherever the install ran.
	return os.Chmod(dst, mode)
}

// skillTreeHash hashes every file a skill ships, with the same entries
// copySkillTree copies: its relative path, whether it is executable, and its
// content. Sorted by path, so the hash depends only on the tree.
func skillTreeHash(dir string) (string, error) {
	type entry struct {
		rel  string
		exec bool
		sum  string
	}
	var entries []entry
	err := filepath.WalkDir(dir, func(path string, item fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == dir {
			return nil
		}
		if skipSkillEntry(item.Name()) {
			if item.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !item.Type().IsRegular() {
			return nil
		}
		info, err := item.Info()
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		entries = append(entries, entry{rel: filepath.ToSlash(rel), exec: info.Mode().Perm()&0o111 != 0, sum: hex.EncodeToString(sum[:])})
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].rel < entries[j].rel })
	hash := sha256.New()
	for _, item := range entries {
		flag := "-"
		if item.exec {
			flag = "x"
		}
		// Length-prefixed, so no path can be read as the start of another entry.
		fmt.Fprintf(hash, "%d:%s|%s|%s;", len(item.rel), item.rel, flag, item.sum)
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil)), nil
}

// BundledFiles lists the files a skill ships besides its SKILL.md, as
// slash-separated paths relative to the skill's directory, sorted. Hidden
// entries and symlinks are left out: the listing tells the model what the
// skill brings, and is not a way to reach anything outside it. total is the
// number of files found; at most limit come back.
func BundledFiles(skill Skill, limit int) (files []string, total int, err error) {
	dir := filepath.Dir(skill.Path)
	if strings.TrimSpace(skill.Path) == "" {
		return nil, 0, errors.New("skill has no path")
	}
	err = filepath.WalkDir(dir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == dir {
			return nil
		}
		if skipSkillEntry(entry.Name()) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == skillFileName {
			return nil
		}
		total++
		if total > maxSkillTreeFiles {
			// A directory this large is not something the listing should walk
			// to the end of; the count stops being exact past the cap.
			return filepath.SkipAll
		}
		files = append(files, rel)
		return nil
	})
	if err != nil {
		return nil, 0, err
	}
	sort.Strings(files)
	if limit >= 0 && len(files) > limit {
		files = files[:limit]
	}
	return files, total, nil
}

// ToolOutput is what the skill tool returns for a loaded skill: its SKILL.md
// body and, when the skill ships other files, the directory they are in and
// what they are, so instructions that say "run scripts/extract.py" can be
// followed. A skill that is only a SKILL.md gets its body back unchanged.
func ToolOutput(skill Skill) string {
	files, total, err := BundledFiles(skill, bundledFileListLimit)
	if err != nil || total == 0 {
		return skill.Content
	}
	var builder strings.Builder
	builder.WriteString(strings.TrimRight(skill.Content, "\n"))
	builder.WriteString("\n\n---\n")
	fmt.Fprintf(&builder, "This skill's files are in %s. Paths in the instructions above are relative to that directory.\n", filepath.Dir(skill.Path))
	for _, file := range files {
		builder.WriteString("- " + file + "\n")
	}
	if more := total - len(files); more > 0 {
		fmt.Fprintf(&builder, "- (and %d more)\n", more)
	}
	return builder.String()
}
