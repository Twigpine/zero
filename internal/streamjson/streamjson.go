package streamjson

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/Gitlawb/zero/internal/sandbox"
	"github.com/Gitlawb/zero/internal/zeroruntime"
)

const SchemaVersion = 2

type EventType string

const (
	EventRunStart           EventType = "run_start"
	EventText               EventType = "text"
	EventReasoning          EventType = "reasoning"
	EventToolCall           EventType = "tool_call"
	EventPermission         EventType = "permission"
	EventPermissionRequest  EventType = "permission_request"
	EventPermissionDecision EventType = "permission_decision"
	EventToolResult         EventType = "tool_result"
	EventCheckpoint         EventType = "checkpoint"
	EventRestore            EventType = "restore"
	EventUsage              EventType = "usage"
	EventFinal              EventType = "final"
	EventWarning            EventType = "warning"
	EventError              EventType = "error"
	EventRunEnd             EventType = "run_end"
)

type InputType string

const (
	InputPrompt  InputType = "prompt"
	InputMessage InputType = "message"
)

// Display is a compact structured summary of a tool result.
type Display struct {
	Summary string `json:"summary,omitempty"`
	Kind    string `json:"kind,omitempty"`
}

// CheckpointInfo describes a captured file checkpoint or an applied restore.
type CheckpointInfo struct {
	Sequence      int      `json:"sequence,omitempty"`
	Tool          string   `json:"tool,omitempty"`
	Files         []string `json:"files,omitempty"`
	FilesRestored int      `json:"filesRestored,omitempty"`
	FilesDeleted  int      `json:"filesDeleted,omitempty"`
	Skipped       []string `json:"skipped,omitempty"`
}

type Event struct {
	SchemaVersion     int               `json:"schemaVersion"`
	Type              EventType         `json:"type"`
	RunID             string            `json:"runId"`
	SessionID         string            `json:"sessionId,omitempty"`
	Cwd               string            `json:"cwd,omitempty"`
	Provider          string            `json:"provider,omitempty"`
	Model             string            `json:"model,omitempty"`
	APIModel          string            `json:"apiModel,omitempty"`
	Delta             string            `json:"delta,omitempty"`
	ID                string            `json:"id,omitempty"`
	Name              string            `json:"name,omitempty"`
	Args              any               `json:"args,omitempty"`
	Action            string            `json:"action,omitempty"`
	Permission        string            `json:"permission,omitempty"`
	PermissionGranted *bool             `json:"permissionGranted,omitempty"`
	PermissionMode    string            `json:"permissionMode,omitempty"`
	Autonomy          string            `json:"autonomy,omitempty"`
	SideEffect        string            `json:"sideEffect,omitempty"`
	Reason            string            `json:"reason,omitempty"`
	DecisionReason    string            `json:"decisionReason,omitempty"`
	Risk              *sandbox.Risk     `json:"risk,omitempty"`
	Block             *sandbox.Block    `json:"block,omitempty"`
	GrantMatched      bool              `json:"grantMatched,omitempty"`
	Grant             *sandbox.Grant    `json:"grant,omitempty"`
	Status            string            `json:"status,omitempty"`
	Output            string            `json:"output,omitempty"`
	Truncated         *bool             `json:"truncated,omitempty"`
	Redacted          *bool             `json:"redacted,omitempty"`
	ChangedFiles      []string          `json:"changedFiles,omitempty"`
	Display           *Display          `json:"display,omitempty"`
	Checkpoint        *CheckpointInfo   `json:"checkpoint,omitempty"`
	Meta              map[string]string `json:"meta,omitempty"`
	PromptTokens      *int              `json:"promptTokens,omitempty"`
	CompletionTokens  *int              `json:"completionTokens,omitempty"`
	CachedInputTokens *int              `json:"cachedInputTokens,omitempty"`
	CacheWriteTokens  *int              `json:"cacheWriteTokens,omitempty"`
	TotalTokens       *int              `json:"totalTokens,omitempty"`
	CostUSD           *float64          `json:"costUsd,omitempty"`
	Text              string            `json:"text,omitempty"`
	Message           string            `json:"message,omitempty"`
	Code              string            `json:"code,omitempty"`
	Recoverable       *bool             `json:"recoverable,omitempty"`
	ExitCode          *int              `json:"exitCode,omitempty"`
}

type InputEvent struct {
	SchemaVersion int          `json:"schemaVersion"`
	Type          InputType    `json:"type"`
	Role          string       `json:"role,omitempty"`
	Content       string       `json:"content"`
	Images        []InputImage `json:"images,omitempty"`
}

