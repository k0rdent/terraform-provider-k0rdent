package provider

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/k0rdent"
)

// notFoundAnswer is one shape of 404 the platform can send. Only the service
// that owns the resource answers a missing one, and it always sends the
// contract envelope. The gateway's unrouted path, a service's region guard
// and Go's default mux answer 404 without it, and none of those says anything
// about the resource.
type notFoundAnswer struct {
	name        string
	contentType string
	body        string
	gone        bool
	// answeredBy is what the diagnostic has to name when the 404 is not
	// counted as gone.
	answeredBy string
}

var notFoundAnswers = []notFoundAnswer{
	{
		name: "gateway no route", contentType: "application/json",
		body:       `{"message":"no Route matched with those values","request_id":"x"}`,
		answeredBy: "API gateway",
	},
	{
		name: "plain text from a router", contentType: "text/plain; charset=utf-8",
		body:       "404 page not found\n",
		answeredBy: "without the service's error envelope",
	},
	{
		name: "empty body", contentType: "",
		body:       "",
		answeredBy: "without the service's error envelope",
	},
	{
		name: "service says missing", contentType: "application/json",
		body: `{"error":{"code":404,"message":"not found","details":[{"reason":"NOT_FOUND"}]}}`,
		gone: true,
	},
}

func (a notFoundAnswer) write(t *testing.T, w http.ResponseWriter) {
	if a.contentType != "" {
		w.Header().Set("Content-Type", a.contentType)
	}
	w.WriteHeader(http.StatusNotFound)
	if _, err := w.Write([]byte(a.body)); err != nil {
		t.Errorf("write: %v", err)
	}
}

// answering404 answers every request with the 404 and records each method.
func answering404(t *testing.T, a notFoundAnswer) (*httptest.Server, func() []string) {
	var mu sync.Mutex
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, r.Method)
		mu.Unlock()
		a.write(t, w)
	}))
	t.Cleanup(server.Close)
	return server, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), calls...)
	}
}

// judgeDelete checks a Delete against the answer: gone only on the contract
// 404, and an error naming what answered otherwise. Terraform keeps the
// resource in state whenever Delete reports an error.
func (a notFoundAnswer) judgeDelete(t *testing.T, resp resource.DeleteResponse) {
	t.Helper()
	if a.gone {
		if resp.Diagnostics.HasError() {
			t.Fatalf("a contract 404 is the resource gone, but the delete failed: %v", resp.Diagnostics.Errors())
		}
		return
	}
	if !resp.Diagnostics.HasError() {
		t.Fatal("a 404 that never reached the service was taken as the resource gone")
	}
	if got := resp.Diagnostics.Errors()[0].Detail(); !strings.Contains(got, a.answeredBy) {
		t.Errorf("the error does not say what answered (want %q): %s", a.answeredBy, got)
	}
}

func (a notFoundAnswer) judgeRead(t *testing.T, resp resource.ReadResponse) {
	t.Helper()
	if a.gone {
		if resp.Diagnostics.HasError() {
			t.Fatalf("read: %v", resp.Diagnostics.Errors())
		}
		if !resp.State.Raw.IsNull() {
			t.Fatal("a contract 404 is the resource gone, but it was kept in state")
		}
		return
	}
	if !resp.Diagnostics.HasError() {
		t.Fatal("a 404 that never reached the service read as success")
	}
	if resp.State.Raw.IsNull() {
		t.Fatal("a 404 that never reached the service removed the resource from state")
	}
	if got := resp.Diagnostics.Errors()[0].Detail(); !strings.Contains(got, a.answeredBy) {
		t.Errorf("the error does not say what answered (want %q): %s", a.answeredBy, got)
	}
}

func TestFileStoreDeleteCountsOnlyTheServicesNotFound(t *testing.T) {
	for _, a := range notFoundAnswers {
		t.Run(a.name, func(t *testing.T) {
			server, calls := answering404(t, a)
			resp := deleteFileStore(t, server.URL, map[string]tftypes.Value{"state": str("active")})
			a.judgeDelete(t, resp)
			if !a.gone && strings.Join(calls(), " ") != "GET" {
				t.Errorf("requests = %v, want only the first read before the delete stopped", calls())
			}
		})
	}
}

func TestFileStoreReadCountsOnlyTheServicesNotFound(t *testing.T) {
	for _, a := range notFoundAnswers {
		t.Run(a.name, func(t *testing.T) {
			server, _ := answering404(t, a)
			r := &fileStoreResource{meta: testClusterMeta(t, server.URL, 2*time.Second)}
			raw, s := testValue(t, r, fileStorePlan(map[string]tftypes.Value{"state": str("active")}))
			st := tfsdk.State{Raw: raw, Schema: s.Schema}
			resp := resource.ReadResponse{State: st}
			r.Read(context.Background(), resource.ReadRequest{State: st}, &resp)
			a.judgeRead(t, resp)
		})
	}
}

func TestNetworkDeleteCountsOnlyTheServicesNotFound(t *testing.T) {
	for _, a := range notFoundAnswers {
		t.Run(a.name, func(t *testing.T) {
			server, _ := answering404(t, a)
			a.judgeDelete(t, deleteNetwork(t, server, time.Minute))
		})
	}
}

func TestNetworkReadCountsOnlyTheServicesNotFound(t *testing.T) {
	for _, a := range notFoundAnswers {
		t.Run(a.name, func(t *testing.T) {
			server, _ := answering404(t, a)
			meta := testClusterMeta(t, server.URL, time.Minute)
			st := testNetworkState(t, "net")
			resp := resource.ReadResponse{State: st}
			(&networkResource{meta: meta}).Read(context.Background(), resource.ReadRequest{State: st}, &resp)
			a.judgeRead(t, resp)
		})
	}
}

// A delete that went through is finished only when the service says the
// resource is missing. Any other 404 stops the wait at once with an error that
// says what answered.
func TestAwaitDeletedCountsOnlyTheServicesNotFound(t *testing.T) {
	for _, a := range notFoundAnswers {
		t.Run(a.name, func(t *testing.T) {
			read := func(context.Context) (*widget, *http.Response, []byte, error) {
				h := http.Header{}
				if a.contentType != "" {
					h.Set("Content-Type", a.contentType)
				}
				return nil, &http.Response{StatusCode: http.StatusNotFound, Status: "404 Not Found", Header: h},
					[]byte(a.body), nil
			}
			err := widgets.awaitDeleted(context.Background(), quickWait, "the widget", read)
			if a.gone {
				if err != nil {
					t.Fatalf("awaitDeleted: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("a 404 that never reached the service ended the delete wait as done")
			}
			if errors.Is(err, errWaitTimeout) {
				t.Fatalf("the wait ran to its timeout instead of stopping on the first 404: %v", err)
			}
			var apiErr *k0rdent.APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("err = %T %v, want a *k0rdent.APIError", err, err)
			}
			if got := apiErrorDetail(apiErr); !strings.Contains(got, a.answeredBy) {
				t.Errorf("the error does not say what answered (want %q): %s", a.answeredBy, got)
			}
		})
	}
}
