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
	return fmt.Sprintf(`list %q "found" {
  provider = k0rdent

  config {
    id = %q
%s  }
}
`, clusterType, id, tokenLine)
}

const clusterTokenQueryProject = "acc-list-token-query"

func seedClusterQueryProject(t *testing.T, tf, build string) {
	t.Helper()
	seed := newQueryWorkspace(t, tf, build, fmt.Sprintf(`
resource %q "seed" {
  id              = %q
  organization_id = %q
  region          = "us-texas-1"
}
`, projectType, clusterTokenQueryProject, accOrg))
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

func TestAccKubernetesClusterListRejectsBlankCreateToken(t *testing.T) {
	t.Parallel()
	if os.Getenv("TF_ACC") == "" {
		skipTest(t, "TF_ACC is not set")
	}
	tf := terraformForQuery(t)
	build := builtProvider(t)
	seedClusterQueryProject(t, tf, build)
	const name = "acc-review-empty-list-token"
	path := "/v1/regions/us-texas-1/projects/" + clusterTokenQueryProject + "/compute/clusters/" + name
	if err := postFake("/v1/regions/us-texas-1/projects/"+clusterTokenQueryProject+"/compute/clusters", fmt.Sprintf(
		`{"id":%q,"clusterType":%q,"displayName":"untagged"}`, name, clusterTypeRef)); err != nil {
		t.Fatal(err)
	}
	awaitFakeState(t, path, "active")
	t.Cleanup(func() { removeClusterQueryCluster(t, path) })

	for _, tc := range []struct {
		name  string
		token string
	}{
		{"empty", ""},
		{"space", " "},
		{"mixed whitespace", " \t\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			query := newQueryWorkspace(t, tf, build, "")
			writeFile(t, filepath.Join(query.dir, "found.tfquery.hcl"), clusterTokenQuery(name, &tc.token))
			query.mustRun("init")
			code, out := query.run("query", "-generate-config-out=adopted.tf")
			if code == 0 {
				generated, err := os.ReadFile(filepath.Join(query.dir, "adopted.tf"))
				if err != nil {
					t.Fatal(err)
				}
				t.Fatalf("a blank token was accepted:\n%s\n%s", out, generated)
			}
			mustSay(t, "the blank list token refusal", out, "create_token", "at least one non-whitespace character")
		})
	}
}
