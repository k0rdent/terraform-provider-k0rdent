package repocheck

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The testing matrix's rules, on a made-up resource, so `make test` proves
// them without the acceptance suite's record.

// rulesAttrs needs an upgrade cell of each attribute, which the check never
// gates.
func rulesAttrs() map[string][]matrixAttr {
	return map[string][]matrixAttr{"example_thing": {
		{Path: "name", Kind: "optional", Needs: map[cell]bool{cellImport: true, cellUpdate: true, cellUpgrade: true}},
		{Path: "id", Kind: "required", Replaces: replacesAlways, Needs: map[cell]bool{cellImport: true, cellUpgrade: true}},
	}}
}

func rulesCoverage() coverage {
	cov := coverage{}
	cov.add(cellKey{"example_thing", "name", cellImport}, "TestAccImport", "")
	cov.add(cellKey{"example_thing", "id", cellImport}, "TestAccImport", "")
	return cov
}

func TestTheMatrixPassesWhenEveryCellIsCoveredOrGapped(t *testing.T) {
	gaps := map[cellKey]bool{{"example_thing", "name", cellUpdate}: true}
	if p := matrixProblems(rulesAttrs(), rulesCoverage(), gaps, nil); len(p) != 0 {
		t.Fatalf("want no problems, got %q", p)
	}
}

// Upgrade cells depend on which release came before, so no test and no gap
// line for one is still no problem.
func TestTheMatrixDoesntGateUpgradeCells(t *testing.T) {
	cov := rulesCoverage()
	cov.add(cellKey{"example_thing", "name", cellUpdate}, "TestAccRename", "")
	if p := matrixProblems(rulesAttrs(), cov, map[cellKey]bool{}, nil); len(p) != 0 {
		t.Fatalf("want no problems with two uncovered upgrade cells, got %q", p)
	}
}

func TestTheMatrixFailsOnACellWithNoTestAndNoGap(t *testing.T) {
	p := matrixProblems(rulesAttrs(), rulesCoverage(), map[cellKey]bool{}, nil)
	if len(p) != 1 || !strings.Contains(p[0], "example_thing name update: no test covers it") ||
		!strings.Contains(p[0], "add the line `example_thing name update`") {
		t.Fatalf("want the missing update cell named, got %q", p)
	}
}

func TestTheMatrixFailsOnAGapATestCovers(t *testing.T) {
	gaps := map[cellKey]bool{
		{"example_thing", "name", cellUpdate}: true,
		{"example_thing", "id", cellImport}:   true,
	}
	p := matrixProblems(rulesAttrs(), rulesCoverage(), gaps, nil)
	if len(p) != 1 || !strings.Contains(p[0], "example_thing id import: listed as a gap, but TestAccImport covers it: drop its line") {
		t.Fatalf("want the covered gap named, got %q", p)
	}
}

func TestTheMatrixFailsOnAGapTheSchemaDoesntHave(t *testing.T) {
	gaps := map[cellKey]bool{
		{"example_thing", "name", cellUpdate}:   true,
		{"example_thing", "id", cellUpdate}:     true, // always replaces, so no update cell
		{"example_thing", "colour", cellImport}: true,
	}
	p := matrixProblems(rulesAttrs(), rulesCoverage(), gaps, nil)
	if len(p) != 2 || !strings.Contains(p[0], "example_thing colour import: listed as a gap, but the schema has no such cell") ||
		!strings.Contains(p[1], "example_thing id update: listed as a gap, but the schema has no such cell") {
		t.Fatalf("want both cells the schema lacks named, got %q", p)
	}
}

func TestTheGapListTakesImportAndUpdateLines(t *testing.T) {
	gaps, err := parseGaps("# a comment\n\nexample_thing name import\nexample_thing name update\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(gaps) != 2 || !gaps[cellKey{"example_thing", "name", cellImport}] || !gaps[cellKey{"example_thing", "name", cellUpdate}] {
		t.Fatalf("got %v", gaps)
	}
}

// A gap line for an upgrade would excuse nothing, so it's refused with where
// upgrades are gated.
func TestTheGapListRefusesAnUpgradeLine(t *testing.T) {
	_, err := parseGaps("example_thing name import\nexample_thing name upgrade\n")
	if err == nil || !strings.Contains(err.Error(), "line 2: want `<resource> <attribute path> <import|update>`") ||
		!strings.Contains(err.Error(), "make upgrade-test gates upgrades") {
		t.Fatalf("want the upgrade line refused, got %v", err)
	}
}

func TestTheMatrixFailsOnABinaryThatWasntToldItsTest(t *testing.T) {
	gaps := map[cellKey]bool{{"example_thing", "name", cellUpdate}: true}
	p := matrixProblems(rulesAttrs(), rulesCoverage(), gaps, []string{"example_thing"})
	if len(p) != 1 || !strings.Contains(p[0], "recorded example_thing cells without being told its test") {
		t.Fatalf("want the unnamed binary named, got %q", p)
	}
}

