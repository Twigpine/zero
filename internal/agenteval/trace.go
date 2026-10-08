package agenteval

import (
	"encoding/json"
	"sort"
	"strings"
)

func ParseTraceEventKeys(stdout string) []string {
	seen := map[string]bool{}
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			continue
		}
		if key, ok := traceEventKey(event); ok {
			seen[key] = true
		}
	}
	keys := make([]string, 0, len(seen))
	for key := range seen {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func MissingTraceEvents(required []string, stdout string) []string {
	seen := map[string]bool{}
	for _, key := range ParseTraceEventKeys(stdout) {
		seen[key] = true
	}
	missingSet := map[string]bool{}
	for _, key := range required {
		key = strings.TrimSpace(key)
		if key == "" || seen[key] {
			continue
		}
		missingSet[key] = true
	}
	missing := make([]string, 0, len(missingSet))
	for key := range missingSet {
		missing = append(missing, key)
	}
	sort.Strings(missing)
	return missing
}

func traceEventKey(event map[string]any) (string, bool) {
	if eventType, ok := traceString(event, "type"); ok {
		return traceKey(eventType, event), true
	}
	if eventType, ok := traceString(event, "event"); ok {
		return traceKey(eventType, event), true
	}
	return "", false
}

func traceKey(eventType string, event map[string]any) string {
	name, ok := traceString(event, "name")
	if !ok {
		return eventType
	}
	return eventType + ":" + name
}

func traceString(event map[string]any, field string) (string, bool) {
	value, ok := event[field].(string)
	if !ok {
		return "", false
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return "", false
	}
	return value, true
}
