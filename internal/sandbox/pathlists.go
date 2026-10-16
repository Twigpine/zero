package sandbox

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/Gitlawb/zero/internal/remotetoken"
)

// This file implements the fine-grained AllowRead/DenyRead/AllowWrite/DenyWrite
// path lists (Policy fields). They layer ON TOP of the workspace + Scope guards
// and never bypass them: AllowRead only re-includes inside a DenyRead carve-out,
// AllowWrite is consulted only after the workspace/Scope guard already denied a
// write, and every match is symlink-resolved so a symlink prefix cannot evade a
// deny or sneak past an allow. All lists default empty, so an unconfigured policy
// behaves exactly as before.

// resolvePolicyPath home-expands, makes absolute, and symlink-resolves a single
// policy path entry. ok is false for a blank entry or one that does not exist
// (EvalSymlinks requires existence) so a bogus entry is dropped — a non-existent
// deny protects nothing and a non-existent allow grants nothing.
func resolvePolicyPath(entry string) (string, bool) {
	trimmed := strings.TrimSpace(entry)
	if trimmed == "" {
		return "", false
	}
	if trimmed == "~" || strings.HasPrefix(trimmed, "~/") || strings.HasPrefix(trimmed, "~"+string(filepath.Separator)) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", false
		}
		trimmed = filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(trimmed[1:], "/"), string(filepath.Separator)))
	}
	absolute, err := filepath.Abs(trimmed)
	if err != nil {
		return "", false
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", false
	}
	return resolved, true
}

// These aliases keep the sandbox tests and environment boundary tied to the
// shared token-source model instead of duplicating string literals.
const (
	daemonRemoteTokenEnv             = remotetoken.EnvToken
	daemonRemoteTokenFileEnv         = remotetoken.EnvTokenFile
	daemonRemoteTokenFileResolvedEnv = remotetoken.EnvTokenFileResolved
	daemonRemoteTokenFileIdentityEnv = remotetoken.EnvTokenFileIdentity
)

// The daemon-token pathname contract
//
// Every layer that interprets ZERO_DAEMON_REMOTE_TOKEN_FILE must agree on the
// SAME bytes, or one layer protects a pathname a different layer never checks.
// Each round of review on this surface came from a new place that disagreed, so
// the rule is written down once here:
//
//  1. The env value is pathname DATA, not a word. Only an all-whitespace value
//     counts as unset (remotetoken.SelectedFilePath). It is never trimmed,
//     never shell-split, and "~" is never expanded — os.ReadFile, the daemon's
//     own reader, treats it literally.
//  2. The token source carries two identities: the operator-configured absolute
//     spelling and the object resolved at daemon startup. Both remain protected;
//     resolving never overwrites the only configured identity.
//  3. Tool arguments are compared as EXACT bytes, because that is what the tool
//     opens (aliasedStringArg does not trim). requestPaths gates on the exact
//     argument. Filesystem-derived case equivalence is applied only by the final
//     containment comparison; it does not rewrite the argument bytes.
//  4. Protection is NOT re-includable. AllowRead, a permission grant, and a
//     session profile all leave it in place, on every platform.
//
// A new consumer of the token pathname belongs on one of these four rules. If
// it needs a fifth, the contract is what changes — not just that call site.
//
// protectedCredentialPaths returns credential files that Zero's own in-process
// file tools must never read or modify, independent of Policy.
//
// The shared token source retains the operator-configured absolute spelling and
// the object resolved at daemon startup. The former reserves the authority
// boundary across replacement and restart; the latter protects the bearer object
// currently held by the daemon. A non-daemon caller without the resolved marker
// resolves the current target best-effort.
func protectedCredentialPaths() []string {
	source, selected := remotetoken.SourceFromEnv()
	if !selected {
		return nil
	}
	return dedupeStrings(source.Paths())
}

