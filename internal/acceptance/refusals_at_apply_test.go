package acceptance

import (
	"encoding/json"
	"fmt"
	"net/http"
	"runtime"
	"testing"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/provider"
)

// Refusals of a change that deletes nothing: a file store's shrink, and a
// create in a region the project isn't in. `terraform destroy` runs a normal
// plan first, so a refusal at plan would stop it too. Each plans with a
// warning instead, and the apply fails before it sends anything.

const (
	shrinkRefused = "A file store cannot be shrunk"
	regionRefused = "This resource's region isn't its project's"
	// warnsAtPlan and destroyUnaffected are what each warning says.
	warnsAtPlan       = "fails before it sends any request, other changes included"
	destroyUnaffected = "`terraform destroy` isn't affected"
)

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
// sends any request, the other changes in the same apply included. Through
// the real CLI, for each change that would otherwise go out with it:
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
			mustSay(t, "the plan", out, "Warning: "+shrinkRefused, "goes from 2048 to 1024", warnsAtPlan,
				destroyUnaffected, "set `capacity_gib` to 2048", "0 to add, 1 to change, 0 to destroy")
			sentNothing("the plan")

			out, code = c.runCode("apply", "-auto-approve")
			if code != 1 {
				t.Errorf("the apply exited %d, want 1:\n%s", code, out)
			}
			mustSay(t, "the apply", out, "Error: "+shrinkRefused, "goes from 2048 to 1024", "This apply sent nothing")
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
		`would be created in "eu-refused-1"`, "fails before it creates anything", destroyUnaffected,
		"1 to add, 0 to change, 0 to destroy")
	sentNothing("the plan")

	out, code = c.runCode("apply", "-auto-approve")
	if code != 1 {
		t.Errorf("the apply exited %d, want 1:\n%s", code, out)
	}
	mustSay(t, "the apply", out, "Error: "+regionRefused, `Project "demo" is in region "us-texas-1"`,
		"This apply sent nothing for this resource")
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
