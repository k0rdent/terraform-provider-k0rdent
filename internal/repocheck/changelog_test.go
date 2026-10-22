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

// noReleaseRecorded is the check that changes the day a release ships. The
// changelog is where a release is recorded in this repository, so that is
// what is read. A reserved heading with no date is the planned state. A dated
// one means something shipped, and docs/distribution.md, which says nothing is
// published yet, needs rewriting rather than re-passing. Both heading styles
// count: Keep a Changelog's "## [0.1.0] - 2026-10-20", which the history is
// written in, and HashiCorp's "## 0.1.0 (October 20, 2026)", which
// `make changelog` writes from the first release on.
func noReleaseRecorded(changelog []byte) error {
	// Proves the heading format this check reads is the format the file uses.
	// Without it, a reorganised changelog would pass by matching nothing.
	if !unreleasedHeading.Match(changelog) {
		return fmt.Errorf("CHANGELOG.md has no `## [Unreleased]` or `## X.Y.Z (Unreleased)` heading, so the format this reads has changed")
	}
	if loc := datedRelease.FindIndex(changelog); loc != nil {
		return fmt.Errorf("CHANGELOG.md records a release: %q. Rewrite docs/distribution.md, which says nothing is published, in the same pull request",
			strings.TrimSpace(string(changelog[loc[0]:loc[1]])))
	}
	return nil
}

func TestNoReleaseIsRecordedInTheChangelog(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), "CHANGELOG.md"))
	if err != nil {
		t.Fatal(err)
	}
	if err := noReleaseRecorded(raw); err != nil {
		t.Fatal(err)
	}
}

const unreleasedChangelog = "# Changelog\n\n## [Unreleased]\n\n### Added\n\n- something\n"

func TestNoReleaseRecordedPassesWhileNothingIsDated(t *testing.T) {
	if err := noReleaseRecorded([]byte(unreleasedChangelog + "\n## [0.1.0-beta] - planned, not published\n")); err != nil {
		t.Fatal(err)
	}
}

// Releases from the switch to .changelog/ on are headed the way HashiCorp's
// specification heads them. The first of them has to turn this check red as
// surely as a Keep a Changelog heading would.
func TestNoReleaseRecordedSeesAReleaseInEitherFormat(t *testing.T) {
	for _, heading := range []string{
		"## [0.1.0] - 2026-10-20",
		"## 0.1.0 (October 20, 2026)",
		"## 0.1.0-beta.1 (October 20, 2026)",
	} {
		body := "# Changelog\n\n" + heading + "\n\nNOTES:\n\n" + unreleasedChangelog[len("# Changelog\n\n"):]
		if err := noReleaseRecorded([]byte(body)); err == nil {
			t.Errorf("%q passed as unreleased", heading)
		}
	}
}

// The unreleased heading HashiCorp's format uses isn't a release.
func TestNoReleaseRecordedIgnoresAnUnreleasedSection(t *testing.T) {
	body := "# Changelog\n\n## 0.2.0 (Unreleased)\n\nNOTES:\n\n" + unreleasedChangelog[len("# Changelog\n\n"):]
	if err := noReleaseRecorded([]byte(body)); err != nil {
		t.Fatal(err)
	}
}

func TestNoReleaseRecordedRefusesAnUnreadableFormat(t *testing.T) {
	if err := noReleaseRecorded([]byte("# Changelog\n\n### Added\n\n- something\n")); err == nil {
		t.Error("a changelog with no unreleased heading passed")
	}
}
