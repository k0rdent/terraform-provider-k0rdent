package provider

import (
	"context"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

func typeName(r resource.Resource) string {
	var md resource.MetadataResponse
	r.Metadata(context.Background(), resource.MetadataRequest{ProviderTypeName: ProviderTypeName}, &md)
	return md.TypeName
}

func identitySchemaOf(t *testing.T, r resource.Resource) resource.IdentitySchemaResponse {
	t.Helper()
	ir, ok := r.(resource.ResourceWithIdentity)
	if !ok {
		t.Fatalf("%s has no resource identity", typeName(r))
	}
	var resp resource.IdentitySchemaResponse
	ir.IdentitySchema(context.Background(), resource.IdentitySchemaRequest{}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("%s: %v", typeName(r), resp.Diagnostics)
	}
	return resp
}

func emptyState(s schema.Schema) tfsdk.State {
	return tfsdk.State{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(context.Background()), nil)}
}

// Every identity attribute is copied from the state attribute of the same
// name, so each one has to exist there as a string. A resource added without
// an identity, or with one naming an attribute its state lacks, fails here
// before it fails in a tenant's refresh.
func TestEveryResourceHasAnIdentityItsStateCanFill(t *testing.T) {
	t.Parallel()
	for _, rr := range registeredResources(t) {
		r := rr.resource
		identity := identitySchemaOf(t, r).IdentitySchema
		attrs := schemaOf(t, r).Attributes
		required := 0
		for name, a := range identity.Attributes {
			sa, ok := attrs[name]
			if !ok {
				t.Errorf("%s: identity attribute %q isn't in the resource's schema", typeName(r), name)
				continue
			}
			if _, ok := sa.(schema.StringAttribute); !ok {
				t.Errorf("%s: identity attribute %q is a %T in state, and identities here are strings",
					typeName(r), name, sa)
			}
			if a.IsRequiredForImport() {
				required++
			}
			if a.GetDescription() == "" {
				t.Errorf("%s: identity attribute %q has no description for its page", typeName(r), name)
			}
		}
		if required == 0 {
			t.Errorf("%s: an identity with nothing required names nothing", typeName(r))
		}
	}
}

func importInto(t *testing.T, r resource.Resource, id string, identity map[string]string) (tfsdk.State, bool) {
	t.Helper()
	ctx := context.Background()
	req := resource.ImportStateRequest{ID: id}
	if identity != nil {
		is := identitySchemaOf(t, r).IdentitySchema
		ri := tfsdk.ResourceIdentity{Schema: is, Raw: tftypes.NewValue(is.Type().TerraformType(ctx), nil)}
		for k, v := range identity {
			if d := ri.SetAttribute(ctx, path.Root(k), v); d.HasError() {
				t.Fatalf("setting identity %s: %v", k, d)
			}
		}
		req.Identity = &ri
	}
	resp := resource.ImportStateResponse{State: emptyState(schemaOf(t, r))}
	ir, ok := r.(resource.ResourceWithImportState)
	if !ok {
		t.Fatalf("%s has no import", typeName(r))
	}
	ir.ImportState(ctx, req, &resp)
	return resp.State, !resp.Diagnostics.HasError()
}

func importedAttr(t *testing.T, s tfsdk.State, name string) string {
	t.Helper()
	var v types.String
	if d := s.GetAttribute(context.Background(), path.Root(name), &v); d.HasError() {
		t.Fatalf("reading %s: %v", name, d)
	}
	return v.ValueString()
}

// The identity form is the string form with its parts named, so the two have
// to land in the same state. A difference would mean the plan after an
// identity import isn't the plan after a string import.
func TestImportByIdentityLandsWhereTheStringFormDoes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		r        resource.Resource
		id       string
		identity map[string]string
		want     []string
	}{
		{newClusterResource(), "us-texas-1/demo/c1",
			map[string]string{"region": "us-texas-1", "project": "demo", "id": "c1"},
			[]string{"region", "project", "id"}},
		{newSubnetResource(), "us-texas-1/demo/fabric/pods",
			map[string]string{"region": "us-texas-1", "project": "demo", "network": "fabric", "id": "pods"},
			[]string{"region", "project", "network", "id"}},
		{vpcPeeringResourceWith(nil)(), "us-texas-1/demo/vpc-a/to-b",
			map[string]string{"region": "us-texas-1", "project": "demo", "vpc": "vpc-a", "id": "to-b"},
			[]string{"region", "project", "vpc", "id"}},
		{newVPCSecurityGroupsResource(), "us-texas-1/demo/vpc-a",
			map[string]string{"region": "us-texas-1", "project": "demo", "vpc": "vpc-a"},
			[]string{"region", "project", "vpc"}},
		{newProjectResource(), "org-demo/team",
			map[string]string{"organization_id": "org-demo", "id": "team"},
			[]string{"organization_id", "id"}},
	}
	for _, c := range cases {
		byString, ok := importInto(t, c.r, c.id, nil)
		if !ok {
			t.Fatalf("%s: the string form %q was refused", typeName(c.r), c.id)
		}
		byIdentity, ok := importInto(t, c.r, "", c.identity)
		if !ok {
			t.Fatalf("%s: the identity %v was refused", typeName(c.r), c.identity)
		}
		for _, name := range c.want {
			if got, want := importedAttr(t, byIdentity, name), importedAttr(t, byString, name); got != want || got == "" {
				t.Errorf("%s.%s: the identity import wrote %q and the string import %q", typeName(c.r), name, got, want)
			}
		}
	}
}

