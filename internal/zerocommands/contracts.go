package zerocommands

import (
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/Twigpine/zero/internal/config"
	"github.com/Twigpine/zero/internal/modelregistry"
	"github.com/Twigpine/zero/internal/providercatalog"
	"github.com/Twigpine/zero/internal/providers"
	"github.com/Twigpine/zero/internal/redaction"
	"github.com/Twigpine/zero/internal/sessions"
)

const RuntimeGo = "go"

type ErrorKind string

const (
	ErrorKindUsage    ErrorKind = "usage"
	ErrorKindProvider ErrorKind = "provider"
	ErrorKindRuntime  ErrorKind = "runtime"
)

type CommandError struct {
	Kind        ErrorKind `json:"kind"`
	Message     string    `json:"message"`
	Recoverable bool      `json:"recoverable,omitempty"`
}

func (err CommandError) Error() string {
	return err.Message
}

type ConfigSnapshot struct {
	Runtime        string             `json:"runtime"`
	ActiveProvider string             `json:"activeProvider,omitempty"`
	MaxTurns       int                `json:"maxTurns"`
	Providers      []ProviderSnapshot `json:"providers"`
}

type ProviderSnapshot struct {
	Name         string `json:"name"`
	ProviderKind string `json:"providerKind,omitempty"`
	BaseURL      string `json:"baseUrl,omitempty"`
	Model        string `json:"model,omitempty"`
	APIModel     string `json:"apiModel,omitempty"`
	Active       bool   `json:"active"`
	APIKeySet    bool   `json:"apiKeySet"`
	// OAuthLogin marks a keyless profile whose credential is a stored OAuth login
	// (e.g. ChatGPT). Filled by callers with access to the oauth store — the
	// snapshot builder itself does no secret I/O.
	OAuthLogin bool   `json:"oauthLogin,omitempty"`
	Status     string `json:"status,omitempty"`
	Message    string `json:"message,omitempty"`
}

type ModelSnapshotOptions struct {
	Provider          modelregistry.ProviderKind
	IncludeDeprecated bool
}

type ModelSnapshot struct {
	ID               string   `json:"id"`
	DisplayName      string   `json:"displayName"`
	Provider         string   `json:"provider"`
	APIModel         string   `json:"apiModel"`
	Status           string   `json:"status"`
	ContextWindow    int      `json:"contextWindow"`
	MaxOutputTokens  int      `json:"maxOutputTokens"`
	Capabilities     []string `json:"capabilities"`
	ReasoningEfforts []string `json:"reasoningEfforts,omitempty"`
	Description      string   `json:"description,omitempty"`
}

type ProviderCatalogSnapshotOptions struct {
	Transport string
}

type ProviderCatalogSnapshot struct {
	ID                       string   `json:"id"`
	Name                     string   `json:"name"`
	Transport                string   `json:"transport"`
	DefaultBaseURL           string   `json:"defaultBaseUrl,omitempty"`
	DefaultModel             string   `json:"defaultModel,omitempty"`
	AuthEnvVars              []string `json:"authEnvVars,omitempty"`
	RequiresAuth             bool     `json:"requiresAuth"`
	Local                    bool     `json:"local"`
	RuntimeSupported         bool     `json:"runtimeSupported"`
	RuntimeUnsupportedReason string   `json:"runtimeUnsupportedReason,omitempty"`
	Recommended              bool     `json:"recommended,omitempty"`
}

type SessionSnapshot struct {
	SessionID           string `json:"sessionId"`
	Kind                string `json:"kind,omitempty"`
	Title               string `json:"title,omitempty"`
	Cwd                 string `json:"cwd,omitempty"`
	ModelID             string `json:"modelId,omitempty"`
	Provider            string `json:"provider,omitempty"`
	Tag                 string `json:"tag,omitempty"`
	Depth               int    `json:"depth,omitempty"`
	ParentSessionID     string `json:"parentSessionId,omitempty"`
	RootSessionID       string `json:"rootSessionId,omitempty"`
	AgentName           string `json:"agentName,omitempty"`
	TaskID              string `json:"taskId,omitempty"`
	ForkedFromEventID   string `json:"forkedFromEventId,omitempty"`
	ForkedFromSequence  int    `json:"forkedFromSequence,omitempty"`
	SpawnedFromEventID  string `json:"spawnedFromEventId,omitempty"`
	SpawnedFromSequence int    `json:"spawnedFromSequence,omitempty"`
	SpecID              string `json:"specId,omitempty"`
	SpecFilePath        string `json:"specFilePath,omitempty"`
	SpecStatus          string `json:"specStatus,omitempty"`
	SpecDraftModelID    string `json:"specDraftModelId,omitempty"`
	SpecDraftReasoning  string `json:"specDraftReasoning,omitempty"`
	SpecUserComment     string `json:"specUserComment,omitempty"`
	SpecRejectReason    string `json:"specRejectReason,omitempty"`
	SpecSourceSessionID string `json:"specSourceSessionId,omitempty"`
	SpecImplSessionID   string `json:"specImplSessionId,omitempty"`
	CreatedAt           string `json:"createdAt,omitempty"`
	UpdatedAt           string `json:"updatedAt,omitempty"`
	EventCount          int    `json:"eventCount"`
	LastEventType       string `json:"lastEventType,omitempty"`
}

