package main

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
)

// Security groups and the tenant quota read.
//
// Same rule: the fake models the reading that breaks a provider. Two here.

type fakeRule struct {
	Name                 *string `json:"name,omitempty"`
	Action               string  `json:"action"`
	Protocol             string  `json:"protocol"`
	SourcePrefix         string  `json:"sourcePrefix"`
	DestinationPrefix    string  `json:"destinationPrefix"`
	SourcePortRange      *string `json:"sourcePortRange,omitempty"`
	DestinationPortRange *string `json:"destinationPortRange,omitempty"`
}

type fakeSecurityGroup struct {
	id          string
	displayName *string
	description *string
	tags        map[string]any
	ingress     []fakeRule
	egress      []fakeRule
	ownerKind   string // "vpc" when the platform owns it and the tenant may not write
	ownerID     string // the owning VPC's uid, beside ownerKind
	state       string
	reads       int
}

func (s *state) listSecurityGroups(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	groups, _ := securityGroupKind.list(w, r, s.in(r))

	// `state` is the one filter this operation documents.
	want := r.URL.Query().Get("state")
	items := make([]any, 0, len(groups))
	for _, g := range groups {
		if want != "" && g.state != want {
			continue
		}
		items = append(items, securityGroupJSON(g))
	}
	securityGroupList.serve(w, r, items)
}

func (s *state) createSecurityGroup(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID          *string         `json:"id"`
		DisplayName *string         `json:"displayName"`
		Description *string         `json:"description"`
		Tags        *map[string]any `json:"tags"`
		Rules       *struct {
			Ingress []fakeRule `json:"ingress"`
			Egress  []fakeRule `json:"egress"`
		} `json:"rules"`
	}
	if !decode(w, r, &body) {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	o, _, ok := securityGroupKind.admit(s, w, r)
	if !ok {
		return
	}
	id, ok := securityGroupKind.claim(s, w, o, "", body.ID)
	if !ok {
		return
	}

	g := &fakeSecurityGroup{id: id, displayName: body.DisplayName, description: body.Description, state: "creating"}
	if body.Tags != nil {
		g.tags = *body.Tags
	}
	if body.Rules != nil {
		g.ingress, g.egress = body.Rules.Ingress, body.Rules.Egress
		normalizePorts(g.ingress)
		normalizePorts(g.egress)
	}
	if err := validateRules(g); err != "" {
		apiError(w, http.StatusUnprocessableEntity, "INVALID_ARGUMENT", err)
		return
	}
	securityGroupKind.put(o, "", id, g)

	writeJSON(w, http.StatusCreated, securityGroupJSON(g))
}

func (s *state) getSecurityGroup(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o := s.in(r)

	if _, held := o.groupsSec[at{id: "default"}]; !held && r.PathValue("id") == "default" {
		// Every VPC starts with its own default group, marked `ownerKind: vpc`.
		writeJSON(w, http.StatusOK, map[string]any{
			"id": "default", "ownerKind": "vpc", "state": "active", "ruleCount": 0,
			"selfLink": "/v1/networking/security-groups/default",
			"rules":    map[string]any{"ingress": []any{}, "egress": []any{}},
		})
		return
	}
	if g, ok := securityGroupKind.read(w, r, o); ok {
		writeJSON(w, http.StatusOK, securityGroupJSON(g))
	}
}

