package provider

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

const testDeliveriesSubscription = "0f8fad5b-d9cb-469f-a165-70867728950e"

// maxDeliveryRequests is more delivery requests than any read here needs.
// Past it the server fails the test and answers an error, so a read that
// doesn't stop fails at once rather than when the test times out.
const maxDeliveryRequests = 10

// deliveriesAPI serves one subscription and its delivery history, newest
// first, at most page rows a page whatever `limit` asks for. It records the
// `limit` and `cursor` of every delivery request.
type deliveriesAPI struct {
	page       int
	deliveries []map[string]any
	// repeat answers every page with the first one and a cursor, the way a
	// server whose cursor doesn't advance would.
	repeat bool
	// overfill answers a full page whatever `limit` asks for.
	overfill bool

	mu       sync.Mutex
	limits   []int
	cursors  []string
	requests int
}

func (a *deliveriesAPI) serve(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const hooks = "/v1/regions/global/notifications/webhooks"
		if r.URL.Path == hooks {
			writeTestJSON(t, w, http.StatusOK, map[string]any{"subscriptions": []any{
				map[string]any{"id": testDeliveriesSubscription, "url": "https://hooks.example.test/x"},
			}})
			return
		}
		if r.URL.Path != hooks+"/"+testDeliveriesSubscription+"/deliveries" {
			writeTestError(t, w, http.StatusNotFound, "no route")
			return
		}
		limit, err := strconv.Atoi(r.URL.Query().Get("limit"))
		if err != nil || limit < 1 || limit > 200 {
			writeTestError(t, w, http.StatusBadRequest, "limit out of range")
			return
		}
		cursor := r.URL.Query().Get("cursor")
		a.mu.Lock()
		a.requests++
		a.limits = append(a.limits, limit)
		a.cursors = append(a.cursors, cursor)
		requests := a.requests
		a.mu.Unlock()
		if requests > maxDeliveryRequests {
			t.Errorf("delivery request %d: the read didn't stop", requests)
			writeTestError(t, w, http.StatusInternalServerError, "too many requests")
			return
		}

		start := 0
		if cursor != "" && !a.repeat {
			if start, err = strconv.Atoi(cursor); err != nil {
				writeTestError(t, w, http.StatusBadRequest, "not a cursor this server issued")
				return
			}
		}
		size := min(limit, a.page)
		if a.overfill {
			size = a.page
		}
		end := min(start+size, len(a.deliveries))
		body := map[string]any{"deliveries": a.deliveries[start:end]}
		if end < len(a.deliveries) {
			body["next_cursor"] = strconv.Itoa(end)
		}
		writeTestJSON(t, w, http.StatusOK, body)
	}))
	t.Cleanup(server.Close)
	return server
}

// testDeliveries is n deliveries, newest first, every third one failed.
func testDeliveries(n int) []map[string]any {
	out := make([]map[string]any, n)
	for i := range out {
		status := "delivered"
		if i%3 == 1 {
			status = "failed"
		}
		out[i] = map[string]any{
			"id":              fmt.Sprintf("00000000-0000-4000-8000-%012d", i),
			"status":          status,
			"attempts":        1,
			"created_at":      time.Date(2026, 10, 26, 12, 0, n-i, 0, time.UTC).Format(time.RFC3339),
			"retry_scheduled": false,
		}
	}
	return out
}

func readDeliveries(t *testing.T, api *deliveriesAPI, set map[string]tftypes.Value) (webhookDeliveriesModel, datasource.ReadResponse) {
	t.Helper()
	meta := testClusterMeta(t, api.serve(t).URL, time.Minute)
	if set == nil {
		set = map[string]tftypes.Value{}
	}
	if _, ok := set["subscription_id"]; !ok {
		set["subscription_id"] = tftypes.NewValue(tftypes.String, testDeliveriesSubscription)
	}
	resp := readTestDataSource(t, &webhookDeliveriesDataSource{meta: meta}, set)
	var got webhookDeliveriesModel
	if !resp.Diagnostics.HasError() {
		resp.Diagnostics.Append(resp.State.Get(context.Background(), &got)...)
	}
	return got, resp
}

func deliveryIDs(m webhookDeliveriesModel) []string {
	ids := make([]string, len(m.Deliveries))
	for i, d := range m.Deliveries {
		ids[i] = d.ID.ValueString()
	}
	return ids
}

