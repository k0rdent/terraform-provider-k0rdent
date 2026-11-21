package provider

import (
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/types"
)

func TestHoldVPCThroughTeardown(t *testing.T) {
	t.Parallel()
	target := clusterTarget{region: "r", project: "p"}
	otherScope := clusterTarget{region: "other", project: "p"}
	prior := types.StringValue("vpc-old")

	t.Run("released owner holds and scopes the exception", func(t *testing.T) {
		meta := &Meta{}
		got := holdVPCThroughTeardown(meta, target, true, types.StringNull(), prior)
		if !got.Equal(prior) || !meta.vpcTeardown.held(target, prior.ValueString()) {
			t.Fatal("the prior VPC wasn't held for the released owner")
		}
		if meta.vpcTeardown.held(otherScope, prior.ValueString()) {
			t.Fatal("the exception crosses a region boundary")
		}
	})

	t.Run("active owner reports a missing VPC and clears a hold", func(t *testing.T) {
		meta := &Meta{}
		holdVPCThroughTeardown(meta, target, true, types.StringNull(), prior)
		got := holdVPCThroughTeardown(meta, target, false, types.StringNull(), prior)
		if !got.IsNull() {
			t.Fatalf("active read kept %q, want null", got.ValueString())
		}
		if meta.vpcTeardown.held(target, prior.ValueString()) {
			t.Fatal("the active read left the 404 exception held")
		}
	})

	t.Run("a fresh answer replaces the hold", func(t *testing.T) {
		meta := &Meta{}
		fresh := types.StringValue("vpc-new")
		got := holdVPCThroughTeardown(meta, target, true, fresh, prior)
		if !got.Equal(fresh) || meta.vpcTeardown.held(target, prior.ValueString()) {
			t.Fatal("a current VPC answer didn't replace the held ID")
		}
	})

	t.Run("there is nothing to hold without a prior ID", func(t *testing.T) {
		meta := &Meta{}
		got := holdVPCThroughTeardown(meta, target, true, types.StringNull(), types.StringNull())
		if !got.IsNull() || len(meta.vpcTeardown.ids) != 0 {
			t.Fatal("a missing prior ID produced a 404 exception")
		}
	})
}
