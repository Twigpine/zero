package config

import (
	"bytes"
	"encoding/json"
	"fmt"
)

type jsonObjectSpan struct {
	start   int
	end     int
	members []jsonMemberSpan
}

type jsonMemberSpan struct {
	key          string
	leadingStart int
	keyStart     int
	valueStart   int
	valueEnd     int
	commaAfter   int
}

// setPetPreferenceJSON changes only preferences.pet. It keeps the original
// bytes for every unrelated member so choosing a companion does not reorder or
// reformat the user's configuration file.
func setPetPreferenceJSON(data []byte, pet string) ([]byte, error) {
	rootStart := skipJSONSpace(data, 0)
	root, err := parseJSONObject(data, rootStart)
	if err != nil {
		return nil, err
	}
	preferencesIndex := lastJSONMember(root.members, "preferences")
	if preferencesIndex < 0 {
		if pet == "" {
			return data, nil
		}
		encodedPet, err := json.Marshal(pet)
		if err != nil {
			return nil, fmt.Errorf("encode pet preference: %w", err)
		}
		return insertJSONMember(data, root, "preferences", petJSONObject(encodedPet)), nil
	}

	preferenceMember := root.members[preferencesIndex]
	preferenceValue := bytes.TrimSpace(data[preferenceMember.valueStart:preferenceMember.valueEnd])
	if bytes.Equal(preferenceValue, []byte("null")) {
		if pet == "" {
			if lastJSONMember(root.members[:preferencesIndex], "preferences") >= 0 {
				return data, nil
			}
			return removeJSONMember(data, root, preferencesIndex), nil
		}
		encodedPet, err := json.Marshal(pet)
		if err != nil {
			return nil, fmt.Errorf("encode pet preference: %w", err)
		}
		return replaceJSONRange(data, preferenceMember.valueStart, preferenceMember.valueEnd, petJSONObject(encodedPet)), nil
	}
	preferences, err := parseJSONObject(data, preferenceMember.valueStart)
	if err != nil {
		return nil, fmt.Errorf("preferences must be a JSON object: %w", err)
	}
	petIndex := lastJSONMember(preferences.members, "pet")
	if pet != "" {
		encodedPet, err := json.Marshal(pet)
		if err != nil {
			return nil, fmt.Errorf("encode pet preference: %w", err)
		}
		if petIndex >= 0 {
			member := preferences.members[petIndex]
			return replaceJSONRange(data, member.valueStart, member.valueEnd, encodedPet), nil
		}
		return insertJSONMember(data, preferences, "pet", encodedPet), nil
	}

	for petIndex >= 0 {
		data = removeJSONMember(data, preferences, petIndex)
		root, err = parseJSONObject(data, rootStart)
		if err != nil {
			return nil, err
		}
		preferencesIndex = lastJSONMember(root.members, "preferences")
		if preferencesIndex < 0 {
			return data, nil
		}
		preferenceMember = root.members[preferencesIndex]
		preferences, err = parseJSONObject(data, preferenceMember.valueStart)
		if err != nil {
			return nil, err
		}
		petIndex = lastJSONMember(preferences.members, "pet")
	}
	if len(preferences.members) == 0 {
		if lastJSONMember(root.members[:preferencesIndex], "preferences") >= 0 {
			return data, nil
		}
		return removeJSONMember(data, root, preferencesIndex), nil
	}
	return data, nil
}

