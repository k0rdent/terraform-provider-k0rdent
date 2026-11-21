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
			mustSay(t, "the provenance refusal", out, "exact `create_token`", tc.want, "No create was sent or state recorded")
			requireNoManagedResourceState(t, c)
			if got := fakeUID(t, clusterAPIPath(name)); got != uid {
				t.Fatalf("refused cluster uid changed from %s to %s", uid, got)
			}
			requireCreateCount(t, creates, 0)
		})
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
