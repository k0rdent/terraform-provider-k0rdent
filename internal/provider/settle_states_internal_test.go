package provider

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// What every waiter does with every state, one kind at a time.
//
// The kinds differ on purpose: `failed` is provisional while a cluster settles
// and terminal while anything else does, a store with no state is a dead end
// where a network with none is done, and so on. Each case is one state in and
// one verdict out, so a change to any kind's policy shows up as a changed row
// here.
//
// Every case runs through the resource's own waiter against a fake control
// plane, so it checks the kind's table and that the resource reads with the
// right reader and decides with the right table. The cases were first checked
// against each kind's own switch, before the tables existed.

// The verdicts a case can pin.
const (
	pinDone        = "done"        // the wait ends and the resource is usable, or gone
	pinWaits       = "waits"       // the wait keeps polling
	pinProvisional = "provisional" // a failure tolerated for ProvisionalFailureWindow
	pinTerminal    = "terminal"    // a failure reported on sight
	pinUnknown     = "unknown"     // a state the provider does not recognise
)

// stateCase is one state and what a waiter makes of it.
type stateCase struct {
	// state is the value served. Empty with absent unset serves `""`.
	state string
	// absent leaves the state out of the body altogether.
	absent bool
	// detail adds a message, a reason and a status reason to the body, so the
	// case shows which of them a failure quotes.
	detail bool
	// extra is merged into the body last.
	extra map[string]any
	// want is the verdict.
	want string
	// says is the error a failure reports. An unknown state's error is
	// unknownStateError's, and is spelled out only where says is set, which
	// one case does so that a change to that wording is caught.
	says string
}

func (c stateCase) label() string {
	label := c.state
	switch {
	case c.absent:
		label = "no state"
	case label == "":
		label = "empty state"
	}
	if c.detail {
		label += " with detail"
	}
	if len(c.extra) > 0 {
		label += " and " + strings.Join(sortedKeys(c.extra), ",")
	}
	return label
}

const (
	pinnedMessage      = "the workflow gave up"
	pinnedReason       = "INSUFFICIENT_CAPACITY"
	pinnedStatusReason = "the status reason"

	// What stateDetail makes of each source the kinds quote.
	fromMessageAndReason = ": reason " + pinnedReason + "; " + pinnedMessage
	fromMessage          = ": " + pinnedMessage
	fromStatusReason     = ": " + pinnedStatusReason
)

// pinnedKind is one resource kind's cases and how to drive them.
type pinnedKind struct {
	// name is the kind as an unknown state's error names it.
	name string
	// stateKey is the body field the state is served in.
	stateKey string
	// base is the body every case starts from.
	base   map[string]any
	settle []stateCase
	delete []stateCase
	// wired drives one case through the resource's own waiter against a fake
	// control plane, and returns the verdict and, for a failure, the error.
	wired func(t *testing.T, op string, body map[string]any) (string, string)
}

const (
	opSettle = "settle"
	opDelete = "delete"
)

func (k pinnedKind) body(c stateCase) map[string]any {
	body := map[string]any{}
	for key, v := range k.base {
		body[key] = v
	}
	if !c.absent {
		body[k.stateKey] = c.state
	}
	if c.detail {
		body["stateMessage"] = pinnedMessage
		body["stateReason"] = pinnedReason
		body["statusReason"] = pinnedStatusReason
	}
	for key, v := range c.extra {
		body[key] = v
	}
	return body
}

func TestEachKindsWaitersPinnedStateByState(t *testing.T) {
	t.Parallel()
	for _, k := range pinnedKinds() {
		for _, op := range []string{opSettle, opDelete} {
			cases := k.settle
			if op == opDelete {
				cases = k.delete
			}
			for _, c := range cases {
				t.Run(k.name+"/"+op+"/"+c.label(), func(t *testing.T) {
					t.Parallel()
					got, says := k.wired(t, op, k.body(c))
					if got != c.want {
						t.Fatalf("verdict = %s (%q), want %s", got, says, c.want)
					}
					want := c.says
					if c.want == pinUnknown && want == "" {
						want = unknownStateError(k.name, c.state).Error()
					}
					if says != want {
						t.Errorf("error = %q\nwant    %q", says, want)
					}
				})
			}
		}
	}
}

