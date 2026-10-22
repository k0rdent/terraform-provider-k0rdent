package provider

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/ephemeral"
	"github.com/hashicorp/terraform-plugin-framework/list"
	"github.com/hashicorp/terraform-plugin-framework/path"
	fwprovider "github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// notAnIdentifier is every string a configuration can set that isn't an
// identifier, keyed by attribute name, with why. Everything else a
// configuration sets as a string has to be refused at plan time when it isn't
// one, because sooner or later it fills a segment of a request URL: as a
// resource's own id, as the parent it lives under, or as the thing a data
// source reads.
//
// The generated client refuses a value that isn't one segment anyway. This is
// about where the error lands, and how strict it is: on the attribute, in the
// plan, held to the identifier shape, rather than as a failed request after
// the plan was approved. A new attribute fails TestEveryIdentifierIsCheckedAtPlanTime
// until it is either checked or listed here, so nobody adds one without
// deciding which it is.
var notAnIdentifier = map[string]string{
	"description":            "free text, sent in a request body",
	"display_name":           "free text, sent in a request body",
	"status":                 "a filter value from a fixed list, sent as a query parameter",
	"state":                  "a filter value from a fixed list, sent as a query parameter or compared in the provider",
	"display_name_regex":     "a regular expression, compared in the provider and never sent",
	"object_kind":            "a kind from a fixed list, sent as a query parameter",
	"cluster_type":           "on a resource, a reference that may be a self-link with slashes, parsed before any part of it is used. The catalog data sources take a bare id, and are checked",
	"remote_vpc":             "a reference that may be a self-link, sent in a request body",
	"address_pool":           "a reference that may be a self-link, sent in a request body",
	"version_id":             "a UUID, which the client takes as a UUID, so it can't carry a path character",
	"access_level":           "a value from a fixed list, sent in a request body",
	"mount_path":             "a filesystem path, sent in a request body",
	"type":                   "a value from a fixed list, sent in a request body",
	"guid":                   "an InfiniBand GUID, sent in a request body",
	"mac_address":            "a MAC address, sent in a request body",
	"sudo":                   "a sudoers rule, sent in a request body",
	"username":               "a login name, sent in a request body",
	"ssh_public_keys":        "public keys, sent in a request body",
	"service_type":           "a value from a fixed list, sent in a request body",
	"vlan_id":                "a VLAN number or `auto`, sent in a request body",
	"partition":              "a fabric partition name, sent in a request body",
	"pkey":                   "an InfiniBand partition key, sent in a request body",
	"action":                 "a value from a fixed list, sent in a request body",
	"protocol":               "a value from a fixed list, sent in a request body",
	"name":                   "a rule's name, sent in a request body",
	"source_prefix":          "a CIDR, sent in a request body",
	"destination_prefix":     "a CIDR, sent in a request body",
	"source_port_range":      "a port range, sent in a request body",
	"destination_port_range": "a port range, sent in a request body",
	"exclude_ranges":         "CIDRs, sent in a request body",
	"dns_servers":            "addresses, sent in a request body",
	"create":                 "a duration in a timeouts block",
	"update":                 "a duration in a timeouts block",
	"delete":                 "a duration in a timeouts block",
	"scope":                  "a level from a fixed list, sent in a request body",
	"open":                   "a duration in a timeouts block",
	"url":                    "an endpoint URL, sent in a request body or compared with a listed one, never part of a path",
	"event_types":            "event type names, held to the catalog's pattern, sent in a request body",
	"secret_wo":              "a signing secret, write-only, sent in a request body",
	"on_destroy_busy":        "a value from a fixed list, sent nowhere: it decides what a destroy does",
}

// identifierSurface is one registered schema, walked through its methods so
// the resource, data source and ephemeral schema packages need no case each.
type identifierSurface struct {
	kind, name string
	schema     any
}

func identifierSurfaces(t *testing.T) []identifierSurface {
	t.Helper()
	ctx := context.Background()
	p := New("test")()
	var out []identifierSurface
	for _, f := range p.Resources(ctx) {
		r := f()
		var md resource.MetadataResponse
		r.Metadata(ctx, resource.MetadataRequest{ProviderTypeName: ProviderTypeName}, &md)
		var s resource.SchemaResponse
		r.Schema(ctx, resource.SchemaRequest{}, &s)
		out = append(out, identifierSurface{"resource", md.TypeName, s.Schema})
	}
	for _, f := range p.DataSources(ctx) {
		d := f()
		var md datasource.MetadataResponse
		d.Metadata(ctx, datasource.MetadataRequest{ProviderTypeName: ProviderTypeName}, &md)
		var s datasource.SchemaResponse
		d.Schema(ctx, datasource.SchemaRequest{}, &s)
		out = append(out, identifierSurface{"data source", md.TypeName, s.Schema})
	}
	ep, ok := p.(fwprovider.ProviderWithEphemeralResources)
	if !ok {
		t.Fatal("the provider registers no ephemeral resources")
	}
	for _, f := range ep.EphemeralResources(ctx) {
		e := f()
		var md ephemeral.MetadataResponse
		e.Metadata(ctx, ephemeral.MetadataRequest{ProviderTypeName: ProviderTypeName}, &md)
		var s ephemeral.SchemaResponse
		e.Schema(ctx, ephemeral.SchemaRequest{}, &s)
		out = append(out, identifierSurface{"ephemeral resource", md.TypeName, s.Schema})
	}
	lp, ok := p.(fwprovider.ProviderWithListResources)
	if !ok {
		t.Fatal("the provider registers no list resources")
	}
	for _, f := range lp.ListResources(ctx) {
		l := f()
		var md resource.MetadataResponse
		l.Metadata(ctx, resource.MetadataRequest{ProviderTypeName: ProviderTypeName}, &md)
		var s list.ListResourceSchemaResponse
		l.ListResourceConfigSchema(ctx, list.ListResourceSchemaRequest{}, &s)
		out = append(out, identifierSurface{"list resource", md.TypeName, s.Schema})
	}
	return out
}

