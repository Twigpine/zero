// Provider manager: the list-first /provider surface. It shares
// providerWizardState (steps providerWizardStepManage / EditMenu / EditValue)
// so the existing overlay gating, key routing, and mouse plumbing that check
// m.providerWizard cover it without touching those call sites.
//
// UX contract: the list IS the home screen — Enter activates the selected
// provider, `a` opens the add wizard, `e` opens a field-level editor, `d`
// asks inline and deletes. Esc walks back one level and closes from the list.
package tui

import (
	"os"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/Gitlawb/zero/internal/config"
	"github.com/Gitlawb/zero/internal/oauth"
)

const providerManagerMaxVisible = 10

// providerManagerRow is one saved provider in the list. cred is resolved
// asynchronously (keychain reads shell out to `security` on macOS and must
// never block the render loop); empty means "still checking".
type providerManagerRow struct {
	profile config.ProviderProfile
	local   bool
	cred    string
}

type providerEditField int

const (
	providerEditFieldName providerEditField = iota
	providerEditFieldEndpoint
	providerEditFieldModel
	providerEditFieldAPIKey
	providerEditFieldDescription
	providerEditFieldSave
)

var providerEditFields = []struct {
	field providerEditField
	label string
	hint  string
}{
	{providerEditFieldName, "Name", "Renames the profile; a stored key and the active pointer follow automatically."},
	{providerEditFieldEndpoint, "Endpoint", "Base URL for the provider's API."},
	{providerEditFieldModel, "Model", "Default model for this provider."},
	{providerEditFieldAPIKey, "API key", "Enter a new key to replace the stored one; leave empty to keep it."},
	{providerEditFieldDescription, "Description", "Optional label shown in the provider list."},
	{providerEditFieldSave, "Save", "Persist the changes."},
}

// providerManagerCredsMsg delivers the asynchronously resolved credential
// states for the manager rows. gen guards against a stale probe finishing
// after the manager was reopened with fresh rows.
type providerManagerCredsMsg struct {
	gen   int
	creds map[string]string
}

// openProviderManager opens /provider on the list of saved providers. With
// nothing saved yet there is nothing to manage — fall through to the add
// wizard, which is also the first-run behavior users already know.
func (m model) openProviderManager() (model, tea.Cmd) {
	if len(m.savedProviders) == 0 {
		m.providerWizard = m.newProviderWizard()
		m.clearSuggestions()
		return m, nil
	}
	wizard := &providerWizardState{
		step:      providerWizardStepManage,
		manage:    true,
		providers: providerWizardProviders(),
	}
	wizard.refreshModels()
	m.providerWizard = wizard
	m.clearSuggestions()
	return m.reloadProviderManagerRows()
}

// reloadProviderManagerRows rebuilds the list from savedProviders and kicks off
// the async credential probe for the new generation.
func (m model) reloadProviderManagerRows() (model, tea.Cmd) {
	if m.providerWizard == nil {
		return m, nil
	}
	// Carry resolved credential states forward for names still present, so a
	// single-row mutation doesn't flash every other row back to "checking…"
	// while the async probe re-runs.
	previous := make(map[string]string, len(m.providerWizard.manageRows))
	for _, row := range m.providerWizard.manageRows {
		previous[row.profile.Name] = row.cred
	}
	rows := make([]providerManagerRow, 0, len(m.savedProviders))
	for _, profile := range m.savedProviders {
		row := providerManagerRow{profile: profile, cred: previous[profile.Name]}
		if descriptor, ok := m.descriptorForProfile(profile); ok {
			row.local = descriptor.Local
		}
		rows = append(rows, row)
	}
	m.providerWizard.manageRows = rows
	m.providerWizard.manageCursor = clampInt(m.providerWizard.manageCursor, 0, maxInt(0, len(rows)-1))
	// The session's live provider is the truth the user cares about (config's
	// activeProvider follows it on every switch).
	m.providerWizard.manageActiveName = m.providerName
	m.providerWizard.manageCredGen++
	return m, providerManagerCredsCmd(m.providerWizard.manageCredGen, rows, m.userConfigPath)
}

