package acceptance

import (
	"fmt"
	"net/http"
	"runtime"
	"strings"
	"testing"
)

// A move of `region` to one the resource's project isn't in, or for a
// project to one that can't hold a project, can't be made: the replacement's
// create would fail after the delete. So the plan warns and plans an update,
// the apply refuses it before it sends any request, allow_replace or not,
// and `terraform destroy` deletes as usual. Each kind runs through the real
// CLI in a project of its own, counting what the fake is sent.

// moveKind is one resource with a region.
type moveKind struct {
	short, noun, addr string
	guarded           bool
	// config is the whole configuration, with extra inside the resource's
	// block. p is the project of its own.
	config func(p, extra string) string
	// writes are the requests that would change the resource: a delete,
	// a create, an update.
	writes func(p string) []fakeRequest
	// deleted is the request a destroy sends.
	deleted func(p string) fakeRequest
}

type fakeRequest struct{ method, path string }

func objectWrites(object, collection string) []fakeRequest {
	return []fakeRequest{{http.MethodDelete, object}, {http.MethodPatch, object}, {http.MethodPut, object},
		{http.MethodPost, object}, {http.MethodPost, collection}}
}

func inScope(p, rest string) string { return "/v1/regions/us-texas-1/projects/" + p + rest }

// moveOwner is the cluster whose VPC a peering or a binding needs.
func moveOwner(p string) string {
	return fmt.Sprintf(`
resource %q "owner" {
  id           = "%s-owner"
  cluster_type = %q
  node_pool    = { "gpu-workers" = { node_count = 1 } }
}
`, clusterType, p, clusterTypeRef)
}

const moveEgress = `egress {
    action             = "permit"
    protocol           = "any"
    source_prefix      = "0.0.0.0/0"
    destination_prefix = "0.0.0.0/0"
  }`

