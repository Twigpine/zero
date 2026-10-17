package tui

import (
	"context"
	"fmt"
	"sort"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/Twigpine/zero/internal/config"
	"github.com/Twigpine/zero/internal/providercatalog"
	"github.com/Twigpine/zero/internal/sandbox"
	"github.com/Twigpine/zero/internal/tools"
	"github.com/Twigpine/zero/internal/zerocommands"
)

func (m model) toolsText() string {
	registered := m.registeredTools()
	count := len(registered)
	if len(registered) == 0 {
		return renderCommandCardTranscript(commandCard{
			Title:   "Tools",
			Summary: []string{"0 registered", "no tools available"},
			Sections: []commandCardSection{{
				Title: "Registry",
				Fields: []commandField{
					{Key: "registered", Value: "0"},
				},
			}},
			Actions: []string{"/mcp manage servers", "/permissions manage access"},
		})
	}

	names := make([]string, 0, count)
	for _, tool := range registered {
		names = append(names, tool.Name())
	}
	sort.Strings(names)
	available := make([]string, 0, len(names))
	for _, name := range names {
		available = append(available, commandBullet(name))
	}

	return renderCommandCardTranscript(commandCard{
		Title:   "Tools",
		Summary: []string{fmt.Sprintf("%d registered", count), "registered catalog"},
		Sections: []commandCardSection{
			{
				Title: "Registry",
				Fields: []commandField{
					{Key: "registered", Value: fmt.Sprint(count)},
				},
			},
			{
				Title: "Available",
				Lines: available,
			},
		},
		Actions: []string{"/mcp manage servers", "/permissions manage access"},
	})
}

func (m *model) mcpText() string {
	width := 0
	if m.width > 0 {
		width = chatWidth(m.width)
	}
	return renderMCPView(m.mcpViewState(), width)
}

func (m *model) refreshMCPViewState() {
	m.mcpViewStateCache = BuildMCPViewState(MCPStateOptions{
		Config:          m.mcpConfig,
		Registry:        m.registry,
		PermissionStore: m.mcpPermissionStore,
		PermissionMode:  string(m.permissionMode),
		TokenStore:      m.mcpTokenStore,
	})
	m.mcpViewStateReady = true
}

func (m *model) mcpViewState() MCPViewState {
	if m.mcpViewStateReady {
		return m.mcpViewStateCache
	}
	// Older tests may construct a zero-value model; keep that path useful, while
	// production refreshes the cache before any MCP view can render.
	m.refreshMCPViewState()
	return m.mcpViewStateCache
}

func (m model) startMCPTranscriptCommand(args string) (model, tea.Cmd) {
	args = strings.TrimSpace(args)
	if args == "" {
		m.transcript = appendTranscriptRow(m.transcript, transcriptRow{kind: rowSystem, tool: "mcp", text: m.mcpText()})
		return m, nil
	}
	parsedArgs, err := splitMCPCommandArgs(args)
	if err != nil {
		text := strings.Join([]string{
			"MCP action failed",
			err.Error(),
			"",
			m.mcpText(),
		}, "\n")
		m.transcript = appendTranscriptRow(m.transcript, transcriptRow{kind: rowSystem, tool: "mcp", text: text})
		return m, nil
	}
	return m.startMCPCommand(mcpCommandRequest{origin: mcpCommandOriginTranscript, raw: args, args: parsedArgs})
}

func (m model) startMCPCommand(request mcpCommandRequest) (model, tea.Cmd) {
	if m.mcpCommand == nil {
		result := MCPCommandResult{
			ExitCode: 1,
			Error:    "MCP action unavailable",
			Config:   m.mcpConfig,
		}
		return m.applyMCPCommandResultMessage(mcpCommandResultMsg{request: request, result: result}), nil
	}
	m.cancelMCPCommand()
	ctx := m.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithCancel(ctx)
	m.mcpCommandSeq++
	request.id = m.mcpCommandSeq
	request.args = append([]string{}, request.args...)
	m.mcpCommandCancel = cancel
	runner := m.mcpCommand
	return m, func() tea.Msg {
		return mcpCommandResultMsg{
			request: request,
			result:  runner(ctx, request.args),
		}
	}
}

