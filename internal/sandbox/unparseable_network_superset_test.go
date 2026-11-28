package sandbox

import "testing"

// THE UNPARSEABLE FALLBACK MUST BE A SUPERSET OF WHAT THE AST PATH FLAGS.
//
// The POSIX parser rejects shell syntax the invoked shell accepts, and the
// analyzer names Windows command strings as exactly that fallback case. When a
// command form loses its network category by being written in a spelling the
// parser cannot read, the approval gate disappears for it — and on Windows that
// approval IS the egress control, because there is no namespace or seatbelt
// underneath it.
//
// The serving forms were briefly dropped from the fallback on the reasoning that
// they bind rather than fetch. The AST path sets Network for them anyway, and
// says why: nothing consumes LocalServer yet, and `npm run dev` is matched by
// script name, so the repository decides what `dev` and `predev` do and either
// can reach out before a port is bound.
func TestUnparseableFallbackKeepsNetworkForServingCommands(t *testing.T) {
	// Each pair is the same intent written two ways: one the POSIX parser reads,
	// one it does not. Both must be classified the same.
	for _, testCase := range []struct{ name, parseable, unparseable string }{
		{
			name:        "package manager dev script",
			parseable:   `npm run dev`,
			unparseable: `if "%OS%"=="Windows_NT" (npm run dev) else (npm start)`,
		},
		{
			// The flagged spelling is the one that regressed: the AST path lost it
			// to option-value confusion and the fallback only ever listed the
			// serving word directly after the program name, so BOTH paths dropped
			// the gate for the same command at the same time.
			name:        "package manager dev script behind a global option",
			parseable:   `npm --prefix ./web run dev`,
			unparseable: `if "%OS%"=="Windows_NT" (npm --prefix ./web run dev) else (npm --prefix ./web start)`,
		},
		{
			// The fetching half of the same confusion: `npm --prefix ./web install`
			// really does reach the registry, and it lost the gate on both paths for
			// the same reason the serving form did.
			name:        "package manager install behind a global option",
			parseable:   `npm --prefix ./web install`,
			unparseable: `if "%OS%"=="Windows_NT" (npm --prefix ./web install) else (true)`,
		},
		{
			name:        "python http server",
			parseable:   `python -m http.server 8000`,
			unparseable: `if "%OS%"=="Windows_NT" (python -m http.server 8000) else (true)`,
		},
		{
			name:        "framework dev server",
			parseable:   `vite --host`,
			unparseable: `if "%OS%"=="Windows_NT" (vite --host) else (true)`,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			// The premise: the first parses and the second does not. If the parser
			// ever learns this syntax, this test is measuring nothing and should
			// fail loudly rather than pass vacuously.
			if AnalyzeCommand(testCase.parseable).TooComplex {
				t.Fatalf("SETUP INVALID: %q was expected to parse", testCase.parseable)
			}
			if !AnalyzeCommand(testCase.unparseable).TooComplex {
				t.Fatalf("SETUP INVALID: %q was expected to defeat the parser", testCase.unparseable)
			}

			for _, script := range []string{testCase.parseable, testCase.unparseable} {
				risk := Classify(Request{
					ToolName:   "bash",
					SideEffect: SideEffectShell,
					Args:       map[string]any{"command": script},
				})
				if !HasRiskCategory(risk, "network") {
					t.Errorf("%q was not classified as network, so it skips the NetworkDeny prompt that its parseable form receives: %v", script, risk.Categories)
				}
			}
		})
	}
}

