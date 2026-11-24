package main

import (
	"net/http"
	"reflect"
	"testing"
)

func TestAPI131MemberGrantEligibility(t *testing.T) {
	member := func(state, address string) fakeInstance {
		return fakeInstance{
			id: "inst-" + state, lifecycle: state,
			mac: "02:00:00:00:00:01", guid: "0x01",
			ethernet: address, ibA: address,
		}
	}
	withoutInterfaces := func(in fakeInstance) fakeInstance {
		in.noInterfaces = true
		return in
	}
	withoutIPv4 := func(in fakeInstance) fakeInstance {
		in.noIPv4 = true
		return in
	}
	ethernet := fakeConsumerNetwork{Type: "ethernet"}

	t.Run("in-flight and degraded members contribute existing addresses and released members do not", func(t *testing.T) {
		members := []fakeInstance{
			member("running", "10.80.10.1"),
			member("provisioning", "10.80.10.2"),
			member("notProvisioned", "10.80.10.3"),
			withoutIPv4(member("deprovisioning", "10.80.10.4")),
			member("degraded", "10.80.10.5"),
			member("deleting", "10.80.10.6"),
		}
		got, refusal := selectConsumerAddresses(members, ethernet)
		want := []string{"10.80.10.1", "10.80.10.2", "10.80.10.3", "10.80.10.5"}
		if refusal != "" || !reflect.DeepEqual(got, want) {
			t.Fatalf("addresses = %v, refusal = %q, want %v without a refusal", got, refusal, want)
		}
	})

	t.Run("missing interfaces and addresses are refused only for serving members", func(t *testing.T) {
		members := []fakeInstance{
			member("running", "10.80.10.1"),
			withoutInterfaces(member("provisioning", "")),
			withoutInterfaces(member("notProvisioned", "")),
			withoutInterfaces(member("degraded", "")),
			withoutIPv4(member("deprovisioning", "10.80.10.4")),
		}
		got, refusal := selectConsumerAddresses(members, ethernet)
		if refusal != "" || !reflect.DeepEqual(got, []string{"10.80.10.1"}) {
			t.Fatalf("addresses = %v, refusal = %q, want only the serving member's address", got, refusal)
		}
		for _, tc := range []struct {
			name   string
			member fakeInstance
		}{
			{"running member has no interface", withoutInterfaces(member("running", ""))},
			{"running member has no IPv4 address", withoutIPv4(member("running", "10.80.10.2"))},
			{"unknown state has no interface", withoutInterfaces(member("rebooting", ""))},
		} {
			t.Run(tc.name, func(t *testing.T) {
				if _, refusal := selectConsumerAddresses([]fakeInstance{member("running", "10.80.10.1"), tc.member}, ethernet); refusal == "" {
					t.Fatal("refusal is empty, want the serving member to be judged")
				}
			})
		}
	})

	t.Run("a narrowed selector ignores in-flight members without a match", func(t *testing.T) {
		mac := "02:00:00:00:00:01"
		narrowed := fakeConsumerNetwork{Type: "ethernet", MacAddress: &mac}
		other := member("running", "10.80.10.9")
		other.mac = "02:00:00:00:00:09"
		members := []fakeInstance{other, withoutInterfaces(member("provisioning", ""))}
		if got, refusal := selectConsumerAddresses(members, narrowed); refusal != "" || len(got) != 0 {
			t.Fatalf("addresses = %v, refusal = %q, want no addresses and no refusal", got, refusal)
		}
		members = append(members, member("provisioning", "10.80.10.2"))
		if got, refusal := selectConsumerAddresses(members, narrowed); refusal != "" || !reflect.DeepEqual(got, []string{"10.80.10.2"}) {
			t.Fatalf("addresses = %v, refusal = %q, want the matching in-flight member's address", got, refusal)
		}
		if _, refusal := selectConsumerAddresses([]fakeInstance{other}, narrowed); refusal == "" {
			t.Fatal("refusal is empty when no required member matches")
		}
	})
}

