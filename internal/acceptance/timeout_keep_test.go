package acceptance

import (
	"encoding/json"
	"fmt"
	"runtime"
	"strings"
	"testing"
)

// timeoutConfig is a slow cluster with the postcondition an unattended
// pipeline uses to reject a create that isn't active yet.
func timeoutConfig(name, createTimeout string, keep bool) string {
	config := slowObject(clusterType, name, createsSlowly, `create = "`+createTimeout+`"`)
	if keep {
		config = strings.Replace(config, "  timeouts {", "  on_create_timeout = \"keep\"\n\n  timeouts {", 1)
	}
	return strings.TrimSuffix(config, "}\n") + `
  lifecycle {
    postcondition {
      condition     = self.state == "active"
      error_message = "The cluster is not active."
    }
  }
}
`
}

type cliInstance struct {
	Status     string `json:"status"`
	Attributes struct {
		ID              string `json:"id"`
		UID             string `json:"uid"`
		State           string `json:"state"`
		OnCreateTimeout string `json:"on_create_timeout"`
	} `json:"attributes"`
}

// cliInstance reads one managed instance exactly as Terraform saved it.
func (c *cliRun) cliInstance(addr string) cliInstance {
	c.t.Helper()
	out, ok := c.run("state", "pull")
	if !ok {
		c.t.Fatalf("state pull failed:\n%s", out)
	}
	var state struct {
		Resources []struct {
			Type      string        `json:"type"`
			Name      string        `json:"name"`
			Instances []cliInstance `json:"instances"`
		} `json:"resources"`
	}
	if err := json.Unmarshal([]byte(out), &state); err != nil {
		c.t.Fatalf("state pull isn't JSON: %v\n%s", err, out)
	}
	for _, r := range state.Resources {
		if r.Type+"."+r.Name == addr && len(r.Instances) == 1 {
			return r.Instances[0]
		}
	}
	c.t.Fatalf("no %s in state:\n%s", addr, out)
	return cliInstance{}
}

// A kept create reaches its saved-plan apply without a manual untaint. The
// postcondition rejects the timed-out run, state remains untainted and says
// creating, and the resume uses the same platform UID and sends no second
// create. A retry after the platform already settled plans nothing. The
// unset default retains the ordinary tainted-create recovery.
func TestAccACreateTimeoutCanKeepTheSameCluster(t *testing.T) { //nolint:paralleltest // it counts every create the fake serves
	if runtime.GOOS == "windows" {
		skipTest(t, "the CLI sequence runs on unix")
	}
	const collection = "/v1/regions/us-texas-1/projects/demo/compute/clusters"
	addr := clusterType + ".test"

	t.Run("retry while creating", func(t *testing.T) {
		const name = "acc-timeout-keep-retry"
		path := clusterAPIPath(name)
		c := newCLIRun(t)
		c.write(timeoutConfig(name, "2s", true))
		creates := &requestCounter{method: "POST", path: collection}
		creates.start(t)()
		t.Cleanup(func() {
			if out, ok := c.run("destroy", "-auto-approve"); !ok {
				t.Errorf("cleanup destroy failed:\n%s", out)
			}
		})

		if out, ok := c.run("plan", "-out=create.tfplan"); !ok {
			t.Fatalf("the create plan failed:\n%s", out)
		}
		out, ok := c.run("apply", "create.tfplan")
		if ok {
			t.Fatalf("the postcondition passed while the cluster was creating:\n%s", out)
		}
		mustSay(t, "the timed-out apply", out, "The cluster is not active", "is still being created")
		before := c.cliInstance(addr)
		if before.Status == "tainted" {
			t.Fatalf("the kept cluster is tainted:\n%s", out)
		}
		if before.Attributes.State != "creating" || before.Attributes.OnCreateTimeout != "keep" {
			t.Errorf("state after the timeout = %+v, want creating with the keep setting", before.Attributes)
		}
		uid := fakeUID(t, path)
		if c.heldUID(addr) != uid {
			t.Errorf("private uid %q, want platform uid %q", c.heldUID(addr), uid)
		}

		out, ok = c.run("plan", "-out=resume.tfplan")
		if !ok {
			t.Fatalf("the resume plan failed before apply, so no wait could run:\n%s", out)
		}
		mustSay(t, "the resume plan", out, "0 to add, 1 to change, 0 to destroy",
			`creating" -> (known after apply)`)
		if out, ok = c.run("apply", "resume.tfplan"); !ok {
			t.Fatalf("the saved resume plan failed:\n%s", out)
		}
		mustSay(t, "the resumed apply", out, "0 added, 1 changed, 0 destroyed")
		after := c.cliInstance(addr)
		if after.Status == "tainted" || after.Attributes.State != "active" || after.Attributes.UID != uid {
			t.Errorf("state after the resume = %+v, want an untainted active cluster at uid %q", after.Attributes, uid)
		}
		if out, code := c.runCode("plan", "-detailed-exitcode"); code != 0 {
			t.Errorf("the plan after the resume exited %d:\n%s", code, out)
		}
		if err := creates.once()(nil); err != nil {
			t.Error(err)
		}
	})

	t.Run("retry after creation finished", func(t *testing.T) {
		const name = "acc-timeout-keep-finished"
		path := clusterAPIPath(name)
		c := newCLIRun(t)
		c.write(timeoutConfig(name, "1s", true))
		creates := &requestCounter{method: "POST", path: collection}
		creates.start(t)()
		t.Cleanup(func() {
			if out, ok := c.run("destroy", "-auto-approve"); !ok {
				t.Errorf("cleanup destroy failed:\n%s", out)
			}
		})

		if out, ok := c.run("plan", "-out=create.tfplan"); !ok {
			t.Fatalf("the create plan failed:\n%s", out)
		}
		if out, ok := c.run("apply", "create.tfplan"); ok {
			t.Fatal("the timed-out create passed its postcondition")
		} else {
			mustSay(t, "the timed-out apply", out, "The cluster is not active")
		}
		uid := fakeUID(t, path)
		awaitFakeState(t, path, "active")

		out, ok := c.run("plan", "-out=finished.tfplan")
		if !ok {
			t.Fatalf("the plan after creation finished failed:\n%s", out)
		}
		mustSay(t, "the settled plan", out, "No changes.")
		if out, ok = c.run("apply", "finished.tfplan"); !ok {
			t.Fatalf("applying the settled plan failed:\n%s", out)
		}
		after := c.cliInstance(addr)
		if after.Status == "tainted" || after.Attributes.State != "active" || after.Attributes.UID != uid {
			t.Errorf("state after the settled apply = %+v, want an untainted active cluster at uid %q", after.Attributes, uid)
		}
		if err := creates.once()(nil); err != nil {
			t.Error(err)
		}
	})

	t.Run("default still taints", func(t *testing.T) {
		const name = "acc-timeout-default"
		c := newCLIRun(t)
		c.write(timeoutConfig(name, "1s", false))
		t.Cleanup(func() {
			if out, ok := c.run("destroy", "-auto-approve"); !ok {
				t.Errorf("cleanup destroy failed:\n%s", out)
			}
		})
		if out, ok := c.run("plan", "-out=create.tfplan"); !ok {
			t.Fatalf("the create plan failed:\n%s", out)
		}
		out, ok := c.run("apply", "create.tfplan")
		if ok {
			t.Fatalf("the default create passed while it was creating:\n%s", out)
		}
		mustSay(t, "the default apply", out, "is still being created", "terraform untaint")
		if got := c.cliInstance(addr); got.Status != "tainted" {
			t.Errorf("the default row is %+v, want tainted", got)
		}
	})
}

