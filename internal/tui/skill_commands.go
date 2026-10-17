package tui

import (
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/Twigpine/zero/internal/skills"
)

// Skill slash invocation: "/<skill-name> [args]" runs an installed skill
// directly, mirroring user commands (.zero/commands). Skills were previously
// model-pulled only (the skill tool), which made invocation a matter of model
// discretion; typing the skill's name makes it deterministic. Precedence is
// builtin > user command > skill: parseCommand resolves builtins first, and the
// commandUnknown fallback tries user commands before skills.

// handleSkillCommand resolves a "/name args" that wasn't a builtin or a user
// command against the installed skills. On a match it launches a normal agent
// turn whose prompt inlines the skill body (its instructions) followed by the
// typed args, returning handled=true. handled=false means no skill matched, so
// the caller falls through to "unknown command".
func (m model) handleSkillCommand(raw string) (model, tea.Cmd, bool) {
	name, args := splitUserCommand(raw)
	if name == "" {
		return m, nil, false
	}
	skill, ok := m.lookupSkillCommand(name)
	if !ok {
		return m, nil, false
	}
	body := strings.TrimSpace(skill.Content)
	if body == "" {
		// The name matched a real skill, so falling through to "unknown command"
		// would mislead; surface the actual problem instead.
		m.transcript = reduceTranscript(m.transcript, transcriptAction{
			kind: actionAppendError,
			text: "skill /" + name + " has an empty SKILL.md body (" + skill.Path + ")",
		})
		return m, nil, true
	}
	return m.launchOrDeferExpandedPrompt(raw, skillInvocationPrompt(body, args))
}

// bareSkillInvocationNote is appended when a skill is invoked with no request.
// The body alone is instructions with no target ("review the PR" — which PR?),
// and without this note the model improvises one instead of asking. The wording
// is conditional so self-contained skills (no target needed) still just run.
const bareSkillInvocationNote = "The user invoked this skill directly without providing a request. " +
	"If these instructions need a target or details that are not already clear from the conversation " +
	"(which pull request, file, branch, topic, …), ask for them first — do not guess or pick one yourself. " +
	"If the instructions are self-contained, proceed."

// skillInvocationPrompt builds the agent prompt for a skill invocation: the
// skill body (its instructions), then either the user's request or — for a bare
// invocation — the ask-first note above. Mirrors usercommands.Expand's
// no-placeholder behavior for the args case.
func skillInvocationPrompt(body, args string) string {
	if args != "" {
		return body + "\n\n" + args
	}
	return body + "\n\n" + bareSkillInvocationNote
}

// launchOrDeferExpandedPrompt applies the same run-state guards the plain
// commandPrompt path has to an expanded (user command / skill) prompt: while
// exiting nothing may start, a prompt submitted mid-run is queued for the next
// turn, and compaction-in-flight warns instead of racing the compactor. The
// EXPANDED prompt is what gets queued — the queue flush path resubmits text as
// a literal prompt, so queuing the raw "/name args" would send it to the model
// as prose instead of re-dispatching it. raw is the typed invocation; on the
// compaction warning it is restored into the composer for an easy re-submit
// (the commandUnknown dispatch cleared the composer before we got here, unlike
// the plain-prompt path whose guard fires before the clear). Empty raw (the
// picker path has no typed form) restores nothing.
func (m model) launchOrDeferExpandedPrompt(raw, prompt string) (model, tea.Cmd, bool) {
	if m.exiting {
		return m, nil, true
	}
	if m.pending {
		return m.queueMessage(prompt), nil, true
	}
	if m.compactInFlight {
		if strings.TrimSpace(raw) != "" {
			m.input.SetValue(raw)
			m.input.CursorEnd()
		}
		m.transcript = reduceTranscript(m.transcript, transcriptAction{
			kind: actionAppendSystem,
			text: "Compact\nstatus: warning\nCompaction is running. Re-run the command when it finishes.",
		})
		return m, nil, true
	}
	next, teaCmd := m.launchPrompt(prompt)
	return next, teaCmd, true
}

