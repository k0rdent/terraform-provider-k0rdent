package acceptance

import (
	"fmt"
	"net/http"
	"path"
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

// keyKind is one resource that holds SSH keys: a cluster or an instance group.
// Every key check runs for both, so neither can be left with a weaker check.
//
// Each has keys of its own. The fake makes one credential per key, so a test
// that refuses a key's reads or revokes it uses a key no other test holds:
// ownKey for kept.
type keyKind struct {
	typ  string
	noun string
	// pool is the node pool the resource is made with, if it needs one.
	pool   string
	pathOf func(map[string]string) string
	// prefix starts every object's name, so the two kinds never share one.
	prefix                                            string
	kept, rotated, revoked, importedRevoked, laterKey string
	// keptText is kept's twin for the cases that write keys in
	// ssh_public_keys, which refuse reads of their own credential.
	keptText string
}

var keyKinds = []keyKind{
	{
		typ:             clusterType,
		noun:            "cluster",
		pathOf:          clusterPath,
		prefix:          "acc-key",
		kept:            "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIDVRLoOGB4z4oua13sMYUEd49DnPqO+5ESIZFH2ljJ6S kept",
		rotated:         "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIN3EPSfa4SVPvt1x7J0vIMDndLM6i2W5gIEqQszx91uz rotated",
		revoked:         "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIDzY1dKtlRvKQf5wqwv1TkGUvWFTUkMQnJpsDqVb4tsQ revoked",
		importedRevoked: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIJMlBeYUQYN8adYlBqhlWvMmOr9ZOKTbfCU4CapmflM6 imported-revoked",
		laterKey:        "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIPZC4mmgLTAKvYFgWijrpOAFwbTeXLTZPHNKZ+yRnX/F unknown-later",
		keptText:        "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIK0k86Z327dihL0TR/17DwFIJgwvy4Rjt9XA37EztJtk kept-text-C",
	},
	{
		typ:             instanceGroupType,
		noun:            "instance group",
		pool:            "\n  node_pool    = { \"gpu-workers\" = { size = 1 } }",
		pathOf:          instanceGroupPath,
		prefix:          "acc-group-key",
		kept:            "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIL39HZl6dANYUmy+iinqWFw2HCPdtRIWZYbdliCrzguw group-kept",
		rotated:         "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOmcU98nftEUHdAimNmEhqp+ru/2EXbohuC8d5ZvwPPy group-rotated",
		revoked:         "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIJViuX0VbpaHakvWou/F+nUd+ZAvtAPaATzXRF5bZnQl group-revoked",
		importedRevoked: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIDEEVIaS1+L+h5bjYEsl/hROcsvKDF+WfwXRBondN1Vq group-imported-revoked",
		laterKey:        "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGYk7Q4Tzu1oDXOM6jLarjVv9yOU8/NzJOcHdLav1yD4 group-unknown-later",
		keptText:        "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIA3Ut3JPYIlbRFjEAYmSFDiGbwRvYTlLQ0UdO92tBCgU kept-text-G",
	},
}

// eachKeyKind runs check once per kind, as a subtest named for its type, so
// `-run '<test>/<type name>'` picks out one.
func eachKeyKind(t *testing.T, check func(t *testing.T, k keyKind)) {
	t.Helper()
	for _, k := range keyKinds {
		t.Run(k.typ, func(t *testing.T) { check(t, k) })
	}
}

func (k keyKind) adopted() string { return k.typ + ".adopted" }

// ownKey is kept with name in its comment, so its credential is name's alone.
func (k keyKind) ownKey(name string) string { return k.kept + "-" + name }

// resource is the resource at address with ops and its write-only key, or
// none, and whatever extra lines are given.
func (k keyKind) resource(name, address, key, extra string) string {
	keys := ""
	if key != "" {
		keys = fmt.Sprintf("\n  ssh_public_keys_wo = { ops = [%s] }", key)
	}
	return fmt.Sprintf(`
resource %q %q {%s
  id           = %q
  cluster_type = %q%s
  ssh_access   = [{ username = "ops" }]%s
}
`, k.typ, address, extra, name, clusterTypeRef, k.pool, keys)
}

// config is the made resource, or the adopted one beside the removed block and
// the import, with ops and its write-only key, or none.
func (k keyKind) config(name, address, key string) string {
	cfg := k.resource(name, address, quoted(key), "")
	if address == "test" {
		return cfg
	}
	return cfg + fmt.Sprintf(`
removed {
  from = %s.test
  lifecycle { destroy = false }
}
import {
  to = %s.adopted
  id = "us-texas-1/demo/%s"
}
`, k.typ, k.typ, name)
}

// adoptedConfig is the adopted resource once the import is done.
func (k keyKind) adoptedConfig(name, key string, replace bool) string {
	extra := ""
	if replace {
		extra = "\n  allow_replace = true"
	}
	return k.resource(name, "adopted", quoted(key), extra)
}

func quoted(key string) string {
	if key == "" {
		return ""
	}
	return fmt.Sprintf("%q", key)
}

// harness acts on the credential of the key the resource's ops user holds:
// revoke, refuse-reads or open-reads.
func (k keyKind) harness(s *terraform.State, action string) error {
	rs, ok := s.RootModule().Resources[k.adopted()]
	if !ok {
		rs, ok = s.RootModule().Resources[k.typ+".test"]
	}
	if !ok {
		return fmt.Errorf("no %s in state", k.noun)
	}
	ref := rs.Primary.Attributes["ssh_access.0.ssh_public_key_refs.0"]
	if ref == "" {
		return fmt.Errorf("the %s holds no key reference: %v", k.noun, rs.Primary.Attributes)
	}
	req, err := http.NewRequest(http.MethodPost, endpoint+"/_harness/regions/us-texas-1/projects/demo/credentials/"+path.Base(ref)+"/"+action, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+fakeOwnerToken)
	resp, err := harnessHTTP.Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("%s on %s answered %s", action, ref, resp.Status)
	}
	return nil
}

