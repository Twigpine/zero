package agent

import "strings"

// Per-model prompt specialization.
//
// Different model families respond to different framing: the GPT family benefits
// from an explicit markdown/output spec and a strong "prefer native tools" nudge;
// Gemini benefits from explicit tool-preference and conciseness guidance. The
// Claude family is already aligned with the core prompt and needs no addendum —
// comment discipline is universal in the core prompt now, so every family gets it.
// modelPromptAddendum returns the family-specific block appended after the core
// prompt, or "" when the family is unknown or needs no specialization.
// Classification is by model id (the agent only has the model string — no registry
// dependency).

const (
	familyOpenAI    = "openai"
	familyGemini    = "gemini"
	familyAnthropic = "anthropic"
)

// modelFamily classifies a model id into a prompt-tuning family, or "" when
// unknown (in which case no addendum is added).
func modelFamily(model string) string {
	m := strings.ToLower(strings.TrimSpace(model))
	switch {
	case m == "":
		return ""
	case strings.HasPrefix(m, "gpt"), strings.HasPrefix(m, "o1"), strings.HasPrefix(m, "o3"),
		strings.HasPrefix(m, "o4"), strings.Contains(m, "openai"):
		return familyOpenAI
	case strings.HasPrefix(m, "gemini"), strings.Contains(m, "google"):
		return familyGemini
	case strings.HasPrefix(m, "claude"), strings.Contains(m, "anthropic"):
		return familyAnthropic
	default:
		return ""
	}
}

// modelPromptAddendum returns the family-specific prompt block for a model id,
// or "" when the family is unknown.
func modelPromptAddendum(model string) string {
	switch modelFamily(model) {
	case familyOpenAI:
		return openAIPromptAddendum
	case familyGemini:
		return geminiPromptAddendum
	default:
		// Anthropic (aligned with the core prompt) and unknown families get no
		// family-specific block; comment discipline now lives in the core prompt
		// for every model.
		return ""
	}
}

const openAIPromptAddendum = `<model_guidance>
- Output your final responses in GitHub-flavored Markdown: headings to structure
  longer answers, fenced code blocks for code, and ` + "`inline code`" + ` for paths,
  commands, and symbols.
- Strongly prefer the native file tools (read_file, list_directory, grep, glob,
  edit_file, apply_patch, write_file) over shelling out to cat/sed/awk/python for
  file work. Use them directly; never run apply_patch through exec_command.
  read_file, edit_file and write_file take one file per call, while one coherent
  apply_patch may span several files. Keep independent native calls in the same
  turn, and do not batch file writes into a script.
- Persist until the task is fully handled this turn: gather context, implement,
  run the validators, and report — do not stop at a partial result.
</model_guidance>`

const geminiPromptAddendum = `<model_guidance>
- Prefer the dedicated tools (read_file, grep, glob, edit_file, apply_patch) over
  equivalent shell commands; they are safer and produce cleaner diffs.
- Be concise and concrete. When you run a shell command with side effects, state
  in one short clause why it is needed.
- Use update_plan only for work with at least three meaningful dependent steps;
  skip it for simple lookups and never create one after the work is complete.
</model_guidance>`
