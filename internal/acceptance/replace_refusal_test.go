package acceptance

import (
	"fmt"
	"net/http"
	"runtime"
	"strings"
	"testing"
)

// A replacement the configuration doesn't permit plans as an update with a
// warning, and the apply refuses it before it sends any request, so the
// plan that `terraform destroy` runs first never fails on it. Each scenario
// runs through the real CLI against its own objects, counting what the fake
// is sent.

// refusalKind is one guarded resource the CLI scenarios run on.
type refusalKind struct {
	addr, noun string
	// attr is the attribute whose change replaces the resource, and its
	// value before and after.
	attr, before, after string
	// config is the whole configuration. changed makes the replacing change,
	// and extra is added inside the resource's block.
	config func(name string, changed bool, extra string) string
	// without is the configuration with the resource's block removed.
	without func(name string) string
	// object is the API path of the resource as first applied, and
	// collection where a create of it is sent.
	object, collection func(name string) string
	// setup runs before the first apply, for what the configuration needs.
	setup func(t *testing.T, name string)
}

// refusalSubnet replaces on `description`, the mildest edit that does, since
// the API has no update for a subnet. Its network is its own, so the
// subnet collection a create would POST to is this test's alone.
func refusalSubnet() refusalKind {
	block := func(name, description, extra string) string {
		return fmt.Sprintf(`
resource %q "test" {
  id               = %q
  network          = %s.test.id
  address_pool     = "/v1/regions/us-texas-1/networking/address-pools/tenant"
  prefix_length    = 24
  allocate_gateway = true
  description      = %q
%s
}
`, subnetType, name, networkType, description, extra)
	}
	return refusalKind{
		addr: subnetAddr, noun: "subnet", attr: "description", before: "before", after: "after",
		config: func(name string, changed bool, extra string) string {
			description := "before"
			if changed {
				description = "after"
			}
			return evpnConfig(name+"-net", "acceptance") + block(name, description, extra)
		},
		without: func(name string) string { return evpnConfig(name+"-net", "acceptance") },
		object: func(name string) string {
			return subnetPath(map[string]string{"region": "us-texas-1", "project": "demo", "network": name + "-net", "id": name})
		},
		collection: func(name string) string {
			return networkPath(map[string]string{"region": "us-texas-1", "project": "demo", "id": name + "-net"}) + "/subnets"
		},
	}
}

// refusalCluster replaces on a renamed `id`. It's in a project of its own,
// so the cluster collection a create would POST to is this test's alone.
func refusalCluster() refusalKind {
	project := func(name string) string { return name + "-p" }
	return refusalKind{
		addr: clusterAddr, noun: "cluster", attr: "id",
		config: func(name string, changed bool, extra string) string {
			id := name
			if changed {
				id = name + "-2"
			}
			return fmt.Sprintf(`
resource %q "test" {
  id           = %q
  project      = %q
  cluster_type = %q
  display_name = "acceptance"
  node_pool = {
    "gpu-workers" = { node_count = 1 }
  }
%s
}
`, clusterType, id, project(name), clusterTypeRef, extra)
		},
		without: func(string) string { return "" },
		object: func(name string) string {
			return fmt.Sprintf("/v1/regions/us-texas-1/projects/%s/compute/clusters/%s", project(name), name)
		},
		collection: func(name string) string {
			return fmt.Sprintf("/v1/regions/us-texas-1/projects/%s/compute/clusters", project(name))
		},
		setup: func(t *testing.T, name string) { ownProject(t, project(name)) },
	}
}

func (k refusalKind) values(name string) (before, after string) {
	if k.attr == "id" {
		return name, name + "-2"
	}
	return k.before, k.after
}

// refusalRun is one scenario's working directory and its objects.
type refusalRun struct {
	*cliRun
	k    refusalKind
	name string
}

