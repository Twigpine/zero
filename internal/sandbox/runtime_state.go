package sandbox

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

var sandboxUserCacheDir = os.UserCacheDir
var sandboxRuntimeNow = time.Now

const (
	sandboxRuntimeMaxAge   = 30 * 24 * time.Hour
	sandboxRuntimeMaxRoots = 64
)

type SandboxRuntime struct {
	Root  string `json:"root,omitempty"`
	Cache string `json:"cache,omitempty"`
	Data  string `json:"data,omitempty"`
	Temp  string `json:"temp,omitempty"`
}

// sandboxRuntimeRootFor derives the per-workspace runtime root. It is separated
// from prepareSandboxRuntime because the elevated Windows setup path needs the
// same answer WITHOUT taking a lease or creating anything: a sandbox principal
// is a separate account with no inherited rights under the user cache, so setup
// has to grant it write access to this tree before any command runs.
//
// Both callers must agree exactly. If they ever drift, setup grants the ACE on
// one directory while commands write to another, and the failure is a bare
// ACCESS_DENIED from npm or go build with nothing pointing at the sandbox.
func sandboxRuntimeRootFor(workspaceRoot string, cacheRoot string) (string, error) {
	if root, ok := deterministicSandboxRuntimeRoot(workspaceRoot, cacheRoot); ok {
		return root, nil
	}
	return fallbackSandboxRuntimeRoot(workspaceRoot)
}

// deterministicSandboxRuntimeRoot returns the cache-derived runtime root and
// whether it is usable, meaning it lands outside the workspace.
//
// Neither this nor the fallback creates anything now, so a caller that only
// needs to NAME the tree can safely go through sandboxRuntimeRootFor and get the
// answer commands will actually use. This remains separate for callers that need
// to distinguish the cache-derived root from the temp-derived one.
func deterministicSandboxRuntimeRoot(workspaceRoot string, cacheRoot string) (string, bool) {
	digest := sha256.Sum256([]byte(workspaceRoot))
	// The same inventory the rooted traversal recognizes a runtime root by. See
	// windowsSandboxRuntimeOwnedNames: two spellings of this list is how a real
	// runtime root stops being recognized as owned, and that failure opens by
	// name instead of by handle.
	root := filepath.Join(append(append([]string{cacheRoot}, windowsSandboxRuntimeOwnedNames...), hex.EncodeToString(digest[:8]))...)
	return root, !runtimeRootWithinWorkspace(workspaceRoot, root)
}

// runtimeRootWithinWorkspace reports whether root lands inside workspaceRoot.
//
// pathWithinRoot compares SPELLINGS, and canonicalSandboxWorkspaceRoot folds only
// the aliases filepath.EvalSymlinks folds. Two get through it. Case: filepath.Rel
// folds case on Windows via sameWord but not elsewhere, so on a case-insensitive
// macOS volume /var/folders/x and /VAR/FOLDERS/X are one directory that every
// string comparison here keeps apart. Junctions: EvalSymlinks returns a Windows
// directory junction unchanged, so a TEMP that reaches the workspace through one
// measures as outside it.
//
// Three checks, each of which can only ADD a containment answer. That asymmetry
// is the safety argument: the failure that matters is the runtime tree living
// inside the workspace, so a missed alias is the expensive direction and an extra
// relocation is the cheap one.
//
//  1. the spellings as given;
//  2. the spellings resolved to physical paths, which on Windows follows
//     junctions at any depth via GetFinalPathNameByHandle;
//  3. filesystem identity across root's existing ancestors, which catches a case
//     alias on a case-insensitive volume where step 2 has no API to call.
//
// Step 3 only sees an alias whose target IS the workspace root, because it walks
// a SPELLING upward and a junction has no spelling chain back into its target's
// parent. That shape is covered by step 2 on Windows. It remains open off Windows
// for a bind mount, which the kernel presents as a real path with no way to ask
// where it came from; closing that needs mountinfo parsing, not a path API.
func runtimeRootWithinWorkspace(workspaceRoot string, root string) bool {
	if pathWithinRoot(workspaceRoot, root) {
		return true
	}
	if physicalWorkspace := physicalSandboxPath(workspaceRoot); physicalWorkspace != "" {
		if pathWithinRoot(physicalWorkspace, physicalSandboxPath(root)) {
			return true
		}
	}
	workspaceInfo, err := os.Stat(workspaceRoot)
	if err != nil {
		// An unresolvable workspace leaves nothing to compare against. The
		// spelling checks above already returned their answer.
		return false
	}
	// root itself usually does not exist yet, which is the point: start at the
	// deepest component and walk up, so the first directory that does exist gets
	// compared and every ancestor above it after that.
	current := filepath.Clean(root)
	for {
		if info, err := os.Stat(current); err == nil && os.SameFile(workspaceInfo, info) {
			return true
		}
		parent := filepath.Dir(current)
		if parent == current {
			return false
		}
		current = parent
	}
}

