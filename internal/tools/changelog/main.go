// Command changelog checks the entries in .changelog/ and renders them as a
// release's section of CHANGELOG.md, in the format HashiCorp's changelog
// specification sets out and the AWS and Google providers publish.
//
// Each pull request adds .changelog/<number>.txt holding one or more
// go-changelog blocks:
//
//	```release-note:enhancement
//	resource/<prefix>project: Add `region` argument
//	```
//
// Entries live one file per pull request so that two branches never edit the
// same lines, which is what a hand-edited CHANGELOG.md does on every merge.
// .changelog/README.md is the guide for writing one.
//
// `make changelog-check` runs -check, which checks that every entry in the
// directory parses. The changelog workflow runs it on every pull request with
// CHANGELOG_PR, CHANGELOG_BASE and CHANGELOG_LABELS from the event, and then
// it also diffs the directory against the base: the pull request has to add
// or edit its own entry unless it carries the no-changelog-needed label, and
// mustn't add a placeholder or touch another pull request's entry. `make
// changelog` runs -render.
//
// The render reads the files on disk rather than diffing two git refs the way
// go-changelog's changelog-build does. The release is cut from one branch and
// the release pull request moves the rendered entries out of the directory, so
// the directory is the unreleased section, and the check and the render read
// the same thing CI just validated.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Note is one release-note block.
type Note struct {
	Type string
	Body string
	// Issue is the pull request number the file is named for, or the
	// placeholder's name before the pull request exists.
	Issue string
}

// noChangelogLabel excuses a pull request from adding an entry. It's the
// label the AWS provider's readiness check reads. It excuses a missing entry
// only: every entry in the directory is still checked.
const noChangelogLabel = "no-changelog-needed"

// The section each type renders under, in the order the specification lists
// them. FEATURES gathers every new-* type, as the AWS template does.
var sections = []struct {
	heading string
	types   []string
}{
	{"BREAKING CHANGES", []string{"breaking-change"}},
	{"NOTES", []string{"note"}},
	{"FEATURES", []string{"new-resource", "new-data-source", "new-ephemeral", "new-list-resource", "new-action", "new-function", "new-guide"}},
	{"ENHANCEMENTS", []string{"enhancement"}},
	{"BUG FIXES", []string{"bug"}},
}

// featureLabels is how a new-* entry is introduced. The body of one is only
// the name, so the label carries what kind of thing it is.
var featureLabels = map[string]string{
	"new-resource":      "New Resource",
	"new-data-source":   "New Data Source",
	"new-ephemeral":     "New Ephemeral Resource",
	"new-list-resource": "New List Resource",
	"new-action":        "New Action",
	"new-function":      "New Function",
	"new-guide":         "New Guide",
}

func knownTypes() []string {
	var out []string
	for _, s := range sections {
		out = append(out, s.types...)
	}
	return out
}

var (
	fenceOpen  = regexp.MustCompile("^```release-note:(.*)$")
	fileName   = regexp.MustCompile(`^([0-9]+|PENDING-[a-z0-9-]+)\.txt$`)
	prNumber   = regexp.MustCompile(`^[1-9][0-9]*$`)
	functionID = `[a-z][a-z0-9_]*`
)

