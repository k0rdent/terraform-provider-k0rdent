package acceptance

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/knownvalue"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/provider"
)

// The create token's tag key, as the provider names it. Built here rather
// than imported, because it's the provider's internal and the point is to
// check it never shows.
var createTokenKey = "terraform-provider-" + provider.ProviderTypeName + "/create-token"

// withDefaultTags is a provider block that sets default_tags, given as HCL.
func withDefaultTags(defaults string) string {
	return fmt.Sprintf("\nprovider %q {\n  default_tags = %s\n}\n", provider.ProviderTypeName, defaults)
}

func taggedNetworkConfig(name, defaults, tags string) string {
	return withDefaultTags(defaults) + fmt.Sprintf(`
resource %q "test" {
  id   = %q
  tags = %s
  evpn = {
    service_type = "l3vpn"
    vlan_id      = "4001"
  }
}
`, networkType, name, tags)
}

// tagsAllIs is the plan check that tags_all is known at plan, and what it is.
func tagsAllIs(addr string, want map[string]string) plancheck.PlanCheck {
	values := make(map[string]knownvalue.Check, len(want))
	for k, v := range want {
		values[k] = knownvalue.StringExact(v)
	}
	return plancheck.ExpectKnownValue(addr, tfjsonpath.New("tags_all"), knownvalue.MapExact(values))
}

// checkPlatformTags reads the object from the fake and compares the tags it
// holds, less the create token, with want.
func checkPlatformTags(addr string, pathOf func(map[string]string) string, want map[string]string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs, ok := s.RootModule().Resources[addr]
		if !ok {
			return fmt.Errorf("%s is not in state", addr)
		}
		code, body, err := readFake(pathOf(rs.Primary.Attributes))
		if err != nil {
			return err
		}
		if code != http.StatusOK {
			return fmt.Errorf("reading %s from the fake answered %d", addr, code)
		}
		var got struct {
			Tags map[string]any `json:"tags"`
		}
		if err := json.Unmarshal(body, &got); err != nil {
			return err
		}
		delete(got.Tags, createTokenKey)
		have := map[string]string{}
		for k, v := range got.Tags {
			have[k] = fmt.Sprint(v)
		}
		if !reflect.DeepEqual(have, want) {
			return fmt.Errorf("%s: the platform holds %v, want %v", addr, have, want)
		}
		return nil
	}
}

// patchFake sends a PATCH the provider didn't, as another client would.
func patchFake(path, body string) error {
	token, err := mintFakeToken()
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPatch, endpoint+path, bytes.NewBufferString(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := harnessHTTP.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		out, rerr := io.ReadAll(resp.Body)
		if rerr != nil {
			return fmt.Errorf("PATCH %s answered %d: %w", path, resp.StatusCode, rerr)
		}
		return fmt.Errorf("PATCH %s answered %d: %s", path, resp.StatusCode, out)
	}
	return nil
}

