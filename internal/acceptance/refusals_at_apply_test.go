package acceptance

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"runtime"
	"slices"
	"testing"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/provider"
)

// Refusals of a change that deletes nothing: a file store's shrink, a
// cluster's lower storage ceiling, and a create in a region the project isn't
// in. `terraform destroy` runs a normal plan first, so a refusal at plan would
// stop it too. Each plans with a warning instead, and the apply fails before
// it sends anything.

const (
	shrinkRefused  = "A file store can't be shrunk"
	ceilingRefused = "The storage ceiling can't be lowered"
	regionRefused  = "This resource's region isn't its project's"
	// The rest of each warning and refusal: what the apply does, and what it
	// leaves.
	destroyUnaffected = "`terraform destroy` isn't affected"
	restApplied       = "Terraform skips the resources that depend on it. The rest of the plan is applied."
	restAppliedNow    = "Terraform skips the resources that depend on it and applies the rest of the plan."
	createWarned      = "the apply fails before it sends the create, so this resource isn't created"
	createSentNothing = "This apply sent nothing to create this resource, so it doesn't exist."
)

// updateWarned is what the plan's warning of a refused update says the apply
// does, for a resource of kind what.
func updateWarned(what string) string {
	return fmt.Sprintf("the apply fails before it sends any request for this %s, so none of its changes "+
		"in this plan are made", what)
}

// updateSentNothing is what the apply's refusal of an update says it did.
func updateSentNothing(what string) string {
	return fmt.Sprintf("This apply sent nothing for this %s, so it's unchanged.", what)
}

// sentNoneOf counts each method on path from the moment it's called, and
// the function it returns fails the test for any sent since.
func sentNoneOf(t *testing.T, path string, methods ...string) func(step string) {
	t.Helper()
	counters := make([]*requestCounter, 0, len(methods))
	for _, m := range methods {
		c := &requestCounter{method: m, path: path}
		c.start(t)()
		counters = append(counters, c)
	}
	return func(step string) {
		t.Helper()
		for _, c := range counters {
			if n, err := fakeServed(c.method, c.path); err != nil || n != c.before {
				t.Errorf("%s sent %d %s %s: %v", step, n-c.before, c.method, c.path, err)
			}
		}
	}
}

// destroyRemoves runs `terraform destroy` and checks it removed n
// resources and that the fake no longer holds path.
func (c *cliRun) destroyRemoves(t *testing.T, what string, n int, path string) {
	t.Helper()
	out, ok := c.run("destroy", "-auto-approve")
	if !ok {
		t.Fatalf("the destroy of %s failed:\n%s", what, out)
	}
	mustSay(t, "the destroy", out, fmt.Sprintf("Destroy complete! Resources: %d destroyed.", n))
	if err := checkGoneAt(path)(nil); err != nil {
		t.Error(err)
	}
}

// cleanupDestroy destroys from state as it is at the end of the test,
// unless done says the test's own destroy already ran.
func (c *cliRun) cleanupDestroy(t *testing.T, done *bool) {
	t.Cleanup(func() {
		if *done {
			return
		}
		if out, ok := c.run("destroy", "-auto-approve", "-refresh=false"); !ok {
			t.Errorf("cleanup destroy failed:\n%s", out)
		}
	})
}

func skipOnWindows(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		skipTest(t, "the CLI sequence runs on unix")
	}
}

func fileStoreAPIPath(name string) string {
	return "/v1/regions/us-texas-1/projects/demo/storage/file-stores/" + name
}