func (m *model) cancelMCPCommand() {
	if m.mcpCommandCancel != nil {
		m.mcpCommandCancel()
		m.mcpCommandCancel = nil
		m.mcpCommandSeq++
	}
}

func (m model) applyMCPCommandResultMessage(msg mcpCommandResultMsg) model {
	if msg.request.id != 0 && msg.request.id != m.mcpCommandSeq {
		return m
	}
	m.mcpCommandCancel = nil
	switch msg.request.origin {
	case mcpCommandOriginManager:
		text := ""
		m, text = m.applyMCPCommandResult(strings.Join(msg.request.args, " "), msg.result)
		m.mcpManager = &mcpManagerState{selected: msg.request.managerSelected, query: msg.request.managerQuery}
		if items := m.mcpManagerItems(); len(items) > 0 {
			m.mcpManager.selected = clampInt(m.mcpManager.selected, 0, len(items)-1)
		}
		if text != "" {
			m.transcript = appendTranscriptRow(m.transcript, transcriptRow{kind: rowSystem, tool: "mcp", text: text})
		}
	case mcpCommandOriginWizard:
		m = m.applyMCPAddWizardSaveResult(msg.result, msg.request.wizardDisabled)
	default:
		text := ""
		m, text = m.applyMCPCommandResult(msg.request.raw, msg.result)
		m.transcript = appendTranscriptRow(m.transcript, transcriptRow{kind: rowSystem, tool: "mcp", text: text})
	}
	return m
}

func (m model) applyMCPCommandResult(args string, result MCPCommandResult) (model, string) {
	if result.ExitCode != 0 || strings.TrimSpace(result.Error) != "" {
		message := strings.TrimSpace(result.Error)
		if message == "" {
			message = strings.TrimSpace(result.Output)
		}
		if message == "" {
			message = "MCP command failed"
		}
		return m, strings.Join([]string{
			"MCP action failed",
			message,
			"",
			m.mcpText(),
		}, "\n")
	}
	if len(result.Config.Servers) > 0 || len(m.mcpConfig.Servers) > 0 {
		m.mcpConfig = result.Config
		m.refreshMCPViewState()
	}
	output := strings.TrimSpace(result.Output)
	if output == "" {
		output = "zero mcp " + args
	}
	return m, strings.Join([]string{
		"MCP action complete",
		output,
		"",
		m.mcpText(),
	}, "\n")
}

