package acceptance

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	fwresource "github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-testing/config"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/provider"
)

// importCase is one managed resource's import, end to end.
//
// Every resource gets the same five proofs, so a resource can't quietly have a
// weaker import than the others:
//
//   - the CLI import, compared attribute by attribute with what the apply wrote
//   - an `import` block naming the string id, whose plan must be a no-op
//   - an `import` block naming the identity, whose plan must be a no-op and
//     must carry the same identity the apply recorded
//   - `terraform plan -generate-config-out`, whose output has to validate and
//     plan as a no-op, because the import guide tells tenants to start there
//   - an applied adoption through the identity form, then a plan that has to be
//     empty: the plan after the import
type importCase struct {
	// config holds the resource at <resourceType>.test, plus whatever it
	// needs to exist. It's written the way the platform reports the object,
	// which is what "a resource that genuinely matches its configuration"
	// means: a value only the configuration can know, such as a sparse
	// override set, has its own test and its own line in the import guide.
	config string
	// importConfig is the configuration the imports and the adoption plan
	// against, when it isn't config. A credential is created with a key to
	// seal its secret to, and an imported one has no secret and no key, so
	// its configuration leaves the key out.
	importConfig string
	resourceType string
	importID     string
	// identity is the `identity = {...}` an adoption writes, as HCL values.
	identity map[string]string
	// verifyIgnore is for the CLI import only, which compares raw state.
	verifyIgnore []string
	// verifyBy names the attribute that identifies the row when it isn't id.
	verifyBy string
	// fromState gives the import id and the identity from the state the
	// first step left, for a resource whose id the platform makes, so no
	// literal can name it before the apply. importID and identity are left
	// out then.
	fromState    func(attrs map[string]string) (string, map[string]string)
	checkDestroy resource.TestCheckFunc
	// seed, when set, is what the first step applies instead of config: the
	// object as somebody else made it, with something config doesn't manage.
	seed string
}

func (c importCase) addr() string { return c.resourceType + ".test" }

// imported is the configuration an import plans against.
func (c importCase) imported() string {
	if c.importConfig != "" {
		return c.importConfig
	}
	return c.config
}

// adopted is the configuration with the resource under test renamed, so a
// `removed` block can let go of the original without destroying it and an
// `import` block can take the same object under the new name.
func (c importCase) adopted() string {
	return strings.Replace(c.imported(),
		fmt.Sprintf("resource %q \"test\"", c.resourceType),
		fmt.Sprintf("resource %q \"adopted\"", c.resourceType), 1)
}

func (c importCase) identityHCL() string {
	keys := make([]string, 0, len(c.identity))
	for k := range c.identity {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "    %s = %q\n", k, c.identity[k])
	}
	return b.String()
}

func (c importCase) adoption() string {
	return c.adopted() + fmt.Sprintf(`
removed {
  from = %s.test
  lifecycle { destroy = false }
}
import {
  to = %s.adopted
  identity = {
%s  }
}
`, c.resourceType, c.resourceType, c.identityHCL())
}

