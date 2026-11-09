package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// busyDeleteAPI is one resource running an operation, as an apply stopped
// mid-create leaves it. It reads busy for busyReads reads, then `active`. A
// DELETE while it's busy is refused with a 409 carrying reason, as the
// platform refuses a delete that isn't from `active` or `failed`. A DELETE
// once it's `active` is taken, and the next read is the 404.
type busyDeleteAPI struct {
	body func(state string) map[string]any
	busy string
	// busyReads is how many reads say busy. Negative never settles.
	busyReads int
	// busyFromDelete starts the operation at the first DELETE, as another
	// run that started one between the destroy's read and its DELETE would.
	busyFromDelete bool
	// reason is the 409's details[].reason, CONFLICT_IN_USE when empty.
	reason string
	// settlesTo is where the operation leaves it: `active` when empty,
	// `deleting` for a delete another run started, "" with gone for one
	// that already finished.
	settlesTo string
	gone      bool

	mu      sync.Mutex
	state   string
	reads   int
	deletes int
	refused int
}

func (a *busyDeleteAPI) serve(t *testing.T) *httptest.Server {
	t.Helper()
	reason := a.reason
	if reason == "" {
		reason = conflictInUse
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		switch r.Method {
		case http.MethodDelete:
			a.deletes++
			if a.busyFromDelete && a.deletes == 1 {
				a.state = a.busy
			}
			switch a.state {
			case "":
				writeTestError(t, w, http.StatusNotFound, "not found")
			case a.busy:
				a.refused++
				writeTestJSON(t, w, http.StatusConflict, map[string]any{"error": map[string]any{
					"code": http.StatusConflict, "requestId": "req-409",
					"message": "conflict: it cannot be deleted in current state",
					"details": []map[string]any{{"reason": reason}},
				}})
			default:
				a.state = "deleting"
				w.WriteHeader(http.StatusNoContent)
			}
		case http.MethodGet:
			a.reads++
			switch a.state {
			case "":
				writeTestError(t, w, http.StatusNotFound, "not found")
				return
			case "deleting":
				writeTestJSON(t, w, http.StatusOK, a.body(a.state))
				a.state = "" // gone on the next read
				return
			}
			writeTestJSON(t, w, http.StatusOK, a.body(a.state))
			if a.state == a.busy && a.busyReads >= 0 {
				if a.busyReads--; a.busyReads <= 0 {
					a.state = a.settlesTo
					if a.state == "" && !a.gone {
						a.state = "active"
					}
				}
			}
		default:
			writeTestError(t, w, http.StatusMethodNotAllowed, r.Method+" "+r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// busyKind is one resource kind a destroy waits out mid-operation, in one of
// the states the platform refuses its delete in.
type busyKind struct {
	name, busy string
	body       func(string) map[string]any
	set        map[string]tftypes.Value
	build      func(*Meta) resource.Resource
}

func busyKinds() []busyKind {
	network := func(s string) map[string]any {
		return map[string]any{"id": "x1", "type": "evpn", "state": s, "evpn": map[string]any{"type": "l3vpn"}}
	}
	subnet := func(m *Meta) resource.Resource { return &subnetResource{meta: m} }
	group := func(m *Meta) resource.Resource { return &securityGroupResource{meta: m} }
	peering := func(m *Meta) resource.Resource { return &vpcPeeringResource{meta: m} }
	net := func(m *Meta) resource.Resource { return &networkResource{meta: m} }
	return []busyKind{
		{"subnet", "creating", stateBody("state"), map[string]tftypes.Value{"network": str("n1")}, subnet},
		{"security group", "creating", stateBody("state"), nil, group},
		{"security group", "updating", stateBody("state"), nil, group},
		{"VPC peering", "creating", stateBody("state"), map[string]tftypes.Value{"vpc": str("v1")}, peering},
		{"network", "creating", network, nil, net},
		{"network", "updating", network, nil, net},
	}
}

// destroyBusy runs the resource's own Delete against api with timeout as the
// provider's operation timeout.
func destroyBusy(t *testing.T, k busyKind, api *busyDeleteAPI, timeout time.Duration) diag.Diagnostics {
	t.Helper()
	api.body = k.body
	api.busy = k.busy
	meta := resendingMeta(t, api.serve(t).URL)
	meta.Wait.Timeout = timeout
	r := k.build(meta)
	set := map[string]tftypes.Value{"id": str("x1")}
	for name, v := range k.set {
		set[name] = v
	}
	raw, s := testValue(t, r, set)
	st := tfsdk.State{Raw: raw, Schema: s.Schema}
	resp := resource.DeleteResponse{State: st}
	r.Delete(context.Background(), resource.DeleteRequest{State: st}, &resp)
	return resp.Diagnostics
}

// A destroy of a resource still being created or updated, as an interrupted
// apply leaves it, waits for the operation to finish and then deletes it,
// rather than failing on the 409 the platform answers until then. It sends
// one DELETE, after the operation finished.
func TestADestroyWaitsOutTheOperationBeforeDeleting(t *testing.T) {
	t.Parallel()
	for _, k := range busyKinds() {
		t.Run(k.name+" "+k.busy, func(t *testing.T) {
			t.Parallel()
			api := &busyDeleteAPI{busyReads: 3}
			api.state = k.busy
			if diags := destroyBusy(t, k, api, time.Minute); diags.HasError() {
				t.Fatalf("destroy failed: %s", diagText(diags))
			}
			if api.deletes != 1 || api.refused != 0 {
				t.Errorf("%d DELETEs, %d refused, want one, sent once the operation finished", api.deletes, api.refused)
			}
			if api.state != "" {
				t.Errorf("the destroy returned while the resource was still %q", api.state)
			}
		})
	}
}

// An operation another run starts between the destroy's read and its DELETE
// is refused the same way. The destroy reads the resource, finds it busy,
// waits for the operation, and sends the DELETE again.
func TestADestroyRefusedMidOperationWaitsAndSendsAgain(t *testing.T) {
	t.Parallel()
	for _, k := range busyKinds() {
		t.Run(k.name+" "+k.busy, func(t *testing.T) {
			t.Parallel()
			api := &busyDeleteAPI{busyReads: 3, busyFromDelete: true}
			api.state = "active"
			if diags := destroyBusy(t, k, api, time.Minute); diags.HasError() {
				t.Fatalf("destroy failed: %s", diagText(diags))
			}
			if api.deletes != 2 || api.refused != 1 {
				t.Errorf("%d DELETEs, %d refused, want the refused one and the one after the wait", api.deletes, api.refused)
			}
			if api.state != "" {
				t.Errorf("the destroy returned while the resource was still %q", api.state)
			}
		})
	}
}

// An operation that outlasts the delete timeout fails the destroy with
// nothing deleted and says what to do next. No DELETE is sent into it.
func TestADestroyThatOutwaitsItsTimeoutSaysToRunItAgain(t *testing.T) {
	t.Parallel()
	for _, k := range busyKinds() {
		t.Run(k.name+" "+k.busy, func(t *testing.T) {
			t.Parallel()
			api := &busyDeleteAPI{busyReads: -1}
			api.state = k.busy
			diags := destroyBusy(t, k, api, 50*time.Millisecond)
			if !diags.HasError() {
				t.Fatal("a destroy that never got to delete reported success")
			}
			text := diagText(diags)
			for _, want := range []string{
				"The " + k.name + "'s operation did not finish before the destroy stopped waiting",
				"It was still `" + k.busy + "` at the last read",
				"Nothing was deleted",
				"run `terraform destroy` again",
				"raise `delete` in the resource's `timeouts` block",
			} {
				if !strings.Contains(strings.Join(strings.Fields(text), " "), want) {
					t.Errorf("the diagnostic doesn't say %q:\n%s", want, text)
				}
			}
			if api.deletes != 0 {
				t.Errorf("%d DELETEs sent into a running operation", api.deletes)
			}
		})
	}
}

// A 409 with another reason isn't a delete that waiting lets through, even on
// a resource mid-operation, so it's reported at once, as before.
func TestADestroyRefusedForAnotherReasonMidOperationIsReported(t *testing.T) {
	t.Parallel()
	k := busyKinds()[0]
	api := &busyDeleteAPI{busyReads: 3, busyFromDelete: true, reason: "CONFLICT_IMMUTABLE"}
	api.state = "active"
	diags := destroyBusy(t, k, api, time.Minute)
	if !diags.HasError() || !strings.Contains(diagText(diags), "CONFLICT_IMMUTABLE") {
		t.Fatalf("want the platform's refusal, got: %s", diagText(diags))
	}
	if api.deletes != 1 {
		t.Errorf("%d DELETEs, want the refused one alone", api.deletes)
	}
}

// An operation that ends with the resource gone, or with a delete another run
// started, leaves nothing to send. The destroy sends no DELETE, and waits for
// that delete rather than meet its 409.
func TestADestroyWaitingOutAnOperationSendsNothingOnceTheResourceGoes(t *testing.T) {
	t.Parallel()
	for _, end := range []struct {
		name      string
		settlesTo string
		gone      bool
	}{
		{"deleting", "deleting", false},
		{"gone", "", true},
	} {
		for _, k := range busyKinds() {
			t.Run(end.name+"/"+k.name+" "+k.busy, func(t *testing.T) {
				t.Parallel()
				api := &busyDeleteAPI{busyReads: 3, settlesTo: end.settlesTo, gone: end.gone}
				api.state = k.busy
				if diags := destroyBusy(t, k, api, time.Minute); diags.HasError() {
					t.Fatalf("destroy failed: %s", diagText(diags))
				}
				if api.deletes != 0 {
					t.Errorf("%d DELETEs, want none", api.deletes)
				}
				if api.state != "" {
					t.Errorf("the destroy returned while the resource was still %q", api.state)
				}
			})
		}
	}
}
