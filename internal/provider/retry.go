package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"net/http"
	"strconv"
	"syscall"
	"time"

	"github.com/hashicorp/terraform-plugin-log/tflog"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/k0rdent"
)

// rateLimitTransport retries a 429, and a 503 the platform says is safe to
// send again, so an apply that meets a throttle or a lock waits instead of
// failing partway through.
//
// A 429 is retried on every method: it says the request was not acted on.
//
// A 503 is retried on two grounds. A GET, HEAD, PUT or DELETE can be sent
// twice without changing the outcome, so its 503 is waited out whatever the
// cause. A POST or PATCH is waited out only when the contract says that
// operation's 503 wrote nothing and the request is safe to resend. Those are
// decided in post503Decisions. Any other 503 on a POST or PATCH, like the
// contract's generic ServiceUnavailable on createCluster, is a missing
// dependency on a request that may not be repeatable, and is reported at once.
//
// A 502 or 504, and a connection dropped before any answer, are retried on a
// GET, HEAD, PUT or DELETE only. Each says the service may or may not have
// acted, which only a request that can be sent twice can shrug off. A refused
// connection isn't retried at all: that's an endpoint nothing listens on, and
// a minute of backoff would only delay saying so.
//
// Nothing else is retried. A 5xx on a POST may have been acted on, a 500 is a
// fault that sending again repeats, a 401 from a revoked credential looks like
// an expired one, and a 409 means something real.
//
// Retry-After is optional. The API sends it on a per-account 429 and on the
// batchCreate lock timeout, and the gateway's per-address 429 never does, so
// without it the transport backs off on its own capped, fully jittered
// schedule. With it, the wait is never shorter than what was asked, because a
// request sent early still counts against the window. Up to backoffBase more
// is added on top, so that concurrent applies told the same number don't all
// come back in the same instant. A Retry-After of 0, or a date already past,
// names no wait at all and is treated as no header, for the same reason.
type rateLimitTransport struct {
	base http.RoundTripper

	// retries bounds how many times one request is re-sent. Six full-length
	// backoffs add up to just over the 60-second window the platform counts in.
	retries int

	// backoffBase and backoffCap shape the schedule used when the platform
	// names no wait.
	backoffBase, backoffCap time.Duration

	// maxWait bounds a Retry-After when the request has no deadline of its
	// own, so a misconfigured hour-long wait surfaces instead of looking hung.
	maxWait time.Duration

	now    func() time.Time
	jitter func(time.Duration) time.Duration
	sleep  func(context.Context, time.Duration) error
}

func newRateLimitTransport(base http.RoundTripper) rateLimitTransport {
	return rateLimitTransport{
		base:        base,
		retries:     6,
		backoffBase: time.Second,
		backoffCap:  30 * time.Second,
		maxWait:     5 * time.Minute,
		now:         time.Now,
		jitter:      fullJitter,
		sleep:       sleepOrCancel,
	}
}

func (t rateLimitTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.base == nil {
		return nil, errNoBase("retry layer")
	}
	ctx := req.Context()

	for attempt := 0; ; attempt++ {
		resp, err := t.base.RoundTrip(req)
		if err != nil {
			if !t.mayResend(req, attempt) || !droppedConnection(ctx, req, err) {
				return resp, err
			}
			if waitErr := t.waitAfterDrop(ctx, req, err, attempt); waitErr != nil {
				return nil, waitErr
			}
		} else {
			operation, _ := k0rdent.OperationIDFromContext(ctx)
			what, retry := retryable(req.Method, operation, resp.StatusCode)
			if !retry || !t.mayResend(req, attempt) {
				return resp, nil
			}
			giveBack, err := t.waitAfterAnswer(ctx, req, resp, what, attempt)
			if err != nil {
				return nil, err
			}
			if giveBack {
				return resp, nil
			}
		}

		if req.GetBody != nil {
			body, err := req.GetBody()
			if err != nil {
				return nil, err
			}
			req.Body = body
		}
	}
}

// mayResend is whether attempt can be followed by another: the budget isn't
// spent, and a body that was sent can be sent again.
func (t rateLimitTransport) mayResend(req *http.Request, attempt int) bool {
	return attempt < t.retries && (req.Body == nil || req.GetBody != nil)
}

