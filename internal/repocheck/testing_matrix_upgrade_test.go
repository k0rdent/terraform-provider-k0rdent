package repocheck

// Upgrade coverage: which upgrade cells the tests and the previous release's
// walkthrough cover. It isn't on docs/testing-matrix.md and fails nothing,
// since it depends on which release came before and not only on the commit.
// make upgrade-test is what gates upgrades. After the matrix check,
// make testacc writes this summary to a file, and in GitHub Actions to the
// job summary, as information.
//
// The previous release is the one make upgrade-test upgrades from, as
// ci/upgrade-against-fake.sh --previous prints it, so there's one rule.
//
// The upgrade job applies the previous release's examples/walkthrough with
// that release and plans it with this build. The recorder can't see into it,
// so an attribute a resource block sets there, directly, through a local, or
// inside a for expression, counts as an upgrade cell that directory covers.
// The reader doesn't evaluate `count`, so a block switched off in a directory
// still counts there.
//
// An attribute the previous release never stored has no upgrade to test. Its
// stored shape is the state schema snapshot for the version it declared
// (internal/provider/testdata/state-schemas/<suffix>/v<N>.json, as tagged),
// plus any attribute its state history lists in addedLater, which is added
// to a version's shape without a snapshot of its own.

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/provider"
)

var upgradeSummary = flag.String("upgrade-summary", "",
	"where make testacc writes upgrade coverage, as information. Needs -matrix-record")

const (
	stateSchemaDir = "internal/provider/testdata/state-schemas"
	// previousRule prints the release make upgrade-test upgrades from.
	previousRule = "ci/upgrade-against-fake.sh"
)

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
	// stored by both, so the summary counts it rather than leaving it out.
	addedLater map[string]bool
}

func git(root string, args ...string) (string, error) {
	out, err := exec.Command("git", append([]string{"-C", root}, args...)...).Output()
	if err != nil {
		msg := ""
		if ee, ok := err.(*exec.ExitError); ok {
			msg = strings.TrimSpace(string(ee.Stderr))
		}
		return "", fmt.Errorf("git %s: %v %s", strings.Join(args, " "), err, msg)
	}
	return string(out), nil
}

var releaseTag = regexp.MustCompile(`^v\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?$`)

// upgradeFrom runs the rule make upgrade-test uses, and returns the tag it
// picks and the releases it skipped as not yet published.
func upgradeFrom(root string) (tag string, skipped []string, err error) {
	return upgradeFromEnv(root, nil)
}

// upgradeFromEnv is upgradeFrom with env as the script's environment, or
// this process's when it's nil.
func upgradeFromEnv(root string, env []string) (tag string, skipped []string, err error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.Command("bash", filepath.Join(root, previousRule), "--previous")
	cmd.Env = env
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()
	var last string
	for _, l := range strings.Split(stderr.String(), "\n") {
		l = strings.TrimSpace(l)
		switch {
		case strings.HasPrefix(l, "::"):
		case strings.Contains(l, " is tagged but "):
			skipped = append(skipped, strings.TrimSpace(strings.TrimPrefix(l, "!")))
		case strings.HasPrefix(l, "upgrade: FAIL - "):
			last = strings.TrimPrefix(l, "upgrade: FAIL - ")
		case l != "" && last == "":
			last = strings.TrimSpace(strings.TrimPrefix(l, "✗"))
		}
	}
	if runErr != nil {
		if last == "" {
			last = runErr.Error()
		}
		return "", skipped, errors.New(last)
	}
	tag = "v" + strings.TrimSpace(stdout.String())
	if !releaseTag.MatchString(tag) {
		return "", skipped, fmt.Errorf("%s --previous printed %q, not a version", previousRule, stdout.String())
	}
	return tag, skipped, nil
}

