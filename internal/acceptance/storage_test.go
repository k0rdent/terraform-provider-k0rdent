package acceptance

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/provider"
)

var (
	fileStoreType  = provider.ResourceName("file_store")
	shareTypesType = provider.ResourceName("share_types")
	shareTypeType  = provider.ResourceName("share_type")
	fileStoreAddr  = fileStoreType + ".test"
	shareTypesAddr = "data." + shareTypesType + ".all"
	shareTypeAddr  = "data." + shareTypeType + ".one"
)

// fileStoreConfig is the store as one address consumer, parameterised on the
// three things every test below varies.
func fileStoreConfig(name, displayName string, capacity int, consumers string) string {
	return fmt.Sprintf(`
resource %q "test" {
  id           = %q
  display_name = %q
  share_type   = "general"
  capacity_gib = %d

  consumers = %s
}
`, fileStoreType, name, displayName, capacity, consumers)
}

const oneConsumer = `{
    "10.0.0.31" = { mount_path = "/mnt/datasets" }
  }`

const twoConsumers = `{
    "10.0.0.31" = { mount_path = "/mnt/datasets" }
    "10.0.0.32" = { mount_path = "/mnt/datasets" }
  }`

// The destroy is the point of this test, and it is the step that would fail
// without the detach: the platform refuses to delete a store that still lists
// consumers, so the resource has to empty them first. Validated by removing the
// detach and watching the destroy answer 409.
//
// The rest is the ordinary lifecycle, plus an expand, because capacity is the
// one mutable field with a rule attached.
func TestAccFileStore(t *testing.T) {
	const name = "acc-datasets"

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy:             checkGone(fileStoreType, fileStorePath),
		Steps: []resource.TestStep{
			{
				Config: fileStoreConfig(name, "acceptance", 1024, twoConsumers),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(fileStoreAddr, "id", name),
					resource.TestCheckResourceAttr(fileStoreAddr, "state", "active"),
					resource.TestCheckResourceAttr(fileStoreAddr, "share_type", "general"),
					resource.TestCheckResourceAttr(fileStoreAddr, "capacity_gib", "1024"),
					resource.TestCheckResourceAttr(fileStoreAddr, "region", "us-texas-1"),
					resource.TestCheckResourceAttr(fileStoreAddr, "project", "demo"),
					resource.TestCheckResourceAttr(fileStoreAddr, "consumers.%", "2"),
					// The defaults have to land in state, or the next plan
					// proposes setting them.
					resource.TestCheckResourceAttr(fileStoreAddr, "consumers.10.0.0.31.type", "ipAddress"),
					resource.TestCheckResourceAttr(fileStoreAddr, "consumers.10.0.0.31.access_level", "rw"),
					resource.TestCheckResourceAttr(fileStoreAddr, "consumers.10.0.0.31.mount_path", "/mnt/datasets"),
					resource.TestCheckResourceAttr(fileStoreAddr, "consumers.10.0.0.32.type", "ipAddress"),
					// `active` means the grant is enforced on the backing
					// storage, not merely recorded. That is the difference
					// between a host that can mount and one that cannot.
					resource.TestCheckResourceAttr(fileStoreAddr, "consumer_status.%", "2"),
					resource.TestCheckResourceAttr(fileStoreAddr, "consumer_status.10.0.0.31.state", "active"),
					resource.TestCheckResourceAttr(fileStoreAddr, "consumer_status.10.0.0.31.resolved_addresses.#", "1"),
					resource.TestCheckResourceAttrSet(fileStoreAddr, "etag"),
					// A tenant-facing location and an operator-only one, in no
					// declared order, so the assertion picks by the flag.
					resource.TestCheckResourceAttr(fileStoreAddr, "export_locations.#", "2"),
					resource.TestCheckTypeSetElemNestedAttrs(fileStoreAddr, "export_locations.*", map[string]string{
						"preferred": "true",
						"protocol":  "nfs",
						"server":    "10.96.0.20",
					}),
					resource.TestCheckTypeSetElemNestedAttrs(fileStoreAddr, "export_locations.*", map[string]string{
						"admin_only": "true",
					}),
				),
			},
			{
				Config:   fileStoreConfig(name, "acceptance", 1024, twoConsumers),
				PlanOnly: true,
			},
			{
				// An expand and a rename in one apply, and a detach: dropping
				// an entry from the list is how a grant is revoked, because the
				// list is desired state rather than a patch.
				Config: fileStoreConfig(name, "renamed", 2048, oneConsumer),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(fileStoreAddr, "display_name", "renamed"),
					resource.TestCheckResourceAttr(fileStoreAddr, "capacity_gib", "2048"),
					resource.TestCheckResourceAttr(fileStoreAddr, "consumers.%", "1"),
					resource.TestCheckResourceAttrSet(fileStoreAddr, "consumers.10.0.0.31.type"),
				),
			},
			{
				Config:   fileStoreConfig(name, "renamed", 2048, oneConsumer),
				PlanOnly: true,
			},
			{
				Config:            fileStoreConfig(name, "renamed", 2048, oneConsumer),
				ResourceName:      fileStoreAddr,
				ImportState:       true,
				ImportStateId:     name,
				ImportStateVerify: true,
				// `timeouts` is Terraform's own and `region`/`project` are
				// resolved from the provider block on a bare import id.
				ImportStateVerifyIgnore: []string{"timeouts", "region", "project"},
			},
			{
				Config:                  fileStoreConfig(name, "renamed", 2048, oneConsumer),
				ResourceName:            fileStoreAddr,
				ImportState:             true,
				ImportStateId:           "us-texas-1/demo/" + name,
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"timeouts"},
			},
		},
	})
}

