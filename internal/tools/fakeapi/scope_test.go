package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// The handler tests address projects "p" and "q" in region "r". A create
// needs its project to exist, so every fake the tests make seeds both beside
// its own.
func TestMain(m *testing.M) {
	for _, id := range []string{"p", "q"} {
		projectSeeds = append(projectSeeds, fakeProject{id: id, org: "org-demo", status: "active"})
	}
	os.Exit(m.Run())
}

// in calls a handler with the path values its route would set, addressed to
// one region and project.
func in(t *testing.T, h http.HandlerFunc, method, region, project string, path map[string]string, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, "/", strings.NewReader(body))
	req.SetPathValue("region", region)
	req.SetPathValue("project", project)
	for k, v := range path {
		req.SetPathValue(k, v)
	}
	rec := httptest.NewRecorder()
	h(rec, req)
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil && rec.Body.Len() > 0 {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return rec.Code, out
}

// listed is how many rows a list answered with. An empty list may leave the
// key out altogether, and that counts as none.
func listed(out map[string]any, key string) int {
	if rows, ok := out[key].([]any); ok {
		return len(rows)
	}
	return 0
}

type scopedRoutes struct {
	create, get, list, del http.HandlerFunc
}

// One project-owned kind each: how to create one called "x", where it lives,
// and what has to exist first.
var scopedKinds = []struct {
	name    string
	routes  func(*state) scopedRoutes
	path    map[string]string
	body    string
	listKey string
	parent  func(*testing.T, *state)
}{
	{
		name: "cluster",
		routes: func(s *state) scopedRoutes {
			return scopedRoutes{s.createCluster, s.getCluster, s.listClusters, s.deleteCluster}
		},
		body:    `{"id":"x","clusterType":"` + inferenceRef + `/versions/` + inferenceV2 + `"}`,
		listKey: "clusters",
	},
	{
		name: "instance group",
		routes: func(s *state) scopedRoutes {
			return scopedRoutes{s.createInstanceGroup, s.getInstanceGroup, s.listInstanceGroups, s.deleteInstanceGroup}
		},
		body:    `{"id":"x","clusterType":"` + inferenceRef + `/versions/` + inferenceV2 + `","nodePools":[{"nodePool":"workers","size":2}]}`,
		listKey: "instanceGroups",
	},
	{
		name: "network",
		routes: func(s *state) scopedRoutes {
			return scopedRoutes{s.createNetwork, s.getNetwork, s.listNetworks, s.deleteNetwork}
		},
		body:    `{"id":"x","type":"evpn","evpn":{"type":"l2vpn"}}`,
		listKey: "networks",
	},
	{
		name: "subnet",
		routes: func(s *state) scopedRoutes {
			return scopedRoutes{s.createSubnet, s.getSubnet, s.listSubnets, s.deleteSubnet}
		},
		path:    map[string]string{"networkId": "n"},
		body:    `{"id":"x","addressPool":"tenant","prefixLength":24}`,
		listKey: "subnets",
		parent: func(t *testing.T, s *state) {
			t.Helper()
			if code, out := in(t, s.createNetwork, http.MethodPost, "r", "p", nil,
				`{"id":"n","type":"evpn","evpn":{"type":"l2vpn"}}`); code != http.StatusCreated {
				t.Fatalf("network: %d %v", code, out)
			}
		},
	},
	{
		name: "security group",
		routes: func(s *state) scopedRoutes {
			return scopedRoutes{s.createSecurityGroup, s.getSecurityGroup, s.listSecurityGroups, s.deleteSecurityGroup}
		},
		body:    `{` + oneRule + `,"id":"x"}`,
		listKey: "securityGroups",
	},
	{
		name: "peering",
		routes: func(s *state) scopedRoutes {
			return scopedRoutes{s.createVPCPeering, s.getVPCPeering, s.listVPCPeerings, s.deleteVPCPeering}
		},
		path:    map[string]string{"vpcId": "vpc-c"},
		body:    `{"id":"x","remoteVpc":"/v1/regions/r/projects/elsewhere/networking/vpcs/vpc-elsewhere"}`,
		listKey: "peerings",
		parent:  clusterInP,
	},
	{
		name: "file store",
		routes: func(s *state) scopedRoutes {
			return scopedRoutes{s.createFileStore, s.getFileStore, s.listFileStores, s.deleteFileStore}
		},
		body:    `{"id":"x","shareType":"general","capacityGiB":10}`,
		listKey: "fileStores",
	},
}

