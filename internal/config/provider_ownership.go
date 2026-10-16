package config

import (
	"fmt"
	"strings"
)

// Provider identity has three distinct questions, and conflating them is what
// let a mutation aimed at one row land on another:
//
//  1. "Which stored secret is this?" — SameProviderIdentity, the credential
//     store's normalization. Two spellings can share one secret.
//  2. "Which row does this mutator target?" — exact trimmed equality. Persisted
//     writers address rows exactly, because that is what they rewrite.
//  3. "Which persisted row, if any, does this RESOLVED row own?" — this file.
//
// A resolved provider list is a merge of user config, project config, and
// environment discovery, and the merge is exact: user "work" and project "WORK"
// are two rows, not one. Once such a row is flattened into a ProviderProfile,
// its display Name no longer says which layer produced it, so asking question 1
// and acting as if it answered question 3 made "shares a credential identity
// with a user row" mean "IS that user row" — and a delete or edit of the project
// row rewrote the user's.

// ProviderNameLookup is the outcome of resolving a provider spelling against a
// set of candidate row names.
type ProviderNameLookup uint8

const (
	// ProviderNameNotFound means no candidate carries the spelling or its
	// credential identity.
	ProviderNameNotFound ProviderNameLookup = iota
	// ProviderNameExact means a candidate carries the spelling byte for byte.
	ProviderNameExact
	// ProviderNameNormalized means exactly one candidate carries the credential
	// identity under a different spelling.
	ProviderNameNormalized
	// ProviderNameAmbiguous means several candidates carry the identity. It is
	// deliberately distinct from NotFound: callers that used first-match picked
	// one of them silently.
	ProviderNameAmbiguous
)

// Resolved reports whether the lookup produced a usable name.
func (lookup ProviderNameLookup) Resolved() bool {
	return lookup == ProviderNameExact || lookup == ProviderNameNormalized
}

// LookupProviderName is the ONE rule for resolving a provider spelling when only
// a name is available: an exact match wins outright, a credential-identity match
// is accepted only when exactly ONE candidate carries that identity, and several
// candidates return Ambiguous rather than the first one encountered.
//
// The exact-first half is what keeps case siblings distinct — a caller holding
// "work" must never be handed "WORK" while both exist — and the single-candidate
// half is what still lines a session launched with ZERO_PROVIDER=openai up with
// a sole saved "OpenAI" row.
func LookupProviderName(candidates []string, want string) (string, ProviderNameLookup) {
	want = strings.TrimSpace(want)
	if want == "" {
		return "", ProviderNameNotFound
	}
	match := ""
	matches := 0
	for _, candidate := range candidates {
		name := strings.TrimSpace(candidate)
		if name == "" {
			continue
		}
		if name == want {
			return name, ProviderNameExact
		}
		if sameProviderIdentity(name, want) {
			match = name
			matches++
		}
	}
	switch {
	case matches == 1:
		return match, ProviderNameNormalized
	case matches > 1:
		return "", ProviderNameAmbiguous
	default:
		return "", ProviderNameNotFound
	}
}

// ProviderProfileNames extracts row spellings for the lookup helpers.
func ProviderProfileNames(profiles []ProviderProfile) []string {
	names := make([]string, 0, len(profiles))
	for _, profile := range profiles {
		names = append(names, strings.TrimSpace(profile.Name))
	}
	return names
}

// ProviderRowOwnership is the answer to question 3 above: may this resolved row
// mutate a user-config row, and which exact row?
type ProviderRowOwnership struct {
	// UserBacked is true only when a user-config mutator may run for this row.
	// A project- or environment-derived row is session-only and never sets it.
	UserBacked bool
	// PersistedName is the EXACT user-config row spelling to hand to mutators —
	// RemoveProvider, EditProvider, SetProviderModel, key deletion. Empty unless
	// UserBacked.
	PersistedName string
	// Reason explains a non-user-backed answer in the user's terms, so a UI can
	// say why an edit or delete is session-only instead of silently doing
	// something else.
	Reason string
	// Lookup is the underlying name-resolution outcome: ProviderNameNotFound for
	// the ordinary case of a row with no persisted counterpart at all (e.g. an
	// environment-derived provider), or ProviderNameAmbiguous when several
	// persisted rows share the identity. A caller that wants to stay quiet for
	// the ordinary case and surface Reason only for the surprising ones branches
	// on this instead of parsing Reason's text.
	Lookup ProviderNameLookup
	// Shadowed is true when a credential-identity match was found but rejected
	// because the resolved list contains a separate row with this spelling or
	// the persisted spelling — even when the saved row was filtered as unusable.
	Shadowed bool
}

