package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The 404s the k0rdent API v1.3.0-dev.4 answered for an id nobody made, one line
// per kind, as recorded. The subnet and the peering were asked for under a
// network and a VPC that don't exist either, and the platform named the
// parent. Only the request id differs from run to run.
var live404s = map[string]string{
	"file store":     `{"error":{"code":404,"message":"not found: file store \"zz-missing\"","requestId":"929ccf60283e864dcd2b335afd4c3531","details":[{"reason":"NOT_FOUND","message":"not found: file store \"zz-missing\""}]}}`,
	"cluster":        `{"error":{"code":404,"message":"not found: cluster \"zz-missing\"","requestId":"8bc57d4cc40e172a2a008a4a2b932a19"}}`,
	"instance group": `{"error":{"code":404,"message":"not found: instance group \"zz-missing\"","requestId":"5f8e7223d16dd80a5512be60c979ec66","details":[{"reason":"NOT_FOUND","message":"not found: instance group \"zz-missing\""}]}}`,
	"network":        `{"error":{"code":404,"message":"not found: network \"zz-missing\"","requestId":"e9d3d12428559052fcff690dc7ff500c"}}`,
	"subnet":         `{"error":{"code":404,"message":"not found: network \"zz-missing\"","requestId":"187cd5316c8ef811942e9919551a5051"}}`,
	"security group": `{"error":{"code":404,"message":"not found: security group \"zz-missing\"","requestId":"801e2f7a5b23017473692b1d0050d26e","details":[{"reason":"NOT_FOUND","message":"not found: security group \"zz-missing\""}]}}`,
	"vpc":            `{"error":{"code":404,"message":"not found: vpc \"zz-missing\"","requestId":"bf847136bd9f894a0021b26852214117"}}`,
	"vpc peering":    `{"error":{"code":404,"message":"not found: vpc \"zz-missing\"","requestId":"e5dd0f18cd4ce5424f6e76483fe7d156"}}`,
	"project":        `{"error":{"code":404,"message":"not found: project \"zz-missing\"","requestId":"3ee729307a0a6e31c2fc985fc9aca77c"}}`,
}

var liveRequestID = regexp.MustCompile(`"requestId":"[0-9a-f]+"`)

// missingRoutes are the read and the delete of each kind, addressed to ids
// nobody made, with the parent missing too where the kind has one.
func missingRoutes(s *state) map[string]struct {
	get, del http.HandlerFunc
	path     map[string]string
} {
	zz := map[string]string{"id": "zz-missing"}
	return map[string]struct {
		get, del http.HandlerFunc
		path     map[string]string
	}{
		"file store":     {s.getFileStore, s.deleteFileStore, zz},
		"cluster":        {s.getCluster, s.deleteCluster, zz},
		"instance group": {s.getInstanceGroup, s.deleteInstanceGroup, zz},
		"network":        {s.getNetwork, s.deleteNetwork, zz},
		"subnet":         {s.getSubnet, s.deleteSubnet, map[string]string{"networkId": "zz-missing", "id": "zz-missing"}},
		"security group": {s.getSecurityGroup, s.deleteSecurityGroup, zz},
		// The VPC has no delete: the cluster workflow owns it.
		"vpc":         {s.getVPC, nil, zz},
		"vpc peering": {s.getVPCPeering, s.deleteVPCPeering, map[string]string{"vpcId": "zz-missing", "id": "zz-missing"}},
		"project": {s.getProjectInOrg, s.deleteProjectInOrg,
			map[string]string{"orgId": "org-demo", "project": "zz-missing"}},
	}
}

// Every field the provider reads off a 404, its code, message, reason and
// details, is what the platform sent, in the order it sent them. Only the
// request id is the fake's own.
func TestAMissingObjectAnswersThe404ThePlatformDoes(t *testing.T) {
	s := newState()
	for kind, route := range missingRoutes(s) {
		want := liveRequestID.ReplaceAllString(live404s[kind], `"requestId":"fake-request-id"`)
		for method, h := range map[string]http.HandlerFunc{http.MethodGet: route.get, http.MethodDelete: route.del} {
			if h == nil {
				continue
			}
			req := httptest.NewRequest(method, "/", nil)
			req.SetPathValue("region", "r")
			req.SetPathValue("project", "p")
			for k, v := range route.path {
				req.SetPathValue(k, v)
			}
			rec := httptest.NewRecorder()
			h(rec, req)
			if got := strings.TrimSpace(rec.Body.String()); rec.Code != http.StatusNotFound || got != want {
				t.Errorf("%s %s: %d\n got %s\nwant %s", method, kind, rec.Code, got, want)
			}
		}
	}
}

