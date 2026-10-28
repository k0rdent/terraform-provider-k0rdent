package acceptance

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strings"
	"testing"
)

// madeBeforeRulesRequired lets a group in the fake have no rules. It stands
// for a group made before the platform refused one.
const madeBeforeRulesRequired = "fake-made-before-rules-required"

// A group made before the platform refused one with no rules can still be in
// state with none, and `terraform destroy` has to delete it. Through the real
// CLI:
//
//  1. `terraform apply` creates the group with a rule
//  2. its rules are emptied past Terraform, as a group from before reads
//  3. with no blocks, `terraform plan` plans no change and warns
//  4. `terraform destroy` deletes it, and state is empty
func TestAccASecurityGroupWithNoRulesInStateIsWarnedAndDestroyed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the CLI sequence runs on unix")
	}
	const name = "acc-no-rules-kept"
	path := "/v1/regions/us-texas-1/projects/demo/networking/security-groups/" + name
	c := newCLIRun(t)
	c.write(noRulesGroup(name, madeBeforeRulesRequired, oneEgressRule))
	if out, ok := c.run("apply", "-auto-approve"); !ok {
		t.Fatalf("the create with a rule failed:\n%s", out)
	}
	destroyed := false
	t.Cleanup(func() {
		if !destroyed {
			c.write(noRulesGroup(name, madeBeforeRulesRequired, oneEgressRule))
			if out, ok := c.run("destroy", "-auto-approve"); !ok {
				t.Errorf("cleanup destroy failed:\n%s", out)
			}
		}
	})

	emptyRulesPastTerraform(t, path)
	awaitFakeState(t, path, "active")
	c.write(noRulesGroup(name, madeBeforeRulesRequired, ""))

	out, ok := c.run("plan", "-detailed-exitcode")
	if !ok {
		t.Fatalf("the plan of a group with no rules in state wasn't a clean no-op:\n%s", out)
	}
	mustSay(t, "the plan", out, "No changes.", "Warning: This security group has no rules",
		"remove its resource block and apply, which deletes it")

	out, ok = c.run("destroy", "-auto-approve")
	if !ok {
		t.Fatalf("the destroy of a group with no rules failed:\n%s", out)
	}
	destroyed = true
	mustSay(t, "the destroy", out, "1 destroyed")
	if listed, ok := c.run("state", "list"); !ok || strings.TrimSpace(listed) != "" {
		t.Errorf("state isn't empty after the destroy:\n%s", listed)
	}
	if status, _, err := readFake(path); err != nil || status != http.StatusNotFound {
		t.Errorf("the fake still holds %s after the destroy: %d %v", name, status, err)
	}
}

// A group with no rules in state is refused at plan when the plan changes it,
// a tag or the provider's default_tags here, since every change sends the rules and the platform refuses a
// PATCH that has none, and when it's tainted, since its replacement is a
// create with no rules. `terraform destroy` runs that same plan first, so it
// stops on the same error, and the error names the way past it:
// `terraform destroy -refresh=false`, which deletes it. Through the real CLI.
func TestAccAChangeOrReplacementOfASecurityGroupWithNoRulesIsRefused(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the CLI sequence runs on unix")
	}
	for _, tc := range []struct {
		name, markers, state, extra, provider, says string
	}{
		{"acc-no-rules-changed", madeBeforeRulesRequired, "active", `  tags         = { team = "ml" }`, "",
			"a change to `tags` alone included"},
		{"acc-no-rules-default-tags", madeBeforeRulesRequired, "active", "", withDefaultTags(`{ team = "ml" }`),
			"a change to `tags` alone included"},
		{"acc-no-rules-tainted", madeBeforeRulesRequired + " " + settlesFailed, "failed", "", "",
			"the platform refuses to create a group with no rules"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := "/v1/regions/us-texas-1/projects/demo/networking/security-groups/" + tc.name
			c := newCLIRun(t)
			c.write(noRulesGroup(tc.name, tc.markers, oneEgressRule))
			out, ok := c.run("apply", "-auto-approve")
			if ok == (tc.state == "failed") {
				t.Fatalf("the create with a rule, settling %s, exited ok=%v:\n%s", tc.state, ok, out)
			}
			destroyed := false
			t.Cleanup(func() {
				if !destroyed {
					c.write(noRulesGroup(tc.name, tc.markers, oneEgressRule))
					if gone, done := c.run("destroy", "-auto-approve"); !done {
						t.Errorf("cleanup destroy failed:\n%s", gone)
					}
				}
			})

			emptyRulesPastTerraform(t, path)
			awaitFakeState(t, path, tc.state)
			c.write(tc.provider + noRulesGroup(tc.name, tc.markers, tc.extra))

			for _, args := range [][]string{{"plan"}, {"destroy", "-auto-approve"}} {
				refused, passed := c.run(args...)
				if passed {
					t.Fatalf("terraform %s passed:\n%s", args[0], refused)
				}
				mustSay(t, "terraform "+args[0], refused, "Error: A security group needs at least one rule", tc.says,
					"If this is `terraform destroy`, remove the block and apply instead, or run "+
						"`terraform destroy -refresh=false`.")
			}

			out, ok = c.run("destroy", "-auto-approve", "-refresh=false")
			if !ok {
				t.Fatalf("terraform destroy -refresh=false failed:\n%s", out)
			}
			destroyed = true
			mustSay(t, "terraform destroy -refresh=false", out, "1 destroyed")
			if listed, ok := c.run("state", "list"); !ok || strings.TrimSpace(listed) != "" {
				t.Errorf("state isn't empty after the destroy:\n%s", listed)
			}
			if status, _, err := readFake(path); err != nil || status != http.StatusNotFound {
				t.Errorf("the fake still holds %s after the destroy: %d %v", tc.name, status, err)
			}
		})
	}
}

func noRulesGroup(name, displayName, rules string) string {
	return fmt.Sprintf(`
resource %q "test" {
  id           = %q
  display_name = %q
%s
}
`, securityGroupType, name, displayName, rules)
}

// emptyRulesPastTerraform sends the fake a PATCH with both rule lists empty,
// which it takes only for a group carrying madeBeforeRulesRequired.
func emptyRulesPastTerraform(t *testing.T, path string) {
	t.Helper()
	token, err := mintFakeToken()
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPatch, endpoint+path,
		bytes.NewBufferString(`{"rules":{"ingress":[],"egress":[]}}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := harnessHTTP.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, rerr := io.ReadAll(resp.Body)
		t.Fatalf("PATCH %s answered %d: %s %v", path, resp.StatusCode, body, rerr)
	}
}
