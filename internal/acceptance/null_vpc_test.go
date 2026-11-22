package acceptance

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

type nullVPCOwner struct {
	name, ownerType, ownerPath string
}

var nullVPCOwners = []nullVPCOwner{
	{
		name:      "acc-nullvpc-cluster-retry",
		ownerType: clusterType,
		ownerPath: "/v1/regions/us-texas-1/projects/demo/compute/clusters/acc-nullvpc-cluster-retry",
	},
	{
		name:      "acc-nullvpc-group-retry",
		ownerType: instanceGroupType,
		ownerPath: "/v1/regions/us-texas-1/projects/demo/compute/instance-groups/acc-nullvpc-group-retry",
	},
}

func nullVPCDestroyConfig(ownerType, name string) string {
	const remote = "/v1/regions/us-texas-1/projects/other/networking/vpcs/vpc-inference"
	return slowObject(ownerType, name, deletesSlowly, `delete = "2m"`) + fmt.Sprintf(`
data %q "owned" {
  id = %s.test.vpc
}

resource %q "first" {
  id         = "%s-first"
  vpc        = %s.test.vpc
  remote_vpc = %q
}

resource %q "second" {
  id         = "%s-second"
  vpc        = %s.test.vpc
  remote_vpc = %q
}
`, vpcType, ownerType, peeringType, name, ownerType, remote,
		peeringType, name, ownerType, remote)
}

// nullVPCWrites are writes a plain apply makes again after the owner's VPC
// is gone: a peering that names it as remote_vpc from a live VPC, and a
// security group binding on it.
func nullVPCWrites(ownerType, name, liveVPC string) string {
	return fmt.Sprintf(`
resource %q "toward" {
  id         = "%s-toward"
  vpc        = %q
  remote_vpc = %s.test.vpc
}

resource %q "binding" {
  vpc             = %s.test.vpc
  security_groups = []
}
`, peeringType, name, liveVPC, ownerType, vpcBindingType, ownerType)
}

// liveVPCOwner applies a cluster in a directory of its own, so its VPC stays
// up while the test tears the other owner down, and destroys it at the end.
func liveVPCOwner(t *testing.T, name string) string {
	t.Helper()
	live := newCLIRun(t)
	live.write(slowObject(clusterType, name, "live", `create = "2m"`))
	if out, ok := live.run("apply", "-auto-approve"); !ok {
		t.Fatalf("applying the live owner failed:\n%s", out)
	}
	t.Cleanup(func() {
		if out, ok := live.run("destroy", "-auto-approve"); !ok {
			t.Errorf("destroying the live owner failed:\n%s", out)
		}
	})
	return "vpc-" + name
}

// countSaid is how many times out, folded to single spaces, says want.
func countSaid(out, want string) int {
	return strings.Count(strings.Join(strings.Fields(strings.ReplaceAll(out, "│", " ")), " "), want)
}

// goneVPCWarnings counts, by address, the gone-VPC warnings a JSON plan gives.
func goneVPCWarnings(t *testing.T, c *cliRun) map[string]int {
	t.Helper()
	out, ok := c.run("plan", "-json")
	if !ok {
		t.Fatalf("the JSON plan failed:\n%s", out)
	}
	warned := map[string]int{}
	for _, line := range strings.Split(out, "\n") {
		var message struct {
			Type       string `json:"type"`
			Diagnostic struct {
				Severity string `json:"severity"`
				Summary  string `json:"summary"`
				Address  string `json:"address"`
			} `json:"diagnostic"`
		}
		if json.Unmarshal([]byte(line), &message) != nil || message.Type != "diagnostic" {
			continue
		}
		if d := message.Diagnostic; d.Severity == "warning" && d.Summary == "VPC gone while its owner is being torn down" {
			warned[d.Address]++
		}
	}
	return warned
}

func dropFakeVPC(t *testing.T, name string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, endpoint+
		"/_harness/regions/us-texas-1/projects/demo/vpcs/vpc-"+name+"/drop", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+fakeOwnerToken)
	resp, err := harnessHTTP.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("dropping the VPC answered %d", resp.StatusCode)
	}
	vpcPath := vpcAPIPath("demo", "vpc-"+name)
	if status, _, err := readFakeStore(vpcPath); err != nil || status != http.StatusNotFound {
		t.Fatalf("the VPC still resolves after the drop: %d %v", status, err)
	}
}

