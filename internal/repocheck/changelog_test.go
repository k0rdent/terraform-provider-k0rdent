package repocheck

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The two changelog heading styles: a dated release, and the unreleased section.
var (
	datedRelease = regexp.MustCompile(`(?m)^## (\[[0-9]+\.[0-9]+\.[0-9]+[^\]]*\][^\n]*[0-9]{4}-[0-9]{2}-[0-9]{2}|` +
		`[0-9]+\.[0-9]+\.[0-9]+\S* \([A-Z][a-z]+ [0-9]{1,2}, [0-9]{4}\))`)
	unreleasedHeading = regexp.MustCompile(`(?m)^## (\[Unreleased\]|\S+ \(Unreleased\))`)
)

// nothingPublished is what docs/distribution.md and docs/install.md said
// until the first release.
var nothingPublished = regexp.MustCompile(`(?i)nothing is published|no tag, release or registry listing`)

// The pages that say whether the provider is published.
var distributionDocs = []string{"docs/distribution.md", "docs/install.md"}

// publishedDocsAgree is the check that changed the day the first release
// shipped. The changelog is where a release is recorded in this repository,
// so that is what is read. Until then it held only the reserved heading with
// no date, and the distribution pages said nothing was published. The release
// pull request that wrote the first dated heading rewrote them, and from then
// on no page may say nothing is published while the changelog records a
// release. Both heading styles count: Keep a Changelog's
// "## [0.1.0] - 2026-11-06, recorded before .changelog/", which the history
// is written in, and HashiCorp's "## 0.1.0 (November 6, 2026)", which
// `make changelog` writes from the first release on.
func publishedDocsAgree(changelog []byte, docs map[string][]byte) error {
	loc := datedRelease.FindIndex(changelog)
	// Proves the heading format this check reads is the format the file uses.
	// Without it, a reorganised changelog would pass by matching nothing.
	if loc == nil && !unreleasedHeading.Match(changelog) {
		return fmt.Errorf("CHANGELOG.md has no dated release heading and no `## [Unreleased]` or `## X.Y.Z (Unreleased)` heading, so the format this reads has changed")
	}
	if loc == nil {
		return nil
	}
	release := strings.TrimSpace(string(changelog[loc[0]:loc[1]]))
	var stale []string
	for _, name := range distributionDocs {
		if m := nothingPublished.Find(docs[name]); m != nil {
			stale = append(stale, fmt.Sprintf("%s says %q", name, m))
		}
	}
	if len(stale) > 0 {
		return fmt.Errorf("CHANGELOG.md records a release, %q, but %s. Say where the provider is published instead",
			release, strings.Join(stale, ", and "))
	}
	return nil
}

func TestDistributionDocsAgreeWithTheChangelog(t *testing.T) {
	root := repoRoot(t)
	changelog, err := os.ReadFile(filepath.Join(root, "CHANGELOG.md"))
	if err != nil {
		t.Fatal(err)
	}
	docs := map[string][]byte{}
	for _, name := range distributionDocs {
		if docs[name], err = os.ReadFile(filepath.Join(root, name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := publishedDocsAgree(changelog, docs); err != nil {
		t.Fatal(err)
	}
}

const unreleasedChangelog = "# Changelog\n\n## [Unreleased]\n\n### Added\n\n- something\n"

var (
	unpublishedDocs = map[string][]byte{
		"docs/distribution.md": []byte("# Distribution\n\n**Nothing is published yet.** There's no tag, release or registry listing.\n"),
		"docs/install.md":      []byte("# Installing the provider\n\n**Nothing is published yet.** Until the first release, build from source.\n"),
	}
	publishedDocs = map[string][]byte{
		"docs/distribution.md": []byte("# Distribution\n\nThe provider is published as `k0rdent/k0rdent`.\n"),
		"docs/install.md":      []byte("# Installing the provider\n\nThe provider is published as `k0rdent/k0rdent`.\n"),
	}
)

func TestPublishedDocsAgreePassesWhileNothingIsDated(t *testing.T) {
	if err := publishedDocsAgree([]byte(unreleasedChangelog+"\n## [0.1.0] - planned, not published\n"), unpublishedDocs); err != nil {
		t.Fatal(err)
	}
}

// Releases from the switch to .changelog/ on are headed the way HashiCorp's
// specification heads them. Each style has to count as a release as surely
// as the other, and a page left saying nothing is published has to fail.
func TestPublishedDocsAgreeSeesAReleaseInEitherFormat(t *testing.T) {
	for _, heading := range []string{
		"## [0.1.0] - 2026-11-06, recorded before .changelog/",
		"## [0.1.0] - 2026-10-20",
		"## 0.1.0 (October 20, 2026)",
		"## 0.1.0-rc.1 (October 20, 2026)",
	} {
		body := []byte("# Changelog\n\n" + heading + "\n\nNOTES:\n\n- something\n")
		if err := publishedDocsAgree(body, unpublishedDocs); err == nil {
			t.Errorf("%q passed with pages that say nothing is published", heading)
		}
		if err := publishedDocsAgree(body, publishedDocs); err != nil {
			t.Errorf("%q with rewritten pages: %v", heading, err)
		}
	}
}

// One page left behind is enough to fail, and the error names it.
func TestPublishedDocsAgreeNamesTheStalePage(t *testing.T) {
	docs := map[string][]byte{
		"docs/distribution.md": publishedDocs["docs/distribution.md"],
		"docs/install.md":      unpublishedDocs["docs/install.md"],
	}
	err := publishedDocsAgree([]byte("# Changelog\n\n## 0.1.0 (November 6, 2026)\n"), docs)
	if err == nil || !strings.Contains(err.Error(), "docs/install.md") || strings.Contains(err.Error(), "docs/distribution.md") {
		t.Errorf("want an error naming docs/install.md alone, got %v", err)
	}
}

// The unreleased heading HashiCorp's format uses isn't a release.
func TestPublishedDocsAgreeIgnoresAnUnreleasedSection(t *testing.T) {
	body := "# Changelog\n\n## 0.2.0 (Unreleased)\n\nNOTES:\n\n" + unreleasedChangelog[len("# Changelog\n\n"):]
	if err := publishedDocsAgree([]byte(body), unpublishedDocs); err != nil {
		t.Fatal(err)
	}
}

func TestPublishedDocsAgreeRefusesAnUnreadableFormat(t *testing.T) {
	if err := publishedDocsAgree([]byte("# Changelog\n\n### Added\n\n- something\n"), publishedDocs); err == nil {
		t.Error("a changelog with neither a dated nor an unreleased heading passed")
	}
}
