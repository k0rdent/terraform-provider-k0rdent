// Command unknownbudget holds plans to a budget of "known after apply"
// values, so the values the provider makes known at plan stay known.
//
// ci/unknowns-against-fake.sh plans each directory in examples/walkthrough
// against the fake and saves `terraform show -json` of each plan as
// <scenario>.json in one directory. This reads them, counts the unknown leaf
// values in every resource change's after_unknown, and compares the counts
// with ci/unknown-budget.json, which holds, per scenario and resource type,
// the count and each unknown attribute path with the reason it can't be known.
//
// It fails, naming the address and attribute, when a count rises or a path
// the budget doesn't list goes unknown. It fails too when a count falls, or a
// listed path is no longer unknown, so the budget stays the measurement and
// a value that comes back unknown later is caught. -write rewrites the counts
// and paths from the plans and keeps every reason already written. A new path
// gets an empty reason, and the check refuses one, so somebody says why.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// Budget is ci/unknown-budget.json.
type Budget struct {
	About     []string             `json:"about"`
	Scenarios map[string]*Scenario `json:"scenarios"`
}

// Scenario is one plan of one walkthrough directory.
type Scenario struct {
	// Plan says which plan this is: the directory and what was changed.
	Plan string `json:"plan"`
	// Changed is the one address the plan changes, for a change plan. Every
	// unknown outside it is called out, since a change to one resource
	// shouldn't leave another unsure.
	Changed   string               `json:"changed,omitempty"`
	Resources map[string]*Resource `json:"resources"`
}

// Resource is one resource type's unknowns in one scenario.
type Resource struct {
	// Count is the number of unknown leaf values across every address of
	// the type.
	Count int `json:"count"`
	// Unknown maps each unknown attribute path, with list indexes as [*], to
	// why it can't be known at plan.
	Unknown map[string]string `json:"unknown"`
}

// leaf is one unknown value in a plan.
type leaf struct {
	addr string // the resource address, such as <type>.this
	path string // subnets["nodes"].id, as the plan has it
	norm string // the path with list indexes as [*]
}

// plan is the part of `terraform show -json` this reads.
type plan struct {
	ResourceChanges []struct {
		Address string `json:"address"`
		Mode    string `json:"mode"`
		Type    string `json:"type"`
		Change  struct {
			Actions      []string `json:"actions"`
			AfterUnknown any      `json:"after_unknown"`
		} `json:"change"`
	} `json:"resource_changes"`
}

// typeName is how the budget names a resource type, with data sources marked.
func typeName(mode, typ string) string {
	if mode == "data" {
		return "data." + typ
	}
	return typ
}

// unknowns reads one plan and returns every unknown leaf, by type.
func unknowns(r io.Reader) (map[string][]leaf, error) {
	var p plan
	dec := json.NewDecoder(r)
	dec.UseNumber()
	if err := dec.Decode(&p); err != nil {
		return nil, err
	}
	out := map[string][]leaf{}
	for _, rc := range p.ResourceChanges {
		if slices.Equal(rc.Change.Actions, []string{"no-op"}) || slices.Equal(rc.Change.Actions, []string{"delete"}) {
			continue
		}
		t := typeName(rc.Mode, rc.Type)
		walk(rc.Change.AfterUnknown, "", "", func(path, norm string) {
			out[t] = append(out[t], leaf{addr: rc.Address, path: path, norm: norm})
		})
		if _, ok := out[t]; !ok {
			out[t] = nil
		}
	}
	return out, nil
}

// walk calls found for every true in an after_unknown structure. A whole
// block or collection that's unknown is one true, and one leaf.
func walk(v any, path, norm string, found func(path, norm string)) {
	switch v := v.(type) {
	case bool:
		if v {
			found(strings.TrimPrefix(path, "."), strings.TrimPrefix(norm, "."))
		}
	case map[string]any:
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, k := range keys {
			walk(v[k], path+"."+k, norm+"."+k, found)
		}
	case []any:
		for i, e := range v {
			walk(e, path+"["+strconv.Itoa(i)+"]", norm+"[*]", found)
		}
	}
}

