package acceptance

import (
	"runtime"
	"strings"
	"testing"
)

// The fake lets a cancel carrying this lose the race: it's answered 202
// `cancelling`, and then the operation lands anyway, `active` and
// `succeeded`, as API v1.3.0-rc.2 did in MOCK_MODE.
const cancelLoses = "fake-cancel-loses"

// cancelOutcomes are the two ways an accepted cancel ends on the fake, and
// what the destroy and the cancel action each have to say about it, and
// must not, and what the plan after the action says.
var cancelOutcomes = []struct {
	name, markers, settles, operation string
	destroySays, destroyNever         []string
	actionSays, actionNever           []string
	planSays                          []string
}{
	{
		name: "cancelled", markers: neverSettles, settles: "failed", operation: "cancelled",
		destroySays:  []string{"Cancelled the cluster's in-flight operation to destroy it", "reads `cancelled`"},
		destroyNever: []string{"finished before the cancel"},
		actionSays:   []string{"Cancelled operation", "is failed, with nothing in flight"},
		actionNever:  []string{"finished before the cancel"},
		planSays:     []string{"is tainted, so must be replaced", "failed: applying this plan replaces it"},
	},
	{
		name: "lost the race", markers: neverSettles + " " + cancelLoses, settles: "active", operation: "succeeded",
		destroySays: []string{"operation finished before the cancel took effect", "reads `succeeded`",
			"Nothing was cancelled"},
		destroyNever: []string{"did not finish", "Cancelled the cluster's in-flight operation"},
		actionSays: []string{"finished before the cancel took effect", "reads `succeeded`",
			"Nothing was cancelled"},
		actionNever: []string{"Cancelled operation"},
		planSays:    []string{"is tainted, so must be replaced", "exists: applying this plan replaces it"},
	},
}

// A destroy that cancels a create says how the cancel ended, from the
// operation the settled cluster reports: cancelled, or finished anyway.
func TestAccDestroySaysHowTheCancelEnded(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the CLI sequence runs on unix")
	}
	for _, tc := range cancelOutcomes {
		t.Run(tc.name, func(t *testing.T) {
			name := "acc-destroy-" + strings.ReplaceAll(tc.name, " ", "-")
			c := newCLIRun(t)
			c.write(taintedObject(clusterType, name, tc.markers))
			if out, ok := c.run("apply", "-auto-approve"); ok {
				t.Fatalf("the apply passed while the create never settles:\n%s", out)
			}
			out, ok := c.run("destroy", "-auto-approve")
			if !ok {
				t.Fatalf("the destroy failed:\n%s", out)
			}
			mustSay(t, "the destroy", out, tc.destroySays...)
			mustNotSay(t, "the destroy", out, tc.destroyNever...)
		})
	}
}

// The cancel action, invoked on a create in flight, says how the cancel
// ended: the cluster settles `failed` and `cancelled`, or the create lands
// anyway and the action says nothing was cancelled.
func TestAccCancelActionSaysHowTheCancelEnded(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the CLI sequence runs on unix")
	}
	for _, tc := range cancelOutcomes {
		t.Run(tc.name, func(t *testing.T) {
			name := "acc-action-" + strings.ReplaceAll(tc.name, " ", "-")
			c := newCLIRun(t)
			c.write(taintedObject(clusterType, name, tc.markers))
			if out, ok := c.run("apply", "-auto-approve"); ok {
				t.Fatalf("the apply passed while the create never settles:\n%s", out)
			}
			t.Cleanup(func() {
				if out, ok := c.run("destroy", "-auto-approve"); !ok {
					t.Errorf("cleanup destroy failed:\n%s", out)
				}
			})
			out := invokeByCLI(t, name)
			mustSay(t, "the action", out, tc.actionSays...)
			mustNotSay(t, "the action", out, tc.actionNever...)
			for _, check := range []func() error{
				func() error { return checkFakeState(name, tc.settles)(nil) },
				func() error { return checkFakeOperation(name, tc.operation)(nil) },
			} {
				if err := check(); err != nil {
					t.Error(err)
				}
			}
			out, ok := c.run("plan")
			if !ok {
				t.Fatalf("the plan after the action was refused:\n%s", out)
			}
			mustSay(t, "the plan after the action", out, tc.planSays...)
		})
	}
}

// mustNotSay fails if out, folded to single spaces, says any of nevers.
func mustNotSay(t *testing.T, step, out string, nevers ...string) {
	t.Helper()
	flat := strings.Join(strings.Fields(strings.ReplaceAll(out, "│", " ")), " ")
	for _, never := range nevers {
		if strings.Contains(flat, never) {
			t.Errorf("%s says %q:\n%s", step, never, out)
		}
	}
}