type SessionTreeSnapshot struct {
	Session  SessionSnapshot       `json:"session"`
	Children []SessionTreeSnapshot `json:"children,omitempty"`
}

func UsageError(message string) CommandError {
	return CommandError{Kind: ErrorKindUsage, Message: strings.TrimSpace(message)}
}

func ConfigSnapshotFromResolved(resolved config.ResolvedConfig) ConfigSnapshot {
	snapshot := ConfigSnapshot{
		Runtime:        RuntimeGo,
		ActiveProvider: resolved.ActiveProvider,
		MaxTurns:       resolved.MaxTurns,
		Providers:      make([]ProviderSnapshot, 0, len(resolved.Providers)),
	}
	for _, profile := range resolved.Providers {
		snapshot.Providers = append(snapshot.Providers, ProviderSnapshotFromProfile(profile, profile.Name == resolved.ActiveProvider))
	}
	sort.SliceStable(snapshot.Providers, func(i int, j int) bool {
		if snapshot.Providers[i].Active != snapshot.Providers[j].Active {
			return snapshot.Providers[i].Active
		}
		return snapshot.Providers[i].Name < snapshot.Providers[j].Name
	})
	return snapshot
}

func ProviderSnapshotFromProfile(profile config.ProviderProfile, active bool) ProviderSnapshot {
	snapshot := ProviderSnapshot{
		Name:         profile.Name,
		ProviderKind: string(profile.ProviderKind),
		BaseURL:      redactProviderBaseURL(profile.BaseURL, profile.APIKey, profile.AuthHeaderValue),
		Model:        profile.Model,
		Active:       active,
		// HasConfiguredCredential is the shared definition of "this profile is
		// key-authed": inline key, raw auth-header value, or a key in the encrypted
		// credential store (APIKeyStored). The resolver is pure and never loads
		// stored keys into APIKey, so checking only the inline key rendered every
		// stored-key profile as "api key: not set" — a healthy provider that read
		// as broken.
		APIKeySet: profile.HasConfiguredCredential(),
		Status:    "ok",
	}
	metadata, err := providers.ResolveRuntimeMetadata(profile, providers.Options{})
	if err != nil {
		snapshot.Status = "warning"
		snapshot.Message = "provider metadata unavailable"
		return snapshot
	}
	snapshot.ProviderKind = string(metadata.ProviderKind)
	snapshot.APIModel = metadata.APIModel
	return snapshot
}

func ModelSnapshots(registry modelregistry.Registry, options ModelSnapshotOptions) ([]ModelSnapshot, error) {
	providerFilter := modelregistry.ProviderKind(strings.TrimSpace(strings.ToLower(string(options.Provider))))
	if providerFilter != "" && !modelregistry.ValidRuntimeProviderKind(providerFilter) {
		return nil, UsageError(fmt.Sprintf("unknown model provider %q", options.Provider))
	}

	models := registry.List(modelregistry.ListOptions{IncludeDeprecated: options.IncludeDeprecated})
	snapshots := make([]ModelSnapshot, 0, len(models))
	for _, model := range models {
		if !modelMatchesProvider(model, providerFilter) {
			continue
		}
		snapshots = append(snapshots, ModelSnapshotFromEntry(model))
	}
	sort.SliceStable(snapshots, func(i int, j int) bool {
		if snapshots[i].Provider == snapshots[j].Provider {
			return snapshots[i].ID < snapshots[j].ID
		}
		return snapshots[i].Provider < snapshots[j].Provider
	})
	return snapshots, nil
}

func ProviderCatalogSnapshots(options ProviderCatalogSnapshotOptions) ([]ProviderCatalogSnapshot, error) {
	transportFilter := strings.TrimSpace(strings.ToLower(options.Transport))
	if transportFilter != "" && !knownProviderCatalogTransport(transportFilter) {
		return nil, UsageError(fmt.Sprintf("unknown provider transport %q", options.Transport))
	}

	descriptors := providercatalog.All()
	snapshots := make([]ProviderCatalogSnapshot, 0, len(descriptors))
	for _, descriptor := range descriptors {
		if transportFilter != "" && strings.ToLower(string(descriptor.Transport)) != transportFilter {
			continue
		}
		snapshots = append(snapshots, ProviderCatalogSnapshotFromDescriptor(descriptor))
	}
	sort.SliceStable(snapshots, func(i int, j int) bool {
		if snapshots[i].Recommended != snapshots[j].Recommended {
			return snapshots[i].Recommended
		}
		return snapshots[i].ID < snapshots[j].ID
	})
	return snapshots, nil
}

func knownProviderCatalogTransport(transport string) bool {
	for _, descriptor := range providercatalog.All() {
		if strings.ToLower(string(descriptor.Transport)) == transport {
			return true
		}
	}
	return false
}

