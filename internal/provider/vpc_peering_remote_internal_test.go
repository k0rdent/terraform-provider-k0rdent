package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

const hostsVPCURI = "/v1/regions/r1/projects/p1/networking/vpcs/hosts"

func TestRemoteVPCURI(t *testing.T) {
	target := clusterTarget{region: "r1", project: "p1"}
	for in, want := range map[string]string{
		"hosts":     hostsVPCURI,
		hostsVPCURI: hostsVPCURI,
		"/v1/regions/r1/projects/other/networking/vpcs/far": "/v1/regions/r1/projects/other/networking/vpcs/far",
	} {
		if got := remoteVPCURI(target, in); got != want {
			t.Errorf("remoteVPCURI(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSameRemoteVPC(t *testing.T) {
	target := clusterTarget{region: "r1", project: "p1"}
	tests := []struct {
		a, b string
		want bool
	}{
		{"hosts", "hosts", true},
		{"hosts", hostsVPCURI, true},
		{hostsVPCURI, "https://api.example.com" + hostsVPCURI, true},
		{"hosts", "cluster", false},
		{"hosts", "/v1/regions/r1/projects/other/networking/vpcs/hosts", false},
		{"hosts", "/v1/regions/r2/projects/p1/networking/vpcs/hosts", false},
		{"hosts", "not a vpc/at all", false},
	}
	for _, tt := range tests {
		if got := sameRemoteVPC(target, tt.a, tt.b); got != tt.want {
			t.Errorf("sameRemoteVPC(%q, %q) = %v, want %v", tt.a, tt.b, got, tt.want)
		}
	}
}

// Moving between an id and the self_link of the same VPC replaces nothing.
// Another VPC, or one not known at plan, replaces.
func TestRemoteVPCReplacesOnlyForAnotherVPC(t *testing.T) {
	r := &vpcPeeringResource{}
	tests := []struct {
		name        string
		prior, plan types.String
		want        bool
	}{
		{"id to its self_link", types.StringValue("hosts"), types.StringValue(hostsVPCURI), false},
		{"self_link to its id", types.StringValue(hostsVPCURI), types.StringValue("hosts"), false},
		{"another id", types.StringValue("hosts"), types.StringValue("cluster"), true},
		{"the same id in another project", types.StringValue("hosts"), types.StringValue("/v1/regions/r1/projects/other/networking/vpcs/hosts"), true},
		{"not known at plan", types.StringValue("hosts"), types.StringUnknown(), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw, s := testValue(t, r, map[string]tftypes.Value{
				"id": str("x"), "region": str("r1"), "project": str("p1"), "vpc": str("cluster"),
				"remote_vpc": str(tt.prior.ValueString()),
			})
			var resp stringplanmodifier.RequiresReplaceIfFuncResponse
			remoteVPCRequiresReplace(context.Background(), planmodifier.StringRequest{
				State: tfsdk.State{Raw: raw, Schema: s.Schema}, StateValue: tt.prior, PlanValue: tt.plan,
			}, &resp)
			if resp.Diagnostics.HasError() {
				t.Fatal(resp.Diagnostics)
			}
			if resp.RequiresReplace != tt.want {
				t.Errorf("RequiresReplace = %v, want %v", resp.RequiresReplace, tt.want)
			}
		})
	}
}

func TestRemoteVPCChecksAnIdAsOne(t *testing.T) {
	for value, ok := range map[string]bool{
		"hosts":       true,
		hostsVPCURI:   true,
		"Hosts":       false,
		"hosts?x=1":   false,
		"hosts-":      false,
		"vpc-0h2wq6d": true,
	} {
		var resp validator.StringResponse
		remoteVPCReference{}.ValidateString(context.Background(), validator.StringRequest{
			Path: path.Root("remote_vpc"), ConfigValue: types.StringValue(value),
		}, &resp)
		if resp.Diagnostics.HasError() == ok {
			t.Errorf("%q: errors %v, want accepted %v", value, resp.Diagnostics, ok)
		}
	}
}

// A create with an id sends the URI the API takes, and state keeps the id,
// so the next plan is empty.
func TestPeeringCreateSendsAnIdAsItsURI(t *testing.T) {
	var mu sync.Mutex
	sent := ""
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		mu.Lock()
		defer mu.Unlock()
		peering := map[string]any{
			"id": "p1", "state": "active", "remoteVpc": sent,
			"selfLink": "/v1/regions/r1/projects/p1/networking/vpcs/cluster/peerings/p1",
		}
		switch {
		case r.Method == http.MethodPost:
			var body struct {
				RemoteVpc string            `json:"remoteVpc"`
				Tags      map[string]string `json:"tags"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("read create: %v", err)
			}
			sent = body.RemoteVpc
			peering["remoteVpc"], peering["tags"] = sent, body.Tags
			w.WriteHeader(http.StatusCreated)
		case strings.HasSuffix(r.URL.Path, "/peerings"):
			if err := json.NewEncoder(w).Encode(map[string]any{"peerings": []any{}}); err != nil {
				t.Errorf("write: %v", err)
			}
			return
		}
		if err := json.NewEncoder(w).Encode(peering); err != nil {
			t.Errorf("write: %v", err)
		}
	}))
	t.Cleanup(server.Close)

	r := &vpcPeeringResource{meta: testClusterMeta(t, server.URL, time.Minute)}
	raw, s := testValue(t, r, map[string]tftypes.Value{
		"id": str("p1"), "vpc": str("cluster"), "region": str("r1"), "project": str("p1"),
		"remote_vpc": str("hosts"),
	})
	resp := resource.CreateResponse{State: tfsdk.State{Raw: tftypes.NewValue(raw.Type(), nil), Schema: s.Schema}}
	r.Create(context.Background(), resource.CreateRequest{Plan: tfsdk.Plan{Raw: raw, Schema: s.Schema}}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatal(resp.Diagnostics.Errors())
	}
	if sent != hostsVPCURI {
		t.Errorf("the create sent remoteVpc %q, want %q", sent, hostsVPCURI)
	}
	var remote types.String
	resp.State.GetAttribute(context.Background(), path.Root("remote_vpc"), &remote)
	if remote.ValueString() != "hosts" {
		t.Errorf("remote_vpc in state = %s, want the id the configuration wrote", remote)
	}
}
