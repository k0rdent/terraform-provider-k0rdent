package acceptance

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/hashicorp/go-version"
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/zclconf/go-cty/cty"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/provider"
)

// `terraform query`, driven by the real CLI against the fake.
//
// The import guide's bulk adoption is one command: run the list blocks with
// -generate-config-out, review what it wrote, plan. This test does exactly
// that for every managed resource. It makes one of each in a project of its
// own, runs every list resource with -generate-config-out, keeps the blocks
// for what it made, and requires them to validate and to plan as imports that
// change nothing. That's the promise the list resources make, kept by the
// same CLI a tenant runs.
//
// The fake is shared with the rest of the suite, so the project, the service
// account and the webhook subscription other tests make show up in the
// organization-wide lists. Only this test's own objects are adopted: another
// test's could be deleted between the query and the plan.

// queryProject is the project this test's objects live in. The fake makes a
// scope for any project it's asked about, and a create still needs the
// project to exist, so the seed makes it first.
const queryProject = "acc-query"

// queryMinimum is the first Terraform with `terraform query`.
var queryMinimum = version.Must(version.NewVersion("1.14.0"))

// querySeed is one of each managed resource, in queryProject.
func querySeed(webhookURL string) string {
	return fmt.Sprintf(`
resource %[1]q "seed" {
  id              = %[2]q
  organization_id = %[3]q
  region          = "us-texas-1"
  display_name    = "Query"
}

resource %[4]q "seed" {
  id           = "acc-query-cluster"
  cluster_type = %[5]q
  display_name = "Query cluster"
  node_pool = {
    "gpu-workers" = { node_count = 1 }
  }
  depends_on = [%[1]s.seed]
}

resource %[6]q "seed" {
  id           = "acc-query-hosts"
  cluster_type = %[5]q
  display_name = "Query hosts"
  node_pool = {
    "gpu-workers" = { size = 1 }
  }
  depends_on = [%[1]s.seed]
}

resource %[7]q "seed" {
  id           = "acc-query-fabric"
  display_name = "Query fabric"
  evpn = {
    service_type = "l3vpn"
    vlan_id      = "4001"
    mtu          = 9000
  }
  depends_on = [%[1]s.seed]
}

resource %[8]q "seed" {
  id            = "acc-query-pods"
  network       = %[7]s.seed.id
  address_pool  = "/v1/regions/us-texas-1/networking/address-pools/tenant"
  prefix_length = 24
}

resource %[9]q "seed" {
  id           = "acc-query-filter"
  display_name = "Query filter"

  ingress {
    name               = "allow-ssh-from-office"
    action             = "permit"
    protocol           = "tcp"
    source_prefix      = "203.0.113.0/24"
    destination_prefix = "0.0.0.0/0"

    destination_port_range = "22"
  }
  depends_on = [%[1]s.seed]
}

resource %[10]q "seed" {
  id           = "acc-query-peer"
  vpc          = "vpc-acc-query-cluster"
  remote_vpc   = "/v1/regions/us-texas-1/projects/other/networking/vpcs/vpc-inference"
  display_name = "Query peering"
  depends_on   = [%[4]s.seed]
}

resource %[11]q "seed" {
  vpc             = "vpc-acc-query-cluster"
  security_groups = [%[9]s.seed.id]
  depends_on      = [%[4]s.seed]
}

resource %[12]q "seed" {
  id           = "acc-query-datasets"
  display_name = "Query datasets"
  share_type   = "general"
  capacity_gib = 1024
  consumers = {
    "10.0.0.31" = { mount_path = "/mnt/datasets" }
  }
  depends_on = [%[1]s.seed]
}

# A new subscription needs a signing secret. It's write-only, so the platform
# never answers it back and the generated configuration can't carry it: the
# adoption below plans against a block with no secret at all.
resource %[13]q "seed" {
  url               = %[14]q
  event_types       = ["io.k0rdent.cluster.ready.v1"]
  secret_wo         = sha256("acc-query")
  secret_wo_version = 1
}

output "webhook" {
  value = %[13]s.seed.id
}
`, projectType, queryProject, accOrg, clusterType, clusterTypeRef, instanceGroupType, networkType, subnetType,
		securityGroupType, peeringType, vpcBindingType, fileStoreType, webhookType, webhookURL)
}