// Unset, `limit` is the platform's own default page of 50. The read follows
// the cursor to get them, asks each page for no more than it still needs, and
// keeps the platform's newest-first order.
func TestWebhookDeliveriesFollowTheCursorToTheLimit(t *testing.T) {
	t.Parallel()
	api := &deliveriesAPI{page: 20, deliveries: testDeliveries(70)}
	got, resp := readDeliveries(t, api, nil)
	if resp.Diagnostics.HasError() {
		t.Fatalf("read: %v", resp.Diagnostics.Errors())
	}
	ids := deliveryIDs(got)
	if len(ids) != webhookDeliveriesDefault {
		t.Fatalf("read %d deliveries, want %d", len(ids), webhookDeliveriesDefault)
	}
	for i, id := range ids {
		if want := api.deliveries[i]["id"]; id != want {
			t.Fatalf("delivery %d is %s, want %s: the platform's order wasn't kept", i, id, want)
		}
	}
	if want := []int{50, 30, 10}; fmt.Sprint(api.limits) != fmt.Sprint(want) {
		t.Errorf("asked for %v a page, want %v", api.limits, want)
	}
	if want := []string{"", "20", "40"}; fmt.Sprint(api.cursors) != fmt.Sprint(want) {
		t.Errorf("sent cursors %q, want %q", api.cursors, want)
	}
}

// A limit past the contract's largest page asks for 200 at a time, and a
// history shorter than the limit ends where its cursor does.
func TestWebhookDeliveriesPageAtTheContractsMaximum(t *testing.T) {
	t.Parallel()
	api := &deliveriesAPI{page: 200, deliveries: testDeliveries(450)}
	got, resp := readDeliveries(t, api, map[string]tftypes.Value{"limit": tftypes.NewValue(tftypes.Number, 1000)})
	if resp.Diagnostics.HasError() {
		t.Fatalf("read: %v", resp.Diagnostics.Errors())
	}
	if len(got.Deliveries) != 450 {
		t.Errorf("read %d deliveries, want the whole history of 450", len(got.Deliveries))
	}
	if want := []int{200, 200, 200}; fmt.Sprint(api.limits) != fmt.Sprint(want) {
		t.Errorf("asked for %v a page, want %v", api.limits, want)
	}
}

// `status` keeps the deliveries with that status among the newest `limit`,
// rather than reading on until it has `limit` of them.
func TestWebhookDeliveriesFilterOnStatusAmongTheNewest(t *testing.T) {
	t.Parallel()
	api := &deliveriesAPI{page: 4, deliveries: testDeliveries(30)}
	got, resp := readDeliveries(t, api, map[string]tftypes.Value{
		"limit":  tftypes.NewValue(tftypes.Number, 9),
		"status": tftypes.NewValue(tftypes.String, "failed"),
	})
	if resp.Diagnostics.HasError() {
		t.Fatalf("read: %v", resp.Diagnostics.Errors())
	}
	want := []any{api.deliveries[1]["id"], api.deliveries[4]["id"], api.deliveries[7]["id"]}
	if ids := deliveryIDs(got); fmt.Sprint(ids) != fmt.Sprint(want) {
		t.Errorf("kept %v, want the failed three of the newest nine %v", ids, want)
	}
}

// A cursor that hands back what was already read ends the read rather than
// looping, and a delivery read twice is kept once.
func TestWebhookDeliveriesStopWhenAPageAddsNothing(t *testing.T) {
	t.Parallel()
	api := &deliveriesAPI{page: 3, deliveries: testDeliveries(10), repeat: true}
	got, resp := readDeliveries(t, api, nil)
	if resp.Diagnostics.HasError() {
		t.Fatalf("read: %v", resp.Diagnostics.Errors())
	}
	if len(got.Deliveries) != 3 || api.requests != 2 {
		t.Errorf("read %d deliveries in %d requests, want 3 in 2", len(got.Deliveries), api.requests)
	}
}

// A page fuller than `limit` asked for is cut to the limit.
func TestWebhookDeliveriesKeepNoMoreThanTheLimit(t *testing.T) {
	t.Parallel()
	api := &deliveriesAPI{page: 10, deliveries: testDeliveries(10), overfill: true}
	got, resp := readDeliveries(t, api, map[string]tftypes.Value{"limit": tftypes.NewValue(tftypes.Number, 3)})
	if resp.Diagnostics.HasError() {
		t.Fatalf("read: %v", resp.Diagnostics.Errors())
	}
	if len(got.Deliveries) != 3 {
		t.Errorf("read %d deliveries, want the limit of 3", len(got.Deliveries))
	}
}

