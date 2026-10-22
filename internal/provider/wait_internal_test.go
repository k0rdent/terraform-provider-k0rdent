package provider

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"
)

// A per-resource `timeouts` block exists to give one slow cluster longer than
// the provider default. Bounding the context again here capped it at that
// default, so raising the block did nothing.
func TestAResourceTimeoutCanExceedTheProviderDefault(t *testing.T) {
	t.Parallel()

	// What withTimeout leaves behind for a resource asking for longer.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	settleAt := time.Now().Add(150 * time.Millisecond)
	err := waitFor(ctx, waitConfig{
		Timeout:  30 * time.Millisecond, // the provider's operation_timeout
		Interval: 10 * time.Millisecond,
	}, "the cluster", func(context.Context) (bool, error) {
		return time.Now().After(settleAt), nil
	})
	if err != nil {
		t.Fatalf("the resource timeout was capped by the provider default: %v", err)
	}
}

// With no deadline from the caller, the provider default still bounds the wait.
func TestTheProviderDefaultStillBoundsAnUnboundedWait(t *testing.T) {
	t.Parallel()

	err := waitFor(context.Background(), waitConfig{
		Timeout:  30 * time.Millisecond,
		Interval: 5 * time.Millisecond,
	}, "the cluster", func(context.Context) (bool, error) { return false, nil })
	if !errors.Is(err, errWaitTimeout) {
		t.Fatalf("err = %v, want a wait timeout", err)
	}
}

// A deadline that lands during a read is still a timeout, so the caller can say
// the operation may still be running rather than report a failed request.
func TestADeadlineDuringAReadIsATimeout(t *testing.T) {
	err := waitFor(context.Background(), waitConfig{Timeout: 10 * time.Millisecond, Interval: time.Microsecond},
		"a test operation", func(ctx context.Context) (bool, error) {
			<-ctx.Done()
			return false, ctx.Err()
		})
	if !errors.Is(err, errWaitTimeout) {
		t.Fatalf("waitFor() error = %v, want the timeout sentinel", err)
	}
}

// The timeout names what it waited for after "for", so a description like
// "the cluster to finish creating" reads as a sentence.
func TestATimeoutSaysWhatItWaitedFor(t *testing.T) {
	err := waitFor(context.Background(), waitConfig{Timeout: 10 * time.Millisecond, Interval: time.Microsecond},
		"the cluster to finish creating", func(context.Context) (bool, error) { return false, nil })
	if !errors.Is(err, errWaitTimeout) {
		t.Fatalf("waitFor() error = %v, want the timeout sentinel", err)
	}
	if !regexp.MustCompile(`: waited \S+ for the cluster to finish creating, `).MatchString(err.Error()) {
		t.Errorf("waitFor() error = %q, want it to say what it waited for", err)
	}
}