// clusterInP creates cluster "c" in project "p", and with it VPC
// "vpc-c" there.
func clusterInP(t *testing.T, s *state) {
	t.Helper()
	if code, out := in(t, s.createCluster, http.MethodPost, "r", "p", nil,
		`{"id":"c","clusterType":"`+inferenceRef+`/versions/`+inferenceV2+`"}`); code != http.StatusCreated {
		t.Fatalf("cluster: %d %v", code, out)
	}
}

// settleIn reads an object until it leaves its transient state, so that a
// delete aimed at it would be admitted rather than refused or deferred.
func settleIn(t *testing.T, get http.HandlerFunc, project string, path map[string]string) {
	t.Helper()
	for range 10 {
		code, out := in(t, get, http.MethodGet, "r", project, path, "")
		if code != http.StatusOK {
			t.Fatalf("settling in %s: %d %v", project, code, out)
		}
		if out["state"] == "active" {
			return
		}
	}
	t.Fatalf("never settled in %s", project)
}

// stillThere asserts an object reads back, and not on its way out.
func stillThere(t *testing.T, get http.HandlerFunc, project string, path map[string]string) {
	t.Helper()
	for range 5 {
		code, out := in(t, get, http.MethodGet, "r", project, path, "")
		if code != http.StatusOK || out["state"] == "deleting" {
			t.Fatalf("%s's copy: %d %v", project, code, out)
		}
	}
}

func withID(path map[string]string, id string) map[string]string {
	out := map[string]string{"id": id}
	for k, v := range path {
		out[k] = v
	}
	return out
}

// An object made in one project is invisible from another, exactly as one
// that does not exist: the same 404 on a read or a delete, and no row in a
// list. A provider that sent a request to the wrong project would otherwise
// find what it was looking for and pass. "The same 404" is the one a fake
// where nothing was ever made answers on the same path, which for a subnet or
// a peering is its parent's.
func TestAnObjectIsInvisibleFromAnotherProject(t *testing.T) {
	for _, k := range scopedKinds {
		t.Run(k.name, func(t *testing.T) {
			s := newState()
			h := k.routes(s)
			if k.parent != nil {
				k.parent(t, s)
			}
			if code, out := in(t, h.create, http.MethodPost, "r", "p", k.path, k.body); code != http.StatusCreated {
				t.Fatalf("create in p: %d %v", code, out)
			}
			// Settled, so a delete that reached p's copy would be admitted.
			settleIn(t, h.get, "p", withID(k.path, "x"))

			_, missing := in(t, k.routes(newState()).get, http.MethodGet, "r", "q", withID(k.path, "x"), "")
			code, out := in(t, h.get, http.MethodGet, "r", "q", withID(k.path, "x"), "")
			if code != http.StatusNotFound {
				t.Errorf("get from q: %d %v", code, out)
			}
			if errorMessage(out) != errorMessage(missing) {
				t.Errorf("get from q says %q, a missing one says %q", errorMessage(out), errorMessage(missing))
			}
			if _, out := in(t, h.list, http.MethodGet, "r", "q", k.path, ""); listed(out, k.listKey) != 0 {
				t.Errorf("list in q: %v", out)
			}
			if code, out := in(t, h.del, http.MethodDelete, "r", "q", withID(k.path, "x"), ""); code != http.StatusNotFound {
				t.Errorf("delete from q: %d %v", code, out)
			}

			// Still there, and still listed, where it was made.
			stillThere(t, h.get, "p", withID(k.path, "x"))
			if _, out := in(t, h.list, http.MethodGet, "r", "p", k.path, ""); listed(out, k.listKey) == 0 {
				t.Errorf("list in p: %v", out)
			}
		})
	}
}

