package tui

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Gitlawb/zero/internal/config"
	"github.com/Gitlawb/zero/internal/oauth"
	"github.com/Gitlawb/zero/internal/providercatalog"
)

// wizardModelAt builds a model whose provider wizard is at step with providerID
// selected.
func wizardModelAt(t *testing.T, providerID string, step providerWizardStep) model {
	t.Helper()
	m := mouseTestModel()
	w := m.newProviderWizard()
	idx := -1
	for i, d := range w.providers {
		if d.ID == providerID {
			idx = i
			break
		}
	}
	if idx < 0 {
		t.Fatalf("provider %q not offered by the wizard", providerID)
	}
	w.selectedProvider = idx
	w.step = step
	m.providerWizard = w
	return m
}

func TestProviderWizardMethodChooserOAuthPath(t *testing.T) {
	m := mouseTestModel()
	m.providerWizard = m.newProviderWizard()
	if m.providerWizard.step != providerWizardStepMethod {
		t.Fatalf("wizard should open on the method chooser, got %v", m.providerWizard.step)
	}
	m.providerWizard.selectedMethod = 0 // "Sign in with OAuth" (default, first)
	next, _ := m.advanceProviderWizard()
	w := next.providerWizard
	if w.step != providerWizardStepProvider || !w.oauthMode {
		t.Fatalf("OAuth method should enter the provider step in oauthMode, got step=%v oauth=%v", w.step, w.oauthMode)
	}
	if len(w.providers) != len(providercatalog.OAuthProviders()) {
		t.Fatalf("OAuth path should list only OAuth providers, got %d", len(w.providers))
	}
	for _, d := range w.providers {
		if !d.OAuth {
			t.Fatalf("non-OAuth provider %q in the OAuth list", d.ID)
		}
	}
}

func TestProviderWizardMethodChooserBrowsePath(t *testing.T) {
	m := mouseTestModel()
	m.providerWizard = m.newProviderWizard()
	m.providerWizard.selectedMethod = len(providerWizardMethodOptions()) - 1 // "browse / API key"
	next, _ := m.advanceProviderWizard()
	w := next.providerWizard
	if w.step != providerWizardStepProvider || w.oauthMode {
		t.Fatalf("browse method should enter the provider step (not oauthMode), got step=%v oauth=%v", w.step, w.oauthMode)
	}
	if len(w.providers) <= len(providercatalog.OAuthProviders()) {
		t.Fatalf("browse path should list the full catalog, got %d", len(w.providers))
	}
}

func selectWizardOAuthProvider(t *testing.T, m model, id string) model {
	t.Helper()
	for i, d := range m.providerWizard.providers {
		if d.ID == id {
			m.providerWizard.selectedProvider = i
			return m
		}
	}
	t.Fatalf("provider %q not in the OAuth list", id)
	return m
}

func beginTestOAuthAttempt(wizard *providerWizardState, device bool) (string, int) {
	providerID := wizard.currentProvider().ID
	return providerID, wizard.beginOAuthAttempt(device)
}

func TestProviderWizardDeviceShortcutStartsDeviceFlow(t *testing.T) {
	m := mouseTestModel()
	m.providerWizard = m.newProviderWizard()
	m.providerWizard.selectedMethod = 0
	next, _ := m.advanceProviderWizard() // → OAuth list
	m = selectWizardOAuthProvider(t, next, "xai")

	out, cmd := m.handleProviderWizardKey(testKeyText("d"))
	if !out.providerWizard.oauthPending || !out.providerWizard.oauthDevice {
		t.Fatalf("'d' should start device login (pending=%v device=%v)", out.providerWizard.oauthPending, out.providerWizard.oauthDevice)
	}
	if out.providerWizard.oauthAttemptID == 0 {
		t.Fatal("'d' should assign an OAuth attempt id")
	}
	if cmd == nil {
		t.Fatal("'d' should return the device-prepare command")
	}
}

