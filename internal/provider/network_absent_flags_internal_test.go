package provider

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/k0rdent"
)

// The platform leaves out a flag nobody set, on a network a cluster made and
// on one created without it. The bodies are what API v1.3.0 answered on kind.
// Each flag defaults to false in the schema, so the read has to say false, or
// the first plan after an import proposes replacing the network.
func TestANetworkReadsAnAbsentFlagAsFalse(t *testing.T) {
	tests := []struct {
		name string
		body string
		got  func(networkModel) map[string]types.Bool
		want map[string]types.Bool
	}{
		{
			name: "an EVPN network a cluster made",
			body: `{"id":"net-1","type":"evpn","evpn":{"type":"l2vpn"},"state":"active",
				"tags":{},"clusterId":"7854f1b8-80a1-457a-9f2a-c9ad74202d5f"}`,
			got: func(m networkModel) map[string]types.Bool {
				return map[string]types.Bool{"dhcp_enabled": m.EVPN.DHCPEnabled}
			},
			want: map[string]types.Bool{"dhcp_enabled": types.BoolValue(false)},
		},
		{
			name: "an EVPN network with DHCP on",
			body: `{"id":"net-2","type":"evpn","evpn":{"type":"l2vpn"},"dhcpEnabled":true,"state":"active","tags":{}}`,
			got: func(m networkModel) map[string]types.Bool {
				return map[string]types.Bool{"dhcp_enabled": m.EVPN.DHCPEnabled}
			},
			want: map[string]types.Bool{"dhcp_enabled": types.BoolValue(true)},
		},
		{
			name: "an InfiniBand network created without either flag",
			body: `{"id":"net-3","type":"infiniband","infiniband":{"partition":"p1"},"state":"active","tags":{}}`,
			got: func(m networkModel) map[string]types.Bool {
				return map[string]types.Bool{"index0": m.InfiniBand.Index0, "ip_over_ib": m.InfiniBand.IPOverIB}
			},
			want: map[string]types.Bool{"index0": types.BoolValue(false), "ip_over_ib": types.BoolValue(false)},
		},
		{
			name: "an InfiniBand network with both flags on",
			body: `{"id":"net-4","type":"infiniband","infiniband":{"partition":"p1","index0":true,"ipOverIb":true},
				"state":"active","tags":{}}`,
			got: func(m networkModel) map[string]types.Bool {
				return map[string]types.Bool{"index0": m.InfiniBand.Index0, "ip_over_ib": m.InfiniBand.IPOverIB}
			},
			want: map[string]types.Bool{"index0": types.BoolValue(true), "ip_over_ib": types.BoolValue(true)},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var n k0rdent.Network
			if err := json.Unmarshal([]byte(tt.body), &n); err != nil {
				t.Fatal(err)
			}
			// An import's prior state: nothing configured to keep.
			m, d := networkStateFrom(context.Background(), n, networkModel{Tags: types.MapNull(types.StringType)}, defaultTags{})
			if d.HasError() {
				t.Fatal(d)
			}
			for name, got := range tt.got(m) {
				if !got.Equal(tt.want[name]) {
					t.Errorf("%s = %s, want %s", name, got, tt.want[name])
				}
			}
		})
	}
}
