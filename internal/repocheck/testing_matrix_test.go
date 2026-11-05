package repocheck

// The testing matrix, docs/testing-matrix.md: for every attribute of every
// resource, the tests that import it and change it.
//
// Nothing in a test declares what it covers. `make testacc` runs the
// acceptance suite with the recorder in internal/testmatrix around every
// provider server, in process and in the built binaries the CLI tests run,
// and this check reads that record. The page and the check depend only on
// the commit, never on a tag. Upgrades aren't on the page: make upgrade-test
// gates them, and testing_matrix_upgrade_test.go reports their coverage
// without failing.
//
// The check fails on a cell the schema needs that no test covers and the gap
// list doesn't name, on a gap a test now covers, on a gap for a cell the
// schema doesn't have, on a cell recorded from a test it couldn't name, and on
// a committed page that isn't what the run saw. Without -matrix-record, as in
// `make test`, it skips.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	rschema "github.com/hashicorp/terraform-plugin-framework/resource/schema"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/provider"
	"github.com/k0rdent/terraform-provider-k0rdent/internal/testmatrix"
)

var (
	matrixRecord = flag.String("matrix-record", "",
		"the record `make testacc` writes, to check docs/testing-matrix.md against")
	updateMatrix = flag.Bool("update-matrix", false, "rewrite docs/testing-matrix.md from -matrix-record")
)

const (
	matrixPage     = "docs/testing-matrix.md"
	matrixGapsFile = "internal/repocheck/testdata/testing-matrix-gaps.txt"
	// matrixRegenerate is the command that rewrites the page.
	matrixRegenerate = "make testing-matrix"
)

type cell string

const (
	cellImport  cell = testmatrix.CellImport
	cellUpdate  cell = testmatrix.CellUpdate
	cellUpgrade cell = testmatrix.CellUpgrade
)

// pageCells are the cells the page shows and the check gates. Upgrade cells
// are only in the upgrade coverage summary.
var pageCells = []cell{cellImport, cellUpdate}

// cellKey names one cell: a resource type, an attribute path with nested
// names joined by dots, and the cell. Its string form is the gap list's line.
type cellKey struct {
	Resource string
	Path     string
	Cell     cell
}

func (k cellKey) String() string { return k.Resource + " " + k.Path + " " + string(k.Cell) }

type replaces int

const (
	replacesNever replaces = iota
	replacesSometimes
	replacesAlways
)

// matrixAttr is one attribute, what the schema says about it, and which
// cells it needs. A cell it doesn't need has the reason in whyNoCell.
type matrixAttr struct {
	Path      string
	Kind      string // required, optional, optional+computed, computed, write-only
	Replaces  replaces
	Needs     map[cell]bool
	whyNoCell map[cell]string
}

// matrixResources is every registered resource type, sorted.
func matrixResources(t *testing.T) []string {
	t.Helper()
	ctx := context.Background()
	var out []string
	for _, newResource := range provider.New("test")().Resources(ctx) {
		var md resource.MetadataResponse
		newResource().Metadata(ctx, resource.MetadataRequest{ProviderTypeName: provider.ProviderTypeName}, &md)
		out = append(out, md.TypeName)
	}
	if len(out) == 0 {
		t.Fatal("no resource is registered, so the matrix proves nothing")
	}
	sort.Strings(out)
	return out
}