func TestProviderWizardDeviceCodeMsgShowsCodeAndPolls(t *testing.T) {
	m := mouseTestModel()
	m.providerWizard = m.newProviderWizard()
	m.providerWizard.selectedMethod = 0
	next, _ := m.advanceProviderWizard()
	m = selectWizardOAuthProvider(t, next, "xai")
	providerID, attemptID := beginTestOAuthAttempt(m.providerWizard, true)

	out, cmd := m.applyProviderWizardDeviceCode(providerWizardDeviceCodeMsg{
		providerID: providerID, attemptID: attemptID, userCode: "ABCD-1234", verifyURL: "https://x.ai/device",
	})
	if out.providerWizard.deviceUserCode != "ABCD-1234" || out.providerWizard.deviceVerificationURI != "https://x.ai/device" {
		t.Fatalf("device code not stored: %+v", out.providerWizard)
	}
	if cmd == nil {
		t.Fatal("device-code msg should start the poll command")
	}
	view := strings.Join(out.providerWizard.renderOAuthWaiting(72), "\n")
	if !strings.Contains(view, "ABCD-1234") || !strings.Contains(view, "x.ai/device") {
		t.Fatalf("waiting render missing device code/uri:\n%s", view)
	}
}

// A failed OAuth attempt leaves the wizard on the provider list; the error must be
// rendered there (not just on the credential step) so a click isn't a silent
// no-op, and Hugging Face gets an actionable client_id hint.
func TestProviderStepSurfacesOAuthError(t *testing.T) {
	m := mouseTestModel()
	m.providerWizard = m.newProviderWizard()
	wizard := m.providerWizard
	wizard.oauthMode = true
	wizard.providers = providerWizardOAuthDescriptors()
	found := false
	for i, p := range wizard.providers {
		if strings.EqualFold(p.ID, "huggingface") {
			wizard.selectedProvider = i
			found = true
		}
	}
	if !found {
		t.Fatal("huggingface should be an OAuth-capable provider")
	}
	wizard.oauthErr = `oauth: provider "huggingface" is not configured; set ZERO_OAUTH_HUGGINGFACE_CLIENT_ID`

	view := strings.Join(wizard.renderProviderStep(72), "\n")
	if !strings.Contains(view, "OAuth login failed") {
		t.Fatalf("provider step should surface the OAuth error:\n%s", view)
	}
	if !strings.Contains(view, "huggingface.co/settings/applications/new") {
		t.Fatalf("Hugging Face hint should point at app registration:\n%s", view)
	}
}

func TestProviderWizardDeviceErrorSurfaced(t *testing.T) {
	m := mouseTestModel()
	m.providerWizard = m.newProviderWizard()
	providerID, attemptID := beginTestOAuthAttempt(m.providerWizard, true)

	out, cmd := m.applyProviderWizardDeviceCode(providerWizardDeviceCodeMsg{
		providerID: providerID, attemptID: attemptID, err: errors.New("device endpoint unreachable"),
	})
	if out.providerWizard.oauthPending || out.providerWizard.oauthDevice {
		t.Fatal("device error should clear pending/device state")
	}
	if out.providerWizard.oauthErr == "" {
		t.Fatal("device error should surface a message")
	}
	if cmd != nil {
		t.Fatal("device error should not start a poll")
	}
}

func TestProviderWizardOAuthSuccessClearsDeviceState(t *testing.T) {
	m := mouseTestModel()
	m.providerWizard = m.newProviderWizard()
	providerID, attemptID := beginTestOAuthAttempt(m.providerWizard, true)
	m.providerWizard.deviceUserCode = "X-1"
	m.providerWizard.deviceVerificationURI = "https://x.ai/device"

	out, _ := m.applyProviderWizardOAuth(providerWizardOAuthMsg{providerID: providerID, attemptID: attemptID, tokenLogin: true})
	if out.providerWizard.oauthDevice || out.providerWizard.deviceUserCode != "" || out.providerWizard.deviceVerificationURI != "" {
		t.Fatalf("success should clear device state: %+v", out.providerWizard)
	}
	if out.providerWizard.step != providerWizardStepModel {
		t.Fatalf("success should advance to the model step, got %v", out.providerWizard.step)
	}
}

func TestProviderWizardOAuthDispatchFromList(t *testing.T) {
	m := mouseTestModel()
	m.providerWizard = m.newProviderWizard()
	m.providerWizard.selectedMethod = 0
	next, _ := m.advanceProviderWizard() // → OAuth provider list
	// select openrouter
	found := false
	for i, d := range next.providerWizard.providers {
		if d.ID == "openrouter" {
			next.providerWizard.selectedProvider = i
			found = true
			break
		}
	}
	if !found {
		t.Fatal("openrouter not present in the OAuth provider list")
	}
	next, cmd := next.advanceProviderWizard()
	if !next.providerWizard.oauthPending {
		t.Fatal("advancing from the OAuth list should start the login (oauthPending)")
	}
	if next.providerWizard.oauthAttemptID == 0 {
		t.Fatal("advancing from the OAuth list should assign an OAuth attempt id")
	}
	if cmd == nil {
		t.Fatal("advancing from the OAuth list should return the OAuth command")
	}
}

