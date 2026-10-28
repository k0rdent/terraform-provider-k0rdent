package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// listOf and jsonOf read a decoded body, failing the test on a wrong shape.
func listOf(t *testing.T, v any) []any {
	t.Helper()
	l, ok := v.([]any)
	if !ok {
		t.Fatalf("%#v is not a list", v)
	}
	return l
}

func jsonOf(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// defaultGroupOf reads a VPC's default group id off its binding, which holds
// only that group until something else is bound. A test never writes the id
// out: upstream it's random.
func defaultGroupOf(t *testing.T, s *state, vpc string) string {
	t.Helper()
	code, out := call(t, s.getVPC, http.MethodGet, vpc, "")
	groups := listOf(t, out["securityGroups"])
	id, ok := "", len(groups) == 1
	if ok {
		id, ok = groups[0].(string)
	}
	if code != http.StatusOK || !ok {
		t.Fatalf("read %s: %d, securityGroups %v, want its one default group", vpc, code, groups)
	}
	return id
}

func clusterNamed(t *testing.T, s *state, id string) {
	t.Helper()
	if code, out := call(t, s.createCluster, http.MethodPost, "",
		`{"id":"`+id+`","clusterType":"/v1/regions/r/compute/cluster-types/gpu-training"}`); code != http.StatusCreated {
		t.Fatalf("create %s: %d %v", id, code, out)
	}
}

// The k0rdent API v1.3.0-rc.3's answer for a VPC's default group with two
// subnet prefixes, recorded on kind. Only the fields defaultGroup decides.
const rc3DefaultGroup = `{
  "displayName": "Default (vpc-57ces008g698sbe5hxh7wb90z4)",
  "description": "Platform-managed default security group for VPC vpc-57ces008g698sbe5hxh7wb90z4. Permits traffic between the VPC's subnet prefixes, in both directions, and nothing else. Not editable and cannot be deleted. To add rules, such as egress leaving the VPC, attach your own group to the cluster or instance group, or list it next to this group on the VPC: a VPC list without this group replaces it.",
  "ownerKind": "vpc", "ownerId": "e7aff1bc-e01d-4ce2-bf63-2361cc777830",
  "rules": {"ingress": [
    {"name":"intra-vpc-0","protocol":"any","action":"permit","sourcePrefix":"10.0.0.64/26","destinationPrefix":"10.0.0.64/26"},
    {"name":"intra-vpc-1","protocol":"any","action":"permit","sourcePrefix":"10.0.0.64/26","destinationPrefix":"10.210.0.0/24"},
    {"name":"intra-vpc-2","protocol":"any","action":"permit","sourcePrefix":"10.210.0.0/24","destinationPrefix":"10.0.0.64/26"},
    {"name":"intra-vpc-3","protocol":"any","action":"permit","sourcePrefix":"10.210.0.0/24","destinationPrefix":"10.210.0.0/24"}],
  "egress": [
    {"name":"intra-vpc-egress-0","protocol":"any","action":"permit","sourcePrefix":"0.0.0.0/0","destinationPrefix":"10.0.0.64/26"},
    {"name":"intra-vpc-egress-1","protocol":"any","action":"permit","sourcePrefix":"0.0.0.0/0","destinationPrefix":"10.210.0.0/24"}]},
  "ruleCount": 6,
  "state": "active"
}`

// What rc.3 made for those prefixes, given in another order and with one
// repeated, which it sorts and deduplicates.
func TestTheDefaultGroupIsRC3s(t *testing.T) {
	g := defaultGroup(&fakeVPC{id: "vpc-57ces008g698sbe5hxh7wb90z4", uid: "e7aff1bc-e01d-4ce2-bf63-2361cc777830"},
		[]string{"10.210.0.0/24", "10.0.0.64/26", "10.210.0.0/24"})

	var want, got map[string]any
	if err := json.Unmarshal([]byte(rc3DefaultGroup), &want); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(jsonOf(t, securityGroupJSON(g))), &got); err != nil {
		t.Fatal(err)
	}
	for key, w := range want {
		if !reflect.DeepEqual(got[key], w) {
			t.Errorf("%s = %v\nwant %v", key, got[key], w)
		}
	}
	if !regexp.MustCompile(`^vpc-57ces008g698sbe5hxh7wb90z4-[0-9a-f]{8}-default$`).MatchString(g.id) {
		t.Errorf("id = %q, want <vpc id>-<8 hex>-default", g.id)
	}
}

