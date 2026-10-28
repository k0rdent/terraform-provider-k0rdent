package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"
)

// The networking slice of the fake: tenant networks, the subnets allocated
// inside them, and the read-only VPCs the cluster workflow creates.
//
// Same rule as the rest of the file: the fake must not agree with itself. Four
// hazards are modelled here, each of them the reading that breaks a provider
// which simply echoes what it was sent.

type fakeNetwork struct {
	id          string
	kind        string // evpn, infiniband, nvlink
	displayName *string
	description *string
	tags        map[string]string

	// EVPN
	serviceType string
	mtu         *int
	dhcpEnabled *bool
	dnsServers  *[]string
	vlanID      string // as resolved, never "auto"

	// InfiniBand
	partition string
	pkey      string
	index0    *bool
	ipOverIB  *bool

	clusterID string // non-empty when the cluster lifecycle owns this, not the tenant
	state     string
	reads     int
}

type fakeSubnet struct {
	id            string
	network       string
	addressPool   string
	prefixLength  int
	gateway       string
	cidr          string
	excludeRanges []string
	displayName   *string
	description   *string
	tags          map[string]string
	clusterID     string // as on fakeNetwork
	state         string
	reads         int
}

type fakeVPC struct {
	id             string
	uid            string
	owner          string
	ownerKind      string
	ownerUID       string
	backend        string // "nico" carries security groups; "ufo" answers 409
	securityGroups []string
	// defaultGroup is the id of the VPC's own default security group, which
	// an empty binding puts back. Empty on a `ufo` VPC, which has none.
	defaultGroup string
	state        string
	reads        int
}

// vpcForOwner is how a VPC comes into being. There is no create operation: the
// cluster and instance-group workflows make one, which is exactly why
// the provider exposes VPCs as a data source and not a resource.
func (o *objects) vpcForOwner(ownerKind, ownerID string) {
	vpc := "vpc-" + ownerID
	// The fabric backend, which used to be the constant "nico" for every VPC.
	// Only a `nico` VPC can carry security groups, the provider has a dedicated
	// 409 handler that says so, and with one backend in the fake that handler
	// could never fire. An owner id carrying the marker lands on `ufo` instead.
	backend := "nico"
	if strings.Contains(ownerID, ufoBackendMarker) {
		backend = "ufo"
	}
	v := &fakeVPC{
		id: vpc, uid: uidFor(o.scope, "vpc", vpc), backend: backend,
		owner: ownerID, ownerKind: ownerKind, ownerUID: uidFor(o.scope, ownerKind, ownerID),
		securityGroups: []string{}, state: "active",
	}
	o.vpcs[at{id: vpc}] = v

	// The network and subnet the workflow creates, unless the owner id
	// asks for a VPC with none. Both are the platform's, not the tenant's, and
	// the tenant API answers 409 to any write.
	var prefixes []string
	if !strings.Contains(ownerID, noSubnetsMarker) {
		o.networks[at{id: vpc + "-net"}] = &fakeNetwork{
			id: vpc + "-net", kind: "evpn", serviceType: "l2vpn", vlanID: "4002",
			clusterID: ownerID, state: "active", displayName: ptr("cluster network"),
		}
		o.subnets[at{vpc + "-net", vpc + "-net-sub"}] = &fakeSubnet{
			id: vpc + "-net-sub", network: vpc + "-net",
			addressPool: "pool-routable", prefixLength: 24,
			cidr: "10.90.0.0/24", clusterID: ownerID, state: "active",
			displayName: ptr("cluster subnet"),
		}
		prefixes = append(prefixes, "10.90.0.0/24")
	}

	// The contract: "Only VPCs on the `nico` fabric backend get one."
	if backend != "nico" {
		return
	}
	g := defaultGroup(v, prefixes)
	o.groupsSec[at{id: g.id}] = g
	v.defaultGroup = g.id
	v.securityGroups = []string{g.id}
}

