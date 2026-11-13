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

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// tokenAnswerKind is one kind with tags, its plan and the body its platform
// answers for one of its resources.
type tokenAnswerKind struct {
	resource func(meta *Meta) resource.Resource
	plan     func(t *testing.T, r resource.Resource) map[string]tftypes.Value
	// body is the resource under id, failed so the create's wait ends at
	// once, with tags.
	body func(id string, tags map[string]any) map[string]any
	noun string
}

func tokenAnswerKinds() map[string]tokenAnswerKind {
	scope := func(set map[string]tftypes.Value) map[string]tftypes.Value {
		set["region"], set["project"] = str("test"), str("test")
		return set
	}
	return map[string]tokenAnswerKind{
		"network": {
			noun:     "network",
			resource: func(meta *Meta) resource.Resource { return &networkResource{meta: meta} },
			plan: func(t *testing.T, r resource.Resource) map[string]tftypes.Value {
				return scope(map[string]tftypes.Value{
					"evpn": nestedValue(t, r, "evpn", map[string]tftypes.Value{"service_type": str("l3vpn")}),
				})
			},
			body: func(id string, tags map[string]any) map[string]any {
				return map[string]any{"id": id, "type": "evpn", "state": "failed",
					"evpn": map[string]any{"type": "l3vpn"}, "tags": tags}
			},
		},
		"subnet": {
			noun:     "subnet",
			resource: func(meta *Meta) resource.Resource { return &subnetResource{meta: meta} },
			plan: func(*testing.T, resource.Resource) map[string]tftypes.Value {
				return scope(map[string]tftypes.Value{
					"network":       str("n1"),
					"address_pool":  str("/v1/regions/test/networking/address-pools/tenant"),
					"prefix_length": tftypes.NewValue(tftypes.Number, 24),
				})
			},
			body: func(id string, tags map[string]any) map[string]any {
				return map[string]any{"id": id, "state": "failed", "prefixLength": 24,
					"addressPool": "/v1/regions/test/networking/address-pools/tenant", "tags": tags}
			},
		},
		"security group": {
			noun:     "security group",
			resource: func(meta *Meta) resource.Resource { return &securityGroupResource{meta: meta} },
			plan:     func(*testing.T, resource.Resource) map[string]tftypes.Value { return scope(map[string]tftypes.Value{}) },
			body: func(id string, tags map[string]any) map[string]any {
				return map[string]any{"id": id, "state": "failed",
					"rules": map[string]any{"ingress": []any{}, "egress": []any{}}, "tags": tags}
			},
		},
		"VPC peering": {
			noun:     "VPC peering",
			resource: func(meta *Meta) resource.Resource { return &vpcPeeringResource{meta: meta} },
			plan: func(*testing.T, resource.Resource) map[string]tftypes.Value {
				return scope(map[string]tftypes.Value{
					"vpc":        str("near"),
					"remote_vpc": str("/v1/regions/test/projects/other/networking/vpcs/far"),
				})
			},
			body: func(id string, tags map[string]any) map[string]any {
				return map[string]any{"id": id, "state": "failed", "tags": tags,
					"remoteVpc": "/v1/regions/test/projects/other/networking/vpcs/far"}
			},
		},
	}
}

// tokenAnswerAPI answers every create 201 naming "chosen", with the tags
// tagsFor gives it from the token the create sent, and every read of
// "chosen" with the same resource.
func tokenAnswerAPI(t *testing.T, k tokenAnswerKind, tagsFor func(sent string) map[string]any) (*httptest.Server, *[]string) {
	var (
		mu      sync.Mutex
		tags    map[string]any
		deleted []string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodPost:
			var body struct {
				Tags map[string]any `json:"tags"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("read create: %v", err)
			}
			sent, ok := body.Tags[createTokenTag()].(string)
			if !ok || sent == "" {
				t.Errorf("the create sent no token: %v", body.Tags)
			}
			tags = tagsFor(sent)
			w.WriteHeader(http.StatusCreated)
		case http.MethodDelete:
			deleted = append(deleted, r.URL.Path)
			w.WriteHeader(http.StatusAccepted)
			return
		}
		if err := json.NewEncoder(w).Encode(k.body("chosen", tags)); err != nil {
			t.Errorf("write: %v", err)
		}
	}))
	t.Cleanup(server.Close)
	return server, &deleted
}

// Each kind with tags reads the create token from its 201's body. An answer
// naming another id is recorded under it when that token is this create's,
// and refused with nothing in state when it's another apply's or missing.
func TestEachTaggedKindReadsTheTokenFromItsAnswer(t *testing.T) {
	for name, k := range tokenAnswerKinds() {
		for answer, tagsFor := range map[string]func(sent string) map[string]any{
			"this create's token":   func(sent string) map[string]any { return map[string]any{createTokenTag(): sent} },
			"another apply's token": func(string) map[string]any { return map[string]any{createTokenTag(): "0000feedface"} },
			"no token":              func(string) map[string]any { return map[string]any{"team": "a"} },
		} {
			t.Run(name+"/"+answer, func(t *testing.T) {
				server, deleted := tokenAnswerAPI(t, k, tagsFor)
				meta := testClusterMeta(t, server.URL, time.Minute)
				meta.Wait.Interval = time.Millisecond
				r := k.resource(meta)
				raw, s := testValue(t, r, k.plan(t, r))
				resp := resource.CreateResponse{State: tfsdk.State{Raw: tftypes.NewValue(raw.Type(), nil), Schema: s.Schema}}
				r.Create(context.Background(), resource.CreateRequest{Plan: tfsdk.Plan{Raw: raw, Schema: s.Schema}}, &resp)
				refused := false
				for _, d := range resp.Diagnostics.Errors() {
					refused = refused || d.Summary() == "The platform named a "+k.noun+" this apply can't show it created"
				}
				ours := strings.HasPrefix(answer, "this")
				if got := stateID(t, resp.State); ours && got != "chosen" {
					t.Errorf("id in state = %q, want %q: %v", got, "chosen", resp.Diagnostics)
				} else if !ours && !resp.State.Raw.IsNull() {
					t.Errorf("state was written for another apply's %s: %v", k.noun, resp.State.Raw)
				}
				if refused == ours {
					t.Errorf("refused %t, want %t: %v", refused, !ours, resp.Diagnostics)
				}
				if len(*deleted) > 0 {
					t.Errorf("the create deleted %v", *deleted)
				}
			})
		}
	}
}
