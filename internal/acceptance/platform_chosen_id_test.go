package acceptance

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/provider"
)

// idIgnoringPlatform sits in front of the fake and plays a platform that
// chooses every id itself and ignores one a create sends. It takes `id` out of
// each create sent to one collection, so the fake makes the resource under an
// id of its own and answers with it. Every other request passes through as
// sent. With timesOut, it answers such a create 504 naming the new id in its
// error metadata, the way the service does when it saved the resource and
// timed out starting its workflow.
type idIgnoringPlatform struct {
	// collection is the end of the create's path, such as
	// "/networking/networks".
	collection string
	// metadataKey names the id in a 504's metadata, such as "clusterId".
	metadataKey string
	timesOut    bool

	mu sync.Mutex
	// made is the API path of every resource a create made through it.
	made []string
}

// serve starts the proxy and returns the endpoint a provider takes.
func (p *idIgnoringPlatform) serve(t *testing.T) string {
	t.Helper()
	target, err := url.Parse(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	proxy := &httputil.ReverseProxy{
		// A pool of its own, so closing this server can't close a
		// connection another test is using.
		Transport: provider.NewPlatformTransport(),
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(target)
			if p.creates(r.In) {
				if err := withoutID(r.Out); err != nil {
					t.Errorf("taking the id out of a create: %v", err)
				}
			}
		},
		ModifyResponse: func(resp *http.Response) error {
			if !p.creates(resp.Request) || resp.StatusCode != http.StatusCreated {
				return nil
			}
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				return err
			}
			var made struct {
				ID string `json:"id"`
			}
			if err = json.Unmarshal(body, &made); err != nil || made.ID == "" {
				return fmt.Errorf("the fake's 201 named no id: %s", body)
			}
			p.mu.Lock()
			p.made = append(p.made, resp.Request.URL.Path+"/"+made.ID)
			p.mu.Unlock()
			if p.timesOut {
				body, err = json.Marshal(map[string]any{"error": map[string]any{
					"code": http.StatusGatewayTimeout, "message": "deadline exceeded", "reason": "DEADLINE_EXCEEDED",
					"requestId": "acc-chosen-id",
					"details": []any{map[string]any{"reason": "DEADLINE_EXCEEDED",
						"metadata": map[string]any{p.metadataKey: made.ID}}},
				}})
				if err != nil {
					return err
				}
				resp.StatusCode, resp.Status = http.StatusGatewayTimeout, "504 Gateway Timeout"
			}
			resp.Body = io.NopCloser(bytes.NewReader(body))
			resp.ContentLength = int64(len(body))
			resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
			return nil
		},
	}
	server := httptest.NewServer(proxy)
	t.Cleanup(server.Close)
	return server.URL
}

func (p *idIgnoringPlatform) creates(r *http.Request) bool {
	return r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, p.collection)
}

// withoutID takes `id` out of a JSON request body.
func withoutID(r *http.Request) error {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return err
	}
	var body map[string]json.RawMessage
	if err = json.Unmarshal(raw, &body); err != nil {
		return err
	}
	delete(body, "id")
	out, err := json.Marshal(body)
	if err != nil {
		return err
	}
	r.Body = io.NopCloser(bytes.NewReader(out))
	r.ContentLength = int64(len(out))
	r.Header.Set("Content-Length", strconv.Itoa(len(out)))
	return nil
}

// madeIDs is the id of every resource made through the proxy.
func (p *idIgnoringPlatform) madeIDs() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	ids := make([]string, 0, len(p.made))
	for _, path := range p.made {
		ids = append(ids, path[strings.LastIndex(path, "/")+1:])
	}
	return ids
}

// leftBehind is every resource made through the proxy that the fake's store
// still holds.
func (p *idIgnoringPlatform) leftBehind(t *testing.T) []string {
	t.Helper()
	p.mu.Lock()
	made := append([]string(nil), p.made...)
	p.mu.Unlock()
	var left []string
	for _, path := range made {
		status, body, err := readFakeStore(path)
		switch {
		case err != nil:
			t.Errorf("reading %s from the store: %v", path, err)
		case status != http.StatusNotFound:
			left = append(left, fmt.Sprintf("%s (%d %s)", path, status, bytes.TrimSpace(body)))
		}
	}
	return left
}

// chosenIDKind is one kind whose create sends an id, and a configuration
// that makes one.
type chosenIDKind struct {
	typeName, noun string
	collection     string
	// metadataKey names the id a 504 names, empty for a kind whose 504 names
	// none.
	metadataKey string
	// idRequired is a kind whose configuration must name its id.
	idRequired bool
	// config is the configuration, with idLine in the resource under test,
	// named "test", and name for anything it needs beside it.
	config func(name, idLine string) string
}

