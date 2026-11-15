package acceptance

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// A network, subnet or security group the platform made for a cluster,
// imported into a configuration, through the real CLI:
//
//  1. an update that sends nothing applies with no write to it
//  2. the provider's default_tags aren't applied to it, and the plan warns
//  3. a change to each attribute its update sends, or a replacement, fails
//     the plan and names the attribute
//  4. with a rename configured, `terraform destroy` fails too, while
//     `terraform destroy -refresh=false` removes it from state with no write
func TestAccAPlatformOwnedObject(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		skipTest(t, "the CLI sequence runs on unix")
	}
	const sgRules = `
ingress {
  name = "intra-vpc-0"
  action = "permit"
  protocol = "any"
  source_prefix = "10.90.0.0/24"
  destination_prefix = "10.90.0.0/24"
}
egress {
  name = "intra-vpc-egress-0"
  action = "permit"
  protocol = "any"
  source_prefix = "0.0.0.0/0"
  destination_prefix = "10.90.0.0/24"
}`
	rule := func(kind string) string {
		return kind + ` {
  name = "ssh"
  action = "permit"
  protocol = "tcp"
  source_prefix = "0.0.0.0/0"
  destination_prefix = "10.90.0.0/24"
  destination_port_range = "22"
}`
	}
	type edit struct{ name, value string }
	attrs := []edit{{"description", `"edited"`}, {"tags", `{ a = "b" }`}}
	for _, tc := range []struct {
		kind, cluster, resourceType string
		// id is "" for the default group, whose id is random. under is the
		// path to it, and importUnder the import id, both without the id.
		id, under, importUnder string
		// attrs and blocks are as the platform holds the object, and noSend
		// an update that sends nothing.
		attrs          map[string]string
		blocks, noSend string
		// edits each change one attribute the update sends. The loop adds a
		// `display_name` rename after them. One whose value starts with its
		// name is a block to add.
		edits []edit
	}{
		{"network", "acc-owned-obj-net", networkType, "vpc-acc-owned-obj-net-net", "networks/", "us-texas-1/demo/",
			map[string]string{"display_name": `"cluster network"`, "evpn": `{ service_type = "l2vpn", vlan_id = "4002" }`},
			"", `timeouts { update = "5m" }`, attrs},
		// Its first apply after the import records `allocate_gateway`.
		{"subnet", "acc-owned-obj-sub", subnetType, "vpc-acc-owned-obj-sub-net-sub",
			"networks/vpc-acc-owned-obj-sub-net/subnets/", "us-texas-1/demo/vpc-acc-owned-obj-sub-net/",
			map[string]string{"display_name": `"cluster subnet"`, "network": `"vpc-acc-owned-obj-sub-net"`,
				"address_pool": `"pool-routable"`, "prefix_length": "24"}, "", "", nil},
		{"security group", "acc-owned-obj-sg", securityGroupType, "", "security-groups/", "us-texas-1/demo/",
			map[string]string{"display_name": `"Default (vpc-acc-owned-obj-sg)"`}, sgRules, `timeouts { update = "5m" }`,
			append(attrs, edit{"ingress", rule("ingress")}, edit{"egress", rule("egress")})},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			t.Parallel()
			c := newCLIRun(t)
			c.write(clusterConfig(tc.cluster, 1))
			if out, ok := c.run("apply", "-auto-approve"); !ok {
				t.Fatalf("the cluster's create failed:\n%s", out)
			}
			t.Cleanup(func() { c.run("destroy", "-auto-approve", "-refresh=false") })
			id, held := tc.id, maps.Clone(tc.attrs)
			if id == "" {
				id = defaultGroupID(t, tc.cluster)
				held["description"] = fmt.Sprintf("%q", storedDescription(t, id))
			}
			held["id"] = fmt.Sprintf("%q", id)
			path := "/v1/regions/us-texas-1/projects/demo/networking/" + tc.under + id
			config := func(e edit, extra string) string {
				set, blocks := maps.Clone(held), tc.blocks
				if strings.HasPrefix(e.value, e.name) {
					blocks += "\n" + e.value
				} else if e.name != "" {
					set[e.name] = e.value
				}
				var b strings.Builder
				for _, k := range slices.Sorted(maps.Keys(set)) {
					fmt.Fprintf(&b, "%s = %s\n", k, set[k])
				}
				return clusterConfig(tc.cluster, 1) + fmt.Sprintf("resource %q \"adopted\" {\n%s%s\n%s\n}\n",
					tc.resourceType, b.String(), blocks, extra)
			}
			c.write(config(edit{}, ""))
			if out, ok := c.run("import", tc.resourceType+".adopted", tc.importUnder+id); !ok {
				t.Fatalf("the import failed:\n%s", out)
			}
			writes := countWrites(t, path)

			c.write(config(edit{}, tc.noSend))
			if out, ok := c.run("apply", "-auto-approve"); !ok || strings.Contains(out, "owned by the platform") ||
				strings.Contains(out, "the platform owns") {
				t.Errorf("an update that sends nothing was refused or warned:\n%s", out)
			}
			if out, code := c.runCode("plan", "-detailed-exitcode"); code != 0 {
				t.Errorf("the plan after it exited %d, want 0:\n%s", code, out)
			}

			if tc.kind != "subnet" {
				c.write(config(edit{}, tc.noSend) + withDefaultTags(`{ team = "ml" }`))
				out, ok := c.run("apply", "-auto-approve")
				if !ok || strings.Contains(out, "adopted will be updated") {
					t.Errorf("default_tags failed or updated the %s:\n%s", tc.kind, out)
				}
				mustSay(t, "the default_tags apply", out,
					"Warning: The provider's default_tags aren't applied to a "+tc.kind+" the platform owns")
			}
			if n := countWrites(t, path); n != writes {
				t.Errorf("the applies sent %d writes to the %s, want none", n-writes, tc.kind)
			}

			for _, e := range append(tc.edits, edit{"display_name", `"renamed by the tenant"`}) {
				c.write(config(e, tc.noSend))
				out, code := c.runCode("plan")
				if code != 1 || strings.Contains(out, "default_tags aren't applied") {
					t.Errorf("the plan changing %s exited %d, want 1 with no default_tags warning:\n%s", e.name, code, out)
				}
				mustSay(t, "the plan changing "+e.name, out,
					"Error: This object is owned by the platform, not by the tenant",
					"This plan would change `"+e.name+"`", "undo that change")
			}
			if tc.edits != nil {
				c.write(config(edit{"description", "\"edited\"\ntags = { a = \"b\" }"}, tc.noSend))
				out, _ := c.runCode("plan")
				mustSay(t, "the plan changing two", out, "This plan would change `description` and `tags`",
					"undo those changes")
			}
			out, code := c.runCode("destroy", "-auto-approve")
			if code != 1 {
				t.Errorf("terraform destroy exited %d, want 1:\n%s", code, out)
			}
			mustSay(t, "terraform destroy", out, "Error: This object is owned by the platform, not by the tenant",
				"run `terraform destroy -refresh=false` instead")
			out, code = c.runCode("destroy", "-auto-approve", "-refresh=false")
			if code != 0 {
				t.Fatalf("the destroy exited %d, want 0:\n%s", code, out)
			}
			mustSay(t, "the destroy", out, "Stopped tracking a platform-owned object rather than deleting it",
				"Destroy complete! Resources: 2 destroyed.")
			if n := countWrites(t, path); n != writes {
				t.Errorf("the destroy sent %d writes to the %s, want none", n-writes, tc.kind)
			}
		})
	}
}

