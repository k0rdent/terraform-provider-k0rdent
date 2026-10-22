package provider

import (
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
)

// TestAFailedWaitKeepsEveryKindInState covers a failed create wait. This covers every path
// at once, an update or a read included, by reading the type-checked source:
//
//   - a DELETE is sent only from code that a resource's Delete alone reaches,
//     and a cluster cancel only from code that a Delete or an action's Invoke
//     alone reaches. An action runs only when a configuration invokes it by
//     name, so the cancel action is the user asking, as a destroy is,
//   - a request body that empties a list with a literal, the way a destroy
//     detaches a file store's consumers or a VPC's security groups, is sent
//     only from that same code,
//   - nothing in the package calls a resource's Delete but another Delete,
//     the replacement guard's handing the destroy on, so Terraform's destroy
//     is the only way in,
//   - a resource leaves state, by RemoveResource only from code its Read
//     alone reaches, and by assigning its state or the state's Raw only from
//     code its Read, Create or Delete alone reaches.
//
// So no wait, failed or not, can reach any of them from an update. What it
// can't see is a list emptied by a value built at run time, such as a plan
// with no consumers, because that's also how an update rightly detaches.
func TestOnlyADestroyDeletesOrCancels(t *testing.T) {
	found, offenders := destroyRule(checkedProvider(t))
	t.Logf("sites checked: %v", found)
	for _, want := range []string{"a delete or cancel", "a literal empty list"} {
		if found[want] == 0 {
			t.Fatalf("found no %s at all, so this test is reading the wrong source", want)
		}
	}
	if len(offenders) > 0 {
		t.Errorf("only a destroy may delete, cancel or detach everything, nothing may call a "+
			"resource's Delete, and only a read, create or destroy may drop a resource from state:\n  %s",
			strings.Join(offenders, "\n  "))
	}
}

const (
	clientPackage     = "github.com/k0rdent/terraform-provider-k0rdent/internal/k0rdent"
	frameworkResource = "github.com/hashicorp/terraform-plugin-framework/resource"
	frameworkState    = "github.com/hashicorp/terraform-plugin-framework/tfsdk"
	frameworkAction   = "github.com/hashicorp/terraform-plugin-framework/action"
)

// destructiveClientCall is every generated client function that sends a
// DELETE or a cancel, or builds the request for one.
var destructiveClientCall = regexp.MustCompile(`^(New)?(Delete|CancelCluster)\w*$`)

// cancelClientCall is the part of destructiveClientCall that cancels rather
// than deletes, which an action may reach too.
var cancelClientCall = regexp.MustCompile(`^(New)?CancelCluster\w*$`)

// checkedPackage is this package's non-test source, type-checked, so a name
// is matched on what it refers to rather than on how it's spelled.
type checkedPackage struct {
	fset  *token.FileSet
	files []*ast.File
	info  *types.Info
	pkg   *types.Package
}

// checkedProvider type-checks every non-test file in this package against the
// export data `go list` builds for its imports.
func checkedProvider(t *testing.T) checkedPackage {
	t.Helper()
	out, err := exec.Command("go", "list", "-export", "-deps",
		"-f", "{{if .Export}}{{.ImportPath}}={{.Export}}{{end}}", ".").Output()
	if err != nil {
		t.Fatalf("list the export data for this package's imports: %v", err)
	}
	exports := map[string]string{}
	for line := range strings.Lines(string(out)) {
		if importPath, file, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			exports[importPath] = file
		}
	}

	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	c := checkedPackage{fset: token.NewFileSet()}
	for _, p := range paths {
		if strings.HasSuffix(p, "_test.go") {
			continue
		}
		f, parseErr := parser.ParseFile(c.fset, p, nil, 0)
		if parseErr != nil {
			t.Fatal(parseErr)
		}
		c.files = append(c.files, f)
	}
	c.info = &types.Info{
		Types: map[ast.Expr]types.TypeAndValue{},
		Defs:  map[*ast.Ident]types.Object{},
		Uses:  map[*ast.Ident]types.Object{},
	}
	conf := types.Config{Importer: importer.ForCompiler(c.fset, "gc", func(importPath string) (io.ReadCloser, error) {
		file, ok := exports[importPath]
		if !ok {
			return nil, fmt.Errorf("go list built no export data for %s", importPath)
		}
		return os.Open(file) //nolint:gosec // the path comes from go list
	})}
	c.pkg, err = conf.Check("github.com/k0rdent/terraform-provider-k0rdent/internal/provider", c.fset, c.files, c.info)
	if err != nil {
		t.Fatalf("type-check this package: %v", err)
	}
	return c
}

