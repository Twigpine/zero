package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Gitlawb/zero/internal/oauth"
)

// tokenStoreSchemaVersion is the schema of the legacy mcp-oauth-tokens.json file,
// retained so migration can recognize a file it understands.
const tokenStoreSchemaVersion = 1
const mcpServerIdentityLength = 32
const maxOAuthTokenKeySegmentLength = 128
const maxMCPServerNameForIdentityToken = maxOAuthTokenKeySegmentLength - 1 - mcpServerIdentityLength

// StoredToken holds the credentials issued by an OAuth 2.0 authorization server
// for a single MCP server. The token fields are sensitive: they are tagged so
// the repo's redaction layer masks them, and they must never be written to logs
// or stream output.
type StoredToken struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	TokenType    string    `json:"token_type,omitempty"`
	Scopes       []string  `json:"scopes,omitempty"`
	ExpiresAt    time.Time `json:"expires_at,omitempty"`
	// TokenEndpoint, ClientID and ClientSecret hold what Login discovered or
	// dynamically registered, so Refresh works for the standard `auth: oauth`
	// setup whose config carries none of them. ClientSecret is sensitive.
	// ProtectedTokenEndpoint is set when the endpoint came from server-advertised
	// discovery and must pass the public-network policy again before use.
	TokenEndpoint          string `json:"token_endpoint,omitempty"`
	ProtectedTokenEndpoint bool   `json:"protected_token_endpoint,omitempty"`
	ClientID               string `json:"client_id,omitempty"`
	ClientSecret           string `json:"client_secret,omitempty"`
}

// TokenStatus is a redaction-safe summary of a stored token. It deliberately
// omits the access and refresh token material so it can be printed by the CLI.
type TokenStatus struct {
	ServerName      string    `json:"serverName"`
	ServerIdentity  string    `json:"serverIdentity,omitempty"`
	HasToken        bool      `json:"hasToken"`
	HasRefreshToken bool      `json:"hasRefreshToken"`
	TokenType       string    `json:"tokenType,omitempty"`
	Scopes          []string  `json:"scopes,omitempty"`
	ExpiresAt       time.Time `json:"expiresAt,omitempty"`
	Expired         bool      `json:"expired"`
}

// TokenStoreOptions configures the unified token store backing MCP OAuth tokens.
// FilePath overrides the store path (default: the shared oauth store path).
// LegacyPath overrides the pre-unification file migrated on construction; when
// empty it defaults to the conventional mcp-oauth-tokens.json only for the
// default (FilePath-unset) store, so an explicit FilePath never triggers an
// unexpected migration from the real user config.
type TokenStoreOptions struct {
	FilePath   string
	LegacyPath string
	Env        map[string]string
	Now        func() time.Time
}

// TokenStore persists MCP OAuth tokens in the unified oauth store
// (internal/oauth) under the "mcp:" namespace, sharing one file with provider
// logins. On construction it transparently and non-destructively migrates a
// legacy mcp-oauth-tokens.json into the unified store.
type TokenStore struct {
	store *oauth.Store
}

// tokenFile is the legacy on-disk format, retained only to read a
// pre-unification mcp-oauth-tokens.json during migration.
type tokenFile struct {
	SchemaVersion int                    `json:"schemaVersion"`
	Tokens        map[string]StoredToken `json:"tokens"`
}

