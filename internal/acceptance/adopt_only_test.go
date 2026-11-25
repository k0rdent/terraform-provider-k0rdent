package acceptance

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func adoptOnlyConfig(_ string) string {
	return fmt.Sprintf(`
terraform {
  required_providers {
    k0rdent = { source = "k0rdent/k0rdent" }
  }
}

variable "adopt_only" {
  type    = bool
  default = false
}

variable "cluster_id" {
  type = string
}

variable "region" {
  type    = string
  default = null
}

variable "project" {
  type    = string
  default = null
}

variable "create_token" {
  type      = string
  ephemeral = true
}

data %[2]q "recovery" {
  count   = var.adopt_only ? 1 : 0
  id      = var.cluster_id
  region  = var.region
  project = var.project
}

locals {
  adoptees = var.adopt_only ? {
    for cluster in data.%[2]s.recovery[0].clusters : cluster.id => cluster
    } : {
    (var.cluster_id) = {
      id      = var.cluster_id
      region  = var.region
      project = var.project
    }
  }
}

import {
  for_each = var.adopt_only ? local.adoptees : {}
  to       = %[1]s.test[each.key]
  identity = {
    id      = each.key
    region  = each.value.region
    project = each.value.project
  }
}

resource %[1]q "test" {
  for_each          = local.adoptees
  id                = each.key
  region            = each.value.region
  project           = each.value.project
  cluster_type      = %[3]q
  adopt_only        = var.adopt_only
  create_token      = var.create_token
  display_name      = "acceptance"
  node_pool = {
    "gpu-workers" = { node_count = 2 }
  }
  timeouts {
    create = "10m"
  }
  lifecycle {
    postcondition {
      condition     = var.adopt_only || self.state == "active"
      error_message = "The cluster isn't active."
    }
  }
}
`, clusterType, clusterType+"s", clusterTypeRef)
}

func TestTheAdoptOnlyGuideUsesTheTestedModule(t *testing.T) {
	t.Parallel()
	doc, err := os.ReadFile("../../templates/guides/import.md.tmpl")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(doc), strings.TrimSpace(adoptOnlyConfig(""))) {
		t.Fatal("the guide's adopt-only HCL differs from the module the acceptance tests execute")
	}
}

func planAndApply(t *testing.T, c *cliRun, plan string) (string, bool) {
	t.Helper()
	if out, ok := c.run("init", "-input=false"); !ok {
		t.Fatalf("init failed:\n%s", out)
	}
	if out, ok := c.run("plan", "-out="+plan); !ok {
		return out, false
	}
	return c.run("apply", plan)
}

func requireCreateCount(t *testing.T, c *requestCounter, want int) {
	t.Helper()
	got, err := fakeServed(c.method, c.path)
	if err != nil {
		t.Fatal(err)
	}
	if got-c.before != want {
		t.Fatalf("fake served %d new %s %s requests, want %d", got-c.before, c.method, c.path, want)
	}
}

func requireNoManagedResourceState(t *testing.T, c *cliRun) {
	t.Helper()
	out, ok := c.run("state", "pull")
	if !ok {
		t.Fatalf("state pull failed:\n%s", out)
	}
	if strings.TrimSpace(out) == "" {
		return
	}
	var state struct {
		Resources []struct {
			Mode string `json:"mode"`
			Type string `json:"type"`
		} `json:"resources"`
	}
	if err := json.Unmarshal([]byte(out), &state); err != nil {
		t.Fatalf("state pull isn't JSON: %v\n%s", err, out)
	}
	for _, r := range state.Resources {
		if r.Mode == "managed" {
			t.Fatalf("state holds managed object %s:\n%s", r.Type, out)
		}
	}
}