// entryPoint names the framework method fn is, such as "Delete" for a
// resource's Delete, by its receiver and request type rather than by its
// name alone, or answers "" when it is no such method.
func entryPoint(obj types.Object) string {
	fn, ok := obj.(*types.Func)
	if !ok {
		return ""
	}
	sig := fn.Signature()
	if sig.Recv() != nil && translatesState(fn.Name(), sig) {
		return fn.Name()
	}
	if sig.Recv() == nil || sig.Params().Len() != 3 {
		return ""
	}
	if fn.Name() == "Invoke" && isNamed(sig.Params().At(1).Type(), frameworkAction, "InvokeRequest") {
		return "Invoke"
	}
	if !isNamed(sig.Params().At(1).Type(), frameworkResource, fn.Name()+"Request") {
		return ""
	}
	return fn.Name()
}

// translatesState says whether a method is a resource's UpgradeState or
// MoveState. Those hand the framework the functions that carry an older or
// another type's state over, so the functions run from them and nowhere else.
func translatesState(name string, sig *types.Signature) bool {
	if sig.Results().Len() != 1 {
		return false
	}
	switch t := sig.Results().At(0).Type().(type) {
	case *types.Map:
		return name == "UpgradeState" && isNamed(t.Elem(), frameworkResource, "StateUpgrader")
	case *types.Slice:
		return name == "MoveState" && isNamed(t.Elem(), frameworkResource, "StateMover")
	}
	return false
}

func isNamed(t types.Type, pkg, name string) bool {
	if p, ok := t.(*types.Pointer); ok {
		t = p.Elem()
	}
	named, ok := t.(*types.Named)
	return ok && named.Obj().Pkg() != nil && named.Obj().Pkg().Path() == pkg && named.Obj().Name() == name
}

// callGraph maps each function, and each package-level variable, to the
// functions and variables that mention it. A variable counts because a
// function literal stored in one runs wherever the variable is mentioned.
type callGraph map[types.Object]map[types.Object]bool

// reachedOnlyFrom says whether every way into obj starts at one of the named
// entry points. Anything nothing mentions, an Update for one, isn't.
func (g callGraph) reachedOnlyFrom(obj types.Object, entries ...string) bool {
	seen := map[types.Object]bool{}
	var walk func(types.Object) bool
	walk = func(n types.Object) bool {
		if slices.Contains(entries, entryPoint(n)) {
			return true
		}
		if seen[n] || len(g[n]) == 0 {
			return false
		}
		seen[n] = true
		for from := range g[n] {
			if !walk(from) {
				return false
			}
		}
		return true
	}
	return walk(obj)
}

