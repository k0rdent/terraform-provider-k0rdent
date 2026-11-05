package repocheck

import (
	"slices"
	"strings"
	"testing"
)

// The testing matrix's rules, on a made-up resource, so `make test` proves
// them without the acceptance suite's record.

func rulesAttrs() map[string][]matrixAttr {
	return map[string][]matrixAttr{"example_thing": {
		{Path: "name", Kind: "optional", Needs: map[cell]bool{cellImport: true, cellUpdate: true, cellUpgrade: true}},
		{Path: "id", Kind: "required", Replaces: replacesAlways, Needs: map[cell]bool{cellImport: true, cellUpgrade: true}},
	}}
}

func rulesCoverage() coverage {
	cov := coverage{}
	cov.add(cellKey{"example_thing", "name", cellImport}, "TestAccImport", "")
	cov.add(cellKey{"example_thing", "name", cellUpdate}, "TestAccRename", "")
	cov.add(cellKey{"example_thing", "id", cellImport}, "TestAccImport", "")
	return cov
}

func TestTheMatrixPassesWhenEveryCellIsCoveredGappedOrNew(t *testing.T) {
	gaps := map[cellKey]bool{{"example_thing", "name", cellUpgrade}: true}
	added := map[cellKey]bool{{"example_thing", "id", cellUpgrade}: true}
	if p := matrixProblems(rulesAttrs(), rulesCoverage(), gaps, added, nil, "v0.1.0"); len(p) != 0 {
		t.Fatalf("want no problems, got %q", p)
	}
}

func TestTheMatrixFailsOnACellWithNoTestAndNoGap(t *testing.T) {
	added := map[cellKey]bool{{"example_thing", "id", cellUpgrade}: true}
	p := matrixProblems(rulesAttrs(), rulesCoverage(), map[cellKey]bool{}, added, nil, "v0.1.0")
	if len(p) != 1 || !strings.Contains(p[0], "example_thing name upgrade: no test covers it") ||
		!strings.Contains(p[0], "add the line `example_thing name upgrade`") {
		t.Fatalf("want the missing upgrade cell named, got %q", p)
	}
}

func TestTheMatrixFailsOnAGapATestCovers(t *testing.T) {
	gaps := map[cellKey]bool{
		{"example_thing", "name", cellUpgrade}: true,
		{"example_thing", "name", cellUpdate}:  true,
	}
	added := map[cellKey]bool{{"example_thing", "id", cellUpgrade}: true}
	p := matrixProblems(rulesAttrs(), rulesCoverage(), gaps, added, nil, "v0.1.0")
	if len(p) != 1 || !strings.Contains(p[0], "example_thing name update: listed as a gap, but TestAccRename covers it: drop its line") {
		t.Fatalf("want the covered gap named, got %q", p)
	}
}

func TestTheMatrixFailsOnAGapTheSchemaDoesntHave(t *testing.T) {
	gaps := map[cellKey]bool{
		{"example_thing", "name", cellUpgrade}:  true,
		{"example_thing", "id", cellUpdate}:     true, // always replaces, so no update cell
		{"example_thing", "colour", cellImport}: true,
	}
	added := map[cellKey]bool{{"example_thing", "id", cellUpgrade}: true}
	p := matrixProblems(rulesAttrs(), rulesCoverage(), gaps, added, nil, "v0.1.0")
	if len(p) != 2 || !strings.Contains(p[0], "example_thing colour import: listed as a gap, but the schema has no such cell") ||
		!strings.Contains(p[1], "example_thing id update: listed as a gap, but the schema has no such cell") {
		t.Fatalf("want both cells the schema lacks named, got %q", p)
	}
}

func TestTheMatrixFailsOnAGapForAnUpgradeThePreviousReleaseNeverStored(t *testing.T) {
	gaps := map[cellKey]bool{
		{"example_thing", "name", cellUpgrade}: true,
		{"example_thing", "id", cellUpgrade}:   true,
	}
	added := map[cellKey]bool{{"example_thing", "id", cellUpgrade}: true}
	p := matrixProblems(rulesAttrs(), rulesCoverage(), gaps, added, nil, "v0.1.0")
	if len(p) != 1 || !strings.Contains(p[0], "example_thing id upgrade: listed as a gap, but v0.1.0, the release the matrix upgrades from, never stored it") {
		t.Fatalf("want the gap for a cell v0.1.0 never stored named, got %q", p)
	}
}

func TestTheMatrixFailsOnABinaryThatWasntToldItsTest(t *testing.T) {
	gaps := map[cellKey]bool{{"example_thing", "name", cellUpgrade}: true}
	added := map[cellKey]bool{{"example_thing", "id", cellUpgrade}: true}
	p := matrixProblems(rulesAttrs(), rulesCoverage(), gaps, added, []string{"example_thing"}, "v0.1.0")
	if len(p) != 1 || !strings.Contains(p[0], "recorded example_thing cells without being told its test") {
		t.Fatalf("want the unnamed binary named, got %q", p)
	}
}

func TestTheMatrixSaysWhichChangeASometimesReplacingAttributeSaw(t *testing.T) {
	attrs := map[string][]matrixAttr{"example_thing": {{Path: "ssh_access", Kind: "optional", Replaces: replacesSometimes,
		Needs: map[cell]bool{cellUpdate: true}}}}
	cov := coverage{}
	k := cellKey{"example_thing", "ssh_access", cellUpdate}
	cov.add(k, "TestAccSSHAccess", "replace")
	cov.add(k, "TestAccEmptySSHAccess", "update")
	page := renderMatrix([]string{"example_thing"}, attrs, cov, nil, nil, "v0.1.0")
	if !strings.Contains(page, "| `ssh_access` | optional | sometimes replaces | n/a () | `TestAccEmptySSHAccess` (update), `TestAccSSHAccess` (replace) | n/a () |") {
		t.Fatalf("want the row to say update and replace:\n%s", page)
	}
}

func TestReleaseTagsSortTheWayTheUpgradeJobSortsThem(t *testing.T) {
	tags := []string{"v0.1.0", "v0.1.0-rc.10", "v0.2.0-rc.1", "v0.1.0-rc.2", "v0.1.1"}
	slices.SortFunc(tags, func(a, b string) int {
		switch {
		case releaseLess(a, b):
			return -1
		case releaseLess(b, a):
			return 1
		}
		return 0
	})
	want := []string{"v0.1.0-rc.2", "v0.1.0-rc.10", "v0.1.0", "v0.1.1", "v0.2.0-rc.1"}
	if !slices.Equal(tags, want) {
		t.Fatalf("got %v, want %v", tags, want)
	}
}

func TestStateSchemaSnapshotPathsIncludeNestedAttributes(t *testing.T) {
	typ := []any{"object", map[string]any{
		"name":       "string",
		"node_pool":  []any{"map", []any{"object", map[string]any{"size": "number"}}},
		"ssh_access": []any{"set", []any{"object", map[string]any{"username": "string", "keys": []any{"list", "string"}}}},
		"timeouts":   []any{"object", map[string]any{"create": "string"}},
	}}
	got := map[string]bool{}
	stateTypePaths(typ, "", got)
	var paths []string
	for p := range got {
		paths = append(paths, p)
	}
	slices.Sort(paths)
	want := []string{"name", "node_pool", "node_pool.size", "ssh_access", "ssh_access.keys", "ssh_access.username",
		"timeouts", "timeouts.create"}
	if !slices.Equal(paths, want) {
		t.Fatalf("got %v, want %v", paths, want)
	}
}
