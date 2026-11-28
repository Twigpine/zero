package tools

import (
	"encoding/json"
	"strings"
)

const humanOutcomePreviewBytes = 8 * 1024

type serializedToolOutcome struct {
	ModelView   string             `json:"modelView,omitempty"`
	HumanView   Display            `json:"humanView,omitempty"`
	Artifact    *ToolArtifact      `json:"artifact,omitempty"`
	Diagnostics OutcomeDiagnostics `json:"diagnostics,omitempty"`
	Finalized   bool               `json:"finalized,omitempty"`
}

// MarshalJSON preserves the private finalized marker without making outcome
// state mutable to callers.
func (outcome ToolOutcome) MarshalJSON() ([]byte, error) {
	return json.Marshal(serializedToolOutcome{
		ModelView:   outcome.ModelView,
		HumanView:   outcome.HumanView,
		Artifact:    outcome.Artifact,
		Diagnostics: outcome.Diagnostics,
		Finalized:   outcome.finalized,
	})
}

// UnmarshalJSON restores the finalized marker used to select canonical views.
func (outcome *ToolOutcome) UnmarshalJSON(data []byte) error {
	var serialized serializedToolOutcome
	if err := json.Unmarshal(data, &serialized); err != nil {
		return err
	}
	outcome.ModelView = serialized.ModelView
	outcome.HumanView = serialized.HumanView
	outcome.Artifact = serialized.Artifact
	outcome.Diagnostics = serialized.Diagnostics
	outcome.finalized = serialized.Finalized
	return nil
}

// finalizeToolOutcome is the single seam between tool execution and its three
// consumers: provider context, human presentation, and recoverable artifacts.
// boundaryOutput must already be redacted. It is the text seen immediately
// before command reduction and semantic budgeting.
func finalizeToolOutcome(result Result, boundaryOutput string) Result {
	// PROMOTED HERE, at the one seam every tool result crosses, rather than at
	// each construction site. addSandboxMeta already carries the plan's notices
	// into metadata for both the bash and the exec_command paths, and any future
	// command tool that calls it gets the same treatment for free. Setting the
	// field at the call sites instead would be a third hand-maintained projection
	// of the same fact, which is exactly how the disclosure went missing from the
	// generic execution adapter in the first place.
	if len(result.EnforcementNotices) == 0 {
		// DERIVED FROM APPLIED STATE, NOT FROM THE PLAN. addSandboxMeta writes the
		// plan's notices at plan time, before anything runs, so promoting them
		// unconditionally claims a token trade for a command that may never have
		// started. The execution outcome is the thing that knows, and it applies
		// the same launched-and-planned rule hooks and plugins use.
		//
		// The metadata stays as diagnostics either way: it records what was
		// planned, which is still worth having.
		if result.ExecutionOutcome != nil {
			result.EnforcementNotices = result.ExecutionOutcome.AppliedEnforcementNotices()
		} else if notices := strings.TrimSpace(result.Meta[sandboxNoticesMeta]); notices != "" {
			result.EnforcementNotices = strings.Split(notices, "\n")
		}
	}
	previous := result.Outcome
	human := result.Display
	if human.Preview == "" && result.Meta["command_output_reduced"] == "true" {
		human.Preview = boundedHumanOutcomePreview(boundaryOutput, result.Meta["spill_path"])
	}
	// A tool-provided success preview (for example, a prospective file diff)
	// must not hide the actual error. Command reduction is different: its
	// preview is the redacted execution output containing that same failure plus
	// the evidence omitted from the model view.
	if result.Status == StatusError && result.Meta["command_output_reduced"] != "true" {
		human.Preview = ""
	}

	originalBytes := len(boundaryOutput)
	originalTokens := estimateOutputTokens(boundaryOutput)
	if previous.Finalized() {
		originalBytes = previous.Diagnostics.OriginalBytes
		originalTokens = previous.Diagnostics.EstimatedOriginalTokens
	}
	// MEASURE WHAT THE MODEL ACTUALLY RECEIVES.
	//
	// The enforcement notices are prepended to the model view on the way out
	// (agent.ToolResult.ModelOutput), so a result carrying them costs more context
	// than result.Output alone. Measuring the bare output undercounts every
	// disclosed call and hands the budget a figure the model never saw.
	//
	// ModelView below stays the bare output on purpose: ModelOutput prepends the
	// notices itself, so storing them here would send them twice.
	canonicalModelOutput := WithEnforcementNotices(result.Output, result.EnforcementNotices)
	modelBytes := len(canonicalModelOutput)
	modelTokens := estimateOutputTokens(canonicalModelOutput)

	var artifact *ToolArtifact
	if previous.Finalized() {
		artifact = previous.Artifact
	}
	if path := strings.TrimSpace(result.Meta["spill_path"]); path != "" {
		if artifact == nil || artifact.Path != path {
			artifact = &ToolArtifact{
				Path:               path,
				CompleteAtBoundary: result.Meta["command_output_reduced"] == "true" || result.Meta[outputBudgetSpillCreatedMeta] == "true",
			}
		}
	}

	result.Display = human
	result.Outcome = ToolOutcome{
		ModelView: result.Output,
		HumanView: human,
		Artifact:  artifact,
		Diagnostics: OutcomeDiagnostics{
			Category:                result.Meta[outputBudgetCategoryMeta],
			OriginalBytes:           originalBytes,
			ModelBytes:              modelBytes,
			EstimatedOriginalTokens: originalTokens,
			EstimatedModelTokens:    modelTokens,
			Truncated:               result.Truncated,
			Redacted:                result.Redacted,
			Reason:                  result.Meta["truncation_reason"],
		},
		finalized: true,
	}
	return result
}

func boundedHumanOutcomePreview(output string, artifactPath string) string {
	if len(output) <= humanOutcomePreviewBytes {
		return output
	}
	marker := "\n[zero] human preview shortened"
	if strings.TrimSpace(artifactPath) != "" {
		marker += "; exact output: " + artifactPath
	}
	marker += "\n"
	contentBudget := max(0, humanOutcomePreviewBytes-len(marker))
	return utf8Prefix(output, contentBudget*3/5) + marker + utf8Suffix(output, contentBudget*2/5)
}
