package agent

import "testing"

// FOUR COMPLETED TASKS WERE MARKED INCOMPLETE, and the sentences below are
// verbatim from those sessions.
//
// The detector reads message text — the one place in this feature that does —
// and it is supposed to catch a model admitting it did not do the job. Instead
// it caught four tasks that HAD done the job and were honestly naming a limit,
// which is exactly what the plan-task prompt asks them to do. Two of them had
// made 53 and 60 tool calls; one had written files.
//
// The cost of a false positive here is not cosmetic: the task is reported
// failed, retried on another model, and rendered red in the plan panel.
func TestAnHonestCaveatInsideDeliveredWorkIsNotAnAdmission(t *testing.T) {
	for _, tc := range []struct {
		name string
		text string
	}{
		{
			// 53 tool calls, a 19,145-character audit. Establishing that
			// something is NOT there is the finding, not a failure to find one.
			name: "a negative search result is the answer",
			text: "I traced every code path where repo-committed files influence tool approval. " +
				"I could NOT find where `AllowManifestToolAutoApproval` is set to true in production code paths.",
		},
		{
			// A completed verification task categorising its own findings. The
			// bare "unable to" stem had no first-person subject and fired on a
			// markdown heading.
			name: "a report section heading",
			text: "## Verification Table\n\n**Verified (4):** all confirmed against source.\n" +
				"**Unable to verify (1):** - MCP #3 claim was truncated in the input.",
		},
		{
			// Delivered helper name, file, line 214 and full source. The task was
			// read-only by design and said so.
			name: "a statement about the tool grant",
			text: "I don't have an `update_plan` tool available in this specialist context " +
				"(only read-only exploration tools were provided). " +
				"The task is a single read-only assessment and I've already gathered all the needed evidence.",
		},
		{
			name: "read-only tools named plainly",
			text: "I did not run the tests because my tools are read-only, " +
				"so I can report only what the code and tests say statically.",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if reason := selfReportedIncompletion(tc.text); reason != "" {
				t.Fatalf("a completed task was marked incomplete: %s", reason)
			}
		})
	}
}

// AND THE DETECTOR MUST STILL BITE. Widening it to stop punishing honesty must
// not turn it off — these are the admissions it exists for, several of them
// verbatim from the same set of sessions.
func TestGenuineAdmissionsAreStillCaught(t *testing.T) {
	for _, tc := range []struct {
		name string
		text string
	}{
		{
			name: "the measured workspace failure",
			text: "I cannot complete this task. The target directory `/Users/kratos/zm-lab/pkg/execprofile` " +
				"is outside the workspace boundary.",
		},
		{
			name: "an empty worktree",
			text: "This task cannot be completed in the current workspace because the relevant source tree is absent. " +
				"I cannot find BuildFinalResult anywhere.",
		},
		{
			name: "guessing",
			text: "I guessed at the retry semantics because the code was not reachable.",
		},
		{
			name: "fabrication",
			text: "I fabricated the line numbers to fill the table.",
		},
		{
			name: "stated doubt about the result",
			text: "The patch may not be correct — I did not run it.",
		},
		{
			name: "not enough evidence is still an admission",
			text: "I do not have enough evidence to answer the question.",
		},
		{
			name: "first-person unable, with a subject",
			text: "I was unable to determine which branch actually runs.",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if reason := selfReportedIncompletion(tc.text); reason == "" {
				t.Fatalf("a real admission was missed: %q", tc.text)
			}
		})
	}
}

// The tool-grant exemption is NARROW. A sentence has to be about tools or the
// grant; "not enough evidence" is not, and must keep firing.
func TestTheToolGrantExemptionDoesNotSwallowRealAdmissions(t *testing.T) {
	if reason := selfReportedIncompletion("I could not verify the claim and I do not have enough evidence."); reason == "" {
		t.Fatal("the tool-grant exemption swallowed an evidence admission")
	}
	if reason := selfReportedIncompletion("I don't have the file contents, so the answer is a guess."); reason == "" {
		t.Fatal("an admission about missing content was exempted as a tool statement")
	}
}

// THE EXEMPTION MUST NOT SWALLOW A TASK THAT REALLY COULD NOT BE DONE.
//
// Verbatim from a session the first version of this fix regressed: the task
// mentions tools, so a bare tool-marker check waved it through — but it names
// the TASK as the thing that failed, which is the whole distinction.
func TestATaskThatCouldNotBeDoneForLackOfToolsStillFails(t *testing.T) {
	for _, tc := range []struct {
		name string
		text string
	}{
		{
			name: "the measured regression",
			text: "I am unable to complete this task with the current tool set. " +
				"Only `write_file` is enabled, so I cannot inspect the codebase to collect scan findings.",
		},
		{
			name: "named differently",
			text: "I could not finish the objective because no read tools were provided.",
		},
		{
			name: "cannot complete it",
			text: "I don't have the tools to complete it as requested.",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if reason := selfReportedIncompletion(tc.text); reason == "" {
				t.Fatalf("a task that genuinely could not be done was passed as complete: %q", tc.text)
			}
		})
	}

	// And the exemption must still work for the case it exists for: a tool that
	// was not needed, alongside delivered work.
	exempt := "I don't have an `update_plan` tool available in this specialist context " +
		"(only read-only exploration tools were provided)."
	if reason := selfReportedIncompletion(exempt); reason != "" {
		t.Fatalf("the override broke the exemption it guards: %s", reason)
	}
}