// parse reads one entry file. name is the file's base name. prefix is the
// provider's type prefix, such as "example_", which every resource, data source,
// ephemeral resource, list resource and action name starts with.
//
// It's stricter than go-changelog, which reads whatever blocks it finds and
// ignores the rest. A fence that's indented, misspelled or never closed is a
// note go-changelog drops without a word, and the release ships without it.
// Here anything that isn't a well-formed block is refused.
func parse(name, body, prefix string) ([]Note, []string) {
	issue := strings.TrimSuffix(name, ".txt")
	typeName := regexp.QuoteMeta(prefix) + `[a-z0-9_]+`
	subsystem := regexp.MustCompile(`^(provider|(resource|data-source|ephemeral-resource|list-resource|action)/` +
		typeName + `|function/` + functionID + `): \S`)
	bareName := regexp.MustCompile(`^` + typeName + `$`)
	bareFunction := regexp.MustCompile(`^` + functionID + `$`)

	var notes []Note
	var problems []string
	report := func(line int, format string, args ...any) {
		problems = append(problems, fmt.Sprintf("%s:%d: %s", name, line, fmt.Sprintf(format, args...)))
	}

	lines := strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n")
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if strings.TrimSpace(line) == "" {
			continue
		}
		m := fenceOpen.FindStringSubmatch(line)
		if m == nil {
			report(i+1, "%q is outside a release-note block. A block opens with ```release-note:<type> at the start of a line", line)
			continue
		}
		start, typ := i+1, m[1]
		var text []string
		closed := false
		for i++; i < len(lines); i++ {
			if lines[i] == "```" {
				closed = true
				break
			}
			if t := strings.TrimSpace(lines[i]); t != "" {
				text = append(text, t)
			}
		}
		if !closed {
			report(start, "the block is never closed with ``` on a line of its own")
			break
		}
		if !slices.Contains(knownTypes(), typ) {
			report(start, "%q isn't a release-note type. Use one of: %s", typ, strings.Join(knownTypes(), ", "))
			continue
		}
		if len(text) != 1 {
			report(start, "a block holds one note on one line, and this one has %d lines", len(text))
			continue
		}
		note := Note{Type: typ, Body: text[0], Issue: issue}
		switch {
		case typ == "new-guide":
		case typ == "new-function":
			if !bareFunction.MatchString(note.Body) {
				report(start, "a new-function block holds only the function's name, such as parse_id, and this one says %q", note.Body)
				continue
			}
		case strings.HasPrefix(typ, "new-"):
			if !bareName.MatchString(note.Body) {
				report(start, "a %s block holds only the type name, such as %sproject, without backticks, and this one says %q", typ, prefix, note.Body)
				continue
			}
		default:
			if !subsystem.MatchString(note.Body) {
				report(start, "a %s note starts with what it changes and a colon, such as \"resource/%sproject: \", "+
					"\"data-source/%sregions: \" or \"provider: \", and this one says %q", typ, prefix, prefix, note.Body)
				continue
			}
		}
		notes = append(notes, note)
	}
	if len(notes) == 0 && len(problems) == 0 {
		problems = append(problems, name+": holds no release-note block")
	}
	return notes, problems
}

// notAnEntry is a file in the directory that isn't an entry: the guide, and
// dotfiles an editor or the operating system leaves behind.
func notAnEntry(name string) bool {
	return name == "README.md" || strings.HasPrefix(name, ".")
}

// load parses every entry in dir.
func load(dir, prefix string) ([]Note, []string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, []string{fmt.Sprintf("can't read %s: %v", dir, err)}
	}
	var notes []Note
	var problems []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || notAnEntry(name) {
			continue
		}
		if !fileName.MatchString(name) {
			problems = append(problems, fmt.Sprintf("%s: an entry is named for its pull request, such as 123.txt", name))
			continue
		}
		body, err := os.ReadFile(filepath.Join(dir, name)) //nolint:gosec // a file in the entries directory
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", name, err))
			continue
		}
		n, p := parse(name, string(body), prefix)
		notes = append(notes, n...)
		problems = append(problems, p...)
	}
	return notes, problems
}

// check validates the format of every entry in dir. It's all `make check`
// runs, since a local tree has no pull request to judge.
func check(dir, prefix string) []string {
	_, problems := load(dir, prefix)
	return problems
}

// change is one file the pull request changed in the entries directory, as
// git diff --name-status reports it: 'A'dded, 'M'odified, 'D'eleted, or 'T'
// for a type change.
type change struct {
	status byte
	name   string
}