func TestAccKubernetesClusterAdoptOnlyAbsentIsNoop(t *testing.T) { //nolint:paralleltest // it counts creates shared fake serves
	const name = "acc-adopt-only-absent"
	c := newCLIRun(t)
	c.addEnv("TF_VAR_adopt_only", "true")
	c.addEnv("TF_VAR_cluster_id", name)
	c.addEnv("TF_VAR_create_token", "adopt-absent-token")
	writeFile(t, filepath.Join(c.dir, "main.tf"), adoptOnlyConfig(name))
	creates := &requestCounter{method: http.MethodPost, path: "/v1/regions/us-texas-1/projects/demo/compute/clusters"}
	creates.start(t)()
	out, ok := planAndApply(t, c, "adopt.tfplan")
	if !ok {
		t.Fatalf("the absent adopt-only apply failed:\n%s", out)
	}
	requireNoManagedResourceState(t, c)
	requireCreateCount(t, creates, 0)
	if shown, ok := c.run("show", "-json", "adopt.tfplan"); !ok || strings.Contains(shown, "adopt-absent-token") {
		t.Errorf("the durable token reached the saved plan:\n%s", shown)
	}
}

func seedAdoptCandidate(t *testing.T, name, token string) string {
	t.Helper()
	path := clusterAPIPath(name)
	tags := ""
	if token != "" {
		tags = fmt.Sprintf(`,"tags":{"terraform-provider-k0rdent/create-token":%q}`, token)
	}
	if err := postFake("/v1/regions/us-texas-1/projects/demo/compute/clusters", fmt.Sprintf(
		`{"id":%q,"clusterType":%q,"displayName":"acceptance"%s}`, name, clusterTypeRef, tags)); err != nil {
		t.Fatal(err)
	}
	awaitFakeState(t, path, "active")
	t.Cleanup(func() { cancelAndRemoveFromFake(t, name) })
	return fakeUID(t, path)
}

func TestAccKubernetesClusterAdoptOnlyWithoutImportSendsNoCreate(t *testing.T) { //nolint:paralleltest // it counts creates shared fake serves
	if os.Getenv("TF_ACC") == "" {
		skipTest(t, "TF_ACC is not set")
	}
	const name, token = "acc-adopt-only-bare", "bare-adopt-token"
	path := clusterAPIPath(name)
	t.Cleanup(func() { cancelAndRemoveFromFake(t, name) })
	if err := postFake("/v1/regions/us-texas-1/projects/demo/compute/clusters", fmt.Sprintf(
		`{"id":%q,"clusterType":%q,"displayName":%q,"nodePools":[{"id":"gpu-workers","nodeCount":2}],"tags":{"terraform-provider-k0rdent/create-token":%q}}`,
		name, clusterTypeRef, neverSettles, token)); err != nil {
		t.Fatal(err)
	}
	uid := fakeUID(t, path)
	c := newCLIRun(t)
	c.addEnv("TF_VAR_create_token", token)
	config := strings.Replace(createTokenConfig(name, ""), "  create_token      = var.create_token", "  create_token      = var.create_token\n  adopt_only        = true", 1)
	config = strings.Replace(config, `"fake-creates-slowly"`, `"`+neverSettles+`"`, 1)
	config = strings.Replace(config, `"10m"`, `"1s"`, 1)
	c.write(config)
	creates := &requestCounter{method: http.MethodPost, path: "/v1/regions/us-texas-1/projects/demo/compute/clusters"}
	creates.start(t)()
	if out, ok := c.run("apply", "-auto-approve"); !ok {
		t.Fatalf("bare adopt-only apply failed:\n%s", out)
	}
	requireCreateCount(t, creates, 0)
	if got := c.cliInstance(clusterType + ".test"); got.Attributes.UID != uid || got.Attributes.State != "creating" || got.Status == "tainted" {
		t.Fatalf("adopted state = %+v, want an untainted creating cluster at UID %s", got, uid)
	}
	if out, ok := c.run("destroy", "-auto-approve"); !ok {
		t.Fatalf("destroy after bare adoption failed:\n%s", out)
	}
}

