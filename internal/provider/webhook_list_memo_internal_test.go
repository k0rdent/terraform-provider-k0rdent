package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// webhookListServer answers the subscription list with n subscriptions and
// counts how often it was asked. failFirst answers the first list with a 500.
func webhookListServer(t *testing.T, n int, failFirst bool) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var lists atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if lists.Add(1) == 1 && failFirst {
			w.WriteHeader(http.StatusInternalServerError)
			if err := json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
				"code": 500, "message": "down", "requestId": "req-1",
			}}); err != nil {
				t.Error(err)
			}
			return
		}
		subs := make([]any, 0, n)
		for i := range n {
			subs = append(subs, map[string]any{
				"id": listedSubscriptionID(i), "url": fmt.Sprintf("https://hooks.example.test/%d", i),
				"event_types": []string{}, "secret_version": 1, "is_active": true,
				"created_at": "2026-10-11T00:00:00Z",
			})
		}
		if err := json.NewEncoder(w).Encode(map[string]any{"subscriptions": subs}); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(server.Close)
	return server, &lists
}

func listedSubscriptionID(i int) string { return fmt.Sprintf("0f8fad5b-d9cb-469f-a165-%012d", i) }

// The contract has no read of one subscription, so each one's refresh reads
// the whole list. Read that way, a configuration of n subscriptions makes n
// lists of n rows on every plan: the quadratic shape docs/performance.md was
// measured to catch. One provider process serves one plan, and a refresh of
// every subscription in it reads the list once.
func TestARefreshOfManySubscriptionsReadsTheListOnce(t *testing.T) {
	const n = 20
	server, lists := webhookListServer(t, n, false)
	r := &webhookSubscriptionResource{meta: testClusterMeta(t, server.URL, time.Minute)}

	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			_, found, diags := r.read(context.Background(), listedSubscriptionID(i), webhookSubscriptionModel{})
			if diags.HasError() || !found {
				t.Errorf("subscription %d: found %v, %v", i, found, diags)
			}
		})
	}
	wg.Wait()
	if got := lists.Load(); got != 1 {
		t.Fatalf("refreshing %d subscriptions read the list %d times, and once answers every one of them", n, got)
	}
}

// A write this provider makes is in the next lookup's list. Without that, a
// create's own read straight after could answer from a list taken before
// the subscription existed, and report the new subscription missing.
func TestAWriteForgetsTheSharedList(t *testing.T) {
	server, lists := webhookListServer(t, 2, false)
	meta := testClusterMeta(t, server.URL, time.Minute)
	ctx := context.Background()

	for range 2 {
		if _, _, diags := findWebhookSubscription(ctx, meta, listedSubscriptionID(0)); diags.HasError() {
			t.Fatal(diags)
		}
	}
	meta.webhookList.forget()
	if _, _, diags := findWebhookSubscription(ctx, meta, listedSubscriptionID(1)); diags.HasError() {
		t.Fatal(diags)
	}
	if got := lists.Load(); got != 2 {
		t.Fatalf("two lookups, a write, then a lookup read the list %d times: once before the write and once after", got)
	}
}

// A list is shared for webhookListFresh and no longer, so a walk that runs
// long doesn't answer its last lookup from its first list.
func TestTheSharedListAges(t *testing.T) {
	server, lists := webhookListServer(t, 1, false)
	meta := testClusterMeta(t, server.URL, time.Minute)
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	meta.webhookList.now = func() time.Time { return now }
	ctx := context.Background()

	lookup := func() {
		t.Helper()
		if _, found, diags := findWebhookSubscription(ctx, meta, listedSubscriptionID(0)); diags.HasError() || !found {
			t.Fatalf("found %v, %v", found, diags)
		}
	}
	lookup()
	now = now.Add(webhookListFresh - time.Millisecond)
	lookup()
	if got := lists.Load(); got != 1 {
		t.Fatalf("a lookup inside the window read the list again (%d reads)", got)
	}
	now = now.Add(time.Millisecond)
	lookup()
	if got := lists.Load(); got != 2 {
		t.Fatalf("a lookup %s after the list was read used it anyway (%d reads)", webhookListFresh, got)
	}
}

// A list that failed answers only the lookup that asked for it. The next one
// asks again rather than inheriting the error, or being told nothing exists.
func TestAFailedListIsNotShared(t *testing.T) {
	server, lists := webhookListServer(t, 1, true)
	meta := testClusterMeta(t, server.URL, time.Minute)
	ctx := context.Background()

	if _, _, diags := findWebhookSubscription(ctx, meta, listedSubscriptionID(0)); !diags.HasError() {
		t.Fatal("the first list answered 500, and the lookup reported no error")
	}
	if _, found, diags := findWebhookSubscription(ctx, meta, listedSubscriptionID(0)); diags.HasError() || !found {
		t.Fatalf("the lookup after a failed list: found %v, %v", found, diags)
	}
	if got := lists.Load(); got != 2 {
		t.Fatalf("the list was read %d times, and the failed one can't answer the second lookup", got)
	}
}