func moveKinds() []moveKind {
	simple := func(short, noun, typ, body, rest string) moveKind {
		return moveKind{short: short, noun: noun, addr: typ + ".test", guarded: true,
			config: func(p, extra string) string {
				return fmt.Sprintf("\nresource %q \"test\" {\n  id = \"%s-x\"\n%s\n%s\n}\n", typ, p, body, extra)
			},
			writes:  func(p string) []fakeRequest { return objectWrites(inScope(p, rest+"/"+p+"-x"), inScope(p, rest)) },
			deleted: func(p string) fakeRequest { return fakeRequest{http.MethodDelete, inScope(p, rest+"/"+p+"-x")} },
		}
	}
	kinds := []moveKind{
		simple("cl", "cluster", clusterType, fmt.Sprintf("  cluster_type = %q\n  node_pool = { \"gpu-workers\" = { node_count = 1 } }", clusterTypeRef),
			"/compute/clusters"),
		simple("ig", "instance group", instanceGroupType, fmt.Sprintf("  cluster_type = %q\n  node_pool = { \"gpu-workers\" = { size = 1 } }", clusterTypeRef),
			"/compute/instance-groups"),
		simple("fs", "file store", fileStoreType, "  share_type = \"general\"\n  capacity_gib = 100", "/storage/file-stores"),
		simple("net", "network", networkType, "  evpn = { service_type = \"l3vpn\", vlan_id = \"auto\" }", "/networking/networks"),
		simple("sg", "security group", securityGroupType, "  "+moveEgress, "/networking/security-groups"),
	}
	kinds[4].guarded = false
	subnets := func(p string) string { return inScope(p, "/networking/networks/"+p+"-net/subnets") }
	vpc := func(p string) string { return inScope(p, "/networking/vpcs/vpc-"+p+"-owner") }
	project := func(p string) string {
		return projectPath(map[string]string{"organization_id": accOrg, "id": p + "-x"})
	}
	return append(kinds,
		moveKind{short: "sub", noun: "subnet", addr: subnetType + ".test", guarded: true,
			config: func(p, extra string) string {
				return fmt.Sprintf(`
resource %q "n" {
  id   = "%s-net"
  evpn = { service_type = "l3vpn", vlan_id = "auto" }
}
resource %q "test" {
  id            = "%s-x"
  network       = %s.n.id
  address_pool  = "/v1/regions/us-texas-1/networking/address-pools/tenant"
  prefix_length = 24
%s
}
`, networkType, p, subnetType, p, networkType, extra)
			},
			writes:  func(p string) []fakeRequest { return objectWrites(subnets(p)+"/"+p+"-x", subnets(p)) },
			deleted: func(p string) fakeRequest { return fakeRequest{http.MethodDelete, subnets(p) + "/" + p + "-x"} },
		},
		// A project's collection is the organization's, which other tests
		// create in, so only its own object is counted. A replacement deletes
		// first.
		moveKind{short: "p", noun: "project", addr: projectType + ".test", guarded: true,
			config: func(p, extra string) string {
				return fmt.Sprintf("\nresource %q \"test\" {\n  id = \"%s-x\"\n  organization_id = %q\n%s\n}\n",
					projectType, p, accOrg, extra)
			},
			writes: func(p string) []fakeRequest {
				return []fakeRequest{{http.MethodDelete, project(p)}, {http.MethodPatch, project(p)}}
			},
			deleted: func(p string) fakeRequest { return fakeRequest{http.MethodDelete, project(p)} },
		},
		moveKind{short: "peer", noun: "peering", addr: peeringType + ".test",
			config: func(p, extra string) string {
				return moveOwner(p) + fmt.Sprintf(`
resource %q "test" {
  id         = "%s-x"
  vpc        = "vpc-%s-owner"
  remote_vpc = "/v1/regions/us-texas-1/projects/other/networking/vpcs/vpc-inference"
  depends_on = [%s.owner]
%s
}
`, peeringType, p, p, clusterType, extra)
			},
			writes: func(p string) []fakeRequest {
				return objectWrites(vpc(p)+"/peerings/"+p+"-x", vpc(p)+"/peerings")
			},
			deleted: func(p string) fakeRequest { return fakeRequest{http.MethodDelete, vpc(p) + "/peerings/" + p + "-x"} },
		},
		// A binding is written, and taken off, with a POST to its VPC.
		moveKind{short: "bind", noun: "binding", addr: vpcBindingType + ".test",
			config: func(p, extra string) string {
				return moveOwner(p) + fmt.Sprintf(`
resource %q "sg" {
  id = "%s-sg"
  %s
}
resource %q "test" {
  vpc             = "vpc-%s-owner"
  security_groups = [%s.sg.id]
  depends_on      = [%s.owner]
%s
}
`, securityGroupType, p, moveEgress, vpcBindingType, p, securityGroupType, clusterType, extra)
			},
			writes:  func(p string) []fakeRequest { return []fakeRequest{{http.MethodPost, vpc(p)}} },
			deleted: func(p string) fakeRequest { return fakeRequest{http.MethodPost, vpc(p)} },
		},
	)
}

// moveRun is one kind's working directory, in its project.
type moveRun struct {
	*cliRun
	k moveKind
	p string
}

func (r *moveRun) write(extra string) { r.cliRun.write(inProject(r.p, r.k.config(r.p, extra))) }

// sent is how many of reqs the fake served while step ran.
func (r *moveRun) sent(reqs []fakeRequest, step func()) int {
	r.t.Helper()
	count := func() int {
		n := 0
		for _, q := range reqs {
			got, err := fakeServed(q.method, q.path)
			if err != nil {
				r.t.Fatal(err)
			}
			n += got
		}
		return n
	}
	before := count()
	step()
	return count() - before
}

const moveLine = `  region = "us-elsewhere-1"`

