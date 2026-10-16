package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"

	"github.com/Gitlawb/zero/internal/credstore"
	"github.com/Gitlawb/zero/internal/notify"
	"github.com/Gitlawb/zero/internal/providercatalog"
)

// ValidatePersistedProviderNames rejects empty names and user-config rows that
// share the credential store's normalized identity. Allowing either would make
// resolver defaults or credential writes and deletes affect another row.
// This validator intentionally applies only to raw persisted user config, not
// to profiles merged from project, environment, or provider-command layers.
//
// A repeated normalized identity is rejected whether or not the spellings
// differ. Exact duplicates are just as broken as case variants: resolver
// merging coalesces the rows, and plaintext-key migration writes both values
// into the same credential-store entry, so the second row's key overwrites the
// first.
func ValidatePersistedProviderNames(cfg FileConfig) error {
	seen := make(map[string]string, len(cfg.Providers))
	for _, provider := range cfg.Providers {
		name := strings.TrimSpace(provider.Name)
		if name == "" {
			return fmt.Errorf("persisted provider name cannot be empty; run `zero providers repair-config` to name the legacy provider")
		}
		folded := credstore.NormalizeProvider(name)
		previous, ok := seen[folded]
		if ok && previous == name {
			return fmt.Errorf("duplicate persisted provider name %q; remove one of the rows in config.json", name)
		}
		if ok {
			// Name the repair command, not just the problem: this rejection is
			// reached at config READ time, so a legacy duplicate blocks the
			// interactive shell and the TUI outright. `zero providers remove`
			// reads config.json directly instead of going through Resolve, so
			// it still works while everything else refuses to start.
			return fmt.Errorf("ambiguous persisted provider names %q and %q differ only by case; run `zero providers remove %s` (exact spelling) or rename one row in config.json", previous, name, name)
		}
		seen[folded] = name
	}
	return nil
}

// persistedProviderNameProblems returns independently repairable persisted-name
// problems. Keys are stable identities so a repair can prove it reduced an
// existing problem without introducing or worsening another one.
func persistedProviderNameProblems(cfg FileConfig) map[string]int {
	problems := map[string]int{}
	seen := map[string]int{}
	for _, provider := range cfg.Providers {
		name := strings.TrimSpace(provider.Name)
		if name == "" {
			problems["unnamed"]++
			continue
		}
		seen[credstore.NormalizeProvider(name)]++
	}
	for identity, count := range seen {
		if count > 1 {
			problems["duplicate:"+identity] = count - 1
		}
	}
	return problems
}

func writeProviderNameRepair(path string, before FileConfig, after FileConfig) error {
	oldProblems := persistedProviderNameProblems(before)
	if len(oldProblems) == 0 {
		return writeConfigFile(path, after)
	}
	newProblems := persistedProviderNameProblems(after)
	oldTotal, newTotal := 0, 0
	for _, count := range oldProblems {
		oldTotal += count
	}
	for problem, count := range newProblems {
		newTotal += count
		if count > oldProblems[problem] {
			return ValidatePersistedProviderNames(after)
		}
	}
	if newTotal >= oldTotal {
		return ValidatePersistedProviderNames(after)
	}
	data, err := json.MarshalIndent(after, "", "  ")
	if err != nil {
		return fmt.Errorf("encode config JSON: %w", err)
	}
	return writeConfigData(path, data)
}

// RepairUnnamedProvider gives legacy provider rows that predate required names
// an explicit persisted identity. Older releases resolved one unnamed row as
// activeProvider, falling back to "openai"; preserve that choice unless the
// user supplies a replacement. Stored-key profiles cannot change credential
// identity during repair. Multiple unnamed rows are left untouched because
// selecting one would silently merge or discard profiles.
//
// The chosen name is returned rather than left for the caller to re-derive: the
// defaulting rules below are the only thing that knows which name the row got,
// and the CLI re-deriving them reported activeProvider as the repaired name
// while the row had actually been named by the fallback.
func RepairUnnamedProvider(path string, replacement string) (result FileConfig, repairedName string, err error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return FileConfig{}, "", fmt.Errorf("config path is required")
	}
	unlock, err := lockConfigFileFn(path)
	if err != nil {
		return FileConfig{}, "", err
	}
	defer func() { err = errors.Join(err, unlock()) }()
	data, err := os.ReadFile(path)
	if err != nil {
		return FileConfig{}, "", fmt.Errorf("read config %s: %w", path, err)
	}
	var cfg FileConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return FileConfig{}, "", fmt.Errorf("invalid config JSON %s: %w", path, err)
	}
	unnamed := -1
	for index := range cfg.Providers {
		if strings.TrimSpace(cfg.Providers[index].Name) != "" {
			continue
		}
		if unnamed >= 0 {
			return FileConfig{}, "", fmt.Errorf("multiple unnamed persisted providers require manual repair in config.json")
		}
		unnamed = index
	}
	if unnamed < 0 {
		return FileConfig{}, "", fmt.Errorf("no unnamed persisted provider found")
	}
	activeName := strings.TrimSpace(cfg.ActiveProvider)
	// Establish legacy row ownership before changing its serialized name. The
	// old merge materialized this name before exact-first active selection, so
	// a case sibling cannot own the unnamed row's exact active selector.
	legacyName := providerMergeName(cfg, cfg.Providers[unnamed])
	var legacyConfig FileConfig
	mergeConfig(&legacyConfig, cfg)
	selected, lookup := LookupProviderName(ProviderProfileNames(legacyConfig.Providers), activeName)
	unnamedWasActive := lookup.Resolved() && selected == legacyName
	// An explicit split leaves an existing exact named selector on that row.
	// Bare repair below instead preserves their legacy merged composition.
	for index, profile := range cfg.Providers {
		if index != unnamed && strings.TrimSpace(profile.Name) == activeName {
			unnamedWasActive = false
			break
		}
	}
	activeMatchesNamedRow := lookup.Resolved() && !unnamedWasActive
	name := strings.TrimSpace(replacement)
	explicit := name != ""
	if !explicit {
		// Older Resolve merged an unnamed row into its exact effective-name
		// sibling, in file order. Preserve that composition, not just the name:
		// the named row may supply the model while the unnamed row supplies the
		// endpoint and credential. Splitting them strands a working legacy config.
		for index, profile := range cfg.Providers {
			if index == unnamed || strings.TrimSpace(profile.Name) != legacyName {
				continue
			}
			before := cfg
			cfg.Providers = append([]ProviderProfile(nil), cfg.Providers...)
			legacy := cfg.Providers[unnamed]
			legacy.Name = legacyName
			remove := unnamed
			if unnamed < index {
				cfg.Providers[unnamed] = mergeProfile(legacy, profile)
				remove = index
			} else {
				cfg.Providers[index] = mergeProfile(profile, legacy)
			}
			cfg.Providers = append(cfg.Providers[:remove], cfg.Providers[remove+1:]...)
			if err := writeProviderNameRepair(path, before, cfg); err != nil {
				return FileConfig{}, "", err
			}
			return cfg, legacyName, nil
		}
		// Preserve the legacy effective name unless the active selector belongs
		// to a different row.
		if !activeMatchesNamedRow {
			name = activeName
		}
		if name == "" {
			name = "openai"
		}
	}
	if cfg.Providers[unnamed].APIKeyStored {
		if sibling, collides := conflictingProviderRowName(cfg, unnamed, legacyName); collides && sibling != legacyName {
			return FileConfig{}, "", fmt.Errorf("the unnamed provider's stored credential uses identity %q, which conflicts with persisted provider %q; manually rename the sibling %q in config.json and reconcile any of its credential references before retrying `zero providers repair-config`; --name cannot safely move the stored credential", legacyName, sibling, sibling)
		}
	}
	if conflict, collides := conflictingProviderRowName(cfg, unnamed, name); collides {
		if explicit {
			return FileConfig{}, "", fmt.Errorf(
				"cannot name the unnamed provider %q: persisted provider %q already uses that identity; choose a different --name",
				name, conflict)
		}
		// Say what the proposed name was, that it is already taken, and the exact
		// command that gets out of it. The bare form has no other escape.
		return FileConfig{}, "", fmt.Errorf(
			"the unnamed provider would default to %q, which persisted provider %q already uses; rerun with `zero providers repair-config --name <unique-name>`",
			name, conflict)
	}
	if cfg.Providers[unnamed].APIKeyStored && !sameProviderIdentity(legacyName, name) {
		return FileConfig{}, "", fmt.Errorf("cannot rename the unnamed provider from %q to %q during repair: its stored credential is indexed by the old identity; repair with the legacy name first, or manually repair the config and credential together", legacyName, name)
	}
	cfg.Providers[unnamed].Name = name
	// Carry the exact effective row's active reference with the repair.
	if activeName != "" && unnamedWasActive {
		cfg.ActiveProvider = name
	}
	var before FileConfig
	if err := json.Unmarshal(data, &before); err != nil {
		return FileConfig{}, "", fmt.Errorf("invalid config JSON %s: %w", path, err)
	}
	if err := writeProviderNameRepair(path, before, cfg); err != nil {
		return FileConfig{}, "", err
	}
	return cfg, name, nil
}

// conflictingProviderRowName reports the persisted row, other than the one being
// repaired, that already owns the proposed name. Identity is the credential
// store's rule, matching ValidatePersistedProviderNames, so the check refuses
// exactly what the write would refuse — before the write, and with a message
// that describes the file rather than the rejected candidate state.
func conflictingProviderRowName(cfg FileConfig, repairing int, name string) (string, bool) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", false
	}
	for index := range cfg.Providers {
		if index == repairing {
			continue
		}
		rowName := strings.TrimSpace(cfg.Providers[index].Name)
		if rowName == "" {
			continue
		}
		if rowName == name || sameProviderIdentity(rowName, name) {
			return rowName, true
		}
	}
	return "", false
}

