package acceptance

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/provider"
)

// State migration, driven by the real Terraform CLI against a state file
// written the way an earlier build left it.
//
// The unit tests in internal/provider call UpgradeResourceState and
// MoveResourceState directly. These check the part they can't: that Terraform
// itself reads the stored schema version, calls the upgrade, writes the result
// back at the new version, and plans a `moved` block across type names. Both
// run with -refresh=false, so neither needs the fake. The API they're pointed
// at fails the test on any request, because a migration that has to ask the
// platform for something isn't a migration of the state file.
//
// They run whenever a terraform binary is available, and they fail rather than
// skip under TF_ACC, where one always should be.

// preReleaseProviderType is the provider type the cluster and the instance
// group were registered under before the name settled. It's history, so it's
// spelled out.
const preReleaseProviderType = "k0rdentai"

func terraformForMigration(t *testing.T) string {
	t.Helper()
	bin := os.Getenv("TF_ACC_TERRAFORM_PATH")
	if bin == "" {
		if found, err := exec.LookPath("terraform"); err == nil {
			bin = found
		}
	}
	if bin == "" {
		if os.Getenv("TF_ACC") != "" {
			t.Fatal("TF_ACC is set and no terraform binary was found")
		}
		t.Skip("no terraform binary on PATH")
	}
	return bin
}

// builtProvider builds the provider into a directory of its own, for a
// dev_overrides block to point at. The build cache makes the second one cheap.
func builtProvider(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "terraform-provider-"+provider.ProviderTypeName)
	if out, err := exec.Command("go", "build", "-o", bin, "../..").CombinedOutput(); err != nil {
		t.Fatalf("building the provider: %v: %s", err, out)
	}
	return dir
}

// migrationWorkspace is a directory holding the configuration and the state,
// with a CLI configuration pointing Terraform at the build under test.
type migrationWorkspace struct {
	t         *testing.T
	terraform string
	dir       string
	env       []string
	requests  *atomic.Int32
}