// Every VPC gets its own, bound to it and owned by it, with the rules for the
// one subnet the fake gives a VPC.
func TestEachVPCGetsItsOwnDefaultGroup(t *testing.T) {
	s := newState()
	clusterNamed(t, s, "a")
	clusterNamed(t, s, "b")
	a, b := defaultGroupOf(t, s, "vpc-a"), defaultGroupOf(t, s, "vpc-b")
	if a == b {
		t.Fatalf("both VPCs are bound to %q", a)
	}

	_, vpc := call(t, s.getVPC, http.MethodGet, "vpc-a", "")
	code, g := call(t, s.getSecurityGroup, http.MethodGet, a, "")
	if code != http.StatusOK || g["ownerKind"] != "vpc" || g["ownerId"] != vpc["uid"] {
		t.Fatalf("read %s: %d, ownerKind %v, ownerId %v, want vpc and %v", a, code, g["ownerKind"], g["ownerId"], vpc["uid"])
	}
	if want := "Platform-managed default security group for VPC vpc-a. "; g["displayName"] != "Default (vpc-a)" ||
		!strings.HasPrefix(jsonOf(t, g["description"]), `"`+want) {
		t.Errorf("displayName %v, description %v", g["displayName"], g["description"])
	}
	// Keys sorted, as a map marshals.
	if got, want := jsonOf(t, g["rules"]), `{"egress":[{"action":"permit","destinationPrefix":"10.90.0.0/24","name":"intra-vpc-egress-0","protocol":"any","sourcePrefix":"0.0.0.0/0"}],`+
		`"ingress":[{"action":"permit","destinationPrefix":"10.90.0.0/24","name":"intra-vpc-0","protocol":"any","sourcePrefix":"10.90.0.0/24"}]}`; got != want {
		t.Errorf("rules = %s\nwant %s", got, want)
	}
	if g["ruleCount"] != float64(2) {
		t.Errorf("ruleCount = %v, want 2", g["ruleCount"])
	}

	// No group is called `default` any more.
	if code, _ := call(t, s.getSecurityGroup, http.MethodGet, "default", ""); code != http.StatusNotFound {
		t.Errorf("read default: %d, want 404", code)
	}
}

// On rc.4 a VPC with no subnet prefixes, as a cluster type with no
// networkSchema subnets makes, gets no default group, and an empty binding
// on it is refused instead of leaving it with no group.
func TestAVPCWithNoSubnetsHasNoDefaultGroup(t *testing.T) {
	s := newState()
	clusterNamed(t, s, "c-"+noSubnetsMarker)
	group(t, s, "mine")
	vpcID := "vpc-c-" + noSubnetsMarker
	_, vpc := call(t, s.getVPC, http.MethodGet, vpcID, "")
	if groups := listOf(t, vpc["securityGroups"]); len(groups) != 0 {
		t.Errorf("securityGroups = %v, want []", groups)
	}
	_, list := call(t, s.listSecurityGroups, http.MethodGet, "", "")
	for _, g := range listOf(t, list["securityGroups"]) {
		if m, ok := g.(map[string]any); !ok || m["ownerKind"] == "vpc" {
			t.Errorf("found a default group: %v", g)
		}
	}

	path := map[string]string{"id": vpcID}
	if code, out := in(t, s.setVPCSecurityGroups, http.MethodPost, "r", "p", path, `{"securityGroups":["mine"]}`); code != http.StatusAccepted {
		t.Fatalf("bind mine: %d %v", code, out)
	}
	settleIn(t, s.getVPC, "p", path)
	code, out := in(t, s.setVPCSecurityGroups, http.MethodPost, "r", "p", path, `{"securityGroups":[]}`)
	want := `{"code":409,"details":[{"message":"conflict: vpc \"` + vpcID + `\" has no default security group to fall ` +
		`back to, so its groups cannot be cleared; attach a group explicitly","reason":"CONFLICT_IN_USE"}],` +
		`"message":"conflict: vpc \"` + vpcID + `\" has no default security group to fall back to, so its groups ` +
		`cannot be cleared; attach a group explicitly","requestId":"` + fakeRequestID + `"}`
	if got := jsonOf(t, out["error"]); code != http.StatusConflict || got != want {
		t.Errorf("clear: %d %s\nwant 409 %s", code, got, want)
	}
}

