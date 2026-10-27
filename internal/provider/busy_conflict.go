package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-log/tflog"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/k0rdent"
)

// A write the platform refuses because something it depends on is mid-change.
//
// Where the platform serializes an operation, the provider waits rather
// than errors. Several writes are serialized against
// a resource they depend on: while that resource is applying a change, the
// write is refused with 409 and details[].reason CONFLICT_IN_USE. Terraform
// can't order around this, because it orders resources
// by the references in configuration and two resources that both reference
// a cluster but not each other go out together. A security-group binding
// moves the VPC to `updating`, so a peering declared in the same apply is
// refused until the render lands.
//
// The way out is the one mature providers take for a "resource busy"
// answer: wait for the busy resource to settle and send the write again,
// inside the operation's own timeout, backing off when the refusal repeats.
// It isn't done in the transport, next to the 503s in retry.go, for two
// reasons. CONFLICT_IN_USE on these operations also covers causes no wait
// clears, like a peering direction already taken, so the 409 alone can't
// say whether to wait. Reading the resource the write depends on can, and
// that read belongs to the resource that knows what it depends on. And the
// wait is a settle wait, polled on the provider's interval, not a retry
// after a throttle.
//
// So a write is waited out only when all of this holds: the answer is 409
// with the reason the decision names, CONFLICT_IN_USE for all but one, a
// resource the contract names as the cause reads one of the states the
// contract names as unsettled, and that resource has since left it. A
// CONFLICT_IN_USE whose dependency already reads `active` is sent once more,
// since the dependency may have settled between the refusal and the read,
// and a second such refusal is reported. A refusal whose dependency can't be
// read is reported at once, since nothing then says waiting helps.
//
// Sending again has to be safe as well as useful. A create names its id, and
// the converging create reads that id back after a 409 before it sends again.
// It finds nothing, or, for a security group whose old namesake is still
// tearing down, a group this apply didn't make that is on its way out. Either
// way the refused create made nothing and the resend can't make a second one. The binding and the security group update each
// carry the whole desired state, and the contract describes each refusal as
// the change not being taken, so a resend asks for exactly what the first
// one did.
//
// Every operation whose 409 the contract describes in those terms is decided
// in busyConflictDecisions, and TestEveryUnsettled409IsDecided reads the
// generated operation table to keep it that way.

