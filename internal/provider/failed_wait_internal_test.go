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
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// The requirement is that a wait which expires or is interrupted leaves the resource
// recorded, so the next run resumes it, and that nothing is deleted because a
// wait failed. Both are asked of every kind, so both are checked per kind
// here rather than once through createFinish, which a kind could stop using.

// pendingKind is one resource kind that waits after it writes: how to build it,
// a plan for its create, the body every read answers while it is still
// settling, and the attribute that identifies it in state.
type pendingKind struct {
	build  func(*Meta) resource.Resource
	plan   func(t *testing.T, r resource.Resource) map[string]tftypes.Value
	body   map[string]any
	idAttr string
}

func pendingKinds() map[string]pendingKind {
	scoped := func(extra map[string]tftypes.Value) func(*testing.T, resource.Resource) map[string]tftypes.Value {
		return func(*testing.T, resource.Resource) map[string]tftypes.Value {
			v := map[string]tftypes.Value{"region": str("test"), "project": str("elsewhere")}
			for k, x := range extra {
				v[k] = x
			}
			return v
		}
	}
	pool := tftypes.Object{AttributeTypes: map[string]tftypes.Type{"size": tftypes.Number}}
	return map[string]pendingKind{
		"cluster": {
			build: func(m *Meta) resource.Resource { return &clusterResource{meta: m} },
			plan:  scoped(map[string]tftypes.Value{"id": str("c1"), "cluster_type": str("/v1/regions/test/compute/cluster-types/test")}),
			body: map[string]any{
				"id": "c1", "clusterType": "/v1/regions/test/compute/cluster-types/test",
				"nodePools": []any{}, "nodeCount": 0, "state": "creating", "securityGroups": []any{},
				"selfLink":  "/v1/regions/test/projects/elsewhere/compute/clusters/c1",
				"uid":       "00000000-0000-4000-8000-00000000000a",
				"createdAt": "2026-09-22T00:00:00Z", "updatedAt": "2026-09-22T00:00:00Z",
				"health": map[string]string{"state": "unknown"},
			},
			idAttr: "id",
		},
		"instance group": {
			build: func(m *Meta) resource.Resource { return &instanceGroupResource{meta: m} },
			plan: scoped(map[string]tftypes.Value{
				"id":           str("g1"),
				"cluster_type": str("/v1/regions/test/compute/cluster-types/test"),
				"node_pool": tftypes.NewValue(tftypes.Map{ElementType: pool}, map[string]tftypes.Value{
					"workers": tftypes.NewValue(pool, map[string]tftypes.Value{"size": tftypes.NewValue(tftypes.Number, 1)}),
				}),
			}),
			body: map[string]any{
				"id": "g1", "clusterType": "/v1/regions/test/compute/cluster-types/test",
				"nodePools": []any{}, "state": "creating",
				"selfLink": "/v1/regions/test/projects/elsewhere/compute/instance-groups/g1",
			},
			idAttr: "id",
		},
		"file store": {
			build:  func(m *Meta) resource.Resource { return &fileStoreResource{meta: m} },
			plan:   func(*testing.T, resource.Resource) map[string]tftypes.Value { return fileStorePlan(nil) },
			body:   fileStoreBody("creating"),
			idAttr: "id",
		},
		"network": {
			build: func(m *Meta) resource.Resource { return &networkResource{meta: m} },
			plan: func(t *testing.T, r resource.Resource) map[string]tftypes.Value {
				return scoped(map[string]tftypes.Value{
					"id":   str("n1"),
					"evpn": nestedValue(t, r, "evpn", map[string]tftypes.Value{"service_type": str("l3vpn")}),
				})(t, r)
			},
			body:   map[string]any{"id": "n1", "type": "evpn", "state": "creating", "evpn": map[string]any{"type": "l3vpn"}},
			idAttr: "id",
		},
		"subnet": {
			build: func(m *Meta) resource.Resource { return &subnetResource{meta: m} },
			plan: scoped(map[string]tftypes.Value{
				"id": str("s1"), "network": str("n1"),
				"address_pool":  str("/v1/regions/test/networking/address-pools/tenant"),
				"prefix_length": tftypes.NewValue(tftypes.Number, 24),
			}),
			body: map[string]any{
				"id": "s1", "state": "creating", "prefixLength": 24,
				"addressPool": "/v1/regions/test/networking/address-pools/tenant",
			},
			idAttr: "id",
		},
		"security group": {
			build: func(m *Meta) resource.Resource { return &securityGroupResource{meta: m} },
			plan: func(t *testing.T, r resource.Resource) map[string]tftypes.Value {
				return scoped(map[string]tftypes.Value{"id": str("g1"), "egress": oneEgressRule(t)})(t, r)
			},
			body:   map[string]any{"id": "g1", "state": "creating", "rules": oneEgressRuleBody},
			idAttr: "id",
		},
		"VPC peering": {
			build: func(m *Meta) resource.Resource { return &vpcPeeringResource{meta: m} },
			plan: scoped(map[string]tftypes.Value{
				"id": str("p1"), "vpc": str("near"),
				"remote_vpc": str("/v1/regions/test/projects/other/networking/vpcs/far"),
			}),
			body:   peeringBody("creating"),
			idAttr: "id",
		},
		// The binding has no id of its own: the VPC it binds is what state
		// has to carry for the next run to find it.
		"VPC security group binding": {
			build: func(m *Meta) resource.Resource { return &vpcSecurityGroupsResource{meta: m} },
			plan: func(*testing.T, resource.Resource) map[string]tftypes.Value {
				return map[string]tftypes.Value{
					"vpc": str("v1"), "region": str("test"), "project": str("elsewhere"),
					"security_groups": tftypes.NewValue(tftypes.List{ElementType: tftypes.String},
						[]tftypes.Value{str("g1")}),
				}
			},
			body:   map[string]any{"id": "v1", "state": "updating", "securityGroups": []any{"g1"}},
			idAttr: "vpc",
		},
	}
}

