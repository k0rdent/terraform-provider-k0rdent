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

// Keys of this file's own, so refusing their reads or revoking them touches
// no other test's: the fake makes one credential per key.
const (
	keptKey            = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIDVRLoOGB4z4oua13sMYUEd49DnPqO+5ESIZFH2ljJ6S kept"
	rotatedKey         = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIN3EPSfa4SVPvt1x7J0vIMDndLM6i2W5gIEqQszx91uz rotated"
	revokedKey         = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIDzY1dKtlRvKQf5wqwv1TkGUvWFTUkMQnJpsDqVb4tsQ revoked"
	importedRevokedKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIJMlBeYUQYN8adYlBqhlWvMmOr9ZOKTbfCU4CapmflM6 imported-revoked"
	laterKey           = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIPZC4mmgLTAKvYFgWijrpOAFwbTeXLTZPHNKZ+yRnX/F unknown-later"
)

// keyCheckConfig is the made cluster, or the adopted one beside the removed
// block and the import, with ops and its write-only key, or none.
func keyCheckConfig(name, address, key string) string {
	keys := ""
	if key != "" {
		keys = fmt.Sprintf("\n  ssh_public_keys_wo = { ops = [%q] }", key)
	}
	cfg := fmt.Sprintf(`
resource %q %q {
  id           = %q
  cluster_type = %q
  ssh_access   = [{ username = "ops" }]%s
}
`, clusterType, address, name, clusterTypeRef, keys)
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
`, clusterType, clusterType, name)
}

// keyCheckAdopted is the adopted cluster once the import is done.
func keyCheckAdopted(name, key string, replace bool) string {
	keys := ""
	if key != "" {
		keys = fmt.Sprintf("\n  ssh_public_keys_wo = { ops = [%q] }", key)
	}
	cfg := fmt.Sprintf(`
resource %q "adopted" {
  id           = %q
  cluster_type = %q
  ssh_access   = [{ username = "ops" }]%s
}
`, clusterType, name, clusterTypeRef, keys)
	if replace {
		cfg = regexp.MustCompile(`(resource "[^"]+" "adopted" \{\n)`).ReplaceAllString(cfg, "${1}  allow_replace = true\n")
	}
	return cfg
}

// keyHarness acts on the credential of the key the adopted cluster's ops
// user holds: revoke, refuse-reads or open-reads.
func keyHarness(s *terraform.State, action string) error {
	rs, ok := s.RootModule().Resources[clusterType+".adopted"]
	if !ok {
		rs, ok = s.RootModule().Resources[clusterAddr]
	}
	if !ok {
		return fmt.Errorf("no cluster in state")
	}
	ref := rs.Primary.Attributes["ssh_access.0.ssh_public_key_refs.0"]
	if ref == "" {
		return fmt.Errorf("the cluster holds no key reference: %v", rs.Primary.Attributes)
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
	const name = "acc-key-import"
	adopted := clusterType + ".adopted"
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy:             checkGone(clusterType, clusterPath),
		Steps: []resource.TestStep{
			{Config: keyCheckConfig(name, "test", keptKey)},
			{
				Config:           keyCheckConfig(name, "adopted", keptKey),
				ConfigPlanChecks: expect(adopted, plancheck.ResourceActionNoop),
			},
			{
				Config:           keyCheckAdopted(name, keptKey, false),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()}},
			},
			{
				Config:      keyCheckAdopted(name, rotatedKey, false),
				PlanOnly:    true,
				ExpectError: regexp.MustCompile(`would replace this cluster`),
			},
			{
				Config:           keyCheckAdopted(name, rotatedKey, true),
				ConfigPlanChecks: expect(adopted, plancheck.ResourceActionReplace),
			},
			{
				Config:           keyCheckAdopted(name, rotatedKey, true),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()}},
			},
		},
	})
}

