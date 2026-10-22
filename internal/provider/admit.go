package provider

import (
	"context"
	"errors"
	"net/http"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/k0rdent"
)

// A cluster's and an instance group's scale, and a file store's delete, are
// refused while the object is mid-change and safe to resend, since a refusal
// writes nothing. Each caller judges its own refusals. The wait's endings are
// shared here so a Ctrl-C never reads as the platform's last refusal.

// admitJudge reads one answer. admitted ends the wait, and a refusal with a
// nil error is waited out. Any other error ends it at once, a
// provisionalFailure only after its window. A refusal returned with an error
// is still kept, so a provisionalFailure that outlasts the timeout reports it.
type admitJudge func(ctx context.Context, resp *http.Response, body []byte) (admitted bool, refusal *k0rdent.APIError, err error)

// awaitAdmitted resends a write until judge admits it. A failed wait returns
// refusedUntilTimeout on a timeout after a refusal, writeInFlight if it
// stopped mid-send, or the wait's own error.
func awaitAdmitted(ctx context.Context, cfg waitConfig, what string,
	send func(context.Context) (*http.Response, []byte, error), judge admitJudge,
) error {
	var last *k0rdent.APIError
	inFlight := false
	err := waitFor(ctx, cfg, what, func(ctx context.Context) (bool, error) {
		resp, body, err := send(ctx)
		inFlight = err != nil && ctx.Err() != nil
		if err != nil {
			return false, err
		}
		admitted, refusal, err := judge(ctx, resp, body)
		if refusal != nil {
			last = refusal
		}
		return admitted, err
	})
	switch {
	case err == nil:
		return nil
	case inFlight && stillRunning(err):
		return writeInFlight{err}
	case last != nil && stillRunning(err) && !waitInterrupted(err):
		return refusedUntilTimeout{last}
	}
	return err
}

// refusedUntilTimeout is the last refusal before the timeout. It isn't
// errWaitTimeout, because nothing is still running.
type refusedUntilTimeout struct{ *k0rdent.APIError }

func (e refusedUntilTimeout) Unwrap() error { return e.APIError }

// writeInFlight is a wait that stopped mid-send, so the platform may have
// admitted the write.
type writeInFlight struct{ err error }

func (e writeInFlight) Error() string { return e.err.Error() }
func (e writeInFlight) Unwrap() error { return e.err }

func isRefusedUntilTimeout(err error) bool {
	var refused refusedUntilTimeout
	return errors.As(err, &refused)
}

func isWriteInFlight(err error) bool {
	var cut writeInFlight
	return errors.As(err, &cut)
}