func TestProviderWizardRetreatFromProviderToMethod(t *testing.T) {
	m := mouseTestModel()
	m.providerWizard = m.newProviderWizard()
	m.providerWizard.selectedMethod = 0
	next, _ := m.advanceProviderWizard() // → OAuth provider list (oauthMode)
	next.providerWizard.retreat()
	if next.providerWizard.step != providerWizardStepMethod {
		t.Fatalf("retreat from provider should return to method, got %v", next.providerWizard.step)
	}
	if next.providerWizard.oauthMode {
		t.Fatal("retreat to method should clear oauthMode")
	}
}

func TestProviderWizardSupportsOAuth(t *testing.T) {
	or, _ := providercatalog.Get("openrouter")
	if !providerWizardSupportsOAuth(or) {
		t.Fatal("openrouter should offer in-wizard OAuth")
	}
	oa, _ := providercatalog.Get("openai")
	if providerWizardSupportsOAuth(oa) {
		t.Fatal("openai should NOT offer in-wizard OAuth (no usable direct OAuth)")
	}
}

func TestProviderWizardChatGPTOAuthModelsUseCodexSet(t *testing.T) {
	chatgpt, ok := providercatalog.Get("chatgpt")
	if !ok {
		t.Fatal("chatgpt provider missing from catalog")
	}
	models := providerWizardModelOptions(chatgpt)
	got := map[string]bool{}
	for _, model := range models {
		got[model.ID] = true
	}
	for _, want := range []string{"gpt-5.5", "gpt-5.4", "gpt-5.4-mini", "gpt-5.3-codex-spark"} {
		if !got[want] {
			t.Fatalf("ChatGPT OAuth models missing %q; got %#v", want, providerWizardModelIDs(models))
		}
	}
	if got["gpt-5"] {
		t.Fatalf("ChatGPT OAuth models should not include stale gpt-5 fallback; got %#v", providerWizardModelIDs(models))
	}
	if models[0].ID != "gpt-5.5" {
		t.Fatalf("default ChatGPT OAuth model = %q, want gpt-5.5", models[0].ID)
	}
}

func TestProviderWizardCtrlOStartsOAuthForOpenRouter(t *testing.T) {
	m := wizardModelAt(t, "openrouter", providerWizardStepCredential)
	next, cmd := m.handleProviderWizardKey(testKeyCtrl('o'))
	if next.providerWizard == nil || !next.providerWizard.oauthPending {
		t.Fatal("ctrl+o should mark the wizard oauthPending")
	}
	if next.providerWizard.oauthAttemptID == 0 {
		t.Fatal("ctrl+o should assign an OAuth attempt id")
	}
	if cmd == nil {
		t.Fatal("ctrl+o should return a command to run the OAuth flow")
	}
}

func TestProviderWizardCtrlONoopForNonOAuthProvider(t *testing.T) {
	m := wizardModelAt(t, "openai", providerWizardStepCredential)
	next, _ := m.handleProviderWizardKey(testKeyCtrl('o'))
	if next.providerWizard != nil && next.providerWizard.oauthPending {
		t.Fatal("ctrl+o must not start OAuth for a provider that doesn't support it")
	}
}

func TestApplyProviderWizardOAuthSuccessAdvances(t *testing.T) {
	m := wizardModelAt(t, "openrouter", providerWizardStepCredential)
	providerID, attemptID := beginTestOAuthAttempt(m.providerWizard, false)
	next, _ := m.applyProviderWizardOAuth(providerWizardOAuthMsg{providerID: providerID, attemptID: attemptID, apiKey: "sk-or-minted"})
	if next.providerWizard == nil {
		t.Fatal("wizard should remain open")
	}
	if next.providerWizard.oauthPending {
		t.Fatal("pending should clear on success")
	}
	if next.providerWizard.apiKey != "sk-or-minted" {
		t.Fatalf("minted key not applied: %q", next.providerWizard.apiKey)
	}
	if next.providerWizard.step != providerWizardStepModel {
		t.Fatalf("should advance to the model step, got %v", next.providerWizard.step)
	}
}