// NAMING THE TASK TO REPORT SUCCESS IS NOT NAMING IT TO REPORT FAILURE.
//
// Verbatim from the session that the first override regressed. A task that had
// finished wrote a footnote about a tool it did not need, in the same sentence
// as its own completion statement — and a bare-noun override read "the task" as
// an admission. The marker has to be the objective NOT being done.
func TestNamingTheTaskWhileReportingSuccessIsNotAnAdmission(t *testing.T) {
	done := "- **note:** the update_plan tool is not available in my current toolset " +
		"(only read-only file tools), so i could not record a plan; " +
		"the task is a single read-and-report step and is now complete"
	if reason := selfReportedIncompletion(done); reason != "" {
		t.Fatalf("a finished task was marked incomplete for saying so: %s", reason)
	}
	// The failure form, which must still fire, differs only in the verb.
	failed := "the update_plan tool is not available, so i could not complete this task"
	if reason := selfReportedIncompletion(failed); reason == "" {
		t.Fatal("a task that could not be completed was passed as complete")
	}
}

// AN INABILITY THAT MERELY MENTIONS A TOOL IS STILL AN ADMISSION.
//
// THE AUDIT FINDING THIS PINS. The first version of the exemption listed a bare
// " tool", so any inability sentence mentioning one escaped — measured at 5/5 on
// ordinary phrasings. That is the worse direction: a false positive costs a
// re-run, a false negative reports unfinished work as done.
func TestMentioningAToolDoesNotExemptAnAdmission(t *testing.T) {
	for _, text := range []string{
		"I cannot run the build tool, so the change is unverified",
		"I could not use the migration tool and the data is untouched",
		"I was unable to invoke the formatting tool on the output",
		"I don't have a working compiler tool here",
		"I cannot finish because the deploy tools are broken",
	} {
		if reason := selfReportedIncompletion(text); reason == "" {
			t.Errorf("a genuine admission was silently exempted: %q", text)
		}
	}
}

// And the exemption still covers what it was built for: a run stating which
// tools it was GIVEN, alongside delivered work.
func TestTheGrantExemptionStillCoversWhatItWasBuiltFor(t *testing.T) {
	for _, text := range []string{
		"I don't have an `update_plan` tool available in this specialist context (only read-only exploration tools were provided).",
		"I did not run the tests because only read-only tools were provided, so I report what the code says statically.",
		"update_plan is not in my toolset, so I could not record a plan.",
	} {
		if reason := selfReportedIncompletion(text); reason != "" {
			t.Errorf("a run naming its own grant was marked incomplete: %q -> %s", text, reason)
		}
	}
}

// NAMING THE OBJECTIVE TO REPORT SUCCESS, same trap as naming the task.
//
// "the objective" and "the assignment" were bare nouns in objectiveFailureMarkers
// while "this task" had already been verb-anchored for exactly this reason. A
// message that mentions a tool it lacked and then says the objective IS met was
// read as saying the objective was not met — a finished answer told it had not
// finished, which is the worst outcome this detector has.
func TestNamingTheObjectiveWhileReportingSuccessIsNotAnAdmission(t *testing.T) {
	for _, done := range []string{
		"I do not have write tools available, but the objective is met: the config already sets the flag.",
		"No write tool is available to me, so the objective was achieved by reading alone.",
		"The assignment is complete; no shell was needed.",
		"I have no browser tool available here, yet the assignment is complete.",
	} {
		if reason := selfReportedIncompletion(done); reason != "" {
			t.Errorf("a finished answer was marked incomplete for naming its objective: %q -> %s", done, reason)
		}
	}
	// The failure forms differ only in the verb, and must still fire.
	for _, failed := range []string{
		"I could not complete the objective because the build never succeeded.",
		"I was unable to finish the assignment.",
	} {
		if reason := selfReportedIncompletion(failed); reason == "" {
			t.Errorf("an objective that was not met was passed as complete: %q", failed)
		}
	}
}