// takenSlashNames returns every slash token already claimed by a builtin
// command (or alias) or a user command — the names a typed "/x" would dispatch
// to before ever reaching a skill.
func (m model) takenSlashNames() map[string]bool {
	taken := map[string]bool{}
	for _, command := range commandDefinitions {
		taken[strings.TrimPrefix(command.name, "/")] = true
		for _, alias := range command.aliases {
			taken[strings.TrimPrefix(alias, "/")] = true
		}
	}
	for _, cmd := range m.userCommands {
		taken[cmd.Name] = true
	}
	return taken
}

// chooseSkillFromPicker handles Enter on a skill-picker row. Most skills want a
// request ("review WHICH pr?"), so selection fills the composer with
// "/name " — cursor at the end — for the user to complete and submit (Enter
// again runs it bare). Skills whose names cannot be typed as a slash command
// (non-slash shapes, or names shadowed by a builtin or user command) fall back
// to running immediately: the picker is the only path that reaches them, and an
// unfillable composer would be a dead end.
func (m model) chooseSkillFromPicker(item pickerItem) (model, tea.Cmd) {
	slash := skillSlashName(item.Value)
	if slash == "" || m.takenSlashNames()[slash] {
		return m.invokeSkillByName(item.Value)
	}
	m.input.SetValue("/" + slash + " ")
	m.input.CursorEnd()
	return m, nil
}

// invokeSkillByName runs the installed skill with the given EXACT raw name (the
// skill-picker path). The same run-state guards as slash invocation apply; the
// picker offers no args affordance, so the prompt is the skill body alone.
func (m model) invokeSkillByName(name string) (model, tea.Cmd) {
	for _, skill := range m.installedSkills() {
		if strings.TrimSpace(skill.Name) != name {
			continue
		}
		body := strings.TrimSpace(skill.Content)
		if body == "" {
			m.transcript = reduceTranscript(m.transcript, transcriptAction{
				kind: actionAppendError,
				text: "skill " + name + " has an empty SKILL.md body (" + skill.Path + ")",
			})
			return m, nil
		}
		next, teaCmd, _ := m.launchOrDeferExpandedPrompt("", skillInvocationPrompt(body, ""))
		return next, teaCmd
	}
	// The picker row came from a slightly older load (TTL cache) and the skill
	// has since been removed.
	m.transcript = reduceTranscript(m.transcript, transcriptAction{
		kind: actionAppendError,
		text: "skill " + name + " is no longer installed",
	})
	return m, nil
}

// lookupSkillCommand returns the installed skill whose slash name matches the
// given (lowercased) name. Linear scan over a fresh load — skills are re-read
// per invocation so a skill installed mid-session is invocable without a
// restart, and the body is never held on the model.
func (m model) lookupSkillCommand(name string) (skills.Skill, bool) {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return skills.Skill{}, false
	}
	for _, skill := range m.installedSkills() {
		if skillSlashName(skill.Name) == name {
			return skill, true
		}
	}
	return skills.Skill{}, false
}

// installedSkills returns the session's installed skills (default dir merged
// with plugin skill roots) via the injected loader, or nil when the session has
// no skills wiring (e.g. bare test models).
func (m model) installedSkills() []skills.Skill {
	if m.loadSkills == nil {
		return nil
	}
	return m.loadSkills()
}

// skillSlashName maps a skill's frontmatter name to its slash-command form:
// lowercased, and only if it fits the slash-token shape (letters, digits,
// dot/underscore/hyphen — a superset of user-command names, since skill names
// are free-form frontmatter). Returns "" for names that cannot be typed as a
// /command (e.g. containing spaces); those skills remain loadable by the model
// via the skill tool and are still listed by /skills.
func skillSlashName(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return ""
	}
	for _, r := range name {
		valid := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.'
		if !valid {
			return ""
		}
	}
	return name
}