// A shrink plans as an update with a warning, and the apply fails before it
// sends any request for the store, so its other changes in the same apply
// aren't made either. Through the real CLI, for each change that would
// otherwise go out with it:
//
//  1. `terraform apply` makes the store at 2048 GiB with one consumer
//  2. the configuration asks for 1024, with the change
//  3. `terraform plan -detailed-exitcode` exits 2 with the warning
//  4. `terraform apply` exits 1 with the refusal and sends nothing
//  5. the platform's store is as it was, and `terraform destroy` removes it
func TestAccAFileStoreShrinkWarnsAtPlanAndFailsAtApply(t *testing.T) {
	t.Parallel()
	skipOnWindows(t)
	for i, tc := range []struct {
		name, display, consumers string
	}{
		{"alone", "acceptance", oneConsumer},
		{"with a display name", "renamed", oneConsumer},
		{"with a consumer", "acceptance", twoConsumers},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			name := fmt.Sprintf("acc-shrink-apply-%d", i)
			path := fileStoreAPIPath(name)
			c := newCLIRun(t)
			done := false
			c.cleanupDestroy(t, &done)
			c.write(fileStoreConfig(name, "acceptance", 2048, oneConsumer))
			if out, ok := c.run("apply", "-auto-approve"); !ok {
				t.Fatalf("the create failed:\n%s", out)
			}

			c.write(fileStoreConfig(name, tc.display, 1024, tc.consumers))
			sentNothing := sentNoneOf(t, path, http.MethodPatch, http.MethodDelete)
			out, code := c.runCode("plan", "-detailed-exitcode")
			if code != 2 {
				t.Errorf("the plan exited %d, want 2, a plan with changes:\n%s", code, out)
			}
			mustSay(t, "the plan", out, "Warning: "+shrinkRefused, "goes from 2048 to 1024", updateWarned("file store"),
				restApplied, destroyUnaffected, "Set `capacity_gib` back to 2048", "0 to add, 1 to change, 0 to destroy")
			sentNothing("the plan")

			out, code = c.runCode("apply", "-auto-approve")
			if code != 1 {
				t.Errorf("the apply exited %d, want 1:\n%s", code, out)
			}
			mustSay(t, "the apply", out, "Error: "+shrinkRefused, "goes from 2048 to 1024",
				updateSentNothing("file store"), "Set `capacity_gib` back to 2048")
			sentNothing("the apply")

			status, body, err := readFakeStore(path)
			if err != nil || status != http.StatusOK {
				t.Fatalf("reading the store: %d %v", status, err)
			}
			var got struct {
				CapacityGiB int    `json:"capacityGiB"`
				DisplayName string `json:"displayName"`
				Consumers   []any  `json:"consumers"`
			}
			if err := json.Unmarshal(body, &got); err != nil {
				t.Fatal(err)
			}
			if got.CapacityGiB != 2048 || got.DisplayName != "acceptance" || len(got.Consumers) != 1 {
				t.Errorf("the store changed: %s", body)
			}

			c.destroyRemoves(t, "the store", 1, path)
			done = true
		})
	}
}

// When the apply refuses one resource, Terraform skips the resources that
// depend on it and applies the rest of the plan, as each warning and refusal
// says. Through the real CLI:
//
//  1. `terraform apply` makes a store at 2048 GiB
//  2. the configuration shrinks it, and adds one store that depends on it
//     and one that doesn't
//  3. `terraform apply` exits 1 with the shrink's refusal
//  4. the platform holds the store that doesn't depend on it, and not the
//     one that does
func TestAccARefusedUpdateSkipsOnlyTheResourcesThatDependOnIt(t *testing.T) {
	t.Parallel()
	skipOnWindows(t)
	const name = "acc-shrink-rest"
	c := newCLIRun(t)
	done := false
	c.cleanupDestroy(t, &done)
	c.write(fileStoreConfig(name, "acceptance", 2048, oneConsumer))
	if out, ok := c.run("apply", "-auto-approve"); !ok {
		t.Fatalf("the create failed:\n%s", out)
	}

	added := func(address, id, display string) string {
		return fmt.Sprintf(`
resource %q %q {
  id           = %q
  display_name = %s
  share_type   = "general"
  capacity_gib = 100
  consumers    = %s
}
`, fileStoreType, address, id, display, oneConsumer)
	}
	c.write(fileStoreConfig(name, "acceptance", 1024, oneConsumer) +
		added("apart", name+"-apart", `"apart"`) +
		added("after", name+"-after", fileStoreType+".test.display_name"))
	out, code := c.runCode("apply", "-auto-approve")
	if code != 1 {
		t.Errorf("the apply exited %d, want 1:\n%s", code, out)
	}
	mustSay(t, "the apply", out, "Error: "+shrinkRefused, updateSentNothing("file store"), restAppliedNow)

	if status, _, err := readFakeStore(fileStoreAPIPath(name + "-apart")); err != nil || status != http.StatusOK {
		t.Errorf("the store that doesn't depend on the refused one: %d %v, want it created", status, err)
	}
	if status, _, err := readFakeStore(fileStoreAPIPath(name + "-after")); err != nil || status != http.StatusNotFound {
		t.Errorf("the store that depends on the refused one: %d %v, want it skipped", status, err)
	}

	c.write(fileStoreConfig(name, "acceptance", 2048, oneConsumer) + added("apart", name+"-apart", `"apart"`))
	c.destroyRemoves(t, "both stores", 2, fileStoreAPIPath(name+"-apart"))
	done = true
}