// ResolveProviderRowOwnership decides which persisted user-config row a resolved
// provider row owns.
//
// persisted is the user config's rows. resolvedNames is every row spelling in
// the resolved list the caller is displaying — the siblings matter, and are what
// a plain name lookup cannot see.
//
// The rule:
//
//   - An exact persisted row is owned outright.
//   - Otherwise, a credential-identity match is a candidate only when exactly
//     one persisted row carries that identity. Several is ambiguous, and a
//     mutation under ambiguity would pick a row at random.
//   - A concrete resolved row with a different spelling is NOT the persisted
//     row. User-layer merging preserves that row's exact name, even when the
//     active selector uses different casing. The display list may omit an
//     unusable user row, so its absence cannot prove ownership. Normalized
//     fallback is only for a session alias not present as a separate row.
//
// The last clause is the whole defect: with user "work" and project "WORK" both
// resolved, "WORK" found the sole identity match "work" and edited or deleted it
// — while the in-memory operation targeted exact "WORK", so the row changed on
// disk was not the row changed in the session.
func ResolveProviderRowOwnership(persisted []ProviderProfile, resolvedNames []string, name string) ProviderRowOwnership {
	name = strings.TrimSpace(name)
	if name == "" {
		return ProviderRowOwnership{Reason: "this provider has no name"}
	}
	persistedName, lookup := LookupProviderName(ProviderProfileNames(persisted), name)
	switch lookup {
	case ProviderNameExact:
		return ProviderRowOwnership{UserBacked: true, PersistedName: persistedName, Lookup: lookup}
	case ProviderNameAmbiguous:
		return ProviderRowOwnership{Lookup: lookup, Reason: fmt.Sprintf(
			"several rows in config.json differ from %q only by case; rename or remove one before changing it", name)}
	case ProviderNameNotFound:
		return ProviderRowOwnership{Lookup: lookup, Reason: fmt.Sprintf("%q isn't saved in config.json", name)}
	}
	for _, resolved := range resolvedNames {
		resolved = strings.TrimSpace(resolved)
		if resolved == "" {
			continue
		}
		// Only a concrete row with the requested spelling shadows the persisted
		// row. The persisted spelling in the list is that row itself.
		if resolved == name {
			return ProviderRowOwnership{Lookup: lookup, Shadowed: true, Reason: fmt.Sprintf(
				"%q is not the saved provider %q — this resolved row comes from project config or the environment",
				name, persistedName)}
		}
	}
	return ProviderRowOwnership{UserBacked: true, PersistedName: persistedName, Lookup: lookup}
}

// ProviderRowOwnershipAt is ResolveProviderRowOwnership reading the persisted
// rows from a config path. A path that cannot be read is not user-backed:
// nothing may be mutated through a file this process cannot see.
func ProviderRowOwnershipAt(path string, resolvedNames []string, name string) (ProviderRowOwnership, error) {
	if strings.TrimSpace(path) == "" {
		return ProviderRowOwnership{Reason: "no user config path"}, nil
	}
	providers, err := persistedProviders(path)
	if err != nil {
		return ProviderRowOwnership{}, err
	}
	return ResolveProviderRowOwnership(providers, resolvedNames, name), nil
}

// ResolveProviderSourceNames loads and merges the identity-bearing sources with
// the same trust checks as Resolve, without requiring runnable providers or an
// active selection. Unusable rows still own their structural names.
func ResolveProviderSourceNames(options ResolveOptions) ([]string, error) {
	cfg, err := resolveSourceConfig(options)
	if err != nil {
		return nil, err
	}
	return ProviderProfileNames(cfg.Providers), nil
}
