package acceptance

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// madeBeforeRulesRequired keeps a fake group active with no rules for these
// legacy-state lifecycle tests. Other empty groups settle failed, as on API 1.3.
const madeBeforeRulesRequired = "fake-made-before-rules-required"

// An unchanged group with no rules warns without planning a change. A normal
// apply, an apply without refresh, and both saved and unsaved destroy plans
// preserve that lifecycle through the real CLI.
func TestAccASecurityGroupWithNoRulesInStateIsWarnedAndDestroyed(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		skipTest(t, "the CLI sequence runs on unix")
	}
	for _, tc := range []struct {
		name           string
		withoutRefresh bool
		savedDestroy   bool
	}{
		{name: "normal apply and destroy"},
		{name: "apply and destroy without refresh", withoutRefresh: true},
		{name: "saved destroy plan", savedDestroy: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			name := "acc-no-rules-kept-" + strings.ReplaceAll(tc.name, " ", "-")
			c, path := unchangedRulelessGroup(t, name)
			out, code := c.runCode("plan", "-detailed-exitcode")
			if code != 0 {
				t.Fatalf("the no-op plan of a group with no rules exited %d:\n%s", code, out)
			}
			mustSay(t, "the plan", out, "No changes.", "Warning: This security group has no rules",
				"remove its resource block and apply, which deletes it")

			out, ok := c.run("apply", "-auto-approve")
			if !ok {
				t.Fatalf("the no-op apply of a group with no rules failed:\n%s", out)
			}
			mustSay(t, "the apply", out, "0 added, 0 changed, 0 destroyed")

			if tc.withoutRefresh {
				out, code = c.runCode("plan", "-detailed-exitcode", "-refresh=false")
				if code != 0 {
					t.Fatalf("the no-op plan without refresh exited %d:\n%s", code, out)
				}
				mustSay(t, "the plan without refresh", out, "No changes.", "Warning: This security group has no rules")
				out, ok = c.run("apply", "-auto-approve", "-refresh=false")
				if !ok {
					t.Fatalf("the no-op apply without refresh failed:\n%s", out)
				}
				mustSay(t, "the apply without refresh", out, "0 added, 0 changed, 0 destroyed")
			}

			if tc.savedDestroy {
				out, code = c.runCode("plan", "-destroy", "-detailed-exitcode", "-out=destroy.tfplan")
				if code != 2 {
					t.Fatalf("the saved destroy plan exited %d, want 2:\n%s", code, out)
				}
				mustSay(t, "the saved destroy plan", out, "Plan: 0 to add, 0 to change, 1 to destroy.")
				out, ok = c.run("apply", "destroy.tfplan")
			} else {
				destroyArgs := []string{"destroy", "-auto-approve"}
				if tc.withoutRefresh {
					destroyArgs = append(destroyArgs, "-refresh=false")
				}
				out, ok = c.run(destroyArgs...)
			}
			if !ok {
				t.Fatalf("the destroy of a group with no rules failed:\n%s", out)
			}
			mustSay(t, "the destroy", out, "1 destroyed")
			assertGroupDestroyed(t, c, path)
		})
	}
}

func unchangedRulelessGroup(t *testing.T, name string) (*cliRun, string) {
	t.Helper()
	c := newCLIRun(t)
	c.write(noRulesGroup(name, madeBeforeRulesRequired, oneEgressRule))
	if out, ok := c.run("apply", "-auto-approve"); !ok {
		t.Fatalf("the create with a rule failed:\n%s", out)
	}
	t.Cleanup(func() {
		c.write(noRulesGroup(name, madeBeforeRulesRequired, oneEgressRule))
		if out, ok := c.run("destroy", "-auto-approve", "-refresh=false"); !ok {
			t.Errorf("cleanup destroy failed:\n%s", out)
		}
	})
	path := "/v1/regions/us-texas-1/projects/demo/networking/security-groups/" + name
	emptyRulesPastTerraform(t, path)
	awaitFakeState(t, path, "active")
	c.write(noRulesGroup(name, madeBeforeRulesRequired, ""))
	return c, path
}

// Import adopts a group with no rules without turning its unchanged plan into
// a refusal. A no-op apply and ordinary destroy still work.
func TestAccImportedSecurityGroupWithNoRulesPlansAndDestroysCleanly(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		skipTest(t, "the CLI sequence runs on unix")
	}
	const name = "acc-no-rules-imported"
	c, path := unchangedRulelessGroup(t, name)
	if out, ok := c.run("state", "rm", securityGroupType+".test"); !ok {
		t.Fatalf("removing the group from state before import failed:\n%s", out)
	}
	if out, ok := c.run("import", securityGroupType+".test", name); !ok {
		t.Fatalf("importing the group with no rules failed:\n%s", out)
	}
	out, code := c.runCode("plan", "-detailed-exitcode")
	if code != 0 {
		t.Fatalf("the plan after importing a group with no rules exited %d:\n%s", code, out)
	}
	mustSay(t, "the plan after import", out, "No changes.", "Warning: This security group has no rules")
	out, ok := c.run("apply", "-auto-approve")
	if !ok {
		t.Fatalf("the no-op apply after import failed:\n%s", out)
	}
	mustSay(t, "the apply after import", out, "0 added, 0 changed, 0 destroyed")
	out, ok = c.run("destroy", "-auto-approve")
	if !ok {
		t.Fatalf("destroying the imported group failed:\n%s", out)
	}
	mustSay(t, "the destroy after import", out, "1 destroyed")
	assertGroupDestroyed(t, c, path)
}