// With `capacity_gib` lower than the store's, the plan warns and `terraform
// destroy` removes the store, in each state the platform can report it in.
// Through the real CLI, for each state:
//
//  1. the store is made, by `terraform apply` or outside Terraform and
//     imported, and is in the state
//  2. the configuration asks for less capacity
//  3. `terraform plan -detailed-exitcode` exits 2 with the warning
//  4. `terraform destroy` removes the store, and the platform no longer has it
func TestAccAFileStoreShrinkDoesNotStopADestroy(t *testing.T) {
	t.Parallel()
	skipOnWindows(t)
	for _, tc := range []struct {
		state, display string
	}{
		// Held `creating` until a write is refused as busy.
		{"creating", busyUntilRefused},
		{"active", "acceptance"},
		{"failed", "acceptance " + settlesFailed},
	} {
		t.Run(tc.state, func(t *testing.T) {
			t.Parallel()
			name := "acc-shrink-destroy-" + tc.state
			path := fileStoreAPIPath(name)
			c := newCLIRun(t)
			done := false
			c.cleanupDestroy(t, &done)
			config := func(capacity int) string {
				return fmt.Sprintf(`
provider %q {
  operation_timeout = "1m"
}
`, provider.ProviderTypeName) + fileStoreConfig(name, tc.display, capacity, oneConsumer)
			}

			if tc.state == "active" {
				c.write(config(2048))
				if out, ok := c.run("apply", "-auto-approve"); !ok {
					t.Fatalf("the create failed:\n%s", out)
				}
			} else {
				if err := postFake("/v1/regions/us-texas-1/projects/demo/storage/file-stores", fmt.Sprintf(
					`{"id":%q,"shareType":"general","capacityGiB":2048,"displayName":%q,`+
						`"consumers":[{"ref":"10.0.0.31","mountPath":"/mnt/datasets"}]}`, name, tc.display)); err != nil {
					t.Fatal(err)
				}
				if tc.state == "failed" {
					awaitFakeState(t, path, "failed")
				}
				c.write(config(2048))
				if out, ok := c.run("import", fileStoreAddr, scopedImport(name)); !ok {
					t.Fatalf("the import failed:\n%s", out)
				}
			}

			c.write(config(1024))
			out, code := c.runCode("plan", "-detailed-exitcode")
			if code != 2 {
				t.Errorf("the plan of the %s store exited %d, want 2:\n%s", tc.state, code, out)
			}
			mustSay(t, "the plan", out, "Warning: "+shrinkRefused, destroyUnaffected)
			if tc.state == "creating" {
				// The plan read it without moving it on. A write refused as
				// busy lets it settle a few reads on, so the destroy's own
				// plan still finds it `creating` and its delete waits it out.
				awaitFakeState(t, path, "creating")
				if err := patchFake(path, `{"description":"busy"}`); err == nil {
					t.Fatal("a write to the creating store wasn't refused")
				}
			}

			c.destroyRemoves(t, "the "+tc.state+" store", 1, path)
			done = true
		})
	}
}