func parseJSONObject(data []byte, start int) (jsonObjectSpan, error) {
	if start < 0 || start >= len(data) || data[start] != '{' {
		return jsonObjectSpan{}, fmt.Errorf("expected JSON object")
	}
	object := jsonObjectSpan{start: start}
	leadingStart := start + 1
	for {
		i := skipJSONSpace(data, leadingStart)
		if i >= len(data) {
			return jsonObjectSpan{}, fmt.Errorf("unterminated JSON object")
		}
		if data[i] == '}' {
			object.end = i
			return object, nil
		}
		if data[i] != '"' {
			return jsonObjectSpan{}, fmt.Errorf("expected JSON object key")
		}
		keyStart := i
		keyEnd, err := scanJSONString(data, i)
		if err != nil {
			return jsonObjectSpan{}, err
		}
		var key string
		if err := json.Unmarshal(data[keyStart:keyEnd], &key); err != nil {
			return jsonObjectSpan{}, err
		}
		i = skipJSONSpace(data, keyEnd)
		if i >= len(data) || data[i] != ':' {
			return jsonObjectSpan{}, fmt.Errorf("expected colon after JSON object key")
		}
		valueStart := skipJSONSpace(data, i+1)
		valueEnd, err := scanJSONValue(data, valueStart)
		if err != nil {
			return jsonObjectSpan{}, err
		}
		i = skipJSONSpace(data, valueEnd)
		member := jsonMemberSpan{
			key:          key,
			leadingStart: leadingStart,
			keyStart:     keyStart,
			valueStart:   valueStart,
			valueEnd:     valueEnd,
			commaAfter:   -1,
		}
		if i < len(data) && data[i] == ',' {
			member.commaAfter = i
			object.members = append(object.members, member)
			leadingStart = i + 1
			continue
		}
		if i >= len(data) || data[i] != '}' {
			return jsonObjectSpan{}, fmt.Errorf("expected comma or end of JSON object")
		}
		object.members = append(object.members, member)
		object.end = i
		return object, nil
	}
}

func scanJSONString(data []byte, start int) (int, error) {
	for i := start + 1; i < len(data); i++ {
		switch data[i] {
		case '\\':
			i++
		case '"':
			return i + 1, nil
		}
	}
	return 0, fmt.Errorf("unterminated JSON string")
}

func scanJSONValue(data []byte, start int) (int, error) {
	if start >= len(data) {
		return 0, fmt.Errorf("missing JSON value")
	}
	if data[start] == '"' {
		return scanJSONString(data, start)
	}
	if data[start] == '{' || data[start] == '[' {
		stack := []byte{data[start]}
		for i := start + 1; i < len(data); i++ {
			if data[i] == '"' {
				end, err := scanJSONString(data, i)
				if err != nil {
					return 0, err
				}
				i = end - 1
				continue
			}
			switch data[i] {
			case '{', '[':
				stack = append(stack, data[i])
			case '}', ']':
				open := stack[len(stack)-1]
				if (open == '{' && data[i] != '}') || (open == '[' && data[i] != ']') {
					return 0, fmt.Errorf("mismatched JSON delimiter")
				}
				stack = stack[:len(stack)-1]
				if len(stack) == 0 {
					return i + 1, nil
				}
			}
		}
		return 0, fmt.Errorf("unterminated JSON value")
	}
	for i := start; i < len(data); i++ {
		switch data[i] {
		case ',', '}', ']', ' ', '\t', '\r', '\n':
			if i == start {
				return 0, fmt.Errorf("missing JSON value")
			}
			return i, nil
		}
	}
	return len(data), nil
}

func skipJSONSpace(data []byte, start int) int {
	for start < len(data) {
		switch data[start] {
		case ' ', '\t', '\r', '\n':
			start++
		default:
			return start
		}
	}
	return start
}

func lastJSONMember(members []jsonMemberSpan, key string) int {
	for i := len(members) - 1; i >= 0; i-- {
		if members[i].key == key {
			return i
		}
	}
	return -1
}

func petJSONObject(encodedPet []byte) []byte {
	result := append([]byte(`{"pet":`), encodedPet...)
	return append(result, '}')
}

