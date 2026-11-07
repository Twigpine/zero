package sandbox

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

// Scope is the shared set of directories the sandbox allows writes in: the
// workspace root plus zero or more user-granted extra roots. One instance is
// created per run and shared by the policy engine, the OS command runners, and
// the file tools, so a mid-session Add is immediately visible to every layer.
type Scope struct {
	mu            sync.RWMutex
	workspaceRoot string
	readRoots     []string
	extraRoots    []string
	// tempReads / tempWrites count how many LIVE temporary grants depend on a
	// root, so one holder's cleanup cannot revoke another's access.
	//
	// Without them a temporary grant was add-then-remove with no notion of who
	// still needed it: the second caller to ask for a root already present got a
	// NO-OP undo, and the first caller's cleanup removed the root out from under
	// it. Two read-only tools in the same parallel batch, both blocked on the
	// same directory, is exactly that shape — and read-only tools are precisely
	// the ones the batch runs concurrently.
	tempReads  map[string]int
	tempWrites map[string]int
}

// NewScope builds a scope for workspaceRoot plus the given extra roots. The
// workspace root is normalized best-effort (it may not exist in tests); every
// extra root must normalize strictly via Add and an invalid one fails the
// whole construction so a bad --add-dir/config entry surfaces at startup.
func NewScope(workspaceRoot string, extras []string) (*Scope, error) {
	scope := &Scope{workspaceRoot: normalizeWorkspaceRootBestEffort(workspaceRoot)}
	for _, extra := range extras {
		if _, err := scope.Add(extra); err != nil {
			return nil, fmt.Errorf("write root %q: %w", extra, err)
		}
	}
	if scope.workspaceRoot != "" {
		for _, root := range defaultTempWriteRootCandidates() {
			_, _ = scope.Add(root)
		}
	}
	return scope, nil
}

// WorkspaceRoot returns the resolved workspace root. It is safe to call
// without acquiring the lock because workspaceRoot is immutable after
// construction.
func (s *Scope) WorkspaceRoot() string {
	return s.workspaceRoot
}

// ExtraRoots returns only the write roots granted beyond the workspace. Child
// agents run in an isolated worktree and must inherit explicitly granted roots
// without reopening the parent's workspace.
func (s *Scope) ExtraRoots() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]string(nil), s.extraRoots...)
}

// Roots returns the workspace root first, then the extra roots, as a copy.
func (s *Scope) Roots() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	roots := make([]string, 0, 1+len(s.extraRoots))
	roots = append(roots, s.workspaceRoot)
	roots = append(roots, s.extraRoots...)
	return roots
}

// ReadRoots returns the workspace root, write roots, and read-only roots as a
// copy. Write roots are included because anything writable must also be readable
// by the tool layer and native sandbox profile.
func (s *Scope) ReadRoots() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	roots := make([]string, 0, 1+len(s.extraRoots)+len(s.readRoots))
	roots = append(roots, s.workspaceRoot)
	roots = append(roots, s.extraRoots...)
	roots = append(roots, s.readRoots...)
	return dedupeScopeRoots(roots)
}

// ExtraReadRoots returns every path readable beyond the workspace. Write roots
// are included because write authority also confers read authority. Like
// ExtraRoots, this deliberately omits the parent workspace when propagating a
// request_permissions grant to an isolated child.
func (s *Scope) ExtraReadRoots() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	roots := make([]string, 0, len(s.extraRoots)+len(s.readRoots))
	roots = append(roots, s.extraRoots...)
	roots = append(roots, s.readRoots...)
	return dedupeScopeRoots(roots)
}