// An interrupted destroy can leave an owner deleting after the platform has
// removed its VPC. A plain retry must still evaluate the configuration's data
// source and peerings, finish the owner's delete, and leave nothing.
func TestAccADestroyRetryAfterTheOwnedVPCIsGone(t *testing.T) { //nolint:paralleltest // it interrupts a destroy through the real CLI
	if runtime.GOOS == "windows" {
		skipTest(t, "the CLI sequence runs on unix")
	}
	for _, tc := range nullVPCOwners {
		t.Run(tc.ownerType, func(t *testing.T) {
			name, ownerPath := tc.name, tc.ownerPath
			vpcPath := vpcAPIPath("demo", "vpc-"+name)
			liveVPC := liveVPCOwner(t, name+"-live")
			config := nullVPCDestroyConfig(tc.ownerType, name) + nullVPCWrites(tc.ownerType, name, liveVPC)
			c := newCLIRun(t)
			c.write(config)
			t.Cleanup(func() {
				if status, _, err := readFakeStore(ownerPath); err == nil && status == http.StatusOK {
					code, _, err := callFake(http.MethodDelete, ownerPath)
					if err != nil {
						t.Errorf("cleanup delete failed: %v", err)
						return
					}
					if code != http.StatusNoContent && code != http.StatusConflict {
						t.Errorf("cleanup delete answered %d", code)
						return
					}
					awaitFakeGone(t, ownerPath)
				}
			})

			if out, ok := c.run("apply", "-auto-approve"); !ok {
				t.Fatalf("apply failed:\n%s", out)
			}
			if out, ok := c.run("plan", "-detailed-exitcode"); !ok {
				t.Fatalf("a healthy owner's plan wasn't empty:\n%s", out)
			} else {
				mustSay(t, "the healthy plan", out, "No changes.")
			}

			deletes := &requestCounter{method: http.MethodDelete, path: ownerPath}
			deletes.start(t)()
			var interrupted bytes.Buffer
			destroy := c.command("destroy", "-auto-approve", "-no-color")
			destroy.Stdout, destroy.Stderr = &interrupted, &interrupted
			if err := destroy.Start(); err != nil {
				t.Fatal(err)
			}
			for deadline := time.Now().Add(2 * time.Minute); ; time.Sleep(20 * time.Millisecond) {
				n, err := fakeServed(http.MethodDelete, ownerPath)
				if err != nil {
					t.Fatal(err)
				}
				if n > deletes.before {
					break
				}
				if time.Now().After(deadline) {
					if err := destroy.Process.Kill(); err != nil {
						t.Errorf("stopping the destroy: %v", err)
					}
					if err := destroy.Wait(); err == nil {
						t.Error("the destroy without an owner delete unexpectedly passed")
					}
					t.Fatalf("the first destroy never started the owner delete:\n%s", interrupted.String())
				}
			}
			if err := destroy.Process.Signal(os.Interrupt); err != nil {
				t.Fatalf("interrupt: %v", err)
			}
			var exited *exec.ExitError
			if err := destroy.Wait(); !errors.As(err, &exited) {
				t.Fatalf("the interrupted destroy ended with %v, not a failure:\n%s", err, interrupted.String())
			}
			if got := fakeObject(t, ownerPath)["state"]; got != "deleting" {
				t.Fatalf("owner state = %v, want deleting", got)
			}

			dropFakeVPC(t, name)

			gone := fmt.Sprintf(`VPC %q in project "demo" of region "us-texas-1" is gone because the `+
				"cluster or instance group that owned it is being torn down. Applying this plan will fail "+
				"for this resource. Run `terraform destroy` to finish the teardown.", "vpc-"+name)
			planned, ok := c.run("plan")
			if !ok {
				t.Fatalf("a plain plan after the interrupt failed:\n%s", planned)
			}
			mustSay(t, "the plan after the interrupt", planned, "VPC released by its owner",
				"Plan: 4 to add", "Warning: VPC gone while its owner is being torn down", gone)
			// The CLI folds warnings with one summary into the first, so the
			// JSON plan says which resources warned.
			warned := goneVPCWarnings(t, c)
			for _, want := range []string{
				peeringType + ".first", peeringType + ".second", peeringType + ".toward", vpcBindingType + ".binding",
			} {
				if warned[want] != 1 {
					t.Errorf("%s warned %d times about the gone VPC, want once: %v", want, warned[want], warned)
				}
			}
			if len(warned) != 4 {
				t.Errorf("the gone VPC warned on %v, want the four writes only", warned)
			}

			peerings := &requestCounter{method: http.MethodPost, path: vpcPath + "/peerings"}
			peerings.start(t)()
			toward := &requestCounter{method: http.MethodPost, path: vpcAPIPath("demo", liveVPC) + "/peerings"}
			toward.start(t)()
			binding := &requestCounter{method: http.MethodPost, path: vpcPath}
			binding.start(t)()
			applied, ok := c.run("apply", "-auto-approve")
			if ok {
				t.Fatalf("a plain apply on the gone VPC passed:\n%s", applied)
			}
			advice := fmt.Sprintf(`VPC %q doesn't exist in project "demo" of region "us-texas-1". If the `+
				"cluster or instance group that owned it is being torn down, run `terraform destroy` to "+
				"finish the teardown. Otherwise check the VPC id in", "vpc-"+name)
			mustSay(t, "the plain apply", applied,
				fmt.Sprintf(`Error: Could not create the VPC peering because VPC %q doesn't exist`, "vpc-"+name),
				fmt.Sprintf(`Error: Could not attach the security groups to the VPC because VPC %q doesn't exist`, "vpc-"+name),
				"The platform answered 404 Not Found.", "The platform answered 422 Unprocessable Entity.",
				advice+" `vpc`.", advice+" `remote_vpc`.")
			if n := countSaid(applied, advice); n != 4 {
				t.Errorf("the apply gave the advice %d times, want 4:\n%s", n, applied)
			}
			// The service's 404 for a VPC under a write, as v1.3.0 answers it.
			if n := countSaid(applied, "Detail reasons: NOT_FOUND"); n != 3 {
				t.Errorf("the apply quoted NOT_FOUND %d times, want 3:\n%s", n, applied)
			}
			// The writes the apply already makes, one each, and nothing more.
			for _, sent := range []struct {
				counter *requestCounter
				want    int
			}{{peerings, 2}, {toward, 1}, {binding, 1}} {
				if got, err := fakeServed(http.MethodPost, sent.counter.path); err != nil || got-sent.counter.before != sent.want {
					t.Errorf("POST %s sent %d, want %d (%v)", sent.counter.path, got-sent.counter.before, sent.want, err)
				}
			}
			for _, path := range []string{
				vpcPath + "/peerings/" + name + "-first",
				vpcPath + "/peerings/" + name + "-second",
				vpcAPIPath("demo", liveVPC) + "/peerings/" + name + "-toward",
			} {
				if status, _, err := readFakeStore(path); err != nil || status != http.StatusNotFound {
					t.Errorf("the failed apply left %s: %d %v", path, status, err)
				}
			}
			if listed, listedOK := c.run("state", "list"); !listedOK || strings.Contains(listed, peeringType) ||
				strings.Contains(listed, vpcBindingType) {
				t.Errorf("the failed apply recorded a write in state:\n%s", listed)
			}

			c.write(config + fmt.Sprintf(`
data %q "also_missing" {
  id = "acc-nullvpc-also-missing"
}
`, vpcType))
			out, ok := c.run("destroy", "-auto-approve")
			if ok {
				t.Fatalf("a second missing VPC was silently accepted:\n%s", out)
			}
			mustSay(t, "the plan with a second missing VPC", out, "No such VPC")
			if strings.Contains(out, "Missing Configuration for Required Attribute") {
				t.Fatalf("the held VPC was null while checking the second missing VPC:\n%s", out)
			}
			c.write(config)
			out, ok = c.run("destroy", "-auto-approve")
			if !ok {
				t.Fatalf("the plain destroy retry failed:\n%s", out)
			}
			if listed, ok := c.run("state", "list"); !ok || strings.TrimSpace(listed) != "" {
				t.Errorf("state isn't empty after the retry:\n%s", listed)
			}
			for _, path := range []string{
				ownerPath,
				vpcPath,
				vpcPath + "/peerings/" + name + "-first",
				vpcPath + "/peerings/" + name + "-second",
			} {
				if status, _, err := readFakeStore(path); err != nil || status != http.StatusNotFound {
					t.Errorf("%s remains after the retry: %d %v", path, status, err)
				}
			}
			if out, ok := c.run("destroy", "-auto-approve"); !ok {
				t.Errorf("destroy with everything already gone failed:\n%s", out)
			} else {
				mustSay(t, "the empty destroy", out, "0 destroyed")
			}
		})
	}
}