// sameProviderIdentity reports whether two persisted spellings name the same
// provider identity. It is credstore.NormalizeProvider — the credential store's
// own rule — rather than strings.EqualFold, because the two disagree and the
// store is the authority: EqualFold folds "s" and Unicode long-s "ſ" together,
// while the store keeps separate entries for them. Treating them as one identity
// let a mutation of one profile reach the other's row and its secret, which is
// precisely what ValidatePersistedProviderNames permits as a distinct pair.
func sameProviderIdentity(a string, b string) bool {
	return credstore.NormalizeProvider(a) == credstore.NormalizeProvider(b)
}

// SameProviderIdentity exposes the credential store's provider-name identity
// rule to UI and CLI list operations. It deliberately differs from
// strings.EqualFold for Unicode spellings such as "s" and long-s.
func SameProviderIdentity(a string, b string) bool {
	return sameProviderIdentity(strings.TrimSpace(a), strings.TrimSpace(b))
}

// PreflightUserConfig validates existing user config before any command makes
// credential-store side effects.
func PreflightUserConfig(path string) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return fmt.Errorf("config path is required")
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read config %s: %w", path, err)
	}
	var cfg FileConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return fmt.Errorf("invalid config JSON %s: %w", path, err)
	}
	return ValidatePersistedProviderNames(cfg)
}

// PreflightCatalogProviderLogin is the preflight for a credential login against
// a catalog provider. A login does not mint a new spelling, so a case-variant
// persisted row may be reused only when its catalogID positively proves that it
// owns the requested catalog identity, or when it claims no catalog identity at
// all and the login can backfill one. A matching display name over a catalogID
// pointing somewhere ELSE is not ownership: custom profiles may use
// catalog-like names for unrelated endpoints.
//
// The collision check still runs when nothing owns the identity, because that is
// the case where a row WILL be created. It is unreachable today given
// EnsureCatalogProvider's own matching, and kept so that if that matching ever
// narrows, this fails fast before the browser flow instead of after it.
func PreflightCatalogProviderLogin(path, catalogID string) error {
	if err := PreflightUserConfig(path); err != nil {
		return err
	}
	descriptor, catalogProvider := providercatalog.Get(catalogID)
	if catalogProvider {
		providers, err := persistedProviders(path)
		if err != nil {
			return err
		}
		// An adoptable row is as good as an owning one here: the login writes
		// the catalog id onto that existing row instead of minting a spelling,
		// so the collision check below does not apply to it either.
		if _, ownership, err := catalogProviderOwner(providers, descriptor.ID); err != nil {
			return err
		} else if ownership != catalogOwnershipNone {
			return nil
		}
		return PreflightProviderWrite(path, descriptor.ID)
	}
	if _, owned, err := PersistedProviderIdentity(path, catalogID); err != nil {
		return err
	} else if owned {
		return nil
	}
	return PreflightProviderWrite(path, catalogID)
}

// providerOwnsCatalog is the positive ownership test for catalog login and
// persistence. A matching display/name spelling is not ownership: custom
// profiles may legitimately use names such as "OpenRouter" while pointing at a
// different catalog provider or carrying no catalog id at all.
func providerOwnsCatalog(provider ProviderProfile, catalogID string) bool {
	rowCatalogID := strings.TrimSpace(provider.CatalogID)
	return rowCatalogID != "" && sameProviderIdentity(rowCatalogID, catalogID)
}

// catalogOwnership reports how a persisted config relates to a catalog identity.
type catalogOwnership int

const (
	// catalogOwnershipNone: no row claims the identity, so a login creates one.
	catalogOwnershipNone catalogOwnership = iota
	// catalogOwnershipOwned: exactly one row proves ownership through its catalogID.
	catalogOwnershipOwned
	// catalogOwnershipAdoptable: exactly one row carries the catalog spelling as
	// its NAME and claims no catalog id at all. An empty catalogID is an absent
	// claim, not a competing one, so that row is the answer once the id is
	// backfilled onto it — the same self-healing `zero providers add <id>`
	// performs for configs written before catalog ids existed.
	catalogOwnershipAdoptable
)

// catalogProviderOwner resolves the one persisted row that owns a catalog
// identity, returning its index in providers (-1 when there is none). Catalog
// ids may normally be shared by named profiles, but a catalog-addressed login
// cannot choose among them: it would store one shared provider:<catalog-id>
// token that logout/status/refresh cannot attribute safely. A different row
// owning the catalog spelling as its NAME is equally ambiguous.
func catalogProviderOwner(providers []ProviderProfile, catalogID string) (int, catalogOwnership, error) {
	catalogID = strings.TrimSpace(catalogID)
	ownerIndex := -1
	owners := 0
	for index, provider := range providers {
		if providerOwnsCatalog(provider, catalogID) {
			ownerIndex = index
			owners++
		}
	}
	if owners > 1 {
		return -1, catalogOwnershipNone, fmt.Errorf("provider identity %q is ambiguous: %d saved profiles use it as a catalogID; remove one with `zero providers remove <name>`", catalogID, owners)
	}
	if owners == 1 {
		for index, provider := range providers {
			if index != ownerIndex && sameProviderIdentity(provider.Name, catalogID) {
				return -1, catalogOwnershipNone, fmt.Errorf("provider identity %q is ambiguous: saved profile %q uses it as a name while %q uses it as a catalogID; rename or remove one with `zero providers remove %s`", catalogID, strings.TrimSpace(provider.Name), strings.TrimSpace(providers[ownerIndex].Name), strings.TrimSpace(provider.Name))
			}
		}
		return ownerIndex, catalogOwnershipOwned, nil
	}

	namedIndex := -1
	named := 0
	for index, provider := range providers {
		if sameProviderIdentity(provider.Name, catalogID) {
			namedIndex = index
			named++
		}
	}
	switch {
	case named == 0:
		return -1, catalogOwnershipNone, nil
	case named > 1:
		// ValidatePersistedProviderNames rejects this pair before every caller
		// reaches here; kept so a narrowing of that validation fails closed.
		return -1, catalogOwnershipNone, fmt.Errorf("provider identity %q is ambiguous: %d saved profiles use it as a name; remove one with `zero providers remove <name>`", catalogID, named)
	}
	if strings.TrimSpace(providers[namedIndex].CatalogID) == "" {
		return namedIndex, catalogOwnershipAdoptable, nil
	}
	// The row names itself for this catalog provider while its catalogID claims
	// a different one. That is a competing claim, not an absent one, so it is
	// refused — but the refusal carries the way out, because this path is
	// reached from `zero auth login`, where a user with an old config arrives.
	name := strings.TrimSpace(providers[namedIndex].Name)
	return -1, catalogOwnershipNone, fmt.Errorf("saved profile %q does not prove ownership of catalog provider %q (catalogID is %q); rename or remove that row with `zero providers remove %s`, then run `zero providers add %s`", name, catalogID, strings.TrimSpace(providers[namedIndex].CatalogID), name, catalogID)
}

// ResolvePersistedProviderName maps a user- or session-supplied provider
// spelling to the EXACT name of the persisted row it addresses, so a caller
// that gated on credential identity (ProviderPersisted, a resolved provider
// list, a live session's provider name) can hand a row-targeting mutator
// (RemoveProvider, RenameProvider, EditProvider, SetProviderModel,
// MarkProviderAPIKeyStored) a spelling those mutators can actually find.
//
// It is the one bridge between the two identity rules this package defines:
// an exact spelling always wins, credential identity is a fallback, and an
// identity that matches more than one row is an error rather than an
// arbitrary pick — the same ambiguity ValidatePersistedProviderNames rejects
// at write time, reported here for configs that predate that validation.
func ResolvePersistedProviderName(path string, input string) (string, error) {
	providers, err := persistedProviders(path)
	if err != nil {
		return "", err
	}
	return resolvePersistedProviderName(providers, input)
}

// resolvePersistedProviderName is LookupProviderName with errors — the shared
// exact-first / unique-normalized / ambiguous rule, not a second copy of it.
func resolvePersistedProviderName(providers []ProviderProfile, input string) (string, error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return "", fmt.Errorf("provider name is required")
	}
	name, lookup := LookupProviderName(ProviderProfileNames(providers), input)
	switch lookup {
	case ProviderNameExact, ProviderNameNormalized:
		return name, nil
	case ProviderNameAmbiguous:
		matches := 0
		for _, provider := range providers {
			if sameProviderIdentity(strings.TrimSpace(provider.Name), input) {
				matches++
			}
		}
		return "", fmt.Errorf("ambiguous provider %q: %d rows in config.json differ only by case; rename or remove one row", input, matches)
	default:
		return "", fmt.Errorf("provider %q not found", input)
	}
}

// CredentialKeyRetained reports whether, after removedName's row is gone, some
// REMAINING row still owns the credential-store entry that row pointed at — so
// deleting the shared secret would break a profile the user did not remove.
//
// Ownership is the marker, not the name: the store normalizes "work" and
// "WORK" to one entry, but a surviving row with apiKeyStored:false never reads
// it (ApplyStoredAPIKey gates on the marker), so keeping the secret for that
// row would only orphan it. Retain the key when a survivor actually claims it;
// otherwise the removal took the last owner with it and the key must go.
func CredentialKeyRetained(providers []ProviderProfile, removedName string) bool {
	removedName = strings.TrimSpace(removedName)
	if removedName == "" {
		return false
	}
	for _, provider := range providers {
		if provider.APIKeyStored && sameProviderIdentity(strings.TrimSpace(provider.Name), removedName) {
			return true
		}
	}
	return false
}

