package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/config"
)

func TestRunContextHelp(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := runWithDeps([]string{"context", "--help"}, &stdout, &stderr, appDeps{})

	if exitCode != exitSuccess {
		t.Fatalf("expected exit code %d, got %d: %s", exitSuccess, exitCode, stderr.String())
	}
	for _, want := range []string{"Usage:", "zero context [flags]", "--json", "-h, --help"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("expected context help to contain %q, got %q", want, stdout.String())
		}
	}
	if stderr.Len() != 0 {
		t.Fatalf("expected empty stderr, got %q", stderr.String())
	}
}

func TestRunContextTextReport(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cwd := t.TempDir()

	exitCode := runWithDeps([]string{"context"}, &stdout, &stderr, appDeps{
		getwd: func() (string, error) {
			return cwd, nil
		},
		resolveConfig: func(workspaceRoot string, overrides config.Overrides) (config.ResolvedConfig, error) {
			if workspaceRoot != cwd {
				t.Fatalf("workspaceRoot = %q, want %q", workspaceRoot, cwd)
			}
			return config.ResolvedConfig{
				Provider: config.ProviderProfile{
					Name:         "openai",
					ProviderKind: config.ProviderKindOpenAI,
					Model:        "gpt-4.1",
				},
			}, nil
		},
	})

	if exitCode != exitSuccess {
		t.Fatalf("expected exit code %d, got %d: %s", exitSuccess, exitCode, stderr.String())
	}
	for _, want := range []string{"Zero context report", "root: " + cwd, "model: gpt-4.1"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("expected context output to contain %q, got %q", want, stdout.String())
		}
	}
	if stderr.Len() != 0 {
		t.Fatalf("expected empty stderr, got %q", stderr.String())
	}
}

func TestRunContextJSONReport(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cwd := t.TempDir()

	exitCode := runWithDeps([]string{"context", "--json"}, &stdout, &stderr, appDeps{
		getwd: func() (string, error) {
			return cwd, nil
		},
		resolveConfig: func(string, config.Overrides) (config.ResolvedConfig, error) {
			return config.ResolvedConfig{
				Provider: config.ProviderProfile{
					Name:         "openai",
					ProviderKind: config.ProviderKindOpenAI,
					Model:        "gpt-4.1",
				},
			}, nil
		},
	})

	if exitCode != exitSuccess {
		t.Fatalf("expected exit code %d, got %d: %s", exitSuccess, exitCode, stderr.String())
	}
	var report struct {
		Root          string `json:"root"`
		ModelID       string `json:"modelId"`
		ContextWindow int    `json:"contextWindow"`
		ToolCount     int    `json:"toolCount"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("context JSON did not decode: %v\n%s", err, stdout.String())
	}
	if report.Root != cwd {
		t.Fatalf("Root = %q, want %q", report.Root, cwd)
	}
	if report.ModelID != "gpt-4.1" {
		t.Fatalf("ModelID = %q, want gpt-4.1", report.ModelID)
	}
	if report.ContextWindow <= 0 {
		t.Fatalf("ContextWindow = %d, want > 0", report.ContextWindow)
	}
	if report.ToolCount <= 0 {
		t.Fatalf("ToolCount = %d, want > 0", report.ToolCount)
	}
	if stderr.Len() != 0 {
		t.Fatalf("expected empty stderr, got %q", stderr.String())
	}
}

func TestRunContextRejectsUnknownFlag(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := runWithDeps([]string{"context", "--wat"}, &stdout, &stderr, appDeps{})

	if exitCode != exitUsage {
		t.Fatalf("expected usage exit %d, got %d", exitUsage, exitCode)
	}
	if stdout.Len() != 0 {
		t.Fatalf("expected empty stdout, got %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), `unknown context flag "--wat"`) {
		t.Fatalf("expected unknown flag error, got %q", stderr.String())
	}
}
