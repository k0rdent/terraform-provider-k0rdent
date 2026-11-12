package acceptance

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"runtime"
	"testing"
)

// The version changes a cluster can't take: an earlier version, and any
// other version while the platform reports the cluster `failed`.
const (
	versionOne   = clusterTypeRef + "/versions/11111111-1111-1111-1111-111111111111"
	versionThree = clusterTypeRef + "/versions/33333333-3333-3333-3333-333333333333"
	// versionRefused is the plan's warning and the apply's error.
	versionRefused = "Cannot move the cluster to a different version of its ClusterType"
)

// versionedCluster is a gpu-training cluster at ref, with what else an apply
// can change in the same run.
func versionedCluster(name, ref, display string, workers, ceiling int) string {
	return fmt.Sprintf(`
resource %q "test" {
  id                  = %q
  cluster_type        = %q
  display_name        = %q
  storage_ceiling_gib = %d
  node_pool = {
    "gpu-workers" = { node_count = %d }
  }
}
`, clusterType, name, ref, display, ceiling, workers)
}

// runCode runs terraform with args and returns what it printed and its exit
// code, for `plan -detailed-exitcode`, whose 2 means a plan with changes.
func (c *cliRun) runCode(args ...string) (string, int) {
	c.t.Helper()
	out, err := c.command(append(args, "-no-color")...).CombinedOutput()
	var exit *exec.ExitError
	switch {
	case errors.As(err, &exit):
		return string(out), exit.ExitCode()
	case err != nil:
		c.t.Fatalf("terraform %v: %v", args, err)
	}
	return string(out), 0
}

// clusterWrites counts every write the provider can send a cluster, from
// the moment they're started.
func clusterWrites(t *testing.T, path string) func(step string) {
	t.Helper()
	writes := []*requestCounter{
		{method: http.MethodPatch, path: path},
		{method: http.MethodPost, path: path + "/scale"},
		{method: http.MethodPost, path: path + "/upgrade"},
		{method: http.MethodPost, path: path + "/cancel"},
		{method: http.MethodDelete, path: path},
	}
	for _, w := range writes {
		w.start(t)()
	}
	return func(step string) {
		t.Helper()
		for _, w := range writes {
			if n, err := fakeServed(w.method, w.path); err != nil || n != w.before {
				t.Errorf("%s sent %d %s %s: %v", step, n-w.before, w.method, w.path, err)
			}
		}
	}
}

// A version change the cluster can't take plans as an update with a
// warning, and the apply fails before it sends any request, the other
// changes in the same apply included. Through the real CLI, for each change
// that would otherwise go out first:
//
//  1. `terraform apply` makes the cluster at version 2
//  2. the configuration pins version 1, which is earlier, with the change
//  3. `terraform plan -detailed-exitcode` exits 2 with the warning
//  4. `terraform apply` exits 1 with the refusal and sends nothing
//  5. the platform's cluster is as it was, and `terraform destroy` removes it
func TestAccARefusedVersionChangeWarnsAtPlanAndFailsAtApply(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		skipTest(t, "the CLI sequence runs on unix")
	}
	for i, tc := range []struct {
		name             string
		display          string
		workers, ceiling int
	}{
		{"alone", "acceptance", 1, 100},
		{"with a node count", "acceptance", 2, 100},
		{"with a storage ceiling", "acceptance", 1, 200},
		{"with a display name", "renamed", 1, 100},
		{"with all three", "renamed", 3, 300},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			name := fmt.Sprintf("acc-refused-version-%d", i)
			path := clusterAPIPath(name)
			c := newCLIRun(t)
			c.write(versionedCluster(name, clusterTypeV2, "acceptance", 1, 100))
			if out, ok := c.run("apply", "-auto-approve"); !ok {
				t.Fatalf("the create failed:\n%s", out)
			}
			t.Cleanup(func() {
				if out, ok := c.run("destroy", "-auto-approve", "-refresh=false"); !ok {
					t.Errorf("cleanup destroy failed:\n%s", out)
				}
			})

			c.write(versionedCluster(name, versionOne, tc.display, tc.workers, tc.ceiling))
			sentNothing := clusterWrites(t, path)
			out, code := c.runCode("plan", "-detailed-exitcode")
			if code != 2 {
				t.Errorf("the plan exited %d, want 2, a plan with changes:\n%s", code, out)
			}
			mustSay(t, "the plan", out, "Warning: "+versionRefused, "forward-only",
				"fails before it sends any request, other changes included", "`terraform destroy` isn't affected",
				clusterTypeV2, "0 to add, 1 to change, 0 to destroy")
			sentNothing("the plan")

			out, code = c.runCode("apply", "-auto-approve")
			if code != 1 {
				t.Errorf("the apply exited %d, want 1:\n%s", code, out)
			}
			mustSay(t, "the apply", out, "Error: "+versionRefused, "forward-only", "This apply sent nothing")
			sentNothing("the apply")

			status, body, err := readFakeStore(path)
			if err != nil || status != http.StatusOK {
				t.Fatalf("reading the cluster: %d %v", status, err)
			}
			var got struct {
				ClusterType string `json:"clusterType"`
				DisplayName string `json:"displayName"`
				NodePools   []struct {
					ID        string `json:"id"`
					NodeCount int    `json:"nodeCount"`
				} `json:"nodePools"`
				Storage struct {
					CeilingGiB int `json:"ceilingGiB"`
				} `json:"storage"`
			}
			if err := json.Unmarshal(body, &got); err != nil {
				t.Fatal(err)
			}
			workers := -1
			for _, p := range got.NodePools {
				if p.ID == "gpu-workers" {
					workers = p.NodeCount
				}
			}
			if got.ClusterType != clusterTypeV2 || got.DisplayName != "acceptance" || workers != 1 || got.Storage.CeilingGiB != 100 {
				t.Errorf("the cluster changed: %s", body)
			}

			out, ok := c.run("destroy", "-auto-approve")
			if !ok {
				t.Fatalf("the destroy failed:\n%s", out)
			}
			mustSay(t, "the destroy", out, "Destroy complete! Resources: 1 destroyed.")
			if err := checkGoneAt(path)(nil); err != nil {
				t.Error(err)
			}
		})
	}
}