// protectedCredentialPathBlock returns the block for the first requested path
// that targets a protected credential file, or nil. It exists for the callers
// that bypass validatePathWithPolicy — currently the ModeDisabled short-circuit,
// where the exclusion still applies because it is not policy-derived.
//
// Only the side effects that name a path are covered. SideEffectShell is not,
// and cannot be: a shell request carries a command line, not a file path, so
// there is nothing here to compare against the token. Shell is confined by the
// OS wrapper instead — which under ModeDisabled does not exist. See the
// ModeDisabled short-circuit in Engine.Evaluate for what that boundary means.
func protectedCredentialPathBlock(request Request, workspaceRoot string) *pathBlock {
	switch request.SideEffect {
	case SideEffectRead, SideEffectWrite, SideEffectOutOfWorkspace:
	default:
		return nil
	}
	protected := protectedCredentialPaths()
	if len(protected) == 0 {
		return nil
	}
	verb := "readable"
	if request.SideEffect != SideEffectRead {
		verb = "writable"
	}
	for _, path := range requestPaths(request) {
		if protectedPathDenied(protected, workspaceRoot, path) {
			return &pathBlock{
				Code:   BlockDenied,
				Path:   path,
				Reason: path + " holds the remote bridge token and is never " + verb,
			}
		}
	}
	return nil
}

// protectedPathDenied reports whether path targets one of the protected
// credential files. There is no allow-list consultation by design.
func protectedPathDenied(protected []string, workspaceRoot, path string) bool {
	return newProtectedPathCache(protected).denied(workspaceRoot, path)
}

// protectedPathCache resolves the parts of the check that depend only on the
// protected set and not on the path being tested: the owning filesystem’s case
// semantics per entry, and each entry’s os.FileInfo for the inode comparison. A
// search walk builds one and reuses it for every visited file, so PathExcluded
// stops paying several stat calls per entry across a large repository.
//
// The cached FileInfo is a snapshot taken when the exclusions are built. That is
// the right lifetime for one walk or one request; the lexical rule below needs
// no stat at all and is what keeps the configured pathname reserved when the
// token is rotated underneath a long walk.
type protectedPathCache struct {
	roots    []string
	folds    []bool
	infos    []os.FileInfo
	identity string
}

func newProtectedPathCache(roots []string) *protectedPathCache {
	if len(roots) == 0 {
		return nil
	}
	cache := &protectedPathCache{
		roots: roots,
		folds: make([]bool, len(roots)),
		infos: make([]os.FileInfo, len(roots)),
	}
	cache.identity = os.Getenv(daemonRemoteTokenFileIdentityEnv)
	for index, root := range roots {
		cache.folds[index] = protectedPathFoldsCase(root)
		if info, err := os.Stat(root); err == nil {
			cache.infos[index] = info
		}
	}
	return cache
}

