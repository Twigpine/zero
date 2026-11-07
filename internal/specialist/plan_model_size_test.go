package specialist

import (
	"strconv"
	"testing"
)

// The size parser handles real ids from both kinds of provider: local ids name
// their size, cloud ids do not (and are priced anyway).
func TestModelSizeBillionsParsesRealIds(t *testing.T) {
	cases := map[string]float64{
		"gpt-oss:120b":          120,
		"gpt-oss:20b":           20,
		"qwen3.5:397b":          397, // 3.5 is a version; 397b is the size
		"mistral-large-3:675b":  675, // -3 is a version; 675b is the size
		"deepseek-coder-v2:16b": 16,  // v2 is a version; 16b is the size
		"phi3:3.8b":             3.8, // fractional billions
		"codellama:13b":         13,
		"smollm2:135m":          0.135, // millions -> billions
		"mixtral:8x7b":          7,     // MoE: the b-suffixed token
		"gpt-4o":                0,     // cloud, no size in the id
		"claude-opus-4":         0,
	}
	for id, want := range cases {
		if got := modelSizeBillions(id); got != want {
			t.Errorf("modelSizeBillions(%q) = %v, want %v", id, got, want)
		}
	}
}

// The human/router-facing size label: billions above 1, millions below, empty
// when the id names no size.
func TestModelSizeLabel(t *testing.T) {
	cases := map[string]string{
		"gpt-oss:20b":  "20B",
		"phi3:3.8b":    "3.8B",
		"smollm2:135m": "135M",
		"gpt-4o":       "",
	}
	for id, want := range cases {
		if got := ModelSizeLabel(id); got != want {
			t.Errorf("ModelSizeLabel(%q) = %q, want %q", id, got, want)
		}
	}
}

// The decency floor drops models KNOWN to be small (a task should not land on a
// toy), keeps decent ones, and keeps models of UNKNOWN size — an unknown size is
// not evidence of a small one, and cloud ids carry none.
func TestMinSizeFloorDropsToysButKeepsDecentAndUnknown(t *testing.T) {
	models := []DiscoveredModel{
		{ID: "toy:1b"}, {ID: "tiny:135m"}, {ID: "decent:7b"}, {ID: "big:70b"}, {ID: "gpt-4o"},
	}
	kept := map[string]bool{}
	for _, m := range applyMinSizeFloor(models, 7) {
		kept[m.ID] = true
	}
	if kept["toy:1b"] || kept["tiny:135m"] {
		t.Fatalf("a sub-floor toy survived the decency floor: %v", kept)
	}
	if !kept["decent:7b"] || !kept["big:70b"] {
		t.Fatalf("a decent model was dropped: %v", kept)
	}
	if !kept["gpt-4o"] {
		t.Fatal("a model of unknown size was dropped — unknown is not evidence of small")
	}
}

// If the floor would leave NO models, it yields: a plan on a small model beats a
// plan with nothing to run.
func TestMinSizeFloorFailsOpenWhenItWouldEmptyThePool(t *testing.T) {
	models := []DiscoveredModel{{ID: "small-a:1b"}, {ID: "small-b:2b"}}
	if kept := applyMinSizeFloor(models, 70); len(kept) != 2 {
		t.Fatalf("the floor emptied the pool instead of failing open: kept %d", len(kept))
	}
}

// Floor 0 (unset) changes nothing — the default for every user who has not asked
// for one.
func TestMinSizeFloorOffKeepsEverything(t *testing.T) {
	models := []DiscoveredModel{{ID: "toy:1b"}, {ID: "big:70b"}}
	if kept := applyMinSizeFloor(models, 0); len(kept) != 2 {
		t.Fatalf("floor 0 (off) dropped models: kept %d", len(kept))
	}
}

