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
	"strconv"
	"strings"
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
//   - A subscription's delivery history is read by its id, and an id the
//     caller's organization doesn't have answers an empty page, not a 404.
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
	// deliveries is the delivery history, newest first.
	deliveries []map[string]any

	// state and reads are what the object table needs from every row. A
	// subscription has no lifecycle state of its own, so it's always
	// `active` and nothing moves it.
	state string
	reads int
}

func (h *fakeWebhook) life() (*string, *int, *string) { return &h.state, &h.reads, nil }

// webhookOverlap is the contract's fixed overlap window, "24 hours in every
// deployment and not configurable".
const webhookOverlap = 24 * time.Hour

// webhookSecretMinLength is the contract's minLength on a caller's secret.
const webhookSecretMinLength = 32

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
	if strings.Contains(h.url, webhookDeliveryHistory) {
		h.deliveries = seededDeliveries(h.createdAt)
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

// webhookDeliveryHistory, in a subscription's URL, gives it a delivery
// history when it's created. The fake sends no events, so a subscription
// without it has none, as the platform answers for an endpoint nothing has
// been sent to yet.
const webhookDeliveryHistory = "fake-delivery-history"

// webhookDeliveryPage is the most deliveries the fake answers a page with,
// whatever `limit` asks for.
// ASSUMPTION: the contract calls `limit` the page size and doesn't promise a
// page that full. A page this small means a handful of deliveries spans
// several, so a provider that reads one page and stops fails here.
const webhookDeliveryPage = 2

// seededDeliveries is one of every shape the contract gives a delivery,
// newest first: queued and never tried, failing with a retry due, failed with
// no answer and dead-lettered, and delivered twice. `response_code` and
// `last_error` are left out where there's none, as the contract's optional
// fields, and the nullable times are sent as null.
func seededDeliveries(created time.Time) []map[string]any {
	after := func(minutes int) string {
		return created.Add(time.Duration(minutes) * time.Minute).Format(time.RFC3339)
	}
	return []map[string]any{
		{"id": newUUID(), "status": "queued", "attempts": 0, "created_at": after(5),
			"settled_at": nil, "next_attempt_at": nil, "retry_scheduled": false},
		{"id": newUUID(), "status": "failed", "attempts": 2, "response_code": 503,
			"last_error": "endpoint answered 503 Service Unavailable", "created_at": after(4),
			"settled_at": nil, "next_attempt_at": after(10), "retry_scheduled": true},
		{"id": newUUID(), "status": "failed", "attempts": 8,
			"last_error": "dial tcp: connection refused", "created_at": after(3),
			"settled_at": after(9), "next_attempt_at": nil, "retry_scheduled": false},
		{"id": newUUID(), "status": "delivered", "attempts": 1, "response_code": 200, "created_at": after(2),
			"settled_at": after(2), "next_attempt_at": nil, "retry_scheduled": false},
		{"id": newUUID(), "status": "delivered", "attempts": 1, "response_code": 204, "created_at": after(1),
			"settled_at": after(1), "next_attempt_at": nil, "retry_scheduled": false},
	}
}

// listWebhookDeliveries answers a page of one subscription's history, newest
// first, behind a cursor that's valid only for that subscription.
//
// What it follows from the contract: `limit` is 1 to 200 and 50 by default,
// and anything else is a 400. So is a repeated parameter, and a cursor this
// server can't decode. A present but empty `cursor=` is read as absent. An id
// the caller's organization doesn't have is an empty page, not an error.
// ASSUMPTION: an id that isn't a UUID is a 400, as a parameter the operation
// can't read.
func (s *state) listWebhookDeliveries(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	for name, values := range q {
		if len(values) > 1 {
			apiError(w, http.StatusBadRequest, "INVALID_ARGUMENT", name+" is repeated; send each parameter once")
			return
		}
	}
	id := r.PathValue("id")
	if len(id) != 36 || strings.Count(id, "-") != 4 {
		apiError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "id is not a UUID")
		return
	}
	limit := 50
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 200 {
			apiError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "limit must be between 1 and 200, got "+raw)
			return
		}
		limit = n
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	var history []map[string]any
	if h, ok := webhookKind.rows(s.global())[at{id: id}]; ok {
		history = h.deliveries
	}
	start := 0
	if raw := q.Get("cursor"); raw != "" {
		owner, offset, found := strings.Cut(raw, ":")
		n, err := strconv.Atoi(offset)
		if !found || owner != id || err != nil || n < 0 || n > len(history) {
			apiError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "cursor is not one this server issued")
			return
		}
		start = n
	}
	end := min(start+min(limit, webhookDeliveryPage), len(history))
	page := make([]any, 0, end-start)
	for _, d := range history[start:end] {
		page = append(page, d)
	}
	body := map[string]any{"deliveries": page}
	if end < len(history) {
		body["next_cursor"] = id + ":" + strconv.Itoa(end)
	}
	writeJSON(w, http.StatusOK, body)
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