// denied reports whether path targets one of the protected credential files.
// There is no allow-list consultation by design.
func (cache *protectedPathCache) denied(workspaceRoot, path string) bool {
	if cache == nil {
		return false
	}
	for index, root := range cache.roots {
		if pathUnderProtectedRootFolding(path, root, workspaceRoot, cache.folds[index]) {
			return true
		}
	}

	// Keep the lexical check above: in particular, it protects a configured
	// pathname even when the file is absent, preventing its replacement. For an
	// existing request, also compare the object reached by the filesystem. This
	// closes aliases created after the token path was selected: EvalSymlinks
	// catches symbolic links, while SameFile catches hard links (and any other
	// platform-specific names for the same file).
	//
	// What this closes and what it does NOT: the comparison is made BEFORE the
	// tool opens the path, so it catches every alias that exists at check time
	// and stays put — a symlink or hard link created after the token path was
	// selected. It is not an open-time binding, so a writer that repoints a
	// symlink between this check and the tool's own open still wins that race.
	// Closing that window means passing an open handle down to the comparison,
	// which the tool layer does not do today: read_file and friends resolve a
	// path argument and open it themselves. ReadExclusions.FileExcluded is the
	// handle-bound form for the callers that DO own the open (MCP resources/read
	// reads through it), and it is where the rest should migrate.
	//
	// The OS layer is pathname-based and
	// stays that way: seatbelt and Bubblewrap rules name paths, so a sandboxed
	// shell on macOS can still `ln <token> alias && cat alias` — a hard link is a
	// second name for the same inode, and no path-based rule covers a name that
	// did not exist when the profile was built. That is the same model a
	// user-configured DenyRead has always had, deliberately: an aliasing defense
	// at the OS layer would have to resolve every path at open time, which is not
	// something either backend's policy language expresses. Shell access to a
	// host running a remote bridge is therefore access to the token, and the
	// protection here is the in-process boundary plus the pathname deny rules,
	// not an inode-tight OS guarantee.
	abs := path
	if !filepath.IsAbs(abs) {
		if workspaceRoot == "" {
			return false
		}
		abs = filepath.Join(workspaceRoot, abs)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return false
	}
	requestInfo, err := os.Stat(resolved)
	if err != nil {
		return false
	}
	// A protected credential is a regular file, so a directory, device, or
	// socket reached by a walk can never be an alias of one. Deciding that from
	// the stat already taken keeps the SameFile comparisons off the majority of
	// the entries in a tree walk.
	if !requestInfo.Mode().IsRegular() {
		return false
	}
	if identity, ok := remotetoken.IdentityOf(requestInfo); ok && identity == cache.identity && identity != "" {
		return true
	}
	for _, protectedInfo := range cache.infos {
		if protectedInfo == nil {
			continue
		}
		if os.SameFile(requestInfo, protectedInfo) {
			return true
		}
	}
	return false
}

// protectedInfoDenied compares an ALREADY-OPEN object against the protected
// credential set. The caller holds the handle its os.FileInfo came from, so the
// answer describes the object that will actually be read rather than whatever
// the pathname resolves to on a second look — there is no window between this
// check and the use. Callers that own the open should prefer it; a caller that
// only has a pathname is stuck with the pre-open comparison above.
func protectedInfoDenied(protected []string, file *os.File, info os.FileInfo) bool {
	if info == nil {
		return false
	}
	identity, ok := remotetoken.IdentityOfFile(file)
	if !ok {
		identity, ok = remotetoken.IdentityOf(info)
	}
	if startupIdentity := os.Getenv(daemonRemoteTokenFileIdentityEnv); startupIdentity != "" {
		// An actual consumer must not fall back to current pathname metadata
		// when the startup identity cannot be established for its open handle.
		if file != nil && !ok || ok && identity == startupIdentity {
			return true
		}
	}
	for _, entry := range protected {
		protectedInfo, err := os.Stat(entry)
		if err == nil && os.SameFile(info, protectedInfo) {
			return true
		}
	}
	return false
}

type pathCaseSemantics uint8

const (
	pathCaseUnknown pathCaseSemantics = iota
	pathCaseSensitive
	pathCaseInsensitive
)

// protectedPathFoldsCase derives name equivalence from the filesystem that owns
// the protected path, not from GOOS. For an absent token it probes the nearest
// existing ancestor, which preserves the reservation through rotation. An
// indeterminate result fails closed by folding.
func protectedPathFoldsCase(path string) bool {
	return detectPathCaseSemantics(path, os.Stat) != pathCaseSensitive
}

func detectPathCaseSemantics(path string, stat func(string) (os.FileInfo, error)) pathCaseSemantics {
	current, err := filepath.Abs(path)
	if err != nil {
		return pathCaseUnknown
	}
	current = filepath.Clean(current)
	for {
		info, statErr := stat(current)
		if statErr == nil {
			name := filepath.Base(current)
			if variantName, ok := caseVariant(name); ok {
				variant := filepath.Join(filepath.Dir(current), variantName)
				variantInfo, variantErr := stat(variant)
				switch {
				case variantErr == nil && os.SameFile(info, variantInfo):
					return pathCaseInsensitive
				case variantErr == nil:
					return pathCaseSensitive
				case os.IsNotExist(variantErr):
					return pathCaseSensitive
				default:
					return pathCaseUnknown
				}
			}
		} else if !os.IsNotExist(statErr) {
			return pathCaseUnknown
		}
		parent := filepath.Dir(current)
		if parent == current {
			return pathCaseUnknown
		}
		current = parent
	}
}

