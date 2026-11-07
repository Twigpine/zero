package specialist

import (
	"embed"
	"encoding/json"
	"path/filepath"
	"strings"
)

// The bundled plan: one worked example, shipped in the binary.
//
// A plan format nobody has seen an example of is a format nobody writes. This
// is the shape working — a fan-out of independent searches, a synthesis, and a
// verification step — available immediately with /plans show research, and
// runnable with /plans run research.
//
// IT ENCODES THE HOUSE STYLE ON PURPOSE. The searches are deliberately
// independent because one search angle finds one kind of thing; the verify task
// is told to REFUTE and to default to refuted when uncertain, because a
// verifier that sets out to agree always does. Those are the two habits worth
// teaching, and a starter plan that merely demonstrated the JSON would teach
// neither.
//
// IT IS SHADOWED, never authoritative. A user or project plan of the same name
// wins, so shipping this can never override something someone wrote.

//go:embed plans/*.json
var builtinPlanFS embed.FS

// builtinPlans returns the plans compiled into the binary.
//
// A parse failure here is a BUILD defect, not a user's problem — these files
// ship with the binary — so an unreadable one is dropped rather than reported
// as a broken plan the user might go looking for. The test that parses every
// bundled plan is what stops one shipping broken.
func builtinPlans() []SavedPlan {
	entries, err := builtinPlanFS.ReadDir("plans")
	if err != nil {
		return nil
	}
	var out []SavedPlan
	for _, entry := range entries {
		if entry.IsDir() || !strings.EqualFold(filepath.Ext(entry.Name()), planFileExt) {
			continue
		}
		name := strings.TrimSuffix(entry.Name(), filepath.Ext(entry.Name()))
		if !validPlanName(name) {
			continue
		}
		raw, err := builtinPlanFS.ReadFile("plans/" + entry.Name())
		if err != nil {
			continue
		}
		var args map[string]any
		if err := json.Unmarshal(raw, &args); err != nil {
			continue
		}
		out = append(out, SavedPlan{
			Name:        name,
			Description: planString(args, "description"),
			TaskCount:   savedTaskCount(args),
			Path:        "(bundled)",
			Scope:       PlanScopeBuiltin,
			Args:        args,
		})
	}
	return out
}