// providerManagerCredsCmd resolves each row's credential state off the UI
// goroutine: encrypted-store reads (keychain subprocess on macOS), env lookups,
// and the OAuth token store. Keys are read from the store BESIDE configPath —
// the one the manager's write paths use.
func providerManagerCredsCmd(gen int, rows []providerManagerRow, configPath string) tea.Cmd {
	profiles := make([]config.ProviderProfile, len(rows))
	locals := make([]bool, len(rows))
	for i, row := range rows {
		profiles[i] = row.profile
		locals[i] = row.local
	}
	return func() tea.Msg {
		store, storeErr := providerKeyStoreForPath(configPath)
		var getter config.APIKeyGetter
		if storeErr == nil {
			getter = store
		}
		// One token-store snapshot answers every row's OAuth question — a fresh
		// store open + full file read per row (×2 candidates) answered the same
		// question N times over.
		logins := storedOAuthLogins()
		creds := make(map[string]string, len(profiles))
		for i, profile := range profiles {
			creds[profile.Name] = providerManagerCredState(profile, locals[i], getter, logins)
		}
		return providerManagerCredsMsg{gen: gen, creds: creds}
	}
}

// providerManagerCredState classifies how a profile authenticates, surfacing
// broken states ("stored key missing") instead of hiding them — that exact
// state is how a lost keychain entry shows up. A stale APIKeyStored marker
// falls THROUGH to the env/OAuth checks first, matching the runtime's own
// fallback order (profileWithCredential): a provider that will switch fine via
// its env var must not render as broken.
func providerManagerCredState(profile config.ProviderProfile, local bool, store config.APIKeyGetter, logins map[string]bool) string {
	if strings.TrimSpace(profile.APIKey) != "" || strings.TrimSpace(profile.AuthHeaderValue) != "" {
		return "key set"
	}
	if profile.APIKeyStored && store != nil {
		if key, ok, err := store.Get(profile.Name); err == nil && ok && strings.TrimSpace(key) != "" {
			return "key stored"
		}
	}
	if env := strings.TrimSpace(profile.APIKeyEnv); env != "" && strings.TrimSpace(os.Getenv(env)) != "" {
		return "env " + env
	}
	// OAuthLoginCandidates is nil for key-authed profiles, so probe a marker-free
	// copy: a stale marker must not hide a usable login.
	keyless := profile
	keyless.APIKeyStored = false
	for _, candidate := range keyless.OAuthLoginCandidates() {
		// Store keys are normalized to lower case (oauth.ProviderKey).
		if logins[strings.ToLower(strings.TrimSpace(candidate))] {
			return "oauth login"
		}
	}
	if local {
		return "local"
	}
	if profile.APIKeyStored {
		return "stored key missing"
	}
	return "no credential"
}

// storedOAuthLogins snapshots which provider logins exist in the token store —
// the same one-read pattern as cli's oauthLoggedInProviders. Errors degrade to
// an empty set.
func storedOAuthLogins() map[string]bool {
	logins := map[string]bool{}
	store, err := oauth.NewStore(oauth.StoreOptions{})
	if err != nil {
		return logins
	}
	statuses, err := store.Status(oauth.KeyPrefixProvider)
	if err != nil {
		return logins
	}
	for _, status := range statuses {
		if status.HasToken {
			logins[strings.ToLower(strings.TrimPrefix(status.Key, oauth.KeyPrefixProvider))] = true
		}
	}
	return logins
}

func (m model) applyProviderManagerCreds(msg providerManagerCredsMsg) (model, tea.Cmd) {
	if m.providerWizard == nil || !m.providerWizard.manage || msg.gen != m.providerWizard.manageCredGen {
		return m, nil
	}
	for i := range m.providerWizard.manageRows {
		if cred, ok := msg.creds[m.providerWizard.manageRows[i].profile.Name]; ok {
			m.providerWizard.manageRows[i].cred = cred
		}
	}
	return m, nil
}

// providerWizardManagerStep reports whether the wizard is on one of the
// manager-owned steps (list / edit menu / edit value).
func (wizard *providerWizardState) managerStep() bool {
	if wizard == nil {
		return false
	}
	switch wizard.step {
	case providerWizardStepManage, providerWizardStepEditMenu, providerWizardStepEditValue:
		return true
	}
	return false
}