// matrixAttributes walks a resource's schema, nested attributes and blocks
// included, and says which cells each attribute needs. Import always. Update
// unless it's computed only or always replaced, itself or through a parent.
// Upgrade, for the summary, unless it's write-only, which is never stored.
func matrixAttributes(t *testing.T, resourceType string) []matrixAttr {
	t.Helper()
	s, ok := resourceSchema(resourceType)
	if !ok {
		t.Fatalf("%s isn't registered", resourceType)
	}
	var out []matrixAttr
	var walk func(attrs map[string]rschema.Attribute, blocks map[string]rschema.Block, prefix string, parent replaces, top bool)
	walk = func(attrs map[string]rschema.Attribute, blocks map[string]rschema.Block, prefix string, parent replaces, top bool) {
		for name, a := range attrs {
			r := max(parent, replacementOf(a))
			if top && (name == "region" || name == "project") {
				r = replacesAlways // replaced from ModifyPlan, not a modifier (scope.go)
			}
			m := matrixAttr{Path: prefix + name, Kind: kindOf(a), Replaces: r,
				Needs: map[cell]bool{cellImport: true}, whyNoCell: map[cell]string{}}
			switch {
			case m.Kind == "computed":
				m.whyNoCell[cellUpdate] = "computed only"
			case r == replacesAlways:
				m.whyNoCell[cellUpdate] = "always replaces"
			default:
				m.Needs[cellUpdate] = true
			}
			if m.Kind == "write-only" {
				m.whyNoCell[cellUpgrade] = "write-only, never stored"
			} else {
				m.Needs[cellUpgrade] = true
			}
			out = append(out, m)
			if n, ok := a.(rschema.NestedAttribute); ok {
				obj, ok := n.GetNestedObject().(rschema.NestedAttributeObject)
				if !ok {
					t.Fatalf("%s %s: a nested object of type %T", resourceType, m.Path, n.GetNestedObject())
				}
				walk(obj.Attributes, nil, m.Path+".", r, false)
			}
		}
		for name, b := range blocks {
			obj := b.GetNestedObject()
			nested := map[string]rschema.Attribute{}
			for k, v := range obj.GetAttributes() {
				a, ok := v.(rschema.Attribute)
				if !ok {
					t.Fatalf("%s %s%s.%s: an attribute of type %T", resourceType, prefix, name, k, v)
				}
				nested[k] = a
			}
			nb := map[string]rschema.Block{}
			for k, v := range obj.GetBlocks() {
				blk, ok := v.(rschema.Block)
				if !ok {
					t.Fatalf("%s %s%s.%s: a block of type %T", resourceType, prefix, name, k, v)
				}
				nb[k] = blk
			}
			walk(nested, nb, prefix+name+".", parent, false)
		}
	}
	walk(s.Attributes, s.Blocks, "", replacesNever, true)
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

func kindOf(a rschema.Attribute) string {
	switch {
	case a.IsWriteOnly():
		return "write-only"
	case a.IsRequired():
		return "required"
	case a.IsOptional() && a.IsComputed():
		return "optional+computed"
	case a.IsOptional():
		return "optional"
	default:
		return "computed"
	}
}

// replacementOf reads the RequiresReplace family off an attribute's plan
// modifiers, the way the provider's own docs writer does (schema_docs.go).
// The unconditional one always replaces. One with a condition replaces
// sometimes.
func replacementOf(a rschema.Attribute) replaces {
	f := reflect.ValueOf(a).FieldByName("PlanModifiers")
	if !f.IsValid() || f.Kind() != reflect.Slice {
		return replacesNever
	}
	r := replacesNever
	for i := range f.Len() {
		m := f.Index(i).Interface()
		if !provider.IsReplacementModifier(m) {
			continue
		}
		desc := ""
		if d, ok := m.(interface{ Description(context.Context) string }); ok {
			desc = d.Description(context.Background())
		}
		if desc == "" || strings.HasPrefix(desc, "If the value of this attribute changes, Terraform will destroy") {
			return replacesAlways
		}
		r = replacesSometimes
	}
	return r
}

// source is one thing that covers a cell: a test, or, for an upgrade, the
// previous release's walkthrough directory. how is "update" or "replace" for a change to an
// attribute that sometimes replaces, and empty otherwise.
type source struct {
	name string
	how  map[string]bool
}

// coverage is the sources behind each cell.
type coverage map[cellKey]map[string]*source

func (c coverage) add(k cellKey, name, how string) {
	if c[k] == nil {
		c[k] = map[string]*source{}
	}
	s := c[k][name]
	if s == nil {
		s = &source{name: name, how: map[string]bool{}}
		c[k][name] = s
	}
	if how != "" {
		s.how[how] = true
	}
}

func (c coverage) names(k cellKey) []string {
	var out []string
	for n := range c[k] {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// readRecord turns the record into coverage for the cells the schema needs.
// A recorded update to a computed attribute is the platform moving it, so it
// fills no cell, and a replacement counts as a change only to an attribute that
// sometimes replaces. unnamed is the cells recorded from a test the recorder
// couldn't name. It returns an error rather than failing, since the upgrade
// coverage summary reads the record too, and that never fails.
func readRecord(file string, attrs map[string][]matrixAttr) (cov coverage, unnamed []string, err error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, nil, fmt.Errorf("reading the matrix record: %w", err)
	}
	defer func() { _ = f.Close() }()
	byPath := map[string]matrixAttr{}
	for typ, as := range attrs {
		for _, a := range as {
			byPath[typ+" "+a.Path] = a
		}
	}
	cov = coverage{}
	var skipped []string
	seenUnnamed := map[string]bool{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	lines, finished := 0, false
	for sc.Scan() {
		lines++
		var l testmatrix.Line
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			return nil, nil, fmt.Errorf("%s line %d: %w", file, lines, err)
		}
		if l.Cell == testmatrix.CellFinished {
			finished = true
			continue
		}
		if l.Cell == testmatrix.CellSkipped {
			skipped = append(skipped, fmt.Sprintf("%s (%s)", l.Test, l.Why))
			continue
		}
		if l.Test == testmatrix.UnknownTest {
			if !seenUnnamed[l.Resource] {
				unnamed = append(unnamed, l.Resource)
				seenUnnamed[l.Resource] = true
			}
			continue
		}
		a, ok := byPath[l.Resource+" "+l.Path]
		if !ok {
			continue
		}
		c, how := cell(l.Cell), ""
		switch {
		case c == testmatrix.CellReplace && a.Replaces == replacesSometimes:
			c, how = cellUpdate, "replace"
		case c == cellUpdate && a.Replaces == replacesSometimes:
			how = "update"
		}
		if !a.Needs[c] {
			continue
		}
		cov.add(cellKey{l.Resource, l.Path, c}, l.Test, how)
	}
	if err := sc.Err(); err != nil {
		return nil, nil, err
	}
	if lines == 0 {
		return nil, nil, fmt.Errorf("%s is empty, so the suite recorded nothing. Did it run with -matrix-record?", file)
	}
	if !finished {
		return nil, nil, errors.New(unfinished(file, skipped))
	}
	sort.Strings(unnamed)
	return cov, unnamed, nil
}

// unfinished says why a record with no finished line isn't the suite's.
func unfinished(file string, skipped []string) string {
	if len(skipped) > 0 {
		return fmt.Sprintf("%s isn't the whole suite's, since tests skipped: %s. "+
			"Run make testacc where none of them skips", file, strings.Join(skipped, ", "))
	}
	return fmt.Sprintf("%s has no %q line: the run that wrote it failed, ran only some tests or stopped early, "+
		"so its cells aren't the suite's. Run make testacc", file, testmatrix.CellFinished)
}

// parseGaps reads the gap list: one cell per line, as cellKey writes it, with
// # comments and blank lines. It takes only the page's cells: upgrade
// coverage isn't gated, so a gap line for it would excuse nothing.
func parseGaps(text string) (map[cellKey]bool, error) {
	gaps := map[cellKey]bool{}
	for i, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Fields(line)
		if len(f) != 3 || !slices.Contains(pageCells, cell(f[2])) {
			return nil, fmt.Errorf("%s line %d: want `<resource> <attribute path> <import|update>`, got %q. "+
				"Upgrade coverage takes no gap line: make upgrade-test gates upgrades, and make testacc only reports them",
				matrixGapsFile, i+1, line)
		}
		gaps[cellKey{f[0], f[1], cell(f[2])}] = true
	}
	return gaps, nil
}

// matrixProblems is the check, over the page's cells.
func matrixProblems(attrs map[string][]matrixAttr, cov coverage, gaps map[cellKey]bool, unnamed []string) []string {
	var problems []string
	for _, r := range unnamed {
		problems = append(problems, fmt.Sprintf("a provider binary recorded %s cells without being told its test: "+
			"append matrixTestEnv(t) to the environment of the command that runs it", r))
	}
	needed := map[cellKey]bool{}
	for typ, as := range attrs {
		for _, a := range as {
			for _, c := range pageCells {
				k := cellKey{typ, a.Path, c}
				if !a.Needs[c] {
					continue
				}
				needed[k] = true
				tests := cov.names(k)
				switch {
				case len(tests) == 0 && !gaps[k]:
					problems = append(problems, fmt.Sprintf("%s: no test covers it and %s doesn't list it. "+
						"Write a test that does, or add the line `%s` there", k, matrixGapsFile, k))
				case len(tests) > 0 && gaps[k]:
					problems = append(problems, fmt.Sprintf("%s: listed as a gap, but %s covers it: drop its line from %s",
						k, strings.Join(tests, ", "), matrixGapsFile))
				}
			}
		}
	}
	for k := range gaps {
		if !needed[k] {
			problems = append(problems, fmt.Sprintf("%s: listed as a gap, but the schema has no such cell: drop its line from %s",
				k, matrixGapsFile))
		}
	}
	sort.Strings(problems)
	return problems
}

// sourceNames is a covered cell's text: its sources, and for an attribute
// that sometimes replaces, which change each one saw.
func sourceNames(cov coverage, k cellKey) string {
	var names []string
	for _, n := range cov.names(k) {
		s := cov[k][n]
		text := "`" + n + "`"
		if dir, ok := strings.CutPrefix(n, walkthroughSource); ok {
			text = "walkthrough `" + dir + "`"
		}
		if len(s.how) > 0 {
			var how []string
			for _, h := range []string{"update", "replace"} {
				if s.how[h] {
					how = append(how, h)
				}
			}
			text += " (" + strings.Join(how, " and ") + ")"
		}
		names = append(names, text)
	}
	return strings.Join(names, ", ")
}

// renderMatrix writes the page.
func renderMatrix(types []string, attrs map[string][]matrixAttr, cov coverage, gaps map[cellKey]bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, `# Testing matrix

Generated by `+"`%s`"+`. Don't edit it by hand.

For every attribute of every resource, this page names the acceptance tests that import it and change it. No test declares what it covers. The acceptance suite runs with a recorder around the provider, in process and in the built binary the CLI tests run, and the recorder writes down what each test made the provider do. `+"`make testacc`"+` runs the suite that way, in CI and locally, and fails when this page isn't what the run saw. The page depends only on the commit, so a new release tag changes nothing here.

Upgrades aren't on this page. `+"`make upgrade-test`"+` is what gates them: state the previous published release wrote has to plan empty under this build. After its check, `+"`make testacc`"+` also reports which attributes the tests and that release's walkthrough upgrade, as information that fails nothing: in the job summary of CI's acceptance job, and in `+"`%s`"+` after a local run.

How to read a row:

- Import names the tests whose plan right after an import keeps the imported value of the attribute.
- Update names the tests that change the attribute in their configuration and plan the change in place. For an attribute that sometimes replaces, a test that plans a replacement for the change counts too, and the cell says which it saw.
- gap means no test covers the cell yet. Each gap is a line in `+"`%s`"+`. Once a test covers it, the check fails until the line goes.
- n/a means the cell can't happen. A computed attribute isn't set by configuration, and one that always replaces has no update.

`, matrixRegenerate, upgradeSummaryFile, matrixGapsFile)

	b.WriteString("| Resource | Attributes | Cells | Covered | Gaps |\n| --- | --- | --- | --- | --- |\n")
	totals := [4]int{}
	for _, typ := range types {
		n := [4]int{len(attrs[typ]), 0, 0, 0}
		for _, a := range attrs[typ] {
			for _, c := range pageCells {
				k := cellKey{typ, a.Path, c}
				switch {
				case !a.Needs[c]:
				case len(cov[k]) > 0:
					n[1]++
					n[2]++
				default:
					n[1]++
					n[3]++
				}
			}
		}
		for i := range n {
			totals[i] += n[i]
		}
		fmt.Fprintf(&b, "| [`%s`](#%s) | %d | %d | %d | %d |\n", typ, typ, n[0], n[1], n[2], n[3])
	}
	fmt.Fprintf(&b, "| All | %d | %d | %d | %d |\n", totals[0], totals[1], totals[2], totals[3])

	changesText := map[replaces]string{replacesNever: "in place", replacesSometimes: "sometimes replaces", replacesAlways: "replaces"}
	for _, typ := range types {
		fmt.Fprintf(&b, "\n## %s\n\n| Attribute | Kind | Changes | Import | Update |\n| --- | --- | --- | --- | --- |\n", typ)
		for _, a := range attrs[typ] {
			changes := changesText[a.Replaces]
			if a.Kind == "computed" {
				changes = "n/a"
			}
			row := []string{"`" + a.Path + "`", a.Kind, changes}
			for _, c := range pageCells {
				k := cellKey{typ, a.Path, c}
				switch {
				case !a.Needs[c]:
					row = append(row, "n/a ("+a.whyNoCell[c]+")")
				case len(cov[k]) > 0:
					row = append(row, sourceNames(cov, k))
				case gaps[k]:
					row = append(row, "**gap**")
				default:
					row = append(row, "**missing**")
				}
			}
			b.WriteString("| " + strings.Join(row, " | ") + " |\n")
		}
	}
	return b.String()
}

