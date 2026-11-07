package specialist

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Parameters for a saved plan: the difference between a plan you re-run and one
// you re-author.
//
// A saved plan is stored args and re-admitted through ParsePlan, which makes it
// exactly repeatable and completely fixed. "Audit internal/tui" cannot be run
// against internal/cli without opening the file and editing the prompt of every
// task — so in practice a plan gets copied per target and the copies drift.
//
// WHAT MAY BE SUBSTITUTED IS DELIBERATELY NARROW. Only prose the model reads:
// each task's prompt, and the plan's description. Not `tools`, which is the
// authority grant; not `id` or `depends_on`, which are the graph; not `model` or
// the budget. A parameter that could reach the grant would turn "run the sweep
// plan with scope=x" into a way to widen what the plan may do, and a parameter
// that could reach an id would let one argument silently detach a dependency
// edge. Those are fixed when the plan is saved and reviewed, and they stay fixed.
//
// Expansion happens BEFORE ParsePlan, so validation, tool-grant narrowing, depth
// and budget checks all run against the plan that will actually execute rather
// than against a template of it.

// planParamPattern matches ${name} with no nesting and no expressions. A
// template language here would be a second thing to validate and a second place
// for authority to leak; substitution is all this needs to be.
var planParamPattern = regexp.MustCompile(`\$\{([a-zA-Z][a-zA-Z0-9_-]*)\}`)

// planParamFields are the keys whose values a parameter may reach. Everything
// absent from this list is structure or authority — see the file comment.
var planParamTaskFields = []string{"prompt"}

// expandPlanParams substitutes params into a saved plan's args.
//
// FAILS CLOSED IN BOTH DIRECTIONS. A placeholder with no argument is an error
// rather than a prompt containing a literal "${scope}", which reads to the model
// as an instruction about a directory that does not exist. An argument with no
// placeholder is an error too: it is almost always a typo, and silently ignoring
// it runs the plan against the wrong target while reporting success.
func expandPlanParams(args map[string]any, params map[string]string) (map[string]any, error) {
	declared := planParamPlaceholders(args)
	if len(declared) == 0 && len(params) == 0 {
		return args, nil
	}

	var missing []string
	for _, name := range declared {
		if _, ok := params[name]; !ok {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("this plan needs %s: supply %s in params",
			pluralParams(missing), strings.Join(quoteAll(missing), ", "))
	}

	known := map[string]bool{}
	for _, name := range declared {
		known[name] = true
	}
	var unused []string
	for name := range params {
		if !known[name] {
			unused = append(unused, name)
		}
	}
	if len(unused) > 0 {
		sort.Strings(unused)
		return nil, fmt.Errorf("this plan has no %s: %s appears nowhere in it%s",
			pluralParams(unused), strings.Join(quoteAll(unused), ", "), declaredHint(declared))
	}

	return substitutePlanParams(args, params), nil
}

// PlanParams lists the parameters a stored plan declares.
//
// Exported so a caller that runs a saved plan BY NAME — the /plans command — can
// ask what it needs before dispatching a turn. Without it the only way to learn
// a plan takes a subject is to run it: expandPlanParams refuses at admission,
// which is correct but arrives one turn and one model call too late, and the
// bundled research plan otherwise spends five child agents researching the
// literal placeholder.
func PlanParams(args map[string]any) []string {
	return planParamPlaceholders(args)
}

// planParamPlaceholders lists every ${name} the plan uses, deduplicated and
// sorted so an error message reads the same way twice.
//
// Scanned ONLY where substitution is allowed. A ${name} inside a tool grant is
// not a parameter this plan takes; it is a literal that will fail tool
// validation, which is the correct outcome and a clearer error than anything
// this could say about it.
func planParamPlaceholders(args map[string]any) []string {
	found := map[string]bool{}
	collect := func(text string) {
		for _, match := range planParamPattern.FindAllStringSubmatch(text, -1) {
			found[match[1]] = true
		}
	}
	collect(planString(args, "description"))
	if tasks, ok := args["tasks"].([]any); ok {
		for _, raw := range tasks {
			entry, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			for _, field := range planParamTaskFields {
				collect(planString(entry, field))
			}
		}
	}
	out := make([]string, 0, len(found))
	for name := range found {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// substitutePlanParams returns a COPY with the substitutions applied. The stored
// plan is never mutated: LoadPlans caches by path, and rewriting a cached plan
// would leave the next run of it carrying the previous run's arguments.
func substitutePlanParams(args map[string]any, params map[string]string) map[string]any {
	replace := func(text string) string {
		return planParamPattern.ReplaceAllStringFunc(text, func(match string) string {
			name := planParamPattern.FindStringSubmatch(match)[1]
			if value, ok := params[name]; ok {
				return value
			}
			return match
		})
	}

	out := make(map[string]any, len(args))
	for key, value := range args {
		out[key] = value
	}
	if description := planString(args, "description"); description != "" {
		out["description"] = replace(description)
	}
	tasks, ok := args["tasks"].([]any)
	if !ok {
		return out
	}
	expanded := make([]any, 0, len(tasks))
	for _, raw := range tasks {
		entry, ok := raw.(map[string]any)
		if !ok {
			expanded = append(expanded, raw)
			continue
		}
		copied := make(map[string]any, len(entry))
		for key, value := range entry {
			copied[key] = value
		}
		for _, field := range planParamTaskFields {
			if text := planString(entry, field); text != "" {
				copied[field] = replace(text)
			}
		}
		expanded = append(expanded, copied)
	}
	out["tasks"] = expanded
	return out
}

// planParamsFromArgs reads the caller's params map, rejecting a shape that would
// otherwise substitute something like "map[]" into a prompt.
func planParamsFromArgs(args map[string]any) (map[string]string, error) {
	raw, present := args["params"]
	if !present {
		return nil, nil
	}
	entries, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("params must be an object of name/value pairs")
	}
	out := make(map[string]string, len(entries))
	for name, value := range entries {
		text, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("params[%q] must be a string; a plan parameter is substituted into a prompt as written", name)
		}
		if strings.TrimSpace(name) == "" {
			return nil, fmt.Errorf("a plan parameter must be named")
		}
		out[name] = text
	}
	return out, nil
}

func quoteAll(names []string) []string {
	out := make([]string, 0, len(names))
	for _, name := range names {
		out = append(out, `"`+name+`"`)
	}
	return out
}

func pluralParams(names []string) string {
	if len(names) == 1 {
		return "parameter"
	}
	return "parameters"
}

func declaredHint(declared []string) string {
	if len(declared) == 0 {
		return " (it takes none)"
	}
	return " (it takes " + strings.Join(quoteAll(declared), ", ") + ")"
}
