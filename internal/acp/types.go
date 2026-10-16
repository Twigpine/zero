package acp

import "encoding/json"

// ProtocolVersion is the ACP protocol version ZERO speaks. Wire compatibility is
// negotiated during initialize; v1 is the current stable version.
const ProtocolVersion = 1

// Method names exactly as they appear on the wire (public ACP spec).
const (
	MethodInitialize             = "initialize"
	MethodAuthenticate           = "authenticate"
	MethodSessionNew             = "session/new"
	MethodSessionLoad            = "session/load"
	MethodSessionList            = "session/list"
	MethodSessionResume          = "session/resume"
	MethodSessionPrompt          = "session/prompt"
	MethodSessionCancel          = "session/cancel" // notification
	MethodSessionUpdate          = "session/update" // notification (agent -> client)
	MethodSessionSetMode         = "session/set_mode"
	MethodSessionSetConfigOption = "session/set_config_option"
	MethodSessionRequestPerm     = "session/request_permission" // agent -> client
	MethodFSReadTextFile         = "fs/read_text_file"          // agent -> client
	MethodFSWriteTextFile        = "fs/write_text_file"         // agent -> client

	// Vendor-prefixed ZERO extensions (clients that don't support them ignore the
	// method and degrade cleanly, per the spec's _-prefixed convention).
	MethodZeroSetModel = "_zero/set_model"
)

// SessionUpdate discriminator values (the "sessionUpdate" field).
const (
	UpdateAgentMessageChunk = "agent_message_chunk"
	UpdateAgentThoughtChunk = "agent_thought_chunk"
	UpdateUserMessageChunk  = "user_message_chunk"
	UpdateToolCall          = "tool_call"
	UpdateToolCallUpdate    = "tool_call_update"
	UpdatePlan              = "plan"
	UpdateAvailableCommands = "available_commands_update"
	UpdateCurrentMode       = "current_mode_update"
)

// ---- initialize ----

type Implementation struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

type FileSystemCapabilities struct {
	ReadTextFile  bool `json:"readTextFile"`
	WriteTextFile bool `json:"writeTextFile"`
}

type ClientCapabilities struct {
	FS       FileSystemCapabilities `json:"fs"`
	Terminal bool                   `json:"terminal"`
}

type PromptCapabilities struct {
	Image           bool `json:"image"`
	Audio           bool `json:"audio"`
	EmbeddedContext bool `json:"embeddedContext"`
}

type AgentCapabilities struct {
	LoadSession         bool                 `json:"loadSession"`
	PromptCapabilities  PromptCapabilities   `json:"promptCapabilities"`
	SessionCapabilities *SessionCapabilities `json:"sessionCapabilities,omitempty"`
}

// Empty capability objects are presence flags in ACP v1. Pointers preserve
// the wire distinction between an advertised `{}` and an omitted capability.
type SessionCapabilities struct {
	List   *struct{} `json:"list,omitempty"`
	Resume *struct{} `json:"resume,omitempty"`
}

type AuthMethod struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

type InitializeParams struct {
	ProtocolVersion    int                `json:"protocolVersion"`
	ClientCapabilities ClientCapabilities `json:"clientCapabilities"`
	ClientInfo         *Implementation    `json:"clientInfo,omitempty"`
}

type InitializeResult struct {
	ProtocolVersion   int               `json:"protocolVersion"`
	AgentCapabilities AgentCapabilities `json:"agentCapabilities"`
	AgentInfo         *Implementation   `json:"agentInfo,omitempty"`
	AuthMethods       []AuthMethod      `json:"authMethods"`
}

// ---- content blocks ----