func ProviderCatalogSnapshotFromDescriptor(descriptor providercatalog.Descriptor) ProviderCatalogSnapshot {
	return ProviderCatalogSnapshot{
		ID:                       descriptor.ID,
		Name:                     descriptor.Name,
		Transport:                string(descriptor.Transport),
		DefaultBaseURL:           descriptor.DefaultBaseURL,
		DefaultModel:             descriptor.DefaultModel,
		AuthEnvVars:              append([]string{}, descriptor.AuthEnvVars...),
		RequiresAuth:             descriptor.RequiresAuth,
		Local:                    descriptor.Local,
		RuntimeSupported:         providercatalog.RuntimeSupported(descriptor),
		RuntimeUnsupportedReason: providercatalog.RuntimeUnsupportedReason(descriptor),
		Recommended:              descriptor.Recommended,
	}
}

func modelMatchesProvider(model modelregistry.ModelEntry, providerFilter modelregistry.ProviderKind) bool {
	if providerFilter == "" {
		return true
	}
	if providerFilter == modelregistry.ProviderOpenAICompatible {
		return model.AllowsProvider(providerFilter)
	}
	return model.Provider == providerFilter
}

func ModelSnapshotFromEntry(model modelregistry.ModelEntry) ModelSnapshot {
	capabilities := make([]string, 0, len(model.Capabilities))
	for _, capability := range model.Capabilities {
		capabilities = append(capabilities, string(capability))
	}
	efforts := make([]string, 0, len(model.ReasoningEfforts))
	for _, effort := range model.ReasoningEfforts {
		efforts = append(efforts, string(effort))
	}
	return ModelSnapshot{
		ID:               model.ID,
		DisplayName:      model.DisplayName,
		Provider:         string(model.Provider),
		APIModel:         model.APIModel,
		Status:           string(model.Status),
		ContextWindow:    model.ContextLimits.ContextWindow,
		MaxOutputTokens:  model.ContextLimits.MaxOutputTokens,
		Capabilities:     capabilities,
		ReasoningEfforts: efforts,
		Description:      model.Description,
	}
}

func SessionSnapshots(items []sessions.Metadata) []SessionSnapshot {
	snapshots := make([]SessionSnapshot, 0, len(items))
	for _, item := range items {
		snapshots = append(snapshots, SessionSnapshotFromMetadata(item))
	}
	return snapshots
}

func SessionSnapshotFromMetadata(item sessions.Metadata) SessionSnapshot {
	return SessionSnapshot{
		SessionID:           item.SessionID,
		Kind:                string(item.SessionKind),
		Title:               item.Title,
		Cwd:                 item.Cwd,
		ModelID:             item.ModelID,
		Provider:            item.Provider,
		Tag:                 item.Tag,
		Depth:               item.Depth,
		ParentSessionID:     item.ParentSessionID,
		RootSessionID:       item.RootSessionID,
		AgentName:           item.AgentName,
		TaskID:              item.TaskID,
		ForkedFromEventID:   item.ForkedFromEventID,
		ForkedFromSequence:  item.ForkedFromSequence,
		SpawnedFromEventID:  item.SpawnedFromEventID,
		SpawnedFromSequence: item.SpawnedFromSequence,
		SpecID:              item.SpecID,
		SpecFilePath:        item.SpecFilePath,
		SpecStatus:          string(item.SpecStatus),
		SpecDraftModelID:    item.SpecDraftModelID,
		SpecDraftReasoning:  item.SpecDraftReasoning,
		SpecUserComment:     item.SpecUserComment,
		SpecRejectReason:    item.SpecRejectReason,
		SpecSourceSessionID: item.SpecSourceSessionID,
		SpecImplSessionID:   item.SpecImplSessionID,
		CreatedAt:           item.CreatedAt,
		UpdatedAt:           item.UpdatedAt,
		EventCount:          item.EventCount,
		LastEventType:       string(item.LastEventType),
	}
}

func SessionTreeSnapshotFromNode(node sessions.TreeNode) SessionTreeSnapshot {
	children := make([]SessionTreeSnapshot, 0, len(node.Children))
	for _, child := range node.Children {
		children = append(children, SessionTreeSnapshotFromNode(child))
	}
	return SessionTreeSnapshot{
		Session:  SessionSnapshotFromMetadata(node.Session),
		Children: children,
	}
}

func redactProviderBaseURL(baseURL string, secrets ...string) string {
	baseURL = strings.TrimSpace(baseURL)
	if baseURL == "" {
		return ""
	}
	safeURL := stripURLCredentials(baseURL)
	// Redact any configured credential (APIKey and/or a raw auth-header value) if
	// it leaked into the URL. Drop empties so an unset credential can't match.
	extra := make([]string, 0, len(secrets))
	for _, secret := range secrets {
		if strings.TrimSpace(secret) != "" {
			extra = append(extra, secret)
		}
	}
	return redaction.RedactString(safeURL, redaction.Options{ExtraSecretValues: extra})
}

func stripURLCredentials(value string) string {
	parsed, err := url.Parse(value)
	if err != nil || parsed.User == nil {
		if err != nil {
			return ""
		}
		return value
	}
	parsed.User = nil
	return parsed.String()
}
