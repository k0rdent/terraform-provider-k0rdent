package provider

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/k0rdent"
)

// The comparison this test pins is a compensation for two filed findings, and it
// is the single most consequential piece of logic in the two resources: if it is
// too strict every cluster has a permanent diff on its only required argument,
// and if it is too loose a cluster genuinely bound to a different ClusterType
// looks unchanged.
//
// Read clustertyperef.go before changing a row here. Deleting the whole thing is
// the right move the day someone runs the three creates that settle whether
// the server resolves a head reference and the contract is corrected — not loosening it further.
func TestClusterTypeRefEquivalent(t *testing.T) {
	t.Parallel()

	const (
		head      = "/v1/regions/us-texas-1/compute/cluster-types/gpu-training"
		pinned    = head + "/versions/1f2e3d4e-a5b6-7c8d-9e0f-a1b2c3d4e5f6"
		absHead   = "https://api.k0rdent.ai" + head
		absPinned = "https://api.k0rdent.ai" + pinned
		other     = "/v1/regions/us-texas-1/compute/cluster-types/cpu-general"
	)

	cases := []struct {
		name       string
		configured string
		returned   string
		want       clusterTypeRefMatch
		why        string
	}{
		{
			name: "identical", configured: head, returned: head, want: refIdentical,
			why: "the easy case, and the one the contract's create-side reading implies",
		},
		{
			name: "the platform resolved the version", configured: head, returned: pinned,
			want: refVersionResolved,
			why: "the damaging reading of the contract. Tolerated, because otherwise every cluster " +
				"created from a head reference diffs forever on an immutable field",
		},
		{
			name: "absolute in, relative out", configured: absHead, returned: head,
			want: refSameOrigin,
			why: "the only row with live evidence behind it: the console's client " +
				"observed the API returning paths against a format:uri declaration",
		},
		{
			name: "relative in, absolute out", configured: head, returned: absHead,
			want: refSameOrigin,
			why: "the same normalization the other way, in case a gateway starts " +
				"returning what the contract declares",
		},
		{
			name: "absolute in, resolved relative out", configured: absHead, returned: pinned,
			want: refVersionResolved,
			why: "both normalizations at once, which is the combination a real landscape " +
				"would produce if both of the rows above hold",
		},
		{
			name: "trailing slash", configured: head + "/", returned: head, want: refIdentical,
			why: "cosmetic, and cheap to tolerate. Reported as identical rather than as a " +
				"normalization, because neither side carried an origin",
		},
		{
			name: "a different cluster type", configured: head, returned: other, want: refMismatch,
			why: "real drift. The cluster is bound to something else and the binding is " +
				"immutable, so this has to reach the user as a replacement",
		},
		{
			name: "a different version of the same type", configured: pinned,
			returned: head + "/versions/99999999-0000-0000-0000-000000000000",
			want:     refMismatch,
			why: "the user pinned a version and the platform reports another one. Not a " +
				"spelling difference — a different binding",
		},
		{
			name: "the platform dropped a pinned version", configured: pinned, returned: head,
			want: refMismatch,
			why: "deliberately NOT tolerated. Accepting it would mean treating " +
				"\"bound to whatever is current\" as equal to \"bound to this exact version\", " +
				"which is the one distinction the version segment exists to make",
		},
		{
			name: "a different region", configured: head,
			returned: "/v1/regions/eu-west-1/compute/cluster-types/gpu-training",
			want:     refMismatch,
			why:      "same slug, different region. Not the same ClusterType",
		},
		{
			name: "the configured value is empty", configured: "", returned: head, want: refMismatch,
			why: "callers handle the empty case before asking; this must not report a match",
		},
		{
			name: "the returned value is empty", configured: head, returned: "", want: refMismatch,
			why: "same, from the other side",
		},
		{
			name: "a version segment with a slash in it", configured: head,
			returned: head + "/versions/1/extra",
			want:     refMismatch,
			why:      "only one extra segment is a version pin; anything deeper is a different shape",
		},
		{
			name: "an empty version segment", configured: head, returned: head + "/versions/",
			want: refMismatch,
			why: "a `/versions/` with nothing after it pins nothing. Trimming the trailing " +
				"slash leaves \".../versions\", which is neither the head form nor a pinned " +
				"one, so it is reported as drift rather than quietly accepted",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := clusterTypeRefEquivalent(tc.configured, tc.returned)
			if got != tc.want {
				t.Fatalf("clusterTypeRefEquivalent(%q, %q) = %v, want %v\n  why this row exists: %s",
					tc.configured, tc.returned, got, tc.want, tc.why)
			}
		})
	}
}

