package acceptance

import (
	"fmt"
	"runtime"
	"testing"
)

// What a user sees after the cancel action stops a scale or an upgrade that
// an apply had stopped waiting for, through the real CLI. The cluster is
// `failed`, and the platform admits neither a scale nor an upgrade from there.

func cancelledCluster(name, version, markers string, workers int) string {
	return fmt.Sprintf(`
resource %q "test" {
  id           = %q
  cluster_type = "%s/versions/%s"
  display_name = %q
  node_pool = {
    "gpu-workers" = { node_count = %d }
  }
  timeouts {
    update = "1s"
  }
}
`, clusterType, name, clusterTypeRef, version, markers, workers)
}

const clusterTypeV2ID = "22222222-2222-2222-2222-222222222222"

// cancelAfterTimeout applies config, which has to time out, then cancels what
// it left running with the action.
func cancelAfterTimeout(t *testing.T, c *cliRun, name, config string) {
	t.Helper()
	c.write(config)
	if out, ok := c.run("apply", "-auto-approve"); ok {
		t.Fatalf("the apply passed while the operation never settles:\n%s", out)
	}
	mustSay(t, "the action", invokeByCLI(t, name), "Cancelled operation")
	checkFakeStateNow(t, name, "failed")
}

// A cancelled scale keeps the old node count. The next plan asks for the
// scale again, and its apply fails, since the platform refuses a scale of a
// `failed` cluster. Setting the count back plans clean.
func TestAccAfterACancelledScale(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the CLI sequence runs on unix")
	}
	const name = "acc-cancelled-scale"
	c := newCLIRun(t)
	c.write(cancelledCluster(name, clusterTypeV2ID, "acceptance", 1))
	if out, ok := c.run("apply", "-auto-approve"); !ok {
		t.Fatalf("create:\n%s", out)
	}
	t.Cleanup(func() {
		if out, ok := c.run("destroy", "-auto-approve"); !ok {
			t.Errorf("cleanup destroy failed:\n%s", out)
		}
	})
	stuck := "acceptance " + neverSettles
	cancelAfterTimeout(t, c, name, cancelledCluster(name, clusterTypeV2ID, stuck, 2))

	out, ok := c.run("plan")
	if !ok {
		t.Fatalf("the plan after the cancel was refused:\n%s", out)
	}
	mustSay(t, "the plan", out, "node_count = 1 -> 2", "0 to add, 1 to change, 0 to destroy")
	out, ok = c.run("apply", "-auto-approve")
	if ok {
		t.Fatalf("the scale of a failed cluster passed:\n%s", out)
	}
	mustSay(t, "the apply", out, "409", "the cluster is failed")

	c.write(cancelledCluster(name, clusterTypeV2ID, stuck, 1))
	out, _ = c.run("plan")
	mustSay(t, "the plan with the count set back", out, "No changes.")
}

// A cancelled upgrade leaves the cluster bound to the old version. A plan
// that names the new one is refused, and so is `terraform destroy`, which
// plans first, until `cluster_type` names the old version again.
func TestAccAfterACancelledUpgrade(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the CLI sequence runs on unix")
	}
	const name = "acc-cancelled-upgrade"
	held := "acceptance " + holdsUpgrade
	c := newCLIRun(t)
	c.write(cancelledCluster(name, clusterTypeV2ID, held, 1))
	if out, ok := c.run("apply", "-auto-approve"); !ok {
		t.Fatalf("create:\n%s", out)
	}
	t.Cleanup(func() {
		c.write(cancelledCluster(name, clusterTypeV2ID, held, 1))
		if out, ok := c.run("destroy", "-auto-approve"); !ok {
			t.Errorf("cleanup destroy failed:\n%s", out)
		}
	})
	cancelAfterTimeout(t, c, name, cancelledCluster(name, clusterTypeV3, held, 1))

	refusal := []string{"does not retry an upgrade from `failed`",
		"set `cluster_type` to \"" + clusterTypeRef + "/versions/" + clusterTypeV2ID + "\""}
	out, ok := c.run("plan")
	if ok {
		t.Fatalf("the plan naming the new version passed:\n%s", out)
	}
	mustSay(t, "the plan", out, refusal...)
	out, ok = c.run("destroy", "-auto-approve")
	if ok {
		t.Fatalf("the destroy naming the new version passed:\n%s", out)
	}
	mustSay(t, "the destroy", out, refusal...)
	checkFakeStateNow(t, name, "failed")

	c.write(cancelledCluster(name, clusterTypeV2ID, held, 1))
	out, _ = c.run("plan")
	mustSay(t, "the plan with the old version", out, "No changes.")
}