// diffChanges lists what the pull request changed in dir, from where its
// branch left base to HEAD. In CI HEAD is the pull request merged into its
// base, and the three-dot form diffs from their merge base, so a base that
// has moved on since doesn't show up as this pull request undoing it.
// Renames are a delete and an add, so a renamed entry is judged as both.
func diffChanges(repoDir, base, dir string) ([]change, error) {
	out, err := exec.Command("git", "-C", repoDir, "diff", "--name-status", "--no-renames", "-z",
		base+"...HEAD", "--", dir).Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return nil, fmt.Errorf("git diff against %s: %v: %s", base, err, strings.TrimSpace(string(exit.Stderr)))
		}
		return nil, fmt.Errorf("git diff against %s: %w", base, err)
	}
	// -z writes status NUL path NUL, so a path can't be misread.
	fields := strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
	var changes []change
	for i := 0; i+1 < len(fields); i += 2 {
		changes = append(changes, change{status: fields[i][0], name: filepath.Base(fields[i+1])})
	}
	return changes, nil
}

// prCheck judges what pull request pr changed in the entries directory.
//
// It passes when the pull request adds or edits its own <pr>.txt, or carries
// the no-changelog-needed label. A placeholder fails only when this pull
// request adds it, since one it inherits from a base branch further down a
// stack is that branch's to rename. Adding, editing or deleting any other
// pull request's entry fails unless the label is set, because each entry
// belongs to the pull request it's named for. The label excuses that, for a
// pull request whose purpose is to fix up entries, and never excuses adding
// a placeholder.
func prCheck(pr string, prLabels []string, changes []change) []string {
	if !prNumber.MatchString(pr) {
		return []string{fmt.Sprintf("%q isn't a pull request number", pr)}
	}
	own := pr + ".txt"
	labelled := slices.Contains(prLabels, noChangelogLabel)
	var problems []string
	hasOwn, addsPlaceholder := false, false
	for _, c := range changes {
		switch {
		case notAnEntry(c.name):
		case c.name == own:
			hasOwn = c.status != 'D'
		case strings.HasPrefix(c.name, "PENDING-") && c.status == 'A':
			addsPlaceholder = true
			problems = append(problems, fmt.Sprintf("this pull request adds %s: rename %s to %s, now that the pull request has a number",
				c.name, c.name, own))
		case labelled:
		case c.status == 'A':
			problems = append(problems, fmt.Sprintf("this pull request adds %s, which is named for another pull request. "+
				"Its own entry is %s. Label it %s if adding it is the point", c.name, own, noChangelogLabel))
		default:
			problems = append(problems, fmt.Sprintf("this pull request changes %s, another pull request's entry. "+
				"Label it %s if changing it is the point", c.name, noChangelogLabel))
		}
	}
	// The rename is the whole fix, so a missing entry isn't reported as well.
	if !hasOwn && !addsPlaceholder && !labelled {
		problems = append(problems, fmt.Sprintf("this pull request adds no .changelog/%s. Add one as .changelog/README.md "+
			"describes, or, for a change no configuration can see, label the pull request %s", own, noChangelogLabel))
	}
	return problems
}