// A child under a parent that isn't there answers the parent's 404 on every
// route, the list and the create as well as the read and the delete the
// platform was seen answering it on.
func TestAChildUnderAMissingParentAnswersTheParents404(t *testing.T) {
	for _, k := range scopedKinds {
		if k.path == nil {
			continue
		}
		t.Run(k.name, func(t *testing.T) {
			s := newState()
			h := k.routes(s)
			path := map[string]string{}
			var parent string
			for key := range k.path {
				path[key] = "zz-missing"
				parent = map[string]string{"networkId": "network", "vpcId": "vpc"}[key]
			}
			want := `not found: ` + parent + ` "zz-missing"`
			for name, call := range map[string]struct {
				h      http.HandlerFunc
				method string
				path   map[string]string
				body   string
			}{
				"get":    {h.get, http.MethodGet, withID(path, "x"), ""},
				"delete": {h.del, http.MethodDelete, withID(path, "x"), ""},
				"list":   {h.list, http.MethodGet, path, ""},
				"create": {h.create, http.MethodPost, path, k.body},
			} {
				code, out := in(t, call.h, call.method, "r", "p", call.path, call.body)
				if code != http.StatusNotFound || errorMessage(out) != want {
					t.Errorf("%s: %d %v, want 404 %q", name, code, out, want)
				}
			}
		})
	}
}

// What the k0rdent API v1.3.0-dev.4 answered for a create in a project it was
// deleting, recorded within 140 ms of the DELETE. Only the cluster and the
// file store refuse, each in its own envelope. The network, subnet, security
// group and instance group creates were made, and the group went on to
// `active`. A peering wasn't tried: its VPC needs a cluster, which the
// project refuses.
var liveDeletingProject = map[string]string{
	"cluster":    `{"error":{"code":409,"message":"conflict: project \"p\" is being deleted and cannot accept new clusters","requestId":"fake-request-id"}}`,
	"file store": `{"error":{"code":409,"message":"conflict: project \"p\" is being deleted and cannot accept new file stores","requestId":"fake-request-id","details":[{"reason":"CONFLICT_IN_USE","message":"conflict: project \"p\" is being deleted and cannot accept new file stores"}]}}`,
}

// rawCall is a handler's status and body as sent.
func rawCall(t *testing.T, h http.HandlerFunc, method, project string, path map[string]string, body string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(method, "/", strings.NewReader(body))
	req.SetPathValue("region", "r")
	req.SetPathValue("project", project)
	for k, v := range path {
		req.SetPathValue(k, v)
	}
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec.Code, strings.TrimSpace(rec.Body.String())
}

// A create in a project being deleted answers, kind by kind, what the
// platform answered: the cluster's and the file store's 409, byte for byte
// apart from the request id, and nothing made. Every other kind is created.
func TestACreateInADeletingProjectAnswersAsRecorded(t *testing.T) {
	for _, k := range scopedKinds {
		t.Run(k.name, func(t *testing.T) {
			s := newState()
			h := k.routes(s)
			if k.parent != nil {
				k.parent(t, s)
			}
			// Put there directly: a peering's VPC is a cluster's, and a project
			// holding a cluster can't be deleted through the API.
			s.global().projects[at{id: "p"}].status = "deleting"

			code, body := rawCall(t, h.create, http.MethodPost, "p", k.path, k.body)
			want, refused := liveDeletingProject[k.name]
			switch {
			case refused && (code != http.StatusConflict || body != want):
				t.Errorf("create: %d\n got %s\nwant %s", code, body, want)
			case !refused && code != http.StatusCreated:
				t.Errorf("create: %d %s, want 201", code, body)
			}
			made := 0
			if !refused {
				made = 1
			}
			if _, list := in(t, h.list, http.MethodGet, "r", "p", k.path, ""); listed(list, k.listKey) != made {
				t.Errorf("listed after the create: %v, want %d", list, made)
			}
		})
	}
}

