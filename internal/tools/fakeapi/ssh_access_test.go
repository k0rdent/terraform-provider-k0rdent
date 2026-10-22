package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A k0rdent API build from main echoes an instance group's `sshAccess` sorted by
// username, with each user's keys in the order sent, and leaves the field out
// when none was sent. The provider's drift handling rests on that, so the fake
// answers the same way.

func groupWithSSH(access string) string {
	body := `{"id":"x","clusterType":"` + inferenceRef + `/versions/` + inferenceV2 +
		`","nodePools":[{"nodePool":"workers","size":2}]`
	if access != "" {
		body += `,"sshAccess":` + access
	}
	return body + "}"
}

func TestAGroupsSSHAccessReadsBackSortedByUsername(t *testing.T) {
	s := newState()
	created := groupWithSSH(`[{"username":"zoe","sshPublicKeys":["zed","abe"],"sudo":"ALL=(ALL) NOPASSWD:ALL"},
		{"username":"adam","sshPublicKeys":["abe"],"displayName":"Adam"}]`)
	code, out := in(t, s.createInstanceGroup, http.MethodPost, "r", "p", nil, created)
	if code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, out)
	}
	_, got := in(t, s.getInstanceGroup, http.MethodGet, "r", "p", map[string]string{"id": "x"}, "")
	_, list := in(t, s.listInstanceGroups, http.MethodGet, "r", "p", nil, "")
	items, ok := list["instanceGroups"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("list: %v, want one group", list)
	}
	listedGroup, ok := items[0].(map[string]any)
	if !ok {
		t.Fatalf("listed group: %v", items[0])
	}

	const want = "[map[displayName:Adam sshPublicKeys:[abe] username:adam] " +
		"map[sshPublicKeys:[zed abe] sudo:ALL=(ALL) NOPASSWD:ALL username:zoe]]"
	for name, body := range map[string]map[string]any{"create": out, "get": got, "list": listedGroup} {
		if echo := fmt.Sprint(body["sshAccess"]); echo != want {
			t.Errorf("%s sshAccess = %s, want %s", name, echo, want)
		}
	}
}

// v1.3.0-dev.4 makes sshAccess required on a read: `[]` when the group has
// none, never left out.
func TestAGroupCreatedWithoutSSHAccessEchoesNone(t *testing.T) {
	s := newState()
	if code, out := in(t, s.createInstanceGroup, http.MethodPost, "r", "p", nil, groupWithSSH("")); code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, out)
	}
	_, got := in(t, s.getInstanceGroup, http.MethodGet, "r", "p", map[string]string{"id": "x"}, "")
	if users, ok := got["sshAccess"].([]any); !ok || len(users) != 0 {
		t.Errorf("sshAccess = %v, want []", got["sshAccess"])
	}
}

func TestAGroupsSSHAccessIsCheckedAgainstTheContract(t *testing.T) {
	for name, access := range map[string]string{
		"a username with a capital":        `[{"username":"Zoe","sshPublicKeys":["k"]}]`,
		"a username starting with a digit": `[{"username":"1zoe","sshPublicKeys":["k"]}]`,
		"no keys":                          `[{"username":"zoe","sshPublicKeys":[]}]`,
		"33 keys":                          `[{"username":"zoe","sshPublicKeys":["k"` + strings.Repeat(`,"k"`, 32) + `]}]`,
	} {
		t.Run(name, func(t *testing.T) {
			s := newState()
			code, out := in(t, s.createInstanceGroup, http.MethodPost, "r", "p", nil, groupWithSSH(access))
			if code != http.StatusUnprocessableEntity || !strings.Contains(errorMessage(out), "sshAccess[0]") {
				t.Errorf("create: %d %v, want 422 naming sshAccess[0]", code, out)
			}
		})
	}
}

// clusterSSHAccess is what listClusterSshAccess answers for cluster c.
func clusterSSHAccess(s *state) (int, string) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.SetPathValue("region", "r")
	req.SetPathValue("project", "p")
	req.SetPathValue("id", "c")
	rec := httptest.NewRecorder()
	s.listClusterSSHAccess(rec, req)
	return rec.Code, strings.TrimSpace(rec.Body.String())
}

func clusterWithSSH(access string) string {
	body := `{"id":"c","clusterType":"` + inferenceRef + `/versions/` + inferenceV2 +
		`","nodePools":[{"id":"workers","nodeCount":1}]`
	if access != "" {
		body += `,"sshAccess":` + access
	}
	return body + "}"
}

// A cluster keeps the users its create carried, keys trimmed, and answers
// them at their own path: sorted by username, `[]` for none, and 404 for a
// cluster that doesn't exist. The cluster's own body never carries them.
func TestAClustersSSHAccessIsServedAtItsOwnPath(t *testing.T) {
	for name, tt := range map[string]struct{ access, want string }{
		"two users": {`[{"username":"zoe","sshPublicKeys":["zed\n","abe"],"sudo":"ALL=(ALL) NOPASSWD:ALL"},
			{"username":"adam","sshPublicKeys":["abe"],"displayName":"Adam"}]`,
			`[{"username":"adam","sshPublicKeys":["abe"],"displayName":"Adam"},` +
				`{"username":"zoe","sshPublicKeys":["zed","abe"],"sudo":"ALL=(ALL) NOPASSWD:ALL"}]`},
		"none": {"", "[]"},
	} {
		t.Run(name, func(t *testing.T) {
			s := newState()
			code, out := call(t, s.createCluster, http.MethodPost, "", clusterWithSSH(tt.access))
			if code != http.StatusCreated {
				t.Fatalf("create: %d %v", code, out)
			}
			if _, echoed := out["sshAccess"]; echoed {
				t.Errorf("the cluster's body carries sshAccess: %v", out)
			}
			if code, got := clusterSSHAccess(s); code != http.StatusOK || got != tt.want {
				t.Errorf("ssh-access = %d %s, want 200 %s", code, got, tt.want)
			}
		})
	}
	if code, _ := clusterSSHAccess(newState()); code != http.StatusNotFound {
		t.Errorf("ssh-access of a missing cluster = %d, want 404", code)
	}
}
