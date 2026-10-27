package acceptance

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/provider"
)

var (
	networkType = provider.ResourceName("network")
	subnetType  = provider.ResourceName("subnet")
	vpcsType    = provider.ResourceName("vpcs")
	networkAddr = networkType + ".test"
	subnetAddr  = subnetType + ".test"
	vpcsAddr    = "data." + vpcsType + ".all"
)

func evpnConfig(name, displayName string) string {
	return fmt.Sprintf(`
resource %q "test" {
  id           = %q
  display_name = %q
  tags         = { team = "ml" }
  evpn = {
    service_type = "l3vpn"
    vlan_id      = "auto"
    mtu          = 9000
  }
}
`, networkType, name, displayName)
}

func subnetConfig(network, name string) string {
	return evpnConfig(network, "acceptance") + fmt.Sprintf(`
resource %q "test" {
  id               = %q
  network          = %s.test.id
  address_pool     = "/v1/regions/us-texas-1/networking/address-pools/tenant"
  prefix_length    = 24
  allocate_gateway = true
  exclude_ranges   = ["10.0.0.10-10.0.0.20"]
}
`, subnetType, name, networkType)
}

// The VLAN is the point of this test. `vlan_id = "auto"` is answered with a
// concrete id, so a provider that wrote the response back into the configured
// attribute would propose replacing the network on the second plan. The
// PlanOnly step is what catches that.
func TestAccNetworkEVPN(t *testing.T) {
	const name = "acc-fabric"
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy:             checkGone(networkType, networkPath),
		Steps: []resource.TestStep{
			{
				Config: evpnConfig(name, "acceptance"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(networkAddr, "id", name),
					resource.TestCheckResourceAttr(networkAddr, "type", "evpn"),
					resource.TestCheckResourceAttr(networkAddr, "state", "active"),
					resource.TestCheckResourceAttr(networkAddr, "region", "us-texas-1"),
					resource.TestCheckResourceAttr(networkAddr, "project", "demo"),
					resource.TestCheckResourceAttr(networkAddr, "evpn.vlan_id", "auto"),
					resource.TestCheckResourceAttr(networkAddr, "evpn.vlan_id_resolved", "4001"),
				),
			},
			{
				Config:   evpnConfig(name, "acceptance"),
				PlanOnly: true,
			},
			{
				Config: evpnConfig(name, "renamed"), // in place: one of three mutable fields
				Check: resource.TestCheckResourceAttr(
					networkAddr, "display_name", "renamed"),
			},
			{
				Config:            evpnConfig(name, "renamed"),
				ResourceName:      networkAddr,
				ImportState:       true,
				ImportStateId:     "us-texas-1/demo/" + name,
				ImportStateVerify: true,
				// An import has no configured vlan_id, so it adopts the one the
				// platform resolved "auto" to. Every other attribute, including
				// vlan_id_resolved, has to match.
				ImportStateVerifyIgnore: []string{"evpn.vlan_id"},
			},
		},
	})
}

func TestAccNetworkInfiniBand(t *testing.T) {
	const name = "acc-ib"
	config := fmt.Sprintf(`
resource %q "test" {
  id = %q
  infiniband = {
    partition = "training"
  }
}
`, networkType, name)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy:             checkGone(networkType, networkPath),
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(networkAddr, "type", "infiniband"),
					// Allocated by the platform, not asked for. It has to land
					// somewhere other than `pkey`, or the next plan replaces.
					resource.TestCheckResourceAttr(networkAddr, "infiniband.pkey_allocated", "0x8001"),
				),
			},
			{
				Config:   config,
				PlanOnly: true,
			},
		},
	})
}