// testRow is the least a row can be, so the table's own rules can be tested
// without any one kind's.
type testRow struct {
	state string
	reads int
}

func (r *testRow) life() (*string, *int, *string) { return &r.state, &r.reads, nil }

// The table's rules, at its interface: a parent that isn't there is the
// parent's 404, a parent in a refusing state turns a create away, an id is
// made when none is sent and refused when taken, and a teardown ends on the
// read the kind declares, taking what goes with the row.
func TestTheObjectTable(t *testing.T) {
	parents, children := table[*testRow]{}, table[*testRow]{}
	var gone []string
	parent := &kind[*testRow]{name: "box", children: childRule{refusedWhile: []string{"deleting"}, refuse: bareConflict},
		rows: func(*objects) table[*testRow] { return parents }}
	child := &kind[*testRow]{name: "thing", parent: parent, parentParam: "boxId", idPrefix: "thing",
		teardown: 3, rows: func(*objects) table[*testRow] { return children },
		gone:      func(_ *objects, r *testRow) { gone = append(gone, r.state) },
		duplicate: alreadyExists("thing", "")}
	s := newState()
	request := func(box, id string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.SetPathValue("region", "r")
		r.SetPathValue("project", "p")
		r.SetPathValue("boxId", box)
		r.SetPathValue("id", id)
		return r
	}

	rec := httptest.NewRecorder()
	if _, ok := child.list(rec, request("b", ""), nil); ok || !strings.Contains(rec.Body.String(), `not found: box \"b\"`) {
		t.Errorf("list under no box: %v %s", ok, rec.Body)
	}

	parents[at{id: "b"}] = &testRow{state: "active"}
	rec = httptest.NewRecorder()
	o, under, ok := child.admit(s, rec, request("b", ""))
	if !ok || under != "b" {
		t.Fatalf("admit: %v %q %s", ok, under, rec.Body)
	}
	id, ok := child.claim(s, rec, o, under, nil)
	if !ok || !strings.HasPrefix(id, "thing-") {
		t.Fatalf("claim with no id: %q %v", id, ok)
	}
	child.put(o, under, id, &testRow{state: "active"})
	if _, taken := child.claim(s, rec, o, under, &id); taken || rec.Code != http.StatusConflict {
		t.Errorf("claim of a taken id: %v %d", taken, rec.Code)
	}

	rec = httptest.NewRecorder()
	got, where, ok := child.find(rec, request("b", id), o)
	if !ok {
		t.Fatalf("find: %s", rec.Body)
	}
	child.tearDown(o, where, got)
	for read := 1; read <= 3; read++ {
		rec = httptest.NewRecorder()
		_, ok := child.read(rec, request("b", id), o)
		if want := read < 3; ok != want {
			t.Fatalf("read %d after the delete: %v %d, want found %v", read, ok, rec.Code, want)
		}
	}
	if len(children) != 0 || len(gone) != 1 || gone[0] != "deleting" {
		t.Errorf("after the teardown: %v rows, gone %v", len(children), gone)
	}

	parents[at{id: "b"}].state = "deleting"
	rec = httptest.NewRecorder()
	if _, _, ok := child.admit(s, rec, request("b", "")); ok || rec.Code != http.StatusConflict ||
		strings.Contains(rec.Body.String(), `"reason"`) {
		t.Errorf("admit under a deleting box: %v %d %s", ok, rec.Code, rec.Body)
	}
}