// A failed first teardown releases the same networking values as a delete in
// flight and finishes.
func TestAccAFailedDestroyRetryAfterTheOwnedVPCIsGone(t *testing.T) {
	t.Parallel()
	for _, tc := range nullVPCOwners {
		t.Run(tc.ownerType, func(t *testing.T) {
			name := strings.Replace(tc.name, "-retry", "-failed", 1)
			ownerPath := strings.Replace(tc.ownerPath, tc.name, name, 1)
			c := newCLIRun(t)
			config := strings.Replace(nullVPCDestroyConfig(tc.ownerType, name),
				deletesSlowly, deletesSlowly+" fake-teardown-fails", 1)
			c.write(config)
			t.Cleanup(func() {
				if status, _, err := readFakeStore(ownerPath); err == nil && status == http.StatusOK {
					code, _, err := callFake(http.MethodDelete, ownerPath)
					if err != nil || (code != http.StatusNoContent && code != http.StatusConflict) {
						t.Errorf("cleanup delete answered %d: %v", code, err)
						return
					}
					awaitFakeGone(t, ownerPath)
				}
			})
			if out, ok := c.run("apply", "-auto-approve"); !ok {
				t.Fatalf("apply failed:\n%s", out)
			}
			if code, _, err := callFake(http.MethodDelete, ownerPath); err != nil || code != http.StatusNoContent {
				t.Fatalf("starting the teardown answered %d: %v", code, err)
			}
			for deadline := time.Now().Add(2 * time.Minute); ; time.Sleep(20 * time.Millisecond) {
				state := fakeObject(t, ownerPath)["state"]
				if state == "failed" {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("owner state = %v, want failed", state)
				}
			}
			dropFakeVPC(t, name)

			if out, ok := c.run("plan"); !ok {
				t.Fatalf("a plan after the failed teardown failed:\n%s", out)
			}
			out, ok := c.run("destroy", "-auto-approve")
			if !ok {
				t.Fatalf("the destroy after a failed teardown failed:\n%s", out)
			}
			if listed, ok := c.run("state", "list"); !ok || strings.TrimSpace(listed) != "" {
				t.Errorf("state isn't empty after the failed-teardown retry:\n%s", listed)
			}
			if status, _, err := readFakeStore(ownerPath); err != nil || status != http.StatusNotFound {
				t.Errorf("owner remains after the failed-teardown retry: %d %v", status, err)
			}
		})
	}
}