// defaultGroup is a VPC's default security group, as the k0rdent API
// v1.3.0-rc.3 makes it: one per VPC, owned by it, and bound to it.
//
// The id is `<vpc id>-<8 random hex>-default`. It's random upstream so a
// tenant group can't claim it first, which also means nothing can derive it:
// read it off the VPC's binding, or find the group whose ownerId is the VPC's
// uid. A provider or a test that wrote the id out would pass against a fixed
// one and fail on a platform.
//
// The rules come from the VPC's subnet prefixes, deduplicated and sorted as
// strings: an `intra-vpc-<i>` ingress permit for every pair of them, both
// orders and each with itself, then one `intra-vpc-egress-<i>` egress permit
// from 0.0.0.0/0 to each. No allow-all egress, which rc.3 dropped. A VPC with
// no prefixes gets a group with no rules.
//
// It's made `active`. rc.3 makes it `creating` and settles it at once, and the
// recorded read shows `active`.
func defaultGroup(v *fakeVPC, prefixes []string) *fakeSecurityGroup {
	const suffix = "-default"
	random := make([]byte, 4)
	_, _ = rand.Read(random)
	// The VPC id is cut to fit 63 characters, never the other two parts.
	slug := v.id
	if budget := 63 - len(suffix) - 8 - 1; len(slug) > budget {
		slug = slug[:budget]
	}
	slug = strings.TrimRight(slug, "-")

	sorted := slices.Compact(slices.Sorted(slices.Values(prefixes)))
	ingress := make([]fakeRule, 0, len(sorted)*len(sorted))
	for _, src := range sorted {
		for _, dst := range sorted {
			ingress = append(ingress, fakeRule{
				Name: ptr(fmt.Sprintf("intra-vpc-%d", len(ingress))), Protocol: "any", Action: "permit",
				SourcePrefix: src, DestinationPrefix: dst,
			})
		}
	}
	egress := make([]fakeRule, 0, len(sorted))
	for _, dst := range sorted {
		egress = append(egress, fakeRule{
			Name: ptr(fmt.Sprintf("intra-vpc-egress-%d", len(egress))), Protocol: "any", Action: "permit",
			SourcePrefix: "0.0.0.0/0", DestinationPrefix: dst,
		})
	}

	return &fakeSecurityGroup{
		id:          slug + "-" + hex.EncodeToString(random) + suffix,
		ownerKind:   "vpc",
		ownerID:     v.uid,
		state:       "active",
		displayName: ptr("Default (" + v.id + ")"),
		description: ptr("Platform-managed default security group for VPC " + v.id +
			". Permits traffic between the VPC's subnet prefixes, in both directions, " +
			"and nothing else. Not editable and cannot be deleted. To add rules, such " +
			"as egress leaving the VPC, attach your own group to the cluster or " +
			"instance group, or list it next to this group on the VPC: a VPC list " +
			"without this group replaces it."),
		ingress: ingress,
		egress:  egress,
	}
}

// dropVPC takes a VPC away with its owner, and its default group with it:
// "removed only when its VPC is". The caller holds s.mu.
func (o *objects) dropVPC(id string) {
	if v, ok := o.vpcs[at{id: id}]; ok && v.defaultGroup != "" {
		delete(o.groupsSec, at{id: v.defaultGroup})
	}
	delete(o.vpcs, at{id: id})
}

func ptr[T any](v T) *T { return &v }

func (s *state) listNetworks(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	networks, _ := networkKind.list(w, r, s.in(r))

	items := make([]any, 0, len(networks))
	for _, n := range networks {
		items = append(items, networkJSON(n))
	}
	networkList.serve(w, r, items)
}

func (s *state) createNetwork(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID          *string            `json:"id"`
		Type        string             `json:"type"`
		DisplayName *string            `json:"displayName"`
		Description *string            `json:"description"`
		Tags        *map[string]string `json:"tags"`
		MTU         *int               `json:"mtu"`
		DhcpEnabled *bool              `json:"dhcpEnabled"`
		DNSServers  *[]string          `json:"dnsServers"`
		Evpn        *struct {
			Type string `json:"type"`
		} `json:"evpn"`
		Vlan *struct {
			ID *string `json:"id"`
		} `json:"vlan"`
		Infiniband *struct {
			Partition *string `json:"partition"`
			Pkey      *string `json:"pkey"`
			Index0    *bool   `json:"index0"`
			IPOverIB  *bool   `json:"ipOverIb"`
		} `json:"infiniband"`
		Nvlink *struct {
			Partition *string `json:"partition"`
		} `json:"nvlink"`
	}
	if !decode(w, r, &body) {
		return
	}

	switch body.Type {
	case "evpn", "infiniband", "nvlink":
	default:
		apiError(w, http.StatusUnprocessableEntity, "INVALID_ARGUMENT",
			"type must be evpn, infiniband or nvlink, got "+body.Type)
		return
	}
	if body.Type == "evpn" && (body.Evpn == nil || body.Evpn.Type == "") {
		apiError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "evpn.type is required on an evpn network")
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	o, _, ok := networkKind.admit(s, w, r)
	if !ok {
		return
	}
	id, ok := networkKind.claim(s, w, o, "", body.ID)
	if !ok {
		return
	}

	n := &fakeNetwork{
		id: id, kind: body.Type,
		displayName: body.DisplayName, description: body.Description,
		mtu: body.MTU, dhcpEnabled: body.DhcpEnabled, dnsServers: body.DNSServers,
		state: "creating",
	}
	if body.Tags != nil {
		n.tags = *body.Tags
	}

	switch body.Type {
	case "evpn":
		n.serviceType = body.Evpn.Type
		// ASSUMPTION: `vlan.id = "auto"` is **resolved to a concrete id** on
		// write and never echoed back as "auto". The contract says `auto` lets
		// the platform pick and says nothing about what a later read returns,
		// and of the two readings this is the one that breaks a provider: a
		// provider that writes the response into the same attribute the user
		// configured proposes replacing the network on every subsequent plan.
		n.vlanID = "4001"
		if body.Vlan != nil && body.Vlan.ID != nil && *body.Vlan.ID != "auto" {
			n.vlanID = *body.Vlan.ID
		}
	case "infiniband":
		if body.Infiniband != nil {
			if body.Infiniband.Partition != nil {
				n.partition = *body.Infiniband.Partition
			}
			n.index0, n.ipOverIB = body.Infiniband.Index0, body.Infiniband.IPOverIB
		}
		// ASSUMPTION: an omitted PKey is allocated by the platform, and a
		// supplied one is ignored in favour of the partition's own. The
		// contract states both behaviours, UFO allocating and NICo ignoring, so
		// a fake that echoed the request would model neither backend.
		n.pkey = "0x8001"
	case "nvlink":
		if body.Nvlink != nil && body.Nvlink.Partition != nil {
			n.partition = *body.Nvlink.Partition
		}
	}

	networkKind.put(o, "", id, n)
	writeJSON(w, http.StatusCreated, networkJSON(n))
}

