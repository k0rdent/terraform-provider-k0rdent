package acceptance

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/provider"
)

// Every managed resource's replace and refuse decisions, every way into
// them. The rules the resource docs, the import guide and the known
// limitations state are written once, in wantReplacePlan and in each kind's
// changes, and every combination of how the resource came to be, which
// attribute the configuration changes, and whether it permits a replacement
// is planned through real Terraform and checked against them. Each plan step
// ends at its plan check, so every case plans from the same start.
//
// A row whose rule no doc states is marked "Doc silent": it pins what the
// provider does today, so a change to it is seen, and it's a question, not a
// rule.

// rule is what a change does, as the docs say.
type rule int

const (
	unchanged rule = iota
	// changesInPlace is a change the docs say is made in place.
	changesInPlace
	// replaces is a change to an attribute the doc's "What replaces it"
	// paragraph lists.
	replaces
	// refused is a refusal of its own, not the guard's, which allow_replace
	// doesn't lift: a file store's shrink, a cluster's earlier version, a
	// region its project isn't in.
	refused
)

// invChange is one row: what the configuration changes from the start.
type invChange struct {
	label string
	// attr is the attribute the guard names for a replacing change.
	attr string
	rule rule
	// refusal matches a refused change's error.
	refusal *regexp.Regexp
	// edit is the change, made on a copy of the start's block.
	edit func(b block) block
	// onCreate says a refusal of its own holds for a create too, so for the
	// plan that replaces a tainted resource. The others are refusals of an
	// update only.
	onCreate bool
	// whenFailed is this change's refusal on a cluster the platform reports
	// `failed`, where it isn't refused otherwise.
	whenFailed *regexp.Regexp
	// silent says which part of this row's rule no doc states.
	silent string
}

// block is one resource block's lines, by attribute name, written in name
// order. A line is the whole attribute or nested block.
type block map[string]string

func (b block) with(name, line string) block {
	out := make(block, len(b)+1)
	for k, v := range b {
		out[k] = v
	}
	if line == "" {
		delete(out, name)
	} else {
		out[name] = line
	}
	return out
}

func (b block) render(typ, address string) string {
	names := make([]string, 0, len(b))
	for k := range b {
		names = append(names, k)
	}
	sort.Strings(names)
	var s strings.Builder
	fmt.Fprintf(&s, "\nresource %q %q {\n", typ, address)
	for _, k := range names {
		fmt.Fprintf(&s, "  %s\n", b[k])
	}
	s.WriteString("}\n")
	return s.String()
}

// set is a change that writes one line.
func set(name, line string) func(block) block {
	return func(b block) block { return b.with(name, line) }
}

// invStart is how the resource under test came to be.
type invStart int

const (
	madeByApply invStart = iota
	imported
	// failedWaits is a cluster the platform reports `failed`, with on_failed
	// left unset.
	failedWaits
	// failedKept is the same with on_failed = "keep".
	failedKept
	// taintedWithToken is a create that stopped waiting: the cluster or group
	// is still `creating` and carries this provider's create token.
	taintedWithToken
	// taintedNoToken is a cluster made outside Terraform, so with no create
	// token, imported, then tainted.
	taintedNoToken
	// turnedOff is a webhook subscription the platform turned off.
	turnedOff
	// turnedOffWithSecret is the same with secret_wo set.
	turnedOffWithSecret
)

func (s invStart) String() string {
	return [...]string{"made by apply", "imported", "failed, on_failed unset", "failed, on_failed keep",
		"tainted, create token", "tainted, no create token", "turned off", "turned off, secret set"}[s]
}

// invKind is one resource type: its doc's list of what replaces it, the
// configuration it starts from, its rows and its starts.
type invKind struct {
	typ, noun string
	// guarded is whether the doc says a replacement needs allow_replace.
	guarded bool
	// replacing is the doc's "What replaces it" list, copied, and checked
	// against the doc file by TestReplacementRulesNameEveryDocumentedAttribute.
	replacing []string
	// prelude is the other resources the block needs, at fixed addresses.
	prelude func(name string) string
	base    func(name string) block
	// importID is the import id, or nil when no start imports.
	importID func(name string) string
	pathOf   func(name string) string
	changes  []invChange
	starts   []invStart
	// replaceRefused is the refusal of a replacement from a start whose
	// configuration can't make the new one.
	replaceRefused map[invStart]*regexp.Regexp
	// unexercised are the doc's replacing attributes no row replaces, and why.
	unexercised map[string]string
	// ownProject puts each start in a project of its own, which ownProject
	// makes. A binding lists every security group in its project.
	ownProject bool
}