// More than 16 distinct prefixes need over 256 intra-VPC rules, so rc.4 makes
// no default group for them either. 16 still gets one.
func TestADefaultGroupCoversAtMostSixteenPrefixes(t *testing.T) {
	prefixes := make([]string, 0, maxDefaultGroupPrefixes+1)
	for i := range maxDefaultGroupPrefixes {
		prefixes = append(prefixes, fmt.Sprintf("10.%d.0.0/24", i))
	}
	v := &fakeVPC{id: "vpc-many", uid: "u"}
	if g := defaultGroup(v, prefixes); g == nil || len(g.ingress) != 256 {
		t.Errorf("16 prefixes: %v, want a group with 256 ingress rules", g)
	}
	// A repeat isn't another prefix.
	if g := defaultGroup(v, append(prefixes, prefixes[0])); g == nil {
		t.Error("16 prefixes and a repeat: no group, want one")
	}
	if g := defaultGroup(v, append(prefixes, "10.99.0.0/24")); g != nil {
		t.Errorf("17 prefixes: %d ingress rules, want no group", len(g.ingress))
	}
}

// The contract: "Only VPCs on the `nico` fabric backend get one."
func TestAUfoVPCHasNoDefaultGroup(t *testing.T) {
	s := newState()
	clusterNamed(t, s, "c-"+ufoBackendMarker)
	_, vpc := call(t, s.getVPC, http.MethodGet, "vpc-c-"+ufoBackendMarker, "")
	if groups := listOf(t, vpc["securityGroups"]); vpc["backend"] != "ufo" || len(groups) != 0 {
		t.Errorf("backend %v, securityGroups %v: want ufo and []", vpc["backend"], groups)
	}
	_, list := call(t, s.listSecurityGroups, http.MethodGet, "", "")
	if items := listOf(t, list["securityGroups"]); len(items) != 0 {
		t.Errorf("groups = %v, want none", items)
	}
}

// Recorded on v1.3.0-rc.3: `[]` puts the VPC's own default back, by id, in
// the 202 and on every read after. Another VPC's default is refused.
func TestAnEmptyBindingPutsTheVPCsOwnDefaultBack(t *testing.T) {
	s := newState()
	clusterNamed(t, s, "a")
	clusterNamed(t, s, "b")
	group(t, s, "mine")
	a, b := defaultGroupOf(t, s, "vpc-a"), defaultGroupOf(t, s, "vpc-b")
	vpc := map[string]string{"id": "vpc-a"}

	code, out := in(t, s.setVPCSecurityGroups, http.MethodPost, "r", "p", vpc, `{"securityGroups":["`+b+`"]}`)
	want := `{"code":422,"message":"validation: security group \"` + b + `\" is the platform-managed default of a different VPC and ` +
		`cannot be attached to vpc \"vpc-a\"; send an empty list to reinstate this VPC's own default","requestId":"` + fakeRequestID + `"}`
	if got := jsonOf(t, out["error"]); code != http.StatusUnprocessableEntity || got != want {
		t.Errorf("another VPC's default: %d %s\nwant 422 %s", code, got, want)
	}

	if code, out = in(t, s.setVPCSecurityGroups, http.MethodPost, "r", "p", vpc, `{"securityGroups":["mine","`+a+`"]}`); code != http.StatusAccepted {
		t.Fatalf("its own default beside a tenant group: %d %v", code, out)
	}
	settleIn(t, s.getVPC, "p", vpc)

	code, out = in(t, s.setVPCSecurityGroups, http.MethodPost, "r", "p", vpc, `{"securityGroups":[]}`)
	if got := jsonOf(t, out["securityGroups"]); code != http.StatusAccepted || got != `["`+a+`"]` {
		t.Fatalf("[]: %d %s, want 202 naming %s", code, got, a)
	}
	settleIn(t, s.getVPC, "p", vpc)
	if got := defaultGroupOf(t, s, "vpc-a"); got != a {
		t.Errorf("read after []: %s, want %s", got, a)
	}

	// Its own default named on its own, rather than put back with [].
	if code, out = in(t, s.setVPCSecurityGroups, http.MethodPost, "r", "p", vpc, `{"securityGroups":["`+a+`"]}`); code != http.StatusAccepted {
		t.Errorf("its own default alone: %d %v, want 202", code, out)
	}
}

