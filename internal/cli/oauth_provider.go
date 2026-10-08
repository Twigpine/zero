package cli

import (
	"github.com/Gitlawb/zero/internal/config"
	"github.com/Gitlawb/zero/internal/providers"
	"github.com/Gitlawb/zero/internal/providers/providerio"
)

// oauthLoginForProfile resolves the user's OAuth login for a provider ONCE and
// returns both a TokenResolver that authenticates model calls with it and the
// credential-store key it bound to. It returns (nil, "") when no login exists —
// keeping API-key users free of any per-request store lookups, since the resolver
// is only attached when a login is present at construction time.
//
// The returned key is the single source of truth for "which login is this
// provider using": callers pass it to providers.Options.OAuthLoginKey so the
// Codex chatgpt-account-id header reads its account from the exact login that
// issued the bearer token, instead of doing a second, independent lookup that
// could select a different login (a backend-rejected mismatch).
//
// Candidate login names (profile name, then a catalog-ID fallback, both gated on
// the profile having no own configured credential) come from the shared
// ProviderProfile.OAuthLoginCandidates so the runtime resolver, the Codex account
// resolver, and the onboarding presence check never diverge.
func oauthLoginForProfile(profile config.ProviderProfile) (providerio.TokenResolver, string) {
	return providers.OAuthLoginForProfile(profile)
}