// The destroy exception doesn't turn a data source for a missing VPC into an
// empty result on an ordinary plan.
func TestAccAMissingVPCStillFailsAPlan(t *testing.T) {
	t.Parallel()
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		Steps: []resource.TestStep{{
			Config: fmt.Sprintf(`data %q "missing" {
  id = "acc-nullvpc-not-created"
}`, vpcType),
			PlanOnly:    true,
			ExpectError: regexp.MustCompile(`No such VPC`),
		}},
	})
}

func nullVPCDeletingOwner(t *testing.T, owner nullVPCOwner, extraConfig string) *cliRun {
	t.Helper()
	t.Cleanup(func() {
		if status, _, err := readFakeStore(owner.ownerPath); err == nil && status == http.StatusOK {
			code, _, err := callFake(http.MethodDelete, owner.ownerPath)
			if err != nil || (code != http.StatusNoContent && code != http.StatusConflict) {
				t.Errorf("cleanup delete answered %d: %v", code, err)
				return
			}
			awaitFakeGone(t, owner.ownerPath)
		}
	})

	c := newCLIRun(t)
	c.write(nullVPCDestroyConfig(owner.ownerType, owner.name) + extraConfig)
	if out, ok := c.run("apply", "-auto-approve"); !ok {
		t.Fatalf("apply failed:\n%s", out)
	}
	if out, ok := c.run("plan", "-detailed-exitcode"); !ok {
		t.Fatalf("healthy plan failed:\n%s", out)
	} else {
		mustSay(t, "healthy plan", out, "No changes.")
	}

	deletes := &requestCounter{method: http.MethodDelete, path: owner.ownerPath}
	deletes.start(t)()
	var interrupted bytes.Buffer
	destroy := c.command("destroy", "-auto-approve", "-no-color")
	destroy.Stdout, destroy.Stderr = &interrupted, &interrupted
	if err := destroy.Start(); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(2 * time.Minute); ; time.Sleep(20 * time.Millisecond) {
		n, err := fakeServed(http.MethodDelete, owner.ownerPath)
		if err != nil {
			t.Fatal(err)
		}
		if n > deletes.before {
			break
		}
		if time.Now().After(deadline) {
			if err := destroy.Process.Kill(); err != nil {
				t.Errorf("stopping the destroy: %v", err)
			}
			if err := destroy.Wait(); err == nil {
				t.Error("the destroy without an owner delete unexpectedly passed")
			}
			t.Fatalf("destroy never started the owner delete:\n%s", interrupted.String())
		}
	}
	if err := destroy.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	var exited *exec.ExitError
	if err := destroy.Wait(); !errors.As(err, &exited) {
		t.Fatalf("interrupted destroy ended with %v:\n%s", err, interrupted.String())
	}
	if got := fakeObject(t, owner.ownerPath)["state"]; got != "deleting" {
		t.Fatalf("owner state = %v, want deleting", got)
	}
	dropFakeVPC(t, owner.name)
	return c
}