// ResolveTokenStorePath determines the on-disk location of the LEGACY OAuth token
// file, honoring an explicit override, XDG_CONFIG_HOME, then the user home dir.
// It is used to locate a pre-unification file for migration.
func ResolveTokenStorePath(env map[string]string) (string, error) {
	override := strings.TrimSpace(envValue(env, "ZERO_MCP_OAUTH_TOKENS_PATH"))
	if override != "" {
		if filepath.IsAbs(override) {
			return filepath.Clean(override), nil
		}
		return filepath.Abs(override)
	}

	configHome := strings.TrimSpace(envValue(env, "XDG_CONFIG_HOME"))
	if configHome == "" {
		home := strings.TrimSpace(firstNonEmpty(envValue(env, "HOME"), envValue(env, "USERPROFILE")))
		var err error
		if home == "" {
			home, err = os.UserHomeDir()
			if err != nil {
				return "", fmt.Errorf("resolve user home: %w", err)
			}
		}
		configHome = filepath.Join(home, ".config")
	} else if !filepath.IsAbs(configHome) {
		resolved, err := filepath.Abs(configHome)
		if err != nil {
			return "", err
		}
		configHome = resolved
	}
	return filepath.Join(configHome, "zero", "mcp-oauth-tokens.json"), nil
}

// NewTokenStore builds the unified-store-backed token store and runs a one-time
// migration from a legacy file when applicable.
func NewTokenStore(options TokenStoreOptions) (*TokenStore, error) {
	now := options.Now
	if now == nil {
		now = time.Now
	}
	unified, err := oauth.NewStore(oauth.StoreOptions{
		FilePath: options.FilePath,
		Env:      options.Env,
		Now:      now,
	})
	if err != nil {
		return nil, err
	}
	store := &TokenStore{store: unified}

	legacyPath := strings.TrimSpace(options.LegacyPath)
	if legacyPath == "" && strings.TrimSpace(options.FilePath) == "" {
		// Default (production) construction: migrate from the conventional path.
		legacyPath, err = ResolveTokenStorePath(options.Env)
		if err != nil {
			return nil, err
		}
	}
	if legacyPath != "" {
		if err := store.migrateLegacy(legacyPath); err != nil {
			return nil, err
		}
	}
	return store, nil
}

// FilePath returns the resolved unified store path.
func (store *TokenStore) FilePath() string {
	return store.store.FilePath()
}

// Save persists the token for a server, replacing any existing entry.
func (store *TokenStore) Save(serverName string, token StoredToken) error {
	key, err := mcpKey(serverName)
	if err != nil {
		return err
	}
	return store.store.Save(key, storedToOAuth(token))
}

// SaveForServer persists a token under the server's target identity. Identity
// binding prevents a project config with the same server name from reusing a
// token issued for a different MCP endpoint.
func (store *TokenStore) SaveForServer(server Server, token StoredToken) error {
	key, err := mcpIdentityKey(server)
	if err != nil {
		return err
	}
	return store.store.Save(key, storedToOAuth(token))
}

// Load returns the stored token for a server. The second return value is false
// when no token has been stored for the server.
func (store *TokenStore) Load(serverName string) (StoredToken, bool, error) {
	key, err := mcpKey(serverName)
	if err != nil {
		return StoredToken{}, false, err
	}
	token, ok, err := store.store.Load(key)
	if err != nil || !ok {
		return StoredToken{}, ok, err
	}
	return tokenToStored(token), true, nil
}

// LoadForServer returns the token matching the server's target identity. Legacy
// name-only tokens are migrated only for servers untouched by project config.
func (store *TokenStore) LoadForServer(server Server) (StoredToken, bool, error) {
	key, err := mcpIdentityKey(server)
	if err != nil {
		return StoredToken{}, false, err
	}
	token, ok, err := store.store.Load(key)
	if err != nil || ok {
		return tokenToStored(token), ok, err
	}
	if server.ProjectConfigured {
		return StoredToken{}, false, nil
	}
	legacy, ok, err := store.Load(server.Name)
	if err != nil || !ok {
		return StoredToken{}, ok, err
	}
	if err := store.store.Save(key, storedToOAuth(legacy)); err != nil {
		return StoredToken{}, false, err
	}
	return legacy, true, nil
}

// Delete removes the stored token for a server. It reports whether an entry was
// present before deletion.
func (store *TokenStore) Delete(serverName string) (bool, error) {
	key, err := mcpKey(serverName)
	if err != nil {
		return false, err
	}
	return store.store.Delete(key)
}

