package provider

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// waitConfig bounds one lifecycle wait.
type waitConfig struct {
	// Timeout is the longest the wait may run before giving up.
	Timeout time.Duration
	// Long is the default for a cluster's and an instance group's create and
	// delete, which take hours on hardware. Zero means Timeout. See
	// DefaultLongOperationTimeout.
	Long time.Duration
	// Interval is how long to sleep between reads.
	Interval time.Duration
	// ProvisionalFailureWindow is how long a provisionalFailure is tolerated
	// before it is reported. Zero means terminal on sight.
	ProvisionalFailureWindow time.Duration
	// Raise names the settings that lengthen this wait, for the timeout
	// message. Empty means a resource's `timeouts` block and
	// `operation_timeout`.
	Raise string
}

// long is the default for a create or delete that builds or frees hardware.
func (c waitConfig) long() time.Duration {
	if c.Long > 0 {
		return c.Long
	}
	return c.Timeout
}

// provisionalFailure marks a failure the API says a poller must not stop on.
// waitFor tolerates it for provisionalFailureWindow and then reports it.
//
// ClusterState in the contract: "`failed` indicates the last lifecycle
// transition failed ... and is not a stop condition for a poller: a
// watchdog-force-failed cluster may still return to `active` with no user
// action". Failing on sight taints a cluster that was about to come good.
// Treating it as pending burns the whole timeout. So it is tolerated, briefly.
//
// Clusters, and a network straight after its delete is accepted, which the
// contract does not promise reads `deleting` at once, and an instance group
// whose create a delete cancelled, which reads `failed` on its way to gone.
// KubeconfigStatus calls `failed` terminal outright.
type provisionalFailure struct{ err error }

func (p provisionalFailure) Error() string { return p.err.Error() }
func (p provisionalFailure) Unwrap() error { return p.err }

// errWaitTimeout is returned when a wait stops before the operation settles,
// because its timeout ran out or because the run was interrupted. It is a
// sentinel so callers can say "this is still in progress" rather than "this
// failed", which is the difference between a user waiting and a user deleting.
var errWaitTimeout = errors.New("timed out waiting for the operation to settle")

// errWaitInterrupted is the part of errWaitTimeout an interrupt caused rather
// than a timeout. Terraform cancels the provider's context on Ctrl-C.
var errWaitInterrupted = errors.New("interrupted while waiting for the operation to settle")

// stoppedWaiting is the error a wait returns when it stops before the
// operation settles. It is errWaitTimeout either way, and errWaitInterrupted
// too when the run was interrupted. It says how far the object had got, so the
// user knows what is still running.
type stoppedWaiting struct {
	// what follows "for", as in "the cluster to finish creating".
	what   string
	waited time.Duration
	// interrupted is true when the context was cancelled rather than timed out.
	interrupted bool
	// last is the state the last read reported, empty when none was read.
	last string
	// raise names the settings that lengthen the wait.
	raise string
}

func (e stoppedWaiting) Error() string {
	var b strings.Builder
	if e.interrupted {
		fmt.Fprintf(&b, "%s: the run was interrupted after %s waiting for %s",
			errWaitInterrupted, e.waited.Round(time.Second), e.what)
	} else {
		fmt.Fprintf(&b, "%s: waited %s for %s, which is as long as the timeout allows",
			errWaitTimeout, e.waited.Round(time.Second), e.what)
	}
	if e.last != "" {
		fmt.Fprintf(&b, ". It was still `%s` at the last read", e.last)
	}
	b.WriteString(". The operation may still be running on the platform: the provider never " +
		"cancels one because it stopped waiting, so read the resource again before assuming it failed")
	if !e.interrupted {
		fmt.Fprintf(&b, ". Raise %s, or set %s, if this is normal for your landscape",
			e.raise, EnvVar(envOperationTimeout))
	}
	return b.String()
}

func (e stoppedWaiting) Is(target error) bool {
	return target == errWaitTimeout || (e.interrupted && target == errWaitInterrupted)
}