func TestAccKubernetesClusterAdoptOnlyMatchAndFlipBack(t *testing.T) { //nolint:paralleltest // it counts creates shared fake serves
	if os.Getenv("TF_ACC") == "" {
		skipTest(t, "TF_ACC is not set")
	}
	const name, token = "acc-adopt-only-match", "adopt-match-token"
	uid := seedAdoptCandidate(t, name, token)
	c := newCLIRun(t)
	c.addEnv("TF_VAR_adopt_only", "true")
	c.addEnv("TF_VAR_cluster_id", name)
	c.addEnv("TF_VAR_create_token", token)
	writeFile(t, filepath.Join(c.dir, "main.tf"), adoptOnlyConfig(name))
	creates := &requestCounter{method: http.MethodPost, path: "/v1/regions/us-texas-1/projects/demo/compute/clusters"}
	creates.start(t)()
	out, ok := planAndApply(t, c, "adopt.tfplan")
	if !ok {
		t.Fatalf("the exact-token adoption failed:\n%s", out)
	}
	addr := clusterType + ".test"
	got := c.cliInstance(addr)
	if got.Attributes.UID != uid || got.Attributes.State != "active" || c.heldUID(addr) != uid {
		t.Fatalf("adopted state = %+v, private uid %q, want active at %q", got.Attributes, c.heldUID(addr), uid)
	}
	if shown, readOK := c.run("show", "-json", "adopt.tfplan"); !readOK || strings.Contains(shown, token) {
		t.Errorf("the durable token reached the saved plan:\n%s", shown)
	}
	if planOutput, code := c.runCode("plan", "-detailed-exitcode"); code != 0 {
		t.Fatalf("the plan after adoption exited %d:\n%s", code, planOutput)
	}

	c.addEnv("TF_VAR_adopt_only", "false")
	out, ok = planAndApply(t, c, "normal.tfplan")
	if !ok {
		t.Fatalf("the return to normal management failed:\n%s", out)
	}
	requireCreateCount(t, creates, 0)
	if got := c.cliInstance(addr); got.Attributes.UID != uid {
		t.Fatalf("flip-back changed uid from %s to %s", uid, got.Attributes.UID)
	}
	if out, code := c.runCode("plan", "-detailed-exitcode"); code != 0 {
		t.Fatalf("the plan after flip-back exited %d:\n%s", code, out)
	}
	if out, ok := c.run("destroy", "-auto-approve"); !ok {
		t.Fatalf("the adopted cluster's destroy failed:\n%s", out)
	}
	if err := checkGoneAt(clusterAPIPath(name))(nil); err != nil {
		t.Error(err)
	}
}

func TestAccKubernetesClusterAdoptOnlyRefusesWrongProvenance(t *testing.T) { //nolint:paralleltest // it counts creates shared fake serves
	if os.Getenv("TF_ACC") == "" {
		skipTest(t, "TF_ACC is not set")
	}
	for _, tc := range []struct {
		name, token, want string
	}{
		{"missing", "", "carries no create token"},
		{"different", "another-token", "create token differs"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			name := "acc-adopt-only-refused-" + tc.name
			uid := seedAdoptCandidate(t, name, tc.token)
			c := newCLIRun(t)
			c.addEnv("TF_VAR_adopt_only", "true")
			c.addEnv("TF_VAR_cluster_id", name)
			c.addEnv("TF_VAR_create_token", "wanted-token")
			writeFile(t, filepath.Join(c.dir, "main.tf"), adoptOnlyConfig(name))
			creates := &requestCounter{method: http.MethodPost, path: "/v1/regions/us-texas-1/projects/demo/compute/clusters"}
			creates.start(t)()
			if out, ok := c.run("init", "-input=false"); !ok {
				t.Fatalf("init failed:\n%s", out)
			}
			out, ok := c.run("plan", "-out=refused.tfplan")
			if ok {
				t.Fatalf("%s provenance planned an adoption:\n%s", tc.name, out)
			}
			mustSay(t, "the provenance refusal", out, "exact `create_token`", tc.want, "No create or delete was sent", "A failed plan saves no managed row")
			requireNoManagedResourceState(t, c)
			if got := fakeUID(t, clusterAPIPath(name)); got != uid {
				t.Fatalf("refused cluster uid changed from %s to %s", uid, got)
			}
			requireCreateCount(t, creates, 0)
		})
	}
}

