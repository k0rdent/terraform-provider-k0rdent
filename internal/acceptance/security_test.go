package acceptance

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/provider"
)

var (
	securityGroupType = provider.ResourceName("security_group")
	quotaType         = provider.ResourceName("organization_quota")
	legacyQuotaType   = provider.ResourceName("tenant_quota")
	securityGroupAddr = securityGroupType + ".test"
	quotaAddr         = "data." + quotaType + ".org"
	legacyQuotaAddr   = "data." + legacyQuotaType + ".org"
	projectQuotaType  = provider.ResourceName("project_quota")
	projectQuotaAddr  = "data." + projectQuotaType + ".team"
)

var regexpPortsOnICMP = regexp.MustCompile(`Ports cannot be set on this protocol`)

func securityGroupConfig(name string, extraIngress bool) string {
	extra := ""
	if extraIngress {
		extra = `
  ingress {
    name               = "allow-metrics"
    action             = "permit"
    protocol           = "tcp"
    source_prefix      = "10.0.0.0/8"
    destination_prefix = "0.0.0.0/0"

    destination_port_range = "9090"
  }
`
	}
	return fmt.Sprintf(`
resource %q "test" {
  id           = %q
  display_name = "acceptance"
  tags         = { team = "ml" }

  ingress {
    name               = "allow-ssh-from-office"
    action             = "permit"
    protocol           = "tcp"
    source_prefix      = "203.0.113.0/24"
    destination_prefix = "0.0.0.0/0"

    destination_port_range = "22"
  }
%s
  egress {
    action             = "permit"
    protocol           = "any"
    source_prefix      = "0.0.0.0/0"
    destination_prefix = "0.0.0.0/0"
  }
}
`, securityGroupType, name, extra)
}

// The rule set is replaced wholesale on update, so the test that matters is
// adding a rule and checking the other two survived.
func TestAccSecurityGroup(t *testing.T) {
	const name = "acc-filter"
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy:             checkGone(securityGroupType, securityGroupPath),
		Steps: []resource.TestStep{
			{
				Config: securityGroupConfig(name, false),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(securityGroupAddr, "id", name),
					resource.TestCheckResourceAttr(securityGroupAddr, "state", "active"),
					resource.TestCheckResourceAttr(securityGroupAddr, "rule_count", "2"),
					resource.TestCheckResourceAttr(securityGroupAddr, "ingress.0.name", "allow-ssh-from-office"),
					resource.TestCheckResourceAttr(securityGroupAddr, "egress.0.protocol", "any"),
				),
			},
			{
				Config:   securityGroupConfig(name, false),
				PlanOnly: true,
			},
			{
				Config: securityGroupConfig(name, true),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(securityGroupAddr, "rule_count", "3"),
					// Order is the filter, so position is asserted, not membership.
					resource.TestCheckResourceAttr(securityGroupAddr, "ingress.0.name", "allow-ssh-from-office"),
					resource.TestCheckResourceAttr(securityGroupAddr, "ingress.1.name", "allow-metrics"),
					resource.TestCheckResourceAttr(securityGroupAddr, "egress.0.protocol", "any"),
				),
			},
			{
				Config:            securityGroupConfig(name, true),
				ResourceName:      securityGroupAddr,
				ImportState:       true,
				ImportStateId:     "us-texas-1/demo/" + name,
				ImportStateVerify: true,
			},
		},
	})
}

// A port range on icmp is a 422 upstream. It should never get that far.
func TestAccSecurityGroupRejectsPortsOnICMP(t *testing.T) {
	config := fmt.Sprintf(`
resource %q "test" {
  id = "acc-bad"
  ingress {
    action             = "permit"
    protocol           = "icmp"
    source_prefix      = "0.0.0.0/0"
    destination_prefix = "0.0.0.0/0"

    destination_port_range = "22"
  }
}
`, securityGroupType)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		Steps: []resource.TestStep{{
			Config:      config,
			PlanOnly:    true,
			ExpectError: regexpPortsOnICMP,
		}},
	})
}

