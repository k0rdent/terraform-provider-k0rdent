package provider

import (
	"context"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// nestedValue builds the value of r's nested attribute name, null except for set.
func nestedValue(t *testing.T, r resource.Resource, name string, set map[string]tftypes.Value) tftypes.Value {
	t.Helper()
	var s resource.SchemaResponse
	r.Schema(context.Background(), resource.SchemaRequest{}, &s)
	object, ok := s.Schema.Attributes[name].GetType().TerraformType(context.Background()).(tftypes.Object)
	if !ok {
		t.Fatalf("%s is not an object", name)
	}
	values := map[string]tftypes.Value{}
	for attr, attrType := range object.AttributeTypes {
		values[attr] = tftypes.NewValue(attrType, nil)
	}
	for attr, v := range set {
		values[attr] = v
	}
	return tftypes.NewValue(object, values)
}

func TestNetworkCreateKeepsScopeWhenTheWaitFails(t *testing.T) {
	server := failingCreate(t, map[string]any{
		"id": "n1", "type": "evpn", "state": "failed", "evpn": map[string]any{"type": "l3vpn"},
	})
	defer server.Close()
	r := &networkResource{meta: testClusterMeta(t, server.URL, time.Minute)}
	assertCreateKeepsScope(t, r, map[string]tftypes.Value{
		"id": str("n1"), "region": str("test"), "project": str("elsewhere"),
		"evpn": nestedValue(t, r, "evpn", map[string]tftypes.Value{"service_type": str("l3vpn")}),
	}, func(req resource.CreateRequest, resp *resource.CreateResponse) {
		r.Create(context.Background(), req, resp)
	})
}

func TestSubnetCreateKeepsScopeWhenTheWaitFails(t *testing.T) {
	server := failingCreate(t, map[string]any{
		"id": "s1", "state": "failed", "prefixLength": 24,
		"addressPool": "/v1/regions/test/networking/address-pools/tenant",
	})
	defer server.Close()
	r := &subnetResource{meta: testClusterMeta(t, server.URL, time.Minute)}
	assertCreateKeepsScope(t, r, map[string]tftypes.Value{
		"id": str("s1"), "network": str("n1"), "region": str("test"), "project": str("elsewhere"),
		"address_pool":  str("/v1/regions/test/networking/address-pools/tenant"),
		"prefix_length": tftypes.NewValue(tftypes.Number, 24),
	}, func(req resource.CreateRequest, resp *resource.CreateResponse) {
		r.Create(context.Background(), req, resp)
	})
}

func TestSecurityGroupCreateKeepsScopeWhenTheWaitFails(t *testing.T) {
	server := failingCreate(t, map[string]any{
		"id": "g1", "state": "failed", "rules": map[string]any{"ingress": []any{}, "egress": []any{}},
	})
	defer server.Close()
	r := &securityGroupResource{meta: testClusterMeta(t, server.URL, time.Minute)}
	assertCreateKeepsScope(t, r, map[string]tftypes.Value{
		"id": str("g1"), "region": str("test"), "project": str("elsewhere"),
	}, func(req resource.CreateRequest, resp *resource.CreateResponse) {
		r.Create(context.Background(), req, resp)
	})
}