// waitAfterAnswer sleeps out a deferral the platform answered with. It hands
// the response back instead when waiting would end past the deadline, since
// the platform's own answer says more than a deadline error. Otherwise the
// response is consumed here.
func (t rateLimitTransport) waitAfterAnswer(ctx context.Context, req *http.Request, resp *http.Response, what string, attempt int) (giveBack bool, err error) {
	wait, asked := retryAfter(resp.Header, t.now())
	// A wait of nothing names no moment worth coming back at, and every
	// apply that read it would come back at the same one.
	asked = asked && wait > 0
	if asked {
		limit, why := t.waitLimit(ctx)
		if wait > limit {
			return false, tooLongToWait(req, resp, wait, what, why)
		}
		// The spread is dropped rather than trimmed when it won't fit: a
		// retry that lands on the deadline loses the platform's answer.
		if spread := t.jitter(t.backoffBase); wait+spread <= limit {
			wait += spread
		}
	} else {
		wait = t.jitter(t.backoff(attempt))
		if t.pastDeadline(ctx, wait) {
			return true, nil
		}
	}

	// Keep the platform's error for the log and for a cancelled wait, then
	// drain the rest so the connection can be reused. A failed drain only
	// costs that.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err == nil {
		_, err = io.Copy(io.Discard, resp.Body)
	}
	if err != nil {
		tflog.Trace(ctx, "could not drain a retried response", map[string]any{"error": redactDiagnosticText(err.Error())})
	}
	_ = resp.Body.Close()
	answer := k0rdent.CheckResponse(resp, body)

	tflog.Debug(ctx, what+", waiting before retrying", withRequestID(map[string]any{
		"status":         resp.StatusCode,
		"method":         req.Method,
		"path":           req.URL.EscapedPath(),
		"wait":           wait.String(),
		"platform_asked": asked,
		"attempt":        attempt + 1,
	}, resp, body))
	if err := t.sleep(ctx, wait); err != nil {
		return false, fmt.Errorf("stopped waiting to retry %s %s, which the platform had answered %w: %w",
			req.Method, req.URL.EscapedPath(), answer, err)
	}
	return false, nil
}

// waitAfterDrop sleeps out a connection that dropped before an answer, on the
// transport's own schedule since there's no header to read. It returns the
// drop itself when waiting would end past the deadline.
func (t rateLimitTransport) waitAfterDrop(ctx context.Context, req *http.Request, dropped error, attempt int) error {
	wait := t.jitter(t.backoff(attempt))
	if t.pastDeadline(ctx, wait) {
		return dropped
	}
	tflog.Debug(ctx, whyDropped+", waiting before retrying", map[string]any{
		"method":  req.Method,
		"path":    req.URL.EscapedPath(),
		"wait":    wait.String(),
		"attempt": attempt + 1,
		"error":   redactDiagnosticText(dropped.Error()),
	})
	if err := t.sleep(ctx, wait); err != nil {
		return fmt.Errorf("stopped waiting to retry %s %s after %w: %w", req.Method, req.URL.EscapedPath(), dropped, err)
	}
	return nil
}

// pastDeadline is whether a wait would end after the request's deadline.
func (t rateLimitTransport) pastDeadline(ctx context.Context, wait time.Duration) bool {
	deadline, ok := ctx.Deadline()
	return ok && wait > deadline.Sub(t.now())
}

// droppedConnection is whether err is a connection that closed or reset
// before the platform answered, on a request that can be sent twice. A
// cancelled or expired run is not a drop, whatever the error says.
func droppedConnection(ctx context.Context, req *http.Request, err error) bool {
	if ctx.Err() != nil || !idempotentMethod(req.Method) {
		return false
	}
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, syscall.ECONNRESET)
}

// What the debug log and a refused wait call each retried status.
const (
	whyRateLimited = "rate limited"
	whyUnavailable = "service unavailable"
	whyGateway     = "no answer through the gateway"
	whyDropped     = "connection dropped before an answer"
)

// retryable says whether a response to method on operation is a deferral
// rather than an answer, and names it for the log. operation is the id the
// generated client put on the request, or "" for one it didn't make.
func retryable(method, operation string, status int) (string, bool) {
	switch status {
	case http.StatusTooManyRequests:
		return whyRateLimited, true
	case http.StatusServiceUnavailable:
		if idempotentMethod(method) || post503Decisions[operation].resends {
			return whyUnavailable, true
		}
	case http.StatusBadGateway, http.StatusGatewayTimeout:
		if idempotentMethod(method) {
			return whyGateway, true
		}
	}
	return "", false
}

// idempotentMethod is a method RFC 9110 calls idempotent and the API uses:
// sending it twice leaves the platform as sending it once does.
func idempotentMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPut, http.MethodDelete:
		return true
	}
	return false
}