// readPreviousRelease reads tag's walkthrough, state schema snapshots and
// state history from git.
func readPreviousRelease(root, tag string) (previousRelease, error) {
	p := previousRelease{tag: tag, walkthrough: map[string][]*hclsyntax.Body{},
		shapes: map[string]map[string]bool{}, addedLater: map[string]bool{}}

	files, err := git(root, "ls-tree", "-r", "--name-only", tag, "examples/walkthrough")
	if err != nil {
		return p, err
	}
	for _, f := range strings.Fields(files) {
		if !strings.HasSuffix(f, ".tf") {
			continue
		}
		src, showErr := git(root, "show", tag+":"+f)
		if showErr != nil {
			return p, showErr
		}
		parsed, diags := hclsyntax.ParseConfig([]byte(src), f, hcl.InitialPos)
		if diags.HasErrors() {
			return p, fmt.Errorf("%s at %s: %v", f, tag, diags)
		}
		dir := strings.Split(f, "/")[2]
		body, ok := parsed.Body.(*hclsyntax.Body)
		if !ok {
			return p, fmt.Errorf("%s at %s: a body of type %T", f, tag, parsed.Body)
		}
		p.walkthrough[dir] = append(p.walkthrough[dir], body)
	}

	// The highest version each resource has a snapshot for is the version
	// the release declared.
	files, err = git(root, "ls-tree", "-r", "--name-only", tag, stateSchemaDir)
	if err != nil {
		return p, err
	}
	newest := map[string]int{}
	for _, f := range strings.Fields(files) {
		parts := strings.Split(strings.TrimPrefix(f, stateSchemaDir+"/"), "/")
		if len(parts) != 2 || !strings.HasPrefix(parts[1], "v") || !strings.HasSuffix(parts[1], ".json") {
			continue
		}
		v, atoiErr := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(parts[1], "v"), ".json"))
		if atoiErr != nil {
			continue
		}
		if cur, ok := newest[parts[0]]; !ok || v > cur {
			newest[parts[0]] = v
		}
	}
	for suffix, v := range newest {
		var typ any
		file := stateSchemaDir + "/" + suffix + "/v" + strconv.Itoa(v) + ".json"
		src, showErr := git(root, "show", tag+":"+file)
		if showErr != nil {
			return p, showErr
		}
		if jsonErr := json.Unmarshal([]byte(src), &typ); jsonErr != nil {
			return p, fmt.Errorf("%s at %s: %v", file, tag, jsonErr)
		}
		paths := map[string]bool{}
		stateTypePaths(typ, "", paths)
		p.shapes[suffix] = paths
	}

	history := "internal/provider/state_history.go"
	listed, err := git(root, "ls-tree", "--name-only", tag, history)
	if err != nil || strings.TrimSpace(listed) == "" {
		return p, err
	}
	src, err := git(root, "show", tag+":"+history)
	if err != nil {
		return p, err
	}
	file, err := parser.ParseFile(token.NewFileSet(), history, src, 0)
	if err != nil {
		return p, fmt.Errorf("%s at %s: %v", history, tag, err)
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
	return p, nil
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

// upgradeSummaryFile is where make testacc writes the summary, for the page
// to name. The Makefile passes it as -upgrade-summary.
const upgradeSummaryFile = ".testing-matrix/upgrade-coverage.md"

const upgradeSummaryTitle = "## Upgrade coverage\n\n"

// notShown is the summary when it can't be worked out: one line, and no
// failure.
func notShown(why string) (text, headline string) {
	headline = "upgrade coverage isn't shown: " + why
	return upgradeSummaryTitle + "Upgrade coverage isn't shown: " + why + "\n", headline
}

// upgradeSummaryText works out the summary from the record's coverage. pick
// is upgradeFrom, or a stand-in in a test. It never fails: what it can't work
// out, it says in one line.
func upgradeSummaryText(root string, pick func(string) (string, []string, error),
	types []string, attrs map[string][]matrixAttr, cov coverage,
) (text, headline string) {
	tag, skipped, err := pick(root)
	if err != nil {
		return notShown(fmt.Sprintf("%s couldn't pick the release make upgrade-test upgrades from: %v", previousRule, err))
	}
	if _, tagErr := git(root, "rev-parse", "-q", "--verify", "refs/tags/"+tag); tagErr != nil {
		return notShown(fmt.Sprintf("%s, the release make upgrade-test upgrades from, isn't a tag in this clone. "+
			"git fetch --tags to see it", tag))
	}
	prev, err := readPreviousRelease(root, tag)
	if err != nil {
		return notShown(fmt.Sprintf("reading %s: %v", tag, err))
	}
	return renderUpgradeSummary(prev, skipped, types, attrs, cov)
}

// renderUpgradeSummary counts, for each resource, the upgrade cells of the
// attributes prev stored, which of them a test or a directory of prev's
// walkthrough covers, and lists each cell's sources.
func renderUpgradeSummary(prev previousRelease, skipped, types []string, attrs map[string][]matrixAttr,
	cov coverage,
) (text, headline string) {
	for _, typ := range types {
		for k, names := range prev.walkthroughCoverage(typ, attrs[typ]) {
			for _, n := range names {
				cov.add(k, n, "")
			}
		}
	}
	var rows, covered, uncovered []string
	totals := [4]int{}
	for _, typ := range types {
		n := [4]int{}
		for _, a := range attrs[typ] {
			k := cellKey{typ, a.Path, cellUpgrade}
			switch {
			case !a.Needs[cellUpgrade]:
			case !prev.stored(typ, a.Path) && len(cov[k]) == 0:
				n[3]++
			case len(cov[k]) > 0:
				n[0]++
				n[1]++
				covered = append(covered, fmt.Sprintf("| `%s` | `%s` | %s |", typ, a.Path, sourceNames(cov, k)))
			default:
				n[0]++
				n[2]++
				uncovered = append(uncovered, fmt.Sprintf("| `%s` | `%s` |", typ, a.Path))
			}
		}
		for i := range n {
			totals[i] += n[i]
		}
		rows = append(rows, fmt.Sprintf("| `%s` | %d | %d | %d | %d |", typ, n[0], n[1], n[2], n[3]))
	}

	headline = fmt.Sprintf("upgrade coverage from %s, information only: %d of %d cells covered, %d not",
		prev.tag, totals[1], totals[0], totals[2])
	var b strings.Builder
	b.WriteString(upgradeSummaryTitle)
	fmt.Fprintf(&b, "Information only, and it fails nothing: `make upgrade-test` is what gates upgrades. "+
		"From %s, the release `make upgrade-test` upgrades from, %d of %d upgrade cells are covered and %d aren't.\n\n",
		prev.tag, totals[1], totals[0], totals[2])
	for _, s := range skipped {
		b.WriteString(s + ".\n\n")
	}
	fmt.Fprintf(&b, "A test covers a cell when it upgrades a state from an older schema version holding the attribute, "+
		"and a directory of %s's walkthrough covers it when it sets the attribute, since the upgrade job applies that "+
		"directory with %s and plans it with this build. An attribute %s didn't store has no upgrade, so no cell.\n\n",
		prev.tag, prev.tag, prev.tag)
	fmt.Fprintf(&b, "| Resource | Cells | Covered | Not covered | Not in %s |\n| --- | --- | --- | --- | --- |\n", prev.tag)
	b.WriteString(strings.Join(rows, "\n") + "\n")
	fmt.Fprintf(&b, "| All | %d | %d | %d | %d |\n", totals[0], totals[1], totals[2], totals[3])
	if len(uncovered) > 0 {
		fmt.Fprintf(&b, "\n<details><summary>Not covered (%d)</summary>\n\n| Resource | Attribute |\n| --- | --- |\n%s\n\n</details>\n",
			len(uncovered), strings.Join(uncovered, "\n"))
	}
	if len(covered) > 0 {
		fmt.Fprintf(&b, "\n<details><summary>Covered (%d)</summary>\n\n| Resource | Attribute | By |\n| --- | --- | --- |\n%s\n\n</details>\n",
			len(covered), strings.Join(covered, "\n"))
	}
	return b.String(), headline
}

// TestTheUpgradeCoverageSummary writes the summary make testacc reports after
// the matrix check. It never fails the run.
func TestTheUpgradeCoverageSummary(t *testing.T) {
	if *upgradeSummary == "" || *matrixRecord == "" {
		t.Skip("no -upgrade-summary and -matrix-record: `make testacc` runs this after the matrix check")
	}
	root := repoRoot(t)
	types, attrs, record := matrixInputs(t)
	var text, headline string
	if cov, _, err := readRecord(record, attrs); err != nil {
		text, headline = notShown(err.Error())
	} else {
		text, headline = upgradeSummaryText(root, upgradeFrom, types, attrs, cov)
	}
	if err := os.WriteFile(*upgradeSummary, []byte(text), 0o600); err != nil {
		t.Logf("writing %s: %v", *upgradeSummary, err)
	}
	if file := os.Getenv("GITHUB_STEP_SUMMARY"); file != "" {
		if err := appendFile(file, text); err != nil {
			t.Logf("writing the job summary: %v", err)
		}
	}
	fmt.Printf("testacc: %s. All of it: %s\n", headline, *upgradeSummary)
}

func appendFile(file, text string) error {
	f, err := os.OpenFile(file, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.WriteString("\n" + text); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}
