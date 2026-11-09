package acceptance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

// The SSH key plan, every way into it. The rules the import guide and the
// resource docs state for keys are written once, in wantPlan, and every
// combination of how the resource came to be, which keys the configuration
// names and how, and whether it permits a replacement is planned through real
// Terraform and checked against them. A case picked by hand can miss a
// combination; this can't, and a change to the provider can't pass by
// weakening a rule, since the rules aren't written next to the code they
// check.

// keyStart is how the resource under test came to be.
type keyStart string

const (
	madeWithWriteOnly keyStart = "made with write-only keys"
	madeWithText      keyStart = "made with keys in ssh_public_keys"
	importedChecked   keyStart = "imported, fingerprints readable"
	importedUnchecked keyStart = "imported, fingerprint reads refused"
)

// keyConfig is what the configuration names for ops's keys.
type keyConfig struct {
	layout string // "", "text" or "write-only"
	key    string // "held", "other" or "unknown"
}

func (c keyConfig) String() string {
	if c.layout == "" {
		return "keys left out"
	}
	return c.layout + " " + c.key
}

var keyConfigs = []keyConfig{
	{},
	{"text", "held"}, {"text", "other"}, {"text", "unknown"},
	{"write-only", "held"}, {"write-only", "other"}, {"write-only", "unknown"},
}

// wantPlan is the rule for one combination: the plans it allows, of
// "no-op", "update", "replace", and "refuse" for the guard's refusal at plan.
//
//   - Keys left out keep what state holds: no change.
//   - The key the platform holds plans no change, whichever attribute names
//     it. Named in ssh_public_keys for the first time, state records it: an
//     update that sends nothing.
//   - Another key, or one not known until the apply, is a replacement, since
//     the platform sets keys only when nodes are made. Without allow_replace
//     the guard refuses it.
//   - Keys that can't be checked never plan no change: the plan warns and
//     shows an update, and the apply stops before sending anything
//     (TestAccKeysThatCantBeCheckedStopTheApply), whichever attribute names
//     the keys and whether they're known yet.
//   - allow_replace changing is itself an update, in place.
func wantPlan(start keyStart, c keyConfig, allowReplace bool) []string {
	var want []string
	switch {
	case c.layout == "":
		want = []string{"no-op"}
	case start == importedUnchecked:
		want = []string{"update"}
	case c.key == "held" && c.layout == "text" && start != madeWithText:
		want = []string{"update"}
	case c.key == "held":
		want = []string{"no-op"}
	default:
		want = []string{"refuse"}
	}
	if allowReplace {
		for i, w := range want {
			switch w {
			case "refuse":
				want[i] = "replace"
			case "no-op":
				want[i] = "update"
			}
		}
	}
	return want
}