// ProviderKeyRetainedAfterRemoval answers CredentialKeyRetained's question
// against the config on disk, simulating the removal of name's row. Callers
// that must know the outcome BEFORE mutating anything use this — a delete
// confirmation has to promise exactly what the delete will do, and computing
// it from a different rule is how the prompt came to claim "this also removes
// its stored API key" for a delete that keeps the key.
func ProviderKeyRetainedAfterRemoval(path string, name string) (bool, error) {
	providers, err := persistedProviders(path)
	if err != nil {
		return false, err
	}
	// Resolve first, for the same reason the delete does: callers hand this a
	// spelling from a resolved list, which may not be the row's own. Previewing
	// against an unresolved name removes nothing, so a "key is kept" preview
	// could precede a delete that resolves the row and takes the key with it.
	name, err = resolvePersistedProviderName(providers, name)
	if err != nil {
		return false, err
	}
	remaining := make([]ProviderProfile, 0, len(providers))
	removed := false
	for _, provider := range providers {
		if !removed && strings.TrimSpace(provider.Name) == name {
			removed = true
			continue
		}
		remaining = append(remaining, provider)
	}
	return CredentialKeyRetained(remaining, name), nil
}

// persistedProviders reads the provider rows out of the user config at path.
// A missing file is an empty list, not an error: every caller here asks "what
// is already saved?", and "nothing yet" is a legitimate answer.
func persistedProviders(path string) ([]ProviderProfile, error) {
	cfg, err := persistedFileConfig(path)
	return cfg.Providers, err
}

func persistedFileConfig(path string) (FileConfig, error) {
	data, err := os.ReadFile(strings.TrimSpace(path))
	if err != nil {
		return FileConfig{}, persistedProviderReadError(strings.TrimSpace(path), err)
	}
	var cfg FileConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return FileConfig{}, fmt.Errorf("invalid config JSON %s: %w", path, err)
	}
	return cfg, nil
}

// PersistedProviderIdentity reports whether a persisted user-config row already
// owns identity — as its name (case-insensitively) or as its catalog id — and
// returns that row's exact name.
//
// Callers use it to answer "is this the config's provider, however it was
// spelled or addressed?", which is a different question from ProviderPersisted's
// "is this the exact key a mutator will match". A row saved as
// {name: "my-router", catalogId: "openrouter"} is owned by both spellings here,
// so `zero providers use openrouter` is a wrong way to address a saved profile
// rather than an environment-derived provider.
func PersistedProviderIdentity(path, identity string) (string, bool, error) {
	row, match, err := ResolvePersistedProviderIdentity(path, identity)
	if err != nil || match == PersistedIdentityNone {
		return "", false, err
	}
	return strings.TrimSpace(row.Name), true, nil
}

// PersistedIdentityMatch says HOW a persisted row was addressed, because the
// two ways carry different authority. A name match identifies exactly one
// profile. A catalog-id match identifies "some profile of this kind" — several
// profiles may legitimately share one catalog provider — so callers that go on
// to delete credentials must not treat it as proof of ownership.
type PersistedIdentityMatch uint8

const (
	// PersistedIdentityNone means no row owns the identity.
	PersistedIdentityNone PersistedIdentityMatch = iota
	// PersistedIdentityName means a row's own name matched, exactly or as a
	// case variant.
	PersistedIdentityName
	// PersistedIdentityCatalog means the identity matched only the catalog id of
	// exactly one row.
	PersistedIdentityCatalog
	// PersistedIdentityAmbiguous means multiple rows own the folded name or
	// catalog id. No caller may select a row or credential from this result.
	PersistedIdentityAmbiguous
)

// ResolvePersistedProviderIdentity finds the persisted user-config row that
// owns identity and reports how it was addressed.
//
// Names win over catalog ids, and an exact name wins over a case variant.
// Scanning in file order and accepting the first name-OR-catalog hit picked
// whichever row came first, so `zero auth logout xai` against
// [{name:"work-xai", catalogId:"xai"}, {name:"xai"}] resolved to "work-xai" —
// a different profile, with different credentials, than the one named.
//
// A catalog id claimed by more than one row resolves to nothing rather than to
// an arbitrary winner: the caller asked for something the config does not
// uniquely identify, and guessing there is what let one profile's logout delete
// a sibling's shared token.
//
// A non-exact name is held to the same rule. A legacy config can still hold
// case-variant rows such as "work" and "WORK" (persistedProviders reads the file
// without validating it), and resolving "wOrK" to whichever appeared first let
// `providers remove` delete that row and its stored credential. Only an exact
// spelling can address such a pair, which keeps repairing a legacy config
// possible.
func ResolvePersistedProviderIdentity(path, identity string) (ProviderProfile, PersistedIdentityMatch, error) {
	identity = strings.TrimSpace(identity)
	if identity == "" {
		return ProviderProfile{}, PersistedIdentityNone, nil
	}
	providers, err := persistedProviders(path)
	if err != nil {
		return ProviderProfile{}, PersistedIdentityNone, err
	}
	var exactName *ProviderProfile
	exactMatches := 0
	var foldedName *ProviderProfile
	foldedMatches := 0
	var catalogRow *ProviderProfile
	catalogMatches := 0
	for index := range providers {
		row := providers[index]
		name := strings.TrimSpace(row.Name)
		if name == identity {
			exactMatches++
			if exactName == nil {
				match := row
				exactName = &match
			}
		} else if sameProviderIdentity(name, identity) {
			foldedMatches++
			if foldedName == nil {
				match := row
				foldedName = &match
			}
		}
		if sameProviderIdentity(row.CatalogID, identity) {
			catalogMatches++
			if catalogRow == nil {
				match := row
				catalogRow = &match
			}
		}
	}
	if exactMatches == 1 {
		return *exactName, PersistedIdentityName, nil
	}
	if exactMatches > 1 {
		return ProviderProfile{}, PersistedIdentityAmbiguous, fmt.Errorf("ambiguous provider name %q matches multiple persisted rows with the same name; remove one duplicate from config.json", identity)
	}
	if foldedMatches == 1 {
		return *foldedName, PersistedIdentityName, nil
	}
	if foldedMatches > 1 {
		return ProviderProfile{}, PersistedIdentityAmbiguous, fmt.Errorf("ambiguous provider name %q matches multiple persisted rows that differ only by case; use the exact spelling from config.json", identity)
	}
	if catalogMatches == 1 {
		return *catalogRow, PersistedIdentityCatalog, nil
	}
	if catalogMatches > 1 {
		return ProviderProfile{}, PersistedIdentityAmbiguous, fmt.Errorf("provider identity %q is ambiguous: %d saved profiles use it as a catalog id", identity, catalogMatches)
	}
	return ProviderProfile{}, PersistedIdentityNone, nil
}

// CatalogIdentityExclusive reports whether catalogID is claimed by the row
// named owner and by nothing else in the persisted config — neither as another
// row's name nor as another row's catalog id.
//
// Credential cleanup uses this before treating the catalog id as one of the
// target profile's own keys. Claims include provider rows and dictation because
// both use the same flat credential-store namespace. Any future feature that
// stores keys there must add its claims here too.
func CatalogIdentityExclusive(path, catalogID, owner string) (bool, error) {
	catalogID = strings.TrimSpace(catalogID)
	owner = strings.TrimSpace(owner)
	if catalogID == "" || owner == "" {
		return false, nil
	}
	cfg, err := persistedFileConfig(path)
	if err != nil {
		return false, err
	}
	if sameProviderIdentity(string(cfg.STT.Provider), catalogID) ||
		sameProviderIdentity(string(cfg.STT.StreamProvider), catalogID) {
		return false, nil
	}
	for _, row := range cfg.Providers {
		name := strings.TrimSpace(row.Name)
		if name == owner {
			continue
		}
		if sameProviderIdentity(name, catalogID) || sameProviderIdentity(row.CatalogID, catalogID) {
			return false, nil
		}
	}
	return true, nil
}

// ProviderCredentialCandidates returns every OAuth-store key that can belong
// exclusively to the addressed persisted profile. The requested spelling and
// canonical row name are included; a catalog id is included only when no sibling
// row can own a token under it. Auth status, refresh, and logout share this
// resolver so each command addresses the same stored login.
//
// The canonical name is returned separately for row-scoped API-key marker and
// key deletion; it is empty when no persisted row owns the address. On a config
// read error, callers still receive the requested spelling so logout can delete
// the OAuth credential it was explicitly asked to clear before reporting the error.
// An ambiguous provider identity is different: it returns no candidates at all,
// so a destructive caller cannot act on a spelling several profiles could own.
func ProviderCredentialCandidates(path, addressedName string) (candidates []string, canonicalName string, err error) {
	add := func(candidate string) {
		candidate = strings.TrimSpace(candidate)
		if candidate != "" && !slices.Contains(candidates, candidate) {
			candidates = append(candidates, candidate)
		}
	}
	canonicalName = strings.TrimSpace(addressedName)
	add(canonicalName)
	row, match, err := ResolvePersistedProviderIdentity(path, addressedName)
	if err != nil {
		if match == PersistedIdentityAmbiguous {
			return nil, canonicalName, err
		}
		return candidates, canonicalName, err
	}
	if match == PersistedIdentityNone {
		// Keep the requested spelling as an OAuth candidate, but return no
		// canonical row name: without a persisted row there is no API-key entry
		// this address is proven to own.
		return candidates, "", nil
	}
	canonicalName = strings.TrimSpace(row.Name)
	add(canonicalName)
	exclusive, err := CatalogIdentityExclusive(path, row.CatalogID, canonicalName)
	if err != nil {
		return candidates, canonicalName, err
	}
	if exclusive {
		add(row.CatalogID)
	}
	return candidates, canonicalName, nil
}