// queryBlocks is one list block per managed resource, each narrowed to this
// test's objects where its config can say so.
func queryBlocks() map[string]string {
	return map[string]string{
		clusterType:       "",
		instanceGroupType: "",
		networkType:       "",
		subnetType:        "",
		securityGroupType: "",
		peeringType:       "",
		vpcBindingType:    "",
		fileStoreType:     "",
		projectType:       fmt.Sprintf("organization_id = %q", accOrg),
		webhookType:       "",
	}
}

func queryFile() string {
	var b strings.Builder
	blocks := queryBlocks()
	for _, typ := range sortedKeys(blocks) {
		fmt.Fprintf(&b, "list %q \"found\" {\n  provider = %s\n", typ, provider.ProviderTypeName)
		if blocks[typ] != "" {
			fmt.Fprintf(&b, "\n  config {\n    %s\n  }\n", blocks[typ])
		}
		b.WriteString("}\n\n")
	}
	return b.String()
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// queryWorkspace is a directory Terraform runs in, against the fake and the
// build under test.
type queryWorkspace struct {
	*migrationWorkspace
}

func newQueryWorkspace(t *testing.T, tf, build, body string) queryWorkspace {
	t.Helper()
	dir := t.TempDir()
	rc := filepath.Join(dir, "terraformrc")
	writeFile(t, rc, fmt.Sprintf("provider_installation {\n  dev_overrides {\n    %q = %q\n  }\n  direct {}\n}\n",
		provider.RegistryAddress(), build))
	writeFile(t, filepath.Join(dir, "main.tf"), fmt.Sprintf(`
terraform {
  required_providers {
    %s = { source = %q }
  }
}

provider %q {
  project = %q
}
`, provider.ProviderTypeName, provider.RegistryNamespace+"/"+provider.ProviderTypeName, provider.ProviderTypeName, queryProject)+body)
	return queryWorkspace{&migrationWorkspace{
		t:         t,
		terraform: tf,
		dir:       dir,
		env: append(quietEnviron(),
			"TF_CLI_CONFIG_FILE="+rc,
			"TF_IN_AUTOMATION=1",
			"TF_INPUT=0",
			"CHECKPOINT_DISABLE=1",
		),
	}}
}

// terraformForQuery is the terraform under test, skipping when it's older
// than `terraform query`. Under TF_ACC the suite's floor is lower, so an
// older binary is a skip rather than a failure.
func terraformForQuery(t *testing.T) string {
	t.Helper()
	tf := terraformForMigration(t)
	ws := &migrationWorkspace{t: t, terraform: tf, dir: t.TempDir(), env: quietEnviron()}
	out := ws.mustRun("version", "-json")
	var v struct {
		Version string `json:"terraform_version"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("terraform version -json: %v: %s", err, out)
	}
	got, err := version.NewVersion(v.Version)
	if err != nil {
		t.Fatal(err)
	}
	if got.LessThan(queryMinimum) {
		t.Skipf("terraform %s has no `terraform query`, which arrived in %s", got, queryMinimum)
	}
	return tf
}

// TestAccQueryGeneratesConfigThatPlansClean is the import guide's bulk
// adoption, for every managed resource.
func TestAccQueryGeneratesConfigThatPlansClean(t *testing.T) {
	if os.Getenv("TF_ACC") == "" {
		t.Skip("TF_ACC is not set")
	}
	tf := terraformForQuery(t)
	build := builtProvider(t)

	seed := newQueryWorkspace(t, tf, build, querySeed(webhookURL("query")))
	seed.mustRun("apply", "-auto-approve")
	t.Cleanup(func() { seed.mustRun("destroy", "-auto-approve") })
	ours := map[string]bool{
		queryProject: true,
		strings.TrimSpace(seed.mustRun("output", "-raw", "webhook")): true,
	}

	query := newQueryWorkspace(t, tf, build, "")
	writeFile(t, filepath.Join(query.dir, "found.tfquery.hcl"), queryFile())
	query.mustRun("init")
	query.mustRun("query", "-generate-config-out=generated.tf")
	generated, err := os.ReadFile(filepath.Join(query.dir, "generated.tf"))
	if err != nil {
		t.Fatal(err)
	}
	kept, types, err := adoptedBlocks(generated, func(identity map[string]string) bool {
		return identity["project"] == queryProject || ours[identity["id"]]
	})
	if err != nil {
		t.Fatalf("%v\n%s", err, generated)
	}

	// Every managed resource has to be among what was adopted: a resource
	// added without a list resource, or one whose list finds nothing it made,
	// fails here.
	for _, f := range provider.New("acc")().Resources(context.Background()) {
		var md resource.MetadataResponse
		f().Metadata(context.Background(), resource.MetadataRequest{ProviderTypeName: provider.ProviderTypeName}, &md)
		if types[md.TypeName] == 0 {
			t.Errorf("the query generated nothing this test made for %s", md.TypeName)
		}
	}

	// The subscription is adopted from a block with no secret, which is all
	// the generator can write. It has to plan as a plain import rather than
	// tripping the refusal a new subscription with no secret gets.
	if !strings.Contains(string(kept), fmt.Sprintf("resource %q", webhookType)) {
		t.Errorf("the adopted configuration has no %s", webhookType)
	}

	adopt := newQueryWorkspace(t, tf, build, "")
	writeFile(t, filepath.Join(adopt.dir, "generated.tf"), string(kept))
	adopt.mustRun("validate")
	adopt.mustRun("plan", "-out=adopt.tfplan")
	changes := plannedChanges(t, adopt.mustRun("show", "-json", "adopt.tfplan"))
	total := 0
	for _, n := range types {
		total += n
	}
	if len(changes) != total {
		t.Errorf("the plan has %d changes for %d adopted objects", len(changes), total)
	}
	webhooks := 0
	for _, c := range changes {
		if strings.HasPrefix(c.address, webhookType+".") {
			webhooks++
		}
		if !c.importing || len(c.actions) != 1 || c.actions[0] != "no-op" {
			t.Errorf("%s: plans %v (importing %v), want an import that changes nothing", c.address, c.actions, c.importing)
		}
	}
	if webhooks == 0 {
		t.Errorf("the plan adopts no %s, so the secretless adoption went unchecked", webhookType)
	}
	if t.Failed() {
		t.Logf("the generated configuration that was planned:\n%s", kept)
	}
}

type plannedChange struct {
	address   string
	actions   []string
	importing bool
}

func plannedChanges(t *testing.T, planJSON string) []plannedChange {
	t.Helper()
	var plan struct {
		ResourceChanges []struct {
			Address string `json:"address"`
			Change  struct {
				Actions   []string        `json:"actions"`
				Importing json.RawMessage `json:"importing"`
			} `json:"change"`
		} `json:"resource_changes"`
	}
	if err := json.Unmarshal([]byte(planJSON), &plan); err != nil {
		t.Fatalf("reading the plan: %v", err)
	}
	out := make([]plannedChange, 0, len(plan.ResourceChanges))
	for _, rc := range plan.ResourceChanges {
		out = append(out, plannedChange{
			address:   rc.Address,
			actions:   rc.Change.Actions,
			importing: len(rc.Change.Importing) > 0 && string(rc.Change.Importing) != "null",
		})
	}
	return out
}

// adoptedBlocks keeps the import blocks in a generated configuration whose
// identity keep accepts, and the resource blocks they import into, and
// counts what it kept by resource type.
func adoptedBlocks(src []byte, keep func(identity map[string]string) bool) ([]byte, map[string]int, error) {
	f, diags := hclsyntax.ParseConfig(src, "generated.tf", hcl.InitialPos)
	if diags.HasErrors() {
		return nil, nil, fmt.Errorf("the generated configuration doesn't parse: %s", diags.Error())
	}
	body, ok := f.Body.(*hclsyntax.Body)
	if !ok {
		return nil, nil, fmt.Errorf("the generated configuration isn't native syntax")
	}
	wanted := map[string]bool{}
	types := map[string]int{}
	for _, b := range body.Blocks {
		if b.Type != "import" {
			continue
		}
		to, ok := b.Body.Attributes["to"]
		if !ok {
			return nil, nil, fmt.Errorf("an import block at line %d has no `to`", b.DefRange().Start.Line)
		}
		traversal, d := hcl.AbsTraversalForExpr(to.Expr)
		if d.HasErrors() || len(traversal) != 2 {
			return nil, nil, fmt.Errorf("an import block at line %d imports to something that isn't a resource address", b.DefRange().Start.Line)
		}
		attr, ok := traversal[1].(hcl.TraverseAttr)
		if !ok {
			return nil, nil, fmt.Errorf("an import block at line %d imports to something that isn't a resource address", b.DefRange().Start.Line)
		}
		typ, name := traversal.RootName(), attr.Name
		identity, err := identityOf(b)
		if err != nil {
			return nil, nil, err
		}
		if keep(identity) {
			wanted[typ+"."+name] = true
			types[typ]++
		}
	}
	var out []byte
	for _, b := range body.Blocks {
		switch {
		case b.Type == "import":
			to := b.Body.Attributes["to"]
			traversal, _ := hcl.AbsTraversalForExpr(to.Expr) //nolint:errcheck // checked in the first pass
			attr, _ := traversal[1].(hcl.TraverseAttr)       //nolint:errcheck // as above
			if !wanted[traversal.RootName()+"."+attr.Name] {
				continue
			}
		case b.Type == "resource" && len(b.Labels) == 2:
			if !wanted[b.Labels[0]+"."+b.Labels[1]] {
				continue
			}
		}
		r := b.Range()
		out = append(out, src[r.Start.Byte:r.End.Byte]...)
		out = append(out, "\n\n"...)
	}
	return out, types, nil
}

// identityOf reads an import block's `identity` as strings.
func identityOf(b *hclsyntax.Block) (map[string]string, error) {
	attr, ok := b.Body.Attributes["identity"]
	if !ok {
		return nil, fmt.Errorf("the import block at line %d names no identity: a list result always has one", b.DefRange().Start.Line)
	}
	v, d := attr.Expr.Value(nil)
	if d.HasErrors() || !v.Type().IsObjectType() {
		return nil, fmt.Errorf("the import block at line %d has an identity that isn't a literal object", b.DefRange().Start.Line)
	}
	out := map[string]string{}
	for name, value := range v.AsValueMap() {
		if value.Type() == cty.String && value.IsKnown() && !value.IsNull() {
			out[name] = value.AsString()
		}
	}
	return out, nil
}

// adoptedBlocks is the filter TestAccQueryGeneratesConfigThatPlansClean
// trusts, so it's checked here without a fake: an import block and its
// resource are kept or dropped together, and the count is by type.
func TestAdoptedBlocksKeepsAnImportWithItsResource(t *testing.T) {
	src := []byte(strings.ReplaceAll(`
resource "NETWORK" "found_0" {
  id = "mine"
}

import {
  to = NETWORK.found_0
  identity = {
    id      = "mine"
    project = "acc-query"
  }
}

resource "NETWORK" "found_1" {
  id = "theirs"
}

import {
  to = NETWORK.found_1
  identity = {
    id      = "theirs"
    project = "demo"
  }
}
`, "NETWORK", networkType))
	kept, types, err := adoptedBlocks(src, func(identity map[string]string) bool { return identity["project"] == "acc-query" })
	if err != nil {
		t.Fatal(err)
	}
	if types[networkType] != 1 || len(types) != 1 {
		t.Errorf("counted %v, want one network", types)
	}
	if !strings.Contains(string(kept), `id = "mine"`) || !strings.Contains(string(kept), networkType+".found_0") {
		t.Errorf("dropped what was kept:\n%s", kept)
	}
	if strings.Contains(string(kept), "theirs") || strings.Contains(string(kept), "found_1") {
		t.Errorf("kept what was dropped:\n%s", kept)
	}
	if _, _, err := adoptedBlocks([]byte("import {\n  to = "+networkType+".x\n  id = \"x\"\n}\n"), func(map[string]string) bool { return true }); err == nil {
		t.Error("an import block with no identity passed")
	}
}