func (wizard *providerWizardState) currentManagerRow() (providerManagerRow, bool) {
	if wizard == nil || len(wizard.manageRows) == 0 {
		return providerManagerRow{}, false
	}
	wizard.manageCursor = clampInt(wizard.manageCursor, 0, len(wizard.manageRows)-1)
	return wizard.manageRows[wizard.manageCursor], true
}

func (m model) handleProviderManagerKey(msg tea.KeyMsg) (model, tea.Cmd) {
	wizard := m.providerWizard
	switch wizard.step {
	case providerWizardStepManage:
		return m.handleProviderManageListKey(msg)
	case providerWizardStepEditMenu:
		return m.handleProviderEditMenuKey(msg)
	case providerWizardStepEditValue:
		return m.handleProviderEditValueKey(msg)
	}
	return m, nil
}

func (m model) handleProviderManageListKey(msg tea.KeyMsg) (model, tea.Cmd) {
	wizard := m.providerWizard
	if wizard.manageDeleting {
		switch {
		case keyIs(msg, tea.KeyEnter) || strings.EqualFold(keyText(msg), "y"):
			return m.deleteManagerSelection()
		case keyIs(msg, tea.KeyEsc) || strings.EqualFold(keyText(msg), "n"):
			wizard.manageDeleting = false
		}
		return m, nil
	}
	switch {
	case keyIs(msg, tea.KeyEsc):
		m.providerWizard = nil
		return m, nil
	case keyIs(msg, tea.KeyUp):
		m.moveProviderManager(-1)
		return m, nil
	case keyIs(msg, tea.KeyDown) || keyIs(msg, tea.KeyTab):
		m.moveProviderManager(1)
		return m, nil
	case keyIs(msg, tea.KeyEnter):
		return m.activateManagerSelection()
	case strings.EqualFold(keyText(msg), "a"):
		// Add: hand over to the normal wizard flow; Esc from its first step
		// returns here (fromManage-style handling in handleProviderWizardKey).
		wizard.step = providerWizardStepMethod
		wizard.err = ""
		wizard.manageStatus = ""
		return m, nil
	case strings.EqualFold(keyText(msg), "e"):
		if row, ok := wizard.currentManagerRow(); ok {
			wizard.beginProviderEdit(row.profile)
		}
		return m, nil
	case strings.EqualFold(keyText(msg), "d"):
		if _, ok := wizard.currentManagerRow(); ok {
			wizard.manageDeleting = true
			wizard.manageStatus = ""
		}
		return m, nil
	}
	return m, nil
}

func (m *model) moveProviderManager(delta int) {
	wizard := m.providerWizard
	count := len(wizard.manageRows)
	if count == 0 {
		wizard.manageCursor = 0
		return
	}
	wizard.manageCursor = ((wizard.manageCursor+delta)%count + count) % count
	wizard.manageStatus = ""
}

// activateManagerSelection makes the selected provider active via the shared
// switch path (persists activeProvider+model, rebuilds the client OAuth-aware,
// exports ZERO_PROVIDER, warms discovery). On success the manager closes and
// the switch notice lands in the transcript; a refusal (busy run, missing
// credential) stays inline so the user keeps their place in the list.
func (m model) activateManagerSelection() (model, tea.Cmd) {
	wizard := m.providerWizard
	row, ok := wizard.currentManagerRow()
	if !ok {
		return m, nil
	}
	if m.pending {
		wizard.manageStatus = "Cannot switch providers while a run is active."
		return m, nil
	}
	next, text, switched, cmd, _ := m.switchProviderModel(row.profile.Name, row.profile.Model)
	if switched {
		next.providerWizard = nil
		next.transcript = reduceTranscript(next.transcript, transcriptAction{kind: actionAppendSystem, text: text})
		return next, cmd
	}
	if next.providerWizard != nil {
		next.providerWizard.manageStatus = strings.TrimPrefix(text, "Model\n")
	}
	return next, cmd
}

