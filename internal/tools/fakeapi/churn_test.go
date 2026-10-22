package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fixedAnswer is a handler that answers the same body every time, which is
// what every read handler in the fake does for an unchanged object.
func fixedAnswer(body string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write([]byte(body)); err != nil {
			panic(err)
		}
	})
}

func churnRead(t *testing.T, h http.Handler, method string) (int, string, http.Header) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, "/v1/regions/r/projects/p/compute/instance-groups/x", nil))
	return rec.Code, rec.Body.String(), rec.Header()
}

const churnedGroup = `{"id":"acc-hosts-fake-churns","state":"active","updatedAt":"2026-09-21T00:00:00Z",` +
	`"progressUpdatedAt":"2026-09-21T00:00:00Z",` +
	`"nodePools":[{"nodePool":"a","desiredCount":2,"readyCount":2,"notReadyCount":0},` +
	`{"nodePool":"b","desiredCount":1,"readyCount":1,"notReadyCount":0}]}`

// An object without the marker has to reach the provider byte for byte, or
// every other test in the suite is reading something this file changed.
func TestChurnLeavesAnUnmarkedAnswerAlone(t *testing.T) {
	const body = `{"id":"acc-hosts","updatedAt":"2026-09-21T00:00:00Z","nodePools":[{"nodePool":"a","desiredCount":2}]}` + "\n"
	h := churning(fixedAnswer(body))
	for range 3 {
		code, got, header := churnRead(t, h, http.MethodGet)
		if code != http.StatusOK || got != body {
			t.Fatalf("answered %d %q, want 200 %q", code, got, body)
		}
		if header.Get("Content-Type") != "application/json" {
			t.Errorf("Content-Type = %q", header.Get("Content-Type"))
		}
	}
}

// A mutation answers what it answers. Only a read is a sample of observed
// state, so only a read is varied.
func TestChurnLeavesAWriteAlone(t *testing.T) {
	h := churning(fixedAnswer(churnedGroup))
	if _, got, _ := churnRead(t, h, http.MethodPatch); got != churnedGroup {
		t.Errorf("a PATCH answer was changed to %q", got)
	}
}

// The whole point: two reads of the same unchanged object differ, in the
// observed fields only, and the tenant's own number never moves.
func TestChurnVariesObservedFieldsAndNothingTheTenantSet(t *testing.T) {
	h := churning(fixedAnswer(churnedGroup))
	seen := map[string]bool{}
	sawMissingCounts, sawNotReady, sawReversed := false, false, false
	for range 6 {
		_, got, _ := churnRead(t, h, http.MethodGet)
		seen[got] = true

		var g struct {
			ID        string `json:"id"`
			State     string `json:"state"`
			UpdatedAt string `json:"updatedAt"`
			NodePools []struct {
				NodePool      string `json:"nodePool"`
				DesiredCount  int    `json:"desiredCount"`
				ReadyCount    *int   `json:"readyCount"`
				NotReadyCount *int   `json:"notReadyCount"`
			} `json:"nodePools"`
		}
		if err := json.Unmarshal([]byte(got), &g); err != nil {
			t.Fatalf("decode %q: %v", got, err)
		}
		if g.ID != "acc-hosts-fake-churns" || g.State != "active" {
			t.Errorf("id or state moved: %q", got)
		}
		want := map[string]int{"a": 2, "b": 1}
		for _, p := range g.NodePools {
			if p.DesiredCount != want[p.NodePool] {
				t.Errorf("pool %s desiredCount = %d, want %d: only a create or a scale changes it",
					p.NodePool, p.DesiredCount, want[p.NodePool])
			}
			switch {
			case p.ReadyCount == nil && p.NotReadyCount == nil:
				sawMissingCounts = true
			case p.ReadyCount == nil || p.NotReadyCount == nil:
				t.Errorf("pool %s carries one count without the other: %q", p.NodePool, got)
			case *p.ReadyCount+*p.NotReadyCount != want[p.NodePool]:
				t.Errorf("pool %s holds %d+%d hosts, want %d", p.NodePool, *p.ReadyCount, *p.NotReadyCount, want[p.NodePool])
			case *p.NotReadyCount > 0:
				sawNotReady = true
			}
		}
		if len(g.NodePools) == 2 && g.NodePools[0].NodePool == "b" {
			sawReversed = true
		}
	}
	if len(seen) < 3 {
		t.Errorf("six reads answered %d distinct bodies, want at least 3", len(seen))
	}
	if !sawMissingCounts || !sawNotReady || !sawReversed {
		t.Errorf("not every shape appeared in six reads: counts missing %v, a host not ready %v, pools reordered %v",
			sawMissingCounts, sawNotReady, sawReversed)
	}
}

// A list answer varies each marked row and leaves the others alone.
func TestChurnReachesARowInAList(t *testing.T) {
	const body = `{"clusters":[{"id":"acc-cluster-fake-churns","updatedAt":"2026-09-21T00:00:00Z","health":{"state":"unknown"}},` +
		`{"id":"acc-cluster","updatedAt":"2026-09-21T00:00:00Z","health":{"state":"unknown"}}]}`
	h := churning(fixedAnswer(body))
	_, first, _ := churnRead(t, h, http.MethodGet)
	_, second, _ := churnRead(t, h, http.MethodGet)
	if first == second {
		t.Fatalf("two reads of a list with a marked row answered the same body %q", first)
	}
	for _, got := range []string{first, second} {
		var out struct {
			Clusters []map[string]any `json:"clusters"`
		}
		if err := json.Unmarshal([]byte(got), &out); err != nil {
			t.Fatal(err)
		}
		for _, c := range out.Clusters {
			id, ok := c["id"].(string)
			if !ok {
				t.Fatalf("a row with no id: %v", c)
			}
			if !strings.Contains(id, churnsOnEveryRead) && c["updatedAt"] != fixedTime {
				t.Errorf("the unmarked row was varied: %v", c)
			}
		}
	}
}

