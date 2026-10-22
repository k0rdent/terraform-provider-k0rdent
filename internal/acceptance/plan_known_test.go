package acceptance

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/knownvalue"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/provider"
)

// onlyChanges is a plan check that an update to addr moves exactly the named
// top-level attributes, a value going unknown included, and nothing else.
type onlyChanges struct {
	addr  string
	names []string
}

// expectOnly fails a plan that shows any other attribute of addr moving, or
// "known after apply", or that leaves one of the named ones as it was.
func expectOnly(addr string, names ...string) plancheck.PlanCheck {
	return onlyChanges{addr: addr, names: names}
}

func (c onlyChanges) CheckPlan(_ context.Context, req plancheck.CheckPlanRequest, resp *plancheck.CheckPlanResponse) {
	for _, rc := range req.Plan.ResourceChanges {
		if rc.Address != c.addr {
			continue
		}
		before, okBefore := rc.Change.Before.(map[string]any)
		after, okAfter := rc.Change.After.(map[string]any)
		unknown, okUnknown := rc.Change.AfterUnknown.(map[string]any)
		if !okBefore || !okAfter || !okUnknown {
			resp.Error = fmt.Errorf("%s doesn't plan an update: before %T, after %T, after_unknown %T",
				c.addr, rc.Change.Before, rc.Change.After, rc.Change.AfterUnknown)
			return
		}
		keys := map[string]bool{}
		for _, m := range []map[string]any{before, after, unknown} {
			for k := range m {
				keys[k] = true
			}
		}
		var moved []string
		for k := range keys {
			if anyUnknown(unknown[k]) || !reflect.DeepEqual(before[k], after[k]) {
				moved = append(moved, k)
			}
		}
		sort.Strings(moved)
		var extra, still []string
		for _, k := range moved {
			if !slices.Contains(c.names, k) {
				extra = append(extra, k)
			}
		}
		for _, k := range c.names {
			if !slices.Contains(moved, k) {
				still = append(still, k)
			}
		}
		if len(extra) > 0 || len(still) > 0 {
			resp.Error = fmt.Errorf("%s plans %v moving, want %v (unexpected: %v, unmoved: %v)",
				c.addr, moved, c.names, extra, still)
		}
		return
	}
	resp.Error = fmt.Errorf("%s is not in the plan", c.addr)
}

// anyUnknown reads one entry of a plan's after_unknown, which is true, or a
// structure of them for a nested value.
func anyUnknown(v any) bool {
	switch v := v.(type) {
	case bool:
		return v
	case map[string]any:
		for _, e := range v {
			if anyUnknown(e) {
				return true
			}
		}
	case []any:
		for _, e := range v {
			if anyUnknown(e) {
				return true
			}
		}
	}
	return false
}

func updates(addr string, also ...plancheck.PlanCheck) resource.ConfigPlanChecks {
	return resource.ConfigPlanChecks{PreApply: append([]plancheck.PlanCheck{
		plancheck.ExpectResourceAction(addr, plancheck.ResourceActionUpdate),
	}, also...)}
}

func known(addr, attr string, v knownvalue.Check) plancheck.PlanCheck {
	return plancheck.ExpectKnownValue(addr, tfjsonpath.New(attr), v)
}

func unknownAfterApply(addr, attr string) plancheck.PlanCheck {
	return plancheck.ExpectUnknownValue(addr, tfjsonpath.New(attr))
}

// renamed is a configuration with its first display name changed.
func renamed(config string) string {
	return strings.Replace(config, `display_name = "acceptance"`, `display_name = "renamed"`, 1)
}

// An in-place change plans what it changes and nothing else: the state the
// update settles back into, the ids, counts and owners it can't move. Each
// step applies the plan it checks, so a value planned here that the apply then
// contradicts fails the step as an inconsistent result.
func TestAccNetworkRenamePlansOnlyTheName(t *testing.T) {
	const name = "acc-known-net"
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy:             checkGone(networkType, networkPath),
		Steps: []resource.TestStep{
			{Config: evpnConfig(name, "acceptance")},
			{
				Config: evpnConfig(name, "renamed"),
				ConfigPlanChecks: updates(networkAddr,
					expectOnly(networkAddr, "display_name"),
					known(networkAddr, "state", knownvalue.StringExact("active")),
					known(networkAddr, "cluster_id", knownvalue.Null()),
					known(networkAddr, "vpc", knownvalue.Null()),
					known(networkAddr, "self_link", knownvalue.NotNull()),
				),
				Check: resource.TestCheckResourceAttr(networkAddr, "state", "active"),
			},
		},
	})
}