// deleteManagerSelection removes the confirmed provider: the config write runs
// synchronously (the list must reflect the config the instant the confirm
// resolves), while the stored-key delete and the OAuth-login lookup — a
// keychain subprocess and a token-store read — run in a follow-up tea.Cmd so
// the confirm keypress never stalls the render loop. The OAuth token is
// deliberately kept — logins outlive profiles so re-adding the provider
// doesn't force a browser round-trip; zero auth logout removes it.
func (m model) deleteManagerSelection() (model, tea.Cmd) {
	wizard := m.providerWizard
	wizard.manageDeleting = false
	row, ok := wizard.currentManagerRow()
	if !ok {
		return m, nil
	}
	name := row.profile.Name
	if strings.TrimSpace(m.userConfigPath) == "" {
		wizard.manageStatus = "No user config path — cannot delete."
		return m, nil
	}

	persisted, err := config.ProviderPersisted(m.userConfigPath, name)
	if err != nil {
		wizard.manageStatus = "Delete failed: " + err.Error()
		return m, nil
	}
	var notes []string
	var activeAfter string
	var cleanup tea.Cmd
	if persisted {
		cfg, err := config.RemoveProvider(m.userConfigPath, name)
		if err != nil {
			wizard.manageStatus = "Delete failed: " + err.Error()
			return m, nil
		}
		activeAfter = cfg.ActiveProvider
		notes = []string{"Deleted " + name + "."}
		cleanup = providerManagerCleanupCmd(m.userConfigPath, row.profile)
	} else {
		// Env-derived providers have no persisted profile or credential to
		// delete. Keep this path session-only.
		notes = []string{
			"Removed " + name + " from this session.",
			"It wasn't saved in config.json (likely set via an environment variable) — unset it to stop Zero from detecting it automatically.",
		}
	}

	// Surgical removal — see saveManagerEdit for why the raw cfg.Providers list
	// must not replace the resolved/filtered savedProviders wholesale.
	m.savedProviders = removeSavedProvider(m.savedProviders, name)

	if strings.EqualFold(strings.TrimSpace(m.providerName), strings.TrimSpace(name)) {
		notes = append(notes, "This session keeps running on it until you switch.")
	} else if activeAfter != "" && !strings.EqualFold(activeAfter, name) {
		notes = append(notes, "Active provider: "+activeAfter+".")
	}

	if len(m.savedProviders) == 0 {
		m.providerWizard = nil
		m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendSystem, text: "Provider\n" + strings.Join(notes, " ")})
		return m, cleanup
	}
	next, cmd := m.reloadProviderManagerRows()
	next.providerWizard.manageStatus = strings.Join(notes, " ")
	return next, tea.Batch(cmd, cleanup)
}

// removeSavedProvider drops one profile from the in-memory saved list.
func removeSavedProvider(saved []config.ProviderProfile, name string) []config.ProviderProfile {
	kept := saved[:0]
	for _, profile := range saved {
		if strings.EqualFold(strings.TrimSpace(profile.Name), strings.TrimSpace(name)) {
			continue
		}
		kept = append(kept, profile)
	}
	return kept
}

// providerManagerCleanupMsg reports the off-thread half of a delete: the
// stored-key removal outcome and the OAuth-login hint.
type providerManagerCleanupMsg struct {
	notes []string
}

// providerManagerCleanupCmd finishes a delete off the UI goroutine: the
// keychain delete shells out to `security` on macOS and the OAuth-login lookup
// reads the token store — blocking work the confirm keypress must not wait on.
// A failed key delete is surfaced rather than letting a lingering secret read
// as a clean removal.
func providerManagerCleanupCmd(configPath string, profile config.ProviderProfile) tea.Cmd {
	name := profile.Name
	catalogID := profile.CatalogID
	return func() tea.Msg {
		notes := []string{}
		keyStore, storeErr := providerKeyStoreForPath(configPath)
		if storeErr == nil {
			_, storeErr = keyStore.Delete(name)
		}
		if storeErr != nil {
			notes = append(notes, "Warning: its stored API key could not be deleted ("+storeErr.Error()+").")
		}
		if login, ok := oauthLoginName(config.ProviderProfile{Name: name, CatalogID: catalogID}); ok {
			notes = append(notes, "OAuth login kept — remove with `zero auth logout "+login+"`.")
		}
		return providerManagerCleanupMsg{notes: notes}
	}
}

