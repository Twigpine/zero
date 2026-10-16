package tui

import (
	"context"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/Gitlawb/zero/internal/browser"
	"github.com/Gitlawb/zero/internal/config"
	"github.com/Gitlawb/zero/internal/oauth"
)

func preflightOAuthLogin(configPath string) error {
	if strings.TrimSpace(configPath) == "" {
		return nil
	}
	return config.PreflightUserConfig(configPath)
}

// oauthPreferDeviceFlow reports whether the device-code flow should be the
// default for a device-capable provider because no usable browser is likely
// present (SSH session or a headless Linux box). On a desktop the browser flow
// stays the default; users can still force device code with the "d" shortcut.
// ZERO_OAUTH_DEVICE forces it on for any environment.
func oauthPreferDeviceFlow() bool {
	if strings.TrimSpace(os.Getenv("ZERO_OAUTH_DEVICE")) != "" {
		return true
	}
	if strings.TrimSpace(os.Getenv("SSH_CONNECTION")) != "" || strings.TrimSpace(os.Getenv("SSH_TTY")) != "" {
		return true
	}
	if runtime.GOOS == "linux" &&
		strings.TrimSpace(os.Getenv("DISPLAY")) == "" &&
		strings.TrimSpace(os.Getenv("WAYLAND_DISPLAY")) == "" {
		return true
	}
	return false
}

// oauthDevicePrepare requests an RFC 8628 device code for the provider (phase 1).
// The returned DeviceAuth carries the verification URI + user code to display;
// pass cfg/auth to oauthDeviceComplete to poll for the token.
func oauthDevicePrepare(name string) (oauth.DeviceAuth, oauth.Config, error) {
	store, err := oauth.NewStore(oauth.StoreOptions{})
	if err != nil {
		return oauth.DeviceAuth{}, oauth.Config{}, err
	}
	manager, err := oauth.NewManager(oauth.ManagerOptions{
		Store:       store,
		HTTPClient:  &http.Client{Timeout: 60 * time.Second},
		OpenBrowser: browser.OpenURL,
		// Device-flow providers (e.g. Hugging Face) rely on the baked-in preset for
		// their endpoints; opt in so the device code can be requested.
		AllowPresets: true,
	})
	if err != nil {
		return oauth.DeviceAuth{}, oauth.Config{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return manager.PrepareDeviceLogin(ctx, oauth.LoginOptions{Provider: name})
}

// oauthDeviceComplete polls for the token authorized via oauthDevicePrepare and
// stores it under provider:<name> (phase 2). The runtime resolver then attaches
// the refreshable token to model calls.
func oauthDeviceComplete(configPath string, name string, cfg oauth.Config, auth oauth.DeviceAuth) error {
	if err := preflightOAuthLogin(configPath); err != nil {
		return err
	}
	store, err := oauth.NewStore(oauth.StoreOptions{})
	if err != nil {
		return err
	}
	manager, err := oauth.NewManager(oauth.ManagerOptions{
		Store:        store,
		HTTPClient:   &http.Client{Timeout: 60 * time.Second},
		AllowPresets: true, // preset config is needed to poll/exchange the device token
		BeforeSave:   func() error { return preflightOAuthLogin(configPath) },
	})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	_, err = manager.CompleteDeviceLogin(ctx, name, cfg, auth)
	return err
}

// oauthStoredToken returns a fresh access token for a provider that was logged in
// via OAuth (token stored under provider:<id>), refreshing on demand. Empty when
// there is no stored login or the refresh fails. Used to authenticate the model
// discovery /models call so the wizard can show the live model list after login.
func oauthStoredToken(ctx context.Context, providerID string) string {
	store, err := oauth.NewStore(oauth.StoreOptions{})
	if err != nil {
		return ""
	}
	manager, err := oauth.NewManager(oauth.ManagerOptions{
		Store:        store,
		HTTPClient:   &http.Client{Timeout: 30 * time.Second},
		AllowPresets: true, // refreshing a preset-provider token re-resolves its config
	})
	if err != nil {
		return ""
	}
	token, err := manager.GetFresh(ctx, oauth.ProviderKey(providerID))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(token)
}

// oauthDeviceVerifyTarget picks the best URL to show the user: the complete URI
// (code pre-filled) when present, else the bare verification URI.
func oauthDeviceVerifyTarget(auth oauth.DeviceAuth) string {
	if target := strings.TrimSpace(auth.VerificationURIComplete); target != "" {
		return target
	}
	return strings.TrimSpace(auth.VerificationURI)
}
