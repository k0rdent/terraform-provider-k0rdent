package acceptance

import (
	"encoding/json"
	"fmt"
	"net/http"
	"runtime"
	"strings"
	"testing"
	"time"
)

// The fake holds an upgrade posted to a cluster carrying holdsUpgrade until
// it's cancelled, and never finishes an instance group's create carrying
// createsForever.
const (
	holdsUpgrade   = "fake-holds-upgrade"
	createsForever = "fake-creates-forever"
)

// A create whose wait stops leaves the object tainted, and `terraform
// destroy` has to remove it from whatever state the platform has taken it to
// since. A destroy runs a normal plan first to refresh state, and that plan
// replaces a tainted object, which reads as a create. So each state below
// runs the real CLI:
//
//  1. `terraform apply` with a 1s create timeout fails, tainted, with the
//     uid its answer named
//  2. the fake takes the object to the state under test
//  3. `terraform plan` passes, plans the replacement, and warns what applying
//     it does
//  4. `terraform destroy` removes it with no untaint, and state is empty
//
// A cluster is told apart from one made outside Terraform by its create
// token. An instance group carries none, so it passes on its state alone.
func TestAccADestroyRunsPastATaintedCreateInEveryState(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the CLI sequence runs on unix")
	}
	const (
		clusters = "/v1/regions/us-texas-1/projects/demo/compute/clusters/"
		groups   = "/v1/regions/us-texas-1/projects/demo/compute/instance-groups/"
	)
	for _, tc := range []struct {
		typeName, name, markers, state string
		// warns is what the plan's warning says for this state.
		warns []string
	}{
		{clusterType, "acc-tainted-creating", neverSettles, "creating",
			[]string{"is still being created: applying this plan cancels it if this state holds it", "terraform untaint <address>"}},
		{clusterType, "acc-tainted-updating", createsSlowly + " " + holdsUpgrade, "updating",
			[]string{"is still being updated: applying this plan cancels it if this state holds it", "terraform untaint <address>"}},
		{clusterType, "acc-tainted-active", createsSlowly, "active",
			[]string{"exists: applying this plan replaces it if this state holds it", "terraform untaint <address>"}},
		{clusterType, "acc-tainted-failed", createsSlowly + " " + createFails, "failed",
			[]string{"failed: applying this plan replaces it if this state holds it"}},
		{instanceGroupType, "acc-tainted-group-creating", createsForever, "creating",
			[]string{"is still being created: applying this plan cancels it if this state holds it", "terraform untaint <address>"}},
		{instanceGroupType, "acc-tainted-group-active", createsSlowly, "active",
			[]string{"exists: applying this plan replaces it if this state holds it", "terraform untaint <address>"}},
		{instanceGroupType, "acc-tainted-group-failed", createsSlowly + " " + createFails, "failed",
			[]string{"failed: applying this plan replaces it if this state holds it"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newCLIRun(t)
			addr := tc.typeName + ".test"
			path := clusters + tc.name
			if tc.typeName == instanceGroupType {
				path = groups + tc.name
			}
			c.write(taintedObject(tc.typeName, tc.name, tc.markers))
			out, ok := c.run("apply", "-auto-approve")
			if ok {
				t.Fatalf("the apply passed while the create is still running:\n%s", out)
			}
			mustSay(t, "the apply", out, "is still being created")
			// The uid the create's answer named, so the destroy below runs
			// with the row bound to the object it made.
			if c.heldUID(addr) == "" {
				t.Error("the tainted row holds no uid")
			}
			destroyed := false
			t.Cleanup(func() {
				if destroyed {
					return
				}
				// The way out the bug left: untaint, then destroy.
				c.run("untaint", addr)
				if gone, done := c.run("destroy", "-auto-approve"); !done {
					t.Errorf("cleanup destroy failed:\n%s", gone)
				}
			})

			switch tc.state {
			case "updating":
				awaitFakeState(t, path, "active")
				if err := postFake(path+"/upgrade", `{}`); err != nil {
					t.Fatal(err)
				}
				awaitFakeState(t, path, "updating")
			case "creating":
				awaitFakeState(t, path, "creating")
			default:
				awaitFakeState(t, path, tc.state)
			}

			out, ok = c.run("plan")
			if !ok {
				t.Errorf("the plan of the tainted %s %s was refused:\n%s", tc.state, tc.typeName, out)
			}
			mustSay(t, "the plan", out, append([]string{"is tainted, so must be replaced",
				"1 to add, 0 to change, 1 to destroy", "`terraform destroy` shows this warning too"}, tc.warns...)...)

			out, ok = c.run("destroy", "-auto-approve")
			if !ok {
				t.Fatalf("the destroy of the tainted %s %s failed:\n%s", tc.state, tc.typeName, out)
			}
			destroyed = true
			mustSay(t, "the destroy", out, "1 destroyed")
			if listed, ok := c.run("state", "list"); !ok || strings.TrimSpace(listed) != "" {
				t.Errorf("state isn't empty after the destroy:\n%s", listed)
			}
			if status, _, err := readFake(path); err != nil || status != http.StatusNotFound {
				t.Errorf("the fake still holds %s after the destroy: %d %v", tc.name, status, err)
			}
		})
	}
}

