package tui

import (
	"fmt"
	"net/url"
	"strings"
	"unicode"

	tea "charm.land/bubbletea/v2"

	internalmcp "github.com/Twigpine/zero/internal/mcp"
	"github.com/Twigpine/zero/internal/redaction"
)

const (
	mcpAddWizardMinWidth = 58
	mcpAddWizardMaxWidth = 92
)

type mcpAddWizardStep int

const (
	mcpAddWizardStepName mcpAddWizardStep = iota
	mcpAddWizardStepType
	mcpAddWizardStepEndpoint
	mcpAddWizardStepHeader
	mcpAddWizardStepConfirm
	mcpAddWizardStepResult
)

type mcpAddWizardState struct {
	step           mcpAddWizardStep
	serverName     string
	serverType     string
	endpoint       string
	headerInput    string
	headerKey      string
	sourceLabel    string
	sourceURL      string
	prerequisites  []string
	selectedType   int
	resultSelected int
	err            string
	result         mcpAddWizardResult
}

type mcpAddWizardResult struct {
	Title      string
	State      string
	Message    string
	Saved      bool
	Connected  bool
	ToolCount  int
	ActionHint string
}

var mcpAddWizardTypes = []struct {
	ID    string
	Label string
	Meta  string
}{
	{ID: "http", Label: "HTTP remote", Meta: "streamable HTTP endpoint"},
	{ID: "sse", Label: "SSE remote", Meta: "legacy server-sent events endpoint"},
	{ID: "stdio", Label: "Local stdio", Meta: "local command process"},
}

func newMCPAddWizard(defaultType string) *mcpAddWizardState {
	wizard := &mcpAddWizardState{step: mcpAddWizardStepName, serverType: "http"}
	defaultType = strings.ToLower(strings.TrimSpace(defaultType))
	for index, candidate := range mcpAddWizardTypes {
		if candidate.ID == defaultType {
			wizard.selectedType = index
			wizard.serverType = candidate.ID
			break
		}
	}
	return wizard
}

func (m model) openMCPAddWizard(defaultType string) model {
	m.cancelMCPCommand()
	m.mcpManager = nil
	m.mcpAddWizard = newMCPAddWizard(defaultType)
	m.clearSuggestions()
	return m
}

func (m model) openMCPAddWizardFromIntent(intent mcpSetupIntent) model {
	m.cancelMCPCommand()
	m.mcpManager = nil
	wizard := newMCPAddWizard(intent.ServerType)
	wizard.serverName = strings.TrimSpace(intent.ServerName)
	wizard.serverType = strings.TrimSpace(intent.ServerType)
	wizard.endpoint = strings.TrimSpace(intent.Endpoint)
	wizard.sourceLabel = strings.TrimSpace(intent.SourceLabel)
	wizard.sourceURL = strings.TrimSpace(intent.SourceURL)
	wizard.prerequisites = append([]string{}, intent.Prerequisites...)
	for index, candidate := range mcpAddWizardTypes {
		if candidate.ID == wizard.serverType {
			wizard.selectedType = index
			break
		}
	}
	wizard.step = mcpAddWizardStepConfirm
	m.mcpAddWizard = wizard
	m.clearSuggestions()
	return m
}