// Each of these is a 422 the server would raise partway through an approved
// apply. `PlanOnly` is what proves the diagnostic arrives before the approval,
// not after it: without the validator the plan succeeds and the step passes.
func TestAccSecurityGroupRejectsBadRuleValuesInThePlan(t *testing.T) {
	// One well-formed rule, one field replaced per case.
	rule := map[string]string{
		"action":             `"permit"`,
		"protocol":           `"tcp"`,
		"source_prefix":      `"0.0.0.0/0"`,
		"destination_prefix": `"0.0.0.0/0"`,
	}

	tests := []struct {
		name   string
		field  string
		value  string
		expect *regexp.Regexp
	}{
		{
			name: "an action the contract does not declare", field: "action", value: `"allow"`,
			expect: regexp.MustCompile(`(?s)action.*value must be one of`),
		},
		{
			name: "a protocol spelled for another platform", field: "protocol", value: `"TCP"`,
			expect: regexp.MustCompile(`(?s)protocol.*value must be one of`),
		},
		{
			name: "a prefix with host bits set", field: "source_prefix", value: `"10.1.2.3/24"`,
			expect: regexp.MustCompile(`CIDR prefix has host bits set`),
		},
		{
			name: "a prefix that is not one", field: "destination_prefix", value: `"10.0.0.0"`,
			expect: regexp.MustCompile(`Not a CIDR prefix`),
		},
		{
			name: "a port range that runs backwards", field: "destination_port_range", value: `"443-80"`,
			expect: regexp.MustCompile(`Port range runs backwards`),
		},
		{
			name: "a port outside the space", field: "source_port_range", value: `"65536"`,
			expect: regexp.MustCompile(`Not a port range`),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := ""
			for _, field := range []string{"action", "protocol", "source_prefix", "destination_prefix"} {
				value := rule[field]
				if field == tt.field {
					value = tt.value
				}
				body += fmt.Sprintf("    %s = %s\n", field, value)
			}
			if _, fixed := rule[tt.field]; !fixed {
				body += fmt.Sprintf("    %s = %s\n", tt.field, tt.value)
			}

			config := fmt.Sprintf(`
resource %q "test" {
  id = "acc-bad-rule"
  ingress {
%s  }
}
`, securityGroupType, body)

			resource.Test(t, resource.TestCase{
				ProtoV6ProviderFactories: protoV6,
				Steps: []resource.TestStep{{
					Config:      config,
					PlanOnly:    true,
					ExpectError: tt.expect,
				}},
			})
		})
	}
}

// The fake names the organization's scope `tenant`, as the pinned contract
// does, unless the organization id asks for `organization`, the name the
// platform's quota API renames it to. Each data source reports its documented
// name from either.
func TestAccOrganizationQuota(t *testing.T) {
	for platform, org := range map[string]string{
		"tenant":       "org-demo",
		"organization": "org-fake-quota-renamed-scope",
	} {
		t.Run(platform, func(t *testing.T) {
			config := fmt.Sprintf(`
data %[1]q "org" {
  organization_id = %[4]q
}

data %[2]q "org" {
  organization_id = %[4]q
}

data %[3]q "team" {
  organization_id = %[4]q
  project_id      = "team-a"
}
`, quotaType, legacyQuotaType, projectQuotaType, org)

			resource.Test(t, resource.TestCase{
				ProtoV6ProviderFactories: protoV6,
				Steps: []resource.TestStep{{
					Config: config,
					Check: resource.ComposeAggregateTestCheckFunc(
						resource.TestCheckResourceAttr(quotaAddr, "profile", "Standard"),
						resource.TestCheckResourceAttr(quotaAddr, "dimensions.node_count.headroom", "26"),
						resource.TestCheckResourceAttr(quotaAddr, "dimensions.node_count.binding_scope", "organization"),
						resource.TestCheckResourceAttr(quotaAddr, "dimensions.node_count.scopes.0.scope", "organization"),
						// Limited nowhere: usage, and no headroom rather than a zero one.
						resource.TestCheckResourceAttr(quotaAddr, "dimensions.gpu_count.scopes.0.used", "8"),
						resource.TestCheckNoResourceAttr(quotaAddr, "dimensions.gpu_count.headroom"),
						// Not enforced: unmeasured usage is null, never 0.
						resource.TestCheckResourceAttr(quotaAddr, "dimensions.cpu_cores.enforced", "false"),
						resource.TestCheckNoResourceAttr(quotaAddr, "dimensions.cpu_cores.scopes.0.used"),
						resource.TestCheckResourceAttr(quotaAddr, "dimensions.cpu_cores.scopes.0.limit", "512"),
						// The deprecated name reads what it always did.
						resource.TestCheckResourceAttr(legacyQuotaAddr, "dimensions.node_count.headroom", "26"),
						resource.TestCheckResourceAttr(legacyQuotaAddr, "dimensions.node_count.binding_scope", "tenant"),
						resource.TestCheckResourceAttr(legacyQuotaAddr, "dimensions.node_count.scopes.0.scope", "tenant"),
						// A project's own smaller headroom binds.
						resource.TestCheckResourceAttr(projectQuotaAddr, "dimensions.node_count.headroom", "2"),
						resource.TestCheckResourceAttr(projectQuotaAddr, "dimensions.node_count.binding_scope", "project"),
						resource.TestCheckResourceAttr(projectQuotaAddr, "dimensions.node_count.scopes.#", "2"),
						resource.TestCheckResourceAttr(projectQuotaAddr, "dimensions.node_count.scopes.0.scope", "organization"),
					),
				}},
			})
		})
	}
}