// PreflightProviderWrite also rejects a new spelling that would share a
// case-insensitive credential key with an existing persisted row.
func PreflightProviderWrite(path, name string) error {
	if err := PreflightUserConfig(path); err != nil {
		return err
	}
	providers, err := persistedProviders(path)
	if err != nil {
		return err
	}
	name = strings.TrimSpace(name)
	for _, provider := range providers {
		existing := strings.TrimSpace(provider.Name)
		if sameProviderIdentity(existing, name) && existing != name {
			return fmt.Errorf("provider %q already exists as %q; provider names must be unique case-insensitively", name, existing)
		}
	}
	return nil
}

// Windows reports a regular-file ancestor as path-not-found too. Only treat
// missing config as an empty provider list when its nearest existing ancestor
// is a directory; otherwise callers must report the persistence failure rather
// than silently treating a saved provider as session-only. This is diagnostic,
// not a containment check: writers still acquire their lock and open the path.
func persistedProviderReadError(path string, readErr error) error {
	if os.IsNotExist(readErr) {
		for parent := filepath.Dir(path); ; parent = filepath.Dir(parent) {
			info, err := os.Stat(parent)
			if err == nil {
				if !info.IsDir() {
					return fmt.Errorf("read config %s: ancestor %s is not a directory", path, parent)
				}
				return nil
			}
			if !os.IsNotExist(err) || filepath.Dir(parent) == parent {
				return fmt.Errorf("read config %s: inspect ancestor %s: %w", path, parent, err)
			}
		}
	}
	return fmt.Errorf("read config %s: %w", path, readErr)
}

func UpsertProvider(path string, profile ProviderProfile, setActive bool) (result FileConfig, err error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return FileConfig{}, fmt.Errorf("config path is required")
	}
	unlock, err := lockConfigFileFn(path)
	if err != nil {
		return FileConfig{}, err
	}
	// Joined, not chosen between: a release failure annotates the result
	// instead of masking the mutation error that actually explains what went
	// wrong. Reporting success after a failed unlock would claim a state the
	// next mutation cannot reproduce.
	defer func() { err = errors.Join(err, unlock()) }()
	return upsertProviderLocked(path, profile, setActive)
}

// upsertProviderLocked is UpsertProvider's body for a caller that already holds
// the config lock. EnsureCatalogProvider reads the document and then upserts
// into it, and the lock is not reentrant, so it must reach the work this way
// rather than through the exported function.
func upsertProviderLocked(path string, profile ProviderProfile, setActive bool) (FileConfig, error) {
	profile.Name = strings.TrimSpace(profile.Name)
	if profile.Name == "" {
		return FileConfig{}, fmt.Errorf("provider name is required")
	}

	cfg := FileConfig{}
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &cfg); err != nil {
			return FileConfig{}, fmt.Errorf("invalid config JSON %s: %w", path, err)
		}
	} else if !os.IsNotExist(err) {
		return FileConfig{}, fmt.Errorf("read config %s: %w", path, err)
	}
	if err := ValidatePersistedProviderNames(cfg); err != nil {
		return FileConfig{}, err
	}
	for _, existing := range cfg.Providers {
		if sameProviderIdentity(existing.Name, profile.Name) && strings.TrimSpace(existing.Name) != profile.Name {
			return FileConfig{}, fmt.Errorf("provider %q already exists as %q; provider names must be unique case-insensitively", profile.Name, existing.Name)
		}
	}

	mergeProvider(&cfg, profile)
	// mergeProfile deliberately ignores APIKeyStored — during resolve-time
	// layering a project config must not be able to claim the user's stored
	// keys. This user-config WRITE path re-applies the marker: capturing a key
	// via SecureProviderProfile onto a previously env/no-key profile must
	// persist apiKeyStored, or the secret sits in the credential store while
	// every ApplyStoredAPIKey gate skips it (PR #560 review).
	if profile.APIKeyStored {
		for index := range cfg.Providers {
			if cfg.Providers[index].Name == profile.Name {
				cfg.Providers[index].APIKeyStored = true
				break
			}
		}
	}
	if setActive || strings.TrimSpace(cfg.ActiveProvider) == "" {
		cfg.ActiveProvider = profile.Name
	}

	if err := writeConfigFile(path, cfg); err != nil {
		return FileConfig{}, err
	}
	return cfg, nil
}

// EnsuredProvider reports the outcome of EnsureCatalogProvider: the profile name
// that serves the catalog entry, whether it was newly created, and which provider
// is active after the call (unchanged unless it was blank).
type EnsuredProvider struct {
	Name    string
	Created bool
	Active  string
}

// EnsureCatalogProvider guarantees a provider profile exists in the config at
// path for the given catalog entry. OAuth login flows call this right after
// storing a token: a login is only reachable from the provider list and
// `zero providers use` when a profile exists, but a login must never replace or
// deactivate the user's current active provider — so an existing profile whose
// CatalogID already matches is left completely untouched (its name,
// credentials, and model are the user's), and a created profile is NOT marked
// active unless no provider was active at all.
//
// The one write onto an existing row is the catalog-id backfill for a legacy
// profile NAMED for the catalog provider that carries no catalog id: without it
// a config written before catalog ids existed dead-ends every login with an
// ownership error the user has no way to act on.
func EnsureCatalogProvider(path string, catalogID string) (result EnsuredProvider, err error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return EnsuredProvider{}, fmt.Errorf("config path is required")
	}
	descriptor, err := providercatalog.Require(catalogID)
	if err != nil {
		return EnsuredProvider{}, err
	}
	// One lock spans the existence scan AND the upsert: releasing between them
	// would let another process create the same catalog profile in the window,
	// and the second writer would clobber the first.
	unlock, err := lockConfigFileFn(path)
	if err != nil {
		return EnsuredProvider{}, err
	}
	// Joined, not chosen between: a release failure annotates the result
	// instead of masking the mutation error that actually explains what went
	// wrong. Reporting success after a failed unlock would claim a state the
	// next mutation cannot reproduce.
	defer func() { err = errors.Join(err, unlock()) }()

	cfg := FileConfig{}
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &cfg); err != nil {
			return EnsuredProvider{}, fmt.Errorf("invalid config JSON %s: %w", path, err)
		}
	} else if !os.IsNotExist(err) {
		return EnsuredProvider{}, fmt.Errorf("read config %s: %w", path, err)
	}
	if err := ValidatePersistedProviderNames(cfg); err != nil {
		return EnsuredProvider{}, err
	}
	owner, ownership, err := catalogProviderOwner(cfg.Providers, descriptor.ID)
	if err != nil {
		return EnsuredProvider{}, err
	}
	switch ownership {
	case catalogOwnershipOwned:
		return EnsuredProvider{Name: cfg.Providers[owner].Name, Active: cfg.ActiveProvider}, nil
	case catalogOwnershipAdoptable:
		// The row already carries this catalog spelling as its name and claims
		// no catalog id, which is how every config written before catalog ids
		// existed looks. Backfilling that one absent field is what makes the
		// row provably the owner; the name, credentials, base URL, and model
		// stay exactly as the user left them.
		cfg.Providers[owner].CatalogID = descriptor.ID
		if err := writeConfigFile(path, cfg); err != nil {
			return EnsuredProvider{}, err
		}
		return EnsuredProvider{Name: cfg.Providers[owner].Name, Active: cfg.ActiveProvider}, nil
	}

	profile := ProviderProfile{
		Name:         descriptor.ID,
		ProviderKind: providerKindForCatalogTransport(descriptor.Transport),
		CatalogID:    descriptor.ID,
		BaseURL:      descriptor.DefaultBaseURL,
		Model:        descriptor.DefaultModel,
	}
	written, err := upsertProviderLocked(path, profile, false)
	if err != nil {
		return EnsuredProvider{}, err
	}
	return EnsuredProvider{Name: profile.Name, Created: true, Active: written.ActiveProvider}, nil
}

// MarkProviderAPIKeyStored records that provider's API key now lives in the
// credential store. It also clears inline/env key fields so the stored key is the
// runtime credential; an old apiKeyEnv value must not keep overriding a freshly
// captured key from `zero auth openrouter` or provider setup.
func MarkProviderAPIKeyStored(path string, provider string) (err error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return fmt.Errorf("config path is required")
	}
	unlock, err := lockConfigFileFn(path)
	if err != nil {
		return err
	}
	// Joined, not chosen between: a release failure annotates the result
	// instead of masking the mutation error that actually explains what went
	// wrong. Reporting success after a failed unlock would claim a state the
	// next mutation cannot reproduce.
	defer func() { err = errors.Join(err, unlock()) }()
	provider = strings.TrimSpace(provider)
	if provider == "" {
		return fmt.Errorf("provider name is required")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read config %s: %w", path, err)
	}
	var cfg FileConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return fmt.Errorf("invalid config JSON %s: %w", path, err)
	}
	if err := ValidatePersistedProviderNames(cfg); err != nil {
		return err
	}
	for index := range cfg.Providers {
		if strings.TrimSpace(cfg.Providers[index].Name) == provider {
			cfg.Providers[index].APIKey = ""
			cfg.Providers[index].APIKeyEnv = ""
			cfg.Providers[index].APIKeyStored = true
			return writeConfigFile(path, cfg)
		}
	}
	return fmt.Errorf("provider %q not found", provider)
}

