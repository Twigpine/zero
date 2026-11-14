package sessions

import (
	"encoding/base64"
	"strings"
)

const importedSessionTagPrefix = "imported:v1:"

// ImportedSessionTag returns the versioned provenance stamp used for a session
// copied from another agent. Operational consumers must validate this complete
// stamp instead of treating the free-form "imported:" tag namespace as
// authority.
func ImportedSessionTag(agent, sourceID string) string {
	encode := base64.RawURLEncoding.EncodeToString
	return importedSessionTagPrefix + encode([]byte(agent)) + ":" + encode([]byte(sourceID))
}

// ParseImportedSessionTag validates and decodes a versioned import provenance
// stamp. Legacy display-only tags such as "imported:claude-code" deliberately
// do not pass this authority boundary.
func ParseImportedSessionTag(tag string) (agent, sourceID string, ok bool) {
	trimmed := strings.TrimSpace(tag)
	encoded := strings.TrimPrefix(trimmed, importedSessionTagPrefix)
	if encoded == trimmed {
		return "", "", false
	}
	encodedAgent, encodedID, found := strings.Cut(encoded, ":")
	if !found || encodedAgent == "" || encodedID == "" || strings.Contains(encodedID, ":") {
		return "", "", false
	}
	decodedAgent, agentErr := base64.RawURLEncoding.DecodeString(encodedAgent)
	decodedID, idErr := base64.RawURLEncoding.DecodeString(encodedID)
	if agentErr != nil || idErr != nil || len(decodedAgent) == 0 || len(decodedID) == 0 {
		return "", "", false
	}
	return string(decodedAgent), string(decodedID), true
}

// IsImportedSession reports whether metadata carries validated foreign-session
// provenance rather than merely a human tag that begins with "imported:".
func IsImportedSession(metadata Metadata) bool {
	_, _, ok := ParseImportedSessionTag(metadata.Tag)
	return ok
}

// resolveImportedModelFields applies the imported-session model policy shared
// by every lineage operation. Older imports stored the foreign model in the
// operational ModelID field. Unless Zero has explicitly selected that model,
// descendants retain it as provenance only.
func resolveImportedModelFields(parent Metadata) (modelID, sourceModelID string) {
	modelID = parent.ModelID
	sourceModelID = parent.SourceModelID
	if IsImportedSession(parent) && sourceModelID == "" && !parent.ModelSelectedLocally {
		sourceModelID = modelID
		modelID = ""
	}
	return modelID, sourceModelID
}
