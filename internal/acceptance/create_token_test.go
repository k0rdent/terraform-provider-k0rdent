package acceptance

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"
)

// createTokenConfig puts the durable provenance token in an ephemeral input,
// so neither the configuration snapshot nor resource state needs its value.
func createTokenConfig(name, region string) string {
	scoped := ""
	if region != "" {
		scoped = fmt.Sprintf("  region            = %q\n", region)
	}
	return fmt.Sprintf(`
variable "create_token" {
  type      = string
  ephemeral = true
}

resource %q "test" {
  id                = %q
  cluster_type      = %q
  create_token      = var.create_token
%s  display_name      = "fake-creates-slowly"
  node_pool = {
    "gpu-workers" = { node_count = 2 }
  }
  timeouts {
    create = "10m"
  }
}
`, clusterType, name, clusterTypeRef, scoped)
}

// killAfterAcceptedCreate starts the saved apply and kills terraform only
// after the fake has filed the create. State has no managed object yet.
func killAfterAcceptedCreate(t *testing.T, c *cliRun, name string) {
	t.Helper()
	if out, ok := c.run("plan", "-out=create.tfplan"); !ok {
		t.Fatalf("the create plan failed:\n%s", out)
	}
	apply := c.command("apply", "-no-color", "create.tfplan")
	var out bytes.Buffer
	apply.Stdout, apply.Stderr = &out, &out
	if err := apply.Start(); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(2 * time.Minute); ; time.Sleep(20 * time.Millisecond) {
		if status, _, err := readFake(clusterAPIPath(name)); err == nil && status == http.StatusOK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the create was never accepted: %v\n%s", apply.Process.Kill(), out.String())
		}
	}
	if err := apply.Process.Kill(); err != nil {
		t.Fatalf("kill: %v", err)
	}
	var exited *exec.ExitError
	if err := apply.Wait(); !errors.As(err, &exited) {
		t.Fatalf("the killed apply ended with %v, not a kill", err)
	}
	requireNoManagedState(t, c)
}

// requireNoManagedState accepts both an empty state list and Terraform's
// error when no state file exists yet.
func requireNoManagedState(t *testing.T, c *cliRun) {
	t.Helper()
	listed, ok := c.run("state", "list")
	if !ok && strings.Contains(listed, "No state file was found!") {
		return
	}
	if !ok {
		t.Fatalf("state list failed:\n%s", listed)
	}
	if strings.TrimSpace(listed) != "" {
		t.Fatalf("state holds managed objects:\n%s", listed)
	}
}

// adoptAfterKill creates a durable-token cluster without saving state, then
// applies the same saved plan with the same token. It returns the adopted run
// and the original UID.
func adoptAfterKill(t *testing.T, name, token string) (*cliRun, string) {
	t.Helper()
	c := newCLIRun(t)
	c.addEnv("TF_VAR_create_token", token)
	c.write(createTokenConfig(name, ""))
	t.Cleanup(func() { cancelAndRemoveFromFake(t, name) })
	killAfterAcceptedCreate(t, c, name)
	uid := fakeUID(t, clusterAPIPath(name))

	if out, ok := c.run("apply", "create.tfplan"); !ok {
		t.Fatalf("the durable-token adoption failed:\n%s", out)
	} else {
		mustSay(t, "the adoption", out, "its create token matches", "Apply complete!")
	}
	got := c.cliInstance(clusterType + ".test")
	if got.Attributes.UID != uid || c.heldUID(clusterType+".test") != uid {
		t.Errorf("adopted uid %q and private uid %q, want original uid %q",
			got.Attributes.UID, c.heldUID(clusterType+".test"), uid)
	}
	if tags := fakeTags(t, clusterAPIPath(name)); tags[createTokenKey] != token {
		t.Errorf("the platform token does not match the durable token")
	}
	if out, code := c.runCode("plan", "-detailed-exitcode"); code != 0 {
		t.Errorf("the plan after adoption exited %d:\n%s", code, out)
	}
	shown, ok := c.run("show", "-json")
	if !ok || strings.Contains(shown, token) {
		t.Errorf("the durable token reached state output:\n%s", shown)
	}
	planned, ok := c.run("show", "-json", "create.tfplan")
	if !ok || strings.Contains(planned, token) {
		t.Errorf("the durable token reached the saved plan:\n%s", planned)
	}
	return c, uid
}

