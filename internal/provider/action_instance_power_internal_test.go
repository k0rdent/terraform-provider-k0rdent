package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/action"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

const testPowerInstance = "inst-0123456789abcdef0123456789abcdef"

// powerAPI is one instance and the power action on it. refuse, when set,
// answers the action instead of accepting it, and hang never answers it,
// holding the request open until the client gives up, or for 5 seconds.
type powerAPI struct {
	refuse func(w http.ResponseWriter)
	hang   bool

	mu    sync.Mutex
	sent  []string // the action in each power request's path
	reads int
}

func (a *powerAPI) serve(t *testing.T) *httptest.Server {
	t.Helper()
	const path = "/v1/regions/test/projects/test/compute/instances/" + testPowerInstance
	instance := map[string]any{"id": testPowerInstance, "state": "running", "hostname": "srv-gpu-1"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == path:
			a.reads++
			writeTestJSON(t, w, http.StatusOK, instance)
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, path+"/"):
			a.sent = append(a.sent, strings.TrimPrefix(r.URL.Path, path+"/"))
			if a.hang {
				// Let go after a while, so a client that never gives up
				// fails the test rather than hanging it.
				a.mu.Unlock()
				select {
				case <-r.Context().Done():
				case <-time.After(5 * time.Second):
				}
				a.mu.Lock()
				return
			}
			if a.refuse != nil {
				a.refuse(w)
				return
			}
			writeTestJSON(t, w, http.StatusAccepted, instance)
		default:
			writeTestError(t, w, http.StatusNotFound, "not found: instance")
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func (a *powerAPI) action(t *testing.T) *instancePowerAction {
	t.Helper()
	return a.actionWaiting(t, time.Minute)
}

// actionWaiting is action with the provider's operation_timeout set to wait.
func (a *powerAPI) actionWaiting(t *testing.T, wait time.Duration) *instancePowerAction {
	t.Helper()
	return &instancePowerAction{meta: testClusterMeta(t, a.serve(t).URL, wait)}
}

func powerConfig(verb string) map[string]tftypes.Value {
	return map[string]tftypes.Value{"instance_id": str(testPowerInstance), "action": str(verb)}
}

func (a *powerAPI) invoke(t *testing.T, verb string) invoked {
	t.Helper()
	return a.invokeWith(t, powerConfig(verb), time.Minute)
}

// invokeWith runs the action with set as its configuration and the
// provider's operation_timeout set to wait.
func (a *powerAPI) invokeWith(t *testing.T, set map[string]tftypes.Value, wait time.Duration) invoked {
	t.Helper()
	act := a.actionWaiting(t, wait)
	var mu sync.Mutex
	var out invoked
	resp := action.InvokeResponse{SendProgress: func(e action.InvokeProgressEvent) {
		mu.Lock()
		defer mu.Unlock()
		out.progress = append(out.progress, e.Message)
	}}
	act.Invoke(context.Background(), action.InvokeRequest{Config: actionConfig(t, act, set)}, &resp)
	out.diags = resp.Diagnostics
	return out
}

// powerRefusal answers the action with the contract's envelope, a
// details[0].reason when reason is set, and headers.
func powerRefusal(t *testing.T, status int, reason string, headers map[string]string, metadata map[string]any) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		for k, v := range headers {
			w.Header().Set(k, v)
		}
		body := map[string]any{"code": status, "message": "refused", "requestId": "req-9"}
		if reason != "" {
			detail := map[string]any{"reason": reason}
			if metadata != nil {
				detail["metadata"] = metadata
			}
			body["details"] = []any{detail}
		}
		writeTestJSON(t, w, status, map[string]any{"error": body})
	}
}

// landedAdvice is what only mayHaveLanded says.
const landedAdvice = "may or may not have run"

// An accepted action is sent once, on the path the contract names, and the
// invoke ends at the 202 without reading the instance again: nothing the
// platform reports would say the machine got there.
func TestThePowerActionIsSentOnceAndNotWaitedOn(t *testing.T) {
	t.Parallel()
	api := &powerAPI{}
	got := api.invoke(t, "powerCycle")
	if got.diags.HasError() {
		t.Fatalf("invoke: %v", got.diags.Errors())
	}
	if len(api.sent) != 1 || api.sent[0] != "powerCycle" {
		t.Errorf("sent %q, want one powerCycle", api.sent)
	}
	if api.reads != 0 {
		t.Errorf("read the instance %d times after the 202, want none", api.reads)
	}
	said := got.said()
	for _, want := range []string{"accepted powerCycle", "`running`", "doesn't report when the machine gets there"} {
		if !strings.Contains(said, want) {
			t.Errorf("the action never said %q:\n%s", want, said)
		}
	}
}