func TestAccKubernetesClusterAdoptOnlyRefusesProvenanceChangedAfterPlan(t *testing.T) { //nolint:paralleltest // it counts requests shared fake serves
	if os.Getenv("TF_ACC") == "" {
		skipTest(t, "TF_ACC is not set")
	}
	const name, token = "acc-adopt-only-token-race", "token-before-plan"
	uid := seedAdoptCandidate(t, name, token)
	c := newCLIRun(t)
	c.addEnv("TF_VAR_adopt_only", "true")
	c.addEnv("TF_VAR_cluster_id", name)
	c.addEnv("TF_VAR_create_token", token)
	writeFile(t, filepath.Join(c.dir, "main.tf"), adoptOnlyConfig(name))
	if out, ok := c.run("init", "-input=false"); !ok {
		t.Fatalf("init failed:\n%s", out)
	}
	if out, ok := c.run("plan", "-out=adopt.tfplan"); !ok {
		t.Fatalf("plan before the provenance change failed:\n%s", out)
	}
	if err := patchFake(clusterAPIPath(name), `{"tags":{"terraform-provider-k0rdent/create-token":"different-after-plan"}}`); err != nil {
		t.Fatal(err)
	}
	path := clusterAPIPath(name)
	creates := &requestCounter{method: http.MethodPost, path: "/v1/regions/us-texas-1/projects/demo/compute/clusters"}
	deletes := &requestCounter{method: http.MethodDelete, path: path}
	cancels := &requestCounter{method: http.MethodPost, path: path + "/cancel"}
	creates.start(t)()
	deletes.start(t)()
	cancels.start(t)()
	out, ok := c.run("apply", "adopt.tfplan")
	if ok {
		t.Fatalf("apply adopted a cluster whose token changed after the plan:\n%s", out)
	}
	mustSay(t, "apply after the provenance change", out, "No create or delete was sent", "leaves the imported row for the old UID in state", "next plain plan", "`terraform destroy`")
	if got := c.cliInstance(clusterType + ".test"); got.Attributes.UID != uid {
		t.Fatalf("saved-plan apply recorded uid %s, want the original %s", got.Attributes.UID, uid)
	}
	out, ok = c.run("plan", "-out=after.tfplan")
	if ok {
		t.Fatalf("plain plan accepted the changed provenance:\n%s", out)
	}
	mustSay(t, "plain plan after the provenance change", out, "create_token")
	if got := c.cliInstance(clusterType + ".test"); got.Attributes.UID != uid {
		t.Fatalf("plain plan changed the recorded uid to %s, want %s", got.Attributes.UID, uid)
	}
	out, ok = c.run("destroy", "-auto-approve")
	if ok {
		t.Fatalf("destroy accepted the changed provenance:\n%s", out)
	}
	mustSay(t, "destroy after the provenance change", out, "create_token")
	requireCreateCount(t, creates, 0)
	requireCreateCount(t, deletes, 0)
	requireCreateCount(t, cancels, 0)
	if got := fakeUID(t, path); got != uid {
		t.Fatalf("provenance-changed cluster uid = %s, want %s", got, uid)
	}
}

