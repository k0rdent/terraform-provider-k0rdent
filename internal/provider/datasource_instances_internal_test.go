package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// oneInstance is a host with one interface on each fabric, as the contract
// reports them: a MAC address on Ethernet and a GUID on InfiniBand.
var oneInstance = map[string]any{
	"id": "inst-0000000000000000000000000000000b", "uid": "00000000-0000-4000-8000-00000000000b",
	"instanceGroupId": "trainers", "nodePool": "gpu", "serverSlug": "srv-b", "hostname": "srv-b",
	"state": "running", "health": map[string]any{"state": "healthy"},
	"createdAt": "2026-09-21T00:00:00Z",
	"networkConfiguration": []any{
		map[string]any{"type": "ethernet", "macAddress": "0c:42:a1:5f:9b:30", "ipAddresses": []any{"10.0.0.7"}},
		map[string]any{"type": "infiniband", "guid": "0x0002c90300a1b2c3", "ipAddresses": []any{"10.1.0.7"}},
	},
}

// The two interface identifiers land where a file store grant reads them, and
// the filters reach the request.
func TestInstancesCarryEachInterfaceIdentifier(t *testing.T) {
	var query string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.RawQuery
		writeTestJSON(t, w, http.StatusOK, map[string]any{
			"instances": []any{oneInstance, map[string]any{"id": "inst-0000000000000000000000000000000a", "state": "not_provisioned"}},
		})
	}))
	t.Cleanup(server.Close)

	resp := readTestDataSource(t, &instancesDataSource{meta: testClusterMeta(t, server.URL, time.Minute)},
		map[string]tftypes.Value{"instance_group": str("trainers"), "node_pool": str("gpu")})
	if resp.Diagnostics.HasError() {
		t.Fatalf("read: %v", resp.Diagnostics.Errors())
	}
	for _, want := range []string{"instanceGroupId=trainers", "nodePool=gpu", "pageSize=200"} {
		if !strings.Contains(query, want) {
			t.Errorf("the request %q does not carry %s", query, want)
		}
	}

	var got instancesModel
	resp.Diagnostics.Append(resp.State.Get(context.Background(), &got)...)
	if len(got.Instances) != 2 || got.Instances[0].ID.ValueString() != "inst-0000000000000000000000000000000a" {
		t.Fatalf("got %d instances, want two sorted by id", len(got.Instances))
	}
	b := got.Instances[1]
	if b.Health.ValueString() != "healthy" || b.InstanceGroup.ValueString() != "trainers" || len(b.NetworkInterfaces) != 2 {
		t.Fatalf("instance b read as %+v", b)
	}
	eth, ib := b.NetworkInterfaces[0], b.NetworkInterfaces[1]
	if eth.MacAddress.ValueString() != "0c:42:a1:5f:9b:30" || !eth.GUID.IsNull() || eth.IPAddresses[0].ValueString() != "10.0.0.7" {
		t.Errorf("the Ethernet interface read as %+v", eth)
	}
	if ib.GUID.ValueString() != "0x0002c90300a1b2c3" || !ib.MacAddress.IsNull() {
		t.Errorf("the InfiniBand interface read as %+v", ib)
	}
	// A host with nothing provisioned has no interfaces, which is an empty
	// list rather than a null a `for` expression would fail on.
	if a := got.Instances[0]; a.NetworkInterfaces == nil || len(a.NetworkInterfaces) != 0 {
		t.Errorf("instance a interfaces read as %v, want an empty list", a.NetworkInterfaces)
	}
}

// A singular read that names something absent is an error, not an empty
// state: the configuration that named it is wrong.
func TestTheSingularReadsFailOnA404(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeTestJSON(t, w, http.StatusNotFound, map[string]any{
			"error": map[string]any{"code": 404, "message": "not found"},
		})
	}))
	t.Cleanup(server.Close)
	meta := testClusterMeta(t, server.URL, time.Minute)

	for name, ds := range map[string]datasource.DataSource{
		"organization": &organizationDataSource{meta: meta},
		"instance":     &instanceDataSource{meta: meta},
	} {
		resp := readTestDataSource(t, ds, map[string]tftypes.Value{"id": str("missing")})
		if !resp.Diagnostics.HasError() {
			t.Errorf("%s: a 404 read without an error", name)
		}
	}
}

// The singular instance reads the same fields a row of the list does.
func TestTheInstanceDataSourceReadsOneHost(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/compute/instances/inst-0000000000000000000000000000000b") {
			t.Errorf("read %s", r.URL.Path)
		}
		writeTestJSON(t, w, http.StatusOK, oneInstance)
	}))
	t.Cleanup(server.Close)

	resp := readTestDataSource(t, &instanceDataSource{meta: testClusterMeta(t, server.URL, time.Minute)},
		map[string]tftypes.Value{"id": str("inst-0000000000000000000000000000000b")})
	if resp.Diagnostics.HasError() {
		t.Fatalf("read: %v", resp.Diagnostics.Errors())
	}
	var got instanceDataSourceModel
	resp.Diagnostics.Append(resp.State.Get(context.Background(), &got)...)
	if got.ID.ValueString() != "inst-0000000000000000000000000000000b" || got.Region.IsNull() || len(got.NetworkInterfaces) != 2 ||
		got.NetworkInterfaces[0].MacAddress.ValueString() != "0c:42:a1:5f:9b:30" {
		t.Errorf("read as %+v", got)
	}
}