// After an import, a key change is checked against the fingerprint the
// platform holds: the same key imports with no change, another key is a
// replacement the guard refuses until the configuration permits it, and the
// replacement carries the new key. It never plans "No changes" over a key the
// nodes don't have.
func TestAccAKeyChangedAfterAnImportReplaces(t *testing.T) {
	t.Parallel()
	eachKeyKind(t, func(t *testing.T, k keyKind) {
		name := k.prefix + "-import"
		resource.Test(t, resource.TestCase{
			ProtoV6ProviderFactories: protoV6,
			CheckDestroy:             checkGone(k.typ, k.pathOf),
			Steps: []resource.TestStep{
				{Config: k.config(name, "test", k.kept)},
				{
					Config:           k.config(name, "adopted", k.kept),
					ConfigPlanChecks: expect(k.adopted(), plancheck.ResourceActionNoop),
				},
				{
					Config:           k.adoptedConfig(name, k.kept, false),
					ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()}},
				},
				{
					Config:      k.adoptedConfig(name, k.rotated, false),
					PlanOnly:    true,
					ExpectError: regexp.MustCompile(`would replace this ` + k.noun),
				},
				{
					Config:           k.adoptedConfig(name, k.rotated, true),
					ConfigPlanChecks: expect(k.adopted(), plancheck.ResourceActionReplace),
				},
				{
					Config:           k.adoptedConfig(name, k.rotated, true),
					ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()}},
				},
			},
		})
	})
}

// textKeys is the resource with ops's key in ssh_public_keys, the layout a
// configuration had before ssh_public_keys_wo.
func (k keyKind) textKeys(name, key string) string {
	return fmt.Sprintf(`
resource %q "test" {
  id           = %q
  cluster_type = %q%s
  ssh_access   = [{ username = "ops", ssh_public_keys = [%q] }]
}
`, k.typ, name, clusterTypeRef, k.pool, key)
}

// Keys made in ssh_public_keys and moved to ssh_public_keys_wo plan no change
// when they're the same keys, checked against the fingerprints the platform
// holds, and a replacement the guard refuses when they aren't.
func TestAccKeysMovedToWriteOnlyPlanNoChange(t *testing.T) {
	t.Parallel()
	eachKeyKind(t, func(t *testing.T, k keyKind) {
		name := k.prefix + "-moved"
		resource.Test(t, resource.TestCase{
			ProtoV6ProviderFactories: protoV6,
			CheckDestroy:             checkGone(k.typ, k.pathOf),
			Steps: []resource.TestStep{
				{Config: k.textKeys(name, k.kept)},
				{
					Config:           k.config(name, "test", k.kept),
					ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()}},
				},
				{
					Config:      k.config(name, "test", k.rotated),
					PlanOnly:    true,
					ExpectError: regexp.MustCompile(`would replace this ` + k.noun),
				},
			},
		})
	})
}