func TestAccKubernetesClusterAdoptOnlyRefusesReplacementCreatedAfterPlan(t *testing.T) { //nolint:paralleltest // it counts requests shared fake serves
	if os.Getenv("TF_ACC") == "" {
		skipTest(t, "TF_ACC is not set")
	}
	const name, token = "acc-adopt-only-reuse-race", "token-before-reuse"
	uid := seedAdoptCandidate(t, name, token)
	c := newCLIRun(t)
	c.addEnv("TF_VAR_adopt_only", "true")
	c.addEnv("TF_VAR_cluster_id", name)
	c.addEnv("TF_VAR_create_token", token)
	writeFile(t, filepath.Join(c.dir, "main.tf"), adoptOnlyConfig(name))
	if out, ok := c.run("init", "-input=false"); !ok {
		t.Fatalf("init failed:\n%s", out)
	}
	if out, ok := c.run("plan", "-out=adopt.tfplan"); !ok {
		t.Fatalf("plan before name reuse failed:\n%s", out)
	}
	path := clusterAPIPath(name)
	if status, body, err := callFake(http.MethodDelete, path); err != nil || status >= 300 {
		t.Fatalf("delete original: %d %s %v", status, body, err)
	}
	awaitFakeGone(t, path)
	if err := postFake("/v1/regions/us-texas-1/projects/demo/compute/clusters", fmt.Sprintf(
		`{"id":%q,"clusterType":%q,"displayName":"replacement","tags":{"terraform-provider-k0rdent/create-token":"replacement-token"}}`, name, clusterTypeRef)); err != nil {
		t.Fatal(err)
	}
	awaitFakeState(t, path, "active")
	replacementUID := fakeUID(t, path)
	creates := &requestCounter{method: http.MethodPost, path: "/v1/regions/us-texas-1/projects/demo/compute/clusters"}
	deletes := &requestCounter{method: http.MethodDelete, path: path}
	cancels := &requestCounter{method: http.MethodPost, path: path + "/cancel"}
	creates.start(t)()
	deletes.start(t)()
	cancels.start(t)()
	out, ok := c.run("apply", "adopt.tfplan")
	if ok {
		t.Fatalf("apply adopted the replacement created after the plan:\n%s", out)
	}
	mustSay(t, "apply after name reuse", out, "No create or delete was sent", "leaves the imported row for the old UID in state", "next plain plan", "`terraform destroy`")
	if got := c.cliInstance(clusterType + ".test"); got.Attributes.UID != uid {
		t.Fatalf("saved-plan apply recorded uid %s, want the original %s", got.Attributes.UID, uid)
	}
	out, ok = c.run("plan", "-out=after.tfplan")
	if ok {
		t.Fatalf("plain plan accepted the replacement:\n%s", out)
	}
	mustSay(t, "plain plan after name reuse", out, "create_token")
	if got := c.cliInstance(clusterType + ".test"); got.Attributes.UID != uid {
		t.Fatalf("plain plan recorded uid %s, want the original %s instead of replacement %s", got.Attributes.UID, uid, replacementUID)
	}
	out, ok = c.run("destroy", "-auto-approve")
	if ok {
		t.Fatalf("destroy accepted the replacement:\n%s", out)
	}
	mustSay(t, "destroy after name reuse", out, "create_token")
	requireCreateCount(t, creates, 0)
	requireCreateCount(t, deletes, 0)
	requireCreateCount(t, cancels, 0)
	if got := fakeUID(t, path); got != replacementUID {
		t.Fatalf("replacement uid changed from %s to %s", replacementUID, got)
	}
}

func TestAccKubernetesClusterAdoptOnlyAlreadyManagedBeforeDestroy(t *testing.T) { //nolint:paralleltest // it counts creates shared fake serves
	const name = "acc-adopt-only-managed"
	c := newCLIRun(t)
	c.addEnv("TF_VAR_cluster_id", name)
	c.addEnv("TF_VAR_create_token", "already-managed-token")
	c.addEnv("TF_VAR_adopt_only", "false")
	writeFile(t, filepath.Join(c.dir, "main.tf"), adoptOnlyConfig(name))
	path := clusterAPIPath(name)
	t.Cleanup(func() { cancelAndRemoveFromFake(t, name) })
	if out, ok := c.run("plan", "-out=create.tfplan"); !ok {
		t.Fatalf("the create plan failed:\n%s", out)
	}
	if out, ok := c.run("apply", "create.tfplan"); !ok {
		t.Fatalf("the create failed:\n%s", out)
	}
	uid := fakeUID(t, path)
	addr := clusterType + ".test"
	c.addEnv("TF_VAR_adopt_only", "true")
	creates := &requestCounter{method: http.MethodPost, path: "/v1/regions/us-texas-1/projects/demo/compute/clusters"}
	creates.start(t)()
	sentNothing := clusterWrites(t, path)
	out, ok := c.run("plan", "-out=adopt.tfplan")
	if !ok {
		t.Fatalf("the adopt plan failed:\n%s", out)
	}
	mustSay(t, "the adopt plan", out, "adopt_only", "0 to add, 1 to change, 0 to destroy")
	if out, ok = c.run("apply", "adopt.tfplan"); !ok {
		t.Fatalf("the adopt apply failed:\n%s", out)
	}
	sentNothing("adopt plan and apply")
	requireCreateCount(t, creates, 0)
	if got := c.heldUID(addr); got != uid {
		t.Fatalf("the adopt apply recorded UID %q, want %q", got, uid)
	}
	if out, ok = c.run("plan", "-destroy", "-out=destroy.tfplan"); !ok {
		t.Fatalf("the destroy plan failed:\n%s", out)
	}
	mustSay(t, "the destroy plan", out, "0 to add, 0 to change, 1 to destroy")
	if out, ok = c.run("apply", "destroy.tfplan"); !ok {
		t.Fatalf("the destroy apply failed:\n%s", out)
	}
	requireNoManagedResourceState(t, c)
	if err := checkGoneAt(path)(nil); err != nil {
		t.Fatal(err)
	}
}