// A durable token makes a lost runner recoverable without import. The saved
// apply adopts the original UID and sends no replacement create.
func TestAccCreateTokenAdoptsAfterLostRunner(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		skipTest(t, "the CLI sequence runs on unix")
	}
	const name = "acc-create-token-adopt"
	c, uid := adoptAfterKill(t, name, "durable-adoption-token")
	if got := c.cliInstance(clusterType + ".test"); got.Attributes.UID != uid {
		t.Fatalf("the adopted uid changed from %s to %s", uid, got.Attributes.UID)
	}
	if out, ok := c.run("destroy", "-auto-approve"); !ok {
		t.Fatalf("the adopted cluster's destroy failed:\n%s", out)
	}
	if err := checkGoneAt(clusterAPIPath(name))(nil); err != nil {
		t.Error(err)
	}
}

// Once adoption has recorded the UID, another cluster under the same id is
// never adopted or deleted. The refresh drops the stale row and destroy sends
// no cancel or delete to the replacement.
func TestAccCreateTokenDestroyLeavesAReusedName(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		skipTest(t, "the CLI sequence runs on unix")
	}
	const name = "acc-create-token-reused"
	path := clusterAPIPath(name)
	c, adoptedUID := adoptAfterKill(t, name, "durable-name-guard-token")

	if status, body, err := callFake(http.MethodDelete, path); err != nil || status >= 300 {
		t.Fatalf("deleting the adopted cluster outside Terraform: %d %s %v", status, body, err)
	}
	awaitFakeGone(t, path)
	if err := postFake("/v1/regions/us-texas-1/projects/demo/compute/clusters", fmt.Sprintf(
		`{"id":%q,"clusterType":%q,"displayName":"another tool's"}`, name, clusterTypeRef)); err != nil {
		t.Fatal(err)
	}
	awaitFakeState(t, path, "active")
	replacementUID := fakeUID(t, path)
	if replacementUID == adoptedUID {
		t.Fatal("the replacement shares the adopted cluster's uid")
	}
	deletes := &requestCounter{method: http.MethodDelete, path: path}
	cancels := &requestCounter{method: http.MethodPost, path: path + "/cancel"}
	deletes.start(t)()
	cancels.start(t)()
	out, ok := c.run("destroy", "-auto-approve")
	if !ok {
		t.Fatalf("the destroy over the reused name failed:\n%s", out)
	}
	mustSay(t, "the reused-name destroy", out, "0 destroyed")
	for _, counter := range []*requestCounter{deletes, cancels} {
		if got, err := fakeServed(counter.method, counter.path); err != nil || got != counter.before {
			t.Errorf("destroy sent %d %s %s to the replacement: %v", got-counter.before, counter.method, counter.path, err)
		}
	}
	if got := fakeUID(t, path); got != replacementUID {
		t.Errorf("replacement uid = %q, want it untouched at %q", got, replacementUID)
	}
}