// `terraform destroy` runs a normal plan first, so a version change refused
// at plan would stop it (mistakes log, Class 1). It's refused at apply, so
// with `cluster_type` pinned to a version the cluster can't take, the plan
// warns and `terraform destroy` removes the cluster, in each state the
// platform can report it in. Through the real CLI, for each state:
//
//  1. the cluster is made, by `terraform apply` or outside Terraform and
//     imported, and taken to the state
//  2. the configuration pins a version it can't take
//  3. `terraform plan -detailed-exitcode` exits 2 with the warning
//  4. `terraform destroy` removes the cluster, and the platform no longer has it
func TestAccARefusedVersionChangeDoesNotStopADestroy(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		skipTest(t, "the CLI sequence runs on unix")
	}
	for _, tc := range []struct {
		state   string
		display string
		// pin is the version the configuration names, and why is what the
		// refusal says about it.
		pin, why string
	}{
		{"creating", neverSettles, versionOne, "forward-only"},
		{"updating", "acceptance " + holdsUpgrade, versionOne, "forward-only"},
		{"active", "acceptance", versionOne, "forward-only"},
		{"failed", createFails, versionThree, "does not retry an upgrade from `failed`"},
	} {
		t.Run(tc.state, func(t *testing.T) {
			t.Parallel()
			name := "acc-refused-version-destroy-" + tc.state
			path := clusterAPIPath(name)
			addr := clusterType + ".test"
			c := newCLIRun(t)
			destroyed := false
			t.Cleanup(func() {
				if destroyed {
					return
				}
				if out, ok := c.run("destroy", "-auto-approve", "-refresh=false"); !ok {
					t.Errorf("cleanup destroy failed:\n%s", out)
				}
			})

			if tc.state == "active" {
				c.write(versionedCluster(name, clusterTypeV2, tc.display, 2, 100))
				if out, ok := c.run("apply", "-auto-approve"); !ok {
					t.Fatalf("the create failed:\n%s", out)
				}
			} else {
				want := tc.state
				if tc.state == "updating" {
					want = "active"
				}
				clusterOutside(t, name, tc.display, want)
				c.write(versionedCluster(name, clusterTypeV2, tc.display, 2, 100))
				if out, ok := c.run("import", addr, scopedImport(name)); !ok {
					t.Fatalf("the import failed:\n%s", out)
				}
			}
			if tc.state == "updating" {
				if err := postFake(path+"/upgrade", `{}`); err != nil {
					t.Fatal(err)
				}
				awaitFakeState(t, path, "updating")
			}

			c.write(versionedCluster(name, tc.pin, tc.display, 2, 100))
			out, code := c.runCode("plan", "-detailed-exitcode")
			if code != 2 {
				t.Errorf("the plan of the %s cluster exited %d, want 2:\n%s", tc.state, code, out)
			}
			mustSay(t, "the plan", out, "Warning: "+versionRefused, tc.why, "`terraform destroy` isn't affected")

			out, ok := c.run("destroy", "-auto-approve")
			if !ok {
				t.Fatalf("the destroy of the %s cluster failed:\n%s", tc.state, out)
			}
			destroyed = true
			mustSay(t, "the destroy", out, "Destroy complete! Resources: 1 destroyed.")
			if err := checkGoneAt(path)(nil); err != nil {
				t.Error(err)
			}
		})
	}
}
