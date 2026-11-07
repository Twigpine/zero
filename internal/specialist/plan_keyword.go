package specialist

import (
	"fmt"
	"strings"
)

// The heavy path may be required to have been ASKED FOR, in the user's own words.
//
// THE THREAT. Once the posture is on, orchestrate exists for the rest of the
// session — and everything the model reads sits in the same context as the
// user's instructions: file contents, a PR comment, a web result, MCP server
// output, a scheduled task's payload. Imperative language in any of them reads
// to a model exactly like an instruction, and this is the tool that spends real
// money and real time. The posture gate stops orchestrate EXISTING while the
// posture is off; it does nothing once it is on.
//
// WHY A HARD CHECK RATHER THAN A PROMPT RULE. A prompt rule is advice to the
// same model the injected text is talking to. This is an admission check: it
// holds regardless of what the model was told or believed.
//
// DEFAULT OFF, and that is a real trade rather than an oversight. On by default
// would refuse a plan for every existing user whose phrasing does not happen to
// match — including a benchmark prompt that says "you are being measured, one
// session, one pass" and means it. This codebase already made that call once,
// for auto_assign: "On means every existing plan silently starts running on
// models the user never chose." A gate that silently starts refusing work is the
// same shape. So it is opt-in, and the honest cost of that is that it protects
// only the sessions that enable it.

// planKeywords are the phrases that count as asking for a plan. Matched as
// substrings of the lowercased message, deliberately: a user who writes "please
// run a plan for this" or "can you fan out over the packages" has asked, and
// demanding an exact form would make the gate a password prompt.
var planKeywords = []string{
	"run a plan",
	"use a plan",
	"a plan for",
	"fan out",
	"fan-out",
	"in parallel",
	"use workflow",
	"use a workflow",
	"run the workflow",
	"orchestrate",
	"multi-agent",
	"sub-agents",
	"subagents",
}

// planRequestedByUser reports whether the turn's own user text asks for a plan.
//
// THE RAW MESSAGE ONLY, never the accumulated context — the whole point is to
// distinguish what the user said from what the model read. An empty message is
// NOT a request: a caller that supplies no user text cannot demonstrate one, and
// a gate that treated absence as consent would be off in exactly the headless
// and scheduled paths most exposed to untrusted payloads.
func planRequestedByUser(message string) bool {
	lowered := strings.ToLower(message)
	for _, keyword := range planKeywords {
		if strings.Contains(lowered, keyword) {
			return true
		}
	}
	return false
}

// planKeywordRefusal is what the model is told, and it names the remedy: the
// point is a user who wants a plan gets one by saying so, not that plans become
// unavailable.
func planKeywordRefusal() error {
	return fmt.Errorf(
		"a multi-agent plan has to be asked for in your own words — say something like "+
			"%q or %q and it will run. This session requires it because plans cost real time and money, "+
			"and instructions can arrive from files, comments and tool output as easily as from you",
		"run a plan for ...", "fan out and investigate ...")
}