// A subnet is create-and-destroy only, so the lifecycle worth covering is the
// CIDR arriving late and `exclude_ranges` never coming back.
func TestAccSubnet(t *testing.T) {
	const network, name = "acc-subnet-net", "acc-pods"
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			checkGone(subnetType, subnetPath),
			checkGone(networkType, networkPath),
		),
		Steps: []resource.TestStep{
			{
				Config: subnetConfig(network, name),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(subnetAddr, "id", name),
					resource.TestCheckResourceAttr(subnetAddr, "state", "active"),
					resource.TestCheckResourceAttrSet(subnetAddr, "cidr"),
					resource.TestCheckResourceAttrSet(subnetAddr, "gateway"),
					resource.TestCheckResourceAttr(subnetAddr, "exclude_ranges.0", "10.0.0.10-10.0.0.20"),
				),
			},
			{
				Config:   subnetConfig(network, name),
				PlanOnly: true,
			},
			{
				Config:            subnetConfig(network, name),
				ResourceName:      subnetAddr,
				ImportState:       true,
				ImportStateId:     "us-texas-1/demo/" + network + "/" + name,
				ImportStateVerify: true,
			},
		},
	})
}

// The platform answers an unset `displayName` or `description` as "", not by
// leaving it out. To a configuration that never set either, that has to stay
// null, or the first apply fails as an inconsistent result and taints a subnet
// that exists.
func TestAccSubnetWithoutMetadataStaysNull(t *testing.T) {
	const network, name = "acc-nometa-net", "acc-nometa"
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			checkGone(subnetType, subnetPath),
			checkGone(networkType, networkPath),
		),
		Steps: []resource.TestStep{
			{
				Config: subnetWithoutRangesConfig(network, name, ""),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckNoResourceAttr(subnetAddr, "display_name"),
					resource.TestCheckNoResourceAttr(subnetAddr, "description"),
					resource.TestCheckNoResourceAttr(networkAddr, "description"),
				),
			},
			{Config: subnetWithoutRangesConfig(network, name, ""), PlanOnly: true},
		},
	})
}

// A VPC has no create operation: it exists because a cluster brought it into
// being. So the data source is exercised against a cluster, not on its own.
func TestAccVPCsDataSource(t *testing.T) {
	const name = "acc-vpc-owner"
	// `depends_on` is load-bearing, not decoration. Nothing in this data
	// source's arguments refers to the cluster, so without it Terraform reads
	// the collection during plan, before the workflow that materializes the
	// VPC has run, and the list comes back empty.
	config := clusterConfig(name, 1) + fmt.Sprintf(`
data %q "all" {
  depends_on = [%s.test]
}
`, vpcsType, clusterType)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy:             checkGone(clusterType, clusterPath),
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(vpcsAddr, "vpcs.0.id", "vpc-"+name),
					resource.TestCheckResourceAttr(vpcsAddr, "vpcs.0.owner_kind", "cluster"),
					resource.TestCheckResourceAttr(vpcsAddr, "vpcs.0.backend", "nico"),
					// The default binding is always reported, never an empty list.
					resource.TestCheckResourceAttr(vpcsAddr, "vpcs.0.security_groups.0", "vpc-"+name+"-default"),
				),
			},
		},
	})
}

// A peering or binding names the cluster's VPC through `vpc`, which stays known
// while the cluster changes. Through a data source it would go unknown on every
// cluster change and replace the binding.
func TestAccClusterVPCSurvivesAClusterChange(t *testing.T) {
	const name = "acc-vpc-stable"
	config := func(nodes int) string {
		return clusterConfig(name, nodes) + fmt.Sprintf(`
resource %q "test" {
  vpc             = %s.test.vpc
  security_groups = []
}
`, vpcBindingType, clusterType)
	}
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy:             checkGone(clusterType, clusterPath),
		Steps: []resource.TestStep{
			{
				Config: config(1),
				Check:  resource.TestCheckResourceAttr(clusterType+".test", "vpc", "vpc-"+name),
			},
			{
				Config: config(2),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(vpcBindingAddr, plancheck.ResourceActionNoop),
				}},
			},
		},
	})
}

