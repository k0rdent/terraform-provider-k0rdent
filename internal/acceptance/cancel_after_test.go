package acceptance

import (
	"encoding/json"
	"fmt"
	"net/http"
	"runtime"
	"testing"
	"time"
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
	t.Parallel()
	// The cluster is cancelled with the action.
	skipUnderOpenTofu(t, openTofuHasNoActions)
	if runtime.GOOS == "windows" {
		skipTest(t, "the CLI sequence runs on unix")
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
// that names the new one warns, its apply is refused with nothing sent, and
// `terraform destroy`, which plans first, still removes the cluster. Naming
// the old version again plans no change.
func TestAccAfterACancelledUpgrade(t *testing.T) {
	t.Parallel()
	// The cluster is cancelled with the action.
	skipUnderOpenTofu(t, openTofuHasNoActions)
	if runtime.GOOS == "windows" {
		skipTest(t, "the CLI sequence runs on unix")
	}
	const name = "acc-cancelled-upgrade"
	held := "acceptance " + holdsUpgrade
	c := newCLIRun(t)
	c.write(cancelledCluster(name, clusterTypeV2ID, held, 1))
	if out, ok := c.run("apply", "-auto-approve"); !ok {
		t.Fatalf("create:\n%s", out)
	}
	t.Cleanup(func() {
		if out, ok := c.run("destroy", "-auto-approve", "-refresh=false"); !ok {
			t.Errorf("cleanup destroy failed:\n%s", out)
		}
	})
	newVersion := cancelledCluster(name, clusterTypeV3, held, 1)
	cancelAfterTimeout(t, c, name, newVersion)

	refusal := []string{versionRefused, "does not retry an upgrade from `failed`",
		"set `cluster_type` to \"" + clusterTypeRef + "/versions/" + clusterTypeV2ID + "\""}
	out, ok := c.run("plan")
	if !ok {
		t.Fatalf("the plan naming the new version was refused:\n%s", out)
	}
	mustSay(t, "the plan", out, append(refusal, "Warning: "+versionRefused, "0 to add, 1 to change, 0 to destroy")...)
	sentNothing := clusterWrites(t, clusterAPIPath(name))
	out, ok = c.run("apply", "-auto-approve")
	if ok {
		t.Fatalf("the apply naming the new version passed:\n%s", out)
	}
	mustSay(t, "the apply", out, append(refusal, "Error: "+versionRefused, "This apply sent nothing")...)
	sentNothing("the apply")
	checkFakeStateNow(t, name, "failed")

	c.write(cancelledCluster(name, clusterTypeV2ID, held, 1))
	out, _ = c.run("plan")
	mustSay(t, "the plan with the old version", out, "No changes.")

	c.write(newVersion)
	out, ok = c.run("destroy", "-auto-approve")
	if !ok {
		t.Fatalf("the destroy naming the new version failed:\n%s", out)
	}
	mustSay(t, "the destroy", out, "Destroy complete! Resources: 1 destroyed.")
	if err := checkGoneAt(clusterAPIPath(name))(nil); err != nil {
		t.Error(err)
	}
}

// metadataCluster is a cluster at one worker with tags, and a description
// unless it's empty. It has no `timeouts` block, so an apply waits as long as
// a default configuration does.
func metadataCluster(name, displayName, description, team string) string {
	if description != "" {
		description = fmt.Sprintf("\n  description  = %q", description)
	}
	return fmt.Sprintf(`
resource %q "test" {
  id           = %q
  cluster_type = "%s/versions/%s"
  display_name = %q%s
  tags         = { team = %q }
  node_pool = {
    "gpu-workers" = { node_count = 1 }
  }
}
`, clusterType, name, clusterTypeRef, clusterTypeV2ID, displayName, description, team)
}

// checkFakeMetadata fails unless the fake holds what the apply sent.
func checkFakeMetadata(t *testing.T, name, displayName, description, team string) {
	t.Helper()
	status, body, err := readFake("/v1/regions/us-texas-1/projects/demo/compute/clusters/" + name)
	if err != nil || status != http.StatusOK {
		t.Fatalf("reading %s from the fake: %d %v", name, status, err)
	}
	var got struct {
		DisplayName string            `json:"displayName"`
		Description string            `json:"description"`
		Tags        map[string]string `json:"tags"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got.DisplayName != displayName || got.Description != description || got.Tags["team"] != team {
		t.Errorf("the fake holds display name %q, description %q, team %q, want %q, %q, %q",
			got.DisplayName, got.Description, got.Tags["team"], displayName, description, team)
	}
}

// Tags, the display name and the description can still change after a
// cancel. The platform leaves the cluster `failed` through each, so the apply
// doesn't wait for it to become `active`.
func TestAccAMetadataChangeToACancelledCluster(t *testing.T) {
	t.Parallel()
	// The cluster is cancelled with the action.
	skipUnderOpenTofu(t, openTofuHasNoActions)
	if runtime.GOOS == "windows" {
		skipTest(t, "the CLI sequence runs on unix")
	}
	const name = "acc-cancelled-metadata"
	stuck := "acceptance " + neverSettles
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
	cancelAfterTimeout(t, c, name, cancelledCluster(name, clusterTypeV2ID, stuck, 2))

	for _, step := range []struct{ what, displayName, description, team string }{
		{"a tag change", stuck, "", "ml"},
		{"a display name change", stuck + " renamed", "", "ml"},
		{"a description change", stuck + " renamed", "after the cancel", "ml"},
	} {
		c.write(metadataCluster(name, step.displayName, step.description, step.team))
		start := time.Now()
		out, ok := c.run("apply", "-auto-approve")
		if !ok {
			t.Fatalf("%s to a failed cluster failed:\n%s", step.what, out)
		}
		if took := time.Since(start); took > 30*time.Second {
			t.Errorf("%s to a failed cluster took %s", step.what, took.Round(time.Second))
		}
		mustSay(t, step.what, out, "0 added, 1 changed, 0 destroyed")
		checkFakeMetadata(t, name, step.displayName, step.description, step.team)
		checkFakeStateNow(t, name, "failed")
		out, _ = c.run("plan")
		mustSay(t, "the plan after "+step.what, out, "No changes.")
	}

	out, ok := c.run("destroy", "-auto-approve")
	if !ok {
		t.Fatalf("the destroy of the failed cluster failed:\n%s", out)
	}
	mustSay(t, "the destroy", out, "1 destroyed")
}

// A tag change to an `active` cluster still reaches the platform and plans
// clean after.
func TestAccATagChangeToAnActiveCluster(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		skipTest(t, "the CLI sequence runs on unix")
	}
	const name = "acc-active-metadata"
	c := newCLIRun(t)
	c.write(metadataCluster(name, "acceptance", "", "ml"))
	if out, ok := c.run("apply", "-auto-approve"); !ok {
		t.Fatalf("create:\n%s", out)
	}
	t.Cleanup(func() {
		if out, ok := c.run("destroy", "-auto-approve"); !ok {
			t.Errorf("cleanup destroy failed:\n%s", out)
		}
	})
	c.write(metadataCluster(name, "acceptance", "", "infra"))
	out, ok := c.run("apply", "-auto-approve")
	if !ok {
		t.Fatalf("the tag change failed:\n%s", out)
	}
	mustSay(t, "the tag change", out, "0 added, 1 changed, 0 destroyed")
	checkFakeMetadata(t, name, "acceptance", "", "infra")
	checkFakeStateNow(t, name, "active")
	out, _ = c.run("plan")
	mustSay(t, "the plan after the tag change", out, "No changes.")
}