// A lower storage ceiling plans as an update with a warning, and the apply
// fails before it sends any request for the cluster, so its other changes in
// the same apply aren't made either. Through the real CLI, for each change that would otherwise go
// out with it:
//
//  1. `terraform apply` makes the cluster with a 500 GiB ceiling
//  2. the configuration asks for 100, with the change
//  3. `terraform plan -detailed-exitcode` exits 2 with the warning
//  4. `terraform apply` exits 1 with the refusal and sends nothing
//  5. the platform's cluster is as it was, and `terraform destroy` removes it
func TestAccALoweredStorageCeilingWarnsAtPlanAndFailsAtApply(t *testing.T) {
	t.Parallel()
	skipOnWindows(t)
	for i, tc := range []struct {
		name    string
		display string
		workers int
	}{
		{"alone", "acceptance", 1},
		{"with a node count", "acceptance", 2},
		{"with a display name", "renamed", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			name := fmt.Sprintf("acc-lowered-ceiling-%d", i)
			path := clusterAPIPath(name)
			c := newCLIRun(t)
			done := false
			c.cleanupDestroy(t, &done)
			c.write(versionedCluster(name, clusterTypeV2, "acceptance", 1, 500))
			if out, ok := c.run("apply", "-auto-approve"); !ok {
				t.Fatalf("the create failed:\n%s", out)
			}

			c.write(versionedCluster(name, clusterTypeV2, tc.display, tc.workers, 100))
			sentNothing := clusterWrites(t, path)
			out, code := c.runCode("plan", "-detailed-exitcode")
			if code != 2 {
				t.Errorf("the plan exited %d, want 2, a plan with changes:\n%s", code, out)
			}
			mustSay(t, "the plan", out, "Warning: "+ceilingRefused, "is 500 GiB and the configuration asks for 100",
				updateWarned("cluster"), restApplied, destroyUnaffected, "0 to add, 1 to change, 0 to destroy")
			sentNothing("the plan")

			out, code = c.runCode("apply", "-auto-approve")
			if code != 1 {
				t.Errorf("the apply exited %d, want 1:\n%s", code, out)
			}
			mustSay(t, "the apply", out, "Error: "+ceilingRefused, updateSentNothing("cluster"))
			sentNothing("the apply")

			status, body, err := readFakeStore(path)
			if err != nil || status != http.StatusOK {
				t.Fatalf("reading the cluster: %d %v", status, err)
			}
			var got struct {
				DisplayName string `json:"displayName"`
				NodePools   []struct {
					ID        string `json:"id"`
					NodeCount int    `json:"nodeCount"`
				} `json:"nodePools"`
				Storage struct {
					CeilingGiB int `json:"ceilingGiB"`
				} `json:"storage"`
			}
			if err := json.Unmarshal(body, &got); err != nil {
				t.Fatal(err)
			}
			workers := -1
			for _, p := range got.NodePools {
				if p.ID == "gpu-workers" {
					workers = p.NodeCount
				}
			}
			if got.DisplayName != "acceptance" || workers != 1 || got.Storage.CeilingGiB != 500 {
				t.Errorf("the cluster changed: %s", body)
			}

			c.destroyRemoves(t, "the cluster", 1, path)
			done = true
		})
	}
}

