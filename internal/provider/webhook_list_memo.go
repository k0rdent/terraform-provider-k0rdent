package provider

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/diag"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/k0rdent"
)

// webhookListFresh is how long one read of the subscription list answers for
// every subscription looked up after it.
//
// The contract has no read of one subscription, so finding one is the whole
// list filtered by id. Done per subscription, a configuration of n of them
// makes n lists of n rows on every plan, and that's quadratic in the very
// dimension that matters for scale. One provider process serves one plan or
// one apply, so a list shared across that process's lookups makes a refresh
// read it at most once every webhookListFresh.
//
// Shared, it's at most this old. A lookup answered from it is the same as a
// lookup made that much earlier, and a refresh's reads are spread over its
// whole walk anyway, so nothing a plan decides moves. The bound is there for
// the long-running walk, so a list read at its start doesn't answer for its
// end.
const webhookListFresh = 10 * time.Second

// webhookListMemo is the last subscription list this process read, shared by
// every lookup of one subscription.
//
// No lock is held across the network. The first lookup that finds nothing
// fresh sends the list request, and lookups that arrive while it's in flight
// wait for that one request and take its answer, an error included. So a
// platform answering 503 through a minute of retries costs a refresh one
// minute, however many subscriptions it reads, rather than a minute each.
//
// Every write this provider makes to a subscription forgets the list
// afterwards. forget also detaches the request in flight, so a lookup after
// the write sends its own, and a list that was in flight while the write
// landed is never stored. A create's own read therefore always sees the
// create. A failed list answers only the lookups that shared it, and the next
// one asks again.
type webhookListMemo struct {
	mu     sync.Mutex
	rows   []k0rdent.WebhookSubscription
	at     time.Time
	held   bool
	flight *webhookListFlight
	// writes counts forget calls, so a request that was in flight across
	// one isn't stored.
	writes uint64

	// now is the clock, for tests. Nil is time.Now.
	now func() time.Time
}

// webhookListFlight is one list request and everyone waiting on it.
type webhookListFlight struct {
	done  chan struct{}
	rows  []k0rdent.WebhookSubscription
	diags diag.Diagnostics
}

func (m *webhookListMemo) clock() time.Time {
	if m.now != nil {
		return m.now()
	}
	return time.Now()
}

// list answers the subscription list, from the one this process read last
// when that's fresh enough and no write has happened since, or from the
// request already in flight.
func (m *webhookListMemo) list(ctx context.Context, meta *Meta) ([]k0rdent.WebhookSubscription, diag.Diagnostics) {
	m.mu.Lock()
	if m.held && m.clock().Sub(m.at) < webhookListFresh {
		rows := slices.Clone(m.rows)
		m.mu.Unlock()
		return rows, nil
	}
	if f := m.flight; f != nil {
		m.mu.Unlock()
		select {
		case <-f.done:
			return slices.Clone(f.rows), f.diags
		case <-ctx.Done():
			var diags diag.Diagnostics
			diags.AddError("Stopped waiting for the webhook subscription list", ctx.Err().Error())
			return nil, diags
		}
	}
	f := &webhookListFlight{done: make(chan struct{})}
	m.flight = f
	writes := m.writes
	m.mu.Unlock()

	f.rows, f.diags = listWebhookSubscriptions(ctx, meta)

	m.mu.Lock()
	if m.flight == f {
		m.flight = nil
	}
	if !f.diags.HasError() && m.writes == writes {
		m.rows, m.at, m.held = f.rows, m.clock(), true
	}
	m.mu.Unlock()
	close(f.done)
	return slices.Clone(f.rows), f.diags
}

// forget drops the remembered list and detaches the request in flight. Every
// create, rotation and delete calls it once the platform has answered,
// whatever the answer was, since a write that failed on the way back may
// still have landed.
func (m *webhookListMemo) forget() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rows, m.held, m.flight = nil, false, nil
	m.writes++
}