func TestAccAdoptOnlyKeepsAnEarlierPinOutOfTheDeleteFlow(t *testing.T) {
	t.Parallel()
	const name = "acc-adopt-only-earlier-pin"
	c := newCLIRun(t)
	c.addEnv("TF_VAR_cluster_id", name)
	c.addEnv("TF_VAR_create_token", "adopt-earlier-pin-token")
	path := clusterAPIPath(name)
	t.Cleanup(func() { cancelAndRemoveFromFake(t, name) })
	config := strings.Replace(adoptOnlyConfig(name), clusterTypeRef+`"`, clusterTypeV2+`"`, 1)
	config = strings.Replace(config, "  timeouts {", "  storage_ceiling_gib = 100\n  timeouts {", 1)
	writeFile(t, filepath.Join(c.dir, "main.tf"), config)
	if out, ok := planAndApply(t, c, "create.tfplan"); !ok {
		t.Fatalf("create failed:\n%s", out)
	}
	uid := fakeUID(t, path)
	config = strings.Replace(config, clusterTypeV2, versionOne, 1)
	config = strings.Replace(config, `display_name      = "acceptance"`, `display_name      = "ignored rename"`, 1)
	config = strings.Replace(config, "node_count = 2", "node_count = 1", 1)
	config = strings.Replace(config, "storage_ceiling_gib = 100", "storage_ceiling_gib = 50", 1)
	writeFile(t, filepath.Join(c.dir, "main.tf"), config)
	c.addEnv("TF_VAR_adopt_only", "true")
	sentNothing := clusterWrites(t, path)
	out, ok := c.run("plan", "-out=adopt.tfplan")
	if !ok {
		t.Fatalf("adopt plan failed:\n%s", out)
	}
	mustSay(t, "the adopt plan", out, "keeps the platform value for `cluster_type`",
		"keeps the platform value for `display_name`", "keeps the platform value for `node_pool`",
		"keeps the platform value for `storage_ceiling_gib`")
	if out, ok = c.run("apply", "adopt.tfplan"); !ok {
		t.Fatalf("adopt apply failed:\n%s", out)
	}
	if out, ok = c.run("show", "-no-color"); !ok || !strings.Contains(out, clusterTypeV2) ||
		strings.Contains(out, versionOne) || strings.Contains(out, "ignored rename") {
		t.Fatalf("adopt state does not hold the platform's values:\n%s", out)
	}
	if got := c.heldUID(clusterType + ".test"); got != uid {
		t.Fatalf("adopt recorded UID %q, want %q", got, uid)
	}
	sentNothing("adopt plan and apply")
	if out, ok = c.run("plan", "-destroy", "-out=destroy.tfplan"); !ok {
		t.Fatalf("destroy plan failed:\n%s", out)
	}
	if out, ok = c.run("apply", "destroy.tfplan"); !ok {
		t.Fatalf("destroy apply failed:\n%s", out)
	}
	if err := checkGoneAt(path)(nil); err != nil {
		t.Fatal(err)
	}
}