func (m model) handleMCPAddWizardKey(msg tea.KeyMsg) (model, tea.Cmd) {
	if m.mcpAddWizard == nil {
		return m, nil
	}
	wizard := m.mcpAddWizard
	switch {
	case keyIs(msg, tea.KeyEsc):
		m.cancelMCPCommand()
		m.mcpAddWizard = nil
		return m, nil
	case keyBackspace(msg):
		wizard.deleteRune()
		return m, nil
	case keyCtrl(msg, 'u'):
		wizard.clearCurrentInput()
		return m, nil
	case keyIs(msg, tea.KeyLeft):
		wizard.back()
		return m, nil
	case keyIs(msg, tea.KeyUp):
		if wizard.step == mcpAddWizardStepType || wizard.step == mcpAddWizardStepResult {
			wizard.move(-1)
		}
		return m, nil
	case keyIs(msg, tea.KeyDown) || keyIs(msg, tea.KeyTab):
		if wizard.step == mcpAddWizardStepType || wizard.step == mcpAddWizardStepResult {
			wizard.move(1)
		}
		return m, nil
	case keyText(msg) != "":
		if wizard.step == mcpAddWizardStepResult {
			return m.handleMCPAddWizardResultShortcut(msg)
		}
		wizard.appendRunes(keyRunes(msg))
		return m, nil
	case keyIs(msg, tea.KeyEnter) || keyIs(msg, tea.KeyRight):
		if wizard.step == mcpAddWizardStepResult {
			return m.handleMCPAddWizardResultEnter()
		}
		return m.advanceMCPAddWizard()
	}
	return m, nil
}

func (m model) advanceMCPAddWizard() (model, tea.Cmd) {
	wizard := m.mcpAddWizard
	if wizard == nil {
		return m, nil
	}
	wizard.err = ""
	switch wizard.step {
	case mcpAddWizardStepName:
		name := strings.TrimSpace(wizard.serverName)
		if err := internalmcp.ValidateServerName(name); err != nil {
			wizard.err = err.Error()
			return m, nil
		}
		wizard.serverName = name
		wizard.step = mcpAddWizardStepType
	case mcpAddWizardStepType:
		wizard.serverType = mcpAddWizardTypes[clampInt(wizard.selectedType, 0, len(mcpAddWizardTypes)-1)].ID
		wizard.step = mcpAddWizardStepEndpoint
	case mcpAddWizardStepEndpoint:
		if wizard.isRemote() {
			if err := validateMCPAddWizardURL(wizard.endpoint); err != nil {
				wizard.result = mcpAddWizardResult{
					Title:      "MCP setup issue",
					State:      "not saved",
					Message:    "URL could not be parsed: " + redactMCPWizardDisplayValue(wizard.endpoint),
					ActionHint: "Edit URL",
				}
				wizard.step = mcpAddWizardStepResult
				return m, nil
			}
			wizard.step = mcpAddWizardStepHeader
			return m, nil
		}
		if strings.TrimSpace(wizard.endpoint) == "" {
			wizard.err = "command is required"
			return m, nil
		}
		wizard.step = mcpAddWizardStepConfirm
	case mcpAddWizardStepHeader:
		if strings.TrimSpace(wizard.headerInput) == "" {
			wizard.step = mcpAddWizardStepConfirm
			return m, nil
		}
		key, value, err := parseMCPWizardHeader(wizard.headerInput)
		if err != nil {
			wizard.err = err.Error()
			return m, nil
		}
		wizard.headerKey = key
		wizard.headerInput = key + "=" + value
		wizard.step = mcpAddWizardStepConfirm
	case mcpAddWizardStepConfirm:
		return m.saveMCPAddWizard(false)
	}
	return m, nil
}

func (m model) handleMCPAddWizardResultEnter() (model, tea.Cmd) {
	wizard := m.mcpAddWizard
	if wizard == nil {
		return m, nil
	}
	switch wizard.currentResultAction() {
	case "use", "manage":
		m.mcpAddWizard = nil
		return m.openMCPManager(), nil
	case "retry":
		return m.saveMCPAddWizard(false)
	case "save-disabled", "disable":
		return m.saveMCPAddWizard(true)
	case "discard", "remove":
		m.mcpAddWizard = nil
		return m, nil
	default:
		wizard.step = mcpAddWizardStepEndpoint
		wizard.err = ""
		return m, nil
	}
}

func (m model) handleMCPAddWizardResultShortcut(msg tea.KeyMsg) (model, tea.Cmd) {
	if m.mcpAddWizard == nil {
		return m, nil
	}
	switch strings.ToLower(keyText(msg)) {
	case "e", "r":
		m.mcpAddWizard.step = mcpAddWizardStepEndpoint
		m.mcpAddWizard.err = ""
	case "d":
		m.mcpAddWizard = nil
	case "s":
		return m.saveMCPAddWizard(true)
	}
	return m, nil
}

