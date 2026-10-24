package repocheck

import (
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// The release tag check reads main, the release branch and the tag from what
// its checkout fetched, with no token left behind. The dispatch can't run
// before a release, so ci.yml's release-checkout job runs the same checkout on
// every pull request and asserts what the step relies on. That proves nothing
// unless the two checkouts stay the same.
const (
	releaseCheckoutDispatch    = "release-dispatch.yml"
	releaseCheckoutDispatchJob = "validate-release-tag"
	releaseCheckoutCI          = "ci.yml"
	releaseCheckoutCIJob       = "release-checkout"
)

type checkoutStep struct {
	Uses string
	With map[string]any
}

// jobCheckout returns the first actions/checkout step of a job.
func jobCheckout(workflow, job string) (checkoutStep, error) {
	var doc struct {
		Jobs map[string]struct {
			Steps []struct {
				Uses string         `yaml:"uses"`
				With map[string]any `yaml:"with"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal([]byte(workflow), &doc); err != nil {
		return checkoutStep{}, fmt.Errorf("parse: %w", err)
	}
	j, ok := doc.Jobs[job]
	if !ok {
		return checkoutStep{}, fmt.Errorf("no job %s", job)
	}
	for _, s := range j.Steps {
		if strings.HasPrefix(s.Uses, "actions/checkout@") {
			return checkoutStep{Uses: s.Uses, With: s.With}, nil
		}
	}
	return checkoutStep{}, fmt.Errorf("job %s has no actions/checkout step", job)
}

func validateReleaseCheckout(workflows map[string]string) error {
	dispatch, err := jobCheckout(workflows[releaseCheckoutDispatch], releaseCheckoutDispatchJob)
	if err != nil {
		return fmt.Errorf("%s: %w", releaseCheckoutDispatch, err)
	}
	ci, err := jobCheckout(workflows[releaseCheckoutCI], releaseCheckoutCIJob)
	if err != nil {
		return fmt.Errorf("%s: %w, so nothing proves the release checkout on a pull request", releaseCheckoutCI, err)
	}
	if fmt.Sprint(dispatch.With["persist-credentials"]) != "false" {
		return fmt.Errorf("%s %s: the checkout keeps its token; set persist-credentials: false", releaseCheckoutDispatch, releaseCheckoutDispatchJob)
	}
	if fmt.Sprint(dispatch.With["fetch-depth"]) != "0" {
		return fmt.Errorf("%s %s: the checkout needs fetch-depth: 0 to fetch every branch and tag", releaseCheckoutDispatch, releaseCheckoutDispatchJob)
	}
	if dispatch.Uses != ci.Uses || !reflect.DeepEqual(dispatch.With, ci.With) {
		return fmt.Errorf("%s %s's checkout (%s %v) differs from %s %s's (%s %v); change both together",
			releaseCheckoutDispatch, releaseCheckoutDispatchJob, dispatch.Uses, dispatch.With,
			releaseCheckoutCI, releaseCheckoutCIJob, ci.Uses, ci.With)
	}
	return nil
}

func releaseCheckoutWorkflows(t *testing.T) map[string]string {
	t.Helper()
	workflows, err := workflowSources(filepath.Join("..", "..", ".github", "workflows"))
	if err != nil {
		t.Fatal(err)
	}
	return workflows
}

func TestTheReleaseCheckoutIsProvenOnPullRequests(t *testing.T) {
	if err := validateReleaseCheckout(releaseCheckoutWorkflows(t)); err != nil {
		t.Fatal(err)
	}
}

func TestTheReleaseCheckoutGuardRefusesDrift(t *testing.T) {
	cases := []struct {
		name, file, old, new, want string
	}{
		{"dispatch keeps its token", releaseCheckoutDispatch,
			"          fetch-depth: 0\n          persist-credentials: false\n", "          fetch-depth: 0\n", "keeps its token"},
		{"dispatch fetches shallow", releaseCheckoutDispatch,
			"          ref: main\n          fetch-depth: 0\n", "          ref: main\n          fetch-depth: 1\n", "fetch-depth: 0"},
		{"ci fetches shallow", releaseCheckoutCI,
			"          ref: main\n          fetch-depth: 0\n          persist-credentials: false\n      - name: the checkout holds",
			"          ref: main\n          fetch-depth: 1\n          persist-credentials: false\n      - name: the checkout holds", "change both together"},
		{"ci checks out another ref", releaseCheckoutCI,
			"          ref: main\n          fetch-depth: 0\n          persist-credentials: false\n      - name: the checkout holds",
			"          fetch-depth: 0\n          persist-credentials: false\n      - name: the checkout holds", "change both together"},
		{"ci pins another checkout", releaseCheckoutCI,
			"  release-checkout:\n    name: release checkout\n    runs-on: ubuntu-latest\n    timeout-minutes: 10\n    steps:\n      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1",
			"  release-checkout:\n    name: release checkout\n    runs-on: ubuntu-latest\n    timeout-minutes: 10\n    steps:\n      - uses: actions/checkout@0000000000000000000000000000000000000000", "change both together"},
		{"ci job removed", releaseCheckoutCI, "  release-checkout:\n", "  renamed-checkout:\n", "nothing proves the release checkout"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			workflows := releaseCheckoutWorkflows(t)
			if !strings.Contains(workflows[c.file], c.old) {
				t.Fatalf("%s no longer holds the text this case edits", c.file)
			}
			workflows[c.file] = strings.Replace(workflows[c.file], c.old, c.new, 1)
			err := validateReleaseCheckout(workflows)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want an error containing %q, got %v", c.want, err)
			}
		})
	}
}