// measure turns one plan's unknowns into the budget's form, reasons empty.
func measure(found map[string][]leaf) map[string]*Resource {
	out := map[string]*Resource{}
	for t, leaves := range found {
		r := &Resource{Count: len(leaves), Unknown: map[string]string{}}
		for _, l := range leaves {
			r.Unknown[l.norm] = ""
		}
		out[t] = r
	}
	return out
}

// findings is what compare has to say, a line each, every line naming the
// scenario and the type and, where one is unknown, the address and attribute.
type findings struct {
	// over and under fail the check, and so does unexplained: a path the
	// budget lists with no reason.
	over, under, unexplained []string
	// outside is every unknown a change plan has outside the one resource it
	// changes. The budget can list them, with a reason, and they're printed
	// on every run so nobody takes them for normal.
	outside []string
}

func (f *findings) add(g findings) {
	f.over = append(f.over, g.over...)
	f.under = append(f.under, g.under...)
	f.unexplained = append(f.unexplained, g.unexplained...)
	f.outside = append(f.outside, g.outside...)
}

// compare holds one scenario's unknowns to its budget.
func compare(name string, want *Scenario, found map[string][]leaf) (f findings) {
	types := map[string]bool{}
	for t := range want.Resources {
		types[t] = true
	}
	for t := range found {
		types[t] = true
	}
	order := make([]string, 0, len(types))
	for t := range types {
		order = append(order, t)
	}
	slices.Sort(order)

	for _, t := range order {
		budget := want.Resources[t]
		if budget == nil {
			budget = &Resource{}
		}
		leaves := found[t]
		seen := map[string]bool{}
		for _, l := range leaves {
			seen[l.norm] = true
			if _, listed := budget.Unknown[l.norm]; !listed {
				f.over = append(f.over, fmt.Sprintf("%s: %s.%s is known after apply, and the budget doesn't list %s %s",
					name, l.addr, l.path, t, l.norm))
			}
		}
		if len(leaves) > budget.Count {
			var at []string
			for _, l := range leaves {
				at = append(at, l.addr+"."+l.path)
			}
			f.over = append(f.over, fmt.Sprintf("%s: %s has %d values known after apply, budget %d:\n      %s",
				name, t, len(leaves), budget.Count, strings.Join(at, "\n      ")))
		}
		if len(leaves) < budget.Count {
			f.under = append(f.under, fmt.Sprintf("%s: %s has %d values known after apply, budget %d. Lower the budget",
				name, t, len(leaves), budget.Count))
		}
		paths := make([]string, 0, len(budget.Unknown))
		for p := range budget.Unknown {
			paths = append(paths, p)
		}
		slices.Sort(paths)
		for _, p := range paths {
			if !seen[p] {
				f.under = append(f.under, fmt.Sprintf("%s: %s %s is known at plan now. Take it out of the budget", name, t, p))
			}
			if strings.TrimSpace(budget.Unknown[p]) == "" {
				f.unexplained = append(f.unexplained, fmt.Sprintf("%s: %s %s has no reason. Say why it can't be known at plan", name, t, p))
			}
		}
		if want.Changed != "" {
			// One line per address, since a deferred data source is unknown
			// in every attribute.
			var addrs []string
			byAddr := map[string][]string{}
			for _, l := range leaves {
				if l.addr == want.Changed {
					continue
				}
				if _, ok := byAddr[l.addr]; !ok {
					addrs = append(addrs, l.addr)
				}
				byAddr[l.addr] = append(byAddr[l.addr], l.path)
			}
			for _, a := range addrs {
				f.outside = append(f.outside, fmt.Sprintf("%s: %s, %d known after apply outside %s: %s",
					name, a, len(byAddr[a]), want.Changed, strings.Join(byAddr[a], ", ")))
			}
		}
	}
	return f
}

