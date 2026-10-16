package tools

import (
	"os"

	"github.com/Gitlawb/zero/internal/sandbox"
)

// readExcluder skips read-denied paths (the sandbox DenyRead policy) during a
// search walk. The zero value (both funcs nil) excludes nothing, so a
// non-sandboxed search behaves exactly as before — the exclusions are opt-in and
// only ever REMOVE results, never add them.
type readExcluder struct {
	file func(string) bool
	dir  func(string) bool
	// handle is the same decision taken from an OPENED object's own metadata.
	// A walk-time pathname check cannot be the enforcement boundary for a read
	// that opens the name again later: the candidate can be replaced by a
	// symlink or hard link to a protected credential in between, and the open
	// then returns the credential. Callers that open must re-ask here with the
	// FileInfo from their handle.
	handle func(string, *os.File, os.FileInfo) bool
}

func (e readExcluder) fileExcluded(path string) bool { return e.file != nil && e.file(path) }
func (e readExcluder) dirExcluded(path string) bool  { return e.dir != nil && e.dir(path) }

// openedFileExcluded is the authoritative check, run after the object is open.
func (e readExcluder) openedFileExcluded(path string, file *os.File, info os.FileInfo) bool {
	if e.handle != nil {
		return e.handle(path, file, info)
	}
	return e.fileExcluded(path)
}

// sandboxReadExcluder builds a readExcluder from a sandbox engine's DenyRead
// policy. The engine's read-exclusion matcher resolves the policy paths ONCE here
// (not per visited path), and the closures reuse it for the whole walk. A nil
// engine or an inactive (no DenyRead) policy yields the no-op excluder, so the
// search tools keep their pre-sandbox behavior.
func sandboxReadExcluder(engine *sandbox.Engine) readExcluder {
	if engine == nil {
		return readExcluder{}
	}
	rx := engine.ReadExclusions()
	if !rx.Active() {
		return readExcluder{}
	}
	return readExcluder{file: rx.PathExcluded, dir: rx.DirExcluded, handle: rx.FileHandleExcluded}
}

// sandboxReadExcluderWithin is sandboxReadExcluder for callers that can name
// their workspace root, and it differs in one way that matters: when there is
// no engine it still excludes the automatic protected credentials.
//
// Registry.Run funnels into RunWithOptions with empty options, so "no engine"
// is a real production path (MCP, legacy callers), not just a test shape. The
// protected-credential set is derived from this process's environment rather
// than from a policy, so there is no engine to consult for it and no reason for
// that path to disclose the bridge bearer token. Policy-driven DenyRead still
// requires an engine — without one there is no policy to apply.
func sandboxReadExcluderWithin(engine *sandbox.Engine, workspaceRoot string) readExcluder {
	if engine != nil {
		return sandboxReadExcluder(engine)
	}
	rx := sandbox.ProtectedCredentialExclusions(workspaceRoot)
	if !rx.Active() {
		return readExcluder{}
	}
	return readExcluder{file: rx.PathExcluded, dir: rx.DirExcluded, handle: rx.FileHandleExcluded}
}
