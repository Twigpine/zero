package agenteval

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/Gitlawb/zero/internal/execution"
)

type Materializer struct{}

type MaterializeInput struct {
	WorkRoot string
}

type Workspace struct {
	Path        string
	TaskID      string
	FixturePath string
}

func (Materializer) MaterializeTask(ctx context.Context, suitePath string, task Task, input MaterializeInput) (Workspace, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	workRoot := strings.TrimSpace(input.WorkRoot)
	if workRoot == "" {
		return Workspace{}, errors.New("work root is required")
	}
	fixturePath, err := resolveFixturePath(suitePath, task.WorkspaceFixture)
	if err != nil {
		return Workspace{}, err
	}
	info, err := os.Stat(fixturePath)
	if err != nil {
		return Workspace{}, fmt.Errorf("workspace fixture: %w", err)
	}
	if !info.IsDir() {
		return Workspace{}, fmt.Errorf("workspace fixture must be a directory: %s", fixturePath)
	}
	if err := os.MkdirAll(workRoot, 0o755); err != nil {
		return Workspace{}, fmt.Errorf("create work root: %w", err)
	}
	workspacePath, err := os.MkdirTemp(workRoot, safeWorkspaceName(task.ID)+"-")
	if err != nil {
		return Workspace{}, fmt.Errorf("create workspace: %w", err)
	}
	if err := copyFixtureDir(fixturePath, workspacePath); err != nil {
		_ = os.RemoveAll(workspacePath)
		return Workspace{}, err
	}
	if err := initGitBaseline(ctx, workspacePath); err != nil {
		_ = os.RemoveAll(workspacePath)
		return Workspace{}, err
	}
	return Workspace{Path: workspacePath, TaskID: task.ID, FixturePath: fixturePath}, nil
}

func resolveFixturePath(suitePath string, fixture string) (string, error) {
	fixture = strings.TrimSpace(fixture)
	if fixture == "" {
		return "", errors.New("workspace fixture is required")
	}
	if filepath.IsAbs(fixture) {
		return "", errors.New("workspace fixture must be relative to suite path")
	}
	base, err := filepath.Abs(filepath.Dir(suitePath))
	if err != nil {
		return "", err
	}
	canonicalBase, err := filepath.EvalSymlinks(base)
	if err != nil {
		return "", fmt.Errorf("resolve suite directory: %w", err)
	}
	resolved, err := filepath.Abs(filepath.Join(base, filepath.FromSlash(fixture)))
	if err != nil {
		return "", err
	}
	canonicalResolved, err := evalSymlinkPath(resolved)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(canonicalBase, canonicalResolved)
	if err != nil {
		return "", err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", errors.New("workspace fixture must stay within suite directory")
	}
	return canonicalResolved, nil
}

func evalSymlinkPath(path string) (string, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	current := filepath.Clean(path)
	missing := []string{}
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}
			return filepath.Clean(resolved), nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", err
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}

func copyFixtureDir(src string, dst string) error {
	return filepath.WalkDir(src, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() && entry.Name() == ".git" {
			return filepath.SkipDir
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		target := filepath.Join(dst, rel)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			// Reject symlinks, devices, sockets, and other non-regular entries
			// rather than silently dropping them, which would materialize an
			// incomplete workspace and skew scoring.
			return fmt.Errorf("unsupported fixture entry %q: only regular files and directories are supported", rel)
		}
		return copyFixtureFile(path, target, info.Mode().Perm())
	})
}

func copyFixtureFile(src string, dst string, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	source, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = source.Close() }()
	target, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(target, source)
	// Close may flush buffered data and surface write errors (e.g. disk full),
	// so its error must not be discarded.
	closeErr := target.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

func initGitBaseline(ctx context.Context, workspace string) error {
	commands := [][]string{
		{"init"},
		{"config", "user.name", "Zero Eval"},
		{"config", "user.email", "zero-eval@example.invalid"},
		{"add", "."},
		{"commit", "-m", "baseline"},
	}
	for _, args := range commands {
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = workspace
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Zero Eval",
			"GIT_AUTHOR_EMAIL=zero-eval@example.invalid",
			"GIT_COMMITTER_NAME=Zero Eval",
			"GIT_COMMITTER_EMAIL=zero-eval@example.invalid",
		)
		var output bytes.Buffer
		cmd.Stdout = &output
		cmd.Stderr = &output
		if err := execution.RunCommand(ctx, cmd); err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			return fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(output.String()))
		}
	}
	return nil
}

func safeWorkspaceName(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "task"
	}
	var builder strings.Builder
	for _, r := range value {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			builder.WriteRune(r)
		} else {
			builder.WriteByte('-')
		}
	}
	name := strings.Trim(builder.String(), "-")
	if name == "" {
		return "task"
	}
	return name
}
