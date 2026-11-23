package acceptance

import (
	"fmt"
	"net/http"
	"runtime"
	"strings"
	"testing"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/provider"
)

// A `cluster_type` the platform's create refuses never reaches it: the plan
// refuses it first, names the way out, and sends nothing. A bare ClusterType
// name is the value the defect reports — it passed plan and failed at apply
// with 422 — and a reference naming another region failed the same way.
// Through the real CLI, for each resource that takes a `cluster_type`:
//
//  1. the configuration sets `cluster_type` to a value the create refuses
//  2. `terraform plan -detailed-exitcode` exits 1 with the refusal, which
//     names the form and the data source whose `self_link` has it
//  3. no request was sent: not the create, not the plan-time read
//  4. the same resource, its `cluster_type` taken from the cluster type data
//     source's `self_link`, plans with changes
//
// Not parallel: it counts every create on the collection, and Go runs a
// sequential test while no parallel one does, so the count is this test's.
func TestAccARefusedClusterTypeNeverReachesTheCreate(t *testing.T) { //nolint:paralleltest // it counts every create on the collection
	if runtime.GOOS == "windows" {
		skipTest(t, "the CLI sequence runs on unix")
	}
	otherRegion := "/v1/regions/us-east-1/compute/cluster-types/gpu-training"
	for i, tc := range []struct {
		name       string
		config     func(name, clusterType string) string
		ref        string
		refusal    string
		says       []string
		wayOut     func(name string) string
		collection string
	}{
		{
			"the reported bare name on a cluster", clusterAtRef,
			"terraform-small-gb300", "Not a ClusterType reference",
			[]string{"is not a ClusterType reference", "/v1/regions/{region}/compute/cluster-types/{id}", "self_link"},
			clusterAtDataSource, "/v1/regions/us-texas-1/projects/demo/compute/clusters",
		},
		{
			"a versions segment with nothing after it on a cluster", clusterAtRef,
			clusterTypeRef + "/versions/", "Not a ClusterType reference",
			[]string{"is not a ClusterType reference", "self_link"},
			clusterAtDataSource, "/v1/regions/us-texas-1/projects/demo/compute/clusters",
		},
		{
			"a cluster type from another region on a cluster", clusterAtRef,
			otherRegion, "The cluster type is in another region",
			[]string{"names the region \"us-east-1\"", "region is \"us-texas-1\"", "self_link"},
			clusterAtDataSource, "/v1/regions/us-texas-1/projects/demo/compute/clusters",
		},
		{
			"a bare ClusterType name on an instance group", instanceGroupAtRef,
			"gpu-training", "Not a ClusterType reference",
			[]string{"is not a ClusterType reference", "self_link"},
			instanceGroupAtDataSource, "/v1/regions/us-texas-1/projects/demo/compute/instance-groups",
		},
		{
			"a cluster type from another region on an instance group", instanceGroupAtRef,
			otherRegion, "The cluster type is in another region",
			[]string{"names the region \"us-east-1\"", "region is \"us-texas-1\"", "self_link"},
			instanceGroupAtDataSource, "/v1/regions/us-texas-1/projects/demo/compute/instance-groups",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			name := fmt.Sprintf("acc-ctref-%d", i)
			c := newCLIRun(t)
			c.write(tc.config(name, tc.ref))

			// From the moment the plan starts, the create and the plan-time
			// read are the only requests a plan of this shape could send.
			counters := []*requestCounter{
				{method: http.MethodPost, path: tc.collection},
				{method: http.MethodGet, path: tc.collection + "/" + name},
			}
			for _, counter := range counters {
				counter.start(t)()
			}
			sentNothing := func(step string) {
				for _, counter := range counters {
					if n, err := fakeServed(counter.method, counter.path); err != nil || n != counter.before {
						t.Errorf("%s sent %d %s %s: %v", step, n-counter.before, counter.method, counter.path, err)
					}
				}
			}

			out, code := c.runCode("plan", "-detailed-exitcode")
			if code != 1 {
				t.Errorf("the plan exited %d, want 1, a refused plan:\n%s", code, out)
			}
			mustSay(t, "the plan", out, append([]string{"Error: " + tc.refusal}, tc.says...)...)
			sentNothing("the refused plan")

			out, code = c.runCode("apply", "-auto-approve")
			if code != 1 {
				t.Errorf("the apply exited %d, want 1:\n%s", code, out)
			}
			mustSay(t, "the apply", out, "Error: "+tc.refusal)
			sentNothing("the apply")

			c.write(tc.wayOut(name))
			out, code = c.runCode("plan", "-detailed-exitcode")
			if code != 2 {
				t.Errorf("the plan with the data source's self_link exited %d, want 2, a plan with changes:\n%s", code, out)
			}
			mustSay(t, "the plan with the way out", out, "1 to add")
		})
	}
}