func prepareSandboxRuntime(workspaceRoot string, sandboxHome string) (SandboxRuntime, func(), error) {
	// One selection function, shared with setup. See selectSandboxRuntimeRoot.
	//
	// The ledger is deliberately unused here. A COMMAND is not a transaction: it
	// does not publish a marker and has nothing to roll back to, and a component
	// created on this path is the runtime tree the command is about to use. Only
	// setup owns an undo.
	root, lease, _, err := selectSandboxRuntimeRoot(workspaceRoot, true, sandboxHome)
	if err != nil {
		return SandboxRuntime{}, nil, err
	}
	prepared := false
	defer func() {
		if !prepared {
			lease.release()
		}
	}()
	runtimeState := SandboxRuntime{
		Root:  root,
		Cache: filepath.Join(root, "cache"),
		Data:  filepath.Join(root, "data"),
		Temp:  filepath.Join(root, "tmp"),
	}
	// BEFORE ANYTHING IS CREATED. os.MkdirAll returns nil when Stat says the path
	// is already a directory, and Stat follows links, so a link planted at an
	// owned component is silently accepted and the whole tree is built inside
	// whatever it points at, and the root then becomes a write root the backend
	// binds read-write with TMPDIR and the build caches pointed inside it.
	if err := refuseAliasedRuntimeComponents(runtimeState.Root); err != nil {
		return SandboxRuntime{}, nil, err
	}
	if err := os.MkdirAll(runtimeState.Root, 0o700); err != nil {
		return SandboxRuntime{}, nil, fmt.Errorf("create sandbox runtime directory %s: %w", runtimeState.Root, err)
	}
	// AND AGAIN AFTER, because the check above is a check-then-use on its own: a
	// component swapped during creation would still redirect the tree. Pairing the
	// two narrows the window to the creation itself.
	if err := refuseAliasedRuntimeComponents(runtimeState.Root); err != nil {
		return SandboxRuntime{}, nil, err
	}
	// Everything below the root goes through a handle on it. The children are
	// the one part of the tree a sandboxed command can replace; see
	// prepareSandboxRuntimeTree.
	now := sandboxRuntimeNow()
	if err := prepareSandboxRuntimeTree(runtimeState.Root, now); err != nil {
		return SandboxRuntime{}, nil, err
	}
	cleanupSandboxRuntimeRoots(filepath.Dir(runtimeState.Root), runtimeState.Root, now)
	prepared = true
	return runtimeState, lease.release, nil
}

func prepareSandboxRuntimeLease(root string) (*sandboxRuntimeLease, error) {
	lease, _, err := prepareSandboxRuntimeLeaseRecording(root)
	return lease, err
}

// prepareSandboxRuntimeLeaseRecording also reports the owned directories it
// created, so a caller that can roll back knows what it owns.
//
// Nothing recorded here is created by the leaf's owner. Provisioning creates and
// records the leaf itself; these are the components above it, which used to be
// produced by an os.MkdirAll that nobody accounted for. Setup could therefore
// fail after the lease and leave a tree behind with no record that it made it.
func prepareSandboxRuntimeLeaseRecording(root string) (*sandboxRuntimeLease, []windowsCreatedRuntimeDir, error) {
	return acquireRuntimeLeaseForPlatform(root)
}

