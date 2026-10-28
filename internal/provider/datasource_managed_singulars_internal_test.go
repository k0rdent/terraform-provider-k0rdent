package provider

import (
	"context"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	dsschema "github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	rschema "github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/k0rdent"
)

func TestSubnetDataReadsTagsAndTheStableReason(t *testing.T) {
	var data subnetDataSourceModel
	diags := data.fill(context.Background(), decodeInto[k0rdent.Subnet](t, `{"id":"s","uid":"00000000-0000-4000-8000-000000000002",
		"selfLink":"/s","network":"/n","addressPool":"/p","prefixLength":24,"state":"failed",
		"stateReason":"INVALID_CONFIGURATION","statusReason":"free text","tags":{"team":"ml"}}`))
	if diags.HasError() {
		t.Fatal(diags)
	}
	if got := data.StateReason.ValueString(); got != "INVALID_CONFIGURATION" {
		t.Errorf("state_reason = %q", got)
	}
	if got := data.Tags.Elements()["team"]; got == nil || got.String() != `"ml"` {
		t.Errorf("tags = %s", data.Tags)
	}
}

func TestSubnetDataLeavesAbsentTagsNull(t *testing.T) {
	var data subnetDataSourceModel
	if diags := data.fill(context.Background(), decodeInto[k0rdent.Subnet](t, `{"id":"s","uid":"00000000-0000-4000-8000-000000000002",
		"selfLink":"/s","network":"/n","addressPool":"/p","prefixLength":24}`)); diags.HasError() {
		t.Fatal(diags)
	}
	if !data.Tags.IsNull() || !data.StateReason.IsNull() {
		t.Errorf("tags %s, state_reason %s: want both null", data.Tags, data.StateReason)
	}
}

func TestSecurityGroupDataNamesTheOwningVPC(t *testing.T) {
	var data securityGroupDataSourceModel
	data.fill(decodeInto[k0rdent.SecurityGroup](t, `{"id":"vpc-a-6484e673-default","ownerKind":"vpc",
		"ownerId":"ad9e7bf7-973b-4063-a309-8f2da3d56046","state":"active","ruleCount":0}`))

	if got := data.OwnerID.ValueString(); got != "ad9e7bf7-973b-4063-a309-8f2da3d56046" {
		t.Errorf("owner_id = %q", got)
	}

	var tenant securityGroupDataSourceModel
	tenant.fill(decodeInto[k0rdent.SecurityGroup](t, `{"id":"mine","state":"active","ruleCount":0}`))
	if !tenant.OwnerKind.IsNull() || !tenant.OwnerID.IsNull() {
		t.Errorf("owner_kind %s, owner_id %s: want both null on a tenant group", tenant.OwnerKind, tenant.OwnerID)
	}
}

// The rules, as k0rdent API v1.3.0-rc.3 answered them for a VPC's default
// group with two subnet prefixes, trimmed to one rule a side, then a tenant
// rule carrying every optional field.
func TestSecurityGroupDataReadsTheRules(t *testing.T) {
	var data securityGroupDataSourceModel
	data.fill(decodeInto[k0rdent.SecurityGroup](t, `{"id":"vpc-a-6484e673-default","ownerKind":"vpc",
		"ownerId":"e7aff1bc-e01d-4ce2-bf63-2361cc777830","state":"active","ruleCount":3,
		"rules":{"ingress":[
			{"name":"intra-vpc-1","protocol":"any","action":"permit","sourcePrefix":"10.0.0.64/26","destinationPrefix":"10.210.0.0/24"},
			{"protocol":"tcp","action":"deny","sourcePrefix":"0.0.0.0/0","destinationPrefix":"10.0.0.0/8",
			 "sourcePortRange":"1024-65535","destinationPortRange":"22"}],
		"egress":[
			{"name":"intra-vpc-egress-0","protocol":"any","action":"permit","sourcePrefix":"0.0.0.0/0","destinationPrefix":"10.0.0.64/26"}]}}`))

	want := ruleModel{
		Name: types.StringValue("intra-vpc-1"), Action: types.StringValue("permit"), Protocol: types.StringValue("any"),
		SourcePrefix: types.StringValue("10.0.0.64/26"), DestinationPrefix: types.StringValue("10.210.0.0/24"),
		SourcePortRange: types.StringNull(), DestinationPortRange: types.StringNull(),
	}
	if len(data.Ingress) != 2 || data.Ingress[0] != want {
		t.Fatalf("ingress = %+v, want %+v first", data.Ingress, want)
	}
	// Absent optional fields are null, and present ones are as sent.
	tenant := data.Ingress[1]
	if !tenant.Name.IsNull() || tenant.SourcePortRange.ValueString() != "1024-65535" ||
		tenant.DestinationPortRange.ValueString() != "22" || tenant.Action.ValueString() != "deny" {
		t.Errorf("ingress[1] = %+v", tenant)
	}
	if len(data.Egress) != 1 || data.Egress[0].Name.ValueString() != "intra-vpc-egress-0" ||
		data.Egress[0].SourcePrefix.ValueString() != "0.0.0.0/0" {
		t.Errorf("egress = %+v", data.Egress)
	}
}

