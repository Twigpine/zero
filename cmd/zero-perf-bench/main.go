package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Twigpine/zero/internal/perfbench"
)

const (
	defaultIterations       = 5
	defaultWarmupIterations = 1
)

type cliOptions struct {
	perfbench.Options
	CI            bool
	JSON          bool
	Output        string
	FailOnWarning bool
	Help          bool
}

func main() {
	os.Exit(run(os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

func run(args []string, getenv func(string) string, stdout io.Writer, stderr io.Writer) int {
	// The `tasks` subcommand is the reproducible Terminal-Bench-style task harness;
	// the default (no subcommand) path stays the cold-start/RSS perf benchmark so
	// existing invocations are unchanged.
	if len(args) > 0 && args[0] == "tasks" {
		return runTasksCommand(args[1:], getenv, stdout, stderr)
	}
	if len(args) > 0 && args[0] == "turn" {
		return runTurnCommand(args[1:], getenv, stdout, stderr)
	}
	options, err := parseArgs(args, getenv)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err.Error())
		return 2
	}
	if options.Help {
		_, _ = fmt.Fprint(stdout, helpText())
		return 0
	}
	result, err := perfbench.Run(context.Background(), options.Options)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "[zero] Performance benchmark failed: "+err.Error())
		return 1
	}
	if options.Output != "" {
		if err := writeReport(options.Output, result); err != nil {
			_, _ = fmt.Fprintln(stderr, "[zero] Performance benchmark failed: "+err.Error())
			return 1
		}
	}
	if options.JSON {
		if err := perfbench.WriteJSON(stdout, result); err != nil {
			_, _ = fmt.Fprintln(stderr, "[zero] Performance benchmark failed: "+err.Error())
			return 1
		}
	} else {
		_, _ = fmt.Fprintln(stdout, perfbench.FormatSummary(result))
	}
	if options.CI {
		perfbench.EmitWarnings(stderr, result)
	}
	if options.FailOnWarning && len(result.Warnings) > 0 {
		return 1
	}
	return 0
}

func parseArgs(args []string, getenv func(string) string) (cliOptions, error) {
	iterations, err := readPositiveIntegerEnv(getenv, "ZERO_PERF_ITERATIONS", defaultIterations)
	if err != nil {
		return cliOptions{}, err
	}
	warmupIterations, err := readNonNegativeIntegerEnv(getenv, "ZERO_PERF_WARMUP_ITERATIONS", defaultWarmupIterations)
	if err != nil {
		return cliOptions{}, err
	}
	coldStartP95Ms, err := readPositiveNumberEnv(getenv, "ZERO_PERF_COLD_START_WARN_MS", perfbench.DefaultThresholds.ColdStartP95Ms)
	if err != nil {
		return cliOptions{}, err
	}
	firstOutputP95Ms, err := readPositiveNumberEnv(getenv, "ZERO_PERF_FIRST_OUTPUT_WARN_MS", perfbench.DefaultThresholds.FirstOutputP95Ms)
	if err != nil {
		return cliOptions{}, err
	}
	harnessEndRssMaxMb, err := readPositiveNumberEnv(getenv, "ZERO_PERF_HARNESS_END_RSS_WARN_MB", perfbench.DefaultThresholds.HarnessEndRssMaxMb)
	if err != nil {
		return cliOptions{}, err
	}
	options := cliOptions{
		Options: perfbench.Options{
			Iterations:       iterations,
			WarmupIterations: warmupIterations,
			Thresholds: perfbench.Thresholds{
				ColdStartP95Ms:     coldStartP95Ms,
				FirstOutputP95Ms:   firstOutputP95Ms,
				HarnessEndRssMaxMb: harnessEndRssMaxMb,
			},
		},
		CI: getenv("GITHUB_ACTIONS") == "true",
	}
	for index := 0; index < len(args); index++ {
		arg := args[index]
		flag, inlineValue := splitFlagValue(arg)
		switch flag {
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
		case "--warmup":
			value, next, err := readOptionValue(args, inlineValue, index, flag)
			if err != nil {
				return options, err
			}
			parsed, err := parseNonNegativeInteger(flag, value)
			if err != nil {
				return options, err
			}
			options.WarmupIterations = parsed
			index = next
		case "--cold-start-warn-ms":
			value, next, err := readOptionValue(args, inlineValue, index, flag)
			if err != nil {
				return options, err
			}
			parsed, err := parsePositiveNumber(flag, value)
			if err != nil {
				return options, err
			}
			options.Thresholds.ColdStartP95Ms = parsed
			index = next
		case "--first-output-warn-ms":
			value, next, err := readOptionValue(args, inlineValue, index, flag)
			if err != nil {
				return options, err
			}
			parsed, err := parsePositiveNumber(flag, value)
			if err != nil {
				return options, err
			}
			options.Thresholds.FirstOutputP95Ms = parsed
			index = next
		case "--harness-end-rss-warn-mb":
			value, next, err := readOptionValue(args, inlineValue, index, flag)
			if err != nil {
				return options, err
			}
			parsed, err := parsePositiveNumber(flag, value)
			if err != nil {
				return options, err
			}
			options.Thresholds.HarnessEndRssMaxMb = parsed
			index = next
		case "--output":
			value, next, err := readOptionValue(args, inlineValue, index, flag)
			if err != nil {
				return options, err
			}
			options.Output = value
			index = next
		case "--ci":
			if strings.Contains(arg, "=") {
				return options, fmt.Errorf("%s does not accept a value", flag)
			}
			options.CI = true
		case "--json":
			if strings.Contains(arg, "=") {
				return options, fmt.Errorf("%s does not accept a value", flag)
			}
			options.JSON = true
		case "--fail-on-warning":
			if strings.Contains(arg, "=") {
				return options, fmt.Errorf("%s does not accept a value", flag)
			}
			options.FailOnWarning = true
		case "-h", "--help":
			if strings.Contains(arg, "=") {
				return options, fmt.Errorf("%s does not accept a value", flag)
			}
			options.Help = true
		default:
			return options, fmt.Errorf("unknown option: %s", arg)
		}
	}
	return options, nil
}