// Each refusal is reported with what it means, the action is never sent a
// second time, and an in-flight refusal says how long to wait, from the
// header first, which the details only repeat, and from the details without
// one.
func TestAPowerActionRefusalSaysWhatToDo(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		refuse func(http.ResponseWriter)
		says   string
	}{
		"in flight, Retry-After": {powerRefusal(t, http.StatusConflict, powerActionInFlight,
			map[string]string{"Retry-After": "42"}, map[string]any{"retryAfterSeconds": 40}), "about 42 seconds"},
		"in flight, metadata only": {powerRefusal(t, http.StatusConflict, powerActionInFlight,
			nil, map[string]any{"retryAfterSeconds": 17}), "about 17 seconds"},
		"in flight, no wait given": {powerRefusal(t, http.StatusConflict, powerActionInFlight, nil, nil),
			"the platform's guard interval"},
		"unsupported":  {powerRefusal(t, http.StatusConflict, powerActionUnsupported, nil, nil), "invoking again won't help"},
		"unidentified": {powerRefusal(t, http.StatusConflict, powerTargetUnidentified, nil, nil), "Invoke the action again later"},
		"drifted":      {powerRefusal(t, http.StatusConflict, reservationDrifted, nil, nil), "might not be"},
		"unreachable":  {powerRefusal(t, http.StatusNotFound, powerTargetUnreachable, nil, nil), "no longer knows the machine"},
		"no instance":  {powerRefusal(t, http.StatusNotFound, "", nil, nil), "There's no instance"},
		"forbidden":    {powerRefusal(t, http.StatusForbidden, "", nil, nil), "may change instances"},
		"deleted":      {powerRefusal(t, http.StatusGone, "", nil, nil), "was deleted"},
		// Every 5xx may have run, and none is said to be safe to repeat.
		"500": {powerRefusal(t, http.StatusInternalServerError, "", nil, nil), landedAdvice},
		"502": {powerRefusal(t, http.StatusBadGateway, "", nil, nil), landedAdvice},
		"503": {powerRefusal(t, http.StatusServiceUnavailable, "", nil, nil), landedAdvice},
		"504": {powerRefusal(t, http.StatusGatewayTimeout, "", nil, nil), landedAdvice},
		"dropped connection": {func(w http.ResponseWriter) {
			hijacker, ok := w.(http.Hijacker)
			if !ok {
				t.Error("the test server can't drop a connection")
				return
			}
			conn, _, err := hijacker.Hijack()
			if err != nil {
				t.Errorf("hijack: %v", err)
				return
			}
			_ = conn.Close()
		}, landedAdvice},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			api := &powerAPI{refuse: c.refuse}
			got := api.invoke(t, "powerOff")
			if !got.diags.HasError() {
				t.Fatal("a refused action succeeded")
			}
			said := got.said()
			if !strings.Contains(said, c.says) {
				t.Errorf("want %q in:\n%s", c.says, said)
			}
			// The platform's guard can refuse a second invoke although
			// nothing ran, so repeating one that may have run isn't safe.
			if c.says == landedAdvice && strings.Contains(said, "safe") {
				t.Errorf("calls invoking again safe:\n%s", said)
			}
			if len(api.sent) != 1 {
				t.Errorf("sent the action %d times, want once", len(api.sent))
			}
		})
	}
}

// A plan that invokes the action reads the instance and says what will be
// sent to it, and sends nothing.
func TestAPowerActionPlanWarnsAndSendsNothing(t *testing.T) {
	t.Parallel()
	api := &powerAPI{}
	act := api.action(t)
	resp := action.ModifyPlanResponse{}
	act.ModifyPlan(context.Background(), action.ModifyPlanRequest{Config: actionConfig(t, act, powerConfig("powerOff"))}, &resp)
	if resp.Diagnostics.HasError() || resp.Diagnostics.WarningsCount() != 1 {
		t.Fatalf("plan: %v", resp.Diagnostics)
	}
	w := resp.Diagnostics.Warnings()[0]
	for _, want := range []string{"powerOff", testPowerInstance, "srv-gpu-1", "`running`"} {
		if !strings.Contains(w.Summary()+w.Detail(), want) {
			t.Errorf("the warning doesn't say %q: %s: %s", want, w.Summary(), w.Detail())
		}
	}
	if len(api.sent) != 0 {
		t.Errorf("the plan sent %q", api.sent)
	}
}

// A request the platform never answers is cut off by timeouts.invoke, or by
// the provider's operation_timeout without one, and the error says it may
// have run, since it may have reached the platform first.
func TestAPowerActionTheAPINeverAnswersTimesOut(t *testing.T) {
	t.Parallel()
	timeouts := tftypes.Object{AttributeTypes: map[string]tftypes.Type{"invoke": tftypes.String}}
	block := powerConfig("powerCycle")
	block["timeouts"] = tftypes.NewValue(timeouts, map[string]tftypes.Value{"invoke": str("100ms")})
	for name, c := range map[string]struct {
		set  map[string]tftypes.Value
		wait time.Duration
	}{
		"timeouts.invoke":   {block, time.Hour},
		"operation_timeout": {powerConfig("powerCycle"), 100 * time.Millisecond},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			api := &powerAPI{hang: true}
			started := time.Now()
			got := api.invokeWith(t, c.set, c.wait)
			if took := time.Since(started); took > 3*time.Second {
				t.Errorf("took %s, so the 100ms didn't bound it", took)
			}
			if !got.diags.HasError() {
				t.Fatal("a request with no answer succeeded")
			}
			if said := got.said(); !strings.Contains(said, landedAdvice) {
				t.Errorf("want %q in:\n%s", landedAdvice, said)
			}
		})
	}
}