func startMove(t *testing.T, k moveKind) *moveRun {
	t.Helper()
	if runtime.GOOS == "windows" {
		skipTest(t, "the CLI sequence runs on unix")
	}
	r := &moveRun{cliRun: newCLIRun(t), k: k}
	r.p = ownProject(t, "acc-rmove-"+k.short)
	r.write("")
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

func TestAccARegionMoveIsRefusedAtApply(t *testing.T) {
	t.Parallel()
	for _, k := range moveKinds() {
		t.Run(k.short, func(t *testing.T) {
			t.Parallel()
			r := startMove(t, k)
			where := fmt.Sprintf(`region "us-elsewhere-1" with project %q`, r.p)
			if k.noun == "project" {
				where = `region "us-elsewhere-1"`
			}
			warned := fmt.Sprintf("Warning: This %s can't be in %s, so the apply will refuse the change", k.noun, where)
			refused := fmt.Sprintf("Error: This %s can't be in %s, so the apply refused the change", k.noun, where)
			back := "set `region` back to \"us-texas-1\""

			r.write(moveLine)
			var out string
			var code int
			if n := r.sent(k.writes(r.p), func() { out, code = r.runCode("plan", "-detailed-exitcode") }); code != 2 || n != 0 {
				t.Errorf("the plan exited %d with %d writes, want 2 and none:\n%s", code, n, out)
			}
			mustSay(t, "the plan", out, warned, "0 to add, 1 to change, 0 to destroy",
				"the apply refuses that update before it sends any request", "`terraform destroy` isn't affected", back)

			if n := r.sent(k.writes(r.p), func() { out, code = r.runCode("apply", "-auto-approve") }); code != 1 || n != 0 {
				t.Errorf("the apply exited %d with %d writes, want 1 and none:\n%s", code, n, out)
			}
			mustSay(t, "the apply", out, refused, "The apply refused before it sent any request", back)

			if k.guarded {
				r.write(moveLine + "\n  allow_replace = true")
				if n := r.sent(k.writes(r.p), func() { out, code = r.runCode("apply", "-auto-approve") }); code != 1 || n != 0 {
					t.Errorf("the apply with allow_replace exited %d with %d writes, want 1 and none:\n%s", code, n, out)
				}
				mustSay(t, "the apply with allow_replace", out, refused, "`allow_replace` doesn't change that")
				r.write(moveLine)
			}

			deleted := []fakeRequest{k.deleted(r.p)}
			if n := r.sent(deleted, func() { out, code = r.runCode("destroy", "-auto-approve") }); code != 0 || n != 1 {
				t.Errorf("the destroy exited %d with %d deletes, want 0 and one:\n%s", code, n, out)
			}
			mustSay(t, "the destroy", out, "Destroy complete!")
		})
	}
}

// With another change that still replaces the resource, Terraform would
// delete it before any update could refuse the move, so that plan is
// refused, and so is the destroy that runs it first. `terraform destroy
// -refresh=false` deletes. A guarded cluster's replacement takes
// allow_replace = true. Any change to a peering's display_name replaces it.
func TestAccARegionMoveWithAReplacementIsRefusedAtPlan(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		k       moveKind
		replace func(config string) string
		attr    string
	}{
		{moveKinds()[0], func(c string) string {
			return strings.Replace(c, `-x"`, `-y"`, 1)
		}, "id"},
		{moveKinds()[7], func(c string) string {
			return strings.Replace(c, "  depends_on", "  display_name = \"renamed\"\n  depends_on", 1)
		}, "display_name"},
	} {
		t.Run(tc.k.short, func(t *testing.T) {
			t.Parallel()
			k := tc.k
			k.short += "-r"
			r := startMove(t, k)
			extra := moveLine
			if k.guarded {
				extra += "\n  allow_replace = true"
			}
			r.cliRun.write(tc.replace(inProject(r.p, k.config(r.p, extra))))
			refused := fmt.Sprintf(`Error: This %s can't be in region "us-elsewhere-1" with project %q, and this plan replaces it`,
				k.noun, r.p)
			var out string
			var code int
			for _, step := range [][]string{{"plan"}, {"apply", "-auto-approve"}, {"destroy", "-auto-approve"}} {
				if n := r.sent(k.writes(r.p), func() { out, code = r.runCode(step...) }); code != 1 || n != 0 {
					t.Errorf("%s exited %d with %d writes, want 1 and none:\n%s", step[0], code, n, out)
				}
				mustSay(t, step[0], out, refused, "for its change to `"+tc.attr+"`", "`terraform destroy -refresh=false`")
			}
			deleted := []fakeRequest{k.deleted(r.p)}
			if n := r.sent(deleted, func() { out, code = r.runCode("destroy", "-auto-approve", "-refresh=false") }); code != 0 || n != 1 {
				t.Errorf("destroy -refresh=false exited %d with %d deletes, want 0 and one:\n%s", code, n, out)
			}
		})
	}
}

