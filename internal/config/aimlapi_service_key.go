package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/Twigpine/zero/internal/aimlapi"
	"github.com/Twigpine/zero/internal/providercatalog"
)

const aimlapiKeyEnv = "AIMLAPI_API_KEY"

var (
	// ErrAIMLAPIKeyMissing means no AI/ML API key is configured.
	ErrAIMLAPIKeyMissing = errors.New("no AI/ML API key is configured; set up aimlapi.com with /provider or set AIMLAPI_API_KEY")
	// ErrAIMLAPIProxyProfile means the aimlapi profile points at a proxy, so its
	// key belongs to that proxy and is withheld from aimlapi.com services.
	ErrAIMLAPIProxyProfile = errors.New("the aimlapi provider points at a proxy base URL; its key is not sent to aimlapi.com services")
	// ErrAIMLAPIKeyInvalid means the key cannot be sent as a single Bearer token.
	ErrAIMLAPIKeyInvalid = errors.New("the AI/ML API key contains whitespace, a comma or a control character")
)

// AIMLAPIServiceKey returns the AI/ML API key that may be sent to the
// aimlapi.com Toolbox and MCP server. It is resolved again on every call, so a
// provider change is seen by the next request.
//
// The key comes from the aimlapi provider profile (the active one when it is
// aimlapi, otherwise the first), including a key held in the credential store,
// or from AIMLAPI_API_KEY when no aimlapi profile exists. The key is withheld
// when that profile, any aimlapi profile, or any profile reading
// AIMLAPI_API_KEY points at a base URL other than the canonical
// https://api.aimlapi.com/v1, and when AIMLAPI_INFERENCE_URL is overridden:
// in those cases the credential may belong to a proxy.
func AIMLAPIServiceKey(resolved ResolvedConfig, store APIKeyGetter, getenv func(string) string) (string, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	if override := strings.TrimSpace(getenv("AIMLAPI_INFERENCE_URL")); override != "" && !sameBaseURL(override, aimlapi.CanonicalInferenceBaseURL) {
		return "", ErrAIMLAPIProxyProfile
	}
	for _, profile := range resolved.Providers {
		if aimlapiKeyedProfile(profile) && !sameBaseURL(profile.BaseURL, aimlapi.CanonicalInferenceBaseURL) {
			return "", ErrAIMLAPIProxyProfile
		}
	}
	if profile, ok := aimlapiServiceProfile(resolved); ok {
		if !sameBaseURL(profile.BaseURL, aimlapi.CanonicalInferenceBaseURL) {
			return "", ErrAIMLAPIProxyProfile
		}
		if store != nil && profile.APIKeyStored && strings.TrimSpace(profile.APIKey) == "" && strings.TrimSpace(profile.Name) != "" {
			key, found, err := store.Get(strings.TrimSpace(profile.Name))
			if err != nil {
				return "", fmt.Errorf("read AI/ML API credential store: %w", err)
			}
			if found {
				profile.APIKey = key
			}
		}
		return validAIMLAPIServiceKey(profile.APIKey)
	}
	return validAIMLAPIServiceKey(getenv(aimlapiKeyEnv))
}

// LoadAIMLAPIServiceKey resolves the user configuration, the environment and
// the user credential store, then applies AIMLAPIServiceKey. Project config is
// not read: provider keys are user-scoped.
func LoadAIMLAPIServiceKey() (string, error) {
	userConfigPath, err := DefaultUserConfigPath()
	if err != nil {
		return "", err
	}
	existing, err := existingConfigFile(userConfigPath)
	if err != nil {
		return "", err
	}
	resolved, err := Resolve(ResolveOptions{UserConfigPath: existing, ExcludeProject: true})
	if err != nil {
		return "", err
	}
	// Check proxy eligibility and already-resolved keys before opening a store.
	key, keyErr := AIMLAPIServiceKey(resolved, nil, os.Getenv)
	if !errors.Is(keyErr, ErrAIMLAPIKeyMissing) {
		return key, keyErr
	}
	profile, ok := aimlapiServiceProfile(resolved)
	if !ok || !profile.APIKeyStored {
		return key, keyErr
	}
	store, err := ProviderKeyStoreAt(filepath.Dir(userConfigPath))
	if err != nil {
		return "", fmt.Errorf("open AI/ML API credential store: %w", err)
	}
	return AIMLAPIServiceKey(resolved, store, os.Getenv)
}

func isAIMLAPIProfile(profile ProviderProfile) bool {
	return strings.EqualFold(providercatalog.NormalizeID(profile.CatalogID), providercatalog.AIMLAPIID)
}

// aimlapiKeyedProfile reports whether profile carries an AI/ML API credential:
// an aimlapi catalog profile, or any profile that reads AIMLAPI_API_KEY.
func aimlapiKeyedProfile(profile ProviderProfile) bool {
	return isAIMLAPIProfile(profile) || strings.EqualFold(strings.TrimSpace(profile.APIKeyEnv), aimlapiKeyEnv)
}

func aimlapiServiceProfile(resolved ResolvedConfig) (ProviderProfile, bool) {
	if isAIMLAPIProfile(resolved.Provider) {
		return resolved.Provider, true
	}
	for _, profile := range resolved.Providers {
		if isAIMLAPIProfile(profile) {
			return profile, true
		}
	}
	return ProviderProfile{}, false
}

func validAIMLAPIServiceKey(raw string) (string, error) {
	key := strings.TrimSpace(raw)
	if key == "" {
		return "", ErrAIMLAPIKeyMissing
	}
	for _, r := range key {
		if unicode.IsSpace(r) || r <= ' ' || r == 0x7f || r == ',' || (r >= 0x80 && r <= 0x9f) {
			return "", ErrAIMLAPIKeyInvalid
		}
	}
	return key, nil
}