// A run where a test skipped names the tests and says to run them, not to
// regenerate the page from a record that lacks their cells.
func TestARecordWhereTestsSkippedNamesThem(t *testing.T) {
	msg := unfinished("record.jsonl", []string{"TestAccQuery (terraform 1.13.0 has no `terraform query`)"})
	if !strings.Contains(msg, "tests skipped: TestAccQuery (terraform 1.13.0 has no `terraform query`)") ||
		strings.Contains(msg, matrixRegenerate) {
		t.Fatalf("want the skipped test named and no %q, got %q", matrixRegenerate, msg)
	}
}

func TestTheMatrixSaysWhichChangeASometimesReplacingAttributeSaw(t *testing.T) {
	attrs := map[string][]matrixAttr{"example_thing": {{Path: "ssh_access", Kind: "optional", Replaces: replacesSometimes,
		Needs: map[cell]bool{cellUpdate: true}}}}
	cov := coverage{}
	k := cellKey{"example_thing", "ssh_access", cellUpdate}
	cov.add(k, "TestAccSSHAccess", "replace")
	cov.add(k, "TestAccEmptySSHAccess", "update")
	page := renderMatrix([]string{"example_thing"}, attrs, cov, nil)
	if !strings.Contains(page, "\n| `ssh_access` | optional | sometimes replaces | n/a () | `TestAccEmptySSHAccess` (update), `TestAccSSHAccess` (replace) |\n") {
		t.Fatalf("want the row to say update and replace:\n%s", page)
	}
}

// The page has no upgrade column, so a new release tag can't change it.
func TestThePageShowsImportAndUpdateOnly(t *testing.T) {
	cov := rulesCoverage()
	cov.add(cellKey{"example_thing", "name", cellUpgrade}, "TestAccUpgrade", "")
	page := renderMatrix([]string{"example_thing"}, rulesAttrs(), cov, nil)
	if !strings.Contains(page, "| Attribute | Kind | Changes | Import | Update |\n") ||
		!strings.Contains(page, "| `name` | optional | in place | `TestAccImport` | **missing** |\n") ||
		strings.Contains(page, "TestAccUpgrade") || strings.Contains(page, "| Upgrade |") {
		t.Fatalf("want import and update columns only:\n%s", page)
	}
	if !strings.Contains(page, "`make upgrade-test` is what gates them") || !strings.Contains(page, upgradeSummaryFile) {
		t.Fatalf("want the page to say where upgrades are gated and shown:\n%s", page)
	}
}

// summaryRelease stored name and id, not colour.
func summaryRelease() previousRelease {
	return previousRelease{tag: "v0.1.0", shapes: map[string]map[string]bool{"example_thing": {"name": true, "id": true}},
		addedLater: map[string]bool{}}
}

func TestTheUpgradeSummaryCountsCellsThePreviousReleaseStored(t *testing.T) {
	attrs := rulesAttrs()
	attrs["example_thing"] = append(attrs["example_thing"],
		matrixAttr{Path: "colour", Kind: "optional", Needs: map[cell]bool{cellImport: true, cellUpgrade: true}},
		matrixAttr{Path: "secret_wo", Kind: "write-only", Needs: map[cell]bool{cellImport: true}})
	cov := coverage{}
	cov.add(cellKey{"example_thing", "name", cellUpgrade}, "TestAccUpgrade", "")
	text, headline := renderUpgradeSummary(summaryRelease(), []string{"v0.2.0 is tagged but registry.opentofu.org doesn't serve it yet, so it's skipped"},
		[]string{"example_thing"}, attrs, cov)
	for _, want := range []string{
		"`make upgrade-test` is what gates upgrades",
		"From v0.1.0, the release `make upgrade-test` upgrades from, 1 of 2 upgrade cells are covered and 1 aren't.",
		"v0.2.0 is tagged but registry.opentofu.org doesn't serve it yet, so it's skipped.",
		"| `example_thing` | 2 | 1 | 1 | 1 |",
		"<summary>Not covered (1)</summary>\n\n| Resource | Attribute |\n| --- | --- |\n| `example_thing` | `id` |",
		"| `example_thing` | `name` | `TestAccUpgrade` |",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("want %q in:\n%s", want, text)
		}
	}
	if headline != "upgrade coverage from v0.1.0, information only: 1 of 2 cells covered, 1 not" {
		t.Errorf("headline %q", headline)
	}
}

// An attribute the previous release didn't store, but a test upgrades
// anyway, counts as covered rather than vanishing.
func TestTheUpgradeSummaryCountsACoveredCellThePreviousReleaseDidntStore(t *testing.T) {
	attrs := map[string][]matrixAttr{"example_thing": {
		{Path: "colour", Kind: "optional", Needs: map[cell]bool{cellUpgrade: true}},
	}}
	cov := coverage{}
	cov.add(cellKey{"example_thing", "colour", cellUpgrade}, "TestAccUpgrade", "")
	if _, headline := renderUpgradeSummary(summaryRelease(), nil, []string{"example_thing"}, attrs, cov); !strings.Contains(headline, "1 of 1 cells covered") {
		t.Fatalf("headline %q", headline)
	}
}