// The same id in two projects is two objects, not a conflict.
func TestTheSameIDInTwoProjectsIsTwoObjects(t *testing.T) {
	for _, k := range scopedKinds {
		t.Run(k.name, func(t *testing.T) {
			s := newState()
			h := k.routes(s)
			if k.parent != nil {
				k.parent(t, s)
				// The parent again, in q this time.
				for key, v := range k.path {
					switch key {
					case "networkId":
						in(t, s.createNetwork, http.MethodPost, "r", "q", nil, `{"id":"`+v+`","type":"evpn","evpn":{"type":"l2vpn"}}`)
					case "vpcId":
						in(t, s.createCluster, http.MethodPost, "r", "q", nil,
							`{"id":"c","clusterType":"`+inferenceRef+`/versions/`+inferenceV2+`"}`)
					}
				}
			}
			for _, project := range []string{"p", "q"} {
				if code, out := in(t, h.create, http.MethodPost, "r", project, k.path, k.body); code != http.StatusCreated {
					t.Fatalf("create in %s: %d %v", project, code, out)
				}
			}
			x := withID(k.path, "x")
			settleIn(t, h.get, "p", x)
			settleIn(t, h.get, "q", x)
			if code, out := in(t, h.del, http.MethodDelete, "r", "q", x, ""); code != http.StatusNoContent && code != http.StatusAccepted {
				t.Fatalf("delete from q: %d %v", code, out)
			}
			// q's copy goes, however long its teardown takes, and p's stays.
			gone := false
			for range 10 {
				if code, _ := in(t, h.get, http.MethodGet, "r", "q", x, ""); code == http.StatusNotFound {
					gone = true
					break
				}
			}
			if !gone {
				t.Fatal("q's copy never went away")
			}
			stillThere(t, h.get, "p", x)
		})
	}
}

// The region is part of the path too, so a project's objects in one region
// are not its objects in another.
func TestAnObjectIsInvisibleFromAnotherRegion(t *testing.T) {
	s := newState()
	if code, out := in(t, s.createNetwork, http.MethodPost, "r", "p", nil,
		`{"id":"x","type":"evpn","evpn":{"type":"l2vpn"}}`); code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, out)
	}
	if code, out := in(t, s.getNetwork, http.MethodGet, "r2", "p", map[string]string{"id": "x"}, ""); code != http.StatusNotFound {
		t.Errorf("get from r2: %d %v", code, out)
	}
}

// What a cluster carries with it, its VPC and its kubeconfig, is in its
// project as well, and so is every operation on the cluster itself. Each row
// runs against a fresh cluster and has to succeed from p, so a 404 from q
// says the project decided it and not the cluster's state.
func TestAClusterAndWhatItCarriesAreInvisibleFromAnotherProject(t *testing.T) {
	c := map[string]string{"id": "c"}
	vpc := map[string]string{"id": "vpc-c"}

	for name, row := range map[string]struct {
		h      func(*state) http.HandlerFunc
		method string
		path   map[string]string
		body   string
		// Cancel needs an operation in flight, so its cluster is left creating.
		inFlight bool
	}{
		"update":             {func(s *state) http.HandlerFunc { return s.updateCluster }, http.MethodPatch, c, `{"displayName":"moved"}`, false},
		"scale":              {func(s *state) http.HandlerFunc { return s.scaleCluster }, http.MethodPost, c, `{"nodePools":[{"id":"workers","nodeCount":3}]}`, false},
		"upgrade":            {func(s *state) http.HandlerFunc { return s.upgradeCluster }, http.MethodPost, c, "", false},
		"cancel":             {func(s *state) http.HandlerFunc { return s.cancelCluster }, http.MethodPost, c, `{"executionId":"c-op-1"}`, true},
		"kubeconfig list":    {func(s *state) http.HandlerFunc { return s.listKubeconfigs }, http.MethodGet, c, "", false},
		"kubeconfig":         {func(s *state) http.HandlerFunc { return s.getClusterKubeconfig }, http.MethodGet, c, "", false},
		"kubeconfig by id":   {func(s *state) http.HandlerFunc { return s.downloadKubeconfig }, http.MethodGet, map[string]string{"id": "c", "kid": "kc-c"}, "", false},
		"vpc":                {func(s *state) http.HandlerFunc { return s.getVPC }, http.MethodGet, vpc, "", false},
		"vpc binding":        {func(s *state) http.HandlerFunc { return s.setVPCSecurityGroups }, http.MethodPost, vpc, `{"securityGroups":[]}`, false},
		"peering on its vpc": {func(s *state) http.HandlerFunc { return s.createVPCPeering }, http.MethodPost, map[string]string{"vpcId": "vpc-c"}, `{"remoteVpc":"/v1/regions/r/projects/elsewhere/networking/vpcs/vpc-elsewhere"}`, false},
	} {
		t.Run(name, func(t *testing.T) {
			s := newState()
			clusterInP(t, s)
			if !row.inFlight {
				settleIn(t, s.getCluster, "p", c)
				// The kubeconfig is issued a few reads after the cluster settles.
				for range 5 {
					in(t, s.listKubeconfigs, http.MethodGet, "r", "p", c, "")
				}
			}
			h := row.h(s)
			if code, out := in(t, h, row.method, "r", "q", row.path, row.body); code != http.StatusNotFound {
				t.Errorf("from q: %d %v", code, out)
			}
			if code := rawCode(t, h, row.method, "p", row.path, row.body); code < 200 || code >= 300 {
				t.Errorf("from p: %d", code)
			}
		})
	}

	s := newState()
	clusterInP(t, s)
	if _, out := in(t, s.listVPCs, http.MethodGet, "r", "q", nil, ""); listed(out, "vpcs") != 0 {
		t.Errorf("VPCs in q: %v", out)
	}
	if _, out := in(t, s.listVPCs, http.MethodGet, "r", "p", nil, ""); listed(out, "vpcs") != 1 {
		t.Errorf("VPCs in p: %v", out)
	}
}