func (s *state) getNetwork(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n, ok := networkKind.read(w, r, s.in(r)); ok {
		writeJSON(w, http.StatusOK, networkJSON(n))
	}
}

func (s *state) updateNetwork(w http.ResponseWriter, r *http.Request) {
	var body struct {
		DisplayName *string            `json:"displayName"`
		Description *string            `json:"description"`
		Tags        *map[string]string `json:"tags"`
	}
	if !decode(w, r, &body) {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	n, _, ok := networkKind.find(w, r, s.in(r))
	if !ok {
		return
	}
	// ASSUMPTION: updateNetwork is a PATCH like updateSecurityGroup, whose
	// contract says only the fields present change and an empty value clears
	// one. updateNetwork's own contract does not say how to clear a field.
	n.displayName = patchString(n.displayName, body.DisplayName)
	n.description = patchString(n.description, body.Description)
	if body.Tags != nil {
		n.tags = *body.Tags
		if len(n.tags) == 0 {
			n.tags = nil
		}
	}
	// v1.3.0-rc.3 writes the three fields in place, in any state, and starts
	// no workflow, so the network keeps its state: a `failed` one stays
	// `failed`.
	writeJSON(w, http.StatusOK, networkJSON(n))
}

func (s *state) deleteNetwork(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o := s.in(r)

	n, where, ok := networkKind.find(w, r, o)
	if !ok || !networkKind.admitDelete(w, where, n) {
		return
	}
	// A network with subnets in it is not deletable. A silent cascade would
	// hide an ordering bug in the provider's dependency graph until a real
	// landscape returned the 409.
	if held := subnetKind.under(o, n.id); len(held) > 0 {
		apiError(w, http.StatusConflict, "CONFLICT",
			"the network still holds subnet "+held[0].id)
		return
	}
	networkKind.tearDown(o, where, n)
	w.WriteHeader(http.StatusNoContent)
}

func (s *state) listSubnets(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	subnets, ok := subnetKind.list(w, r, s.in(r))
	if !ok {
		return
	}
	items := make([]any, 0, len(subnets))
	for _, sub := range subnets {
		items = append(items, subnetJSON(sub))
	}
	subnetList.serve(w, r, items)
}

func (s *state) createSubnet(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID              *string   `json:"id"`
		AddressPool     string    `json:"addressPool"`
		PrefixLength    int       `json:"prefixLength"`
		AllocateGateway *bool     `json:"allocateGateway"`
		ExcludeRanges   *[]string `json:"excludeRanges"`
		DisplayName     *string   `json:"displayName"`
		Description     *string   `json:"description"`
		// The contract takes tags on a subnet's create. The provider sends
		// only its create token there, and reads it back after a lost answer.
		Tags *map[string]string `json:"tags"`
	}
	if !decode(w, r, &body) {
		return
	}
	if body.AddressPool == "" {
		apiError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "addressPool is required")
		return
	}
	// Against the named pool's own bounds, not a global 1..32: a provider that
	// never reads minPrefixLength has to fail here.
	pool, ok := findAddressPool(body.AddressPool)
	if !ok {
		apiError(w, http.StatusUnprocessableEntity, "INVALID_ARGUMENT",
			"addressPool does not resolve in this region: "+body.AddressPool)
		return
	}
	// Asserted rather than ignored: these come from the literal catalog above,
	// so a failure here is a typo in this file and should say so loudly rather
	// than silently validating against zero.
	minLen, minOK := pool["minPrefixLength"].(int)
	maxLen, maxOK := pool["maxPrefixLength"].(int)
	prefixes, prefixesOK := pool["prefixes"].([]string)
	if !minOK || !maxOK || !prefixesOK || len(prefixes) == 0 {
		apiError(w, http.StatusInternalServerError, "INTERNAL",
			fmt.Sprintf("the fake's pool %v has no integer prefix bounds or no prefix", pool["id"]))
		return
	}
	if body.PrefixLength < minLen || body.PrefixLength > maxLen {
		apiError(w, http.StatusUnprocessableEntity, "INVALID_ARGUMENT",
			fmt.Sprintf("prefixLength %d is outside pool %v's range of %d to %d",
				body.PrefixLength, pool["id"], minLen, maxLen))
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	o, network, ok := subnetKind.admit(s, w, r)
	if !ok {
		return
	}
	id, ok := subnetKind.claim(s, w, o, network, body.ID)
	if !ok {
		return
	}

	s.subnetSeq++
	// ASSUMPTION: `excludeRanges` is not echoed on read, so nothing from
	// body.ExcludeRanges is stored. The contract declares the field on the
	// create request and on the resource and does not say the values
	// round-trip; a provider that overwrote its state from the response would
	// drop the user's ranges and propose recreating the subnet.
	sub := &fakeSubnet{
		id: id, network: network, addressPool: body.AddressPool,
		prefixLength: body.PrefixLength,
		displayName:  body.DisplayName, description: body.Description,
		tags: derefTags(body.Tags),
		// The contract says the CIDR is "empty until `state=active`". A
		// provider that writes the create response straight to state records
		// an empty `cidr` and never corrects it.
		state: "creating",
	}
	// ASSUMPTION: `excludeRanges` is on the resource, so a read returns it, but
	// the contract does not say in which of its three spellings. It comes back
	// as `a-b` ranges, the reading that breaks a provider comparing strings.
	if body.ExcludeRanges != nil {
		sub.excludeRanges = asRanges(*body.ExcludeRanges)
	}
	// "Server-allocated from the AddressPool": a block inside the pool's
	// prefix, so a subnet never reads back outside the pool it came from.
	cidr, last := allocateInPool(prefixes[0], s.subnetSeq, body.PrefixLength)
	sub.cidr = cidr
	// "Server-populated when the parent AddressPool is routable and the create
	// request set `allocateGateway: true`", the last usable host address.
	if body.AllocateGateway != nil && *body.AllocateGateway && poolIsRoutable(body.AddressPool) {
		sub.gateway = last
	}
	subnetKind.put(o, network, id, sub)

	writeJSON(w, http.StatusCreated, subnetJSON(sub))
}