// A shrink is refused before the apply, and deliberately not by replacing the
// store: replacing it would destroy the data it holds.
func TestAccFileStoreShrinkIsRefusedAtPlanTime(t *testing.T) {
	const name = "acc-shrink"

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy:             checkGone(fileStoreType, fileStorePath),
		Steps: []resource.TestStep{
			{
				Config: fileStoreConfig(name, "acceptance", 2048, oneConsumer),
			},
			{
				Config:      fileStoreConfig(name, "acceptance", 1024, oneConsumer),
				PlanOnly:    true,
				ExpectError: regexp.MustCompile(`cannot be shrunk`),
			},
		},
	})
}

// The class is fixed for the life of the store, so a change is a replacement
// rather than an update. Unlike a shrink, asking for a different class is
// asking for a different store.
func TestAccFileStoreShareTypeReplaces(t *testing.T) {
	const name = "acc-class"

	config := func(class string, capacity int) string {
		return fmt.Sprintf(`
resource %q "test" {
  id           = %q
  share_type   = %q
  capacity_gib = %d
}
`, fileStoreType, name, class, capacity)
	}

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy:             checkGone(fileStoreType, fileStorePath),
		Steps: []resource.TestStep{
			{
				Config: config("general", 512),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(fileStoreAddr, "share_type", "general"),
					// A store nobody can mount is the intended default, and it
					// is also the shape whose destroy needs no detach.
					resource.TestCheckResourceAttr(fileStoreAddr, "consumer_status.%", "0"),
				),
			},
			{
				// Refused without the permission: the store holds data.
				Config:      config("high-speed", 256),
				PlanOnly:    true,
				ExpectError: refusal("share_type", "file store"),
			},
			{
				// A new store, so a smaller capacity is not a shrink.
				Config: permitReplace(config("high-speed", 256), fileStoreType),
				Check:  resource.TestCheckResourceAttr(fileStoreAddr, "share_type", "high-speed"),
			},
		},
	})
}

