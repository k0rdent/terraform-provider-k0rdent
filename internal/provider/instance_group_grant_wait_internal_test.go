package provider

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type grantWaitResponse struct {
	status int
	body   string
}

func grantWaitServer(t *testing.T, memberCount int, responses ...grantWaitResponse) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var storeReads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/compute/instance-groups/g1"):
			fmt.Fprintf(w, `{"id":"g1","clusterType":"t","state":"active","nodePools":[{"nodePool":"workers","desiredCount":%d}]}`, memberCount)
		case strings.HasSuffix(r.URL.Path, "/storage/file-stores/fs1"):
			i := int(storeReads.Add(1)) - 1
			if i >= len(responses) {
				i = len(responses) - 1
			}
			response := responses[i]
			if response.status == 0 {
				response.status = http.StatusOK
			}
			w.WriteHeader(response.status)
			fmt.Fprint(w, response.body)
		case strings.HasSuffix(r.URL.Path, "/storage/file-stores"):
			fmt.Fprint(w, `{"fileStores":[{"id":"fs1","consumers":[{"ref":"g1","type":"instanceGroup","network":{"type":"ethernet"}}]}]}`)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return server, &storeReads
}

func runConsumerGrantWait(t *testing.T, server *httptest.Server, timeout time.Duration) error {
	t.Helper()
	meta := testClusterMeta(t, server.URL, timeout)
	meta.Wait.Interval = 5 * time.Millisecond
	r := &instanceGroupResource{meta: meta}
	return r.waitForConsumerGrants(context.Background(), clusterTarget{region: "test", project: "test"}, "g1")
}

func consumerGrantStore(state, reason string, addresses ...string) string {
	return consumerGrantStoreOnStore("active", state, reason, addresses...)
}

func consumerGrantStoreOnStore(storeState, grantState, reason string, addresses ...string) string {
	body := fmt.Sprintf(`{"id":"fs1","state":%q,"consumers":[{"ref":"g1","type":"instanceGroup","network":{"type":"ethernet"}}],`+
		`"consumerStatus":[{"ref":"g1","state":%q,"resolvedAddresses":[`, storeState, grantState)
	for i, address := range addresses {
		if i > 0 {
			body += ","
		}
		body += fmt.Sprintf("%q", address)
	}
	body += `]`
	if reason != "" {
		body += fmt.Sprintf(`,"stateReason":%q`, reason)
	}
	return body + `}]}`
}

func TestInstanceGroupGrantWaitObservesDelayedRepair(t *testing.T) {
	server, reads := grantWaitServer(t, 2,
		grantWaitResponse{body: consumerGrantStore("active", "", "10.80.11.1")},
		grantWaitResponse{body: consumerGrantStore("creating", "")},
		grantWaitResponse{body: consumerGrantStore("active", "", "192.0.2.10", "192.0.2.20")},
	)
	if err := runConsumerGrantWait(t, server, time.Second); err != nil {
		t.Fatalf("wait: %v", err)
	}
	if got := reads.Load(); got != 3 {
		t.Fatalf("store reads = %d, want 3 so the wait sees active, creating, active", got)
	}
}

func TestInstanceGroupGrantWaitReturnsWhenNoRepairIsNeeded(t *testing.T) {
	server, reads := grantWaitServer(t, 2,
		grantWaitResponse{body: consumerGrantStore("active", "", "192.0.2.10", "192.0.2.20")})
	if err := runConsumerGrantWait(t, server, time.Second); err != nil {
		t.Fatalf("wait: %v", err)
	}
	if got := reads.Load(); got != 1 {
		t.Fatalf("store reads = %d, want 1", got)
	}
}

func TestInstanceGroupGrantWaitStopsOnVerdict(t *testing.T) {
	for _, reason := range []string{
		"NETWORK_MATCHED_NO_INTERFACE",
		"GROUP_NO_LONGER_GRANTABLE",
		"GROUP_MEMBER_SET_EMPTY",
		"STORAGE_UNAVAILABLE",
	} {
		t.Run(reason, func(t *testing.T) {
			server, reads := grantWaitServer(t, 2,
				grantWaitResponse{body: consumerGrantStore("creating", reason, "10.80.11.1")})
			if err := runConsumerGrantWait(t, server, 80*time.Millisecond); err != nil {
				t.Fatalf("wait: %v", err)
			}
			if got := reads.Load(); got != 1 {
				t.Fatalf("store reads = %d, want 1", got)
			}
		})
	}
}

func TestInstanceGroupGrantWaitStopsOnFailedGrant(t *testing.T) {
	server, reads := grantWaitServer(t, 2,
		grantWaitResponse{body: consumerGrantStore("failed", "GROUP_MEMBERSHIP_DRIFTED", "10.80.11.1")})
	if err := runConsumerGrantWait(t, server, time.Second); err != nil {
		t.Fatalf("wait: %v", err)
	}
	if got := reads.Load(); got != 1 {
		t.Fatalf("store reads = %d, want 1", got)
	}
}

func TestInstanceGroupGrantWaitIgnoresDeletedStore(t *testing.T) {
	server, _ := grantWaitServer(t, 2, grantWaitResponse{
		status: http.StatusNotFound,
		body:   `{"error":{"code":404,"message":"file store fs1 not found","requestId":"req-gone"}}`,
	})
	if err := runConsumerGrantWait(t, server, time.Second); err != nil {
		t.Fatalf("a store deleted between list and read holds the scaled group: %v", err)
	}
}

func TestInstanceGroupGrantWaitSkipsStoreBeingDeleted(t *testing.T) {
	server, reads := grantWaitServer(t, 2,
		grantWaitResponse{body: consumerGrantStoreOnStore("deleting", "active", "", "192.0.2.10")})
	if err := runConsumerGrantWait(t, server, 80*time.Millisecond); err != nil {
		t.Fatalf("a store being deleted holds the scaled group: %v", err)
	}
	if got := reads.Load(); got != 1 {
		t.Fatalf("store reads = %d, want 1", got)
	}
}

func TestInstanceGroupGrantWaitStopsOnGoingAwayGrant(t *testing.T) {
	for _, state := range []string{"deleting"} {
		t.Run(state, func(t *testing.T) {
			server, reads := grantWaitServer(t, 2,
				grantWaitResponse{body: consumerGrantStore(state, "", "192.0.2.10")})
			if err := runConsumerGrantWait(t, server, 80*time.Millisecond); err != nil {
				t.Fatalf("a grant going away holds the scaled group: %v", err)
			}
			if got := reads.Load(); got != 1 {
				t.Fatalf("store reads = %d, want 1", got)
			}
		})
	}
}

func TestInstanceGroupGrantWaitTimeoutNamesStore(t *testing.T) {
	server, reads := grantWaitServer(t, 2,
		grantWaitResponse{body: consumerGrantStore("creating", "", "10.80.11.1")})
	err := runConsumerGrantWait(t, server, time.Second)
	if err == nil {
		t.Fatal("wait succeeded, want a timeout")
	}
	if reads.Load() == 0 {
		t.Fatal("the wait timed out before reading the store")
	}
	for _, want := range []string{"fs1", "g1", "Read file store"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("timeout does not name %q:\n%s", want, err)
		}
	}
}