// post503Decisions is the decision on every POST and PATCH whose 503 the
// contract describes in its own words, keyed by operation id. Every other POST
// and PATCH carries the contract's generic ServiceUnavailable, a dependency
// that isn't configured, or no documented 503 at all, and its 503 is reported
// at once.
//
// The contract carries no machine-readable mark for a 503 that's safe to
// resend, only prose, so the list can't be generated. It can't drift either:
// TestEveryPostAndPatch503IsDecided reads the generated operation table and
// fails when an operation with its own 503 is missing here, when an entry here
// no longer has one, or when the words an entry stands on are gone from the
// contract.
var post503Decisions = map[string]post503Decision{
	// The bounded wait on the VPC's row lock ran out, or a serialization
	// failure or deadlock.
	"createVPCPeering": {
		says:    "NOTHING was written: no peering was created, so unlike the `504` below this is safe to retry as-is.",
		resends: true,
	},
	// The project or the quota service couldn't be asked before the store was
	// placed.
	"createFileStore": {
		says:    "nothing was written, retry.",
		resends: true,
	},
	"updateFileStore": {
		says:    "nothing was changed, retry.",
		resends: true,
	},
	// The storage raise couldn't be checked against the quota. A PATCH
	// carries the whole of what it changes, so the same request asks for
	// the same thing again.
	"updateCluster": {
		says:    "nothing was changed, retry.",
		resends: true,
	},
	"scaleInstanceGroup": {
		says: "Nothing is written. Retry",
		refused: "the service also answers 503 there for causes that never clear, so the instance " +
			"group resource retries it itself for a short window, and a retry here as well would " +
			"multiply that window",
	},
}

// post503Decision is one operation's entry in post503Decisions. Exactly one of
// resends and refused is set.
type post503Decision struct {
	// says is the contract's own sentence the decision stands on, quoted from
	// the operation's 503 description with its line breaks folded to spaces.
	says string

	// resends says the 503 is waited out.
	resends bool

	// refused says why a 503 the contract calls retryable isn't retried here.
	refused string
}

// backoff is the ceiling for the nth header-less retry: base doubled per
// attempt, capped.
func (t rateLimitTransport) backoff(attempt int) time.Duration {
	d := t.backoffBase
	for i := 0; i < attempt && d < t.backoffCap; i++ {
		d *= 2
	}
	return min(d, t.backoffCap)
}

// waitLimit is the longest Retry-After worth honouring: the time left before
// the request's deadline, or maxWait when it has none.
func (t rateLimitTransport) waitLimit(ctx context.Context) (time.Duration, string) {
	if deadline, ok := ctx.Deadline(); ok {
		left := deadline.Sub(t.now())
		return left, fmt.Sprintf("the %s left before this operation's deadline", left.Round(time.Second))
	}
	return t.maxWait, fmt.Sprintf("the %s this provider waits at most", t.maxWait)
}

// tooLongToWait keeps the platform's own error, request id included, inside
// the one that names the wait.
func tooLongToWait(req *http.Request, resp *http.Response, wait time.Duration, what, why string) error {
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
	if err == nil {
		err = k0rdent.CheckResponse(resp, body)
	}
	return fmt.Errorf("%s on %s %s: the platform asked for a wait of %s, longer than %s: %w",
		what, req.Method, req.URL.EscapedPath(), wait, why, err)
}

// fullJitter draws a wait anywhere from nothing to the whole of d.
func fullJitter(d time.Duration) time.Duration {
	return jitterUpTo(d, rand.N[time.Duration])
}

// jitterUpTo is fullJitter with the draw passed in, so a test can seed it.
// draw(n) answers in [0, n).
func jitterUpTo(d time.Duration, draw func(time.Duration) time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	return draw(d + 1)
}

// retryAfter reads the header in both forms the specification allows, a count
// of seconds and an HTTP date. A date in the past means now: a second of clock
// skew against the gateway is not worth failing a run over.
func retryAfter(h http.Header, now time.Time) (time.Duration, bool) {
	raw := h.Get("Retry-After")
	if raw == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(raw); err == nil {
		if secs < 0 {
			return 0, false
		}
		if secs > int(math.MaxInt64/int64(time.Second)) {
			return time.Duration(math.MaxInt64), true
		}
		return time.Duration(secs) * time.Second, true
	}
	when, err := http.ParseTime(raw)
	if err != nil {
		return 0, false
	}
	if d := when.Sub(now); d > 0 {
		return d, true
	}
	return 0, true
}

// sleepOrCancel waits, and stops waiting when the operation is cancelled so
// Ctrl-C works during a throttled apply.
func sleepOrCancel(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// RetriedStatuses is every status some request is retried on, in order. It's
// read from retryable itself, asking it about every status with each method
// the API uses, so docs/known-limitations.md can be held to what the
// provider does rather than to what someone remembered it doing. A DELETE
// isn't asked about: it's retried as the other idempotent methods are, and
// destroy_rule_internal_test.go keeps it to the destroy path.
func RetriedStatuses() []int {
	var out []int
	for status := 100; status < 600; status++ {
		for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodPut,
			http.MethodPost, http.MethodPatch} {
			if _, ok := retryable(method, "", status); ok {
				out = append(out, status)
				break
			}
		}
	}
	return out
}
