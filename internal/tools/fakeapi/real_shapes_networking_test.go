package main

import (
	"net/http"
	"testing"
)

// Shapes from the run-3 drift triage on the networking routes, each answered
// as API v1.3.0-rc.5 does.

// A network answers its uid, both timestamps and its subnets, `[]` on the
// create, and only a network the cluster lifecycle made names its VPC and
// schema entry.
func TestANetworkAnswersItsUIDAndItsSubnets(t *testing.T) {
	s := newState()
	n := map[string]string{"id": "n", "networkId": "n"}
	_, created := in(t, s.createNetwork, http.MethodPost, "r", "p", nil, `{"id":"n","type":"evpn","evpn":{"type":"l2vpn"}}`)
	if created["uid"] == nil || created["uid"] == "" || created["createdAt"] == nil || created["updatedAt"] == nil {
		t.Errorf("create: uid %v, createdAt %v, updatedAt %v", created["uid"], created["createdAt"], created["updatedAt"])
	}
	if subs, ok := created["subnets"].([]any); !ok || len(subs) != 0 {
		t.Errorf("create: subnets = %v, want []", created["subnets"])
	}
	for _, key := range []string{"vpc", "schemaEntryId"} {
		if _, ok := created[key]; ok {
			t.Errorf("a tenant network answers %s %v", key, created[key])
		}
	}
	if code, out := in(t, s.createSubnet, http.MethodPost, "r", "p", n, `{"id":"x","addressPool":"tenant","prefixLength":24}`); code != http.StatusCreated {
		t.Fatalf("subnet: %d %v", code, out)
	}
	_, got := in(t, s.getNetwork, http.MethodGet, "r", "p", n, "")
	subs := listOf(t, got["subnets"])
	if len(subs) != 1 || object(t, subs[0])["id"] != "x" || got["uid"] != created["uid"] {
		t.Errorf("get: uid %v, subnets %v, want the create's uid and subnet x", got["uid"], subs)
	}

	clusterInP(t, s)
	_, list := in(t, s.listNetworks, http.MethodGet, "r", "p", nil, "")
	var owned map[string]any
	for _, row := range listOf(t, list["networks"]) {
		if net := object(t, row); net["clusterId"] != nil {
			owned = net
		}
	}
	if owned == nil {
		t.Fatalf("no cluster network in %v", list)
	}
	if owned["vpc"] != ufoCRName("vpc-c") || owned["schemaEntryId"] == nil || owned["schemaEntryId"] == "" {
		t.Errorf("cluster network: vpc %v, schemaEntryId %v, want vpc %s and an entry", owned["vpc"], owned["schemaEntryId"], ufoCRName("vpc-c"))
	}
	if sub := object(t, listOf(t, owned["subnets"])[0]); sub["schemaEntryId"] == nil || sub["uid"] == nil {
		t.Errorf("cluster subnet: %v, want a uid and a schema entry", sub)
	}
}

// A subnet answers its uid and timestamps always, and its IP version with its
// CIDR, once the create settles.
func TestASubnetAnswersItsIPVersionOnceAllocated(t *testing.T) {
	s := newState()
	if code, out := in(t, s.createNetwork, http.MethodPost, "r", "p", nil, `{"id":"n","type":"evpn","evpn":{"type":"l2vpn"}}`); code != http.StatusCreated {
		t.Fatalf("network: %d %v", code, out)
	}
	path := map[string]string{"networkId": "n", "id": "x"}
	_, created := in(t, s.createSubnet, http.MethodPost, "r", "p", path, `{"id":"x","addressPool":"tenant","prefixLength":24}`)
	if created["state"] != "creating" || created["uid"] == nil || created["createdAt"] == nil || created["updatedAt"] == nil {
		t.Errorf("create: %v, want creating with a uid and both timestamps", created)
	}
	if _, ok := created["ipVersion"]; ok {
		t.Errorf("create answers ipVersion %v before the CIDR is allocated", created["ipVersion"])
	}
	var active map[string]any
	for range settleAfter + 2 {
		_, active = in(t, s.getSubnet, http.MethodGet, "r", "p", path, "")
	}
	if active["state"] != "active" || active["ipVersion"] != "ipv4" || active["cidr"] == nil || active["uid"] != created["uid"] {
		t.Errorf("active: %v, want ipv4 beside the CIDR and the create's uid", active)
	}
}