func TestAPI131ScaleUpGrantUsesAddressesAlreadyPresent(t *testing.T) {
	for _, tc := range []struct {
		name    string
		marker  string
		want    int
		wantOld bool
	}{
		{name: "API v1.3.0 keeps its original mid-scale resolution", wantOld: true, want: 2},
		{name: "API v1.3.1 grants only addressed members", marker: apiV131, want: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newState()
			body := `{"id":"g","clusterType":"/v1/regions/r/compute/cluster-types/cpu-general",` +
				`"nodePools":[{"nodePool":"workers","size":1}],"displayName":"` + tc.marker + `"}`
			if code, out := in(t, s.createInstanceGroup, http.MethodPost, "r", "p", nil, body); code != http.StatusCreated {
				t.Fatalf("create: %d %v", code, out)
			}
			g := map[string]string{"id": "g"}
			for i := 0; i < settleAfter+1; i++ {
				in(t, s.getInstanceGroup, http.MethodGet, "r", "p", g, "")
			}
			if code, out := in(t, s.scaleInstanceGroup, http.MethodPost, "r", "p", g, `{"nodePools":[{"nodePool":"workers","size":2}]}`); code != http.StatusOK {
				t.Fatalf("scale: %d %v", code, out)
			}

			o := s.scopes[scope{region: "r", project: "p"}]
			created := s.groupBody("r", o.groups[at{id: "g"}])
			_, hasPhase := onlyPool(t, created)["reservationPhase"]
			if hasPhase == tc.wantOld {
				t.Fatalf("reservationPhase present = %v, want %v", hasPhase, !tc.wantOld)
			}

			kind := "instanceGroup"
			consumers, statuses, refusal := o.resolveConsumers([]fakeConsumer{{
				Ref: "g", Type: &kind, Network: &fakeConsumerNetwork{Type: "ethernet"},
			}})
			if refusal != "" {
				t.Fatalf("resolveConsumers: %s", refusal)
			}
			if len(consumers) != 1 || len(statuses) != 1 || len(statuses[0].addresses) != tc.want {
				t.Fatalf("consumers = %v, addresses = %v, want %d address(es)", consumers, statuses[0].addresses, tc.want)
			}
		})
	}
}

func TestAPI131FailedCreateReleasesReservationsBeforeFailing(t *testing.T) {
	s := newState()
	body := `{"id":"g","clusterType":"/v1/regions/r/compute/cluster-types/cpu-general",` +
		`"nodePools":[{"nodePool":"workers","size":1}],"displayName":"` + apiV131 + " " + createFails + `"}`
	if code, out := in(t, s.createInstanceGroup, http.MethodPost, "r", "p", nil, body); code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, out)
	}
	o := s.scopes[scope{region: "r", project: "p"}]
	g := o.groups[at{id: "g"}]
	if got := onlyPool(t, s.groupBody("r", g))["reservationPhase"]; got != "acquiring" {
		t.Fatalf("initial reservationPhase = %v, want acquiring", got)
	}

	for _, step := range []struct {
		name, state, phase string
	}{
		{name: "release starts", state: "creating", phase: "releasing"},
		{name: "release finishes", state: "creating", phase: "released"},
		{name: "group reports failed", state: "failed"},
	} {
		t.Run(step.name, func(t *testing.T) {
			advanceGroup(o, g)
			advanceGroup(o, g)
			body := s.groupBody("r", g)
			if body["state"] != step.state {
				t.Fatalf("state = %v, want %s", body["state"], step.state)
			}
			got := onlyPool(t, body)["reservationPhase"]
			if step.phase == "" {
				if got != nil {
					t.Fatalf("reservationPhase = %v, want none after settling", got)
				}
			} else if got != step.phase {
				t.Fatalf("reservationPhase = %v, want %s", got, step.phase)
			}
			if step.phase != "" && body["statusReason"] == nil {
				t.Fatal("rollback answer has no statusReason")
			}
		})
	}
}