func caseVariant(name string) (string, bool) {
	for index := range len(name) {
		switch {
		case name[index] >= 'a' && name[index] <= 'z':
			return name[:index] + string(name[index]-('a'-'A')) + name[index+1:], true
		case name[index] >= 'A' && name[index] <= 'Z':
			return name[:index] + string(name[index]+('a'-'A')) + name[index+1:], true
		}
	}
	return "", false
}

// pathUnderProtectedRootFolding is pathUnderPolicyRoot for the automatic
// credential exclusions: identical anchoring and symlink normalization, plus the
// owning filesystem's case semantics — which the caller supplies, because
// deriving it probes ancestors and depends only on the root. protectedPathCache
// derives it once per root and reuses it for every path a walk visits.
func pathUnderProtectedRootFolding(requestedPath, root, workspaceRoot string, folds bool) bool {
	normalized, ok := normalizePathForPolicyRoot(requestedPath, root, workspaceRoot)
	if !ok {
		return false
	}
	if pathWithinRootExact(root, normalized) {
		return true
	}
	if !folds {
		return false
	}
	return pathWithinRootExact(strings.ToLower(root), strings.ToLower(normalized))
}

func pathWithinRootExact(root, candidate string) bool {
	root = filepath.Clean(root)
	candidate = filepath.Clean(candidate)
	if candidate == root {
		return true
	}
	if filepath.Dir(root) == root {
		return strings.HasPrefix(candidate, root)
	}
	return strings.HasPrefix(candidate, root+string(filepath.Separator))
}

// resolvePolicyPaths resolves and de-duplicates a list of policy path entries,
// dropping blanks and non-existent entries. Files and directories are both kept
// (a DenyRead/DenyWrite entry may target a single sensitive file).
func resolvePolicyPaths(entries []string) []string {
	if len(entries) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(entries))
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		resolved, ok := resolvePolicyPath(entry)
		if !ok {
			continue
		}
		if _, dup := seen[resolved]; dup {
			continue
		}
		seen[resolved] = struct{}{}
		out = append(out, resolved)
	}
	return out
}

// resolveWriteRootPaths is resolvePolicyPaths restricted to existing directories
// that are not the filesystem root — the only valid targets for an OS write bind
// and for an AllowWrite grant root.
func resolveWriteRootPaths(entries []string) []string {
	resolved := resolvePolicyPaths(entries)
	if len(resolved) == 0 {
		return nil
	}
	out := make([]string, 0, len(resolved))
	for _, path := range resolved {
		info, err := os.Stat(path)
		if err != nil || !info.IsDir() {
			continue
		}
		if filepath.Dir(path) == path {
			continue // refuse the filesystem root as a write root
		}
		out = append(out, path)
	}
	return out
}

// pathUnderPolicyRoot reports whether requestedPath lies within root. A relative
// requestedPath is resolved against workspaceRoot; the portion of an absolute
// path outside root is symlink-resolved (via NormalizePrefixForRoot) so a
// symlink prefix cannot evade the match. root must be an already-resolved
// absolute path.
func pathUnderPolicyRoot(requestedPath, root, workspaceRoot string) bool {
	normalized, ok := normalizePathForPolicyRoot(requestedPath, root, workspaceRoot)
	if !ok {
		return false
	}
	return pathWithinRoot(root, normalized)
}

// normalizePathForPolicyRoot anchors requestedPath (a relative one against
// workspaceRoot) and symlink-normalizes the portion outside root, yielding the
// path pathUnderPolicyRoot compares. ok is false when there is nothing to
// compare against: a blank root, or a relative path with no workspace root.
func normalizePathForPolicyRoot(requestedPath, root, workspaceRoot string) (string, bool) {
	if root == "" {
		return "", false
	}
	abs := requestedPath
	if !filepath.IsAbs(abs) {
		if workspaceRoot == "" {
			return "", false
		}
		abs = filepath.Join(workspaceRoot, abs)
	}
	return NormalizePrefixForRoot(abs, root), true
}

