package tui

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/Gitlawb/zero/internal/config"
	"github.com/Gitlawb/zero/internal/oauth"
	"github.com/Gitlawb/zero/internal/zeroruntime"
)

// managerTestModel builds a model with two saved providers, a seeded config
// file, and a fake provider factory, then opens /provider on the manager list.
func managerTestModel(t *testing.T) model {
	t.Helper()
	// Isolate every credential surface the manager touches: XDG redirects the
	// default config dir (oauth token store, default credential store location)
	// and the file backend keeps the OS keychain out of tests entirely.
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", home)
	t.Setenv("ZERO_OAUTH_TOKENS_PATH", filepath.Join(home, "oauth-tokens.json"))
	t.Setenv("ZERO_CRED_STORAGE", "encrypted-file")
	configPath := filepath.Join(t.TempDir(), "config.json")
	seed := config.FileConfig{
		ActiveProvider: "opengateway",
		Providers: []config.ProviderProfile{
			{Name: "opengateway", ProviderKind: config.ProviderKindOpenAICompatible, BaseURL: "https://gateway.example.com/v1", APIKey: "sk-gw", Model: "mimo-v2.5-pro", Description: "Main gateway"},
			{Name: "backup", ProviderKind: config.ProviderKindOpenAICompatible, BaseURL: "https://backup.example.com/v1", APIKey: "sk-backup", Model: "backup-model"},
		},
	}
	data, err := json.MarshalIndent(seed, "", "  ")
	if err != nil {
		t.Fatalf("encode seed config: %v", err)
	}
	if err := os.WriteFile(configPath, data, 0o600); err != nil {
		t.Fatalf("write seed config: %v", err)
	}
	m := newModel(context.Background(), Options{
		ProviderName:    "opengateway",
		ModelName:       "mimo-v2.5-pro",
		Provider:        &fakeProvider{},
		ProviderProfile: seed.Providers[0],
		SavedProviders:  seed.Providers,
		UserConfigPath:  configPath,
		NewProvider: func(config.ProviderProfile) (zeroruntime.Provider, error) {
			return &fakeProvider{}, nil
		},
	})
	m.width = 120
	m.height = 40
	next, _ := m.openProviderManager()
	return next
}

func managerKey(t *testing.T, m model, msg tea.KeyMsg) model {
	t.Helper()
	next, _ := m.handleProviderWizardKey(msg)
	return next
}

func TestOpenProviderManagerListsSavedProviders(t *testing.T) {
	m := managerTestModel(t)
	if m.providerWizard == nil || m.providerWizard.step != providerWizardStepManage {
		t.Fatalf("expected the manager list to open, got %+v", m.providerWizard)
	}
	if len(m.providerWizard.manageRows) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(m.providerWizard.manageRows))
	}
	rendered := m.providerWizard.render(m.width, m.spinnerGlyph())
	for _, want := range []string{"opengateway", "backup", "active", "Enter activate"} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("manager render missing %q:\n%s", want, rendered)
		}
	}
}

func TestOpenProviderManagerFallsBackToWizardWhenEmpty(t *testing.T) {
	m := newModel(context.Background(), Options{})
	next, _ := m.openProviderManager()
	if next.providerWizard == nil || next.providerWizard.step != providerWizardStepMethod {
		t.Fatalf("no saved providers should open the add wizard, got %+v", next.providerWizard)
	}
}

func TestProviderManagerEnterActivatesSelection(t *testing.T) {
	m := managerTestModel(t)
	m = managerKey(t, m, testKey(tea.KeyDown)) // move to "backup"
	next, _ := m.handleProviderWizardKey(testKey(tea.KeyEnter))
	if next.providerWizard != nil {
		t.Fatalf("manager should close after a successful switch")
	}
	if next.providerName != "backup" || next.modelName != "backup-model" {
		t.Fatalf("switch did not commit: provider=%q model=%q", next.providerName, next.modelName)
	}
	persisted := readManagerConfig(t, next.userConfigPath)
	if persisted.ActiveProvider != "backup" {
		t.Fatalf("activeProvider not persisted, got %q", persisted.ActiveProvider)
	}
}

func TestProviderManagerActivateBlockedWhilePending(t *testing.T) {
	m := managerTestModel(t)
	m.pending = true
	next, _ := m.handleProviderWizardKey(testKey(tea.KeyEnter))
	if next.providerWizard == nil || next.providerWizard.manageStatus == "" {
		t.Fatalf("a pending run must keep the manager open with a status, got %+v", next.providerWizard)
	}
	if next.providerName != "opengateway" {
		t.Fatalf("provider must not switch while pending, got %q", next.providerName)
	}
}

func TestProviderManagerDeleteConfirmsAndRemoves(t *testing.T) {
	m := managerTestModel(t)
	m = managerKey(t, m, testKey(tea.KeyDown)) // select "backup"
	m = managerKey(t, m, testKeyText("d"))
	if !m.providerWizard.manageDeleting {
		t.Fatalf("d must arm the inline delete confirm")
	}
	// Esc cancels the confirm without closing the manager.
	m = managerKey(t, m, testKey(tea.KeyEsc))
	if m.providerWizard == nil || m.providerWizard.manageDeleting {
		t.Fatalf("Esc must cancel the confirm, keeping the manager open")
	}
	if got := readManagerConfig(t, m.userConfigPath); len(got.Providers) != 2 {
		t.Fatalf("cancelled delete must not touch config, got %d providers", len(got.Providers))
	}

	m = managerKey(t, m, testKeyText("d"))
	next, _ := m.handleProviderWizardKey(testKeyText("y"))
	if next.providerWizard == nil {
		t.Fatalf("manager should stay open while providers remain")
	}
	if len(next.savedProviders) != 1 || next.savedProviders[0].Name != "opengateway" {
		t.Fatalf("savedProviders not updated: %+v", next.savedProviders)
	}
	persisted := readManagerConfig(t, next.userConfigPath)
	if len(persisted.Providers) != 1 || persisted.Providers[0].Name != "opengateway" {
		t.Fatalf("config not updated: %+v", persisted.Providers)
	}
	if persisted.ActiveProvider != "opengateway" {
		t.Fatalf("active must be untouched when a non-active profile is deleted, got %q", persisted.ActiveProvider)
	}
	if !strings.Contains(next.providerWizard.manageStatus, "Deleted backup") {
		t.Fatalf("expected delete status, got %q", next.providerWizard.manageStatus)
	}
}