// The operation answers an empty page for an id the organization doesn't
// have, so the read checks the subscription is there first. A mistyped id is
// an error on the attribute, not an endpoint that reads as never sent to.
func TestWebhookDeliveriesOfAnUnknownSubscriptionIsAnError(t *testing.T) {
	t.Parallel()
	api := &deliveriesAPI{page: 2, deliveries: testDeliveries(2)}
	_, resp := readDeliveries(t, api, map[string]tftypes.Value{
		"subscription_id": tftypes.NewValue(tftypes.String, "0f8fad5b-d9cb-469f-a165-70867728950f"),
	})
	if !resp.Diagnostics.HasError() {
		t.Fatal("an unknown subscription read as one with deliveries")
	}
	if d := resp.Diagnostics.Errors()[0]; !strings.Contains(d.Summary(), "No such webhook subscription") {
		t.Errorf("error = %s: %s", d.Summary(), d.Detail())
	}
	if api.requests != 0 {
		t.Errorf("read the deliveries %d times for a subscription that isn't there", api.requests)
	}
}

// Every field the contract gives a delivery reaches the model, and the
// optional ones it leaves out read as null. `next_attempt_at` reads as null
// unless a retry is scheduled: the platform keeps one on every delivery, from
// when it was queued or claimed, and doesn't clear it when the delivery
// settles.
func TestWebhookDeliveriesCarryEveryField(t *testing.T) {
	t.Parallel()
	api := &deliveriesAPI{page: 2, deliveries: []map[string]any{
		{"id": "00000000-0000-4000-8000-000000000001", "status": "failed", "attempts": 2, "response_code": 503,
			"last_error": "endpoint answered 503", "created_at": "2026-10-26T12:00:00Z", "settled_at": nil,
			"next_attempt_at": "2026-10-26T12:10:00Z", "retry_scheduled": true},
		{"id": "00000000-0000-4000-8000-000000000002", "status": "queued", "attempts": 0,
			"created_at": "2026-10-26T11:00:00Z", "next_attempt_at": "2026-10-26T11:00:00Z", "retry_scheduled": false},
		{"id": "00000000-0000-4000-8000-000000000003", "status": "failed", "attempts": 8,
			"last_error": "dial tcp: connection refused", "created_at": "2026-10-26T10:00:00Z",
			"settled_at": "2026-10-26T10:30:00Z", "next_attempt_at": "2026-10-26T10:31:00Z", "retry_scheduled": false},
		{"id": "00000000-0000-4000-8000-000000000004", "status": "delivered", "attempts": 3, "response_code": 200,
			"created_at": "2026-10-26T09:00:00Z", "settled_at": "2026-10-26T09:05:00Z",
			"next_attempt_at": "2026-10-26T09:06:00Z", "retry_scheduled": false},
	}}
	got, resp := readDeliveries(t, api, nil)
	if resp.Diagnostics.HasError() {
		t.Fatalf("read: %v", resp.Diagnostics.Errors())
	}
	if len(got.Deliveries) != 4 {
		t.Fatalf("read %d deliveries", len(got.Deliveries))
	}
	failed, queued := got.Deliveries[0], got.Deliveries[1]
	if failed.Status.ValueString() != "failed" || failed.Attempts.ValueInt64() != 2 ||
		failed.ResponseCode.ValueInt64() != 503 || failed.LastError.ValueString() != "endpoint answered 503" ||
		failed.CreatedAt.ValueString() != "2026-10-26T12:00:00Z" || !failed.SettledAt.IsNull() ||
		failed.NextAttemptAt.ValueString() != "2026-10-26T12:10:00Z" || !failed.RetryScheduled.ValueBool() {
		t.Errorf("failed delivery = %+v", failed)
	}
	if !queued.ResponseCode.IsNull() || !queued.LastError.IsNull() || !queued.SettledAt.IsNull() ||
		!queued.NextAttemptAt.IsNull() || queued.RetryScheduled.ValueBool() {
		t.Errorf("queued delivery = %+v", queued)
	}
	for _, settled := range got.Deliveries[2:] {
		if !settled.NextAttemptAt.IsNull() || settled.SettledAt.IsNull() {
			t.Errorf("%s delivery = %+v, want a settled one with no next attempt", settled.Status, settled)
		}
	}
}