// A security group answers its uid and both timestamps, a VPC's default group
// its own uid, and a failed group its detail in `statusReason`, the only field
// the model has for it.
func TestASecurityGroupAnswersItsUIDAndStatusReason(t *testing.T) {
	s := newState()
	_, created := in(t, s.createSecurityGroup, http.MethodPost, "r", "p", nil, `{`+oneRule+`,"id":"g"}`)
	if created["uid"] == nil || created["uid"] == "" || created["createdAt"] == nil || created["updatedAt"] == nil {
		t.Errorf("create: %v, want a uid and both timestamps", created)
	}

	clusterInP(t, s)
	_, list := in(t, s.listSecurityGroups, http.MethodGet, "r", "p", nil, "")
	uids := map[any]bool{}
	for _, row := range listOf(t, list["securityGroups"]) {
		g := object(t, row)
		if g["uid"] == nil || g["uid"] == "" || uids[g["uid"]] {
			t.Errorf("group %v: uid %v, want one of its own", g["id"], g["uid"])
		}
		uids[g["uid"]] = true
	}
	if len(uids) != 2 {
		t.Errorf("groups = %d, want the tenant's and the VPC's default", len(uids))
	}

	failed := map[string]string{"id": "f"}
	in(t, s.createSecurityGroup, http.MethodPost, "r", "p", nil, `{`+oneRule+`,"id":"f","displayName":"`+settlesFailed+`"}`)
	var got map[string]any
	for range settleAfter + 2 {
		_, got = in(t, s.getSecurityGroup, http.MethodGet, "r", "p", failed, "")
	}
	if got["state"] != "failed" || got["statusReason"] == nil || got["statusReason"] == "" {
		t.Errorf("failed group: %v, want statusReason", got)
	}
	for _, key := range []string{"stateMessage", "stateReason"} {
		if _, ok := got[key]; ok {
			t.Errorf("failed group answers %s, which a security group doesn't have", key)
		}
	}
}