func (s *state) getSubnet(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sub, ok := subnetKind.read(w, r, s.in(r)); ok {
		writeJSON(w, http.StatusOK, subnetJSON(sub))
	}
}

func (s *state) deleteSubnet(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o := s.in(r)

	sub, where, ok := subnetKind.find(w, r, o)
	if !ok || !subnetKind.admitDelete(w, where, sub) {
		return
	}
	subnetKind.tearDown(o, where, sub)
	w.WriteHeader(http.StatusNoContent)
}

func (s *state) listVPCs(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o := s.in(r)
	vpcs, _ := vpcKind.list(w, r, o)

	// The owner filters match the owner's uid, as the contract says.
	items := make([]any, 0, len(vpcs))
	for _, v := range vpcs {
		if c := r.URL.Query().Get("clusterId"); c != "" && (v.ownerKind != "cluster" || v.ownerUID != c) {
			continue
		}
		if g := r.URL.Query().Get("instanceGroupId"); g != "" && (v.ownerKind != "instance_group" || v.ownerUID != g) {
			continue
		}
		items = append(items, vpcJSON(o.scope, v))
	}
	vpcList.serve(w, r, items)
}

func (s *state) getVPC(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o := s.in(r)
	if v, ok := vpcKind.read(w, r, o); ok {
		writeJSON(w, http.StatusOK, vpcJSON(o.scope, v))
	}
}

