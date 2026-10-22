package repocheck

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/provider"
)

// Checks that hold a serviceability page to the files it describes. A page
// that names the contract the client is generated from, or the statuses the
// provider retries, goes stale the day either changes, and nothing about the
// change sends anybody back to the page. These fail that change instead.

// contractRelease is a k0rdent API release tag as a page names one.
var contractRelease = regexp.MustCompile(`\bv\d+\.\d+\.\d+-[a-z]+\.\d+\b|\bv1\.\d+\.\d+\b`)

// clientVersion is a client version a page names in prose, from when the
// client was a separate module pinned by version.
var clientVersion = regexp.MustCompile(`client version\W+(v\d+\.\d+\.\d+(?:-[0-9A-Za-z.]+)?)\b`)

// clientCommit is a client commit a page names, from when the client was
// pinned by commit.
var clientCommit = regexp.MustCompile(`client commit\W+([0-9a-f]{40})\b`)

// clientContractIsNamed asserts that the page at doc names release, the
// contract the client in internal/k0rdent is generated from, and no other
// contract. No page under docs/ may name a client version or a client commit:
// the client is generated in this repository, so either is stale. A
// regenerate from a newer contract that leaves the prose on the old one fails
// here.
func clientContractIsNamed(root, doc, release string) error {
	page, err := os.ReadFile(filepath.Join(root, doc)) //nolint:gosec // a path inside this repository
	if err != nil {
		return fmt.Errorf("read %s: %w", doc, err)
	}
	if problems := proseMatchesTheContract(root, doc, string(page), release); len(problems) > 0 {
		return fmt.Errorf("%s", strings.Join(problems, "; "))
	}
	return nil
}

// clientContract is the contract release internal/k0rdent/spec/SOURCE.json
// records the client's spec was cut from.
func clientContract(t *testing.T, root string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, "internal", "k0rdent", "spec", "SOURCE.json")) //nolint:gosec // a path inside this repository
	if err != nil {
		t.Fatalf("read the client's source record: %v", err)
	}
	var src struct {
		Contract struct {
			ReleaseTag string `json:"releaseTag"`
		} `json:"contract"`
	}
	if err := json.Unmarshal(raw, &src); err != nil || src.Contract.ReleaseTag == "" {
		t.Fatalf("internal/k0rdent/spec/SOURCE.json has no contract release (%v)", err)
	}
	return src.Contract.ReleaseTag
}

// statusNumber is a three-digit HTTP status as prose writes one.
var statusNumber = regexp.MustCompile(`\b[1-5][0-9]{2}\b`)

// answersToAResentDelete are the statuses the section names as what a DELETE
// sent twice can be answered with. They're answers the provider reads, not
// statuses it retries, so the section may name them in any paragraph.
var answersToAResentDelete = map[string]bool{"204": true, "409": true}

// notRetriedLead opens the paragraph that names what isn't retried.
const notRetriedLead = "Nothing else is retried"

// everyRetriedStatusIsDocumented holds the "Behaviour under load" section of
// the page at doc to provider.RetriedStatuses, which reads them from the
// retry code, both ways. Every retried status is named in it. A status the
// "Nothing else is retried" paragraph names isn't retried. Any other status
// it names is retried, or is an answer to a resent DELETE.
func everyRetriedStatusIsDocumented(root, doc string) error {
	raw, err := os.ReadFile(filepath.Join(root, doc)) //nolint:gosec // a path inside this repository
	if err != nil {
		return fmt.Errorf("read %s: %w", doc, err)
	}
	const heading = "## Behaviour under load"
	_, section, found := strings.Cut(string(raw), heading)
	if !found {
		return fmt.Errorf("%s has no %q section", doc, heading)
	}
	if end := strings.Index(section, "\n## "); end >= 0 {
		section = section[:end]
	}
	retried := map[string]bool{}
	var all []string
	for _, status := range provider.RetriedStatuses() {
		retried[strconv.Itoa(status)] = true
		all = append(all, strconv.Itoa(status))
	}

	named := map[string]bool{}
	var problems []string
	for _, para := range strings.Split(section, "\n\n") {
		refusing := strings.HasPrefix(strings.TrimSpace(para), notRetriedLead)
		for _, s := range statusNumber.FindAllString(para, -1) {
			named[s] = true
			switch {
			case refusing && retried[s]:
				problems = append(problems, fmt.Sprintf("%s says %s isn't retried and the provider retries it", doc, s))
			case !refusing && !retried[s] && !answersToAResentDelete[s]:
				problems = append(problems, fmt.Sprintf("%s says %s is retried and the provider doesn't retry it", doc, s))
			}
		}
	}
	var missing []string
	for _, s := range all {
		if !named[s] {
			missing = append(missing, s)
		}
	}
	if len(missing) > 0 {
		problems = append(problems, fmt.Sprintf("the provider retries %s and %s's %q doesn't say so",
			strings.Join(missing, ", "), doc, heading))
	}
	if len(problems) > 0 {
		return fmt.Errorf("%s", strings.Join(problems, "; "))
	}
	return nil
}