func TestApplyProviderWizardOAuthErrorStays(t *testing.T) {
	m := wizardModelAt(t, "openrouter", providerWizardStepCredential)
	providerID, attemptID := beginTestOAuthAttempt(m.providerWizard, false)
	next, _ := m.applyProviderWizardOAuth(providerWizardOAuthMsg{providerID: providerID, attemptID: attemptID, err: errors.New("nope")})
	if next.providerWizard == nil {
		t.Fatal("wizard should remain open on error")
	}
	if next.providerWizard.oauthPending {
		t.Fatal("pending should clear on error")
	}
	if next.providerWizard.step != providerWizardStepCredential {
		t.Fatalf("should stay at credential step, got %v", next.providerWizard.step)
	}
	if next.providerWizard.oauthErr == "" {
		t.Fatal("oauthErr should be set")
	}
}

func TestRenderCredentialStepShowsOAuthHintAndPending(t *testing.T) {
	m := wizardModelAt(t, "openrouter", providerWizardStepCredential)
	w := m.providerWizard
	if !strings.Contains(strings.Join(w.renderCredentialStep(80), "\n"), "ctrl+o") {
		t.Fatal("credential step should show the ctrl+o OAuth hint for openrouter")
	}
	w.oauthPending = true
	if !strings.Contains(strings.Join(w.renderOAuthWaiting(80), "\n"), "Waiting for authorization") {
		t.Fatal("pending state should show the browser-waiting message")
	}
}

func TestApplyProviderWizardOAuthIgnoresStaleAttempt(t *testing.T) {
	m := wizardModelAt(t, "openrouter", providerWizardStepCredential)
	providerID, attemptID := beginTestOAuthAttempt(m.providerWizard, false)

	next, cmd := m.applyProviderWizardOAuth(providerWizardOAuthMsg{
		providerID: providerID,
		attemptID:  attemptID - 1,
		apiKey:     "sk-or-stale",
	})
	if cmd != nil {
		t.Fatal("stale OAuth result should not start a command")
	}
	if !next.providerWizard.oauthPending {
		t.Fatal("stale OAuth result should leave the active attempt pending")
	}
	if next.providerWizard.apiKey != "" {
		t.Fatalf("stale OAuth result applied api key %q", next.providerWizard.apiKey)
	}
	if next.providerWizard.step != providerWizardStepCredential {
		t.Fatalf("stale OAuth result moved step to %v", next.providerWizard.step)
	}
}

func TestProviderWizardDeviceCodeIgnoresStaleAttempt(t *testing.T) {
	m := mouseTestModel()
	m.providerWizard = m.newProviderWizard()
	m.providerWizard.selectedMethod = 0
	next, _ := m.advanceProviderWizard()
	m = selectWizardOAuthProvider(t, next, "xai")
	providerID, attemptID := beginTestOAuthAttempt(m.providerWizard, true)

	out, cmd := m.applyProviderWizardDeviceCode(providerWizardDeviceCodeMsg{
		providerID: providerID,
		attemptID:  attemptID - 1,
		userCode:   "STALE",
		verifyURL:  "https://x.ai/device",
	})
	if cmd != nil {
		t.Fatal("stale device-code result should not start polling")
	}
	if !out.providerWizard.oauthPending {
		t.Fatal("stale device-code result should leave the active attempt pending")
	}
	if out.providerWizard.deviceUserCode != "" || out.providerWizard.deviceVerificationURI != "" {
		t.Fatalf("stale device-code result applied device details: %+v", out.providerWizard)
	}
}

func TestPersistOAuthLoginProviderWritesKeylessProfileWithoutStealingActive(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	seed := `{"activeProvider":"opengateway","providers":[{"name":"opengateway","provider_kind":"openai-compatible","baseURL":"https://gateway.example.com/v1","apiKeyStored":true,"model":"some-model"}]}`
	if err := os.WriteFile(configPath, []byte(seed), 0o600); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	persistOAuthLoginProvider(configPath, "chatgpt")

	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var cfg config.FileConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("decode config: %v", err)
	}
	if cfg.ActiveProvider != "opengateway" {
		t.Fatalf("active provider changed to %q", cfg.ActiveProvider)
	}
	if len(cfg.Providers) != 2 || cfg.Providers[1].CatalogID != "chatgpt" {
		t.Fatalf("expected chatgpt profile appended, got %+v", cfg.Providers)
	}
	if cfg.Providers[1].APIKey != "" || cfg.Providers[1].APIKeyStored {
		t.Fatalf("OAuth profile must stay keyless so the token resolver attaches, got %+v", cfg.Providers[1])
	}

	// Blank path (no user config, e.g. tests/ephemeral runs) must be a no-op.
	persistOAuthLoginProvider("", "chatgpt")
}