// ContentBlock is the polymorphic content type. ZERO emits "text" (and "image"
// on tool content); it parses "text", "image", and "resource"/"resource_link"
// from inbound prompts. A single struct with omitempty fields covers both
// directions since the field names do not collide across the variants ZERO uses.
type ContentBlock struct {
	Type     string          `json:"type"`
	Text     string          `json:"text,omitempty"`
	Data     string          `json:"data,omitempty"`     // image/audio: base64
	MimeType string          `json:"mimeType,omitempty"` // image/audio
	URI      string          `json:"uri,omitempty"`      // resource_link
	Name     string          `json:"name,omitempty"`     // resource_link
	Resource json.RawMessage `json:"resource,omitempty"` // embedded resource
}

func TextBlock(text string) ContentBlock { return ContentBlock{Type: "text", Text: text} }

// ---- sessions ----

// McpServer mirrors the editor-provided MCP server entry. ZERO owns its own MCP
// configuration (BYOK), so these are accepted for spec compliance; ZERO's
// configured servers remain authoritative.
type McpServer struct {
	Name    string          `json:"name"`
	Command string          `json:"command,omitempty"`
	Args    []string        `json:"args,omitempty"`
	Env     json.RawMessage `json:"env,omitempty"`
	URL     string          `json:"url,omitempty"`
}

type NewSessionParams struct {
	Cwd        string      `json:"cwd"`
	McpServers []McpServer `json:"mcpServers"`
	// AdditionalDirectories is declared by the protocol and consumed nowhere in
	// this repo yet. When it is wired up it must go through requestedWorkspace
	// like Cwd does: these are client-supplied paths with no absoluteness rule of
	// their own, which is the same shape as the resume-cwd hole that made
	// {"sessionId":"known"} activate a session against this process's directory.
	AdditionalDirectories []string `json:"additionalDirectories,omitempty"`
}

type NewSessionResult struct {
	SessionID     string                `json:"sessionId"`
	ConfigOptions []SessionConfigOption `json:"configOptions,omitempty"`
	Modes         *SessionModeState     `json:"modes,omitempty"`
}

type LoadSessionParams struct {
	SessionID             string      `json:"sessionId"`
	Cwd                   string      `json:"cwd"`
	McpServers            []McpServer `json:"mcpServers"`
	AdditionalDirectories []string    `json:"additionalDirectories,omitempty"`
}

type LoadSessionResult struct {
	ConfigOptions []SessionConfigOption `json:"configOptions,omitempty"`
	Modes         *SessionModeState     `json:"modes,omitempty"`
}

// ListSessionsParams and the following types implement ACP v1 session/list.
// Transcript contents stay behind session/load; this method returns metadata
// only and leaves the optional pagination cursor opaque.
type ListSessionsParams struct {
	Cwd    string `json:"cwd,omitempty"`
	Cursor string `json:"cursor,omitempty"`
}

type SessionInfoMeta struct {
	ModelID   string `json:"modelId,omitempty"`
	CreatedAt string `json:"createdAt,omitempty"`
}

type SessionInfo struct {
	SessionID string           `json:"sessionId"`
	Cwd       string           `json:"cwd"`
	Title     string           `json:"title,omitempty"`
	UpdatedAt string           `json:"updatedAt,omitempty"`
	Meta      *SessionInfoMeta `json:"_meta,omitempty"`
}

type ListSessionsResult struct {
	Sessions   []SessionInfo `json:"sessions"`
	NextCursor string        `json:"nextCursor,omitempty"`
}

// session/resume takes session/load's wire shape, so its cwd is a plain string
// too and an omitted one decodes exactly like an empty one. Sharing the type is
// safe only because activatePersistedSession requires an absolute cwd from the
// request itself — nothing downstream may supply a default for either method.
type ResumeSessionParams = LoadSessionParams
type ResumeSessionResult = LoadSessionResult

// ---- prompt turn ----

type PromptParams struct {
	SessionID string         `json:"sessionId"`
	Prompt    []ContentBlock `json:"prompt"`
}

// StopReason values (why a prompt turn ended).
const (
	StopEndTurn   = "end_turn"
	StopMaxTokens = "max_tokens"
	StopRefusal   = "refusal"
	StopCancelled = "cancelled"
)

