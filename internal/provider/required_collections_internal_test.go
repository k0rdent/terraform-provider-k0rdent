package provider

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/k0rdent"
)

// v1.3.0-dev.4 made these collections required on every read, so the client
// decodes them as values where it had pointers. A pointer's nil was how each
// reader below learned there was nothing there, and taking the address of the
// value is never nil. Each test is the dev.4 wire shape for "none" ({} or [])
// against a configuration that set nothing, and each wants the null the
// reader gave an absent collection before, so no configuration gets a diff
// from the contract change.

func TestClusterDataReadsEmptyTagsAsNone(t *testing.T) {
	data := filledCluster(t, `{"id":"c","uid":"00000000-0000-4000-8000-000000000001","selfLink":"/c",
		"nodePools":[],"tags":{},"systemTags":{}}`)
	if !data.Tags.IsNull() {
		t.Errorf("tags = %s, want null", data.Tags)
	}
}

func TestSubnetDataReadsEmptyTagsAsNone(t *testing.T) {
	var data subnetDataSourceModel
	if diags := data.fill(context.Background(), decodeInto[k0rdent.Subnet](t, `{"id":"s",
		"uid":"00000000-0000-4000-8000-000000000002","selfLink":"/s","network":"/n","addressPool":"/p",
		"prefixLength":24,"tags":{},"systemTags":{},"excludeRanges":[]}`)); diags.HasError() {
		t.Fatal(diags)
	}
	if !data.Tags.IsNull() {
		t.Errorf("tags = %s, want null", data.Tags)
	}
	// This attribute was always a list, never null, and stays one.
	if data.ExcludeRanges == nil || len(data.ExcludeRanges) != 0 {
		t.Errorf("exclude_ranges = %v, want an empty list", data.ExcludeRanges)
	}
}

func TestAddressPoolDataReadsEmptyExcludeRangesAsNone(t *testing.T) {
	ctx := context.Background()
	got, diags := addressPoolEntryFrom(ctx, decodeInto[k0rdent.AddressPool](t,
		`{"id":"p","prefixes":["10.0.0.0/16"],"excludeRanges":[]}`))
	if diags.HasError() {
		t.Fatal(diags)
	}
	if !got.ExcludeRanges.IsNull() {
		t.Errorf("exclude_ranges = %s, want null", got.ExcludeRanges)
	}

	got, diags = addressPoolEntryFrom(ctx, decodeInto[k0rdent.AddressPool](t,
		`{"id":"p","prefixes":["10.0.0.0/16"],"excludeRanges":["10.0.0.9","10.0.0.1"]}`))
	if diags.HasError() {
		t.Fatal(diags)
	}
	if got.ExcludeRanges.String() != `["10.0.0.1","10.0.0.9"]` {
		t.Errorf("exclude_ranges = %s, want both, sorted", got.ExcludeRanges)
	}
}

func TestSubnetReadsEmptyExcludeRangesAsNoneWhenNoneWereConfigured(t *testing.T) {
	ctx := context.Background()
	prior := subnetModel{ExcludeRanges: types.ListNull(types.StringType)}
	got, diags := subnetStateFrom(ctx, decodeInto[k0rdent.Subnet](t, `{"id":"s","network":"/n",
		"prefixLength":28,"tags":{},"systemTags":{},"excludeRanges":[]}`), "n", prior)
	if diags.HasError() {
		t.Fatal(diags)
	}
	if !got.ExcludeRanges.IsNull() {
		t.Errorf("exclude_ranges = %s, want null", got.ExcludeRanges)
	}

	// A configuration that wrote `exclude_ranges = []` gets its empty list back.
	prior.ExcludeRanges = types.ListValueMust(types.StringType, nil)
	got, diags = subnetStateFrom(ctx, decodeInto[k0rdent.Subnet](t, `{"id":"s","network":"/n",
		"prefixLength":28,"tags":{},"excludeRanges":[]}`), "n", prior)
	if diags.HasError() {
		t.Fatal(diags)
	}
	if got.ExcludeRanges.IsNull() || len(got.ExcludeRanges.Elements()) != 0 {
		t.Errorf("exclude_ranges = %s, want []", got.ExcludeRanges)
	}
}

func TestClusterAndSecurityGroupReadEmptyTagsAsNone(t *testing.T) {
	ctx := context.Background()
	cluster, diags := clusterStateFrom(ctx, decodeInto[k0rdent.Cluster](t, `{"id":"c","nodePools":[],
		"tags":{},"systemTags":{}}`), clusterModel{Tags: types.MapNull(types.StringType),
		NodePool: types.MapNull(clusterNodePoolObjectType())}, defaultTags{})
	if diags.HasError() {
		t.Fatal(diags)
	}
	if !cluster.Tags.IsNull() {
		t.Errorf("cluster tags = %s, want null", cluster.Tags)
	}

	group, diags := securityGroupStateFrom(ctx, decodeInto[k0rdent.SecurityGroup](t,
		`{"id":"g","ruleCount":0,"tags":{}}`), securityGroupModel{Tags: types.MapNull(types.StringType)}, defaultTags{})
	if diags.HasError() {
		t.Fatal(diags)
	}
	if !group.Tags.IsNull() {
		t.Errorf("security group tags = %s, want null", group.Tags)
	}
}

// The create token is read out of a create's answer. An answer with no tags
// at all names no token, whatever shape "none" arrives in.
func TestNoTagsNamesNoCreateToken(t *testing.T) {
	if got := createTokenOf(presentMap(map[string]string{})); got != "" {
		t.Errorf("token = %q from {}", got)
	}
	if got := createTokenOfFreeform(presentMap(map[string]any{})); got != "" {
		t.Errorf("token = %q from a freeform {}", got)
	}
	if got := createTokenOf(presentMap(map[string]string{createTokenTag(): "t"})); got != "t" {
		t.Errorf("token = %q, want t", got)
	}
}