func newMigrationWorkspace(t *testing.T, config, state string) *migrationWorkspace {
	t.Helper()
	tf := terraformForMigration(t)
	build := builtProvider(t)
	dir := t.TempDir()

	var requests atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, "a state migration has no business calling the API", http.StatusTeapot)
	}))
	t.Cleanup(api.Close)

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
  endpoint = %q
  region   = "us-texas-1"
  project  = "demo"
}
`, provider.ProviderTypeName, provider.RegistryNamespace+"/"+provider.ProviderTypeName, provider.ProviderTypeName, api.URL)+config)
	writeFile(t, filepath.Join(dir, "terraform.tfstate"), state)

	return &migrationWorkspace{
		t:         t,
		terraform: tf,
		dir:       dir,
		requests:  &requests,
		env: append(quietEnviron(),
			"TF_CLI_CONFIG_FILE="+rc,
			"TF_IN_AUTOMATION=1",
			"TF_INPUT=0",
			"CHECKPOINT_DISABLE=1",
			provider.EnvVar("CLIENT_ID")+"=sa-terraform-provider@vendor.iam",
			provider.EnvVar("CLIENT_SECRET")+"=never-sent",
		),
	}
}

// quietEnviron is the environment without Terraform's logging. Under TF_ACC
// the harness turns trace logging on for the suite, and here it would bury a
// failure's output and land in the JSON a plan is read back from.
func quietEnviron() []string {
	var out []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "TF_LOG") && !strings.HasPrefix(kv, "TF_ACC_LOG_PATH=") {
			out = append(out, kv)
		}
	}
	return out
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// run runs terraform and returns its exit code and output.
func (w *migrationWorkspace) run(args ...string) (int, string) {
	w.t.Helper()
	cmd := exec.Command(w.terraform, append(args, "-no-color")...) //nolint:gosec // the terraform under test
	cmd.Dir = w.dir
	cmd.Env = w.env
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	if exit, ok := err.(*exec.ExitError); ok {
		return exit.ExitCode(), out.String()
	}
	if err != nil {
		w.t.Fatalf("terraform %s: %v", strings.Join(args, " "), err)
	}
	return 0, out.String()
}

func (w *migrationWorkspace) mustRun(args ...string) string {
	w.t.Helper()
	code, out := w.run(args...)
	if code != 0 {
		w.t.Fatalf("terraform %s exited %d:\n%s", strings.Join(args, " "), code, out)
	}
	return out
}

func (w *migrationWorkspace) noRequestReachedTheAPI() {
	w.t.Helper()
	if n := w.requests.Load(); n > 0 {
		w.t.Errorf("the migration sent %d request(s) to the API", n)
	}
}

type storedInstance struct {
	SchemaVersion int64          `json:"schema_version"`
	Attributes    map[string]any `json:"attributes"`
}

func (w *migrationWorkspace) storedInstance(resourceType, name string) storedInstance {
	w.t.Helper()
	raw, err := os.ReadFile(filepath.Join(w.dir, "terraform.tfstate"))
	if err != nil {
		w.t.Fatal(err)
	}
	var st struct {
		Resources []struct {
			Type      string           `json:"type"`
			Name      string           `json:"name"`
			Instances []storedInstance `json:"instances"`
		} `json:"resources"`
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		w.t.Fatal(err)
	}
	for _, r := range st.Resources {
		if r.Type == resourceType && r.Name == name && len(r.Instances) == 1 {
			return r.Instances[0]
		}
	}
	w.t.Fatalf("the state holds no single %s.%s:\n%s", resourceType, name, raw)
	return storedInstance{}
}

func stateFile(providerAddress, resourceType, name string, attributes string) string {
	return fmt.Sprintf(`{
  "version": 4,
  "terraform_version": "1.10.0",
  "serial": 1,
  "lineage": "6f1c2a8e-4d3b-4c5a-9e7f-0a1b2c3d4e5f",
  "outputs": {},
  "resources": [
    {
      "mode": "managed",
      "type": %q,
      "name": %q,
      "provider": "provider[\"%s\"]",
      "instances": [
        {
          "schema_version": 0,
          "attributes": %s,
          "sensitive_attributes": []
        }
      ]
    }
  ]
}
`, resourceType, name, providerAddress, attributes)
}

// A project as a build before schema versions were declared stored it, at
// version 0, carrying an attribute that build had and this one doesn't. The
// first plan upgrades it with no change to propose, and the first apply writes
// it back at the current version with every value it had. The provider block
// has a region, and the project's stays null, since nothing has read it yet.
func TestAStateFromBeforeVersionsUpgradesUnderTerraform(t *testing.T) {
	t.Parallel()
	project := provider.ResourceName("project")

	w := newMigrationWorkspace(t, fmt.Sprintf(`
resource %q "demo" {
  id              = "tf-0123456789abcdef"
  organization_id = "org-demo"
  display_name    = "Demo"
  description     = "created by the demo"
}
`, project), stateFile(provider.RegistryAddress(), project, "demo", `{
            "allow_replace": null,
            "description": "created by the demo",
            "display_name": "Demo",
            "id": "tf-0123456789abcdef",
            "organization_id": "org-demo",
            "retired_attribute": "written by an older build",
            "self_link": "/v1/organizations/org-demo/projects/tf-0123456789abcdef",
            "status": "active",
            "timeouts": null
          }`))

	if code, out := w.run("plan", "-refresh=false", "-detailed-exitcode"); code != 0 {
		t.Fatalf("the plan over a version 0 state exited %d, want 0 (no changes):\n%s", code, out)
	}
	w.mustRun("apply", "-refresh=false", "-auto-approve")

	got := w.storedInstance(project, "demo")
	// Version 2 since the k0rdent API v1.3.0-dev.4 added the project's region.
	if got.SchemaVersion != 2 {
		t.Errorf("the state was written back at schema version %d, want 2", got.SchemaVersion)
	}
	// A refresh fills the region from the project. Without one, as here, it
	// stays null rather than taking the provider's, which would be a change
	// to a project no apply can make.
	if region, set := got.Attributes["region"]; set && region != nil {
		t.Errorf("region = %v, want null without a refresh", region)
	}
	for name, want := range map[string]any{
		"id":              "tf-0123456789abcdef",
		"organization_id": "org-demo",
		"display_name":    "Demo",
		"description":     "created by the demo",
		"status":          "active",
		"self_link":       "/v1/organizations/org-demo/projects/tf-0123456789abcdef",
	} {
		if got.Attributes[name] != want {
			t.Errorf("%s = %v, want %v", name, got.Attributes[name], want)
		}
	}
	if _, kept := got.Attributes["retired_attribute"]; kept {
		t.Error("an attribute this build doesn't have was written back")
	}
	w.noRequestReachedTheAPI()
}

// A cluster the pre-release build stored under its old type name. Terraform
// won't plan while the state names a provider it can't resolve, so the state
// is first handed to this provider, and then a `moved` block lands it on the
// current type. Terraform's own `state mv` refuses this move outright, with
// "resource types don't match", so this is the route.
func TestAMovedBlockFromThePreReleaseTypeUnderTerraform(t *testing.T) {
	t.Parallel()
	cluster := provider.ResourceName("kubernetes_cluster")
	oldType := preReleaseProviderType + "_kubernetes_cluster"
	oldProvider := "registry.terraform.io/example-user/" + preReleaseProviderType

	w := newMigrationWorkspace(t, fmt.Sprintf(`