func (s *state) updateSecurityGroup(w http.ResponseWriter, r *http.Request) {
	var body struct {
		DisplayName *string         `json:"displayName"`
		Description *string         `json:"description"`
		Tags        *map[string]any `json:"tags"`
		Rules       *struct {
			Ingress []fakeRule `json:"ingress"`
			Egress  []fakeRule `json:"egress"`
		} `json:"rules"`
	}
	if !decode(w, r, &body) {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	g, _, ok := securityGroupKind.find(w, r, s.in(r))
	if !ok {
		return
	}
	// "Only the fields present in the body are changed." An empty string clears
	// a field and an empty object clears every tag.
	g.displayName = patchString(g.displayName, body.DisplayName)
	g.description = patchString(g.description, body.Description)
	if body.Tags != nil {
		g.tags = *body.Tags
		if len(g.tags) == 0 {
			g.tags = nil
		}
	}
	// Omitting `rules` leaves the set alone; sending it replaces the set
	// wholesale. Modelled because a provider that sent a patch of one rule
	// would silently drop the others here, exactly as it would upstream.
	if body.Rules != nil {
		g.ingress, g.egress = body.Rules.Ingress, body.Rules.Egress
		normalizePorts(g.ingress)
		normalizePorts(g.egress)
		if err := validateRules(g); err != "" {
			apiError(w, http.StatusUnprocessableEntity, "INVALID_ARGUMENT", err)
			return
		}
	}
	g.state, g.reads = "updating", 0
	writeJSON(w, http.StatusOK, securityGroupJSON(g))
}

func (s *state) deleteSecurityGroup(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o := s.in(r)

	g, where, ok := securityGroupKind.find(w, r, o)
	if !ok {
		return
	}
	id := g.id
	// Measured on the k0rdent API v1.3.0-dev.3: a group a cluster holds is not
	// deleted out from under it. The contract's "a group deleted out from
	// under the binding is omitted rather than echoed" never happens through
	// this call, which answers 409 naming the holder.
	//
	// A VPC holds a group too. v1.3.0-dev.4 refused the delete of a group a
	// VPC carried with the body below, the dash included, naming the VPC.
	// ASSUMPTION: VPCs and clusters held together are named in one message,
	// VPCs first, as the contract says every holder is named at once.
	var holders []string
	for _, v := range o.vpcsHolding(id) {
		holders = append(holders, "vpc "+v.id)
	}
	for _, c := range o.clustersHolding(id) {
		holders = append(holders, "cluster "+c.id)
	}
	if len(holders) > 0 {
		recordedConflict(w, fmt.Sprintf("conflict: security group %q is attached to: %s \u2014 detach it before deleting",
			id, strings.Join(holders, ", ")), "CONFLICT_IN_USE")
		return
	}
	// After the holders, which the contract lists first. ASSUMPTION: a group
	// both held and unsettled is refused for being held.
	if !securityGroupKind.admitDelete(w, where, g) {
		return
	}
	securityGroupKind.tearDown(o, where, g)
	w.WriteHeader(http.StatusNoContent)
}

// maxClusterGroups is the contract's `maxItems` on a cluster's securityGroups.
const maxClusterGroups = 16

// admitsGroups answers the 422s a cluster's securityGroups can draw, on a
// create or an update, and reports whether the list may stand. The messages
// are the ones the k0rdent API v1.3.0-dev.3 answers where they were measured.
// The caller holds s.mu.
func (o *objects) admitsGroups(w http.ResponseWriter, ct fakeClusterType, groups []string) bool {
	refuse := func(message string) bool {
		apiError(w, http.StatusUnprocessableEntity, "INVALID_ARGUMENT", "validation: "+message)
		return false
	}
	if len(groups) > maxClusterGroups {
		return refuse(fmt.Sprintf("securityGroups: at most %d, got %d", maxClusterGroups, len(groups)))
	}
	seen := map[string]bool{}
	var unknown []string
	for i, g := range groups {
		if seen[g] {
			return refuse(fmt.Sprintf("securityGroups[%d]: id %q is duplicated", i, g))
		}
		seen[g] = true
		held, ok := o.groupsSec[at{id: g}]
		switch {
		case !ok:
			unknown = append(unknown, g)
		case held.ownerKind == "vpc":
			// The contract: a VPC's default group "cannot be named here".
			return refuse(fmt.Sprintf("securityGroups[%d]: %q is a VPC's default group and "+
				"attaches only to that VPC", i, g))
		}
	}
	if len(unknown) > 0 {
		// Every unresolvable id at once, as the contract says.
		return refuse("unknown security group(s): " + strings.Join(unknown, ", "))
	}
	if len(groups) > 0 && ct.noGroupInterface {
		return refuse("securityGroups: no node pool has an interface to apply them to; at least one " +
			"pool needs an ethernet whose connectToNetwork names a network on a nico-backed VPC")
	}
	return true
}

// clusterGroups is a cluster's binding as the read renders it: always a list,
// `[]` when none.
func clusterGroups(c *cluster) []any {
	out := make([]any, 0, len(c.securityGroups))
	for _, g := range c.securityGroups {
		out = append(out, g)
	}
	return out
}

// vpcsHolding names the VPCs whose binding includes a group. The caller holds
// s.mu.
func (o *objects) vpcsHolding(group string) []*fakeVPC {
	var holders []*fakeVPC
	for _, v := range vpcKind.under(o, "") {
		if slices.Contains(v.securityGroups, group) {
			holders = append(holders, v)
		}
	}
	return holders
}

// clustersHolding names the live clusters whose own binding includes a group.
// The caller holds s.mu.
func (o *objects) clustersHolding(group string) []*cluster {
	var holders []*cluster
	for _, c := range clusterKind.under(o, "") {
		if slices.Contains(c.securityGroups, group) {
			holders = append(holders, c)
		}
	}
	return holders
}

// listSecurityGroupAttachments names what holds a group: the VPCs that carry
// it, then the clusters whose own binding does. Those are the two a refused
// delete can come from in this fake, and the provider reads this to say so.
func (s *state) listSecurityGroupAttachments(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o := s.in(r)

	g, _, ok := securityGroupKind.find(w, r, o)
	if !ok {
		return
	}
	items := []any{}
	for _, v := range o.vpcsHolding(g.id) {
		items = append(items, map[string]any{"kind": "vpc", "id": v.id, "state": v.state})
	}
	for _, holder := range o.clustersHolding(g.id) {
		items = append(items, map[string]any{"kind": "cluster", "id": holder.id, "state": holder.state})
	}
	attachmentList.serve(w, r, items)
}

// validateRules is the 422 the server answers, modelled so the provider's
// plan-time check has something to be checked against.
func validateRules(g *fakeSecurityGroup) string {
	for _, list := range [][]fakeRule{g.ingress, g.egress} {
		for _, rule := range list {
			switch rule.Protocol {
			case "tcp", "udp":
			case "icmp", "icmp6", "any":
				if rule.SourcePortRange != nil || rule.DestinationPortRange != nil {
					return "a port range is not valid on protocol " + rule.Protocol
				}
			default:
				return "protocol must be tcp, udp, icmp, icmp6 or any, got " + rule.Protocol
			}
			switch rule.Action {
			case "permit", "deny":
			default:
				return "action must be permit or deny, got " + rule.Action
			}
		}
	}
	if len(g.ingress)+len(g.egress) > 256 {
		return "a security group holds at most 256 rules"
	}
	return ""
}

func securityGroupJSON(g *fakeSecurityGroup) map[string]any {
	// Never nil, either list: the contract declares both as present, and a
	// provider that tolerated a missing one here would fault on a real server.
	ingress, egress := g.ingress, g.egress
	if ingress == nil {
		ingress = []fakeRule{}
	}
	if egress == nil {
		egress = []fakeRule{}
	}

	out := map[string]any{
		"id":        g.id,
		"state":     g.state,
		"selfLink":  "/v1/networking/security-groups/" + g.id,
		"ruleCount": len(ingress) + len(egress),
		"rules":     map[string]any{"ingress": ingress, "egress": egress},
	}
	failureDetail(g.state, out)
	if g.ownerKind != "" {
		out["ownerKind"] = g.ownerKind
		out["ownerId"] = g.ownerID
	}
	if g.displayName != nil {
		out["displayName"] = *g.displayName
	}
	if g.description != nil {
		out["description"] = *g.description
	}
	// Unset tags read back as {}, as on kind. The name and description are
	// left out there, so they are here.
	out["tags"] = map[string]any{}
	if g.tags != nil {
		out["tags"] = g.tags
	}
	return out
}

// quotaView is the view both quota reads answer. `gpu_count` is limited
// nowhere but enforced, so it has usage and no headroom. `cpu_cores` is capped
// and not enforced, so its usage is unmeasured and absent: a provider that read
// that as 0 would report headroom nobody vouches for. `node_count` is capped at
// the organization and, on a project with its own assignment, at the project,
// where the project's smaller headroom binds.
func quotaView(project string) map[string]any {
	tenant := func(extra map[string]any) map[string]any {
		out := map[string]any{"scope": "tenant", "targetId": "org-demo", "overLimit": false}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}
	node := []any{tenant(map[string]any{"limit": 32, "used": 6, "headroom": 26})}
	nodeHeadroom, binding := 26, "tenant"
	if project != "" {
		node = append(node, map[string]any{
			"scope": "project", "targetId": project, "limit": 8, "used": 6, "headroom": 2, "overLimit": false,
		})
		nodeHeadroom, binding = 2, "project"
	}
	return map[string]any{
		"dimensions": []any{
			map[string]any{
				"dimension": "gpu_count", "unit": "count", "enforced": true, "overLimit": false,
				"used": 8, "scopes": []any{tenant(map[string]any{"used": 8})},
			},
			map[string]any{
				"dimension": "node_count", "unit": "count", "enforced": true, "overLimit": false,
				"limit": 32, "used": 6, "remaining": 26,
				"headroom": nodeHeadroom, "bindingScope": binding, "scopes": node,
			},
			map[string]any{
				"dimension": "cpu_cores", "unit": "cores", "enforced": false, "overLimit": false,
				"limit": 512, "scopes": []any{tenant(map[string]any{"limit": 512})},
			},
		},
		"profile": map[string]any{"displayName": "Standard", "id": "standard"},
	}
}

func (s *state) getTenantQuota(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, quotaView(""))
}