// busyConflictDecisions is the decision on every POST and PATCH whose 409 the
// contract says can mean something isn't settled, keyed by operation id.
var busyConflictDecisions = map[string]busyConflictDecision{
	"createVPCPeering": {
		says: "when a VPC you can already read is not settled `active`: the local VPC, or a " +
			"`remoteVpc` in your OWN project.",
		waits: true,
	},
	"setVPCSecurityGroups": {
		says: "a second change is refused rather than layered on a render that is running, so wait " +
			"for the VPC to settle and retry",
		waits: true,
	},
	"createSubnet": {
		says:  "the referenced AddressPool is not active",
		waits: true,
	},
	"updateSecurityGroup": {
		says: "the group is not in a state that accepts updates, because it is " +
			"`creating`, `updating` or `deleting`. Poll until it settles to `active` or `failed`, then retry.",
		waits: true,
	},
	"scaleCluster": {
		says: "Scale is admitted only from the settled `active` state",
		elsewhere: "the cluster resource sends its scale inside a wait that resends the reason-less " +
			"409 while the cluster reads `active` (scaleMayRetry)",
	},
	"scaleInstanceGroup": {
		says: "scale is admitted only from the settled `active` state",
		elsewhere: "the instance group resource resends the reason-less 409 while the group reads " +
			"`active`, `creating` or `updating` (scaleOnce)",
	},
	"upgradeCluster": {
		says: "Upgrade is admitted only from the settled `active` state",
		elsewhere: "the cluster resource waits for the cluster to settle before each upgrade step, and a " +
			"step refused while another run's operation has the cluster `creating` or `updating` waits for " +
			"it to finish and plans the step again (afterRefusedUpgrade). The same 409 without a reason " +
			"also answers a `failed` cluster, which no wait clears, so it reads the cluster rather than " +
			"going through busyRetry",
	},
	"createSecurityGroup": {
		says: "A group that is still tearing down (`state: deleting`) has not released its `id` and " +
			"produces the same conflict; retry once it is gone.",
		reason: "CONFLICT_DUPLICATE",
		waits:  true,
	},
	"createCluster": {
		says: "Retrying without freeing capacity, asking for `admissible` or less, or raising the binding " +
			"limit returns the same result.",
		refused: "neither cause clears by waiting: a quota refusal needs capacity freed or a limit raised, " +
			"and the contract says a project being deleted leaves `deleting` only for a terminal state",
	},
	"createInstanceGroup": {
		says: "Retrying without freeing capacity, asking for `admissible` or less, or raising the binding " +
			"limit returns the same result.",
		refused: "neither cause clears by waiting: a duplicate id is a name in use, which the converging " +
			"create reads back, and a quota refusal needs capacity freed or a limit raised",
	},
	"cancelCluster": {
		says: "Re-read the cluster and retry against its current operation.",
		elsewhere: "cluster_cancel.go reads the cluster after any refused cancel. A settled cluster has " +
			"nothing left to cancel, which is the opposite of busy, and an operation id that moved on is " +
			"cancelled once more by its new id",
	},
	"updateFileStore": {
		says: "the store is not in a state that accepts updates, because it is `creating`, `deleting` or " +
			"`failed`, a previous capacity change is still settling its quota accounting, or the reconcile " +
			"sweep is charging the store's quota; retry shortly.",
		waits: true,
	},
	"updateCluster": {
		says: "A `storage` raise on a cluster that is not `ACTIVE` answers this `409` without a reason; one " +
			"whose quota accounting is still settling or whose ceiling changed concurrently answers the same " +
			"and can be retried shortly.",
		elsewhere: "the cluster resource sends a storage raise as a PATCH of its own after the security groups " +
			"settle, and resends it after the cluster settles when it meets the reason-less 409, at most " +
			"maxStorageRaises times (raiseStorage)",
	},
	"createFileStore": {
		says: "`details[].reason` is `CONFLICT_DUPLICATE` when a store with the supplied `id` already exists " +
			"in the project",
		refused: "no cause clears by waiting: the converging create reads a duplicate id back, capacity and " +
			"quota refusals need capacity freed or a limit raised, and a project being deleted only ends",
	},
	"updateNetwork": {
		says:    "is managed by the cluster lifecycle, so it cannot be modified via the tenant API.",
		refused: "a network a cluster provisioned stays read-only, which no wait changes",
	},
	"updateInstanceGroup": {
		says: "If the group is not in a settled state (it is `creating`, `updating` or `deleting`), poll " +
			"until it settles to `active`/`failed` and retry.",
		refused: "that cause answers a `securityGroups` change, and the instance group resource's update " +
			"sends only `displayName` and `description`. The causes it can meet, an immutable field or " +
			"a concurrent writer, don't clear by waiting",
	},
	"executeInstancePowerAction": {
		says: "a power action was already accepted against that server within the guard interval",
		refused: "the guard interval passing is the wait, and what clears it is another power action " +
			"being accepted, which a second send would be. A power action twice isn't the same as once, " +
			"so the power action reports the wait and leaves the resend to whoever invoked it",
	},
}

// busyConflictDecision is one operation's entry in busyConflictDecisions.
// Exactly one of waits, elsewhere and refused is set.
type busyConflictDecision struct {
	// says is the contract's own words for the unsettled cause, quoted from
	// the operation's 409 description with its line breaks folded to spaces.
	says string
	// waits says the 409 is waited out here, through a busyRetry.
	waits bool
	// reason is the details[].reason a waited-out refusal carries,
	// CONFLICT_IN_USE when empty.
	reason string
	// elsewhere names the code that already waits it out, and how.
	elsewhere string
	// refused says why a 409 the contract calls unsettled isn't waited out.
	refused string
}

// dependency is a resource a write is refused against while it is mid-change.
type dependency struct {
	// name is the resource as a sentence names it, e.g. `the VPC "near"`.
	name string
	// state reads its state. A read that doesn't answer the resource returns
	// ok false, so the refusal stands.
	state func(ctx context.Context) (state string, ok bool, err error)
	// busy are the states the contract names as unsettled.
	busy []string
}

// dependsOn builds a dependency from a stateRead and the kind's state.
func dependsOn[T any](name string, read stateRead[T], state func(T) (string, bool), busy ...string) dependency {
	return dependency{
		name: name,
		busy: busy,
		state: func(ctx context.Context) (string, bool, error) {
			body, _, _, err := read(ctx)
			if err != nil || body == nil {
				return "", false, err
			}
			s, ok := state(*body)
			return s, ok, nil
		},
	}
}

