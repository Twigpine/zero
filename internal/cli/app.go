package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Gitlawb/zero/internal/agent"
	"github.com/Gitlawb/zero/internal/config"
	"github.com/Gitlawb/zero/internal/execution"
	"github.com/Gitlawb/zero/internal/hooks"
	"github.com/Gitlawb/zero/internal/localcontrol"
	"github.com/Gitlawb/zero/internal/mcp"
	"github.com/Gitlawb/zero/internal/modelregistry"
	"github.com/Gitlawb/zero/internal/observability"
	"github.com/Gitlawb/zero/internal/peermsg"
	"github.com/Gitlawb/zero/internal/plugins"
	"github.com/Gitlawb/zero/internal/providerhealth"
	"github.com/Gitlawb/zero/internal/providermodeldiscovery"
	"github.com/Gitlawb/zero/internal/provideroauth"
	"github.com/Gitlawb/zero/internal/provideronboarding"
	"github.com/Gitlawb/zero/internal/providers"
	"github.com/Gitlawb/zero/internal/redaction"
	"github.com/Gitlawb/zero/internal/sandbox"
	"github.com/Gitlawb/zero/internal/selfverify"
	"github.com/Gitlawb/zero/internal/sessions"
	"github.com/Gitlawb/zero/internal/skills"
	"github.com/Gitlawb/zero/internal/specialist"
	"github.com/Gitlawb/zero/internal/swarm"
	"github.com/Gitlawb/zero/internal/tools"
	"github.com/Gitlawb/zero/internal/tui"
	"github.com/Gitlawb/zero/internal/update"
	"github.com/Gitlawb/zero/internal/verify"
	"github.com/Gitlawb/zero/internal/worktrees"
	"github.com/Gitlawb/zero/internal/zerogit"
	"github.com/Gitlawb/zero/internal/zeroruntime"
	"github.com/charmbracelet/x/term"
)

var version = "dev"

type appDeps struct {
	getwd            func() (string, error)
	stdin            io.Reader
	userConfigPath   func() (string, error)
	resolveConfig    func(workspaceRoot string, overrides config.Overrides) (config.ResolvedConfig, error)
	resolveMCPConfig func(workspaceRoot string, excludeProject bool) (config.MCPConfig, error)
	newProvider      func(config.ProviderProfile) (zeroruntime.Provider, error)
	// exportActiveProvider pins spawned children to the run's provider (production:
	// config.SetActiveProviderEnv, set in defaultAppDeps — deliberately NOT filled
	// by fillAppDeps, so tests never mutate the process environment unless they
	// inject it). nil ⇒ no export.
	exportActiveProvider func(providerName string)
	// getenv reads a process environment variable (production: os.Getenv, set in
	// defaultAppDeps — deliberately NOT filled by fillAppDeps, so tests are hermetic
	// against ambient vars like ZERO_PROVIDER unless they inject it). nil ⇒ empty.
	getenv                       func(string) string
	probeProviderHealth          func(context.Context, providerhealth.Options) providerhealth.Result
	discoverProviderModels       func(context.Context, config.ProviderProfile) ([]providermodeldiscovery.Model, error)
	detectLocalRuntimes          func(context.Context, provideronboarding.LocalDetectOptions) []provideronboarding.DetectedLocalRuntime
	openRouterLogin              func(context.Context, provideroauth.OpenRouterOptions) (string, error)
	newSessionStore              func() *sessions.Store
	loadPlugins                  func(plugins.LoadOptions) (plugins.LoadResult, error)
	loadHooks                    func(hooks.LoadOptions) (hooks.LoadResult, error)
	skillsDir                    func() string
	pluginsDir                   func() string
	toolsDir                     func() string
	newMCPStore                  func() (*mcp.PermissionStore, error)
	newMCPTokenStore             func() (*mcp.TokenStore, error)
	newSandboxStore              func() (*sandbox.GrantStore, error)
	selectSandboxBackend         func(sandbox.BackendOptions) sandbox.Backend
	runSandboxSetupHelper        func(path string, args []string, stdout io.Writer, stderr io.Writer) error
	registerMCPTools             func(context.Context, *tools.Registry, config.MCPConfig, mcp.RegisterOptions) (mcpToolRuntime, error)
	prepareWorktree              func(context.Context, worktrees.Options) (worktrees.Result, error)
	releaseWorktree              func(context.Context, worktrees.Options, string) error
	detectVerifyPlan             func(string) (verify.Plan, error)
	runVerify                    func(context.Context, verify.Plan, verify.RunOptions) verify.Report
	runSelfVerify                func(context.Context, verify.Plan, selfverify.Options) selfverify.Report
	runAgentEval                 func(context.Context, agentEvalOptions) (agentEvalReport, error)
	inspectChanges               func(context.Context, zerogit.InspectOptions) (zerogit.ChangeSummary, error)
	commitChanges                func(context.Context, zerogit.CommitOptions) (zerogit.CommitResult, error)
	pushChanges                  func(context.Context, zerogit.PushOptions) (zerogit.PushResult, error)
	createPR                     func(context.Context, zerogit.PROptions) (zerogit.PRResult, error)
	createBranch                 func(context.Context, zerogit.BranchOptions) (zerogit.BranchResult, error)
	isDefaultBranch              func(context.Context, zerogit.DefaultBranchOptions) (bool, string, string, error)
	currentGitUser               func(context.Context, string) string
	headCommitSubject            func(context.Context, string) string
	commitsAhead                 func(context.Context, string, string, string) (int, error)
	isUnbornRemote               func(context.Context, string, string) (bool, error)
	refreshTrackingRef           func(context.Context, string, string, string) error
	branchUpstreamRemote         func(context.Context, string, string) string
	branchUpstreamRemoteAndMerge func(context.Context, string, string) (string, string)
	resolveRemoteBranchTip       func(context.Context, string, string, string) (string, error)
	remoteHasBranch              func(context.Context, string, string, string) (bool, error)
	currentGitBranch             func(context.Context, string) string
	currentBranchTip             func(context.Context, string) string
	deleteBranch                 func(context.Context, string, string, string) error
	resetBranchRef               func(context.Context, string, string, string, string) error
	runTUI                       func(context.Context, tui.Options) int
	runEditor                    func(string) error
	checkUpdate                  func(context.Context, update.Options) (update.Result, error)
	applyUpdate                  func(context.Context, update.Options) (update.ApplyResult, error)
	now                          func() time.Time
}

type mcpToolRuntime interface {
	Close() error
	Skipped() []mcp.SkippedServer
}

type noopMCPRuntime struct{}

func (noopMCPRuntime) Close() error {
	return nil
}

func (noopMCPRuntime) Skipped() []mcp.SkippedServer {
	return nil
}

// Run executes the minimal Go CLI surface. It returns an exit code so tests can
// exercise command behavior without terminating the test process.
func Run(args []string, stdout io.Writer, stderr io.Writer) int {
	return runWithDeps(args, stdout, stderr, defaultAppDeps())
}

