package provider

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// The plan judges grants from the last refresh. A group the platform has since
// called no longer grantable, deleted and made again under its id, must not
// be sent: that grants whatever machines the id names now.
func TestFileStoreUpdateJudgesGrantsAgainstTheLiveStore(t *testing.T) {
	var mu sync.Mutex
	var order []string
	var sent string
	body := fileStoreBody("active")
	body["consumerStatus"] = []any{map[string]any{
		"ref": "trainers", "state": "failed", "stateReason": "GROUP_NO_LONGER_GRANTABLE", "statusReason": "GROUP_NO_LONGER_GRANTABLE",
	}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		order = append(order, r.Method)
		if r.Method == http.MethodPatch {
			raw, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("read PATCH: %v", err)
			}
			sent = string(raw)
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(body); err != nil {
			t.Errorf("write: %v", err)
		}
	}))
	defer server.Close()
	r := &fileStoreResource{meta: testClusterMeta(t, server.URL, time.Minute)}
	resp := runFileStoreUpdate(r, t,
		map[string]tftypes.Value{"etag": str("e1"), "consumers": groupConsumerValue(t, r, "trainers")},
		map[string]tftypes.Value{"etag": str("e1"), "consumers": mergeConsumers(t, r, "trainers", "10.0.0.31")})
	patched := false
	for _, m := range order {
		patched = patched || m == http.MethodPatch
	}
	refused := false
	for _, d := range resp.Diagnostics.Errors() {
		refused = refused || d.Summary() == "A consumer group can no longer be granted"
	}
	if patched || !refused {
		t.Errorf("want no PATCH and the refusal: requests %v, PATCH %q, diagnostics %v", order, sent, resp.Diagnostics.Errors())
	}
}

// The same holds for the resend after a busy refusal: the wait can be long
// enough for a group to be made again under its id, so the newest read is
// judged before `consumers` goes again.
func TestFileStoreResendJudgesGrantsAgainstTheNewestRead(t *testing.T) {
	var mu sync.Mutex
	var patches, gets int
	healthy := fileStoreBody("active")
	creating := fileStoreBody("creating")
	ungrantable := fileStoreBody("active")
	ungrantable["consumerStatus"] = []any{map[string]any{
		"ref": "trainers", "state": "failed", "stateReason": "GROUP_NO_LONGER_GRANTABLE", "statusReason": "GROUP_NO_LONGER_GRANTABLE",
	}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPatch {
			patches++
			w.WriteHeader(http.StatusConflict)
			if _, err := w.Write([]byte(inUseBody)); err != nil {
				t.Errorf("write: %v", err)
			}
			return
		}
		gets++
		body := ungrantable
		switch gets {
		case 1:
			body = healthy
		case 2:
			body = creating
		}
		if err := json.NewEncoder(w).Encode(body); err != nil {
			t.Errorf("write: %v", err)
		}
	}))
	defer server.Close()
	r := &fileStoreResource{meta: testClusterMeta(t, server.URL, time.Minute)}
	resp := runFileStoreUpdate(r, t,
		map[string]tftypes.Value{"etag": str("e1"), "consumers": groupConsumerValue(t, r, "trainers")},
		map[string]tftypes.Value{"etag": str("e1"), "consumers": mergeConsumers(t, r, "trainers", "10.0.0.31")})
	refused := false
	for _, d := range resp.Diagnostics.Errors() {
		refused = refused || d.Summary() == "A consumer group can no longer be granted"
	}
	if patches != 1 || !refused {
		t.Errorf("patches %d, diagnostics %v", patches, resp.Diagnostics.Errors())
	}
}
