package acceptance

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/provider"
)

var (
	peeringType    = provider.ResourceName("vpc_peering")
	vpcBindingType = provider.ResourceName("vpc_security_groups")
	peeringAddr    = peeringType + ".test"
	vpcBindingAddr = vpcBindingType + ".test"
)

// Both resources hang off a VPC, and a VPC only exists because a cluster made
// one, so every case here builds the cluster first.
func vpcOwnerConfig(cluster string) string {
	return clusterConfig(cluster, 1)
}

func peeringConfig(cluster, name, remote string) string {
	return vpcOwnerConfig(cluster) + fmt.Sprintf(`
resource %q "test" {
  id         = %q
  vpc        = "vpc-%s"
  remote_vpc = %q

  display_name = "acceptance"
  depends_on   = [%s.test]
}
`, peeringType, name, cluster, remote, clusterType)
}

// `state = active` on one side says nothing about the other side existing. The
// fake settles this peering with no counterpart anywhere, which is the point:
// a provider that waited for reachability would hang here and on a real
// landscape.
func TestAccVPCPeeringIsOneSided(t *testing.T) {
	const cluster, name = "acc-peer-owner", "acc-to-inference"
	const remote = "/v1/regions/us-texas-1/projects/other/networking/vpcs/vpc-inference"

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			checkGone(peeringType, peeringPath),
			checkGone(clusterType, clusterPath),
		),
		Steps: []resource.TestStep{
			{
				Config: peeringConfig(cluster, name, remote),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(peeringAddr, "id", name),
					resource.TestCheckResourceAttr(peeringAddr, "vpc", "vpc-"+cluster),
					resource.TestCheckResourceAttr(peeringAddr, "remote_vpc", remote),
					// Active with no counterpart declared anywhere.
					resource.TestCheckResourceAttr(peeringAddr, "state", "active"),
					// Read back as "" and {}, and still null to a configuration
					// that set neither.
					resource.TestCheckNoResourceAttr(peeringAddr, "description"),
					resource.TestCheckNoResourceAttr(peeringAddr, "tags.%"),
				),
			},
			{
				Config:   peeringConfig(cluster, name, remote),
				PlanOnly: true,
			},
			{
				Config:            peeringConfig(cluster, name, remote),
				ResourceName:      peeringAddr,
				ImportState:       true,
				ImportStateId:     "us-texas-1/demo/vpc-" + cluster + "/" + name,
				ImportStateVerify: true,
			},
		},
	})
}

// createVPCPeering waits on a row lock for its VPC, and when that wait runs
// out it answers 503 and the contract says the create wrote nothing and is
// safe to send again. The marker in this id makes the fake answer exactly
// that to the first create, so the apply passes only if the provider waits
// it out and sends the create again, and it must end with one peering, not
// two.
func TestAccVPCPeeringWaitsOutALostVPCLock(t *testing.T) {
	const cluster, name = "acc-peer-lock", "acc-to-inference-fake-vpc-lock-lost"
	const remote = "/v1/regions/us-texas-1/projects/other/networking/vpcs/vpc-inference"

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			checkGone(peeringType, peeringPath),
			checkGone(clusterType, clusterPath),
		),
		Steps: []resource.TestStep{
			{
				Config: peeringConfig(cluster, name, remote),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(peeringAddr, "id", name),
					resource.TestCheckResourceAttr(peeringAddr, "state", "active"),
					checkPeeringCount("vpc-"+cluster, 1),
				),
			},
			{
				Config:   peeringConfig(cluster, name, remote),
				PlanOnly: true,
			},
		},
	})
}