func defaultAppDeps() appDeps {
	return appDeps{
		getwd:                os.Getwd,
		stdin:                os.Stdin,
		userConfigPath:       config.DefaultUserConfigPath,
		exportActiveProvider: config.SetActiveProviderEnv,
		getenv:               os.Getenv,
		resolveConfig: func(workspaceRoot string, overrides config.Overrides) (config.ResolvedConfig, error) {
			options, err := config.DefaultResolveOptions(workspaceRoot)
			if err != nil {
				return config.ResolvedConfig{}, err
			}
			options.Overrides = overrides
			return config.Resolve(options)
		},
		resolveMCPConfig: func(workspaceRoot string, excludeProject bool) (config.MCPConfig, error) {
			options, err := config.DefaultResolveOptions(workspaceRoot)
			if err != nil {
				return config.MCPConfig{}, err
			}
			options.ExcludeProject = excludeProject
			return config.ResolveMCP(options)
		},
		newProvider: func(profile config.ProviderProfile) (zeroruntime.Provider, error) {
			// Resolve the OAuth login ONCE: the bearer resolver and the login key it
			// bound must describe the same login (the key is passed on to the Codex
			// account-header resolver so it never re-selects independently).
			resolver, loginKey := oauthLoginForProfile(profile)
			return providers.New(profile, providers.Options{
				UserAgent:     userAgent(),
				OAuthResolver: resolver,
				OAuthLoginKey: loginKey,
			})
		},
		probeProviderHealth:    providerhealth.Probe,
		discoverProviderModels: defaultDiscoverProviderModels,
		detectLocalRuntimes:    provideronboarding.DetectLocalRuntimes,
		openRouterLogin:        provideroauth.OpenRouterLogin,
		newSessionStore: func() *sessions.Store {
			return sessions.NewStore(sessions.StoreOptions{})
		},
		loadPlugins: plugins.Load,
		loadHooks:   hooks.LoadConfig,
		skillsDir: func() string {
			return skills.DefaultDir(nil)
		},
		pluginsDir: defaultUserPluginsDir,
		// Scaffolded tools are plugins, so the toolbox dir is the user plugins root:
		// after activation a `tools make` skeleton is discovered like any plugin.
		toolsDir: defaultUserPluginsDir,
		newMCPStore: func() (*mcp.PermissionStore, error) {
			return mcp.NewPermissionStore(mcp.StoreOptions{})
		},
		newMCPTokenStore: func() (*mcp.TokenStore, error) {
			return mcp.NewTokenStore(mcp.TokenStoreOptions{})
		},
		newSandboxStore: func() (*sandbox.GrantStore, error) {
			return sandbox.NewGrantStore(sandbox.StoreOptions{})
		},
		selectSandboxBackend: sandbox.SelectBackend,
		runSandboxSetupHelper: func(path string, args []string, stdout io.Writer, stderr io.Writer) error {
			cmd := exec.Command(path, args...)
			cmd.Stdout = stdout
			cmd.Stderr = stderr
			return cmd.Run()
		},
		registerMCPTools: func(ctx context.Context, registry *tools.Registry, cfg config.MCPConfig, options mcp.RegisterOptions) (mcpToolRuntime, error) {
			return mcp.RegisterTools(ctx, registry, cfg, options)
		},
		prepareWorktree:  worktrees.Prepare,
		releaseWorktree:  worktrees.Release,
		detectVerifyPlan: verify.DetectPlan,
		runVerify:        verify.Run,
		runSelfVerify:    selfverify.Run,
		runAgentEval:     defaultRunAgentEval,
		inspectChanges:   zerogit.Inspect,
		commitChanges:    zerogit.Commit,
		pushChanges:      zerogit.Push,
		createPR:         zerogit.CreatePR,
		createBranch:     zerogit.CreateBranch,
		isDefaultBranch:  zerogit.IsDefaultBranch,
		currentGitUser: func(ctx context.Context, cwd string) string {
			return zerogit.CurrentGitUser(ctx, cwd, nil)
		},
		headCommitSubject: func(ctx context.Context, cwd string) string {
			return zerogit.HeadCommitSubject(ctx, cwd, nil)
		},
		commitsAhead: func(ctx context.Context, cwd, remote, branch string) (int, error) {
			return zerogit.CommitsAhead(ctx, cwd, remote, branch, nil)
		},
		isUnbornRemote: func(ctx context.Context, cwd, remote string) (bool, error) {
			return zerogit.IsUnbornRemote(ctx, cwd, remote, nil)
		},
		refreshTrackingRef: func(ctx context.Context, cwd, remote, branch string) error {
			return zerogit.RefreshTrackingRef(ctx, cwd, remote, branch, nil)
		},
		branchUpstreamRemote: func(ctx context.Context, cwd, branch string) string {
			return zerogit.UpstreamRemote(ctx, cwd, branch, nil)
		},
		branchUpstreamRemoteAndMerge: func(ctx context.Context, cwd, branch string) (string, string) {
			return zerogit.UpstreamRemoteAndMergeBranch(ctx, cwd, branch, nil)
		},
		resolveRemoteBranchTip: func(ctx context.Context, cwd, remote, branch string) (string, error) {
			return zerogit.ResolveRemoteBranchTip(ctx, cwd, remote, branch, nil)
		},
		remoteHasBranch: func(ctx context.Context, cwd, remote, branch string) (bool, error) {
			return zerogit.RemoteHasBranch(ctx, cwd, remote, branch, nil)
		},
		currentGitBranch: func(ctx context.Context, cwd string) string {
			return zerogit.CurrentBranch(ctx, cwd, nil)
		},
		currentBranchTip: func(ctx context.Context, cwd string) string {
			return zerogit.CurrentBranchTip(ctx, cwd, nil)
		},
		deleteBranch: func(ctx context.Context, cwd, fallbackBranch, branchToDelete string) error {
			return zerogit.DeleteBranch(ctx, cwd, fallbackBranch, branchToDelete, nil)
		},
		resetBranchRef: func(ctx context.Context, cwd, branch, newTip, expectedOld string) error {
			return zerogit.ResetBranchRef(ctx, cwd, branch, newTip, nil, expectedOld)
		},
		runTUI:      tui.Run,
		runEditor:   openEditor,
		checkUpdate: update.Check,
		applyUpdate: update.Apply,
		now:         time.Now,
	}
}

func userAgent() string {
	return "zero/" + version
}

// defaultUserPluginsDir resolves the user-scoped plugins root
// ($XDG_CONFIG_HOME/zero/plugins) used as the install target for `plugin add`
// and the toolbox for `tools make`. It is the SourceUser root from
// plugins.ResolveRoots; an empty string is returned only if it cannot be
// resolved, which the command layer surfaces as an error.
func defaultUserPluginsDir() string {
	roots, err := plugins.ResolveRoots(plugins.ResolveRootOptions{})
	if err != nil {
		return ""
	}
	for _, root := range roots {
		if root.Source == plugins.SourceUser {
			return root.Path
		}
	}
	return ""
}