// TestAccDefaultTags is default_tags through a network's whole life: the
// merge known at plan, a resource tag overriding a default, a changed default
// as an in-place update, a removed one removed from the platform, a tag set
// some other way removed as drift, import, and a resource tag equal to a
// default planning nothing afterwards (AWS's perpetual-diff pitfall). The
// testing framework plans again after every apply and fails on any change, so
// each step also proves its own result is stable.
func TestAccDefaultTags(t *testing.T) {
	const name = "acc-default-tags"
	path := "/v1/regions/us-texas-1/projects/demo/networking/networks/" + name

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy:             checkGone(networkType, networkPath),
		Steps: []resource.TestStep{
			{
				Config: taggedNetworkConfig(name, `{ owner = "pipeline", env = "prod" }`, `{ env = "dev", team = "ml" }`),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					tagsAllIs(networkAddr, map[string]string{"owner": "pipeline", "env": "dev", "team": "ml"}),
				}},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(networkAddr, "tags.%", "2"),
					resource.TestCheckResourceAttr(networkAddr, "tags_all.%", "3"),
					resource.TestCheckResourceAttr(networkAddr, "tags_all.env", "dev"),
					resource.TestCheckNoResourceAttr(networkAddr, "tags_all."+createTokenKey),
					checkPlatformTags(networkAddr, networkPath, map[string]string{"owner": "pipeline", "env": "dev", "team": "ml"}),
				),
			},
			{
				Config: taggedNetworkConfig(name, `{ owner = "platform-team", env = "prod" }`, `{ env = "dev", team = "ml" }`),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(networkAddr, plancheck.ResourceActionUpdate),
					tagsAllIs(networkAddr, map[string]string{"owner": "platform-team", "env": "dev", "team": "ml"}),
				}},
				Check: checkPlatformTags(networkAddr, networkPath, map[string]string{"owner": "platform-team", "env": "dev", "team": "ml"}),
			},
			{
				Config: taggedNetworkConfig(name, `{ env = "prod" }`, `{ env = "dev", team = "ml" }`),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(networkAddr, plancheck.ResourceActionUpdate),
					tagsAllIs(networkAddr, map[string]string{"env": "dev", "team": "ml"}),
				}},
				Check: checkPlatformTags(networkAddr, networkPath, map[string]string{"env": "dev", "team": "ml"}),
			},
			{
				Config:            taggedNetworkConfig(name, `{ env = "prod" }`, `{ env = "dev", team = "ml" }`),
				ResourceName:      networkAddr,
				ImportState:       true,
				ImportStateId:     "us-texas-1/demo/" + name,
				ImportStateVerify: true,
			},
			{
				// Another client adds a tag. `tags` is authoritative, as it
				// was before default_tags, so it's drift and the apply
				// removes it.
				PreConfig: func() {
					if err := patchFake(path, `{"tags": {"env": "dev", "team": "ml", "added": "by-hand"}}`); err != nil {
						t.Fatal(err)
					}
				},
				Config: taggedNetworkConfig(name, `{ env = "prod" }`, `{ env = "dev", team = "ml" }`),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(networkAddr, plancheck.ResourceActionUpdate),
					tagsAllIs(networkAddr, map[string]string{"env": "dev", "team": "ml"}),
				}},
				Check: checkPlatformTags(networkAddr, networkPath, map[string]string{"env": "dev", "team": "ml"}),
			},
			{
				// A resource tag equal to a default. AWS's first version read
				// it back as the default's and planned to add it again,
				// forever. The empty plan after this apply is the check.
				Config: taggedNetworkConfig(name, `{ env = "prod", owner = "pipeline" }`, `{ env = "prod", team = "ml" }`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(networkAddr, "tags.%", "2"),
					resource.TestCheckResourceAttr(networkAddr, "tags.env", "prod"),
					resource.TestCheckResourceAttr(networkAddr, "tags_all.%", "3"),
				),
			},
			{
				Config:   taggedNetworkConfig(name, `{ env = "prod", owner = "pipeline" }`, `{ env = "prod", team = "ml" }`),
				PlanOnly: true,
			},
			{
				// An import has no configuration to read, so a tag with a
				// default's value is the default's.
				Config:        taggedNetworkConfig(name, `{ env = "prod", owner = "pipeline" }`, `{ env = "prod", team = "ml" }`),
				ResourceName:  networkAddr,
				ImportState:   true,
				ImportStateId: "us-texas-1/demo/" + name,
				ImportStateCheck: func(states []*terraform.InstanceState) error {
					if len(states) != 1 {
						return fmt.Errorf("imported %d states, want 1", len(states))
					}
					a := states[0].Attributes
					if a["tags.%"] != "1" || a["tags.team"] != "ml" || a["tags_all.%"] != "3" || a["tags_all.env"] != "prod" {
						return fmt.Errorf("imported tags %v", a)
					}
					return nil
				},
			},
		},
	})
}