// labels reads the pull request's labels from what CI passes: the JSON array
// toJSON(github.event.pull_request.labels.*.name) writes, or, by hand, a
// comma-separated list.
func labels(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	var out []string
	if strings.HasPrefix(s, "[") && json.Unmarshal([]byte(s), &out) == nil {
		return out
	}
	for _, l := range strings.Split(s, ",") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

// render writes one release's section. An empty date is the unreleased
// section, which the specification heads "(Unreleased)".
//
// Within a section, provider-wide notes come first and the rest go in
// alphabetical order, which sorts them by what they change, as the
// specification asks. FEATURES sorts by kind, then name.
func render(notes []Note, version, date, repo string) string {
	when := "Unreleased"
	if d, err := time.Parse(time.DateOnly, date); err == nil {
		when = d.Format("January 2, 2006")
	}
	var b strings.Builder
	fmt.Fprintf(&b, "## %s (%s)\n", version, when)
	if len(notes) == 0 {
		b.WriteString("\nNo changes recorded in .changelog/ yet.\n")
		return b.String()
	}
	for _, s := range sections {
		var lines []string
		for _, n := range notes {
			if slices.Contains(s.types, n.Type) {
				lines = append(lines, line(n, repo))
			}
		}
		if len(lines) == 0 {
			continue
		}
		slices.SortStableFunc(lines, func(a, b string) int {
			pa, pb := strings.HasPrefix(a, "* provider: "), strings.HasPrefix(b, "* provider: ")
			if pa != pb {
				if pa {
					return -1
				}
				return 1
			}
			return strings.Compare(a, b)
		})
		fmt.Fprintf(&b, "\n%s:\n\n%s\n", s.heading, strings.Join(lines, "\n"))
	}
	return b.String()
}

func line(n Note, repo string) string {
	ref := " (" + n.Issue + ")"
	if _, err := strconv.Atoi(n.Issue); err == nil {
		ref = fmt.Sprintf(" ([#%s](%s/pull/%s))", n.Issue, repo, n.Issue)
	}
	switch label := featureLabels[n.Type]; {
	case n.Type == "new-guide":
		return "* **" + label + ":** " + n.Body + ref
	case label != "":
		return "* **" + label + ":** `" + n.Body + "`" + ref
	}
	return "* " + n.Body + ref
}

func main() {
	var (
		dir     = flag.String("dir", ".changelog", "the directory of entries")
		prefix  = flag.String("prefix", "", "the provider's type prefix, which the Makefile gives as $(PROVIDER_NAME)_")
		doCheck = flag.Bool("check", false, "validate every entry, and the pull request's own when -pr is set")
		pr      = flag.String("pr", os.Getenv("CHANGELOG_PR"), "the pull request number (default $CHANGELOG_PR)")
		base    = flag.String("base", os.Getenv("CHANGELOG_BASE"), "the commit the pull request is based on (default $CHANGELOG_BASE)")
		labelsF = flag.String("labels", os.Getenv("CHANGELOG_LABELS"), "the pull request's labels, a JSON array or a comma-separated list (default $CHANGELOG_LABELS)")
		version = flag.String("version", "", "the version heading to render, such as 0.2.0")
		date    = flag.String("date", "", "the release date, YYYY-MM-DD. Without one the section is headed (Unreleased)")
		repo    = flag.String("repo", "", "the repository URL pull request links go to")
	)
	flag.Parse()
	if *prefix == "" {
		fail("-prefix is required")
	}

	if *doCheck {
		problems := check(*dir, *prefix)
		pull := strings.TrimSpace(*pr)
		if pull != "" {
			if strings.TrimSpace(*base) == "" {
				fail("CHANGELOG_PR needs CHANGELOG_BASE, the commit the pull request is based on, to tell what it changed")
			}
			changes, err := diffChanges(".", strings.TrimSpace(*base), *dir)
			if err != nil {
				fail(err.Error())
			}
			problems = append(problems, prCheck(pull, labels(*labelsF), changes)...)
		}
		for _, p := range problems {
			fmt.Fprintln(os.Stderr, "changelog: "+p)
		}
		if len(problems) > 0 {
			fail(fmt.Sprintf("%d problem(s)", len(problems)))
		}
		notes, _ := load(*dir, *prefix)
		fmt.Printf("changelog: %d note(s) in %s, all well formed\n", len(notes), *dir)
		return
	}

	if *version == "" || *repo == "" {
		fail("-version and -repo are required to render")
	}
	if *date != "" {
		if _, err := time.Parse(time.DateOnly, *date); err != nil {
			fail(fmt.Sprintf("-date %q isn't YYYY-MM-DD", *date))
		}
	}
	notes, problems := load(*dir, *prefix)
	for _, p := range problems {
		fmt.Fprintln(os.Stderr, "changelog: "+p)
	}
	if len(problems) > 0 {
		fail("fix the entries before rendering them")
	}
	fmt.Print(render(notes, *version, *date, *repo))
}

func fail(msg string) {
	fmt.Fprintln(os.Stderr, "changelog: "+msg)
	os.Exit(1)
}