func (c importCase) run(t *testing.T) {
	t.Helper()
	adoptedAddr := c.resourceType + ".adopted"

	// An id the platform makes is known only once the first step has made
	// it, so a case that names one through fromState reads it from that
	// step's state, and every later step asks for it then.
	var created map[string]string
	importID := func(*terraform.State) (string, error) {
		if c.fromState == nil {
			return c.importID, nil
		}
		if created == nil {
			return "", fmt.Errorf("%s was never created, so there is no id to import", c.addr())
		}
		id, _ := c.fromState(created)
		return id, nil
	}
	create := resource.TestStep{Config: c.config}
	if c.seed != "" {
		create.Config = c.seed
	}
	adoption := resource.TestStep{
		Config: c.adoption(),
		ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
			plancheck.ExpectResourceAction(adoptedAddr, plancheck.ResourceActionNoop),
		}},
	}
	if c.fromState != nil {
		create.Check = func(s *terraform.State) error {
			rs, ok := s.RootModule().Resources[c.addr()]
			if !ok {
				return fmt.Errorf("%s is not in state", c.addr())
			}
			created = rs.Primary.Attributes
			return nil
		}
		dir := t.TempDir()
		adoption.Config = ""
		adoption.ConfigFile = func(config.TestStepConfigRequest) string {
			late := c
			_, late.identity = c.fromState(created)
			path := filepath.Join(dir, "adoption.tf")
			if err := os.WriteFile(path, []byte(late.adoption()), 0o600); err != nil {
				t.Fatal(err)
			}
			return path
		}
	}

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy:             c.checkDestroy,
		Steps: []resource.TestStep{
			create,
			{
				Config:                               c.imported(),
				ResourceName:                         c.addr(),
				ImportState:                          true,
				ImportStateIdFunc:                    importID,
				ImportStateVerify:                    true,
				ImportStateVerifyIgnore:              c.verifyIgnore,
				ImportStateVerifyIdentifierAttribute: c.verifyBy,
			},
			{
				Config:            c.imported(),
				ResourceName:      c.addr(),
				ImportState:       true,
				ImportStateIdFunc: importID,
				ImportStateKind:   resource.ImportBlockWithID,
			},
			{
				Config:          c.imported(),
				ResourceName:    c.addr(),
				ImportState:     true,
				ImportStateKind: resource.ImportBlockWithResourceIdentity,
			},
			{
				Config:            c.imported(),
				ResourceName:      c.addr(),
				ImportState:       true,
				ImportStateIdFunc: importID,
				ImportStateKind:   resource.ImportBlockWithID,
				GenerateConfig:    true,
			},
			adoption,
			{Config: c.adopted(), PlanOnly: true},
		},
	})
}

// scoped is the identity of a resource addressed by region, project and id.
func scoped(id string) map[string]string {
	return map[string]string{"region": "us-texas-1", "project": "demo", "id": id}
}

