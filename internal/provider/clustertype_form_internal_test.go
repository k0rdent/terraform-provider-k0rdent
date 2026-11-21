package provider

import (
	"context"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	rschema "github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// runStringValidatorDetail runs a string validator and returns its errors'
// detail text joined, for checks on what the message says.
func runStringValidatorDetail(v validator.String, value types.String) string {
	req := validator.StringRequest{Path: path.Root("attr"), ConfigValue: value}
	resp := &validator.StringResponse{}
	v.ValidateString(context.Background(), req, resp)
	var out []string
	for _, d := range resp.Diagnostics.Errors() {
		out = append(out, d.Detail())
	}
	return strings.Join(out, "\n")
}

// The form validator refuses at plan exactly the values the platform's
// create refuses at apply, the bare ClusterType name the defect reports
// included, and passes the rest. The rule is the platform's
// parseClusterTypeURI, run on the path the value carries verbatim.
func TestClusterTypeRefFormRefusesWhatTheCreateRefuses(t *testing.T) {
	t.Parallel()

	const base = "/v1/regions/us-texas-1/compute/cluster-types/"
	tests := []struct {
		name  string
		value types.String
		want  string
	}{
		{"a bare ClusterType name", types.StringValue("gpu-training"), "Not a ClusterType reference"},
		{"the reported bare name", types.StringValue("terraform-small-gb300"), "Not a ClusterType reference"},
		{"nothing", types.StringValue(""), "Not a ClusterType reference"},
		{"a missing /v1", types.StringValue("/regions/us-texas-1/compute/cluster-types/gpu-training"), "Not a ClusterType reference"},
		{"a missing region", types.StringValue("/v1/regions//compute/cluster-types/gpu-training"), "Not a ClusterType reference"},
		{"a region with a segment", types.StringValue("/v1/regions/us/texas/compute/cluster-types/gpu-training"), "Not a ClusterType reference"},
		{"a missing ClusterType id", types.StringValue(base), "Not a ClusterType reference"},
		{"an id with a segment", types.StringValue(base + "gpu/training"), "Not a ClusterType reference"},
		{"a trailing slash", types.StringValue(base + "gpu-training/"), "Not a ClusterType reference"},
		{"a versions segment with nothing after it", types.StringValue(base + "gpu-training/versions/"), "Not a ClusterType reference"},
		{"a versions segment with two segments after it", types.StringValue(base + "gpu-training/versions/1/2"), "Not a ClusterType reference"},
		{"a query", types.StringValue(base + "gpu-training?v=1"), "Not a ClusterType reference"},
		{"a fragment", types.StringValue(base + "gpu-training#x"), "Not a ClusterType reference"},

		{"a relative reference", types.StringValue(base + "gpu-training"), ""},
		{"a pinned relative reference", types.StringValue(base + "gpu-training/versions/3"), ""},
		{"an absolute URL", types.StringValue("https://api.example.com" + base + "gpu-training"), ""},
		{"a pinned absolute URL", types.StringValue("https://api.example.com" + base + "gpu-training/versions/3"), ""},
		{"a version that is not a UUID, the version validator's", types.StringValue(base + "gpu-training/versions/not-a-uuid"), ""},
		{"an unknown value", types.StringUnknown(), ""},
		{"no value", types.StringNull(), ""},
	}
	for _, tc := range tests {
		got := runStringValidator(clusterTypeRefForm{}, tc.value)
		switch tc.want {
		case "":
			if len(got) != 0 {
				t.Errorf("%s: a value the create takes was refused: %v", tc.name, got)
			}
		default:
			if len(got) == 0 || got[0] != tc.want {
				t.Errorf("%s: got %v, want [%s]", tc.name, got, tc.want)
			}
			// The way out is named: the data source whose self_link has the form.
			body := runStringValidatorDetail(clusterTypeRefForm{}, tc.value)
			if !strings.Contains(body, "self_link") || !strings.Contains(body, "/v1/regions/") {
				t.Errorf("%s: the error doesn't name the way out: %s", tc.name, body)
			}
		}
	}
}

// A value both validators could refuse says so once. The form is the deeper
// problem, so it's the form's message, not the version's.
func TestASloppyVersionsSegmentIsReportedOnce(t *testing.T) {
	t.Parallel()

	for _, value := range []string{
		"/v1/regions/us-texas-1/compute/cluster-types/gpu-training/versions/",
		"/v1/regions/us-texas-1/compute/cluster-types/gpu-training/versions/1/2",
	} {
		got := runStringValidator(clusterTypeRefForm{}, types.StringValue(value))
		got = append(got, runStringValidator(clusterTypeVersionRef{}, types.StringValue(value))...)
		if len(got) != 1 || got[0] != "Not a ClusterType reference" {
			t.Errorf("%q was reported %v, want the form refusal alone", value, got)
		}
	}
}

// A plan whose cluster_type names another region than the resource's is
// refused at plan, with both regions and a way out, when both values are
// known. Unknown or null values skip: a value a data source fills during
// the apply can't be checked yet.
func TestClusterTypeRegionMoveRefusedAtPlan(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		clusterType tftypes.Value
		region      tftypes.Value
		want        string
	}{
		{"another region", str("/v1/regions/us-east-1/compute/cluster-types/gpu-training"), str("us-texas-1"), "The cluster type is in another region"},
		{"the same region", str("/v1/regions/us-texas-1/compute/cluster-types/gpu-training"), str("us-texas-1"), ""},
		{"an unknown cluster type", tftypes.NewValue(tftypes.String, tftypes.UnknownValue), str("us-texas-1"), ""},
		{"no cluster type", tftypes.NewValue(tftypes.String, nil), str("us-texas-1"), ""},
		{"an unknown region", str("/v1/regions/us-east-1/compute/cluster-types/gpu-training"), tftypes.NewValue(tftypes.String, tftypes.UnknownValue), ""},
		{"no region", str("/v1/regions/us-east-1/compute/cluster-types/gpu-training"), tftypes.NewValue(tftypes.String, nil), ""},
		{"a malformed reference, the form validator's", str("gpu-training"), str("us-texas-1"), ""},
	}
	for _, tc := range tests {
		for _, kind := range []struct {
			name string
			r    resource.Resource
		}{{"cluster", &clusterResource{}}, {"instance group", &instanceGroupResource{}}} {
			raw, s := testValue(t, kind.r, map[string]tftypes.Value{
				"cluster_type": tc.clusterType,
				"region":       tc.region,
			})
			var diags diag.Diagnostics
			refuseClusterTypeRegionMove(context.Background(), tfsdk.Plan{Raw: raw, Schema: s.Schema}, &diags, kind.name)
			if tc.want == "" {
				if diags.HasError() {
					t.Errorf("%s, %s: refused a plan it takes: %v", tc.name, kind.name, diags.Errors())
				}
				continue
			}
			errs := diags.Errors()
			if len(errs) != 1 || errs[0].Summary() != tc.want {
				t.Errorf("%s, %s: got %v, want [%s]", tc.name, kind.name, errs, tc.want)
				continue
			}
			body := errs[0].Detail()
			for _, want := range []string{"us-east-1", "us-texas-1", "self_link", kind.name} {
				if !strings.Contains(body, want) {
					t.Errorf("%s, %s: the error doesn't name %q: %s", tc.name, kind.name, want, body)
				}
			}
		}
	}
}

