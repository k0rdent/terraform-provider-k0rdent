package repocheck

// What the testing matrix knows about the previous release: which tag it is,
// what its walkthrough sets, and what its state holds.
//
// The upgrade job (ci/upgrade-against-fake.sh) applies the previous release's
// examples/walkthrough with that release and plans it with this build. The
// recorder can't see into it, so an attribute a resource block sets there,
// directly, through a local, or inside a for expression, counts as an upgrade
// cell that directory covers. The reader doesn't evaluate `count`, so a block
// switched off in a directory still counts there.
//
// An attribute the previous release never stored has no upgrade to test. Its
// stored shape is the state schema snapshot for the version it declared
// (internal/provider/testdata/state-schemas/<suffix>/v<N>.json, as tagged),
// plus any attribute its state history lists in addedLater, which is added
// to a version's shape without a snapshot of its own.

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/provider"
)

const stateSchemaDir = "internal/provider/testdata/state-schemas"

// walkthroughSource prefixes a walkthrough directory among a cell's tests.
const walkthroughSource = "walkthrough "

type previousRelease struct {
	tag string
	// walkthrough is each directory's parsed files.
	walkthrough map[string][]*hclsyntax.Body
	// shapes is each resource suffix's stored attribute paths.
	shapes map[string]map[string]bool
	// addedLater is every path any of its state histories lists in
	// addedLater, for every resource: a path two resources share is taken as
	// stored by both, so the check asks for an upgrade test rather than
	// excusing one.
	addedLater map[string]bool
}

func git(t *testing.T, root string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", root}, args...)...).Output()
	if err != nil {
		msg := ""
		if ee, ok := err.(*exec.ExitError); ok {
			msg = string(ee.Stderr)
		}
		t.Fatalf("git %s: %v %s. The matrix reads the previous release from its tag: git fetch --tags",
			strings.Join(args, " "), err, msg)
	}
	return string(out)
}

var releaseTag = regexp.MustCompile(`^v(\d+)\.(\d+)\.(\d+)(?:-([0-9A-Za-z.-]+))?$`)

// releaseLess orders release tags the way ci/upgrade-against-fake.sh does: a
// release after its pre-releases, and rc.10 after rc.2.
func releaseLess(a, b string) bool {
	ma, mb := releaseTag.FindStringSubmatch(a), releaseTag.FindStringSubmatch(b)
	for i := 1; i <= 3; i++ {
		// The pattern only matches digits here.
		x, errX := strconv.Atoi(ma[i])
		y, errY := strconv.Atoi(mb[i])
		if errX == nil && errY == nil && x != y {
			return x < y
		}
	}
	if (ma[4] == "") != (mb[4] == "") {
		return ma[4] != ""
	}
	pa, pb := strings.Split(ma[4], "."), strings.Split(mb[4], ".")
	for i := 0; i < len(pa) && i < len(pb); i++ {
		x, errX := strconv.Atoi(pa[i])
		y, errY := strconv.Atoi(pb[i])
		switch {
		case errX == nil && errY == nil && x != y:
			return x < y
		case (errX == nil) != (errY == nil):
			return errX == nil
		case errX != nil && pa[i] != pb[i]:
			return pa[i] < pb[i]
		}
	}
	return len(pa) < len(pb)
}

// releaseLine is X.Y on a release-vX.Y.x branch, as the upgrade job works it
// out, and empty on main.
func releaseLine(t *testing.T, root string) string {
	t.Helper()
	branch := os.Getenv("GITHUB_BASE_REF")
	if branch == "" && os.Getenv("GITHUB_ACTIONS") == "true" {
		branch = os.Getenv("GITHUB_REF_NAME")
	}
	if branch == "" {
		// Locally: the release branch HEAD has commits from, if any.
		var lines []string
		for _, b := range strings.Fields(git(t, root, "for-each-ref", "--format=%(refname:short)", "refs/remotes/origin/release-v*.x")) {
			line := strings.TrimSuffix(strings.TrimPrefix(b, "origin/release-v"), ".x")
			base, err := exec.Command("git", "-C", root, "merge-base", "HEAD", b).Output()
			if err != nil {
				continue
			}
			count := strings.TrimSpace(git(t, root, "rev-list", "--count", "origin/main.."+strings.TrimSpace(string(base))))
			if count != "0" {
				lines = append(lines, line)
			}
		}
		if len(lines) > 1 {
			t.Fatalf("HEAD has commits from more than one release branch (%v). Set PREVIOUS", lines)
		}
		if len(lines) == 1 {
			return lines[0]
		}
		return ""
	}
	if m := regexp.MustCompile(`^release-v(\d+\.\d+)\.x$`).FindStringSubmatch(branch); m != nil {
		return m[1]
	}
	return ""
}