type PromptResult struct {
	StopReason string `json:"stopReason"`
}

type CancelParams struct {
	SessionID string `json:"sessionId"`
}

// ---- session/update notification ----

type SessionNotification struct {
	SessionID string `json:"sessionId"`
	Update    any    `json:"update"`
}

// AgentMessageChunk / AgentThoughtChunk / UserMessageChunk all carry a single
// ContentBlock under "content"; the variant is set via SessionUpdate.
type ContentChunk struct {
	SessionUpdate string       `json:"sessionUpdate"`
	MessageID     string       `json:"messageId,omitempty"`
	Content       ContentBlock `json:"content"`
}

// ToolKind classifies a tool call for client rendering.
const (
	ToolKindRead    = "read"
	ToolKindEdit    = "edit"
	ToolKindDelete  = "delete"
	ToolKindMove    = "move"
	ToolKindSearch  = "search"
	ToolKindExecute = "execute"
	ToolKindThink   = "think"
	ToolKindFetch   = "fetch"
	ToolKindOther   = "other"
)

// ToolCallStatus values.
const (
	ToolStatusPending    = "pending"
	ToolStatusInProgress = "in_progress"
	ToolStatusCompleted  = "completed"
	ToolStatusFailed     = "failed"
)

// ToolCallUpdate is used for both the initial "tool_call" and subsequent
// "tool_call_update" notifications (distinguished by SessionUpdate). It also
// appears inside session/request_permission.
type ToolCallUpdate struct {
	SessionUpdate string             `json:"sessionUpdate,omitempty"`
	ToolCallID    string             `json:"toolCallId"`
	Title         string             `json:"title,omitempty"`
	Kind          string             `json:"kind,omitempty"`
	Status        string             `json:"status,omitempty"`
	RawInput      json.RawMessage    `json:"rawInput,omitempty"`
	Content       []ToolCallContent  `json:"content,omitempty"`
	Locations     []ToolCallLocation `json:"locations,omitempty"`
	// Meta is ACP's extension channel. ZERO-owned values must remain beneath a
	// namespaced key so protocol-shaped clients can preserve them while decoding
	// and re-encoding a tool call.
	Meta map[string]json.RawMessage `json:"_meta,omitempty"`
}

// BrowserToolDetails identifies the browser helper operation behind a tool
// call. Version is the schema version for this optional ZERO extension;
// Command is one of install, launch, connect, open, snapshot, click, type,
// press, or action. Future fields must remain display-safe and must not
// include browser profile data, cookies, typed text, URL paths/queries, or
// DevTools endpoints.
type BrowserToolDetails struct {
	Version int    `json:"version"`
	Command string `json:"command"`
}

// ToolCallContent is a tool call's rendered output. ZERO emits "content" (a
// text/image block) and "diff" (a file change); "terminal" is part of the spec
// but unused because ZERO executes locally.
type ToolCallContent struct {
	Type string `json:"type"`
	// type == "content"
	Content *ContentBlock `json:"content,omitempty"`
	// type == "diff"
	Path    string  `json:"path,omitempty"`
	OldText *string `json:"oldText,omitempty"`
	NewText *string `json:"newText,omitempty"`
}

// MarshalJSON preserves ACP's discriminated content union. A diff always has
// path, while nullable oldText/newText distinguish creation, deletion, and an
// existing file replaced with empty content. Other content variants omit all
// diff fields rather than serializing irrelevant nulls.
func (content ToolCallContent) MarshalJSON() ([]byte, error) {
	if content.Type == "diff" {
		return json.Marshal(struct {
			Type    string  `json:"type"`
			Path    string  `json:"path"`
			OldText *string `json:"oldText"`
			NewText *string `json:"newText"`
		}{
			Type: content.Type, Path: content.Path, OldText: content.OldText, NewText: content.NewText,
		})
	}
	return json.Marshal(struct {
		Type    string        `json:"type"`
		Content *ContentBlock `json:"content,omitempty"`
	}{Type: content.Type, Content: content.Content})
}