// A network being deleted takes no new subnet, and a VPC that isn't settled
// `active` takes no new peering. Both are a 409 with CONFLICT_IN_USE under
// `details`, as createSubnet and createVPCPeering document, and nothing is
// made.
func TestAChildUnderAParentNotTakingChildrenIs409InUse(t *testing.T) {
	s := newState()
	if code, out := in(t, s.createNetwork, http.MethodPost, "r", "p", nil,
		`{"id":"n","type":"evpn","evpn":{"type":"l2vpn"}}`); code != http.StatusCreated {
		t.Fatalf("network: %d %v", code, out)
	}
	// A network's delete is immediate unless it holds subnets, so the test
	// puts it there itself.
	s.scopes[scope{"r", "p"}].networks[at{id: "n"}].state = "deleting"

	clusterInP(t, s)
	settleIn(t, s.getVPC, "p", map[string]string{"id": "vpc-c"})
	// Rebinding the VPC's groups leaves it `updating` until it settles.
	if code, out := in(t, s.setVPCSecurityGroups, http.MethodPost, "r", "p", map[string]string{"id": "vpc-c"},
		`{"securityGroups":[]}`); code != http.StatusAccepted {
		t.Fatalf("binding: %d %v", code, out)
	}

	for name, c := range map[string]struct {
		h       http.HandlerFunc
		path    map[string]string
		body    string
		list    http.HandlerFunc
		listKey string
	}{
		"subnet in a deleting network": {s.createSubnet, map[string]string{"networkId": "n"},
			`{"id":"x","addressPool":"tenant","prefixLength":24}`, s.listSubnets, "subnets"},
		"peering on an updating VPC": {s.createVPCPeering, map[string]string{"vpcId": "vpc-c"},
			`{"id":"x","remoteVpc":"/v1/regions/r/projects/elsewhere/networking/vpcs/vpc-elsewhere"}`, s.listVPCPeerings, "peerings"},
	} {
		code, out := in(t, c.h, http.MethodPost, "r", "p", c.path, c.body)
		if code != http.StatusConflict || detailReason(out) != "CONFLICT_IN_USE" {
			t.Errorf("%s: %d %v, want 409 with details[].reason CONFLICT_IN_USE", name, code, out)
		}
		if _, list := in(t, c.list, http.MethodGet, "r", "p", c.path, ""); listed(list, c.listKey) != 0 {
			t.Errorf("%s: the 409 made one: %v", name, list)
		}
	}
}

// A subnet, a security group and a peering each enter `deleting` on a delete
// and are polled until they 404, as their operations document. A subnet or a
// peering still `creating`, or already `deleting`, refuses a delete with
// CONFLICT_IN_USE. A security group refuses one while `creating`, and takes a
// second delete while `deleting` as the idempotent 204. A network still
// holding a subnet on its way out can't be deleted yet.
func TestSubnetsGroupsAndPeeringsTearDownLikeThePlatform(t *testing.T) {
	s := newState()
	if code, out := in(t, s.createNetwork, http.MethodPost, "r", "p", nil,
		`{"id":"n","type":"evpn","evpn":{"type":"l2vpn"}}`); code != http.StatusCreated {
		t.Fatalf("network: %d %v", code, out)
	}
	clusterInP(t, s)
	for name, k := range map[string]struct {
		create, get, del http.HandlerFunc
		path             map[string]string
		body             string
		again            int
	}{
		"subnet": {s.createSubnet, s.getSubnet, s.deleteSubnet, map[string]string{"networkId": "n"},
			`{"id":"x","addressPool":"tenant","prefixLength":24}`, http.StatusConflict},
		"security group": {s.createSecurityGroup, s.getSecurityGroup, s.deleteSecurityGroup, nil,
			`{` + oneRule + `,"id":"x"}`, http.StatusNoContent},
		"peering": {s.createVPCPeering, s.getVPCPeering, s.deleteVPCPeering, map[string]string{"vpcId": "vpc-c"},
			`{"id":"x","remoteVpc":"/v1/regions/r/projects/elsewhere/networking/vpcs/vpc-elsewhere"}`, http.StatusConflict},
	} {
		x := withID(k.path, "x")
		if code, out := in(t, k.create, http.MethodPost, "r", "p", k.path, k.body); code != http.StatusCreated {
			t.Fatalf("%s: create: %d %v", name, code, out)
		}
		if code, out := in(t, k.del, http.MethodDelete, "r", "p", x, ""); code != http.StatusConflict || detailReason(out) != "CONFLICT_IN_USE" {
			t.Errorf("%s: delete while creating: %d %v, want 409 CONFLICT_IN_USE", name, code, out)
		}
		settleIn(t, k.get, "p", x)
		if code, out := in(t, k.del, http.MethodDelete, "r", "p", x, ""); code != http.StatusNoContent {
			t.Fatalf("%s: delete: %d %v", name, code, out)
		}
		if code, out := in(t, k.del, http.MethodDelete, "r", "p", x, ""); code != k.again {
			t.Errorf("%s: delete while deleting: %d %v, want %d", name, code, out, k.again)
		}
		if name == "subnet" {
			if code, out := in(t, s.deleteNetwork, http.MethodDelete, "r", "p", map[string]string{"id": "n"}, ""); code != http.StatusConflict {
				t.Errorf("network delete under a deleting subnet: %d %v, want 409", code, out)
			}
		}
		if code, out := in(t, k.get, http.MethodGet, "r", "p", x, ""); code != http.StatusOK || out["state"] != "deleting" {
			t.Errorf("%s: read after the delete: %d %v, want it `deleting`", name, code, out)
		}
		if code, out := in(t, k.get, http.MethodGet, "r", "p", x, ""); code != http.StatusNotFound {
			t.Errorf("%s: second read after the delete: %d %v, want 404", name, code, out)
		}
	}
}