// previousTag is the release to upgrade from: PREVIOUS when set, as for the
// upgrade job, or else the newest release tag not on HEAD, up to the release
// line on a release branch. The upgrade job also skips a tag a registry
// doesn't serve yet, which needs the network, so for the day a release is
// still being published the two can differ.
func previousTag(t *testing.T, root string) string {
	t.Helper()
	if v := os.Getenv("PREVIOUS"); v != "" {
		return "v" + strings.TrimPrefix(v, "v")
	}
	onHead := map[string]bool{}
	for _, tag := range strings.Fields(git(t, root, "tag", "--points-at", "HEAD", "-l", "v*")) {
		onHead[tag] = true
	}
	line := releaseLine(t, root)
	var tags []string
	for _, tag := range strings.Fields(git(t, root, "tag", "-l", "v*")) {
		m := releaseTag.FindStringSubmatch(tag)
		if m == nil || onHead[tag] {
			continue
		}
		if line != "" && releaseLess("v"+line+".999999", "v"+m[1]+"."+m[2]+".0") {
			continue
		}
		tags = append(tags, tag)
	}
	if len(tags) == 0 {
		t.Fatal("no release tag to upgrade from. git fetch --tags, or set PREVIOUS")
	}
	sort.Slice(tags, func(i, j int) bool { return releaseLess(tags[i], tags[j]) })
	return tags[len(tags)-1]
}

func readPreviousRelease(t *testing.T, root string) previousRelease {
	t.Helper()
	tag := previousTag(t, root)
	p := previousRelease{tag: tag, walkthrough: map[string][]*hclsyntax.Body{},
		shapes: map[string]map[string]bool{}, addedLater: map[string]bool{}}

	for _, f := range strings.Fields(git(t, root, "ls-tree", "-r", "--name-only", tag, "examples/walkthrough")) {
		if !strings.HasSuffix(f, ".tf") {
			continue
		}
		parsed, diags := hclsyntax.ParseConfig([]byte(git(t, root, "show", tag+":"+f)), f, hcl.InitialPos)
		if diags.HasErrors() {
			t.Fatalf("%s at %s: %v", f, tag, diags)
		}
		dir := strings.Split(f, "/")[2]
		body, ok := parsed.Body.(*hclsyntax.Body)
		if !ok {
			t.Fatalf("%s at %s: a body of type %T", f, tag, parsed.Body)
		}
		p.walkthrough[dir] = append(p.walkthrough[dir], body)
	}

	// The highest version each resource has a snapshot for is the version
	// the release declared.
	newest := map[string]int{}
	for _, f := range strings.Fields(git(t, root, "ls-tree", "-r", "--name-only", tag, stateSchemaDir)) {
		parts := strings.Split(strings.TrimPrefix(f, stateSchemaDir+"/"), "/")
		if len(parts) != 2 || !strings.HasPrefix(parts[1], "v") || !strings.HasSuffix(parts[1], ".json") {
			continue
		}
		v, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(parts[1], "v"), ".json"))
		if err != nil {
			continue
		}
		if cur, ok := newest[parts[0]]; !ok || v > cur {
			newest[parts[0]] = v
		}
	}
	for suffix, v := range newest {
		var typ any
		file := stateSchemaDir + "/" + suffix + "/v" + strconv.Itoa(v) + ".json"
		if err := json.Unmarshal([]byte(git(t, root, "show", tag+":"+file)), &typ); err != nil {
			t.Fatalf("%s at %s: %v", file, tag, err)
		}
		paths := map[string]bool{}
		stateTypePaths(typ, "", paths)
		p.shapes[suffix] = paths
	}

	history := "internal/provider/state_history.go"
	if strings.TrimSpace(git(t, root, "ls-tree", "--name-only", tag, history)) != "" {
		file, err := parser.ParseFile(token.NewFileSet(), history, git(t, root, "show", tag+":"+history), 0)
		if err != nil {
			t.Fatalf("%s at %s: %v", history, tag, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			kv, ok := n.(*ast.KeyValueExpr)
			if !ok {
				return true
			}
			if id, ok := kv.Key.(*ast.Ident); ok && id.Name == "addedLater" {
				if lit, ok := kv.Value.(*ast.CompositeLit); ok {
					for _, el := range lit.Elts {
						if e, ok := el.(*ast.KeyValueExpr); ok {
							if s, ok := e.Key.(*ast.BasicLit); ok {
								if path, err := strconv.Unquote(s.Value); err == nil {
									p.addedLater[path] = true
								}
							}
						}
					}
				}
			}
			return true
		})
	}
	return p
}

