package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// A subnet whose pool can't give its size used to fail at create, after
// everything before it in the apply. These pin that it fails at plan, from
// the pool's own read, and that the check stays out of the way otherwise.

// poolAPI serves the address pools in pools by id, in region us-texas-1, and
// counts the reads.
type poolAPI struct {
	pools map[string]map[string]any
	reads atomic.Int32
}

func (a *poolAPI) meta(t *testing.T) *Meta {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const poolPath = "/v1/regions/us-texas-1/networking/address-pools/"
		switch {
		case r.URL.Path == "/v1/regions/global/auth/token":
			writeJSON(w, map[string]any{"access_token": "t", "token_type": "Bearer", "expires_in": 3600})
		case r.URL.Path == "/v1/regions/global/projects":
			writeJSON(w, map[string]any{"projects": []any{
				map[string]any{"id": "p", "organizationId": "o", "region": "us-texas-1"},
			}, "totalCount": 1})
		case strings.HasPrefix(r.URL.Path, poolPath):
			a.reads.Add(1)
			pool, ok := a.pools[strings.TrimPrefix(r.URL.Path, poolPath)]
			if !ok {
				writeTestError(t, w, http.StatusNotFound, "no such pool")
				return
			}
			writeJSON(w, pool)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	meta, err := newMeta(Settings{Endpoint: server.URL, Region: "us-texas-1", Project: "p",
		Credential: CredentialSource{Kind: APIKeyCredential, APIKey: "k"}}, "test")
	if err != nil {
		t.Fatal(err)
	}
	return meta
}

func pool(id string, minLen, maxLen int, prefixes ...string) map[string]any {
	p := map[string]any{"id": id, "ipVersion": "ipv4", "prefixes": prefixes, "routable": false,
		"excludeRanges": []string{}, "state": "active"}
	if minLen > 0 {
		p["minPrefixLength"] = minLen
	}
	if maxLen > 0 {
		p["maxPrefixLength"] = maxLen
	}
	return p
}

const poolURI = "/v1/regions/us-texas-1/networking/address-pools/"

// planSubnet runs the subnet's ModifyPlan for a subnet asking poolRef for a
// /length. A nil prior is a create.
func planSubnet(t *testing.T, meta *Meta, poolRef, length tftypes.Value, prior map[string]tftypes.Value) resource.ModifyPlanResponse {
	t.Helper()
	r := &subnetResource{meta: meta}
	set := map[string]tftypes.Value{"id": str("s"), "network": str("n"), "region": str("us-texas-1"),
		"project": str("p"), "address_pool": poolRef, "prefix_length": length}
	configRaw, s := testValue(t, r, set)
	stateRaw := tftypes.NewValue(configRaw.Type(), nil)
	if prior != nil {
		stateRaw, _ = testValue(t, r, prior)
	}
	plan := tfsdk.Plan{Raw: configRaw, Schema: s.Schema}
	resp := resource.ModifyPlanResponse{Plan: plan}
	r.ModifyPlan(context.Background(), resource.ModifyPlanRequest{
		Config: tfsdk.Config{Raw: configRaw, Schema: s.Schema},
		Plan:   plan,
		State:  tfsdk.State{Raw: stateRaw, Schema: s.Schema},
	}, &resp)
	return resp
}

func TestASubnetItsPoolCannotGiveIsRefusedAtPlan(t *testing.T) {
	api := &poolAPI{pools: map[string]map[string]any{
		"tenant":   pool("tenant", 16, 28, "10.0.0.0/8"),
		"small":    pool("small", 8, 30, "10.1.0.0/26", "10.2.0.0/27"),
		"unbound":  pool("unbound", 0, 0, "10.0.0.0/8"),
		"only-min": pool("only-min", 20, 0, "10.0.0.0/8"),
	}}
	tests := []struct {
		name   string
		pool   string
		length int64
		want   []string // "" for a plan that passes
	}{
		{"inside the range", "tenant", 24, nil},
		{"larger than the range", "tenant", 12, []string{"prefix_length", `"tenant"`, "/16 to /28",
			"is 12", "between 16 and 28", "`address_pool`", "/12"}},
		{"smaller than the range", "tenant", 30, []string{"between 16 and 28", "is 30"}},
		{"larger than any prefix", "small", 24, []string{"larger than any prefix", "10.1.0.0/26, 10.2.0.0/27",
			"a /26", "is 24", "to 26 or more"}},
		{"fits the largest prefix", "small", 26, nil},
		{"a bound the read leaves out isn't checked", "unbound", 30, nil},
		{"a prefix still bounds a pool with no range", "unbound", 4, []string{"larger than any prefix", "a /8"}},
		{"a lone minimum", "only-min", 16, []string{"/20 or smaller", "to 20 or more"}},
		{"a pool that can't be read is left to the platform", "missing", 24, nil},
		{"a bare id in the subnet's region", "tenant", 12, []string{"between 16 and 28"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ref := poolURI + tt.pool
			if strings.HasPrefix(tt.name, "a bare id") {
				ref = tt.pool
			}
			resp := planSubnet(t, api.meta(t), str(ref), num(tt.length), nil)
			if len(tt.want) == 0 {
				if resp.Diagnostics.HasError() {
					t.Fatalf("refused: %v", resp.Diagnostics)
				}
				return
			}
			if !resp.Diagnostics.HasError() {
				t.Fatal("planned")
			}
			d := resp.Diagnostics.Errors()[0]
			text := d.Summary() + "\n" + d.Detail()
			for _, want := range tt.want {
				if !strings.Contains(text, want) {
					t.Errorf("diagnostic does not say %q:\n%s", want, text)
				}
			}
			for _, banned := range []string{"\u2014", ";", "->"} {
				if strings.Contains(text, banned) {
					t.Errorf("diagnostic contains %q:\n%s", banned, text)
				}
			}
		})
	}
}

// A value unknown at plan, a pool from another resource in the same apply,
// is left to the platform without a read.
func TestAnUnknownPoolOrSizeIsLeftToThePlatform(t *testing.T) {
	api := &poolAPI{pools: map[string]map[string]any{"tenant": pool("tenant", 16, 28, "10.0.0.0/8")}}
	meta := api.meta(t)
	unknownStr := tftypes.NewValue(tftypes.String, tftypes.UnknownValue)
	unknownNum := tftypes.NewValue(tftypes.Number, tftypes.UnknownValue)
	for _, resp := range []resource.ModifyPlanResponse{
		planSubnet(t, meta, unknownStr, num(12), nil),
		planSubnet(t, meta, str(poolURI+"tenant"), unknownNum, nil),
	} {
		if resp.Diagnostics.HasError() {
			t.Errorf("refused: %v", resp.Diagnostics)
		}
	}
	if n := api.reads.Load(); n != 0 {
		t.Errorf("the pool was read %d times, want 0", n)
	}
}

// A subnet that exists keeps its size, whatever its pool says now, and is
// planned without a read. A move to another size is checked.
func TestAnExistingSubnetIsCheckedOnlyWhenItsSizeOrPoolMoves(t *testing.T) {
	api := &poolAPI{pools: map[string]map[string]any{"tenant": pool("tenant", 16, 28, "10.0.0.0/8")}}
	meta := api.meta(t)
	prior := map[string]tftypes.Value{"id": str("s"), "network": str("n"), "region": str("us-texas-1"),
		"project": str("p"), "address_pool": str(poolURI + "tenant"), "prefix_length": num(12)}
	if resp := planSubnet(t, meta, str(poolURI+"tenant"), num(12), prior); resp.Diagnostics.HasError() {
		t.Errorf("an unchanged subnet refused: %v", resp.Diagnostics)
	}
	if n := api.reads.Load(); n != 0 {
		t.Errorf("the pool was read %d times for an unchanged subnet, want 0", n)
	}
	if resp := planSubnet(t, meta, str(poolURI+"tenant"), num(30), prior); !resp.Diagnostics.HasError() {
		t.Error("a replacement onto a size the pool can't give planned")
	}
}

// Many subnets on one pool read it once.
func TestThePoolIsReadOncePerRun(t *testing.T) {
	api := &poolAPI{pools: map[string]map[string]any{"tenant": pool("tenant", 16, 28, "10.0.0.0/8")}}
	meta := api.meta(t)
	for range 3 {
		planSubnet(t, meta, str(poolURI+"tenant"), num(24), nil)
	}
	if n := api.reads.Load(); n != 1 {
		t.Errorf("the pool was read %d times, want 1", n)
	}
}