// An account that may not read the key's fingerprint imports a resource that
// leaves its keys out with no change. With keys set, the plan warns and shows
// an update, never no change, and the apply stops naming the role, every
// time. A destroy with those keys still set runs: it plans first.
func TestAccKeysThatCantBeCheckedStopTheApply(t *testing.T) {
	t.Parallel()
	stops := regexp.MustCompile(`(?s)The SSH keys can't be checked.*HTTP 403.*project.*credentials-admin`)
	eachKeyKind(t, func(t *testing.T, k keyKind) {
		name := k.prefix + "-refused"
		kept := k.ownKey(name)
		resource.Test(t, resource.TestCase{
			ProtoV6ProviderFactories: protoV6,
			CheckDestroy:             checkGone(k.typ, k.pathOf),
			Steps: []resource.TestStep{
				{
					Config: k.config(name, "test", kept),
					Check:  func(s *terraform.State) error { return k.harness(s, "refuse-reads") },
				},
				{
					Config:           k.config(name, "adopted", ""),
					ConfigPlanChecks: expect(k.adopted(), plancheck.ResourceActionNoop),
				},
				{
					Config:           k.adoptedConfig(name, kept, false),
					ConfigPlanChecks: expect(k.adopted(), plancheck.ResourceActionUpdate),
					ExpectError:      stops,
				},
				{
					Config:      k.adoptedConfig(name, kept, false),
					ExpectError: stops,
				},
			},
		})
	})
}

// A key whose credential an operator revoked warns on every plan and plans
// nothing: the nodes still take it, and a replacement would put it back.
func TestAccARevokedKeyWarnsAndPlansNothing(t *testing.T) {
	t.Parallel()
	eachKeyKind(t, func(t *testing.T, k keyKind) {
		name := k.prefix + "-revoked"
		resource.Test(t, resource.TestCase{
			ProtoV6ProviderFactories: protoV6,
			CheckDestroy:             checkGone(k.typ, k.pathOf),
			Steps: []resource.TestStep{
				{
					Config: k.config(name, "test", k.revoked),
					Check:  func(s *terraform.State) error { return k.harness(s, "revoke") },
				},
				{
					Config:           k.config(name, "test", k.revoked),
					ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()}},
				},
			},
		})
	})
}

// A revoked key keeps its fingerprint, so after an import the same key plans
// no change, with the revocation's warning, and a destroy with it set runs.
func TestAccARevokedKeyIsStillCheckedAfterAnImport(t *testing.T) {
	t.Parallel()
	eachKeyKind(t, func(t *testing.T, k keyKind) {
		name := k.prefix + "-revoked-import"
		resource.Test(t, resource.TestCase{
			ProtoV6ProviderFactories: protoV6,
			CheckDestroy:             checkGone(k.typ, k.pathOf),
			Steps: []resource.TestStep{
				{
					Config: k.config(name, "test", k.importedRevoked),
					Check:  func(s *terraform.State) error { return k.harness(s, "revoke") },
				},
				{
					Config:           k.config(name, "adopted", k.importedRevoked),
					ConfigPlanChecks: expect(k.adopted(), plancheck.ResourceActionNoop),
				},
				{
					Config:           k.adoptedConfig(name, k.importedRevoked, false),
					ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()}},
				},
			},
		})
	})
}

// A key not known until the apply, on an imported resource whose keys can't
// be read, can't be checked at plan either, in either attribute: the plan
// shows the update that stops, never no change and never a replacement.
func TestAccAKeyUnknownAtPlanThatCantBeCheckedStopsTheApply(t *testing.T) {
	t.Parallel()
	eachKeyKind(t, func(t *testing.T, k keyKind) {
		for _, layout := range []string{"write-only", "text"} {
			t.Run(layout, func(t *testing.T) {
				name := k.prefix + "-unknown-" + map[string]string{"write-only": "wo", "text": "text"}[layout]
				kept := k.ownKey(name)
				if layout == "text" {
					kept = k.keptText
				}
				cfg := k.invariantConfig("adopted", name, [2]string{"", k.laterKey}, keyConfig{layout, "unknown"}, false, true)
				resource.Test(t, resource.TestCase{
					ProtoV6ProviderFactories: protoV6,
					CheckDestroy:             checkGone(k.typ, k.pathOf),
					Steps: []resource.TestStep{
						{
							Config: k.config(name, "test", kept),
							Check:  func(s *terraform.State) error { return k.harness(s, "refuse-reads") },
						},
						{
							Config:           k.config(name, "adopted", ""),
							ConfigPlanChecks: expect(k.adopted(), plancheck.ResourceActionNoop),
						},
						{
							Config:           cfg,
							ConfigPlanChecks: expect(k.adopted(), plancheck.ResourceActionUpdate),
							ExpectError:      regexp.MustCompile(`The SSH keys can't be checked`),
						},
					},
				})
			})
		}
	})
}
