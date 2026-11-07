package config

import (
	"fmt"
	"strings"
)

// The zeromaxing plan-size tiers.
//
// A plan's task count was bounded by ONE hard-coded number with no way to move
// it: a twenty-task ceiling that a user with a genuinely large sweep could not
// raise and a user on a metered provider could not lower. The tier is the knob,
// and it is NAMED rather than numeric so the config file says what it means and
// so "no ceiling at all" is expressible without a sentinel integer.
//
// IT REMAINS A HARD CAP, not an advisory warning. The reference implementation
// warns and proceeds; here a plan over the tier is REJECTED, because under this
// posture every plan task is a child inheriting a 480-turn ceiling — fifty tasks
// authorise 16,000 child turns from one tool call. A warning the model emits to
// itself is not a bound on that. The rejection names the tier and the setting,
// so the ceiling is discoverable at the moment it is hit rather than being a
// number the user has to find in the source.
type PlanSize string

const (
	// PlanSizeSmall suits a metered provider or a first look at the feature.
	PlanSizeSmall PlanSize = "small"
	// PlanSizeMedium is the default, and its 20 is TODAY'S number. Phase 2 exists
	// to measure whether fan-out would pay, not to run large plans, and changing
	// the default while making it configurable would have hidden a behaviour
	// change inside a mechanism change.
	PlanSizeMedium PlanSize = "medium"
	// PlanSizeLarge is for a real sweep across a large tree.
	PlanSizeLarge PlanSize = "large"
	// PlanSizeUnrestricted removes the ceiling. Named rather than "0" so a reader
	// of the config file cannot mistake it for "unset".
	PlanSizeUnrestricted PlanSize = "unrestricted"
)

// DefaultPlanSize is what an unset — or unreadable — setting resolves to.
const DefaultPlanSize = PlanSizeMedium

// planSizeTiers is THE tier table: the only place a tier maps to a number.
//
// Ordered smallest-first, and that order is load-bearing twice over: it gives
// PlanSizeNames a stable rendering and it gives the project-config merge its
// tighten-only comparison. A second ordered list elsewhere would drift
// (invariant 5), so both read this one.
var planSizeTiers = []struct {
	size     PlanSize
	maxTasks int
}{
	{PlanSizeSmall, 5},
	{PlanSizeMedium, 20},
	{PlanSizeLarge, 50},
	// 0 means no ceiling. It is the LAST entry, so it is also the loosest rank —
	// which is what makes the tighten-only rule reject it from project config.
	{PlanSizeUnrestricted, 0},
}

// MaxTasks is the tier's ceiling on a plan's task count. 0 means no ceiling.
//
// An unrecognised tier resolves to the DEFAULT rather than to no ceiling. Fail
// closed: a typo in a config file must never be the thing that removes a bound.
func (size PlanSize) MaxTasks() int {
	for _, tier := range planSizeTiers {
		if tier.size == size {
			return tier.maxTasks
		}
	}
	return DefaultPlanSize.MaxTasks()
}

// Valid reports whether the tier is one this build knows.
func (size PlanSize) Valid() bool {
	for _, tier := range planSizeTiers {
		if tier.size == size {
			return true
		}
	}
	return false
}

// rank orders the tiers from tightest to loosest. Used only by the merge rules;
// unexported because "how loose is this" is not a question a caller outside this
// package should be answering for itself.
func (size PlanSize) rank() int {
	for index, tier := range planSizeTiers {
		if tier.size == size {
			return index
		}
	}
	// An unknown tier ranks LOOSEST, so a "may only tighten" comparison can never
	// adopt it.
	//
	// The first version returned -1 here with a comment claiming that made it
	// safe. It does the opposite: the comparison adopts the SMALLER rank, so a
	// tier ranked -1 would win against every real tier. Nothing was reachable
	// through it — ParsePlanSize rejects an unknown name before the merge calls
	// rank — but a defence-in-depth value that only holds because of the guard in
	// front of it is not defence in depth. It must be safe on its own.
	return len(planSizeTiers)
}

// ParsePlanSize resolves a configured value, case- and space-insensitively.
//
// An empty value is NOT an error: an unset setting is the default, which is the
// overwhelmingly common case and must not make a config file unloadable.
func ParsePlanSize(value string) (PlanSize, error) {
	trimmed := strings.ToLower(strings.TrimSpace(value))
	if trimmed == "" {
		return DefaultPlanSize, nil
	}
	size := PlanSize(trimmed)
	if !size.Valid() {
		return DefaultPlanSize, fmt.Errorf("unknown plan size %q; expected one of: %s", value, strings.Join(PlanSizeNames(), ", "))
	}
	return size, nil
}

// PlanSizeNames renders the tiers for an error message, tightest first.
func PlanSizeNames() []string {
	names := make([]string, 0, len(planSizeTiers))
	for _, tier := range planSizeTiers {
		names = append(names, string(tier.size))
	}
	return names
}

// PlanSize resolves the configured tier for this workspace.
//
// It never fails: an unreadable or unknown value is the default, which is what
// keeps a typo in a config file from either breaking the run or removing the
// ceiling. Callers wanting to REPORT a bad value call ParsePlanSize.
func (cfg ProfilesConfig) PlanSizeTier() PlanSize {
	size, err := ParsePlanSize(cfg.PlanSize)
	if err != nil {
		return DefaultPlanSize
	}
	return size
}