func SetActiveProvider(path string, name string) (result FileConfig, err error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return FileConfig{}, fmt.Errorf("config path is required")
	}
	unlock, err := lockConfigFileFn(path)
	if err != nil {
		return FileConfig{}, err
	}
	// Joined, not chosen between: a release failure annotates the result
	// instead of masking the mutation error that actually explains what went
	// wrong. Reporting success after a failed unlock would claim a state the
	// next mutation cannot reproduce.
	defer func() { err = errors.Join(err, unlock()) }()
	name = strings.TrimSpace(name)
	if name == "" {
		return FileConfig{}, fmt.Errorf("provider name is required")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return FileConfig{}, fmt.Errorf("read config %s: %w", path, err)
	}

	cfg := FileConfig{}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return FileConfig{}, fmt.Errorf("invalid config JSON %s: %w", path, err)
	}

	// Activation accepts any spelling that names this credential identity, then
	// records the row's own spelling so every later row-targeting mutator can
	// find it. ResolvePersistedProviderName is that bridge — see its doc for
	// why an ambiguous identity is an error rather than an arbitrary pick.
	resolved, err := resolvePersistedProviderName(cfg.Providers, name)
	if err != nil {
		return FileConfig{}, err
	}
	cfg.ActiveProvider = resolved
	if err := writeConfigFile(path, cfg); err != nil {
		return FileConfig{}, err
	}
	return cfg, nil
}

// SetActiveProviderModel persists a provider selection as one lock-held
// read-modify-write. The active provider and that provider's model are one UI
// choice; publishing them separately can leave a partially applied selection
// if another writer wins the lock between calls.
func SetActiveProviderModel(path string, name string, model string) (result FileConfig, err error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return FileConfig{}, fmt.Errorf("config path is required")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return FileConfig{}, fmt.Errorf("provider name is required")
	}
	model = strings.TrimSpace(model)
	if model == "" {
		return FileConfig{}, fmt.Errorf("model is required")
	}

	unlock, err := lockConfigFileFn(path)
	if err != nil {
		return FileConfig{}, err
	}
	defer func() { err = errors.Join(err, unlock()) }()

	data, err := os.ReadFile(path)
	if err != nil {
		return FileConfig{}, fmt.Errorf("read config %s: %w", path, err)
	}
	cfg := FileConfig{}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return FileConfig{}, fmt.Errorf("invalid config JSON %s: %w", path, err)
	}
	name, err = resolvePersistedProviderName(cfg.Providers, name)
	if err != nil {
		return FileConfig{}, err
	}
	for index := range cfg.Providers {
		if strings.TrimSpace(cfg.Providers[index].Name) == name {
			cfg.ActiveProvider = cfg.Providers[index].Name
			cfg.Providers[index].Model = model
			if err := writeConfigFile(path, cfg); err != nil {
				return FileConfig{}, err
			}
			return cfg, nil
		}
	}
	return FileConfig{}, fmt.Errorf("provider %q not found", name)
}

// ProviderPersisted reports whether a provider profile named name actually has
// a row in the config file at path. A provider can appear in the resolved/
// in-memory provider list without ever being written to config.json — e.g.
// applyProviderEnv synthesizes an "openai" profile purely from an ambient
// OPENAI_API_KEY environment variable on every Resolve() call, without ever
// persisting it. RemoveProvider/SetActiveProvider/SetProviderModel only ever
// look at what's on disk, so a caller offering to mutate a provider by name
// should check this first: "not on disk" needs different handling (nothing to
// persist/remove there) than a name that doesn't exist anywhere at all.
func ProviderPersisted(path string, name string) (bool, error) {
	path = strings.TrimSpace(path)
	name = strings.TrimSpace(name)
	if path == "" || name == "" {
		return false, nil
	}
	cfg, err := loadConfigFile(path)
	if err != nil {
		return false, err
	}
	for _, provider := range cfg.Providers {
		if sameProviderIdentity(provider.Name, name) {
			return true, nil
		}
	}
	return false, nil
}

// RemoveProvider deletes the named provider profile from the config at path.
// When the removed profile was active, activeProvider hands off to the first
// remaining provider (or clears when none remain) so the config never points at
// a profile that no longer exists. The caller owns cleaning up the credential
// store entry — config stays pure of secret I/O on the read path, and remove
// keeps that symmetry by only touching config.json.
func RemoveProvider(path string, name string) (result FileConfig, err error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return FileConfig{}, fmt.Errorf("config path is required")
	}
	unlock, err := lockConfigFileFn(path)
	if err != nil {
		return FileConfig{}, err
	}
	// Joined, not chosen between: a release failure annotates the result
	// instead of masking the mutation error that actually explains what went
	// wrong. Reporting success after a failed unlock would claim a state the
	// next mutation cannot reproduce.
	defer func() { err = errors.Join(err, unlock()) }()
	name = strings.TrimSpace(name)
	if name == "" {
		return FileConfig{}, fmt.Errorf("provider name is required")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return FileConfig{}, fmt.Errorf("read config %s: %w", path, err)
	}
	cfg := FileConfig{}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return FileConfig{}, fmt.Errorf("invalid config JSON %s: %w", path, err)
	}
	before := cfg
	before.Providers = append([]ProviderProfile(nil), cfg.Providers...)

	// Persisted provider identity is exact. Resolution may fold names from
	// runtime sources, but config mutations must target the requested row. This
	// lookup intentionally precedes validation so an exact removal can repair a
	// case-duplicate config; writeConfigFile validates the resulting config.
	index := -1
	for i, provider := range cfg.Providers {
		if strings.TrimSpace(provider.Name) == name {
			index = i
			break
		}
	}
	if index < 0 {
		return FileConfig{}, fmt.Errorf("provider %q not found", name)
	}
	activeIndex := -1
	activeFoldedIndex := -1
	activeFoldedMatches := 0
	for i, provider := range cfg.Providers {
		providerName := strings.TrimSpace(provider.Name)
		if providerName == strings.TrimSpace(cfg.ActiveProvider) {
			activeIndex = i
		}
		if sameProviderIdentity(providerName, cfg.ActiveProvider) {
			activeFoldedIndex = i
			activeFoldedMatches++
		}
	}
	removedWasActive := activeIndex == index || (activeIndex < 0 && activeFoldedMatches == 1 && activeFoldedIndex == index)
	cfg.Providers = append(cfg.Providers[:index], cfg.Providers[index+1:]...)
	if removedWasActive {
		cfg.ActiveProvider = ""
		if len(cfg.Providers) > 0 {
			cfg.ActiveProvider = cfg.Providers[0].Name
		}
	} else if active := strings.TrimSpace(cfg.ActiveProvider); active != "" {
		// Repairing a case-duplicate config can strand activeProvider on a third
		// spelling ("WoRk" with rows "work"/"WORK"): the pointer survived the
		// removal but now matches no remaining row exactly, so every exact
		// mutator fails until the user hand-edits config.json. Re-point it at the
		// survivor's own spelling when exactly one row still carries the
		// identity; leave it alone when it is still ambiguous or already exact.
		if resolved, resolveErr := resolvePersistedProviderName(cfg.Providers, active); resolveErr == nil {
			cfg.ActiveProvider = resolved
		}
	}
	if err := writeProviderNameRepair(path, before, cfg); err != nil {
		return FileConfig{}, err
	}
	return cfg, nil
}

// RenameProvider renames a provider profile, keeping everything keyed by the
// profile name consistent: the activeProvider pointer follows the rename, and a
// key in the encrypted credential store (APIKeyStored) is migrated to the new
// name BEFORE the config is rewritten — the store write must succeed first so a
// failed migration never strands the config pointing at a key that no longer
// resolves. OAuth tokens are deliberately not migrated: the runtime's login
// candidates fall back to the profile's CatalogID, which every OAuth-capable
// catalog profile carries, so a rename keeps the login reachable.
func RenameProvider(path string, oldName string, newName string) (result FileConfig, err error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return FileConfig{}, fmt.Errorf("config path is required")
	}
	unlock, err := lockConfigFileFn(path)
	if err != nil {
		return FileConfig{}, err
	}
	// Joined, not chosen between: a release failure annotates the result
	// instead of masking the mutation error that actually explains what went
	// wrong. Reporting success after a failed unlock would claim a state the
	// next mutation cannot reproduce.
	defer func() { err = errors.Join(err, unlock()) }()
	oldName = strings.TrimSpace(oldName)
	newName = strings.TrimSpace(newName)
	if oldName == "" || newName == "" {
		return FileConfig{}, fmt.Errorf("provider names are required")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return FileConfig{}, fmt.Errorf("read config %s: %w", path, err)
	}
	cfg := FileConfig{}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return FileConfig{}, fmt.Errorf("invalid config JSON %s: %w", path, err)
	}
	if err := ValidatePersistedProviderNames(cfg); err != nil {
		return FileConfig{}, err
	}

	// oldName is matched exactly, like ProviderPersisted/SetActiveProvider.
	// newName collides case-insensitively because the credential store retains
	// legacy case-insensitive keys.
	index := -1
	for i, provider := range cfg.Providers {
		providerName := strings.TrimSpace(provider.Name)
		if providerName == oldName {
			index = i
			continue
		}
		if sameProviderIdentity(providerName, newName) {
			return FileConfig{}, fmt.Errorf("provider %q already exists", newName)
		}
	}
	if index < 0 {
		return FileConfig{}, fmt.Errorf("provider %q not found", oldName)
	}
	if sameProviderIdentity(oldName, newName) && cfg.Providers[index].Name == newName {
		return cfg, nil
	}

	previousName := cfg.Providers[index].Name
	keyMigrated := false
	if cfg.Providers[index].APIKeyStored {
		if err := migrateStoredProviderKey(path, previousName, newName); err != nil {
			return FileConfig{}, fmt.Errorf("migrate stored key for %q: %w", oldName, err)
		}
		keyMigrated = true
	}
	if sameProviderIdentity(cfg.ActiveProvider, previousName) {
		cfg.ActiveProvider = newName
	}
	cfg.Providers[index].Name = newName
	if err := writeConfigFile(path, cfg); err != nil {
		if keyMigrated {
			// Compensate best-effort: config.json still names the OLD profile, so
			// move the key back where that config can find it — otherwise a failed
			// rewrite strands the key under a name no profile carries.
			_ = migrateStoredProviderKey(path, newName, previousName)
		}
		return FileConfig{}, err
	}
	return cfg, nil
}