func helpText() string {
	return strings.Join([]string{
		"Usage: zero-perf-bench [options]",
		"       zero-perf-bench tasks [options]   (Terminal-Bench-style task harness; see `tasks --help`)",
		"       zero-perf-bench turn  [options]   (per-turn tracing benchmark; see `turn --help`)",
		"",
		"Options:",
		"  --iterations <n>             Measured samples to collect (default: 5)",
		"  --warmup <n>                 Warmup samples to discard (default: 1)",
		"  --cold-start-warn-ms <n>     Warn when cold-start p95 is above n ms",
		"  --first-output-warn-ms <n>   Warn when binary first-output p95 is above n ms",
		"  --harness-end-rss-warn-mb <n>",
		"                               Warn when harness end-state RSS is above n MB",
		"  --output <path>              Write the JSON report to path",
		"  --json                       Print only the JSON report",
		"  --ci                         Emit GitHub Actions warning annotations",
		"  --fail-on-warning            Exit non-zero when thresholds are exceeded",
		"  -h, --help                   Show this help",
		"",
		"Environment overrides:",
		"  ZERO_PERF_ITERATIONS, ZERO_PERF_WARMUP_ITERATIONS",
		"  ZERO_PERF_COLD_START_WARN_MS, ZERO_PERF_FIRST_OUTPUT_WARN_MS",
		"  ZERO_PERF_HARNESS_END_RSS_WARN_MB",
	}, "\n") + "\n"
}

func writeReport(path string, result perfbench.Result) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	var buffer bytes.Buffer
	if err := perfbench.WriteJSON(&buffer, result); err != nil {
		return err
	}
	return os.WriteFile(path, buffer.Bytes(), 0o644)
}

func splitFlagValue(arg string) (string, string) {
	flag, value, ok := strings.Cut(arg, "=")
	if !ok {
		return arg, ""
	}
	return flag, value
}

func readOptionValue(args []string, inlineValue string, index int, flag string) (string, int, error) {
	if inlineValue != "" {
		return inlineValue, index, nil
	}
	if strings.Contains(args[index], "=") {
		return "", index, fmt.Errorf("%s requires a value", flag)
	}
	next := index + 1
	if next >= len(args) || strings.HasPrefix(args[next], "--") {
		return "", index, fmt.Errorf("%s requires a value", flag)
	}
	return args[next], next, nil
}

func readPositiveIntegerEnv(getenv func(string) string, name string, fallback int) (int, error) {
	value := getenv(name)
	if value == "" {
		return fallback, nil
	}
	parsed, err := parsePositiveInteger(name, value)
	if err != nil {
		return 0, err
	}
	return parsed, nil
}

func readNonNegativeIntegerEnv(getenv func(string) string, name string, fallback int) (int, error) {
	value := getenv(name)
	if value == "" {
		return fallback, nil
	}
	parsed, err := parseNonNegativeInteger(name, value)
	if err != nil {
		return 0, err
	}
	return parsed, nil
}

func readPositiveNumberEnv(getenv func(string) string, name string, fallback float64) (float64, error) {
	value := getenv(name)
	if value == "" {
		return fallback, nil
	}
	parsed, err := parsePositiveNumber(name, value)
	if err != nil {
		return 0, err
	}
	return parsed, nil
}

func parsePositiveInteger(name string, value string) (int, error) {
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 1 {
		return 0, fmt.Errorf("%s must be a positive integer", name)
	}
	return parsed, nil
}

func parseNonNegativeInteger(name string, value string) (int, error) {
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 0 {
		return 0, fmt.Errorf("%s must be a non-negative integer", name)
	}
	return parsed, nil
}

func parsePositiveNumber(name string, value string) (float64, error) {
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) || parsed <= 0 {
		return 0, fmt.Errorf("%s must be a positive number", name)
	}
	return parsed, nil
}
