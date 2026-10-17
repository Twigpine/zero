package agent

import (
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/Twigpine/zero/internal/zeroruntime"
)

// Compaction preservation.
//
// A plain prose summary loses structured state the model needs to keep working:
// the active plan, loaded deferred-tool schemas, loaded skills, and project
// instruction blocks. When those turns fall into the elided middle, Compact
// appends that state to the injected summary as JSON so it survives exactly
// rather than being paraphrased away.

const (
	toolNameUpdatePlan = "update_plan"
	toolNameToolSearch = "tool_search"
	toolNameSkill      = "skill"
	toolNameWriteFile  = "write_file"
	toolNameEditFile   = "edit_file"
)

// maxRecentEdits caps how many edited files are carried across a compaction, and
// maxEditNoteBytes caps each edit's one-line note, so a long editing run can't
// bloat the preserved-state block.
const (
	maxRecentEdits   = 20
	maxEditNoteBytes = 160
	// maxTaskObjectiveBytes keeps the original objective recognizable after
	// compaction without allowing an unusually large prompt to recreate the
	// context pressure the compaction just removed.
	maxTaskObjectiveBytes = 512
)

const (
	projectInstructionsHeadingPrefix = "# "
	projectInstructionsHeadingMarker = " instructions for "
	projectInstructionsOpenTag       = "<INSTRUCTIONS>"
	projectInstructionsCloseTag      = "</INSTRUCTIONS>"
)

// preservedStateLabel heads the preserved-state block. Keep this label stable so
// summaries created by earlier builds remain parseable; the JSON body may carry
// more fields than the historical label names.
//
// The block body is a single line of JSON (see formatPreservedState): JSON
// escapes everything, so markdown headings, code fences, or quotes round-trip
// losslessly across repeated compactions.
const preservedStateLabel = "## Preserved state (active plan + loaded skills; carried across compaction)"

// maxPreservedSkillBytes caps how much of each loaded skill body is carried
// across a compaction, so a large skill can't defeat the compaction it is part
// of. The skill's name and head survive; the model can re-load it in full if it
// needs the rest.
const maxPreservedSkillBytes = 2 << 10 // 2 KiB

// extractLatestPlan returns a formatted view of the most recent update_plan tool
// call in messages, so an in-progress plan survives when its turns are elided by
// compaction. Returns "" when no plan was issued.
func extractLatestPlan(messages []zeroruntime.Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		calls := messages[i].ToolCalls
		for j := len(calls) - 1; j >= 0; j-- {
			if calls[j].Name != toolNameUpdatePlan {
				continue
			}
			if formatted := formatPlanArguments(calls[j].Arguments); formatted != "" {
				return formatted
			}
		}
	}
	return ""
}