// readDenied reports whether path is excluded by the DenyRead list with no
// more-specific AllowRead re-inclusion. "More specific" means an AllowRead entry
// nested inside the matched DenyRead entry — that subtree is read back in while
// the rest of the denied tree stays blocked. It resolves the policy entries on
// each call, so it suits a one-shot check (the Evaluate gate). A search walk that
// checks many paths should use ReadExclusions, which resolves the entries once.
func readDenied(policy Policy, workspaceRoot, path string) bool {
	return readDeniedResolved(workspaceRoot, resolvePolicyPaths(policy.DenyRead), resolvePolicyPaths(policy.AllowRead), path)
}

// readDeniedResolved is readDenied operating on ALREADY-resolved deny/allow roots,
// so a caller that resolved the policy entries once can reuse them across many
// path checks instead of re-running Abs/EvalSymlinks per path.
func readDeniedResolved(workspaceRoot string, denyRoots, allowRoots []string, path string) bool {
	if len(denyRoots) == 0 {
		return false
	}
	for _, deny := range denyRoots {
		if !pathUnderPolicyRoot(path, deny, workspaceRoot) {
			continue
		}
		reincluded := false
		for _, allow := range allowRoots {
			// The allow entry must sit inside the deny entry to be "more specific",
			// and the path must fall under that allow entry.
			if pathWithinRoot(deny, allow) && pathUnderPolicyRoot(path, allow, workspaceRoot) {
				reincluded = true
				break
			}
		}
		if !reincluded {
			return true
		}
	}
	return false
}

// ProtectedCredentialExclusions returns exclusions covering ONLY the automatic,
// non-overrideable credential paths — no policy involved.
//
// Every other exclusion set is built from an Engine, which is the right shape
// for a tool invoked through the agent. But the protected-credential set comes
// from this process's own environment rather than from any policy, so a tool
// reached WITHOUT an engine (MCP, legacy registry.Run) has no reason to be less
// protected than the same tool reached with one. Callers on that path use this
// to keep the bridge bearer token out of their output.
//
// Active() only when a credential is actually selected, so the no-token case
// stays a no-op and the walk behaves exactly as it did before.
func ProtectedCredentialExclusions(workspaceRoot string) ReadExclusions {
	return newReadExclusions(workspaceRoot, nil, nil, protectedCredentialPaths())
}

// newReadExclusions builds the matcher with the per-run constants of the
// protected set resolved once. Every construction site goes through it so the
// cache is never left nil on a set that has entries — a ReadExclusions built as
// a bare literal would re-stat each protected entry per visited path.
func newReadExclusions(workspaceRoot string, denyRoots, allowRoots, protectedRoots []string) ReadExclusions {
	return ReadExclusions{
		workspaceRoot:  workspaceRoot,
		denyRoots:      denyRoots,
		allowRoots:     allowRoots,
		protectedRoots: protectedRoots,
		protected:      newProtectedPathCache(protectedRoots),
	}
}

// ReadExclusions holds the resolved DenyRead/AllowRead roots for a policy so a
// search walk resolves each policy entry ONCE (Abs/EvalSymlinks) and reuses the
// result across every visited path, rather than re-resolving per path. Build it
// with Engine.ReadExclusions and reuse it for the whole grep/glob walk.
type ReadExclusions struct {
	workspaceRoot string
	denyRoots     []string
	allowRoots    []string
	// protectedRoots are the automatic credential exclusions
	// (protectedCredentialPaths); AllowRead never re-includes them.
	protectedRoots []string
	// protected caches what the protected-set check can resolve ahead of the
	// walk. Nil when nothing is protected.
	protected *protectedPathCache
}

