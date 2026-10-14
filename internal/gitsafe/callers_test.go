package gitsafe

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The packages whose git calls Zero makes on its own. The CLI runs git for the
// scratch-file warning and the TUI for its files sweep and pull request
// diffstat; each call has to come through Command.
var automaticGitPackages = []string{
	filepath.Join("..", "cli"),
	filepath.Join("..", "tui"),
}

// parsePackage parses the non-test Go files in dir.
func parsePackage(t *testing.T, dir string) (*token.FileSet, []*ast.File) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		files = append(files, file)
	}
	if len(files) == 0 {
		t.Fatalf("no Go files in %s", dir)
	}
	return fset, files
}

// importName is the name file uses for the package at path, or "" when the
// file does not import it.
func importName(file *ast.File, path string) string {
	for _, spec := range file.Imports {
		if value, err := strconv.Unquote(spec.Path.Value); err != nil || value != path {
			continue
		}
		if spec.Name != nil {
			return spec.Name.Name
		}
		return filepath.Base(path)
	}
	return ""
}

// directGitExecs lists the exec.Command and exec.CommandContext calls in files
// whose program is the literal "git".
func directGitExecs(fset *token.FileSet, files []*ast.File) []string {
	var found []string
	for _, file := range files {
		execName := importName(file, "os/exec")
		if execName == "" {
			continue
		}
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if pkg, ok := selector.X.(*ast.Ident); !ok || pkg.Name != execName {
				return true
			}
			program := 0
			switch selector.Sel.Name {
			case "Command":
			case "CommandContext":
				program = 1
			default:
				return true
			}
			if len(call.Args) <= program {
				return true
			}
			if literal, ok := call.Args[program].(*ast.BasicLit); ok && literal.Kind == token.STRING {
				if value, err := strconv.Unquote(literal.Value); err == nil && value == "git" {
					found = append(found, fset.Position(call.Pos()).String())
				}
			}
			return true
		})
	}
	return found
}

func TestAutomaticGitCallsDoNotExecGitDirectly(t *testing.T) {
	for _, dir := range automaticGitPackages {
		fset, files := parsePackage(t, dir)
		for _, position := range directGitExecs(fset, files) {
			t.Errorf("%s runs git directly; go through gitsafe.Command so the repository's config cannot make it run a program", position)
		}
	}
}

// gitArguments returns the git arguments of call when it is gitsafe.Command or a
// runner called with the program "git" (the TUI's runner(ctx, cwd, "git", ...)),
// as far as they are string literals.
func gitArguments(call *ast.CallExpr, gitsafeName string) ([]string, bool) {
	start := -1
	if selector, ok := call.Fun.(*ast.SelectorExpr); ok && gitsafeName != "" && selector.Sel.Name == "Command" {
		if pkg, ok := selector.X.(*ast.Ident); ok && pkg.Name == gitsafeName {
			start = 2
		}
	}
	if start < 0 {
		for index, argument := range call.Args {
			if literal, ok := argument.(*ast.BasicLit); ok && literal.Kind == token.STRING && literal.Value == `"git"` {
				start = index + 1
				break
			}
		}
	}
	if start < 0 || start > len(call.Args) {
		return nil, false
	}
	var arguments []string
	for _, argument := range call.Args[start:] {
		if literal, ok := argument.(*ast.BasicLit); ok && literal.Kind == token.STRING {
			if value, err := strconv.Unquote(literal.Value); err == nil {
				arguments = append(arguments, value)
				continue
			}
		}
		arguments = append(arguments, "")
	}
	return arguments, true
}

// A submodule has its own config, which Command does not check, so the
// automatic status and diff calls stay out of submodule work trees.
func TestAutomaticStatusAndDiffSkipSubmoduleWorkTrees(t *testing.T) {
	checked := 0
	for _, dir := range automaticGitPackages {
		fset, files := parsePackage(t, dir)
		for _, file := range files {
			gitsafeName := importName(file, "github.com/Gitlawb/zero/internal/gitsafe")
			ast.Inspect(file, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				arguments, ok := gitArguments(call, gitsafeName)
				if !ok || len(arguments) == 0 || (arguments[0] != "status" && arguments[0] != "diff") {
					return true
				}
				checked++
				for _, argument := range arguments {
					if argument == "--ignore-submodules=dirty" {
						return true
					}
				}
				t.Errorf("%s runs git %s without --ignore-submodules=dirty", fset.Position(call.Pos()), arguments[0])
				return true
			})
		}
	}
	// The scratch-file status, the files sweep's status and diff, and the pull
	// request diffstat.
	if checked != 4 {
		t.Fatalf("checked %d automatic status or diff calls, want 4; update this test when they change", checked)
	}
}

// The TUI runs every git command through one runner, whose program name is a
// parameter, so the check above cannot see it. The runner has to hand git to
// Command itself.
func TestTUICommandRunnerSendsGitThroughCommand(t *testing.T) {
	_, files := parsePackage(t, filepath.Join("..", "tui"))
	var runner *ast.FuncDecl
	var runnerFile *ast.File
	for _, file := range files {
		for _, declaration := range file.Decls {
			if function, ok := declaration.(*ast.FuncDecl); ok && function.Recv == nil && function.Name.Name == "defaultPRCommandRunner" {
				runner, runnerFile = function, file
			}
		}
	}
	if runner == nil {
		t.Fatal("defaultPRCommandRunner not found in internal/tui")
	}
	gitsafeName := importName(runnerFile, "github.com/Gitlawb/zero/internal/gitsafe")
	calls := false
	if gitsafeName != "" {
		ast.Inspect(runner.Body, func(node ast.Node) bool {
			if call, ok := node.(*ast.CallExpr); ok {
				if selector, ok := call.Fun.(*ast.SelectorExpr); ok && selector.Sel.Name == "Command" {
					if pkg, ok := selector.X.(*ast.Ident); ok && pkg.Name == gitsafeName {
						calls = true
					}
				}
			}
			return !calls
		})
	}
	if !calls {
		t.Fatal("defaultPRCommandRunner does not call gitsafe.Command, so the TUI's git calls skip it")
	}
}
