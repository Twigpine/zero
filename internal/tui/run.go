package tui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/term"

	"github.com/Gitlawb/zero/internal/peermsg"
	"github.com/Gitlawb/zero/internal/terminalpet"
)

// Run starts the Zero Bubble Tea shell and returns a process-style exit code.
func Run(ctx context.Context, options Options) int {
	// The interactive shell needs a real terminal on stdin: with piped or
	// redirected input Bubble Tea blocks forever waiting for events that never
	// arrive (e.g. `echo "" | zero`). Fail fast with guidance toward the headless
	// path instead of hanging. term.IsTerminal is a true TTY check (it rejects
	// pipes, regular files, and non-terminal char devices like /dev/null) and
	// fails closed — anything that is not a verified terminal blocks the shell.
	if !term.IsTerminal(os.Stdin.Fd()) {
		fmt.Fprintln(os.Stderr, "zero: the interactive shell needs a terminal (stdin is not a TTY). For non-interactive use, run: zero exec \"<prompt>\"")
		return 2
	}

	externalSink := options.RuntimeMessageSink
	var program *tea.Program
	forward := func(msg tea.Msg) {
		if externalSink != nil {
			externalSink(msg)
		}
		if program != nil {
			program.Send(msg)
		}
	}
	// Coalesce streamed assistant-text deltas to ~one frame each so a fast provider
	// can't drive a full Update→View per token; every other message flushes pending
	// text first, keeping order intact.
	options.RuntimeMessageSink = newTextCoalescer(forward).send
	options.AltScreen = useAltScreen(options)
	imageSupport := terminalpet.DetectImageSupport(os.Getenv)
	imageCache := terminalPetFrameCache(options)
	petRenderer := terminalpet.NewImageRendererWithCache(imageSupport, imageCache)
	attachmentRenderers := make([]*terminalpet.ImageRenderer, attachmentPreviewMaxImages)
	outputRenderers := make([]*terminalpet.ImageRenderer, 0, 1+len(attachmentRenderers))
	outputRenderers = append(outputRenderers, petRenderer)
	for index := range attachmentRenderers {
		attachmentRenderers[index] = terminalpet.NewImageRendererWithCache(imageSupport, imageCache)
		outputRenderers = append(outputRenderers, attachmentRenderers[index])
	}
	petOutput := newPetImageOutput(os.Stdout, outputRenderers...)

	programOpts := []tea.ProgramOption{
		tea.WithContext(ctx),
		tea.WithInput(os.Stdin),
		tea.WithOutput(petOutput),
		tea.WithFilter(mouseEventFilter()),
	}
	// Honor the no-color.org spec ourselves: NO_COLOR set to ANY non-empty value
	// disables color. bubbletea/colorprofile only respects strconv.ParseBool-style
	// values, so NO_COLOR=yes / NO_COLOR=foo would otherwise leave the UI in full
	// color. Force the Ascii (no-color, bold-still-allowed) profile. (AUDIT-M3)
	if noColorRequested(os.Getenv) {
		programOpts = append(programOpts, tea.WithColorProfile(colorprofile.Ascii))
	}
	initialModel := newModel(ctx, options)
	// Apply the terminal-native system default at the real interactive entry point.
	// Constructing a model alone is also useful to tests and non-TUI helpers, and
	// should not mutate the package-level render palette for those callers.
	applyTheme(initialModel.themeMode, initialModel.hasDarkBg)
	initialModel.petRenderer = petRenderer
	initialModel.attachmentRenderers = attachmentRenderers
	if initialModel.wantsMouseCapture() {
		initialModel.mouseCapture = true
	}
	program = tea.NewProgram(initialModel, programOpts...)
	peerStarted := false
	if options.PeerService != nil {
		options.PeerService.SetStatusHandler(func(event peermsg.StatusEvent) {
			forward(peerStatusMsg{event: event})
		})
		options.PeerService.SetHeldEvictionHandler(func(messageID string) {
			forward(peerApprovalExpiredMsg{messageID: messageID})
		})
		options.PeerService.SetHeldReleaseHandler(func(message peermsg.InboundMessage) {
			forward(peerHeldReleasedMsg{message: message})
		})
		if err := options.PeerService.Start(func(message peermsg.InboundMessage) bool {
			admit := make(chan bool, 1)
			forward(peerMessageMsg{message: message, admit: admit})
			select {
			case accepted := <-admit:
				return accepted
			case <-time.After(4 * time.Second):
				return false
			}
		}); err != nil {
			forward(peerRuntimeErrorMsg{err: err})
		} else {
			peerStarted = true
		}
	}

	_, runErr := program.Run()
	clearErr := petOutput.clearImage()
	var closeErr error
	if peerStarted {
		closeErr = options.PeerService.Close()
	}
	if runErr != nil {
		// Surface the failure: exiting 1 with zero diagnostics left users
		// guessing why the default chat surface died.
		fmt.Fprintln(os.Stderr, "zero: tui error:", runErr)
		return 1
	}
	if closeErr != nil {
		fmt.Fprintln(os.Stderr, "zero: peer messaging cleanup error:", closeErr)
		return 1
	}
	if clearErr != nil {
		fmt.Fprintln(os.Stderr, "zero: terminal companion cleanup error:", clearErr)
		return 1
	}
	return 0
}

func terminalPetFrameCache(options Options) string {
	return terminalPetFrameCacheWith(options, os.UserConfigDir, os.UserCacheDir)
}

func terminalPetFrameCacheWith(options Options, userConfigDir, userCacheDir func() (string, error)) string {
	root := ""
	if configPath := strings.TrimSpace(options.UserConfigPath); filepath.IsAbs(configPath) {
		root = filepath.Dir(filepath.Clean(configPath))
	}
	if root == "" {
		configDir, err := userConfigDir()
		if err == nil && strings.TrimSpace(configDir) != "" {
			root = filepath.Join(configDir, "zero")
		}
	}
	if root == "" {
		cacheDir, err := userCacheDir()
		if err == nil && strings.TrimSpace(cacheDir) != "" {
			root = filepath.Join(cacheDir, "zero")
		}
	}
	if root == "" {
		return ""
	}
	return filepath.Join(root, "pets", "frame-cache")
}

func useAltScreen(_ Options) bool {
	return true
}

// noColorRequested reports whether the no-color.org spec is in effect: NO_COLOR set
// to any non-empty value. Checked here rather than via the colorprofile dependency,
// whose strconv.ParseBool gate ignores common values like NO_COLOR=yes. (AUDIT-M3)
func noColorRequested(getenv func(string) string) bool {
	return getenv("NO_COLOR") != ""
}
