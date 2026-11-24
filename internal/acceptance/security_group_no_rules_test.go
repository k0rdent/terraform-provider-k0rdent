package acceptance

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// madeBeforeRulesRequired keeps a fake group active with no rules for these
// legacy-state lifecycle tests. Other empty groups settle failed, as on API 1.3.
const madeBeforeRulesRequired = "fake-made-before-rules-required"

// A group already in state with no rules can plan no change with a warning,
// and `terraform destroy` has to delete it. Through the real
// CLI:
//
//  1. `terraform apply` creates the group with a rule
//  2. its rules are emptied past Terraform, as a group from before reads
//  3. with no blocks, `terraform plan` plans no change and warns
//  4. `terraform destroy` deletes it, and state is empty
func TestAccASecurityGroupWithNoRulesInStateIsWarnedAndDestroyed(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		skipTest(t, "the CLI sequence runs on unix")
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

// A configuration change to a group with no rules is refused at plan, a tag
// or the provider's default_tags here, since every change sends the rules. The
// refusal is configuration-caused, so `terraform destroy -refresh=false` is the
// way out when the same configuration would stop the destroy's refresh plan.
func TestAccAChangeOrReplacementOfASecurityGroupWithNoRulesIsRefused(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		skipTest(t, "the CLI sequence runs on unix")
	}
	for _, tc := range []struct {
		name, extra, provider string
	}{
		{"acc-no-rules-changed", `  tags         = { team = "ml" }`, ""},
		{"acc-no-rules-default-tags", "", withDefaultTags(`{ team = "ml" }`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := "/v1/regions/us-texas-1/projects/demo/networking/security-groups/" + tc.name
			c := newCLIRun(t)
			c.write(noRulesGroup(tc.name, madeBeforeRulesRequired, oneEgressRule))
			if out, ok := c.run("apply", "-auto-approve"); !ok {
				t.Fatalf("the create with a rule failed:\n%s", out)
			}
			destroyed := false
			t.Cleanup(func() {
				if !destroyed {
					c.write(noRulesGroup(tc.name, madeBeforeRulesRequired, oneEgressRule))
					if gone, done := c.run("destroy", "-auto-approve"); !done {
						t.Errorf("cleanup destroy failed:\n%s", gone)
					}
				}
			})

			emptyRulesPastTerraform(t, path)
			awaitFakeState(t, path, "active")
			c.write(tc.provider + noRulesGroup(tc.name, madeBeforeRulesRequired, tc.extra))

			for _, args := range [][]string{{"plan"}, {"destroy", "-auto-approve"}} {
				refused, passed := c.run(args...)
				if passed {
					t.Fatalf("terraform %s passed:\n%s", args[0], refused)
				}
				mustSay(t, "terraform "+args[0], refused, "Error: A security group needs at least one rule",
					"a change to `tags` alone included", "API 1.4", "API 1.3",
					"If this is `terraform destroy`, remove the block and apply instead, or run "+
						"`terraform destroy -refresh=false`.")
			}

			out, ok := c.run("destroy", "-auto-approve", "-refresh=false")
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

// Tainting changes what Terraform will do, not what the platform holds. No
// rules in configuration make the replacement's create invalid for either id
// form. Through the real CLI, plan refuses, destroy without refresh deletes,
// and adding a rule lets the replacement apply.
func TestAccTaintedRulelessGroupRefusalComesFromConfiguration(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		skipTest(t, "the CLI sequence runs on unix")
	}

	for _, tc := range []struct {
		name, slug   string
		configuredID bool
	}{
		{"configured id", "configured", true},
		{"generated id", "generated", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Run("plan refused", func(t *testing.T) {
				c, _ := taintedRulelessGroup(t, "acc-no-rules-"+tc.slug+"-tainted-plan", tc.configuredID)
				out, code := c.runCode("plan", "-detailed-exitcode")
				if code != 1 {
					t.Fatalf("plan of a tainted group with no rules exited %d, want 1:\n%s", code, out)
				}
				mustSay(t, "the plan", out, "Error: A security group needs at least one rule",
					"Add an `ingress` or `egress` block", "`terraform destroy -refresh=false`")
			})

			t.Run("destroy without refresh", func(t *testing.T) {
				c, path := taintedRulelessGroup(t, "acc-no-rules-"+tc.slug+"-tainted-no-refresh", tc.configuredID)
				out, ok := c.run("destroy", "-auto-approve", "-refresh=false")
				if !ok {
					t.Fatalf("terraform destroy -refresh=false failed:\n%s", out)
				}
				mustSay(t, "the destroy", out, "1 destroyed")
				assertGroupDestroyed(t, c, path)
			})

			t.Run("rule allows replacement", func(t *testing.T) {
				name := "acc-no-rules-" + tc.slug + "-tainted-replacement"
				c, _ := taintedRulelessGroup(t, name, tc.configuredID)
				c.write(noRulesGroupConfig(name, madeBeforeRulesRequired, oneEgressRule, tc.configuredID))
				out, code := c.runCode("plan", "-detailed-exitcode")
				if code != 2 {
					t.Fatalf("plan after adding a rule exited %d, want 2:\n%s", code, out)
				}
				mustSay(t, "the replacement plan", out, "must be replaced",
					"Plan: 1 to add, 0 to change, 1 to destroy.")
				out, ok := c.run("apply", "-auto-approve")
				if !ok {
					t.Fatalf("replacement after adding a rule failed:\n%s", out)
				}
				mustSay(t, "the replacement", out, "1 added, 0 changed, 1 destroyed")
			})
		})
	}
}

func taintedRulelessGroup(t *testing.T, name string, configuredID bool) (*cliRun, string) {
	t.Helper()
	c := newCLIRun(t)
	c.write(noRulesGroupConfig(name, madeBeforeRulesRequired, oneEgressRule, configuredID))
	if out, ok := c.run("apply", "-auto-approve"); !ok {
		t.Fatalf("the create with a rule failed:\n%s", out)
	}
	t.Cleanup(func() {
		if out, ok := c.run("destroy", "-auto-approve", "-refresh=false"); !ok {
			t.Errorf("cleanup destroy failed:\n%s", out)
		}
	})
	id := name
	if !configuredID {
		shown, ok := c.run("state", "show", securityGroupType+".test")
		if !ok {
			t.Fatalf("showing the generated id failed:\n%s", shown)
		}
		match := regexp.MustCompile(`(?m)^\s+id\s+=\s+"([^"]+)"`).FindStringSubmatch(shown)
		if match == nil {
			t.Fatalf("state has no generated id:\n%s", shown)
		}
		id = match[1]
	}
	path := "/v1/regions/us-texas-1/projects/demo/networking/security-groups/" + id
	emptyRulesPastTerraform(t, path)
	awaitFakeState(t, path, "active")
	c.write(noRulesGroupConfig(name, madeBeforeRulesRequired, "", configuredID))
	if out, ok := c.run("taint", securityGroupType+".test"); !ok {
		t.Fatalf("tainting the group failed:\n%s", out)
	}
	return c, path
}

func assertGroupDestroyed(t *testing.T, c *cliRun, path string) {
	t.Helper()
	if listed, ok := c.run("state", "list"); !ok || strings.TrimSpace(listed) != "" {
		t.Errorf("state isn't empty after the destroy:\n%s", listed)
	}
	if status, _, err := readFake(path); err != nil || status != http.StatusNotFound {
		t.Errorf("the fake still holds the group after the destroy: %d %v", status, err)
	}
}

func noRulesGroup(name, displayName, rules string) string {
	return noRulesGroupConfig(name, displayName, rules, true)
}

func noRulesGroupConfig(name, displayName, rules string, configuredID bool) string {
	id := ""
	if configuredID {
		id = fmt.Sprintf("  id           = %q\n", name)
	}
	return fmt.Sprintf(`
resource %q "test" {
%s  display_name = %q
%s
}
`, securityGroupType, id, displayName, rules)
}

// emptyRulesPastTerraform sends the fake a PATCH with both rule lists empty.
// The legacy marker keeps this fixture active instead of settling failed.
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