func splitMCPCommandArgs(args string) ([]string, error) {
	args = strings.TrimSpace(args)
	if args == "" {
		return nil, nil
	}
	out := []string{}
	var current strings.Builder
	var quote rune
	hasToken := false
	runes := []rune(args)
	for index := 0; index < len(runes); index++ {
		r := runes[index]
		if quote != 0 {
			if r == quote {
				quote = 0
				hasToken = true
				continue
			}
			if r == '\\' && index+1 < len(runes) && runes[index+1] == quote {
				index++
				current.WriteRune(runes[index])
				hasToken = true
				continue
			}
			current.WriteRune(r)
			hasToken = true
			continue
		}
		switch {
		case r == '\'' || r == '"':
			quote = r
			hasToken = true
		case r == '\\' && index+1 < len(runes) && (runes[index+1] == '\'' || runes[index+1] == '"' || strings.TrimSpace(string(runes[index+1])) == ""):
			index++
			current.WriteRune(runes[index])
			hasToken = true
		case strings.TrimSpace(string(r)) == "":
			if hasToken {
				out = append(out, current.String())
				current.Reset()
				hasToken = false
			}
		default:
			current.WriteRune(r)
			hasToken = true
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("unterminated quote in MCP command")
	}
	if hasToken {
		out = append(out, current.String())
	}
	return out, nil
}

func (m model) permissionsText() string {
	if m.sandboxStore == nil {
		return m.permissionsTextWithStore(nil)
	}
	return m.permissionsTextWithStore(m.sandboxStore)
}

func (m model) permissionsTextWithStore(store grantLister) string {
	mode := string(m.permissionMode)
	if store == nil {
		return renderCommandCardTranscript(commandCard{
			Title:   "Permissions",
			Summary: []string{mode + " permissions", "grants unavailable"},
			Sections: []commandCardSection{
				{
					Title: "State",
					Fields: []commandField{
						{Key: "mode", Value: mode},
					},
				},
				{
					Title: "Grants",
					Lines: []string{"persistent grants: unavailable"},
				},
			},
		})
	}

	grants, err := store.List()
	if err != nil {
		return renderCommandCardTranscript(commandCard{
			Title:   "Permissions",
			Summary: []string{mode + " permissions", "grants error"},
			Sections: []commandCardSection{
				{
					Title: "State",
					Fields: []commandField{
						{Key: "mode", Value: mode},
					},
				},
				{
					Title: "Grants",
					Lines: []string{"error: " + err.Error()},
				},
			},
		})
	}
	prefixes := []sandbox.CommandPrefixGrant{}
	if prefixStore, ok := store.(commandPrefixGrantLister); ok {
		var prefixErr error
		prefixes, prefixErr = prefixStore.ListCommandPrefixes()
		if prefixErr != nil {
			return renderCommandCardTranscript(commandCard{
				Title:   "Permissions",
				Summary: []string{mode + " permissions", "grants error"},
				Sections: []commandCardSection{
					{
						Title: "State",
						Fields: []commandField{
							{Key: "mode", Value: mode},
						},
					},
					{
						Title: "Grants",
						Lines: []string{"error: " + prefixErr.Error()},
					},
				},
			})
		}
	}

	snapshots := zerocommands.SandboxGrantSnapshots(grants)
	grantRows := []commandRow{}
	if len(snapshots) == 0 && len(prefixes) == 0 {
		grantRows = append(grantRows, commandRow{Text: "none"})
	} else {
		for _, grant := range snapshots {
			line := fmt.Sprintf("%s [%s]", grant.ToolName, grant.Decision)
			if grant.ApprovedAt != "" {
				line += " approved " + grant.ApprovedAt
			}
			if grant.Reason != "" {
				line += " - " + grant.Reason
			}
			grantRows = append(grantRows, commandRow{Text: line})
		}
		for _, grant := range prefixes {
			line := fmt.Sprintf("%s `%s` [command-prefix]", grant.ToolName, strings.Join(grant.Prefix, " "))
			if grant.ApprovedAt != "" {
				line += " approved " + grant.ApprovedAt
			}
			if grant.Reason != "" {
				line += " - " + grant.Reason
			}
			grantRows = append(grantRows, commandRow{Text: line})
		}
	}

	return renderCommandCardTranscript(commandCard{
		Title:   "Permissions",
		Summary: []string{mode + " permissions", formatGrantCount(len(snapshots) + len(prefixes))},
		Sections: []commandCardSection{
			{
				Title: "State",
				Fields: []commandField{
					{Key: "mode", Value: mode},
				},
			},
			{
				Title: "Grants",
				Rows:  grantRows,
			},
		},
	})
}

// grantLister is the subset of sandbox.GrantStore used by permissionsText().
// It exists to let tests inject error-stub stores without reaching for a real
// filesystem path.
type grantLister interface {
	List() ([]sandbox.Grant, error)
}

type commandPrefixGrantLister interface {
	ListCommandPrefixes() ([]sandbox.CommandPrefixGrant, error)
}

func formatGrantCount(count int) string {
	if count == 0 {
		return "no persistent grants"
	}
	if count == 1 {
		return "1 persistent grant"
	}
	return fmt.Sprintf("%d persistent grants", count)
}

func (m model) providerText() string {
	profileLines := []string{
		"provider: " + displayValue(m.providerName, "none"),
		"model: " + displayValue(m.modelName, "none"),
	}
	if !config.HasProviderProfile(m.providerProfile) {
		profileLines = append(profileLines, "profile: not configured")
		return renderCommandOutput(commandOutput{
			Title:  "Provider",
			Status: commandStatusWarning,
			Sections: []commandSection{
				{Title: "Active", Lines: profileLines},
				{Title: "Next actions", Lines: []string{
					"zero providers catalog",
					"zero providers setup openai --set-active",
					"zero providers add openai --api-key-env OPENAI_API_KEY --set-active",
				}},
			},
		})
	}

	snapshot := zerocommands.ProviderSnapshotFromProfile(m.providerProfile, true)
	// A keyless profile backed by a stored OAuth login (e.g. ChatGPT) is fully
	// authenticated — without this, /provider status showed a WORKING provider
	// as "api key: not set" with a warning, unlike `zero providers list` which
	// already fills OAuthLogin.
	if !snapshot.APIKeySet {
		snapshot.OAuthLogin = oauthLoginAvailable(m.providerProfile)
	}
	credentialState := apiKeyState(snapshot.APIKeySet)
	if !snapshot.APIKeySet && snapshot.OAuthLogin {
		credentialState = "oauth login"
	}
	profileLines = append(profileLines,
		"active: "+boolText(snapshot.Active),
		"kind: "+displayValue(snapshot.ProviderKind, "unknown"),
		"api model: "+displayValue(snapshot.APIModel, "unknown"),
		"base url: "+displayValue(snapshot.BaseURL, "default"),
		"api key: "+credentialState,
	)
	if snapshot.Message != "" {
		profileLines = append(profileLines, "provider status: "+snapshot.Status+" - "+snapshot.Message)
	}

	status := commandStatusOK
	actionLines := providerNextActionLines(m.providerProfile, snapshot, m.providerName)
	if providerCredentialRequired(m.providerProfile, snapshot.ProviderKind) && !providerProfileHasCredential(m.providerProfile) && !snapshot.OAuthLogin {
		status = commandStatusWarning
	}
	return renderCommandOutput(commandOutput{
		Title:  "Provider",
		Status: status,
		Sections: []commandSection{
			{Title: "Active", Lines: profileLines},
			{Title: "Next actions", Lines: actionLines},
		},
	})
}

func providerNextActionLines(profile config.ProviderProfile, snapshot zerocommands.ProviderSnapshot, activeName string) []string {
	providerName := firstProviderDisplayValue(snapshot.Name, activeName, profile.Name, providerSetupCatalogID(profile, snapshot.ProviderKind), "openai")
	setupID := providerSetupCatalogID(profile, snapshot.ProviderKind)
	lines := []string{}
	if providerCredentialRequired(profile, snapshot.ProviderKind) && !providerProfileHasCredential(profile) {
		if envName := providerCredentialEnvName(profile, snapshot.ProviderKind); envName != "" {
			lines = append(lines,
				"set "+envName+" in your environment",
				"zero providers add "+setupID+" --api-key-env "+envName+" --set-active",
			)
		} else {
			lines = append(lines, "set provider credentials in your environment")
		}
	}
	return append(lines,
		"zero providers check "+providerName+" --connectivity",
		"zero providers catalog",
		"zero providers setup "+setupID+" --set-active",
	)
}

func providerProfileHasCredential(profile config.ProviderProfile) bool {
	return profile.HasConfiguredCredential()
}

func providerCredentialRequired(profile config.ProviderProfile, providerKind string) bool {
	if descriptor, ok := providerCatalogDescriptor(profile); ok {
		// A custom endpoint's RequiresAuth is just the wizard's template
		// default, not a fact about this profile's actual endpoint (issue
		// #555 follow-up) — only an explicit, non-default APIKeyEnv means
		// this saved profile itself expects a credential.
		if descriptor.Custom {
			envVar := strings.TrimSpace(profile.APIKeyEnv)
			return envVar != "" && envVar != firstProviderDisplayValue(descriptor.AuthEnvVars...)
		}
		return descriptor.RequiresAuth
	}
	switch config.ProviderKind(strings.TrimSpace(providerKind)) {
	case config.ProviderKindOpenAI, config.ProviderKindOpenAICompatible, config.ProviderKindAnthropic, config.ProviderKindAnthropicCompat, config.ProviderKindGoogle:
		return true
	default:
		return false
	}
}

func providerCredentialEnvName(profile config.ProviderProfile, providerKind string) string {
	if envName := strings.TrimSpace(profile.APIKeyEnv); envName != "" {
		return envName
	}
	if descriptor, ok := providerCatalogDescriptor(profile); ok && len(descriptor.AuthEnvVars) > 0 {
		return descriptor.AuthEnvVars[0]
	}
	switch config.ProviderKind(strings.TrimSpace(providerKind)) {
	case config.ProviderKindOpenAI, config.ProviderKindOpenAICompatible:
		return "OPENAI_API_KEY"
	case config.ProviderKindAnthropic, config.ProviderKindAnthropicCompat:
		return "ANTHROPIC_API_KEY"
	case config.ProviderKindGoogle:
		return "GEMINI_API_KEY"
	default:
		return ""
	}
}

func providerSetupCatalogID(profile config.ProviderProfile, providerKind string) string {
	if catalogID := strings.TrimSpace(profile.CatalogID); catalogID != "" {
		return catalogID
	}
	switch config.ProviderKind(strings.TrimSpace(providerKind)) {
	case config.ProviderKindOpenAI:
		return "openai"
	case config.ProviderKindAnthropic:
		return "anthropic"
	case config.ProviderKindGoogle:
		return "google"
	case config.ProviderKindOpenAICompatible:
		return "custom-openai-compatible"
	case config.ProviderKindAnthropicCompat:
		return "custom-anthropic-compatible"
	default:
		return firstProviderDisplayValue(profile.Name, "openai")
	}
}

func providerCatalogDescriptor(profile config.ProviderProfile) (providercatalog.Descriptor, bool) {
	catalogID := strings.TrimSpace(profile.CatalogID)
	if catalogID == "" {
		return providercatalog.Descriptor{}, false
	}
	descriptor, err := providercatalog.Require(catalogID)
	if err != nil {
		return providercatalog.Descriptor{}, false
	}
	return descriptor, true
}

func firstProviderDisplayValue(values ...string) string {
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			return value
		}
	}
	return ""
}

