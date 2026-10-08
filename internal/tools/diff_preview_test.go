package tools

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestBoundedFileDiffRefusesPartialOrBinaryContent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.txt")
	if diff, ok := boundedFileDiff(path, "old", "new", true, true); !ok || diff.Path != path || !diff.OldExists || !diff.NewExists || diff.OldText != "old" || diff.NewText != "new" {
		t.Fatalf("small text diff = %#v, %t", diff, ok)
	}
	for _, tc := range []struct {
		name string
		old  string
		new  string
	}{
		{"unchanged", "same", "same"},
		{"binary old", string([]byte{0xff}), "text"},
		{"binary new", "text", string([]byte{0xff})},
		{"nul old", "token=sk-proj-abc\x00def", "text"},
		{"escape new", "text", "token=sk-proj-abc\x1bdef"},
		{"c1 old", "token=sk-proj-abc\u0085def", "text"},
		{"zero width space", "token=sk-proj-abc\u200bdef", "text"},
		{"zero width joiner", "token=sk-proj-abc\u200ddef", "text"},
		{"byte order mark", "token=sk-proj-abc\ufeffdef", "text"},
		{"soft hyphen", "token=sk-proj-abc\u00addef", "text"},
		{"non breaking space", "token=sk-proj-abc\u00a0def", "text"},
		{"too large", strings.Repeat("a", maxToolPreviewBytes+1), "b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if diff, ok := boundedFileDiff(path, tc.old, tc.new, true, true); ok || diff != (FileDiff{}) {
				t.Fatalf("unexpected diff = %#v, %t", diff, ok)
			}
		})
	}
}

func TestBoundedFileDiffPreservesEmptyFileOperations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.txt")
	for _, tc := range []struct {
		name                 string
		oldExists, newExists bool
	}{
		{name: "create", newExists: true},
		{name: "delete", oldExists: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			diff, ok := boundedFileDiff(path, "", "", tc.oldExists, tc.newExists)
			if !ok || diff.OldExists != tc.oldExists || diff.NewExists != tc.newExists {
				t.Fatalf("empty %s = %#v, %t", tc.name, diff, ok)
			}
		})
	}
}

func TestBoundedUnifiedDiffRejectsUnsafeRichText(t *testing.T) {
	if got := boundedUnifiedDiff("safe.txt", "before\n", "after\n"); got == "" {
		t.Fatal("safe unified diff was unexpectedly omitted")
	}
	for _, content := range []string{
		"token=sk-proj-abc\x00def",
		"token=sk-proj-abc\x1bdef",
		"token=sk-proj-abc\u200bdef",
		string([]byte{0xff}),
	} {
		if got := boundedUnifiedDiff("secret.txt", content, "safe\n"); got != "" {
			t.Fatalf("unsafe rich diff = %q", got)
		}
	}

	old := strings.Repeat("unchanged\n", 12) + "form\ffeed\n" + strings.Repeat("unchanged\n", 12)
	updated := strings.Replace(old, "unchanged\n", "changed\n", 1)
	if got := boundedUnifiedDiff("safe-hunk.txt", old, updated); got == "" || strings.Contains(got, "\f") {
		t.Fatalf("safe hunk near unrelated unsafe text = %q", got)
	}
}

func TestStructuredPatchFileDiffsPreserveEmptyOperationsAndResultBudget(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	changes := []structuredPatchChange{
		{kind: structuredPatchAdd, to: structuredPatchTarget{absolute: filepath.Join(root, "created"), relative: "created"}},
		{kind: structuredPatchDelete, from: structuredPatchTarget{absolute: filepath.Join(root, "deleted"), relative: "deleted"}},
		{kind: structuredPatchUpdate, from: structuredPatchTarget{absolute: filepath.Join(root, "from"), relative: "from"}, to: structuredPatchTarget{absolute: filepath.Join(root, "to"), relative: "to"}},
	}
	diffs := fileDiffsFromStructuredPatch(".", changes)
	if len(diffs) != 4 {
		t.Fatalf("empty create/delete/move diffs = %#v", diffs)
	}
	for _, diff := range diffs {
		if diff.Path == "" || (!diff.OldExists && !diff.NewExists) {
			t.Fatalf("invalid diff = %#v", diff)
		}
	}

	large := strings.Repeat("x", maxToolResultFileDiffBytes/3+1)
	budgeted := fileDiffsFromStructuredPatch(".", []structuredPatchChange{
		{kind: structuredPatchAdd, to: structuredPatchTarget{absolute: filepath.Join(root, "one")}, after: large},
		{kind: structuredPatchAdd, to: structuredPatchTarget{absolute: filepath.Join(root, "two")}, after: "tiny"},
		{kind: structuredPatchAdd, to: structuredPatchTarget{absolute: filepath.Join(root, "three")}, after: large},
		{kind: structuredPatchAdd, to: structuredPatchTarget{absolute: filepath.Join(root, "four")}, after: large},
	})
	if len(budgeted) != 3 || filepath.Base(budgeted[0].Path) != "one" || filepath.Base(budgeted[1].Path) != "two" || filepath.Base(budgeted[2].Path) != "three" {
		t.Fatalf("ordered aggregate file-diff budget = %#v", budgeted)
	}
}