// classify reads a waiter's outcome back as a verdict. The provisional window
// is zero, so a provisional failure is reported on the first poll, with the
// window's suffix.
func classify(err error) (string, string) {
	if err == nil {
		return pinDone, ""
	}
	msg := err.Error()
	switch {
	case errors.Is(err, errWaitTimeout), strings.Contains(msg, "timed out waiting for"):
		return pinWaits, ""
	case strings.HasSuffix(msg, ", and stayed that way for 0s"):
		return pinProvisional, strings.TrimSuffix(msg, ", and stayed that way for 0s")
	case strings.Contains(msg, "which this provider does not recognise"):
		return pinUnknown, msg
	default:
		return pinTerminal, msg
	}
}

// overHTTP drives a case through the resource's own waiter against a fake
// control plane. A delete answers DELETE with 204, and the GETs before it read
// `active`, so a delete reaches its wait whatever the case serves.
func overHTTP(
	stateKey string,
	settle func(context.Context, *Meta) error,
	del func(context.Context, *testing.T, *Meta) diag.Diagnostics,
) func(*testing.T, string, map[string]any) (string, string) {
	return func(t *testing.T, op string, body map[string]any) (string, string) {
		var deleted atomic.Bool
		before := map[string]any{}
		for key, v := range body {
			before[key] = v
		}
		before[stateKey] = "active"
		delete(before, "stateMessage")
		delete(before, "stateReason")
		delete(before, "statusReason")
		// The wait's deadline is the test's to call, not the clock's. A waiter
		// that reads the case's state a second time has shown it keeps
		// polling, which is all "waits" means, so that read ends it. A
		// wall-clock deadline short enough to keep the suite fast was also
		// short enough for a loaded machine to spend before the first read,
		// which turned a case that should end at once into "waits".
		ctx := newTestDeadline(t)
		var polled atomic.Int32
		server := resettingServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// A cluster's wait follows its event log, which isn't a poll. This
			// fake has none.
			if strings.HasSuffix(r.URL.Path, "/runtime/events") {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			if r.Method == http.MethodDelete {
				deleted.Store(true)
				w.WriteHeader(http.StatusNoContent)
				return
			}
			served := body
			if op == opDelete && !deleted.Load() {
				served = before
			} else if polled.Add(1) == 2 {
				ctx.expire()
			}
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(served); err != nil {
				t.Errorf("write: %v", err)
			}
		}))
		defer server.Close()

		meta := testClusterMeta(t, server.URL, time.Hour)
		meta.Wait.Interval = time.Millisecond
		if op == opSettle {
			return classify(settle(ctx, meta))
		}
		diags := del(ctx, t, meta)
		if !diags.HasError() {
			return pinDone, ""
		}
		// The reading, without the way out a delete the platform ended adds
		// after it: TestADeleteThePlatformEndedSaysWhatNext.
		reading, _, _ := strings.Cut(diags.Errors()[0].Detail(), "\n\n"+deleteRefusedAdvice)
		return classify(errors.New(reading))
	}
}

// testDeadline is a context whose deadline passes when the test says, not
// when the clock does. Err reports context.DeadlineExceeded once it has, so a
// waiter treats it exactly as its operation timeout running out. It passes on
// its own after a minute, so a waiter that never polls again still ends.
type testDeadline struct {
	context.Context
	at   time.Time
	done chan struct{}
	once sync.Once
}

func newTestDeadline(t *testing.T) *testDeadline {
	t.Helper()
	d := &testDeadline{Context: context.Background(), at: time.Now().Add(time.Minute), done: make(chan struct{})}
	timer := time.AfterFunc(time.Minute, d.expire)
	t.Cleanup(func() { timer.Stop() })
	return d
}

func (d *testDeadline) expire()                     { d.once.Do(func() { close(d.done) }) }
func (d *testDeadline) Deadline() (time.Time, bool) { return d.at, true }
func (d *testDeadline) Done() <-chan struct{}       { return d.done }

func (d *testDeadline) Err() error {
	select {
	case <-d.done:
		return context.DeadlineExceeded
	default:
		return nil
	}
}

