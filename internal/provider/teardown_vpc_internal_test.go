package provider

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
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

// unscoped is a planned value whose configuration leaves the region and
// project to the provider.
func unscoped(planned map[string]tftypes.Value) map[string]tftypes.Value {
	out := maps.Clone(planned)
	out["region"], out["project"] = tftypes.NewValue(tftypes.String, nil), tftypes.NewValue(tftypes.String, nil)
	return out
}

func heldVPCPlan(t *testing.T, r resource.ResourceWithModifyPlan, prior, planned map[string]tftypes.Value) resource.ModifyPlanResponse {
	t.Helper()
	pair := pairOf(t, r, prior, planned)
	if prior != nil && planned == nil {
		pair.plan.Raw = tftypes.NewValue(pair.plan.Raw.Type(), nil)
	}
	req := resource.ModifyPlanRequest{
		Config: tfsdk.Config{Raw: pair.plan.Raw, Schema: pair.plan.Schema},
		Plan:   pair.plan,
		State:  pair.state,
	}
	freshPrivate(t, &req.Private, nil)
	resp := resource.ModifyPlanResponse{Plan: pair.plan, Private: req.Private}
	r.ModifyPlan(context.Background(), req, &resp)
	return resp
}

// goneVPCWarnings are the attributes the plan warned on about a gone VPC.
func goneVPCWarnings(t *testing.T, diags diag.Diagnostics) []string {
	t.Helper()
	if diags.HasError() {
		t.Fatalf("the plan has an error: %v", diags)
	}
	var on []string
	for _, d := range diags.Warnings() {
		if d.Summary() != "VPC gone after its owner's teardown started" {
			continue
		}
		if !strings.Contains(d.Detail(), "Applying this plan will fail for this resource. "+
			"Run `terraform destroy` to finish the teardown.") {
			t.Errorf("the warning doesn't say what happens next: %s", d.Detail())
		}
		withPath, ok := d.(diag.DiagnosticWithPath)
		if !ok {
			t.Fatalf("the warning has no attribute: %v", d)
		}
		on = append(on, withPath.Path().String())
	}
	return on
}

func TestWarnHeldVPCWrite(t *testing.T) {
	t.Parallel()
	target := clusterTarget{region: "us-texas-1", project: "demo"}
	meta := &Meta{}
	meta.vpcTeardown.record(target, "vpc-gone", true)
	meta.vpcTeardown.record(clusterTarget{region: "us-texas-1", project: "other"}, "vpc-elsewhere", true)
	defaulted := &Meta{Region: target.region, Project: target.project}
	defaulted.vpcTeardown.record(target, "vpc-gone", true)
	peering := func(vpc, remote string) map[string]tftypes.Value {
		return map[string]tftypes.Value{"region": str(target.region), "project": str(target.project),
			"id": str("p"), "vpc": str(vpc), "remote_vpc": str(remote)}
	}
	binding := map[string]tftypes.Value{"region": str(target.region), "project": str(target.project),
		"vpc": str("vpc-gone")}
	cases := []struct {
		name           string
		r              resource.ResourceWithModifyPlan
		prior, planned map[string]tftypes.Value
		want           []string
	}{
		{"a peering created under the gone VPC", &vpcPeeringResource{meta: meta},
			nil, peering("vpc-gone", "vpc-live"), []string{"vpc"}},
		{"a peering created toward it by id", &vpcPeeringResource{meta: meta},
			nil, peering("vpc-live", "vpc-gone"), []string{"remote_vpc"}},
		{"a peering created toward it by self_link", &vpcPeeringResource{meta: meta},
			nil, peering("vpc-live", "/v1/regions/us-texas-1/projects/demo/networking/vpcs/vpc-gone"),
			[]string{"remote_vpc"}},
		{"a peering toward a gone VPC in another project", &vpcPeeringResource{meta: meta},
			nil, peering("vpc-live", "/v1/regions/us-texas-1/projects/other/networking/vpcs/vpc-elsewhere"),
			[]string{"remote_vpc"}},
		{"a binding created on it", &vpcSecurityGroupsResource{meta: meta}, nil, binding, []string{"vpc"}},
		{"a peering created on it whose scope the provider fills in", &vpcPeeringResource{meta: defaulted},
			nil, unscoped(peering("vpc-live", "vpc-gone")), []string{"remote_vpc"}},
		{"a peering on live VPCs", &vpcPeeringResource{meta: meta}, nil, peering("vpc-live", "vpc-other"), nil},
		{"the same id in another project", &vpcPeeringResource{meta: meta},
			nil, peering("vpc-live", "/v1/regions/us-texas-1/projects/other/networking/vpcs/vpc-gone"), nil},
		{"an unchanged peering", &vpcPeeringResource{meta: meta},
			peering("vpc-gone", "vpc-live"), peering("vpc-gone", "vpc-live"), nil},
		{"an unchanged peering whose scope the provider fills in", &vpcPeeringResource{meta: defaulted},
			peering("vpc-live", "vpc-gone"), unscoped(peering("vpc-live", "vpc-gone")), nil},
		{"a destroy", &vpcPeeringResource{meta: meta}, peering("vpc-gone", "vpc-live"), nil, nil},
		{"a run that saw no teardown", &vpcPeeringResource{meta: &Meta{}}, nil, peering("vpc-gone", "vpc-live"), nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := goneVPCWarnings(t, heldVPCPlan(t, tc.r, tc.prior, tc.planned).Diagnostics)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("warned on %v, want %v", got, tc.want)
			}
		})
	}
}