// A FREE provider tiers by SIZE, not alphabet. The ids are arranged so the
// alphabetical order (zzz > mmm > aaa) disagrees with the size order
// (1b < 7b < 70b) — a passing test proves size won.
func TestFreeProviderTiersBySizeNotAlphabet(t *testing.T) {
	models := []DiscoveredModel{
		{ID: "zzz:1b"},
		{ID: "aaa:70b"},
		{ID: "mmm:7b"},
	}
	sortModelsByCapability(models)
	got := []string{models[0].ID, models[1].ID, models[2].ID}
	want := []string{"zzz:1b", "mmm:7b", "aaa:70b"} // light -> heavy
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("free-provider order = %v, want %v (by size, not alphabet)", got, want)
		}
	}
}

// A PRICED provider is unchanged: cost is its capability proxy, and the size
// branch must not leak in. The costs and sizes are arranged to DISAGREE, so if
// size influenced a priced sort this fails.
func TestPricedProviderTiersByCostUnchanged(t *testing.T) {
	models := []DiscoveredModel{
		{ID: "big-but-cheap:70b", InputCost: 1},
		{ID: "small-but-pricey:1b", InputCost: 5},
		{ID: "mid:7b", InputCost: 3},
	}
	sortModelsByCapability(models)
	got := []string{models[0].ID, models[1].ID, models[2].ID}
	want := []string{"big-but-cheap:70b", "mid:7b", "small-but-pricey:1b"} // cost asc
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("priced order = %v, want cost order %v (size must not leak in)", got, want)
		}
	}
}

// THE SHORTLIST. A provider can list far more than a plan can sensibly use, and
// ranking the whole field is what put the SMALLEST model the provider serves on
// the cheap tier — a toy doing a scan while nineteen better ones sat unused —
// and handed the router twenty candidates to choose between.
func TestOnlyTheTopRankedModelsAreOffered(t *testing.T) {
	// Twelve free models, sized in the id: the ranking is by size, ascending.
	sizes := []string{"1b", "3b", "7b", "8b", "14b", "20b", "27b", "32b", "70b", "120b", "235b", "397b"}
	models := make([]DiscoveredModel, 0, len(sizes))
	for _, size := range sizes {
		models = append(models, DiscoveredModel{ID: "m:" + size, ToolCall: true})
	}
	ranked := rankedEligibleModels(models, ModelPreferences{})
	if len(ranked) != defaultTopRankedModels {
		t.Fatalf("offered %d models, want the top %d", len(ranked), defaultTopRankedModels)
	}
	// BOTH ENDS are kept, and the middle is what goes. The tiers read
	// cheap/balanced/strong off this list, so a cut that kept only the capable
	// end moved the cheap tier up the catalogue — on a priced provider, by an
	// order of magnitude of real money.
	if ranked[0].ID != "m:1b" || ranked[len(ranked)-1].ID != "m:397b" {
		t.Fatalf("kept %q..%q, want both ends of the ranking", ranked[0].ID, ranked[len(ranked)-1].ID)
	}
	// The middle is thinned: something between the ends must be gone, or this
	// is not a cut at all.
	for _, model := range ranked {
		if model.ID == "m:8b" || model.ID == "m:14b" {
			t.Fatalf("%s survived; the cut must thin the middle, not the ends", model.ID)
		}
	}
	if tiers := buildModelTiers(models, ModelPreferences{}); tiers.cheap != "m:1b" {
		t.Fatalf("cheap tier = %q, want the cheapest end of the ranking", tiers.cheap)
	}
}

// FAIL-OPEN, like every other narrowing here: a provider offering fewer than the
// cap keeps all of them, and a configured cap is honoured.
func TestTheShortlistNeverEmptiesTheField(t *testing.T) {
	three := []DiscoveredModel{{ID: "m:7b"}, {ID: "m:70b"}, {ID: "m:120b"}}
	if got := rankedEligibleModels(three, ModelPreferences{}); len(got) != 3 {
		t.Fatalf("a provider with three models kept %d", len(got))
	}
	if got := applyTopRank(three, 0); len(got) != 3 {
		t.Fatal("a zero cap must mean the default, never none")
	}
	if got := applyTopRank(three, -5); len(got) != 3 {
		t.Fatal("a negative cap must mean the default, never none")
	}
	// A cap of two keeps one from each end, never two from the same end: the
	// tiers need a cheap candidate and a capable one.
	if got := applyTopRank(three, 2); len(got) != 2 || got[0].ID != "m:7b" || got[1].ID != "m:120b" {
		t.Fatalf("a configured cap of 2 kept %v, want one model from each end", got)
	}
	if got := rankedEligibleModels(nil, ModelPreferences{}); len(got) != 0 {
		t.Fatalf("no models in, %d out", len(got))
	}
}