// Add grants write access under path. The path must be an existing directory;
// it is home-expanded, made absolute, and symlink-resolved before being
// trusted, and the filesystem root is rejected outright. Adding a path already
// covered by an existing root is an idempotent success.
func (s *Scope) Add(path string) (string, error) {
	root, err := normalizeScopeRoot(path)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// COVERED BY WHAT. extraRoots holds temporary write roots alongside
	// permanent ones, so "something already covers this" was not the question
	// worth asking: a permanent grant made over a path a temporary holder
	// happened to cover recorded nothing, and vanished the moment that holder
	// released. A session-scoped grant outliving the request that prompted it is
	// the entire difference between Add and AddTemporaryWrite.
	//
	// Permanent coverage is looked for FIRST and across every root, because a
	// path can be covered twice and the temporary cover must not decide the
	// answer when a permanent one is also present.
	if s.permanentWriteRootCoversLocked(root) {
		return root, nil
	}
	for _, existing := range s.extraRoots {
		// The same root, held temporarily: promote it in place. Its holders'
		// releases become no-ops, which is what permanence means here.
		if existing == root && s.temporaryWriteLocked(existing) {
			delete(s.tempWrites, root)
			return root, nil
		}
	}
	// Either uncovered, or covered only by a BROADER temporary root that is
	// going to be released. Recording the narrower root in its own right is what
	// survives that release.
	s.extraRoots = append(s.extraRoots, root)
	return root, nil
}

// temporaryWriteLocked reports whether root is held by a temporary write grant
// rather than a session-scoped one. Callers must hold the lock.
func (s *Scope) temporaryWriteLocked(root string) bool {
	_, temporary := s.tempWrites[root]
	return temporary
}

// permanentWriteRootCoversLocked reports whether root sits under write authority
// that outlives every temporary holder: the workspace root, or a session-scoped
// grant. Coverage by a TEMPORARY write root deliberately does not count —
// it is going to be released, so nothing recorded on the strength of it
// survives. Callers must hold the lock.
//
// One helper rather than a copy per caller because Add, AddRead and
// AddTemporaryRead all turn on the same question, and three copies of "covered,
// but by what" is how one of them ends up answering it differently.
// permanentReadRootCoversLocked reports whether root is already readable by a
// grant that outlives every temporary holder.
//
// SLICE ORDER MUST NOT DECIDE THIS. The old check walked readRoots and stopped at
// the first entry covering the path, so a BROAD TEMPORARY root sitting earlier in
// the slice shadowed a narrower PERMANENT one later — and the caller was handed a
// reference on somebody else's grant when it needed no reference at all. Asking
// the whole question first, over both lists, makes the answer independent of
// insertion order.
//
// Write roots count: a grant that permits writing permits reading the same tree.
func (s *Scope) permanentReadRootCoversLocked(root string) bool {
	if s.permanentWriteRootCoversLocked(root) {
		return true
	}
	for _, existing := range s.readRoots {
		if pathWithinRoot(existing, root) && !s.temporaryReadLocked(existing) {
			return true
		}
	}
	return false
}

func (s *Scope) permanentWriteRootCoversLocked(root string) bool {
	for _, existing := range append([]string{s.workspaceRoot}, s.extraRoots...) {
		if pathWithinRoot(existing, root) && !s.temporaryWriteLocked(existing) {
			return true
		}
	}
	return false
}

// temporaryReadLocked is temporaryWriteLocked for read roots.
func (s *Scope) temporaryReadLocked(root string) bool {
	_, temporary := s.tempReads[root]
	return temporary
}

// AddRead grants read-only access under path. If the path is already covered by
// a writable root, no separate read root is stored.
func (s *Scope) AddRead(path string) (string, error) {
	root, err := normalizeScopeRoot(path)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Same rule as Add: coverage that is going to be released is not coverage a
	// permanent grant can rely on, whichever side of the read/write boundary it
	// sits on. A permanent read covered only by a temporary WRITE root died with
	// that root just as surely.
	if s.permanentWriteRootCoversLocked(root) {
		return root, nil
	}
	if s.permanentReadRootCoversLocked(root) {
		return root, nil
	}
	for _, existing := range s.readRoots {
		if existing == root && s.temporaryReadLocked(existing) {
			delete(s.tempReads, root)
			return root, nil
		}
	}
	s.readRoots = append(s.readRoots, root)
	return root, nil
}