func TestAccAdoptOnlyRecordsACreatingClusterBeforeDestroy(t *testing.T) {
	t.Parallel()
	const name = "acc-adopt-only-still-creating"
	c := newCLIRun(t)
	c.addEnv("TF_VAR_cluster_id", name)
	c.addEnv("TF_VAR_create_token", "adopt-busy-token")
	path := clusterAPIPath(name)
	t.Cleanup(func() { cancelAndRemoveFromFake(t, name) })
	config := strings.Replace(adoptOnlyConfig(name), `"10m"`, `"1s"`, 1)
	config = strings.Replace(config, `"acceptance"`, `"`+neverSettles+`"`, 1)
	config = strings.Replace(config, "  timeouts {", "  on_create_timeout = \"keep\"\n  timeouts {", 1)
	writeFile(t, filepath.Join(c.dir, "main.tf"), config)
	out, ok := planAndApply(t, c, "create.tfplan")
	if ok {
		t.Fatalf("the fixture settled unexpectedly:\n%s", out)
	}
	mustSay(t, "the create", out, "The cluster isn't active", "is still being created")
	uid := fakeUID(t, path)
	c.addEnv("TF_VAR_adopt_only", "true")
	sentNothing := clusterWrites(t, path)
	if out, ok = c.run("plan", "-out=adopt.tfplan"); !ok {
		t.Fatalf("adopt plan failed:\n%s", out)
	}
	if out, ok = c.run("apply", "adopt.tfplan"); !ok {
		t.Fatalf("adopt apply failed:\n%s", out)
	}
	addr := clusterType + ".test"
	if got := c.cliInstance(addr); got.Status == "tainted" || got.Attributes.State != "creating" || got.Attributes.UID != uid || c.heldUID(addr) != uid {
		t.Fatalf("adopt state doesn't hold the untainted creating cluster %q: %+v", uid, got)
	}
	sentNothing("adopt plan and apply")
	if out, ok = c.run("plan", "-destroy", "-out=destroy.tfplan"); !ok {
		t.Fatalf("destroy plan failed:\n%s", out)
	}
	if out, ok = c.run("apply", "destroy.tfplan"); !ok {
		t.Fatalf("destroy apply failed:\n%s", out)
	}
	mustSay(t, "the destroy", out, "Cancelled the cluster's in-flight operation to destroy it")
	if err := checkGoneAt(path)(nil); err != nil {
		t.Fatal(err)
	}
}

func TestAccKubernetesClusterAdoptOnlyFalseStillCreates(t *testing.T) { //nolint:paralleltest // it counts creates shared fake serves
	const name = "acc-adopt-only-normal-create"
	c := newCLIRun(t)
	c.addEnv("TF_VAR_adopt_only", "false")
	c.addEnv("TF_VAR_cluster_id", name)
	c.addEnv("TF_VAR_create_token", "ordinary-create-token")
	writeFile(t, filepath.Join(c.dir, "main.tf"), adoptOnlyConfig(name))
	t.Cleanup(func() { cancelAndRemoveFromFake(t, name) })
	creates := &requestCounter{method: http.MethodPost, path: "/v1/regions/us-texas-1/projects/demo/compute/clusters"}
	creates.start(t)()
	out, ok := planAndApply(t, c, "create.tfplan")
	if !ok {
		t.Fatalf("the ordinary create failed:\n%s", out)
	}
	addr := clusterType + ".test"
	if got := c.cliInstance(addr); got.Attributes.UID == "" || got.Attributes.State != "active" {
		t.Fatalf("ordinary create state = %+v", got.Attributes)
	}
	requireCreateCount(t, creates, 1)
	if out, ok := c.run("destroy", "-auto-approve"); !ok {
		t.Fatalf("ordinary destroy failed:\n%s", out)
	}
}