func (m model) saveMCPAddWizard(disabled bool) (model, tea.Cmd) {
	wizard := m.mcpAddWizard
	if wizard == nil {
		return m, nil
	}
	if m.mcpCommand == nil {
		wizard.result = mcpAddWizardResult{Title: "MCP setup issue", State: "not saved", Message: "MCP action unavailable", ActionHint: "Discard"}
		wizard.step = mcpAddWizardStepResult
		return m, nil
	}
	args, err := wizard.commandArgs(disabled)
	if err != nil {
		wizard.err = err.Error()
		return m, nil
	}
	wizard.result = mcpAddWizardResult{Title: "Testing MCP server", State: "running", Message: "Connecting and saving the MCP server...", ActionHint: "Esc cancels"}
	wizard.resultSelected = 0
	wizard.step = mcpAddWizardStepResult
	return m.startMCPCommand(mcpCommandRequest{origin: mcpCommandOriginWizard, args: args, wizardDisabled: disabled})
}

func (m model) applyMCPAddWizardSaveResult(result MCPCommandResult, disabled bool) model {
	wizard := m.mcpAddWizard
	if wizard == nil {
		return m
	}
	if result.ExitCode != 0 || strings.TrimSpace(result.Error) != "" {
		message := strings.TrimSpace(result.Error)
		if message == "" {
			message = strings.TrimSpace(result.Output)
		}
		if message == "" {
			message = "MCP command failed"
		}
		wizard.result = mcpAddWizardResult{
			Title:      "MCP setup issue",
			State:      "not saved",
			Message:    redaction.RedactString(message, redaction.Options{}),
			ActionHint: "Edit config",
		}
		wizard.step = mcpAddWizardStepResult
		wizard.resultSelected = 0
		return m
	}
	if len(result.Config.Servers) > 0 || len(m.mcpConfig.Servers) > 0 {
		m.mcpConfig = result.Config
		m.refreshMCPViewState()
	}
	server := result.Config.Servers[wizard.serverName]
	wizard.result = mcpAddWizardResult{
		Title:      "MCP server ready",
		State:      "connected",
		Saved:      true,
		Connected:  true,
		ToolCount:  0,
		ActionHint: "Use server",
	}
	if disabled || server.Disabled {
		wizard.result.Title = "MCP server saved"
		wizard.result.State = "disabled"
		wizard.result.Connected = false
		wizard.result.ActionHint = "Edit config"
	}
	wizard.step = mcpAddWizardStepResult
	wizard.resultSelected = 0
	return m
}

func (wizard *mcpAddWizardState) commandArgs(disabled bool) ([]string, error) {
	args := []string{"add", strings.TrimSpace(wizard.serverName), "--type", strings.TrimSpace(wizard.serverType)}
	if disabled {
		args = append(args, "--disabled")
	}
	if wizard.isRemote() {
		args = append(args, "--url", strings.TrimSpace(wizard.endpoint))
		if strings.TrimSpace(wizard.headerInput) != "" {
			key, value, err := parseMCPWizardHeader(wizard.headerInput)
			if err != nil {
				return nil, err
			}
			args = append(args, "--header", key+"="+value)
		}
		return args, nil
	}
	command, err := splitMCPCommandArgs(wizard.endpoint)
	if err != nil {
		return nil, err
	}
	if len(command) == 0 {
		return nil, fmt.Errorf("command is required")
	}
	args = append(args, "--")
	args = append(args, command...)
	return args, nil
}

func (wizard *mcpAddWizardState) appendRunes(runes []rune) {
	if wizard == nil {
		return
	}
	for _, r := range runes {
		if r == '\n' || r == '\r' || r == '\t' || unicode.IsControl(r) {
			continue
		}
		switch wizard.step {
		case mcpAddWizardStepName:
			wizard.serverName += string(r)
		case mcpAddWizardStepEndpoint:
			wizard.endpoint += string(r)
		case mcpAddWizardStepHeader:
			wizard.headerInput += string(r)
		}
	}
	wizard.err = ""
}