// TestAccDefaultTagsKnownOnlyAtApply is a default that waits on another
// resource. The plan can't know the merge, so tags_all is unknown rather than
// a guess the apply would contradict, and the apply fills it.
func TestAccDefaultTagsKnownOnlyAtApply(t *testing.T) {
	const name = "acc-default-tags-later"
	config := `
resource "terraform_data" "run" {
  input = "run"
}
` + withDefaultTags(`{ run = terraform_data.run.id }`) + fmt.Sprintf(`
resource %q "test" {
  id   = %q
  tags = { team = "ml" }
  evpn = {
    service_type = "l3vpn"
    vlan_id      = "4001"
  }
}
`, networkType, name)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy:             checkGone(networkType, networkPath),
		Steps: []resource.TestStep{
			{
				Config: config,
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectUnknownValue(networkAddr, tfjsonpath.New("tags_all")),
				}},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPair(networkAddr, "tags_all.run", "terraform_data.run", "id"),
					resource.TestCheckResourceAttr(networkAddr, "tags_all.team", "ml"),
					resource.TestCheckResourceAttr(networkAddr, "tags.%", "1"),
				),
			},
		},
	})
}

// TestAccDefaultTagsReachEveryTaggedKind is every resource that carries tags
// under one provider block: the cluster, the network, the security group and
// the VPC peering. A changed default updates the first three in place. The
// API sets a peering's tags only at creation, so it keeps the ones it has
// and plans nothing, rather than being replaced, and writing a default out
// in its own tags doesn't replace it either.
func TestAccDefaultTagsReachEveryTaggedKind(t *testing.T) {
	const cluster, network, sg, peer = "acc-dt-owner", "acc-dt-net", "acc-dt-sg", "acc-dt-peer"
	const remote = "/v1/regions/us-texas-1/projects/other/networking/vpcs/vpc-inference"
	config := func(defaults, peerTags string) string {
		return withDefaultTags(defaults) +
			clusterConfig(cluster, 1) +
			securityGroupConfig(sg, false) +
			fmt.Sprintf(`
resource %q "test" {
  id   = %q
  evpn = {
    service_type = "l3vpn"
    vlan_id      = "4001"
  }
}

resource %q "test" {
  id         = %q
  vpc        = "vpc-%s"
  remote_vpc = %q
  tags       = %s
  depends_on = [%s.test]
}
`, networkType, network, peeringType, peer, cluster, remote, peerTags, clusterType)
	}
	every := []string{clusterAddr, networkAddr, securityGroupAddr, peeringAddr}

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			checkGone(peeringType, peeringPath),
			checkGone(networkType, networkPath),
			checkGone(securityGroupType, securityGroupPath),
			checkGone(clusterType, clusterPath),
		),
		Steps: []resource.TestStep{
			{
				Config: config(`{ owner = "pipeline" }`, "null"),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					tagsAllIs(clusterAddr, map[string]string{"owner": "pipeline"}),
					tagsAllIs(networkAddr, map[string]string{"owner": "pipeline"}),
					tagsAllIs(securityGroupAddr, map[string]string{"owner": "pipeline", "team": "ml"}),
					tagsAllIs(peeringAddr, map[string]string{"owner": "pipeline"}),
				}},
				Check: resource.ComposeAggregateTestCheckFunc(
					checkPlatformTags(clusterAddr, clusterPath, map[string]string{"owner": "pipeline"}),
					checkPlatformTags(networkAddr, networkPath, map[string]string{"owner": "pipeline"}),
					checkPlatformTags(securityGroupAddr, securityGroupPath, map[string]string{"owner": "pipeline", "team": "ml"}),
					checkPlatformTags(peeringAddr, peeringPath, map[string]string{"owner": "pipeline"}),
					noTokenIn(every...),
				),
			},
			{
				Config: config(`{ owner = "platform-team" }`, "null"),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(clusterAddr, plancheck.ResourceActionUpdate),
					plancheck.ExpectResourceAction(networkAddr, plancheck.ResourceActionUpdate),
					plancheck.ExpectResourceAction(securityGroupAddr, plancheck.ResourceActionUpdate),
					plancheck.ExpectResourceAction(peeringAddr, plancheck.ResourceActionNoop),
					tagsAllIs(peeringAddr, map[string]string{"owner": "pipeline"}),
				}},
				Check: resource.ComposeAggregateTestCheckFunc(
					checkPlatformTags(clusterAddr, clusterPath, map[string]string{"owner": "platform-team"}),
					checkPlatformTags(networkAddr, networkPath, map[string]string{"owner": "platform-team"}),
					checkPlatformTags(securityGroupAddr, securityGroupPath, map[string]string{"owner": "platform-team", "team": "ml"}),
					checkPlatformTags(peeringAddr, peeringPath, map[string]string{"owner": "pipeline"}),
					noTokenIn(every...),
				),
			},
			{
				// The peering writes out the tag it already holds. Its tags
				// change, the platform's don't, so it's an update with no
				// request rather than a replacement.
				Config: config(`{ owner = "platform-team" }`, `{ owner = "pipeline" }`),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(peeringAddr, plancheck.ResourceActionUpdate),
					tagsAllIs(peeringAddr, map[string]string{"owner": "pipeline"}),
				}},
				Check: resource.TestCheckResourceAttr(peeringAddr, "tags.owner", "pipeline"),
			},
		},
	})
}