// The same request gets the same answer every time: node pools in id order on
// a cluster and an instance group, and a peering create carrying several
// server-owned fields refused for the first of them in the order the fake
// checks them. Go's map order would otherwise decide each.
func TestAnswersDontDependOnMapOrder(t *testing.T) {
	pools := func(out map[string]any, key string) []string {
		var ids []string
		rows, ok := out["nodePools"].([]any)
		if !ok {
			t.Fatalf("no nodePools in %v", out)
		}
		for _, p := range rows {
			pool, ok := p.(map[string]any)
			if !ok {
				t.Fatalf("pool %v", p)
			}
			ids = append(ids, fmt.Sprint(pool[key]))
		}
		return ids
	}
	s := newState()
	clusterInP(t, s)
	if code, out := in(t, s.createInstanceGroup, http.MethodPost, "r", "p", nil,
		`{"id":"g","clusterType":"`+inferenceRef+`/versions/`+inferenceV2+`","nodePools":[{"nodePool":"workers","size":2},{"nodePool":"control-plane","size":1}]}`); code != http.StatusCreated {
		t.Fatalf("group: %d %v", code, out)
	}
	for range 30 {
		if _, out := in(t, s.getCluster, http.MethodGet, "r", "p", map[string]string{"id": "c"}, ""); !slices.IsSorted(pools(out, "id")) {
			t.Fatalf("cluster pools %v", pools(out, "id"))
		}
		if _, out := in(t, s.getInstanceGroup, http.MethodGet, "r", "p", map[string]string{"id": "g"}, ""); !slices.IsSorted(pools(out, "nodePool")) {
			t.Fatalf("group pools %v", pools(out, "nodePool"))
		}
		_, out := in(t, s.createVPCPeering, http.MethodPost, "r", "p", map[string]string{"vpcId": "vpc-c"},
			`{"remoteVpc":"/v1/regions/r/projects/elsewhere/networking/vpcs/v","region":"r","projectId":"p","localVpc":"l","state":"s","selfLink":"x"}`)
		if !strings.HasPrefix(errorMessage(out), "selfLink ") {
			t.Fatalf("server-owned fields refused as %q, want selfLink first", errorMessage(out))
		}
	}
}

// detailReason is the one `details[].reason` an error carries, or "" when it
// carries none or several.
func detailReason(out map[string]any) string {
	e, ok := out["error"].(map[string]any)
	if !ok {
		return ""
	}
	details, ok := e["details"].([]any)
	if !ok || len(details) != 1 {
		return ""
	}
	d, ok := details[0].(map[string]any)
	if !ok {
		return ""
	}
	return fmt.Sprint(d["reason"])
}

// The gateway's 403 for a project or an organization that doesn't exist, as
// v1.3.0-dev.4 answered it for every kind's read, list and create: the caller
// holds no role on it, so authorization answers before any service is asked.
const live403 = `{"error":{"code":403,"message":"forbidden","requestId":"fake-request-id"}}`

