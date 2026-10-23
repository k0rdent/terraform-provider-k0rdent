package acceptance

import (
	"encoding/json"
	"net/http"
	"runtime"
	"strings"
	"testing"
)

// heldObject is taintedObject with a create timeout long enough to finish.
func heldObject(typeName, name, markers string) string {
	return strings.Replace(taintedObject(typeName, name, markers), `create = "1s"`, `create = "5m"`, 1)
}

// A second configuration that names an id another state holds plans the same
// create a tainted object does, and the provider can't tell them apart, so
// the plan passes. Its warning has to say what applying really does there:
// nothing is replaced, and the create fails with "already exists". The state
// that holds the object is left alone.
func TestAccASecondStateOverATakenIDIsWarnedAndFailsAtApply(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the CLI sequence runs on unix")
	}
	for _, tc := range []struct{ typeName, name string }{
		{clusterType, "acc-taken-cluster"},
		{instanceGroupType, "acc-taken-group"},
	} {
		t.Run(tc.typeName, func(t *testing.T) {
			holder, other := newCLIRun(t), newCLIRun(t)
			holder.write(heldObject(tc.typeName, tc.name, "held"))
			if out, ok := holder.run("apply", "-auto-approve"); !ok {
				t.Fatalf("the holder's apply failed:\n%s", out)
			}
			t.Cleanup(func() {
				if out, ok := holder.run("destroy", "-auto-approve"); !ok {
					t.Errorf("cleanup destroy failed:\n%s", out)
				}
			})

			other.write(heldObject(tc.typeName, tc.name, "held"))
			out, ok := other.run("plan")
			if !ok {
				t.Fatalf("the second state's plan was refused:\n%s", out)
			}
			mustSay(t, "the second state's plan", out, "1 to add, 0 to change, 0 to destroy",
				"applying this plan replaces it if this state holds it",
				"If this state doesn't hold it", `the create fails with "already exists" and nothing is deleted`)

			out, ok = other.run("apply", "-auto-approve")
			if ok {
				t.Fatalf("the second state's apply passed over a taken id:\n%s", out)
			}
			mustSay(t, "the second state's apply", out, "already exists")
			if listed, _ := other.run("state", "list"); strings.TrimSpace(listed) != "" {
				t.Errorf("the second state took the object:\n%s", listed)
			}

			if out, ok := holder.run("plan", "-detailed-exitcode"); !ok {
				t.Errorf("the holder's replan isn't empty after the second state's apply:\n%s", out)
			}
		})
	}
}

// A tag change sends the cluster's tags whole, and the create token isn't in
// state, so the PATCH has to carry it back or the platform drops it. A cluster
// without its token reads as made outside Terraform, and the plan that
// destroys it once it's tainted is refused. The token stays out of the plan
// and state either way.
func TestAccATagChangeKeepsTheCreateToken(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the CLI sequence runs on unix")
	}
	const name = "acc-retagged"
	path := "/v1/regions/us-texas-1/projects/demo/compute/clusters/" + name
	c := newCLIRun(t)
	base := heldObject(clusterType, name, "retag")
	withTags := func(tags string) string {
		return strings.Replace(base, "  timeouts {", "  tags = "+tags+"\n  timeouts {", 1)
	}
	destroyed := false
	t.Cleanup(func() {
		if destroyed {
			return
		}
		c.run("untaint", clusterType+".test")
		if out, ok := c.run("destroy", "-auto-approve"); !ok {
			t.Errorf("cleanup destroy failed:\n%s", out)
		}
	})
	c.write(base)
	if out, ok := c.run("apply", "-auto-approve"); !ok {
		t.Fatalf("apply:\n%s", out)
	}
	for _, step := range []struct{ tags, want string }{
		{`{ a = "b" }`, "a"},
		{`{}`, ""},
	} {
		c.write(withTags(step.tags))
		if out, ok := c.run("apply", "-auto-approve"); !ok {
			t.Fatalf("apply with tags %s:\n%s", step.tags, out)
		}
		tags := fakeTags(t, path)
		if tags[createTokenKey] == "" {
			t.Errorf("tags %s: the cluster lost its create token: %v", step.tags, tags)
		}
		if step.want != "" && tags[step.want] == "" {
			t.Errorf("tags %s: the platform doesn't hold %q: %v", step.tags, step.want, tags)
		}
		if out, ok := c.run("plan", "-detailed-exitcode"); !ok {
			t.Errorf("tags %s: the replan isn't empty:\n%s", step.tags, out)
		}
		if out, _ := c.run("show", "-json"); strings.Contains(out, "create-token") {
			t.Errorf("tags %s: the create token reached state:\n%s", step.tags, out)
		}
	}
	if out, ok := c.run("taint", clusterType+".test"); !ok {
		t.Fatalf("taint:\n%s", out)
	}
	out, ok := c.run("destroy", "-auto-approve")
	if !ok {
		t.Fatalf("the destroy of the retagged, tainted cluster failed:\n%s", out)
	}
	destroyed = true
	mustSay(t, "the destroy", out, "1 destroyed")
}

// fakeTags reads an object's tags from the fake, past the provider.
func fakeTags(t *testing.T, path string) map[string]string {
	t.Helper()
	status, body, err := readFake(path)
	if err != nil || status != http.StatusOK {
		t.Fatalf("reading %s: %d %v", path, status, err)
	}
	var got struct {
		Tags map[string]string `json:"tags"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	return got.Tags
}
