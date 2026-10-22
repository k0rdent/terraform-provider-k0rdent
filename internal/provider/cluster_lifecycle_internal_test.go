package provider

import (
	"net/http"
	"strings"
	"testing"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/k0rdent"
)

// The states the contract names, and one it does not.
var everyClusterState = append(k0rdent.ClusterStateValues(), "quiescing")

// Plan and apply both ask this, so this table is what both do.
func TestClassifyRefChange(t *testing.T) {
	other := "/v1/regions/test/compute/cluster-types/cpu"
	for _, tc := range []struct {
		name, configured, bound string
		state                   k0rdent.ClusterState
		want                    refChange
		refusal                 string
	}{
		{"the bound version", verRef(1), verRef(1), "active", refUnchanged, ""},
		{"the bound version with an origin", "https://api.k0rdent.test" + verRef(1), verRef(1), "active", refUnchanged, ""},
		{"the head of the bound version", upgradeTypeRef, verRef(1), "active", refUnchanged, ""},
		{"the bound version on a failed cluster", verRef(1), verRef(1), "failed", refUnchanged, ""},
		{"another version", verRef(4), verRef(1), "active", refUpgrade, ""},
		{"another version while busy", verRef(4), verRef(1), "updating", refUpgrade, ""},
		{"another version with no state", verRef(4), verRef(1), "", refUpgrade, ""},
		{"an earlier version", verRef(1), verRef(4), "active", refUpgrade, ""},
		{"another version on a failed cluster", verRef(4), verRef(1), "failed", refRefused, "does not retry an upgrade from `failed`"},
		{"another ClusterType", other, verRef(1), "active", refReplace, ""},
		{"another ClusterType on a failed cluster", other, verRef(1), "failed", refReplace, ""},
		{"another host", "https://elsewhere.test" + verRef(4), "https://api.k0rdent.test" + verRef(1), "active", refReplace, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, refusal := classifyRefChange(tc.configured, tc.bound, tc.state)
			if got != tc.want || (tc.refusal == "") != (refusal == "") || !strings.Contains(refusal, tc.refusal) {
				t.Errorf("got %v %q, want %v %q", got, refusal, tc.want, tc.refusal)
			}
		})
	}
}

func TestScaleMayRetry(t *testing.T) {
	bare := &k0rdent.APIError{StatusCode: http.StatusConflict}
	for _, tc := range []struct {
		name    string
		refusal *k0rdent.APIError
		state   k0rdent.ClusterState
		read    bool // the read answered
		want    bool
	}{
		{"a bare 409 on an active cluster", bare, "active", true, true},
		{"a bare 409 the read did not answer", bare, "", false, false},
		{"a 409 with a reason", &k0rdent.APIError{StatusCode: 409, Reason: "QUOTA_EXCEEDED"}, "active", true, false},
		{"a 409 with detail reasons", &k0rdent.APIError{StatusCode: 409,
			Details: []map[string]any{{"reason": "POOL_BUSY"}}}, "active", true, false},
		{"a 422", &k0rdent.APIError{StatusCode: 422}, "active", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := scaleMayRetry(tc.refusal, func() (k0rdent.ClusterState, bool) { return tc.state, tc.read }); got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
	for _, state := range everyClusterState {
		if got := scaleMayRetry(bare, func() (k0rdent.ClusterState, bool) { return state, true }); got != (state == "active") {
			t.Errorf("a bare 409 on a %s cluster: retry = %v", state, got)
		}
	}
	read := false
	scaleMayRetry(&k0rdent.APIError{StatusCode: 409, Reason: "QUOTA_EXCEEDED"}, func() (k0rdent.ClusterState, bool) {
		read = true
		return "active", true
	})
	if read {
		t.Error("read the cluster for a refusal that cannot be the window")
	}
}

func TestUpgradeRefusal(t *testing.T) {
	for _, state := range everyClusterState {
		if got := upgradeRefusal(state) != ""; got != (state == "failed") {
			t.Errorf("%s: refused = %v", state, got)
		}
	}
}

func TestAfterRefusedDelete(t *testing.T) {
	for _, tc := range []struct {
		state             k0rdent.ClusterState
		first, afterAgain deleteStep
	}{
		{"active", deleteAsksAgain, deleteAsksAgain},
		{"failed", deleteAsksAgain, deleteAsksAgain},
		{"creating", deleteCancelsFirst, deleteGivesUp},
		{"updating", deleteCancelsFirst, deleteGivesUp},
		{"deleting", deleteWaits, deleteWaits},
		{"deleted", deleteWaits, deleteWaits},
		{"quiescing", deleteUnrecognised, deleteUnrecognised},
	} {
		for _, onBusy := range []string{"", destroyCancels} {
			if got := afterRefusedDelete(tc.state, false, onBusy); got != tc.first {
				t.Errorf("%s (on_destroy_busy %q): got %v, want %v", tc.state, onBusy, got, tc.first)
			}
			if got := afterRefusedDelete(tc.state, true, onBusy); got != tc.afterAgain {
				t.Errorf("%s after a cancel (on_destroy_busy %q): got %v, want %v", tc.state, onBusy, got, tc.afterAgain)
			}
		}
	}
}

// A destroy told to wait never cancels: an operation in flight is waited out
// however many times one is met, and every other state reads as it does when
// the destroy cancels.
func TestAfterRefusedDeleteWhenToldToWait(t *testing.T) {
	for _, tc := range []struct {
		state k0rdent.ClusterState
		want  deleteStep
	}{
		{"active", deleteAsksAgain},
		{"failed", deleteAsksAgain},
		{"creating", deleteAwaitsOperation},
		{"updating", deleteAwaitsOperation},
		{"deleting", deleteWaits},
		{"deleted", deleteWaits},
		{"quiescing", deleteUnrecognised},
	} {
		for _, cancelled := range []bool{false, true} {
			if got := afterRefusedDelete(tc.state, cancelled, destroyWaits); got != tc.want {
				t.Errorf("%s (cancelled %v): got %v, want %v", tc.state, cancelled, got, tc.want)
			}
		}
	}
}

func TestCancelTransitions(t *testing.T) {
	for _, tc := range []struct {
		state       k0rdent.ClusterState
		cancellable bool
		after       verdict
	}{
		{"active", false, settled},
		{"failed", false, settled},
		{"deleting", false, settled},
		{"deleted", false, settled},
		{"creating", true, keepWaiting},
		{"updating", true, keepWaiting},
		{"quiescing", false, unrecognised},
	} {
		if got := cancellable(tc.state); got != tc.cancellable {
			t.Errorf("%s: cancellable = %v", tc.state, got)
		}
		got, err := settledAfterCancel(tc.state)
		if got != tc.after || (err != nil) != (tc.after == unrecognised) {
			t.Errorf("%s: after a cancel = %v %v, want %v", tc.state, got, err, tc.after)
		}
	}
}

func TestKubeconfigOutlookFor(t *testing.T) {
	want := map[k0rdent.ClusterState]kubeconfigOutlook{
		"active":    kubeconfigIssuing,
		"creating":  kubeconfigAfterProvisioning,
		"updating":  kubeconfigAfterProvisioning,
		"failed":    kubeconfigNotWhileFailed,
		"deleting":  kubeconfigNever,
		"deleted":   kubeconfigNever,
		"quiescing": kubeconfigUnrecognised,
	}
	for _, state := range everyClusterState {
		if got := kubeconfigOutlookFor(state); got != want[state] {
			t.Errorf("%s: got %v, want %v", state, got, want[state])
		}
	}
}