// pageDiff is the lines only one side has, in order, for a failure message.
func pageDiff(committed, generated string) string {
	have := map[string]int{}
	for _, l := range strings.Split(committed, "\n") {
		have[l]++
	}
	want := map[string]int{}
	for _, l := range strings.Split(generated, "\n") {
		want[l]++
	}
	var out []string
	for _, l := range strings.Split(committed, "\n") {
		if want[l] == 0 {
			out = append(out, "- "+l)
		}
	}
	for _, l := range strings.Split(generated, "\n") {
		if have[l] == 0 {
			out = append(out, "+ "+l)
		}
	}
	return strings.Join(out, "\n")
}

// matrixInputs is what the check and the upgrade summary both read: every
// resource type, its attributes, and the record's coverage.
func matrixInputs(t *testing.T) (types []string, attrs map[string][]matrixAttr, record string) {
	t.Helper()
	types = matrixResources(t)
	attrs = map[string][]matrixAttr{}
	for _, typ := range types {
		attrs[typ] = matrixAttributes(t, typ)
	}
	record, err := filepath.Abs(*matrixRecord)
	if err != nil {
		t.Fatal(err)
	}
	return types, attrs, record
}

func TestTheTestingMatrixIsCurrent(t *testing.T) {
	if *matrixRecord == "" {
		t.Skip("no -matrix-record: `make testacc` runs this after the acceptance suite")
	}
	root := repoRoot(t)
	types, attrs, record := matrixInputs(t)
	cov, unnamed, err := readRecord(record, attrs)
	if err != nil {
		t.Fatal(err)
	}
	gapText, err := os.ReadFile(filepath.Join(root, matrixGapsFile))
	if err != nil {
		t.Fatalf("reading the gap list: %v", err)
	}
	gaps, err := parseGaps(string(gapText))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range matrixProblems(attrs, cov, gaps, unnamed) {
		t.Error(p)
	}

	page := renderMatrix(types, attrs, cov, gaps)
	file := filepath.Join(root, matrixPage)
	if *updateMatrix {
		if err := os.WriteFile(file, []byte(page), 0o644); err != nil { //nolint:gosec // a committed doc, readable by all
			t.Fatal(err)
		}
		return
	}
	committed, readErr := os.ReadFile(file)
	if readErr != nil {
		t.Fatalf("%s: %v. Run `%s` and commit it", matrixPage, readErr, matrixRegenerate)
	}
	if string(committed) != page {
		t.Errorf("%s isn't what this run saw. Run `%s` and commit it. What changed:\n%s",
			matrixPage, matrixRegenerate, pageDiff(string(committed), page))
	}
}
