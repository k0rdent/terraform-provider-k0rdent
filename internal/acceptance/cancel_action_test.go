package acceptance

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/hashicorp/terraform-plugin-testing/tfversion"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/apiaudit"
	"github.com/k0rdent/terraform-provider-k0rdent/internal/provider"
)

// The cancel action, run by real Terraform against the fake. Actions need
// Terraform 1.14, and each test skips itself below that.
//
// terraform-plugin-testing runs an action only from `action_trigger`, which
// HashiCorp's own guide calls the way to test one, so the trigger tests go
// through it. `terraform apply -invoke`, the way a person runs this action,
// has no step mode there, so TestAccCancelActionByInvoke runs the CLI itself.

var cancelAction = provider.ResourceName("cluster_cancel_operation")

// actionsVersion is the first Terraform with actions.
var actionsVersion = []tfversion.TerraformVersionCheck{tfversion.SkipBelow(tfversion.Version1_14_0)}

// heldUpgradeCluster is an active cluster the fake holds mid-upgrade once an
// upgrade is posted for it, and trigger, when set, is the configuration of a
// terraform_data whose create runs the cancel action on it.
func heldUpgradeCluster(name, trigger string) string {
	return fmt.Sprintf(`
resource %q "test" {
  id           = %q
  cluster_type = %q
  display_name = "acceptance fake-holds-upgrade"
  node_pool = {
    "gpu-workers" = { node_count = 1 }
  }
}
%s`, clusterType, name, clusterTypeV2, trigger)
}

// cancelTrigger runs the cancel action, with extra in its config, once the
// terraform_data is created, which is the step that adds it.
func cancelTrigger(extra string) string {
	return fmt.Sprintf(`
action %[1]q "stop" {
  config {
    cluster_id = %[2]s.test.id
%[3]s  }
}

resource "terraform_data" "cancel" {
  input = %[2]s.test.id
  lifecycle {
    action_trigger {
      events  = [after_create]
      actions = [action.%[1]s.stop]
    }
  }
}
`, cancelAction, clusterType, extra)
}

func clusterAPIPath(name string) string {
	return "/v1/regions/us-texas-1/projects/demo/compute/clusters/" + name
}

func holdAnUpgrade(t *testing.T, name string) func() {
	return func() {
		if err := postFake(clusterAPIPath(name)+"/upgrade", `{}`); err != nil {
			t.Fatal(err)
		}
	}
}

// checkFakeOperation reads the cluster's operation from the fake, past the
// provider, and wants its status.
func checkFakeOperation(name, want string) resource.TestCheckFunc {
	return func(*terraform.State) error {
		status, body, err := readFake(clusterAPIPath(name))
		if err != nil {
			return err
		}
		if status != http.StatusOK {
			return fmt.Errorf("reading %s answered %d", name, status)
		}
		var got struct {
			Operation struct {
				Status string `json:"status"`
			} `json:"operation"`
		}
		if err := json.Unmarshal(body, &got); err != nil {
			return err
		}
		if got.Operation.Status != want {
			return fmt.Errorf("%s's operation reads %q, want %q", name, got.Operation.Status, want)
		}
		return nil
	}
}

// An upgrade held mid-flight is cancelled by the action and the cluster is
// kept: the fake reads it `failed` with its operation `cancelled`, the
// configuration then plans clean, and the destroy at the end deletes the
// `failed` cluster without cancelling anything.
func TestAccCancelActionStopsAnUpgradeAndKeepsTheCluster(t *testing.T) {
	const name = "acc-cancel-action"
	resource.Test(t, resource.TestCase{
		TerraformVersionChecks:   actionsVersion,
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy:             checkGone(clusterType, clusterPath),
		Steps: []resource.TestStep{
			{
				Config: heldUpgradeCluster(name, ""),
				Check:  checkFakeState(name, "active"),
			},
			{
				PreConfig: holdAnUpgrade(t, name),
				Config:    heldUpgradeCluster(name, cancelTrigger("")),
				Check: resource.ComposeAggregateTestCheckFunc(
					checkFakeState(name, "failed"),
					checkFakeOperation(name, "cancelled"),
					resource.TestCheckResourceAttr(clusterAddr, "id", name),
				),
			},
			{
				// The refresh reads the cluster the cancel left.
				Config: heldUpgradeCluster(name, cancelTrigger("")),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(clusterAddr, "state", "failed"),
					checkFakeState(name, "failed"),
				),
			},
		},
	})
}