// invariantKeys are each kind's and each start's own, so refusing one
// credential's reads touches no other case: the fake makes one credential per
// key.
var invariantKeys = map[string]map[keyStart][2]string{
	clusterType: {
		madeWithWriteOnly: {"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAICalTR9icsm8WarWXVQxwy8TRDjP7bQ/eoJdFARMvUlQ inv-c-wo-held", "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAINAlPi0zEcvmv6dncodHzEVk2cbaBM0fvpQTJMRF/16E inv-c-wo-other"},
		madeWithText:      {"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIPzNk0/F5A2fCcHZYZOjOXUTiw0LGC4FP/8FPobtDdGj inv-c-text-held", "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIH9MjzhMa9ba42yi6rjgrwd9H1HcGB/fTHtTPjliQKnL inv-c-text-other"},
		importedChecked:   {"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIO/WsoAYtmplmx2x8DbcCZcTpeiM9+heHms2d6IbApX+ inv-c-imp-held", "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIE+/P+VEJml7xL1i6qnynZAaoG3ZUdVI2pEKZ3KaMgL5 inv-c-imp-other"},
		importedUnchecked: {"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAINVgl1e8YAaeQOu3N9VH4QbAtwHWVMGfrn1UACrtwmYe inv-c-imp403-held", "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFOxktA8cIBvBleQ88SU3Y0N/iRctW/NPvPTxfgdOHRT inv-c-imp403-other"},
	},
	instanceGroupType: {
		madeWithWriteOnly: {"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFvlSsv6yTw641PagD23Uh8LWUWony/+K37b0uCJ+xUy inv-g-wo-held", "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFeNFzXc0+Vmo69gr5lvD2QVqvo19x9zM6VgtctUv+5d inv-g-wo-other"},
		madeWithText:      {"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIO9g6vZHGK/UQMn5T/IYoKU8bS6yU+AW1RafbGy6tFFg inv-g-text-held", "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIPMAwtg1Zu5fFOo8nVf+b2uRHlhiHpydrx1b/UipbO40 inv-g-text-other"},
		importedChecked:   {"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIKTsxK5qZ/AHY0AlQtB2PF+ZizmdcbkdWDl43fxXSwu2 inv-g-imp-held", "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIHpD8nIBPzxKVvNP3PFpM9JYcbHU4J3oJObwa9FtsXxd inv-g-imp-other"},
		importedUnchecked: {"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIEDCwS2JeWWzedZIfSHKIu0veqa6eNWHi0C9FTusPbxU inv-g-imp403-held", "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIKOXjQEsGkLqkmM6iAD+GB1cuWnXa34szZpGnI5xPEUC inv-g-imp403-other"},
	},
}

// invariantConfig is the resource at address with ops's keys as c names
// them. A plan step also has the terraform_data whose output is a key not
// known until the apply: never applied, it's a create every time, so its
// output stays unknown.
func (k keyKind) invariantConfig(address, name string, keys [2]string, c keyConfig, allowReplace, planStep bool) string {
	key := ""
	switch c.key {
	case "held":
		key = fmt.Sprintf("%q", keys[0])
	case "other":
		key = fmt.Sprintf("%q", keys[1])
	case "unknown":
		key = "terraform_data.later.output"
	}
	user, writeOnly, extra := `{ username = "ops" }`, "", ""
	switch c.layout {
	case "text":
		user = fmt.Sprintf(`{ username = "ops", ssh_public_keys = [%s] }`, key)
	case "write-only":
		writeOnly = fmt.Sprintf("\n  ssh_public_keys_wo = { ops = [%s] }", key)
	}
	if allowReplace {
		extra = "\n  allow_replace = true"
	}
	later := ""
	if planStep {
		later = fmt.Sprintf("resource \"terraform_data\" \"later\" {\n  input = %q\n}\n", keys[1])
	}
	return later + fmt.Sprintf(`
resource %q %q {%s
  id           = %q
  cluster_type = %q%s
  ssh_access   = [%s]%s
}
`, k.typ, address, extra, name, clusterTypeRef, k.pool, user, writeOnly)
}

// plannedAs holds the plan for address to want, then ends the step with
// errPlanHeld so nothing applies and every case plans from the same start. A
// refusal is never a plan, so a plan where the rule is a refusal fails here.
type plannedAs struct {
	address, what string
	want          []string
}

var errPlanHeld = errors.New("plan follows the rule")

func (p plannedAs) CheckPlan(_ context.Context, req plancheck.CheckPlanRequest, resp *plancheck.CheckPlanResponse) {
	for _, rc := range req.Plan.ResourceChanges {
		if rc.Address != p.address {
			continue
		}
		got := "update"
		switch {
		case rc.Change.Actions.NoOp():
			got = "no-op"
		case rc.Change.Actions.Replace():
			got = "replace"
		case !rc.Change.Actions.Update():
			got = fmt.Sprint(rc.Change.Actions)
		}
		if !slices.Contains(p.want, got) {
			resp.Error = fmt.Errorf("%s: %s rule: %s, planned: %s", p.what, p.address, strings.Join(p.want, " or "), got)
			return
		}
		resp.Error = errPlanHeld
		return
	}
	resp.Error = fmt.Errorf("%s is not in the plan", p.address)
}