// importCases is every managed resource's import case, keyed by type name.
// TestEveryResourceHasAnImportCase holds it to the provider's resource list,
// so a resource can't be registered without one.
func importCases() map[string]func() importCase {
	return map[string]func() importCase{
		clusterType: func() importCase {
			const name = "acc-import-cluster"
			return importCase{
				// Every pool named, at the counts the platform holds. A
				// configuration naming a subset is
				// TestAccImportClusterSparsePools. The SSH users are read
				// from the platform, so an import fills them in.
				config: fmt.Sprintf(`
resource %q "test" {
  id           = %q
  cluster_type = %q
  display_name = "acceptance"
  node_pool = {
    "control-plane" = { node_count = 1 }
    "gpu-workers"   = { node_count = 2 }
  }
  ssh_access = [%s
  ]
}
`, clusterType, name, clusterTypeRef, sshZoe+sshAdam),
				resourceType: clusterType,
				importID:     "us-texas-1/demo/" + name,
				identity:     scoped(name),
				checkDestroy: checkGone(clusterType, clusterPath),
			}
		},
		instanceGroupType: func() importCase {
			const name = "acc-import-hosts"
			return importCase{
				config:       instanceGroupConfig(name, 1),
				resourceType: instanceGroupType,
				importID:     "us-texas-1/demo/" + name,
				identity:     scoped(name),
				checkDestroy: checkGone(instanceGroupType, instanceGroupPath),
			}
		},
		networkType: func() importCase {
			const name = "acc-import-fabric"
			return importCase{
				// A VLAN id named outright. `vlan_id = "auto"` is a value
				// only the configuration knows, and has its own test and
				// line in the guide.
				config: strings.Replace(evpnConfig(name, "acceptance"),
					`vlan_id      = "auto"`, `vlan_id      = "4001"`, 1),
				resourceType: networkType,
				importID:     "us-texas-1/demo/" + name,
				identity:     scoped(name),
				checkDestroy: checkGone(networkType, networkPath),
			}
		},
		subnetType: func() importCase {
			const network, name = "acc-import-net", "acc-import-sub"
			identity := scoped(name)
			identity["network"] = network
			return importCase{
				config:       subnetConfig(network, name),
				resourceType: subnetType,
				importID:     "us-texas-1/demo/" + network + "/" + name,
				identity:     identity,
				checkDestroy: resource.ComposeAggregateTestCheckFunc(
					checkGone(subnetType, subnetPath),
					checkGone(networkType, networkPath),
				),
			}
		},
		securityGroupType: func() importCase {
			const name = "acc-import-filter"
			return importCase{
				config:       securityGroupConfig(name, true),
				resourceType: securityGroupType,
				importID:     "us-texas-1/demo/" + name,
				identity:     scoped(name),
				checkDestroy: checkGone(securityGroupType, securityGroupPath),
			}
		},
		peeringType: func() importCase {
			const cluster, name = "acc-import-peer-owner", "to-inference"
			const remote = "/v1/regions/us-texas-1/projects/other/networking/vpcs/vpc-inference"
			identity := scoped(name)
			identity["vpc"] = "vpc-" + cluster
			return importCase{
				config:       peeringConfig(cluster, name, remote),
				resourceType: peeringType,
				importID:     "us-texas-1/demo/vpc-" + cluster + "/" + name,
				identity:     identity,
				checkDestroy: resource.ComposeAggregateTestCheckFunc(
					checkGone(peeringType, peeringPath),
					checkGone(clusterType, clusterPath),
				),
			}
		},
		vpcBindingType: func() importCase {
			const cluster, group = "acc-import-bind-owner", "acc-import-bind-filter"
			return importCase{
				config:       vpcBindingConfig(cluster, group, true),
				resourceType: vpcBindingType,
				importID:     "us-texas-1/demo/vpc-" + cluster,
				identity:     map[string]string{"region": "us-texas-1", "project": "demo", "vpc": "vpc-" + cluster},
				verifyBy:     "vpc",
				checkDestroy: checkGone(clusterType, clusterPath),
			}
		},
		fileStoreType: func() importCase {
			const name = "acc-import-datasets"
			return importCase{
				config:       fileStoreConfig(name, "acceptance", 1024, oneConsumer),
				resourceType: fileStoreType,
				importID:     "us-texas-1/demo/" + name,
				identity:     scoped(name),
				verifyIgnore: []string{"timeouts"},
				checkDestroy: checkGone(fileStoreType, fileStorePath),
			}
		},
		webhookType: func() importCase {
			// Made with a secret, and adopted by a configuration that
			// leaves it out. The platform never answers a secret back, so an
			// import can't know it, and a configuration naming one plans a
			// rotation to it: TestAccWebhookSubscriptionImportThenSecret.
			events := []string{"io.k0rdent.cluster.ready.v1"}
			return importCase{
				seed:         webhookConfigFixedSecret(webhookURL("import"), events, "import"),
				config:       webhookConfig(webhookURL("import"), events, 0, ""),
				resourceType: webhookType,
				fromState: func(a map[string]string) (string, map[string]string) {
					return a["id"], map[string]string{"id": a["id"]}
				},
				verifyIgnore: []string{"secret_wo_version"},
				checkDestroy: checkWebhookGone,
			}
		},
		projectType: func() importCase {
			const id = "acc-import-project"
			return importCase{
				config:       projectConfig(id, "Import", "imported every way"),
				resourceType: projectType,
				importID:     accOrg + "/" + id,
				identity:     map[string]string{"organization_id": accOrg, "id": id},
				verifyIgnore: []string{"timeouts"},
				checkDestroy: checkGone(projectType, projectPath),
			}
		},
	}
}

// TestAccImport runs every resource's case, one subtest each, so
// `-run 'TestAccImport/<type name>'` picks out one.
func TestAccImport(t *testing.T) {
	cases := importCases()
	names := make([]string, 0, len(cases))
	for name := range cases {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		t.Run(name, func(t *testing.T) { cases[name]().run(t) })
	}
}