func TestProviderManagerEditModelPersists(t *testing.T) {
	m := managerTestModel(t)
	m = managerKey(t, m, testKeyText("e"))
	if m.providerWizard.step != providerWizardStepEditMenu {
		t.Fatalf("e must open the field editor, got step %v", m.providerWizard.step)
	}
	// Field order: Name, Endpoint, Model, API key, Description, Save.
	m = managerKey(t, m, testKey(tea.KeyDown))
	m = managerKey(t, m, testKey(tea.KeyDown))
	m = managerKey(t, m, testKey(tea.KeyEnter)) // edit Model
	if m.providerWizard.step != providerWizardStepEditValue || m.providerWizard.editField != providerEditFieldModel {
		t.Fatalf("expected model value editor, got step %v field %v", m.providerWizard.step, m.providerWizard.editField)
	}
	m.providerWizard.editBuffer = "mimo-v3"
	m = managerKey(t, m, testKey(tea.KeyEnter)) // commit field
	if m.providerWizard.step != providerWizardStepEditMenu {
		t.Fatalf("commit should return to the field menu")
	}
	// Move to Save (cursor still on Model=index 2; Save is index 5).
	m = managerKey(t, m, testKey(tea.KeyDown))
	m = managerKey(t, m, testKey(tea.KeyDown))
	m = managerKey(t, m, testKey(tea.KeyDown))
	next, _ := m.handleProviderWizardKey(testKey(tea.KeyEnter))
	if next.providerWizard == nil || next.providerWizard.step != providerWizardStepManage {
		t.Fatalf("save should land back on the manager list")
	}
	persisted := readManagerConfig(t, next.userConfigPath)
	if persisted.Providers[0].Model != "mimo-v3" {
		t.Fatalf("edited model not persisted: %+v", persisted.Providers[0])
	}
	if persisted.Providers[0].APIKey != "sk-gw" {
		t.Fatalf("unrelated credential must survive the edit: %+v", persisted.Providers[0])
	}
}

func TestProviderManagerRenameFollowsLiveSession(t *testing.T) {
	m := managerTestModel(t)
	m = managerKey(t, m, testKeyText("e"))
	m = managerKey(t, m, testKey(tea.KeyEnter)) // edit Name (cursor 0)
	m.providerWizard.editBuffer = "gateway-main"
	m = managerKey(t, m, testKey(tea.KeyEnter))
	// Save is 5 rows below Name.
	for range 5 {
		m = managerKey(t, m, testKey(tea.KeyDown))
	}
	next, _ := m.handleProviderWizardKey(testKey(tea.KeyEnter))
	if next.providerWizard == nil || next.providerWizard.step != providerWizardStepManage {
		t.Fatalf("save should return to the list, got %+v", next.providerWizard)
	}
	if next.providerName != "gateway-main" {
		t.Fatalf("live session name must follow the rename, got %q", next.providerName)
	}
	persisted := readManagerConfig(t, next.userConfigPath)
	if persisted.ActiveProvider != "gateway-main" {
		t.Fatalf("activeProvider must follow the rename, got %q", persisted.ActiveProvider)
	}
	names := []string{}
	for _, profile := range persisted.Providers {
		names = append(names, profile.Name)
	}
	if len(persisted.Providers) != 2 || persisted.Providers[0].Name != "gateway-main" {
		t.Fatalf("rename not persisted, providers: %v", names)
	}
}

func TestProviderManagerEscWalksBackThenCloses(t *testing.T) {
	m := managerTestModel(t)
	m = managerKey(t, m, testKeyText("e"))
	m = managerKey(t, m, testKey(tea.KeyEnter)) // into Name editor
	m = managerKey(t, m, testKey(tea.KeyEsc))
	if m.providerWizard.step != providerWizardStepEditMenu {
		t.Fatalf("Esc from a field editor must return to the field menu")
	}
	m = managerKey(t, m, testKey(tea.KeyEsc))
	if m.providerWizard.step != providerWizardStepManage {
		t.Fatalf("Esc from the field menu must return to the list")
	}
	m = managerKey(t, m, testKey(tea.KeyEsc))
	if m.providerWizard != nil {
		t.Fatalf("Esc from the list must close the manager")
	}
}

func TestProviderManagerAddReturnsToListOnEsc(t *testing.T) {
	m := managerTestModel(t)
	m = managerKey(t, m, testKeyText("a"))
	if m.providerWizard.step != providerWizardStepMethod {
		t.Fatalf("a must open the add wizard's method step")
	}
	m = managerKey(t, m, testKey(tea.KeyEsc))
	if m.providerWizard == nil || m.providerWizard.step != providerWizardStepManage {
		t.Fatalf("Esc from a manager-opened add flow must return to the list")
	}
}

func TestProviderManagerCredsMsgFillsRowsAndIgnoresStale(t *testing.T) {
	m := managerTestModel(t)
	gen := m.providerWizard.manageCredGen
	next, _ := m.applyProviderManagerCreds(providerManagerCredsMsg{gen: gen, creds: map[string]string{
		"opengateway": "key set",
		"backup":      "no credential",
	}})
	if next.providerWizard.manageRows[0].cred != "key set" || next.providerWizard.manageRows[1].cred != "no credential" {
		t.Fatalf("creds not applied: %+v", next.providerWizard.manageRows)
	}
	// A stale generation must not clobber fresh rows.
	next, _ = next.applyProviderManagerCreds(providerManagerCredsMsg{gen: gen - 1, creds: map[string]string{
		"opengateway": "stale",
	}})
	if next.providerWizard.manageRows[0].cred != "key set" {
		t.Fatalf("stale creds applied: %+v", next.providerWizard.manageRows[0])
	}
}

func readManagerConfig(t *testing.T, path string) config.FileConfig {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var cfg config.FileConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("decode config: %v", err)
	}
	return cfg
}