// Every one of these is a 422 if it reaches the platform, which means a
// rejection partway through an approved apply. None of them makes a request.
func TestAccFileStoreConfigIsValidatedBeforeAnyRequest(t *testing.T) {
	store := func(consumers string) string {
		return fmt.Sprintf(`
resource %q "test" {
  id           = "acc-validate"
  share_type   = "general"
  capacity_gib = 512
  consumers    = %s
}
`, fileStoreType, consumers)
	}

	for _, tc := range []struct {
		name      string
		consumers string
		wantErr   *regexp.Regexp
	}{
		{
			name: "read-only is refused with the reason, not a bare rejection",
			consumers: `{
    "10.0.0.31" = {
      access_level = "ro"
    }
  }`,
			wantErr: regexp.MustCompile(`Only read-write access can be granted`),
		},
		{
			name: "an address entry has no interface to select",
			consumers: `{
    "10.0.0.31" = {
      network = { type = "ethernet" }
    }
  }`,
			wantErr: regexp.MustCompile(`no interface to select`),
		},
		{
			name: "a group entry has to name a fabric",
			consumers: `{
    "trainers" = {
      type = "instanceGroup"
    }
  }`,
			wantErr: regexp.MustCompile(`has to name a fabric`),
		},
		{
			name: "an InfiniBand interface has no MAC address",
			consumers: `{
    "trainers" = {
      type    = "instanceGroup"
      network = { type = "infiniband", mac_address = "aa:bb:cc:dd:ee:ff" }
    }
  }`,
			wantErr: regexp.MustCompile(`no MAC address`),
		},
		{
			name: "an Ethernet interface is not identified by a GUID",
			consumers: `{
    "trainers" = {
      type    = "instanceGroup"
      network = { type = "ethernet", guid = "0x0002c90300a1b2c3" }
    }
  }`,
			wantErr: regexp.MustCompile(`does not identify an Ethernet interface`),
		},
		{
			name: "one hardware identifier, not two",
			consumers: `{
    "trainers" = {
      type    = "instanceGroup"
      network = { type = "ethernet", mac_address = "aa:bb:cc:dd:ee:ff", guid = "0x0002c90300a1b2c3" }
    }
  }`,
			wantErr: regexp.MustCompile(`one hardware identifier, not two`),
		},
		{
			name: "an address key has to be a literal IPv4 address",
			consumers: `{
    "::ffff:10.0.0.31" = {}
  }`,
			wantErr: regexp.MustCompile(`not an IPv4 host address`),
		},
		{
			name: "an address key has to name one host",
			consumers: `{
    "0.0.0.0" = {}
  }`,
			wantErr: regexp.MustCompile(`not an IPv4 host address`),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resource.Test(t, resource.TestCase{
				ProtoV6ProviderFactories: protoV6,
				Steps: []resource.TestStep{{
					Config:      store(tc.consumers),
					PlanOnly:    true,
					ExpectError: tc.wantErr,
				}},
			})
		})
	}
}

// The catalog is what makes a class id discoverable. Without it a configuration
// either hard-codes one, which is a 422 in the region that does not have it, or
// is handed one out of band.
func TestAccShareTypes(t *testing.T) {
	config := fmt.Sprintf(`
data %q "all" {}

data %q "one" {
  id = "general"
}
`, shareTypesType, shareTypeType)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		Steps: []resource.TestStep{{
			Config: config,
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr(shareTypesAddr, "region", "us-texas-1"),
				resource.TestCheckResourceAttrSet(shareTypesAddr, "share_types.0.id"),
				resource.TestCheckTypeSetElemAttr(shareTypesAddr, "ids.*", "general"),
				resource.TestCheckTypeSetElemAttr(shareTypesAddr, "ids.*", "high-speed"),
				resource.TestCheckResourceAttr(shareTypeAddr, "id", "general"),
				// The capability map is the reason to read the catalog rather
				// than hard-code an id, so an empty one would make the data
				// source pointless even while it passed.
				resource.TestCheckResourceAttrSet(shareTypeAddr, "capabilities.protocol"),
			),
		}},
	})
}

// A class that does not exist is an error, not an empty result: the store about
// to be planned against it has nowhere to provision from.
func TestAccShareTypeNotFound(t *testing.T) {
	config := fmt.Sprintf(`
data %q "one" {
  id = "no-such-class"
}
`, shareTypeType)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		Steps: []resource.TestStep{{
			Config:      config,
			ExpectError: regexp.MustCompile(`No such storage share type`),
		}},
	})
}

// checkStoreHasNoConsumers reads the store straight from the fake and asserts
// the grant list is empty, so the check is against the platform rather than
// Terraform's own state.
func checkStoreHasNoConsumers(name string) resource.TestCheckFunc {
	return func(*terraform.State) error {
		status, body, err := readFake(fmt.Sprintf(
			"/v1/regions/us-texas-1/projects/demo/storage/file-stores/%s", name))
		if err != nil {
			return err
		}
		if status != http.StatusOK {
			return fmt.Errorf("reading %s answered %d, so the detach cannot be checked", name, status)
		}

		var read struct {
			Consumers []struct {
				Ref string `json:"ref"`
			} `json:"consumers"`
		}
		if err := json.Unmarshal(body, &read); err != nil {
			return err
		}
		if len(read.Consumers) != 0 {
			return fmt.Errorf("%s still lists %d consumer(s); the platform refuses to delete a "+
				"store anyone can mount, so the destroy would 409", name, len(read.Consumers))
		}
		return nil
	}
}

