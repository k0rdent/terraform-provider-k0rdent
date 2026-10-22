package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

func testNetworkState(t *testing.T, id string) tfsdk.State {
	t.Helper()
	var schemaResp resource.SchemaResponse
	(&networkResource{}).Schema(context.Background(), resource.SchemaRequest{}, &schemaResp)
	object, ok := schemaResp.Schema.Type().TerraformType(context.Background()).(tftypes.Object)
	if !ok {
		t.Fatal("network schema is not an object")
	}
	values := make(map[string]tftypes.Value, len(object.AttributeTypes))
	for name, attrType := range object.AttributeTypes {
		values[name] = tftypes.NewValue(attrType, nil)
	}
	values["id"] = tftypes.NewValue(tftypes.String, id)
	return tfsdk.State{Raw: tftypes.NewValue(object, values), Schema: schemaResp.Schema}
}

// deletingNetwork answers DELETE with 204, then reads `before` for the first
// `reads` GETs and 404 after that. A negative `reads` never gets to 404.
func deletingNetwork(t *testing.T, before string, reads int32) *httptest.Server {
	var n atomic.Int32
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if reads >= 0 && n.Add(1) > reads {
			writeTestError(t, w, http.StatusNotFound, "not found")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{
			"id": "net", "type": "evpn", "state": before,
			"evpn": map[string]any{"type": "l3vpn"},
		}); err != nil {
			t.Errorf("write network: %v", err)
		}
	}))
}

func deleteNetwork(t *testing.T, server *httptest.Server, window time.Duration) resource.DeleteResponse {
	meta := testClusterMeta(t, server.URL, time.Minute)
	meta.Wait.ProvisionalFailureWindow = window
	state := testNetworkState(t, "net")
	resp := resource.DeleteResponse{State: state}
	(&networkResource{meta: meta}).Delete(context.Background(), resource.DeleteRequest{State: state}, &resp)
	return resp
}

// Destroying a failed network: the read straight after the 204 can still say
// `failed`, and that is the platform catching up, not refusing.
func TestNetworkDeleteToleratesThePriorStateBriefly(t *testing.T) {
	server := deletingNetwork(t, "failed", 3)
	defer server.Close()
	if resp := deleteNetwork(t, server, time.Minute); resp.Diagnostics.HasError() {
		t.Fatalf("delete: %v", resp.Diagnostics.Errors())
	}
}

func TestNetworkDeleteReportsARefusalAfterTheWindow(t *testing.T) {
	server := deletingNetwork(t, "active", -1)
	defer server.Close()
	resp := deleteNetwork(t, server, 10*time.Millisecond)
	if !resp.Diagnostics.HasError() {
		t.Fatal("a network still active after the window must fail the delete")
	}
	if got := resp.Diagnostics.Errors()[0].Detail(); !strings.Contains(got, `"active"`) {
		t.Errorf("error does not name the state: %s", got)
	}
}
