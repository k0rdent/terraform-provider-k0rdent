package acceptance

import (
	"encoding/json"
	"fmt"
	"net/http"
	"runtime"
	"strings"
	"testing"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/provider"
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
func TestAccAFailedClusterPlansAnUpdateThatWaitsForIt(t *testing.T) {
	if runtime.GOOS == "windows" {
		skipTest(t, "the CLI sequence runs on unix")
	}
	for _, tc := range []struct {
		name, markers, reason string
		comesBack             bool
	}{
		{"acc-failed-watchdog", watchdogFails, "TIMEOUT", true},
		{"acc-failed-reserve", createFails, "RESERVATION_FAILED", false},
	} {
		t.Run(tc.reason, func(t *testing.T) {
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
				"can still bring it back to `active`", "`terraform apply` waits up to "+provider.ProvisionalFailureWindow.String(),
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
	t.Parallel()
	if runtime.GOOS == "windows" {
		skipTest(t, "the CLI sequence runs on unix")
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

// A cluster read `failed` when the apply's wait for it times out is reported
// as failed, with the platform's reason, not as still being created. The wait
// is shorter than the provisional window, so it stops while the cluster is
// `failed`. Three ways in, through the real CLI:
//
//   - the create: the platform fails it while the create waits
//   - failed before the wait: imported `failed`, so the apply waits for it
//   - failed during the wait: imported `creating`, and the platform fails it
//     while the apply waits for the create to finish
//
// Each apply fails and deletes nothing, and `terraform destroy` removes the
// cluster. After the create, `-replace` plans the replacement its error names.
func TestAccAClusterFailedWhenItsWaitTimesOutIsReportedFailed(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		skipTest(t, "the CLI sequence runs on unix")
	}
	const short = "  timeouts {\n    create = \"1s\"\n  }\n"
	for _, tc := range []struct {
		name, markers string
		want          []string
		// imported makes the cluster outside Terraform and imports it. reads
		// then advances the fake's create by that many reads before the
		// import, so the platform fails it during the apply's wait. Zero
		// imports it once it's `failed`. The fake fails a slow create on its
		// 30th read. 24, and the one creatingOutside checks with, leave 5: the
		// import and the refresh take 2, and the wait's third read fails it.
		imported bool
		reads    int
	}{
		{name: "acc-failed-create-wait", markers: createFails,
			want: []string{"The cluster did not become active", "terraform destroy"}},
		{name: "acc-failed-before-wait", markers: createFails, imported: true,
			want: []string{"The cluster is still failed", "terraform destroy"}},
		{name: "acc-failed-during-wait", markers: createsSlowly + " " + createFails, imported: true, reads: 24,
			want: []string{"The cluster's create failed"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := newCLIRun(t)
			addr := clusterType + ".test"
			t.Cleanup(func() { removeFromFake(t, tc.name) })
			switch {
			case !tc.imported:
			case tc.reads == 0:
				failedOutside(t, tc.name, tc.markers)
			default:
				creatingOutside(t, tc.name, tc.markers, tc.reads)
			}
			c.write(importedFailed(tc.name, tc.markers, short))
			if tc.imported {
				if out, ok := c.run("import", addr, tc.name); !ok {
					t.Fatalf("the import failed:\n%s", out)
				}
			}

			out, ok := c.run("apply", "-auto-approve")
			if ok {
				t.Fatalf("the apply passed on a cluster that's failed:\n%s", out)
			}
			mustSay(t, "the apply", out, append(tc.want, "At the last read", "reason RESERVATION_FAILED",
				"Nothing was deleted", "terraform apply -replace=<address>")...)
			if strings.Contains(out, "is still being created") {
				t.Errorf("the apply calls a failed cluster still being created:\n%s", out)
			}
			checkFakeStateNow(t, tc.name, "failed")

			if !tc.imported {
				out, ok = c.run("plan", "-replace="+addr)
				if !ok {
					t.Fatalf("the plan with -replace was refused:\n%s", out)
				}
				mustSay(t, "the plan with -replace", out, "1 to add, 0 to change, 1 to destroy")
			}
			out, ok = c.run("destroy", "-auto-approve")
			if !ok {
				t.Fatalf("the destroy of the failed cluster failed:\n%s", out)
			}
			mustSay(t, "the destroy", out, "1 destroyed")
		})
	}
}

// creatingOutside creates a cluster past the provider and reads it reads
// times, which moves a create marked createsSlowly that much closer to
// settling. It fails the test if the cluster isn't still `creating`.
func creatingOutside(t *testing.T, name, markers string, reads int) {
	t.Helper()
	if err := postFake("/v1/regions/us-texas-1/projects/demo/compute/clusters", fmt.Sprintf(
		`{"id":%q,"clusterType":%q,"displayName":%q,"nodePools":[{"id":"gpu-workers","nodeCount":2}]}`,
		name, clusterTypeRef, markers)); err != nil {
		t.Fatal(err)
	}
	for range reads {
		if _, _, err := readFake(clusterAPIPath(name)); err != nil {
			t.Fatal(err)
		}
	}
	checkFakeStateNow(t, name, "creating")
}
