package main

import (
	"fmt"
	"net/http"
	"net/netip"
	"testing"
)

// A subnet's CIDR is "server-allocated from the AddressPool", so it lies inside
// the pool's prefix, and its gateway is the block's last usable host address.
func TestSubnetCIDRComesFromItsPool(t *testing.T) {
	s := newState()
	if code, out := in(t, s.createNetwork, http.MethodPost, "r", "p", nil,
		`{"id":"n","type":"evpn","evpn":{"type":"l3vpn"}}`); code != http.StatusCreated {
		t.Fatalf("network: %d %v", code, out)
	}
	for _, c := range []struct {
		id, pool, within, gateway string
		length                    int
	}{
		{"a", "tenant", "10.0.0.0/8", "10.0.0.254", 24},
		{"b", "tenant", "10.0.0.0/8", "10.0.1.254", 24},
		{"c", "cluster-local", "192.168.0.0/16", "", 28},
		{"d", "vip", "198.51.100.0/24", "", 32},
	} {
		path := map[string]string{"networkId": "n"}
		body := fmt.Sprintf(`{"id":%q,"addressPool":"/v1/regions/us-texas-1/networking/address-pools/%s","prefixLength":%d,"allocateGateway":true}`,
			c.id, c.pool, c.length)
		if code, out := in(t, s.createSubnet, http.MethodPost, "r", "p", path, body); code != http.StatusCreated {
			t.Fatalf("%s: create: %d %v", c.id, code, out)
		}
		settleIn(t, s.getSubnet, "p", withID(path, c.id))
		_, out := in(t, s.getSubnet, http.MethodGet, "r", "p", withID(path, c.id), "")

		cidr := fmt.Sprint(out["cidr"])
		got, err := netip.ParsePrefix(cidr)
		if err != nil || got.Bits() != c.length || !netip.MustParsePrefix(c.within).Contains(got.Addr()) {
			t.Errorf("%s: cidr %q, want a /%d inside %s", c.id, cidr, c.length, c.within)
		}
		// cluster-local isn't routable and a /32 has no host to spare, so
		// neither gets a gateway.
		gateway, ok := out["gateway"].(string)
		if !ok {
			gateway = ""
		}
		if gateway != c.gateway {
			t.Errorf("%s: gateway %q in %s, want %q", c.id, gateway, cidr, c.gateway)
		}
	}
}