// wantReplacePlan is the rule for one combination: the plans it allows, of
// "no-op", "update", "replace" and "create", or a refusal, with the error
// that says so.
//
//   - Nothing changed plans nothing, but a failed cluster with on_failed
//     unset plans an update that waits for it (on_failed doc).
//   - A change the docs say is made in place is an update.
//   - A change to an attribute "What replaces it" lists is a replacement,
//     refused on a guarded resource unless allow_replace = true, and refused
//     with the attribute's name.
//   - A refusal of its own is refused whatever allow_replace says.
//   - allow_replace appearing is itself an update, in place.
//   - A tainted resource's plan is a replacement whatever changed: the
//     cluster description says the plan warns, not that it's refused. A
//     cluster with no create token is the exception: a create of an id the
//     platform holds for it is refused unless allow_replace = true, and a
//     create that names another id or project finds nothing there.
//   - A subscription the platform turned off is replaced on the next plan
//     with secret_wo set, and only warned about without it (is_active doc).
//     secret_wo is required to create or replace one (secret_wo doc), so
//     without it a replacing change is refused.
func wantReplacePlan(k invKind, start invStart, c invChange, allow bool) ([]string, *regexp.Regexp) {
	tainted := start == taintedWithToken || start == taintedNoToken
	if c.rule == refused && (c.onCreate || !tainted) {
		return []string{"refuse"}, c.refusal
	}
	if c.whenFailed != nil && (start == failedWaits || start == failedKept) {
		return []string{"refuse"}, c.whenFailed
	}
	switch start {
	case taintedWithToken:
		return []string{"replace"}, nil
	case taintedNoToken:
		if allow || c.attr == "id" || c.attr == "project" {
			return []string{"replace"}, nil
		}
		return []string{"refuse"}, takenID(k.noun)
	case turnedOffWithSecret:
		return []string{"replace"}, nil
	}
	var want string
	switch c.rule {
	case unchanged:
		want = "no-op"
		if start == failedWaits {
			want = "update"
		}
	case changesInPlace:
		want = "update"
	case replaces:
		if k.guarded && !allow {
			return []string{"refuse"}, refusal(c.attr, k.noun)
		}
		if r := k.replaceRefused[start]; r != nil {
			return []string{"refuse"}, r
		}
		want = "replace"
	}
	if allow && want == "no-op" {
		want = "update"
	}
	return []string{want}, nil
}

// takenID matches the create guard's refusal of an id the platform holds
// for an object with no create token.
func takenID(noun string) *regexp.Regexp {
	return regexp.MustCompile(`(?s)(` + aOrAn(noun) + ` named "[^"]+" already exists|The ` + noun +
		` "[^"]+" is still being created).*carries\s+no\s+create\s+token`)
}

func aOrAn(noun string) string {
	if strings.ContainsRune("aeiou", rune(noun[0])) {
		return "An " + noun
	}
	return "A " + noun
}

// plansAs holds the plan for address to want, then ends the step with
// errPlanFollowsRule so nothing applies and every row plans from the same
// start. A refusal is never a plan, so a plan where the rule is a refusal
// fails here, naming the rule and the plan.
type plansAs struct {
	address, what string
	want          []string
}

var errPlanFollowsRule = errors.New("the plan follows the rule")

func (p plansAs) CheckPlan(_ context.Context, req plancheck.CheckPlanRequest, resp *plancheck.CheckPlanResponse) {
	for _, rc := range req.Plan.ResourceChanges {
		if rc.Address != p.address {
			continue
		}
		got := fmt.Sprint(rc.Change.Actions)
		switch {
		case rc.Change.Actions.NoOp():
			got = "no-op"
		case rc.Change.Actions.Replace():
			got = "replace"
		case rc.Change.Actions.Update():
			got = "update"
		case rc.Change.Actions.Create():
			got = "create"
		}
		if !slices.Contains(p.want, got) {
			resp.Error = fmt.Errorf("%s: rule: %s, planned: %s", p.what, strings.Join(p.want, " or "), got)
			return
		}
		resp.Error = errPlanFollowsRule
		return
	}
	resp.Error = fmt.Errorf("%s: %s is not in the plan", p.what, p.address)
}

