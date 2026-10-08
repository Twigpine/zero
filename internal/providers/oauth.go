package providers

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/Gitlawb/zero/internal/config"
	"github.com/Gitlawb/zero/internal/oauth"
	"github.com/Gitlawb/zero/internal/providers/providerio"
)

// OAuthLoginForProfile resolves the OAuth login used by profile once and
// returns both its bearer resolver and credential-store key. Keeping the key
// alongside the resolver lets auxiliary requests derive account-scoped headers
// from the same login that supplies the bearer.
func OAuthLoginForProfile(profile config.ProviderProfile) (providerio.TokenResolver, string) {
	candidates := profile.OAuthLoginCandidates()
	if len(candidates) == 0 {
		return nil, ""
	}
	store, err := oauth.NewStore(oauth.StoreOptions{})
	if err != nil {
		return nil, ""
	}
	_, key, ok := oauth.FirstStored(store, candidates)
	if !ok {
		return nil, ""
	}
	manager, err := oauth.NewManager(oauth.ManagerOptions{
		Store:        store,
		HTTPClient:   &http.Client{Timeout: 30 * time.Second},
		AllowPresets: true,
	})
	if err != nil {
		return nil, ""
	}
	resolver := func(ctx context.Context, forceRefresh bool) (string, string, bool, error) {
		var token string
		var resolveErr error
		if forceRefresh {
			token, resolveErr = manager.Handle401(ctx, key)
		} else {
			token, resolveErr = manager.GetFresh(ctx, key)
		}
		if errors.Is(resolveErr, oauth.ErrNoToken) {
			return "", "", false, nil
		}
		if resolveErr != nil {
			return "", "", false, resolveErr
		}
		return "Authorization", "Bearer " + token, true, nil
	}
	return resolver, key
}