// configurableStrings calls fn for every attribute a configuration can set
// that holds a string or a list or set of them, at any depth.
func configurableStrings(v reflect.Value, prefix string, fn func(path string, a any)) {
	for _, method := range []string{"GetAttributes", "GetBlocks"} {
		get := v.MethodByName(method)
		if !get.IsValid() {
			continue
		}
		children := get.Call(nil)[0]
		for it := children.MapRange(); it.Next(); {
			name := prefix + it.Key().String()
			child := it.Value().Elem()
			if nested := child.MethodByName("GetNestedObject"); nested.IsValid() {
				configurableStrings(nested.Call(nil)[0], name+".", fn)
				continue
			}
			a := child.Interface()
			c, ok := a.(interface {
				IsRequired() bool
				IsOptional() bool
			})
			if !ok || (!c.IsRequired() && !c.IsOptional()) {
				continue
			}
			switch a.(type) {
			case interface{ StringValidators() []validator.String },
				interface{ ListValidators() []validator.List },
				interface{ SetValidators() []validator.Set }:
				fn(name, a)
			}
		}
	}
}

// identifierShapes are the forms an id takes in the contract: the common one,
// an instance's, which has its own pattern, a new service account's, which
// has to start `sa-`, and a UUID. An attribute has to accept one of them, or a
// check that refused everything would pass.
var identifierShapes = []string{
	"valid-id", "inst-0123456789abcdef0123456789abcdef", "sa-valid-id",
	// A webhook subscription's, which the platform names by UUID.
	"0f8fad5b-d9cb-469f-a165-70867728950e",
}

// refusesNonIdentifier runs an attribute's own validators on `..` and on the
// identifierShapes, each as one value or as the only element of a list or set,
// and reports whether the first is refused and one of the shapes isn't. It asks the validators
// rather than looking for a particular one, so any check that does the job
// counts.
func refusesNonIdentifier(a any) bool {
	ctx := context.Background()
	p := path.Root("x")
	switch a := a.(type) {
	case interface{ StringValidators() []validator.String }:
		run := func(v string) bool {
			var resp validator.StringResponse
			for _, val := range a.StringValidators() {
				val.ValidateString(ctx, validator.StringRequest{Path: p, ConfigValue: types.StringValue(v)}, &resp)
			}
			return resp.Diagnostics.HasError()
		}
		return run("..") && slices.ContainsFunc(identifierShapes, func(v string) bool { return !run(v) })
	case interface{ ListValidators() []validator.List }:
		run := func(v string) bool {
			list, _ := types.ListValueFrom(ctx, types.StringType, []string{v})
			var resp validator.ListResponse
			for _, val := range a.ListValidators() {
				val.ValidateList(ctx, validator.ListRequest{Path: p, ConfigValue: list}, &resp)
			}
			return resp.Diagnostics.HasError()
		}
		return run("..") && slices.ContainsFunc(identifierShapes, func(v string) bool { return !run(v) })
	case interface{ SetValidators() []validator.Set }:
		run := func(v string) bool {
			set, _ := types.SetValueFrom(ctx, types.StringType, []string{v})
			var resp validator.SetResponse
			for _, val := range a.SetValidators() {
				val.ValidateSet(ctx, validator.SetRequest{Path: p, ConfigValue: set}, &resp)
			}
			return resp.Diagnostics.HasError()
		}
		return run("..") && slices.ContainsFunc(identifierShapes, func(v string) bool { return !run(v) })
	}
	return false
}

// Every identifier a configuration sets is refused at plan time when it isn't
// one: a data source's argument, a resource's id and the parent it lives
// under, and the region and project every one of them takes.
func TestEveryIdentifierIsCheckedAtPlanTime(t *testing.T) {
	var unchecked []string
	for _, s := range identifierSurfaces(t) {
		configurableStrings(reflect.ValueOf(s.schema), "", func(attr string, a any) {
			leaf := attr[strings.LastIndex(attr, ".")+1:]
			if _, listed := notAnIdentifier[leaf]; listed && (leaf != "cluster_type" || s.kind == "resource") {
				return
			}
			if !refusesNonIdentifier(a) {
				unchecked = append(unchecked, s.kind+" "+s.name+"."+attr)
			}
		})
	}
	slices.Sort(unchecked)
	for _, u := range unchecked {
		t.Errorf("%s takes `..` at plan time: give it urlSafeIdentifier, or list it in notAnIdentifier with why", u)
	}
}

// An entry nothing carries any more would wave through the next attribute
// that takes its name, unexamined. So every name listed is still in use.
func TestEveryNameThatIsNotAnIdentifierIsStillInUse(t *testing.T) {
	used := map[string]bool{}
	for _, s := range identifierSurfaces(t) {
		configurableStrings(reflect.ValueOf(s.schema), "", func(attr string, _ any) {
			used[attr[strings.LastIndex(attr, ".")+1:]] = true
		})
	}
	for name := range notAnIdentifier {
		if !used[name] {
			t.Errorf("notAnIdentifier lists %q, which no configuration can set any more", name)
		}
	}
}