func (m model) applyProviderManagerCleanup(msg providerManagerCleanupMsg) (model, tea.Cmd) {
	if len(msg.notes) == 0 {
		return m, nil
	}
	extra := strings.Join(msg.notes, " ")
	if m.providerWizard != nil && m.providerWizard.manage {
		if m.providerWizard.manageStatus != "" {
			m.providerWizard.manageStatus += " " + extra
		} else {
			m.providerWizard.manageStatus = extra
		}
		return m, nil
	}
	m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendSystem, text: "Provider\n" + extra})
	return m, nil
}

// --- edit -------------------------------------------------------------------

func (wizard *providerWizardState) beginProviderEdit(profile config.ProviderProfile) {
	wizard.editOriginal = profile
	wizard.editDraft = profile
	wizard.editDraft.APIKey = "" // key field is enter-to-replace, never prefilled
	wizard.editCursor = 0
	wizard.err = ""
	wizard.manageStatus = ""
	wizard.step = providerWizardStepEditMenu
}

func (wizard *providerWizardState) editFieldValue(field providerEditField, draft config.ProviderProfile) string {
	switch field {
	case providerEditFieldName:
		return draft.Name
	case providerEditFieldEndpoint:
		return draft.BaseURL
	case providerEditFieldModel:
		return draft.Model
	case providerEditFieldAPIKey:
		return draft.APIKey
	case providerEditFieldDescription:
		return draft.Description
	}
	return ""
}

func (m model) handleProviderEditMenuKey(msg tea.KeyMsg) (model, tea.Cmd) {
	wizard := m.providerWizard
	switch {
	case keyIs(msg, tea.KeyEsc) || keyIs(msg, tea.KeyLeft):
		wizard.step = providerWizardStepManage
		wizard.err = ""
		return m, nil
	case keyIs(msg, tea.KeyUp):
		wizard.editCursor = ((wizard.editCursor-1)%len(providerEditFields) + len(providerEditFields)) % len(providerEditFields)
		return m, nil
	case keyIs(msg, tea.KeyDown) || keyIs(msg, tea.KeyTab):
		wizard.editCursor = (wizard.editCursor + 1) % len(providerEditFields)
		return m, nil
	case keyIs(msg, tea.KeyEnter):
		entry := providerEditFields[clampInt(wizard.editCursor, 0, len(providerEditFields)-1)]
		if entry.field == providerEditFieldSave {
			return m.saveManagerEdit()
		}
		wizard.editField = entry.field
		wizard.editBuffer = wizard.editFieldValue(entry.field, wizard.editDraft)
		wizard.err = ""
		wizard.step = providerWizardStepEditValue
		return m, nil
	}
	return m, nil
}

func (m model) handleProviderEditValueKey(msg tea.KeyMsg) (model, tea.Cmd) {
	wizard := m.providerWizard
	switch {
	case keyIs(msg, tea.KeyEsc):
		wizard.step = providerWizardStepEditMenu
		wizard.err = ""
		return m, nil
	case keyIs(msg, tea.KeyEnter):
		if err := wizard.commitEditBuffer(); err != "" {
			wizard.err = err
			return m, nil
		}
		wizard.err = ""
		wizard.step = providerWizardStepEditMenu
		return m, nil
	case keyCtrl(msg, 'u'):
		wizard.editBuffer = ""
		wizard.err = ""
		return m, nil
	case keyBackspace(msg):
		if wizard.editBuffer != "" {
			runes := []rune(wizard.editBuffer)
			wizard.editBuffer = string(runes[:len(runes)-1])
		}
		wizard.err = ""
		return m, nil
	case keyText(msg) != "":
		for _, r := range keyRunes(msg) {
			if r == '\n' || r == '\r' || r == '\t' {
				continue
			}
			wizard.editBuffer += string(r)
		}
		wizard.err = ""
		return m, nil
	}
	return m, nil
}