func TestStructuredPatchFileDiffsKeepEligibleSameBasenameSibling(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	diffs := fileDiffsFromStructuredPatch(".", []structuredPatchChange{
		{
			kind:  structuredPatchAdd,
			to:    structuredPatchTarget{absolute: filepath.Join(root, "a.go"), relative: "a.go"},
			after: strings.Repeat("x", maxToolPreviewBytes+1),
		},
		{
			kind:  structuredPatchAdd,
			to:    structuredPatchTarget{absolute: filepath.Join(root, "sub", "a.go"), relative: filepath.Join("sub", "a.go")},
			after: "package sub\n",
		},
	})
	if len(diffs) != 1 || diffs[0].Path != filepath.Join(root, "sub", "a.go") {
		t.Fatalf("same-basename rich diffs = %#v", diffs)
	}
}

func TestBoundedDiffPreservesOrdinaryUnicodeButRejectsObfuscatedSecrets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "unicode.txt")
	for name, content := range map[string]string{
		"family emoji":              "family: 👨‍👩‍👧‍👦\n",
		"emoji variation selector":  "heart: ❤️\n",
		"combining grapheme joiner": "ordinary\u034fprose\n",
		"nonbreaking space":         "ordinary\u00a0prose\n",
		"byte order mark":           "\ufeffdocument\n",
		"soft hyphen":               "co\u00adoperate\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, ok := boundedFileDiff(path, "before\n", content, true, true); !ok {
				t.Fatalf("ordinary Unicode content was rejected: %q", content)
			}
			if preview := boundedUnifiedDiff("unicode.txt", "before\n", content); preview == "" {
				t.Fatal("ordinary Unicode preview was omitted")
			}
		})
	}

	secret := "sk-ant-api03-AAAABBBBCCCCDDDDEEEEFFFFGGGG"
	for name, separator := range map[string]string{
		"zero width space":          "\u200b",
		"zero width joiner":         "\u200d",
		"byte order mark":           "\ufeff",
		"soft hyphen":               "\u00ad",
		"nonbreaking space":         "\u00a0",
		"combining grapheme joiner": "\u034f",
		"variation selector":        "\ufe0f",
	} {
		t.Run("split "+name, func(t *testing.T) {
			obfuscated := secret[:20] + separator + secret[20:]
			if _, ok := boundedFileDiff(path, "before\n", obfuscated, true, true); ok {
				t.Fatalf("obfuscated credential produced a rich diff: %q", obfuscated)
			}
			if preview := boundedUnifiedDiff("secret.txt", "before\n", obfuscated); preview != "" {
				t.Fatalf("obfuscated credential produced preview: %q", preview)
			}
		})
	}
}

func TestWriteFileMarksSuppressedObfuscatedSecretAsRedacted(t *testing.T) {
	root := t.TempDir()
	secret := "sk-ant-api03-AAAABBBBCCCCDDDDEEEEFFFFGGGG"
	obfuscated := secret[:20] + "\u200b" + secret[20:]
	result := NewScopedWriteFileTool(root, nil).Run(t.Context(), map[string]any{
		"path": "secret.txt", "content": obfuscated,
	})
	if result.Status != StatusOK || len(result.ChangedFiles) != 1 || len(result.FileDiffs) != 0 || !result.Redacted {
		t.Fatalf("obfuscated-secret result = status=%s changed=%#v diffs=%#v redacted=%t", result.Status, result.ChangedFiles, result.FileDiffs, result.Redacted)
	}
}