// A PIN OUTSIDE THE SHORTLIST STILL APPLIES. The user naming a model is an
// instruction, not a suggestion, and pins are validated against what the
// provider SERVES rather than against this heuristic list.
func TestAPinOutsideTheShortlistStillRoutes(t *testing.T) {
	sizes := []string{"1b", "3b", "7b", "8b", "14b", "20b", "27b", "32b", "70b", "120b", "235b", "397b"}
	models := make([]DiscoveredModel, 0, len(sizes))
	served := map[string]bool{}
	for _, size := range sizes {
		models = append(models, DiscoveredModel{ID: "m:" + size, ToolCall: true})
		served["m:"+size] = true
	}
	prefs := ModelPreferences{Scan: "m:1b"} // deliberately the smallest, off the list
	tiers := buildModelTiers(models, prefs)
	if got := tiers.modelForRoleWith(TaskRoleScan, prefs, served); got != "m:1b" {
		t.Fatalf("scan resolved to %q; a pin the provider serves must win over the shortlist", got)
	}
}

// A PRICED CATALOGUE KEEPS BOTH ENDS, AND STILL HAS A CAP.
//
// Two regressions met here. Taking a plain TAIL kept the ten DEAREST models,
// because a priced catalogue ranks by cost — the cheap tier moved from the
// cheapest model to the tenth-dearest. Exempting priced catalogues entirely was
// worse: catalogueIsPriced is an ANY test, so ONE priced model among three
// hundred disabled the cut for all three hundred, planModels.topModels went
// inert, and the router prompt lost its only bound.
func TestAPricedCatalogueKeepsBothEndsAndStaysCapped(t *testing.T) {
	var priced []DiscoveredModel
	for i := 1; i <= 300; i++ {
		priced = append(priced, DiscoveredModel{ID: "p" + strconv.Itoa(i), ToolCall: true, InputCost: float64(i)})
	}
	ranked := rankedEligibleModels(priced, ModelPreferences{TopModels: 10})
	if len(ranked) > 10 {
		t.Fatalf("300 priced models with TopModels=10 kept %d; the cap must be real", len(ranked))
	}
	tiers := buildModelTiers(priced, ModelPreferences{TopModels: 10})
	if tiers.cheap != "p1" {
		t.Fatalf("cheap tier = %q, want the cheapest model in the catalogue", tiers.cheap)
	}
	if tiers.strong != "p300" {
		t.Fatalf("strong tier = %q, want the most capable model in the catalogue", tiers.strong)
	}
	// ONE priced model must not disable the cut for a mostly-free catalogue.
	mixed := make([]DiscoveredModel, 0, 300)
	for i := 1; i <= 300; i++ {
		mixed = append(mixed, DiscoveredModel{ID: "m" + strconv.Itoa(i) + ":" + strconv.Itoa(i) + "b", ToolCall: true})
	}
	mixed[0].InputCost = 1
	if got := rankedEligibleModels(mixed, ModelPreferences{TopModels: 10}); len(got) > 10 {
		t.Fatalf("one priced model among 300 disabled the cap: kept %d", len(got))
	}
}

// TRILLIONS PARSE. "kimi-k2:1t" read as size 0 — unknown — and on a free
// provider sorted BELOW gpt-oss:20b, so the largest model on the account became
// the cheap tier every scan task was routed to, and the first thing the
// shortlist discarded.
func TestATrillionParameterModelIsNotSizeZero(t *testing.T) {
	if got := modelSizeBillions("kimi-k2:1t"); got != 1000 {
		t.Fatalf("modelSizeBillions(kimi-k2:1t) = %v, want 1000", got)
	}
	free := []DiscoveredModel{
		{ID: "gpt-oss:20b", ToolCall: true},
		{ID: "kimi-k2:1t", ToolCall: true},
		{ID: "qwen3-coder:480b", ToolCall: true},
	}
	tiers := buildModelTiers(free, ModelPreferences{})
	if tiers.cheap == "kimi-k2:1t" {
		t.Fatal("the trillion-parameter model became the cheap tier")
	}
	if tiers.strong != "kimi-k2:1t" {
		t.Fatalf("strong tier = %q, want the largest model", tiers.strong)
	}
}