// A module output keeps the rule list unknown at plan. An empty result is
// refused by Create, and one resolved rule applies and replans empty.
func TestAccUnknownSecurityGroupRulesFromModuleOutputResolveAtApply(t *testing.T) {
	t.Parallel()
	testUnknownSecurityGroupRules(t, "module-output")
}

// Static resource for_each keys keep the group instances known while their
// rule lists stay unknown at plan. Apply checks the resolved lists.
func TestAccUnknownSecurityGroupRulesWithResourceForEachResolveAtApply(t *testing.T) {
	t.Parallel()
	testUnknownSecurityGroupRules(t, "resource-for-each")
}

// A dynamic block over a resource output defers the no-rule decision until
// apply. The empty and one-rule results take their respective paths.
func TestAccUnknownSecurityGroupRulesFromDynamicBlockResolveAtApply(t *testing.T) {
	t.Parallel()
	testUnknownSecurityGroupRules(t, "dynamic-block")
}

func testUnknownSecurityGroupRules(t *testing.T, source string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		skipTest(t, "the CLI sequence runs on unix")
	}
	for _, tc := range []struct {
		name, slug, rules string
	}{
		{name: "empty-result-is-refused-at-apply", slug: "empty", rules: `[]`},
		{name: "one-rule-applies-and-replans-empty", slug: "one", rules: `[{
      action             = "permit"
      protocol           = "any"
      source_prefix      = "0.0.0.0/0"
      destination_prefix = "0.0.0.0/0"
    }]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			name := "acc-unknown-" + source + "-" + tc.slug
			c := newCLIRun(t)
			addr := writeUnknownRulesConfig(t, c, source, name, tc.rules)
			if source == "module-output" {
				if out, ok := c.run("init", "-backend=false"); !ok {
					t.Fatalf("initializing the local rules module failed:\n%s", out)
				}
			}
			t.Cleanup(func() {
				if out, ok := c.run("destroy", "-auto-approve", "-refresh=false"); !ok {
					t.Errorf("cleanup destroy failed:\n%s", out)
				}
			})

			out, code := c.runCode("plan", "-detailed-exitcode", "-out=rules.tfplan")
			if code != 2 {
				t.Fatalf("the plan with unknown rules exited %d, want 2:\n%s", code, out)
			}
			mustSay(t, "the plan with unknown rules", out, "known after apply")

			out, ok := c.run("apply", "rules.tfplan")
			if tc.rules == `[]` {
				if ok {
					t.Fatalf("applying the resolved empty rule list passed:\n%s", out)
				}
				mustSay(t, "the apply with resolved empty rules", out,
					"Error: A security group needs at least one rule", "API 1.4",
					"Add an `ingress` or `egress` block")
				if listed, listedOK := c.run("state", "list"); !listedOK || strings.Contains(listed, addr) {
					t.Errorf("state contains a group after the refused apply:\n%s", listed)
				}
				path := "/v1/regions/us-texas-1/projects/demo/networking/security-groups/" + name
				if status, _, err := readFake(path); err != nil || status != http.StatusNotFound {
					t.Errorf("the refused apply created a fake group: %d %v", status, err)
				}
				return
			}
			if !ok {
				t.Fatalf("applying one resolved rule failed:\n%s", out)
			}
			shown, ok := c.run("state", "show", addr)
			if !ok {
				t.Fatalf("showing the group with one resolved rule failed:\n%s", shown)
			}
			mustSay(t, "state with one resolved rule", shown, "rule_count = 1", "ingress {")
			out, code = c.runCode("plan", "-detailed-exitcode")
			if code != 0 {
				t.Fatalf("the plan after applying one resolved rule exited %d:\n%s", code, out)
			}
			mustSay(t, "the plan after one resolved rule", out, "No changes.")
		})
	}
}

func writeUnknownRulesConfig(t *testing.T, c *cliRun, source, name, rules string) string {
	t.Helper()
	producer := fmt.Sprintf(`
resource "terraform_data" "rules" {
  input = %s
}
`, rules)
	rulesExpression := "terraform_data.rules.output"
	moduleBlock := ""
	if source == "module-output" {
		producer = ""
		rulesExpression = "module.rules.value"
		moduleBlock = `module "rules" { source = "./rules" }`
		dir := filepath.Join(c.dir, "rules")
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(dir, "main.tf"), fmt.Sprintf(`
resource "terraform_data" "rules" {
  input = %s
}

output "value" {
  value = terraform_data.rules.output
}
`, rules))
	}
	forEach := ""
	id := fmt.Sprintf("id = %q", name)
	addr := securityGroupType + ".test"
	if source == "resource-for-each" {
		forEach = fmt.Sprintf("for_each = { test = %q }", name)
		id = "id = each.value"
		addr += `["test"]`
	}
	c.write(fmt.Sprintf(`%s
%s

resource %q "test" {
  %s
  %s

  dynamic "ingress" {
    for_each = %s
    content {
      action             = ingress.value.action
      protocol           = ingress.value.protocol
      source_prefix      = ingress.value.source_prefix
      destination_prefix = ingress.value.destination_prefix
    }
  }
}
`, producer, moduleBlock, securityGroupType, forEach, id, rulesExpression))
	return addr
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
