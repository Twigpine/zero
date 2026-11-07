package tui

import (
	"io/fs"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Twigpine/zero/internal/agent"
	"github.com/Twigpine/zero/internal/workspaceindex"
)

// commandSuggestion is one row in the slash-command autocomplete overlay: the
// canonical command name and its short description.
type commandSuggestion struct {
	Name string
	Desc string
}

const (
	// maxCommandSuggestions caps command matches kept in memory; rendering still
	// shows a smaller window so a bare "/" can report the remaining commands.
	maxCommandSuggestions = 32
	maxFileSuggestions    = 16
)

const fileIndexTTL = 30 * time.Second

type fileSuggestionIndex struct {
	files []string
	dirs  []string
}

type cachedFileSuggestionIndex struct {
	index     fileSuggestionIndex
	expiresAt time.Time
}

type fileSuggestionCandidate struct {
	path  string
	isDir bool
}

var fileSuggestionIndexCache = struct {
	sync.Mutex
	entries map[string]cachedFileSuggestionIndex
}{entries: map[string]cachedFileSuggestionIndex{}}

// suggestionsActive reports whether the autocomplete overlay should drive key
// handling. A slash-command palette stays active even with zero matches so the
// query remains in the palette instead of leaking back into the composer.
func (m model) suggestionsActive() bool {
	if m.pendingPermission != nil || m.pendingAskUser != nil || m.pendingSpecReview != nil || m.providerWizard != nil || m.mcpManager != nil {
		return false
	}
	if len(m.suggestions) > 0 {
		return true
	}
	return m.commandPaletteOpen || m.filePaletteOpen
}

func (m *model) clearSuggestions() {
	m.suggestions = nil
	m.suggestionIdx = 0
	m.suggestionsAreFiles = false
	m.suggestionsAreSpecialists = false
	m.commandPaletteOpen = false
	m.filePaletteOpen = false
}

// recomputeSuggestions rebuilds the autocomplete match list from the current
// input. It only matches a leading slash token (no spaces yet) so suggestions
// disappear once the user starts typing arguments. Modals suppress matching
// entirely. The selected index is preserved when still in range, otherwise reset.
func (m *model) recomputeSuggestions() {
	if m.pendingPermission != nil || m.pendingAskUser != nil || m.pendingSpecReview != nil || m.providerWizard != nil || m.mcpManager != nil {
		m.clearSuggestions()
		return
	}

	value := m.input.Value()
	m.suggestionsAreSpecialists = false

	// File reference: a trailing "@token" (even mid-prompt) drives a workspace
	// file picker. Checked before the slash path so "@" is handled distinctly.
	if query := extractPathQuery(value, m.input.Position()); query != nil {
		// A LEADING "@token" (the first word of the message) instead offers the
		// delegatable specialists, so "@explorer" routes to a sub-agent rather than
		// a file. Mid-message "@token" stays a file reference.
		if specs := m.leadingSpecialistSuggestions(value, query); len(specs) > 0 {
			m.commandPaletteOpen = false
			m.filePaletteOpen = true
			m.suggestionsAreFiles = true
			m.suggestionsAreSpecialists = true
			m.suggestions = specs
			if m.suggestionIdx >= len(specs) || m.suggestionIdx < 0 {
				m.suggestionIdx = 0
			}
			return
		}
		m.commandPaletteOpen = false
		m.filePaletteOpen = true
		m.suggestionsAreFiles = true
		m.suggestions = fileSuggestions(m.cwd, query.Query)
		if m.suggestionIdx >= len(m.suggestions) || m.suggestionIdx < 0 {
			m.suggestionIdx = 0
		}
		return
	}
	m.suggestionsAreFiles = false

	if !commandSuggestionsOpen(value) {
		m.suggestions = nil
		m.suggestionIdx = 0
		m.commandPaletteOpen = false
		m.filePaletteOpen = false
		return
	}
	trimmed := strings.TrimLeft(value, " ")
	token := strings.TrimSpace(trimmed)

	m.commandPaletteOpen = true
	matches := m.matchCommandSuggestions(token)
	m.suggestions = matches
	if m.suggestionIdx >= len(matches) {
		m.suggestionIdx = 0
	}
	if m.suggestionIdx < 0 {
		m.suggestionIdx = 0
	}
}

