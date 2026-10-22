package provider

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// busyStore is a store that reads `creating` until it has been read `creating` times
// and then settles `active` under a new etag, as a store another run is still
// creating does. A PATCH is refused with CONFLICT_IN_USE while it's
// `creating`, with CONFLICT_VERSION when it carries an etag other than the
// current one, and taken otherwise.
type busyStore struct {
	api      *storeAPI
	creating int
	// settlesAs is the store as it reads once settled, before the etag.
	settlesAs map[string]any
	// inUseWhileActive is how many PATCHes an `active` store refuses with
	// CONFLICT_IN_USE first, as one whose quota accounting is settling does.
	inUseWhileActive int
}

func newBusyStore(t *testing.T, creating int) *busyStore {
	b := &busyStore{creating: creating, settlesAs: fileStoreBody("active")}
	b.api = &storeAPI{get: b.read}
	b.api.patch = func(w http.ResponseWriter) {
		sent := b.api.patches[len(b.api.patches)-1]
		now := b.read(b.api.reads)
		switch {
		case now["state"] != "active":
			refuse(t, w, `{"error":{"code":409,"message":"the store is creating and does not accept updates",`+
				`"details":[{"reason":"CONFLICT_IN_USE"}]}}`)
		case b.inUseWhileActive > 0:
			b.inUseWhileActive--
			refuse(t, w, `{"error":{"code":409,"message":"a capacity change is still settling",`+
				`"details":[{"reason":"CONFLICT_IN_USE"}]}}`)
		case sent["etag"] != nil && sent["etag"] != now["etag"]:
			refuse(t, w, `{"error":{"code":409,"message":"the store changed since it was read",`+
				`"details":[{"reason":"CONFLICT_VERSION"}]}}`)
		default:
			if err := json.NewEncoder(w).Encode(now); err != nil {
				t.Errorf("write: %v", err)
			}
		}
	}
	return b
}

func (b *busyStore) read(reads int) map[string]any {
	if reads < b.creating {
		return fileStoreBody("creating") // etag e1
	}
	out := map[string]any{}
	for k, v := range b.settlesAs {
		out[k] = v
	}
	out["etag"] = "e2"
	return out
}

func refuse(t *testing.T, w http.ResponseWriter, body string) {
	w.WriteHeader(http.StatusConflict)
	if _, err := w.Write([]byte(body)); err != nil {
		t.Errorf("write: %v", err)
	}
}

// etags is the etag each PATCH carried.
func (a *storeAPI) etags() []any {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := []any{}
	for _, p := range a.patches {
		out = append(out, p["etag"])
	}
	return out
}

func updateBusyStore(t *testing.T, b *busyStore, timeout time.Duration) (*fileStoreResource, []string) {
	t.Helper()
	r := &fileStoreResource{meta: testClusterMeta(t, b.api.serve(t).URL, timeout)}
	resp := runFileStoreUpdate(r, t, map[string]tftypes.Value{
		"etag": str("e1"), "state": str("creating"),
	}, map[string]tftypes.Value{"etag": str("e1"), "description": str("later")})
	var errs []string
	for _, d := range resp.Diagnostics.Errors() {
		errs = append(errs, d.Summary()+"\n"+d.Detail())
	}
	return r, errs
}

// An update that meets a store still `creating` waits for it to settle and
// sends the update again, with the etag the settled store reads. The etag
// state held was read while the store was `creating`, and the platform would
// refuse it as a concurrent edit.
func TestFileStoreUpdateWaitsForACreatingStore(t *testing.T) {
	b := newBusyStore(t, 3)
	if _, errs := updateBusyStore(t, b, time.Minute); len(errs) > 0 {
		t.Fatalf("Update: %v", errs)
	}
	if got := b.api.etags(); len(got) != 2 || got[0] != "e1" || got[1] != "e2" {
		t.Errorf("PATCHes carried etags %v, want e1, then e2 once the store settled", got)
	}
}