// formatPlanArguments renders an update_plan tool call's JSON arguments
// ({"plan":[{content,status,...}]}) as terse status-tagged bullet lines. Returns
// "" on malformed arguments or an empty plan.
func formatPlanArguments(arguments string) string {
	plan, ok := parseTaskPlan(arguments)
	if !ok {
		return ""
	}
	lines := make([]string, 0, len(plan))
	for _, item := range plan {
		line := "- [" + item.Status + "] " + item.Content
		if notes := item.Notes; notes != "" {
			line += "\n  Notes: " + notes
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

// skillEntry is a named preserved body. It began as loaded-skill state and is
// reused for loaded tools, project instruction blocks, and recent file edits.
type skillEntry struct {
	name string
	body string
}

// recentEdits returns files mutated by editing calls in messages
// — latest note per path, in last-seen order — as skillEntry{name: path, body:
// note}. After compaction elides the editing turns, this tells the model WHAT it
// changed in each file (from the tool's result) so it need not re-read to
// rediscover its own footprint. Capped at maxRecentEdits paths.
//
// Ordering is by LAST edit, not first: re-editing a file moves it to the newest
// position so the tail cap below keeps the file the model most recently touched
// rather than an earlier, now-stale entry.
func recentEdits(messages []zeroruntime.Message) []skillEntry {
	pathByID := map[string]string{}
	noteByPath := map[string]string{}
	sequence := make([]string, 0)
	for _, message := range messages {
		for _, call := range message.ToolCalls {
			if call.Name != toolNameWriteFile && call.Name != toolNameEditFile {
				continue
			}
			path := editPathFromArguments(call.Arguments)
			if path == "" {
				continue
			}
			if call.ID != "" {
				pathByID[call.ID] = path
			}
			sequence = append(sequence, path)
		}
	}
	for _, message := range messages {
		if message.Role != zeroruntime.MessageRoleTool || len(message.ChangedFiles) == 0 {
			continue
		}
		for _, path := range message.ChangedFiles {
			path = strings.TrimSpace(path)
			if path == "" {
				continue
			}
			sequence = append(sequence, path)
			noteByPath[path] = editNote(message.Content)
		}
	}
	order := lastSeenOrder(sequence)
	if len(order) == 0 {
		return nil
	}

	for _, message := range messages {
		if message.Role != zeroruntime.MessageRoleTool || message.ToolCallID == "" {
			continue
		}
		if path, ok := pathByID[message.ToolCallID]; ok {
			noteByPath[path] = editNote(message.Content)
		}
	}

	// Keep the most recent maxRecentEdits paths (the tail of last-seen order).
	if len(order) > maxRecentEdits {
		order = order[len(order)-maxRecentEdits:]
	}
	entries := make([]skillEntry, 0, len(order))
	for _, path := range order {
		entries = append(entries, skillEntry{name: path, body: noteByPath[path]})
	}
	return entries
}

// lastSeenOrder dedupes paths keeping each at its LAST occurrence, preserving
// chronological order otherwise. A re-edited path therefore lands at the newest
// position rather than staying pinned to where it first appeared.
func lastSeenOrder(paths []string) []string {
	lastIdx := make(map[string]int, len(paths))
	for i, p := range paths {
		lastIdx[p] = i
	}
	order := make([]string, 0, len(lastIdx))
	for i, p := range paths {
		if lastIdx[p] == i {
			order = append(order, p)
		}
	}
	return order
}

// editPathFromArguments pulls the target file path from a write_file/edit_file
// call's JSON arguments (path/file/file_path/filename aliases). Returns "" on
// malformed arguments or no path.
func editPathFromArguments(arguments string) string {
	var parsed struct {
		Path     string `json:"path"`
		File     string `json:"file"`
		FilePath string `json:"file_path"`
		Filename string `json:"filename"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(arguments)), &parsed); err != nil {
		return ""
	}
	for _, candidate := range []string{parsed.Path, parsed.File, parsed.FilePath, parsed.Filename} {
		if s := strings.TrimSpace(candidate); s != "" {
			return s
		}
	}
	return ""
}

// editNote reduces an edit tool's result to a single short line (its first
// non-empty line, byte-capped on a rune boundary) for the preserved summary.
func editNote(content string) string {
	for _, line := range strings.Split(content, "\n") {
		s := strings.TrimSpace(line)
		if s == "" {
			continue
		}
		if len(s) > maxEditNoteBytes {
			limit := maxEditNoteBytes
			for limit > 0 && !utf8.RuneStart(s[limit]) {
				limit--
			}
			s = s[:limit] + "…"
		}
		return s
	}
	return ""
}

// loadedSkills returns the skills loaded via the skill tool in messages — the
// latest body per name, in first-seen order — matching each skill tool call to
// its tool result by id.
func loadedSkills(messages []zeroruntime.Message) []skillEntry {
	nameByID := map[string]string{}
	for _, message := range messages {
		for _, call := range message.ToolCalls {
			if call.Name == toolNameSkill && call.ID != "" {
				nameByID[call.ID] = skillNameFromArguments(call.Arguments)
			}
		}
	}
	if len(nameByID) == 0 {
		return nil
	}

	bodyByName := map[string]string{}
	nameOrder := make([]string, 0, len(nameByID))
	for _, message := range messages {
		if message.Role != zeroruntime.MessageRoleTool || message.ToolCallID == "" {
			continue
		}
		name, ok := nameByID[message.ToolCallID]
		if !ok {
			continue
		}
		if name == "" {
			name = "(unnamed)"
		}
		body := strings.TrimSpace(message.Content)
		if body == "" {
			continue
		}
		if _, seen := bodyByName[name]; !seen {
			nameOrder = append(nameOrder, name)
		}
		bodyByName[name] = capBody(body)
	}

	entries := make([]skillEntry, 0, len(nameOrder))
	for _, name := range nameOrder {
		entries = append(entries, skillEntry{name: name, body: bodyByName[name]})
	}
	return entries
}

// loadedToolSchemas returns tool_search-loaded schemas from their normal tool
// result text. ToolResult.Meta is not part of zeroruntime.Message history, so the
// rendered "Loaded N tools" output is the durable transcript format.
func loadedToolSchemas(messages []zeroruntime.Message) []skillEntry {
	toolSearchIDs := map[string]bool{}
	for _, message := range messages {
		for _, call := range message.ToolCalls {
			if call.Name == toolNameToolSearch && call.ID != "" {
				toolSearchIDs[call.ID] = true
			}
		}
	}
	if len(toolSearchIDs) == 0 {
		return nil
	}

	bodyByName := map[string]string{}
	nameOrder := make([]string, 0)
	for _, message := range messages {
		if message.Role != zeroruntime.MessageRoleTool || !toolSearchIDs[message.ToolCallID] {
			continue
		}
		for _, entry := range loadedToolEntriesFromOutput(message.Content) {
			if _, seen := bodyByName[entry.name]; !seen {
				nameOrder = append(nameOrder, entry.name)
			}
			bodyByName[entry.name] = entry.body
		}
	}

	entries := make([]skillEntry, 0, len(nameOrder))
	for _, name := range nameOrder {
		entries = append(entries, skillEntry{name: name, body: bodyByName[name]})
	}
	return entries
}

func loadedToolEntriesFromOutput(output string) []skillEntry {
	output = strings.TrimSpace(output)
	if !strings.HasPrefix(output, "Loaded ") || !strings.Contains(output, "Full schemas follow") {
		return nil
	}
	lines := strings.Split(output, "\n")
	var entries []skillEntry
	for i := 0; i < len(lines); i++ {
		name, ok := strings.CutPrefix(strings.TrimSpace(lines[i]), "## ")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		start := i
		end := len(lines)
		for j := i + 1; j < len(lines); j++ {
			if strings.HasPrefix(strings.TrimSpace(lines[j]), "## ") {
				end = j
				break
			}
		}
		entries = append(entries, skillEntry{name: name, body: capBody(strings.TrimSpace(strings.Join(lines[start:end], "\n")))})
		i = end - 1
	}
	return entries
}

func projectInstructionEntries(messages []zeroruntime.Message) []skillEntry {
	bodyBySource := map[string]string{}
	sourceOrder := make([]string, 0)
	for _, message := range messages {
		if message.Role != zeroruntime.MessageRoleUser {
			continue
		}
		source, body := projectInstructionBlock(message.Content)
		if body == "" {
			continue
		}
		if _, seen := bodyBySource[source]; !seen {
			sourceOrder = append(sourceOrder, source)
		}
		bodyBySource[source] = body
	}

	entries := make([]skillEntry, 0, len(sourceOrder))
	for _, source := range sourceOrder {
		entries = append(entries, skillEntry{name: source, body: bodyBySource[source]})
	}
	return entries
}

func projectInstructionBlock(content string) (string, string) {
	content = strings.TrimSpace(content)
	if !strings.HasPrefix(content, projectInstructionsHeadingPrefix) {
		return "", ""
	}
	firstLineEnd := strings.IndexByte(content, '\n')
	if firstLineEnd < 0 {
		return "", ""
	}
	heading := strings.TrimSpace(content[:firstLineEnd])
	if !strings.Contains(heading, projectInstructionsHeadingMarker) {
		return "", ""
	}
	open := strings.Index(content, projectInstructionsOpenTag)
	close := strings.Index(content, projectInstructionsCloseTag)
	if open < 0 || close < open {
		return "", ""
	}
	close += len(projectInstructionsCloseTag)

	source := strings.TrimPrefix(heading, "# ")
	body := strings.TrimSpace(heading + "\n\n" + strings.TrimSpace(content[open:close]))
	return source, body
}

// skillNameFromArguments pulls the "name" field from a skill tool call's JSON
// arguments ({"name":"..."}). Returns "" on malformed arguments.
func skillNameFromArguments(arguments string) string {
	var parsed struct {
		Name  string `json:"name"`
		Skill string `json:"skill"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(arguments)), &parsed); err != nil {
		return ""
	}
	if name := strings.TrimSpace(parsed.Name); name != "" {
		return name
	}
	return strings.TrimSpace(parsed.Skill)
}

// truncationNote is appended to a capped skill body. Its length is reserved
// within the byte budget so the result never exceeds maxPreservedSkillBytes.
const truncationNote = "\n… (truncated; re-load the skill for the full body)"

// capBody truncates an over-long skill body to maxPreservedSkillBytes BYTES,
// cutting on a UTF-8 rune boundary (never splitting a multibyte rune) and
// reserving room for the note so the result stays within the byte budget. The
// note is added only when the body is actually truncated.
func capBody(body string) string {
	if len(body) <= maxPreservedSkillBytes {
		return body
	}
	limit := maxPreservedSkillBytes - len(truncationNote)
	if limit < 0 {
		limit = 0
	}
	// Walk back to the start of a rune so a multibyte sequence is never split.
	for limit > 0 && !utf8.RuneStart(body[limit]) {
		limit--
	}
	return body[:limit] + truncationNote
}

// preservedState is the JSON shape of the carried-across-compaction block.
type preservedState struct {
	Plan                string                 `json:"plan,omitempty"`
	Task                *preservedTaskState    `json:"task,omitempty"`
	RecentEdits         []preservedEdit        `json:"recent_edits,omitempty"`
	Tools               []preservedTool        `json:"tools,omitempty"`
	Skills              []preservedSkill       `json:"skills,omitempty"`
	ProjectInstructions []preservedInstruction `json:"project_instructions,omitempty"`
}

type preservedTaskState struct {
	Objective           string              `json:"objective"`
	Status              taskStatus          `json:"status,omitempty"`
	Pending             int                 `json:"pending,omitempty"`
	InProgress          int                 `json:"in_progress,omitempty"`
	Completed           int                 `json:"completed,omitempty"`
	Failed              int                 `json:"failed,omitempty"`
	VerificationPassed  int                 `json:"verification_passed,omitempty"`
	VerificationFailed  int                 `json:"verification_failed,omitempty"`
	VerificationOutcome Outcome             `json:"verification_outcome,omitempty"`
	Constraints         []string            `json:"constraints,omitempty"`
	ChangedFiles        []string            `json:"changed_files,omitempty"`
	UnresolvedFailures  []taskFailureState  `json:"unresolved_failures,omitempty"`
	Approvals           []taskApprovalState `json:"approvals,omitempty"`
	Artifacts           []taskArtifactState `json:"artifacts,omitempty"`
}

type preservedEdit struct {
	Path string `json:"path"`
	Note string `json:"note,omitempty"`
}

type preservedTool struct {
	Name string `json:"name"`
	Body string `json:"body"`
}

type preservedSkill struct {
	Name string `json:"name"`
	Body string `json:"body"`
}

type preservedInstruction struct {
	Source string `json:"source"`
	Body   string `json:"body"`
}

// appendPreservedState appends active structured state to a compaction summary
// as a single JSON block. middle is the slice being summarized away.
//
// It is robust across repeated compactions: after the first compaction the state
// may live only inside the injected summary message, which on a later compaction
// lands in middle with no real tool calls left to extract. Fresh tool calls and
// instruction blocks override the carried-forward copy by name/source.
func appendPreservedState(summary string, middle []zeroruntime.Message, taskSnapshot *taskStateSnapshot) string {
	priorState := parsePreservedStateBlock(latestSummaryContent(middle))
	task := priorState.Task
	if taskSnapshot != nil {
		task = &preservedTaskState{
			Objective:          capTaskObjective(taskSnapshot.Objective),
			Constraints:        mergeBoundedComparable(priorTaskConstraints(priorState.Task), taskSnapshot.Constraints, maxTaskConstraints),
			ChangedFiles:       mergeBoundedComparable(priorTaskChangedFiles(priorState.Task), taskSnapshot.ChangedFiles, maxTaskEvidenceEntries),
			UnresolvedFailures: mergeTaskFailures(priorTaskFailures(priorState.Task), taskSnapshot.UnresolvedFailures, taskSnapshot.ResolvedFailureKeys),
			Approvals:          mergeBoundedComparable(priorTaskApprovals(priorState.Task), taskSnapshot.Approvals, maxTaskEvidenceEntries),
			Artifacts:          mergeBoundedComparable(priorTaskArtifacts(priorState.Task), taskSnapshot.Artifacts, maxTaskEvidenceEntries),
		}
		// Plan parity corroborates only the mutable task projection. The objective
		// comes directly from the run prompt and is immutable, so it must survive
		// even after compaction removes the plan tool call needed for comparison.
		if taskSnapshot.PlanParity == taskPlanParityMatch {
			task.Status = taskSnapshot.Status
			task.Pending = taskSnapshot.Plan.Pending
			task.InProgress = taskSnapshot.Plan.InProgress
			task.Completed = taskSnapshot.Plan.Completed
			task.Failed = taskSnapshot.Plan.Failed
			task.VerificationPassed = taskSnapshot.Verification.Passed
			task.VerificationFailed = taskSnapshot.Verification.Failed
			task.VerificationOutcome = taskSnapshot.Verification.LastOutcome
		}
	}
	freshConstraints := explicitConstraintsFromMessages(middle)
	if len(freshConstraints) > 0 {
		if task == nil {
			task = &preservedTaskState{}
		}
		task.Constraints = mergeBoundedComparable(task.Constraints, freshConstraints, maxTaskConstraints)
	}

	// Plan: a fresh update_plan in middle is authoritative; otherwise carry the
	// plan preserved by an earlier compaction.
	plan := extractLatestPlan(middle)
	if plan == "" {
		plan = priorState.Plan
	}

	// Recent edits: merge edits preserved earlier with fresh write_file/edit_file
	// results in middle (newer note per path wins AND moves to the newest
	// position), so the tail cap keeps the files the model most recently touched
	// across repeated compactions rather than dropping a just-re-edited file.
	edits := capRecentEdits(mergeRecentEdits(preservedEditsToEntries(priorState.RecentEdits), recentEdits(middle)))

	// Tools: preserve deferred tool_search schemas from the transcript. Fresh
	// loads override older carried copies by name.
	tools := mergeSkillEntries(preservedToolsToEntries(priorState.Tools), loadedToolSchemas(middle))

	// Skills: merge skills preserved earlier (older) with fresh loads (newer wins
	// per name), so a loaded skill survives repeated compactions.
	skills := mergeSkillEntries(preservedSkillsToEntries(priorState.Skills), loadedSkills(middle))

	instructions := mergeSkillEntries(
		preservedInstructionsToEntries(priorState.ProjectInstructions),
		projectInstructionEntries(middle),
	)

	if block := formatPreservedState(plan, task, edits, tools, skills, instructions); block != "" {
		summary += "\n\n" + block
	}
	return summary
}

func explicitConstraintsFromMessages(messages []zeroruntime.Message) []string {
	var constraints []string
	for _, message := range messages {
		if message.Role != zeroruntime.MessageRoleUser || strings.Contains(message.Content, preservedStateLabel) {
			continue
		}
		if _, body := projectInstructionBlock(message.Content); body != "" {
			continue
		}
		constraints = mergeBoundedComparable(constraints, extractExplicitConstraints(message.Content), maxTaskConstraints)
	}
	return constraints
}

func priorTaskConstraints(task *preservedTaskState) []string {
	if task == nil {
		return nil
	}
	return task.Constraints
}

func priorTaskChangedFiles(task *preservedTaskState) []string {
	if task == nil {
		return nil
	}
	return task.ChangedFiles
}

func priorTaskFailures(task *preservedTaskState) []taskFailureState {
	if task == nil {
		return nil
	}
	return task.UnresolvedFailures
}

func priorTaskApprovals(task *preservedTaskState) []taskApprovalState {
	if task == nil {
		return nil
	}
	return task.Approvals
}

func priorTaskArtifacts(task *preservedTaskState) []taskArtifactState {
	if task == nil {
		return nil
	}
	return task.Artifacts
}

func mergeTaskFailures(older, newer []taskFailureState, resolved []string) []taskFailureState {
	resolvedSet := make(map[string]struct{}, len(resolved))
	for _, key := range resolved {
		resolvedSet[key] = struct{}{}
	}
	merged := make([]taskFailureState, 0, len(older)+len(newer))
	for _, failure := range append(append([]taskFailureState(nil), older...), newer...) {
		if _, ok := resolvedSet[failure.Key]; ok {
			continue
		}
		withoutSameKey := make([]taskFailureState, 0, len(merged))
		for _, existing := range merged {
			if existing.Key != failure.Key {
				withoutSameKey = append(withoutSameKey, existing)
			}
		}
		merged = append(withoutSameKey, failure)
	}
	if len(merged) > maxTaskEvidenceEntries {
		merged = merged[len(merged)-maxTaskEvidenceEntries:]
	}
	return merged
}

func mergeBoundedComparable[T comparable](older, newer []T, limit int) []T {
	merged := make([]T, 0, len(older)+len(newer))
	seen := make(map[T]struct{}, len(older)+len(newer))
	for _, value := range append(append([]T(nil), older...), newer...) {
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		merged = append(merged, value)
	}
	if len(merged) > limit {
		merged = merged[len(merged)-limit:]
	}
	return merged
}

func capTaskObjective(objective string) string {
	objective = strings.TrimSpace(objective)
	if len(objective) <= maxTaskObjectiveBytes {
		return objective
	}
	const suffix = "…"
	limit := maxTaskObjectiveBytes - len(suffix)
	for limit > 0 && !utf8.RuneStart(objective[limit]) {
		limit--
	}
	return strings.TrimSpace(objective[:limit]) + suffix
}

// mergeRecentEdits overlays fresh edits onto edits preserved by an earlier
// compaction. Unlike mergeSkillEntries (which keeps refreshed entries in their
// original slot), a path touched again by a fresh edit MOVES to the newest
// position and takes the fresh note, so capRecentEdits — which keeps only the
// most-recent tail — never drops a file the model just re-edited. Paths present
// only in the older set keep their relative order, ahead of the fresh ones.
func mergeRecentEdits(older, newer []skillEntry) []skillEntry {
	if len(newer) == 0 {
		return older
	}
	freshBody := make(map[string]string, len(newer))
	freshOrder := make([]string, 0, len(newer))
	for _, e := range newer {
		if _, ok := freshBody[e.name]; !ok {
			freshOrder = append(freshOrder, e.name)
		}
		freshBody[e.name] = e.body
	}
	merged := make([]skillEntry, 0, len(older)+len(freshOrder))
	// Older entries that were NOT re-edited keep their position, oldest first.
	for _, e := range older {
		if _, refreshed := freshBody[e.name]; refreshed {
			continue // re-appended at its newest position below
		}
		merged = append(merged, e)
	}
	// Fresh edits follow, in last-seen order, each carrying the fresh note — so
	// the most recently touched files sit at the tail the cap preserves.
	for _, name := range freshOrder {
		merged = append(merged, skillEntry{name: name, body: freshBody[name]})
	}
	return merged
}

// capRecentEdits bounds the preserved edit list to the most recent maxRecentEdits
// entries after a merge, so repeated compactions can't grow it without limit.
func capRecentEdits(entries []skillEntry) []skillEntry {
	if len(entries) > maxRecentEdits {
		return entries[len(entries)-maxRecentEdits:]
	}
	return entries
}

func preservedEditsToEntries(edits []preservedEdit) []skillEntry {
	entries := make([]skillEntry, 0, len(edits))
	for _, e := range edits {
		if e.Path == "" {
			continue
		}
		entries = append(entries, skillEntry{name: e.Path, body: e.Note})
	}
	return entries
}

// formatPreservedState renders state as the labelled, single-line
// JSON block. Returns "" when there is nothing to preserve.
func formatPreservedState(plan string, task *preservedTaskState, edits, tools, skills, instructions []skillEntry) string {
	if plan == "" && task == nil && len(edits) == 0 && len(tools) == 0 && len(skills) == 0 && len(instructions) == 0 {
		return ""
	}
	state := preservedState{Plan: plan, Task: task}
	for _, e := range edits {
		state.RecentEdits = append(state.RecentEdits, preservedEdit{Path: e.name, Note: e.body})
	}
	for _, t := range tools {
		state.Tools = append(state.Tools, preservedTool{Name: t.name, Body: t.body})
	}
	for _, s := range skills {
		state.Skills = append(state.Skills, preservedSkill{Name: s.name, Body: s.body})
	}
	for _, i := range instructions {
		state.ProjectInstructions = append(state.ProjectInstructions, preservedInstruction{Source: i.name, Body: i.body})
	}
	encoded, err := json.Marshal(state)
	if err != nil {
		return ""
	}
	return preservedStateLabel + "\n" + string(encoded)
}

func parsePreservedStateBlock(summaryContent string) preservedState {
	idx := strings.LastIndex(summaryContent, preservedStateLabel)
	if idx < 0 {
		return preservedState{}
	}
	rest := strings.TrimPrefix(summaryContent[idx+len(preservedStateLabel):], "\n")
	// The JSON is a single line (json.Marshal escapes newlines).
	if nl := strings.IndexByte(rest, '\n'); nl >= 0 {
		rest = rest[:nl]
	}
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return preservedState{}
	}
	var state preservedState
	if err := json.Unmarshal([]byte(rest), &state); err != nil {
		return preservedState{}
	}
	return state
}

func preservedToolsToEntries(tools []preservedTool) []skillEntry {
	entries := make([]skillEntry, 0, len(tools))
	for _, t := range tools {
		if t.Name == "" {
			continue
		}
		entries = append(entries, skillEntry{name: t.Name, body: t.Body})
	}
	return entries
}

func preservedSkillsToEntries(skills []preservedSkill) []skillEntry {
	entries := make([]skillEntry, 0, len(skills))
	for _, s := range skills {
		if s.Name == "" {
			continue
		}
		entries = append(entries, skillEntry{name: s.Name, body: s.Body})
	}
	return entries
}

func preservedInstructionsToEntries(instructions []preservedInstruction) []skillEntry {
	entries := make([]skillEntry, 0, len(instructions))
	for _, i := range instructions {
		if i.Source == "" {
			continue
		}
		entries = append(entries, skillEntry{name: i.Source, body: i.Body})
	}
	return entries
}

// latestSummaryContent returns the content of the most recent injected summary
// message in messages (a user message beginning with summaryLabel), or "".
func latestSummaryContent(messages []zeroruntime.Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		m := messages[i]
		if m.Role == zeroruntime.MessageRoleUser && strings.HasPrefix(strings.TrimSpace(m.Content), summaryLabel) {
			return m.Content
		}
	}
	return ""
}

// mergeSkillEntries overlays newer skill loads onto older preserved entries by
// name (newer body wins), keeping the older order and appending genuinely-new
// skills after.
func mergeSkillEntries(older, newer []skillEntry) []skillEntry {
	if len(newer) == 0 {
		return older
	}
	newBody := make(map[string]string, len(newer))
	for _, e := range newer {
		newBody[e.name] = e.body
	}
	merged := make([]skillEntry, 0, len(older)+len(newer))
	seen := make(map[string]bool, len(older))
	for _, e := range older {
		if b, ok := newBody[e.name]; ok {
			e.body = b
		}
		merged = append(merged, e)
		seen[e.name] = true
	}
	for _, e := range newer {
		if !seen[e.name] {
			merged = append(merged, e)
		}
	}
	return merged
}