func TestAccSecurityGroupChangePlansOnlyWhatMoves(t *testing.T) {
	const name = "acc-known-sg"
	three := securityGroupConfig(name, true)
	renamedNewPort := strings.Replace(renamed(three), `destination_port_range = "9090"`, `destination_port_range = "9091"`, 1)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy:             checkGone(securityGroupType, securityGroupPath),
		Steps: []resource.TestStep{
			{Config: three},
			{
				// A rename and one rule's port: the count stays known.
				Config: renamedNewPort,
				ConfigPlanChecks: updates(securityGroupAddr,
					expectOnly(securityGroupAddr, "display_name", "ingress"),
					known(securityGroupAddr, "rule_count", knownvalue.Int64Exact(3)),
					known(securityGroupAddr, "state", knownvalue.StringExact("active")),
					known(securityGroupAddr, "owner_kind", knownvalue.Null()),
				),
				Check: resource.TestCheckResourceAttr(securityGroupAddr, "rule_count", "3"),
			},
			{
				// A rule removed: the plan counts what's left.
				Config: renamed(securityGroupConfig(name, false)),
				ConfigPlanChecks: updates(securityGroupAddr,
					expectOnly(securityGroupAddr, "ingress", "rule_count"),
					known(securityGroupAddr, "rule_count", knownvalue.Int64Exact(2)),
					known(securityGroupAddr, "state", knownvalue.StringExact("active")),
				),
				Check: resource.TestCheckResourceAttr(securityGroupAddr, "rule_count", "2"),
			},
		},
	})
}

func TestAccClusterRenamePlansOnlyTheName(t *testing.T) {
	const name = "acc-known-cluster"
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy:             checkGone(clusterType, clusterPath),
		Steps: []resource.TestStep{
			{Config: clusterConfig(name, 2)},
			{
				Config: renamed(clusterConfig(name, 2)),
				ConfigPlanChecks: updates(clusterAddr,
					expectOnly(clusterAddr, "display_name"),
					known(clusterAddr, "state", knownvalue.StringExact("active")),
					known(clusterAddr, "node_count", knownvalue.NotNull()),
					known(clusterAddr, "vpc", knownvalue.NotNull()),
				),
			},
			{
				// A scale does move the total, and says so.
				Config: renamed(clusterConfig(name, 3)),
				ConfigPlanChecks: updates(clusterAddr,
					expectOnly(clusterAddr, "node_pool", "node_count"),
					unknownAfterApply(clusterAddr, "node_count"),
					known(clusterAddr, "state", knownvalue.StringExact("active")),
				),
			},
		},
	})
}

func TestAccInstanceGroupRenamePlansOnlyTheName(t *testing.T) {
	const name = "acc-known-hosts"
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy:             checkGone(instanceGroupType, instanceGroupPath),
		Steps: []resource.TestStep{
			{Config: instanceGroupConfig(name, 2)},
			{
				Config: renamed(instanceGroupConfig(name, 2)),
				ConfigPlanChecks: updates(instanceGroupAddr,
					expectOnly(instanceGroupAddr, "display_name"),
					known(instanceGroupAddr, "state", knownvalue.StringExact("active")),
				),
			},
		},
	})
}

func TestAccFileStoreRenamePlansOnlyTheNameAndRevision(t *testing.T) {
	const name = "acc-known-store"
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy:             checkGone(fileStoreType, fileStorePath),
		Steps: []resource.TestStep{
			{Config: fileStoreConfig(name, "acceptance", 100, oneConsumer)},
			{
				// Every write moves the revision, so etag is the one unknown.
				Config: fileStoreConfig(name, "renamed", 100, oneConsumer),
				ConfigPlanChecks: updates(fileStoreAddr,
					expectOnly(fileStoreAddr, "display_name", "etag"),
					unknownAfterApply(fileStoreAddr, "etag"),
					known(fileStoreAddr, "state", knownvalue.StringExact("active")),
					known(fileStoreAddr, "consumer_status", knownvalue.MapSizeExact(1)),
					known(fileStoreAddr, "preferred_export_location", knownvalue.NotNull()),
				),
			},
			{
				// An expansion reconciles the store, which can move its grants
				// and where it's mounted from.
				Config: fileStoreConfig(name, "renamed", 200, oneConsumer),
				ConfigPlanChecks: updates(fileStoreAddr,
					expectOnly(fileStoreAddr, "capacity_gib", "etag", "consumer_status", "export_locations",
						"preferred_export_location"),
					known(fileStoreAddr, "state", knownvalue.StringExact("active")),
				),
			},
		},
	})
}

