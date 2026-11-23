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
		if d := message.Diagnostic; d.Severity == "warning" && d.Summary == "VPC gone after its owner's teardown started" {
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

			gone := fmt.Sprintf(`VPC %q in project "demo" of region "us-texas-1" is gone because its `+
				"owner's teardown started. Applying this plan will fail "+
				"for this resource. Run `terraform destroy` to finish the teardown.", "vpc-"+name)
			planned, ok := c.run("plan")
			if !ok {
				t.Fatalf("a plain plan after the interrupt failed:\n%s", planned)
			}
			mustSay(t, "the plan after the interrupt", planned, "VPC released by its owner",
				"Plan: 4 to add", "Warning: VPC gone after its owner's teardown started", gone)
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
			advice := fmt.Sprintf(`VPC %q doesn't exist in project "demo" of region "us-texas-1". If its `+
				"owner's teardown started, run `terraform destroy` to finish it. "+
				"Otherwise check the VPC id in", "vpc-"+name)
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

// assertHeldVPCPlan pins every value a successful plan's held read returns.
// A required argument can instead stop the plan after its recovery warning.
func assertHeldVPCPlan(t *testing.T, c *cliRun, owner nullVPCOwner, planError string) {
	t.Helper()
	planned, ok := c.run("plan", "-out=held.tfplan")
	if planError != "" {
		if ok {
			t.Fatalf("a plain plan after the interrupt passed, want %s:\n%s", planError, planned)
		}
		mustSay(t, "the failed plan after the interrupt", planned, "VPC released by its owner",
			"`terraform destroy -refresh=false`", planError)
		return
	}
	if !ok {
		t.Fatalf("a plain plan after the interrupt failed:\n%s", planned)
	}
	mustSay(t, "the plan after the interrupt", planned, "VPC released by its owner",
		"`terraform destroy -refresh=false`")
	shown, ok := c.run("show", "-json", "held.tfplan")
	if !ok {
		t.Fatalf("showing the held-read plan failed:\n%s", shown)
	}
	var plan struct {
		PriorState struct {
			Values struct {
				RootModule struct {
					Resources []struct {
						Address string                     `json:"address"`
						Values  map[string]json.RawMessage `json:"values"`
					} `json:"resources"`
				} `json:"root_module"`
			} `json:"values"`
		} `json:"prior_state"`
	}
	if err := json.Unmarshal([]byte(shown), &plan); err != nil {
		t.Fatal(err)
	}
	var values map[string]json.RawMessage
	for _, r := range plan.PriorState.Values.RootModule.Resources {
		if r.Address == "data."+vpcType+".owned" {
			values = r.Values
			break
		}
	}
	if values == nil {
		t.Fatalf("the held VPC isn't in the prior state:\n%s", shown)
	}
	want := map[string]string{
		"id":              string(mustJSON(t, "vpc-"+owner.name)),
		"region":          `"us-texas-1"`,
		"project":         `"demo"`,
		"self_link":       string(mustJSON(t, vpcAPIPath("demo", "vpc-"+owner.name))),
		"security_groups": "[]",
		"display_name":    "null",
		"backend":         "null",
		"owner_kind":      "null",
		"owner_id":        "null",
		"state":           "null",
		"ufo_cr_name":     "null",
	}
	if len(values) != len(want) {
		t.Errorf("the held VPC has %d attributes, want %d: %v", len(values), len(want), values)
	}
	for attribute, expected := range want {
		got, present := values[attribute]
		if !present || string(got) != expected {
			t.Errorf("the held VPC's %s = %s, want %s", attribute, got, expected)
		}
	}
}

// A default security group read whose id comes from its owner applies with
// the owner, then applies again when the owner changes.
func TestAccADefaultSecurityGroupReadAppliesWithOwnerChanges(t *testing.T) {
	t.Parallel()
	name := "acc-nullvpc-one-fresh-apply"
	c := newCLIRun(t)
	ownerConfig := slowObject(clusterType, name, "live", `create = "2m"`)
	config := ownerConfig + fmt.Sprintf(`
data %q "owned" {
  id = %s.test.vpc
}
data %q "default" {
  id = one(data.%s.owned.security_groups)
}
output "default_ingress" {
  value = data.%s.default.ingress
}
`, vpcType, clusterType, securityGroupType, vpcType, securityGroupType)
	c.write(config)
	t.Cleanup(func() {
		c.write(ownerConfig)
		if out, ok := c.run("destroy", "-auto-approve"); !ok {
			t.Errorf("cleanup destroy failed:\n%s", out)
		}
	})
	if out, ok := c.run("apply", "-auto-approve"); !ok {
		t.Fatalf("fresh apply failed:\n%s", out)
	}
	updated := strings.Replace(config, "node_count = 2", "node_count = 3", 1)
	if updated == config {
		t.Fatal("the owner change didn't replace node_count")
	}
	c.write(updated)
	if out, ok := c.run("apply", "-auto-approve"); !ok {
		t.Fatalf("apply after an owner change failed:\n%s", out)
	}
}

// The documented reads of a held VPC must also finish a destroy. Each case
// applies the owner and documented read together before the interrupt.
func TestAccADestroyRetryAfterTheOwnedVPCIsGoneDocumentedReads(t *testing.T) { //nolint:paralleltest // it interrupts a destroy through the real CLI
	if runtime.GOOS == "windows" {
		skipTest(t, "the CLI sequence runs on unix")
	}
	for _, tc := range nullVPCOwners {
		for _, repro := range []string{"self_link", "security_groups", "one", "walkthrough-precondition"} {
			t.Run(tc.ownerType+"/"+repro, func(t *testing.T) {
				owner := tc
				owner.name = strings.Replace(tc.name, "-retry", "-"+strings.ReplaceAll(repro, "_", "-"), 1)
				owner.ownerPath = strings.Replace(tc.ownerPath, tc.name, owner.name, 1)
				liveVPC := liveVPCOwner(t, owner.name+"-live")

				var extra string
				switch repro {
				case "self_link":
					extra = fmt.Sprintf(`
resource %q "viaself" {
  id         = "%s-viaself"
  vpc        = %q
  remote_vpc = data.%s.owned.self_link
}
`, peeringType, owner.name, liveVPC, vpcType)
				case "security_groups":
					extra = fmt.Sprintf(`
output "attached_groups" {
  value = data.%s.owned.security_groups
}
`, vpcType)
				case "one":
					extra = fmt.Sprintf(`
data %q "default" {
  id = one(data.%s.owned.security_groups)
}

output "default_ingress" {
  value = data.%s.default.ingress
}
`, securityGroupType, vpcType, securityGroupType)
				case "walkthrough-precondition":
					extra = fmt.Sprintf(`
data %q "all" {
  region = data.%s.owned.region
}

locals {
  def = [for g in data.%s.all.security_groups : g.id if g.owner_kind == "vpc" && contains(data.%s.owned.security_groups, g.id)]
}

resource %q "b" {
  vpc             = %s.test.vpc
  security_groups = local.def

  lifecycle {
    precondition {
      condition     = data.%s.owned.state == null || data.%s.owned.backend == "nico"
      error_message = "not nico"
    }
    precondition {
      condition     = data.%s.owned.backend != "nico" || length(local.def) == 1
      error_message = "not one default group"
    }
  }
}
`, securityGroupsType, vpcType, securityGroupsType, vpcType, vpcBindingType, owner.ownerType,
						vpcType, vpcType, vpcType)
				}

				c := nullVPCDeletingOwner(t, owner, extra)
				planError := ""
				destroyArgs := []string{"destroy", "-auto-approve"}
				if repro == "one" {
					planError = "Missing Configuration for Required Attribute"
					destroyArgs = []string{"destroy", "-refresh=false", "-auto-approve"}
				}
				assertHeldVPCPlan(t, c, owner, planError)
				destroyed, ok := c.run(destroyArgs...)
				if !ok {
					t.Fatalf("the destroy retry using %s failed:\n%s", repro, destroyed)
				}
				mustSay(t, "the destroy retry using "+repro, destroyed, "Destroy complete!")
				if listed, ok := c.run("state", "list"); !ok || strings.TrimSpace(listed) != "" {
					t.Fatalf("state isn't empty after the %s retry:\n%s", repro, listed)
				}
			})
		}
	}
}

// A literal VPC id can be read before its owner during a destroy retry. The
// checked-in examples must name the recovery command when that read fails.
func TestAccADestroyRetryAfterLiteralVPCReadNamesRecovery(t *testing.T) { //nolint:paralleltest // it interrupts a destroy through the real CLI
	if runtime.GOOS == "windows" {
		skipTest(t, "the CLI sequence runs on unix")
	}
	examples := []struct {
		name, path, literal string
	}{
		{name: "vpc", path: fmt.Sprintf("examples/data-sources/%s/data-source.tf", vpcType), literal: "vpc-training"},
		{name: "security-group", path: fmt.Sprintf("examples/data-sources/%s/data-source.tf", securityGroupType), literal: "training-vpc"},
	}
	for _, tc := range nullVPCOwners {
		for _, example := range examples {
			t.Run(tc.ownerType+"/"+example.name, func(t *testing.T) {
				owner := tc
				owner.name = strings.Replace(tc.name, "-retry", "-literal-"+example.name, 1)
				owner.ownerPath = strings.Replace(tc.ownerPath, tc.name, owner.name, 1)
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

				body, err := os.ReadFile(filepath.Join("..", "..", filepath.FromSlash(example.path))) //nolint:gosec // A checked-in example.
				if err != nil {
					t.Fatal(err)
				}
				extra := strings.ReplaceAll(string(body), `"`+example.literal+`"`, fmt.Sprintf("%q", "vpc-"+owner.name))
				if example.name == "security-group" {
					extra = strings.Replace(extra, `"shared-ingress"`, securityGroupType+".shared.id", 1)
					extra = fmt.Sprintf(`
resource %q "shared" {
  id = "%s-shared"
  ingress {
    action                 = "permit"
    protocol               = "tcp"
    source_prefix          = "203.0.113.0/24"
    destination_prefix     = "0.0.0.0/0"
    destination_port_range = "6443"
  }
}
`, securityGroupType, owner.name) + extra
				}
				if strings.Contains(extra, example.literal) || strings.Contains(extra, `"shared-ingress"`) {
					t.Fatal("example substitution failed")
				}

				c := newCLIRun(t)
				ownerConfig := slowObject(owner.ownerType, owner.name, deletesSlowly, `delete = "2m"`)
				c.write(ownerConfig)
				if out, ok := c.run("apply", "-auto-approve"); !ok {
					t.Fatalf("applying the owner failed:\n%s", out)
				}
				c.write(ownerConfig + extra)
				if out, ok := c.run("apply", "-auto-approve"); !ok {
					t.Fatalf("applying the checked-in example failed:\n%s", out)
				}
				if out, ok := c.run("plan", "-detailed-exitcode"); !ok {
					t.Fatalf("healthy plan failed:\n%s", out)
				} else {
					mustSay(t, "healthy plan", out, "No changes.")
				}

				interruptOwnerDestroy(t, c, owner.ownerPath)
				dropFakeVPC(t, owner.name)
				planned, ok := c.run("plan")
				if ok {
					t.Fatalf("a plain plan after the interrupt passed:\n%s", planned)
				}
				mustSay(t, "the failed plan after the interrupt", planned,
					"No such VPC", "`terraform destroy -refresh=false`")

				destroyed, ok := c.run("destroy", "-refresh=false", "-auto-approve")
				if !ok {
					t.Fatalf("destroy without refresh failed:\n%s", destroyed)
				}
				mustSay(t, "destroy without refresh", destroyed, "Destroy complete!")
				if listed, ok := c.run("state", "list"); !ok || strings.TrimSpace(listed) != "" {
					t.Fatalf("state isn't empty after destroy without refresh:\n%s", listed)
				}
			})
		}
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

			planned, ok := c.run("plan")
			if !ok {
				t.Fatalf("a plan after the failed teardown failed:\n%s", planned)
			}
			mustSay(t, "the plan after the failed teardown", planned,
				"Warning: VPC gone after its owner's teardown started")
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

// interruptOwnerDestroy stops a destroy after the fake accepts the owner's
// delete, while its state still says deleting.
func interruptOwnerDestroy(t *testing.T, c *cliRun, ownerPath string) {
	t.Helper()
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
			t.Fatalf("destroy never started the owner delete:\n%s", interrupted.String())
		}
	}
	if err := destroy.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("interrupt: %v", err)
	}
	var exited *exec.ExitError
	if err := destroy.Wait(); !errors.As(err, &exited) {
		t.Fatalf("interrupted destroy ended with %v:\n%s", err, interrupted.String())
	}
	if got := fakeObject(t, ownerPath)["state"]; got != "deleting" {
		t.Fatalf("owner state = %v, want deleting", got)
	}
	t.Logf("interrupted destroy output:\n%s", interrupted.String())
}

