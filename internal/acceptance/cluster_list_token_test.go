package acceptance

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func clusterTokenQuery(id string, token *string) string {
	tokenLine := ""
	if token != nil {
		tokenLine = fmt.Sprintf("    create_token = %q\n", *token)
	}
	return fmt.Sprintf(`list "k0rdent_kubernetes_cluster" "found" {
  provider = k0rdent

  config {
    id = %q
%s  }
}
`, id, tokenLine)
}

func seedClusterQueryProject(t *testing.T, tf, build string) {
	t.Helper()
	seed := newQueryWorkspace(t, tf, build, fmt.Sprintf(`
resource %q "seed" {
  id              = %q
  organization_id = %q
  region          = "us-texas-1"
}
`, projectType, queryProject, accOrg))
	seed.mustRun("apply", "-auto-approve")
	t.Cleanup(func() { seed.mustRun("destroy", "-auto-approve") })
}

func removeClusterQueryCluster(t *testing.T, path string) {
	t.Helper()
	status, body, err := callFake("DELETE", path)
	if err != nil || (status >= 300 && status != 404) {
		t.Errorf("cleanup delete: status %d, body %s, error %v", status, body, err)
		return
	}
	if status != 404 {
		awaitFakeGone(t, path)
	}
}

func TestAccKubernetesClusterListRejectsEmptyCreateToken(t *testing.T) {
	tf := terraformForQuery(t)
	build := builtProvider(t)
	seedClusterQueryProject(t, tf, build)
	const name = "acc-review-empty-list-token"
	path := "/v1/regions/us-texas-1/projects/" + queryProject + "/compute/clusters/" + name
	if err := postFake("/v1/regions/us-texas-1/projects/"+queryProject+"/compute/clusters", fmt.Sprintf(
		`{"id":%q,"clusterType":%q,"displayName":"untagged"}`, name, clusterTypeRef)); err != nil {
		t.Fatal(err)
	}
	awaitFakeState(t, path, "active")
	t.Cleanup(func() { removeClusterQueryCluster(t, path) })

	query := newQueryWorkspace(t, tf, build, "")
	empty := ""
	writeFile(t, filepath.Join(query.dir, "found.tfquery.hcl"), clusterTokenQuery(name, &empty))
	query.mustRun("init")
	code, out := query.run("query", "-generate-config-out=adopted.tf")
	if code == 0 {
		generated, _ := os.ReadFile(filepath.Join(query.dir, "adopted.tf"))
		t.Fatalf("an empty token adopted an untagged cluster:\n%s\n%s", out, generated)
	}
	mustSay(t, "the empty list token refusal", out, "create_token", "at least 1")
}
