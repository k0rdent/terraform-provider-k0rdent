package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A subscription through the API and the store, the way the provider and the
// harness reach it: the secret is answered once and never listed, a rotation
// parks the old secret for the overlap unless told not to, and a delete is
// gone at once.
func TestWebhookSubscriptionLifecycle(t *testing.T) {
	h, secrets := guardedFake(t)
	send := func(method, path, bearer, body string) (int, map[string]any) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Host = "127.0.0.1:" + testPort
		req.Header.Set("Authorization", "Bearer "+bearer)
		rec := serve(h, req)
		out := map[string]any{}
		if rec.Body.Len() > 0 {
			if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
				t.Fatalf("%s %s: %v: %s", method, path, err, rec.Body)
			}
		}
		return rec.Code, out
	}
	const hooks = "/v1/regions/global/notifications/webhooks"
	first := strings.Repeat("a", 32)
	second := strings.Repeat("b", 32)

	for _, refused := range []string{
		`{"url":"ftp://hooks.example.test/x"}`,
		`{"url":"/relative"}`,
		`{"url":"https://hooks.example.test/x","secret":"short"}`,
	} {
		if code, _ := send(http.MethodPost, hooks, secrets.AccessToken, refused); code != http.StatusUnprocessableEntity {
			t.Errorf("%s: %d, want 422", refused, code)
		}
	}

	code, created := send(http.MethodPost, hooks, secrets.AccessToken,
		`{"url":"https://hooks.example.test/x","event_types":["a.b.c","d.e.f"],"secret":"`+first+`"}`)
	if code != http.StatusCreated || created["signing_secret"] != first {
		t.Fatalf("create: %d %v", code, created)
	}
	id, ok := created["id"].(string)
	if !ok {
		t.Fatalf("create answered no id: %v", created)
	}

	_, list := send(http.MethodGet, hooks, secrets.AccessToken, "")
	body, err := json.Marshal(list)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), first) || !strings.Contains(string(body), id) {
		t.Errorf("the list carries the secret, or not the subscription: %s", body)
	}

	_, stored := send(http.MethodGet, "/_harness/store"+hooks+"/"+id, secrets.OwnerToken, "")
	if stored["signing_secret"] != first {
		t.Errorf("the store holds %v, want the configured secret", stored["signing_secret"])
	}
	if read, _ := send(http.MethodGet, hooks+"/"+id, secrets.AccessToken, ""); read == http.StatusOK {
		t.Error("the API answers a read of one subscription, which the contract has no operation for")
	}

	code, rotated := send(http.MethodPost, hooks+"/"+id+"/rotate", secrets.AccessToken, `{"secret":"`+second+`"}`)
	if code != http.StatusOK || rotated["secret_version"] != float64(2) || rotated["previous_secret_expires_at"] == nil {
		t.Fatalf("rotate: %d %v", code, rotated)
	}
	_, stored = send(http.MethodGet, "/_harness/store"+hooks+"/"+id, secrets.OwnerToken, "")
	if stored["signing_secret"] != second || stored["previous_secret"] != first {
		t.Errorf("after an overlapping rotation the store holds %v and %v", stored["signing_secret"], stored["previous_secret"])
	}

	code, rotated = send(http.MethodPost, hooks+"/"+id+"/rotate", secrets.AccessToken, `{"overlap":false}`)
	if code != http.StatusOK || rotated["previous_secret_expires_at"] != nil || rotated["signing_secret"] == second {
		t.Fatalf("cutover: %d %v", code, rotated)
	}
	_, stored = send(http.MethodGet, "/_harness/store"+hooks+"/"+id, secrets.OwnerToken, "")
	if stored["previous_secret"] != "" {
		t.Error("a cutover kept the previous secret")
	}
	// No body at all is the default rotation.
	if bare, _ := send(http.MethodPost, hooks+"/"+id+"/rotate", secrets.AccessToken, ""); bare != http.StatusOK {
		t.Errorf("a rotation with no body: %d", bare)
	}

	if deleted, _ := send(http.MethodDelete, hooks+"/"+id, secrets.AccessToken, ""); deleted != http.StatusNoContent {
		t.Fatalf("delete: %d", deleted)
	}
	code, gone := send(http.MethodDelete, hooks+"/"+id, secrets.AccessToken, "")
	if _, envelope := gone["error"]; code != http.StatusNotFound || !envelope {
		t.Errorf("a second delete: %d %v, want the service's 404", code, gone)
	}
	if code, _ := send(http.MethodGet, "/_harness/store"+hooks+"/"+id, secrets.OwnerToken, ""); code != http.StatusNotFound {
		t.Errorf("the store still has the deleted subscription: %d", code)
	}
}