// ProviderEdit is a field-level edit of one saved provider, applied by
// EditProvider in a single atomic write. Name is the CURRENT profile name
// (matched exactly); NewName renames (case-only renames included).
// Empty BaseURL/Model/APIKey mean "leave unchanged"; Description is applied
// VERBATIM (the editor always knows the full desired text, so clearing works).
type ProviderEdit struct {
	Name         string
	NewName      string
	BaseURL      string
	Model        string
	APIKey       string
	APIKeyStored bool
	Description  string
}

// EditProvider applies a provider edit in ONE read-modify-write: rename
// (activeProvider follows; a stored key migrates, with a best-effort rollback
// if the config write fails), field updates, the stored-key marker, and the
// verbatim description. A single write keeps the operation atomic — the
// previous rename+upsert+describe sequence could fail halfway and leave
// config.json renamed while every in-memory consumer still held the old name —
// and a case-only rename (groq -> Groq) remains an in-place update instead of
// an appended duplicate profile.
func EditProvider(path string, edit ProviderEdit) (result FileConfig, err error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return FileConfig{}, fmt.Errorf("config path is required")
	}
	unlock, err := lockConfigFileFn(path)
	if err != nil {
		return FileConfig{}, err
	}
	// Joined, not chosen between: a release failure annotates the result
	// instead of masking the mutation error that actually explains what went
	// wrong. Reporting success after a failed unlock would claim a state the
	// next mutation cannot reproduce.
	defer func() { err = errors.Join(err, unlock()) }()
	oldName := strings.TrimSpace(edit.Name)
	if oldName == "" {
		return FileConfig{}, fmt.Errorf("provider name is required")
	}
	newName := strings.TrimSpace(edit.NewName)
	if newName == "" {
		newName = oldName
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return FileConfig{}, fmt.Errorf("read config %s: %w", path, err)
	}
	cfg := FileConfig{}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return FileConfig{}, fmt.Errorf("invalid config JSON %s: %w", path, err)
	}

	if err := ValidatePersistedProviderNames(cfg); err != nil {
		return FileConfig{}, err
	}

	index := -1
	newIdentity := credstore.NormalizeProvider(newName)
	for i, provider := range cfg.Providers {
		providerName := strings.TrimSpace(provider.Name)
		if providerName == oldName {
			index = i
			continue
		}
		if credstore.NormalizeProvider(providerName) == newIdentity {
			return FileConfig{}, fmt.Errorf("provider %q already exists", newName)
		}
	}
	if index < 0 {
		return FileConfig{}, fmt.Errorf("provider %q not found", oldName)
	}

	previousName := cfg.Providers[index].Name
	renamed := previousName != newName
	keyMigrated := false
	// A rename moves the stored key along: either the profile's existing entry,
	// or a replacement key the caller just captured — the contract is that a
	// captured key is stored under the CURRENT name before EditProvider runs, so
	// one migration covers both. migrateStoredProviderKey no-ops on case-only
	// renames (the store normalizes names), so it cannot delete the key it just
	// moved.
	if renamed && (cfg.Providers[index].APIKeyStored || edit.APIKeyStored) {
		if err := migrateStoredProviderKey(path, previousName, newName); err != nil {
			return FileConfig{}, fmt.Errorf("migrate stored key for %q: %w", oldName, err)
		}
		keyMigrated = true
	}
	if renamed && sameProviderIdentity(cfg.ActiveProvider, previousName) {
		cfg.ActiveProvider = newName
	}

	profile := &cfg.Providers[index]
	profile.Name = newName
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
	}
	profile.Description = strings.TrimSpace(edit.Description)

	if err := writeConfigFile(path, cfg); err != nil {
		if keyMigrated {
			// Compensate best-effort: config.json still names the OLD profile, so
			// move the key back where that config can find it.
			_ = migrateStoredProviderKey(path, newName, previousName)
		}
		return FileConfig{}, err
	}
	return cfg, nil
}

// migrateStoredProviderKey moves a credential-store entry to a new provider
// name: write-new-then-delete-old, so an interruption can leave a duplicate but
// never a missing key. A missing source entry is a no-op (the marker may be
// stale); only a failed WRITE aborts the rename.
func migrateStoredProviderKey(configPath string, oldName string, newName string) error {
	// The store normalizes names case-insensitively, so a case-only rename
	// (groq -> Groq) targets ONE entry: Set(new) rewrites it in place and
	// Delete(old) would then remove the key that was just "moved". Nothing to
	// migrate — the existing entry already serves the new name.
	if sameProviderIdentity(oldName, newName) {
		return nil
	}
	store, err := ProviderKeyStoreAt(filepath.Dir(configPath))
	if err != nil {
		return err
	}
	key, ok, err := store.Get(oldName)
	if err != nil {
		return err
	}
	if !ok || strings.TrimSpace(key) == "" {
		return nil
	}
	if err := store.Set(newName, key); err != nil {
		return err
	}
	_, _ = store.Delete(oldName)
	return nil
}

func SetProviderModel(path string, name string, model string) (result FileConfig, err error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return FileConfig{}, fmt.Errorf("config path is required")
	}
	unlock, err := lockConfigFileFn(path)
	if err != nil {
		return FileConfig{}, err
	}
	// Joined, not chosen between: a release failure annotates the result
	// instead of masking the mutation error that actually explains what went
	// wrong. Reporting success after a failed unlock would claim a state the
	// next mutation cannot reproduce.
	defer func() { err = errors.Join(err, unlock()) }()
	name = strings.TrimSpace(name)
	if name == "" {
		return FileConfig{}, fmt.Errorf("provider name is required")
	}
	model = strings.TrimSpace(model)
	if model == "" {
		return FileConfig{}, fmt.Errorf("model is required")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return FileConfig{}, fmt.Errorf("read config %s: %w", path, err)
	}

	cfg := FileConfig{}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return FileConfig{}, fmt.Errorf("invalid config JSON %s: %w", path, err)
	}

	// Persisted provider identity is exact. Resolution may fold names from
	// runtime sources, but config mutations must target the requested row.
	for index := range cfg.Providers {
		if strings.TrimSpace(cfg.Providers[index].Name) == name {
			cfg.Providers[index].Model = model
			if err := writeConfigFile(path, cfg); err != nil {
				return FileConfig{}, err
			}
			return cfg, nil
		}
	}

	return FileConfig{}, fmt.Errorf("provider %q not found", name)
}

func SetFavoriteModels(path string, models []string) (result FileConfig, err error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return FileConfig{}, fmt.Errorf("config path is required")
	}
	unlock, err := lockConfigFileFn(path)
	if err != nil {
		return FileConfig{}, err
	}
	// Joined, not chosen between: a release failure annotates the result
	// instead of masking the mutation error that actually explains what went
	// wrong. Reporting success after a failed unlock would claim a state the
	// next mutation cannot reproduce.
	defer func() { err = errors.Join(err, unlock()) }()

	cfg := FileConfig{}
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &cfg); err != nil {
			return FileConfig{}, fmt.Errorf("invalid config JSON %s: %w", path, err)
		}
	} else if !os.IsNotExist(err) {
		return FileConfig{}, fmt.Errorf("read config %s: %w", path, err)
	}

	cfg.Preferences.FavoriteModels = normalizeFavoriteModels(models)
	if err := writeConfigFile(path, cfg); err != nil {
		return FileConfig{}, err
	}
	return cfg, nil
}

// SetRecentModels persists the automatic recent-model-switch history,
// mirroring SetFavoriteModels (read-modify-atomic-write). Unlike favorites,
// order is preserved (newest first) rather than sorted, since it reflects
// switch recency, not an alphabetical preference list.
func SetRecentModels(path string, entries []RecentModelEntry) (result FileConfig, err error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return FileConfig{}, fmt.Errorf("config path is required")
	}
	unlock, err := lockConfigFileFn(path)
	if err != nil {
		return FileConfig{}, err
	}
	// Joined, not chosen between: a release failure annotates the result
	// instead of masking the mutation error that actually explains what went
	// wrong. Reporting success after a failed unlock would claim a state the
	// next mutation cannot reproduce.
	defer func() { err = errors.Join(err, unlock()) }()

	cfg := FileConfig{}
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &cfg); err != nil {
			return FileConfig{}, fmt.Errorf("invalid config JSON %s: %w", path, err)
		}
	} else if !os.IsNotExist(err) {
		return FileConfig{}, fmt.Errorf("read config %s: %w", path, err)
	}

	cfg.Preferences.RecentModels = NormalizeRecentModels(entries)
	if err := writeConfigFile(path, cfg); err != nil {
		return FileConfig{}, err
	}
	return cfg, nil
}