func (m model) modelText(args string) string {
	return renderCommandOutput(commandOutput{
		Title:  "Model",
		Status: commandStatusOK,
		Sections: []commandSection{{
			Title: "Active",
			Lines: []string{
				"model: " + displayValue(m.modelName, "none"),
				"provider: " + displayValue(m.providerName, "none"),
				"effort: " + m.effortDisplay(),
			},
		}},
		Hints: []string{"use /model list to inspect models or /model <id> to switch this TUI session"},
	})
}

// avgTurnLatencyText reports the session's rolling average turn wall-time for
// /context — the "is it slow?" signal a user otherwise can only feel. "n/a" until
// a turn has completed.
func (m model) avgTurnLatencyText() string {
	if m.turnLatencyCount == 0 {
		return "n/a"
	}
	avgSeconds := m.turnLatencySum.Seconds() / float64(m.turnLatencyCount)
	if m.turnTTFTCount > 0 {
		ttftSeconds := m.turnTTFTSum.Seconds() / float64(m.turnTTFTCount)
		return fmt.Sprintf("%.1fs avg (%.1fs to first token, %d turns)", avgSeconds, ttftSeconds, m.turnLatencyCount)
	}
	return fmt.Sprintf("%.1fs avg (%d turns)", avgSeconds, m.turnLatencyCount)
}

