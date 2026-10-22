package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const prefix = "example_"

func block(typ, body string) string {
	return "```release-note:" + typ + "\n" + body + "\n```\n"
}

func TestParseReadsEveryBlock(t *testing.T) {
	body := block("enhancement", "resource/example_project: Add `region` argument") + "\n" +
		block("new-data-source", "example_regions")
	notes, problems := parse("75.txt", body, prefix)
	if len(problems) > 0 {
		t.Fatalf("problems: %v", problems)
	}
	if len(notes) != 2 || notes[0].Type != "enhancement" || notes[1].Body != "example_regions" || notes[1].Issue != "75" {
		t.Fatalf("notes: %+v", notes)
	}
}

// Each of these is a mistake the AWS changelog shipped at least once:
// `resoource/`, `data/`, and a block go-changelog read as nothing.
func TestParseRefuses(t *testing.T) {
	cases := map[string]string{
		"an unknown type":               block("feature", "resource/example_project: Add `region`"),
		"a misspelled subsystem":        block("bug", "resoource/example_project: Fix the read"),
		"a subsystem missing its kind":  block("bug", "example_project: Fix the read"),
		"another provider's type":       block("bug", "resource/aws_instance: Fix the read"),
		"no message after the prefix":   block("bug", "resource/example_project:"),
		"no subsystem at all":           block("enhancement", "Add a thing"),
		"a new resource with a verb":    block("new-resource", "Add example_project"),
		"a new resource in backticks":   block("new-resource", "`example_project`"),
		"an empty block":                "```release-note:bug\n```\n",
		"two lines in one note":         block("bug", "resource/example_project: Fix\nthe read"),
		"an unclosed block":             "```release-note:bug\nresource/example_project: Fix the read\n",
		"text outside a block":          "see below\n" + block("bug", "resource/example_project: Fix the read"),
		"an indented fence":             "  " + block("bug", "resource/example_project: Fix the read"),
		"a fence go-changelog wouldn't": "```release_note:bug\nresource/example_project: Fix the read\n```\n",
		"no block at all":               "\n\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, problems := parse("75.txt", body, prefix); len(problems) == 0 {
				t.Fatalf("accepted %q", body)
			}
		})
	}
}

func TestParseAccepts(t *testing.T) {
	for _, body := range []string{
		block("breaking-change", "provider: Refuse `region = \"\"`"),
		block("note", "data-source/example_regions: The `id` attribute is deprecated and is removed in v1.0.0"),
		block("bug", "ephemeral-resource/example_cluster_kubeconfig: Fix the expiry"),
		block("enhancement", "list-resource/example_project: Add `region` to the filter"),
		block("enhancement", "action/example_cancel_operation: Wait for the cancel to settle"),
		block("enhancement", "function/parse_id: Accept a region"),
		block("new-resource", "example_project"),
		block("new-data-source", "example_regions"),
		block("new-ephemeral", "example_cluster_kubeconfig"),
		block("new-list-resource", "example_project"),
		block("new-action", "example_cancel_operation"),
		block("new-function", "parse_id"),
		block("new-guide", "Upgrading to v1.0.0"),
		// CRLF from an editor on Windows is the same entry.
		strings.ReplaceAll(block("bug", "resource/example_project: Fix the read"), "\n", "\r\n"),
	} {
		if _, problems := parse("75.txt", body, prefix); len(problems) > 0 {
			t.Errorf("refused %q: %v", body, problems)
		}
	}
}