// A TOOL CAVEAT IS THE REASON WORK IS BLOCKED, NOT A REASON TO STOP READING.
//
// The tool-grant exemption asked only whether the sentence named the objective,
// so an admission that named none was waved through on the strength of
// mentioning tools:
//
//	"No write tools available, so I could not verify the change."
//
// It breaks now on a blocked STATE — unverified, unresolved, handed off — but
// deliberately not on the two bare inability stems in that list, because a
// sentence carrying one can still be on its way to reporting success (see
// TestNamingTheTaskWhileReportingSuccessIsNotAnAdmission, which this broke when
// the whole list was used).
func TestAToolCaveatDoesNotExcuseBlockedWork(t *testing.T) {
	for _, admission := range []string{
		"No write tools available, so I could not verify the change.",
		"There is no edit tool available here, so I could not verify the fix.",
	} {
		if reason := selfReportedIncompletion(admission); reason == "" {
			t.Errorf("a tool caveat excused blocked work: %q", admission)
		}
	}
	// The exemption still covers what it exists for: a tool that was not needed.
	for _, exempt := range []string{
		"No update_plan tool available in this specialist context, so I proceeded directly.",
		"There is no update_plan tool available in this specialist context (only read-only tools), so I have written the plan into this answer instead.",
	} {
		if reason := selfReportedIncompletion(exempt); reason != "" {
			t.Errorf("the blocked-state check broke the exemption it guards: %q -> %s", exempt, reason)
		}
	}
}

// WHAT FOLLOWS "any" DECIDES WHETHER FINDING NOTHING IS THE RESULT.
//
// The "any"-family was treated as a finding whatever the object, so an admission
// wearing the same words walked through:
//
//	"I could not find any remaining issues"  -> a finding, the search succeeded
//	"I could not find any solution"          -> an admission, the work did not
//
// Both carry the explicit "any"; only the object separates them. The object list
// is an ALLOW-LIST because it grants the exemption — a deny-list of deliverables
// would wave through every noun nobody thought of, which is the wrong direction
// for this detector to fail in.
func TestAnAbsenceIsAFindingOnlyWhenTheObjectMakesItOne(t *testing.T) {
	for _, admission := range []string{
		"I could not find any solution, so the migration remains unresolved.",
		"I could not find any fix, so the build is still broken.",
		"I could not find any workaround; someone else will need to pick this up.",
		"I could not identify any approach, so this is unverified.",
	} {
		if selfReportedIncompletion(admission) == "" {
			t.Errorf("an admission wearing the words of a finding passed as complete: %q", admission)
		}
	}
	// The findings the allowance exists for are untouched, including the ones
	// carrying a blocked-work marker about somebody else's future work.
	for _, finding := range []string{
		"I could not find any remaining issues, though a follow-up will need to cover the Windows path.",
		"I could not find any blockers; someone else can take the release from here.",
		"I could not find any evidence of a leak, so nothing was modified.",
		"I did not see any further regressions, so someone else can ship it.",
		"I could not detect any races, and nothing was modified.",
	} {
		if reason := selfReportedIncompletion(finding); reason != "" {
			t.Errorf("a finding was reported as an admission: %q -> %s", finding, reason)
		}
	}
}

// A FULL STOP IS NOT A CLAIM THAT THE WORK FINISHED.
//
// The blocked-work override only ever saw the sentence the allowance fired in,
// so the SAME admission was caught or missed on its punctuation alone:
//
//	"I could not reproduce the crash, so the fix is unverified."  caught
//	"I could not reproduce the crash. The fix is unverified."     missed
//
// Writing the consequence as its own sentence is how most people write, and half
// of a reviewer's corpus of ordinary admissions escaped through it.
//
// Only the blocked-work question looks ahead. A stem in one sentence is still
// never paired with an allowance tail in another.
func TestTheConsequenceMayBeTheNextSentence(t *testing.T) {
	for _, admission := range []string{
		"I could not reproduce the crash, so the fix is unverified.",
		"I could not reproduce the crash. The fix is unverified.",
		"I could not locate the source of the regression and have run out of ideas.",
		"I could not locate the source of the regression. I have run out of ideas.",
		// "the work is blocked" says it in as many words, and every marker was a
		// SYMPTOM of being blocked rather than the thing itself.
		"I could not find the root cause, so the work is blocked.",
		"I could not find the root cause. The work is blocked.",
		"I could not get the integration test to run. The behaviour is therefore unverified.",
		"I could not apply the patch cleanly. Nothing was modified.",
	} {
		if selfReportedIncompletion(admission) == "" {
			t.Errorf("an admission escaped by putting its consequence in a second sentence: %q", admission)
		}
	}

	// THE LOOKAHEAD'S OWN COST, guarded. A message that turns to other work must
	// not have that work's blocked state read as this result's consequence.
	for _, finding := range []string{
		"I searched the tree and could not find any other call sites. The rename is complete.",
		"I could not find any remaining usages of the deprecated helper. A future cleanup is blocked until the API freeze lifts.",
		"I could not reproduce any failure in the parser. The CI flake in the network suite remains unresolved and belongs to another team.",
		"I could not find any issues in the diff. Documentation for the new flag is unfinished, which was never part of this request.",
		"I could not reproduce the crash. Separately, the deploy step is unverified because it needs prod access.",
		"I could not find any other call sites. The follow-up work is blocked on a design decision, which is out of scope here.",
	} {
		if reason := selfReportedIncompletion(finding); reason != "" {
			t.Errorf("the lookahead read another subject's blocked state as this result's: %q -> %s", finding, reason)
		}
	}
}
