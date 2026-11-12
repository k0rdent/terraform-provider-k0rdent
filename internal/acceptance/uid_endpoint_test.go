package acceptance

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/knownvalue"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"
)

// platformAnswer is what the fake answers for an object, read past the
// provider.
type platformAnswer struct {
	UID          string `json:"uid"`
	ControlPlane struct {
		Endpoint struct {
			URL string `json:"url"`
		} `json:"endpoint"`
	} `json:"controlPlane"`
}

func readFakeObject(path string) (platformAnswer, error) {
	var got platformAnswer
	status, body, err := readFake(path)
	if err != nil || status != http.StatusOK {
		return got, fmt.Errorf("reading %s: %d %v", path, status, err)
	}
	if err := json.Unmarshal(body, &got); err != nil {
		return got, fmt.Errorf("reading %s: %w", path, err)
	}
	return got, nil
}

// fromFake checks that attr on addr is what the fake answers for the object
// at pathOf, past the provider. want picks the value, and an empty one fails:
// a check that compares two empty strings proves nothing.
func fromFake(addr, attr string, pathOf func(map[string]string) string, want func(platformAnswer) string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs, ok := s.RootModule().Resources[addr]
		if !ok {
			return fmt.Errorf("%s is not in state", addr)
		}
		got, err := readFakeObject(pathOf(rs.Primary.Attributes))
		if err != nil {
			return err
		}
		if want(got) == "" {
			return fmt.Errorf("the fake answers no %s for %s", attr, addr)
		}
		return resource.TestCheckResourceAttr(addr, attr, want(got))(s)
	}
}

func fakeUIDOf(o platformAnswer) string      { return o.UID }
func fakeEndpointOf(o platformAnswer) string { return o.ControlPlane.Endpoint.URL }

// importFromFake is fromFake for an import, which checks the imported row.
func importFromFake(attr string, pathOf func(map[string]string) string, want func(platformAnswer) string) resource.ImportStateCheckFunc {
	return func(states []*terraform.InstanceState) error {
		if len(states) != 1 {
			return fmt.Errorf("the import made %d rows, want 1", len(states))
		}
		got, err := readFakeObject(pathOf(states[0].Attributes))
		if err != nil {
			return err
		}
		if want(got) == "" || states[0].Attributes[attr] != want(got) {
			return fmt.Errorf("the import holds %s %q, and the fake answers %q", attr, states[0].Attributes[attr], want(got))
		}
		return nil
	}
}

// A create, an import and the data sources all record the uid the platform
// gave the cluster and the instance group, and the cluster's API server URL,
// and nothing plans after either.
func TestAccUIDAndAPIEndpointComeFromThePlatform(t *testing.T) {
	t.Parallel()
	const cluster, group = "acc-uid-cluster", "acc-uid-hosts"
	// Every pool named, so the import reads back what the create wrote.
	config := fmt.Sprintf(`
resource %q "test" {
  id           = %q
  cluster_type = %q
  display_name = "acceptance"
  node_pool = {
    "control-plane" = { node_count = 1 }
    "gpu-workers"   = { node_count = 2 }
  }
}
`, clusterType, cluster, clusterTypeRef) + instanceGroupConfig(group, 1) + fmt.Sprintf(`
data %q "read" {
  id = %s.id
}

data %q "read" {
  id = %s.id
}
`, clusterType, clusterAddr, instanceGroupType, instanceGroupAddr)
	dataPath := func(pathOf func(map[string]string) string) func(map[string]string) string {
		return func(a map[string]string) string {
			return pathOf(map[string]string{"region": "us-texas-1", "project": "demo", "id": a["id"]})
		}
	}
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			checkGone(clusterType, clusterPath),
			checkGone(instanceGroupType, instanceGroupPath),
		),
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					fromFake(clusterAddr, "uid", clusterPath, fakeUIDOf),
					fromFake(clusterAddr, "api_endpoint", clusterPath, fakeEndpointOf),
					fromFake(instanceGroupAddr, "uid", instanceGroupPath, fakeUIDOf),
					fromFake(clusterDataAddr, "uid", dataPath(clusterPath), fakeUIDOf),
					fromFake(clusterDataAddr, "api_endpoint", dataPath(clusterPath), fakeEndpointOf),
					fromFake(instanceGroupDataAddr, "uid", dataPath(instanceGroupPath), fakeUIDOf),
				),
			},
			{Config: config, PlanOnly: true},
			{
				Config:            config,
				ResourceName:      clusterAddr,
				ImportState:       true,
				ImportStateId:     "us-texas-1/demo/" + cluster,
				ImportStateVerify: true,
				ImportStateCheck: func(states []*terraform.InstanceState) error {
					if err := importFromFake("uid", clusterPath, fakeUIDOf)(states); err != nil {
						return err
					}
					return importFromFake("api_endpoint", clusterPath, fakeEndpointOf)(states)
				},
			},
			{
				Config:            config,
				ResourceName:      instanceGroupAddr,
				ImportState:       true,
				ImportStateId:     "us-texas-1/demo/" + group,
				ImportStateVerify: true,
				ImportStateCheck:  importFromFake("uid", instanceGroupPath, fakeUIDOf),
			},
			// An import block plans a no-op, the uid and the endpoint
			// included.
			{
				Config:          config,
				ResourceName:    clusterAddr,
				ImportState:     true,
				ImportStateKind: resource.ImportBlockWithID,
				ImportStateId:   "us-texas-1/demo/" + cluster,
			},
			{
				Config:          config,
				ResourceName:    instanceGroupAddr,
				ImportState:     true,
				ImportStateKind: resource.ImportBlockWithID,
				ImportStateId:   "us-texas-1/demo/" + group,
			},
		},
	})
}