// pendingPlatform accepts every write and never lets anything settle. It
// records every request, and interrupt, when set, is called on the first read,
// the way Terraform cancels the context of an apply interrupted mid-wait.
type pendingPlatform struct {
	mu        sync.Mutex
	requests  []string
	body      map[string]any
	interrupt func()
}

func (p *pendingPlatform) serve(t *testing.T) *httptest.Server {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		p.requests = append(p.requests, r.Method+" "+r.URL.Path)
		interrupt := p.interrupt
		if r.Method == http.MethodGet {
			p.interrupt = nil
		}
		p.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodPost:
			// The binding's POST answers 202, every create 201.
			status := http.StatusCreated
			if strings.Contains(r.URL.Path, "/networking/vpcs/") && !strings.Contains(r.URL.Path, "/peerings") {
				status = http.StatusAccepted
			}
			w.WriteHeader(status)
		case http.MethodGet:
			if interrupt != nil {
				interrupt()
			}
		}
		if err := json.NewEncoder(w).Encode(p.body); err != nil {
			t.Errorf("write: %v", err)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// destructive is every request that removes something or stops it.
func (p *pendingPlatform) destructive() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for _, req := range p.requests {
		if strings.HasPrefix(req, http.MethodDelete+" ") || strings.HasSuffix(req, "/cancel") {
			out = append(out, req)
		}
	}
	return out
}

func TestAFailedWaitKeepsEveryKindInState(t *testing.T) {
	for kind, k := range pendingKinds() {
		for mode, interrupted := range map[string]bool{"times out": false, "is interrupted": true} {
			t.Run(kind+" whose wait "+mode, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				platform := &pendingPlatform{body: k.body}
				timeout := 300 * time.Millisecond
				if interrupted {
					platform.interrupt, timeout = cancel, time.Minute
				}
				r := k.build(testClusterMeta(t, platform.serve(t).URL, timeout))
				planRaw, s := testValue(t, r, k.plan(t, r))
				resp := resource.CreateResponse{State: tfsdk.State{Raw: tftypes.NewValue(planRaw.Type(), nil), Schema: s.Schema}}
				r.Create(ctx, resource.CreateRequest{Plan: tfsdk.Plan{Raw: planRaw, Schema: s.Schema}}, &resp)

				// An error, so no pipeline reads the object as ready, saying
				// it's still being built and how to wait for it. The binding
				// creates nothing, so it says it didn't settle.
				if !resp.Diagnostics.HasError() {
					t.Fatal("a create whose wait never ended reported success")
				}
				e := resp.Diagnostics.Errors()[0]
				switch {
				case strings.Contains(e.Summary(), "did not settle"):
				case !strings.Contains(e.Summary(), "is still being created"):
					t.Fatalf("the create failed before its wait: %s", e.Summary())
				case !strings.Contains(e.Detail(), "terraform untaint <address>\n    terraform apply"):
					t.Errorf("the error doesn't give the untaint and apply:\n%s", e.Detail())
				}
				if resp.State.Raw.IsNull() {
					t.Fatalf("nothing was written to state, so the next run cannot find what was created: %v",
						resp.Diagnostics.Errors())
				}
				var id types.String
				resp.Diagnostics.Append(resp.State.GetAttribute(context.Background(), path.Root(k.idAttr), &id)...)
				if id.ValueString() == "" {
					t.Errorf("state carries no %s, so the next run cannot resume", k.idAttr)
				}
				if sent := platform.destructive(); len(sent) > 0 {
					t.Errorf("a failed wait sent %v", sent)
				}
			})
		}
	}
}
