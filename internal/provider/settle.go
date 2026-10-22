package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-log/tflog"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/k0rdent"
)

// What a waiter does with each state a resource can report.
//
// Every kind of resource waits twice: for a create or update to settle, and
// for a delete to finish. Each kind declares one table per wait, mapping each
// state the contract names to a verdict, and the tables are the whole of the
// policy. The kinds differ on purpose, and each difference is explained next to
// the table entry that makes it.
//
// Four sets, not three. A state the contract does not name stops the wait at
// once and says which value it was, rather than burning the timeout against a
// draft API that added one. So a table lists every state it expects, and
// anything else is unknownStateError.
//
// A delete waits for a 404, which every contract calls the end state, and
// still reads the state on the way, so that a delete the platform refused is
// reported rather than waited out. See the cluster's delete table.

// verdict is what one reading means to a waiter.
type verdict int

const (
	// unrecognised is the zero value, so a state a table leaves out stops the
	// wait rather than being waited on.
	unrecognised verdict = iota
	// keepWaiting polls again.
	keepWaiting
	// settled ends the wait: the resource is usable, or gone.
	settled
	// settledOnceReady ends the wait once the kind's ready check passes. It is
	// for a state that alone does not say whether a change is still landing.
	settledOnceReady
	// failsProvisionally is a failure tolerated for ProvisionalFailureWindow.
	// See provisionalFailure.
	failsProvisionally
	// failsTerminally is a failure reported on sight.
	failsTerminally
)

// The wordings a failure can take. Each is a format given the kind's subject
// and the state.
const (
	// reportsAs is the default.
	reportsAs = "the platform reports %s as %s"
	// afterDelete is for a state a delete should have moved the resource out of.
	afterDelete = "the platform reports %s as %q after the delete was accepted"
)

// stateRule is what a table says about one state.
type stateRule struct {
	verdict verdict
	// says is the failure's wording, reportsAs when empty.
	says string
	// detail appends the platform's own account of the failure, as the kind's
	// detail reads it.
	detail bool
}

// stateTable is one kind's policy for one wait.
type stateTable struct {
	// missing is the rule for a body that reports no state at all.
	missing stateRule
	// states maps each state the contract names to its rule.
	states map[string]stateRule
}

// stateKind is everything the settle module needs to know about a kind of
// resource whose body is T.
type stateKind[T any] struct {
	// name is the kind as unknownStateError and a failed read name it.
	name string
	// subject is the kind in a failure's wording, "the <name>" when empty.
	subject string
	// state reads the state from a body, and whether the body reported one.
	state func(T) (string, bool)
	// detail renders the platform's account of a failure, with stateDetail.
	detail func(T) string
	// ready is the check settledOnceReady defers to.
	ready func(T) (bool, error)

	// settle is the policy while a create or update settles.
	settle stateTable
	// delete is the policy while a delete finishes.
	delete stateTable

	// deleting is the state the kind reads while a delete is running. A 409
	// on a delete of a resource that reads it is that delete, not a refusal.
	// See startDelete.
	deleting string

	// released are the states in which a read may answer blank for what the
	// create sent, because a delete has released it. See teardown_read.go.
	released []string

	// resource is the type suffix a failed delete's `removed` advice names.
	// Empty for a kind no resource deletes.
	resource string
	// long says a create and a delete build or free hardware, so they
	// default to waitConfig.long.
	long bool
}

// timeout is the kind's default create and delete timeout. Update's resume
// of a create reads it too, so the two agree.
func (k stateKind[T]) timeout(cfg waitConfig) time.Duration {
	if k.long {
		return cfg.long()
	}
	return cfg.Timeout
}

// stateRead is one GET of a resource, as the generated client returns it.
type stateRead[T any] func(context.Context) (body *T, resp *http.Response, raw []byte, err error)

// decide is the whole policy: one body in, one verdict out, with the error a
// failure reports.
func (k stateKind[T]) decide(table stateTable, body T) (verdict, error) {
	state, reported := k.state(body)
	rule := table.missing
	if reported {
		rule = table.states[state]
	}

	switch rule.verdict {
	case settled, keepWaiting:
		return rule.verdict, nil
	case settledOnceReady:
		ready, err := k.ready(body)
		switch {
		case err != nil:
			return failsTerminally, err
		case ready:
			return settled, nil
		default:
			return keepWaiting, nil
		}
	case failsProvisionally, failsTerminally:
		subject := k.subject
		if subject == "" {
			subject = "the " + k.name
		}
		says := rule.says
		if says == "" {
			says = reportsAs
		}
		msg := fmt.Sprintf(says, subject, state)
		if rule.detail && k.detail != nil {
			msg += k.detail(body)
		}
		return rule.verdict, platformRefusal{msg}
	default:
		return unrecognised, unknownStateError(k.name, state)
	}
}

// platformRefusal is a wait the platform ended by reporting a state the
// operation can't come back from, such as a delete it settled `failed`.
type platformRefusal struct{ msg string }

func (p platformRefusal) Error() string { return p.msg }

// refusedByPlatform reports whether err is a wait the platform ended that way.
func refusedByPlatform(err error) bool {
	var r platformRefusal
	return errors.As(err, &r)
}