// The guide's module with a null token, against a cluster with no token. An
// untagged cluster's empty token equals a null token's empty value, so only the
// plan's adopt-only token check stops the import flow from adopting, and later
// destroying, a cluster this configuration never made.
func TestAccKubernetesClusterAdoptOnlyNullTokenRefusesAnUntaggedCluster(t *testing.T) { //nolint:paralleltest // it counts requests shared fake serves
	if os.Getenv("TF_ACC") == "" {
		skipTest(t, "TF_ACC is not set")
	}
	const name = "acc-adopt-only-null-token"
	uid := seedAdoptCandidate(t, name, "")
	c := newCLIRun(t)
	c.addEnv("TF_VAR_adopt_only", "true")
	c.addEnv("TF_VAR_cluster_id", name)
	writeFile(t, filepath.Join(c.dir, "main.tf"), adoptOnlyConfig(name))
	writeFile(t, filepath.Join(c.dir, "terraform.tfvars"), "create_token = null\n")
	path := clusterAPIPath(name)
	creates := &requestCounter{method: http.MethodPost, path: "/v1/regions/us-texas-1/projects/demo/compute/clusters"}
	deletes := &requestCounter{method: http.MethodDelete, path: path}
	cancels := &requestCounter{method: http.MethodPost, path: path + "/cancel"}
	creates.start(t)()
	deletes.start(t)()
	cancels.start(t)()
	if out, ok := c.run("init", "-input=false"); !ok {
		t.Fatalf("init failed:\n%s", out)
	}
	out, ok := c.run("plan", "-out=adopt.tfplan")
	if ok {
		t.Fatalf("adopt-only with a null token planned an untagged cluster's adoption:\n%s", out)
	}
	mustSay(t, "the null-token refusal", out, "Adopt-only requires a create token")
	requireNoManagedResourceState(t, c)
	requireCreateCount(t, creates, 0)
	requireCreateCount(t, deletes, 0)
	requireCreateCount(t, cancels, 0)
	if got := fakeUID(t, path); got != uid {
		t.Fatalf("the untagged cluster's uid changed from %s to %s", uid, got)
	}
}

// A bare adopt-only resource with `allow_replace = true`, which skips the plan
// guard, against a cluster with another token. It's still refused, and
// nothing is created, cancelled or deleted.
func TestAccKubernetesClusterAdoptOnlyMismatchWithAllowReplaceIsRefused(t *testing.T) { //nolint:paralleltest // it counts requests shared fake serves
	if os.Getenv("TF_ACC") == "" {
		skipTest(t, "TF_ACC is not set")
	}
	const name = "acc-adopt-only-allow-replace"
	uid := seedAdoptCandidate(t, name, "another-token")
	c := newCLIRun(t)
	c.addEnv("TF_VAR_create_token", "wanted-token")
	config := strings.Replace(createTokenConfig(name, ""), "  create_token      = var.create_token",
		"  create_token      = var.create_token\n  adopt_only        = true\n  allow_replace     = true", 1)
	config = strings.Replace(config, `display_name      = "fake-creates-slowly"`, `display_name      = "acceptance"`, 1)
	c.write(config)
	path := clusterAPIPath(name)
	creates := &requestCounter{method: http.MethodPost, path: "/v1/regions/us-texas-1/projects/demo/compute/clusters"}
	deletes := &requestCounter{method: http.MethodDelete, path: path}
	cancels := &requestCounter{method: http.MethodPost, path: path + "/cancel"}
	creates.start(t)()
	deletes.start(t)()
	cancels.start(t)()
	out, ok := c.run("apply", "-auto-approve")
	if ok {
		t.Fatalf("adopt-only with allow_replace adopted another token's cluster:\n%s", out)
	}
	mustSay(t, "the refusal", out, "has different provenance", "create token differs", "No create or delete was sent")
	requireNoManagedState(t, c)
	requireCreateCount(t, creates, 0)
	requireCreateCount(t, deletes, 0)
	requireCreateCount(t, cancels, 0)
	if got := fakeUID(t, path); got != uid {
		t.Fatalf("the refused cluster's uid changed from %s to %s", uid, got)
	}
}