// The seeded landscape holds the rows an earlier cluster's workflow left, and
// look/ reads them first (run rapi-hw-039727): the network and subnet the
// workflow materialized, an InfiniBand one beside them with no attributes, as
// the schema's own entry, and the VPC's default group. Each carries what the
// platform owns it by, which a kind stack, starting empty, never answers.
func TestTheSeededLeftoverClusterRowsCarryWhatTheWorkflowMade(t *testing.T) {
	s := newState()
	const region, project = "us-texas-1", "demo"
	const leftover = "ml-platform-01"
	_, list := in(t, s.listNetworks, http.MethodGet, region, project, nil, "")
	var net, ib map[string]any
	for _, row := range listOf(t, list["networks"]) {
		switch object(t, row)["id"] {
		case "vpc-" + leftover + "-net":
			net = object(t, row)
		case "vpc-" + leftover + "-ib":
			ib = object(t, row)
		}
	}
	if net == nil || ib == nil {
		t.Fatalf("the leftover rows are missing from %v", list["networks"])
	}
	for key, want := range map[string]string{
		"clusterId": leftover, "vpc": ufoCRName("vpc-" + leftover), "schemaEntryId": "cluster-net",
	} {
		if net[key] != want {
			t.Errorf("leftover network %s = %v, want %q", key, net[key], want)
		}
	}
	sub := object(t, listOf(t, net["subnets"])[0])
	if sub["clusterId"] != leftover || sub["schemaEntryId"] != "cluster-subnet" {
		t.Errorf("leftover subnet = %v, want the cluster id and its schema entry", sub)
	}
	fabric, ok := ib["infiniband"].(map[string]any)
	if ib["type"] != "infiniband" || !ok || len(fabric) != 0 {
		t.Errorf("leftover infiniband = %v %v, want an empty object", ib["type"], ib["infiniband"])
	}
	if ib["clusterId"] != leftover || ib["vpc"] != ufoCRName("vpc-"+leftover) || ib["schemaEntryId"] != "cluster-ib" {
		t.Errorf("leftover infiniband network = %v", ib)
	}

	_, got := in(t, s.getNetwork, http.MethodGet, region, project, map[string]string{"id": "vpc-" + leftover + "-net"}, "")
	if got["clusterId"] != leftover || got["vpc"] != ufoCRName("vpc-"+leftover) {
		t.Errorf("leftover network by id = %v", got)
	}
	_, subnet := in(t, s.getSubnet, http.MethodGet, region, project,
		map[string]string{"networkId": "vpc-" + leftover + "-net", "id": "vpc-" + leftover + "-net-sub"}, "")
	if subnet["clusterId"] != leftover || subnet["schemaEntryId"] != "cluster-subnet" {
		t.Errorf("leftover subnet by id = %v", subnet)
	}

	_, vpc := in(t, s.getVPC, http.MethodGet, region, project, map[string]string{"id": "vpc-" + leftover}, "")
	if vpc["ownerKind"] != "cluster" || vpc["ownerId"] == nil || vpc["ownerId"] == "" {
		t.Errorf("leftover vpc = %v, want the cluster's ownership", vpc)
	}

	_, groups := in(t, s.listSecurityGroups, http.MethodGet, region, project, nil, "")
	rows := listOf(t, groups["securityGroups"])
	if len(rows) != 1 {
		t.Fatalf("the seeded project holds %d security groups, want the default one", len(rows))
	}
	g := object(t, rows[0])
	if g["ownerKind"] != "vpc" || g["ownerId"] == nil || g["ownerId"] == "" {
		t.Errorf("default group = %v, want the VPC's ownership", g)
	}
	id, ok := g["id"].(string)
	if !ok {
		t.Fatalf("default group id = %v, want a string", g["id"])
	}
	_, byID := in(t, s.getSecurityGroup, http.MethodGet, region, project, map[string]string{"id": id}, "")
	if byID["ownerKind"] != "vpc" || byID["ownerId"] != g["ownerId"] {
		t.Errorf("default group by id = %v, want the VPC's ownership", byID)
	}
}

// The catalog documents its three named pools, as a landscape's admin has
// (run rapi-hw-039727 answered a description on 2 of 11 pools), and look/
// reads those three first. The generated ones carry none, so the difference
// between a documented and an undocumented pool stays visible.
func TestTheNamedAddressPoolsAnswerDescriptions(t *testing.T) {
	s := newState()
	_, list := in(t, s.listAddressPools, http.MethodGet, "us-texas-1", "demo", nil, "")
	// The list's default page is small; its first row is the named pool
	// look/ reads first.
	rows := listOf(t, list["addressPools"])
	if len(rows) == 0 || object(t, rows[0])["description"] == nil {
		t.Errorf("the first listed pool answers no description: %v", rows)
	}
	for _, id := range []string{"tenant", "cluster-local", "vip"} {
		_, pool := in(t, s.getAddressPool, http.MethodGet, "us-texas-1", "demo", map[string]string{"id": id}, "")
		if pool["description"] == nil || pool["description"] == "" {
			t.Errorf("pool %s answers no description: %v", id, pool)
		}
	}
	_, filler := in(t, s.getAddressPool, http.MethodGet, "us-texas-1", "demo", map[string]string{"id": "filler-000"}, "")
	if _, ok := filler["description"]; ok {
		t.Errorf("a filler pool answers a description: %v", filler)
	}
}