// Every route under a project or an organization answers the gateway's 403
// when it doesn't exist, and goes through to the service when it does.
func TestAMissingProjectOrOrganizationIsTheGateways403(t *testing.T) {
	s := newState()
	secrets, err := newRunSecrets()
	if err != nil {
		t.Fatalf("newRunSecrets: %v", err)
	}
	mux := http.NewServeMux()
	s.routes(mux, secrets.auth)
	send := func(method, path, body string) (int, string) {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+secrets.AccessToken)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec.Code, strings.TrimSpace(rec.Body.String())
	}
	under := "/v1/regions/r/projects/nobody"
	org := "/v1/regions/global/organizations/zz-missing"
	for _, c := range []struct{ method, path, body string }{
		{http.MethodGet, under + "/networking/networks/n", ""},
		{http.MethodGet, under + "/networking/networks/n/subnets", ""},
		{http.MethodPost, under + "/networking/networks", `{"id":"x","type":"evpn","evpn":{"type":"l2vpn"}}`},
		{http.MethodGet, under + "/networking/security-groups", ""},
		{http.MethodGet, under + "/networking/vpcs/v", ""},
		{http.MethodPost, under + "/networking/vpcs/v", `{"securityGroups":[]}`},
		{http.MethodGet, under + "/networking/vpcs/v/peerings", ""},
		{http.MethodGet, under + "/compute/clusters", ""},
		{http.MethodPost, under + "/compute/instance-groups", `{}`},
		{http.MethodDelete, under + "/storage/file-stores/f", ""},
		{http.MethodGet, org, ""},
		{http.MethodGet, org + "/projects/demo", ""},
		{http.MethodPost, org + "/projects", `{"id":"x"}`},
		{http.MethodGet, org + "/quota", ""},
		{http.MethodGet, org + "/projects", ""},
	} {
		if code, body := send(c.method, c.path, c.body); code != http.StatusForbidden || body != live403 {
			t.Errorf("%s %s: %d %s, want the gateway's 403", c.method, c.path, code, body)
		}
	}
	for _, path := range []string{"/v1/regions/r/projects/p/networking/networks", "/v1/regions/global/organizations/org-demo/projects/demo",
		"/v1/regions/global/organizations/org-demo/projects"} {
		if code, body := send(http.MethodGet, path, ""); code != http.StatusOK {
			t.Errorf("GET %s: %d %s, want it served", path, code, body)
		}
	}
}

// A create under a missing parent names the parent as a read does, and also
// repeats itself under `details` with NOT_FOUND, where the read doesn't. Both
// recorded on v1.3.0-dev.4, as was a VPC binding on a missing VPC, which
// answers as a read.
func TestACreateUnderAMissingParentCarriesDetails(t *testing.T) {
	s := newState()
	for name, c := range map[string]struct {
		h    http.HandlerFunc
		path map[string]string
		body string
		want string
	}{
		"subnet": {s.createSubnet, map[string]string{"networkId": "zz-missing"}, `{"id":"x","addressPool":"tenant","prefixLength":24}`,
			`{"error":{"code":404,"message":"not found: network \"zz-missing\"","requestId":"fake-request-id","details":[{"reason":"NOT_FOUND","message":"not found: network \"zz-missing\""}]}}`},
		"vpc peering": {s.createVPCPeering, map[string]string{"vpcId": "zz-missing"}, `{"id":"x","remoteVpc":"/v1/regions/r/projects/elsewhere/networking/vpcs/v"}`,
			`{"error":{"code":404,"message":"not found: vpc \"zz-missing\"","requestId":"fake-request-id","details":[{"reason":"NOT_FOUND","message":"not found: vpc \"zz-missing\""}]}}`},
		"vpc binding": {s.setVPCSecurityGroups, map[string]string{"id": "zz-missing"}, `{"securityGroups":[]}`,
			`{"error":{"code":404,"message":"not found: vpc \"zz-missing\"","requestId":"fake-request-id"}}`},
	} {
		if code, body := rawCall(t, c.h, http.MethodPost, "p", c.path, c.body); code != http.StatusNotFound || body != c.want {
			t.Errorf("%s: %d\n got %s\nwant %s", name, code, body, c.want)
		}
	}
}

