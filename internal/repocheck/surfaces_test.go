package repocheck

import (
	"context"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/action"
	aschema "github.com/hashicorp/terraform-plugin-framework/action/schema"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	dschema "github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/ephemeral"
	eschema "github.com/hashicorp/terraform-plugin-framework/ephemeral/schema"
	"github.com/hashicorp/terraform-plugin-framework/list"
	lschema "github.com/hashicorp/terraform-plugin-framework/list/schema"
	fwprovider "github.com/hashicorp/terraform-plugin-framework/provider"
	pschema "github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	rschema "github.com/hashicorp/terraform-plugin-framework/resource/schema"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/provider"
)

// This file reads the provider's own schema, through the same interface
// Terraform reads it through. Nothing here restates a schema: the checks ask
// the registered resources and data sources what they declare, so a check
// cannot pass because someone updated a list.

// surface is one registered resource, data source, ephemeral resource, list
// resource, action, or the provider block.
type surface struct {
	Kind       string // "resource", "data source", "ephemeral resource", "list resource", "action" or "provider"
	Name       string // the Terraform type name, from ResourceName
	Doc        string // the schema's own markdown description
	Attributes map[string]attribute
}

// attribute is the part of a schema attribute a check here cares about.
type attribute struct {
	// WriteOnly is set only on a managed resource's attributes, the one kind
	// that can have one.
	WriteOnly bool
	Sensitive bool
	Required  bool
	Optional  bool
	Computed  bool
	Doc       string
	Nested    map[string]attribute
}

// surfaces builds the provider's whole declared surface, once per call. It is
// cheap — no network, no Terraform — and building it fresh keeps checks
// independent.
func surfaces() []surface {
	ctx := context.Background()
	p := provider.New("test")()

	var providerSchema fwprovider.SchemaResponse
	p.Schema(ctx, fwprovider.SchemaRequest{}, &providerSchema)
	out := []surface{{
		Kind:       "provider",
		Name:       provider.ProviderTypeName,
		Doc:        providerSchema.Schema.MarkdownDescription,
		Attributes: providerAttributes(providerSchema.Schema.Attributes),
	}}

	for _, newResource := range p.Resources(ctx) {
		r := newResource()
		var md resource.MetadataResponse
		r.Metadata(ctx, resource.MetadataRequest{ProviderTypeName: provider.ProviderTypeName}, &md)
		var sr resource.SchemaResponse
		r.Schema(ctx, resource.SchemaRequest{}, &sr)
		out = append(out, surface{
			Kind:       "resource",
			Name:       md.TypeName,
			Doc:        sr.Schema.MarkdownDescription,
			Attributes: resourceAttributes(sr.Schema.Attributes),
		})
	}

	for _, newDataSource := range p.DataSources(ctx) {
		d := newDataSource()
		var md datasource.MetadataResponse
		d.Metadata(ctx, datasource.MetadataRequest{ProviderTypeName: provider.ProviderTypeName}, &md)
		var sr datasource.SchemaResponse
		d.Schema(ctx, datasource.SchemaRequest{}, &sr)
		out = append(out, surface{
			Kind:       "data source",
			Name:       md.TypeName,
			Doc:        sr.Schema.MarkdownDescription,
			Attributes: dataSourceAttributes(sr.Schema.Attributes),
		})
	}

	// A list resource's configuration is what a `list` block in a
	// `.tfquery.hcl` file sets, so its inputs are judged like any other.
	if lp, ok := p.(fwprovider.ProviderWithListResources); ok {
		for _, newList := range lp.ListResources(ctx) {
			l := newList()
			var md resource.MetadataResponse
			l.Metadata(ctx, resource.MetadataRequest{ProviderTypeName: provider.ProviderTypeName}, &md)
			var sr list.ListResourceSchemaResponse
			l.ListResourceConfigSchema(ctx, list.ListResourceSchemaRequest{}, &sr)
			out = append(out, surface{
				Kind:       "list resource",
				Name:       md.TypeName,
				Doc:        sr.Schema.MarkdownDescription,
				Attributes: listAttributes(sr.Schema.Attributes),
			})
		}
	}

	// An action's config block is set in a configuration like a resource's
	// arguments, so its inputs are judged too.
	if ap, ok := p.(fwprovider.ProviderWithActions); ok {
		for _, newAction := range ap.Actions(ctx) {
			a := newAction()
			var md action.MetadataResponse
			a.Metadata(ctx, action.MetadataRequest{ProviderTypeName: provider.ProviderTypeName}, &md)
			var sr action.SchemaResponse
			a.Schema(ctx, action.SchemaRequest{}, &sr)
			out = append(out, surface{
				Kind:       "action",
				Name:       md.TypeName,
				Doc:        sr.Schema.MarkdownDescription,
				Attributes: actionAttributes(sr.Schema.Attributes),
			})
		}
	}

	ep, hasEphemeral := p.(fwprovider.ProviderWithEphemeralResources)
	if !hasEphemeral {
		return out
	}
	for _, newEphemeral := range ep.EphemeralResources(ctx) {
		e := newEphemeral()
		var md ephemeral.MetadataResponse
		e.Metadata(ctx, ephemeral.MetadataRequest{ProviderTypeName: provider.ProviderTypeName}, &md)
		var sr ephemeral.SchemaResponse
		e.Schema(ctx, ephemeral.SchemaRequest{}, &sr)
		out = append(out, surface{
			Kind:       "ephemeral resource",
			Name:       md.TypeName,
			Doc:        sr.Schema.MarkdownDescription,
			Attributes: ephemeralAttributes(sr.Schema.Attributes),
		})
	}

	return out
}