// A cluster with nothing in flight has nothing to cancel, and the action
// succeeds without touching it. Nothing to cancel looks the same from outside
// as an action that never ran, so the cluster is let go of with a removed
// block first: every provider read of it in that step is then the action's,
// and the step wants at least one and no cancel. The last step adopts it
// again, so the destroy deletes it.
func TestAccCancelActionWithNothingToCancelSucceeds(t *testing.T) {
	const name = "acc-cancel-idle"
	var mark int64
	resource.Test(t, resource.TestCase{
		TerraformVersionChecks:   actionsVersion,
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy:             checkGone(clusterType, clusterPath),
		Steps: []resource.TestStep{
			{
				Config: heldUpgradeCluster(name, ""),
			},
			{
				PreConfig: func() { mark = fakeLogSize(t) },
				Config: fmt.Sprintf(`
removed {
  from = %[1]s.test
  lifecycle { destroy = false }
}

action %[2]q "stop" {
  config {
    cluster_id = %[3]q
  }
}

resource "terraform_data" "cancel" {
  input = %[3]q
  lifecycle {
    action_trigger {
      events  = [after_create]
      actions = [action.%[2]s.stop]
    }
  }
}
`, clusterType, cancelAction, name),
				Check: resource.ComposeAggregateTestCheckFunc(
					checkFakeState(name, "active"),
					checkFakeOperation(name, "succeeded"),
					func(*terraform.State) error {
						reads, cancels := providerRequestsSince(t, mark, clusterAPIPath(name))
						if reads == 0 {
							return fmt.Errorf("the provider never read %s, so the action never ran", name)
						}
						if cancels != 0 {
							return fmt.Errorf("the provider sent %d cancels to an idle cluster", cancels)
						}
						return nil
					},
				),
			},
			{
				Config: heldUpgradeCluster(name, "") + fmt.Sprintf(`
import {
  to = %s.test
  id = %q
}
`, clusterType, name),
			},
		},
	})
}

func fakeLogSize(t *testing.T) int64 {
	t.Helper()
	info, err := os.Stat(fakeLogPath)
	if err != nil {
		t.Fatal(err)
	}
	return info.Size()
}

// providerRequestsSince counts the provider's reads of the cluster at path,
// and its cancels of it, in the fake's log from offset on. The harness's own
// requests carry its user agent and aren't counted.
func providerRequestsSince(t *testing.T, offset int64, path string) (reads, cancels int) {
	t.Helper()
	body, err := os.ReadFile(fakeLogPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(body[offset:]), "\n") {
		if strings.Contains(line, `ua="`+apiaudit.HarnessUserAgent+`"`) {
			continue
		}
		switch {
		case strings.Contains(line, " GET "+path+" "), strings.Contains(line, " GET "+path+"?"):
			reads++
		case strings.Contains(line, " POST "+path+"/cancel "):
			cancels++
		}
	}
	return reads, cancels
}

// Pinned to an operation the cluster isn't running, the action refuses and
// cancels nothing. The held upgrade is still running afterwards, and the
// destroy at the end is what cancels it.
func TestAccCancelActionPinnedToAnotherOperationCancelsNothing(t *testing.T) {
	const name = "acc-cancel-pinned"
	resource.Test(t, resource.TestCase{
		TerraformVersionChecks:   actionsVersion,
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy:             checkGone(clusterType, clusterPath),
		Steps: []resource.TestStep{
			{
				Config: heldUpgradeCluster(name, ""),
			},
			{
				PreConfig:   holdAnUpgrade(t, name),
				Config:      heldUpgradeCluster(name, cancelTrigger("    execution_id = \"not-the-one-running\"\n")),
				ExpectError: regexp.MustCompile(`(?s)not-the-one-running.*Nothing\s+was\s+cancelled`),
			},
			{
				// No trigger, so nothing runs the action again.
				Config: heldUpgradeCluster(name, ""),
				Check: resource.ComposeAggregateTestCheckFunc(
					checkFakeState(name, "updating"),
					checkFakeOperation(name, "running"),
				),
			},
		},
	})
}