func commandSuggestionsOpen(value string) bool {
	trimmed := strings.TrimLeft(value, " ")
	return strings.HasPrefix(trimmed, "/") && strings.TrimSpace(trimmed) != "" && !strings.ContainsAny(trimmed, " \t")
}

func fileSuggestionOnlyInput(value string) bool {
	trimmed := strings.TrimSpace(value)
	return strings.HasPrefix(trimmed, "@") && !strings.ContainsAny(trimmed, " \t\n")
}

type pathQuery struct {
	Query      string
	StartIndex int
	EndIndex   int
}

func extractPathQuery(text string, cursorPos int) *pathQuery {
	runes := []rune(text)
	cursorPos = clampInt(cursorPos, 0, len(runes))
	at := -1
	for i := cursorPos - 1; i >= 0; i-- {
		if runes[i] == '@' {
			at = i
			break
		}
		if isPathQueryBoundary(runes[i]) {
			break
		}
	}
	if at < 0 {
		return nil
	}
	end := at + 1
	for end < len(runes) && !isPathQueryBoundary(runes[end]) {
		end++
	}
	if cursorPos > end {
		return nil
	}
	return &pathQuery{
		Query:      string(runes[at+1 : end]),
		StartIndex: at,
		EndIndex:   end,
	}
}

func isPathQueryBoundary(r rune) bool {
	return r == ' ' || r == '\t' || r == '\n' || r == '\r'
}

func (m model) matchCommandSuggestions(token string) []commandSuggestion {
	out := matchCommandSuggestionsWithFilter(token, func(command commandDefinition) bool {
		return command.kind != commandSandboxSetup || m.sandboxSetupCommand != nil
	})
	out = append(out, m.matchUserCommandSuggestions(token)...)
	out = append(out, m.matchSkillSuggestions(token)...)
	// Each source caps itself, but the merged list must honor the shared cap too
	// (three sources could otherwise stack up to ~3x the palette bound).
	if len(out) > maxCommandSuggestions {
		out = out[:maxCommandSuggestions]
	}
	return out
}

// matchSkillSuggestions returns installed skills whose slash name has the typed
// prefix, so skills are discoverable and invocable from the palette like user
// commands. Precedence (builtin > user command > skill) is enforced here by
// skipping any skill whose name is claimed by a builtin (or alias) or a user
// command — unlike the builtin/user pair, a shadowed skill row would be dead at
// dispatch time, so it must not be advertised at all.
func (m model) matchSkillSuggestions(token string) []commandSuggestion {
	prefix := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(token, "/")))
	if prefix == "" || m.loadSkills == nil {
		return nil
	}
	taken := m.takenSlashNames()
	var out []commandSuggestion
	// Read through the loader (not the startup snapshot in agentOptions) so the
	// palette matches what dispatch will actually resolve — a skill installed or
	// removed mid-session appears/disappears without a restart.
	for _, skill := range m.installedSkills() {
		name := skillSlashName(skill.Name)
		if name == "" || taken[name] || !strings.HasPrefix(name, prefix) {
			continue
		}
		// Two skills whose names collide after lowercasing (e.g. "Deploy" and
		// "deploy") map to one slash name; only the first — the one dispatch will
		// run — may be advertised, or the row's description and the executed
		// instructions silently diverge.
		taken[name] = true
		desc := strings.TrimSpace(skill.Description)
		if desc == "" {
			desc = "Skill: /" + name
		} else {
			desc += " (skill)"
		}
		out = append(out, commandSuggestion{Name: "/" + name, Desc: desc})
		if len(out) >= maxCommandSuggestions {
			break
		}
	}
	return out
}

// matchUserCommandSuggestions returns file-sourced /commands whose name has the
// typed prefix, so user-defined commands appear in the autocomplete menu
// alongside builtins. Builtins win on a name collision (they are listed first
// and a builtin name shadows a same-named user command at dispatch time).
func (m model) matchUserCommandSuggestions(token string) []commandSuggestion {
	prefix := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(token, "/")))
	if prefix == "" {
		return nil
	}
	var out []commandSuggestion
	for _, cmd := range m.userCommands {
		if strings.HasPrefix(cmd.Name, prefix) {
			out = append(out, commandSuggestion{Name: "/" + cmd.Name, Desc: cmd.Description})
			if len(out) >= maxCommandSuggestions {
				break
			}
		}
	}
	return out
}

