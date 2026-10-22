package provider

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/k0rdent"
)

var busyRefusal = &k0rdent.APIError{StatusCode: http.StatusConflict, Status: "409 Conflict", Message: "busy"}

// sendOK answers every send with an empty 409, leaving the judging to judge.
func sendOK(sends *atomic.Int32) func(context.Context) (*http.Response, []byte, error) {
	return func(context.Context) (*http.Response, []byte, error) {
		sends.Add(1)
		return &http.Response{StatusCode: http.StatusConflict}, nil, nil
	}
}

func admitConfig(timeout time.Duration) waitConfig {
	return waitConfig{Timeout: timeout, Interval: 5 * time.Millisecond}
}

// A refusal the judge waits out is sent again until it is admitted.
func TestAwaitAdmittedSendsAgainUntilAdmitted(t *testing.T) {
	var sends atomic.Int32
	err := awaitAdmitted(context.Background(), admitConfig(time.Minute), "the write to be admitted", sendOK(&sends),
		func(context.Context, *http.Response, []byte) (bool, *k0rdent.APIError, error) {
			if sends.Load() < 3 {
				return false, busyRefusal, nil
			}
			return true, nil, nil
		})
	if err != nil || sends.Load() != 3 {
		t.Fatalf("err=%v sends=%d, want admitted on the third", err, sends.Load())
	}
}

// A timeout that runs out while the platform keeps refusing reports the
// refusal, and doesn't say the write may still be running.
func TestAwaitAdmittedReportsTheRefusalAtTimeout(t *testing.T) {
	var sends atomic.Int32
	err := awaitAdmitted(context.Background(), admitConfig(30*time.Millisecond), "the write to be admitted", sendOK(&sends),
		func(context.Context, *http.Response, []byte) (bool, *k0rdent.APIError, error) {
			return false, busyRefusal, nil
		})
	var apiErr *k0rdent.APIError
	switch {
	case !isRefusedUntilTimeout(err):
		t.Fatalf("err = %v, want the refusal through the timeout", err)
	case !errors.As(err, &apiErr) || apiErr != busyRefusal:
		t.Errorf("err doesn't unwrap to the refusal: %v", err)
	case err.Error() != busyRefusal.Error():
		t.Errorf("err reads %q, want the refusal's own %q", err, busyRefusal)
	case stillRunning(err) || isWriteInFlight(err):
		t.Errorf("a refused write reads as one that may still be running: %v", err)
	}
}

// A provisional refusal that outlasts the timeout is reported the same way.
func TestAwaitAdmittedReportsAProvisionalRefusalAtTimeout(t *testing.T) {
	var sends atomic.Int32
	cfg := admitConfig(30 * time.Millisecond)
	cfg.ProvisionalFailureWindow = time.Minute
	err := awaitAdmitted(context.Background(), cfg, "the write to be admitted", sendOK(&sends),
		func(context.Context, *http.Response, []byte) (bool, *k0rdent.APIError, error) {
			return false, busyRefusal, provisionalFailure{busyRefusal}
		})
	if !isRefusedUntilTimeout(err) {
		t.Fatalf("err = %v, want the refusal through the timeout", err)
	}
}

// An interrupt between sends is the interrupt, not the platform's last
// refusal: a Ctrl-C mustn't read as the platform saying no.
func TestAwaitAdmittedReportsAnInterruptAsOne(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var sends atomic.Int32
	err := awaitAdmitted(ctx, admitConfig(time.Minute), "the write to be admitted", sendOK(&sends),
		func(context.Context, *http.Response, []byte) (bool, *k0rdent.APIError, error) {
			if sends.Load() == 2 {
				cancel()
			}
			return false, busyRefusal, nil
		})
	switch {
	case !waitInterrupted(err):
		t.Fatalf("err = %v, want the interrupt", err)
	case isRefusedUntilTimeout(err) || isWriteInFlight(err):
		t.Errorf("an interrupt between sends reads as something else: %v", err)
	}
}

// A wait that stops with a send on its way says so, because the platform may
// have admitted it. That holds for a timeout and an interrupt alike.
func TestAwaitAdmittedSaysAWriteWasCutOff(t *testing.T) {
	for name, stop := range map[string]func(context.CancelFunc){
		"times out":      func(context.CancelFunc) {},
		"is interrupted": func(cancel context.CancelFunc) { cancel() },
	} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			var sends atomic.Int32
			err := awaitAdmitted(ctx, admitConfig(time.Minute), "the write to be admitted",
				func(ctx context.Context) (*http.Response, []byte, error) {
					if sends.Add(1) == 1 {
						return &http.Response{StatusCode: http.StatusConflict}, nil, nil
					}
					stop(cancel)
					<-ctx.Done()
					return nil, nil, ctx.Err()
				},
				func(context.Context, *http.Response, []byte) (bool, *k0rdent.APIError, error) {
					return false, busyRefusal, nil
				})
			if !isWriteInFlight(err) || !stillRunning(err) || isRefusedUntilTimeout(err) {
				t.Fatalf("err = %v, want a write cut off on its way", err)
			}
		})
	}
}

// Whatever else the judge says ends the wait with it.
func TestAwaitAdmittedEndsOnTheJudgesError(t *testing.T) {
	var sends atomic.Int32
	final := errors.New("the answer")
	err := awaitAdmitted(context.Background(), admitConfig(time.Minute), "the write to be admitted", sendOK(&sends),
		func(context.Context, *http.Response, []byte) (bool, *k0rdent.APIError, error) {
			if sends.Load() == 1 {
				return false, busyRefusal, nil
			}
			return false, nil, final
		})
	if !errors.Is(err, final) || sends.Load() != 2 {
		t.Fatalf("err=%v sends=%d, want the judge's error on the second send", err, sends.Load())
	}
}