// A VPC with no subnet prefixes gets a default group with no rules. Both lists
// read as empty, not null, so length() on them is 0 rather than an error.
func TestSecurityGroupDataReadsAnEmptyGroupAsEmptyLists(t *testing.T) {
	var data securityGroupDataSourceModel
	data.fill(decodeInto[k0rdent.SecurityGroup](t, `{"id":"vpc-b-a7b5b819-default","ownerKind":"vpc",
		"state":"active","ruleCount":0,"rules":{"ingress":[],"egress":[]}}`))
	if data.Ingress == nil || data.Egress == nil || len(data.Ingress)+len(data.Egress) != 0 {
		t.Errorf("ingress %#v, egress %#v: want two empty, non-nil lists", data.Ingress, data.Egress)
	}
}

// The data source's rule attributes are the resource's, read only: the same
// names, each a string, with the same description. The resource's docs add
// the constraints its validators check, which a read has no use for.
func TestSecurityGroupDataRulesMatchTheResource(t *testing.T) {
	ctx := context.Background()
	var ds datasource.SchemaResponse
	(&securityGroupDataSource{}).Schema(ctx, datasource.SchemaRequest{}, &ds)
	var rs resource.SchemaResponse
	(&securityGroupResource{}).Schema(ctx, resource.SchemaRequest{}, &rs)

	for _, direction := range []string{"ingress", "egress"} {
		list, ok := ds.Schema.Attributes[direction].(dsschema.ListNestedAttribute)
		if !ok || !list.Computed {
			t.Fatalf("%s: %T, want a computed list", direction, ds.Schema.Attributes[direction])
		}
		block, ok := rs.Schema.Blocks[direction].(rschema.ListNestedBlock)
		if !ok {
			t.Fatalf("resource %s: %T, want a list block", direction, rs.Schema.Blocks[direction])
		}
		if list.MarkdownDescription != block.MarkdownDescription {
			t.Errorf("%s: description %q, the resource's is %q", direction, list.MarkdownDescription, block.MarkdownDescription)
		}
		got, want := list.NestedObject.Attributes, block.NestedObject.Attributes
		if len(got) != len(want) {
			t.Errorf("%s: %d attributes, the resource has %d", direction, len(got), len(want))
		}
		for name, w := range want {
			g, ok := got[name].(dsschema.StringAttribute)
			if !ok {
				t.Errorf("%s.%s: %T, want a string", direction, name, got[name])
				continue
			}
			if !g.Computed || !strings.HasPrefix(w.GetMarkdownDescription(), g.MarkdownDescription+" **Constraints:**") {
				t.Errorf("%s.%s: computed %v, description %q, want %q", direction, name, g.Computed,
					g.MarkdownDescription, w.GetMarkdownDescription())
			}
		}
	}
}