// busyRetry decides what one write does with a refusal a dependency explains.
// It counts the refusals it has waited out, so it lives as long as one write.
type busyRetry struct {
	operation string
	// reason is the details[].reason of a refusal worth reading further.
	reason string
	cfg    waitConfig
	deps   []dependency

	// backoffCap bounds the pause added before a repeated refusal's wait. The
	// pause starts at the poll interval and doubles.
	backoffCap time.Duration
	jitter     func(time.Duration) time.Duration
	sleep      func(context.Context, time.Duration) error

	// resentSettled says the write was already sent once more on a refusal
	// whose dependency read settled, which happens at most once.
	resentSettled bool

	// refusals is how many refusals this write has waited out.
	refusals int
}

// newBusyRetry is the busyRetry for one write. operation has to be one that
// busyConflictDecisions says waits, or this is a programming error.
func newBusyRetry(operation string, cfg waitConfig, deps ...dependency) *busyRetry {
	d, ok := busyConflictDecisions[operation]
	if !ok || !d.waits {
		panic(fmt.Sprintf("%s is not decided as waiting out an unsettled 409", operation))
	}
	reason := d.reason
	if reason == "" {
		reason = conflictInUse
	}
	return &busyRetry{
		operation:  operation,
		reason:     reason,
		cfg:        cfg,
		deps:       deps,
		backoffCap: time.Minute,
		jitter:     fullJitter,
		sleep:      sleepOrCancel,
	}
}

// afterConflict reads a write's refusal. It returns true when the write is
// worth sending again: the refusal carried the decision's reason, a
// dependency read an unsettled state, and it has since settled. err is a wait that ended first,
// to be reported with the refusal through report.
func (b *busyRetry) afterConflict(ctx context.Context, resp *http.Response, body []byte) (bool, error) {
	if b == nil || resp == nil || resp.StatusCode != http.StatusConflict {
		return false, nil
	}
	var refusal *k0rdent.APIError
	if !errors.As(k0rdent.CheckResponse(resp, body), &refusal) || !refusal.HasReason(b.reason) {
		return false, nil
	}

	dep, state, found, settledDep := b.busyDependency(ctx)
	if !found {
		return b.resendOnceSettled(ctx, settledDep, resp, body)
	}

	// A repeated refusal means something keeps moving the dependency, so the
	// next wait starts later each time, spread so that runs contending for one
	// resource don't come back in step.
	b.refusals++
	if b.refusals > 1 {
		if err := b.sleep(ctx, b.jitter(b.backoff(b.refusals-2))); err != nil {
			return false, fmt.Errorf("%s kept refusing while %s was mid-change, and the wait "+
				"before sending again ended: %w", b.operation, dep.name, err)
		}
	}

	tflog.Info(ctx, "the platform refused the write while something it depends on is mid-change, "+
		"so it's sent again once that settles", withRequestID(map[string]any{
		"operation": b.operation,
		"waiting":   dep.name,
		"state":     state,
		"refusals":  b.refusals,
	}, resp, body))
	if err := b.awaitLeaving(ctx, dep); err != nil {
		return false, fmt.Errorf("%s read %q, so this apply waited for it to settle before sending "+
			"%s again, and the wait ended: %w", dep.name, state, b.operation, err)
	}
	return true, nil
}

// resendOnceSettled answers a CONFLICT_IN_USE whose dependency already reads
// `active`. The dependency may have been mid-change when the platform refused
// and settled a moment before the read, and reporting the refusal then loses
// a race the user has no part in. So the write goes once more, after a
// jittered pause of one poll interval. The same 409 also means causes no
// wait clears, like a peering direction already taken, so a second refusal
// on a settled dependency is the answer and is reported.
func (b *busyRetry) resendOnceSettled(ctx context.Context, dep *dependency, resp *http.Response, body []byte) (bool, error) {
	if dep == nil || b.reason != conflictInUse || b.resentSettled {
		return false, nil
	}
	b.resentSettled = true
	if err := b.sleep(ctx, b.jitter(b.backoff(0))); err != nil {
		return false, fmt.Errorf("%s refused while %s read %q, and the wait before sending once "+
			"more ended: %w", b.operation, dep.name, settledState, err)
	}
	tflog.Info(ctx, "the platform refused the write, and what it depends on already reads settled, "+
		"so it's sent once more", withRequestID(map[string]any{
		"operation":  b.operation,
		"dependency": dep.name,
	}, resp, body))
	return true, nil
}

// settledState is the state every dependency's kind calls settled.
const settledState = "active"

// conflictInUse is the refusal a dependency mid-change answers with on every
// operation but one.
const conflictInUse = "CONFLICT_IN_USE"