// adopt stops managing the resource at <type>.seed without destroying it and
// imports the same object as <type>.test.
func adopt(config, resourceType, importID string) string {
	return config + fmt.Sprintf(`
removed {
  from = %s.seed
  lifecycle { destroy = false }
}
import {
  to = %s.test
  id = %q
}
`, resourceType, resourceType, importID)
}

// An import has no configured value to keep, so the read records what the
// platform reports. For a value the platform does not echo as written, that
// must not plan a replacement of the real object.
func TestAccNetworkImportDoesNotReplace(t *testing.T) {
	const name = "acc-adopt-evpn"
	seed := strings.Replace(evpnConfig(name, "a"), `"test"`, `"seed"`, 1)
	adopted := adopt(evpnConfig(name, "a"), networkType, "us-texas-1/demo/"+name)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy:             checkGone(networkType, networkPath),
		Steps: []resource.TestStep{
			{Config: seed},
			{
				Config: adopted,
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(networkAddr, plancheck.ResourceActionUpdate),
				}},
				Check: resource.TestCheckResourceAttr(networkAddr, "evpn.vlan_id", "auto"),
			},
			{Config: evpnConfig(name, "a"), PlanOnly: true},
		},
	})
}

// NICo ignores a requested PKey, so an imported InfiniBand network reports the
// partition's own and the configured one never matches it.
func TestAccNetworkInfiniBandImportDoesNotReplace(t *testing.T) {
	const name = "acc-adopt-ib"
	config := func(pkey string) string {
		return fmt.Sprintf(`
resource %q "test" {
  id = %q
  infiniband = {
    partition = "training"
    %s
  }
}
`, networkType, name, pkey)
	}
	seed := strings.Replace(config(""), `"test"`, `"seed"`, 1)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy:             checkGone(networkType, networkPath),
		Steps: []resource.TestStep{
			{Config: seed},
			{
				Config: adopt(config(`pkey = "0x10"`), networkType, "us-texas-1/demo/"+name),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(networkAddr, plancheck.ResourceActionUpdate),
				}},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(networkAddr, "infiniband.pkey", "0x10"),
					resource.TestCheckResourceAttr(networkAddr, "infiniband.pkey_allocated", "0x8001"),
				),
			},
			{Config: config(`pkey = "0x10"`), PlanOnly: true},
			// Once recorded, a changed PKey is a real request again.
			{
				Config: permitReplace(config(`pkey = "0x20"`), networkType),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(networkAddr, plancheck.ResourceActionReplace),
				}},
			},
		},
	})
}

func TestAccNetworkClearsMetadata(t *testing.T) {
	const name = "acc-clear"
	bare := fmt.Sprintf(`
resource %q "test" {
  id = %q
  evpn = {
    service_type = "l3vpn"
    vlan_id      = "auto"
  }
}
`, networkType, name)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy:             checkGone(networkType, networkPath),
		Steps: []resource.TestStep{
			{Config: strings.Replace(bare, `id = `, `description = "temporary"
  display_name = "temporary"
  tags = { team = "ml" }
  id = `, 1)},
			{
				Config: bare,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckNoResourceAttr(networkAddr, "display_name"),
					resource.TestCheckNoResourceAttr(networkAddr, "description"),
					resource.TestCheckNoResourceAttr(networkAddr, "tags.%"),
				),
			},
			{Config: bare, PlanOnly: true},
		},
	})
}

func clusterLocalSubnetConfig(network, name string) string {
	return strings.Replace(subnetConfig(network, name), "address-pools/tenant", "address-pools/cluster-local", 1)
}