// A MODEL WHOSE SIZE CANNOT BE READ IS NEVER DROPPED FOR BEING SMALL.
//
// The ranking sorts an unparseable id as size 0 — the least-capable end — so
// the first version of the shortlist deleted exactly the models whose names do
// not state a parameter count. On a real ollama-cloud account that removed
// kimi-k2.6, glm-5.2 and deepseek-v4-flash from routing while gpt-oss:20b
// survived, and the operator's own router guidance called the first two the
// strongest reasoners on the machine. Dropping a model because its name is
// uninformative is a parsing accident, not a capability judgement — and
// applyMinSizeFloor already follows exactly this fail-open rule.
func TestTheShortlistNeverDropsAnUnsizedModel(t *testing.T) {
	ids := []string{
		"gpt-oss:20b", "gpt-oss:120b", "deepseek-v3.1:671b", "qwen3-coder:480b",
		"qwen3.5:397b", "kimi-k2.6", "glm-5.2", "deepseek-v4-flash",
		"llama4:400b", "mistral-large:123b", "gemma3:27b", "qwen3:32b",
		"phi4:14b", "llama3.3:70b",
	}
	models := make([]DiscoveredModel, 0, len(ids))
	for _, id := range ids {
		models = append(models, DiscoveredModel{ID: id, ToolCall: true})
	}
	kept := map[string]bool{}
	for _, model := range rankedEligibleModels(models, ModelPreferences{}) {
		kept[model.ID] = true
	}
	for _, unsized := range []string{"kimi-k2.6", "glm-5.2", "deepseek-v4-flash"} {
		if !kept[unsized] {
			t.Errorf("%s was dropped for having no size in its id", unsized)
		}
	}
	// The cut still applies to models we CAN compare: the smallest sized one goes.
	if kept["phi4:14b"] {
		t.Error("phi4:14b survived; the shortlist must still cut the smallest sized models")
	}
	// And the cap is still real: the catalogue is larger than it.
	if len(kept) > defaultTopRankedModels {
		t.Fatalf("kept %d models, want at most the cap of %d", len(kept), defaultTopRankedModels)
	}
}

// VERSION TEXT IS NOT A PARAMETER COUNT.
//
// The suffix boundary was [^a-z], which a DIGIT satisfies — so
// "deepseek-r1t2-chimera" matched "1t" followed by "2" and reported a
// TRILLION-parameter model, which on a free provider would have made it the
// strong tier every judgement task was routed to. The b/m suffixes always had
// the same hole ("r1b2-x" read as 1B); adding t is what made it reachable on a
// real id, so the boundary is fixed for all three.
func TestVersionTextIsNotReadAsASize(t *testing.T) {
	for _, id := range []string{
		"deepseek-r1t2-chimera", // the reported case
		"r1b2-x",                // the same hole on b
		"m3m4-preview",          // and on m
		"qwen2t5-experimental",
	} {
		if got := modelSizeBillions(id); got != 0 {
			t.Errorf("modelSizeBillions(%q) = %v, want 0 — that is version text, not a size", id, got)
		}
	}
	// Real ids still parse, including at a boundary and mid-id.
	for id, want := range map[string]float64{
		"kimi-k2:1t":         1000,
		"gpt-oss:20b":        20,
		"qwen3.5:397b":       397,
		"gpt-oss:120b-cloud": 120,
		"x:350m":             0.35,
	} {
		if got := modelSizeBillions(id); got != want {
			t.Errorf("modelSizeBillions(%q) = %v, want %v", id, got, want)
		}
	}
}