// startRefusal applies the resource as configured before the change, and
// destroys whatever is left at the end, from state as it is.
func startRefusal(t *testing.T, k refusalKind, name string) *refusalRun {
	t.Helper()
	if runtime.GOOS == "windows" {
		skipTest(t, "the CLI sequence runs on unix")
	}
	r := &refusalRun{cliRun: newCLIRun(t), k: k, name: name}
	if k.setup != nil {
		k.setup(t, name)
	}
	r.write(k.config(name, false, ""))
	if out, ok := r.run("apply", "-auto-approve"); !ok {
		t.Fatalf("the create failed:\n%s", out)
	}
	t.Cleanup(func() {
		if out, ok := r.run("destroy", "-auto-approve", "-refresh=false"); !ok {
			t.Errorf("cleanup destroy failed:\n%s", out)
		}
	})
	return r
}

// served is how many method requests to path the fake has served so far.
func (r *refusalRun) served(method, path string) int {
	r.t.Helper()
	n, err := fakeServed(method, path)
	if err != nil {
		r.t.Fatal(err)
	}
	return n
}

// sends is the deletes of the object and the creates in its collection that
// step makes.
func (r *refusalRun) sends(step func()) (deletes, creates int) {
	r.t.Helper()
	d0, c0 := r.served(http.MethodDelete, r.k.object(r.name)), r.served(http.MethodPost, r.k.collection(r.name))
	step()
	return r.served(http.MethodDelete, r.k.object(r.name)) - d0, r.served(http.MethodPost, r.k.collection(r.name)) - c0
}

// holds checks state still has the value the resource was applied with.
func (r *refusalRun) holds(step string) {
	r.t.Helper()
	before, _ := r.k.values(r.name)
	out, ok := r.run("state", "show", r.k.addr)
	if !ok {
		r.t.Fatalf("%s: terraform state show failed:\n%s", step, out)
	}
	mustSay(r.t, step+": the state", out, fmt.Sprintf("%s = %q", r.k.attr, before))
}

// gone checks the fake no longer holds the object as first applied.
func (r *refusalRun) gone(step string) {
	r.t.Helper()
	if err := checkGoneAt(r.k.object(r.name))(nil); err != nil {
		r.t.Errorf("%s: %v", step, err)
	}
}

// refusedAtApply is what the apply's error says.
func (k refusalKind) refusedAtApply() []string {
	return []string{
		"Error: Changing `" + k.attr + "` would replace this " + k.noun + ", so the apply refused it",
		"The apply refused before it sent any request",
		"`terraform destroy` isn't affected",
		"add `allow_replace = true` to this resource",
		"set `" + k.attr + "` back to its current value",
	}
}

// warnedAtPlan is what the plan's warning says.
func (k refusalKind) warnedAtPlan() []string {
	return []string{
		"Warning: Changing `" + k.attr + "` would replace this " + k.noun + ", so the apply will refuse it",
		"the apply refuses that update before it sends any request",
		"`terraform destroy` isn't affected",
		"add `allow_replace = true` to this resource and plan again",
		"set `" + k.attr + "` back to its current value",
	}
}

// replaces checks an apply replaced the object: a delete and a create, and
// the new value in state.
func (r *refusalRun) replaces(step string, args ...string) {
	r.t.Helper()
	var out string
	var code int
	deletes, creates := r.sends(func() { out, code = r.runCode(args...) })
	if code != 0 || deletes == 0 || creates == 0 {
		r.t.Errorf("%s exited %d with %d deletes and %d creates, want a replacement:\n%s", step, code, deletes, creates, out)
	}
}