func matchCommandSuggestionsWithFilter(token string, include func(commandDefinition) bool) []commandSuggestion {
	prefix := strings.ToLower(strings.TrimSpace(token))
	if prefix == "" {
		return nil
	}
	out := make([]commandSuggestion, 0, minInt(maxCommandSuggestions, len(commandDefinitions)))
	for _, command := range commandDefinitions {
		if include != nil && !include(command) {
			continue
		}
		if !commandHasPrefix(command, prefix) {
			continue
		}
		out = append(out, commandSuggestion{Name: command.name, Desc: command.description})
		if len(out) >= maxCommandSuggestions {
			break
		}
	}
	return out
}

func commandHasPrefix(command commandDefinition, prefix string) bool {
	if strings.HasPrefix(command.name, prefix) {
		return true
	}
	for _, alias := range command.aliases {
		if strings.HasPrefix(alias, prefix) {
			return true
		}
	}
	return false
}

// moveSuggestion advances (delta +1) or rewinds (delta -1) the selected
// suggestion, wrapping at both ends.
func (m *model) moveSuggestion(delta int) {
	n := len(m.suggestions)
	if n == 0 {
		return
	}
	m.suggestionIdx = ((m.suggestionIdx+delta)%n + n) % n
}

// completeSuggestion replaces the input with the selected suggestion and
// dismisses the overlay. Required-argument commands stay tight ("/spec") so
// the rendered argument hint can sit next to the cursor without dead padding.
func (m model) completeSuggestion() model {
	if !m.suggestionsActive() || len(m.suggestions) == 0 {
		return m
	}
	idx := m.suggestionIdx
	if idx < 0 || idx >= len(m.suggestions) {
		idx = 0
	}
	chosen := m.suggestions[idx].Name
	switch {
	case m.suggestionsAreSpecialists:
		m = m.completeSpecialistSuggestion(chosen)
	case m.suggestionsAreFiles:
		isDir := fileSuggestionIsDirectory(m.suggestions[idx])
		m = m.completeFileSuggestion(chosen, !isDir)
	default:
		if commandSelectionRequiresInput(chosen) {
			m.input.SetValue(chosen)
		} else {
			m.input.SetValue(chosen + " ")
		}
		m.input.CursorEnd()
	}
	m.suggestions = nil
	m.suggestionIdx = 0
	m.suggestionsAreFiles = false
	m.suggestionsAreSpecialists = false
	m.commandPaletteOpen = false
	m.filePaletteOpen = false
	return m
}

func (m model) completeFileSuggestion(chosen string, trailingSpace bool) model {
	state := m.fileSuggestionComposerState()
	query := extractPathQuery(state.text, state.cursor)
	if query == nil {
		nextValue, nextCursor := completePathQueryWithTrailingSpace(m.input.Value(), m.input.Position(), chosen, trailingSpace)
		m.input.SetValue(nextValue)
		m.input.SetCursor(nextCursor)
		m.resetComposerFromInput()
		return m
	}
	replacement := chosen
	if trailingSpace {
		replacement += " "
	}
	return m.replaceComposerRangeWithPastePreviews(state, query.StartIndex, query.EndIndex, replacement)
}

func (m model) fileSuggestionComposerState() composerState {
	state := m.currentComposerState()
	inputState := normalizeComposerState(composerState{text: m.input.Value(), cursor: m.input.Position()})
	if m.composerActive && inputState.text == state.text && inputState.cursor != state.cursor {
		return inputState
	}
	return state
}

func (m model) selectedSuggestionIsDirectory() bool {
	if !m.suggestionsAreFiles || len(m.suggestions) == 0 {
		return false
	}
	idx := clampInt(m.suggestionIdx, 0, len(m.suggestions)-1)
	return fileSuggestionIsDirectory(m.suggestions[idx])
}