// An identity's values become path segments, the same as the string form's,
// so what the string form can't express is refused here too rather than sent.
func TestImportByIdentityRefusesWhatTheStringFormCannotSay(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		r        resource.Resource
		identity map[string]string
	}{
		"a slash in the id":      {newClusterResource(), map[string]string{"id": "a/b"}},
		"a blank id":             {newFileStoreResource(), map[string]string{"id": " "}},
		"a malformed region":     {newNetworkResource(), map[string]string{"region": "US Texas", "id": "n"}},
		"a slash in the parent":  {newSubnetResource(), map[string]string{"network": "a/b", "id": "s"}},
		"a slash in the project": {newProjectResource(), map[string]string{"organization_id": "o/x", "id": "p"}},
	}
	for name, c := range cases {
		if _, ok := importInto(t, c.r, "", c.identity); ok {
			t.Errorf("%s: %s was accepted", name, typeName(c.r))
		}
	}
}

// The identity follows state, and a row with its id still unknown, a create
// that failed before the platform named the resource, gets none: an identity
// half filled in would be refused as a change once the rest arrived.
func TestSyncIdentityWaitsForEveryPart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newClusterResource()
	is := identitySchemaOf(t, r).IdentitySchema
	newIdentity := func() *tfsdk.ResourceIdentity {
		return &tfsdk.ResourceIdentity{Schema: is, Raw: tftypes.NewValue(is.Type().TerraformType(ctx), nil)}
	}

	partial := emptyState(schemaOf(t, r))
	partial.SetAttribute(ctx, path.Root("region"), "us-texas-1")
	partial.SetAttribute(ctx, path.Root("project"), "demo")
	identity := newIdentity()
	var diags diag.Diagnostics
	syncIdentity(ctx, nil, identity, &diags, &partial)
	if !identity.Raw.IsFullyNull() {
		t.Errorf("an identity was written from a row with no id: %s", identity.Raw)
	}

	full := partial
	full.Raw = partial.Raw.Copy()
	full.SetAttribute(ctx, path.Root("id"), "c1")
	gone := emptyState(schemaOf(t, r))
	identity = newIdentity()
	syncIdentity(ctx, nil, identity, &diags, &gone, &full)
	if diags.HasError() {
		t.Fatal(diags)
	}
	var id types.String
	identity.GetAttribute(ctx, path.Root("id"), &id)
	if id.ValueString() != "c1" || !strings.Contains(identity.Raw.String(), "us-texas-1") {
		t.Errorf("a read that removed the row kept no identity from the prior state: %s", identity.Raw)
	}
}

// A refused identity names the attribute that was wrong, rather than quoting
// a string the tenant never wrote.
func TestAProjectIdentityWithASlashNamesTheAttribute(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newProjectResource()
	is := identitySchemaOf(t, r).IdentitySchema
	ri := tfsdk.ResourceIdentity{Schema: is, Raw: tftypes.NewValue(is.Type().TerraformType(ctx), nil)}
	ri.SetAttribute(ctx, path.Root("organization_id"), "org/x")
	ri.SetAttribute(ctx, path.Root("id"), "team")
	resp := resource.ImportStateResponse{State: emptyState(schemaOf(t, r))}
	ir, ok := r.(resource.ResourceWithImportState)
	if !ok {
		t.Fatal("the project has no import")
	}
	ir.ImportState(ctx, resource.ImportStateRequest{Identity: &ri}, &resp)
	if !resp.Diagnostics.HasError() {
		t.Fatal("a slash in organization_id was accepted")
	}
	got := resp.Diagnostics.Errors()[0]
	if !strings.Contains(got.Detail(), "organization_id") || strings.Contains(got.Detail(), `"/"`) {
		t.Errorf("the refusal doesn't name organization_id: %s: %s", got.Summary(), got.Detail())
	}
}