// rawCode is the status alone, for a handler whose success body is not JSON:
// a kubeconfig is served as YAML.
func rawCode(t *testing.T, h http.HandlerFunc, method, project string, path map[string]string, body string) int {
	t.Helper()
	req := httptest.NewRequest(method, "/", strings.NewReader(body))
	req.SetPathValue("region", "r")
	req.SetPathValue("project", project)
	for k, v := range path {
		req.SetPathValue(k, v)
	}
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec.Code
}

// A file store's instance-group consumer resolves in the store's own project.
// A group of the same name in another project is not one it can grant.
func TestAFileStoreConsumerResolvesInItsOwnProject(t *testing.T) {
	s := newState()
	if code, out := in(t, s.createInstanceGroup, http.MethodPost, "r", "q", nil,
		`{"id":"g","clusterType":"`+inferenceRef+`/versions/`+inferenceV2+`","nodePools":[{"nodePool":"workers","size":2}]}`); code != http.StatusCreated {
		t.Fatalf("group in q: %d %v", code, out)
	}
	code, out := in(t, s.createFileStore, http.MethodPost, "r", "p", nil,
		`{"id":"fs","shareType":"general","capacityGiB":10,"consumers":[{"type":"instanceGroup","ref":"g","network":{"type":"ethernet"}}]}`)
	if code != http.StatusUnprocessableEntity || !strings.Contains(errorMessage(out), "no instance group named g") {
		t.Errorf("a store in p granted q's group: %d %v", code, out)
	}
}

// A create names a project that has to exist. The contract documents the 404
// for createCluster and createFileStore, with the plain
// NotFound envelope and no reason.
func TestACreateInAProjectThatDoesNotExistIs404(t *testing.T) {
	for _, k := range scopedKinds {
		t.Run(k.name, func(t *testing.T) {
			s := newState()
			h := k.routes(s)
			code, out := in(t, h.create, http.MethodPost, "r", "no-such-project", k.path, k.body)
			if code != http.StatusNotFound {
				t.Fatalf("create: %d %v", code, out)
			}
			e, ok := out["error"].(map[string]any)
			if !ok {
				t.Fatalf("no error envelope: %v", out)
			}
			if e["code"] != float64(http.StatusNotFound) || !strings.Contains(errorMessage(out), `project "no-such-project"`) {
				t.Errorf("error = %v", e)
			}
			if _, has := e["reason"]; has {
				t.Errorf("the contract's NotFound carries no reason, got %v", e["reason"])
			}
		})
	}
}