// cleanupSandboxRuntimeRoots applies a conservative age/count policy. Cleanup
// is best-effort and never removes the runtime selected for the current
// command, so cache maintenance cannot turn a valid execution into a failure.
func cleanupSandboxRuntimeRoots(parent, current string, now time.Time) {
	entries, err := os.ReadDir(parent)
	if err != nil {
		return
	}
	type candidate struct {
		path    string
		modTime time.Time
	}
	var candidates []candidate
	for _, entry := range entries {
		path := filepath.Join(parent, entry.Name())
		if !entry.IsDir() || filepath.Clean(path) == filepath.Clean(current) {
			continue
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			continue
		}
		if now.Sub(info.ModTime()) > sandboxRuntimeMaxAge {
			removeSandboxRuntimeRootIfUnused(path)
			continue
		}
		candidates = append(candidates, candidate{path: path, modTime: info.ModTime()})
	}
	if len(candidates) < sandboxRuntimeMaxRoots {
		return
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].modTime.Before(candidates[j].modTime) })
	for len(candidates) >= sandboxRuntimeMaxRoots {
		removeSandboxRuntimeRootIfUnused(candidates[0].path)
		candidates = candidates[1:]
	}
}

func removeSandboxRuntimeRootIfUnused(root string) {
	lease, inUse, err := tryAcquireSandboxRuntimeCleanupLease(root)
	if err != nil || inUse {
		return
	}
	defer lease.release()
	_ = os.RemoveAll(root)
}

func combineSandboxCleanups(cleanups ...func()) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			for _, cleanup := range cleanups {
				if cleanup != nil {
					cleanup()
				}
			}
		})
	}
}

// fallbackSandboxRuntimeRoot returns the runtime root for a workspace whose
// cache-derived root would land inside itself.
//
// DERIVED, not minted, and that is the whole of the fix. It used to call
// os.MkdirTemp and remember the answer in a process-global map, which made the
// result private to whichever process asked first. Elevated Windows setup
// granted the sandbox principal write access to the directory IT created, then
// every later __windows-command-runner process created a DIFFERENT one and
// pointed TMP, GOCACHE, npm and the rest at it. Those directories are created by
// the calling user and carry no ACE for the principal, so ordinary cache and
// temp writes failed with a bare ACCESS_DENIED and nothing naming the sandbox.
//
// sandboxRuntimeRootFor already documents that both callers must agree exactly.
// Hashing the workspace, the same way the cache-derived root does, is what makes
// that true for this branch too: every process reaches the same path without
// having to share any state.
//
// It creates nothing, so deterministicSandboxRuntimeRoot's promise about naming
// a tree without materializing it now holds for the fallback as well.
func fallbackSandboxRuntimeRoot(workspaceRoot string) (string, error) {
	// Canonicalized for the same reason prepareSandboxRuntime canonicalizes the
	// workspace and cache roots: pathWithinRoot compares SPELLINGS, so a raw
	// os.TempDir() measured against a canonical workspace root compares two
	// different names for one directory and the containment check misses. Both
	// callers resolve this in the operator's environment, so the derived path is
	// identical on the setup and command sides and the plan hashes still agree.
	//
	// This closes the 8.3 short-name and symlink spellings, not every alias: a
	// Windows directory JUNCTION comes back from EvalSymlinks unchanged, and case
	// survives it on a case-insensitive macOS volume. The containment check below
	// therefore does not rely on canonicalization alone; runtimeRootWithinWorkspace
	// falls through to filesystem identity for exactly those aliases.
	tempRoot := canonicalSandboxWorkspaceRoot(os.TempDir())
	if tempRoot == "" || tempRoot == "." {
		return "", errors.New("temp directory is unavailable")
	}
	// SCOPED TO THIS USER, unlike the cache-derived root, because this one lives
	// in shared temp. On Linux os.TempDir() is the world-writable /tmp whenever
	// TMPDIR is unset, and a digest of the workspace path alone is the same string
	// for every user on the host: two accounts working on the same path would name
	// one directory and the first one there would own it. The uid is not a secret
	// and is not doing secrecy work; it removes the collision, and
	// refuseAliasedRuntimeComponents handles somebody having got there first.
	digest := sha256.Sum256([]byte(workspaceRoot + "\x00" + sandboxRuntimeUserScope()))
	root := filepath.Join(append(append([]string{tempRoot}, sandboxRuntimeFallbackOwnedNames()...), hex.EncodeToString(digest[:8]))...)
	if runtimeRootWithinWorkspace(workspaceRoot, root) {
		// Both candidates land inside the workspace, so there is nowhere left to
		// put a runtime tree the workspace's own policy does not govern. Refused
		// rather than pointed somewhere arbitrary: a runtime root inside the
		// workspace makes the sandbox's own cache writes indistinguishable from
		// the work it is meant to be confining.
		return "", fmt.Errorf("sandbox runtime root %q would fall inside workspace %q; "+
			"open the workspace somewhere other than the cache or temp directory", root, workspaceRoot)
	}
	return root, nil
}