// The action as a person runs it: `terraform apply -invoke`, in a working
// directory of its own that holds only the action, against a cluster this
// test's other configuration holds mid-upgrade. The plan names the operation
// before the apply cancels it, the apply streams its progress, and the fake
// reads the cluster `failed` and `cancelled` afterwards.
func TestAccCancelActionByInvoke(t *testing.T) {
	const name = "acc-cancel-invoke"
	resource.Test(t, resource.TestCase{
		TerraformVersionChecks:   actionsVersion,
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy:             checkGone(clusterType, clusterPath),
		Steps: []resource.TestStep{
			{
				Config: heldUpgradeCluster(name, ""),
			},
			{
				PreConfig: func() {
					holdAnUpgrade(t, name)()
					out := invokeByCLI(t, name)
					// Terraform wraps a diagnostic's detail to the terminal, so
					// the words are compared with the line breaks taken out.
					flat := strings.Join(strings.Fields(out), " ")
					for _, want := range []string{
						// The plan's warning. The action here is unpinned, so it
						// says the invoke cancels whatever runs, and how to pin.
						"Invoking this cancels whatever cluster " + name + " is running",
						"set `execution_id = \"" + name + "-op-",
						"Cancelling operation", // progress, streamed by the apply
						"Cancelled operation",
					} {
						if !strings.Contains(flat, want) {
							t.Errorf("terraform apply -invoke never said %q:\n%s", want, out)
						}
					}
				},
				Config: heldUpgradeCluster(name, ""),
				Check: resource.ComposeAggregateTestCheckFunc(
					checkFakeState(name, "failed"),
					checkFakeOperation(name, "cancelled"),
				),
			},
		},
	})
}

// invokeByCLI builds the provider, points a CLI configuration's dev_overrides
// at it, and runs `terraform apply -invoke` on a configuration holding only
// the action. Under dev_overrides there's nothing to initialise. The child
// inherits this suite's endpoint, credentials and scope from the environment,
// and writes its trace to the log gate S4 searches.
func invokeByCLI(t *testing.T, cluster string) string {
	t.Helper()
	// The test case's version check has already skipped below 1.14 for this
	// same binary by the time a step's PreConfig runs.
	tf := terraformForMigration(t)
	bin := builtProvider(t)
	dir := t.TempDir()
	cliConfig := filepath.Join(dir, "cli.tfrc")
	writeFile(t, cliConfig, fmt.Sprintf("provider_installation {\n  dev_overrides {\n    %q = %q\n  }\n  direct {}\n}\n",
		provider.RegistryAddress(), bin))
	work := filepath.Join(dir, "work")
	if err := os.Mkdir(work, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(work, "main.tf"), fmt.Sprintf(`
terraform {
  required_providers {
    %[1]s = { source = %[2]q }
  }
}

action %[3]q "stop" {
  config {
    cluster_id = %[4]q
  }
}
`, provider.ProviderTypeName, provider.RegistryNamespace+"/"+provider.ProviderTypeName, cancelAction, cluster))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, tf, "apply", "-invoke=action."+cancelAction+".stop", "-auto-approve", "-no-color", "-input=false")
	cmd.Dir = work
	cmd.Env = append(os.Environ(), "TF_CLI_CONFIG_FILE="+cliConfig, "TF_IN_AUTOMATION=1")
	if logPath := os.Getenv("TF_ACC_LOG_PATH"); logPath != "" {
		cmd.Env = append(cmd.Env, "TF_LOG_PATH="+logPath)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("terraform apply -invoke: %v\n%s", err, out)
	}
	return string(out)
}

// The count the idle test rests on: only the provider's own requests to that
// cluster count, from the offset on, so an action that never ran reads zero.
func TestProviderRequestsSinceCountsOnlyTheProvidersLaterRequests(t *testing.T) {
	saved := fakeLogPath
	t.Cleanup(func() { fakeLogPath = saved })
	fakeLogPath = filepath.Join(t.TempDir(), "fake.log")
	path := clusterAPIPath("acc-cancel-idle")
	before := "2026/10/12 12:00:00 GET " + path + ` ua="terraform-provider-k0rdent/acc" auth=present` + "\n"
	writeFile(t, fakeLogPath, before+
		"2026/10/12 12:00:01 GET "+path+` ua="`+apiaudit.HarnessUserAgent+`" auth=present`+"\n"+
		"2026/10/12 12:00:02 GET "+path+"-other"+` ua="terraform-provider-k0rdent/acc" auth=present`+"\n")
	if reads, cancels := providerRequestsSince(t, int64(len(before)), path); reads != 0 || cancels != 0 {
		t.Errorf("counted %d reads and %d cancels in a log with none of the provider's after the mark", reads, cancels)
	}
	writeFile(t, fakeLogPath, before+
		"2026/10/12 12:00:03 GET "+path+` ua="terraform-provider-k0rdent/acc" auth=present`+"\n"+
		"2026/10/12 12:00:04 POST "+path+`/cancel ua="terraform-provider-k0rdent/acc" auth=present`+"\n")
	if reads, cancels := providerRequestsSince(t, int64(len(before)), path); reads != 1 || cancels != 1 {
		t.Errorf("counted %d reads and %d cancels, want 1 and 1", reads, cancels)
	}
}
