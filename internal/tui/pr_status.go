package tui

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"charm.land/lipgloss/v2"

	"github.com/Gitlawb/zero/internal/gitsafe"
)

type PrStatus string

const (
	PrIdle     PrStatus = "idle"
	PrLoading  PrStatus = "loading"
	PrFound    PrStatus = "found"
	PrNotFound PrStatus = "notfound"
)

type PrProvider string

const (
	ProviderGitHub PrProvider = "github"
	ProviderGitLab PrProvider = "gitlab"
)

type PrState struct {
	Status    PrStatus
	PrURL     string
	PrNumber  string
	Provider  PrProvider
	Additions int
	Deletions int
}

type PrService struct {
	mu        sync.RWMutex
	state     PrState
	listeners []func(PrState)
	cwd       string
	run       prCommandRunner
}

type prCommandRunner func(context.Context, string, string, ...string) (string, error)

const prRefreshInterval = 60 * time.Second
const prCommandTimeout = 8 * time.Second

func NewPrService(cwdValues ...string) *PrService {
	cwd := ""
	if len(cwdValues) > 0 {
		cwd = cwdValues[0]
	}
	if strings.TrimSpace(cwd) == "" {
		if current, err := os.Getwd(); err == nil {
			cwd = current
		}
	}
	return &PrService{
		state: PrState{Status: PrIdle},
		cwd:   cwd,
		run:   defaultPRCommandRunner,
	}
}

func (s *PrService) GetState() PrState {
	if s == nil {
		return PrState{Status: PrIdle}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state
}

func (s *PrService) Subscribe(fn func(PrState)) func() {
	if s == nil || fn == nil {
		return func() {}
	}
	s.mu.Lock()
	s.listeners = append(s.listeners, fn)
	index := len(s.listeners) - 1
	s.mu.Unlock()

	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if index < 0 || index >= len(s.listeners) || s.listeners[index] == nil {
			return
		}
		s.listeners[index] = nil
	}
}

func (s *PrService) Refresh() {
	if s == nil {
		return
	}
	if s.GetState().Status == PrIdle {
		s.setState(PrState{Status: PrLoading})
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), prCommandTimeout)
		defer cancel()
		s.setState(s.detect(ctx))
	}()
}

func (s *PrService) setState(state PrState) {
	s.mu.Lock()
	s.state = state
	listeners := append([]func(PrState){}, s.listeners...)
	s.mu.Unlock()

	for _, listener := range listeners {
		if listener != nil {
			listener(state)
		}
	}
}

func (s *PrService) detect(ctx context.Context) PrState {
	if s == nil {
		return PrState{Status: PrNotFound}
	}
	run := s.run
	if run == nil {
		run = defaultPRCommandRunner
	}
	cwd := s.cwd
	if strings.TrimSpace(cwd) == "" {
		if current, err := os.Getwd(); err == nil {
			cwd = current
		}
	}

	if state, ok := detectGitHubPR(ctx, cwd, run); ok {
		return state
	}
	if state, ok := detectGitLabMR(ctx, cwd, run); ok {
		return state
	}
	return PrState{Status: PrNotFound}
}

func WatchPRStateContext(ctx context.Context, service *PrService, onChange func(PrState)) func() {
	if service == nil || onChange == nil {
		return func() {}
	}
	ctx, cancel := context.WithCancel(ctx)
	onChange(service.GetState())
	unsubscribe := service.Subscribe(onChange)
	service.Refresh()

	go func() {
		ticker := time.NewTicker(prRefreshInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				service.Refresh()
			}
		}
	}()

	return func() {
		cancel()
		unsubscribe()
	}
}

type githubPRView struct {
	URL         string          `json:"url"`
	Number      json.RawMessage `json:"number"`
	BaseRefName string          `json:"baseRefName"`
}

type gitlabMRView struct {
	WebURL       string          `json:"web_url"`
	IID          json.RawMessage `json:"iid"`
	TargetBranch string          `json:"target_branch"`
}

func detectGitHubPR(ctx context.Context, cwd string, run prCommandRunner) (PrState, bool) {
	output, err := run(ctx, cwd, "gh", "pr", "view", "--json", "url,number,baseRefName")
	if err != nil {
		return PrState{}, false
	}
	var view githubPRView
	if err := json.Unmarshal([]byte(output), &view); err != nil {
		return PrState{}, false
	}
	state := PrState{
		Status:   PrFound,
		Provider: ProviderGitHub,
		PrURL:    strings.TrimSpace(view.URL),
		PrNumber: jsonScalarString(view.Number),
	}
	if state.PrNumber == "" {
		return PrState{}, false
	}
	state.Additions, state.Deletions, _ = getLocalDiffStats(ctx, cwd, view.BaseRefName, run)
	return state, true
}

