package cli

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/sandbox"
)

// AN INHERITED OVERRIDE DOES NOT REACH AROUND THE ISOLATION.
//
// ZERO_SANDBOX_GRANTS_PATH and ZERO_WINDOWS_SANDBOX_HOME outrank the base
// directories the helper redirects. A developer who has either set would
// otherwise have the doctor tests read the setup marker from, and save
// capability entries into, their real sandbox home, and get results that
// depend on that machine. Both resolvers must land under the fixture root
// whatever the test process inherited.
func TestIsolateCLIUserStateNeutralizesSandboxOverrides(t *testing.T) {
	outside := t.TempDir()
	t.Setenv("ZERO_SANDBOX_GRANTS_PATH", filepath.Join(outside, "grants.json"))
	t.Setenv("ZERO_WINDOWS_SANDBOX_HOME", filepath.Join(outside, "sandbox-home"))

	isolateCLIUserState(t)

	fixtureRoot := filepath.Clean(os.Getenv("XDG_CONFIG_HOME"))
	if fixtureRoot == "." || fixtureRoot == filepath.Clean(outside) {
		t.Fatalf("SETUP INVALID: the helper left XDG_CONFIG_HOME at %q", fixtureRoot)
	}
	grants, err := sandbox.ResolveGrantPath(nil)
	if err != nil {
		t.Fatal(err)
	}
	home, err := sandbox.ResolveWindowsSandboxHome(nil)
	if err != nil {
		t.Fatal(err)
	}
	for name, resolved := range map[string]string{"sandbox grants path": grants, "Windows sandbox home": home} {
		if !strings.HasPrefix(filepath.Clean(resolved), fixtureRoot+string(os.PathSeparator)) {
			t.Errorf("%s = %s, want it under the fixture root %s; an inherited override still decides it", name, resolved, fixtureRoot)
		}
	}
}

// THE DOCTOR COMMAND WRITES PER-USER STATE, SO EVERY TEST THAT RUNS IT ISOLATES.
//
// On Windows `zero doctor` resolves the sandbox home from the process
// environment and validates the setup marker for the directory it runs in.
// Building the expected marker asks for that directory's capability SID, and a
// directory the sandbox home has not seen gets one generated and saved. A test
// that runs doctor from a fresh temp directory without redirecting the
// per-user roots therefore appended one entry to the developer's real
// windows-cap-sids.json on every run: six tests here did, and a machine that
// ran this suite a few hundred times carried thousands of them.
//
// isolateCLIUserState moves every per-user root under the test's own temp
// directory, which is where that file then lands. This guard reads the test
// sources instead of watching the real file, because a test must not depend
// on, or look at, the developer's home to pass.
func TestDoctorTestsIsolateUserState(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fileSet := token.NewFileSet()
	runsDoctor := 0
	var missing []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fileSet, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Body == nil {
				continue
			}
			doctor, isolated := false, false
			ast.Inspect(function.Body, func(node ast.Node) bool {
				switch typed := node.(type) {
				case *ast.CompositeLit:
					if startsWithDoctor(typed) {
						doctor = true
					}
				case *ast.CallExpr:
					if callee, ok := typed.Fun.(*ast.Ident); ok && callee.Name == "isolateCLIUserState" {
						isolated = true
					}
				}
				return true
			})
			if !doctor {
				continue
			}
			runsDoctor++
			if !isolated {
				missing = append(missing, name+": "+function.Name.Name)
			}
		}
	}
	// A guard that finds nothing to check proves nothing: the argument shape it
	// looks for may have changed.
	if runsDoctor == 0 {
		t.Fatal("SETUP INVALID: found no test that passes a doctor argument list; update startsWithDoctor")
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Fatalf("%d of %d functions run `zero doctor` without isolateCLIUserState, so they write the developer's real per-user state:\n  %s",
			len(missing), runsDoctor, strings.Join(missing, "\n  "))
	}
}

// startsWithDoctor reports an argument list whose first word is the top-level
// doctor command: []string{"doctor", ...}, or a row of a [][]string table.
// "backends doctor" is a different command and starts with another word.
func startsWithDoctor(literal *ast.CompositeLit) bool {
	if len(literal.Elts) == 0 {
		return false
	}
	first, ok := literal.Elts[0].(*ast.BasicLit)
	if !ok || first.Kind != token.STRING {
		return false
	}
	value, err := strconv.Unquote(first.Value)
	return err == nil && value == "doctor"
}