func (s *Scope) AddTemporaryRead(path string) (string, func(), error) {
	root, err := normalizeScopeRoot(path)
	if err != nil {
		return "", nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Genuinely permanent — the workspace root, or a session-scoped grant. It
	// outlives this reader, so there is nothing to release.
	if s.permanentWriteRootCoversLocked(root) {
		return root, func() {}, nil
	}
	// A TEMPORARY write root covering this path is NOT borrowed, and this is the
	// one place the distinction bites hardest.
	//
	// The dependency on that root is real — the write holder's cleanup must not
	// revoke this reader, which is the defect the refcount exists to close,
	// reached across the read/write boundary. But taking a reference on the
	// WRITE root pays for the lifetime with the authority: the reference keeps
	// the root in extraRoots, extraRoots is what Roots() feeds validate(), and
	// validate() is WRITE authorization. So a reader outliving its writer went on
	// writing anywhere under a write grant that had already ended — not merely
	// under the path it asked to read.
	//
	// One reference cannot be both the lifetime and the capability. The lifetime
	// is kept below, as a temporary READ root for the path this caller actually
	// asked for. readRoots feeds ReadRoots(), which confers no write authority —
	// validate() authorises writes from workspaceRoot plus extraRoots, and this
	// path never enters that list. So the reader survives its writer with read
	// authority and only read authority.
	//
	// That claim is the written justification for the whole design, so it has to
	// stay exhaustive as the accessors change. It has already been wrong once, in
	// both directions: an earlier draft said readRoots fed ReadRoots() and nothing
	// else while ExtraReadRoots() also read it, and that accessor has since been
	// removed as belonging to a different change.
	if s.permanentReadRootCoversLocked(root) {
		return root, func() {}, nil
	}
	if s.tempReads == nil {
		s.tempReads = map[string]int{}
	}
	// THE SAME ROOT, ANOTHER HOLDER: one entry, two references. Only an EXACT
	// match shares — a narrower request must not, which is the whole point below.
	if _, held := s.tempReads[root]; held {
		s.tempReads[root]++
		return root, oncePerHolder(func() { s.releaseTemporaryRead(root) }), nil
	}
	// A NARROWER REQUEST RECORDS ITS OWN ROOT, even under a broader temporary
	// read that currently covers it.
	//
	// Borrowing the covering root's reference kept this reader alive by keeping
	// the BROAD ROOT alive, and readRoots feeds validateRead and the native
	// sandbox profile. So once the broad reader released, a caller that had asked
	// to read one subdirectory was left able to read the whole tree its neighbour
	// had asked for, siblings included. Reported by @jatmn, and it is the read
	// twin of the write escalation fixed above: the refcount key decides not only
	// WHEN an entry disappears but WHICH path stays authorised while it lives.
	s.readRoots = append(s.readRoots, root)
	if s.tempReads == nil {
		s.tempReads = map[string]int{}
	}
	s.tempReads[root] = 1
	return root, oncePerHolder(func() { s.releaseTemporaryRead(root) }), nil
}

func (s *Scope) AddTemporaryWrite(path string) (string, func(), error) {
	root, err := normalizeScopeRoot(path)
	if err != nil {
		return "", nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// A PERMANENT grant is the only cover that needs no bookkeeping: it outlives
	// every temporary holder, so there is nothing to keep alive and nothing to
	// release.
	if s.permanentWriteRootCoversLocked(root) {
		return root, func() {}, nil
	}
	if s.tempWrites == nil {
		s.tempWrites = map[string]int{}
	}
	// THE SAME ROOT, ANOTHER HOLDER: one entry, two references. Only an exact
	// match shares an entry — a NARROWER request must not, which is the whole
	// point below.
	if _, held := s.tempWrites[root]; held {
		s.tempWrites[root]++
		return root, oncePerHolder(func() { s.releaseTemporaryWrite(root) }), nil
	}
	// A NARROWER REQUEST RECORDS ITS OWN ROOT, even when a broader temporary
	// grant currently covers it.
	//
	// Borrowing a reference on the covering root kept this holder alive, which
	// was the point, but it kept the COVERING ROOT alive to do it — and
	// extraRoots is what validate() authorises writes from. So once the broad
	// holder released, a caller that had asked to write one subdirectory was
	// left holding write authority over the whole tree its neighbour had asked
	// for, including siblings it never named. Reported by CodeRabbit, and it is
	// the write-side twin of the read-side escalation @jatmn found: one
	// reference cannot be both a lifetime and a capability, whichever side of
	// the read/write boundary it sits on.
	//
	// Recording the requested root separately gives each holder exactly the
	// authority it asked for and a lifetime of its own. The nesting costs an
	// extra entry in extraRoots while both are live, which is redundant but not
	// wrong — the broader root already permits everything the narrower one does.
	s.extraRoots = append(s.extraRoots, root)
	s.tempWrites[root] = 1
	return root, oncePerHolder(func() { s.releaseTemporaryWrite(root) }), nil
}

// releaseTemporaryRead drops ONE holder's reference and removes the root only
// when the last one is gone.
//
// The count alone cannot make this safe, and an earlier comment here claimed it
// could — that each undo is called exactly once, and flooring at zero handled
// the rest. Flooring stops the count going negative; it does not stop one
// holder's second call consuming a DIFFERENT holder's reference. With two
// readers, calling the first's undo twice took the count 2 -> 1 -> 0 and removed
// a root the second was still using. Idempotency belongs to the closure, which
// is the thing that knows whose reference it is, so every undo handed out is
// wrapped in oncePerHolder.
func (s *Scope) releaseTemporaryRead(root string) {
	s.mu.Lock()
	remaining, tracked := s.tempReads[root]
	if !tracked {
		s.mu.Unlock()
		return
	}
	remaining--
	if remaining > 0 {
		s.tempReads[root] = remaining
		s.mu.Unlock()
		return
	}
	// Both mutations under ONE hold. Dropping the lock between them opened a
	// window where the root was still in readRoots but no longer in tempReads:
	// a concurrent AddTemporaryRead landing there reads it as a PERMANENT root,
	// hands its caller a no-op undo, and then this call strips the root — so
	// that caller believes it holds access it has already silently lost.
	delete(s.tempReads, root)
	s.readRoots = removeScopeRoot(s.readRoots, root)
	s.mu.Unlock()
}

// releaseTemporaryWrite is releaseTemporaryRead for write roots. Two functions
// rather than one generic helper because they guard different slices and the
// generic version would take the slice by name — which is how the wrong one
// gets passed.
func (s *Scope) releaseTemporaryWrite(root string) {
	s.mu.Lock()
	remaining, tracked := s.tempWrites[root]
	if !tracked {
		s.mu.Unlock()
		return
	}
	remaining--
	if remaining > 0 {
		s.tempWrites[root] = remaining
		s.mu.Unlock()
		return
	}
	// Same single-hold rule as releaseTemporaryRead above.
	delete(s.tempWrites, root)
	s.extraRoots = removeScopeRoot(s.extraRoots, root)
	s.mu.Unlock()
}

// oncePerHolder makes one holder's undo safe to call more than once. The
// reference it drops is that holder's own, so a second call must do nothing
// rather than reach into the shared count and take somebody else's.
func oncePerHolder(release func()) func() {
	var once sync.Once
	return func() { once.Do(release) }
}

func removeScopeRoot(roots []string, root string) []string {
	next := roots[:0]
	for _, existing := range roots {
		if existing != root {
			next = append(next, existing)
		}
	}
	return next
}

func dedupeScopeRoots(roots []string) []string {
	seen := map[string]struct{}{}
	out := roots[:0]
	for _, root := range roots {
		if root == "" {
			continue
		}
		if _, ok := seen[root]; ok {
			continue
		}
		seen[root] = struct{}{}
		out = append(out, root)
	}
	return out
}

// validate reports whether requestedPath is allowed by any scope root.
// Relative paths resolve against the workspace root only; absolute paths are
// accepted if they validate (including per-segment symlink checks) under ANY
// root. A symlink whose final target lies inside any granted root is allowed —
// this is a deliberate semantic widening compared with single-root validation,
// because the true write target is inside an allowed root.
//
// When all roots deny, a BlockSymlinkTraversal result from any root is
// preferred over BlockOutsideWorkspace; the --add-dir hint is appended
// only on outside_workspace results. The returned block always carries
// the caller's original requestedPath.
func (s *Scope) validate(requestedPath string) *pathBlock {
	return withOutsideWorkspaceAccessReason(s.validateAgainstRoots(requestedPath, s.Roots()), requestedPath, SideEffectWrite)
}

func (s *Scope) validateRead(requestedPath string) *pathBlock {
	return withOutsideWorkspaceAccessReason(s.validateAgainstRoots(requestedPath, s.ReadRoots()), requestedPath, SideEffectRead)
}

func withOutsideWorkspaceAccessReason(block *pathBlock, requestedPath string, sideEffect SideEffect) *pathBlock {
	if block == nil || block.Code != BlockOutsideWorkspace || !strings.Contains(block.Reason, " is outside the workspace") {
		return block
	}
	switch sideEffect {
	case SideEffectRead:
		block.Reason = fmt.Sprintf("Reading %s requires access outside the workspace.", requestedPath)
	case SideEffectWrite, SideEffectOutOfWorkspace:
		block.Reason = fmt.Sprintf("Writing to %s requires access outside the workspace. Use /add-dir or --add-dir to allow writes there.", requestedPath)
	}
	return block
}

func (s *Scope) validateAgainstRoots(requestedPath string, roots []string) *pathBlock {
	if len(roots) == 0 {
		return &pathBlock{
			Code:   BlockOutsideWorkspace,
			Path:   requestedPath,
			Reason: fmt.Sprintf("%s is outside the workspace", requestedPath),
		}
	}
	if !filepath.IsAbs(requestedPath) {
		return validateWorkspacePath(roots[0], requestedPath)
	}
	// For each root, normalize the leading path prefix so that platform-level
	// symlinks (e.g. macOS /var -> /private/var) are resolved before comparing
	// against the symlink-resolved scope roots, while leaving workspace-internal
	// symlinks intact so validateWorkspacePath can detect traversal blocks.
	var outsideBlock *pathBlock
	var traversalBlock *pathBlock
	for _, root := range roots {
		normalized := NormalizePrefixForRoot(requestedPath, root)
		block := validateWorkspacePath(root, normalized)
		if block == nil {
			return nil
		}
		switch block.Code {
		case BlockSymlinkTraversal:
			if traversalBlock == nil {
				traversalBlock = block
			}
		default:
			if outsideBlock == nil {
				outsideBlock = block
			}
		}
	}
	// Prefer symlink-traversal: the path was lexically inside a granted root
	// but crossed an in-root symlink — the --add-dir hint would be misleading.
	if traversalBlock != nil {
		return &pathBlock{
			Code:   BlockSymlinkTraversal,
			Path:   requestedPath,
			Reason: traversalBlock.Reason,
		}
	}
	// Plain outside-workspace denial — rebuild with the original path and hint.
	return &pathBlock{
		Code:   BlockOutsideWorkspace,
		Path:   requestedPath,
		Reason: fmt.Sprintf("%s is outside the workspace (use /add-dir or --add-dir to allow writes there)", requestedPath),
	}
}

// NormalizePrefixForRoot resolves platform-level symlinks (e.g. macOS
// /var -> /private/var) in the portion of absPath that lies outside
// resolvedRoot, while leaving workspace-internal path components intact so
// that validateWorkspacePath can detect symlink traversal blocks there.
// It is exported because the tools layer shares it to normalize absolute
// paths per scope root before running its own single-root checks.
//
// Algorithm: walk absPath component-by-component, resolving each via
// EvalSymlinks. Once the running resolved prefix equals resolvedRoot we are
// inside the root — stop resolving and append the remaining components
// verbatim. If a component inside the root is a symlink, leave it for
// validateWorkspacePath to handle. Non-existent tail components are always
// appended verbatim.
//
// The walk is volume-aware so it works on Windows as well as POSIX. On
// Windows the same alias problem appears in a different guise — a workspace
// created under an 8.3 short path (C:\Users\RUNNER~1\...) is resolved by
// EvalSymlinks to its long form (C:\Users\runneradmin\...), so a raw
// short-form request would escape the long-form root unless its prefix is
// resolved here first. The component walk must therefore start from the
// volume root (C:\ or \\host\share\), not "/", or it would mangle a drive
// path into a drive-relative form (C:\Users -> C:Users) that the single-root
// checks treat as RELATIVE — failing the policy gate OPEN. On POSIX
// VolumeName is empty and the volume root reduces to "/", so behavior there
// is byte-identical to a plain "/"-rooted walk.
func NormalizePrefixForRoot(absPath, resolvedRoot string) string {
	volume := filepath.VolumeName(absPath)
	volumeRoot := volume + string(filepath.Separator)
	tail := strings.TrimPrefix(filepath.Clean(absPath), volume)
	parts := strings.Split(strings.TrimPrefix(tail, string(filepath.Separator)), string(filepath.Separator))
	current := volumeRoot
	for i, part := range parts {
		if part == "" {
			continue
		}
		// If we've reached the resolved root boundary, stop resolving and
		// append the remaining tail verbatim so validateWorkspacePath sees the
		// original symlink names.
		if current == resolvedRoot {
			return filepath.Join(append([]string{current}, parts[i:]...)...)
		}
		next := filepath.Join(current, part)
		info, lerr := os.Lstat(next)
		if lerr != nil {
			// Non-existent component — append rest verbatim.
			return filepath.Join(append([]string{current}, parts[i:]...)...)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			// Symlink. Only resolve it if we're still outside the root.
			if pathWithinRoot(resolvedRoot, current) {
				// Inside root — leave this symlink for validateWorkspacePath.
				return filepath.Join(append([]string{current}, parts[i:]...)...)
			}
			// Outside root (or a jump into the root) — resolve this platform-level symlink.
			resolved, err := filepath.EvalSymlinks(next)
			if err != nil {
				return filepath.Join(append([]string{current}, parts[i:]...)...)
			}
			current = resolved
			continue
		}
		// Regular component outside root — resolve it.
		resolved, err := filepath.EvalSymlinks(next)
		if err != nil {
			current = next
		} else {
			current = resolved
		}
	}
	return current
}

func normalizeWorkspaceRootBestEffort(workspaceRoot string) string {
	trimmed := strings.TrimSpace(workspaceRoot)
	if trimmed == "" {
		return ""
	}
	absolute, err := filepath.Abs(trimmed)
	if err != nil {
		return filepath.Clean(trimmed)
	}
	if resolved, err := filepath.EvalSymlinks(absolute); err == nil {
		return resolved
	}
	return filepath.Clean(absolute)
}

func normalizeScopeRoot(path string) (string, error) {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return "", errors.New("write root path is empty")
	}
	if trimmed == "~" || strings.HasPrefix(trimmed, "~/") || strings.HasPrefix(trimmed, "~"+string(filepath.Separator)) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("expand ~: %w", err)
		}
		trimmed = filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(trimmed[1:], "/"), string(filepath.Separator)))
	}
	absolute, err := filepath.Abs(trimmed)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", fmt.Errorf("write root must exist: %w", err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("write root %s is not a directory", resolved)
	}
	if filepath.Dir(resolved) == resolved {
		return "", fmt.Errorf("refusing filesystem root %s as a write root", resolved)
	}
	return resolved, nil
}

func defaultTempWriteRootCandidates() []string {
	return defaultTempWriteRootCandidatesForGOOS(runtime.GOOS, os.Getenv)
}

func defaultTempWriteRootCandidatesForGOOS(goos string, getenv func(string) string) []string {
	var roots []string
	if goos == "windows" {
		for _, key := range []string{"TEMP", "TMP"} {
			if root := strings.TrimSpace(getenv(key)); root != "" {
				roots = append(roots, root)
			}
		}
		return roots
	}
	if goos != "windows" {
		roots = append(roots, "/tmp")
	}
	if tmpdir := strings.TrimSpace(getenv("TMPDIR")); tmpdir != "" {
		roots = append(roots, tmpdir)
	}
	return roots
}

func defaultTempWriteRoots() []string {
	return normalizeProfileDirs(defaultTempWriteRootCandidates())
}

func pathWithinRoot(root string, candidate string) bool {
	if root == "" {
		return false
	}
	relative, err := filepath.Rel(root, candidate)
	if err != nil {
		return false
	}
	return relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative))
}