// The relative path against `format: uri` is a *validation* hazard, not just a
// comparison one: anything in this provider's path that insisted on an absolute
// URI would reject the values the live API actually returns. Nothing does, and
// this asserts it from the outside — a relative reference is accepted on both
// sides of the comparison.
//
// The returned side carries a trailing slash. Comparing a reference with
// itself returns at the string-equality check before any parsing, so it would
// pass whatever the parser did with a relative reference.
func TestRelativeReferencesAreNeverRejected(t *testing.T) {
	t.Parallel()

	relative := []string{
		"/v1/regions/us-texas-1/compute/cluster-types/gpu-training",
		"/v1/regions/us-texas-1/compute/cluster-types/gpu-training/versions/3",
	}
	for _, ref := range relative {
		if got := clusterTypeRefEquivalent(ref, ref+"/"); got != refIdentical {
			t.Errorf("a relative reference %q did not compare equal to its trailing-slash "+
				"spelling (%v); something in the path is treating it as invalid", ref, got)
		}
		if path, _ := refPath(ref); path == "" {
			t.Errorf("refPath(%q) rejected a relative reference; the live API returns these", ref)
		}
	}
}

// Two absolute references differing only in host used to compare identical,
// because refPath reported "carried an origin" as a bool and discarded it.
func TestClusterTypeRefComparesTheOrigin(t *testing.T) {
	t.Parallel()

	const path = "/v1/regions/us-texas-1/compute/cluster-types/gpu-training"

	for _, tc := range []struct {
		name              string
		configured, given string
		want              clusterTypeRefMatch
	}{
		{"a different host", "https://api.k0rdent.ai" + path, "https://api.k0rdent.ai.evil.test" + path, refMismatch},
		{"an unrelated host", "https://api.k0rdent.ai" + path, "https://evil.example" + path, refMismatch},
		{"a downgrade to http", "https://api.k0rdent.ai" + path, "http://api.k0rdent.ai" + path, refMismatch},
		{"the same origin", "https://api.k0rdent.ai" + path, "https://api.k0rdent.ai" + path, refIdentical},
		{"relative against absolute", path, "https://api.k0rdent.ai" + path, refSameOrigin},
	} {
		if got := clusterTypeRefEquivalent(tc.configured, tc.given); got != tc.want {
			t.Errorf("%s: clusterTypeRefEquivalent(%q, %q) = %v, want %v",
				tc.name, tc.configured, tc.given, got, tc.want)
		}
	}
}

// The same holds when the platform answers with a version pinned under the
// configured head. That branch accepted on the path alone, so a pinned answer
// from another host, or over http, read as the same binding.
func TestAVersionResolvedReferenceComparesTheOrigin(t *testing.T) {
	t.Parallel()

	const (
		path   = "/v1/regions/us-texas-1/compute/cluster-types/gpu-training"
		pinned = path + "/versions/1f2e3d4e-a5b6-7c8d-9e0f-a1b2c3d4e5f6"
	)

	for _, tc := range []struct {
		name              string
		configured, given string
		want              clusterTypeRefMatch
	}{
		{"an unrelated host", "https://api.k0rdent.ai" + path, "https://evil.example" + pinned, refMismatch},
		{"a downgrade to http", "https://api.k0rdent.ai" + path, "http://api.k0rdent.ai" + pinned, refMismatch},
		{"the same origin", "https://api.k0rdent.ai" + path, "https://api.k0rdent.ai" + pinned, refVersionResolved},
		{"relative against absolute", path, "https://api.k0rdent.ai" + pinned, refVersionResolved},
		{"absolute against relative", "https://api.k0rdent.ai" + path, pinned, refVersionResolved},
	} {
		if got := clusterTypeRefEquivalent(tc.configured, tc.given); got != tc.want {
			t.Errorf("%s: clusterTypeRefEquivalent(%q, %q) = %v, want %v",
				tc.name, tc.configured, tc.given, got, tc.want)
		}
	}
}

// A hostname is case-insensitive and the default port is not part of the
// origin. Reading either as a different binding replaces a running cluster over
// a difference nobody made.
func TestACosmeticHostDifferenceIsTheSameBinding(t *testing.T) {
	t.Parallel()

	const configured = "https://api.k0rdent.ai/v1/regions/us-texas-1/compute/cluster-types/gpu-training"

	for _, returned := range []string{
		"https://API.k0rdent.ai/v1/regions/us-texas-1/compute/cluster-types/gpu-training",
		"https://api.k0rdent.ai:443/v1/regions/us-texas-1/compute/cluster-types/gpu-training",
		"HTTPS://api.k0rdent.ai/v1/regions/us-texas-1/compute/cluster-types/gpu-training",
	} {
		if got := clusterTypeRefEquivalent(configured, returned); got == refMismatch {
			t.Errorf("%q read as a different binding from the configured value", returned)
		}
	}

	// A genuinely different host still is one.
	other := "https://api.other.example/v1/regions/us-texas-1/compute/cluster-types/gpu-training"
	if got := clusterTypeRefEquivalent(configured, other); got != refMismatch {
		t.Errorf("a different host read as the same binding, got %v", got)
	}
}