// resettingServer is httptest.NewServer with connections that end in a reset
// rather than a FIN, so a closed one leaves no TIME_WAIT behind. Every case
// above starts a server of its own, and a stress run at -count=200 opens tens
// of thousands of connections inside the 30 seconds macOS holds a closed one,
// more than its 16,384 ephemeral ports. The run looking for a flake then fails
// on "can't assign requested address" instead, which hides the flake it was
// looking for.
func resettingServer(t *testing.T, h http.Handler) *httptest.Server {
	t.Helper()
	server := httptest.NewUnstartedServer(h)
	server.Config.ConnState = func(c net.Conn, state http.ConnState) {
		tcp, ok := c.(*net.TCPConn)
		if !ok || state != http.StateNew {
			return
		}
		// A connection the client already dropped has nothing left to linger.
		if err := tcp.SetLinger(0); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Errorf("set linger: %v", err)
		}
	}
	server.Start()
	return server
}

// deleteThrough runs res's Delete against state built from attrs.
func deleteThrough(ctx context.Context, t *testing.T, res resource.Resource, attrs map[string]string) diag.Diagnostics {
	set := make(map[string]tftypes.Value, len(attrs))
	for name, v := range attrs {
		set[name] = tftypes.NewValue(tftypes.String, v)
	}
	raw, schema := testValue(t, res, set)
	state := tfsdk.State{Raw: raw, Schema: schema.Schema}
	resp := resource.DeleteResponse{State: state}
	res.Delete(ctx, resource.DeleteRequest{State: state}, &resp)
	return resp.Diagnostics
}

var pinnedTarget = clusterTarget{region: "test", project: "test"}