// Something else changed the store while the update waited. Its etag moved
// for a reason that isn't the settle, so the update goes with the etag state
// holds, and the platform refuses it as the concurrent edit it is.
func TestFileStoreUpdateAfterAWaitKeepsTheEtagWhenTheStoreChanged(t *testing.T) {
	b := newBusyStore(t, 3)
	b.settlesAs["description"] = "someone else's"
	_, errs := updateBusyStore(t, b, time.Minute)
	if len(errs) == 0 {
		t.Fatal("an update over a concurrent edit reported success")
	}
	if !strings.Contains(errs[0], "the store changed since it was read") {
		t.Errorf("the error isn't the platform's refusal of the stale etag:\n%s", errs[0])
	}
	if got := b.api.etags(); len(got) != 2 || got[1] != "e1" {
		t.Errorf("PATCHes carried etags %v, want the second to keep e1", got)
	}
}

// A store that is still `creating` when the update timeout runs out is
// reported with the platform's refusal and what the wait was for.
func TestFileStoreUpdateReportsAStoreThatOutlastsTheWait(t *testing.T) {
	b := newBusyStore(t, 1<<30)
	_, errs := updateBusyStore(t, b, 50*time.Millisecond)
	if len(errs) == 0 {
		t.Fatal("an update that never got through reported success")
	}
	for _, want := range []string{"409", "CONFLICT_IN_USE", "creating", "settle"} {
		if !strings.Contains(errs[0], want) {
			t.Errorf("the error doesn't say %q:\n%s", want, errs[0])
		}
	}
	if got := len(b.api.etags()); got != 1 {
		t.Errorf("%d PATCHes, want the one refused", got)
	}
}

// The contract's other unsettled CONFLICT_IN_USE, a capacity change still
// settling its quota accounting, comes from a store that reads `active`. It is
// sent once more, and a second such refusal is the answer.
func TestFileStoreUpdateSendsOnceMoreWhenAnActiveStoreIsInUse(t *testing.T) {
	for _, tc := range []struct {
		refusals, patches int
		fails             bool
	}{
		{refusals: 1, patches: 2},
		{refusals: 2, patches: 2, fails: true},
	} {
		b := newBusyStore(t, 0)
		b.inUseWhileActive = tc.refusals
		_, errs := updateBusyStore(t, b, time.Minute)
		if (len(errs) > 0) != tc.fails {
			t.Errorf("%d refusals: errors %v, want failure %v", tc.refusals, errs, tc.fails)
		}
		if got := len(b.api.etags()); got != tc.patches {
			t.Errorf("%d refusals: %d PATCHes, want %d", tc.refusals, got, tc.patches)
		}
	}
}

// A store being deleted doesn't come back, so nothing is waited for.
func TestFileStoreUpdateDoesNotWaitOnADeletingStore(t *testing.T) {
	b := newBusyStore(t, 0)
	b.api.get = func(int) map[string]any { return fileStoreBody("deleting") }
	_, errs := updateBusyStore(t, b, time.Minute)
	if len(errs) == 0 {
		t.Fatal("an update of a store being deleted reported success")
	}
	if got := len(b.api.etags()); got != 1 {
		t.Errorf("%d PATCHes, want the one refused", got)
	}
}

// The wait sits in front of the 422 handling and doesn't add to it. A drift
// re-send refused with 422 after the wait is sent once more without
// `consumers`, as without the wait, and that one carries the settled etag too.
func TestFileStoreUpdateWaitsThenSettlesARefusedResendAsBefore(t *testing.T) {
	b := newBusyStore(t, 3)
	b.settlesAs["consumers"] = []any{map[string]any{
		"type": "instanceGroup", "ref": "trainers", "accessLevel": "rw",
		"network": map[string]any{"type": "ethernet"},
	}}
	settled := b.api.patch
	b.api.patch = func(w http.ResponseWriter) {
		sent := b.api.patches[len(b.api.patches)-1]
		if _, resend := sent["consumers"]; resend && b.read(b.api.reads)["state"] == "active" {
			unprocessable(t, w)
			return
		}
		settled(w)
	}
	resp := updateWithPrivate(t, b.api.serve(t).URL, nil)
	if resp.Diagnostics.HasError() {
		t.Fatalf("Update: %v", resp.Diagnostics.Errors())
	}
	if got := b.api.etags(); len(got) != 3 || got[0] != "e1" || got[1] != "e2" || got[2] != "e2" {
		t.Errorf("PATCHes carried etags %v, want e1 refused while creating, then e2 twice", got)
	}
	if !hasWarning(resp.Diagnostics, "a member cannot be vouched for") {
		t.Errorf("the 422 isn't reported as the re-send's: %v", resp.Diagnostics)
	}
}