// countWrites is how many writes the fake has served on path.
func countWrites(t *testing.T, path string) int {
	t.Helper()
	total := 0
	for _, m := range []string{http.MethodPatch, http.MethodPut, http.MethodPost, http.MethodDelete} {
		n, err := fakeServed(m, path)
		if err != nil {
			t.Fatal(err)
		}
		total += n
	}
	return total
}

// readStored reads one object from the fake's store into into.
func readStored(t *testing.T, path string, into any) {
	t.Helper()
	status, body, err := readFakeStore(path)
	if err != nil || status != http.StatusOK || json.Unmarshal(body, into) != nil {
		t.Fatalf("reading %s: %d %v %s", path, status, err, body)
	}
}

// defaultGroupID reads the id of the cluster's VPC's default security group,
// which the platform generates.
func defaultGroupID(t *testing.T, cluster string) string {
	t.Helper()
	var v struct {
		SecurityGroups []string `json:"securityGroups"`
	}
	readStored(t, "/v1/regions/us-texas-1/projects/demo/networking/vpcs/vpc-"+cluster, &v)
	if len(v.SecurityGroups) != 1 {
		t.Fatalf("the VPC has groups %v, want one", v.SecurityGroups)
	}
	return v.SecurityGroups[0]
}

// storedDescription is the description the platform gave a security group.
func storedDescription(t *testing.T, id string) string {
	t.Helper()
	var g struct {
		Description string `json:"description"`
	}
	readStored(t, "/v1/regions/us-texas-1/projects/demo/networking/security-groups/"+id, &g)
	return g.Description
}
