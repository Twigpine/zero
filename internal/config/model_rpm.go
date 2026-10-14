package config

import (
	"fmt"
	"github.com/Gitlawb/zero/internal/modelregistry"
	"strings"
)

func normalizeModelRPM(limits map[string]int) (map[string]int, error) {
	if len(limits) == 0 {
		return nil, nil
	}
	registry, err := modelregistry.DefaultRegistry()
	if err != nil {
		return nil, err
	}
	out := make(map[string]int, len(limits))
	for key, n := range limits {
		model := strings.TrimSpace(key)
		if model == "" || n < 0 {
			return nil, fmt.Errorf("invalid modelRPM entry %q: model must be non-empty and limit >= 0", key)
		}
		if id, ok := registry.ResolveID(model); ok {
			model = id
		}
		if old, exists := out[model]; exists && old != n {
			return nil, fmt.Errorf("conflicting modelRPM entries resolve to %q", model)
		}
		out[model] = n
	}
	return out, nil
}

// Project config can tighten a user cap but cannot relax it.
func mergeModelRPM(dst *map[string]int, src map[string]int, tighten bool) {
	if len(src) == 0 {
		return
	}
	if *dst == nil {
		*dst = make(map[string]int)
	}
	for model, n := range src {
		old := (*dst)[model]
		if !tighten || old == 0 || (n > 0 && n < old) {
			(*dst)[model] = n
		}
	}
}