// setNotifyJSONObject replaces only the notify member's value, keeping the
// original bytes of every unrelated member so a notification save cannot
// reorder, reformat, or drop the user's other settings — including values whose
// EXPLICIT presence matters (tools.deferThreshold: 0, mcp servers'
// disabled: false) and keys the typed FileConfig does not model at all. The
// typed serializer cannot round-trip explicit zeros (omitempty drops them), so
// notification writes go through this byte-preserving editor instead
// (maintainer review, PR #1001). An empty notify block removes the member
// entirely so a reset leaves no `"notify": {}` husk behind.
//
// A hand-edited file may contain DUPLICATE notify members. Go's decoder
// MERGES the fields of duplicate object members into the same struct, so an
// earlier duplicate's field survives a replacement that omits it — e.g.
// {"notify":{"mode":"off"},"notify":{"focusMode":"focused"}} + a
// replacement {"focusMode":"always"} would leave mode "off" effective.
// The nonempty path therefore removes EVERY duplicate and inserts the
// replacement fresh (full-replace semantics: nothing inherited), and the
// reset path removes every member the same way. UpdateNotify's partial
// merge is unaffected: it seeds omitted fields from the stored value BEFORE
// calling this editor, so the value it passes is already complete.
// Mirrors setPetPreferenceJSON's duplicate handling.
func setNotifyJSONObject(data []byte, notify NotifyConfig) ([]byte, error) {
	encoded, err := json.Marshal(notify)
	if err != nil {
		return nil, fmt.Errorf("encode notify preference: %w", err)
	}

	rootStart := skipJSONSpace(data, 0)
	root, err := parseJSONObject(data, rootStart)
	if err != nil {
		return nil, err
	}
	notifyIndex := lastJSONMember(root.members, "notify")
	// Count duplicates: the ordinary file has exactly one notify member, and
	// for it a positional replace keeps the key where the user put it and
	// preserves their formatting (byte-preserving contract). The
	// remove-all-and-reinsert path below is ONLY for files that actually
	// contain duplicates, where Go's field-merging decode would otherwise let
	// an earlier block's fields leak into the replacement.
	duplicates := 0
	for _, member := range root.members {
		if member.key == "notify" {
			duplicates++
		}
	}
	if string(encoded) == "{}" {
		for notifyIndex >= 0 {
			data = removeJSONMember(data, root, notifyIndex)
			rootStart = skipJSONSpace(data, 0)
			root, err = parseJSONObject(data, rootStart)
			if err != nil {
				return nil, err
			}
			notifyIndex = lastJSONMember(root.members, "notify")
		}
		return data, nil
	}
	if duplicates <= 1 {
		if notifyIndex < 0 {
			return insertJSONMember(data, root, "notify", encoded), nil
		}
		// Single member: replace its value in place — no key movement, no
		// reformatting of the surrounding bytes.
		return replaceJSONRange(data, root.members[notifyIndex].valueStart, root.members[notifyIndex].valueEnd, encoded), nil
	}
	// Multiple duplicates: Go merges their fields at decode time, so the
	// replacement must not inherit anything — remove every duplicate and
	// insert the complete value fresh.
	for notifyIndex >= 0 {
		data = removeJSONMember(data, root, notifyIndex)
		rootStart = skipJSONSpace(data, 0)
		root, err = parseJSONObject(data, rootStart)
		if err != nil {
			return nil, err
		}
		notifyIndex = lastJSONMember(root.members, "notify")
	}
	return insertJSONMember(data, root, "notify", encoded), nil
}

func replaceJSONRange(data []byte, start, end int, replacement []byte) []byte {
	result := make([]byte, 0, len(data)-(end-start)+len(replacement))
	result = append(result, data[:start]...)
	result = append(result, replacement...)
	return append(result, data[end:]...)
}

func removeJSONMember(data []byte, object jsonObjectSpan, index int) []byte {
	member := object.members[index]
	start, end := member.leadingStart, member.valueEnd
	if member.commaAfter >= 0 {
		end = member.commaAfter + 1
	} else if index > 0 {
		start = object.members[index-1].commaAfter
	}
	return replaceJSONRange(data, start, end, nil)
}

func insertJSONMember(data []byte, object jsonObjectSpan, key string, value []byte) []byte {
	encodedKey, _ := json.Marshal(key)
	entry := append(append(encodedKey, ':', ' '), value...)
	if len(object.members) == 0 {
		interior := data[object.start+1 : object.end]
		if newline := bytes.LastIndexByte(interior, '\n'); newline >= 0 {
			closingIndent := interior[newline+1:]
			indentUnit := []byte("  ")
			if bytes.Contains(closingIndent, []byte("\t")) {
				indentUnit = []byte("\t")
			}
			lineBreak := []byte("\n")
			if newline > 0 && interior[newline-1] == '\r' {
				lineBreak = []byte("\r\n")
			}
			insert := append(append(append(lineBreak, closingIndent...), indentUnit...), entry...)
			return replaceJSONRange(data, object.start+1, object.start+1, insert)
		}
		return replaceJSONRange(data, object.end, object.end, entry)
	}
	last := object.members[len(object.members)-1]
	separator := []byte(", ")
	if bytes.Contains(data[last.valueEnd:object.end], []byte("\n")) {
		lineStart := bytes.LastIndexByte(data[:last.keyStart], '\n') + 1
		indent := data[lineStart:last.keyStart]
		separator = append([]byte(",\n"), indent...)
	}
	insert := append(separator, entry...)
	return replaceJSONRange(data, last.valueEnd, last.valueEnd, insert)
}