// commitEditBuffer validates and folds the edited value into the draft.
// Returns a non-empty error string to keep the user on the field.
func (wizard *providerWizardState) commitEditBuffer() string {
	value := strings.TrimSpace(wizard.editBuffer)
	switch wizard.editField {
	case providerEditFieldName:
		if value == "" {
			return "name cannot be empty"
		}
		wizard.editDraft.Name = value
	case providerEditFieldEndpoint:
		if err := providerWizardEndpointError(value); err != "" {
			return err
		}
		wizard.editDraft.BaseURL = value
	case providerEditFieldModel:
		if value == "" {
			return "model cannot be empty"
		}
		wizard.editDraft.Model = value
	case providerEditFieldAPIKey:
		wizard.editDraft.APIKey = strings.TrimSpace(wizard.editBuffer)
	case providerEditFieldDescription:
		wizard.editDraft.Description = value
	}
	return ""
}

// saveManagerEdit persists the draft in ONE atomic config write
// (config.EditProvider handles rename — case-only included — field merge,
// verbatim description, the stored-key marker, and key migration together, so
// a failure leaves nothing half-applied). A freshly entered key is captured
// into the encrypted store under the CURRENT name first (EditProvider's rename
// migration then moves it), so config.json never holds it in cleartext. The
// live session follows a rename so ZERO_PROVIDER and the status line never
// point at a name that no longer exists.
func (m model) saveManagerEdit() (model, tea.Cmd) {
	wizard := m.providerWizard
	if strings.TrimSpace(m.userConfigPath) == "" {
		wizard.err = "no user config path — cannot save"
		return m, nil
	}
	oldName := strings.TrimSpace(wizard.editOriginal.Name)
	persisted, err := config.ProviderPersisted(m.userConfigPath, oldName)
	if err != nil {
		wizard.err = err.Error()
		return m, nil
	}
	if !persisted {
		wizard.err = "provider " + oldName + " is not saved in config.json, so there is no saved profile to edit"
		return m, nil
	}
	newName := strings.TrimSpace(wizard.editDraft.Name)
	if newName == "" {
		wizard.err = "name cannot be empty"
		return m, nil
	}
	edit := config.ProviderEdit{
		Name:        oldName,
		NewName:     newName,
		BaseURL:     strings.TrimSpace(wizard.editDraft.BaseURL),
		Model:       strings.TrimSpace(wizard.editDraft.Model),
		Description: wizard.editDraft.Description,
	}
	if key := strings.TrimSpace(wizard.editDraft.APIKey); key != "" {
		captured := config.SecureProviderProfile(config.ProviderProfile{Name: oldName, APIKey: key}, m.userConfigPath)
		// On a store failure SecureProviderProfile keeps the inline key, which
		// EditProvider then persists (the startup migration re-captures later) —
		// the same fail-soft posture as every other capture path.
		edit.APIKey = captured.APIKey
		edit.APIKeyStored = captured.APIKeyStored
	}
	if _, err := config.EditProvider(m.userConfigPath, edit); err != nil {
		wizard.err = err.Error()
		return m, nil
	}
	// Mirror the edit into the in-memory list surgically. savedProviders was
	// seeded from the RESOLVED (project-config layered) and usability-FILTERED
	// provider set — substituting the raw user-file list here would drop
	// project-contributed providers and resurrect filtered stubs for the session.
	m.savedProviders = applySavedProviderEdit(m.savedProviders, oldName, edit)

	// Keep the live session's identity in sync with a rename of the provider it
	// is running on: the exported ZERO_PROVIDER must resolve for spawned children.
	if strings.EqualFold(strings.TrimSpace(m.providerName), oldName) {
		m.providerName = newName
		m.providerProfile.Name = newName
		config.SetActiveProviderEnv(newName)
	}

	wizard.step = providerWizardStepManage
	next, cmd := m.reloadProviderManagerRows()
	next.providerWizard.manageStatus = "Updated " + newName + "." + providerEditRestartNote(next.providerName, newName)
	return next, cmd
}