// A cluster in someone else's hands, made with no create token, is still
// refused at plan in every state, and so is the destroy that would replace
// nothing: the configuration doesn't hold it, so nothing is destroyed.
func TestAccAPlanToCreateOverAClusterMadeElsewhereIsRefused(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the CLI sequence runs on unix")
	}
	const name = "acc-made-elsewhere"
	c := newCLIRun(t)
	path := "/v1/regions/us-texas-1/projects/demo/compute/clusters"
	if err := postFake(path, fmt.Sprintf(`{"id":%q,"clusterType":%q,"displayName":%q}`,
		name, clusterTypeV2, neverSettles)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c.write(taintedObject(clusterType, name, neverSettles))
		if out, ok := c.run("import", clusterType+".test", name); !ok {
			t.Errorf("cleanup import failed:\n%s", out)
		}
		if out, ok := c.run("destroy", "-auto-approve"); !ok {
			t.Errorf("cleanup destroy failed:\n%s", out)
		}
	})
	c.write(taintedObject(clusterType, name, neverSettles))
	out, ok := c.run("plan")
	if ok {
		t.Fatalf("the plan to create over a cluster made elsewhere passed:\n%s", out)
	}
	mustSay(t, "the refused plan", out, "is still being created", "carries no create token",
		"terraform untaint <address>", "terraform import", "Nothing was sent: the plan was refused.")
	// No token doesn't mean no apply made it: an imported cluster has none.
	if strings.Contains(out, "wasn't made by a") {
		t.Errorf("the refusal claims no Terraform apply made it:\n%s", out)
	}
}

// taintedObject is a cluster or an instance group whose create stops waiting
// after a second. A held upgrade needs a version to upgrade to, so a cluster
// is pinned to one that has a next.
func taintedObject(typeName, name, markers string) string {
	ref, pool := clusterTypeV2, `"gpu-workers" = { node_count = 1 }`
	if typeName == instanceGroupType {
		ref, pool = clusterTypeRef, `"gpu-workers" = { size = 1 }`
	}
	return fmt.Sprintf(`
resource %q "test" {
  id           = %q
  cluster_type = %q
  display_name = %q
  node_pool = {
    %s
  }
  timeouts {
    create = "1s"
  }
}
`, typeName, name, ref, markers, pool)
}

// awaitFakeState reads the object from the fake, past the provider, until it
// reports want. Each read is one step of the fake's settle.
func awaitFakeState(t *testing.T, path, want string) {
	t.Helper()
	last := ""
	for range 200 {
		status, body, err := readFake(path)
		if err != nil {
			t.Fatal(err)
		}
		if status != http.StatusOK {
			t.Fatalf("reading %s answered %d", path, status)
		}
		var got struct {
			State string `json:"state"`
		}
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatal(err)
		}
		if last = got.State; last == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s never read %q, last %q", path, want, last)
}
