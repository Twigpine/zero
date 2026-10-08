package oauth

import (
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// macOSLikeBudget is the real macOS ceiling: `security -i` reads at most 4095
// bytes of command line, and the fake charges the account name against it the
// way the real command line does.
const macOSLikeBudget = 4095

// bigToken builds a credential the size of a real OIDC login (a JWT access
// token, an ID token, and an opaque refresh token), which is what pushes the
// combined blob past a single keychain entry.
func bigToken(seed string) Token {
	return Token{
		AccessToken:  strings.Repeat(seed, 1200),
		IDToken:      strings.Repeat(seed, 900),
		RefreshToken: strings.Repeat(seed, 300),
		TokenType:    "Bearer",
		Scopes:       []string{"openid", "profile", "email", "offline_access"},
		ExpiresAt:    time.Unix(1_800_000_000, 0).UTC(),
		Account:      seed + "@example.com",
	}
}

func newCappedKeyringStore(t *testing.T, kr KeyringClient) *Store {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	s, err := NewStore(StoreOptions{Storage: "keyring", Keyring: kr})
	if err != nil {
		t.Fatalf("NewStore(keyring): %v", err)
	}
	return s
}

func mustSave(t *testing.T, s *Store, name string, token Token) {
	t.Helper()
	if err := s.Save(ProviderKey(name), token); err != nil {
		t.Fatalf("Save(%s): %v", name, err)
	}
}

func mustLoad(t *testing.T, s *Store, name string) Token {
	t.Helper()
	token, ok, err := s.Load(ProviderKey(name))
	if err != nil {
		t.Fatalf("Load(%s): %v", name, err)
	}
	if !ok {
		t.Fatalf("Load(%s): no token stored", name)
	}
	return token
}

func manifestOf(t *testing.T, kr *fakeKR) keyringManifest {
	t.Helper()
	head, ok, _ := kr.Get(keyringService, keyringAccount)
	if !ok {
		t.Fatal("no anchor entry stored")
	}
	if !strings.HasPrefix(head, keyringManifestPrefix) {
		t.Fatalf("anchor entry is a whole blob, not a manifest: %.32s...", head)
	}
	manifest, err := parseKeyringManifest(head)
	if err != nil {
		t.Fatalf("parseKeyringManifest(%q): %v", head, err)
	}
	return manifest
}

// TestStoreKeyringSavesSecondLoginOverEntryLimit is the regression: two OIDC
// logins do not fit one macOS keychain entry, and storing the blob as a single
// entry made the second Save fail outright with nothing persisted.
func TestStoreKeyringSavesSecondLoginOverEntryLimit(t *testing.T) {
	kr := newCappedFakeKR(macOSLikeBudget)
	s := newCappedKeyringStore(t, kr)

	mustSave(t, s, "first", bigToken("a"))
	mustSave(t, s, "second", bigToken("b"))

	if got := mustLoad(t, s, "first"); got.AccessToken != bigToken("a").AccessToken {
		t.Error("first login did not survive the second save")
	}
	if got := mustLoad(t, s, "second"); got.AccessToken != bigToken("b").AccessToken {
		t.Error("second login did not round-trip")
	}

	manifest := manifestOf(t, kr)
	if manifest.counts[manifest.live] < 2 {
		t.Fatalf("blob fit in %d chunk(s); the test no longer exercises splitting", manifest.counts[manifest.live])
	}
	for _, account := range kr.chunkAccounts(manifest.live) {
		if got := len(kr.data[keyringService+"/"+account]); got > macOSLikeBudget-len(account) {
			t.Errorf("chunk %s is %d bytes, over its own entry budget", account, got)
		}
	}
}

// TestStoreKeyringChunkSizingSurvivesLongerChunkAccounts pins the reason chunks
// are sized against the highest possible index: on macOS the account shares the
// command line with the secret, so a budget taken from chunk 0 overflows once
// the index grows a digit.
func TestStoreKeyringChunkSizingSurvivesLongerChunkAccounts(t *testing.T) {
	// A tiny budget forces enough chunks that the index reaches two digits.
	kr := newCappedFakeKR(keyringMinChunkLen + len(keyringAccount) + 8)
	s := newCappedKeyringStore(t, kr)

	mustSave(t, s, "first", bigToken("a"))

	manifest := manifestOf(t, kr)
	if manifest.counts[manifest.live] < 10 {
		t.Fatalf("only %d chunks; the test needs a two-digit index", manifest.counts[manifest.live])
	}
	if got := mustLoad(t, s, "first"); got.AccessToken != bigToken("a").AccessToken {
		t.Error("blob did not round-trip across two-digit chunk indices")
	}
}

// TestStoreKeyringChunkedReadRejectsMissingChunk covers a torn read: a chunk the
// manifest names is gone, which must be reported rather than decoded as a
// truncated blob.
func TestStoreKeyringChunkedReadRejectsMissingChunk(t *testing.T) {
	kr := newCappedFakeKR(macOSLikeBudget)
	s := newCappedKeyringStore(t, kr)
	mustSave(t, s, "first", bigToken("a"))
	mustSave(t, s, "second", bigToken("b"))

	manifest := manifestOf(t, kr)
	accounts := kr.chunkAccounts(manifest.live)
	delete(kr.data, keyringService+"/"+accounts[len(accounts)-1])

	_, _, err := s.Load(ProviderKey("first"))
	if err == nil || !strings.Contains(err.Error(), "missing chunk") {
		t.Fatalf("Load after losing a chunk: err = %v, want a missing-chunk error", err)
	}
}

// TestStoreKeyringChunkedReadRejectsTruncatedChunk covers the corruption that
// motivated chunking in the first place: `security -i` splits an overlong line
// into two commands instead of refusing it, so a chunk can come back short. The
// digest has to catch that even when the result is still valid base64.
func TestStoreKeyringChunkedReadRejectsTruncatedChunk(t *testing.T) {
	kr := newCappedFakeKR(macOSLikeBudget)
	s := newCappedKeyringStore(t, kr)
	mustSave(t, s, "first", bigToken("a"))
	mustSave(t, s, "second", bigToken("b"))

	manifest := manifestOf(t, kr)
	account := kr.chunkAccounts(manifest.live)[0]
	key := keyringService + "/" + account
	// Drop a whole base64 quantum so the concatenation still decodes cleanly.
	kr.data[key] = kr.data[key][:len(kr.data[key])-4]

	_, _, err := s.Load(ProviderKey("first"))
	if err == nil {
		t.Fatal("Load of a truncated chunk succeeded; the digest did not catch it")
	}
	if !strings.Contains(err.Error(), "integrity check") && !strings.Contains(err.Error(), "invalid token store") {
		t.Fatalf("Load of a truncated chunk: err = %v, want an integrity or parse failure", err)
	}
}

// TestStoreKeyringWriteCommitsOnlyAtTheManifest asserts the commit point: a
// write that dies while filling chunks must leave the previous generation
// readable, because the manifest still names it.
func TestStoreKeyringWriteCommitsOnlyAtTheManifest(t *testing.T) {
	kr := newCappedFakeKR(macOSLikeBudget)
	s := newCappedKeyringStore(t, kr)
	mustSave(t, s, "first", bigToken("a"))
	mustSave(t, s, "second", bigToken("b"))

	before := manifestOf(t, kr)
	boom := errors.New("keychain is locked")
	// Fail the second chunk of whichever generation the next write targets, so
	// the write dies partway through filling it.
	kr.failSet = func(account string) error {
		if strings.HasSuffix(account, ".1") {
			return boom
		}
		return nil
	}

	if err := s.Save(ProviderKey("third"), bigToken("c")); !errors.Is(err, boom) {
		t.Fatalf("Save with a failing chunk write: err = %v, want %v", err, boom)
	}

	kr.failSet = nil
	if got := mustLoad(t, s, "first"); got.AccessToken != bigToken("a").AccessToken {
		t.Error("a failed write damaged the committed blob")
	}
	if _, ok, _ := s.Load(ProviderKey("third")); ok {
		t.Error("a write that never reached the manifest was visible anyway")
	}
	if after := manifestOf(t, kr); after.live != before.live || after.digest != before.digest {
		t.Errorf("manifest moved on a failed write: %+v -> %+v", before, after)
	}
}

// TestStoreKeyringWriteFailsOnFinalManifestPublication asserts that when the
// final manifest publication fails after all chunk writes succeed, the previous
// manifest and committed blob remain readable, the new login is not visible,
// and the target generation's written accounts remain tracked for cleanup.
func TestStoreKeyringWriteFailsOnFinalManifestPublication(t *testing.T) {
	kr := newCappedFakeKR(macOSLikeBudget)
	s := newCappedKeyringStore(t, kr)
	mustSave(t, s, "first", bigToken("a"))
	mustSave(t, s, "second", bigToken("b"))

	before := manifestOf(t, kr)
	boom := errors.New("keychain is locked on manifest publish")

	// Allow reservation and chunk writes to succeed, but fail when publishing
	// the final manifest with next.live.
	anchorSets := 0
	kr.failSet = func(account string) error {
		if account == keyringAccount {
			anchorSets++
			if anchorSets > 1 {
				return boom
			}
		}
		return nil
	}

	if err := s.Save(ProviderKey("third"), bigToken("c")); !errors.Is(err, boom) {
		t.Fatalf("Save with failing final manifest publication: err = %v, want %v", err, boom)
	}
	kr.failSet = nil
	if anchorSets != 2 {
		t.Errorf("anchor writes = %d, want 2", anchorSets)
	}
	if kr.sets[keyringAccount] < 1 {
		t.Errorf("successful anchor sets = %d, want at least 1", kr.sets[keyringAccount])
	}

	if got := mustLoad(t, s, "first"); got.AccessToken != bigToken("a").AccessToken {
		t.Error("a failed final manifest publication damaged the committed blob")
	}
	if _, ok, _ := s.Load(ProviderKey("third")); ok {
		t.Error("a write that failed final manifest publication was visible anyway")
	}

	after := manifestOf(t, kr)
	if after.live != before.live {
		t.Fatalf("live generation moved on failed final manifest publication: %q -> %q", before.live, after.live)
	}
	assertNoStrayChunks(t, kr, after)

	// A subsequent write successfully cleans up the orphaned generation chunks
	mustSave(t, s, "fourth", bigToken("d"))
	assertNoStrayChunks(t, kr, manifestOf(t, kr))
}

// TestStoreKeyringAlternatesGenerationsAndRetiresTheOld covers the ping-pong:
// each write lands in the generation that is not live, and the retired one is
// removed so a previous login's tokens do not linger in the keychain.
func TestStoreKeyringAlternatesGenerationsAndRetiresTheOld(t *testing.T) {
	kr := newCappedFakeKR(macOSLikeBudget)
	s := newCappedKeyringStore(t, kr)
	mustSave(t, s, "first", bigToken("a"))
	mustSave(t, s, "second", bigToken("b"))

	seen := map[string]bool{}
	for round := range 3 {
		mustSave(t, s, "second", bigToken(string(rune('c'+round))))
		manifest := manifestOf(t, kr)
		seen[manifest.live] = true

		retired := keyringChunkFamilyA
		if manifest.live == keyringChunkFamilyA {
			retired = keyringChunkFamilyB
		}
		if left := kr.chunkAccounts(retired); len(left) != 0 {
			t.Errorf("round %d: retired generation %q still holds %v", round, retired, left)
		}
		// The retired count deliberately stays in the manifest. Over-stating is
		// the safe direction, so the invariant to hold is the one-sided one.
		assertNoStrayChunks(t, kr, manifest)
	}
	if len(seen) != 2 {
		t.Errorf("writes stayed in generation(s) %v; they must alternate", seen)
	}
}

// TestStoreKeyringGrowsIntoChunksAndShrinksBack covers both layout transitions,
// including that shrinking below the cap removes the chunks rather than
// stranding a logged-out provider's tokens in the keychain.
func TestStoreKeyringGrowsIntoChunksAndShrinksBack(t *testing.T) {
	kr := newCappedFakeKR(macOSLikeBudget)
	s := newCappedKeyringStore(t, kr)

	mustSave(t, s, "small", Token{AccessToken: "short", RefreshToken: "r"})
	if head, _, _ := kr.Get(keyringService, keyringAccount); strings.HasPrefix(head, keyringManifestPrefix) {
		t.Fatal("a blob that fits one entry was chunked anyway")
	}

	mustSave(t, s, "first", bigToken("a"))
	mustSave(t, s, "second", bigToken("b"))
	manifest := manifestOf(t, kr)
	if len(kr.chunkAccounts(manifest.live)) == 0 {
		t.Fatal("blob outgrew one entry but no chunks were written")
	}

	if _, err := s.Delete(ProviderKey("first")); err != nil {
		t.Fatalf("Delete(first): %v", err)
	}
	if _, err := s.Delete(ProviderKey("second")); err != nil {
		t.Fatalf("Delete(second): %v", err)
	}

	head, ok, _ := kr.Get(keyringService, keyringAccount)
	if !ok || strings.HasPrefix(head, keyringManifestPrefix) {
		t.Fatalf("store did not return to a whole entry after shrinking: %.32s...", head)
	}
	for _, family := range []string{keyringChunkFamilyA, keyringChunkFamilyB} {
		if left := kr.chunkAccounts(family); len(left) != 0 {
			t.Errorf("generation %q still holds %v after shrinking", family, left)
		}
	}
	if got := mustLoad(t, s, "small"); got.AccessToken != "short" {
		t.Errorf("small token lost across the layout transitions: %#v", got)
	}
}

// TestStoreKeyringUnboundedBackendNeverChunks pins that a backend without a
// size limit (Linux secret-tool reads the secret from stdin) keeps the original
// single-entry form, so nothing about its stored layout changes.
func TestStoreKeyringUnboundedBackendNeverChunks(t *testing.T) {
	kr := newFakeKR()
	s := newCappedKeyringStore(t, kr)
	mustSave(t, s, "first", bigToken("a"))
	mustSave(t, s, "second", bigToken("b"))

	head, ok, _ := kr.Get(keyringService, keyringAccount)
	if !ok || strings.HasPrefix(head, keyringManifestPrefix) {
		t.Fatalf("unbounded backend used the chunked layout: %.32s...", head)
	}
	if got := mustLoad(t, s, "second"); got.AccessToken != bigToken("b").AccessToken {
		t.Error("unbounded backend did not round-trip")
	}
}

// TestStoreKeyringReadsLegacyWholeEntry covers the upgrade path: a blob written
// by a build that only knew the single-entry layout is still read, without a
// migration step.
func TestStoreKeyringReadsLegacyWholeEntry(t *testing.T) {
	kr := newFakeKR()
	legacy := newCappedKeyringStore(t, kr)
	mustSave(t, legacy, "first", Token{AccessToken: "legacy", RefreshToken: "r"})

	kr.budget = macOSLikeBudget
	s := newCappedKeyringStore(t, kr)
	if got := mustLoad(t, s, "first"); got.AccessToken != "legacy" {
		t.Fatalf("legacy entry did not load: %#v", got)
	}
	mustSave(t, s, "second", bigToken("b"))
	if got := mustLoad(t, s, "first"); got.AccessToken != "legacy" {
		t.Error("legacy token lost when the store grew into chunks")
	}
}

func TestParseKeyringManifestRejectsMalformed(t *testing.T) {
	digest := strings.Repeat("0", 64)
	for name, head := range map[string]string{
		"too few fields":     keyringManifestPrefix + "a:1:" + digest,
		"unknown generation": keyringManifestPrefix + "c:1:0:" + digest,
		"negative count":     keyringManifestPrefix + "a:-1:0:" + digest,
		"count over the cap": keyringManifestPrefix + "a:65:0:" + digest,
		"non-numeric count":  keyringManifestPrefix + "a:x:0:" + digest,
		"live has no chunks": keyringManifestPrefix + "a:0:2:" + digest,
		"short digest":       keyringManifestPrefix + "a:1:0:beef",
		"non-hex digest":     keyringManifestPrefix + "a:1:0:" + strings.Repeat("g", 64),
	} {
		if _, err := parseKeyringManifest(head); err == nil {
			t.Errorf("%s: parseKeyringManifest(%q) succeeded, want an error", name, head)
		}
	}
}

// assertNoStrayChunks checks the invariant the whole cleanup design rests on:
// the manifest may over-state how many chunks a generation holds (a later write
// deletes the excess) but must never under-state it, because an uncounted chunk
// is a fragment of a token blob nothing will ever delete.
func assertNoStrayChunks(t *testing.T, kr *fakeKR, manifest keyringManifest) {
	t.Helper()
	for _, family := range []string{keyringChunkFamilyA, keyringChunkFamilyB} {
		count := manifest.counts[family]
		for index := 0; index < keyringMaxChunks; index++ {
			account := keyringAccount + "." + family + "." + strconv.Itoa(index)
			_, exists := kr.data[keyringService+"/"+account]
			if family == manifest.live {
				if index < count && !exists {
					t.Errorf("live generation %q is missing expected chunk index %d", family, index)
				}
				if index >= count && exists {
					t.Errorf("live generation %q has stray chunk at index %d (count %d)", family, index, count)
				}
			} else {
				if index >= count && exists {
					t.Errorf("generation %q has stray chunk at index %d (count %d)", family, index, count)
				}
			}
		}
	}
	if got := len(kr.chunkAccounts(manifest.live)); got != manifest.counts[manifest.live] {
		t.Errorf("live generation %q holds %d chunks, manifest says %d", manifest.live, got, manifest.counts[manifest.live])
	}
}

// TestStoreKeyringReservesChunkRangeBeforeFilling covers the failure path the
// reservation exists for: a write that dies partway through filling a longer
// generation must still leave those chunks counted, so the next write deletes
// them instead of stranding token material in the keychain.
func TestStoreKeyringReservesChunkRangeBeforeFilling(t *testing.T) {
	// A small per-entry budget makes the token blob span many chunks, so a
	// failure can land in the middle of one generation.
	kr := newCappedFakeKR(keyringMinChunkLen + len(keyringAccount) + 40)
	s := newCappedKeyringStore(t, kr)
	mustSave(t, s, "first", bigToken("a"))

	before := manifestOf(t, kr)
	target := keyringChunkFamilyB
	if before.live == keyringChunkFamilyB {
		target = keyringChunkFamilyA
	}

	boom := errors.New("keychain is locked")
	kr.failSet = func(account string) error {
		if account == keyringAccount+"."+target+".5" {
			return boom
		}
		return nil
	}
	if err := s.Save(ProviderKey("second"), bigToken("b")); !errors.Is(err, boom) {
		t.Fatalf("Save with a failing chunk write: err = %v, want %v", err, boom)
	}
	kr.failSet = nil

	interrupted := manifestOf(t, kr)
	if interrupted.live != before.live {
		t.Fatalf("a failed write moved the live generation: %q -> %q", before.live, interrupted.live)
	}
	if len(kr.chunkAccounts(target)) == 0 {
		t.Fatal("the failed write left no chunks; the test no longer exercises the reservation")
	}
	assertNoStrayChunks(t, kr, interrupted)

	// A later, much smaller write into the same generation must reclaim every
	// chunk the interrupted one left behind.
	if _, err := s.Delete(ProviderKey("first")); err != nil {
		t.Fatalf("Delete(first): %v", err)
	}
	mustSave(t, s, "small", Token{AccessToken: strings.Repeat("s", 900)})
	assertNoStrayChunks(t, kr, manifestOf(t, kr))
}

// TestStoreKeyringSweepsStrayChunksOnFirstGrowth covers the one transition the
// reservation cannot protect: while the anchor still holds the whole blob there
// is no manifest to reserve into, because writing one would destroy the only
// copy. Chunks left by an interrupted shrink are swept instead.
func TestStoreKeyringSweepsStrayChunksOnFirstGrowth(t *testing.T) {
	kr := newCappedFakeKR(macOSLikeBudget)
	s := newCappedKeyringStore(t, kr)
	mustSave(t, s, "small", Token{AccessToken: "short"})

	// Stand in for a shrink whose cleanup was interrupted: the anchor holds a
	// whole blob, but a previous chunked generation is still on disk.
	strays := []string{keyringAccount + ".a.3", keyringAccount + ".a.7"}
	for _, account := range strays {
		kr.data[keyringService+"/"+account] = "c3RhbGUtdG9rZW4tbWF0ZXJpYWw="
	}

	mustSave(t, s, "first", bigToken("a"))
	mustSave(t, s, "second", bigToken("b"))

	assertNoStrayChunks(t, kr, manifestOf(t, kr))
	for _, account := range strays {
		if _, ok := kr.data[keyringService+"/"+account]; ok {
			t.Errorf("stray chunk %s survived the growth into the chunked layout", account)
		}
	}
}

// TestStoreKeyringReaderDoesNotBlockOrMissDuringSlowWriter verifies that readers
// (Load, Status, FirstStored) executing concurrently with a slow writer holding
// the cross-process lock do not block or treat contention as a missing credential.
func TestStoreKeyringReaderDoesNotBlockOrMissDuringSlowWriter(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)

	kr := newCappedFakeKR(macOSLikeBudget)
	writer, err := NewStore(StoreOptions{Storage: "keyring", Keyring: kr})
	if err != nil {
		t.Fatalf("NewStore(writer): %v", err)
	}
	reader, err := NewStore(StoreOptions{Storage: "keyring", Keyring: kr})
	if err != nil {
		t.Fatalf("NewStore(reader): %v", err)
	}

	mustSave(t, writer, "first", bigToken("a"))
	mustSave(t, writer, "second", bigToken("b"))

	// Manually acquire the lock to simulate writer holding the lock across
	// manifest commit and chunk rotation.
	lockPath := writer.blob.(keyringBlob).lockPath
	unlock, err := acquireFileLock(lockPath, time.Now)
	if err != nil {
		t.Fatalf("acquireFileLock: %v", err)
	}

	type loadResult struct {
		tok Token
		ok  bool
		err error
	}
	done := make(chan loadResult, 1)
	go func() {
		tok, ok, err := reader.Load(ProviderKey("first"))
		done <- loadResult{tok: tok, ok: ok, err: err}
	}()

	// Brief pause to ensure reader goroutine has entered acquireFileLock contention loop.
	time.Sleep(50 * time.Millisecond)

	// Release lock; reader must acquire lock and complete.
	unlock()

	select {
	case res := <-done:
		if res.err != nil {
			t.Fatalf("reader.Load failed after lock release: %v", res.err)
		}
		if !res.ok || res.tok.AccessToken != bigToken("a").AccessToken {
			t.Fatalf("reader.Load returned unexpected token: ok=%v, token=%v", res.ok, res.tok)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("reader.Load timed out waiting for lock release")
	}

	firstTok, key, found := FirstStored(reader, []string{"first", "second"})
	if !found || key != ProviderKey("first") || firstTok.AccessToken != bigToken("a").AccessToken {
		t.Fatalf("FirstStored returned (%v, %q, %v), want valid 'first' token", firstTok, key, found)
	}

	st, err := reader.Status(KeyPrefixProvider)
	if err != nil {
		t.Fatalf("reader.Status failed: %v", err)
	}
	if len(st) != 2 {
		t.Fatalf("reader.Status returned %d entries, want 2", len(st))
	}
}

// TestStoreKeyringSharedLockAcrossDifferentEnvironmentRoots is the regression for
// split lock domains when processes run with different ZERO_OAUTH_TOKENS_PATH,
// FilePath, or XDG_CONFIG_HOME. Because the OS keychain is single-domain for the
// user ("zero"/"oauth-tokens"), all stores must share the same lock file so
// concurrent writes and manifest rotations remain strictly serialized.
func TestStoreKeyringSharedLockAcrossDifferentEnvironmentRoots(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv("USERPROFILE", homeDir)

	dirA := t.TempDir()
	dirB := t.TempDir()

	kr := newCappedFakeKR(macOSLikeBudget)
	storeA, err := NewStore(StoreOptions{
		Storage:  "keyring",
		Keyring:  kr,
		FilePath: filepath.Join(dirA, "custom-tokens.json"),
		Env: map[string]string{
			"ZERO_OAUTH_TOKENS_PATH": filepath.Join(dirA, "custom-tokens.json"),
			"XDG_CONFIG_HOME":        dirA,
		},
	})
	if err != nil {
		t.Fatalf("NewStore(storeA): %v", err)
	}

	storeB, err := NewStore(StoreOptions{
		Storage:  "keyring",
		Keyring:  kr,
		FilePath: filepath.Join(dirB, "custom-tokens.json"),
		Env: map[string]string{
			"ZERO_OAUTH_TOKENS_PATH": filepath.Join(dirB, "custom-tokens.json"),
			"XDG_CONFIG_HOME":        dirB,
		},
	})
	if err != nil {
		t.Fatalf("NewStore(storeB): %v", err)
	}

	lockA := storeA.blob.(keyringBlob).lockPath
	lockB := storeB.blob.(keyringBlob).lockPath
	if lockA == "" || lockB == "" {
		t.Fatalf("lockPath should not be empty: lockA=%q lockB=%q", lockA, lockB)
	}
	if lockA != lockB {
		t.Fatalf("stores with distinct env roots must share the keyring lock: lockA=%q != lockB=%q", lockA, lockB)
	}

	mustSave(t, storeA, "keyA", bigToken("a"))

	// Force overlapping save/save: write from storeB and storeA consecutively,
	// verifying both updates are preserved and readable by either store.
	if err := storeB.Save(ProviderKey("keyB"), bigToken("b")); err != nil {
		t.Fatalf("storeB.Save: %v", err)
	}
	if err := storeA.Save(ProviderKey("keyC"), bigToken("c")); err != nil {
		t.Fatalf("storeA.Save: %v", err)
	}

	gotA := mustLoad(t, storeB, "keyA")
	gotB := mustLoad(t, storeA, "keyB")
	gotC := mustLoad(t, storeB, "keyC")
	if gotA.AccessToken != bigToken("a").AccessToken ||
		gotB.AccessToken != bigToken("b").AccessToken ||
		gotC.AccessToken != bigToken("c").AccessToken {
		t.Errorf("concurrent state was not preserved across stores")
	}
}

// TestStoreKeyringFirstMigrationFailureCleansUpWrittenChunks is the regression for
// P1: during the first migration from a whole-entry layout to a chunked layout,
// if a chunk write fails partway through, the written chunks must be cleaned up
// so future small saves (which do not sweep chunk ranges) do not leave orphaned
// token material in the keychain.
func TestStoreKeyringFirstMigrationFailureCleansUpWrittenChunks(t *testing.T) {
	kr := newCappedFakeKR(macOSLikeBudget)
	s := newCappedKeyringStore(t, kr)

	smallToken := Token{
		AccessToken: "small-secret-token",
		TokenType:   "Bearer",
		Account:     "user@example.com",
	}
	mustSave(t, s, "small", smallToken)

	// Verify initial store is a single whole entry (not chunked).
	head, ok, err := kr.Get(keyringService, keyringAccount)
	if err != nil || !ok {
		t.Fatalf("initial Get failed: %v", err)
	}
	if strings.HasPrefix(head, keyringManifestPrefix) {
		t.Fatal("initial small save unexpectedly created a manifest")
	}

	// Inject failure on writing the second chunk during the first oversized save.
	boom := errors.New("keychain write failed on chunk 1")
	kr.failSet = func(account string) error {
		if strings.HasSuffix(account, "."+keyringChunkFamilyA+".1") {
			return boom
		}
		return nil
	}

	// Try saving an oversized token (triggering first migration to chunked layout).
	hugeToken := bigToken("x")
	hugeToken.AccessToken = strings.Repeat("x", 4000)
	if err := s.Save(ProviderKey("big"), hugeToken); !errors.Is(err, boom) {
		t.Fatalf("Save with chunk write failure: got %v, want %v", err, boom)
	}
	kr.failSet = nil

	// Verify the original small token remains intact and readable.
	if got := mustLoad(t, s, "small"); got.AccessToken != smallToken.AccessToken {
		t.Fatalf("small token was damaged by failed migration: %v", got)
	}

	// Verify no chunks survived in either chunk family.
	if chunksA := kr.chunkAccounts(keyringChunkFamilyA); len(chunksA) != 0 {
		t.Fatalf("chunks for family A survived failed first migration: %v", chunksA)
	}
	if chunksB := kr.chunkAccounts(keyringChunkFamilyB); len(chunksB) != 0 {
		t.Fatalf("chunks for family B survived failed first migration: %v", chunksB)
	}

	// A subsequent small save succeeds and stays whole without strays.
	mustSave(t, s, "small2", smallToken)
	if chunksA := kr.chunkAccounts(keyringChunkFamilyA); len(chunksA) != 0 {
		t.Fatalf("chunks for family A found after subsequent small save: %v", chunksA)
	}
	if got := mustLoad(t, s, "small"); got.AccessToken != smallToken.AccessToken {
		t.Errorf("small token not readable: %v", got)
	}
	if got := mustLoad(t, s, "small2"); got.AccessToken != smallToken.AccessToken {
		t.Errorf("small2 token not readable: %v", got)
	}
}

// TestStoreKeyringFirstMigrationFinalManifestFailureCleansUpWrittenChunks verifies that
// if the final manifest write fails during first migration, written chunks are removed.
func TestStoreKeyringFirstMigrationFinalManifestFailureCleansUpWrittenChunks(t *testing.T) {
	kr := newCappedFakeKR(macOSLikeBudget)
	s := newCappedKeyringStore(t, kr)

	smallToken := Token{
		AccessToken: "small-secret-token",
		TokenType:   "Bearer",
		Account:     "user@example.com",
	}
	mustSave(t, s, "small", smallToken)

	boom := errors.New("keychain write failed on anchor manifest commit")
	kr.failSet = func(account string) error {
		if account == keyringAccount {
			return boom
		}
		return nil
	}

	hugeToken := bigToken("x")
	hugeToken.AccessToken = strings.Repeat("x", 4000)
	if err := s.Save(ProviderKey("big"), hugeToken); !errors.Is(err, boom) {
		t.Fatalf("Save with final manifest commit failure: got %v, want %v", err, boom)
	}
	kr.failSet = nil

	// Verify original small token is still intact.
	if got := mustLoad(t, s, "small"); got.AccessToken != smallToken.AccessToken {
		t.Fatalf("small token was damaged by failed migration: %v", got)
	}

	// Verify all chunks in family A were cleaned up.
	if chunksA := kr.chunkAccounts(keyringChunkFamilyA); len(chunksA) != 0 {
		t.Fatalf("chunks for family A survived failed first migration: %v", chunksA)
	}
}

// TestStoreKeyringCorruptStateActionableErrorAndRecovery verifies that corrupt
// manifests, missing chunks, and digest mismatches fail closed with actionable
// error messages naming the anchor and remediation command, and that Reset()
// provides a full recovery path.
func TestStoreKeyringCorruptStateActionableErrorAndRecovery(t *testing.T) {
	kr := newCappedFakeKR(macOSLikeBudget)
	s := newCappedKeyringStore(t, kr)

	mustSave(t, s, "first", bigToken("a"))
	mustSave(t, s, "second", bigToken("b"))

	manifest := manifestOf(t, kr)

	// 1. Missing chunk error test
	firstChunk := keyringAccount + "." + manifest.live + ".0"
	savedChunk := kr.data[keyringService+"/"+firstChunk]
	delete(kr.data, keyringService+"/"+firstChunk)

	_, _, err := s.Load(ProviderKey("first"))
	if err == nil || !strings.Contains(err.Error(), "zero auth reset") || !strings.Contains(err.Error(), keyringAccount) {
		t.Fatalf("Load with missing chunk did not return actionable recovery error: %v", err)
	}
	if err := s.Save(ProviderKey("third"), bigToken("c")); err == nil || !strings.Contains(err.Error(), "zero auth reset") {
		t.Fatalf("Save with missing chunk did not fail closed with actionable recovery error: %v", err)
	}
	if _, err := s.Status(KeyPrefixProvider); err == nil || !strings.Contains(err.Error(), "zero auth reset") {
		t.Fatalf("Status with missing chunk did not return actionable recovery error: %v", err)
	}
	if _, err := s.Delete(ProviderKey("first")); err == nil || !strings.Contains(err.Error(), "zero auth reset") {
		t.Fatalf("Delete with missing chunk did not return actionable recovery error: %v", err)
	}

	// Restore chunk
	kr.data[keyringService+"/"+firstChunk] = savedChunk

	// 2. Digest mismatch test: modify one character in the chunk while keeping it valid base64
	mutated := []byte(savedChunk)
	if mutated[0] == 'A' {
		mutated[0] = 'B'
	} else {
		mutated[0] = 'A'
	}
	kr.data[keyringService+"/"+firstChunk] = string(mutated)
	_, _, err = s.Load(ProviderKey("first"))
	if err == nil || !strings.Contains(err.Error(), "failed its integrity check") || !strings.Contains(err.Error(), "zero auth reset") {
		t.Fatalf("Load with digest mismatch did not return actionable recovery error: %v", err)
	}

	// 3. Malformed manifest test
	kr.data[keyringService+"/"+keyringAccount] = "zc1:invalid-manifest-data"
	_, _, err = s.Load(ProviderKey("first"))
	if err == nil || !strings.Contains(err.Error(), "malformed manifest") || !strings.Contains(err.Error(), "zero auth reset") {
		t.Fatalf("Load with malformed manifest did not return actionable recovery error: %v", err)
	}
	if err := s.Save(ProviderKey("fourth"), bigToken("d")); err == nil || !strings.Contains(err.Error(), "zero auth reset") {
		t.Fatalf("Save with malformed manifest did not fail closed: %v", err)
	}

	// 4. Test recovery via Reset()
	if err := s.Reset(); err != nil {
		t.Fatalf("Reset() failed: %v", err)
	}

	// Verify all entries in fake keyring were purged.
	if len(kr.data) != 0 {
		t.Fatalf("Reset did not clear all keyring entries, remaining: %v", kr.data)
	}

	// 5. Fresh save and load after reset succeeds
	mustSave(t, s, "fresh", bigToken("e"))
	if got := mustLoad(t, s, "fresh"); got.AccessToken != bigToken("e").AccessToken {
		t.Fatalf("fresh load after reset returned %v", got)
	}
}

// TestStoreKeyringShrinkResidueIsReclaimedOnRegrowth is the regression for a
// retired generation orphaned permanently. Cleanup is hygiene rather than
// correctness only while a manifest exists to state the counts; writeWhole
// replaces the anchor with the blob and the counts go with it. A shrink whose
// delete of the retired generation fails therefore leaves chunks that the next
// growth would never sweep, because that growth only ever targets family A.
// The token material in them would sit in the keychain for good.
func TestStoreKeyringShrinkResidueIsReclaimedOnRegrowth(t *testing.T) {
	kr := newCappedFakeKR(macOSLikeBudget)
	s := newCappedKeyringStore(t, kr)

	// Grow until family B is the live generation, so a shrink from here is the
	// one that retires it.
	mustSave(t, s, "first", bigToken("a"))
	mustSave(t, s, "second", bigToken("b"))
	mustSave(t, s, "third", bigToken("c"))
	if live := manifestOf(t, kr).live; live != keyringChunkFamilyB {
		t.Fatalf("live generation = %q, want %q", live, keyringChunkFamilyB)
	}

	// Shrink back to a whole entry with a keychain that refuses to remove
	// family B, the generation being retired on the way down.
	kr.failDelete = func(account string) error {
		if strings.HasPrefix(account, keyringAccount+"."+keyringChunkFamilyB+".") {
			return errors.New("keychain busy")
		}
		return nil
	}
	for _, key := range []string{"second", "third"} {
		if _, err := s.Delete(ProviderKey(key)); err != nil && !strings.Contains(err.Error(), "could not be removed") {
			t.Fatalf("Delete(%s): %v", key, err)
		}
	}
	kr.failDelete = nil
	if head, _, _ := kr.Get(keyringService, keyringAccount); strings.HasPrefix(head, keyringManifestPrefix) {
		t.Fatal("store did not shrink back to a whole entry, so the orphaning path was never taken")
	}
	orphans := kr.chunkAccounts(keyringChunkFamilyB)
	if len(orphans) == 0 {
		t.Fatal("setup did not strand any family-B chunks")
	}

	// Grow back into the chunked layout. This is the only sweep that can still
	// reach the stranded generation, because the manifest that named it is gone.
	mustSave(t, s, "fourth", bigToken("d"))

	manifest := manifestOf(t, kr)
	if manifest.live != keyringChunkFamilyA {
		t.Fatalf("live generation after regrowth = %q, want %q", manifest.live, keyringChunkFamilyA)
	}
	if got := kr.chunkAccounts(keyringChunkFamilyB); len(got) != 0 {
		t.Errorf("family B still holds %d unreferenced chunks after regrowth: %v", len(got), got)
	}
	assertNoStrayChunks(t, kr, manifest)
	if got := mustLoad(t, s, "fourth"); got.AccessToken != bigToken("d").AccessToken {
		t.Error("token stored across the regrowth did not survive")
	}
}

// TestStoreKeyringFirstMigrationRollbackFailurePreservesReclaimableCleanup tests that
// if first-migration chunk write fails and compensating deletion also fails, the
// cleanup error is joined into the return error and cleanup state is durably recorded
// such that a subsequent small save reclaims the stranded chunks.
func TestStoreKeyringFirstMigrationRollbackFailurePreservesReclaimableCleanup(t *testing.T) {
	kr := newCappedFakeKR(macOSLikeBudget)
	s := newCappedKeyringStore(t, kr)

	smallToken := Token{
		AccessToken: "small-secret-token",
		TokenType:   "Bearer",
		Account:     "user@example.com",
	}
	mustSave(t, s, "small", smallToken)

	// Inject failure on writing chunk 1, AND failure on deleting chunk 0 during rollback.
	writeBoom := errors.New("keychain write failed on chunk 1")
	deleteBoom := errors.New("keychain delete failed on chunk 0 during rollback")
	kr.failSet = func(account string) error {
		if strings.HasSuffix(account, "."+keyringChunkFamilyA+".1") {
			return writeBoom
		}
		return nil
	}
	kr.failDelete = func(account string) error {
		if strings.HasSuffix(account, "."+keyringChunkFamilyA+".0") {
			return deleteBoom
		}
		return nil
	}

	hugeToken := bigToken("x")
	hugeToken.AccessToken = strings.Repeat("x", 4000)
	err := s.Save(ProviderKey("big"), hugeToken)
	if err == nil {
		t.Fatal("expected Save to fail")
	}
	if !errors.Is(err, writeBoom) {
		t.Errorf("expected error to wrap writeBoom: %v", err)
	}
	if !strings.Contains(err.Error(), "cleanup orphaned migration chunks") {
		t.Errorf("expected error to join rollback cleanup failure: %v", err)
	}

	// Verify the original small token is still intact and readable.
	if got := mustLoad(t, s, "small"); got.AccessToken != smallToken.AccessToken {
		t.Fatalf("small token was damaged by failed migration: %v", got)
	}

	// Clear failure hooks.
	kr.failSet = nil
	kr.failDelete = nil

	// Verify chunk 0 was stranded initially.
	if chunksA := kr.chunkAccounts(keyringChunkFamilyA); len(chunksA) != 1 {
		t.Fatalf("expected 1 stranded chunk in family A before cleanup, got %v", chunksA)
	}

	// Perform a subsequent small save (which uses writeWhole).
	mustSave(t, s, "small2", smallToken)

	// Verify both families are now completely empty of chunks.
	if chunksA := kr.chunkAccounts(keyringChunkFamilyA); len(chunksA) != 0 {
		t.Fatalf("chunks for family A survived subsequent small save: %v", chunksA)
	}
	if chunksB := kr.chunkAccounts(keyringChunkFamilyB); len(chunksB) != 0 {
		t.Fatalf("chunks for family B found after subsequent small save: %v", chunksB)
	}
	if _, ok, _ := kr.Get(keyringService, keyringAccount+".cleanup"); ok {
		t.Fatal(".cleanup marker was not removed after sweep")
	}
}

func TestStoreKeyringCleanupPreservesLiveMigrationAfterFailedWrite(t *testing.T) {
	for _, failMarkerDelete := range []bool{false, true} {
		t.Run(strconv.FormatBool(failMarkerDelete), func(t *testing.T) {
			kr := newCappedFakeKR(macOSLikeBudget)
			s := newCappedKeyringStore(t, kr)
			mustSave(t, s, "first", bigToken("a"))
			writeErr := errors.New("keychain full")
			deleteErr := errors.New("keychain busy")
			kr.failSet = func(account string) error {
				if account == keyringAccount+".a.1" {
					return writeErr
				}
				return nil
			}
			kr.failDelete = func(account string) error {
				if account == keyringAccount+".a.0" || (failMarkerDelete && account == keyringAccount+".cleanup") {
					return deleteErr
				}
				return nil
			}
			if err := s.Save(ProviderKey("second"), bigToken("b")); !errors.Is(err, writeErr) || !errors.Is(err, deleteErr) {
				t.Fatalf("migration error = %v, want write and rollback failures", err)
			}
			if marker, ok, _ := kr.Get(keyringService, keyringAccount+".cleanup"); !ok || marker != "a:1" {
				t.Fatalf("cleanup marker = %q, present = %v", marker, ok)
			}

			kr.failSet = nil
			mustSave(t, s, "second", bigToken("b"))
			if live := manifestOf(t, kr).live; live != keyringChunkFamilyA {
				t.Fatalf("live family = %q, want a", live)
			}
			kr.failDelete = func(account string) error {
				if failMarkerDelete && account == keyringAccount+".cleanup" {
					return deleteErr
				}
				return nil
			}
			kr.failSet = func(account string) error {
				if account == keyringAccount+".b.0" {
					return writeErr
				}
				return nil
			}
			if err := s.Save(ProviderKey("third"), bigToken("c")); !errors.Is(err, writeErr) {
				t.Fatalf("replacement error = %v, want write failure", err)
			}
			for name, seed := range map[string]string{"first": "a", "second": "b"} {
				if got := mustLoad(t, s, name); got.AccessToken != bigToken(seed).AccessToken {
					t.Errorf("%s token changed after failed replacement", name)
				}
			}
			if _, ok, _ := kr.Get(keyringService, keyringAccount+".cleanup"); ok != failMarkerDelete {
				t.Errorf("cleanup marker present = %v, want %v", ok, failMarkerDelete)
			}
		})
	}
}

func TestStoreKeyringCleanupPreservesChunksWithMalformedManifest(t *testing.T) {
	kr := newCappedFakeKR(macOSLikeBudget)
	blob := keyringBlob{kr: kr, service: keyringService, account: keyringAccount}
	kr.data[keyringService+"/"+keyringAccount] = keyringManifestPrefix + "invalid"
	kr.data[keyringService+"/"+blob.cleanupAccount()] = "a:1"
	kr.data[keyringService+"/"+blob.chunkAccount(keyringChunkFamilyA, 0)] = "credential fragment"
	blob.sweepCleanupAccount()
	if len(kr.deletes) != 0 {
		t.Fatalf("cleanup deleted entries without establishing manifest ownership: %v", kr.deletes)
	}
}

// TestStoreKeyringResetIsBoundedByBackendAndManifest verifies that Reset only
// issues the necessary delete operations according to backend capabilities and
// known manifest layout.
func TestStoreKeyringResetIsBoundedByBackendAndManifest(t *testing.T) {
	t.Run("unbounded backend issues only anchor delete", func(t *testing.T) {
		kr := newFakeKR()
		s := newCappedKeyringStore(t, kr)

		mustSave(t, s, "first", bigToken("a"))
		mustSave(t, s, "second", bigToken("b"))

		kr.deletes = map[string]int{}
		if err := s.Reset(); err != nil {
			t.Fatalf("Reset: %v", err)
		}

		totalDeletes := 0
		for _, count := range kr.deletes {
			totalDeletes += count
		}
		// On unbounded backend (Linux), at most anchor + cleanup marker = 2 deletes, not 129
		if totalDeletes > 2 {
			t.Errorf("unbounded reset performed %d delete calls, want <= 2", totalDeletes)
		}
	})

	t.Run("bounded backend with valid manifest issues bounded chunk deletes", func(t *testing.T) {
		kr := newCappedFakeKR(macOSLikeBudget)
		s := newCappedKeyringStore(t, kr)

		mustSave(t, s, "first", bigToken("a"))
		mustSave(t, s, "second", bigToken("b"))

		manifest := manifestOf(t, kr)
		expectedChunks := manifest.counts[keyringChunkFamilyA] + manifest.counts[keyringChunkFamilyB]
		if expectedChunks == 0 {
			t.Fatal("expected non-zero chunks in manifest")
		}

		kr.deletes = map[string]int{}
		if err := s.Reset(); err != nil {
			t.Fatalf("Reset: %v", err)
		}

		totalDeletes := 0
		for _, count := range kr.deletes {
			totalDeletes += count
		}
		// Chunk count + anchor + cleanup marker
		if totalDeletes > expectedChunks+2 {
			t.Errorf("manifest-bounded reset performed %d delete calls, want <= %d", totalDeletes, expectedChunks+2)
		}
	})
}

func TestStoreKeyringSweepCleanupAccountBoundsAndValidatesMarker(t *testing.T) {
	kr := newCappedFakeKR(macOSLikeBudget)
	blob := keyringBlob{kr: kr, service: keyringService, account: keyringAccount}

	// 1. Invalid family: must not delete any chunks, and marker is removed
	kr.data[keyringService+"/"+blob.cleanupAccount()] = "invalid_family:5"
	kr.deletes = map[string]int{}
	blob.sweepCleanupAccount()
	if len(kr.deletes) != 1 || kr.deletes[blob.cleanupAccount()] != 1 {
		t.Errorf("expected only cleanup marker delete for invalid family, got %v", kr.deletes)
	}

	// 2. Count larger than keyringMaxChunks: must be clamped to keyringMaxChunks
	kr.data[keyringService+"/"+blob.cleanupAccount()] = keyringChunkFamilyA + ":1000000"
	kr.deletes = map[string]int{}
	blob.sweepCleanupAccount()
	// Should delete chunks 0..keyringMaxChunks-1 plus the cleanup marker
	expectedDeletes := keyringMaxChunks + 1
	totalDeletes := 0
	for _, c := range kr.deletes {
		totalDeletes += c
	}
	if totalDeletes != expectedDeletes {
		t.Errorf("expected %d deletes (clamped to keyringMaxChunks + marker), got %d", expectedDeletes, totalDeletes)
	}

	// 3. Failed delete preserves the cleanup marker
	failKR := &erroringFakeKR{
		fakeKR:    newCappedFakeKR(macOSLikeBudget),
		deleteErr: errors.New("keychain delete failure"),
	}
	blobWithErr := keyringBlob{kr: failKR, service: keyringService, account: keyringAccount}
	failKR.data[keyringService+"/"+blobWithErr.cleanupAccount()] = keyringChunkFamilyA + ":3"
	blobWithErr.sweepCleanupAccount()

	// Marker must still be in keychain
	if val, ok := failKR.data[keyringService+"/"+blobWithErr.cleanupAccount()]; !ok || val != keyringChunkFamilyA+":3" {
		t.Errorf("expected cleanup marker preserved after delete failure, got %q, ok=%v", val, ok)
	}
}

type erroringFakeKR struct {
	*fakeKR
	deleteErr error
}

func (e *erroringFakeKR) Delete(service, account string) (bool, error) {
	if strings.Contains(account, ".a.") && e.deleteErr != nil {
		return false, e.deleteErr
	}
	return e.fakeKR.Delete(service, account)
}