func (s *state) getProjectQuota(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, quotaView(r.PathValue("id")))
}

// normalizePorts stores "80-80" as "80": the contract says the two are
// equivalent and "the platform reports the normalized spelling it stored".
func normalizePorts(rules []fakeRule) {
	for i := range rules {
		for _, p := range []**string{&rules[i].SourcePortRange, &rules[i].DestinationPortRange} {
			if *p == nil {
				continue
			}
			if lo, hi, ok := strings.Cut(**p, "-"); ok && lo == hi {
				v := lo
				*p = &v
			}
		}
	}
}

// listSecurityGroupsEffective merges the owner's binding with each VPC's floor.
//
// ASSUMPTION: the owner's groups come first and the VPC's floor after, and a
// group bound to both appears once at the owner's position. That ordering is
// precedence, not presentation, and it is modelled because the intuitive
// reading (concatenate and dedupe in whatever order) puts a VPC's floor ahead
// of the owner's groups and silently inverts which rule wins.
func (s *state) listSecurityGroupsEffective(w http.ResponseWriter, r *http.Request) {
	kind := r.URL.Query().Get("objectKind")
	id := r.URL.Query().Get("objectId")
	if kind == "" || id == "" {
		apiError(w, http.StatusUnprocessableEntity, "INVALID_ARGUMENT",
			"objectKind and objectId are both required")
		return
	}
	switch kind {
	case "cluster", "instance_group", "vpc":
	default:
		apiError(w, http.StatusUnprocessableEntity, "INVALID_ARGUMENT",
			"objectKind must be cluster, instance_group or vpc, got "+kind)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	o := s.in(r)

	// The owner's own groups come first, then the VPC's, and a group bound to
	// both appears once at the owner's position. Only a cluster's binding is
	// modelled: the provider writes no instance group's.
	vpcs := []any{}
	for _, v := range vpcKind.under(o, "") {
		if kind == "vpc" && v.id != id {
			continue
		}
		if kind != "vpc" && v.owner != id {
			continue
		}
		groups := []any{}
		seen := map[string]bool{}
		add := func(g, source, sourceID string) {
			if seen[g] {
				return
			}
			seen[g] = true
			count := 0
			if held, ok := o.groupsSec[at{id: g}]; ok {
				count = len(held.ingress) + len(held.egress)
			}
			groups = append(groups, map[string]any{
				"id": g, "source": source, "sourceId": sourceID, "ruleCount": count,
				"uid": "00000000-0000-4000-8000-" + fmt.Sprintf("%012x", len(g)),
			})
		}
		if owner, ok := o.clusters[at{id: v.owner}]; ok && v.ownerKind == "cluster" {
			for _, g := range owner.securityGroups {
				add(g, "cluster", owner.id)
			}
		}
		for _, g := range v.securityGroups {
			add(g, "vpc", v.id)
		}
		vpcs = append(vpcs, map[string]any{
			"id":             v.id,
			"securityGroups": groups,
			"securityGroupRules": map[string]any{
				"ingress": []any{}, "egress": []any{},
			},
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"object": map[string]any{
			"id": id, "kind": kind,
			"uid": "00000000-0000-4000-8000-" + fmt.Sprintf("%012x", len(id)),
		},
		"vpcs": vpcs,
	})
}
