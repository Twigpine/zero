package specialist

import (
	"strings"
	"testing"
)

// A PLAN TASK CARRIES THE EVIDENCE RULES, because it does not inherit them.
//
// The posture's contract (agent.ZeromaxingEvidenceNotice) rides the turn loop's
// reminders, and a plan task runs as a child process that BuildArgs never hands
// --exec-profile — so it starts with the posture off and the contract never
// reaches it. Most of the substance was already in this prompt; two rules were
// not, and both are ones a task can violate straight into the plan's report.
//
// Pinned on the RULES, not the wording, so the text can be rewritten freely and
// a deletion still fails.
func TestAPlanTaskCarriesTheEvidenceRules(t *testing.T) {
	for _, grant := range [][]string{
		{"read_file", "grep"},      // read-only task
		{"read_file", "edit_file"}, // write-capable task
	} {
		prompt := planTaskSystemPrompt(grant)
		for _, required := range []string{
			// Already present, and must stay: a claim needs something read.
			"backed by something you read in this run",
			"file:line",
			"not found",
			// The two that were missing.
			"passing test is not proof",
			"what it would still pass with",
			"must come from a command you ran",
		} {
			if !strings.Contains(prompt, required) {
				t.Errorf("grant %v: the plan-task prompt no longer says %q", grant, required)
			}
		}
	}
}

// The rules must reach the CHILD, not merely exist in a helper. The manifest is
// what the child process is actually launched with.
func TestTheEvidenceRulesReachTheTaskManifest(t *testing.T) {
	manifest := planTaskManifest("explorer", "", "", []string{"read_file"})
	for _, required := range []string{"passing test is not proof", "must come from a command you ran"} {
		if !strings.Contains(manifest.SystemPrompt, required) {
			t.Errorf("the manifest handed to the child does not carry %q", required)
		}
	}
}