// With `storage_ceiling_gib` lower than the cluster's, the plan warns and
// `terraform destroy` removes the cluster, in each state the platform can
// report it in. Through the real CLI, for each state:
//
//  1. the cluster is made with a 500 GiB ceiling, by `terraform apply` or
//     outside Terraform and imported, and taken to the state
//  2. the configuration asks for 100
//  3. `terraform plan -detailed-exitcode` exits 2 with the warning
//  4. `terraform destroy` removes the cluster, and the platform no longer has it
func TestAccALoweredStorageCeilingDoesNotStopADestroy(t *testing.T) {
	t.Parallel()
	skipOnWindows(t)
	for _, tc := range []struct {
		state, display string
	}{
		{"creating", neverSettles},
		{"updating", "acceptance " + holdsUpgrade},
		{"active", "acceptance"},
		{"failed", createFails},
	} {
		t.Run(tc.state, func(t *testing.T) {
			t.Parallel()
			name := "acc-lowered-ceiling-destroy-" + tc.state
			path := clusterAPIPath(name)
			c := newCLIRun(t)
			done := false
			c.cleanupDestroy(t, &done)

			if tc.state == "active" {
				c.write(versionedCluster(name, clusterTypeV2, tc.display, 2, 500))
				if out, ok := c.run("apply", "-auto-approve"); !ok {
					t.Fatalf("the create failed:\n%s", out)
				}
			} else {
				want := tc.state
				if tc.state == "updating" {
					want = "active"
				}
				if err := postFake("/v1/regions/us-texas-1/projects/demo/compute/clusters", fmt.Sprintf(
					`{"id":%q,"clusterType":%q,"displayName":%q,"storage":{"ceilingGiB":500},`+
						`"nodePools":[{"id":"gpu-workers","nodeCount":2}]}`, name, clusterTypeV2, tc.display)); err != nil {
					t.Fatal(err)
				}
				awaitFakeState(t, path, want)
				c.write(versionedCluster(name, clusterTypeV2, tc.display, 2, 500))
				if out, ok := c.run("import", clusterType+".test", scopedImport(name)); !ok {
					t.Fatalf("the import failed:\n%s", out)
				}
			}
			if tc.state == "updating" {
				if err := postFake(path+"/upgrade", `{}`); err != nil {
					t.Fatal(err)
				}
				awaitFakeState(t, path, "updating")
			}

			c.write(versionedCluster(name, clusterTypeV2, tc.display, 2, 100))
			out, code := c.runCode("plan", "-detailed-exitcode")
			if code != 2 {
				t.Errorf("the plan of the %s cluster exited %d, want 2:\n%s", tc.state, code, out)
			}
			mustSay(t, "the plan", out, "Warning: "+ceilingRefused, destroyUnaffected)

			c.destroyRemoves(t, "the "+tc.state+" cluster", 1, path)
			done = true
		})
	}
}

// regionNetwork is a network block at address, in region when it isn't
// empty, else the provider's.
func regionNetwork(address, name, region string) string {
	scope := ""
	if region != "" {
		scope = fmt.Sprintf("  region  = %q\n  project = \"demo\"\n", region)
	}
	return fmt.Sprintf(`
resource %q %q {
  id = %q
%s  evpn = {
    service_type = "l3vpn"
    vlan_id      = "auto"
  }
}
`, networkType, address, name, scope)
}