func detectGitLabMR(ctx context.Context, cwd string, run prCommandRunner) (PrState, bool) {
	output, err := run(ctx, cwd, "glab", "mr", "view", "--json", "web_url,iid,target_branch")
	if err != nil {
		return PrState{}, false
	}
	var view gitlabMRView
	if err := json.Unmarshal([]byte(output), &view); err != nil {
		return PrState{}, false
	}
	state := PrState{
		Status:   PrFound,
		Provider: ProviderGitLab,
		PrURL:    strings.TrimSpace(view.WebURL),
		PrNumber: jsonScalarString(view.IID),
	}
	if state.PrNumber == "" {
		return PrState{}, false
	}
	state.Additions, state.Deletions, _ = getLocalDiffStats(ctx, cwd, view.TargetBranch, run)
	return state, true
}

func getLocalDiffStats(ctx context.Context, cwd string, baseBranch string, run prCommandRunner) (int, int, error) {
	baseBranch = strings.TrimSpace(baseBranch)
	if baseBranch == "" {
		return 0, 0, nil
	}
	originBase := "origin/" + baseBranch
	mergeBase, err := run(ctx, cwd, "git", "merge-base", originBase, "HEAD")
	if err != nil {
		mergeBase = originBase
	}
	output, err := run(ctx, cwd, "git", "diff", "--numstat", "--ignore-submodules=dirty", strings.TrimSpace(mergeBase))
	if err != nil {
		return 0, 0, err
	}
	additions, deletions := parseGitNumStat(output)
	return additions, deletions, nil
}

func parseGitNumStat(output string) (int, int) {
	additions := 0
	deletions := 0
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		additions += parseNumStatValue(fields[0])
		deletions += parseNumStatValue(fields[1])
	}
	return additions, deletions
}

func parseNumStatValue(value string) int {
	count, err := strconv.Atoi(value)
	if err != nil {
		return 0
	}
	return count
}

func jsonScalarString(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return ""
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return strings.TrimSpace(text)
	}
	var number int
	if err := json.Unmarshal(raw, &number); err == nil {
		return strconv.Itoa(number)
	}
	return ""
}

func defaultPRCommandRunner(ctx context.Context, cwd string, name string, args ...string) (string, error) {
	var command *exec.Cmd
	if name == "git" {
		// Every git call the TUI makes on its own comes through here (the files
		// sweep and the pull request diffstat), so the repository being shown
		// cannot make them run a program.
		safe, err := gitsafe.Command(ctx, cwd, args...)
		if err != nil {
			return "", fmt.Errorf("%s: %w", name, err)
		}
		command = safe
	} else {
		command = exec.CommandContext(ctx, name, args...)
		if strings.TrimSpace(cwd) != "" {
			command.Dir = cwd
		}
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = err.Error()
		}
		return "", fmt.Errorf("%s: %s", name, detail)
	}
	return stdout.String(), nil
}

type Color string

const (
	ColorDefault Color = "default"
	ColorAccent  Color = "accent"
	ColorGreen   Color = "green"
	ColorRed     Color = "red"
)

type PrSegment struct {
	Text  string
	Color Color
	Bold  bool
	URL   string
}

func BuildPRSegments(state PrState, useNerdFont bool) []PrSegment {
	if state.Status != PrFound || strings.TrimSpace(state.PrNumber) == "" {
		return nil
	}
	url := strings.TrimSpace(state.PrURL)
	segments := []PrSegment{}
	if useNerdFont {
		segments = append(segments, PrSegment{Text: " ", Color: ColorAccent, Bold: true, URL: url})
	}
	segments = append(segments,
		PrSegment{Text: "+" + strconv.Itoa(maxInt(0, state.Additions)), Color: ColorGreen, Bold: true, URL: url},
		PrSegment{Text: " ", Color: ColorDefault, URL: url},
		PrSegment{Text: "-" + strconv.Itoa(maxInt(0, state.Deletions)), Color: ColorRed, Bold: true, URL: url},
		PrSegment{Text: " ", Color: ColorDefault, URL: url},
		PrSegment{Text: "#" + strings.TrimSpace(state.PrNumber), Color: ColorAccent, Bold: true, URL: url},
	)
	return segments
}

func renderPRSegments(segments []PrSegment) string {
	var builder strings.Builder
	for _, segment := range segments {
		text := renderPRSegment(segment)
		if segment.URL != "" {
			text = hyperlink(segment.URL, text)
		}
		builder.WriteString(text)
	}
	return builder.String()
}

func renderPRSegment(segment PrSegment) string {
	style := lipgloss.NewStyle()
	switch segment.Color {
	case ColorAccent:
		style = zeroTheme.accent
	case ColorGreen:
		style = zeroTheme.gitAdd
	case ColorRed:
		style = zeroTheme.gitDel
	case ColorDefault:
		style = zeroTheme.muted
	}
	if segment.Bold {
		style = style.Bold(true)
	}
	return style.Render(segment.Text)
}
