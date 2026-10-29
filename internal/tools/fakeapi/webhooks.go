package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"time"
)

// Webhook subscriptions live on the global tier and belong to the caller's
// organization. The fake mints one credential per run, so it holds one
// organization's subscriptions and nobody else's.
//
// What this follows from the contract, because the provider branches on each:
//
//   - The platform picks the id, a UUID. Nothing a client sends names one.
//   - A signing secret is answered once, by the create or the rotate that set
//     it, and the list never carries one. The store read the harness has is
//     the only way to see one again, which is how a test proves a rotation
//     really changed what the platform signs with.
//   - There is no read of one subscription and no update. Create, list,
//     delete and rotate are the whole surface.
//   - A rotation keeps the previous secret for 24 hours unless asked not to,
//     and `previous_secret_expires_at` is reported only while that's open.
//   - A delete is synchronous: 204, and the next list doesn't have it.
//   - The platform turns a subscription off after sustained delivery failure,
//     and nothing turns it back on. The list still has it, with `is_active`
//     false and `disabled_at` set, and a rotate or a delete still works on it.
//     The fake sends no events, so the harness turns one off by hand, at
//     POST /_harness/webhooks/{id}/disable.
type fakeWebhook struct {
	id             string
	url            string
	eventTypes     []string // as sent
	secret         string
	previousSecret string
	version        int
	createdAt      time.Time
	rotatedAt      *time.Time
	previousUntil  *time.Time
	// disabledAt is when the platform turned the subscription off, nil while
	// it's on.
	disabledAt *time.Time

	// state and reads are what the object table needs from every row. A
	// subscription has no lifecycle state of its own, so it's always
	// `active` and nothing moves it. Being turned off is `disabledAt`.
	state string
	reads int
}

func (h *fakeWebhook) life() (*string, *int, *string) { return &h.state, &h.reads, nil }

// webhookOverlap is the contract's fixed overlap window, "24 hours in every
// deployment and not configurable".
const webhookOverlap = 24 * time.Hour

// webhookSecretMinLength is the contract's minLength on a caller's secret.
const webhookSecretMinLength = 32

// The contract's defaults for turning a subscription off: at least 10
// dead-lettered deliveries in a row, in a run that began at least 72 hours
// ago.
const (
	webhookDisableAfterFailures = 10
	webhookDisableAfter         = 72 * time.Hour
)

// listWebhooks answers every subscription, newest first.
//
// ASSUMPTION: the order. The contract gives none, and newest first is the
// order the delivery history uses, so a provider that took a subscription's
// place in the list for anything would be caught as the list grows.
//
// ASSUMPTION: an omitted filter reads back as an empty list, not absent or
// null. The contract says "empty/omitted = all" and declares an array, so a
// provider has to read the two as the same thing either way.
//
// ASSUMPTION: a filter reads back in the reverse of the order it was sent.
// The contract gives no order for it, and an echo of the configuration's
// order would let a provider that stored the filter as a list pass.
func (s *state) listWebhooks(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	hooks := webhookKind.rows(s.global())
	rows := make([]*fakeWebhook, 0, len(hooks))
	for _, h := range hooks {
		rows = append(rows, h)
	}
	sort.Slice(rows, func(i, j int) bool {
		if !rows[i].createdAt.Equal(rows[j].createdAt) {
			return rows[i].createdAt.After(rows[j].createdAt)
		}
		return rows[i].id > rows[j].id
	})
	items := make([]any, 0, len(rows))
	for _, h := range rows {
		items = append(items, h.listed(time.Now()))
	}
	writeJSON(w, http.StatusOK, map[string]any{"subscriptions": items})
}

// listed is a subscription as the list answers it: no secret.
func (h *fakeWebhook) listed(now time.Time) map[string]any {
	events := slices.Clone(h.eventTypes)
	slices.Reverse(events)
	if events == nil {
		events = []string{}
	}
	out := map[string]any{
		"id":                         h.id,
		"url":                        h.url,
		"event_types":                events,
		"secret_version":             h.version,
		"is_active":                  true,
		"created_at":                 h.createdAt.Format(time.RFC3339),
		"rotated_at":                 nil,
		"previous_secret_expires_at": nil,
		"last_success_at":            nil,
		"consecutive_failures":       0,
		"failing_since":              nil,
		"disabled_at":                nil,
	}
	if h.disabledAt != nil {
		out["is_active"] = false
		out["disabled_at"] = h.disabledAt.Format(time.RFC3339)
		out["consecutive_failures"] = webhookDisableAfterFailures
		out["failing_since"] = h.disabledAt.Add(-webhookDisableAfter).Format(time.RFC3339)
	}
	if h.rotatedAt != nil {
		out["rotated_at"] = h.rotatedAt.Format(time.RFC3339)
	}
	if h.previousUntil != nil && h.previousUntil.After(now) {
		out["previous_secret_expires_at"] = h.previousUntil.Format(time.RFC3339)
	}
	return out
}

// stored is everything the store holds for a subscription, secrets included,
// for the harness's own read and nothing else.
func (h *fakeWebhook) stored() map[string]any {
	out := h.listed(time.Now())
	out["event_types"] = h.eventTypes
	out["signing_secret"] = h.secret
	out["previous_secret"] = ""
	if h.previousUntil != nil && h.previousUntil.After(time.Now()) {
		out["previous_secret"] = h.previousSecret
	}
	return out
}

