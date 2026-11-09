package acceptance

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"
)

// The platform refuses a DELETE of a subnet, a security group, a VPC peering
// or a network that is still `creating`, with a CONFLICT_IN_USE 409: each
// one's delete moves it to `deleting` only from `active` or `failed`. One
// Ctrl-C during an apply leaves them that way, tainted in state, and the
// `terraform destroy` that follows has to wait for each create to finish
// rather than fail on the refusal. So, through the real CLI:
//
//  1. `terraform apply` is interrupted while each one is still `creating`
//  2. state holds each, tainted, and the platform still reads it `creating`
//  3. `terraform destroy` removes them all, and state is empty
func TestAccADestroyAfterAnInterruptWaitsOutEachCreate(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		skipTest(t, "the CLI sequence runs on unix")
	}
	const (
		owner   = "acc-mid-create-owner"
		parent  = "acc-mid-create-parent"
		network = "acc-mid-create-network"
		subnet  = "acc-mid-create-subnet"
		group   = "acc-mid-create-sg"
		peering = "acc-mid-create-peering"
		project = "/v1/regions/us-texas-1/projects/demo"
		remote  = "/v1/regions/us-texas-1/projects/other/networking/vpcs/vpc-inference"
	)
	slow := map[string]string{
		networkType + ".slow":       project + "/networking/networks/" + network,
		subnetType + ".slow":        project + "/networking/networks/" + parent + "/subnets/" + subnet,
		securityGroupType + ".slow": project + "/networking/security-groups/" + group,
		peeringType + ".slow":       project + "/networking/vpcs/vpc-" + owner + "/peerings/" + peering,
	}
	c := newCLIRun(t)
	c.write(clusterConfig(owner, 1) + evpnConfig(parent, "acceptance") + fmt.Sprintf(`
resource %[1]q "slow" {
  id           = %[2]q
  display_name = %[3]q
  evpn = {
    service_type = "l3vpn"
    vlan_id      = "auto"
  }
}

resource %[4]q "slow" {
  id            = %[5]q
  network       = %[6]s.test.id
  address_pool  = "/v1/regions/us-texas-1/networking/address-pools/tenant"
  prefix_length = 24
  display_name  = %[3]q
}

resource %[7]q "slow" {
  id           = %[8]q
  display_name = %[3]q
}

resource %[9]q "slow" {
  id           = %[10]q
  vpc          = "vpc-%[11]s"
  remote_vpc   = %[12]q
  display_name = %[3]q
  depends_on   = [%[13]s.test]
}
`, networkType, network, createsSlowly, subnetType, subnet, networkType, securityGroupType, group,
		peeringType, peering, owner, remote, clusterType))

	destroyed := false
	t.Cleanup(func() {
		if destroyed {
			return
		}
		// Let every create finish, so the destroy that cleans up isn't the
		// one under test.
		for _, path := range slow {
			if status, _, err := readFake(path); err == nil && status == http.StatusOK {
				awaitFakeState(t, path, "active")
			}
		}
		if out, ok := c.run("destroy", "-auto-approve"); !ok {
			t.Errorf("cleanup destroy failed:\n%s", out)
		}
	})

	var out bytes.Buffer
	apply := c.command("apply", "-auto-approve", "-no-color")
	apply.Stdout, apply.Stderr = &out, &out
	if err := apply.Start(); err != nil {
		t.Fatal(err)
	}
	// Each object is read once it's there and not again, since every read is
	// a step of the fake's settle.
	seen := map[string]bool{}
	for deadline := time.Now().Add(2 * time.Minute); len(seen) < len(slow); time.Sleep(100 * time.Millisecond) {
		for addr, path := range slow {
			if seen[addr] {
				continue
			}
			if status, _, err := readFake(path); err == nil && status == http.StatusOK {
				seen[addr] = true
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %v were accepted, and stopping the apply said: %v\n%s", seen, apply.Process.Kill(), out.String())
		}
	}
	if err := apply.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("interrupt: %v", err)
	}
	var exited *exec.ExitError
	if err := apply.Wait(); !errors.As(err, &exited) {
		t.Fatalf("the interrupted apply ended with %v, not a failure:\n%s", err, out.String())
	}

	pulled, ok := c.run("state", "pull")
	if !ok {
		t.Fatalf("state pull failed:\n%s", pulled)
	}
	var state struct {
		Resources []struct {
			Mode, Type, Name string
			Instances        []struct {
				Status string `json:"status"`
			} `json:"instances"`
		} `json:"resources"`
	}
	if err := json.Unmarshal([]byte(pulled), &state); err != nil {
		t.Fatalf("state after the interrupt isn't JSON: %v\n%s", err, pulled)
	}
	kept := map[string]string{}
	for _, r := range state.Resources {
		for _, i := range r.Instances {
			kept[r.Type+"."+r.Name] = i.Status
		}
	}
	for addr, path := range slow {
		if status, present := kept[addr]; !present || status != "tainted" {
			t.Errorf("%s is in state as %q (present %v), want tainted", addr, status, present)
		}
		// Without this the destroy below could pass against objects that
		// had already settled, and prove nothing.
		checkStoredState(t, path, "creating")
	}
	if t.Failed() {
		t.Fatalf("state after the interrupt:\n%s\napply said:\n%s", pulled, out.String())
	}

	gone, ok := c.run("destroy", "-auto-approve")
	if !ok {
		t.Fatalf("the destroy after the interrupt failed:\n%s", gone)
	}
	destroyed = true
	if listed, ok := c.run("state", "list"); !ok || strings.TrimSpace(listed) != "" {
		t.Errorf("state isn't empty after the destroy:\n%s", listed)
	}
	for addr, path := range slow {
		if status, _, err := readFakeStore(path); err != nil || status != http.StatusNotFound {
			t.Errorf("%s is still on the platform after the destroy: %d %v", addr, status, err)
		}
	}
}

// checkStoredState fails unless the fake's store reads path in state want.
// The read is one step of the fake's settle, like any other.
func checkStoredState(t *testing.T, path, want string) {
	t.Helper()
	status, body, err := readFakeStore(path)
	if err != nil || status != http.StatusOK {
		t.Errorf("reading %s: %d %v", path, status, err)
		return
	}
	var got struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal(body, &got); err != nil || got.State != want {
		t.Errorf("%s reads %q, want %q (%v)", path, got.State, want, err)
	}
}