// A configuration that names a subset of a cluster's pools doesn't plan clean
// straight after an import, and the guide says so. The import can't know which
// pools the configuration manages, so it adopts all of them, and the plan
// narrows the managed set to what the configuration names. This pins the
// declared shape: an update, never a replacement, and once applied, nothing
// left to plan.
func TestAccImportClusterSparsePools(t *testing.T) {
	const name = "acc-import-sparse"
	config := clusterConfig(name, 2)
	seed := strings.Replace(config, `"test"`, `"seed"`, 1)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy:             checkGone(clusterType, clusterPath),
		Steps: []resource.TestStep{
			{Config: seed},
			{
				Config: adopt(config, clusterType, "us-texas-1/demo/"+name),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(clusterAddr, plancheck.ResourceActionUpdate),
				}},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(clusterAddr, "node_pool.%", "1"),
					resource.TestCheckResourceAttr(clusterAddr, "node_pool.gpu-workers.node_count", "2"),
					// The pool the configuration left out is still there.
					resource.TestCheckResourceAttr(clusterAddr, "node_count", "3"),
				),
			},
			{Config: config, PlanOnly: true},
		},
	})
}

// A cluster pinned to a version that isn't the ClusterType's current one keeps
// its pin on import, because there the unversioned spelling would mean a
// different version. The pinned configuration plans clean.
func TestAccImportKeepsAnOlderClusterTypePin(t *testing.T) {
	const name = "acc-import-pinned"
	importCase{
		config: fmt.Sprintf(`
resource %q "test" {
  id           = %q
  cluster_type = %q
  node_pool = {
    "control-plane" = { node_count = 1 }
    "gpu-workers"   = { node_count = 2 }
  }
}
`, clusterType, name, clusterTypeRef+"/versions/22222222-2222-2222-2222-222222222222"),
		resourceType: clusterType,
		importID:     "us-texas-1/demo/" + name,
		identity:     scoped(name),
		checkDestroy: checkGone(clusterType, clusterPath),
	}.run(t)
}

// notImportable names, by type, a managed resource that has no import, and
// where the import guide says why. It's empty: every resource imports. A
// resource added to it has to be named in the guide, so the gap is written
// down where a tenant looks before anyone finds it in a failed import.
var notImportable = map[string]string{}

// This needs no fake, so it runs under `make test` as well as `make testacc`:
// a resource registered without an import case, and not named as having no
// import, fails the build.
func TestEveryResourceHasAnImportCase(t *testing.T) {
	cases := importCases()
	guide, err := os.ReadFile(filepath.Join("..", "..", "docs", "guides", "import.md"))
	if err != nil {
		t.Fatal(err)
	}
	registered := map[string]bool{}
	for _, newResource := range provider.New("test")().Resources(context.Background()) {
		var md fwresource.MetadataResponse
		newResource().Metadata(context.Background(),
			fwresource.MetadataRequest{ProviderTypeName: provider.ProviderTypeName}, &md)
		registered[md.TypeName] = true
		_, tested := cases[md.TypeName]
		_, named := notImportable[md.TypeName]
		switch {
		case tested && named:
			t.Errorf("%s has an import case and is named as not importable: drop one", md.TypeName)
		case !tested && !named:
			t.Errorf("%s has no import case in importCases, and isn't named in notImportable", md.TypeName)
		case named && !strings.Contains(string(guide), md.TypeName):
			t.Errorf("%s has no import, and docs/guides/import.md doesn't name it", md.TypeName)
		}
	}
	for name := range cases {
		if !registered[name] {
			t.Errorf("importCases has %s, which the provider doesn't register", name)
		}
	}
}

