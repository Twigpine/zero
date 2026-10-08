package tools

// ModelVisible reports whether a registered tool is advertised to an agent.
// Every registered tool is visible. edit_file (exact search/replace with a
// uniqueness guard, CRLF and fuzzy fallbacks) was hidden for a while in favour
// of apply_patch; head-to-head benchmarking showed that when a model misses the
// patch grammar it degrades to whole-file rewrites, so both edit tools are
// advertised and the prompt steers between them.
func ModelVisible(tool Tool) bool {
	return tool != nil
}
