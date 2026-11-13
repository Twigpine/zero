package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/Twigpine/zero/internal/execprofile"
	"github.com/Twigpine/zero/internal/perfbench"
)

// turnOptions configures the `zero-perf-bench turn` subcommand: the per-turn
// benchmark harness that runs ZERO headlessly with --trace, parses each turn's
// NDJSON trace, and records per-span latency plus the top controllable latency
// sources — the Phase 0 baseline's "do not proceed until" criterion.
type turnOptions struct {
	SuitePath   string
	Model       string
	Mode        string
	ExecProfile string
	SelfCorrect bool
	Binary      string
	Iterations  int
	Version     string
	Commit      string
	Output      string
	JSON        bool
	DryRun      bool
	Help        bool
}

func runTurnCommand(args []string, getenv func(string) string, stdout io.Writer, stderr io.Writer) int {
	options, err := parseTurnArgs(args, getenv)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err.Error())
		return 2
	}
	if options.Help {
		_, _ = fmt.Fprint(stdout, turnHelpText())
		return 0
	}

	set, err := perfbench.LoadTaskSet(options.SuitePath)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "[zero] Turn benchmark failed: "+err.Error())
		return 1
	}

	// The dry-run path records a zero-iteration run without a binary, so the
	// manifest loads and the report path is exercised in CI without a model.
	if options.DryRun {
		_, _ = fmt.Fprintln(stdout, "[zero] turn benchmark: dry run (no agent invoked)")
		return 0
	}

	binary, err := perfbench.ResolveBinary(options.Binary)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "[zero] Turn benchmark failed: "+err.Error())
		return 2
	}

	result, err := perfbench.RunTurnBench(context.Background(), set, perfbench.TurnBenchConfig{
		Model:       options.Model,
		Mode:        options.Mode,
		ExecProfile: options.ExecProfile,
		SelfCorrect: options.SelfCorrect,
		Version:     options.Version,
		Commit:      options.Commit,
		Iterations:  options.Iterations,
		Runner:      perfbench.NewTurnExecRunner(binary),
	})
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "[zero] Turn benchmark failed: "+err.Error())
		return 1
	}

	if options.Output != "" {
		if err := writeTurnReport(options.Output, result); err != nil {
			_, _ = fmt.Fprintln(stderr, "[zero] Turn benchmark failed: "+err.Error())
			return 1
		}
	}
	if options.JSON {
		if err := perfbench.WriteTurnBenchJSON(stdout, result); err != nil {
			_, _ = fmt.Fprintln(stderr, "[zero] Turn benchmark failed: "+err.Error())
			return 1
		}
		return turnExitCode(result, stderr)
	}
	_, _ = fmt.Fprintln(stdout, perfbench.FormatTurnBenchSummary(result))
	return turnExitCode(result, stderr)
}

// turnExitCode fails the command when every attempted task errored (no
// iteration produced an accepted benchmark sample): such a report measures
// nothing, and exiting 0 would let a broken configuration (missing binary, bad
// path, failing harness step) pass as a clean baseline. Partial errors keep
// exit 0 — the summary surfaces them loudly and the surviving samples are
// still valid measurements.
func turnExitCode(result perfbench.TurnBenchResult, stderr io.Writer) int {
	if result.TasksAttempted > 0 && result.TasksErrored == result.TasksAttempted {
		_, _ = fmt.Fprintln(stderr, "[zero] Turn benchmark failed: every task errored with no accepted benchmark sample (see warnings); the report contains no valid measurements")
		return 1
	}
	return 0
}