func (k refusalKind) scenarios() map[string]func(t *testing.T, r *refusalRun) {
	return map[string]func(t *testing.T, r *refusalRun){
		// 1 and 2: the plan warns and sends nothing, the apply refuses and
		// sends nothing, state keeps the value, and `terraform destroy`
		// with the same configuration deletes the object.
		"refused then destroyed": func(t *testing.T, r *refusalRun) {
			r.write(k.config(r.name, true, ""))
			var out string
			var code int
			deletes, creates := r.sends(func() { out, code = r.runCode("plan", "-detailed-exitcode") })
			if code != 2 || deletes+creates != 0 {
				t.Errorf("the plan exited %d with %d deletes and %d creates, want 2 and none:\n%s", code, deletes, creates, out)
			}
			mustSay(t, "the plan", out, append(k.warnedAtPlan(), "0 to add, 1 to change, 0 to destroy")...)

			deletes, creates = r.sends(func() { out, code = r.runCode("apply", "-auto-approve") })
			if code != 1 || deletes+creates != 0 {
				t.Errorf("the apply exited %d with %d deletes and %d creates, want 1 and none:\n%s", code, deletes, creates, out)
			}
			mustSay(t, "the apply", out, k.refusedAtApply()...)
			r.holds("after the refusal")

			deletes, _ = r.sends(func() { out, code = r.runCode("destroy", "-auto-approve") })
			if code != 0 || deletes == 0 {
				t.Errorf("the destroy exited %d with %d deletes, want 0 and a delete:\n%s", code, deletes, out)
			}
			mustSay(t, "the destroy", out, "Destroy complete!")
			r.gone("the destroy")
		},
		// 3: the opt-in in the same configuration as the change replaces.
		"permitted": func(t *testing.T, r *refusalRun) {
			r.write(k.config(r.name, true, "  allow_replace = true"))
			r.replaces("the permitted apply", "apply", "-auto-approve")
		},
		// 4: a saved destroy plan applies.
		"saved destroy plan": func(t *testing.T, r *refusalRun) {
			r.write(k.config(r.name, true, ""))
			if out, code := r.runCode("plan", "-destroy", "-out=destroy.tfplan"); code != 0 {
				t.Fatalf("plan -destroy exited %d:\n%s", code, out)
			}
			var out string
			var code int
			deletes, _ := r.sends(func() { out, code = r.runCode("apply", "destroy.tfplan") })
			if code != 0 || deletes == 0 {
				t.Errorf("applying the saved destroy exited %d with %d deletes:\n%s", code, deletes, out)
			}
			r.gone("the saved destroy")
		},
		// 5: a saved plan of the change is refused when it's applied.
		"saved plan of the change": func(t *testing.T, r *refusalRun) {
			r.write(k.config(r.name, true, ""))
			out, code := r.runCode("plan", "-out=change.tfplan")
			if code != 0 {
				t.Fatalf("the plan exited %d:\n%s", code, out)
			}
			mustSay(t, "the plan", out, k.warnedAtPlan()[0])
			deletes, creates := r.sends(func() { out, code = r.runCode("apply", "change.tfplan") })
			if code != 1 || deletes+creates != 0 {
				t.Errorf("applying the saved plan exited %d with %d deletes and %d creates, want 1 and none:\n%s", code, deletes, creates, out)
			}
			mustSay(t, "the apply", out, k.refusedAtApply()[0])
			r.holds("after the refusal")
		},
		// 6: create_before_destroy would create first. Nothing is sent.
		"create_before_destroy": func(t *testing.T, r *refusalRun) {
			r.write(k.config(r.name, true, "  lifecycle {\n    create_before_destroy = true\n  }"))
			var out string
			var code int
			deletes, creates := r.sends(func() { out, code = r.runCode("apply", "-auto-approve") })
			if code != 1 || deletes+creates != 0 {
				t.Errorf("the apply exited %d with %d deletes and %d creates, want 1 and none:\n%s", code, deletes, creates, out)
			}
			mustSay(t, "the apply", out, k.refusedAtApply()[0])
		},
		// 7: taint is an explicit request for the replacement.
		"taint": func(t *testing.T, r *refusalRun) {
			if out, ok := r.run("taint", k.addr); !ok {
				t.Fatalf("taint failed:\n%s", out)
			}
			r.replaces("the apply of the tainted "+k.noun, "apply", "-auto-approve")
		},
		// 8: so is -replace.
		"-replace": func(t *testing.T, r *refusalRun) {
			r.replaces("apply -replace", "apply", "-auto-approve", "-replace="+k.addr)
		},
		// 9: a removed block deletes.
		"removed block": func(t *testing.T, r *refusalRun) {
			r.write(k.without(r.name))
			var out string
			var code int
			deletes, _ := r.sends(func() { out, code = r.runCode("apply", "-auto-approve") })
			if code != 0 || deletes == 0 {
				t.Errorf("the apply exited %d with %d deletes:\n%s", code, deletes, out)
			}
			r.gone("the removed block")
		},
		// 10: a destroy that skips the refresh deletes.
		"destroy -refresh=false": func(t *testing.T, r *refusalRun) {
			r.write(k.config(r.name, true, ""))
			var out string
			var code int
			deletes, _ := r.sends(func() { out, code = r.runCode("destroy", "-auto-approve", "-refresh=false") })
			if code != 0 || deletes == 0 {
				t.Errorf("the destroy exited %d with %d deletes:\n%s", code, deletes, out)
			}
			r.gone("the destroy")
		},
		// An opt-in known only after apply can't permit the replacement at
		// plan. Terraform's final plan at apply then asks for one, and
		// Terraform stops on that before it sends anything for the
		// resource. Nothing is deleted, and destroy still works.
		"opt-in known only after apply": func(t *testing.T, r *refusalRun) {
			r.write(k.config(r.name, true, "  allow_replace = terraform_data.permit.output") +
				"\nresource \"terraform_data\" \"permit\" {\n  input = true\n}\n")
			var out string
			var code int
			deletes, creates := r.sends(func() { out, code = r.runCode("apply", "-auto-approve") })
			if code != 1 || deletes+creates != 0 {
				t.Errorf("the apply exited %d with %d deletes and %d creates, want 1 and none:\n%s", code, deletes, creates, out)
			}
			mustSay(t, "the apply", out, "known only after apply, so this plan can't count it",
				"Provider produced inconsistent final plan")
			r.holds("after the apply")
			deletes, _ = r.sends(func() { out, code = r.runCode("destroy", "-auto-approve") })
			if code != 0 || deletes == 0 {
				t.Errorf("the destroy exited %d with %d deletes:\n%s", code, deletes, out)
			}
			r.gone("the destroy")
		},
	}
}