func actionAttributes(attrs map[string]aschema.Attribute) map[string]attribute {
	out := map[string]attribute{}
	for name, a := range attrs {
		info := attribute{
			Sensitive: a.IsSensitive(), Required: a.IsRequired(), Optional: a.IsOptional(), Computed: a.IsComputed(),
			Doc: a.GetMarkdownDescription(),
		}
		switch nested := a.(type) {
		case aschema.MapNestedAttribute:
			info.Nested = actionAttributes(nested.NestedObject.Attributes)
		case aschema.ListNestedAttribute:
			info.Nested = actionAttributes(nested.NestedObject.Attributes)
		case aschema.SetNestedAttribute:
			info.Nested = actionAttributes(nested.NestedObject.Attributes)
		case aschema.SingleNestedAttribute:
			info.Nested = actionAttributes(nested.Attributes)
		}
		out[name] = info
	}
	return out
}

func listAttributes(attrs map[string]lschema.Attribute) map[string]attribute {
	out := map[string]attribute{}
	for name, a := range attrs {
		info := attribute{
			Sensitive: a.IsSensitive(), Required: a.IsRequired(), Optional: a.IsOptional(), Computed: a.IsComputed(),
			Doc: a.GetMarkdownDescription(),
		}
		switch nested := a.(type) {
		case lschema.MapNestedAttribute:
			info.Nested = listAttributes(nested.NestedObject.Attributes)
		case lschema.ListNestedAttribute:
			info.Nested = listAttributes(nested.NestedObject.Attributes)
		case lschema.SingleNestedAttribute:
			info.Nested = listAttributes(nested.Attributes)
		}
		out[name] = info
	}
	return out
}

func ephemeralAttributes(attrs map[string]eschema.Attribute) map[string]attribute {
	out := map[string]attribute{}
	for name, a := range attrs {
		info := attribute{
			Sensitive: a.IsSensitive(), Required: a.IsRequired(), Optional: a.IsOptional(), Computed: a.IsComputed(),
			Doc: a.GetMarkdownDescription(),
		}
		switch nested := a.(type) {
		case eschema.MapNestedAttribute:
			info.Nested = ephemeralAttributes(nested.NestedObject.Attributes)
		case eschema.ListNestedAttribute:
			info.Nested = ephemeralAttributes(nested.NestedObject.Attributes)
		case eschema.SetNestedAttribute:
			info.Nested = ephemeralAttributes(nested.NestedObject.Attributes)
		case eschema.SingleNestedAttribute:
			info.Nested = ephemeralAttributes(nested.Attributes)
		}
		out[name] = info
	}
	return out
}

func providerAttributes(attrs map[string]pschema.Attribute) map[string]attribute {
	out := map[string]attribute{}
	for name, a := range attrs {
		info := attribute{
			Sensitive: a.IsSensitive(), Required: a.IsRequired(), Optional: a.IsOptional(), Computed: a.IsComputed(),
			Doc: a.GetMarkdownDescription(),
		}
		if nested, ok := a.(pschema.SingleNestedAttribute); ok {
			info.Nested = providerAttributes(nested.Attributes)
		}
		out[name] = info
	}
	return out
}