func (m model) contextText() string {
	toolCount := len(m.registeredTools())
	return renderCommandCardTranscript(commandCard{
		Title: "Context",
		Summary: []string{
			"go runtime",
			string(m.permissionMode) + " permissions",
			pluralizeCount(toolCount, "tool", "tools"),
		},
		Sections: []commandCardSection{
			{
				Title: "Runtime",
				Fields: []commandField{
					{Key: "cwd", Value: displayValue(m.cwd, "unknown")},
					{Key: "provider", Value: displayValue(m.providerName, "none")},
					{Key: "model", Value: displayValue(m.modelName, "none")},
					{Key: "effort", Value: m.effortDisplay()},
					{Key: "style", Value: displayValue(m.responseStyle, defaultResponseStyle)},
					{Key: "usage", Value: m.usageSummaryText()},
					{Key: "cache", Value: m.cacheEfficiencyText()},
					{Key: "latency", Value: m.avgTurnLatencyText()},
					{Key: "max turns", Value: fmt.Sprint(m.agentOptions.MaxTurns)},
				},
			},
			{
				Title: "Session",
				Fields: []commandField{
					{Key: "active", Value: displayValue(m.activeSession.SessionID, "none")},
					{Key: "root", Value: displayValue(m.sessionRootDir(), "unknown")},
					{Key: "compaction", Value: contextCompactionStatus(m.compactionStatus())},
				},
			},
			{
				Title: "Tools",
				Fields: []commandField{
					{Key: "registered", Value: fmt.Sprint(toolCount)},
				},
			},
		},
		Actions: []string{"/permissions manage access", "/tools inspect catalog"},
	})
}