// moveEndpoint has the fake publish or move a cluster's API server URL, as
// the platform's observation of its control plane does.
func moveEndpoint(t *testing.T, cluster, action string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost,
		endpoint+"/_harness/regions/us-texas-1/projects/demo/clusters/"+cluster+"/endpoint/"+action, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+fakeOwnerToken)
	resp, err := harnessHTTP.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("%s on %s's endpoint answered %s", action, cluster, resp.Status)
	}
}

// The platform can report a hosted control plane's endpoint only after the
// cluster is `active`, and moves it when the control plane's address moves.
// Either way a refresh takes the new value into state and the plan stays
// empty. An update plans the endpoint state holds, or leaves it to the apply
// while state has none, so an endpoint published during the update lands
// without an inconsistent result.
func TestAccAPIEndpointArrivesAfterTheCreate(t *testing.T) {
	t.Parallel()
	const name = "acc-endpoint-late"
	config := func(label string) string {
		return fmt.Sprintf(`
resource %q "test" {
  id           = %q
  cluster_type = %q
  display_name = "%s fake-endpoint-late"
  node_pool = {
    "gpu-workers" = { node_count = 1 }
  }
}
`, clusterType, name, clusterTypeRef, label)
	}
	url := func(port int) string {
		return fmt.Sprintf("https://%s.us-texas-1.k8s.example.invalid:%d", name, port)
	}
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy:             checkGone(clusterType, clusterPath),
		Steps: []resource.TestStep{
			{
				Config: config("first"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(clusterAddr, "state", "active"),
					resource.TestCheckNoResourceAttr(clusterAddr, "api_endpoint"),
					fromFake(clusterAddr, "uid", clusterPath, fakeUIDOf),
				),
			},
			// With none in state, an update leaves it to the apply.
			{
				Config: config("second"),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(clusterAddr, plancheck.ResourceActionUpdate),
					unknownAfterApply(clusterAddr, "api_endpoint"),
					plancheck.ExpectKnownValue(clusterAddr, tfjsonpath.New("uid"), knownvalue.NotNull()),
				}},
				Check: resource.TestCheckNoResourceAttr(clusterAddr, "api_endpoint"),
			},
			{
				PreConfig: func() { moveEndpoint(t, name, "publish") },
				Config:    config("second"),
				PlanOnly:  true,
			},
			{
				RefreshState: true,
				Check:        resource.TestCheckResourceAttr(clusterAddr, "api_endpoint", url(6443)),
			},
			{
				PreConfig: func() { moveEndpoint(t, name, "move") },
				Config:    config("second"),
				PlanOnly:  true,
			},
			{
				RefreshState: true,
				Check:        resource.TestCheckResourceAttr(clusterAddr, "api_endpoint", url(6444)),
			},
			// With one in state, an update plans it as it is.
			{
				Config: config("third"),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(clusterAddr, plancheck.ResourceActionUpdate),
					plancheck.ExpectKnownValue(clusterAddr, tfjsonpath.New("api_endpoint"), knownvalue.StringExact(url(6444))),
				}},
				Check: resource.TestCheckResourceAttr(clusterAddr, "api_endpoint", url(6444)),
			},
		},
	})
}