// orEmpty is a stored optional string as the platform reads it back.
func orEmpty(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func networkJSON(n *fakeNetwork) map[string]any {
	out := map[string]any{
		"id":       n.id,
		"type":     n.kind,
		"state":    n.state,
		"selfLink": "/v1/networking/networks/" + n.id,
		// A server-owned tag alongside whatever the tenant set. The provider
		// must not fold this into `tags`, or every plan proposes removing it.
		"systemTags": map[string]string{"managed-by": "fakeapi"},
	}
	failureDetail(n.state, out)
	if n.clusterID != "" {
		out["clusterId"] = n.clusterID
	}
	// The platform answers an unset name or description as "", and unset tags
	// as {}, not by leaving them out (the k0rdent API v1.3.0-dev.3 on kind). A
	// provider that read those back as values fails its first apply on a
	// configuration that set none of them.
	out["displayName"] = orEmpty(n.displayName)
	out["description"] = orEmpty(n.description)
	out["tags"] = map[string]string{}
	if n.tags != nil {
		out["tags"] = n.tags
	}

	switch n.kind {
	case "evpn":
		out["evpn"] = map[string]any{"type": n.serviceType}
		out["vlan"] = map[string]any{"id": n.vlanID}
		if n.mtu != nil {
			out["mtu"] = *n.mtu
		}
		if n.dhcpEnabled != nil {
			out["dhcpEnabled"] = *n.dhcpEnabled
		}
		if n.dnsServers != nil {
			out["dnsServers"] = *n.dnsServers
		}
	case "infiniband":
		ib := map[string]any{"pkey": n.pkey}
		if n.partition != "" {
			ib["partition"] = n.partition
		}
		if n.index0 != nil {
			ib["index0"] = *n.index0
		}
		if n.ipOverIB != nil {
			ib["ipOverIb"] = *n.ipOverIB
		}
		out["infiniband"] = ib
	case "nvlink":
		if n.partition != "" {
			out["nvlink"] = map[string]any{"partition": n.partition}
		}
	}
	return out
}

func subnetJSON(sub *fakeSubnet) map[string]any {
	out := map[string]any{
		"id":           sub.id,
		"addressPool":  sub.addressPool,
		"prefixLength": sub.prefixLength,
		"ipVersion":    "ipv4",
		"state":        sub.state,
		"network":      "/v1/networking/networks/" + sub.network,
		"selfLink":     "/v1/networking/networks/" + sub.network + "/subnets/" + sub.id,
	}
	failureDetail(sub.state, out)
	if sub.clusterID != "" {
		out["clusterId"] = sub.clusterID
	}
	if sub.state == "active" {
		out["cidr"] = sub.cidr
		if sub.gateway != "" {
			out["gateway"] = sub.gateway
		}
	}
	// Required since v1.3.0-dev.4: `[]` when none are excluded.
	out["excludeRanges"] = []string{}
	if sub.excludeRanges != nil {
		out["excludeRanges"] = sub.excludeRanges
	}
	// "" when unset, as networkJSON. Kind answers unset tags and system tags
	// as {}, and a create that sends tags gets them back.
	out["displayName"] = orEmpty(sub.displayName)
	out["description"] = orEmpty(sub.description)
	if sub.tags != nil {
		out["tags"] = sub.tags
	} else {
		out["tags"] = map[string]string{}
	}
	out["systemTags"] = map[string]string{}
	return out
}

// derefTags is a create's tags, nil when it sent none.
func derefTags(p *map[string]string) map[string]string {
	if p == nil {
		return nil
	}
	return *p
}

// asRanges respells each exclude entry as `first-last`.
func asRanges(items []string) []string {
	out := make([]string, 0, len(items))
	for _, item := range items {
		switch p, err := netip.ParsePrefix(item); {
		case err == nil:
			p = p.Masked()
			last := p.Addr()
			for next := last.Next(); next.IsValid() && p.Contains(next); next = next.Next() {
				last = next
			}
			out = append(out, p.Addr().String()+"-"+last.String())
		case !strings.Contains(item, "-"):
			out = append(out, item+"-"+item)
		default:
			out = append(out, item)
		}
	}
	return out
}

func vpcJSON(sc scope, v *fakeVPC) map[string]any {
	return map[string]any{
		"id":        v.id,
		"uid":       v.uid,
		"backend":   v.backend,
		"ownerKind": v.ownerKind,
		"ownerId":   v.ownerUID,
		// Never empty: clearing the binding reinstates the DEFAULT group, which
		// is why the provider documents this list as always populated.
		"securityGroups": v.securityGroups,
		"state":          v.state,
		// getVPC's path, which is the contract's example and remoteVpc's shape.
		"selfLink":  "/v1/regions/" + sc.region + "/projects/" + sc.project + "/networking/vpcs/" + v.id,
		"ufoCrName": strings.ReplaceAll(v.id, "-", "_"),
	}
}

// The address pool catalog, deliberately more than one page. Every pool
// answers `excludeRanges`, which v1.3.0-dev.4 makes required, as `[]` when it
// excludes nothing.
//
// ASSUMPTION: nothing in the contract says a catalog is small enough to arrive
// whole. A fake that returned everything in one page would let a provider that
// reads the first page and stops pass, and that provider then silently loses
// pools on any landscape with more of them. The provider asks for the offset
// contract's largest page, 500, and gets exactly that, so the three named pools
// are followed by 498 generated ones and the 501st is only on the second page.
var addressPools = append([]map[string]any{
	{
		"id": "tenant", "displayName": "Tenant subnets", "ipVersion": "ipv4",
		"prefixes": []string{"10.0.0.0/8"}, "minPrefixLength": 16, "maxPrefixLength": 28,
		"routable": true, "state": "active", "excludeRanges": []string{},
		"selfLink": "/v1/regions/us-texas-1/networking/address-pools/tenant",
	},
	{
		"id": "cluster-local", "displayName": "Cluster-local", "ipVersion": "ipv4",
		"prefixes": []string{"192.168.0.0/16"}, "minPrefixLength": 20, "maxPrefixLength": 30,
		"routable": false, "state": "active", "excludeRanges": []string{},
		"selfLink": "/v1/regions/us-texas-1/networking/address-pools/cluster-local",
	},
	{
		"id": "vip", "displayName": "Control-plane VIPs", "ipVersion": "ipv4",
		"prefixes": []string{"198.51.100.0/24"}, "minPrefixLength": 32, "maxPrefixLength": 32,
		"routable": true, "state": "active",
		"excludeRanges": []string{"198.51.100.1"},
		"selfLink":      "/v1/regions/us-texas-1/networking/address-pools/vip",
	},
}, generatedAddressPools(498)...)

// generatedAddressPools makes n unroutable pools that exist only to fill pages.
// Their ids sort between the named pools', so the provider's sorted list ends
// on `vip` only when it has read every page.
func generatedAddressPools(n int) []map[string]any {
	out := make([]map[string]any, 0, n)
	for i := range n {
		id := fmt.Sprintf("filler-%03d", i)
		out = append(out, map[string]any{
			"id": id, "displayName": "Filler " + strconv.Itoa(i), "ipVersion": "ipv4",
			"prefixes":        []string{fmt.Sprintf("100.%d.%d.0/24", 64+i/256, i%256)},
			"minPrefixLength": 24, "maxPrefixLength": 28,
			"routable": false, "state": "active", "excludeRanges": []string{},
			"selfLink": "/v1/regions/us-texas-1/networking/address-pools/" + id,
		})
	}
	return out
}

func (s *state) listAddressPools(w http.ResponseWriter, r *http.Request) {
	all := make([]any, 0, len(addressPools))
	for _, pool := range addressPools {
		all = append(all, pool)
	}
	addressPoolList.serve(w, r, all)
}

// findAddressPool resolves the selfLink or the bare id a subnet was created
// with. Both forms are accepted because the contract types the field as a URI
// and the platform has been observed answering with a relative path.
func findAddressPool(ref string) (map[string]any, bool) {
	for _, pool := range addressPools {
		if pool["selfLink"] == ref || pool["id"] == ref {
			return pool, true
		}
	}
	return nil, false
}

func (s *state) getAddressPool(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	for _, pool := range addressPools {
		if pool["id"] == id {
			writeJSON(w, http.StatusOK, pool)
			return
		}
	}
	apiError(w, http.StatusNotFound, "NOT_FOUND", "no such address pool")
}

// A peering create whose id carries this loses the race for its VPC's row
// lock the first time it is sent, and answers the 503 the contract documents:
// nothing is written and the create is safe to send again. Every later send
// of the same id goes through. A real lost lock needs another writer on the
// same VPC at the same moment, most often a run deleting the cluster that owns
// it, which a fake cannot stage, so the trigger is a marker in the id.
const vpcLockLostMarker = "fake-vpc-lock-lost"

// loseVPCLockOnce reports whether this create is the first for its key, and
// records it. The caller holds s.mu.
func (s *state) loseVPCLockOnce(sc scope, key string) bool {
	full := sc.region + "/" + sc.project + "/" + key
	if s.lockLost[full] {
		return false
	}
	if s.lockLost == nil {
		s.lockLost = map[string]bool{}
	}
	s.lockLost[full] = true
	return true
}

type fakePeering struct {
	id          string
	vpc         string
	remoteVPC   string
	displayName *string
	description *string
	tags        map[string]string
	state       string
	reads       int
}

func (s *state) listVPCPeerings(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	peerings, ok := peeringKind.list(w, r, s.in(r))
	if !ok {
		return
	}
	items := make([]any, 0, len(peerings))
	for _, p := range peerings {
		items = append(items, peeringJSON(p))
	}
	peeringList.serve(w, r, items)
}

func (s *state) createVPCPeering(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID          *string            `json:"id"`
		RemoteVpc   string             `json:"remoteVpc"`
		DisplayName *string            `json:"displayName"`
		Description *string            `json:"description"`
		Tags        *map[string]string `json:"tags"`
		// Declared so the fake can refuse them. The contract sets
		// `additionalProperties: false` and says every server-owned field is
		// rejected rather than ignored, so a provider that posted back a
		// resource it had read would get a 400. Modelled because a fake that
		// quietly accepted the round trip would hide that.
		SelfLink  *string `json:"selfLink"`
		State     *string `json:"state"`
		LocalVpc  *string `json:"localVpc"`
		ProjectID *string `json:"projectId"`
		Region    *string `json:"region"`
	}
	if !decode(w, r, &body) {
		return
	}
	// In a fixed order, so a body carrying several is refused for the
	// same one every time.
	for _, f := range []struct {
		name    string
		present bool
	}{
		{"selfLink", body.SelfLink != nil}, {"state", body.State != nil},
		{"localVpc", body.LocalVpc != nil}, {"projectId", body.ProjectID != nil},
		{"region", body.Region != nil},
	} {
		if name := f.name; f.present {
			apiError(w, http.StatusBadRequest, "INVALID_ARGUMENT",
				name+" is server-owned and is rejected on create, not ignored")
			return
		}
	}
	if body.RemoteVpc == "" {
		apiError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "remoteVpc is required")
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	o, vpc, ok := peeringKind.admit(s, w, r)
	if !ok {
		return
	}
	// The contract's shape, in the local VPC's region. A bare id isn't a
	// URI the platform can resolve.
	if !strings.HasPrefix(vpcPath(body.RemoteVpc), "/v1/regions/"+o.scope.region+"/projects/") ||
		!strings.Contains(vpcPath(body.RemoteVpc), "/networking/vpcs/") {
		apiError(w, http.StatusUnprocessableEntity, "INVALID_ARGUMENT",
			"remoteVpc must be /v1/regions/{region}/projects/{project}/networking/vpcs/{id}, in this VPC's region")
		return
	}
	// The whole project-scoped path, not the id: VPC ids repeat across
	// projects, and the remote may be in another one.
	if vpcPath(body.RemoteVpc) == "/v1/regions/"+o.scope.region+"/projects/"+o.scope.project+"/networking/vpcs/"+vpc {
		apiError(w, http.StatusUnprocessableEntity, "INVALID_ARGUMENT",
			"remoteVpc must not be the local VPC")
		return
	}

	if body.ID != nil && strings.Contains(*body.ID, vpcLockLostMarker) && s.loseVPCLockOnce(o.scope, vpc+"/"+*body.ID) {
		// The contract's body is deliberately generic and carries no
		// Retry-After, so neither does this one.
		errorEnvelope(w, http.StatusServiceUnavailable, "", "service temporarily unavailable", nil)
		return
	}

	id, ok := peeringKind.claim(s, w, o, vpc, body.ID)
	if !ok {
		return
	}

	p := &fakePeering{
		id: id, vpc: vpc, remoteVPC: body.RemoteVpc,
		displayName: body.DisplayName, description: body.Description,
		// ASSUMPTION: this side reaches `active` on its own, with no counterpart
		// anywhere. That is what makes the half-open case invisible, and it is
		// what the contract describes: `active` means this side's declaration is
		// applied, never that traffic flows. A fake that withheld `active` until
		// both sides existed would let a provider that waits for reachability
		// pass, and that provider would hang forever on a real landscape.
		state: "creating",
	}
	if body.Tags != nil {
		p.tags = *body.Tags
	}
	peeringKind.put(o, vpc, id, p)

	writeJSON(w, http.StatusCreated, peeringJSON(p))
}