// A selfLink names the project the object is in, not the demo one.
func TestASelfLinkNamesTheProjectTheObjectIsIn(t *testing.T) {
	s := newState()
	for name, create := range map[string]struct {
		h    http.HandlerFunc
		body string
	}{
		"cluster": {s.createCluster, `{"id":"x","clusterType":"` + inferenceRef + `/versions/` + inferenceV2 + `"}`},
		"instance group": {s.createInstanceGroup,
			`{"id":"x","clusterType":"` + inferenceRef + `/versions/` + inferenceV2 + `","nodePools":[{"nodePool":"workers","size":2}]}`},
	} {
		_, out := in(t, create.h, http.MethodPost, "r", "q", nil, create.body)
		if link, ok := out["selfLink"].(string); !ok || !strings.Contains(link, "/projects/q/") {
			t.Errorf("%s selfLink = %v", name, out["selfLink"])
		}
	}
}

// The same id in two projects gets two uids, and a VPC's ownerId still
// matches its own owner's uid rather than the other project's.
func TestTheSameIDInTwoProjectsHasTwoUIDs(t *testing.T) {
	s := newState()
	uids := map[string]map[string]any{}
	for _, project := range []string{"p", "q"} {
		_, c := in(t, s.createCluster, http.MethodPost, "r", project, nil,
			`{"id":"c","clusterType":"`+inferenceRef+`/versions/`+inferenceV2+`"}`)
		_, fs := in(t, s.createFileStore, http.MethodPost, "r", project, nil,
			`{"id":"fs","shareType":"general","capacityGiB":10}`)
		_, vpc := in(t, s.getVPC, http.MethodGet, "r", project, map[string]string{"id": "vpc-c"}, "")
		if vpc["ownerId"] != c["uid"] {
			t.Errorf("%s: the VPC's ownerId %v is not its cluster's uid %v", project, vpc["ownerId"], c["uid"])
		}
		uids[project] = map[string]any{"cluster": c["uid"], "file store": fs["uid"], "vpc": vpc["uid"]}
	}
	for kind, uid := range uids["p"] {
		if uid == nil || uid == uids["q"][kind] {
			t.Errorf("%s: p's uid %v, q's %v", kind, uid, uids["q"][kind])
		}
	}
}

// A remote VPC is a project-scoped path. The same VPC id in another project
// is a peer, and only this project's own VPC is refused as the local one, or
// one that isn't a VPC path in this region.
func TestAPeeringRefusesOnlyTheLocalVPC(t *testing.T) {
	s := newState()
	clusterInP(t, s)
	vpc := map[string]string{"vpcId": "vpc-c"}
	if code, out := in(t, s.createVPCPeering, http.MethodPost, "r", "p", vpc,
		`{"remoteVpc":"/v1/regions/r/projects/q/networking/vpcs/vpc-c"}`); code != http.StatusCreated {
		t.Errorf("the same id in q: %d %v", code, out)
	}
	for _, self := range []string{
		"/v1/regions/r/projects/p/networking/vpcs/vpc-c",
		"https://api.example.invalid/v1/regions/r/projects/p/networking/vpcs/vpc-c/",
		// Not the contract's shape, or not this region.
		"vpc-d",
		"/v1/networking/vpcs/vpc-d",
		"/v1/regions/other/projects/q/networking/vpcs/vpc-c",
	} {
		if code, out := in(t, s.createVPCPeering, http.MethodPost, "r", "p", vpc,
			`{"remoteVpc":"`+self+`"}`); code != http.StatusUnprocessableEntity {
			t.Errorf("%s: %d %v", self, code, out)
		}
	}
}