// awaitSettled waits for a create or update to settle. A read that does not
// answer the resource, a 404 included, ends the wait.
func (k stateKind[T]) awaitSettled(ctx context.Context, cfg waitConfig, what string, read stateRead[T]) error {
	var last string
	err := waitFor(ctx, cfg, what, func(ctx context.Context) (bool, error) {
		body, resp, raw, err := read(ctx)
		if err != nil {
			return false, err
		}
		if body == nil {
			return false, fmt.Errorf("reading the %s while waiting: %w", k.name,
				k0rdent.CheckResponse(resp, raw))
		}
		last, _ = k.state(*body)
		return probeVerdict(k.decide(k.settle, *body))
	})
	return withLastState(err, last)
}

// awaitDeleted waits for a delete to finish, which a 404 says it has.
func (k stateKind[T]) awaitDeleted(ctx context.Context, cfg waitConfig, what string, read stateRead[T]) error {
	var last string
	err := waitFor(ctx, cfg, what, func(ctx context.Context) (bool, error) {
		body, resp, raw, err := read(ctx)
		if err != nil {
			return false, err
		}
		if isNotFound(resp, raw) {
			return true, nil
		}
		if body == nil {
			return false, k0rdent.CheckResponse(resp, raw)
		}
		last, _ = k.state(*body)
		return probeVerdict(k.decide(k.delete, *body))
	})
	return withLastState(err, last)
}

// What a destroy makes of its DELETE's answer.
type deleteStart int

const (
	// deleteFailed was reported, and there's nothing to wait for.
	deleteFailed deleteStart = iota
	// deleteRefused is a 409 that isn't a delete already running. It was
	// reported, and the caller may add what would make it actionable.
	deleteRefused
	// deleteGone means the resource is already gone.
	deleteGone
	// deleteAccepted means a delete is running, and awaitDeleted waits it out.
	deleteAccepted
)

// startDelete sends a delete and says what the destroy should do next.
//
// A 409 is read before it's reported. The retry layer resends a DELETE that
// met a 502, a 504 or a dropped connection, and the platform may have acted
// on the first one. The resend then meets the provider's own delete, and
// a kind that refuses a delete while one is running answers it with a 409.
// A read tells the two apart: a resource that reads k.deleting, or is
// already gone, has a delete to wait out, and anything else is the refusal
// the platform meant. A failed read leaves the 409 as the answer.
//
// The resource is read before the DELETE too. A destroy whose wait timed out
// or was interrupted leaves the resource in state, `deleting`, and the next
// destroy finds it so. That delete is the one to wait for: a second DELETE
// would only meet a 409 on most kinds. A failed read sends the DELETE anyway,
// and the 409 handling above still applies.
func (k stateKind[T]) startDelete(ctx context.Context, what apiCall, send func(context.Context) (*http.Response, []byte, error), read stateRead[T], diags *diag.Diagnostics) deleteStart {
	if k.deleting != "" {
		if body, readResp, readRaw, readErr := read(ctx); readErr == nil {
			if isNotFound(readResp, readRaw) {
				return deleteGone
			}
			if state, ok := k.stateOf(body); ok && state == k.deleting {
				tflog.Info(ctx, "the "+k.name+" is already being deleted, so that delete is waited out rather than sent again",
					map[string]any{"state": state})
				return deleteAccepted
			}
		}
	}
	resp, raw, err := send(ctx)
	switch {
	case err != nil:
		addAPIError(diags, what, nil, nil, err)
		return deleteFailed
	case isNotFound(resp, raw):
		return deleteGone
	case resp != nil && resp.StatusCode < 300:
		return deleteAccepted
	case resp == nil || resp.StatusCode != http.StatusConflict:
		addAPIError(diags, what, resp, raw, nil)
		return deleteFailed
	}

	if k.deleting != "" {
		if body, readResp, readRaw, readErr := read(ctx); readErr == nil {
			if isNotFound(readResp, readRaw) {
				return deleteGone
			}
			if state, ok := k.stateOf(body); ok && state == k.deleting {
				tflog.Info(ctx, "the delete was refused because the "+k.name+" is already being deleted, so it's waited out",
					withRequestID(map[string]any{"state": state}, resp, raw))
				return deleteAccepted
			}
		}
	}
	addAPIError(diags, what, resp, raw, nil)
	return deleteRefused
}

// destroy starts a delete and waits for the 404, reporting any failure. It
// returns startDelete's verdict.
func (k stateKind[T]) destroy(ctx context.Context, cfg waitConfig, send func(context.Context) (*http.Response, []byte, error), read stateRead[T], diags *diag.Diagnostics) deleteStart {
	started := k.startDelete(ctx, apiCall("delete the "+k.name), send, read, diags)
	if started == deleteAccepted {
		k.reportDeleteWait(diags, k.awaitDeleted(ctx, cfg, "the "+k.name+" to finish deleting", read))
	}
	return started
}

// reportDeleteWait reports a delete wait that failed with err.
func (k stateKind[T]) reportDeleteWait(diags *diag.Diagnostics, err error) {
	if err != nil {
		addDeleteWaitError(diags, k.name, ResourceName(k.resource), "The "+k.name+" did not finish deleting", err)
	}
}

// stateOf is k.state on a body that may be missing.
func (k stateKind[T]) stateOf(body *T) (string, bool) {
	if body == nil {
		return "", false
	}
	return k.state(*body)
}

// probeVerdict turns a verdict into what waitFor's probe answers.
func probeVerdict(v verdict, err error) (bool, error) {
	switch v {
	case settled:
		return true, nil
	case keepWaiting:
		return false, nil
	case failsProvisionally:
		return false, provisionalFailure{err}
	default:
		return false, err
	}
}