func runRefusalScenarios(t *testing.T, k refusalKind, short string) {
	i := 0
	for scenario, run := range k.scenarios() {
		i++
		name := fmt.Sprintf("acc-refusal-%s-%d", short, i)
		t.Run(scenario, func(t *testing.T) {
			t.Parallel()
			run(t, startRefusal(t, k, name))
		})
	}
}

func TestAccASubnetReplacementIsRefusedAtApply(t *testing.T) {
	t.Parallel()
	runRefusalScenarios(t, refusalSubnet(), "sub")
}

func TestAccAClusterReplacementIsRefusedAtApply(t *testing.T) {
	t.Parallel()
	runRefusalScenarios(t, refusalCluster(), "cl")
}

// The apply refuses the guarded resource's update. Other resources in the
// same apply still change, the subnet's network here, and the plan's warning
// says so.
func TestAccARefusedReplacementLeavesOtherChangesToTheApply(t *testing.T) {
	t.Parallel()
	const name = "acc-refusal-mixed"
	k := refusalSubnet()
	r := startRefusal(t, k, name)
	r.write(strings.Replace(k.config(name, true, ""), `display_name = "acceptance"`, `display_name = "renamed"`, 1))
	network := networkPath(map[string]string{"region": "us-texas-1", "project": "demo", "id": name + "-net"})
	before := r.served(http.MethodPatch, network)
	var out string
	var code int
	deletes, creates := r.sends(func() { out, code = r.runCode("apply", "-auto-approve") })
	if code != 1 || deletes+creates != 0 {
		t.Errorf("the apply exited %d with %d deletes and %d creates, want 1 and none:\n%s", code, deletes, creates, out)
	}
	mustSay(t, "the apply", out, k.refusedAtApply()[0],
		"Other resources in the plan that don't depend on it are still changed")
	if n := r.served(http.MethodPatch, network) - before; n != 1 {
		t.Errorf("the network was sent %d PATCHes, want its rename", n)
	}
	r.holds("after the refusal")
}

