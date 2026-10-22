package provider

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	k0rdent "github.com/k0rdent/terraform-provider-k0rdent/internal/k0rdent"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// sentDeadline is one request and the deadline it was sent under.
type sentDeadline struct {
	request string
	at      time.Time
	bounded bool
}

// deadlineRecorder notes the deadline on every request's context, so a test
// can check which timeout an operation ran under without waiting for one.
type deadlineRecorder struct {
	base http.RoundTripper
	mu   sync.Mutex
	sent []sentDeadline
}

func (d *deadlineRecorder) RoundTrip(req *http.Request) (*http.Response, error) {
	at, bounded := req.Context().Deadline()
	d.mu.Lock()
	d.sent = append(d.sent, sentDeadline{request: req.Method + " " + req.URL.Path, at: at, bounded: bounded})
	d.mu.Unlock()
	return d.base.RoundTrip(req)
}

// A create runs under the resource's own `timeouts.create`, or without one the
// provider's default for a cluster, which is DefaultLongOperationTimeout unless
// operation_timeout is set. That bound covers the POST as well as
// the wait after it. The deadlines are read off the requests rather than
// waited out, so this holds on a loaded machine.
func TestACreateRunsUnderItsOperationTimeout(t *testing.T) {
	const operationTimeout = 3 * time.Hour
	for _, tc := range []struct {
		name   string
		create string
		// long is the provider's default for a cluster's create, zero for
		// operation_timeout's.
		long time.Duration
		want time.Duration
	}{
		{"operation_timeout", "", 0, operationTimeout},
		{"the hardware default", "", 4 * time.Hour, 4 * time.Hour},
		{"timeouts.create", "5h", 4 * time.Hour, 5 * time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodPost {
					w.WriteHeader(http.StatusCreated)
					_, _ = fmt.Fprint(w, `{"id":"c1","state":"creating"}`)
					return
				}
				writeJSON(w, map[string]any{"id": "c1", "state": "active"})
			}))
			t.Cleanup(server.Close)

			seen := &deadlineRecorder{base: testTransport(t)}
			client, err := k0rdent.New(server.URL, k0rdent.WithTransport(seen))
			if err != nil {
				t.Fatalf("new test client: %v", err)
			}
			r := &clusterResource{meta: &Meta{
				Client: client, Region: "test", Project: "test",
				Wait: waitConfig{Timeout: operationTimeout, Long: tc.long, Interval: time.Microsecond},
			}}

			values := clusterValues(verRef(1), "", 2, "")
			if tc.create != "" {
				raw, _ := testValue(t, r, values)
				object, ok := raw.Type().(tftypes.Object)
				if !ok {
					t.Fatal("cluster schema is not an object")
				}
				timeoutsType, ok := object.AttributeTypes["timeouts"].(tftypes.Object)
				if !ok {
					t.Fatal("the cluster has no timeouts block")
				}
				set := make(map[string]tftypes.Value, len(timeoutsType.AttributeTypes))
				for name, attrType := range timeoutsType.AttributeTypes {
					set[name] = tftypes.NewValue(attrType, nil)
				}
				set["create"] = tftypes.NewValue(tftypes.String, tc.create)
				values["timeouts"] = tftypes.NewValue(timeoutsType, set)
			}
			planRaw, s := testValue(t, r, values)

			start := time.Now()
			resp := resource.CreateResponse{State: tfsdk.State{Raw: planRaw, Schema: s.Schema}}
			r.Create(context.Background(), resource.CreateRequest{Plan: tfsdk.Plan{Raw: planRaw, Schema: s.Schema}}, &resp)
			end := time.Now()
			if resp.Diagnostics.HasError() {
				t.Fatalf("create: %v", resp.Diagnostics.Errors())
			}

			seen.mu.Lock()
			defer seen.mu.Unlock()
			var posted, read bool
			for _, s := range seen.sent {
				posted = posted || strings.HasPrefix(s.request, "POST ")
				read = read || strings.HasPrefix(s.request, "GET ")
				switch {
				case !s.bounded:
					t.Errorf("%s was sent with no deadline, want one %s out", s.request, tc.want)
				case s.at.Before(start.Add(tc.want)) || s.at.After(end.Add(tc.want)):
					t.Errorf("%s was sent with a deadline %s out, want %s",
						s.request, s.at.Sub(start).Round(time.Minute), tc.want)
				}
			}
			if !posted || !read {
				t.Errorf("requests %v, want the POST and a read after it", seen.sent)
			}
		})
	}
}