func ToolContent(block ContentBlock) ToolCallContent {
	return ToolCallContent{Type: "content", Content: &block}
}

type ToolCallLocation struct {
	Path string `json:"path"`
	Line *int   `json:"line,omitempty"`
}

// ---- plan ----

const (
	PlanStatusPending    = "pending"
	PlanStatusInProgress = "in_progress"
	PlanStatusCompleted  = "completed"

	PlanPriorityHigh   = "high"
	PlanPriorityMedium = "medium"
	PlanPriorityLow    = "low"
)

type PlanEntry struct {
	Content  string `json:"content"`
	Priority string `json:"priority"`
	Status   string `json:"status"`
}

type PlanUpdate struct {
	SessionUpdate string      `json:"sessionUpdate"`
	Entries       []PlanEntry `json:"entries"`
}

type AvailableCommand struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

type AvailableCommandsUpdate struct {
	SessionUpdate     string             `json:"sessionUpdate"`
	AvailableCommands []AvailableCommand `json:"availableCommands"`
}

type CurrentModeUpdate struct {
	SessionUpdate string `json:"sessionUpdate"`
	CurrentModeID string `json:"currentModeId"`
}

// ---- permissions ----

const (
	PermAllowOnce    = "allow_once"
	PermAllowAlways  = "allow_always"
	PermRejectOnce   = "reject_once"
	PermRejectAlways = "reject_always"
)

type PermissionOption struct {
	OptionID string `json:"optionId"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
}

type RequestPermissionParams struct {
	SessionID string             `json:"sessionId"`
	ToolCall  ToolCallUpdate     `json:"toolCall"`
	Options   []PermissionOption `json:"options"`
}

// RequestPermissionOutcome is a tagged union: {"outcome":"cancelled"} or
// {"outcome":"selected","optionId":"..."}.
type RequestPermissionOutcome struct {
	Outcome  string `json:"outcome"`
	OptionID string `json:"optionId,omitempty"`
}

const (
	OutcomeSelected  = "selected"
	OutcomeCancelled = "cancelled"
)

type RequestPermissionResult struct {
	Outcome RequestPermissionOutcome `json:"outcome"`
}

// ---- session modes ----

type SessionMode struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

type SessionModeState struct {
	CurrentModeID  string        `json:"currentModeId"`
	AvailableModes []SessionMode `json:"availableModes"`
}

type SetSessionModeParams struct {
	SessionID string `json:"sessionId"`
	ModeID    string `json:"modeId"`
}

type SetSessionModeResult struct{}

// ---- session config options ----

type SessionConfigOptionValue struct {
	Value       string `json:"value"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

type SessionConfigOption struct {
	ID           string                     `json:"id"`
	Name         string                     `json:"name"`
	Description  string                     `json:"description,omitempty"`
	Category     string                     `json:"category,omitempty"`
	Type         string                     `json:"type"`
	CurrentValue string                     `json:"currentValue"`
	Options      []SessionConfigOptionValue `json:"options"`
}

type SetSessionConfigOptionParams struct {
	SessionID string `json:"sessionId"`
	ConfigID  string `json:"configId"`
	Value     string `json:"value"`
}

type SetSessionConfigOptionResult struct {
	ConfigOptions []SessionConfigOption `json:"configOptions"`
}

// ---- vendor: _zero/set_model ----

type ZeroSetModelParams struct {
	SessionID string `json:"sessionId"`
	Model     string `json:"model"`
}

type ZeroSetModelResult struct {
	Model string `json:"model"`
}

const (
	configIDModel = "model"
	configIDMode  = "mode"

	configOptionTypeSelect = "select"
	configCategoryModel    = "model"
	configCategoryMode     = "mode"
)