func sandboxRuntimeEnvironment(env []string, runtimeState *SandboxRuntime) []string {
	if runtimeState == nil || strings.TrimSpace(runtimeState.Root) == "" {
		return env
	}
	overrides := []string{
		"XDG_CACHE_HOME=" + runtimeState.Cache,
		"TMPDIR=" + runtimeState.Temp,
		"TMP=" + runtimeState.Temp,
		"TEMP=" + runtimeState.Temp,
		"npm_config_cache=" + filepath.Join(runtimeState.Cache, "npm"),
		"YARN_CACHE_FOLDER=" + filepath.Join(runtimeState.Cache, "yarn"),
		"COREPACK_HOME=" + filepath.Join(runtimeState.Cache, "corepack"),
		"PIP_CACHE_DIR=" + filepath.Join(runtimeState.Cache, "pip"),
		"GOCACHE=" + filepath.Join(runtimeState.Cache, "go-build"),
		"GOMODCACHE=" + filepath.Join(runtimeState.Data, "go-mod"),
		"CARGO_HOME=" + filepath.Join(runtimeState.Data, "cargo"),
	}
	return upsertEnvList(env, overrides...)
}

func permissionProfileWithRuntime(profile PermissionProfile, runtimeState SandboxRuntime) PermissionProfile {
	profile.Runtime = &runtimeState
	if profile.FileSystem.Kind != FileSystemRestricted || runtimeState.Root == "" {
		return profile
	}
	for _, root := range profile.FileSystem.WriteRoots {
		if filepath.Clean(root.Root) == filepath.Clean(runtimeState.Root) {
			return profile
		}
	}
	profile.FileSystem.WriteRoots = append(profile.FileSystem.WriteRoots, WritableRoot{Root: runtimeState.Root})
	return profile
}

// canonicalSandboxWorkspaceRoot normalizes a workspace root the way
// Engine.resolveCommandDir already does — clean, absolutize, then resolve
// symlinks — so every derivation keyed to a workspace agrees on the string.
//
// The runtime root is a hash of this, and the elevated Windows setup grants the
// principal that tree while commands derive it again. Cleaning alone was not
// enough for the two to agree, and it does not take a symlink for them to
// differ: a path opened in different casing, or through an 8.3 short name (what
// a Windows CI runner's TEMP looks like), resolves to a different spelling.
// Setup then granted one tree and every command used another, so the grant that
// makes npm/go/pip caches writable landed where nothing reads and surfaced as a
// bare ACCESS_DENIED.
//
// Resolution failing is not an error: an unresolvable root still needs a stable
// key, and falling back to the cleaned absolute path is what the command path
// does too.
func canonicalSandboxWorkspaceRoot(root string) string {
	cleaned := filepath.Clean(strings.TrimSpace(root))
	if cleaned == "" || cleaned == "." {
		return ""
	}
	if !filepath.IsAbs(cleaned) {
		if absolute, err := filepath.Abs(cleaned); err == nil {
			cleaned = absolute
		}
	}
	// EvalSymlinks fails outright when the LEAF does not exist, which is the
	// normal case for a cache or runtime root that has not been created yet. A
	// plain call therefore resolved an existing workspace while leaving a
	// not-yet-created cache root unresolved, and the two were compared against
	// each other — the containment check that decides whether the runtime tree
	// must move out of the workspace then ran on /private/var/... versus
	// /var/..., missed, and left the tree inside the workspace.
	//
	// Resolve the longest existing ancestor and re-append the rest, so a path
	// normalizes the same way whether or not its final segments exist yet.
	remainder := ""
	current := cleaned
	for {
		if resolved, err := filepath.EvalSymlinks(current); err == nil {
			if remainder == "" {
				return resolved
			}
			return filepath.Join(resolved, remainder)
		}
		parent := filepath.Dir(current)
		if parent == current {
			// Nothing along the path resolved; the cleaned absolute form is the
			// best stable key available.
			return cleaned
		}
		remainder = filepath.Join(filepath.Base(current), remainder)
		current = parent
	}
}

