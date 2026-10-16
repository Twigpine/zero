package tools

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func mustApplyPatchPaths(t *testing.T, patch string) []string {
	t.Helper()
	prepared, err := prepareApplyPatchArguments(map[string]any{"patch": patch})
	if err != nil {
		t.Fatalf("prepareApplyPatchArguments: %v", err)
	}
	return prepared.paths
}

func TestApplyPatchPathsHandleQuotedAndSpacedNames(t *testing.T) {
	// git C-quotes a path that contains a space; the old strings.Fields parse kept
	// only the first whitespace-delimited token ("a/dir/file" and the literal `name`
	// pieces would split), losing the real name. The whole post-prefix value, with a
	// trailing tab timestamp dropped and the quotes removed, must survive (L18).
	patch := "--- \"a/dir/file name.go\"\t2024-01-01 00:00:00\n" +
		"+++ \"b/dir/file name.go\"\n" +
		"@@ -1 +1 @@\n-old\n+new\n"
	got := mustApplyPatchPaths(t, patch)
	if !slices.Contains(got, "dir/file name.go") {
		t.Fatalf("quoted spaced path not extracted: %v", got)
	}
}

func TestApplyPatchPathsUnspacedStillWork(t *testing.T) {
	patch := "--- a/x.go\n+++ b/x.go\n@@ -1 +1 @@\n-old\n+new\n"
	got := mustApplyPatchPaths(t, patch)
	if !slices.Contains(got, "x.go") {
		t.Fatalf("plain path not extracted: %v", got)
	}
	// /dev/null (a deletion/creation sentinel) must not be reported as a target.
	if slices.Contains(got, "/dev/null") {
		t.Fatalf("/dev/null should not be a header path: %v", got)
	}
}

// git only C-quotes names containing characters it must escape, so a filename
// whose last byte is an ordinary space reaches the header raw. git apply reads
// it to the tab or newline and patches that exact file, so trimming the operand
// here would leave the token gate comparing against a neighbouring pathname.
func TestApplyPatchPathsPreserveSurroundingSpacesInNames(t *testing.T) {
	patch := "--- a/bridge-token \t2024-01-01 00:00:00\n" +
		"+++ b/bridge-token \n" +
		"@@ -1 +1 @@\n-old\n+new\n"
	got := mustApplyPatchPaths(t, patch)
	if !slices.Contains(got, "bridge-token ") {
		t.Fatalf("trailing-space path not preserved: %q", got)
	}
	if slices.Contains(got, "bridge-token") {
		t.Fatalf("trailing space trimmed off the header path: %q", got)
	}
}

func TestApplyPatchPathsRejectAmbiguousDiffOperands(t *testing.T) {
	patch := "diff --git a/source b/part b/destination\nnew file mode 100644\n"
	if prepared, err := prepareApplyPatchArguments(map[string]any{"patch": patch}); err == nil {
		t.Fatalf("prepareApplyPatchArguments = %#v, want ambiguous-path error", prepared)
	}
}

func TestApplyPatchPathsParseGitDefaultAndNoPrefixOutput(t *testing.T) {
	for _, operation := range []string{"rename", "copy", "modify"} {
		for _, noPrefix := range []bool{false, true} {
			name := operation + "/default-prefix"
			if noPrefix {
				name = operation + "/no-prefix"
			}
			t.Run(name, func(t *testing.T) {
				patch, source, destination := gitGeneratedPatch(t, operation, noPrefix)
				got := mustApplyPatchPaths(t, patch)
				for _, want := range []string{source, destination} {
					if !slices.Contains(got, want) {
						t.Fatalf("Git-generated %s paths = %q, want %q\npatch:\n%s", operation, got, want, patch)
					}
				}
			})
		}
	}
}

func TestApplyPatchPathsAcceptNoPrefixSpacedRenameAndCopyWithHunks(t *testing.T) {
	for _, operation := range []string{"rename", "copy"} {
		t.Run(operation, func(t *testing.T) {
			patch := "diff --git old name.txt new name.txt\n" +
				"similarity index 75%\n" +
				operation + " from old name.txt\n" +
				operation + " to new name.txt\n" +
				"--- old name.txt\n" +
				"+++ new name.txt\n" +
				"@@ -1 +1 @@\n" +
				"-old\n" +
				"+new\n"
			paths := mustApplyPatchPaths(t, patch)
			for _, want := range []string{"old name.txt", "new name.txt"} {
				if !slices.Contains(paths, want) {
					t.Fatalf("%s paths = %v, want %q", operation, paths, want)
				}
			}
		})
	}
}