func runWithDeps(args []string, stdout io.Writer, stderr io.Writer, deps appDeps) (exitCode int) {
	// Self-dispatch as the Windows sandbox helper. When no standalone helper .exe
	// is shipped (dev / plain `go build`), the sandbox launches the running zero
	// binary with one of these hidden subcommands instead of a separate
	// executable (see resolveWindowsSandboxHelper). Routed before crash-recover,
	// dep-fill, and --add-dir splitting so it can never collide with a real
	// subcommand; the "__" prefix is unreachable by normal invocation.
	if len(args) > 0 {
		switch args[0] {
		case sandbox.WindowsCommandRunnerSubcommand:
			return sandbox.RunWindowsSandboxCommandRunner(args[1:], stderr)
		case sandbox.WindowsSandboxSetupSubcommand:
			return sandbox.RunWindowsSandboxSetup(args[1:], stderr)
		}
	}

	// Convert an unexpected panic anywhere in the CLI into a saved crash report and
	// a brief notice, rather than a raw stack trace dumped at the user.
	defer observability.Recover(observability.DefaultCrashDir(), "cli", stderr, &exitCode)
	deps = fillAppDeps(deps)

	// CLI runs opt into the models.dev overlay (cached live context limits and
	// pricing on top of the curated catalog). Explicitly enabled here — and only
	// here — so library consumers and hermetic tests are never perturbed by a
	// cache file on the machine. The refresh itself is fired in exec/TUI startup.
	modelregistry.EnableModelsDevOverlay()

	// --add-dir, --theme and --allow-escalation may be written in any order;
	// see splitLeadingRootFlags for why they are split together.
	root, args, err := splitLeadingRootFlags(args)
	if err != nil {
		return writeAppError(stderr, err.Error(), 1)
	}
	addDirs, theme, allowEscalation := root.addDirs, root.theme, root.allowEscalation

	if len(args) == 0 {
		return runInteractiveTUI(stderr, deps, agent.PermissionModeAsk, addDirs, theme, allowEscalation)
	}

	// --add-dir grants an extra write root and --allow-escalation opts a run
	// into mid-run model escalation; only the interactive TUI and exec consume
	// either. Both are forwarded to exec below and rejected loudly everywhere
	// else rather than silently discarded, including help/version, which run
	// no agent and could only ignore them. The allowlist names exactly the
	// cases below that forward; a future subcommand is rejected by default
	// until it opts in here.
	forwardsRootFlags := false
	switch args[0] {
	case "--skip-permissions-unsafe", "-p", "--prompt", "exec":
		forwardsRootFlags = true
	}
	if len(addDirs) > 0 && !forwardsRootFlags {
		return writeAppError(stderr, "--add-dir is only supported for the interactive TUI and exec", 1)
	}
	if allowEscalation && !forwardsRootFlags {
		return writeAppError(stderr, "--allow-escalation is only supported for the interactive TUI and exec", 1)
	}

	switch args[0] {
	case "--skip-permissions-unsafe":
		// Launch the interactive TUI directly in unsafe mode. Without this, the
		// flag fell through to the unknown-command path, so a user could never
		// reach unsafe mode in the shell — and the "!" shell escape (which is
		// gated behind unsafe) was therefore unreachable.
		//
		// The root flags may legally appear on either side of this flag, so
		// split them again from what follows it and merge with what the root
		// already took. Any trailing non-flag args were ignored on this path
		// before --add-dir existed and still are, but an --add-dir hidden
		// BEHIND one would be silently dropped with them, so reject that
		// misplacement loudly below.
		more, rest, err := splitLeadingRootFlags(args[1:])
		if err != nil {
			return writeAppError(stderr, err.Error(), 1)
		}
		moreDirs := more.addDirs
		// A --theme written before the flag was taken at the root and used to be
		// dropped here; one written after it wins, as the last occurrence does.
		skipTheme := theme
		if more.theme != "" {
			skipTheme = more.theme
		}
		skipAllowEscalation := more.allowEscalation
		// A misplaced --add-dir anywhere in the remainder is the more specific error,
		// so check for it across all of rest before rejecting stray args.
		for _, arg := range rest {
			if arg == "--add-dir" || strings.HasPrefix(arg, "--add-dir=") {
				return writeAppError(stderr, "--add-dir must come before any other arguments (it may precede or follow --skip-permissions-unsafe)", 1)
			}
		}
		// This path launches the interactive TUI, which takes no positional prompt or
		// subcommand. Reject any remaining trailing arg loudly instead of silently
		// dropping it, so `zero --skip-permissions-unsafe "fix bug"` doesn't appear to
		// hang in the TUI with the prompt discarded. (AUDIT-L3)
		for _, arg := range rest {
			if strings.TrimSpace(arg) != "" {
				return writeAppError(stderr, "--skip-permissions-unsafe launches the interactive TUI and takes no prompt or subcommand; for a one-shot unsafe run use `zero exec --skip-permissions-unsafe -p \"...\"`", 1)
			}
		}
		return runInteractiveTUI(stderr, deps, agent.PermissionModeUnsafe, append(append([]string{}, addDirs...), moreDirs...), skipTheme, allowEscalation || skipAllowEscalation)
	case "-h", "--help", "help":
		if err := writeHelp(stdout); err != nil {
			return 1
		}
		return 0
	case "-v", "--version", "version":
		for _, a := range args[1:] {
			if a == "-h" || a == "--help" {
				if _, err := fmt.Fprintln(stdout, "Usage: zero version\n\nPrint the Zero CLI version. Takes no flags."); err != nil {
					return 1
				}
				return 0
			}
		}
		// Pipes and redirects keep the machine-readable contract exactly as it
		// was: a single "zero <version>" line. Scripts, command substitutions,
		// and the NPM wrapper smoke check all parse that record, so the banner
		// is strictly a TTY affordance.
		if !stdoutIsTerminal(stdout) {
			if _, err := fmt.Fprintf(stdout, "zero %s\n", version); err != nil {
				return 1
			}
			return 0
		}
		if _, err := fmt.Fprintf(stdout, "%s\n\nzero %s\n", tui.Wordmark(), version); err != nil {
			return 1
		}
		return 0
	case "-p", "--prompt":
		if len(args) < 2 {
			return writePromptRequired(stderr)
		}
		// Forward the root flags exec consumes so its own parser collects them.
		// Use the inline --prompt=<value> form so a prompt whose first character is a
		// dash (e.g. `zero -p "-foo"`) is taken verbatim instead of being mistaken for
		// a flag and rejected with "--prompt requires a value" (matches the cron path).
		execArgs := append(rootFlagArgs(addDirs, allowEscalation), "--prompt="+args[1])
		execArgs = append(execArgs, args[2:]...)
		return runExec(execArgs, stdout, stderr, deps)
	case "exec":
		// Forward the root flags exec consumes so its own parser collects them.
		return runExec(append(rootFlagArgs(addDirs, allowEscalation), args[1:]...), stdout, stderr, deps)
	case "completions":
		return runCompletions(args[1:], stdout, stderr)
	case "daemon":
		return runDaemon(args[1:], stdout, stderr, deps)
	case "config":
		return runConfig(args[1:], stdout, stderr, deps)
	case "models":
		return runModels(args[1:], stdout, stderr)
	case "providers":
		return runProviders(args[1:], stdout, stderr, deps)
	case "doctor":
		return runDoctor(args[1:], stdout, stderr, deps)
	case "setup":
		return runSetup(args[1:], stdout, stderr, deps)
	case "context":
		return runContext(args[1:], stdout, stderr, deps)
	case "repo-map", "repomap":
		return runRepoMap(args[1:], stdout, stderr, deps)
	case "search", "find":
		return runSearch(args[1:], stdout, stderr, deps)
	case "sessions", "session":
		return runSessions(args[1:], stdout, stderr, deps)
	case "spec":
		return runSpec(args[1:], stdout, stderr, deps)
	case "init":
		return runInit(args[1:], stdout, stderr, deps)
	case "specialists", "specialist":
		return runSpecialists(args[1:], stdout, stderr, deps)
	case "plugins", "plugin":
		return runPlugins(args[1:], stdout, stderr, deps)
	case "backends", "backend":
		return runBackends(args[1:], stdout, stderr, deps)
	case "skills", "skill":
		return runSkills(args[1:], stdout, stderr, deps)
	case "tools", "tool":
		return runTools(args[1:], stdout, stderr, deps)
	case "hooks":
		return runHooks(args[1:], stdout, stderr, deps)
	case "mcp":
		return runMCP(args[1:], stdout, stderr, deps)
	case "auth":
		return runAuth(args[1:], stdout, stderr, deps)
	case "sandbox":
		return runSandbox(args[1:], stdout, stderr, deps)
	case "update":
		return runUpdate(args[1:], stdout, stderr, deps)
	case "upgrade":
		return runUpgrade(args[1:], stdout, stderr, deps)
	case "worktrees", "worktree":
		return runWorktrees(args[1:], stdout, stderr, deps)
	case "verify":
		return runVerifyCommand(args[1:], stdout, stderr, deps)
	case "trust":
		return runTrust(args[1:], stdout, stderr, deps)
	case "eval":
		return runAgentEvalCommand(args[1:], stdout, stderr, deps)
	case "changes", "change":
		return runChanges(args[1:], stdout, stderr, deps)
	case "usage":
		return runUsage(args[1:], stdout, stderr, deps)
	case "cron":
		return runCron(args[1:], stdout, stderr, deps)
	case "repo-info", "repoinfo":
		return runRepoInfo(args[1:], stdout, stderr, deps)
	case "serve":
		return runServe(args[1:], stdout, stderr, deps)
	case "acp":
		return runACP(args[1:], stdout, stderr, deps)
	default:
		if _, err := fmt.Fprintf(stderr, "unknown command %q\n", args[0]); err != nil {
			return 1
		}
		// First-run users reach for `zero login`/`zero logout` (reported in the
		// wild); point them at the real command instead of a bare usage pointer.
		switch strings.ToLower(args[0]) {
		case "login", "logout":
			if _, err := fmt.Fprintf(stderr, "did you mean %q?\n", "zero auth "+strings.ToLower(args[0])); err != nil {
				return 1
			}
		}
		if _, err := fmt.Fprintln(stderr, "Run zero --help for usage."); err != nil {
			return 1
		}
		return 2
	}
}

func fillAppDeps(deps appDeps) appDeps {
	defaults := defaultAppDeps()
	if deps.getwd == nil {
		deps.getwd = defaults.getwd
	}
	if deps.stdin == nil {
		deps.stdin = defaults.stdin
	}
	if deps.userConfigPath == nil {
		deps.userConfigPath = defaults.userConfigPath
	}
	if deps.resolveConfig == nil {
		deps.resolveConfig = defaults.resolveConfig
	}
	if deps.resolveMCPConfig == nil {
		deps.resolveMCPConfig = defaults.resolveMCPConfig
	}
	if deps.newProvider == nil {
		deps.newProvider = defaults.newProvider
	}
	if deps.probeProviderHealth == nil {
		deps.probeProviderHealth = defaults.probeProviderHealth
	}
	if deps.discoverProviderModels == nil {
		deps.discoverProviderModels = defaults.discoverProviderModels
	}
	if deps.detectLocalRuntimes == nil {
		deps.detectLocalRuntimes = defaults.detectLocalRuntimes
	}
	if deps.openRouterLogin == nil {
		deps.openRouterLogin = defaults.openRouterLogin
	}
	if deps.newSessionStore == nil {
		deps.newSessionStore = defaults.newSessionStore
	}
	if deps.loadPlugins == nil {
		deps.loadPlugins = defaults.loadPlugins
	}
	if deps.loadHooks == nil {
		deps.loadHooks = defaults.loadHooks
	}
	if deps.skillsDir == nil {
		deps.skillsDir = defaults.skillsDir
	}
	if deps.pluginsDir == nil {
		deps.pluginsDir = defaults.pluginsDir
	}
	if deps.toolsDir == nil {
		deps.toolsDir = defaults.toolsDir
	}
	if deps.newMCPStore == nil {
		deps.newMCPStore = defaults.newMCPStore
	}
	if deps.newMCPTokenStore == nil {
		deps.newMCPTokenStore = defaults.newMCPTokenStore
	}
	if deps.newSandboxStore == nil {
		deps.newSandboxStore = defaults.newSandboxStore
	}
	if deps.selectSandboxBackend == nil {
		deps.selectSandboxBackend = defaults.selectSandboxBackend
	}
	if deps.runSandboxSetupHelper == nil {
		deps.runSandboxSetupHelper = defaults.runSandboxSetupHelper
	}
	if deps.registerMCPTools == nil {
		deps.registerMCPTools = defaults.registerMCPTools
	}
	if deps.prepareWorktree == nil {
		deps.prepareWorktree = defaults.prepareWorktree
	}
	if deps.releaseWorktree == nil {
		deps.releaseWorktree = defaults.releaseWorktree
	}
	if deps.detectVerifyPlan == nil {
		deps.detectVerifyPlan = defaults.detectVerifyPlan
	}
	if deps.runVerify == nil {
		deps.runVerify = defaults.runVerify
	}
	if deps.runSelfVerify == nil {
		deps.runSelfVerify = defaults.runSelfVerify
	}
	if deps.runAgentEval == nil {
		deps.runAgentEval = defaults.runAgentEval
	}
	if deps.inspectChanges == nil {
		deps.inspectChanges = defaults.inspectChanges
	}
	if deps.commitChanges == nil {
		deps.commitChanges = defaults.commitChanges
	}
	if deps.pushChanges == nil {
		deps.pushChanges = defaults.pushChanges
	}
	if deps.createPR == nil {
		deps.createPR = defaults.createPR
	}
	if deps.createBranch == nil {
		deps.createBranch = defaults.createBranch
	}
	if deps.isDefaultBranch == nil {
		deps.isDefaultBranch = defaults.isDefaultBranch
	}
	if deps.currentGitUser == nil {
		deps.currentGitUser = defaults.currentGitUser
	}
	if deps.headCommitSubject == nil {
		deps.headCommitSubject = defaults.headCommitSubject
	}
	if deps.commitsAhead == nil {
		deps.commitsAhead = defaults.commitsAhead
	}
	if deps.isUnbornRemote == nil {
		deps.isUnbornRemote = defaults.isUnbornRemote
	}
	if deps.refreshTrackingRef == nil {
		deps.refreshTrackingRef = defaults.refreshTrackingRef
	}
	if deps.branchUpstreamRemote == nil {
		deps.branchUpstreamRemote = defaults.branchUpstreamRemote
	}
	if deps.branchUpstreamRemoteAndMerge == nil {
		deps.branchUpstreamRemoteAndMerge = defaults.branchUpstreamRemoteAndMerge
	}
	if deps.remoteHasBranch == nil {
		deps.remoteHasBranch = defaults.remoteHasBranch
	}
	if deps.currentGitBranch == nil {
		deps.currentGitBranch = defaults.currentGitBranch
	}
	if deps.currentBranchTip == nil {
		deps.currentBranchTip = defaults.currentBranchTip
	}
	// resolveRemoteBranchTip, deleteBranch, and resetBranchRef stay nil when
	// unset so unit tests that mock createBranch without a real git tree do not
	// hit real git restore/delete commands.
	if deps.runTUI == nil {
		deps.runTUI = defaults.runTUI
	}
	if deps.runEditor == nil {
		deps.runEditor = defaults.runEditor
	}
	if deps.checkUpdate == nil {
		deps.checkUpdate = defaults.checkUpdate
	}
	if deps.applyUpdate == nil {
		deps.applyUpdate = defaults.applyUpdate
	}
	if deps.now == nil {
		deps.now = defaults.now
	}
	// Wrap newProvider ONCE, after all defaults are filled, so every surface that
	// builds the runtime provider gets the credential-store key applied. The
	// resolver is pure and leaves apiKeyStored profiles keyless; an unwrapped
	// build sends unauthenticated requests. config.ApplyStoredAPIKey is a no-op
	// when the key is already set or the profile isn't stored-key, so this is
	// safe and idempotent for every profile kind and every injected newProvider.
	baseNewProvider := deps.newProvider
	userConfigPath := deps.userConfigPath
	deps.newProvider = func(profile config.ProviderProfile) (zeroruntime.Provider, error) {
		return baseNewProvider(applyStoredProviderKeyAt(profile, userConfigPath))
	}
	baseProbeProviderHealth := deps.probeProviderHealth
	deps.probeProviderHealth = func(ctx context.Context, options providerhealth.Options) providerhealth.Result {
		options.Profile = applyStoredProviderKeyAt(options.Profile, userConfigPath)
		if options.OAuthResolver == nil {
			options.OAuthResolver, _ = oauthLoginForProfile(options.Profile)
		}
		return baseProbeProviderHealth(ctx, options)
	}
	return deps
}

