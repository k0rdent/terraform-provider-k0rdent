package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A cluster named for busyUntilRefused stays `creating` until a delete is
// refused against it, then settles `active` on its own, with no cancel, and
// deletes like any other.
func TestABusyClusterFinishesOnceADeleteIsRefused(t *testing.T) {
	s := newState()
	body := `{"id":"c","clusterType":"` + inferenceRef + `/versions/` + inferenceV2 +
		`","displayName":"` + busyUntilRefused + `"}`
	if code, out := call(t, s.createCluster, http.MethodPost, "", body); code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, out)
	}
	for range 10 {
		if _, out := read(t, s); out["state"] != "creating" {
			t.Fatalf("settled to %v before anything was refused", out["state"])
		}
	}
	if code, out := call(t, s.deleteCluster, http.MethodDelete, "c", ""); code != http.StatusConflict {
		t.Fatalf("delete while creating: %d %v", code, out)
	}
	settled := false
	for range settleAfter + 1 {
		if _, out := read(t, s); out["state"] == "active" {
			settled = true
			break
		}
	}
	if !settled {
		t.Fatal("the cluster didn't settle after the refused delete")
	}
	if code, out := call(t, s.deleteCluster, http.MethodDelete, "c", ""); code != http.StatusNoContent {
		t.Fatalf("delete once settled: %d %v", code, out)
	}
}

// A store named for busyUntilRefused stays `creating` until an update is
// refused against it with CONFLICT_IN_USE. Once it settles its etag has moved,
// so the update sent again with the etag read while it was `creating` is
// refused as a concurrent edit, and one with the etag read since is taken.
func TestABusyStoreSettlesWithANewEtag(t *testing.T) {
	s := newState()
	code, created := in(t, s.createFileStore, http.MethodPost, "r", "p", nil,
		`{"id":"fs","shareType":"general","capacityGiB":10,"displayName":"`+busyUntilRefused+`"}`)
	if code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, created)
	}
	readStore := func() map[string]any {
		t.Helper()
		_, out := call(t, s.getFileStore, http.MethodGet, "fs", "")
		return out
	}
	var before map[string]any
	for range 10 {
		if before = readStore(); before["state"] != "creating" {
			t.Fatalf("settled to %v before anything was refused", before["state"])
		}
	}
	stale := fmt.Sprint(before["etag"])
	update := func(etag string) (int, map[string]any) {
		return call(t, s.updateFileStore, http.MethodPatch, "fs", `{"description":"later","etag":"`+etag+`"}`)
	}
	if code, out := update(stale); code != http.StatusConflict || detailReason(out) != "CONFLICT_IN_USE" {
		t.Fatalf("update while creating: %d %v", code, out)
	}
	var after map[string]any
	for range settleAfter + 1 {
		if after = readStore(); after["state"] == "active" {
			break
		}
	}
	if after["state"] != "active" {
		t.Fatalf("the store didn't settle after the refused update: %v", after["state"])
	}
	fresh := fmt.Sprint(after["etag"])
	if fresh == stale {
		t.Fatalf("the etag stayed %q across the settle", stale)
	}
	if code, out := update(stale); code != http.StatusConflict || detailReason(out) != "CONFLICT_VERSION" {
		t.Errorf("update with the etag from before the settle: %d %v", code, out)
	}
	if code, out := update(fresh); code != http.StatusOK {
		t.Errorf("update with the etag read since: %d %v", code, out)
	}
}

// A cluster named for busyAtFirstUpgrade is busy with another run's operation
// when its first upgrade arrives, which is refused with the 409 a cluster that
// isn't `active` answers. Once that operation settles, the same upgrade is
// taken, and no later upgrade meets it again.
func TestTheFirstUpgradeMeetsAnotherRunsOperation(t *testing.T) {
	s := newState()
	body := `{"id":"c","clusterType":"` + inferenceRef + `/versions/` + inferenceV1 +
		`","displayName":"` + busyAtFirstUpgrade + `"}`
	if code, out := call(t, s.createCluster, http.MethodPost, "", body); code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, out)
	}
	settle := func() {
		t.Helper()
		for range settleAfter + 1 {
			if _, out := read(t, s); out["state"] == "active" {
				return
			}
		}
		t.Fatal("the cluster didn't settle")
	}
	settle()
	upgrade := `{"clusterTypeVersionId":"` + inferenceV2 + `"}`
	if code, out := call(t, s.upgradeCluster, http.MethodPost, "c", upgrade); code != http.StatusConflict {
		t.Fatalf("first upgrade: %d %v, want 409", code, out)
	}
	if _, out := read(t, s); out["state"] != "updating" {
		t.Fatalf("after the refused upgrade the cluster reads %v, want updating", out["state"])
	}
	settle()
	if code, out := call(t, s.upgradeCluster, http.MethodPost, "c", upgrade); code != http.StatusOK {
		t.Fatalf("the upgrade once settled: %d %v", code, out)
	}
}

// The harness reads what the provider sent from the fake's own count, behind
// the owner token, and the count includes a request the moment it's served.
func TestTheFakeCountsWhatItServes(t *testing.T) {
	h, secrets := guardedFake(t)
	count := func(bearer string) (int, map[string]any) {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet,
			"/_harness/requests?method=GET&path=/v1/regions/r/projects/p/compute/clusters/nope", nil)
		req.Host = "127.0.0.1:" + testPort
		req.Header.Set("Authorization", "Bearer "+bearer)
		rec := serve(h, req)
		var out map[string]any
		if rec.Code == http.StatusOK {
			if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
				t.Fatalf("count body %q: %v", rec.Body.String(), err)
			}
		}
		return rec.Code, out
	}
	if code, _ := count(secrets.AccessToken); code != http.StatusUnauthorized {
		t.Errorf("the count answered %d to a tenant bearer, want 401", code)
	}
	for want := range 3 {
		code, out := count(secrets.OwnerToken)
		if code != http.StatusOK || out["count"] != float64(want) {
			t.Fatalf("count = %d %v, want %d", code, out, want)
		}
		req := httptest.NewRequest(http.MethodGet, "/v1/regions/r/projects/p/compute/clusters/nope", nil)
		req.Host = "127.0.0.1:" + testPort
		serve(h, req)
	}
}