// noTokenIn checks that no resource's state carries the create token, in
// tags or in tags_all.
func noTokenIn(addrs ...string) resource.TestCheckFunc {
	checks := make([]resource.TestCheckFunc, 0, 2*len(addrs))
	for _, addr := range addrs {
		checks = append(checks,
			resource.TestCheckNoResourceAttr(addr, "tags_all."+createTokenKey),
			resource.TestCheckNoResourceAttr(addr, "tags."+createTokenKey))
	}
	return resource.ComposeAggregateTestCheckFunc(checks...)
}

// TestAccDefaultTagsTakeOverAPeeringsTag is the migration a configuration
// makes when it adopts default_tags: a tag the peering wrote itself moves to
// the provider block with the same value. The platform's tags don't change,
// so the peering is updated in state and not replaced. Peerings have no
// allow_replace guard, so a replacement here would have gone ahead.
func TestAccDefaultTagsTakeOverAPeeringsTag(t *testing.T) {
	const cluster, peer = "acc-dt-move-owner", "acc-dt-move"
	const remote = "/v1/regions/us-texas-1/projects/other/networking/vpcs/vpc-inference"
	config := func(defaults, peerTags string) string {
		return withDefaultTags(defaults) + clusterConfig(cluster, 1) + fmt.Sprintf(`
resource %q "test" {
  id         = %q
  vpc        = "vpc-%s"
  remote_vpc = %q
  tags       = %s
  depends_on = [%s.test]
}
`, peeringType, peer, cluster, remote, peerTags, clusterType)
	}

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			checkGone(peeringType, peeringPath),
			checkGone(clusterType, clusterPath),
		),
		Steps: []resource.TestStep{
			{
				Config: config("{}", `{ team = "ml" }`),
				Check:  checkPlatformTags(peeringAddr, peeringPath, map[string]string{"team": "ml"}),
			},
			{
				Config: config(`{ team = "ml" }`, "null"),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(peeringAddr, plancheck.ResourceActionUpdate),
					tagsAllIs(peeringAddr, map[string]string{"team": "ml"}),
				}},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckNoResourceAttr(peeringAddr, "tags.team"),
					resource.TestCheckResourceAttr(peeringAddr, "tags_all.team", "ml"),
					checkPlatformTags(peeringAddr, peeringPath, map[string]string{"team": "ml"}),
				),
			},
		},
	})
}