// Another version of the same ClusterType is never a replacement: replacing
// would destroy a cluster to change its version. Whether the move is an upgrade
// or a refusal needs the version list, so ModifyPlan decides it. Another
// ClusterType still replaces.
func TestClusterTypeVersionGuardNeverReplacesAVersionChange(t *testing.T) {
	const base = "/v1/regions/r/compute/cluster-types/gpu"
	for _, tc := range []struct {
		name, stored, resolved, configured string
		replace, err                       bool
	}{
		{"upgraded outside Terraform", base + "/versions/v2", base + "/versions/v2", base + "/versions/v1", false, false},
		{"pinning the bound version", base, base + "/versions/v1", base + "/versions/v1", false, false},
		{"pinning another version", base, base + "/versions/v1", base + "/versions/v9", false, false},
		{"another ClusterType", base, base + "/versions/v1", "/v1/regions/r/compute/cluster-types/cpu", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &clusterResource{}
			raw, s := testValue(t, r, map[string]tftypes.Value{
				"id": tftypes.NewValue(tftypes.String, "c1"), "cluster_type": tftypes.NewValue(tftypes.String, tc.stored),
				"cluster_type_resolved": tftypes.NewValue(tftypes.String, tc.resolved),
			})
			req := planmodifier.StringRequest{
				Path:        path.Root("cluster_type"),
				ConfigValue: types.StringValue(tc.configured),
				PlanValue:   types.StringValue(tc.configured),
				StateValue:  types.StringValue(tc.stored),
				State:       tfsdk.State{Raw: raw, Schema: s.Schema},
				Plan:        tfsdk.Plan{Raw: raw, Schema: s.Schema},
			}
			resp := planmodifier.StringResponse{PlanValue: req.PlanValue}
			clusterTypeVersionGuard().PlanModifyString(context.Background(), req, &resp)
			if resp.RequiresReplace != tc.replace || resp.Diagnostics.HasError() != tc.err {
				t.Errorf("replace=%v error=%v %v, want replace=%v error=%v",
					resp.RequiresReplace, resp.Diagnostics.HasError(), resp.Diagnostics, tc.replace, tc.err)
			}
		})
	}
}

// An unversioned reference binds the newest `available` version, so a newer
// draft or deprecated row isn't "current" for an import's spelling.
func TestNewestAvailableSkipsADraftAndADeprecatedVersion(t *testing.T) {
	t.Parallel()
	id := func(n byte) *openapi_types.UUID {
		u := openapi_types.UUID{n}
		return &u
	}
	versions := []k0rdent.ClusterTypeVersion{
		{Version: ptr(int32(1)), VersionId: id(1), Status: k0rdent.ClusterTypeVersionStatusAvailable},
		{Version: ptr(int32(3)), VersionId: id(3), Status: k0rdent.ClusterTypeVersionStatusAvailable},
		{Version: ptr(int32(4)), VersionId: id(4), Status: k0rdent.ClusterTypeVersionStatusDraft},
		{Version: ptr(int32(5)), VersionId: id(5), Status: k0rdent.ClusterTypeVersionStatusDeprecated},
	}
	if got, want := newestAvailable(versions), id(3).String(); got != want {
		t.Errorf("newestAvailable = %s, want v3's %s", got, want)
	}
	if got := newestAvailable(versions[2:]); got != "" {
		t.Errorf("with nothing available, newestAvailable = %q, want none", got)
	}
}

// An instance group can't change version in place, so its modifier replaces
// on any other binding. An import that stored the unversioned spelling, while
// the configuration pins the version that spelling resolved to, is the same
// binding, and comparing with cluster_type_resolved is what says so.
func TestAnInstanceGroupPinnedToItsResolvedVersionIsNotReplaced(t *testing.T) {
	t.Parallel()
	const base = "/v1/regions/r/compute/cluster-types/gpu"
	for _, tc := range []struct {
		name, stored, resolved, configured string
		replace                            bool
	}{
		{"imported unversioned, pinned to what it resolved to", base, base + "/versions/v3", base + "/versions/v3", false},
		{"imported unversioned, pinned to another version", base, base + "/versions/v3", base + "/versions/v2", true},
		{"pinned, written unversioned", base + "/versions/v3", base + "/versions/v3", base, false},
		{"another ClusterType", base, base + "/versions/v3", "/v1/regions/r/compute/cluster-types/cpu", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &instanceGroupResource{}
			raw, s := testValue(t, r, map[string]tftypes.Value{
				"id": tftypes.NewValue(tftypes.String, "g1"), "cluster_type": tftypes.NewValue(tftypes.String, tc.stored),
				"cluster_type_resolved": tftypes.NewValue(tftypes.String, tc.resolved),
			})
			req := planmodifier.StringRequest{
				Path:        path.Root("cluster_type"),
				ConfigValue: types.StringValue(tc.configured),
				PlanValue:   types.StringValue(tc.configured),
				StateValue:  types.StringValue(tc.stored),
				State:       tfsdk.State{Raw: raw, Schema: s.Schema},
				Plan:        tfsdk.Plan{Raw: raw, Schema: s.Schema},
			}
			resp := planmodifier.StringResponse{PlanValue: req.PlanValue}
			clusterTypeRefReplacement().PlanModifyString(context.Background(), req, &resp)
			if resp.RequiresReplace != tc.replace {
				t.Errorf("replace=%v, want %v", resp.RequiresReplace, tc.replace)
			}
		})
	}
}