func TestAccSubnetTimeoutsPlanOnlyTheTimeouts(t *testing.T) {
	const network, name = "acc-known-subnet-net", "acc-known-subnet"
	base := subnetConfig(network, name)
	withTimeouts := strings.Replace(base, `exclude_ranges   = ["10.0.0.10-10.0.0.20"]`,
		`exclude_ranges   = ["10.0.0.10-10.0.0.20"]
  timeouts { create = "30m" }`, 1)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			checkGone(subnetType, subnetPath),
			checkGone(networkType, networkPath),
		),
		Steps: []resource.TestStep{
			{Config: base},
			{
				Config: withTimeouts,
				ConfigPlanChecks: updates(subnetAddr,
					expectOnly(subnetAddr, "timeouts"),
					known(subnetAddr, "state", knownvalue.StringExact("active")),
				),
			},
		},
	})
}

func TestAccPeeringTimeoutsPlanOnlyTheTimeouts(t *testing.T) {
	const cluster, name = "acc-known-peer-owner", "acc-known-peer"
	const remote = "/v1/regions/us-texas-1/projects/other/networking/vpcs/vpc-inference"
	base := peeringConfig(cluster, name, remote)
	withTimeouts := strings.Replace(base, "  depends_on   = [", "  timeouts { create = \"30m\" }\n  depends_on   = [", 1)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			checkGone(peeringType, peeringPath),
			checkGone(clusterType, clusterPath),
		),
		Steps: []resource.TestStep{
			{Config: base},
			{
				Config: withTimeouts,
				ConfigPlanChecks: updates(peeringAddr,
					expectOnly(peeringAddr, "timeouts"),
					known(peeringAddr, "state", knownvalue.StringExact("active")),
				),
			},
		},
	})
}

func TestAccVPCBindingChangePlansWhatIsAttached(t *testing.T) {
	const cluster, group = "acc-known-bind", "acc-known-bind-sg"
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy:             checkGone(clusterType, clusterPath),
		Steps: []resource.TestStep{
			{Config: vpcBindingConfig(cluster, group, false)},
			{
				// The groups sent are the groups the platform reports.
				Config: vpcBindingConfig(cluster, group, true),
				ConfigPlanChecks: updates(vpcBindingAddr,
					expectOnly(vpcBindingAddr, "security_groups", "attached"),
					known(vpcBindingAddr, "attached", knownvalue.ListExact([]knownvalue.Check{knownvalue.StringExact(group)})),
					known(vpcBindingAddr, "state", knownvalue.StringExact("active")),
				),
			},
			{
				// Back to the default group, which only a read can name.
				Config: vpcBindingConfig(cluster, group, false),
				ConfigPlanChecks: updates(vpcBindingAddr,
					expectOnly(vpcBindingAddr, "security_groups", "attached"),
					unknownAfterApply(vpcBindingAddr, "attached"),
					known(vpcBindingAddr, "state", knownvalue.StringExact("active")),
				),
			},
		},
	})
}

func TestAccProjectRenamePlansOnlyTheName(t *testing.T) {
	const id = "acc-known-project"
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy:             checkGone(projectType, projectPath),
		Steps: []resource.TestStep{
			{Config: projectConfig(id, "acceptance", "plan known")},
			{
				Config: projectConfig(id, "renamed", "plan known"),
				ConfigPlanChecks: updates(projectAddr,
					expectOnly(projectAddr, "display_name"),
					known(projectAddr, "status", knownvalue.StringExact("active")),
				),
			},
		},
	})
}

func TestAccWebhookSubscriptionPlansOnlyWhatMoves(t *testing.T) {
	url := webhookURL("plan-known")
	events := []string{"io.k0rdent.cluster.ready.v1"}
	base := webhookConfigFixedSecret(url, events, "known")
	cutover := strings.TrimSuffix(base, "}\n") + "  rotation_overlap = false\n}\n"
	rotated := strings.Replace(strings.Replace(cutover, `sha256("known")`, `sha256("rotated")`, 1),
		"secret_wo_version = 1", "secret_wo_version = 2", 1)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy:             checkWebhookGone,
		Steps: []resource.TestStep{
			{Config: base},
			{
				// The overlap applies to the next rotation, and sends nothing.
				Config: cutover,
				ConfigPlanChecks: updates(webhookAddr,
					expectOnly(webhookAddr, "rotation_overlap"),
					known(webhookAddr, "secret_version", knownvalue.Int64Exact(1)),
					known(webhookAddr, "is_active", knownvalue.Bool(true)),
					known(webhookAddr, "rotated_at", knownvalue.Null()),
					known(webhookAddr, "previous_secret_expires_at", knownvalue.Null()),
				),
			},
			{
				// A rotation moves everything the platform counts and stamps.
				Config: rotated,
				ConfigPlanChecks: updates(webhookAddr,
					expectOnly(webhookAddr, "secret_wo_version", "secret_version", "is_active", "rotated_at",
						"previous_secret_expires_at"),
					unknownAfterApply(webhookAddr, "secret_version"),
				),
				Check: resource.TestCheckResourceAttr(webhookAddr, "secret_version", "2"),
			},
		},
	})
}

