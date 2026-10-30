package acceptance

import (
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"
)

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
	if runtime.GOOS == "windows" {
		t.Skip("the CLI sequence runs on unix")
	}
	const name = "acc-killed-create"
	c := newCLIRun(t)
	c.write(slowObject(clusterType, name, createsSlowly, `create = "10m"`))
	t.Cleanup(func() { removeFromFake(t, name) })

	apply := exec.Command(c.terraform, "apply", "-auto-approve", "-no-color")
	apply.Dir = c.dir
	apply.Env = append(quietEnviron(), "TF_CLI_CONFIG_FILE="+c.rc, "TF_IN_AUTOMATION=1", "TF_INPUT=0",
		"CHECKPOINT_DISABLE=1")
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
	if runtime.GOOS == "windows" {
		t.Skip("the CLI sequence runs on unix")
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

// removeFromFake deletes a cluster a test left on the fake, if it's there.
func removeFromFake(t *testing.T, name string) {
	t.Helper()
	status, body, err := callFake(http.MethodDelete, clusterAPIPath(name))
	if err != nil || (status >= 300 && status != http.StatusNotFound && status != http.StatusConflict) {
		t.Logf("cleanup of %s: %d %s %v", name, status, body, err)
	}
}