func runInteractiveTUI(stderr io.Writer, deps appDeps, permissionMode agent.PermissionMode, addDirs []string, theme string, allowEscalation bool) int {
	return runInteractiveTUIWithSetup(stderr, deps, permissionMode, addDirs, theme, false, allowEscalation)
}

func runInteractiveTUIWithSetup(stderr io.Writer, deps appDeps, permissionMode agent.PermissionMode, addDirs []string, theme string, forceSetup bool, allowEscalation bool) int {
	// Refresh the models.dev pricing/limits cache in the background when stale;
	// the overlay is read at registry construction from the cache file, so this
	// benefits the next run and never blocks or fails this one.
	go func() { _ = modelregistry.RefreshModelsDevCache(context.Background()) }()

	workspaceRoot, err := deps.getwd()
	if err != nil {
		return writeAppError(stderr, "failed to resolve workspace: "+err.Error(), 1)
	}

	resolved, err := deps.resolveConfig(workspaceRoot, config.Overrides{})
	if err != nil {
		// A resolve failure the setup wizard can FIX is not fatal for the
		// interactive TUI: drop into the wizard with an empty config so the user
		// can onboard/repair, instead of exiting with an error they can only fix
		// by hand-editing config.json. That covers a missing/unresolvable active
		// provider and an active provider without a model (custom endpoints have
		// no catalog default) — previously the second shape bricked bare `zero`
		// and `zero setup`, the exact commands that could have fixed it. Any
		// other error (malformed JSON, directory conflict, etc.) is still fatal,
		// and headless commands (zero config / zero exec) still fail with the
		// actionable message.
		if !errors.Is(err, config.ErrNoActiveProvider) && !errors.Is(err, config.ErrProviderRequiresModel) {
			return writeAppError(stderr, err.Error(), 1)
		}
		// ErrNoActiveProvider can mean "nothing configured yet" (needs onboarding)
		// or "providers ARE configured, just none marked active" (e.g. config.json's
		// activeProvider is blank/stale). In the second case resolved.Providers still
		// carries the already-normalized list — prefer falling back to one of those
		// over wiping everything and forcing the user to re-enter credentials they
		// already saved.
		if usable, ok := firstUsableProvider(resolved.Providers); errors.Is(err, config.ErrNoActiveProvider) && ok {
			resolved.Provider = usable
			resolved.ActiveProvider = usable.Name
		} else {
			resolved = config.ResolvedConfig{}
			forceSetup = true
		}
	}
	userConfigPath, err := deps.userConfigPath()
	if err != nil {
		return writeAppError(stderr, "failed to resolve user config path: "+err.Error(), 1)
	}
	// Fail-soft, one-time: move any inline plaintext API keys in config.json into
	// the encrypted credential store (interactive runs only; headless exec keeps
	// its existing behavior). Non-fatal — a missing keyring or write error leaves
	// the inline key in place and this run still uses the already-resolved key.
	if store, storeErr := config.ProviderKeyStoreAt(filepath.Dir(userConfigPath)); storeErr == nil {
		_, _ = config.MigratePlaintextProviderKeys(userConfigPath, store)
	}
	doctorUserConfigPath := ""
	projectConfigPath := ""
	if resolveOptions, optErr := config.DefaultResolveOptions(workspaceRoot); optErr == nil {
		doctorUserConfigPath = resolveOptions.UserConfigPath
		projectConfigPath = resolveOptions.ProjectConfigPath
	}

	needsSetup := setupRequired(resolved)
	if needsSetup && !forceSetup {
		// The active provider lacks a usable credential, but if another saved
		// provider already has one, fall back to it instead of forcing onboarding
		// again. Saved logins persist across launches; switch the active provider
		// any time with `zero provider use <name>`. Onboarding only runs when no
		// configured provider is usable (a genuinely fresh setup).
		if usable, ok := firstUsableProvider(resolved.Providers); ok {
			resolved.Provider = usable
			resolved.ActiveProvider = usable.Name
			needsSetup = false
		}
	}
	setupVisible := forceSetup || needsSetup
	configPath := ""
	if setupVisible {
		configPath, err = deps.userConfigPath()
		if err != nil {
			return writeAppError(stderr, err.Error(), 1)
		}
	}

	scope, err := sandbox.NewScope(workspaceRoot, append(append([]string{}, resolved.Sandbox.AdditionalWriteRoots...), addDirs...))
	if err != nil {
		return writeAppError(stderr, err.Error(), 1)
	}

	provider, err := buildProvider(resolved, deps)
	if err != nil {
		return writeAppError(stderr, err.Error(), 1)
	}

	registry := newCoreRegistryScoped(workspaceRoot, scope)
	registerLocalControlTools(registry, workspaceRoot, resolved.LocalControl)
	// Mid-run model escalation is opt-in on this surface too. The tool is present
	// only when the operator asked for it with --allow-escalation, and the
	// switchers that make it do anything ride on the same flag through
	// Options.AllowEscalation below. Registering one without the other ships a
	// tool the loop will never act on, which looks like a feature and is not.
	if allowEscalation {
		registry.Register(tools.NewEscalateModelTool())
	}
	executionRunner := execution.NewRunner(nil)
	sandboxStore, err := deps.newSandboxStore()
	if err != nil {
		return writeAppError(stderr, "failed to initialize sandbox grants: "+err.Error(), 1)
	}
	if notice, err := sandboxStore.ConsumeMigrationNotice(); err != nil {
		return writeAppError(stderr, "failed to migrate sandbox grants: "+err.Error(), 1)
	} else if notice != "" {
		_, _ = fmt.Fprintln(stderr, "[zero] "+notice)
	}
	sandboxBackend := deps.selectSandboxBackend(sandbox.BackendOptions{})
	sandboxEngine := sandbox.NewEngine(sandbox.EngineOptions{
		WorkspaceRoot:    workspaceRoot,
		Policy:           applyConfiguredSandboxPolicy(sandbox.DefaultPolicy(), resolved.Sandbox),
		Store:            sandboxStore,
		Backend:          sandboxBackend,
		Scope:            scope,
		SensitiveEnvKeys: providerSensitiveEnvKeys(resolved),
	})
	executionRunner.SetPreparer(sandboxEngine)
	specialistRuntime, err := registerSpecialistTools(registry, workspaceRoot, resolved.Swarm.MaxTeamSize)
	if err != nil {
		return writeAppError(stderr, "failed to initialize specialist tools: "+err.Error(), 1)
	}
	defer closeSpecialistRuntime(stderr, specialistRuntime)
	// The TUI has no --worktree reassignment, so trustRoot == workspaceRoot here.
	// Gate the project MCP layer behind the workspace-trust check (fail-closed): an
	// untrusted workspace must not spawn its ./.zero/config.json stdio MCP servers.
	// Keep the store-read error so the notice below can distinguish a fail-closed
	// store error from a clean untrusted verdict.
	mcpExcludeProject, mcpTrustErrored := resolveTrust(workspaceRoot)
	mcpSkip := trustSkip{
		excludedProjectConfig: mcpExcludeProject && projectMCPConfigExists(workspaceRoot),
		trustCheckErrored:     mcpTrustErrored,
	}
	mcpConfig, err := deps.resolveMCPConfig(workspaceRoot, mcpExcludeProject)
	if err != nil {
		return writeAppError(stderr, err.Error(), 1)
	}
	mcpPermissionStore, err := deps.newMCPStore()
	if err != nil {
		return writeAppError(stderr, "failed to initialize MCP permissions: "+err.Error(), 1)
	}
	mcpTokenStore, err := deps.newMCPTokenStore()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "[zero] warning: failed to initialize MCP OAuth tokens: %s\n", err)
		mcpTokenStore = nil
		err = nil
	}
	criticalMCPConfig, optionalMCPConfig := splitMCPStartupConfig(mcpConfig)
	mcpRuntime := mcpToolRuntime(noopMCPRuntime{})
	if len(criticalMCPConfig.Servers) > 0 {
		runtime, registerErr := deps.registerMCPTools(context.Background(), registry, criticalMCPConfig, mcp.RegisterOptions{
			PermissionStore: mcpPermissionStore,
			Autonomy:        mcp.AutonomyLow,
			Execution:       executionRunner,
			WorkspaceRoot:   workspaceRoot,
		})
		if registerErr != nil {
			closeMCPRuntime(stderr, runtime)
			return writeAppError(stderr, redaction.ErrorMessage(registerErr, redaction.Options{}), 1)
		}
		mcpRuntime = runtime
	}
	defer closeMCPRuntime(stderr, mcpRuntime)
	// A server that could not be reached or validated is skipped, not fatal (one
	// bad MCP server must not abort startup) — surface each so a missing tool set is
	// explained rather than silently absent. A built-in default the user never
	// configured (e.g. keyless Exa with no credentials) is the exception: it
	// was never asked for, so its failure is not worth a startup warning — only
	// servers the user actually configured warn on failure (issue #552).
	for _, skipped := range mcpRuntime.Skipped() {
		if skipped.UnconfiguredDefault {
			continue
		}
		fmt.Fprintf(stderr, "warning: MCP server %s unavailable, skipped: %s\n", skipped.Name, redaction.ErrorMessage(skipped.Err, redaction.Options{}))
	}
	// Make local plugins live: register their declared tools into the registry and
	// collect their hooks + skill roots for the dispatcher and skill tool below.
	// Done after specialist + MCP registration so plugin tools are part of the
	// deferral count, and it fails OPEN — a malformed plugin is warned and skipped.
	// The interactive TUI is not worktree-reassigned, so the trust root is the
	// launch directory itself.
	trustRoot := workspaceRoot
	pluginActivation := activatePlugins(workspaceRoot, registry, deps, stderr, trustRoot, executionRunner)
	// Ask (not Auto) is the interactive default: in Auto, ToolAdvertised exposes
	// only PermissionAllow tools, so prompt-gated tools (write_file/edit_file/bash/
	// apply_patch) would never be offered to the model — the TUI could neither edit
	// files nor run shell. Ask advertises them and routes each through the existing
	// OnPermissionRequest flow; shift+tab lets the user switch modes live. An
	// explicit --skip-permissions-unsafe launch overrides this to unsafe (the only
	// way to reach unsafe, since shift+tab deliberately cycles auto↔ask only).
	//
	// Resolve the effective mode BEFORE the deferral gate below so the registration
	// count uses the SAME permission mode the agent loop's partition will use; an
	// empty mode here would mis-gate prompt-advertised deferred tools.
	if permissionMode == "" {
		permissionMode = agent.PermissionModeAsk
	}
	sessionStore := deps.newSessionStore()
	peerClass := peermsg.PermissionPrompting
	if permissionMode == agent.PermissionModeUnsafe {
		peerClass = peermsg.PermissionBypass
	}
	peerService, peerErr := peermsg.New(peermsg.Options{
		Identity: peermsg.Identity{
			Cwd:             workspaceRoot,
			PermissionClass: peerClass,
		},
		InboundPolicy: peermsg.InboundPolicy(resolved.CrossSessionInbound),
	})
	if peerErr != nil {
		fmt.Fprintf(stderr, "warning: cross-session messaging unavailable: %s\n", peerErr)
		peerService = nil
	} else {
		for _, tool := range tools.NewPeerSessionTools(peerService) {
			registry.Register(tool)
		}
	}
	// Activate deferred MCP-tool loading for the interactive run only when the
	// VISIBLE deferred-eligible count meets the resolved threshold, matching exec.
	// This first pass covers every prompt-critical tool. Optional built-in MCP
	// defaults re-run the same gate after publishing their atomic batch, so a
	// later catalog generation cannot miss its loader. The interactive surface
	// applies no operator tool filters, so enabled/disabled are nil — matching the
	// AgentOptions below.
	registerToolSearchIfEligible(registry, resolved.Tools.DeferThreshold, permissionMode, nil, nil)
	var optionalMCPRuntime *optionalMCPStartup
	var awaitToolReadiness func(context.Context)
	if len(optionalMCPConfig.Servers) > 0 {
		optionalMCPRuntime = startOptionalMCP(
			context.Background(),
			registry,
			optionalMCPConfig,
			mcp.RegisterOptions{
				PermissionStore: mcpPermissionStore,
				Autonomy:        mcp.AutonomyLow,
				Execution:       executionRunner,
				WorkspaceRoot:   workspaceRoot,
			},
			deps.registerMCPTools,
			func() {
				registerToolSearchIfEligible(registry, resolved.Tools.DeferThreshold, permissionMode, nil, nil)
			},
		)
		defer closeMCPRuntime(stderr, optionalMCPRuntime)
		awaitToolReadiness = func(ctx context.Context) {
			optionalMCPRuntime.Await(ctx, optionalMCPPromptGrace)
		}
	}
	lastKnownMCPConfig := mcpConfig
	fileTracker := tools.NewFileTracker()
	var scratchBaseline scratchFileBaseline
	sttServerManager := newDictationServerManager(resolved.STT)
	// Keep STT downloads in the SAME config tree the rest of the TUI uses. Deriving
	// from userConfigPath (rather than config.UserConfigDir()) matters when the config
	// root is overridden — e.g. in tests or a custom ZERO config dir — so the two
	// don't diverge. userConfigPath points at .../zero/config.json, so its dir is the
	// zero config dir.
	sttDownloadRoot := ""
	if userConfigPath != "" {
		sttDownloadRoot = filepath.Join(filepath.Dir(userConfigPath), "stt")
	}
	// Build the hooks dispatcher out of the AgentOptions literal so its trust skip
	// report can be combined with the plugin activation's, and emit at most one
	// notice when project hooks/plugins were dropped for an untrusted workspace.
	hookDispatcher, hookSkip := newHookDispatcherWithExtra(workspaceRoot, pluginActivation.hooks, trustRoot, executionRunner)
	emitTrustNotice(stderr, hookSkip, pluginActivation.trustSkip, mcpSkip)
	return deps.runTUI(context.Background(), tui.Options{
		Cwd:                  workspaceRoot,
		Version:              version,
		Theme:                theme,
		SavedTheme:           resolved.Preferences.Theme,
		SavedPet:             resolved.Preferences.Pet,
		UserConfigPath:       userConfigPath,
		DoctorUserConfigPath: doctorUserConfigPath,
		ProjectConfigPath:    projectConfigPath,
		ProviderName:         resolved.Provider.Name,
		ModelName:            resolved.Provider.Model,
		ProviderProfile:      resolved.Provider,
		SavedProviders:       usableSavedProviders(resolved.Providers),
		FavoriteModels:       resolved.Preferences.FavoriteModels,
		RecentModels:         resolved.Preferences.RecentModels,
		RecapsEnabled:        resolved.Preferences.RecapsEnabled(),
		CompactionModel:      resolved.Preferences.CompactionModel,
		Provider:             provider,
		NewProvider:          deps.newProvider,
		NewTurnSessionProvider: func(profile config.ProviderProfile, provider zeroruntime.Provider) zeroruntime.TurnSessionProvider {
			if provider == nil {
				return nil
			}
			if optimized, ok := providers.OptimizedTurnSessions(profile, provider, providers.Options{}); ok {
				return optimized
			}
			return providers.DefaultTurnSessions(profile, provider, providers.Options{})
		},
		ProbeProviderHealth: deps.probeProviderHealth,
		UserAgent:           userAgent(),
		PrepareRunCompletionWarning: func() {
			scratchBaseline = scratchFileSnapshot(workspaceRoot)
		},
		RunCompletionWarning: func() string {
			return scratchFileWarning(workspaceRoot, scratchBaseline)
		},
		Registry:           registry,
		AwaitToolReadiness: awaitToolReadiness,
		SessionStore:       sessionStore,
		PeerService:        peerService,
		SandboxStore:       sandboxStore,
		MCPConfig:          mcpConfig,
		MCPPermissionStore: mcpPermissionStore,
		MCPTokenStore:      mcpTokenStore,
		MCPCommand: func(ctx context.Context, args []string) tui.MCPCommandResult {
			if ctx == nil {
				ctx = context.Background()
			}
			var stdout, stderr bytes.Buffer
			exitCode := runMCPWithContext(ctx, args, &stdout, &stderr, deps)
			nextConfig := lastKnownMCPConfig
			if refreshed, err := deps.resolveMCPConfig(workspaceRoot, mcpExcludeProject); err == nil {
				lastKnownMCPConfig = refreshed
				nextConfig = refreshed
			}
			return tui.MCPCommandResult{
				Config:   nextConfig,
				Output:   strings.TrimSpace(stdout.String()),
				Error:    strings.TrimSpace(stderr.String()),
				ExitCode: exitCode,
			}
		},
		SandboxSetupCommand: tuiSandboxSetupCommand(sandboxBackend, deps),
		AgentOptions: agent.Options{
			MaxTurns:       resolved.MaxTurns,
			Registry:       registry,
			PermissionMode: permissionMode,
			Autonomy:       "low",
			Sandbox:        sandboxEngine,
			FileTracker:    fileTracker,
			Hooks:          hookDispatcher,
			DeferThreshold: resolved.Tools.DeferThreshold,
			Specialists:    specialistRuntime.specialists,
			Skills:         pluginActivation.skillInfos(deps.skillsDir()),
		},
		AllowEscalation: allowEscalation,
		// LoadSkills backs /skills and direct /<skill-name> invocation in the TUI.
		// It resolves against the same merged set (default dir + plugin skill
		// roots) as the skill tool and the system-prompt list, re-read per use so
		// newly installed skills work without a restart.
		LoadSkills: cachedSkillsLoader(func() []skills.Skill {
			merged, _ := plugins.MergedSkillsLoaded(deps.skillsDir(), pluginActivation.skillRoots)
			return merged
		}),
		PermissionMode:            permissionMode,
		Notify:                    resolved.Notify,
		KeyBindings:               resolved.KeyBindings,
		STT:                       resolved.STT,
		BuildDictationTranscriber: newDictationTranscriberFactory(resolved, userConfigPath, sttServerManager),
		ShutdownDictationServer:   sttServerManager.Shutdown,
		STTDownloadRoot:           sttDownloadRoot,
		STTKeyStatus:              newSTTKeyStatus(resolved, userConfigPath),
		SaveSTTKey:                newSaveSTTKey(userConfigPath),
		Setup: tui.SetupOptions{
			Visible:    setupVisible,
			Required:   needsSetup,
			ConfigPath: configPath,
			Providers:  setupProviderOptions(),
			Save: func(selection tui.SetupSelection) (tui.SetupResult, error) {
				return saveSetupProvider(deps, selection, setupSaveOptions{})
			},
		},
	})
}