// tagRepo is a git repository with v0.1.0 tagged on a commit before HEAD.
func tagRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"},
		{"commit", "-q", "--allow-empty", "-m", "one"},
		{"tag", "v0.1.0"},
		{"commit", "-q", "--allow-empty", "-m", "two"},
	} {
		cmd := exec.Command("git", append([]string{"-C", root, "-c", "user.name=t", "-c", "user.email=t@example.com",
			"-c", "commit.gpgsign=false", "-c", "tag.gpgsign=false"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	return root
}

// Without the previous release's tag, or without a previous release at all,
// the summary says so in one line and the run goes on.
func TestTheUpgradeSummaryIsOneLineWhenItCantBeWorkedOut(t *testing.T) {
	root := tagRepo(t)
	untagged := func(string) (string, []string, error) { return "v0.2.0", nil, nil }
	none := func(string) (string, []string, error) {
		return "", nil, errors.New("no published release tag to upgrade from")
	}
	for name, c := range map[string]struct {
		pick func(string) (string, []string, error)
		want string
	}{
		"tag missing": {untagged, "Upgrade coverage isn't shown: v0.2.0, the release make upgrade-test upgrades from, " +
			"isn't a tag in this clone. git fetch --tags to see it\n"},
		"no release": {none, "Upgrade coverage isn't shown: " + previousRule + " couldn't pick the release " +
			"make upgrade-test upgrades from: no published release tag to upgrade from\n"},
	} {
		text, _ := upgradeSummaryText(root, c.pick, []string{"example_thing"}, rulesAttrs(), rulesCoverage())
		if text != upgradeSummaryTitle+c.want {
			t.Errorf("%s: got %q", name, text)
		}
	}
}

// upgradeFromScratch runs ci/upgrade-against-fake.sh --previous in a copy of
// the scripts, against tags made for the test, with the registries serving
// served. go and curl are stand-ins, so it needs no network.
func upgradeFromScratch(t *testing.T, tags []string, served string, env ...string) (string, []string, error) {
	t.Helper()
	root := t.TempDir()
	for _, f := range []string{previousRule, "scripts/lib/common.sh", "scripts/lib/walkthrough.sh"} {
		b, err := os.ReadFile(filepath.Join(repoRoot(t), f))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, f)), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, f), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", root, "-c", "user.name=t", "-c", "user.email=t@example.com",
			"-c", "commit.gpgsign=false", "-c", "tag.gpgsign=false"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run("init", "-q")
	for _, tag := range append(tags, "") {
		run("commit", "-q", "--allow-empty", "-m", "commit")
		if tag != "" {
			run("tag", tag)
		}
	}
	run("update-ref", "refs/remotes/origin/main", "HEAD")
	bin := t.TempDir()
	stubs := map[string]string{
		"go":   "#!/bin/sh\nprintf 'Provider type name: example\\nRegistry namespace: example\\n'\n",
		"curl": "#!/bin/sh\ncat <<'JSON'\n" + served + "\nJSON\n",
	}
	for name, body := range stubs {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o700); err != nil { //nolint:gosec // a stand-in to run
			t.Fatal(err)
		}
	}
	// What CI sets would pick a release line from the job, not the repo.
	var base []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		switch name {
		case "GITHUB_ACTIONS", "GITHUB_BASE_REF", "GITHUB_REF_NAME", "PREVIOUS", "PATH":
		default:
			base = append(base, kv)
		}
	}
	base = append(base, "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return upgradeFromEnv(root, append(base, env...))
}

// The summary's release is the upgrade job's: the newest published one, a
// tagged but unpublished one skipped, and PREVIOUS when it's set.
func TestTheUpgradeSummaryUsesTheUpgradeJobsRule(t *testing.T) {
	served := `{"versions":[{"version":"0.1.0"},{"version":"0.2.0"}]}`
	tag, skipped, err := upgradeFromScratch(t, []string{"v0.1.0", "v0.2.0", "v0.3.0"}, served)
	if err != nil || tag != "v0.2.0" || len(skipped) != 1 ||
		skipped[0] != "v0.3.0 is tagged but registry.terraform.io and registry.opentofu.org doesn't serve it yet, so it's skipped" {
		t.Fatalf("got %q %q %v, want v0.2.0 with v0.3.0 skipped", tag, skipped, err)
	}
	if pinned, _, pinErr := upgradeFromScratch(t, []string{"v0.1.0", "v0.2.0"}, served, "PREVIOUS=0.1.0"); pinErr != nil || pinned != "v0.1.0" {
		t.Fatalf("with PREVIOUS=0.1.0 got %q %v", pinned, pinErr)
	}
	_, _, err = upgradeFromScratch(t, nil, served)
	if err == nil || !strings.Contains(err.Error(), "no published release tag to upgrade from") {
		t.Fatalf("with no tags want the rule's failure, got %v", err)
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