// Active reports whether anything is excluded: a configured DenyRead root or an
// automatic protected credential path. When false the exclusions are a no-op and
// the search behaves exactly as before.
func (rx *ReadExclusions) Active() bool {
	return rx != nil && (len(rx.denyRoots) > 0 || len(rx.protectedRoots) > 0)
}

// PathExcluded reports whether reading path is excluded by DenyRead, honoring a
// more-specific AllowRead re-inclusion, or by an automatic credential exclusion,
// which no allow entry re-includes. It is the per-file predicate for a walk.
func (rx *ReadExclusions) PathExcluded(path string) bool {
	if !rx.Active() {
		return false
	}
	if rx.protectedDenied(path) {
		return true
	}
	return readDeniedResolved(rx.workspaceRoot, rx.denyRoots, rx.allowRoots, path)
}

// FileExcluded is PathExcluded for a caller that has already OPENED the object,
// with info taken from that handle (os.File.Stat). Binding the credential
// comparison to the opened object removes the check-to-use window a pathname
// check leaves open: a rename or a repointed symlink between the check and the
// read cannot change what info describes, because it describes the handle the
// caller will read from rather than whatever the name resolves to next.
//
// The pathname rules still apply on top, so a configured token pathname stays
// excluded even when the file behind it does not exist.
func (rx *ReadExclusions) FileExcluded(path string, info os.FileInfo) bool {
	return rx.FileHandleExcluded(path, nil, info)
}

// FileHandleExcluded performs the stable-identity check against the consumed
// handle. This is required on Windows, where FileInfo omits the file index.
func (rx *ReadExclusions) FileHandleExcluded(path string, file *os.File, info os.FileInfo) bool {
	if !rx.Active() {
		return false
	}
	if rx.protectedDenied(path) {
		return true
	}
	if protectedInfoDenied(rx.protectedRoots, file, info) {
		return true
	}
	return readDeniedResolved(rx.workspaceRoot, rx.denyRoots, rx.allowRoots, path)
}

func (rx *ReadExclusions) protectedDenied(path string) bool {
	if rx.protected != nil {
		return rx.protected.denied(rx.workspaceRoot, path)
	}
	// A ReadExclusions built as a bare literal (tests, older call sites) still
	// gets the full check, just without the per-run cache.
	return protectedPathDenied(rx.protectedRoots, rx.workspaceRoot, path)
}

// DirExcluded reports whether a directory subtree can be skipped wholesale during
// a walk: it is read-denied AND contains no nested AllowRead root (descending is
// required to reach a re-included subtree). When it returns false on a denied dir
// because of a nested allow, PathExcluded still filters the denied siblings.
func (rx *ReadExclusions) DirExcluded(path string) bool {
	if !rx.Active() {
		return false
	}
	// A protected credential entry is a file, so it only prunes a directory when
	// the directory IS that entry; PathExcluded still filters it during the walk.
	if rx.protectedDenied(path) {
		return true
	}
	if !readDeniedResolved(rx.workspaceRoot, rx.denyRoots, rx.allowRoots, path) {
		return false
	}
	abs := path
	if !filepath.IsAbs(abs) && rx.workspaceRoot != "" {
		abs = filepath.Join(rx.workspaceRoot, path)
	}
	// allowRoots are symlink-resolved (resolvePolicyPaths), so resolve abs the
	// same way before the prefix comparison — otherwise a dir reached THROUGH a
	// symlink would fail to match a nested AllowRead root and be wrongly skipped,
	// dropping a re-included subtree. Best-effort: keep abs if it can't resolve
	// (e.g. a non-existent path), matching the deny check's tolerant behavior.
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	return !hasNestedAllowReadResolved(rx.allowRoots, abs)
}

// allowWriteScope builds an ad-hoc Scope from the resolved AllowWrite roots so a
// write to an AllowWrite path is validated with the SAME symlink-traversal logic
// the workspace Scope uses. Returns nil when there are no usable AllowWrite roots.
func allowWriteScope(policy Policy) *Scope {
	roots := resolveWriteRootPaths(policy.AllowWrite)
	if len(roots) == 0 {
		return nil
	}
	return &Scope{workspaceRoot: roots[0], extraRoots: roots[1:]}
}

