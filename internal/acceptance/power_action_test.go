package acceptance

import (
	"fmt"
	"net/http"
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/provider"
)

// The power action, run by real Terraform against the fake through
// `action_trigger`, as the cancel action's tests run it.

var powerAction = provider.ResourceName("instance_power")

// powerFails, in an instance group's display name, makes the fake report
// `lastPowerActionError` on each of its hosts once a power action has been
// accepted for it, as the platform does when the machine's controller
// refuses the action after the 202.
const powerFails = "fake-power-fails"

// powerTriggered is a one-host instance group whose display name is name,
// and a terraform_data per entry in triggers whose create sends that power
// action to the host. The instances data source reads the host's id, and
// again after every trigger.
func powerTriggered(group, displayName string, triggers ...string) string {
	config := fmt.Sprintf(`
resource %[1]q "test" {
  id           = %[2]q
  cluster_type = %[3]q
  display_name = %[4]q
  node_pool = {
    "gpu-workers" = { size = 1 }
  }
}

data %[5]q "group" {
  instance_group = %[1]s.test.id
  depends_on     = [%[1]s.test]
}
`, instanceGroupType, group, clusterTypeRef, displayName, instancesType)
	for i, verb := range triggers {
		config += fmt.Sprintf(`
action %[1]q "power_%[2]d" {
  config {
    instance_id = data.%[3]s.group.ids[0]
    action      = %[4]q
  }
}

resource "terraform_data" "power_%[2]d" {
  input = data.%[3]s.group.ids[0]
  lifecycle {
    action_trigger {
      events  = [after_create]
      actions = [action.%[1]s.power_%[2]d]
    }
  }
}
`, powerAction, i, instancesType, verb)
	}
	return config
}

// checkPowerSent wants the fake to have served n power requests of verb for
// the host the instances data source read.
func checkPowerSent(verb string, n int) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs, ok := s.RootModule().Resources["data."+instancesType+".group"]
		if !ok {
			return fmt.Errorf("the instances data source isn't in state")
		}
		id := rs.Primary.Attributes["ids.0"]
		got, err := fakeServed(http.MethodPost, "/v1/regions/us-texas-1/projects/demo/compute/instances/"+id+"/"+verb)
		if err != nil {
			return err
		}
		if got != n {
			return fmt.Errorf("the fake served %d %s requests for %s, want %d", got, verb, id, n)
		}
		return nil
	}
}

// One power action is sent and accepted, and a second on the same machine
// inside the platform's guard interval fails with the wait, sending nothing
// more: the provider never sends a power action again on its own.
func TestAccPowerActionIsSentOnceAndRefusedInFlight(t *testing.T) {
	const group = "acc-power"
	resource.Test(t, resource.TestCase{
		TerraformVersionChecks:   actionsVersion,
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy:             checkGone(instanceGroupType, instanceGroupPath),
		Steps: []resource.TestStep{
			{
				Config: powerTriggered(group, "acceptance", "powerCycle"),
				Check: resource.ComposeAggregateTestCheckFunc(
					checkPowerSent("powerCycle", 1),
					// The 202 is all the action waits for: the host reads as
					// it did, and no failure is known.
					resource.TestCheckResourceAttr("data."+instancesType+".group", "instances.0.state", "running"),
				),
			},
			{
				Config:      powerTriggered(group, "acceptance", "powerCycle", "gracefulRestart"),
				ExpectError: regexp.MustCompile(`(?s)POWER_ACTION_IN_FLIGHT.*about [0-9]+ seconds`),
			},
			{
				// The refused trigger's terraform_data was never created, so
				// leaving it in would trigger it again. Without it, the counts
				// show the refused action went out once and wasn't resent.
				Config: powerTriggered(group, "acceptance", "powerCycle"),
				Check: resource.ComposeAggregateTestCheckFunc(
					checkPowerSent("powerCycle", 1),
					checkPowerSent("gracefulRestart", 1),
				),
			},
		},
	})
}

// A failure the platform learns of after the 202 is the instance's
// `last_power_action_error`, which the instance data sources read, while the
// action itself succeeded. The data source reads the host before the action
// is sent, so it's the next refresh that sees it.
func TestAccPowerActionFailureShowsOnTheInstance(t *testing.T) {
	const group = "acc-power-fails"
	list := "data." + instancesType + ".group"
	config := powerTriggered(group, "acceptance "+powerFails, "powerOff")
	resource.Test(t, resource.TestCase{
		TerraformVersionChecks:   actionsVersion,
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy:             checkGone(instanceGroupType, instanceGroupPath),
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					checkPowerSent("powerOff", 1),
					resource.TestCheckNoResourceAttr(list, "instances.0.last_power_action_error"),
				),
			},
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					checkPowerSent("powerOff", 1),
					resource.TestMatchResourceAttr(list, "instances.0.last_power_action_error", regexp.MustCompile(`refused`)),
				),
			},
		},
	})
}