// A `project` move alone is refused the same way, and the warning and the
// error point at `project`. The fake's project made before regions isn't
// locked to one, so a resource in it can be in a region "demo" isn't in.
func TestAccAProjectMoveIsRefusedAtApply(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		skipTest(t, "the CLI sequence runs on unix")
	}
	const legacy, region = "before-regions", "us-elsewhere-1"
	at := func(p, rest string) string { return "/v1/regions/" + region + "/projects/" + p + rest }
	for _, tc := range []struct {
		short, noun, typ, body, rest string
		guarded                      bool
	}{
		{"net", "network", networkType, `evpn = { service_type = "l3vpn", vlan_id = "auto" }`, "/networking/networks", true},
		{"sg", "security group", securityGroupType, moveEgress, "/networking/security-groups", false},
	} {
		t.Run(tc.short, func(t *testing.T) {
			t.Parallel()
			id := "acc-pmove-" + tc.short
			config := func(project, extra string) string {
				return fmt.Sprintf("\nresource %q \"test\" {\n  id      = %q\n  region  = %q\n  project = %q\n  %s\n%s\n}\n",
					tc.typ, id, region, project, tc.body, extra)
			}
			object := at(legacy, tc.rest+"/"+id)
			writes := append(objectWrites(object, at(legacy, tc.rest)), fakeRequest{http.MethodPost, at("demo", tc.rest)})
			r := &moveRun{cliRun: newCLIRun(t)}
			r.cliRun.write(config(legacy, ""))
			if out, ok := r.run("apply", "-auto-approve"); !ok {
				t.Fatalf("the create failed:\n%s", out)
			}
			t.Cleanup(func() {
				if out, ok := r.run("destroy", "-auto-approve", "-refresh=false"); !ok {
					t.Errorf("cleanup destroy failed:\n%s", out)
				}
			})
			where := fmt.Sprintf(`region %q with project "demo"`, region)
			back := "set `project` back to \"" + legacy + "\""

			r.cliRun.write(config("demo", ""))
			var out string
			var code int
			if n := r.sent(writes, func() { out, code = r.runCode("plan", "-detailed-exitcode") }); code != 2 || n != 0 {
				t.Errorf("the plan exited %d with %d writes, want 2 and none:\n%s", code, n, out)
			}
			mustSay(t, "the plan", out, fmt.Sprintf("Warning: This %s can't be in %s, so the apply will refuse the change", tc.noun, where),
				`project = "demo"`, fmt.Sprintf(`from project %q to "demo"`, legacy), back)

			extras := []string{""}
			if tc.guarded {
				extras = append(extras, "  allow_replace = true")
			}
			for _, extra := range extras {
				r.cliRun.write(config("demo", extra))
				if n := r.sent(writes, func() { out, code = r.runCode("apply", "-auto-approve") }); code != 1 || n != 0 {
					t.Errorf("the apply (%q) exited %d with %d writes, want 1 and none:\n%s", extra, code, n, out)
				}
				mustSay(t, "the apply", out, fmt.Sprintf("Error: This %s can't be in %s, so the apply refused the change", tc.noun, where),
					`project = "demo"`, back)
			}

			r.cliRun.write(config("demo", ""))
			deleted := []fakeRequest{{http.MethodDelete, object}}
			if n := r.sent(deleted, func() { out, code = r.runCode("destroy", "-auto-approve") }); code != 0 || n != 1 {
				t.Errorf("the destroy exited %d with %d deletes, want 0 and one:\n%s", code, n, out)
			}
		})
	}
}
