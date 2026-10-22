package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// planJSON is a cut-down `terraform show -json`: a create with two unknowns,
// one of them inside a list, a data source read during the apply, a no-op
// and a delete, which count for nothing.
const planJSON = `{"resource_changes": [
  {"address": "x_net.a", "mode": "managed", "type": "x_net",
   "change": {"actions": ["create"], "after_unknown": {"id": true, "name": false, "subnets": [{"cidr": true}, {"cidr": false}]}}},
  {"address": "data.x_vpc.b", "mode": "data", "type": "x_vpc",
   "change": {"actions": ["read"], "after_unknown": {"self_link": true}}},
  {"address": "x_net.same", "mode": "managed", "type": "x_net",
   "change": {"actions": ["no-op"], "after_unknown": {"id": true}}},
  {"address": "x_net.gone", "mode": "managed", "type": "x_net",
   "change": {"actions": ["delete"], "after_unknown": {"id": true}}}
]}`

func budgetFor(t *testing.T, resources map[string]*Resource, changed string) *Scenario {
	t.Helper()
	return &Scenario{Plan: "test", Changed: changed, Resources: resources}
}

func measured(t *testing.T) map[string][]leaf {
	t.Helper()
	found, err := unknowns(strings.NewReader(planJSON))
	if err != nil {
		t.Fatal(err)
	}
	return found
}

func TestUnknownsCountsLeavesOfChangesOnly(t *testing.T) {
	found := measured(t)
	if got := len(found["x_net"]); got != 2 {
		t.Errorf("x_net: %d unknowns, want 2 (the no-op and the delete count for nothing): %v", got, found["x_net"])
	}
	if got := len(found["data.x_vpc"]); got != 1 {
		t.Errorf("data.x_vpc: %d unknowns, want 1", got)
	}
	var paths []string
	for _, l := range found["x_net"] {
		paths = append(paths, l.addr+" "+l.path+" "+l.norm)
	}
	want := "x_net.a id id,x_net.a subnets[0].cidr subnets[*].cidr"
	if strings.Join(paths, ",") != want {
		t.Errorf("leaves %q, want %q", strings.Join(paths, ","), want)
	}
}

func exact() map[string]*Resource {
	return map[string]*Resource{
		"x_net":      {Count: 2, Unknown: map[string]string{"id": "assigned", "subnets[*].cidr": "allocated"}},
		"data.x_vpc": {Count: 1, Unknown: map[string]string{"self_link": "read during the apply"}},
	}
}

func TestCompareWithinBudget(t *testing.T) {
	f := compare("s", budgetFor(t, exact(), ""), measured(t))
	if len(f.over)+len(f.under)+len(f.unexplained)+len(f.outside) != 0 {
		t.Errorf("an exact budget found %+v", f)
	}
}

func TestCompareNamesTheAddressAndAttributeOverBudget(t *testing.T) {
	r := exact()
	r["x_net"] = &Resource{Count: 1, Unknown: map[string]string{"id": "assigned"}}
	f := compare("s", budgetFor(t, r, ""), measured(t))
	all := strings.Join(f.over, "\n")
	for _, want := range []string{
		"s: x_net.a.subnets[0].cidr is known after apply, and the budget doesn't list x_net subnets[*].cidr",
		"s: x_net has 2 values known after apply, budget 1",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("over budget says\n%s\nwant it to say %q", all, want)
		}
	}
}

func TestCompareSaysToLowerTheBudget(t *testing.T) {
	r := exact()
	r["x_net"] = &Resource{Count: 3, Unknown: map[string]string{"id": "a", "subnets[*].cidr": "b", "name": "c"}}
	f := compare("s", budgetFor(t, r, ""), measured(t))
	all := strings.Join(f.under, "\n")
	for _, want := range []string{
		"s: x_net has 2 values known after apply, budget 3. Lower the budget",
		"s: x_net name is known at plan now. Take it out of the budget",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("under budget says\n%s\nwant it to say %q", all, want)
		}
	}
	if len(f.over) != 0 {
		t.Errorf("under budget also found over: %v", f.over)
	}
}

func TestCompareRefusesAPathWithNoReason(t *testing.T) {
	r := exact()
	r["x_net"].Unknown["id"] = " "
	f := compare("s", budgetFor(t, r, ""), measured(t))
	if len(f.unexplained) != 1 || !strings.Contains(f.unexplained[0], "x_net id has no reason") {
		t.Errorf("unexplained %v", f.unexplained)
	}
}

func TestCompareCallsOutUnknownsOutsideTheChange(t *testing.T) {
	f := compare("s", budgetFor(t, exact(), "x_net.a"), measured(t))
	if len(f.outside) != 1 || !strings.Contains(f.outside[0], "s: data.x_vpc.b, 1 known after apply outside x_net.a: self_link") {
		t.Errorf("outside %v", f.outside)
	}
	if len(f.over) != 0 {
		t.Errorf("a budgeted unknown outside the change failed the check: %v", f.over)
	}
}

func writeFixture(t *testing.T, b Budget) (budgetPath, plans string) {
	t.Helper()
	dir := t.TempDir()
	plans = filepath.Join(dir, "plans")
	if err := os.Mkdir(plans, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(plans, "s.json"), []byte(planJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	budgetPath = filepath.Join(dir, "budget.json")
	if err = os.WriteFile(budgetPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return budgetPath, plans
}

func TestRunWriteKeepsReasonsAndLeavesNewPathsToExplain(t *testing.T) {
	budgetPath, plans := writeFixture(t, Budget{Scenarios: map[string]*Scenario{
		"s": {Plan: "test", Resources: map[string]*Resource{
			"x_net": {Count: 9, Unknown: map[string]string{"id": "assigned", "gone": "no longer"}},
		}},
	}})
	var out bytes.Buffer
	if err := run(budgetPath, plans, true, &out); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(budgetPath)
	if err != nil {
		t.Fatal(err)
	}
	var b Budget
	if err = json.Unmarshal(raw, &b); err != nil {
		t.Fatal(err)
	}
	got := b.Scenarios["s"].Resources["x_net"]
	if got.Count != 2 || got.Unknown["id"] != "assigned" || len(got.Unknown) != 2 {
		t.Errorf("rewritten x_net %+v", got)
	}
	if reason, ok := got.Unknown["subnets[*].cidr"]; !ok || reason != "" {
		t.Errorf("a new path should be written with no reason, got %q, %v", reason, ok)
	}
	// The rewritten budget still fails until the new paths are explained.
	if err = run(budgetPath, plans, false, &out); err == nil || !strings.Contains(err.Error(), "has no reason") {
		t.Errorf("a budget with an unexplained path passed: %v", err)
	}
}

func TestRunFailsOnAScenarioWithNoPlan(t *testing.T) {
	budgetPath, plans := writeFixture(t, Budget{Scenarios: map[string]*Scenario{
		"s":     {Plan: "test", Resources: exact()},
		"other": {Plan: "test"},
	}})
	var out bytes.Buffer
	err := run(budgetPath, plans, false, &out)
	if err == nil || !strings.Contains(err.Error(), "other: the budget has it and no plan was made for it") {
		t.Errorf("run said %v", err)
	}
}