func TestSSHKeyPlansFollowTheDocumentedRules(t *testing.T) {
	t.Parallel()
	eachKeyKind(t, func(t *testing.T, k keyKind) {
		for i, start := range []keyStart{madeWithWriteOnly, madeWithText, importedChecked, importedUnchecked} {
			t.Run(string(start), func(t *testing.T) {
				t.Parallel()
				keys := invariantKeys[k.typ][start]
				name := fmt.Sprintf("%s-inv-%d", k.prefix, i)
				address := "test"
				var steps []resource.TestStep
				switch start {
				case madeWithWriteOnly:
					steps = append(steps, resource.TestStep{Config: k.invariantConfig("test", name, keys, keyConfig{"write-only", "held"}, false, false)})
				case madeWithText:
					steps = append(steps, resource.TestStep{Config: k.invariantConfig("test", name, keys, keyConfig{"text", "held"}, false, false)})
				case importedChecked, importedUnchecked:
					address = "adopted"
					made := resource.TestStep{Config: k.config(name, "test", keys[0])}
					adopt := k.config(name, "adopted", "")
					if start == importedUnchecked {
						made.Check = func(s *terraform.State) error { return k.harness(s, "refuse-reads") }
					}
					steps = append(steps, made, resource.TestStep{
						Config:           adopt,
						ConfigPlanChecks: expect(k.typ+".adopted", plancheck.ResourceActionNoop),
					})
				}
				addr := k.typ + "." + address
				for _, allow := range []bool{false, true} {
					for _, c := range keyConfigs {
						want := wantPlan(start, c, allow)
						what := fmt.Sprintf("%s, %s, allow_replace %v", start, c, allow)
						step := resource.TestStep{
							Config:           k.invariantConfig(address, name, keys, c, allow, true),
							ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plannedAs{address: addr, what: what, want: want}}},
							ExpectError:      regexp.MustCompile(regexp.QuoteMeta(errPlanHeld.Error())),
						}
						if slices.Contains(want, "refuse") {
							step.ExpectError = regexp.MustCompile(`would replace this ` + k.noun + `|` + regexp.QuoteMeta(errPlanHeld.Error()))
						}
						steps = append(steps, step)
					}
				}
				resource.Test(t, resource.TestCase{
					ProtoV6ProviderFactories: protoV6,
					CheckDestroy:             checkGone(k.typ, k.pathOf),
					Steps:                    steps,
				})
			})
		}
	})
}

// TestWriteSSHKeyRules writes wantPlan for every combination to the file
// SSH_KEY_RULES_OUT names, so a run against the real API checks the same
// rules. It needs no fake and does nothing without the variable.
func TestWriteSSHKeyRules(t *testing.T) {
	t.Parallel()
	out := os.Getenv("SSH_KEY_RULES_OUT")
	if out == "" {
		// Not a skip: a full run never sets it, and a skip would leave the
		// testing matrix's record unfinished.
		t.Log("SSH_KEY_RULES_OUT is not set, so there's nothing to write")
		return
	}
	type rule struct {
		Start  keyStart `json:"start"`
		Layout string   `json:"layout"`
		Key    string   `json:"key"`
		Allow  bool     `json:"allow_replace"`
		Want   []string `json:"want"`
	}
	var rules []rule
	for _, start := range []keyStart{madeWithWriteOnly, madeWithText, importedChecked, importedUnchecked} {
		for _, allow := range []bool{false, true} {
			for _, c := range keyConfigs {
				rules = append(rules, rule{start, c.layout, c.key, allow, wantPlan(start, c, allow)})
			}
		}
	}
	b, err := json.MarshalIndent(rules, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(out, b, 0o600); err != nil {
		t.Fatal(err)
	}
}