// "Removed only when its VPC is."
func TestADefaultGroupGoesWithItsVPC(t *testing.T) {
	s := newState()
	clusterNamed(t, s, "c")
	id := defaultGroupOf(t, s, "vpc-c")
	s.mu.Lock()
	s.scopes[scope{"r", "p"}].dropVPC("vpc-c")
	s.mu.Unlock()
	if code, _ := call(t, s.getSecurityGroup, http.MethodGet, id, ""); code != http.StatusNotFound {
		t.Errorf("read %s after its VPC went: %d, want 404", id, code)
	}
}

// Recorded on v1.3.0-rc.3: a default group can't be changed or deleted, and
// the delete is refused as immutable even though its VPC holds it.
func TestADefaultGroupRefusesWrites(t *testing.T) {
	s := newState()
	clusterNamed(t, s, "c")
	id := defaultGroupOf(t, s, "vpc-c")
	for _, tt := range []struct {
		name string
		h    http.HandlerFunc
		verb string
		body string
		want string
	}{
		{"PATCH", s.updateSecurityGroup, http.MethodPatch, `{"description":"x"}`, "cannot be modified; attach your own group to the VPC instead"},
		{"DELETE", s.deleteSecurityGroup, http.MethodDelete, "", "cannot be deleted or modified; attach your own security group"},
	} {
		code, out := call(t, tt.h, tt.verb, id, tt.body)
		if got := jsonOf(t, out["error"]); code != http.StatusConflict || !strings.Contains(got, tt.want) ||
			!strings.Contains(got, `"reason":"CONFLICT_IMMUTABLE"`) {
			t.Errorf("%s: %d %s, want 409 CONFLICT_IMMUTABLE saying %q", tt.name, code, got, tt.want)
		}
	}
	if _, g := call(t, s.getSecurityGroup, http.MethodGet, id, ""); g["state"] != "active" || g["description"] == "x" {
		t.Errorf("after the refused writes: state %v, description %v", g["state"], g["description"])
	}
}

// Recorded on v1.3.0-rc.3: an unknown id is reported before another VPC's
// default in the same list.
func TestABindingReportsUnknownIDsFirst(t *testing.T) {
	s := newState()
	clusterNamed(t, s, "a")
	clusterNamed(t, s, "b")
	b := defaultGroupOf(t, s, "vpc-b")
	code, out := in(t, s.setVPCSecurityGroups, http.MethodPost, "r", "p", map[string]string{"id": "vpc-a"},
		`{"securityGroups":["`+b+`","nope","gone"]}`)
	want := `{"code":422,"message":"validation: unknown security group(s): nope, gone","requestId":"` + fakeRequestID + `"}`
	if got := jsonOf(t, out["error"]); code != http.StatusUnprocessableEntity || got != want {
		t.Errorf("%d %s\nwant 422 %s", code, got, want)
	}
}

// The VPC id is cut to keep the group's id a valid name, never the random
// part or the suffix.
func TestALongVPCIDStillMakesAValidName(t *testing.T) {
	g := defaultGroup(&fakeVPC{id: "vpc-" + strings.Repeat("a", 41) + "-" + strings.Repeat("b", 20)}, []string{"10.0.0.0/24"})
	if len(g.id) > 63 || !regexp.MustCompile(`^vpc-a+-[0-9a-f]{8}-default$`).MatchString(g.id) {
		t.Errorf("id = %q (%d characters), want at most 63 ending in -<8 hex>-default", g.id, len(g.id))
	}
}
