package acceptance

import (
	"fmt"
	"net/http"
	"runtime"
	"strings"
	"testing"
	"time"
)

// noDefaultBindingConfig is a cluster whose VPC has no subnets, and so no
// default group on API v1.3.0-rc.4, with a tenant group bound to that VPC.
// Nothing orders the group after the cluster, so a destroy deletes the two
// in parallel once the binding is gone.
func noDefaultBindingConfig(cluster, group string) string {
	return clusterConfig(cluster, 1) + securityGroupConfig(group, false) + fmt.Sprintf(`
resource %q "test" {
  vpc             = %s.test.vpc
  security_groups = [%s.test.id]
}
`, vpcBindingType, clusterType, securityGroupType)
}

// A VPC with no default group refuses `[]`, so a destroy that sent it failed
// and only `terraform state rm` got past it. The destroy leaves the groups on
// the VPC instead, and the group's delete waits for the VPC to go with its
// cluster, so one `terraform destroy` ends with nothing left.
func TestAccABindingOnAVPCWithNoDefaultGroupDestroysInOneRun(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the CLI sequence runs on unix")
	}
	const cluster, group = "acc-nodef-no-subnets", "acc-nodef-filter"
	c := newCLIRun(t)
	c.write(noDefaultBindingConfig(cluster, group))
	destroyed := false
	t.Cleanup(func() {
		if !destroyed {
			c.run("state", "rm", vpcBindingType+".test")
			c.run("destroy", "-auto-approve")
		}
	})

	out, ok := c.run("apply", "-auto-approve")
	if !ok {
		t.Fatalf("the apply failed:\n%s", out)
	}
	// The VPC isn't known until the cluster exists, so only the apply can say.
	mustSay(t, "the apply", out, "This binding can't be taken off the VPC later",
		`"vpc-`+cluster+`" has no default security group to fall back to`)

	if out, ok = c.run("plan", "-detailed-exitcode"); !ok {
		t.Errorf("the plan after the apply isn't empty:\n%s", out)
	}

	vpcPath := "/v1/regions/us-texas-1/projects/demo/networking/vpcs/vpc-" + cluster
	written, err := fakeServed(http.MethodPost, vpcPath)
	if err != nil {
		t.Fatal(err)
	}
	out, ok = c.run("destroy", "-auto-approve")
	if !ok {
		t.Fatalf("the destroy failed:\n%s", out)
	}
	destroyed = true
	mustSay(t, "the destroy", out,
		"Destroying this resource leaves the groups on the VPC",
		"Left the security groups on a VPC with no default group",
		"3 destroyed")
	// The destroy read the VPC's groups first, so it never sent the `[]` the
	// VPC would refuse.
	if after, err := fakeServed(http.MethodPost, vpcPath); err != nil || after != written {
		t.Errorf("the destroy wrote the VPC's binding %d times, want none: %v", after-written, err)
	}
	if listed, ok := c.run("state", "list"); !ok || strings.TrimSpace(listed) != "" {
		t.Errorf("state isn't empty after the destroy:\n%s", listed)
	}
	for _, path := range []string{
		"/v1/regions/us-texas-1/projects/demo/compute/clusters/" + cluster,
		"/v1/regions/us-texas-1/projects/demo/networking/vpcs/vpc-" + cluster,
		"/v1/regions/us-texas-1/projects/demo/networking/security-groups/" + group,
	} {
		if status, _, err := readFake(path); err != nil || status != http.StatusNotFound {
			t.Errorf("%s is still there after the destroy: %d %v", path, status, err)
		}
	}
}

// The wait is only for a VPC this run's binding destroy left holding the
// group. A group bound to a VPC some other way still fails its delete at
// once, naming the VPC, as before.
func TestAccAGroupHeldByAVPCThisRunDidntLeaveFailsFast(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the CLI sequence runs on unix")
	}
	const cluster, group = "acc-nodef-held-no-subnets", "acc-nodef-held"
	c := newCLIRun(t)
	// A delete that waited would stop at this timeout, well past the bound
	// checked below.
	c.write(clusterConfig(cluster, 1) + fmt.Sprintf(`
resource %q "test" {
  id = %q
  ingress {
    action             = "permit"
    protocol           = "tcp"
    source_prefix      = "203.0.113.0/24"
    destination_prefix = "0.0.0.0/0"

    destination_port_range = "22"
  }
  timeouts {
    delete = "40s"
  }
}
`, securityGroupType, group))
	t.Cleanup(func() {
		c.run("destroy", "-auto-approve", "-target", clusterType+".test")
		c.run("destroy", "-auto-approve")
	})
	if out, ok := c.run("apply", "-auto-approve"); !ok {
		t.Fatalf("the apply failed:\n%s", out)
	}
	vpc := "vpc-" + cluster
	if err := postFake("/v1/regions/us-texas-1/projects/demo/networking/vpcs/"+vpc,
		`{"securityGroups":["`+group+`"]}`); err != nil {
		t.Fatal(err)
	}
	awaitFakeState(t, "/v1/regions/us-texas-1/projects/demo/networking/vpcs/"+vpc, "active")

	started := time.Now()
	out, ok := c.run("destroy", "-auto-approve", "-target", securityGroupType+".test")
	if ok {
		t.Fatalf("the delete of a group a VPC holds passed:\n%s", out)
	}
	mustSay(t, "the destroy", out, "Could not delete the security group", "vpc "+vpc)
	if took := time.Since(started); took > 20*time.Second {
		t.Errorf("the destroy took %s, so it waited on a holder it wasn't told about", took)
	}
}

// A plan to bind a VPC that's already known, and has no default group, warns
// before anything is written. A VPC with a default group doesn't.
func TestAccAPlanWarnsBeforeBindingAVPCWithNoDefaultGroup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the CLI sequence runs on unix")
	}
	const warning = "This binding can't be taken off the VPC later"
	for _, tc := range []struct {
		cluster string
		warns   bool
	}{
		{"acc-nodef-plan-no-subnets", true},
		{"acc-nodef-plan-subnets", false},
	} {
		t.Run(tc.cluster, func(t *testing.T) {
			group := tc.cluster + "-filter"
			c := newCLIRun(t)
			c.write(clusterConfig(tc.cluster, 1) + securityGroupConfig(group, false))
			t.Cleanup(func() { c.run("destroy", "-auto-approve") })
			if out, ok := c.run("apply", "-auto-approve"); !ok {
				t.Fatalf("the apply failed:\n%s", out)
			}
			c.write(clusterConfig(tc.cluster, 1) + securityGroupConfig(group, false) + fmt.Sprintf(`
resource %q "test" {
  vpc             = "vpc-%s"
  security_groups = [%s.test.id]
}
`, vpcBindingType, tc.cluster, securityGroupType))
			out, ok := c.run("plan")
			if !ok {
				t.Fatalf("the plan failed:\n%s", out)
			}
			if said := strings.Contains(strings.Join(strings.Fields(out), " "), warning); said != tc.warns {
				t.Errorf("the plan warned %v, want %v:\n%s", said, tc.warns, out)
			}
		})
	}
}