func writeEntries(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

var good = block("bug", "resource/example_project: Fix the read")

func TestCheckValidatesEveryEntryAndSkipsTheRest(t *testing.T) {
	dir := writeEntries(t, map[string]string{
		"75.txt": good, "PENDING-changelog.txt": good, "README.md": "# about\n",
		".DS_Store": "not an entry", ".gitkeep": "",
	})
	if problems := check(dir, prefix); len(problems) > 0 {
		t.Fatalf("problems: %v", problems)
	}
}

// The label excuses a missing entry, not a broken one somebody else wrote,
// so the format check runs whatever the pull request carries.
func TestCheckRefusesABrokenEntry(t *testing.T) {
	dir := writeEntries(t, map[string]string{"75.txt": block("feature", "x")})
	if problems := check(dir, prefix); len(problems) == 0 {
		t.Fatal("a broken entry passed")
	}
}

func TestCheckRefusesAFileNamedForNothing(t *testing.T) {
	dir := writeEntries(t, map[string]string{"fix-the-read.txt": good, "75.md": good})
	if problems := check(dir, prefix); len(problems) != 2 {
		t.Fatalf("want both names refused: %v", problems)
	}
}

// prCheck judges what the pull request changed in .changelog/ against its
// base, as git diff --name-status reports it.
func TestPRCheck(t *testing.T) {
	label := []string{"documentation", noChangelogLabel}
	cases := []struct {
		name    string
		changes []change
		labels  []string
		want    string // a fragment of the one problem, or "" for none
	}{
		{"adds its own entry", []change{{'A', "107.txt"}}, nil, ""},
		{"edits its own entry", []change{{'M', "107.txt"}}, nil, ""},
		{"no entry", nil, nil, "adds no .changelog/107.txt"},
		{"only touches the guide", []change{{'M', "README.md"}}, nil, "adds no .changelog/107.txt"},
		{"no entry, labelled", nil, label, ""},
		{"deletes its own entry", []change{{'D', "107.txt"}}, nil, "adds no .changelog/107.txt"},
		{"adds a placeholder", []change{{'A', "PENDING-x.txt"}}, nil, "rename PENDING-x.txt to 107.txt"},
		{"adds a placeholder, labelled", []change{{'A', "PENDING-x.txt"}}, label, "rename PENDING-x.txt to 107.txt"},
		{"rewrites another entry", []change{{'A', "107.txt"}, {'M', "75.txt"}}, nil, "changes 75.txt"},
		{"deletes another entry", []change{{'A', "107.txt"}, {'D', "75.txt"}}, nil, "changes 75.txt"},
		{"rewrites an inherited placeholder", []change{{'A', "107.txt"}, {'M', "PENDING-x.txt"}}, nil, "changes PENDING-x.txt"},
		{"adds another number's entry", []change{{'A', "107.txt"}, {'A', "75.txt"}}, nil, "adds 75.txt"},
		{"rewrites another entry, labelled", []change{{'M', "75.txt"}, {'D', "76.txt"}}, label, ""},
		{"a dotfile", []change{{'A', "107.txt"}, {'A', ".DS_Store"}}, nil, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			problems := prCheck("107", c.labels, c.changes)
			switch {
			case c.want == "" && len(problems) > 0:
				t.Fatalf("problems: %v", problems)
			case c.want != "" && (len(problems) != 1 || !strings.Contains(problems[0], c.want)):
				t.Fatalf("want one problem containing %q, got %v", c.want, problems)
			}
		})
	}
}

func TestPRCheckRefusesANonNumericPullRequest(t *testing.T) {
	if problems := prCheck("75; rm -rf /", nil, []change{{'A', "75.txt"}}); len(problems) == 0 {
		t.Fatal("accepted a pull request number that isn't one")
	}
}

// diffChanges against a real repository: an inherited placeholder that the
// branch leaves alone doesn't appear, and what the branch did does.
func TestDiffChangesReportsOnlyWhatTheBranchDid(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	repoDir := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repoDir, "-c", "user.name=t", "-c", "user.email=t@example.com",
			"-c", "commit.gpgsign=false"}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	write := func(name, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(repoDir, ".changelog"), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repoDir, ".changelog", name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	git("init", "-q")
	write("75.txt", good)
	write("76.txt", good)
	write("PENDING-old.txt", good)
	git("add", "-A")
	git("commit", "-q", "-m", "base")
	base := git("rev-parse", "HEAD")
	write("107.txt", good)
	write("76.txt", block("bug", "provider: Fix"))
	if err := os.Remove(filepath.Join(repoDir, ".changelog", "75.txt")); err != nil {
		t.Fatal(err)
	}
	git("add", "-A")
	git("commit", "-q", "-m", "branch")

	got, err := diffChanges(repoDir, base, ".changelog")
	if err != nil {
		t.Fatal(err)
	}
	want := []change{{'A', "107.txt"}, {'D', "75.txt"}, {'M', "76.txt"}}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if _, err := diffChanges(repoDir, "0000000000000000000000000000000000000000", ".changelog"); err == nil {
		t.Fatal("a base that isn't in the repository read as no changes")
	}
}

