package provider

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"sort"
	"strings"
	"testing"
)

// TestEveryReplacementPlannedInCodeIsGuarded covers what the schema walk in
// TestEveryReplaceableResourceIsGuardedOrSaysWhy can't see: a replacement a
// resource adds to ModifyPlan's RequiresReplace in code rather than through
// an attribute's modifier. Reading the type-checked source, every write to
// that field has to be one of
//
//   - the scope's, modifyScopePlan or the project's planRegion, whose paths
//     are region and project, which ReplacingAttributes already counts;
//   - the guard's own, which passes the resource's answer through;
//   - code that only a guarded resource's ModifyPlan reaches, where the guard
//     reads the path back and refuses it.
//
// So an unguarded resource can't plan a replacement of anything else without
// failing here, and being told to guard it.
func TestEveryReplacementPlannedInCodeIsGuarded(t *testing.T) {
	sites, offenders := codedReplacements(checkedProvider(t))
	t.Logf("writes to RequiresReplace checked: %d", sites)
	if sites == 0 {
		t.Fatal("found no write to RequiresReplace at all, so this test is reading the wrong source")
	}
	if len(offenders) > 0 {
		t.Errorf("a replacement planned in code outside the scope's has to come from a resource "+
			"registered through guardReplacements:\n  %s", strings.Join(offenders, "\n  "))
	}
}

// scopeReplacements are the functions allowed to plan a replacement on any
// resource, because the paths they add are region and project.
var scopeReplacements = map[string]bool{"modifyScopePlan": true, "*projectResource.planRegion": true}

func codedReplacements(c checkedPackage) (int, []string) {
	// The types handed to guardReplacements as &T{...}.
	guarded := map[*types.TypeName]bool{}
	// Who mentions each function in this package.
	mentions := map[types.Object]map[types.Object]bool{}
	type site struct {
		owner types.Object
		at    token.Pos
	}
	var sites []site

	inspect := func(owner types.Object, root ast.Node) {
		ast.Inspect(root, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.Ident:
				obj := c.info.Uses[n]
				if f, ok := obj.(*types.Func); ok {
					obj = f.Origin()
				}
				if obj != nil && obj.Pkg() == c.pkg && (isFunc(obj) || obj.Parent() == c.pkg.Scope()) {
					if mentions[obj] == nil {
						mentions[obj] = map[types.Object]bool{}
					}
					mentions[obj][owner] = true
				}
			case *ast.CallExpr:
				if id, ok := n.Fun.(*ast.Ident); ok && id.Name == "guardReplacements" && len(n.Args) > 0 {
					if u, ok := n.Args[0].(*ast.UnaryExpr); ok && u.Op == token.AND {
						if named, ok := c.info.TypeOf(u.X).(*types.Named); ok {
							guarded[named.Obj()] = true
						}
					}
				}
			case *ast.AssignStmt:
				for _, lhs := range n.Lhs {
					if writesRequiresReplace(c.info, lhs) {
						sites = append(sites, site{owner, lhs.Pos()})
					}
				}
			}
			return true
		})
	}

	for _, f := range c.files {
		for _, decl := range f.Decls {
			for owner, body := range ownedBodies(c, decl) {
				inspect(owner, body)
			}
		}
	}

	// reachedOnlyFromGuarded says whether every way into obj starts at the
	// ModifyPlan of a guarded type, or at the guard itself.
	var reachedOnlyFromGuarded func(obj types.Object, seen map[types.Object]bool) bool
	reachedOnlyFromGuarded = func(obj types.Object, seen map[types.Object]bool) bool {
		if recv := receiverOf(obj); recv != nil && (recv.Name() == "guardedResource" ||
			(guarded[recv] && obj.Name() == "ModifyPlan")) {
			return true
		}
		if seen[obj] || len(mentions[obj]) == 0 {
			return false
		}
		seen[obj] = true
		for from := range mentions[obj] {
			if !reachedOnlyFromGuarded(from, seen) {
				return false
			}
		}
		return true
	}

	var offenders []string
	for _, s := range sites {
		if scopeReplacements[describe(s.owner)] || reachedOnlyFromGuarded(s.owner, map[types.Object]bool{}) {
			continue
		}
		offenders = append(offenders, fmt.Sprintf("%s: in %s", c.fset.Position(s.at), describe(s.owner)))
	}
	sort.Strings(offenders)
	return len(sites), offenders
}

// writesRequiresReplace says whether lhs is a ModifyPlanResponse's
// RequiresReplace. An attribute modifier's response has a field of the same
// name, a bool, which is a different thing.
func writesRequiresReplace(info *types.Info, lhs ast.Expr) bool {
	sel, ok := ast.Unparen(lhs).(*ast.SelectorExpr)
	return ok && sel.Sel.Name == "RequiresReplace" && isNamed(info.TypeOf(sel.X), frameworkResource, "ModifyPlanResponse")
}

func receiverOf(obj types.Object) *types.TypeName {
	fn, ok := obj.(*types.Func)
	if !ok || fn.Signature().Recv() == nil {
		return nil
	}
	t := fn.Signature().Recv().Type()
	if p, ok := t.(*types.Pointer); ok {
		t = p.Elem()
	}
	if named, ok := t.(*types.Named); ok {
		return named.Obj()
	}
	return nil
}

// ownedBodies is each function body in decl, and each value of a
// package-level variable, by what owns it: a function literal stored in a
// variable runs wherever the variable is mentioned.
func ownedBodies(c checkedPackage, decl ast.Decl) map[types.Object]ast.Node {
	out := map[types.Object]ast.Node{}
	switch decl := decl.(type) {
	case *ast.FuncDecl:
		if decl.Body != nil {
			out[c.info.Defs[decl.Name]] = decl.Body
		}
	case *ast.GenDecl:
		for _, spec := range decl.Specs {
			if vs, ok := spec.(*ast.ValueSpec); ok {
				for i, value := range vs.Values {
					out[c.info.Defs[vs.Names[min(i, len(vs.Names)-1)]]] = value
				}
			}
		}
	}
	return out
}