// proseMatchesTheContract lists what in the docs disagrees with release.
func proseMatchesTheContract(root, doc, page, release string) []string {
	var problems []string
	if !strings.Contains(page, release) {
		problems = append(problems, fmt.Sprintf("%s doesn't name %s, the contract the client is generated from", doc, release))
	}
	for _, named := range contractRelease.FindAllString(page, -1) {
		if named != release {
			problems = append(problems, fmt.Sprintf("%s names contract %s, and the client is generated from %s",
				doc, named, release))
		}
	}
	pages, err := filepath.Glob(filepath.Join(root, "docs", "*.md"))
	if err != nil {
		return append(problems, err.Error())
	}
	for _, path := range pages {
		raw, err := os.ReadFile(path) //nolint:gosec // a path inside this repository
		if err != nil {
			return append(problems, err.Error())
		}
		for _, m := range clientVersion.FindAllStringSubmatch(string(raw), -1) {
			problems = append(problems, fmt.Sprintf("docs/%s names client version %s, and the client is generated "+
				"in internal/k0rdent", filepath.Base(path), m[1]))
		}
		for _, m := range clientCommit.FindAllStringSubmatch(string(raw), -1) {
			problems = append(problems, fmt.Sprintf("docs/%s names client commit %s, and the client is generated "+
				"in internal/k0rdent", filepath.Base(path), m[1]))
		}
	}
	return problems
}

func TestTheClientContractIsNamed(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files map[string]string
		want  string
	}{
		{"named", map[string]string{
			"docs/support-matrix.md": "the client in internal/k0rdent, generated from v1.3.0-dev.4",
		}, ""},
		{"a page naming an older contract", map[string]string{
			"docs/support-matrix.md": "v1.3.0-dev.4, then **v1.3.0-dev.3**",
		}, "docs/support-matrix.md names contract v1.3.0-dev.3, and the client is generated from v1.3.0-dev.4"},
		{"a page naming no contract", map[string]string{
			"docs/support-matrix.md": "the client",
		}, "docs/support-matrix.md doesn't name v1.3.0-dev.4"},
		{"another page still naming a client version", map[string]string{
			"docs/support-matrix.md":  "generated from v1.3.0-dev.4",
			"docs/release-signing.md": "builds against the client version\n`v0.1.0`, from",
		}, "docs/release-signing.md names client version v0.1.0"},
		{"a page still naming a client commit", map[string]string{
			"docs/support-matrix.md":  "generated from v1.3.0-dev.4",
			"docs/release-signing.md": "the pinned client commit `8bca31ed9bba55263b3e57b654ec7aa3f940fd00`",
		}, "docs/release-signing.md names client commit 8bca31ed9bba55263b3e57b654ec7aa3f940fd00"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := clientContractIsNamed(writeTree(t, tc.files), "docs/support-matrix.md", "v1.3.0-dev.4")
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("refused: %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestEveryRetriedStatusIsDocumented(t *testing.T) {
	full := "## Behaviour under load\n\nA 429, a 503, and a 502 or 504 are waited out.\n\n## Next\n"
	if err := everyRetriedStatusIsDocumented(
		writeTree(t, map[string]string{"docs/known-limitations.md": full}), "docs/known-limitations.md"); err != nil {
		t.Fatalf("refused: %v", err)
	}
	// The number has to be in the section, not anywhere on the page.
	short := "## Behaviour under load\n\nA 429 is waited out.\n\n## Next\n503 502 504\n"
	err := everyRetriedStatusIsDocumented(
		writeTree(t, map[string]string{"docs/known-limitations.md": short}), "docs/known-limitations.md")
	if err == nil || !strings.Contains(err.Error(), "502, 503, 504") {
		t.Fatalf("err = %v", err)
	}
}

// The other way round: a status the section says is retried, and the
// provider doesn't retry, fails too, and so does one the "Nothing else is
// retried" paragraph names that the provider does retry.
func TestADocumentedRetryTheProviderDoesntMakeFails(t *testing.T) {
	const retried = "A 429, a 503, and a 502 or 504 are waited out."
	for name, tc := range map[string]struct{ section, want string }{
		"a retry the code doesn't make": {retried + "\n\nA 418 is waited out the same way.",
			"says 418 is retried and the provider doesn't retry it"},
		"a refusal the code retries": {retried + "\n\nNothing else is retried, deliberately. A 503 is reported at once.",
			"says 503 isn't retried and the provider retries it"},
		"answers to a resent delete": {retried + "\n\nA resent DELETE can meet a 409, or a 204 by the contract." +
			"\n\nNothing else is retried, deliberately. A 401 or a 500 is reported.", ""},
	} {
		t.Run(name, func(t *testing.T) {
			err := everyRetriedStatusIsDocumented(writeTree(t, map[string]string{
				"docs/known-limitations.md": "## Behaviour under load\n\n" + tc.section + "\n\n## Next\n"}), "docs/known-limitations.md")
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("refused: %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

// The real files.
func TestTheRepositorysClientAndRetriesAreDocumented(t *testing.T) {
	root := repoRoot(t)
	if err := clientContractIsNamed(root, "docs/support-matrix.md", clientContract(t, root)); err != nil {
		t.Error(err)
	}
	if err := everyRetriedStatusIsDocumented(root, "docs/known-limitations.md"); err != nil {
		t.Error(err)
	}
}