func tuiSandboxSetupCommand(backend sandbox.Backend, deps appDeps) func(context.Context) tui.SandboxSetupCommandResult {
	if backend.Platform != "windows" || backend.Name != sandbox.BackendWindowsRestrictedToken || !backend.Available || backend.Executable == "" {
		return nil
	}
	return func(context.Context) tui.SandboxSetupCommandResult {
		var stdout, stderr bytes.Buffer
		exitCode := runSandboxSetup(nil, &stdout, &stderr, deps)
		return tui.SandboxSetupCommandResult{
			Output:   strings.TrimSpace(stdout.String()),
			Error:    strings.TrimSpace(stderr.String()),
			ExitCode: exitCode,
		}
	}
}

// buildProvider constructs the run's provider at STARTUP — it is called only from
// the two launch paths (interactive TUI and headless exec), never from mid-run
// rebuilds (escalation, wizard, ACP go through deps.newProvider directly).
func buildProvider(resolved config.ResolvedConfig, deps appDeps) (zeroruntime.Provider, error) {
	if !config.HasProviderProfile(resolved.Provider) {
		return nil, nil
	}
	// deps.newProvider is wrapped in fillAppDeps to apply the stored key, so this
	// (and every other newProvider caller) needs no per-site key handling.
	provider, err := deps.newProvider(resolved.Provider)
	if err != nil {
		return nil, err
	}
	// Pin spawned children (sub-agents / swarm members inherit the environment) to
	// THIS run's provider from launch, not only after an in-session switch. Without
	// the launch-time export a child re-resolves config.json at spawn time, so a
	// provider switch persisted by ANOTHER zero process mid-session would silently
	// move new children onto a different provider (and different credentials) than
	// the parent is running. Runtime switches (/model, /provider, wizard,
	// onboarding) re-export on commit, keeping the pin current. Injected via deps
	// (nil in tests) because it mutates the process environment.
	if deps.exportActiveProvider != nil {
		deps.exportActiveProvider(resolved.Provider.Name)
	}
	return provider, nil
}