// busyDependency is the first dependency that reads an unsettled state. When
// none does, settled is the first that reads `active`, or nil.
func (b *busyRetry) busyDependency(ctx context.Context) (dep dependency, state string, found bool, settled *dependency) {
	for _, dep := range b.deps {
		state, ok, err := dep.state(ctx)
		if err != nil {
			tflog.Debug(ctx, "could not read what a refused write depends on, so the refusal stands",
				map[string]any{"operation": b.operation, "dependency": dep.name, "error": redactDiagnosticText(err.Error())})
			continue
		}
		if ok && slices.Contains(dep.busy, state) {
			return dep, state, true, nil
		}
		if ok && state == settledState && settled == nil {
			settled = &dep
		}
	}
	return dependency{}, "", false, settled
}

// awaitLeaving waits for dep to read anything but an unsettled state. Where
// it lands isn't judged here: the resent write gets the platform's answer
// to that, and a refusal from a settled dependency is reported.
func (b *busyRetry) awaitLeaving(ctx context.Context, dep dependency) error {
	return waitFor(ctx, b.cfg, dep.name+" to settle", func(ctx context.Context) (bool, error) {
		state, ok, err := dep.state(ctx)
		if err != nil {
			return false, err
		}
		return !ok || !slices.Contains(dep.busy, state), nil
	})
}

// backoff is the ceiling for the nth pause: the poll interval doubled per
// repeat, capped.
func (b *busyRetry) backoff(n int) time.Duration {
	return backoffCeiling(b.cfg, n, b.backoffCap)
}

// backoffCeiling is the ceiling for the nth pause before a repeated wait: the
// poll interval doubled per repeat, capped at most.
func backoffCeiling(cfg waitConfig, n int, most time.Duration) time.Duration {
	d := cfg.Interval
	if d <= 0 {
		d = DefaultPollInterval
	}
	for i := 0; i < n && d < most; i++ {
		d *= 2
	}
	return min(d, most)
}

// route is an operation's path template from the generated table, split
// into segments, so a path the contract moves moves with it.
type route []string

// routeOf is operation id's route. An id the client doesn't have is a
// programming error.
func routeOf(id string) route {
	op, ok := k0rdent.Operations[id]
	if !ok {
		panic("the client has no operation " + id)
	}
	return strings.Split(strings.Trim(op.Path, "/"), "/")
}

// params matches a path, or a reference such as a self_link, against r, and
// returns its parameters by name. The route has to match the end of the
// path, so a reference that carries the endpoint's own prefix still matches.
// The path is read escaped, so an id carrying an escaped slash stays one
// segment.
func (r route) params(ref string) (map[string]string, bool) {
	u, err := url.Parse(ref)
	if err != nil {
		return nil, false
	}
	got := strings.Split(strings.Trim(u.EscapedPath(), "/"), "/")
	if len(got) < len(r) {
		return nil, false
	}
	got = got[len(got)-len(r):]
	out := map[string]string{}
	for i, want := range r {
		name, isParam := strings.CutPrefix(want, "{")
		if !isParam {
			if want != got[i] {
				return nil, false
			}
			continue
		}
		v, err := url.PathUnescape(got[i])
		if err != nil || v == "" {
			return nil, false
		}
		out[strings.TrimSuffix(name, "}")] = v
	}
	return out, true
}

// path fills r's parameters in from values, each escaped as one segment.
func (r route) path(values map[string]string) string {
	out := make([]string, len(r))
	for i, seg := range r {
		if name, isParam := strings.CutPrefix(seg, "{"); isParam {
			seg = url.PathEscape(values[strings.TrimSuffix(name, "}")])
		}
		out[i] = seg
	}
	return "/" + strings.Join(out, "/")
}

// sendUntilSettled sends a write, and sends it again for as long as each
// refusal is one afterConflict says to wait out. send keeps the answer it
// got in the caller's own variables, and the caller reads the last one as
// usual. A non-nil error is a wait that ended first, which the caller reports
// through report with that last answer.
func (b *busyRetry) sendUntilSettled(ctx context.Context, send func(context.Context) (*http.Response, []byte, error)) error {
	for {
		resp, body, err := send(ctx)
		if err == nil {
			again, waitErr := b.afterConflict(ctx, resp, body)
			if waitErr != nil {
				return waitErr
			}
			if again {
				continue
			}
		}
		return nil
	}
}

// report is addAPIError for a refusal whose wait ended, with the wait's
// account after the platform's.
func (b *busyRetry) report(diags *diag.Diagnostics, what apiCall, resp *http.Response, body []byte, waitErr error) {
	var refused diag.Diagnostics
	addAPIError(&refused, what, resp, body, nil)
	for _, d := range refused.Errors() {
		diags.AddError(d.Summary(), d.Detail()+"\n\n"+redactDiagnosticText(waitErr.Error()+requestIDLine(requestIDIn(waitErr))))
	}
}