// peeredOwnersConfig is a cluster and an instance group, peered both ways
// the way examples/walkthrough/full does it with every switch on, each
// VPC also read through the data source as full/ reads it. remote names the
// far side of the cluster's peering.
func peeredOwnersConfig(cluster, group, remote string) string {
	vpcData := provider.ResourceName("vpc")
	return clusterConfig(cluster, 1) + instanceGroupConfig(group, 1) + fmt.Sprintf(`
locals {
  owners = {
    cluster = %[1]s.test.vpc
    hosts   = %[2]s.test.vpc
  }
}

data %[3]q "cluster" {
  id = local.owners.cluster
}

data %[3]q "hosts" {
  id = local.owners.hosts
}

resource %[4]q "to_hosts" {
  id           = "%[5]s-to-hosts"
  vpc          = local.owners.cluster
  remote_vpc   = %[6]s
  display_name = "to hosts"
}

resource %[4]q "to_cluster" {
  id           = "%[5]s-to-cluster"
  vpc          = local.owners.hosts
  remote_vpc   = local.owners.cluster
  display_name = "to cluster"
}
`, clusterType, instanceGroupType, vpcData, peeringType, cluster, remote)
}

// Renaming a peered cluster replaces no peering. A peering that names the far
// VPC by id keeps a value known at plan while the cluster changes, where a
// self_link read through the data source would be read during the apply and
// replace it. Moving between the id and the self_link of the same VPC then
// replaces nothing either.
func TestAccRenamingAPeeredClusterReplacesNoPeering(t *testing.T) {
	const cluster, group = "acc-peered-cluster", "acc-peered-hosts"
	byID := peeredOwnersConfig(cluster, group, "local.owners.hosts")
	bySelfLink := peeredOwnersConfig(cluster, group, "data."+provider.ResourceName("vpc")+".hosts.self_link")
	to := func(name string) string { return peeringType + "." + name }
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			checkGone(peeringType, peeringPath),
			checkGone(instanceGroupType, instanceGroupPath),
			checkGone(clusterType, clusterPath),
		),
		Steps: []resource.TestStep{
			{
				Config: byID,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(to("to_hosts"), "remote_vpc", "vpc-"+group),
					resource.TestCheckResourceAttr(to("to_hosts"), "state", "active"),
					resource.TestCheckResourceAttr(to("to_cluster"), "remote_vpc", "vpc-"+cluster),
				),
			},
			{
				Config: renamed(byID),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(clusterAddr, plancheck.ResourceActionUpdate),
					plancheck.ExpectResourceAction(to("to_hosts"), plancheck.ResourceActionNoop),
					plancheck.ExpectResourceAction(to("to_cluster"), plancheck.ResourceActionNoop),
					known(to("to_hosts"), "remote_vpc", knownvalue.StringExact("vpc-"+group)),
					known(to("to_cluster"), "remote_vpc", knownvalue.StringExact("vpc-"+cluster)),
				}},
			},
			{
				// The same VPC through its self_link: an update that sends
				// nothing, not a replacement.
				Config: renamed(bySelfLink),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(to("to_hosts"), plancheck.ResourceActionUpdate),
					plancheck.ExpectResourceAction(to("to_cluster"), plancheck.ResourceActionNoop),
				}},
				Check: resource.TestCheckResourceAttr(to("to_hosts"), "remote_vpc",
					"/v1/regions/us-texas-1/projects/demo/networking/vpcs/vpc-"+group),
			},
			{
				// And back to the id.
				Config: byID,
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(clusterAddr, plancheck.ResourceActionUpdate),
					plancheck.ExpectResourceAction(to("to_hosts"), plancheck.ResourceActionUpdate),
					plancheck.ExpectResourceAction(to("to_cluster"), plancheck.ResourceActionNoop),
				}},
			},
			{Config: byID, PlanOnly: true},
		},
	})
}