// stateTypePaths collects the attribute paths in a type written in
// Terraform's type notation, as the snapshots are: ["object", {...}], and a
// list, set or map of objects, whose attributes take the collection's name.
func stateTypePaths(typ any, prefix string, out map[string]bool) {
	l, ok := typ.([]any)
	if !ok || len(l) != 2 {
		return
	}
	switch l[0] {
	case "object":
		attrs, ok := l[1].(map[string]any)
		if !ok {
			return
		}
		for name, sub := range attrs {
			out[prefix+name] = true
			stateTypePaths(sub, prefix+name+".", out)
		}
	case "list", "set", "map":
		stateTypePaths(l[1], prefix, out)
	}
}

// stored says whether the previous release's state holds the attribute.
func (p previousRelease) stored(resourceType, path string) bool {
	shape := p.shapes[strings.TrimPrefix(resourceType, provider.ResourcePrefix())]
	return shape[path] || (shape != nil && p.addedLater[path])
}

// walkthroughCoverage is the upgrade cells the previous release's walkthrough
// directories cover for resourceType, named "walkthrough <dir>".
func (p previousRelease) walkthroughCoverage(resourceType string, attrs []matrixAttr) map[cellKey][]string {
	known := map[string]bool{}
	for _, a := range attrs {
		known[a.Path] = true
	}
	cov := map[cellKey][]string{}
	for dir, bodies := range p.walkthrough {
		locals := map[string]hclsyntax.Expression{}
		for _, b := range bodies {
			for _, blk := range b.Blocks {
				if blk.Type == "locals" {
					for name, a := range blk.Body.Attributes {
						locals[name] = a.Expr
					}
				}
			}
		}
		set := map[string]bool{}
		var expr func(e hclsyntax.Expression, prefix string, depth int)
		attr := func(path string, e hclsyntax.Expression, depth int) {
			if !known[path] {
				return
			}
			set[path] = true
			expr(e, path+".", depth)
		}
		expr = func(e hclsyntax.Expression, prefix string, depth int) {
			if depth > 8 {
				return
			}
			switch e := e.(type) {
			case *hclsyntax.ObjectConsExpr:
				for _, item := range e.Items {
					key := ""
					if k, ok := item.KeyExpr.(*hclsyntax.ObjectConsKeyExpr); ok {
						key = hcl.ExprAsKeyword(k.Wrapped)
						if key == "" {
							if v, diags := k.Wrapped.Value(nil); !diags.HasErrors() && v.Type().FriendlyName() == "string" {
								key = v.AsString()
							}
						}
					}
					if key != "" && known[prefix+key] {
						attr(prefix+key, item.ValueExpr, depth+1)
					} else {
						expr(item.ValueExpr, prefix, depth+1) // a map key, so the value is the element
					}
				}
			case *hclsyntax.TupleConsExpr:
				for _, el := range e.Exprs {
					expr(el, prefix, depth+1)
				}
			case *hclsyntax.ForExpr:
				expr(e.ValExpr, prefix, depth+1)
			case *hclsyntax.ConditionalExpr:
				expr(e.TrueResult, prefix, depth+1)
				expr(e.FalseResult, prefix, depth+1)
			case *hclsyntax.ScopeTraversalExpr:
				if e.Traversal.RootName() == "local" && len(e.Traversal) == 2 {
					if a, ok := e.Traversal[1].(hcl.TraverseAttr); ok && locals[a.Name] != nil {
						expr(locals[a.Name], prefix, depth+1)
					}
				}
			}
		}
		var body func(b *hclsyntax.Body, prefix string)
		body = func(b *hclsyntax.Body, prefix string) {
			for name, a := range b.Attributes {
				attr(prefix+name, a.Expr, 0)
			}
			for _, blk := range b.Blocks {
				body(blk.Body, prefix+blk.Type+".")
			}
		}
		for _, b := range bodies {
			for _, blk := range b.Blocks {
				if blk.Type == "resource" && len(blk.Labels) == 2 && blk.Labels[0] == resourceType {
					body(blk.Body, "")
				}
			}
		}
		if len(set) == 0 {
			continue
		}
		// What state holds without the configuration naming it: the
		// computed attributes, and region and project from the provider.
		// A nested one only where its parent is set. attrs is sorted, so a
		// parent comes before its attributes.
		for _, a := range attrs {
			parent, nested := "", false
			if i := strings.LastIndex(a.Path, "."); i >= 0 {
				parent, nested = a.Path[:i], true
			}
			switch {
			case a.Kind == "computed" && (!nested || set[parent]):
				set[a.Path] = true
			case !nested && (a.Path == "region" || a.Path == "project"):
				set[a.Path] = true
			}
		}
		for path := range set {
			k := cellKey{resourceType, path, cellUpgrade}
			cov[k] = append(cov[k], walkthroughSource+dir)
		}
	}
	return cov
}