func parseTurnArgs(args []string, getenv func(string) string) (turnOptions, error) {
	options := turnOptions{
		Iterations: 1,
		Version:    strings.TrimSpace(getenv("ZERO_BENCH_VERSION")),
		Commit:     strings.TrimSpace(getenv("ZERO_BENCH_COMMIT")),
	}
	for index := 0; index < len(args); index++ {
		arg := args[index]
		flag, inlineValue := splitFlagValue(arg)
		switch flag {
		case "--suite":
			value, next, err := readOptionValue(args, inlineValue, index, flag)
			if err != nil {
				return options, err
			}
			options.SuitePath = value
			index = next
		case "--model":
			value, next, err := readOptionValue(args, inlineValue, index, flag)
			if err != nil {
				return options, err
			}
			options.Model = value
			index = next
		case "--mode":
			value, next, err := readOptionValue(args, inlineValue, index, flag)
			if err != nil {
				return options, err
			}
			options.Mode = value
			index = next
		case "--exec-profile":
			value, next, err := readOptionValue(args, inlineValue, index, flag)
			if err != nil {
				return options, err
			}
			options.ExecProfile = value
			index = next
		case "--binary":
			value, next, err := readOptionValue(args, inlineValue, index, flag)
			if err != nil {
				return options, err
			}
			options.Binary = value
			index = next
		case "--iterations":
			value, next, err := readOptionValue(args, inlineValue, index, flag)
			if err != nil {
				return options, err
			}
			parsed, err := parsePositiveInteger(flag, value)
			if err != nil {
				return options, err
			}
			options.Iterations = parsed
			index = next
		case "--version":
			value, next, err := readOptionValue(args, inlineValue, index, flag)
			if err != nil {
				return options, err
			}
			options.Version = value
			index = next
		case "--commit":
			value, next, err := readOptionValue(args, inlineValue, index, flag)
			if err != nil {
				return options, err
			}
			options.Commit = value
			index = next
		case "--output":
			value, next, err := readOptionValue(args, inlineValue, index, flag)
			if err != nil {
				return options, err
			}
			options.Output = value
			index = next
		case "--self-correct":
			if strings.Contains(arg, "=") {
				return options, fmt.Errorf("%s does not accept a value", flag)
			}
			options.SelfCorrect = true
		case "--json":
			if strings.Contains(arg, "=") {
				return options, fmt.Errorf("%s does not accept a value", flag)
			}
			options.JSON = true
		case "--dry-run":
			if strings.Contains(arg, "=") {
				return options, fmt.Errorf("%s does not accept a value", flag)
			}
			options.DryRun = true
		case "-h", "--help":
			if strings.Contains(arg, "=") {
				return options, fmt.Errorf("%s does not accept a value", flag)
			}
			options.Help = true
		default:
			return options, fmt.Errorf("unknown option: %s", arg)
		}
	}
	if options.Help {
		return options, nil
	}
	if strings.TrimSpace(options.SuitePath) == "" {
		return options, fmt.Errorf("--suite is required")
	}
	if strings.TrimSpace(options.Model) == "" && !options.DryRun {
		return options, fmt.Errorf("--model is required (or pass --dry-run)")
	}
	// Validate the profile here, before anything spawns: an unknown name would
	// otherwise make every child exit with a usage error in milliseconds, and
	// those near-zero walls would be recorded as valid latency samples in an
	// exit-0 report — exactly the misread-as-improvement trap the harness
	// closed for spawn failures. Normalizing to the catalog name also keeps
	// the stamped execProfile canonical (Lookup is case-insensitive), so two
	// captures of the same posture always compare equal.
	if raw := strings.TrimSpace(options.ExecProfile); raw != "" {
		profile, ok := execprofile.Lookup(raw)
		if !ok {
			return options, fmt.Errorf("unknown execution profile %q for --exec-profile. Valid profiles: %s", raw, strings.Join(execprofile.Names(), ", "))
		}
		options.ExecProfile = profile.Name
	}
	return options, nil
}

func writeTurnReport(path string, result perfbench.TurnBenchResult) error {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	var buffer bytes.Buffer
	if err := perfbench.WriteTurnBenchJSON(&buffer, result); err != nil {
		return err
	}
	return os.WriteFile(path, buffer.Bytes(), 0o644)
}

func turnHelpText() string {
	return strings.Join([]string{
		"Usage: zero-perf-bench turn [options]",
		"",
		"Runs ZERO headlessly with --trace against a per-turn benchmark task set and",
		"records per-span latency plus the top controllable latency sources (the",
		"Phase 0 baseline's \"do not proceed until\" criterion). Each task is a fresh",
		"`zero exec` process, so iterations are cold-start samples; a warm path needs",
		"an in-process runner (future work).",
		"",
		"Options:",
		"  --suite <path>      Task set JSON file (required)",
		"  --model <model>     Model to run (required unless --dry-run)",
		"  --mode <name>       Exec mode preset to apply",
		"  --exec-profile <name>",
		"                      Execution profile for every task (fast|balanced|thorough);",
		"                      forwarded to zero exec and stamped into the result",
		"  --self-correct      Enable the post-edit verify-and-correct loop",
		"  --binary <path>     Path to the `zero` binary (default: zero on PATH / repo root)",
		"  --iterations <n>    Times to run each task (default: 1)",
		"  --version <v>       Record the ZERO version (default: $ZERO_BENCH_VERSION)",
		"  --commit <sha>      Record the ZERO commit (default: $ZERO_BENCH_COMMIT)",
		"  --output <path>     Write the JSON result to path",
		"  --json              Print only the JSON result",
		"  --dry-run           Load the manifest and exit without invoking the agent",
		"  -h, --help          Show this help",
	}, "\n") + "\n"
}