func (s *state) createWebhook(w http.ResponseWriter, r *http.Request) {
	var body struct {
		URL        string    `json:"url"`
		EventTypes *[]string `json:"event_types"`
		Secret     *string   `json:"secret"`
	}
	if !decode(w, r, &body) {
		return
	}
	if !webhookURLAccepted(w, body.URL) || !webhookSecretAccepted(w, body.Secret) {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	h := &fakeWebhook{
		id:        newUUID(),
		url:       body.URL,
		version:   1,
		createdAt: time.Now().UTC().Truncate(time.Second),
		state:     "active",
	}
	if body.EventTypes != nil {
		h.eventTypes = slices.Clone(*body.EventTypes)
	}
	h.secret = generatedWebhookSecret()
	if body.Secret != nil {
		h.secret = *body.Secret
	}
	webhookKind.put(s.global(), "", h.id, h)
	writeJSON(w, http.StatusCreated, map[string]any{
		"id":             h.id,
		"signing_secret": h.secret,
		"created_at":     h.createdAt.Format(time.RFC3339),
	})
}

func (s *state) deleteWebhook(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h, where, ok := webhookKind.find(w, r, s.global())
	if !ok {
		return
	}
	webhookKind.drop(s.global(), where, h)
	w.WriteHeader(http.StatusNoContent)
}

// rotateWebhook takes an optional body: "Send no body at all to generate a
// new secret with the default 24-hour overlap."
func (s *state) rotateWebhook(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Secret  *string `json:"secret"`
		Overlap *bool   `json:"overlap"`
	}
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		apiError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "cannot read the request body")
		return
	}
	if len(bytes.TrimSpace(raw)) > 0 {
		r.Body = io.NopCloser(bytes.NewReader(raw))
		if !decode(w, r, &body) {
			return
		}
	}
	if !webhookSecretAccepted(w, body.Secret) {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	h, _, ok := webhookKind.find(w, r, s.global())
	if !ok {
		return
	}
	now := time.Now().UTC().Truncate(time.Second)
	overlap := body.Overlap == nil || *body.Overlap
	// "Only the most recent previous secret is retained", so a second
	// rotation inside a window parks the one being rotated out and drops the
	// one parked before it.
	h.previousSecret, h.previousUntil = "", nil
	if overlap {
		until := now.Add(webhookOverlap)
		h.previousSecret, h.previousUntil = h.secret, &until
	}
	h.secret = generatedWebhookSecret()
	if body.Secret != nil {
		h.secret = *body.Secret
	}
	h.version++
	h.rotatedAt = &now

	answer := map[string]any{
		"id":                         h.id,
		"signing_secret":             h.secret,
		"secret_version":             h.version,
		"previous_secret_expires_at": nil,
		"rotated_at":                 now.Format(time.RFC3339),
	}
	if h.previousUntil != nil {
		answer["previous_secret_expires_at"] = h.previousUntil.Format(time.RFC3339)
	}
	writeJSON(w, http.StatusOK, answer)
}

// storedWebhook is the harness's read of one subscription, the route the API
// doesn't have. It's served only behind the owner token, by storeRead.
func (s *state) storedWebhook(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h, ok := webhookKind.read(w, r, s.global())
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, h.stored())
}

// disableWebhook turns a subscription off as the platform's delivery worker
// does after sustained failure: `is_active` false and `disabled_at` now. One
// already off keeps the time it was turned off. It's the harness's alone,
// behind the owner token, since no API route turns a subscription off.
func (s *state) disableWebhook(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h, _, ok := webhookKind.find(w, r, s.global())
	if !ok {
		return
	}
	if h.disabledAt == nil {
		now := time.Now().UTC().Truncate(time.Second)
		h.disabledAt = &now
	}
	w.WriteHeader(http.StatusNoContent)
}

// webhookURLAccepted is the contract's "absolute http(s) endpoint".
// ASSUMPTION: refused with a 422, as the operation's response list suggests.
func webhookURLAccepted(w http.ResponseWriter, raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		apiError(w, http.StatusUnprocessableEntity, "INVALID_ARGUMENT",
			fmt.Sprintf("url %q is not an absolute http(s) URL", raw))
		return false
	}
	return true
}

// webhookSecretAccepted is the contract's minLength on a caller's secret.
func webhookSecretAccepted(w http.ResponseWriter, secret *string) bool {
	if secret != nil && len(*secret) < webhookSecretMinLength {
		apiError(w, http.StatusUnprocessableEntity, "INVALID_ARGUMENT",
			fmt.Sprintf("secret must be at least %d characters", webhookSecretMinLength))
		return false
	}
	return true
}

// generatedWebhookSecret is the secret the platform makes when it isn't
// given one.
func generatedWebhookSecret() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return "whsec_fake_" + hex.EncodeToString(b)
}

// newUUID is a random version 4 UUID, in the lower-case form the platform
// writes ids in.
func newUUID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// churnWebhook varies a marked subscription between reads, in the delivery
// health the list rolls up and nothing else. `last_success_at`,
// `consecutive_failures` and `failing_since` move with every delivery, and
// none of them is anything a configuration sets.
func churnWebhook(obj map[string]any, read int64) {
	switch read % 3 {
	case 0:
		obj["last_success_at"] = churnTime(read)
		obj["consecutive_failures"] = 0
		obj["failing_since"] = nil
	case 1:
		obj["consecutive_failures"] = 2
		obj["failing_since"] = churnTime(read - 60)
	default:
		obj["last_success_at"] = nil
	}
	if events, ok := obj["event_types"].([]any); ok && read%2 == 1 {
		slices.Reverse(events)
	}
}