// vpcPath is a VPC reference as a path, with any scheme and host taken off
// the front, the way a gateway accepts what it issues.
func vpcPath(ref string) string {
	if i := strings.Index(ref, "://"); i >= 0 {
		if j := strings.Index(ref[i+3:], "/"); j >= 0 {
			ref = ref[i+3+j:]
		}
	}
	return strings.TrimSuffix(ref, "/")
}

func (s *state) getVPCPeering(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p, ok := peeringKind.read(w, r, s.in(r)); ok {
		writeJSON(w, http.StatusOK, peeringJSON(p))
	}
}

func (s *state) deleteVPCPeering(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o := s.in(r)

	p, where, ok := peeringKind.find(w, r, o)
	if !ok || !peeringKind.admitDelete(w, where, p) {
		return
	}
	peeringKind.tearDown(o, where, p)
	w.WriteHeader(http.StatusNoContent)
}

func peeringJSON(p *fakePeering) map[string]any {
	out := map[string]any{
		"id":        p.id,
		"remoteVpc": p.remoteVPC,
		"localVpc":  "/v1/networking/vpcs/" + p.vpc,
		"state":     p.state,
		"selfLink":  "/v1/networking/vpcs/" + p.vpc + "/peerings/" + p.id,
		// Server-owned, and deliberately not the same key as `tags`: a provider
		// that folded these in would propose removing them on every plan.
		"systemTags": map[string]string{"managed-by": "fakeapi"},
	}
	failureDetail(p.state, out)
	// ASSUMPTION: kind has no VPC to peer from, so a real peering's read is
	// unseen. Networks and subnets from the same service answer an unset name
	// or description as "" and unset tags as {}, so a peering does too here.
	out["displayName"] = orEmpty(p.displayName)
	out["description"] = orEmpty(p.description)
	out["tags"] = map[string]string{}
	if p.tags != nil {
		out["tags"] = p.tags
	}
	return out
}