// Every guarded kind, through the real CLI: with a change in the
// configuration that would replace it, the plan warns and exits 2, and
// `terraform destroy` deletes it.
func TestAccARefusedReplacementDoesNotStopADestroy(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		skipTest(t, "the CLI sequence runs on unix")
	}
	for _, tc := range []struct {
		kind, noun, attr string
		before, after    string
		object           string
	}{
		{"subnet", "subnet", "description",
			subnetConfig("acc-refusal-d-net", "acc-refusal-d-sub"),
			strings.Replace(subnetConfig("acc-refusal-d-net", "acc-refusal-d-sub"), "prefix_length    = 24", "prefix_length    = 24\n  description      = \"renamed\"", 1),
			subnetPath(map[string]string{"region": "us-texas-1", "project": "demo", "network": "acc-refusal-d-net", "id": "acc-refusal-d-sub"})},
		{"cluster", "cluster", "id", clusterConfig("acc-refusal-d-cl", 1), clusterConfig("acc-refusal-d-cl-2", 1),
			clusterAPIPath("acc-refusal-d-cl")},
		{"instance group", "instance group", "node_pool",
			instanceGroupConfig("acc-refusal-d-ig", 2), instanceGroupConfig("acc-refusal-d-ig", 1),
			instanceGroupPath(map[string]string{"region": "us-texas-1", "project": "demo", "id": "acc-refusal-d-ig"})},
		{"file store", "file store", "share_type",
			fileStoreConfig("acc-refusal-d-fs", "acceptance", 100, oneConsumer),
			strings.Replace(fileStoreConfig("acc-refusal-d-fs", "acceptance", 100, oneConsumer), `"general"`, `"high-speed"`, 1),
			fileStorePath(map[string]string{"region": "us-texas-1", "project": "demo", "id": "acc-refusal-d-fs"})},
		{"network", "network", "evpn", evpnConfig("acc-refusal-d-net2", "acceptance"),
			strings.Replace(evpnConfig("acc-refusal-d-net2", "acceptance"), "mtu          = 9000", "mtu          = 1500", 1),
			networkPath(map[string]string{"region": "us-texas-1", "project": "demo", "id": "acc-refusal-d-net2"})},
		{"project", "project", "id", projectConfig("acc-refusal-d-p", "Acceptance", "guarded"),
			projectConfig("acc-refusal-d-p-2", "Acceptance", "guarded"),
			projectPath(map[string]string{"organization_id": accOrg, "id": "acc-refusal-d-p"})},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			t.Parallel()
			c := newCLIRun(t)
			destroyed := false
			t.Cleanup(func() {
				if destroyed {
					return
				}
				if out, ok := c.run("destroy", "-auto-approve", "-refresh=false"); !ok {
					t.Errorf("cleanup destroy failed:\n%s", out)
				}
			})
			c.write(tc.before)
			if out, ok := c.run("apply", "-auto-approve"); !ok {
				t.Fatalf("the create failed:\n%s", out)
			}
			r := &refusalRun{cliRun: c}
			c.write(tc.after)
			out, code := r.runCode("plan", "-detailed-exitcode")
			if code != 2 {
				t.Errorf("the plan exited %d, want 2, a plan with changes:\n%s", code, out)
			}
			mustSay(t, "the plan", out,
				"Warning: Changing `"+tc.attr+"` would replace this "+tc.noun+", so the apply will refuse it",
				"`terraform destroy` isn't affected")

			deletes := r.served(http.MethodDelete, tc.object)
			out, code = r.runCode("destroy", "-auto-approve")
			if code != 0 {
				t.Fatalf("the destroy exited %d:\n%s", code, out)
			}
			destroyed = true
			mustSay(t, "the destroy", out, "Destroy complete!")
			if r.served(http.MethodDelete, tc.object) == deletes {
				t.Error("the destroy sent no delete")
			}
			if err := checkGoneAt(tc.object)(nil); err != nil {
				t.Error(err)
			}
		})
	}
}