// applyStoredProviderKeyAt loads the API key from the encrypted credential store
// when the resolver left it empty (the resolver is pure — no I/O — so an
// apiKeyStored profile reaches this layer keyless). No-op for inline/env-key
// providers and when nothing is stored. This is applied once, centrally, by the
// deps.newProvider wrapper in fillAppDeps so EVERY surface that builds a runtime
// provider (headless exec, the ACP builder, exec's mid-run escalation switcher,
// and any future caller) is covered without a per-site invariant to forget.
func applyStoredProviderKeyAt(profile config.ProviderProfile, userConfigPath func() (string, error)) config.ProviderProfile {
	if userConfigPath == nil {
		return profile
	}
	if path, perr := userConfigPath(); perr == nil {
		if store, err := config.ProviderKeyStoreAt(filepath.Dir(path)); err == nil {
			profile = config.ApplyStoredAPIKey(profile, store)
		}
	}
	return profile
}

func newCoreRegistry(workspaceRoot string) *tools.Registry {
	return newCoreRegistryScoped(workspaceRoot, nil)
}

func newCoreRegistryScoped(workspaceRoot string, scope tools.PathScope) *tools.Registry {
	registry := tools.NewRegistry()
	for _, tool := range tools.CoreToolsScoped(workspaceRoot, scope) {
		registry.Register(tool)
	}
	registerLocalControlTools(registry, workspaceRoot, config.LocalControlConfig{})
	return registry
}

func registerLocalControlTools(registry *tools.Registry, workspaceRoot string, cfg config.LocalControlConfig) {
	if registry == nil {
		return
	}
	browserOptions := localBrowserOptionsFromConfig(cfg)
	desktopOptions := localDesktopOptionsFromConfig(cfg)
	terminalOptions := localTerminalOptionsFromConfig(cfg)
	for _, tool := range tools.NewLocalBrowserTools(browserOptions) {
		registry.Register(tool)
	}
	for _, tool := range tools.NewLocalDesktopTools(desktopOptions) {
		registry.Register(tool)
	}
	for _, tool := range tools.NewLocalTerminalTools(terminalOptions) {
		registry.Register(tool)
	}
	for _, tool := range tools.NewLocalControlArtifactTools(tools.LocalControlArtifactOptions{
		Browser:      browserOptions,
		Desktop:      desktopOptions,
		Terminal:     terminalOptions,
		ArtifactsDir: localArtifactsDirFromConfig(workspaceRoot, cfg),
	}) {
		registry.Register(tool)
	}
}

func localBrowserOptionsFromConfig(cfg config.LocalControlConfig) localcontrol.BrowserOptions {
	return localcontrol.BrowserOptions{
		Enabled:    cfg.BrowserEnabled(),
		Driver:     cfg.Browser.Driver,
		HelperPath: cfg.Browser.HelperPath,
	}
}

func localDesktopOptionsFromConfig(cfg config.LocalControlConfig) localcontrol.DesktopOptions {
	return localcontrol.DesktopOptions{
		Enabled:    cfg.DesktopEnabled(),
		Driver:     cfg.Desktop.Driver,
		HelperPath: cfg.Desktop.HelperPath,
	}
}

func localTerminalOptionsFromConfig(cfg config.LocalControlConfig) localcontrol.TerminalOptions {
	return localcontrol.TerminalOptions{
		Enabled:    cfg.TerminalEnabled(),
		Driver:     cfg.Terminal.Driver,
		HelperPath: cfg.Terminal.HelperPath,
	}
}

func localArtifactsDirFromConfig(workspaceRoot string, cfg config.LocalControlConfig) string {
	dir := strings.TrimSpace(cfg.ArtifactsDir)
	if dir == "" {
		dir = filepath.Join(".zero", "artifacts")
	}
	if filepath.IsAbs(dir) {
		return dir
	}
	return filepath.Join(workspaceRoot, dir)
}