// THE FALLBACK IS DERIVED FROM THE ANALYZER, NOT MAINTAINED BESIDE IT.
//
// The unparseable fallback is meant to be a conservative SUPERSET of everything
// the AST path flags for network. It was maintained as a separate inventory and
// drifted: the analyzer accepted "py" as a Python launcher, the regex listed
// only python, python2 and python3, and a Windows batch spelling the POSIX
// parser rejects therefore lost the approval gate that its parseable equivalent
// receives. On Windows that gate IS the egress control.
//
// Driving the loop off pythonLauncherPrograms means a launcher added there
// cannot silently weaken the fallback: this fails the moment the two disagree.
// Fetch subcommands are one inventory. The AST path flags ci, create, view,
// search, and the rest of packageManagerNetworkSubcommands. The fallback used
// to list a shorter set, so a Windows conditional the POSIX parser rejects
// lost the network category the parseable spelling still had.
func TestUnparseableFallbackCoversPackageManagerFetchSubcommands(t *testing.T) {
	if len(packageManagerNetworkSubcommands) == 0 {
		t.Fatal("SETUP INVALID: no package-manager fetch subcommands")
	}
	type commandPair struct {
		name        string
		parseable   string
		unparseable string
	}
	cases := make([]commandPair, 0, len(packageManagerNetworkSubcommands)+len(packageManagerNetworkLiteralAliases))
	for subcommand := range packageManagerNetworkSubcommands {
		parseable := "npm --prefix ./web " + subcommand
		cases = append(cases, commandPair{
			name:        "npm " + subcommand,
			parseable:   parseable,
			unparseable: `if "%OS%"=="Windows_NT" (` + parseable + ") else (true)",
		})
	}
	for _, alias := range packageManagerNetworkLiteralAliases {
		// The alias is rewritten before the AST switch, so the program under
		// test has to be one that actually owns it. bun maps x to exec.
		parseable := "bun --cwd ./web " + alias + " cowsay"
		cases = append(cases, commandPair{
			name:        "bun " + alias,
			parseable:   parseable,
			unparseable: `if "%OS%"=="Windows_NT" (` + parseable + ") else (true)",
		})
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if AnalyzeCommand(testCase.parseable).TooComplex {
				t.Fatalf("SETUP INVALID: %q was expected to parse", testCase.parseable)
			}
			if !AnalyzeCommand(testCase.unparseable).TooComplex {
				t.Fatalf("SETUP INVALID: %q was expected to defeat the parser", testCase.unparseable)
			}
			for _, script := range []string{testCase.parseable, testCase.unparseable} {
				risk := Classify(Request{
					ToolName:   "bash",
					SideEffect: SideEffectShell,
					Args:       map[string]any{"command": script},
				})
				if !HasRiskCategory(risk, "network") {
					t.Errorf("%q was not classified as network: %v", script, risk.Categories)
				}
			}
		})
	}
}

func TestUnparseableFallbackCoversEveryPythonLauncher(t *testing.T) {
	if len(pythonLauncherPrograms) == 0 {
		t.Fatal("SETUP INVALID: no python launchers to check")
	}
	for launcher := range pythonLauncherPrograms {
		t.Run(launcher, func(t *testing.T) {
			parseable := launcher + " -m http.server 8000"
			unparseable := `if "%OS%"=="Windows_NT" (` + parseable + ") else (true)"

			if AnalyzeCommand(parseable).TooComplex {
				t.Fatalf("SETUP INVALID: %q was expected to parse", parseable)
			}
			if !AnalyzeCommand(unparseable).TooComplex {
				t.Fatalf("SETUP INVALID: %q was expected to defeat the parser", unparseable)
			}

			for _, script := range []string{parseable, unparseable} {
				risk := Classify(Request{
					ToolName:   "bash",
					SideEffect: SideEffectShell,
					Args:       map[string]any{"command": script},
				})
				if !HasRiskCategory(risk, "network") {
					t.Errorf("%q was not classified as network, so it skips the approval its parseable form receives: %v", script, risk.Categories)
				}
			}
		})
	}
}

func TestPackageManagerOptionsPreserveNetworkApproval(t *testing.T) {
	engine := NewEngine(EngineOptions{Policy: Policy{Mode: ModeEnforce, Network: NetworkDeny}})
	for _, testCase := range []struct {
		name        string
		command     string
		wantNetwork bool
	}{
		{"double quoted fetch", `npm --prefix "./web app" ci`, true},
		{"quoted Windows path", `npm --prefix "C:\work\web app" ci`, true},
		{"single quoted fetch", `npm --prefix './web app' ci`, true},
		{"escaped space", `npm --prefix ./web\ app ci`, true},
		{"attached quoted value", `npm --prefix="./web app folder" ci`, true},
		{"quoted value before serving", `pnpm --silent -C "./web app" run dev`, true},
		{"valueless flag", `npm --silent ci`, true},
		{"local version check", `npm --prefix "./web app" --version`, false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			unparseable := `if "%OS%"=="Windows_NT" (` + testCase.command + ") else (true)"
			if AnalyzeCommand(testCase.command).TooComplex || !AnalyzeCommand(unparseable).TooComplex {
				t.Fatal("expected the standalone command to parse and the conditional to use the fallback")
			}
			for _, command := range []string{testCase.command, unparseable} {
				decision := engine.Evaluate(t.Context(), Request{
					ToolName:          "bash",
					SideEffect:        SideEffectShell,
					PermissionGranted: true,
					Args:              map[string]any{"command": command},
				})
				network := HasRiskCategory(decision.Risk, "network")
				prompt := decision.Action == ActionPrompt && decision.Reason == ReasonNetworkBlocked
				if network != testCase.wantNetwork || prompt != testCase.wantNetwork {
					t.Errorf("%q: network=%v, decision=%s (%s), want network approval=%v", command, network, decision.Action, decision.Reason, testCase.wantNetwork)
				}
			}
		})
	}
}