// `allocate_gateway` is not in the response, and on a pool that is not routable
// an empty gateway fits either value, so the import cannot record it.
func TestAccSubnetImportDoesNotReplace(t *testing.T) {
	const network, name = "acc-adopt-net", "acc-adopt-sub"
	config := clusterLocalSubnetConfig(network, name)
	seed := strings.Replace(config, `"test" {
  id               = "`+name, `"seed" {
  id               = "`+name, 1)
	adopted := adopt(config, subnetType, "us-texas-1/demo/"+network+"/"+name)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			checkGone(subnetType, subnetPath),
			checkGone(networkType, networkPath),
		),
		Steps: []resource.TestStep{
			{
				Config: seed,
				Check:  resource.TestCheckNoResourceAttr(subnetType+".seed", "gateway"),
			},
			{
				Config: adopted,
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(subnetAddr, plancheck.ResourceActionUpdate),
				}},
			},
			{Config: config, PlanOnly: true},
		},
	})
}

// A `timeouts` block is the one thing that changes without a subnet operation.
func TestAccSubnetTimeoutsChangeInPlace(t *testing.T) {
	const network, name = "acc-timeouts-net", "acc-timeouts"
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
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(subnetAddr, plancheck.ResourceActionUpdate),
				}},
			},
			{Config: withTimeouts, PlanOnly: true},
		},
	})
}

// An import that plans no change must not leave anything behind that later
// swallows a real change.
func TestAccNetworkChangeAfterQuietImportReplaces(t *testing.T) {
	const name = "acc-quiet-ib"
	config := func(pkey string) string {
		return fmt.Sprintf(`
resource %q "test" {
  id = %q
  infiniband = {
    partition = "training"
    %s
  }
}
`, networkType, name, pkey)
	}
	seed := strings.Replace(config(""), `"test"`, `"seed"`, 1)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy:             checkGone(networkType, networkPath),
		Steps: []resource.TestStep{
			{Config: seed},
			{Config: adopt(config(""), networkType, "us-texas-1/demo/"+name)},
			{Config: config(""), PlanOnly: true},
			{
				Config: permitReplace(config(`pkey = "0x20"`), networkType),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(networkAddr, plancheck.ResourceActionReplace),
				}},
			},
		},
	})
}

func TestAccSubnetChangeAfterQuietImportReplaces(t *testing.T) {
	const network, name = "acc-quiet-net", "acc-quiet-sub"
	config := subnetConfig(network, name)
	seed := strings.Replace(config, `"test" {
  id               = "`+name, `"seed" {
  id               = "`+name, 1)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			checkGone(subnetType, subnetPath),
			checkGone(networkType, networkPath),
		),
		Steps: []resource.TestStep{
			{Config: seed},
			{Config: adopt(config, subnetType, "us-texas-1/demo/"+network+"/"+name)},
			{Config: config, PlanOnly: true},
			{
				Config: permitReplace(strings.Replace(config, "allocate_gateway = true", "allocate_gateway = false", 1), subnetType),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(subnetAddr, plancheck.ResourceActionReplace),
				}},
			},
		},
	})
}

// The contract accepts three spellings for the same addresses.
func TestAccSubnetExcludeRangesCompareAsAddresses(t *testing.T) {
	const network, name = "acc-ranges-net", "acc-ranges"
	config := func(ranges string) string {
		return strings.Replace(subnetConfig(network, name),
			`exclude_ranges   = ["10.0.0.10-10.0.0.20"]`, `exclude_ranges   = [`+ranges+`]`, 1)
	}
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			checkGone(subnetType, subnetPath),
			checkGone(networkType, networkPath),
		),
		Steps: []resource.TestStep{
			{Config: config(`"10.0.0.4", "10.0.0.8-10.0.0.15"`)},
			{
				Config: config(`"10.0.0.4/32", "10.0.0.8/29"`),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(subnetAddr, plancheck.ResourceActionUpdate),
				}},
			},
			{
				Config: permitReplace(config(`"10.0.0.5"`), subnetType),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(subnetAddr, plancheck.ResourceActionReplace),
				}},
			},
		},
	})
}