// stillRunning reports whether err is a wait that stopped while the operation
// was still going, rather than one that failed.
func stillRunning(err error) bool { return errors.Is(err, errWaitTimeout) }

// waitInterrupted reports whether err is a wait the run's interrupt stopped.
func waitInterrupted(err error) bool { return errors.Is(err, errWaitInterrupted) }

// withLastState puts the state a wait last read on a stoppedWaiting error, and
// returns any other error as it is.
func withLastState(err error, last string) error {
	var stopped stoppedWaiting
	if last == "" || !errors.As(err, &stopped) || stopped.last != "" {
		return err
	}
	stopped.last = last
	return stopped
}

// waitFor polls probe until it reports done, the context is cancelled, or the
// timeout expires.
//
// The API is asynchronous: `createCluster` answers `201` with `state: creating`
// and `scaleCluster` answers `200` while the node count is still the old one,
// while Terraform's contract is that a resource is usable once `apply` returns.
// Waiting lives in the provider rather than the client because the layer that
// knows which lifecycle transition it is in is the layer that knows what
// settled means. A waiter in the client would also collapse a `404` to a nil
// result, which in Terraform is the difference between no change and destroy
// and recreate.
//
// It does not retry a failed request: a probe that returns an error ends the
// wait. A rejected mint must not be replayed, because it spends one of ten per
// minute and a revoked credential answers the same `401`, and a `409` during a
// lifecycle change means something real. Telling those apart is not a loop's
// job.
//
// A wait that stops early returns stoppedWaiting, a provisionalFailure still
// inside its window included: the contract says the platform may yet recover
// from one.
func waitFor(ctx context.Context, cfg waitConfig, what string, probe func(context.Context) (bool, error)) error {
	interval := cfg.Interval
	if interval <= 0 {
		interval = DefaultPollInterval
	}
	// The caller may already have bounded the context from the resource's own
	// `timeouts` block. Bounding it again with the provider default would cap
	// that, so raising a per-resource timeout above `operation_timeout` would
	// do nothing, which is the main reason anyone sets one.
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = DefaultOperationTimeout
	}
	if _, bounded := ctx.Deadline(); !bounded {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	raise := cfg.Raise
	if raise == "" {
		raise = "the resource's `timeouts` block, or `operation_timeout` in the provider block"
	}

	started := time.Now()
	var failingSince time.Time
	stopped := func() error {
		// `what` reads "the cluster to finish creating", so it follows "for".
		return stoppedWaiting{
			what: what, waited: time.Since(started), raise: raise,
			interrupted: !errors.Is(ctx.Err(), context.DeadlineExceeded),
		}
	}
	for {
		// Probe before sleeping. A fast operation should not pay a poll
		// interval to notice it already finished, and the fake control plane
		// used for local verification settles immediately.
		done, err := probe(ctx)
		var provisional provisionalFailure
		switch {
		case errors.As(err, &provisional):
			if failingSince.IsZero() {
				failingSince = time.Now()
			}
			if held := time.Since(failingSince); held >= cfg.ProvisionalFailureWindow {
				return fmt.Errorf("%w, and stayed that way for %s",
					provisional.err, held.Round(time.Second))
			}
		case err != nil && ctx.Err() != nil:
			// The deadline or an interrupt landed during a read. That stops
			// the wait. It isn't a failed read.
			return stopped()
		case err != nil:
			return err
		default:
			// Leaving the failure state restarts the window.
			failingSince = time.Time{}
		}
		if done {
			return nil
		}

		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return stopped()
		case <-timer.C:
		}
	}
}

// unknownStateError reports a state the contract does not define. The provider
// cannot tell progress from a dead end, so it stops now and names the value
// rather than burning the full timeout on a draft API.
func unknownStateError(what, state string) error {
	return fmt.Errorf(
		"the platform reports the %s as %q, which this provider does not recognise. "+
			"Either the landscape is running a newer API than this provider was built "+
			"against, or the value is a typo upstream. Check the resource in the console",
		what, state)
}