// TestProviderManagerEditKeyPersistsStoredMarker: replacing the key of a
// provider that previously had NO stored-key marker (e.g. env-authed) must
// persist apiKeyStored — otherwise the secret sits in the credential store
// while every ApplyStoredAPIKey gate skips it (PR #560 review, P2).
func TestProviderManagerEditKeyPersistsStoredMarker(t *testing.T) {
	m := managerTestModel(t)
	// Reshape "backup" into an env-authed profile with no marker and no inline key.
	seedCfg := readManagerConfig(t, m.userConfigPath)
	seedCfg.Providers[1].APIKey = ""
	seedCfg.Providers[1].APIKeyEnv = "BACKUP_API_KEY"
	data, err := json.MarshalIndent(seedCfg, "", "  ")
	if err != nil {
		t.Fatalf("encode config: %v", err)
	}
	if err := os.WriteFile(m.userConfigPath, data, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	m = managerKey(t, m, testKey(tea.KeyDown)) // select "backup"
	m = managerKey(t, m, testKeyText("e"))
	for range 3 { // Name → Endpoint → Model → API key
		m = managerKey(t, m, testKey(tea.KeyDown))
	}
	m = managerKey(t, m, testKey(tea.KeyEnter))
	if m.providerWizard.editField != providerEditFieldAPIKey {
		t.Fatalf("expected API key editor, got field %v", m.providerWizard.editField)
	}
	m.providerWizard.editBuffer = "sk-new-secret"
	m = managerKey(t, m, testKey(tea.KeyEnter))
	m = managerKey(t, m, testKey(tea.KeyDown)) // Description
	m = managerKey(t, m, testKey(tea.KeyDown)) // Save
	next, _ := m.handleProviderWizardKey(testKey(tea.KeyEnter))
	if next.providerWizard == nil || next.providerWizard.step != providerWizardStepManage {
		t.Fatalf("save should return to the list, err=%q", next.providerWizard.err)
	}

	persisted := readManagerConfig(t, next.userConfigPath)
	backup := persisted.Providers[1]
	if !backup.APIKeyStored {
		t.Fatalf("apiKeyStored marker must persist after a key edit: %+v", backup)
	}
	if backup.APIKey != "" {
		t.Fatalf("cleartext key must never land in config.json: %+v", backup)
	}
	// The env reference is deliberately preserved as an explicit override: the
	// resolver fills APIKey from a SET env var first, and the stored key applies
	// only when the env var is absent — locked in here so a future merge change
	// can't silently flip the precedence.
	if backup.APIKeyEnv != "BACKUP_API_KEY" {
		t.Fatalf("APIKeyEnv must survive a key edit as an explicit override, got %q", backup.APIKeyEnv)
	}
	store, err := config.ProviderKeyStoreAt(filepath.Dir(next.userConfigPath))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if key, ok, err := store.Get("backup"); err != nil || !ok || key != "sk-new-secret" {
		t.Fatalf("key must be captured into the store beside the config, got key=%q ok=%v err=%v", key, ok, err)
	}
}

// TestProviderManagerDescriptionClearPersists: clearing a description must
// actually persist (the upsert merge treats empty as "unchanged" — PR #560, P3).
func TestProviderManagerDescriptionClearPersists(t *testing.T) {
	m := managerTestModel(t) // opengateway has description "Main gateway"
	m = managerKey(t, m, testKeyText("e"))
	for range 4 { // Name → Endpoint → Model → API key → Description
		m = managerKey(t, m, testKey(tea.KeyDown))
	}
	m = managerKey(t, m, testKey(tea.KeyEnter))
	if m.providerWizard.editField != providerEditFieldDescription {
		t.Fatalf("expected description editor, got field %v", m.providerWizard.editField)
	}
	m.providerWizard.editBuffer = ""
	m = managerKey(t, m, testKey(tea.KeyEnter))
	m = managerKey(t, m, testKey(tea.KeyDown)) // Save
	next, _ := m.handleProviderWizardKey(testKey(tea.KeyEnter))
	if next.providerWizard == nil || next.providerWizard.step != providerWizardStepManage {
		t.Fatalf("save should return to the list, err=%q", next.providerWizard.err)
	}

	persisted := readManagerConfig(t, next.userConfigPath)
	if persisted.Providers[0].Description != "" {
		t.Fatalf("cleared description must persist, got %q", persisted.Providers[0].Description)
	}
	if row, ok := next.providerWizard.currentManagerRow(); !ok || row.profile.Description != "" {
		t.Fatalf("manager rows must reflect the cleared description: %+v", row.profile)
	}
}

// TestProviderManagerDeleteHintNamesActualOAuthLogin: after a rename the token
// lives under the catalog id, not the profile name — the cleanup hint must name
// the entry `zero auth logout` would actually delete (PR #560 review, P3).
func TestProviderManagerDeleteHintNamesActualOAuthLogin(t *testing.T) {
	m := managerTestModel(t)
	// Reshape "backup" into a renamed OAuth catalog profile: keyless, named
	// "codex", backed by a token stored under the chatgpt catalog id.
	seedCfg := readManagerConfig(t, m.userConfigPath)
	seedCfg.Providers[1] = config.ProviderProfile{Name: "codex", CatalogID: "chatgpt", ProviderKind: config.ProviderKindOpenAICompatible, BaseURL: "https://chatgpt.com/backend-api/codex", Model: "gpt-5.5"}
	data, err := json.MarshalIndent(seedCfg, "", "  ")
	if err != nil {
		t.Fatalf("encode config: %v", err)
	}
	if err := os.WriteFile(m.userConfigPath, data, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	m.savedProviders = seedCfg.Providers
	next, _ := m.reloadProviderManagerRows()
	m = next

	store, err := oauth.NewStore(oauth.StoreOptions{})
	if err != nil {
		t.Fatalf("oauth store: %v", err)
	}
	if err := store.Save(oauth.ProviderKey("chatgpt"), oauth.Token{AccessToken: "bearer-123"}); err != nil {
		t.Fatalf("save token: %v", err)
	}

	m = managerKey(t, m, testKey(tea.KeyDown)) // select "codex"
	m = managerKey(t, m, testKeyText("d"))
	next, cmd := m.handleProviderWizardKey(testKeyText("y"))
	if next.providerWizard == nil {
		t.Fatalf("manager should stay open")
	}
	// The stored-key delete and OAuth hint run in a follow-up cmd off the UI
	// goroutine; drain it into the model like the runtime would.
	next = drainProviderManagerCmds(t, next, cmd)
	status := next.providerWizard.manageStatus
	if !strings.Contains(status, "zero auth logout chatgpt") {
		t.Fatalf("hint must name the stored login (chatgpt), got %q", status)
	}
	if strings.Contains(status, "logout codex") {
		t.Fatalf("hint must not point at a login key that does not exist, got %q", status)
	}
}

// TestProviderManagerReadsStoredKeyBesideConfig: the TUI must READ stored keys
// from the same config-adjacent store its write paths use. managerTestModel
// deliberately puts userConfigPath outside XDG_CONFIG_HOME, so a key seeded
// beside the config is invisible to the default-path store — with the old
// default-store reads, the switch gate rejected the provider and the manager
// showed "stored key missing" for a perfectly healthy profile (PR #560, P2).
func TestProviderManagerReadsStoredKeyBesideConfig(t *testing.T) {
	m := managerTestModel(t)
	// Reshape "backup" into a stored-key profile whose key lives ONLY in the
	// store beside userConfigPath.
	seedCfg := readManagerConfig(t, m.userConfigPath)
	seedCfg.Providers[1].APIKey = ""
	seedCfg.Providers[1].APIKeyStored = true
	data, err := json.MarshalIndent(seedCfg, "", "  ")
	if err != nil {
		t.Fatalf("encode config: %v", err)
	}
	if err := os.WriteFile(m.userConfigPath, data, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	store, err := config.ProviderKeyStoreAt(filepath.Dir(m.userConfigPath))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := store.Set("backup", "sk-beside-config"); err != nil {
		t.Fatalf("seed key: %v", err)
	}
	m.savedProviders = seedCfg.Providers
	next, _ := m.reloadProviderManagerRows()
	m = next

	// The async credential probe must find the key in the config-adjacent store.
	cmd := providerManagerCredsCmd(m.providerWizard.manageCredGen, m.providerWizard.manageRows, m.userConfigPath)
	msg, ok := cmd().(providerManagerCredsMsg)
	if !ok {
		t.Fatalf("expected creds msg")
	}
	if msg.creds["backup"] != "key stored" {
		t.Fatalf("creds probe must read the store beside the config, got %q", msg.creds["backup"])
	}

	// And the switch path must load the key instead of rejecting the provider.
	var built config.ProviderProfile
	m.newProvider = func(profile config.ProviderProfile) (zeroruntime.Provider, error) {
		built = profile
		return &fakeProvider{}, nil
	}
	m = managerKey(t, m, testKey(tea.KeyDown)) // select "backup"
	next, _ = m.handleProviderWizardKey(testKey(tea.KeyEnter))
	if next.providerWizard != nil {
		t.Fatalf("switch must succeed on a config-adjacent stored key, status=%q", next.providerWizard.manageStatus)
	}
	if built.APIKey != "sk-beside-config" {
		t.Fatalf("switch must load the key from the store beside the config, got %q", built.APIKey)
	}
}

// drainProviderManagerCmds executes a manager action's follow-up cmds (batch
// or single) and applies any providerManagerCleanupMsg, mirroring what the
// bubbletea runtime does with the returned tea.Cmd.
func drainProviderManagerCmds(t *testing.T, m model, cmd tea.Cmd) model {
	t.Helper()
	var apply func(c tea.Cmd)
	apply = func(c tea.Cmd) {
		if c == nil {
			return
		}
		switch msg := c().(type) {
		case tea.BatchMsg:
			for _, sub := range msg {
				apply(sub)
			}
		case providerManagerCleanupMsg:
			m, _ = m.applyProviderManagerCleanup(msg)
		}
	}
	apply(cmd)
	return m
}

// TestProviderManagerCaseOnlyRenameUpdatesInPlace: groq → Groq through the
// editor must rename in place — the old EqualFold-skip + case-sensitive upsert
// combination appended a duplicate profile (review finding, empirically shown).
func TestProviderManagerCaseOnlyRenameUpdatesInPlace(t *testing.T) {
	m := managerTestModel(t)
	m = managerKey(t, m, testKeyText("e"))
	m = managerKey(t, m, testKey(tea.KeyEnter)) // edit Name (cursor 0)
	m.providerWizard.editBuffer = "OpenGateway"
	m = managerKey(t, m, testKey(tea.KeyEnter))
	for range 5 {
		m = managerKey(t, m, testKey(tea.KeyDown))
	}
	next, _ := m.handleProviderWizardKey(testKey(tea.KeyEnter)) // Save
	if next.providerWizard == nil || next.providerWizard.step != providerWizardStepManage {
		t.Fatalf("save should return to the list, err=%q", next.providerWizard.err)
	}
	persisted := readManagerConfig(t, next.userConfigPath)
	if len(persisted.Providers) != 2 {
		t.Fatalf("case-only rename must not duplicate: %d providers", len(persisted.Providers))
	}
	if persisted.Providers[0].Name != "OpenGateway" || persisted.Providers[0].APIKey != "sk-gw" {
		t.Fatalf("in-place update wrong: %+v", persisted.Providers[0])
	}
	if persisted.ActiveProvider != "OpenGateway" {
		t.Fatalf("active must follow, got %q", persisted.ActiveProvider)
	}
	if len(next.savedProviders) != 2 || next.savedProviders[0].Name != "OpenGateway" {
		t.Fatalf("in-memory list must mirror the rename: %+v", next.savedProviders)
	}
}

// TestProviderManagerMutationsKeepResolvedProviders: savedProviders is seeded
// from the RESOLVED+FILTERED provider set — a manager delete/edit must mutate
// it surgically, not replace it with the raw user-config list (which would drop
// project-config-contributed providers for the session).
func TestProviderManagerMutationsKeepResolvedProviders(t *testing.T) {
	m := managerTestModel(t)
	// Simulate a project-config-contributed provider: present in the session's
	// resolved list, absent from the user config.json the writers operate on.
	projectProvider := config.ProviderProfile{Name: "team-gateway", ProviderKind: config.ProviderKindOpenAICompatible, BaseURL: "https://team.example.com/v1", APIKey: "sk-team", Model: "team-model"}
	m.savedProviders = append(m.savedProviders, projectProvider)
	next, _ := m.reloadProviderManagerRows()
	m = next

	// Delete "backup" (a user-config profile): team-gateway must survive.
	m = managerKey(t, m, testKey(tea.KeyDown)) // select "backup"
	m = managerKey(t, m, testKeyText("d"))
	next, _ = m.handleProviderWizardKey(testKeyText("y"))
	names := []string{}
	for _, profile := range next.savedProviders {
		names = append(names, profile.Name)
	}
	if len(next.savedProviders) != 2 || names[0] != "opengateway" || names[1] != "team-gateway" {
		t.Fatalf("project-contributed provider lost by delete: %v", names)
	}

	// Edit opengateway's model: team-gateway must still survive.
	m = next
	m.providerWizard.manageCursor = 0
	m = managerKey(t, m, testKeyText("e"))
	for range 2 {
		m = managerKey(t, m, testKey(tea.KeyDown))
	}
	m = managerKey(t, m, testKey(tea.KeyEnter)) // Model field
	m.providerWizard.editBuffer = "mimo-next"
	m = managerKey(t, m, testKey(tea.KeyEnter))
	for range 3 {
		m = managerKey(t, m, testKey(tea.KeyDown))
	}
	next, _ = m.handleProviderWizardKey(testKey(tea.KeyEnter)) // Save
	names = names[:0]
	for _, profile := range next.savedProviders {
		names = append(names, profile.Name)
	}
	if len(next.savedProviders) != 2 || names[1] != "team-gateway" {
		t.Fatalf("project-contributed provider lost by edit: %v", names)
	}
	if next.savedProviders[0].Model != "mimo-next" {
		t.Fatalf("edit not mirrored in-memory: %+v", next.savedProviders[0])
	}
}

// TestProviderManagerEscWalksBackFromDeepAddFlow: Esc anywhere inside a
// manager-launched add flow returns to the provider list ("Esc walks back one
// level"), never destroying the manager context — previously only the first
// step walked back and every deeper step hard-closed the overlay.
func TestProviderManagerEscWalksBackFromDeepAddFlow(t *testing.T) {
	m := managerTestModel(t)
	m = managerKey(t, m, testKeyText("a")) // add flow, method step
	m = managerKey(t, m, testKey(tea.KeyEnter))
	if m.providerWizard == nil || m.providerWizard.step == providerWizardStepMethod {
		t.Fatalf("expected to advance past the method step, got %+v", m.providerWizard)
	}
	deepStep := m.providerWizard.step
	m = managerKey(t, m, testKey(tea.KeyEsc))
	if m.providerWizard == nil {
		t.Fatalf("Esc on step %v must not destroy the manager overlay", deepStep)
	}
	if m.providerWizard.step != providerWizardStepManage {
		t.Fatalf("Esc must return to the provider list, got step %v", m.providerWizard.step)
	}
}

// TestProviderManagerActivationUsesStructuredResult: a refusal whose display
// text happens to contain "Switched to" (a provider literally named that) must
// NOT close the manager as a success — the outcome bool, not UI copy, decides.
func TestProviderManagerActivationUsesStructuredResult(t *testing.T) {
	m := managerTestModel(t)
	seedCfg := readManagerConfig(t, m.userConfigPath)
	seedCfg.Providers[1] = config.ProviderProfile{Name: "Switched to prod", ProviderKind: config.ProviderKindOpenAICompatible, BaseURL: "https://p.example.com/v1", Model: "m"}
	data, err := json.MarshalIndent(seedCfg, "", "  ")
	if err != nil {
		t.Fatalf("encode config: %v", err)
	}
	if err := os.WriteFile(m.userConfigPath, data, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	m.savedProviders = seedCfg.Providers
	next, _ := m.reloadProviderManagerRows()
	m = next

	m = managerKey(t, m, testKey(tea.KeyDown)) // select the credential-less provider
	next, _ = m.handleProviderWizardKey(testKey(tea.KeyEnter))
	if next.providerWizard == nil {
		t.Fatalf("a refused switch must keep the manager open even when the refusal text contains \"Switched to\"")
	}
	if !strings.Contains(next.providerWizard.manageStatus, "no usable credential") {
		t.Fatalf("expected the refusal inline, got %q", next.providerWizard.manageStatus)
	}
	if next.providerName != "opengateway" {
		t.Fatalf("provider must not switch, got %q", next.providerName)
	}
}

// TestProviderManagerCredStateFallsThroughStaleMarker: a stale APIKeyStored
// marker with a SET env var must render the env credential (the runtime falls
// back the same way and switches fine), not "stored key missing".
func TestProviderManagerCredStateFallsThroughStaleMarker(t *testing.T) {
	t.Setenv("STALE_MARKER_KEY", "sk-env")
	profile := config.ProviderProfile{Name: "gw", APIKeyStored: true, APIKeyEnv: "STALE_MARKER_KEY"}
	state := providerManagerCredState(profile, false, nil, map[string]bool{})
	if state != "env STALE_MARKER_KEY" {
		t.Fatalf("stale marker must fall through to the env credential, got %q", state)
	}
	// Marker with neither store entry nor fallback: the broken state still shows.
	t.Setenv("STALE_MARKER_KEY", "")
	state = providerManagerCredState(profile, false, nil, map[string]bool{})
	if state != "stored key missing" {
		t.Fatalf("expected stored key missing with no fallback, got %q", state)
	}
}

func TestProviderManagerRemoveKeepsSharedCredentialForCaseVariantSurvivor(t *testing.T) {
	t.Setenv("ZERO_CRED_STORAGE", "encrypted-file")
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	profiles := []config.ProviderProfile{
		{Name: "work", APIKeyStored: true},
		{Name: "WORK", APIKeyStored: true},
	}
	if err := os.WriteFile(configPath, []byte(`{"activeProvider":"work","providers":[{"name":"work","apiKeyStored":true},{"name":"WORK","apiKeyStored":true}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := config.ProviderKeyStoreAt(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Set("work", "sk-shared"); err != nil {
		t.Fatal(err)
	}
	m := newModel(context.Background(), Options{
		ProviderName:    "work",
		ProviderProfile: profiles[0],
		SavedProviders:  profiles,
		UserConfigPath:  configPath,
	})
	m, _ = m.openProviderManager()
	m.providerWizard.manageCursor = 1
	next, cmd := m.deleteManagerSelection()
	next = drainProviderManagerCmds(t, next, cmd)

	cfg := readManagerConfig(t, configPath)
	if len(cfg.Providers) != 1 || cfg.Providers[0].Name != "work" || !cfg.Providers[0].APIKeyStored {
		t.Fatalf("survivor = %+v, want credentialed work row", cfg.Providers)
	}
	if len(next.savedProviders) != 1 || next.savedProviders[0].Name != "work" {
		t.Fatalf("in-memory survivor = %+v, want work", next.savedProviders)
	}
	if key, ok, getErr := store.Get("work"); getErr != nil || !ok || key != "sk-shared" {
		t.Fatalf("shared key changed: present=%v err=%v", ok, getErr)
	}
	if next.providerName != "work" || next.providerProfile.Name != "work" {
		t.Fatalf("removing WORK changed live work identity: name=%q profile=%q", next.providerName, next.providerProfile.Name)
	}
	if status := next.providerWizard.manageStatus; !strings.Contains(status, "Kept its stored API key") || !strings.Contains(status, "Active provider: work") {
		t.Fatalf("delete status did not describe retained key and surviving active row: %q", status)
	}
}

func TestProviderManagerKeepsDistinctUnicodeLiveProviderOnOtherRowMutation(t *testing.T) {
	newModelWithRows := func(t *testing.T) model {
		t.Helper()
		t.Setenv("ZERO_CRED_STORAGE", "encrypted-file")
		profiles := []config.ProviderProfile{
			{Name: "s", ProviderKind: config.ProviderKindOpenAICompatible, BaseURL: "https://s.example/v1", Model: "s-model"},
			{Name: "ſ", ProviderKind: config.ProviderKindOpenAICompatible, BaseURL: "https://long-s.example/v1", Model: "long-s-model"},
		}
		path := filepath.Join(t.TempDir(), "config.json")
		data, err := json.Marshal(config.FileConfig{ActiveProvider: "ſ", Providers: profiles})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		m := newModel(context.Background(), Options{
			ProviderName:    "ſ",
			ProviderProfile: profiles[1],
			SavedProviders:  profiles,
			UserConfigPath:  path,
		})
		m, _ = m.openProviderManager()
		return m
	}

	t.Run("edit s", func(t *testing.T) {
		t.Setenv(config.ActiveProviderEnv, "ſ")
		m := newModelWithRows(t)
		m.providerWizard.beginProviderEdit(m.savedProviders[0], managerRowOwnership(t, m, m.savedProviders[0].Name))
		m.providerWizard.editDraft.Model = "s-updated"
		next, _ := m.saveManagerEdit()
		if next.providerName != "ſ" || next.providerProfile.Name != "ſ" {
			t.Fatalf("editing s rewrote live long-s identity: name=%q profile=%q", next.providerName, next.providerProfile.Name)
		}
		if got := os.Getenv(config.ActiveProviderEnv); got != "ſ" {
			t.Fatalf("%s = %q, want long-s unchanged", config.ActiveProviderEnv, got)
		}
		if next.savedProviders[0].Model != "s-updated" || next.savedProviders[1].Name != "ſ" {
			t.Fatalf("wrong in-memory edit target: %+v", next.savedProviders)
		}
	})

	t.Run("remove s", func(t *testing.T) {
		m := newModelWithRows(t)
		m.providerWizard.manageCursor = 0
		next, _ := m.deleteManagerSelection()
		if next.providerName != "ſ" {
			t.Fatalf("removing s changed live long-s provider to %q", next.providerName)
		}
		if len(next.savedProviders) != 1 || next.savedProviders[0].Name != "ſ" {
			t.Fatalf("wrong in-memory removal target: %+v", next.savedProviders)
		}
	})
}

// A session can spell its provider differently from the row it runs on —
// ZERO_PROVIDER=work against a saved "WORK", resumed session metadata, or a
// `zero providers use work` from another terminal. When that row is the SOLE
// carrier of the credential identity there is no other row the session could
// mean, so the manager must mark it active and carry a rename onto the live
// session; otherwise ZERO_PROVIDER keeps exporting a name no row answers to.
func TestProviderManagerSoleRowCaseVariantTracksLiveSession(t *testing.T) {
	t.Setenv(config.ActiveProviderEnv, "work")
	profile := config.ProviderProfile{
		Name:         "WORK",
		ProviderKind: config.ProviderKindOpenAICompatible,
		BaseURL:      "https://other.example/v1",
		Model:        "other-model",
	}
	path := filepath.Join(t.TempDir(), "config.json")
	data, err := json.Marshal(config.FileConfig{ActiveProvider: "WORK", Providers: []config.ProviderProfile{profile}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	m := newModel(context.Background(), Options{
		ProviderName:    "work",
		ProviderProfile: config.ProviderProfile{Name: "work"},
		SavedProviders:  []config.ProviderProfile{profile},
		UserConfigPath:  path,
	})
	m, _ = m.openProviderManager()

	// The row the session actually runs on must render as active even though
	// the session spells it differently.
	if got := m.providerWizard.manageActiveName; got != "WORK" {
		t.Fatalf("manageActiveName = %q, want the sole row's spelling WORK", got)
	}

	m.providerWizard.beginProviderEdit(profile, managerRowOwnership(t, m, profile.Name))
	m.providerWizard.editDraft.Name = "OFFICE"
	next, _ := m.saveManagerEdit()

	if next.providerWizard == nil || next.providerWizard.err != "" {
		t.Fatalf("sole-row case-variant edit failed: %+v", next.providerWizard)
	}
	if next.providerName != "OFFICE" || next.providerProfile.Name != "OFFICE" {
		t.Fatalf("rename did not follow the live session: name=%q profile=%q", next.providerName, next.providerProfile.Name)
	}
	if got := os.Getenv(config.ActiveProviderEnv); got != "OFFICE" {
		t.Fatalf("%s = %q, want the renamed row so spawned children resolve it", config.ActiveProviderEnv, got)
	}
	if len(next.savedProviders) != 1 || next.savedProviders[0].Name != "OFFICE" {
		t.Fatalf("wrong in-memory edit target: %+v", next.savedProviders)
	}
	cfg := readManagerConfig(t, path)
	if len(cfg.Providers) != 1 || cfg.Providers[0].Name != "OFFICE" {
		t.Fatalf("wrong persisted edit target: %+v", cfg.Providers)
	}
}

// The sole-row resolution above must NOT reach case-variant siblings: with both
// "work" and "WORK" persisted, a session on "work" is one specific row, and a
// delete aimed at the other must leave it alone. (Edit cannot be exercised here
// — EditProvider validates the duplicate-identity config before mutating.)
func TestProviderManagerCaseVariantDeleteDoesNotChangeLiveSibling(t *testing.T) {
	t.Setenv("ZERO_CRED_STORAGE", "encrypted-file")
	t.Setenv(config.ActiveProviderEnv, "work")
	profiles := []config.ProviderProfile{
		{Name: "work", ProviderKind: config.ProviderKindOpenAICompatible, BaseURL: "https://work.example/v1", Model: "work-model"},
		{Name: "WORK", ProviderKind: config.ProviderKindOpenAICompatible, BaseURL: "https://other.example/v1", Model: "other-model"},
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"activeProvider":"work","providers":[{"name":"work"},{"name":"WORK"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	m := newModel(context.Background(), Options{
		ProviderName:    "work",
		ProviderProfile: profiles[0],
		SavedProviders:  profiles,
		UserConfigPath:  path,
	})
	m, _ = m.openProviderManager()
	// Exact spelling wins, so the live row is "work" and not its sibling.
	if got := m.providerWizard.manageActiveName; got != "work" {
		t.Fatalf("manageActiveName = %q, want the exact live row work", got)
	}

	m.providerWizard.manageCursor = 1
	next, _ := m.deleteManagerSelection()

	if next.providerName != "work" || next.providerProfile.Name != "work" {
		t.Fatalf("deleting WORK rewrote live work identity: name=%q profile=%q", next.providerName, next.providerProfile.Name)
	}
	if got := os.Getenv(config.ActiveProviderEnv); got != "work" {
		t.Fatalf("%s = %q, want live work unchanged", config.ActiveProviderEnv, got)
	}
	if status := next.providerWizard.manageStatus; strings.Contains(status, "keeps running on it until you switch") {
		t.Fatalf("delete of the sibling row claimed the live session runs on it: %q", status)
	}
	if len(next.savedProviders) != 1 || next.savedProviders[0].Name != "work" {
		t.Fatalf("wrong in-memory removal target: %+v", next.savedProviders)
	}
}

func TestProviderManagerAmbiguousCaseVariantSessionDoesNotGuessLiveRow(t *testing.T) {
	providers := []config.ProviderProfile{{Name: "work"}, {Name: "WORK"}}
	if got := sessionRowName("Work", providers); got != "Work" {
		t.Fatalf("sessionRowName = %q, want unresolved live spelling Work", got)
	}
	for _, row := range providers {
		if sessionRefersToPersistedRow("Work", row.Name, providers) {
			t.Fatalf("ambiguous live spelling must not select row %q", row.Name)
		}
	}
}

func TestProviderManagerCleanupRedactsCredentialStoreError(t *testing.T) {
	t.Setenv("ZERO_CRED_STORAGE", "file")
	secret := "sk-proj-12345678901234567890"
	dir := filepath.Join(t.TempDir(), secret)
	if err := os.MkdirAll(filepath.Join(dir, "credentials.json.lock"), 0o700); err != nil {
		t.Fatal(err)
	}
	msg, ok := providerManagerCleanupCmd(filepath.Join(dir, "config.json"), config.ProviderProfile{Name: "work"}, true)().(providerManagerCleanupMsg)
	if !ok {
		t.Fatal("cleanup command returned the wrong message type")
	}
	text := strings.Join(msg.notes, " ")
	if strings.Contains(text, secret) {
		t.Fatalf("cleanup warning leaked credential-like text: %q", text)
	}
	if !strings.Contains(text, "could not be deleted") {
		t.Fatalf("cleanup warning missing failure context: %q", text)
	}
}

// The confirmation prompt must promise what the delete actually does: with a
// case variant that still claims the shared credential, the key is kept, so
// the prompt must not say it is about to be removed.
func TestProviderManagerDeleteConfirmMatchesKeyRetentionPolicy(t *testing.T) {
	t.Setenv("ZERO_CRED_STORAGE", "encrypted-file")

	newManagerAtRow := func(t *testing.T, configJSON string, profiles []config.ProviderProfile, cursor int) model {
		t.Helper()
		dir := t.TempDir()
		configPath := filepath.Join(dir, "config.json")
		if err := os.WriteFile(configPath, []byte(configJSON), 0o600); err != nil {
			t.Fatal(err)
		}
		m := newModel(context.Background(), Options{
			ProviderName:    profiles[0].Name,
			ProviderProfile: profiles[0],
			SavedProviders:  profiles,
			UserConfigPath:  configPath,
		})
		m, _ = m.openProviderManager()
		m.providerWizard.manageCursor = cursor
		next, _ := m.handleProviderWizardKey(testKeyText("d"))
		if !next.providerWizard.manageDeleting {
			t.Fatal("d must arm the delete confirm")
		}
		return next
	}

	t.Run("shared credential is kept", func(t *testing.T) {
		m := newManagerAtRow(t,
			`{"activeProvider":"work","providers":[{"name":"work","apiKeyStored":true},{"name":"WORK","apiKeyStored":true}]}`,
			[]config.ProviderProfile{{Name: "work", APIKeyStored: true}, {Name: "WORK", APIKeyStored: true}},
			1,
		)
		if m.providerWizard.manageDeleteKeyNote == "" {
			t.Fatal("retention not resolved for a survivor that claims the credential")
		}
		view := strings.Join(m.providerWizard.renderManageStep(80), "\n")
		if !strings.Contains(view, "stored API key is kept") {
			t.Fatalf("confirm text = %q, want the key-kept wording", view)
		}
	})

	t.Run("last owner removal deletes the key", func(t *testing.T) {
		m := newManagerAtRow(t,
			`{"activeProvider":"work","providers":[{"name":"work","apiKeyStored":true},{"name":"other"}]}`,
			[]config.ProviderProfile{{Name: "work", APIKeyStored: true}, {Name: "other"}},
			0,
		)
		if m.providerWizard.manageDeleteKeyNote == "" {
			t.Fatal("delete confirmation made no claim about a persisted row's key")
		}
		view := strings.Join(m.providerWizard.renderManageStep(80), "\n")
		if !strings.Contains(view, "also removes its stored API key") {
			t.Fatalf("confirm text = %q, want the key-removal wording", view)
		}
	})
}

// A markerless case variant does not own the shared credential, so removing
// the only row that claimed it must delete the secret rather than orphan it
// behind a profile ApplyStoredAPIKey will never read.
func TestProviderManagerRemoveDeletesKeyWhenSurvivorNeverClaimedIt(t *testing.T) {
	t.Setenv("ZERO_CRED_STORAGE", "encrypted-file")
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	profiles := []config.ProviderProfile{
		{Name: "work", APIKeyStored: true},
		{Name: "WORK"},
	}
	if err := os.WriteFile(configPath, []byte(`{"activeProvider":"work","providers":[{"name":"work","apiKeyStored":true},{"name":"WORK"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := config.ProviderKeyStoreAt(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Set("work", "sk-shared"); err != nil {
		t.Fatal(err)
	}
	m := newModel(context.Background(), Options{
		ProviderName:    "work",
		ProviderProfile: profiles[0],
		SavedProviders:  profiles,
		UserConfigPath:  configPath,
	})
	m, _ = m.openProviderManager()
	m.providerWizard.manageCursor = 0
	next, cmd := m.deleteManagerSelection()
	next = drainProviderManagerCmds(t, next, cmd)

	if _, ok, getErr := store.Get("WORK"); getErr != nil {
		t.Fatal(getErr)
	} else if ok {
		t.Fatal("shared key was orphaned behind a markerless survivor")
	}
	if status := next.providerWizard.manageStatus; !strings.Contains(status, "stored API key will also be deleted") {
		t.Fatalf("delete status = %q, want the key-deletion note", status)
	}
}

func TestProviderManagerDeleteReconcilesStoredKeyMarkers(t *testing.T) {
	for _, tc := range []struct {
		name       string
		retainKey  bool
		failDelete bool
	}{
		{name: "successful removal"},
		{name: "shared credential retained", retainKey: true},
		{name: "credential deletion fails", failDelete: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ZERO_CRED_STORAGE", "encrypted-file")
			dir := t.TempDir()
			t.Setenv("ZERO_OAUTH_TOKENS_PATH", filepath.Join(dir, "oauth.json"))
			configPath := filepath.Join(dir, "config.json")
			profiles := []config.ProviderProfile{
				{Name: "work", APIKeyStored: true},
				{Name: "WORK", APIKeyStored: tc.retainKey},
				{Name: "other", APIKeyStored: true},
			}
			data, err := json.Marshal(config.FileConfig{ActiveProvider: "work", Providers: profiles})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(configPath, data, 0o600); err != nil {
				t.Fatal(err)
			}
			store, err := config.ProviderKeyStoreAt(dir)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.Set("work", "shared-key"); err != nil {
				t.Fatal(err)
			}
			// Resolved session state can carry a marker from another source even
			// when the persisted sibling does not claim the shared credential.
			profiles[1].APIKeyStored = true
			live := profiles[0]
			live.APIKey = "live-inline-key"
			m := newModel(context.Background(), Options{
				ProviderName: "work", ProviderProfile: live,
				SavedProviders: profiles, UserConfigPath: configPath,
			})
			m, _ = m.openProviderManager()
			m = managerKey(t, m, testKeyText("d"))
			lockPath := filepath.Join(dir, "credentials.enc.lock")
			if tc.failDelete {
				if err := os.Remove(lockPath); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(lockPath, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			next, cmd := m.handleProviderWizardKey(testKeyText("y"))
			next = drainProviderManagerCmds(t, next, cmd)
			if tc.failDelete {
				if err := os.Remove(lockPath); err != nil {
					t.Fatal(err)
				}
			}
			wantStored := tc.retainKey || tc.failDelete
			key, present, err := store.Get("WORK")
			if err != nil || present != wantStored || (present && key != "shared-key") {
				t.Fatalf("credential outcome: present=%v err=%v, want present=%v and unchanged key", present, err, wantStored)
			}
			if next.providerProfile.APIKeyStored != wantStored {
				t.Errorf("live APIKeyStored = %v, want %v after manager cleanup", next.providerProfile.APIKeyStored, wantStored)
			}
			for _, profile := range next.savedProviders {
				want := true
				if profile.Name == "WORK" {
					want = wantStored
				}
				if profile.APIKeyStored != want {
					t.Errorf("saved %s APIKeyStored = %v, want %v", profile.Name, profile.APIKeyStored, want)
				}
			}
			if next.providerName != "work" || next.providerProfile.APIKey != "live-inline-key" {
				t.Fatal("cleanup changed the continuing provider or its inline credential")
			}
			if !tc.failDelete && (next.removedLiveRow != "work" || len(next.savedProviders) != 2) {
				t.Fatal("manager did not remove and retain the live row's identity")
			}
		})
	}
}

// A row visible only because Resolve() synthesized it from an env var has no
// persisted profile and no stored key, so the confirmation must make no claim
// about a key rather than promising a removal that cannot happen. The same
// holds when the config is too ambiguous for the delete to proceed at all.
func TestProviderDeleteKeyNoteMakesNoClaimWithoutAResolvableRow(t *testing.T) {
	t.Setenv("ZERO_CRED_STORAGE", "encrypted-file")

	cases := []struct {
		name       string
		configJSON string
		row        string
		// resolved is every row spelling the manager is displaying.
		resolved []string
	}{
		{
			name:       "env-derived row with no persisted profile",
			configJSON: `{"providers":[{"name":"other"}]}`,
			row:        "openai",
			resolved:   []string{"other", "openai"},
		},
		{
			name:       "ambiguous duplicate rows the delete cannot resolve",
			configJSON: `{"providers":[{"name":"work","apiKeyStored":true},{"name":"WORK","apiKeyStored":true}]}`,
			row:        "Work",
			resolved:   []string{"work", "WORK", "Work"},
		},
		{
			// The project row's identity belongs to the user row listed beside
			// it, so this row owns nothing on disk and promises nothing.
			name:       "project row whose identity a listed user row owns",
			configJSON: `{"providers":[{"name":"work","apiKeyStored":true}]}`,
			row:        "WORK",
			resolved:   []string{"work", "WORK"},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, []byte(testCase.configJSON), 0o600); err != nil {
				t.Fatal(err)
			}
			owner, err := config.ProviderRowOwnershipAt(path, testCase.resolved, testCase.row)
			if err != nil {
				t.Fatal(err)
			}
			if note := providerDeleteKeyNote(path, owner); note != "" {
				t.Fatalf("note = %q, want no claim about the stored key", note)
			}
		})
	}
	// No user config path at all: nothing can be promised either.
	if note := providerDeleteKeyNote("", config.ProviderRowOwnership{UserBacked: true, PersistedName: "work"}); note != "" {
		t.Fatalf("note = %q, want no claim without a config path", note)
	}
}

// The preview must resolve the row the same way the delete does: a case-variant
// spelling that removes nothing would preview "key kept" for a delete that
// resolves the row and takes the key with it.
func TestProviderDeleteKeyNoteResolvesCaseVariantSpelling(t *testing.T) {
	t.Setenv("ZERO_CRED_STORAGE", "encrypted-file")
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"providers":[{"name":"WORK","apiKeyStored":true},{"name":"other"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// A session alias can address the sole WORK row when no concrete resolved
	// row named "work" exists. A project row with that name is tested separately.
	owner, err := config.ProviderRowOwnershipAt(path, nil, "work")
	if err != nil {
		t.Fatal(err)
	}
	if !owner.UserBacked || owner.PersistedName != "WORK" {
		t.Fatalf("ownership = %+v, want the sole WORK row", owner)
	}
	note := providerDeleteKeyNote(path, owner)
	if !strings.Contains(note, "also removes its stored API key") {
		t.Fatalf("note = %q, want the key-removal wording for the resolved row", note)
	}
}

// managerRowOwnership resolves a row's provenance exactly as
// reloadProviderManagerRows does, so a test that drives beginProviderEdit
// directly cannot hand the edit a stronger ownership than the manager would.
func managerRowOwnership(t *testing.T, m model, name string) config.ProviderRowOwnership {
	t.Helper()
	owner, err := config.ProviderRowOwnershipAt(m.userConfigPath, config.ProviderProfileNames(m.savedProviders), name)
	if err != nil {
		t.Fatalf("resolve ownership for %q: %v", name, err)
	}
	return owner
}