// planRow is the step that plans one row and stops.
func planRow(config, address, what string, want []string, refusal *regexp.Regexp) resource.TestStep {
	step := resource.TestStep{
		Config:           config,
		ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plansAs{address: address, what: what, want: want}}},
		ExpectError:      regexp.MustCompile(regexp.QuoteMeta(errPlanFollowsRule.Error())),
	}
	if refusal != nil {
		// Only the refusal: a plan here fails in plansAs with what it was.
		step.ExpectError = refusal
	}
	return step
}

// noRefusal is what a destroy that has to pass expects: nothing, so any
// error fails the step.
var noRefusal *regexp.Regexp

func TestReplacementsFollowTheDocumentedRules(t *testing.T) {
	t.Parallel()
	for _, k := range invKinds() {
		t.Run(k.typ, func(t *testing.T) {
			t.Parallel()
			for i, start := range k.starts {
				t.Run(start.String(), func(t *testing.T) {
					t.Parallel()
					name := fmt.Sprintf("acc-inv-%s-%d", shortKind(k.typ), i)
					steps, address, base, before := k.startSteps(t, start, name)
					addr := k.typ + "." + address
					allows := []bool{false}
					if k.guarded {
						allows = append(allows, true)
					}
					render := func(b block) string { return k.prelude(name) + b.render(k.typ, address) }
					first := len(steps)
					for _, allow := range allows {
						for _, c := range k.changes {
							b := c.edit(base)
							if allow {
								b = b.with("allow_replace", "allow_replace = true")
							}
							want, refusal := wantReplacePlan(k, start, c, allow)
							what := fmt.Sprintf("%s, %s, allow_replace %v", start, c.label, allow)
							if c.silent != "" {
								what += " (doc silent, pinned: " + c.silent + ")"
							}
							step := planRow(render(b), addr, what, want, refusal)
							// A refusal where the rule is a plan never reaches the plan
							// check, so the row is logged before it plans.
							n, rule := len(steps)+1, strings.Join(want, " or ")
							step.PreConfig = func() { t.Logf("step %d: %s, rule: %s", n, what, rule) }
							steps = append(steps, step)
						}
					}
					logRow := steps[first].PreConfig
					steps[first].PreConfig = func() { before(); logRow() }
					if start == taintedNoToken {
						steps[first].Taint = []string{addr}
					}
					for _, d := range k.destroySteps(start, base, render) {
						n, what := len(steps)+1, d.what
						d.step.PreConfig = func() { t.Logf("step %d: terraform destroy, %s, %s", n, start, what) }
						steps = append(steps, d.step)
					}
					if k.ownProject {
						steps = stepsInProject(ownProject(t, name+"-project"), steps)
					}
					resource.Test(t, resource.TestCase{
						ProtoV6ProviderFactories: protoV6,
						CheckDestroy:             checkGoneAt(k.pathOf(name)),
						Steps:                    steps,
					})
				})
			}
		})
	}
}

// destroySteps are `terraform destroy` right after each refusal, then with
// the start's own configuration, each through a Destroy step: a plan with
// -destroy and a refresh, which runs the normal plan `terraform destroy`
// runs first. terraform-plugin-testing's own destroy at the end passes
// -refresh=false, so it never runs that plan (mistakes log, Class 1).
//
//   - A configuration whose change the guard refuses refuses `terraform
//     destroy` too, since destroy runs that plan first: "A refused plan stops
//     `terraform destroy` too" in docs/known-limitations.md, and the guard's
//     error text, which names -refresh=false. The test's own destroy, with
//     -refresh=false, then has to remove it.
//   - With the start's configuration the destroy passes, from every start
//     (Class 1), except a tainted cluster with no create token, which the
//     cluster description says refuses destroy until it's untainted.
func (k invKind) destroySteps(start invStart, base block, render func(block) string) []destroyRow {
	var rows []destroyRow
	for _, c := range k.changes {
		want, refusal := wantReplacePlan(k, start, c, false)
		if want[0] != "refuse" || c.rule == unchanged {
			continue
		}
		rows = append(rows, destroyRow{what: c.label + " changed, rule: refuse",
			step: resource.TestStep{Config: render(c.edit(base)), Destroy: true, ExpectError: refusal}})
	}
	final := destroyRow{what: "nothing changed, rule: destroyed",
		step: resource.TestStep{Config: render(base), Destroy: true, ExpectError: noRefusal}}
	if start == taintedNoToken {
		final.what = "nothing changed, rule: refuse until untainted"
		final.step.ExpectError = takenID(k.noun)
	}
	return append(rows, final)
}