// validateWritePath enforces the write precedence: DenyWrite wins, then (when
// workspace enforcement is on) a workspace/Scope-writable path is allowed, then
// an absolute path under an AllowWrite root is allowed, otherwise the base
// workspace/Scope block stands. The DenyWrite list applies regardless of
// enforceWorkspace; the workspace boundary itself applies only when
// enforceWorkspace. It never bypasses the symlink/out-of-workspace guards.
func validateWritePath(scope *Scope, policy Policy, enforceWorkspace bool, workspaceRoot, path string) *pathBlock {
	// The protected credential files outrank every allow: overwriting or
	// truncating the bridge token denies service, and replacing it hands the next
	// bridge start an attacker-chosen secret.
	if protectedPathDenied(protectedCredentialPaths(), workspaceRoot, path) {
		return &pathBlock{
			Code:   BlockDenied,
			Path:   path,
			Reason: path + " holds the remote bridge token and is never writable",
		}
	}
	// DenyWrite wins regardless of workspace enforcement.
	for _, deny := range resolvePolicyPaths(policy.DenyWrite) {
		if pathUnderPolicyRoot(path, deny, workspaceRoot) {
			return &pathBlock{
				Code:   BlockDenied,
				Path:   path,
				Reason: path + " is excluded by the sandbox DenyWrite policy",
			}
		}
	}
	if !enforceWorkspace {
		// Workspace confinement is off: only the explicit DenyWrite list restricts.
		return nil
	}
	base := scope.validate(path)
	if base == nil {
		return nil // writable under the workspace / Scope guard
	}
	// AllowWrite only extends ABSOLUTE paths: a relative path is inherently
	// workspace-relative and already resolved by the base guard above.
	if filepath.IsAbs(path) {
		if allow := allowWriteScope(policy); allow != nil && allow.validate(path) == nil {
			return nil
		}
	}
	return base
}

// validatePathWithPolicy is the single entry point the engine uses to validate a
// request path. The fine-grained read/write lists (DenyRead/DenyWrite, with
// AllowRead/AllowWrite) apply whenever the sandbox is enforcing, INDEPENDENT of
// enforceWorkspace, so they match the grep/glob path that also honors DenyRead
// directly. The workspace boundary (scope.validate) applies only when
// enforceWorkspace. Behavior is unchanged when the lists are empty.
func validatePathWithPolicy(scope *Scope, policy Policy, sideEffect SideEffect, enforceWorkspace bool, workspaceRoot, path string) *pathBlock {
	// A relative path cannot be anchored without a workspace root, so it cannot be
	// checked against the (absolute) path lists or workspace boundary. Fail closed
	// when there is anything to enforce; otherwise it is a no-op (unchanged from the
	// pre-path-list behavior, where an empty workspace root skipped validation).
	if workspaceRoot == "" && !filepath.IsAbs(path) {
		// A configured bridge token counts as something to enforce: the relative
		// path cannot be anchored, so it cannot be proven to miss the token file.
		if enforceWorkspace || policyHasPathLists(policy) || len(protectedCredentialPaths()) > 0 {
			return &pathBlock{
				Code:   BlockOutsideWorkspace,
				Path:   path,
				Reason: path + " cannot be validated without a workspace root",
			}
		}
		return nil
	}
	switch sideEffect {
	case SideEffectRead:
		if protectedPathDenied(protectedCredentialPaths(), workspaceRoot, path) {
			return &pathBlock{
				Code:   BlockDenied,
				Path:   path,
				Reason: path + " holds the remote bridge token and is never readable",
			}
		}
		if readDenied(policy, workspaceRoot, path) {
			return &pathBlock{
				Code:   BlockDenied,
				Path:   path,
				Reason: path + " is excluded by the sandbox DenyRead policy",
			}
		}
		if enforceWorkspace {
			return scope.validateRead(path)
		}
		return nil
	case SideEffectWrite, SideEffectOutOfWorkspace:
		return validateWritePath(scope, policy, enforceWorkspace, workspaceRoot, path)
	default:
		if enforceWorkspace {
			return scope.validate(path)
		}
		return nil
	}
}