// providerEditRestartNote reminds the user when the edited provider is the one
// this session is running on — endpoint/model/key changes only apply to the
// built client after a switch (Enter on the row re-activates and rebuilds).
// liveName is the session's provider AFTER any rename sync, so a single
// comparison against the edited profile's final name suffices.
func providerEditRestartNote(liveName string, editedName string) string {
	if strings.EqualFold(strings.TrimSpace(liveName), strings.TrimSpace(editedName)) {
		return " Press Enter on it to apply the changes to this session."
	}
	return ""
}

// applySavedProviderEdit mirrors a persisted config.EditProvider into the
// in-memory saved list without wholesale replacement (see saveManagerEdit).
func applySavedProviderEdit(saved []config.ProviderProfile, oldName string, edit config.ProviderEdit) []config.ProviderProfile {
	for index := range saved {
		if !strings.EqualFold(strings.TrimSpace(saved[index].Name), strings.TrimSpace(oldName)) {
			continue
		}
		profile := &saved[index]
		if newName := strings.TrimSpace(edit.NewName); newName != "" {
			profile.Name = newName
		}
		if baseURL := strings.TrimSpace(edit.BaseURL); baseURL != "" {
			profile.BaseURL = baseURL
		}
		if model := strings.TrimSpace(edit.Model); model != "" {
			profile.Model = model
		}
		if apiKey := strings.TrimSpace(edit.APIKey); apiKey != "" {
			profile.APIKey = apiKey
		}
		if edit.APIKeyStored {
			profile.APIKeyStored = true
			profile.APIKey = ""
		}
		profile.Description = strings.TrimSpace(edit.Description)
		break
	}
	return saved
}

// --- rendering ---------------------------------------------------------------

// upsertSavedProviderProfile mirrors a freshly persisted profile into the
// in-memory saved list (replace by name, else append).
func upsertSavedProviderProfile(saved []config.ProviderProfile, profile config.ProviderProfile) []config.ProviderProfile {
	for index := range saved {
		if strings.EqualFold(strings.TrimSpace(saved[index].Name), strings.TrimSpace(profile.Name)) {
			saved[index] = profile
			return saved
		}
	}
	return append(saved, profile)
}

func (wizard *providerWizardState) renderManageStep(width int) []string {
	rows := wizard.manageRows
	lines := []string{}
	if wizard.manageStatus != "" {
		lines = append(lines, fitStyledLine(zeroTheme.accent.Render(wizard.manageStatus), width), "")
	}
	if len(rows) == 0 {
		lines = append(lines, zeroTheme.faint.Render("  No providers saved — press a to add one."))
		return lines
	}
	wizard.manageCursor = clampInt(wizard.manageCursor, 0, len(rows)-1)
	nameWidth := 0
	for _, row := range rows {
		nameWidth = maxInt(nameWidth, len([]rune(row.profile.Name)))
	}
	nameWidth = minInt(nameWidth, 28)

	maxVisible := minInt(providerManagerMaxVisible, len(rows))
	start := selectableListStart(len(rows), maxVisible, wizard.manageCursor)
	for offset, row := range rows[start : start+maxVisible] {
		index := start + offset
		surface := transparentSurface
		marker := surface(zeroTheme.faintest).Render("  ")
		if index == wizard.manageCursor {
			surface = zeroTheme.onSel
			marker = surface(zeroTheme.accent).Render("❯ ")
		}
		active := ""
		if strings.EqualFold(strings.TrimSpace(row.profile.Name), strings.TrimSpace(wizard.manageActiveName)) {
			active = surface(zeroTheme.accent).Render(" ● active")
		}
		name := padProviderManagerCell(row.profile.Name, nameWidth)
		meta := providerManagerRowMeta(row.profile)
		left := marker + surface(zeroTheme.ink).Render(name) + "  " + surface(zeroTheme.faint).Render(meta)
		right := surface(zeroTheme.faint).Render(providerManagerCredDisplay(row)) + active
		gap := width - lipgloss.Width(left) - lipgloss.Width(right)
		line := left + surface(zeroTheme.ink).Render(strings.Repeat(" ", maxInt(1, gap))) + right
		lines = append(lines, fillPaletteLine(line, width, surface))
	}

	if row, ok := wizard.currentManagerRow(); ok {
		lines = append(lines, zeroTheme.line.Render(strings.Repeat("─", width)))
		detail := strings.TrimSpace(row.profile.BaseURL)
		if description := strings.TrimSpace(row.profile.Description); description != "" {
			if detail != "" {
				detail += " — "
			}
			detail += description
		}
		if detail == "" {
			detail = "(no endpoint)"
		}
		lines = append(lines, fitStyledLine(zeroTheme.faint.Render(detail), width))
		if wizard.manageDeleting {
			lines = append(lines, fitStyledLine(zeroTheme.red.Render("Delete "+row.profile.Name+"? This also removes its stored API key.  Enter/y confirm · Esc/n cancel"), width))
		}
	}
	return lines
}

