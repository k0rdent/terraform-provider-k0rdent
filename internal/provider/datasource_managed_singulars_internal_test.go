package provider

import (
	"context"
	"testing"

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
	data.fill(decodeInto[k0rdent.SecurityGroup](t, `{"id":"default","ownerKind":"vpc",
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