func resourceAttributes(attrs map[string]rschema.Attribute) map[string]attribute {
	out := map[string]attribute{}
	for name, a := range attrs {
		info := attribute{
			Sensitive: a.IsSensitive(), Required: a.IsRequired(), Optional: a.IsOptional(), Computed: a.IsComputed(),
			WriteOnly: a.IsWriteOnly(),
			Doc:       a.GetMarkdownDescription(),
		}
		switch nested := a.(type) {
		case rschema.MapNestedAttribute:
			info.Nested = resourceAttributes(nested.NestedObject.Attributes)
		case rschema.ListNestedAttribute:
			info.Nested = resourceAttributes(nested.NestedObject.Attributes)
		case rschema.SetNestedAttribute:
			info.Nested = resourceAttributes(nested.NestedObject.Attributes)
		case rschema.SingleNestedAttribute:
			info.Nested = resourceAttributes(nested.Attributes)
		}
		out[name] = info
	}
	return out
}

func dataSourceAttributes(attrs map[string]dschema.Attribute) map[string]attribute {
	out := map[string]attribute{}
	for name, a := range attrs {
		info := attribute{
			Sensitive: a.IsSensitive(), Required: a.IsRequired(), Optional: a.IsOptional(), Computed: a.IsComputed(),
			Doc: a.GetMarkdownDescription(),
		}
		switch nested := a.(type) {
		case dschema.MapNestedAttribute:
			info.Nested = dataSourceAttributes(nested.NestedObject.Attributes)
		case dschema.ListNestedAttribute:
			info.Nested = dataSourceAttributes(nested.NestedObject.Attributes)
		case dschema.SetNestedAttribute:
			info.Nested = dataSourceAttributes(nested.NestedObject.Attributes)
		case dschema.SingleNestedAttribute:
			info.Nested = dataSourceAttributes(nested.Attributes)
		}
		out[name] = info
	}
	return out
}

// lookup walks a dotted attribute path, descending through nested objects.
func (s surface) lookup(path string) (attribute, bool) {
	attrs, current := s.Attributes, attribute{}
	for _, part := range strings.Split(path, ".") {
		a, ok := attrs[part]
		if !ok {
			return attribute{}, false
		}
		current, attrs = a, a.Nested
	}
	return current, true
}

// findSurface takes a kind because a data source and an ephemeral resource are
// allowed to share a type name, and cluster_kubeconfig is both.
func findSurface(kind, name string) (surface, bool) {
	for _, s := range surfaces() {
		if s.Name == name && s.Kind == kind {
			return s, true
		}
	}
	return surface{}, false
}

func walkAttributes(attrs map[string]attribute, prefix string, fn func(string, attribute)) {
	for name, a := range attrs {
		path := name
		if prefix != "" {
			path = prefix + "." + name
		}
		fn(path, a)
		walkAttributes(a.Nested, path, fn)
	}
}

// Every check that walks the provider reads surfaces(), so a kind it leaves
// out is a kind none of them judges: an input on it is never classified, an
// attribute never has to be described. So it has to reach every list resource
// the provider registers, as it reaches every resource.
func TestSurfacesReachEveryListResource(t *testing.T) {
	ctx := context.Background()
	p, ok := provider.New("test")().(fwprovider.ProviderWithListResources)
	if !ok {
		t.Fatal("the provider registers no list resources, so this test has nothing to hold")
	}
	want := len(p.ListResources(ctx))
	got := 0
	for _, s := range surfaces() {
		if s.Kind == "list resource" {
			got++
		}
	}
	if got != want || want == 0 {
		t.Fatalf("surfaces() has %d list resources, and the provider registers %d", got, want)
	}
}

// The same holds for actions: an action's config block is set in a
// configuration like any resource's arguments.
func TestSurfacesReachEveryAction(t *testing.T) {
	ctx := context.Background()
	p, ok := provider.New("test")().(fwprovider.ProviderWithActions)
	if !ok {
		t.Fatal("the provider registers no actions, so this test has nothing to hold")
	}
	want := len(p.Actions(ctx))
	got := 0
	for _, s := range surfaces() {
		if s.Kind == "action" {
			got++
		}
	}
	if got != want || want == 0 {
		t.Fatalf("surfaces() has %d actions, and the provider registers %d", got, want)
	}
}