// manageActiveName is resolved at render time via the field set in
// reloadProviderManagerRows — kept on the struct so the pure render funcs
// don't need the model.
func providerManagerRowMeta(profile config.ProviderProfile) string {
	kind := strings.TrimSpace(string(profile.ProviderKind))
	if kind == "" {
		kind = strings.TrimSpace(profile.Provider)
	}
	if catalog := strings.TrimSpace(profile.CatalogID); catalog != "" && !strings.EqualFold(catalog, profile.Name) {
		kind = catalog
	}
	model := strings.TrimSpace(profile.Model)
	switch {
	case kind != "" && model != "":
		return kind + " · " + model
	case kind != "":
		return kind
	default:
		return model
	}
}

func providerManagerCredDisplay(row providerManagerRow) string {
	if row.cred == "" {
		return "checking…"
	}
	return row.cred
}

func padProviderManagerCell(value string, width int) string {
	runes := []rune(value)
	if len(runes) > width {
		if width <= 1 {
			return string(runes[:width])
		}
		return string(runes[:width-1]) + "…"
	}
	return value + strings.Repeat(" ", width-len(runes))
}

func (wizard *providerWizardState) renderEditMenuStep(width int) []string {
	lines := []string{zeroTheme.accent.Render("Edit " + wizard.editOriginal.Name)}
	wizard.editCursor = clampInt(wizard.editCursor, 0, len(providerEditFields)-1)
	for index, entry := range providerEditFields {
		surface := transparentSurface
		marker := surface(zeroTheme.faintest).Render("  ")
		if index == wizard.editCursor {
			surface = zeroTheme.onSel
			marker = surface(zeroTheme.accent).Render("❯ ")
		}
		value := wizard.editFieldValue(entry.field, wizard.editDraft)
		display := ""
		switch entry.field {
		case providerEditFieldAPIKey:
			if value != "" {
				display = maskedProviderWizardKey(value) + " (new)"
			} else if wizard.editOriginal.APIKeyStored {
				display = "stored — enter to replace"
			} else {
				display = "none — enter to add"
			}
		case providerEditFieldSave:
			display = ""
		default:
			display = displayValue(value, "(empty)")
		}
		left := marker + surface(zeroTheme.ink).Render(padProviderManagerCell(entry.label, 12))
		if display != "" {
			left += surface(zeroTheme.faint).Render(display)
		}
		lines = append(lines, fillPaletteLine(fitStyledLine(left, width), width, surface))
	}
	entry := providerEditFields[wizard.editCursor]
	lines = append(lines, "", fitStyledLine(zeroTheme.faint.Render(entry.hint), width))
	return lines
}

func (wizard *providerWizardState) renderEditValueStep(width int) []string {
	entry := providerEditFields[clampInt(wizard.editCursor, 0, len(providerEditFields)-1)]
	value := wizard.editBuffer
	if wizard.editField == providerEditFieldAPIKey {
		value = maskedProviderWizardKey(value)
	}
	prompt := zeroTheme.userPrompt.Render(entry.label + " > ")
	if value == "" {
		value = zeroTheme.faint.Render("(empty)")
	} else {
		value = zeroTheme.ink.Render(value)
	}
	return []string{
		zeroTheme.accent.Render("Edit " + wizard.editOriginal.Name + " — " + entry.label),
		fitStyledLine(prompt+value, width),
		"",
		fitStyledLine(zeroTheme.faint.Render(entry.hint), width),
	}
}