// agentToolRuntime bundles the specialist runtime with the swarm it backs so
// their lifetimes (and shutdown) stay paired: registerSpecialistTools brings up
// both, closeSpecialistRuntime tears down both.
type agentToolRuntime struct {
	specialist *specialist.Runtime
	swarm      *swarm.Swarm
	// specialists summarizes the registered specialists (name + description) for
	// the orchestrator's system-prompt delegation section. Populated alongside the
	// tools, so it is present exactly when the Task tool is.
	specialists []agent.SpecialistInfo
}

// specialistInfos returns the runtime's specialist summaries, nil-safe so the
// exec path can call it whether or not specialist tools were registered.
func (r *agentToolRuntime) specialistInfos() []agent.SpecialistInfo {
	if r == nil {
		return nil
	}
	return r.specialists
}

func registerSpecialistTools(registry *tools.Registry, workspaceRoot string, maxTeamSize int) (*agentToolRuntime, error) {
	paths, err := specialist.DefaultPaths(workspaceRoot)
	if err != nil {
		return nil, err
	}
	executor := specialist.Executor{Paths: paths}
	runtime, err := specialist.RegisterTools(registry, executor)
	if err != nil {
		return nil, err
	}
	// The swarm reuses the same specialist executor to launch each member, so
	// every member runs under the orchestrator's sandbox + policy. Mailbox state
	// lives under the workspace so its files fall within the sandbox write rules.
	// MaxTeamSize (0 => the swarm's default of 8) caps concurrent members per team.
	sw, err := swarm.New(swarm.Options{
		BaseDir:     filepath.Join(workspaceRoot, ".zero", "swarm"),
		Launcher:    swarm.NewSpecialistLauncher(executor),
		MaxTeamSize: maxTeamSize,
	})
	if err != nil {
		runtime.Close()
		return nil, err
	}
	swarm.RegisterTools(registry, sw)
	return &agentToolRuntime{specialist: runtime, swarm: sw, specialists: specialistSummaries(paths)}, nil
}

// specialistSummaries loads the available specialists (built-ins + user/project
// profiles) and returns their name + description for the orchestrator's
// delegation prompt. A load error yields no summaries (the prompt simply omits
// the delegation section) rather than failing the run.
func specialistSummaries(paths specialist.Paths) []agent.SpecialistInfo {
	result, err := specialist.Load(specialist.LoadOptions{Paths: paths})
	if err != nil {
		return nil
	}
	summaries := make([]agent.SpecialistInfo, 0, len(result.Specialists))
	for _, manifest := range result.Specialists {
		name := strings.TrimSpace(manifest.Metadata.Name)
		if name == "" {
			continue
		}
		summaries = append(summaries, agent.SpecialistInfo{
			Name:      name,
			WhenToUse: strings.TrimSpace(manifest.Metadata.Description),
		})
	}
	return summaries
}

func shouldRegisterExecSpecialistTools(options execOptions) bool {
	if options.useSpec {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(options.tag), specialist.SessionTagSpecialist) {
		return false
	}
	// Register at medium/high (and unsafe). The Task tool's permission gate
	// auto-approves only read-only specialist spawns and prompts for write-capable
	// ones, so a non-trivial exec can auto-delegate exploration safely (a headless
	// write spawn still gets denied at the prompt). Default "low" stays clean — no
	// specialist tooling or swarm runtime for trivial/CI one-shots.
	autonomy := strings.ToLower(strings.TrimSpace(options.autonomy))
	return options.skipPermissionsUnsafe || autonomy == "high" || autonomy == "medium"
}

func closeMCPRuntime(stderr io.Writer, runtime mcpToolRuntime) {
	if runtime == nil {
		return
	}
	if err := runtime.Close(); err != nil {
		_, _ = fmt.Fprintf(stderr, "[zero] mcp_close_error: %s\n", err)
	}
}

func closeSpecialistRuntime(stderr io.Writer, runtime *agentToolRuntime) {
	if runtime == nil {
		return
	}
	if runtime.swarm != nil {
		runtime.swarm.Close()
	}
	if runtime.specialist != nil {
		if err := runtime.specialist.Close(); err != nil {
			_, _ = fmt.Fprintf(stderr, "[zero] specialist_cleanup_error: %s\n", err)
		}
	}
}

// stdoutIsTerminal reports whether w is an interactive terminal. Tests pass
// bytes.Buffer writers and pipes/redirects fail term.IsTerminal, so both take
// the machine-readable path.
func stdoutIsTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	return term.IsTerminal(f.Fd())
}

func writeAppError(stderr io.Writer, message string, exitCode int) int {
	if _, err := fmt.Fprintf(stderr, "[zero] %s\n", message); err != nil {
		return 1
	}
	return exitCode
}

func writeUsageError(stderr io.Writer, message string) int {
	return writeExecUsageError(stderr, message)
}

func writeHelp(w io.Writer) error {
	_, err := fmt.Fprint(w, `ZERO terminal coding agent

Usage:
  zero [command]

Commands:
  exec       Run a one-shot prompt through the Go agent runtime
  completions Generate shell completion scripts
  daemon     Manage the local background worker daemon (start/stop/status/run/attach)
  setup      Guide first-run provider setup
  config     Inspect resolved Go configuration without leaking secrets
  models     List Zero model registry entries
  providers  Inspect resolved provider profiles
  doctor     Run backend health checks for config and provider setup
  context    Report workspace context budget usage
  repo-map   Build a deterministic repository map for agent context
  search     Search persisted local Zero session events
  find       Alias for search
  sessions   Inspect local Zero session lineage
  spec       Review and approve saved spec-mode drafts
  specialist Manage local Zero specialist profiles
  plugins    Inspect, install, and remove local Zero plugins
  backends   Inspect MCP, hook, and plugin backend lifecycle state
  skills     Inspect, install, and remove local Zero skills
  tools      Scaffold and list local Zero plugin-tools
  hooks      Inspect Zero hook configuration
  mcp        Manage MCP backend settings
  auth       Log in to model providers via OAuth
  sandbox    Inspect sandbox policy and persistent grants
  update     Check or apply Zero CLI updates (requires --check or --apply)
  upgrade    Download, verify, and install available Zero CLI updates
  worktrees  Prepare isolated git worktrees
  verify     Detect and run local verification checks
  eval       Validate offline agent eval suites
  changes    Inspect and commit local git changes
  usage      Summarize token usage and estimated cost
  cron       Schedule agent jobs (foreground, file-backed)
  repo-info  Characterize the current repository (local git only)
  serve      Run Zero protocol servers
  acp        Serve the Agent Client Protocol over stdio (editor backend)
  help       Show this help
  version    Print version

Flags:
  -h, --help                     Show this help
  -v, --version                  Print version
  -p, --prompt                   Run a one-shot prompt
      --add-dir <path>           Allow writes in an extra directory (repeatable)
      --allow-escalation         Let the agent escalate to a stronger model mid-run via escalate_model
      --skip-permissions-unsafe  Launch the interactive shell in unsafe mode (enables the ! shell escape)
`)
	return err
}

// addDirFlagArgs rebuilds "--add-dir <dir>" flag pairs so dirs collected by
// splitLeadingAddDirFlags can be forwarded into exec's own argument parser
// (which accepts --add-dir anywhere) instead of being silently dropped.
func addDirFlagArgs(addDirs []string) []string {
	flags := make([]string, 0, 2*len(addDirs))
	for _, dir := range addDirs {
		flags = append(flags, "--add-dir", dir)
	}
	return flags
}

// rootFlagArgs re-synthesises the root flags exec consumes, in the spelling
// its own parser accepts, so a flag written before the subcommand reaches
// the run exactly as one written after it would.
func rootFlagArgs(addDirs []string, allowEscalation bool) []string {
	flags := addDirFlagArgs(addDirs)
	if allowEscalation {
		flags = append(flags, "--allow-escalation")
	}
	return flags
}

// splitLeadingAddDirFlags strips leading --add-dir flags from the root
// argument list (zero --add-dir <path> [--add-dir <path>] [subcommand …]).
// Subcommands like exec parse their own --add-dir occurrences.
func splitLeadingAddDirFlags(args []string) ([]string, []string, error) {
	addDirs := []string{}
	for len(args) > 0 {
		switch {
		case args[0] == "--add-dir":
			if len(args) < 2 {
				return nil, nil, errors.New("--add-dir requires a directory path")
			}
			value := strings.TrimSpace(args[1])
			if value == "" {
				return nil, nil, errors.New("--add-dir requires a directory path")
			}
			if strings.HasPrefix(value, "-") {
				return nil, nil, errors.New("--add-dir requires a directory path")
			}
			addDirs = append(addDirs, value)
			args = args[2:]
		case strings.HasPrefix(args[0], "--add-dir="):
			value := strings.TrimSpace(strings.TrimPrefix(args[0], "--add-dir="))
			if value == "" {
				return nil, nil, errors.New("--add-dir requires a directory path")
			}
			// Match the space form: a flag-like value is almost certainly a
			// mistyped option, not a directory. A directory literally named
			// "-foo" is still reachable as --add-dir=./-foo.
			if strings.HasPrefix(value, "-") {
				return nil, nil, errors.New("--add-dir requires a directory path")
			}
			addDirs = append(addDirs, value)
			args = args[1:]
		default:
			return addDirs, args, nil
		}
	}
	return addDirs, args, nil
}

