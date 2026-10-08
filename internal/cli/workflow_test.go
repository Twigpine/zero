package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Gitlawb/zero/internal/config"
	"github.com/Gitlawb/zero/internal/redaction"
	"github.com/Gitlawb/zero/internal/selfverify"
	"github.com/Gitlawb/zero/internal/testrunner"
	"github.com/Gitlawb/zero/internal/verify"
	"github.com/Gitlawb/zero/internal/worktrees"
	"github.com/Gitlawb/zero/internal/zerogit"
	"github.com/Gitlawb/zero/internal/zeroruntime"
)

// featureBranchInspect returns an inspectChanges stub for ensureFeatureBranch:
// a clean working tree when BaseRef is empty, and the given committed range
// summary when BaseRef is set (the remote/branch naming path).
func featureBranchInspect(files []zerogit.FileChange, diff string) func(context.Context, zerogit.InspectOptions) (zerogit.ChangeSummary, error) {
	return func(ctx context.Context, options zerogit.InspectOptions) (zerogit.ChangeSummary, error) {
		if strings.TrimSpace(options.BaseRef) == "" {
			return zerogit.ChangeSummary{Clean: true}, nil
		}
		return zerogit.ChangeSummary{Files: files, Diff: diff}, nil
	}
}

func TestRunWorktreesPrepareTextAndJSON(t *testing.T) {
	cwd := t.TempDir()
	base := t.TempDir()
	prepared := worktrees.Result{
		Name:         "agent-task",
		Path:         filepath.Join(base, "agent-task"),
		RepoRoot:     cwd,
		SourceBranch: "main",
		SourceCommit: "abc1234",
	}

	for _, args := range [][]string{
		{"worktrees", "prepare", "--name", "agent-task", "--dir", base},
		{"worktrees", "prepare", "--name=agent-task", "--dir=" + base, "--json"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var stdout bytes.Buffer
			var stderr bytes.Buffer
			exitCode := runWithDeps(args, &stdout, &stderr, appDeps{
				getwd: func() (string, error) { return cwd, nil },
				prepareWorktree: func(ctx context.Context, options worktrees.Options) (worktrees.Result, error) {
					if options.Cwd != cwd || options.Name != "agent-task" || options.BaseDir != base {
						t.Fatalf("unexpected worktree options: %#v", options)
					}
					return prepared, nil
				},
			})

			if exitCode != exitSuccess {
				t.Fatalf("expected exit code %d, got %d: %s", exitSuccess, exitCode, stderr.String())
			}
			if stderr.Len() != 0 {
				t.Fatalf("expected empty stderr, got %q", stderr.String())
			}
			if strings.Contains(strings.Join(args, " "), "--json") {
				var decoded worktrees.Result
				if err := json.Unmarshal(stdout.Bytes(), &decoded); err != nil {
					t.Fatalf("decode worktree JSON: %v\n%s", err, stdout.String())
				}
				if decoded.Path != prepared.Path || decoded.Name != prepared.Name {
					t.Fatalf("unexpected JSON result: %#v", decoded)
				}
			} else if !strings.Contains(stdout.String(), "Zero worktree ready") || !strings.Contains(stdout.String(), prepared.Path) {
				t.Fatalf("unexpected worktree text output: %q", stdout.String())
			}
		})
	}
}

