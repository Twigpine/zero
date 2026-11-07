package tui

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// pointerReceiverMutators are methods on model that MUTATE through a pointer
// receiver and are called for their effect on the model as much as for their
// return value. Add one here when you write it.
var pointerReceiverMutators = map[string]bool{
	"ensureSpinnerTick": true,
}

// A RETURN MUST NOT BOTH RETURN A MODEL AND MUTATE IT IN THE SAME STATEMENT.
//
// `return m, m.ensureSpinnerTick()` reads as though the call runs first. Go does
// not promise that: the spec orders function calls among THEMSELVES left to
// right, but leaves the order of a plain operand relative to a call operand
// unspecified. If m is copied first, the returned model carries the old
// spinnerTicking and the flag stops suppressing anything — every later hover
// issues another spinner.Tick, which is the precise double-issue the flag was
// added to prevent. It went unnoticed because the compiler happens to pick the
// helpful order today, at five separate call sites.
//
// The fix at every site is one line: call, assign, then return.
func TestNoReturnMutatesTheModelItAlsoReturns(t *testing.T) {
	sources, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("list the tui sources: %v", err)
	}
	fileSet := token.NewFileSet()

	for _, path := range sources {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fileSet, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		ast.Inspect(file, func(node ast.Node) bool {
			ret, ok := node.(*ast.ReturnStmt)
			if !ok || len(ret.Results) < 2 {
				return true
			}
			for i, result := range ret.Results {
				returned, ok := result.(*ast.Ident)
				if !ok {
					continue
				}
				for j, other := range ret.Results {
					if i == j {
						continue
					}
					if method := mutatingCallOn(other, returned.Name); method != "" {
						t.Errorf("%s:%d: `return %s, ... %s.%s() ...` mutates %s through a pointer receiver in the same statement that returns it, and Go does not specify which happens first. Assign the call to a local, then return.",
							filepath.Base(path), fileSet.Position(ret.Pos()).Line,
							returned.Name, returned.Name, method, returned.Name)
					}
				}
			}
			return true
		})
	}
}

// mutatingCallOn reports the method name when expr contains a call to one of the
// pointer-receiver mutators on the variable named receiver, at any depth — the
// call is often wrapped, as in tea.Batch(cmd, m.ensureSpinnerTick()).
func mutatingCallOn(expr ast.Expr, receiver string) string {
	found := ""
	ast.Inspect(expr, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || !pointerReceiverMutators[selector.Sel.Name] {
			return true
		}
		if ident, ok := selector.X.(*ast.Ident); ok && ident.Name == receiver {
			found = selector.Sel.Name
			return false
		}
		return true
	})
	return found
}