// A list in flight when a write lands is never kept, and a lookup after the
// write doesn't join it. Otherwise a create racing another lookup could be
// answered by a list the platform built before the create.
func TestAListInFlightDuringAWriteIsNotKept(t *testing.T) {
	release := make(chan struct{})
	var lists atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// The list from before the write is empty. Every later one has the
		// subscription the write made.
		subs := []any{map[string]any{
			"id": listedSubscriptionID(0), "url": "https://hooks.example.test/0", "event_types": []string{},
			"secret_version": 1, "is_active": true, "created_at": "2026-10-11T00:00:00Z",
		}}
		if lists.Add(1) == 1 {
			<-release
			subs = []any{}
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{"subscriptions": subs}); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(server.Close)
	meta := testClusterMeta(t, server.URL, time.Minute)
	ctx := context.Background()

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _, _ = findWebhookSubscription(ctx, meta, listedSubscriptionID(0))
	}()
	for lists.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	meta.webhookList.forget()

	// The first list is still held, and a lookup after the write sends its
	// own rather than joining it.
	_, _, _ = findWebhookSubscription(ctx, meta, listedSubscriptionID(0))
	if got := lists.Load(); got != 2 {
		t.Fatalf("the lookup after the write sent %d lists in all; it joined the one in flight during the write", got)
	}
	close(release)
	<-done

	// The list from before the write finished last, and wasn't stored over
	// the one from after it.
	_, found, _ := findWebhookSubscription(ctx, meta, listedSubscriptionID(0))
	if !found {
		t.Fatal("a lookup after both lists finished was answered by the list from before the write")
	}
}

// A create reads its subscription back from a list taken after it, even when
// this run already holds one from before. One apply creates every new
// subscription in a configuration, so the second create's read would
// otherwise be answered by the first one's list and report itself missing.
func TestACreateIsReadFromAListTakenAfterIt(t *testing.T) {
	server := webhookServer(t, http.StatusCreated, "application/json",
		`{"id":"`+testSubscriptionID+`","signing_secret":"s","created_at":"2026-10-11T00:00:00Z"}`, testSubscriptionID)
	defer server.Close()
	r := &webhookSubscriptionResource{meta: testClusterMeta(t, server.URL, time.Minute)}
	if _, found, diags := findWebhookSubscription(context.Background(), r.meta, testSubscriptionID); diags.HasError() || found {
		t.Fatalf("before the create: found %v, %v", found, diags)
	}

	unknown := func(typ tftypes.Type) tftypes.Value { return tftypes.NewValue(typ, tftypes.UnknownValue) }
	planned, s := testValue(t, r, map[string]tftypes.Value{
		"id": unknown(tftypes.String), "url": str("https://hooks.example.test/x"),
		"secret_wo_version": num(1), "rotation_overlap": tftypes.NewValue(tftypes.Bool, true),
		"secret_version": unknown(tftypes.Number), "is_active": unknown(tftypes.Bool),
		"created_at": unknown(tftypes.String), "rotated_at": unknown(tftypes.String),
		"previous_secret_expires_at": unknown(tftypes.String),
	})
	config, _ := testValue(t, r, map[string]tftypes.Value{
		"url": str("https://hooks.example.test/x"), "secret_wo_version": num(1),
		"secret_wo": str(strings.Repeat("n", 32)),
	})
	empty, _ := testValue(t, r, nil)
	resp := resource.CreateResponse{State: tfsdk.State{Raw: empty, Schema: s.Schema}}
	r.Create(context.Background(), resource.CreateRequest{
		Plan:   tfsdk.Plan{Raw: planned, Schema: s.Schema},
		Config: tfsdk.Config{Raw: config, Schema: s.Schema},
	}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("the create read itself from the list it had before: %v", resp.Diagnostics)
	}
}

// Lookups that arrive while a list is failing share its answer, error and
// all. Waiting their turn to each send their own, behind a platform that
// answers 503 for a minute of retries, would make a refresh of thirty
// subscriptions take half an hour.
func TestConcurrentLookupsShareAFailingList(t *testing.T) {
	const n = 10
	var lists atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		lists.Add(1)
		time.Sleep(200 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		if err := json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
			"code": 500, "message": "down", "requestId": "req-1",
		}}); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(server.Close)
	meta := testClusterMeta(t, server.URL, time.Minute)

	start := time.Now()
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() {
			if _, _, diags := findWebhookSubscription(context.Background(), meta, listedSubscriptionID(0)); !diags.HasError() {
				t.Error("a lookup behind a failing list reported no error")
			}
		})
	}
	wg.Wait()
	if got, took := lists.Load(), time.Since(start); got != 1 || took > time.Second {
		t.Fatalf("%d concurrent lookups behind a failing list sent %d lists and took %s, and one list answers them all", n, got, took)
	}
}