// A second DELETE of an object already being deleted, as v1.3.0-dev.4
// answered it: a 409 for every kind but the security group, whose second
// DELETE is the idempotent 204. A network enters `deleting` as the others do.
var liveSecondDelete = map[string]string{
	"network":        `{"error":{"code":409,"message":"conflict: network \"x\" cannot be deleted in current state","requestId":"fake-request-id","details":[{"reason":"CONFLICT_IN_USE","message":"conflict: network \"x\" cannot be deleted in current state"}]}}`,
	"subnet":         `{"error":{"code":409,"message":"conflict: subnet \"x\" cannot be deleted in current state","requestId":"fake-request-id","details":[{"reason":"CONFLICT_IN_USE","message":"conflict: subnet \"x\" cannot be deleted in current state"}]}}`,
	"peering":        `{"error":{"code":409,"message":"conflict: vpc peering \"x\" cannot be deleted in current state","requestId":"fake-request-id","details":[{"reason":"CONFLICT_IN_USE","message":"conflict: vpc peering \"x\" cannot be deleted in current state"}]}}`,
	"instance group": `{"error":{"code":409,"message":"conflict: instance group \"x\" is in state deleting; delete requires terminal state (active or failed)","requestId":"fake-request-id","details":[{"reason":"CONFLICT_IN_USE","message":"conflict: instance group \"x\" is in state deleting; delete requires terminal state (active or failed)"}]}}`,
	"cluster":        `{"error":{"code":409,"message":"conflict: cluster \"x\" is already being deleted; wait for the delete to settle","requestId":"fake-request-id"}}`,
	"security group": "",
}

func TestASecondDeleteAnswersAsRecorded(t *testing.T) {
	for _, k := range scopedKinds {
		want, recorded := liveSecondDelete[k.name]
		if !recorded {
			continue
		}
		t.Run(k.name, func(t *testing.T) {
			s := newState()
			h := k.routes(s)
			if k.parent != nil {
				k.parent(t, s)
			}
			if code, out := in(t, h.create, http.MethodPost, "r", "p", k.path, k.body); code != http.StatusCreated {
				t.Fatalf("create: %d %v", code, out)
			}
			x := withID(k.path, "x")
			settleIn(t, h.get, "p", x)
			if code, body := rawCall(t, h.del, http.MethodDelete, "p", x, ""); code != http.StatusNoContent {
				t.Fatalf("first delete: %d %s", code, body)
			}
			if code, out := in(t, h.get, http.MethodGet, "r", "p", x, ""); code != http.StatusOK || out["state"] != "deleting" {
				t.Errorf("read after the delete: %d %v, want it `deleting`", code, out)
			}
			code, body := rawCall(t, h.del, http.MethodDelete, "p", x, "")
			switch {
			case want == "" && code != http.StatusNoContent:
				t.Errorf("second delete: %d %s, want the idempotent 204", code, body)
			case want != "" && (code != http.StatusConflict || body != want):
				t.Errorf("second delete: %d\n got %s\nwant %s", code, body, want)
			}
		})
	}
	s := newState()
	org := map[string]string{"orgId": "org-demo", "project": "twice"}
	if code, out := in(t, s.createProjectInOrg, http.MethodPost, "", "", org, `{"id":"twice","region":"us-texas-1"}`); code != http.StatusCreated {
		t.Fatalf("project: %d %v", code, out)
	}
	rawCall(t, s.deleteProjectInOrg, http.MethodDelete, "", org, "")
	want := `{"error":{"code":409,"message":"conflict: project \"twice\" is already being deleted","requestId":"fake-request-id"}}`
	if code, body := rawCall(t, s.deleteProjectInOrg, http.MethodDelete, "", org, ""); code != http.StatusConflict || body != want {
		t.Errorf("project second delete: %d\n got %s\nwant %s", code, body, want)
	}
}

