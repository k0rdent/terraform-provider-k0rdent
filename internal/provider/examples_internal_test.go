package provider

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/terraform-plugin-framework/resource"
)

// The examples are what a reader copies, and the registry page renders them
// as they are. Nothing plans them on every change, because most of them read
// a platform the unit tests don't have. So this checks each one against the
// schema it configures: every argument and block it writes exists, none of
// them is read-only, and every required argument is there. A renamed or
// removed attribute fails here instead of on the registry page.

const examplesDir = "../../examples"

// metaArguments are Terraform's own, valid in any resource or data block.
var metaArguments = map[string]bool{
	"count": true, "for_each": true, "depends_on": true, "provider": true, "lifecycle": true,
	"connection": true, "provisioner": true,
}

func TestEveryExampleMatchesItsSchema(t *testing.T) {
	ctx := context.Background()
	schemas := map[string]docSurface{}
	for _, s := range docSurfaces(ctx) {
		if s.page == "" {
			continue
		}
		schemas[blockType(s.kind)+" "+s.name] = s
	}
	identities := identitySchemas(ctx)

	files := 0
	checked := map[string]bool{}
	err := filepath.WalkDir(examplesDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || (filepath.Ext(path) != ".tf" && !strings.HasSuffix(path, ".tfquery.hcl")) {
			return err
		}
		src, err := os.ReadFile(path) //nolint:gosec // an example in this repository
		if err != nil {
			return err
		}
		f, diags := hclsyntax.ParseConfig(src, path, hcl.InitialPos)
		if diags.HasErrors() {
			t.Errorf("%s does not parse: %s", path, diags.Error())
			return nil
		}
		files++
		body, ok := f.Body.(*hclsyntax.Body)
		if !ok {
			t.Errorf("%s is not native HCL syntax", path)
			return nil
		}
		for _, b := range body.Blocks {
			switch {
			case (b.Type == "resource" || b.Type == "data" || b.Type == "ephemeral") && len(b.Labels) == 2:
				s, ok := schemas[b.Type+" "+b.Labels[0]]
				if !ok {
					if strings.HasPrefix(b.Labels[0], ResourcePrefix()) {
						t.Errorf("%s:%d: %s %q is not registered", path, b.DefRange().Start.Line, b.Type, b.Labels[0])
					}
					continue
				}
				checked[b.Type+" "+b.Labels[0]] = true
				where := fmt.Sprintf("%s:%d: %s.%s", path, b.DefRange().Start.Line, b.Labels[0], b.Labels[1])
				for _, problem := range checkBody(b.Body, s.attributes, s.blocks, true) {
					t.Errorf("%s%s", where, problem)
				}
			case b.Type == "list" && len(b.Labels) == 2:
				for _, problem := range checkList(b, schemas) {
					t.Errorf("%s:%d: list.%s.%s%s", path, b.DefRange().Start.Line, b.Labels[0], b.Labels[1], problem)
				}
				checked["list "+b.Labels[0]] = true
			case b.Type == "action" && len(b.Labels) == 2:
				s, ok := schemas[b.Type+" "+b.Labels[0]]
				if !ok {
					if strings.HasPrefix(b.Labels[0], ResourcePrefix()) {
						t.Errorf("%s:%d: action %q is not registered", path, b.DefRange().Start.Line, b.Labels[0])
					}
					continue
				}
				checked[b.Type+" "+b.Labels[0]] = true
				where := fmt.Sprintf("%s:%d: action.%s.%s", path, b.DefRange().Start.Line, b.Labels[0], b.Labels[1])
				// An action's arguments go in its `config` block. Everything
				// else in the action block is Terraform's.
				var config *hclsyntax.Body
				for _, inner := range b.Body.Blocks {
					if inner.Type == "config" {
						config = inner.Body
					}
				}
				if config == nil {
					t.Errorf("%s: has no config block", where)
					continue
				}
				for _, problem := range checkBody(config, s.attributes, s.blocks, false) {
					t.Errorf("%s%s", where, problem)
				}
			case b.Type == "import":
				for _, problem := range checkImport(b.Body, identities) {
					t.Errorf("%s:%d: %s", path, b.DefRange().Start.Line, problem)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if files == 0 {
		t.Fatalf("no examples found under %s", examplesDir)
	}
	// Every registered surface is configured by at least one example.
	for key := range schemas {
		if !checked[key] {
			t.Errorf("no example configures %s", key)
		}
	}
}

// Every resource that imports has the three examples the registry page's
// Import section renders: the command, an import block by id, and, where the
// resource declares an identity, an import block by identity.
func TestEveryImportableResourceShowsHowToImport(t *testing.T) {
	ctx := context.Background()
	identities := identitySchemas(ctx)
	n := 0
	for _, s := range docSurfaces(ctx) {
		if s.kind != "resource" || !s.importable {
			continue
		}
		n++
		dir := filepath.Join(examplesDir, "resources", s.name)
		want := []string{"import.sh", "import-by-string-id.tf"}
		if _, ok := identities[s.name]; ok {
			want = append(want, "import-by-identity.tf")
		}
		for _, f := range want {
			if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
				t.Errorf("%s imports, and has no %s", s.name, filepath.Join(dir, f))
			}
		}
	}
	if n == 0 {
		t.Fatal("no importable resource found")
	}
}

func blockType(kind string) string {
	switch kind {
	case "list resource":
		return "list"
	case "data source":
		return "data"
	case "ephemeral resource":
		return "ephemeral"
	case "action":
		return "action"
	}
	return "resource"
}

// listArguments are what a `list` block takes beside its `config` block:
// Terraform's own, the same for every list resource.
var listArguments = map[string]bool{
	"provider": true, "count": true, "for_each": true, "include_resource": true, "limit": true,
}

// checkList checks a `list` block in a .tfquery.hcl file: the type is
// registered, its own arguments are Terraform's, and its `config` block
// matches the list resource's schema.
func checkList(b *hclsyntax.Block, schemas map[string]docSurface) []string {
	s, ok := schemas["list "+b.Labels[0]]
	if !ok {
		return []string{" is not a registered list resource"}
	}
	var out []string
	if _, ok := b.Body.Attributes["provider"]; !ok {
		out = append(out, ": has no `provider`, which a list block requires")
	}
	for name := range b.Body.Attributes {
		if !listArguments[name] {
			out = append(out, fmt.Sprintf(": sets %q, which belongs in its config block", name))
		}
	}
	for _, c := range b.Body.Blocks {
		if c.Type != "config" {
			out = append(out, fmt.Sprintf(": has a %q block, which a list block doesn't take", c.Type))
			continue
		}
		for _, p := range checkBody(c.Body, s.attributes, s.blocks, false) {
			out = append(out, ".config"+p)
		}
	}
	if len(b.Body.Blocks) == 0 {
		out = append(out, missingRequired(&hclsyntax.Body{}, s.attributes)...)
	}
	return out
}

// identitySchemas maps each resource with an identity to its identity
// attributes.
func identitySchemas(ctx context.Context) map[string]map[string]bool {
	out := map[string]map[string]bool{}
	for _, newResource := range New("docs")().Resources(ctx) {
		r := newResource()
		ir, ok := r.(resource.ResourceWithIdentity)
		if !ok {
			continue
		}
		var md resource.MetadataResponse
		r.Metadata(ctx, resource.MetadataRequest{ProviderTypeName: ProviderTypeName}, &md)
		var is resource.IdentitySchemaResponse
		ir.IdentitySchema(ctx, resource.IdentitySchemaRequest{}, &is)
		names := map[string]bool{}
		for name := range is.IdentitySchema.Attributes {
			names[name] = true
		}
		out[md.TypeName] = names
	}
	return out
}

// checkImport checks an import block's identity keys against the resource's
// identity schema.
func checkImport(body *hclsyntax.Body, identities map[string]map[string]bool) []string {
	to, ok := body.Attributes["to"]
	if !ok {
		return []string{"import block with no `to`"}
	}
	traversal, diags := hcl.AbsTraversalForExpr(to.Expr)
	if diags.HasErrors() || len(traversal) < 2 {
		return nil
	}
	typeName := traversal.RootName()
	id, ok := body.Attributes["identity"]
	if !ok {
		return nil
	}
	names, ok := identities[typeName]
	if !ok {
		return []string{fmt.Sprintf("imports %s by identity, and it declares none", typeName)}
	}
	obj, ok := id.Expr.(*hclsyntax.ObjectConsExpr)
	if !ok {
		return nil
	}
	var out []string
	for _, item := range obj.Items {
		if key := objectKey(item.KeyExpr); key != "" && !names[key] {
			out = append(out, fmt.Sprintf("identity of %s has no %q", typeName, key))
		}
	}
	return out
}

// checkBody checks one block body against an attribute map and a block map.
func checkBody(body *hclsyntax.Body, attrs, blocks reflect.Value, top bool) []string {
	var out []string
	for name, a := range body.Attributes {
		if top && metaArguments[name] {
			continue
		}
		v := lookup(attrs, name)
		if !v.IsValid() {
			out = append(out, fmt.Sprintf(": sets %q, which the schema doesn't have", name))
			continue
		}
		if boolField(v, "Computed") && !boolField(v, "Optional") && !boolField(v, "Required") {
			out = append(out, fmt.Sprintf(": sets %q, which is read-only", name))
			continue
		}
		for _, p := range checkValue(a.Expr, v) {
			out = append(out, "."+name+p)
		}
	}
	for _, b := range body.Blocks {
		if top && metaArguments[b.Type] {
			continue
		}
		// `dynamic "ingress" { content { ... } }` writes ingress blocks.
		if b.Type == "dynamic" && len(b.Labels) == 1 {
			v := lookup(blocks, b.Labels[0])
			if !v.IsValid() {
				out = append(out, fmt.Sprintf(": has a dynamic %q block, which the schema doesn't have", b.Labels[0]))
				continue
			}
			inner, innerBlocks := nestedOf(v)
			for _, c := range b.Body.Blocks {
				if c.Type == "content" {
					for _, p := range checkBody(c.Body, inner, innerBlocks, false) {
						out = append(out, "."+b.Labels[0]+p)
					}
				}
			}
			continue
		}
		v := lookup(blocks, b.Type)
		if !v.IsValid() {
			out = append(out, fmt.Sprintf(": has a %q block, which the schema doesn't have", b.Type))
			continue
		}
		inner, innerBlocks := nestedOf(v)
		for _, p := range checkBody(b.Body, inner, innerBlocks, false) {
			out = append(out, "."+b.Type+p)
		}
	}
	out = append(out, missingRequired(body, attrs)...)
	return out
}

// checkValue follows an object or a list of objects written for a nested
// attribute into its nested schema. Anything else is an expression this test
// can't see into, and is left to Terraform.
func checkValue(expr hclsyntax.Expression, v reflect.Value) []string {
	attrs, _ := nestedOf(v)
	if !attrs.IsValid() {
		return nil
	}
	switch v.Type().Name() {
	case "SingleNestedAttribute":
		if obj, ok := expr.(*hclsyntax.ObjectConsExpr); ok {
			return checkObject(obj, attrs)
		}
	case "MapNestedAttribute":
		if obj, ok := expr.(*hclsyntax.ObjectConsExpr); ok {
			var out []string
			for _, item := range obj.Items {
				if inner, ok := item.ValueExpr.(*hclsyntax.ObjectConsExpr); ok {
					out = append(out, checkObject(inner, attrs)...)
				}
			}
			return out
		}
	case "ListNestedAttribute", "SetNestedAttribute":
		if tuple, ok := expr.(*hclsyntax.TupleConsExpr); ok {
			var out []string
			for _, e := range tuple.Exprs {
				if inner, ok := e.(*hclsyntax.ObjectConsExpr); ok {
					out = append(out, checkObject(inner, attrs)...)
				}
			}
			return out
		}
	}
	return nil
}

func checkObject(obj *hclsyntax.ObjectConsExpr, attrs reflect.Value) []string {
	var out []string
	seen := map[string]bool{}
	for _, item := range obj.Items {
		key := objectKey(item.KeyExpr)
		if key == "" {
			continue
		}
		seen[key] = true
		v := lookup(attrs, key)
		if !v.IsValid() {
			out = append(out, fmt.Sprintf(": sets %q, which the schema doesn't have", key))
			continue
		}
		if boolField(v, "Computed") && !boolField(v, "Optional") && !boolField(v, "Required") {
			out = append(out, fmt.Sprintf(": sets %q, which is read-only", key))
			continue
		}
		for _, p := range checkValue(item.ValueExpr, v) {
			out = append(out, "."+key+p)
		}
	}
	for _, name := range requiredNames(attrs) {
		if !seen[name] {
			out = append(out, fmt.Sprintf(": leaves out %q, which is required", name))
		}
	}
	return out
}

func missingRequired(body *hclsyntax.Body, attrs reflect.Value) []string {
	var out []string
	for _, name := range requiredNames(attrs) {
		if _, ok := body.Attributes[name]; !ok {
			out = append(out, fmt.Sprintf(": leaves out %q, which is required", name))
		}
	}
	return out
}

func requiredNames(attrs reflect.Value) []string {
	if !attrs.IsValid() || attrs.Kind() != reflect.Map {
		return nil
	}
	var out []string
	for _, k := range attrs.MapKeys() {
		if boolField(concrete(attrs.MapIndex(k)), "Required") {
			out = append(out, k.String())
		}
	}
	slices.Sort(out)
	return out
}

// objectKey is a literal object key, `name` or `"name"`. A computed key, such
// as a map key taken from another resource, is not a schema name.
func objectKey(expr hclsyntax.Expression) string {
	if wrapped, ok := expr.(*hclsyntax.ObjectConsKeyExpr); ok {
		if wrapped.ForceNonLiteral {
			return ""
		}
		if name := hcl.ExprAsKeyword(wrapped.Wrapped); name != "" {
			return name
		}
		expr = wrapped.Wrapped
	}
	if tmpl, ok := expr.(*hclsyntax.TemplateExpr); ok && tmpl.IsStringLiteral() {
		v, diags := tmpl.Value(nil)
		if !diags.HasErrors() {
			return v.AsString()
		}
	}
	return ""
}

func lookup(m reflect.Value, name string) reflect.Value {
	if !m.IsValid() || m.Kind() != reflect.Map || m.IsNil() {
		return reflect.Value{}
	}
	return concrete(m.MapIndex(reflect.ValueOf(name)))
}

// nestedOf returns the attribute and block maps inside a nested attribute or
// block.
func nestedOf(v reflect.Value) (attrs, blocks reflect.Value) {
	holder := v
	if obj := v.FieldByName("NestedObject"); obj.IsValid() && obj.Kind() == reflect.Struct {
		holder = obj
	}
	return holder.FieldByName("Attributes"), holder.FieldByName("Blocks")
}

// An example that reads a data source or refers to another resource shows a
// reader that the two go together, so the page links that one under Related.
func TestEveryPageLinksWhatItsExamplesUse(t *testing.T) {
	ctx := context.Background()
	registered := map[string]docSurface{}
	for _, s := range docSurfaces(ctx) {
		if s.page != "" {
			registered[s.page] = s
		}
	}
	files := map[string]string{
		"resource": "resource*.tf", "data source": "data-source*.tf", "ephemeral resource": "ephemeral-resource*.tf",
		"list resource": "list-resource*.tfquery.hcl", "action": "action*.tf",
	}
	for page, s := range registered {
		dir, _, _ := strings.Cut(page, "/")
		matches, err := filepath.Glob(filepath.Join(examplesDir, dir, s.name, files[s.kind]))
		if err != nil {
			t.Fatal(err)
		}
		linked := map[string]bool{}
		for _, l := range relatedLinks(page, registered) {
			linked[l.page] = true
		}
		for _, f := range matches {
			src, err := os.ReadFile(f) //nolint:gosec // an example in this repository
			if err != nil {
				t.Fatal(err)
			}
			for _, used := range referencedPages(string(src), registered) {
				if used != page && !linked[used] {
					t.Errorf("%s uses %s, and %s's Related section doesn't link it", f, used, page)
				}
			}
		}
	}
}

// referencedPages finds the surfaces an example refers to by address:
// data.<prefix>x.name, ephemeral.<prefix>x.name, action.<prefix>x.name and
// <prefix>x.name.
func referencedPages(src string, registered map[string]docSurface) []string {
	ref := regexp.MustCompile(`\b(data\.|ephemeral\.|action\.)?(` + regexp.QuoteMeta(ResourcePrefix()) + `[a-z0-9_]+)\.[a-z_]`)
	seen := map[string]bool{}
	var out []string
	for _, m := range ref.FindAllStringSubmatch(src, -1) {
		dir := "resources"
		switch m[1] {
		case "data.":
			dir = "data-sources"
		case "ephemeral.":
			dir = "ephemeral-resources"
		case "action.":
			dir = "actions"
		}
		page := dir + "/" + strings.TrimPrefix(m[2], ResourcePrefix())
		if _, ok := registered[page]; ok && !seen[page] {
			seen[page] = true
			out = append(out, page)
		}
	}
	return out
}
