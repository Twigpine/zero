package tools

import "github.com/Twigpine/zero/internal/sandbox"

// readExcluder skips read-denied paths (the sandbox DenyRead policy) during a
// search walk. The zero value (both funcs nil) excludes nothing, so a
// non-sandboxed search behaves exactly as before — the exclusions are opt-in and
// only ever REMOVE results, never add them.
type readExcluder struct {
	file func(string) bool
	dir  func(string) bool
}

func (e readExcluder) fileExcluded(path string) bool { return e.file != nil && e.file(path) }
func (e readExcluder) dirExcluded(path string) bool  { return e.dir != nil && e.dir(path) }

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
	return readExcluder{file: rx.PathExcluded, dir: rx.DirExcluded}
}