// selectSandboxRuntimeRoot picks the runtime root a command will actually use,
// and holds a lease on it while the caller decides what to do with it.
//
// SETUP AND THE COMMAND HAVE TO SELECT THE SAME WAY, or they disagree about
// which tree exists. Setup used to derive the cache-based root and fingerprint a
// plan naming it, while a command derived the same root, failed to lease it, and
// silently relocated to the temp fallback. The command's plan then named the
// fallback and the marker rejected it:
//
//	windows sandbox setup is out of date: permission roots or deny lists changed
//
// which blames permissions for a runtime-root disagreement, and re-running setup
// could not recover because setup deterministically chose the same unleasable
// root again. That is a permanent brick, not a retry.
//
// Extracted from prepareSandboxRuntime so both sides run this one function. The
// caller must release the returned lease.
// pinnedSandboxRuntimeRoot returns the root a previous setup recorded, when
// that root is one this workspace could actually select.
//
// The candidate check is what keeps this honest. One sandbox home serves
// whichever workspace ran setup last, so a recorded root can belong to a
// different workspace entirely; pinning to that would point this command's
// runtime at another workspace's tree. A recorded root is only honoured when it
// matches one of the two roots THIS workspace derives, which is also the only
// pair the selections could ever have disagreed about.
// sandboxHome is the home THIS command asked for, not the one the parent
// process happens to be pointed at.
//
// TWO ENVIRONMENT AUTHORITIES IS ONE TOO MANY. Runtime preparation ran before
// Windows platform planning and resolved the home from the ambient environment,
// while the planner resolves ZERO_WINDOWS_SANDBOX_HOME out of the command's own
// spec.Env and hands THAT to the runner for marker validation. A request that
// selects home B while the parent still points at home A pinned A's recorded
// root into the profile, and the runner then loaded B's marker and rejected the
// command as out of date even though setup for B was perfectly valid. The two
// homes do not need different derivation rules to disagree, only different valid
// selections from the same preferred/fallback pair.
//
// Empty means no command context, so the ambient environment is the only
// authority there is and resolving it here is correct.
func pinnedSandboxRuntimeRoot(workspaceRoot, preferred, fallback, sandboxHome string) string {
	// No GOOS gate. The marker only exists where setup wrote one, so this is
	// already Windows-only in practice, and keeping the code path platform-neutral
	// means the setup-to-command contract is exercised on every CI runner instead
	// of only the Windows one.
	home := strings.TrimSpace(sandboxHome)
	if home == "" {
		resolved, err := ResolveWindowsSandboxHome(nil)
		if err != nil {
			return ""
		}
		home = resolved
	}
	recorded := WindowsSandboxRecordedRuntimeRoot(home)
	if recorded == "" {
		return ""
	}
	for _, candidate := range []string{preferred, fallback} {
		if candidate != "" && sameWindowsRuntimeRootPath(recorded, candidate) {
			return candidate
		}
	}
	// A RECORD IS HISTORY, NOT SOMETHING TO RE-DERIVE.
	//
	// The candidates above are both computed from THIS process's environment, and
	// the fallback one is computed from its TEMP. Setup can legitimately land on
	// the fallback when the preferred cache root cannot be leased, and it records
	// that concrete root. If a later IDE, service or terminal runs with a
	// different TEMP, the recorded root matches neither of today's candidates, the
	// record is ignored, and selection picks a tree setup never provisioned. The
	// runner then rejects the marker as out of date, and re-running setup from the
	// original environment does not make the other environment converge.
	//
	// So the recorded root is recognised by its OWN shape instead: it must be a
	// runtime root Zero owns, its first component must be the fallback's, and its
	// leaf must be this workspace's fallback digest. That answers "does this
	// record belong to this workspace and home" without asking "what would I
	// choose from scratch right now", which is the question that reintroduced the
	// second selection.
	//
	// The cache-derived digest is deliberately NOT accepted here. A moved user
	// cache stays stale, which is a different failure with a different remedy.
	if ownedFallbackRuntimeRecord(workspaceRoot, recorded) {
		return recorded
	}
	return ""
}

