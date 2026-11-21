package acceptance

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
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
			c := newCLIRun(t)
			c.write(nullVPCDestroyConfig(tc.ownerType, name))
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

			if out, ok := c.run("plan"); !ok {
				t.Fatalf("a plain plan after the interrupt failed:\n%s", out)
			} else {
				mustSay(t, "the plan after the interrupt", out, "VPC released by its owner")
			}

			c.write(nullVPCDestroyConfig(tc.ownerType, name) + fmt.Sprintf(`
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
			c.write(nullVPCDestroyConfig(tc.ownerType, name))
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