func pinnedKinds() []pinnedKind {
	return []pinnedKind{
		{
			name: "cluster", stateKey: "state",
			base: map[string]any{"id": "c1"},
			settle: []stateCase{
				{absent: true, want: pinDone},
				{state: "", want: pinUnknown},
				{state: "active", want: pinDone},
				{state: "creating", want: pinWaits},
				{state: "updating", want: pinWaits},
				{state: "deleting", want: pinWaits},
				{state: "failed", detail: true, want: pinProvisional,
					says: "the platform reports the cluster as failed" + fromMessageAndReason},
				{state: "deleted", detail: true, want: pinTerminal,
					says: "the platform reports the cluster as deleted"},
				{state: "quiescing", want: pinUnknown,
					says: `the platform reports the cluster as "quiescing", which this provider does not ` +
						`recognise. Either the landscape is running a newer API than this provider was ` +
						`built against, or the value is a typo upstream. Check the resource in the console`},
			},
			delete: []stateCase{
				{absent: true, want: pinWaits},
				{state: "", want: pinUnknown},
				{state: "deleting", want: pinWaits},
				{state: "deleted", want: pinWaits},
				{state: "failed", detail: true, want: pinTerminal,
					says: "the platform reports the cluster as failed" + fromMessageAndReason},
				{state: "active", detail: true, want: pinTerminal,
					says: `the platform reports the cluster as "active" after the delete was accepted`},
				{state: "creating", want: pinTerminal,
					says: `the platform reports the cluster as "creating" after the delete was accepted`},
				{state: "updating", want: pinTerminal,
					says: `the platform reports the cluster as "updating" after the delete was accepted`},
				{state: "quiescing", want: pinUnknown},
			},
			wired: overHTTP("state",
				func(ctx context.Context, meta *Meta) error {
					return (&clusterResource{meta: meta}).waitForSettled(ctx, pinnedTarget, "c1", "the cluster to settle")
				},
				func(ctx context.Context, t *testing.T, meta *Meta) diag.Diagnostics {
					return deleteThrough(ctx, t, &clusterResource{meta: meta}, map[string]string{"id": "c1"})
				}),
		},
		{
			name: "instance group", stateKey: "state",
			base: map[string]any{"id": "g1", "clusterType": "/v1/regions/test/compute/cluster-types/test"},
			settle: []stateCase{
				{absent: true, want: pinDone},
				{state: "", want: pinUnknown},
				{state: "active", want: pinDone},
				{state: "active", detail: true, want: pinDone},
				{state: "creating", want: pinWaits},
				{state: "updating", want: pinWaits},
				{state: "deleting", want: pinWaits},
				{state: "failed", detail: true, want: pinTerminal,
					says: "the platform reports the instance group as failed" + fromMessageAndReason},
				{state: "deleted", detail: true, want: pinTerminal,
					says: "the platform reports the instance group as deleted"},
				{state: "quiescing", want: pinUnknown},
			},
			delete: []stateCase{
				{absent: true, want: pinWaits},
				{state: "", want: pinUnknown},
				{state: "deleted", want: pinDone},
				{state: "deleting", want: pinWaits},
				{state: "creating", want: pinWaits},
				{state: "failed", detail: true, want: pinProvisional,
					says: "the platform reports the instance group as failed" + fromMessageAndReason},
				{state: "active", detail: true, want: pinTerminal,
					says: `the platform reports the instance group as "active" after the delete was accepted`},
				{state: "updating", want: pinTerminal,
					says: `the platform reports the instance group as "updating" after the delete was accepted`},
				{state: "quiescing", want: pinUnknown},
			},
			wired: overHTTP("state",
				func(ctx context.Context, meta *Meta) error {
					return (&instanceGroupResource{meta: meta}).waitForSettled(ctx, pinnedTarget, "g1", "the instance group to settle")
				},
				func(ctx context.Context, t *testing.T, meta *Meta) diag.Diagnostics {
					return deleteThrough(ctx, t, &instanceGroupResource{meta: meta}, map[string]string{"id": "g1"})
				}),
		},
		{
			name: "network", stateKey: "state",
			base: map[string]any{"id": "net", "type": "evpn", "evpn": map[string]any{"type": "l3vpn"}},
			settle: []stateCase{
				{absent: true, want: pinDone},
				{state: "", want: pinDone},
				{state: "active", want: pinDone},
				{state: "creating", want: pinWaits},
				{state: "updating", want: pinWaits},
				{state: "deleting", want: pinWaits},
				{state: "failed", detail: true, want: pinTerminal,
					says: "the platform reports the network as failed" + fromMessageAndReason},
				{state: "quiescing", want: pinUnknown},
			},
			delete: []stateCase{
				{absent: true, want: pinWaits},
				{state: "", want: pinWaits},
				{state: "deleting", want: pinWaits},
				{state: "failed", detail: true, want: pinProvisional,
					says: "the platform reports the network as failed" + fromMessageAndReason},
				{state: "active", detail: true, want: pinProvisional,
					says: `the platform reports the network as "active" after the delete was accepted`},
				{state: "creating", want: pinProvisional,
					says: `the platform reports the network as "creating" after the delete was accepted`},
				{state: "updating", want: pinProvisional,
					says: `the platform reports the network as "updating" after the delete was accepted`},
				{state: "quiescing", want: pinUnknown},
			},
			wired: overHTTP("state",
				func(ctx context.Context, meta *Meta) error {
					return (&networkResource{meta: meta}).waitForSettled(ctx, pinnedTarget, "net", "the network to settle")
				},
				func(ctx context.Context, t *testing.T, meta *Meta) diag.Diagnostics {
					return deleteThrough(ctx, t, &networkResource{meta: meta}, map[string]string{"id": "net"})
				}),
		},
		{
			name: "security group", stateKey: "state",
			base: map[string]any{"id": "sg"},
			settle: []stateCase{
				{absent: true, want: pinDone},
				{state: "", want: pinDone},
				{state: "active", want: pinDone},
				{state: "creating", want: pinWaits},
				{state: "updating", want: pinWaits},
				{state: "deleting", want: pinWaits},
				{state: "failed", detail: true, want: pinTerminal,
					says: "the platform reports the security group as failed" + fromStatusReason},
				{state: "quiescing", want: pinUnknown},
			},
			delete: []stateCase{
				{absent: true, want: pinWaits},
				{state: "", want: pinWaits},
				{state: "deleting", want: pinWaits},
				{state: "failed", detail: true, want: pinTerminal,
					says: "the platform reports the security group as failed" + fromStatusReason},
				{state: "active", detail: true, want: pinTerminal,
					says: `the platform reports the security group as "active" after the delete was accepted`},
				{state: "creating", want: pinTerminal,
					says: `the platform reports the security group as "creating" after the delete was accepted`},
				{state: "updating", want: pinTerminal,
					says: `the platform reports the security group as "updating" after the delete was accepted`},
				{state: "quiescing", want: pinUnknown},
			},
			wired: overHTTP("state",
				func(ctx context.Context, meta *Meta) error {
					return (&securityGroupResource{meta: meta}).waitForSettled(ctx, pinnedTarget, "sg", "the security group to settle")
				},
				func(ctx context.Context, t *testing.T, meta *Meta) diag.Diagnostics {
					return deleteThrough(ctx, t, &securityGroupResource{meta: meta}, map[string]string{"id": "sg"})
				}),
		},
		{
			name: "subnet", stateKey: "state",
			base: map[string]any{"id": "sn", "prefixLength": 24},
			settle: []stateCase{
				{absent: true, want: pinDone},
				{state: "", want: pinDone},
				{state: "active", want: pinDone},
				{state: "creating", want: pinWaits},
				{state: "deleting", want: pinWaits},
				{state: "failed", detail: true, want: pinTerminal,
					says: "the platform reports the subnet as failed" + fromMessageAndReason},
				{state: "updating", want: pinUnknown},
				{state: "quiescing", want: pinUnknown},
			},
			delete: []stateCase{
				{absent: true, want: pinWaits},
				{state: "", want: pinWaits},
				{state: "deleting", want: pinWaits},
				{state: "failed", detail: true, want: pinTerminal,
					says: "the platform reports the subnet as failed" + fromMessageAndReason},
				{state: "active", detail: true, want: pinTerminal,
					says: `the platform reports the subnet as "active" after the delete was accepted`},
				{state: "creating", want: pinTerminal,
					says: `the platform reports the subnet as "creating" after the delete was accepted`},
				{state: "updating", want: pinUnknown},
				{state: "quiescing", want: pinUnknown},
			},
			wired: overHTTP("state",
				func(ctx context.Context, meta *Meta) error {
					return (&subnetResource{meta: meta}).waitForSettled(ctx, pinnedTarget, "net", "sn")
				},
				func(ctx context.Context, t *testing.T, meta *Meta) diag.Diagnostics {
					return deleteThrough(ctx, t, &subnetResource{meta: meta}, map[string]string{"id": "sn", "network": "net"})
				}),
		},
		{
			name: "VPC peering", stateKey: "state",
			base: map[string]any{"id": "peer"},
			settle: []stateCase{
				{absent: true, want: pinDone},
				{state: "", want: pinDone},
				{state: "active", want: pinDone},
				{state: "creating", want: pinWaits},
				{state: "deleting", want: pinWaits},
				{state: "failed", detail: true, want: pinTerminal,
					says: "the platform reports this side of the peering as failed" + fromMessage},
				{state: "updating", want: pinUnknown},
				{state: "quiescing", want: pinUnknown},
			},
			delete: []stateCase{
				{absent: true, want: pinWaits},
				{state: "", want: pinWaits},
				{state: "deleting", want: pinWaits},
				{state: "failed", detail: true, want: pinTerminal,
					says: "the platform reports this side of the peering as failed" + fromMessage},
				{state: "active", detail: true, want: pinTerminal,
					says: `the platform reports this side of the peering as "active" after the delete was accepted`},
				{state: "creating", want: pinTerminal,
					says: `the platform reports this side of the peering as "creating" after the delete was accepted`},
				{state: "updating", want: pinUnknown},
				{state: "quiescing", want: pinUnknown},
			},
			wired: overHTTP("state",
				func(ctx context.Context, meta *Meta) error {
					return (&vpcPeeringResource{meta: meta}).waitForSettled(ctx, pinnedTarget, "vpc", "peer")
				},
				func(ctx context.Context, t *testing.T, meta *Meta) diag.Diagnostics {
					return deleteThrough(ctx, t, &vpcPeeringResource{meta: meta}, map[string]string{"id": "peer", "vpc": "vpc"})
				}),
		},
		{
			// The VPC is only ever waited on, by the security group binding.
			name: "VPC", stateKey: "state",
			base: map[string]any{"id": "vpc"},
			settle: []stateCase{
				{absent: true, want: pinDone},
				{state: "", want: pinDone},
				{state: "active", want: pinDone},
				{state: "creating", want: pinWaits},
				{state: "updating", want: pinWaits},
				{state: "deleting", want: pinWaits},
				{state: "failed", detail: true, want: pinTerminal,
					says: "the platform reports the VPC as failed" + fromMessage},
				{state: "quiescing", want: pinUnknown},
			},
			wired: overHTTP("state",
				func(ctx context.Context, meta *Meta) error {
					return (&vpcSecurityGroupsResource{meta: meta}).waitForSettled(ctx, pinnedTarget, "vpc")
				}, nil),
		},
		{
			name: "file store", stateKey: "state",
			base: map[string]any{"id": "fs", "capacityGiB": 10},
			settle: []stateCase{
				{absent: true, want: pinUnknown},
				{state: "", want: pinUnknown},
				{state: "active", want: pinDone},
				{state: "active", extra: map[string]any{"reconciling": true}, want: pinWaits},
				{state: "active", extra: map[string]any{"reconciling": false}, want: pinDone},
				{state: "active", detail: true, want: pinTerminal,
					says: "a change to the file store failed, and the store stays active" + fromMessageAndReason},
				{state: "active", extra: map[string]any{"statusReason": pinnedStatusReason}, want: pinTerminal,
					says: "a change to the file store failed, and the store stays active"},
				{state: "active", extra: map[string]any{
					"consumerStatus": []any{map[string]any{"state": "creating"}},
				}, want: pinWaits},
				{state: "active", extra: map[string]any{
					"consumerStatus": []any{map[string]any{"state": "deleting"}},
				}, want: pinWaits},
				{state: "active", extra: map[string]any{
					"reconciling":    true,
					"consumerStatus": []any{map[string]any{"state": "failed"}},
				}, want: pinDone},
				{state: "creating", want: pinWaits},
				{state: "failed", detail: true, want: pinTerminal,
					says: "the platform reports the file store as failed" + fromMessageAndReason},
				{state: "deleting", detail: true, want: pinTerminal,
					says: "the platform reports the file store as deleting, so it is being torn down and " +
						"will not become active" + fromMessageAndReason},
				{state: "updating", want: pinUnknown},
			},
			delete: []stateCase{
				{absent: true, want: pinUnknown},
				{state: "", want: pinUnknown},
				{state: "deleting", want: pinWaits},
				{state: "failed", detail: true, want: pinTerminal,
					says: "the platform reports the file store as failed" + fromMessageAndReason},
				{state: "active", detail: true, want: pinTerminal,
					says: `the platform reports the file store as "active" after the delete was accepted`},
				{state: "creating", want: pinTerminal,
					says: `the platform reports the file store as "creating" after the delete was accepted`},
				{state: "updating", want: pinUnknown},
			},
			wired: overHTTP("state",
				func(ctx context.Context, meta *Meta) error {
					return (&fileStoreResource{meta: meta}).waitForSettled(ctx, pinnedTarget, "fs", "the file store to settle")
				},
				func(ctx context.Context, t *testing.T, meta *Meta) diag.Diagnostics {
					return deleteThrough(ctx, t, &fileStoreResource{meta: meta}, map[string]string{"id": "fs"})
				}),
		},
		{
			// A project is only waited on while it is deleted, and reports
			// `status` where everything else reports `state`.
			name: "project", stateKey: "status",
			base: map[string]any{"id": "p1", "organizationId": "org"},
			delete: []stateCase{
				{absent: true, want: pinUnknown},
				{state: "", want: pinUnknown},
				{state: "deleting", want: pinWaits},
				{state: "active", want: pinTerminal,
					says: `the platform reports the project as "active" after the delete was accepted`},
				{state: "suspended", want: pinTerminal,
					says: `the platform reports the project as "suspended" after the delete was accepted`},
				{state: "disabled", want: pinTerminal,
					says: `the platform reports the project as "disabled" after the delete was accepted`},
				{state: "failed", want: pinUnknown},
			},
			wired: overHTTP("status", nil,
				func(ctx context.Context, t *testing.T, meta *Meta) diag.Diagnostics {
					return deleteThrough(ctx, t, &projectResource{meta: meta},
						map[string]string{"id": "p1", "organization_id": "org"})
				}),
		},
	}
}
