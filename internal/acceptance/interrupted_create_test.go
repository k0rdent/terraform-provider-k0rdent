package acceptance

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"
)

// One Ctrl-C while several creates wait, through the real CLI. Every create
// answers the interrupt at once with its object and an error, so Terraform
// writes them to state together. Terraform 1.16.0 to 1.16.4 can crash there
// and leave the state file empty (hashicorp/terraform#39283), which the
// interrupt makes likely rather than rare. So:
//
//  1. the apply stops without a crash
//  2. state holds every cluster the platform accepted, each tainted
//  3. `terraform destroy` removes them all
func TestAccOneInterruptKeepsEveryCreateInState(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		skipTest(t, "the CLI sequence runs on unix")
	}
	const n = 8
	c := newCLIRun(t)
	names := make([]string, n)
	for i := range names {
		names[i] = fmt.Sprintf("acc-interrupted-%d", i)
	}
	t.Cleanup(func() {
		for _, name := range names {
			cancelAndRemoveFromFake(t, name)
		}
	})
	c.write(fmt.Sprintf(`
resource %q "test" {
  count        = %d
  id           = "acc-interrupted-${count.index}"
  cluster_type = %q
  display_name = %q
  timeouts {
    create = "10m"
  }
}
`, clusterType, n, clusterTypeRef, neverSettles))

	var out bytes.Buffer
	apply := c.command("apply", "-auto-approve", "-no-color")
	apply.Stdout, apply.Stderr = &out, &out
	if err := apply.Start(); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(2 * time.Minute); ; time.Sleep(100 * time.Millisecond) {
		accepted := 0
		for _, name := range names {
			if status, _, err := readFake(clusterAPIPath(name)); err == nil && status == http.StatusOK {
				accepted++
			}
		}
		if accepted == n {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d of %d creates were accepted, and stopping the apply said: %v",
				accepted, n, apply.Process.Kill())
		}
	}
	if err := apply.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("interrupt: %v", err)
	}
	var exited *exec.ExitError
	if err := apply.Wait(); !errors.As(err, &exited) {
		t.Fatalf("the interrupted apply ended with %v, not a failure:\n%s", err, out.String())
	}
	for _, crash := range []string{"TERRAFORM CRASH", "panic:", "Failed to serialize"} {
		if strings.Contains(out.String(), crash) {
			t.Fatalf("the interrupted apply says %q:\n%s", crash, out.String())
		}
	}
	mustSay(t, "the interrupted apply", out.String(), "is still being created", "the run was interrupted")

	pulled, ok := c.run("state", "pull")
	if !ok {
		t.Fatalf("state pull failed:\n%s", pulled)
	}
	var state struct {
		Resources []struct {
			Mode, Type string
			Instances  []struct {
				Status     string `json:"status"`
				Attributes struct {
					ID string `json:"id"`
				} `json:"attributes"`
			} `json:"instances"`
		} `json:"resources"`
	}
	if err := json.Unmarshal([]byte(pulled), &state); err != nil {
		t.Fatalf("state after the interrupt isn't JSON: %v\n%s", err, pulled)
	}
	kept := map[string]string{}
	for _, r := range state.Resources {
		if r.Mode != "managed" || r.Type != clusterType {
			continue
		}
		for _, i := range r.Instances {
			kept[i.Attributes.ID] = i.Status
		}
	}
	for _, name := range names {
		switch status, ok := kept[name]; {
		case !ok:
			t.Errorf("%s was accepted by the platform but isn't in state", name)
		case status != "tainted":
			t.Errorf("%s is in state as %q, not tainted, though its create never finished", name, status)
		}
	}
	if t.Failed() {
		t.Fatalf("state after the interrupt:\n%s", pulled)
	}

	if gone, ok := c.run("destroy", "-auto-approve"); !ok {
		t.Fatalf("the destroy after the interrupt failed:\n%s", gone)
	}
	if listed, ok := c.run("state", "list"); !ok || strings.TrimSpace(listed) != "" {
		t.Errorf("state isn't empty after the destroy:\n%s", listed)
	}
	for _, name := range names {
		if status, _, err := readFakeStore(clusterAPIPath(name)); err != nil || status != http.StatusNotFound {
			t.Errorf("%s is still on the platform after the destroy: %d %v", name, status, err)
		}
	}
}