// The two waiter branches that no test could reach.
//
// Every four-set waiter in the provider has a `failed` branch and a
// `default: unknownStateError` branch, and both were dead code as far as any
// test was concerned: the fake only ever settled to `active`. The display-name
// markers exist so that a fake object can settle somewhere else, because
// nothing a client can legitimately send makes a platform fail on demand.
//
// Removing either branch from the waiter makes the matching case here hang
// until the step times out rather than fail fast, which is the behaviour the
// four-set rule was written to prevent.
func TestAccNetworkStopsOnAFailedOrUnrecognisedState(t *testing.T) {
	tests := []struct {
		name   string
		marker string
		expect string
	}{
		{
			name:   "a failed network names the reason",
			marker: "fake-settles-failed",
			// The detail is part of the assertion: the state message and
			// reason go through `stateDetail`, which nothing exercised.
			expect: `(?s)reports the network as failed.*INVALID_CONFIGURATION`,
		},
		{
			name:   "an unrecognised state stops instead of waiting it out",
			marker: "fake-settles-unknown",
			expect: `(?s)reports the network as "quiescing".*does not\s+recognise`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resource.Test(t, resource.TestCase{
				ProtoV6ProviderFactories: protoV6,
				Steps: []resource.TestStep{{
					Config:      evpnConfig("acc-"+tt.marker, tt.marker),
					ExpectError: regexp.MustCompile(tt.expect),
				}},
			})
		})
	}
}

// A cluster-owned network cannot be managed, and used to say so only at apply.
//
// The import is clean and the first plan is clean, because nothing read
// `cluster_id`. The 409 arrived partway through the apply, and on a destroy
// that leaves the state entry behind for the next run to try again.
//
// The fake materializes this network alongside the VPC, the way the cluster
// workflow does, so the object being adopted here is one no tenant created.
func TestAccNetworkRefusesToManageAClusterOwnedOne(t *testing.T) {
	const cluster = "acc-owned-net"
	owned := "vpc-" + cluster + "-net"

	// Declared to match what the platform materialized, so that only the
	// ownership guard can fail the second step.
	adoption := func(displayName string) string {
		return clusterConfig(cluster, 1) + fmt.Sprintf(`
resource %q "adopted" {
  id           = %q
  display_name = %q
  evpn = {
    service_type = "l2vpn"
    vlan_id      = "4002"
  }
  depends_on = [%s.test]
}
`, networkType, owned, displayName, clusterType)
	}

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		Steps: []resource.TestStep{
			{
				// The cluster alone: it is what brings the owned network into
				// being, and there is nothing yet to import from.
				Config: clusterConfig(cluster, 1),
			},
			{
				Config:             adoption("cluster network"),
				ResourceName:       networkType + ".adopted",
				ImportState:        true,
				ImportStateId:      "us-texas-1/demo/" + owned,
				ImportStatePersist: true,
				ImportStateVerify:  false,
			},
			{
				Config:      adoption("renamed by the tenant"),
				PlanOnly:    true,
				ExpectError: regexp.MustCompile(`owned by the platform, not by the tenant`),
			},
		},
	})
}

func subnetWithoutRangesConfig(network, name, extra string) string {
	return evpnConfig(network, "acceptance") + fmt.Sprintf(`
resource %q "test" {
  id            = %q
  network       = %s.test.id
  address_pool  = "/v1/regions/us-texas-1/networking/address-pools/tenant"
  prefix_length = 24
  %s
}
`, subnetType, name, networkType, extra)
}

// With `exclude_ranges` left out it is computed, and a change to anything else
// must still plan rather than fail on the unknown list.
func TestAccSubnetWithoutRangesStillChanges(t *testing.T) {
	const network, name = "acc-noranges-net", "acc-noranges"
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			checkGone(subnetType, subnetPath),
			checkGone(networkType, networkPath),
		),
		Steps: []resource.TestStep{
			{Config: subnetWithoutRangesConfig(network, name, "")},
			{Config: subnetWithoutRangesConfig(network, name, ""), PlanOnly: true},
			{
				Config: subnetWithoutRangesConfig(network, name, `timeouts { create = "30m" }`),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(subnetAddr, plancheck.ResourceActionUpdate),
				}},
			},
		},
	})
}