// A create in a region its project isn't in plans with a warning, and the
// apply fails before it sends the create, while the rest of the apply goes
// ahead. The fake's project `demo` is in us-texas-1. Through the real CLI:
//
//  1. `terraform apply` makes one network in the project's region
//  2. the configuration adds a network in another region
//  3. `terraform plan -detailed-exitcode` exits 2 with the warning
//  4. `terraform apply` exits 1 with the refusal, and sends no create there
//  5. `terraform destroy` removes the first network
func TestAccACreateInAnotherRegionThanItsProjectWarnsAtPlanAndFailsAtApply(t *testing.T) {
	t.Parallel()
	skipOnWindows(t)
	const name, region = "acc-region-apply", "eu-refused-1"
	wrong := "/v1/regions/" + region + "/projects/demo/networking/networks"
	c := newCLIRun(t)
	done := false
	c.cleanupDestroy(t, &done)
	kept := regionNetwork("kept", name+"-kept", "")
	c.write(kept)
	if out, ok := c.run("apply", "-auto-approve"); !ok {
		t.Fatalf("the create failed:\n%s", out)
	}

	c.write(kept + regionNetwork("refused", name, region))
	sentNothing := sentNoneOf(t, wrong, http.MethodPost)
	out, code := c.runCode("plan", "-detailed-exitcode")
	if code != 2 {
		t.Errorf("the plan exited %d, want 2, a plan with changes:\n%s", code, out)
	}
	mustSay(t, "the plan", out, "Warning: "+regionRefused, `Project "demo" is in region "us-texas-1"`,
		`would be created in "eu-refused-1"`, createWarned, restApplied, destroyUnaffected,
		"1 to add, 0 to change, 0 to destroy")
	sentNothing("the plan")

	out, code = c.runCode("apply", "-auto-approve")
	if code != 1 {
		t.Errorf("the apply exited %d, want 1:\n%s", code, out)
	}
	mustSay(t, "the apply", out, "Error: "+regionRefused, `Project "demo" is in region "us-texas-1"`,
		createSentNothing)
	sentNothing("the apply")

	c.destroyRemoves(t, "the network", 1, networkPath(map[string]string{
		"region": "us-texas-1", "project": "demo", "id": name + "-kept"}))
	done = true
}

// With a network in a region its project isn't in, the plan warns and
// `terraform destroy` removes what state holds. Through the real CLI, for
// each way the network can be in the configuration:
//
//   - never created, beside a network that was
//   - created in the project's region, then tainted, and moved, so its
//     replacement is a create
//
// in each, `terraform plan -detailed-exitcode` exits 2 with the warning, and
// `terraform destroy` removes the network state holds.
func TestAccACreateInAnotherRegionThanItsProjectDoesNotStopADestroy(t *testing.T) {
	t.Parallel()
	skipOnWindows(t)
	for _, tc := range []struct {
		name    string
		tainted bool
	}{
		{"never created", false},
		{"tainted", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			name := "acc-region-destroy-new"
			if tc.tainted {
				name = "acc-region-destroy-tainted"
			}
			const region = "eu-refused-2"
			c := newCLIRun(t)
			done := false
			c.cleanupDestroy(t, &done)
			var made, refused, gone string
			if tc.tainted {
				made = regionNetwork("refused", name, "")
				refused = regionNetwork("refused", name, region)
				gone = name
			} else {
				made = regionNetwork("kept", name+"-kept", "")
				refused = made + regionNetwork("refused", name, region)
				gone = name + "-kept"
			}
			c.write(made)
			if out, ok := c.run("apply", "-auto-approve"); !ok {
				t.Fatalf("the create failed:\n%s", out)
			}
			if tc.tainted {
				if out, ok := c.run("taint", networkType+".refused"); !ok {
					t.Fatalf("the taint failed:\n%s", out)
				}
			}

			c.write(refused)
			out, code := c.runCode("plan", "-detailed-exitcode")
			if code != 2 {
				t.Errorf("the plan exited %d, want 2:\n%s", code, out)
			}
			mustSay(t, "the plan", out, "Warning: "+regionRefused, destroyUnaffected)

			c.destroyRemoves(t, "the network", 1, networkPath(map[string]string{
				"region": "us-texas-1", "project": "demo", "id": gone}))
			done = true
		})
	}
}

// replacementRefused is the summary of a delete refused because the create
// after it would be.
const replacementRefused = "This replacement would be refused, so nothing was deleted"