// DeleteForServerName removes both legacy name-only and identity-bound tokens
// for a server name.
func (store *TokenStore) DeleteForServerName(serverName string) (bool, error) {
	name := strings.TrimSpace(serverName)
	if err := ValidateServerName(name); err != nil {
		return false, err
	}
	removed, err := store.Delete(name)
	if err != nil {
		return false, err
	}
	statuses, err := store.store.Status(oauth.KeyPrefixMCP)
	if err != nil {
		return removed, err
	}
	for _, status := range statuses {
		segment := strings.TrimPrefix(status.Key, oauth.KeyPrefixMCP)
		parsedName, _, ok := splitMCPIdentityKeySegment(segment)
		if !ok || parsedName != name {
			continue
		}
		deleted, err := store.store.Delete(status.Key)
		if err != nil {
			return removed, err
		}
		removed = removed || deleted
	}
	return removed, nil
}

// Status returns a redaction-safe summary of every stored MCP token, sorted by
// server name. It never includes the token material.
func (store *TokenStore) Status() ([]TokenStatus, error) {
	statuses, err := store.store.Status(oauth.KeyPrefixMCP)
	if err != nil {
		return nil, err
	}
	out := make([]TokenStatus, 0, len(statuses))
	for _, s := range statuses {
		serverName, serverIdentity, _ := splitMCPIdentityKeySegment(strings.TrimPrefix(s.Key, oauth.KeyPrefixMCP))
		out = append(out, TokenStatus{
			ServerName:      serverName,
			ServerIdentity:  serverIdentity,
			HasToken:        s.HasToken,
			HasRefreshToken: s.HasRefreshToken,
			TokenType:       s.TokenType,
			Scopes:          s.Scopes,
			ExpiresAt:       s.ExpiresAt,
			Expired:         s.Expired,
		})
	}
	return out, nil
}