// churnEpoch is written out rather than parsed, so it has to stay fixedTime.
func TestChurnCountsFromTheFakesFixedTime(t *testing.T) {
	if got := churnTime(0); got != fixedTime {
		t.Errorf("churnTime(0) = %s, want fixedTime %s", got, fixedTime)
	}
}

const churnedCluster = `{"id":"acc-cluster-fake-churns","state":"active","updatedAt":"2026-09-21T00:00:00Z",` +
	`"health":{"state":"unknown"},` +
	`"controlPlane":{"hosted":{"state":"ready","desiredReplicas":3,"readyReplicas":3,"unavailableReplicas":0,` +
	`"message":"3 of 3 control-plane replicas are ready","observedAt":"2026-09-21T00:00:00Z"}},` +
	`"nodePools":[{"id":"gpu-workers","nodeCount":2,"createdAt":"2026-09-21T00:00:00Z","updatedAt":"2026-09-21T00:00:00Z"}]}`

// A marked object's nested entries move too: a pool's own `updatedAt` is a
// timestamp like any other, and a provider that read it would plan a change
// nobody made just as surely as one reading the top-level one.
func TestChurnMovesNestedTimestamps(t *testing.T) {
	h := churning(fixedAnswer(churnedCluster))
	moved := false
	for range 4 {
		_, got, _ := churnRead(t, h, http.MethodGet)
		var c struct {
			NodePools []struct {
				NodeCount int    `json:"nodeCount"`
				CreatedAt string `json:"createdAt"`
				UpdatedAt string `json:"updatedAt"`
			} `json:"nodePools"`
		}
		if err := json.Unmarshal([]byte(got), &c); err != nil {
			t.Fatal(err)
		}
		for _, p := range c.NodePools {
			if p.NodeCount != 2 || p.CreatedAt != fixedTime {
				t.Errorf("a pool's count or createdAt moved: %s", got)
			}
			if p.UpdatedAt != fixedTime {
				moved = true
			}
		}
	}
	if !moved {
		t.Error("a pool's updatedAt never moved")
	}
}

// The observed state the contract says moves with no write: a cluster that
// the watchdog force-fails "may still return to `active` with no user
// action", health carries a staleness boundary, the control plane's ready
// replicas are sampled, and a pool can carry a target for a scale that was
// force-failed until its retry window retires.
func TestChurnMovesObservedState(t *testing.T) {
	h := churning(fixedAnswer(churnedCluster))
	var sawFailed, sawActive, sawExpiry, sawReplicaDip, sawTarget bool
	for range 6 {
		_, got, _ := churnRead(t, h, http.MethodGet)
		var c struct {
			State        string `json:"state"`
			StateMessage string `json:"stateMessage"`
			Health       struct {
				State     string  `json:"state"`
				ExpiresAt *string `json:"expiresAt"`
			} `json:"health"`
			ControlPlane struct {
				Hosted struct {
					ReadyReplicas       int `json:"readyReplicas"`
					UnavailableReplicas int `json:"unavailableReplicas"`
				} `json:"hosted"`
			} `json:"controlPlane"`
			NodePools []struct {
				NodeCount       int  `json:"nodeCount"`
				TargetNodeCount *int `json:"targetNodeCount"`
			} `json:"nodePools"`
		}
		if err := json.Unmarshal([]byte(got), &c); err != nil {
			t.Fatal(err)
		}
		switch c.State {
		case "failed":
			sawFailed = true
			if c.StateMessage == "" {
				t.Errorf("failed with no stateMessage: %s", got)
			}
		case "active":
			sawActive = true
			if c.StateMessage != "" {
				t.Errorf("active with a stateMessage: %s", got)
			}
		default:
			t.Errorf("state %q", c.State)
		}
		if c.Health.State != "unknown" && c.Health.ExpiresAt != nil {
			sawExpiry = true
		}
		if hosted := c.ControlPlane.Hosted; hosted.ReadyReplicas+hosted.UnavailableReplicas != 3 {
			t.Errorf("replicas don't add up to 3: %s", got)
		} else if hosted.ReadyReplicas < 3 {
			sawReplicaDip = true
		}
		for _, p := range c.NodePools {
			if p.TargetNodeCount != nil {
				sawTarget = true
				if *p.TargetNodeCount != p.NodeCount {
					t.Errorf("a target that isn't the settled count: %s", got)
				}
			}
		}
	}
	if !sawFailed || !sawActive || !sawExpiry || !sawReplicaDip || !sawTarget {
		t.Errorf("not every shape appeared: failed %v, active %v, expiry %v, replica dip %v, pool target %v",
			sawFailed, sawActive, sawExpiry, sawReplicaDip, sawTarget)
	}
}

// A file store's etag names its revision, and a server-driven transition is
// a revision, so it moves as `updatedAt` does.
func TestChurnMovesAFileStoresEtag(t *testing.T) {
	const store = `{"id":"acc-store-fake-churns","state":"active","etag":"rev-1","updatedAt":"2026-09-21T00:00:00Z"}`
	h := churning(fixedAnswer(store))
	seen := map[string]bool{}
	for range 3 {
		_, got, _ := churnRead(t, h, http.MethodGet)
		var fs struct {
			Etag string `json:"etag"`
		}
		if err := json.Unmarshal([]byte(got), &fs); err != nil {
			t.Fatal(err)
		}
		seen[fs.Etag] = true
	}
	if len(seen) < 2 {
		t.Errorf("three reads answered etags %v", seen)
	}
}