moved {
  from = %s.training
  to   = %s.training
}

resource %q "training" {
  id           = "tf-demo"
  cluster_type = "/v1/regions/us-texas-1/compute/cluster-types/gpu-training"
  display_name = "Training"
  tags         = { team = "ml" }
  node_pool = {
    gpu-workers = { node_count = 2 }
  }
}
`, oldType, cluster, cluster), stateFile(oldProvider, oldType, "training", `{
            "cluster_type": "/v1/regions/us-texas-1/compute/cluster-types/gpu-training",
            "cluster_type_resolved": "/v1/regions/us-texas-1/compute/cluster-types/gpu-training/versions/3",
            "description": null,
            "display_name": "Training",
            "id": "tf-demo",
            "node_count": 2,
            "node_pool": {"gpu-workers": {"node_count": 2}},
            "self_link": "/v1/regions/us-texas-1/projects/demo/compute/clusters/tf-demo",
            "state": "ready",
            "tags": {"team": "ml"}
          }`))

	if code, out := w.run("state", "mv", oldType+".training", cluster+".training"); code == 0 ||
		!strings.Contains(out, "resource types don't match") {
		t.Errorf("terraform state mv across type names exited %d, and the docs say it refuses:\n%s", code, out)
	}
	w.mustRun("state", "replace-provider", "-auto-approve", oldProvider, provider.RegistryAddress())
	w.mustRun("plan", "-refresh=false", "-out=moved.tfplan")
	shown := w.mustRun("show", "-json", "moved.tfplan")

	var plan struct {
		ResourceChanges []struct {
			Address         string `json:"address"`
			PreviousAddress string `json:"previous_address"`
			Change          struct {
				Actions []string       `json:"actions"`
				Before  map[string]any `json:"before"`
			} `json:"change"`
		} `json:"resource_changes"`
	}
	if err := json.Unmarshal([]byte(shown[strings.Index(shown, "{"):]), &plan); err != nil {
		t.Fatalf("reading the plan: %v\n%s", err, shown)
	}
	if len(plan.ResourceChanges) != 1 {
		t.Fatalf("the plan changes %d resources, want the one moved cluster:\n%s", len(plan.ResourceChanges), shown)
	}
	rc := plan.ResourceChanges[0]
	if rc.Address != cluster+".training" || rc.PreviousAddress != oldType+".training" {
		t.Errorf("the plan moves %q to %q, want %s.training to %s.training", rc.PreviousAddress, rc.Address, oldType, cluster)
	}
	// An update in place, for the region and project the pre-release build
	// never stored. Never a replacement: that would destroy the cluster.
	if strings.Join(rc.Change.Actions, ",") != "update" {
		t.Errorf("the moved cluster plans %v, want an update in place", rc.Change.Actions)
	}
	if rc.Change.Before["id"] != "tf-demo" || rc.Change.Before["display_name"] != "Training" {
		t.Errorf("the moved state lost its values: %v", rc.Change.Before)
	}
	w.noRequestReachedTheAPI()
}