// migrateLegacy imports tokens from a legacy mcp-oauth-tokens.json into the
// unified store (under "mcp:" keys), then renames the legacy file to a
// ".migrated" backup. It is non-destructive and idempotent: a newer unified
// entry is never overwritten, a missing/unreadable/foreign-schema legacy file is
// left untouched, and the rename ensures it is imported at most once.
func (store *TokenStore) migrateLegacy(legacyPath string) error {
	legacyPath = filepath.Clean(legacyPath)
	// FilePath() is an absolute path; resolve a relative LegacyPath so the
	// same-file guard can't be bypassed by a relative spelling of the same file.
	if !filepath.IsAbs(legacyPath) {
		abs, err := filepath.Abs(legacyPath)
		if err != nil {
			return err
		}
		legacyPath = filepath.Clean(abs)
	}
	if legacyPath == store.store.FilePath() {
		return nil // legacy and unified resolve to the same file; nothing to migrate
	}
	data, err := os.ReadFile(legacyPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	var legacy tokenFile
	if err := json.Unmarshal(data, &legacy); err != nil {
		return nil // unreadable legacy file: leave it in place, don't block startup
	}
	if legacy.SchemaVersion != tokenStoreSchemaVersion {
		return nil // unknown legacy schema: leave it untouched
	}
	for serverName, token := range legacy.Tokens {
		key, err := mcpKey(serverName)
		if err != nil {
			continue // a name that cannot form a valid unified key is skipped
		}
		if _, ok, loadErr := store.store.Load(key); loadErr == nil && ok {
			continue // a unified entry already exists; never overwrite
		}
		if err := store.store.Save(key, storedToOAuth(token)); err != nil {
			return err
		}
	}
	// A concurrent migrator may have already renamed the legacy file; treat a
	// "source is gone" rename as success so the server isn't wrongly dropped on
	// startup just because it lost the cleanup race (the tokens migrated fine) (M8).
	if err := os.Rename(legacyPath, legacyPath+".migrated"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// mcpKey builds and validates the unified store key for an MCP server token.
func mcpKey(serverName string) (string, error) {
	name := strings.TrimSpace(serverName)
	if err := ValidateServerName(name); err != nil {
		return "", err
	}
	if hasMCPIdentitySuffix(name) {
		return "", fmt.Errorf("MCP OAuth server name %q cannot end with .<32 hex chars> because it collides with identity-bound token storage", name)
	}
	key := oauth.KeyPrefixMCP + name
	if err := oauth.ValidateKey(key); err != nil {
		return "", err
	}
	return key, nil
}

func mcpIdentityKey(server Server) (string, error) {
	name := strings.TrimSpace(server.Name)
	if err := ValidateServerName(name); err != nil {
		return "", err
	}
	if hasMCPIdentitySuffix(name) {
		return "", fmt.Errorf("MCP OAuth server name %q cannot end with .<32 hex chars> because it collides with identity-bound token storage", name)
	}
	identity := strings.TrimSpace(server.Identity)
	if len(name) > maxMCPServerNameForIdentityToken {
		return "", fmt.Errorf("MCP OAuth server name %q is too long for identity-bound token storage", name)
	}
	if len(identity) != mcpServerIdentityLength || !isMCPServerIdentity(identity) {
		return "", fmt.Errorf("MCP OAuth server %s has invalid identity %q", name, identity)
	}
	key := oauth.KeyPrefixMCP + name + "." + identity
	if err := oauth.ValidateKey(key); err != nil {
		return "", err
	}
	return key, nil
}

func splitMCPIdentityKeySegment(segment string) (string, string, bool) {
	index := strings.LastIndex(segment, ".")
	if index <= 0 || index == len(segment)-1 {
		return segment, "", false
	}
	identity := segment[index+1:]
	if len(identity) != mcpServerIdentityLength || !isMCPServerIdentity(identity) {
		return segment, "", false
	}
	return segment[:index], identity, true
}

func hasMCPIdentitySuffix(value string) bool {
	_, _, ok := splitMCPIdentityKeySegment(value)
	return ok
}

func isMCPServerIdentity(value string) bool {
	for _, ch := range value {
		if (ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f') || (ch >= 'A' && ch <= 'F') {
			continue
		}
		return false
	}
	return value != ""
}

// storedToOAuth converts an MCP StoredToken to the shared oauth.Token (the
// inverse of tokenToStored). MCP never sets the oauth Account field.
func storedToOAuth(s StoredToken) oauth.Token {
	return oauth.Token{
		AccessToken:  s.AccessToken,
		RefreshToken: s.RefreshToken,
		TokenType:    s.TokenType,
		Scopes:       s.Scopes,
		ExpiresAt:    s.ExpiresAt,

		TokenEndpoint:          s.TokenEndpoint,
		ProtectedTokenEndpoint: s.ProtectedTokenEndpoint,
		ClientID:               s.ClientID,
		ClientSecret:           s.ClientSecret,
	}
}

// FormatTokenStatuses renders a human-readable status table without leaking any
// token material.
func FormatTokenStatuses(statuses []TokenStatus) string {
	if len(statuses) == 0 {
		return "No MCP OAuth tokens are stored."
	}
	var builder strings.Builder
	for index, status := range statuses {
		if index > 0 {
			builder.WriteByte('\n')
		}
		builder.WriteString(status.ServerName)
		if status.ServerIdentity != "" {
			builder.WriteString(" [")
			builder.WriteString(status.ServerIdentity)
			builder.WriteString("]")
		}
		builder.WriteString(": ")
		if !status.HasToken {
			builder.WriteString("no token")
			continue
		}
		builder.WriteString("token present")
		if status.HasRefreshToken {
			builder.WriteString(" (refreshable)")
		}
		if !status.ExpiresAt.IsZero() {
			if status.Expired {
				builder.WriteString(", expired at ")
			} else {
				builder.WriteString(", expires ")
			}
			builder.WriteString(status.ExpiresAt.UTC().Format(time.RFC3339))
		}
	}
	return builder.String()
}