func run(budgetPath, plansDir string, write bool, stdout io.Writer) error {
	raw, err := os.ReadFile(budgetPath)
	if err != nil {
		return err
	}
	var b Budget
	if err = json.Unmarshal(raw, &b); err != nil {
		return fmt.Errorf("%s: %w", budgetPath, err)
	}

	planned, err := filepath.Glob(filepath.Join(plansDir, "*.json"))
	if err != nil {
		return err
	}
	measured := map[string]map[string][]leaf{}
	for _, p := range planned {
		f, openErr := os.Open(p)
		if openErr != nil {
			return openErr
		}
		found, readErr := unknowns(f)
		if closeErr := f.Close(); closeErr != nil && readErr == nil {
			readErr = closeErr
		}
		if readErr != nil {
			return fmt.Errorf("%s: %w", p, readErr)
		}
		measured[strings.TrimSuffix(filepath.Base(p), ".json")] = found
	}

	var all findings
	names := make([]string, 0, len(b.Scenarios))
	for name := range b.Scenarios {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		if _, ok := measured[name]; !ok {
			all.over = append(all.over, fmt.Sprintf("%s: the budget has it and no plan was made for it", name))
		}
	}
	for name := range measured {
		if _, ok := b.Scenarios[name]; !ok {
			if !write {
				all.over = append(all.over, fmt.Sprintf("%s: a plan was made for it and the budget doesn't have it", name))
				continue
			}
			b.Scenarios[name] = &Scenario{Plan: "say which plan this is"}
		}
	}

	if write {
		for name, found := range measured {
			s := b.Scenarios[name]
			fresh := measure(found)
			for t, r := range fresh {
				if old := s.Resources[t]; old != nil {
					for p := range r.Unknown {
						r.Unknown[p] = old.Unknown[p]
					}
				}
			}
			s.Resources = fresh
		}
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetIndent("", "  ")
		enc.SetEscapeHTML(false)
		if err = enc.Encode(b); err != nil {
			return err
		}
		if err = os.WriteFile(budgetPath, buf.Bytes(), 0o600); err != nil {
			return err
		}
		_, err = fmt.Fprintf(stdout, "wrote %s. Give every new path a reason before committing it.\n", budgetPath)
		return err
	}

	for _, name := range names {
		if found, ok := measured[name]; ok {
			all.add(compare(name, b.Scenarios[name], found))
		}
	}
	for _, name := range names {
		if found, ok := measured[name]; ok {
			if _, err = fmt.Fprintf(stdout, "%-28s %s\n", name, summary(found)); err != nil {
				return err
			}
		}
	}
	if len(all.outside) > 0 {
		if _, err = fmt.Fprintf(stdout, "unknown outside the resource a change plan changes, each listed in the budget with why:\n  %s\n",
			strings.Join(all.outside, "\n  ")); err != nil {
			return err
		}
	}
	if len(all.over)+len(all.under)+len(all.unexplained) == 0 {
		_, err = fmt.Fprintln(stdout, "every plan is within the budget in "+budgetPath)
		return err
	}
	var msg []string
	if len(all.over) > 0 {
		msg = append(msg, "over the budget:\n  "+strings.Join(all.over, "\n  "))
	}
	if len(all.under) > 0 {
		msg = append(msg, "under the budget, so lower it with `make unknown-budget-update`:\n  "+strings.Join(all.under, "\n  "))
	}
	if len(all.unexplained) > 0 {
		msg = append(msg, "in the budget without a reason, in "+budgetPath+":\n  "+strings.Join(all.unexplained, "\n  "))
	}
	return errors.New(strings.Join(msg, "\n"))
}

// summary is one plan's counts, by type, on one line.
func summary(found map[string][]leaf) string {
	types := make([]string, 0, len(found))
	total := 0
	for t, leaves := range found {
		types = append(types, fmt.Sprintf("%s %d", t, len(leaves)))
		total += len(leaves)
	}
	slices.Sort(types)
	return fmt.Sprintf("%d unknown: %s", total, strings.Join(types, ", "))
}

func main() {
	budget := flag.String("budget", "ci/unknown-budget.json", "the budget file")
	plans := flag.String("plans", "", "the directory of <scenario>.json plans, from terraform show -json")
	write := flag.Bool("write", false, "rewrite the budget's counts and paths from the plans, keeping reasons")
	flag.Parse()
	if *plans == "" {
		fmt.Fprintln(os.Stderr, "unknownbudget: -plans is required")
		os.Exit(2)
	}
	if err := run(*budget, *plans, *write, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "unknownbudget:", err)
		os.Exit(1)
	}
}