// nullVPCSetOwnerVPCNull writes vpc = null into the owner's state, as a run
// before the hold could have left it.
func nullVPCSetOwnerVPCNull(t *testing.T, c *cliRun, owner nullVPCOwner) {
	t.Helper()
	statePath := filepath.Join(c.dir, "terraform.tfstate")
	raw, err := os.ReadFile(statePath) //nolint:gosec // Test state in its temporary directory.
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]json.RawMessage
	var resources []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(document["resources"], &resources); err != nil {
		t.Fatal(err)
	}
	changed := false
	for _, r := range resources {
		var mode, typ, name string
		for key, into := range map[string]*string{"mode": &mode, "type": &typ, "name": &name} {
			if err := json.Unmarshal(r[key], into); err != nil {
				t.Fatal(err)
			}
		}
		if mode != "managed" || typ != owner.ownerType || name != "test" {
			continue
		}
		var instances []map[string]json.RawMessage
		if err := json.Unmarshal(r["instances"], &instances); err != nil {
			t.Fatal(err)
		}
		for _, instance := range instances {
			var attributes map[string]json.RawMessage
			if err := json.Unmarshal(instance["attributes"], &attributes); err != nil {
				t.Fatal(err)
			}
			attributes["vpc"] = json.RawMessage("null")
			instance["attributes"] = mustJSON(t, attributes)
			changed = true
		}
		r["instances"] = mustJSON(t, instances)
	}
	if !changed {
		t.Fatal("owner state was not found")
	}
	document["resources"] = mustJSON(t, resources)
	if err := os.WriteFile(statePath, mustJSON(t, document), 0o600); err != nil {
		t.Fatal(err)
	}
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// A destroy without refresh finishes from state as it is, also when an
// earlier run left the owner's vpc null.
func TestAccADestroyReleasedVPCWithoutRefresh(t *testing.T) { //nolint:paralleltest // this drives and interrupts the CLI
	for i, owner := range nullVPCOwners {
		t.Run(owner.ownerType, func(t *testing.T) {
			owner.name = strings.Replace(owner.name, "-retry", "-no-refresh", 1)
			owner.ownerPath = strings.Replace(owner.ownerPath, nullVPCOwners[i].name, owner.name, 1)
			c := nullVPCDeletingOwner(t, owner, "")

			if i == 1 {
				nullVPCSetOwnerVPCNull(t, c, owner)
				shown, ok := c.run("state", "show", owner.ownerType+".test")
				if !ok {
					t.Fatalf("state show failed:\n%s", shown)
				}
				mustSay(t, "changed state", shown, "vpc", "null")
			}

			destroyed, ok := c.run("destroy", "-refresh=false", "-auto-approve")
			if !ok {
				t.Fatalf("destroy without refresh failed:\n%s", destroyed)
			}
			mustSay(t, "destroy without refresh", destroyed, "Destroy complete!")
			if listed, ok := c.run("state", "list"); !ok || strings.TrimSpace(listed) != "" {
				t.Fatalf("state is not empty after destroy without refresh:\n%s", listed)
			}
		})
	}
}
