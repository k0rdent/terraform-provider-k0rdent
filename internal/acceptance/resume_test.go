package acceptance

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/provider"
)

// The fake settles a create, and finishes a cluster's teardown, only after
// many reads when the display name carries these, so a short timeout runs
// out first.
const (
	createsSlowly = "fake-creates-slowly"
	deletesSlowly = "fake-deletes-slowly"
	// The fake settles the create `failed`, a cluster's with reason
	// RESERVATION_FAILED.
	createFails = "fake-create-fails"
)

// slowObject is a cluster or an instance group the fake builds slowly, with
// the given timeouts.
func slowObject(typeName, name, markers, timeouts string) string {
	pool := `"gpu-workers" = { node_count = 2 }`
	if typeName == instanceGroupType {
		pool = `"gpu-workers" = { size = 1 }`
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
    %s
  }
}
`, typeName, name, clusterTypeRef, markers, pool, timeouts)
}

// requestCounter counts one method on one path from the moment it's started,
// since the fake's count runs across every test.
type requestCounter struct {
	method, path string
	before       int
}

func (c *requestCounter) start(t *testing.T) func() {
	return func() {
		n, err := fakeServed(c.method, c.path)
		if err != nil {
			t.Fatal(err)
		}
		c.before = n
	}
}

// once checks the fake served exactly one since start.
func (c *requestCounter) once() resource.TestCheckFunc {
	return func(*terraform.State) error {
		n, err := fakeServed(c.method, c.path)
		if err != nil {
			return err
		}
		if n-c.before != 1 {
			return fmt.Errorf("the fake served %d %s %s, want one", n-c.before, c.method, c.path)
		}
		return nil
	}
}

// cliRun is one working directory driven by the terraform CLI, against a
// build of the provider through dev_overrides, the way a user's shell drives
// it. terraform-plugin-testing has no untaint step, and the sequence that
// needs one runs here.
type cliRun struct {
	t         *testing.T
	terraform string
	dir       string
	rc        string
}

func newCLIRun(t *testing.T) *cliRun {
	t.Helper()
	if os.Getenv("TF_ACC") == "" {
		t.Skip("TF_ACC is not set, so there is no fake to run against")
	}
	c := &cliRun{t: t, terraform: terraformForMigration(t), dir: t.TempDir()}
	c.rc = filepath.Join(c.dir, "terraformrc")
	writeFile(t, c.rc, fmt.Sprintf("provider_installation {\n  dev_overrides {\n    %q = %q\n  }\n  direct {}\n}\n",
		provider.RegistryAddress(), builtProvider(t)))
	return c
}

func (c *cliRun) write(config string) {
	c.t.Helper()
	writeFile(c.t, filepath.Join(c.dir, "main.tf"), fmt.Sprintf(
		"terraform {\n  required_providers {\n    %s = { source = %q }\n  }\n}\n%s",
		provider.ProviderTypeName, provider.RegistryNamespace+"/"+provider.ProviderTypeName, config))
}

// run runs terraform with args and returns what it printed and whether it
// exited 0. The provider's settings come from this process's environment,
// which TestMain pointed at the fake.
func (c *cliRun) run(args ...string) (string, bool) {
	c.t.Helper()
	cmd := exec.Command(c.terraform, append(args, "-no-color")...)
	cmd.Dir = c.dir
	cmd.Env = append(quietEnviron(), "TF_CLI_CONFIG_FILE="+c.rc, "TF_IN_AUTOMATION=1", "TF_INPUT=0",
		"CHECKPOINT_DISABLE=1")
	out, err := cmd.CombinedOutput()
	return string(out), err == nil
}

// mustSay fails unless out, folded to single spaces, says every want.
func mustSay(t *testing.T, step, out string, wants ...string) {
	t.Helper()
	flat := strings.Join(strings.Fields(strings.ReplaceAll(out, "│", " ")), " ")
	for _, want := range wants {
		if !strings.Contains(flat, want) {
			t.Errorf("%s doesn't say %q:\n%s", step, want, out)
		}
	}
}

// The whole sequence a user sees when a cluster's or an instance group's
// create outlives its timeout, through the real CLI:
//
//  1. `terraform apply` fails, exit 1, saying it's still being created and to
//     untaint and apply
//  2. `terraform apply` again, without the untaint, is refused at plan, so
//     nothing is destroyed
//  3. `terraform untaint` and an apply with `timeouts.create` raised, as the
//     error suggests, waits for the same object and finishes it
//
// One create is sent in all, and the destroy at the end cleans up.
func TestAccACreateThatTimesOutIsWaitedForAfterAnUntaint(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the CLI sequence runs on unix")
	}
	for _, tc := range []struct {
		typeName, name, path string
	}{
		{clusterType, "acc-untaint-cluster", "/v1/regions/us-texas-1/projects/demo/compute/clusters"},
		{instanceGroupType, "acc-untaint-group", "/v1/regions/us-texas-1/projects/demo/compute/instance-groups"},
	} {
		t.Run(tc.typeName, func(t *testing.T) {
			c := newCLIRun(t)
			addr := tc.typeName + ".test"
			creates := &requestCounter{method: "POST", path: tc.path}
			creates.start(t)()

			c.write(slowObject(tc.typeName, tc.name, createsSlowly, `create = "1s"`))
			out, ok := c.run("apply", "-auto-approve")
			if ok {
				t.Fatalf("the apply passed while the create is still running:\n%s", out)
			}
			mustSay(t, "the apply", out, "is still being created", "still `creating`",
				"terraform untaint <address> terraform apply", "Don't run `terraform apply` first")
			t.Cleanup(func() {
				if gone, destroyed := c.run("destroy", "-auto-approve"); !destroyed {
					t.Errorf("cleanup destroy failed:\n%s", gone)
				}
			})

			out, ok = c.run("apply", "-auto-approve")
			if ok {
				t.Fatalf("the apply that forgot the untaint went ahead:\n%s", out)
			}
			mustSay(t, "the apply without the untaint", out, "is still being created",
				"terraform untaint <address> terraform apply", "allow_replace = true")
			if strings.Contains(out, "Destroying...") {
				t.Errorf("the refused apply destroyed something:\n%s", out)
			}

			if untainted, done := c.run("untaint", addr); !done {
				t.Fatalf("untaint failed:\n%s", untainted)
			}
			c.write(slowObject(tc.typeName, tc.name, createsSlowly, `create = "2m"`))
			out, ok = c.run("apply", "-auto-approve")
			if !ok {
				t.Fatalf("the apply after the untaint failed:\n%s", out)
			}
			mustSay(t, "the apply after the untaint", out, "0 added, 1 changed, 0 destroyed")
			if shown, ok := c.run("state", "show", addr); !ok || !regexp.MustCompile(`state\s+=\s+"active"`).MatchString(shown) {
				t.Errorf("the object isn't active in state:\n%s", shown)
			}
			out, _ = c.run("plan", "-detailed-exitcode")
			mustSay(t, "the plan after", out, "No changes.")
			if err := creates.once()(nil); err != nil {
				t.Error(err)
			}
		})
	}
}

// The sequence a user sees when the platform fails a cluster's or an
// instance group's create, through the real CLI:
//
//  1. `terraform apply` fails, saying it didn't become active, and Terraform
//     keeps it tainted
//  2. `terraform plan` plans to replace it, not a refusal: nothing is being
//     built
//  3. `terraform destroy` runs without an untaint and leaves state empty
//
// The CLI runs its own build of the provider, which holds a cluster's `failed`
// for the full provisional window before reporting it, so the cluster's run
// takes that long.
func TestAccADestroyRunsPastACreateThePlatformFailed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the CLI sequence runs on unix")
	}
	for _, tc := range []struct {
		typeName, name, path string
	}{
		{clusterType, "acc-failed-cluster", "/v1/regions/us-texas-1/projects/demo/compute/clusters"},
		{instanceGroupType, "acc-failed-group", "/v1/regions/us-texas-1/projects/demo/compute/instance-groups"},
	} {
		t.Run(tc.typeName, func(t *testing.T) {
			c := newCLIRun(t)
			creates := &requestCounter{method: "POST", path: tc.path}
			creates.start(t)()

			c.write(slowObject(tc.typeName, tc.name, createFails, `create = "5m"`))
			out, ok := c.run("apply", "-auto-approve")
			if ok {
				t.Fatalf("the apply passed while the platform failed the create:\n%s", out)
			}
			mustSay(t, "the apply", out, "did not become active", "failed")
			if tc.typeName == clusterType {
				mustSay(t, "the apply", out, "RESERVATION_FAILED")
			}

			out, ok = c.run("plan")
			if !ok {
				t.Fatalf("the plan after the failed create was refused:\n%s", out)
			}
			mustSay(t, "the plan", out, "is tainted, so must be replaced", "1 to add, 0 to change, 1 to destroy",
				"failed, and applying this plan replaces it", "run `terraform destroy` instead")

			out, ok = c.run("destroy", "-auto-approve")
			if !ok {
				t.Fatalf("the destroy after the failed create failed:\n%s", out)
			}
			mustSay(t, "the destroy", out, "1 destroyed")
			if listed, ok := c.run("state", "list"); !ok || strings.TrimSpace(listed) != "" {
				t.Errorf("state isn't empty after the destroy:\n%s", listed)
			}
			if err := creates.once()(nil); err != nil {
				t.Error(err)
			}
		})
	}
}

// A destroy whose timeout runs out before the platform reaps the cluster
// fails and leaves it in state, `deleting`, and says the next destroy picks
// the delete up. That destroy, with the timeout raised, waits for the same
// delete: the fake serves one DELETE in all, and the cluster is gone.
func TestAccADestroyThatTimesOutResumesOnTheNextDestroy(t *testing.T) {
	const name = "acc-resume-destroy"
	deletes := &requestCounter{method: "DELETE", path: "/v1/regions/us-texas-1/projects/demo/compute/clusters/" + name}
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			checkGone(clusterType, clusterPath),
			deletes.once(),
		),
		Steps: []resource.TestStep{
			{PreConfig: deletes.start(t), Config: slowObject(clusterType, name, deletesSlowly, `delete = "1s"`)},
			{
				Config:  slowObject(clusterType, name, deletesSlowly, `delete = "1s"`),
				Destroy: true,
				ExpectError: regexp.MustCompile(`(?s)did not finish deleting.*still\s+` + "`deleting`" +
					`.*` + "`terraform destroy`" + `\s+again.*rather\s+than\s+sending\s+another`),
			},
			{
				// Raising the delete timeout is an apply, since a destroy reads
				// it from state. It sends nothing to a cluster mid-delete.
				Config: slowObject(clusterType, name, deletesSlowly, `delete = "2m"`),
				Check: resource.ComposeAggregateTestCheckFunc(
					checkFakeState(name, "deleting"),
					deletes.once(),
				),
			},
		},
	})
}