// The full walkthrough's destroy retry exercises its default-group lookup,
// preconditions and outputs, not a reduced copy of their expressions.
func TestAccAWalkthroughFullDestroyRetryAfterTheOwnedVPCIsGone(t *testing.T) {
	if runtime.GOOS == "windows" {
		skipTest(t, "the CLI sequence runs on unix")
	}
	const prefix = "acc-nullvpc-full"
	name := prefix + "-walkthrough"
	ownerPath := clusterAPIPath(name)
	c := newCLIRun(t)
	for _, file := range []string{"main.tf", "outputs.tf", "variables.tf"} {
		body, err := os.ReadFile(filepath.Join("..", "..", "examples", "walkthrough", "full", file)) //nolint:gosec // A checked-in example.
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(c.dir, file), string(body))
	}
	keyPath := filepath.Join(c.dir, "id_ed25519.pub")
	writeFile(t, keyPath, "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAICalTR9icsm8WarWXVQxwy8TRDjP7bQ/eoJdFARMvUlQ walkthrough\n")
	writeFile(t, filepath.Join(c.dir, "terraform.tfvars"), fmt.Sprintf(`
prefix              = %q
region              = "us-texas-1"
project             = "demo"
cluster_type        = "gpu-training"
ssh_public_key_file = %q
allowed_cidrs       = ["203.0.113.0/24", "198.51.100.0/25"]
webhook_url         = "https://hooks.example.com/k0rdent"
test_purpose        = %q
`, prefix, keyPath, deletesSlowly))
	t.Setenv("TF_VAR_webhook_secret", strings.Repeat("0123456789abcdef", 4))
	t.Cleanup(func() {
		if out, ok := c.run("destroy", "-auto-approve"); !ok {
			t.Logf("cleanup destroy failed:\n%s", out)
		}
	})

	applied, ok := c.run("apply", "-auto-approve")
	if !ok {
		t.Fatalf("the full walkthrough apply failed:\n%s", applied)
	}
	t.Logf("full walkthrough apply output:\n%s", applied)
	if out, planOK := c.run("plan", "-detailed-exitcode"); !planOK {
		t.Fatalf("the full walkthrough's healthy plan wasn't empty:\n%s", out)
	}

	interruptOwnerDestroy(t, c, ownerPath)
	dropFakeVPC(t, name)

	planned, planOK := c.run("plan")
	if !planOK {
		t.Fatalf("the full walkthrough's plain plan after the interrupt failed:\n%s", planned)
	}
	mustSay(t, "the full walkthrough's plan after the interrupt", planned,
		"VPC released by its owner", "`terraform destroy -refresh=false`")

	destroyed, ok := c.run("destroy", "-auto-approve")
	t.Logf("plain destroy retry output:\n%s", destroyed)
	if !ok {
		t.Fatalf("the full walkthrough's plain destroy retry failed:\n%s", destroyed)
	}
	mustSay(t, "the full walkthrough's destroy retry", destroyed, "Destroy complete!")
	if listed, ok := c.run("state", "list"); !ok || strings.TrimSpace(listed) != "" {
		t.Fatalf("the full walkthrough's state isn't empty after its retry:\n%s", listed)
	}
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
	config := nullVPCDestroyConfig(owner.ownerType, owner.name)
	c.write(config + extraConfig)
	if out, ok := c.run("apply", "-auto-approve"); !ok {
		t.Fatalf("applying the owner and documented read failed:\n%s", out)
	}
	if out, ok := c.run("plan", "-detailed-exitcode"); !ok {
		t.Fatalf("healthy plan failed:\n%s", out)
	} else {
		mustSay(t, "healthy plan", out, "No changes.")
	}

	interruptOwnerDestroy(t, c, owner.ownerPath)
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