func TestLabelsReadsJSONOrAList(t *testing.T) {
	if got := labels(`["a, b","no-changelog-needed"]`); len(got) != 2 || got[0] != "a, b" {
		t.Fatalf("JSON: %q", got)
	}
	if got := labels("a,no-changelog-needed"); len(got) != 2 || got[1] != noChangelogLabel {
		t.Fatalf("list: %q", got)
	}
	if got := labels(""); len(got) != 0 {
		t.Fatalf("empty: %q", got)
	}
}

const repo = "https://github.com/example/terraform-provider-example"

func TestRenderFollowsTheHashiCorpSpec(t *testing.T) {
	notes := []Note{
		{Type: "bug", Body: "resource/example_subnet: Fix the read", Issue: "80"},
		{Type: "bug", Body: "provider: Fix the token refresh", Issue: "81"},
		{Type: "bug", Body: "data-source/example_regions: Fix the filter", Issue: "79"},
		{Type: "enhancement", Body: "resource/example_project: Add `region` argument", Issue: "106"},
		{Type: "new-resource", Body: "example_project", Issue: "75"},
		{Type: "new-data-source", Body: "example_regions", Issue: "76"},
		{Type: "breaking-change", Body: "resource/example_project: `region` is required", Issue: "106"},
		{Type: "note", Body: "provider: Terraform 1.10 is the minimum", Issue: "77"},
	}
	got := render(notes, "0.2.0", "", repo)
	want := `## 0.2.0 (Unreleased)

BREAKING CHANGES:

* resource/example_project: ` + "`region`" + ` is required ([#106](` + repo + `/pull/106))

NOTES:

* provider: Terraform 1.10 is the minimum ([#77](` + repo + `/pull/77))

FEATURES:

* **New Data Source:** ` + "`example_regions`" + ` ([#76](` + repo + `/pull/76))
* **New Resource:** ` + "`example_project`" + ` ([#75](` + repo + `/pull/75))

ENHANCEMENTS:

* resource/example_project: Add ` + "`region`" + ` argument ([#106](` + repo + `/pull/106))

BUG FIXES:

* provider: Fix the token refresh ([#81](` + repo + `/pull/81))
* data-source/example_regions: Fix the filter ([#79](` + repo + `/pull/79))
* resource/example_subnet: Fix the read ([#80](` + repo + `/pull/80))
`
	if got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
}

func TestRenderDatesARelease(t *testing.T) {
	got := render([]Note{{Type: "bug", Body: "provider: Fix", Issue: "1"}}, "0.2.0", "2026-10-20", repo)
	if !strings.HasPrefix(got, "## 0.2.0 (October 20, 2026)\n") {
		t.Fatalf("got\n%s", got)
	}
}

// A placeholder renders without a link, since there is nothing to link to.
func TestRenderLeavesAPlaceholderUnlinked(t *testing.T) {
	got := render([]Note{{Type: "bug", Body: "provider: Fix", Issue: "PENDING-changelog"}}, "0.2.0", "", repo)
	if !strings.Contains(got, "* provider: Fix (PENDING-changelog)\n") {
		t.Fatalf("got\n%s", got)
	}
}

func TestRenderSaysWhenThereIsNothing(t *testing.T) {
	got := render(nil, "0.2.0", "", repo)
	if got != "## 0.2.0 (Unreleased)\n\nNo changes recorded in .changelog/ yet.\n" {
		t.Fatalf("got\n%s", got)
	}
}
