package sessions

import "testing"

func TestImportedSessionProvenanceRequiresValidatedVersionedTag(t *testing.T) {
	tag := ImportedSessionTag("claude-code", "foreign:id")
	agent, sourceID, ok := ParseImportedSessionTag(tag)
	if !ok || agent != "claude-code" || sourceID != "foreign:id" {
		t.Fatalf("ParseImportedSessionTag(%q) = %q, %q, %v", tag, agent, sourceID, ok)
	}
	if !IsImportedSession(Metadata{Tag: tag}) {
		t.Fatal("versioned provenance was not recognized")
	}
	for _, untrusted := range []string{
		"imported:archive",
		"imported:claude-code:foreign-id",
		"imported:v1:not-base64:not-base64:extra",
		"imported:v1::",
	} {
		if IsImportedSession(Metadata{Tag: untrusted}) {
			t.Errorf("display or malformed tag %q gained import authority", untrusted)
		}
	}
}