func TestApplyPatchPathsRejectContradictoryRenameMetadata(t *testing.T) {
	patch := "diff --git source.txt destination.txt\n" +
		"similarity index 100%\n" +
		"rename from other.txt\n" +
		"rename to destination.txt\n"
	if prepared, err := prepareApplyPatchArguments(map[string]any{"patch": patch}); err == nil {
		t.Fatalf("prepareApplyPatchArguments = %#v, want disagreement error", prepared)
	}
}

func TestApplyPatchPathsRejectContradictoryGitHeaders(t *testing.T) {
	tests := map[string]string{
		"rename destination": "diff --git old.txt new.txt\nrename from old.txt\nrename to other.txt\n",
		"copy source":        "diff --git old.txt new.txt\ncopy from other.txt\ncopy to new.txt\n",
		"update":             "diff --git old.txt new.txt\n--- old.txt\n+++ other.txt\n@@ -1 +1 @@\n-old\n+new\n",
		"create":             "diff --git new.txt new.txt\n--- /dev/null\n+++ other.txt\n@@ -0,0 +1 @@\n+new\n",
		"delete":             "diff --git old.txt old.txt\n--- other.txt\n+++ /dev/null\n@@ -1 +0,0 @@\n-old\n",
	}
	for name, patch := range tests {
		t.Run(name, func(t *testing.T) {
			if prepared, err := prepareApplyPatchArguments(map[string]any{"patch": patch}); err == nil {
				t.Fatalf("prepareApplyPatchArguments = %#v, want disagreement error", prepared)
			}
		})
	}
}

func TestApplyPatchPathsAcceptMatchingGitHeaders(t *testing.T) {
	tests := map[string]string{
		"rename": "diff --git old.txt new.txt\nrename from old.txt\nrename to new.txt\n",
		"copy":   "diff --git old.txt new.txt\ncopy from old.txt\ncopy to new.txt\n",
		"update": "diff --git old.txt new.txt\n--- old.txt\n+++ new.txt\n@@ -1 +1 @@\n-old\n+new\n",
		"create": "diff --git new.txt new.txt\n--- /dev/null\n+++ new.txt\n@@ -0,0 +1 @@\n+new\n",
		"delete": "diff --git old.txt old.txt\n--- old.txt\n+++ /dev/null\n@@ -1 +0,0 @@\n-old\n",
	}
	for name, patch := range tests {
		t.Run(name, func(t *testing.T) {
			mustApplyPatchPaths(t, patch)
		})
	}
}

func gitGeneratedPatch(t *testing.T, operation string, noPrefix bool) (string, string, string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git unavailable: %v", err)
	}
	dir := t.TempDir()
	runGit := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		for _, entry := range os.Environ() {
			if !strings.HasPrefix(entry, "GIT_CONFIG_") {
				cmd.Env = append(cmd.Env, entry)
			}
		}
		cmd.Env = append(cmd.Env,
			"GIT_CONFIG_NOSYSTEM=1",
			"GIT_CONFIG_GLOBAL="+filepath.Join(dir, "empty-global-gitconfig"),
		)
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
		}
		return string(output)
	}
	runGit("init", "-q")
	runGit("config", "user.name", "Zero Tests")
	runGit("config", "user.email", "zero-tests@example.invalid")

	source := "source name.txt"
	destination := "destination name.txt"
	if operation == "modify" {
		source = "quoted-\u00e9.txt"
		destination = source
	}
	if err := os.WriteFile(filepath.Join(dir, source), []byte("before\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit("add", "--", source)
	runGit("commit", "-qm", "base")

	switch operation {
	case "rename":
		if err := os.Rename(filepath.Join(dir, source), filepath.Join(dir, destination)); err != nil {
			t.Fatal(err)
		}
	case "copy":
		if err := os.WriteFile(filepath.Join(dir, destination), []byte("before\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	case "modify":
		if err := os.WriteFile(filepath.Join(dir, source), []byte("after\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatalf("unknown operation %q", operation)
	}
	runGit("add", "-A")
	args := []string{"diff", "--cached"}
	if noPrefix {
		args = append(args, "--no-prefix")
	}
	switch operation {
	case "rename":
		args = append(args, "-M100%")
	case "copy":
		args = append(args, "--find-copies-harder", "-C100%")
	}
	patch := runGit(args...)
	if operation != "modify" && !strings.Contains(patch, operation+" from ") {
		t.Fatalf("git did not produce %s metadata:\n%s", operation, patch)
	}
	return patch, source, destination
}