// Detaching every consumer through configuration empties the list upstream.
func TestAccFileStoreConsumersDetach(t *testing.T) {
	const name = "acc-detach"

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy:             checkGone(fileStoreType, fileStorePath),
		Steps: []resource.TestStep{
			{
				Config: fileStoreConfig(name, "acceptance", 512, twoConsumers),
				Check:  resource.TestCheckResourceAttr(fileStoreAddr, "consumers.%", "2"),
			},
			{
				Config: fileStoreConfig(name, "acceptance", 512, "{}"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(fileStoreAddr, "consumers.%", "0"),
					checkStoreHasNoConsumers(name),
				),
			},
		},
	})
}

// The three tests below drive the waiter's `failed` and unrecognised arms. An
// arm nothing exercises is a three-state waiter with extra lines.

// A store that settles `failed` fails the apply and names the reason, rather
// than polling until the timeout and reporting a wait that expired.
func TestAccFileStoreFailedIsReportedNotWaitedOut(t *testing.T) {
	config := fmt.Sprintf(`
resource %q "test" {
  id           = "acc-store-fails"
  display_name = "acceptance fake-settles-failed"
  share_type   = "general"
  capacity_gib = 512
}
`, fileStoreType)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		Steps: []resource.TestStep{{
			Config:      config,
			ExpectError: regexp.MustCompile(`(?s)did not become active.*failed`),
		}},
	})
}

// A state the contract does not name stops the wait immediately and says which
// value it was. Waiting it out against a draft API turns a clear answer into a
// slow one, and guessing which known state it resembles puts an invented value
// in state.
func TestAccFileStoreUnrecognisedStateNamesTheValue(t *testing.T) {
	config := fmt.Sprintf(`
resource %q "test" {
  id           = "acc-store-unknown"
  display_name = "acceptance fake-settles-unknown"
  share_type   = "general"
  capacity_gib = 512
}
`, fileStoreType)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		Steps: []resource.TestStep{{
			Config: config,
			// Matched on the value rather than on the sentence around it.
			// Naming the value is the whole claim, and Terraform hard-wraps
			// diagnostic detail at the terminal width, so any phrase long
			// enough to be distinctive is also long enough to be split across
			// lines by the time it reaches here.
			ExpectError: regexp.MustCompile(`"quiescing"`),
		}},
	})
}

// groupStoreConfig is a store with one instance-group consumer and the group,
// at the given size.
func groupStoreConfig(store, group string, size int) string {
	return instanceGroupConfig(group, size) + fmt.Sprintf(`
resource %q "test" {
  id           = %q
  share_type   = "general"
  capacity_gib = 512

  consumers = {
    (%s.test.id) = {
      type    = "instanceGroup"
      network = { type = "ethernet" }
    }
  }
}
`, fileStoreType, store, instanceGroupType)
}

// A group that grows is repaired by the platform on its own, so the plan stays
// clean while the grant catches up to the new members.
func TestAccFileStoreRepairedGroupPlansNothing(t *testing.T) {
	const group = "acc-trainers-repaired"
	status := "consumer_status." + group + "."

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy:             checkGone(fileStoreType, fileStorePath),
		Steps: []resource.TestStep{
			{Config: groupStoreConfig("acc-repaired", group, 2)},
			{Config: groupStoreConfig("acc-repaired", group, 3)},
			{
				Config: groupStoreConfig("acc-repaired", group, 3),
				Check:  resource.TestCheckResourceAttr(fileStoreAddr, status+"resolved_addresses.#", "3"),
			},
		},
	})
}

// With repair off, a group that grew reports its grant `failed` with
// GROUP_MEMBERSHIP_DRIFTED while the old members can still mount. The apply
// that scaled it succeeds, the next plan proposes sending the entry again,
// which is the only way to reach the new members, and the plan after that is
// clean. A green plan here would leave them unable to mount.
func TestAccFileStoreDriftedGroupIsResolvedAgain(t *testing.T) {
	const group = "acc-trainers-fake-drifts"
	status := "consumer_status." + group + "."
	grown := groupStoreConfig("acc-drifted", group, 3)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy:             checkGone(fileStoreType, fileStorePath),
		Steps: []resource.TestStep{
			{
				Config: groupStoreConfig("acc-drifted", group, 2),
				Check:  resource.TestCheckResourceAttr(fileStoreAddr, status+"state", "active"),
			},
			{
				// The store reads drift only after the group has grown, so
				// the plan after this apply is the one that reports it.
				Config:             grown,
				ExpectNonEmptyPlan: true,
			},
			{
				Config: grown,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(fileStoreAddr, status+"state", "active"),
					resource.TestCheckResourceAttr(fileStoreAddr, status+"resolved_addresses.#", "3"),
				),
			},
			{Config: grown, PlanOnly: true},
		},
	})
}