// The import guide's bulk adoption, as written there: one `import` block with
// `for_each`, naming each cluster by an identity that leaves the scope to the
// provider. Both clusters have to adopt with no change, and stay that way.
func TestAccImportForEachByIdentity(t *testing.T) {
	clusters := fmt.Sprintf(`
locals {
  clusters = toset(["acc-import-each-a", "acc-import-each-b"])
}

resource %q "LABEL" {
  for_each     = local.clusters
  id           = each.key
  cluster_type = %q
  node_pool = {
    "control-plane" = { node_count = 1 }
    "gpu-workers"   = { node_count = 2 }
  }
}
`, clusterType, clusterTypeRef)
	seed := strings.Replace(clusters, "LABEL", "seed", 1)
	adopted := strings.Replace(clusters, "LABEL", "adopted", 1)
	adoption := adopted + fmt.Sprintf(`
removed {
  from = %s.seed
  lifecycle { destroy = false }
}

import {
  for_each = local.clusters
  to       = %s.adopted[each.key]
  identity = {
    id = each.key
  }
}
`, clusterType, clusterType)
	// The harness's state reader refuses a for_each instance, so the last
	// step destroys both clusters through the configuration and checks the
	// fake directly, leaving the harness nothing to read.
	gone := func(*terraform.State) error {
		for _, id := range []string{"acc-import-each-a", "acc-import-each-b"} {
			path := clusterPath(map[string]string{"region": "us-texas-1", "project": "demo", "id": id})
			status, _, err := readFake(path)
			if err != nil {
				return err
			}
			if status != http.StatusNotFound {
				return fmt.Errorf("%s still resolves: the fake answered %d", id, status)
			}
		}
		return nil
	}
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		Steps: []resource.TestStep{
			{Config: seed},
			{
				Config: adoption,
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(clusterType+`.adopted["acc-import-each-a"]`, plancheck.ResourceActionNoop),
					plancheck.ExpectResourceAction(clusterType+`.adopted["acc-import-each-b"]`, plancheck.ResourceActionNoop),
				}},
			},
			{Config: adopted, PlanOnly: true},
			{Config: "locals {}", Check: gone},
		},
	})
}

// A configuration that pins the version its ClusterType would bind anyway is
// the one spelling the import can't guess: the import records the
// unversioned reference, so the plan after it updates `cluster_type` in place
// to the pinned spelling. It mustn't be refused as a replacement, on either
// resource, and once applied nothing is left to plan. The binding doesn't
// move, so that plan moves `cluster_type` and nothing else: a cluster's
// `node_count` stays known.
func TestAccImportAPinToTheCurrentVersion(t *testing.T) {
	pinned := clusterTypeRef + "/versions/33333333-3333-3333-3333-333333333333"
	cases := map[string]string{
		clusterType: fmt.Sprintf(`
resource %q "test" {
  id           = "acc-import-pin-current"
  cluster_type = %q
  node_pool = {
    "control-plane" = { node_count = 1 }
    "gpu-workers"   = { node_count = 2 }
  }
}
`, clusterType, pinned),
		instanceGroupType: strings.Replace(instanceGroupConfig("acc-import-pin-current-hosts", 1),
			fmt.Sprintf("%q", clusterTypeRef), fmt.Sprintf("%q", pinned), 1),
	}
	paths := map[string]func(map[string]string) string{
		clusterType: clusterPath, instanceGroupType: instanceGroupPath,
	}
	for typ, config := range cases {
		t.Run(typ, func(t *testing.T) {
			addr := typ + ".test"
			id := "acc-import-pin-current"
			if typ == instanceGroupType {
				id += "-hosts"
			}
			seed := strings.Replace(config, `"test"`, `"seed"`, 1)
			resource.Test(t, resource.TestCase{
				ProtoV6ProviderFactories: protoV6,
				CheckDestroy:             checkGone(typ, paths[typ]),
				Steps: []resource.TestStep{
					{Config: seed},
					{
						Config:           adopt(config, typ, "us-texas-1/demo/"+id),
						ConfigPlanChecks: updates(addr, expectOnly(addr, "cluster_type")),
						Check:            resource.TestCheckResourceAttr(addr, "cluster_type", pinned),
					},
					{Config: config, PlanOnly: true},
				},
			})
		})
	}
}

// A binding configured as `[]` holds the VPC's own default group, which is
// what the contract says `[]` means. The import can see that, from the group's
// owner, so it records `[]` and that configuration plans clean.
func TestAccImportABindingAtItsDefault(t *testing.T) {
	const cluster, group = "acc-import-default-owner", "acc-import-default-filter"
	importCase{
		config:       vpcBindingConfig(cluster, group, false),
		resourceType: vpcBindingType,
		importID:     "us-texas-1/demo/vpc-" + cluster,
		identity:     map[string]string{"region": "us-texas-1", "project": "demo", "vpc": "vpc-" + cluster},
		verifyBy:     "vpc",
		checkDestroy: checkGone(clusterType, clusterPath),
	}.run(t)
}