// destroyRule walks the package once and answers how many of each kind of
// site it found, and every site outside what's allowed to reach it.
func destroyRule(c checkedPackage) (map[string]int, []string) {
	type site struct {
		kind    string
		owner   types.Object
		at      token.Pos
		allowed []string // nil means nowhere
	}
	graph := callGraph{}
	var sites []site

	visit := func(owner types.Object, root ast.Node) {
		// A DELETE named in a switch's case only classifies a request.
		inCase := map[*ast.Ident]bool{}
		ast.Inspect(root, func(n ast.Node) bool {
			if cc, ok := n.(*ast.CaseClause); ok {
				for _, e := range cc.List {
					ast.Inspect(e, func(n ast.Node) bool {
						if id, ok := n.(*ast.Ident); ok {
							inCase[id] = true
						}
						return true
					})
				}
			}
			return true
		})
		ast.Inspect(root, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.Ident:
				// Every mention counts, not only a call: a method value
				// handed to a wait would otherwise escape the rule.
				obj := c.info.Uses[n]
				if fn, ok := obj.(*types.Func); ok {
					obj = fn.Origin()
				}
				if obj == nil || obj.Pkg() == nil {
					return true
				}
				switch {
				case obj.Pkg() == c.pkg && (isFunc(obj) || obj.Parent() == c.pkg.Scope()):
					if graph[obj] == nil {
						graph[obj] = map[types.Object]bool{}
					}
					graph[obj][owner] = true
				case obj.Pkg().Path() == clientPackage && isFunc(obj) && destructiveClientCall.MatchString(obj.Name()):
					allowed := []string{"Delete"}
					if cancelClientCall.MatchString(obj.Name()) {
						allowed = append(allowed, "Invoke")
					}
					sites = append(sites, site{"a delete or cancel", owner, n.Pos(), allowed})
				case obj.Pkg().Path() == "net/http" && obj.Name() == "MethodDelete" && !inCase[n]:
					sites = append(sites, site{"a delete or cancel", owner, n.Pos(), []string{"Delete"}})
				case obj.Pkg().Path() == frameworkState && obj.Name() == "RemoveResource":
					sites = append(sites, site{"a removal from state", owner, n.Pos(), []string{"Read"}})
				}
				if entryPoint(obj) == "Delete" {
					// Only another Delete may call one: the replacement
					// guard's hands the destroy to the resource it wraps.
					sites = append(sites, site{"a call to a resource's Delete", owner, n.Pos(), []string{"Delete"}})
				}
			case *ast.AssignStmt:
				if n.Tok != token.ASSIGN {
					return true
				}
				for _, lhs := range n.Lhs {
					if writesState(c.info, lhs) {
						// A state upgrade and a moved block write the state
						// they translate. Neither can drop a live resource,
						// because the framework hands them one to carry over.
						sites = append(sites, site{"an assignment of state", owner, lhs.Pos(), []string{"Read", "Create", "Delete", "UpgradeState", "MoveState"}})
					}
				}
			case *ast.CompositeLit:
				if !isClientStruct(c.info.TypeOf(n)) {
					return true
				}
				for _, elt := range n.Elts {
					if kv, ok := elt.(*ast.KeyValueExpr); ok && isEmptyListLiteral(c.info, kv.Value) {
						sites = append(sites, site{"a literal empty list", owner, kv.Pos(), []string{"Delete"}})
					}
				}
			}
			return true
		})
	}

	for _, f := range c.files {
		for _, decl := range f.Decls {
			switch decl := decl.(type) {
			case *ast.FuncDecl:
				if decl.Body != nil {
					visit(c.info.Defs[decl.Name], decl.Body)
				}
			case *ast.GenDecl:
				for _, spec := range decl.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					for i, value := range vs.Values {
						name := vs.Names[min(i, len(vs.Names)-1)]
						visit(c.info.Defs[name], value)
					}
				}
			}
		}
	}

	found := map[string]int{}
	var offenders []string
	for _, s := range sites {
		found[s.kind]++
		if s.allowed != nil && graph.reachedOnlyFrom(s.owner, s.allowed...) {
			continue
		}
		offenders = append(offenders, fmt.Sprintf("%s: %s in %s", c.fset.Position(s.at), s.kind, describe(s.owner)))
	}
	sort.Strings(offenders)
	return found, offenders
}

func isFunc(obj types.Object) bool {
	_, ok := obj.(*types.Func)
	return ok
}

// writesState says whether lhs is a whole tfsdk.State, or its Raw, so that
// assigning to it can drop the resource.
func writesState(info *types.Info, lhs ast.Expr) bool {
	if isNamed(info.TypeOf(lhs), frameworkState, "State") {
		return true
	}
	sel, ok := ast.Unparen(lhs).(*ast.SelectorExpr)
	return ok && sel.Sel.Name == "Raw" && isNamed(info.TypeOf(sel.X), frameworkState, "State")
}

func isClientStruct(t types.Type) bool {
	if t == nil {
		return false
	}
	if p, ok := t.(*types.Pointer); ok {
		t = p.Elem()
	}
	named, ok := t.(*types.Named)
	if !ok || named.Obj().Pkg() == nil || named.Obj().Pkg().Path() != clientPackage {
		return false
	}
	_, ok = named.Underlying().(*types.Struct)
	return ok
}

// isEmptyListLiteral says whether e is a slice literal with nothing in it,
// or a pointer to one.
func isEmptyListLiteral(info *types.Info, e ast.Expr) bool {
	e = ast.Unparen(e)
	if u, ok := e.(*ast.UnaryExpr); ok && u.Op == token.AND {
		e = ast.Unparen(u.X)
	}
	lit, ok := e.(*ast.CompositeLit)
	if !ok || len(lit.Elts) > 0 {
		return false
	}
	_, ok = info.TypeOf(lit).Underlying().(*types.Slice)
	return ok
}

// describe names a function by its receiver type too, so two resources'
// Updates read apart.
func describe(obj types.Object) string {
	if fn, ok := obj.(*types.Func); ok {
		if recv := fn.Signature().Recv(); recv != nil {
			return types.TypeString(recv.Type(), types.RelativeTo(fn.Pkg())) + "." + fn.Name()
		}
	}
	return obj.Name()
}