// What a project delete does to what the project holds, as the contract
// states it for deleteProjectInOrg: it is refused while the project holds
// clusters, and nothing else holds it up. Networks and the rest "key the
// project id and are NOT removed by this call, so they remain addressable
// under the new project" when one is created with the same id.
func TestAProjectDeleteWaitsOnClustersAndLeavesTheRest(t *testing.T) {
	s := newState()
	org := map[string]string{"orgId": "org-demo", "project": "doomed"}
	create := func() {
		t.Helper()
		if code, out := in(t, s.createProjectInOrg, http.MethodPost, "", "", org, `{"id":"doomed","region":"us-texas-1"}`); code != http.StatusCreated {
			t.Fatalf("project: %d %v", code, out)
		}
	}
	create()
	if code, out := in(t, s.createNetwork, http.MethodPost, "r", "doomed", nil,
		`{"id":"n","type":"evpn","evpn":{"type":"l2vpn"}}`); code != http.StatusCreated {
		t.Fatalf("network: %d %v", code, out)
	}
	if code, out := in(t, s.createCluster, http.MethodPost, "r", "doomed", nil,
		`{"id":"c","clusterType":"`+inferenceRef+`/versions/`+inferenceV2+`"}`); code != http.StatusCreated {
		t.Fatalf("cluster: %d %v", code, out)
	}

	if code, out := in(t, s.deleteProjectInOrg, http.MethodDelete, "", "", org, ""); code != http.StatusConflict {
		t.Fatalf("delete with a cluster: %d %v", code, out)
	}
	// Settle the cluster, delete it, and read until it is gone.
	cluster := map[string]string{"id": "c"}
	for range 5 {
		in(t, s.getCluster, http.MethodGet, "r", "doomed", cluster, "")
	}
	if code, out := in(t, s.deleteCluster, http.MethodDelete, "r", "doomed", cluster, ""); code != http.StatusNoContent {
		t.Fatalf("cluster delete: %d %v", code, out)
	}
	for range 5 {
		in(t, s.getCluster, http.MethodGet, "r", "doomed", cluster, "")
	}

	// The network does not hold the delete up.
	if code, out := in(t, s.deleteProjectInOrg, http.MethodDelete, "", "", org, ""); code != http.StatusNoContent {
		t.Fatalf("delete with only a network: %d %v", code, out)
	}
	for range teardownReads {
		in(t, s.getProjectInOrg, http.MethodGet, "", "", org, "")
	}
	if code, _ := in(t, s.getProjectInOrg, http.MethodGet, "", "", org, ""); code != http.StatusNotFound {
		t.Fatalf("the project outlived its teardown: %d", code)
	}

	create()
	if code, out := in(t, s.getNetwork, http.MethodGet, "r", "doomed", map[string]string{"id": "n"}, ""); code != http.StatusOK {
		t.Errorf("the network under the re-created project: %d %v", code, out)
	}
}

// Each fake owns every table it serves, seeds included. What one fake makes,
// changes or deletes, a project, an organization or a webhook subscription,
// is nowhere in the next, which starts from the same seeds as the first did.
func TestEachFakeStartsFromItsOwnSeeds(t *testing.T) {
	used := newState()
	org := map[string]string{"orgId": "org-demo", "project": "demo"}
	if code, out := in(t, used.createProjectInOrg, http.MethodPost, "", "", org, `{"id":"made","region":"us-texas-1"}`); code != http.StatusCreated {
		t.Fatalf("project: %d %v", code, out)
	}
	if code, out := in(t, used.updateProjectInOrg, http.MethodPatch, "", "", org, `{"displayName":"Changed"}`); code != http.StatusOK {
		t.Fatalf("update: %d %v", code, out)
	}
	if code, out := in(t, used.createWebhook, http.MethodPost, "", "", nil, `{"url":"https://hooks.example.test/x"}`); code != http.StatusCreated {
		t.Fatalf("webhook: %d %v", code, out)
	}
	if _, out := in(t, used.listWebhooks, http.MethodGet, "", "", nil, ""); listed(out, "subscriptions") != 1 {
		t.Fatalf("webhooks: %v, want the one made", out)
	}
	delete(used.organizations, "org-empty")

	fresh := newState()
	if _, out := in(t, fresh.getProjectInOrg, http.MethodGet, "", "", org, ""); out["displayName"] != "Demo" {
		t.Errorf("seeded project: %v, want it as seeded", out)
	}
	if code, _ := in(t, fresh.getProjectInOrg, http.MethodGet, "", "", map[string]string{"orgId": "org-demo", "project": "made"}, ""); code != http.StatusNotFound {
		t.Errorf("another fake's project: %d, want 404", code)
	}
	if _, out := in(t, fresh.listWebhooks, http.MethodGet, "", "", nil, ""); listed(out, "subscriptions") != 0 {
		t.Errorf("another fake's webhook: %v, want none", out)
	}
	if code, _ := in(t, fresh.getOrganization, http.MethodGet, "", "", map[string]string{"orgId": "org-empty"}, ""); code != http.StatusOK {
		t.Errorf("seeded organization: %d, want 200", code)
	}
	if got := len(fresh.global().projects); got != len(projectSeeds) {
		t.Errorf("%d projects, want the %d seeds", got, len(projectSeeds))
	}
}