// deleteNetwork documents no state a network must settle into first: only
// its read-only and in-use 409s. So a network the fake steered into a state
// the contract doesn't declare is still deleted, and only one already
// `deleting` is refused, as v1.3.0-dev.4 refused it.
func TestANetworkIsRefusedADeleteOnlyWhileDeleting(t *testing.T) {
	s := newState()
	if code, out := in(t, s.createNetwork, http.MethodPost, "r", "p", nil,
		`{"id":"q","type":"evpn","evpn":{"type":"l2vpn"},"displayName":"`+settlesUnknown+`"}`); code != http.StatusCreated {
		t.Fatalf("network: %d %v", code, out)
	}
	for range settleAfter + 1 {
		in(t, s.getNetwork, http.MethodGet, "r", "p", map[string]string{"id": "q"}, "")
	}
	if code, out := in(t, s.deleteNetwork, http.MethodDelete, "r", "p", map[string]string{"id": "q"}, ""); code != http.StatusNoContent {
		t.Errorf("delete of a network in %q: %d %v, want 204", unrecognisedState, code, out)
	}
}

// A security group a VPC carries is refused on DELETE, as v1.3.0-dev.4 refused
// it, naming the VPC, and left as it was.
func TestAGroupAVPCCarriesIsNotDeleted(t *testing.T) {
	s := newState()
	clusterInP(t, s)
	if code, out := in(t, s.createSecurityGroup, http.MethodPost, "r", "p", nil, `{`+oneRule+`,"id":"psg"}`); code != http.StatusCreated {
		t.Fatalf("group: %d %v", code, out)
	}
	settleIn(t, s.getSecurityGroup, "p", map[string]string{"id": "psg"})
	if code, out := in(t, s.setVPCSecurityGroups, http.MethodPost, "r", "p", map[string]string{"id": "vpc-c"},
		`{"securityGroups":["psg"]}`); code != http.StatusAccepted {
		t.Fatalf("binding: %d %v", code, out)
	}
	message := "conflict: security group \\\"psg\\\" is attached to: vpc vpc-c \u2014 detach it before deleting"
	want := `{"error":{"code":409,"message":"` + message + `","requestId":"fake-request-id","details":[{"reason":"CONFLICT_IN_USE","message":"` + message + `"}]}}`
	if code, body := rawCall(t, s.deleteSecurityGroup, http.MethodDelete, "p", map[string]string{"id": "psg"}, ""); code != http.StatusConflict || body != want {
		t.Errorf("delete: %d\n got %s\nwant %s", code, body, want)
	}
	if code, out := in(t, s.getSecurityGroup, http.MethodGet, "r", "p", map[string]string{"id": "psg"}, ""); code != http.StatusOK || out["state"] != "active" {
		t.Errorf("after the refused delete: %d %v, want it untouched", code, out)
	}
	if _, out := in(t, s.listSecurityGroupAttachments, http.MethodGet, "r", "p", map[string]string{"id": "psg"}, ""); !strings.Contains(fmt.Sprint(out), "kind:vpc") {
		t.Errorf("attachments: %v, want the VPC named", out)
	}
}

// listProjectsInOrg lists one organization's projects, on the offset contract
// and on the deprecated cursor, with the status and search filters it
// documents. A status it doesn't accept is a 422.
func TestTheProjectsOfOneOrganization(t *testing.T) {
	s := newState()
	org := map[string]string{"orgId": "org-demo"}
	_, out := in(t, s.listProjectsInOrg, http.MethodGet, "", "", org, "")
	if listed(out, "projects") == 0 {
		t.Fatalf("org-demo lists nothing: %v", out)
	}
	rows, ok := out["projects"].([]any)
	if !ok {
		t.Fatalf("projects: %v", out)
	}
	for _, p := range rows {
		if row, ok := p.(map[string]any); !ok || row["organizationId"] != "org-demo" {
			t.Errorf("a project of another organization: %v", p)
		}
	}
	if _, out := in(t, s.listProjectsInOrg, http.MethodGet, "", "", map[string]string{"orgId": "org-empty"}, ""); listed(out, "projects") != 0 {
		t.Errorf("org-empty lists %v", out)
	}
	for query, want := range map[string]int{"status=deleting": 200, "status=deleted": 422, "status=failed": 422, "offset=0&limit=1": 200, "offset=0&pageSize=1": 422} {
		req := httptest.NewRequest(http.MethodGet, "/?"+query, nil)
		req.SetPathValue("orgId", "org-demo")
		rec := httptest.NewRecorder()
		s.listProjectsInOrg(rec, req)
		if rec.Code != want {
			t.Errorf("?%s: %d %s, want %d", query, rec.Code, rec.Body, want)
		}
	}
}