// A state the last release wrote holds no uid and no endpoint. An update
// planned without a refresh leaves both to the apply, which reads them back,
// rather than planning the nulls and failing on what the read returns.
func TestAccAnUpdateWithoutARefreshFillsTheUID(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		skipTest(t, "the CLI sequence runs on unix")
	}
	for _, kind := range []struct {
		name, addr, id string
		config         func(name string, n int) string
		path           func(map[string]string) string
		added          []string
	}{
		{"cluster", clusterAddr, "acc-uid-norefresh-cluster", clusterConfig, clusterPath, []string{"uid", "api_endpoint"}},
		{"instance group", instanceGroupAddr, "acc-uid-norefresh-hosts", instanceGroupConfig, instanceGroupPath, []string{"uid"}},
	} {
		t.Run(kind.name, func(t *testing.T) {
			t.Parallel()
			c := newCLIRun(t)
			c.write(kind.config(kind.id, 1))
			t.Cleanup(func() {
				if out, ok := c.run("destroy", "-auto-approve"); !ok {
					t.Errorf("destroy failed:\n%s", out)
				}
			})
			if out, ok := c.run("apply", "-auto-approve"); !ok {
				t.Fatalf("the create failed:\n%s", out)
			}

			// The state as the last release left it: the same row without
			// the attributes this build added.
			pulled, err := c.command("state", "pull").Output()
			if err != nil {
				t.Fatalf("terraform state pull: %v", err)
			}
			var st map[string]any
			if err = json.Unmarshal(pulled, &st); err != nil {
				t.Fatal(err)
			}
			attrs, serial, ok := onlyRow(st)
			if !ok {
				t.Fatalf("the state isn't one row:\n%s", pulled)
			}
			for _, a := range kind.added {
				if attrs[a] == nil {
					t.Fatalf("the create left no %s to take out: %v", a, attrs)
				}
				delete(attrs, a)
			}
			st["serial"] = serial + 1
			old, err := json.Marshal(st)
			if err != nil {
				t.Fatal(err)
			}
			writeFile(t, filepath.Join(c.dir, "old.tfstate"), string(old))
			if out, ok := c.run("state", "push", "old.tfstate"); !ok {
				t.Fatalf("terraform state push:\n%s", out)
			}

			c.write(strings.Replace(kind.config(kind.id, 1), `display_name = "acceptance"`, `display_name = "renamed"`, 1))
			if out, ok := c.run("apply", "-auto-approve", "-refresh=false"); !ok {
				t.Fatalf("the update without a refresh failed:\n%s", out)
			}
			shown, err := c.command("show", "-json").Output()
			if err != nil {
				t.Fatalf("terraform show: %v", err)
			}
			var after struct {
				Values struct {
					RootModule struct {
						Resources []struct {
							Values map[string]any `json:"values"`
						} `json:"resources"`
					} `json:"root_module"`
				} `json:"values"`
			}
			if err = json.Unmarshal(shown, &after); err != nil || len(after.Values.RootModule.Resources) != 1 {
				t.Fatalf("reading the state back: %v\n%s", err, shown)
			}
			values := after.Values.RootModule.Resources[0].Values
			got, err := readFakeObject(kind.path(map[string]string{"region": "us-texas-1", "project": "demo", "id": kind.id}))
			if err != nil {
				t.Fatal(err)
			}
			want := map[string]string{"uid": got.UID, "api_endpoint": got.ControlPlane.Endpoint.URL}
			for _, a := range kind.added {
				if want[a] == "" || values[a] != want[a] {
					t.Errorf("after the update %s is %v, and the fake answers %q", a, values[a], want[a])
				}
			}
		})
	}
}

// onlyRow is the attributes of a state's one resource instance, and the
// state's serial.
func onlyRow(st map[string]any) (map[string]any, float64, bool) {
	serial, ok := st["serial"].(float64)
	resources, ok2 := st["resources"].([]any)
	if !ok || !ok2 || len(resources) != 1 {
		return nil, 0, false
	}
	res, ok := resources[0].(map[string]any)
	if !ok {
		return nil, 0, false
	}
	instances, ok := res["instances"].([]any)
	if !ok || len(instances) != 1 {
		return nil, 0, false
	}
	inst, ok := instances[0].(map[string]any)
	if !ok {
		return nil, 0, false
	}
	attrs, ok := inst["attributes"].(map[string]any)
	return attrs, serial, ok
}