func (m model) selectedCommandSuggestionRequiresInput() bool {
	if m.suggestionsAreFiles || len(m.suggestions) == 0 {
		return false
	}
	idx := clampInt(m.suggestionIdx, 0, len(m.suggestions)-1)
	return commandSelectionRequiresInput(m.suggestions[idx].Name)
}

func fileSuggestionIsDirectory(suggestion commandSuggestion) bool {
	return suggestion.Desc == "directory" || strings.HasSuffix(suggestion.Name, "/")
}

// trailingAtToken returns the file-reference fragment at the end of value: the
// part AFTER an "@" in the last whitespace-delimited word (empty for a bare "@").
// ok is false when that word does not start with "@".
func trailingAtToken(value string) (string, bool) {
	last := value
	if i := strings.LastIndexAny(value, " \t\n"); i >= 0 {
		last = value[i+1:]
	}
	if !strings.HasPrefix(last, "@") {
		return "", false
	}
	return last[1:], true
}

// replaceTrailingAtToken swaps the trailing "@token" word in value for path.
func replaceTrailingAtToken(value, path string) string {
	if i := strings.LastIndexAny(value, " \t\n"); i >= 0 {
		return value[:i+1] + path
	}
	return path
}

// leadingSpecialistSuggestions returns the delegatable specialists matching a
// LEADING "@token" (the first word of the message), so "@explorer" routes to a
// sub-agent. A non-leading "@token" stays a file reference, so this returns nil
// there. Rows reuse commandSuggestion: Name is the "@name" insert text, Desc the
// specialist's description.
func (m model) leadingSpecialistSuggestions(value string, query *pathQuery) []commandSuggestion {
	if query == nil || len(m.agentOptions.Specialists) == 0 {
		return nil
	}
	runes := []rune(value)
	if query.StartIndex < 0 || query.StartIndex > len(runes) {
		return nil
	}
	if strings.TrimSpace(string(runes[:query.StartIndex])) != "" {
		return nil // "@" is not the first word: treat as a file reference
	}
	prefix := strings.ToLower(strings.TrimSpace(query.Query))
	out := make([]commandSuggestion, 0, len(m.agentOptions.Specialists))
	for _, spec := range m.agentOptions.Specialists {
		name := strings.TrimSpace(spec.Name)
		if name == "" {
			continue
		}
		if prefix != "" && !strings.HasPrefix(strings.ToLower(name), prefix) {
			continue
		}
		out = append(out, commandSuggestion{Name: "@" + name, Desc: strings.TrimSpace(spec.WhenToUse)})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// completeSpecialistSuggestion replaces the leading "@token" with the chosen
// "@name " and keeps the composer open so the user can type the task.
func (m model) completeSpecialistSuggestion(chosen string) model {
	state := m.fileSuggestionComposerState()
	query := extractPathQuery(state.text, state.cursor)
	if query == nil {
		m.input.SetValue(chosen + " ")
		m.input.CursorEnd()
		m.resetComposerFromInput()
		return m
	}
	return m.replaceComposerRangeWithPastePreviews(state, query.StartIndex, query.EndIndex, chosen+" ")
}

// expandSpecialistMention rewrites a message that begins with "@<specialist> <task>"
// into an explicit Task-delegation directive for the agent. It returns ok=false
// unless the first word is "@<known-specialist>" with a task following, so normal
// prompts and mid-message "@file" references are untouched. Callers keep the
// user's verbatim "@mention" in the transcript and expand only the agent-facing
// text.
func expandSpecialistMention(prompt string, specialists []agent.SpecialistInfo) (string, bool) {
	trimmed := strings.TrimLeft(prompt, " \t")
	if !strings.HasPrefix(trimmed, "@") {
		return "", false
	}
	rest := trimmed[1:]
	name := rest
	task := ""
	if i := strings.IndexAny(rest, " \t\n"); i >= 0 {
		name = rest[:i]
		task = strings.TrimSpace(rest[i+1:])
	}
	if name = strings.TrimSpace(name); name == "" || task == "" {
		return "", false
	}
	matched := ""
	for _, spec := range specialists {
		if strings.EqualFold(strings.TrimSpace(spec.Name), name) {
			matched = strings.TrimSpace(spec.Name)
			break
		}
	}
	if matched == "" {
		return "", false
	}
	return "Use the Task tool to delegate this to the " + matched +
		" specialist, then report its result back to me:\n\n" + task, true
}

func completePathQueryWithTrailingSpace(value string, cursorPos int, selectedPath string, trailingSpace bool) (string, int) {
	query := extractPathQuery(value, cursorPos)
	suffix := ""
	if trailingSpace {
		suffix = " "
	}
	if query == nil {
		next := replaceTrailingAtToken(value, selectedPath) + suffix
		return next, len([]rune(next))
	}
	replacement := []rune(selectedPath + suffix)
	runes := []rune(value)
	out := make([]rune, 0, len(runes)-query.EndIndex+query.StartIndex+len(replacement))
	out = append(out, runes[:query.StartIndex]...)
	out = append(out, replacement...)
	out = append(out, runes[query.EndIndex:]...)
	return string(out), query.StartIndex + len(replacement)
}

// maxFileWalk bounds how many filesystem entries the "@file" picker visits per
// keystroke so a large workspace tree can't stall the TUI.
const maxFileWalk = 4000

// fileSuggestions lists ranked workspace files and directories for the "@file"
// picker. The walk skips VCS/dependency directories, is TTL-cached, and is
// bounded so it stays responsive. Each suggestion's Name is the "@<relpath>"
// token that completion inserts.
func fileSuggestions(cwd, partial string) []commandSuggestion {
	return fileSuggestionsBounded(cwd, partial, maxFileWalk)
}

// fileSuggestionsBounded is fileSuggestions with an explicit walk budget so the
// per-keystroke bound is unit-testable without materializing maxFileWalk entries.
func fileSuggestionsBounded(cwd, partial string, maxVisited int) []commandSuggestion {
	cwd = strings.TrimSpace(cwd)
	if cwd == "" {
		return nil
	}
	index := cachedFileSuggestionsIndex(cwd, maxVisited)
	return rankedFileSuggestions(index, partial, maxFileSuggestions)
}

func cachedFileSuggestionsIndex(cwd string, maxVisited int) fileSuggestionIndex {
	key := filepath.Clean(cwd) + "\x00" + strconv.Itoa(maxVisited)
	now := time.Now()
	fileSuggestionIndexCache.Lock()
	if cached, ok := fileSuggestionIndexCache.entries[key]; ok && now.Before(cached.expiresAt) {
		fileSuggestionIndexCache.Unlock()
		return cached.index
	}
	fileSuggestionIndexCache.Unlock()

	index := buildFileSuggestionsIndex(cwd, maxVisited)
	fileSuggestionIndexCache.Lock()
	fileSuggestionIndexCache.entries[key] = cachedFileSuggestionIndex{index: index, expiresAt: now.Add(fileIndexTTL)}
	fileSuggestionIndexCache.Unlock()
	return index
}

func buildFileSuggestionsIndex(cwd string, maxVisited int) fileSuggestionIndex {
	index := fileSuggestionIndex{}
	visited := 0
	_ = filepath.WalkDir(cwd, func(currentPath string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if visited >= maxVisited {
			return fs.SkipAll
		}
		// Count every entry (directories included) so the walk is bounded even in
		// directory-heavy trees where few entries are files; otherwise a deep tree
		// could be traversed in full on each keystroke and stall the TUI.
		visited++
		if d.IsDir() {
			if currentPath != cwd && workspaceindex.ShouldSkipDir(d.Name()) {
				return fs.SkipDir
			}
			if currentPath != cwd {
				rel, relErr := filepath.Rel(cwd, currentPath)
				if relErr == nil {
					rel = filepath.ToSlash(rel)
					index.dirs = append(index.dirs, rel)
				}
			}
			return nil
		}
		rel, relErr := filepath.Rel(cwd, currentPath)
		if relErr != nil {
			rel = filepath.Base(currentPath)
		}
		// Emit forward-slash paths on every platform (filepath.Rel uses "\" on
		// Windows) so the inserted "@path" token is portable and matchable.
		rel = filepath.ToSlash(rel)
		if workspaceindex.ShouldSkipFile(rel) {
			return nil
		}
		index.files = append(index.files, rel)
		return nil
	})
	sort.Strings(index.files)
	sort.Strings(index.dirs)
	return index
}

func rankedFileSuggestions(index fileSuggestionIndex, partial string, limit int) []commandSuggestion {
	query := strings.ToLower(strings.TrimSpace(partial))
	candidates := make([]fileSuggestionCandidate, 0, len(index.files)+len(index.dirs))
	for _, file := range index.files {
		candidates = append(candidates, fileSuggestionCandidate{path: file})
	}
	for _, dir := range index.dirs {
		candidates = append(candidates, fileSuggestionCandidate{path: dir, isDir: true})
	}
	type scoredCandidate struct {
		candidate fileSuggestionCandidate
		score     int
	}
	scored := make([]scoredCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		score, ok := scoreFileSuggestion(candidate, query)
		if ok {
			scored = append(scored, scoredCandidate{candidate: candidate, score: score})
		}
	}
	sort.SliceStable(scored, func(i, j int) bool {
		left, right := scored[i], scored[j]
		if left.score != right.score {
			return left.score < right.score
		}
		leftDepth := strings.Count(left.candidate.path, "/")
		rightDepth := strings.Count(right.candidate.path, "/")
		if leftDepth != rightDepth {
			return leftDepth < rightDepth
		}
		if left.candidate.isDir != right.candidate.isDir {
			return !left.candidate.isDir
		}
		return left.candidate.path < right.candidate.path
	})

	out := make([]commandSuggestion, 0, minInt(limit, len(scored)))
	for _, item := range scored {
		if len(out) >= limit {
			break
		}
		name := "@" + item.candidate.path
		desc := "file"
		if item.candidate.isDir {
			name += "/"
			desc = "directory"
		}
		out = append(out, commandSuggestion{Name: name, Desc: desc})
	}
	return out
}

func scoreFileSuggestion(candidate fileSuggestionCandidate, query string) (int, bool) {
	cleanPath := strings.TrimSuffix(filepath.ToSlash(candidate.path), "/")
	base := strings.ToLower(path.Base(cleanPath))
	full := strings.ToLower(cleanPath)
	depth := strings.Count(cleanPath, "/")
	dirPenalty := 0
	if candidate.isDir {
		dirPenalty = 5
	}
	if query == "" {
		return depth*20 + len(base)/8 + dirPenalty, true
	}
	switch {
	case base == query:
		return 0 + depth + dirPenalty, true
	case strings.HasPrefix(base, query):
		return 20 + depth + dirPenalty, true
	case strings.Contains(base, query):
		return 40 + depth + dirPenalty, true
	case strings.HasPrefix(full, query):
		return 60 + depth + dirPenalty, true
	case strings.Contains(full, query):
		return 80 + depth + dirPenalty, true
	default:
		if gap, ok := fuzzySubsequenceGap(full, query); ok {
			return 120 + gap + depth + dirPenalty, true
		}
		return 0, false
	}
}

func fuzzySubsequenceGap(value, query string) (int, bool) {
	if query == "" {
		return 0, true
	}
	valueRunes := []rune(value)
	gap := 0
	last := -1
	pos := 0
	for _, q := range query {
		found := -1
		for pos < len(valueRunes) {
			if valueRunes[pos] == q {
				found = pos
				pos++
				break
			}
			pos++
		}
		if found < 0 {
			return 0, false
		}
		if last >= 0 {
			gap += found - last - 1
		}
		last = found
	}
	return gap, true
}

// dismissSuggestions clears the overlay without touching the run. Command
// palette dismissal also clears the leading slash fragment because the palette
// owns that search text while it is open.
func (m model) dismissSuggestions() model {
	if m.suggestionsAreFiles {
		state := m.fileSuggestionComposerState()
		if query := extractPathQuery(state.text, state.cursor); query != nil {
			m = m.replaceComposerRangeWithPastePreviews(state, query.StartIndex, query.EndIndex, "")
		}
	} else {
		m.clearComposer()
	}
	m.suggestions = nil
	m.suggestionIdx = 0
	m.suggestionsAreFiles = false
	m.commandPaletteOpen = false
	m.filePaletteOpen = false
	return m
}
