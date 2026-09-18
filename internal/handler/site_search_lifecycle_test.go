package handler

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"testing"
)

func TestSiteSearchEnqueueTransactionalPlacementAndFailureGuards(t *testing.T) {
	source, err := os.ReadFile("site.go")
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := parser.ParseFile(token.NewFileSet(), "site.go", source, 0)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		function       string
		operation      string
		orderedCalls   []string
		compensationFn string
	}{
		{
			// The primitive, not the route. createSite is now a thin wrapper
			// that resolves the namespace and calls this; the owner-qualified
			// route for team sites calls the same one, so asserting here
			// covers both and cannot be satisfied by only one of them.
			function:       "createSiteForTarget",
			operation:      "SiteSearchReconcile",
			orderedCalls:   []string{"SetCurrentVersion", "EnqueueSiteSearch", "Commit"},
			compensationFn: "compensatePublishedCreate",
		},
		{
			function:       "updateSite",
			operation:      "SiteSearchReconcile",
			orderedCalls:   []string{"SetCurrentVersion", "EnqueueSiteSearch", "Commit"},
			compensationFn: "compensatePublishedUpdate",
		},
		{
			function:       "deleteSiteForTarget",
			operation:      "SiteSearchDelete",
			orderedCalls:   []string{"HideCurrent", "EnqueueSiteSearch", "DeleteSite", "Commit"},
			compensationFn: "RestoreCurrent",
		},
		{
			function:       "rollbackSite",
			operation:      "SiteSearchReconcile",
			orderedCalls:   []string{"SetCurrentVersion", "EnqueueSiteSearch", "Commit"},
			compensationFn: "restorePriorCurrent",
		},
	}

	for _, test := range tests {
		t.Run(test.function, func(t *testing.T) {
			function := findSiteHandlerFunction(t, parsed, test.function)
			positions := make(map[string]token.Pos)
			ast.Inspect(function.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				name := calledFunctionName(call)
				for _, required := range append(append([]string(nil), test.orderedCalls...), test.compensationFn) {
					if name == required {
						if prior, exists := positions[required]; !exists || call.Pos() < prior {
							positions[required] = call.Pos()
						}
					}
				}
				return true
			})

			var prior token.Pos
			for _, call := range test.orderedCalls {
				position := positions[call]
				if position == token.NoPos {
					t.Fatalf("%s has no %s call", test.function, call)
				}
				if prior != token.NoPos && position <= prior {
					t.Fatalf("%s call order is not %v", test.function, test.orderedCalls)
				}
				prior = position
			}
			if compensation := positions[test.compensationFn]; compensation == token.NoPos || compensation >= positions["EnqueueSiteSearch"] {
				t.Fatalf("%s does not install %s compensation before enqueue", test.function, test.compensationFn)
			}

			enqueueGuard := findEnqueueGuard(t, function)
			if len(enqueueGuard.Init.(*ast.AssignStmt).Rhs) != 1 {
				t.Fatal("enqueue guard has unexpected assignment shape")
			}
			call := enqueueGuard.Init.(*ast.AssignStmt).Rhs[0].(*ast.CallExpr)
			if len(call.Args) != 4 {
				t.Fatalf("enqueue arguments = %d, want 4", len(call.Args))
			}
			transaction, ok := call.Args[1].(*ast.Ident)
			if !ok || transaction.Name != "tx" {
				t.Fatalf("enqueue does not use the authoritative transaction: %#v", call.Args[1])
			}
			operation, ok := call.Args[3].(*ast.SelectorExpr)
			if !ok || operation.Sel.Name != test.operation {
				t.Fatalf("enqueue operation = %#v, want %s", call.Args[3], test.operation)
			}
			if !containsReturn(enqueueGuard.Body) {
				t.Fatal("enqueue failure guard does not return before commit")
			}
		})
	}

	visibility := findSiteHandlerFunction(t, parsed, "setSiteVisibility")
	ast.Inspect(visibility.Body, func(node ast.Node) bool {
		if call, ok := node.(*ast.CallExpr); ok && calledFunctionName(call) == "EnqueueSiteSearch" {
			t.Fatal("visibility update unexpectedly enqueues site search")
		}
		return true
	})
}

func findSiteHandlerFunction(t *testing.T, file *ast.File, name string) *ast.FuncDecl {
	t.Helper()
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if ok && function.Name.Name == name {
			return function
		}
	}
	t.Fatalf("function %s not found", name)
	return nil
}

func findEnqueueGuard(t *testing.T, function *ast.FuncDecl) *ast.IfStmt {
	t.Helper()
	var found *ast.IfStmt
	for _, statement := range function.Body.List {
		guard, ok := statement.(*ast.IfStmt)
		if !ok || guard.Init == nil {
			continue
		}
		assignment, ok := guard.Init.(*ast.AssignStmt)
		if !ok || len(assignment.Rhs) != 1 {
			continue
		}
		call, ok := assignment.Rhs[0].(*ast.CallExpr)
		if ok && calledFunctionName(call) == "EnqueueSiteSearch" {
			if found != nil {
				t.Fatalf("%s has more than one enqueue guard", function.Name.Name)
			}
			found = guard
		}
	}
	if found == nil {
		t.Fatalf("%s has no guarded enqueue", function.Name.Name)
	}
	return found
}

func calledFunctionName(call *ast.CallExpr) string {
	switch function := call.Fun.(type) {
	case *ast.Ident:
		return function.Name
	case *ast.SelectorExpr:
		return function.Sel.Name
	default:
		return ""
	}
}

func containsReturn(block *ast.BlockStmt) bool {
	found := false
	ast.Inspect(block, func(node ast.Node) bool {
		if _, ok := node.(*ast.ReturnStmt); ok {
			found = true
			return false
		}
		return !found
	})
	return found
}

// Every namespace-scoped write must reach the shared primitive rather than
// growing its own copy of the publish-and-index sequence. That is exactly how
// search indexing would quietly go missing for team sites: the route works,
// the site serves, and nothing is ever indexed.
func TestNamespaceRoutesDelegateToTheSharedPrimitives(t *testing.T) {
	fileSet := token.NewFileSet()
	site, err := parser.ParseFile(fileSet, "site.go", mustRead(t, "site.go"), 0)
	if err != nil {
		t.Fatal(err)
	}
	collaboration, err := parser.ParseFile(fileSet, "collaboration.go", mustRead(t, "collaboration.go"), 0)
	if err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		file      *ast.File
		function  string
		delegates string
	}{
		{site, "createSite", "createSiteForTarget"},
		{site, "deleteSite", "deleteSiteForTarget"},
		{site, "setSiteVisibility", "setSiteVisibilityForTarget"},
		{collaboration, "createCollaborationSite", "createSiteForTarget"},
		{collaboration, "deleteCollaborationSite", "deleteSiteForTarget"},
		{collaboration, "setCollaborationSiteVisibility", "setSiteVisibilityForTarget"},
	} {
		t.Run(test.function, func(t *testing.T) {
			function := findSiteHandlerFunction(t, test.file, test.function)
			found := false
			ast.Inspect(function.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				if calledFunctionName(call) == test.delegates {
					found = true
				}
				return true
			})
			if !found {
				t.Errorf("%s does not call %s; a second copy of the publish-and-index sequence is how indexing goes missing", test.function, test.delegates)
			}
		})
	}
}

func mustRead(t *testing.T, name string) []byte {
	t.Helper()
	source, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return source
}
