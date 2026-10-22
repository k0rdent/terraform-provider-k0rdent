package provider

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// `allocate_gateway` is never echoed, so an import has to work it out. A
// populated `gateway` proves it was true. An empty one proves it was false
// only on a routable pool, since a pool that isn't routable never fills one:
// there it fits either value and stays null. Without reading the pool, a
// subnet made with the default imported as null and the plan after adopting
// it proposed setting it to false, which is what `terraform query
// -generate-config-out` hit for the default subnet.
func TestASubnetImportsItsGatewayFlagFromThePool(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		gateway  string
		routable bool
		poolGone bool
		// poolRegion is the region the pool's URI names, the subnet's own
		// when empty.
		poolRegion string
		want       any // nil for null
	}{
		{name: "a gateway proves true", gateway: "10.0.0.254", routable: false, want: true},
		{name: "no gateway on a routable pool proves false", routable: true, want: false},
		{name: "no gateway on a pool that isn't routable proves nothing", routable: false, want: nil},
		{name: "a pool that can't be read proves nothing", routable: true, poolGone: true, want: nil},
		{name: "a pool in another region is read there", routable: true, poolRegion: "us-oregon-1", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			poolRegion := tc.poolRegion
			if poolRegion == "" {
				poolRegion = "us-texas-1"
			}
			poolPath := "/v1/regions/" + poolRegion + "/networking/address-pools/tenant"
			subnet := map[string]any{
				"id": "pods", "network": "fabric", "state": "active", "prefixLength": 24, "tags": map[string]any{},
				"excludeRanges": []any{}, "cidr": "10.0.0.0/24",
				"addressPool": poolPath,
			}
			if tc.gateway != "" {
				subnet["gateway"] = tc.gateway
			}
			var poolReads atomic.Int32
			api := listAPI(t, map[string]http.HandlerFunc{
				"GET " + projectPath + "/networking/networks/fabric/subnets/pods": func(w http.ResponseWriter, _ *http.Request) {
					writeTestJSON(t, w, http.StatusOK, subnet)
				},
				"GET " + poolPath: func(w http.ResponseWriter, _ *http.Request) {
					poolReads.Add(1)
					if tc.poolGone {
						writeTestJSON(t, w, http.StatusNotFound, map[string]any{"error": map[string]any{"code": 404, "message": "not found"}})
						return
					}
					writeTestJSON(t, w, http.StatusOK, map[string]any{"id": "tenant", "routable": tc.routable, "state": "active"})
				},
			})
			server, schemas := listServer(t, api.URL)
			typeName := ResourceName(subnetResourceSuffix)
			identity, err := tfprotov6.NewDynamicValue(identityType(t, server, typeName),
				identityValue(t, server, typeName, map[string]string{"id": "pods", "network": "fabric"}))
			if err != nil {
				t.Fatal(err)
			}
			imported, err := server.ImportResourceState(ctx, &tfprotov6.ImportResourceStateRequest{
				TypeName: typeName, Identity: &tfprotov6.ResourceIdentityData{IdentityData: &identity},
			})
			if err != nil || len(diagnosticErrors(imported.Diagnostics)) > 0 {
				t.Fatalf("import: %v %v", err, diagnosticErrors(imported.Diagnostics))
			}
			read, err := server.ReadResource(ctx, &tfprotov6.ReadResourceRequest{
				TypeName: typeName, CurrentState: imported.ImportedResources[0].State,
				CurrentIdentity: imported.ImportedResources[0].Identity, Private: imported.ImportedResources[0].Private,
			})
			if err != nil || len(diagnosticErrors(read.Diagnostics)) > 0 {
				t.Fatalf("read: %v %v", err, diagnosticErrors(read.Diagnostics))
			}
			// A second refresh in the same run reads the pool from the memo.
			again, err := server.ReadResource(ctx, &tfprotov6.ReadResourceRequest{
				TypeName: typeName, CurrentState: read.NewState, CurrentIdentity: read.NewIdentity, Private: read.Private,
			})
			if err != nil || len(diagnosticErrors(again.Diagnostics)) > 0 {
				t.Fatalf("second read: %v %v", err, diagnosticErrors(again.Diagnostics))
			}
			if n := poolReads.Load(); tc.gateway == "" && !tc.poolGone && n != 1 {
				t.Errorf("read the pool %d times over two refreshes, want once", n)
			}
			state, err := read.NewState.Unmarshal(schemas.ResourceSchemas[typeName].ValueType())
			if err != nil {
				t.Fatal(err)
			}
			var attrs map[string]tftypes.Value
			if err := state.As(&attrs); err != nil {
				t.Fatal(err)
			}
			got := attrs["allocate_gateway"]
			if tc.want == nil {
				if !got.IsNull() {
					t.Errorf("allocate_gateway = %v, want null", got)
				}
				return
			}
			var b bool
			if err := got.As(&b); err != nil || got.IsNull() || b != tc.want {
				t.Errorf("allocate_gateway = %v, want %v", got, tc.want)
			}
		})
	}
}
