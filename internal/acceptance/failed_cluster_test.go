package acceptance

import (
	"encoding/json"
	"fmt"
	"net/http"
	"runtime"
	"testing"
)

// A cluster the platform reports `failed`, through the real CLI. Each one is
// made outside Terraform and imported, which is how state comes to hold a
// cluster that's `failed` without the create that failed it tainting it. The
// plan is an update with a warning that carries the platform's reason, and
// the apply waits through the provisional window: a cluster the watchdog
// failed comes back and the apply passes, and one whose hosts couldn't be
// reserved stays `failed` and the apply fails, saying how to replace it. A
// cluster a cancel left `failed` is TestAccCancelActionStopsAnUpgradeAndKeepsTheCluster's,
// which plans nothing.
//
// The CLI runs its own build of the provider, which holds a cluster's
// `failed` for the full provisional window, so the failing apply takes that
// long.
func TestAccAFailedClusterPlansAnUpdateThatWaitsForIt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the CLI sequence runs on unix")
	}
	for _, tc := range []struct {
		name, markers, reason string
		comesBack             bool
	}{
		{"acc-failed-watchdog", watchdogFails, "TIMEOUT", true},
		{"acc-failed-reserve", createFails, "RESERVATION_FAILED", false},
	} {
		t.Run(tc.reason, func(t *testing.T) {
			if testing.Short() && !tc.comesBack {
				t.Skip("waits the full provisional window")
			}
			c := newCLIRun(t)
			addr := clusterType + ".test"
			failedOutside(t, tc.name, tc.markers)
			c.write(importedFailed(tc.name, tc.markers, ""))
			t.Cleanup(func() {
				if out, ok := c.run("destroy", "-auto-approve"); !ok {
					t.Errorf("the destroy of the failed cluster failed:\n%s", out)
				} else {
					mustSay(t, "the destroy", out, "1 destroyed")
				}
			})

			out, ok := c.run("import", addr, tc.name)
			if !ok {
				t.Fatalf("the import of the failed cluster failed:\n%s", out)
			}
			out, ok = c.run("plan")
			if !ok {
				t.Fatalf("the plan on the failed cluster was refused:\n%s", out)
			}
			mustSay(t, "the plan", out, fmt.Sprintf("Cluster %q is failed", tc.name), "reason "+tc.reason,
				"can still bring it back to `active`", "`terraform apply` waits up to 2m",
				"terraform apply -replace=<address>", "1 to change")

			out, ok = c.run("apply", "-auto-approve")
			if tc.comesBack {
				if !ok {
					t.Fatalf("the apply failed on a cluster that came back:\n%s", out)
				}
				mustSay(t, "the apply", out, "1 changed")
				shown, _ := c.run("state", "show", addr)
				mustSay(t, "state", shown, `state = "active"`)
				out, ok = c.run("plan", "-detailed-exitcode")
				if !ok {
					t.Errorf("the plan after the cluster came back isn't empty:\n%s", out)
				}
				return
			}
			if ok {
				t.Fatalf("the apply passed on a cluster that's still failed:\n%s", out)
			}
			mustSay(t, "the apply", out, "The cluster is still failed", "didn't return to `active`",
				"Nothing was deleted", "terraform apply -replace=<address>", tc.reason)
			shown, _ := c.run("state", "show", addr)
			mustSay(t, "state", shown, `state = "failed"`)
		})
	}
}

// importedFailed is the configuration that matches a cluster failedOutside
// made, every node pool the import reads included, so the only change a plan
// can have is the wait.
func importedFailed(name, markers, extra string) string {
	return fmt.Sprintf(`
resource %q "test" {
  id           = %q
  cluster_type = %q
  display_name = %q
  node_pool = {
    "control-plane" = { node_count = 1 }
    "gpu-workers"   = { node_count = 2 }
  }
%s}
`, clusterType, name, clusterTypeRef, markers, extra)
}

// A cluster kept with `on_failed = "keep"`, such as one whose upgrade
// failed, warns on every plan and plans nothing, so its applies pass. Setting
// `on_failed` back to `wait` plans the wait again.
func TestAccAFailedClusterKeptPlansNothing(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the CLI sequence runs on unix")
	}
	const name = "acc-failed-kept"
	c := newCLIRun(t)
	addr := clusterType + ".test"
	failedOutside(t, name, createFails)
	c.write(importedFailed(name, createFails, "  on_failed = \"keep\"\n"))
	t.Cleanup(func() {
		if out, ok := c.run("destroy", "-auto-approve"); !ok {
			t.Errorf("the destroy of the kept cluster failed:\n%s", out)
		}
	})
	if out, ok := c.run("import", addr, name); !ok {
		t.Fatalf("the import of the failed cluster failed:\n%s", out)
	}
	// The import records `on_failed` as null, so the first apply records
	// `keep`, sending nothing and waiting for nothing.
	out, ok := c.run("apply", "-auto-approve")
	if !ok {
		t.Fatalf("the apply that records on_failed failed:\n%s", out)
	}
	mustSay(t, "the apply", out, "`on_failed` is `keep`, so the plan doesn't change it", "1 changed")
	out, ok = c.run("plan", "-detailed-exitcode")
	if !ok {
		t.Fatalf("the plan on the kept cluster isn't empty:\n%s", out)
	}
	mustSay(t, "the plan", out, fmt.Sprintf("Cluster %q is failed", name), "reason RESERVATION_FAILED",
		"`on_failed` is `keep`", "No changes")

	c.write(importedFailed(name, createFails, "  on_failed = \"wait\"\n"))
	out, ok = c.run("plan")
	if !ok {
		t.Fatalf("the plan back to wait was refused:\n%s", out)
	}
	mustSay(t, "the plan back to wait", out, "can still bring it back to `active`", "1 to change")
}

// failedOutside creates a cluster past the provider and reads it until the
// fake reports it `failed`.
func failedOutside(t *testing.T, name, markers string) {
	t.Helper()
	if err := postFake("/v1/regions/us-texas-1/projects/demo/compute/clusters", fmt.Sprintf(
		`{"id":%q,"clusterType":%q,"displayName":%q,"nodePools":[{"id":"gpu-workers","nodeCount":2}]}`,
		name, clusterTypeRef, markers)); err != nil {
		t.Fatal(err)
	}
	for range 20 {
		status, body, err := readFake(clusterAPIPath(name))
		if err != nil {
			t.Fatal(err)
		}
		var got struct {
			State string `json:"state"`
		}
		if status != http.StatusOK || json.Unmarshal(body, &got) != nil {
			t.Fatalf("reading %s answered %d: %s", name, status, body)
		}
		if got.State == "failed" {
			return
		}
	}
	t.Fatalf("%s never read failed", name)
}

// watchdogFails is the fake's marker for a cluster whose create the watchdog
// fails, and which comes back `active` on its own a few reads later.
const watchdogFails = "fake-watchdog-fails"
