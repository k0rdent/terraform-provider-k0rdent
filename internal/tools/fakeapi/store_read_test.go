package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Once a project is gone the API answers the gateway's 403 for anything that
// was in it, deleted or not. The harness's store read still tells the two
// apart, for the owner token only.
func TestTheHarnessReadsTheStorePastTheGateway(t *testing.T) {
	s := newState()
	h, secrets := guardedFakeOf(t, s)
	send := func(method, path, bearer, body string) int {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Host = "127.0.0.1:" + testPort
		req.Header.Set("Authorization", "Bearer "+bearer)
		return serve(h, req).Code
	}

	const project = "store-read-gone"
	s.global().projects[at{id: project}] = &fakeProject{id: project, org: "org-demo", status: "active"}
	under := "/v1/regions/r/projects/" + project + "/networking/networks"
	if code := send(http.MethodPost, under, secrets.AccessToken, `{"id":"n","type":"evpn","evpn":{"type":"l2vpn"}}`); code != http.StatusCreated {
		t.Fatalf("create: %d", code)
	}
	// The project goes and the network is left behind in it.
	delete(s.global().projects, at{id: project})

	if code := send(http.MethodGet, under+"/n", secrets.AccessToken, ""); code != http.StatusForbidden {
		t.Fatalf("the API answered %d, want the gateway's 403", code)
	}
	for _, c := range []struct {
		path, bearer string
		want         int
	}{
		{under + "/n", secrets.OwnerToken, http.StatusOK},
		{under + "/never-made", secrets.OwnerToken, http.StatusNotFound},
		{under + "/n", secrets.AccessToken, http.StatusUnauthorized},
		{under + "/n", "", http.StatusUnauthorized},
	} {
		if code := send(http.MethodGet, "/_harness/store"+c.path, c.bearer, ""); code != c.want {
			t.Errorf("store read of %s: %d, want %d", c.path, code, c.want)
		}
	}
	// It reads and nothing else.
	if code := send(http.MethodDelete, "/_harness/store"+under+"/n", secrets.OwnerToken, ""); code == http.StatusNoContent {
		t.Error("the store read took a DELETE")
	}
}