// Changing the provider-only timeout policy on an active cluster records the
// setting in place and sends nothing to the platform.
func TestAccClusterOnCreateTimeoutChangesInPlace(t *testing.T) {
	t.Parallel()
	const name = "acc-timeout-policy-update"
	path := clusterAPIPath(name)
	c := newCLIRun(t)
	c.write(clusterConfig(name, 2))
	if out, ok := c.run("apply", "-auto-approve"); !ok {
		t.Fatalf("the create failed:\n%s", out)
	}
	t.Cleanup(func() {
		if out, ok := c.run("destroy", "-auto-approve"); !ok {
			t.Errorf("cleanup destroy failed:\n%s", out)
		}
	})
	before, err := fakeServed("PATCH", path)
	if err != nil {
		t.Fatal(err)
	}
	c.write(strings.Replace(clusterConfig(name, 2), `display_name = "acceptance"`,
		`display_name = "acceptance"`+"\n  on_create_timeout = \"keep\"", 1))
	out, ok := c.run("apply", "-auto-approve")
	if !ok {
		t.Fatalf("the policy update failed:\n%s", out)
	}
	mustSay(t, "the policy update", out, "0 added, 1 changed, 0 destroyed")
	if after, err := fakeServed("PATCH", path); err != nil || after != before {
		t.Errorf("the policy update sent %d PATCHes, want none: %v", after-before, err)
	}
	if got := c.cliInstance(clusterType + ".test"); got.Attributes.OnCreateTimeout != "keep" || got.Attributes.State != "active" {
		t.Errorf("state after the policy update = %+v", got.Attributes)
	}
	if out, code := c.runCode("plan", "-detailed-exitcode"); code != 0 {
		t.Errorf("the plan after the policy update exited %d:\n%s", code, out)
	}
}

// The attribute takes only the two meanings the implementation supports.
func TestAccClusterOnCreateTimeoutRefusesAnotherValue(t *testing.T) {
	t.Parallel()
	c := newCLIRun(t)
	c.write(fmt.Sprintf(`
resource %q "test" {
  id                = "acc-timeout-invalid"
  cluster_type      = %q
  on_create_timeout = "delete"
}
`, clusterType, clusterTypeRef))
	out, ok := c.run("plan")
	if ok {
		t.Fatalf("an unknown timeout policy passed:\n%s", out)
	}
	mustSay(t, "the invalid policy", out, "on_create_timeout", `"taint"`, `"keep"`)
}
