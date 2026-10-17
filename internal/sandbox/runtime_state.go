package sandbox

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Twigpine/zero/internal/peermsg"
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
	root := filepath.Join(cacheRoot, "zero", "runtime", "v1", hex.EncodeToString(digest[:8]))
	return root, !pathWithinRoot(workspaceRoot, root)
}

func prepareSandboxRuntime(workspaceRoot string) (SandboxRuntime, func(), error) {
	workspaceRoot = canonicalSandboxWorkspaceRoot(workspaceRoot)
	if workspaceRoot == "" || workspaceRoot == "." {
		return SandboxRuntime{}, nil, errors.New("sandbox runtime requires a workspace root")
	}
	cacheRoot, err := sandboxUserCacheDir()
	if err != nil {
		return SandboxRuntime{}, nil, fmt.Errorf("resolve user cache directory: %w", err)
	}
	// Canonicalized the SAME way as the workspace root, because
	// sandboxRuntimeRootFor compares the two: it falls back to a private temp
	// tree when the derived runtime root would land inside the workspace.
	// Normalizing only one side made that comparison run on two different
	// spellings of the same path — /var vs /private/var on macOS, an 8.3 short
	// name vs its long form on Windows — so the containment check missed and the
	// fallback never fired.
	cacheRoot = canonicalSandboxWorkspaceRoot(cacheRoot)
	if cacheRoot == "" || cacheRoot == "." {
		return SandboxRuntime{}, nil, errors.New("user cache directory is unavailable")
	}
	root, err := sandboxRuntimeRootFor(workspaceRoot, cacheRoot)
	if err != nil {
		return SandboxRuntime{}, nil, err
	}
	// THE ANCHOR IS PROVEN BEFORE THE TREE IS BUILT, on both paths that can
	// hand back a fallback root: sandboxRuntimeRootFor returns one itself when
	// the cache lands inside the workspace, and the branch below asks for one
	// when the cache root cannot be leased. A deterministic name under the
	// shared temp is only safe beneath a directory this user owns; if the anchor
	// is a link, or exists and belongs to someone else, this refuses rather than
	// leasing, creating and chmod-ing through it.
	if err := ensureFallbackRuntimeAnchor(root); err != nil {
		return SandboxRuntime{}, nil, err
	}
	lease, err := prepareSandboxRuntimeLease(root)
	if err != nil {
		root, err = fallbackSandboxRuntimeRoot(workspaceRoot)
		if err != nil {
			return SandboxRuntime{}, nil, err
		}
		if err := ensureFallbackRuntimeAnchor(root); err != nil {
			return SandboxRuntime{}, nil, err
		}
		lease, err = prepareSandboxRuntimeLease(root)
		if err != nil {
			return SandboxRuntime{}, nil, err
		}
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
	directories := []string{
		runtimeState.Root,
		runtimeState.Cache,
		runtimeState.Data,
		runtimeState.Temp,
		filepath.Join(runtimeState.Cache, "npm"),
		filepath.Join(runtimeState.Cache, "yarn"),
		filepath.Join(runtimeState.Cache, "corepack"),
		filepath.Join(runtimeState.Cache, "pip"),
		filepath.Join(runtimeState.Cache, "go-build"),
		filepath.Join(runtimeState.Data, "go-mod"),
		filepath.Join(runtimeState.Data, "cargo"),
	}
	// Handle-relative and no-follow below the operator-owned base. The tree is
	// persistent input from the previous sandboxed command, which is granted write
	// access to cache, data and tmp and can leave a link in place of one of them.
	// os.MkdirAll and os.Chmod both follow, so the host process created package
	// caches wherever that command pointed them. See ensureRuntimeTreeDirs.
	if err := ensureRuntimeTreeDirs(runtimeState.Root, directories); err != nil {
		return SandboxRuntime{}, nil, err
	}
	now := sandboxRuntimeNow()
	if err := os.Chtimes(runtimeState.Root, now, now); err != nil {
		return SandboxRuntime{}, nil, fmt.Errorf("touch sandbox runtime root: %w", err)
	}
	cleanupSandboxRuntimeRoots(filepath.Dir(runtimeState.Root), runtimeState.Root, now)
	prepared = true
	return runtimeState, lease.release, nil
}

func prepareSandboxRuntimeLease(root string) (*sandboxRuntimeLease, error) {
	// THE OWNED TAIL IS VALIDATED BEFORE THE FIRST EFFECT, AND THE LEASE IS
	// OPENED THROUGH THAT VALIDATION. The anchor check above this covers the
	// anchor; the reusable "v1" child beneath it is the runtime's too, and
	// creating it by pathname followed whatever was sitting at that name.
	// Redirect v1 into another directory and the old sequence created a .lease
	// there and only then failed validation. EnsurePrivateDir walks each
	// component without following links and creates what is missing in place;
	// the lease is then opened relative to a no-follow handle on that parent, so
	// a component swapped in after the check cannot move the open either.
	parent := filepath.Dir(root)
	if err := peermsg.EnsurePrivateDir(parent); err != nil {
		return nil, fmt.Errorf("sandbox runtime parent %s is not a private directory owned by this user: %w", parent, err)
	}
	return acquireSandboxRuntimeLeaseIn(parent, filepath.Base(root)+sandboxRuntimeLeaseSuffix)
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

// fallbackRuntimeAnchor is the per-user directory the deterministic fallback
// runtime tree lives beneath.
//
// DETERMINISM IS NOT OWNERSHIP. The fallback moved from a fresh, private
// os.MkdirTemp parent to a predictable name so that elevated setup and the
// later command process agree on the runtime root. That agreement is required
// and must stay. But a predictable path directly under the shared system temp
// is not a private allocation: on an ordinary shared /tmp another local user
// can create /tmp/zero with mode 0700 first, and every affected user then fails
// before the sandbox command starts; a pre-existing redirected component makes
// host-side preparation operate somewhere other than the derived tree.
//
// So the tree is rooted beneath an anchor that names THIS user, and the anchor
// is created and validated by peermsg.EnsurePrivateDir before anything is
// built under it: handle-relative descent, no-follow, refused if any component
// is a link or the leaf is not owned by the current user, mode 0700. On Unix
// the uid is in the name because /tmp is shared between users; on Windows
// os.TempDir is already per-user, and the private descriptor plus the owner
// check do the rest.
func fallbackRuntimeAnchor() string {
	tag := "user"
	if runtime.GOOS != "windows" {
		tag = strconv.Itoa(os.Getuid())
	}
	// PHYSICAL PARENT, OWNED LEAF. The validator walks every component no-follow
	// and refuses links, which is right for the anchor Zero owns and wrong for
	// the operator's ancestors above it: macOS puts TempDir under /var, a
	// symlink to /private/var, and a redirected %LOCALAPPDATA% is ordinary on
	// Windows. The first version handed the validator the unresolved path and
	// refused every fallback on every Mac. Resolving the parent first means the
	// only new component the validator sees is the anchor itself.
	//
	// On a resolve error the unresolved path is used, deliberately: the
	// validator is still the fail-closed check, and refusing there names the
	// real component rather than hiding it behind a resolver failure.
	base := os.TempDir()
	if physical, err := physicalTempDir(); err == nil && physical != "" {
		base = physical
	}
	return filepath.Join(base, "zero-runtime-"+tag)
}

// isFallbackRuntimeRoot reports whether root was derived by
// fallbackSandboxRuntimeRoot, so the caller knows the anchor must be validated
// before the tree beneath it is prepared. Both candidates a command derives can
// be fallback roots: sandboxRuntimeRootFor returns one itself when the cache
// lands inside the workspace, and prepareSandboxRuntime asks for one when the
// cache root cannot be leased.
func isFallbackRuntimeRoot(root string) bool {
	// pathWithinRoot(parent, child): is root beneath the anchor. The first
	// draft had these reversed, which asked whether the anchor was beneath the
	// root, answered false every time, and skipped the validation entirely
	// while every test that depended on it reported success through a
	// junction. Argument order is the whole function.
	return pathWithinRoot(fallbackRuntimeAnchor(), root)
}

// ensureFallbackRuntimeAnchor validates the anchor beneath which a fallback
// runtime tree is about to be built, and fails closed. Called only when the
// root is a fallback root; the cache-derived root lives under the user cache,
// which is already the user's own directory.
func ensureFallbackRuntimeAnchor(root string) error {
	if !isFallbackRuntimeRoot(root) {
		return nil
	}
	anchor := fallbackRuntimeAnchor()
	if err := peermsg.EnsurePrivateDir(anchor); err != nil {
		return fmt.Errorf("sandbox runtime fallback anchor %s is not a private directory owned by this user: %w", anchor, err)
	}
	return nil
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
	digest := sha256.Sum256([]byte(workspaceRoot))
	root := filepath.Join(fallbackRuntimeAnchor(), "v1", hex.EncodeToString(digest[:8]))
	if pathWithinRoot(workspaceRoot, root) {
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