// A run killed once the create is accepted, as `kill -9` or a lost CI runner
// stops it, saves nothing: Terraform writes a create's state only when Create
// returns. So, through the real CLI:
//
//  1. `terraform apply` is killed while it waits, and there's no state
//  2. `terraform destroy` finds nothing to destroy and leaves the cluster
//  3. `terraform apply` fails with "already exists", names the interrupted
//     apply, and gives the import
//  4. that import takes the cluster back, and `terraform destroy` removes it
func TestAccAKilledCreateIsTakenBackByTheImportItsErrorGives(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		skipTest(t, "the CLI sequence runs on unix")
	}
	const name = "acc-killed-create"
	c := newCLIRun(t)
	c.write(slowObject(clusterType, name, createsSlowly, `create = "10m"`))
	t.Cleanup(func() { removeFromFake(t, name) })

	apply := exec.Command(c.terraform, "apply", "-auto-approve", "-no-color")
	apply.Dir = c.dir
	apply.Env = append(quietEnviron(), "TF_CLI_CONFIG_FILE="+c.rc, "TF_IN_AUTOMATION=1", "TF_INPUT=0",
		"CHECKPOINT_DISABLE=1")
	apply.Env = append(apply.Env, matrixTestEnv(t)...)
	if err := apply.Start(); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(2 * time.Minute); ; time.Sleep(100 * time.Millisecond) {
		if status, _, err := readFake(clusterAPIPath(name)); err == nil && status == http.StatusOK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the create was never accepted, and stopping the apply said: %v", apply.Process.Kill())
		}
	}
	if err := apply.Process.Kill(); err != nil {
		t.Fatalf("kill: %v", err)
	}
	var exited *exec.ExitError
	if err := apply.Wait(); !errors.As(err, &exited) {
		t.Fatalf("the killed apply ended with %v, not a kill", err)
	}
	checkFakeStateNow(t, name, "creating")

	out, ok := c.run("destroy", "-auto-approve")
	if !ok {
		t.Fatalf("the destroy with nothing in state failed:\n%s", out)
	}
	mustSay(t, "the destroy", out, "No objects need to be destroyed")
	checkFakeStateNow(t, name, "creating")

	out, ok = c.run("apply", "-auto-approve")
	if ok {
		t.Fatalf("the apply over the killed run's cluster passed:\n%s", out)
	}
	mustSay(t, "the apply", out, "already exists", "an apply stopped before it could save state",
		"terraform import <address> us-texas-1/demo/"+name)

	importID := regexp.MustCompile(`terraform import <address> (\S+)`).FindStringSubmatch(
		strings.Join(strings.Fields(strings.ReplaceAll(out, "│", " ")), " "))
	if importID == nil {
		t.Fatalf("no import command in:\n%s", out)
	}
	if out, ok := c.run("import", clusterType+".test", importID[1]); !ok {
		t.Fatalf("the import the error gives failed:\n%s", out)
	}
	if out, ok := c.run("destroy", "-auto-approve"); !ok {
		t.Fatalf("the destroy after the import failed:\n%s", out)
	}
	status, _, err := readFake(clusterAPIPath(name))
	if err != nil {
		t.Fatal(err)
	}
	if status == http.StatusOK {
		checkFakeStateNow(t, name, "deleting")
	}
}

// A cluster made outside Terraform, imported, then tainted by hand: the
// destroy's refresh plan plans its replacement and is refused, since it
// carries no create token. The refusal says to untaint first, and doing that
// lets the destroy remove it.
func TestAccATaintedImportIsDestroyedTheWayItsRefusalSays(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		skipTest(t, "the CLI sequence runs on unix")
	}
	const name = "acc-tainted-import"
	c := newCLIRun(t)
	if err := postFake("/v1/regions/us-texas-1/projects/demo/compute/clusters", fmt.Sprintf(
		`{"id":%q,"clusterType":%q,"displayName":"made elsewhere"}`, name, clusterTypeV2)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { removeFromFake(t, name) })
	c.write(taintedObject(clusterType, name, "made elsewhere"))
	addr := clusterType + ".test"
	if out, ok := c.run("import", addr, name); !ok {
		t.Fatalf("import failed:\n%s", out)
	}
	if out, ok := c.run("taint", addr); !ok {
		t.Fatalf("taint failed:\n%s", out)
	}

	out, ok := c.run("destroy", "-auto-approve")
	if ok {
		t.Fatalf("the destroy of a tainted cluster without a token passed:\n%s", out)
	}
	mustSay(t, "the refused destroy", out, "carries no create token", "so it's refused too",
		"terraform untaint <address> terraform destroy")

	if out, ok := c.run("untaint", addr); !ok {
		t.Fatalf("untaint failed:\n%s", out)
	}
	if out, ok := c.run("destroy", "-auto-approve"); !ok {
		t.Fatalf("the destroy the refusal advises failed:\n%s", out)
	}
	if listed, ok := c.run("state", "list"); !ok || strings.TrimSpace(listed) != "" {
		t.Errorf("state isn't empty after the destroy:\n%s", listed)
	}
}

// cancelAndRemoveFromFake cancels the create of a cluster a failed test left
// on the fake, since the fake refuses to delete one mid-create, then deletes
// it.
func cancelAndRemoveFromFake(t *testing.T, name string) {
	t.Helper()
	status, body, err := readFake(clusterAPIPath(name))
	if err != nil || status != http.StatusOK {
		return
	}
	var c struct {
		State     string `json:"state"`
		Operation struct {
			ExecutionID string `json:"executionId"`
		} `json:"operation"`
	}
	if json.Unmarshal(body, &c) == nil && c.State == "creating" && c.Operation.ExecutionID != "" {
		if err := postFake(clusterAPIPath(name)+"/cancel",
			fmt.Sprintf(`{"executionId":%q}`, c.Operation.ExecutionID)); err != nil {
			t.Logf("cleanup of %s: %v", name, err)
		}
	}
	removeFromFake(t, name)
}

// removeFromFake deletes a cluster a test left on the fake, if it's there.
func removeFromFake(t *testing.T, name string) {
	t.Helper()
	status, body, err := callFake(http.MethodDelete, clusterAPIPath(name))
	if err != nil || (status >= 300 && status != http.StatusNotFound && status != http.StatusConflict) {
		t.Logf("cleanup of %s: %d %s %v", name, status, body, err)
	}
}