func chosenIDKinds() []chosenIDKind {
	return []chosenIDKind{
		{
			typeName: clusterType, noun: "cluster", collection: "/compute/clusters",
			metadataKey: "clusterId", idRequired: true,
			config: func(_, idLine string) string {
				return fmt.Sprintf(`
resource %q "test" {
  %s
  cluster_type = %q
  display_name = "acceptance"
  node_pool = {
    "gpu-workers" = { node_count = 1 }
  }
}
`, clusterType, idLine, clusterTypeRef)
			},
		},
		{
			typeName: instanceGroupType, noun: "instance group", collection: "/compute/instance-groups",
			metadataKey: "instanceGroupId", idRequired: true,
			config: func(_, idLine string) string {
				return fmt.Sprintf(`
resource %q "test" {
  %s
  cluster_type = %q
  display_name = "acceptance"
  node_pool = {
    "gpu-workers" = { size = 1 }
  }
}
`, instanceGroupType, idLine, clusterTypeRef)
			},
		},
		{
			typeName: networkType, noun: "network", collection: "/networking/networks",
			config: func(_, idLine string) string {
				return fmt.Sprintf(`
resource %q "test" {
  %s
  display_name = "acceptance"
  evpn = {
    service_type = "l3vpn"
    vlan_id      = "auto"
    mtu          = 9000
  }
}
`, networkType, idLine)
			},
		},
		{
			typeName: subnetType, noun: "subnet", collection: "/subnets",
			config: func(name, idLine string) string {
				return evpnConfig(name+"-net", "acceptance") + fmt.Sprintf(`
resource %q "test" {
  %s
  network       = %s.test.id
  address_pool  = "/v1/regions/us-texas-1/networking/address-pools/tenant"
  prefix_length = 24
}
`, subnetType, idLine, networkType)
			},
		},
		{
			typeName: securityGroupType, noun: "security group", collection: "/networking/security-groups",
			metadataKey: "securityGroupId",
			config: func(_, idLine string) string {
				return fmt.Sprintf(`
resource %q "test" {
  %s
  display_name = "acceptance"

  ingress {
    name               = "allow-ssh-from-office"
    action             = "permit"
    protocol           = "tcp"
    source_prefix      = "203.0.113.0/24"
    destination_prefix = "0.0.0.0/0"

    destination_port_range = "22"
  }
}
`, securityGroupType, idLine)
			},
		},
		{
			typeName: fileStoreType, noun: "file store", collection: "/storage/file-stores",
			metadataKey: "fileStoreId",
			config: func(_, idLine string) string {
				return fmt.Sprintf(`
resource %q "test" {
  %s
  display_name = "acceptance"
  share_type   = "general"
  capacity_gib = 1024
}
`, fileStoreType, idLine)
			},
		},
		{
			typeName: peeringType, noun: "VPC peering", collection: "/peerings",
			config: func(name, idLine string) string {
				return clusterConfig(name+"-owner", 1) + fmt.Sprintf(`
resource %q "test" {
  %s
  vpc          = "vpc-%s-owner"
  remote_vpc   = "/v1/regions/us-texas-1/projects/other/networking/vpcs/vpc-inference"
  display_name = "acceptance"
  depends_on   = [%s.test]
}
`, peeringType, idLine, name, clusterType)
			},
		},
		{
			// Its create converges the same way, so it's held to the same rule.
			typeName: projectType, noun: "project", collection: "/organizations/" + accOrg + "/projects",
			config: func(_, idLine string) string {
				return fmt.Sprintf(`
resource %q "test" {
  %s
  organization_id = %q
  region          = "us-texas-1"
  display_name    = "acceptance"
}
`, projectType, idLine, accOrg)
			},
		},
	}
}

// A platform that chooses ids itself may ignore the one a create sends and
// answer with its own. Every kind whose create sends an id is run through the
// real CLI against such a platform, and nothing it makes may be left on it:
//
//   - With the id generated, the apply passes with the platform's id in state,
//     and the next plan is empty.
//   - With the id configured, the apply fails with the platform's id in state,
//     tainted, and says how to keep it. Following that, an untaint and an
//     apply, leaves a plan with no changes.
//   - `terraform destroy` then leaves nothing the creates made.
//
// Each runs with the create answered 201, and, for a kind whose 504 names the
// id it saved, answered 504. A cluster's and an instance group's id is
// required, so they have no generated case.
func TestAccACreateUnderAnIDThePlatformChoseIsKept(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		skipTest(t, "the CLI sequence runs on unix")
	}
	if os.Getenv("TF_ACC") == "" {
		skipTest(t, "TF_ACC is not set, so there is no fake to run against")
	}
	built := builtProvider(t)
	for _, k := range chosenIDKinds() {
		for _, answer := range []string{"201", "504"} {
			if answer == "504" && k.metadataKey == "" {
				continue
			}
			for _, configured := range []bool{false, true} {
				if !configured && k.idRequired {
					continue
				}
				name := "acc-chosen-" + strings.ReplaceAll(strings.ToLower(k.noun), " ", "-") + "-" + answer
				how := "generated"
				if configured {
					name += "-configured"
					how = "configured"
				}
				t.Run(k.typeName+"/"+how+"/"+answer, func(t *testing.T) {
					t.Parallel()
					runChosenID(t, built, k, name, answer == "504", configured)
				})
			}
		}
	}
}