type destroyRow struct {
	what string
	step resource.TestStep
}

// startSteps are the steps that take the resource to start, the address it
// has there, its block once it's there, and what has to run before the
// first row plans.
func (k invKind) startSteps(t *testing.T, start invStart, name string) ([]resource.TestStep, string, block, func()) {
	t.Helper()
	base := k.base(name)
	made := func(b block) string { return k.prelude(name) + b.render(k.typ, "test") }
	adopt := func(b block) resource.TestStep {
		return resource.TestStep{
			Config: k.prelude(name) + fmt.Sprintf(`
removed {
  from = %s.test
  lifecycle { destroy = false }
}
import {
  to = %s.adopted
  id = %q
}
`, k.typ, k.typ, k.importID(name)) + b.render(k.typ, "adopted"),
			// The import guide: what only the configuration knows plans an
			// in-place update after an import, and none of it is a
			// replacement.
			ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
				notReplaced{address: k.typ + ".adopted"},
			}},
		}
	}
	nothingFirst := func() {}
	switch start {
	case madeByApply:
		return []resource.TestStep{{Config: made(base)}}, "test", base, nothingFirst
	case imported:
		return []resource.TestStep{{Config: made(base)}, adopt(base)}, "adopted", base, nothingFirst
	case failedWaits, failedKept:
		base = base.with("display_name", fmt.Sprintf("display_name = %q", createFails))
		steps := []resource.TestStep{k.importOutside(t, name, base, "failed")}
		if start == failedKept {
			// The import records on_failed as null, so an apply records
			// keep, sending nothing (TestAccAFailedClusterKeptPlansNothing).
			base = base.with("on_failed", `on_failed = "keep"`)
			steps = append(steps, resource.TestStep{Config: made(base)})
		}
		return steps, "test", base, nothingFirst
	case taintedWithToken:
		marker := neverSettles
		if k.typ == instanceGroupType {
			marker = createsForever
		}
		base = base.with("display_name", fmt.Sprintf("display_name = %q", marker)).
			with("timeouts", `timeouts { create = "1s" }`)
		return []resource.TestStep{{Config: made(base), ExpectError: regexp.MustCompile(`is\s+still\s+being\s+created`)}},
			"test", base, nothingFirst
	case taintedNoToken:
		// Made outside Terraform, so it carries no create token, and
		// imported. The first row taints it.
		return []resource.TestStep{k.importOutside(t, name, base, "active")}, "test", base, nothingFirst
	case turnedOff, turnedOffWithSecret:
		// The platform turns it off once it's made, before the first row.
		var id string
		steps := []resource.TestStep{{Config: made(base), Check: func(s *terraform.State) error {
			id = s.RootModule().Resources[k.typ+".test"].Primary.ID
			return nil
		}}}
		if start == turnedOff {
			// Letting go of the secret sends nothing.
			base = base.with("secret_wo", "").with("secret_wo_version", "")
			steps = append(steps, resource.TestStep{Config: made(base)})
		}
		return steps, "test", base, func() {
			if err := disableWebhookOutOfBand(id); err != nil {
				t.Fatal(err)
			}
		}
	}
	t.Fatalf("no steps for %s", start)
	return nil, "", nil, nil
}