// InputImage carries a base64-encoded image attached to a stream-json
// message event. MediaType is a MIME type (e.g. "image/png"); Data is the
// standard base64 encoding of the raw image bytes (no data: URI prefix).
type InputImage struct {
	MediaType string `json:"mediaType"`
	Data      string `json:"data"`
}

type ProtocolError struct {
	message string
}

func (err ProtocolError) Error() string {
	return err.message
}

func CreateRunID(now time.Time) (string, error) {
	random := make([]byte, 3)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}
	return fmt.Sprintf("run_%s_%s", now.UTC().Format("20060102150405"), hex.EncodeToString(random)), nil
}

func FormatEvent(event Event) (string, error) {
	if event.SchemaVersion == 0 {
		event.SchemaVersion = SchemaVersion
	}
	if err := validateOutputEvent(event); err != nil {
		return "", err
	}

	data, err := json.Marshal(event)
	if err != nil {
		return "", err
	}

	var payload any
	if err := json.Unmarshal(data, &payload); err != nil {
		return "", err
	}
	redacted := redactValue(payload)
	output, err := json.Marshal(redacted)
	if err != nil {
		return "", err
	}
	return string(output), nil
}

func ParseInput(input string) ([]InputEvent, error) {
	lines := strings.Split(input, "\n")
	events := []InputEvent{}
	for index, line := range lines {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line == "" {
			continue
		}

		var raw map[string]json.RawMessage
		if err := json.Unmarshal([]byte(line), &raw); err != nil {
			return nil, ProtocolError{fmt.Sprintf("Invalid stream-json input at line %d: expected a JSON object.", index+1)}
		}
		if err := validateInputFields(raw); err != nil {
			return nil, ProtocolError{fmt.Sprintf("Invalid stream-json input at line %d: %s", index+1, err.Error())}
		}
		var event InputEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			return nil, ProtocolError{fmt.Sprintf("Invalid stream-json input at line %d: expected a JSON object.", index+1)}
		}
		if err := validateInputEvent(event); err != nil {
			return nil, ProtocolError{fmt.Sprintf("Invalid stream-json input at line %d: %s", index+1, err.Error())}
		}
		events = append(events, event)
	}
	return events, nil
}

func ResolvePrompt(events []InputEvent) (string, error) {
	parts := []string{}
	for _, event := range events {
		content := strings.TrimSpace(event.Content)
		if content != "" {
			parts = append(parts, content)
		}
	}
	if len(parts) == 0 {
		return "", ProtocolError{"Stream-json input must include at least one prompt or user message event."}
	}
	return strings.Join(parts, "\n\n"), nil
}

// maxStreamImageBytes caps a single decoded image at 10 MiB to bound request bodies.
const maxStreamImageBytes = 10 << 20

// ResolveImages decodes every base64 image attached to the input events into
// raw-byte ImageBlocks. Each image's media type is normalized and validated
// against the supported allow-list, and its decoded size is capped. Returns nil
// when no events carry images.
func ResolveImages(events []InputEvent) ([]zeroruntime.ImageBlock, error) {
	var images []zeroruntime.ImageBlock
	for _, event := range events {
		for _, image := range event.Images {
			// Reject an oversized payload from the ENCODED length BEFORE decoding,
			// so a multi-gigabyte base64 blob is never allocated just to be capped
			// after the fact. DecodedLen is the upper bound on the decoded size.
			if base64.StdEncoding.DecodedLen(len(image.Data)) > maxStreamImageBytes {
				return nil, ProtocolError{fmt.Sprintf("Stream-json image exceeds the %d byte limit.", maxStreamImageBytes)}
			}
			data, err := base64.StdEncoding.DecodeString(image.Data)
			if err != nil {
				return nil, ProtocolError{fmt.Sprintf("Stream-json image data is not valid base64: %s", err.Error())}
			}
			// Backstop: the exact decoded length (padding makes DecodedLen an upper
			// bound) must also stay within the cap.
			if len(data) > maxStreamImageBytes {
				return nil, ProtocolError{fmt.Sprintf("Stream-json image exceeds the %d byte limit.", maxStreamImageBytes)}
			}
			mediaType := zeroruntime.NormalizeImageMediaType(image.MediaType)
			if mediaType == "" {
				return nil, ProtocolError{fmt.Sprintf("Stream-json image has an unsupported image media type %q.", image.MediaType)}
			}
			images = append(images, zeroruntime.ImageBlock{MediaType: mediaType, Data: data})
		}
	}
	return images, nil
}