// Missing or different provenance is refused without state or deletion. The
// error names the exact-token check and the values to reuse or change.
func TestAccCreateTokenRefusesMissingAndMismatchedProvenance(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		tags string
		want string
	}{
		{"missing", "", "carries no create token"},
		{"different", `,"tags":{"terraform-provider-k0rdent/create-token":"another-run"}`, "does not exactly match"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := newCLIRun(t)
			name := "acc-create-token-refused-" + tc.name
			path := clusterAPIPath(name)
			if err := postFake("/v1/regions/us-texas-1/projects/demo/compute/clusters", fmt.Sprintf(
				`{"id":%q,"clusterType":%q,"displayName":"made elsewhere"%s}`, name, clusterTypeRef, tc.tags)); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { removeFromFake(t, name) })
			c.addEnv("TF_VAR_create_token", "wanted-durable-token")
			c.write(createTokenConfig(name, ""))
			out, ok := c.run("apply", "-auto-approve")
			if ok {
				t.Fatalf("the %s-token cluster was adopted:\n%s", tc.name, out)
			}
			mustSay(t, "the provenance refusal", out, "create_token", tc.want,
				"same `id`, `region`, and `project`", "choose another `id`")
			requireNoManagedState(t, c)
			if got := fakeUID(t, path); got == "" {
				t.Error("the refused cluster disappeared")
			}
		})
	}
}

// The token lookup is project-scoped. A matching token in another project is
// not adopted. Region and ClusterType scope likewise has to agree before any
// create can proceed.
func TestAccCreateTokenChecksProjectAndRegion(t *testing.T) {
	t.Parallel()
	t.Run("project", func(t *testing.T) {
		t.Parallel()
		c := newCLIRun(t)
		const name = "acc-create-token-scope"
		otherPath := "/v1/regions/us-texas-1/projects/example/compute/clusters/" + name
		if err := postFake("/v1/regions/us-texas-1/projects/example/compute/clusters", fmt.Sprintf(
			`{"id":%q,"clusterType":%q,"tags":{"terraform-provider-k0rdent/create-token":"scoped-token"}}`,
			name, clusterTypeRef)); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if status, _, err := callFake(http.MethodDelete, otherPath); err != nil || status >= 300 {
				t.Errorf("cleanup in the other project: %d %v", status, err)
			}
			removeFromFake(t, name)
		})
		otherUID := fakeUID(t, otherPath)
		c.addEnv("TF_VAR_create_token", "scoped-token")
		c.write(createTokenConfig(name, ""))
		if out, ok := c.run("apply", "-auto-approve"); !ok {
			t.Fatalf("the create in the intended project failed:\n%s", out)
		}
		if got := c.cliInstance(clusterType + ".test").Attributes.UID; got == otherUID {
			t.Errorf("the cluster in the other project was adopted at uid %s", otherUID)
		}
		if got := fakeUID(t, otherPath); got != otherUID {
			t.Errorf("the other project's cluster changed from %s to %s", otherUID, got)
		}
	})

	t.Run("region", func(t *testing.T) {
		t.Parallel()
		c := newCLIRun(t)
		const name = "acc-create-token-region"
		if err := postFake("/v1/regions/us-texas-1/projects/demo/compute/clusters", fmt.Sprintf(
			`{"id":%q,"clusterType":%q,"tags":{"terraform-provider-k0rdent/create-token":"scoped-token"}}`,
			name, clusterTypeRef)); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { removeFromFake(t, name) })
		c.addEnv("TF_VAR_create_token", "scoped-token")
		c.write(createTokenConfig(name, "us-east-1"))
		out, ok := c.run("plan")
		if ok {
			t.Fatalf("the region-mismatched adoption planned:\n%s", out)
		}
		mustSay(t, "the region refusal", out, "another region", `region is "us-east-1"`, "us-texas-1")
		requireNoManagedState(t, c)
	})
}

// A supplied empty token is not silently replaced by a random one.
func TestAccCreateTokenRefusesEmpty(t *testing.T) {
	t.Parallel()
	c := newCLIRun(t)
	c.addEnv("TF_VAR_create_token", "")
	c.write(createTokenConfig("acc-create-token-empty", ""))
	out, ok := c.run("plan")
	if ok {
		t.Fatalf("an empty create token planned:\n%s", out)
	}
	mustSay(t, "the empty token refusal", out, "create_token", "at least 1")
}