// SetRecapsEnabled persists the idle recap preference, mirroring
// SetFavoriteModels (read-modify-atomic-write).
func SetRecapsEnabled(path string, enabled bool) (result FileConfig, err error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return FileConfig{}, fmt.Errorf("config path is required")
	}
	unlock, err := lockConfigFileFn(path)
	if err != nil {
		return FileConfig{}, err
	}
	// Joined, not chosen between: a release failure annotates the result
	// instead of masking the mutation error that actually explains what went
	// wrong. Reporting success after a failed unlock would claim a state the
	// next mutation cannot reproduce.
	defer func() { err = errors.Join(err, unlock()) }()
	cfg := FileConfig{}
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &cfg); err != nil {
			return FileConfig{}, fmt.Errorf("invalid config JSON %s: %w", path, err)
		}
	} else if !os.IsNotExist(err) {
		return FileConfig{}, fmt.Errorf("read config %s: %w", path, err)
	}
	v := enabled
	cfg.Preferences.Recaps = &v
	if err := writeConfigFile(path, cfg); err != nil {
		return FileConfig{}, err
	}
	return cfg, nil
}

// SetTheme persists the TUI theme preference, mirroring SetFavoriteModels
// (read-modify-atomic-write). A blank theme clears the stored preference.
func SetTheme(path string, theme string) (result FileConfig, err error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return FileConfig{}, fmt.Errorf("config path is required")
	}
	unlock, err := lockConfigFileFn(path)
	if err != nil {
		return FileConfig{}, err
	}
	// Joined, not chosen between: a release failure annotates the result
	// instead of masking the mutation error that actually explains what went
	// wrong. Reporting success after a failed unlock would claim a state the
	// next mutation cannot reproduce.
	defer func() { err = errors.Join(err, unlock()) }()
	cfg := FileConfig{}
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &cfg); err != nil {
			return FileConfig{}, fmt.Errorf("invalid config JSON %s: %w", path, err)
		}
	} else if !os.IsNotExist(err) {
		return FileConfig{}, fmt.Errorf("read config %s: %w", path, err)
	}
	cfg.Preferences.Theme = strings.TrimSpace(theme)
	if err := writeConfigFile(path, cfg); err != nil {
		return FileConfig{}, err
	}
	return cfg, nil
}

// UserNotify returns the notify block stored in the user's own config file at
// path, trimmed. A missing file or missing/empty block returns the zero value:
// callers that need to preserve "whatever the user already chose" on a partial
// update must seed from THIS value, not from the resolved view — the resolver
// merges project config and the TUI applies its own defaults, so seeding from
// resolved copies choices the user never made into their global file (a
// project's mode: off, or a pinned default) and across every other project.
func UserNotify(path string) (NotifyConfig, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return NotifyConfig{}, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return NotifyConfig{}, nil
		}
		return NotifyConfig{}, fmt.Errorf("read config %s: %w", path, err)
	}
	var cfg FileConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return NotifyConfig{}, fmt.Errorf("invalid config JSON %s: %w", path, err)
	}
	cfg.Notify.Mode = strings.TrimSpace(cfg.Notify.Mode)
	cfg.Notify.FocusMode = strings.TrimSpace(cfg.Notify.FocusMode)
	return cfg.Notify, nil
}

// validateNotify trims both fields and checks them against the accepted vocab
// (mode in {off,bell,notify,both}; focusMode in {unfocused,always,focused}) so
// a bad caller cannot write a value the resolver would later reject at
// startup. An empty Mode or FocusMode is stored as-is — blank means "use the
// built-in defaults" (the TUI's effectiveTUINotifyMode maps an empty mode to
// both; the notify package treats an empty focusMode as unfocused), not "off".
func validateNotify(value NotifyConfig) (NotifyConfig, error) {
	value.Mode = strings.TrimSpace(value.Mode)
	value.FocusMode = strings.TrimSpace(value.FocusMode)
	if mode := value.Mode; mode != "" {
		switch notify.Mode(mode) {
		case notify.ModeOff, notify.ModeBell, notify.ModeNotify, notify.ModeBoth:
		default:
			return NotifyConfig{}, fmt.Errorf("invalid notify.mode %q: expected off, bell, notify, or both", mode)
		}
	}
	if focus := value.FocusMode; focus != "" {
		switch notify.FocusMode(focus) {
		case notify.FocusUnfocused, notify.FocusAlways, notify.FocusFocused:
		default:
			return NotifyConfig{}, fmt.Errorf("invalid notify.focusMode %q: expected unfocused, always, or focused", focus)
		}
	}
	return value, nil
}

// UpdateNotify runs one SERIALIZED read-modify-write transaction on the user's
// notify block. merge receives the currently stored block and returns the
// block to store; it must preserve any field the caller is not explicitly
// changing. The lock covers reading the stored block, applying merge, and
// replacing the file, so two concurrent partial updates (for example
// `--mode off` and `--focus always` from two terminals) cannot interleave
// their reads and writes and silently undo each other's explicit change
// (maintainer review, PR #1001 reproduced a 50/50 lost-update without it).
//
// The write replaces ONLY the notify member's bytes (setNotifyJSONObject),
// preserving every unrelated value and its explicit presence — including
// explicit zeros/falses like tools.deferThreshold: 0 or an MCP server's
// disabled: false, which the typed serializer's omitempty cannot round-trip —
// through the existing temp-file-and-rename atomic publish. Locking only the
// final write would leave the stale-field merge outside the transaction, so
// the whole sequence runs under one lock — the SAME lockConfigFileFn authority
// every other config mutator uses (provider/theme/credential/MCP writes), not
// a private lock: two authorities over the same file would not contend
// (maintainer review, PR #1001). The release error is joined into the
// result like every other writer: a failed unlock would leave the advisory
// lock held and later updates blocked while the caller is told success.
func UpdateNotify(path string, merge func(current NotifyConfig) NotifyConfig) (result NotifyConfig, err error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return NotifyConfig{}, fmt.Errorf("config path is required")
	}
	if merge == nil {
		return NotifyConfig{}, fmt.Errorf("merge function is required")
	}
	release, err := lockConfigFileFn(path)
	if err != nil {
		return NotifyConfig{}, err
	}
	// Joined, not chosen between: a release failure annotates the result
	// instead of masking the mutation error that actually explains what went
	// wrong. Reporting success after a failed release would claim a state the
	// next update cannot reproduce.
	defer func() { err = errors.Join(err, release()) }()

	current, err := UserNotify(path)
	if err != nil {
		return NotifyConfig{}, err
	}
	next, err := validateNotify(merge(current))
	if err != nil {
		return NotifyConfig{}, err
	}

	// Read the raw file and edit only the notify member. A missing file starts
	// from an empty object so a first-time write does not need a pre-seeded
	// config.
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return NotifyConfig{}, fmt.Errorf("read config %s: %w", path, err)
		}
		data = []byte("{}")
	}
	updated, err := setNotifyJSONObject(data, next)
	if err != nil {
		return NotifyConfig{}, fmt.Errorf("invalid config JSON %s: %w", path, err)
	}
	if err := writeConfigData(path, updated); err != nil {
		return NotifyConfig{}, err
	}
	return next, nil
}

// SetPet persists only the terminal-pet preference while preserving every
// unrelated user setting through the config writer's atomic replace path.
func SetPet(path string, pet string) (result FileConfig, err error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return FileConfig{}, fmt.Errorf("config path is required")
	}
	unlock, err := lockConfigFileFn(path)
	if err != nil {
		return FileConfig{}, err
	}
	// Joined, not chosen between: a release failure annotates the result
	// instead of masking the mutation error that actually explains what went
	// wrong. Reporting success after a failed unlock would claim a state the
	// next mutation cannot reproduce.
	defer func() { err = errors.Join(err, unlock()) }()
	cfg := FileConfig{}
	data := []byte("{}")
	if existing, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(existing, &cfg); err != nil {
			return FileConfig{}, fmt.Errorf("invalid config JSON %s: %w", path, err)
		}
		data = existing
	} else if !os.IsNotExist(err) {
		return FileConfig{}, fmt.Errorf("read config %s: %w", path, err)
	}
	pet = strings.TrimSpace(pet)
	cfg.Preferences.Pet = pet
	data, err = setPetPreferenceJSON(data, pet)
	if err != nil {
		return FileConfig{}, fmt.Errorf("invalid config JSON %s: %w", path, err)
	}
	if err := writeConfigData(path, data); err != nil {
		return FileConfig{}, err
	}
	return cfg, nil
}

// SetSTTModel persists the dictation model and its provider, mirroring
// SetTheme (read-modify-atomic-write). provider must be one of the known STT
// provider kinds; a local provider stores the model as stt.localModelPath,
// otherwise as stt.model. A blank model clears the stored value for that slot.
func SetSTTModel(path string, provider STTProviderKind, model string) (result FileConfig, err error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return FileConfig{}, fmt.Errorf("config path is required")
	}
	unlock, err := lockConfigFileFn(path)
	if err != nil {
		return FileConfig{}, err
	}
	// Joined, not chosen between: a release failure annotates the result
	// instead of masking the mutation error that actually explains what went
	// wrong. Reporting success after a failed unlock would claim a state the
	// next mutation cannot reproduce.
	defer func() { err = errors.Join(err, unlock()) }()
	cfg := FileConfig{}
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &cfg); err != nil {
			return FileConfig{}, fmt.Errorf("invalid config JSON %s: %w", path, err)
		}
	} else if !os.IsNotExist(err) {
		return FileConfig{}, fmt.Errorf("read config %s: %w", path, err)
	}
	if provider != "" {
		cfg.STT.Provider = provider
	}
	model = strings.TrimSpace(model)
	if cfg.STT.STTProvider() == STTProviderLocal {
		cfg.STT.LocalModelPath = model
	} else {
		cfg.STT.Model = model
	}
	if err := validateSTTConfig(cfg.STT); err != nil {
		return FileConfig{}, err
	}
	if err := writeConfigFile(path, cfg); err != nil {
		return FileConfig{}, err
	}
	return cfg, nil
}