// setVPCSecurityGroups replaces the whole binding.
//
// ASSUMPTION, and the one that matters: an empty list reinstates the default
// group rather than clearing the binding, and the response reports that group
// explicitly. The contract is unambiguous about it, and it is modelled because
// the damaging reading is the intuitive one: a provider whose destroy sends
// `[]` expecting an empty result would record an empty list in state and then
// disagree with the platform forever.
func (s *state) setVPCSecurityGroups(w http.ResponseWriter, r *http.Request) {
	var body struct {
		SecurityGroups *[]string `json:"securityGroups"`
	}
	if !decode(w, r, &body) {
		return
	}
	if body.SecurityGroups == nil {
		apiError(w, http.StatusUnprocessableEntity, "INVALID_ARGUMENT",
			"securityGroups is required; omitting it is not read as 'change nothing'")
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	o := s.in(r)

	target, _, ok := vpcKind.find(w, r, o)
	if !ok {
		return
	}
	// The real reason a binding is refused, and the one the upstream message
	// does not name. The provider attaches a warning saying which backend to
	// check; this is what makes that path reachable.
	if target.backend != "nico" {
		apiError(w, http.StatusConflict, "CONFLICT",
			"the VPC is not on a fabric backend that carries security groups")
		return
	}

	// "A second binding change while this VPC is still `updating` is refused
	// with `409` rather than queued." Without it a provider that wrote twice
	// without waiting for the first render would pass here and fail on a
	// platform.
	if target.state == "updating" {
		inUse(w, "the VPC already has a binding change in flight, so wait for it to settle and retry")
		return
	}

	// Recorded on v1.3.0-rc.3: every unknown id at once, then another VPC's
	// default, both with no reason and no details.
	var unknown []string
	for _, group := range *body.SecurityGroups {
		if _, ok := o.groupsSec[at{id: group}]; !ok {
			unknown = append(unknown, group)
		}
	}
	if len(unknown) > 0 {
		errorEnvelope(w, http.StatusUnprocessableEntity, "",
			"validation: unknown security group(s): "+strings.Join(unknown, ", "), nil)
		return
	}
	for _, group := range *body.SecurityGroups {
		// A default group binds only to its own VPC.
		if held := o.groupsSec[at{id: group}]; held.ownerKind == "vpc" && held.ownerID != target.uid {
			errorEnvelope(w, http.StatusUnprocessableEntity, "", fmt.Sprintf(
				"validation: security group %q is the platform-managed default of a different VPC and "+
					"cannot be attached to vpc %q; send an empty list to reinstate this VPC's own default",
				group, target.id), nil)
			return
		}
	}

	target.securityGroups = *body.SecurityGroups
	if len(target.securityGroups) == 0 {
		// This VPC's own default, by id, which the response names.
		target.securityGroups = []string{target.defaultGroup}
	}
	target.state, target.reads = "updating", 0

	// 202, not 200: accepted, and rendered onto the owner's bundles after.
	writeJSON(w, http.StatusAccepted, vpcJSON(o.scope, target))
}

// allocateInPool returns the seq-th block of the given length inside a pool's
// prefix, wrapping when the prefix runs out, and that block's last usable
// host address. A /31 or /32 has no host to spare, so no address.
func allocateInPool(prefix string, seq, length int) (cidr, last string) {
	base := netip.MustParsePrefix(prefix).Masked()
	start := base.Addr().As4()
	first := uint64(start[0])<<24 | uint64(start[1])<<16 | uint64(start[2])<<8 | uint64(start[3])
	size := uint64(1) << (32 - length)
	blocks := uint64(1) << (length - base.Bits())
	at := first + (uint64(seq-1)%blocks)*size

	addr := func(v uint64) netip.Addr {
		return netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)})
	}
	cidr = netip.PrefixFrom(addr(at), length).String()
	if length < 31 {
		last = addr(at + size - 2).String()
	}
	return cidr, last
}

// poolIsRoutable reads `routable` off the catalog entry a pool URI names.
func poolIsRoutable(uri string) bool {
	for _, pool := range addressPools {
		if pool["selfLink"] == uri {
			routable, ok := pool["routable"].(bool)
			return ok && routable
		}
	}
	return false
}