// The contract caps a subnet prefix at /32, so /33 fails the plan, not the apply.
func TestAccSubnetPrefixPastThirtyTwoFailsThePlan(t *testing.T) {
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		Steps: []resource.TestStep{{
			Config:      strings.Replace(subnetWithoutRangesConfig("acc-p33-net", "acc-p33", ""), "prefix_length = 24", "prefix_length = 33", 1),
			PlanOnly:    true,
			ExpectError: regexp.MustCompile(`(?s)prefix_length.*between 1 and 32`),
		}},
	})
}

// The tenant pool gives /16 to /28, so a /12 fails the plan with the pool's
// range, not the create.
func TestAccSubnetPrefixOutsideItsPoolFailsThePlan(t *testing.T) {
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		Steps: []resource.TestStep{{
			Config:      strings.Replace(subnetWithoutRangesConfig("acc-p12-net", "acc-p12", ""), "prefix_length = 24", "prefix_length = 12", 1),
			PlanOnly:    true,
			ExpectError: regexp.MustCompile(`(?s)outside its address pool's range.*"tenant" gives subnets from /16 to\s+/28.*between 16 and 28`),
		}},
	})
}

// Each fabric block has its own replace rule, and on a switch one side of it is
// null: the block being dropped has no plan and the one being added has no state.
func TestAccNetworkFabricSwitchReplaces(t *testing.T) {
	const name = "acc-switch"
	ib := fmt.Sprintf(`
resource %q "test" {
  id = %q
  infiniband = {
    partition = "training"
  }
}
`, networkType, name)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy:             checkGone(networkType, networkPath),
		Steps: []resource.TestStep{
			{Config: evpnConfig(name, "acceptance")},
			{
				Config: permitReplace(ib, networkType),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(networkAddr, plancheck.ResourceActionReplace),
				}},
				Check: resource.TestCheckResourceAttr(networkAddr, "type", "infiniband"),
			},
			{
				Config: permitReplace(evpnConfig(name, "acceptance"), networkType),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(networkAddr, plancheck.ResourceActionReplace),
				}},
				Check: resource.TestCheckResourceAttr(networkAddr, "type", "evpn"),
			},
		},
	})
}

// A fabric written the way a module would: the block from a variable, the
// other two null by condition. Under `terraform validate` all three are
// unknown, and the one-fabric rule has to wait for them.
func fabricFromVariableConfig(name, kind string) string {
	return fmt.Sprintf(`
variable "kind" {
  type    = string
  default = %q
}

variable "evpn" {
  type    = object({ service_type = string, vlan_id = string, mtu = number })
  default = { service_type = "l3vpn", vlan_id = "auto", mtu = 9000 }
}

resource %q "test" {
  id         = %q
  evpn       = var.kind == "ib" ? null : var.evpn
  infiniband = var.kind == "evpn" ? null : { partition = "training" }
}
`, kind, networkType, name)
}

func TestAccNetworkFabricFromAVariable(t *testing.T) {
	const name = "acc-fabric-var"
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy:             checkGone(networkType, networkPath),
		Steps: []resource.TestStep{
			{
				Config: fabricFromVariableConfig(name, "evpn"),
				Check:  resource.TestCheckResourceAttr(networkAddr, "type", "evpn"),
			},
			{Config: fabricFromVariableConfig(name, "evpn"), PlanOnly: true},
		},
	})
}

func TestAccNetworkFabricFromAVariableStillRefusesTwo(t *testing.T) {
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		Steps: []resource.TestStep{{
			Config:      fabricFromVariableConfig("acc-fabric-two", "both"),
			PlanOnly:    true,
			ExpectError: regexp.MustCompile(`A network has one fabric, not several`),
		}},
	})
}