// ownedFallbackRuntimeRecord reports whether recorded is a fallback runtime root
// this workspace would own, judged on the record's own shape.
func ownedFallbackRuntimeRecord(workspaceRoot, recorded string) bool {
	workspaceRoot = canonicalSandboxWorkspaceRoot(workspaceRoot)
	if workspaceRoot == "" || strings.TrimSpace(recorded) == "" {
		return false
	}
	base, components, owned := windowsSandboxRuntimeOwnedTail(recorded)
	if !owned || base == "" || len(components) == 0 {
		return false
	}
	names := sandboxRuntimeFallbackOwnedNames()
	if len(names) == 0 || !strings.EqualFold(components[0], names[0]) {
		return false
	}
	digest := sha256.Sum256([]byte(workspaceRoot + "\x00" + sandboxRuntimeUserScope()))
	return strings.EqualFold(components[len(components)-1], hex.EncodeToString(digest[:8]))
}

// resolveSandboxRuntimeRootInputs is the part of runtime-root selection that
// names things and touches nothing: the canonical workspace, and the root a
// command prefers for it. It takes no lease and creates no directory.
//
// ONE RESOLVER, BECAUSE THERE WERE TWO AND THEY DISAGREED. Selection and the
// diagnostic that asks "would a command still select the recorded root?" each
// spelled these steps out, and the diagnostic's copy was missing one: a user
// cache directory that resolves to nothing. os.UserCacheDir can succeed and
// still hand back a value that canonicalizes to "" or ".", and selection fails
// closed on that with "user cache directory is unavailable" while the
// diagnostic carried on and derived candidates from the empty root. Doctor
// could then report the recorded root as current, or as merely stale, on a
// machine where every command fails before it ever looks at a marker. That is
// the false-healthy signal this whole change exists to remove, reached through
// a different edge of the same resolver.
//
// The diagnostic's doc comment already said it asked the command's question
// through the command's function. Now it does: both callers get their inputs
// here, so an input one of them refuses is refused by the other, with the same
// error. Reported by @jatmn.
func resolveSandboxRuntimeRootInputs(workspaceRoot string) (workspace string, preferred string, err error) {
	workspace = canonicalSandboxWorkspaceRoot(workspaceRoot)
	if workspace == "" || workspace == "." {
		return "", "", errors.New("sandbox runtime requires a workspace root")
	}
	cacheRoot, err := sandboxUserCacheDir()
	if err != nil {
		return "", "", fmt.Errorf("resolve user cache directory: %w", err)
	}
	cacheRoot = canonicalSandboxWorkspaceRoot(cacheRoot)
	if cacheRoot == "" || cacheRoot == "." {
		return "", "", errors.New("user cache directory is unavailable")
	}
	preferred, err = sandboxRuntimeRootFor(workspace, cacheRoot)
	if err != nil {
		return "", "", err
	}
	return workspace, preferred, nil
}

// WindowsSandboxRecordedRuntimeRootIsCurrent answers the question a diagnostic
// has to ask before trusting the marker: would a command run NOW still select
// the runtime root that setup recorded?
//
// The marker's root is a historical fact, needed to check the stamp without
// mutating anything. It is not proof that the root is still selectable. Setup
// can run with the user cache at A, the cache can then move so commands derive
// B, and the stamped A tree stays behind. A diagnostic that pins A reports a
// healthy machine while every real command rejects A as not a current
// candidate and fails on the out-of-date marker.
//
// This derives the same candidates a command derives, through the same
// resolver, and applies the same equality, by calling the same function the
// command path calls. It takes no lease and creates nothing.
func WindowsSandboxRecordedRuntimeRootIsCurrent(sandboxHome, workspaceRoot string) (recorded string, current bool, err error) {
	recorded = WindowsSandboxRecordedRuntimeRoot(sandboxHome)
	if recorded == "" {
		return "", false, nil
	}
	workspaceRoot, preferred, err := resolveSandboxRuntimeRootInputs(workspaceRoot)
	if err != nil {
		return recorded, false, err
	}
	fallback, _ := fallbackSandboxRuntimeRoot(workspaceRoot)
	return recorded, pinnedSandboxRuntimeRoot(workspaceRoot, preferred, fallback, sandboxHome) != "", nil
}