// checkPeeringCount asks the fake, not Terraform, how many peerings the VPC
// has, so a create that landed twice would show.
func checkPeeringCount(vpc string, want int) resource.TestCheckFunc {
	return func(*terraform.State) error {
		token, err := mintFakeToken()
		if err != nil {
			return err
		}
		req, err := http.NewRequest(http.MethodGet,
			fmt.Sprintf("%s/v1/regions/us-texas-1/projects/demo/networking/vpcs/%s/peerings", endpoint, vpc), nil)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := harnessHTTP.Do(req)
		if err != nil {
			return err
		}
		defer func() { _ = resp.Body.Close() }()
		var page struct {
			Peerings []json.RawMessage `json:"peerings"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
			return fmt.Errorf("decoding the fake's %s: %w", resp.Status, err)
		}
		if len(page.Peerings) != want {
			return fmt.Errorf("the fake holds %d peerings on %s, want %d", len(page.Peerings), vpc, want)
		}
		return nil
	}
}

func vpcBindingConfig(cluster, group string, attach bool) string {
	groups := "[]"
	if attach {
		groups = fmt.Sprintf("[%s.test.id]", securityGroupType)
	}
	return vpcOwnerConfig(cluster) + securityGroupConfig(group, false) + fmt.Sprintf(`
resource %q "test" {
  vpc             = "vpc-%s"
  security_groups = %s

  depends_on = [%s.test]
}
`, vpcBindingType, cluster, groups, clusterType)
}

// The empty list is the case worth testing. It does not clear the binding, it
// reinstates the platform's default group, and `attached` has to report that
// while `security_groups` keeps what was configured or every plan proposes
// changing it back.
func TestAccVPCSecurityGroupsEmptyMeansDefault(t *testing.T) {
	const cluster, group = "acc-bind-owner", "acc-bind-filter"

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		// Only the cluster is left for the final destroy. The last step drops
		// the binding and the group, and checks the VPC's filter instead.
		CheckDestroy: checkGone(clusterType, clusterPath),
		Steps: []resource.TestStep{
			{
				Config: vpcBindingConfig(cluster, group, true),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(vpcBindingAddr, "vpc", "vpc-"+cluster),
					resource.TestCheckResourceAttr(vpcBindingAddr, "security_groups.0", group),
					resource.TestCheckResourceAttr(vpcBindingAddr, "attached.0", group),
					resource.TestCheckResourceAttr(vpcBindingAddr, "state", "active"),
				),
			},
			{
				Config:   vpcBindingConfig(cluster, group, true),
				PlanOnly: true,
			},
			{
				Config: vpcBindingConfig(cluster, group, false),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(vpcBindingAddr, "security_groups.#", "0"),
					// Not empty. The default came back, explicitly.
					resource.TestCheckResourceAttr(vpcBindingAddr, "attached.0", "vpc-"+cluster+"-default"),
				),
			},
			{
				Config:   vpcBindingConfig(cluster, group, false),
				PlanOnly: true,
			},
			{
				Config:            vpcBindingConfig(cluster, group, false),
				ResourceName:      vpcBindingAddr,
				ImportState:       true,
				ImportStateId:     "us-texas-1/demo/vpc-" + cluster,
				ImportStateVerify: true,
				// The binding has no id attribute. It is named by its VPC.
				// Nothing is ignored: a binding holding only the VPC's default
				// group imports as the empty list that put it there.
				ImportStateVerifyIdentifierAttribute: "vpc",
			},
			{
				// The binding out of the configuration, the cluster still in
				// it. This is the destroy the resource documents as widening
				// the filter, and the only place it can be observed: a
				// CheckDestroy runs after the cluster has gone too, and the VPC
				// goes with it.
				Config: clusterConfig(cluster, 1),
				Check:  checkVPCCarriesOnlyDefault("vpc-" + cluster),
			},
		},
	})
}

// A binding changed outside Terraform has to produce a plan.
//
// Someone swaps a VPC's security groups in the console and the plan has to
// show it, on a security boundary. `ExpectNonEmptyPlan` is what proves it:
// with the read keeping prior state, that step fails.
func TestAccVPCSecurityGroupsDetectsOutOfBandChange(t *testing.T) {
	const cluster, group = "acc-drift-owner", "acc-drift-filter"
	config := vpcBindingConfig(cluster, group, true)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			checkGone(securityGroupType, securityGroupPath),
			checkGone(clusterType, clusterPath),
		),
		Steps: []resource.TestStep{
			{
				Config: config,
				Check:  resource.TestCheckResourceAttr(vpcBindingAddr, "attached.0", group),
			},
			{
				PreConfig: func() {
					if err := setVPCGroupsOutOfBand(t, "vpc-"+cluster, []string{}); err != nil {
						t.Fatalf("could not change the binding out of band: %v", err)
					}
				},
				Config:             config,
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
			{
				// And reconciling puts it back.
				Config: config,
				Check:  resource.TestCheckResourceAttr(vpcBindingAddr, "attached.0", group),
			},
		},
	})
}

// setVPCGroupsOutOfBand goes straight at the fake: through the API, not
// through Terraform, because that is the whole point.
func setVPCGroupsOutOfBand(t *testing.T, vpc string, groups []string) error {
	t.Helper()

	token, err := mintFakeToken()
	if err != nil {
		return err
	}
	payload, err := json.Marshal(map[string]any{"securityGroups": groups})
	if err != nil {
		return err
	}

	url := fmt.Sprintf("%s/v1/regions/us-texas-1/projects/demo/networking/vpcs/%s", endpoint, vpc)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(payload))
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
	if resp.StatusCode != http.StatusAccepted {
		body, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			return fmt.Errorf("the fake answered %s and its body could not be read: %w",
				resp.Status, readErr)
		}
		return fmt.Errorf("the fake answered %s: %s", resp.Status, body)
	}
	return awaitVPCSettled(vpc)
}

// awaitVPCSettled reads the VPC until its binding change has rendered, the
// way a change made in the console has long since rendered by the time
// anyone runs Terraform. The fake refuses a second change while one renders,
// as the contract does, and it settles a VPC by being read, so a test that
// went straight on would race its own out-of-band write.
func awaitVPCSettled(vpc string) error {
	for range 20 {
		status, body, err := readFake("/v1/regions/us-texas-1/projects/demo/networking/vpcs/" + vpc)
		if err != nil {
			return err
		}
		var read struct {
			State string `json:"state"`
		}
		if err := json.Unmarshal(body, &read); err != nil {
			return fmt.Errorf("reading %s answered %d: %w", vpc, status, err)
		}
		if read.State != "updating" {
			return nil
		}
	}
	return fmt.Errorf("%s never settled after the out-of-band write", vpc)
}

func mintFakeToken() (string, error) {
	payload := []byte(fmt.Sprintf(
		`{"grant_type":"client_credentials","client_id":%q,"client_secret":%q}`,
		os.Getenv(provider.EnvVar("CLIENT_ID")), os.Getenv(provider.EnvVar("CLIENT_SECRET"))))

	resp, err := harnessHTTP.Post(endpoint+"/v1/regions/global/auth/token",
		"application/json", bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	var minted struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&minted); err != nil {
		return "", err
	}
	if minted.AccessToken == "" {
		return "", fmt.Errorf("the fake minted no token")
	}
	return minted.AccessToken, nil
}

// With an empty list, a group attached in the console has to plan, and keep
// planning until it is applied away. Not a warning that fires once.
func TestAccVPCSecurityGroupsEmptyStillReportsDrift(t *testing.T) {
	const cluster, group = "acc-bind-quiet", "acc-bind-quiet-filter"
	attach := func() {
		if err := setVPCGroupsOutOfBand(t, "vpc-"+cluster, []string{group}); err != nil {
			t.Fatal(err)
		}
	}
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			checkGone(securityGroupType, securityGroupPath),
			checkGone(clusterType, clusterPath),
		),
		Steps: []resource.TestStep{
			{Config: vpcBindingConfig(cluster, group, false)},
			{Config: vpcBindingConfig(cluster, group, false), PlanOnly: true},
			{PreConfig: attach, RefreshState: true, ExpectNonEmptyPlan: true},
			{Config: vpcBindingConfig(cluster, group, false), PlanOnly: true, ExpectNonEmptyPlan: true},
			{
				Config: vpcBindingConfig(cluster, group, false),
				Check:  resource.TestCheckResourceAttr(vpcBindingAddr, "attached.0", "vpc-"+cluster+"-default"),
			},
			{Config: vpcBindingConfig(cluster, group, false), PlanOnly: true},
		},
	})
}

// A `timeouts` block is the one change a peering takes without an operation.
func TestAccVPCPeeringTimeoutsChangeInPlace(t *testing.T) {
	const cluster, name = "acc-peer-timeouts-owner", "acc-peer-timeouts"
	remote := "/v1/regions/us-texas-1/projects/inference/networking/vpcs/vpc-far"
	base := peeringConfig(cluster, name, remote)
	withTimeouts := strings.Replace(base, "remote_vpc", "timeouts {\n    create = \"30m\"\n  }\n  remote_vpc", 1)
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
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(peeringAddr, plancheck.ResourceActionUpdate),
				}},
			},
			{Config: withTimeouts, PlanOnly: true},
		},
	})
}

// A binding on the wrong fabric backend fails the apply.
//
// This pins the 409 end to end: the fake refuses a binding on a `ufo` VPC, and
// the apply has to stop with the provider's error. Removing the backend check
// in the fake makes this pass an apply it should have refused.
//
// It doesn't prove the warning that names the backend. A test step can't see
// warnings, and this passes with the warning removed.
// TestBindingConflictNamesTheBackendOnlyWhenItIsTheCause is what covers it.
func TestAccVPCSecurityGroupsRefusedOnTheWrongBackend(t *testing.T) {
	const cluster, group = "acc-on-ufo-owner", "acc-ufo-filter"

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			checkGone(securityGroupType, securityGroupPath),
			checkGone(clusterType, clusterPath),
		),
		Steps: []resource.TestStep{{
			Config:      vpcBindingConfig(cluster, group, true),
			ExpectError: regexp.MustCompile(`(?s)attach the security groups to the VPC`),
		}},
	})
}

// vpcBindingReplacedConfig binds one of two groups to the cluster's VPC, with
// create_before_destroy and a replacement forced by bumping rev. A change to
// `security_groups` alone is an update, so replace_triggered_by is how a
// configuration reaches a replacement on the same VPC, and -replace is the
// other way, which a test step can't pass.
func vpcBindingReplacedConfig(cluster, first, second, attach, rev string) string {
	other := strings.Replace(securityGroupConfig(second, false), `"test" {`, `"second" {`, 1)
	return vpcOwnerConfig(cluster) + securityGroupConfig(first, false) + other + fmt.Sprintf(`
resource "terraform_data" "rev" {
  input = %q
}

resource %q "test" {
  vpc             = "vpc-%s"
  security_groups = [%q]

  depends_on = [%s.test, %s.test, %s.second]

  lifecycle {
    create_before_destroy = true
    replace_triggered_by  = [terraform_data.rev]
  }
}
`, rev, vpcBindingType, cluster, attach, clusterType, securityGroupType, securityGroupType)
}

// A replacement with create_before_destroy on the same VPC writes the new
// binding first and destroys the old one after. The destroy used to send the
// empty list whatever it found, which put the VPC back on DEFAULT and took
// off the binding the same apply had just written. The VPC has to end the
// apply holding the new binding, both when the replacement changes the groups
// and when it writes the same ones again, which is what -replace does.
func TestAccVPCSecurityGroupsReplacedBeforeDestroyKeepsTheNewBinding(t *testing.T) {
	const cluster, first, second = "acc-cbd-owner", "acc-cbd-first", "acc-cbd-second"
	vpc := "vpc-" + cluster
	replaced := resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
		plancheck.ExpectResourceAction(vpcBindingAddr, plancheck.ResourceActionCreateBeforeDestroy),
	}}

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy:             checkGone(clusterType, clusterPath),
		Steps: []resource.TestStep{
			{
				Config: vpcBindingReplacedConfig(cluster, first, second, first, "1"),
				Check:  checkVPCHolds(vpc, first),
			},
			{
				// Other groups.
				Config:           vpcBindingReplacedConfig(cluster, first, second, second, "2"),
				ConfigPlanChecks: replaced,
				Check: resource.ComposeAggregateTestCheckFunc(
					checkVPCHolds(vpc, second),
					resource.TestCheckResourceAttr(vpcBindingAddr, "attached.0", second),
				),
			},
			{Config: vpcBindingReplacedConfig(cluster, first, second, second, "2"), PlanOnly: true},
			{
				// The same groups, written again by the new instance.
				Config:           vpcBindingReplacedConfig(cluster, first, second, second, "3"),
				ConfigPlanChecks: replaced,
				Check:            checkVPCHolds(vpc, second),
			},
			{Config: vpcBindingReplacedConfig(cluster, first, second, second, "3"), PlanOnly: true},
			{
				// And a plain destroy still hands the VPC back to DEFAULT.
				Config: clusterConfig(cluster, 1),
				Check:  checkVPCCarriesOnlyDefault(vpc),
			},
		},
	})
}

// checkVPCHolds asks the fake, not Terraform, what the VPC's binding is, in
// order, so a destroy that cleared it after the state was written shows.
func checkVPCHolds(vpc string, want ...string) resource.TestCheckFunc {
	return func(*terraform.State) error {
		status, body, err := readFake("/v1/regions/us-texas-1/projects/demo/networking/vpcs/" + vpc)
		if err != nil {
			return err
		}
		if status != http.StatusOK {
			return fmt.Errorf("reading %s answered %d", vpc, status)
		}
		var read struct {
			SecurityGroups []string `json:"securityGroups"`
		}
		if err := json.Unmarshal(body, &read); err != nil {
			return err
		}
		if strings.Join(read.SecurityGroups, ",") != strings.Join(want, ",") {
			return fmt.Errorf("%s holds %v, wanted %v", vpc, read.SecurityGroups, want)
		}
		return nil
	}
}