// Both resources that take a `cluster_type` carry the form check, so a bare
// name is refused at plan on each. The schemas are read through the same
// interface Terraform reads them.
func TestBothClusterTypeAttributesCarryTheFormCheck(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		r    resource.Resource
	}{
		{"cluster", &clusterResource{}},
		{"instance group", &instanceGroupResource{}},
	} {
		var s resource.SchemaResponse
		tc.r.Schema(context.Background(), resource.SchemaRequest{}, &s)
		attr, ok := s.Schema.Attributes["cluster_type"].(rschema.StringAttribute)
		if !ok {
			t.Fatalf("%s has no cluster_type string attribute", tc.name)
		}
		found := false
		for _, v := range attr.Validators {
			if _, is := v.(clusterTypeRefForm); is {
				found = true
			}
		}
		if !found {
			t.Errorf("%s's cluster_type doesn't carry the ClusterType form check", tc.name)
		}
	}
}

func TestClusterTypeCreateRefSendsThePath(t *testing.T) {
	const p = "/v1/regions/us-texas-1/compute/cluster-types/gpu-training/versions/3"
	for in, want := range map[string]string{
		p:                                 p,
		"https://api.example.com" + p:     p,
		"https://api.example.com:443" + p: p,
		"gpu-training":                    "gpu-training",
	} {
		if got := clusterTypeCreateRef(in); got != want {
			t.Errorf("clusterTypeCreateRef(%q) = %q, want %q", in, got, want)
		}
	}
}