// An account that may not read the key's fingerprint imports a cluster that
// leaves its keys out with no change. With keys set, the plan warns and shows
// an update, never no change, and the apply stops naming the role, every
// time. A destroy with those keys still set runs: it plans first.
func TestAccKeysThatCantBeCheckedStopTheApply(t *testing.T) {
	const name = "acc-key-refused"
	stops := regexp.MustCompile(`(?s)The SSH keys can't be checked.*HTTP 403.*project.*credentials-admin`)
	adopted := clusterType + ".adopted"
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy:             checkGone(clusterType, clusterPath),
		Steps: []resource.TestStep{
			{
				Config: keyCheckConfig(name, "test", keptKey),
				Check:  func(s *terraform.State) error { return keyHarness(s, "refuse-reads") },
			},
			{
				Config:           keyCheckConfig(name, "adopted", ""),
				ConfigPlanChecks: expect(adopted, plancheck.ResourceActionNoop),
			},
			{
				Config:           keyCheckAdopted(name, keptKey, false),
				ConfigPlanChecks: expect(adopted, plancheck.ResourceActionUpdate),
				ExpectError:      stops,
			},
			{
				Config:      keyCheckAdopted(name, keptKey, false),
				ExpectError: stops,
			},
		},
	})
}

// A key whose credential an operator revoked warns on every plan and plans
// nothing: the nodes still take it, and a replacement would put it back.
func TestAccARevokedKeyWarnsAndPlansNothing(t *testing.T) {
	const name = "acc-key-revoked"
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy:             checkGone(clusterType, clusterPath),
		Steps: []resource.TestStep{
			{
				Config: keyCheckConfig(name, "test", revokedKey),
				Check:  func(s *terraform.State) error { return keyHarness(s, "revoke") },
			},
			{
				Config:           keyCheckConfig(name, "test", revokedKey),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()}},
			},
		},
	})
}

// A revoked key keeps its fingerprint, so after an import the same key plans
// no change, with the revocation's warning, and a destroy with it set runs.
func TestAccARevokedKeyIsStillCheckedAfterAnImport(t *testing.T) {
	const name = "acc-key-revoked-import"
	adopted := clusterType + ".adopted"
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy:             checkGone(clusterType, clusterPath),
		Steps: []resource.TestStep{
			{
				Config: keyCheckConfig(name, "test", importedRevokedKey),
				Check:  func(s *terraform.State) error { return keyHarness(s, "revoke") },
			},
			{
				Config:           keyCheckConfig(name, "adopted", importedRevokedKey),
				ConfigPlanChecks: expect(adopted, plancheck.ResourceActionNoop),
			},
			{
				Config:           keyCheckAdopted(name, importedRevokedKey, false),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()}},
			},
		},
	})
}

// A key not known until the apply, on an imported cluster whose keys can't be
// read, can't be checked at plan either: the plan shows the update that stops,
// never no change.
func TestAccAKeyUnknownAtPlanThatCantBeCheckedStopsTheApply(t *testing.T) {
	const name = "acc-key-unknown"
	adopted := clusterType + ".adopted"
	cfg := fmt.Sprintf(`
resource "terraform_data" "k" {
  input = %q
}
resource %q "adopted" {
  id                 = %q
  cluster_type       = %q
  ssh_access         = [{ username = "ops" }]
  ssh_public_keys_wo = { ops = [terraform_data.k.output] }
}
`, laterKey, clusterType, name, clusterTypeRef)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy:             checkGone(clusterType, clusterPath),
		Steps: []resource.TestStep{
			{
				Config: keyCheckConfig(name, "test", keptKey),
				Check:  func(s *terraform.State) error { return keyHarness(s, "refuse-reads") },
			},
			{
				Config:           keyCheckConfig(name, "adopted", ""),
				ConfigPlanChecks: expect(adopted, plancheck.ResourceActionNoop),
			},
			{
				Config:           cfg,
				ConfigPlanChecks: expect(adopted, plancheck.ResourceActionUpdate),
				ExpectError:      regexp.MustCompile(`The SSH keys can't be checked`),
			},
		},
	})
}