func runChosenID(t *testing.T, built string, k chosenIDKind, name string, timesOut, configured bool) {
	platform := &idIgnoringPlatform{collection: k.collection, metadataKey: k.metadataKey, timesOut: timesOut}
	c := newCLIRunWith(t, built)
	c.env = []string{provider.EnvVar("ENDPOINT") + "=" + platform.serve(t)}
	addr := k.typeName + ".test"

	// Whatever happens above, the destroy runs, and every resource the
	// creates made must be gone from the platform after it.
	t.Cleanup(func() {
		if out, ok := c.run("destroy", "-auto-approve"); !ok {
			t.Errorf("the destroy failed:\n%s", out)
		}
		if left := platform.leftBehind(t); len(left) > 0 {
			t.Errorf("the platform still holds what the creates made, with nothing in state to destroy it:\n  %s",
				strings.Join(left, "\n  "))
		}
	})

	idLine := ""
	if configured {
		idLine = fmt.Sprintf("id = %q", name)
	}
	c.write(k.config(name, idLine))
	out, applied := c.run("apply", "-auto-approve")
	made := platform.madeIDs()
	if len(made) != 1 {
		t.Fatalf("the creates made %v, want one resource:\n%s", made, out)
	}
	chosen := made[0]
	if chosen == name {
		t.Fatalf("the platform kept the id sent, %q, so this proves nothing", name)
	}

	if !configured {
		if !applied {
			t.Fatalf("the apply failed on an id the platform chose for a generated one:\n%s", out)
		}
		if timesOut {
			mustSay(t, "the apply", out, fmt.Sprintf("The %s was created, and its workflow was slow to start", k.noun))
		}
		if id, tainted := stateOf(t, c, addr); id != chosen || tainted {
			t.Errorf("state holds %q, tainted %t, want the platform's %q, not tainted", id, tainted, chosen)
		}
		if plan, ok := c.run("plan", "-detailed-exitcode"); !ok {
			t.Errorf("the plan after the apply isn't empty:\n%s", plan)
		}
		return
	}

	if applied {
		t.Fatalf("the apply passed though the configuration names an id the platform didn't use:\n%s", out)
	}
	keep := fmt.Sprintf("remove `id` from this resource's configuration, or set it to %q", chosen)
	if k.idRequired {
		keep = fmt.Sprintf("set `id` in this resource's configuration to %q", chosen)
	}
	mustSay(t, "the apply", out,
		fmt.Sprintf("The platform chose another id for the %s", k.noun),
		fmt.Sprintf("The create sent %s id %q", k.noun, name),
		fmt.Sprintf("State records %q", chosen),
		"To keep it, "+keep+".",
		"terraform untaint <address> terraform apply")
	if id, tainted := stateOf(t, c, addr); id != chosen || !tainted {
		t.Fatalf("state holds %q, tainted %t, want the platform's %q, tainted", id, tainted, chosen)
	}

	// The advice, as written: a kind that can leave `id` out drops it on a
	// 201 and sets it to the platform's on a 504, so both are run.
	if k.idRequired || timesOut {
		idLine = fmt.Sprintf("id = %q", chosen)
	} else {
		idLine = ""
	}
	c.write(k.config(name, idLine))
	// The advice says not to apply before the untaint, since the plan
	// replaces it.
	out, _ = c.run("plan")
	mustSay(t, "the plan before the untaint", out, taintedReplaced(t))
	if out, ok := c.run("untaint", addr); !ok {
		t.Fatalf("the untaint failed:\n%s", out)
	}
	if out, ok := c.run("apply", "-auto-approve"); !ok {
		t.Fatalf("the apply after the advice failed:\n%s", out)
	}
	if id, tainted := stateOf(t, c, addr); id != chosen || tainted {
		t.Errorf("state holds %q, tainted %t, want the platform's %q, not tainted", id, tainted, chosen)
	}
	if out, ok := c.run("plan", "-detailed-exitcode"); !ok {
		t.Errorf("the plan after the advice isn't empty:\n%s", out)
	}
	if made := platform.madeIDs(); len(made) != 1 {
		t.Errorf("the creates made %v, want the one resource", made)
	}
}

// stateOf is the id state holds for addr, and whether it's tainted. Both are
// zero when state holds nothing there.
func stateOf(t *testing.T, c *cliRun, addr string) (string, bool) {
	t.Helper()
	out, ok := c.run("show", "-json")
	if !ok {
		t.Fatalf("terraform show failed:\n%s", out)
	}
	var shown struct {
		Values struct {
			RootModule struct {
				Resources []struct {
					Address string         `json:"address"`
					Tainted bool           `json:"tainted"`
					Values  map[string]any `json:"values"`
				} `json:"resources"`
			} `json:"root_module"`
		} `json:"values"`
	}
	if err := json.Unmarshal([]byte(out), &shown); err != nil {
		t.Fatalf("reading terraform show: %v\n%s", err, out)
	}
	for _, r := range shown.Values.RootModule.Resources {
		if r.Address == addr {
			id, ok := r.Values["id"].(string)
			if !ok {
				t.Fatalf("state holds no id for %s: %v", addr, r.Values["id"])
			}
			return id, r.Tainted
		}
	}
	return "", false
}