func TestRunWorktreesPrepareReportsErrors(t *testing.T) {
	cwd := t.TempDir()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := runWithDeps([]string{"worktrees", "prepare", "--name", "bad"}, &stdout, &stderr, appDeps{
		getwd: func() (string, error) { return cwd, nil },
		prepareWorktree: func(context.Context, worktrees.Options) (worktrees.Result, error) {
			return worktrees.Result{}, errors.New("not a git repository")
		},
	})

	if exitCode != exitUsage {
		t.Fatalf("expected usage exit %d, got %d", exitUsage, exitCode)
	}
	if stdout.Len() != 0 {
		t.Fatalf("expected empty stdout, got %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "not a git repository") {
		t.Fatalf("expected worktree error, got %q", stderr.String())
	}
}

func TestRunWorktreesPrepareRedactsPathsInOutput(t *testing.T) {
	secret := "sk-proj-abcdefghijklmnopqrstuvwxyz0"
	cwd := filepath.Join(t.TempDir(), secret, "repo")
	if err := os.MkdirAll(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	prepared := worktrees.Result{
		Name:         "agent-task",
		Path:         filepath.Join(t.TempDir(), secret, "agent-task"),
		RepoRoot:     cwd,
		SourceBranch: "feature/" + secret,
		SourceCommit: "abc1234",
	}

	for _, args := range [][]string{
		{"worktrees", "prepare", "--name", "agent-task"},
		{"worktrees", "prepare", "--name", "agent-task", "--json"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var stdout bytes.Buffer
			var stderr bytes.Buffer
			exitCode := runWithDeps(args, &stdout, &stderr, appDeps{
				getwd: func() (string, error) { return cwd, nil },
				prepareWorktree: func(context.Context, worktrees.Options) (worktrees.Result, error) {
					return prepared, nil
				},
			})

			if exitCode != exitSuccess {
				t.Fatalf("expected exit code %d, got %d: %s", exitSuccess, exitCode, stderr.String())
			}
			if strings.Contains(stdout.String(), secret) {
				t.Fatalf("worktree output leaked secret path segment: %q", stdout.String())
			}
			if !strings.Contains(stdout.String(), redaction.RedactedSecret) {
				t.Fatalf("expected redaction marker in worktree output, got %q", stdout.String())
			}
		})
	}
}

func TestRunWorktreesPrepareRejectsDuplicateNames(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := runWithDeps([]string{"worktrees", "prepare", "--name", "first", "second"}, &stdout, &stderr, appDeps{
		prepareWorktree: func(context.Context, worktrees.Options) (worktrees.Result, error) {
			t.Fatal("prepareWorktree should not be called for invalid flags")
			return worktrees.Result{}, nil
		},
	})

	if exitCode != exitUsage {
		t.Fatalf("expected usage exit %d, got %d", exitUsage, exitCode)
	}
	if stdout.Len() != 0 {
		t.Fatalf("expected empty stdout, got %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "worktree name was provided more than once") {
		t.Fatalf("expected duplicate name error, got %q", stderr.String())
	}
}

func TestRunWorktreesRelease(t *testing.T) {
	worktreeDir := t.TempDir()
	var releasedPath string

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := runWithDeps([]string{"worktrees", "release", worktreeDir}, &stdout, &stderr, appDeps{
		releaseWorktree: func(ctx context.Context, options worktrees.Options, path string) error {
			releasedPath = path
			return nil
		},
	})

	if exitCode != exitSuccess {
		t.Fatalf("expected exit code %d, got %d: %s", exitSuccess, exitCode, stderr.String())
	}
	if releasedPath != worktreeDir {
		t.Fatalf("released path = %q, want %q", releasedPath, worktreeDir)
	}
	if !strings.Contains(stdout.String(), worktreeDir) {
		t.Fatalf("expected confirmation output to mention path, got %q", stdout.String())
	}
}

func TestRunWorktreesReleaseNormalizesRelativePath(t *testing.T) {
	// git worktree unlock matches against the path git recorded when the
	// worktree was created, so a relative argument (resolved against
	// whatever directory the caller happens to be running `zero` from) can
	// fail to match. Chdir into a known directory and pass a relative
	// argument to confirm it reaches releaseWorktree as an absolute path.
	origWd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	parent := t.TempDir()
	worktreeDir := filepath.Join(parent, "task-a")
	if err := os.Mkdir(worktreeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(parent); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.Chdir(origWd); err != nil {
			t.Fatal(err)
		}
	}()

	// filepath.Abs resolves against os.Getwd, whose spelling of the temp dir
	// can differ from t.TempDir()'s (macOS reports /private/var for /var), so
	// derive the expected value through the same resolution instead of
	// joining onto the lexical parent path.
	expectedPath, err := filepath.Abs("task-a")
	if err != nil {
		t.Fatal(err)
	}

	var releasedPath string
	var stdout, stderr bytes.Buffer
	exitCode := runWithDeps([]string{"worktrees", "release", "task-a"}, &stdout, &stderr, appDeps{
		releaseWorktree: func(ctx context.Context, options worktrees.Options, path string) error {
			releasedPath = path
			return nil
		},
	})

	if exitCode != exitSuccess {
		t.Fatalf("expected exit code %d, got %d: %s", exitSuccess, exitCode, stderr.String())
	}
	if releasedPath != expectedPath {
		t.Fatalf("released path = %q, want absolute %q", releasedPath, expectedPath)
	}
}

func TestRunWorktreesReleaseRequiresPath(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := runWithDeps([]string{"worktrees", "release"}, &stdout, &stderr, appDeps{
		releaseWorktree: func(context.Context, worktrees.Options, string) error {
			t.Fatal("releaseWorktree should not be called without a path")
			return nil
		},
	})

	if exitCode != exitUsage {
		t.Fatalf("expected usage exit %d, got %d", exitUsage, exitCode)
	}
	if !strings.Contains(stderr.String(), "requires a worktree path") {
		t.Fatalf("expected missing-path error, got %q", stderr.String())
	}
}

func TestRunWorktreesReleaseWiresWorkspaceCwd(t *testing.T) {
	// Release only consults Options.Cwd when the worktree directory itself is
	// already gone (deleted by hand instead of released); git then has to run
	// from the source repository to clear the orphaned lock. The CLI must
	// wire the resolved workspace root through, or that advertised recovery
	// path runs git from a possibly non-repository directory and the lock is
	// never cleared (Clean skips locked entries).
	root := t.TempDir()
	var gotCwd string

	var stdout, stderr bytes.Buffer
	exitCode := runWithDeps([]string{"worktrees", "release", filepath.Join(root, "already-deleted")}, &stdout, &stderr, appDeps{
		getwd: func() (string, error) { return root, nil },
		releaseWorktree: func(ctx context.Context, options worktrees.Options, path string) error {
			gotCwd = options.Cwd
			return nil
		},
	})

	if exitCode != exitSuccess {
		t.Fatalf("expected exit code %d, got %d: %s", exitSuccess, exitCode, stderr.String())
	}
	if gotCwd != root {
		t.Fatalf("release Options.Cwd = %q, want workspace root %q", gotCwd, root)
	}
}

func TestRunWorktreesReleaseHonorsExplicitCwd(t *testing.T) {
	// The deleted-path recovery cannot derive the source repository from the
	// worktree path (the directory is gone and its name is a one-way hash),
	// so -C names it explicitly; the resolved root must reach Release as
	// Options.Cwd regardless of where the command was launched.
	launchDir := t.TempDir()
	repoDir := t.TempDir()
	var gotCwd string

	var stdout, stderr bytes.Buffer
	exitCode := runWithDeps([]string{"worktrees", "release", "-C", repoDir, filepath.Join(repoDir, "already-deleted")}, &stdout, &stderr, appDeps{
		getwd: func() (string, error) { return launchDir, nil },
		releaseWorktree: func(ctx context.Context, options worktrees.Options, path string) error {
			gotCwd = options.Cwd
			return nil
		},
	})

	if exitCode != exitSuccess {
		t.Fatalf("expected exit code %d, got %d: %s", exitSuccess, exitCode, stderr.String())
	}
	if gotCwd != repoDir {
		t.Fatalf("release Options.Cwd = %q, want explicit -C root %q", gotCwd, repoDir)
	}
}

func TestRunWorktreesReleaseReportsErrors(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := runWithDeps([]string{"worktrees", "release", "/no/such/worktree"}, &stdout, &stderr, appDeps{
		releaseWorktree: func(context.Context, worktrees.Options, string) error {
			return errors.New("unlock git worktree: not a valid worktree")
		},
	})

	if exitCode != exitUsage {
		t.Fatalf("expected usage exit %d, got %d", exitUsage, exitCode)
	}
	if !strings.Contains(stderr.String(), "not a valid worktree") {
		t.Fatalf("expected underlying error, got %q", stderr.String())
	}
}

func TestRunWorktreesReleaseRedactsErrorText(t *testing.T) {
	// Release errors interpolate the caller-supplied path (see
	// verifyZeroOwnedWorktree's "refusing to release %s" messages); unlike the
	// success path, which redacts before printing, the error was previously
	// forwarded to stderr verbatim, so a rejected path containing a key-shaped
	// segment would reach terminal/model-visible output unredacted.
	secret := "sk-proj-abcDEF123_ghiJKL456-mnoPQR789stu"
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := runWithDeps([]string{"worktrees", "release", "/tmp/" + secret + "/task"}, &stdout, &stderr, appDeps{
		releaseWorktree: func(context.Context, worktrees.Options, string) error {
			return fmt.Errorf("refusing to release /tmp/%s/task: not a registered worktree of this repository", secret)
		},
	})

	if exitCode != exitUsage {
		t.Fatalf("expected usage exit %d, got %d", exitUsage, exitCode)
	}
	if strings.Contains(stderr.String(), secret) {
		t.Fatalf("release error leaked unredacted key-shaped path: %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "[REDACTED]") {
		t.Fatalf("expected redaction placeholder in error output, got %q", stderr.String())
	}
}

func TestRunVerifyTextAndJSON(t *testing.T) {
	cwd := t.TempDir()
	plan := verify.Plan{Root: cwd, Checks: []verify.Check{{ID: "go.test", Name: "Go tests", Command: []string{"go", "test", "./..."}}}}
	report := verify.Report{
		Root:      cwd,
		StartedAt: "2026-06-05T11:00:00Z",
		EndedAt:   "2026-06-05T11:00:01Z",
		OK:        true,
		Summary:   verify.Summary{Total: 1, Passed: 1},
		Results: []verify.Result{{
			ID:       "go.test",
			Name:     "Go tests",
			Command:  []string{"go", "test", "./..."},
			Status:   verify.StatusPass,
			ExitCode: 0,
			Stdout:   "ok",
			TestSummary: &testrunner.Summary{
				Framework: testrunner.FrameworkGo,
				Total:     2,
				Passed:    1,
				Failed:    1,
				Failures:  []testrunner.Failure{{Name: "TestBroken"}},
			},
		}},
	}

	for _, args := range [][]string{
		{"verify"},
		{"verify", "--json", "--only", "go.test"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var stdout bytes.Buffer
			var stderr bytes.Buffer
			exitCode := runWithDeps(args, &stdout, &stderr, appDeps{
				getwd: func() (string, error) { return cwd, nil },
				detectVerifyPlan: func(root string) (verify.Plan, error) {
					if root != cwd {
						t.Fatalf("verify root = %q, want %q", root, cwd)
					}
					return plan, nil
				},
				runVerify: func(ctx context.Context, gotPlan verify.Plan, options verify.RunOptions) verify.Report {
					if gotPlan.Root != cwd {
						t.Fatalf("plan root = %q, want %q", gotPlan.Root, cwd)
					}
					if strings.Contains(strings.Join(args, " "), "--only") && (len(options.Only) != 1 || options.Only[0] != "go.test") {
						t.Fatalf("Only = %#v, want go.test", options.Only)
					}
					return report
				},
				now: fixedCLITime("2026-06-05T11:00:00Z"),
			})

			if exitCode != exitSuccess {
				t.Fatalf("expected exit code %d, got %d: %s", exitSuccess, exitCode, stderr.String())
			}
			if stderr.Len() != 0 {
				t.Fatalf("expected empty stderr, got %q", stderr.String())
			}
			if strings.Contains(strings.Join(args, " "), "--json") {
				var decoded verify.Report
				if err := json.Unmarshal(stdout.Bytes(), &decoded); err != nil {
					t.Fatalf("decode verify JSON: %v\n%s", err, stdout.String())
				}
				if !decoded.OK || decoded.Summary.Passed != 1 {
					t.Fatalf("unexpected verify JSON: %#v", decoded)
				}
				if decoded.Root != cwd {
					t.Fatalf("decoded verify root = %q, want %q", decoded.Root, cwd)
				}
				var snapshot verify.ReportSnapshot
				if err := json.Unmarshal(stdout.Bytes(), &snapshot); err != nil {
					t.Fatalf("decode verify snapshot JSON: %v\n%s", err, stdout.String())
				}
				if snapshot.Contract != verify.ReportContractVersion || len(snapshot.Events) == 0 {
					t.Fatalf("verify JSON did not expose runtime contract: %#v", snapshot)
				}
			} else if !strings.Contains(stdout.String(), "Zero verification") || !strings.Contains(stdout.String(), "go.test") || !strings.Contains(stdout.String(), cwd) || !strings.Contains(stdout.String(), "tests: 2 total, 1 passed, 1 failed") {
				t.Fatalf("unexpected verify text output: %q", stdout.String())
			}
		})
	}
}

func TestRunVerifyRedactsWorkspacePathsInOutput(t *testing.T) {
	secret := "sk-proj-abcdefghijklmnopqrstuvwxyz0"
	cwd := filepath.Join(t.TempDir(), secret, "workspace")
	if err := os.MkdirAll(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	plan := verify.Plan{Root: cwd}
	report := verify.Report{
		Root:      cwd,
		StartedAt: "2026-06-05T11:05:00Z",
		EndedAt:   "2026-06-05T11:05:01Z",
		OK:        true,
		Summary:   verify.Summary{},
		Results: []verify.Result{{
			ID:            "go.test",
			Name:          "Go tests",
			Command:       []string{"go", "test", "./..."},
			Status:        verify.StatusFail,
			OutputSummary: &verify.OutputSummary{Lines: []string{"failure at " + secret}},
			TestSummary: &testrunner.Summary{
				Framework: testrunner.FrameworkGo,
				Total:     1,
				Failed:    1,
				Failures: []testrunner.Failure{{
					Name:    secret,
					File:    filepath.Join(secret, "secret_test.go:12"),
					Message: "token " + secret,
				}},
			},
		}},
	}

	for _, args := range [][]string{
		{"verify"},
		{"verify", "--json"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var stdout bytes.Buffer
			var stderr bytes.Buffer
			exitCode := runWithDeps(args, &stdout, &stderr, appDeps{
				getwd: func() (string, error) { return cwd, nil },
				detectVerifyPlan: func(root string) (verify.Plan, error) {
					if root != cwd {
						t.Fatalf("verify root = %q, want %q", root, cwd)
					}
					return plan, nil
				},
				runVerify: func(context.Context, verify.Plan, verify.RunOptions) verify.Report {
					return report
				},
			})

			if exitCode != exitSuccess {
				t.Fatalf("expected exit code %d, got %d: %s", exitSuccess, exitCode, stderr.String())
			}
			if strings.Contains(stdout.String(), secret) {
				t.Fatalf("verify output leaked secret path segment: %q", stdout.String())
			}
			if !strings.Contains(stdout.String(), redaction.RedactedSecret) {
				t.Fatalf("expected redaction marker in verify output, got %q", stdout.String())
			}
		})
	}
}

func TestRunVerifyReturnsProviderExitWhenChecksFail(t *testing.T) {
	cwd := t.TempDir()
	report := verify.Report{
		Root:    cwd,
		OK:      false,
		Summary: verify.Summary{Total: 1, Failed: 1},
		Results: []verify.Result{{
			ID:       "bun.test",
			Name:     "Bun tests",
			Command:  []string{"bun", "test"},
			Status:   verify.StatusFail,
			ExitCode: 1,
		}},
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := runWithDeps([]string{"verify"}, &stdout, &stderr, appDeps{
		getwd: func() (string, error) { return cwd, nil },
		detectVerifyPlan: func(string) (verify.Plan, error) {
			return verify.Plan{Root: cwd, Checks: []verify.Check{{ID: "bun.test", Name: "Bun tests", Command: []string{"bun", "test"}}}}, nil
		},
		runVerify: func(context.Context, verify.Plan, verify.RunOptions) verify.Report { return report },
	})

	if exitCode != exitProvider {
		t.Fatalf("expected provider-style failure exit %d, got %d", exitProvider, exitCode)
	}
	if !strings.Contains(stdout.String(), "failed") {
		t.Fatalf("expected failure summary in stdout, got %q", stdout.String())
	}
}

func TestRunVerifyAttemptsUsesSelfVerifyLoop(t *testing.T) {
	cwd := t.TempDir()
	plan := verify.Plan{Root: cwd, Checks: []verify.Check{{ID: "go.test", Name: "Go tests", Command: []string{"go", "test", "./..."}}}}
	loopReport := selfverify.Report{
		Root:       cwd,
		OK:         true,
		StopReason: selfverify.StopReasonPassed,
		Attempts: []selfverify.Attempt{
			{Number: 1, Report: verify.Report{Root: cwd, OK: false, Summary: verify.Summary{Total: 1, Failed: 1}}},
			{Number: 2, Report: verify.Report{Root: cwd, OK: true, Summary: verify.Summary{Total: 1, Passed: 1}}},
		},
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := runWithDeps([]string{"verify", "--attempts", "2", "--json"}, &stdout, &stderr, appDeps{
		getwd: func() (string, error) { return cwd, nil },
		detectVerifyPlan: func(root string) (verify.Plan, error) {
			if root != cwd {
				t.Fatalf("verify root = %q, want %q", root, cwd)
			}
			return plan, nil
		},
		runSelfVerify: func(ctx context.Context, gotPlan verify.Plan, options selfverify.Options) selfverify.Report {
			if options.MaxAttempts != 2 {
				t.Fatalf("MaxAttempts = %d, want 2", options.MaxAttempts)
			}
			return loopReport
		},
	})

	if exitCode != exitSuccess {
		t.Fatalf("expected exit code %d, got %d: %s", exitSuccess, exitCode, stderr.String())
	}
	var decoded selfverify.Report
	if err := json.Unmarshal(stdout.Bytes(), &decoded); err != nil {
		t.Fatalf("decode verify loop JSON: %v\n%s", err, stdout.String())
	}
	if len(decoded.Attempts) != 2 || !decoded.OK || decoded.StopReason != selfverify.StopReasonPassed {
		t.Fatalf("unexpected loop JSON: %#v", decoded)
	}
	var snapshot selfverify.LoopSnapshot
	if err := json.Unmarshal(stdout.Bytes(), &snapshot); err != nil {
		t.Fatalf("decode verify loop snapshot JSON: %v\n%s", err, stdout.String())
	}
	if snapshot.Contract != selfverify.LoopContractVersion || len(snapshot.Events) == 0 {
		t.Fatalf("verify loop JSON did not expose runtime contract: %#v", snapshot)
	}
}

func TestRunVerifyAttemptsFormatsSelfVerifyText(t *testing.T) {
	cwd := t.TempDir()
	plan := verify.Plan{Root: cwd, Checks: []verify.Check{{ID: "go.test", Name: "Go tests", Command: []string{"go", "test", "./..."}}}}
	loopReport := selfverify.Report{
		Root:       cwd,
		OK:         true,
		StopReason: selfverify.StopReasonPassed,
		Summary:    verify.Summary{Total: 1, Passed: 1},
		Attempts: []selfverify.Attempt{
			{
				Number:      1,
				Report:      verify.Report{Root: cwd, OK: false, Summary: verify.Summary{Total: 1, Failed: 1}},
				Remediation: &selfverify.Remediation{Applied: true, Message: "prepared retry"},
			},
			{Number: 2, Report: verify.Report{Root: cwd, OK: true, Summary: verify.Summary{Total: 1, Passed: 1}}},
		},
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := runWithDeps([]string{"verify", "--attempts=2"}, &stdout, &stderr, appDeps{
		getwd: func() (string, error) { return cwd, nil },
		detectVerifyPlan: func(string) (verify.Plan, error) {
			return plan, nil
		},
		runSelfVerify: func(context.Context, verify.Plan, selfverify.Options) selfverify.Report {
			return loopReport
		},
	})

	if exitCode != exitSuccess {
		t.Fatalf("expected exit code %d, got %d: %s", exitSuccess, exitCode, stderr.String())
	}
	output := stdout.String()
	for _, want := range []string{"Zero self-verification", "root: " + cwd, "stop: passed", "attempt 1: failed", "remediation: applied - prepared retry", "attempt 2: passed"} {
		if !strings.Contains(output, want) {
			t.Fatalf("expected %q in output: %q", want, output)
		}
	}
}

func TestRunChangesInspectAndCommit(t *testing.T) {
	cwd := t.TempDir()
	summary := zerogit.ChangeSummary{
		Root:   cwd,
		Branch: "main",
		Commit: "abc1234",
		Files:  []zerogit.FileChange{{Path: "README.md", Status: "modified", Unstaged: true}},
	}
	commit := zerogit.CommitResult{
		Root:       cwd,
		Message:    "Update README",
		DryRun:     true,
		Committed:  false,
		Before:     summary,
		CommitHash: "",
	}

	t.Run("inspect json", func(t *testing.T) {
		var stdout bytes.Buffer
		var stderr bytes.Buffer
		exitCode := runWithDeps([]string{"changes", "inspect", "--json"}, &stdout, &stderr, appDeps{
			getwd: func() (string, error) { return cwd, nil },
			inspectChanges: func(ctx context.Context, options zerogit.InspectOptions) (zerogit.ChangeSummary, error) {
				if options.Cwd != cwd {
					t.Fatalf("inspect cwd = %q, want %q", options.Cwd, cwd)
				}
				return summary, nil
			},
		})

		if exitCode != exitSuccess {
			t.Fatalf("expected exit code %d, got %d: %s", exitSuccess, exitCode, stderr.String())
		}
		var decoded zerogit.ChangeSummary
		if err := json.Unmarshal(stdout.Bytes(), &decoded); err != nil {
			t.Fatalf("decode changes JSON: %v\n%s", err, stdout.String())
		}
		if len(decoded.Files) != 1 || decoded.Files[0].Path != "README.md" {
			t.Fatalf("unexpected changes JSON: %#v", decoded)
		}
		var snapshot zerogit.ChangeSnapshot
		if err := json.Unmarshal(stdout.Bytes(), &snapshot); err != nil {
			t.Fatalf("decode changes snapshot JSON: %v\n%s", err, stdout.String())
		}
		if snapshot.Contract != zerogit.ChangeContractVersion || len(snapshot.Events) == 0 {
			t.Fatalf("changes JSON did not expose runtime contract: %#v", snapshot)
		}
	})

	t.Run("commit dry-run", func(t *testing.T) {
		var stdout bytes.Buffer
		var stderr bytes.Buffer
		exitCode := runWithDeps([]string{"changes", "commit", "--message", "Update README", "--dry-run"}, &stdout, &stderr, appDeps{
			getwd: func() (string, error) { return cwd, nil },
			commitChanges: func(ctx context.Context, options zerogit.CommitOptions) (zerogit.CommitResult, error) {
				if options.Cwd != cwd || options.Message != "Update README" || !options.DryRun {
					t.Fatalf("unexpected commit options: %#v", options)
				}
				return commit, nil
			},
		})

		if exitCode != exitSuccess {
			t.Fatalf("expected exit code %d, got %d: %s", exitSuccess, exitCode, stderr.String())
		}
		if !strings.Contains(stdout.String(), "Zero changes commit") || !strings.Contains(stdout.String(), "dry-run: true") {
			t.Fatalf("unexpected changes commit output: %q", stdout.String())
		}
	})
}

func TestRunChangesInspectThreadsBaseRef(t *testing.T) {
	cwd := t.TempDir()
	summary := zerogit.ChangeSummary{
		Root:   cwd,
		Branch: "feature",
		Base:   "main",
		Files:  []zerogit.FileChange{{Path: "feature.md", Status: "added"}},
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := runWithDeps([]string{"changes", "inspect", "--base", "main"}, &stdout, &stderr, appDeps{
		getwd: func() (string, error) { return cwd, nil },
		inspectChanges: func(ctx context.Context, options zerogit.InspectOptions) (zerogit.ChangeSummary, error) {
			if options.BaseRef != "main" {
				t.Fatalf("InspectOptions.BaseRef = %q, want main", options.BaseRef)
			}
			return summary, nil
		},
	})

	if exitCode != exitSuccess {
		t.Fatalf("expected exit code %d, got %d: %s", exitSuccess, exitCode, stderr.String())
	}
	if !strings.Contains(stdout.String(), "base: main") {
		t.Fatalf("expected base line in output, got %q", stdout.String())
	}
}

func TestRunChangesCommitRejectsBaseRef(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := runWithDeps([]string{"changes", "commit", "--base", "main"}, &stdout, &stderr, appDeps{
		getwd: func() (string, error) { return t.TempDir(), nil },
		commitChanges: func(context.Context, zerogit.CommitOptions) (zerogit.CommitResult, error) {
			t.Fatal("commitChanges should not be called when --base is rejected")
			return zerogit.CommitResult{}, nil
		},
	})

	if exitCode != exitUsage {
		t.Fatalf("expected usage exit %d, got %d", exitUsage, exitCode)
	}
	if !strings.Contains(stderr.String(), "--base") {
		t.Fatalf("expected --base rejection error, got %q", stderr.String())
	}
}

func TestWriteChangesHelpMentionsBase(t *testing.T) {
	var out bytes.Buffer
	if err := writeChangesHelp(&out); err != nil {
		t.Fatalf("writeChangesHelp error: %v", err)
	}
	if !strings.Contains(out.String(), "--base") {
		t.Fatalf("expected --base in changes help, got %q", out.String())
	}
}

func TestParseChangesArgsBaseRef(t *testing.T) {
	for _, args := range [][]string{
		{"--base", "main"},
		{"--base=main"},
	} {
		options, help, err := parseChangesArgs(args, "inspect")
		if err != nil {
			t.Fatalf("parseChangesArgs(%v) error: %v", args, err)
		}
		if help {
			t.Fatalf("parseChangesArgs(%v) returned help", args)
		}
		if options.baseRef != "main" {
			t.Fatalf("baseRef = %q, want main (args %v)", options.baseRef, args)
		}
	}
}

func TestParseChangesArgsRejectsBaseOnCommit(t *testing.T) {
	_, _, err := parseChangesArgs([]string{"--base", "main"}, "commit")
	if err == nil || !strings.Contains(err.Error(), "--base") {
		t.Fatalf("expected --base rejection on commit, got %v", err)
	}
}

func TestParseChangesArgsRequiresBaseValue(t *testing.T) {
	if _, _, err := parseChangesArgs([]string{"--base"}, "inspect"); err == nil {
		t.Fatalf("expected error when --base has no value")
	}
}

func TestRunExecWorktreeUsesPreparedWorkspace(t *testing.T) {
	root := t.TempDir()
	worktreeDir := t.TempDir()
	var resolvedWorkspace string

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := runWithDeps([]string{"exec", "--worktree", "task-a", "hello"}, &stdout, &stderr, appDeps{
		getwd: func() (string, error) { return root, nil },
		prepareWorktree: func(ctx context.Context, options worktrees.Options) (worktrees.Result, error) {
			if options.Cwd != root || options.Name != "task-a" {
				t.Fatalf("unexpected worktree options: %#v", options)
			}
			return worktrees.Result{Name: "task-a", Path: worktreeDir, RepoRoot: root, SourceBranch: "main", SourceCommit: "abc1234"}, nil
		},
		resolveConfig: func(workspaceRoot string, overrides config.Overrides) (config.ResolvedConfig, error) {
			resolvedWorkspace = workspaceRoot
			return execResolvedConfig(), nil
		},
		newProvider: func(config.ProviderProfile) (zeroruntime.Provider, error) {
			return echoExecProvider{}, nil
		},
	})

	if exitCode != exitSuccess {
		t.Fatalf("expected exit code %d, got %d: %s", exitSuccess, exitCode, stderr.String())
	}
	if resolvedWorkspace != worktreeDir {
		t.Fatalf("resolved workspace = %q, want worktree %q", resolvedWorkspace, worktreeDir)
	}
	if !strings.Contains(stdout.String(), "hello") {
		t.Fatalf("expected provider output, got %q", stdout.String())
	}
}

func TestRunExecWorktreeReleasesLockAfterRun(t *testing.T) {
	root := t.TempDir()
	worktreeDir := t.TempDir()
	var releasedPath string
	releaseCalls := 0

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := runWithDeps([]string{"exec", "--worktree", "task-a", "hello"}, &stdout, &stderr, appDeps{
		getwd: func() (string, error) { return root, nil },
		prepareWorktree: func(ctx context.Context, options worktrees.Options) (worktrees.Result, error) {
			return worktrees.Result{Name: "task-a", Path: worktreeDir, RepoRoot: root, SourceBranch: "main", SourceCommit: "abc1234", LockAcquired: true}, nil
		},
		releaseWorktree: func(ctx context.Context, options worktrees.Options, path string) error {
			releaseCalls++
			releasedPath = path
			return nil
		},
		resolveConfig: func(workspaceRoot string, overrides config.Overrides) (config.ResolvedConfig, error) {
			return execResolvedConfig(), nil
		},
		newProvider: func(config.ProviderProfile) (zeroruntime.Provider, error) {
			return echoExecProvider{}, nil
		},
	})

	if exitCode != exitSuccess {
		t.Fatalf("expected exit code %d, got %d: %s", exitSuccess, exitCode, stderr.String())
	}
	// A `zero exec --worktree` run is the only user of the worktree it prepares:
	// its own process created it and is now exiting, so it must release the
	// Prepare lock itself rather than leaving it locked forever (unlike `zero
	// worktrees prepare`, which hands the path to a longer-lived external
	// caller and has no such end-of-life signal to act on).
	if releaseCalls != 1 {
		t.Fatalf("releaseWorktree call count = %d, want 1", releaseCalls)
	}
	if releasedPath != worktreeDir {
		t.Fatalf("released path = %q, want %q", releasedPath, worktreeDir)
	}
}

func TestRunExecWorktreeKeepsLockItDidNotAcquire(t *testing.T) {
	// Defense in depth: Prepare now rejects an in-use lease outright, so a
	// successful result always reports LockAcquired=true. Should that ever
	// change, exec must still only release the ownership its own invocation
	// established; releasing a lock it did not acquire would clear another
	// caller's lease and let a later Clean force-delete a live workspace.
	root := t.TempDir()
	worktreeDir := t.TempDir()
	releaseCalls := 0

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := runWithDeps([]string{"exec", "--worktree", "task-a", "hello"}, &stdout, &stderr, appDeps{
		getwd: func() (string, error) { return root, nil },
		prepareWorktree: func(ctx context.Context, options worktrees.Options) (worktrees.Result, error) {
			return worktrees.Result{Name: "task-a", Path: worktreeDir, RepoRoot: root, SourceBranch: "main", SourceCommit: "abc1234", Reused: true, LockAcquired: false}, nil
		},
		releaseWorktree: func(ctx context.Context, options worktrees.Options, path string) error {
			releaseCalls++
			return nil
		},
		resolveConfig: func(workspaceRoot string, overrides config.Overrides) (config.ResolvedConfig, error) {
			return execResolvedConfig(), nil
		},
		newProvider: func(config.ProviderProfile) (zeroruntime.Provider, error) {
			return echoExecProvider{}, nil
		},
	})

	if exitCode != exitSuccess {
		t.Fatalf("expected exit code %d, got %d: %s", exitSuccess, exitCode, stderr.String())
	}
	if releaseCalls != 0 {
		t.Fatalf("releaseWorktree call count = %d, want 0 for a lock this run did not acquire", releaseCalls)
	}
}

func TestRunExecWorktreeSurfacesReleaseFailure(t *testing.T) {
	// A failed unlock leaves a lock Clean permanently skips, recreating the
	// disk leak silently; the failure must reach the user with the affected
	// path so the leaked lock can be cleared by hand.
	root := t.TempDir()
	worktreeDir := t.TempDir()

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := runWithDeps([]string{"exec", "--worktree", "task-a", "hello"}, &stdout, &stderr, appDeps{
		getwd: func() (string, error) { return root, nil },
		prepareWorktree: func(ctx context.Context, options worktrees.Options) (worktrees.Result, error) {
			return worktrees.Result{Name: "task-a", Path: worktreeDir, RepoRoot: root, SourceBranch: "main", SourceCommit: "abc1234", LockAcquired: true}, nil
		},
		releaseWorktree: func(ctx context.Context, options worktrees.Options, path string) error {
			return errors.New("unlock git worktree: boom")
		},
		resolveConfig: func(workspaceRoot string, overrides config.Overrides) (config.ResolvedConfig, error) {
			return execResolvedConfig(), nil
		},
		newProvider: func(config.ProviderProfile) (zeroruntime.Provider, error) {
			return echoExecProvider{}, nil
		},
	})

	if exitCode != exitSuccess {
		t.Fatalf("expected exit code %d, got %d: %s", exitSuccess, exitCode, stderr.String())
	}
	if !strings.Contains(stderr.String(), worktreeDir) || !strings.Contains(stderr.String(), "boom") {
		t.Fatalf("expected release failure with path on stderr, got %q", stderr.String())
	}
}

func TestRunExecWorktreeRedactsReleaseFailureText(t *testing.T) {
	// The release path argument was already redacted before this diagnostic
	// was added, but the error's own text was forwarded verbatim; Release's
	// ownership errors interpolate the caller-supplied path (see
	// verifyZeroOwnedWorktree), so a key-shaped path reaching this message
	// leaked unredacted onto stderr.
	root := t.TempDir()
	worktreeDir := t.TempDir()
	secret := "sk-proj-abcDEF123_ghiJKL456-mnoPQR789stu"

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := runWithDeps([]string{"exec", "--worktree", "task-a", "hello"}, &stdout, &stderr, appDeps{
		getwd: func() (string, error) { return root, nil },
		prepareWorktree: func(ctx context.Context, options worktrees.Options) (worktrees.Result, error) {
			return worktrees.Result{Name: "task-a", Path: worktreeDir, RepoRoot: root, SourceBranch: "main", SourceCommit: "abc1234", LockAcquired: true}, nil
		},
		releaseWorktree: func(ctx context.Context, options worktrees.Options, path string) error {
			return fmt.Errorf("refusing to release /tmp/%s/task: not a registered worktree of this repository", secret)
		},
		resolveConfig: func(workspaceRoot string, overrides config.Overrides) (config.ResolvedConfig, error) {
			return execResolvedConfig(), nil
		},
		newProvider: func(config.ProviderProfile) (zeroruntime.Provider, error) {
			return echoExecProvider{}, nil
		},
	})

	if exitCode != exitSuccess {
		t.Fatalf("expected exit code %d, got %d: %s", exitSuccess, exitCode, stderr.String())
	}
	if strings.Contains(stderr.String(), secret) {
		t.Fatalf("deferred release diagnostic leaked unredacted key-shaped path: %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "[REDACTED]") {
		t.Fatalf("expected redaction placeholder in deferred release diagnostic, got %q", stderr.String())
	}
}

func TestRunExecRejectsForkWithWorktree(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := runWithDeps([]string{"exec", "--worktree", "--fork", "zero_parent", "hello"}, &stdout, &stderr, appDeps{
		prepareWorktree: func(context.Context, worktrees.Options) (worktrees.Result, error) {
			t.Fatal("prepareWorktree should not be called for invalid flags")
			return worktrees.Result{}, nil
		},
	})

	if exitCode != exitUsage {
		t.Fatalf("expected usage exit %d, got %d", exitUsage, exitCode)
	}
	if !strings.Contains(stderr.String(), "--fork cannot be used with --worktree") {
		t.Fatalf("expected flag conflict error, got %q", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("expected empty stdout, got %q", stdout.String())
	}
}

func TestRunExecRejectsWorktreeDirWithoutWorktree(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := runWithDeps([]string{"exec", "--worktree-dir", "/tmp/zero", "hello"}, &stdout, &stderr, appDeps{
		prepareWorktree: func(context.Context, worktrees.Options) (worktrees.Result, error) {
			t.Fatal("prepareWorktree should not be called for invalid flags")
			return worktrees.Result{}, nil
		},
	})

	if exitCode != exitUsage {
		t.Fatalf("expected usage exit %d, got %d", exitUsage, exitCode)
	}
	if !strings.Contains(stderr.String(), "--worktree-dir requires --worktree") {
		t.Fatalf("expected worktree-dir dependency error, got %q", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("expected empty stdout, got %q", stdout.String())
	}
}

func TestParseChangesArgsAuto(t *testing.T) {
	// 1. Correct parsing of auto on commit, push, and pr
	for _, cmd := range []string{"commit", "push", "pr"} {
		for _, args := range [][]string{
			{"--auto"},
			{"-a"},
		} {
			options, help, err := parseChangesArgs(args, cmd)
			if err != nil {
				t.Fatalf("parseChangesArgs(%v, %q) error: %v", args, cmd, err)
			}
			if help {
				t.Fatalf("parseChangesArgs(%v, %q) returned help", args, cmd)
			}
			if !options.auto {
				t.Fatalf("auto = false, want true (args %v, cmd %q)", args, cmd)
			}
		}
	}

	// 2. Reject --auto on inspect
	_, _, err := parseChangesArgs([]string{"--auto"}, "inspect")
	if err == nil || !strings.Contains(err.Error(), "--auto is only valid with commit, push, or pr") {
		t.Fatalf("expected --auto rejection on inspect, got %v", err)
	}

	// 3. Reject both --message and --auto on commit
	_, _, err = parseChangesArgs([]string{"--message", "foo", "--auto"}, "commit")
	if err == nil || !strings.Contains(err.Error(), "cannot specify both --message and --auto") {
		t.Fatalf("expected --message and --auto conflict error, got %v", err)
	}

	// 4. Reject both empty message and --auto on commit
	_, _, err = parseChangesArgs([]string{"--message=", "--auto"}, "commit")
	if err == nil || !strings.Contains(err.Error(), "cannot specify both --message and --auto") {
		t.Fatalf("expected empty --message and --auto conflict error, got %v", err)
	}

	// 5. Dry-run allowed on commit and push, rejected on pr and inspect
	for _, cmd := range []string{"commit", "push"} {
		opts, _, err := parseChangesArgs([]string{"--dry-run"}, cmd)
		if err != nil || !opts.dryRun {
			t.Fatalf("expected --dry-run accepted on %q, got opts=%+v err=%v", cmd, opts, err)
		}
	}
	for _, cmd := range []string{"pr", "inspect"} {
		_, _, err := parseChangesArgs([]string{"--dry-run"}, cmd)
		if err == nil || !strings.Contains(err.Error(), "--dry-run is only valid with commit or push") {
			t.Fatalf("expected --dry-run rejected on %q, got %v", cmd, err)
		}
	}
}

type mockCommitMsgProvider struct {
	response string
	req      zeroruntime.CompletionRequest
	err      error
}

func (p *mockCommitMsgProvider) StreamCompletion(ctx context.Context, request zeroruntime.CompletionRequest) (<-chan zeroruntime.StreamEvent, error) {
	p.req = request
	if p.err != nil {
		return nil, p.err
	}
	events := make(chan zeroruntime.StreamEvent, 2)
	events <- zeroruntime.StreamEvent{Type: zeroruntime.StreamEventText, Content: p.response}
	events <- zeroruntime.StreamEvent{Type: zeroruntime.StreamEventDone}
	close(events)
	return events, nil
}

func TestRunChangesCommitAuto(t *testing.T) {
	cwd := t.TempDir()
	summary := zerogit.ChangeSummary{
		Root:   cwd,
		Branch: "main",
		Files:  []zerogit.FileChange{{Path: "README.md", Status: "modified"}},
		Diff:   "some diff content with ghp_1234567890abcdefghijklmnopqrstuvwxyz",
	}

	mockProv := &mockCommitMsgProvider{
		response: "```\nfeat: auto commit message\n```",
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	commitCalled := false

	exitCode := runWithDeps([]string{"changes", "commit", "--auto"}, &stdout, &stderr, appDeps{
		getwd: func() (string, error) { return cwd, nil },
		inspectChanges: func(ctx context.Context, options zerogit.InspectOptions) (zerogit.ChangeSummary, error) {
			return summary, nil
		},
		resolveConfig: func(workspaceRoot string, overrides config.Overrides) (config.ResolvedConfig, error) {
			cfg := execResolvedConfig()
			cfg.Provider.Name = "openai"
			cfg.Provider.Model = "gpt-4o"
			return cfg, nil
		},
		newProvider: func(profile config.ProviderProfile) (zeroruntime.Provider, error) {
			return mockProv, nil
		},
		commitChanges: func(ctx context.Context, options zerogit.CommitOptions) (zerogit.CommitResult, error) {
			commitCalled = true
			if options.Message != "feat: auto commit message" {
				t.Fatalf("expected commit message 'feat: auto commit message', got %q", options.Message)
			}
			return zerogit.CommitResult{
				Root:       cwd,
				Message:    options.Message,
				Committed:  true,
				CommitHash: "abc1234",
				Before:     summary,
			}, nil
		},
	})

	if exitCode != exitSuccess {
		t.Fatalf("expected exit code %d, got %d: %s", exitSuccess, exitCode, stderr.String())
	}
	if !commitCalled {
		t.Fatal("expected commitChanges to be called")
	}
	// Verify that secret in the diff was redacted
	promptContent := mockProv.req.Messages[0].Content
	if strings.Contains(promptContent, "ghp_1234567890abcdefghijklmnopqrstuvwxyz") {
		t.Fatal("expected secret in diff to be redacted, but it was found in the prompt")
	}
	if !strings.Contains(promptContent, "[REDACTED]") && !strings.Contains(promptContent, "REDACTED") {
		t.Fatalf("expected redacted diff content in prompt, got: %q", promptContent)
	}
	if !strings.Contains(stdout.String(), "Generating commit message using LLM...") {
		t.Fatalf("expected status message in stdout, got %q", stdout.String())
	}

	t.Run("EmptyLLMResponse", func(t *testing.T) {
		mockProvEmpty := &mockCommitMsgProvider{
			response: "   \n\n   ",
		}
		var stdout, stderr bytes.Buffer
		code := runWithDeps([]string{"changes", "commit", "--auto"}, &stdout, &stderr, appDeps{
			getwd: func() (string, error) { return cwd, nil },
			inspectChanges: func(ctx context.Context, options zerogit.InspectOptions) (zerogit.ChangeSummary, error) {
				return summary, nil
			},
			resolveConfig: func(workspaceRoot string, overrides config.Overrides) (config.ResolvedConfig, error) {
				return execResolvedConfig(), nil
			},
			newProvider: func(profile config.ProviderProfile) (zeroruntime.Provider, error) {
				return mockProvEmpty, nil
			},
		})
		if code == exitSuccess {
			t.Fatal("expected command to fail when LLM returns empty response, got success")
		}
		if !strings.Contains(stderr.String(), "empty commit message") {
			t.Fatalf("expected empty commit message error in stderr, got %q", stderr.String())
		}
	})
}

func TestEnsureFeatureBranchCreatesBranchOffDefaultWithoutProvider(t *testing.T) {
	cwd := t.TempDir()
	var createdName string

	branch, _, _, err := ensureFeatureBranch(context.Background(), &bytes.Buffer{}, cwd, "", featureBranchOptions{}, appDeps{
		isDefaultBranch: func(ctx context.Context, options zerogit.DefaultBranchOptions) (bool, string, string, error) {
			return true, "main", "origin", nil
		},
		commitsAhead:       func(ctx context.Context, cwd, remote, branch string) (int, error) { return 1, nil },
		refreshTrackingRef: func(ctx context.Context, cwd, remote, branch string) error { return nil },
		inspectChanges:     featureBranchInspect([]zerogit.FileChange{{Path: "README.md", Status: "modified"}}, ""),
		resolveConfig: func(workspaceRoot string, overrides config.Overrides) (config.ResolvedConfig, error) {
			return config.ResolvedConfig{}, nil
		},
		currentGitUser: func(ctx context.Context, cwd string) string { return "Someone" },
		createBranch: func(ctx context.Context, options zerogit.BranchOptions) (zerogit.BranchResult, error) {
			createdName = options.Name
			return zerogit.BranchResult{Branch: options.Name}, nil
		},
	})
	if err != nil {
		t.Fatalf("ensureFeatureBranch returned error: %v", err)
	}
	if branch != "someone/readme-md" || createdName != "someone/readme-md" {
		t.Fatalf("unexpected branch: got %q (created %q)", branch, createdName)
	}
}

func TestEnsureFeatureBranchUsesLLMSlugWhenProviderConfigured(t *testing.T) {
	cwd := t.TempDir()
	mockProv := &mockCommitMsgProvider{response: "add login page"}
	var createdName string

	branch, _, _, err := ensureFeatureBranch(context.Background(), &bytes.Buffer{}, cwd, "", featureBranchOptions{AutoNaming: true}, appDeps{
		isDefaultBranch: func(ctx context.Context, options zerogit.DefaultBranchOptions) (bool, string, string, error) {
			return true, "main", "origin", nil
		},
		commitsAhead:   func(ctx context.Context, cwd, remote, branch string) (int, error) { return 1, nil },
		inspectChanges: featureBranchInspect([]zerogit.FileChange{{Path: "login.go", Status: "added"}}, "+func Login() {}"),
		resolveConfig: func(workspaceRoot string, overrides config.Overrides) (config.ResolvedConfig, error) {
			return execResolvedConfig(), nil
		},
		newProvider: func(profile config.ProviderProfile) (zeroruntime.Provider, error) {
			return mockProv, nil
		},
		currentGitUser: func(ctx context.Context, cwd string) string { return "Someone" },
		createBranch: func(ctx context.Context, options zerogit.BranchOptions) (zerogit.BranchResult, error) {
			createdName = options.Name
			return zerogit.BranchResult{Branch: options.Name}, nil
		},
	})
	if err != nil {
		t.Fatalf("ensureFeatureBranch returned error: %v", err)
	}
	if branch != "someone/add-login-page" || createdName != "someone/add-login-page" {
		t.Fatalf("unexpected branch: got %q (created %q)", branch, createdName)
	}
}

func TestEnsureFeatureBranchNormalizesMessyLLMSlugResponse(t *testing.T) {
	cwd := t.TempDir()
	// The prompt asks the model for "ONLY the raw slug text," but models don't
	// always comply: this response wraps the actual slug in quotes with a
	// blank line first. Slugifying the whole response verbatim would fold that
	// noise into the branch name instead of just "add-login-page".
	mockProv := &mockCommitMsgProvider{response: "\n\"add login page\"\n"}
	var createdName string

	branch, _, _, err := ensureFeatureBranch(context.Background(), &bytes.Buffer{}, cwd, "", featureBranchOptions{AutoNaming: true}, appDeps{
		isDefaultBranch: func(ctx context.Context, options zerogit.DefaultBranchOptions) (bool, string, string, error) {
			return true, "main", "origin", nil
		},
		commitsAhead:   func(ctx context.Context, cwd, remote, branch string) (int, error) { return 1, nil },
		inspectChanges: featureBranchInspect([]zerogit.FileChange{{Path: "login.go", Status: "added"}}, "+func Login() {}"),
		resolveConfig: func(workspaceRoot string, overrides config.Overrides) (config.ResolvedConfig, error) {
			return execResolvedConfig(), nil
		},
		newProvider: func(profile config.ProviderProfile) (zeroruntime.Provider, error) {
			return mockProv, nil
		},
		currentGitUser: func(ctx context.Context, cwd string) string { return "Someone" },
		createBranch: func(ctx context.Context, options zerogit.BranchOptions) (zerogit.BranchResult, error) {
			createdName = options.Name
			return zerogit.BranchResult{Branch: options.Name}, nil
		},
	})
	if err != nil {
		t.Fatalf("ensureFeatureBranch returned error: %v", err)
	}
	if branch != "someone/add-login-page" || createdName != "someone/add-login-page" {
		t.Fatalf("unexpected branch: got %q (created %q)", branch, createdName)
	}
}

func TestExtractBranchSlug(t *testing.T) {
	// The prompt asks for "ONLY the raw slug", but models add a preamble line,
	// wrap the answer in a code fence, or quote a multi-word phrase. The real
	// slug must be recovered rather than slugified whole (which would turn a
	// preamble into the branch name) or dropped (a bare fence line slugifies to
	// nothing).
	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{"RawSlug", "add-login-page", "add-login-page"},
		{"Preamble", "Here is a suggested branch name:\nadd-login-page", "add-login-page"},
		{"PreambleWithMultiWord", "Here is a suggested branch name:\nadd login page", "add login page"},
		{"PreambleInlineWithColon", "Here is a suggested branch name: add login page", "add login page"},
		{"InlineLabeledSlug", "Branch name: add-login-page", "add-login-page"},
		// One-word acknowledgements match the slug regex; preamble filter must
		// run before the early return so the real suggestion on the next line wins.
		{"OneWordAckThenSpaced", "Sure\nadd login page", "add login page"},
		{"OneWordAckThenKebab", "Certainly\nadd-login-page", "add-login-page"},
		{"CodeFence", "```\nadd-login-page\n```", "add-login-page"},
		{"FencedWithLanguage", "```text\nadd-login-page\n```", "add-login-page"},
		{"QuotedPhrase", "\n\"add login page\"\n", "add login page"},
		// Trailing sentence punctuation must not discard the suggestion as preamble.
		{"TrailingPeriodSpaced", "add login page.", "add login page"},
		{"TrailingPeriodKebab", "add-login-page.", "add-login-page"},
		{"EmptyResponse", "   \n\n  ", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractBranchSlug(tc.in); got != tc.want {
				t.Fatalf("extractBranchSlug(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestFallbackBranchSlug(t *testing.T) {
	// Git paths are slash-separated on every platform; the filename must be
	// extracted with path.Base semantics so Windows cannot mangle a path.
	for _, tc := range []struct {
		name  string
		files []zerogit.FileChange
		want  string
	}{
		{"NoFiles", nil, "changes"},
		{"RootFile", []zerogit.FileChange{{Path: "README.md", Status: "modified"}}, "readme-md"},
		{"SlashSeparatedPath", []zerogit.FileChange{{Path: "docs/guide.md", Status: "modified"}}, "guide-md"},
		{"DeepGitStylePath", []zerogit.FileChange{{Path: "internal/cli/workflows.go", Status: "modified"}}, "workflows-go"},
		{"MultipleFiles", []zerogit.FileChange{{Path: "a.md", Status: "added"}, {Path: "b.md", Status: "added"}}, "update-2-files"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := fallbackBranchSlug(zerogit.ChangeSummary{Files: tc.files}); got != tc.want {
				t.Fatalf("fallbackBranchSlug(%#v) = %q, want %q", tc.files, got, tc.want)
			}
		})
	}
}

func TestEnsureFeatureBranchExtractsSlugFromMessyLLMReplies(t *testing.T) {
	// End-to-end: a preamble line or a code fence around the slug must still
	// yield the intended branch name, not one derived from the preamble or a
	// silent fallback when the fence line slugifies to nothing.
	for _, tc := range []struct {
		name     string
		response string
	}{
		{"Preamble", "Here is a suggested branch name:\nadd-login-page"},
		{"PreambleWithMultiWord", "Here is a suggested branch name:\nadd login page"},
		{"CodeFence", "```\nadd-login-page\n```"},
		{"FencedWithLanguage", "```text\nadd-login-page\n```"},
		{"TrailingPeriod", "add login page."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cwd := t.TempDir()
			mockProv := &mockCommitMsgProvider{response: tc.response}
			var createdName string

			branch, _, _, err := ensureFeatureBranch(context.Background(), &bytes.Buffer{}, cwd, "", featureBranchOptions{AutoNaming: true}, appDeps{
				isDefaultBranch: func(ctx context.Context, options zerogit.DefaultBranchOptions) (bool, string, string, error) {
					return true, "main", "origin", nil
				},
				commitsAhead:   func(ctx context.Context, cwd, remote, branch string) (int, error) { return 1, nil },
				inspectChanges: featureBranchInspect([]zerogit.FileChange{{Path: "login.go", Status: "added"}}, "+func Login() {}"),
				resolveConfig: func(workspaceRoot string, overrides config.Overrides) (config.ResolvedConfig, error) {
					return execResolvedConfig(), nil
				},
				newProvider: func(profile config.ProviderProfile) (zeroruntime.Provider, error) {
					return mockProv, nil
				},
				currentGitUser: func(ctx context.Context, cwd string) string { return "Someone" },
				createBranch: func(ctx context.Context, options zerogit.BranchOptions) (zerogit.BranchResult, error) {
					createdName = options.Name
					return zerogit.BranchResult{Branch: options.Name}, nil
				},
			})
			if err != nil {
				t.Fatalf("ensureFeatureBranch returned error: %v", err)
			}
			if branch != "someone/add-login-page" || createdName != "someone/add-login-page" {
				t.Fatalf("unexpected branch: got %q (created %q)", branch, createdName)
			}
		})
	}
}

// TestEnsureFeatureBranchFallsBackWhenLLMNamingFails pins the silent-to-notice
// path: provider errors and empty generated slugs keep the deterministic name
// and print a short notice (unless JSON mode).
func TestEnsureFeatureBranchFallsBackWhenLLMNamingFails(t *testing.T) {
	for _, tc := range []struct {
		name     string
		provider zeroruntime.Provider
		provErr  error
	}{
		{"ProviderError", &mockCommitMsgProvider{err: errors.New("provider down")}, nil},
		{"EmptySlug", &mockCommitMsgProvider{response: "   \n\n  "}, nil},
		{"NewProviderFails", nil, errors.New("cannot construct provider")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cwd := t.TempDir()
			var createdName string
			var stdout bytes.Buffer

			branch, _, _, err := ensureFeatureBranch(context.Background(), &stdout, cwd, "", featureBranchOptions{AutoNaming: true}, appDeps{
				isDefaultBranch: func(ctx context.Context, options zerogit.DefaultBranchOptions) (bool, string, string, error) {
					return true, "main", "origin", nil
				},
				commitsAhead:   func(ctx context.Context, cwd, remote, branch string) (int, error) { return 1, nil },
				inspectChanges: featureBranchInspect([]zerogit.FileChange{{Path: "login.go", Status: "added"}}, "+func Login() {}"),
				resolveConfig: func(workspaceRoot string, overrides config.Overrides) (config.ResolvedConfig, error) {
					return execResolvedConfig(), nil
				},
				newProvider: func(profile config.ProviderProfile) (zeroruntime.Provider, error) {
					if tc.provErr != nil {
						return nil, tc.provErr
					}
					return tc.provider, nil
				},
				currentGitUser: func(ctx context.Context, cwd string) string { return "Someone" },
				createBranch: func(ctx context.Context, options zerogit.BranchOptions) (zerogit.BranchResult, error) {
					createdName = options.Name
					return zerogit.BranchResult{Branch: options.Name}, nil
				},
			})
			if err != nil {
				t.Fatalf("ensureFeatureBranch returned error: %v", err)
			}
			if branch != "someone/login-go" || createdName != "someone/login-go" {
				t.Fatalf("expected deterministic fallback someone/login-go, got %q (created %q)", branch, createdName)
			}
			if !strings.Contains(stdout.String(), "LLM branch naming unavailable; using deterministic name.") {
				t.Fatalf("expected fallback notice in stdout, got %q", stdout.String())
			}
		})
	}

	t.Run("JSONModeSuppressesNotice", func(t *testing.T) {
		cwd := t.TempDir()
		var stdout bytes.Buffer
		branch, _, _, err := ensureFeatureBranch(context.Background(), &stdout, cwd, "", featureBranchOptions{JSONMode: true, AutoNaming: true}, appDeps{
			isDefaultBranch: func(ctx context.Context, options zerogit.DefaultBranchOptions) (bool, string, string, error) {
				return true, "main", "origin", nil
			},
			commitsAhead:   func(ctx context.Context, cwd, remote, branch string) (int, error) { return 1, nil },
			inspectChanges: featureBranchInspect([]zerogit.FileChange{{Path: "login.go", Status: "added"}}, "+func Login() {}"),
			resolveConfig: func(workspaceRoot string, overrides config.Overrides) (config.ResolvedConfig, error) {
				return execResolvedConfig(), nil
			},
			newProvider: func(profile config.ProviderProfile) (zeroruntime.Provider, error) {
				return &mockCommitMsgProvider{err: errors.New("provider down")}, nil
			},
			currentGitUser: func(ctx context.Context, cwd string) string { return "Someone" },
			createBranch: func(ctx context.Context, options zerogit.BranchOptions) (zerogit.BranchResult, error) {
				return zerogit.BranchResult{Branch: options.Name}, nil
			},
		})
		if err != nil {
			t.Fatalf("ensureFeatureBranch returned error: %v", err)
		}
		if branch != "someone/login-go" {
			t.Fatalf("expected deterministic fallback, got %q", branch)
		}
		if strings.Contains(stdout.String(), "LLM branch naming unavailable") || strings.Contains(stdout.String(), "Generating branch name") {
			t.Fatalf("json mode must suppress naming notices, got %q", stdout.String())
		}
	})
}

func TestEnsureFeatureBranchSkipsWhenNotOnDefault(t *testing.T) {
	cwd := t.TempDir()
	createBranchCalled := false

	branch, _, created, err := ensureFeatureBranch(context.Background(), &bytes.Buffer{}, cwd, "", featureBranchOptions{}, appDeps{
		isDefaultBranch: func(ctx context.Context, options zerogit.DefaultBranchOptions) (bool, string, string, error) {
			return false, "feat/existing", "origin", nil
		},
		remoteHasBranch: func(ctx context.Context, cwd, remote, branch string) (bool, error) {
			// An ordinary, already-published feature branch: no lease needed.
			return true, nil
		},
		createBranch: func(ctx context.Context, options zerogit.BranchOptions) (zerogit.BranchResult, error) {
			createBranchCalled = true
			return zerogit.BranchResult{}, nil
		},
	})
	if err != nil {
		t.Fatalf("ensureFeatureBranch returned error: %v", err)
	}
	if branch != "feat/existing" {
		t.Fatalf("expected existing branch to be returned unchanged, got %q", branch)
	}
	if created {
		t.Fatal("expected an already-published branch not to require the nonexistence lease")
	}
	if createBranchCalled {
		t.Fatal("expected createBranch not to be called when already off the default branch")
	}
}

func TestEnsureFeatureBranchSkipsWhenAllowDefaultOrDryRun(t *testing.T) {
	for _, tc := range []struct {
		name               string
		allowDefaultBranch bool
		dryRun             bool
	}{
		{"AllowDefaultBranch", true, false},
		{"DryRun", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cwd := t.TempDir()
			branch, _, _, err := ensureFeatureBranch(context.Background(), &bytes.Buffer{}, cwd, "", featureBranchOptions{AllowDefaultBranch: tc.allowDefaultBranch, DryRun: tc.dryRun}, appDeps{
				isDefaultBranch: func(ctx context.Context, options zerogit.DefaultBranchOptions) (bool, string, string, error) {
					t.Fatal("isDefaultBranch should not be called")
					return false, "", "", nil
				},
			})
			if err != nil {
				t.Fatalf("ensureFeatureBranch returned error: %v", err)
			}
			if branch != "" {
				t.Fatalf("expected empty branch (defer to current HEAD), got %q", branch)
			}
		})
	}
}

func TestEnsureFeatureBranchNamesFromHeadCommitAfterCommit(t *testing.T) {
	// The ordinary sequence is `changes commit` then `changes push`: the
	// working tree is clean by the time the branch is named, so the
	// diff-derived fallback would always be the meaningless "changes". The
	// name must come from the commit being pushed instead.
	cwd := t.TempDir()
	var createdName string

	branch, _, _, err := ensureFeatureBranch(context.Background(), &bytes.Buffer{}, cwd, "", featureBranchOptions{}, appDeps{
		isDefaultBranch: func(ctx context.Context, options zerogit.DefaultBranchOptions) (bool, string, string, error) {
			return true, "main", "origin", nil
		},
		commitsAhead:   func(ctx context.Context, cwd, remote, branch string) (int, error) { return 1, nil },
		inspectChanges: featureBranchInspect(nil, ""), // clean tree + empty committed range: name from HEAD
		headCommitSubject: func(ctx context.Context, cwd string) string {
			return "fix(parser): handle empty input"
		},
		resolveConfig: func(workspaceRoot string, overrides config.Overrides) (config.ResolvedConfig, error) {
			return config.ResolvedConfig{}, nil
		},
		currentGitUser: func(ctx context.Context, cwd string) string { return "Someone" },
		createBranch: func(ctx context.Context, options zerogit.BranchOptions) (zerogit.BranchResult, error) {
			createdName = options.Name
			return zerogit.BranchResult{Branch: options.Name}, nil
		},
	})
	if err != nil {
		t.Fatalf("ensureFeatureBranch returned error: %v", err)
	}
	if branch != createdName || !strings.HasPrefix(branch, "someone/fix-parser") {
		t.Fatalf("expected a name derived from the HEAD commit subject, got %q", branch)
	}
}

func TestEnsureFeatureBranchDoesNotCallProviderWithoutAuto(t *testing.T) {
	// changes push/pr were git-only commands: a configured provider must not
	// cause the change diff to be uploaded for naming unless --auto opts in.
	cwd := t.TempDir()

	branch, _, _, err := ensureFeatureBranch(context.Background(), &bytes.Buffer{}, cwd, "", featureBranchOptions{}, appDeps{
		isDefaultBranch: func(ctx context.Context, options zerogit.DefaultBranchOptions) (bool, string, string, error) {
			return true, "main", "origin", nil
		},
		commitsAhead:   func(ctx context.Context, cwd, remote, branch string) (int, error) { return 1, nil },
		inspectChanges: featureBranchInspect([]zerogit.FileChange{{Path: "login.go", Status: "added"}}, "+func Login() {}"),
		resolveConfig: func(workspaceRoot string, overrides config.Overrides) (config.ResolvedConfig, error) {
			return execResolvedConfig(), nil // provider IS configured
		},
		newProvider: func(profile config.ProviderProfile) (zeroruntime.Provider, error) {
			t.Fatal("provider must not be constructed without --auto")
			return nil, nil
		},
		currentGitUser: func(ctx context.Context, cwd string) string { return "Someone" },
		createBranch: func(ctx context.Context, options zerogit.BranchOptions) (zerogit.BranchResult, error) {
			return zerogit.BranchResult{Branch: options.Name}, nil
		},
	})
	if err != nil {
		t.Fatalf("ensureFeatureBranch returned error: %v", err)
	}
	if branch != "someone/login-go" {
		t.Fatalf("expected the deterministic local name, got %q", branch)
	}
}

func TestEnsureFeatureBranchThreadsDiffBytesToInspect(t *testing.T) {
	// --diff-bytes caps how much of the diff Inspect returns; with --auto that
	// diff is embedded in the provider request, so the cap must reach Inspect
	// or a user bounding the proprietary source sent for LLM naming would still
	// upload the complete diff.
	cwd := t.TempDir()
	var gotMaxDiffBytes int
	mockProv := &mockCommitMsgProvider{
		response: "feat/capped-diff-branch",
	}

	_, _, _, err := ensureFeatureBranch(context.Background(), &bytes.Buffer{}, cwd, "", featureBranchOptions{AutoNaming: true, MaxDiffBytes: 4096}, appDeps{
		isDefaultBranch: func(ctx context.Context, options zerogit.DefaultBranchOptions) (bool, string, string, error) {
			return true, "main", "origin", nil
		},
		commitsAhead: func(ctx context.Context, cwd, remote, branch string) (int, error) { return 1, nil },
		inspectChanges: func(ctx context.Context, options zerogit.InspectOptions) (zerogit.ChangeSummary, error) {
			if strings.TrimSpace(options.BaseRef) == "" {
				return zerogit.ChangeSummary{Clean: true}, nil
			}
			gotMaxDiffBytes = options.MaxDiffBytes
			return zerogit.ChangeSummary{
				Files: []zerogit.FileChange{{Path: "README.md", Status: "modified"}},
				Diff:  "diff --git a/README.md b/README.md\n+capped diff content",
			}, nil
		},
		resolveConfig: func(workspaceRoot string, overrides config.Overrides) (config.ResolvedConfig, error) {
			return execResolvedConfig(), nil
		},
		newProvider: func(profile config.ProviderProfile) (zeroruntime.Provider, error) {
			return mockProv, nil
		},
		currentGitUser: func(ctx context.Context, cwd string) string { return "Someone" },
		createBranch: func(ctx context.Context, options zerogit.BranchOptions) (zerogit.BranchResult, error) {
			return zerogit.BranchResult{Branch: options.Name}, nil
		},
	})
	if err != nil {
		t.Fatalf("ensureFeatureBranch returned error: %v", err)
	}
	if gotMaxDiffBytes != 4096 {
		t.Fatalf("expected MaxDiffBytes 4096 threaded into Inspect, got %d", gotMaxDiffBytes)
	}
	if len(mockProv.req.Messages) == 0 || !strings.Contains(mockProv.req.Messages[len(mockProv.req.Messages)-1].Content, "capped diff content") {
		t.Fatalf("expected provider request to contain capped diff content, got %#v", mockProv.req)
	}
}

func TestEnsureFeatureBranchRefusesWhenNothingToPublish(t *testing.T) {
	// On a clean, up-to-date default branch HEAD is not ahead of the remote
	// default, so a push would publish nothing. ensureFeatureBranch must refuse
	// instead of creating and pushing an empty feature branch.
	cwd := t.TempDir()
	createBranchCalled := false

	_, _, _, err := ensureFeatureBranch(context.Background(), &bytes.Buffer{}, cwd, "", featureBranchOptions{}, appDeps{
		isDefaultBranch: func(ctx context.Context, options zerogit.DefaultBranchOptions) (bool, string, string, error) {
			return true, "main", "origin", nil
		},
		commitsAhead: func(ctx context.Context, cwd, remote, branch string) (int, error) {
			return 0, nil
		},
		inspectChanges: func(ctx context.Context, options zerogit.InspectOptions) (zerogit.ChangeSummary, error) {
			if strings.TrimSpace(options.BaseRef) != "" {
				t.Fatal("base-ref inspect should not run when there is nothing to publish")
			}
			return zerogit.ChangeSummary{Clean: true}, nil
		},
		createBranch: func(ctx context.Context, options zerogit.BranchOptions) (zerogit.BranchResult, error) {
			createBranchCalled = true
			return zerogit.BranchResult{Branch: options.Name}, nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "no changes to publish") {
		t.Fatalf("expected a no-changes-to-publish error, got %v", err)
	}
	if createBranchCalled {
		t.Fatal("expected createBranch not to be called when nothing is publishable")
	}
}

func TestEnsureFeatureBranchRefusesDirtyWorkingTree(t *testing.T) {
	// CreateBranch/Push publish commits only. With an ahead commit plus
	// uncommitted edits, naming and pushing would leave those edits behind
	// under a branch/PR that does not include them.
	cwd := t.TempDir()
	createBranchCalled := false

	_, _, _, err := ensureFeatureBranch(context.Background(), &bytes.Buffer{}, cwd, "", featureBranchOptions{}, appDeps{
		isDefaultBranch: func(ctx context.Context, options zerogit.DefaultBranchOptions) (bool, string, string, error) {
			return true, "main", "origin", nil
		},
		commitsAhead: func(ctx context.Context, cwd, remote, branch string) (int, error) {
			t.Fatal("commitsAhead should not run when the working tree is dirty")
			return 0, nil
		},
		inspectChanges: func(ctx context.Context, options zerogit.InspectOptions) (zerogit.ChangeSummary, error) {
			if strings.TrimSpace(options.BaseRef) != "" {
				t.Fatal("base-ref inspect should not run when the working tree is dirty")
			}
			return zerogit.ChangeSummary{
				Clean: false,
				Files: []zerogit.FileChange{{Path: "wip.go", Status: "modified"}},
			}, nil
		},
		createBranch: func(ctx context.Context, options zerogit.BranchOptions) (zerogit.BranchResult, error) {
			createBranchCalled = true
			return zerogit.BranchResult{Branch: options.Name}, nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "uncommitted changes") {
		t.Fatalf("expected an uncommitted-changes error, got %v", err)
	}
	if createBranchCalled {
		t.Fatal("expected createBranch not to be called when the working tree is dirty")
	}
}

func TestEnsureFeatureBranchFailsWhenAheadCountUnknown(t *testing.T) {
	// A missing remote-tracking ref (never fetched) means the ahead count
	// cannot be determined. Fail closed rather than guessing that there is
	// something to publish (or naming a branch from a working-tree snapshot).
	cwd := t.TempDir()
	createBranchCalled := false

	_, _, _, err := ensureFeatureBranch(context.Background(), &bytes.Buffer{}, cwd, "", featureBranchOptions{}, appDeps{
		isDefaultBranch: func(ctx context.Context, options zerogit.DefaultBranchOptions) (bool, string, string, error) {
			return true, "main", "origin", nil
		},
		commitsAhead: func(ctx context.Context, cwd, remote, branch string) (int, error) {
			return 0, errors.New("unknown revision origin/main")
		},
		isUnbornRemote: func(ctx context.Context, cwd, remote string) (bool, error) {
			// Not unborn: the remote exists and has branches, it just was
			// never fetched locally. This must still fail closed.
			return false, nil
		},
		inspectChanges: func(ctx context.Context, options zerogit.InspectOptions) (zerogit.ChangeSummary, error) {
			if strings.TrimSpace(options.BaseRef) != "" {
				t.Fatal("base-ref inspect should not run when the ahead count is unknown")
			}
			return zerogit.ChangeSummary{Clean: true}, nil
		},
		createBranch: func(ctx context.Context, options zerogit.BranchOptions) (zerogit.BranchResult, error) {
			createBranchCalled = true
			return zerogit.BranchResult{Branch: options.Name}, nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "cannot determine whether HEAD is ahead") {
		t.Fatalf("expected an ahead-count-unknown error, got %v", err)
	}
	if createBranchCalled {
		t.Fatal("expected createBranch not to be called when the publishable range is unknown")
	}
}

func TestEnsureFeatureBranchFailsWhenUnbornCheckErrors(t *testing.T) {
	// The ahead count is unknown AND the unborn probe itself fails (remote
	// unreachable): this must still fail closed exactly like the plain
	// unknown-ahead-count case, not be treated as a confirmed-unborn remote.
	cwd := t.TempDir()
	createBranchCalled := false

	_, _, _, err := ensureFeatureBranch(context.Background(), &bytes.Buffer{}, cwd, "", featureBranchOptions{}, appDeps{
		isDefaultBranch: func(ctx context.Context, options zerogit.DefaultBranchOptions) (bool, string, string, error) {
			return true, "main", "origin", nil
		},
		commitsAhead: func(ctx context.Context, cwd, remote, branch string) (int, error) {
			return 0, errors.New("unknown revision origin/main")
		},
		isUnbornRemote: func(ctx context.Context, cwd, remote string) (bool, error) {
			return false, errors.New("remote unreachable")
		},
		inspectChanges: func(ctx context.Context, options zerogit.InspectOptions) (zerogit.ChangeSummary, error) {
			if strings.TrimSpace(options.BaseRef) != "" {
				t.Fatal("base-ref inspect should not run when the ahead count is unknown")
			}
			return zerogit.ChangeSummary{Clean: true}, nil
		},
		createBranch: func(ctx context.Context, options zerogit.BranchOptions) (zerogit.BranchResult, error) {
			createBranchCalled = true
			return zerogit.BranchResult{Branch: options.Name}, nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "cannot determine whether HEAD is ahead") {
		t.Fatalf("expected an ahead-count-unknown error, got %v", err)
	}
	if createBranchCalled {
		t.Fatal("expected createBranch not to be called when the unborn state is unconfirmed")
	}
}

// TestEnsureFeatureBranchRefusesUnbornRemote covers jatmn's P1: auto-creating
// only a feature branch on a brand-new empty remote leaves remote HEAD
// dangling (no default branch), so the next `changes pr` fails closed and a
// later `push --yes` of main publishes the same tip with no PR diff. Refuse
// auto-branching until the initial default branch is established with --yes.
func TestEnsureFeatureBranchRefusesUnbornRemote(t *testing.T) {
	cwd := t.TempDir()
	createBranchCalled := false
	var isUnbornRemoteCalled bool

	_, _, created, err := ensureFeatureBranch(context.Background(), &bytes.Buffer{}, cwd, "", featureBranchOptions{}, appDeps{
		isDefaultBranch: func(ctx context.Context, options zerogit.DefaultBranchOptions) (bool, string, string, error) {
			return true, "main", "origin", nil
		},
		inspectChanges: featureBranchInspect(nil, ""),
		commitsAhead: func(ctx context.Context, cwd, remote, branch string) (int, error) {
			return 0, errors.New("unknown revision origin/main..HEAD")
		},
		isUnbornRemote: func(ctx context.Context, cwd, remote string) (bool, error) {
			isUnbornRemoteCalled = true
			if remote != "origin" {
				t.Fatalf("expected the resolved remote %q, got %q", "origin", remote)
			}
			return true, nil
		},
		createBranch: func(ctx context.Context, options zerogit.BranchOptions) (zerogit.BranchResult, error) {
			createBranchCalled = true
			return zerogit.BranchResult{Branch: options.Name}, nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "no branches yet") || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("expected unborn-remote refuse error mentioning --yes, got %v", err)
	}
	if !isUnbornRemoteCalled {
		t.Fatal("expected isUnbornRemote to be consulted after commitsAhead failed")
	}
	if created || createBranchCalled {
		t.Fatal("expected no feature branch on an unborn remote")
	}
}

func TestEnsureFeatureBranchInspectsAgainstResolvedRemoteBranch(t *testing.T) {
	// Push and CreateBranch only publish commits, so the branch (and, with
	// --auto, the diff sent to a provider) must be named from what HEAD is
	// actually ahead of the resolved remote branch by, not from the working
	// tree: Inspect must be asked to diff against "<remote>/<branch>", the
	// same ref commitsAhead just checked.
	cwd := t.TempDir()
	var gotBaseRef string

	_, _, _, err := ensureFeatureBranch(context.Background(), &bytes.Buffer{}, cwd, "", featureBranchOptions{}, appDeps{
		isDefaultBranch: func(ctx context.Context, options zerogit.DefaultBranchOptions) (bool, string, string, error) {
			return true, "main", "origin", nil
		},
		commitsAhead: func(ctx context.Context, cwd, remote, branch string) (int, error) { return 1, nil },
		inspectChanges: func(ctx context.Context, options zerogit.InspectOptions) (zerogit.ChangeSummary, error) {
			if strings.TrimSpace(options.BaseRef) == "" {
				return zerogit.ChangeSummary{Clean: true}, nil
			}
			gotBaseRef = options.BaseRef
			return zerogit.ChangeSummary{Files: []zerogit.FileChange{{Path: "README.md", Status: "modified"}}}, nil
		},
		resolveConfig: func(workspaceRoot string, overrides config.Overrides) (config.ResolvedConfig, error) {
			return config.ResolvedConfig{}, nil
		},
		currentGitUser: func(ctx context.Context, cwd string) string { return "Someone" },
		createBranch: func(ctx context.Context, options zerogit.BranchOptions) (zerogit.BranchResult, error) {
			return zerogit.BranchResult{Branch: options.Name}, nil
		},
	})
	if err != nil {
		t.Fatalf("ensureFeatureBranch returned error: %v", err)
	}
	if gotBaseRef != "origin/main" {
		t.Fatalf("expected Inspect to diff against %q, got %q", "origin/main", gotBaseRef)
	}
}

func TestRunChangesPushUsesResolvedRemoteForNewBranch(t *testing.T) {
	// In a fork setup the original branch tracks a non-origin remote. The
	// freshly created feature branch has no tracking configuration, so the
	// resolved remote must be threaded into Push explicitly or it would
	// silently fall back to origin.
	cwd := t.TempDir()
	var pushedRemote string

	var stdout, stderr bytes.Buffer
	exitCode := runWithDeps([]string{"changes", "push"}, &stdout, &stderr, appDeps{
		getwd: func() (string, error) { return cwd, nil },
		isDefaultBranch: func(ctx context.Context, options zerogit.DefaultBranchOptions) (bool, string, string, error) {
			return true, "main", "upstream", nil
		},
		commitsAhead:       func(ctx context.Context, cwd, remote, branch string) (int, error) { return 1, nil },
		refreshTrackingRef: func(ctx context.Context, cwd, remote, branch string) error { return nil },
		inspectChanges:     featureBranchInspect([]zerogit.FileChange{{Path: "README.md", Status: "modified"}}, ""),
		resolveConfig: func(workspaceRoot string, overrides config.Overrides) (config.ResolvedConfig, error) {
			return config.ResolvedConfig{}, nil
		},
		currentGitUser: func(ctx context.Context, cwd string) string { return "Someone" },
		createBranch: func(ctx context.Context, options zerogit.BranchOptions) (zerogit.BranchResult, error) {
			return zerogit.BranchResult{Branch: options.Name}, nil
		},
		pushChanges: func(ctx context.Context, options zerogit.PushOptions) (zerogit.PushResult, error) {
			pushedRemote = options.Remote
			return zerogit.PushResult{Remote: options.Remote, Branch: options.Branch}, nil
		},
	})

	if exitCode != exitSuccess {
		t.Fatalf("expected exit code %d, got %d: %s", exitSuccess, exitCode, stderr.String())
	}
	if pushedRemote != "upstream" {
		t.Fatalf("expected push to target the resolved remote %q, got %q", "upstream", pushedRemote)
	}
}

func TestRunChangesPushCreatesFeatureBranchWhenOnDefault(t *testing.T) {
	cwd := t.TempDir()
	var pushedBranch string
	var requiredNewRemoteBranch bool

	var stdout, stderr bytes.Buffer
	exitCode := runWithDeps([]string{"changes", "push"}, &stdout, &stderr, appDeps{
		getwd: func() (string, error) { return cwd, nil },
		isDefaultBranch: func(ctx context.Context, options zerogit.DefaultBranchOptions) (bool, string, string, error) {
			return true, "main", "origin", nil
		},
		commitsAhead:       func(ctx context.Context, cwd, remote, branch string) (int, error) { return 1, nil },
		refreshTrackingRef: func(ctx context.Context, cwd, remote, branch string) error { return nil },
		inspectChanges:     featureBranchInspect([]zerogit.FileChange{{Path: "README.md", Status: "modified"}}, ""),
		resolveConfig: func(workspaceRoot string, overrides config.Overrides) (config.ResolvedConfig, error) {
			return config.ResolvedConfig{}, nil
		},
		currentGitUser: func(ctx context.Context, cwd string) string { return "Someone" },
		createBranch: func(ctx context.Context, options zerogit.BranchOptions) (zerogit.BranchResult, error) {
			return zerogit.BranchResult{Branch: options.Name}, nil
		},
		pushChanges: func(ctx context.Context, options zerogit.PushOptions) (zerogit.PushResult, error) {
			pushedBranch = options.Branch
			requiredNewRemoteBranch = options.RequireNewRemoteBranch
			return zerogit.PushResult{Remote: "origin", Branch: options.Branch}, nil
		},
	})

	if exitCode != exitSuccess {
		t.Fatalf("expected exit code %d, got %d: %s", exitSuccess, exitCode, stderr.String())
	}
	if pushedBranch != "someone/readme-md" {
		t.Fatalf("expected push to target the newly created branch, got %q", pushedBranch)
	}
	if !strings.Contains(stdout.String(), "Created branch someone/readme-md") {
		t.Fatalf("expected branch-creation message in stdout, got %q", stdout.String())
	}
	// CreateBranch's own remote-collision probe runs before this push, so the
	// push itself must assert the destination is still new, closing the
	// window for a concurrent creator of the same name.
	if !requiredNewRemoteBranch {
		t.Fatal("expected Push to require the destination not already exist on the remote")
	}
}

func TestRunChangesPRCreatesFeatureBranchWhenOnDefault(t *testing.T) {
	cwd := t.TempDir()
	var pushedBranch string

	var stdout, stderr bytes.Buffer
	exitCode := runWithDeps([]string{"changes", "pr", "--fill"}, &stdout, &stderr, appDeps{
		getwd: func() (string, error) { return cwd, nil },
		isDefaultBranch: func(ctx context.Context, options zerogit.DefaultBranchOptions) (bool, string, string, error) {
			return true, "main", "origin", nil
		},
		commitsAhead:       func(ctx context.Context, cwd, remote, branch string) (int, error) { return 1, nil },
		refreshTrackingRef: func(ctx context.Context, cwd, remote, branch string) error { return nil },
		inspectChanges:     featureBranchInspect([]zerogit.FileChange{{Path: "README.md", Status: "modified"}}, ""),
		resolveConfig: func(workspaceRoot string, overrides config.Overrides) (config.ResolvedConfig, error) {
			return config.ResolvedConfig{}, nil
		},
		currentGitUser: func(ctx context.Context, cwd string) string { return "Someone" },
		createBranch: func(ctx context.Context, options zerogit.BranchOptions) (zerogit.BranchResult, error) {
			return zerogit.BranchResult{Branch: options.Name}, nil
		},
		pushChanges: func(ctx context.Context, options zerogit.PushOptions) (zerogit.PushResult, error) {
			pushedBranch = options.Branch
			return zerogit.PushResult{Remote: "origin", Branch: options.Branch}, nil
		},
		createPR: func(ctx context.Context, options zerogit.PROptions) (zerogit.PRResult, error) {
			return zerogit.PRResult{Output: "https://example.invalid/pr/1"}, nil
		},
	})

	if exitCode != exitSuccess {
		t.Fatalf("expected exit code %d, got %d: %s", exitSuccess, exitCode, stderr.String())
	}
	// runChangesPR hardcodes dryRun=false into ensureFeatureBranch (unlike push,
	// which forwards options.dryRun), so it always creates and forwards the
	// branch on the default branch, with no --dry-run bypass to verify here.
	if pushedBranch != "someone/readme-md" {
		t.Fatalf("expected pushChanges to target the newly created branch, got %q", pushedBranch)
	}
	if !strings.Contains(stdout.String(), "Created branch someone/readme-md") {
		t.Fatalf("expected branch-creation message in stdout, got %q", stdout.String())
	}
}

func TestRunChangesPushSkipsBranchCreationWithYes(t *testing.T) {
	cwd := t.TempDir()
	isDefaultBranchCalled := false

	var stdout, stderr bytes.Buffer
	exitCode := runWithDeps([]string{"changes", "push", "--yes"}, &stdout, &stderr, appDeps{
		getwd: func() (string, error) { return cwd, nil },
		isDefaultBranch: func(ctx context.Context, options zerogit.DefaultBranchOptions) (bool, string, string, error) {
			isDefaultBranchCalled = true
			return true, "main", "origin", nil
		},
		pushChanges: func(ctx context.Context, options zerogit.PushOptions) (zerogit.PushResult, error) {
			if options.Branch != "" {
				t.Fatalf("expected empty Branch (defer to current HEAD) with --yes, got %q", options.Branch)
			}
			if options.RequireNewRemoteBranch {
				t.Fatal("expected RequireNewRemoteBranch to be false: no branch was created")
			}
			return zerogit.PushResult{Remote: "origin", Branch: "main"}, nil
		},
	})

	if exitCode != exitSuccess {
		t.Fatalf("expected exit code %d, got %d: %s", exitSuccess, exitCode, stderr.String())
	}
	if isDefaultBranchCalled {
		t.Fatal("expected isDefaultBranch not to be consulted when --yes is passed")
	}
}

func TestRunChangesPRRejectsUnbornRemoteEvenWithYes(t *testing.T) {
	cwd := t.TempDir()
	isUnbornRemoteCalled := false

	var stdout, stderr bytes.Buffer
	exitCode := runWithDeps([]string{"changes", "pr", "--yes", "--fill"}, &stdout, &stderr, appDeps{
		getwd: func() (string, error) { return cwd, nil },
		isUnbornRemote: func(ctx context.Context, cwd, remote string) (bool, error) {
			isUnbornRemoteCalled = true
			if remote != "origin" {
				t.Fatalf("expected remote %q, got %q", "origin", remote)
			}
			return true, nil
		},
		pushChanges: func(ctx context.Context, options zerogit.PushOptions) (zerogit.PushResult, error) {
			t.Fatal("pushChanges should not be called on unborn remote")
			return zerogit.PushResult{}, nil
		},
		createPR: func(ctx context.Context, options zerogit.PROptions) (zerogit.PRResult, error) {
			t.Fatal("createPR should not be called on unborn remote")
			return zerogit.PRResult{}, nil
		},
	})

	if exitCode != exitUsage {
		t.Fatalf("expected exit code %d, got %d", exitUsage, exitCode)
	}
	if !isUnbornRemoteCalled {
		t.Fatal("expected isUnbornRemote to be consulted even when --yes is passed")
	}
	if !strings.Contains(stderr.String(), "cannot create pull request on unborn remote origin") {
		t.Fatalf("unexpected stderr: %q", stderr.String())
	}
}

// TestRunChangesPushRequiresLeaseWhenBranchMissingOnRemote covers the retry
// case: HEAD is already on a previously-generated branch (whether from a
// lost force-with-lease race or any other reason it never landed), and the
// live remote check finds no branch of that name yet. The nonexistence lease
// must stay so a concurrent creator of the same name cannot be silently
// fast-forwarded over.
func TestRunChangesPushRequiresLeaseWhenBranchMissingOnRemote(t *testing.T) {
	cwd := t.TempDir()
	var requireNewRemoteBranch bool
	var pushedBranch string
	var remoteProbed bool
	createBranchCalled := false

	var stdout, stderr bytes.Buffer
	exitCode := runWithDeps([]string{"changes", "push"}, &stdout, &stderr, appDeps{
		getwd: func() (string, error) { return cwd, nil },
		isDefaultBranch: func(ctx context.Context, options zerogit.DefaultBranchOptions) (bool, string, string, error) {
			return false, "someone/readme-md", "origin", nil
		},
		remoteHasBranch: func(ctx context.Context, cwd, remote, branch string) (bool, error) {
			remoteProbed = true
			if remote != "origin" || branch != "someone/readme-md" {
				t.Fatalf("unexpected remote probe: %s/%s", remote, branch)
			}
			return false, nil
		},
		createBranch: func(ctx context.Context, options zerogit.BranchOptions) (zerogit.BranchResult, error) {
			createBranchCalled = true
			return zerogit.BranchResult{}, nil
		},
		pushChanges: func(ctx context.Context, options zerogit.PushOptions) (zerogit.PushResult, error) {
			pushedBranch = options.Branch
			requireNewRemoteBranch = options.RequireNewRemoteBranch
			return zerogit.PushResult{Remote: options.Remote, Branch: options.Branch}, nil
		},
	})

	if exitCode != exitSuccess {
		t.Fatalf("expected exit code %d, got %d: %s", exitSuccess, exitCode, stderr.String())
	}
	if createBranchCalled {
		t.Fatal("expected createBranch not to be called on a retry already off the default branch")
	}
	if !remoteProbed {
		t.Fatal("expected a live remote check before deciding the lease")
	}
	if pushedBranch != "someone/readme-md" {
		t.Fatalf("expected the retry to target the same branch, got %q", pushedBranch)
	}
	if !requireNewRemoteBranch {
		t.Fatal("expected the nonexistence lease when the remote check finds no branch")
	}
}

// TestRunChangesPushDropsLeaseWhenBranchExistsOnRemote is the companion case:
// the live remote check finds the branch already exists, so the push must go
// through as a plain (non-leased) push. Git's own fast-forward check is the
// safety net if that existing branch is not actually ours.
func TestRunChangesPushDropsLeaseWhenBranchExistsOnRemote(t *testing.T) {
	cwd := t.TempDir()
	var requireNewRemoteBranch bool
	var remoteProbed bool

	var stdout, stderr bytes.Buffer
	exitCode := runWithDeps([]string{"changes", "push"}, &stdout, &stderr, appDeps{
		getwd: func() (string, error) { return cwd, nil },
		isDefaultBranch: func(ctx context.Context, options zerogit.DefaultBranchOptions) (bool, string, string, error) {
			return false, "someone/readme-md", "origin", nil
		},
		remoteHasBranch: func(ctx context.Context, cwd, remote, branch string) (bool, error) {
			remoteProbed = true
			if remote != "origin" || branch != "someone/readme-md" {
				t.Fatalf("unexpected remote probe: %s/%s", remote, branch)
			}
			return true, nil
		},
		pushChanges: func(ctx context.Context, options zerogit.PushOptions) (zerogit.PushResult, error) {
			requireNewRemoteBranch = options.RequireNewRemoteBranch
			return zerogit.PushResult{Remote: options.Remote, Branch: options.Branch}, nil
		},
	})

	if exitCode != exitSuccess {
		t.Fatalf("expected exit code %d, got %d: %s", exitSuccess, exitCode, stderr.String())
	}
	if !remoteProbed {
		t.Fatal("expected a live remote check before deciding the lease")
	}
	if requireNewRemoteBranch {
		t.Fatal("expected no nonexistence lease when the remote check finds the branch")
	}
}

// TestRunChangesPushChecksSelectedRemoteNotConfiguredUpstream covers a
// generated branch published to origin while this invocation targets a
// different remote (--remote upstream): the live check must probe the
// selected remote, not whatever the branch happens to track locally.
func TestRunChangesPushChecksSelectedRemoteNotConfiguredUpstream(t *testing.T) {
	cwd := t.TempDir()
	var probedRemote string
	var requireNewRemoteBranch bool

	var stdout, stderr bytes.Buffer
	exitCode := runWithDeps([]string{"changes", "push", "--remote", "upstream"}, &stdout, &stderr, appDeps{
		getwd: func() (string, error) { return cwd, nil },
		isDefaultBranch: func(ctx context.Context, options zerogit.DefaultBranchOptions) (bool, string, string, error) {
			if options.Remote != "upstream" {
				t.Fatalf("expected requested remote upstream, got %q", options.Remote)
			}
			return false, "user/feature", "origin", nil
		},
		remoteHasBranch: func(ctx context.Context, cwd, remote, branch string) (bool, error) {
			probedRemote = remote
			// Exists on origin (this invocation's default), but this push
			// targets upstream, where it has never been published.
			return false, nil
		},
		pushChanges: func(ctx context.Context, options zerogit.PushOptions) (zerogit.PushResult, error) {
			requireNewRemoteBranch = options.RequireNewRemoteBranch
			return zerogit.PushResult{Remote: options.Remote, Branch: options.Branch}, nil
		},
	})

	if exitCode != exitSuccess {
		t.Fatalf("expected exit code %d, got %d: %s", exitSuccess, exitCode, stderr.String())
	}
	if probedRemote != "upstream" {
		t.Fatalf("expected the live check against the requested remote upstream, got %q", probedRemote)
	}
	if !requireNewRemoteBranch {
		t.Fatal("expected the nonexistence lease for the not-yet-published upstream remote")
	}
}

// TestEnsureFeatureBranchRefreshesTrackingRefBeforeCheckingAhead covers
// jatmn's P2 finding: validate publishability against the live default, not a
// stale local tracking ref. IsDefaultBranch already contacted the remote for
// its symref check, but a merely-cached origin/main (last fetched earlier)
// can sit behind the remote's live tip. Before the fix, commitsAhead ran
// straight against that stale ref: this simulates the stale ref reporting
// zero commits ahead until the tracking ref is refreshed, matching the
// scenario where the remote has already advanced past what was last fetched.
func TestEnsureFeatureBranchRefreshesTrackingRefBeforeCheckingAhead(t *testing.T) {
	cwd := t.TempDir()
	refreshed := false
	var createdName string

	branch, _, _, err := ensureFeatureBranch(context.Background(), &bytes.Buffer{}, cwd, "", featureBranchOptions{}, appDeps{
		isDefaultBranch: func(ctx context.Context, options zerogit.DefaultBranchOptions) (bool, string, string, error) {
			return true, "main", "origin", nil
		},
		refreshTrackingRef: func(ctx context.Context, cwd, remote, branch string) error {
			if remote != "origin" || branch != "main" {
				t.Fatalf("unexpected refresh target: %s/%s", remote, branch)
			}
			refreshed = true
			return nil
		},
		commitsAhead: func(ctx context.Context, cwd, remote, branch string) (int, error) {
			if !refreshed {
				// The stale, unrefreshed local origin/main is behind and
				// would report zero commits ahead even though HEAD carries
				// real, unpublished work relative to the remote's live tip.
				return 0, nil
			}
			return 2, nil
		},
		inspectChanges: featureBranchInspect([]zerogit.FileChange{{Path: "README.md", Status: "modified"}}, ""),
		resolveConfig: func(workspaceRoot string, overrides config.Overrides) (config.ResolvedConfig, error) {
			return config.ResolvedConfig{}, nil
		},
		currentGitUser: func(ctx context.Context, cwd string) string { return "Someone" },
		createBranch: func(ctx context.Context, options zerogit.BranchOptions) (zerogit.BranchResult, error) {
			createdName = options.Name
			return zerogit.BranchResult{Branch: options.Name}, nil
		},
	})
	if err != nil {
		t.Fatalf("ensureFeatureBranch returned error: %v", err)
	}
	if !refreshed {
		t.Fatal("expected the tracking ref to be refreshed before the ahead check")
	}
	if branch != "someone/readme-md" || createdName != "someone/readme-md" {
		t.Fatalf("unexpected branch: got %q (created %q)", branch, createdName)
	}
}

// TestEnsureFeatureBranchFailsClosedWhenTrackingRefCannotBeRefreshed asserts
// that a failed refresh is never silently ignored in favor of trusting
// whatever commitsAhead reports against the (potentially stale, unrefreshed)
// local ref: the command must fail closed instead of risking a false "no
// changes to publish" from comparing against a tracking ref that could not be
// confirmed fresh.
func TestEnsureFeatureBranchFailsClosedWhenTrackingRefCannotBeRefreshed(t *testing.T) {
	cwd := t.TempDir()
	createBranchCalled := false

	_, _, _, err := ensureFeatureBranch(context.Background(), &bytes.Buffer{}, cwd, "", featureBranchOptions{}, appDeps{
		isDefaultBranch: func(ctx context.Context, options zerogit.DefaultBranchOptions) (bool, string, string, error) {
			return true, "main", "origin", nil
		},
		refreshTrackingRef: func(ctx context.Context, cwd, remote, branch string) error {
			return errors.New("remote unreachable")
		},
		commitsAhead: func(ctx context.Context, cwd, remote, branch string) (int, error) {
			return 0, nil
		},
		isUnbornRemote: func(ctx context.Context, cwd, remote string) (bool, error) {
			return false, nil
		},
		inspectChanges: func(ctx context.Context, options zerogit.InspectOptions) (zerogit.ChangeSummary, error) {
			return zerogit.ChangeSummary{Clean: true}, nil
		},
		createBranch: func(ctx context.Context, options zerogit.BranchOptions) (zerogit.BranchResult, error) {
			createBranchCalled = true
			return zerogit.BranchResult{Branch: options.Name}, nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "cannot determine whether HEAD is ahead") {
		t.Fatalf("expected a fail-closed error when the tracking ref could not be refreshed, got %v", err)
	}
	if createBranchCalled {
		t.Fatal("expected createBranch not to be called when the tracking ref could not be refreshed")
	}
}

// TestEnsureFeatureBranchErrorsWhenRemoteCheckFails covers the surfaced-error
// path: when the live remote check itself fails, ensureFeatureBranch must
// fail closed with a clear error rather than guessing a lease value.
func TestEnsureFeatureBranchErrorsWhenRemoteCheckFails(t *testing.T) {
	cwd := t.TempDir()
	_, _, _, err := ensureFeatureBranch(context.Background(), &bytes.Buffer{}, cwd, "", featureBranchOptions{}, appDeps{
		isDefaultBranch: func(ctx context.Context, options zerogit.DefaultBranchOptions) (bool, string, string, error) {
			return false, "user/slug", "origin", nil
		},
		remoteHasBranch: func(ctx context.Context, cwd, remote, branch string) (bool, error) {
			return false, errors.New("remote unreachable")
		},
	})
	if err == nil || !strings.Contains(err.Error(), "cannot check whether") {
		t.Fatalf("expected a clear remote-check error, got %v", err)
	}
}

func TestRunChangesPRYesBypassesDefaultBranchCheck(t *testing.T) {
	cwd := t.TempDir()
	var stdout, stderr bytes.Buffer
	exitCode := runWithDeps([]string{"changes", "pr", "--yes", "--fill"}, &stdout, &stderr, appDeps{
		getwd: func() (string, error) { return cwd, nil },
		isDefaultBranch: func(ctx context.Context, options zerogit.DefaultBranchOptions) (bool, string, string, error) {
			return false, "", "", errors.New("remote unreachable")
		},
		pushChanges: func(ctx context.Context, options zerogit.PushOptions) (zerogit.PushResult, error) {
			return zerogit.PushResult{Remote: "origin", Branch: "main"}, nil
		},
		createPR: func(ctx context.Context, options zerogit.PROptions) (zerogit.PRResult, error) {
			return zerogit.PRResult{Output: "https://example.invalid/pr/1"}, nil
		},
	})
	if exitCode != exitSuccess {
		t.Fatalf("expected exit code %d with --yes even when isDefaultBranch errors, got %d: %s", exitSuccess, exitCode, stderr.String())
	}
}

// TestEnsureFeatureBranchRestoresDefaultBranchAfterCreate covers jatmn's P2
// finding: after committing on main and auto-creating user/slug at the same
// HEAD, the local default branch must be moved back to its remote-tracking
// tip so it does not keep the new commit (which would diverge after a
// squash-merge).
func TestEnsureFeatureBranchRestoresDefaultBranchAfterCreate(t *testing.T) {
	cwd := t.TempDir()
	var resetBranch, resetTip, resetExpectedOld string
	var createdName string

	branch, _, created, err := ensureFeatureBranch(context.Background(), &bytes.Buffer{}, cwd, "", featureBranchOptions{}, appDeps{
		isDefaultBranch: func(ctx context.Context, options zerogit.DefaultBranchOptions) (bool, string, string, error) {
			return true, "main", "origin", nil
		},
		commitsAhead:       func(ctx context.Context, cwd, remote, branch string) (int, error) { return 1, nil },
		refreshTrackingRef: func(ctx context.Context, cwd, remote, branch string) error { return nil },
		inspectChanges:     featureBranchInspect([]zerogit.FileChange{{Path: "README.md", Status: "modified"}}, ""),
		currentGitUser:     func(ctx context.Context, cwd string) string { return "Someone" },
		currentBranchTip:   func(ctx context.Context, cwd string) string { return "abc123" },
		createBranch: func(ctx context.Context, options zerogit.BranchOptions) (zerogit.BranchResult, error) {
			createdName = options.Name
			return zerogit.BranchResult{Branch: options.Name}, nil
		},
		resetBranchRef: func(ctx context.Context, cwd, branch, newTip, expectedOld string) error {
			resetBranch = branch
			resetTip = newTip
			resetExpectedOld = expectedOld
			return nil
		},
	})
	if err != nil {
		t.Fatalf("ensureFeatureBranch returned error: %v", err)
	}
	if !created || branch != createdName {
		t.Fatalf("expected a created branch, got branch=%q created=%v", branch, created)
	}
	if resetBranch != "main" || resetTip != "origin/main" {
		t.Fatalf("expected reset of main to origin/main, got branch=%q tip=%q", resetBranch, resetTip)
	}
	// The pre-create HEAD tip must reach ResetBranchRef as the compare-and-swap
	// old value, or a concurrent advance of the default branch is silently
	// overwritten instead of refused.
	if resetExpectedOld != "abc123" {
		t.Fatalf("expected compare-and-swap old value abc123, got %q", resetExpectedOld)
	}
}

// TestEnsureFeatureBranchRollsBackWhenDefaultRestoreFails covers the
// failure-safe path: if restoring the default branch ref fails after the
// feature branch was created, delete the feature branch and surface the error
// rather than leaving a half-moved tree.
func TestEnsureFeatureBranchRollsBackWhenDefaultRestoreFails(t *testing.T) {
	cwd := t.TempDir()
	deletedBranch := ""

	_, _, _, err := ensureFeatureBranch(context.Background(), &bytes.Buffer{}, cwd, "", featureBranchOptions{}, appDeps{
		isDefaultBranch: func(ctx context.Context, options zerogit.DefaultBranchOptions) (bool, string, string, error) {
			return true, "main", "origin", nil
		},
		commitsAhead:   func(ctx context.Context, cwd, remote, branch string) (int, error) { return 1, nil },
		inspectChanges: featureBranchInspect([]zerogit.FileChange{{Path: "README.md", Status: "modified"}}, ""),
		currentGitUser: func(ctx context.Context, cwd string) string { return "user" },
		createBranch: func(ctx context.Context, options zerogit.BranchOptions) (zerogit.BranchResult, error) {
			return zerogit.BranchResult{Branch: options.Name}, nil
		},
		resetBranchRef: func(ctx context.Context, cwd, branch, newTip, expectedOld string) error {
			return errors.New("update-ref failed")
		},
		deleteBranch: func(ctx context.Context, cwd, fallbackBranch, branchToDelete string) error {
			if fallbackBranch != "main" {
				t.Fatalf("expected fallback main, got %q", fallbackBranch)
			}
			deletedBranch = branchToDelete
			return nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "restore default branch") {
		t.Fatalf("expected restore error, got %v", err)
	}
	if deletedBranch != "user/readme-md" {
		t.Fatalf("expected rollback deletion of user/readme-md, got %q", deletedBranch)
	}
}

// TestEnsureFeatureBranchReportsRollbackFailureWhenDeleteAlsoFails covers the
// dual-failure path: restore fails and feature-branch deletion also fails, so
// the error must mention both so the user knows manual repair is required.
func TestEnsureFeatureBranchReportsRollbackFailureWhenDeleteAlsoFails(t *testing.T) {
	cwd := t.TempDir()

	_, _, _, err := ensureFeatureBranch(context.Background(), &bytes.Buffer{}, cwd, "", featureBranchOptions{}, appDeps{
		isDefaultBranch: func(ctx context.Context, options zerogit.DefaultBranchOptions) (bool, string, string, error) {
			return true, "main", "origin", nil
		},
		commitsAhead:   func(ctx context.Context, cwd, remote, branch string) (int, error) { return 1, nil },
		inspectChanges: featureBranchInspect([]zerogit.FileChange{{Path: "README.md", Status: "modified"}}, ""),
		currentGitUser: func(ctx context.Context, cwd string) string { return "user" },
		createBranch: func(ctx context.Context, options zerogit.BranchOptions) (zerogit.BranchResult, error) {
			return zerogit.BranchResult{Branch: options.Name}, nil
		},
		resetBranchRef: func(ctx context.Context, cwd, branch, newTip, expectedOld string) error {
			return errors.New("update-ref failed")
		},
		deleteBranch: func(ctx context.Context, cwd, fallbackBranch, branchToDelete string) error {
			return errors.New("cannot delete branch: checked out")
		},
	})
	if err == nil {
		t.Fatal("expected dual-failure error, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "restore default branch") || !strings.Contains(msg, "rollback of user/readme-md also failed") {
		t.Fatalf("expected restore+rollback error, got %v", err)
	}
	if !strings.Contains(msg, "cannot delete branch") {
		t.Fatalf("expected delete cause in error, got %v", err)
	}
}

// TestEnsureFeatureBranchPreservesBranchOnCompareAndSwapConflict covers the
// concurrent-modification case: the default branch moved while we were
// branching, so the compare-and-swap restore refuses. The generic restore
// failure above rolls back by deleting the feature branch, but that branch now
// exclusively owns the user's commits, so deleting it here would destroy work
// to "recover" from a race. The branch must survive and the error must say so.
func TestEnsureFeatureBranchPreservesBranchOnCompareAndSwapConflict(t *testing.T) {
	cwd := t.TempDir()
	deleteCalled := false

	branch, _, created, err := ensureFeatureBranch(context.Background(), &bytes.Buffer{}, cwd, "", featureBranchOptions{}, appDeps{
		isDefaultBranch: func(ctx context.Context, options zerogit.DefaultBranchOptions) (bool, string, string, error) {
			return true, "main", "origin", nil
		},
		commitsAhead:   func(ctx context.Context, cwd, remote, branch string) (int, error) { return 1, nil },
		inspectChanges: featureBranchInspect([]zerogit.FileChange{{Path: "README.md", Status: "modified"}}, ""),
		currentGitUser: func(ctx context.Context, cwd string) string { return "user" },
		createBranch: func(ctx context.Context, options zerogit.BranchOptions) (zerogit.BranchResult, error) {
			return zerogit.BranchResult{Branch: options.Name}, nil
		},
		resetBranchRef: func(ctx context.Context, cwd, branch, newTip, expectedOld string) error {
			return fmt.Errorf("%w: update-ref refs/heads/main: stale", zerogit.ErrCompareAndSwapConflict)
		},
		deleteBranch: func(ctx context.Context, cwd, fallbackBranch, branchToDelete string) error {
			deleteCalled = true
			return nil
		},
	})
	if err == nil {
		t.Fatal("expected a restore error on a compare-and-swap conflict")
	}
	if deleteCalled {
		t.Fatal("compare-and-swap conflict must not delete the generated branch: it owns the user's commits")
	}
	if !strings.Contains(err.Error(), "preserved") {
		t.Fatalf("expected the error to report the branch was preserved, got %v", err)
	}
	if !strings.Contains(err.Error(), "user/readme-md") {
		t.Fatalf("expected the error to name the preserved branch, got %v", err)
	}
	// The call still fails, so the caller must not proceed to push.
	if created || branch != "" {
		t.Fatalf("expected no usable branch result, got branch=%q created=%v", branch, created)
	}
}

// TestEnsureFeatureBranchRestoresSourceUpstreamOnForkRemote covers jatmn's P2:
// --remote selects the push destination only. When local main tracks
// origin/main and the user passes --remote upstream, restore must leave main
// at origin/main, not rewrite it to upstream/main.
func TestEnsureFeatureBranchRestoresSourceUpstreamOnForkRemote(t *testing.T) {
	cwd := t.TempDir()
	var resetBranch, resetTip string
	var refreshedRemote, refreshedBranch string

	_, _, created, err := ensureFeatureBranch(context.Background(), &bytes.Buffer{}, cwd, "upstream", featureBranchOptions{}, appDeps{
		isDefaultBranch: func(ctx context.Context, options zerogit.DefaultBranchOptions) (bool, string, string, error) {
			if options.Remote != "upstream" {
				t.Fatalf("expected requested remote upstream, got %q", options.Remote)
			}
			return true, "main", "upstream", nil
		},
		commitsAhead: func(ctx context.Context, cwd, remote, branch string) (int, error) {
			if remote != "origin" || branch != "main" {
				t.Fatalf("expected commitsAhead check against origin/main, got %s/%s", remote, branch)
			}
			return 1, nil
		},
		refreshTrackingRef: func(ctx context.Context, cwd, remote, branch string) error {
			refreshedRemote = remote
			refreshedBranch = branch
			return nil
		},
		inspectChanges: featureBranchInspect([]zerogit.FileChange{{Path: "README.md", Status: "modified"}}, ""),
		currentGitUser: func(ctx context.Context, cwd string) string { return "Someone" },
		branchUpstreamRemoteAndMerge: func(ctx context.Context, cwd, branch string) (string, string) {
			if branch != "main" {
				t.Fatalf("expected upstream query for main, got %q", branch)
			}
			// Fork: main tracks origin, not the push destination.
			return "origin", "main"
		},
		createBranch: func(ctx context.Context, options zerogit.BranchOptions) (zerogit.BranchResult, error) {
			if options.Remote != "upstream" {
				t.Fatalf("expected create against upstream, got %q", options.Remote)
			}
			return zerogit.BranchResult{Branch: options.Name}, nil
		},
		resetBranchRef: func(ctx context.Context, cwd, branch, newTip, expectedOld string) error {
			resetBranch = branch
			resetTip = newTip
			return nil
		},
	})
	if err != nil {
		t.Fatalf("ensureFeatureBranch returned error: %v", err)
	}
	if !created {
		t.Fatal("expected a feature branch to be created")
	}
	if refreshedRemote != "origin" || refreshedBranch != "main" {
		t.Fatalf("expected refreshTrackingRef for origin/main, got %s/%s", refreshedRemote, refreshedBranch)
	}
	if resetBranch != "main" || resetTip != "origin/main" {
		t.Fatalf("expected restore main -> origin/main (source upstream), got %q -> %q", resetBranch, resetTip)
	}
}

// TestEnsureFeatureBranchHandlesSlashContainingRemoteName covers configured
// remote names with slashes (e.g. "team/upstream"). It must not split the
// remote name on '/' or fail tracking ref refresh.
func TestEnsureFeatureBranchHandlesSlashContainingRemoteName(t *testing.T) {
	cwd := t.TempDir()
	var resetBranch, resetTip string
	var refreshedRemote, refreshedBranch string

	_, _, created, err := ensureFeatureBranch(context.Background(), &bytes.Buffer{}, cwd, "", featureBranchOptions{}, appDeps{
		isDefaultBranch: func(ctx context.Context, options zerogit.DefaultBranchOptions) (bool, string, string, error) {
			return true, "main", "team/upstream", nil
		},
		branchUpstreamRemoteAndMerge: func(ctx context.Context, cwd, branch string) (string, string) {
			return "team/upstream", "main"
		},
		commitsAhead: func(ctx context.Context, cwd, remote, branch string) (int, error) {
			if remote != "team/upstream" || branch != "main" {
				t.Fatalf("expected commitsAhead check against team/upstream/main, got %s/%s", remote, branch)
			}
			return 1, nil
		},
		refreshTrackingRef: func(ctx context.Context, cwd, remote, branch string) error {
			refreshedRemote = remote
			refreshedBranch = branch
			return nil
		},
		inspectChanges: featureBranchInspect([]zerogit.FileChange{{Path: "README.md", Status: "modified"}}, ""),
		currentGitUser: func(ctx context.Context, cwd string) string { return "Someone" },
		createBranch: func(ctx context.Context, options zerogit.BranchOptions) (zerogit.BranchResult, error) {
			return zerogit.BranchResult{Branch: options.Name}, nil
		},
		resetBranchRef: func(ctx context.Context, cwd, branch, newTip, expectedOld string) error {
			resetBranch = branch
			resetTip = newTip
			return nil
		},
	})
	if err != nil {
		t.Fatalf("ensureFeatureBranch returned error: %v", err)
	}
	if !created {
		t.Fatal("expected a feature branch to be created")
	}
	if refreshedRemote != "team/upstream" || refreshedBranch != "main" {
		t.Fatalf("expected refreshTrackingRef for team/upstream/main, got %s/%s", refreshedRemote, refreshedBranch)
	}
	if resetBranch != "main" || resetTip != "team/upstream/main" {
		t.Fatalf("expected restore main -> team/upstream/main, got %q -> %q", resetBranch, resetTip)
	}
}

// TestEnsureFeatureBranchResolvesRestoreTipToCommit covers jatmn's P2: when the
// source upstream is a direct URL or path (branch.main.remote is a transport
// locator, not a named remote), restore must resolve a concrete commit, never
// fall back to the bare branch name. Resolving "main" yields the very commit
// the feature branch was just created from, making the restore a silent no-op
// and leaving the publishable commits on the default branch.
func TestEnsureFeatureBranchResolvesRestoreTipToCommit(t *testing.T) {
	tests := []struct {
		name           string
		upstreamRemote string
		upstreamBranch string
		tipSHA         string
		tipErr         error
		wantRestoreTip string
		wantErr        bool
		errContains    string
	}{
		{
			name:           "NamedRemoteOrigin",
			upstreamRemote: "origin",
			upstreamBranch: "main",
			tipSHA:         "1111111111111111111111111111111111111111",
			wantRestoreTip: "1111111111111111111111111111111111111111",
		},
		{
			name:           "DirectHTTPSURL",
			upstreamRemote: "https://github.com/example/repo.git",
			upstreamBranch: "main",
			tipSHA:         "2222222222222222222222222222222222222222",
			wantRestoreTip: "2222222222222222222222222222222222222222",
		},
		{
			name:           "DirectFilesystemPath",
			upstreamRemote: "/path/to/bare.git",
			upstreamBranch: "main",
			tipSHA:         "3333333333333333333333333333333333333333",
			wantRestoreTip: "3333333333333333333333333333333333333333",
		},
		{
			name:           "ResolutionErrorFailsClosedBeforeBranchCreation",
			upstreamRemote: "https://github.com/example/missing.git",
			upstreamBranch: "main",
			tipErr:         errors.New("remote has no branch main to restore to"),
			wantErr:        true,
			errContains:    "cannot resolve restore tip",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cwd := t.TempDir()
			var resetBranch, resetTip string
			branchCreated := false

			_, _, created, err := ensureFeatureBranch(context.Background(), &bytes.Buffer{}, cwd, "", featureBranchOptions{}, appDeps{
				isDefaultBranch: func(ctx context.Context, options zerogit.DefaultBranchOptions) (bool, string, string, error) {
					return true, "main", "origin", nil
				},
				branchUpstreamRemoteAndMerge: func(ctx context.Context, cwd, branch string) (string, string) {
					return tc.upstreamRemote, tc.upstreamBranch
				},
				resolveRemoteBranchTip: func(ctx context.Context, cwd, remote, branch string) (string, error) {
					if remote != tc.upstreamRemote || branch != tc.upstreamBranch {
						t.Fatalf("expected restore-tip resolution for %s/%s, got %s/%s", tc.upstreamRemote, tc.upstreamBranch, remote, branch)
					}
					return tc.tipSHA, tc.tipErr
				},
				commitsAhead: func(ctx context.Context, cwd, remote, branch string) (int, error) {
					return 1, nil
				},
				refreshTrackingRef: func(ctx context.Context, cwd, remote, branch string) error {
					return nil
				},
				inspectChanges: featureBranchInspect([]zerogit.FileChange{{Path: "README.md", Status: "modified"}}, ""),
				currentGitUser: func(ctx context.Context, cwd string) string { return "Someone" },
				createBranch: func(ctx context.Context, options zerogit.BranchOptions) (zerogit.BranchResult, error) {
					branchCreated = true
					return zerogit.BranchResult{Branch: options.Name}, nil
				},
				resetBranchRef: func(ctx context.Context, cwd, branch, newTip, expectedOld string) error {
					resetBranch = branch
					resetTip = newTip
					return nil
				},
			})

			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				if tc.errContains != "" && !strings.Contains(err.Error(), tc.errContains) {
					t.Fatalf("expected error containing %q, got %v", tc.errContains, err)
				}
				if branchCreated || created {
					t.Fatal("expected no branch creation when restore tip resolution fails")
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !created {
				t.Fatal("expected a feature branch to be created")
			}
			if resetBranch != "main" || resetTip != tc.wantRestoreTip {
				t.Fatalf("expected restore main -> %s, got %q -> %q", tc.wantRestoreTip, resetBranch, resetTip)
			}
		})
	}
}

// TestEnsureFeatureBranchUnbornDestinationMatrix covers jatmn's P2: unborn
// remote policy must gate the push destination as well as the source upstream.
func TestEnsureFeatureBranchUnbornDestinationMatrix(t *testing.T) {
	matrix := []struct {
		name               string
		requestedRemote    string
		defaultRemote      string
		unbornRemotes      map[string]bool
		allowDefaultBranch bool
		wantBranch         bool
		wantErr            bool
		errContains        string
	}{
		{
			name:            "PushDestinationUnbornViaFlagRefuses",
			requestedRemote: "backup",
			defaultRemote:   "origin",
			unbornRemotes:   map[string]bool{"backup": true, "origin": false},
			wantErr:         true,
			errContains:     "remote backup has no branches yet",
		},
		{
			name:            "SourceUpstreamUnbornRefuses",
			requestedRemote: "",
			defaultRemote:   "origin",
			unbornRemotes:   map[string]bool{"origin": true},
			wantErr:         true,
			errContains:     "remote origin has no branches yet",
		},
		{
			name:            "PopulatedDestinationAndSourceAllows",
			requestedRemote: "upstream",
			defaultRemote:   "origin",
			unbornRemotes:   map[string]bool{"upstream": false, "origin": false},
			wantBranch:      true,
		},
		{
			name:               "AllowDefaultBranchBypassesAutoBranching",
			requestedRemote:    "backup",
			defaultRemote:      "origin",
			unbornRemotes:      map[string]bool{"backup": true},
			allowDefaultBranch: true,
			wantBranch:         false,
		},
	}

	for _, tc := range matrix {
		t.Run(tc.name, func(t *testing.T) {
			cwd := t.TempDir()
			branchCreated := false

			var createdRemote string

			_, _, created, err := ensureFeatureBranch(context.Background(), &bytes.Buffer{}, cwd, tc.requestedRemote, featureBranchOptions{
				AllowDefaultBranch: tc.allowDefaultBranch,
			}, appDeps{
				isDefaultBranch: func(ctx context.Context, options zerogit.DefaultBranchOptions) (bool, string, string, error) {
					return true, "main", tc.defaultRemote, nil
				},
				branchUpstreamRemoteAndMerge: func(ctx context.Context, cwd, branch string) (string, string) {
					return tc.defaultRemote, "main"
				},
				resolveRemoteBranchTip: func(ctx context.Context, cwd, remote, branch string) (string, error) {
					return "abc123sha", nil
				},
				commitsAhead: func(ctx context.Context, cwd, remote, branch string) (int, error) {
					if tc.unbornRemotes[remote] {
						return 0, errors.New("cannot determine ahead")
					}
					return 1, nil
				},
				refreshTrackingRef: func(ctx context.Context, cwd, remote, branch string) error {
					return nil
				},
				isUnbornRemote: func(ctx context.Context, cwd, remote string) (bool, error) {
					return tc.unbornRemotes[remote], nil
				},
				inspectChanges: featureBranchInspect([]zerogit.FileChange{{Path: "README.md", Status: "modified"}}, ""),
				currentGitUser: func(ctx context.Context, cwd string) string { return "Someone" },
				createBranch: func(ctx context.Context, options zerogit.BranchOptions) (zerogit.BranchResult, error) {
					branchCreated = true
					createdRemote = options.Remote
					return zerogit.BranchResult{Branch: options.Name}, nil
				},
			})

			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				if tc.errContains != "" && !strings.Contains(err.Error(), tc.errContains) {
					t.Fatalf("expected error containing %q, got %v", tc.errContains, err)
				}
				if branchCreated || created {
					t.Fatal("expected no branch creation on error")
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if created != tc.wantBranch {
				t.Fatalf("created = %v, want %v", created, tc.wantBranch)
			}
			if tc.wantBranch {
				wantRemote := tc.defaultRemote
				if tc.requestedRemote != "" {
					wantRemote = tc.requestedRemote
				}
				if createdRemote != wantRemote {
					t.Fatalf("createdRemote = %q, want %q", createdRemote, wantRemote)
				}
			}
		})
	}
}

// TestResolveBranchPushPlan covers the pure plan resolution function directly
// without mutating repository state.
func TestResolveBranchPushPlan(t *testing.T) {
	cwd := t.TempDir()

	t.Run("NonDefaultBranchPlan", func(t *testing.T) {
		plan, err := resolveBranchPushPlan(context.Background(), &bytes.Buffer{}, cwd, "", featureBranchOptions{}, appDeps{
			isDefaultBranch: func(ctx context.Context, options zerogit.DefaultBranchOptions) (bool, string, string, error) {
				return false, "feat/my-feature", "origin", nil
			},
			remoteHasBranch: func(ctx context.Context, cwd, remote, branch string) (bool, error) {
				return false, nil
			},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if plan.ShouldBranch {
			t.Fatal("expected ShouldBranch to be false for non-default branch")
		}
		if plan.CurrentBranch != "feat/my-feature" || plan.PushRemote != "origin" || !plan.RequireNewRemoteBranch {
			t.Fatalf("unexpected plan values: %+v", plan)
		}
	})

	t.Run("DefaultBranchPlanWithResolvedCommit", func(t *testing.T) {
		plan, err := resolveBranchPushPlan(context.Background(), &bytes.Buffer{}, cwd, "upstream", featureBranchOptions{}, appDeps{
			isDefaultBranch: func(ctx context.Context, options zerogit.DefaultBranchOptions) (bool, string, string, error) {
				return true, "main", "upstream", nil
			},
			branchUpstreamRemoteAndMerge: func(ctx context.Context, cwd, branch string) (string, string) {
				return "origin", "main"
			},
			resolveRemoteBranchTip: func(ctx context.Context, cwd, remote, branch string) (string, error) {
				return "fedcba9876", nil
			},
			commitsAhead: func(ctx context.Context, cwd, remote, branch string) (int, error) {
				return 2, nil
			},
			refreshTrackingRef: func(ctx context.Context, cwd, remote, branch string) error {
				return nil
			},
			isUnbornRemote: func(ctx context.Context, cwd, remote string) (bool, error) {
				return false, nil
			},
			inspectChanges: featureBranchInspect([]zerogit.FileChange{{Path: "main.go", Status: "modified"}}, ""),
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !plan.ShouldBranch {
			t.Fatal("expected ShouldBranch to be true for ahead default branch")
		}
		if plan.CurrentBranch != "main" || plan.PushRemote != "upstream" {
			t.Fatalf("unexpected branch/remote: %s/%s", plan.CurrentBranch, plan.PushRemote)
		}
		if plan.TrackingRemote != "origin" || plan.TrackingBranch != "main" {
			t.Fatalf("unexpected tracking: %s/%s", plan.TrackingRemote, plan.TrackingBranch)
		}
		if plan.RestoreTip != "fedcba9876" || plan.PublishabilityBase != "fedcba9876" {
			t.Fatalf("unexpected restore tip / base: %s / %s", plan.RestoreTip, plan.PublishabilityBase)
		}
		if plan.Slug != "main-go" {
			t.Fatalf("unexpected slug: %s", plan.Slug)
		}
	})

	t.Run("RefusesDirtyWorkingTree", func(t *testing.T) {
		_, err := resolveBranchPushPlan(context.Background(), &bytes.Buffer{}, cwd, "", featureBranchOptions{}, appDeps{
			isDefaultBranch: func(ctx context.Context, options zerogit.DefaultBranchOptions) (bool, string, string, error) {
				return true, "main", "origin", nil
			},
			inspectChanges: func(ctx context.Context, options zerogit.InspectOptions) (zerogit.ChangeSummary, error) {
				return zerogit.ChangeSummary{Clean: false}, nil
			},
		})
		if err == nil || !strings.Contains(err.Error(), "uncommitted changes") {
			t.Fatalf("expected dirty working tree error, got %v", err)
		}
	})
}

// TestRunChangesPushRestoresDefaultBranchOnAutoBranchPath exercises the
// commit-then-push auto-branch path end-to-end through runChangesPush and
// asserts the default branch is restored to its remote tip after the feature
// branch is created.
func TestRunChangesPushRestoresDefaultBranchOnAutoBranchPath(t *testing.T) {
	cwd := t.TempDir()
	var resetBranch, resetTip string
	var pushedBranch string

	var stdout, stderr bytes.Buffer
	exitCode := runWithDeps([]string{"changes", "push"}, &stdout, &stderr, appDeps{
		getwd: func() (string, error) { return cwd, nil },
		isDefaultBranch: func(ctx context.Context, options zerogit.DefaultBranchOptions) (bool, string, string, error) {
			return true, "main", "origin", nil
		},
		commitsAhead:       func(ctx context.Context, cwd, remote, branch string) (int, error) { return 1, nil },
		refreshTrackingRef: func(ctx context.Context, cwd, remote, branch string) error { return nil },
		inspectChanges:     featureBranchInspect([]zerogit.FileChange{{Path: "README.md", Status: "modified"}}, ""),
		currentGitUser:     func(ctx context.Context, cwd string) string { return "Someone" },
		createBranch: func(ctx context.Context, options zerogit.BranchOptions) (zerogit.BranchResult, error) {
			return zerogit.BranchResult{Branch: options.Name}, nil
		},
		resetBranchRef: func(ctx context.Context, cwd, branch, newTip, expectedOld string) error {
			resetBranch = branch
			resetTip = newTip
			return nil
		},
		pushChanges: func(ctx context.Context, options zerogit.PushOptions) (zerogit.PushResult, error) {
			pushedBranch = options.Branch
			return zerogit.PushResult{Remote: "origin", Branch: options.Branch}, nil
		},
	})
	if exitCode != exitSuccess {
		t.Fatalf("expected exit code %d, got %d: %s", exitSuccess, exitCode, stderr.String())
	}
	if resetBranch != "main" || resetTip != "origin/main" {
		t.Fatalf("expected default branch restore main -> origin/main, got %q -> %q", resetBranch, resetTip)
	}
	if pushedBranch != "someone/readme-md" {
		t.Fatalf("expected push of auto-created branch, got %q", pushedBranch)
	}
}

// TestRunChangesPRYesUsesActualUpstreamForUnbornCheck covers jatmn's P2
// finding: with changes pr --yes and no --remote, the unborn-remote preflight
// must resolve the current branch's configured upstream (e.g. a fork's
// "upstream") rather than calling branchUpstreamRemote with an empty branch
// name and falling back to origin.
func TestRunChangesPRYesUsesActualUpstreamForUnbornCheck(t *testing.T) {
	cwd := t.TempDir()
	var unbornRemoteChecked string
	var upstreamBranchArg string
	isDefaultBranchCalled := false

	var stdout, stderr bytes.Buffer
	exitCode := runWithDeps([]string{"changes", "pr", "--yes", "--fill"}, &stdout, &stderr, appDeps{
		getwd: func() (string, error) { return cwd, nil },
		isDefaultBranch: func(ctx context.Context, options zerogit.DefaultBranchOptions) (bool, string, string, error) {
			isDefaultBranchCalled = true
			return false, "", "", errors.New("remote HEAD unreachable")
		},
		currentGitBranch: func(ctx context.Context, cwd string) string {
			return "feature/fork-work"
		},
		branchUpstreamRemote: func(ctx context.Context, cwd, branch string) string {
			upstreamBranchArg = branch
			if branch == "feature/fork-work" {
				return "upstream"
			}
			return ""
		},
		isUnbornRemote: func(ctx context.Context, cwd, remote string) (bool, error) {
			unbornRemoteChecked = remote
			return false, nil
		},
		pushChanges: func(ctx context.Context, options zerogit.PushOptions) (zerogit.PushResult, error) {
			return zerogit.PushResult{Remote: "upstream", Branch: "feature/fork-work"}, nil
		},
		createPR: func(ctx context.Context, options zerogit.PROptions) (zerogit.PRResult, error) {
			return zerogit.PRResult{Output: "https://example.invalid/pr/2"}, nil
		},
	})
	if exitCode != exitSuccess {
		t.Fatalf("expected exit code %d, got %d: %s", exitSuccess, exitCode, stderr.String())
	}
	if isDefaultBranchCalled {
		t.Fatal("expected isDefaultBranch not to be consulted under --yes")
	}
	if upstreamBranchArg != "feature/fork-work" {
		t.Fatalf("expected branchUpstreamRemote to receive current branch, got %q", upstreamBranchArg)
	}
	if unbornRemoteChecked != "upstream" {
		t.Fatalf("expected unborn check against upstream, got %q", unbornRemoteChecked)
	}
}

// TestRunChangesPRYesRejectsUnbornNonOriginUpstream: when the resolved
// upstream is unborn, the preflight must still block even under --yes.
func TestRunChangesPRYesRejectsUnbornNonOriginUpstream(t *testing.T) {
	cwd := t.TempDir()
	var stdout, stderr bytes.Buffer
	exitCode := runWithDeps([]string{"changes", "pr", "--yes", "--fill"}, &stdout, &stderr, appDeps{
		getwd: func() (string, error) { return cwd, nil },
		currentGitBranch: func(ctx context.Context, cwd string) string {
			return "feature/fork-work"
		},
		branchUpstreamRemote: func(ctx context.Context, cwd, branch string) string {
			return "upstream"
		},
		isUnbornRemote: func(ctx context.Context, cwd, remote string) (bool, error) {
			if remote != "upstream" {
				t.Fatalf("expected remote upstream, got %q", remote)
			}
			return true, nil
		},
		pushChanges: func(ctx context.Context, options zerogit.PushOptions) (zerogit.PushResult, error) {
			t.Fatal("pushChanges should not run on unborn upstream")
			return zerogit.PushResult{}, nil
		},
	})
	if exitCode != exitUsage {
		t.Fatalf("expected exit code %d, got %d: %s", exitUsage, exitCode, stderr.String())
	}
	if !strings.Contains(stderr.String(), "cannot create pull request on unborn remote upstream") {
		t.Fatalf("unexpected stderr: %q", stderr.String())
	}
}

// TestRunChangesBareRemotePushThenPRUsable is the end-to-end bare-remote
// regression for jatmn's unborn P1: auto-branch must not be the first remote
// ref. Establish main with --yes, then auto-branch a second commit and show
// that changes pr remains usable (push + createPR succeed against a remote
// that has a real default branch).
func TestRunChangesBareRemotePushThenPRUsable(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git unavailable: %v", err)
	}
	// The fixture relies on `git init --bare -b main` to point the bare
	// remote's HEAD at main; older git (before 2.28) lacks -b on init and
	// would leave the remote on master, failing the test for the wrong reason.
	requireGitAtLeast(t, 2, 28)

	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")

	tmp := t.TempDir()
	bare := filepath.Join(tmp, "remote.git")
	repo := filepath.Join(tmp, "repo")
	// -b main so remote HEAD points at the default branch GitHub/new repos use;
	// otherwise a bare repo's HEAD stays on master and IsDefaultBranch fails
	// closed after the first main push (dangling HEAD + existing heads).
	runWorkflowGit(t, tmp, "init", "--bare", "-b", "main", bare)
	runWorkflowGit(t, tmp, "init", repo)
	runWorkflowGit(t, repo, "config", "user.name", "Zero")
	runWorkflowGit(t, repo, "config", "user.email", "zero@example.invalid")
	runWorkflowGit(t, repo, "checkout", "-b", "main")
	writeWorkflowFile(t, filepath.Join(repo, "README.md"), "initial\n")
	runWorkflowGit(t, repo, "add", "README.md")
	runWorkflowGit(t, repo, "commit", "-m", "Initial commit")
	runWorkflowGit(t, repo, "remote", "add", "origin", bare)

	// Auto-branch on an unborn remote must refuse (not publish only a feature branch).
	var stdout, stderr bytes.Buffer
	exitCode := runWithDeps([]string{"changes", "push"}, &stdout, &stderr, appDeps{
		getwd: func() (string, error) { return repo, nil },
	})
	if exitCode == exitSuccess {
		t.Fatalf("expected unborn remote to refuse auto-branch, stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "no branches yet") {
		t.Fatalf("expected unborn refuse message, got stderr=%q", stderr.String())
	}

	// Establish the default branch.
	stdout.Reset()
	stderr.Reset()
	exitCode = runWithDeps([]string{"changes", "push", "--yes"}, &stdout, &stderr, appDeps{
		getwd: func() (string, error) { return repo, nil },
	})
	if exitCode != exitSuccess {
		t.Fatalf("push --yes failed: exit=%d stdout=%q stderr=%q", exitCode, stdout.String(), stderr.String())
	}
	heads := runWorkflowGit(t, bare, "show-ref", "--heads")
	if !strings.Contains(heads, "refs/heads/main") {
		t.Fatalf("expected bare remote to have main after --yes, got %q", heads)
	}

	// Second commit: auto-branch + push, then pr against an established remote.
	writeWorkflowFile(t, filepath.Join(repo, "feature.txt"), "work\n")
	runWorkflowGit(t, repo, "add", "feature.txt")
	runWorkflowGit(t, repo, "commit", "-m", "add feature")

	var pushedBranch string
	var prCalled bool
	stdout.Reset()
	stderr.Reset()
	deps := defaultAppDeps()
	deps.getwd = func() (string, error) { return repo, nil }
	deps.currentGitUser = func(ctx context.Context, cwd string) string { return "Someone" }
	basePush := deps.pushChanges
	deps.pushChanges = func(ctx context.Context, options zerogit.PushOptions) (zerogit.PushResult, error) {
		pushedBranch = options.Branch
		return basePush(ctx, options)
	}
	deps.createPR = func(ctx context.Context, options zerogit.PROptions) (zerogit.PRResult, error) {
		prCalled = true
		return zerogit.PRResult{Output: "https://example.invalid/pr/1"}, nil
	}

	exitCode = runWithDeps([]string{"changes", "push"}, &stdout, &stderr, deps)
	if exitCode != exitSuccess {
		t.Fatalf("auto-branch push failed: exit=%d stdout=%q stderr=%q", exitCode, stdout.String(), stderr.String())
	}
	if pushedBranch == "" || pushedBranch == "main" {
		t.Fatalf("expected auto-created feature branch push, got %q", pushedBranch)
	}
	heads = runWorkflowGit(t, bare, "show-ref", "--heads")
	if !strings.Contains(heads, "refs/heads/main") {
		t.Fatalf("expected main to remain on bare remote, got %q", heads)
	}
	if !strings.Contains(heads, "refs/heads/"+pushedBranch) {
		t.Fatalf("expected feature branch %q on bare remote, got %q", pushedBranch, heads)
	}
	// The feature branch must exclusively own the new commit: local main goes
	// back to its upstream tip so it does not diverge after a squash-merge.
	localMain := strings.TrimSpace(runWorkflowGit(t, repo, "rev-parse", "refs/heads/main"))
	remoteMain := strings.TrimSpace(runWorkflowGit(t, repo, "rev-parse", "refs/remotes/origin/main"))
	if localMain != remoteMain {
		t.Fatalf("expected local main restored to origin/main, got %s vs %s", localMain, remoteMain)
	}

	stdout.Reset()
	stderr.Reset()
	exitCode = runWithDeps([]string{"changes", "pr", "--fill"}, &stdout, &stderr, deps)
	if exitCode != exitSuccess {
		t.Fatalf("changes pr failed after established remote: exit=%d stdout=%q stderr=%q", exitCode, stdout.String(), stderr.String())
	}
	if !prCalled {
		t.Fatal("expected createPR to run once the default branch exists")
	}
}

func runWorkflowGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	ctx := context.Background()
	if dl, ok := t.Deadline(); ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, dl)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, string(out))
	}
	return string(out)
}

// requireGitAtLeast skips the test when the installed Git predates the given
// version, for tests that rely on newer git features (for example
// `git init --bare -b main`, added in 2.28).
func requireGitAtLeast(t *testing.T, wantMajor, wantMinor int) {
	t.Helper()
	out, err := exec.Command("git", "--version").Output()
	if err != nil {
		t.Skipf("cannot determine git version: %v", err)
	}
	version := strings.TrimSpace(string(out))
	rest, ok := strings.CutPrefix(version, "git version ")
	if !ok {
		t.Skipf("cannot parse git version %q", version)
	}
	var major, minor int
	if n, err := fmt.Sscanf(rest, "%d.%d", &major, &minor); err != nil || n != 2 {
		t.Skipf("cannot parse git version %q", version)
	}
	if major < wantMajor || (major == wantMajor && minor < wantMinor) {
		t.Skipf("git %d.%d+ required, have %s", wantMajor, wantMinor, version)
	}
}

func writeWorkflowFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
