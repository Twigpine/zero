package specialist

import "testing"

// Identity is stable, order-independent for the set-valued fields, and moves
// with every field that changes what the task does.
func TestTaskIdentityIsStableAndCoversWhatVaries(t *testing.T) {
	base := Task{ID: "a", Prompt: "do it", Model: "m1",
		Tools: []string{"read_file", "grep"}, DependsOn: []string{"x", "y"}}

	// Tool and dependency ORDER must not matter — both are sets.
	reordered := Task{ID: "a", Prompt: "do it", Model: "m1",
		Tools: []string{"grep", "read_file"}, DependsOn: []string{"y", "x"}}
	if taskIdentity(base) != taskIdentity(reordered) {
		t.Fatal("identity changed when only tool/dep order changed — the grant is a set")
	}

	// Every field that changes execution changes identity.
	for name, mutate := range map[string]func(Task) Task{
		"prompt": func(k Task) Task { k.Prompt = "do it differently"; return k },
		"model":  func(k Task) Task { k.Model = "m2"; return k },
		"tools":  func(k Task) Task { k.Tools = []string{"read_file"}; return k },
		"deps":   func(k Task) Task { k.DependsOn = []string{"x"}; return k },
	} {
		if taskIdentity(mutate(base)) == taskIdentity(base) {
			t.Fatalf("changing %s did not change identity — a resume would replay a stale result", name)
		}
	}

	// Fields with no execution meaning must NOT change identity, or an unrelated
	// relabel forces the whole plan to re-run.
	idChanged := base
	idChanged.ID = "b"
	phaseChanged := base
	phaseChanged.Phase = "later"
	if taskIdentity(idChanged) != taskIdentity(base) {
		t.Fatal("changing the id changed identity — id is the match key, not the fingerprint")
	}
	if taskIdentity(phaseChanged) != taskIdentity(base) {
		t.Fatal("changing the phase changed identity — phase has no execution semantics")
	}
}

// Length delimiting must stop adjacent field values from aliasing into one
// fingerprint. The per-field tags are not enough on their own: a value can end
// with the NEXT field's tag and shift a character across the boundary. Here
// prompt "xmodel"+model "y" and prompt "x"+model "modely" write the identical
// tag-and-value byte stream unless each value is length-prefixed.
func TestTaskIdentityDoesNotAliasAcrossFieldBoundaries(t *testing.T) {
	shifted := taskIdentity(Task{Prompt: "xmodel", Model: "y"})
	other := taskIdentity(Task{Prompt: "x", Model: "modely"})
	if shifted == other {
		t.Fatal("a character shifted across the prompt/model boundary produced one identity — values are not length-delimited")
	}
	// The same hazard within the repeated tool tag: "atool"+"b" vs "a"+"toolb".
	if taskIdentity(Task{Tools: []string{"atool", "b"}}) == taskIdentity(Task{Tools: []string{"a", "toolb"}}) {
		t.Fatal("two different tool sets produced one identity — tool values are not length-delimited")
	}
	// And a tool named like a dependency must not let the two fields trade values.
	if taskIdentity(Task{Tools: []string{"x"}}) == taskIdentity(Task{DependsOn: []string{"x"}}) {
		t.Fatal("a tool and a dependency with the same name produced one identity")
	}
}