// A member the infrastructure service does not know cannot be vouched for, so
// the drift never clears and sending the entry again is refused. The apply has
// to land the rest of the change and stop proposing the re-send, or every
// later apply to the store fails.
func TestAccFileStoreUnresolvableDriftDoesNotBlockTheStore(t *testing.T) {
	const group = "acc-trainers-fake-unknown-host"
	grown := groupStoreConfig("acc-unknown-host", group, 3)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy:             checkGone(fileStoreType, fileStorePath),
		Steps: []resource.TestStep{
			{Config: groupStoreConfig("acc-unknown-host", group, 2)},
			{Config: grown, ExpectNonEmptyPlan: true},
			{Config: grown},
			{Config: grown, PlanOnly: true},
			{
				Config: strings.Replace(grown, "capacity_gib = 512", "capacity_gib = 1024", 1),
				Check:  resource.TestCheckResourceAttr(fileStoreAddr, "capacity_gib", "1024"),
			},
		},
	})
}

// A group that is gone, or whose id now names a different group, is the one
// comparison verdict the contract calls pager-worthy. Sending the entry again
// would grant whatever the id names now, so the plan fails instead.
func TestAccFileStoreUngrantableGroupFailsThePlan(t *testing.T) {
	const group = "acc-trainers-fake-ungrantable"

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy:             checkGone(fileStoreType, fileStorePath),
		Steps: []resource.TestStep{
			{
				Config:             groupStoreConfig("acc-ungrantable", group, 2),
				ExpectNonEmptyPlan: true,
				ExpectError:        regexp.MustCompile(`can no longer be granted`),
			},
			{
				// Removing the entry is the fix, so it must plan.
				Config: instanceGroupConfig(group, 2) + fileStoreConfig("acc-ungrantable", "acceptance", 512, "{}"),
			},
		},
	})
}

// A day-2 change can fail while the store stays `active`, and the contract
// says to treat a reason on the store as terminal. The apply has to fail.
func TestAccFileStoreFailedExpansionFailsTheApply(t *testing.T) {
	const name = "acc-expansion-fails"

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy:             checkGone(fileStoreType, fileStorePath),
		Steps: []resource.TestStep{
			{Config: fileStoreConfig(name, "acceptance fake-expansion-fails", 512, oneConsumer)},
			{
				// The destroy after this has to detach from a store whose
				// failed expansion keeps `reconciling` true for good.
				Config:      fileStoreConfig(name, "acceptance fake-expansion-fails", 1024, oneConsumer),
				ExpectError: regexp.MustCompile(`(?s)did not settle.*STORAGE_ERROR`),
			},
		},
	})
}

// A 422 on an update that also re-sends a drifted group is not necessarily
// about the group. Reading every 422 as the re-send's refusal reported a false
// warning and stopped proposing a re-send the platform never refused.
func TestAccFileStoreAnUnrelatedRefusalDoesNotSuppressDrift(t *testing.T) {
	const group = "acc-ceiling-fake-drifts"
	config := func(size, capacity int) string {
		return strings.Replace(strings.Replace(groupStoreConfig("acc-ceiling", group, size),
			`share_type   = "general"`, `share_type   = "high-speed"`, 1),
			"capacity_gib = 512", fmt.Sprintf("capacity_gib = %d", capacity), 1)
	}

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy:             checkGone(fileStoreType, fileStorePath),
		Steps: []resource.TestStep{
			{Config: config(2, 512)},
			{Config: config(3, 512), ExpectNonEmptyPlan: true},
			{Config: config(3, 9000), ExpectError: regexp.MustCompile(`exceeds`)},
			{Config: config(3, 512), PlanOnly: true, ExpectNonEmptyPlan: true},
			{Config: config(3, 512)},
			{Config: config(3, 512), PlanOnly: true},
		},
	})
}