// importOutside is a `terraform import` of a cluster made past the
// provider, which reads want before the import runs.
func (k invKind) importOutside(t *testing.T, name string, base block, want string) resource.TestStep {
	display := "inv"
	if want == "failed" {
		display = createFails
	}
	return resource.TestStep{
		PreConfig:          func() { clusterOutside(t, name, display, want) },
		Config:             k.prelude(name) + base.render(k.typ, "test"),
		ResourceName:       k.typ + ".test",
		ImportState:        true,
		ImportStateId:      k.importID(name),
		ImportStatePersist: true,
	}
}

// notReplaced fails a plan that replaces address.
type notReplaced struct{ address string }

func (n notReplaced) CheckPlan(_ context.Context, req plancheck.CheckPlanRequest, resp *plancheck.CheckPlanResponse) {
	for _, rc := range req.Plan.ResourceChanges {
		if rc.Address == n.address && (rc.Change.Actions.Replace() || rc.Change.Actions.Delete()) {
			resp.Error = fmt.Errorf("the import plans %s for %s", rc.Change.Actions, n.address)
		}
	}
}

// clusterOutside creates a cluster past the provider, so with no create
// token, pinned to version 2 with the pools the base names, and waits for it
// to read want.
func clusterOutside(t *testing.T, name, display, want string) {
	t.Helper()
	if err := postFake("/v1/regions/us-texas-1/projects/demo/compute/clusters", fmt.Sprintf(
		`{"id":%q,"clusterType":%q,"displayName":%q,"nodePools":[{"id":"gpu-workers","nodeCount":2}]}`,
		name, clusterTypeV2, display)); err != nil {
		t.Fatal(err)
	}
	awaitFakeState(t, clusterAPIPath(name), want)
}

// checkGoneAt checks the fake no longer holds path, past the provider.
func checkGoneAt(path string) resource.TestCheckFunc {
	return func(*terraform.State) error {
		if path == "" {
			return nil
		}
		status, _, err := readFakeStore(path)
		if err != nil {
			return err
		}
		if status != http.StatusNotFound {
			return fmt.Errorf("the fake still holds %s: it answered %d, wanted 404", path, status)
		}
		return nil
	}
}

func shortKind(typ string) string {
	return strings.NewReplacer(provider.ResourcePrefix(), "", "kubernetes_", "", "_", "").Replace(typ)
}

// The rows name every attribute the docs say replaces each resource, and the
// lists the rows are written from are the docs' own. The "What replaces it"
// paragraph is generated from the schema, so a RequiresReplace added without
// a row fails here, and one removed fails the rows that plan it. The
// paragraph names the attributes any change to replaces, then those only
// some changes to do, and the list here is both.
func TestReplacementRulesNameEveryDocumentedAttribute(t *testing.T) {
	t.Parallel()
	paragraph := regexp.MustCompile(`\*\*What replaces it\.\*\*( Changing (.*?) cannot be done in place[^.]*\.)?( Some changes to (.*?) replace )?`)
	quoted := regexp.MustCompile("`([a-z_]+)`")
	for _, k := range invKinds() {
		t.Run(k.typ, func(t *testing.T) {
			doc, err := os.ReadFile(filepath.Join("..", "..", "docs", "resources", strings.TrimPrefix(k.typ, provider.ResourcePrefix())+".md"))
			if err != nil {
				t.Fatal(err)
			}
			m := paragraph.FindSubmatch(doc)
			if m == nil || len(m[2])+len(m[4]) == 0 {
				t.Fatal("the doc has no \"What replaces it\" paragraph")
			}
			var documented []string
			for _, a := range quoted.FindAllSubmatch(append(append([]byte{}, m[2]...), m[4]...), -1) {
				documented = append(documented, string(a[1]))
			}
			slices.Sort(documented)
			if !slices.Equal(documented, k.replacing) {
				t.Errorf("the doc says %v replace the %s, the rows are written from %v", documented, k.noun, k.replacing)
			}
			for _, a := range k.replacing {
				rows := 0
				for _, c := range k.changes {
					if c.attr == a && c.rule == replaces {
						rows++
					}
				}
				_, skipped := k.unexercised[a]
				switch {
				case rows == 0 && !skipped:
					t.Errorf("no row replaces the %s by changing %s", k.noun, a)
				case rows > 0 && skipped:
					t.Errorf("%s has rows and is listed as unexercised", a)
				}
			}
		})
	}
}