func TestAddMissingVPCError(t *testing.T) {
	t.Parallel()
	target := clusterTarget{region: "us-texas-1", project: "demo"}
	envelope := func(message, reason string) string {
		return fmt.Sprintf(`{"error":{"code":0,"message":%q,"requestId":"r1","details":[{"reason":%q,"message":%q}]}}`,
			message, reason, message)
	}
	const advice = "If its owner's teardown started, run `terraform destroy` to finish it. " +
		"Otherwise check the VPC id in "
	cases := []struct {
		name          string
		status        int
		body          string
		remote        string
		wantSummary   string
		wantInDetail  []string
		wantUnhandled bool
	}{
		{name: "the service's 404 for the local VPC", status: 404,
			body: envelope(`not found: vpc "vpc-gone"`, "NOT_FOUND"), remote: "vpc-live",
			wantSummary: `Could not create the VPC peering because VPC "vpc-gone" doesn't exist`,
			wantInDetail: []string{"The platform answered 404 Not Found.", `not found: vpc "vpc-gone"`,
				`VPC "vpc-gone" doesn't exist in project "demo" of region "us-texas-1". ` + advice + "`vpc`.",
				"Request id: r1"}},
		{name: "a 422 for a remote VPC in this project", status: 422,
			body:        envelope(`validation: remoteVpc "vpc-gone" not found in project "demo"`, "INVALID_ARGUMENT"),
			remote:      "vpc-gone",
			wantSummary: `Could not create the VPC peering because VPC "vpc-gone" doesn't exist`,
			wantInDetail: []string{"The platform answered 422 Unprocessable Entity.",
				`VPC "vpc-gone" doesn't exist in project "demo" of region "us-texas-1". ` + advice + "`remote_vpc`."}},
		{name: "a 422 for a remote VPC named by self_link", status: 422,
			body:         envelope(`validation: remoteVpc "vpc-gone" not found in project "demo"`, "INVALID_ARGUMENT"),
			remote:       "/v1/regions/us-texas-1/projects/demo/networking/vpcs/vpc-gone",
			wantSummary:  `Could not create the VPC peering because VPC "vpc-gone" doesn't exist`,
			wantInDetail: []string{advice + "`remote_vpc`."}},
		{name: "a 422 for a remote VPC in another project that says not found", status: 422,
			body:        envelope(`validation: remoteVpc "vpc-gone" not found in project "other"`, "INVALID_ARGUMENT"),
			remote:      "/v1/regions/us-texas-1/projects/other/networking/vpcs/vpc-gone",
			wantSummary: `Could not create the VPC peering because VPC "vpc-gone" doesn't exist`,
			wantInDetail: []string{`VPC "vpc-gone" doesn't exist in project "other" of region "us-texas-1". ` +
				advice + "`remote_vpc`."}},
		{name: "a 404 without the service's envelope", status: 404, body: "404 page not found",
			remote: "vpc-live", wantUnhandled: true},
		{name: "the service's 404 for the project", status: 404,
			body: envelope(`not found: project "demo"`, "NOT_FOUND"), remote: "vpc-live", wantUnhandled: true},
		{name: "the service's 404 for another VPC", status: 404,
			body: envelope(`not found: vpc "vpc-somebody-elses"`, "NOT_FOUND"), remote: "vpc-live", wantUnhandled: true},
		{name: "a 422 another project gives, which doesn't say the VPC is missing", status: 422,
			body: envelope(`validation: remoteVpc "vpc-gone" is not available for peering: it must name an `+
				`existing, active VPC in the same region`, "INVALID_ARGUMENT"),
			remote: "/v1/regions/us-texas-1/projects/other/networking/vpcs/vpc-gone", wantUnhandled: true},
		{name: "a 422 about another VPC", status: 422,
			body:   envelope(`validation: remoteVpc "vpc-other" not found in project "demo"`, "INVALID_ARGUMENT"),
			remote: "vpc-gone", wantUnhandled: true},
		{name: "a 422 with no remote VPC sent", status: 422,
			body:          envelope(`validation: remoteVpc "vpc-gone" not found in project "demo"`, "INVALID_ARGUMENT"),
			wantUnhandled: true},
		{name: "a 409", status: 409, body: envelope("conflict", "CONFLICT_IN_USE"), remote: "vpc-gone",
			wantUnhandled: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var diags diag.Diagnostics
			handled := addMissingVPCError(&diags, "create the VPC peering", target,
				answer(tc.status, "", tc.body), []byte(tc.body), "vpc-gone", tc.remote)
			if handled == tc.wantUnhandled {
				t.Fatalf("handled = %v, diagnostics %v", handled, diags)
			}
			if tc.wantUnhandled {
				if len(diags) != 0 {
					t.Fatalf("an unhandled answer added %v", diags)
				}
				return
			}
			if len(diags) != 1 || diags[0].Severity() != diag.SeverityError || diags[0].Summary() != tc.wantSummary {
				t.Fatalf("got %v, want one error %q", diags, tc.wantSummary)
			}
			for _, want := range tc.wantInDetail {
				if !strings.Contains(diags[0].Detail(), want) {
					t.Errorf("the detail doesn't say %q:\n%s", want, diags[0].Detail())
				}
			}
		})
	}
}