func validateOutputEvent(event Event) error {
	if event.SchemaVersion != SchemaVersion {
		return fmt.Errorf("schemaVersion must be %d", SchemaVersion)
	}
	if !isValidID(event.RunID) {
		return fmt.Errorf("runId is required")
	}
	if event.Type == "" {
		return fmt.Errorf("type is required")
	}
	return nil
}

func validateInputEvent(event InputEvent) error {
	if event.SchemaVersion != SchemaVersion {
		return fmt.Errorf("schemaVersion must be %d", SchemaVersion)
	}
	if strings.TrimSpace(event.Content) == "" && len(event.Images) == 0 {
		return fmt.Errorf("content is required")
	}
	switch event.Type {
	case InputPrompt:
		return nil
	case InputMessage:
		if event.Role != "user" {
			return fmt.Errorf("role must be user")
		}
		return nil
	default:
		return fmt.Errorf("type must be prompt or message")
	}
}

func validateInputFields(raw map[string]json.RawMessage) error {
	var inputType string
	if rawType, ok := raw["type"]; ok {
		_ = json.Unmarshal(rawType, &inputType)
	}
	allowed := map[string]bool{
		"schemaVersion": true,
		"type":          true,
		"content":       true,
	}
	if inputType == string(InputMessage) {
		allowed["role"] = true
		allowed["images"] = true
	}
	for key := range raw {
		if !allowed[key] {
			return fmt.Errorf("unknown field %s", key)
		}
	}
	return nil
}

var idPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

func isValidID(value string) bool {
	return idPattern.MatchString(value)
}

var secretPatterns = []*regexp.Regexp{
	// OpenAI-style keys: anchor on a word boundary and require a long key body so
	// "sk-" inside ordinary words (e.g. "task-list") is never redacted while real
	// keys ("sk-proj-…", "sk-…") still are.
	regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{16,}`),
	// api_key=/api-key:/apiKey "…": require a real assignment delimiter (= or :) and
	// a credential-length body so ordinary prose ("the api_key: setting", "apiKey
	// value spans…") is not mangled — a bare space after the marker no longer counts
	// as a delimiter. (AUDIT-M1)
	regexp.MustCompile(`(?i)(api[_-]?key)\s*[=:]\s*["']?[A-Za-z0-9._-]{12,}`),
	// Bearer tokens: require a credential-length token so prose such as
	// "bearer of bad news" or "bearer token" is not mistaken for a secret.
	regexp.MustCompile(`(?i)(bearer\s+)[A-Za-z0-9._-]{16,}`),
}

func redactValue(value any) any {
	switch typed := value.(type) {
	case string:
		return redactString(typed)
	case []any:
		next := make([]any, len(typed))
		for index, item := range typed {
			next[index] = redactValue(item)
		}
		return next
	case map[string]any:
		next := make(map[string]any, len(typed))
		for key, item := range typed {
			if isSensitiveKey(key) {
				next[key] = "[REDACTED]"
				continue
			}
			next[key] = redactValue(item)
		}
		return next
	default:
		return value
	}
}

var sensitiveKeyNames = map[string]bool{
	"accesstoken":   true,
	"apikey":        true,
	"authorization": true,
	"bearer":        true,
	"clientsecret":  true,
	"credential":    true,
	"credentials":   true,
	"idtoken":       true,
	"password":      true,
	"passwd":        true,
	"privatekey":    true,
	"pwd":           true,
	"refreshtoken":  true,
	"secret":        true,
	"token":         true,
}

func isSensitiveKey(key string) bool {
	normalized := normalizeSensitiveKey(key)
	if sensitiveKeyNames[normalized] {
		return true
	}
	for _, suffix := range []string{"apikey", "clientsecret", "accesstoken", "refreshtoken", "idtoken", "privatekey"} {
		if strings.HasSuffix(normalized, suffix) {
			return true
		}
	}
	return false
}

func normalizeSensitiveKey(key string) string {
	var normalized strings.Builder
	for _, char := range strings.ToLower(key) {
		if (char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') {
			normalized.WriteRune(char)
		}
	}
	return normalized.String()
}

func redactString(value string) string {
	redacted := value
	for _, pattern := range secretPatterns {
		redacted = pattern.ReplaceAllStringFunc(redacted, func(match string) string {
			for _, prefix := range []string{"apiKey=", "api_key=", "api-key=", "Bearer "} {
				if strings.HasPrefix(strings.ToLower(match), strings.ToLower(prefix)) {
					return match[:len(prefix)] + "[REDACTED]"
				}
			}
			return "[REDACTED]"
		})
	}
	return redacted
}