// SetSTTLocalEngine persists the paths of an auto-downloaded local engine +
// model and switches dictation to the local provider, mirroring SetTheme
// (read-modify-atomic-write). streaming selects the pipeline matching the
// downloaded model (a streaming transducer vs a batch model). Called after a
// download completes.
func SetSTTLocalEngine(path, binary, serverBinary, modelPath string, streaming bool) (result FileConfig, err error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return FileConfig{}, fmt.Errorf("config path is required")
	}
	unlock, err := lockConfigFileFn(path)
	if err != nil {
		return FileConfig{}, err
	}
	// Joined, not chosen between: a release failure annotates the result
	// instead of masking the mutation error that actually explains what went
	// wrong. Reporting success after a failed unlock would claim a state the
	// next mutation cannot reproduce.
	defer func() { err = errors.Join(err, unlock()) }()
	cfg := FileConfig{}
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &cfg); err != nil {
			return FileConfig{}, fmt.Errorf("invalid config JSON %s: %w", path, err)
		}
	} else if !os.IsNotExist(err) {
		return FileConfig{}, fmt.Errorf("read config %s: %w", path, err)
	}
	cfg.STT.Provider = STTProviderLocal
	// Point BOTH pipelines at the local engine. Without resetting StreamProvider, a
	// previously-chosen cloud value (deepgram/openai) would still win in
	// buildStreamingTranscriber, so the live transcript would keep hitting the cloud
	// after the user switched to a downloaded local model.
	cfg.STT.StreamProvider = STTProviderLocal
	cfg.STT.LocalBinary = strings.TrimSpace(binary)
	cfg.STT.LocalServerBinary = strings.TrimSpace(serverBinary)
	cfg.STT.LocalModelPath = strings.TrimSpace(modelPath)
	// Match the pipeline to the downloaded model: a streaming transducer drives
	// the websocket server for a live transcript; a batch model uses the offline
	// binary.
	s := streaming
	cfg.STT.Streaming = &s
	if err := validateSTTConfig(cfg.STT); err != nil {
		return FileConfig{}, err
	}
	if err := writeConfigFile(path, cfg); err != nil {
		return FileConfig{}, err
	}
	return cfg, nil
}

// SetSTTProvider persists just the dictation batch provider, mirroring SetTheme.
func SetSTTProvider(path string, provider STTProviderKind) (result FileConfig, err error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return FileConfig{}, fmt.Errorf("config path is required")
	}
	unlock, err := lockConfigFileFn(path)
	if err != nil {
		return FileConfig{}, err
	}
	// Joined, not chosen between: a release failure annotates the result
	// instead of masking the mutation error that actually explains what went
	// wrong. Reporting success after a failed unlock would claim a state the
	// next mutation cannot reproduce.
	defer func() { err = errors.Join(err, unlock()) }()
	cfg := FileConfig{}
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &cfg); err != nil {
			return FileConfig{}, fmt.Errorf("invalid config JSON %s: %w", path, err)
		}
	} else if !os.IsNotExist(err) {
		return FileConfig{}, fmt.Errorf("read config %s: %w", path, err)
	}
	cfg.STT.Provider = provider
	if err := validateSTTConfig(cfg.STT); err != nil {
		return FileConfig{}, err
	}
	if err := writeConfigFile(path, cfg); err != nil {
		return FileConfig{}, err
	}
	return cfg, nil
}

func normalizeFavoriteModels(models []string) []string {
	seen := map[string]bool{}
	favorites := make([]string, 0, len(models))
	for _, model := range models {
		model = strings.TrimSpace(model)
		if model == "" || seen[model] {
			continue
		}
		seen[model] = true
		favorites = append(favorites, model)
	}
	sort.Strings(favorites)
	return favorites
}

// NormalizeRecentModels trims, drops entries with no model id, de-duplicates
// by provider+model pair (keeping the first/newest occurrence), and caps the
// result to MaxRecentModels. Order is preserved — the caller is responsible
// for passing entries newest-first. Exported so callers outside this package
// (e.g. the TUI, which keeps its own in-memory copy of recent history) apply
// the exact same normalization rules as the persisted config, instead of
// maintaining a second, independently-drifting copy of this logic.
func NormalizeRecentModels(entries []RecentModelEntry) []RecentModelEntry {
	seen := map[string]bool{}
	recent := make([]RecentModelEntry, 0, len(entries))
	for _, entry := range entries {
		provider := strings.TrimSpace(entry.Provider)
		model := strings.TrimSpace(entry.Model)
		if model == "" {
			continue
		}
		key := strings.ToLower(provider) + "\x00" + model
		if seen[key] {
			continue
		}
		seen[key] = true
		recent = append(recent, RecentModelEntry{Provider: provider, Model: model})
		if len(recent) >= MaxRecentModels {
			break
		}
	}
	return recent
}

func writeConfigFile(path string, cfg FileConfig) error {
	if err := ValidatePersistedProviderNames(cfg); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("encode config JSON: %w", err)
	}
	return writeConfigData(path, data)
}

// Sync seams allow tests to inject persistence failures at each barrier.
var syncConfigFileFn = (*os.File).Sync
var syncConfigDirFn = syncConfigDir

// Windows cannot sync directory handles, so rename durability is best effort
// there, matching the sessions store. Elsewhere a directory the user can write
// but not read cannot be opened for syncing either; that is also best effort,
// because the write it follows has already succeeded. Every other failure to
// open, sync, or close the directory is reported: the barrier was requested and
// not reached, so the caller must not acknowledge the save.
func syncConfigDir(dir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	d, err := os.Open(dir)
	if err != nil {
		if errors.Is(err, os.ErrPermission) {
			return nil
		}
		return err
	}
	return errors.Join(d.Sync(), d.Close())
}

// ensureConfigDir is the ONE path that creates the config directory, shared by
// lockConfigFile and writeConfigData. Every exported mutator locks before it
// writes, and the lock file is a sibling of the config, so in practice the lock
// path is what first creates the directory; a barrier that lived only in
// writeConfigData would find nothing missing by then and never run.
//
// Each missing component is created with os.Mkdir rather than os.MkdirAll so
// the call knows exactly which entries it created, then each new entry is
// synced into its parent (deepest first) so a crash cannot lose the directory,
// and with it the config, after a successful write. A component that appeared
// between the lookup and the Mkdir belongs to another writer: it is not rolled
// back, but its parent is still synced, because this write depends on the
// entry being durable whoever created it.
//
// If a barrier fails, the directories this call created are removed again.
// Leaving them would let a retry find nothing missing, skip the barrier, and
// report a durability it never established. Only what this call created is
// removed, with os.Remove so a directory another process has since populated
// stays; a failed rollback is joined into the error, never swallowed.
func ensureConfigDir(dir string) error {
	if dir == "." || dir == "" {
		return nil
	}
	missing := missingConfigDirs(dir)
	if len(missing) == 0 {
		return nil
	}
	var created []string
	for i := len(missing) - 1; i >= 0; i-- {
		d := missing[i]
		err := os.Mkdir(d, 0o700)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return errors.Join(fmt.Errorf("create config directory %s: %w", d, err), removeConfigDirs(created))
		}
		created = append(created, d)
	}
	for _, d := range missing {
		parent := filepath.Dir(d)
		if err := syncConfigDirFn(parent); err != nil {
			return errors.Join(fmt.Errorf("sync config directory %s: %w", parent, err), removeConfigDirs(created))
		}
	}
	return nil
}

// removeConfigDirs rolls back directories ensureConfigDir created in this
// call, deepest first. os.Remove refuses a non-empty directory, so an entry
// another process already populated survives and its error is reported.
func removeConfigDirs(created []string) error {
	var errs []error
	for i := len(created) - 1; i >= 0; i-- {
		if err := os.Remove(created[i]); err != nil {
			errs = append(errs, fmt.Errorf("roll back config directory %s: %w", created[i], err))
		}
	}
	return errors.Join(errs...)
}

// missingConfigDirs lists dir and each ancestor that does not exist yet,
// deepest first, so ensureConfigDir can create them shallowest first and sync
// each new entry into its parent deepest first.
func missingConfigDirs(dir string) []string {
	var missing []string
	for d := dir; ; {
		if _, err := os.Lstat(d); !errors.Is(err, os.ErrNotExist) {
			return missing
		}
		missing = append(missing, d)
		parent := filepath.Dir(d)
		if parent == d {
			return missing
		}
		d = parent
	}
}

func writeConfigData(path string, data []byte) error {
	dir := filepath.Dir(path)
	// Usually a no-op: the lock path has already created and synced the
	// directory. It still matters for the rare unlocked caller and for a
	// directory removed between the lock and the write.
	if err := ensureConfigDir(dir); err != nil {
		return err
	}
	if len(data) == 0 || data[len(data)-1] != '\n' {
		data = append(data, '\n')
	}
	// Write-to-temp + rename: an in-place write interrupted mid-way (crash,
	// disk full) would leave the user's only config truncated or corrupt.
	tmp, err := os.CreateTemp(dir, ".zero-config-*.tmp")
	if err != nil {
		return fmt.Errorf("write config %s: %w", path, err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("secure config permissions %s: %w", path, err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write config %s: %w", path, err)
	}
	// Persist the complete contents before making the replacement visible.
	if err := syncConfigFileFn(tmp); err != nil {
		return fmt.Errorf("sync config %s: %w", path, errors.Join(err, tmp.Close()))
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write config %s: %w", path, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("write config %s: %w", path, err)
	}
	// The replacement is already visible if this fails; do not roll it back.
	if err := syncConfigDirFn(dir); err != nil {
		return fmt.Errorf("sync config directory %s: %w", dir, err)
	}
	return nil
}
