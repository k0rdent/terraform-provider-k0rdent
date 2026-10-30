package acceptance

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"runtime"
	"strings"
	"testing"
	"time"
)

// A create that stops waiting leaves its row tainted, and the platform can
// still finish the cluster. If that cluster is then deleted outside Terraform
// and another tool makes one under the same id, the row must not take the new
// one for its own: a destroy would delete it. The row keeps the uid the
// create's answer named, so through the real CLI:
//
//  1. `terraform apply` with a 1s create timeout fails, tainted, with the uid
//     recorded
//  2. the platform finishes it, it's deleted outside Terraform, and another
//     tool makes a cluster under the id, which gets a new uid and settles
//     `active`
//  3. `terraform destroy` refreshes, drops the row with a warning naming both
//     uids, and its plan is then refused as a create over a cluster this
//     configuration didn't make. Nothing is sent
//  4. `terraform untaint` and `terraform destroy`, which deleted the other
//     tool's cluster before the uid was recorded, sends nothing either
//  5. `terraform destroy -refresh=false` reaches the delete, which reads the
//     other uid, sends nothing, and drops the row
//  6. the other tool's cluster is still there
func TestAccADestroyLeavesAClusterMadeUnderAReusedID(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the CLI sequence runs on unix")
	}
	const name = "acc-reused-id"
	path := clusterAPIPath(name)
	c := newCLIRun(t)
	addr := clusterType + ".test"
	c.write(slowObject(clusterType, name, createsSlowly, `create = "1s"`))
	out, ok := c.run("apply", "-auto-approve")
	if ok {
		t.Fatalf("the create didn't time out:\n%s", out)
	}
	mustSay(t, "the apply", out, "is still being created")
	ours := c.heldUID(addr)
	if ours == "" {
		t.Fatal("the tainted row holds no uid")
	}

	awaitFakeState(t, path, "active")
	if status, body, err := callFake(http.MethodDelete, path); err != nil || status >= 300 {
		t.Fatalf("deleting it outside Terraform: %d %s %v", status, body, err)
	}
	awaitFakeGone(t, path)
	if err := postFake("/v1/regions/us-texas-1/projects/demo/compute/clusters", fmt.Sprintf(
		`{"id": %q, "clusterType": %q, "displayName": "another tool's", "nodePools": [{"id": "gpu-workers", "nodeCount": 2}]}`,
		name, clusterTypeRef)); err != nil {
		t.Fatalf("another tool's create: %v", err)
	}
	// The fake finishes a teardown only as it's read, so the cleanup reads
	// it until it's gone, or a later test meets it.
	t.Cleanup(func() {
		status, body, err := callFake(http.MethodDelete, path)
		if err != nil || (status >= 300 && status != http.StatusNotFound) {
			t.Errorf("cleanup delete of the other tool's cluster: %d %s %v", status, body, err)
			return
		}
		awaitFakeGone(t, path)
	})
	awaitFakeState(t, path, "active")
	theirs := fakeUID(t, path)
	if theirs == ours {
		t.Fatalf("the cluster made under the reused id has the deleted one's uid %s", ours)
	}
	another := fmt.Sprintf("The cluster %q is another cluster now", name)

	deletes := &requestCounter{method: http.MethodDelete, path: path}
	deletes.start(t)()
	cancels := &requestCounter{method: http.MethodPost, path: path + "/cancel"}
	cancels.start(t)()
	sentNothing := func(step string) {
		t.Helper()
		for _, sent := range []*requestCounter{deletes, cancels} {
			if n, err := fakeServed(sent.method, sent.path); err != nil || n != sent.before {
				t.Errorf("%s sent %d %s %s: %v", step, n-sent.before, sent.method, sent.path, err)
			}
		}
	}

	out, ok = c.run("destroy", "-auto-approve")
	if ok {
		t.Errorf("the destroy's plan passed:\n%s", out)
	}
	mustSay(t, "the destroy", out, another, ours, theirs, "Nothing was sent: the plan was refused.")
	sentNothing("the destroy")

	if out, ok = c.run("untaint", addr); !ok {
		t.Fatalf("untaint:\n%s", out)
	}
	out, ok = c.run("destroy", "-auto-approve")
	if ok {
		t.Errorf("the destroy's plan passed after the untaint:\n%s", out)
	}
	mustSay(t, "the destroy after the untaint", out, another, "Nothing was sent: the plan was refused.")
	sentNothing("the destroy after the untaint")

	out, ok = c.run("destroy", "-auto-approve", "-refresh=false")
	if !ok {
		t.Fatalf("the destroy without a refresh failed:\n%s", out)
	}
	mustSay(t, "the destroy without a refresh", out, another, ours, theirs, "The destroy sent nothing to it")
	sentNothing("the destroy without a refresh")
	if listed, ok := c.run("state", "list"); !ok || strings.TrimSpace(listed) != "" {
		t.Errorf("state isn't empty after the destroy:\n%s", listed)
	}

	status, body, err := readFake(path)
	if err != nil || status != http.StatusOK {
		t.Fatalf("the other tool's cluster is gone: %d %v", status, err)
	}
	var got struct {
		UID         string `json:"uid"`
		State       string `json:"state"`
		DisplayName string `json:"displayName"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got.UID != theirs || got.State != "active" || got.DisplayName != "another tool's" {
		t.Errorf("the other tool's cluster reads %+v after the destroys, want it untouched", got)
	}
}

// heldUID is the uid the provider keeps for addr in this run's state, "" for
// none.
func (c *cliRun) heldUID(addr string) string {
	c.t.Helper()
	cmd := c.command("state", "pull")
	out, err := cmd.Output()
	if err != nil {
		c.t.Fatalf("terraform state pull: %v", err)
	}
	var st struct {
		Resources []struct {
			Type, Name string
			Instances  []struct {
				Private string `json:"private"`
			}
		}
	}
	if err := json.Unmarshal(out, &st); err != nil {
		c.t.Fatal(err)
	}
	for _, r := range st.Resources {
		if r.Type+"."+r.Name != addr || len(r.Instances) == 0 {
			continue
		}
		// Terraform keeps the provider's private state base64 encoded, and
		// the framework keeps each key's JSON value base64 encoded in it.
		raw, err := base64.StdEncoding.DecodeString(r.Instances[0].Private)
		if err != nil {
			c.t.Fatalf("decoding %s's private state: %v", addr, err)
		}
		var private map[string][]byte
		if err := json.Unmarshal(raw, &private); err != nil {
			c.t.Fatalf("reading %s's private state %s: %v", addr, raw, err)
		}
		var uid string
		if held, ok := private["uid"]; ok {
			if err := json.Unmarshal(held, &uid); err != nil {
				c.t.Fatalf("reading %s's uid %s: %v", addr, held, err)
			}
		}
		return uid
	}
	c.t.Fatalf("no %s in state:\n%s", addr, out)
	return ""
}

// fakeUID reads an object's uid from the fake, past the provider.
func fakeUID(t *testing.T, path string) string {
	t.Helper()
	status, body, err := readFake(path)
	if err != nil || status != http.StatusOK {
		t.Fatalf("reading %s: %d %v", path, status, err)
	}
	var got struct {
		UID string `json:"uid"`
	}
	if err := json.Unmarshal(body, &got); err != nil || got.UID == "" {
		t.Fatalf("%s names no uid: %s %v", path, body, err)
	}
	return got.UID
}

// awaitFakeGone reads path from the fake until it answers 404. Each read is
// one step of the fake's teardown.
func awaitFakeGone(t *testing.T, path string) {
	t.Helper()
	for range 200 {
		status, _, err := readFake(path)
		if err != nil {
			t.Fatal(err)
		}
		if status == http.StatusNotFound {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s is still there", path)
}
