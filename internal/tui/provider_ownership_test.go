package tui

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Gitlawb/zero/internal/config"
	"github.com/Gitlawb/zero/internal/credstore"
	"github.com/Gitlawb/zero/internal/zeroruntime"
)

// caseSiblingModel builds the shape the resolver validly produces and the
// identity comparisons could not tell apart: user config holds "work", and the
// session ALSO resolved a project-config "WORK" with its own endpoint and model.
//
// activeName puts either row in the active seat, because the defect behaved
// differently depending on which one the session ran on.
//
// builtProfiles records every profile handed to newProvider, so a test can
// assert which endpoint a selection actually built — the outcome neither a
// status line nor a config row can show.
func caseSiblingModel(t *testing.T, activeName string, builtProfiles *[]config.ProviderProfile) model {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("APPDATA", home)
	t.Setenv("LOCALAPPDATA", home)
	t.Setenv("XDG_CACHE_HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	t.Setenv("ZERO_OAUTH_TOKENS_PATH", filepath.Join(home, "oauth-tokens.json"))
	t.Setenv("ZERO_CRED_STORAGE", "encrypted-file")

	configPath, err := config.DefaultUserConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatal(err)
	}
	userRow := config.ProviderProfile{
		Name:         "work",
		ProviderKind: config.ProviderKindOpenAICompatible,
		BaseURL:      "https://user.example.com/v1",
		Model:        "user-model",
		Description:  "User row",
		APIKeyStored: true,
	}
	seed := config.FileConfig{ActiveProvider: activeName, Providers: []config.ProviderProfile{userRow}}
	data, err := json.MarshalIndent(seed, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := config.ProviderKeyStore()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Set("work", "sk-user"); err != nil {
		t.Fatal(err)
	}

	// The project row exists only in the resolved/session list, exactly as
	// cross-layer merging produces it: same credential identity, different
	// endpoint and model, and NO row of its own in config.json.
	projectRow := config.ProviderProfile{
		Name:         "WORK",
		ProviderKind: config.ProviderKindOpenAICompatible,
		BaseURL:      "https://project.example.com/v1",
		Model:        "project-model",
		Description:  "Project row",
		APIKey:       "sk-project",
	}
	resolved := []config.ProviderProfile{userRow, projectRow}
	active := userRow
	if activeName == "WORK" {
		active = projectRow
	}
	m := newModel(context.Background(), Options{
		ProviderName:    activeName,
		ModelName:       active.Model,
		Provider:        &fakeProvider{},
		ProviderProfile: active,
		SavedProviders:  resolved,
		UserConfigPath:  configPath,
		NewProvider: func(profile config.ProviderProfile) (zeroruntime.Provider, error) {
			if builtProfiles != nil {
				*builtProfiles = append(*builtProfiles, profile)
			}
			return &fakeProvider{}, nil
		},
	})
	m.width = 120
	m.height = 40
	next, _ := m.openProviderManager()
	return next
}

func TestProviderManagerFilteredUserRowDoesNotGrantProjectOwnership(t *testing.T) {
	for _, action := range []string{"edit", "delete", "model"} {
		t.Run(action, func(t *testing.T) {
			m := caseSiblingModel(t, "WORK", nil)
			// The user row is filtered out when its stored key cannot be loaded.
			// Ownership must not depend on that row still being displayed.
			m.savedProviders = m.savedProviders[1:]
			m, _ = m.openProviderManager()
			before, err := os.ReadFile(m.userConfigPath)
			if err != nil {
				t.Fatal(err)
			}
			switch action {
			case "edit":
				m = managerKey(t, m, testKeyText("e"))
				if m.providerWizard.step == providerWizardStepEditMenu {
					t.Fatal("project row acquired edit ownership after user row was filtered")
				}
			case "delete":
				m = managerKey(t, m, testKeyText("d"))
				next, cmd := m.handleProviderWizardKey(testKeyText("y"))
				m = drainProviderManagerCmds(t, next, cmd)
			case "model":
				persisted, _, err := m.persistSelectedModel(config.ProviderProfile{Name: "WORK", Model: "changed"})
				if err != nil || persisted {
					t.Fatalf("project model persistence = %t, error=%v", persisted, err)
				}
			}
			assertUserRowUntouched(t, m, before)
		})
	}
}

// selectManagerRow moves the manager cursor onto the named row.
func selectManagerRow(t *testing.T, m model, name string) model {
	t.Helper()
	for index, row := range m.providerWizard.manageRows {
		if strings.TrimSpace(row.profile.Name) == name {
			m.providerWizard.manageCursor = index
			return m
		}
	}
	t.Fatalf("manager has no row %q", name)
	return m
}

// assertUserRowUntouched pins both durable surfaces at once: the config bytes
// and the credential store.
func assertUserRowUntouched(t *testing.T, m model, before []byte) {
	t.Helper()
	after, err := os.ReadFile(m.userConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("user config changed:\nbefore=%s\nafter=%s", before, after)
	}
	store, err := config.ProviderKeyStore()
	if err != nil {
		t.Fatal(err)
	}
	key, ok, err := store.Get("work")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || key != "sk-user" {
		t.Fatalf("user credential changed: present=%t", ok)
	}
}

// Deleting the project row must remove it from the SESSION and leave the user's
// row and its credential exactly as they were. The delete used to resolve the
// project row's spelling onto the user row and remove that instead, so the row
// gone from disk was not the row gone from the list.
func TestProviderManagerDeleteProjectRowLeavesUserRowIntact(t *testing.T) {
	for _, activeName := range []string{"work", "WORK"} {
		t.Run("active_"+activeName, func(t *testing.T) {
			m := caseSiblingModel(t, activeName, nil)
			before, err := os.ReadFile(m.userConfigPath)
			if err != nil {
				t.Fatal(err)
			}
			m = selectManagerRow(t, m, "WORK")
			m = managerKey(t, m, testKeyText("d"))
			// The confirmation must promise nothing about a key it cannot delete.
			if note := m.providerWizard.manageDeleteKeyNote; note != "" {
				t.Fatalf("delete note = %q, want no claim for a row with no config row", note)
			}
			next, cmd := m.handleProviderWizardKey(testKeyText("y"))
			next = drainProviderManagerCmds(t, next, cmd)

			assertUserRowUntouched(t, next, before)
			if len(next.savedProviders) != 1 || next.savedProviders[0].Name != "work" {
				t.Fatalf("savedProviders = %+v, want only the user row left in session", next.savedProviders)
			}
			if next.providerWizard == nil {
				t.Fatal("manager closed while a provider remains")
			}
			if !strings.Contains(next.providerWizard.manageStatus, "nothing in config.json changed") {
				t.Fatalf("status = %q, want it to say the config was untouched", next.providerWizard.manageStatus)
			}
		})
	}
}

// Editing the project row must not apply this row's draft — a replacement key
// included — to the user row that merely shares its credential identity.
func TestProviderManagerEditProjectRowIsRefused(t *testing.T) {
	for _, activeName := range []string{"work", "WORK"} {
		t.Run("active_"+activeName, func(t *testing.T) {
			m := caseSiblingModel(t, activeName, nil)
			before, err := os.ReadFile(m.userConfigPath)
			if err != nil {
				t.Fatal(err)
			}
			m = selectManagerRow(t, m, "WORK")
			m = managerKey(t, m, testKeyText("e"))

			if m.providerWizard.step == providerWizardStepEditMenu {
				t.Fatal("edit opened for a row with no config.json row of its own")
			}
			if !strings.Contains(m.providerWizard.manageStatus, "Can't edit WORK") {
				t.Fatalf("status = %q, want a refusal naming the row", m.providerWizard.manageStatus)
			}
			assertUserRowUntouched(t, m, before)

			// The user row beside it stays editable — the refusal is about
			// provenance, not about the name colliding.
			m = selectManagerRow(t, m, "work")
			m = managerKey(t, m, testKeyText("e"))
			if m.providerWizard.step != providerWizardStepEditMenu {
				t.Fatalf("user row must stay editable, step = %v status = %q", m.providerWizard.step, m.providerWizard.manageStatus)
			}
		})
	}
}

// assertUserProviderRowUnchanged is assertUserRowUntouched for the paths that
// legitimately write elsewhere in config.json — model selection records recent
// models under preferences — so the assertion is on the provider row and the
// active pointer rather than the whole file.
func assertUserProviderRowUnchanged(t *testing.T, m model, want config.ProviderProfile, wantActive string) {
	t.Helper()
	cfg := readManagerConfig(t, m.userConfigPath)
	if cfg.ActiveProvider != wantActive {
		t.Fatalf("activeProvider = %q, want %q", cfg.ActiveProvider, wantActive)
	}
	found := false
	for _, provider := range cfg.Providers {
		if provider.Name != want.Name {
			continue
		}
		found = true
		if provider.Model != want.Model || provider.BaseURL != want.BaseURL ||
			provider.Description != want.Description || provider.APIKeyStored != want.APIKeyStored {
			t.Fatalf("user row changed:\n got %+v\nwant %+v", provider, want)
		}
	}
	if !found {
		t.Fatalf("user row %q is gone from %+v", want.Name, cfg.Providers)
	}
	store, err := config.ProviderKeyStore()
	if err != nil {
		t.Fatal(err)
	}
	key, ok, err := store.Get("work")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || key != "sk-user" {
		t.Fatalf("user credential changed: present=%t", ok)
	}
}

// Choosing a model listed under the project row must build THAT endpoint and
// must not write the model onto the user row. The picker used to treat the two
// as one provider through credential normalization.
func TestModelPickerSelectionStaysOnTheOwningRow(t *testing.T) {
	for _, activeName := range []string{"work", "WORK"} {
		t.Run("active_"+activeName, func(t *testing.T) {
			var built []config.ProviderProfile
			m := caseSiblingModel(t, activeName, &built)
			m.providerWizard = nil
			userRow := config.ProviderProfile{
				Name:         "work",
				BaseURL:      "https://user.example.com/v1",
				Model:        "user-model",
				Description:  "User row",
				APIKeyStored: true,
			}

			m.picker = &commandPicker{
				kind:  pickerModel,
				items: []pickerItem{{Label: "project-next", Value: "project-next", OwnerProvider: "WORK"}},
			}
			updated, _ := m.choosePicker()
			next, ok := updated.(model)
			if !ok {
				t.Fatalf("choosePicker returned %T", updated)
			}

			// The user row's model must not move, and its key must not be touched.
			assertUserProviderRowUnchanged(t, next, userRow, activeName)
			for _, profile := range built {
				if strings.TrimSpace(profile.Name) == "work" {
					t.Fatalf("selection under WORK built the user row's endpoint: %+v", profile)
				}
			}
			if activeName == "work" {
				// A real switch: the project endpoint must be what got built.
				if len(built) == 0 {
					t.Fatal("no provider was built for a cross-row selection")
				}
				last := built[len(built)-1]
				if last.BaseURL != "https://project.example.com/v1" {
					t.Fatalf("built endpoint = %q, want the project row's", last.BaseURL)
				}
			}
			// Whatever happened, the session must not claim to run on the user row
			// under the project row's model.
			if next.providerName == "work" && next.modelName == "project-next" {
				t.Fatalf("project row's model landed on the user row: provider=%q model=%q", next.providerName, next.modelName)
			}
		})
	}
}

// Both rows must not read as active at once: they are different endpoints, and
// the picker is where the user decides between them.
func TestActiveProviderRowNameIsTheExactRow(t *testing.T) {
	for _, activeName := range []string{"work", "WORK"} {
		t.Run("active_"+activeName, func(t *testing.T) {
			m := caseSiblingModel(t, activeName, nil)
			other := "WORK"
			if activeName == "WORK" {
				other = "work"
			}
			if credstore.NormalizeProvider(activeName) != credstore.NormalizeProvider(other) {
				t.Fatalf("fixture no longer exercises a shared credential identity: %q vs %q", activeName, other)
			}
			if got := m.activeProviderRowName(); got != activeName {
				t.Fatalf("activeProviderRowName = %q, want the exact active row %q", got, activeName)
			}
		})
	}
}

// syncSavedProviderModel is a PARTIAL update: persisting a model must not clear
// the description config.json keeps, and must not reach other holders of the
// slice through the shared backing array.
func TestSyncSavedProviderModelPreservesTheRestOfTheProfile(t *testing.T) {
	saved := []config.ProviderProfile{
		{Name: "work", Model: "old", Description: "User row", BaseURL: "https://user.example.com/v1", APIKeyStored: true},
		{Name: "other", Model: "other-model", Description: "Other"},
	}
	snapshot := append([]config.ProviderProfile{}, saved...)

	updated := syncSavedProviderModel(saved, "work", "new")

	if updated[0].Model != "new" {
		t.Fatalf("model not updated: %+v", updated[0])
	}
	for _, field := range []struct{ name, got, want string }{
		{"Description", updated[0].Description, "User row"},
		{"BaseURL", updated[0].BaseURL, "https://user.example.com/v1"},
	} {
		if field.got != field.want {
			t.Fatalf("%s = %q, want %q", field.name, field.got, field.want)
		}
	}
	if !updated[0].APIKeyStored {
		t.Fatal("stored-key marker cleared by a model-only sync")
	}
	if updated[1].Name != snapshot[1].Name || updated[1].Model != snapshot[1].Model ||
		updated[1].Description != snapshot[1].Description {
		t.Fatalf("unrelated row changed: %+v", updated[1])
	}
	// The slice handed in must be unchanged: a picker snapshot taken before the
	// switch must not observe the new model through the same backing array.
	if saved[0].Model != "old" {
		t.Fatalf("input slice mutated in place: %+v", saved[0])
	}
}

func TestModelPickerAfterDeletingLiveProjectRow(t *testing.T) {
	var built []config.ProviderProfile
	m := caseSiblingModel(t, "WORK", &built)
	wantUser := m.savedProviders[0]
	t.Setenv("ZERO_PROVIDER", "WORK")
	before, err := os.ReadFile(m.userConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	m = selectManagerRow(t, m, "WORK")
	m = managerKey(t, m, testKeyText("d"))
	next, cmd := m.handleProviderWizardKey(testKeyText("y"))
	m = drainProviderManagerCmds(t, next, cmd)
	assertUserRowUntouched(t, m, before)
	if m.providerName != "WORK" || m.providerProfile.BaseURL != "https://project.example.com/v1" {
		t.Fatal("deletion changed the running client")
	}
	if m.activeProviderRowName() != "WORK" || m.providerWizard.manageActiveName != "WORK" {
		t.Error("deletion reassigned the live identity to the surviving user row")
	}
	m.providerWizard = nil
	m.picker = m.newModelPicker()
	if m.picker == nil {
		t.Fatal("missing picker")
	}
	found := false
	chosen := ""
	for i, item := range m.picker.items {
		if item.OwnerProvider == "work" && item.Value != "" {
			m.picker.selected = i
			chosen = item.Value
			found = true
			break
		}
	}
	if !found {
		t.Fatal("actual picker did not offer the surviving user model")
	}
	updated, _ := m.choosePicker()
	m = updated.(model)
	if len(built) == 0 {
		t.Fatal("selection did not construct a provider")
	}
	p := built[len(built)-1]
	if p.Name != "work" || p.BaseURL != "https://user.example.com/v1" || p.APIKey != "sk-user" {
		t.Fatal("picker constructed the continuing project profile instead of the selected user profile")
	}
	if m.providerName != "work" || m.modelName != chosen || m.activeProviderRowName() != "work" || len(m.savedProviders) != 1 {
		t.Fatal("live/saved identity did not follow selection")
	}
	cfg := readManagerConfig(t, m.userConfigPath)
	if cfg.ActiveProvider != "work" || cfg.Providers[0].Model != chosen {
		t.Fatal("user selection was not persisted")
	}
	wantUser.Model = chosen
	assertUserProviderRowUnchanged(t, m, wantUser, "work")
}