func TestOAuthCommandsRejectInvalidConfigBeforeCredentialSideEffects(t *testing.T) {
	t.Setenv("ZERO_OAUTH_STORAGE", "file")
	t.Setenv("ZERO_OAUTH_TOKENS_PATH", filepath.Join(t.TempDir(), "oauth.json"))
	configPath := filepath.Join(t.TempDir(), "config.json")
	seed := []byte(`{"providers":[{"name":"xai"},{"name":"XAI"}]}`)
	if err := os.WriteFile(configPath, seed, 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := oauth.NewStore(oauth.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	previous := oauth.Token{AccessToken: "previous-access", RefreshToken: "previous-refresh"}
	if err := store.Save(oauth.ProviderKey("xai"), previous); err != nil {
		t.Fatal(err)
	}

	for _, providerID := range []string{"openrouter", "chatgpt", "xai"} {
		descriptor, ok := providercatalog.Get(providerID)
		if !ok {
			t.Fatalf("missing catalog provider %q", providerID)
		}
		msg, ok := providerWizardOAuthCmdFor(descriptor, 7, configPath)().(providerWizardOAuthMsg)
		if !ok || msg.err == nil || !strings.Contains(msg.err.Error(), "ambiguous persisted provider names") {
			t.Fatalf("wizard OAuth %s did not preflight: %#v", providerID, msg)
		}
		setupMsg, ok := setupOAuthCmd(descriptor, configPath)().(setupOAuthMsg)
		if !ok || setupMsg.err == nil || !strings.Contains(setupMsg.err.Error(), "ambiguous persisted provider names") {
			t.Fatalf("setup OAuth %s did not preflight: %#v", providerID, setupMsg)
		}
	}
	deviceMsg, ok := providerWizardDevicePollCmd(configPath, "xai", 8, oauth.Config{}, oauth.DeviceAuth{})().(providerWizardOAuthMsg)
	if !ok || deviceMsg.err == nil || !strings.Contains(deviceMsg.err.Error(), "ambiguous persisted provider names") {
		t.Fatalf("device completion did not revalidate config: %#v", deviceMsg)
	}
	stored, ok, err := store.Load(oauth.ProviderKey("xai"))
	if err != nil || !ok || stored.AccessToken != previous.AccessToken || stored.RefreshToken != previous.RefreshToken {
		t.Fatalf("rejected TUI login changed previous token: ok=%v err=%v", ok, err)
	}
	after, err := os.ReadFile(configPath)
	if err != nil || string(after) != string(seed) {
		t.Fatalf("rejected TUI login changed config: readErr=%v", err)
	}
}

func TestAppendOAuthLoginProfileAddsOnceAndRespectsRenames(t *testing.T) {
	saved := []config.ProviderProfile{{Name: "opengateway", ProviderKind: config.ProviderKindOpenAICompatible}}

	saved = appendOAuthLoginProfile(saved, "chatgpt")
	if len(saved) != 2 || saved[1].Name != "chatgpt" || saved[1].CatalogID != "chatgpt" {
		t.Fatalf("expected chatgpt appended, got %+v", saved)
	}
	if saved[1].Model == "" || saved[1].BaseURL == "" {
		t.Fatalf("appended profile must carry catalog defaults, got %+v", saved[1])
	}

	// Idempotent: a second login must not duplicate.
	saved = appendOAuthLoginProfile(saved, "chatgpt")
	if len(saved) != 2 {
		t.Fatalf("duplicate appended: %+v", saved)
	}

	// A renamed profile serving the same catalog entry blocks the append too.
	renamed := []config.ProviderProfile{{Name: "codex", CatalogID: "chatgpt"}}
	renamed = appendOAuthLoginProfile(renamed, "chatgpt")
	if len(renamed) != 1 {
		t.Fatalf("renamed profile not respected: %+v", renamed)
	}

	// Unknown catalog id: no-op.
	if got := appendOAuthLoginProfile(nil, "no-such-provider"); got != nil {
		t.Fatalf("unknown provider must not append, got %+v", got)
	}
}