func TestAccSecurityGroupClearsMetadata(t *testing.T) {
	const name = "acc-sg-clear"
	full := strings.Replace(securityGroupConfig(name, false), `display_name = "acceptance"`,
		`display_name = "acceptance"
  description  = "temporary"`, 1)
	bare := strings.Replace(strings.Replace(securityGroupConfig(name, false),
		`  display_name = "acceptance"`+"\n", "", 1), `  tags         = { team = "ml" }`+"\n", "", 1)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy:             checkGone(securityGroupType, securityGroupPath),
		Steps: []resource.TestStep{
			{Config: full},
			{
				Config: bare,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckNoResourceAttr(securityGroupAddr, "display_name"),
					resource.TestCheckNoResourceAttr(securityGroupAddr, "description"),
					resource.TestCheckNoResourceAttr(securityGroupAddr, "tags.%"),
				),
			},
			{Config: bare, PlanOnly: true},
		},
	})
}

// The platform stores "22-22" as "22" and reports that.
func TestAccSecurityGroupKeepsPortSpelling(t *testing.T) {
	config := strings.Replace(securityGroupConfig("acc-sg-ports", false),
		`destination_port_range = "22"`, `destination_port_range = "22-22"`, 1)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy:             checkGone(securityGroupType, securityGroupPath),
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.TestCheckResourceAttr(securityGroupAddr,
					"ingress.0.destination_port_range", "22-22"),
			},
			{Config: config, PlanOnly: true},
		},
	})
}

// `terraform validate` evaluates a variable as unknown, so the rule list a
// `dynamic` block builds from one is unknown too. Validation has to wait for it
// rather than fail to decode it, and still catch the bad rule once it is known.
func dynamicIngressConfig(protocol, ports string) string {
	return fmt.Sprintf(`
variable "ingress" {
  type = list(object({ protocol = string, ports = string }))
  default = [{ protocol = %q, ports = %q }]
}

resource %q "test" {
  id = "acc-dynamic"
  dynamic "ingress" {
    for_each = var.ingress
    content {
      action                 = "permit"
      protocol               = ingress.value.protocol
      source_prefix          = "0.0.0.0/0"
      destination_prefix     = "0.0.0.0/0"
      destination_port_range = ingress.value.ports
    }
  }
}
`, protocol, ports, securityGroupType)
}

func TestAccSecurityGroupDynamicRulesFromAVariable(t *testing.T) {
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy:             checkGone(securityGroupType, securityGroupPath),
		Steps: []resource.TestStep{
			{
				Config: dynamicIngressConfig("tcp", "443"),
				Check:  resource.TestCheckResourceAttr(securityGroupAddr, "ingress.0.destination_port_range", "443"),
			},
			{Config: dynamicIngressConfig("tcp", "443"), PlanOnly: true},
		},
	})
}

func TestAccSecurityGroupDynamicRulesStillRejectPortsOnICMP(t *testing.T) {
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		Steps: []resource.TestStep{{
			Config:      dynamicIngressConfig("icmp", "22"),
			PlanOnly:    true,
			ExpectError: regexpPortsOnICMP,
		}},
	})
}