// hasNestedAllowReadResolved reports whether any already-resolved AllowRead root
// sits strictly inside dir (an already-resolved absolute path). When true, a
// read-denied dir must still be descended during a walk so the re-included
// subtree is reachable.
func hasNestedAllowReadResolved(allowRoots []string, dir string) bool {
	for _, allow := range allowRoots {
		if allow != dir && pathWithinRoot(dir, allow) {
			return true
		}
	}
	return false
}

// workspaceRelGlob returns target as a clean, slash-separated path relative to
// workspaceRoot, or ok=false when target is the root itself or lies outside it
// (a workspace-rooted search never reaches such a path, so no glob is needed).
func workspaceRelGlob(workspaceRoot, target string) (string, bool) {
	rel, err := filepath.Rel(workspaceRoot, target)
	if err != nil {
		return "", false
	}
	if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", false
	}
	return filepath.ToSlash(rel), true
}

// ReadExclusionGlobs returns ripgrep-style --glob exclusion args for the policy's
// DenyRead subtrees that fall inside the scope's workspace root, so a
// ripgrep-based search never descends into a read-denied subtree. For each such
// entry it emits `--glob`, `!<rel>` and `--glob`, `!<rel>/**`. Mirrors the
// read-subtree exclusion globs used by comparable executor sandboxes.
//
// The projection is exclusions-only: a positive ripgrep glob would switch the
// search into whitelist mode and restrict it to only matching files, so AllowRead
// re-inclusion is NOT expressed here. The Go-native grep/glob tools honor
// AllowRead precisely via the cached predicate (Engine.ReadExclusions); this
// function is the coarser ripgrep-format export for an external rg-based
// consumer. Empty when DenyRead is unset (the default), so search behavior is
// unchanged.
func ReadExclusionGlobs(policy Policy, scope *Scope) []string {
	// The automatic credential exclusions ride along so an rg-based consumer never
	// walks the bridge token when it happens to live inside the workspace.
	denyRoots := dedupeStrings(append(resolvePolicyPaths(policy.DenyRead), protectedCredentialPaths()...))
	if len(denyRoots) == 0 || scope == nil {
		return nil
	}
	workspaceRoot := scope.WorkspaceRoot()
	if workspaceRoot == "" {
		return nil
	}
	var globs []string
	for _, deny := range denyRoots {
		rel, ok := workspaceRelGlob(workspaceRoot, deny)
		if !ok {
			continue
		}
		globs = append(globs, "--glob", "!"+rel, "--glob", "!"+rel+"/**")
	}
	return globs
}

// policyHasPathLists reports whether any fine-grained path list has an
// ENFORCEABLE entry. It resolves the lists (matching how the rest of this file
// normalizes them) rather than counting raw config, so a typo or non-existent
// entry — which resolution drops — doesn't spuriously fail-close relative
// requests when there is no workspace root.
func policyHasPathLists(policy Policy) bool {
	return len(resolvePolicyPaths(policy.DenyRead)) > 0 ||
		len(resolvePolicyPaths(policy.AllowRead)) > 0 ||
		len(resolvePolicyPaths(policy.DenyWrite)) > 0 ||
		len(resolveWriteRootPaths(policy.AllowWrite)) > 0
}

// dedupeStrings returns xs with duplicates removed, preserving first-seen order.
func dedupeStrings(xs []string) []string {
	if len(xs) <= 1 {
		return xs
	}
	seen := make(map[string]struct{}, len(xs))
	out := make([]string, 0, len(xs))
	for _, x := range xs {
		if _, dup := seen[x]; dup {
			continue
		}
		seen[x] = struct{}{}
		out = append(out, x)
	}
	return out
}