func (m model) registeredTools() []tools.Tool {
	if m.registry == nil {
		return nil
	}
	return m.registry.All()
}

func (m model) sessionRootDir() string {
	if m.sessionStore == nil {
		return ""
	}
	return m.sessionStore.RootDir
}

func pluralizeCount(count int, singular string, plural string) string {
	label := plural
	if count == 1 {
		label = singular
	}
	return fmt.Sprintf("%d %s", count, label)
}

func contextCompactionStatus(status string) string {
	if status == "not compacted" {
		return "idle"
	}
	return status
}

// onOff renders a boolean preference as "on"/"off" for config display.
func onOff(v bool) string {
	if v {
		return "on"
	}
	return "off"
}

func (m model) configText() string {
	return renderCommandOutput(commandOutput{
		Title:  "Config",
		Status: commandStatusOK,
		Sections: []commandSection{
			{
				Title: "Runtime",
				Lines: []string{
					"runtime: go",
					fmt.Sprintf("max turns: %d", m.agentOptions.MaxTurns),
					"permission mode: " + string(m.permissionMode),
					"recaps: " + onOff(m.recapsEnabled),
				},
			},
			{
				Title: "Provider",
				Lines: []string{
					"provider: " + displayValue(m.providerName, "none"),
					"model: " + displayValue(m.modelName, "none"),
					"api key: " + apiKeyState(strings.TrimSpace(m.providerProfile.APIKey) != "" || m.providerProfile.APIKeyStored),
				},
			},
		},
	})
}

func (m model) debugText() string {
	state := "idle"
	if m.pending {
		state = "running"
	}
	return renderCommandOutput(commandOutput{
		Title:  "Debug",
		Status: commandStatusInfo,
		Sections: []commandSection{{
			Title: "Runtime",
			Lines: []string{
				"run state: " + state,
				"active run: " + fmt.Sprint(m.activeRunID),
				"pending permission: " + boolText(m.pendingPermission != nil),
			},
		}},
	})
}

// skillsText is the /skills fallback when NO skills are installed — an install
// hint. With skills present /skills opens the searchable skill picker instead
// (see newSkillPicker), matching how /model works.
func (m model) skillsText() string {
	return renderCommandOutput(commandOutput{
		Title:  "Skills",
		Status: commandStatusInfo,
		Sections: []commandSection{{
			Lines: []string{"No skills installed."},
		}},
		Hints: []string{
			"install one: create <skills-dir>/<name>/SKILL.md (see `zero skills`)",
		},
	})
}