func (wizard *mcpAddWizardState) deleteRune() {
	if wizard == nil {
		return
	}
	switch wizard.step {
	case mcpAddWizardStepName:
		wizard.serverName = trimLastRune(wizard.serverName)
	case mcpAddWizardStepEndpoint:
		wizard.endpoint = trimLastRune(wizard.endpoint)
	case mcpAddWizardStepHeader:
		wizard.headerInput = trimLastRune(wizard.headerInput)
	}
	wizard.err = ""
}

func (wizard *mcpAddWizardState) clearCurrentInput() {
	if wizard == nil {
		return
	}
	switch wizard.step {
	case mcpAddWizardStepName:
		wizard.serverName = ""
	case mcpAddWizardStepEndpoint:
		wizard.endpoint = ""
	case mcpAddWizardStepHeader:
		wizard.headerInput = ""
		wizard.headerKey = ""
	}
	wizard.err = ""
}

func (wizard *mcpAddWizardState) back() {
	if wizard == nil {
		return
	}
	wizard.err = ""
	switch wizard.step {
	case mcpAddWizardStepType:
		wizard.step = mcpAddWizardStepName
	case mcpAddWizardStepEndpoint:
		wizard.step = mcpAddWizardStepType
	case mcpAddWizardStepHeader:
		wizard.step = mcpAddWizardStepEndpoint
	case mcpAddWizardStepConfirm:
		if wizard.isRemote() {
			wizard.step = mcpAddWizardStepHeader
		} else {
			wizard.step = mcpAddWizardStepEndpoint
		}
	case mcpAddWizardStepResult:
		wizard.step = mcpAddWizardStepEndpoint
	}
}

func (wizard *mcpAddWizardState) move(delta int) {
	if wizard == nil {
		return
	}
	if wizard.step == mcpAddWizardStepType {
		wizard.selectedType = ((wizard.selectedType+delta)%len(mcpAddWizardTypes) + len(mcpAddWizardTypes)) % len(mcpAddWizardTypes)
		wizard.serverType = mcpAddWizardTypes[wizard.selectedType].ID
		return
	}
	if wizard.step == mcpAddWizardStepResult {
		count := len(wizard.resultActions())
		if count > 0 {
			wizard.resultSelected = ((wizard.resultSelected+delta)%count + count) % count
		}
	}
}

func (wizard *mcpAddWizardState) isRemote() bool {
	return wizard == nil || wizard.serverType == "http" || wizard.serverType == "sse" || wizard.serverType == ""
}

func validateMCPAddWizardURL(value string) error {
	value = strings.TrimSpace(value)
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return fmt.Errorf("url could not be parsed")
	}
	return nil
}

func parseMCPWizardHeader(raw string) (string, string, error) {
	key, value, ok := strings.Cut(raw, "=")
	if !ok {
		key, value, ok = strings.Cut(raw, ":")
	}
	key = strings.TrimSpace(key)
	value = strings.TrimSpace(value)
	if !ok || key == "" || value == "" {
		return "", "", fmt.Errorf("header must be in format key: value")
	}
	return key, value, nil
}

func (wizard *mcpAddWizardState) resultActions() []string {
	if wizard == nil {
		return nil
	}
	if wizard.result.Connected {
		return []string{"use", "manage", "edit", "disable"}
	}
	if wizard.result.Saved {
		return []string{"retry", "edit", "disable", "remove"}
	}
	return []string{"edit", "save-disabled", "discard"}
}

func (wizard *mcpAddWizardState) currentResultAction() string {
	actions := wizard.resultActions()
	if len(actions) == 0 {
		return ""
	}
	return actions[clampInt(wizard.resultSelected, 0, len(actions)-1)]
}

func trimLastRune(value string) string {
	if value == "" {
		return ""
	}
	runes := []rune(value)
	return string(runes[:len(runes)-1])
}