// This is a regression test for destroy without refresh, not proof of the
// owner-side hold. It also covers state from before the hold, with vpc null.
func TestAccADestroyReleasedVPCWithoutRefresh(t *testing.T) { //nolint:paralleltest // this drives and interrupts the CLI
	for i, owner := range nullVPCOwners {
		t.Run(owner.ownerType, func(t *testing.T) {
			owner.name = strings.Replace(owner.name, "-retry", "-no-refresh", 1)
			owner.ownerPath = strings.Replace(owner.ownerPath, nullVPCOwners[i].name, owner.name, 1)
			c := nullVPCDeletingOwner(t, owner, "")

			if i == 1 {
				nullVPCSetOwnerVPCNull(t, c, owner)
				shown, ok := c.run("show", "-json")
				if !ok {
					t.Fatalf("state JSON show failed:\n%s", shown)
				}
				var state struct {
					Values struct {
						RootModule struct {
							Resources []struct {
								Address string                     `json:"address"`
								Values  map[string]json.RawMessage `json:"values"`
							} `json:"resources"`
						} `json:"root_module"`
					} `json:"values"`
				}
				if err := json.Unmarshal([]byte(shown), &state); err != nil {
					t.Fatal(err)
				}
				found := false
				for _, r := range state.Values.RootModule.Resources {
					if r.Address != owner.ownerType+".test" {
						continue
					}
					found = true
					if vpc, present := r.Values["vpc"]; !present || string(vpc) != "null" {
						t.Errorf("the changed owner's vpc = %s, want explicit null", string(vpc))
					}
				}
				if !found {
					t.Errorf("the changed owner isn't in state JSON:\n%s", shown)
				}
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