// selectSandboxRuntimeRoot picks the runtime root and returns the lease on it
// together with the directories acquiring that lease had to create.
//
// honorRecorded is true on the command side and false during setup: setup is
// making the choice, so it must not consult a record it is about to overwrite,
// or a single unlucky relocation to the temp fallback would pin every future
// setup to temp.
//
// THE LEDGER IS PART OF THE ANSWER, NOT A DETAIL OF IT. Acquiring the lease
// creates zero/runtime/v1 when they are not there, and this was the two-result
// wrapper that dropped that fact on the floor. Setup then provisioned the leaf,
// recorded only the leaf for rollback, and any failure before the marker left
// the parents behind with nothing that knew it had made them. An error carries
// the partial ledger out for the same reason: acquisition can create one
// component and fail on the next.
func selectSandboxRuntimeRoot(workspaceRoot string, honorRecorded bool, sandboxHome string) (string, *sandboxRuntimeLease, []windowsCreatedRuntimeDir, error) {
	workspaceRoot, root, err := resolveSandboxRuntimeRootInputs(workspaceRoot)
	if err != nil {
		return "", nil, nil, err
	}
	// CONSUME SETUP'S CHOICE, do not re-make it. Everything below is a fresh
	// selection whose answer depends on whether a lease can be taken right now,
	// and a command reaching a different answer than setup did is the outage this
	// contract exists to prevent: the tree the command names was never
	// provisioned, so its ACL plan hash cannot match and no amount of re-running
	// setup fixes it.
	if honorRecorded {
		fallbackRoot, _ := fallbackSandboxRuntimeRoot(workspaceRoot)
		if pinned := pinnedSandboxRuntimeRoot(workspaceRoot, root, fallbackRoot, sandboxHome); pinned != "" {
			lease, pinnedCreated, leaseErr := prepareSandboxRuntimeLeaseRecording(pinned)
			if leaseErr != nil {
				// NOT relocated. Relocating is what produced the permanent brick:
				// the other root has no capability ACL, so the command would be
				// rejected anyway, with a message about permissions. Failing here
				// says the true thing and points at the action that fixes it.
				return "", nil, pinnedCreated, fmt.Errorf("sandbox runtime root %s was provisioned by setup but cannot be used now (%w); "+
					"re-run `zero sandbox setup` from an elevated (Administrator) terminal", pinned, leaseErr)
			}
			return pinned, lease, pinnedCreated, nil
		}
	}
	lease, created, err := prepareSandboxRuntimeLeaseRecording(root)
	if err == nil {
		return root, lease, created, nil
	}
	// AN ALIASED COMPONENT IS NOT A REASON TO RELOCATE. Falling back here would
	// leave the link in place, report nothing, and move to the next predictable
	// name, which the same attacker can take as well. Relocating is for a root
	// that is merely unusable.
	if errors.Is(err, errRuntimeComponentAliased) {
		return "", nil, created, err
	}
	// The preferred root could not be leased. Relocating is right, and it is what
	// commands already did; the defect was that setup never learned about it.
	root, err = fallbackSandboxRuntimeRoot(workspaceRoot)
	if err != nil {
		return "", nil, created, err
	}
	// APPENDED, not replaced. The preferred attempt may have created components
	// before it failed to lease, and those are this invocation's too.
	fallbackLease, fallbackCreated, err := prepareSandboxRuntimeLeaseRecording(root)
	created = append(created, fallbackCreated...)
	if err != nil {
		return "", nil, created, err
	}
	return root, fallbackLease, created, nil
}