// clusterAtRef is clusterConfig with the `cluster_type` spelled out.
func clusterAtRef(name, ref string) string {
	return fmt.Sprintf(`
resource %q "test" {
  id           = %q
  cluster_type = %q
  display_name = "acceptance"
  node_pool = {
    "gpu-workers" = { node_count = 1 }
  }
}
`, clusterType, name, ref)
}

// instanceGroupAtRef is instanceGroupConfig with the `cluster_type` spelled
// out.
func instanceGroupAtRef(name, ref string) string {
	return fmt.Sprintf(`
resource %q "test" {
  id           = %q
  cluster_type = %q
  display_name = "acceptance"
  node_pool = {
    "gpu-workers" = { size = 1 }
  }
}
`, instanceGroupType, name, ref)
}

// clusterAtDataSource reads the ClusterType through the data source and uses
// its `self_link`, the way out a refusal names.
func clusterAtDataSource(name string) string {
	return fmt.Sprintf(`
data %[1]q "gpu" {
  id = "gpu-training"
}

resource %[2]q "test" {
  id           = %[3]q
  cluster_type = data.%[1]s.gpu.self_link
  display_name = "acceptance"
  node_pool = {
    "gpu-workers" = { node_count = 1 }
  }
}
`, provider.ResourceName("cluster_type"), clusterType, name)
}

// instanceGroupAtDataSource is clusterAtDataSource for an instance group.
func instanceGroupAtDataSource(name string) string {
	return fmt.Sprintf(`
data %[1]q "gpu" {
  id = "gpu-training"
}

resource %[2]q "test" {
  id           = %[3]q
  cluster_type = data.%[1]s.gpu.self_link
  display_name = "acceptance"
  node_pool = {
    "gpu-workers" = { size = 1 }
  }
}
`, provider.ResourceName("cluster_type"), instanceGroupType, name)
}

// TestAccAnAbsoluteClusterTypeCreates: a `cluster_type` given as an absolute
// URL creates a cluster and an instance group, and plans empty after. The
// platform's create parses only the path, and so does the fake's, so this
// passes only while the create sends the URL's path.
func TestAccAnAbsoluteClusterTypeCreates(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		skipTest(t, "the CLI sequence runs on unix")
	}
	const ref = "/v1/regions/us-texas-1/compute/cluster-types/gpu-training"
	for _, tc := range []struct {
		name   string
		config func(name, clusterType string) string
	}{
		{"cluster", clusterAtRef},
		{"instance group", instanceGroupAtRef},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			name := "acc-ctref-absolute-" + strings.ReplaceAll(tc.name, " ", "-")
			c := newCLIRun(t)
			c.write(tc.config(name, endpoint+ref))
			if out, ok := c.run("apply", "-auto-approve"); !ok {
				t.Fatalf("the create with an absolute cluster_type failed:\n%s", out)
			}
			t.Cleanup(func() {
				if out, ok := c.run("destroy", "-auto-approve"); !ok {
					t.Errorf("cleanup destroy failed:\n%s", out)
				}
			})
			if out, code := c.runCode("plan", "-detailed-exitcode"); code != 0 {
				t.Errorf("the plan after the create exited %d, want 0:\n%s", code, out)
			}
		})
	}
}