// stateOf is addr's status in this run's state, "" for present and clean,
// "tainted" or "absent", and the keys of its private state.
func (c *cliRun) stateOf(addr string) (string, []string) {
	c.t.Helper()
	out, err := c.command("state", "pull").Output()
	if err != nil {
		c.t.Fatalf("terraform state pull: %v", err)
	}
	var st struct {
		Resources []struct {
			Type, Name string
			Instances  []struct {
				Status  string `json:"status"`
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
		var keys []string
		if raw, err := base64.StdEncoding.DecodeString(r.Instances[0].Private); err == nil && len(raw) > 0 {
			var private map[string]json.RawMessage
			if json.Unmarshal(raw, &private) == nil {
				for k := range private {
					keys = append(keys, k)
				}
			}
		}
		return r.Instances[0].Status, keys
	}
	return "absent", nil
}

// A tainted resource whose replacement the apply would refuse is kept: the
// apply refuses the delete that starts the replacement, before it sends
// anything. Through the real CLI, for a network moved to a region its
// project isn't in:
//
//  1. `terraform apply` makes the network, and `terraform taint` marks it
//  2. the configuration moves it to another region
//  3. `terraform plan -detailed-exitcode` exits 2 with the warning
//  4. `terraform apply` exits 1 with the replacement's refusal, sends no
//     delete and no create, and the network stays, tainted, with nothing
//     left in its private state
//  5. back in the project's region, `terraform apply` replaces it, and its
//     private state keeps nothing of the plan's checks
//  6. `terraform destroy` removes it
func TestAccATaintedReplacementTheApplyWouldRefuseIsKept(t *testing.T) {
	t.Parallel()
	skipOnWindows(t)
	const name = "acc-region-tainted-kept"
	addr := networkType + ".test"
	kept := networkPath(map[string]string{"region": "us-texas-1", "project": "demo", "id": name})
	c := newCLIRun(t)
	done := false
	c.cleanupDestroy(t, &done)
	c.write(regionNetwork("test", name, ""))
	if out, ok := c.run("apply", "-auto-approve"); !ok {
		t.Fatalf("the create failed:\n%s", out)
	}
	if out, ok := c.run("taint", addr); !ok {
		t.Fatalf("the taint failed:\n%s", out)
	}

	c.write(regionNetwork("test", name, "eu-refused-1"))
	out, code := c.runCode("plan", "-detailed-exitcode")
	if code != 2 {
		t.Errorf("the plan exited %d, want 2:\n%s", code, out)
	}
	mustSay(t, "the plan", out, "Warning: "+regionRefused, createWarned,
		"If this replaces a tainted object, the apply checks before it deletes that object, and keeps it.")

	sentNothing := sentNoneOf(t, kept, http.MethodDelete)
	sentNoCreate := sentNoneOf(t, "/v1/regions/eu-refused-1/projects/demo/networking/networks", http.MethodPost)
	out, code = c.runCode("apply", "-auto-approve")
	if code != 1 {
		t.Errorf("the apply exited %d, want 1:\n%s", code, out)
	}
	mustSay(t, "the apply", out, "Error: "+replacementRefused, `Project "demo" is in region "us-texas-1"`,
		"this apply sent nothing for this resource, and it's still on the platform and in state",
		"`terraform destroy` still deletes it")
	sentNothing("the apply")
	sentNoCreate("the apply")
	if status, _, err := readFakeStore(kept); err != nil || status != http.StatusOK {
		t.Errorf("the network after the refused apply: %d %v, want it kept", status, err)
	}
	if status, keys := c.stateOf(addr); status != "tainted" || slices.Contains(keys, "replacement_checks") {
		t.Errorf("state after the refused apply: %q with private keys %v, want tainted with no checks", status, keys)
	}

	c.write(regionNetwork("test", name, ""))
	if out, ok := c.run("apply", "-auto-approve"); !ok {
		t.Fatalf("the replacement in the project's region failed:\n%s", out)
	}
	if status, keys := c.stateOf(addr); status != "" || slices.Contains(keys, "replacement_checks") {
		t.Errorf("state after the replacement: %q with private keys %v, want clean with no checks", status, keys)
	}

	c.destroyRemoves(t, "the network", 1, kept)
	done = true
}