// rootFlags is what the leading root flags amount to once every one of them
// has been stripped from the front of the argument list.
type rootFlags struct {
	addDirs         []string
	theme           string
	allowEscalation bool
}

// splitLeadingRootFlags strips --add-dir, --theme and --allow-escalation from
// the front of args in whatever order they were written, stopping at the
// first token none of them claims.
//
// EACH SPLITTER STOPS AT THE FIRST TOKEN IT DOES NOT OWN, so running them once
// in a fixed sequence made the order the operator wrote them in load-bearing:
// a flag handled late in the sequence stranded every flag written after it as
// an unknown command, and `zero --allow-escalation --theme auto` exited with
// an argument error instead of launching. Running the sequence until it makes
// no progress accepts every ordering, including a flag repeated on both sides
// of another.
func splitLeadingRootFlags(args []string) (rootFlags, []string, error) {
	var flags rootFlags
	for {
		before := len(args)
		addDirs, rest, err := splitLeadingAddDirFlags(args)
		if err != nil {
			return rootFlags{}, nil, err
		}
		flags.addDirs = append(flags.addDirs, addDirs...)
		theme, rest, err := splitLeadingThemeFlag(rest)
		if err != nil {
			return rootFlags{}, nil, err
		}
		if theme != "" {
			// The last occurrence wins across passes, as it does within one.
			flags.theme = theme
		}
		allowEscalation, rest, err := splitLeadingAllowEscalationFlag(rest)
		if err != nil {
			return rootFlags{}, nil, err
		}
		flags.allowEscalation = flags.allowEscalation || allowEscalation
		args = rest
		if len(args) == before {
			return flags, args, nil
		}
	}
}

// splitLeadingAllowEscalationFlag strips a leading --allow-escalation from the
// root argument list, opting the interactive session into mid-run model
// escalation.
//
// OPT-IN, THE SAME WAY exec IS. Escalation moves a run onto a different model,
// which changes what the run costs and which provider sees the conversation, so
// it is a decision the operator makes rather than a default. The exec flag
// already answers this conservatively and the interactive surface should not
// answer it differently.
//
// Bare flag only: repeating it is harmless, and an =value form is rejected so a
// mistyped --allow-escalation=false is a loud error instead of silently enabling
// the thing it was trying to turn off.
func splitLeadingAllowEscalationFlag(args []string) (bool, []string, error) {
	allow := false
	for len(args) > 0 {
		switch {
		case args[0] == "--allow-escalation":
			allow = true
			args = args[1:]
		case strings.HasPrefix(args[0], "--allow-escalation="):
			return false, nil, errors.New("--allow-escalation takes no value; pass it bare to enable mid-run model escalation, or omit it")
		default:
			return allow, args, nil
		}
	}
	return allow, args, nil
}

// splitLeadingThemeFlag strips a leading --theme <auto|theme-name> (space or =form)
// from the root argument list and validates it against the registered themes. The
// last occurrence wins. A value outside the allowed set is a loud error rather than
// a silent fallback.
func splitLeadingThemeFlag(args []string) (string, []string, error) {
	theme := ""
	for len(args) > 0 {
		var value string
		switch {
		case args[0] == "--theme":
			if len(args) < 2 {
				return "", nil, errors.New("--theme requires a value (auto or a theme name; try --theme auto)")
			}
			value = strings.TrimSpace(args[1])
			args = args[2:]
		case strings.HasPrefix(args[0], "--theme="):
			value = strings.TrimSpace(strings.TrimPrefix(args[0], "--theme="))
			args = args[1:]
		default:
			return theme, args, nil
		}
		if !tui.ValidThemeArg(value) {
			return "", nil, fmt.Errorf("--theme must be auto or a registered theme name (got %q)", value)
		}
		theme = strings.ToLower(value)
	}
	return theme, args, nil
}

// profileHasCredential reports whether the profile can authenticate: a direct API
// key, an auth-header value, or an APIKeyEnv whose environment variable is set. The
// setup result is not env-resolved, so checking APIKey alone would wrongly flag every
// env-var-based provider as keyless.
func profileHasCredential(profile config.ProviderProfile) bool {
	if strings.TrimSpace(profile.APIKey) != "" || profile.APIKeyStored || strings.TrimSpace(profile.AuthHeaderValue) != "" {
		return true
	}
	if env := strings.TrimSpace(profile.APIKeyEnv); env != "" {
		return strings.TrimSpace(os.Getenv(env)) != ""
	}
	return false
}

// baseURLIsLoopback reports whether a provider base_url points at a loopback host
// or a private-network host (192.168.x.y / 10.x.y.z / 172.16.x.y) — a local
// provider like Ollama/LM Studio that needs no API key.
func baseURLIsLoopback(baseURL string) bool {
	u := strings.TrimSpace(baseURL)
	if u == "" {
		return false
	}
	parsed, err := url.Parse(u)
	if err != nil || parsed.Host == "" {
		return false
	}
	host := strings.TrimSuffix(strings.ToLower(parsed.Hostname()), ".")
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		return addr.IsLoopback() || addr.IsPrivate()
	}
	return false
}

func writePromptRequired(stderr io.Writer) int {
	if _, err := fmt.Fprintln(stderr, "[zero] Prompt required. Use `zero exec \"prompt\"` or `zero exec --file prompt.txt`."); err != nil {
		return 1
	}
	return 2
}

func writeExecHelp(w io.Writer) error {
	_, err := fmt.Fprint(w, `Usage:
  zero exec [flags] [prompt]

Runs a one-shot prompt through the Go agent runtime.

Flags:
  -f, --file <path>                  Read prompt text from a file
      --image <path>                 Attach a local image (repeatable; vision models only)
      --add-dir <path>               Allow writes in an extra directory (repeatable)
      --mode <name>                  Apply a preset (smart, deep, fast, large, precise); explicit flags override it
  -m, --model <model>                Select the model for provider setup
      --use-spec                     Draft a spec first and stop for review
      --spec-model <model>           Override the draft model when --use-spec is set
      --spec-reasoning-effort <effort>
                                    Override draft reasoning effort when --use-spec is set
      --plan                         Read-only planning mode: write and shell tools are hidden
      --permission-mode <mode>       Set permission mode directly (plan, spec-draft, auto, member,
                                    ask, unsafe). Outranks --auto; prefer --plan / --auto for
                                    interactive use. Used by specialist/swarm child processes.
      --max-turns <number>           Override the maximum agent loop turns
      --exec-profile <name>          Apply an execution profile (balanced, fast, thorough): loop
                                    posture only (turn budget, effort, self-correction, escalation);
                                    composes with --mode (which picks the model) and explicit flags win
      --auto <low|medium|high>       Set exec autonomy; high enables unsafe tools
      --enabled-tools <tools>        Only expose these comma or space separated tools
      --disabled-tools <tools>       Hide these comma or space separated tools
      --list-tools                   List model-visible tools and exit
      --profile <profile>            Accept legacy model profile selection
  -r, --reasoning-effort <effort>    Accept legacy reasoning effort selection
  -C, --cwd <path>                   Set the workspace directory
  -w, --worktree [name]              Run from an isolated git worktree
      --worktree-dir <path>          Base directory for created worktrees
  -i, --input-format text|stream-json
                                    Select prompt input format
  -o, --output-format text|json|stream-json
                                    Select text, JSON, or schema-versioned JSONL output
                                    ("debug" is accepted as a stream-json alias)
      --prompt <prompt>              Provide prompt text as a flag
      --resume [id]                  Resume a session; omit id to use the latest
      --fork <id>                    Fork an existing session into a new session
      --calling-session-id <id>      Parent session id for specialist child runs
      --calling-tool-use-id <id>     Parent tool-call id for specialist child runs
      --tag <tag>                    Attach runtime tag metadata to the exec run
      --depth <number>               Set specialist nesting depth metadata
      --session-title <text>         Set the created session title
      --init-session-id <id>         Create a new exec session with this id
      --skip-permissions-unsafe      Allow prompt-gated tools without approval
      --allow-escalation             Let the agent escalate to a stronger model mid-run via escalate_model
      --self-correct                 Run the post-edit verify-and-correct loop (auto-fix needs --auto medium or high)
      --notify <off|bell|notify|both>
                                    Override notification mode for this run
      --no-notify                   Disable notifications for this run
      --no-completion-gate          Accept a no-tool-call reply as final without the INCOMPLETE
                                    downgrade (for conversational callers with an operator present)
`)
	return err
}

// cachedSkillsLoader memoizes a skills loader for a short interval. The TUI
// calls the loader from autocomplete on every "/x" keystroke; without a cache
// each keystroke would re-read every SKILL.md body. Two seconds is fresh enough
// that a newly installed skill still shows up "immediately" while typing.
func cachedSkillsLoader(load func() []skills.Skill) func() []skills.Skill {
	var (
		mu     sync.Mutex
		at     time.Time
		cached []skills.Skill
	)
	const ttl = 2 * time.Second
	return func() []skills.Skill {
		mu.Lock()
		defer mu.Unlock()
		if cached != nil && time.Since(at) < ttl {
			return cached
		}
		cached = load()
		at = time.Now()
		return cached
	}
}
